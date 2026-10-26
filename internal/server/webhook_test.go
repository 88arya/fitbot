package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/agent"
	"github.com/88arya/fitbot/internal/db"
	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/llm"
	"github.com/88arya/fitbot/internal/telnyx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakes records what Fitbot sent to Telnyx and OpenAI.
type fakes struct {
	mu       sync.Mutex
	sends    []string
	requests [][]llm.Message
}

func (f *fakes) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sends...)
}

// seq makes uniq distinct within this test binary even when the clock has
// not moved, as on Windows, where readings are coarse.
var seq atomic.Int64

// uniq returns a number for test phone numbers and message IDs that differs
// on every call and, with high odds, from earlier runs on the same database.
func uniq() int64 {
	return time.Now().UnixNano()/1000 + seq.Add(1)
}

type harness struct {
	t     *testing.T
	pool  *pgxpool.Pool
	priv  ed25519.PrivateKey
	h     http.Handler
	fakes *fakes
	phone string
	sms   *SMS
}

func newHarness(t *testing.T) *harness {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	f := &fakes{}
	telnyxAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Text string }
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.sends = append(f.sends, req.Text)
		id := fmt.Sprintf("server-out-%d", uniq())
		f.mu.Unlock()
		fmt.Fprintf(w, `{"data":{"id":%q}}`, id)
	}))
	t.Cleanup(telnyxAPI.Close)
	openAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []llm.Message }
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.requests = append(f.requests, req.Messages)
		n := len(f.requests)
		f.mu.Unlock()
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"reply %d"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":10}}`, n)
	}))
	t.Cleanup(openAI.Close)

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := telnyx.New("key", base64.StdEncoding.EncodeToString(pub), "+12505550100")
	if err != nil {
		t.Fatal(err)
	}
	tc.APIURL = telnyxAPI.URL

	// A fresh number per run keeps tests independent without wiping the database.
	phone := fmt.Sprintf("+1999%07d", uniq()%10_000_000)
	q := store.New(pool)
	sms := &SMS{
		Store:  q,
		Telnyx: tc,
		Agent: &agent.Agent{
			Pool:            pool,
			Store:           q,
			LLM:             llm.New(openAI.URL, "key", "test-model", 1, 2),
			DefaultTimezone: "America/Toronto",
			Now:             func() time.Time { return time.Date(2026, 10, 3, 16, 5, 0, 0, time.UTC) },
		},
		AllowedSenders: []string{phone},
	}
	return &harness{t: t, pool: pool, priv: priv, h: New(pool, sms), fakes: f, phone: phone, sms: sms}
}

func (h *harness) post(event map[string]any) int {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"data": event})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telnyx", bytes.NewReader(body))
	req.Header.Set("Telnyx-Timestamp", ts)
	req.Header.Set("Telnyx-Signature-Ed25519",
		base64.StdEncoding.EncodeToString(ed25519.Sign(h.priv, []byte(ts+"|"+string(body)))))
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec.Code
}

func (h *harness) text(id, body string) int {
	return h.post(map[string]any{"event_type": "message.received", "payload": map[string]any{
		"id": id, "text": body, "parts": 1,
		"from": map[string]any{"phone_number": h.phone},
		"cost": map[string]any{"amount": "0.0025", "currency": "USD"},
	}})
}

// waitSends waits until Fitbot has sent n texts.
func (h *harness) waitSends(n int) []string {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := h.fakes.sent(); len(s) >= n {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %d sends, got %v", n, h.fakes.sent())
	return nil
}

func TestWebhookConversation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inID := fmt.Sprintf("server-in-%d", uniq())

	if code := h.text(inID, "hi"); code != http.StatusOK {
		t.Fatalf("first webhook: %d", code)
	}
	if code := h.text(inID, "hi"); code != http.StatusOK {
		t.Fatalf("retried webhook: %d", code)
	}
	h.waitSends(1)
	time.Sleep(200 * time.Millisecond)
	if s := h.fakes.sent(); len(s) != 1 {
		t.Fatalf("retried webhook caused another reply: %v", s)
	}

	var outID string
	if err := h.pool.QueryRow(ctx, `SELECT m.provider_id FROM messages m JOIN users u ON u.id = m.user_id
		WHERE u.phone = $1 AND m.direction = 'outbound'`, h.phone).Scan(&outID); err != nil {
		t.Fatal(err)
	}
	if code := h.post(map[string]any{"event_type": "message.finalized", "payload": map[string]any{
		"id": outID, "parts": 1, "to": []any{map[string]any{"status": "delivered"}},
		"cost": map[string]any{"amount": "0.0098", "currency": "USD"},
	}}); code != http.StatusOK {
		t.Fatalf("delivery report: %d", code)
	}

	// A first text gets the intro without an LLM call; the second text's
	// request carries that exchange as history, and its reply asks the first
	// setup question.
	h.text(inID+"-2", "and again")
	if s := h.waitSends(2); s[0] != agent.Intro() || s[1] != "reply 1 What should I call you?" {
		t.Errorf("sent = %q", s)
	}
	h.fakes.mu.Lock()
	last := h.fakes.requests[len(h.fakes.requests)-1]
	h.fakes.mu.Unlock()
	if want := "Saturday, October 3, 2026, 12:05 PM (America/Toronto)"; !strings.Contains(last[0].Content, want) {
		t.Errorf("system prompt lacks local time %q:\n%s", want, last[0].Content)
	}
	var roles, bodies []string
	for _, m := range last[1:] {
		roles = append(roles, m.Role)
		bodies = append(bodies, m.Content)
	}
	if fmt.Sprint(roles) != "[user assistant user]" || fmt.Sprint(bodies) != fmt.Sprint([]string{"hi", agent.Intro(), "and again"}) {
		t.Errorf("history = %v %v", roles, bodies)
	}

	rows, err := h.pool.Query(ctx, `SELECT m.direction, m.status, m.cost_usd::text FROM messages m
		JOIN users u ON u.id = m.user_id WHERE u.phone = $1 ORDER BY m.id`, h.phone)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var dir, status string
		var cost *string
		if err := rows.Scan(&dir, &status, &cost); err != nil {
			t.Fatal(err)
		}
		c := "-"
		if cost != nil {
			c = *cost
		}
		got = append(got, dir+"/"+status+"/"+c)
	}
	want := "[inbound/received/0.002500 outbound/delivered/0.009800 inbound/received/0.002500 outbound/queued/-]"
	if fmt.Sprint(got) != want {
		t.Errorf("messages =\n %v\nwant\n %v", got, want)
	}

	var calls int
	var llmCost string
	if err := h.pool.QueryRow(ctx, `SELECT count(*), sum(c.cost_usd)::text FROM llm_calls c
		JOIN messages m ON m.id = c.message_id JOIN users u ON u.id = m.user_id
		WHERE u.phone = $1`, h.phone).Scan(&calls, &llmCost); err != nil {
		t.Fatal(err)
	}
	// 100 input tokens at $1/M plus 10 output at $2/M, once.
	if calls != 1 || llmCost != "0.00012000" {
		t.Errorf("llm_calls: count=%d cost=%s", calls, llmCost)
	}
}

func TestGreetOnce(t *testing.T) {
	h := newHarness(t)
	h.sms.Greet(context.Background())
	if s := h.waitSends(1); s[0] != agent.Intro() {
		t.Errorf("greeting = %q", s[0])
	}
	// A restart does not greet again.
	h.sms.Greet(context.Background())
	time.Sleep(100 * time.Millisecond)
	if s := h.fakes.sent(); len(s) != 1 {
		t.Errorf("greeted twice: %q", s)
	}
}

func TestWebhookRejectsUnsigned(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telnyx", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned webhook: %d", rec.Code)
	}
}
