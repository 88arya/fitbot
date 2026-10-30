package agent_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/88arya/fitbot/internal/agent"
	"github.com/88arya/fitbot/internal/sim"
)

// statsCall builds get_stats arguments: metric, then name, period, days,
// first, and last, each JSON or null.
func statsCall(metric, name, period, days, first, last string) string {
	return fmt.Sprintf(`{"metric":"%s","name":%s,"period":"%s","days":%s,"first":%s,"last":%s}`,
		metric, name, period, days, first, last)
}

// dayCall builds get_day arguments.
func dayCall(date string) string {
	return `{"date":` + date + `}`
}

// nounPlural is n and noun, with an s unless n is 1.
func nounPlural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// short shows a day like results do, like "Oct 3".
func short(d time.Time) string {
	return d.Format("Jan 2")
}

// spanText shows a period like results do, like "this week (Oct 26 to
// Oct 29)".
func spanText(name string, first, last time.Time) string {
	dates := short(first)
	if !last.Equal(first) {
		dates += " to " + short(last)
	}
	if name == "" {
		return dates
	}
	return name + " (" + dates + ")"
}

// questions is the simulated month the questions are asked about, with the
// hand-run SQL that answers them.
type questions struct {
	t     *testing.T
	s     *sim.Sim
	today time.Time
}

func (q questions) day(n int) time.Time {
	return q.today.AddDate(0, 0, n)
}

// ask runs one question whose answer is one tool call, and returns its
// result.
func (q questions) ask(body, tool, args string) string {
	q.t.Helper()
	r := logged(q.t, q.s, body, tool, args)
	if len(r) != 1 {
		q.t.Fatalf("%s: results %q", body, r)
	}
	return r[0]
}

// dailyQ is a daily metric as hand-run SQL sees it: src selects each day's
// value as (local_date, v) for user $1, and metric names its goals.
type dailyQ struct {
	label, metric, src, noun string
	atLeast, partial, total  bool
}

// dailyNums is a daily metric over a range of days, from SQL.
type dailyNums struct {
	logged, total, avg, withGoal, onGoal, best int
	goals                                      []int32
}

// judged is the SQL judging each day from $2 to $3 against the goal in
// force then: at least the goal, or within 10% of it.
func (d dailyQ) judged() string {
	return `WITH v AS (` + d.src + `),
		k AS (
			SELECT gs::date AS day, v.v,
				(SELECT g.value FROM goals g WHERE g.user_id = $1 AND g.metric = $4 AND g.start_date <= gs::date
				 ORDER BY g.start_date DESC LIMIT 1) AS goal
			FROM generate_series($2::date, $3::date, interval '1 day') gs LEFT JOIN v ON v.local_date = gs::date),
		j AS (
			SELECT *, coalesce(v IS NOT NULL AND goal IS NOT NULL AND
				CASE WHEN $5::bool THEN v >= goal ELSE abs(v - goal) * 100 <= goal * 10 END, false) AS on_goal
			FROM k)`
}

func (q questions) daily(d dailyQ, first, last time.Time) dailyNums {
	q.t.Helper()
	var n dailyNums
	err := q.s.Pool.QueryRow(q.s.Ctx, d.judged()+`,
		runs AS (SELECT day - (row_number() OVER (ORDER BY day))::int AS grp FROM j WHERE on_goal)
		SELECT count(v), coalesce(sum(v), 0)::int, coalesce(round(avg(v)), 0)::int,
			count(*) FILTER (WHERE v IS NOT NULL AND goal IS NOT NULL), count(*) FILTER (WHERE on_goal),
			coalesce((SELECT max(n) FROM (SELECT count(*) AS n FROM runs GROUP BY grp) r), 0)::int,
			coalesce((SELECT array_agg(goal ORDER BY first) FROM (SELECT goal, min(day) AS first FROM j
				WHERE goal IS NOT NULL GROUP BY goal) x), '{}')
		FROM j`, q.s.UserID, first.Format(time.DateOnly), last.Format(time.DateOnly), d.metric, d.atLeast).
		Scan(&n.logged, &n.total, &n.avg, &n.withGoal, &n.onGoal, &n.best, &n.goals)
	if err != nil {
		q.t.Fatal(err)
	}
	return n
}

// streak counts the days on goal in a row ending on end.
func (q questions) streak(d dailyQ, end time.Time) int {
	q.t.Helper()
	var n int
	err := q.s.Pool.QueryRow(q.s.Ctx, d.judged()+`
		SELECT count(*) FROM j WHERE on_goal AND day > coalesce((SELECT max(day) FROM j WHERE NOT on_goal), '-infinity'::date)`,
		q.s.UserID, end.AddDate(0, 0, -400).Format(time.DateOnly), end.Format(time.DateOnly), d.metric, d.atLeast).Scan(&n)
	if err != nil {
		q.t.Fatal(err)
	}
	return n
}

// value is the metric's value on day, if anything is logged.
func (q questions) value(d dailyQ, day time.Time) (int, bool) {
	q.t.Helper()
	rows := q.s.Rows(`SELECT v FROM (`+d.src+`) v WHERE local_date = $2`, q.s.UserID, day.Format(time.DateOnly))
	if len(rows) == 0 {
		return 0, false
	}
	return mustInt(q.t, rows[0]), true
}

// tallyText words a range of days as get_stats does.
func (d dailyQ) tallyText(n dailyNums, days int) string {
	f := func(v int) string { return agent.FormatAmount(d.metric, v) }
	if n.logged == 0 {
		return "nothing logged on " + nounPlural(days, d.noun)
	}
	out := fmt.Sprintf("%d of %s logged", n.logged, nounPlural(days, d.noun))
	if n.logged == 1 {
		out += ", " + f(n.total)
	} else {
		out += fmt.Sprintf(", %s average a %s logged", f(n.avg), d.noun)
		if d.total {
			out += ", " + f(n.total) + " total"
		}
	}
	if len(n.goals) == 0 {
		return out
	}
	rule := "each " + d.noun + "'s goal"
	if len(n.goals) == 1 {
		rule = f(int(n.goals[0]))
	}
	if d.atLeast {
		rule = "at least " + rule
	} else {
		rule = "within 10% of " + rule
	}
	of := nounPlural(n.withGoal, d.noun) + " logged"
	if n.withGoal < n.logged {
		of += " with a goal"
	}
	out += fmt.Sprintf(", on goal (%s) on %d of the %s", rule, n.onGoal, of)
	if n.onGoal > 0 && days > 1 {
		out += ", best streak " + nounPlural(n.best, d.noun)
	}
	return out
}

// wantDaily is what get_stats should say about a daily metric from first
// to last, against prevFirst to prevLast, worked out from hand-run SQL.
func (q questions) wantDaily(d dailyQ, name string, first, last time.Time, prevName string, prevFirst, prevLast time.Time) string {
	q.t.Helper()
	f := func(v int) string { return agent.FormatAmount(d.metric, v) }
	var parts []string
	judgedLast := last
	if d.partial && last.Equal(q.today) {
		judgedLast = q.day(-1)
	}
	var n dailyNums
	if !judgedLast.Before(first) {
		n = q.daily(d, first, judgedLast)
		text := d.tallyText(n, int(judgedLast.Sub(first).Hours()/24)+1)
		if !judgedLast.Equal(last) {
			text = "complete days " + spanText("", first, judgedLast) + ": " + text
		}
		parts = append(parts, text)
	}
	todayValue, loggedToday := q.value(d, q.today)
	if d.partial && last.Equal(q.today) {
		parts = append(parts, "today so far: "+f(todayValue)+", counted once the day ends")
	}
	end, when := q.today, "last night"
	if d.partial || !loggedToday {
		end, when = q.day(-1), "the night before last"
		if d.partial {
			when = "yesterday"
		}
	}
	if s := q.streak(d, end); s > 0 {
		parts = append(parts, fmt.Sprintf("current streak: %s on goal in a row through %s", nounPlural(s, d.noun), when))
	} else {
		parts = append(parts, "current streak: none, "+when+" wasn't on goal")
	}
	prev := spanText(prevName, prevFirst, prevLast)
	p := q.daily(d, prevFirst, prevLast)
	switch p.logged {
	case 0:
		parts = append(parts, prev+": nothing logged to compare")
	case 1:
		parts = append(parts, fmt.Sprintf("%s: %s over 1 %s logged", prev, f(p.total), d.noun))
	default:
		parts = append(parts, fmt.Sprintf("%s: %s average over %d %ss logged", prev, f(p.avg), p.logged, d.noun))
	}
	if n.logged > 0 && p.logged > 0 {
		switch diff := n.avg - p.avg; {
		case diff > 0:
			parts[len(parts)-1] += ", so the average is up " + f(diff)
		case diff < 0:
			parts[len(parts)-1] += ", so the average is down " + f(-diff)
		default:
			parts[len(parts)-1] += ", so the average is the same"
		}
	}
	return d.label + ", " + spanText(name, first, last) + ": " + strings.Join(parts, "; ") + "."
}

// weightNums is the days weighed from first to last, from SQL: each day's
// first weigh-in, in tenths of a pound.
type weightNums struct {
	n, avg, lo, hi int
	loDay, hiDay   time.Time
}

func (q questions) weight(first, last time.Time) weightNums {
	q.t.Helper()
	var w weightNums
	var lo, hi *time.Time
	err := q.s.Pool.QueryRow(q.s.Ctx, `WITH f AS (
			SELECT DISTINCT ON (local_date) local_date, round(grams / 453.59237 * 10)::int AS t FROM weight_logs
			WHERE user_id = $1 AND deleted_at IS NULL AND local_date BETWEEN $2::date AND $3::date
			ORDER BY local_date, logged_at, id)
		SELECT count(*), coalesce(round(avg(t)), 0)::int, coalesce(min(t), 0), coalesce(max(t), 0),
			(SELECT local_date FROM f WHERE t = (SELECT min(t) FROM f) ORDER BY local_date LIMIT 1),
			(SELECT local_date FROM f WHERE t = (SELECT max(t) FROM f) ORDER BY local_date LIMIT 1)
		FROM f`, q.s.UserID, first.Format(time.DateOnly), last.Format(time.DateOnly)).Scan(&w.n, &w.avg, &w.lo, &w.hi, &lo, &hi)
	if err != nil {
		q.t.Fatal(err)
	}
	if lo != nil {
		w.loDay, w.hiDay = *lo, *hi
	}
	return w
}

// wantWeight is what get_stats should say about body weight from first to
// last, with a 175 lbs target, worked out from hand-run SQL.
func (q questions) wantWeight(name string, first, last time.Time, prevName string, prevFirst, prevLast time.Time) string {
	q.t.Helper()
	var parts []string
	switch w := q.weight(first, last); w.n {
	case 0:
		parts = append(parts, "no weigh-ins")
	case 1:
		parts = append(parts, "1 day weighed, "+showLbs(w.avg)+" on "+short(w.loDay))
	default:
		parts = append(parts, fmt.Sprintf("%d days weighed, %s average, lowest %s on %s, highest %s on %s", w.n, showLbs(w.avg),
			showLbs(w.lo), short(w.loDay), showLbs(w.hi), short(w.hiDay)))
	}
	end := q.weight(last.AddDate(0, 0, -6), last)
	before := first.AddDate(0, 0, -1)
	start := q.weight(before.AddDate(0, 0, -6), before)
	text := "7-day average as of " + short(last) + ": " + showLbs(end.avg)
	switch d := end.avg - start.avg; {
	case start.n == 0:
		text += ", no weigh-ins in the 7 days ending " + short(before) + " to compare"
	case d < 0:
		text += ", down " + showLbs(-d) + " from " + showLbs(start.avg) + " as of " + short(before)
	case d > 0:
		text += ", up " + showLbs(d) + " from " + showLbs(start.avg) + " as of " + short(before)
	default:
		text += ", the same as of " + short(before)
	}
	parts = append(parts, text)
	text = "target weight: 175 lbs"
	switch d := end.avg - 1750; {
	case d > 0:
		text += ", 7-day average " + showLbs(d) + " above"
	case d < 0:
		text += ", 7-day average " + showLbs(-d) + " below"
	default:
		text += ", 7-day average on target"
	}
	parts = append(parts, text)
	prev := spanText(prevName, prevFirst, prevLast)
	switch p := q.weight(prevFirst, prevLast); p.n {
	case 0:
		parts = append(parts, prev+": no weigh-ins to compare")
	case 1:
		parts = append(parts, prev+": "+showLbs(p.avg)+" over 1 day weighed")
	default:
		parts = append(parts, fmt.Sprintf("%s: %s average over %d days weighed", prev, showLbs(p.avg), p.n))
	}
	return "body weight, " + spanText(name, first, last) + ": " + strings.Join(parts, "; ") + "."
}

// sessionNums counts one session's outcomes, or every session's, from SQL.
type sessionNums struct {
	name                                         string
	planned, done, skipped, missed, open, extras int
}

func (c sessionNums) String() string {
	if c.planned == 0 {
		s := "nothing planned"
		if c.extras > 0 {
			s += ", " + nounPlural(c.extras, "extra session") + " done"
		}
		return s
	}
	s := fmt.Sprintf("%d of %s done", c.done, nounPlural(c.planned, "planned session"))
	for _, n := range []struct {
		n    int
		word string
	}{{c.skipped, "skipped"}, {c.missed, "missed"}, {c.open, "to go"}} {
		if n.n > 0 {
			s += fmt.Sprintf(", %d %s", n.n, n.word)
		}
	}
	if c.extras > 0 {
		s += ", plus " + nounPlural(c.extras, "extra session")
	}
	return s
}

// outcomes is the SQL for what became of each session from $2 to $3 as of
// today $4: every planned session from the schedule in force that day, by
// its position in the day, and each done session that wasn't planned.
const outcomes = `WITH days AS (SELECT gs::date AS day FROM generate_series($2::date, $3::date, interval '1 day') gs),
	planned AS (
		SELECT days.day, p.s, p.pos FROM days
		JOIN LATERAL (SELECT id FROM training_schedules ts WHERE ts.user_id = $1 AND ts.start_date <= days.day
			ORDER BY ts.start_date DESC LIMIT 1) ts ON true
		JOIN schedule_days sd ON sd.schedule_id = ts.id AND sd.slot = extract(dow FROM days.day)::int
		CROSS JOIN LATERAL unnest(sd.sessions) WITH ORDINALITY AS p(s, pos)),
	logged AS (
		SELECT w.local_date AS day, x.s, w.status FROM workouts w CROSS JOIN LATERAL unnest(w.sessions) AS x(s)
		WHERE w.user_id = $1 AND w.deleted_at IS NULL AND w.local_date BETWEEN $2::date AND $3::date),
	outcomes AS (
		SELECT p.day, p.s, p.pos, coalesce(l.status, CASE WHEN p.day < $4::date THEN 'missed' ELSE 'open' END) AS state, true AS planned
		FROM planned p LEFT JOIN logged l ON l.day = p.day AND l.s = p.s
		UNION ALL
		SELECT l.day, l.s, 100, 'extra', false FROM logged l
		WHERE l.status = 'done' AND NOT EXISTS (SELECT 1 FROM planned p WHERE p.day = l.day AND p.s = l.s))`

// sessions counts each session's outcomes from first to last, in the order
// first seen, and their sum. With name set, only that session counts.
func (q questions) sessions(first, last time.Time, name string) (sessionNums, []sessionNums) {
	q.t.Helper()
	rows, err := q.s.Pool.Query(q.s.Ctx, outcomes+`
		SELECT s, count(*) FILTER (WHERE planned), count(*) FILTER (WHERE planned AND state = 'done'),
			count(*) FILTER (WHERE state = 'skipped'), count(*) FILTER (WHERE state = 'missed'),
			count(*) FILTER (WHERE state = 'open'), count(*) FILTER (WHERE NOT planned)
		FROM outcomes WHERE $5 = '' OR s = $5
		GROUP BY s ORDER BY min((day - date '2000-01-01') * 1000 + pos)`,
		q.s.UserID, first.Format(time.DateOnly), last.Format(time.DateOnly), q.today.Format(time.DateOnly), name)
	if err != nil {
		q.t.Fatal(err)
	}
	defer rows.Close()
	var all sessionNums
	var each []sessionNums
	for rows.Next() {
		var c sessionNums
		if err := rows.Scan(&c.name, &c.planned, &c.done, &c.skipped, &c.missed, &c.open, &c.extras); err != nil {
			q.t.Fatal(err)
		}
		each = append(each, c)
		all.planned += c.planned
		all.done += c.done
		all.skipped += c.skipped
		all.missed += c.missed
		all.open += c.open
		all.extras += c.extras
	}
	if err := rows.Err(); err != nil {
		q.t.Fatal(err)
	}
	return all, each
}

// leading counts the rows of states, newest first, that are want before
// the first that isn't.
func (q questions) leading(want, query string, args ...any) int {
	q.t.Helper()
	n := 0
	for _, s := range q.s.Rows(query, args...) {
		if s != want {
			break
		}
		n++
	}
	return n
}

// wantWorkouts is what get_stats should say about workouts, or one
// session, worked out from hand-run SQL.
func (q questions) wantWorkouts(name, spanName string, first, last time.Time, prevName string, prevFirst, prevLast time.Time) string {
	q.t.Helper()
	all, each := q.sessions(first, last, name)
	label, what := "workouts", "planned session"
	if name != "" {
		label, what = name, "planned "+name+" session"
	}
	lines := []string{label + ", " + spanText(spanName, first, last) + ": " + all.String()}
	if name == "" && len(each) > 1 {
		var parts []string
		for _, c := range each {
			parts = append(parts, c.name+": "+c.String())
		}
		lines = append(lines, "by session: "+strings.Join(parts, "; "))
	}
	n := q.leading("done", outcomes+`
		SELECT state FROM outcomes WHERE planned AND state <> 'open' AND ($5 = '' OR s = $5) ORDER BY day DESC, pos DESC`,
		q.s.UserID, q.day(-370).Format(time.DateOnly), q.today.Format(time.DateOnly), q.today.Format(time.DateOnly), name)
	lines = append(lines, "current streak: "+nounPlural(n, what)+" done in a row")
	p, _ := q.sessions(prevFirst, prevLast, name)
	lines = append(lines, spanText(prevName, prevFirst, prevLast)+": "+p.String())
	return strings.Join(lines, "\n")
}

// doses is the SQL for each supplement's due doses from $2 to $3 as of
// today $4, by the plan in force each day.
const doses = `WITH due AS (
		SELECT gs::date AS day, s.id, s.name FROM generate_series($2::date, $3::date, interval '1 day') gs
		CROSS JOIN supplements s
		JOIN LATERAL (SELECT * FROM supplement_plans p WHERE p.supplement_id = s.id AND p.start_date <= gs::date
			ORDER BY p.start_date DESC LIMIT 1) p ON true
		WHERE s.user_id = $1 AND p.active AND (p.days IS NULL OR extract(dow FROM gs::date)::int = ANY (p.days))),
	doses AS (
		SELECT due.day, due.name, coalesce(l.status, CASE WHEN due.day < $4::date THEN 'missed' ELSE 'open' END) AS state
		FROM due LEFT JOIN supplement_logs l ON l.supplement_id = due.id AND l.local_date = due.day AND l.deleted_at IS NULL)`

// doseText counts one supplement's due doses from first to last.
func (q questions) doseText(name string, first, last time.Time) string {
	q.t.Helper()
	var due, taken, skipped, missed, open int
	err := q.s.Pool.QueryRow(q.s.Ctx, doses+`
		SELECT count(*), count(*) FILTER (WHERE state = 'taken'), count(*) FILTER (WHERE state = 'skipped'),
			count(*) FILTER (WHERE state = 'missed'), count(*) FILTER (WHERE state = 'open')
		FROM doses WHERE name = $5`, q.s.UserID, first.Format(time.DateOnly), last.Format(time.DateOnly),
		q.today.Format(time.DateOnly), name).Scan(&due, &taken, &skipped, &missed, &open)
	if err != nil {
		q.t.Fatal(err)
	}
	if due == 0 {
		return "none due"
	}
	s := fmt.Sprintf("%d of %s taken", taken, nounPlural(due, "due dose"))
	for _, n := range []struct {
		n    int
		word string
	}{{skipped, "skipped"}, {missed, "missed"}, {open, "still due today"}} {
		if n.n > 0 {
			s += fmt.Sprintf(", %d %s", n.n, n.word)
		}
	}
	return s
}

// wantSupplements is what get_stats should say about the supplements
// named, worked out from hand-run SQL.
func (q questions) wantSupplements(names []string, spanName string, first, last time.Time, prevName string, prevFirst, prevLast time.Time) string {
	q.t.Helper()
	lines := []string{"supplements, " + spanText(spanName, first, last) + ":"}
	for _, n := range names {
		line := n + ": " + q.doseText(n, first, last)
		streak := q.leading("taken", doses+`SELECT state FROM doses WHERE name = $5 AND state <> 'open' ORDER BY day DESC`,
			q.s.UserID, q.day(-370).Format(time.DateOnly), q.today.Format(time.DateOnly), q.today.Format(time.DateOnly), n)
		if streak > 0 {
			line += "; current streak: taken " + nounPlural(streak, "day") + " in a row when due"
		} else {
			line += "; current streak: none"
		}
		line += "; " + spanText(prevName, prevFirst, prevLast) + ": " + q.doseText(n, prevFirst, prevLast)
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// seedMonth seeds a month of calories, protein, fat, water, sleep,
// weigh-ins, workouts on a weekly schedule, and supplements, Sep 28 to Oct
// 29, with gaps and goals that change partway. It returns the supplement
// ids by name.
func seedMonth(t *testing.T) (*sim.Sim, map[string]int64) {
	loc, _ := time.LoadLocation("America/Toronto")
	start := time.Date(2026, 9, 28, 7, 0, 0, 0, loc)
	s := sim.New(t, start)
	logged(t, s, "goals", "set_goals", `{"goals":[{"metric":"calories","value":2000,"unit":null},`+
		`{"metric":"protein","value":150,"unit":"g"},{"metric":"fat","value":70,"unit":"g"},`+
		`{"metric":"water","value":2.5,"unit":"l"},{"metric":"sleep","value":7.5,"unit":"hours"},`+
		`{"metric":"weight","value":175,"unit":"lbs"}]}`)
	logged(t, s, "ppl", "set_schedule", weeklyPPL)
	logged(t, s, "creatine", "set_supplement", supplementCall("set", `"creatine"`, `"5 g"`, `"morning"`, "null"))
	logged(t, s, "fish oil", "set_supplement", supplementCall("set", `"fish oil"`, "null", `"21:00"`, `["mon","wed","fri"]`))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(s.Ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Calories drop to 1,800 from Oct 15, and sleep rises to 8 hours from
	// Oct 20.
	exec(`INSERT INTO goals (user_id, metric, value, start_date) VALUES ($1, 'calories', 1800, '2026-10-15'),
		($1, 'sleep', 480, '2026-10-20')`, s.UserID)

	// Everything is logged against one seed text, from Sep 28 to today,
	// Thu Oct 29.
	var msgID, actionID int64
	if err := s.Pool.QueryRow(s.Ctx, `INSERT INTO messages (user_id, direction, body, status)
		VALUES ($1, 'inbound', 'seed', 'received') RETURNING id`, s.UserID).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(s.Ctx, `INSERT INTO actions (user_id, message_id, tool, args, occurrence, result)
		VALUES ($1, $2, 'seed', '{}', 1, '') RETURNING id`, s.UserID, msgID).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, n := range []string{"creatine", "fish oil"} {
		var id int64
		if err := s.Pool.QueryRow(s.Ctx, `SELECT id FROM supplements WHERE user_id = $1 AND name = $2`, s.UserID, n).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[n] = id
	}
	plan := map[time.Weekday][]string{time.Monday: {"push"}, time.Tuesday: {"pull", "cardio"}, time.Wednesday: {"legs"},
		time.Thursday: {"push"}, time.Friday: {"pull", "cardio"}, time.Saturday: {"legs"}}
	const days = 31
	for d := 0; d <= days; d++ {
		day := time.Date(2026, 9, 28+d, 0, 0, 0, 0, time.UTC)
		date := day.Format(time.DateOnly)
		at := func(h, m int) time.Time { return time.Date(2026, 9, 28+d, h, m, 0, 0, loc) }
		intake := func(metric string, amount, minute int) {
			exec(`INSERT INTO intake_logs (user_id, action_id, message_id, local_date, metric, amount, logged_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.UserID, actionID, msgID, date, metric, amount, at(12, minute))
		}
		if d%7 != 3 {
			intake("calories", 1200+(d*173)%500, 0)
			if d < days {
				intake("calories", 400+(d*91)%600, 1)
			}
		}
		if d%5 != 1 {
			intake("protein", 100+(d*37)%90, 2)
		}
		if d%6 != 2 {
			intake("water", 1500+(d*311)%2000, 3)
		}
		intake("fat", 50+(d*13)%40, 4)
		if d%8 != 5 {
			exec(`INSERT INTO sleep_logs (user_id, action_id, message_id, local_date, minutes, logged_at)
				VALUES ($1, $2, $3, $4, $5, $6)`, s.UserID, actionID, msgID, date, 360+(d*47)%180, at(8, 0))
		}
		if d%3 != 1 {
			weigh := func(grams int, h int) {
				exec(`INSERT INTO weight_logs (user_id, action_id, message_id, local_date, grams, logged_at)
					VALUES ($1, $2, $3, $4, $5, $6)`, s.UserID, actionID, msgID, date, grams, at(h, 0))
			}
			grams := 83000 - d*60 + (d*7919)%900
			weigh(grams, 7)
			if d%10 == 0 {
				weigh(grams+500, 9)
			}
		}
		workout := func(status string, sessions []string) {
			exec(`INSERT INTO workouts (user_id, action_id, message_id, local_date, status, sessions, logged_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.UserID, actionID, msgID, date, status, sessions, at(19, 0))
		}
		switch sessions := plan[day.Weekday()]; {
		case d == days:
			// Today's push isn't logged yet.
		case len(sessions) == 0:
			if d%14 == 13 {
				workout("done", []string{"cardio"})
			}
		case d%9 == 4:
			workout("skipped", sessions)
		case d%11 == 7:
		case len(sessions) == 2 && d%4 == 0:
			workout("done", sessions[:1])
		default:
			workout("done", sessions)
		}
		take := func(name, status string) {
			exec(`INSERT INTO supplement_logs (user_id, action_id, message_id, supplement_id, local_date, status, logged_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.UserID, actionID, msgID, ids[name], date, status, at(8, 30))
		}
		switch {
		case d == days:
		case d%6 == 4:
			take("creatine", "skipped")
		case d%13 == 9:
		default:
			take("creatine", "taken")
		}
		if wd := day.Weekday(); (wd == time.Monday || wd == time.Wednesday || wd == time.Friday) && d%4 != 1 {
			take("fish oil", "taken")
		}
	}
	return s, ids
}

// TestQuestions asks 30 questions about the seeded month and checks each
// answer against hand-run SQL.
func TestQuestions(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s, ids := seedMonth(t)
	s.Set(time.Date(2026, 10, 29, 15, 0, 0, 0, loc))
	q := questions{t: t, s: s, today: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}
	monday := q.day(-3)
	lastMonday := monday.AddDate(0, 0, -7)
	oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	aug1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	date := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }
	quote := func(d time.Time) string { return `"` + d.Format(time.DateOnly) + `"` }

	src := func(metric string) string {
		return `SELECT local_date, sum(amount)::int AS v FROM intake_logs WHERE user_id = $1 AND metric = '` + metric +
			`' AND deleted_at IS NULL GROUP BY local_date`
	}
	calories := dailyQ{label: "calories", metric: "calories", src: src("calories"), noun: "day", partial: true, total: true}
	protein := dailyQ{label: "protein", metric: "protein", src: src("protein"), noun: "day", atLeast: true, partial: true, total: true}
	fat := dailyQ{label: "fat", metric: "fat", src: src("fat"), noun: "day", partial: true, total: true}
	water := dailyQ{label: "water", metric: "water", src: src("water"), noun: "day", atLeast: true, partial: true, total: true}
	sleep := dailyQ{label: "sleep", metric: "sleep", src: `SELECT local_date, minutes AS v FROM sleep_logs WHERE user_id = $1 AND deleted_at IS NULL`,
		noun: "night", atLeast: true}

	type question struct {
		body, tool, args, want string
	}
	stats := []question{
		{"calories this past week", "get_stats", statsCall("calories", "null", "last_days", "7", "null", "null"),
			q.wantDaily(calories, "last 7 days", q.day(-6), q.today, "the 7 days before", q.day(-13), q.day(-7))},
		{"how are my calories this week", "get_stats", statsCall("calories", "null", "this_week", "null", "null", "null"),
			q.wantDaily(calories, "this week", monday, q.today, "the week before", lastMonday, monday.AddDate(0, 0, -1))},
		{"calories last week", "get_stats", statsCall("calories", "null", "last_week", "null", "null", "null"),
			q.wantDaily(calories, "last week", lastMonday, monday.AddDate(0, 0, -1), "the week before", lastMonday.AddDate(0, 0, -7),
				lastMonday.AddDate(0, 0, -1))},
		{"how many days did I hit calories this month", "get_stats", statsCall("calories", "null", "this_month", "null", "null", "null"),
			q.wantDaily(calories, "this month", oct1, q.today, "the month before", sep1, date(9, 30))},
		{"calories last month", "get_stats", statsCall("calories", "null", "last_month", "null", "null", "null"),
			q.wantDaily(calories, "last month", sep1, date(9, 30), "the month before", aug1, date(8, 31))},
		{"calories oct 10 to 20", "get_stats", statsCall("calories", "null", "dates", "null", `"2026-10-10"`, `"2026-10-20"`),
			q.wantDaily(calories, "", date(10, 10), date(10, 20), "the 11 days before", date(9, 29), date(10, 9))},
		{"calories on oct 20", "get_stats", statsCall("calories", "null", "dates", "null", `"2026-10-20"`, `"2026-10-20"`),
			q.wantDaily(calories, "", date(10, 20), date(10, 20), "the day before", date(10, 19), date(10, 19))},
		{"did I hit protein every day the last 2 weeks", "get_stats", statsCall("protein", "null", "last_days", "14", "null", "null"),
			q.wantDaily(protein, "last 14 days", q.day(-13), q.today, "the 14 days before", q.day(-27), q.day(-14))},
		{"water today", "get_stats", statsCall("water", "null", "last_days", "1", "null", "null"),
			q.wantDaily(water, "today", q.today, q.today, "the day before", q.day(-1), q.day(-1))},
		{"water this month", "get_stats", statsCall("water", "null", "this_month", "null", "null", "null"),
			q.wantDaily(water, "this month", oct1, q.today, "the month before", sep1, date(9, 30))},
		{"fat last week", "get_stats", statsCall("fat", "null", "last_week", "null", "null", "null"),
			q.wantDaily(fat, "last week", lastMonday, monday.AddDate(0, 0, -1), "the week before", lastMonday.AddDate(0, 0, -7),
				lastMonday.AddDate(0, 0, -1))},
		{"carbs this month", "get_stats", statsCall("carbs", "null", "this_month", "null", "null", "null"),
			"carbs: not tracked; the user has no carbs goal and has logged none. They can start by setting a goal."},
		{"average sleep last 2 weeks", "get_stats", statsCall("sleep", "null", "last_days", "14", "null", "null"),
			q.wantDaily(sleep, "last 14 days", q.day(-13), q.today, "the 14 days before", q.day(-27), q.day(-14))},
		{"sleep last week", "get_stats", statsCall("sleep", "null", "last_week", "null", "null", "null"),
			q.wantDaily(sleep, "last week", lastMonday, monday.AddDate(0, 0, -1), "the week before", lastMonday.AddDate(0, 0, -7),
				lastMonday.AddDate(0, 0, -1))},
		{"how many nights did I hit my sleep goal this month", "get_stats", statsCall("sleep", "null", "this_month", "null", "null", "null"),
			q.wantDaily(sleep, "this month", oct1, q.today, "the month before", sep1, date(9, 30))},
		{"sleep oct 18 to 24", "get_stats", statsCall("sleep", "null", "dates", "null", `"2026-10-18"`, `"2026-10-24"`),
			q.wantDaily(sleep, "", date(10, 18), date(10, 24), "the 7 days before", date(10, 11), date(10, 17))},
		{"how much have I lost this month", "get_stats", statsCall("weight", "null", "this_month", "null", "null", "null"),
			q.wantWeight("this month", oct1, q.today, "the month before", sep1, date(9, 30))},
		{"what's my 7-day average", "get_stats", statsCall("weight", "null", "last_days", "7", "null", "null"),
			q.wantWeight("last 7 days", q.day(-6), q.today, "the 7 days before", q.day(-13), q.day(-7))},
		{"weight last month", "get_stats", statsCall("weight", "null", "last_month", "null", "null", "null"),
			q.wantWeight("last month", sep1, date(9, 30), "the month before", aug1, date(8, 31))},
		{"did I miss any workouts this month", "get_stats", statsCall("workout", "null", "this_month", "null", "null", "null"),
			q.wantWorkouts("", "this month", oct1, q.today, "the month before", sep1, date(9, 30))},
		{"how many leg days did I miss this month", "get_stats", statsCall("workout", `"Legs"`, "this_month", "null", "null", "null"),
			q.wantWorkouts("legs", "this month", oct1, q.today, "the month before", sep1, date(9, 30))},
		{"workouts last week", "get_stats", statsCall("workout", "null", "last_week", "null", "null", "null"),
			q.wantWorkouts("", "last week", lastMonday, monday.AddDate(0, 0, -1), "the week before", lastMonday.AddDate(0, 0, -7),
				lastMonday.AddDate(0, 0, -1))},
		{"push days the last 2 weeks", "get_stats", statsCall("workout", `"push"`, "last_days", "14", "null", "null"),
			q.wantWorkouts("push", "last 14 days", q.day(-13), q.today, "the 14 days before", q.day(-27), q.day(-14))},
		{"workouts this week", "get_stats", statsCall("workout", "null", "this_week", "null", "null", "null"),
			q.wantWorkouts("", "this week", monday, q.today, "the week before", lastMonday, monday.AddDate(0, 0, -1))},
		{"supplements this week", "get_stats", statsCall("supplement", "null", "last_days", "7", "null", "null"),
			q.wantSupplements([]string{"creatine", "fish oil"}, "last 7 days", q.day(-6), q.today, "the 7 days before", q.day(-13), q.day(-7))},
		{"how many days did I miss creatine this month", "get_stats", statsCall("supplement", `"Creatine"`, "this_month", "null", "null", "null"),
			q.wantSupplements([]string{"creatine"}, "this month", oct1, q.today, "the month before", sep1, date(9, 30))},
		{"fish oil last week", "get_stats", statsCall("supplement", `"fish oil"`, "last_week", "null", "null", "null"),
			q.wantSupplements([]string{"fish oil"}, "last week", lastMonday, monday.AddDate(0, 0, -1), "the week before",
				lastMonday.AddDate(0, 0, -7), lastMonday.AddDate(0, 0, -1))},
	}
	for _, c := range stats {
		if got := q.ask(c.body, c.tool, c.args); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.body, got, c.want)
		}
	}

	// get_day, against the day's rows.
	total := func(metric string, day time.Time) int {
		v, _ := q.value(dailyQ{src: src(metric)}, day)
		return v
	}
	g := func(metric string, v int) string { return agent.FormatAmount(metric, v) }
	idOf := func(query string, args ...any) string {
		t.Helper()
		rows := s.Rows(query, args...)
		if len(rows) != 1 {
			t.Fatalf("%s: rows %v", query, rows)
		}
		return rows[0]
	}

	// Today: calories, protein, fat, and water so far against today's
	// goals, with what's left.
	got := q.ask("how many calories do I have left", "get_day", dayCall("null"))
	toGo := func(metric string, v, goal int) string {
		f := func(n int) string { return g(metric, n) }
		got, of := f(v), f(goal)
		if unit := of[strings.LastIndex(of, " "):]; strings.HasSuffix(got, unit) {
			got = strings.TrimSuffix(got, unit)
		}
		s := got + " / " + of + " today"
		switch {
		case v < goal:
			return s + ", " + f(goal-v) + " to go"
		case v == goal || metric == "protein" || metric == "water":
			return s + ", goal hit"
		}
		return s + ", " + f(v-goal) + " over"
	}
	wantToday := []string{
		"Thu Oct 29 (today)",
		"totals: calories: " + toGo("calories", total("calories", q.today), 1800) + "; protein: " +
			toGo("protein", total("protein", q.today), 150) + "; fat: " + toGo("fat", total("fat", q.today), 70) + "; water: " +
			toGo("water", total("water", q.today), 2500) + ".",
		"training plan: push at 6:00 PM; workouts: push not logged yet",
		"supplements today: creatine not logged yet",
	}
	for _, w := range wantToday {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("today lacks %q:\n%s", w, got)
		}
	}
	// Every amount logged today is listed with its id, in the order logged.
	var amounts []string
	for _, r := range s.Rows(`SELECT id, metric, amount, extract(minute FROM logged_at)::int FROM intake_logs
		WHERE user_id = $1 AND local_date = '2026-10-29' ORDER BY logged_at, id`, s.UserID) {
		f := strings.Split(r, " | ")
		m, n := f[1], mustInt(t, f[2])
		a := g(m, n)
		if m != "calories" {
			a += " " + m
		}
		amounts = append(amounts, fmt.Sprintf("#%s at 12:%02d PM: %s", f[0], mustInt(t, f[3]), a))
	}
	if want := "amounts logged: " + strings.Join(amounts, ", ") + "\n"; !strings.Contains(got, want) {
		t.Errorf("today lacks %q:\n%s", want, got)
	}

	// Yesterday, Wed Oct 28 (d 30): legs done, creatine taken, fish oil
	// taken, weighed in.
	got = q.ask("did I hit my water yesterday", "get_day", dayCall(quote(q.day(-1))))
	yCal, yWater, yProtein := total("calories", q.day(-1)), total("water", q.day(-1)), total("protein", q.day(-1))
	verdict := func(metric string, v, goal int, atLeast bool) string {
		f := func(n int) string { return g(metric, n) }
		got, of := f(v), f(goal)
		if unit := of[strings.LastIndex(of, " "):]; strings.HasSuffix(got, unit) {
			got = strings.TrimSuffix(got, unit)
		}
		s := got + " / " + of + " yesterday"
		on := v >= goal
		if !atLeast {
			d := v - goal
			if d < 0 {
				d = -d
			}
			on = d*100 <= goal*10
		}
		switch {
		case atLeast && on:
			return s + ", goal hit"
		case atLeast:
			return s + ", " + f(goal-v) + " short, goal missed"
		case v < goal:
			s += ", " + f(goal-v) + " under"
		case v > goal:
			s += ", " + f(v-goal) + " over"
		}
		if on {
			return s + ", on goal"
		}
		return s + ", off goal"
	}
	proteinText := "nothing logged yesterday (goal 150 g)"
	if yProtein > 0 {
		proteinText = verdict("protein", yProtein, 150, true)
	}
	waterText := "nothing logged yesterday (goal 2.5 L)"
	if yWater > 0 {
		waterText = verdict("water", yWater, 2500, true)
	}
	// Oct 28 has two weigh-ins; the first is its weight.
	var weighIns []string
	for _, r := range s.Rows(`SELECT id, grams FROM weight_logs WHERE user_id = $1 AND local_date = '2026-10-28'
		ORDER BY logged_at, id`, s.UserID) {
		id, grams, _ := strings.Cut(r, " | ")
		weighIns = append(weighIns, "(#"+id+": "+showLbs(tenthsOf(t, grams))+")")
	}
	if len(weighIns) != 2 {
		t.Fatalf("Oct 28 weigh-ins = %v", weighIns)
	}
	yFirst := strings.TrimSuffix(weighIns[0][strings.Index(weighIns[0], ": ")+2:], ")")
	for _, w := range []string{
		"Wed Oct 28 (yesterday)\n",
		"totals: calories: " + verdict("calories", yCal, 1800, false) + "; protein: " + proteinText + "; fat: " +
			verdict("fat", total("fat", q.day(-1)), 70, false) + "; water: " + waterText + ".\n",
		"training plan: legs at 6:00 PM; workouts: legs done (#" +
			idOf(`SELECT id FROM workouts WHERE user_id = $1 AND local_date = '2026-10-28'`, s.UserID) + ": legs done)\n",
		"supplements yesterday: creatine taken, fish oil taken (#" +
			idOf(`SELECT id FROM supplement_logs WHERE supplement_id = $1 AND local_date = '2026-10-28'`, ids["creatine"]) +
			": creatine taken) (#" +
			idOf(`SELECT id FROM supplement_logs WHERE supplement_id = $1 AND local_date = '2026-10-28'`, ids["fish oil"]) +
			": fish oil taken)\n",
		"weight: " + yFirst + " yesterday (the first of 2 weigh-ins that day) " + strings.Join(weighIns, " ") + "\n",
	} {
		if !strings.Contains(got+"\n", w) {
			t.Errorf("yesterday lacks %q:\n%s", w, got)
		}
	}

	// Sun Oct 11 (d 13): a rest day with extra cardio, no night or weigh-in
	// logged, and calories against the old goal.
	got = q.ask("what did I do on oct 11", "get_day", dayCall(`"2026-10-11"`))
	latest := idOf(`SELECT grams || ' ' || local_date FROM weight_logs WHERE user_id = $1 AND local_date < '2026-10-11'
		ORDER BY local_date DESC, logged_at LIMIT 1`, s.UserID)
	latestGrams, latestDay, _ := strings.Cut(latest, " ")
	latestDate, err := time.Parse(time.DateOnly, latestDay)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"Sun Oct 11\n",
		"totals: calories: " + strings.Replace(verdict("calories", total("calories", date(10, 11)), 2000, false), "yesterday", "on Oct 11", 1),
		"sleep: nothing logged for the night before Oct 11\n",
		"training plan: rest; workouts: rest day, cardio done (extra) (#" +
			idOf(`SELECT id FROM workouts WHERE user_id = $1 AND local_date = '2026-10-11'`, s.UserID) + ": cardio done)\n",
		// A Sunday, so fish oil isn't due.
		"supplements on Oct 11: creatine taken (#" +
			idOf(`SELECT id FROM supplement_logs WHERE supplement_id = $1 AND local_date = '2026-10-11'`, ids["creatine"]) +
			": creatine taken)\n",
		"weight: nothing logged on Oct 11; latest weight before then: " + showLbs(tenthsOf(t, latestGrams)) + " on " +
			short(latestDate) + "\n",
	} {
		if !strings.Contains(got+"\n", w) {
			t.Errorf("Oct 11 lacks %q:\n%s", w, got)
		}
	}
	if len(stats)+3 != 30 {
		t.Errorf("%d questions, want 30", len(stats)+3)
	}

	// Questions change nothing, so undo reverses the last change before
	// them.
	if got := s.Text("undo"); !strings.HasPrefix(got, "Undid adding fish oil") {
		t.Errorf("undo = %q", got)
	}
}

func mustInt(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	return n
}

// tenthsOf converts grams, as text, to tenths of a pound.
func tenthsOf(t *testing.T, grams string) int {
	t.Helper()
	return int(float64(mustInt(t, grams))/453.59237*10 + 0.5)
}

// TestQuestionsRefused checks that bad get_day and get_stats arguments come
// back as errors the model can correct.
func TestQuestionsRefused(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	s := sim.New(t, time.Date(2026, 10, 29, 9, 0, 0, 0, loc))
	for _, c := range []struct{ tool, args, want string }{
		{"get_stats", statsCall("steps", "null", "this_week", "null", "null", "null"),
			`error: metric "steps" must be one of calories, protein, carbs, fat, water, sleep, weight, workout, supplement`},
		{"get_stats", statsCall("calories", `"legs"`, "this_week", "null", "null", "null"), "error: name is only for workout and supplement"},
		{"get_stats", statsCall("calories", "null", "fortnight", "null", "null", "null"),
			`error: period "fortnight" must be one of last_days, this_week, last_week, this_month, last_month, dates`},
		{"get_stats", statsCall("calories", "null", "last_days", "null", "null", "null"), "error: last_days needs days from 1 to 366"},
		{"get_stats", statsCall("calories", "null", "last_days", "0", "null", "null"), "error: last_days needs days from 1 to 366"},
		{"get_stats", statsCall("calories", "null", "last_days", "367", "null", "null"), "error: last_days needs days from 1 to 366"},
		{"get_stats", statsCall("calories", "null", "this_week", "7", "null", "null"), "error: days is only for last_days"},
		{"get_stats", statsCall("calories", "null", "this_month", "null", `"2026-10-01"`, "null"), "error: first and last are only for dates"},
		{"get_stats", statsCall("calories", "null", "dates", "null", "null", `"2026-10-01"`), "error: dates needs first"},
		{"get_stats", statsCall("calories", "null", "dates", "null", `"Oct 1"`, "null"), `error: date "Oct 1" is not YYYY-MM-DD`},
		{"get_stats", statsCall("calories", "null", "dates", "null", `"2026-10-30"`, "null"),
			"error: date 2026-10-30 is in the future; today is 2026-10-29"},
		{"get_stats", statsCall("calories", "null", "dates", "null", `"2026-10-01"`, `"2026-11-01"`),
			"error: date 2026-11-01 is in the future; today is 2026-10-29"},
		{"get_stats", statsCall("calories", "null", "dates", "null", `"2025-10-27"`, "null"), "error: date 2025-10-27 is more than 366 days ago"},
		{"get_stats", statsCall("calories", "null", "dates", "null", `"2026-10-10"`, `"2026-10-09"`), "error: last is before first"},
		{"get_stats", statsCall("supplement", `"zinc"`, "this_week", "null", "null", "null"),
			"error: zinc has never been on the supplement list; today it has nothing"},
		{"get_stats", `{"metric":"calories","name":null,"period":"last_days","days":7.5,"first":null,"last":null}`,
			"error: invalid arguments: json: cannot unmarshal number 7.5 into Go struct field statsArgs.days of type int"},
		{"get_day", dayCall(`"2026-10-30"`), "error: date 2026-10-30 is in the future; today is 2026-10-29"},
		{"get_day", dayCall(`"2025-10-27"`), "error: date 2025-10-27 is more than 366 days ago"},
		{"get_day", dayCall(`"yesterday"`), `error: date "yesterday" is not YYYY-MM-DD`},
	} {
		if got := logged(t, s, "how am I doing", c.tool, c.args); fmt.Sprint(got) != "["+c.want+"]" {
			t.Errorf("%s %s = %q\nwant %q", c.tool, c.args, got, c.want)
		}
	}
	// The prompt says how to answer questions.
	reqs := s.Requests()
	sys := reqs[len(reqs)-1][0].Content
	if !strings.Contains(sys, "\n## Questions\nAnswer questions about how the user is doing from get_day and get_stats") ||
		!strings.Contains(sys, "within 10% of it for calories, carbs, and fat") {
		t.Errorf("prompt lacks the questions section:\n%s", sys)
	}
}
