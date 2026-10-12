// Package llm is a minimal client for the OpenAI chat completions API.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// ToolCalls is set on assistant messages that call tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on tool messages, naming the call they answer.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments is a JSON object encoded as a string.
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool describes a function the model may call. Parameters is a JSON schema.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

func (t Tool) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
			"strict":      true,
		},
	})
}

type Usage struct {
	InputTokens  int `json:"prompt_tokens"`
	OutputTokens int `json:"completion_tokens"`
}

type Client struct {
	baseURL string
	apiKey  string
	model   string
	// USD per 1M tokens; zero means unknown.
	inputPrice, outputPrice float64
	http                    *http.Client
	// Backoff is the wait before the first retry; it doubles after each one.
	Backoff time.Duration
}

// attempts is how many times a request is tried before giving up.
const attempts = 3

func New(baseURL, apiKey, model string, inputPrice, outputPrice float64) *Client {
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		model:       model,
		inputPrice:  inputPrice,
		outputPrice: outputPrice,
		http:        &http.Client{Timeout: 30 * time.Second},
		Backoff:     time.Second,
	}
}

func (c *Client) Model() string { return c.model }

// Cost returns what a call with usage u cost in USD, or ok=false when the
// model's prices are not configured.
func (c *Client) Cost(u Usage) (usd float64, ok bool) {
	if c.inputPrice == 0 && c.outputPrice == 0 {
		return 0, false
	}
	return (float64(u.InputTokens)*c.inputPrice + float64(u.OutputTokens)*c.outputPrice) / 1e6, true
}

// Complete returns the model's next message, which holds either a reply or
// tool calls. Rate limits, server errors, and network failures are retried
// with backoff.
func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool) (Message, Usage, error) {
	req := map[string]any{"model": c.model, "messages": messages}
	if len(tools) > 0 {
		req["tools"] = tools
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return Message{}, Usage{}, err
	}

	wait := c.Backoff
	for attempt := 1; ; attempt++ {
		msg, usage, err := c.do(ctx, payload)
		var re retryable
		if err == nil || !errors.As(err, &re) || attempt == attempts {
			return msg, usage, err
		}
		// Up to 25% jitter keeps concurrent retries from landing together.
		d := wait + time.Duration(rand.Int64N(int64(wait)/4+1))
		select {
		case <-ctx.Done():
			return Message{}, Usage{}, err
		case <-time.After(d):
		}
		wait *= 2
	}
}

// retryable marks errors worth trying again.
type retryable struct{ error }

func (r retryable) Unwrap() error { return r.error }

func (c *Client) do(ctx context.Context, payload []byte) (Message, Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Message{}, Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Message{}, Usage{}, err
		}
		return Message{}, Usage{}, retryable{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		err := fmt.Errorf("openai: %s: %s", resp.Status, msg)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return Message{}, Usage{}, retryable{err}
		}
		return Message{}, Usage{}, err
	}

	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Message{}, Usage{}, fmt.Errorf("openai: decoding response: %w", err)
	}
	if len(out.Choices) == 0 {
		return Message{}, out.Usage, errors.New("openai: no choices in response")
	}
	m := out.Choices[0].Message
	if m.Content == "" && len(m.ToolCalls) == 0 {
		return Message{}, out.Usage, errors.New("openai: empty reply")
	}
	return m, out.Usage, nil
}
