package agent

import "testing"

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
