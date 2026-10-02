package config

import (
	"strings"
	"testing"
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
	if c.SMSProvider != "" || c.LLMProvider != "" {
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
		{"bad sender", map[string]string{"DATABASE_URL": "x", "ALLOWED_SENDERS": "4165551234"}, "not E.164"},
		{"unknown sms", map[string]string{"DATABASE_URL": "x", "SMS_PROVIDER": "pigeon"}, "unknown provider"},
		{"twilio missing creds", map[string]string{"DATABASE_URL": "x", "SMS_PROVIDER": "twilio"}, "TWILIO_AUTH_TOKEN is required"},
		{"openai missing model", map[string]string{"DATABASE_URL": "x", "LLM_PROVIDER": "openai", "OPENAI_BASE_URL": "http://localhost:11434/v1"}, "OPENAI_MODEL is required"},
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
