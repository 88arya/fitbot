package config

import (
	"errors"
	"fmt"
	"log/slog"
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

	tz, err := time.LoadLocation(env("DEFAULT_TIMEZONE", "America/Toronto"))
	if err != nil {
		errs = append(errs, fmt.Errorf("DEFAULT_TIMEZONE: %w", err))
	}
	c.DefaultTimezone = tz

	floor, err := strconv.Atoi(env("CALORIE_FLOOR", "1200"))
	if err != nil || floor < 1 || floor > 5000 {
		errs = append(errs, fmt.Errorf("CALORIE_FLOOR: %q is not a calorie amount from 1 to 5000", env("CALORIE_FLOOR", "")))
	}
	c.CalorieFloor = floor

	band, err := strconv.Atoi(env("GOAL_BAND", "10"))
	if err != nil || band < 1 || band > 50 {
		errs = append(errs, fmt.Errorf("GOAL_BAND: %q is not a percent from 1 to 50", env("GOAL_BAND", "")))
	}
	c.GoalBand = band

	followup, err := time.ParseDuration(env("GYM_FOLLOWUP_DELAY", "2h"))
	if err != nil || followup < 15*time.Minute || followup > 6*time.Hour {
		errs = append(errs, fmt.Errorf("GYM_FOLLOWUP_DELAY: %q is not a duration from 15m to 6h", env("GYM_FOLLOWUP_DELAY", "")))
	}
	c.GymFollowup = followup

	reping, err := time.ParseDuration(env("SUPPLEMENT_REPING", "0"))
	if err != nil || reping != 0 && (reping < 15*time.Minute || reping > 6*time.Hour) {
		errs = append(errs, fmt.Errorf("SUPPLEMENT_REPING: %q is not 0 or a duration from 15m to 6h", env("SUPPLEMENT_REPING", "")))
	}
	c.SupplementReping = reping

	nudges, err := strconv.Atoi(env("WATER_NUDGES", "1"))
	if err != nil || nudges < 0 || nudges > 4 {
		errs = append(errs, fmt.Errorf("WATER_NUDGES: %q is not a number from 0 to 4", env("WATER_NUDGES", "")))
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
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 {
				errs = append(errs, fmt.Errorf("%s: %q is not a price in USD per 1M tokens", key, v))
			}
			*dst = f
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid config:\n%w", err)
	}
	return c, nil
}
