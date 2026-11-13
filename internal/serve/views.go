package serve

import (
	"context"
	"fmt"
	"html/template"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// weeks is how many weeks the overview and the rules' sparklines show.
const weeks = 12

func noisy(firings, white int, o verdict.Options) bool {
	n, p := o.NoisyFirings, o.NoisyPrecision
	if n == 0 {
		n = verdict.NoisyFirings
	}
	if p == 0 {
		p = verdict.NoisyPrecision
	}
	return firings > 0 && firings >= n && float64(white)/float64(firings) < p
}

// tallies are each rule's stones in each of the last n weeks.
type tallies struct {
	weeks []int64                          // Mondays, the earliest first
	by    map[int64]map[int64]ledger.Tally // week to rule id to stones
	// everyone is by, before only kept one team's rules.
	everyone map[int64]map[int64]ledger.Tally
	rules    map[int64]ledger.RuleRow
	names    map[string]int64 // rule ids by name, made when first needed
	o        verdict.Options
	// firstWeek is the Monday of the week the ledger's first firing fell
	// in; ok false with no firing.
	firstWeek int64
	firstOK   bool
}

// whole are, of the weeks ws, those the ledger has whole: not the week
// under way, the last t has, nor the one its first firing fell in, which
// it holds only part of. What "weeks with a re-fire" are counted of,
// everywhere (docs/design/noc.md).
func (t *tallies) whole(ws []int64) []int64 {
	var out []int64
	for _, w := range ws {
		if t.firstOK && w > t.firstWeek && w < t.weeks[len(t.weeks)-1] {
			out = append(out, w)
		}
	}
	return out
}

// refireWeeks are, of the whole weeks among ws, how many had a firing of
// the rules in that re-fired, of how many.
func (t *tallies) refireWeeks(ws []int64, in func(id int64) bool) (with, of int) {
	for _, w := range t.whole(ws) {
		of++
		for id, x := range t.by[w] {
			if x.Refired > 0 && in(id) {
				with++
				break
			}
		}
	}
	return with, of
}

func (s *Server) tallies(ctx context.Context, n int) (*tallies, error) {
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	now := ledger.Week(s.l.Day(s.now()))
	t := &tallies{by: map[int64]map[int64]ledger.Tally{}, o: o}
	for i := n - 1; i >= 0; i-- {
		w := now - int64(7*i)
		t.weeks = append(t.weeks, w)
		t.by[w] = map[int64]ledger.Tally{}
	}
	rows, err := s.l.Weeks(ctx, t.weeks[0], now)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		t.by[r.At][r.Rule] = r
	}
	if t.rules, err = s.l.RuleTable(ctx); err != nil {
		return nil, err
	}
	first, _, err := s.l.Bounds(ctx)
	if err != nil {
		return nil, err
	}
	if !first.IsZero() {
		t.firstWeek, t.firstOK = ledger.Week(s.l.Day(first)), true
	}
	return t, nil
}

// id is a rule's id, by its name.
func (t *tallies) id(name string) int64 {
	if t.names == nil {
		t.names = map[string]int64{}
		for id, r := range t.rules {
			t.names[r.Name] = id
		}
	}
	return t.names[name]
}

func (t *tallies) noisy(week, rule int64) bool {
	x := t.by[week][rule]
	return noisy(x.Firings, x.White, t.o)
}

// state is whether an incident was caught, caught only by rules noisy that
// week, or missed.
func (t *tallies) state(in ledger.Incident, week int64) string {
	if len(in.CaughtBy) == 0 {
		return "missed"
	}
	for _, c := range in.CaughtBy {
		if !t.noisy(week, t.id(c.Rule)) {
			return "caught"
		}
	}
	return "noisy"
}

type spark struct {
	X, WhiteY, WhiteH, BlackY, BlackH float64
	Title                             string
}

type ruleLine struct {
	Name, Severity, Group                  string
	Team, Status                           string // from teams and the rule's note
	Firings, White, Black, Refired, Streak int
	Excused                                int
	Precision                              string
	Noisy                                  bool
	Spark                                  []spark
	Suggestion, Unsupported                string
	// Source is where a rule's firings were recorded before they were
	// copied in: one not replayed.
	Source string
	// What the rule adds to the others, over the weeks shown: see
	// verdict.Credit.
	Incidents, First, Alone int
	Spare                   bool
	// SomeRules says only some rules are judged (--rule), so what one adds to
	// the others can't be said.
	SomeRules bool
	// AsleepBlack and OffHoursBlack are the firings that caught nothing while
	// people slept, and off hours, over the weeks shown.
	AsleepBlack, OffHoursBlack int
	// What its notifications came to, over the weeks shown.
	pagedLine
	// Differs says its replay has differed from what really fired over the
	// last week (docs/design/fidelity.md).
	Differs bool
	// Content is how many problems what it says has, by kind
	// (docs/design/content.md).
	Content contentCount
	// Proposed says a change to it is proposed and not yet finished with.
	Proposed bool
	// RefiredShare is the share of its firings that re-fired, and
	// RefiredWeeks how many of the whole weeks among those shown, RefiredOf,
	// had one (tallies.whole).
	RefiredShare            string
	RefiredWeeks, RefiredOf int
	// Deploys are its firings that came during a deploy, DeploysCaught
	// those of them that caught something, and Without its precision
	// leaving them out.
	Deploys, DeploysCaught int
	Without                string
}

// pct is a share as a page shows it: "–" for none.
func pct(n, of int) string {
	if of == 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(of))
}

// hoursCache keeps when each rule's firings came, week by week and by each
// of the hours rules are counted by, each week as read at a generation of
// the ledger: it stands until the ledger says that week's firings changed.
type hoursCache struct {
	sync.Mutex
	by map[hoursAt]hoursWeek
}

// hoursAt is a week, and the key of the hours it was counted by.
type hoursAt struct {
	week  int64
	hours string
	rules string // of these rules only, when not every rule's
}

type hoursWeek struct {
	gen int64
	by  map[int64]ledger.HourTally // by rule id
}

// byHours is when each rule's firings came in each of the weeks t has, by
// the hours of the rule's team, or the server's: by week, then rule id. It
// is nil without hours to count by. A run changes the last week or two, so
// only those are counted again.
func (s *Server) byHours(ctx context.Context, t *tallies) (map[int64]map[int64]ledger.HourTally, error) {
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	sc := own.schedules(t.o.Hours)
	if sc.none() {
		return nil, nil
	}
	// The hours the rules go by, each counted once however many go by it:
	// the server's a stretch of hours at a time for every rule, a team's by
	// reading the firings of its rules that fired that week, so that many
	// teams' hours cost no more than reading the firings once.
	server := ""
	if sc.server != nil {
		server = ledger.HoursKey(sc.server)
	}
	hours := map[string]*verdict.Hours{}
	keys := make(map[int64]string, len(t.rules))
	for id, r := range t.rules {
		if h := sc.of(r.Name); h != nil {
			k := ledger.HoursKey(h)
			hours[k], keys[id] = h, k
		}
	}
	out := make(map[int64]map[int64]ledger.HourTally, len(t.weeks))
	for _, w := range t.weeks {
		fired := map[string][]int64{}
		for id := range t.by[w] {
			if k, ok := keys[id]; ok {
				fired[k] = append(fired[k], id)
			}
		}
		mixed := map[int64]ledger.HourTally{}
		for k, ids := range fired {
			var by map[int64]ledger.HourTally
			var err error
			if k == server {
				by, err = s.weekHours(ctx, w, k, hours[k], nil)
			} else {
				slices.Sort(ids)
				by, err = s.weekHours(ctx, w, k, hours[k], ids)
			}
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				if x, ok := by[id]; ok {
					mixed[id] = x
				}
			}
		}
		out[w] = mixed
	}
	s.hours.Lock()
	for at := range s.hours.by {
		if at.week < t.weeks[0]-7*weeks || hours[at.hours] == nil {
			delete(s.hours.by, at) // long gone from every page, or no rule's hours now
		}
	}
	s.hours.Unlock()
	return out, nil
}

// weekHours is when each rule's firings came in a week, by hours h whose key
// is k, kept until the ledger says the week's firings changed: of every rule,
// or of the rules ids only.
func (s *Server) weekHours(ctx context.Context, w int64, k string, h *verdict.Hours, ids []int64) (map[int64]ledger.HourTally, error) {
	at := hoursAt{week: w, hours: k}
	if ids != nil {
		at.rules = fmt.Sprint(ids)
	}
	s.hours.Lock()
	c, ok := s.hours.by[at]
	s.hours.Unlock()
	if ok && c.gen >= s.l.Changed(w) {
		return c.by, nil
	}
	gen := s.l.Gen()
	var by map[int64]map[int64]ledger.HourTally
	var err error
	if ids == nil {
		by, err = s.l.ByHours(ctx, w, w+7, h)
	} else {
		by, err = s.l.ByHoursOf(ctx, w, w+7, h, ids)
	}
	if err != nil {
		return nil, err
	}
	s.hours.Lock()
	if s.hours.by == nil {
		s.hours.by = map[hoursAt]hoursWeek{}
	}
	for old := range s.hours.by {
		if old.week == w && old.hours == k && old.rules != "" && old != at {
			delete(s.hours.by, old) // the rules that fired that week, before more did
		}
	}
	s.hours.by[at] = hoursWeek{gen, by[w]}
	s.hours.Unlock()
	return by[w], nil
}

// creditCache keeps what each rule adds, worked out once for each version of
// the ledger and each span of weeks: the overview, the rules, every rule's page
// and the digest ask for it.
type creditCache struct {
	sync.Mutex
	gen int64
	by  map[[2]int64]map[string]verdict.RuleResult // the last week and n
}

// credit is what each rule adds to the others over the last n weeks: against
// every rule's catches, whichever team's rules are shown, since another
// team's rule catching an incident as soon is as good.
func (s *Server) credit(ctx context.Context, t *tallies, n int) (map[string]verdict.RuleResult, error) {
	gen, key := s.l.Gen(), [2]int64{t.weeks[len(t.weeks)-1], int64(n)}
	s.creds.Lock()
	if s.creds.gen == gen && s.creds.by[key] != nil {
		out := s.creds.by[key]
		s.creds.Unlock()
		return out, nil
	}
	s.creds.Unlock()
	out, err := s.workCredit(ctx, t, n)
	if err != nil {
		return nil, err
	}
	s.creds.Lock()
	if s.creds.gen != gen {
		s.creds.gen, s.creds.by = gen, map[[2]int64]map[string]verdict.RuleResult{}
	}
	s.creds.by[key] = out
	s.creds.Unlock()
	return out, nil
}

func (s *Server) workCredit(ctx context.Context, t *tallies, n int) (map[string]verdict.RuleResult, error) {
	by := t.by
	if t.everyone != nil {
		by = t.everyone
	}
	recent := t.weeks[len(t.weeks)-n:]
	sums := map[int64]*verdict.RuleResult{}
	for _, w := range recent {
		for id, x := range by[w] {
			r := sums[id]
			if r == nil {
				row := t.rules[id]
				r = &verdict.RuleResult{Rule: row.Name, Severity: row.Severity, Unsupported: row.Unsupported}
				sums[id] = r
			}
			r.Firings += x.Firings
			r.White += x.White
		}
	}
	rs := make([]verdict.RuleResult, 0, len(sums))
	noisyRule := make(map[string]bool, len(sums))
	for _, r := range sums {
		r.Noisy = noisy(r.Firings, r.White, t.o)
		noisyRule[r.Rule] = r.Noisy
		rs = append(rs, *r)
	}
	incs, err := s.l.Incidents(ctx, s.l.Midnight(recent[0]), s.now(), false)
	if err != nil {
		return nil, err
	}
	results := make([]verdict.IncidentResult, len(incs))
	for i, in := range incs {
		for j := range in.CaughtBy {
			in.CaughtBy[j].Noisy = noisyRule[in.CaughtBy[j].Rule]
		}
		results[i] = verdict.IncidentResult{Incident: in.Incident, CaughtBy: in.CaughtBy}
	}
	// What can still fire is what the rules in effect now hold, and every
	// recorded rule: its source may send more.
	var inEffect func(string) bool
	if latest, ok, err := s.l.Latest(ctx); err != nil {
		return nil, err
	} else if ok {
		names := make(map[string]bool, len(latest.Names))
		for _, name := range latest.Names {
			names[name] = true
		}
		for _, r := range t.rules {
			if r.Source != "" {
				names[r.Name] = true
			}
		}
		inEffect = func(rule string) bool { return names[rule] }
	}
	rep := verdict.Report{Rules: rs, Incidents: results, SomeRules: t.o.SomeRules}
	rep.Credit(inEffect)
	out := make(map[string]verdict.RuleResult, len(rs))
	for _, r := range rs {
		out[r.Rule] = r
	}
	return out, nil
}

// ruleLines are each rule's stones over the last n weeks, the noisiest
// first, and the rules that never fired.
func (s *Server) ruleLines(ctx context.Context, l string, t *tallies, n int) ([]ruleLine, []string, error) {
	sugg, err := s.suggestions(ctx, l)
	if err != nil {
		return nil, nil, err
	}
	sum := map[int64]*ruleLine{}
	recent := t.weeks[len(t.weeks)-n:]
	for _, w := range recent {
		for id, x := range t.by[w] {
			l := sum[id]
			if l == nil {
				r := t.rules[id]
				l = &ruleLine{Name: r.Name, Severity: r.Severity, Group: r.Group, Unsupported: r.Unsupported, Source: r.Source}
				sum[id] = l
			}
			l.Firings += x.Firings
			l.White += x.White
			l.Refired += x.Refired
			l.Excused += x.Excused
			l.Deploys += x.Deploy
			l.DeploysCaught += x.DeployWhite
		}
	}
	credit, err := s.credit(ctx, t, n)
	if err != nil {
		return nil, nil, err
	}
	hours, err := s.byHours(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range recent {
		for id, x := range hours[w] {
			if l := sum[id]; l != nil {
				l.AsleepBlack += x.AsleepBlack
				l.OffHoursBlack += x.OffHoursBlack
			}
		}
	}
	var out []ruleLine
	for id, l := range sum {
		c := credit[l.Name]
		l.Incidents, l.First, l.Alone, l.Spare, l.SomeRules = c.Incidents, c.First, c.Alone, c.Spare, t.o.SomeRules
		l.Black = l.Firings - l.White
		l.Noisy = noisy(l.Firings, l.White, t.o)
		l.Precision = pct(l.White, l.Firings)
		l.RefiredShare = pct(l.Refired, l.Firings)
		// refireWeeks for this rule alone, a week at a time rather than
		// every rule's each week: two thousand rules cost what forty do.
		for _, w := range t.whole(recent) {
			l.RefiredOf++
			if t.by[w][id].Refired > 0 {
				l.RefiredWeeks++
			}
		}
		l.Without = pct(l.White-l.DeploysCaught, l.Firings-l.Deploys)
		// Weeks running it has been noisy, up to the last whole week.
		for i := len(t.weeks) - 2; i >= 0 && t.noisy(t.weeks[i], id); i-- {
			l.Streak++
		}
		most := 1
		for _, w := range t.weeks {
			most = max(most, t.by[w][id].Firings)
		}
		for i, w := range t.weeks {
			x := t.by[w][id]
			const h = 18.0
			wh, bh := h*float64(x.White)/float64(most), h*float64(x.Black())/float64(most)
			l.Spark = append(l.Spark, spark{X: float64(i) * 6, WhiteY: h - wh, WhiteH: wh, BlackY: h - wh - bh, BlackH: bh,
				Title: fmt.Sprintf("%s: %d ○ %d ●", s.l.Midnight(w).In(s.cfg.Loc).Format("01-02"), x.White, x.Black())})
		}
		l.Suggestion = sugg[l.Name]
		out = append(out, *l)
	}
	sort.Slice(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.Black != y.Black {
			return x.Black > y.Black
		}
		if x.Firings != y.Firings {
			return x.Firings > y.Firings
		}
		return x.Name < y.Name
	})
	var silent []string
	if latest, ok, err := s.l.Latest(ctx); err != nil {
		return nil, nil, err
	} else if ok {
		// Fired at all in the twelve weeks kept, not only in the n shown.
		fired := map[string]bool{}
		for _, w := range t.weeks {
			for id, x := range t.by[w] {
				if x.Firings+x.Excused > 0 {
					fired[t.rules[id].Name] = true
				}
			}
		}
		for _, name := range latest.Names {
			if name != "" && !fired[name] && !slices.Contains(silent, name) {
				silent = append(silent, name)
			}
		}
		// A rule that isn't replayed has no firings of its own to have. It
		// isn't silent; it is unknown: unless it is a cloud's alarm whose
		// history, copied in, is tied to it, and reaches back twelve weeks.
		// One turned off is silent.
		if len(silent) > 0 {
			all, names, err := s.named(ctx, latest.ID)
			if err != nil {
				return nil, nil, err
			}
			ties, err := s.l.Ties(ctx)
			if err != nil {
				return nil, nil, err
			}
			for i, r := range all {
				if ok, _, _, err := s.recordOK(ctx, names[i], r, ties); err != nil {
					return nil, nil, err
				} else if !ok {
					silent = slices.DeleteFunc(silent, func(n string) bool { return n == names[i] })
				}
			}
		}
		slices.Sort(silent)
	}
	if err := s.pagedOf(ctx, t, n, out); err != nil {
		return nil, nil, err
	}
	return out, silent, nil
}

// suggestions are the latest week's, by rule.
func (s *Server) suggestions(ctx context.Context, l string) (map[string]string, error) {
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil || len(rs) == 0 {
		return map[string]string{}, err
	}
	var ss []verdict.Suggestion
	if err := rs[0].Decode(&ss); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, x := range ss {
		out[x.Rule] = tr(l, `"for" %s → %s: fired %d → %d, still catching %d`, verdict.Duration(x.For), verdict.Duration(x.NewFor),
			x.Before.Firings, x.After.Firings, x.Incidents)
	}
	return out, nil
}

type rulesView struct {
	Weeks  int
	Team   teamFilter
	Lines  []ruleLine
	Silent []string
	// Facts are how many problems of fact what each rule says has, for the
	// rules that never fired as for the rest.
	Facts map[string]int
	// Sort is what the rules are in order of: "" for the noisiest first,
	// refired or deploys; Sorts, those offered.
	Sort  string
	Sorts []string
	// Deploys says the ledger keeps deploys: the firings during them have
	// a column.
	Deploys bool
}

// sortRules puts the rules in order of what by: refired, the most re-fired
// first; deploys, the most firings during deploys first; or else the
// noisiest first, as ruleLines has them.
func sortRules(lines []ruleLine, by string) {
	var key func(ruleLine) int
	switch by {
	case "refired":
		key = func(l ruleLine) int { return l.Refired }
	case "deploys":
		key = func(l ruleLine) int { return l.Deploys }
	default:
		return
	}
	sort.SliceStable(lines, func(a, b int) bool { return key(lines[a]) > key(lines[b]) })
}

// rulesData is the rules page's view, as the API gives it, and what the
// list works the rest out from: the tallies, kept to the team's, the
// owners, the rules a change is proposed to, and the last week's checks
// of the replay, with when one differs.
type rulesData struct {
	rulesView
	t        *tallies
	own      *owners
	proposed map[string]bool
	checks   map[string]ledger.Check
	differs  Differs
}

func (s *Server) rulesPage(r *http.Request) (any, error) {
	d, err := s.rulesOf(r)
	if err != nil {
		return nil, err
	}
	return d.rulesView, nil
}

func (s *Server) rulesOf(r *http.Request) (rulesData, error) {
	n := 4
	switch r.URL.Query().Get("weeks") {
	case "1":
		n = 1
	case "12":
		n = 12
	}
	t, err := s.tallies(r.Context(), weeks)
	if err != nil {
		return rulesData{}, err
	}
	o, err := s.owners(r.Context())
	if err != nil {
		return rulesData{}, err
	}
	f, team := s.teamFilter(o, r.URL.Query().Get("team"))
	if team != nil {
		t.only(o, *team)
	}
	lines, silent, err := s.ruleLines(r.Context(), lang(r), t, n)
	if err != nil {
		return rulesData{}, err
	}
	if team != nil {
		silent = slices.DeleteFunc(silent, func(name string) bool { return o.rule[name] != team.ID })
	}
	s.annotate(o, lines)
	cw, err := s.checksLastWeek(r.Context())
	if err != nil {
		return rulesData{}, err
	}
	d := s.differs()
	proposed, err := s.proposedRules(r.Context())
	if err != nil {
		return rulesData{}, err
	}
	for i := range lines {
		lines[i].Differs = cw[lines[i].Name].Differs(d.N, d.Ratio)
		lines[i].Proposed = proposed[lines[i].Name]
	}
	cf, err := s.contentFindings(r.Context())
	if err != nil {
		return rulesData{}, err
	}
	facts := map[string]int{}
	for name, fs := range cf {
		if c := countOf(fs); c.Facts > 0 {
			facts[name] = c.Facts
		}
	}
	for i := range lines {
		lines[i].Content = countOf(cf[lines[i].Name])
	}
	deploys, err := s.l.HasDeploys(r.Context())
	if err != nil {
		return rulesData{}, err
	}
	v := rulesView{Weeks: n, Team: f, Lines: lines, Silent: silent, Facts: facts, Deploys: deploys, Sorts: []string{"", "refired"}}
	if deploys {
		v.Sorts = append(v.Sorts, "deploys")
	}
	if by := r.URL.Query().Get("sort"); slices.Contains(v.Sorts, by) {
		v.Sort = by
	}
	sortRules(v.Lines, v.Sort)
	return rulesData{v, t, o, proposed, cw, d}, nil
}

// annotate gives rule lines their teams and what is to become of them.
func (s *Server) annotate(o *owners, lines []ruleLine) {
	for i := range lines {
		lines[i].Team = o.teamOf(lines[i].Name)
		lines[i].Status = o.notes[lines[i].Name].Status
	}
}

type change struct {
	Since                   time.Time
	Expr, For, Keep, Labels string
	Gone                    bool
}

type reviewView struct {
	At       time.Time
	From, To time.Time
	Rules    []verdict.RuleChange
	Incs     []verdict.IncidentChange
}

// reviews are the latest change reviews, those naming the rule when one is
// given.
func (s *Server) reviews(ctx context.Context, rule string, limit int) ([]reviewView, error) {
	rs, err := s.l.Results(ctx, "review", 50)
	if err != nil {
		return nil, err
	}
	var out []reviewView
	for _, r := range rs {
		var c verdict.Comparison
		if err := r.Decode(&c); err != nil {
			return nil, err
		}
		if rule != "" && !slices.ContainsFunc(c.Rules, func(rc verdict.RuleChange) bool { return rc.Rule == rule }) {
			continue
		}
		out = append(out, reviewView{r.At, c.From, c.To, c.Rules, c.Incidents})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type ruleView struct {
	ruleLine
	// Unseen says what its replay can't see (rules.Rule.Unseen).
	Unseen  []string
	Changes []change
	Reviews []reviewView
	Caught  []incidentLine
	Latest  []firingLine
	Known   bool
	Note    ledger.Note
	Teams   []ledger.Team
	// TeamID is the team that owns it, 0 for none.
	TeamID   int64
	Statuses []string
	Tasks    []taskLine
	Comments commentsView
	// Check is its replay beside what really fired, when that is checked.
	Check *ruleCheck
	// Says is what it says when it fires, as the rules read last have it,
	// and the problems found in it; nil when it isn't among them.
	Says *saysView
	// The changes proposed to it, and what a form would propose.
	ruleChanges
	// Tie is, for a cloud's alarm read with --alarms and not replayed, the
	// history tied to it, or why none is.
	Tie *tieView
	// Refires are its firings that re-fired, week by week, and its alerts
	// that re-fired most; nil when it hasn't fired.
	Refires *refiresView
}

// refiresView is a rule's re-fires over the twelve weeks shown.
type refiresView struct {
	Within string
	Weeks  []refireWeek // the latest first
	Worst  []ledger.Refired
}

type refireWeek struct {
	Week             string
	Firings, Refired int
	Shared           string
}

// tieView is an alarm's history as its page says it.
type tieView struct {
	Source string
	Since  time.Time
	Why    string
}

// ruleCheck is a rule's replay beside what really fired, as its page shows
// it: over twelve weeks, and over the last week, which says whether it
// differs.
type ruleCheck struct {
	ledger.Check
	Share                  int // percent alike
	OnlyReplayed, OnlyReal int
	Week                   ledger.Check
	Misses                 []ledger.Miss
}

// checksLastWeek are the rules' replays beside what really fired over the
// last week; nil when that isn't checked.
func (s *Server) checksLastWeek(ctx context.Context) (map[string]ledger.Check, error) {
	if !s.checking() {
		return nil, nil
	}
	from, to := s.lastWeek()
	return s.l.Checks(ctx, from, to)
}

func (s *Server) rulePage(r *http.Request) (any, error) {
	ctx := r.Context()
	name := r.PathValue("name")
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	lines, silent, err := s.ruleLines(ctx, lang(r), t, weeks)
	if err != nil {
		return nil, err
	}
	v := ruleView{ruleLine: ruleLine{Name: name, Precision: "–"}}
	if i := slices.IndexFunc(lines, func(l ruleLine) bool { return l.Name == name }); i >= 0 {
		v.ruleLine, v.Known = lines[i], true
	}
	v.Known = v.Known || slices.Contains(silent, name)
	if s.replaying() {
		// Its versions over the twelve weeks shown.
		vs, err := s.versions(ctx, s.l.Midnight(t.weeks[0]), s.now())
		if err != nil {
			return nil, err
		}
		var last *rules.Rule
		for _, ver := range vs {
			i := slices.IndexFunc(ver.Rules, func(x rules.Rule) bool { return x.Name == name })
			var cur *rules.Rule
			if i >= 0 {
				cur = &ver.Rules[i]
				v.Known = true
				if v.Group == "" {
					v.Group = cur.Group
				}
			}
			switch {
			case cur == nil && last != nil:
				v.Changes = append(v.Changes, change{Since: ver.Since, Gone: true})
			case cur != nil && (last == nil || last.Whole() != cur.Whole()):
				// As it fires: its labels with its group's, a change to
				// which is a version.
				c := describe(*cur, ver.Since)
				c.Labels = labelsText(cur.AlertLabels())
				v.Changes = append(v.Changes, c)
			}
			last = cur
		}
		slices.Reverse(v.Changes)
		if last != nil {
			for _, u := range last.Unseen() {
				v.Unseen = append(v.Unseen, tr(lang(r), "Replayed in part: %s.", tr(lang(r), verdict.UnseenText(u))))
			}
		}
	}
	if !v.Known {
		return nil, ledger.ErrNotFound
	}
	if id := t.id(name); id != 0 && v.Firings > 0 {
		v.Refires = &refiresView{Within: verdict.Duration(t.o.RefireWithin)}
		for i := len(t.weeks) - 1; i >= 0; i-- {
			x := t.by[t.weeks[i]][id]
			v.Refires.Weeks = append(v.Refires.Weeks, refireWeek{Week: s.l.Midnight(t.weeks[i]).In(s.cfg.Loc).Format("2006-01-02"),
				Firings: x.Firings, Refired: x.Refired, Shared: pct(x.Refired, x.Firings)})
		}
		if v.Refired > 0 {
			if v.Refires.Worst, err = s.l.RefiredOf(ctx, name, s.l.Midnight(t.weeks[0]), s.now().Add(time.Hour), 5); err != nil {
				return nil, err
			}
		}
	}
	if len(s.alarms) > 0 {
		ties, err := s.l.Ties(ctx)
		if err != nil {
			return nil, err
		}
		if x, ok := ties[name]; ok {
			v.Tie = &tieView{Source: x.Source, Why: x.Why}
			if x.Since > 0 {
				v.Tie.Since = time.Unix(x.Since, 0)
			}
		}
	}
	st, err := s.content(ctx)
	if err != nil {
		return nil, err
	}
	if says, ok := st.says[name]; ok {
		v.Says = saysViewOf(says, st.findings[name], s.cfg.RunbookAnnotations, s.cfg.RunbookBase)
		v.Says.Unchecked = st.unchecked[name]
		v.Content = countOf(st.findings[name])
	}
	if v.Reviews, err = s.reviews(ctx, name, 10); err != nil {
		return nil, err
	}
	caught, err := s.l.CaughtBy(ctx, name, 20)
	if err != nil {
		return nil, err
	}
	for _, in := range caught {
		v.Caught = append(v.Caught, s.incidentLine(lang(r), in, t.state(in, ledger.Week(s.l.Day(in.Start)))))
	}
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: time.Unix(0, 0), To: s.now().Add(time.Hour), Rule: name, Limit: 50})
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		v.Latest = append(v.Latest, s.firingLine(f))
	}
	if err := s.notifiedFirings(ctx, v.Latest); err != nil {
		return nil, err
	}
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	v.Team, v.TeamID, v.Note, v.Teams, v.Statuses = o.teamOf(name), o.rule[name], o.notes[name], o.teams, ledger.Statuses
	v.Status = v.Note.Status
	ts, err := s.l.Tasks(ctx, ledger.TaskQuery{Rule: name, Limit: 50})
	if err != nil {
		return nil, err
	}
	v.Tasks = s.taskLines(o, ts)
	if v.Comments, err = s.commentsView(r, "rule", name); err != nil {
		return nil, err
	}
	if err := s.ruleChangesOf(r, &v); err != nil {
		return nil, err
	}
	if s.checking() {
		all, err := s.l.Checks(ctx, s.l.Midnight(t.weeks[0]), s.now())
		if err != nil {
			return nil, err
		}
		cw, err := s.checksLastWeek(ctx)
		if err != nil {
			return nil, err
		}
		if c, ok := all[name]; ok && c.Either() > 0 {
			d := s.differs()
			v.Check = &ruleCheck{Check: c, Share: int(math.Round(c.Share() * 100)), OnlyReplayed: c.Replayed - c.Alike,
				OnlyReal: c.Real - c.Alike, Week: cw[name]}
			v.Differs = cw[name].Differs(d.N, d.Ratio)
			if v.Check.Misses, err = s.l.CheckMisses(ctx, name); err != nil {
				return nil, err
			}
		}
	}
	return v, nil
}

func describe(r rules.Rule, since time.Time) change {
	c := change{Since: since, For: verdict.Duration(r.For)}
	if r.KeepFiringFor > 0 {
		c.Keep = verdict.Duration(r.KeepFiringFor)
	}
	switch {
	case r.Graph != nil:
		var qs []string
		for _, ref := range slices.Sorted(maps.Keys(r.Graph.Queries)) {
			qs = append(qs, ref+": "+r.Graph.Queries[ref])
		}
		c.Expr = strings.Join(qs, "\n")
	default:
		c.Expr = r.Expr
	}
	c.Labels = labelsText(r.Labels)
	return c
}

// labelsText is a rule's labels as k=v, in order.
func labelsText(l map[string]string) string {
	var ls []string
	for _, k := range slices.Sorted(maps.Keys(l)) {
		ls = append(ls, k+"="+l[k])
	}
	return strings.Join(ls, " ")
}

type incidentLine struct {
	ledger.Incident
	// Status is caught, noisy (only noisy rules caught it) or missed; or
	// why an impact window doesn't count: covered, confirmed, dismissed.
	Status string
	First  string
	Mine   bool // written in the UI, so it can be changed there
	// Lead is, for one caught, how long before its start the first alert
	// of a rule that isn't noisy came, or after; Nearly, for one that
	// wasn't, the rule that nearly caught it, and NearlyMore how many more
	// did.
	Lead       string
	Nearly     string
	NearlyMore int
}

// impactTitle is the title an impact window is found with.
const impactTitle = "user impact (--impact)"

// shown is an incident as shown in a language: an impact window's title is
// the UI's own, so it is translated.
func shown(lang string, in ledger.Incident) ledger.Incident {
	if in.Origin == "impact" && in.Title == impactTitle {
		in.Title = tr(lang, impactTitle)
	}
	return in
}

func (s *Server) incidentLine(lang string, in ledger.Incident, state string) incidentLine {
	in = shown(lang, in)
	l := incidentLine{Incident: in, Status: state, Mine: in.Origin == "ui"}
	if in.Origin == "impact" && !in.Counts {
		switch in.State {
		case "":
			l.Status = "covered"
		default:
			l.Status = in.State
		}
	}
	if len(in.CaughtBy) > 0 {
		c := in.CaughtBy[0]
		l.First = c.Rule + " " + verdict.Delay(c.Delay)
	}
	return l
}

type incidentsView struct {
	Weeks int
	Lines []incidentLine
	Match string
	Now   time.Time
	// Team and Source are what the incidents are picked by: a team's (one
	// of its rules caught it, or it has a value of a label the team owns
	// by), and written down or found by --impact.
	Team   teamFilter
	Source string
	// Judged are those judged against, as Lines picks them: how many each
	// way, and the median lead.
	Judged summaryIncidents
	Lead   string
}

// leadText says how long before an incident's start its first alert came,
// or after.
func leadText(l string, d time.Duration) string {
	if d > 0 {
		return tr(l, "%s before", strings.TrimPrefix(verdict.Delay(d), "+"))
	}
	if d < 0 {
		return tr(l, "%s after", strings.TrimPrefix(verdict.Delay(-d), "+"))
	}
	return tr(l, "at its start")
}

func (s *Server) incidentsPage(r *http.Request) (any, error) {
	ctx := r.Context()
	l := lang(r)
	q := r.URL.Query()
	n := 4
	switch q.Get("weeks") {
	case "1":
		n = 1
	case "12":
		n = 12
	}
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	incs, err := s.l.Incidents(ctx, s.l.Midnight(t.weeks[len(t.weeks)-n]), s.now().Add(24*time.Hour), true)
	if err != nil {
		return nil, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	near, err := s.nearMisses(ctx)
	if err != nil {
		return nil, err
	}
	v := incidentsView{Weeks: n, Match: strings.Join(t.o.Match, ", "), Now: s.now()}
	var team *ledger.Team
	v.Team, team = s.teamFilter(own, q.Get("team"))
	if src := q.Get("source"); src == "impact" || src == "written" {
		v.Source = src
	}
	var leads []time.Duration
	for _, in := range incs {
		if team != nil && !own.ownsIncident(*team, in) {
			continue
		}
		if (v.Source == "impact") != (in.Origin == "impact") && v.Source != "" {
			continue
		}
		w := ledger.Week(s.l.Day(in.Start))
		line := s.incidentLine(l, in, t.state(in, w))
		if in.Counts {
			v.Judged.Total++
			switch line.Status {
			case "caught":
				v.Judged.Caught++
				for _, c := range in.CaughtBy {
					if !t.noisy(w, t.id(c.Rule)) {
						line.Lead = leadText(l, -c.Delay)
						leads = append(leads, -c.Delay)
						break
					}
				}
			case "noisy":
				v.Judged.NoisyOnly++
			default:
				v.Judged.Missed++
			}
			if line.Status != "caught" {
				if nms := near[in.ID]; len(nms) > 0 {
					line.Nearly, line.NearlyMore = nms[0].Rule, len(nms)-1
				}
			}
		}
		v.Lines = append(v.Lines, line)
	}
	if m := medianSeconds(leads); m != nil {
		v.Lead = leadText(l, time.Duration(*m)*time.Second)
	}
	slices.Reverse(v.Lines)
	return v, nil
}

type incidentView struct {
	incidentLine
	Labels   string
	Match    string
	Tasks    []taskLine
	Comments commentsView
	Nearly   []nearLine
}

// nearLine is a rule that nearly caught an incident, as a person reads it.
type nearLine struct {
	Rule, Text, Cost string
}

// nearMisses are the rules that nearly caught each incident, over the
// twelve weeks kept: for each week, as last worked out, by the weekly digest
// or for an incident written down since.
func (s *Server) nearMisses(ctx context.Context) (map[string][]verdict.NearMiss, error) {
	rs, err := s.l.ByPeriod(ctx, "near_misses", s.l.Midnight(ledger.Week(s.l.Day(s.now()))-7*weeks))
	if err != nil {
		return nil, err
	}
	out := map[string][]verdict.NearMiss{}
	seen := map[string]bool{}
	for _, r := range rs { // the latest week first
		var nms []verdict.NearMiss
		if err := r.Decode(&nms); err != nil {
			return nil, err
		}
		got := map[string]bool{}
		for _, nm := range nms {
			if !seen[nm.Incident] {
				out[nm.Incident] = append(out[nm.Incident], nm)
				got[nm.Incident] = true
			}
		}
		for id := range got {
			seen[id] = true
		}
	}
	return out, nil
}

func nearText(l string, nm verdict.NearMiss) nearLine {
	x := nearLine{Rule: nm.Rule, Text: tr(l, `its condition held for %s, from %s, under its "for" of %s: with %s it would have fired at %s`,
		verdict.Duration(nm.Held), verdict.Delay(nm.From), verdict.Duration(nm.For), verdict.Duration(nm.NewFor), verdict.Delay(nm.Delay))}
	switch {
	case nm.Value != nil:
		v := nm.Value
		switch {
		case v.Reached != nil && v.Of == "":
			x.Text = tr(l, `its value came to %s, short of its threshold of %s %s: with %s it would have fired at %s`,
				verdict.Number(*v.Reached), v.Op, verdict.Number(v.Bound), v.Op+" "+verdict.Number(v.New), verdict.Delay(nm.Delay))
		case v.Reached != nil:
			x.Text = tr(l, `the value of %s came to %s, short of %s: with %s it would have fired at %s`,
				v.Of, verdict.Number(*v.Reached), v.Compared(), v.Changed(), verdict.Delay(nm.Delay))
		default:
			x.Text = tr(l, `in place of %s, with %s it would have fired at %s`, v.Compared(), v.Changed(), verdict.Delay(nm.Delay))
		}
	case nm.Late:
		x.Text = tr(l, `its condition held from %s, so with its "for" of %s it fired at %s, after the incident: with %s it would have fired at %s`,
			verdict.Delay(nm.From), verdict.Duration(nm.For), verdict.Delay(nm.Fired), verdict.Duration(nm.NewFor), verdict.Delay(nm.Delay))
	}
	if nm.After != nil {
		x.Cost = tr(l, "That week it would have fired %d times instead of %d, %d of them catching nothing.", nm.After.Firings, nm.Before.Firings, nm.After.Black)
	}
	return x
}

func (s *Server) incidentPage(r *http.Request) (any, error) {
	ctx := r.Context()
	in, err := s.l.IncidentByID(ctx, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	v := incidentView{incidentLine: s.incidentLine(lang(r), in, t.state(in, ledger.Week(s.l.Day(in.Start)))), Labels: labelText(in.Labels), Match: strings.Join(t.o.Match, ", ")}
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	ts, err := s.l.Tasks(ctx, ledger.TaskQuery{Incident: in.ID, Limit: 50})
	if err != nil {
		return nil, err
	}
	v.Tasks = s.taskLines(o, ts)
	if v.Status != "caught" {
		near, err := s.nearMisses(ctx)
		if err != nil {
			return nil, err
		}
		for _, nm := range near[in.ID] {
			v.Nearly = append(v.Nearly, nearText(lang(r), nm))
		}
	}
	v.Comments, err = s.commentsView(r, "incident", in.ID)
	return v, err
}

func labelText(l incident.Labels) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(l)) {
		for _, v := range l[k] {
			out = append(out, k+"="+v)
		}
	}
	return strings.Join(out, " ")
}

type firingLine struct {
	ledger.Firing
	Labels string
	// Notified is how many notifications told of it; Counted, whether they
	// were counted for its rule by the time it started, so that none says
	// it paged no one.
	Notified int
	Counted  bool
}

func (s *Server) firingLine(f ledger.Firing) firingLine {
	var ls []string
	for _, k := range slices.Sorted(maps.Keys(f.Labels)) {
		ls = append(ls, k+"="+f.Labels[k])
	}
	return firingLine{Firing: f, Labels: strings.Join(ls, " ")}
}

type firingsView struct {
	Day, Rule  string
	Black      bool
	Lines      []firingLine
	Next       string
	Prev, Then string // the day before and after
}

func (s *Server) firingsPage(r *http.Request) (any, error) {
	q := r.URL.Query()
	day := s.now().In(s.cfg.Loc)
	if v := q.Get("day"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, s.cfg.Loc)
		if err != nil {
			return nil, bad("day %q: want a date like 2026-11-02", v)
		}
		day = d
	}
	d := s.l.Day(day)
	v := firingsView{Day: s.l.Midnight(d).In(s.cfg.Loc).Format("2006-01-02"), Rule: q.Get("rule"), Black: q.Get("black") != "",
		Prev: s.l.Midnight(d - 1).In(s.cfg.Loc).Format("2006-01-02"), Then: s.l.Midnight(d + 1).In(s.cfg.Loc).Format("2006-01-02")}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	const page = 100
	fs, err := s.l.Firings(r.Context(), ledger.FiringQuery{From: s.l.Midnight(d), To: s.l.Midnight(d + 1), Rule: v.Rule, Black: v.Black, Before: before, Limit: page})
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		v.Lines = append(v.Lines, s.firingLine(f))
	}
	if err := s.notifiedFirings(r.Context(), v.Lines); err != nil {
		return nil, err
	}
	if len(fs) == page {
		nq := url.Values{"day": {v.Day}, "before": {fmt.Sprint(fs[len(fs)-1].ID)}}
		if v.Rule != "" {
			nq.Set("rule", v.Rule)
		}
		if v.Black {
			nq.Set("black", "1")
		}
		v.Next = "/firings?" + nq.Encode()
	}
	return v, nil
}

type settingView struct {
	Key, Flag   string
	CommandLine string
	Changed     bool
	Value       string // in effect
	By          string
	Updated     time.Time
}

type settingsView struct {
	Settings []settingView
	Changes  []ledger.Change
	Try      *tryView
}

func (s *Server) settingsView(ctx context.Context) (settingsView, error) {
	o, err := s.options(ctx)
	if err != nil {
		return settingsView{}, err
	}
	set, err := s.l.Settings(ctx)
	if err != nil {
		return settingsView{}, err
	}
	var v settingsView
	for _, st := range settable {
		x := settingView{Key: st.key, Flag: st.flag, CommandLine: show(s.cfg.Options, st.key), Value: show(o, st.key)}
		if c, ok := set[st.key]; ok {
			x.Changed, x.By, x.Updated = true, c.By, c.Updated
		}
		v.Settings = append(v.Settings, x)
	}
	cs, err := s.l.Changes(ctx, 100)
	if err != nil {
		return v, err
	}
	for _, c := range cs {
		if strings.HasPrefix(c.What, "setting ") && len(v.Changes) < 20 {
			v.Changes = append(v.Changes, c)
		}
	}
	return v, nil
}

func (s *Server) settingsPage(r *http.Request) (any, error) {
	v, err := s.settingsView(r.Context())
	if err != nil {
		return nil, err
	}
	if r.URL.Query().Get("try") == "" {
		return v, nil
	}
	o, err := s.options(r.Context())
	if err != nil {
		return nil, err
	}
	tried := o
	for _, st := range settable {
		if val := strings.TrimSpace(r.URL.Query().Get(st.key)); val != "" {
			if err := apply(&tried, st.key, val); err != nil {
				return nil, bad("%s %s: %v", st.flag, val, err)
			}
		}
	}
	to := s.now()
	done, err := s.heavyTurn(r.Context(), to.Add(-7*24*time.Hour), to)
	if err != nil {
		return nil, err
	}
	defer done()
	if v.Try, err = s.try(r.Context(), o, tried); err != nil {
		return nil, err
	}
	return v, nil
}

type tryView struct {
	From, To time.Time
	Settings []settingView
	Rules    []verdict.RuleChange
	Incs     []tryIncident
	Same     bool
}

type tryIncident struct {
	ID, Title  string
	Start      time.Time
	Was, Would string
}

// try judges the last seven days with other settings, without saving them,
// and sets the result beside the ledger's.
func (s *Server) try(ctx context.Context, o, tried verdict.Options) (*tryView, error) {
	to := s.now()
	from := to.Add(-7 * 24 * time.Hour)
	o.From, o.To = from, to
	tried.From, tried.To = from, to
	now, err := s.l.Report(ctx, from, to, o, false)
	if err != nil {
		return nil, err
	}
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
	if err != nil {
		return nil, err
	}
	plain := make([]firing.Firing, len(fs))
	for i, f := range fs {
		plain[i] = f.Firing
	}
	incs := make([]incident.Incident, len(now.Incidents))
	for i, in := range now.Incidents {
		incs[i] = in.Incident
	}
	would := verdict.Judge(nil, plain, incs, tried)
	v := &tryView{From: from, To: to}
	for _, st := range settable {
		v.Settings = append(v.Settings, settingView{Key: st.key, Flag: st.flag, Value: show(tried, st.key), CommandLine: show(o, st.key)})
	}
	var names []string
	for _, r := range now.Rules {
		names = append(names, r.Rule)
	}
	cmp := verdict.Compare(now, would, names, 0)
	for _, rc := range cmp.Rules {
		b, a := rc.Before, rc.After
		if b == nil || a == nil || b.White != a.White || b.Noisy != a.Noisy || b.Refired != a.Refired {
			v.Rules = append(v.Rules, rc)
		}
	}
	state := func(ir verdict.IncidentResult, noisyRule map[string]bool) string {
		switch {
		case len(ir.CaughtBy) == 0:
			return "missed"
		case slices.ContainsFunc(ir.CaughtBy, func(c verdict.Catch) bool { return !noisyRule[c.Rule] }):
			return "caught"
		}
		return "noisy only"
	}
	nb, na := map[string]bool{}, map[string]bool{}
	for _, r := range now.Rules {
		nb[r.Rule] = r.Noisy
	}
	for _, r := range would.Rules {
		na[r.Rule] = r.Noisy
	}
	for i, in := range now.Incidents {
		if i >= len(would.Incidents) {
			break
		}
		was, wd := state(in, nb), state(would.Incidents[i], na)
		if was != wd {
			v.Incs = append(v.Incs, tryIncident{in.ID, in.Title, in.Start, was, wd})
		}
	}
	v.Same = len(v.Rules) == 0 && len(v.Incs) == 0
	return v, nil
}

type runsView struct {
	Runs    []ledger.Run
	Changes []ledger.Change
	Cursor  time.Time
	Counts  map[string]int
	Took    time.Duration
	// Sources are where firings are copied in from; Copying, that they can
	// be.
	Sources []ledger.Source
	Copying bool
	// Check is how the replay is set beside what really fired.
	Check *runsCheck
	// Stopped are the reports that stopped as their team was removed.
	Stopped []string
	// OnCall are the on-call schedules, as each was last read.
	OnCall []onCallRead
}

// runsCheck is how far the replay has been set beside what really fired,
// the series it is read from, those that held nothing over the last day
// checked, and alerts that really fired that no rule is named, over the
// last week.
type runsCheck struct {
	To     time.Time
	Series []string
	// Alarms are the --alarms sources whose copied history stands in for
	// a series, for the alarms of theirs Lapilli replays.
	Alarms    []string
	Empty     []string
	EmptyDay  time.Time
	Elsewhere []string
}

func (s *Server) runsPage(r *http.Request) (any, error) {
	runs, err := s.l.Runs(r.Context(), 100)
	if err != nil {
		return nil, err
	}
	cs, err := s.l.Changes(r.Context(), 100)
	if err != nil {
		return nil, err
	}
	cs = s.changesSeen(r, cs)
	cursor, err := s.l.Cursor(r.Context())
	if err != nil {
		return nil, err
	}
	srcs, err := s.l.Sources(r.Context())
	if err != nil {
		return nil, err
	}
	s.status.Lock()
	counts := maps.Clone(s.status.runs)
	took := s.status.took
	s.status.Unlock()
	v := runsView{runs, cs, cursor, counts, took, srcs, s.cfg.FiringsToken != "", nil, nil, s.onCallView()}
	if v.Stopped, err = s.stoppedReports(r.Context()); err != nil {
		return nil, err
	}
	if s.checking() {
		c := &runsCheck{Series: s.cfg.CheckSeries}
		if len(c.Series) == 0 {
			c.Series = []string{alertsSeries, grafanaAlertsSeries}
		}
		for _, a := range s.cfg.Alarms {
			c.Alarms = append(c.Alarms, a.Source)
		}
		if c.To, err = s.l.CheckedTo(r.Context()); err != nil {
			return nil, err
		}
		s.status.Lock()
		c.Empty, c.EmptyDay = slices.Clone(s.status.checkEmpty), s.status.checkDay
		s.status.Unlock()
		el, err := s.l.CheckElsewhere(r.Context(), s.now().Add(-checkWeek), s.now())
		if err != nil {
			return nil, err
		}
		c.Elsewhere = slices.Sorted(maps.Keys(el))
		v.Check = c
	}
	return v, nil
}

var funcs = template.FuncMap{
	"static": static,
	"t":      func(msg string, args ...any) string { return tr("en", msg, args...) }, // each language's own, in pagesIn
	"tbdi":   func(msg string, args ...any) template.HTML { return trBDI("en", msg, args...) },
	// shown is a text as a page shows what it can't change: an expression,
	// a ruler's namespace, with what turns text around as U+FFFD.
	"shown": rules.Shown,
	"who":   func(by string) string { return by }, // likewise
	"dur":   verdict.Duration,
	"delay": verdict.Delay,
	"path":  url.PathEscape,
	"join":  strings.Join,
	"at": func(loc *time.Location, t time.Time) string {
		if t.IsZero() {
			return "–"
		}
		return t.In(loc).Format("2006-01-02 15:04")
	},
	"day": func(loc *time.Location, t time.Time) string { return t.In(loc).Format("2006-01-02") },
	"input": func(loc *time.Location, t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(loc).Format("2006-01-02T15:04")
	},
	"pct": func(r *verdict.RuleResult) string {
		if r == nil {
			return "–"
		}
		p, ok := r.Precision()
		if !ok {
			return "–"
		}
		return fmt.Sprintf("%.0f%%", 100*p)
	},
	"fired": func(r *verdict.RuleResult) int {
		if r == nil {
			return 0
		}
		return r.Firings
	},
	"caught": func(r *verdict.RuleResult) int {
		if r == nil {
			return 0
		}
		return r.White
	},
	"catches": func(cs []verdict.Catch) string {
		var out []string
		for _, c := range cs {
			x := c.Rule + " " + verdict.Delay(c.Delay)
			if c.Noisy {
				x += " (noisy)"
			}
			out = append(out, x)
		}
		if len(out) == 0 {
			return "nothing"
		}
		return strings.Join(out, ", ")
	},
	// windowForm is how a window repeats, for its form.
	"windowForm": func(every, on string) struct{ Every, On string } { return struct{ Every, On string }{every, on} },
	"everyText": func(every string) string {
		return map[string]string{"day": "every day", "week": "every week", "month": "every month", "cron": "by a cron expression"}[every]
	},
	"list": func(xs ...string) []string { return xs },
	// pickOf is the teams a form offers, and the one picked.
	"pickOf": func(p teamPick, team int64) struct {
		Pick teamPick
		Team int64
	} {
		return struct {
			Pick teamPick
			Team int64
		}{p, team}
	},
	// windowTeam is a window's team, as the windows show it.
	"windowTeam": func(team int64, names map[int64]string) struct {
		Name    string
		Removed bool
	} {
		return struct {
			Name    string
			Removed bool
		}{names[team], team < 0}
	},
	"teamMap": func(team int64, name string) map[int64]string { return map[int64]string{team: name} },
	// repeats is how a quiet window repeats, for the "repeats" template.
	"repeats": func(w ledger.Window, loc *time.Location) struct{ Every, On, Until string } {
		r := struct{ Every, On, Until string }{Every: w.Every, On: w.On}
		if !w.Until.IsZero() {
			r.Until = w.Until.In(loc).Format("2006-01-02")
		}
		return r
	},
	"statusesBut": func(status string) []string {
		var out []string
		for _, st := range ledger.TaskStatuses {
			if st != status {
				out = append(out, st)
			}
		}
		return out
	},
	"has": func(xs []string, x string) bool { return slices.Contains(xs, x) },
	// edits reports whether the reader may change what is a team's: an
	// editor anything, a team's editor what is their team's.
	"edits": func(st statusView, team int64) bool {
		return !st.ReadOnly || team > 0 && slices.Contains(st.Teams, team)
	},
	// editsAny reports whether the reader may change anything a team has.
	"editsAny": func(st statusView) bool { return !st.ReadOnly || len(st.Teams) > 0 },
	// titled is the page with its title, as its h1 has it.
	"titled": func(p page, title string) page {
		p.Title = title
		return p
	},
	// sparkText is what a rule's sparkline shows, week by week, for those
	// who can't see it.
	"sparkText": func(weeks []spark) string {
		var out []string
		for _, w := range weeks {
			out = append(out, w.Title)
		}
		return strings.Join(out, "; ")
	},
	"kindText": kindText,
	// navOf is the sidebar of a page, as the menu or the sidebar draws it.
	"navOf": func(p page, kind string) struct {
		P    page
		Kind string
	} {
		return struct {
			P    page
			Kind string
		}{p, kind}
	},
	"mainAction": mainAction,
	"emptyOf": func(kind, title, text string) emptyView {
		return emptyView{Kind: kind, Title: title, Text: text}
	},
	// on is what a template given a part of a page draws, with the page it
	// is on, for its links.
	"on": func(p page, v any) struct {
		P page
		V any
	} {
		return struct {
			P page
			V any
		}{p, v}
	},
}
