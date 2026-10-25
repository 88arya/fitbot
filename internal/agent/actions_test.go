package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/llm"
	"github.com/88arya/fitbot/internal/sim"
)

func toolResults(req []llm.Message) []string {
	var out []string
	for _, m := range req {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

// TestTextAppliedOnce runs a scripted conversation, retries its webhook,
// and runs the agent on the same text again, and checks each change lands
// once.
func TestTextAppliedOnce(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 20, 12, 0, 0, 0, loc))

	s.Script(
		sim.Call("set_profile", `{"name":"Sammy","timezone":null}`,
			"remember", `{"fact":"Trains legs on Mondays"}`),
		sim.Say("Got it, Sammy."),
	)
	if reply := s.Text("call me Sammy, I train legs on mondays"); reply != "Got it, Sammy." {
		t.Fatalf("reply = %q", reply)
	}
	first := toolResults(s.Requests()[1])

	// A retried webhook is dropped before the agent runs.
	s.Retry()
	if n, sends := len(s.Requests()), s.Sends(); n != 2 || len(sends) != 1 {
		t.Fatalf("retried webhook ran again: %d LLM calls, sends %q", n, sends)
	}

	// Running the text again, with the same calls spelled differently,
	// returns the first run's results and changes nothing.
	s.Script(
		sim.Call("set_profile", `{ "timezone": null, "name": "Sammy" }`,
			"remember", `{"fact": "Trains legs on Mondays"}`),
		sim.Say("Got it, Sammy."),
	)
	if reply := s.Rerun(); reply != "Got it, Sammy." {
		t.Fatalf("rerun reply = %q", reply)
	}
	if again := toolResults(s.Requests()[3]); fmt.Sprint(again) != fmt.Sprint(first) {
		t.Errorf("rerun tool results = %v, want %v", again, first)
	}

	if got := s.Rows(`SELECT name FROM users`); fmt.Sprint(got) != "[Sammy]" {
		t.Errorf("users = %v", got)
	}
	if got := s.Rows(`SELECT fact FROM memories`); fmt.Sprint(got) != "[Trains legs on Mondays]" {
		t.Errorf("memories = %v", got)
	}
	got := s.Rows(`SELECT tool, occurrence, result FROM actions ORDER BY id`)
	if len(got) != 2 || got[0] != "set_profile | 1 | saved" || !strings.HasPrefix(got[1], "remember | 1 | saved as #") {
		t.Errorf("actions = %q", got)
	}
}

// TestRepeatedCallsInOneText checks identical calls for one text each
// apply, and still apply only once when the text is run again.
func TestRepeatedCallsInOneText(t *testing.T) {
	s := sim.New(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	calls := sim.Call("remember", `{"fact":"Drinks coffee"}`, "remember", `{"fact":"Drinks coffee"}`)

	s.Script(calls, sim.Say("Noted."))
	s.Text("coffee, and more coffee")
	s.Script(calls, sim.Say("Noted."))
	s.Rerun()

	if got := s.Rows(`SELECT fact FROM memories`); len(got) != 2 {
		t.Errorf("memories = %v, want 2", got)
	}
	if got := s.Rows(`SELECT occurrence FROM actions ORDER BY id`); fmt.Sprint(got) != "[1 2]" {
		t.Errorf("action occurrences = %v", got)
	}
}

// TestRejectedCallNotRecorded checks a call the handler refuses leaves no
// action behind, so a corrected call in the same text applies.
func TestRejectedCallNotRecorded(t *testing.T) {
	s := sim.New(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	s.Script(
		sim.Call("set_profile", `{"name":null,"timezone":"Mars/Base"}`, "remember", `{"fact":"x","extra":1}`),
		sim.Call("set_profile", `{"name":null,"timezone":"America/Toronto"}`),
		sim.Say("Saved."),
	)
	s.Text("I'm on mars")

	results := toolResults(s.Requests()[1])
	if len(results) != 2 || !strings.HasPrefix(results[0], "error:") || !strings.HasPrefix(results[1], "error: invalid arguments") {
		t.Errorf("tool results = %q", results)
	}
	if got := s.Rows(`SELECT tool, args::jsonb->>'timezone' FROM actions`); fmt.Sprint(got) != "[set_profile | America/Toronto]" {
		t.Errorf("actions = %v", got)
	}
}
