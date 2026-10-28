package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/88arya/fitbot/internal/db/store"
	"github.com/88arya/fitbot/internal/scheduler"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// maxSupplements caps the supplements on the list at once.
	maxSupplements = 20
	// maxSupplementName and maxDose cap a name and a dose, in characters.
	maxSupplementName = 40
	maxDose           = 30
	// supplementWeek is how many days, ending today, the adherence count
	// covers.
	supplementWeek = 7
)

// supplementStatuses are what a supplement log records.
var supplementStatuses = []string{"taken", "skipped"}

// supplementList is every plan of the user's supplements, for working out
// what is on the list and due on any day.
type supplementList struct {
	user store.User
	// names are sorted, and plans each name's plans, oldest first.
	names []string
	plans map[string][]store.SupplementHistoryRow
	// ids names each supplement by id.
	ids map[int64]string
}

func loadSupplements(ctx context.Context, q *store.Queries, user store.User) (*supplementList, error) {
	rows, err := q.SupplementHistory(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	l := &supplementList{user: user, plans: map[string][]store.SupplementHistoryRow{}, ids: map[int64]string{}}
	for _, r := range rows {
		if _, ok := l.plans[r.Name]; !ok {
			l.names = append(l.names, r.Name)
		}
		l.plans[r.Name] = append(l.plans[r.Name], r)
		l.ids[r.SupplementID] = r.Name
	}
	return l, nil
}

// on is name's plan in force on d, and whether name is on the list then.
func (l *supplementList) on(name string, d pgtype.Date) (store.SupplementHistoryRow, bool) {
	var out store.SupplementHistoryRow
	for _, p := range l.plans[name] {
		if p.StartDate.Time.After(d.Time) {
			break
		}
		out = p
	}
	return out, out.Active
}

// listOn is the plans of every supplement on the list on d, by name.
func (l *supplementList) listOn(d pgtype.Date) []store.SupplementHistoryRow {
	var out []store.SupplementHistoryRow
	for _, n := range l.names {
		if p, ok := l.on(n, d); ok {
			out = append(out, p)
		}
	}
	return out
}

// listNames is the names on the list on d, like "creatine and fish oil",
// or "nothing" for an empty list.
func (l *supplementList) listNames(d pgtype.Date) string {
	var names []string
	for _, p := range l.listOn(d) {
		names = append(names, p.Name)
	}
	if len(names) == 0 {
		return "nothing"
	}
	return joinAnd(names)
}

// due reports whether a supplement on the list is taken on d.
func due(p store.SupplementHistoryRow, d pgtype.Date) bool {
	return p.Days == nil || slices.Contains(p.Days, int32(d.Time.Weekday()))
}

// describe shows a plan, like "creatine 5 g, every day, at night (10:00
// PM)" or "fish oil, Mon/Wed/Fri, any time".
func (l *supplementList) describe(p store.SupplementHistoryRow) string {
	s := p.Name
	if p.Dose.Valid {
		s += " " + p.Dose.String
	}
	return s + ", " + daysText(p.Days) + ", " + whenText(p.TakeAt, p.Anchor, l.user)
}

// daysText shows weekdays from Monday, like "Mon/Wed/Fri", or "every day"
// for none.
func daysText(days []int32) string {
	if days == nil {
		return "every day"
	}
	var names []string
	for _, slot := range weekOrder {
		if slices.Contains(days, slot) {
			names = append(names, strings.ToUpper(weekdays[slot][:1])+weekdays[slot][1:])
		}
	}
	return strings.Join(names, "/")
}

// whenText shows when a supplement is taken, like "at 9:00 PM", "in the
// morning (7:00 AM)", or "any time". A wake time or bedtime anchor follows
// the user's profile.
func whenText(at pgtype.Time, anchor pgtype.Text, user store.User) string {
	switch {
	case at.Valid:
		return "at " + clock(micros(at), "3:04 PM")
	case anchor.String == "wake":
		return "in the morning (" + clock(scheduler.WakeTime(user), "3:04 PM") + ")"
	case anchor.String == "bed":
		return "at night (" + clock(scheduler.Bedtime(user), "3:04 PM") + ")"
	}
	return "any time"
}

// joinAnd joins names like "a, b and c".
func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// cleanSupplementName normalizes a supplement's name to lowercase single
// spaces, or returns why it isn't one.
func cleanSupplementName(s string) (string, string) {
	n := strings.Join(strings.Fields(strings.ToLower(s)), " ")
	switch {
	case n == "":
		return "", "a supplement name is empty"
	case len(n) > maxSupplementName:
		return "", fmt.Sprintf("supplement name %q is longer than %d characters", n, maxSupplementName)
	}
	return n, ""
}

type supplementArgs struct {
	Action string   `json:"action"`
	Name   *string  `json:"name"`
	Dose   *string  `json:"dose"`
	Time   *string  `json:"time"`
	Days   []string `json:"days"`
}

// supplementPlanState is a supplement's plan row on one day as it was, for
// undo. Existed is false when the day had no row. Times are microseconds
// since midnight.
type supplementPlanState struct {
	Existed bool    `json:"existed"`
	Active  bool    `json:"active"`
	Dose    *string `json:"dose,omitempty"`
	TakeAt  *int64  `json:"take_at,omitempty"`
	Anchor  *string `json:"anchor,omitempty"`
	Days    []int32 `json:"days,omitempty"`
}

// newPlan is what set_supplement saves for a supplement.
type newPlan struct {
	dose, anchor pgtype.Text
	at           pgtype.Time
	days         []int32
}

// parsePlan checks set's dose, time, and days, or returns why they aren't
// valid.
func parsePlan(args supplementArgs) (newPlan, string) {
	var p newPlan
	if args.Dose != nil {
		d := strings.Join(strings.Fields(*args.Dose), " ")
		switch {
		case d == "":
			return p, "dose is empty; pass null if the user gave none"
		case len(d) > maxDose:
			return p, fmt.Sprintf("dose %q is longer than %d characters", d, maxDose)
		}
		p.dose = pgtype.Text{String: d, Valid: true}
	}
	if args.Time != nil {
		switch t := strings.ToLower(strings.TrimSpace(*args.Time)); t {
		case "morning":
			p.anchor = pgtype.Text{String: "wake", Valid: true}
		case "night":
			p.anchor = pgtype.Text{String: "bed", Valid: true}
		default:
			at, ok := timeOfDay(t)
			if !ok {
				return p, fmt.Sprintf("time %q must be a 24-hour time like 21:00, morning, or night, or null for any time", *args.Time)
			}
			p.at = at
		}
	}
	for _, d := range args.Days {
		slot := slices.Index(weekdays, strings.ToLower(strings.TrimSpace(d)))
		switch {
		case slot < 0:
			return p, fmt.Sprintf("day %q must be one of %s", d, strings.Join(weekdays, ", "))
		case slices.Contains(p.days, int32(slot)):
			return p, fmt.Sprintf("%s is listed twice", weekdays[slot])
		}
		p.days = append(p.days, int32(slot))
	}
	// Every weekday is every day.
	if len(p.days) == len(weekdays) {
		p.days = nil
	}
	slices.Sort(p.days)
	return p, ""
}

func (a *Agent) setSupplement(ctx context.Context, c *call, args supplementArgs) (string, error) {
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	l, err := loadSupplements(ctx, c.q, user)
	if err != nil {
		return "", err
	}
	answered := user.SupplementsAnswered
	switch args.Action {
	case "none":
		if args.Name != nil || args.Dose != nil || args.Time != nil || len(args.Days) > 0 {
			return "error: none takes no name, dose, time, or days", nil
		}
		if on := l.listOn(today); len(on) > 0 {
			return fmt.Sprintf("error: the list has %s; remove each with action remove instead", l.listNames(today)), nil
		}
		if err := c.q.SetSupplementsAnswered(ctx, store.SetSupplementsAnsweredParams{ID: c.userID, Answered: true}); err != nil {
			return "", err
		}
		c.undo = &undoRecord{Op: undoRestoreSupplement, Desc: "saving that you take no supplements", Answered: &answered}
		return "saved: the user takes no supplements.", nil
	case "set", "remove":
	default:
		return fmt.Sprintf("error: action %q must be set, remove, or none", args.Action), nil
	}
	if args.Name == nil {
		return "error: " + args.Action + " needs the supplement's name", nil
	}
	name, errText := cleanSupplementName(*args.Name)
	if errText != "" {
		return "error: " + errText, nil
	}
	old, onList := l.on(name, today)

	var p newPlan
	if args.Action == "remove" {
		if args.Dose != nil || args.Time != nil || len(args.Days) > 0 {
			return "error: remove takes only the name", nil
		}
		if !onList {
			return fmt.Sprintf("error: %s isn't on the list; the list has %s", name, l.listNames(today)), nil
		}
	} else {
		if p, errText = parsePlan(args); errText != "" {
			return "error: " + errText, nil
		}
		if !onList && len(l.listOn(today)) >= maxSupplements {
			return fmt.Sprintf("error: the list is full (%d supplements); remove one first", maxSupplements), nil
		}
	}
	updated := store.SupplementHistoryRow{Name: name, StartDate: today, Active: args.Action == "set", Dose: p.dose, TakeAt: p.at,
		Anchor: p.anchor, Days: p.days}
	if onList && updated.Active && l.describe(updated) == l.describe(old) {
		return fmt.Sprintf("error: %s is already saved as %s; nothing changed", name, l.describe(old)), nil
	}

	s, err := c.q.SupplementByName(ctx, store.SupplementByNameParams{UserID: c.userID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		s, err = c.q.InsertSupplement(ctx, store.InsertSupplementParams{UserID: c.userID, Name: name})
	}
	if err != nil {
		return "", err
	}
	undo := &undoRecord{Op: undoRestoreSupplement, ID: s.ID, Day: today.Time.Format(time.DateOnly), Answered: &answered,
		Plan: &supplementPlanState{}}
	// A change made earlier today is replaced, so undo puts it back.
	prev, err := c.q.SupplementPlanOnDay(ctx, store.SupplementPlanOnDayParams{SupplementID: s.ID, StartDate: today})
	switch {
	case err == nil:
		undo.Plan = &supplementPlanState{Existed: true, Active: prev.Active, Dose: textPtr(prev.Dose), TakeAt: microsOf(prev.TakeAt),
			Anchor: textPtr(prev.Anchor), Days: prev.Days}
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	if err := c.q.SetSupplementPlan(ctx, store.SetSupplementPlanParams{SupplementID: s.ID, StartDate: today, Active: updated.Active,
		Dose: p.dose, TakeAt: p.at, Anchor: p.anchor, Days: p.days}); err != nil {
		return "", err
	}
	if err := c.q.SetSupplementsAnswered(ctx, store.SetSupplementsAnsweredParams{ID: c.userID, Answered: true}); err != nil {
		return "", err
	}

	var result string
	switch {
	case !updated.Active:
		undo.Desc = "removing " + l.describe(old)
		result = "removed " + name + " from today."
	case onList:
		undo.Desc = fmt.Sprintf("changing %s to %s", l.describe(old), l.describe(updated))
		result = "saved from today: " + l.describe(updated) + " (was " + l.describe(old) + ")."
	default:
		undo.Desc = "adding " + l.describe(updated)
		result = "saved from today: " + l.describe(updated) + "."
	}
	c.undo = undo
	st, err := a.supplementStatus(ctx, c.q, user, today)
	if err != nil {
		return "", err
	}
	return result + " " + st.forModel(), nil
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

type supplementLogArgs struct {
	Names  []string `json:"names"`
	Status string   `json:"status"`
	Date   *string  `json:"date"`
}

func (a *Agent) logSupplement(ctx context.Context, c *call, args supplementLogArgs) (string, error) {
	if !slices.Contains(supplementStatuses, args.Status) {
		return fmt.Sprintf("error: status %q must be taken or skipped", args.Status), nil
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	day := today
	var errText string
	if args.Date != nil {
		if day, errText = entryDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	l, err := loadSupplements(ctx, c.q, user)
	if err != nil {
		return "", err
	}
	rows, err := c.q.SupplementLogsBetween(ctx, store.SupplementLogsBetweenParams{UserID: c.userID, First: day, Last: day})
	if err != nil {
		return "", err
	}
	logged := map[int64]store.SupplementLog{}
	for _, r := range rows {
		logged[r.SupplementID] = r
	}
	when := dayName(day)

	var targets []store.SupplementHistoryRow
	if len(args.Names) == 0 {
		on := l.listOn(day)
		dueCount := 0
		for _, p := range on {
			if !due(p, day) {
				continue
			}
			dueCount++
			if _, ok := logged[p.SupplementID]; !ok {
				targets = append(targets, p)
			}
		}
		switch {
		case len(targets) > 0:
		case len(on) == 0:
			return "error: no supplements are on the list for " + when, nil
		case dueCount == 0:
			return fmt.Sprintf("error: nothing on the list is due on %s; name a supplement to log it anyway", when), nil
		default:
			return fmt.Sprintf("error: everything due on %s is already logged; change a log with edit_log", when), nil
		}
	}
	for _, raw := range args.Names {
		name, errText := cleanSupplementName(raw)
		if errText != "" {
			return "error: " + errText, nil
		}
		p, ok := l.on(name, day)
		switch {
		case !ok:
			return fmt.Sprintf("error: %s isn't on the list for %s; it has %s", name, when, l.listNames(day)), nil
		case slices.ContainsFunc(targets, func(t store.SupplementHistoryRow) bool { return t.Name == name }):
			return fmt.Sprintf("error: %s is listed twice", name), nil
		case args.Status == "skipped" && !due(p, day):
			return fmt.Sprintf("error: %s isn't due on %s (%s), so it can't be skipped", name, when, daysText(p.Days)), nil
		}
		if r, ok := logged[p.SupplementID]; ok {
			return fmt.Sprintf("error: %s on %s is already logged as #%d (%s %s); change it with edit_log instead", name, when, r.ID,
				name, r.Status), nil
		}
		targets = append(targets, p)
	}

	undo := &undoRecord{Op: undoDeleteSupplementLogs, Day: day.Time.Format(time.DateOnly), Metrics: []string{"supplement"}}
	var summaries, results []string
	for _, p := range targets {
		row, err := c.q.InsertSupplementLog(ctx, store.InsertSupplementLogParams{
			UserID:       c.userID,
			ActionID:     c.actionID,
			MessageID:    c.msgID,
			SupplementID: p.SupplementID,
			LocalDate:    day,
			Status:       args.Status,
			LoggedAt:     pgtype.Timestamptz{Time: a.now(), Valid: true},
		})
		if err != nil {
			return "", err
		}
		undo.IDs = append(undo.IDs, row.ID)
		summaries = append(summaries, p.Name+" "+args.Status)
		results = append(results, fmt.Sprintf("%s %s (#%d)", p.Name, args.Status, row.ID))
	}
	undo.Desc = "logging " + joinAnd(summaries) + " on " + when
	c.undo = undo
	st, err := a.supplementStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("logged %s %s. %s", strings.Join(results, ", "), dayWord(day, today), st.forModel()), nil
}

func (a *Agent) editSupplementLog(ctx context.Context, c *call, args editLogArgs) (string, error) {
	if args.Amount != nil || args.Unit != nil || args.Bedtime != nil || args.WakeTime != nil || args.Sessions != nil {
		return "error: a supplement log has only a status and a date", nil
	}
	if args.Status == nil && args.Date == nil {
		return "error: no changes given", nil
	}
	old, errText, err := targetSupplementLog(ctx, c, args.ID)
	if errText != "" || err != nil {
		return errText, err
	}
	status := old.Status
	if args.Status != nil {
		if status = *args.Status; !slices.Contains(supplementStatuses, status) {
			return fmt.Sprintf("error: status %q must be taken or skipped", status), nil
		}
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	today := a.localDate(user)
	day := old.LocalDate
	if args.Date != nil {
		if day, errText = entryDate(*args.Date, today); errText != "" {
			return "error: " + errText, nil
		}
	}
	l, err := loadSupplements(ctx, c.q, user)
	if err != nil {
		return "", err
	}
	name := l.ids[old.SupplementID]
	p, ok := l.on(name, day)
	when := dayName(day)
	switch {
	case !ok:
		return fmt.Sprintf("error: %s isn't on the list for %s", name, when), nil
	case status == "skipped" && !due(p, day):
		return fmt.Sprintf("error: %s isn't due on %s (%s), so it can't be skipped", name, when, daysText(p.Days)), nil
	}
	moved := !day.Time.Equal(old.LocalDate.Time)
	if moved {
		rows, err := c.q.SupplementLogsBetween(ctx, store.SupplementLogsBetweenParams{UserID: c.userID, First: day, Last: day})
		if err != nil {
			return "", err
		}
		for _, r := range rows {
			if r.SupplementID == old.SupplementID {
				return fmt.Sprintf("error: %s on %s is already logged as #%d (%s %s)", name, when, r.ID, name, r.Status), nil
			}
		}
	}
	if _, err := c.q.UpdateSupplementLog(ctx, store.UpdateSupplementLogParams{ID: old.ID, UserID: c.userID, LocalDate: day,
		Status: status}); err != nil {
		return "", err
	}

	was, now := name+" "+old.Status, name+" "+status
	var desc string
	switch {
	case moved && was == now:
		desc = fmt.Sprintf("moving %s from %s to %s", was, dayName(old.LocalDate), when)
	case moved:
		desc = fmt.Sprintf("changing %s to %s and moving it from %s to %s", was, now, dayName(old.LocalDate), when)
	default:
		desc = fmt.Sprintf("changing %s to %s on %s", was, now, when)
	}
	oldStatus := old.Status
	c.undo = &undoRecord{Op: undoRestoreSupplementLog, Desc: desc, ID: old.ID, Status: &oldStatus,
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{"supplement"}}

	st, err := a.supplementStatus(ctx, c.q, user, day)
	if err != nil {
		return "", err
	}
	result := fmt.Sprintf("updated #%d: now %s %s. %s", old.ID, now, dayWord(day, today), st.day)
	if moved {
		prev, err := a.supplementStatus(ctx, c.q, user, old.LocalDate)
		if err != nil {
			return "", err
		}
		result += "; " + prev.day
	}
	return result + "; " + st.week + ".", nil
}

func (a *Agent) deleteSupplementLog(ctx context.Context, c *call, id *int64) (string, error) {
	old, errText, err := targetSupplementLog(ctx, c, id)
	if errText != "" || err != nil {
		return errText, err
	}
	if _, err := c.q.SetSupplementLogDeleted(ctx, store.SetSupplementLogDeletedParams{ID: old.ID, UserID: c.userID,
		DeletedAt: pgtype.Timestamptz{Time: a.now(), Valid: true}}); err != nil {
		return "", err
	}
	user, err := c.q.GetUser(ctx, c.userID)
	if err != nil {
		return "", err
	}
	l, err := loadSupplements(ctx, c.q, user)
	if err != nil {
		return "", err
	}
	summary := l.ids[old.SupplementID] + " " + old.Status
	c.undo = &undoRecord{Op: undoUndeleteSupplementLog, Desc: "deleting " + summary + " on " + dayName(old.LocalDate), ID: old.ID,
		Day: old.LocalDate.Time.Format(time.DateOnly), Metrics: []string{"supplement"}}
	st, err := a.supplementStatus(ctx, c.q, user, old.LocalDate)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted #%d: %s %s. %s", old.ID, summary, dayWord(old.LocalDate, a.localDate(user)), st.forModel()), nil
}

// targetSupplementLog finds the supplement log with id, or the latest one
// when id is nil, or returns an error for the model.
func targetSupplementLog(ctx context.Context, c *call, id *int64) (store.SupplementLog, string, error) {
	var row store.SupplementLog
	var err error
	if id == nil {
		row, err = c.q.LastSupplementLog(ctx, c.userID)
	} else {
		row, err = c.q.GetSupplementLog(ctx, store.GetSupplementLogParams{ID: *id, UserID: c.userID})
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows) && id == nil:
		return row, "error: no supplement logged", nil
	case errors.Is(err, pgx.ErrNoRows):
		return row, fmt.Sprintf("error: no supplement log #%d", *id), nil
	}
	return row, "", err
}

// supplementStatus is a day's supplements and the last week's doses, like
// "supplements today: creatine taken, fish oil not logged yet" and "last 7
// days: 12 of 14 due doses taken, 1 missed, 1 still due today".
type supplementStatus struct {
	day, week string
}

func (a *Agent) supplementStatus(ctx context.Context, q *store.Queries, user store.User, day pgtype.Date) (supplementStatus, error) {
	today := a.localDate(user)
	first := addDays(today, 1-supplementWeek)
	if day.Time.Before(first.Time) {
		first = day
	}
	l, err := loadSupplements(ctx, q, user)
	if err != nil {
		return supplementStatus{}, err
	}
	rows, err := q.SupplementLogsBetween(ctx, store.SupplementLogsBetweenParams{UserID: user.ID, First: first, Last: today})
	if err != nil {
		return supplementStatus{}, err
	}
	logs := map[string][]store.SupplementLog{}
	for _, r := range rows {
		logs[dateKey(r.LocalDate)] = append(logs[dateKey(r.LocalDate)], r)
	}
	return supplementStatus{
		day:  "supplements " + dayWord(day, today) + ": " + l.dayText(day, today, logs[dateKey(day)]),
		week: l.week(today, logs),
	}, nil
}

func (s supplementStatus) forModel() string {
	return s.day + "; " + s.week + "."
}

// forUser is the status for a text, like "Supplements today: creatine
// taken. Last 7 days: 6 of 7 due doses taken, 1 missed."
func (s supplementStatus) forUser() string {
	return upperFirst(s.day) + ". " + upperFirst(s.week) + "."
}

// doses is what became of each supplement due on d, by name: taken,
// skipped, missed once the day is over, or open until then.
func (l *supplementList) doses(d, today pgtype.Date, logs []store.SupplementLog) []sessionOutcome {
	byID := map[int64]string{}
	for _, r := range logs {
		byID[r.SupplementID] = r.Status
	}
	var out []sessionOutcome
	for _, p := range l.listOn(d) {
		if !due(p, d) {
			continue
		}
		state, ok := byID[p.SupplementID]
		switch {
		case ok:
		case d.Time.Before(today.Time):
			state = sessionMissed
		default:
			state = sessionOpen
		}
		out = append(out, sessionOutcome{p.Name, state})
	}
	return out
}

// dayText shows d's supplements, like "creatine taken, fish oil not logged
// yet" or "none due". A log for a supplement not due that day shows as
// such.
func (l *supplementList) dayText(d, today pgtype.Date, logs []store.SupplementLog) string {
	var parts []string
	for _, o := range l.doses(d, today, logs) {
		if o.state == sessionOpen {
			parts = append(parts, o.name+" not logged yet")
		} else {
			parts = append(parts, o.name+" "+o.state)
		}
	}
	for _, r := range logs {
		name := l.ids[r.SupplementID]
		if p, ok := l.on(name, d); !ok || !due(p, d) {
			parts = append(parts, name+" "+r.Status+" (not due)")
		}
	}
	if len(parts) == 0 {
		return "none due"
	}
	return strings.Join(parts, ", ")
}

// week counts the doses due over the supplementWeek days ending today, like
// "last 7 days: 12 of 14 due doses taken, 1 skipped, 1 still due today".
// logs are the days' logs by date.
func (l *supplementList) week(today pgtype.Date, logs map[string][]store.SupplementLog) string {
	counts := map[string]int{}
	total := 0
	for i := range supplementWeek {
		d := addDays(today, i+1-supplementWeek)
		for _, o := range l.doses(d, today, logs[dateKey(d)]) {
			counts[o.state]++
			total++
		}
	}
	s := fmt.Sprintf("last %d days: ", supplementWeek)
	if total == 0 {
		return s + "none due"
	}
	s += fmt.Sprintf("%d of %s taken", counts["taken"], plural(total, "due dose"))
	for _, c := range []struct{ state, word string }{{"skipped", "skipped"}, {sessionMissed, "missed"}, {sessionOpen, "still due today"}} {
		if n := counts[c.state]; n > 0 {
			s += fmt.Sprintf(", %d %s", n, c.word)
		}
	}
	return s
}

// supplementUndoReply is the list after undoing a change to it.
func (a *Agent) supplementUndoReply(ctx context.Context, q *store.Queries, userID int64) (string, error) {
	user, err := q.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	l, err := loadSupplements(ctx, q, user)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, p := range l.listOn(a.localDate(user)) {
		parts = append(parts, l.describe(p))
	}
	if len(parts) == 0 {
		return "Supplements: none.", nil
	}
	return "Supplements: " + strings.Join(parts, "; ") + ".", nil
}

// supplementSection is the prompt's supplement list, the last week's logs
// with ids for edit_log and delete_log, and where today and the week stand.
func (a *Agent) supplementSection(ctx context.Context, user store.User) (string, error) {
	today := a.localDate(user)
	l, err := loadSupplements(ctx, a.Store, user)
	if err != nil {
		return "", fmt.Errorf("loading supplements: %w", err)
	}
	st, err := a.supplementStatus(ctx, a.Store, user, today)
	if err != nil {
		return "", fmt.Errorf("loading supplement logs: %w", err)
	}
	rows, err := a.Store.SupplementLogsBetween(ctx, store.SupplementLogsBetweenParams{UserID: user.ID,
		First: addDays(today, 1-supplementWeek), Last: today})
	if err != nil {
		return "", fmt.Errorf("loading supplement logs: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n## Supplements\nOn the list today:\n")
	on := l.listOn(today)
	switch {
	case len(on) == 0 && user.SupplementsAnswered:
		b.WriteString("(none; the user takes no supplements)\n")
	case len(on) == 0:
		b.WriteString("(none yet)\n")
	}
	for _, p := range on {
		b.WriteString(l.describe(p) + "\n")
	}
	b.WriteString("Logged in the last week, newest first:\n")
	if len(rows) == 0 {
		b.WriteString("(none yet)\n")
	}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		fmt.Fprintf(&b, "#%d %s: %s %s\n", r.ID, dayLabel(r.LocalDate, today), l.ids[r.SupplementID], r.Status)
	}
	b.WriteString("Status: " + st.forModel() + "\n")
	b.WriteString("Taking each supplement on its days is the goal. Save one the user takes with set_supplement action set, always " +
		"its whole plan: a short lowercase name (creatine, fish oil, vitamin d), the dose only if the user said one, the time " +
		"as 24-hour HH:MM if they gave a clock time, morning for \"in the morning\" or \"with breakfast\", night for \"every " +
		"night\" or \"before bed\", or null for any time, and days only if it isn't every day. To change one, set it again " +
		"with every field, copying the unchanged ones from the list above. Remove one with action remove, like \"I stopped " +
		"taking zinc\" or \"stop reminding me about zinc\". If the user takes none, use action none. Log with log_supplement: " +
		"taken for \"took my creatine\", skipped for \"skipped fish oil\"; names null for everything due that day and not " +
		"logged yet, like \"took my supplements\". date null for today, or the day it was for. A due supplement with nothing " +
		"logged by the end of the day counts as missed. Fix a log with edit_log type supplement (status or date) and remove " +
		"one with delete_log, by its id above or null for the latest. Reply to a change or log with the status in the tool result.\n")
	return b.String(), nil
}
