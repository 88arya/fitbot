package config

import (
	"strings"
	"testing"
	"time"
)

func lookup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnvDefaults(t *testing.T) {
	c, err := FromEnv(lookup(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q", c.HTTPAddr)
	}
	if c.DefaultTimezone.String() != "America/Toronto" {
		t.Errorf("DefaultTimezone = %q", c.DefaultTimezone)
	}
	if c.CalorieFloor != 1200 {
		t.Errorf("CalorieFloor = %d", c.CalorieFloor)
	}
	if c.GoalBand != 10 {
		t.Errorf("GoalBand = %d", c.GoalBand)
	}
	if c.GymFollowup != 2*time.Hour || c.SupplementReping != 0 || c.WaterNudges != 1 {
		t.Errorf("GymFollowup = %s, SupplementReping = %s, WaterNudges = %d", c.GymFollowup, c.SupplementReping, c.WaterNudges)
	}
	if c.Telnyx.APIKey != "" || c.OpenAI.APIKey != "" {
		t.Errorf("providers should default to disabled")
	}
}

func TestFromEnvErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing database", map[string]string{}, "DATABASE_URL is required"},
		{"bad timezone", map[string]string{"DATABASE_URL": "x", "DEFAULT_TIMEZONE": "Mars/Base"}, "DEFAULT_TIMEZONE"},
		{"bad calorie floor", map[string]string{"DATABASE_URL": "x", "CALORIE_FLOOR": "lots"}, "CALORIE_FLOOR"},
		{"zero calorie floor", map[string]string{"DATABASE_URL": "x", "CALORIE_FLOOR": "0"}, "CALORIE_FLOOR"},
		{"bad goal band", map[string]string{"DATABASE_URL": "x", "GOAL_BAND": "10%"}, "GOAL_BAND"},
		{"wide goal band", map[string]string{"DATABASE_URL": "x", "GOAL_BAND": "51"}, "GOAL_BAND"},
		{"bad follow-up delay", map[string]string{"DATABASE_URL": "x", "GYM_FOLLOWUP_DELAY": "2"}, "GYM_FOLLOWUP_DELAY"},
		{"short follow-up delay", map[string]string{"DATABASE_URL": "x", "GYM_FOLLOWUP_DELAY": "10m"}, "GYM_FOLLOWUP_DELAY"},
		{"long re-ping", map[string]string{"DATABASE_URL": "x", "SUPPLEMENT_REPING": "7h"}, "SUPPLEMENT_REPING"},
		{"too many nudges", map[string]string{"DATABASE_URL": "x", "WATER_NUDGES": "5"}, "WATER_NUDGES"},
		{"bad sender", map[string]string{"DATABASE_URL": "x", "ALLOWED_SENDERS": "4165551234"}, "not E.164"},
		{"telnyx missing public key", map[string]string{"DATABASE_URL": "x", "TELNYX_API_KEY": "k"}, "TELNYX_PUBLIC_KEY is required"},
		{"openai missing model", map[string]string{"DATABASE_URL": "x", "OPENAI_API_KEY": "k"}, "OPENAI_MODEL is required"},
		{"bad price", map[string]string{"DATABASE_URL": "x", "OPENAI_API_KEY": "k", "OPENAI_MODEL": "m", "OPENAI_INPUT_PRICE": "$1"}, "OPENAI_INPUT_PRICE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromEnv(lookup(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestFromEnvSenders(t *testing.T) {
	c, err := FromEnv(lookup(map[string]string{
		"DATABASE_URL":    "x",
		"ALLOWED_SENDERS": " +14165551234, +16475550000 ,",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedSenders) != 2 || c.AllowedSenders[1] != "+16475550000" {
		t.Errorf("AllowedSenders = %v", c.AllowedSenders)
	}
}

func TestFromEnvCheckins(t *testing.T) {
	c, err := FromEnv(lookup(map[string]string{
		"DATABASE_URL":       "x",
		"GYM_FOLLOWUP_DELAY": "90m",
		"SUPPLEMENT_REPING":  "1h",
		"WATER_NUDGES":       "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.GymFollowup != 90*time.Minute || c.SupplementReping != time.Hour || c.WaterNudges != 0 {
		t.Errorf("GymFollowup = %s, SupplementReping = %s, WaterNudges = %d", c.GymFollowup, c.SupplementReping, c.WaterNudges)
	}
}
