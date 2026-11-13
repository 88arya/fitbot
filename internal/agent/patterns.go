package agent

import (
	"context"
	"fmt"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5/pgtype"
)

// Patterns are what code finds across the last few weeks of logs, shown at
// the end of the weekly summary. Each is a plain count or average from the
// same rows as get_stats, shown only with enough days behind it and a gap
// big enough to matter. They describe, never explain: the LLM isn't
// involved.

const (
	// patternWeeks is how many weeks, ending with the summary's week, the
	// patterns look at.
	patternWeeks = 4
	// weekendGap is the least difference between the average calories of
	// weekend days and weekdays to show, and weekendDays and weekdayDays
	// how many logged days each needs.
	weekendGap  = 250
	weekendDays = 4
	weekdayDays = 10
	// sleepNights is how many days with planned sessions each kind of night
	// before needs, and sleepGap how many percentage points fewer sessions
	// done after short nights to show.
	sleepNights = 3
	sleepGap    = 25
)

// weeklyPatterns are the patterns over the patternWeeks weeks ending on
// sp.last, each a line like "Pattern (last 4 weeks): ...". Paused days
// count for neither side. today is the summary's day, after sp.
func (a *Agent) weeklyPatterns(ctx context.Context, q *store.Queries, userID int64, sp span, today pgtype.Date) ([]string, error) {
	w := span{addDays(sp.last, -7*patternWeeks+1), sp.last, ""}
	paused, err := loadPauses(ctx, q, userID, w.first, w.last)
	if err != nil {
		return nil, err
	}
	cal, err := q.IntakeDaily(ctx, store.IntakeDailyParams{UserID: userID, Metric: "calories", First: w.first, Last: w.last})
	if err != nil {
		return nil, err
	}
	calories := map[string]int{}
	for _, r := range cal {
		calories[dateKey(r.LocalDate)] = int(r.Total)
	}
	sleep, err := q.SleepBetween(ctx, store.SleepBetweenParams{UserID: userID, First: w.first, Last: w.last})
	if err != nil {
		return nil, err
	}
	nights := map[string]int{}
	for _, r := range sleep {
		nights[dateKey(r.LocalDate)] = int(r.Minutes)
	}
	goals, err := q.GoalHistory(ctx, store.GoalHistoryParams{UserID: userID, Metric: "sleep"})
	if err != nil {
		return nil, err
	}
	r, err := loadWorkouts(ctx, q, userID, w.first, w.last)
	if err != nil {
		return nil, err
	}
	var out []string
	if s := weekendPattern(w, calories, paused); s != "" {
		out = append(out, s)
	}
	if s := sleepPattern(w, today, nights, goals, r, paused); s != "" {
		out = append(out, s)
	}
	return out, nil
}

func patternLine(s string) string {
	return fmt.Sprintf("Pattern (last %d weeks): %s.", patternWeeks, s)
}

// weekendPattern compares the average calories logged on Saturdays and
// Sundays with the other days, like "weekend calories averaged 2,310 cal,
// 450 cal more than weekdays (1,860 cal)". The gap is between the rounded
// averages, so the numbers shown add up.
func weekendPattern(w span, calories map[string]int, paused pauseSet) string {
	var end, week struct{ days, total int }
	for d := w.first; !d.Time.After(w.last.Time); d = addDays(d, 1) {
		v, ok := calories[dateKey(d)]
		if _, p := paused.on(d); !ok || p {
			continue
		}
		side := &week
		if wd := d.Time.Weekday(); wd == 0 || wd == 6 {
			side = &end
		}
		side.days++
		side.total += v
	}
	if end.days < weekendDays || week.days < weekdayDays {
		return ""
	}
	endAvg := tally{logged: end.days, total: end.total}.avg()
	weekAvg := tally{logged: week.days, total: week.total}.avg()
	gap, more := endAvg-weekAvg, "more"
	if gap < 0 {
		gap, more = -gap, "less"
	}
	if gap < weekendGap {
		return ""
	}
	cal, _ := metricByName("calories")
	return patternLine(fmt.Sprintf("weekend calories averaged %s, %s %s than weekdays (%s)",
		cal.format(endAvg), cal.format(gap), more, cal.format(weekAvg)))
}

// sleepPattern compares the planned sessions done on days after a night on
// the sleep goal with days after a shorter night, like "after nights on
// your sleep goal you did 9 of 10 planned sessions, after shorter nights 2
// of 5". It is shown only when fewer get done after short nights. A day
// counts with sessions planned, its night logged, and a sleep goal in
// force.
func sleepPattern(w span, today pgtype.Date, nights map[string]int, goals []store.Goal, r *workoutRange, paused pauseSet) string {
	type side struct{ days, planned, done int }
	var good, short side
	for d := w.first; !d.Time.After(w.last.Time); d = addDays(d, 1) {
		mins, ok := nights[dateKey(d)]
		g := goalOn(goals, d)
		if _, p := paused.on(d); !ok || p || !g.Value.Valid {
			continue
		}
		planned, _ := r.day(d).outcomes(today)
		if len(planned) == 0 {
			continue
		}
		s := &short
		if mins >= int(g.Value.Int32) {
			s = &good
		}
		s.days++
		for _, o := range planned {
			s.planned++
			if o.state == sessionDone {
				s.done++
			}
		}
	}
	if good.days < sleepNights || short.days < sleepNights {
		return ""
	}
	// The gap in percentage points, without rounding: good.done/good.planned
	// less short.done/short.planned, against sleepGap/100.
	if (good.done*short.planned-short.done*good.planned)*100 < sleepGap*good.planned*short.planned {
		return ""
	}
	return patternLine(fmt.Sprintf("after nights on your sleep goal you did %d of %s, after shorter nights %d of %d",
		good.done, plural(good.planned, "planned session"), short.done, short.planned))
}
