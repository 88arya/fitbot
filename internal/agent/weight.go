package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// weightWeek is how many days, ending today, the average weight covers.
// The weekly change compares it with the same number of days before.
const weightWeek = 7

type weightArgs struct {
	Weight *float64 `json:"weight"`
	Unit   *string  `json:"unit"`
	Date   *string  `json:"date"`
}

func (a *Agent) logWeight(ctx context.Context, c *call, args weightArgs) (string, error) {
	if args.Weight == nil {
		return "error: no weight given; ask the user what they weighed", nil
	}
	grams, errText := weightGrams(*args.Weight, args.Unit)
	if errText != "" {
		return "error: " + errText, nil
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	day := today
	if args.Date != nil {
		if day, errText = entryDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	row, err := c.q.InsertWeight(ctx, store.InsertWeightParams{
		UserID:    c.userID,
		ActionID:  c.actionID,
		MessageID: c.msgID,
		LocalDate: day,
		Grams:     grams,
		LoggedAt:  pgtype.Timestamptz{Time: a.now(), Valid: true},
	})
	if err != nil {
		return "", err
	}
	summary := weighIn(row.Grams)
	c.undo = &undoRecord{Op: undoDeleteWeight, Desc: "logging " + summary + " for " + shortDate(day), ID: row.ID,
		Day: day.Time.Format(time.DateOnly), Metrics: []string{"weight"}}
	// A weight given in kg shows what it was given as, so the conversion
	// can be checked.
	if args.Unit != nil && strings.EqualFold(strings.TrimSpace(*args.Unit), "kg") {
		summary += " (" + decimal(*args.Weight, 2) + " kg)"
	}
	st, err := a.weightStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("logged #%d: %s %s. %s", row.ID, summary, dayWord(day, today), st.forModel()), nil
}

// weightGrams converts a weight in unit, lbs when nil, to grams and checks
// it is in range, or returns why not.
func weightGrams(v float64, unit *string) (int32, string) {
	m, _ := metricByName("weight")
	if unit == nil {
		unit = &m.unitOrder[0]
	}
	g, errText := inUnit(m, v, unit)
	if errText != "" {
		return 0, errText
	}
	if math.IsNaN(g) || g < float64(m.min) || g > float64(m.max) {
		return 0, fmt.Sprintf("weight must be from %s to %s", m.format(m.min), m.format(m.max))
	}
	return int32(math.Round(g)), ""
}

func (a *Agent) editWeight(ctx context.Context, c *call, args editLogArgs) (string, error) {
	if args.Amount == nil && args.Date == nil {
		return "error: no changes given", nil
	}
	if args.Amount == nil && args.Unit != nil {
		return "error: unit given without amount", nil
	}
	old, errText, err := targetWeight(ctx, c, args.ID)
	if errText != "" || err != nil {
		return errText, err
	}
	grams := old.Grams
	if args.Amount != nil {
		if grams, errText = weightGrams(*args.Amount, args.Unit); errText != "" {
			return "error: " + errText, nil
		}
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	day := old.LocalDate
	if args.Date != nil {
		if day, errText = entryDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	if _, err := c.q.UpdateWeight(ctx, store.UpdateWeightParams{ID: old.ID, UserID: c.userID, LocalDate: day, Grams: grams}); err != nil {
		return "", err
	}

	was, now := weighIn(old.Grams), weighIn(grams)
	moved := !day.Time.Equal(old.LocalDate.Time)
	var desc string
	switch {
	case moved && was == now:
		desc = fmt.Sprintf("moving %s from %s to %s", was, shortDate(old.LocalDate), shortDate(day))
	case moved:
		desc = fmt.Sprintf("changing %s to %s and moving it from %s to %s", was, now, shortDate(old.LocalDate), shortDate(day))
	default:
		desc = fmt.Sprintf("changing %s to %s for %s", was, now, shortDate(day))
	}
	oldGrams := old.Grams
	c.undo = &undoRecord{Op: undoRestoreWeight, Desc: desc, ID: old.ID, Amount: &oldGrams,
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{"weight"}}

	st, err := a.weightStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	result := fmt.Sprintf("updated #%d: now %s %s. %s", old.ID, now, dayWord(day, today), st.day)
	if moved {
		prev, err := a.weightStatus(ctx, c.q, user, old.LocalDate)
		if err != nil {
			return "", err
		}
		result += "; " + prev.day
	}
	return result + "; " + st.trend.String() + ".", nil
}

func (a *Agent) deleteWeight(ctx context.Context, c *call, id *int64) (string, error) {
	old, errText, err := targetWeight(ctx, c, id)
	if errText != "" || err != nil {
		return errText, err
	}
	if _, err := c.q.SetWeightDeleted(ctx, store.SetWeightDeletedParams{ID: old.ID, UserID: c.userID,
		DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}}); err != nil {
		return "", err
	}
	summary := weighIn(old.Grams)
	c.undo = &undoRecord{Op: undoUndeleteWeight, Desc: "deleting " + summary + " for " + shortDate(old.LocalDate), ID: old.ID,
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{"weight"}}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	st, err := a.weightStatus(ctx, c.q, user, old.LocalDate)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted #%d: %s %s. %s", old.ID, summary, dayWord(old.LocalDate, a.localDate(user)), st.forModel()), nil
}

// targetWeight finds the weigh-in with id, or the most recently logged one
// when id is nil, or returns an error for the model.
func targetWeight(ctx context.Context, c *call, id *int64) (store.WeightLog, string, error) {
	var row store.WeightLog
	var err error
	if id == nil {
		row, err = c.q.LastWeight(ctx, c.userID)
	} else {
		row, err = c.q.GetWeight(ctx, store.GetWeightParams{ID: *id, UserID: c.userID})
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows) && id == nil:
		return row, "error: no weigh-ins logged", nil
	case errors.Is(err, pgx.ErrNoRows):
		return row, fmt.Sprintf("error: no weigh-in #%d", *id), nil
	}
	return row, "", err
}

// tenths converts grams to tenths of a pound, the precision weight is shown
// in. Averages and changes are worked out in tenths, so the numbers shown
// add up.
func tenths(grams int) int {
	return int(math.Round(float64(grams) / gramsPerPound * 10))
}

// lbs shows tenths of a pound, like "182.4 lbs".
func lbs(t int) string {
	return decimal(float64(t)/10, 1) + " lbs"
}

// weighIn shows a weight in grams, like "182.4 lbs".
func weighIn(grams int32) string {
	return lbs(tenths(int(grams)))
}

// weightTrend is the average weight over the week ending on a day, against
// the week before and the target weight in force that day. Weights are in
// tenths of a pound, and each day weighed counts its first weigh-in.
type weightTrend struct {
	// days and prevDays are how many days were weighed in the week and the
	// week before, and avg and prev their average weights.
	days, avg, prevDays, prev int
	// target is the target weight, if hasTarget.
	target    int
	hasTarget bool
	// logs are the week's weigh-ins, oldest first.
	logs []store.WeightLog
}

// loadWeightTrend works out the trend for the week ending on end.
func loadWeightTrend(ctx context.Context, q *store.Queries, userID int64, end pgtype.Date) (weightTrend, error) {
	rows, err := q.WeightBetween(ctx, store.WeightBetweenParams{UserID: userID, First: addDays(end, 1-2*weightWeek), Last: end})
	if err != nil {
		return weightTrend{}, err
	}
	goals, err := q.GoalHistory(ctx, store.GoalHistoryParams{UserID: userID, Metric: "weight"})
	if err != nil {
		return weightTrend{}, err
	}
	var t weightTrend
	if g := goalOn(goals, end); g.Value.Valid {
		t.target, t.hasTarget = tenths(int(g.Value.Int32)), true
	}
	start := addDays(end, 1-weightWeek)
	sum, prevSum := 0, 0
	var last pgtype.Date
	for _, r := range rows {
		inWeek := !r.LocalDate.Time.Before(start.Time)
		if inWeek {
			t.logs = append(t.logs, r)
		}
		// Rows are in order within a day, so only the first counts.
		if r.LocalDate.Time.Equal(last.Time) && last.Valid {
			continue
		}
		last = r.LocalDate
		if inWeek {
			t.days++
			sum += tenths(int(r.Grams))
		} else {
			t.prevDays++
			prevSum += tenths(int(r.Grams))
		}
	}
	if t.days > 0 {
		t.avg = int(math.Round(float64(sum) / float64(t.days)))
	}
	if t.prevDays > 0 {
		t.prev = int(math.Round(float64(prevSum) / float64(t.prevDays)))
	}
	return t, nil
}

// String is the week's average, its change from the week before, and the
// distance to the target, like "last 7 days: 182.6 lbs average over 5 days
// weighed, down 1.2 lbs from 183.8 lbs the 7 days before; target weight:
// 175 lbs, 7-day average 7.6 lbs above".
func (t weightTrend) String() string {
	s := fmt.Sprintf("last %d days: ", weightWeek)
	switch t.days {
	case 0:
		s += "no weigh-ins"
	case 1:
		s += lbs(t.avg) + " over 1 day weighed"
	default:
		s += fmt.Sprintf("%s average over %d days weighed", lbs(t.avg), t.days)
	}
	switch d := t.avg - t.prev; {
	case t.days == 0:
	case t.prevDays == 0:
		s += fmt.Sprintf(", none the %d days before to compare", weightWeek)
	case d < 0:
		s += fmt.Sprintf(", down %s from %s the %d days before", lbs(-d), lbs(t.prev), weightWeek)
	case d > 0:
		s += fmt.Sprintf(", up %s from %s the %d days before", lbs(d), lbs(t.prev), weightWeek)
	default:
		s += fmt.Sprintf(", the same as the %d days before", weightWeek)
	}
	if !t.hasTarget {
		return s
	}
	s += "; target weight: " + lbs(t.target)
	switch d := t.avg - t.target; {
	case t.days == 0:
	case d > 0:
		s += fmt.Sprintf(", %d-day average %s above", weightWeek, lbs(d))
	case d < 0:
		s += fmt.Sprintf(", %d-day average %s below", weightWeek, lbs(-d))
	default:
		s += fmt.Sprintf(", %d-day average on target", weightWeek)
	}
	return s
}

// weightDay is a day's weight and the trend as of today.
type weightDay struct {
	// day is like "weight: 182 lbs today".
	day   string
	trend weightTrend
}

func (a *Agent) weightStatus(ctx context.Context, q *store.Queries, user store.User, day pgtype.Date) (weightDay, error) {
	today := a.localDate(user)
	trend, err := loadWeightTrend(ctx, q, user.ID, today)
	if err != nil {
		return weightDay{}, err
	}
	logs, err := q.WeightBetween(ctx, store.WeightBetweenParams{UserID: user.ID, First: day, Last: day})
	if err != nil {
		return weightDay{}, err
	}
	st := weightDay{trend: trend}
	when := dayWord(day, today)
	switch len(logs) {
	case 0:
		st.day = "weight: nothing logged " + when
	case 1:
		st.day = "weight: " + weighIn(logs[0].Grams) + " " + when
	default:
		st.day = fmt.Sprintf("weight: %s %s (the first of %d weigh-ins that day)", weighIn(logs[0].Grams), when, len(logs))
	}
	return st, nil
}

func (s weightDay) forModel() string {
	return s.day + "; " + s.trend.String() + "."
}

// forUser is the status for a text, like "Weight: 182 lbs today. Last 7
// days: 182.6 lbs average over 5 days weighed. Target weight: 175 lbs."
func (s weightDay) forUser() string {
	parts := append([]string{s.day}, strings.Split(s.trend.String(), "; ")...)
	for i, p := range parts {
		parts[i] = upperFirst(p) + "."
	}
	return strings.Join(parts, " ")
}

// weightSection is the prompt's list of the last week's weigh-ins, with ids
// for edit_log and delete_log, and where weight stands.
func (a *Agent) weightSection(ctx context.Context, user store.User) (string, error) {
	today := a.localDate(user)
	st, err := a.weightStatus(ctx, a.Store, user, today)
	if err != nil {
		return "", fmt.Errorf("loading weight: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Body weight\nWeigh-ins in the last %d days, newest first:\n", weightWeek)
	logs := st.trend.logs
	if len(logs) == 0 {
		b.WriteString("(none yet)\n")
	}
	for i := len(logs) - 1; i >= 0; i-- {
		l := logs[i]
		fmt.Fprintf(&b, "#%d %s: %s", l.ID, dayLabel(l.LocalDate, today), weighIn(l.Grams))
		if i > 0 && logs[i-1].LocalDate.Time.Equal(l.LocalDate.Time) {
			b.WriteString(" (not the day's first, so not counted)")
		}
		b.WriteString("\n")
	}
	b.WriteString("Status: " + st.forModel() + "\n")
	b.WriteString("Log a weigh-in with log_weight, using the number the user gave: \"weighed 182\" is lbs, and pass kg only if " +
		"they said kg. If they say they weighed in without a number, ask what they weighed; never estimate. A day can have " +
		"several weigh-ins, and its first counts as its weight. The average, its change from the week before, and the " +
		"distance to the target weight are worked out for you, so use them as given. Fix a weigh-in with edit_log type " +
		"weight (amount in lbs or kg, or date) and remove one with delete_log, by its id above or null for the most " +
		"recently logged. Reply to a log with the status in the tool result. The target weight is a goal, set with set_goals.\n")
	return b.String(), nil
}
