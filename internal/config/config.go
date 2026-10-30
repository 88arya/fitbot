package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/joho/godotenv"
)

type Config struct {
	HTTPAddr      string
	PublicBaseURL string
	DatabaseURL   string
	LogLevel      slog.Level

	DefaultTimezone *time.Location
	AllowedSenders  []string
	// CalorieFloor is the lowest daily calorie goal Fitbot accepts.
	CalorieFloor int
	// GoalBand is how far from the goal, in percent, a day's calories,
	// carbs, or fat can be and still count as on goal.
	GoalBand int
	// GymFollowup is how long after the gym reminder the follow-up goes.
	GymFollowup time.Duration
	// SupplementReping is how long after a supplement reminder it goes
	// again; zero means it doesn't.
	SupplementReping time.Duration
	// WaterNudges is the most water nudges a day; zero turns them off.
	WaterNudges int

	Telnyx TelnyxConfig
	OpenAI OpenAIConfig
}

type TelnyxConfig struct {
	APIKey     string
	PublicKey  string
	FromNumber string
}

type OpenAIConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	// Prices in USD per 1M tokens, used to record what each call cost.
	// Zero means unknown, and costs are left unrecorded.
	InputPrice  float64
	OutputPrice float64
}

// The Parse functions check a setting's value, from env or from a text.

// ParseTimezone reads an IANA timezone.
func ParseTimezone(v string) (*time.Location, error) {
	loc, err := time.LoadLocation(v)
	if err != nil || v == "" || v == "Local" {
		return nil, fmt.Errorf("%q is not an IANA timezone like America/Toronto", v)
	}
	return loc, nil
}

// ParseCalorieFloor reads the lowest daily calorie goal accepted.
func ParseCalorieFloor(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 5000 {
		return 0, fmt.Errorf("%q is not a calorie amount from 1 to 5000", v)
	}
	return n, nil
}

// ParseGoalBand reads the on-goal band, in percent.
func ParseGoalBand(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 50 {
		return 0, fmt.Errorf("%q is not a percent from 1 to 50", v)
	}
	return n, nil
}

// ParseGymFollowup reads how long after the gym reminder the follow-up goes.
func ParseGymFollowup(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d < 15*time.Minute || d > 6*time.Hour {
		return 0, fmt.Errorf("%q is not a duration from 15m to 6h", v)
	}
	return d, nil
}

// ParseSupplementReping reads how long after a supplement reminder it goes
// again; zero means it doesn't.
func ParseSupplementReping(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d != 0 && (d < 15*time.Minute || d > 6*time.Hour) {
		return 0, fmt.Errorf("%q is not 0 or a duration from 15m to 6h", v)
	}
	return d, nil
}

// ParseWaterNudges reads the most water nudges a day.
func ParseWaterNudges(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 4 {
		return 0, fmt.Errorf("%q is not a number from 0 to 4", v)
	}
	return n, nil
}

// ParsePrice reads a price in USD per 1M tokens.
func ParsePrice(v string) (float64, error) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%q is not a price in USD per 1M tokens", v)
	}
	return f, nil
}

var e164 = regexp.MustCompile(`^\+[1-9]\d{6,14}$`)

func Load() (*Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading .env: %w", err)
	}
	return FromEnv(os.Getenv)
}

func FromEnv(get func(string) string) (*Config, error) {
	env := func(key, def string) string {
		if v := strings.TrimSpace(get(key)); v != "" {
			return v
		}
		return def
	}

	var errs []error
	require := func(key string) string {
		v := env(key, "")
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return v
	}

	c := &Config{
		HTTPAddr:      env("HTTP_ADDR", ":8080"),
		PublicBaseURL: env("PUBLIC_BASE_URL", ""),
		DatabaseURL:   require("DATABASE_URL"),
	}

	if err := c.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	tz, err := ParseTimezone(env("DEFAULT_TIMEZONE", "America/Toronto"))
	if err != nil {
		errs = append(errs, fmt.Errorf("DEFAULT_TIMEZONE: %w", err))
	}
	c.DefaultTimezone = tz

	floor, err := ParseCalorieFloor(env("CALORIE_FLOOR", "1200"))
	if err != nil {
		errs = append(errs, fmt.Errorf("CALORIE_FLOOR: %w", err))
	}
	c.CalorieFloor = floor

	band, err := ParseGoalBand(env("GOAL_BAND", "10"))
	if err != nil {
		errs = append(errs, fmt.Errorf("GOAL_BAND: %w", err))
	}
	c.GoalBand = band

	followup, err := ParseGymFollowup(env("GYM_FOLLOWUP_DELAY", "2h"))
	if err != nil {
		errs = append(errs, fmt.Errorf("GYM_FOLLOWUP_DELAY: %w", err))
	}
	c.GymFollowup = followup

	reping, err := ParseSupplementReping(env("SUPPLEMENT_REPING", "0"))
	if err != nil {
		errs = append(errs, fmt.Errorf("SUPPLEMENT_REPING: %w", err))
	}
	c.SupplementReping = reping

	nudges, err := ParseWaterNudges(env("WATER_NUDGES", "1"))
	if err != nil {
		errs = append(errs, fmt.Errorf("WATER_NUDGES: %w", err))
	}
	c.WaterNudges = nudges

	for _, s := range strings.Split(env("ALLOWED_SENDERS", ""), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if !e164.MatchString(s) {
			errs = append(errs, fmt.Errorf("ALLOWED_SENDERS: %q is not E.164 (like +14165551234)", s))
		}
		c.AllowedSenders = append(c.AllowedSenders, s)
	}

	// Telnyx and OpenAI are enabled by setting their API keys.
	if key := env("TELNYX_API_KEY", ""); key != "" {
		c.Telnyx = TelnyxConfig{
			APIKey:     key,
			PublicKey:  require("TELNYX_PUBLIC_KEY"),
			FromNumber: require("TELNYX_FROM_NUMBER"),
		}
	}

	if key := env("OPENAI_API_KEY", ""); key != "" {
		c.OpenAI = OpenAIConfig{
			BaseURL: env("OPENAI_BASE_URL", "https://api.openai.com/v1"),
			APIKey:  key,
			Model:   require("OPENAI_MODEL"),
		}
		for key, dst := range map[string]*float64{
			"OPENAI_INPUT_PRICE":  &c.OpenAI.InputPrice,
			"OPENAI_OUTPUT_PRICE": &c.OpenAI.OutputPrice,
		} {
			v := env(key, "")
			if v == "" {
				continue
			}
			f, err := ParsePrice(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
			}
			*dst = f
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid config:\n%w", err)
	}
	return c, nil
}
