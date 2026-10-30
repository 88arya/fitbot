package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5/pgtype"
)

// The weekly summary goes out on Monday at the morning check-in time, just
// before the check-in, and covers the Monday-to-Sunday week before. Every
// day in it is over, so each day is judged the way get_stats judges
// last_week.

const kindWeekly = "weekly_summary"

// weeklyDue reports whether the weekly summary goes out on day.
func weeklyDue(day pgtype.Date) bool {
	return day.Time.Weekday() == time.Monday
}

// weeklyText is the week before day: each intake metric's average and days
// on goal, sleep the same way, workouts done against planned by session,
// each supplement's doses taken, and the 7-day average weight against the
// week before. It is empty when there is nothing to report.
func (a *Agent) weeklyText(ctx context.Context, user store.User, day pgtype.Date) (string, error) {
	q := a.Store
	monday := weekStart(day)
	sp := span{addDays(monday, -7), addDays(monday, -1), "last week"}
	var lines []string
	for _, name := range intakeMetrics {
		m, _ := metricByName(name)
		rows, err := q.IntakeDaily(ctx, store.IntakeDailyParams{UserID: user.ID, Metric: name, First: sp.first, Last: sp.last})
		if err != nil {
			return "", err
		}
		goals, err := q.GoalHistory(ctx, store.GoalHistoryParams{UserID: user.ID, Metric: name})
		if err != nil {
			return "", err
		}
		s := daily{m: m, values: map[string]int{}, goals: goals, noun: "day"}
		for _, r := range rows {
			s.values[dateKey(r.LocalDate)] = int(r.Total)
		}
		lines = append(lines, a.weeklyLine(s, sp))
	}
	sleep, err := q.SleepBetween(ctx, store.SleepBetweenParams{UserID: user.ID, First: sp.first, Last: sp.last})
	if err != nil {
		return "", err
	}
	goals, err := q.GoalHistory(ctx, store.GoalHistoryParams{UserID: user.ID, Metric: "sleep"})
	if err != nil {
		return "", err
	}
	m, _ := metricByName("sleep")
	s := daily{m: m, values: map[string]int{}, goals: goals, noun: "night"}
	for _, r := range sleep {
		s.values[dateKey(r.LocalDate)] = int(r.Minutes)
	}
	lines = append(lines, a.weeklyLine(s, sp))

	r, err := loadWorkouts(ctx, q, user.ID, sp.first, day)
	if err != nil {
		return "", err
	}
	lines = append(lines, weeklyWorkouts(r, sp, day))
	l, err := loadSupplements(ctx, q, a.timed(user))
	if err != nil {
		return "", err
	}
	logs, err := q.SupplementLogsBetween(ctx, store.SupplementLogsBetweenParams{UserID: user.ID, First: sp.first, Last: sp.last})
	if err != nil {
		return "", err
	}
	lines = append(lines, weeklySupplements(l, sp, day, logs))
	w, err := loadWeightTrend(ctx, q, user.ID, sp.last)
	if err != nil {
		return "", err
	}
	lines = append(lines, weeklyWeight(w))

	var shown []string
	for _, line := range lines {
		if line != "" {
			shown = append(shown, line)
		}
	}
	if len(shown) == 0 {
		return "", nil
	}
	return "Last week (" + sp.dates() + "):\n" + strings.Join(shown, "\n"), nil
}

// weeklyLine is a daily metric over the week, like "Calories: 1,790 cal
// average over 6 days logged, on goal 5 of 7 days." A day with nothing
// logged isn't on goal. It is empty for a metric with no goal that week and
// nothing logged, and says nothing logged for one with a goal.
func (a *Agent) weeklyLine(s daily, sp span) string {
	t := a.tallyDays(s, sp)
	goalDays := 0
	for d := sp.first; !d.Time.After(sp.last.Time); d = addDays(d, 1) {
		if goalOn(s.goals, d).Value.Valid {
			goalDays++
		}
	}
	if t.logged == 0 && goalDays == 0 {
		return ""
	}
	out := upperFirst(s.m.label) + ": "
	if t.logged == 0 {
		return out + "nothing logged."
	}
	out += s.m.format(t.avg()) + " average"
	if t.logged < sp.days() {
		out += " over " + plural(t.logged, s.noun) + " logged"
	}
	if goalDays > 0 {
		out += fmt.Sprintf(", on goal %d of %s", t.onGoal, plural(goalDays, s.noun))
	}
	return out + "."
}

// weeklyWorkouts is the week's planned sessions done, like "Workouts: 3 of
// 4 planned sessions done, 1 missed (legs 1 of 2, push 1 of 1, pull 1 of
// 1).", or empty with nothing planned or done.
func weeklyWorkouts(r *workoutRange, sp span, today pgtype.Date) string {
	all, byName, names := r.count(sp.first, sp.last, today, "")
	if all.planned == 0 && all.extras == 0 {
		return ""
	}
	out := "Workouts: " + all.String()
	if len(names) > 1 {
		var parts []string
		for _, n := range names {
			c := byName[n]
			s := n
			if c.planned > 0 {
				s += fmt.Sprintf(" %d of %d", c.states[sessionDone], c.planned)
				if c.extras > 0 {
					s += fmt.Sprintf(" + %d extra", c.extras)
				}
			} else {
				s += fmt.Sprintf(" %d extra", c.extras)
			}
			parts = append(parts, s)
		}
		out += " (" + strings.Join(parts, ", ") + ")"
	}
	return out + "."
}

// weeklySupplements is each supplement's due doses taken that week, like
// "Supplements taken: creatine 7 of 7; fish oil 5 of 7, 1 skipped, 1
// missed.", or empty with none due.
func weeklySupplements(l *supplementList, sp span, today pgtype.Date, rows []store.SupplementLog) string {
	logs := map[string][]store.SupplementLog{}
	for _, r := range rows {
		logs[dateKey(r.LocalDate)] = append(logs[dateKey(r.LocalDate)], r)
	}
	var parts []string
	for _, n := range l.names {
		c := l.count(sp.first, sp.last, today, logs, n)
		if c.due == 0 {
			continue
		}
		s := fmt.Sprintf("%s %d of %d", n, c.states["taken"], c.due)
		for _, st := range []string{sessionSkipped, sessionMissed} {
			if k := c.states[st]; k > 0 {
				s += fmt.Sprintf(", %d %s", k, st)
			}
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return ""
	}
	return "Supplements taken: " + strings.Join(parts, "; ") + "."
}

// weeklyWeight is the 7-day average weight at the end of the week against
// the week before and the target, like "Weight: 181.2 lbs 7-day average,
// down 0.8 lbs from the week before, 6.2 lbs above the 175 lbs target.",
// or empty with no weigh-ins that week.
func weeklyWeight(t weightTrend) string {
	if t.days == 0 {
		return ""
	}
	out := fmt.Sprintf("Weight: %s %d-day average", lbs(t.avg), weightWeek)
	if t.prevDays > 0 {
		switch d := t.avg - t.prev; {
		case d < 0:
			out += ", down " + lbs(-d) + " from the week before"
		case d > 0:
			out += ", up " + lbs(d) + " from the week before"
		default:
			out += ", the same as the week before"
		}
	}
	if t.hasTarget {
		switch d := t.avg - t.target; {
		case d > 0:
			out += ", " + lbs(d) + " above the " + lbs(t.target) + " target"
		case d < 0:
			out += ", " + lbs(-d) + " below the " + lbs(t.target) + " target"
		default:
			out += ", on the " + lbs(t.target) + " target"
		}
	}
	return out + "."
}
