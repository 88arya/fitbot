package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// maxSavedFoods is how many foods a user can have saved at once.
	maxSavedFoods = 50
	// maxFoodName is the longest a saved food's name can be.
	maxFoodName = 40
	// maxServings is the most servings of a saved food one log can add.
	maxServings = 10
)

// reservedFoodNames are texts a fast path or setup already answers, so a
// food saved under one could never be logged by its name.
var reservedFoodNames = map[string]bool{"none": true, "undo": true, "unmute": true, "snooze": true, "manual": true}

// foodName cleans a saved food's name: lowercase, single spaces, and no
// closing punctuation. It returns why the name can't be used, if it can't.
func foodName(s string) (string, string) {
	name := strings.Join(strings.Fields(strings.ToLower(s)), " ")
	name = strings.TrimRight(name, ".!?")
	switch {
	case name == "":
		return "", "name is empty"
	case len(name) > maxFoodName:
		return "", fmt.Sprintf("name must be at most %d characters", maxFoodName)
	case !strings.ContainsFunc(name, unicode.IsLetter):
		return "", "name needs a letter"
	case unicode.IsDigit(rune(name[0])):
		// "2 bottle" logs two bottles, so a name can't start with a number.
		return "", "name can't start with a number"
	case reservedFoodNames[name] || isUndo(name) || unmuteText.MatchString(name) || snoozeText.MatchString(name) ||
		isReplyWord(name) || bareWater.MatchString(name):
		return "", fmt.Sprintf("%q already means something else when texted on its own", name)
	}
	return name, ""
}

// foodAmounts are a saved food's amounts by metric, in canonical units,
// for the metrics it has.
func foodAmounts(f store.SavedFood) map[string]int32 {
	out := map[string]int32{}
	for name, v := range map[string]pgtype.Int4{"calories": f.Calories, "protein": f.Protein, "carbs": f.Carbs, "fat": f.Fat,
		"water": f.Water} {
		if v.Valid {
			out[name] = v.Int32
		}
	}
	return out
}

// foodSummary shows a saved food's amounts, like "450 cal, 30 g protein".
func foodSummary(f store.SavedFood) string {
	amounts := foodAmounts(f)
	var parts []string
	for _, name := range intakeMetrics {
		if v, ok := amounts[name]; ok {
			m, _ := metricByName(name)
			parts = append(parts, intakeSummary(m, v))
		}
	}
	return strings.Join(parts, ", ")
}

func foodID(f *store.SavedFood) pgtype.Int8 {
	if f == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: f.ID, Valid: true}
}

// logEntry shows one logged amount, with the saved food it came from.
func logEntry(m metric, l store.IntakeOnRow) string {
	s := intakeSummary(m, l.Amount)
	if l.Food.Valid {
		s += " (" + l.Food.String + ")"
	}
	return s
}

type saveFoodArgs struct {
	Action    string   `json:"action"`
	Name      string   `json:"name"`
	Calories  *float64 `json:"calories"`
	Protein   *float64 `json:"protein"`
	Carbs     *float64 `json:"carbs"`
	Fat       *float64 `json:"fat"`
	Water     *float64 `json:"water"`
	WaterUnit *string  `json:"water_unit"`
}

func (a *Agent) saveFood(ctx context.Context, c *call, args saveFoodArgs) (string, error) {
	name, errText := foodName(args.Name)
	if errText != "" {
		return "error: " + errText, nil
	}
	old, err := c.q.GetSavedFood(ctx, store.GetSavedFoodParams{UserID: c.userID, Name: name})
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	now := pgtype.Timestamptz{Time: a.now(), Valid: true}

	switch args.Action {
	case "remove":
		if args.Calories != nil || args.Protein != nil || args.Carbs != nil || args.Fat != nil || args.Water != nil || args.WaterUnit != nil {
			return "error: remove takes only a name", nil
		}
		if !found {
			return fmt.Sprintf("error: nothing is saved as %q", name), nil
		}
		if _, err := c.q.SetSavedFoodDeleted(ctx, store.SetSavedFoodDeletedParams{ID: old.ID, UserID: c.userID, DeletedAt: now}); err != nil {
			return "", err
		}
		c.undo = &undoRecord{Op: undoRestoreFood, Desc: fmt.Sprintf("removing %s (%s)", name, foodSummary(old)), Prev: old.ID}
		return fmt.Sprintf("removed %s (%s). Logs made from it stay.", name, foodSummary(old)), nil
	case "set":
	default:
		return fmt.Sprintf("error: unknown action %q", args.Action), nil
	}

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
	amounts := map[string]pgtype.Int4{}
	for _, metricName := range intakeMetrics {
		v := given[metricName]
		if v == nil {
			continue
		}
		m, _ := metricByName(metricName)
		n, errText := intakeAmount(m, *v)
		if errText != "" {
			return "error: " + errText, nil
		}
		amounts[metricName] = pgtype.Int4{Int32: n, Valid: true}
	}
	if len(amounts) == 0 {
		return "error: no amounts given; save only amounts the user gave", nil
	}

	if found {
		if _, err := c.q.SetSavedFoodDeleted(ctx, store.SetSavedFoodDeletedParams{ID: old.ID, UserID: c.userID, DeletedAt: now}); err != nil {
			return "", err
		}
	} else {
		n, err := c.q.CountSavedFoods(ctx, c.userID)
		if err != nil {
			return "", err
		}
		if n >= maxSavedFoods {
			return fmt.Sprintf("error: %d foods are saved, the most allowed; remove one first", maxSavedFoods), nil
		}
	}
	f, err := c.q.InsertSavedFood(ctx, store.InsertSavedFoodParams{UserID: c.userID, Name: name, Calories: amounts["calories"],
		Protein: amounts["protein"], Carbs: amounts["carbs"], Fat: amounts["fat"], Water: amounts["water"]})
	if err != nil {
		return "", err
	}
	undo := &undoRecord{Op: undoRestoreFood, Desc: fmt.Sprintf("saving %s (%s)", name, foodSummary(f)), ID: f.ID}
	result := fmt.Sprintf("saved %s: %s. Texting %q logs it.", name, foodSummary(f), name)
	if found {
		undo.Prev = old.ID
		undo.Desc = fmt.Sprintf("changing %s from %s to %s", name, foodSummary(old), foodSummary(f))
		result = fmt.Sprintf("changed %s from %s to %s. Earlier logs keep their amounts. Texting %q logs it.",
			name, foodSummary(old), foodSummary(f), name)
	}
	c.undo = undo
	return result, nil
}

type logFoodArgs struct {
	Name     string   `json:"name"`
	Servings *float64 `json:"servings"`
	Date     *string  `json:"date"`
}

func (a *Agent) logFood(ctx context.Context, c *call, args logFoodArgs) (string, error) {
	name, _ := foodName(args.Name)
	f, err := c.q.GetSavedFood(ctx, store.GetSavedFoodParams{UserID: c.userID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Sprintf("error: nothing is saved as %q; ask for the amounts, or save it first with save_food", args.Name), nil
	}
	if err != nil {
		return "", err
	}
	servings := 1.0
	if args.Servings != nil {
		servings = *args.Servings
	}
	if math.IsNaN(servings) || servings <= 0 || servings > maxServings {
		return fmt.Sprintf("error: servings must be more than 0 and at most %d", maxServings), nil
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
	given := map[string]*float64{}
	for metricName, v := range foodAmounts(f) {
		x := float64(v) * servings
		given[metricName] = &x
	}
	if servings != 1 {
		f.Name = servingsText(servings) + " " + f.Name
	}
	return a.insertIntake(ctx, c, user, day, given, &f)
}

// servingsText shows a number of servings, like "2x" or "1.5x".
func servingsText(n float64) string {
	return strconv.FormatFloat(n, 'f', -1, 64) + "x"
}

// savedFoodSection is the prompt's list of saved foods.
func (a *Agent) savedFoodSection(ctx context.Context, user store.User) (string, error) {
	foods, err := a.Store.ListSavedFoods(ctx, user.ID)
	if err != nil {
		return "", fmt.Errorf("loading saved foods: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n## Saved foods\n")
	if len(foods) == 0 {
		b.WriteString("(none saved)\n")
	}
	for _, f := range foods {
		fmt.Fprintf(&b, "%s: %s\n", f.Name, foodSummary(f))
	}
	b.WriteString("When the user names a saved food as something they ate or drank, log it with log_food, with servings " +
		"if they had more or less than one. Save one with save_food when they ask, like \"save that as my usual breakfast\" " +
		"(use the amounts they just logged) or \"my bottle is 750ml\"; saving a name again replaces it. Only save amounts " +
		"the user gave. Texting just a saved name, or a number and the name like \"2 bottle\", logs it without you.\n")
	return b.String(), nil
}

// savedFoodText matches a text that is only a saved food's name, maybe
// with a number of servings first, like "bottle", "2 bottles", or "2x usual
// breakfast". It returns the name and servings to look up, plural forms
// included.
var savedFoodText = regexp.MustCompile(`(?i)^\s*(?:(\d+(?:\.\d+)?)\s*x?\s+|(\d+(?:\.\d+)?)x)?\s*([^\d\s].*?)\s*[.!]?\s*$`)

// foodCandidates returns the names body could mean, and the servings.
func foodCandidates(body string) ([]string, float64, bool) {
	if len(body) > maxFoodName+10 {
		return nil, 0, false
	}
	m := savedFoodText.FindStringSubmatch(body)
	if m == nil {
		return nil, 0, false
	}
	servings := 1.0
	if n := m[1] + m[2]; n != "" {
		v, err := strconv.ParseFloat(n, 64)
		if err != nil || v <= 0 || v > maxServings {
			return nil, 0, false
		}
		servings = v
	}
	name := strings.Join(strings.Fields(strings.ToLower(m[3])), " ")
	names := []string{name}
	if servings != 1 {
		if s, ok := strings.CutSuffix(name, "es"); ok {
			names = append(names, s)
		}
		if s, ok := strings.CutSuffix(name, "s"); ok {
			names = append(names, s)
		}
	}
	return names, servings, true
}

// logSavedFood logs a text naming a saved food through log_food, so it is
// recorded and undone like any other call, and words the reply in code. It
// reports false when the text names no saved food.
func (a *Agent) logSavedFood(ctx context.Context, userID int64, in store.Message) (string, bool, error) {
	names, servings, ok := foodCandidates(in.Body)
	if !ok {
		return "", false, nil
	}
	var food store.SavedFood
	found := false
	for _, name := range names {
		f, err := a.Store.GetSavedFood(ctx, store.GetSavedFoodParams{UserID: userID, Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", true, err
		}
		food, found = f, true
		break
	}
	if !found {
		return "", false, nil
	}
	tracked, err := trackedMetrics(ctx, a.Store, userID)
	if err != nil {
		return "", true, err
	}
	what := food.Name
	if servings != 1 {
		what = servingsText(servings) + " " + what
	}
	amounts := foodAmounts(food)
	var logged, metrics, untracked []string
	for _, name := range intakeMetrics {
		v, ok := amounts[name]
		if !ok {
			continue
		}
		m, _ := metricByName(name)
		if !tracked[name] {
			untracked = append(untracked, m.label)
			continue
		}
		logged = append(logged, intakeSummary(m, int32(math.Round(float64(v)*servings))))
		metrics = append(metrics, name)
	}
	it := "it"
	if len(untracked) > 1 {
		it = "them"
	}
	if len(logged) == 0 {
		// The tool would refuse, so it isn't run and nothing is recorded.
		return fmt.Sprintf("Nothing logged: %s has only %s, and you don't track %s. Set a goal for %s to start.",
			food.Name, strings.Join(untracked, " and "), strings.Join(untracked, " or "), it), true, nil
	}

	args := map[string]any{"name": food.Name, "servings": nil, "date": nil}
	if servings != 1 {
		args["servings"] = servings
	}
	result := a.fastTool(ctx, userID, in.ID, "log_food", args)
	if errText, failed := strings.CutPrefix(result, "error: "); failed {
		return "Couldn't log that: " + errText + ".", true, nil
	}
	user, err := a.Store.GetUser(ctx, userID)
	if err != nil {
		return "", true, err
	}
	st, err := a.dayStatus(ctx, a.Store, user, a.localDate(user))
	if err != nil {
		return "", true, err
	}
	reply := "Logged " + what + ": " + strings.Join(logged, ", ") + ". " + st.forUser(metrics)
	if len(untracked) > 0 {
		reply += fmt.Sprintf(" Not logged: %s, since you don't track %s.", strings.Join(untracked, " and "), it)
	}
	return reply, true, nil
}

// restoreFood reverses save_food: the food it added is removed and the one
// it replaced or removed is back.
func (a *Agent) restoreFood(ctx context.Context, q *store.Queries, userID int64, rec undoRecord) error {
	if rec.ID != 0 {
		if _, err := q.SetSavedFoodDeleted(ctx, store.SetSavedFoodDeletedParams{ID: rec.ID, UserID: userID,
			DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}}); err != nil {
			return err
		}
	}
	if rec.Prev != 0 {
		if _, err := q.SetSavedFoodDeleted(ctx, store.SetSavedFoodDeletedParams{ID: rec.Prev, UserID: userID}); err != nil {
			return err
		}
	}
	return nil
}
