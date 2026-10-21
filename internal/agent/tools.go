package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/llm"
	"github.com/jackc/pgx/v5/pgtype"
)

// object builds a strict-mode JSON schema: every property is required, and
// optional ones are made nullable instead.
func object(props map[string]any) map[string]any {
	required := make([]string, 0, len(props))
	for k := range props {
		required = append(required, k)
	}
	// A stable order keeps requests identical, which prompt caching needs.
	sort.Strings(required)
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

var tools = []llm.Tool{
	{
		Name:        "set_profile",
		Description: "Save the user's profile fields. Pass null for fields that are not changing.",
		Parameters: object(map[string]any{
			"name":     map[string]any{"type": []string{"string", "null"}, "description": "What the user wants to be called, capitalized normally."},
			"timezone": map[string]any{"type": []string{"string", "null"}, "description": "IANA timezone, like America/Toronto."},
		}),
	},
	{
		Name:        "remember",
		Description: "Save a new lasting fact about the user as a short sentence, like \"Trains legs on Mondays\".",
		Parameters:  object(map[string]any{"fact": map[string]any{"type": "string"}}),
	},
	{
		Name:        "update_memory",
		Description: "Replace the fact with this id with a new sentence.",
		Parameters: object(map[string]any{
			"id":   map[string]any{"type": "integer"},
			"fact": map[string]any{"type": "string"},
		}),
	},
	{
		Name:        "forget",
		Description: "Delete the fact with this id.",
		Parameters:  object(map[string]any{"id": map[string]any{"type": "integer"}}),
	},
}

// runTool applies one tool call and returns its result for the model. Bad
// arguments come back as errors the model can correct.
func (a *Agent) runTool(ctx context.Context, userID int64, tc llm.ToolCall) string {
	var (
		result string
		err    error
	)
	args := []byte(tc.Function.Arguments)
	switch tc.Function.Name {
	case "set_profile":
		result, err = a.setProfile(ctx, userID, args)
	case "remember":
		result, err = a.remember(ctx, userID, args)
	case "update_memory":
		result, err = a.updateMemory(ctx, userID, args)
	case "forget":
		result, err = a.forget(ctx, userID, args)
	default:
		result = "error: unknown tool"
	}
	if err != nil {
		slog.Error("tool failed", "tool", tc.Function.Name, "user_id", userID, "err", err)
		result = "error: could not save, try again later"
	}

	outcome := "ok"
	if strings.HasPrefix(result, "error") {
		outcome = "error"
	}
	toolCalls.WithLabelValues(tc.Function.Name, outcome).Inc()
	slog.Info("tool call", "tool", tc.Function.Name, "user_id", userID, "outcome", outcome)
	return result
}

func (a *Agent) setProfile(ctx context.Context, userID int64, raw []byte) (string, error) {
	var args struct {
		Name     *string `json:"name"`
		Timezone *string `json:"timezone"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "error: invalid arguments", nil
	}

	p := store.UpdateProfileParams{ID: userID}
	if args.Name != nil {
		name := strings.TrimSpace(*args.Name)
		if name == "" || len(name) > 100 {
			return "error: name must be 1 to 100 characters", nil
		}
		p.Name = pgtype.Text{String: name, Valid: true}
	}
	if args.Timezone != nil {
		tz := strings.TrimSpace(*args.Timezone)
		if _, err := time.LoadLocation(tz); err != nil || tz == "" || tz == "Local" {
			return fmt.Sprintf("error: %q is not an IANA timezone like America/Toronto", tz), nil
		}
		p.Timezone = pgtype.Text{String: tz, Valid: true}
	}
	if !p.Name.Valid && !p.Timezone.Valid {
		return "error: no fields given", nil
	}
	if _, err := a.Store.UpdateProfile(ctx, p); err != nil {
		return "", err
	}
	return "saved", nil
}

func cleanFact(s string) (string, bool) {
	s = strings.TrimSpace(s)
	return s, s != "" && len(s) <= 300
}

func (a *Agent) remember(ctx context.Context, userID int64, raw []byte) (string, error) {
	var args struct{ Fact string }
	if err := json.Unmarshal(raw, &args); err != nil {
		return "error: invalid arguments", nil
	}
	fact, ok := cleanFact(args.Fact)
	if !ok {
		return "error: fact must be 1 to 300 characters", nil
	}
	n, err := a.Store.CountMemories(ctx, userID)
	if err != nil {
		return "", err
	}
	if n >= maxMemories {
		return fmt.Sprintf("error: memory is full (%d facts); update or forget one first", maxMemories), nil
	}
	m, err := a.Store.InsertMemory(ctx, store.InsertMemoryParams{UserID: userID, Fact: fact})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("saved as #%d", m.ID), nil
}

func (a *Agent) updateMemory(ctx context.Context, userID int64, raw []byte) (string, error) {
	var args struct {
		ID   int64
		Fact string
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "error: invalid arguments", nil
	}
	fact, ok := cleanFact(args.Fact)
	if !ok {
		return "error: fact must be 1 to 300 characters", nil
	}
	n, err := a.Store.UpdateMemory(ctx, store.UpdateMemoryParams{ID: args.ID, UserID: userID, Fact: fact})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return fmt.Sprintf("error: no fact #%d", args.ID), nil
	}
	return "updated", nil
}

func (a *Agent) forget(ctx context.Context, userID int64, raw []byte) (string, error) {
	var args struct{ ID int64 }
	if err := json.Unmarshal(raw, &args); err != nil {
		return "error: invalid arguments", nil
	}
	n, err := a.Store.DeleteMemory(ctx, store.DeleteMemoryParams{ID: args.ID, UserID: userID})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return fmt.Sprintf("error: no fact #%d", args.ID), nil
	}
	return "forgotten", nil
}
