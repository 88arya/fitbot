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

// maxSleep bounds one night, in minutes. More is more likely bedtime and
// wake time with AM and PM mixed up than a real night.
const maxSleep = 16 * 60

// sleepWeek is how many nights, ending with last night, the weekly average
// and nights on goal cover.
const sleepWeek = 7

type sleepArgs struct {
	Hours    *float64 `json:"hours"`
	Bedtime  *string  `json:"bedtime"`
	WakeTime *string  `json:"wake_time"`
	Date     *string  `json:"date"`
}

func (a *Agent) logSleep(ctx context.Context, c *call, args sleepArgs) (string, error) {
	var bed, wake pgtype.Time
	if errText := parseSleepTimes(args.Bedtime, args.WakeTime, &bed, &wake); errText != "" {
		return "error: " + errText, nil
	}
	var given *float64
	if args.Hours != nil {
		v := *args.Hours * 60
		given = &v
	}
	minutes, errText := sleepMinutes(given, bed, wake)
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
	if errText, err := nightFree(ctx, c, day, today, 0); errText != "" || err != nil {
		return errText, err
	}
	row, err := c.q.InsertSleep(ctx, store.InsertSleepParams{
		UserID:    c.userID,
		ActionID:  c.actionID,
		MessageID: c.msgID,
		LocalDate: day,
		Minutes:   minutes,
		Bedtime:   bed,
		WakeTime:  wake,
		LoggedAt:  pgtype.Timestamptz{Time: a.now(), Valid: true},
	})
	if err != nil {
		return "", err
	}
	summary := sleepSummary(row)
	c.undo = &undoRecord{Op: undoDeleteSleep, Desc: "logging " + summary + " for " + shortNight(day), ID: row.ID,
		Day: day.Time.Format(time.DateOnly), Metrics: []string{"sleep"}}
	st, err := a.sleepStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("logged #%d: %s %s. %s", row.ID, summary, nightWord(day, today), st.forModel()), nil
}

// nightFree returns an error for the model when a night other than id is
// already logged for day, since each day has one night.
func nightFree(ctx context.Context, c *call, day, today pgtype.Date, id int64) (string, error) {
	other, err := c.q.SleepOn(ctx, store.SleepOnParams{UserID: c.userID, Day: day})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", err
	case other.ID == id:
		return "", nil
	}
	return fmt.Sprintf("error: %s is already logged for %s as #%d; change it with edit_log instead",
		sleepSummary(other), nightWord(day, today), other.ID), nil
}

func (a *Agent) editSleep(ctx context.Context, c *call, args editLogArgs) (string, error) {
	if args.Amount == nil && args.Date == nil && args.Bedtime == nil && args.WakeTime == nil {
		return "error: no changes given", nil
	}
	if args.Amount == nil && args.Unit != nil {
		return "error: unit given without amount", nil
	}
	old, errText, err := targetSleep(ctx, c, args.ID)
	if errText != "" || err != nil {
		return errText, err
	}
	bed, wake := old.Bedtime, old.WakeTime
	if errText := parseSleepTimes(args.Bedtime, args.WakeTime, &bed, &wake); errText != "" {
		return "error: " + errText, nil
	}
	// A new amount is used as given. A changed time with both times known
	// sets the amount to the time between them; otherwise it stays.
	var given *float64
	switch sleep, _ := metricByName("sleep"); {
	case args.Amount != nil:
		// Sleep is stored in minutes, but given in hours.
		unit := args.Unit
		if unit == nil {
			unit = &sleep.unitOrder[0]
		}
		v, errText := inUnit(sleep, *args.Amount, unit)
		if errText != "" {
			return "error: " + errText, nil
		}
		given = &v
	case (args.Bedtime == nil && args.WakeTime == nil) || !bed.Valid || !wake.Valid:
		v := float64(old.Minutes)
		given = &v
	}
	minutes, errText := sleepMinutes(given, bed, wake)
	if errText != "" {
		return "error: " + errText, nil
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
	moved := !day.Time.Equal(old.LocalDate.Time)
	if moved {
		if errText, err := nightFree(ctx, c, day, today, old.ID); errText != "" || err != nil {
			return errText, err
		}
	}
	if _, err := c.q.UpdateSleep(ctx, store.UpdateSleepParams{ID: old.ID, UserID: c.userID, LocalDate: day, Minutes: minutes,
		Bedtime: bed, WakeTime: wake}); err != nil {
		return "", err
	}

	updated := old
	updated.LocalDate, updated.Minutes, updated.Bedtime, updated.WakeTime = day, minutes, bed, wake
	was, now := sleepSummary(old), sleepSummary(updated)
	var desc string
	switch {
	case moved && was == now:
		desc = fmt.Sprintf("moving %s from %s to %s", was, shortNight(old.LocalDate), shortNight(day))
	case moved:
		desc = fmt.Sprintf("changing %s to %s and moving it from %s to %s", was, now, shortNight(old.LocalDate), shortNight(day))
	default:
		desc = fmt.Sprintf("changing %s to %s for %s", was, now, shortNight(day))
	}
	oldMinutes := old.Minutes
	c.undo = &undoRecord{Op: undoRestoreSleep, Desc: desc, ID: old.ID, Amount: &oldMinutes,
		Bedtime: microsOf(old.Bedtime), WakeTime: microsOf(old.WakeTime),
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{"sleep"}}

	st, err := a.sleepStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	result := fmt.Sprintf("updated #%d: now %s %s. %s", old.ID, now, nightWord(day, today), st.night)
	if moved {
		prev, err := a.sleepStatus(ctx, c.q, user, old.LocalDate)
		if err != nil {
			return "", err
		}
		result += "; " + prev.night
	}
	return result + "; " + st.week + ".", nil
}

func (a *Agent) deleteSleep(ctx context.Context, c *call, id *int64) (string, error) {
	old, errText, err := targetSleep(ctx, c, id)
	if errText != "" || err != nil {
		return errText, err
	}
	if _, err := c.q.SetSleepDeleted(ctx, store.SetSleepDeletedParams{ID: old.ID, UserID: c.userID,
		DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}}); err != nil {
		return "", err
	}
	summary := sleepSummary(old)
	c.undo = &undoRecord{Op: undoUndeleteSleep, Desc: "deleting " + summary + " for " + shortNight(old.LocalDate), ID: old.ID,
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{"sleep"}}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	st, err := a.sleepStatus(ctx, c.q, user, old.LocalDate)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted #%d: %s %s. %s", old.ID, summary, nightWord(old.LocalDate, a.localDate(user)), st.forModel()), nil
}

// targetSleep finds the night with id, or the latest night when id is nil,
// or returns an error for the model.
func targetSleep(ctx context.Context, c *call, id *int64) (store.SleepLog, string, error) {
	var row store.SleepLog
	var err error
	if id == nil {
		row, err = c.q.LastSleep(ctx, c.userID)
	} else {
		row, err = c.q.GetSleep(ctx, store.GetSleepParams{ID: *id, UserID: c.userID})
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows) && id == nil:
		return row, "error: no sleep logged", nil
	case errors.Is(err, pgx.ErrNoRows):
		return row, fmt.Sprintf("error: no night #%d", *id), nil
	}
	return row, "", err
}

// parseSleepTimes sets bed and wake from 24-hour HH:MM times that are given,
// or returns why one is not a time.
func parseSleepTimes(bedtime, wakeTime *string, bed, wake *pgtype.Time) string {
	for _, f := range []struct {
		name string
		arg  *string
		dst  *pgtype.Time
	}{{"bedtime", bedtime, bed}, {"wake_time", wakeTime, wake}} {
		if f.arg == nil {
			continue
		}
		t, ok := timeOfDay(*f.arg)
		if !ok {
			return fmt.Sprintf("%s %q is not a 24-hour time like 23:30", f.name, *f.arg)
		}
		*f.dst = t
	}
	return ""
}

// timeOfDay parses a 24-hour HH:MM time.
func timeOfDay(s string) (pgtype.Time, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return pgtype.Time{}, false
	}
	return pgtype.Time{Microseconds: int64(t.Hour()*60+t.Minute()) * 60_000_000, Valid: true}, true
}

// sleepMinutes works out a night's length: given minutes when there are
// some, or else the time from bed to wake. Given minutes can't be more than
// the time from bed to wake when both are known.
func sleepMinutes(given *float64, bed, wake pgtype.Time) (int32, string) {
	sleep, _ := metricByName("sleep")
	window := 0
	if bed.Valid && wake.Valid {
		window = int((wake.Microseconds - bed.Microseconds) / 60_000_000)
		if window <= 0 {
			window += 24 * 60
		}
		if window > maxSleep {
			return 0, fmt.Sprintf("%s to %s is %s, more than %s; check the bedtime and wake time are the right AM or PM",
				clock(micros(bed), "3:04 PM"), clock(micros(wake), "3:04 PM"), sleep.format(window), sleep.format(maxSleep))
		}
	}
	if given == nil {
		if window == 0 {
			return 0, "give the hours slept, or both bedtime and wake time"
		}
		return int32(window), ""
	}
	v := *given
	if math.IsNaN(v) || math.IsInf(v, 0) || math.Round(v) < 1 {
		return 0, "sleep must be more than 0"
	}
	if v > maxSleep {
		return 0, "sleep must be at most " + sleep.format(maxSleep) + " a night"
	}
	n := int(math.Round(v))
	if window > 0 && n > window {
		return 0, fmt.Sprintf("%s is more than the %s from %s to %s", sleep.format(n), sleep.format(window),
			clock(micros(bed), "3:04 PM"), clock(micros(wake), "3:04 PM"))
	}
	return int32(n), ""
}

// sleepSummary shows a night, like "7 hours 30 min of sleep (11:30 PM to
// 7:00 AM)".
func sleepSummary(n store.SleepLog) string {
	sleep, _ := metricByName("sleep")
	s := sleep.format(int(n.Minutes)) + " of sleep"
	switch {
	case n.Bedtime.Valid && n.WakeTime.Valid:
		s += fmt.Sprintf(" (%s to %s)", clock(micros(n.Bedtime), "3:04 PM"), clock(micros(n.WakeTime), "3:04 PM"))
	case n.Bedtime.Valid:
		s += " (bed at " + clock(micros(n.Bedtime), "3:04 PM") + ")"
	case n.WakeTime.Valid:
		s += " (up at " + clock(micros(n.WakeTime), "3:04 PM") + ")"
	}
	return s
}

// nightWord names the night before day, relative to today: "last night",
// "the night before last", or "the night before Oct 3".
func nightWord(day, today pgtype.Date) string {
	switch {
	case day.Time.Equal(today.Time):
		return "last night"
	case day.Time.Equal(today.Time.AddDate(0, 0, -1)):
		return "the night before last"
	}
	return "the night before " + shortDate(day)
}

// shortNight names the night before day by date, like "the night before
// Oct 3", for undo, which may be read on a later day.
func shortNight(day pgtype.Date) string {
	return "the night before " + shortDate(day)
}

// nightStatus is a night against the sleep goal in force that day, and the
// last week of nights.
type nightStatus struct {
	// night is like "sleep: 7 hours / 8 hours last night, 1 hour short".
	night string
	// week is like "last 7 nights: 7 hours 12 min average over 5 logged, 3
	// of 5 on goal", and nights the nights it covers, oldest first.
	week   string
	nights []store.SleepLog
}

// sleepStatus is the night before day against that day's goal, and the
// week of nights ending last night.
func (a *Agent) sleepStatus(ctx context.Context, q *store.Queries, user store.User, day pgtype.Date) (nightStatus, error) {
	sleep, _ := metricByName("sleep")
	today := a.localDate(user)
	goals, err := q.GoalHistory(ctx, store.GoalHistoryParams{UserID: user.ID, Metric: "sleep"})
	if err != nil {
		return nightStatus{}, err
	}
	first := pgtype.Date{Time: today.Time.AddDate(0, 0, 1-sleepWeek), Valid: true}
	nights, err := q.SleepBetween(ctx, store.SleepBetweenParams{UserID: user.ID, First: first, Last: today})
	if err != nil {
		return nightStatus{}, err
	}

	st := nightStatus{nights: nights}
	when := nightWord(day, today)
	night, err := q.SleepOn(ctx, store.SleepOnParams{UserID: user.ID, Day: day})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		st.night = "sleep: nothing logged for " + when
	case err != nil:
		return nightStatus{}, err
	default:
		st.night = "sleep: " + sleepProgress(sleep, int(night.Minutes), goalOn(goals, day), when)
	}

	total, withGoal, onGoal := 0, 0, 0
	for _, n := range nights {
		total += int(n.Minutes)
		if g := goalOn(goals, n.LocalDate); g.Value.Valid {
			withGoal++
			if n.Minutes >= g.Value.Int32 {
				onGoal++
			}
		}
	}
	st.week = fmt.Sprintf("last %d nights: ", sleepWeek)
	switch len(nights) {
	case 0:
		st.week += "none logged"
		return st, nil
	case 1:
		st.week += sleep.format(total) + " over 1 logged"
	default:
		avg := int(math.Round(float64(total) / float64(len(nights))))
		st.week += fmt.Sprintf("%s average over %d logged", sleep.format(avg), len(nights))
	}
	if withGoal > 0 {
		st.week += fmt.Sprintf(", %d of %d on goal", onGoal, withGoal)
	}
	return st, nil
}

func (s nightStatus) forModel() string {
	return s.night + "; " + s.week + "."
}

// forUser is the status for a text, like "Sleep: 7 hours / 8 hours last
// night, 1 hour short. Last 7 nights: 7 hours average over 3 logged."
func (s nightStatus) forUser() string {
	return upperFirst(s.night) + ". " + upperFirst(s.week) + "."
}

// sleepProgress is a night against its goal, like "6 hours 30 min / 8 hours
// last night, 1 hour 30 min short", or just the night with no goal.
func sleepProgress(m metric, minutes int, g store.Goal, when string) string {
	if !g.Value.Valid {
		return m.format(minutes) + " " + when
	}
	goal := int(g.Value.Int32)
	s := m.format(minutes) + " / " + m.format(goal) + " " + when
	if minutes < goal {
		return s + ", " + m.format(goal-minutes) + " short"
	}
	return s + ", goal hit"
}

// goalOn is the goal in force on day from goals, which are oldest first.
func goalOn(goals []store.Goal, day pgtype.Date) store.Goal {
	var out store.Goal
	for _, g := range goals {
		if g.StartDate.Time.After(day.Time) {
			break
		}
		out = g
	}
	return out
}

// sleepSection is the prompt's list of the last week's nights, with ids for
// edit_log and delete_log, and where sleep stands.
func (a *Agent) sleepSection(ctx context.Context, user store.User) (string, error) {
	today := a.localDate(user)
	st, err := a.sleepStatus(ctx, a.Store, user, today)
	if err != nil {
		return "", fmt.Errorf("loading sleep: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n## Sleep\nNights logged in the last week, by the day the user woke up:\n")
	if len(st.nights) == 0 {
		b.WriteString("(none yet)\n")
	}
	for i := len(st.nights) - 1; i >= 0; i-- {
		n := st.nights[i]
		fmt.Fprintf(&b, "#%d %s (%s): %s\n", n.ID, n.LocalDate.Time.Format("Mon Jan 2"), nightWord(n.LocalDate, today), sleepSummary(n))
	}
	b.WriteString("Status: " + st.forModel() + "\n")
	b.WriteString("Log a night with log_sleep, using only what the user said: hours if they said how long, and bedtime and " +
		"wake_time if they said when (\"bed at 12\" is 00:00). With only the two times, the hours are worked out for you. A night " +
		"counts toward the day the user woke up: date null for last night, or the wake day for an earlier night. Answers to " +
		"\"How did you sleep?\" with hours or times are logs. If they give neither, ask how long they slept; never estimate. " +
		"Each day has one night: fix one with edit_log type sleep (amount in hours, bedtime, wake_time, or date) and remove " +
		"one with delete_log, by its id above or null for the latest night. Reply to a log with the status in the tool result.\n")
	return b.String(), nil
}
