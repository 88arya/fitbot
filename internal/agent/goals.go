package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// defaultCalorieFloor is the lowest daily calorie goal Fitbot accepts when
// Agent.CalorieFloor is unset.
const defaultCalorieFloor = 1200

// metric is something the user can set a goal for. Goals are stored in a
// canonical unit and converted from whatever unit the user gave.
type metric struct {
	name  string
	label string
	// units maps each unit the model may pass to its size in the canonical
	// unit. The first listed in unitOrder is used when the model passes none.
	units     map[string]float64
	unitOrder []string
	// min and max bound the canonical value. Calories use the calorie floor
	// as their minimum instead.
	min, max int
	// per is how often the goal applies, like "a day"; empty for a target.
	per string
	// atLeast is true when going past the goal still counts as meeting it,
	// as with protein, rather than going over.
	atLeast bool
}

var metrics = []metric{
	{name: "calories", label: "calories", units: map[string]float64{"cal": 1}, unitOrder: []string{"cal"}, max: 10000, per: "a day"},
	{name: "protein", label: "protein", units: map[string]float64{"g": 1}, unitOrder: []string{"g"}, min: 1, max: 1000, per: "a day", atLeast: true},
	{name: "carbs", label: "carbs", units: map[string]float64{"g": 1}, unitOrder: []string{"g"}, min: 1, max: 1500, per: "a day"},
	{name: "fat", label: "fat", units: map[string]float64{"g": 1}, unitOrder: []string{"g"}, min: 1, max: 500, per: "a day"},
	{name: "water", label: "water", units: map[string]float64{"ml": 1, "l": 1000, "oz": 29.5735}, unitOrder: []string{"ml", "l", "oz"},
		min: 250, max: 10000, per: "a day", atLeast: true},
	{name: "sleep", label: "sleep", units: map[string]float64{"hours": 60}, unitOrder: []string{"hours"}, min: 3 * 60, max: 14 * 60, per: "a night",
		atLeast: true},
	// 50 to 1,000 lbs, in grams.
	{name: "weight", label: "target weight", units: map[string]float64{"lbs": gramsPerPound, "kg": 1000}, unitOrder: []string{"lbs", "kg"},
		min: 22680, max: 453592},
}

const gramsPerPound = 453.59237

func metricNames() []string {
	var out []string
	for _, m := range metrics {
		out = append(out, m.name)
	}
	return out
}

// goalUnits lists every unit set_goals takes, plus null for the default.
func goalUnits() []any {
	var out []any
	for _, m := range metrics {
		for _, u := range m.unitOrder {
			if !slices.Contains(out, any(u)) {
				out = append(out, u)
			}
		}
	}
	return append(out, nil)
}

func metricByName(name string) (metric, bool) {
	for _, m := range metrics {
		if m.name == name {
			return m, true
		}
	}
	return metric{}, false
}

// format shows a canonical value in the display unit: cal, g, ml or L,
// hours and minutes, or lbs.
func (m metric) format(v int) string {
	switch m.name {
	case "calories":
		return commas(v) + " cal"
	case "water":
		if v < 1000 {
			return commas(v) + " ml"
		}
		return decimal(float64(v)/1000, 2) + " L"
	case "sleep":
		var parts []string
		switch h := v / 60; {
		case h == 1:
			parts = append(parts, "1 hour")
		case h > 1:
			parts = append(parts, strconv.Itoa(h)+" hours")
		}
		if v%60 > 0 || v < 60 {
			parts = append(parts, strconv.Itoa(v%60)+" min")
		}
		return strings.Join(parts, " ")
	case "weight":
		return lbs(tenths(v))
	}
	return commas(v) + " g"
}

// describe is the goal as the prompt and tool results show it.
func (m metric) describe(g store.Goal) string {
	if !g.Value.Valid {
		return m.label + ": not tracking"
	}
	s := m.label + ": " + m.format(int(g.Value.Int32))
	if m.per != "" {
		s += " " + m.per
	}
	return s
}

// commas formats n with thousands separators, like 1,800.
func commas(n int) string {
	if n < 0 {
		return "-" + commas(-n)
	}
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// decimal formats f with at most places decimals and no trailing zeros.
func decimal(f float64, places int) string {
	s := strconv.FormatFloat(f, 'f', places, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func (a *Agent) calorieFloor() int {
	if a.CalorieFloor > 0 {
		return a.CalorieFloor
	}
	return defaultCalorieFloor
}

type goalArg struct {
	Metric string   `json:"metric"`
	Value  *float64 `json:"value"`
	Unit   *string  `json:"unit"`
}

type goalsArgs struct {
	Goals []goalArg `json:"goals"`
}

func (a *Agent) setGoals(ctx context.Context, c *call, args goalsArgs) (string, error) {
	if len(args.Goals) == 0 {
		return "error: no goals given", nil
	}
	type change struct {
		m     metric
		value pgtype.Int4
	}
	var changes []change
	seen := map[string]bool{}
	for _, g := range args.Goals {
		m, ok := metricByName(g.Metric)
		if !ok {
			return fmt.Sprintf("error: unknown metric %q", g.Metric), nil
		}
		if seen[m.name] {
			return fmt.Sprintf("error: %s is listed twice", m.name), nil
		}
		seen[m.name] = true
		if g.Value == nil {
			changes = append(changes, change{m: m})
			continue
		}
		v, errText := a.canonicalGoal(m, *g.Value, g.Unit)
		if errText != "" {
			return "error: " + errText, nil
		}
		changes = append(changes, change{m, pgtype.Int4{Int32: int32(v), Valid: true}})
	}

	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	undo := &undoRecord{Op: undoRestoreGoals, Day: today.Time.Format(time.DateOnly)}
	var saved []string
	for _, ch := range changes {
		// A change made earlier today is replaced, so undo puts its value
		// back; otherwise undo removes today's row.
		before := goalBefore{Metric: ch.m.name}
		old, err := c.q.GoalOnDay(ctx, store.GoalOnDayParams{UserID: c.userID, Metric: ch.m.name, StartDate: today})
		switch {
		case err == nil:
			before.Existed = true
			if old.Value.Valid {
				before.Value = &old.Value.Int32
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return "", err
		}
		undo.Goals = append(undo.Goals, before)
		if err := c.q.SetGoal(ctx, store.SetGoalParams{UserID: c.userID, Metric: ch.m.name, Value: ch.value, StartDate: today}); err != nil {
			return "", err
		}
		saved = append(saved, ch.m.describe(store.Goal{Value: ch.value}))
	}
	undo.Desc = "setting goals (" + strings.Join(saved, "; ") + ")"
	c.undo = undo
	return "saved, starting today: " + strings.Join(saved, "; "), nil
}

// canonicalGoal converts value in unit to m's canonical unit and checks it
// is in range, or returns why not.
func (a *Agent) canonicalGoal(m metric, value float64, unit *string) (int, string) {
	u := m.unitOrder[0]
	if unit != nil {
		u = strings.ToLower(strings.TrimSpace(*unit))
	}
	size, ok := m.units[u]
	if !ok {
		return 0, fmt.Sprintf("%s is not a unit for %s; use %s", u, m.name, strings.Join(m.unitOrder, " or "))
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0, fmt.Sprintf("%s must be a positive amount, or null to stop tracking it", m.name)
	}
	v := value * size
	lo, hi := m.min, m.max
	if m.name == "calories" {
		lo = a.calorieFloor()
	}
	if v > float64(hi) {
		return 0, fmt.Sprintf("%s must be at most %s", m.label, m.format(hi))
	}
	n := int(math.Round(v))
	if n < lo {
		if m.name == "calories" {
			return 0, fmt.Sprintf("%s is below the safe minimum of %s a day, so it was not saved. "+
				"Tell the user Fitbot won't set a goal that low and suggest a doctor or dietitian for one", m.format(n), m.format(lo))
		}
		return 0, fmt.Sprintf("%s must be at least %s", m.label, m.format(lo))
	}
	return n, ""
}

// goalsByMetric indexes goals by metric name.
func goalsByMetric(goals []store.Goal) map[string]store.Goal {
	out := make(map[string]store.Goal, len(goals))
	for _, g := range goals {
		out[g.Metric] = g
	}
	return out
}
