package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/config"
	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/scheduler"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Settings are install-wide: a row in settings overrides the default from
// env, and every user shares it. Secrets, the sender allowlist, and server
// addresses stay in env, so no text can reach them.

// Settings, by key.
const (
	settingModel       = "model"
	settingInputPrice  = "input_price"
	settingOutputPrice = "output_price"
	settingLogLevel    = "log_level"
	settingFloor       = "calorie_floor"
	settingBand        = "goal_band"
	settingTimezone    = "default_timezone"
	settingWake        = "default_wake"
	settingBedtime     = "default_bedtime"
	settingFollowup    = "gym_followup_delay"
	settingReping      = "supplement_reping"
	settingWater       = "water_nudges"
	settingMorning     = "morning_checkin"
	settingNight       = "night_checkin"
	settingGym         = "gym_reminders"
	settingFollowups   = "gym_followups"
	settingSupplements = "supplement_reminders"
	settingWeekly      = "weekly_summary"
)

// resetValue is the value that puts a setting back to its default.
const resetValue = "default"

// settingDef is one setting. parse checks a value and returns it in the
// form it is stored.
type settingDef struct {
	key, label string
	// help says what values it takes, for the prompt.
	help  string
	parse func(string) (string, error)
}

var settingDefs = []settingDef{
	{settingModel, "model", "the OpenAI model that reads texts", parseModel},
	{settingInputPrice, "input price", "USD per 1M input tokens, 0 for unknown", parsePriceSetting},
	{settingOutputPrice, "output price", "USD per 1M output tokens, 0 for unknown", parsePriceSetting},
	{settingLogLevel, "log level", "debug, info, warn, or error", parseLogLevel},
	{settingFloor, "calorie floor", "the lowest daily calorie goal accepted, 1 to 5000", parseIntSetting(config.ParseCalorieFloor)},
	{settingBand, "goal band", "percent from the goal that calories, carbs, and fat stay on goal, 1 to 50", parseIntSetting(config.ParseGoalBand)},
	{settingTimezone, "default timezone", "IANA timezone for a user who hasn't given one", parseTimezone},
	{settingWake, "default wake time", "24-hour HH:MM for a user who hasn't given one", parseClock},
	{settingBedtime, "default bedtime", "24-hour HH:MM for a user who hasn't given one", parseClock},
	{settingFollowup, "gym follow-up delay", "how long after the gym reminder the follow-up goes, 15m to 6h", parseFollowup},
	{settingReping, "supplement re-ping", "how long after a supplement reminder it goes again, 15m to 6h, or off", parseReping},
	{settingWater, "water nudges", "the most water nudges a day, 1 to 4, or off", parseWaterNudges},
	{settingMorning, "morning check-in", "on or off", parseToggle},
	{settingNight, "night check-in", "on or off", parseToggle},
	{settingGym, "gym reminders", "on or off", parseToggle},
	{settingFollowups, "gym follow-ups", "on or off", parseToggle},
	{settingSupplements, "supplement reminders", "on or off; a re-ping needs these on", parseToggle},
	{settingWeekly, "weekly summary", "on or off", parseToggle},
}

func settingByKey(key string) (settingDef, bool) {
	i := slices.IndexFunc(settingDefs, func(d settingDef) bool { return d.key == key })
	if i < 0 {
		return settingDef{}, false
	}
	return settingDefs[i], true
}

func settingKeys() []string {
	keys := make([]string, len(settingDefs))
	for i, d := range settingDefs {
		keys[i] = d.key
	}
	return keys
}

func parseModel(v string) (string, error) {
	if v == "" || len(v) > 100 || strings.ContainsAny(v, " \t\r\n") {
		return "", fmt.Errorf("%q is not a model name", v)
	}
	return v, nil
}

func parsePriceSetting(v string) (string, error) {
	f, err := config.ParsePrice(v)
	if err != nil {
		return "", err
	}
	return formatPrice(f), nil
}

func formatPrice(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func parseLogLevel(v string) (string, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		return "", fmt.Errorf("%q is not debug, info, warn, or error", v)
	}
	return strings.ToLower(l.String()), nil
}

func parseIntSetting(parse func(string) (int, error)) func(string) (string, error) {
	return func(v string) (string, error) {
		n, err := parse(strings.ReplaceAll(v, ",", ""))
		if err != nil {
			return "", err
		}
		return strconv.Itoa(n), nil
	}
}

func parseTimezone(v string) (string, error) {
	if _, err := config.ParseTimezone(v); err != nil {
		return "", err
	}
	return v, nil
}

func parseClock(v string) (string, error) {
	t, ok := timeOfDay(v)
	if !ok {
		return "", fmt.Errorf("%q is not a 24-hour time like 07:30", v)
	}
	return clock(micros(t), "15:04"), nil
}

func parseFollowup(v string) (string, error) {
	d, err := config.ParseGymFollowup(v)
	if err != nil {
		return "", err
	}
	return shortDuration(d), nil
}

func parseReping(v string) (string, error) {
	if strings.EqualFold(v, "off") {
		v = "0"
	}
	d, err := config.ParseSupplementReping(v)
	if err != nil {
		return "", err
	}
	return shortDuration(d), nil
}

func parseWaterNudges(v string) (string, error) {
	if strings.EqualFold(v, "off") {
		v = "0"
	}
	return parseIntSetting(config.ParseWaterNudges)(v)
}

func parseToggle(v string) (string, error) {
	switch strings.ToLower(v) {
	case "on", "true", "yes":
		return "on", nil
	case "off", "false", "no":
		return "off", nil
	}
	return "", fmt.Errorf("%q is not on or off", v)
}

// shortDuration shows a duration the way it is stored, like "2h", "1h30m",
// or "0".
func shortDuration(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// settingValues holds every setting's current value in its stored form.
type settingValues map[string]string

func (v settingValues) int(key string) int {
	n, _ := strconv.Atoi(v[key])
	return n
}

func (v settingValues) duration(key string) time.Duration {
	d, _ := time.ParseDuration(v[key])
	return d
}

func (v settingValues) price(key string) float64 {
	f, _ := strconv.ParseFloat(v[key], 64)
	return f
}

func (v settingValues) on(key string) bool {
	return v[key] == "on"
}

// defaults is each setting's value with no row: the Agent's fields, which
// come from env, or the built-in default.
func (a *Agent) defaults() settingValues {
	v := settingValues{
		settingLogLevel:    strings.ToLower(a.DefaultLogLevel.String()),
		settingInputPrice:  "0",
		settingOutputPrice: "0",
		settingFloor:       strconv.Itoa(cmp.Or(a.CalorieFloor, defaultCalorieFloor)),
		settingBand:        strconv.Itoa(cmp.Or(a.GoalBand, defaultGoalBand)),
		settingTimezone:    a.DefaultTimezone,
		settingWake:        clock(scheduler.DefaultWake, "15:04"),
		settingBedtime:     clock(scheduler.DefaultBedtime, "15:04"),
		settingFollowup:    shortDuration(cmp.Or(a.GymFollowup, defaultGymFollowup)),
		settingReping:      shortDuration(a.SupplementReping),
		settingWater:       strconv.Itoa(a.WaterNudges),
		settingMorning:     "on",
		settingNight:       "on",
		settingGym:         "on",
		settingFollowups:   "on",
		settingSupplements: "on",
		settingWeekly:      "on",
	}
	if a.LLM != nil {
		input, output := a.LLM.Prices()
		v[settingModel] = a.LLM.Model()
		v[settingInputPrice], v[settingOutputPrice] = formatPrice(input), formatPrice(output)
	}
	return v
}

// settings are the values as of the last refresh, or the defaults before
// the first.
func (a *Agent) settings() settingValues {
	if v := a.current.Load(); v != nil {
		return *v
	}
	return a.defaults()
}

// loadSettings reads the settings rows over the defaults. A stored value no
// longer accepted, or a key no longer known, is left out.
func (a *Agent) loadSettings(ctx context.Context, q *store.Queries) (settingValues, []store.ListSettingsRow, error) {
	rows, err := q.ListSettings(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("loading settings: %w", err)
	}
	v := a.defaults()
	for _, r := range rows {
		def, ok := settingByKey(r.Key)
		if !ok {
			continue
		}
		val, err := def.parse(r.Value)
		if err != nil {
			slog.Warn("ignoring stored setting", "key", r.Key, "err", err)
			continue
		}
		v[r.Key] = val
	}
	return v, rows, nil
}

// refreshSettings picks up changes. It runs when a text comes in, before
// each LLM round and tool call, and when the scheduler asks what's due, so
// a change applies from the next one.
func (a *Agent) refreshSettings(ctx context.Context) error {
	v, _, err := a.loadSettings(ctx, a.Store)
	if err != nil {
		return err
	}
	a.current.Store(&v)
	if a.LogLevel != nil {
		var l slog.Level
		if err := l.UnmarshalText([]byte(v[settingLogLevel])); err == nil {
			a.LogLevel.Set(l)
		}
	}
	return nil
}

// settingText shows a value for a reply, like "off" for no water nudges.
func settingText(key, value string) string {
	if value == "0" && (key == settingWater || key == settingReping) {
		return "off"
	}
	return value
}

type updateSettingArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// updateSetting changes one setting, or puts it back to its default. The
// row says who changed it and when, and the action row keeps the text that
// did it and what it was before.
func (a *Agent) updateSetting(ctx context.Context, c *call, args updateSettingArgs) (string, error) {
	def, ok := settingByKey(args.Key)
	if !ok {
		return fmt.Sprintf("error: unknown setting %q", args.Key), nil
	}
	old, err := c.q.GetSetting(ctx, args.Key)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	v := strings.TrimSpace(args.Value)
	reset := strings.EqualFold(v, resetValue)
	if reset {
		if !exists {
			return fmt.Sprintf("%s is already the default, %s", def.label, settingText(def.key, a.defaults()[def.key])), nil
		}
		v = a.defaults()[def.key]
	} else {
		if v, err = def.parse(v); err != nil {
			return fmt.Sprintf("error: %s: %v", def.key, err), nil
		}
		if exists && old.Value == v {
			return fmt.Sprintf("%s is already %s", def.label, settingText(def.key, v)), nil
		}
	}
	if !reset && def.key == settingModel && a.LLM != nil && !a.LLM.ModelExists(ctx, v) {
		return fmt.Sprintf("error: the provider has no model %q", v), nil
	}
	if reset {
		err = c.q.DeleteSetting(ctx, def.key)
	} else {
		err = c.q.SetSetting(ctx, store.SetSettingParams{Key: def.key, Value: v,
			UpdatedBy: pgtype.Int8{Int64: c.userID, Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}})
	}
	if err != nil {
		return "", err
	}
	c.undo = &undoRecord{Op: undoRestoreSetting, Desc: "setting " + def.label + " to " + settingText(def.key, v), Setting: def.key}
	if exists {
		c.undo.Value = &old.Value
	}
	result := fmt.Sprintf("saved: %s is now %s", def.label, settingText(def.key, v))
	if reset {
		result += " (the default)"
	}
	if def.key == settingModel {
		result += ". input_price and output_price are unchanged; tell the user to set them for this model, or costs will be wrong"
	}
	return result, nil
}

// settingsSection shows every setting with its value and who changed it,
// for the prompt.
func (a *Agent) settingsSection(ctx context.Context) (string, error) {
	v, rows, err := a.loadSettings(ctx, a.Store)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("\n## Settings\nThese apply to everyone on this Fitbot. Each is key: value, what it takes, and who changed it:\n")
	for _, d := range settingDefs {
		changed := "default"
		if i := slices.IndexFunc(rows, func(r store.ListSettingsRow) bool { return r.Key == d.key }); i >= 0 {
			changed = "changed by " + rows[i].ChangedBy + " on " + rows[i].UpdatedAt.Time.In(time.UTC).Format("Jan 2, 2006")
		}
		fmt.Fprintf(&b, "%s: %s (%s; %s)\n", d.key, settingText(d.key, v[d.key]), d.help, changed)
	}
	b.WriteString("Show these when asked. Change one with update_setting, like \"no weekly summary\" or \"turn off water nudges\", " +
		"or use value default to go back to the default; \"undo\" reverts the change. A text turned off here doesn't go out. " +
		"API keys, allowed phone numbers, and server addresses can't be changed by text.\n")
	return b.String(), nil
}
