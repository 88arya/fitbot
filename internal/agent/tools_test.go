package agent

import (
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCanonical(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{ "timezone": null, "name": "Sam" }`, `{"name":"Sam","timezone":null}`},
		{`{"id": 9007199254740993}`, `{"id":9007199254740993}`},
		{`{"ml": 500.0, "n": {"b": 1, "a": 2}}`, `{"ml":500.0,"n":{"a":2,"b":1}}`},
	} {
		got, err := canonical(c.in)
		if err != nil || string(got) != c.want {
			t.Errorf("canonical(%s) = %s, %v; want %s", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{``, `{"a":`, `{"a":1} {"b":2}`} {
		if _, err := canonical(in); err == nil {
			t.Errorf("canonical(%q) accepted invalid JSON", in)
		}
	}
}

func TestIsUndo(t *testing.T) {
	for _, in := range []string{"undo", "Undo", " UNDO. ", "undo that", "Undo it!", "undo last"} {
		if !isUndo(in) {
			t.Errorf("isUndo(%q) = false", in)
		}
	}
	for _, in := range []string{"", "undo the lunch", "can you undo", "undone", "redo"} {
		if isUndo(in) {
			t.Errorf("isUndo(%q) = true", in)
		}
	}
}

func TestWaterAmount(t *testing.T) {
	for _, c := range []struct {
		in   string
		v    float64
		unit string
	}{
		{"500ml", 500, "ml"}, {" 500 ML ", 500, "ml"}, {"1.5L", 1.5, "l"}, {"1 litre", 1, "l"},
		{"2 liters!", 2, "l"}, {"16 oz.", 16, "oz"}, {"+250ml", 250, "ml"},
	} {
		v, unit, ok := waterAmount(c.in)
		if !ok || v != c.v || unit != c.unit {
			t.Errorf("waterAmount(%q) = %v %q %v", c.in, v, unit, ok)
		}
	}
	for _, in := range []string{"", "500", "500 cal", "drank 500ml", "500ml and 300 cal", "ml", "1,000ml", "5 lbs"} {
		if _, _, ok := waterAmount(in); ok {
			t.Errorf("waterAmount(%q) matched", in)
		}
	}
}

func TestWeightTrend(t *testing.T) {
	for _, c := range []struct {
		t    weightTrend
		want string
	}{
		{weightTrend{}, "last 7 days: no weigh-ins"},
		{weightTrend{target: 1750, hasTarget: true}, "last 7 days: no weigh-ins; target weight: 175 lbs"},
		{weightTrend{days: 1, avg: 1824, prevDays: 0}, "last 7 days: 182.4 lbs over 1 day weighed, none the 7 days before to compare"},
		{weightTrend{days: 3, avg: 1824, prevDays: 2, prev: 1810, target: 1850, hasTarget: true},
			"last 7 days: 182.4 lbs average over 3 days weighed, up 1.4 lbs from 181 lbs the 7 days before; " +
				"target weight: 185 lbs, 7-day average 2.6 lbs below"},
		{weightTrend{days: 2, avg: 1750, prevDays: 5, prev: 1750, target: 1750, hasTarget: true},
			"last 7 days: 175 lbs average over 2 days weighed, the same as the 7 days before; target weight: 175 lbs, 7-day average on target"},
		// No weigh-ins this week means no change, even with some before.
		{weightTrend{prevDays: 4, prev: 1800}, "last 7 days: no weigh-ins"},
	} {
		if got := c.t.String(); got != c.want {
			t.Errorf("%+v = %q\nwant %q", c.t, got, c.want)
		}
	}
}

func TestStatsSpan(t *testing.T) {
	day := func(y int, m time.Month, d int) pgtype.Date {
		return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}
	}
	n := func(v int) *int { return &v }
	str := func(v string) *string { return &v }
	thu := day(2026, 10, 29)
	for _, c := range []struct {
		today        pgtype.Date
		args         statsArgs
		span, before string
	}{
		{thu, statsArgs{Period: "last_days", Days: n(7)}, "last 7 days (Oct 23 to Oct 29)", "the 7 days before (Oct 16 to Oct 22)"},
		{thu, statsArgs{Period: "last_days", Days: n(1)}, "today (Oct 29)", "the day before (Oct 28)"},
		{thu, statsArgs{Period: "this_week"}, "this week (Oct 26 to Oct 29)", "the week before (Oct 19 to Oct 25)"},
		{thu, statsArgs{Period: "last_week"}, "last week (Oct 19 to Oct 25)", "the week before (Oct 12 to Oct 18)"},
		{thu, statsArgs{Period: "this_month"}, "this month (Oct 1 to Oct 29)", "the month before (Sep 1 to Sep 30)"},
		{thu, statsArgs{Period: "last_month"}, "last month (Sep 1 to Sep 30)", "the month before (Aug 1 to Aug 31)"},
		{thu, statsArgs{Period: "dates", First: str("2026-10-03"), Last: str("2026-10-05")}, "Oct 3 to Oct 5",
			"the 3 days before (Sep 30 to Oct 2)"},
		{thu, statsArgs{Period: "dates", First: str("2026-10-20")}, "Oct 20 to Oct 29", "the 10 days before (Oct 10 to Oct 19)"},
		// On a Monday or the 1st, this week or month is just today.
		{day(2026, 10, 26), statsArgs{Period: "this_week"}, "this week (Oct 26)", "the week before (Oct 19 to Oct 25)"},
		{day(2026, 10, 1), statsArgs{Period: "this_month"}, "this month (Oct 1)", "the month before (Sep 1 to Sep 30)"},
		// Months of different lengths, and across a year.
		{day(2027, 3, 31), statsArgs{Period: "last_month"}, "last month (Feb 1 to Feb 28)", "the month before (Jan 1 to Jan 31)"},
		{day(2027, 1, 15), statsArgs{Period: "last_month"}, "last month (Dec 1 to Dec 31)", "the month before (Nov 1 to Nov 30)"},
		{day(2027, 1, 3), statsArgs{Period: "last_week"}, "last week (Dec 21 to Dec 27)", "the week before (Dec 14 to Dec 20)"},
	} {
		sp, errText := statsSpan(c.args, c.today)
		if errText != "" || sp.String() != c.span || sp.before().String() != c.before {
			t.Errorf("%+v on %s = %q, before %q, %q; want %q, before %q", c.args, dateKey(c.today), sp, sp.before(), errText,
				c.span, c.before)
		}
	}
}

func TestOnGoal(t *testing.T) {
	calories, _ := metricByName("calories")
	water, _ := metricByName("water")
	a := &Agent{}
	for _, c := range []struct {
		m           metric
		total, goal int
		want        bool
	}{
		{calories, 1800, 1800, true}, {calories, 1620, 1800, true}, {calories, 1619, 1800, false},
		{calories, 1980, 1800, true}, {calories, 1981, 1800, false},
		{water, 2500, 2500, true}, {water, 9000, 2500, true}, {water, 2499, 2500, false},
	} {
		if got := a.onGoal(c.m, c.total, c.goal); got != c.want {
			t.Errorf("onGoal(%s, %d, %d) = %v", c.m.name, c.total, c.goal, got)
		}
	}
	// A narrower band from config.
	a.GoalBand = 5
	if !a.onGoal(calories, 1890, 1800) || a.onGoal(calories, 1891, 1800) {
		t.Errorf("5%% band misjudged 1,890 or 1,891 against 1,800")
	}
}

func TestVerdict(t *testing.T) {
	calories, _ := metricByName("calories")
	water, _ := metricByName("water")
	goal := func(v int32) store.Goal { return store.Goal{Value: pgtype.Int4{Int32: v, Valid: true}} }
	a := &Agent{}
	for _, c := range []struct {
		m     metric
		total int
		g     store.Goal
		want  string
	}{
		{calories, 1700, goal(1800), "1,700 / 1,800 cal yesterday, 100 cal under, on goal"},
		{calories, 1800, goal(1800), "1,800 / 1,800 cal yesterday, on goal"},
		{calories, 2300, goal(1800), "2,300 / 1,800 cal yesterday, 500 cal over, off goal"},
		{calories, 1500, goal(1800), "1,500 / 1,800 cal yesterday, 300 cal under, off goal"},
		{calories, 1500, store.Goal{}, "1,500 cal yesterday"},
		{calories, 0, goal(1800), "nothing logged yesterday (goal 1,800 cal)"},
		{water, 0, store.Goal{}, "nothing logged yesterday"},
		{water, 1500, goal(2000), "1.5 / 2 L yesterday, 500 ml short, goal missed"},
		{water, 2600, goal(2000), "2.6 / 2 L yesterday, goal hit"},
	} {
		if got := a.verdict(c.m, c.total, c.g, "yesterday"); got != c.want {
			t.Errorf("verdict(%s, %d) = %q, want %q", c.m.name, c.total, got, c.want)
		}
	}
}
