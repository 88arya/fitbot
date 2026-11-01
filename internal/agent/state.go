package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// A user has a target state, the one they chose, which decides how calories
// are judged, and an actual state, worked out from their weigh-ins, which
// shows whether their calories are getting them there.

const (
	stateBulk     = "bulk"
	stateMaintain = "maintain"
	stateCut      = "cut"
)

var bodyStates = []string{stateBulk, stateMaintain, stateCut}

const (
	// maintenanceDays is how many days of calories maintenance averages, and
	// maintenanceLogged how many of them must be logged.
	maintenanceDays   = 14
	maintenanceLogged = 10
	// caloriesPerPound is the calories in a pound of body weight.
	caloriesPerPound = 3500
)

type stateArgs struct {
	State string `json:"state"`
}

func (a *Agent) setState(ctx context.Context, c *call, args stateArgs) (string, error) {
	if !slices.Contains(bodyStates, args.State) {
		return fmt.Sprintf("error: state %q must be one of %s", args.State, strings.Join(bodyStates, ", ")), nil
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	states, err := c.q.BodyStateHistory(ctx, c.userID)
	if err != nil {
		return "", err
	}
	if stateOn(states, today) == args.State {
		return "the state is already " + args.State, nil
	}
	// A change made earlier today is replaced, so undo puts it back;
	// otherwise undo removes today's row.
	undo := &undoRecord{Op: undoRestoreState, Desc: "setting the state to " + args.State, Day: today.Time.Format(time.DateOnly)}
	old, err := c.q.BodyStateOnDay(ctx, store.BodyStateOnDayParams{UserID: c.userID, StartDate: today})
	switch {
	case err == nil:
		undo.Value = &old.State
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	if err := c.q.SetBodyState(ctx, store.SetBodyStateParams{UserID: c.userID, State: args.State, StartDate: today}); err != nil {
		return "", err
	}
	c.undo = undo
	return "saved, starting today: state " + args.State + ". Calories are on goal " + calorieRule(args.State) + ".", nil
}

// stateOn is the state in force on day: the last to start by then, or the
// first for a day before any started, since a user is always in one. It is
// empty only for a user who hasn't picked one.
func stateOn(states []store.BodyState, day pgtype.Date) string {
	if len(states) == 0 {
		return ""
	}
	out := states[0].State
	for _, s := range states {
		if s.StartDate.Time.After(day.Time) {
			break
		}
		out = s.State
	}
	return out
}

// underGoal reports whether metric's goal is a limit to stay at or under in
// state. Only calories are, unless the user is bulking.
func underGoal(metric, state string) bool {
	return metric == "calories" && state != stateBulk
}

// calorieRule says when calories are on goal in state.
func calorieRule(state string) string {
	if underGoal("calories", state) {
		return "at or under the goal"
	}
	return "at or above the goal"
}

// stateTrend is the target state against what the weigh-ins show for the
// week ending on a day.
type stateTrend struct {
	state string
	// weighed is true with weigh-ins in that week and the week before, and
	// change is then the 7-day average's change between them, in tenths of a
	// pound.
	weighed bool
	change  int
	// limit is how many tenths of a pound a week count as holding.
	limit int
	// maintenance is the calories that would hold weight, or 0 without
	// enough logged to work out a sensible number.
	maintenance int
}

// loadStateTrend works out the trend for the week ending on end. Maintenance
// uses complete days only, so today's calories don't count.
func (a *Agent) loadStateTrend(ctx context.Context, q *store.Queries, userID int64, end, today pgtype.Date) (stateTrend, error) {
	states, err := q.BodyStateHistory(ctx, userID)
	if err != nil {
		return stateTrend{}, err
	}
	t := stateTrend{state: stateOn(states, end), limit: int(math.Round(a.settings().price(settingStateChange) * 10))}
	w, err := loadWeightTrend(ctx, q, userID, end)
	if err != nil {
		return stateTrend{}, err
	}
	if w.days == 0 || w.prevDays == 0 {
		return t, nil
	}
	t.weighed, t.change = true, w.avg-w.prev

	last := end
	if yesterday := addDays(today, -1); last.Time.After(yesterday.Time) {
		last = yesterday
	}
	rows, err := q.IntakeDaily(ctx, store.IntakeDailyParams{UserID: userID, Metric: "calories",
		First: addDays(last, 1-maintenanceDays), Last: last})
	if err != nil {
		return stateTrend{}, err
	}
	if len(rows) < maintenanceLogged {
		return t, nil
	}
	total := 0
	for _, r := range rows {
		total += int(r.Total)
	}
	// The two weeks' averages sit a week apart, so the change is over
	// weightWeek days.
	avg := float64(total) / float64(len(rows))
	t.maintenance = max(int(math.Round(avg-float64(t.change)/10*caloriesPerPound/weightWeek)), 0)
	return t, nil
}

// actual is the state the weigh-ins show, and movement what they show, like
// "gaining 0.6 lbs a week".
func (t stateTrend) actual() (state, movement string) {
	switch {
	case t.change > t.limit:
		return stateBulk, "gaining " + lbs(t.change) + " a week"
	case t.change < -t.limit:
		return stateCut, "losing " + lbs(-t.change) + " a week"
	}
	return stateMaintain, "holding steady"
}

func (t stateTrend) maintenanceText() string {
	m, _ := metricByName("calories")
	return "about " + m.format(t.maintenance)
}

// forModel is the trend for a tool result or the prompt, like "target state:
// cut; actual state: bulk, gaining 0.6 lbs a week; maintenance about 2,450
// cal".
func (t stateTrend) forModel() string {
	out := "target state: " + t.state + "; actual state: "
	if !t.weighed {
		return out + fmt.Sprintf("not enough weigh-ins, it needs one in each of the last two %d-day stretches", weightWeek)
	}
	state, movement := t.actual()
	out += state + ", " + movement + "; maintenance "
	if t.maintenance == 0 {
		return out + fmt.Sprintf("unknown, it needs calories logged on %d of the last %d days", maintenanceLogged, maintenanceDays)
	}
	return out + t.maintenanceText()
}

// forUser is the trend for a text, like "State: cut, actually gaining 0.6
// lbs a week. Maintenance is about 2,450 cal."
func (t stateTrend) forUser() string {
	if !t.weighed {
		return "State: " + t.state + ", not enough weigh-ins."
	}
	_, movement := t.actual()
	out := "State: " + t.state + ", actually " + movement + "."
	if t.maintenance > 0 {
		out += " Maintenance is " + t.maintenanceText() + "."
	}
	return out
}

// stateSection is the prompt's target and actual state.
func (a *Agent) stateSection(ctx context.Context, user store.User) (string, error) {
	today := a.localDate(user)
	t, err := a.loadStateTrend(ctx, a.Store, user.ID, today, today)
	if err != nil {
		return "", fmt.Errorf("loading state: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n## Body state\n")
	if t.state == "" {
		b.WriteString("target state: not set\n")
	} else {
		fmt.Fprintf(&b, "%s. Calories are on goal %s.\n", t.forModel(), calorieRule(t.state))
	}
	b.WriteString("The target state is bulk, maintain, or cut, the one the user chose, saved with set_state; a change starts today, " +
		"and earlier days keep the state they had. The actual state comes from the change in the 7-day average weight: gaining " +
		"is bulk, holding steady is maintain, losing is cut. Maintenance is the calories that would hold their weight, worked " +
		"out for you from their logs. Use these as given to answer whether they're on track, and never suggest or change a " +
		"calorie goal from them unless they ask to set one.\n")
	return b.String(), nil
}
