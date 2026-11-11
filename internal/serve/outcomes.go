package serve

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The outcomes page asks, of Lapilli itself, what the overview asks of the
// alerts: did the changes it led to do what it said they would, is the
// record it judges by kept up, and do people use what it shows. It answers
// from what the ledger already holds. Nothing is sent anywhere to answer
// them, and a page read isn't counted: only what people write is.

const (
	// A change is measured over up to four weeks either side of it; under
	// a week is too little to say.
	baseline = 28 * 24 * time.Hour
	leastFor = 7 * 24 * time.Hour
	// quietFor is how long without an incident before the page asks
	// whether they are still being written down.
	quietFor = 14 * 24 * time.Hour
	weekLen  = 7 * 24 * time.Hour
)

// outcomes are what the outcomes page, the API, the metrics and the digest
// show.
type outcomes struct {
	At time.Time `json:"at"`
	// Began is when lapilli serve first ran on the ledger.
	Began  time.Time `json:"began,omitzero"`
	Effect effect    `json:"effect"`
	Record record    `json:"record"`
	Use    use       `json:"use"`
}

// effect is whether the changes Lapilli led to did what it said.
type effect struct {
	// Then is the four weeks before lapilli serve began, or, with less than
	// a week kept from before, its first four; Now is the latest four.
	Then      *period `json:"then,omitempty"`
	ThenFirst bool    `json:"then_first_weeks,omitempty"`
	Now       *period `json:"now,omitempty"`
	// Changes are the reviewed changes to rules, the latest first.
	Changes []ruleOutcome `json:"changes"`
	// Fewer is how many fewer times the changed rules fired since than at
	// their rates before, the rules written counting as more; FewerBlack,
	// of firings that caught nothing.
	Fewer       int `json:"fewer_firings"`
	FewerBlack  int `json:"fewer_black"`
	Measured    int `json:"measured"`
	AsPredicted int `json:"as_predicted"`
	Measuring   int `json:"measuring"`
	// Added are the rules written, and the incidents they caught since.
	Added       int `json:"rules_added"`
	AddedCaught int `json:"rules_added_caught"`
}

// period is the stones of all the rules over a stretch of time.
type period struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Firings   int       `json:"firings"`
	White     int       `json:"white"`
	Incidents int       `json:"incidents"`
	Missed    int       `json:"missed"`
}

// perWeek is n over the period, a week at a time.
func (p period) perWeek(n int) float64 { return rate(n, p.From, p.To) }

func rate(n int, from, to time.Time) float64 {
	if !to.After(from) {
		return 0
	}
	return float64(n) * float64(weekLen) / float64(to.Sub(from))
}

// ruleOutcome is a reviewed change to a rule: what replaying the week
// before said it would do, and what it did.
type ruleOutcome struct {
	Rule string    `json:"rule"`
	At   time.Time `json:"at"`
	// Kind is changed, added or removed; unreplayed when the rule can't be
	// replayed after it (paused, disabled, or beyond the replay), resumed
	// when it couldn't be before.
	Kind string `json:"kind"`
	// PredBefore and PredAfter are its firings in the week before, as it
	// was and as it became: -1 where it wasn't there, or wasn't replayed.
	PredBefore int `json:"predicted_before"`
	PredAfter  int `json:"predicted_after"`
	// Before is measured over up to four weeks before it, since the rule
	// last changed; After, over up to four weeks after; Since, until the
	// rule next changed or now.
	BeforeFrom time.Time   `json:"before_from"`
	Before     ledger.Span `json:"before"`
	AfterTo    time.Time   `json:"after_to"`
	After      ledger.Span `json:"after"`
	Until      time.Time   `json:"until"`
	Since      ledger.Span `json:"since"`
	// State is as-predicted or otherwise once measured; before then
	// measuring; superseded when the rule changed again within the week;
	// no-baseline with under a week before it; else its Kind.
	State string `json:"state"`
	// Fewer is how many fewer times it fired since than at its rate before
	// (none, for a rule written); FewerBlack, of firings with no incident.
	Fewer      int `json:"fewer_firings"`
	FewerBlack int `json:"fewer_black"`
}

// record is whether the record is kept up: the runs, the incidents, the
// impact windows decided.
type record struct {
	Since      time.Time `json:"since,omitzero"` // the earliest firing kept
	Cursor     time.Time `json:"cursor,omitzero"`
	LastRun    time.Time `json:"last_run,omitzero"`
	Behind     bool      `json:"behind"`
	FailedRuns int       `json:"failed_runs_7d"`
	Unreplayed []string  `json:"unreplayed"`
	// LastIncident is when the latest incident written down started.
	LastIncident time.Time    `json:"last_incident,omitzero"`
	Quiet        bool         `json:"quiet"` // none written down in two weeks
	Decided      int          `json:"impact_decided_4w"`
	Undecided    int          `json:"impact_undecided_4w"`
	Weeks        []sourceWeek `json:"weeks"` // twelve, the earliest first
}

// sourceWeek is a week's incidents by where they came from: the incidents
// file or a tool they were copied in from, the UI (impact windows confirmed
// among them), or impact windows not yet decided.
type sourceWeek struct {
	Week   time.Time `json:"week"`
	File   int       `json:"file"`
	UI     int       `json:"ui"`
	Impact int       `json:"impact"`
}

// use is whether people use what the ledger shows: what they write.
type use struct {
	Edits int `json:"edits_4w"`
	// People is -1 when no sign-in proxy names them.
	People    int            `json:"people_4w"`
	ByKind    map[string]int `json:"edits_by_kind_4w"`
	Weeks     []editWeek     `json:"weeks"` // twelve, the earliest first
	TasksMade int            `json:"tasks_made_12w"`
	TasksDone int            `json:"tasks_done_12w"`
	// DoneIn is the median time from a task made to done.
	DoneIn  time.Duration `json:"median_done_in_ns"`
	Open    int           `json:"tasks_open"`
	Overdue int           `json:"tasks_overdue"`
	// Suggested are the rules the last four digests suggested a longer
	// "for" for; ActedOn, those a task was made for or that changed since.
	Suggested int `json:"suggested_4w"`
	ActedOn   int `json:"acted_on_4w"`
}

type editWeek struct {
	Week   time.Time `json:"week"`
	Edits  int       `json:"edits"`
	People int       `json:"people"`
}

// outcomesCache keeps the outcomes worked out, for the pages, the API and
// the metrics alike, until the ledger changes or the hour does.
type outcomesCache struct {
	sync.Mutex
	key string
	v   *outcomes
}

// outcomes are the outcomes as of now, not to be changed: they may be
// another reader's too.
func (s *Server) outcomes(ctx context.Context) (*outcomes, error) {
	key := fmt.Sprint(s.l.Gen(), s.now().Truncate(time.Hour).Unix())
	s.outs.Lock()
	if v := s.outs.v; v != nil && s.outs.key == key {
		s.outs.Unlock()
		return v, nil
	}
	s.outs.Unlock()
	v, err := s.workOutcomes(ctx)
	if err != nil {
		return nil, err
	}
	s.outs.Lock()
	s.outs.key, s.outs.v = key, v
	s.outs.Unlock()
	return v, nil
}

// workOutcomes works the outcomes out from the ledger.
func (s *Server) workOutcomes(ctx context.Context) (*outcomes, error) {
	out := &outcomes{At: s.now().UTC()}
	rec := &out.Record
	var err error
	if rec.Since, _, err = s.l.Bounds(ctx); err != nil {
		return nil, err
	}
	if rec.Cursor, err = s.l.Cursor(ctx); err != nil {
		return nil, err
	}
	if out.Began, err = s.began(ctx); err != nil {
		return nil, err
	}
	if err := s.recordKept(ctx, out); err != nil {
		return nil, err
	}
	if err := s.effects(ctx, out); err != nil {
		return nil, err
	}
	if err := s.inUse(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// began is when lapilli serve first ran on the ledger: kept from the first
// run on, or else the earliest run the ledger has.
func (s *Server) began(ctx context.Context) (time.Time, error) {
	v, err := s.l.Meta(ctx, "began")
	if err != nil {
		return time.Time{}, err
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && v != "" {
		return time.Unix(n, 0).UTC(), nil
	}
	return s.l.FirstRun(ctx)
}

// keepBegan notes, once, when lapilli serve first ran on the ledger, so
// that pruning the runs doesn't move it.
func (s *Server) keepBegan(ctx context.Context) error {
	if v, err := s.l.Meta(ctx, "began"); err != nil || v != "" {
		return err
	}
	at, err := s.l.FirstRun(ctx)
	if err != nil || at.IsZero() {
		return err
	}
	return s.l.SetMeta(ctx, "began", strconv.FormatInt(at.Unix(), 10))
}

func (s *Server) recordKept(ctx context.Context, out *outcomes) error {
	now, rec := out.At, &out.Record
	runs, err := s.l.Runs(ctx, 1)
	if err != nil {
		return err
	}
	if len(runs) > 0 {
		rec.LastRun = runs[0].Finished
	}
	if rec.FailedRuns, err = s.l.FailedRuns(ctx, now.Add(-weekLen)); err != nil {
		return err
	}
	rec.Behind = !rec.Cursor.IsZero() && now.Sub(rec.Cursor) > 2*s.cfg.Every+s.cfg.Settle
	rec.Unreplayed = []string{}
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return err
	}
	if ok {
		table, err := s.l.RuleTable(ctx)
		if err != nil {
			return err
		}
		why := map[string]string{}
		for _, r := range table {
			why[r.Name] = r.Unsupported
		}
		for _, n := range latest.Names {
			if n != "" && why[n] != "" && !slices.Contains(rec.Unreplayed, n) {
				rec.Unreplayed = append(rec.Unreplayed, n)
			}
		}
		slices.Sort(rec.Unreplayed)
	}
	if rec.LastIncident, err = s.l.LastIncident(ctx, now); err != nil {
		return err
	}
	rec.Quiet = !rec.Since.IsZero() && now.Sub(rec.Since) >= quietFor &&
		(rec.LastIncident.IsZero() || now.Sub(rec.LastIncident) >= quietFor)

	// Twelve weeks of incidents, by where they came from, and the impact
	// windows of the last four, decided or not.
	this := ledger.Week(s.l.Day(now))
	first := this - 7*(weeks-1)
	for i := range weeks {
		rec.Weeks = append(rec.Weeks, sourceWeek{Week: s.l.Midnight(first + 7*int64(i))})
	}
	incs, err := s.l.Incidents(ctx, s.l.Midnight(first), now.Add(24*time.Hour), true)
	if err != nil {
		return err
	}
	recent := now.Add(-baseline)
	for _, in := range incs {
		if in.Origin == "impact" && !in.Start.Before(recent) {
			if in.State != "" {
				rec.Decided++
			} else if in.Counts {
				rec.Undecided++
			}
		}
		i := int((ledger.Week(s.l.Day(in.Start)) - first) / 7)
		if !in.Counts || i < 0 || i >= weeks {
			continue
		}
		switch x := &rec.Weeks[i]; in.Origin {
		case "file", "copy":
			x.File++
		case "impact":
			x.Impact++
		default:
			x.UI++
		}
	}
	return nil
}

func (s *Server) effects(ctx context.Context, out *outcomes) error {
	e, rec := &out.Effect, out.Record
	e.Changes = []ruleOutcome{}
	if rec.Cursor.IsZero() {
		return nil
	}

	// Then and now.
	began := out.Began
	if began.IsZero() || began.Before(rec.Since) {
		began = rec.Since
	}
	var then *period
	if from := later(began.Add(-baseline), rec.Since); began.Sub(from) >= leastFor {
		then = &period{From: from, To: began}
	} else {
		// Too little kept from before: its first weeks stand for then.
		e.ThenFirst = true
		if to := earlier(began.Add(baseline), rec.Cursor); to.Sub(began) >= leastFor {
			then = &period{From: began, To: to}
		}
	}
	if then != nil {
		if from := later(rec.Cursor.Add(-baseline), then.To); rec.Cursor.Sub(from) >= leastFor {
			e.Then, e.Now = then, &period{From: from, To: rec.Cursor}
			for _, p := range []*period{e.Then, e.Now} {
				var err error
				if p.Firings, p.White, err = s.l.Stones(ctx, p.From, p.To); err != nil {
					return err
				}
				incs, err := s.l.Incidents(ctx, p.From, p.To, false)
				if err != nil {
					return err
				}
				p.Incidents = len(incs)
				for _, in := range incs {
					if len(in.CaughtBy) == 0 {
						p.Missed++
					}
				}
			}
		}
	}

	// Each reviewed change to a rule, measured.
	rs, err := s.l.Results(ctx, "review", 500)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range rs {
		var c verdict.Comparison
		if err := r.Decode(&c); err != nil {
			return err
		}
		for _, rc := range c.Rules {
			// One that fires alike changes nothing to measure: the change
			// before it goes on being measured. A review kept twice, after
			// a write that failed half way, counts once.
			key := fmt.Sprint(rc.Rule, "\x00", r.At.Unix())
			if rc.Alike() || seen[key] {
				continue
			}
			seen[key] = true
			x := ruleOutcome{Rule: rc.Rule, At: r.At, Kind: "changed", PredBefore: -1, PredAfter: -1}
			replayed := func(r *verdict.RuleResult) bool { return r != nil && r.Unsupported == "" }
			if replayed(rc.Before) {
				x.PredBefore = rc.Before.Firings
			}
			if replayed(rc.After) {
				x.PredAfter = rc.After.Firings
			}
			// One that can't be replayed after it (paused, disabled, or
			// beyond the replay) has nothing in the ledger to measure, and
			// one that couldn't before, nothing to measure against; both
			// still end the change before.
			switch {
			case rc.After != nil && !replayed(rc.After):
				x.Kind = "unreplayed"
			case rc.After == nil:
				x.Kind = "removed"
			case rc.Before != nil && !replayed(rc.Before):
				x.Kind = "resumed"
			case rc.Before == nil:
				x.Kind = "added"
			}
			e.Changes = append(e.Changes, x)
		}
	}
	// A rule's changes in turn, to know when it last changed and next did.
	cs := e.Changes
	sort.SliceStable(cs, func(a, b int) bool {
		if cs[a].Rule != cs[b].Rule {
			return cs[a].Rule < cs[b].Rule
		}
		return cs[a].At.Before(cs[b].At)
	})
	for a := 0; a < len(cs); {
		b := a + 1
		for b < len(cs) && cs[b].Rule == cs[a].Rule {
			b++
		}
		if err := s.measure(ctx, cs[a:b], rec); err != nil {
			return err
		}
		a = b
	}
	for _, x := range cs {
		e.Fewer += x.Fewer
		e.FewerBlack += x.FewerBlack
		switch x.State {
		case "as-predicted":
			e.Measured++
			e.AsPredicted++
		case "otherwise":
			e.Measured++
		case "measuring":
			e.Measuring++
		}
		if x.Kind == "added" {
			e.Added++
			e.AddedCaught += x.Since.Caught
		}
	}
	sort.SliceStable(cs, func(a, b int) bool {
		if !cs[a].At.Equal(cs[b].At) {
			return cs[a].At.After(cs[b].At)
		}
		return cs[a].Rule < cs[b].Rule
	})
	return nil
}

// measure sets a rule's changes, the earliest first, beside what came of
// each, reading the rule's stones once for them all. The firings a change
// saved are those at the rule's rate before it, over the time since, less
// those there were.
func (s *Server) measure(ctx context.Context, xs []ruleOutcome, rec record) error {
	var cuts []time.Time
	for i := range xs {
		x := &xs[i]
		x.Until = rec.Cursor
		if i+1 < len(xs) && xs[i+1].At.Before(x.Until) {
			x.Until = xs[i+1].At
		}
		x.Until = later(x.Until, x.At)
		x.BeforeFrom = later(x.At.Add(-baseline), rec.Since)
		if i > 0 {
			x.BeforeFrom = later(x.BeforeFrom, xs[i-1].At)
		}
		x.BeforeFrom = earlier(x.BeforeFrom, x.At)
		if x.Kind == "added" || x.Kind == "resumed" {
			x.BeforeFrom = x.At
		}
		x.AfterTo = earlier(x.At.Add(baseline), x.Until)
		cuts = append(cuts, x.BeforeFrom, x.At, x.AfterTo, x.Until)
	}
	slices.SortFunc(cuts, time.Time.Compare)
	cuts = slices.CompactFunc(cuts, time.Time.Equal)
	spans, err := s.l.RuleSpans(ctx, xs[0].Rule, cuts)
	if err != nil {
		return err
	}
	sum := func(from, to time.Time) ledger.Span {
		var t ledger.Span
		for i, sp := range spans {
			if !cuts[i].Before(from) && !cuts[i+1].After(to) {
				t.Firings += sp.Firings
				t.White += sp.White
				t.Excused += sp.Excused
				t.Caught += sp.Caught
			}
		}
		return t
	}
	for i := range xs {
		x := &xs[i]
		x.Before = sum(x.BeforeFrom, x.At)
		x.After, x.Since = sum(x.At, x.AfterTo), sum(x.At, x.Until)
		// A rule written made none before; its firings since are as many
		// more, so a rule renamed, taken away and written in one change,
		// comes to nothing.
		base := x.At.Sub(x.BeforeFrom) >= leastFor
		switch {
		case x.Kind == "added":
			x.Fewer, x.FewerBlack = -x.Since.Firings, -x.Since.Black()
		case (x.Kind == "changed" || x.Kind == "removed") && base:
			k := float64(x.Until.Sub(x.At)) / float64(x.At.Sub(x.BeforeFrom))
			x.Fewer = int(math.Round(float64(x.Before.Firings)*k)) - x.Since.Firings
			x.FewerBlack = int(math.Round(float64(x.Before.Black())*k)) - x.Since.Black()
		}
		switch {
		case i+1 < len(xs) && xs[i+1].At.Sub(x.At) < leastFor:
			x.State = "superseded"
		case x.Kind != "changed":
			x.State = x.Kind
		case x.AfterTo.Sub(x.At) < leastFor:
			x.State = "measuring"
		case !base:
			x.State = "no-baseline"
		case asPredicted(*x):
			x.State = "as-predicted"
		default:
			x.State = "otherwise"
		}
	}
	return nil
}

// asPredicted reports whether a change came out as its review said: its
// firings a week since within a quarter, or two, of those the replay of
// the week before gave it, scaled by how the weeks before compare with
// that one.
func asPredicted(x ruleOutcome) bool {
	before, after := rate(x.Before.Firings, x.BeforeFrom, x.At), rate(x.After.Firings, x.At, x.AfterTo)
	want := float64(x.PredAfter)
	if x.PredBefore > 0 && before > 0 {
		want *= before / float64(x.PredBefore)
	}
	return math.Abs(after-want) <= max(2, want/4)
}

func (s *Server) inUse(ctx context.Context, out *outcomes) error {
	now, u := out.At, &out.Use
	this := ledger.Week(s.l.Day(now))
	first := this - 7*(weeks-1)
	from, recent := s.l.Midnight(first), now.Add(-baseline)
	cs, err := s.l.ChangesSince(ctx, from, 100000)
	if err != nil {
		return err
	}
	named := s.cfg.UserHeader != ""
	u.ByKind = map[string]int{}
	people, byWeek := map[string]bool{}, make([]map[string]bool, weeks)
	for i := range weeks {
		u.Weeks = append(u.Weeks, editWeek{Week: s.l.Midnight(first + 7*int64(i))})
		byWeek[i] = map[string]bool{}
	}
	for _, c := range cs {
		i := int((ledger.Week(s.l.Day(c.At)) - first) / 7)
		if i < 0 || i >= weeks || c.By == tokenUser || c.By == ledger.FiringsCopier || c.By == ledger.ChangesCopier || c.By == ledger.Proposer {
			continue
		}
		u.Weeks[i].Edits++
		who := c.By != "" && c.By != "anonymous"
		if who {
			byWeek[i][c.By] = true
		}
		if !c.At.Before(recent) {
			u.Edits++
			u.ByKind[editKind(c.What)]++
			if who {
				people[c.By] = true
			}
		}
	}
	u.People = len(people)
	for i := range u.Weeks {
		u.Weeks[i].People = len(byWeek[i])
		if !named {
			u.Weeks[i].People = -1
		}
	}
	if !named {
		u.People = -1
	}

	ts, err := s.l.TasksSince(ctx, from)
	if err != nil {
		return err
	}
	var took []time.Duration
	for _, t := range ts {
		if !t.Created.Before(from) {
			u.TasksMade++
		}
		if t.Status == "done" && !t.Done.Before(from) {
			u.TasksDone++
			took = append(took, t.Done.Sub(t.Created))
		}
	}
	if n := len(took); n > 0 {
		slices.Sort(took)
		u.DoneIn = took[n/2]
		if n%2 == 0 {
			u.DoneIn = (took[n/2-1] + took[n/2]) / 2
		}
	}
	// Overdue as the tasks page has it: due before today.
	if u.Open, u.Overdue, _, err = s.l.TaskCounts(ctx, now, s.l.Midnight(s.l.Day(now))); err != nil {
		return err
	}

	// The suggestions of the last four digests: was anything done about
	// each rule since the week it was suggested in?
	rs, err := s.l.Results(ctx, "suggestions", 4)
	if err != nil {
		return err
	}
	since := map[string]time.Time{}
	for _, r := range rs {
		if r.At.Before(now.Add(-5 * weekLen)) {
			continue
		}
		var ss []verdict.Suggestion
		if err := r.Decode(&ss); err != nil {
			return err
		}
		for _, x := range ss {
			since[x.Rule] = r.From // the earliest, as the latest come first
		}
	}
	u.Suggested = len(since)
	for rule, at := range since {
		if slices.ContainsFunc(ts, func(t ledger.Task) bool { return t.Rule == rule && !t.Created.Before(at) }) ||
			slices.ContainsFunc(out.Effect.Changes, func(c ruleOutcome) bool { return c.Rule == rule && !c.At.Before(at) }) {
			u.ActedOn++
		}
	}
	return nil
}

// editKinds are what an edit can be to, by the start of what the ledger
// noted of it.
var editKinds = []struct{ prefix, kind, label string }{
	{"impact window ", "decisions", "impact windows decided"}, {"incident ", "incidents", "incidents written"},
	{"task ", "tasks", "tasks"}, {"quiet window ", "windows", "quiet windows"}, {"note on ", "notes", "notes on rules"},
	{"comment ", "comments", "comments"}, {"team ", "teams", "teams"}, {"setting ", "settings", "settings"},
}

func editKind(what string) string {
	for _, k := range editKinds {
		if strings.HasPrefix(what, k.prefix) {
			return k.kind
		}
	}
	return "other"
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
