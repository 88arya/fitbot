package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/llm"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestFormat(t *testing.T) {
	for _, c := range []struct {
		metric string
		v      int
		want   string
	}{
		{"calories", 1800, "1,800 cal"},
		{"calories", 950, "950 cal"},
		{"protein", 150, "150 g"},
		{"water", 750, "750 ml"},
		{"water", 2000, "2 L"},
		{"water", 1893, "1.89 L"},
		{"sleep", 450, "7 hours 30 min"},
		{"sleep", 45, "45 min"},
		{"sleep", 0, "0 min"},
		{"sleep", 65, "1 hour 5 min"},
		{"sleep", 60, "1 hour"},
		{"weight", 81647, "180 lbs"},
		{"weight", 82000, "180.8 lbs"},
	} {
		m, _ := metricByName(c.metric)
		if got := m.format(c.v); got != c.want {
			t.Errorf("%s %d = %q, want %q", c.metric, c.v, got, c.want)
		}
	}
	if got := commas(1234567); got != "1,234,567" {
		t.Errorf("commas = %q", got)
	}
}

// setGoals runs one set_goals call for a fresh user with a name and
// timezone, and returns the tool result and the user.
func (e *env) setGoals(args string) (string, store.User) {
	e.t.Helper()
	user := e.newUser()
	if _, err := e.q.UpdateProfile(e.ctx, store.UpdateProfileParams{ID: user.ID,
		Name: pgtype.Text{String: "Sam", Valid: true}, Timezone: pgtype.Text{String: "America/Toronto", Valid: true}}); err != nil {
		e.t.Fatal(err)
	}
	e.llm.script = []string{calls("set_goals", args)}
	n := len(e.llm.requests)
	if _, _, err := e.text(user, "x"); err != nil {
		e.t.Fatal(err)
	}
	results := toolResultsIn(e.llm.requests[n+1])
	if len(results) != 1 {
		e.t.Fatalf("tool results = %v", results)
	}
	return results[0], user
}

func toolResultsIn(req []llm.Message) []string {
	var out []string
	for _, m := range req {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestSetGoalsUnits(t *testing.T) {
	e := newEnv(t)
	result, user := e.setGoals(`{"goals":[
		{"metric":"calories","value":1800,"unit":"cal"},
		{"metric":"protein","value":150,"unit":null},
		{"metric":"water","value":64,"unit":"oz"},
		{"metric":"sleep","value":7.5,"unit":"hours"},
		{"metric":"weight","value":82,"unit":"kg"},
		{"metric":"fat","value":null,"unit":null}]}`)
	want := "saved, starting today: calories: 1,800 cal a day; protein: 150 g a day; water: 1.89 L a day; " +
		"sleep: 7 hours 30 min a night; target weight: 180.8 lbs; fat: not tracking"
	if result != want {
		t.Errorf("result = %q\nwant %q", result, want)
	}
	if got := e.goals(user.ID); got != "calories=1800 fat=null protein=150 sleep=450 water=1893 weight=82000" {
		t.Errorf("goals = %s", got)
	}
}

func TestSetGoalsValidation(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ args, want string }{
		{`{"goals":[]}`, "error: no goals given"},
		{`{"goals":[{"metric":"calories","value":1000,"unit":null}]}`, "error: 1,000 cal is below the safe minimum of 1,200 cal a day"},
		{`{"goals":[{"metric":"calories","value":1199.6,"unit":null}]}`, ""},
		{`{"goals":[{"metric":"calories","value":20000,"unit":null}]}`, "error: calories must be at most 10,000 cal"},
		{`{"goals":[{"metric":"water","value":2,"unit":null}]}`, "error: water must be at least 250 ml"},
		{`{"goals":[{"metric":"water","value":2,"unit":"g"}]}`, "error: g is not a unit for water; use ml or l or oz"},
		{`{"goals":[{"metric":"protein","value":-5,"unit":"g"}]}`, "error: protein must be a positive amount"},
		{`{"goals":[{"metric":"sleep","value":20,"unit":"hours"}]}`, "error: sleep must be at most 14 hours"},
		{`{"goals":[{"metric":"weight","value":20,"unit":"lbs"}]}`, "error: target weight must be at least 50 lbs"},
		{`{"goals":[{"metric":"steps","value":10000,"unit":null}]}`, `error: unknown metric "steps"`},
		{`{"goals":[{"metric":"water","value":2,"unit":"l"},{"metric":"water","value":3,"unit":"l"}]}`, "error: water is listed twice"},
		// A bad goal rejects the whole call, so the good one is not saved either.
		{`{"goals":[{"metric":"water","value":2,"unit":"l"},{"metric":"calories","value":800,"unit":null}]}`, "error: 800 cal is below"},
	} {
		result, user := e.setGoals(c.args)
		if c.want == "" {
			if !strings.HasPrefix(result, "saved") {
				t.Errorf("%s: result = %q, want saved", c.args, result)
			}
			continue
		}
		if !strings.HasPrefix(result, c.want) {
			t.Errorf("%s: result = %q, want %q", c.args, result, c.want)
		}
		if got := e.goals(user.ID); got != "" {
			t.Errorf("%s: rejected call saved %s", c.args, got)
		}
	}
}

func TestCalorieFloorSetting(t *testing.T) {
	e := newEnv(t)
	e.agent.CalorieFloor = 1500
	if result, _ := e.setGoals(`{"goals":[{"metric":"calories","value":1400,"unit":null}]}`); !strings.Contains(result, "safe minimum of 1,500 cal") {
		t.Errorf("result = %q", result)
	}
	if !strings.Contains(e.llm.requests[0][0].Content, "below 1,500 cal a day") {
		t.Errorf("prompt does not state the floor:\n%s", e.llm.requests[0][0].Content)
	}
}

// TestGoalHistory changes a goal mid-week and checks earlier days keep the
// old one, and that a second change on one day replaces the first.
func TestGoalHistory(t *testing.T) {
	e := newEnv(t)
	vancouver, _ := time.LoadLocation("America/Vancouver")
	at := func(d, h int) {
		e.agent.Now = func() time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, vancouver) }
	}
	user := e.newUser()
	if _, err := e.q.UpdateProfile(e.ctx, store.UpdateProfileParams{ID: user.ID,
		Timezone: pgtype.Text{String: "America/Vancouver", Valid: true}}); err != nil {
		t.Fatal(err)
	}
	set := func(args string) {
		t.Helper()
		e.llm.script = []string{calls("set_goals", args)}
		if _, _, err := e.text(user, "x"); err != nil {
			t.Fatal(err)
		}
	}

	at(5, 9)
	set(`{"goals":[{"metric":"calories","value":2000,"unit":null},{"metric":"water","value":2,"unit":"l"}]}`)
	// 11 PM Thursday in Vancouver is already Friday in UTC; the change
	// still starts Thursday, the user's local day.
	at(8, 23)
	set(`{"goals":[{"metric":"calories","value":1800,"unit":null}]}`)
	at(8, 23)
	set(`{"goals":[{"metric":"calories","value":1700,"unit":null}]}`)

	on := func(d int) string {
		t.Helper()
		gs, err := e.q.GoalsOn(e.ctx, store.GoalsOnParams{UserID: user.ID,
			Day: pgtype.Date{Time: time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC), Valid: true}})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range gs {
			out = append(out, fmt.Sprintf("%s=%d", g.Metric, g.Value.Int32))
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		day  int
		want string
	}{
		{4, ""},
		{5, "calories=2000 water=2000"},
		{7, "calories=2000 water=2000"},
		{8, "calories=1700 water=2000"},
		{12, "calories=1700 water=2000"},
	} {
		if got := on(c.day); got != c.want {
			t.Errorf("goals on Oct %d = %q, want %q", c.day, got, c.want)
		}
	}
	if got := e.goals(user.ID); got != "calories=1700 water=2000" {
		t.Errorf("latest goals = %s", got)
	}
	rows, err := e.agent.Pool.Query(e.ctx, `SELECT metric || ' ' || start_date::text FROM goals WHERE user_id = $1 ORDER BY id`, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		got = append(got, s)
	}
	if want := "[calories 2026-10-05 water 2026-10-05 calories 2026-10-08]"; fmt.Sprint(got) != want {
		t.Errorf("goal rows = %v, want %s", got, want)
	}
}

func TestProgress(t *testing.T) {
	goal := func(v int32) store.Goal { return store.Goal{Value: pgtype.Int4{Int32: v, Valid: true}} }
	for _, c := range []struct {
		metric string
		total  int
		g      store.Goal
		set    bool
		want   string
	}{
		{"calories", 1240, goal(1800), true, "1,240 / 1,800 cal today, 560 cal to go"},
		{"calories", 1800, goal(1800), true, "1,800 / 1,800 cal today, goal hit"},
		{"calories", 1950, goal(1800), true, "1,950 / 1,800 cal today, 150 cal over"},
		{"protein", 170, goal(150), true, "170 / 150 g today, goal hit"},
		{"fat", 80, goal(70), true, "80 / 70 g today, 10 g over"},
		{"water", 750, goal(2000), true, "750 ml / 2 L today, 1.25 L to go"},
		{"water", 2500, goal(2000), true, "2.5 / 2 L today, goal hit"},
		{"calories", 350, store.Goal{}, true, "350 cal today"},
		{"calories", 350, store.Goal{}, false, "350 cal today"},
	} {
		m, _ := metricByName(c.metric)
		if got := progress(m, c.total, c.g, c.set, "today"); got != c.want {
			t.Errorf("%s %d = %q, want %q", c.metric, c.total, got, c.want)
		}
	}
}
