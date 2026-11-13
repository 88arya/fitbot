package agent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// TestBedtimeReminder checks the bedtime reminder goes 30 minutes before
// the lights-out the sleep goal needs, only when the usual bedtime is too
// late for it: on its own, joined to a night check-in within 30 minutes
// and before lights-out, on its own again with that check-in off or at
// lights-out, with a bedtime and a lights-out after midnight, and not at
// all with no sleep goal, enough time, the setting off, a pause, a
// reminder past midnight, or wake time equal to bedtime. It also goes out
// through the scheduler.
func TestBedtimeReminder(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	mon := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s := sim.New(t, time.Date(2026, 11, 2, 6, 0, 0, 0, loc))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(s.Ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	bedtime := func() string {
		t.Helper()
		var out []string
		for _, d := range strings.Split(dueTexts(t, s, mon), ", ") {
			if strings.HasPrefix(d, "bedtime_reminder") || strings.HasPrefix(d, "night_checkin") {
				out = append(out, d)
			}
		}
		return strings.Join(out, ", ")
	}
	text := func(kind string) string {
		t.Helper()
		got, err := s.Agent.CheckinText(s.Ctx, s.UserID, mon, kind, "")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	const reminder = "Bedtime soon: lights out by 10:00 PM for 8 hours of sleep before your 6:00 AM wake-up."
	const line = "Lights out by 10:00 PM for 8 hours of sleep before your 6:00 AM wake-up."

	// No sleep goal: nothing.
	exec(`UPDATE users SET wake_time = '06:00', bedtime = '23:00' WHERE id = $1`, s.UserID)
	if got := bedtime(); got != "night_checkin at 22:30" {
		t.Errorf("no goal: %s", got)
	}

	// 8 hours before 6:00 AM is 10:00 PM, an hour before the usual 11:00 PM.
	exec(`UPDATE goals SET value = 480 WHERE user_id = $1 AND metric = 'sleep'`, s.UserID)
	if got := bedtime(); got != "night_checkin at 22:30, bedtime_reminder at 21:30" {
		t.Errorf("own reminder: %s", got)
	}
	if got := text("bedtime_reminder"); got != reminder {
		t.Errorf("reminder = %q", got)
	}
	if got := text("night_checkin"); strings.Contains(got, "Lights out") {
		t.Errorf("night check-in repeats it: %q", got)
	}

	// A 10:20 PM bedtime puts the night check-in at 9:50, within 30 minutes
	// of the reminder and before lights-out, so the check-in carries it.
	exec(`UPDATE users SET bedtime = '22:20' WHERE id = $1`, s.UserID)
	if got := bedtime(); got != "night_checkin at 21:50" {
		t.Errorf("joined: %s", got)
	}
	if got := text("night_checkin"); got != "Good evening, Sam. "+line {
		t.Errorf("night check-in = %q", got)
	}
	if got := text("bedtime_reminder"); got != "" {
		t.Errorf("joined reminder still sent alone: %q", got)
	}

	// With the night check-in off, it goes alone.
	updateSetting(s, "night_checkin", "off")
	if got := bedtime(); got != "bedtime_reminder at 21:30" {
		t.Errorf("night check-in off: %s", got)
	}
	updateSetting(s, "night_checkin", "default")

	// A night check-in at lights-out itself is too late to carry it.
	exec(`UPDATE users SET bedtime = '22:30' WHERE id = $1`, s.UserID)
	if got := bedtime(); got != "night_checkin at 22:00, bedtime_reminder at 21:30" {
		t.Errorf("check-in at lights-out: %s", got)
	}

	// A 10:00 PM bedtime already leaves 8 hours.
	exec(`UPDATE users SET bedtime = '22:00' WHERE id = $1`, s.UserID)
	if got := bedtime(); got != "night_checkin at 21:30" {
		t.Errorf("enough time: %s", got)
	}

	// The setting turns it off, joined or not.
	exec(`UPDATE users SET bedtime = '23:00' WHERE id = $1`, s.UserID)
	updateSetting(s, "bedtime_reminders", "off")
	if got := bedtime(); got != "night_checkin at 22:30" {
		t.Errorf("setting off: %s", got)
	}
	exec(`UPDATE users SET bedtime = '22:20' WHERE id = $1`, s.UserID)
	if got := text("night_checkin"); strings.Contains(got, "Lights out") {
		t.Errorf("setting off, still joined: %q", got)
	}
	updateSetting(s, "bedtime_reminders", "default")
	exec(`UPDATE users SET bedtime = '23:00' WHERE id = $1`, s.UserID)

	// A paused day sends none.
	logged(t, s, "sick today", "set_pause", pauseCall("set", "null", `"2026-11-02"`, "null"))
	if got := bedtime(); got != "" {
		t.Errorf("paused: %s", got)
	}
	exec(`DELETE FROM pauses WHERE user_id = $1`, s.UserID)

	// Through the scheduler, it goes at 9:30 PM.
	before := len(s.Sends())
	for _, hm := range [][2]int{{21, 0}, {21, 30}, {21, 45}} {
		s.Set(time.Date(2026, 11, 2, hm[0], hm[1], 0, 0, loc))
		s.Tick()
	}
	if sends := s.Sends()[before:]; len(sends) != 1 || sends[0] != reminder {
		t.Errorf("sent = %q", sends)
	}

	// A bedtime after midnight: 7.5 hours before 7:00 AM is 11:30 PM, an
	// hour before 12:30 AM, so it goes at 11:00 PM, here apart from a night
	// check-in set for 9:00 PM.
	exec(`UPDATE users SET wake_time = '07:00', bedtime = '00:30', night_checkin = '21:00' WHERE id = $1`, s.UserID)
	exec(`UPDATE goals SET value = 450 WHERE user_id = $1 AND metric = 'sleep'`, s.UserID)
	if got := bedtime(); got != "night_checkin at 21:00, bedtime_reminder at 23:00" {
		t.Errorf("after midnight: %s", got)
	}
	if got, want := text("bedtime_reminder"), "Bedtime soon: lights out by 11:30 PM for 7 hours 30 min of sleep before your 7:00 AM wake-up."; got != want {
		t.Errorf("after midnight = %q", got)
	}

	// Lights-out past midnight: 7 hours 45 min before 8:00 AM is 12:15 AM.
	// The night check-in, capped at 11:30 PM, carries it across midnight.
	exec(`UPDATE users SET wake_time = '08:00', bedtime = '01:00', night_checkin = NULL WHERE id = $1`, s.UserID)
	exec(`UPDATE goals SET value = 465 WHERE user_id = $1 AND metric = 'sleep'`, s.UserID)
	if got := bedtime(); got != "night_checkin at 23:30" {
		t.Errorf("joined across midnight: %s", got)
	}
	if got := text("night_checkin"); !strings.HasSuffix(got, "Lights out by 12:15 AM for 7 hours 45 min of sleep before your 8:00 AM wake-up.") {
		t.Errorf("night check-in across midnight = %q", got)
	}
	// Alone, it goes at 11:45 PM, still before midnight.
	updateSetting(s, "night_checkin", "off")
	if got := bedtime(); got != "bedtime_reminder at 23:45" {
		t.Errorf("alone across midnight: %s", got)
	}
	// One that would go past midnight isn't sent: 8 hours before 9:00 AM is
	// 1:00 AM, so 12:30 AM, which belongs to the night before.
	exec(`UPDATE users SET wake_time = '09:00', bedtime = '02:00' WHERE id = $1`, s.UserID)
	exec(`UPDATE goals SET value = 480 WHERE user_id = $1 AND metric = 'sleep'`, s.UserID)
	if got := bedtime(); got != "" {
		t.Errorf("past midnight: %s", got)
	}
	updateSetting(s, "night_checkin", "default")

	// Wake time and bedtime the same leave no night to go by.
	exec(`UPDATE users SET wake_time = '07:00', bedtime = '07:00' WHERE id = $1`, s.UserID)
	if got := bedtime(); strings.Contains(got, "bedtime_reminder") {
		t.Errorf("wake = bedtime: %s", got)
	}
}
