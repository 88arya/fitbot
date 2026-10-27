package agent_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// planCall builds plan_session arguments: date, change, to_date, and time,
// each JSON or null except change.
func planCall(date, change, to, at string) string {
	return fmt.Sprintf(`{"date":%s,"change":"%s","to_date":%s,"time":%s}`, date, change, to, at)
}

// weeklyPPL is push/pull/legs pinned to weekdays, with cardio after pull,
// rest on Sunday, a usual time of 6 PM, and Saturday at 10 AM.
const weeklyPPL = `{"kind":"weekly","days":[` +
	`{"weekday":"mon","sessions":["Push"],"time":null},` +
	`{"weekday":"tue","sessions":["pull","cardio"],"time":null},` +
	`{"weekday":"wed","sessions":["legs"],"time":null},` +
	`{"weekday":"thu","sessions":["push"],"time":null},` +
	`{"weekday":"fri","sessions":["pull","cardio"],"time":null},` +
	`{"weekday":"sat","sessions":["legs"],"time":"10:00"},` +
	`{"weekday":"sun","sessions":["rest"],"time":null}],` +
	`"cycle_day_today":null,"training_time":"18:00"}`

// rotation builds a rotation's set_schedule arguments from its days and
// the cycle day today is, as JSON or null.
func rotation(today string, days ...string) string {
	var parts []string
	for _, d := range days {
		parts = append(parts, fmt.Sprintf(`{"weekday":null,"sessions":["%s"],"time":null}`, d))
	}
	return fmt.Sprintf(`{"kind":"rotation","days":[%s],"cycle_day_today":%s,"training_time":null}`, strings.Join(parts, ","), today)
}

func planLines(t *testing.T, s *sim.Sim, first time.Time, n int) []string {
	t.Helper()
	lines, err := s.Agent.PlanLines(s.Ctx, s.UserID, first, n)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// checkPlan checks the planned session for every day from first.
func checkPlan(t *testing.T, s *sim.Sim, first time.Time, want []string) {
	t.Helper()
	got := planLines(t, s, first, len(want))
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("day %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func dayName(d time.Time) string {
	return d.Format("Mon Jan 2")
}

// TestWeeklySchedule sets a weekly PPL with cardio days, then skips, moves,
// and times sessions, and checks the planned session for every day over
// four weeks, the prompt, and that undo reverses each change.
func TestWeeklySchedule(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	// Monday.
	start := time.Date(2026, 10, 26, 8, 0, 0, 0, loc)
	s := sim.New(t, start)

	r := logged(t, s, "push mon/thu, pull and cardio tue/fri, legs wed/sat at 10am, usually 6pm", "set_schedule", weeklyPPL)
	want := "saved, starting today: weekly: push Mon/Thu, pull + cardio Tue/Fri, legs Wed, legs Sat at 10:00 AM, rest Sun; " +
		"usually at 6:00 PM. Today: push at 6:00 PM."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("set_schedule = %q\nwant %q", r, want)
	}

	r = logged(t, s, "skipping thursday", "plan_session", planCall(`"2026-10-29"`, "skip", "null", "null"))
	if want := "saved. Thu Oct 29: push, skipped."; fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("skip = %q, want %q", r, want)
	}
	// A retried webhook and a rerun of the same text skip nothing more.
	s.Retry()
	s.Script(sim.Call("plan_session", planCall(`"2026-10-29"`, "skip", "null", "null")), sim.Say("ok"))
	s.Rerun()
	if got := lastResults(s); fmt.Sprint(got) != "[saved. Thu Oct 29: push, skipped.]" {
		t.Errorf("rerun = %q", got)
	}
	r = logged(t, s, "gym at 7am wednesday", "plan_session", planCall(`"2026-10-28"`, "time", "null", `"07:00"`))
	if want := "saved. Wed Oct 28: legs at 7:00 AM."; fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("time = %q, want %q", r, want)
	}
	r = logged(t, s, "doing next friday's pull on sunday instead, at 11", "plan_session",
		planCall(`"2026-11-06"`, "move", `"2026-11-08"`, `"11:00"`))
	want = "saved. Fri Nov 6: pull + cardio, moved to Sun Nov 8; Sun Nov 8: pull + cardio at 11:00 AM (pull + cardio moved here)."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("move = %q\nwant %q", r, want)
	}

	// Every day for four weeks: the weekday's sessions, except the days
	// changed above.
	week := []string{"rest", "push at 6:00 PM", "pull + cardio at 6:00 PM", "legs at 6:00 PM", "push at 6:00 PM",
		"pull + cardio at 6:00 PM", "legs at 10:00 AM"}
	changed := map[string]string{
		"2026-10-28": "legs at 7:00 AM",
		"2026-10-29": "push, skipped",
		"2026-11-06": "pull + cardio, moved to Sun Nov 8",
		"2026-11-08": "pull + cardio at 11:00 AM (pull + cardio moved here)",
	}
	var all []string
	for i := range 28 {
		d := start.AddDate(0, 0, i)
		line, ok := changed[d.Format(time.DateOnly)]
		if !ok {
			line = week[d.Weekday()]
		}
		all = append(all, dayName(d)+": "+line)
	}
	checkPlan(t, s, start, all)

	// The prompt lists the schedule and the next 7 days.
	s.Script(sim.Say("ok"))
	s.Text("what's this week")
	reqs := s.Requests()
	sys := reqs[len(reqs)-1][0].Content
	for _, w := range []string{
		"Schedule: weekly: push Mon/Thu, pull + cardio Tue/Fri, legs Wed, legs Sat at 10:00 AM, rest Sun; usually at 6:00 PM\n",
		"Mon Oct 26 (today): push at 6:00 PM\nTue Oct 27 (tomorrow): pull + cardio at 6:00 PM\nWed Oct 28: legs at 7:00 AM\n" +
			"Thu Oct 29: push, skipped\nFri Oct 30: pull + cardio at 6:00 PM\nSat Oct 31: legs at 10:00 AM\nSun Nov 1: rest\n",
	} {
		if !strings.Contains(sys, w) {
			t.Errorf("prompt lacks %q:\n%s", w, sys)
		}
	}

	// Clearing the move puts Friday back and takes it off Sunday.
	r = logged(t, s, "actually friday is fine", "plan_session", planCall(`"2026-11-06"`, "clear", "null", "null"))
	if want := "saved. Fri Nov 6: pull + cardio at 6:00 PM; Sun Nov 8: rest."; fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("clear = %q, want %q", r, want)
	}

	// Each undo reverses one change, newest first, without the LLM.
	llmCalls := len(s.Requests())
	for _, w := range []string{
		"Undid bringing back pull + cardio on Fri Nov 6. Fri Nov 6: pull + cardio, moved to Sun Nov 8; " +
			"Sun Nov 8: pull + cardio at 11:00 AM (pull + cardio moved here).",
		"Undid moving pull + cardio from Fri Nov 6 to Sun Nov 8. Fri Nov 6: pull + cardio at 6:00 PM; Sun Nov 8: rest.",
		"Undid setting legs on Wed Oct 28 for 7:00 AM. Wed Oct 28: legs at 6:00 PM.",
		"Undid skipping push on Thu Oct 29. Thu Oct 29: push at 6:00 PM.",
		"Undid setting your training schedule (weekly: push Mon/Thu, pull + cardio Tue/Fri, legs Wed, legs Sat at 10:00 AM, " +
			"rest Sun; usually at 6:00 PM). Schedule: none, the user doesn't follow one.",
	} {
		if reply := s.Text("undo"); reply != w {
			t.Errorf("undo = %q\nwant %q", reply, w)
		}
	}
	if n := len(s.Requests()); n != llmCalls {
		t.Errorf("undo made %d LLM calls", n-llmCalls)
	}
	if got := s.Rows(`SELECT count(*) FROM session_plans`); fmt.Sprint(got) != "[0]" {
		t.Errorf("plans left after undo = %v", got)
	}
	if got := s.Rows(`SELECT kind IS NULL, start_date::text FROM training_schedules`); fmt.Sprint(got) != "[true | 2026-10-26]" {
		t.Errorf("schedules after undo = %v", got)
	}
}

// rotationWalk works out a rotation day by day, the way a user would: each
// day takes the next day of the cycle, a skipped day still uses its turn,
// and a move holds the cycle until the day it moved to.
func rotationWalk(start time.Time, n int, cycle []string, first int, skips map[string]bool, moves map[string]string) []string {
	var out []string
	idx := first
	until := ""
	for i := range n {
		d := start.AddDate(0, 0, i)
		key := d.Format(time.DateOnly)
		line := cycle[idx%len(cycle)]
		switch {
		case until != "" && key < until:
			line = "nothing, the rotation waits for the session moved to " + until
		case moves[key] != "":
			until = moves[key]
			line += ", moved to " + until
		case skips[key]:
			line += ", skipped"
			idx++
		default:
			idx++
		}
		out = append(out, dayName(d)+": "+line)
	}
	// Moves are given as dates; show them as day names.
	for i := range out {
		for _, to := range moves {
			day, _ := time.Parse(time.DateOnly, to)
			out[i] = strings.ReplaceAll(out[i], to, dayName(day))
		}
	}
	return out
}

// TestRotationSchedule sets a push, pull, legs, rest rotation that doesn't
// follow weekdays, skips one session, and moves another, which shifts the
// rest of the cycle, and checks every day over four weeks.
func TestRotationSchedule(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	start := time.Date(2026, 10, 26, 8, 0, 0, 0, loc)
	s := sim.New(t, start)
	cycle := []string{"push", "pull", "legs", "rest"}

	// Today is pull, the second day of the cycle.
	r := logged(t, s, "ppl then a rest day, today's pull", "set_schedule", rotation("2", cycle...))
	if want := "saved, starting today: 4-day rotation: push, pull, legs, rest. Today: pull."; fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("set_schedule = %q, want %q", r, want)
	}
	checkPlan(t, s, start, rotationWalk(start, 28, cycle, 1, nil, nil))

	// Oct 27 is legs; skipping it keeps the cycle where it is.
	r = logged(t, s, "skipping legs tomorrow", "plan_session", planCall(`"2026-10-27"`, "skip", "null", "null"))
	if want := "saved. Tue Oct 27 (tomorrow): legs, skipped."; fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("skip = %q, want %q", r, want)
	}
	// Oct 30 is pull; moving it to Nov 1 waits through Oct 31, and
	// everything after shifts two days later.
	r = logged(t, s, "doing friday's pull on sunday", "plan_session", planCall(`"2026-10-30"`, "move", `"2026-11-01"`, "null"))
	want := "saved. Fri Oct 30: pull, moved to Sun Nov 1; Sat Oct 31: nothing, the rotation waits for the session moved to Sun Nov 1; " +
		"Sun Nov 1: pull."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("move = %q\nwant %q", r, want)
	}
	walk := rotationWalk(start, 28, cycle, 1, map[string]bool{"2026-10-27": true}, map[string]string{"2026-10-30": "2026-11-01"})
	checkPlan(t, s, start, walk)
	if walk[6] != "Sun Nov 1: pull" || walk[7] != "Mon Nov 2: legs" {
		t.Errorf("walk = %q", walk[4:8])
	}

	// A second move later in the cycle shifts it again.
	r = logged(t, s, "push on the 4th moves to the 5th", "plan_session", planCall(`"2026-11-04"`, "move", `"2026-11-05"`, "null"))
	if len(r) != 1 || !strings.HasPrefix(r[0], "saved. Wed Nov 4: push, moved to Thu Nov 5; Thu Nov 5: push.") {
		t.Errorf("second move = %q", r)
	}
	checkPlan(t, s, start, rotationWalk(start, 28, cycle, 1, map[string]bool{"2026-10-27": true},
		map[string]string{"2026-10-30": "2026-11-01", "2026-11-04": "2026-11-05"}))

	// A move or clear that would shift the cycle under a later skip or move
	// is refused.
	r = logged(t, s, "move today to wednesday", "plan_session", planCall("null", "move", `"2026-10-28"`, "null"))
	want = "error: Tue Oct 27 is already legs, skipped, and this would shift the rotation under it; clear it first"
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("move before a skip = %q\nwant %q", r, want)
	}
	r = logged(t, s, "friday after all", "plan_session", planCall(`"2026-10-30"`, "clear", "null", "null"))
	want = "error: Wed Nov 4 is already push, moved to Thu Nov 5, and this would shift the rotation under it; clear it first"
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("clear before a move = %q\nwant %q", r, want)
	}
	r = logged(t, s, "skip saturday", "plan_session", planCall(`"2026-10-31"`, "skip", "null", "null"))
	want = "error: nothing is planned on Sat Oct 31; the rotation waits for the session moved to Sun Nov 1"
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("skip a paused day = %q\nwant %q", r, want)
	}

	// The prompt says where today is in the cycle.
	s.Script(sim.Say("ok"))
	s.Text("what's next")
	reqs := s.Requests()
	if sys := reqs[len(reqs)-1][0].Content; !strings.Contains(sys, "Schedule: 4-day rotation: push, pull, legs, rest (today is day 2 of 4)\n") {
		t.Errorf("prompt lacks the cycle day:\n%s", sys)
	}

	// Undoing the moves puts the cycle back.
	for _, w := range []string{
		"Undid moving push from Wed Nov 4 to Thu Nov 5. Wed Nov 4: push; Thu Nov 5: pull.",
		"Undid moving pull from Fri Oct 30 to Sun Nov 1. Fri Oct 30: pull; Sat Oct 31: legs; Sun Nov 1: rest.",
	} {
		if reply := s.Text("undo"); reply != w {
			t.Errorf("undo = %q\nwant %q", reply, w)
		}
	}
	checkPlan(t, s, start, rotationWalk(start, 28, cycle, 1, map[string]bool{"2026-10-27": true}, nil))
}

// TestTwoDaySplit is a full body day then a rest day, repeating whatever
// the weekday. Moving a full body day onto the rest day after it pushes
// the cycle back a day.
func TestTwoDaySplit(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	start := time.Date(2026, 10, 26, 8, 0, 0, 0, loc)
	s := sim.New(t, start)
	cycle := []string{"full body", "rest"}

	logged(t, s, "full body one day, rest the next", "set_schedule", rotation("null", cycle...))
	checkPlan(t, s, start, rotationWalk(start, 28, cycle, 0, nil, nil))
	// Day 9 lands on a Tuesday, unlike a weekly plan.
	if got := planLines(t, s, start, 9)[8]; got != "Tue Nov 3: full body" {
		t.Errorf("Nov 3 = %q", got)
	}

	r := logged(t, s, "can't make it today, tomorrow instead", "plan_session", planCall("null", "move", `"2026-10-27"`, `"07:00"`))
	if want := "saved. Mon Oct 26 (today): full body, moved to Tue Oct 27; Tue Oct 27 (tomorrow): full body at 7:00 AM."; fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("move = %q, want %q", r, want)
	}
	want := rotationWalk(start, 28, cycle, 0, nil, map[string]string{"2026-10-26": "2026-10-27"})
	want[1] += " at 7:00 AM"
	checkPlan(t, s, start, want)
	if want[2] != "Wed Oct 28: rest" || want[3] != "Thu Oct 29: full body" {
		t.Errorf("walk = %q", want[:4])
	}
}

// TestScheduleChanges replaces a schedule the same day and on a later day,
// and checks skips and moves made for the old one are cleared, past days
// keep the schedule they had, and undo brings each back.
func TestScheduleChanges(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	start := time.Date(2026, 10, 26, 8, 0, 0, 0, loc)
	s := sim.New(t, start)

	logged(t, s, "ppl", "set_schedule", weeklyPPL)
	logged(t, s, "skip tomorrow", "plan_session", planCall(`"2026-10-27"`, "skip", "null", "null"))
	logged(t, s, "move thursday to friday", "plan_session", planCall(`"2026-10-29"`, "move", `"2026-10-30"`, "null"))
	logged(t, s, "gym at 5 on saturday", "plan_session", planCall(`"2026-10-31"`, "time", "null", `"17:00"`))

	// Next day: a new schedule from Tuesday on. Monday keeps push; Tuesday's
	// skip and the move are cleared; Saturday's time stays.
	s.Set(time.Date(2026, 10, 27, 9, 0, 0, 0, loc))
	r := logged(t, s, "switching to upper lower", "set_schedule", `{"kind":"weekly","days":[`+
		`{"weekday":"mon","sessions":["upper"],"time":null},{"weekday":"tue","sessions":["lower"],"time":null},`+
		`{"weekday":"thu","sessions":["upper"],"time":null},{"weekday":"sat","sessions":["lower"],"time":"08:00"}],`+
		`"cycle_day_today":null,"training_time":null}`)
	want := "saved, starting today: weekly: upper Mon/Thu, lower Tue, rest Wed/Fri/Sun, lower Sat at 8:00 AM. Today: lower. " +
		"Cleared 2 skips or moves made for the old schedule."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("new schedule = %q\nwant %q", r, want)
	}
	checkPlan(t, s, start, []string{"Mon Oct 26: push at 6:00 PM", "Tue Oct 27: lower", "Wed Oct 28: rest", "Thu Oct 29: upper",
		"Fri Oct 30: rest", "Sat Oct 31: lower at 5:00 PM"})

	// The same day, the user corrects it: the earlier change is replaced,
	// not kept as a second schedule.
	r = logged(t, s, "actually a 3 day rotation", "set_schedule", rotation("null", "upper", "lower", "rest"))
	if want := "saved, starting today: 3-day rotation: upper, lower, rest. Today: upper."; fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("same-day change = %q, want %q", r, want)
	}
	if got := s.Rows(`SELECT kind, start_date::text FROM training_schedules WHERE kind IS NOT NULL ORDER BY start_date`); fmt.Sprint(got) !=
		"[weekly | 2026-10-26 rotation | 2026-10-27]" {
		t.Errorf("schedules = %v", got)
	}

	for _, w := range []string{
		"Undid setting your training schedule (3-day rotation: upper, lower, rest). Schedule: weekly: upper Mon/Thu, lower Tue, " +
			"rest Wed/Fri/Sun, lower Sat at 8:00 AM. Today: lower.",
		"Undid setting your training schedule (weekly: upper Mon/Thu, lower Tue, rest Wed/Fri/Sun, lower Sat at 8:00 AM). " +
			"Schedule: weekly: push Mon/Thu, pull + cardio Tue/Fri, legs Wed, legs Sat at 10:00 AM, rest Sun; usually at 6:00 PM. " +
			"Today: pull + cardio, skipped.",
	} {
		if reply := s.Text("undo"); reply != w {
			t.Errorf("undo = %q\nwant %q", reply, w)
		}
	}
	checkPlan(t, s, start, []string{"Mon Oct 26: push at 6:00 PM", "Tue Oct 27: pull + cardio, skipped", "Wed Oct 28: legs at 6:00 PM",
		"Thu Oct 29: push, moved to Fri Oct 30", "Fri Oct 30: pull + cardio + push at 6:00 PM (push moved here)", "Sat Oct 31: legs at 5:00 PM"})
}

// TestPlanRefused checks invalid schedules and plan changes are refused
// and change nothing.
func TestPlanRefused(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 8, 0, 0, 0, loc))

	if r := logged(t, s, "skip today", "plan_session", planCall("null", "skip", "null", "null")); fmt.Sprint(r) !=
		"[error: no training schedule; save one with set_schedule first]" {
		t.Errorf("no schedule = %q", r)
	}
	day := func(weekday, sessions, at string) string {
		return fmt.Sprintf(`{"weekday":%s,"sessions":%s,"time":%s}`, weekday, sessions, at)
	}
	sched := func(kind string, today, at string, days ...string) string {
		return fmt.Sprintf(`{"kind":%s,"days":[%s],"cycle_day_today":%s,"training_time":%s}`, kind, strings.Join(days, ","), today, at)
	}
	long := make([]string, 29)
	for i := range long {
		long[i] = day("null", `["push"]`, "null")
	}
	for _, c := range []struct{ args, want string }{
		{sched("null", "null", "null", day(`"mon"`, `["push"]`, "null")),
			"error: kind null means no schedule, so days, cycle_day_today, and training_time must be empty"},
		{sched(`"daily"`, "null", "null"), `error: kind "daily" must be weekly, rotation, or null`},
		{sched(`"weekly"`, "2", "null", day(`"mon"`, `["push"]`, "null")), "error: cycle_day_today is only for a rotation"},
		{sched(`"weekly"`, "null", "null", day("null", `["push"]`, "null")), "error: every day of a weekly schedule needs its weekday"},
		{sched(`"weekly"`, "null", "null", day(`"monday"`, `["push"]`, "null")),
			`error: weekday "monday" must be one of sun, mon, tue, wed, thu, fri, sat`},
		{sched(`"weekly"`, "null", "null", day(`"mon"`, `["push"]`, "null"), day(`"mon"`, `["legs"]`, "null")),
			"error: mon is listed twice; list all of a day's sessions together"},
		{sched(`"weekly"`, "null", "null", day(`"mon"`, `["rest"]`, "null")), "error: the schedule has no training days"},
		{sched(`"weekly"`, "null", "null", day(`"mon"`, `["rest","push"]`, "null")), "error: a rest day has no other sessions"},
		{sched(`"weekly"`, "null", "null", day(`"mon"`, `["push"," Push "]`, "null")), "error: push is listed twice on one day"},
		{sched(`"weekly"`, "null", "null", day(`"mon"`, `["  "]`, "null")), "error: a session name is empty"},
		{sched(`"weekly"`, "null", "null", day(`"mon"`, `["a","b","c","d","e"]`, "null")), "error: a day can have at most 4 sessions"},
		{sched(`"weekly"`, "null", "null", day(`"sun"`, `["rest"]`, `"10:00"`)), "error: a rest day has no time"},
		{sched(`"weekly"`, "null", "null", day(`"mon"`, `["push"]`, `"6pm"`)), `error: time "6pm" is not a 24-hour time like 18:00`},
		{sched(`"weekly"`, "null", `"25:00"`, day(`"mon"`, `["push"]`, "null")),
			`error: training_time "25:00" is not a 24-hour time like 18:00`},
		{sched(`"rotation"`, "null", "null"), "error: a rotation lists every day of its cycle in order, 1 to 28 days"},
		{sched(`"rotation"`, "null", "null", long...), "error: a rotation lists every day of its cycle in order, 1 to 28 days"},
		{sched(`"rotation"`, "3", "null", day("null", `["push"]`, "null"), day("null", `["rest"]`, "null")),
			"error: cycle_day_today must be 1 to 2, the days in the rotation"},
		{sched(`"rotation"`, "null", "null", day(`"mon"`, `["push"]`, "null")),
			"error: a rotation's days follow the cycle, not weekdays, so weekday must be null"},
	} {
		if r := logged(t, s, "schedule", "set_schedule", c.args); fmt.Sprint(r) != "["+c.want+"]" {
			t.Errorf("%s = %q\nwant %q", c.args, r, c.want)
		}
	}
	if got := s.Rows(`SELECT count(*) FROM training_schedules`); fmt.Sprint(got) != "[1]" {
		t.Errorf("schedules = %v, want only the one setup saved", got)
	}

	logged(t, s, "ppl", "set_schedule", weeklyPPL)
	logged(t, s, "skip tomorrow", "plan_session", planCall(`"2026-10-27"`, "skip", "null", "null"))
	for _, c := range []struct{ args, want string }{
		{planCall(`"2026-11-01"`, "skip", "null", "null"), "error: Sun Nov 1 is a rest day, with nothing planned"},
		{planCall(`"2026-11-01"`, "time", "null", `"10:00"`), "error: Sun Nov 1 is a rest day, with nothing planned"},
		{planCall(`"2026-10-25"`, "skip", "null", "null"),
			"error: date is in the past; today is 2026-10-26, and only today or later can be planned"},
		{planCall(`"2026-11-10"`, "skip", "null", "null"), "error: date is more than 14 days ahead"},
		{planCall(`"Oct 28"`, "skip", "null", "null"), `error: date "Oct 28" is not YYYY-MM-DD`},
		{planCall("null", "cancel", "null", "null"), `error: change "cancel" must be skip, move, time, or clear`},
		{planCall("null", "move", "null", "null"), "error: move needs to_date, the day the session moves to"},
		{planCall(`"2026-10-28"`, "move", `"2026-10-28"`, "null"), "error: to_date must be after date; a session can only move later"},
		{planCall("null", "move", `"2026-11-03"`, "null"), "error: to_date is more than 7 days after date"},
		{planCall("null", "move", `"Tuesday"`, "null"), `error: to_date "Tuesday" is not YYYY-MM-DD`},
		{planCall("null", "skip", `"2026-10-28"`, "null"), "error: to_date is only for move"},
		{planCall("null", "skip", "null", `"18:00"`), "error: time is only for move and time"},
		{planCall("null", "time", "null", `"6pm"`), `error: time "6pm" is not a 24-hour time like 18:00`},
		{planCall("null", "time", "null", "null"), "error: no time is set for Mon Oct 26 itself; give time to set one"},
		{planCall(`"2026-10-27"`, "skip", "null", "null"), "error: Tue Oct 27 is already pull + cardio, skipped; clear it first"},
		{planCall("null", "move", `"2026-10-27"`, "null"), "error: Tue Oct 27 is already pull + cardio, skipped; clear it first"},
		{planCall(`"2026-10-28"`, "clear", "null", "null"), "error: nothing is skipped or moved on Wed Oct 28"},
	} {
		if r := logged(t, s, "plan", "plan_session", c.args); fmt.Sprint(r) != "["+c.want+"]" {
			t.Errorf("%s = %q\nwant %q", c.args, r, c.want)
		}
	}
	if got := s.Rows(`SELECT local_date::text, status, sessions::text FROM session_plans`); fmt.Sprint(got) !=
		"[2026-10-27 | skipped | {pull,cardio}]" {
		t.Errorf("plans = %v", got)
	}
	// A move can't be cleared while the day it went to is skipped.
	logged(t, s, "move thursday to saturday", "plan_session", planCall(`"2026-10-29"`, "move", `"2026-10-31"`, "null"))
	logged(t, s, "skip saturday", "plan_session", planCall(`"2026-10-31"`, "skip", "null", "null"))
	if r := logged(t, s, "thursday after all", "plan_session", planCall(`"2026-10-29"`, "clear", "null", "null")); fmt.Sprint(r) !=
		"[error: Sat Oct 31, where it moved, is already legs + push, skipped; clear that first]" {
		t.Errorf("clear under a skip = %q", r)
	}
	if !slices.Contains(planLines(t, s, time.Date(2026, 10, 29, 0, 0, 0, 0, loc), 1), "Thu Oct 29: push, moved to Sat Oct 31") {
		t.Errorf("move changed by refused clear")
	}
	// The last day that can be planned can still move a week on.
	if r := logged(t, s, "move the 9th to the 11th", "plan_session", planCall(`"2026-11-09"`, "move", `"2026-11-11"`, "null")); fmt.Sprint(r) !=
		"[saved. Mon Nov 9: push, moved to Wed Nov 11; Wed Nov 11: legs + push at 6:00 PM (push moved here).]" {
		t.Errorf("late move = %q", r)
	}
}
