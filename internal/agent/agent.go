// Package agent turns a user's text into a reply, using the LLM and tools
// that update the user's profile and memory.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/88arya/spotter/internal/db"
	"github.com/88arya/spotter/internal/db/store"
	"github.com/88arya/spotter/internal/llm"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// historyLen is how many recent messages are sent with each request.
	historyLen = 20
	// maxRounds caps LLM calls per text, so a model stuck calling tools stops.
	maxRounds = 5
	// maxMemories caps stored facts so the list stays short enough to read every time.
	maxMemories = 30
)

type Agent struct {
	Store           *store.Queries
	LLM             *llm.Client
	DefaultTimezone string
	// Now returns the current time; nil means time.Now. Tests set it.
	Now func() time.Time
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Reply returns the text to send back for in, the user's latest message,
// which must already be stored. Tool calls along the way are applied to the
// database.
func (a *Agent) Reply(ctx context.Context, userID int64, in store.Message) (string, error) {
	history, err := a.Store.RecentMessages(ctx, store.RecentMessagesParams{UserID: userID, Limit: historyLen})
	if err != nil {
		return "", fmt.Errorf("loading history: %w", err)
	}
	var convo []llm.Message
	for _, m := range history {
		switch {
		case m.Direction == "inbound":
			convo = append(convo, llm.Message{Role: "user", Content: m.Body})
		case m.Status != "failed" && m.Status != "sending":
			convo = append(convo, llm.Message{Role: "assistant", Content: m.Body})
		}
	}
	first := len(history) == 1

	for range maxRounds {
		// The prompt is rebuilt each round so it shows changes made by tools.
		user, err := a.Store.GetUser(ctx, userID)
		if err != nil {
			return "", fmt.Errorf("loading user: %w", err)
		}
		memories, err := a.Store.ListMemories(ctx, userID)
		if err != nil {
			return "", fmt.Errorf("loading memories: %w", err)
		}
		sys := llm.Message{Role: "system", Content: a.systemPrompt(user, memories, first)}

		out, err := a.complete(ctx, in.ID, append([]llm.Message{sys}, convo...))
		if err != nil {
			return "", err
		}
		if len(out.ToolCalls) == 0 {
			return out.Content, nil
		}
		convo = append(convo, out)
		for _, tc := range out.ToolCalls {
			convo = append(convo, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: a.runTool(ctx, userID, tc)})
		}
	}
	return "", errors.New("too many tool rounds without a reply")
}

// complete makes one LLM call and records it.
func (a *Agent) complete(ctx context.Context, inID int64, msgs []llm.Message) (llm.Message, error) {
	model := a.LLM.Model()
	start := time.Now()
	out, usage, err := a.LLM.Complete(ctx, msgs, tools)
	latency := time.Since(start)

	call := store.InsertLLMCallParams{
		MessageID:    inID,
		Model:        model,
		InputTokens:  int32(usage.InputTokens),
		OutputTokens: int32(usage.OutputTokens),
		LatencyMs:    int32(latency.Milliseconds()),
	}
	cost, priced := a.LLM.Cost(usage)
	if priced {
		call.CostUsd = db.Numeric(strconv.FormatFloat(cost, 'f', 8, 64))
		llmCost.WithLabelValues(model).Add(cost)
	}
	result := "ok"
	if err != nil {
		result = "error"
		call.Error = pgtype.Text{String: err.Error(), Valid: true}
	}
	if dbErr := a.Store.InsertLLMCall(ctx, call); dbErr != nil {
		slog.Error("storing llm call", "message_id", inID, "err", dbErr)
	}

	llmCalls.WithLabelValues(model, result).Inc()
	llmLatency.WithLabelValues(model).Observe(latency.Seconds())
	llmTokens.WithLabelValues(model, "input").Add(float64(usage.InputTokens))
	llmTokens.WithLabelValues(model, "output").Add(float64(usage.OutputTokens))

	if err != nil {
		slog.Error("llm call failed", "message_id", inID, "model", model, "err", err)
		return llm.Message{}, err
	}
	slog.Info("llm call", "message_id", inID, "model", model, "latency_ms", latency.Milliseconds(),
		"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens,
		"tool_calls", len(out.ToolCalls), "cost_usd", cost)
	return out, nil
}

const basePrompt = `You are Spotter, an accountability assistant that texts with people over SMS.
Keep replies short and plain: one or two sentences, no markdown, no emoji.`

func (a *Agent) systemPrompt(user store.User, memories []store.Memory, first bool) string {
	var b strings.Builder
	b.WriteString(basePrompt)

	tz := a.DefaultTimezone
	var missing []string
	b.WriteString("\n\n## Profile\n")
	if user.Name.Valid {
		fmt.Fprintf(&b, "name: %s\n", user.Name.String)
	} else {
		b.WriteString("name: missing\n")
		missing = append(missing, "name")
	}
	if user.Timezone.Valid {
		tz = user.Timezone.String
		fmt.Fprintf(&b, "timezone: %s\n", tz)
	} else {
		fmt.Fprintf(&b, "timezone: missing (times below assume %s)\n", tz)
		missing = append(missing, "timezone")
	}
	if budget, ok := usd(user.MonthlyBudgetUsd); ok {
		fmt.Fprintf(&b, "monthly budget: $%.2f\n", budget)
	} else {
		b.WriteString("monthly budget: missing\n")
		missing = append(missing, "monthly budget")
	}
	b.WriteString("The monthly budget is what the user wants to spend each month on running Spotter (texts and AI), in USD.\n")
	if len(missing) > 0 {
		fmt.Fprintf(&b, "\nStill missing: %s. Ask for these one at a time, woven into the conversation, "+
			"and save each answer with set_profile. Convert a city or region to its IANA timezone.\n", strings.Join(missing, ", "))
		if first {
			b.WriteString("This is the user's first message: start with a one-sentence introduction of yourself.\n")
		}
	}
	b.WriteString("The user can change any field later, like \"set my budget to 20\"; save changes with set_profile.\n")

	b.WriteString("\n## Memory\nFacts the user has told you, by id:\n")
	if len(memories) == 0 {
		b.WriteString("(none yet)\n")
	}
	for _, m := range memories {
		fmt.Fprintf(&b, "#%d %s\n", m.ID, m.Fact)
	}
	fmt.Fprintf(&b, "Save lasting facts worth knowing later (work, goals, routines, preferences, important people) with remember. "+
		"Skip small talk and anything already in the profile. If a fact changes or replaces one above, call update_memory "+
		"with its id instead of remember. Call forget when a fact stops being true or the user asks. At most %d facts.\n", maxMemories)

	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	fmt.Fprintf(&b, "\n## Time\nThe user's local time is %s (%s).",
		a.now().In(loc).Format("Monday, January 2, 2006, 3:04 PM"), loc)
	return b.String()
}

func usd(n pgtype.Numeric) (float64, bool) {
	f, err := n.Float64Value()
	return f.Float64, err == nil && f.Valid
}
