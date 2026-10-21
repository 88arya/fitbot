package scheduler_test

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
	"sync"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/agent"
	"github.com/88arya/fitbot/internal/db"
	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/llm"
	"github.com/88arya/fitbot/internal/scheduler"
	"github.com/88arya/fitbot/internal/server"
	"github.com/88arya/fitbot/internal/telnyx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCheckinEndToEnd sends a morning check-in through the Telnyx client,
// records its delivery report through the webhook, and checks the user's
// reply reaches the LLM with the check-in in its history.
func TestCheckinEndToEnd(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("scheduler_e2e_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var sends []string
	var requests [][]llm.Message
	telnyxAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Text string }
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		sends = append(sends, req.Text)
		n := len(sends)
		mu.Unlock()
		fmt.Fprintf(w, `{"data":{"id":"out-%d"}}`, n)
	}))
	t.Cleanup(telnyxAPI.Close)
	openAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []llm.Message }
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requests = append(requests, req.Messages)
		mu.Unlock()
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Nice, 7 hours is solid."}}],
			"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
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

	const phone = "+15550001234"
	loc, _ := time.LoadLocation("America/Toronto")
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, loc)
	if _, err := pool.Exec(ctx, `INSERT INTO users (phone, timezone, name) VALUES ($1, 'America/Toronto', 'Sam')`, phone); err != nil {
		t.Fatal(err)
	}
	q := store.New(pool)
	sms := &server.SMS{
		Store:  q,
		Telnyx: tc,
		Agent: &agent.Agent{
			Store:           q,
			LLM:             llm.New(openAI.URL, "key", "test-model", 1, 2),
			DefaultTimezone: "America/Toronto",
			Now:             func() time.Time { return now },
		},
		AllowedSenders: []string{phone},
	}
	h := server.New(pool, sms)
	post := func(event map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"data": event})
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/telnyx", bytes.NewReader(body))
		req.Header.Set("Telnyx-Timestamp", ts)
		req.Header.Set("Telnyx-Signature-Ed25519",
			base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(ts+"|"+string(body)))))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook %v: %d", event["event_type"], rec.Code)
		}
	}
	waitSends := func(n int) []string {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			mu.Lock()
			s := append([]string(nil), sends...)
			mu.Unlock()
			if len(s) >= n {
				return s
			}
		}
		t.Fatalf("timed out waiting for %d sends", n)
		return nil
	}

	s := &scheduler.Scheduler{
		Pool:       pool,
		Sender:     tc,
		Recipients: []string{phone},
		Checkins:   []scheduler.Checkin{scheduler.MorningCheckin},
		Now:        func() time.Time { return now },
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := waitSends(1); got[0] != "Good morning, Sam. How did you sleep?" {
		t.Fatalf("check-in = %q", got[0])
	}

	post(map[string]any{"event_type": "message.finalized", "payload": map[string]any{
		"id": "out-1", "parts": 1, "to": []any{map[string]any{"status": "delivered"}},
		"cost": map[string]any{"amount": "0.0098", "currency": "USD"},
	}})
	var status, deliveryStatus string
	if err := pool.QueryRow(ctx, `SELECT m.status, d.status FROM deliveries d JOIN messages m ON m.id = d.message_id`).
		Scan(&status, &deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || deliveryStatus != "sent" {
		t.Errorf("message status %q, delivery status %q", status, deliveryStatus)
	}

	post(map[string]any{"event_type": "message.received", "payload": map[string]any{
		"id": "in-1", "text": "slept 7 hours", "parts": 1,
		"from": map[string]any{"phone_number": phone},
	}})
	if got := waitSends(2); got[1] != "Nice, 7 hours is solid." {
		t.Errorf("reply = %q", got[1])
	}
	mu.Lock()
	last := requests[len(requests)-1]
	mu.Unlock()
	var history []string
	for _, m := range last[1:] {
		history = append(history, m.Role+": "+m.Content)
	}
	want := "[assistant: Good morning, Sam. How did you sleep? user: slept 7 hours]"
	if fmt.Sprint(history) != want {
		t.Errorf("history = %v\nwant %s", history, want)
	}

	// The same minute again sends nothing more.
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(sends) != 2 {
		t.Errorf("sends = %q", sends)
	}
}
