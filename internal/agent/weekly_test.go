package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/agent"
	"github.com/88arya/fitbot/internal/sim"
)

// wantWeekly is the weekly summary sent on monday, worked out from
// hand-run SQL over the week before.
func (q questions) wantWeekly(monday time.Time, metrics []dailyQ) string {
	q.t.Helper()
	first, last := monday.AddDate(0, 0, -7), monday.AddDate(0, 0, -1)
	lines := []string{"Last week (" + short(first) + " to " + short(last) + "):"}
	for _, d := range metrics {
		n := q.daily(d, first, last)
		var goalDays int
		if err := q.s.Pool.QueryRow(q.s.Ctx, d.judged()+` SELECT count(*) FILTER (WHERE goal IS NOT NULL) FROM j`,
			q.s.UserID, first.Format(time.DateOnly), last.Format(time.DateOnly), d.metric, d.atLeast).Scan(&goalDays); err != nil {
			q.t.Fatal(err)
		}
		if n.logged == 0 && goalDays == 0 {
			continue
		}
		line := strings.ToUpper(d.label[:1]) + d.label[1:] + ": "
		if n.logged == 0 {
			lines = append(lines, line+"nothing logged.")
			continue
		}
		line += agent.FormatAmount(d.metric, n.avg) + " average"
		if n.logged < 7 {
			line += " over " + nounPlural(n.logged, d.noun) + " logged"
		}
		if goalDays > 0 {
			line += fmt.Sprintf(", on goal %d of %s", n.onGoal, nounPlural(goalDays, d.noun))
		}
		lines = append(lines, line+".")
	}

	if all, each := q.sessions(first, last, ""); all.planned > 0 || all.extras > 0 {
		line := "Workouts: " + all.String()
		if len(each) > 1 {
			var parts []string
			for _, c := range each {
				switch {
				case c.planned == 0:
					parts = append(parts, fmt.Sprintf("%s %d extra", c.name, c.extras))
				case c.extras > 0:
					parts = append(parts, fmt.Sprintf("%s %d of %d + %d extra", c.name, c.done, c.planned, c.extras))
				default:
					parts = append(parts, fmt.Sprintf("%s %d of %d", c.name, c.done, c.planned))
				}
			}
			line += " (" + strings.Join(parts, ", ") + ")"
		}
		lines = append(lines, line+".")
	}

	var supps []string
	for _, n := range q.s.Rows(`SELECT name FROM supplements WHERE user_id = $1 ORDER BY name`, q.s.UserID) {
		var due, taken, skipped, missed int
		if err := q.s.Pool.QueryRow(q.s.Ctx, doses+`
			SELECT count(*), count(*) FILTER (WHERE state = 'taken'), count(*) FILTER (WHERE state = 'skipped'),
				count(*) FILTER (WHERE state = 'missed')
			FROM doses WHERE name = $5`, q.s.UserID, first.Format(time.DateOnly), last.Format(time.DateOnly),
			q.today.Format(time.DateOnly), n).Scan(&due, &taken, &skipped, &missed); err != nil {
			q.t.Fatal(err)
		}
		if due == 0 {
			continue
		}
		s := fmt.Sprintf("%s %d of %d", n, taken, due)
		if skipped > 0 {
			s += fmt.Sprintf(", %d skipped", skipped)
		}
		if missed > 0 {
			s += fmt.Sprintf(", %d missed", missed)
		}
		supps = append(supps, s)
	}
	if len(supps) > 0 {
		lines = append(lines, "Supplements taken: "+strings.Join(supps, "; ")+".")
	}

	if w := q.weight(last.AddDate(0, 0, -6), last); w.n > 0 {
		line := "Weight: " + showLbs(w.avg) + " 7-day average"
		if p := q.weight(last.AddDate(0, 0, -13), last.AddDate(0, 0, -7)); p.n > 0 {
			switch d := w.avg - p.avg; {
			case d < 0:
				line += ", down " + showLbs(-d) + " from the week before"
			case d > 0:
				line += ", up " + showLbs(d) + " from the week before"
			default:
				line += ", the same as the week before"
			}
		}
		switch d := w.avg - 1750; {
		case d > 0:
			line += ", " + showLbs(d) + " above the " + showLbs(1750) + " target"
		case d < 0:
			line += ", " + showLbs(-d) + " below the " + showLbs(1750) + " target"
		default:
			line += ", on the " + showLbs(1750) + " target"
		}
		lines = append(lines, line+".")
	}
	return strings.Join(lines, "\n")
}

// TestWeeklySummary checks the summary sent on each Monday of the seeded
// month against hand-run SQL. The weeks cover a calorie goal and a sleep
// goal that change midweek, days with nothing logged, skipped and missed
// sessions, an extra session, and skipped and missed doses.
func TestWeeklySummary(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s, _ := seedMonth(t)
	src := func(metric string) string {
		return `SELECT local_date, sum(amount)::int AS v FROM intake_logs WHERE user_id = $1 AND metric = '` + metric +
			`' AND deleted_at IS NULL GROUP BY local_date`
	}
	metrics := []dailyQ{
		{label: "calories", metric: "calories", src: src("calories"), noun: "day"},
		{label: "protein", metric: "protein", src: src("protein"), noun: "day", atLeast: true},
		{label: "carbs", metric: "carbs", src: src("carbs"), noun: "day"},
		{label: "fat", metric: "fat", src: src("fat"), noun: "day"},
		{label: "water", metric: "water", src: src("water"), noun: "day", atLeast: true},
		{label: "sleep", metric: "sleep", src: `SELECT local_date, minutes AS v FROM sleep_logs WHERE user_id = $1 AND deleted_at IS NULL`,
			noun: "night", atLeast: true},
	}
	for _, day := range []int{5, 12, 19, 26} {
		monday := time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC)
		s.Set(time.Date(2026, 10, day, 8, 0, 0, 0, loc))
		q := questions{t: t, s: s, today: monday}
		got, err := s.Agent.CheckinText(s.Ctx, s.UserID, monday, "weekly_summary", "")
		if err != nil {
			t.Fatal(err)
		}
		if want := q.wantWeekly(monday, metrics); got != want {
			t.Errorf("Oct %d:\n got %s\nwant %s", day, got, want)
		}
		if !strings.Contains(dueTexts(t, s, monday), "weekly_summary at ") {
			t.Errorf("Oct %d: no weekly summary due", day)
		}
		if tue := monday.AddDate(0, 0, 1); strings.Contains(dueTexts(t, s, tue), "weekly_summary") {
			t.Errorf("Oct %d: weekly summary due on a Tuesday", day+1)
		}
	}

	// The week of Oct 19, spelled out, for how it reads. Sleep rises to 8
	// hours on Oct 20, and there are no carbs to report.
	got, err := s.Agent.CheckinText(s.Ctx, s.UserID, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), "weekly_summary", "")
	want := "Last week (Oct 19 to Oct 25):\n" +
		"Calories: 2,203 cal average over 6 days logged, on goal 2 of 7 days.\n" +
		"Protein: 131 g average over 5 days logged, on goal 1 of 7 days.\n" +
		"Fat: 71 g average, on goal 2 of 7 days.\n" +
		"Water: 2.53 L average over 6 days logged, on goal 3 of 7 days.\n" +
		"Sleep: 7 hours 12 min average over 6 nights logged, on goal 2 of 7 nights.\n" +
		"Workouts: 6 of 8 planned sessions done, 2 skipped, plus 1 extra session " +
		"(push 2 of 2, pull 1 of 2, cardio 1 of 2 + 1 extra, legs 2 of 2).\n" +
		"Supplements taken: creatine 6 of 7, 1 skipped; fish oil 1 of 3, 2 missed.\n" +
		"Weight: 180.8 lbs 7-day average, down 1.2 lbs from the week before, 5.8 lbs above the 175 lbs target."
	if err != nil || got != want {
		t.Errorf("Oct 26 = %q, %v\nwant %q", got, err, want)
	}
}

// TestWeeklySummaryEmpty checks that a user with no goals, schedule,
// supplements, or logs gets no weekly summary.
func TestWeeklySummaryEmpty(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 11, 2, 7, 0, 0, 0, loc))
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE goals SET value = NULL`); err != nil {
		t.Fatal(err)
	}
	got, err := s.Agent.CheckinText(s.Ctx, s.UserID, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), "weekly_summary", "")
	if err != nil || got != "" {
		t.Errorf("summary = %q, %v; want none", got, err)
	}
}
