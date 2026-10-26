// Package scheduler sends the texts Fitbot sends on its own, like the
// morning check-in, at each user's local time.
//
// Each text is a row in deliveries, keyed on user, kind, and local day, so a
// day is never sent twice. Every time comes from Now instead of the database
// clock, so tests can run weeks of fake time.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/telnyx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultInterval = 30 * time.Second
	defaultGrace    = 30 * time.Minute
	// retryDelay and maxAttempts apply to sends Telnyx turned away without
	// sending.
	retryDelay  = time.Minute
	maxAttempts = 3
	// staleAfter is how long a claimed delivery may stay unfinished before it
	// is treated as abandoned by a crashed worker. Well past sendTimeout.
	staleAfter  = 2 * time.Minute
	sendTimeout = 30 * time.Second
	batchSize   = 50
)

// Sender sends a text and returns the provider's message ID.
type Sender interface {
	Send(ctx context.Context, to, body string) (string, error)
}

// Times of day used for users who have not given their own.
const (
	DefaultWake    = 8 * time.Hour
	DefaultBedtime = 22 * time.Hour
)

// WakeTime is the user's wake time as time since midnight, or DefaultWake.
func WakeTime(u store.User) time.Duration {
	return timeOfDay(u.WakeTime, DefaultWake)
}

// Bedtime is the user's bedtime as time since midnight, or DefaultBedtime.
func Bedtime(u store.User) time.Duration {
	return timeOfDay(u.Bedtime, DefaultBedtime)
}

func timeOfDay(t pgtype.Time, def time.Duration) time.Duration {
	if !t.Valid {
		return def
	}
	return time.Duration(t.Microseconds) * time.Microsecond
}

// Checkin is a text sent once a day at a local time.
type Checkin struct {
	Kind string
	// At is the user's local time to send it, as time since midnight.
	At   func(store.User) time.Duration
	Text func(store.User) string
}

// MorningCheckin goes out at the user's wake time. Its text is a placeholder
// until goals and the training schedule exist to fill it in.
var MorningCheckin = Checkin{
	Kind: "morning_checkin",
	At:   WakeTime,
	Text: func(u store.User) string {
		if u.Name.Valid {
			return "Good morning, " + u.Name.String + ". How did you sleep?"
		}
		return "Good morning. How did you sleep?"
	},
}

type Scheduler struct {
	Pool   *pgxpool.Pool
	Sender Sender
	// Recipients are the only numbers texted, like the webhook's sender
	// allowlist, so removing a number stops its scheduled texts too.
	Recipients []string
	Checkins   []Checkin
	// Grace is how late a text may still go out; later ones are skipped.
	// Zero means 30 minutes.
	Grace time.Duration
	// Interval is how often Run checks for due texts. Zero means 30 seconds.
	Interval time.Duration
	// Now returns the current time; nil means time.Now. Tests set it.
	Now func() time.Time
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) grace() time.Duration {
	if s.Grace > 0 {
		return s.Grace
	}
	return defaultGrace
}

// Run ticks until ctx is done. A tick in progress finishes first, so a send
// is never cut off between Telnyx and the database.
func (s *Scheduler) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Tick(context.WithoutCancel(ctx)); err != nil {
			slog.Error("scheduler tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick settles abandoned deliveries, plans upcoming ones, and sends those due.
func (s *Scheduler) Tick(ctx context.Context) error {
	q := store.New(s.Pool)
	if err := s.reconcile(ctx, q); err != nil {
		return fmt.Errorf("reconciling: %w", err)
	}
	if err := s.plan(ctx, q); err != nil {
		return fmt.Errorf("planning: %w", err)
	}
	for {
		claimed, err := q.ClaimDueDeliveries(ctx, store.ClaimDueDeliveriesParams{Now: ts(s.now()), MaxRows: batchSize})
		if err != nil {
			return fmt.Errorf("claiming: %w", err)
		}
		for _, d := range claimed {
			s.deliver(ctx, q, d)
		}
		if len(claimed) < batchSize {
			return nil
		}
	}
}

func (s *Scheduler) reconcile(ctx context.Context, q *store.Queries) error {
	now := s.now()
	settled, err := q.ReconcileStaleDeliveries(ctx, store.ReconcileStaleDeliveriesParams{
		Now:    ts(now),
		Cutoff: ts(now.Add(-staleAfter)),
	})
	for _, d := range settled {
		slog.Warn("settled unfinished delivery", "delivery_id", d.ID, "kind", d.Kind, "status", d.Status)
		deliveries.WithLabelValues(d.Kind, "reconciled_"+d.Status).Inc()
	}
	return err
}

// plan makes sure every recipient with a timezone has a row for each
// check-in's next occurrence.
func (s *Scheduler) plan(ctx context.Context, q *store.Queries) error {
	users, err := q.ListUsersWithTimezone(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	for _, u := range users {
		if !slices.Contains(s.Recipients, u.Phone) {
			continue
		}
		loc, err := time.LoadLocation(u.Timezone.String)
		if err != nil {
			slog.Warn("skipping user with bad timezone", "user_id", u.ID, "timezone", u.Timezone.String)
			continue
		}
		for _, c := range s.Checkins {
			day, at := c.next(now, loc, c.At(u), s.grace())
			if err := q.PlanDelivery(ctx, store.PlanDeliveryParams{
				UserID:      u.ID,
				Kind:        c.Kind,
				LocalDate:   day,
				ScheduledAt: ts(at),
				Now:         ts(now),
			}); err != nil {
				// One user's failure should not hold up everyone else's texts.
				slog.Error("planning delivery", "user_id", u.ID, "kind", c.Kind, "err", err)
			}
		}
	}
	return nil
}

// next returns the local day and time of the check-in's next occurrence at
// tod, time since local midnight: today's if it is not yet more than grace
// late, otherwise tomorrow's.
func (c Checkin) next(now time.Time, loc *time.Location, tod, grace time.Duration) (pgtype.Date, time.Time) {
	y, m, d := now.In(loc).Date()
	hour, minute := int(tod/time.Hour), int(tod%time.Hour/time.Minute)
	at := time.Date(y, m, d, hour, minute, 0, 0, loc)
	if now.Sub(at) > grace {
		d++
		at = time.Date(y, m, d, hour, minute, 0, 0, loc)
	}
	return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}, at
}

// deliver sends one claimed delivery. The text is stored and linked before
// Telnyx is called, so after a crash reconcile can tell whether it might
// have gone out. A database error leaves the delivery claimed for reconcile
// to settle.
func (s *Scheduler) deliver(ctx context.Context, q *store.Queries, d store.Delivery) {
	log := slog.With("delivery_id", d.ID, "kind", d.Kind, "user_id", d.UserID)
	c, ok := s.checkin(d.Kind)
	if !ok {
		s.finish(ctx, q, d, "failed", "no check-in of this kind")
		return
	}
	if late := s.now().Sub(d.ScheduledAt.Time); late > s.grace() {
		log.Info("skipped late delivery", "late", late.Round(time.Second))
		s.finish(ctx, q, d, "skipped", fmt.Sprintf("late by %s", late.Round(time.Second)))
		return
	}
	user, err := q.GetUser(ctx, d.UserID)
	if err != nil {
		log.Error("loading user", "err", err)
		return
	}
	if !slices.Contains(s.Recipients, user.Phone) {
		s.finish(ctx, q, d, "skipped", "not a recipient")
		return
	}
	text := c.Text(user)
	msg, err := s.store(ctx, d, text)
	if errors.Is(err, errLostClaim) {
		log.Warn("delivery was taken over while stalled; not sending")
		return
	}
	if err != nil {
		log.Error("storing outbound text", "err", err)
		return
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	providerID, err := s.Sender.Send(sendCtx, user.Phone, text)
	cancel()
	if err != nil {
		s.sendFailed(ctx, q, d, msg, err)
		return
	}
	// A crash between these two leaves a provider ID behind, which reconcile
	// reads as sent.
	if err := q.MarkOutboundSent(ctx, store.MarkOutboundSentParams{
		ID:         msg.ID,
		ProviderID: pgtype.Text{String: providerID, Valid: true},
	}); err != nil {
		log.Error("marking text sent", "message_id", msg.ID, "err", err)
		return
	}
	s.finish(ctx, q, d, "sent", "")
	log.Info("scheduled text sent", "message_id", msg.ID, "provider_id", providerID)
}

func (s *Scheduler) checkin(kind string) (Checkin, bool) {
	for _, c := range s.Checkins {
		if c.Kind == kind {
			return c, true
		}
	}
	return Checkin{}, false
}

var errLostClaim = errors.New("delivery no longer claimed by this worker")

// store saves the outbound text and links it to the delivery in one
// transaction, or returns errLostClaim if the claim was taken over.
func (s *Scheduler) store(ctx context.Context, d store.Delivery, text string) (store.Message, error) {
	var msg store.Message
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		msg, err = q.InsertOutbound(ctx, store.InsertOutboundParams{UserID: d.UserID, Body: text})
		if err != nil {
			return err
		}
		n, err := q.LinkDeliveryMessage(ctx, store.LinkDeliveryMessageParams{
			MessageID: pgtype.Int8{Int64: msg.ID, Valid: true},
			Now:       ts(s.now()),
			ID:        d.ID,
			Attempts:  d.Attempts,
		})
		if err == nil && n == 0 {
			err = errLostClaim
		}
		return err
	})
	return msg, err
}

// sendFailed retries sends Telnyx turned away without sending, and never
// retries one that may have gone out.
func (s *Scheduler) sendFailed(ctx context.Context, q *store.Queries, d store.Delivery, msg store.Message, sendErr error) {
	log := slog.With("delivery_id", d.ID, "kind", d.Kind, "user_id", d.UserID, "message_id", msg.ID, "err", sendErr)
	switch classify(sendErr) {
	case notSent:
		if err := q.MarkOutboundFailed(ctx, msg.ID); err != nil {
			log.Error("marking text failed", "mark_err", err)
		}
		if d.Attempts < maxAttempts {
			log.Warn("scheduled send failed, will retry")
			deliveries.WithLabelValues(d.Kind, "retry").Inc()
			if err := q.RetryDelivery(ctx, store.RetryDeliveryParams{
				RunAt: ts(s.now().Add(retryDelay)),
				Error: pgtype.Text{String: sendErr.Error(), Valid: true},
				Now:   ts(s.now()),
				ID:    d.ID,
			}); err != nil {
				log.Error("rescheduling delivery", "retry_err", err)
			}
			return
		}
		log.Error("scheduled send failed, giving up")
		s.finish(ctx, q, d, "failed", sendErr.Error())
	case refused:
		if err := q.MarkOutboundFailed(ctx, msg.ID); err != nil {
			log.Error("marking text failed", "mark_err", err)
		}
		log.Error("scheduled send refused")
		s.finish(ctx, q, d, "failed", sendErr.Error())
	default:
		// The stored text stays "sending", so the agent leaves it out of
		// the conversation history.
		log.Error("scheduled send may or may not have gone out; not retrying")
		s.finish(ctx, q, d, "unknown", sendErr.Error())
	}
}

func (s *Scheduler) finish(ctx context.Context, q *store.Queries, d store.Delivery, status, errText string) {
	if err := q.FinishDelivery(ctx, store.FinishDeliveryParams{
		Status: status,
		Error:  pgtype.Text{String: errText, Valid: errText != ""},
		Now:    ts(s.now()),
		ID:     d.ID,
	}); err != nil {
		slog.Error("finishing delivery", "delivery_id", d.ID, "status", status, "err", err)
		return
	}
	deliveries.WithLabelValues(d.Kind, status).Inc()
}

type outcome int

const (
	// unknown: the text may have been sent, like after a timeout.
	unknown outcome = iota
	// notSent: Telnyx was unavailable or busy; safe to try again.
	notSent
	// refused: Telnyx rejected the text; trying again will not help.
	refused
)

func classify(err error) outcome {
	if apiErr, ok := errors.AsType[*telnyx.APIError](err); ok {
		switch {
		case apiErr.StatusCode == 429 || apiErr.StatusCode == 503:
			return notSent
		case apiErr.StatusCode < 500:
			return refused
		}
		// Other 5xx, like a gateway timeout, may have come after Telnyx
		// accepted the text.
		return unknown
	}
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op == "dial" {
		return notSent
	}
	return unknown
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
