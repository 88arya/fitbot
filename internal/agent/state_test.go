package agent_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// wantState is the trend as of end for a user in state, from hand-run SQL,
// for the prompt and for a text.
func (q questions) wantState(state string, end time.Time) (forModel, forUser string) {
	q.t.Helper()
	forModel = "target state: " + state + "; actual state: "
	forUser = "State: " + state + ", "
	week, before := q.weight(end.AddDate(0, 0, -6), end), q.weight(end.AddDate(0, 0, -13), end.AddDate(0, 0, -7))
	if week.n == 0 || before.n == 0 {
		return forModel + "not enough weigh-ins, it needs one in each of the last two 7-day stretches", forUser + "not enough weigh-ins."
	}
	change := week.avg - before.avg
	actual, movement := "maintain", "holding steady"
	switch {
	case change > 5:
		actual, movement = "bulk", "gaining "+showLbs(change)+" a week"
	case change < -5:
		actual, movement = "cut", "losing "+showLbs(-change)+" a week"
	}
	forModel += actual + ", " + movement + "; maintenance "
	forUser += "actually " + movement + "."

	last := end
	if yesterday := q.today.AddDate(0, 0, -1); last.After(yesterday) {
		last = yesterday
	}
	var days, total int
	if err := q.s.Pool.QueryRow(q.s.Ctx, `SELECT count(DISTINCT local_date), coalesce(sum(amount), 0)::int FROM intake_logs
		WHERE user_id = $1 AND metric = 'calories' AND deleted_at IS NULL AND local_date BETWEEN $2::date AND $3::date`,
		q.s.UserID, last.AddDate(0, 0, -13).Format(time.DateOnly), last.Format(time.DateOnly)).Scan(&days, &total); err != nil {
		q.t.Fatal(err)
	}
	if days < 10 {
		return forModel + "unknown, it needs calories logged on 10 of the last 14 days", forUser
	}
	// 3,500 cal a pound, over the 7 days between the two averages.
	m := fmtCalories(int(math.Round(float64(total)/float64(days) - float64(change)*3500/70)))
	return forModel + "about " + m, forUser + " Maintenance is about " + m + "."
}

func fmtCalories(n int) string {
	return fmt.Sprintf("%d,%03d cal", n/1000, n%1000)
}

func intakeCall(cal int, date string) string {
	return fmt.Sprintf(`{"calories":%d,"protein":null,"carbs":null,"fat":null,"water":null,"water_unit":null,"date":%s}`, cal, date)
}

// TestStateJudging changes state during a week and checks each day is judged
// by the state in force then, in progress, get_day, get_stats, and the
// weekly summary, and that a change can be refused and undone.
func TestStateJudging(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, loc) }
	s := sim.New(t, day(5))
	one := func(body, tool, args, want string) {
		t.Helper()
		if got := logged(t, s, body, tool, args); len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("%s: results %q, want %q", body, got, want)
		}
	}
	one("2000 cal", "set_goals", `{"goals":[{"metric":"calories","value":2000,"unit":null}]}`, "saved")

	// Bulk: over is a hit.
	one("ate 2100", "log_intake", intakeCall(2100, "null"), "2,100 / 2,000 cal today, goal hit")
	s.Set(day(6))
	one("ate 1900", "log_intake", intakeCall(1900, "null"), "1,900 / 2,000 cal today, 100 cal to go")

	// Cut: room left today, and over is a miss.
	s.Set(day(7))
	one("cutting now", "set_state", `{"state":"cut"}`, "saved, starting today: state cut. Calories are on goal at or under the goal.")
	one("again", "set_state", `{"state":"cut"}`, "the state is already cut")
	one("shredding", "set_state", `{"state":"shred"}`, `error: state "shred" must be one of bulk, maintain, cut`)
	one("ate 1900", "log_intake", intakeCall(1900, "null"), "1,900 / 2,000 cal today, 100 cal left")
	s.Set(day(8))
	one("ate 2100", "log_intake", intakeCall(2100, "null"), "2,100 / 2,000 cal today, 100 cal over, goal missed")
	// A day logged late is judged by its own state, not today's.
	one("yesterday too", "log_intake", intakeCall(1, `"2026-10-05"`), "2,101 / 2,000 cal on Oct 5, goal hit")
	if got := s.Rows(`SELECT state, start_date::text FROM body_states ORDER BY start_date`); fmt.Sprint(got) !=
		"[bulk | 2026-10-05 cut | 2026-10-07]" {
		t.Errorf("states = %v", got)
	}
	one("fix", "delete_log", `{"type":"calories","id":null}`, "deleted")

	// Maintain counts at or under, like a cut.
	s.Set(day(9))
	one("maintaining", "set_state", `{"state":"maintain"}`, "state maintain. Calories are on goal at or under the goal.")
	one("ate 2000", "log_intake", intakeCall(2000, "null"), "2,000 / 2,000 cal today, goal hit")

	s.Set(day(10))
	for _, c := range []struct{ date, want string }{
		{"2026-10-05", "2,100 / 2,000 cal on Oct 5, goal hit"},
		{"2026-10-06", "1,900 / 2,000 cal on Oct 6, 100 cal short, goal missed"},
		{"2026-10-07", "1,900 / 2,000 cal on Oct 7, goal hit"},
		{"2026-10-08", "2,100 / 2,000 cal on Oct 8, 100 cal over, goal missed"},
		{"2026-10-09", "2,000 / 2,000 cal yesterday, goal hit"},
	} {
		one("how was "+c.date, "get_day", `{"date":"`+c.date+`"}`, c.want)
	}
	stats := func(first, last string) string {
		return `{"metric":"calories","name":null,"period":"dates","days":null,"first":"` + first + `","last":"` + last + `"}`
	}
	one("that week", "get_stats", stats("2026-10-05", "2026-10-09"),
		"on goal (each day's goal for its state) on 3 of the 5 days logged, best streak 1 day")
	one("the cut days", "get_stats", stats("2026-10-07", "2026-10-08"), "on goal (at most 2,000 cal) on 1 of the 2 days logged")
	one("the bulk days", "get_stats", stats("2026-10-05", "2026-10-06"), "on goal (at least 2,000 cal) on 1 of the 2 days logged")

	got, err := s.Agent.CheckinText(s.Ctx, s.UserID, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), "weekly_summary", "")
	if want := "Calories: 2,000 cal average over 5 days logged, on goal 3 of 7 days."; err != nil || !strings.Contains(got, want) {
		t.Errorf("summary = %q, %v; want %q", got, err, want)
	}

	// Two changes on one day: the second replaces the first, and undo puts
	// the first back, then removes it.
	one("bulking", "set_state", `{"state":"bulk"}`, "state bulk. Calories are on goal at or above the goal.")
	one("no, cutting", "set_state", `{"state":"cut"}`, "saved")
	if got := s.Text("undo"); got != "Undid setting the state to cut. State: bulk." {
		t.Errorf("undo = %q", got)
	}
	if got := s.Text("undo"); got != "Undid setting the state to bulk. State: maintain." {
		t.Errorf("undo = %q", got)
	}
	if got := s.Rows(`SELECT state, start_date::text FROM body_states ORDER BY start_date`); fmt.Sprint(got) !=
		"[bulk | 2026-10-05 cut | 2026-10-07 maintain | 2026-10-09]" {
		t.Errorf("states after undo = %v", got)
	}
}

// TestStateImpossibleMaintenance checks that a gain too big for the calories
// logged shows no maintenance instead of a negative one.
func TestStateImpossibleMaintenance(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 19, 12, 0, 0, 0, loc))
	logged(t, s, "weighed", "log_weight", `{"weight":180,"unit":null,"date":"2026-10-08"}`)
	logged(t, s, "weighed", "log_weight", `{"weight":190,"unit":null,"date":"2026-10-15"}`)
	for d := 6; d <= 15; d++ {
		logged(t, s, "ate", "log_intake", intakeCall(2000, fmt.Sprintf(`"2026-10-%02d"`, d)))
	}
	got, err := s.Agent.CheckinText(s.Ctx, s.UserID, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), "weekly_summary", "")
	if want := "State: bulk, actually gaining 10 lbs a week."; err != nil || !strings.HasSuffix(got, want) {
		t.Errorf("summary = %q, %v; want it to end %q", got, err, want)
	}
	stats := `{"metric":"weight","name":null,"period":"last_days","days":7,"first":null,"last":null}`
	want := "actual state: bulk, gaining 10 lbs a week; maintenance unknown"
	if got := logged(t, s, "on track?", "get_stats", stats); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("get_stats = %q, want %q", got, want)
	}
}

// TestStateTrend checks the actual state and maintenance from a few weeks of
// logs, in the prompt, get_stats, and the weekly summary, with too few
// weigh-ins and calories, and with the setting changed.
func TestStateTrend(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 19, 12, 0, 0, 0, loc))
	today := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	q := questions{t: t, s: s, today: today}
	monday := today

	weigh := func(lbs float64, date string) {
		t.Helper()
		logged(t, s, "weighed", "log_weight", fmt.Sprintf(`{"weight":%g,"unit":null,"date":"%s"}`, lbs, date))
	}
	weigh(180, "2026-10-08")
	weigh(180.7, "2026-10-15")
	// Oct 6 to 17, and today's meal doesn't count.
	for d := 6; d <= 17; d++ {
		logged(t, s, "ate", "log_intake", intakeCall(2500, fmt.Sprintf(`"2026-10-%02d"`, d)))
	}
	logged(t, s, "ate", "log_intake", intakeCall(9000, "null"))

	check := func(when, model, user string) {
		t.Helper()
		wantModel, wantUser := q.wantState("bulk", today)
		if model != "" && wantModel != "target state: bulk; actual state: "+model {
			t.Errorf("%s: hand-run model text = %q", when, wantModel)
		}
		s.Script(sim.Say("ok"))
		s.Text("hi")
		reqs := s.Requests()
		sys := reqs[len(reqs)-1][0].Content
		if !strings.Contains(sys, "\n## Body state\n"+wantModel+". Calories are on goal at or above the goal.\n") {
			t.Errorf("%s: prompt lacks %q:\n%s", when, wantModel, sys)
		}
		stats := `{"metric":"weight","name":null,"period":"last_days","days":7,"first":null,"last":null}`
		if got := logged(t, s, "on track?", "get_stats", stats); len(got) != 1 || !strings.HasSuffix(got[0], "; "+wantModel+".") {
			t.Errorf("%s: get_stats = %q, want it to end %q", when, got, wantModel)
		}
		_, wantWeek := questions{t: t, s: s, today: monday}.wantState("bulk", monday.AddDate(0, 0, -1))
		if wantWeek != "State: bulk, "+user {
			t.Errorf("%s: hand-run text = %q, wantUser %q", when, wantWeek, wantUser)
		}
		got, err := s.Agent.CheckinText(s.Ctx, s.UserID, monday, "weekly_summary", "")
		if err != nil || !strings.HasSuffix(got, "\n"+wantWeek) {
			t.Errorf("%s: summary = %q, %v; want it to end %q", when, got, err, wantWeek)
		}
	}
	// 2,500 cal average, less 0.7 lbs a week at 500 cal a day per pound.
	check("gaining", "bulk, gaining 0.7 lbs a week; maintenance about 2,150 cal",
		"actually gaining 0.7 lbs a week. Maintenance is about 2,150 cal.")

	// A change at the limit counts as holding, and the limit is a setting.
	if got := logged(t, s, "bad", "update_setting", `{"key":"state_weekly_change","value":"0"}`); len(got) != 1 ||
		!strings.Contains(got[0], "error: state_weekly_change") {
		t.Errorf("setting 0 = %q", got)
	}
	if got := logged(t, s, "0.7 lbs", "update_setting", `{"key":"state_weekly_change","value":"0.7"}`); len(got) != 1 ||
		got[0] != "saved: state weekly change is now 0.7" {
		t.Errorf("setting = %q", got)
	}
	s.Script(sim.Say("ok"))
	s.Text("hi")
	reqs := s.Requests()
	if sys := reqs[len(reqs)-1][0].Content; !strings.Contains(sys, "actual state: maintain, holding steady; maintenance about 2,150 cal") {
		t.Errorf("0.7 lbs at a 0.7 limit should hold:\n%s", sys)
	}
	logged(t, s, "back", "update_setting", `{"key":"state_weekly_change","value":"default"}`)

	// 9 days of calories isn't enough.
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE intake_logs SET deleted_at = now() WHERE local_date IN ('2026-10-06', '2026-10-07', '2026-10-08')`); err != nil {
		t.Fatal(err)
	}
	check("few calories", "bulk, gaining 0.7 lbs a week; maintenance unknown, it needs calories logged on 10 of the last 14 days",
		"actually gaining 0.7 lbs a week.")

	// No weigh-in in the week before.
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE weight_logs SET deleted_at = now() WHERE local_date = '2026-10-08'`); err != nil {
		t.Fatal(err)
	}
	check("few weigh-ins", "not enough weigh-ins, it needs one in each of the last two 7-day stretches", "not enough weigh-ins.")
}
