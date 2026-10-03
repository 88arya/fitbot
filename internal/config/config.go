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
