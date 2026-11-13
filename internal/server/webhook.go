package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

	replies sync.WaitGroup
	// mu guards recovering, which holds a channel for each user whose
	// unanswered texts Recover is still answering, closed when done.
	mu         sync.Mutex
	recovering map[int64]chan struct{}
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

	s.replies.Go(func() {
		s.waitRecovery(user.ID)
		s.reply(context.WithoutCancel(ctx), user, msg)
	})
}

// WaitReplies waits for replies in progress and reports whether they all
// finished before ctx ended. Shutdown needs this because a text is
// acknowledged before its reply, so Telnyx won't send it again.
func (s *SMS) WaitReplies(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		s.replies.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
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
	// Answered even if the send failed or the reply ran out of time: the
	// agent's changes are made, and running it again after a restart would
	// only send the same reply.
	mark, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	if err := s.Store.MarkInbound(mark, store.MarkInboundParams{ID: in.ID, Status: agent.TextAnswered}); err != nil {
		slog.Error("marking text answered", "message_id", in.ID, "err", err)
	}
}

// missedLen is how much of a missed text the notice quotes.
const missedLen = 60

// missedFor is how long after a text arrives a missed one is still worth
// a notice; older ones are marked missed quietly.
const missedFor = 24 * time.Hour

// markTimeout bounds marking a text answered, which runs even after the
// reply's own deadline.
const markTimeout = 5 * time.Second

// Recover answers texts left without a reply by a crash or a restart
// mid-reply, since Telnyx won't send them again. It lists them now, so call
// it before taking webhooks, and answers them in the background, each
// user's in order and users in parallel, which WaitReplies waits for. A
// new text from a user waits until their recovered texts are done, so
// replies keep their order and "undo" reverses the right change. A text
// too late to answer (see agent.Late) is marked missed instead, and the
// user gets one notice quoting the last of theirs from the past day, so
// they know to send it again. Tool calls are keyed on the message, so a
// change the first run already made isn't made twice.
func (s *SMS) Recover(ctx context.Context) error {
	texts, err := s.Store.UnansweredTexts(ctx, s.AllowedSenders)
	if err != nil {
		return fmt.Errorf("listing unanswered texts: %w", err)
	}
	if len(texts) == 0 {
		return nil
	}
	slog.Info("answering texts left unanswered", "count", len(texts))
	byUser := map[int64][]store.Message{}
	var users []int64
	for _, m := range texts {
		if byUser[m.UserID] == nil {
			users = append(users, m.UserID)
		}
		byUser[m.UserID] = append(byUser[m.UserID], m)
	}
	ctx = context.WithoutCancel(ctx)
	for _, id := range users {
		// A second Recover for a user still recovering waits for the first.
		wait := s.recovery(id)
		done := make(chan struct{})
		s.mu.Lock()
		if s.recovering == nil {
			s.recovering = map[int64]chan struct{}{}
		}
		s.recovering[id] = done
		s.mu.Unlock()
		s.replies.Go(func() {
			defer s.recovered(id, done)
			if wait != nil {
				<-wait
			}
			s.recover(ctx, id, byUser[id])
		})
	}
	return nil
}

// recovery is closed once the user's recovered texts are answered, or nil
// when none are being answered.
func (s *SMS) recovery(userID int64) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recovering[userID]
}

// recovered ends a recovery for a user, letting their new texts through
// unless a later one is still running.
func (s *SMS) recovered(userID int64, done chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(done)
	if s.recovering[userID] == done {
		delete(s.recovering, userID)
	}
}

// waitRecovery waits until the user's recovered texts are answered.
func (s *SMS) waitRecovery(userID int64) {
	if done := s.recovery(userID); done != nil {
		<-done
	}
}

// recover answers one user's unanswered texts, oldest first, after a
// notice for any missed.
func (s *SMS) recover(ctx context.Context, userID int64, texts []store.Message) {
	user, err := s.Store.GetUser(ctx, userID)
	if err != nil {
		slog.Error("loading user", "user_id", userID, "err", err)
		return
	}
	now := time.Now()
	if s.Agent != nil {
		now = s.Agent.Clock()
	}
	var missed, answer []store.Message
	for _, m := range texts {
		late := now.Sub(m.CreatedAt.Time) > agent.MaxLate
		if s.Agent != nil {
			if late, err = s.Agent.Late(ctx, m.UserID, m.CreatedAt.Time); err != nil {
				slog.Error("checking unanswered text", "message_id", m.ID, "err", err)
				continue
			}
		}
		if !late {
			answer = append(answer, m)
			continue
		}
		if err := s.Store.MarkInbound(ctx, store.MarkInboundParams{ID: m.ID, Status: agent.TextMissed}); err != nil {
			slog.Error("marking text missed", "message_id", m.ID, "err", err)
			continue
		}
		slog.Warn("text missed", "message_id", m.ID)
		if now.Sub(m.CreatedAt.Time) <= missedFor {
			missed = append(missed, m)
		}
	}
	if len(missed) > 0 {
		s.send(ctx, user, missedText(missed))
	}
	for _, m := range answer {
		slog.Info("answering text late", "message_id", m.ID)
		s.reply(ctx, user, m)
	}
}

// missedText tells the user texts were missed, quoting the last, like
// `Sorry, I was offline and missed your text "drank 500ml". Send it again
// if it still applies.`
func missedText(texts []store.Message) string {
	quote := strings.Join(strings.Fields(texts[len(texts)-1].Body), " ")
	if r := []rune(quote); len(r) > missedLen {
		// Cut at the last space that keeps it short, unless that loses most
		// of it.
		cut := string(r[:missedLen])
		if i := strings.LastIndex(cut, " "); utf8.RuneCountInString(cut[:i+1]) > missedLen/2 {
			cut = cut[:i]
		}
		quote = cut + "..."
	}
	switch {
	case len(texts) == 1 && quote == "":
		// A picture with no words.
		return "Sorry, I was offline and missed a text you sent. Send it again if it still applies."
	case len(texts) == 1:
		return fmt.Sprintf("Sorry, I was offline and missed your text \"%s\". Send it again if it still applies.", quote)
	case quote == "":
		return fmt.Sprintf("Sorry, I was offline and missed %d of your texts. Send again anything that still applies.", len(texts))
	}
	return fmt.Sprintf("Sorry, I was offline and missed %d of your texts, the latest \"%s\". "+
		"Send again anything that still applies.", len(texts), quote)
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
