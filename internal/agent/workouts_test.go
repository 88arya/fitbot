package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// workoutCall builds log_workout arguments: status, then sessions and date,
// each JSON or null.
func workoutCall(status, sessions, date string) string {
	return fmt.Sprintf(`{"status":"%s","sessions":%s,"date":%s}`, status, sessions, date)
}

// editWorkout builds edit_log arguments for a workout: id, status,
// sessions, and date, each JSON or null.
func editWorkout(id, status, sessions, date string) string {
	return fmt.Sprintf(`{"type":"workout","id":%s,"amount":null,"unit":null,"bedtime":null,"wake_time":null,`+
		`"status":%s,"sessions":%s,"date":%s}`, id, status, sessions, date)
}

func workoutWeek(t *testing.T, s *sim.Sim, monday time.Time) (string, []string) {
	t.Helper()
	line, days, err := s.Agent.WorkoutWeek(s.Ctx, s.UserID, monday)
	if err != nil {
		t.Fatal(err)
	}
	return line, days
}

func lastWorkout(s *sim.Sim) string {
	return s.Rows(`SELECT max(id) FROM workouts`)[0]
}

// workoutText is one text on a day: the local time, and the tool calls the
// LLM answers it with.
type workoutText struct {
	hour, min int
	calls     []string
}

// workoutDay is one simulated day: its texts, and what should become of
// each session, in plan order then extras: "legs:done", "cardio:missed",
// or "+cardio" for an extra. desc is the day as shown when nothing was
// planned.
type workoutDay struct {
	texts []workoutText
	want  string
	desc  string
}

// tally counts a week's outcomes the way the week line reports them.
func tally(monday time.Time, days []workoutDay) string {
	counts := map[string]int{}
	planned, extra := 0, 0
	for _, d := range days {
		for _, tok := range strings.Fields(d.want) {
			if strings.HasPrefix(tok, "+") {
				extra++
				continue
			}
			planned++
			counts[tok[strings.Index(tok, ":")+1:]]++
		}
	}
	plural := func(n int, noun string) string {
		if n == 1 {
			return "1 " + noun
		}
		return fmt.Sprintf("%d %ss", n, noun)
	}
	s := "week of " + monday.Format("Jan 2") + ": "
	if planned == 0 {
		return s + "nothing planned"
	}
	s += fmt.Sprintf("%d of %s done", counts["done"], plural(planned, "planned session"))
	for _, k := range []string{"skipped", "missed"} {
		if counts[k] > 0 {
			s += fmt.Sprintf(", %d %s", counts[k], k)
		}
	}
	if extra > 0 {
		s += ", plus " + plural(extra, "extra session")
	}
	return s
}

// shown is how a day with outcomes want is described once it is over.
func shown(d workoutDay) string {
	if d.desc != "" && !strings.Contains(d.want, ":") {
		parts := []string{d.desc}
		for _, tok := range strings.Fields(d.want) {
			parts = append(parts, tok[1:]+" done (extra)")
		}
		return strings.Join(parts, ", ")
	}
	var parts, extras []string
	for _, tok := range strings.Fields(d.want) {
		if name, ok := strings.CutPrefix(tok, "+"); ok {
			extras = append(extras, name+" done (extra)")
			continue
		}
		parts = append(parts, strings.Replace(tok, ":", " ", 1))
	}
	return strings.Join(append(parts, extras...), ", ")
}

// TestWorkoutMonth runs five weeks of a weekly PPL with cardio: done,
// partly done, skipped by log and ahead with plan_session, missed,
// backfilled, corrected, moved, deleted, and extra workouts. Every day and
// every week's planned-vs-done count is checked against a tally of what
// should have happened.
func TestWorkoutMonth(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	// Monday.
	start := time.Date(2026, 10, 26, 8, 0, 0, 0, loc)
	s := sim.New(t, start)
	logged(t, s, "ppl", "set_schedule", weeklyPPL)

	done := workoutCall("done", "null", "null")
	evening := func(calls ...string) []workoutText { return []workoutText{{20, 0, calls}} }
	rest := workoutDay{desc: "rest day"}
	weeks := [][]workoutDay{
		{
			{texts: evening("log_workout", done), want: "push:done"},
			{texts: evening("log_workout", workoutCall("done", `["pull"]`, "null")), want: "pull:done cardio:missed"},
			{texts: evening("log_workout", workoutCall("skipped", "null", "null")), want: "legs:skipped"},
			{want: "push:missed"},
			{texts: evening("log_workout", done), want: "pull:done cardio:done"},
			{texts: evening("log_workout", done), want: "legs:done"},
			{texts: evening("log_workout", workoutCall("done", `["cardio"]`, "null")), want: "+cardio", desc: "rest day"},
		},
		{
			// Skipped ahead of time, and nothing logged.
			{texts: []workoutText{{8, 0, []string{"plan_session", planCall("null", "skip", "null", "null")}}}, want: "push:skipped"},
			{texts: evening("log_workout", workoutCall("done", `["pull"]`, "null"), "log_workout", workoutCall("skipped", `["cardio"]`, "null")),
				want: "pull:done cardio:skipped"},
			// Skipped ahead, then went after all.
			{texts: []workoutText{{7, 0, []string{"plan_session", planCall("null", "skip", "null", "null")}}, {20, 0, []string{"log_workout", done}}},
				want: "legs:done"},
			// Nothing logged Thursday; Friday morning "I did go yesterday", then
			// Friday's session moves to Sunday.
			{want: "push:done"},
			{texts: []workoutText{{8, 0, []string{"log_workout", workoutCall("done", "null", `"2026-11-05"`)}},
				{8, 5, []string{"plan_session", planCall("null", "move", `"2026-11-08"`, "null")}}},
				desc: "nothing planned, pull + cardio moved to Sun Nov 8"},
			// Logged skipped, then corrected on Sunday.
			{texts: evening("log_workout", workoutCall("skipped", "null", "null")), want: "legs:done"},
			{texts: []workoutText{{9, 0, []string{"edit_log", editWorkout("null", `"done"`, "null", "null")}}, {18, 0, []string{"log_workout", done}}},
				want: "pull:done cardio:done"},
		},
		{
			// A second workout the same day is an extra.
			{texts: []workoutText{{18, 0, []string{"log_workout", done}}, {21, 0, []string{"log_workout", workoutCall("done", `["cardio"]`, "null")}}},
				want: "push:done +cardio"},
			{texts: evening("log_workout", done), want: "pull:done cardio:done"},
			// Logged, then deleted.
			{texts: []workoutText{{19, 0, []string{"log_workout", done}}, {19, 5, []string{"delete_log", `{"type":"workout","id":null}`}}},
				want: "legs:missed"},
			{want: "push:missed"},
			{want: "pull:missed cardio:missed"},
			{texts: evening("log_workout", workoutCall("done", `["legs"]`, "null")), want: "legs:done"},
			rest,
		},
		{
			{texts: evening("log_workout", done), want: "push:done"},
			{texts: evening("log_workout", done), want: "pull:done cardio:done"},
			{texts: evening("log_workout", done), want: "legs:done"},
			{texts: evening("log_workout", done), want: "push:done"},
			{texts: evening("log_workout", done), want: "pull:done cardio:done"},
			{texts: evening("log_workout", done), want: "legs:done"},
			rest,
		},
		{
			// Monday's workout is logged just after midnight, then moved back to
			// Monday, where it covers Monday's session instead of Tuesday's.
			{want: "push:done"},
			{texts: []workoutText{{0, 30, []string{"log_workout", done}}, {0, 31, []string{"edit_log", editWorkout("null", "null", "null", `"2026-11-23"`)}}},
				want: "pull:missed cardio:missed"},
			{want: "legs:missed"},
			{want: "push:missed"},
			{want: "pull:missed cardio:missed"},
			{want: "legs:missed"},
			rest,
		},
	}

	for w, week := range weeks {
		for i, d := range week {
			day := start.AddDate(0, 0, 7*w+i)
			for _, txt := range d.texts {
				s.Set(time.Date(day.Year(), day.Month(), day.Day(), txt.hour, txt.min, 0, 0, loc))
				r := logged(t, s, "workout", txt.calls...)
				for _, res := range r {
					if strings.HasPrefix(res, "error") {
						t.Errorf("%s %02d:%02d: %s", dayName(day), txt.hour, txt.min, res)
					}
				}
			}
			// Midweek, the week counts the sessions still to come.
			if w == 0 && i == 2 {
				if line, _ := workoutWeek(t, s, start); line != "week of Oct 26: 2 of 8 planned sessions done, 1 skipped, 1 missed, 4 to go" {
					t.Errorf("midweek = %q", line)
				}
			}
		}
	}

	// A day the week after the last one, every week is over.
	s.Set(time.Date(2026, 11, 30, 9, 0, 0, 0, loc))
	for w, week := range weeks {
		monday := start.AddDate(0, 0, 7*w)
		line, days := workoutWeek(t, s, monday)
		if want := tally(monday, week); line != want {
			t.Errorf("week %d = %q\nwant %q", w+1, line, want)
		}
		for i, d := range week {
			if want := shown(d); days[i] != want {
				t.Errorf("%s = %q, want %q", dayName(monday.AddDate(0, 0, i)), days[i], want)
			}
		}
	}
	// The tally isn't trivially right: check its counts once by hand.
	if got := tally(start.AddDate(0, 0, 7), weeks[1]); got != "week of Nov 2: 6 of 8 planned sessions done, 2 skipped" {
		t.Errorf("tally = %q", got)
	}
	if got := tally(start.AddDate(0, 0, 14), weeks[2]); got != "week of Nov 9: 4 of 8 planned sessions done, 4 missed, plus 1 extra session" {
		t.Errorf("tally = %q", got)
	}
}

// TestWorkoutLogs logs, edits, moves, and deletes a day's workouts, checks
// the results and the prompt, and undoes every change.
func TestWorkoutLogs(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 8, 0, 0, 0, loc))
	logged(t, s, "ppl", "set_schedule", weeklyPPL)
	// Tuesday: pull + cardio. Monday's push was missed.
	s.Set(time.Date(2026, 10, 27, 19, 0, 0, 0, loc))

	r := logged(t, s, "did pull", "log_workout", workoutCall("done", `["Pull"]`, "null"))
	pull := lastWorkout(s)
	want := "logged #" + pull + ": pull done today. workouts today: pull done, cardio not logged yet; " +
		"week of Oct 26: 1 of 8 planned sessions done, 1 missed, 6 to go."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("log = %q\nwant %q", r, want)
	}
	// A retried webhook and a rerun of the same text log nothing more.
	s.Retry()
	s.Script(sim.Call("log_workout", workoutCall("done", `["Pull"]`, "null")), sim.Say("ok"))
	s.Rerun()
	if got := lastResults(s); fmt.Sprint(got) != "["+want+"]" {
		t.Errorf("rerun = %q", got)
	}

	r = logged(t, s, "skipped cardio", "log_workout", workoutCall("skipped", "null", "null"))
	cardio := lastWorkout(s)
	want = "logged #" + cardio + ": cardio skipped today. workouts today: pull done, cardio skipped; " +
		"week of Oct 26: 1 of 8 planned sessions done, 1 skipped, 1 missed, 5 to go."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("log skipped = %q\nwant %q", r, want)
	}
	r = logged(t, s, "actually I did cardio", "edit_log", editWorkout(cardio, `"done"`, "null", "null"))
	want = "updated #" + cardio + ": now cardio done today. workouts today: pull done, cardio done; " +
		"week of Oct 26: 2 of 8 planned sessions done, 1 missed, 5 to go."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("edit status = %q\nwant %q", r, want)
	}
	// Moving the pull log to Monday covers Monday's push instead.
	r = logged(t, s, "the pull was yesterday", "edit_log", editWorkout(pull, "null", "null", `"2026-10-26"`))
	want = "updated #" + pull + ": now push done yesterday. workouts yesterday: push done; workouts today: pull not logged yet, " +
		"cardio done; week of Oct 26: 2 of 8 planned sessions done, 6 to go."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("edit date = %q\nwant %q", r, want)
	}

	// The prompt lists the week's workouts, newest first, with the status.
	s.Script(sim.Say("ok"))
	s.Text("how's my week")
	reqs := s.Requests()
	sys := reqs[len(reqs)-1][0].Content
	for _, w := range []string{
		"#" + cardio + " Tue Oct 27 (today): cardio done\n#" + pull + " Mon Oct 26: push done\n",
		"Status: workouts today: pull not logged yet, cardio done; week of Oct 26: 2 of 8 planned sessions done, 6 to go.\n",
	} {
		if !strings.Contains(sys, w) {
			t.Errorf("prompt lacks %q:\n%s", w, sys)
		}
	}

	// A null id is the latest day's workout.
	r = logged(t, s, "delete that", "delete_log", `{"type":"workout","id":null}`)
	want = "deleted #" + cardio + ": cardio done today. workouts today: pull not logged yet, cardio not logged yet; " +
		"week of Oct 26: 1 of 8 planned sessions done, 7 to go."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("delete = %q\nwant %q", r, want)
	}

	llmCalls := len(s.Requests())
	for _, w := range []string{
		"Undid deleting cardio done on Tue Oct 27. Workouts today: pull not logged yet, cardio done. " +
			"Week of Oct 26: 2 of 8 planned sessions done, 6 to go.",
		"Undid changing pull done to push done and moving it from Tue Oct 27 to Mon Oct 26. Workouts today: pull done, cardio done. " +
			"Week of Oct 26: 2 of 8 planned sessions done, 1 missed, 5 to go.",
		"Undid changing cardio skipped to cardio done on Tue Oct 27. Workouts today: pull done, cardio skipped. " +
			"Week of Oct 26: 1 of 8 planned sessions done, 1 skipped, 1 missed, 5 to go.",
		"Undid logging cardio skipped on Tue Oct 27. Workouts today: pull done, cardio not logged yet. " +
			"Week of Oct 26: 1 of 8 planned sessions done, 1 missed, 6 to go.",
		"Undid logging pull done on Tue Oct 27. Workouts today: pull not logged yet, cardio not logged yet. " +
			"Week of Oct 26: 0 of 8 planned sessions done, 1 missed, 7 to go.",
	} {
		if reply := s.Text("undo"); reply != w {
			t.Errorf("undo = %q\nwant %q", reply, w)
		}
	}
	if n := len(s.Requests()); n != llmCalls {
		t.Errorf("undo made %d LLM calls", n-llmCalls)
	}
	if got := s.Rows(`SELECT local_date::text, status, sessions::text, deleted_at IS NOT NULL FROM workouts ORDER BY id`); fmt.Sprint(got) !=
		"[2026-10-27 | done | {pull} | true 2026-10-27 | skipped | {cardio} | true]" {
		t.Errorf("workouts = %v", got)
	}
}

// TestWorkoutRotation checks workouts against a rotation with a move: the
// day moved off and the day the rotation waits through have nothing
// planned, and the moved session counts on the day it moved to.
func TestWorkoutRotation(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	start := time.Date(2026, 10, 26, 8, 0, 0, 0, loc)
	s := sim.New(t, start)
	logged(t, s, "push pull rest", "set_schedule", rotation("null", "push", "pull", "rest"))
	logged(t, s, "doing push wednesday", "plan_session", planCall("null", "move", `"2026-10-28"`, "null"))

	s.Set(time.Date(2026, 10, 28, 18, 0, 0, 0, loc))
	r := logged(t, s, "hit the gym", "log_workout", workoutCall("done", "null", "null"))
	if len(r) != 1 || !strings.HasSuffix(r[0], "workouts today: push done; week of Oct 26: 1 of 4 planned sessions done, 3 to go.") {
		t.Errorf("log = %q", r)
	}
	// Yesterday the rotation waited, so there was nothing to skip.
	r = logged(t, s, "skipped yesterday", "log_workout", workoutCall("skipped", "null", `"2026-10-27"`))
	if want := "error: nothing is planned on Tue Oct 27 (nothing, the rotation waits for the session moved to Wed Oct 28), " +
		"so there is nothing to skip"; fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("skip a waiting day = %q\nwant %q", r, want)
	}
	line, days := workoutWeek(t, s, start)
	want := []string{"nothing planned, push moved to Wed Oct 28", "nothing planned, the rotation waits for the session moved to Wed Oct 28",
		"push done", "pull not logged yet", "rest day", "push not logged yet", "pull not logged yet"}
	if line != "week of Oct 26: 1 of 4 planned sessions done, 3 to go" || fmt.Sprint(days) != fmt.Sprint(want) {
		t.Errorf("week = %q %q", line, days)
	}
}

// TestWorkoutRefused checks invalid workout logs and changes are refused
// and change nothing.
func TestWorkoutRefused(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 26, 8, 0, 0, 0, loc))
	for _, c := range []struct{ tool, args, want string }{
		{"delete_log", `{"type":"workout","id":null}`, "error: no workout logged"},
		{"edit_log", editWorkout("null", `"done"`, "null", "null"), "error: no workout logged"},
	} {
		if r := logged(t, s, "fix", c.tool, c.args); fmt.Sprint(r) != "["+c.want+"]" {
			t.Errorf("%s = %q, want %q", c.args, r, c.want)
		}
	}
	logged(t, s, "ppl", "set_schedule", weeklyPPL)

	// Before the schedule started, nothing was planned, so a workout is an
	// extra.
	r := logged(t, s, "worked out yesterday", "log_workout", workoutCall("done", "null", `"2026-10-25"`))
	extra := lastWorkout(s)
	want := "logged #" + extra + ": workout done yesterday. workouts yesterday: nothing planned, workout done (extra); " +
		"week of Oct 19: nothing planned, 1 extra session done."
	if fmt.Sprint(r) != "["+want+"]" {
		t.Errorf("extra = %q\nwant %q", r, want)
	}
	logged(t, s, "did push", "log_workout", workoutCall("done", "null", "null"))
	push := lastWorkout(s)

	for _, c := range []struct{ tool, args, want string }{
		{"log_workout", workoutCall("finished", "null", "null"), `error: status "finished" must be done or skipped`},
		{"log_workout", workoutCall("done", `["rest"]`, "null"), "error: rest is not a session; a rest day needs no log"},
		{"log_workout", workoutCall("done", `["legs","Legs"]`, "null"), "error: legs is listed twice on one day"},
		{"log_workout", workoutCall("done", "null", `"2026-10-27"`), "error: date is in the future; today is 2026-10-26"},
		{"log_workout", workoutCall("done", "null", `"2026-09-01"`), "error: date is more than 30 days ago"},
		{"log_workout", workoutCall("done", "null", `"yesterday"`), `error: date "yesterday" is not YYYY-MM-DD`},
		{"log_workout", workoutCall("skipped", `["legs"]`, "null"),
			"error: legs isn't planned on Mon Oct 26 (push at 6:00 PM), so it can't be skipped"},
		{"log_workout", workoutCall("skipped", "null", `"2026-10-24"`),
			"error: nothing is planned on Sat Oct 24 (no schedule), so there is nothing to skip"},
		{"log_workout", workoutCall("done", "null", "null"),
			"error: everything planned on Mon Oct 26 is already logged; name the sessions of an extra workout, or change a log with edit_log"},
		{"log_workout", workoutCall("skipped", `["push"]`, "null"),
			"error: push on Mon Oct 26 is already logged as #" + push + " (push done); change it with edit_log instead"},
		{"log_workout", workoutCall("done", "null", `"2026-10-25"`),
			"error: workout on Sun Oct 25 is already logged as #" + extra + " (workout done); change it with edit_log instead"},
		{"plan_session", planCall("null", "skip", "null", "null"),
			"error: Mon Oct 26 already has a workout logged (#" + push + ", push done); change it with edit_log instead"},
		{"plan_session", planCall("null", "move", `"2026-10-27"`, "null"),
			"error: Mon Oct 26 already has a workout logged (#" + push + ", push done); change it with edit_log instead"},
		{"edit_log", editWorkout(push, "null", "null", "null"), "error: no changes given"},
		{"edit_log", editWorkout(push, `"maybe"`, "null", "null"), `error: status "maybe" must be done or skipped`},
		{"edit_log", editWorkout(push, "null", "[]", "null"), "error: sessions must name what was done or skipped, or be null to keep them"},
		{"edit_log", editWorkout("999999", `"done"`, "null", "null"), "error: no workout #999999"},
		{"edit_log", `{"type":"workout","id":null,"amount":1,"unit":null,"bedtime":null,"wake_time":null,"status":null,"sessions":null,"date":null}`,
			"error: a workout has no amount or times; change its status, sessions, or date"},
		{"edit_log", `{"type":"calories","id":null,"amount":null,"unit":null,"bedtime":null,"wake_time":null,"status":"done","sessions":null,"date":null}`,
			"error: status is only for workouts and supplements, and sessions only for workouts"},
		// Moved to a day before the schedule, push becomes that day's plan,
		// which has nothing to skip.
		{"edit_log", editWorkout(push, `"skipped"`, "null", `"2026-10-24"`),
			"error: nothing is planned on Sat Oct 24 (no schedule), so there is nothing to skip"},
		{"edit_log", editWorkout(push, "null", `["workout"]`, `"2026-10-25"`),
			"error: workout on Sun Oct 25 is already logged as #" + extra + " (workout done); change it with edit_log instead"},
		{"edit_log", editWorkout(push, `"skipped"`, `["pull"]`, "null"),
			"error: pull isn't planned on Mon Oct 26 (push at 6:00 PM), so it can't be skipped"},
		{"delete_log", `{"type":"workout","id":999999}`, "error: no workout #999999"},
	} {
		if r := logged(t, s, "workout", c.tool, c.args); fmt.Sprint(r) != "["+c.want+"]" {
			t.Errorf("%s %s = %q\nwant %q", c.tool, c.args, r, c.want)
		}
	}
	if got := s.Rows(`SELECT local_date::text, status, sessions::text, deleted_at IS NULL FROM workouts ORDER BY id`); fmt.Sprint(got) !=
		"[2026-10-25 | done | {workout} | true 2026-10-26 | done | {push} | true]" {
		t.Errorf("workouts = %v", got)
	}
	if got := s.Rows(`SELECT count(*) FROM session_plans`); fmt.Sprint(got) != "[0]" {
		t.Errorf("plans = %v", got)
	}
}
