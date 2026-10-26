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
	"sync/atomic"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/db"
	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/llm"
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

// seq makes uniq distinct within this test binary even when the clock has
// not moved, as on Windows, where readings are coarse.
var seq atomic.Int64

// uniq returns a number for test phone numbers and message IDs that differs
// on every call and, with high odds, from earlier runs on the same database.
func uniq() int64 {
	return time.Now().UnixNano()/1000 + seq.Add(1)
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
		Pool:            pool,
		Store:           q,
		LLM:             llm.New(srv.URL, "key", "test-model", 1, 2),
		DefaultTimezone: "America/Toronto",
		Now:             func() time.Time { return time.Date(2026, 10, 3, 16, 5, 0, 0, time.UTC) },
	}}
}

// newUser returns a fresh user who has been sent the intro, as Greet does.
func (e *env) newUser() store.User {
	e.t.Helper()
	user, err := e.q.UpsertUser(e.ctx, fmt.Sprintf("+1888%07d", uniq()%10_000_000))
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
		ProviderID: pgtype.Text{String: fmt.Sprintf("agent-in-%d", uniq()), Valid: true},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	reply, err := e.agent.Reply(e.ctx, user.ID, in)
	return user, reply, err
}

// goals returns the user's latest goals as metric=value pairs, null for
// metrics not tracked.
func (e *env) goals(userID int64) string {
	e.t.Helper()
	gs, err := e.q.LatestGoals(e.ctx, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, g := range gs {
		v := "null"
		if g.Value.Valid {
			v = fmt.Sprint(g.Value.Int32)
		}
		out = append(out, g.Metric+"="+v)
	}
	return strings.Join(out, " ")
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
	if err != nil || reply != "Nice to meet you, Arya. "+question(2) {
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
	if !strings.Contains(sys, "Still missing: wake time and bedtime;") {
		t.Errorf("prompt should only be missing the steps after timezone:\n%s", sys)
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

// question is setup step i's question for a user who has answered none of it.
func question(i int) string {
	return fields[i].question(setupState{})
}

func TestSetup(t *testing.T) {
	gotIt := `{"role":"assistant","content":"Got it."}`
	e := newEnv(t,
		calls("set_profile", `{"name":"Arya","timezone":null,"wake_time":null,"bedtime":null}`), gotIt,
		`{"role":"assistant","content":"I'm here to keep you on track."}`,
		calls("set_profile", `{"name":null,"timezone":"America/Toronto","wake_time":null,"bedtime":null}`), gotIt,
		calls("set_profile", `{"name":null,"timezone":null,"wake_time":"6:30","bedtime":null}`), gotIt,
		calls("set_profile", `{"name":null,"timezone":null,"wake_time":null,"bedtime":"23:00"}`), gotIt,
		calls("set_goals", `{"goals":[{"metric":"water","value":3,"unit":"l"}]}`), gotIt,
		calls("set_goals", `{"goals":[{"metric":"calories","value":2200,"unit":null},{"metric":"protein","value":160,"unit":"g"}]}`), gotIt,
		calls("set_goals", `{"goals":[{"metric":"sleep","value":null,"unit":null}]}`), gotIt,
		calls("set_goals", `{"goals":[{"metric":"weight","value":null,"unit":null}]}`), gotIt,
		`{"role":"assistant","content":"Hey Arya."}`,
	)
	user := e.newUser()
	for _, step := range []struct{ in, want string }{
		{"Arya", "Got it. " + question(1)},
		// An off-topic text is answered, and the same question asked again.
		{"what do you do", "I'm here to keep you on track. " + question(1)},
		{"toronto", "Got it. " + question(2)},
		// A partial answer asks only for the rest.
		{"up at 6:30", "Got it. And what time do you usually go to bed?"},
		{"11pm", "Got it. " + question(3)},
		{"3 litres of water", `Got it. And your daily calorie goal? Say "none" if you don't want to track it.`},
		{"2200 cal, 160g protein", "Got it. " + question(4)},
		{"none", "Got it. " + question(5)},
		{"none", "Got it. " + setupDone},
		{"hey", "Hey Arya."},
	} {
		_, reply, err := e.text(user, step.in)
		if err != nil || reply != step.want {
			t.Errorf("%q: reply=%q err=%v, want %q", step.in, reply, err, step.want)
		}
	}

	user, _ = e.q.GetUser(e.ctx, user.ID)
	if got := clock(time.Duration(user.WakeTime.Microseconds)*time.Microsecond, "15:04") + " " +
		clock(time.Duration(user.Bedtime.Microseconds)*time.Microsecond, "15:04"); got != "06:30 23:00" {
		t.Errorf("wake and bedtime = %s", got)
	}
	if got := e.goals(user.ID); got != "calories=2200 protein=160 sleep=null water=3000 weight=null" {
		t.Errorf("goals = %s", got)
	}

	sys := e.llm.requests[0][0].Content
	for _, want := range []string{"Still missing: name; timezone; wake time and bedtime;", "assume America/Toronto",
		"save the default: wake time 08:00, bedtime 22:00.", "Do not ask", "calories: not set"} {
		if !strings.Contains(sys, want) {
			t.Errorf("setup prompt lacks %q:\n%s", want, sys)
		}
	}
	sys = e.llm.requests[len(e.llm.requests)-1][0].Content
	if strings.Contains(sys, "Setup is in progress") {
		t.Errorf("prompt after setup still mentions it:\n%s", sys)
	}
	for _, want := range []string{"wake time: 6:30 AM", "bedtime: 11:00 PM", "calories: 2,200 cal a day", "protein: 160 g a day",
		"carbs: not set", "water: 3 L a day", "sleep: not tracking", "target weight: not tracking", "below 1,200 cal a day"} {
		if !strings.Contains(sys, want) {
			t.Errorf("prompt after setup lacks %q:\n%s", want, sys)
		}
	}
}

func TestIntroWhenUserTextsFirst(t *testing.T) {
	e := newEnv(t)
	user, err := e.q.UpsertUser(e.ctx, fmt.Sprintf("+1666%07d", uniq()%10_000_000))
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
		calls("set_profile", `{"name":null,"timezone":"Mars/Base","wake_time":null,"bedtime":null}`,
			"set_profile", `{"name":"  ","timezone":null,"wake_time":null,"bedtime":null}`,
			"remember", `{"fact":"  "}`,
			"set_profile", `{"name":null,"timezone":null,"wake_time":"7am","bedtime":null}`,
			"set_profile", `{"name":null,"timezone":null,"wake_time":null,"bedtime":"24:00"}`,
			"set_profile", `{"name":null,"timezone":null,"wake_time":null,"bedtime":null}`),
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
	if len(results) != 6 {
		t.Fatalf("tool results = %v", results)
	}
	for _, r := range results {
		if !strings.HasPrefix(r, "error:") {
			t.Errorf("invalid call accepted: %q", r)
		}
	}
	user, _ = e.q.GetUser(e.ctx, user.ID)
	if user.Name.Valid || user.Timezone.Valid || user.WakeTime.Valid || user.Bedtime.Valid || len(e.memories(user.ID)) != 0 {
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
	user, err := e.q.UpsertUser(e.ctx, fmt.Sprintf("+1777%07d", uniq()%10_000_000))
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
