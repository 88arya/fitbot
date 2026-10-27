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
