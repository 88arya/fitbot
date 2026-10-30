package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/llm"
	"github.com/88arya/fitbot/internal/scheduler"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Check-ins and reminders are worded in code from the same queries as
// get_day, so they go out even when the LLM is down. Which texts are due
// is worked out again when each one goes out, so logging a workout or a
// supplement first cancels a reminder that hasn't gone yet, and moving the
// gym time moves its reminder and follow-up.

// Kinds of scheduled text.
const (
	kindMorning     = "morning_checkin"
	kindNight       = "night_checkin"
	kindGym         = "gym_reminder"
	kindGymFollowup = "gym_followup"
	kindSupplements = "supplement_reminder"
	kindReping      = "supplement_reping"
	kindWater       = "water_nudge"
)

const (
	// nightLead is how long before bedtime the night check-in goes, and
	// lastNight the latest it goes, for a bedtime after midnight.
	nightLead = 30 * time.Minute
	lastNight = 23*time.Hour + 30*time.Minute
	// joinWindow is how close to a check-in a supplement can be due and be
	// named in the check-in instead of a reminder of its own.
	joinWindow = 30 * time.Minute
	// waterBehind is how far behind a straight-line pace, in percent of the
	// goal, the user must be for a water nudge.
	waterBehind = 25
	// replyDays is how many days back a short reply can answer a check-in:
	// "done" the next morning still counts for last night's reminder.
	replyDays = 1
	// maxMute is how far ahead texts can be muted.
	maxMute = 30 * 24 * time.Hour
	// maxSnooze is the longest a "snooze" text can mute for.
	maxSnooze = 24 * time.Hour
	// defaultGymFollowup is the follow-up delay when Agent.GymFollowup is
	// unset.
	defaultGymFollowup = 2 * time.Hour
	// muteLayout is how mute_until gives a local time.
	muteLayout = "2006-01-02 15:04"
)

// asks is what a scheduled text asked about, stored with its delivery, so
// a short reply like "yes" or "done" can answer it.
type asks struct {
	// Workout are the planned sessions it asked about, and Supplements the
	// supplements, by name.
	Workout     []string `json:"workout,omitempty"`
	Supplements []string `json:"supplements,omitempty"`
}

// morningTime is when the morning check-in goes: the time the user set,
// or their wake time.
func morningTime(u store.User) time.Duration {
	if u.MorningCheckin.Valid {
		return micros(u.MorningCheckin)
	}
	return scheduler.WakeTime(u)
}

// nightTime is when the night check-in goes: the time the user set, or
// nightLead before bedtime, but no later than lastNight, so a bedtime after
// midnight still gets it on the day it covers.
func nightTime(u store.User) time.Duration {
	if u.NightCheckin.Valid {
		return micros(u.NightCheckin)
	}
	bed := scheduler.Bedtime(u)
	if bed < scheduler.WakeTime(u) || bed < nightLead {
		return lastNight
	}
	return bed - nightLead
}

// quiet reports whether t, time since midnight, falls in quiet hours,
// from bedtime to wake time. Only texts at times the user set themselves,
// like a supplement at 11:30 PM, go out then.
func quiet(u store.User, t time.Duration) bool {
	bed, wake := scheduler.Bedtime(u), scheduler.WakeTime(u)
	if bed > wake {
		return t >= bed || t < wake
	}
	return t >= bed && t < wake
}

// slotName names a time-of-day slot, like "18:00".
func slotName(t time.Duration) string {
	return clock(t, "15:04")
}

// doseGroup is the supplements due at one time of day.
type doseGroup struct {
	at    time.Duration
	plans []store.SupplementHistoryRow
	// join is the check-in that names them, kindMorning or kindNight, or ""
	// when they get a reminder of their own.
	join string
}

// checkinDay is what a day's scheduled texts are built from.
type checkinDay struct {
	user           store.User
	day            pgtype.Date
	morning, night time.Duration
	goals          map[string]store.Goal
	totals         map[string]int
	work           workoutDay
	// open are the day's planned sessions not logged or skipped yet.
	open  []string
	supps *supplementList
	logs  []store.SupplementLog
	// doses are the day's due supplements that have a time, earliest first.
	doses []doseGroup
}

func (a *Agent) checkinDay(ctx context.Context, user store.User, day pgtype.Date) (*checkinDay, error) {
	q := a.Store
	user = a.timed(user)
	settings := a.settings()
	d := &checkinDay{user: user, day: day, morning: morningTime(user), night: nightTime(user), totals: map[string]int{}}
	goals, err := q.GoalsOn(ctx, store.GoalsOnParams{UserID: user.ID, Day: day})
	if err != nil {
		return nil, err
	}
	d.goals = goalsByMetric(goals)
	totals, err := q.IntakeTotals(ctx, store.IntakeTotalsParams{UserID: user.ID, Day: day})
	if err != nil {
		return nil, err
	}
	for _, t := range totals {
		d.totals[t.Metric] = int(t.Total)
	}
	r, err := loadWorkouts(ctx, q, user.ID, day, day)
	if err != nil {
		return nil, err
	}
	d.work = r.day(day)
	// The day is judged as if it were still going, so a text that goes out
	// just past midnight still asks about its own day.
	planned, _ := d.work.outcomes(day)
	for _, o := range planned {
		if o.state == sessionOpen {
			d.open = append(d.open, o.name)
		}
	}
	if d.supps, err = loadSupplements(ctx, q, user); err != nil {
		return nil, err
	}
	if d.logs, err = q.SupplementLogsBetween(ctx, store.SupplementLogsBetweenParams{UserID: user.ID, First: day, Last: day}); err != nil {
		return nil, err
	}
	for _, p := range d.supps.listOn(day) {
		at, ok := doseTime(p, user)
		if !due(p, day) || !ok {
			continue
		}
		i := slices.IndexFunc(d.doses, func(g doseGroup) bool { return g.at == at })
		if i < 0 {
			g := doseGroup{at: at}
			switch {
			case near(at, d.morning) && settings.on(settingMorning):
				g.join = kindMorning
			case near(at, d.night) && settings.on(settingNight):
				g.join = kindNight
			}
			d.doses = append(d.doses, g)
			i = len(d.doses) - 1
		}
		d.doses[i].plans = append(d.doses[i].plans, p)
	}
	slices.SortFunc(d.doses, func(a, b doseGroup) int { return int(a.at - b.at) })
	return d, nil
}

// doseTime is when a supplement is taken, as time since midnight: its
// clock time, or the wake time or bedtime it's anchored to. ok is false for
// one taken any time.
func doseTime(p store.SupplementHistoryRow, u store.User) (time.Duration, bool) {
	switch {
	case p.TakeAt.Valid:
		return micros(p.TakeAt), true
	case p.Anchor.String == "wake":
		return scheduler.WakeTime(u), true
	case p.Anchor.String == "bed":
		return scheduler.Bedtime(u), true
	}
	return 0, false
}

func near(a, b time.Duration) bool {
	return max(a-b, b-a) <= joinWindow
}

// pending are the supplements of g not logged yet that day.
func (d *checkinDay) pending(g doseGroup) []store.SupplementHistoryRow {
	var out []store.SupplementHistoryRow
	for _, p := range g.plans {
		if !slices.ContainsFunc(d.logs, func(l store.SupplementLog) bool { return l.SupplementID == p.SupplementID }) {
			out = append(out, p)
		}
	}
	return out
}

// joined are the supplements not logged yet that the check-in kind names
// as due now.
func (d *checkinDay) joined(kind string) []store.SupplementHistoryRow {
	var out []store.SupplementHistoryRow
	for _, g := range d.doses {
		if g.join == kind {
			out = append(out, d.pending(g)...)
		}
	}
	return out
}

// gymTime is when the day's open sessions are planned, if they have a time.
func (d *checkinDay) gymTime() (time.Duration, bool) {
	if len(d.open) == 0 || !d.work.plan.at.Valid {
		return 0, false
	}
	return micros(d.work.plan.at), true
}

// waterTimes are the times of the day's water nudges: n points spread
// evenly between the morning and night check-ins. There are none without a
// water goal.
func (d *checkinDay) waterTimes(n int) []time.Duration {
	g, ok := d.goals["water"]
	window := d.night - d.morning
	if n <= 0 || !ok || !g.Value.Valid || window <= 0 {
		return nil
	}
	var out []time.Duration
	for i := 1; i <= n; i++ {
		out = append(out, (d.morning + window*time.Duration(i)/time.Duration(n+1)).Truncate(time.Minute))
	}
	return out
}

// checkins gives the scheduler the agent's texts.
type checkins struct{ a *Agent }

// Checkins is what the scheduler sends for the agent: the morning and night
// check-ins, gym reminders and follow-ups, supplement reminders, water
// nudges, and the weekly summary.
func (a *Agent) Checkins() scheduler.Checkins {
	return checkins{a}
}

func (c checkins) Due(ctx context.Context, u store.User, day pgtype.Date) ([]scheduler.Due, error) {
	if err := c.a.refreshSettings(ctx); err != nil {
		return nil, err
	}
	d, err := c.a.checkinDay(ctx, u, day)
	if err != nil {
		return nil, err
	}
	u = d.user
	v := c.a.settings()
	var out []scheduler.Due
	// The summary is listed first so it is planned, and sent, before the
	// check-in, which a short reply then answers.
	if weeklyDue(day) && v.on(settingWeekly) {
		out = append(out, scheduler.Due{Kind: kindWeekly, At: d.morning})
	}
	if v.on(settingMorning) {
		out = append(out, scheduler.Due{Kind: kindMorning, At: d.morning})
	}
	if v.on(settingNight) {
		out = append(out, scheduler.Due{Kind: kindNight, At: d.night})
	}
	// later is a time derived from another, which must stay on the day and
	// out of quiet hours.
	later := func(t time.Duration) bool {
		return t < 24*time.Hour && !quiet(u, t)
	}
	if at, ok := d.gymTime(); ok {
		if v.on(settingGym) {
			out = append(out, scheduler.Due{Kind: kindGym, Slot: slotName(at), At: at})
		}
		// A follow-up after the night check-in would ask what it already did.
		if f := at + v.duration(settingFollowup); v.on(settingFollowups) && (f < d.night || !v.on(settingNight)) && later(f) {
			out = append(out, scheduler.Due{Kind: kindGymFollowup, Slot: slotName(at), At: f})
		}
	}
	for _, g := range d.doses {
		if len(d.pending(g)) == 0 || !v.on(settingSupplements) {
			continue
		}
		if g.join == "" {
			out = append(out, scheduler.Due{Kind: kindSupplements, Slot: slotName(g.at), At: g.at})
		}
		if r := v.duration(settingReping); r > 0 && later(g.at+r) {
			out = append(out, scheduler.Due{Kind: kindReping, Slot: slotName(g.at), At: g.at + r})
		}
	}
	for i, at := range d.waterTimes(v.int(settingWater)) {
		out = append(out, scheduler.Due{Kind: kindWater, Slot: strconv.Itoa(i + 1), At: at})
	}
	return out, nil
}

func (c checkins) Text(ctx context.Context, u store.User, day pgtype.Date, kind, slot string) (scheduler.Text, error) {
	if err := c.a.refreshSettings(ctx); err != nil {
		return scheduler.Text{}, err
	}
	d, err := c.a.checkinDay(ctx, u, day)
	if err != nil {
		return scheduler.Text{}, err
	}
	switch kind {
	case kindMorning:
		return c.a.morningText(ctx, d)
	case kindNight:
		return c.a.nightText(ctx, d)
	case kindGym:
		at, ok := d.gymTime()
		if !ok {
			return scheduler.Text{}, nil
		}
		return scheduler.Text{Body: "Time to head out: " + strings.Join(d.open, " + ") + " at " + clock(at, "3:04 PM") + "."}, nil
	case kindGymFollowup:
		if len(d.open) == 0 {
			return scheduler.Text{}, nil
		}
		return withAsks("Did you make it to the gym for "+strings.Join(d.open, " + ")+
			"? Reply yes or no, or tell me when you're going instead.", asks{Workout: d.open})
	case kindSupplements, kindReping:
		i := slices.IndexFunc(d.doses, func(g doseGroup) bool { return slotName(g.at) == slot })
		if i < 0 {
			return scheduler.Text{}, nil
		}
		due := d.pending(d.doses[i])
		if len(due) == 0 {
			return scheduler.Text{}, nil
		}
		lead := "Time for "
		if kind == kindReping {
			lead = "Still due: "
		}
		return withAsks(lead+doseList(due)+". Reply done once you've taken "+itOrThem(len(due))+", or skip.", asksFor(due))
	case kindWater:
		return scheduler.Text{Body: c.a.waterText(d, slot)}, nil
	case kindWeekly:
		body, err := c.a.weeklyText(ctx, u, day)
		return scheduler.Text{Body: body}, err
	}
	return scheduler.Text{}, fmt.Errorf("unknown kind %q", kind)
}

func withAsks(body string, q asks) (scheduler.Text, error) {
	b, err := json.Marshal(q)
	if err != nil {
		return scheduler.Text{}, err
	}
	return scheduler.Text{Body: body, Asks: b}, nil
}

func asksFor(plans []store.SupplementHistoryRow) asks {
	var q asks
	for _, p := range plans {
		q.Supplements = append(q.Supplements, p.Name)
	}
	return q
}

// doseList names supplements with their doses, like "creatine 5 g and fish
// oil".
func doseList(plans []store.SupplementHistoryRow) string {
	var names []string
	for _, p := range plans {
		s := p.Name
		if p.Dose.Valid {
			s += " " + p.Dose.String
		}
		names = append(names, s)
	}
	return joinAnd(names)
}

func itOrThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func greeting(hello string, u store.User) string {
	if u.Name.Valid {
		return hello + ", " + u.Name.String + "."
	}
	return hello + "."
}

// morningText is today's session, goals, and supplements, and last night's
// sleep or a question about it. Supplements due around now are asked
// about, so "done" logs them.
func (a *Agent) morningText(ctx context.Context, d *checkinDay) (scheduler.Text, error) {
	parts := []string{greeting("Good morning", d.user)}
	if s := d.planLine(); s != "" {
		parts = append(parts, s)
	}
	var goals []string
	for _, name := range intakeMetrics {
		m, _ := metricByName(name)
		if g, ok := d.goals[name]; ok && g.Value.Valid {
			s := m.format(int(g.Value.Int32))
			if name != "calories" {
				s += " " + m.label
			}
			goals = append(goals, s)
		}
	}
	if len(goals) > 0 {
		parts = append(parts, "Goals: "+strings.Join(goals, ", ")+".")
	}
	now := d.joined(kindMorning)
	if s := d.supplementPlan(now); s != "" {
		parts = append(parts, s)
	}
	sleep, _ := metricByName("sleep")
	night, err := a.Store.SleepOn(ctx, store.SleepOnParams{UserID: d.user.ID, Day: d.day})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		parts = append(parts, "How did you sleep?")
	case err != nil:
		return scheduler.Text{}, err
	default:
		parts = append(parts, "Sleep: "+sleepProgress(sleep, int(night.Minutes), d.goals["sleep"], "last night")+".")
	}
	if len(now) == 0 {
		return scheduler.Text{Body: strings.Join(parts, " ")}, nil
	}
	return withAsks(strings.Join(parts, " "), asksFor(now))
}

// planLine is today's training plan, like "Today: legs + cardio at 6:00
// PM." or "Today is a rest day.", asking for a time when none is set, or
// nothing without a schedule.
func (d *checkinDay) planLine() string {
	p := d.work.plan
	switch {
	case p.sched == nil || !p.sched.Kind.Valid:
		return ""
	case len(p.all()) == 0 && !p.pausedUntil.Valid:
		return "Today is a rest day."
	case len(d.work.planned) > 0 && len(d.open) == 0 && p.status == "":
		// Everything planned is already logged, like after an early session.
		return "Today: " + d.work.describe(d.day) + "."
	}
	s := "Today: " + p.describe() + "."
	if len(d.open) > 0 && !p.at.Valid {
		s += " What time are you going?"
	}
	return s
}

// supplementPlan lists the day's due supplements and when, like
// "Supplements: creatine 5 g now, fish oil at 9:00 PM; reply done once
// you've taken creatine." now are the ones due around the check-in.
func (d *checkinDay) supplementPlan(now []store.SupplementHistoryRow) string {
	var parts []string
	for _, p := range d.supps.listOn(d.day) {
		if !due(p, d.day) {
			continue
		}
		s := doseList([]store.SupplementHistoryRow{p})
		logged := slices.IndexFunc(d.logs, func(l store.SupplementLog) bool { return l.SupplementID == p.SupplementID })
		switch {
		case logged >= 0:
			s += " " + d.logs[logged].Status
		case slices.ContainsFunc(now, func(n store.SupplementHistoryRow) bool { return n.SupplementID == p.SupplementID }):
			s += " now"
		default:
			s += " " + whenText(p.TakeAt, p.Anchor, d.user)
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return ""
	}
	s := "Supplements: " + strings.Join(parts, ", ")
	if len(now) > 0 {
		s += "; reply done once you've taken " + joinAnd(asksFor(now).Supplements) + ", or skip"
	}
	return s + "."
}

// nightText is today's totals against goals, workouts, and supplements,
// asking whether the user went to the gym if a planned session isn't
// logged, and naming supplements due around now. A day with nothing to
// report sends nothing.
func (a *Agent) nightText(ctx context.Context, d *checkinDay) (scheduler.Text, error) {
	var parts []string
	st, err := a.dayStatus(ctx, a.Store, d.user, d.day)
	if err != nil {
		return scheduler.Text{}, err
	}
	var shown []string
	for _, name := range intakeMetrics {
		if g := d.goals[name]; g.Value.Valid || d.totals[name] > 0 {
			shown = append(shown, name)
		}
	}
	if s := st.forUser(shown); s != "" {
		parts = append(parts, s)
	}
	if p := d.work.plan; (p.sched != nil && p.sched.Kind.Valid) || len(d.work.logs) > 0 {
		parts = append(parts, "Workouts: "+d.work.describe(d.day)+".")
	}
	if s := d.supps.dayText(d.day, d.day, d.logs); s != "none due" {
		parts = append(parts, "Supplements: "+s+".")
	}
	var q asks
	if len(d.open) > 0 {
		q.Workout = d.open
		parts = append(parts, "Did you make it to the gym for "+strings.Join(d.open, " + ")+" today? Reply yes or no.")
	}
	if now := d.joined(kindNight); len(now) > 0 {
		q.Supplements = asksFor(now).Supplements
		parts = append(parts, "Reply done once you've taken "+joinAnd(q.Supplements)+", or skip.")
	}
	if len(parts) == 0 {
		return scheduler.Text{}, nil
	}
	body := greeting("Good evening", d.user) + " " + strings.Join(parts, " ")
	if len(q.Workout) == 0 && len(q.Supplements) == 0 {
		return scheduler.Text{Body: body}, nil
	}
	return withAsks(body, q)
}

// waterText nudges the user when they are well behind a straight-line pace
// to their water goal at the nudge's time, or is empty when they aren't.
func (a *Agent) waterText(d *checkinDay, slot string) string {
	times := d.waterTimes(a.settings().int(settingWater))
	i, err := strconv.Atoi(slot)
	if err != nil || i < 1 || i > len(times) {
		return ""
	}
	water, _ := metricByName("water")
	goal := int(d.goals["water"].Value.Int32)
	pace := goal * int(times[i-1]-d.morning) / int(d.night-d.morning)
	behind := pace - d.totals["water"]
	if behind*100 < goal*waterBehind {
		return ""
	}
	return fmt.Sprintf("Water: %s so far today, %s behind pace. Text an amount like 500ml to log it.",
		ratio(water, d.totals["water"], goal), water.format(behind))
}

// Short replies to a check-in, as yes or no.
var (
	yesReplies = []string{"yes", "y", "yep", "yeah", "yup", "done", "did it", "i did", "i went", "went", "took it", "took them", "taken"}
	noReplies  = []string{"no", "n", "nope", "skip", "skipped", "skip it", "didn't", "didnt", "did not", "not today"}
)

// replyWord reads a short yes or no reply, like "Done!" or "nope".
func replyWord(body string) (yes, ok bool) {
	s := strings.Trim(strings.ToLower(strings.TrimSpace(body)), ".! ")
	switch {
	case slices.Contains(yesReplies, s):
		return true, true
	case slices.Contains(noReplies, s):
		return false, true
	}
	return false, false
}

// checkinReply answers a short yes or no reply to the scheduled text just
// before it, when that text asked about one thing: "yes" to "Did you make
// it to the gym?" logs the sessions done, and "done" to a supplement
// reminder logs them taken. Anything else, or a text that asked about both
// a workout and supplements, goes to the LLM.
func (a *Agent) checkinReply(ctx context.Context, userID int64, in store.Message) (string, bool, error) {
	yes, ok := replyWord(in.Body)
	if !ok {
		return "", false, nil
	}
	d, err := a.Store.DeliveryBefore(ctx, store.DeliveryBeforeParams{UserID: userID, BeforeID: in.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var q asks
	if d.Asks != nil {
		if err := json.Unmarshal(d.Asks, &q); err != nil {
			return "", false, err
		}
	}
	user, err := a.Store.GetUser(ctx, userID)
	if err != nil {
		return "", false, err
	}
	if d.LocalDate.Time.Before(addDays(a.localDate(user), -replyDays).Time) {
		return "", false, nil
	}
	switch {
	case len(q.Workout) > 0 && len(q.Supplements) == 0:
		reply, err := a.answerWorkout(ctx, user, in.ID, d.LocalDate, q.Workout, yes)
		return reply, true, err
	case len(q.Supplements) > 0 && len(q.Workout) == 0:
		reply, err := a.answerSupplements(ctx, user, in.ID, d.LocalDate, q.Supplements, yes)
		return reply, true, err
	}
	return "", false, nil
}

// answerWorkout logs the sessions a check-in asked about that are still
// open, done or skipped, through log_workout.
func (a *Agent) answerWorkout(ctx context.Context, user store.User, msgID int64, day pgtype.Date, sessions []string, yes bool) (string, error) {
	r, err := loadWorkouts(ctx, a.Store, user.ID, day, day)
	if err != nil {
		return "", err
	}
	planned, _ := r.day(day).outcomes(a.localDate(user))
	var open []string
	for _, o := range planned {
		if (o.state == sessionOpen || o.state == sessionMissed) && slices.Contains(sessions, o.name) {
			open = append(open, o.name)
		}
	}
	status := sessionSkipped
	if yes {
		status = sessionDone
	}
	logged, errText, err := a.checkinLog(ctx, user.ID, msgID, "log_workout", "sessions", status, day, open)
	if err != nil || errText != "" {
		return errText, err
	}
	st, err := a.workoutStatus(ctx, a.Store, user, day)
	if err != nil {
		return "", err
	}
	if len(logged) == 0 {
		return "That's already logged. " + st.forUser(), nil
	}
	return "Logged " + strings.Join(logged, " + ") + " " + status + ". " + st.forUser(), nil
}

// answerSupplements logs the supplements a check-in asked about that
// aren't logged yet, taken or skipped, through log_supplement.
func (a *Agent) answerSupplements(ctx context.Context, user store.User, msgID int64, day pgtype.Date, names []string, yes bool) (string, error) {
	l, err := loadSupplements(ctx, a.Store, a.timed(user))
	if err != nil {
		return "", err
	}
	logs, err := a.Store.SupplementLogsBetween(ctx, store.SupplementLogsBetweenParams{UserID: user.ID, First: day, Last: day})
	if err != nil {
		return "", err
	}
	var open []string
	for _, n := range names {
		p, ok := l.on(n, day)
		if ok && due(p, day) && !slices.ContainsFunc(logs, func(r store.SupplementLog) bool { return r.SupplementID == p.SupplementID }) {
			open = append(open, n)
		}
	}
	status := "skipped"
	if yes {
		status = "taken"
	}
	logged, errText, err := a.checkinLog(ctx, user.ID, msgID, "log_supplement", "names", status, day, open)
	if err != nil || errText != "" {
		return errText, err
	}
	st, err := a.supplementStatus(ctx, a.Store, user, day)
	if err != nil {
		return "", err
	}
	if len(logged) == 0 {
		return "That's already logged. " + st.forUser(), nil
	}
	return "Logged " + joinAnd(logged) + " " + status + ". " + st.forUser(), nil
}

// checkinLog logs open, the items a short reply covers, with tool, field
// naming them in its arguments. If this text already logged them, as when
// it is run again after a crash, it returns what was logged then instead,
// so the reply is the same. It returns no items when everything was already
// logged, and the reply to send when the tool refused.
func (a *Agent) checkinLog(ctx context.Context, userID, msgID int64, tool, field, status string, day pgtype.Date,
	open []string) ([]string, string, error) {
	prev, err := a.Store.MessageAction(ctx, store.MessageActionParams{MessageID: msgID, Tool: tool})
	switch {
	case err == nil:
		var args map[string]json.RawMessage
		var items []string
		if err := json.Unmarshal([]byte(prev.Args), &args); err != nil {
			return nil, "", err
		}
		if err := json.Unmarshal(args[field], &items); err != nil {
			return nil, "", err
		}
		return items, "", nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, "", err
	case len(open) == 0:
		return nil, "", nil
	}
	result := a.fastTool(ctx, userID, msgID, tool, map[string]any{field: open, "status": status, "date": dateKey(day)})
	if errText, failed := strings.CutPrefix(result, "error: "); failed {
		return nil, "Couldn't log that: " + errText + ".", nil
	}
	return open, "", nil
}

// fastTool runs a tool for a fast-path reply, so the change is recorded and
// undone like any other call.
func (a *Agent) fastTool(ctx context.Context, userID, msgID int64, name string, args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return "error: " + err.Error()
	}
	tc := llm.ToolCall{ID: "fast_path", Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = string(raw)
	return a.runTool(ctx, userID, msgID, tc, map[string]int{})
}

// snoozeText matches "snooze", "snooze 2h", or "snooze for 30 min", which
// mute scheduled texts for that long, an hour by default.
var snoozeText = regexp.MustCompile(`(?i)^\s*snooze(?:\s+(?:for\s+)?(\d+)\s*(m|mins?|minutes?|h|hrs?|hours?))?\s*[.!]?\s*$`)

var unmuteText = regexp.MustCompile(`(?i)^\s*unmute\s*[.!]?\s*$`)

// snoozeFor returns how long a snooze text mutes texts for.
func snoozeFor(body string) (time.Duration, bool) {
	m := snoozeText.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	if m[1] == "" {
		return time.Hour, true
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	d := time.Duration(n) * time.Minute
	if strings.HasPrefix(strings.ToLower(m[2]), "h") {
		d = time.Duration(n) * time.Hour
	}
	return d, d > 0 && d <= maxSnooze
}

// snooze mutes scheduled texts for d through set_profile, or unmutes them
// when d is zero.
func (a *Agent) snooze(ctx context.Context, userID, msgID int64, d time.Duration) (string, error) {
	user, err := a.Store.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	loc := a.location(user)
	until := "off"
	if d > 0 {
		// Rounded up to the minute, so a snooze is never shorter than asked.
		until = a.now().Add(d).Add(time.Minute - time.Nanosecond).Truncate(time.Minute).In(loc).Format(muteLayout)
	}
	// When the text is run again, as after a crash, it keeps the time it
	// first muted until instead of muting again from now.
	prev, err := a.Store.MessageAction(ctx, store.MessageActionParams{MessageID: msgID, Tool: "set_profile"})
	switch {
	case err == nil:
		var args struct {
			MuteUntil string `json:"mute_until"`
		}
		if err := json.Unmarshal([]byte(prev.Args), &args); err != nil {
			return "", err
		}
		until = args.MuteUntil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	default:
		result := a.fastTool(ctx, userID, msgID, "set_profile", map[string]any{"name": nil, "timezone": nil, "wake_time": nil,
			"bedtime": nil, "morning_checkin": nil, "night_checkin": nil, "mute_until": until})
		if errText, failed := strings.CutPrefix(result, "error: "); failed {
			return upperFirst(errText) + ".", nil
		}
	}
	if until == "off" {
		return "Unmuted. Scheduled texts are back on.", nil
	}
	end, err := time.ParseInLocation(muteLayout, until, loc)
	if err != nil {
		return "", err
	}
	return "Snoozed until " + a.muteText(end, user) + ". Text unmute to turn scheduled texts back on.", nil
}

// muteText shows when a mute ends, like "4:15 PM" today or "Fri Oct 30,
// 9:00 AM" on another day.
func (a *Agent) muteText(t time.Time, user store.User) string {
	loc := a.location(user)
	lt := t.In(loc)
	if y, m, d := a.now().In(loc).Date(); lt.Year() == y && lt.Month() == m && lt.Day() == d {
		return lt.Format("3:04 PM")
	}
	return lt.Format("Mon Jan 2, 3:04 PM")
}
