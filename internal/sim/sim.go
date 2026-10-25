// Package sim runs Fitbot end to end for tests: a real Postgres schema,
// the real webhook, agent, and scheduler, with Telnyx and OpenAI faked and
// the clock under the test's control. A test scripts what the LLM answers,
// sends texts, and checks every reply and row.
package sim

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

// Phone is the simulated user's number.
const Phone = "+15550001234"

type Sim struct {
	t     *testing.T
	Ctx   context.Context
	Pool  *pgxpool.Pool
	Store *store.Queries
	Agent *agent.Agent
	// Scheduler sends Checkins on the simulated clock.
	Scheduler *scheduler.Scheduler
	// UserID is the simulated user, set up with name Sam and the start
	// time's timezone, so setup is already done.
	UserID int64

	handler http.Handler
	priv    ed25519.PrivateKey

	mu       sync.Mutex
	now      time.Time
	script   []string
	requests [][]llm.Message
	sends    []string
	inbound  int
	// lastText is the last inbound webhook, so Retry can send it again.
	lastText map[string]any
}

// New starts a simulation at start, in a fresh schema that is dropped when
// the test ends. start's location is the user's timezone, so it must be an
// IANA zone, not time.Local. It skips the test when DATABASE_URL is not set.
func New(t *testing.T, start time.Time) *Sim {
	t.Helper()
	if start.Location() == time.Local {
		t.Fatal("sim.New: start needs an IANA location, not time.Local")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("sim_%d", time.Now().UnixNano())
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

	s := &Sim{t: t, Ctx: ctx, Pool: pool, Store: store.New(pool), now: start}
	telnyxAPI := httptest.NewServer(http.HandlerFunc(s.serveTelnyx))
	t.Cleanup(telnyxAPI.Close)
	openAI := httptest.NewServer(http.HandlerFunc(s.serveOpenAI))
	t.Cleanup(openAI.Close)

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	s.priv = priv
	tc, err := telnyx.New("key", base64.StdEncoding.EncodeToString(pub), "+12505550100")
	if err != nil {
		t.Fatal(err)
	}
	tc.APIURL = telnyxAPI.URL

	if err := pool.QueryRow(ctx, `INSERT INTO users (phone, timezone, name) VALUES ($1, $2, 'Sam') RETURNING id`,
		Phone, start.Location().String()).Scan(&s.UserID); err != nil {
		t.Fatal(err)
	}
	s.Agent = &agent.Agent{
		Pool:            pool,
		Store:           s.Store,
		LLM:             llm.New(openAI.URL, "key", "test-model", 1, 2),
		DefaultTimezone: start.Location().String(),
		Now:             s.Now,
	}
	s.handler = server.New(pool, &server.SMS{
		Store:          s.Store,
		Telnyx:         tc,
		Agent:          s.Agent,
		AllowedSenders: []string{Phone},
	})
	s.Scheduler = &scheduler.Scheduler{
		Pool:       pool,
		Sender:     tc,
		Recipients: []string{Phone},
		Checkins:   []scheduler.Checkin{scheduler.MorningCheckin},
		Now:        s.Now,
	}
	return s
}

func (s *Sim) serveTelnyx(w http.ResponseWriter, r *http.Request) {
	var req struct{ Text string }
	json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	s.sends = append(s.sends, req.Text)
	n := len(s.sends)
	s.mu.Unlock()
	fmt.Fprintf(w, `{"data":{"id":"out-%d"}}`, n)
}

func (s *Sim) serveOpenAI(w http.ResponseWriter, r *http.Request) {
	var req struct{ Messages []llm.Message }
	json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req.Messages)
	msg := Say("ok")
	if len(s.script) > 0 {
		msg, s.script = s.script[0], s.script[1:]
	}
	fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`, msg)
}

// Now is the simulated time.
func (s *Sim) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// Set moves the simulated clock to t.
func (s *Sim) Set(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = t
}

// Tick runs one scheduler tick at the simulated time.
func (s *Sim) Tick() {
	s.t.Helper()
	if err := s.Scheduler.Tick(s.Ctx); err != nil {
		s.t.Fatal(err)
	}
}

// Say is an LLM answer that replies with text.
func Say(text string) string {
	b, _ := json.Marshal(text)
	return `{"role":"assistant","content":` + string(b) + `}`
}

// Call is an LLM answer that calls tools, given alternating names and JSON
// arguments.
func Call(nameArgs ...string) string {
	var tcs []string
	for i := 0; i < len(nameArgs); i += 2 {
		args, _ := json.Marshal(nameArgs[i+1])
		tcs = append(tcs, fmt.Sprintf(`{"id":"call_%d","type":"function","function":{"name":%q,"arguments":%s}}`,
			i/2, nameArgs[i], args))
	}
	return `{"role":"assistant","content":null,"tool_calls":[` + strings.Join(tcs, ",") + `]}`
}

// Script queues what the LLM answers to its next requests, in order. Once
// the script runs out it answers "ok".
func (s *Sim) Script(answers ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = append(s.script, answers...)
}

// Requests returns every request the LLM has been sent.
func (s *Sim) Requests() [][]llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]llm.Message(nil), s.requests...)
}

// Sends returns every text Fitbot has sent, oldest first.
func (s *Sim) Sends() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sends...)
}

// Post sends a signed Telnyx webhook event and fails the test unless it is
// accepted.
func (s *Sim) Post(event map[string]any) {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{"data": event})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telnyx", bytes.NewReader(body))
	req.Header.Set("Telnyx-Timestamp", ts)
	req.Header.Set("Telnyx-Signature-Ed25519",
		base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, []byte(ts+"|"+string(body)))))
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		s.t.Fatalf("webhook %v: %d", event["event_type"], rec.Code)
	}
}

// Text sends body from the user through the webhook and returns Fitbot's
// reply once it is stored as sent, so the next text sees it in history.
func (s *Sim) Text(body string) string {
	s.t.Helper()
	s.mu.Lock()
	s.inbound++
	event := map[string]any{"event_type": "message.received", "payload": map[string]any{
		"id": fmt.Sprintf("in-%d", s.inbound), "text": body, "parts": 1,
		"from": map[string]any{"phone_number": Phone},
	}}
	s.lastText = event
	n := len(s.sends)
	s.mu.Unlock()
	s.Post(event)
	reply := s.WaitSends(n + 1)[n]
	s.waitStored()
	return reply
}

// waitStored waits until no outbound text is still being sent. A reply
// reaches the fake Telnyx before it is marked sent, and history leaves out
// texts still sending.
func (s *Sim) waitStored() {
	s.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var n int
		if err := s.Pool.QueryRow(s.Ctx, `SELECT count(*) FROM messages
			WHERE user_id = $1 AND direction = 'outbound' AND status = 'sending'`, s.UserID).Scan(&n); err != nil {
			s.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	s.t.Fatal("timed out waiting for the reply to be marked sent")
}

// Retry sends the last text's webhook again, as Telnyx does when it does
// not hear back in time.
func (s *Sim) Retry() {
	s.t.Helper()
	s.mu.Lock()
	event := s.lastText
	s.mu.Unlock()
	if event == nil {
		s.t.Fatal("Retry before any Text")
	}
	s.Post(event)
}

// Rerun runs the agent again on the last text, as a run interrupted by a
// crash would be, and returns the reply it would send.
func (s *Sim) Rerun() string {
	s.t.Helper()
	var in store.Message
	if err := s.Pool.QueryRow(s.Ctx, `SELECT id, body FROM messages
		WHERE user_id = $1 AND direction = 'inbound' ORDER BY id DESC LIMIT 1`, s.UserID).
		Scan(&in.ID, &in.Body); err != nil {
		s.t.Fatal(err)
	}
	reply, err := s.Agent.Reply(s.Ctx, s.UserID, in)
	if err != nil {
		s.t.Fatal(err)
	}
	return reply
}

// WaitSends waits until Fitbot has sent n texts and returns them all.
func (s *Sim) WaitSends(n int) []string {
	s.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if sends := s.Sends(); len(sends) >= n {
			return sends
		}
	}
	s.t.Fatalf("timed out waiting for %d sends, got %q", n, s.Sends())
	return nil
}

// Rows runs query and returns each row as its values joined by " | ", for
// comparing against expected rows.
func (s *Sim) Rows(query string, args ...any) []string {
	s.t.Helper()
	rows, err := s.Pool.Query(s.Ctx, query, args...)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			s.t.Fatal(err)
		}
		cols := make([]string, len(vals))
		for i, v := range vals {
			cols[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(cols, " | "))
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}
