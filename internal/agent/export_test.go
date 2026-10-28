package agent

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// PlanLines shows what is planned on each of the n days from first, like
// "Wed Oct 28: legs + cardio at 6:00 PM", for tests outside the package.
func (a *Agent) PlanLines(ctx context.Context, userID int64, first time.Time, n int) ([]string, error) {
	start := pgtype.Date{Time: time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
	t, err := loadTraining(ctx, a.Store, userID, start, addDays(start, n-1))
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range n {
		d := addDays(start, i)
		out = append(out, dayName(d)+": "+t.on(d).describe())
	}
	return out, nil
}

// WorkoutWeek counts the planned sessions of the week starting monday as of
// now, like "week of Oct 26: 3 of 8 planned sessions done, 5 to go", and
// shows each of its days, like "legs done", for tests outside the package.
func (a *Agent) WorkoutWeek(ctx context.Context, userID int64, monday time.Time) (string, []string, error) {
	user, err := a.Store.GetUser(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	first := pgtype.Date{Time: time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
	r, err := loadWorkouts(ctx, a.Store, userID, first, addDays(first, 6))
	if err != nil {
		return "", nil, err
	}
	today := a.localDate(user)
	var days []string
	for i := range 7 {
		days = append(days, r.day(addDays(first, i)).describe(today))
	}
	return r.week(first, today), days, nil
}
