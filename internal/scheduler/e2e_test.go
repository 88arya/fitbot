package scheduler_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// TestCheckinEndToEnd sends a morning check-in through the Telnyx client,
// records its delivery report through the webhook, and checks the user's
// reply reaches the LLM with the check-in in its history.
func TestCheckinEndToEnd(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 20, 8, 0, 0, 0, loc))

	s.Tick()
	if got := s.WaitSends(1); got[0] != "Good morning, Sam. How did you sleep?" {
		t.Fatalf("check-in = %q", got[0])
	}

	s.Post(map[string]any{"event_type": "message.finalized", "payload": map[string]any{
		"id": "out-1", "parts": 1, "to": []any{map[string]any{"status": "delivered"}},
		"cost": map[string]any{"amount": "0.0098", "currency": "USD"},
	}})
	got := s.Rows(`SELECT m.status, d.status FROM deliveries d JOIN messages m ON m.id = d.message_id`)
	if fmt.Sprint(got) != "[delivered | sent]" {
		t.Errorf("message and delivery status = %v", got)
	}

	s.Script(sim.Say("Nice, 7 hours is solid."))
	if reply := s.Text("slept 7 hours"); reply != "Nice, 7 hours is solid." {
		t.Errorf("reply = %q", reply)
	}
	reqs := s.Requests()
	var history []string
	for _, m := range reqs[len(reqs)-1][1:] {
		history = append(history, m.Role+": "+m.Content)
	}
	want := "[assistant: Good morning, Sam. How did you sleep? user: slept 7 hours]"
	if fmt.Sprint(history) != want {
		t.Errorf("history = %v\nwant %s", history, want)
	}

	// The same minute again sends nothing more.
	s.Tick()
	time.Sleep(100 * time.Millisecond)
	if sends := s.Sends(); len(sends) != 2 {
		t.Errorf("sends = %q", sends)
	}
}
