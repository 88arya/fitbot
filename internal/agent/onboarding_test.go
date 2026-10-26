package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/agent"
	"github.com/88arya/fitbot/internal/sim"
)

// TestOnboardingEndToEnd takes a new user from the intro to every goal set,
// entirely by text through the webhook, then checks a retried webhook and a
// rerun change nothing and the morning check-in comes at the new wake time.
func TestOnboardingEndToEnd(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 20, 12, 0, 0, 0, loc))
	s.ResetSetup()

	if reply := s.Text("hi"); reply != agent.Intro() {
		t.Fatalf("first reply = %q, want the intro", reply)
	}
	for _, step := range []struct {
		text, call, args, next string
	}{
		{"Sam", "set_profile", `{"name":"Sam","timezone":null,"wake_time":null,"bedtime":null}`, "What city are you in?"},
		{"toronto", "set_profile", `{"name":null,"timezone":"America/Toronto","wake_time":null,"bedtime":null}`,
			"What time do you usually wake up and go to bed?"},
		{"6:15, and bed around 10:30", "set_profile", `{"name":null,"timezone":null,"wake_time":"06:15","bedtime":"22:30"}`,
			"What are your daily calorie and water goals?"},
		{"2000 cal and 2.5L water, 150g protein", "set_goals",
			`{"goals":[{"metric":"calories","value":2000,"unit":"cal"},{"metric":"water","value":2.5,"unit":"l"},{"metric":"protein","value":150,"unit":"g"}]}`,
			"How many hours of sleep"},
		{"8 hours", "set_goals", `{"goals":[{"metric":"sleep","value":8,"unit":"hours"}]}`, "Do you have a target weight?"},
		{"175 lbs", "set_goals", `{"goals":[{"metric":"weight","value":175,"unit":"lbs"}]}`, "You're all set."},
	} {
		s.Script(sim.Call(step.call, step.args), sim.Say("Got it."))
		reply := s.Text(step.text)
		if !strings.HasPrefix(reply, "Got it. "+step.next) {
			t.Errorf("%q: reply = %q, want it to ask %q", step.text, reply, step.next)
		}
	}

	user := `SELECT name, timezone, wake_time::text, bedtime::text FROM users`
	goals := `SELECT metric, value, start_date::text FROM goals ORDER BY metric`
	wantUser := "[Sam | America/Toronto | 06:15:00 | 22:30:00]"
	wantGoals := "[calories | 2000 | 2026-10-20 protein | 150 | 2026-10-20 sleep | 480 | 2026-10-20 " +
		"water | 2500 | 2026-10-20 weight | 79379 | 2026-10-20]"
	check := func(when string) {
		t.Helper()
		if got := s.Rows(user); fmt.Sprint(got) != wantUser {
			t.Errorf("%s: user = %v, want %s", when, got, wantUser)
		}
		if got := s.Rows(goals); fmt.Sprint(got) != wantGoals {
			t.Errorf("%s: goals = %v\nwant %s", when, got, wantGoals)
		}
	}
	check("after setup")

	// Redo the last text: the webhook retried, then the agent run again
	// with the same call, as after a crash. Neither saves anything again.
	actions := s.Rows(`SELECT id FROM actions`)
	s.Retry()
	s.Script(sim.Call("set_goals", `{"goals":[{"metric":"weight","value":175,"unit":"lbs"}]}`), sim.Say("Got it."))
	s.Rerun()
	check("after retry and rerun")
	if again := s.Rows(`SELECT id FROM actions`); fmt.Sprint(again) != fmt.Sprint(actions) {
		t.Errorf("actions after rerun = %v, want %v", again, actions)
	}

	// Setup is done, so a later text gets no setup question.
	s.Script(sim.Say("Morning!"))
	if reply := s.Text("morning"); reply != "Morning!" {
		t.Errorf("reply after setup = %q", reply)
	}

	// The morning check-in moved from the 8:00 default to the wake time.
	before := len(s.Sends())
	s.Set(time.Date(2026, 10, 21, 6, 15, 0, 0, loc))
	s.Tick()
	if sends := s.WaitSends(before + 1); sends[before] != "Good morning, Sam. How did you sleep?" {
		t.Errorf("check-in = %q", sends[before])
	}
	s.Set(time.Date(2026, 10, 21, 8, 0, 0, 0, loc))
	s.Tick()
	time.Sleep(100 * time.Millisecond)
	if n := len(s.Sends()); n != before+1 {
		t.Errorf("sends after 8:00 tick = %d, want %d", n, before+1)
	}
	if got := s.Rows(`SELECT local_date::text, to_char(scheduled_at AT TIME ZONE 'America/Toronto', 'HH24:MI'), status
		FROM deliveries ORDER BY local_date`); fmt.Sprint(got) != "[2026-10-21 | 06:15 | sent 2026-10-22 | 06:15 | pending]" {
		t.Errorf("deliveries = %v, want Oct 21 sent and Oct 22 planned, both at 6:15", got)
	}
}
