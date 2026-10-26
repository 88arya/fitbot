package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/db"
	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/telnyx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// clock is a fake time the test moves forward by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type sentText struct {
	to, body string
	at       time.Time
}

// fakeSender records texts, or returns the queued errors first.
type fakeSender struct {
	mu    sync.Mutex
	clock *clock
	sent  []sentText
	errs  []error
}

func (f *fakeSender) Send(_ context.Context, to, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return "", err
	}
	f.sent = append(f.sent, sentText{to, body, f.clock.now()})
	return fmt.Sprintf("out-%d", len(f.sent)), nil
}

func (f *fakeSender) to(phone string) []sentText {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentText
	for _, s := range f.sent {
		if s.to == phone {
			out = append(out, s)
		}
	}
	return out
}

type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	clock  *clock
	sender *fakeSender
	// phones are the users added so far, all of them recipients.
	phones []string
}

// newEnv migrates a fresh schema, so each test sees only its own users.
func newEnv(t *testing.T, start time.Time) *env {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("scheduler_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: start}
	return &env{t: t, ctx: ctx, pool: pool, clock: c, sender: &fakeSender{clock: c}}
}

// scheduler returns a new scheduler on the shared database and clock, as a
// restarted process would be, texting every user added so far.
func (e *env) scheduler() *Scheduler {
	return &Scheduler{
		Pool:       e.pool,
		Sender:     e.sender,
		Recipients: slices.Clone(e.phones),
		Checkins:   []Checkin{MorningCheckin},
		Now:        e.clock.now,
	}
}

// user adds a user in timezone tz, or with no timezone when tz is empty.
func (e *env) user(tz string) (int64, string) {
	e.t.Helper()
	phone := fmt.Sprintf("+1555000%04d", len(e.phones)+1)
	e.phones = append(e.phones, phone)
	var id int64
	var zone *string
	if tz != "" {
		zone = &tz
	}
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO users (phone, timezone, name) VALUES ($1, $2, 'Sam') RETURNING id`,
		phone, zone).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id, phone
}

func (e *env) tickAt(s *Scheduler, t time.Time) {
	e.t.Helper()
	e.clock.set(t)
	if err := s.Tick(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// statuses returns the user's delivery statuses, oldest day first.
func (e *env) statuses(userID int64) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT local_date::text || ' ' || status FROM deliveries
		WHERE user_id = $1 ORDER BY local_date`, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func local(tz string, y int, m time.Month, d, h, min int) time.Time {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		panic(err)
	}
	return time.Date(y, m, d, h, min, 0, 0, loc)
}

// TestDailyCheckinOverWeeks replays three weeks of fake time across a DST
// change, restarting the scheduler halfway, and expects exactly one morning
// check-in per local day at 8 for every user with a timezone.
func TestDailyCheckinOverWeeks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start time.Time
	}{
		{"fall back", time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)},
		{"spring forward", time.Date(2026, 2, 26, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.start)
			zones := []string{"America/Toronto", "Europe/London", "Asia/Kolkata", "Australia/Sydney"}
			phones := map[string]string{}
			for _, tz := range zones {
				_, phones[tz] = e.user(tz)
			}
			_, noZone := e.user("")

			// An odd step means ticks rarely land on the hour, so sends
			// come a few minutes late, within the grace window.
			const step = 11 * time.Minute
			end := tc.start.Add(21 * 24 * time.Hour)
			s := e.scheduler()
			for now := tc.start; now.Before(end); now = now.Add(step) {
				if now.Sub(tc.start) >= 10*24*time.Hour && now.Sub(tc.start) < 10*24*time.Hour+step {
					s = e.scheduler()
				}
				e.tickAt(s, now)
			}

			for _, tz := range zones {
				loc, _ := time.LoadLocation(tz)
				var want []string
				y, m, d := tc.start.In(loc).Date()
				for day := 0; day <= 22; day++ {
					at := time.Date(y, m, d+day, 8, 0, 0, 0, loc)
					if !at.Before(tc.start) && !at.After(end.Add(-step)) {
						want = append(want, at.Format("2006-01-02"))
					}
				}
				var got []string
				for _, txt := range e.sender.to(phones[tz]) {
					lt := txt.at.In(loc)
					if lt.Hour() != 8 || lt.Minute() >= int(step/time.Minute) {
						t.Errorf("%s: sent at %s local, want 8:00 to 8:%02d", tz, lt.Format("Jan 2 15:04 MST"), int(step/time.Minute))
					}
					if txt.body != "Good morning, Sam. How did you sleep?" {
						t.Errorf("%s: body %q", tz, txt.body)
					}
					got = append(got, lt.Format("2006-01-02"))
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s: sent on\n %v\nwant\n %v", tz, got, want)
				}
			}
			if n := len(e.sender.to(noZone)); n != 0 {
				t.Errorf("user without a timezone got %d texts", n)
			}
		})
	}
}

// TestMissedFire checks that a text up to the grace window late still goes
// out, and a later one is skipped instead of arriving hours late.
func TestMissedFire(t *testing.T) {
	const tz = "America/Toronto"
	e := newEnv(t, local(tz, 2026, 10, 20, 7, 0))
	id, phone := e.user(tz)
	s := e.scheduler()

	// Day 1: down from 7:00 to 8:20; sent 20 minutes late.
	e.tickAt(s, local(tz, 2026, 10, 20, 7, 0))
	e.tickAt(s, local(tz, 2026, 10, 20, 8, 20))
	// Day 2: down from 7:00 to 9:00; skipped.
	e.tickAt(s, local(tz, 2026, 10, 21, 7, 0))
	e.tickAt(s, local(tz, 2026, 10, 21, 9, 0))
	// Day 3: on time.
	e.tickAt(s, local(tz, 2026, 10, 22, 8, 0))
	// Day 4: first started at 10:00, so the day is never planned.
	e.tickAt(s, local(tz, 2026, 10, 23, 10, 0))
	e.tickAt(s, local(tz, 2026, 10, 24, 8, 0))

	var got []string
	for _, txt := range e.sender.to(phone) {
		got = append(got, txt.at.Format("Jan 2 15:04"))
	}
	if want := "[Oct 20 08:20 Oct 22 08:00 Oct 24 08:00]"; fmt.Sprint(got) != want {
		t.Errorf("sent at %v, want %s", got, want)
	}
	want := "[2026-10-20 sent 2026-10-21 skipped 2026-10-22 sent 2026-10-24 sent]"
	if got := e.statuses(id); fmt.Sprint(got) != want {
		t.Errorf("deliveries = %v\nwant %s", got, want)
	}
}

// claim is how far a crashed worker got with a delivery.
type claim int

const (
	claimedOnly  claim = iota // claimed, text not stored yet
	textStored                // text stored, Telnyx never answered
	telnyxAnswer              // Telnyx returned an ID
)

// TestCrashRecovery sets up deliveries a crashed worker left claimed and
// checks each is settled without sending anything twice.
func TestCrashRecovery(t *testing.T) {
	const tz = "America/Toronto"
	now := local(tz, 2026, 10, 20, 8, 10)
	e := newEnv(t, now)

	// claimed leaves a delivery for the user in the given state, last
	// touched at updated.
	claimed := func(userID int64, c claim, updated time.Time) {
		t.Helper()
		var msgID *int64
		if c != claimedOnly {
			var providerID *string
			if c == telnyxAnswer {
				id := fmt.Sprintf("out-before-crash-%d", userID)
				providerID = &id
			}
			var id int64
			if err := e.pool.QueryRow(e.ctx, `INSERT INTO messages (user_id, direction, body, status, provider_id)
				VALUES ($1, 'outbound', 'Good morning', 'sending', $2) RETURNING id`, userID, providerID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			msgID = &id
		}
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO deliveries
			(user_id, kind, local_date, scheduled_at, run_at, status, attempts, message_id, updated_at)
			VALUES ($1, 'morning_checkin', '2026-10-20', $2, $2, 'sending', 1, $3, $4)`,
			userID, local(tz, 2026, 10, 20, 8, 0), msgID, updated); err != nil {
			t.Fatal(err)
		}
	}
	stale := now.Add(-5 * time.Minute)

	// Crashed before storing the text: nothing went out, so it is sent now.
	beforeStore, beforeStorePhone := e.user(tz)
	claimed(beforeStore, claimedOnly, stale)
	// Crashed while calling Telnyx: it may have gone out, so it is not resent.
	midSend, midSendPhone := e.user(tz)
	claimed(midSend, textStored, stale)
	// Crashed after Telnyx answered: it went out.
	afterSend, afterSendPhone := e.user(tz)
	claimed(afterSend, telnyxAnswer, stale)
	// Another worker is sending right now: left alone.
	busy, busyPhone := e.user(tz)
	claimed(busy, claimedOnly, now)

	e.tickAt(e.scheduler(), now)
	for _, c := range []struct {
		name   string
		id     int64
		phone  string
		sends  int
		status string
	}{
		{"before store", beforeStore, beforeStorePhone, 1, "sent"},
		{"mid send", midSend, midSendPhone, 0, "unknown"},
		{"after send", afterSend, afterSendPhone, 0, "sent"},
		{"busy", busy, busyPhone, 0, "sending"},
	} {
		if n := len(e.sender.to(c.phone)); n != c.sends {
			t.Errorf("%s: %d sends, want %d", c.name, n, c.sends)
		}
		if got := e.statuses(c.id); fmt.Sprint(got) != fmt.Sprint([]string{"2026-10-20 " + c.status}) {
			t.Errorf("%s: deliveries = %v, want %s", c.name, got, c.status)
		}
	}
}

// TestLostClaim has a stalled worker try to store its text after the claim
// was taken over, and expects it to stop without storing or sending.
func TestLostClaim(t *testing.T) {
	const tz = "America/Toronto"
	e := newEnv(t, local(tz, 2026, 10, 20, 8, 0))
	id, _ := e.user(tz)
	s := e.scheduler()
	e.clock.set(local(tz, 2026, 10, 20, 7, 0))
	if err := s.plan(e.ctx, store.New(e.pool)); err != nil {
		t.Fatal(err)
	}
	e.clock.set(local(tz, 2026, 10, 20, 8, 0))
	claimed, err := store.New(e.pool).ClaimDueDeliveries(e.ctx, store.ClaimDueDeliveriesParams{Now: ts(e.clock.now()), MaxRows: 10})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed %d, err %v", len(claimed), err)
	}
	// Another worker reclaimed it after it went stale.
	if _, err := e.pool.Exec(e.ctx, `UPDATE deliveries SET attempts = attempts + 1 WHERE id = $1`, claimed[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store(e.ctx, claimed[0], "Good morning"); !errors.Is(err, errLostClaim) {
		t.Fatalf("store err = %v, want errLostClaim", err)
	}
	var msgs int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM messages WHERE user_id = $1`, id).Scan(&msgs); err != nil {
		t.Fatal(err)
	}
	if msgs != 0 {
		t.Errorf("%d messages stored, want 0", msgs)
	}
}

// TestRecipients checks that a number dropped from the allowlist gets no
// more scheduled texts, even one already planned.
func TestRecipients(t *testing.T) {
	const tz = "America/Toronto"
	e := newEnv(t, local(tz, 2026, 10, 20, 7, 0))
	planned, plannedPhone := e.user(tz)
	never, _ := e.user(tz)
	s := e.scheduler()
	s.Recipients = []string{plannedPhone}
	e.tickAt(s, local(tz, 2026, 10, 20, 7, 0))

	s.Recipients = nil
	e.tickAt(s, local(tz, 2026, 10, 20, 8, 0))
	if n := len(e.sender.to(plannedPhone)); n != 0 {
		t.Errorf("removed number got %d texts", n)
	}
	if got := e.statuses(planned); fmt.Sprint(got) != "[2026-10-20 skipped]" {
		t.Errorf("deliveries = %v, want skipped", got)
	}
	if got := e.statuses(never); len(got) != 0 {
		t.Errorf("non-recipient has deliveries %v", got)
	}
}

// TestSendFailures checks which Telnyx errors are retried.
func TestSendFailures(t *testing.T) {
	const tz = "America/Toronto"
	busy := &telnyx.APIError{StatusCode: 503, Status: "503 Service Unavailable"}
	for _, tc := range []struct {
		name      string
		errs      []error
		sends     int
		status    string
		msgStatus string
	}{
		{"busy once", []error{busy}, 1, "sent", "failed queued"},
		{"busy every time", []error{busy, busy, busy}, 0, "failed", "failed failed failed"},
		{"refused", []error{&telnyx.APIError{StatusCode: 400, Status: "400 Bad Request"}}, 0, "failed", "failed"},
		{"connection refused", []error{&net.OpError{Op: "dial", Err: errors.New("refused")}}, 1, "sent", "failed queued"},
		{"timed out", []error{context.DeadlineExceeded}, 0, "unknown", "sending"},
		{"gateway timeout", []error{&telnyx.APIError{StatusCode: 504, Status: "504 Gateway Timeout"}}, 0, "unknown", "sending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, local(tz, 2026, 10, 20, 8, 0))
			id, phone := e.user(tz)
			e.sender.errs = tc.errs
			s := e.scheduler()
			for m := 0; m <= 5; m++ {
				e.tickAt(s, local(tz, 2026, 10, 20, 8, m))
			}
			if n := len(e.sender.to(phone)); n != tc.sends {
				t.Errorf("%d sends, want %d", n, tc.sends)
			}
			if got := e.statuses(id); fmt.Sprint(got) != fmt.Sprint([]string{"2026-10-20 " + tc.status}) {
				t.Errorf("deliveries = %v, want %s", got, tc.status)
			}
			rows, err := e.pool.Query(e.ctx, `SELECT status FROM messages WHERE user_id = $1 ORDER BY id`, id)
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(msgs); got != "["+tc.msgStatus+"]" {
				t.Errorf("messages = %s, want [%s]", got, tc.msgStatus)
			}
		})
	}
}

// TestConcurrentWorkers runs several schedulers against one database at the
// same moment and expects each user to get one text.
func TestConcurrentWorkers(t *testing.T) {
	const tz = "America/Toronto"
	e := newEnv(t, local(tz, 2026, 10, 20, 8, 0))
	var phones []string
	for range 30 {
		_, p := e.user(tz)
		phones = append(phones, p)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- e.scheduler().Tick(e.ctx) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range phones {
		if n := len(e.sender.to(p)); n != 1 {
			t.Errorf("%s got %d texts, want 1", p, n)
		}
	}
}

// TestTimezoneChange moves a user before their check-in and expects it at
// 8 in the new timezone.
func TestTimezoneChange(t *testing.T) {
	e := newEnv(t, local("America/Toronto", 2026, 10, 20, 6, 0))
	id, phone := e.user("America/Toronto")
	s := e.scheduler()
	e.tickAt(s, local("America/Toronto", 2026, 10, 20, 6, 0))
	if _, err := e.pool.Exec(e.ctx, `UPDATE users SET timezone = 'America/Vancouver' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	for h := 6; h <= 12; h++ {
		e.tickAt(s, local("America/Toronto", 2026, 10, 20, h, 0))
	}
	sent := e.sender.to(phone)
	if len(sent) != 1 || !sent[0].at.Equal(local("America/Vancouver", 2026, 10, 20, 8, 0)) {
		t.Errorf("sent = %v, want one text at 8:00 Vancouver time", sent)
	}
}

// TestWakeTime sends each user's check-in at their wake time, or 8:00 if
// they gave none, and moves a planned one when the wake time changes.
func TestWakeTime(t *testing.T) {
	const tz = "America/Toronto"
	e := newEnv(t, local(tz, 2026, 10, 20, 5, 0))
	early, earlyPhone := e.user(tz)
	_, defaultPhone := e.user(tz)
	setWake := func(wake string) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, `UPDATE users SET wake_time = $2 WHERE id = $1`, early, wake); err != nil {
			t.Fatal(err)
		}
	}
	setWake("06:45")
	s := e.scheduler()
	for _, at := range []time.Time{
		local(tz, 2026, 10, 20, 5, 0),
		local(tz, 2026, 10, 20, 6, 45),
		local(tz, 2026, 10, 20, 8, 0),
		// Day 2: the wake time moves from 6:45 to 9:30 before the planned send.
		local(tz, 2026, 10, 21, 6, 0),
	} {
		e.tickAt(s, at)
	}
	setWake("09:30")
	for _, at := range []time.Time{
		local(tz, 2026, 10, 21, 6, 45),
		local(tz, 2026, 10, 21, 8, 0),
		local(tz, 2026, 10, 21, 9, 30),
	} {
		e.tickAt(s, at)
	}

	for _, c := range []struct {
		name, phone, want string
	}{
		{"early riser", earlyPhone, "[Oct 20 06:45 Oct 21 09:30]"},
		{"no wake time", defaultPhone, "[Oct 20 08:00 Oct 21 08:00]"},
	} {
		var got []string
		for _, txt := range e.sender.to(c.phone) {
			got = append(got, txt.at.Format("Jan 2 15:04"))
		}
		if fmt.Sprint(got) != c.want {
			t.Errorf("%s: sent at %v, want %s", c.name, got, c.want)
		}
	}
}
