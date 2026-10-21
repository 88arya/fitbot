package agent

import (
	"strings"

	"github.com/88arya/fitbot/internal/db/store"
)

// Setup collects the profile one field at a time. Code asks the questions so
// setup never depends on the model remembering to; the model only reads the
// answers and saves them with set_profile.

type field struct {
	name     string
	question string
	missing  func(store.User) bool
}

var fields = []field{
	{"name", "What should I call you?", func(u store.User) bool { return !u.Name.Valid }},
	{"timezone", "What city are you in? I'll use it for your timezone.", func(u store.User) bool { return !u.Timezone.Valid }},
}

const setupDone = `You're all set. You can change these any time, like "I moved to Vancouver".`

// Intro is the first text Fitbot sends a new user.
func Intro() string {
	return "Hi, I'm Fitbot. I'll help you stay on track with your training, food, and sleep. " +
		"A few quick questions to set up first. " + fields[0].question
}

func missingFields(u store.User) []field {
	var out []field
	for _, f := range fields {
		if f.missing(u) {
			out = append(out, f)
		}
	}
	return out
}

// withSetup adds the next setup question to reply, or a closing line when
// this reply finished setup.
func withSetup(reply string, user store.User, wasMissing bool) string {
	next := ""
	if missing := missingFields(user); len(missing) > 0 {
		next = missing[0].question
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
