package agent_test

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

func settingsSim(t *testing.T) (*sim.Sim, time.Time) {
	t.Helper()
	loc, _ := time.LoadLocation("America/Toronto")
	mon := time.Date(2026, 11, 2, 6, 0, 0, 0, loc)
	return sim.New(t, mon), mon
}

// updateSetting has the LLM call update_setting and returns the tool's
// result.
func updateSetting(s *sim.Sim, key, value string) string {
	s.Script(sim.Call("update_setting", fmt.Sprintf(`{"key":%q,"value":%q}`, key, value)), sim.Say("Saved."))
	s.Text("change " + key + " to " + value)
	reqs := s.Requests()
	msgs := reqs[len(reqs)-1]
	return msgs[len(msgs)-1].Content
}

func prompt(s *sim.Sim) string {
	s.Text("how am I doing")
	reqs := s.Requests()
	return reqs[len(reqs)-1][0].Content
}

func due(t *testing.T, s *sim.Sim, day time.Time) []string {
	t.Helper()
	out, err := s.Agent.DueTexts(s.Ctx, s.UserID, day)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestEverySetting changes each setting by text, sees it in the prompt,
// sends a bad value, and puts it back to its default.
func TestEverySetting(t *testing.T) {
	s, _ := settingsSim(t)
	for _, c := range []struct{ key, value, stored, shown, bad string }{
		{"model", "gpt-next", "gpt-next", "gpt-next", "two words"},
		{"input_price", "0.15", "0.15", "0.15", "$1"},
		{"output_price", "0.6", "0.6", "0.6", "NaN"},
		{"log_level", "DEBUG", "debug", "debug", "loud"},
		{"calorie_floor", "1,500", "1500", "1500", "0"},
		{"goal_band", "5", "5", "5", "51"},
		{"default_timezone", "Europe/Paris", "Europe/Paris", "Europe/Paris", "Mars/Base"},
		{"default_wake", "6:30", "06:30", "06:30", "25:00"},
		{"default_bedtime", "23:15", "23:15", "23:15", "late"},
		{"gym_followup_delay", "90m", "1h30m", "1h30m", "10m"},
		{"supplement_reping", "1h", "1h", "1h", "7h"},
		{"water_nudges", "off", "0", "off", "5"},
		{"morning_checkin", "off", "off", "off", "maybe"},
		{"night_checkin", "off", "off", "off", "maybe"},
		{"gym_reminders", "off", "off", "off", "maybe"},
		{"gym_followups", "off", "off", "off", "maybe"},
		{"supplement_reminders", "off", "off", "off", "maybe"},
		{"weekly_summary", "off", "off", "off", "maybe"},
	} {
		if result := updateSetting(s, c.key, c.bad); !strings.HasPrefix(result, "error: "+c.key) {
			t.Errorf("%s = %q: result = %q", c.key, c.bad, result)
		}
		if rows := s.Rows(`SELECT value FROM settings WHERE key = $1`, c.key); len(rows) != 0 {
			t.Errorf("%s: bad value saved: %q", c.key, rows)
		}
		if result := updateSetting(s, c.key, c.value); !strings.HasPrefix(result, "saved") {
			t.Errorf("%s = %q: result = %q", c.key, c.value, result)
		}
		if rows := s.Rows(`SELECT value FROM settings WHERE key = $1`, c.key); !slices.Equal(rows, []string{c.stored}) {
			t.Errorf("%s stored %q, want %q", c.key, rows, c.stored)
		}
		if p := prompt(s); !strings.Contains(p, c.key+": "+c.shown+" (") || !strings.Contains(p, "changed by Sam on Nov 2, 2026") {
			t.Errorf("prompt does not show %s: %s:\n%s", c.key, c.shown, p)
		}
		if result := updateSetting(s, c.key, c.value); !strings.Contains(result, "already") {
			t.Errorf("%s set twice: result = %q", c.key, result)
		}
		if result := updateSetting(s, c.key, "default"); !strings.Contains(result, "(the default)") {
			t.Errorf("%s reset: result = %q", c.key, result)
		}
		if rows := s.Rows(`SELECT value FROM settings WHERE key = $1`, c.key); len(rows) != 0 {
			t.Errorf("%s: reset left %q", c.key, rows)
		}
		if result := updateSetting(s, c.key, "default"); !strings.Contains(result, "already the default") {
			t.Errorf("%s reset twice: result = %q", c.key, result)
		}
	}
	if result := updateSetting(s, "database_url", "x"); !strings.Contains(result, "unknown setting") {
		t.Errorf("unknown key: result = %q", result)
	}
}

// TestSettingUndo reverts a first change to the default and a second to the
// value before it, and records who changed the setting and when.
func TestSettingUndo(t *testing.T) {
	s, mon := settingsSim(t)
	updateSetting(s, "water_nudges", "3")
	if got := s.Rows(`SELECT value, updated_by, updated_at FROM settings`); len(got) != 1 ||
		!strings.Contains(got[0], fmt.Sprintf("3 | %d | ", s.UserID)) {
		t.Errorf("row = %q", got)
	}
	s.Set(mon.Add(time.Hour))
	updateSetting(s, "water_nudges", "off")
	var at time.Time
	if err := s.Pool.QueryRow(s.Ctx, `SELECT updated_at FROM settings`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if !at.Equal(mon.Add(time.Hour)) {
		t.Errorf("updated_at = %s", at)
	}
	if got := s.Text("undo"); got != "Undid setting water nudges to off. Water nudges: 3." {
		t.Errorf("reply = %q", got)
	}
	if got := s.Text("undo"); got != "Undid setting water nudges to 3. Water nudges: 1." {
		t.Errorf("reply = %q", got)
	}
	if rows := s.Rows(`SELECT key FROM settings`); len(rows) != 0 {
		t.Errorf("rows = %q", rows)
	}
	if got := s.Text("undo"); got != "Nothing to undo." {
		t.Errorf("reply = %q", got)
	}
	// Undoing a reset brings the value back.
	updateSetting(s, "weekly_summary", "off")
	updateSetting(s, "weekly_summary", "default")
	if got := s.Text("undo"); got != "Undid setting weekly summary to on. Weekly summary: off." {
		t.Errorf("reply = %q", got)
	}
}

// TestSettingReplay reruns a text that changed a setting: the result is the
// stored one and nothing is applied twice.
func TestSettingReplay(t *testing.T) {
	s, _ := settingsSim(t)
	updateSetting(s, "goal_band", "7")
	s.Script(sim.Call("update_setting", `{"key":"goal_band","value":"7"}`), sim.Say("Saved."))
	s.Rerun()
	reqs := s.Requests()
	msgs := reqs[len(reqs)-1]
	if got := msgs[len(msgs)-1].Content; !strings.HasPrefix(got, "saved") {
		t.Errorf("rerun result = %q", got)
	}
	if rows := s.Rows(`SELECT count(*) FROM actions WHERE tool = 'update_setting'`); rows[0] != "1" {
		t.Errorf("actions = %q", rows)
	}
}

// TestSettingToggles turns each scheduled text off and on, and checks a
// supplement near a check-in that's off gets its own reminder.
func TestSettingToggles(t *testing.T) {
	s, mon := settingsSim(t)
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE goals SET value = 2500 WHERE metric = 'water'`); err != nil {
		t.Fatal(err)
	}
	s.Agent.SupplementReping = time.Hour
	setupWeek(t, s, map[int]string{1: "17:00"})
	s.Script(sim.Call("set_supplement", supplementCall("set", `"creatine"`, "null", `"08:20"`, "null"),
		"set_supplement", supplementCall("set", `"zinc"`, "null", `"12:00"`, "null")), sim.Say("Saved."))
	s.Text("creatine at 8:20, zinc at noon")

	has := func(kind string) bool {
		return slices.ContainsFunc(due(t, s, mon), func(d string) bool { return strings.HasPrefix(d, kind) })
	}
	for _, c := range []struct {
		key  string
		gone []string
	}{
		{"weekly_summary", []string{"weekly_summary"}},
		{"gym_reminders", []string{"gym_reminder"}},
		{"gym_followups", []string{"gym_followup"}},
		{"water_nudges", []string{"water_nudge"}},
		{"supplement_reminders", []string{"supplement_reminder/12:00", "supplement_reping"}},
		{"morning_checkin", []string{"morning_checkin"}},
		{"night_checkin", []string{"night_checkin"}},
	} {
		for _, kind := range c.gone {
			if !has(kind) {
				t.Errorf("%s not due before turning off %s: %q", kind, c.key, due(t, s, mon))
			}
		}
		updateSetting(s, c.key, "off")
		for _, kind := range c.gone {
			if has(kind) {
				t.Errorf("%s still due with %s off: %q", kind, c.key, due(t, s, mon))
			}
		}
		updateSetting(s, c.key, "default")
		for _, kind := range c.gone {
			if !has(kind) {
				t.Errorf("%s not back with %s on: %q", kind, c.key, due(t, s, mon))
			}
		}
	}

	// Creatine at 8:20 joins the morning check-in, unless it's off.
	if has("supplement_reminder/08:20") {
		t.Errorf("creatine has its own reminder: %q", due(t, s, mon))
	}
	updateSetting(s, "morning_checkin", "off")
	if !has("supplement_reminder/08:20") {
		t.Errorf("creatine has no reminder with the check-in off: %q", due(t, s, mon))
	}
	updateSetting(s, "morning_checkin", "default")

	// With the night check-in off, a follow-up after its time goes out.
	updateSetting(s, "gym_followup_delay", "4h30m")
	if has("gym_followup") {
		t.Errorf("follow-up due at the night check-in: %q", due(t, s, mon))
	}
	updateSetting(s, "night_checkin", "off")
	if !has("gym_followup/17:00 at 21:30") {
		t.Errorf("follow-up not due with the night check-in off: %q", due(t, s, mon))
	}
}

// TestSettingStopsPlannedText turns a text off after it's planned: it is
// skipped when its time comes.
func TestSettingStopsPlannedText(t *testing.T) {
	s, mon := settingsSim(t)
	s.Tick()
	if rows := s.Rows(`SELECT status FROM deliveries WHERE kind = 'weekly_summary'`); !slices.Equal(rows, []string{"pending"}) {
		t.Fatalf("weekly summary not planned: %q", rows)
	}
	updateSetting(s, "weekly_summary", "off")
	s.Set(mon.Add(2 * time.Hour))
	s.Tick()
	if rows := s.Rows(`SELECT status, error FROM deliveries WHERE kind = 'weekly_summary'`); !slices.Equal(rows, []string{"skipped | no longer due"}) {
		t.Errorf("weekly summary: %q", rows)
	}
}

// TestModelSetting checks a new model and prices are used and costed from
// the next call, and a model the provider doesn't have is refused.
func TestModelSetting(t *testing.T) {
	s, _ := settingsSim(t)
	if result := updateSetting(s, "model", sim.NoSuchModel); !strings.Contains(result, "no model") {
		t.Errorf("result = %q", result)
	}
	result := updateSetting(s, "model", "gpt-next")
	if !strings.Contains(result, "input_price and output_price are unchanged") {
		t.Errorf("result = %q", result)
	}
	updateSetting(s, "input_price", "5")
	updateSetting(s, "output_price", "10")
	s.Text("hi")
	// 100 input and 10 output tokens.
	if rows := s.Rows(`SELECT model, cost_usd::float8 FROM llm_calls ORDER BY id DESC LIMIT 1`); !slices.Equal(rows, []string{"gpt-next | 0.0006"}) {
		t.Errorf("last call = %q", rows)
	}
}

// TestSettingsApply checks settings change what the agent does: the floor,
// the defaults for a user who gave no times, and the log level.
func TestSettingsApply(t *testing.T) {
	s, mon := settingsSim(t)
	updateSetting(s, "calorie_floor", "1000")
	s.Script(sim.Call("set_goals", `{"goals":[{"metric":"calories","value":1100,"unit":null}]}`), sim.Say("Saved."))
	s.Text("1100 calories")
	if rows := s.Rows(`SELECT value FROM goals WHERE metric = 'calories' AND value IS NOT NULL`); !slices.Equal(rows, []string{"1100"}) {
		t.Errorf("goal = %q", rows)
	}

	if _, err := s.Pool.Exec(s.Ctx, `UPDATE users SET wake_time = NULL, bedtime = NULL WHERE id = $1`, s.UserID); err != nil {
		t.Fatal(err)
	}
	updateSetting(s, "default_wake", "06:30")
	updateSetting(s, "default_bedtime", "23:30")
	got := due(t, s, mon)
	if !slices.Contains(got, "morning_checkin at 06:30") || !slices.Contains(got, "night_checkin at 23:00") {
		t.Errorf("due = %q", got)
	}
	p := prompt(s)
	for _, want := range []string{"wake time: missing (check-ins assume 6:30 AM)", "bedtime: missing (check-ins assume 11:30 PM)",
		"morning check-in at 6:30 AM (the default wake time)", "night check-in at 11:00 PM (30 minutes before the default bedtime)"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}

	if _, err := s.Pool.Exec(s.Ctx, `UPDATE users SET timezone = NULL WHERE id = $1`, s.UserID); err != nil {
		t.Fatal(err)
	}
	updateSetting(s, "default_timezone", "Europe/Paris")
	if p := prompt(s); !strings.Contains(p, "times below assume Europe/Paris") || !strings.Contains(p, "(Europe/Paris)") {
		t.Errorf("prompt lacks the timezone:\n%s", p)
	}

	s.Agent.LogLevel = &slog.LevelVar{}
	updateSetting(s, "log_level", "error")
	s.Text("hi")
	if got := s.Agent.LogLevel.Level(); got != slog.LevelError {
		t.Errorf("log level = %s", got)
	}
	s.Agent.DefaultLogLevel = slog.LevelWarn
	updateSetting(s, "log_level", "default")
	s.Text("hi")
	if got := s.Agent.LogLevel.Level(); got != slog.LevelWarn {
		t.Errorf("log level after reset = %s", got)
	}
}

// TestStoredSettingIgnored falls back to the default for a stored value no
// longer accepted.
func TestStoredSettingIgnored(t *testing.T) {
	s, _ := settingsSim(t)
	if _, err := s.Pool.Exec(s.Ctx, `INSERT INTO settings (key, value, updated_at) VALUES
		('calorie_floor', 'lots', now()), ('retired', 'x', now())`); err != nil {
		t.Fatal(err)
	}
	if p := prompt(s); !strings.Contains(p, "calorie_floor: 1200 (") || !strings.Contains(p, "below 1,200 cal a day") {
		t.Errorf("prompt:\n%s", p)
	}
}
