package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// supplementCall builds set_supplement arguments: action, then name, dose,
// time, and days, each JSON or null.
func supplementCall(action, name, dose, at, days string) string {
	return fmt.Sprintf(`{"action":"%s","name":%s,"dose":%s,"time":%s,"days":%s}`, action, name, dose, at, days)
}

// takeCall builds log_supplement arguments: names, status, and date.
func takeCall(names, status, date string) string {
	return fmt.Sprintf(`{"names":%s,"status":"%s","date":%s}`, names, status, date)
}

// editTake builds edit_log arguments for a supplement log.
func editTake(id, status, date string) string {
	return fmt.Sprintf(`{"type":"supplement","id":%s,"amount":null,"unit":null,"bedtime":null,"wake_time":null,`+
		`"status":%s,"sessions":null,"date":%s}`, id, status, date)
}

func lastTake(s *sim.Sim) string {
	return s.Rows(`SELECT max(id) FROM supplement_logs`)[0]
}

// TestSupplements adds, changes, and removes supplements, logs them over
// four days with one missed, edits, backfills, and deletes logs, checks the
// prompt, and undoes the changes.
func TestSupplements(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	at := func(day, h int) time.Time { return time.Date(2026, 10, day, h, 0, 0, 0, loc) }
	// Monday; wake time 8:00 AM, bedtime 10:00 PM.
	s := sim.New(t, at(26, 8))

	r := logged(t, s, "5g creatine every night", "set_supplement", supplementCall("set", `"Creatine"`, `"5 g"`, `"night"`, "null"))
	want := "saved from today: creatine 5 g, every day, at night (10:00 PM). supplements today: creatine not logged yet; " +
		"last 7 days: 0 of 1 due dose taken, 1 still due today."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("add = %q\nwant %q", r, want)
	}
	// A retried webhook and a rerun of the same text add nothing more.
	s.Retry()
	s.Script(sim.Call("set_supplement", supplementCall("set", `"Creatine"`, `"5 g"`, `"night"`, "null")), sim.Say("ok"))
	s.Rerun()
	if got := lastResults(s); fmt.Sprint(got) != "["+want+"]" {
		t.Errorf("rerun = %q", got)
	}
	r = logged(t, s, "fish oil with breakfast mon wed fri", "set_supplement",
		supplementCall("set", `"fish oil"`, "null", `"morning"`, `["fri","mon","wed"]`))
	want = "saved from today: fish oil, Mon/Wed/Fri, in the morning (8:00 AM). supplements today: creatine not logged yet, " +
		"fish oil not logged yet; last 7 days: 0 of 2 due doses taken, 2 still due today."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("add days = %q\nwant %q", r, want)
	}
	logged(t, s, "and zinc", "set_supplement", supplementCall("set", `"zinc"`, "null", "null", "null"))

	r = logged(t, s, "took my supplements", "log_supplement", takeCall("null", "taken", "null"))
	ids := s.Rows(`SELECT id FROM supplement_logs ORDER BY id`)
	want = fmt.Sprintf("logged creatine taken (#%s), fish oil taken (#%s), zinc taken (#%s) today. supplements today: "+
		"creatine taken, fish oil taken, zinc taken; last 7 days: 3 of 3 due doses taken.", ids[0], ids[1], ids[2])
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("log all = %q\nwant %q", r, want)
	}

	// Tuesday: creatine drops to 3 g. Fish oil isn't due, but is taken.
	s.Set(at(27, 9))
	r = logged(t, s, "creatine is 3g now", "set_supplement", supplementCall("set", `"creatine"`, `"3 g"`, `"night"`, "null"))
	want = "saved from today: creatine 3 g, every day, at night (10:00 PM) (was creatine 5 g, every day, at night (10:00 PM)). " +
		"supplements today: creatine not logged yet, zinc not logged yet; last 7 days: 3 of 5 due doses taken, 2 still due today."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("change = %q\nwant %q", r, want)
	}
	logged(t, s, "took creatine, skipped zinc, had fish oil too", "log_supplement", takeCall(`["creatine"]`, "taken", "null"),
		"log_supplement", takeCall(`["zinc"]`, "skipped", "null"), "log_supplement", takeCall(`["fish oil"]`, "taken", "null"))
	zinc := s.Rows(`SELECT l.id FROM supplement_logs l JOIN supplements s ON s.id = l.supplement_id WHERE s.name = 'zinc' AND l.local_date = '2026-10-27'`)[0]

	// Wednesday: zinc stops, and nothing is logged.
	s.Set(at(28, 9))
	r = logged(t, s, "stopped taking zinc", "set_supplement", supplementCall("remove", `"zinc"`, "null", "null", "null"))
	if len(r) != 1 || !strings.HasPrefix(r[0], "removed zinc from today. supplements today: creatine not logged yet, fish oil not logged yet; ") {
		t.Errorf("remove = %q", r)
	}

	// Thursday: Wednesday's doses were missed, and creatine is backfilled.
	s.Set(at(29, 9))
	r = logged(t, s, "took creatine last night", "log_supplement", takeCall(`["creatine"]`, "taken", `"2026-10-28"`))
	backfill := lastTake(s)
	want = "logged creatine taken (#" + backfill + ") yesterday. supplements yesterday: creatine taken, fish oil missed; " +
		"last 7 days: 5 of 8 due doses taken, 1 skipped, 1 missed, 1 still due today."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("backfill = %q\nwant %q", r, want)
	}
	r = logged(t, s, "actually I did take zinc tuesday", "edit_log", editTake(zinc, `"taken"`, "null"))
	want = "updated #" + zinc + ": now zinc taken on Oct 27. supplements on Oct 27: creatine taken, zinc taken, fish oil taken (not due); " +
		"last 7 days: 6 of 8 due doses taken, 1 missed, 1 still due today."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("edit = %q\nwant %q", r, want)
	}

	// The prompt lists today's plan, the week's logs, and the status.
	s.Script(sim.Say("ok"))
	s.Text("supplements?")
	reqs := s.Requests()
	sys := reqs[len(reqs)-1][0].Content
	for _, w := range []string{
		"On the list today:\ncreatine 3 g, every day, at night (10:00 PM)\nfish oil, Mon/Wed/Fri, in the morning (8:00 AM)\n",
		"#" + backfill + " Wed Oct 28: creatine taken\n",
		"#" + ids[0] + " Mon Oct 26: creatine taken\n",
		"Status: supplements today: creatine not logged yet; last 7 days: 6 of 8 due doses taken, 1 missed, 1 still due today.\n",
	} {
		if !strings.Contains(sys, w) {
			t.Errorf("prompt lacks %q:\n%s", w, sys)
		}
	}

	// A null id is the latest day's log.
	r = logged(t, s, "delete that", "delete_log", `{"type":"supplement","id":null}`)
	want = "deleted #" + backfill + ": creatine taken yesterday. supplements yesterday: creatine missed, fish oil missed; " +
		"last 7 days: 5 of 8 due doses taken, 2 missed, 1 still due today."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("delete = %q\nwant %q", r, want)
	}

	llmCalls := len(s.Requests())
	for _, w := range []string{
		"Undid deleting creatine taken on Wed Oct 28. Supplements yesterday: creatine taken, fish oil missed. " +
			"Last 7 days: 6 of 8 due doses taken, 1 missed, 1 still due today.",
		"Undid changing zinc skipped to zinc taken on Tue Oct 27. Supplements on Oct 27: creatine taken, zinc skipped, " +
			"fish oil taken (not due). Last 7 days: 5 of 8 due doses taken, 1 skipped, 1 missed, 1 still due today.",
		"Undid logging creatine taken on Wed Oct 28. Supplements yesterday: creatine missed, fish oil missed. " +
			"Last 7 days: 4 of 8 due doses taken, 1 skipped, 2 missed, 1 still due today.",
		"Undid removing zinc, every day, any time. Supplements: creatine 3 g, every day, at night (10:00 PM); " +
			"fish oil, Mon/Wed/Fri, in the morning (8:00 AM); zinc, every day, any time.",
	} {
		if reply := s.Text("undo"); reply != w {
			t.Errorf("undo = %q\nwant %q", reply, w)
		}
	}
	if n := len(s.Requests()); n != llmCalls {
		t.Errorf("undo made %d LLM calls", n-llmCalls)
	}
	// Restoring zinc brings back Wednesday's dose as missed.
	r = logged(t, s, "how about yesterday", "log_supplement", takeCall(`["zinc"]`, "skipped", `"2026-10-28"`))
	if len(r) != 1 || !strings.Contains(r[0], "supplements yesterday: creatine missed, fish oil missed, zinc skipped; ") {
		t.Errorf("zinc after undo = %q", r)
	}
	if got := s.Rows(`SELECT s.name, p.start_date::text, p.active, p.dose FROM supplement_plans p JOIN supplements s ON s.id = p.supplement_id
		ORDER BY s.name, p.start_date`); fmt.Sprint(got) !=
		"[creatine | 2026-10-26 | true | 5 g creatine | 2026-10-27 | true | 3 g fish oil | 2026-10-26 | true | <nil> zinc | 2026-10-26 | true | <nil>]" {
		t.Errorf("plans = %v", got)
	}
}

// TestSupplementSameDay changes a supplement twice in a day, which keeps
// one row for the day, and checks undo puts back the first change.
func TestSupplementSameDay(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 8, 0, 0, 0, loc))
	logged(t, s, "creatine at 9pm", "set_supplement", supplementCall("set", `"creatine"`, "null", `"21:00"`, "null"))
	logged(t, s, "make that weekdays", "set_supplement", supplementCall("set", `"creatine"`, "null", `"21:00"`, `["mon","tue","wed","thu","fri"]`))
	logged(t, s, "every day after all", "set_supplement",
		supplementCall("set", `"creatine"`, "null", `"21:00"`, `["mon","tue","wed","thu","fri","sat","sun"]`))
	if got := s.Rows(`SELECT start_date::text, take_at::text, days IS NULL FROM supplement_plans`); fmt.Sprint(got) != "[2026-10-26 | 21:00:00 | true]" {
		t.Errorf("plans = %v", got)
	}
	for _, w := range []string{
		"Undid changing creatine, Mon/Tue/Wed/Thu/Fri, at 9:00 PM to creatine, every day, at 9:00 PM. " +
			"Supplements: creatine, Mon/Tue/Wed/Thu/Fri, at 9:00 PM.",
		"Undid changing creatine, every day, at 9:00 PM to creatine, Mon/Tue/Wed/Thu/Fri, at 9:00 PM. Supplements: creatine, every day, at 9:00 PM.",
		"Undid adding creatine, every day, at 9:00 PM. Supplements: none.",
	} {
		if reply := s.Text("undo"); reply != w {
			t.Errorf("undo = %q\nwant %q", reply, w)
		}
	}
	if got := s.Rows(`SELECT count(*) FROM supplement_plans`); fmt.Sprint(got) != "[0]" {
		t.Errorf("plans after undo = %v", got)
	}
}

// TestSupplementSetup answers setup's supplement question with "none",
// which ends setup, and checks undo asks it again.
func TestSupplementSetup(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 8, 0, 0, 0, loc))
	// A user who set up before supplements were asked: their first text
	// came before, so this one isn't answered with the intro.
	s.Script(sim.Say("Hey."))
	s.Text("hey")
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE users SET supplements_answered = false`); err != nil {
		t.Fatal(err)
	}
	question := `Do you take any supplements? Tell me what and when, like "creatine 5 g every night", or say "none".`
	s.Script(sim.Say("Hi."))
	if reply := s.Text("hi"); reply != "Hi. "+question {
		t.Errorf("reply = %q", reply)
	}
	s.Script(sim.Call("set_supplement", supplementCall("none", "null", "null", "null", "null")), sim.Say("Got it."))
	if reply := s.Text("none"); !strings.HasPrefix(reply, "Got it. You're all set.") {
		t.Errorf("reply = %q", reply)
	}
	if got := lastResults(s); fmt.Sprint(got) != "[saved: the user takes no supplements.]" {
		t.Errorf("none = %q", got)
	}
	s.Script(sim.Say("ok"))
	s.Text("thanks")
	reqs := s.Requests()
	if sys := reqs[len(reqs)-1][0].Content; !strings.Contains(sys, "On the list today:\n(none; the user takes no supplements)\n") {
		t.Errorf("prompt lacks the answer:\n%s", sys)
	}
	if reply := s.Text("undo"); reply != "Undid saving that you take no supplements. Supplements: none. "+question {
		t.Errorf("undo = %q", reply)
	}
}

// TestSupplementRefused checks invalid supplement changes and logs are
// refused and change nothing.
func TestSupplementRefused(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 8, 0, 0, 0, loc))
	check := func(cases []struct{ tool, args, want string }) {
		t.Helper()
		for _, c := range cases {
			if r := logged(t, s, "supplement", c.tool, c.args); fmt.Sprint(r) != "["+c.want+"]" {
				t.Errorf("%s %s = %q\nwant %q", c.tool, c.args, r, c.want)
			}
		}
	}
	check([]struct{ tool, args, want string }{
		{"delete_log", `{"type":"supplement","id":null}`, "error: no supplement logged"},
		{"set_supplement", supplementCall("add", `"zinc"`, "null", "null", "null"), `error: action "add" must be set, remove, or none`},
		{"set_supplement", supplementCall("set", "null", "null", "null", "null"), "error: set needs the supplement's name"},
		{"set_supplement", supplementCall("set", `"  "`, "null", "null", "null"), "error: a supplement name is empty"},
		{"set_supplement", supplementCall("set", `"`+strings.Repeat("x", 41)+`"`, "null", "null", "null"),
			`error: supplement name "` + strings.Repeat("x", 41) + `" is longer than 40 characters`},
		{"set_supplement", supplementCall("set", `"zinc"`, `" "`, "null", "null"), "error: dose is empty; pass null if the user gave none"},
		{"set_supplement", supplementCall("set", `"zinc"`, "null", `"9pm"`, "null"),
			`error: time "9pm" must be a 24-hour time like 21:00, morning, or night, or null for any time`},
		{"set_supplement", supplementCall("set", `"zinc"`, "null", "null", `["monday"]`),
			`error: day "monday" must be one of sun, mon, tue, wed, thu, fri, sat`},
		{"set_supplement", supplementCall("set", `"zinc"`, "null", "null", `["mon","mon"]`), "error: mon is listed twice"},
		{"set_supplement", supplementCall("remove", `"zinc"`, "null", "null", "null"), "error: zinc isn't on the list; the list has nothing"},
		{"set_supplement", supplementCall("none", `"zinc"`, "null", "null", "null"), "error: none takes no name, dose, time, or days"},
	})
	logged(t, s, "creatine", "set_supplement", supplementCall("set", `"creatine"`, `"5 g"`, `"night"`, "null"))
	logged(t, s, "fish oil tuesdays", "set_supplement", supplementCall("set", `"fish oil"`, "null", "null", `["tue"]`))
	logged(t, s, "took it", "log_supplement", takeCall(`["creatine"]`, "taken", "null"))
	taken := lastTake(s)
	check([]struct{ tool, args, want string }{
		{"set_supplement", supplementCall("none", "null", "null", "null", "null"),
			"error: the list has creatine and fish oil; remove each with action remove instead"},
		{"set_supplement", supplementCall("set", `"creatine"`, `"5 g"`, `"night"`, "null"),
			"error: creatine is already saved as creatine 5 g, every day, at night (10:00 PM); nothing changed"},
		{"set_supplement", supplementCall("remove", `"creatine"`, `"5 g"`, "null", "null"), "error: remove takes only the name"},
		{"log_supplement", takeCall("null", "took", "null"), `error: status "took" must be taken or skipped`},
		{"log_supplement", takeCall(`["zinc"]`, "taken", "null"), "error: zinc isn't on the list for Mon Oct 26; it has creatine and fish oil"},
		{"log_supplement", takeCall(`["fish oil","Fish  Oil"]`, "taken", "null"), "error: fish oil is listed twice"},
		{"log_supplement", takeCall("null", "taken", `"2026-10-27"`), "error: date is in the future; today is 2026-10-26"},
		{"log_supplement", takeCall("null", "taken", `"2026-09-01"`), "error: date is more than 30 days ago"},
		{"log_supplement", takeCall("null", "taken", `"2026-10-25"`), "error: no supplements are on the list for Sun Oct 25"},
		{"log_supplement", takeCall("null", "taken", "null"),
			"error: everything due on Mon Oct 26 is already logged; change a log with edit_log"},
		{"log_supplement", takeCall(`["creatine"]`, "skipped", "null"),
			"error: creatine on Mon Oct 26 is already logged as #" + taken + " (creatine taken); change it with edit_log instead"},
		{"log_supplement", takeCall(`["fish oil"]`, "skipped", "null"), "error: fish oil isn't due on Mon Oct 26 (Tue), so it can't be skipped"},
		{"edit_log", editTake("null", "null", "null"), "error: no changes given"},
		{"edit_log", `{"type":"supplement","id":null,"amount":1,"unit":null,"bedtime":null,"wake_time":null,"status":null,"sessions":null,"date":null}`,
			"error: a supplement log has only a status and a date"},
		{"edit_log", editTake("null", `"done"`, "null"), `error: status "done" must be taken or skipped`},
		{"edit_log", editTake("null", "null", `"2026-10-25"`), "error: creatine isn't on the list for Sun Oct 25"},
		{"edit_log", editTake("999999", `"taken"`, "null"), "error: no supplement log #999999"},
		{"delete_log", `{"type":"supplement","id":999999}`, "error: no supplement log #999999"},
	})
	// taken is a supplement status, not a workout's.
	logged(t, s, "worked out", "log_workout", workoutCall("done", "null", "null"))
	check([]struct{ tool, args, want string }{
		{"edit_log", editWorkout("null", `"taken"`, "null", "null"), `error: status "taken" must be done or skipped`},
	})

	// The list holds at most 20 at once.
	var calls []string
	for i := range 18 {
		calls = append(calls, "set_supplement", supplementCall("set", fmt.Sprintf(`"extra %d"`, i), "null", "null", "null"))
	}
	calls = append(calls, "set_supplement", supplementCall("set", `"one too many"`, "null", "null", "null"))
	if r := logged(t, s, "lots", calls...); r[len(r)-1] != "error: the list is full (20 supplements); remove one first" {
		t.Errorf("full list = %q", r[len(r)-1])
	}

	// With creatine removed, nothing on the list is due today.
	s.Set(time.Date(2026, 10, 26, 9, 0, 0, 0, loc))
	logged(t, s, "stop creatine", "set_supplement", supplementCall("remove", `"creatine"`, "null", "null", "null"))
	var removes []string
	for i := range 18 {
		removes = append(removes, "set_supplement", supplementCall("remove", fmt.Sprintf(`"extra %d"`, i), "null", "null", "null"))
	}
	logged(t, s, "stop the rest", removes...)
	check([]struct{ tool, args, want string }{
		{"log_supplement", takeCall("null", "taken", "null"), "error: nothing on the list is due on Mon Oct 26; name a supplement to log it anyway"},
	})
	if got := s.Rows(`SELECT count(*) FROM supplement_logs`); fmt.Sprint(got) != "[1]" {
		t.Errorf("logs = %v", got)
	}
}
