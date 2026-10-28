// Package agent turns a user's text into a reply, using the LLM and tools
// that update the user's profile, goals, and memory.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db"
	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/llm"
	"github.com/88arya/fitbot/internal/scheduler"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// historyLen is how many recent messages are sent with each request.
	historyLen = 20
	// maxRounds caps LLM calls per text, so a model stuck calling tools stops.
	maxRounds = 5
	// maxMemories caps stored facts so the list stays short enough to read every time.
	maxMemories = 30
)

// ErrNoLLM is returned for a text only the LLM can answer when no LLM is
// configured.
var ErrNoLLM = errors.New("no LLM configured")

type Agent struct {
	// Pool runs each tool call in a transaction; Store uses the same database.
	Pool  *pgxpool.Pool
	Store *store.Queries
	// LLM may be nil, in which case only fast-path texts like "undo" get an
	// answer.
	LLM             *llm.Client
	DefaultTimezone string
	// CalorieFloor is the lowest daily calorie goal accepted; zero means 1,200.
	CalorieFloor int
	// Now returns the current time; nil means time.Now. Tests set it.
	Now func() time.Time
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Reply returns the text to send back for in, the user's latest message,
// which must already be stored. Tool calls along the way are applied to the
// database, each at most once per message, so running the same message
// again does not repeat a change. While setup is unfinished, the reply ends
// with the next setup question. Fast-path texts like "undo" are answered in
// code, without the LLM.
func (a *Agent) Reply(ctx context.Context, userID int64, in store.Message) (string, error) {
	history, err := a.Store.RecentMessages(ctx, store.RecentMessagesParams{UserID: userID, Limit: historyLen})
	if err != nil {
		return "", fmt.Errorf("loading history: %w", err)
	}
	state, err := a.setupState(ctx, userID)
	if err != nil {
		return "", err
	}
	wasMissing := len(missingFields(state)) > 0
	// A user who texts before Fitbot has greeted them gets the intro, so
	// setup starts the same way either way.
	if len(history) == 1 && wasMissing {
		return Intro(), nil
	}
	if reply, ok, err := a.fastPath(ctx, userID, in, wasMissing); err != nil || ok {
		if err != nil {
			return "", err
		}
		state, err := a.setupState(ctx, userID)
		if err != nil {
			return "", err
		}
		return withSetup(reply, state, wasMissing), nil
	}
	if a.LLM == nil {
		return "", ErrNoLLM
	}

	var convo []llm.Message
	for _, m := range history {
		switch {
		case m.Direction == "inbound":
			convo = append(convo, llm.Message{Role: "user", Content: m.Body})
		case m.Status != "failed" && m.Status != "sending":
			convo = append(convo, llm.Message{Role: "assistant", Content: m.Body})
		}
	}
	seen := map[string]int{}
	for range maxRounds {
		// The prompt is rebuilt each round so it shows changes made by tools.
		state, err := a.setupState(ctx, userID)
		if err != nil {
			return "", err
		}
		memories, err := a.Store.ListMemories(ctx, userID)
		if err != nil {
			return "", fmt.Errorf("loading memories: %w", err)
		}
		intake, err := a.intakeSection(ctx, state.user)
		if err != nil {
			return "", err
		}
		sleep, err := a.sleepSection(ctx, state.user)
		if err != nil {
			return "", err
		}
		training, err := a.trainingSection(ctx, state.user)
		if err != nil {
			return "", err
		}
		workouts, err := a.workoutSection(ctx, state.user)
		if err != nil {
			return "", err
		}
		sys := llm.Message{Role: "system", Content: a.systemPrompt(state, memories, intake+sleep+training+workouts)}

		out, err := a.complete(ctx, in.ID, append([]llm.Message{sys}, convo...))
		if err != nil {
			return "", err
		}
		if len(out.ToolCalls) == 0 {
			return withSetup(out.Content, state, wasMissing), nil
		}
		convo = append(convo, out)
		for _, tc := range out.ToolCalls {
			convo = append(convo, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: a.runTool(ctx, userID, in.ID, tc, seen)})
		}
	}
	return "", errors.New("too many tool rounds without a reply")
}

func (a *Agent) setupState(ctx context.Context, userID int64) (setupState, error) {
	user, err := a.Store.GetUser(ctx, userID)
	if err != nil {
		return setupState{}, fmt.Errorf("loading user: %w", err)
	}
	goals, err := a.Store.LatestGoals(ctx, userID)
	if err != nil {
		return setupState{}, fmt.Errorf("loading goals: %w", err)
	}
	_, err = a.Store.LatestSchedule(ctx, userID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return setupState{}, fmt.Errorf("loading schedule: %w", err)
	}
	return setupState{user: user, goals: goalsByMetric(goals), hasSchedule: err == nil}, nil
}

// location is the user's timezone, or the default until they give one.
func (a *Agent) location(user store.User) *time.Location {
	tz := a.DefaultTimezone
	if user.Timezone.Valid {
		tz = user.Timezone.String
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// localDate is the user's current local day.
func (a *Agent) localDate(user store.User) pgtype.Date {
	y, m, d := a.now().In(a.location(user)).Date()
	return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}
}

// fastPath answers texts that need no LLM, like "undo" and a bare water
// amount, and reports whether it did. During setup a bare amount is more
// likely an answer to a goal question, like "2L" for the water goal, so it
// goes to the LLM instead.
func (a *Agent) fastPath(ctx context.Context, userID int64, in store.Message, inSetup bool) (string, bool, error) {
	if isUndo(in.Body) {
		reply, err := a.undo(ctx, userID, in.ID)
		if err != nil {
			return "", true, fmt.Errorf("undoing: %w", err)
		}
		return reply, true, nil
	}
	if v, unit, ok := waterAmount(in.Body); ok && !inSetup {
		reply, err := a.logWater(ctx, userID, in.ID, v, unit)
		if err != nil {
			return "", true, fmt.Errorf("logging water: %w", err)
		}
		return reply, true, nil
	}
	return "", false, nil
}

// micros converts a time of day to time since midnight.
func micros(t pgtype.Time) time.Duration {
	return time.Duration(t.Microseconds) * time.Microsecond
}

// clock formats a time of day, given as time since midnight, in layout.
func clock(tod time.Duration, layout string) string {
	return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(tod).Format(layout)
}

// complete makes one LLM call and records it.
func (a *Agent) complete(ctx context.Context, inID int64, msgs []llm.Message) (llm.Message, error) {
	model := a.LLM.Model()
	start := time.Now()
	out, usage, err := a.LLM.Complete(ctx, msgs, toolSpecs)
	latency := time.Since(start)

	call := store.InsertLLMCallParams{
		MessageID:    inID,
		Model:        model,
		InputTokens:  int32(usage.InputTokens),
		OutputTokens: int32(usage.OutputTokens),
		LatencyMs:    int32(latency.Milliseconds()),
	}
	cost, priced := a.LLM.Cost(usage)
	if priced {
		call.CostUsd = db.Numeric(strconv.FormatFloat(cost, 'f', 8, 64))
		llmCost.WithLabelValues(model).Add(cost)
	}
	result := "ok"
	if err != nil {
		result = "error"
		call.Error = pgtype.Text{String: err.Error(), Valid: true}
	}
	if dbErr := a.Store.InsertLLMCall(ctx, call); dbErr != nil {
		slog.Error("storing llm call", "message_id", inID, "err", dbErr)
	}

	llmCalls.WithLabelValues(model, result).Inc()
	llmLatency.WithLabelValues(model).Observe(latency.Seconds())
	llmTokens.WithLabelValues(model, "input").Add(float64(usage.InputTokens))
	llmTokens.WithLabelValues(model, "output").Add(float64(usage.OutputTokens))

	if err != nil {
		slog.Error("llm call failed", "message_id", inID, "model", model, "err", err)
		return llm.Message{}, err
	}
	slog.Info("llm call", "message_id", inID, "model", model, "latency_ms", latency.Milliseconds(),
		"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens,
		"tool_calls", len(out.ToolCalls), "cost_usd", cost)
	return out, nil
}

const basePrompt = `You are Fitbot, a health and fitness assistant that texts with people over SMS.
Keep replies short and plain: one or two sentences, no markdown, no emoji.`

// systemPrompt builds the prompt from the user's setup state, memories, and
// sections of what they logged.
func (a *Agent) systemPrompt(s setupState, memories []store.Memory, logs string) string {
	var b strings.Builder
	b.WriteString(basePrompt)
	user := s.user

	b.WriteString("\n\n## Profile\n")
	if user.Name.Valid {
		fmt.Fprintf(&b, "name: %s\n", user.Name.String)
	} else {
		b.WriteString("name: missing\n")
	}
	if user.Timezone.Valid {
		fmt.Fprintf(&b, "timezone: %s\n", user.Timezone.String)
	} else {
		fmt.Fprintf(&b, "timezone: missing (times below assume %s)\n", a.DefaultTimezone)
	}
	for _, t := range []struct {
		name string
		set  bool
		at   time.Duration
	}{
		{"wake time", user.WakeTime.Valid, scheduler.WakeTime(user)},
		{"bedtime", user.Bedtime.Valid, scheduler.Bedtime(user)},
	} {
		if t.set {
			fmt.Fprintf(&b, "%s: %s\n", t.name, clock(t.at, "3:04 PM"))
		} else {
			fmt.Fprintf(&b, "%s: missing (check-ins assume %s)\n", t.name, clock(t.at, "3:04 PM"))
		}
	}
	if missing := missingFields(s); len(missing) > 0 {
		var labels []string
		for _, f := range missing {
			labels = append(labels, f.label)
		}
		fmt.Fprintf(&b, "\nSetup is in progress. Still missing: %s. If the conversation gives any of these, save them "+
			"with set_profile, set_goals, or set_schedule. Convert a city or region to its IANA timezone. If the user says \"none\" "+
			"or doesn't want to track a goal, save it with value null, or a schedule with kind null. If they don't know their wake time or bedtime, "+
			"save the default: wake time %s, bedtime %s. Do not ask for missing fields yourself: the next setup question is added after your reply. "+
			"If the user only answered a setup question, reply with a brief acknowledgement like \"Got it.\"\n",
			strings.Join(labels, "; "), clock(scheduler.DefaultWake, "15:04"), clock(scheduler.DefaultBedtime, "15:04"))
	}
	b.WriteString("The user can change any field later, like \"I moved to Vancouver\"; save changes with set_profile. " +
		"The morning check-in is sent at the wake time.\n")

	b.WriteString("\n## Goals\n")
	for _, m := range metrics {
		if g, ok := s.goals[m.name]; ok {
			b.WriteString(m.describe(g) + "\n")
		} else {
			b.WriteString(m.label + ": not set\n")
		}
	}
	fmt.Fprintf(&b, "Daily goals count against the user's local day. Save new or changed goals with set_goals; a change "+
		"starts today, and earlier days keep the goal they had. Never make up a goal the user didn't give. Fitbot does "+
		"not set a calorie goal below %s a day. Show amounts in cal, ml or L, g, hours, and lbs.\n",
		metrics[0].format(a.calorieFloor()))

	b.WriteString(logs)

	b.WriteString("\n## Memory\nFacts the user has told you, by id:\n")
	if len(memories) == 0 {
		b.WriteString("(none yet)\n")
	}
	for _, m := range memories {
		fmt.Fprintf(&b, "#%d %s\n", m.ID, m.Fact)
	}
	fmt.Fprintf(&b, "Save lasting facts worth knowing later (training, diet, sleep, injuries, goals, routines, preferences) with remember. "+
		"Skip small talk and anything already in the profile or goals. If a fact changes or replaces one above, call update_memory "+
		"with its id instead of remember. Call forget when a fact stops being true or the user asks. At most %d facts.\n", maxMemories)

	loc := a.location(user)
	fmt.Fprintf(&b, "\n## Time\nThe user's local time is %s (%s).",
		a.now().In(loc).Format("Monday, January 2, 2006, 3:04 PM"), loc)
	return b.String()
}
