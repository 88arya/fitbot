package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// checkinEvent is a text the user sends at a moment of the simulated week,
// the LLM's scripted answers to it (none for a fast-path text), and the
// start of the reply it should get.
type checkinEvent struct {
	at     time.Time
	text   string
	script []string
	reply  string
}

// TestCheckinWeek runs a week of scheduled texts, Wed Oct 28 to Wed Nov 4,
// across the end of daylight saving time, with the scheduler restarted
// halfway. The user has goals, a weekly schedule at 6:00 PM, and four
// supplements: creatine in the morning, fish oil at 1:00 PM on Mon/Wed/Fri,
// magnesium at night, and vitamin d any time. Through the week they answer
// check-ins with short replies, log ahead of reminders, move the gym later,
// skip and move sessions, snooze, and change the night check-in time. Every
// delivery and what became of it is checked, and the texts of most.
func TestCheckinWeek(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	// at is a local time on day d of October; 32 is Nov 1.
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	s := sim.New(t, at(28, 0, 0))
	s.Agent.SupplementReping = time.Hour
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE goals SET value = CASE metric WHEN 'calories' THEN 1800 WHEN 'water' THEN 2500
		WHEN 'sleep' THEN 480 END WHERE metric <> 'weight'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(s.Ctx, `INSERT INTO goals (user_id, metric, value, start_date)
		VALUES ($1, 'protein', 150, '2026-10-28')`, s.UserID); err != nil {
		t.Fatal(err)
	}

	// plan builds plan_session arguments for today: change, then to_date and
	// time, each JSON or null.
	plan := func(change, to, when string) string {
		return fmt.Sprintf(`{"date":null,"change":"%s","to_date":%s,"time":%s}`, change, to, when)
	}
	events := []checkinEvent{
		{at(28, 7, 0), "push mon/fri, pull tue/sat, legs and cardio wed, at 6pm", []string{sim.Call("set_schedule",
			`{"kind":"weekly","days":[{"weekday":"mon","sessions":["push"],"time":null},{"weekday":"tue","sessions":["pull"],"time":null},`+
				`{"weekday":"wed","sessions":["legs","cardio"],"time":null},{"weekday":"fri","sessions":["push"],"time":null},`+
				`{"weekday":"sat","sessions":["pull"],"time":null}],"cycle_day_today":null,"training_time":"18:00"}`),
			sim.Say("Saved.")}, "Saved."},
		{at(28, 7, 1), "creatine 5g mornings, fish oil 1g at 1pm mon wed fri, magnesium at night, vitamin d whenever", []string{sim.Call(
			"set_supplement", supplementCall("set", `"creatine"`, `"5 g"`, `"morning"`, "null"),
			"set_supplement", supplementCall("set", `"fish oil"`, `"1 g"`, `"13:00"`, `["mon","wed","fri"]`),
			"set_supplement", supplementCall("set", `"magnesium"`, "null", `"night"`, "null"),
			"set_supplement", supplementCall("set", `"vitamin d"`, "null", "null", "null")),
			sim.Say("Saved.")}, "Saved."},
		// Wed: creatine answered with "done"; the gym follow-up with "yes".
		{at(28, 8, 10), "done", nil, "Logged creatine taken. Supplements today: creatine taken, fish oil not logged yet"},
		{at(28, 18, 30), "500ml", nil, "Logged 500 ml water."},
		{at(28, 20, 5), "yes", nil, "Logged legs + cardio done. Workouts today: legs done, cardio done."},
		// Thu, a rest day: creatine before the check-in, enough water early,
		// and magnesium skipped from the night check-in.
		{at(29, 7, 30), "took creatine", []string{sim.Call("log_supplement", takeCall(`["creatine"]`, "taken", "null")),
			sim.Say("Logged.")}, "Logged."},
		{at(29, 12, 0), "1.5L", nil, "Logged 1.5 L water."},
		{at(29, 21, 40), "skip", nil, "Logged magnesium skipped."},
		// Fri: sleep logged first, the gym pushed to 7:30, fish oil taken
		// before its reminder, and "no" to a night check-in that asked about
		// both the gym and magnesium, so the LLM answers it.
		{at(30, 7, 45), "slept 7.5 hours", []string{sim.Call("log_sleep", `{"hours":7.5,"bedtime":null,"wake_time":null,"date":null}`),
			sim.Say("Logged.")}, "Logged."},
		{at(30, 9, 0), "going at 7:30pm", []string{sim.Call("plan_session", plan("time", "null", `"19:30"`)),
			sim.Say("Got it, 7:30.")}, "Got it, 7:30."},
		{at(30, 12, 30), "took fish oil", []string{sim.Call("log_supplement", takeCall(`["fish oil"]`, "taken", "null")),
			sim.Say("Logged.")}, "Logged."},
		{at(30, 21, 35), "no", []string{sim.Call("log_workout", workoutCall("skipped", "null", "null")),
			sim.Say("Logged push skipped.")}, "Logged push skipped."},
		// Sat: snoozed over the water nudge, and the workout logged before
		// its follow-up.
		{at(31, 8, 5), "done", nil, "Logged creatine taken."},
		{at(31, 12, 0), "snooze 3h", nil, "Snoozed until 3:00 PM. Text unmute to turn scheduled texts back on."},
		{at(31, 19, 0), "did pull", []string{sim.Call("log_workout", workoutCall("done", "null", "null")),
			sim.Say("Nice.")}, "Nice."},
		// Sun Nov 1, after the clocks go back: the night check-in moves to
		// 9:00, so magnesium at 10:00 gets its own reminder.
		{at(32, 10, 0), "send the night check-in at 9", []string{sim.Call("set_profile",
			`{"name":null,"timezone":null,"wake_time":null,"bedtime":null,"morning_checkin":null,"night_checkin":"21:00","mute_until":null}`),
			sim.Say("Done.")}, "Done."},
		// Mon: creatine answered with "done" after the weekly summary, which
		// goes out first; the gym skipped ahead of time; fish oil answered
		// with "done".
		{at(33, 8, 5), "done", nil, "Logged creatine taken."},
		{at(33, 8, 30), "skipping the gym today", []string{sim.Call("plan_session", plan("skip", "null", "null")),
			sim.Say("Skipped.")}, "Skipped."},
		{at(33, 13, 5), "done", nil, "Logged fish oil taken."},
		// Tue: pull moved to Wednesday, water ahead of pace, and "unmute"
		// with nothing muted.
		{at(34, 8, 5), "yes", nil, "Logged creatine taken."},
		{at(34, 8, 20), "moving pull to tomorrow", []string{sim.Call("plan_session", plan("move", `"2026-11-04"`, "null")),
			sim.Say("Moved.")}, "Moved."},
		{at(34, 10, 0), "2L", nil, "Logged 2 L water."},
		{at(34, 12, 0), "unmute", nil, "Scheduled texts aren't muted."},
		{at(34, 22, 5), "took it", nil, "Logged magnesium taken."},
		// Wed Nov 4: "no" to the follow-up skips everything planned,
		// including the session moved in.
		{at(35, 20, 5), "no", nil, "Logged legs + cardio + pull skipped."},
	}

	const step = 5 * time.Minute
	restart := at(32, 12, 0)
	next := 0
	for now := at(28, 0, 0); now.Before(at(36, 0, 0)); now = now.Add(step) {
		for next < len(events) && !events[next].at.After(now) {
			e := events[next]
			next++
			s.Set(e.at)
			s.Script(e.script...)
			calls := len(s.Requests())
			if reply := s.Text(e.text); !strings.HasPrefix(reply, e.reply) {
				t.Errorf("%s %q: reply = %q, want it to start %q", e.at.Format("Jan 2 15:04"), e.text, reply, e.reply)
			}
			if got := len(s.Requests()) - calls; got != len(e.script) {
				t.Errorf("%s %q: %d LLM calls, want %d", e.at.Format("Jan 2 15:04"), e.text, got, len(e.script))
			}
		}
		if now.Equal(restart) {
			s.Restart()
		}
		s.Set(now)
		s.Tick()
	}

	want := []string{
		"Oct 28 08:00 morning_checkin sent",
		"Oct 28 09:00 supplement_reping/08:00 skipped (no longer due)",
		"Oct 28 13:00 supplement_reminder/13:00 sent",
		"Oct 28 14:00 supplement_reping/13:00 sent",
		"Oct 28 14:45 water_nudge/1 sent",
		"Oct 28 18:00 gym_reminder/18:00 sent",
		"Oct 28 20:00 gym_followup/18:00 sent",
		"Oct 28 21:30 night_checkin sent",

		"Oct 29 08:00 morning_checkin sent",
		"Oct 29 09:00 supplement_reping/08:00 skipped (no longer due)",
		"Oct 29 14:45 water_nudge/1 skipped (nothing to send)",
		"Oct 29 21:30 night_checkin sent",

		"Oct 30 08:00 morning_checkin sent",
		"Oct 30 09:00 supplement_reping/08:00 sent",
		"Oct 30 13:00 supplement_reminder/13:00 skipped (no longer due)",
		"Oct 30 14:00 supplement_reping/13:00 skipped (no longer due)",
		"Oct 30 14:45 water_nudge/1 sent",
		"Oct 30 18:00 gym_reminder/18:00 skipped (no longer due)",
		"Oct 30 19:30 gym_reminder/19:30 sent",
		"Oct 30 20:00 gym_followup/18:00 skipped (no longer due)",
		"Oct 30 21:30 night_checkin sent",

		"Oct 31 08:00 morning_checkin sent",
		"Oct 31 09:00 supplement_reping/08:00 skipped (no longer due)",
		"Oct 31 14:45 water_nudge/1 skipped (muted)",
		"Oct 31 18:00 gym_reminder/18:00 sent",
		"Oct 31 20:00 gym_followup/18:00 skipped (no longer due)",
		"Oct 31 21:30 night_checkin sent",

		"Nov 01 08:00 morning_checkin sent",
		"Nov 01 09:00 supplement_reping/08:00 sent",
		"Nov 01 14:30 water_nudge/1 sent",
		"Nov 01 21:00 night_checkin sent",
		"Nov 01 22:00 supplement_reminder/22:00 sent",

		"Nov 02 08:00 morning_checkin sent",
		"Nov 02 08:00 weekly_summary sent",
		"Nov 02 09:00 supplement_reping/08:00 skipped (no longer due)",
		"Nov 02 13:00 supplement_reminder/13:00 sent",
		"Nov 02 14:00 supplement_reping/13:00 skipped (no longer due)",
		"Nov 02 14:30 water_nudge/1 sent",
		"Nov 02 18:00 gym_reminder/18:00 skipped (no longer due)",
		"Nov 02 20:00 gym_followup/18:00 skipped (no longer due)",
		"Nov 02 21:00 night_checkin sent",
		"Nov 02 22:00 supplement_reminder/22:00 sent",

		"Nov 03 08:00 morning_checkin sent",
		"Nov 03 09:00 supplement_reping/08:00 skipped (no longer due)",
		"Nov 03 14:30 water_nudge/1 skipped (nothing to send)",
		"Nov 03 18:00 gym_reminder/18:00 skipped (no longer due)",
		"Nov 03 20:00 gym_followup/18:00 skipped (no longer due)",
		"Nov 03 21:00 night_checkin sent",
		"Nov 03 22:00 supplement_reminder/22:00 sent",

		"Nov 04 08:00 morning_checkin sent",
		"Nov 04 09:00 supplement_reping/08:00 sent",
		"Nov 04 13:00 supplement_reminder/13:00 sent",
		"Nov 04 14:00 supplement_reping/13:00 sent",
		"Nov 04 14:30 water_nudge/1 sent",
		"Nov 04 18:00 gym_reminder/18:00 sent",
		"Nov 04 20:00 gym_followup/18:00 sent",
		"Nov 04 21:00 night_checkin sent",
		"Nov 04 22:00 supplement_reminder/22:00 sent",
	}
	got := s.Rows(`SELECT to_char(scheduled_at AT TIME ZONE 'America/Toronto', 'Mon DD HH24:MI') || ' ' || kind ||
		CASE WHEN slot = '' THEN '' ELSE '/' || slot END || ' ' || status || coalesce(' (' || error || ')', '')
		FROM deliveries ORDER BY scheduled_at, kind`)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("deliveries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	goals := "Goals: 1,800 cal, 150 g protein, 2.5 L water."
	intake := func(water string) string {
		return "Calories: 0 / 1,800 cal today, 1,800 cal to go. Protein: 0 / 150 g today, 150 g to go. Water: " + water + "."
	}
	bodies := map[string]string{
		"Oct 28 08:00 morning_checkin": "Good morning, Sam. Today: legs + cardio at 6:00 PM. " + goals + " Supplements: creatine 5 g now, " +
			"fish oil 1 g at 1:00 PM, magnesium at night (10:00 PM), vitamin d any time; reply done once you've taken creatine, or skip. " +
			"How did you sleep?",
		"Oct 28 13:00 supplement_reminder": "Time for fish oil 1 g. Reply done once you've taken it, or skip.",
		"Oct 28 14:00 supplement_reping":   "Still due: fish oil 1 g. Reply done once you've taken it, or skip.",
		"Oct 28 14:45 water_nudge":         "Water: 0 ml / 2.5 L so far today, 1.25 L behind pace. Text an amount like 500ml to log it.",
		"Oct 28 18:00 gym_reminder":        "Time to head out: legs + cardio at 6:00 PM.",
		"Oct 28 20:00 gym_followup":        "Did you make it to the gym for legs + cardio? Reply yes or no, or tell me when you're going instead.",
		"Oct 28 21:30 night_checkin": "Good evening, Sam. " + intake("500 ml / 2.5 L today, 2 L to go") + " Workouts: legs done, cardio done. " +
			"Supplements: creatine taken, fish oil not logged yet, magnesium not logged yet, vitamin d not logged yet. " +
			"Reply done once you've taken magnesium, or skip.",
		"Oct 29 08:00 morning_checkin": "Good morning, Sam. Today is a rest day. " + goals + " Supplements: creatine 5 g taken, " +
			"magnesium at night (10:00 PM), vitamin d any time. How did you sleep?",
		"Oct 30 08:00 morning_checkin": "Good morning, Sam. Today: push at 6:00 PM. " + goals + " Supplements: creatine 5 g now, " +
			"fish oil 1 g at 1:00 PM, magnesium at night (10:00 PM), vitamin d any time; reply done once you've taken creatine, or skip. " +
			"Sleep: 7 hours 30 min / 8 hours last night, 30 min short.",
		"Oct 30 09:00 supplement_reping": "Still due: creatine 5 g. Reply done once you've taken it, or skip.",
		"Oct 30 19:30 gym_reminder":      "Time to head out: push at 7:30 PM.",
		"Oct 30 21:30 night_checkin": "Good evening, Sam. " + intake("0 ml / 2.5 L today, 2.5 L to go") + " Workouts: push not logged yet. " +
			"Supplements: creatine not logged yet, fish oil taken, magnesium not logged yet, vitamin d not logged yet. " +
			"Did you make it to the gym for push today? Reply yes or no. Reply done once you've taken magnesium, or skip.",
		"Nov 01 21:00 night_checkin": "Good evening, Sam. " + intake("0 ml / 2.5 L today, 2.5 L to go") + " Workouts: rest day. " +
			"Supplements: creatine not logged yet, magnesium not logged yet, vitamin d not logged yet.",
		"Nov 01 22:00 supplement_reminder": "Time for magnesium. Reply done once you've taken it, or skip.",
		"Nov 02 21:00 night_checkin": "Good evening, Sam. " + intake("0 ml / 2.5 L today, 2.5 L to go") + " Workouts: push skipped. " +
			"Supplements: creatine taken, fish oil taken, magnesium not logged yet, vitamin d not logged yet.",
		"Nov 03 21:00 night_checkin": "Good evening, Sam. " + intake("2 / 2.5 L today, 500 ml to go") +
			" Workouts: nothing planned, pull moved to Wed Nov 4. Supplements: creatine taken, magnesium not logged yet, " +
			"vitamin d not logged yet.",
		"Nov 04 08:00 morning_checkin": "Good morning, Sam. Today: legs + cardio + pull at 6:00 PM (pull moved here). " + goals +
			" Supplements: creatine 5 g now, fish oil 1 g at 1:00 PM, magnesium at night (10:00 PM), vitamin d any time; " +
			"reply done once you've taken creatine, or skip. How did you sleep?",
		"Nov 04 18:00 gym_reminder": "Time to head out: legs + cardio + pull at 6:00 PM.",
		// The user started on Wednesday, so the goals cover 5 days.
		"Nov 02 08:00 weekly_summary": "Last week (Oct 26 to Nov 1):\nCalories: nothing logged.\nProtein: nothing logged.\n" +
			"Water: 1 L average over 2 days logged, on goal 0 of 5 days.\n" +
			"Sleep: 7 hours 30 min average over 1 night logged, on goal 0 of 5 nights.\n" +
			"Workouts: 3 of 4 planned sessions done, 1 skipped (legs 1 of 1, cardio 1 of 1, push 0 of 1, pull 1 of 1).\n" +
			"Supplements taken: creatine 3 of 5, 2 missed; fish oil 1 of 2, 1 missed; magnesium 0 of 5, 1 skipped, 4 missed; " +
			"vitamin d 0 of 5, 5 missed.",
	}
	sent := map[string]string{}
	for _, r := range s.Rows(`SELECT to_char(d.scheduled_at AT TIME ZONE 'America/Toronto', 'Mon DD HH24:MI') || ' ' || d.kind, m.body
		FROM deliveries d JOIN messages m ON m.id = d.message_id`) {
		k, body, _ := strings.Cut(r, " | ")
		sent[k] = body
	}
	for k, want := range bodies {
		if sent[k] != want {
			t.Errorf("%s:\n got %q\nwant %q", k, sent[k], want)
		}
	}
	// The summary goes out before Monday's check-in, so "done" answers the
	// check-in.
	got = s.Rows(`SELECT d.kind FROM deliveries d JOIN messages m ON m.id = d.message_id
		WHERE d.local_date = '2026-11-02' AND d.kind IN ('weekly_summary', 'morning_checkin') ORDER BY m.id`)
	if fmt.Sprint(got) != "[weekly_summary morning_checkin]" {
		t.Errorf("Monday morning sent %v, want the summary first", got)
	}

	// Each short reply logged what its text asked about, on its day.
	wantWorkouts := "[2026-10-28 | done | [legs cardio] 2026-10-30 | skipped | [push] 2026-10-31 | done | [pull] " +
		"2026-11-04 | skipped | [legs cardio pull]]"
	if got := s.Rows(`SELECT local_date::text, status, sessions FROM workouts WHERE deleted_at IS NULL ORDER BY local_date, id`); fmt.Sprint(got) != wantWorkouts {
		t.Errorf("workouts = %v\nwant %s", got, wantWorkouts)
	}
	wantTakes := "[2026-10-28 | creatine | taken 2026-10-29 | creatine | taken 2026-10-29 | magnesium | skipped " +
		"2026-10-30 | fish oil | taken 2026-10-31 | creatine | taken 2026-11-02 | creatine | taken 2026-11-02 | fish oil | taken " +
		"2026-11-03 | creatine | taken 2026-11-03 | magnesium | taken]"
	if got := s.Rows(`SELECT l.local_date::text, s.name, l.status FROM supplement_logs l JOIN supplements s ON s.id = l.supplement_id
		WHERE l.deleted_at IS NULL ORDER BY l.local_date, s.name`); fmt.Sprint(got) != wantTakes {
		t.Errorf("supplement logs = %v\nwant %s", got, wantTakes)
	}

	// The last short reply is undone like any other change.
	if reply := s.Text("undo"); !strings.HasPrefix(reply, "Undid logging legs + cardio + pull skipped") {
		t.Errorf("undo = %q", reply)
	}
}

// setupWeek gives the simulated user calorie and water goals and a weekly
// schedule of legs, by SQL: sessions maps each weekday slot to its
// training time, or "" for none.
func setupWeek(t *testing.T, s *sim.Sim, sessions map[int]string) {
	t.Helper()
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE goals SET value = CASE metric WHEN 'calories' THEN 1800 WHEN 'water' THEN 2500 END
		WHERE metric IN ('calories', 'water')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.Pool.QueryRow(s.Ctx, `UPDATE training_schedules SET kind = 'weekly', cycle_days = 7 WHERE user_id = $1 RETURNING id`,
		s.UserID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for slot, at := range sessions {
		if _, err := s.Pool.Exec(s.Ctx, `INSERT INTO schedule_days (schedule_id, slot, sessions, training_time)
			VALUES ($1, $2, ARRAY['legs'], NULLIF($3, '')::time)`, id, slot, at); err != nil {
			t.Fatal(err)
		}
	}
}

func dueTexts(t *testing.T, s *sim.Sim, day time.Time) string {
	t.Helper()
	got, err := s.Agent.DueTexts(s.Ctx, s.UserID, day)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(got, ", ")
}

// TestCheckinRules checks which texts are due: a follow-up only while it
// comes before the night check-in and outside quiet hours, a supplement at
// a time the user set even in quiet hours but no re-ping past midnight,
// supplements near a check-in joining it, and water nudges spread between
// the check-ins, none without a water goal.
func TestCheckinRules(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	mon := time.Date(2026, 11, 2, 6, 0, 0, 0, loc)
	s := sim.New(t, mon)
	s.Agent.SupplementReping = time.Hour
	s.Agent.WaterNudges = 2
	// Legs on Mon at 7:00 PM, Tue at 8:00 PM, and Wed with no time.
	setupWeek(t, s, map[int]string{1: "19:00", 2: "20:00", 3: ""})
	s.Script(sim.Call(
		"set_supplement", supplementCall("set", `"zinc"`, "null", `"07:00"`, "null"),
		"set_supplement", supplementCall("set", `"iron"`, `"18 mg"`, `"07:00"`, "null"),
		"set_supplement", supplementCall("set", `"creatine"`, "null", `"08:20"`, "null"),
		"set_supplement", supplementCall("set", `"melatonin"`, "null", `"23:30"`, "null"),
		"set_supplement", supplementCall("set", `"vitamin d"`, "null", "null", "null")), sim.Say("Saved."))
	s.Text("zinc and iron at 7, creatine at 8:20, melatonin at 11:30pm, vitamin d whenever")

	// Monday: the weekly summary goes just before the morning check-in;
	// the follow-up at 9:00 PM is before the night check-in; creatine joins
	// the morning check-in but is still re-pinged; iron and zinc at 7:00
	// share a reminder, and melatonin at 11:30 PM gets its own, quiet hours
	// or not, but its re-ping would be past midnight.
	want := "weekly_summary at 08:00, morning_checkin at 08:00, night_checkin at 21:30, gym_reminder/19:00 at 19:00, " +
		"gym_followup/19:00 at 21:00, supplement_reminder/07:00 at 07:00, supplement_reping/07:00 at 08:00, " +
		"supplement_reping/08:20 at 09:20, supplement_reminder/23:30 at 23:30, water_nudge/1 at 12:30, water_nudge/2 at 17:00"
	if got := dueTexts(t, s, mon); got != want {
		t.Errorf("Monday:\n got %s\nwant %s", got, want)
	}
	text, err := s.Agent.CheckinText(s.Ctx, s.UserID, mon, "supplement_reminder", "07:00")
	if want := "Time for iron 18 mg and zinc. Reply done once you've taken them, or skip."; err != nil || text != want {
		t.Errorf("7:00 reminder = %q, %v; want %q", text, err, want)
	}
	// Tuesday: a follow-up at 10:00 PM would come after the night check-in.
	if got := dueTexts(t, s, mon.AddDate(0, 0, 1)); strings.Contains(got, "gym_followup") || !strings.Contains(got, "gym_reminder/20:00 at 20:00") {
		t.Errorf("Tuesday: %s", got)
	}
	// Wednesday: no time is set, so no gym texts; the morning check-in asks.
	if got := dueTexts(t, s, mon.AddDate(0, 0, 2)); strings.Contains(got, "gym_") {
		t.Errorf("Wednesday: %s", got)
	}
	// A follow-up landing in quiet hours is dropped too: with bedtime at
	// 9:00 PM and the night check-in at 9:45 PM, 9:00 PM is quiet.
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE users SET bedtime = '21:00', night_checkin = '21:45' WHERE id = $1`, s.UserID); err != nil {
		t.Fatal(err)
	}
	if got := dueTexts(t, s, mon); strings.Contains(got, "gym_followup") {
		t.Errorf("Monday with an early bedtime: %s", got)
	}
	// No water goal, no nudges; the user's own morning check-in time starts
	// the day, with the weekly summary, and iron and zinc at 7:00 now join
	// it.
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE goals SET value = NULL WHERE metric = 'water'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE users SET bedtime = '22:00', night_checkin = NULL, morning_checkin = '07:00'
		WHERE id = $1`, s.UserID); err != nil {
		t.Fatal(err)
	}
	want = "weekly_summary at 07:00, morning_checkin at 07:00, night_checkin at 21:30, gym_reminder/19:00 at 19:00, " +
		"gym_followup/19:00 at 21:00, supplement_reping/07:00 at 08:00, supplement_reminder/08:20 at 08:20, " +
		"supplement_reping/08:20 at 09:20, supplement_reminder/23:30 at 23:30"
	if got := dueTexts(t, s, mon); got != want {
		t.Errorf("Monday, 7:00 check-in and no water goal:\n got %s\nwant %s", got, want)
	}
}

// TestCheckinReplies checks what a short reply does: it answers only the
// scheduled text just before it, once even when retried or rerun, only up
// to the next day, only when that text asked about one thing, and says so
// when it was already logged.
func TestCheckinReplies(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	day := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, loc) }
	s := sim.New(t, day(2, 6, 0))
	setupWeek(t, s, map[int]string{1: "17:00"})
	s.Script(sim.Call(
		"set_supplement", supplementCall("set", `"creatine"`, "null", `"morning"`, "null"),
		"set_supplement", supplementCall("set", `"magnesium"`, "null", `"night"`, "null")), sim.Say("Saved."))
	s.Text("creatine mornings, magnesium at night")

	// "yes" with no scheduled text before it goes to the LLM.
	s.Script(sim.Say("Yes to what?"))
	if reply := s.Text("yes"); reply != "Yes to what?" {
		t.Errorf("yes with nothing asked = %q", reply)
	}

	// tick runs the scheduler at at and returns the text it sent.
	tick := func(at time.Time) string {
		t.Helper()
		n := len(s.Sends())
		s.Set(at)
		s.Tick()
		return s.WaitSends(n + 1)[n]
	}
	if got := tick(day(2, 8, 0)); !strings.Contains(got, "reply done once you've taken creatine") {
		t.Fatalf("morning check-in = %q", got)
	}
	// The reply needs no LLM, so it works while the LLM is down.
	client := s.Agent.LLM
	s.Agent.LLM = nil
	reply := s.Text("Done")
	s.Agent.LLM = client
	if !strings.HasPrefix(reply, "Logged creatine taken. Supplements today: creatine taken, magnesium not logged yet.") {
		t.Errorf("done = %q", reply)
	}
	s.Retry()
	if again := s.Rerun(); again != reply {
		t.Errorf("rerun = %q, want %q", again, reply)
	}
	if got := s.Rows(`SELECT count(*) FROM supplement_logs`); fmt.Sprint(got) != "[1]" {
		t.Errorf("supplement logs = %v, want 1", got)
	}
	// A second "done" answers Fitbot's reply, not the check-in.
	s.Script(sim.Say("Already got it."))
	if reply := s.Text("done"); reply != "Already got it." {
		t.Errorf("second done = %q", reply)
	}

	// The night check-in asks about the gym and magnesium, so "yes" is left
	// to the LLM.
	if got := tick(day(2, 21, 30)); !strings.Contains(got, "Did you make it to the gym for legs today?") ||
		!strings.Contains(got, "Reply done once you've taken magnesium") {
		t.Fatalf("night check-in = %q", got)
	}
	s.Script(sim.Say("Which one?"))
	if reply := s.Text("yes"); reply != "Which one?" {
		t.Errorf("yes to two questions = %q", reply)
	}

	// Tuesday's check-in asks about creatine; logged another way before the
	// reply, "took it" says it's already logged.
	if got := tick(day(3, 8, 0)); !strings.Contains(got, "reply done once you've taken creatine") {
		t.Fatalf("Tuesday's check-in = %q", got)
	}
	if _, err := s.Pool.Exec(s.Ctx, `INSERT INTO supplement_logs (user_id, action_id, message_id, supplement_id, local_date, status, logged_at)
		SELECT $1, a.id, a.message_id, s.id, '2026-11-03', 'taken', now()
		FROM supplements s, actions a WHERE s.name = 'creatine' ORDER BY a.id LIMIT 1`, s.UserID); err != nil {
		t.Fatal(err)
	}
	if reply := s.Text("took it"); !strings.HasPrefix(reply, "That's already logged. Supplements today: creatine taken") {
		t.Errorf("took it after logging = %q", reply)
	}

	// A reply on Friday to Wednesday's check-in is too late to count.
	if got := tick(day(4, 8, 0)); !strings.Contains(got, "reply done once you've taken creatine") {
		t.Fatalf("Wednesday's check-in = %q", got)
	}
	s.Set(day(6, 9, 0))
	s.Script(sim.Say("Taken when?"))
	if reply := s.Text("done"); reply != "Taken when?" {
		t.Errorf("done two days later = %q", reply)
	}
	if got := s.Rows(`SELECT count(*) FROM supplement_logs WHERE local_date = '2026-11-04'`); fmt.Sprint(got) != "[0]" {
		t.Errorf("Wednesday's logs = %v, want none", got)
	}
}

// TestCheckinSettings changes the check-in times and mutes texts through
// set_profile, refuses bad values, shows the settings in the prompt, and
// undoes a mute.
func TestCheckinSettings(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 11, 2, 12, 0, 0, 0, loc))
	call := func(morning, night, mute string) string {
		return fmt.Sprintf(`{"name":null,"timezone":null,"wake_time":null,"bedtime":null,"morning_checkin":%s,"night_checkin":%s,"mute_until":%s}`,
			morning, night, mute)
	}
	// results runs set_profile calls in one text and returns their results.
	results := func(calls ...string) []string {
		t.Helper()
		var nameArgs []string
		for _, c := range calls {
			nameArgs = append(nameArgs, "set_profile", c)
		}
		s.Script(sim.Call(nameArgs...), sim.Say("ok"))
		s.Text("settings")
		var out []string
		reqs := s.Requests()
		for _, m := range reqs[len(reqs)-1] {
			if m.Role == "tool" {
				out = append(out, m.Content)
			}
		}
		return out
	}

	got := results(call(`"07:15"`, `"21:00"`, "null"))
	if want := "[saved. Now: morning check-in at 7:15 AM, night check-in at 9:00 PM.]"; fmt.Sprint(got) != want {
		t.Errorf("set times = %v, want %s", got, want)
	}
	got = results(call(`"default"`, "null", `"2026-11-03 09:00"`))
	if want := "[saved. Now: morning check-in at 8:00 AM (the wake time), night check-in at 9:00 PM; scheduled texts muted " +
		"until Tue Nov 3, 9:00 AM.]"; fmt.Sprint(got) != want {
		t.Errorf("default and mute = %v, want %s", got, want)
	}
	if got := s.Rows(`SELECT morning_checkin IS NULL, night_checkin::text, muted_until = '2026-11-03 09:00-05' FROM users`); fmt.Sprint(got) != "[true | 21:00:00 | true]" {
		t.Errorf("user = %v", got)
	}
	reqs := s.Requests()
	if sys := reqs[len(reqs)-1][0].Content; !strings.Contains(sys, "Scheduled texts: morning check-in at 8:00 AM (the wake time), "+
		"night check-in at 9:00 PM; scheduled texts muted until Tue Nov 3, 9:00 AM.\n") {
		t.Errorf("prompt is missing the scheduled texts line:\n%s", sys)
	} else if !strings.Contains(sys, "on Mondays, just before the morning check-in, a summary of last week") {
		t.Errorf("prompt is missing the weekly summary:\n%s", sys)
	}

	got = results(
		call(`"7am"`, "null", "null"),
		call("null", `"25:00"`, "null"),
		call("null", "null", `"tomorrow"`),
		call("null", "null", `"2026-11-02 11:00"`),
		call("null", "null", `"2026-12-03 09:00"`),
		call("null", "null", "null"),
	)
	want := []string{
		`error: morning_checkin "7am" is not a 24-hour time like 07:30, or default`,
		`error: night_checkin "25:00" is not a 24-hour time like 07:30, or default`,
		`error: mute_until "tomorrow" is not a local time like 2026-10-30 09:00, or off`,
		"error: mute_until is in the past; it's 2026-11-02 12:00 now",
		"error: scheduled texts can be muted for at most 30 days",
		"error: no fields given",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("refusals:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Undo brings back the times before the mute; then nothing is muted.
	if reply := s.Text("undo"); !strings.HasPrefix(reply, "Undid setting your morning check-in back to the wake time, mute until Tue Nov 3, 9:00 AM") {
		t.Errorf("undo = %q", reply)
	}
	if got := s.Rows(`SELECT morning_checkin::text, night_checkin::text, muted_until IS NULL FROM users`); fmt.Sprint(got) != "[07:15:00 | 21:00:00 | true]" {
		t.Errorf("user after undo = %v", got)
	}
	if reply := s.Text("unmute"); reply != "Scheduled texts aren't muted." {
		t.Errorf("unmute = %q", reply)
	}
	if reply := s.Text("snooze 90 min"); reply != "Snoozed until 1:30 PM. Text unmute to turn scheduled texts back on." {
		t.Errorf("snooze = %q", reply)
	}
	// Run again later, as after a crash, it keeps its first time.
	s.Set(time.Date(2026, 11, 2, 12, 10, 0, 0, loc))
	if reply := s.Rerun(); reply != "Snoozed until 1:30 PM. Text unmute to turn scheduled texts back on." {
		t.Errorf("snooze rerun = %q", reply)
	}
	if got := s.Rows(`SELECT muted_until = '2026-11-02 13:30-05' FROM users`); fmt.Sprint(got) != "[true]" {
		t.Errorf("muted until 1:30 PM = %v", got)
	}
	if reply := s.Text("Unmute."); reply != "Unmuted. Scheduled texts are back on." {
		t.Errorf("unmute = %q", reply)
	}
}
