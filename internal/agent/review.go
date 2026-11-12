package agent

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The weekly review goes out on Monday with the summary and compares the last
// two Monday-to-Sunday weeks' change in average weight with the rate the plan
// expects. One odd week is only noted, since water and food volume move
// weight for days. Two weeks off the same way, with enough logged and calories
// near the target, move the maintenance estimate by a small step. Targets
// change only in this text, so a user always hears why, and a target_reviews
// row keeps a review to once a week however many times its text is built.

const (
	kindReview = "target_review"
	// reviewTolerance is how far from the planned rate a week can be and
	// still be on track, in tenths of a pound.
	reviewTolerance = 3
	// reviewWeighIns is how many days a week must be weighed, and
	// reviewLogged how many must have calories logged.
	reviewWeighIns = 3
	reviewLogged   = 5
	// reviewIntakeRange is how far average calories can be from the target
	// for the target to be what was tested.
	reviewIntakeRange = 200
	// reviewMinStep and reviewMaxStep bound a change, in calories a day.
	// reviewStepCalories is half what a tenth of a pound a week is worth, so
	// a change closes half the gap and the next review sees what it did.
	reviewMinStep      = 100
	reviewMaxStep      = 200
	reviewStepCalories = caloriesPerWeeklyPound / 20
)

// reviewCheck is one review's numbers. Changes are in tenths of a pound.
type reviewCheck struct {
	// planned is the plan's change a week, last the latest week's, and before
	// the week before that's.
	planned, last, before int
	// hasLast and hasBefore say each week had enough weigh-ins, and settled
	// that the plan was unchanged for both weeks being compared.
	hasLast, hasBefore, settled bool
	// logged says both weeks have calories logged, and near that both
	// averages were close to the target.
	logged, near bool
}

// reviewVerdict is what a review decided. step is the change to maintenance,
// signed.
type reviewVerdict struct {
	outcome string
	step    int
}

// judge decides a review from its numbers.
func (r reviewCheck) judge() reviewVerdict {
	if !r.hasLast {
		return reviewVerdict{outcome: "unknown"}
	}
	off := r.last - r.planned
	if abs(off) <= reviewTolerance {
		return reviewVerdict{outcome: "on_track"}
	}
	prev := r.before - r.planned
	sustained := r.hasBefore && r.settled && abs(prev) > reviewTolerance && (off > 0) == (prev > 0)
	if !sustained {
		return reviewVerdict{outcome: "watching"}
	}
	if !r.logged || !r.near {
		return reviewVerdict{outcome: "held"}
	}
	gap := float64(abs(off)+abs(prev)) / 2
	step := int(math.Round(gap*reviewStepCalories/50)) * 50
	step = min(max(step, reviewMinStep), reviewMaxStep)
	// Gaining faster than planned means maintenance is lower than thought.
	if off > 0 {
		step = -step
	}
	return reviewVerdict{outcome: "changed", step: step}
}

func abs(n int) int {
	return max(n, -n)
}

// reviewText is the week's review, run once and stored. It is empty when
// there is nothing to tell the user.
func (a *Agent) reviewText(ctx context.Context, user store.User, day pgtype.Date) (string, error) {
	var body string
	err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		q := a.Store.WithTx(tx)
		if _, err := q.LockUser(ctx, user.ID); err != nil {
			return err
		}
		week := addDays(weekStart(day), -7)
		old, err := q.TargetReview(ctx, store.TargetReviewParams{UserID: user.ID, WeekStart: week})
		if err == nil {
			body = old.Body
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		fresh, err := q.GetUser(ctx, user.ID)
		if err != nil {
			return err
		}
		body, err = a.runReview(ctx, q, fresh, week, day)
		return err
	})
	return body, err
}

// runReview reviews the week starting on week and records the outcome.
func (a *Agent) runReview(ctx context.Context, q *store.Queries, user store.User, week, today pgtype.Date) (string, error) {
	states, err := q.BodyStateHistory(ctx, user.ID)
	if err != nil {
		return "", err
	}
	row, ok := stateRowOn(states, today)
	if !ok || isManual(row) {
		return "", nil
	}
	delta, ok := dailyDelta(row)
	if !ok {
		return "", nil
	}
	last, err := q.LatestTargetChange(ctx, user.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sunday := addDays(week, 6)
	recent, err := loadWeightTrend(ctx, q, user.ID, sunday)
	if err != nil {
		return "", err
	}
	// A cut or bulk that got to the target weight is done: the review says
	// so and changes nothing, since going past it isn't what the plan is for.
	o, hasOutlook, err := loadOutlook(ctx, q, user.ID, sunday)
	if err != nil {
		return "", err
	}
	if hasOutlook && row.State != stateMaintain && recent.days >= reviewWeighIns && o.reached() {
		body := fmt.Sprintf("You've reached your %s target: your average weight last week was %s. Want to hold it there? "+
			"Reply \"maintain\" and I'll set your calories to maintenance, or tell me a new target.", lbs(recent.target), lbs(recent.avg))
		if err := q.InsertTargetReview(ctx, store.InsertTargetReviewParams{UserID: user.ID, WeekStart: week, Outcome: "reached", Body: body}); err != nil {
			return "", err
		}
		return body, nil
	}
	earlier, err := loadWeightTrend(ctx, q, user.ID, addDays(sunday, -7))
	if err != nil {
		return "", err
	}
	r := reviewCheck{
		planned:   weeklyTenths(delta),
		last:      recent.avg - recent.prev,
		before:    earlier.avg - earlier.prev,
		hasLast:   recent.days >= reviewWeighIns && recent.prevDays >= reviewWeighIns,
		hasBefore: earlier.days >= reviewWeighIns && earlier.prevDays >= reviewWeighIns,
		// Both weeks have to have run on the current targets.
		settled: !last.LocalDate.Time.After(addDays(week, -7).Time),
	}
	logged := true
	near := true
	var average int
	for _, first := range []pgtype.Date{addDays(week, -7), week} {
		rows, err := q.IntakeDaily(ctx, store.IntakeDailyParams{UserID: user.ID, Metric: "calories", First: first, Last: addDays(first, 6)})
		if err != nil {
			return "", err
		}
		total := 0
		for _, d := range rows {
			total += int(d.Total)
		}
		if len(rows) < reviewLogged {
			logged = false
			continue
		}
		if avg := total / len(rows); abs(avg-int(last.Calories)) > reviewIntakeRange {
			near, average = false, avg
		}
	}
	r.logged, r.near = logged, near

	v := r.judge()
	moved := fmt.Sprintf("Your average weight changed %s last week and %s the week before, against %s a week planned",
		signedLbs(r.last), signedLbs(r.before), signedLbs(r.planned))
	var body string
	var changeID pgtype.Int8
	switch v.outcome {
	case "watching":
		body = fmt.Sprintf("Progress check: your average weight changed %s last week, against %s a week planned. One week can just be water, "+
			"so no change yet. I'll look again next Monday.", signedLbs(r.last), signedLbs(r.planned))
	case "held":
		if !r.logged {
			body = fmt.Sprintf("%s. I need calories logged at least %d days a week to tell if your target is off, so no change yet.", moved, reviewLogged)
		} else {
			body = fmt.Sprintf("%s. Your calories averaged %s against a %s target, so the target wasn't really tested. No change yet.",
				moved, commas(average), commas(int(last.Calories)))
		}
	case "changed":
		step := v.step
		if floor := minCalories(user.Sex.String); int(last.Calories)+step < floor {
			step = floor - int(last.Calories)
		}
		if step >= 0 && v.step < 0 {
			v.outcome = "held"
			body = fmt.Sprintf("%s. Your target is already at the %s cal minimum, so I can't lower it. Consider a slower rate.",
				moved, commas(minCalories(user.Sex.String)))
			break
		}
		adjustment := int(last.Adjustment) + step
		detail := fmt.Sprintf("weekly review: weight changed %s then %s against %s a week planned", signedLbs(r.before), signedLbs(r.last), signedLbs(r.planned))
		res, _, err := a.updateTargets(ctx, q, user, today, targetOpts{reason: "review", detail: detail, adjustment: &adjustment, keepEstimate: true})
		if err != nil {
			return "", err
		}
		if res == nil {
			return "", nil
		}
		changeID = pgtype.Int8{Int64: res.change.ID, Valid: true}
		verb := "raised"
		if step < 0 {
			verb, step = "lowered", -step
		}
		body = fmt.Sprintf("%s. That's two weeks off pace, so I %s your calories by %s, from %s to %s. New targets: %s.",
			moved, verb, commas(step), commas(int(last.Calories)), commas(int(res.change.Calories)), targetLine(res.change))
	}
	if err := q.InsertTargetReview(ctx, store.InsertTargetReviewParams{UserID: user.ID, WeekStart: week, Outcome: v.outcome, Body: body, ChangeID: changeID}); err != nil {
		return "", err
	}
	return body, nil
}
