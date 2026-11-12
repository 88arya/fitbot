package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

func pauseCall(action, first, last, reason string) string {
	return fmt.Sprintf(`{"action":"%s","first_date":%s,"last_date":%s,"reason":%s}`, action, first, last, reason)
}

func pauseRows(s *sim.Sim) string {
	return fmt.Sprint(s.Rows(`SELECT first_date::text || '..' || last_date::text FROM pauses WHERE deleted_at IS NULL ORDER BY first_date`))
}

// TestPause takes a vacation from Thu Nov 5 to Sun Nov 8 in a week of
// training, logging, and a supplement, and checks what's planned, what's
// due, how stats, streaks, and the weekly summary count the days, and that
// ending, cancelling, overlapping, and undoing pauses work.
func TestPause(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	at := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, loc) }
	date := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	s := sim.New(t, at(2, 7))
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE goals SET value = 2000 WHERE metric = 'calories'`); err != nil {
		t.Fatal(err)
	}
	logged(t, s, "push mon, pull wed, legs fri at 6pm; creatine mornings", "set_schedule",
		`{"kind":"weekly","days":[{"weekday":"mon","sessions":["push"],"time":null},{"weekday":"wed","sessions":["pull"],"time":null},`+
			`{"weekday":"fri","sessions":["legs"],"time":null}],"cycle_day_today":null,"training_time":"18:00"}`,
		"set_supplement", supplementCall("set", `"creatine"`, `"5 g"`, `"morning"`, "null"))
	var seed []string
	for d := 2; d <= 4; d++ {
		seed = append(seed, "log_intake", intake("2100", "date", fmt.Sprintf(`"2026-11-0%d"`, d)))
	}
	seed = append(seed, "log_workout", `{"status":"done","sessions":null,"date":"2026-11-02"}`,
		"log_workout", `{"status":"done","sessions":null,"date":"2026-11-04"}`)
	s.Set(at(4, 20))
	logged(t, s, "seed", seed...)

	// Thursday morning: off on vacation through Sunday.
	s.Set(at(5, 7))
	one(t, s, "on vacation til sunday", "set_pause", pauseCall("set", "null", `"2026-11-08"`, `"Vacation"`),
		"paused Nov 5 to Nov 8 (vacation). Those days plan no sessions, aren't judged against goals, and don't break streaks; "+
			"only supplement reminders go out. Back on Mon Nov 9.")
	lines, err := s.Agent.PlanLines(s.Ctx, s.UserID, date(5), 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, "; "); got != "Thu Nov 5: nothing, paused Nov 5 to Nov 8 (vacation); "+
		"Fri Nov 6: nothing, paused Nov 5 to Nov 8 (vacation); Sat Nov 7: nothing, paused Nov 5 to Nov 8 (vacation); "+
		"Sun Nov 8: nothing, paused Nov 5 to Nov 8 (vacation); Mon Nov 9: push at 6:00 PM" {
		t.Errorf("plan = %s", got)
	}
	// Creatine joins the morning check-in on a normal day, and goes alone
	// on a paused one, when nothing else does.
	if got := dueTexts(t, s, date(6)); got != "supplement_reminder/08:00 at 08:00" {
		t.Errorf("due on a paused day = %s", got)
	}
	if got := dueTexts(t, s, date(9)); strings.Contains(got, "supplement_reminder") || !strings.Contains(got, "morning_checkin") ||
		!strings.Contains(got, "gym_reminder") || !strings.Contains(got, "weekly_summary") {
		t.Errorf("due once back = %s", got)
	}
	one(t, s, "move legs to saturday", "plan_session", `{"date":"2026-11-06","change":"skip","to_date":null,"time":null}`,
		"error: Fri Nov 6 is paused (Nov 5 to Nov 8 (vacation)), with nothing planned")

	// Through the scheduler: Friday sends only the creatine reminder.
	before := len(s.Sends())
	for h := 6; h <= 23; h++ {
		s.Set(at(6, h))
		s.Tick()
	}
	if sends := s.Sends()[before:]; len(sends) != 1 || !strings.HasPrefix(sends[0], "Time for creatine") {
		t.Errorf("Friday's texts = %q", sends)
	}

	// Monday: the week is counted without the paused days, and the streak
	// carries across them.
	s.Set(at(9, 7))
	summary, err := s.Agent.CheckinText(s.Ctx, s.UserID, date(9), "weekly_summary", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Last week (Nov 2 to Nov 8):\nPaused Nov 5 to Nov 8 (vacation); those days aren't counted.\n",
		"Calories: 2,100 cal average, on goal 3 of 3 days.",
		"Workouts: 2 of 2 planned sessions done",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary = %q\nwant it to contain %q", summary, want)
		}
	}
	got := logged(t, s, "how was my week", "get_stats", `{"metric":"calories","name":null,"period":"last_week","days":null,"first":null,"last":null}`)
	if len(got) != 1 || !strings.Contains(got[0], "on goal (at least 2,000 cal) on 3 of the 3 days logged, best streak 3 days (4 days paused, not counted)") ||
		!strings.Contains(got[0], "current streak: 3 days on goal in a row") {
		t.Errorf("stats = %q", got)
	}
	got = logged(t, s, "what about saturday", "get_day", `{"date":"2026-11-07"}`)
	if len(got) != 1 || !strings.Contains(got[0], "paused Nov 5 to Nov 8 (vacation): this day isn't judged") {
		t.Errorf("get_day = %q", got)
	}

	// Back early: ending on Friday keeps Thursday paused, and undo puts
	// the rest back.
	s.Set(at(6, 9))
	one(t, s, "back early", "set_pause", pauseCall("end", "null", "null", "null"),
		"ended the pause; it now covers Nov 5 (vacation). Today counts as usual again.")
	if got := pauseRows(s); got != "[2026-11-05..2026-11-05]" {
		t.Errorf("after end = %s", got)
	}
	if got := s.Text("undo"); got != "Undid ending the pause Nov 5 to Nov 8 (vacation) early." {
		t.Errorf("undo end = %q", got)
	}
	// A pause that overlaps from Saturday on trims the first, which undo
	// restores.
	one(t, s, "actually sick sat to thu", "set_pause", pauseCall("set", `"2026-11-07"`, `"2026-11-12"`, `"sick"`),
		"paused Nov 7 to Nov 12 (sick). ")
	if got := pauseRows(s); got != "[2026-11-05..2026-11-06 2026-11-07..2026-11-12]" {
		t.Errorf("after overlap = %s", got)
	}
	if got := s.Text("undo"); got != "Undid pausing Nov 7 to Nov 12 (sick)." {
		t.Errorf("undo overlap = %q", got)
	}
	// A future pause is cancelled whole.
	one(t, s, "trip on the 20th", "set_pause", pauseCall("set", `"2026-11-20"`, `"2026-11-22"`, "null"), "paused Nov 20 to Nov 22.")
	one(t, s, "move wed to fri", "plan_session", `{"date":"2026-11-18","change":"move","to_date":"2026-11-20","time":null}`,
		"error: Fri Nov 20 is paused (Nov 20 to Nov 22), so nothing can move there")
	one(t, s, "trip's off", "set_pause", pauseCall("end", `"2026-11-21"`, "null", "null"),
		"cancelled the pause Nov 20 to Nov 22. Every day counts as usual.")
	if got := pauseRows(s); got != "[2026-11-05..2026-11-08]" {
		t.Errorf("after cancel = %s", got)
	}

	// A pause can be set after the fact, for days already over.
	one(t, s, "I was sick mon to wed", "set_pause", pauseCall("set", `"2026-11-02"`, `"2026-11-04"`, `"sick"`),
		"paused Nov 2 to Nov 4 (sick). Those days plan no sessions, aren't judged against goals, and don't break streaks; "+
			"only supplement reminders go out. It's already over, so it only changes how those days count.")
	one(t, s, "end it", "set_pause", pauseCall("end", `"2026-11-03"`, "null", "null"), "error: that pause is already over")

	// Refusals.
	for _, c := range []struct{ args, want string }{
		{pauseCall("set", "null", "null", "null"), "error: last_date is needed"},
		{pauseCall("set", `"2026-10-20"`, `"2026-11-07"`, "null"), "error: a pause can start at most 7 days ago"},
		{pauseCall("set", "null", `"2026-12-20"`, "null"), "error: a pause can be at most 31 days"},
		{pauseCall("end", `"2026-12-30"`, "null", "null"), "error: no pause covers that day or comes after it"},
		{pauseCall("end", "null", `"2026-11-09"`, "null"), "error: end takes only first_date"},
	} {
		one(t, s, "pause", "set_pause", c.args, c.want)
	}
}
