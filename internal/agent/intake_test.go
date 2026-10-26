package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// lastResults returns the tool results the LLM was sent in its latest
// request.
func lastResults(s *sim.Sim) []string {
	reqs := s.Requests()
	return toolResults(reqs[len(reqs)-1])
}

// logged runs one text whose tool calls are calls, and returns the results.
func logged(t *testing.T, s *sim.Sim, body string, calls ...string) []string {
	t.Helper()
	s.Script(sim.Call(calls...), sim.Say("ok"))
	s.Text(body)
	return lastResults(s)
}

// intake builds log_intake arguments: calories, and any other fields as
// "name":value pairs.
func intake(cal string, fields ...string) string {
	args := map[string]string{"calories": cal, "protein": "null", "carbs": "null", "fat": "null", "date": "null"}
	for i := 0; i < len(fields); i += 2 {
		args[fields[i]] = fields[i+1]
	}
	return fmt.Sprintf(`{"calories":%s,"protein":%s,"carbs":%s,"fat":%s,"date":%s}`,
		args["calories"], args["protein"], args["carbs"], args["fat"], args["date"])
}

// TestIntakeDay logs, edits, and deletes a day of calories and protein,
// undoes every change one at a time, and checks the totals and rows at each
// step.
func TestIntakeDay(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	at := func(h int) time.Time { return time.Date(2026, 10, 26, h, 0, 0, 0, loc) }
	s := sim.New(t, at(9))

	logged(t, s, "1800 cal and 150g protein a day",
		"set_goals", `{"goals":[{"metric":"calories","value":1800,"unit":null},{"metric":"protein","value":150,"unit":"g"}]}`)

	// Carbs aren't tracked, so only calories and protein are logged.
	s.Set(at(12))
	lunch := intake("350", "protein", "30", "carbs", "40")
	results := logged(t, s, "350 cal, 30g protein, 40g carbs", "log_intake", lunch)
	ids := s.Rows(`SELECT id FROM intake_logs ORDER BY id`)
	if len(ids) != 2 {
		t.Fatalf("intake logs = %v", ids)
	}
	want := "logged 350 cal (#" + ids[0] + "), 30 g protein (#" + ids[1] + ") today. calories: 350 / 1,800 cal today, " +
		"1,450 cal to go; protein: 30 / 150 g today, 120 g to go. Not logged: carbs, because the user doesn't track it. " +
		"They can start by setting a goal for it."
	if fmt.Sprint(results) != "["+want+"]" {
		t.Errorf("log result = %q\nwant %q", results, want)
	}

	// A retried webhook and a rerun of the same text log nothing more.
	s.Retry()
	s.Script(sim.Call("log_intake", lunch), sim.Say("ok"))
	s.Rerun()
	if got := lastResults(s); fmt.Sprint(got) != "["+want+"]" {
		t.Errorf("rerun result = %q", got)
	}
	if got := s.Rows(`SELECT count(*) FROM intake_logs`); fmt.Sprint(got) != "[2]" {
		t.Fatalf("intake logs after retry and rerun = %v", got)
	}

	s.Set(at(15))
	results = logged(t, s, "890 cal", "log_intake", intake("890"))
	if len(results) != 1 || !strings.HasSuffix(results[0], " today. calories: 1,240 / 1,800 cal today, 560 cal to go; "+
		"protein: 30 / 150 g today, 120 g to go.") {
		t.Errorf("second log = %q", results)
	}
	snackID := s.Rows(`SELECT max(id) FROM intake_logs`)[0]
	results = logged(t, s, "no wait that was 900",
		"edit_log", `{"type":"calories","id":null,"amount":900,"date":null}`)
	want = "updated #" + snackID + ": now 900 cal today. calories: 1,250 / 1,800 cal today, 550 cal to go; " +
		"protein: 30 / 150 g today, 120 g to go."
	if fmt.Sprint(results) != "["+want+"]" {
		t.Errorf("edit result = %q\nwant %q", results, want)
	}

	// The prompt lists today's amounts with their ids and the totals.
	s.Script(sim.Say("You have 550 cal left."))
	s.Text("how many calories left")
	reqs := s.Requests()
	sys := reqs[len(reqs)-1][0].Content
	for _, w := range []string{
		"#" + ids[0] + " at 12:00 PM: 350 cal\n",
		"#" + ids[1] + " at 12:00 PM: 30 g protein\n",
		"#" + snackID + " at 3:00 PM: 900 cal\n",
		"Totals: calories: 1,250 / 1,800 cal today, 550 cal to go; protein: 30 / 150 g today, 120 g to go.",
	} {
		if !strings.Contains(sys, w) {
			t.Errorf("prompt lacks %q:\n%s", w, sys)
		}
	}

	results = logged(t, s, "remove the 350", "delete_log", `{"type":"calories","id":`+ids[0]+`}`)
	want = "deleted #" + ids[0] + ": 350 cal today. calories: 900 / 1,800 cal today, 900 cal to go; " +
		"protein: 30 / 150 g today, 120 g to go."
	if fmt.Sprint(results) != "["+want+"]" {
		t.Errorf("delete result = %q\nwant %q", results, want)
	}

	// Each undo reverses one change, newest first, without the LLM.
	llmCalls := len(s.Requests())
	for _, w := range []string{
		"Undid deleting 350 cal. Calories: 1,250 / 1,800 cal today, 550 cal to go.",
		"Undid changing 890 cal to 900 cal. Calories: 1,240 / 1,800 cal today, 560 cal to go.",
		"Undid logging 890 cal. Calories: 350 / 1,800 cal today, 1,450 cal to go.",
		"Undid logging 350 cal and 30 g protein. Calories: 0 / 1,800 cal today, 1,800 cal to go. " +
			"Protein: 0 / 150 g today, 150 g to go.",
		"Undid setting goals (calories: 1,800 cal a day; protein: 150 g a day).",
	} {
		if reply := s.Text("undo"); reply != w {
			t.Errorf("undo = %q\nwant %q", reply, w)
		}
	}
	// Running the last undo again doesn't undo anything more.
	if reply := s.Rerun(); reply != "Undid setting goals (calories: 1,800 cal a day; protein: 150 g a day)." {
		t.Errorf("rerun undo = %q", reply)
	}
	if reply := s.Text("Undo."); reply != "Nothing to undo." {
		t.Errorf("undo with nothing left = %q", reply)
	}
	if n := len(s.Requests()); n != llmCalls {
		t.Errorf("undo made %d LLM calls", n-llmCalls)
	}

	if got := s.Rows(`SELECT metric, amount, deleted_at IS NOT NULL FROM intake_logs ORDER BY id`); fmt.Sprint(got) !=
		"[calories | 350 | true protein | 30 | true calories | 890 | true]" {
		t.Errorf("intake logs = %v", got)
	}
	// Calories goes back to not tracking, as the sim user started, and the
	// protein goal set today is gone.
	if got := s.Rows(`SELECT metric, value FROM goals WHERE metric IN ('calories', 'protein') ORDER BY metric`); fmt.Sprint(got) != "[calories | <nil>]" {
		t.Errorf("goals = %v", got)
	}
	if got := s.Rows(`SELECT count(*) FROM actions WHERE undo IS NOT NULL AND undone_at IS NULL`); fmt.Sprint(got) != "[0]" {
		t.Errorf("actions not undone = %v", got)
	}
}

// TestIntakeDays checks amounts count toward the user's local day, and are
// judged against the goal in force that day.
func TestIntakeDays(t *testing.T) {
	vancouver, _ := time.LoadLocation("America/Vancouver")
	// 11:30 PM Monday in Vancouver is already Tuesday in UTC.
	s := sim.New(t, time.Date(2026, 10, 26, 23, 30, 0, 0, vancouver))
	for _, q := range []string{
		`UPDATE goals SET value = 1800 WHERE user_id = $1 AND metric = 'calories'`,
		`INSERT INTO goals (user_id, metric, value, start_date) VALUES ($1, 'calories', 2000, '2026-10-20')`,
	} {
		if _, err := s.Pool.Exec(s.Ctx, q, s.UserID); err != nil {
			t.Fatal(err)
		}
	}

	if r := logged(t, s, "500 cal", "log_intake", intake("500")); len(r) != 1 ||
		!strings.HasSuffix(r[0], " today. calories: 500 / 1,800 cal today, 1,300 cal to go.") {
		t.Errorf("log today = %q", r)
	}
	r := logged(t, s, "yesterday I had 2,100 cal", "log_intake", intake("2100", "date", `"2026-10-25"`))
	if len(r) != 1 || !strings.HasSuffix(r[0], " yesterday. calories: 2,100 / 2,000 cal yesterday, 100 cal over.") {
		t.Errorf("log yesterday = %q", r)
	}
	id := s.Rows(`SELECT max(id) FROM intake_logs`)[0]
	r = logged(t, s, "actually that was saturday", "edit_log", `{"type":"calories","id":null,"amount":null,"date":"2026-10-24"}`)
	want := "updated #" + id + ": now 2,100 cal on Oct 24. calories: 2,100 / 2,000 cal on Oct 24, 100 cal over. " +
		"calories: 0 / 2,000 cal yesterday, 2,000 cal to go."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("move result = %q\nwant %q", r, want)
	}
	if reply := s.Text("undo"); reply != "Undid moving 2,100 cal from Oct 25 to Oct 24. "+
		"Calories: 2,100 / 2,000 cal yesterday, 100 cal over." {
		t.Errorf("undo move = %q", reply)
	}
	// An edit that changes nothing doesn't claim the amount moved.
	logged(t, s, "it was 2100", "edit_log", `{"type":"calories","id":null,"amount":2100,"date":null}`)
	if reply := s.Text("undo"); reply != "Undid changing 2,100 cal to 2,100 cal. Calories: 2,100 / 2,000 cal yesterday, 100 cal over." {
		t.Errorf("undo no-op edit = %q", reply)
	}

	// Invalid calls are refused and change nothing.
	r = logged(t, s, "x",
		"log_intake", intake("100", "date", `"2026-10-27"`),
		"log_intake", intake("100", "date", `"2026-09-01"`),
		"log_intake", intake("100", "date", `"Monday"`),
		"log_intake", intake("-5"),
		"log_intake", intake("0.4"),
		"log_intake", intake("20000"),
		"log_intake", intake("100", "protein", "2000"),
		"log_intake", intake("null", "protein", "30"),
		"log_intake", intake("null"),
		"edit_log", `{"type":"water","id":null,"amount":1,"date":null}`,
		"edit_log", `{"type":"calories","id":null,"amount":null,"date":null}`,
		"edit_log", `{"type":"calories","id":999999999,"amount":1,"date":null}`,
		"edit_log", `{"type":"protein","id":`+id+`,"amount":1,"date":null}`,
		"delete_log", `{"type":"calories","id":999999999}`,
		"delete_log", `{"type":"protein","id":null}`,
	)
	for _, w := range []string{
		"error: date is in the future; today is 2026-10-26",
		"error: date is more than 30 days ago",
		`error: date "Monday" is not YYYY-MM-DD`,
		"error: calories must be more than 0",
		"error: calories must be more than 0",
		"error: calories must be at most 10,000 cal per entry",
		"error: protein must be at most 1,000 g per entry",
		"error: the user doesn't track protein, so nothing was logged",
		"error: no amounts given",
		`error: unknown log type "water"`,
		"error: no changes given",
		"error: no entry #999999999",
		"error: #" + id + " is calories, not protein",
		"error: no entry #999999999",
		"error: no protein logged",
	} {
		if len(r) == 0 || !strings.HasPrefix(r[0], w) {
			t.Errorf("result = %q, want %q", r, w)
			break
		}
		r = r[1:]
	}
	if got := s.Rows(`SELECT local_date::text, metric, amount FROM intake_logs WHERE deleted_at IS NULL ORDER BY id`); fmt.Sprint(got) !=
		"[2026-10-26 | calories | 500 2026-10-25 | calories | 2100]" {
		t.Errorf("intake logs = %v", got)
	}
}

// TestIntakeNoGoal checks calories log with no calorie goal. A user who
// never set one is asked for it by setup; one who chose not to track
// calories isn't asked.
func TestIntakeNoGoal(t *testing.T) {
	s := sim.New(t, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	s.Script(sim.Call("log_intake", intake("350")), sim.Say("Logged 350 cal."))
	if reply := s.Text("350 cal"); reply != "Logged 350 cal." {
		t.Errorf("not tracking: reply = %q", reply)
	}
	if r := lastResults(s); len(r) != 1 || !strings.HasSuffix(r[0], " today. calories: 350 cal today.") {
		t.Errorf("not tracking = %q", r)
	}
	if _, err := s.Pool.Exec(s.Ctx, `DELETE FROM goals WHERE metric = 'calories'`); err != nil {
		t.Fatal(err)
	}
	s.Script(sim.Call("log_intake", intake("200")), sim.Say("Logged 200 cal, 550 today."))
	if reply := s.Text("200 cal"); reply != `Logged 200 cal, 550 today. And your daily calorie goal? Say "none" if you don't want to track it.` {
		t.Errorf("no goal: reply = %q", reply)
	}
	if r := lastResults(s); len(r) != 1 || !strings.HasSuffix(r[0], " today. calories: 550 cal today.") {
		t.Errorf("no goal = %q", r)
	}
}

// TestMacrosFollowGoals checks a macro is logged only while the user has a
// goal for it, and stops when they say they no longer track it.
func TestMacrosFollowGoals(t *testing.T) {
	s := sim.New(t, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	logged(t, s, "track 200g carbs", "set_goals", `{"goals":[{"metric":"carbs","value":200,"unit":"g"}]}`)
	if r := logged(t, s, "60g carbs", "log_intake", intake("null", "carbs", "60")); len(r) != 1 ||
		!strings.HasSuffix(r[0], " today. calories: 0 cal today; carbs: 60 / 200 g today, 140 g to go.") {
		t.Errorf("tracked carbs = %q", r)
	}
	logged(t, s, "stop tracking carbs", "set_goals", `{"goals":[{"metric":"carbs","value":null,"unit":null}]}`)
	if r := logged(t, s, "300 cal, 40g carbs, 10g fat", "log_intake", intake("300", "carbs", "40", "fat", "10")); len(r) != 1 ||
		!strings.HasSuffix(r[0], "Not logged: carbs and fat, because the user doesn't track them. They can start by setting a goal for them.") {
		t.Errorf("two untracked macros = %q", r)
	}
	if r := logged(t, s, "40g carbs", "log_intake", intake("null", "carbs", "40")); len(r) != 1 ||
		!strings.HasPrefix(r[0], "error: the user doesn't track carbs") {
		t.Errorf("untracked carbs = %q", r)
	}
	if got := s.Rows(`SELECT metric, amount FROM intake_logs ORDER BY id`); fmt.Sprint(got) != "[carbs | 60 calories | 300]" {
		t.Errorf("intake logs = %v", got)
	}
}

// TestUndoWithoutLLM checks "undo" works with no LLM configured, while
// other texts get the fallback reply.
func TestUndoWithoutLLM(t *testing.T) {
	s := sim.New(t, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	logged(t, s, "350 cal", "log_intake", intake("350"))
	s.Agent.LLM = nil
	calls := len(s.Requests())
	if reply := s.Text("undo that"); reply != "Undid logging 350 cal. Calories: 0 cal today." {
		t.Errorf("undo = %q", reply)
	}
	if reply := s.Text("hello"); !strings.HasPrefix(reply, "Sorry") {
		t.Errorf("reply without LLM = %q", reply)
	}
	if n := len(s.Requests()); n != calls {
		t.Errorf("LLM calls after removing it = %d", n-calls)
	}
}

// TestUndoProfileAndMemory undoes profile and memory changes in reverse
// order, including a forgotten fact that a later undo changes back.
func TestUndoProfileAndMemory(t *testing.T) {
	s := sim.New(t, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	logged(t, s, "call me Sammy, I have a dog",
		"set_profile", `{"name":"Sammy","timezone":null,"wake_time":"06:30","bedtime":null}`,
		"remember", `{"fact":"Has a dog"}`)
	id := s.Rows(`SELECT id FROM memories`)[0]
	logged(t, s, "two dogs now", "update_memory", `{"id":`+id+`,"fact":"Has two dogs"}`)
	logged(t, s, "forget the dogs", "forget", `{"id":`+id+`}`)

	for _, c := range []struct{ reply, memories, profile string }{
		{`Undid forgetting "Has two dogs".`, "[" + id + " | Has two dogs]", "[Sammy | 06:30:00]"},
		{`Undid changing "Has a dog" to "Has two dogs".`, "[" + id + " | Has a dog]", "[Sammy | 06:30:00]"},
		{`Undid saving "Has a dog".`, "[]", "[Sammy | 06:30:00]"},
		{"Undid setting your name to Sammy, wake time to 6:30 AM.", "[]", "[Sam | 08:00:00]"},
	} {
		if reply := s.Text("undo"); reply != c.reply {
			t.Errorf("undo = %q, want %q", reply, c.reply)
		}
		if got := s.Rows(`SELECT id, fact FROM memories`); fmt.Sprint(got) != c.memories {
			t.Errorf("memories = %v, want %s", got, c.memories)
		}
		if got := s.Rows(`SELECT name, wake_time::text FROM users`); fmt.Sprint(got) != c.profile {
			t.Errorf("profile = %v, want %s", got, c.profile)
		}
	}
}

// TestUndoDuringSetup checks undo during setup asks the setup question the
// undone answer had closed.
func TestUndoDuringSetup(t *testing.T) {
	s := sim.New(t, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	s.ResetSetup()
	s.Text("hi")
	s.Script(sim.Call("set_profile", `{"name":"Sam","timezone":null,"wake_time":null,"bedtime":null}`), sim.Say("Got it."))
	if reply := s.Text("Sam"); !strings.HasPrefix(reply, "Got it. What city") {
		t.Errorf("reply = %q", reply)
	}
	if reply := s.Text("undo"); reply != "Undid setting your name to Sam. What should I call you?" {
		t.Errorf("undo = %q", reply)
	}
}
