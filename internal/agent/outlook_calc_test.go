package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestGoalOutlook(t *testing.T) {
	day := pgtype.Date{Time: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), Valid: true}
	for _, c := range []struct {
		name    string
		o       goalOutlook
		reached bool
		user    string
		model   string
	}{
		{
			// 4.4 lbs at 1 lb a week is 31 days; at 0.6, 51 days.
			name: "cut on the way, plan and pace",
			o: goalOutlook{state: stateCut, avg: 1744, target: 1700, planned: -10, hasPlan: true, actual: -6, hasActual: true,
				start: 1800, hasStart: true},
			user: "At your planned 1 lbs a week, you'd reach 170 lbs around Dec 11; at your pace the last 2 weeks (0.6 lbs a week), around Dec 31.",
			model: "target weight 170 lbs, 7-day average 174.4 lbs; at the planned -1 lbs a week, reached around Dec 11 (about 4 weeks); " +
				"at the last 2 weeks' pace of -0.6 lbs a week, around Dec 31 (about 7 weeks)",
		},
		{
			name:  "cut stalled",
			o:     goalOutlook{state: stateCut, avg: 1744, target: 1700, planned: -10, hasPlan: true, actual: 2, hasActual: true},
			user:  "At your planned 1 lbs a week, you'd reach 170 lbs around Dec 11; the last 2 weeks moved +0.2 lbs a week, not toward it yet.",
			model: "the last 2 weeks' pace of +0.2 lbs a week isn't moving toward it",
		},
		{
			name:  "manual tier, pace only, into next year",
			o:     goalOutlook{state: stateBulk, avg: 1600, target: 1700, actual: 5, hasActual: true},
			user:  "At your pace the last 2 weeks (0.5 lbs a week), you'd reach 170 lbs around Mar 30, 2027.",
			model: "around Mar 30, 2027 (about 20 weeks)",
		},
		{
			name:  "no pace yet",
			o:     goalOutlook{state: stateCut, avg: 1744, target: 1700},
			model: "no pace yet, it needs a weigh-in 2 weeks ago",
		},
		{
			name:    "cut reached from above",
			o:       goalOutlook{state: stateCut, avg: 1698, target: 1700, start: 1800, hasStart: true},
			reached: true,
			user:    "You've reached your 170 lbs target.",
			model:   "reached. Suggest maintain",
		},
		{
			name:    "exactly on it, no start needed",
			o:       goalOutlook{state: stateBulk, avg: 1700, target: 1700},
			reached: true,
			user:    "You've reached your 170 lbs target.",
		},
		{
			name:  "bulk with the target below from the start",
			o:     goalOutlook{state: stateBulk, avg: 1837, target: 1750, start: 1840, hasStart: true},
			user:  "Your 175 lbs target is below your average, but a bulk moves away from it.",
			model: "the target is below the average, so the bulk moves away from it",
		},
		{
			name:  "past it with no starting weigh-in",
			o:     goalOutlook{state: stateCut, avg: 1650, target: 1700},
			model: "no weigh-in from when the cut began",
		},
		{
			name:    "maintain within a pound",
			o:       goalOutlook{state: stateMaintain, avg: 1709, target: 1700},
			reached: true,
			model:   "holding at it on maintain",
		},
		{
			name:  "maintain away from it",
			o:     goalOutlook{state: stateMaintain, avg: 1750, target: 1700},
			model: "maintain won't move toward it",
		},
		{
			name:  "too slow to give a date",
			o:     goalOutlook{state: stateCut, avg: 2400, target: 1700, planned: -2, hasPlan: true},
			model: "the planned -0.2 lbs a week takes more than 2 years",
		},
		{
			name:  "pace too slow to give a date",
			o:     goalOutlook{state: stateCut, avg: 2400, target: 1700, actual: -1, hasActual: true},
			model: "pace of -0.1 lbs a week doesn't get there within 2 years",
		},
	} {
		if got := c.o.reached(); got != c.reached {
			t.Errorf("%s: reached = %v", c.name, got)
		}
		if got := c.o.forUser(day); got != c.user {
			t.Errorf("%s: forUser\n got %q\nwant %q", c.name, got, c.user)
		}
		if got := c.o.forModel(day); !strings.Contains(got, c.model) {
			t.Errorf("%s: forModel\n got %q\nwant it to contain %q", c.name, got, c.model)
		}
	}
}
