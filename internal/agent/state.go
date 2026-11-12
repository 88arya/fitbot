package agent

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
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
	State string  `json:"state"`
	Tier  *string `json:"tier"`
	// DailyCalories is a custom daily deficit or surplus, and WeeklyRate a
	// custom pace in RateUnit a week.
	DailyCalories *float64 `json:"daily_calories"`
	WeeklyRate    *float64 `json:"weekly_rate"`
	RateUnit      *string  `json:"rate_unit"`
}

// stateRate works out the tier and daily change a call asks for, keeping the
// current ones where it names none, or returns why not.
func stateRate(args stateArgs, old store.BodyState, hasOld bool) (pgtype.Text, pgtype.Int4, string) {
	var tier pgtype.Text
	var delta pgtype.Int4
	if args.DailyCalories != nil && args.WeeklyRate != nil {
		return tier, delta, "give daily_calories or weekly_rate, not both"
	}
	custom := args.DailyCalories != nil || args.WeeklyRate != nil
	if custom && args.Tier != nil {
		return tier, delta, "give a tier or a custom rate, not both"
	}
	switch {
	case custom:
		if args.State == stateMaintain {
			return tier, delta, "maintain has no rate"
		}
		var size float64
		if args.DailyCalories != nil {
			size = *args.DailyCalories
		} else {
			size = *args.WeeklyRate * caloriesPerPound / 7
			if args.RateUnit != nil && *args.RateUnit == "kg" {
				size *= 1000 / gramsPerPound
			}
		}
		if math.IsNaN(size) || size <= 0 {
			return tier, delta, "the rate must be a positive amount; the state says which way"
		}
		n := int(math.Round(size))
		if n > maxDailyDelta {
			return tier, delta, fmt.Sprintf("%s cal a day is more than the %s cal a day (7,000 a week) Fitbot will set either way. Suggest %s cal a day at most.",
				commas(n), commas(maxDailyDelta), commas(maxDailyDelta))
		}
		if args.State == stateCut {
			n = -n
		}
		return pgtype.Text{String: tierCustom, Valid: true}, pgtype.Int4{Int32: int32(n), Valid: true}, ""
	case args.Tier != nil && *args.Tier == tierManual:
		return pgtype.Text{String: tierManual, Valid: true}, delta, ""
	case args.Tier != nil:
		d, ok := tierDeltas[*args.Tier]
		if !ok || (args.State == stateCut) != (d < 0) || args.State == stateMaintain {
			return tier, delta, fmt.Sprintf("%q isn't a tier for %s: a cut is light, moderate, aggressive, or extreme, and a bulk is lean, standard, or dirty", *args.Tier, args.State)
		}
		return pgtype.Text{String: *args.Tier, Valid: true}, pgtype.Int4{Int32: int32(d), Valid: true}, ""
	case hasOld && isManual(old):
		return old.Tier, delta, ""
	case hasOld && old.State == args.State:
		return old.Tier, old.CalorieDelta, ""
	case args.State == stateMaintain:
		return tier, pgtype.Int4{Valid: true}, ""
	}
	return tier, delta, ""
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
	old, hasOld := stateRowOn(states, today)
	tier, delta, errText := stateRate(args, old, hasOld)
	if errText != "" {
		return "error: " + errText, nil
	}
	if hasOld && old.State == args.State && old.Tier == tier && old.CalorieDelta == delta {
		return "the state is already " + planName(args.State, tier), nil
	}
	// A change made earlier today is replaced, so undo puts it back;
	// otherwise undo removes today's row.
	before, err := loadStateBefore(ctx, c.q, c.userID, today)
	if err != nil {
		return "", err
	}
	row := store.BodyState{UserID: c.userID, State: args.State, Tier: tier, CalorieDelta: delta, ProteinG: old.ProteinG, FatG: old.FatG}
	if err := saveStateRow(ctx, c.q, row, today); err != nil {
		return "", err
	}
	result := "saved, starting today: state " + planName(args.State, tier) + ". Calories are on goal " + calorieRule(args.State) + "."
	targets := &targetsBefore{Day: today.Time.Format(time.DateOnly), State: before}
	if tier.String != tierManual {
		res, errText, err := a.updateTargets(ctx, c.q, user, today, targetOpts{reason: "plan", strict: true,
			detail: "you chose " + planName(args.State, tier)})
		if err != nil {
			return "", err
		}
		if errText != "" {
			return "error: " + errText, nil
		}
		if res != nil {
			targets.Goals, targets.ChangeID = res.before.Goals, res.before.ChangeID
			result += " " + res.text
		}
	}
	c.undo = &undoRecord{Op: undoRestoreState, Desc: "setting the state to " + planName(args.State, tier), Day: today.Time.Format(time.DateOnly), Targets: targets}
	return result, nil
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
	status, err := a.planStatus(ctx, a.Store, user)
	if err != nil {
		return "", fmt.Errorf("loading plan: %w", err)
	}
	b.WriteString(status + "\n")
	b.WriteString("The target state is bulk, maintain, or cut, the one the user chose, saved with set_state; a change starts today, " +
		"and earlier days keep the state they had. The actual state comes from the change in the 7-day average weight: gaining " +
		"is bulk, holding steady is maintain, losing is cut. The maintenance above comes from their logs and is only a check. " +
		"Use these as given to answer whether they're on track.\n" +
		"The state is the user's overall objective, and code works out their calorie and macro targets from it and saves them as " +
		"goals, so never work out a target yourself. Call set_state when the user clearly asks to change state or rate: tier for " +
		"a named pace, daily_calories or weekly_rate for their own. A tentative message like \"I think my loss has stalled\" or " +
		"\"maybe I should bulk\" isn't a change, so answer from get_plan or ask first. Something temporary, like a vacation or " +
		"a sick week, never changes the state. Reply to a change with the targets and how they were worked out from the tool " +
		"result; historical logs are kept and \"undo\" reverts it. If a tool refuses a rate as unsafe, explain why and offer the " +
		"alternative it gives. Height, age, sex, and activity are saved with set_profile, and targets follow. If the user " +
		"mentions pregnancy, an eating disorder, or a medical condition, don't change their targets and suggest they ask a " +
		"doctor. Tier manual means the user sets their own goals: never suggest or change a calorie goal for them unless they " +
		"ask. Answer \"am I on track\", \"how much should I have lost\", \"when will I reach my goal weight\", and \"why did my " +
		"target change\" from get_plan, and \"how " +
		"much is left\" or \"how much for dinner\" from get_day.\n")
	return b.String(), nil
}
