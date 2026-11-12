package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The outlook is where the 7-day average weight stands against the target
// weight: whether the state moves toward it, whether it's been reached, and
// when it would be at the planned rate and at the pace of the last two weeks.
// Code works out every date, so the model only repeats them.

const (
	// outlookWeeks is how many weeks back the actual pace is measured.
	outlookWeeks = 2
	// maxOutlookDays is the furthest out a date is given.
	maxOutlookDays = 2 * 365
	// holdBand is how close to the target, in tenths of a pound, a user on
	// maintain counts as at it.
	holdBand = 10
)

// goalOutlook is the numbers behind the outlook. Weights and rates are in
// tenths of a pound, rates signed a week.
type goalOutlook struct {
	state       string
	avg, target int
	// planned is the plan's rate, if hasPlan; actual the pace over the last
	// outlookWeeks weeks, if hasActual.
	planned, actual    int
	hasPlan, hasActual bool
	// start is the first weight from the week before the state began, if
	// hasStart, which tells a target passed from one set on the wrong side.
	start    int
	hasStart bool
}

// loadOutlook works out the outlook as of day. It reports false without a
// target weight in force, a state, or a weigh-in in the last week.
func loadOutlook(ctx context.Context, q *store.Queries, userID int64, day pgtype.Date) (goalOutlook, bool, error) {
	t, err := loadWeightTrend(ctx, q, userID, day)
	if err != nil || !t.hasTarget || t.days == 0 {
		return goalOutlook{}, false, err
	}
	states, err := q.BodyStateHistory(ctx, userID)
	if err != nil {
		return goalOutlook{}, false, err
	}
	row, ok := stateRowOn(states, day)
	if !ok {
		return goalOutlook{}, false, nil
	}
	o := goalOutlook{state: row.State, avg: t.avg, target: t.target}
	first, err := q.FirstWeightSince(ctx, store.FirstWeightSinceParams{UserID: userID, First: addDays(row.StartDate, 1-weightWeek)})
	switch {
	case err == nil:
		o.start, o.hasStart = tenths(int(first.Grams)), true
	case !errors.Is(err, pgx.ErrNoRows):
		return goalOutlook{}, false, err
	}
	if delta, ok := dailyDelta(row); ok && !isManual(row) {
		o.planned, o.hasPlan = weeklyTenths(delta), true
	}
	before, err := loadWeightTrend(ctx, q, userID, addDays(day, -7*outlookWeeks))
	if err != nil {
		return goalOutlook{}, false, err
	}
	if before.days > 0 {
		o.actual = int(math.Round(float64(t.avg-before.avg) / outlookWeeks))
		o.hasActual = true
	}
	return o, true, nil
}

// past reports whether the average is at or past the target in the
// direction the state moves weight.
func (o goalOutlook) past() bool {
	return o.state == stateCut && o.avg <= o.target || o.state == stateBulk && o.avg >= o.target
}

// started reports whether the state began on the far side of the target, so
// it was moving toward it.
func (o goalOutlook) started() bool {
	return o.hasStart && (o.state == stateCut && o.start > o.target || o.state == stateBulk && o.start < o.target)
}

// reached reports whether a cut or bulk has brought the average to the
// target, or a maintain holds it within holdBand.
func (o goalOutlook) reached() bool {
	if o.state == stateMaintain {
		return abs(o.avg-o.target) <= holdBand
	}
	return o.past() && (o.avg == o.target || o.started())
}

// away reports whether the state moves weight away from the target: it
// began on the same side the average is on now.
func (o goalOutlook) away() bool {
	return o.past() && o.avg != o.target && o.hasStart && !o.started()
}

// eta is the day the target is reached moving rate a week from the average
// on day, and whether rate gets there within maxOutlookDays.
func (o goalOutlook) eta(rate int, day pgtype.Date) (pgtype.Date, bool) {
	need := o.target - o.avg
	if rate == 0 || need == 0 || (need > 0) != (rate > 0) {
		return pgtype.Date{}, false
	}
	days := int(math.Round(float64(need) / float64(rate) * 7))
	if days > maxOutlookDays {
		return pgtype.Date{}, false
	}
	return addDays(day, max(days, 1)), true
}

// etaDate shows a date, with the year when it isn't day's.
func etaDate(d, day pgtype.Date) string {
	if d.Time.Year() != day.Time.Year() {
		return d.Time.Format("Jan 2, 2006")
	}
	return shortDate(d)
}

// weeksText says roughly how long until d, like "about 5 weeks".
func weeksText(d, day pgtype.Date) string {
	days := int(d.Time.Sub(day.Time).Hours() / 24)
	if days < 14 {
		return plural(days, "day")
	}
	return "about " + plural(int(math.Round(float64(days)/7)), "week")
}

// paceText is a weekly rate, like "0.6 lbs a week" or "+0.2 lbs a week".
func paceText(rate int) string {
	return lbs(abs(rate)) + " a week"
}

// forUser is the outlook for a text, like "At your planned 1 lbs a week,
// you'd reach 170 lbs around Dec 16; at your pace the last 2 weeks (0.6 lbs
// a week), around Jan 6." It is empty when there's nothing to add to the
// distance the weight line already shows.
func (o goalOutlook) forUser(day pgtype.Date) string {
	target := lbs(o.target)
	switch {
	case o.state != stateMaintain && o.reached():
		return fmt.Sprintf("You've reached your %s target.", target)
	case o.away():
		return fmt.Sprintf("Your %s target is %s your average, but a %s moves away from it.", target, aboveBelow(o.target-o.avg), o.state)
	case o.state == stateMaintain, o.past():
		// Past it with no weigh-in from the start: passed it or set it on
		// the wrong side, there's no telling which.
		return ""
	}
	var parts []string
	if o.hasPlan {
		if d, ok := o.eta(o.planned, day); ok {
			parts = append(parts, fmt.Sprintf("at your planned %s, you'd reach %s around %s", paceText(o.planned), target, etaDate(d, day)))
		}
	}
	if o.hasActual {
		d, ok := o.eta(o.actual, day)
		switch {
		case ok && len(parts) > 0:
			parts = append(parts, fmt.Sprintf("at your pace the last %d weeks (%s), around %s", outlookWeeks, paceText(o.actual), etaDate(d, day)))
		case ok:
			parts = append(parts, fmt.Sprintf("at your pace the last %d weeks (%s), you'd reach %s around %s", outlookWeeks,
				paceText(o.actual), target, etaDate(d, day)))
		case o.actual == 0 || (o.target-o.avg > 0) != (o.actual > 0):
			parts = append(parts, fmt.Sprintf("the last %d weeks moved %s a week, not toward it yet", outlookWeeks, signedLbs(o.actual)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return upperFirst(strings.Join(parts, "; ")) + "."
}

// forModel is the outlook for a tool result or the prompt.
func (o goalOutlook) forModel(day pgtype.Date) string {
	s := fmt.Sprintf("target weight %s, 7-day average %s", lbs(o.target), lbs(o.avg))
	switch {
	case o.state != stateMaintain && o.reached():
		return s + "; reached. Suggest maintain to hold it, or a new target"
	case o.away():
		return s + fmt.Sprintf("; the target is %s the average, so the %s moves away from it", aboveBelow(o.target-o.avg), o.state)
	case o.state == stateMaintain && o.reached():
		return s + "; holding at it on maintain"
	case o.state == stateMaintain:
		return s + "; maintain won't move toward it, a cut or bulk would"
	case o.past():
		return s + fmt.Sprintf("; the average is %s the target, and there's no weigh-in from when the %s began to tell whether "+
			"they passed it", aboveBelow(o.avg-o.target), o.state)
	}
	if o.hasPlan {
		if d, ok := o.eta(o.planned, day); ok {
			s += fmt.Sprintf("; at the planned %s a week, reached around %s (%s)", signedLbs(o.planned), etaDate(d, day), weeksText(d, day))
		} else {
			s += fmt.Sprintf("; the planned %s a week takes more than 2 years", signedLbs(o.planned))
		}
	}
	if o.hasActual {
		if d, ok := o.eta(o.actual, day); ok {
			s += fmt.Sprintf("; at the last %d weeks' pace of %s a week, around %s (%s)", outlookWeeks, signedLbs(o.actual), etaDate(d, day),
				weeksText(d, day))
		} else if o.actual == 0 || (o.target-o.avg > 0) != (o.actual > 0) {
			s += fmt.Sprintf("; the last %d weeks' pace of %s a week isn't moving toward it", outlookWeeks, signedLbs(o.actual))
		} else {
			s += fmt.Sprintf("; the last %d weeks' pace of %s a week doesn't get there within 2 years", outlookWeeks, signedLbs(o.actual))
		}
	} else {
		s += fmt.Sprintf("; no pace yet, it needs a weigh-in %d weeks ago", outlookWeeks)
	}
	return s + ". Give these dates as given; never work one out yourself"
}

func aboveBelow(d int) string {
	if d > 0 {
		return "above"
	}
	return "below"
}

// outlookFor is the outlook as of day for the model, or empty.
func (a *Agent) outlookFor(ctx context.Context, q *store.Queries, userID int64, day pgtype.Date) (string, error) {
	o, ok, err := loadOutlook(ctx, q, userID, day)
	if err != nil || !ok {
		return "", err
	}
	return "Target weight outlook: " + o.forModel(day) + ".", nil
}
