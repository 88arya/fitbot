package llm

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCost(t *testing.T) {
	c := New("", "", "m", 0.75, 4.50)
	usd, ok := c.Cost(Usage{InputTokens: 43, OutputTokens: 14})
	if !ok || math.Abs(usd-0.00009525) > 1e-12 {
		t.Errorf("Cost = %v, %v", usd, ok)
	}

	if _, ok := New("", "", "m", 0, 0).Cost(Usage{InputTokens: 1}); ok {
		t.Error("unpriced model reported a cost")
	}
}

func TestCompleteRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "key", "m", 0, 0)
	c.Backoff = time.Millisecond
	msg, usage, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hey"}}, nil)
	if err != nil || msg.Content != "hi" || usage.InputTokens != 5 || calls.Load() != 3 {
		t.Errorf("msg=%+v usage=%+v err=%v calls=%d", msg, usage, err, calls.Load())
	}
}

func TestCompleteDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, "key", "m", 0, 0)
	c.Backoff = time.Millisecond
	if _, _, err := c.Complete(context.Background(), nil, nil); err == nil || calls.Load() != 1 {
		t.Errorf("err=%v calls=%d", err, calls.Load())
	}
}

func TestCompleteToolCalls(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sent)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"remember","arguments":"{\"fact\":\"Likes tea\"}"}}]}}]}`))
	}))
	defer srv.Close()

	tool := Tool{Name: "remember", Description: "d", Parameters: map[string]any{"type": "object"}}
	msg, _, err := New(srv.URL, "key", "m", 0, 0).Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, []Tool{tool})
	if err != nil || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"fact":"Likes tea"}` {
		t.Fatalf("msg=%+v err=%v", msg, err)
	}
	tools, _ := sent["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["strict"] != true {
		t.Errorf("tools sent = %v", sent["tools"])
	}
}

func TestWith(t *testing.T) {
	c := New("http://x", "k", "a", 1, 2).With("b", 3, 4)
	in, out := c.Prices()
	if c.Model() != "b" || in != 3 || out != 4 {
		t.Errorf("With: %s %v %v", c.Model(), in, out)
	}
}

func TestModelExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models/gone":
			http.NotFound(w, r)
		case "/models/broken":
			http.Error(w, "down", http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "a", 0, 0)
	for model, want := range map[string]bool{"here": true, "gone": false, "broken": true} {
		if got := c.ModelExists(context.Background(), model); got != want {
			t.Errorf("ModelExists(%s) = %v", model, got)
		}
	}
}
