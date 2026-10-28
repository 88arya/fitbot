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

// call is one tool call being applied. q is bound to the call's
// transaction.
type call struct {
	q      *store.Queries
	userID int64
	// msgID is the inbound text being answered, and actionID the action row
	// recording this call.
	msgID, actionID int64
	// undo is set by a handler whose change can be reversed, to what
	// reverses it.
	undo *undoRecord
}

// tool is one function the model can call. run decodes the arguments and
// applies the call.
type tool struct {
	spec llm.Tool
	run  func(a *Agent, ctx context.Context, c *call, raw []byte) (string, error)
}

// handle wraps fn so its arguments are decoded into T first. Unknown fields
// and wrong types come back as errors the model can correct.
func handle[T any](fn func(*Agent, context.Context, *call, T) (string, error)) func(*Agent, context.Context, *call, []byte) (string, error) {
	return func(a *Agent, ctx context.Context, c *call, raw []byte) (string, error) {
		var args T
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err), nil
		}
		return fn(a, ctx, c, args)
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
			Name: "log_intake",
			Description: "Add amounts the user ate or drank to their daily totals, using only the amounts they gave. Never estimate: " +
				"if they gave no amount, don't call this; ask for one. Protein, carbs, and fat are logged only if the user tracks them.",
			Parameters: object(map[string]any{
				"calories": map[string]any{"type": []string{"number", "null"}, "description": "Calories (kcal), or null if not given."},
				"protein":  map[string]any{"type": []string{"number", "null"}, "description": "Grams of protein, or null if not given."},
				"carbs":    map[string]any{"type": []string{"number", "null"}, "description": "Grams of carbs, or null if not given."},
				"fat":      map[string]any{"type": []string{"number", "null"}, "description": "Grams of fat, or null if not given."},
				"water":    map[string]any{"type": []string{"number", "null"}, "description": "Water in water_unit, or null if not given."},
				"water_unit": map[string]any{"type": []string{"string", "null"}, "enum": []any{"ml", "l", "oz", nil},
					"description": "The unit the user gave water in; null for ml."},
				"date": map[string]any{"type": []string{"string", "null"},
					"description": "The user's local day it counts toward, YYYY-MM-DD, when they name one like \"yesterday\". null for today."},
			}),
		},
		run: handle((*Agent).logIntake),
	},
	{
		spec: llm.Tool{
			Name: "log_sleep",
			Description: "Log a night of sleep, using only what the user said. Give hours, or bedtime and wake_time, or all three; " +
				"with only the two times, the hours are worked out from them. Never estimate.",
			Parameters: object(map[string]any{
				"hours":     map[string]any{"type": []string{"number", "null"}, "description": "Hours slept, or null if not given."},
				"bedtime":   map[string]any{"type": []string{"string", "null"}, "description": "When they went to bed, 24-hour HH:MM, or null."},
				"wake_time": map[string]any{"type": []string{"string", "null"}, "description": "When they woke up, 24-hour HH:MM, or null."},
				"date": map[string]any{"type": []string{"string", "null"},
					"description": "The user's local day they woke up, YYYY-MM-DD, for a night before last night. null for last night."},
			}),
		},
		run: handle((*Agent).logSleep),
	},
	{
		spec: llm.Tool{
			Name: "log_workout",
			Description: "Log whether the user worked out on a day: done, or skipped. Leave sessions null to cover the day's " +
				"planned sessions; a workout on a day with nothing planned counts as extra.",
			Parameters: object(map[string]any{
				"status": map[string]any{"type": "string", "enum": workoutStatuses},
				"sessions": map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string"},
					"description": "The sessions done or skipped, with the schedule's names, like [\"legs\"], only if the user said " +
						"which. null for the day's planned sessions not logged yet."},
				"date": map[string]any{"type": []string{"string", "null"},
					"description": "The user's local day it happened, YYYY-MM-DD, when they name one like \"yesterday\". null for today."},
			}),
		},
		run: handle((*Agent).logWorkout),
	},
	{
		spec: llm.Tool{
			Name: "set_supplement",
			Description: "Add a supplement the user takes, or change it, by name, with its whole plan; or remove one; or save " +
				"that the user takes none. A change starts today.",
			Parameters: object(map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"set", "remove", "none"},
					"description": "set adds or changes one; remove stops it; none means the user takes no supplements."},
				"name": map[string]any{"type": []string{"string", "null"},
					"description": "Short lowercase name, like creatine or fish oil. null only for none."},
				"dose": map[string]any{"type": []string{"string", "null"},
					"description": "set only: the dose as the user said it, like \"5 g\", or null if they gave none."},
				"time": map[string]any{"type": []string{"string", "null"},
					"description": "set only: 24-hour HH:MM, morning (at wake time), night (at bedtime), or null for any time."},
				"days": map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string", "enum": weekdays},
					"description": "set only: the weekdays it's taken on, or null for every day."},
			}),
		},
		run: handle((*Agent).setSupplement),
	},
	{
		spec: llm.Tool{
			Name:        "log_supplement",
			Description: "Log supplements the user took or skipped on a day.",
			Parameters: object(map[string]any{
				"names": map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string"},
					"description": "Supplements by their names on the list, or null for everything due that day and not logged yet."},
				"status": map[string]any{"type": "string", "enum": supplementStatuses},
				"date": map[string]any{"type": []string{"string", "null"},
					"description": "The user's local day it was for, YYYY-MM-DD, when they name one like \"yesterday\". null for today."},
			}),
		},
		run: handle((*Agent).logSupplement),
	},
	{
		spec: llm.Tool{
			Name:        "edit_log",
			Description: "Change a logged amount, night of sleep, workout, or supplement log. Pass null for the fields that are not changing.",
			Parameters: object(map[string]any{
				"type": map[string]any{"type": "string", "enum": logTypes},
				"id": map[string]any{"type": []string{"integer", "null"},
					"description": "The entry's id, or null for the most recent of this type (for sleep, the latest night; for a workout or supplement, the latest day's)."},
				"amount": map[string]any{"type": []string{"number", "null"}, "description": "The corrected amount, in unit."},
				"unit": map[string]any{"type": []string{"string", "null"}, "enum": intakeUnits,
					"description": "cal for calories; g for protein, carbs, and fat; ml, l, or oz for water; hours for sleep. null means cal, g, ml, or hours."},
				"bedtime": map[string]any{"type": []string{"string", "null"},
					"description": "Sleep only: the corrected bedtime, 24-hour HH:MM."},
				"wake_time": map[string]any{"type": []string{"string", "null"},
					"description": "Sleep only: the corrected wake time, 24-hour HH:MM."},
				"status": map[string]any{"type": []string{"string", "null"}, "enum": []any{"done", "taken", "skipped", nil},
					"description": "Workout (done or skipped) or supplement (taken or skipped) only: the corrected status."},
				"sessions": map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string"},
					"description": "Workout only: the corrected sessions."},
				"date": map[string]any{"type": []string{"string", "null"},
					"description": "Move the entry to this local day, YYYY-MM-DD, like when the user says it was yesterday. For sleep, the day they woke up."},
			}),
		},
		run: handle((*Agent).editLog),
	},
	{
		spec: llm.Tool{
			Name:        "delete_log",
			Description: "Delete a logged amount, night of sleep, workout, or supplement log.",
			Parameters: object(map[string]any{
				"type": map[string]any{"type": "string", "enum": logTypes},
				"id": map[string]any{"type": []string{"integer", "null"},
					"description": "The entry's id, or null for the most recent of this type (for sleep, the latest night; for a workout or supplement, the latest day's)."},
			}),
		},
		run: handle((*Agent).deleteLog),
	},
	{
		spec: llm.Tool{
			Name: "set_schedule",
			Description: "Save the user's whole training schedule, starting today. It replaces the old one; skips and moves from " +
				"today on are cleared.",
			Parameters: object(map[string]any{
				"kind": map[string]any{"type": []string{"string", "null"}, "enum": []any{"weekly", "rotation", nil},
					"description": "weekly pins sessions to weekdays; rotation is a cycle of days that repeats regardless of weekday. " +
						"null if the user doesn't follow a schedule."},
				"days": map[string]any{"type": "array", "description": "Weekly: each training day, once. Rotation: every day of the cycle in order.",
					"items": object(map[string]any{
						"weekday": map[string]any{"type": []string{"string", "null"}, "enum": append(anySlice(weekdays), nil),
							"description": "Weekly only; null for a rotation."},
						"sessions": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
							"description": "Session names, like [\"legs\", \"cardio\"], or [\"rest\"] for a rest day."},
						"time": map[string]any{"type": []string{"string", "null"},
							"description": "This day's training time, 24-hour HH:MM, if it differs from training_time."},
					})},
				"cycle_day_today": map[string]any{"type": []string{"integer", "null"},
					"description": "Rotation only: which day of the cycle today is, from 1, if the user said. null for day 1."},
				"training_time": map[string]any{"type": []string{"string", "null"},
					"description": "When the user usually trains, 24-hour HH:MM, or null."},
			}),
		},
		run: handle((*Agent).setSchedule),
	},
	{
		spec: llm.Tool{
			Name:        "plan_session",
			Description: "Change one day of the training plan, today or later, without changing the schedule.",
			Parameters: object(map[string]any{
				"date": map[string]any{"type": []string{"string", "null"},
					"description": "The user's local day to change, YYYY-MM-DD; null for today."},
				"change": map[string]any{"type": "string", "enum": []string{"skip", "move", "time", "clear"},
					"description": "skip the day's sessions; move them to to_date; time sets when they're going; clear undoes a skip or move."},
				"to_date": map[string]any{"type": []string{"string", "null"},
					"description": "move only: the later day it moves to, YYYY-MM-DD."},
				"time": map[string]any{"type": []string{"string", "null"},
					"description": "time, or optionally move: when they're going, 24-hour HH:MM. null with time clears the day's own time."},
			}),
		},
		run: handle((*Agent).planSession),
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

// anySlice converts a list of strings for a schema enum.
func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
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
		// Takes turns with "undo", so an undo never picks its action while a
		// newer one is half written.
		if _, err := q.LockUser(ctx, userID); err != nil {
			return err
		}
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
		c := &call{q: q, userID: userID, msgID: msgID, actionID: id}
		if result, err = t.run(a, ctx, c, args); err != nil {
			return err
		}
		if strings.HasPrefix(result, "error") {
			return errRejected
		}
		var undo []byte
		if c.undo != nil {
			if undo, err = json.Marshal(c.undo); err != nil {
				return err
			}
		}
		return q.SetActionResult(ctx, store.SetActionResultParams{ID: id, Result: result, Undo: undo})
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

func (a *Agent) setProfile(ctx context.Context, c *call, args profileArgs) (string, error) {
	p := store.UpdateProfileParams{ID: c.userID}
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
		t, ok := timeOfDay(*f.arg)
		if !ok {
			return fmt.Sprintf("error: %s %q is not a 24-hour time like 07:30", f.name, *f.arg), nil
		}
		*f.dst = t
	}
	if !p.Name.Valid && !p.Timezone.Valid && !p.WakeTime.Valid && !p.Bedtime.Valid {
		return "error: no fields given", nil
	}
	old, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	if _, err := c.q.UpdateProfile(ctx, p); err != nil {
		return "", err
	}
	var changed []string
	if p.Name.Valid {
		changed = append(changed, "name to "+p.Name.String)
	}
	if p.Timezone.Valid {
		changed = append(changed, "timezone to "+p.Timezone.String)
	}
	if p.WakeTime.Valid {
		changed = append(changed, "wake time to "+clock(micros(p.WakeTime), "3:04 PM"))
	}
	if p.Bedtime.Valid {
		changed = append(changed, "bedtime to "+clock(micros(p.Bedtime), "3:04 PM"))
	}
	c.undo = &undoRecord{
		Op:      undoRestoreProfile,
		Desc:    "setting your " + strings.Join(changed, ", "),
		Profile: profileOf(old),
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

func (a *Agent) remember(ctx context.Context, c *call, args rememberArgs) (string, error) {
	fact, ok := cleanFact(args.Fact)
	if !ok {
		return "error: fact must be 1 to 300 characters", nil
	}
	n, err := c.q.CountMemories(ctx, c.userID)
	if err != nil {
		return "", err
	}
	if n >= maxMemories {
		return fmt.Sprintf("error: memory is full (%d facts); update or forget one first", maxMemories), nil
	}
	m, err := c.q.InsertMemory(ctx, store.InsertMemoryParams{UserID: c.userID, Fact: fact})
	if err != nil {
		return "", err
	}
	c.undo = &undoRecord{Op: undoDeleteMemory, Desc: fmt.Sprintf("saving %q", fact), ID: m.ID}
	return fmt.Sprintf("saved as #%d", m.ID), nil
}

type updateMemoryArgs struct {
	ID   int64  `json:"id"`
	Fact string `json:"fact"`
}

func (a *Agent) updateMemory(ctx context.Context, c *call, args updateMemoryArgs) (string, error) {
	fact, ok := cleanFact(args.Fact)
	if !ok {
		return "error: fact must be 1 to 300 characters", nil
	}
	old, err := c.q.GetMemory(ctx, store.GetMemoryParams{ID: args.ID, UserID: c.userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Sprintf("error: no fact #%d", args.ID), nil
	}
	if err != nil {
		return "", err
	}
	if _, err := c.q.UpdateMemory(ctx, store.UpdateMemoryParams{ID: args.ID, UserID: c.userID, Fact: fact}); err != nil {
		return "", err
	}
	c.undo = &undoRecord{Op: undoSetMemory, Desc: fmt.Sprintf("changing %q to %q", old.Fact, fact), ID: old.ID, Fact: old.Fact}
	return "updated", nil
}

type forgetArgs struct {
	ID int64 `json:"id"`
}

func (a *Agent) forget(ctx context.Context, c *call, args forgetArgs) (string, error) {
	old, err := c.q.GetMemory(ctx, store.GetMemoryParams{ID: args.ID, UserID: c.userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Sprintf("error: no fact #%d", args.ID), nil
	}
	if err != nil {
		return "", err
	}
	if _, err := c.q.DeleteMemory(ctx, store.DeleteMemoryParams{ID: args.ID, UserID: c.userID}); err != nil {
		return "", err
	}
	c.undo = &undoRecord{Op: undoRestoreMemory, Desc: fmt.Sprintf("forgetting %q", old.Fact), ID: old.ID, Fact: old.Fact}
	return "forgotten", nil
}
