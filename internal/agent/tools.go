package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/llm"
	"github.com/jackc/pgx/v5"
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

// tool is one function the model can call. run decodes the arguments and
// applies the call through q, which is bound to the call's transaction.
type tool struct {
	spec llm.Tool
	run  func(a *Agent, ctx context.Context, q *store.Queries, userID int64, raw []byte) (string, error)
}

// handle wraps fn so its arguments are decoded into T first. Unknown fields
// and wrong types come back as errors the model can correct.
func handle[T any](fn func(*Agent, context.Context, *store.Queries, int64, T) (string, error)) func(*Agent, context.Context, *store.Queries, int64, []byte) (string, error) {
	return func(a *Agent, ctx context.Context, q *store.Queries, userID int64, raw []byte) (string, error) {
		var args T
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err), nil
		}
		return fn(a, ctx, q, userID, args)
	}
}

var tools = []tool{
	{
		spec: llm.Tool{
			Name:        "set_profile",
			Description: "Save the user's profile fields. Pass null for fields that are not changing.",
			Parameters: object(map[string]any{
				"name":      map[string]any{"type": []string{"string", "null"}, "description": "What the user wants to be called, capitalized normally."},
				"timezone":  map[string]any{"type": []string{"string", "null"}, "description": "IANA timezone, like America/Toronto."},
				"wake_time": map[string]any{"type": []string{"string", "null"}, "description": "When the user usually wakes up, 24-hour HH:MM."},
				"bedtime":   map[string]any{"type": []string{"string", "null"}, "description": "When the user usually goes to bed, 24-hour HH:MM."},
			}),
		},
		run: handle((*Agent).setProfile),
	},
	{
		spec: llm.Tool{
			Name: "set_goals",
			Description: "Set or change the user's goals. List only the metrics that are changing. A change starts today; " +
				"earlier days keep the goal they had. Only save amounts the user gave.",
			Parameters: object(map[string]any{
				"goals": map[string]any{"type": "array", "items": object(map[string]any{
					"metric": map[string]any{"type": "string", "enum": metricNames()},
					"value": map[string]any{"type": []string{"number", "null"},
						"description": "The amount in unit. Daily for calories, macros, and water; nightly hours for sleep; a single target for weight. " +
							"null stops tracking the metric, like when the user says none."},
					"unit": map[string]any{"type": []string{"string", "null"}, "enum": goalUnits(),
						"description": "cal for calories; g for protein, carbs, and fat; ml, l, or oz for water; hours for sleep; lbs or kg for weight. null uses the first of these."},
				})},
			}),
		},
		run: handle((*Agent).setGoals),
	},
	{
		spec: llm.Tool{
			Name:        "remember",
			Description: "Save a new lasting fact about the user as a short sentence, like \"Trains legs on Mondays\".",
			Parameters:  object(map[string]any{"fact": map[string]any{"type": "string"}}),
		},
		run: handle((*Agent).remember),
	},
	{
		spec: llm.Tool{
			Name:        "update_memory",
			Description: "Replace the fact with this id with a new sentence.",
			Parameters: object(map[string]any{
				"id":   map[string]any{"type": "integer"},
				"fact": map[string]any{"type": "string"},
			}),
		},
		run: handle((*Agent).updateMemory),
	},
	{
		spec: llm.Tool{
			Name:        "forget",
			Description: "Delete the fact with this id.",
			Parameters:  object(map[string]any{"id": map[string]any{"type": "integer"}}),
		},
		run: handle((*Agent).forget),
	},
}

var (
	toolSpecs   []llm.Tool
	toolsByName = map[string]tool{}
)

func init() {
	for _, t := range tools {
		toolSpecs = append(toolSpecs, t.spec)
		toolsByName[t.spec.Name] = t
	}
}

// errRejected rolls back a call whose handler refused its arguments.
var errRejected = errors.New("tool call rejected")

// runTool applies one tool call made while answering message msgID and
// returns its result for the model. Bad arguments come back as errors the
// model can correct. seen counts identical calls earlier in this run, so a
// repeated call applies again, while running the same message again returns
// the results stored the first time for calls with the same arguments
// instead of applying them twice.
func (a *Agent) runTool(ctx context.Context, userID, msgID int64, tc llm.ToolCall, seen map[string]int) string {
	name := tc.Function.Name
	result, outcome, err := a.applyTool(ctx, userID, msgID, tc, seen)
	if err != nil {
		slog.Error("tool failed", "tool", name, "user_id", userID, "message_id", msgID, "err", err)
		result, outcome = "error: could not save, try again later", "error"
	}
	toolCalls.WithLabelValues(name, outcome).Inc()
	slog.Info("tool call", "tool", name, "user_id", userID, "message_id", msgID, "outcome", outcome)
	return result
}

func (a *Agent) applyTool(ctx context.Context, userID, msgID int64, tc llm.ToolCall, seen map[string]int) (result, outcome string, err error) {
	t, ok := toolsByName[tc.Function.Name]
	if !ok {
		return "error: unknown tool", "error", nil
	}
	args, err := canonical(tc.Function.Arguments)
	if err != nil {
		return "error: arguments are not valid JSON", "error", nil
	}
	key := t.spec.Name + "\x00" + string(args)
	seen[key]++
	claim := store.ClaimActionParams{
		UserID:     userID,
		MessageID:  msgID,
		Tool:       t.spec.Name,
		Args:       string(args),
		Occurrence: int32(seen[key]),
	}

	outcome = "ok"
	err = pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		q := a.Store.WithTx(tx)
		id, err := q.ClaimAction(ctx, claim)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = "replayed"
			result, err = q.ActionResult(ctx, store.ActionResultParams{
				MessageID: claim.MessageID, Tool: claim.Tool, Args: claim.Args, Occurrence: claim.Occurrence,
			})
			return err
		}
		if err != nil {
			return err
		}
		if result, err = t.run(a, ctx, q, userID, args); err != nil {
			return err
		}
		if strings.HasPrefix(result, "error") {
			return errRejected
		}
		return q.SetActionResult(ctx, store.SetActionResultParams{ID: id, Result: result})
	})
	if errors.Is(err, errRejected) {
		return result, "error", nil
	}
	return result, outcome, err
}

// canonical re-encodes JSON with sorted keys and no spacing, so the same
// arguments always compare equal. Numbers keep their exact text, so large
// IDs are not rounded through float64.
func canonical(raw string) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	return json.Marshal(v)
}

type profileArgs struct {
	Name     *string `json:"name"`
	Timezone *string `json:"timezone"`
	WakeTime *string `json:"wake_time"`
	Bedtime  *string `json:"bedtime"`
}

func (a *Agent) setProfile(ctx context.Context, q *store.Queries, userID int64, args profileArgs) (string, error) {
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
	for _, f := range []struct {
		name string
		arg  *string
		dst  *pgtype.Time
	}{{"wake_time", args.WakeTime, &p.WakeTime}, {"bedtime", args.Bedtime, &p.Bedtime}} {
		if f.arg == nil {
			continue
		}
		t, err := time.Parse("15:04", strings.TrimSpace(*f.arg))
		if err != nil {
			return fmt.Sprintf("error: %s %q is not a 24-hour time like 07:30", f.name, *f.arg), nil
		}
		*f.dst = pgtype.Time{Microseconds: int64(t.Hour()*60+t.Minute()) * 60_000_000, Valid: true}
	}
	if !p.Name.Valid && !p.Timezone.Valid && !p.WakeTime.Valid && !p.Bedtime.Valid {
		return "error: no fields given", nil
	}
	if _, err := q.UpdateProfile(ctx, p); err != nil {
		return "", err
	}
	return "saved", nil
}

func cleanFact(s string) (string, bool) {
	s = strings.TrimSpace(s)
	return s, s != "" && len(s) <= 300
}

type rememberArgs struct {
	Fact string `json:"fact"`
}

func (a *Agent) remember(ctx context.Context, q *store.Queries, userID int64, args rememberArgs) (string, error) {
	fact, ok := cleanFact(args.Fact)
	if !ok {
		return "error: fact must be 1 to 300 characters", nil
	}
	n, err := q.CountMemories(ctx, userID)
	if err != nil {
		return "", err
	}
	if n >= maxMemories {
		return fmt.Sprintf("error: memory is full (%d facts); update or forget one first", maxMemories), nil
	}
	m, err := q.InsertMemory(ctx, store.InsertMemoryParams{UserID: userID, Fact: fact})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("saved as #%d", m.ID), nil
}

type updateMemoryArgs struct {
	ID   int64  `json:"id"`
	Fact string `json:"fact"`
}

func (a *Agent) updateMemory(ctx context.Context, q *store.Queries, userID int64, args updateMemoryArgs) (string, error) {
	fact, ok := cleanFact(args.Fact)
	if !ok {
		return "error: fact must be 1 to 300 characters", nil
	}
	n, err := q.UpdateMemory(ctx, store.UpdateMemoryParams{ID: args.ID, UserID: userID, Fact: fact})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return fmt.Sprintf("error: no fact #%d", args.ID), nil
	}
	return "updated", nil
}

type forgetArgs struct {
	ID int64 `json:"id"`
}

func (a *Agent) forget(ctx context.Context, q *store.Queries, userID int64, args forgetArgs) (string, error) {
	n, err := q.DeleteMemory(ctx, store.DeleteMemoryParams{ID: args.ID, UserID: userID})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return fmt.Sprintf("error: no fact #%d", args.ID), nil
	}
	return "forgotten", nil
}
