package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/88arya/spotter/internal/db"
	"github.com/88arya/spotter/internal/db/store"
	"github.com/88arya/spotter/internal/llm"
	"github.com/jackc/pgx/v5/pgtype"
)

// fakeLLM answers each request with the next scripted message and records
// what it was sent.
type fakeLLM struct {
	mu       sync.Mutex
	script   []string
	requests [][]llm.Message
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct{ Messages []llm.Message }
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req.Messages)
	msg := `{"role":"assistant","content":"done"}`
	if len(f.script) > 0 {
		msg, f.script = f.script[0], f.script[1:]
	}
	fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`, msg)
}

// calls builds a scripted assistant message that calls tools, given
// alternating names and JSON arguments.
func calls(nameArgs ...string) string {
	var tcs []string
	for i := 0; i < len(nameArgs); i += 2 {
		args, _ := json.Marshal(nameArgs[i+1])
		tcs = append(tcs, fmt.Sprintf(`{"id":"c%d","type":"function","function":{"name":%q,"arguments":%s}}`, i, nameArgs[i], args))
	}
	return `{"role":"assistant","content":null,"tool_calls":[` + strings.Join(tcs, ",") + `]}`
}

type env struct {
	t     *testing.T
	ctx   context.Context
	q     *store.Queries
	agent *Agent
	llm   *fakeLLM
}

func newEnv(t *testing.T, script ...string) *env {
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
	f := &fakeLLM{script: script}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	q := store.New(pool)
	return &env{t: t, ctx: ctx, q: q, llm: f, agent: &Agent{
		Store:           q,
		LLM:             llm.New(srv.URL, "key", "test-model", 1, 2),
		DefaultTimezone: "America/Toronto",
		Now:             func() time.Time { return time.Date(2026, 10, 3, 16, 5, 0, 0, time.UTC) },
	}}
}

// newUser returns a fresh user who has been sent the intro, as Greet does.
func (e *env) newUser() store.User {
	e.t.Helper()
	user, err := e.q.UpsertUser(e.ctx, fmt.Sprintf("+1888%07d", time.Now().UnixNano()%10_000_000))
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.q.InsertOutbound(e.ctx, store.InsertOutboundParams{UserID: user.ID, Body: Intro()}); err != nil {
		e.t.Fatal(err)
	}
	return user
}

// text stores an inbound text from a fresh user (or the given one) and
// returns the agent's reply.
func (e *env) text(user store.User, body string) (store.User, string, error) {
	e.t.Helper()
	if user.ID == 0 {
		user = e.newUser()
	}
	in, err := e.q.InsertInbound(e.ctx, store.InsertInboundParams{
		UserID:     user.ID,
		Body:       body,
		ProviderID: pgtype.Text{String: fmt.Sprintf("in-%d", time.Now().UnixNano()), Valid: true},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	reply, err := e.agent.Reply(e.ctx, user.ID, in)
	return user, reply, err
}

func (e *env) memories(userID int64) []string {
	e.t.Helper()
	ms, err := e.q.ListMemories(e.ctx, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	var facts []string
	for _, m := range ms {
		facts = append(facts, m.Fact)
	}
	return facts
}

func TestToolsUpdateProfileAndMemory(t *testing.T) {
	e := newEnv(t,
		calls("set_profile", `{"name":"Arya","timezone":"America/Vancouver"}`,
			"remember", `{"fact":"Works as a doctor"}`),
		`{"role":"assistant","content":"Nice to meet you, Arya."}`,
	)
	user, reply, err := e.text(store.User{}, "I'm Arya, a doctor in Vancouver")
	if err != nil || reply != "Nice to meet you, Arya. "+setupDone {
		t.Fatalf("reply=%q err=%v", reply, err)
	}

	user, err = e.q.GetUser(e.ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if user.Name.String != "Arya" || user.Timezone.String != "America/Vancouver" {
		t.Errorf("profile = %q %q", user.Name.String, user.Timezone.String)
	}
	if got := e.memories(user.ID); fmt.Sprint(got) != "[Works as a doctor]" {
		t.Errorf("memories = %v", got)
	}

	// The second request carries the tool results and a prompt rebuilt from
	// the saved profile and memory.
	second := e.llm.requests[1]
	sys := second[0].Content
	for _, want := range []string{"name: Arya", "timezone: America/Vancouver", "Works as a doctor",
		"Saturday, October 3, 2026, 9:05 AM (America/Vancouver)"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "Still missing") {
		t.Errorf("complete profile still asks for fields:\n%s", sys)
	}
	var results []string
	for _, m := range second {
		if m.Role == "tool" {
			results = append(results, m.Content)
		}
	}
	if len(results) != 2 || results[0] != "saved" || !strings.HasPrefix(results[1], "saved as #") {
		t.Errorf("tool results = %v", results)
	}
}

func TestSetup(t *testing.T) {
	e := newEnv(t,
		calls("set_profile", `{"name":"Arya","timezone":null}`),
		`{"role":"assistant","content":"Got it."}`,
		`{"role":"assistant","content":"I'm here to keep you on track."}`,
		calls("set_profile", `{"name":null,"timezone":"America/Toronto"}`),
		`{"role":"assistant","content":"Got it."}`,
		`{"role":"assistant","content":"Hey Arya."}`,
	)
	user := e.newUser()
	for _, step := range []struct{ in, want string }{
		{"Arya", "Got it. " + fields[1].question},
		// An off-topic text is answered, and the same question asked again.
		{"what do you do", "I'm here to keep you on track. " + fields[1].question},
		{"toronto", "Got it. " + setupDone},
		{"hey", "Hey Arya."},
	} {
		_, reply, err := e.text(user, step.in)
		if err != nil || reply != step.want {
			t.Errorf("%q: reply=%q err=%v, want %q", step.in, reply, err, step.want)
		}
	}

	sys := e.llm.requests[0][0].Content
	for _, want := range []string{"Still missing: name, timezone", "assume America/Toronto", "Do not ask"} {
		if !strings.Contains(sys, want) {
			t.Errorf("setup prompt lacks %q:\n%s", want, sys)
		}
	}
	if sys := e.llm.requests[len(e.llm.requests)-1][0].Content; strings.Contains(sys, "Setup is in progress") {
		t.Errorf("prompt after setup still mentions it:\n%s", sys)
	}
}

func TestIntroWhenUserTextsFirst(t *testing.T) {
	e := newEnv(t)
	user, err := e.q.UpsertUser(e.ctx, fmt.Sprintf("+1666%07d", time.Now().UnixNano()%10_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if _, reply, err := e.text(user, "hello?"); err != nil || reply != Intro() {
		t.Errorf("reply=%q err=%v, want the intro", reply, err)
	}
	if n := len(e.llm.requests); n != 0 {
		t.Errorf("LLM calls = %d, want 0", n)
	}
}

func TestToolValidation(t *testing.T) {
	e := newEnv(t,
		calls("set_profile", `{"name":null,"timezone":"Mars/Base"}`,
			"set_profile", `{"name":"  ","timezone":null}`,
			"remember", `{"fact":"  "}`),
	)
	user, _, err := e.text(store.User{}, "x")
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, m := range e.llm.requests[1] {
		if m.Role == "tool" {
			results = append(results, m.Content)
		}
	}
	if len(results) != 3 {
		t.Fatalf("tool results = %v", results)
	}
	for _, r := range results {
		if !strings.HasPrefix(r, "error:") {
			t.Errorf("invalid call accepted: %q", r)
		}
	}
	user, _ = e.q.GetUser(e.ctx, user.ID)
	if user.Name.Valid || user.Timezone.Valid || len(e.memories(user.ID)) != 0 {
		t.Errorf("invalid calls changed data: %+v", user)
	}
}

func TestMemoryIsPerUser(t *testing.T) {
	e := newEnv(t, calls("remember", `{"fact":"Has a dog"}`))
	owner, _, err := e.text(store.User{}, "I have a dog")
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := e.q.ListMemories(e.ctx, owner.ID)
	if len(ms) != 1 {
		t.Fatalf("memories = %v", ms)
	}
	id := ms[0].ID

	// Another user cannot change or delete it by guessing its id.
	e.llm.script = []string{calls("update_memory", fmt.Sprintf(`{"id":%d,"fact":"hacked"}`, id), "forget", fmt.Sprintf(`{"id":%d}`, id))}
	if _, _, err := e.text(store.User{}, "x"); err != nil {
		t.Fatal(err)
	}
	if got := e.memories(owner.ID); fmt.Sprint(got) != "[Has a dog]" {
		t.Errorf("other user changed memory: %v", got)
	}
}

func TestMemoryCap(t *testing.T) {
	e := newEnv(t)
	user, err := e.q.UpsertUser(e.ctx, fmt.Sprintf("+1777%07d", time.Now().UnixNano()%10_000_000))
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxMemories {
		if _, err := e.q.InsertMemory(e.ctx, store.InsertMemoryParams{UserID: user.ID, Fact: fmt.Sprint("fact ", i)}); err != nil {
			t.Fatal(err)
		}
	}
	e.llm.script = []string{calls("remember", `{"fact":"one too many"}`)}
	if _, _, err := e.text(user, "x"); err != nil {
		t.Fatal(err)
	}
	if n := len(e.memories(user.ID)); n != maxMemories {
		t.Errorf("memories = %d, want cap %d", n, maxMemories)
	}
}

func TestToolLoopIsBounded(t *testing.T) {
	var script []string
	for range maxRounds + 1 {
		script = append(script, calls("remember", `{"fact":"again"}`))
	}
	e := newEnv(t, script...)
	if _, _, err := e.text(store.User{}, "x"); err == nil {
		t.Error("endless tool calls did not fail")
	}
	if n := len(e.llm.requests); n != maxRounds {
		t.Errorf("LLM calls = %d, want %d", n, maxRounds)
	}
}
