package agent_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// weightCall builds log_weight arguments: weight, unit, and date, each JSON
// or null.
func weightCall(weight, unit, date string) string {
	return fmt.Sprintf(`{"weight":%s,"unit":%s,"date":%s}`, weight, unit, date)
}

// editWeight builds edit_log arguments for a weigh-in.
func editWeight(id, amount, unit, date string) string {
	return fmt.Sprintf(`{"type":"weight","id":%s,"amount":%s,"unit":%s,"bedtime":null,"wake_time":null,"date":%s}`,
		id, amount, unit, date)
}

// showLbs shows tenths of a pound as results do, like "182.4 lbs" or "182
// lbs".
func showLbs(v int) string {
	return strings.TrimSuffix(fmt.Sprintf("%d.%d", v/10, v%10), ".0") + " lbs"
}

// TestWeight logs weigh-ins in lbs and kg, two in a day, and one a week
// back, edits, moves, and deletes them, undoes every change, and checks
// rows and statuses.
func TestWeight(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	at := func(h int) time.Time { return time.Date(2026, 10, 26, h, 0, 0, 0, loc) }
	s := sim.New(t, at(7))
	// A 175 lbs target from well before the sim starts.
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE goals SET value = 79379, start_date = '2026-10-01' WHERE user_id = $1 AND metric = 'weight'`,
		s.UserID); err != nil {
		t.Fatal(err)
	}
	target := "target weight: 175 lbs, 7-day average "

	r := logged(t, s, "weighed 182", "log_weight", weightCall("182", "null", "null"))
	first := s.Rows(`SELECT max(id) FROM weight_logs`)[0]
	want := "logged #" + first + ": 182 lbs today. weight: 182 lbs today; last 7 days: 182 lbs over 1 day weighed, " +
		"none the 7 days before to compare; " + target + "7 lbs above."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("log = %q\nwant %q", r, want)
	}
	// A retried webhook and a rerun of the same text log nothing more.
	s.Retry()
	s.Script(sim.Call("log_weight", weightCall("182", "null", "null")), sim.Say("ok"))
	s.Rerun()
	if got := lastResults(s); fmt.Sprint(got) != "["+want+"]" {
		t.Errorf("rerun = %q", got)
	}
	if got := s.Rows(`SELECT count(*) FROM weight_logs`); fmt.Sprint(got) != "[1]" {
		t.Errorf("weigh-ins after rerun = %v", got)
	}

	// kg is converted to lbs and shown as given.
	r = logged(t, s, "82.5 kg yesterday", "log_weight", weightCall("82.5", `"kg"`, `"2026-10-25"`))
	kg := s.Rows(`SELECT max(id) FROM weight_logs`)[0]
	want = "logged #" + kg + ": 181.9 lbs (82.5 kg) yesterday. weight: 181.9 lbs yesterday; last 7 days: 182 lbs average " +
		"over 2 days weighed, none the 7 days before to compare; " + target + "7 lbs above."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("log kg = %q\nwant %q", r, want)
	}
	r = logged(t, s, "I was 184 last sunday", "log_weight", weightCall("184", `"lbs"`, `"2026-10-18"`))
	before := s.Rows(`SELECT max(id) FROM weight_logs`)[0]
	want = "logged #" + before + ": 184 lbs on Oct 18. weight: 184 lbs on Oct 18; last 7 days: 182 lbs average over 2 days " +
		"weighed, down 2 lbs from 184 lbs the 7 days before; " + target + "7 lbs above."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("log week before = %q\nwant %q", r, want)
	}
	// A second weigh-in today is kept, but the first is today's weight.
	s.Set(at(13))
	r = logged(t, s, "183.4 after lunch", "log_weight", weightCall("183.4", "null", "null"))
	second := s.Rows(`SELECT max(id) FROM weight_logs`)[0]
	want = "logged #" + second + ": 183.4 lbs today. weight: 182 lbs today (the first of 2 weigh-ins that day); last 7 days: " +
		"182 lbs average over 2 days weighed, down 2 lbs from 184 lbs the 7 days before; " + target + "7 lbs above."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("second log = %q\nwant %q", r, want)
	}

	// The prompt lists the week's weigh-ins, newest first, with the status.
	s.Script(sim.Say("ok"))
	s.Text("how's my weight")
	reqs := s.Requests()
	sys := reqs[len(reqs)-1][0].Content
	for _, w := range []string{
		"\n## Body weight\nWeigh-ins in the last 7 days, newest first:\n#" + second +
			" Mon Oct 26 (today): 183.4 lbs (not the day's first, so not counted)\n#" + first +
			" Mon Oct 26 (today): 182 lbs\n#" + kg + " Sun Oct 25: 181.9 lbs\nStatus: weight: 182 lbs today (the first of 2 " +
			"weigh-ins that day); last 7 days: 182 lbs average over 2 days weighed, down 2 lbs from 184 lbs the 7 days before; " +
			target + "7 lbs above.\n",
		"target weight: 175 lbs\n",
	} {
		if !strings.Contains(sys, w) {
			t.Errorf("prompt lacks %q:\n%s", w, sys)
		}
	}

	// A null id edits the most recently logged weigh-in.
	r = logged(t, s, "make that 183", "edit_log", editWeight("null", "183", "null", "null"))
	want = "updated #" + second + ": now 183 lbs today. weight: 182 lbs today (the first of 2 weigh-ins that day); last 7 days: " +
		"182 lbs average over 2 days weighed, down 2 lbs from 184 lbs the 7 days before; " + target + "7 lbs above."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("edit latest = %q\nwant %q", r, want)
	}
	r = logged(t, s, "this morning was 181", "edit_log", editWeight(first, "181", `"lbs"`, "null"))
	want = "updated #" + first + ": now 181 lbs today. weight: 181 lbs today (the first of 2 weigh-ins that day); last 7 days: " +
		"181.5 lbs average over 2 days weighed, down 2.5 lbs from 184 lbs the 7 days before; " + target + "6.5 lbs above."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("edit first = %q\nwant %q", r, want)
	}
	r = logged(t, s, "the kg one was saturday", "edit_log", editWeight(kg, "null", "null", `"2026-10-24"`))
	want = "updated #" + kg + ": now 181.9 lbs on Oct 24. weight: 181.9 lbs on Oct 24; weight: nothing logged yesterday; " +
		"last 7 days: 181.5 lbs average over 2 days weighed, down 2.5 lbs from 184 lbs the 7 days before; " + target + "6.5 lbs above."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("move = %q\nwant %q", r, want)
	}
	// Deleting the first makes the second today's weight.
	r = logged(t, s, "delete this morning's", "delete_log", `{"type":"weight","id":`+first+`}`)
	want = "deleted #" + first + ": 181 lbs today. weight: 183 lbs today; last 7 days: 182.5 lbs average over 2 days weighed, " +
		"down 1.5 lbs from 184 lbs the 7 days before; " + target + "7.5 lbs above."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("delete = %q\nwant %q", r, want)
	}

	// Each undo reverses one change, newest first, without the LLM.
	llmCalls := len(s.Requests())
	week := "Last 7 days: 182 lbs average over 2 days weighed, down 2 lbs from 184 lbs the 7 days before. Target weight: 175 lbs, " +
		"7-day average 7 lbs above."
	edited := "Last 7 days: 181.5 lbs average over 2 days weighed, down 2.5 lbs from 184 lbs the 7 days before. Target weight: " +
		"175 lbs, 7-day average 6.5 lbs above."
	for _, w := range []string{
		"Undid deleting 181 lbs for Oct 26. Weight: 181 lbs today (the first of 2 weigh-ins that day). " + edited,
		"Undid moving 181.9 lbs from Oct 25 to Oct 24. Weight: 181.9 lbs yesterday. " + edited,
		"Undid changing 182 lbs to 181 lbs for Oct 26. Weight: 182 lbs today (the first of 2 weigh-ins that day). " + week,
		"Undid changing 183.4 lbs to 183 lbs for Oct 26. Weight: 182 lbs today (the first of 2 weigh-ins that day). " + week,
		"Undid logging 183.4 lbs for Oct 26. Weight: 182 lbs today. " + week,
		"Undid logging 184 lbs for Oct 18. Weight: nothing logged on Oct 18. Last 7 days: 182 lbs average over 2 days weighed, " +
			"none the 7 days before to compare. Target weight: 175 lbs, 7-day average 7 lbs above.",
		"Undid logging 181.9 lbs for Oct 25. Weight: nothing logged yesterday. Last 7 days: 182 lbs over 1 day weighed, " +
			"none the 7 days before to compare. Target weight: 175 lbs, 7-day average 7 lbs above.",
		"Undid logging 182 lbs for Oct 26. Weight: nothing logged today. Last 7 days: no weigh-ins. Target weight: 175 lbs.",
	} {
		if reply := s.Text("undo"); reply != w {
			t.Errorf("undo = %q\nwant %q", reply, w)
		}
	}
	if n := len(s.Requests()); n != llmCalls {
		t.Errorf("undo made %d LLM calls", n-llmCalls)
	}
	if got := s.Rows(`SELECT local_date::text, grams, deleted_at IS NOT NULL FROM weight_logs ORDER BY id`); fmt.Sprint(got) !=
		"[2026-10-26 | 82554 | true 2026-10-25 | 82500 | true 2026-10-18 | 83461 | true 2026-10-26 | 83189 | true]" {
		t.Errorf("weight logs = %v", got)
	}
}

// TestWeightRefused checks invalid weigh-ins and edits are refused and
// change nothing.
func TestWeightRefused(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 8, 0, 0, 0, loc))
	r := logged(t, s, "x",
		"edit_log", editWeight("null", "180", "null", "null"),
		"delete_log", `{"type":"weight","id":null}`,
	)
	if fmt.Sprint(r) != "[error: no weigh-ins logged error: no weigh-ins logged]" {
		t.Errorf("with none logged = %q", r)
	}
	logged(t, s, "180", "log_weight", weightCall("180", "null", "null"))
	id := s.Rows(`SELECT id FROM weight_logs`)[0]
	logged(t, s, "300 cal", "log_intake", intake("300"))

	r = logged(t, s, "x",
		"log_weight", weightCall("49.9", "null", "null"),
		"log_weight", weightCall("1001", "null", "null"),
		"log_weight", weightCall("0", "null", "null"),
		"log_weight", weightCall("-180", "null", "null"),
		"log_weight", weightCall("22", `"kg"`, "null"),
		"log_weight", weightCall("12", `"stone"`, "null"),
		"log_weight", weightCall("180", "null", `"2026-10-27"`),
		"log_weight", weightCall("180", "null", `"2026-09-01"`),
		"log_weight", weightCall("180", "null", `"Oct 3"`),
		"log_weight", `{"weight":null,"unit":null,"date":null}`,
		"edit_log", editWeight("null", "null", "null", "null"),
		"edit_log", editWeight("null", "null", `"kg"`, `"2026-10-25"`),
		"edit_log", editWeight("999999999", "180", "null", "null"),
		"edit_log", editWeight(id, "180", `"cal"`, "null"),
		"edit_log", editWeight(id, "40", "null", "null"),
		"edit_log", editWeight(id, "null", "null", `"2026-10-27"`),
		"edit_log", `{"type":"weight","id":null,"amount":null,"unit":null,"bedtime":"23:00","wake_time":null,"date":"2026-10-25"}`,
		"edit_log", `{"type":"weight","id":null,"amount":null,"unit":null,"bedtime":null,"wake_time":null,"status":"done","date":"2026-10-25"}`,
		"edit_log", `{"type":"calories","id":null,"amount":300,"unit":"lbs","bedtime":null,"wake_time":null,"date":null}`,
		"delete_log", `{"type":"weight","id":999999999}`,
	)
	for _, w := range []string{
		"error: weight must be from 50 lbs to 1000 lbs",
		"error: weight must be from 50 lbs to 1000 lbs",
		"error: weight must be from 50 lbs to 1000 lbs",
		"error: weight must be from 50 lbs to 1000 lbs",
		"error: weight must be from 50 lbs to 1000 lbs",
		"error: stone is not a unit for weight; use lbs or kg",
		"error: date is in the future; today is 2026-10-26",
		"error: date is more than 30 days ago",
		`error: date "Oct 3" is not YYYY-MM-DD`,
		"error: no weight given; ask the user what they weighed",
		"error: no changes given",
		"error: unit given without amount",
		"error: no weigh-in #999999999",
		"error: cal is not a unit for weight; use lbs or kg",
		"error: weight must be from 50 lbs to 1000 lbs",
		"error: date is in the future; today is 2026-10-26",
		"error: bedtime and wake_time are only for sleep",
		"error: status is only for workouts and supplements, and sessions only for workouts",
		"error: lbs is not a unit for calories; use cal",
		"error: no weigh-in #999999999",
	} {
		if len(r) == 0 || r[0] != w {
			t.Errorf("result = %q, want %q", r, w)
			break
		}
		r = r[1:]
	}
	if got := s.Rows(`SELECT local_date::text, grams, deleted_at IS NULL FROM weight_logs ORDER BY id`); fmt.Sprint(got) !=
		"[2026-10-26 | 81647 | true]" {
		t.Errorf("weight logs = %v", got)
	}
	if got := s.Rows(`SELECT amount FROM intake_logs WHERE deleted_at IS NULL`); fmt.Sprint(got) != "[300]" {
		t.Errorf("intake logs = %v", got)
	}
}

// TestWeightMonth logs a month of weigh-ins with gaps, a backfill, two
// weigh-ins on some days, some in kg, and a target that changes partway.
// After every day it checks the day's weight, the 7-day average, the change
// from the 7 days before, and the target against a tally kept here, and at
// the end checks every day of the month again.
func TestWeightMonth(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	day := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, loc) }
	s := sim.New(t, day(1, 7))
	logged(t, s, "my target is 175", "set_goals", `{"goals":[{"metric":"weight","value":175,"unit":null}]}`)

	// Oct d's weigh-in in tenths of a pound: down 0.2 lbs a day, with
	// noise that repeats weekly.
	noise := []int{6, -4, 2, -8, 4, 0, -2}
	weighed := func(d int) int { return 1900 - 2*(d-1) + noise[d%7] }
	gaps := map[int]bool{4: true, 10: true, 11: true, 18: true, 19: true, 25: true}
	kgDays := map[int]bool{7: true, 20: true}
	// Days with a later second weigh-in, by how far it is from the first.
	seconds := map[int]int{13: 15, 22: -12}

	// daily is each day's weight as logged so far: its first weigh-in.
	daily := map[int]int{}
	// logWeigh logs v, in kg rounded to 0.1 if kg, toward Oct d.
	logWeigh := func(d, v int, date string, kg bool) string {
		t.Helper()
		weight, unit := fmt.Sprintf("%d.%d", v/10, v%10), "null"
		if kg {
			k := math.Round(float64(v)*0.45359237) / 10
			weight, unit = fmt.Sprint(k), `"kg"`
			v = int(math.Round(k * 1000 / 453.59237 * 10))
		}
		r := logged(t, s, "weighed "+weight, "log_weight", weightCall(weight, unit, date))
		if _, ok := daily[d]; !ok {
			daily[d] = v
		}
		return r[0]
	}
	// want is the tally's numbers for the week ending on Oct end.
	want := func(end int) string {
		avg := func(from, to int) (int, int) {
			n, sum := 0, 0
			for d := from; d <= to; d++ {
				if v, ok := daily[d]; ok {
					n++
					sum += v
				}
			}
			if n == 0 {
				return 0, 0
			}
			return n, int(math.Round(float64(sum) / float64(n)))
		}
		days, a := avg(end-6, end)
		prevDays, p := avg(end-13, end-7)
		target := 1750
		if end >= 15 {
			target = 1800
		}
		out := fmt.Sprintf("days %d avg %d prev %d %d target %d", days, a, prevDays, p, target)
		if v, ok := daily[end]; ok {
			out = fmt.Sprintf("day %d ", v) + out
		}
		return out
	}
	check := func(end int, what string) {
		t.Helper()
		got, err := s.Agent.WeightNumbers(s.Ctx, s.UserID, day(end, 0))
		if err != nil {
			t.Fatal(err)
		}
		if w := want(end); got != w {
			t.Errorf("%s Oct %d: %s\nwant %s", what, end, got, w)
		}
	}

	for d := 1; d <= 31; d++ {
		s.Set(day(d, 7))
		if d == 15 {
			logged(t, s, "new target 180", "set_goals", `{"goals":[{"metric":"weight","value":180,"unit":"lbs"}]}`)
		}
		if d == 12 {
			// Oct 10 is backfilled; Oct 11 stays a gap.
			r := logWeigh(10, weighed(10), `"2026-10-10"`, false)
			if w := "weight: " + showLbs(daily[10]) + " on Oct 10; "; !strings.Contains(r, w) {
				t.Errorf("backfill = %q, want %q", r, w)
			}
		}
		if !gaps[d] {
			r := logWeigh(d, weighed(d), "null", kgDays[d])
			if w := ". weight: " + showLbs(daily[d]) + " today; "; !strings.Contains(r, w) {
				t.Errorf("Oct %d: result = %q, want %q", d, r, w)
			}
		}
		if diff, ok := seconds[d]; ok {
			s.Set(day(d, 19))
			r := logWeigh(d, weighed(d)+diff, "null", false)
			if w := ". weight: " + showLbs(daily[d]) + " today (the first of 2 weigh-ins that day); "; !strings.Contains(r, w) {
				t.Errorf("Oct %d second: result = %q, want %q", d, r, w)
			}
		}
		check(d, "that day")
	}
	for d := 1; d <= 31; d++ {
		check(d, "at the end")
	}
	if got := s.Rows(`SELECT count(*), count(DISTINCT local_date) FROM weight_logs WHERE deleted_at IS NULL`); fmt.Sprint(got) != "[28 | 26]" {
		t.Errorf("weigh-ins = %v", got)
	}

	// The last day's status, worded.
	s.Script(sim.Say("ok"))
	s.Text("how am I doing")
	reqs := s.Requests()
	if sys := reqs[len(reqs)-1][0].Content; !strings.Contains(sys, "Status: weight: "+showLbs(daily[31])+" today; last 7 days: ") {
		t.Errorf("prompt:\n%s", sys)
	}
}
