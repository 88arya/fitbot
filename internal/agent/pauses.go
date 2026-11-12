package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5/pgtype"
)

// A pause is a break like a vacation or a sick week. Its days plan no
// sessions, aren't judged against goals, and neither count toward nor break
// a streak. Only supplement reminders go out on them, and a weekly review
// covering one changes nothing.

const (
	// pauseBackdate is how far back a pause can start, for "I've been sick
	// since Monday", and pauseAhead how far ahead.
	pauseBackdate = 7
	pauseAhead    = 90
	// maxPauseDays is the longest one pause can be.
	maxPauseDays = 31
	// maxReason is the longest a pause's reason can be.
	maxReason = 40
)

// pauseSet is a user's pauses, oldest first.
type pauseSet []store.Pause

func loadPauses(ctx context.Context, q *store.Queries, userID int64, first, last pgtype.Date) (pauseSet, error) {
	return q.PausesBetween(ctx, store.PausesBetweenParams{UserID: userID, First: first, Last: last})
}

// on is the pause covering d, if any.
func (ps pauseSet) on(d pgtype.Date) (store.Pause, bool) {
	for _, p := range ps {
		if !d.Time.Before(p.FirstDate.Time) && !d.Time.After(p.LastDate.Time) {
			return p, true
		}
	}
	return store.Pause{}, false
}

// count is how many days from first to last are paused.
func (ps pauseSet) count(first, last pgtype.Date) int {
	n := 0
	for d := first; !d.Time.After(last.Time); d = addDays(d, 1) {
		if _, ok := ps.on(d); ok {
			n++
		}
	}
	return n
}

// pauseText shows a pause, like "Nov 12 to Nov 15 (vacation)".
func pauseText(p store.Pause) string {
	s := shortDate(p.FirstDate)
	if !p.LastDate.Time.Equal(p.FirstDate.Time) {
		s += " to " + shortDate(p.LastDate)
	}
	if p.Reason.Valid {
		s += " (" + p.Reason.String + ")"
	}
	return s
}

type pauseArgs struct {
	Action    string  `json:"action"`
	FirstDate *string `json:"first_date"`
	LastDate  *string `json:"last_date"`
	Reason    *string `json:"reason"`
}

func (a *Agent) setPause(ctx context.Context, c *call, args pauseArgs) (string, error) {
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	switch args.Action {
	case "set":
		return a.startPause(ctx, c, args, today)
	case "end":
		if args.LastDate != nil || args.Reason != nil {
			return "error: end takes only first_date, to pick which pause", nil
		}
		return a.endPause(ctx, c, args.FirstDate, today)
	}
	return fmt.Sprintf("error: unknown action %q", args.Action), nil
}

func (a *Agent) startPause(ctx context.Context, c *call, args pauseArgs, today pgtype.Date) (string, error) {
	first := today
	if args.FirstDate != nil {
		d, err := parseDay(strings.TrimSpace(*args.FirstDate))
		if err != nil {
			return fmt.Sprintf("error: first_date %q is not YYYY-MM-DD", *args.FirstDate), nil
		}
		first = d
	}
	if args.LastDate == nil {
		return "error: last_date is needed; ask the user when they'll be back", nil
	}
	last, err := parseDay(strings.TrimSpace(*args.LastDate))
	if err != nil {
		return fmt.Sprintf("error: last_date %q is not YYYY-MM-DD", *args.LastDate), nil
	}
	switch {
	case first.Time.Before(addDays(today, -pauseBackdate).Time):
		return fmt.Sprintf("error: a pause can start at most %d days ago; today is %s", pauseBackdate, today.Time.Format(time.DateOnly)), nil
	case first.Time.After(addDays(today, pauseAhead).Time):
		return fmt.Sprintf("error: a pause can start at most %d days ahead", pauseAhead), nil
	case last.Time.Before(first.Time):
		return "error: last_date is before first_date", nil
	case daysBetween(first, last) >= maxPauseDays:
		return fmt.Sprintf("error: a pause can be at most %d days", maxPauseDays), nil
	}
	reason := pgtype.Text{}
	if args.Reason != nil {
		r := strings.Join(strings.Fields(strings.ToLower(*args.Reason)), " ")
		if len(r) > maxReason {
			return fmt.Sprintf("error: reason must be at most %d characters", maxReason), nil
		}
		reason = pgtype.Text{String: r, Valid: r != ""}
	}

	// This pause replaces what overlaps it from first on: a pause starting
	// earlier keeps its days before first, and any other overlapping one
	// goes. So "til the 15th instead" with the same start replaces it.
	old, err := loadPauses(ctx, c.q, c.userID, first, last)
	if err != nil {
		return "", err
	}
	undo := &undoRecord{Op: undoRestorePause}
	var replaced []string
	for _, p := range old {
		if p.FirstDate.Time.Before(first.Time) {
			if _, err := c.q.SetPauseLast(ctx, store.SetPauseLastParams{ID: p.ID, UserID: c.userID, LastDate: addDays(first, -1)}); err != nil {
				return "", err
			}
			undo.Prev, undo.Day = p.ID, p.LastDate.Time.Format(time.DateOnly)
			replaced = append(replaced, pauseText(p)+" from "+shortDate(first)+" on")
			continue
		}
		if _, err := c.q.SetPauseDeleted(ctx, store.SetPauseDeletedParams{ID: p.ID, UserID: c.userID,
			DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}}); err != nil {
			return "", err
		}
		undo.IDs = append(undo.IDs, p.ID)
		replaced = append(replaced, pauseText(p))
	}
	p, err := c.q.InsertPause(ctx, store.InsertPauseParams{UserID: c.userID, FirstDate: first, LastDate: last, Reason: reason})
	if err != nil {
		return "", err
	}
	undo.ID = p.ID
	undo.Desc = "pausing " + pauseText(p)
	c.undo = undo
	back := "Back on " + dayName(addDays(last, 1)) + "."
	if last.Time.Before(today.Time) {
		back = "It's already over, so it only changes how those days count."
	}
	result := fmt.Sprintf("paused %s. Those days plan no sessions, aren't judged against goals, and don't break streaks; only "+
		"supplement reminders go out. %s", pauseText(p), back)
	if len(replaced) > 0 {
		result += " It replaces " + strings.Join(replaced, ", ") + "."
	}
	return result, nil
}

func (a *Agent) endPause(ctx context.Context, c *call, on *string, today pgtype.Date) (string, error) {
	day := today
	if on != nil {
		d, err := parseDay(strings.TrimSpace(*on))
		if err != nil {
			return fmt.Sprintf("error: first_date %q is not YYYY-MM-DD", *on), nil
		}
		day = d
	}
	// The pause covering the day, or else the next one after it.
	ps, err := loadPauses(ctx, c.q, c.userID, day, addDays(day, pauseAhead+maxPauseDays))
	if err != nil {
		return "", err
	}
	if len(ps) == 0 {
		return "error: no pause covers that day or comes after it", nil
	}
	p := ps[0]
	if p.LastDate.Time.Before(today.Time) {
		return "error: that pause is already over", nil
	}
	if !p.FirstDate.Time.Before(today.Time) {
		// It hasn't started, or starts today: it goes entirely.
		if _, err := c.q.SetPauseDeleted(ctx, store.SetPauseDeletedParams{ID: p.ID, UserID: c.userID,
			DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}}); err != nil {
			return "", err
		}
		c.undo = &undoRecord{Op: undoRestorePause, Desc: "cancelling the pause " + pauseText(p), IDs: []int64{p.ID}}
		return "cancelled the pause " + pauseText(p) + ". Every day counts as usual.", nil
	}
	yesterday := addDays(today, -1)
	if _, err := c.q.SetPauseLast(ctx, store.SetPauseLastParams{ID: p.ID, UserID: c.userID, LastDate: yesterday}); err != nil {
		return "", err
	}
	c.undo = &undoRecord{Op: undoRestorePause, Desc: "ending the pause " + pauseText(p) + " early", Prev: p.ID,
		Day: p.LastDate.Time.Format(time.DateOnly)}
	p.LastDate = yesterday
	return "ended the pause; it now covers " + pauseText(p) + ". Today counts as usual again.", nil
}

// restorePause reverses set_pause: the pause it added is removed, the ones
// it replaced or cancelled are back, and one it ended early ends on Day
// again.
func (a *Agent) restorePause(ctx context.Context, q *store.Queries, userID int64, rec undoRecord) error {
	if rec.ID != 0 {
		if _, err := q.SetPauseDeleted(ctx, store.SetPauseDeletedParams{ID: rec.ID, UserID: userID,
			DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}}); err != nil {
			return err
		}
	}
	for _, id := range rec.IDs {
		if _, err := q.SetPauseDeleted(ctx, store.SetPauseDeletedParams{ID: id, UserID: userID}); err != nil {
			return err
		}
	}
	if rec.Prev != 0 {
		day, err := parseDay(rec.Day)
		if err != nil {
			return err
		}
		if _, err := q.SetPauseLast(ctx, store.SetPauseLastParams{ID: rec.Prev, UserID: userID, LastDate: day}); err != nil {
			return err
		}
	}
	return nil
}

// pauseSection is the prompt's current and upcoming pauses.
func (a *Agent) pauseSection(ctx context.Context, user store.User) (string, error) {
	today := a.localDate(user)
	ps, err := loadPauses(ctx, a.Store, user.ID, addDays(today, -pauseBackdate), addDays(today, pauseAhead+maxPauseDays))
	if err != nil {
		return "", fmt.Errorf("loading pauses: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n## Pauses\n")
	if p, ok := ps.on(today); ok {
		fmt.Fprintf(&b, "Paused today: %s.\n", pauseText(p))
	}
	var lines []string
	for _, p := range ps {
		lines = append(lines, pauseText(p))
	}
	if len(lines) == 0 {
		b.WriteString("(none)\n")
	} else {
		b.WriteString("Recent and upcoming: " + strings.Join(lines, "; ") + "\n")
	}
	b.WriteString("For a break like a vacation, a sick week, or a deload, use set_pause with the dates the user gave, asking " +
		"when they'll be back if they didn't say. Paused days plan no sessions, aren't judged against goals, and neither count " +
		"toward nor break streaks; only supplement reminders go out, and the state and targets don't change. Logging still " +
		"works. When they're back early, end it with set_pause action end. A pause is never a reason to change the state.\n")
	return b.String(), nil
}
