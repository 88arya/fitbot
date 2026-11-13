package agent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// patternLines are the pattern lines of the weekly summary sent on day.
func patternLines(t *testing.T, s *sim.Sim, day time.Time) []string {
	t.Helper()
	got, err := s.Agent.CheckinText(s.Ctx, s.UserID, day, "weekly_summary", "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "Pattern") {
			out = append(out, line)
		}
	}
	return out
}

// TestWeeklyPatterns seeds the 4 weeks from Mon Nov 2 to Sun Nov 29 with
// push, pull, and legs on Monday, Wednesday, and Friday and an 8-hour sleep
// goal: 8-hour nights and every session done the first two weeks, 6-hour
// nights and only Mondays done the last two. Weekdays are 2,000 cal and
// weekends 2,500, except 4,000 on the last weekend. It checks the patterns
// in the Nov 30 summary, then with the last week paused, a gap too small,
// too few nights, and short nights doing better.
func TestWeeklyPatterns(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	date := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	s := sim.New(t, time.Date(2026, 11, 2, 7, 0, 0, 0, loc))
	logged(t, s, "push mon, pull wed, legs fri; sleep 8 hours", "set_schedule",
		`{"kind":"weekly","days":[{"weekday":"mon","sessions":["push"],"time":null},{"weekday":"wed","sessions":["pull"],"time":null},`+
			`{"weekday":"fri","sessions":["legs"],"time":null}],"cycle_day_today":null,"training_time":"18:00"}`,
		"set_goals", `{"goals":[{"metric":"sleep","value":8,"unit":"hours"}]}`)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(s.Ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	var msgID, actionID int64
	if err := s.Pool.QueryRow(s.Ctx, `INSERT INTO messages (user_id, direction, body, status)
		VALUES ($1, 'inbound', 'seed', 'received') RETURNING id`, s.UserID).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(s.Ctx, `INSERT INTO actions (user_id, message_id, tool, args, occurrence, result)
		VALUES ($1, $2, 'seed', '{}', 1, '') RETURNING id`, s.UserID, msgID).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	for d := 2; d <= 29; d++ {
		day := date(d)
		at := time.Date(2026, 11, d, 12, 0, 0, 0, loc)
		cal := 2000
		switch wd := day.Weekday(); {
		case d >= 28:
			cal = 4000
		case wd == time.Saturday || wd == time.Sunday:
			cal = 2500
		}
		exec(`INSERT INTO intake_logs (user_id, action_id, message_id, local_date, metric, amount, logged_at)
			VALUES ($1, $2, $3, $4, 'calories', $5, $6)`, s.UserID, actionID, msgID, day, cal, at)
		mins := 480
		if d >= 16 {
			mins = 360
		}
		exec(`INSERT INTO sleep_logs (user_id, action_id, message_id, local_date, minutes, logged_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, s.UserID, actionID, msgID, day, mins, at)
		var session string
		switch day.Weekday() {
		case time.Monday:
			session = "push"
		case time.Wednesday:
			session = "pull"
		case time.Friday:
			session = "legs"
		}
		if session != "" && (d < 16 || day.Weekday() == time.Monday) {
			exec(`INSERT INTO workouts (user_id, action_id, message_id, local_date, status, sessions, logged_at)
				VALUES ($1, $2, $3, $4, 'done', $5, $6)`, s.UserID, actionID, msgID, day, []string{session}, at)
		}
	}
	s.Set(time.Date(2026, 11, 30, 8, 0, 0, 0, loc))
	check := func(name string, want ...string) {
		t.Helper()
		if got := patternLines(t, s, date(30)); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", name, got, want)
		}
	}

	// Weekends: 6 at 2,500 and 2 at 4,000 average 2,875; weekdays 2,000.
	check("seeded",
		"Pattern (last 4 weeks): weekend calories averaged 2,875 cal, 875 cal more than weekdays (2,000 cal).",
		"Pattern (last 4 weeks): after nights on your sleep goal you did 6 of 6 planned sessions, after shorter nights 2 of 6.")

	// Pausing the last week drops its days from both sides: the 4,000 cal
	// weekend, and Nov 23 to 27's sessions.
	logged(t, s, "i was sick last week", "set_pause", pauseCall("set", `"2026-11-23"`, `"2026-11-29"`, `"sick"`))
	check("last week paused",
		"Pattern (last 4 weeks): weekend calories averaged 2,500 cal, 500 cal more than weekdays (2,000 cal).",
		"Pattern (last 4 weeks): after nights on your sleep goal you did 6 of 6 planned sessions, after shorter nights 1 of 3.")
	exec(`DELETE FROM pauses WHERE user_id = $1`, s.UserID)

	// Weekends of 2,200 are within 250 of weekdays; and weekends below
	// weekdays read as less.
	exec(`UPDATE intake_logs SET amount = 2200 WHERE user_id = $1 AND extract(isodow FROM local_date) >= 6`, s.UserID)
	check("gap too small",
		"Pattern (last 4 weeks): after nights on your sleep goal you did 6 of 6 planned sessions, after shorter nights 2 of 6.")
	exec(`UPDATE intake_logs SET amount = 1500 WHERE user_id = $1 AND extract(isodow FROM local_date) >= 6`, s.UserID)
	check("weekends lower",
		"Pattern (last 4 weeks): weekend calories averaged 1,500 cal, 500 cal less than weekdays (2,000 cal).",
		"Pattern (last 4 weeks): after nights on your sleep goal you did 6 of 6 planned sessions, after shorter nights 2 of 6.")

	// Exactly 4 weekend days logged are enough; 3 aren't.
	exec(`DELETE FROM intake_logs WHERE user_id = $1 AND extract(isodow FROM local_date) >= 6 AND local_date > '2026-11-15'`, s.UserID)
	check("exactly 4 weekend days",
		"Pattern (last 4 weeks): weekend calories averaged 1,500 cal, 500 cal less than weekdays (2,000 cal).",
		"Pattern (last 4 weeks): after nights on your sleep goal you did 6 of 6 planned sessions, after shorter nights 2 of 6.")
	exec(`DELETE FROM intake_logs WHERE user_id = $1 AND local_date = '2026-11-08'`, s.UserID)
	check("too few weekend days",
		"Pattern (last 4 weeks): after nights on your sleep goal you did 6 of 6 planned sessions, after shorter nights 2 of 6.")

	// Exactly 3 short nights on training days are enough; 2 aren't.
	exec(`DELETE FROM sleep_logs WHERE user_id = $1 AND local_date IN ('2026-11-18', '2026-11-20', '2026-11-23')`, s.UserID)
	check("exactly 3 short nights",
		"Pattern (last 4 weeks): after nights on your sleep goal you did 6 of 6 planned sessions, after shorter nights 1 of 3.")
	exec(`DELETE FROM sleep_logs WHERE user_id = $1 AND local_date = '2026-11-16'`, s.UserID)
	check("too few short nights")

	// Short nights doing as well isn't shown: with Nov 23's night back and
	// Nov 25 and 27 done, short nights are 3 of 3.
	exec(`INSERT INTO workouts (user_id, action_id, message_id, local_date, status, sessions, logged_at)
		VALUES ($1, $2, $3, '2026-11-25', 'done', '{pull}', now()), ($1, $2, $3, '2026-11-27', 'done', '{legs}', now())`,
		s.UserID, actionID, msgID)
	exec(`INSERT INTO sleep_logs (user_id, action_id, message_id, local_date, minutes, logged_at)
		VALUES ($1, $2, $3, '2026-11-23', 360, now())`, s.UserID, actionID, msgID)
	check("short nights doing as well")

	// A gap of exactly 25 points is shown: with Nov 20's missed legs back,
	// short nights are 3 of 4, against 6 of 6.
	exec(`INSERT INTO sleep_logs (user_id, action_id, message_id, local_date, minutes, logged_at)
		VALUES ($1, $2, $3, '2026-11-20', 360, now())`, s.UserID, actionID, msgID)
	check("exactly 25 points",
		"Pattern (last 4 weeks): after nights on your sleep goal you did 6 of 6 planned sessions, after shorter nights 3 of 4.")

	// Under 25 points isn't: 4 of 5 is 20 points short of 6 of 6.
	exec(`INSERT INTO sleep_logs (user_id, action_id, message_id, local_date, minutes, logged_at)
		VALUES ($1, $2, $3, '2026-11-18', 360, now())`, s.UserID, actionID, msgID)
	exec(`INSERT INTO workouts (user_id, action_id, message_id, local_date, status, sessions, logged_at)
		VALUES ($1, $2, $3, '2026-11-18', 'done', '{pull}', now())`, s.UserID, actionID, msgID)
	check("20 points")
}
