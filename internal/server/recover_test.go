package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/llm"
)

// TestRecover restarts just after midnight in Toronto with texts left
// unanswered: one from two days ago, one from 10 PM, one from 6 minutes
// before (but yesterday), and two from the last few minutes. The old ones
// are marked missed, with one notice for the two from the past day, and
// the recent ones are answered in order, each seeing only the conversation
// up to it. Another number's text and answered texts are left alone.
func TestRecover(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	loc, _ := time.LoadLocation("America/Toronto")
	now := time.Date(2026, 10, 3, 0, 4, 0, 0, loc)
	h.sms.Agent.Now = func() time.Time { return now }

	user, err := h.sms.Store.UpsertUser(ctx, h.phone)
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.sms.Store.UpsertUser(ctx, fmt.Sprintf("+1888%07d", uniq()%10_000_000))
	if err != nil {
		t.Fatal(err)
	}
	insert := func(userID int64, dir, body, status string, at time.Time) int64 {
		t.Helper()
		var id int64
		if err := h.pool.QueryRow(ctx, `INSERT INTO messages (user_id, direction, body, status, created_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, userID, dir, body, status, at).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, loc) }
	insert(user.ID, "inbound", "hi", "answered", at(1, 9, 0))
	insert(user.ID, "outbound", "hello", "delivered", at(1, 9, 1))
	ids := []int64{
		insert(user.ID, "inbound", "two days ago", "received", at(1, 10, 0)),
		insert(user.ID, "inbound", "drank 500ml", "received", at(2, 22, 0)),
		insert(user.ID, "inbound", "  ate a big lunch today, about 900 calories with 50 grams of protein and some fries  ",
			"received", at(2, 23, 58)),
		insert(user.ID, "inbound", "first", "received", at(3, 0, 2)),
		insert(user.ID, "inbound", "second", "received", at(3, 0, 3)),
	}
	otherID := insert(other.ID, "inbound", "not allowed", "received", at(3, 0, 1))

	if err := h.sms.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !h.sms.WaitReplies(wait) {
		t.Fatal("timed out waiting for recovery")
	}
	sent := h.fakes.sent()
	if len(sent) != 3 {
		t.Fatalf("sent = %q", sent)
	}
	if want := `Sorry, I was offline and missed 2 of your texts, the latest "ate a big lunch today, about 900 calories with 50 grams of...". ` +
		`Send again anything that still applies.`; sent[0] != want {
		t.Errorf("notice = %q\nwant %q", sent[0], want)
	}
	if !strings.HasPrefix(sent[1], "reply 1") || !strings.HasPrefix(sent[2], "reply 2") {
		t.Errorf("replies = %q", sent[1:])
	}

	// Each reply's request has the conversation up to its own text, without
	// the missed ones.
	h.fakes.mu.Lock()
	requests := append([][]llm.Message(nil), h.fakes.requests...)
	h.fakes.mu.Unlock()
	for i, want := range []string{"[user:hi assistant:hello user:first]", "[user:hi assistant:hello user:first user:second]"} {
		var got []string
		for _, m := range requests[i][1:] {
			got = append(got, m.Role+":"+m.Content)
		}
		if fmt.Sprint(got) != want {
			t.Errorf("request %d history = %v, want %s", i+1, got, want)
		}
	}

	var statuses []string
	for _, id := range append(ids, otherID) {
		var s string
		if err := h.pool.QueryRow(ctx, `SELECT status FROM messages WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, s)
	}
	if got := strings.Join(statuses, " "); got != "missed missed missed answered answered received" {
		t.Errorf("statuses = %s", got)
	}

	// Nothing is left to answer after.
	if err := h.sms.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.sms.WaitReplies(wait) {
		t.Fatal("timed out")
	}
	if s := h.fakes.sent(); len(s) != 3 {
		t.Errorf("sent again: %q", s[3:])
	}
}

// TestRecoverOneMissed checks the notice for a single missed text, and
// that a text missed over a day ago gets none.
func TestRecoverOneMissed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user, err := h.sms.Store.UpsertUser(ctx, h.phone)
	if err != nil {
		t.Fatal(err)
	}
	// The harness's clock is Oct 3, 12:05 PM in Toronto.
	for _, c := range []struct {
		body string
		at   time.Time
	}{
		{"old", time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)},
		{"slept 7 hours", time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC)},
	} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO messages (user_id, direction, body, status, created_at)
			VALUES ($1, 'inbound', $2, 'received', $3)`, user.ID, c.body, c.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.sms.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.waitSends(1); len(s) != 1 ||
		s[0] != `Sorry, I was offline and missed your text "slept 7 hours". Send it again if it still applies.` {
		t.Errorf("sent = %q", s)
	}
}

// TestRecoverFirst checks a text arriving while a user's unanswered texts
// are being answered waits for them, so replies keep their order.
func TestRecoverFirst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user, err := h.sms.Store.UpsertUser(ctx, h.phone)
	if err != nil {
		t.Fatal(err)
	}
	// The harness's clock is Oct 3, 12:05 PM in Toronto.
	for _, c := range []struct {
		dir, body, status string
		at                time.Time
	}{
		{"inbound", "hi", "answered", time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC)},
		{"outbound", "hello", "delivered", time.Date(2026, 10, 3, 15, 1, 0, 0, time.UTC)},
		{"inbound", "left over", "received", time.Date(2026, 10, 3, 16, 3, 0, 0, time.UTC)},
	} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO messages (user_id, direction, body, status, created_at)
			VALUES ($1, $2, $3, $4, $5)`, user.ID, c.dir, c.body, c.status, c.at); err != nil {
			t.Fatal(err)
		}
	}
	// Hold the fake Telnyx so the recovered reply can't finish yet.
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	defer release()
	h.fakes.mu.Lock()
	h.fakes.hold = hold
	h.fakes.mu.Unlock()
	if err := h.sms.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if code := h.text(fmt.Sprintf("server-in-%d", uniq()), "new one"); code != 200 {
		t.Fatalf("webhook: %d", code)
	}
	time.Sleep(200 * time.Millisecond)
	h.fakes.mu.Lock()
	n := len(h.fakes.requests)
	h.fakes.mu.Unlock()
	if n != 1 {
		t.Errorf("LLM calls while recovering = %d, want 1 (the new text waits)", n)
	}
	release()
	sent := h.waitSends(2)
	h.fakes.mu.Lock()
	requests := append([][]llm.Message(nil), h.fakes.requests...)
	h.fakes.mu.Unlock()
	if len(requests) != 2 || requests[0][len(requests[0])-1].Content != "left over" ||
		requests[1][len(requests[1])-1].Content != "new one" || !strings.HasPrefix(sent[0], "reply 1") || !strings.HasPrefix(sent[1], "reply 2") {
		t.Errorf("sent = %q", sent)
	}
}
