package agent

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestMifflin(t *testing.T) {
	for _, c := range []struct {
		kg, cm float64
		age    int
		sex    string
		want   int
	}{
		{80, 180, 30, "male", 1780},
		{60, 165, 25, "female", 1345},
	} {
		if got := mifflin(c.kg, c.cm, c.age, c.sex); got != c.want {
			t.Errorf("mifflin(%v, %v, %d, %s) = %d, want %d", c.kg, c.cm, c.age, c.sex, got, c.want)
		}
	}
}

func TestDeficitLimit(t *testing.T) {
	for _, c := range []struct {
		name        string
		maintenance int
		weight      int
		sex         string
		want        int
	}{
		{"7,000 a week caps a big user", 4000, 2800, "male", 1000},
		{"share of maintenance", 2730, 1750, "male", 810},
		{"1% of body weight", 3200, 1500, "male", 750},
		{"the calorie floor", 1700, 1400, "female", 500},
		{"no room at all", 1450, 1400, "male", 0},
	} {
		if got, why := deficitLimit(c.maintenance, c.weight, c.sex); got != c.want || why == "" {
			t.Errorf("%s: limit = %d (%q), want %d", c.name, got, why, c.want)
		}
	}
}

// maxMacroRounding is how far macros' calories can be from the target: whole
// grams of carbs miss by less than a gram's 4 cal.
const maxMacroRounding = 3

// TestMacroSplit checks macros add up to the target for every kind of user.
func TestMacroSplit(t *testing.T) {
	for _, state := range bodyStates {
		for calories := 1200; calories <= 5000; calories += 50 {
			for weight := 900; weight <= 4000; weight += 50 {
				p, c, f, errText := macroSplit(calories, weight, state, pgtype.Int4{}, pgtype.Int4{})
				if errText != "" {
					t.Fatalf("%s %d cal at %d: %s", state, calories, weight, errText)
				}
				total := 4*p + 4*c + 9*f
				if total > calories+maxMacroRounding || total < calories-maxMacroRounding {
					t.Fatalf("%s %d cal at %d: %dp %dc %df adds to %d", state, calories, weight, p, c, f, total)
				}
			}
		}
	}
	p, c, f, _ := macroSplit(2230, 1750, stateCut, pgtype.Int4{}, pgtype.Int4{})
	if p != 175 || c != 245 || f != 61 {
		t.Errorf("cut split = %d %d %d", p, c, f)
	}
	if p, _, _, _ := macroSplit(2230, 1750, stateBulk, pgtype.Int4{}, pgtype.Int4{}); p != 140 {
		t.Errorf("bulk protein = %d", p)
	}
	if p, _, _, _ := macroSplit(2230, 1750, stateCut, pgtype.Int4{Int32: 200, Valid: true}, pgtype.Int4{}); p != 200 {
		t.Errorf("the user's protein = %d", p)
	}
	if _, _, _, errText := macroSplit(1500, 1750, stateCut, pgtype.Int4{Int32: 400, Valid: true}, pgtype.Int4{}); errText == "" {
		t.Error("a protein goal over the calories should be refused")
	}
}

func TestRates(t *testing.T) {
	for delta, want := range map[int]string{
		100:  "gaining about 0.2 lbs a week, 0.9 lbs a month",
		250:  "gaining about 0.5 lbs a week, 2.1 lbs a month",
		500:  "gaining about 1 lbs a week, 4.3 lbs a month",
		-500: "losing about 1 lbs a week, 4.3 lbs a month",
		0:    "holding weight",
	} {
		if got := rateText(delta); got != want {
			t.Errorf("rateText(%d) = %q, want %q", delta, got, want)
		}
	}
}

func TestReviewJudge(t *testing.T) {
	good := reviewCheck{hasLast: true, hasBefore: true, settled: true, logged: true, near: true}
	with := func(planned, last, before int) reviewCheck {
		r := good
		r.planned, r.last, r.before = planned, last, before
		return r
	}
	for _, c := range []struct {
		name    string
		r       reviewCheck
		outcome string
		step    int
	}{
		{"close to plan", with(-10, -8, -8), "on_track", 0},
		{"the spec's slow week is only noted", with(-10, -1, -8), "watching", 0},
		{"two slow weeks lower calories, by at most 200", with(-10, -1, -1), "changed", -200},
		{"a small miss moves the least", with(-10, -6, -6), "changed", -100},
		{"a slow bulk raises calories", with(5, 0, 1), "changed", 100},
		{"a fast bulk lowers calories", with(5, 12, 10), "changed", -150},
		{"losing too fast raises calories", with(-10, -16, -15), "changed", 150},
		{"maintain drifting up", with(0, 5, 5), "changed", -150},
		{"weeks off opposite ways", with(-10, -1, -16), "watching", 0},
		{"too few weigh-ins", reviewCheck{planned: -10, last: 0, before: 0}, "unknown", 0},
	} {
		v := c.r.judge()
		if v.outcome != c.outcome || v.step != c.step {
			t.Errorf("%s: %+v, want %s %d", c.name, v, c.outcome, c.step)
		}
	}

	if v := with(0, 4, 2).judge(); v.outcome != "watching" {
		t.Errorf("one week inside the tolerance = %+v", v)
	}

	r := with(-10, -1, -1)
	r.settled = false
	if v := r.judge(); v.outcome != "watching" {
		t.Errorf("a target that just changed = %+v", v)
	}
	r = with(-10, -1, -1)
	r.logged = false
	if v := r.judge(); v.outcome != "held" {
		t.Errorf("no calories logged = %+v", v)
	}
	r = with(-10, -1, -1)
	r.near = false
	if v := r.judge(); v.outcome != "held" {
		t.Errorf("calories off target = %+v", v)
	}
}
