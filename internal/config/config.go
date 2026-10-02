package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
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

	SMSProvider string
	Twilio      TwilioConfig
	Telnyx      TelnyxConfig

	LLMProvider string
	Anthropic   AnthropicConfig
	OpenAI      OpenAIConfig
}

type TwilioConfig struct {
	AccountSID string
	AuthToken  string
	FromNumber string
}

type TelnyxConfig struct {
	APIKey     string
	PublicKey  string
	FromNumber string
}

type AnthropicConfig struct {
	APIKey string
	Model  string
}

type OpenAIConfig struct {
	BaseURL string
	APIKey  string
	Model   string
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
		SMSProvider:   strings.ToLower(env("SMS_PROVIDER", "")),
		LLMProvider:   strings.ToLower(env("LLM_PROVIDER", "")),
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

	switch c.SMSProvider {
	case "":
	case "twilio":
		c.Twilio = TwilioConfig{
			AccountSID: require("TWILIO_ACCOUNT_SID"),
			AuthToken:  require("TWILIO_AUTH_TOKEN"),
			FromNumber: require("TWILIO_FROM_NUMBER"),
		}
		errs = append(errs, requirePublicURL(c)...)
	case "telnyx":
		c.Telnyx = TelnyxConfig{
			APIKey:     require("TELNYX_API_KEY"),
			PublicKey:  require("TELNYX_PUBLIC_KEY"),
			FromNumber: require("TELNYX_FROM_NUMBER"),
		}
	default:
		errs = append(errs, fmt.Errorf("SMS_PROVIDER: unknown provider %q (want twilio or telnyx)", c.SMSProvider))
	}

	switch c.LLMProvider {
	case "":
	case "anthropic":
		c.Anthropic = AnthropicConfig{
			APIKey: require("ANTHROPIC_API_KEY"),
			Model:  env("ANTHROPIC_MODEL", "claude-haiku-4-5"),
		}
	case "openai":
		c.OpenAI = OpenAIConfig{
			BaseURL: require("OPENAI_BASE_URL"),
			APIKey:  env("OPENAI_API_KEY", ""),
			Model:   require("OPENAI_MODEL"),
		}
	default:
		errs = append(errs, fmt.Errorf("LLM_PROVIDER: unknown provider %q (want anthropic or openai)", c.LLMProvider))
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid config:\n%w", err)
	}
	return c, nil
}

func requirePublicURL(c *Config) []error {
	if c.PublicBaseURL == "" {
		return []error{errors.New("PUBLIC_BASE_URL is required for twilio webhook signature checks")}
	}
	return nil
}
