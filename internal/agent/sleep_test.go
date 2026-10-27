package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// sleepCall builds log_sleep arguments: hours, bedtime, wake time, and
// date, each JSON or null.
func sleepCall(hours, bed, wake, date string) string {
	return fmt.Sprintf(`{"hours":%s,"bedtime":%s,"wake_time":%s,"date":%s}`, hours, bed, wake, date)
}

// editSleep builds edit_log arguments for a night.
func editSleep(id, amount, bed, wake, date string) string {
	return fmt.Sprintf(`{"type":"sleep","id":%s,"amount":%s,"unit":null,"bedtime":%s,"wake_time":%s,"date":%s}`,
		id, amount, bed, wake, date)
}

// setSleepGoal sets the sleep goal from well before the sim starts, so
// earlier nights are judged against it too.
func setSleepGoal(t *testing.T, s *sim.Sim, minutes int) {
	t.Helper()
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE goals SET value = $2, start_date = '2026-10-01' WHERE user_id = $1 AND metric = 'sleep'`,
		s.UserID, minutes); err != nil {
		t.Fatal(err)
	}
}

// TestSleep logs nights by hours and by bed and wake times, edits, moves,
// and deletes them, undoes every change, and checks rows and statuses.
func TestSleep(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 8, 0, 0, 0, loc))
	setSleepGoal(t, s, 480)

	r := logged(t, s, "slept 7 hours", "log_sleep", sleepCall("7", "null", "null", "null"))
	last := s.Rows(`SELECT max(id) FROM sleep_logs`)[0]
	want := "logged #" + last + ": 7 hours of sleep last night. sleep: 7 hours / 8 hours last night, 1 hour short; " +
		"last 7 nights: 7 hours over 1 logged, 0 of 1 on goal."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("log hours = %q\nwant %q", r, want)
	}
	// A retried webhook and a rerun of the same text log nothing more.
	s.Retry()
	s.Script(sim.Call("log_sleep", sleepCall("7", "null", "null", "null")), sim.Say("ok"))
	s.Rerun()
	if got := lastResults(s); fmt.Sprint(got) != "["+want+"]" {
		t.Errorf("rerun = %q", got)
	}

	// Times across midnight give the hours, toward the day the user woke.
	r = logged(t, s, "saturday night bed at 12, up at 7:30", "log_sleep", sleepCall("null", `"00:00"`, `"07:30"`, `"2026-10-25"`))
	sunday := s.Rows(`SELECT max(id) FROM sleep_logs`)[0]
	want = "logged #" + sunday + ": 7 hours 30 min of sleep (12:00 AM to 7:30 AM) the night before last. " +
		"sleep: 7 hours 30 min / 8 hours the night before last, 30 min short; " +
		"last 7 nights: 7 hours 15 min average over 2 logged, 0 of 2 on goal."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("log times = %q\nwant %q", r, want)
	}
	r = logged(t, s, "friday 11:30pm to 7, slept 7 hours", "log_sleep", sleepCall("7", `"23:30"`, `"07:00"`, `"2026-10-24"`))
	saturday := s.Rows(`SELECT max(id) FROM sleep_logs`)[0]
	if len(r) != 1 || !strings.HasPrefix(r[0], "logged #"+saturday+": 7 hours of sleep (11:30 PM to 7:00 AM) the night before Oct 24. ") {
		t.Errorf("log hours and times = %q", r)
	}

	// The prompt lists the week's nights, newest first, with the status.
	s.Script(sim.Say("ok"))
	s.Text("how am I sleeping")
	reqs := s.Requests()
	sys := reqs[len(reqs)-1][0].Content
	for _, w := range []string{
		"#" + last + " Mon Oct 26 (last night): 7 hours of sleep\n#" + sunday +
			" Sun Oct 25 (the night before last): 7 hours 30 min of sleep (12:00 AM to 7:30 AM)\n#" + saturday +
			" Sat Oct 24 (the night before Oct 24): 7 hours of sleep (11:30 PM to 7:00 AM)\n",
		"Status: sleep: 7 hours / 8 hours last night, 1 hour short; last 7 nights: 7 hours 10 min average over 3 logged, 0 of 3 on goal.\n",
	} {
		if !strings.Contains(sys, w) {
			t.Errorf("prompt lacks %q:\n%s", w, sys)
		}
	}

	// A wake time change works out the hours again from both times.
	r = logged(t, s, "actually I got up at 8 sunday", "edit_log", editSleep(sunday, "null", "null", `"08:00"`, "null"))
	want = "updated #" + sunday + ": now 8 hours of sleep (12:00 AM to 8:00 AM) the night before last. " +
		"sleep: 8 hours / 8 hours the night before last, goal hit; last 7 nights: 7 hours 20 min average over 3 logged, 1 of 3 on goal."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("edit wake time = %q\nwant %q", r, want)
	}
	r = logged(t, s, "last night was 6.5", "edit_log", editSleep("null", "6.5", "null", "null", "null"))
	if len(r) != 1 || !strings.HasPrefix(r[0], "updated #"+last+": now 6 hours 30 min of sleep last night. "+
		"sleep: 6 hours 30 min / 8 hours last night, 1 hour 30 min short; ") {
		t.Errorf("edit hours = %q", r)
	}
	r = logged(t, s, "the friday one was thursday", "edit_log", editSleep(saturday, "null", "null", "null", `"2026-10-23"`))
	want = "updated #" + saturday + ": now 7 hours of sleep (11:30 PM to 7:00 AM) the night before Oct 23. " +
		"sleep: 7 hours / 8 hours the night before Oct 23, 1 hour short; sleep: nothing logged for the night before Oct 24; " +
		"last 7 nights: 7 hours 10 min average over 3 logged, 1 of 3 on goal."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("move = %q\nwant %q", r, want)
	}
	r = logged(t, s, "delete last night", "delete_log", `{"type":"sleep","id":null}`)
	want = "deleted #" + last + ": 6 hours 30 min of sleep last night. sleep: nothing logged for last night; " +
		"last 7 nights: 7 hours 30 min average over 2 logged, 1 of 2 on goal."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("delete = %q\nwant %q", r, want)
	}

	// Each undo reverses one change, newest first, without the LLM.
	llmCalls := len(s.Requests())
	for _, w := range []string{
		"Undid deleting 6 hours 30 min of sleep for the night before Oct 26. Sleep: 6 hours 30 min / 8 hours last night, " +
			"1 hour 30 min short. Last 7 nights: 7 hours 10 min average over 3 logged, 1 of 3 on goal.",
		"Undid moving 7 hours of sleep (11:30 PM to 7:00 AM) from the night before Oct 24 to the night before Oct 23. " +
			"Sleep: 7 hours / 8 hours the night before Oct 24, 1 hour short. Last 7 nights: 7 hours 10 min average over 3 logged, 1 of 3 on goal.",
		"Undid changing 7 hours of sleep to 6 hours 30 min of sleep for the night before Oct 26. Sleep: 7 hours / 8 hours last night, " +
			"1 hour short. Last 7 nights: 7 hours 20 min average over 3 logged, 1 of 3 on goal.",
		"Undid changing 7 hours 30 min of sleep (12:00 AM to 7:30 AM) to 8 hours of sleep (12:00 AM to 8:00 AM) for the night before Oct 25. " +
			"Sleep: 7 hours 30 min / 8 hours the night before last, 30 min short. Last 7 nights: 7 hours 10 min average over 3 logged, 0 of 3 on goal.",
		"Undid logging 7 hours of sleep (11:30 PM to 7:00 AM) for the night before Oct 24. Sleep: nothing logged for the night before Oct 24. " +
			"Last 7 nights: 7 hours 15 min average over 2 logged, 0 of 2 on goal.",
		"Undid logging 7 hours 30 min of sleep (12:00 AM to 7:30 AM) for the night before Oct 25. Sleep: nothing logged for the night before last. " +
			"Last 7 nights: 7 hours over 1 logged, 0 of 1 on goal.",
		"Undid logging 7 hours of sleep for the night before Oct 26. Sleep: nothing logged for last night. Last 7 nights: none logged.",
	} {
		if reply := s.Text("undo"); reply != w {
			t.Errorf("undo = %q\nwant %q", reply, w)
		}
	}
	if n := len(s.Requests()); n != llmCalls {
		t.Errorf("undo made %d LLM calls", n-llmCalls)
	}
	if got := s.Rows(`SELECT local_date::text, minutes, bedtime::text, wake_time::text, deleted_at IS NOT NULL FROM sleep_logs ORDER BY id`); fmt.Sprint(got) !=
		"[2026-10-26 | 420 | <nil> | <nil> | true 2026-10-25 | 450 | 00:00:00 | 07:30:00 | true 2026-10-24 | 420 | 23:30:00 | 07:00:00 | true]" {
		t.Errorf("sleep logs = %v", got)
	}
}

// TestSleepRefused checks invalid nights are refused and change nothing.
func TestSleepRefused(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 8, 0, 0, 0, loc))
	logged(t, s, "slept 7", "log_sleep", sleepCall("7", "null", "null", "null"))
	logged(t, s, "and 6 the night before", "log_sleep", sleepCall("6", "null", "null", `"2026-10-25"`))
	id := s.Rows(`SELECT min(id) FROM sleep_logs`)[0]
	other := s.Rows(`SELECT max(id) FROM sleep_logs`)[0]
	calorie := logged(t, s, "300 cal", "log_intake", intake("300"))
	if len(calorie) != 1 || !strings.HasPrefix(calorie[0], "logged") {
		t.Fatalf("log calories = %q", calorie)
	}

	r := logged(t, s, "x",
		"log_sleep", sleepCall("8", "null", "null", "null"),
		"log_sleep", sleepCall("0", "null", "null", `"2026-10-24"`),
		"log_sleep", sleepCall("20", "null", "null", `"2026-10-24"`),
		"log_sleep", sleepCall("null", `"23:00"`, "null", `"2026-10-24"`),
		"log_sleep", sleepCall("null", "null", "null", `"2026-10-24"`),
		"log_sleep", sleepCall("null", `"25:00"`, `"07:00"`, `"2026-10-24"`),
		"log_sleep", sleepCall("null", `"11pm"`, `"07:00"`, `"2026-10-24"`),
		"log_sleep", sleepCall("null", `"07:00"`, `"07:00"`, `"2026-10-24"`),
		"log_sleep", sleepCall("null", `"06:00"`, `"23:30"`, `"2026-10-24"`),
		"log_sleep", sleepCall("9", `"00:00"`, `"07:30"`, `"2026-10-24"`),
		"log_sleep", sleepCall("7", `"23:00"`, `"22:00"`, `"2026-10-24"`),
		"log_sleep", sleepCall("7", "null", "null", `"2026-10-27"`),
		"log_sleep", sleepCall("7", "null", "null", `"2026-09-01"`),
		"edit_log", editSleep("null", "null", "null", "null", "null"),
		"edit_log", editSleep(id, "null", "null", "null", `"2026-10-25"`),
		"edit_log", editSleep(id, "null", `"07:00"`, `"06:00"`, "null"),
		"edit_log", editSleep("999999999", "7", "null", "null", "null"),
		"edit_log", `{"type":"sleep","id":null,"amount":7,"unit":"cal","bedtime":null,"wake_time":null,"date":null}`,
		"edit_log", `{"type":"sleep","id":null,"amount":null,"unit":"hours","bedtime":null,"wake_time":null,"date":"2026-10-26"}`,
		"edit_log", `{"type":"calories","id":null,"amount":null,"unit":null,"bedtime":"23:00","wake_time":null,"date":null}`,
		"delete_log", `{"type":"sleep","id":999999999}`,
	)
	for _, w := range []string{
		"error: 7 hours of sleep is already logged for last night as #" + id + "; change it with edit_log instead",
		"error: sleep must be more than 0",
		"error: sleep must be at most 16 hours a night",
		"error: give the hours slept, or both bedtime and wake time",
		"error: give the hours slept, or both bedtime and wake time",
		`error: bedtime "25:00" is not a 24-hour time like 23:30`,
		`error: bedtime "11pm" is not a 24-hour time like 23:30`,
		"error: 7:00 AM to 7:00 AM is 24 hours, more than 16 hours; check the bedtime and wake time are the right AM or PM",
		"error: 6:00 AM to 11:30 PM is 17 hours 30 min, more than 16 hours; check the bedtime and wake time are the right AM or PM",
		"error: 9 hours is more than the 7 hours 30 min from 12:00 AM to 7:30 AM",
		"error: 11:00 PM to 10:00 PM is 23 hours, more than 16 hours; check the bedtime and wake time are the right AM or PM",
		"error: date is in the future; today is 2026-10-26",
		"error: date is more than 30 days ago",
		"error: no changes given",
		"error: 6 hours of sleep is already logged for the night before last as #" + other + "; change it with edit_log instead",
		"error: 7:00 AM to 6:00 AM is 23 hours, more than 16 hours; check the bedtime and wake time are the right AM or PM",
		"error: no night #999999999",
		"error: cal is not a unit for sleep; use hours",
		"error: unit given without amount",
		"error: bedtime and wake_time are only for sleep",
		"error: no night #999999999",
	} {
		if len(r) == 0 || r[0] != w {
			t.Errorf("result = %q, want %q", r, w)
			break
		}
		r = r[1:]
	}
	if got := s.Rows(`SELECT local_date::text, minutes, deleted_at IS NULL FROM sleep_logs ORDER BY id`); fmt.Sprint(got) !=
		"[2026-10-26 | 420 | true 2026-10-25 | 360 | true]" {
		t.Errorf("sleep logs = %v", got)
	}
	if got := s.Rows(`SELECT count(*) FROM sleep_logs`); fmt.Sprint(got) != "[2]" {
		t.Errorf("sleep rows = %v", got)
	}
}

// TestSleepWeek logs a run of nights against a goal that changes partway,
// and checks each night's status and the last week's average and nights on
// goal after every log, then as the week moves on a day.
func TestSleepWeek(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 9, 0, 0, 0, loc))
	for _, q := range []string{
		`DELETE FROM goals WHERE user_id = $1 AND metric = 'sleep'`,
		`INSERT INTO goals (user_id, metric, value, start_date) VALUES ($1, 'sleep', 420, '2026-10-01'), ($1, 'sleep', 480, '2026-10-24')`,
	} {
		if _, err := s.Pool.Exec(s.Ctx, q, s.UserID); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct{ date, hours, night, week string }{
		// Oct 18 is before the week, so it shows but doesn't count.
		{"2026-10-18", "6", "6 hours / 7 hours the night before Oct 18, 1 hour short", "none logged"},
		{"2026-10-20", "7", "7 hours / 7 hours the night before Oct 20, goal hit", "7 hours over 1 logged, 1 of 1 on goal"},
		{"2026-10-21", "6.5", "6 hours 30 min / 7 hours the night before Oct 21, 30 min short",
			"6 hours 45 min average over 2 logged, 1 of 2 on goal"},
		{"2026-10-23", "7.25", "7 hours 15 min / 7 hours the night before Oct 23, goal hit",
			"6 hours 55 min average over 3 logged, 2 of 3 on goal"},
		// The 8 hour goal starts Oct 24.
		{"2026-10-24", "7.5", "7 hours 30 min / 8 hours the night before Oct 24, 30 min short",
			"7 hours 4 min average over 4 logged, 2 of 4 on goal"},
		{"2026-10-25", "8", "8 hours / 8 hours the night before last, goal hit", "7 hours 15 min average over 5 logged, 3 of 5 on goal"},
		{"null", "9", "9 hours / 8 hours last night, goal hit", "7 hours 33 min average over 6 logged, 4 of 6 on goal"},
	} {
		date := c.date
		if date != "null" {
			date = `"` + date + `"`
		}
		r := logged(t, s, "slept "+c.hours, "log_sleep", sleepCall(c.hours, "null", "null", date))
		want := ". sleep: " + c.night + "; last 7 nights: " + c.week + "."
		if len(r) != 1 || !strings.Contains(r[0], want) {
			t.Errorf("%s: result = %q\nwant %q", c.date, r, want)
		}
	}

	// A night with no goal in force counts toward the average, not the
	// nights on goal.
	logged(t, s, "stop tracking sleep", "set_goals", `{"goals":[{"metric":"sleep","value":null,"unit":null}]}`)
	s.Script(sim.Say("ok"))
	s.Text("how's my sleep")
	reqs := s.Requests()
	if sys := reqs[len(reqs)-1][0].Content; !strings.Contains(sys,
		"Status: sleep: 9 hours last night; last 7 nights: 7 hours 33 min average over 6 logged, 3 of 5 on goal.\n") {
		t.Errorf("prompt after stopping:\n%s", sys)
	}

	// A day later Oct 20 leaves the week, and nothing is logged for last
	// night yet.
	s.Set(time.Date(2026, 10, 27, 9, 0, 0, 0, loc))
	s.Script(sim.Say("ok"))
	s.Text("and now")
	reqs = s.Requests()
	if sys := reqs[len(reqs)-1][0].Content; !strings.Contains(sys,
		"Status: sleep: nothing logged for last night; last 7 nights: 7 hours 39 min average over 5 logged, 2 of 4 on goal.\n") {
		t.Errorf("prompt a day later:\n%s", sys)
	}
}

// TestSleepWakeDate checks a night logged the next morning counts toward
// that morning's local date, even when it is still the day before in UTC,
// including as an answer to the morning check-in.
func TestSleepWakeDate(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	// 8:00 AM Tuesday in Tokyo is still Monday in UTC.
	s := sim.New(t, time.Date(2026, 10, 27, 8, 0, 0, 0, tokyo))
	s.Tick()
	if got := s.WaitSends(1); got[0] != "Good morning, Sam. How did you sleep?" {
		t.Fatalf("check-in = %q", got[0])
	}
	s.Script(sim.Call("log_sleep", sleepCall("null", `"23:15"`, `"07:45"`, "null")), sim.Say("Logged 8 hours 30 min."))
	if reply := s.Text("bed 11:15, up 7:45"); reply != "Logged 8 hours 30 min." {
		t.Errorf("reply = %q", reply)
	}
	reqs := s.Requests()
	if sys := reqs[len(reqs)-1][0].Content; !strings.Contains(sys, " Tue Oct 27 (last night): 8 hours 30 min of sleep (11:15 PM to 7:45 AM)\n") {
		t.Errorf("prompt:\n%s", sys)
	}
	if got := s.Rows(`SELECT local_date::text, minutes FROM sleep_logs`); fmt.Sprint(got) != "[2026-10-27 | 510]" {
		t.Errorf("sleep logs = %v", got)
	}
}
