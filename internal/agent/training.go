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

const (
	// planAhead is how many days after today plan_session can change.
	planAhead = 14
	// maxMove is how many days later a session can move.
	maxMove = 7
	// maxSessions caps the sessions on one day, and maxSessionName a
	// session's name.
	maxSessions    = 4
	maxSessionName = 30
	// maxCycle is the longest rotation, in days.
	maxCycle = 28
)

// weekdays names weekday slots, 0 being Sunday, as set_schedule takes them.
var weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// weekOrder lists weekday slots from Monday, the order schedules are shown in.
var weekOrder = []int32{1, 2, 3, 4, 5, 6, 0}

// training is the schedules and day plans needed to work out the planned
// sessions for a range of days.
type training struct {
	userID int64
	// schedules are oldest first, and days each schedule's days by slot.
	schedules []store.TrainingSchedule
	days      map[int64]map[int32]store.ScheduleDay
	// plans are the range's day plans by date, and moves every move that
	// can pause a rotation in force in the range.
	plans map[string]store.SessionPlan
	moves []store.SessionPlan
}

// loadTraining loads what working out the plan for first to last needs.
func loadTraining(ctx context.Context, q *store.Queries, userID int64, first, last pgtype.Date) (*training, error) {
	t := &training{userID: userID, days: map[int64]map[int32]store.ScheduleDay{}, plans: map[string]store.SessionPlan{}}
	var err error
	if t.schedules, err = q.SchedulesThrough(ctx, store.SchedulesThroughParams{UserID: userID, Last: last}); err != nil {
		return nil, err
	}
	if len(t.schedules) == 0 {
		return t, nil
	}
	ids := make([]int64, len(t.schedules))
	for i, s := range t.schedules {
		ids[i] = s.ID
	}
	days, err := q.ScheduleDays(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, d := range days {
		if t.days[d.ScheduleID] == nil {
			t.days[d.ScheduleID] = map[int32]store.ScheduleDay{}
		}
		t.days[d.ScheduleID][d.Slot] = d
	}
	plans, err := q.PlansBetween(ctx, store.PlansBetweenParams{UserID: userID, First: first, Last: last})
	if err != nil {
		return nil, err
	}
	for _, p := range plans {
		t.plans[dateKey(p.LocalDate)] = p
	}
	t.moves, err = q.MovesBefore(ctx, store.MovesBeforeParams{UserID: userID, Day: addDays(last, 1), Since: t.schedules[0].StartDate})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// scheduleOn is the schedule in force on d, or nil before the first one.
func (t *training) scheduleOn(d pgtype.Date) *store.TrainingSchedule {
	var out *store.TrainingSchedule
	for i := range t.schedules {
		if t.schedules[i].StartDate.Time.After(d.Time) {
			break
		}
		out = &t.schedules[i]
	}
	return out
}

// row is d's plan, or an empty one when d has none.
func (t *training) row(d pgtype.Date) store.SessionPlan {
	if p, ok := t.plans[dateKey(d)]; ok {
		return p
	}
	return store.SessionPlan{UserID: t.userID, LocalDate: d}
}

// snapshot is d's plan as it is, for undo.
func (t *training) snapshot(d pgtype.Date) planState {
	p, ok := t.plans[dateKey(d)]
	if !ok {
		return planState{Day: d.Time.Format(time.DateOnly)}
	}
	return planStateOf(p)
}

// dayPlan is what is planned on one day.
type dayPlan struct {
	sched *store.TrainingSchedule
	// pos is a rotation's cycle day, from 0, or -1.
	pos int
	// sessions are the day's sessions from the schedule, or for a skipped
	// or moved day the sessions skipped or moved; movedIn are sessions
	// moved here from another day.
	sessions, movedIn []string
	status            string
	movedTo           pgtype.Date
	// pausedUntil is set on a day a rotation waits through for a session
	// moved to that day.
	pausedUntil pgtype.Date
	at          pgtype.Time
}

// all is every session planned on the day.
func (p dayPlan) all() []string {
	return append(slices.Clone(p.sessions), p.movedIn...)
}

// on works out what is planned on d: the schedule's sessions for that
// weekday or cycle day, changed by the day's plan. A rotation counts days
// from its start, except days it waits through for a moved session, so
// the moved session and everything after it shift later.
func (t *training) on(d pgtype.Date) dayPlan {
	p := dayPlan{sched: t.scheduleOn(d), pos: -1}
	s := p.sched
	if s == nil || !s.Kind.Valid {
		return p
	}
	slot := int(d.Time.Weekday())
	if s.Kind.String == "rotation" {
		elapsed := daysBetween(s.StartDate, d)
		for _, m := range t.moves {
			if m.LocalDate.Time.Before(s.StartDate.Time) || !m.LocalDate.Time.Before(d.Time) {
				continue
			}
			if m.MovedTo.Time.After(d.Time) {
				p.pausedUntil = m.MovedTo
			}
			elapsed -= min(daysBetween(m.LocalDate, m.MovedTo), daysBetween(m.LocalDate, d))
		}
		cycle := int(s.CycleDays.Int32)
		slot = ((int(s.StartOffset)+elapsed)%cycle + cycle) % cycle
		if !p.pausedUntil.Valid {
			p.pos = slot
		}
	}
	if day, ok := t.days[s.ID][int32(slot)]; ok && !p.pausedUntil.Valid {
		p.sessions, p.at = day.Sessions, day.TrainingTime
	}
	if !p.at.Valid {
		p.at = s.TrainingTime
	}
	if row, ok := t.plans[dateKey(d)]; ok {
		p.movedIn = row.MovedIn
		if row.TrainingTime.Valid {
			p.at = row.TrainingTime
		}
		if row.Status.Valid {
			p.status, p.movedTo = row.Status.String, row.MovedTo
			p.sessions, p.movedIn = row.Sessions, nil
		}
	}
	return p
}

// describe shows the day's plan, like "legs + cardio at 6:00 PM", "rest",
// or "pull, moved to Wed Oct 28".
func (p dayPlan) describe() string {
	switch {
	case p.sched == nil || !p.sched.Kind.Valid:
		return "no schedule"
	case p.pausedUntil.Valid:
		return "nothing, the rotation waits for the session moved to " + dayName(p.pausedUntil)
	}
	s := strings.Join(p.all(), " + ")
	switch {
	case s == "":
		return "rest"
	case p.status == "skipped":
		return s + ", skipped"
	case p.status == "moved":
		return s + ", moved to " + dayName(p.movedTo)
	}
	if p.at.Valid {
		s += " at " + clock(micros(p.at), "3:04 PM")
	}
	if len(p.movedIn) > 0 {
		s += " (" + strings.Join(p.movedIn, " + ") + " moved here)"
	}
	return s
}

// describe shows the schedule in force on d, like "weekly: push Mon/Thu,
// pull Tue/Fri, legs Wed/Sat, rest Sun; usually at 6:00 PM" or "2-day
// rotation: full body, rest".
func (t *training) describe(d pgtype.Date) string {
	s := t.scheduleOn(d)
	switch {
	case s == nil:
		return "none set"
	case !s.Kind.Valid:
		return "none, the user doesn't follow one"
	}
	days := t.days[s.ID]
	dayText := func(slot int32) (string, string) {
		day, ok := days[slot]
		if !ok {
			return "rest", ""
		}
		at := ""
		if day.TrainingTime.Valid {
			at = " at " + clock(micros(day.TrainingTime), "3:04 PM")
		}
		return strings.Join(day.Sessions, " + "), at
	}
	var out string
	if s.Kind.String == "weekly" {
		// Weekdays with the same sessions and time are listed together.
		type group struct{ sessions, at, names string }
		var groups []group
	slots:
		for _, slot := range weekOrder {
			sessions, at := dayText(slot)
			name := strings.ToUpper(weekdays[slot][:1]) + weekdays[slot][1:]
			for i := range groups {
				if groups[i].sessions == sessions && groups[i].at == at {
					groups[i].names += "/" + name
					continue slots
				}
			}
			groups = append(groups, group{sessions, at, name})
		}
		var parts []string
		for _, g := range groups {
			parts = append(parts, g.sessions+" "+g.names+g.at)
		}
		out = "weekly: " + strings.Join(parts, ", ")
	} else {
		var parts []string
		for slot := range s.CycleDays.Int32 {
			sessions, at := dayText(slot)
			parts = append(parts, sessions+at)
		}
		out = fmt.Sprintf("%d-day rotation: %s", s.CycleDays.Int32, strings.Join(parts, ", "))
	}
	if s.TrainingTime.Valid {
		out += "; usually at " + clock(micros(s.TrainingTime), "3:04 PM")
	}
	return out
}

// todayLine is " Today: " and today's plan, or nothing without a schedule.
func (t *training) todayLine(today pgtype.Date) string {
	if s := t.scheduleOn(today); s == nil || !s.Kind.Valid {
		return ""
	}
	return " Today: " + t.on(today).describe() + "."
}

// dayName names a day by date, like "Wed Oct 28".
func dayName(d pgtype.Date) string {
	return d.Time.Format("Mon Jan 2")
}

// dayLabel names a day by date and, for today and tomorrow, relative to
// today, like "Wed Oct 28 (tomorrow)".
func dayLabel(d, today pgtype.Date) string {
	switch {
	case d.Time.Equal(today.Time):
		return dayName(d) + " (today)"
	case d.Time.Equal(today.Time.AddDate(0, 0, 1)):
		return dayName(d) + " (tomorrow)"
	}
	return dayName(d)
}

func dateKey(d pgtype.Date) string {
	return d.Time.Format(time.DateOnly)
}

func addDays(d pgtype.Date, n int) pgtype.Date {
	return pgtype.Date{Time: d.Time.AddDate(0, 0, n), Valid: true}
}

// daysBetween counts the days from a to b.
func daysBetween(a, b pgtype.Date) int {
	return int(b.Time.Sub(a.Time).Hours() / 24)
}

// scheduleState is a schedule as set_schedule saves it and undo restores
// it. Times are microseconds since midnight.
type scheduleState struct {
	Kind         *string            `json:"kind"`
	CycleDays    *int32             `json:"cycle_days"`
	StartOffset  int32              `json:"start_offset"`
	TrainingTime *int64             `json:"training_time"`
	Days         []scheduleDayState `json:"days"`
}

type scheduleDayState struct {
	Slot         int32    `json:"slot"`
	Sessions     []string `json:"sessions"`
	TrainingTime *int64   `json:"training_time"`
}

func scheduleStateOf(s store.TrainingSchedule, days []store.ScheduleDay) *scheduleState {
	st := &scheduleState{StartOffset: s.StartOffset, TrainingTime: microsOf(s.TrainingTime)}
	if s.Kind.Valid {
		st.Kind = &s.Kind.String
	}
	if s.CycleDays.Valid {
		st.CycleDays = &s.CycleDays.Int32
	}
	for _, d := range days {
		st.Days = append(st.Days, scheduleDayState{Slot: d.Slot, Sessions: d.Sessions, TrainingTime: microsOf(d.TrainingTime)})
	}
	return st
}

// insertSchedule saves st as the schedule starting on day.
func insertSchedule(ctx context.Context, q *store.Queries, userID int64, day pgtype.Date, st scheduleState) error {
	s, err := q.InsertSchedule(ctx, store.InsertScheduleParams{UserID: userID, Kind: text(st.Kind), CycleDays: int4(st.CycleDays),
		StartOffset: st.StartOffset, TrainingTime: timeOf(st.TrainingTime), StartDate: day})
	if err != nil {
		return err
	}
	for _, d := range st.Days {
		if err := q.InsertScheduleDay(ctx, store.InsertScheduleDayParams{ScheduleID: s.ID, Slot: d.Slot, Sessions: d.Sessions,
			TrainingTime: timeOf(d.TrainingTime)}); err != nil {
			return err
		}
	}
	return nil
}

// planState is one day's plan as it was, for undo. Existed is false when
// the day had no plan.
type planState struct {
	Day          string   `json:"day"`
	Existed      bool     `json:"existed"`
	Status       *string  `json:"status,omitempty"`
	MovedTo      string   `json:"moved_to,omitempty"`
	Sessions     []string `json:"sessions,omitempty"`
	MovedIn      []string `json:"moved_in,omitempty"`
	TrainingTime *int64   `json:"training_time,omitempty"`
}

func planStateOf(p store.SessionPlan) planState {
	st := planState{Day: p.LocalDate.Time.Format(time.DateOnly), Existed: true, Sessions: p.Sessions, MovedIn: p.MovedIn,
		TrainingTime: microsOf(p.TrainingTime)}
	if p.Status.Valid {
		st.Status = &p.Status.String
	}
	if p.MovedTo.Valid {
		st.MovedTo = p.MovedTo.Time.Format(time.DateOnly)
	}
	return st
}

// restorePlans puts day plans back as they were.
func restorePlans(ctx context.Context, q *store.Queries, userID int64, plans []planState) error {
	for _, st := range plans {
		day, err := parseDay(st.Day)
		if err != nil {
			return err
		}
		p := store.SessionPlan{UserID: userID, LocalDate: day, Status: text(st.Status), Sessions: st.Sessions, MovedIn: st.MovedIn,
			TrainingTime: timeOf(st.TrainingTime)}
		if st.MovedTo != "" {
			if p.MovedTo, err = parseDay(st.MovedTo); err != nil {
				return err
			}
		}
		if !st.Existed {
			err = q.DeletePlan(ctx, store.DeletePlanParams{UserID: userID, LocalDate: day})
		} else {
			err = savePlan(ctx, q, p)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// savePlan saves a day's plan, or removes it when it changes nothing.
func savePlan(ctx context.Context, q *store.Queries, p store.SessionPlan) error {
	if !p.Status.Valid && len(p.MovedIn) == 0 && !p.TrainingTime.Valid {
		return q.DeletePlan(ctx, store.DeletePlanParams{UserID: p.UserID, LocalDate: p.LocalDate})
	}
	return q.SetPlan(ctx, store.SetPlanParams{UserID: p.UserID, LocalDate: p.LocalDate, Status: p.Status, MovedTo: p.MovedTo,
		Sessions: orEmpty(p.Sessions), MovedIn: orEmpty(p.MovedIn), TrainingTime: p.TrainingTime})
}

// orEmpty keeps a nil list from being saved as NULL.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type scheduleDayArg struct {
	Weekday  *string  `json:"weekday"`
	Sessions []string `json:"sessions"`
	Time     *string  `json:"time"`
}

type scheduleArgs struct {
	Kind          *string          `json:"kind"`
	Days          []scheduleDayArg `json:"days"`
	CycleDayToday *int             `json:"cycle_day_today"`
	TrainingTime  *string          `json:"training_time"`
}

// parseSchedule checks set_schedule's arguments and returns the schedule
// they give, or why they don't give one.
func parseSchedule(args scheduleArgs) (scheduleState, string) {
	var st scheduleState
	if args.Kind == nil {
		if len(args.Days) > 0 || args.CycleDayToday != nil || args.TrainingTime != nil {
			return st, "kind null means no schedule, so days, cycle_day_today, and training_time must be empty"
		}
		return st, ""
	}
	kind := *args.Kind
	var cycle int32
	switch kind {
	case "weekly":
		cycle = 7
		if args.CycleDayToday != nil {
			return st, "cycle_day_today is only for a rotation"
		}
	case "rotation":
		cycle = int32(len(args.Days))
		if cycle == 0 || cycle > maxCycle {
			return st, fmt.Sprintf("a rotation lists every day of its cycle in order, 1 to %d days", maxCycle)
		}
		if n := args.CycleDayToday; n != nil {
			if *n < 1 || *n > int(cycle) {
				return st, fmt.Sprintf("cycle_day_today must be 1 to %d, the days in the rotation", cycle)
			}
			st.StartOffset = int32(*n - 1)
		}
	default:
		return st, fmt.Sprintf("kind %q must be weekly, rotation, or null", kind)
	}
	st.Kind, st.CycleDays = &kind, &cycle
	if args.TrainingTime != nil {
		at, ok := timeOfDay(*args.TrainingTime)
		if !ok {
			return st, fmt.Sprintf("training_time %q is not a 24-hour time like 18:00", *args.TrainingTime)
		}
		st.TrainingTime = &at.Microseconds
	}
	seen := map[int32]bool{}
	for i, d := range args.Days {
		slot := int32(i)
		if kind == "weekly" {
			if d.Weekday == nil {
				return st, "every day of a weekly schedule needs its weekday"
			}
			n := slices.Index(weekdays, strings.ToLower(strings.TrimSpace(*d.Weekday)))
			if n < 0 {
				return st, fmt.Sprintf("weekday %q must be one of %s", *d.Weekday, strings.Join(weekdays, ", "))
			}
			slot = int32(n)
			if seen[slot] {
				return st, fmt.Sprintf("%s is listed twice; list all of a day's sessions together", weekdays[slot])
			}
			seen[slot] = true
		} else if d.Weekday != nil {
			return st, "a rotation's days follow the cycle, not weekdays, so weekday must be null"
		}
		sessions, errText := cleanSessions(d.Sessions)
		if errText != "" {
			return st, errText
		}
		if len(sessions) == 0 {
			if d.Time != nil {
				return st, "a rest day has no time"
			}
			continue
		}
		day := scheduleDayState{Slot: slot, Sessions: sessions}
		if d.Time != nil {
			at, ok := timeOfDay(*d.Time)
			if !ok {
				return st, fmt.Sprintf("time %q is not a 24-hour time like 18:00", *d.Time)
			}
			day.TrainingTime = &at.Microseconds
		}
		st.Days = append(st.Days, day)
	}
	if len(st.Days) == 0 {
		return st, "the schedule has no training days"
	}
	slices.SortFunc(st.Days, func(a, b scheduleDayState) int { return int(a.Slot - b.Slot) })
	return st, ""
}

// cleanSessions normalizes a day's session names to lowercase single-spaced
// names. No sessions, or only "rest", is a rest day and comes back empty.
func cleanSessions(names []string) ([]string, string) {
	var out []string
	for _, n := range names {
		n = strings.Join(strings.Fields(strings.ToLower(n)), " ")
		switch {
		case n == "":
			return nil, "a session name is empty"
		case len(n) > maxSessionName:
			return nil, fmt.Sprintf("session name %q is longer than %d characters", n, maxSessionName)
		case slices.Contains(out, n):
			return nil, fmt.Sprintf("%s is listed twice on one day", n)
		}
		out = append(out, n)
	}
	if slices.Contains(out, "rest") {
		if len(out) > 1 {
			return nil, "a rest day has no other sessions"
		}
		return nil, ""
	}
	if len(out) > maxSessions {
		return nil, fmt.Sprintf("a day can have at most %d sessions", maxSessions)
	}
	return out, ""
}

func (a *Agent) setSchedule(ctx context.Context, c *call, args scheduleArgs) (string, error) {
	st, errText := parseSchedule(args)
	if errText != "" {
		return "error: " + errText, nil
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	undo := &undoRecord{Op: undoRestoreSchedule, Day: today.Time.Format(time.DateOnly)}
	// A change made earlier today is replaced, so undo puts it back.
	old, err := c.q.ScheduleOnDay(ctx, store.ScheduleOnDayParams{UserID: c.userID, StartDate: today})
	switch {
	case err == nil:
		days, err := c.q.ScheduleDays(ctx, []int64{old.ID})
		if err != nil {
			return "", err
		}
		undo.Schedule = scheduleStateOf(old, days)
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	if err := c.q.DeleteScheduleOnDay(ctx, store.DeleteScheduleOnDayParams{UserID: c.userID, StartDate: today}); err != nil {
		return "", err
	}
	if err := insertSchedule(ctx, c.q, c.userID, today, st); err != nil {
		return "", err
	}

	// Skips and moves from today on were made for the old schedule, so they
	// are cleared. Times stay.
	plans, err := c.q.PlansBetween(ctx, store.PlansBetweenParams{UserID: c.userID, First: today, Last: addDays(today, planAhead+maxMove)})
	if err != nil {
		return "", err
	}
	cleared := 0
	for _, p := range plans {
		if !p.Status.Valid && len(p.MovedIn) == 0 {
			continue
		}
		undo.Plans = append(undo.Plans, planStateOf(p))
		if p.Status.Valid {
			cleared++
		}
		p.Status, p.MovedTo, p.Sessions, p.MovedIn = pgtype.Text{}, pgtype.Date{}, nil, nil
		if err := savePlan(ctx, c.q, p); err != nil {
			return "", err
		}
	}

	t, err := loadTraining(ctx, c.q, c.userID, today, today)
	if err != nil {
		return "", err
	}
	desc := t.describe(today)
	undo.Desc = "setting your training schedule (" + desc + ")"
	c.undo = undo
	result := "saved, starting today: " + desc + "." + t.todayLine(today)
	switch cleared {
	case 0:
	case 1:
		result += " Cleared 1 skip or move made for the old schedule."
	default:
		result += fmt.Sprintf(" Cleared %d skips or moves made for the old schedule.", cleared)
	}
	return result, nil
}

type planArgs struct {
	Date   *string `json:"date"`
	Change string  `json:"change"`
	ToDate *string `json:"to_date"`
	Time   *string `json:"time"`
}

// planDate parses a YYYY-MM-DD local date to plan, which must be today or
// up to planAhead days after.
func planDate(s string, today pgtype.Date) (pgtype.Date, string) {
	day, err := parseDay(strings.TrimSpace(s))
	if err != nil {
		return day, fmt.Sprintf("date %q is not YYYY-MM-DD", s)
	}
	if day.Time.Before(today.Time) {
		return day, "date is in the past; today is " + today.Time.Format(time.DateOnly) + ", and only today or later can be planned"
	}
	if day.Time.After(today.Time.AddDate(0, 0, planAhead)) {
		return day, fmt.Sprintf("date is more than %d days ahead", planAhead)
	}
	return day, ""
}

func (a *Agent) planSession(ctx context.Context, c *call, args planArgs) (string, error) {
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	day := today
	var errText string
	if args.Date != nil {
		if day, errText = planDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	var to pgtype.Date
	switch args.Change {
	case "move":
		if args.ToDate == nil {
			return "error: move needs to_date, the day the session moves to", nil
		}
		var err error
		if to, err = parseDay(strings.TrimSpace(*args.ToDate)); err != nil {
			return fmt.Sprintf("error: to_date %q is not YYYY-MM-DD", *args.ToDate), nil
		}
		if !to.Time.After(day.Time) {
			return "error: to_date must be after date; a session can only move later", nil
		}
		if daysBetween(day, to) > maxMove {
			return fmt.Sprintf("error: to_date is more than %d days after date", maxMove), nil
		}
	case "skip", "time", "clear":
		if args.ToDate != nil {
			return "error: to_date is only for move", nil
		}
	default:
		return fmt.Sprintf("error: change %q must be skip, move, time, or clear", args.Change), nil
	}
	var at pgtype.Time
	if args.Time != nil {
		if args.Change != "move" && args.Change != "time" {
			return "error: time is only for move and time", nil
		}
		var ok bool
		if at, ok = timeOfDay(*args.Time); !ok {
			return fmt.Sprintf("error: time %q is not a 24-hour time like 18:00", *args.Time), nil
		}
	}

	// This covers every day that can have a plan: up to planAhead days
	// ahead, and a move from there.
	last := addDays(today, planAhead+maxMove)
	t, err := loadTraining(ctx, c.q, c.userID, day, last)
	if err != nil {
		return "", err
	}
	s := t.scheduleOn(day)
	if s == nil || !s.Kind.Valid {
		return "error: no training schedule; save one with set_schedule first", nil
	}
	p := t.on(day)
	switch {
	case args.Change == "clear":
		if p.status == "" {
			return "error: nothing is skipped or moved on " + dayName(day), nil
		}
	case p.pausedUntil.Valid:
		return fmt.Sprintf("error: nothing is planned on %s; the rotation waits for the session moved to %s", dayName(day),
			dayName(p.pausedUntil)), nil
	case p.status != "":
		return fmt.Sprintf("error: %s is already %s; clear it first", dayName(day), p.describe()), nil
	case len(p.all()) == 0:
		return fmt.Sprintf("error: %s is a rest day, with nothing planned", dayName(day)), nil
	}

	// A skip or move would leave a logged workout counting as an extra.
	if args.Change == "skip" || args.Change == "move" {
		logs, err := c.q.WorkoutsBetween(ctx, store.WorkoutsBetweenParams{UserID: c.userID, First: day, Last: day})
		if err != nil {
			return "", err
		}
		if len(logs) > 0 {
			return fmt.Sprintf("error: %s already has a workout logged (#%d, %s); change it with edit_log instead", dayName(day),
				logs[0].ID, workoutSummary(logs[0])), nil
		}
	}

	weekly := s.Kind.String == "weekly"
	row := t.row(day)
	names := strings.Join(p.all(), " + ")
	undo := &undoRecord{Op: undoRestorePlans, Plans: []planState{t.snapshot(day)}}
	shown := []pgtype.Date{day}
	var saves []store.SessionPlan
	switch args.Change {
	case "skip":
		row.Status, row.Sessions = pgtype.Text{String: "skipped", Valid: true}, p.all()
		undo.Desc = fmt.Sprintf("skipping %s on %s", names, dayName(day))
	case "move":
		if !weekly {
			if errText := t.shiftBlocked(day); errText != "" {
				return errText, nil
			}
			// The rotation waits through the days before to.
			for d := addDays(day, 1); d.Time.Before(to.Time); d = addDays(d, 1) {
				shown = append(shown, d)
			}
		}
		if q := t.on(to); q.status != "" {
			return fmt.Sprintf("error: %s is already %s; clear it first", dayName(to), q.describe()), nil
		}
		row.Status, row.MovedTo, row.Sessions = pgtype.Text{String: "moved", Valid: true}, to, p.all()
		target := t.row(to)
		if weekly {
			target.MovedIn = append(slices.Clone(target.MovedIn), p.all()...)
		}
		if args.Time != nil {
			target.TrainingTime = at
		}
		if weekly || args.Time != nil {
			undo.Plans = append(undo.Plans, t.snapshot(to))
			saves = append(saves, target)
		}
		shown = append(shown, to)
		undo.Desc = fmt.Sprintf("moving %s from %s to %s", names, dayName(day), dayName(to))
	case "time":
		if args.Time == nil {
			if !row.TrainingTime.Valid {
				return "error: no time is set for " + dayName(day) + " itself; give time to set one", nil
			}
			row.TrainingTime = pgtype.Time{}
			undo.Desc = fmt.Sprintf("clearing the time for %s on %s", names, dayName(day))
		} else {
			row.TrainingTime = at
			undo.Desc = fmt.Sprintf("setting %s on %s for %s", names, dayName(day), clock(micros(at), "3:04 PM"))
		}
	case "clear":
		if p.status == "moved" {
			if q := t.on(row.MovedTo); q.status != "" {
				return fmt.Sprintf("error: %s, where it moved, is already %s; clear that first", dayName(row.MovedTo), q.describe()), nil
			}
			if !weekly {
				if errText := t.shiftBlocked(day); errText != "" {
					return errText, nil
				}
			}
			if weekly {
				target := t.row(row.MovedTo)
				undo.Plans = append(undo.Plans, t.snapshot(row.MovedTo))
				target.MovedIn = removeEach(target.MovedIn, row.Sessions)
				saves = append(saves, target)
			}
			shown = append(shown, row.MovedTo)
		}
		undo.Desc = fmt.Sprintf("bringing back %s on %s", names, dayName(day))
		row.Status, row.MovedTo, row.Sessions = pgtype.Text{}, pgtype.Date{}, nil
	}
	for _, r := range append([]store.SessionPlan{row}, saves...) {
		if err := savePlan(ctx, c.q, r); err != nil {
			return "", err
		}
	}
	slices.SortFunc(shown, func(a, b pgtype.Date) int { return a.Time.Compare(b.Time) })
	for _, d := range shown {
		undo.Days = append(undo.Days, d.Time.Format(time.DateOnly))
	}
	c.undo = undo

	t, err = loadTraining(ctx, c.q, c.userID, day, last)
	if err != nil {
		return "", err
	}
	return "saved. " + t.lines(shown, today) + ".", nil
}

// shiftBlocked returns an error for the model when a day after day is
// skipped or moved. Moving a rotation session, or clearing a move, shifts
// every later day of the cycle, which would leave those changes on the
// wrong sessions. The loaded plans must reach every day that can have one.
func (t *training) shiftBlocked(day pgtype.Date) string {
	var first pgtype.Date
	for _, p := range t.plans {
		if p.Status.Valid && p.LocalDate.Time.After(day.Time) && (!first.Valid || p.LocalDate.Time.Before(first.Time)) {
			first = p.LocalDate
		}
	}
	if !first.Valid {
		return ""
	}
	return fmt.Sprintf("error: %s is already %s, and this would shift the rotation under it; clear it first",
		dayName(first), t.on(first).describe())
}

// removeEach removes one of each of names from list.
func removeEach(list, names []string) []string {
	out := slices.Clone(list)
	for _, n := range names {
		if i := slices.Index(out, n); i >= 0 {
			out = slices.Delete(out, i, i+1)
		}
	}
	return out
}

// lines shows the plan for days, like "Tue Oct 27 (today): legs, skipped".
func (t *training) lines(days []pgtype.Date, today pgtype.Date) string {
	var parts []string
	for _, d := range days {
		parts = append(parts, dayLabel(d, today)+": "+t.on(d).describe())
	}
	return strings.Join(parts, "; ")
}

// trainingUndoReply is where the schedule, or the days a plan change
// showed, stand after undoing it.
func (a *Agent) trainingUndoReply(ctx context.Context, q *store.Queries, userID int64, rec undoRecord) (string, error) {
	user, err := q.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	if rec.Op == undoRestoreSchedule {
		t, err := loadTraining(ctx, q, userID, today, today)
		if err != nil {
			return "", err
		}
		return "Schedule: " + t.describe(today) + "." + t.todayLine(today), nil
	}
	var days []pgtype.Date
	for _, s := range rec.Days {
		d, err := parseDay(s)
		if err != nil {
			return "", err
		}
		days = append(days, d)
	}
	if len(days) == 0 {
		return "", nil
	}
	t, err := loadTraining(ctx, q, userID, days[0], days[len(days)-1])
	if err != nil {
		return "", err
	}
	return upperFirst(t.lines(days, today)) + ".", nil
}

// trainingSection is the prompt's schedule and the next week's plan.
func (a *Agent) trainingSection(ctx context.Context, user store.User) (string, error) {
	today := a.localDate(user)
	t, err := loadTraining(ctx, a.Store, user.ID, today, addDays(today, 6))
	if err != nil {
		return "", fmt.Errorf("loading training: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n## Training\nSchedule: " + t.describe(today))
	if p := t.on(today); p.pos >= 0 {
		fmt.Fprintf(&b, " (today is day %d of %d)", p.pos+1, p.sched.CycleDays.Int32)
	}
	b.WriteString("\n")
	if s := t.scheduleOn(today); s != nil && s.Kind.Valid {
		b.WriteString("The next 7 days, worked out by code:\n")
		for i := range 7 {
			d := addDays(today, i)
			b.WriteString(dayLabel(d, today) + ": " + t.on(d).describe() + "\n")
		}
	}
	b.WriteString("The schedule is the user's workout goal. Save a new or changed one with set_schedule, always the whole plan; it " +
		"starts today. Weekly pins sessions to weekdays: list each training day with its weekday. A rotation is a cycle of days " +
		"that repeats regardless of weekday, like \"full body, rest\" or \"push, pull, legs, rest\": list every day of the cycle " +
		"in order with weekday null and rest days as [\"rest\"], and set cycle_day_today if the user says where they are in it. " +
		"Name sessions in short lowercase words, like push, pull, legs, upper, lower, full body, cardio. If the user doesn't follow " +
		"a schedule, save kind null. Change one day's plan, today or later, with plan_session: skip it ahead of time, move it to " +
		"a later day, set the time they're going, or clear a skip or move. Use the days above for what is planned; never work it " +
		"out yourself.\n")
	return b.String(), nil
}
