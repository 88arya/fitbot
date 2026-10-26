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

	"github.com/88arya/fitbot/internal/agent"
	"github.com/88arya/fitbot/internal/db"
	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/telnyx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const fallbackReply = "Sorry, I can't think right now. Try again in a few minutes."

// SMS holds what the Telnyx webhook needs. A nil Agent sends the fallback
// reply, as does an Agent without an LLM for texts that need one.
type SMS struct {
	Store          *store.Queries
	Telnyx         *telnyx.Client
	Agent          *agent.Agent
	AllowedSenders []string
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
	user, err := s.Store.UpsertUser(ctx, e.From)
	if err != nil {
		s.fail(w, e, "storing user", err)
		return
	}
	msg, err := s.Store.InsertInbound(ctx, store.InsertInboundParams{
		UserID:     user.ID,
		Body:       e.Text,
		ProviderID: pgtype.Text{String: e.ID, Valid: true},
		Parts:      parts(e.Parts),
		CostUsd:    db.Numeric(e.Cost),
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
		CostUsd:    db.Numeric(e.Cost),
	})
	if err != nil {
		s.fail(w, e, "storing delivery report", err)
		return
	}
	w.WriteHeader(http.StatusOK)
	if n == 0 {
		// Texts sent outside Fitbot, like ones sent by hand from the API.
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	text := fallbackReply
	if s.Agent != nil {
		out, err := s.Agent.Reply(ctx, user.ID, in)
		switch {
		case errors.Is(err, agent.ErrNoLLM):
			slog.Warn("no LLM to answer text", "message_id", in.ID)
		case err != nil:
			slog.Error("agent failed", "message_id", in.ID, "err", err)
		default:
			text = out
		}
	}
	s.send(ctx, user, text)
}

// Greet texts the intro to allowed senders Fitbot has never texted, so setup
// starts as soon as Fitbot runs instead of waiting for their first text.
// Setup needs the LLM, so without one nobody is greeted.
func (s *SMS) Greet(ctx context.Context) {
	if s.Agent == nil || s.Agent.LLM == nil {
		return
	}
	for _, phone := range s.AllowedSenders {
		user, err := s.Store.UpsertUser(ctx, phone)
		if err != nil {
			slog.Error("storing user", "phone", phone, "err", err)
			continue
		}
		n, err := s.Store.CountMessages(ctx, user.ID)
		if err != nil {
			slog.Error("counting messages", "phone", phone, "err", err)
			continue
		}
		if n == 0 {
			s.send(ctx, user, agent.Intro())
		}
	}
}

// send stores text as an outbound message and sends it.
func (s *SMS) send(ctx context.Context, user store.User, text string) {
	out, err := s.Store.InsertOutbound(ctx, store.InsertOutboundParams{UserID: user.ID, Body: text})
	if err != nil {
		slog.Error("storing outbound text", "to", user.Phone, "err", err)
		return
	}
	id, err := s.Telnyx.Send(ctx, user.Phone, text)
	if err != nil {
		slog.Error("send failed", "to", user.Phone, "message_id", out.ID, "err", err)
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
	slog.Info("text sent", "id", id, "to", user.Phone, "message_id", out.ID)
}

func parts(n int) pgtype.Int4 {
	return pgtype.Int4{Int32: int32(n), Valid: n > 0}
}

func parseUSD(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && s != ""
}
