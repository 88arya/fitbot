package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/sim"
)

// food builds save_food arguments: action, name, then any amounts as
// "name":value pairs.
func food(action, name string, fields ...string) string {
	args := map[string]string{"calories": "null", "protein": "null", "carbs": "null", "fat": "null", "water": "null", "water_unit": "null"}
	for i := 0; i < len(fields); i += 2 {
		args[fields[i]] = fields[i+1]
	}
	return fmt.Sprintf(`{"action":"%s","name":%q,"calories":%s,"protein":%s,"carbs":%s,"fat":%s,"water":%s,"water_unit":%s}`,
		action, name, args["calories"], args["protein"], args["carbs"], args["fat"], args["water"], args["water_unit"])
}

func foodRows(s *sim.Sim) []string {
	return s.Rows(`SELECT name || ' ' || coalesce(calories, 0) || '/' || coalesce(protein, 0) || '/' || coalesce(water, 0)
		FROM saved_foods WHERE deleted_at IS NULL ORDER BY name`)
}

func dayTotals(s *sim.Sim, day string) []string {
	return s.Rows(`SELECT metric || ' ' || sum(amount) FROM intake_logs WHERE local_date = $1 AND deleted_at IS NULL
		GROUP BY metric ORDER BY metric`, day)
}

// TestSavedFoods saves foods and drinks, logs them by name with and
// without the LLM, in servings and on another day, replaces and removes
// them, undoes every change, and checks the rows and replies.
func TestSavedFoods(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 11, 10, 9, 0, 0, 0, loc))
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE goals SET value = CASE metric WHEN 'calories' THEN 2600 WHEN 'water' THEN 3000 END
		WHERE metric IN ('calories', 'water')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(s.Ctx, `INSERT INTO goals (user_id, metric, value, start_date) VALUES ($1, 'protein', 160, '2026-11-10')`,
		s.UserID); err != nil {
		t.Fatal(err)
	}

	one(t, s, "my bottle is 750ml", "save_food", food("set", "Bottle", "water", "750", "water_unit", `"ml"`),
		`saved bottle: 750 ml water. Texting "bottle" logs it.`)
	one(t, s, "save 450 cal 30g protein as usual breakfast", "save_food",
		food("set", "usual breakfast", "calories", "450", "protein", "30"), "saved usual breakfast: 450 cal, 30 g protein.")
	if got := fmt.Sprint(foodRows(s)); got != "[bottle 0/0/750 usual breakfast 450/30/0]" {
		t.Fatalf("saved foods %s", got)
	}

	// Fast path: the name alone, plurals with a count, and a rerun that
	// doesn't log twice. None of these call the LLM.
	calls := len(s.Requests())
	steps := []struct{ text, reply string }{
		{"Bottle", "Logged bottle: 750 ml water. Water: 750 ml / 3 L today, 2.25 L to go."},
		{"2 bottles", "Logged 2x bottle: 1.5 L water. Water: 2.25 / 3 L today, 750 ml to go."},
		{"usual breakfast!", "Logged usual breakfast: 450 cal, 30 g protein. Calories: 450 / 2,600 cal today, 2,150 cal to go. " +
			"Protein: 30 / 160 g today, 130 g to go."},
	}
	for _, st := range steps {
		if got := s.Text(st.text); got != st.reply {
			t.Errorf("%s: reply %q, want %q", st.text, got, st.reply)
		}
	}
	if got := s.Rerun(); !strings.HasPrefix(got, "Logged usual breakfast") {
		t.Errorf("rerun reply %q", got)
	}
	if n := len(s.Requests()); n != calls {
		t.Errorf("fast path made %d LLM calls", n-calls)
	}
	if got := fmt.Sprint(dayTotals(s, "2026-11-10")); got != "[calories 450 protein 30 water 2250]" {
		t.Errorf("totals %s", got)
	}

	// get_day names the food each amount came from.
	got := logged(t, s, "what did I log today", "get_day", `{"date":null}`)
	if len(got) != 1 || !strings.Contains(got[0], "450 cal (usual breakfast)") || !strings.Contains(got[0], "1.5 L water (bottle)") {
		t.Errorf("get_day %q", got)
	}

	// Through the LLM: half a serving, yesterday.
	one(t, s, "had half my usual breakfast yesterday", "log_food", `{"name":"usual breakfast","servings":0.5,"date":"2026-11-09"}`,
		"logged 0.5x usual breakfast: 225 cal (#")
	if got := fmt.Sprint(dayTotals(s, "2026-11-09")); got != "[calories 225 protein 15]" {
		t.Errorf("yesterday %s", got)
	}
	if got := s.Text("undo"); !strings.HasPrefix(got, "Undid logging 0.5x usual breakfast (225 cal and 15 g protein).") {
		t.Errorf("undo log reply %q", got)
	}
	if got := dayTotals(s, "2026-11-09"); len(got) != 0 {
		t.Errorf("yesterday after undo %s", got)
	}

	// Replacing keeps earlier logs, and undo brings the old amounts back.
	one(t, s, "usual breakfast is 500 cal 35g protein now", "save_food",
		food("set", "usual breakfast", "calories", "500", "protein", "35"),
		"changed usual breakfast from 450 cal, 30 g protein to 500 cal, 35 g protein. Earlier logs keep their amounts.")
	if got := s.Text("undo"); got != "Undid changing usual breakfast from 450 cal, 30 g protein to 500 cal, 35 g protein." {
		t.Errorf("undo change reply %q", got)
	}
	one(t, s, "forget my bottle", "save_food", food("remove", "bottle"), "removed bottle (750 ml water). Logs made from it stay.")
	if got := fmt.Sprint(foodRows(s)); got != "[usual breakfast 450/30/0]" {
		t.Errorf("after remove %s", got)
	}
	// A removed name goes to the LLM instead.
	s.Script(sim.Say("How much was it?"))
	if got := s.Text("bottle"); got != "How much was it?" {
		t.Errorf("removed name reply %q", got)
	}
	if got := s.Text("undo"); got != "Undid removing bottle (750 ml water)." {
		t.Errorf("undo remove reply %q", got)
	}
	if got := fmt.Sprint(foodRows(s)); got != "[bottle 0/0/750 usual breakfast 450/30/0]" {
		t.Errorf("after undos %s", got)
	}
	if got := fmt.Sprint(dayTotals(s, "2026-11-10")); got != "[calories 450 protein 30 water 2250]" {
		t.Errorf("logs changed: %s", got)
	}
}

// TestSavedFoodLimits checks refused names and amounts, a food with
// macros the user doesn't track, and logging an unknown name.
func TestSavedFoodLimits(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 11, 10, 9, 0, 0, 0, loc))

	for _, c := range []struct{ name, want string }{
		{"done", `error: "done" already means something else`},
		{"500ml", "error: name can't start with a number"},
		{"undo", `error: "undo" already means something else`},
		{"2 eggs", "error: name can't start with a number"},
		{"!!!", "error: name is empty"},
		{"  ", "error: name is empty"},
		{strings.Repeat("a", 41), "error: name must be at most 40 characters"},
	} {
		one(t, s, "save "+c.name, "save_food", food("set", c.name, "calories", "100"), c.want)
	}
	one(t, s, "save shake", "save_food", food("set", "shake"), "error: no amounts given")
	one(t, s, "save shake", "save_food", food("set", "shake", "calories", "20000"), "error: calories must be at most 10,000 cal per entry")
	one(t, s, "remove shake", "save_food", food("remove", "shake"), `error: nothing is saved as "shake"`)
	one(t, s, "log shake", "log_food", `{"name":"shake","servings":null,"date":null}`, `error: nothing is saved as "shake"`)

	// Protein isn't tracked here: a protein-only food logs nothing, and
	// one with calories logs those and says protein wasn't.
	one(t, s, "save scoop", "save_food", food("set", "scoop", "protein", "25"), "saved scoop: 25 g protein.")
	one(t, s, "save shake", "save_food", food("set", "shake", "calories", "300", "protein", "40"), "saved shake")
	one(t, s, "11 shakes", "log_food", `{"name":"shake","servings":11,"date":null}`, "error: servings must be more than 0 and at most 10")
	if got := s.Text("scoop"); got != "Nothing logged: scoop has only protein, and you don't track protein. Set a goal for it to start." {
		t.Errorf("scoop reply %q", got)
	}
	if got := s.Text("shake"); got != "Logged shake: 300 cal. Calories: 300 cal today. Not logged: protein, since you don't track it." {
		t.Errorf("shake reply %q", got)
	}
	if got := fmt.Sprint(dayTotals(s, "2026-11-10")); got != "[calories 300]" {
		t.Errorf("totals %s", got)
	}
}
