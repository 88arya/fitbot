package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/88arya/spotter/internal/db/store"
	"github.com/88arya/spotter/internal/llm"
	"github.com/88arya/spotter/internal/telnyx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const systemPrompt = `You are Spotter, an accountability assistant that talks to people over SMS.
Keep replies short and plain: one or two sentences, no markdown, no emoji.`

const fallbackReply = "Sorry, I can't think right now. Try again in a few minutes."

// historyLen is how many recent messages are sent to the model with each request.
const historyLen = 20

// SMS holds what the Telnyx webhook needs. A nil LLM sends the fallback reply.
type SMS struct {
	Store           *store.Queries
	Telnyx          *telnyx.Client
	LLM             *llm.Client
	AllowedSenders  []string
	DefaultTimezone string
	// Now returns the current time; nil means time.Now. Tests set it.
	Now func() time.Time
}

func (s *SMS) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// localTime tells the model the user's date and time, which it cannot know
// on its own.
func localTime(now time.Time, tz string) string {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return "The user's local time is " + now.In(loc).Format("Monday, January 2, 2006, 3:04 PM") + " (" + loc.String() + ")."
}

func (s *SMS) handleTelnyx(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Telnyx.Verify(r.Header, body, time.Now()); err != nil {
		slog.Warn("rejected telnyx webhook", "err", err)
		webhookEvents.WithLabelValues("unknown", "rejected").Inc()
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	e, err := telnyx.ParseEvent(body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	switch e.Type {
	case telnyx.EventReceived:
		s.handleInbound(w, r, e)
	case telnyx.EventSent, telnyx.EventFinalized:
		s.handleDelivery(w, r, e)
	default:
		webhookEvents.WithLabelValues(e.Type, "ignored").Inc()
		w.WriteHeader(http.StatusOK)
	}
}

func (s *SMS) handleInbound(w http.ResponseWriter, r *http.Request, e telnyx.Event) {
	if !slices.Contains(s.AllowedSenders, e.From) {
		slog.Info("ignored text from unlisted sender", "from", e.From)
		webhookEvents.WithLabelValues(e.Type, "unlisted_sender").Inc()
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()
	user, err := s.Store.UpsertUser(ctx, store.UpsertUserParams{Phone: e.From, Timezone: s.DefaultTimezone})
	if err != nil {
		s.fail(w, e, "storing user", err)
		return
	}
	msg, err := s.Store.InsertInbound(ctx, store.InsertInboundParams{
		UserID:     user.ID,
		Body:       e.Text,
		ProviderID: pgtype.Text{String: e.ID, Valid: true},
		Parts:      parts(e.Parts),
		CostUsd:    numeric(e.Cost),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("ignored duplicate webhook", "id", e.ID)
		webhookEvents.WithLabelValues(e.Type, "duplicate").Inc()
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		s.fail(w, e, "storing inbound text", err)
		return
	}

	// Acknowledge before calling the LLM so Telnyx does not time out and retry.
	w.WriteHeader(http.StatusOK)
	webhookEvents.WithLabelValues(e.Type, "ok").Inc()
	smsMessages.WithLabelValues("inbound", "received").Inc()
	if f, ok := parseUSD(e.Cost); ok {
		smsCost.WithLabelValues("inbound").Add(f)
	}
	slog.Info("text received", "id", e.ID, "from", e.From, "message_id", msg.ID)

	go s.reply(context.WithoutCancel(ctx), user, msg)
}

func (s *SMS) handleDelivery(w http.ResponseWriter, r *http.Request, e telnyx.Event) {
	n, err := s.Store.UpdateDelivery(r.Context(), store.UpdateDeliveryParams{
		ProviderID: pgtype.Text{String: e.ID, Valid: true},
		Status:     e.Status,
		Parts:      parts(e.Parts),
		CostUsd:    numeric(e.Cost),
	})
	if err != nil {
		s.fail(w, e, "storing delivery report", err)
		return
	}
	w.WriteHeader(http.StatusOK)
	if n == 0 {
		// Texts sent outside Spotter, like ones sent by hand from the API.
		slog.Debug("delivery report for unknown message", "id", e.ID)
		webhookEvents.WithLabelValues(e.Type, "unknown_message").Inc()
		return
	}
	webhookEvents.WithLabelValues(e.Type, "ok").Inc()

	// Only the final report is counted, so a text is never counted twice.
	if e.Type == telnyx.EventFinalized {
		smsDeliveries.WithLabelValues(e.Status).Inc()
		if f, ok := parseUSD(e.Cost); ok {
			smsCost.WithLabelValues("outbound").Add(f)
		}
	}
	slog.Info("delivery report", "id", e.ID, "type", e.Type, "status", e.Status, "cost_usd", e.Cost)
}

// fail logs err and answers 500 so Telnyx retries the webhook later.
func (s *SMS) fail(w http.ResponseWriter, e telnyx.Event, what string, err error) {
	slog.Error(what, "id", e.ID, "err", err)
	webhookEvents.WithLabelValues(e.Type, "error").Inc()
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *SMS) reply(ctx context.Context, user store.User, in store.Message) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	text := fallbackReply
	if s.LLM != nil {
		if out, err := s.complete(ctx, user, in); err == nil {
			text = out
		}
	}

	out, err := s.Store.InsertOutbound(ctx, store.InsertOutboundParams{UserID: user.ID, Body: text})
	if err != nil {
		slog.Error("storing outbound text", "to", user.Phone, "err", err)
		return
	}
	id, err := s.Telnyx.Send(ctx, user.Phone, text)
	if err != nil {
		slog.Error("reply failed", "to", user.Phone, "message_id", out.ID, "err", err)
		smsMessages.WithLabelValues("outbound", "failed").Inc()
		if err := s.Store.MarkOutboundFailed(ctx, out.ID); err != nil {
			slog.Error("marking text failed", "message_id", out.ID, "err", err)
		}
		return
	}
	if err := s.Store.MarkOutboundSent(ctx, store.MarkOutboundSentParams{
		ID:         out.ID,
		ProviderID: pgtype.Text{String: id, Valid: true},
	}); err != nil {
		slog.Error("marking text sent", "message_id", out.ID, "err", err)
	}
	smsMessages.WithLabelValues("outbound", "sent").Inc()
	slog.Info("reply sent", "id", id, "to", user.Phone, "message_id", out.ID)
}

// complete asks the LLM for a reply to the conversation so far and records the call.
func (s *SMS) complete(ctx context.Context, user store.User, in store.Message) (string, error) {
	history, err := s.Store.RecentMessages(ctx, store.RecentMessagesParams{UserID: user.ID, Limit: historyLen})
	if err != nil {
		slog.Error("loading history", "user_id", user.ID, "err", err)
		return "", err
	}
	msgs := []llm.Message{{Role: "system", Content: systemPrompt + "\n\n" + localTime(s.now(), user.Timezone)}}
	for _, m := range history {
		switch {
		case m.Direction == "inbound":
			msgs = append(msgs, llm.Message{Role: "user", Content: m.Body})
		case m.Status != "failed" && m.Status != "sending":
			msgs = append(msgs, llm.Message{Role: "assistant", Content: m.Body})
		}
	}

	model := s.LLM.Model()
	start := time.Now()
	out, usage, err := s.LLM.Complete(ctx, msgs)
	latency := time.Since(start)

	call := store.InsertLLMCallParams{
		MessageID:    in.ID,
		Model:        model,
		InputTokens:  int32(usage.InputTokens),
		OutputTokens: int32(usage.OutputTokens),
		LatencyMs:    int32(latency.Milliseconds()),
	}
	cost, priced := s.LLM.Cost(usage)
	if priced {
		call.CostUsd = numeric(strconv.FormatFloat(cost, 'f', 8, 64))
		llmCost.WithLabelValues(model).Add(cost)
	}
	result := "ok"
	if err != nil {
		result = "error"
		call.Error = pgtype.Text{String: err.Error(), Valid: true}
	}
	if dbErr := s.Store.InsertLLMCall(ctx, call); dbErr != nil {
		slog.Error("storing llm call", "message_id", in.ID, "err", dbErr)
	}

	llmCalls.WithLabelValues(model, result).Inc()
	llmLatency.WithLabelValues(model).Observe(latency.Seconds())
	llmTokens.WithLabelValues(model, "input").Add(float64(usage.InputTokens))
	llmTokens.WithLabelValues(model, "output").Add(float64(usage.OutputTokens))

	if err != nil {
		slog.Error("llm call failed", "message_id", in.ID, "model", model, "err", err)
		return "", err
	}
	slog.Info("llm call", "message_id", in.ID, "model", model, "latency_ms", latency.Milliseconds(),
		"history", len(history), "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens,
		"cost_usd", cost)
	return out, nil
}

func parts(n int) pgtype.Int4 {
	return pgtype.Int4{Int32: int32(n), Valid: n > 0}
}

// numeric converts a decimal string like "0.0025" to a NUMERIC, or NULL when
// it is empty or malformed.
func numeric(s string) pgtype.Numeric {
	var n pgtype.Numeric
	if s == "" || n.Scan(s) != nil {
		return pgtype.Numeric{}
	}
	return n
}

func parseUSD(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && s != ""
}
