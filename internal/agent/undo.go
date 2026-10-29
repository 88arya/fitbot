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
	undoRestoreProfile  = "restore_profile"
	undoDeleteMemory    = "delete_memory"
	undoSetMemory       = "set_memory"
	undoRestoreMemory   = "restore_memory"
	undoRestoreGoals    = "restore_goals"
	undoDeleteIntake    = "delete_intake"
	undoRestoreIntake   = "restore_intake"
	undoUndeleteIntake  = "undelete_intake"
	undoDeleteSleep     = "delete_sleep"
	undoRestoreSleep    = "restore_sleep"
	undoUndeleteSleep   = "undelete_sleep"
	undoRestoreSchedule = "restore_schedule"
	undoRestorePlans    = "restore_plans"
	undoDeleteWorkout   = "delete_workout"
	undoRestoreWorkout  = "restore_workout"
	undoUndeleteWorkout = "undelete_workout"
	// A supplement's plan for a day and whether setup's supplement
	// question was answered, as they were.
	undoRestoreSupplement     = "restore_supplement"
	undoDeleteSupplementLogs  = "delete_supplement_logs"
	undoRestoreSupplementLog  = "restore_supplement_log"
	undoUndeleteSupplementLog = "undelete_supplement_log"
	undoDeleteWeight          = "delete_weight"
	undoRestoreWeight         = "restore_weight"
	undoUndeleteWeight        = "undelete_weight"
)

// undoRecord is what reverses one action, stored with it as JSON. Op says
// how, and the other fields hold what that needs.
type undoRecord struct {
	Op string `json:"op"`
	// Desc says what the action did, like "logging 350 cal and 30 g
	// protein", for the reply to "undo".
	Desc string `json:"desc"`
	// ID is the memory, logged amount, night, workout, supplement,
	// supplement log, or weigh-in the action changed, and IDs the amounts
	// log_intake or the logs log_supplement added.
	ID      int64         `json:"id,omitempty"`
	IDs     []int64       `json:"ids,omitempty"`
	Fact    string        `json:"fact,omitempty"`
	Profile *profileState `json:"profile,omitempty"`
	// Day is the local day the action changed, as YYYY-MM-DD: the day of
	// set_goals' or set_schedule's change, or the day a logged amount,
	// night, workout, supplement log, or weigh-in was on before it.
	Day   string       `json:"day,omitempty"`
	Goals []goalBefore `json:"goals,omitempty"`
	// Amount is a logged amount's value, a night's minutes, or a
	// weigh-in's grams, before an edit, and Bedtime and WakeTime the
	// night's times, in microseconds since midnight.
	Amount   *int32 `json:"amount,omitempty"`
	Bedtime  *int64 `json:"bedtime,omitempty"`
	WakeTime *int64 `json:"wake_time,omitempty"`
	// Status is a workout's or supplement log's before an edit, and
	// Sessions a workout's.
	Status   *string  `json:"status,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
	// Plan is a supplement's plan row on Day before set_supplement, and
	// Answered whether setup's supplement question was answered.
	Plan     *supplementPlanState `json:"plan,omitempty"`
	Answered *bool                `json:"answered,omitempty"`
	// Metrics are the intake metrics, sleep, workout, supplement, or weight
	// the action changed, whose status for Day the reply to "undo" reports.
	Metrics []string `json:"metrics,omitempty"`
	// Schedule is the schedule set_schedule replaced on Day, if one started
	// that day, and Plans the day plans a change touched, as they were.
	// Days are the days, oldest first, whose plan plan_session showed, for
	// the reply to "undo".
	Schedule *scheduleState `json:"schedule,omitempty"`
	Plans    []planState    `json:"plans,omitempty"`
	Days     []string       `json:"days,omitempty"`
}

// profileState is the profile fields set_profile changes, as they were.
type profileState struct {
	Name     *string `json:"name"`
	Timezone *string `json:"timezone"`
	// Microseconds since midnight.
	WakeTime       *int64     `json:"wake_time"`
	Bedtime        *int64     `json:"bedtime"`
	MorningCheckin *int64     `json:"morning_checkin"`
	NightCheckin   *int64     `json:"night_checkin"`
	MutedUntil     *time.Time `json:"muted_until"`
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
	p.MorningCheckin, p.NightCheckin = microsOf(u.MorningCheckin), microsOf(u.NightCheckin)
	if u.MutedUntil.Valid {
		p.MutedUntil = &u.MutedUntil.Time
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
		muted := pgtype.Timestamptz{}
		if p.MutedUntil != nil {
			muted = pgtype.Timestamptz{Time: *p.MutedUntil, Valid: true}
		}
		return q.RestoreProfile(ctx, store.RestoreProfileParams{ID: userID, Name: text(p.Name), Timezone: text(p.Timezone),
			WakeTime: timeOf(p.WakeTime), Bedtime: timeOf(p.Bedtime), MorningCheckin: timeOf(p.MorningCheckin),
			NightCheckin: timeOf(p.NightCheckin), MutedUntil: muted})
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
	case undoDeleteWorkout:
		_, err := q.SetWorkoutDeleted(ctx, store.SetWorkoutDeletedParams{ID: rec.ID, UserID: userID,
			DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}})
		return err
	case undoUndeleteWorkout:
		_, err := q.SetWorkoutDeleted(ctx, store.SetWorkoutDeletedParams{ID: rec.ID, UserID: userID})
		return err
	case undoRestoreWorkout:
		if rec.Status == nil || len(rec.Sessions) == 0 {
			return errors.New("no workout to restore")
		}
		day, err := parseDay(rec.Day)
		if err != nil {
			return err
		}
		_, err = q.UpdateWorkout(ctx, store.UpdateWorkoutParams{ID: rec.ID, UserID: userID, LocalDate: day, Status: *rec.Status,
			Sessions: rec.Sessions})
		return err
	case undoRestoreSupplement:
		if rec.Answered == nil {
			return errors.New("no supplement state to restore")
		}
		if p := rec.Plan; p != nil {
			day, err := parseDay(rec.Day)
			if err != nil {
				return err
			}
			if p.Existed {
				err = q.SetSupplementPlan(ctx, store.SetSupplementPlanParams{SupplementID: rec.ID, StartDate: day, Active: p.Active,
					Dose: text(p.Dose), TakeAt: timeOf(p.TakeAt), Anchor: text(p.Anchor), Days: p.Days})
			} else {
				err = q.DeleteSupplementPlan(ctx, store.DeleteSupplementPlanParams{SupplementID: rec.ID, StartDate: day})
			}
			if err != nil {
				return err
			}
		}
		return q.SetSupplementsAnswered(ctx, store.SetSupplementsAnsweredParams{ID: userID, Answered: *rec.Answered})
	case undoDeleteSupplementLogs:
		at := pgtype.Timestamptz{Time: a.now(), Valid: true}
		for _, id := range rec.IDs {
			if _, err := q.SetSupplementLogDeleted(ctx, store.SetSupplementLogDeletedParams{ID: id, UserID: userID, DeletedAt: at}); err != nil {
				return err
			}
		}
		return nil
	case undoUndeleteSupplementLog:
		_, err := q.SetSupplementLogDeleted(ctx, store.SetSupplementLogDeletedParams{ID: rec.ID, UserID: userID})
		return err
	case undoRestoreSupplementLog:
		if rec.Status == nil {
			return errors.New("no supplement log to restore")
		}
		day, err := parseDay(rec.Day)
		if err != nil {
			return err
		}
		_, err = q.UpdateSupplementLog(ctx, store.UpdateSupplementLogParams{ID: rec.ID, UserID: userID, LocalDate: day, Status: *rec.Status})
		return err
	case undoDeleteWeight:
		_, err := q.SetWeightDeleted(ctx, store.SetWeightDeletedParams{ID: rec.ID, UserID: userID,
			DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}})
		return err
	case undoUndeleteWeight:
		_, err := q.SetWeightDeleted(ctx, store.SetWeightDeletedParams{ID: rec.ID, UserID: userID})
		return err
	case undoRestoreWeight:
		if rec.Amount == nil {
			return errors.New("no weight to restore")
		}
		day, err := parseDay(rec.Day)
		if err != nil {
			return err
		}
		_, err = q.UpdateWeight(ctx, store.UpdateWeightParams{ID: rec.ID, UserID: userID, LocalDate: day, Grams: *rec.Amount})
		return err
	case undoRestoreSchedule:
		day, err := parseDay(rec.Day)
		if err != nil {
			return err
		}
		if err := q.DeleteScheduleOnDay(ctx, store.DeleteScheduleOnDayParams{UserID: userID, StartDate: day}); err != nil {
			return err
		}
		if rec.Schedule != nil {
			if err := insertSchedule(ctx, q, userID, day, *rec.Schedule); err != nil {
				return err
			}
		}
		return restorePlans(ctx, q, userID, rec.Plans)
	case undoRestorePlans:
		return restorePlans(ctx, q, userID, rec.Plans)
	}
	return fmt.Errorf("unknown undo op %q", rec.Op)
}

// undoReply says what was undone and, for logged amounts, nights,
// workouts, supplements, weigh-ins, and training plans, where they now
// stand.
func (a *Agent) undoReply(ctx context.Context, q *store.Queries, userID int64, act store.Action) (string, error) {
	var rec undoRecord
	if err := json.Unmarshal(act.Undo, &rec); err != nil {
		return "", fmt.Errorf("decoding undo for action %d: %w", act.ID, err)
	}
	reply := "Undid " + rec.Desc + "."
	if rec.Op == undoRestoreSchedule || rec.Op == undoRestorePlans {
		status, err := a.trainingUndoReply(ctx, q, userID, rec)
		if err != nil || status == "" {
			return reply, err
		}
		return reply + " " + status, nil
	}
	if rec.Op == undoRestoreSupplement {
		status, err := a.supplementUndoReply(ctx, q, userID)
		if err != nil {
			return "", err
		}
		return reply + " " + status, nil
	}
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
	if slices.Contains(rec.Metrics, "supplement") {
		st, err := a.supplementStatus(ctx, q, user, day)
		if err != nil {
			return "", err
		}
		return reply + " " + st.forUser(), nil
	}
	if slices.Contains(rec.Metrics, "weight") {
		st, err := a.weightStatus(ctx, q, user, day)
		if err != nil {
			return "", err
		}
		return reply + " " + st.forUser(), nil
	}
	if slices.Contains(rec.Metrics, "workout") {
		st, err := a.workoutStatus(ctx, q, user, day)
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
