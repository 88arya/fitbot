// Package scheduler sends the texts Fitbot sends on its own, like the
// morning check-in, at each user's local time.
//
// Each text is a row in deliveries, keyed on user, kind, local day, and
// slot, so a day's text is never sent twice. Every time comes from Now
// instead of the database clock, so tests can run weeks of fake time.
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

// Due is one text due on a local day.
type Due struct {
	Kind string
	// Slot tells apart a kind's texts on one day, like a supplement
	// reminder for each time; "" for a kind sent once a day.
	Slot string
	// At is the user's local time to send it, as time since midnight.
	At time.Duration
}

// Text is a scheduled text as it goes out.
type Text struct {
	// Body is the text; empty means it is no longer needed.
	Body string
	// Asks is what the text asks about, as JSON, stored with the delivery so
	// a short reply like "done" can answer it. nil when it asks nothing.
	Asks []byte
}

// Checkins says what to send and when. Due is asked again when a text is
// about to go out, so a text whose slot is gone or has moved is not sent.
type Checkins interface {
	// Due lists the texts due on the user's local day.
	Due(ctx context.Context, u store.User, day pgtype.Date) ([]Due, error)
	// Text builds one of the day's texts when it is sent.
	Text(ctx context.Context, u store.User, day pgtype.Date, kind, slot string) (Text, error)
}

type Scheduler struct {
	Pool   *pgxpool.Pool
	Sender Sender
	// Recipients are the only numbers texted, like the webhook's sender
	// allowlist, so removing a number stops its scheduled texts too.
	Recipients []string
	Checkins   Checkins
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

// plan makes sure every recipient with a timezone has a row for each text
// due today that is not yet more than grace late.
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
		day := localDay(now, loc)
		due, err := s.Checkins.Due(ctx, u, day)
		if err != nil {
			// One user's failure should not hold up everyone else's texts.
			slog.Error("listing due texts", "user_id", u.ID, "err", err)
			continue
		}
		for _, d := range due {
			at := atTime(day, d.At, loc)
			if now.Sub(at) > s.grace() {
				continue
			}
			if err := q.PlanDelivery(ctx, store.PlanDeliveryParams{
				UserID:      u.ID,
				Kind:        d.Kind,
				LocalDate:   day,
				Slot:        d.Slot,
				ScheduledAt: ts(at),
				Now:         ts(now),
			}); err != nil {
				slog.Error("planning delivery", "user_id", u.ID, "kind", d.Kind, "err", err)
			}
		}
	}
	return nil
}

// localDay is now's date in loc.
func localDay(now time.Time, loc *time.Location) pgtype.Date {
	y, m, d := now.In(loc).Date()
	return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}
}

// atTime is tod, time since local midnight, on day in loc.
func atTime(day pgtype.Date, tod time.Duration, loc *time.Location) time.Time {
	y, m, d := day.Time.Date()
	return time.Date(y, m, d, int(tod/time.Hour), int(tod%time.Hour/time.Minute), 0, 0, loc)
}

// deliver sends one claimed delivery. The text is stored and linked before
// Telnyx is called, so after a crash reconcile can tell whether it might
// have gone out. A database error leaves the delivery claimed for reconcile
// to settle.
func (s *Scheduler) deliver(ctx context.Context, q *store.Queries, d store.Delivery) {
	log := slog.With("delivery_id", d.ID, "kind", d.Kind, "slot", d.Slot, "user_id", d.UserID)
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
	// A text due while muted is dropped, not held until the mute ends.
	if user.MutedUntil.Valid && user.MutedUntil.Time.After(s.now()) {
		s.finish(ctx, q, d, "skipped", "muted")
		return
	}
	if !user.Timezone.Valid {
		s.finish(ctx, q, d, "skipped", "no timezone")
		return
	}
	loc, err := time.LoadLocation(user.Timezone.String)
	if err != nil {
		s.finish(ctx, q, d, "skipped", "bad timezone")
		return
	}
	// The text goes out only if it is still due then, like a gym reminder
	// after the workout is logged or the session moved.
	due, err := s.Checkins.Due(ctx, user, d.LocalDate)
	if err != nil {
		log.Error("listing due texts", "err", err)
		return
	}
	if !slices.ContainsFunc(due, func(x Due) bool {
		return x.Kind == d.Kind && x.Slot == d.Slot && atTime(d.LocalDate, x.At, loc).Equal(d.ScheduledAt.Time)
	}) {
		s.finish(ctx, q, d, "skipped", "no longer due")
		return
	}
	text, err := s.Checkins.Text(ctx, user, d.LocalDate, d.Kind, d.Slot)
	if err != nil {
		log.Error("building text", "err", err)
		return
	}
	if text.Body == "" {
		s.finish(ctx, q, d, "skipped", "nothing to send")
		return
	}
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
	providerID, err := s.Sender.Send(sendCtx, user.Phone, text.Body)
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

var errLostClaim = errors.New("delivery no longer claimed by this worker")

// store saves the outbound text and links it to the delivery in one
// transaction, or returns errLostClaim if the claim was taken over.
func (s *Scheduler) store(ctx context.Context, d store.Delivery, text Text) (store.Message, error) {
	var msg store.Message
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		msg, err = q.InsertOutbound(ctx, store.InsertOutboundParams{UserID: d.UserID, Body: text.Body})
		if err != nil {
			return err
		}
		n, err := q.LinkDeliveryMessage(ctx, store.LinkDeliveryMessageParams{
			MessageID: pgtype.Int8{Int64: msg.ID, Valid: true},
			Asks:      text.Asks,
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
