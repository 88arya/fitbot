package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

func stateCall(state, tier string) string {
	return fmt.Sprintf(`{"state":%q,"tier":%s,"daily_calories":null,"weekly_rate":null,"rate_unit":null}`, state, tier)
}

const stats175 = `{"height_cm":178,"age":30,"sex":"male","activity":"moderate"}`

func one(t *testing.T, s *sim.Sim, body, tool, args, want string) {
	t.Helper()
	if got := logged(t, s, body, tool, args); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("%s: results %q, want %q", body, got, want)
	}
}

func goalRows(s *sim.Sim, day string) string {
	return fmt.Sprint(s.Rows(`SELECT metric, value FROM (SELECT DISTINCT ON (metric) metric, value FROM goals WHERE start_date <= $1::date
		ORDER BY metric, start_date DESC) g WHERE value IS NOT NULL ORDER BY metric`, day))
}

// planned sets the 175 lbs user up on a plan.
func planned(t *testing.T, s *sim.Sim, tier string) {
	t.Helper()
	logged(t, s, "weighed", "log_weight", `{"weight":175,"unit":null,"date":null}`)
	logged(t, s, "stats", "set_profile", stats175)
	logged(t, s, "cut", "set_state", stateCall("cut", tier))
}

// TestPlanTargets takes targets through every way they change: stats arriving
// in any order, each kind of rate, a state change, hand-set goals, a stat
// that makes the rate unsafe, and undo. Logs stay through all of it.
func TestPlanTargets(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, loc) }
	s := sim.New(t, day(5))

	// The state comes first; the targets wait for the last stat.
	one(t, s, "cut", "set_state", stateCall("cut", `"moderate"`), "saved, starting today: state moderate cut.")
	one(t, s, "weighed", "log_weight", `{"weight":175,"unit":null,"date":null}`, "logged")
	one(t, s, "height", "set_profile", `{"height_ft":5,"height_in":10}`, "saved")
	one(t, s, "age", "set_profile", `{"age":30}`, "saved")
	one(t, s, "sex", "set_profile", `{"sex":"male"}`, "saved")
	if got := goalRows(s, "2026-10-05"); got != "[]" {
		t.Errorf("targets before the last stat = %s", got)
	}
	// BMR 793.8 + 1,111.3 - 150 + 5 = 1,760, times 1.55 is 2,728, so 2,730.
	want := "Targets: 2,230 cal, 175 g protein, 245 g carbs, 61 g fat. Worked out from a BMR of 1,760 cal, times 1.55 for moderate " +
		"activity, for 2,730 cal maintenance, less 500 for your moderate cut: losing about 1 lbs a week, 4.3 lbs a month."
	one(t, s, "moderate", "set_profile", `{"activity":"moderate"}`, want)
	if got := goalRows(s, "2026-10-05"); got != "[calories | 2230 carbs | 245 fat | 61 protein | 175]" {
		t.Errorf("goals = %s", got)
	}

	// Trackers use the targets, and carbs are tracked without being asked.
	one(t, s, "ate", "log_intake", intake("500", "carbs", "40"), "500 / 2,230 cal today, 1,730 cal left")
	one(t, s, "plan?", "get_plan", `{}`, "Plan: moderate cut since Oct 5, -500 cal a day from maintenance")

	// An unsafe rate is refused with what to do instead, and nothing changes.
	one(t, s, "extreme", "set_state", stateCall("cut", `"extreme"`),
		"error: a 1,000 cal a day deficit isn't safe for you: it would take more than 30% of your 2,730 maintenance. The most Fitbot will set is 810 cal a day")
	one(t, s, "3 lbs a week", "set_state", `{"state":"cut","tier":null,"daily_calories":null,"weekly_rate":3,"rate_unit":null}`,
		"error: 1,500 cal a day is more than the 1,000 cal a day (7,000 a week)")
	one(t, s, "wrong tier", "set_state", stateCall("cut", `"lean"`), "error:")

	// A custom pace, and the same pace in kg.
	s.Set(day(6))
	one(t, s, "1.5 lbs", "set_state", `{"state":"cut","tier":null,"daily_calories":null,"weekly_rate":1.5,"rate_unit":null}`,
		"less 750 for your custom cut: losing about 1.5 lbs a week")
	one(t, s, "same", "set_state", stateCall("cut", "null"), "the state is already custom cut")
	one(t, s, "0.5 kg", "set_state", `{"state":"cut","tier":null,"daily_calories":null,"weekly_rate":0.5,"rate_unit":"kg"}`, "less 551 for your custom cut")

	// Changing state recalculates everything, and past days keep their goals.
	s.Set(day(7))
	one(t, s, "lean bulk", "set_state", stateCall("bulk", `"lean"`), "Targets: 2,830 cal, 140 g protein")
	if got := goalRows(s, "2026-10-05"); got != "[calories | 2230 carbs | 245 fat | 61 protein | 175]" {
		t.Errorf("Oct 5's goals = %s", got)
	}
	if got := s.Rows(`SELECT count(*) FROM intake_logs WHERE deleted_at IS NULL`); fmt.Sprint(got) != "[2]" {
		t.Errorf("logs after changing state = %v", got)
	}
	if got := s.Rows(`SELECT state, tier, calorie_delta, start_date::text FROM body_states WHERE tier <> 'manual' ORDER BY start_date`); fmt.Sprint(got) !=
		"[cut | moderate | -500 | 2026-10-05 cut | custom | -551 | 2026-10-06 bulk | lean | 100 | 2026-10-07]" {
		t.Errorf("states = %v", got)
	}

	// undo puts the state and the targets back, and removes the history row.
	if got := s.Text("undo"); got != "Undid setting the state to lean bulk. State: custom cut." {
		t.Errorf("undo = %q", got)
	}
	if got := goalRows(s, "2026-10-07"); !strings.Contains(got, "calories | 2179") {
		t.Errorf("goals after undo = %s", got)
	}

	// A calorie goal the user names sets the rate, and an amount they set
	// stays when targets are worked out again.
	s.Set(day(8))
	one(t, s, "eat 2,000", "set_goals", `{"goals":[{"metric":"calories","value":2000,"unit":null}]}`, "less 730 for your custom cut")
	one(t, s, "protein", "set_goals", `{"goals":[{"metric":"protein","value":190,"unit":"g"},{"metric":"water","value":3,"unit":"l"}]}`,
		"Targets: 2,000 cal, 190 g protein")
	one(t, s, "bulk", "set_state", stateCall("bulk", `"standard"`), "Targets: 2,980 cal, 190 g protein")
	if got := s.Text("undo"); !strings.HasPrefix(got, "Undid setting the state to standard bulk.") {
		t.Errorf("undo = %q", got)
	}
	one(t, s, "too much", "set_goals", `{"goals":[{"metric":"protein","value":900,"unit":"g"}]}`, "error: 900 g protein and 61 g fat leave no calories")
	one(t, s, "over", "set_goals", `{"goals":[{"metric":"calories","value":2900,"unit":null}]}`, "error: 2,900 cal isn't a deficit")
	one(t, s, "unsafe", "set_goals", `{"goals":[{"metric":"calories","value":1800,"unit":null}]}`, "error: a 930 cal a day deficit isn't safe")
	one(t, s, "off", "set_goals", `{"goals":[{"metric":"calories","value":null,"unit":null}]}`, "error: calories drive the plan")
	// Carbs are what's left, and one the user stops stays stopped.
	one(t, s, "no carbs", "set_goals", `{"goals":[{"metric":"carbs","value":null,"unit":null}]}`, "saved")
	one(t, s, "bulk", "set_state", stateCall("bulk", `"lean"`), "Targets: 2,830 cal")
	if got := goalRows(s, "2026-10-08"); strings.Contains(got, "carbs") {
		t.Errorf("carbs should stay off: %s", got)
	}
	if got := s.Text("undo"); !strings.HasPrefix(got, "Undid setting the state to lean bulk.") {
		t.Errorf("undo = %q", got)
	}

	// A lower activity level shrinks maintenance, so the deficit comes down
	// to what's safe, and undo restores both.
	s.Set(day(9))
	one(t, s, "aggressive", "set_state", stateCall("cut", `"aggressive"`), "less 750 for your aggressive cut")
	one(t, s, "desk job", "set_profile", `{"activity":"sedentary"}`, "The deficit came down to 610 cal a day")
	if got := s.Rows(`SELECT tier, calorie_delta FROM body_states ORDER BY start_date DESC LIMIT 1`); fmt.Sprint(got) != "[custom | -610]" {
		t.Errorf("state after the limit = %v", got)
	}
	s.Text("undo")
	if got := s.Rows(`SELECT u.activity, b.tier, b.calorie_delta FROM users u JOIN body_states b ON b.user_id = u.id ORDER BY b.start_date DESC LIMIT 1`); fmt.Sprint(got) !=
		"[moderate | aggressive | -750]" {
		t.Errorf("after undo = %v", got)
	}

	// A hand-set goal undoes in one short line.
	one(t, s, "protein", "set_goals", `{"goals":[{"metric":"protein","value":170,"unit":"g"}]}`, "Targets:")
	if got := s.Text("undo"); !strings.HasPrefix(got, "Undid setting goals (calorie and macro targets).") {
		t.Errorf("undo = %q", got)
	}

	// Going manual hands the goals back and stops the reviews.
	s.Set(day(10))
	one(t, s, "my own", "set_state", stateCall("cut", `"manual"`), "saved, starting today: state cut.")
	one(t, s, "1800", "set_goals", `{"goals":[{"metric":"calories","value":1800,"unit":null}]}`, "saved, starting today: calories: 1,800 cal a day")
	one(t, s, "plan?", "get_plan", `{}`, "The user sets their own goals")
}

// TestPlanSafety checks users a cut isn't safe for at all, and the calorie
// floor.
func TestPlanSafety(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, loc)

	s := sim.New(t, start)
	logged(t, s, "weighed", "log_weight", `{"weight":160,"unit":null,"date":null}`)
	logged(t, s, "stats", "set_profile", `{"height_cm":175,"age":16,"sex":"male","activity":"moderate"}`)
	one(t, s, "cut", "set_state", stateCall("cut", `"light"`), "error: Fitbot doesn't set a calorie deficit for anyone under 18")
	one(t, s, "stats", "set_state", stateCall("bulk", `"lean"`), "Targets:")

	s = sim.New(t, start)
	logged(t, s, "weighed", "log_weight", `{"weight":115,"unit":null,"date":null}`)
	logged(t, s, "stats", "set_profile", `{"height_cm":178,"age":25,"sex":"female","activity":"light"}`)
	one(t, s, "cut", "set_state", stateCall("cut", `"light"`), "error: a cut isn't safe at a BMI of 16.5")

	// A small sedentary user can only cut down to the floor.
	s = sim.New(t, start)
	logged(t, s, "weighed", "log_weight", `{"weight":130,"unit":null,"date":null}`)
	logged(t, s, "stats", "set_profile", `{"height_cm":160,"age":60,"sex":"female","activity":"sedentary"}`)
	one(t, s, "cut", "set_state", stateCall("cut", `"moderate"`), "under 1,200 cal a day, the lowest Fitbot will set. The most Fitbot will set is 160 cal a day")
	one(t, s, "bad age", "set_profile", `{"age":7}`, "error: age must be")
	one(t, s, "bad sex", "set_profile", `{"sex":"x"}`, "error: sex must be")
	one(t, s, "bad height", "set_profile", `{"height_cm":20}`, "error: height must be")
}

// TestManualToPlan checks a user with hand-set goals and no weigh-in is asked
// for everything targets need when they choose a rate.
func TestManualToPlan(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 5, 12, 0, 0, 0, loc))
	one(t, s, "2000", "set_goals", `{"goals":[{"metric":"calories","value":2000,"unit":null}]}`, "saved")
	s.Script(sim.Call("set_state", stateCall("cut", `"moderate"`)), sim.Say("Got it."))
	if got := s.Text("work out a moderate cut for me"); got != "Got it. What do you weigh right now?" {
		t.Errorf("reply = %q", got)
	}
	s.Script(sim.Call("log_weight", `{"weight":175,"unit":null,"date":null}`), sim.Say("Got it."))
	if got := s.Text("175"); got != "Got it. How tall are you?" {
		t.Errorf("reply = %q", got)
	}
	one(t, s, "stats", "set_profile", stats175, "Targets: 2,230 cal")

	// Maintain targets maintenance, and a calorie goal named there moves the
	// estimate, which a later state keeps.
	s.Set(time.Date(2026, 10, 6, 12, 0, 0, 0, loc))
	one(t, s, "maintain", "set_state", stateCall("maintain", "null"), "Targets: 2,730 cal")
	one(t, s, "2,600 holds me", "set_goals", `{"goals":[{"metric":"calories","value":2600,"unit":null}]}`, "less 130 from your weight trend")
	s.Set(time.Date(2026, 10, 7, 12, 0, 0, 0, loc))
	one(t, s, "lean", "set_state", stateCall("bulk", `"lean"`), "Targets: 2,700 cal")
}

// weekOf logs a week's weigh-ins at avg and, unless cal is 0, its calories.
func weekOf(t *testing.T, s *sim.Sim, monday time.Time, avg float64, cal int) {
	t.Helper()
	s.Set(monday.AddDate(0, 0, 6))
	var calls []string
	for _, d := range []int{0, 2, 4} {
		calls = append(calls, "log_weight", fmt.Sprintf(`{"weight":%g,"unit":null,"date":%q}`, avg, monday.AddDate(0, 0, d).Format(time.DateOnly)))
	}
	for d := 0; d < 6 && cal > 0; d++ {
		calls = append(calls, "log_intake", intakeCall(cal, `"`+monday.AddDate(0, 0, d).Format(time.DateOnly)+`"`))
	}
	logged(t, s, "seed", calls...)
}

func reviewOn(t *testing.T, s *sim.Sim, monday time.Time) string {
	t.Helper()
	s.Set(time.Date(monday.Year(), monday.Month(), monday.Day(), 8, 0, 0, 0, monday.Location()))
	got, err := s.Agent.CheckinText(s.Ctx, s.UserID, time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.UTC), "target_review", "")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestWeeklyReviewCut runs the spec's cutting example, then a stall that
// lasts, through the scheduler.
func TestWeeklyReviewCut(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	week1 := time.Date(2026, 9, 7, 12, 0, 0, 0, loc)
	week := func(n int) time.Time { return week1.AddDate(0, 0, 7*(n-1)) }
	s := sim.New(t, week1)
	planned(t, s, `"light"`)
	s.Set(week1.AddDate(0, 0, 1))
	logged(t, s, "faster", "set_state", stateCall("cut", `"moderate"`))

	weekOf(t, s, week(1), 175, 2230)
	weekOf(t, s, week(2), 174.2, 2230)
	if got := reviewOn(t, s, week(3)); got != "" {
		t.Errorf("a first week's review = %q", got)
	}
	weekOf(t, s, week(3), 173.4, 2230)
	if got := reviewOn(t, s, week(4)); got != "" {
		t.Errorf("-0.8 lbs on a 1 lb pace should be on track: %q", got)
	}
	// One slow week is noted, and no target moves.
	weekOf(t, s, week(4), 173.3, 2230)
	want := "Progress check: your average weight changed -0.1 lbs last week, against -1 lbs a week planned. One week can just be water, " +
		"so no change yet. I'll look again next Monday."
	if got := reviewOn(t, s, week(5)); got != want {
		t.Errorf("slow week = %q", got)
	}
	if got := s.Rows(`SELECT count(*) FROM target_changes WHERE reason = 'review'`); fmt.Sprint(got) != "[0]" {
		t.Errorf("changes = %v", got)
	}

	// A second slow week moves calories, through the scheduler, in order.
	weekOf(t, s, week(5), 173.2, 2230)
	monday := time.Date(2026, 10, 12, 8, 0, 0, 0, loc)
	s.Set(monday)
	if got, _ := s.Agent.DueTexts(s.Ctx, s.UserID, monday); fmt.Sprint(got) == "" || !strings.Contains(fmt.Sprint(got), "target_review at 08:00") {
		t.Errorf("due = %v", got)
	}
	before := len(s.Sends())
	s.Tick()
	sends := s.WaitSends(before + 3)
	wantText := "Your average weight changed -0.1 lbs last week and -0.1 lbs the week before, against -1 lbs a week planned. That's two " +
		"weeks off pace, so I lowered your calories by 200, from 2,230 to 2,030. New targets: 2,030 cal, 175 g protein, 195 g carbs, 61 g fat."
	if sends[before+1] != wantText {
		t.Errorf("sends = %q\nwant %q", sends[before:], wantText)
	}
	if !strings.HasPrefix(sends[before], "Last week (") || !strings.HasPrefix(sends[before+2], "Good morning") {
		t.Errorf("order = %q", sends[before:])
	}

	// Running it again changes nothing and says the same thing.
	if got := reviewOn(t, s, week(6)); got != wantText {
		t.Errorf("rerun = %q", got)
	}
	s.Tick()
	if got := s.Rows(`SELECT tdee, adjustment, calories, reason FROM target_changes WHERE reason = 'review'`); fmt.Sprint(got) != "[2730 | -200 | 2030 | review]" {
		t.Errorf("changes = %v", got)
	}
	if got := s.Rows(`SELECT week_start::text, outcome FROM target_reviews ORDER BY week_start`); fmt.Sprint(got) !=
		"[2026-09-14 | on_track 2026-09-21 | on_track 2026-09-28 | watching 2026-10-05 | changed]" {
		t.Errorf("reviews = %v", got)
	}
	if got := goalRows(s, "2026-10-12"); got != "[calories | 2030 carbs | 195 fat | 61 protein | 175]" {
		t.Errorf("goals = %s", got)
	}
	if got := goalRows(s, "2026-10-11"); !strings.Contains(got, "calories | 2230") {
		t.Errorf("last week's goals = %s", got)
	}
	one(t, s, "why", "get_plan", `{}`, "Oct 12: 2,030 cal, weekly review: weight changed -0.1 lbs then -0.1 lbs against -1 lbs a week planned")
	// 34 days at 500 cal a day is 4.9 lbs.
	one(t, s, "by now?", "get_plan", `{}`, "the 7-day average went from 175 lbs to 173.2 lbs (-1.8 lbs), against about -4.9 lbs planned by now")

	// A slow week right after a change isn't sustained yet.
	weekOf(t, s, week(6), 173.1, 2030)
	if got := reviewOn(t, s, week(7)); !strings.HasPrefix(got, "Progress check") {
		t.Errorf("a week after a change = %q", got)
	}

	// Undoing an older change keeps what the review learned: 2,720 for the
	// lighter weight now, less 200, less the light cut's 250.
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE actions SET undone_at = now() WHERE tool <> 'set_state'`); err != nil {
		t.Fatal(err)
	}
	if got := s.Text("undo"); !strings.HasPrefix(got, "Undid setting the state to moderate cut. State: light cut.") {
		t.Errorf("undo = %q", got)
	}
	if got := goalRows(s, "2026-10-19"); !strings.Contains(got, "calories | 2270") {
		t.Errorf("goals after undoing the older change = %s", got)
	}
}

// TestWeeklyReviewBulk checks a bulk that's too slow gets more calories, and
// that nothing moves when the weigh-ins or calories can't show why.
func TestWeeklyReviewBulk(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	week1 := time.Date(2026, 9, 7, 12, 0, 0, 0, loc)
	week := func(n int) time.Time { return week1.AddDate(0, 0, 7*(n-1)) }

	s := sim.New(t, week1)
	logged(t, s, "weighed", "log_weight", `{"weight":175,"unit":null,"date":null}`)
	logged(t, s, "stats", "set_profile", stats175)
	logged(t, s, "bulk", "set_state", stateCall("bulk", `"standard"`))
	for i := 1; i <= 3; i++ {
		weekOf(t, s, week(i), 175, 2980)
	}
	if got := reviewOn(t, s, week(4)); !strings.Contains(got, "I raised your calories by 150, from 2,980 to 3,130") {
		t.Errorf("slow bulk = %q", got)
	}

	// Calories nowhere near the target leave it alone.
	s = sim.New(t, week1)
	logged(t, s, "weighed", "log_weight", `{"weight":175,"unit":null,"date":null}`)
	logged(t, s, "stats", "set_profile", stats175)
	logged(t, s, "bulk", "set_state", stateCall("bulk", `"standard"`))
	for i := 1; i <= 3; i++ {
		weekOf(t, s, week(i), 175, 2500)
	}
	if got := reviewOn(t, s, week(4)); !strings.Contains(got, "Your calories averaged 2,500 against a 2,980 target") {
		t.Errorf("off target = %q", got)
	}

	// So do calories nobody logged.
	s = sim.New(t, week1)
	logged(t, s, "weighed", "log_weight", `{"weight":175,"unit":null,"date":null}`)
	logged(t, s, "stats", "set_profile", stats175)
	logged(t, s, "bulk", "set_state", stateCall("bulk", `"standard"`))
	for i := 1; i <= 3; i++ {
		weekOf(t, s, week(i), 175, 0)
	}
	if got := reviewOn(t, s, week(4)); !strings.Contains(got, "I need calories logged at least 5 days a week") {
		t.Errorf("not logged = %q", got)
	}

	// The setting stops the text, and a manual user never gets one.
	logged(t, s, "off", "update_setting", `{"key":"target_review","value":"off"}`)
	if got, _ := s.Agent.DueTexts(s.Ctx, s.UserID, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)); strings.Contains(fmt.Sprint(got), "target_review") {
		t.Errorf("due with the setting off = %v", got)
	}
	s = sim.New(t, week1)
	if got, _ := s.Agent.DueTexts(s.Ctx, s.UserID, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)); strings.Contains(fmt.Sprint(got), "target_review") {
		t.Errorf("due for a manual user = %v", got)
	}
}

// TestTargetReached follows a light cut to its target weight: get_plan
// gives the dates, the summary the outlook, and the Monday review once the
// average reaches the target says so and changes nothing.
func TestTargetReached(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	week1 := time.Date(2026, 9, 7, 12, 0, 0, 0, loc)
	week := func(n int) time.Time { return week1.AddDate(0, 0, 7*(n-1)) }
	s := sim.New(t, week1)
	planned(t, s, `"light"`)
	one(t, s, "target 173", "set_goals", `{"goals":[{"metric":"weight","value":173,"unit":"lbs"}]}`, "")

	weekOf(t, s, week(1), 175, 2480)
	weekOf(t, s, week(2), 174.4, 2480)
	// Mon Sep 21: 1.4 lbs at 0.5 a week is 20 days; at the 0.3 a week since
	// Sep 7's 175, 33 days.
	s.Set(week(3))
	one(t, s, "when will I get there", "get_plan", `{}`,
		"Target weight outlook: target weight 173 lbs, 7-day average 174.4 lbs; at the planned -0.5 lbs a week, reached around Oct 11 "+
			"(about 3 weeks); at the last 2 weeks' pace of -0.3 lbs a week, around Oct 24 (about 5 weeks).")
	sum, err := s.Agent.CheckinText(s.Ctx, s.UserID, time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), "weekly_summary", "")
	if err != nil || !strings.Contains(sum, "\nAt your planned 0.5 lbs a week, you'd reach 173 lbs around Oct 10") {
		t.Errorf("summary = %q, %v", sum, err)
	}

	weekOf(t, s, week(3), 173.6, 2480)
	if got := reviewOn(t, s, week(4)); got != "" {
		t.Errorf("on track = %q", got)
	}
	weekOf(t, s, week(4), 172.8, 2480)
	want := "You've reached your 173 lbs target: your average weight last week was 172.8 lbs. Want to hold it there? " +
		`Reply "maintain" and I'll set your calories to maintenance, or tell me a new target.`
	if got := reviewOn(t, s, week(5)); got != want {
		t.Errorf("reached = %q\nwant %q", got, want)
	}
	if got := reviewOn(t, s, week(5)); got != want {
		t.Errorf("rerun = %q", got)
	}
	if got := s.Rows(`SELECT outcome FROM target_reviews ORDER BY week_start DESC LIMIT 1`); fmt.Sprint(got) != "[reached]" {
		t.Errorf("outcome = %v", got)
	}
	if got := s.Rows(`SELECT count(*) FROM target_changes WHERE reason = 'review'`); fmt.Sprint(got) != "[0]" {
		t.Errorf("review changes = %v", got)
	}
	one(t, s, "am I done", "get_plan", `{}`, "target weight 173 lbs, 7-day average 172.8 lbs; reached. Suggest maintain to hold it, or a new target.")
}
