package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Undo kinds, one for each way a change is reversed.
const (
	undoRestoreProfile = "restore_profile"
	undoDeleteMemory   = "delete_memory"
	undoSetMemory      = "set_memory"
	undoRestoreMemory  = "restore_memory"
	undoRestoreGoals   = "restore_goals"
	undoDeleteIntake   = "delete_intake"
	undoRestoreIntake  = "restore_intake"
	undoUndeleteIntake = "undelete_intake"
	undoDeleteSleep    = "delete_sleep"
	undoRestoreSleep   = "restore_sleep"
	undoUndeleteSleep  = "undelete_sleep"
)

// undoRecord is what reverses one action, stored with it as JSON. Op says
// how, and the other fields hold what that needs.
type undoRecord struct {
	Op string `json:"op"`
	// Desc says what the action did, like "logging 350 cal and 30 g
	// protein", for the reply to "undo".
	Desc string `json:"desc"`
	// ID is the memory, logged amount, or night the action changed, and
	// IDs the amounts log_intake added.
	ID      int64         `json:"id,omitempty"`
	IDs     []int64       `json:"ids,omitempty"`
	Fact    string        `json:"fact,omitempty"`
	Profile *profileState `json:"profile,omitempty"`
	// Day is the local day the action changed, as YYYY-MM-DD: the day of
	// set_goals' change, or the day a logged amount or night was on before
	// it.
	Day   string       `json:"day,omitempty"`
	Goals []goalBefore `json:"goals,omitempty"`
	// Amount is a logged amount's value, or a night's minutes, before an
	// edit, and Bedtime and WakeTime the night's times, in microseconds
	// since midnight.
	Amount   *int32 `json:"amount,omitempty"`
	Bedtime  *int64 `json:"bedtime,omitempty"`
	WakeTime *int64 `json:"wake_time,omitempty"`
	// Metrics are the intake metrics, or sleep, the action changed, whose
	// status for Day the reply to "undo" reports.
	Metrics []string `json:"metrics,omitempty"`
}

// profileState is the profile fields set_profile changes, as they were.
type profileState struct {
	Name     *string `json:"name"`
	Timezone *string `json:"timezone"`
	// Microseconds since midnight.
	WakeTime *int64 `json:"wake_time"`
	Bedtime  *int64 `json:"bedtime"`
}

func profileOf(u store.User) *profileState {
	p := &profileState{}
	if u.Name.Valid {
		p.Name = &u.Name.String
	}
	if u.Timezone.Valid {
		p.Timezone = &u.Timezone.String
	}
	if u.WakeTime.Valid {
		p.WakeTime = &u.WakeTime.Microseconds
	}
	if u.Bedtime.Valid {
		p.Bedtime = &u.Bedtime.Microseconds
	}
	return p
}

// goalBefore is one metric's goal row on the day set_goals changed it.
// Existed is false when the change added the row.
type goalBefore struct {
	Metric  string `json:"metric"`
	Existed bool   `json:"existed"`
	Value   *int32 `json:"value"`
}

func int4(p *int32) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *p, Valid: true}
}

func text(p *string) pgtype.Text {
	if p == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *p, Valid: true}
}

func timeOf(p *int64) pgtype.Time {
	if p == nil {
		return pgtype.Time{}
	}
	return pgtype.Time{Microseconds: *p, Valid: true}
}

func microsOf(t pgtype.Time) *int64 {
	if !t.Valid {
		return nil
	}
	return &t.Microseconds
}

func parseDay(s string) (pgtype.Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return pgtype.Date{}, err
	}
	return pgtype.Date{Time: t, Valid: true}, nil
}

// isUndo reports whether body asks to undo the last change, like "undo" or
// "Undo that."
func isUndo(body string) bool {
	s := strings.TrimRight(strings.ToLower(strings.TrimSpace(body)), ".! ")
	switch s {
	case "undo", "undo that", "undo it", "undo last":
		return true
	}
	return false
}

const nothingToUndo = "Nothing to undo."

// undo reverses the user's newest change that isn't undone yet and returns
// the reply. It runs without the LLM. The change is marked as undone by
// msgID, so running the same text again replies the same way instead of
// undoing a second change.
func (a *Agent) undo(ctx context.Context, userID, msgID int64) (string, error) {
	var reply string
	err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		q := a.Store.WithTx(tx)
		// Undo texts and tool calls for one user take turns, so two undos
		// at once undo two changes instead of the same one twice, and an
		// undo sees every committed change before it.
		if _, err := q.LockUser(ctx, userID); err != nil {
			return err
		}
		by := pgtype.Int8{Int64: msgID, Valid: true}
		act, err := q.UndoneBy(ctx, by)
		if err == nil {
			reply, err = a.undoReply(ctx, q, userID, act)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		act, err = q.NextUndo(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			reply = nothingToUndo
			return nil
		}
		if err != nil {
			return err
		}
		var rec undoRecord
		if err := json.Unmarshal(act.Undo, &rec); err != nil {
			return fmt.Errorf("decoding undo for action %d: %w", act.ID, err)
		}
		if err := a.applyUndo(ctx, q, userID, rec); err != nil {
			return fmt.Errorf("undoing action %d: %w", act.ID, err)
		}
		if err := q.MarkUndone(ctx, store.MarkUndoneParams{ID: act.ID, MessageID: by}); err != nil {
			return err
		}
		reply, err = a.undoReply(ctx, q, userID, act)
		return err
	})
	return reply, err
}

func (a *Agent) applyUndo(ctx context.Context, q *store.Queries, userID int64, rec undoRecord) error {
	switch rec.Op {
	case undoRestoreProfile:
		p := rec.Profile
		if p == nil {
			return errors.New("no profile to restore")
		}
		return q.RestoreProfile(ctx, store.RestoreProfileParams{ID: userID, Name: text(p.Name), Timezone: text(p.Timezone),
			WakeTime: timeOf(p.WakeTime), Bedtime: timeOf(p.Bedtime)})
	case undoDeleteMemory:
		_, err := q.DeleteMemory(ctx, store.DeleteMemoryParams{ID: rec.ID, UserID: userID})
		return err
	case undoSetMemory:
		_, err := q.UpdateMemory(ctx, store.UpdateMemoryParams{ID: rec.ID, UserID: userID, Fact: rec.Fact})
		return err
	case undoRestoreMemory:
		return q.RestoreMemory(ctx, store.RestoreMemoryParams{ID: rec.ID, UserID: userID, Fact: rec.Fact})
	case undoRestoreGoals:
		day, err := parseDay(rec.Day)
		if err != nil {
			return err
		}
		for _, g := range rec.Goals {
			if !g.Existed {
				err = q.DeleteGoal(ctx, store.DeleteGoalParams{UserID: userID, Metric: g.Metric, StartDate: day})
			} else {
				err = q.SetGoal(ctx, store.SetGoalParams{UserID: userID, Metric: g.Metric, Value: int4(g.Value), StartDate: day})
			}
			if err != nil {
				return err
			}
		}
		return nil
	case undoDeleteIntake:
		at := pgtype.Timestamptz{Time: a.now(), Valid: true}
		for _, id := range rec.IDs {
			if _, err := q.SetIntakeDeleted(ctx, store.SetIntakeDeletedParams{ID: id, UserID: userID, DeletedAt: at}); err != nil {
				return err
			}
		}
		return nil
	case undoUndeleteIntake:
		_, err := q.SetIntakeDeleted(ctx, store.SetIntakeDeletedParams{ID: rec.ID, UserID: userID})
		return err
	case undoRestoreIntake:
		if rec.Amount == nil {
			return errors.New("no amount to restore")
		}
		day, err := parseDay(rec.Day)
		if err != nil {
			return err
		}
		_, err = q.UpdateIntake(ctx, store.UpdateIntakeParams{ID: rec.ID, UserID: userID, LocalDate: day, Amount: *rec.Amount})
		return err
	case undoDeleteSleep:
		_, err := q.SetSleepDeleted(ctx, store.SetSleepDeletedParams{ID: rec.ID, UserID: userID,
			DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}})
		return err
	case undoUndeleteSleep:
		_, err := q.SetSleepDeleted(ctx, store.SetSleepDeletedParams{ID: rec.ID, UserID: userID})
		return err
	case undoRestoreSleep:
		if rec.Amount == nil {
			return errors.New("no minutes to restore")
		}
		day, err := parseDay(rec.Day)
		if err != nil {
			return err
		}
		_, err = q.UpdateSleep(ctx, store.UpdateSleepParams{ID: rec.ID, UserID: userID, LocalDate: day, Minutes: *rec.Amount,
			Bedtime: timeOf(rec.Bedtime), WakeTime: timeOf(rec.WakeTime)})
		return err
	}
	return fmt.Errorf("unknown undo op %q", rec.Op)
}

// undoReply says what was undone and, for logged amounts and nights, where
// that day's totals or sleep now stand.
func (a *Agent) undoReply(ctx context.Context, q *store.Queries, userID int64, act store.Action) (string, error) {
	var rec undoRecord
	if err := json.Unmarshal(act.Undo, &rec); err != nil {
		return "", fmt.Errorf("decoding undo for action %d: %w", act.ID, err)
	}
	reply := "Undid " + rec.Desc + "."
	if len(rec.Metrics) == 0 {
		return reply, nil
	}
	day, err := parseDay(rec.Day)
	if err != nil {
		return "", err
	}
	user, err := q.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	if slices.Contains(rec.Metrics, "sleep") {
		st, err := a.sleepStatus(ctx, q, user, day)
		if err != nil {
			return "", err
		}
		return reply + " " + st.forUser(), nil
	}
	st, err := a.dayStatus(ctx, q, user, day)
	if err != nil {
		return "", err
	}
	if status := st.forUser(rec.Metrics); status != "" {
		reply += " " + status
	}
	return reply, nil
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
