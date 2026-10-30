package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// statsLookback is how many days before today get_day and get_stats
	// reach, and how far back a current streak is counted.
	statsLookback = 366
	// defaultGoalBand is the goal band when Agent.GoalBand is unset.
	defaultGoalBand = 10
)

// statsMetrics are what get_stats reports on.
var statsMetrics = append(slices.Clone(intakeMetrics), "sleep", "weight", "workout", "supplement")

// statsPeriods are the periods get_stats takes.
var statsPeriods = []string{"last_days", "this_week", "last_week", "this_month", "last_month", "dates"}

func (a *Agent) goalBand() int {
	return a.settings().int(settingBand)
}

// onGoal reports whether a day's total meets its goal: at or above it for
// protein, water, and sleep, and within the goal band of it for calories,
// carbs, and fat.
func (a *Agent) onGoal(m metric, total, goal int) bool {
	if m.atLeast {
		return total >= goal
	}
	d := total - goal
	if d < 0 {
		d = -d
	}
	return d*100 <= goal*a.goalBand()
}

// span is a range of local days, first to last.
type span struct {
	first, last pgtype.Date
	// name is like "this week" or "last 7 days", or empty for plain dates.
	name string
}

func (s span) days() int {
	return daysBetween(s.first, s.last) + 1
}

func (s span) has(d pgtype.Date) bool {
	return !d.Time.Before(s.first.Time) && !d.Time.After(s.last.Time)
}

// dates shows the span's days, like "Oct 26 to Oct 29", or "Oct 26" for
// one day.
func (s span) dates() string {
	if s.first.Time.Equal(s.last.Time) {
		return shortDate(s.first)
	}
	return shortDate(s.first) + " to " + shortDate(s.last)
}

// String names the span, like "this week (Oct 26 to Oct 29)".
func (s span) String() string {
	if s.name == "" {
		return s.dates()
	}
	return s.name + " (" + s.dates() + ")"
}

// before is the span to compare s with: the whole week or month before a
// week or month, or else as many days just before.
func (s span) before() span {
	switch s.name {
	case "this week", "last week":
		monday := weekStart(s.first)
		return span{addDays(monday, -7), addDays(monday, -1), "the week before"}
	case "this month", "last month":
		return span{pgtype.Date{Time: s.first.Time.AddDate(0, -1, 0), Valid: true}, addDays(s.first, -1), "the month before"}
	}
	n := s.days()
	name := fmt.Sprintf("the %d days before", n)
	if n == 1 {
		name = "the day before"
	}
	return span{addDays(s.first, -n), addDays(s.first, -1), name}
}

// statsDate parses a YYYY-MM-DD local date to look up, which must be today
// or up to statsLookback days before.
func statsDate(s string, today pgtype.Date) (pgtype.Date, string) {
	day, err := parseDay(strings.TrimSpace(s))
	if err != nil {
		return day, fmt.Sprintf("date %q is not YYYY-MM-DD", s)
	}
	if day.Time.After(today.Time) {
		return day, "date " + dateKey(day) + " is in the future; today is " + dateKey(today)
	}
	if day.Time.Before(addDays(today, -statsLookback).Time) {
		return day, fmt.Sprintf("date %s is more than %d days ago", dateKey(day), statsLookback)
	}
	return day, ""
}

type statsArgs struct {
	Metric string  `json:"metric"`
	Name   *string `json:"name"`
	Period string  `json:"period"`
	Days   *int    `json:"days"`
	First  *string `json:"first"`
	Last   *string `json:"last"`
}

// statsSpan works out the days a get_stats period covers, or returns why
// it can't.
func statsSpan(args statsArgs, today pgtype.Date) (span, string) {
	if args.Days != nil && args.Period != "last_days" {
		return span{}, "days is only for last_days"
	}
	if (args.First != nil || args.Last != nil) && args.Period != "dates" {
		return span{}, "first and last are only for dates"
	}
	monday := weekStart(today)
	month := pgtype.Date{Time: time.Date(today.Time.Year(), today.Time.Month(), 1, 0, 0, 0, 0, time.UTC), Valid: true}
	switch args.Period {
	case "last_days":
		if args.Days == nil || *args.Days < 1 || *args.Days > statsLookback {
			return span{}, fmt.Sprintf("last_days needs days from 1 to %d", statsLookback)
		}
		n := *args.Days
		if n == 1 {
			return span{today, today, "today"}, ""
		}
		return span{addDays(today, 1-n), today, fmt.Sprintf("last %d days", n)}, ""
	case "this_week":
		return span{monday, today, "this week"}, ""
	case "last_week":
		return span{addDays(monday, -7), addDays(monday, -1), "last week"}, ""
	case "this_month":
		return span{month, today, "this month"}, ""
	case "last_month":
		return span{pgtype.Date{Time: month.Time.AddDate(0, -1, 0), Valid: true}, addDays(month, -1), "last month"}, ""
	case "dates":
		if args.First == nil {
			return span{}, "dates needs first"
		}
		first, errText := statsDate(*args.First, today)
		if errText != "" {
			return span{}, errText
		}
		last := today
		if args.Last != nil {
			if last, errText = statsDate(*args.Last, today); errText != "" {
				return span{}, errText
			}
		}
		if last.Time.Before(first.Time) {
			return span{}, "last is before first"
		}
		return span{first: first, last: last}, ""
	}
	return span{}, fmt.Sprintf("period %q must be one of %s", args.Period, strings.Join(statsPeriods, ", "))
}

func (a *Agent) getStats(ctx context.Context, c *call, args statsArgs) (string, error) {
	if !slices.Contains(statsMetrics, args.Metric) {
		return fmt.Sprintf("error: metric %q must be one of %s", args.Metric, strings.Join(statsMetrics, ", ")), nil
	}
	if args.Name != nil && args.Metric != "workout" && args.Metric != "supplement" {
		return "error: name is only for workout and supplement", nil
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	sp, errText := statsSpan(args, today)
	if errText != "" {
		return "error: " + errText, nil
	}
	switch args.Metric {
	case "sleep":
		return a.sleepStats(ctx, c.q, user.ID, sp, today)
	case "weight":
		return a.weightStats(ctx, c.q, user.ID, sp)
	case "workout":
		return a.workoutStats(ctx, c.q, user.ID, sp, today, args.Name)
	case "supplement":
		return a.supplementStats(ctx, c.q, user, sp, today, args.Name)
	}
	m, _ := metricByName(args.Metric)
	return a.intakeStats(ctx, c.q, user.ID, m, sp, today)
}

// statsFrom is the first day stats over sp need: the span before it, and
// the days a current streak can reach back to.
func statsFrom(sp span, today pgtype.Date) pgtype.Date {
	from := addDays(today, -statsLookback-1)
	if prev := sp.before(); prev.first.Time.Before(from.Time) {
		from = prev.first
	}
	return from
}

// daily is one metric's value on each day with anything logged, by date,
// and its goals, oldest first.
type daily struct {
	m      metric
	values map[string]int
	goals  []store.Goal
	// noun is what one value is for, day or night.
	noun string
	// partial is true when today's value isn't judged until the day ends.
	partial bool
	// when names the day a current streak runs through.
	when func(pgtype.Date) string
}

// judge reports whether d has a value, whether it also has a goal, and
// whether it meets that goal.
func (a *Agent) judge(s daily, d pgtype.Date) (logged, withGoal, on bool) {
	v, ok := s.values[dateKey(d)]
	if !ok {
		return false, false, false
	}
	g := goalOn(s.goals, d)
	if !g.Value.Valid {
		return true, false, false
	}
	return true, true, a.onGoal(s.m, v, int(g.Value.Int32))
}

// tally is a daily metric over a span.
type tally struct {
	logged, total int
	// withGoal counts the days logged with a goal in force, and onGoal those
	// on goal; best is the most days on goal in a row.
	withGoal, onGoal, best int
	// goals are the goals in force on the span's days, each once.
	goals []int
}

func (t tally) avg() int {
	return int(math.Round(float64(t.total) / float64(t.logged)))
}

func (a *Agent) tallyDays(s daily, sp span) tally {
	var t tally
	run := 0
	for d := sp.first; !d.Time.After(sp.last.Time); d = addDays(d, 1) {
		if g := goalOn(s.goals, d); g.Value.Valid && !slices.Contains(t.goals, int(g.Value.Int32)) {
			t.goals = append(t.goals, int(g.Value.Int32))
		}
		logged, withGoal, on := a.judge(s, d)
		if logged {
			t.logged++
			t.total += s.values[dateKey(d)]
		}
		if withGoal {
			t.withGoal++
		}
		if !on {
			run = 0
			continue
		}
		t.onGoal++
		run++
		t.best = max(t.best, run)
	}
	return t
}

// streak counts the days on goal in a row ending on last.
func (a *Agent) streak(s daily, last pgtype.Date) int {
	n := 0
	for d := last; n < statsLookback; d = addDays(d, -1) {
		if _, _, on := a.judge(s, d); !on {
			break
		}
		n++
	}
	return n
}

// goalRule says what counts as on goal, like "within 10% of 1,800 cal" or
// "at least 2 L", or "each day's goal" when it changed. noun is day or
// night.
func (a *Agent) goalRule(m metric, goals []int, noun string) string {
	of := "each " + noun + "'s goal"
	if len(goals) == 1 {
		of = m.format(goals[0])
	}
	if m.atLeast {
		return "at least " + of
	}
	return fmt.Sprintf("within %d%% of %s", a.goalBand(), of)
}

// tallyText is a tally over days of s's noun, like "5 of 6 days logged,
// 1,850 cal average a day logged, 9,250 cal total, on goal (within 10% of
// 1,800 cal) on 3 of the 5 days logged, best streak 2 days".
func (a *Agent) tallyText(s daily, t tally, days int, total bool) string {
	if t.logged == 0 {
		return "nothing logged on " + plural(days, s.noun)
	}
	out := fmt.Sprintf("%d of %s logged", t.logged, plural(days, s.noun))
	if t.logged == 1 {
		out += ", " + s.m.format(t.total)
	} else {
		out += fmt.Sprintf(", %s average a %s logged", s.m.format(t.avg()), s.noun)
		if total {
			out += ", " + s.m.format(t.total) + " total"
		}
	}
	if len(t.goals) == 0 {
		return out
	}
	of := plural(t.withGoal, s.noun) + " logged"
	if t.withGoal < t.logged {
		of += " with a goal"
	}
	out += fmt.Sprintf(", on goal (%s) on %d of the %s", a.goalRule(s.m, t.goals, s.noun), t.onGoal, of)
	if t.onGoal > 0 && days > 1 {
		out += ", best streak " + plural(t.best, s.noun)
	}
	return out
}

// dailyStats reports on a daily metric over sp, against the span before
// it, and its current streak.
func (a *Agent) dailyStats(s daily, sp span, today pgtype.Date, total bool) string {
	var parts []string
	judged := sp
	streakEnd := today
	if _, ok := s.values[dateKey(today)]; s.partial || !ok {
		streakEnd = addDays(today, -1)
	}
	if s.partial && sp.has(today) {
		judged = span{first: sp.first, last: addDays(today, -1)}
	}
	var t tally
	if !judged.last.Time.Before(judged.first.Time) {
		t = a.tallyDays(s, judged)
		text := a.tallyText(s, t, judged.days(), total)
		if !judged.last.Time.Equal(sp.last.Time) {
			text = "complete days " + judged.dates() + ": " + text
		}
		parts = append(parts, text)
	}
	if s.partial && sp.has(today) {
		parts = append(parts, "today so far: "+s.m.format(s.values[dateKey(today)])+", counted once the day ends")
	}
	if slices.ContainsFunc(s.goals, func(g store.Goal) bool { return g.Value.Valid }) {
		if n := a.streak(s, streakEnd); n > 0 {
			parts = append(parts, fmt.Sprintf("current streak: %s on goal in a row through %s", plural(n, s.noun), s.when(streakEnd)))
		} else {
			parts = append(parts, "current streak: none, "+s.when(streakEnd)+" wasn't on goal")
		}
	}
	prev := sp.before()
	pt := a.tallyDays(s, prev)
	switch {
	case pt.logged == 0:
		parts = append(parts, prev.String()+": nothing logged to compare")
	case pt.logged == 1:
		parts = append(parts, fmt.Sprintf("%s: %s over 1 %s logged", prev, s.m.format(pt.total), s.noun))
	default:
		parts = append(parts, fmt.Sprintf("%s: %s average over %d %ss logged", prev, s.m.format(pt.avg()), pt.logged, s.noun))
	}
	if t.logged > 0 && pt.logged > 0 {
		switch d := t.avg() - pt.avg(); {
		case d > 0:
			parts[len(parts)-1] += ", so the average is up " + s.m.format(d)
		case d < 0:
			parts[len(parts)-1] += ", so the average is down " + s.m.format(-d)
		default:
			parts[len(parts)-1] += ", so the average is the same"
		}
	}
	return s.m.label + ", " + sp.String() + ": " + strings.Join(parts, "; ") + "."
}

// intakeStats reports on calories, a macro, or water over sp.
func (a *Agent) intakeStats(ctx context.Context, q *store.Queries, userID int64, m metric, sp span, today pgtype.Date) (string, error) {
	rows, err := q.IntakeDaily(ctx, store.IntakeDailyParams{UserID: userID, Metric: m.name, First: statsFrom(sp, today), Last: today})
	if err != nil {
		return "", err
	}
	goals, err := q.GoalHistory(ctx, store.GoalHistoryParams{UserID: userID, Metric: m.name})
	if err != nil {
		return "", err
	}
	s := daily{m: m, values: map[string]int{}, goals: goals, noun: "day", partial: true,
		when: func(d pgtype.Date) string { return dayWord(d, today) }}
	for _, r := range rows {
		s.values[dateKey(r.LocalDate)] = int(r.Total)
	}
	tracked := m.name == "calories" || m.name == "water" || len(rows) > 0 ||
		slices.ContainsFunc(goals, func(g store.Goal) bool { return g.Value.Valid })
	if !tracked {
		return fmt.Sprintf("%s: not tracked; the user has no %s goal and has logged none. They can start by setting a goal.",
			m.label, m.label), nil
	}
	return a.dailyStats(s, sp, today, true), nil
}

// sleepStats reports on the nights counting toward the days of sp. A night
// is complete once logged, so today counts.
func (a *Agent) sleepStats(ctx context.Context, q *store.Queries, userID int64, sp span, today pgtype.Date) (string, error) {
	rows, err := q.SleepBetween(ctx, store.SleepBetweenParams{UserID: userID, First: statsFrom(sp, today), Last: today})
	if err != nil {
		return "", err
	}
	goals, err := q.GoalHistory(ctx, store.GoalHistoryParams{UserID: userID, Metric: "sleep"})
	if err != nil {
		return "", err
	}
	m, _ := metricByName("sleep")
	s := daily{m: m, values: map[string]int{}, goals: goals, noun: "night",
		when: func(d pgtype.Date) string { return nightWord(d, today) }}
	for _, r := range rows {
		s.values[dateKey(r.LocalDate)] = int(r.Minutes)
	}
	return a.dailyStats(s, sp, today, false), nil
}

// weightStats reports on the weigh-ins in sp, the change in the 7-day
// average over it, and the distance to the target at its end. Each day
// weighed counts its first weigh-in, in tenths of a pound.
func (a *Agent) weightStats(ctx context.Context, q *store.Queries, userID int64, sp span) (string, error) {
	prev := sp.before()
	rows, err := q.WeightBetween(ctx, store.WeightBetweenParams{UserID: userID, First: prev.first, Last: sp.last})
	if err != nil {
		return "", err
	}
	type day struct {
		d pgtype.Date
		v int
	}
	var days []day
	for _, r := range rows {
		if n := len(days); n > 0 && days[n-1].d.Time.Equal(r.LocalDate.Time) {
			continue
		}
		days = append(days, day{r.LocalDate, tenths(int(r.Grams))})
	}
	type summary struct {
		n, sum int
		lo, hi day
	}
	over := func(s span) summary {
		var out summary
		for _, d := range days {
			if !s.has(d.d) {
				continue
			}
			if out.n == 0 || d.v < out.lo.v {
				out.lo = d
			}
			if out.n == 0 || d.v > out.hi.v {
				out.hi = d
			}
			out.n++
			out.sum += d.v
		}
		return out
	}
	avg := func(s summary) int { return int(math.Round(float64(s.sum) / float64(s.n))) }

	var parts []string
	cur := over(sp)
	switch cur.n {
	case 0:
		parts = append(parts, "no weigh-ins")
	case 1:
		parts = append(parts, "1 day weighed, "+lbs(cur.sum)+" on "+shortDate(cur.lo.d))
	default:
		parts = append(parts, fmt.Sprintf("%d days weighed, %s average, lowest %s on %s, highest %s on %s", cur.n, lbs(avg(cur)),
			lbs(cur.lo.v), shortDate(cur.lo.d), lbs(cur.hi.v), shortDate(cur.hi.d)))
	}
	end, err := loadWeightTrend(ctx, q, userID, sp.last)
	if err != nil {
		return "", err
	}
	start, err := loadWeightTrend(ctx, q, userID, addDays(sp.first, -1))
	if err != nil {
		return "", err
	}
	if end.days == 0 {
		parts = append(parts, fmt.Sprintf("no weigh-ins in the %d days ending %s", weightWeek, shortDate(sp.last)))
	} else {
		text := fmt.Sprintf("%d-day average as of %s: %s", weightWeek, shortDate(sp.last), lbs(end.avg))
		was := shortDate(addDays(sp.first, -1))
		switch d := end.avg - start.avg; {
		case start.days == 0:
			text += fmt.Sprintf(", no weigh-ins in the %d days ending %s to compare", weightWeek, was)
		case d < 0:
			text += fmt.Sprintf(", down %s from %s as of %s", lbs(-d), lbs(start.avg), was)
		case d > 0:
			text += fmt.Sprintf(", up %s from %s as of %s", lbs(d), lbs(start.avg), was)
		default:
			text += fmt.Sprintf(", the same as of %s", was)
		}
		parts = append(parts, text)
	}
	if end.hasTarget {
		text := "target weight: " + lbs(end.target)
		switch d := end.avg - end.target; {
		case end.days == 0:
		case d > 0:
			text += fmt.Sprintf(", %d-day average %s above", weightWeek, lbs(d))
		case d < 0:
			text += fmt.Sprintf(", %d-day average %s below", weightWeek, lbs(-d))
		default:
			text += fmt.Sprintf(", %d-day average on target", weightWeek)
		}
		parts = append(parts, text)
	}
	switch p := over(prev); p.n {
	case 0:
		parts = append(parts, prev.String()+": no weigh-ins to compare")
	case 1:
		parts = append(parts, prev.String()+": "+lbs(p.sum)+" over 1 day weighed")
	default:
		parts = append(parts, fmt.Sprintf("%s: %s average over %d days weighed", prev, lbs(avg(p)), p.n))
	}
	return "body weight, " + sp.String() + ": " + strings.Join(parts, "; ") + ".", nil
}

// sessionCounts tallies planned sessions by outcome, and extra sessions.
type sessionCounts struct {
	planned, extras int
	states          map[string]int
}

// String is like "3 of 6 planned sessions done, 1 skipped, 1 missed, 1 to
// go, plus 1 extra session".
func (c *sessionCounts) String() string {
	if c.planned == 0 {
		s := "nothing planned"
		if c.extras > 0 {
			s += fmt.Sprintf(", %s done", plural(c.extras, "extra session"))
		}
		return s
	}
	s := fmt.Sprintf("%d of %s done", c.states[sessionDone], plural(c.planned, "planned session"))
	for _, st := range []struct{ state, word string }{{sessionSkipped, "skipped"}, {sessionMissed, "missed"}, {sessionOpen, "to go"}} {
		if n := c.states[st.state]; n > 0 {
			s += fmt.Sprintf(", %d %s", n, st.word)
		}
	}
	if c.extras > 0 {
		s += ", plus " + plural(c.extras, "extra session")
	}
	return s
}

// count tallies the sessions from first to last as of today, in all and
// by session name in the order first seen. With name set, only that
// session counts. The range must cover the days.
func (r *workoutRange) count(first, last, today pgtype.Date, name string) (*sessionCounts, map[string]*sessionCounts, []string) {
	all := &sessionCounts{states: map[string]int{}}
	byName := map[string]*sessionCounts{}
	var names []string
	get := func(n string) *sessionCounts {
		if byName[n] == nil {
			byName[n] = &sessionCounts{states: map[string]int{}}
			names = append(names, n)
		}
		return byName[n]
	}
	for d := first; !d.Time.After(last.Time); d = addDays(d, 1) {
		planned, extras := r.day(d).outcomes(today)
		for _, p := range planned {
			if name != "" && p.name != name {
				continue
			}
			for _, c := range []*sessionCounts{all, get(p.name)} {
				c.planned++
				c.states[p.state]++
			}
		}
		for _, e := range extras {
			if name != "" && e != name {
				continue
			}
			all.extras++
			get(e).extras++
		}
	}
	return all, byName, names
}

// doneStreak counts the planned sessions named name, or any when name is
// empty, done in a row up to today, back to first. Today's sessions not
// logged yet don't break it.
func (r *workoutRange) doneStreak(first, today pgtype.Date, name string) int {
	n := 0
	for d := today; !d.Time.Before(first.Time); d = addDays(d, -1) {
		planned, _ := r.day(d).outcomes(today)
		for i := len(planned) - 1; i >= 0; i-- {
			p := planned[i]
			if (name != "" && p.name != name) || p.state == sessionOpen {
				continue
			}
			if p.state != sessionDone {
				return n
			}
			n++
		}
	}
	return n
}

// workoutStats reports on the planned and extra sessions in sp, by
// session, against the span before it, and the current streak.
func (a *Agent) workoutStats(ctx context.Context, q *store.Queries, userID int64, sp span, today pgtype.Date, name *string) (string, error) {
	session := ""
	if name != nil {
		session = strings.Join(strings.Fields(strings.ToLower(*name)), " ")
	}
	from := statsFrom(sp, today)
	r, err := loadWorkouts(ctx, q, userID, from, today)
	if err != nil {
		return "", err
	}
	all, byName, names := r.count(sp.first, sp.last, today, session)
	label := "workouts"
	if session != "" {
		label = session
	}
	lines := []string{label + ", " + sp.String() + ": " + all.String()}
	if session == "" && len(names) > 1 {
		var parts []string
		for _, n := range names {
			parts = append(parts, n+": "+byName[n].String())
		}
		lines = append(lines, "by session: "+strings.Join(parts, "; "))
	}
	what := "planned session"
	if session != "" {
		what = "planned " + session + " session"
	}
	lines = append(lines, "current streak: "+plural(r.doneStreak(from, today, session), what)+" done in a row")
	prev := sp.before()
	p, _, _ := r.count(prev.first, prev.last, today, session)
	lines = append(lines, prev.String()+": "+p.String())
	return strings.Join(lines, "\n"), nil
}

// doseCounts tallies the due doses of supplements by outcome.
type doseCounts struct {
	due    int
	states map[string]int
}

// String is like "12 of 14 due doses taken, 1 skipped, 1 still due today".
func (c *doseCounts) String() string {
	if c.due == 0 {
		return "none due"
	}
	s := fmt.Sprintf("%d of %s taken", c.states["taken"], plural(c.due, "due dose"))
	for _, st := range []struct{ state, word string }{{"skipped", "skipped"}, {sessionMissed, "missed"}, {sessionOpen, "still due today"}} {
		if n := c.states[st.state]; n > 0 {
			s += fmt.Sprintf(", %d %s", n, st.word)
		}
	}
	return s
}

// count tallies the due doses from first to last as of today, of the
// supplement named name or every one when name is empty. logs are the
// days' logs by date.
func (l *supplementList) count(first, last, today pgtype.Date, logs map[string][]store.SupplementLog, name string) *doseCounts {
	c := &doseCounts{states: map[string]int{}}
	for d := first; !d.Time.After(last.Time); d = addDays(d, 1) {
		for _, o := range l.doses(d, today, logs[dateKey(d)]) {
			if name != "" && o.name != name {
				continue
			}
			c.due++
			c.states[o.state]++
		}
	}
	return c
}

// takenStreak counts the due days in a row name was taken, up to today and
// back to first. Today not logged yet doesn't break it, and days it isn't
// due are passed over.
func (l *supplementList) takenStreak(name string, first, today pgtype.Date, logs map[string][]store.SupplementLog) int {
	n := 0
	for d := today; !d.Time.Before(first.Time); d = addDays(d, -1) {
		if _, ok := l.on(name, d); !ok {
			return n
		}
		for _, o := range l.doses(d, today, logs[dateKey(d)]) {
			switch {
			case o.name != name || o.state == sessionOpen:
			case o.state != "taken":
				return n
			default:
				n++
			}
		}
	}
	return n
}

// supplementStats reports on each supplement's due doses in sp, or one
// supplement's, against the span before it, and its current streak.
func (a *Agent) supplementStats(ctx context.Context, q *store.Queries, user store.User, sp span, today pgtype.Date, name *string) (string, error) {
	l, err := loadSupplements(ctx, q, a.timed(user))
	if err != nil {
		return "", err
	}
	from := statsFrom(sp, today)
	rows, err := q.SupplementLogsBetween(ctx, store.SupplementLogsBetweenParams{UserID: user.ID, First: from, Last: today})
	if err != nil {
		return "", err
	}
	logs := map[string][]store.SupplementLog{}
	for _, r := range rows {
		logs[dateKey(r.LocalDate)] = append(logs[dateKey(r.LocalDate)], r)
	}
	var names []string
	if name != nil {
		n, errText := cleanSupplementName(*name)
		if errText != "" {
			return "error: " + errText, nil
		}
		if _, ok := l.plans[n]; !ok {
			return fmt.Sprintf("error: %s has never been on the supplement list; today it has %s", n, l.listNames(today)), nil
		}
		names = []string{n}
	} else {
		for _, n := range l.names {
			if _, onToday := l.on(n, today); onToday || l.count(sp.first, sp.last, today, logs, n).due > 0 {
				names = append(names, n)
			}
		}
	}
	head := "supplements, " + sp.String() + ":"
	if len(names) == 0 {
		return head + " none on the list.", nil
	}
	prev := sp.before()
	lines := []string{head}
	for _, n := range names {
		line := n + ": " + l.count(sp.first, sp.last, today, logs, n).String()
		if s := l.takenStreak(n, from, today, logs); s > 0 {
			line += fmt.Sprintf("; current streak: taken %s in a row when due", plural(s, "day"))
		} else if _, ok := l.on(n, today); ok {
			line += "; current streak: none"
		}
		line += "; " + prev.String() + ": " + l.count(prev.first, prev.last, today, logs, n).String()
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

type dayArgs struct {
	Date *string `json:"date"`
}

func (a *Agent) getDay(ctx context.Context, c *call, args dayArgs) (string, error) {
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	day := today
	if args.Date != nil {
		var errText string
		if day, errText = statsDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	head := dayName(day)
	switch {
	case day.Time.Equal(today.Time):
		head += " (today)"
	case day.Time.Equal(addDays(today, -1).Time):
		head += " (yesterday)"
	}
	lines := []string{head}

	totals, err := a.dayTotals(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	lines = append(lines, "totals: "+totals)
	logs, err := c.q.IntakeOn(ctx, store.IntakeOnParams{UserID: c.userID, Day: day})
	if err != nil {
		return "", err
	}
	loc := a.location(user)
	var entries []string
	for _, l := range logs {
		m, _ := metricByName(l.Metric)
		entries = append(entries, fmt.Sprintf("#%d at %s: %s", l.ID, l.LoggedAt.Time.In(loc).Format("3:04 PM"), intakeSummary(m, l.Amount)))
	}
	if len(entries) == 0 {
		entries = []string{"none"}
	}
	lines = append(lines, "amounts logged: "+strings.Join(entries, ", "))

	sleep, _ := metricByName("sleep")
	night, err := c.q.SleepOn(ctx, store.SleepOnParams{UserID: c.userID, Day: day})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		lines = append(lines, "sleep: nothing logged for "+nightWord(day, today))
	case err != nil:
		return "", err
	default:
		goals, err := c.q.GoalHistory(ctx, store.GoalHistoryParams{UserID: c.userID, Metric: "sleep"})
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("sleep: %s (#%d: %s)", sleepProgress(sleep, int(night.Minutes), goalOn(goals, day),
			nightWord(day, today)), night.ID, sleepSummary(night)))
	}

	r, err := loadWorkouts(ctx, c.q, c.userID, day, day)
	if err != nil {
		return "", err
	}
	w := r.day(day)
	line := "training plan: " + w.plan.describe() + "; workouts: " + w.describe(today)
	for _, l := range w.logs {
		line += fmt.Sprintf(" (#%d: %s)", l.ID, workoutSummary(l))
	}
	lines = append(lines, line)

	sup, err := a.supplementStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	line = sup.day
	supLogs, err := c.q.SupplementLogsBetween(ctx, store.SupplementLogsBetweenParams{UserID: c.userID, First: day, Last: day})
	if err != nil {
		return "", err
	}
	if len(supLogs) > 0 {
		l, err := loadSupplements(ctx, c.q, a.timed(user))
		if err != nil {
			return "", err
		}
		for _, s := range supLogs {
			line += fmt.Sprintf(" (#%d: %s %s)", s.ID, l.ids[s.SupplementID], s.Status)
		}
	}
	lines = append(lines, line)

	wt, err := a.weightStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	line = wt.day
	weighIns, err := c.q.WeightBetween(ctx, store.WeightBetweenParams{UserID: c.userID, First: day, Last: day})
	if err != nil {
		return "", err
	}
	for _, l := range weighIns {
		line += fmt.Sprintf(" (#%d: %s)", l.ID, weighIn(l.Grams))
	}
	if len(weighIns) == 0 {
		latest, err := c.q.WeightThrough(ctx, store.WeightThroughParams{UserID: c.userID, Day: day})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return "", err
		default:
			line += "; latest weight before then: " + weighIn(latest.Grams) + " " + dayWord(latest.LocalDate, today)
		}
	}
	lines = append(lines, line)
	return strings.Join(lines, "\n"), nil
}

// dayTotals is day's intake totals against its goals. Today shows what's
// left; a past day shows whether each total was on goal.
func (a *Agent) dayTotals(ctx context.Context, q *store.Queries, user store.User, day pgtype.Date) (string, error) {
	today := a.localDate(user)
	if day.Time.Equal(today.Time) {
		st, err := a.dayStatus(ctx, q, user, day)
		if err != nil {
			return "", err
		}
		return st.forModel(), nil
	}
	rows, err := q.IntakeTotals(ctx, store.IntakeTotalsParams{UserID: user.ID, Day: day})
	if err != nil {
		return "", err
	}
	totals := map[string]int{}
	for _, r := range rows {
		totals[r.Metric] = int(r.Total)
	}
	goals, err := q.GoalsOn(ctx, store.GoalsOnParams{UserID: user.ID, Day: day})
	if err != nil {
		return "", err
	}
	byMetric := goalsByMetric(goals)
	when := dayWord(day, today)
	var parts []string
	for _, name := range intakeMetrics {
		m, _ := metricByName(name)
		g := byMetric[m.name]
		if m.name != "calories" && totals[m.name] == 0 && !g.Value.Valid {
			continue
		}
		parts = append(parts, m.label+": "+a.verdict(m, totals[m.name], g, when))
	}
	return strings.Join(parts, "; ") + ".", nil
}

// verdict is a past day's total against its goal, like "1,700 / 1,800 cal
// yesterday, 100 cal under, on goal" or "1.5 / 2 L yesterday, 500 ml short,
// goal missed". A day with nothing logged isn't judged.
func (a *Agent) verdict(m metric, total int, g store.Goal, when string) string {
	if total == 0 {
		if g.Value.Valid {
			return "nothing logged " + when + " (goal " + m.format(int(g.Value.Int32)) + ")"
		}
		return "nothing logged " + when
	}
	if !g.Value.Valid {
		return m.format(total) + " " + when
	}
	goal := int(g.Value.Int32)
	s := ratio(m, total, goal) + " " + when
	on := a.onGoal(m, total, goal)
	switch {
	case m.atLeast && on:
		return s + ", goal hit"
	case m.atLeast:
		return s + ", " + m.format(goal-total) + " short, goal missed"
	case total < goal:
		s += ", " + m.format(goal-total) + " under"
	case total > goal:
		s += ", " + m.format(total-goal) + " over"
	}
	if on {
		return s + ", on goal"
	}
	return s + ", off goal"
}

// questionSection tells the model how to answer questions with get_day and
// get_stats.
func (a *Agent) questionSection() string {
	return fmt.Sprintf("\n## Questions\nAnswer questions about how the user is doing from get_day and get_stats, never from your "+
		"own math. get_day is one day: totals against goals and what's left today, everything logged, the training plan and "+
		"workouts, supplements, and weight. get_stats is one metric over a period: averages, totals, days on goal, streaks, "+
		"workouts planned, done, skipped, and missed by session, supplement doses taken, weight change, and the period before "+
		"to compare. The sections above show only today and the last week, so call a tool for any other day, and for any "+
		"average, total, or count. Periods: last_days with days, like 14 for \"the last 2 weeks\"; this_week or last_week "+
		"(Monday to Sunday); this_month or last_month; dates with first and last for named days. Name a session, like legs, "+
		"or a supplement in name to ask about just that one. Today's calories, macros, and water count toward averages once "+
		"the day ends. On goal means at least the goal for protein, water, and sleep, and within %d%% of it for calories, "+
		"carbs, and fat. Use the numbers in the result as given.\n", a.goalBand())
}
