package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/llm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// intakeMetrics are what log_intake adds to daily totals, calories first.
// Calories and water are always tracked; protein, carbs, and fat only when
// the user has a goal for them.
var intakeMetrics = []string{"calories", "protein", "carbs", "fat", "water"}

// maxIntake bounds one logged amount, in canonical units.
var maxIntake = map[string]int{"calories": 10000, "protein": 1000, "carbs": 1000, "fat": 1000, "water": 5000}

// logTypes are what edit_log and delete_log change: an intake metric, or a
// night of sleep.
var logTypes = append(slices.Clone(intakeMetrics), "sleep")

// intakeUnits are the units edit_log takes, plus null for the canonical one.
var intakeUnits = []any{"cal", "g", "ml", "l", "oz", "hours", nil}

// backfillDays is how far back a log or edit may date an amount.
const backfillDays = 30

type intakeArgs struct {
	Calories  *float64 `json:"calories"`
	Protein   *float64 `json:"protein"`
	Carbs     *float64 `json:"carbs"`
	Fat       *float64 `json:"fat"`
	Water     *float64 `json:"water"`
	WaterUnit *string  `json:"water_unit"`
	Date      *string  `json:"date"`
}

func (a *Agent) logIntake(ctx context.Context, c *call, args intakeArgs) (string, error) {
	given := map[string]*float64{"calories": args.Calories, "protein": args.Protein, "carbs": args.Carbs, "fat": args.Fat}
	if args.Water != nil {
		water, _ := metricByName("water")
		v, errText := inUnit(water, *args.Water, args.WaterUnit)
		if errText != "" {
			return "error: " + errText, nil
		}
		given["water"] = &v
	} else if args.WaterUnit != nil {
		return "error: water_unit given without water", nil
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	day := today
	if args.Date != nil {
		var errText string
		if day, errText = entryDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	tracked, err := trackedMetrics(ctx, c.q, c.userID)
	if err != nil {
		return "", err
	}

	type entry struct {
		m      metric
		amount int32
	}
	var entries []entry
	var skipped []string
	for _, name := range intakeMetrics {
		v := given[name]
		if v == nil {
			continue
		}
		m, _ := metricByName(name)
		n, errText := intakeAmount(m, *v)
		if errText != "" {
			return "error: " + errText, nil
		}
		if !tracked[m.name] {
			skipped = append(skipped, m.label)
			continue
		}
		entries = append(entries, entry{m, n})
	}
	it := "it"
	if len(skipped) > 1 {
		it = "them"
	}
	switch {
	case len(entries) == 0 && len(skipped) == 0:
		return "error: no amounts given", nil
	case len(entries) == 0:
		return fmt.Sprintf("error: the user doesn't track %s, so nothing was logged. They can start by setting a goal for %s",
			strings.Join(skipped, " or "), it), nil
	}

	undo := &undoRecord{Op: undoDeleteIntake, Day: day.Time.Format(time.DateOnly)}
	var summaries, logged []string
	for _, e := range entries {
		row, err := c.q.InsertIntake(ctx, store.InsertIntakeParams{
			UserID:    c.userID,
			ActionID:  c.actionID,
			MessageID: c.msgID,
			LocalDate: day,
			Metric:    e.m.name,
			Amount:    e.amount,
			LoggedAt:  pgtype.Timestamptz{Time: a.now(), Valid: true},
		})
		if err != nil {
			return "", err
		}
		undo.IDs = append(undo.IDs, row.ID)
		undo.Metrics = append(undo.Metrics, e.m.name)
		summaries = append(summaries, intakeSummary(e.m, e.amount))
		logged = append(logged, fmt.Sprintf("%s (#%d)", intakeSummary(e.m, e.amount), row.ID))
	}
	undo.Desc = "logging " + strings.Join(summaries, " and ")
	c.undo = undo

	st, err := a.dayStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	result := fmt.Sprintf("logged %s %s. %s", strings.Join(logged, ", "), dayWord(day, today), st.forModel())
	if len(skipped) > 0 {
		result += fmt.Sprintf(" Not logged: %s, because the user doesn't track %s. They can start by setting a goal for %s.",
			strings.Join(skipped, " and "), it, it)
	}
	return result, nil
}

// trackedMetrics reports which intake metrics the user tracks: calories and
// water always, and each macro with a goal set.
func trackedMetrics(ctx context.Context, q *store.Queries, userID int64) (map[string]bool, error) {
	goals, err := q.LatestGoals(ctx, userID)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{"calories": true, "water": true}
	for _, g := range goals {
		if g.Value.Valid {
			tracked[g.Metric] = true
		}
	}
	return tracked, nil
}

// inUnit converts v in unit to m's canonical unit, or returns why not. A nil
// unit means the canonical unit.
func inUnit(m metric, v float64, unit *string) (float64, string) {
	if unit == nil {
		return v, ""
	}
	u := strings.ToLower(strings.TrimSpace(*unit))
	size, ok := m.units[u]
	if !ok {
		return 0, fmt.Sprintf("%s is not a unit for %s; use %s", u, m.name, strings.Join(m.unitOrder, " or "))
	}
	return v * size, ""
}

type editLogArgs struct {
	Type     string   `json:"type"`
	ID       *int64   `json:"id"`
	Amount   *float64 `json:"amount"`
	Unit     *string  `json:"unit"`
	Bedtime  *string  `json:"bedtime"`
	WakeTime *string  `json:"wake_time"`
	Date     *string  `json:"date"`
}

func (a *Agent) editLog(ctx context.Context, c *call, args editLogArgs) (string, error) {
	if args.Type == "sleep" {
		return a.editSleep(ctx, c, args)
	}
	if args.Bedtime != nil || args.WakeTime != nil {
		return "error: bedtime and wake_time are only for sleep", nil
	}
	if args.Amount == nil && args.Date == nil {
		return "error: no changes given", nil
	}
	if args.Amount == nil && args.Unit != nil {
		return "error: unit given without amount", nil
	}
	old, m, errText, err := a.targetIntake(ctx, c, args.Type, args.ID)
	if errText != "" || err != nil {
		return errText, err
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	amount, day := old.Amount, old.LocalDate
	if args.Amount != nil {
		v, errText := inUnit(m, *args.Amount, args.Unit)
		if errText != "" {
			return "error: " + errText, nil
		}
		if amount, errText = intakeAmount(m, v); errText != "" {
			return "error: " + errText, nil
		}
	}
	if args.Date != nil {
		if day, errText = entryDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	if _, err := c.q.UpdateIntake(ctx, store.UpdateIntakeParams{ID: old.ID, UserID: c.userID, LocalDate: day, Amount: amount}); err != nil {
		return "", err
	}

	was, now := intakeSummary(m, old.Amount), intakeSummary(m, amount)
	moved := !day.Time.Equal(old.LocalDate.Time)
	var desc string
	switch {
	case moved && was == now:
		desc = fmt.Sprintf("moving %s from %s to %s", was, shortDate(old.LocalDate), shortDate(day))
	case moved:
		desc = fmt.Sprintf("changing %s to %s and moving it from %s to %s", was, now, shortDate(old.LocalDate), shortDate(day))
	default:
		desc = fmt.Sprintf("changing %s to %s", was, now)
	}
	oldAmount := old.Amount
	c.undo = &undoRecord{Op: undoRestoreIntake, Desc: desc, ID: old.ID, Amount: &oldAmount,
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{m.name}}

	st, err := a.dayStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	result := fmt.Sprintf("updated #%d: now %s %s. %s", old.ID, now, dayWord(day, today), st.forModel())
	if moved {
		prev, err := a.dayStatus(ctx, c.q, user, old.LocalDate)
		if err != nil {
			return "", err
		}
		result += " " + prev.forModel()
	}
	return result, nil
}

type deleteLogArgs struct {
	Type string `json:"type"`
	ID   *int64 `json:"id"`
}

func (a *Agent) deleteLog(ctx context.Context, c *call, args deleteLogArgs) (string, error) {
	if args.Type == "sleep" {
		return a.deleteSleep(ctx, c, args.ID)
	}
	old, m, errText, err := a.targetIntake(ctx, c, args.Type, args.ID)
	if errText != "" || err != nil {
		return errText, err
	}
	if _, err := c.q.SetIntakeDeleted(ctx, store.SetIntakeDeletedParams{ID: old.ID, UserID: c.userID,
		DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}}); err != nil {
		return "", err
	}
	summary := intakeSummary(m, old.Amount)
	c.undo = &undoRecord{Op: undoUndeleteIntake, Desc: "deleting " + summary, ID: old.ID,
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{m.name}}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	st, err := a.dayStatus(ctx, c.q, user, old.LocalDate)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted #%d: %s %s. %s", old.ID, summary, dayWord(old.LocalDate, a.localDate(user)), st.forModel()), nil
}

// targetIntake finds the amount of type with id, or the newest one of that
// type when id is nil, or returns an error for the model.
func (a *Agent) targetIntake(ctx context.Context, c *call, typ string, id *int64) (store.IntakeLog, metric, string, error) {
	var row store.IntakeLog
	if !slices.Contains(intakeMetrics, typ) {
		return row, metric{}, fmt.Sprintf("error: unknown log type %q", typ), nil
	}
	m, _ := metricByName(typ)
	var err error
	if id == nil {
		row, err = c.q.LastIntake(ctx, store.LastIntakeParams{UserID: c.userID, Metric: typ})
	} else {
		row, err = c.q.GetIntake(ctx, store.GetIntakeParams{ID: *id, UserID: c.userID})
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows) && id == nil:
		return row, m, fmt.Sprintf("error: no %s logged", m.label), nil
	case errors.Is(err, pgx.ErrNoRows):
		return row, m, fmt.Sprintf("error: no entry #%d", *id), nil
	case err != nil:
		return row, m, "", err
	case row.Metric != typ:
		return row, m, fmt.Sprintf("error: #%d is %s, not %s", row.ID, row.Metric, typ), nil
	}
	return row, m, "", nil
}

// intakeAmount rounds v to a whole number of m's canonical unit and checks
// it is more than 0 and at most the per-entry limit.
func intakeAmount(m metric, v float64) (int32, string) {
	if math.IsNaN(v) || math.IsInf(v, 0) || math.Round(v) < 1 {
		return 0, m.label + " must be more than 0"
	}
	if limit := maxIntake[m.name]; v > float64(limit) {
		return 0, fmt.Sprintf("%s must be at most %s per entry", m.label, m.format(limit))
	}
	return int32(math.Round(v)), ""
}

// entryDate parses a YYYY-MM-DD local date for a log, which must be today or
// up to backfillDays before.
func entryDate(s string, today pgtype.Date) (pgtype.Date, string) {
	day, err := parseDay(strings.TrimSpace(s))
	if err != nil {
		return day, fmt.Sprintf("date %q is not YYYY-MM-DD", s)
	}
	if day.Time.After(today.Time) {
		return day, "date is in the future; today is " + today.Time.Format(time.DateOnly)
	}
	if day.Time.Before(today.Time.AddDate(0, 0, -backfillDays)) {
		return day, fmt.Sprintf("date is more than %d days ago", backfillDays)
	}
	return day, ""
}

// intakeSummary shows an amount, like "350 cal", "30 g protein", or "500 ml
// water".
func intakeSummary(m metric, amount int32) string {
	s := m.format(int(amount))
	if m.name != "calories" {
		s += " " + m.label
	}
	return s
}

// statusLine is one metric's total against its goal for a day.
type statusLine struct {
	m    metric
	text string
}

// intakeDay is a day's totals against the goals in force that day: calories
// always, then each macro, and water, with a goal or anything logged.
type intakeDay struct {
	lines []statusLine
}

func (a *Agent) dayStatus(ctx context.Context, q *store.Queries, user store.User, day pgtype.Date) (intakeDay, error) {
	rows, err := q.IntakeTotals(ctx, store.IntakeTotalsParams{UserID: user.ID, Day: day})
	if err != nil {
		return intakeDay{}, err
	}
	totals := map[string]int{}
	for _, r := range rows {
		totals[r.Metric] = int(r.Total)
	}
	goals, err := q.GoalsOn(ctx, store.GoalsOnParams{UserID: user.ID, Day: day})
	if err != nil {
		return intakeDay{}, err
	}
	byMetric := goalsByMetric(goals)
	when := dayWord(day, a.localDate(user))
	var out intakeDay
	for _, name := range intakeMetrics {
		m, _ := metricByName(name)
		g, set := byMetric[m.name]
		if m.name != "calories" && totals[m.name] == 0 && !g.Value.Valid {
			continue
		}
		out.lines = append(out.lines, statusLine{m: m, text: progress(m, totals[m.name], g, set, when)})
	}
	return out, nil
}

// forModel is the day's status for a tool result or the prompt. A missing
// calorie goal needs no mention: it is a setup step, so setup asks for it.
func (d intakeDay) forModel() string {
	var parts []string
	for _, l := range d.lines {
		parts = append(parts, l.m.label+": "+l.text)
	}
	return strings.Join(parts, "; ") + "."
}

// forUser is the status of metrics for a text to the user, like "Calories:
// 1,240 / 1,800 cal today, 560 cal to go."
func (d intakeDay) forUser(metrics []string) string {
	var parts []string
	for _, l := range d.lines {
		if slices.Contains(metrics, l.m.name) {
			parts = append(parts, upperFirst(l.m.label)+": "+l.text+".")
		}
	}
	return strings.Join(parts, " ")
}

// progress is a total against a goal, like "1,240 / 1,800 cal today, 560
// cal to go", or just the total when there is no goal.
func progress(m metric, total int, g store.Goal, set bool, when string) string {
	if !set || !g.Value.Valid {
		return m.format(total) + " " + when
	}
	goal := int(g.Value.Int32)
	got, of := m.format(total), m.format(goal)
	if unit := of[strings.LastIndex(of, " "):]; strings.HasSuffix(got, unit) {
		got = strings.TrimSuffix(got, unit)
	}
	s := got + " / " + of + " " + when
	switch {
	case total < goal:
		return s + ", " + m.format(goal-total) + " to go"
	case total == goal || m.atLeast:
		return s + ", goal hit"
	}
	return s + ", " + m.format(total-goal) + " over"
}

// dayWord names day relative to today: "today", "yesterday", or "on Oct 3".
func dayWord(day, today pgtype.Date) string {
	switch {
	case day.Time.Equal(today.Time):
		return "today"
	case day.Time.Equal(today.Time.AddDate(0, 0, -1)):
		return "yesterday"
	}
	return "on " + shortDate(day)
}

func shortDate(day pgtype.Date) string {
	return day.Time.Format("Jan 2")
}

// intakeSection is the prompt's list of today's logged amounts, with ids for
// edit_log and delete_log, and today's totals.
func (a *Agent) intakeSection(ctx context.Context, user store.User) (string, error) {
	today := a.localDate(user)
	logs, err := a.Store.IntakeOn(ctx, store.IntakeOnParams{UserID: user.ID, Day: today})
	if err != nil {
		return "", fmt.Errorf("loading intake: %w", err)
	}
	st, err := a.dayStatus(ctx, a.Store, user, today)
	if err != nil {
		return "", fmt.Errorf("loading totals: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n## Logged today\n")
	if len(logs) == 0 {
		b.WriteString("(nothing logged yet)\n")
	}
	loc := a.location(user)
	for _, l := range logs {
		m, _ := metricByName(l.Metric)
		fmt.Fprintf(&b, "#%d at %s: %s\n", l.ID, l.LoggedAt.Time.In(loc).Format("3:04 PM"), intakeSummary(m, l.Amount))
	}
	b.WriteString("Totals: " + st.forModel() + "\n")
	b.WriteString("Add what the user ate and drank to today's totals with log_intake, using only the amounts they gave: " +
		"calories and water always, and protein, carbs, or fat only if the user tracks them (has a goal). Pass water in the " +
		"unit the user said. If they say they ate or drank something without an amount, ask how much it was; never " +
		"estimate or look one up. Amounts count toward today unless the user names another day. Reply to a log with the " +
		"totals in the tool result, like \"Logged 350 cal. 1,240 / 1,800 cal today, 560 cal to go.\" Fix an amount with " +
		"edit_log and remove one with delete_log, by its id above or null for the most recent of that type. The user can " +
		"text \"undo\" to reverse their last change, or a bare amount like \"500ml\" to log water.\n")
	return b.String(), nil
}

// bareWater matches a text that is only an amount of water, like "500ml",
// "1.5 L", or "16 oz", which logs water without the LLM.
var bareWater = regexp.MustCompile(`(?i)^\s*\+?(\d+(?:\.\d+)?)\s*(ml|l|oz|litres?|liters?)\s*[.!]?\s*$`)

// waterAmount returns the amount and unit of a bare water amount.
func waterAmount(body string) (float64, string, bool) {
	m := bareWater.FindStringSubmatch(body)
	if m == nil {
		return 0, "", false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, "", false
	}
	unit := strings.ToLower(m[2])
	if strings.HasPrefix(unit, "lit") {
		unit = "l"
	}
	return v, unit, true
}

// logWater logs a bare water amount through log_intake, so it is recorded
// and undone like any other call, and words the reply in code.
func (a *Agent) logWater(ctx context.Context, userID, msgID int64, v float64, unit string) (string, error) {
	args, err := json.Marshal(map[string]any{"calories": nil, "protein": nil, "carbs": nil, "fat": nil,
		"water": v, "water_unit": unit, "date": nil})
	if err != nil {
		return "", err
	}
	tc := llm.ToolCall{ID: "fast_path", Type: "function"}
	tc.Function.Name = "log_intake"
	tc.Function.Arguments = string(args)
	result := a.runTool(ctx, userID, msgID, tc, map[string]int{})
	if errText, failed := strings.CutPrefix(result, "error: "); failed {
		return "Couldn't log that: " + errText + ".", nil
	}
	user, err := a.Store.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	water, _ := metricByName("water")
	canonical, _ := inUnit(water, v, &unit)
	st, err := a.dayStatus(ctx, a.Store, user, a.localDate(user))
	if err != nil {
		return "", err
	}
	return "Logged " + intakeSummary(water, int32(math.Round(canonical))) + ". " + st.forUser([]string{"water"}), nil
}
