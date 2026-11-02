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

// The state drives the user's nutrition targets. Code works out maintenance
// from their stats, applies the state's daily change, splits the result into
// macros, and saves it all as ordinary goals, so every tracker reads the
// targets the way it reads any goal. target_changes says how each set was
// worked out and why it changed. A user on the manual tier sets their own
// goals and none of this runs.

const (
	tierManual = "manual"
	tierCustom = "custom"
	// maxDailyDelta is the most calories a day from maintenance, either way:
	// 7,000 a week.
	maxDailyDelta = 1000
	// maxDeficitShare is the most of maintenance, in percent, a cut takes.
	maxDeficitShare = 30
	// caloriesPerWeeklyPound is the daily change that moves a pound a week.
	caloriesPerWeeklyPound = caloriesPerPound / 7
	// maxWeeklyLossPercent is the fastest loss, as a percent of body weight.
	maxWeeklyLossPercent = 1
	// minBMI is the BMI below which a cut is refused.
	minBMI = 18.5
	// minCutAge is the youngest a user can cut.
	minCutAge = 18
	// minMaleCalories and minFemaleCalories are the lowest targets.
	minMaleCalories   = 1500
	minFemaleCalories = 1200
	// Macro rules. Protein and fat are per pound of body weight, each kept
	// to maxMacroShare percent of calories so carbs always get the rest.
	proteinPerPoundCut = 1.0
	proteinPerPound    = 0.8
	fatPerPound        = 0.35
	maxMacroShare      = 35
)

// tierDeltas are the presets' daily changes from maintenance.
var tierDeltas = map[string]int{
	"light": -250, "moderate": -500, "aggressive": -750, "extreme": -1000,
	"lean": 100, "standard": 250, "dirty": 500,
}

// tierNames are the values set_state takes, for its schema.
var tierNames = []any{"light", "moderate", "aggressive", "extreme", "lean", "standard", "dirty", tierManual, nil}

var activityFactors = map[string]float64{
	"sedentary": 1.2, "light": 1.375, "moderate": 1.55, "active": 1.725, "very_active": 1.9,
}

var activityLevels = []string{"sedentary", "light", "moderate", "active", "very_active"}

// mifflin is the Mifflin-St Jeor BMR.
func mifflin(kg, cm float64, age int, sex string) int {
	bmr := 10*kg + 6.25*cm - 5*float64(age)
	if sex == "male" {
		return int(math.Round(bmr + 5))
	}
	return int(math.Round(bmr - 161))
}

// nearest10 rounds to the nearest 10, so targets read cleanly and the
// worked-out sums shown to the user add up.
func nearest10(n int) int {
	return int(math.Round(float64(n)/10)) * 10
}

func minCalories(sex string) int {
	if sex == "male" {
		return minMaleCalories
	}
	return minFemaleCalories
}

// deficitLimit is the largest safe daily deficit for someone at maintenance
// and weight, in tenths of a pound, and what sets it.
func deficitLimit(maintenance, weight int, sex string) (int, string) {
	limit, why := maxDailyDelta, "7,000 cal a week is the most Fitbot will set"
	if n := maintenance * maxDeficitShare / 100; n < limit {
		limit, why = n, fmt.Sprintf("it would take more than %d%% of your %s maintenance", maxDeficitShare, commas(maintenance))
	}
	if n := weight * maxWeeklyLossPercent * caloriesPerWeeklyPound / 1000; n < limit {
		limit, why = n, fmt.Sprintf("it would lose more than %d%% of your body weight a week", maxWeeklyLossPercent)
	}
	floor := minCalories(sex)
	if n := maintenance - floor; n < limit {
		limit, why = n, "it would put you under "+commas(floor)+" cal a day, the lowest Fitbot will set"
	}
	return max(limit/10*10, 0), why
}

// macroSplit divides calories into grams. Protein and fat are set by body
// weight unless the user set them, and carbs get what's left, so the macros'
// calories match the target to within a carb's 4 cal. It returns why not
// when the user's amounts leave nothing for carbs.
func macroSplit(calories, weight int, state string, pinProtein, pinFat pgtype.Int4) (protein, carbs, fat int, errText string) {
	pounds := float64(weight) / 10
	perPound := proteinPerPound
	if state == stateCut {
		perPound = proteinPerPoundCut
	}
	limit := float64(calories) * maxMacroShare / 100
	protein = int(math.Round(min(pounds*perPound, limit/4)))
	fat = int(math.Round(min(pounds*fatPerPound, limit/9)))
	if pinProtein.Valid {
		protein = int(pinProtein.Int32)
	}
	if pinFat.Valid {
		fat = int(pinFat.Int32)
	}
	carbs = (calories - 4*protein - 9*fat) / 4
	if carbs < 1 {
		return 0, 0, 0, fmt.Sprintf("%s g protein and %s g fat leave no calories for carbs out of %s cal", commas(protein), commas(fat), commas(calories))
	}
	return protein, carbs, fat, ""
}

// weeklyTenths is the weight change a daily calorie change moves in a week,
// in tenths of a pound.
func weeklyTenths(delta int) int {
	return int(math.Round(float64(delta) * 7 * 10 / caloriesPerPound))
}

// rateText says what a daily change moves, like "about 1 lbs a week, 4.3 lbs
// a month".
func rateText(delta int) string {
	if delta == 0 {
		return "holding weight"
	}
	verb := "gaining"
	if delta < 0 {
		verb, delta = "losing", -delta
	}
	month := int(math.Round(float64(delta) * 30 * 10 / caloriesPerPound))
	return fmt.Sprintf("%s about %s a week, %s a month", verb, lbs(weeklyTenths(delta)), lbs(month))
}

// currentAge is the user's age today, from the age they gave.
func currentAge(u store.User, today pgtype.Date) int {
	years := today.Time.Year() - u.AgeOn.Time.Year()
	if today.Time.YearDay() < u.AgeOn.Time.YearDay() {
		years--
	}
	return int(u.Age.Int32) + max(years, 0)
}

// missingStats names the stats a user hasn't given, weight aside.
func missingStats(u store.User) []string {
	var out []string
	if !u.HeightMm.Valid {
		out = append(out, "height")
	}
	if !u.Age.Valid {
		out = append(out, "age")
	}
	if !u.Sex.Valid {
		out = append(out, "sex")
	}
	if !u.Activity.Valid {
		out = append(out, "activity level")
	}
	return out
}

// stateRowOn is the state row in force on day, like stateOn.
func stateRowOn(states []store.BodyState, day pgtype.Date) (store.BodyState, bool) {
	if len(states) == 0 {
		return store.BodyState{}, false
	}
	out := states[0]
	for _, s := range states {
		if s.StartDate.Time.After(day.Time) {
			break
		}
		out = s
	}
	return out, true
}

func isManual(s store.BodyState) bool {
	return s.Tier.String == tierManual
}

// dailyDelta is the state's change from maintenance. ok is false until a cut
// or bulk has a rate.
func dailyDelta(s store.BodyState) (int, bool) {
	if s.CalorieDelta.Valid {
		return int(s.CalorieDelta.Int32), true
	}
	return 0, s.State == stateMaintain
}

// planName is the state as the user would say it, like "moderate cut".
func planName(state string, tier pgtype.Text) string {
	if state == stateMaintain || !tier.Valid || tier.String == tierManual {
		return state
	}
	return tier.String + " " + state
}

// stateBefore is the state row on a day before a change. Existed is false
// when the change added it.
type stateBefore struct {
	Existed bool    `json:"existed"`
	State   string  `json:"state,omitempty"`
	Tier    *string `json:"tier,omitempty"`
	Delta   *int32  `json:"delta,omitempty"`
	Protein *int32  `json:"protein,omitempty"`
	Fat     *int32  `json:"fat,omitempty"`
}

// targetsBefore is what reverses a change to the state or targets: rows on
// Day as they were, and the target_changes row it added.
type targetsBefore struct {
	Day      string       `json:"day"`
	Goals    []goalBefore `json:"goals,omitempty"`
	State    *stateBefore `json:"state,omitempty"`
	ChangeID int64        `json:"change_id,omitempty"`
}

func optText(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func optInt(n pgtype.Int4) *int32 {
	if !n.Valid {
		return nil
	}
	return &n.Int32
}

func loadStateBefore(ctx context.Context, q *store.Queries, userID int64, day pgtype.Date) (*stateBefore, error) {
	old, err := q.BodyStateOnDay(ctx, store.BodyStateOnDayParams{UserID: userID, StartDate: day})
	if errors.Is(err, pgx.ErrNoRows) {
		return &stateBefore{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &stateBefore{Existed: true, State: old.State, Tier: optText(old.Tier), Delta: optInt(old.CalorieDelta),
		Protein: optInt(old.ProteinG), Fat: optInt(old.FatG)}, nil
}

// targetOpts says how to work out new targets.
type targetOpts struct {
	reason, detail string
	// strict refuses a rate that isn't safe. Without it the rate is lowered
	// to the safe limit, for a change the user didn't make to the rate.
	strict bool
	// adjustment replaces the learned adjustment, if set.
	adjustment *int
	// keepEstimate keeps the last BMR, maintenance, and weight and the rate,
	// so a review changes only the adjustment. It checks the floor itself.
	keepEstimate bool
}

// targetResult is a saved set of targets.
type targetResult struct {
	before targetsBefore
	change store.TargetChange
	// text gives the targets and how they were worked out, for the reply.
	text string
}

// updateTargets works out the user's targets and saves them, starting today.
// It returns nil when they can't be worked out yet or the user sets their
// own, and why not when strict refuses.
func (a *Agent) updateTargets(ctx context.Context, q *store.Queries, user store.User, today pgtype.Date, o targetOpts) (*targetResult, string, error) {
	states, err := q.BodyStateHistory(ctx, user.ID)
	if err != nil {
		return nil, "", err
	}
	row, ok := stateRowOn(states, today)
	if !ok || isManual(row) {
		return nil, "", nil
	}
	delta, ok := dailyDelta(row)
	if !ok || len(missingStats(user)) > 0 {
		return nil, "", nil
	}
	last, err := q.LatestTargetChange(ctx, user.ID)
	hasLast := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, "", err
	}

	var bmr, tdee int
	var grams int
	adjustment := 0
	if hasLast {
		adjustment = int(last.Adjustment)
	}
	if o.adjustment != nil {
		adjustment = *o.adjustment
	}
	if o.keepEstimate && hasLast {
		bmr, tdee, grams = int(last.Bmr), int(last.Tdee), int(last.WeightGrams)
	} else {
		if grams, err = currentGrams(ctx, q, user.ID, today); err != nil {
			return nil, "", err
		}
		if grams == 0 {
			return nil, "", nil
		}
		bmr = mifflin(float64(grams)/1000, float64(user.HeightMm.Int32)/10, currentAge(user, today), user.Sex.String)
		tdee = nearest10(int(math.Round(float64(bmr) * activityFactors[user.Activity.String])))
	}
	weight := tenths(grams)
	maintenance := tdee + adjustment

	var before targetsBefore
	before.Day = today.Time.Format(time.DateOnly)
	note := ""
	if delta < 0 && !o.keepEstimate {
		if errText := cutRefusal(user, today, grams); errText != "" {
			if o.strict {
				return nil, errText, nil
			}
			note = " " + errText + " So the cut is on hold at maintenance."
			delta = 0
		} else if limit, why := deficitLimit(maintenance, weight, user.Sex.String); -delta > limit {
			if o.strict {
				return nil, fmt.Sprintf("a %s cal a day deficit isn't safe for you: %s. The most Fitbot will set is %s cal a day, %s. Ask if they want that instead.",
					commas(-delta), why, commas(limit), rateText(-limit)), nil
			}
			note = fmt.Sprintf(" The deficit came down to %s cal a day because %s.", commas(limit), why)
			delta = -limit
		}
		if note != "" {
			if before.State, err = loadStateBefore(ctx, q, user.ID, today); err != nil {
				return nil, "", err
			}
			row.CalorieDelta = pgtype.Int4{Int32: int32(delta), Valid: true}
			row.Tier = pgtype.Text{String: tierCustom, Valid: true}
			if err := saveStateRow(ctx, q, row, today); err != nil {
				return nil, "", err
			}
		}
	}

	calories := maintenance + delta
	protein, carbs, fat, errText := macroSplit(calories, weight, row.State, row.ProteinG, row.FatG)
	if errText != "" {
		if o.strict {
			return nil, errText, nil
		}
		// The user's own amounts no longer fit, so they give way.
		protein, carbs, fat, _ = macroSplit(calories, weight, row.State, pgtype.Int4{}, pgtype.Int4{})
	}

	for _, g := range []struct {
		metric string
		value  int
	}{{"calories", calories}, {"protein", protein}, {"carbs", carbs}, {"fat", fat}} {
		written, b, err := setTargetGoal(ctx, q, user.ID, g.metric, g.value, today)
		if err != nil {
			return nil, "", err
		}
		if written {
			before.Goals = append(before.Goals, b)
		}
	}
	change, err := q.InsertTargetChange(ctx, store.InsertTargetChangeParams{UserID: user.ID, LocalDate: today, Reason: o.reason, Detail: o.detail,
		State: row.State, Tier: row.Tier, CalorieDelta: int32(delta), Bmr: int32(bmr), Tdee: int32(tdee), Adjustment: int32(adjustment),
		Calories: int32(calories), Protein: int32(protein), Carbs: int32(carbs), Fat: int32(fat), WeightGrams: int32(grams)})
	if err != nil {
		return nil, "", err
	}
	before.ChangeID = change.ID
	return &targetResult{before: before, change: change, text: targetsText(change, user) + note}, "", nil
}

// cutRefusal says why this user shouldn't cut at all, or is empty.
func cutRefusal(user store.User, today pgtype.Date, grams int) string {
	if currentAge(user, today) < minCutAge {
		return fmt.Sprintf("Fitbot doesn't set a calorie deficit for anyone under %d. Suggest maintain, and a doctor or dietitian for weight loss.", minCutAge)
	}
	metres := float64(user.HeightMm.Int32) / 1000
	if bmi := float64(grams) / 1000 / (metres * metres); bmi < minBMI {
		return fmt.Sprintf("a cut isn't safe at a BMI of %s, under %s. Suggest maintain or a lean bulk instead.", decimal(bmi, 1), decimal(minBMI, 1))
	}
	return ""
}

// currentGrams is the user's weight for working out targets: the 7-day
// average, or the latest weigh-in, or 0 with none.
func currentGrams(ctx context.Context, q *store.Queries, userID int64, today pgtype.Date) (int, error) {
	t, err := loadWeightTrend(ctx, q, userID, today)
	if err != nil {
		return 0, err
	}
	if t.days > 0 {
		return int(math.Round(float64(t.avg) * gramsPerPound / 10)), nil
	}
	w, err := q.LastWeight(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return int(w.Grams), err
}

// setTargetGoal saves one target as a goal, and what it replaced. A macro the
// user stopped tracking stays stopped, so written is false.
func setTargetGoal(ctx context.Context, q *store.Queries, userID int64, metric string, value int, today pgtype.Date) (bool, goalBefore, error) {
	b := goalBefore{Metric: metric}
	if metric != "calories" {
		goals, err := q.GoalsOn(ctx, store.GoalsOnParams{UserID: userID, Day: today})
		if err != nil {
			return false, b, err
		}
		if g, ok := goalsByMetric(goals)[metric]; ok && !g.Value.Valid {
			return false, b, nil
		}
	}
	old, err := q.GoalOnDay(ctx, store.GoalOnDayParams{UserID: userID, Metric: metric, StartDate: today})
	switch {
	case err == nil:
		b.Existed = true
		if old.Value.Valid {
			b.Value = &old.Value.Int32
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return false, b, err
	}
	err = q.SetGoal(ctx, store.SetGoalParams{UserID: userID, Metric: metric, Value: pgtype.Int4{Int32: int32(value), Valid: true}, StartDate: today})
	return true, b, err
}

func saveStateRow(ctx context.Context, q *store.Queries, row store.BodyState, today pgtype.Date) error {
	return q.SetBodyState(ctx, store.SetBodyStateParams{UserID: row.UserID, State: row.State, StartDate: today, Tier: row.Tier,
		CalorieDelta: row.CalorieDelta, ProteinG: row.ProteinG, FatG: row.FatG})
}

// targetLine is the targets alone, like "2,230 cal, 175 g protein, 240 g
// carbs, 60 g fat".
func targetLine(c store.TargetChange) string {
	return fmt.Sprintf("%s cal, %s g protein, %s g carbs, %s g fat", commas(int(c.Calories)), commas(int(c.Protein)), commas(int(c.Carbs)), commas(int(c.Fat)))
}

// targetsText gives the targets and how they were worked out.
func targetsText(c store.TargetChange, user store.User) string {
	s := fmt.Sprintf("Targets: %s. Worked out from a BMR of %s cal, times %s for %s activity, for %s cal maintenance",
		targetLine(c), commas(int(c.Bmr)), decimal(activityFactors[user.Activity.String], 3),
		strings.ReplaceAll(user.Activity.String, "_", " "), commas(int(c.Tdee)))
	switch {
	case c.Adjustment > 0:
		s += fmt.Sprintf(", plus %s from your weight trend", commas(int(c.Adjustment)))
	case c.Adjustment < 0:
		s += fmt.Sprintf(", less %s from your weight trend", commas(int(-c.Adjustment)))
	}
	switch {
	case c.CalorieDelta < 0:
		s += fmt.Sprintf(", less %s for your %s", commas(int(-c.CalorieDelta)), planName(c.State, c.Tier))
	case c.CalorieDelta > 0:
		s += fmt.Sprintf(", plus %s for your %s", commas(int(c.CalorieDelta)), planName(c.State, c.Tier))
	}
	return s + ": " + rateText(int(c.CalorieDelta)) + ". These are estimates; weekly reviews adjust them from your real trend."
}

// restoreTargets reverses a change to the state or targets. A review may have
// moved the targets since, and those rows stay, so targets are worked out
// again from the restored state.
func (a *Agent) restoreTargets(ctx context.Context, q *store.Queries, userID int64, t *targetsBefore) error {
	day, err := parseDay(t.Day)
	if err != nil {
		return err
	}
	if err := restoreGoals(ctx, q, userID, day, t.Goals); err != nil {
		return err
	}
	if s := t.State; s != nil {
		if s.Existed {
			err = q.SetBodyState(ctx, store.SetBodyStateParams{UserID: userID, State: s.State, StartDate: day, Tier: text(s.Tier),
				CalorieDelta: int4(s.Delta), ProteinG: int4(s.Protein), FatG: int4(s.Fat)})
		} else {
			err = q.DeleteBodyState(ctx, store.DeleteBodyStateParams{UserID: userID, StartDate: day})
		}
		if err != nil {
			return err
		}
	}
	if t.ChangeID == 0 {
		return nil
	}
	if err := q.DeleteTargetChange(ctx, store.DeleteTargetChangeParams{ID: t.ChangeID, UserID: userID}); err != nil {
		return err
	}
	last, err := q.LatestTargetChange(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil || last.ID < t.ChangeID {
		return err
	}
	user, err := q.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	_, _, err = a.updateTargets(ctx, q, user, a.localDate(user), targetOpts{reason: "undo", detail: "an earlier change was undone"})
	return err
}

// planStatus says where the plan stands, for get_plan and the prompt.
func (a *Agent) planStatus(ctx context.Context, q *store.Queries, user store.User) (string, error) {
	today := a.localDate(user)
	states, err := q.BodyStateHistory(ctx, user.ID)
	if err != nil {
		return "", err
	}
	row, ok := stateRowOn(states, today)
	if !ok {
		return "No state chosen yet.", nil
	}
	if isManual(row) {
		return "The user sets their own goals (the manual tier), so there are no worked-out targets or weekly reviews. " +
			"Offer to work them out if they ask, with set_state and a rate.", nil
	}
	delta, ok := dailyDelta(row)
	if !ok {
		return "State " + row.State + ", waiting on a rate.", nil
	}
	last, err := q.LatestTargetChange(ctx, user.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Sprintf("Plan: %s, no targets yet; still needed: %s.", planName(row.State, row.Tier),
			strings.Join(append(missingStats(user), "a weigh-in"), ", ")), nil
	}
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan: %s since %s, %s a day from maintenance, %s. %s", planName(row.State, row.Tier), shortDate(row.StartDate),
		signed(delta), rateText(delta), targetsText(last, user))
	fmt.Fprintf(&b, " Maintenance estimate: %s cal. The target is the same every day, training or rest.", commas(int(last.Tdee+last.Adjustment)))

	first, err := q.FirstTargetChangeSince(ctx, store.FirstTargetChangeSinceParams{UserID: user.ID, Day: row.StartDate})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	t, err := loadWeightTrend(ctx, q, user.ID, today)
	if err != nil {
		return "", err
	}
	if first.ID != 0 && t.days > 0 {
		days := int(today.Time.Sub(row.StartDate.Time).Hours() / 24)
		start := tenths(int(first.WeightGrams))
		expected := int(math.Round(float64(delta) * float64(days) * 10 / caloriesPerPound))
		fmt.Fprintf(&b, " Since the plan started %s, the 7-day average went from %s to %s (%s), against about %s planned by now.",
			plural(days, "day")+" ago", lbs(start), lbs(t.avg), signedLbs(t.avg-start), signedLbs(expected))
	}
	if t.days > 0 && t.prevDays > 0 {
		fmt.Fprintf(&b, " Last 7 days against the 7 before: %s, planned %s a week.", signedLbs(t.avg-t.prev), signedLbs(weeklyTenths(delta)))
	}
	review, err := q.LatestTargetReview(ctx, user.ID)
	switch {
	case err == nil:
		fmt.Fprintf(&b, " Latest weekly review, for the week of %s: %s.", shortDate(review.WeekStart), strings.ReplaceAll(review.Outcome, "_", " "))
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	changes, err := q.RecentTargetChanges(ctx, store.RecentTargetChangesParams{UserID: user.ID, Count: 5})
	if err != nil {
		return "", err
	}
	var history []string
	for _, c := range changes {
		history = append(history, fmt.Sprintf("%s: %s cal, %s", shortDate(c.LocalDate), commas(int(c.Calories)), c.Detail))
	}
	b.WriteString(" Target history, newest first: " + strings.Join(history, "; ") + ".")
	return b.String(), nil
}

func signed(n int) string {
	if n > 0 {
		return "+" + commas(n) + " cal"
	}
	return commas(n) + " cal"
}

// signedLbs shows a change in tenths of a pound, like "-0.8 lbs".
func signedLbs(t int) string {
	if t > 0 {
		return "+" + lbs(t)
	}
	if t < 0 {
		return "-" + lbs(-t)
	}
	return "0 lbs"
}

type getPlanArgs struct{}

func (a *Agent) getPlan(ctx context.Context, c *call, _ getPlanArgs) (string, error) {
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	return a.planStatus(ctx, c.q, user)
}

// statArgs are the stats set_profile takes.
type statArgs struct {
	HeightFt *float64 `json:"height_ft"`
	HeightIn *float64 `json:"height_in"`
	HeightCm *float64 `json:"height_cm"`
	Age      *int     `json:"age"`
	Sex      *string  `json:"sex"`
	Activity *string  `json:"activity"`
}

func (s statArgs) given() bool {
	return s.HeightFt != nil || s.HeightIn != nil || s.HeightCm != nil || s.Age != nil || s.Sex != nil || s.Activity != nil
}

// applyStats puts the stats in s over the user's, and returns why not if one
// is out of range.
func applyStats(p *store.SetBodyStatsParams, s statArgs, today pgtype.Date) string {
	if s.HeightCm != nil && (s.HeightFt != nil || s.HeightIn != nil) {
		return "give height in feet and inches or in cm, not both"
	}
	if s.HeightIn != nil && s.HeightFt == nil {
		return "height_in needs height_ft"
	}
	mm := -1.0
	switch {
	case s.HeightCm != nil:
		mm = *s.HeightCm * 10
	case s.HeightFt != nil:
		inches := *s.HeightFt * 12
		if s.HeightIn != nil {
			inches += *s.HeightIn
		}
		mm = inches * 25.4
	}
	if s.HeightCm != nil || s.HeightFt != nil {
		if math.IsNaN(mm) || mm < 1000 || mm > 2500 {
			return "height must be from 100 to 250 cm (3'4\" to 8'2\")"
		}
		p.HeightMm = pgtype.Int4{Int32: int32(math.Round(mm)), Valid: true}
	}
	if s.Age != nil {
		if *s.Age < 13 || *s.Age > 100 {
			return "age must be from 13 to 100"
		}
		p.Age, p.AgeOn = pgtype.Int4{Int32: int32(*s.Age), Valid: true}, today
	}
	if s.Sex != nil {
		if *s.Sex != "male" && *s.Sex != "female" {
			return "sex must be male or female; the calorie formula needs one of these"
		}
		p.Sex = pgtype.Text{String: *s.Sex, Valid: true}
	}
	if s.Activity != nil {
		if !slices.Contains(activityLevels, *s.Activity) {
			return "activity must be one of " + strings.Join(activityLevels, ", ")
		}
		p.Activity = pgtype.Text{String: *s.Activity, Valid: true}
	}
	return ""
}

// statsState is the stats as they were, for undo.
type statsState struct {
	HeightMm *int32  `json:"height_mm"`
	Age      *int32  `json:"age"`
	AgeOn    *string `json:"age_on"`
	Sex      *string `json:"sex"`
	Activity *string `json:"activity"`
}

func statsOf(u store.User) *statsState {
	s := &statsState{HeightMm: optInt(u.HeightMm), Age: optInt(u.Age), Sex: optText(u.Sex), Activity: optText(u.Activity)}
	if u.AgeOn.Valid {
		d := u.AgeOn.Time.Format(time.DateOnly)
		s.AgeOn = &d
	}
	return s
}

func statsParams(u store.User) store.SetBodyStatsParams {
	return store.SetBodyStatsParams{ID: u.ID, HeightMm: u.HeightMm, Age: u.Age, AgeOn: u.AgeOn, Sex: u.Sex, Activity: u.Activity}
}

// restoreStats puts the stats back.
func restoreStats(ctx context.Context, q *store.Queries, userID int64, s *statsState) error {
	p := store.SetBodyStatsParams{ID: userID, HeightMm: int4(s.HeightMm), Age: int4(s.Age), Sex: text(s.Sex), Activity: text(s.Activity)}
	if s.AgeOn != nil {
		day, err := parseDay(*s.AgeOn)
		if err != nil {
			return err
		}
		p.AgeOn = day
	}
	return q.SetBodyStats(ctx, p)
}

// planGoals applies the calorie and macro goals in changes to a user whose
// targets code works out, so a goal the user names changes the plan rather
// than being overwritten at the next review. It returns the changes left for
// set_goals to save as usual, the targets, and why not.
func (a *Agent) planGoals(ctx context.Context, c *call, user store.User, today pgtype.Date, changes []goalChange) ([]goalChange, *targetResult, string, error) {
	states, err := c.q.BodyStateHistory(ctx, c.userID)
	if err != nil {
		return nil, nil, "", err
	}
	row, ok := stateRowOn(states, today)
	if !ok || isManual(row) {
		return changes, nil, "", nil
	}
	last, err := c.q.LatestTargetChange(ctx, c.userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return changes, nil, "", nil
	}
	if err != nil {
		return nil, nil, "", err
	}
	var rest []goalChange
	touched := false
	var adjustment *int
	var saved []string
	for _, ch := range changes {
		switch ch.m.name {
		case "calories":
			if !ch.value.Valid {
				return nil, nil, "calories drive the plan, so they can't be turned off; use set_state with tier manual to set goals by hand", nil
			}
			v := int(ch.value.Int32)
			if row.State == stateMaintain {
				n := v - int(last.Tdee)
				adjustment = &n
			} else {
				delta := v - int(last.Tdee+last.Adjustment)
				switch {
				case row.State == stateCut && delta >= 0:
					return nil, nil, fmt.Sprintf("%s cal isn't a deficit: maintenance is about %s cal. Ask if they want to change state.", commas(v), commas(int(last.Tdee+last.Adjustment))), nil
				case row.State == stateBulk && delta <= 0:
					return nil, nil, fmt.Sprintf("%s cal isn't a surplus: maintenance is about %s cal. Ask if they want to change state.", commas(v), commas(int(last.Tdee+last.Adjustment))), nil
				case max(delta, -delta) > maxDailyDelta:
					return nil, nil, fmt.Sprintf("%s cal is more than %s cal a day from your %s maintenance, the most Fitbot will set", commas(v), commas(maxDailyDelta), commas(int(last.Tdee+last.Adjustment))), nil
				}
				row.CalorieDelta = pgtype.Int4{Int32: int32(delta), Valid: true}
				row.Tier = pgtype.Text{String: tierCustom, Valid: true}
			}
			touched = true
			saved = append(saved, "calories "+commas(v))
		case "protein", "fat":
			if ch.m.name == "protein" {
				row.ProteinG = ch.value
			} else {
				row.FatG = ch.value
			}
			touched = true
			if ch.value.Valid {
				saved = append(saved, fmt.Sprintf("%s %d g", ch.m.name, ch.value.Int32))
			} else {
				rest = append(rest, ch)
			}
		case "carbs":
			// Carbs are what's left, so only stopping them is the user's.
			touched = true
			if !ch.value.Valid {
				rest = append(rest, ch)
			}
		default:
			rest = append(rest, ch)
		}
	}
	if !touched {
		return changes, nil, "", nil
	}
	sb, err := loadStateBefore(ctx, c.q, c.userID, today)
	if err != nil {
		return nil, nil, "", err
	}
	if err := saveStateRow(ctx, c.q, row, today); err != nil {
		return nil, nil, "", err
	}
	detail := "you set " + strings.Join(saved, " and ")
	if len(saved) == 0 {
		detail = "you changed which macros you track"
	}
	res, errText, err := a.updateTargets(ctx, c.q, user, today, targetOpts{reason: "goal", detail: detail, strict: true, adjustment: adjustment})
	if err != nil || errText != "" {
		return nil, nil, errText, err
	}
	if res == nil {
		res = &targetResult{before: targetsBefore{Day: today.Time.Format(time.DateOnly)}}
	}
	if res.before.State == nil {
		res.before.State = sb
	}
	return rest, res, "", nil
}
