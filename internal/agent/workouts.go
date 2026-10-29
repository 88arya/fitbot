package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// workoutStatuses are what a workout log records for its sessions.
var workoutStatuses = []string{"done", "skipped"}

// unnamedWorkout names a workout on a day with nothing planned when the user
// didn't say what it was.
const unnamedWorkout = "workout"

type workoutArgs struct {
	Status   string   `json:"status"`
	Sessions []string `json:"sessions"`
	Date     *string  `json:"date"`
}

func (a *Agent) logWorkout(ctx context.Context, c *call, args workoutArgs) (string, error) {
	if !slices.Contains(workoutStatuses, args.Status) {
		return fmt.Sprintf("error: status %q must be done or skipped", args.Status), nil
	}
	named, errText := workoutSessions(args.Sessions)
	if errText != "" {
		return "error: " + errText, nil
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	day := today
	if args.Date != nil {
		if day, errText = entryDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	r, err := loadWorkouts(ctx, c.q, c.userID, day, day)
	if err != nil {
		return "", err
	}
	sessions, errText := r.day(day).pick(args.Status, named, 0)
	if errText != "" {
		return "error: " + errText, nil
	}
	row, err := c.q.InsertWorkout(ctx, store.InsertWorkoutParams{
		UserID:    c.userID,
		ActionID:  c.actionID,
		MessageID: c.msgID,
		LocalDate: day,
		Status:    args.Status,
		Sessions:  sessions,
		LoggedAt:  pgtype.Timestamptz{Time: a.now(), Valid: true},
	})
	if err != nil {
		return "", err
	}
	summary := workoutSummary(row)
	c.undo = &undoRecord{Op: undoDeleteWorkout, Desc: "logging " + summary + " on " + dayName(day), ID: row.ID,
		Day: day.Time.Format(time.DateOnly), Metrics: []string{"workout"}}
	st, err := a.workoutStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("logged #%d: %s %s. %s", row.ID, summary, dayWord(day, today), st.forModel()), nil
}

func (a *Agent) editWorkout(ctx context.Context, c *call, args editLogArgs) (string, error) {
	if args.Amount != nil || args.Unit != nil || args.Bedtime != nil || args.WakeTime != nil {
		return "error: a workout has no amount or times; change its status, sessions, or date", nil
	}
	if args.Status == nil && args.Sessions == nil && args.Date == nil {
		return "error: no changes given", nil
	}
	old, errText, err := targetWorkout(ctx, c, args.ID)
	if errText != "" || err != nil {
		return errText, err
	}
	status, sessions := old.Status, old.Sessions
	if args.Status != nil {
		if status = *args.Status; !slices.Contains(workoutStatuses, status) {
			return fmt.Sprintf("error: status %q must be done or skipped", status), nil
		}
	}
	if args.Sessions != nil {
		named, errText := workoutSessions(args.Sessions)
		switch {
		case errText != "":
			return "error: " + errText, nil
		case named == nil:
			return "error: sessions must name what was done or skipped, or be null to keep them", nil
		}
		sessions = named
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	day := old.LocalDate
	if args.Date != nil {
		if day, errText = entryDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	moved := !day.Time.Equal(old.LocalDate.Time)
	first, last := day, old.LocalDate
	if last.Time.Before(first.Time) {
		first, last = last, first
	}
	r, err := loadWorkouts(ctx, c.q, c.userID, first, last)
	if err != nil {
		return "", err
	}
	// A log of its day's planned sessions moved to another day, like "that
	// was yesterday", covers that day's planned sessions instead.
	named := sessions
	if moved && args.Sessions == nil && allIn(old.Sessions, r.day(old.LocalDate).planned) {
		named = nil
	}
	if sessions, errText = r.day(day).pick(status, named, old.ID); errText != "" {
		return "error: " + errText, nil
	}
	if _, err := c.q.UpdateWorkout(ctx, store.UpdateWorkoutParams{ID: old.ID, UserID: c.userID, LocalDate: day, Status: status,
		Sessions: sessions}); err != nil {
		return "", err
	}

	updated := old
	updated.LocalDate, updated.Status, updated.Sessions = day, status, sessions
	was, now := workoutSummary(old), workoutSummary(updated)
	var desc string
	switch {
	case moved && was == now:
		desc = fmt.Sprintf("moving %s from %s to %s", was, dayName(old.LocalDate), dayName(day))
	case moved:
		desc = fmt.Sprintf("changing %s to %s and moving it from %s to %s", was, now, dayName(old.LocalDate), dayName(day))
	default:
		desc = fmt.Sprintf("changing %s to %s on %s", was, now, dayName(day))
	}
	oldStatus := old.Status
	c.undo = &undoRecord{Op: undoRestoreWorkout, Desc: desc, ID: old.ID, Status: &oldStatus, Sessions: old.Sessions,
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{"workout"}}

	st, err := a.workoutStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	result := fmt.Sprintf("updated #%d: now %s %s. %s", old.ID, now, dayWord(day, today), st.day)
	weeks := []string{st.week}
	if moved {
		prev, err := a.workoutStatus(ctx, c.q, user, old.LocalDate)
		if err != nil {
			return "", err
		}
		result += "; " + prev.day
		if prev.week != st.week {
			weeks = append(weeks, prev.week)
		}
	}
	return result + "; " + strings.Join(weeks, "; ") + ".", nil
}

func (a *Agent) deleteWorkout(ctx context.Context, c *call, id *int64) (string, error) {
	old, errText, err := targetWorkout(ctx, c, id)
	if errText != "" || err != nil {
		return errText, err
	}
	if _, err := c.q.SetWorkoutDeleted(ctx, store.SetWorkoutDeletedParams{ID: old.ID, UserID: c.userID,
		DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}}); err != nil {
		return "", err
	}
	summary := workoutSummary(old)
	c.undo = &undoRecord{Op: undoUndeleteWorkout, Desc: "deleting " + summary + " on " + dayName(old.LocalDate), ID: old.ID,
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{"workout"}}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	st, err := a.workoutStatus(ctx, c.q, user, old.LocalDate)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted #%d: %s %s. %s", old.ID, summary, dayWord(old.LocalDate, a.localDate(user)), st.forModel()), nil
}

// targetWorkout finds the workout with id, or the latest one when id is
// nil, or returns an error for the model.
func targetWorkout(ctx context.Context, c *call, id *int64) (store.Workout, string, error) {
	var row store.Workout
	var err error
	if id == nil {
		row, err = c.q.LastWorkout(ctx, c.userID)
	} else {
		row, err = c.q.GetWorkout(ctx, store.GetWorkoutParams{ID: *id, UserID: c.userID})
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows) && id == nil:
		return row, "error: no workout logged", nil
	case errors.Is(err, pgx.ErrNoRows):
		return row, fmt.Sprintf("error: no workout #%d", *id), nil
	}
	return row, "", err
}

// workoutSessions normalizes the session names a log gives, or returns why
// they aren't sessions. None named comes back nil.
func workoutSessions(names []string) ([]string, string) {
	if len(names) == 0 {
		return nil, ""
	}
	out, errText := cleanSessions(names)
	if errText != "" {
		return nil, errText
	}
	if len(out) == 0 {
		return nil, "rest is not a session; a rest day needs no log"
	}
	return out, ""
}

// allIn reports whether every name is in list.
func allIn(names, list []string) bool {
	for _, n := range names {
		if !slices.Contains(list, n) {
			return false
		}
	}
	return true
}

// workoutSummary shows a log, like "legs + cardio done".
func workoutSummary(w store.Workout) string {
	return strings.Join(w.Sessions, " + ") + " " + w.Status
}

// workoutRange is the training plan and the workouts logged for a range of
// days.
type workoutRange struct {
	t    *training
	logs map[string][]store.Workout
}

func loadWorkouts(ctx context.Context, q *store.Queries, userID int64, first, last pgtype.Date) (*workoutRange, error) {
	t, err := loadTraining(ctx, q, userID, first, last)
	if err != nil {
		return nil, err
	}
	rows, err := q.WorkoutsBetween(ctx, store.WorkoutsBetweenParams{UserID: userID, First: first, Last: last})
	if err != nil {
		return nil, err
	}
	r := &workoutRange{t: t, logs: map[string][]store.Workout{}}
	for _, w := range rows {
		r.logs[dateKey(w.LocalDate)] = append(r.logs[dateKey(w.LocalDate)], w)
	}
	return r, nil
}

// workoutDay is one day's plan and the workouts logged for it.
type workoutDay struct {
	day  pgtype.Date
	plan dayPlan
	// planned are the sessions planned on the day, including ones skipped
	// with plan_session. A moved session is planned on the day it moved to.
	planned []string
	logs    []store.Workout
}

func (r *workoutRange) day(d pgtype.Date) workoutDay {
	p := r.t.on(d)
	w := workoutDay{day: d, plan: p, logs: r.logs[dateKey(d)]}
	if p.sched != nil && p.sched.Kind.Valid && !p.pausedUntil.Valid && p.status != "moved" {
		w.planned = p.all()
	}
	return w
}

// pick works out the sessions a log of status covers: named, or with none
// named, the planned sessions not logged yet, or an unnamed workout on a
// day with nothing planned. It returns why not when a session is already
// logged, a skip names a session that isn't planned, or nothing planned is
// left. The log with id is left out, for an edit.
func (w workoutDay) pick(status string, named []string, id int64) ([]string, string) {
	logged := map[string]store.Workout{}
	for _, l := range w.logs {
		if l.ID == id {
			continue
		}
		for _, s := range l.Sessions {
			logged[s] = l
		}
	}
	when := dayName(w.day)
	if named == nil {
		for _, s := range w.planned {
			if _, ok := logged[s]; !ok {
				named = append(named, s)
			}
		}
		switch {
		case len(named) > 0:
		case len(w.planned) > 0:
			return nil, fmt.Sprintf("everything planned on %s is already logged; name the sessions of an extra workout, "+
				"or change a log with edit_log", when)
		case status == "skipped":
			return nil, fmt.Sprintf("nothing is planned on %s (%s), so there is nothing to skip", when, w.plan.describe())
		default:
			named = []string{unnamedWorkout}
		}
	}
	for _, s := range named {
		if l, ok := logged[s]; ok {
			return nil, fmt.Sprintf("%s on %s is already logged as #%d (%s); change it with edit_log instead", s, when, l.ID,
				workoutSummary(l))
		}
		if status == "skipped" && !slices.Contains(w.planned, s) {
			return nil, fmt.Sprintf("%s isn't planned on %s (%s), so it can't be skipped", s, when, w.plan.describe())
		}
	}
	return named, ""
}

// Session outcomes. A planned session with no log is missed once its day
// is over, and open until then.
const (
	sessionDone    = "done"
	sessionSkipped = "skipped"
	sessionMissed  = "missed"
	sessionOpen    = "open"
)

// sessionOutcome is what became of one planned session.
type sessionOutcome struct {
	name, state string
}

// outcomes is what became of each planned session, in plan order, and the
// sessions done that weren't planned, in the order logged. A logged session
// counts as logged even on a day skipped with plan_session.
func (w workoutDay) outcomes(today pgtype.Date) (planned []sessionOutcome, extras []string) {
	logged := map[string]string{}
	for _, l := range w.logs {
		for _, s := range l.Sessions {
			logged[s] = l.Status
			if l.Status == sessionDone && !slices.Contains(w.planned, s) {
				extras = append(extras, s)
			}
		}
	}
	for _, s := range w.planned {
		state, ok := logged[s]
		switch {
		case ok:
		case w.plan.status == "skipped":
			state = sessionSkipped
		case w.day.Time.Before(today.Time):
			state = sessionMissed
		default:
			state = sessionOpen
		}
		planned = append(planned, sessionOutcome{s, state})
	}
	return planned, extras
}

// describe shows the day's sessions, like "legs done, cardio not logged
// yet", "rest day, cardio done (extra)", or "push missed".
func (w workoutDay) describe(today pgtype.Date) string {
	planned, extras := w.outcomes(today)
	var parts []string
	for _, p := range planned {
		if p.state == sessionOpen {
			parts = append(parts, p.name+" not logged yet")
		} else {
			parts = append(parts, p.name+" "+p.state)
		}
	}
	if len(planned) == 0 {
		p := w.plan
		switch {
		case p.sched == nil || !p.sched.Kind.Valid:
			parts = append(parts, "nothing planned")
		case p.pausedUntil.Valid:
			parts = append(parts, "nothing planned, the rotation waits for the session moved to "+dayName(p.pausedUntil))
		case p.status == "moved":
			parts = append(parts, "nothing planned, "+strings.Join(p.sessions, " + ")+" moved to "+dayName(p.movedTo))
		default:
			parts = append(parts, "rest day")
		}
	}
	for _, s := range extras {
		parts = append(parts, s+" done (extra)")
	}
	return strings.Join(parts, ", ")
}

// weekStart is the Monday on or before d.
func weekStart(d pgtype.Date) pgtype.Date {
	return addDays(d, -((int(d.Time.Weekday()) + 6) % 7))
}

// week counts the planned sessions of the Monday-to-Sunday week starting
// first, like "week of Oct 26: 3 of 6 planned sessions done, 1 skipped, 1
// missed, 1 to go, plus 1 extra session". Sessions planned today or later
// and not logged are to go. The range must cover the week.
func (r *workoutRange) week(first, today pgtype.Date) string {
	c, _, _ := r.count(first, addDays(first, 6), today, "")
	return "week of " + shortDate(first) + ": " + c.String()
}

// plural is n and noun, with an s unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// workoutStatus is a day's sessions and its week, like "workouts today:
// legs done" and "week of Oct 26: 1 of 6 planned sessions done, 5 to go".
type workoutStatus struct {
	day, week string
}

func (a *Agent) workoutStatus(ctx context.Context, q *store.Queries, user store.User, day pgtype.Date) (workoutStatus, error) {
	today := a.localDate(user)
	first := weekStart(day)
	r, err := loadWorkouts(ctx, q, user.ID, first, addDays(first, 6))
	if err != nil {
		return workoutStatus{}, err
	}
	return workoutStatus{
		day:  "workouts " + dayWord(day, today) + ": " + r.day(day).describe(today),
		week: r.week(first, today),
	}, nil
}

func (s workoutStatus) forModel() string {
	return s.day + "; " + s.week + "."
}

// forUser is the status for a text, like "Workouts today: legs done. Week
// of Oct 26: 1 of 6 planned sessions done, 5 to go."
func (s workoutStatus) forUser() string {
	return upperFirst(s.day) + ". " + upperFirst(s.week) + "."
}

// workoutSection is the prompt's list of the last week's workouts, with ids
// for edit_log and delete_log, and where today and this week stand.
func (a *Agent) workoutSection(ctx context.Context, user store.User) (string, error) {
	today := a.localDate(user)
	// The range covers the last 7 days, for the list, and this week.
	first := addDays(today, -6)
	if monday := weekStart(today); monday.Time.Before(first.Time) {
		first = monday
	}
	r, err := loadWorkouts(ctx, a.Store, user.ID, first, addDays(weekStart(today), 6))
	if err != nil {
		return "", fmt.Errorf("loading workouts: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n## Workouts\nWorkouts logged in the last week, newest first:\n")
	n := 0
	for d := today; !d.Time.Before(addDays(today, -6).Time); d = addDays(d, -1) {
		logs := r.logs[dateKey(d)]
		for i := len(logs) - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "#%d %s: %s\n", logs[i].ID, dayLabel(d, today), workoutSummary(logs[i]))
			n++
		}
	}
	if n == 0 {
		b.WriteString("(none yet)\n")
	}
	fmt.Fprintf(&b, "Status: workouts today: %s; %s.\n", r.day(today).describe(today), r.week(weekStart(today), today))
	b.WriteString("Log whether the user worked out with log_workout: done for \"hit the gym\" or \"did legs\", skipped for " +
		"\"skipped the gym\" or \"didn't go\". Leave sessions null to cover the day's planned sessions from Training above. Name " +
		"sessions, with the schedule's names, only when the user said which: [\"legs\"] for \"did legs\" on a legs + cardio day, " +
		"or what they did on a day with nothing planned, which counts as extra. date null for today, or the day it happened. " +
		"A planned session with nothing logged by the end of its day counts as missed. What happened today or earlier is " +
		"logged with log_workout; plan_session is only for changing the plan ahead. If the user trains today after moving " +
		"today's session to a later day, clear the move with plan_session before logging it. Fix a log with edit_log type workout " +
		"(status, sessions, or date), like \"I did go yesterday\" when yesterday is logged skipped; if nothing is logged for " +
		"that day, use log_workout with its date instead. Remove one with delete_log, by its id above or null for the latest. " +
		"Reply to a log with the status in the tool result.\n")
	return b.String(), nil
}
