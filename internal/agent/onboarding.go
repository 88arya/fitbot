package agent

import (
	"strings"

	"github.com/88arya/fitbot/internal/db/store"
)

// Setup collects the profile, state and rate, body stats, goals, training
// schedule, and supplements one step at a time. Code asks the questions so
// setup never depends on the model remembering to; the model only reads the
// answers and saves them with set_profile, set_state, log_weight, set_goals,
// set_schedule, and set_supplement. Calorie and macro targets follow from the
// state and stats, unless the user chose to set their own.

// setupState is what setup checks to find the next step.
type setupState struct {
	user store.User
	// state is the user's latest state, or empty before they pick one.
	state string
	// manual is true for a user who sets their own goals, and hasRate once
	// the state has a rate, or needs none.
	manual, hasRate bool
	// weighed is true once a weigh-in is logged, and hasTargets once code
	// has worked out targets.
	weighed, hasTargets bool
	// goals holds each metric's latest goal, including ones the user chose
	// not to track, so a "none" answer counts as answered.
	goals map[string]store.Goal
	// hasSchedule is true once a training schedule is saved, even as
	// "none".
	hasSchedule bool
}

func (s setupState) hasGoal(metric string) bool {
	_, ok := s.goals[metric]
	return ok
}

// needsStat reports whether a body stat is still needed for targets.
func (s setupState) needsStat(missing bool) bool {
	return !s.manual && missing
}

type field struct {
	// label names what is missing, for the system prompt.
	label    string
	question func(setupState) string
	missing  func(setupState) bool
}

var fields = []field{
	{
		label:    "name",
		question: ask("What should I call you?"),
		missing:  func(s setupState) bool { return !s.user.Name.Valid },
	},
	{
		label:    "timezone",
		question: ask("What city are you in? I'll use it for your timezone."),
		missing:  func(s setupState) bool { return !s.user.Timezone.Valid },
	},
	{
		label: "wake time and bedtime",
		question: func(s setupState) string {
			switch {
			case s.user.WakeTime.Valid:
				return "And what time do you usually go to bed?"
			case s.user.Bedtime.Valid:
				return "And what time do you usually wake up?"
			}
			return "What time do you usually wake up and go to bed?"
		},
		missing: func(s setupState) bool { return !s.user.WakeTime.Valid || !s.user.Bedtime.Valid },
	},
	{
		label:    "bulk, maintain, or cut",
		question: ask("Are you looking to bulk, maintain, or cut right now?"),
		missing:  func(s setupState) bool { return s.state == "" },
	},
	{
		label: "how fast to cut or bulk",
		question: func(s setupState) string {
			if s.state == stateBulk {
				return "How fast? Lean (+100 cal a day, about 0.2 lbs a week), standard (+250, about 0.5), dirty (+500, about 1), " +
					`or your own surplus. Say "manual" to set your own goals instead.`
			}
			return "How fast? Light (-250 cal a day, about 0.5 lbs a week), moderate (-500, about 1), aggressive (-750, about 1.5), " +
				`extreme (-1,000, about 2), or your own pace. Say "manual" to set your own goals instead.`
		},
		missing: func(s setupState) bool { return s.state != "" && !s.manual && !s.hasRate },
	},
	{
		label:    "current weight",
		question: ask("What do you weigh right now?"),
		// One with targets keeps them, even if the weigh-in behind them is
		// deleted.
		missing: func(s setupState) bool { return s.needsStat(!s.weighed && !s.hasTargets) },
	},
	{
		label:    "height",
		question: ask("How tall are you?"),
		missing:  func(s setupState) bool { return s.needsStat(!s.user.HeightMm.Valid) },
	},
	{
		label:    "age",
		question: ask("How old are you?"),
		missing:  func(s setupState) bool { return s.needsStat(!s.user.Age.Valid) },
	},
	{
		label:    "sex",
		question: ask("Male or female? The calorie formula needs it."),
		missing:  func(s setupState) bool { return s.needsStat(!s.user.Sex.Valid) },
	},
	{
		label: "activity level",
		question: ask("How active are you? Sedentary (desk job, little exercise), light (1-3 days a week), moderate (3-5), " +
			"active (6-7), or very active (hard daily training or a physical job)."),
		missing: func(s setupState) bool { return s.needsStat(!s.user.Activity.Valid) },
	},
	{
		label: "calorie and water goals (protein, carbs, and fat are optional)",
		question: func(s setupState) string {
			switch {
			case !s.manual:
				return `What's your daily water goal? Say "none" if you don't want to track it.`
			case s.hasGoal("calories"):
				return `And your daily water goal? Say "none" if you don't want to track it.`
			case s.hasGoal("water"):
				return `And your daily calorie goal? Say "none" if you don't want to track it.`
			}
			return "What are your daily calorie and water goals? Add protein, carbs, or fat in grams if you track them, " +
				`or say "none" for any you don't want to track.`
		},
		missing: func(s setupState) bool {
			if !s.manual {
				return !s.hasGoal("water")
			}
			return !s.hasGoal("calories") || !s.hasGoal("water")
		},
	},
	{
		label:    "sleep goal",
		question: ask(`How many hours of sleep a night are you aiming for? Say "none" to skip.`),
		missing:  func(s setupState) bool { return !s.hasGoal("sleep") },
	},
	{
		label:    "target weight",
		question: ask(`Do you have a target weight? Say "none" if not.`),
		missing:  func(s setupState) bool { return !s.hasGoal("weight") },
	},
	{
		label: "training schedule",
		question: ask("What's your training schedule? It can be by weekday, like push Mon/Thu, pull Tue/Fri, legs Wed/Sat, " +
			`or a rotation like full body, rest, repeat. Say "none" if you don't follow one.`),
		missing: func(s setupState) bool { return !s.hasSchedule },
	},
	{
		label: "supplements",
		question: ask(`Do you take any supplements? Tell me what and when, like "creatine 5 g every night", ` +
			`or say "none".`),
		missing: func(s setupState) bool { return !s.user.SupplementsAnswered },
	},
}

func ask(q string) func(setupState) string {
	return func(setupState) string { return q }
}

const setupDone = `You're all set. You can change any of this by text, like "make my water goal 3L" or "I moved to Vancouver".`

// Intro is the first text Fitbot sends a new user.
func Intro() string {
	return "Hi, I'm Fitbot. I'll help you stay on track with your training, food, and sleep. " +
		"A few quick questions to set up first. " + fields[0].question(setupState{})
}

func missingFields(s setupState) []field {
	var out []field
	for _, f := range fields {
		if f.missing(s) {
			out = append(out, f)
		}
	}
	return out
}

// withSetup adds the next setup question to reply, or a closing line when
// this reply finished setup.
func withSetup(reply string, s setupState, wasMissing bool) string {
	next := ""
	if missing := missingFields(s); len(missing) > 0 {
		next = missing[0].question(s)
	} else if wasMissing {
		next = setupDone
	}
	reply = strings.TrimSpace(reply)
	switch {
	case next == "":
		return reply
	case reply == "":
		return next
	}
	return reply + " " + next
}
