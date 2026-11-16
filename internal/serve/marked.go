package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// Marked windows (docs/design/marked.md): named kinds of window (a sale, a
// launch, a campaign) that mark the firings and incidents during them and
// excuse nothing, so every count can be read inside each kind and outside
// it. They come from the marked file (--marked), --config, the UI and the
// API, and PromQL expressions (--mark KIND=PROMQL).

// Mark is an expression that returns something while a window of Kind is
// on (--mark KIND=PROMQL): a business exporter's gauge, a feature flag's
// metric. Its windows mark everything, whatever labels its series carry,
// but those Match names (--mark-match KIND=LABEL,...), which each series'
// windows keep.
type Mark struct {
	Kind, Expr string
	Match      []string
}

// Key is what its windows are kept with: a hash of the expression and the
// labels it keeps. Changed, the expression is read again over the last
// four weeks, and the windows it found before are replaced.
func (m Mark) Key() string { return ledger.MarkKey(m.Expr, m.Match) }

// markedFileEntry is a window as the marked file writes it.
type markedFileEntry struct {
	Kind   string            `yaml:"kind"`
	Title  string            `yaml:"title"`
	Start  time.Time         `yaml:"start"`
	End    time.Time         `yaml:"end"`
	Every  string            `yaml:"every"`
	On     string            `yaml:"on"`
	Until  string            `yaml:"until"`
	Labels map[string]string `yaml:"labels"`
	Team   string            `yaml:"team"`
}

// readMarkedFile reads the marked file's windows, as the ledger keeps them
// but for their teams, named:
//
//	marked:
//	  - kind: sale
//	    title: Autumn sale
//	    start: 2026-11-20T00:00:00+09:00
//	    end: 2026-11-23T00:00:00+09:00
//	  - kind: launch
//	    title: Search, new ranking
//	    start: 2026-11-25T10:00:00+09:00
//	    end: 2026-11-25T18:00:00+09:00
//	    labels: {service: search}
//	    team: search
//	  - kind: sale
//	    title: Friday flash sale
//	    start: 2026-11-06T20:00:00+09:00
//	    end: 2026-11-06T22:00:00+09:00
//	    every: week
//	    until: 2027-03-31
//
// A file with no marked key is refused: a job that failed leaves an empty
// file, and the file owns what it holds. A key it doesn't know is refused:
// lables: would otherwise mark everything.
func readMarkedFile(b []byte, path string) ([]markedFileEntry, error) {
	var raw struct {
		Marked yaml.Node `yaml:"marked"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if raw.Marked.Kind == 0 {
		return nil, fmt.Errorf("%s: no marked key: want marked:, with a list under it, an empty one if there are none", path)
	}
	// Strictly: each entry's keys are the file's.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(&raw.Marked); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	var es []markedFileEntry
	if err := dec.Decode(&es); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(es) > ledger.MaxMarkedFile {
		return nil, fmt.Errorf("%s: over %d windows", path, ledger.MaxMarkedFile)
	}
	return es, nil
}

// markedOf are the file's windows, their teams found by name and their
// days read in the ledger's zone.
func (s *Server) markedOf(ctx context.Context, es []markedFileEntry, path string) ([]ledger.Marked, error) {
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.Marked, 0, len(es))
	for i, e := range es {
		m := ledger.Marked{Kind: e.Kind, Title: e.Title, Start: e.Start, End: e.End, Every: e.Every, On: e.On, Labels: e.Labels}
		if e.Until != "" {
			day, err := dateDay(e.Until)
			if err != nil {
				return nil, fmt.Errorf("%s: window %d: until %q: want a day like 2027-03-31", path, i+1, e.Until)
			}
			m.Until = s.l.Midnight(day)
		}
		if e.Team != "" {
			t, ok := o.team(e.Team)
			if !ok {
				return nil, fmt.Errorf("%s: window %d: there is no team called %q", path, i+1, e.Team)
			}
			m.TeamID = t.ID
		}
		out = append(out, m)
	}
	return out, nil
}

// syncMarked reads the marked file again, and keeps what it holds: when it
// is as it was, nothing more is read or written.
func (s *Server) syncMarked(ctx context.Context, o verdict.Options) error {
	b, err := readRegular(s.cfg.Marked)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	read := ledger.MarkedRead{At: s.now(), Sum: hex.EncodeToString(sum[:])}
	unchanged, err := s.l.MarkedUnchanged(ctx, read)
	if err != nil {
		return err
	}
	if !unchanged {
		es, err := readMarkedFile(b, s.cfg.Marked)
		if err != nil {
			return err
		}
		ms, err := s.markedOf(ctx, es, s.cfg.Marked)
		if err != nil {
			return err
		}
		changed, err := s.l.SyncMarked(ctx, ms, o, read)
		if err != nil {
			return fmt.Errorf("%s: %w", s.cfg.Marked, err)
		}
		if changed {
			s.log.Info("marked file read again", "windows", len(ms))
		}
	}
	s.markedRead.Store(read.At.Unix())
	return nil
}

// FromMarks are the windows an expression's series make: each series on
// its own, its samples, gaps of two steps or fewer not splitting them
// (incident.FromImpact's rule), a window from the first to a step after
// the last, of any length. They aren't merged across series: a mark has
// no labels to lose, and two series on at once are two windows, which mark
// the same firings once. Each keeps the labels of its series match names,
// as an alert's are kept (verdict.KeptLabels), none by default: a
// window that marks everything.
func FromMarks(series []prom.Series, step time.Duration, kind string, match []string) []ledger.Marked {
	var out []ledger.Marked
	for _, s := range series {
		ts := make([]time.Time, 0, len(s.Samples))
		for _, smp := range s.Samples {
			ts = append(ts, smp.T)
		}
		slices.SortFunc(ts, time.Time.Compare)
		var labels map[string]string
		for _, k := range match {
			if v, ok := s.Labels[k]; ok && v != "" {
				if labels == nil {
					labels = map[string]string{}
				}
				labels[k] = v
			}
		}
		if labels != nil {
			labels = verdict.KeptLabels(labels)
		}
		for i := 0; i < len(ts); {
			start, last := ts[i], ts[i]
			for i++; i < len(ts) && ts[i].Sub(last) <= 2*step; i++ {
				last = ts[i]
			}
			out = append(out, ledger.Marked{Kind: kind, Title: kind, Start: start, End: last.Add(step), Labels: labels})
		}
	}
	slices.SortStableFunc(out, func(a, b ledger.Marked) int { return a.Start.Compare(b.Start) })
	return out
}

// readMarks asks each expression for its windows over [since, to], with
// time of its own: one that fails is missed, its windows kept as they were,
// and asked again by the next run; it holds up nothing else. again reads
// each from since whatever its key.
func (s *Server) readMarks(parent context.Context, marks []Mark, since, to time.Time, again bool) []ledger.MarkRead {
	var out []ledger.MarkRead
	for _, m := range marks {
		ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
		series, err := s.h.QueryRange(ctx, m.Expr, since, to, s.cfg.Step)
		cancel()
		if err != nil {
			if parent.Err() == nil {
				s.fail("reading a marked window's expression", fmt.Errorf("--mark %s: %w", m.Kind, err))
			}
			continue
		}
		ws := FromMarks(series, s.cfg.Step, m.Kind, m.Match)
		out = append(out, ledger.MarkRead{Kind: m.Kind, Key: m.Key(), Since: since, To: to, Step: s.cfg.Step, Again: again, Windows: ws})
	}
	return out
}

// keepMarks keeps what the expressions found, and says what it dropped,
// past the bounds.
func (s *Server) keepMarks(ctx context.Context, reads []ledger.MarkRead, o verdict.Options) error {
	if len(reads) == 0 {
		return nil
	}
	said, err := s.l.KeepMarksSaid(ctx, reads, o)
	if err != nil {
		return err
	}
	if said.Dropped > 0 {
		s.log.Warn("windows the expressions (--mark) found past the bounds on marked windows were dropped for this read", "dropped", said.Dropped,
			"bounds", fmt.Sprintf("%d occurrences of a kind in a week, %d in all, %d a read", ledger.MarkedKindWeek, ledger.MarkedWeek, ledger.MaxMarkedExpr))
	}
	if len(said.Kinds) > 0 {
		s.log.Warn("the reads of these expressions (--mark) weren't kept: changed, they wait to be read again over the last four weeks; or their kind is past the 15 the ledger keeps",
			"kinds", strings.Join(said.Kinds, ","))
	}
	return nil
}

// forgetMarks lets go, once, of the keys of the expressions no longer
// given, every one when no --mark is: their windows stay as found, and a
// kind nothing names any more is let go with them.
func (s *Server) forgetMarks(ctx context.Context) error {
	if !s.forgotMarks.CompareAndSwap(false, true) {
		return nil
	}
	var kinds []string
	for _, m := range s.cfg.Marks {
		kinds = append(kinds, m.Kind)
	}
	if err := s.l.ForgetMarks(ctx, kinds); err != nil {
		s.forgotMarks.Store(false)
		return err
	}
	return nil
}

// marksAgain reads again, over the last four weeks, each expression whose
// key isn't the one its windows were kept with: its windows are replaced,
// and its key kept. One whose read fails is tried again by the next round;
// until then the runs' reads of it wait (ledger.KeepMarks). Once a ledger
// has a cursor: before, the first runs read it.
func (s *Server) marksAgain(ctx context.Context, o verdict.Options) error {
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.IsZero() {
		return err
	}
	var stale []Mark
	for _, m := range s.cfg.Marks {
		k, err := s.l.MarkKeyOf(ctx, m.Kind)
		if err != nil {
			return err
		}
		if k != m.Key() {
			stale = append(stale, m)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	s.status.set("reading the marked windows' expressions again over the last four weeks")
	return s.keepMarks(ctx, s.readMarks(ctx, stale, cursor.Add(-28*24*time.Hour), cursor, true), o)
}

// markedMetrics are the marked windows' series of /metrics, the last seven
// days': each rule's firings marked with each kind, and those that caught
// something, a series for a rule and kind with firings. Deploys are no
// kind: lapilli_rule_firings_during_deploys says theirs.
func (s *Server) markedMetrics(ctx context.Context, b *bytes.Buffer, rep *verdict.Report, today int64) error {
	ms, err := s.l.MarkedDays(ctx, today-6, today)
	if err != nil || len(ms) == 0 {
		return err
	}
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		return err
	}
	byName := map[string]verdict.RuleResult{}
	for _, rr := range rep.Rules {
		byName[rr.Rule] = rr
	}
	type key struct {
		rule int64
		kind string
	}
	sum := map[key][2]int{}
	for _, x := range ms {
		k := key{x.Rule, x.Kind}
		c := sum[k]
		sum[k] = [2]int{c[0] + x.Firings, c[1] + x.White}
	}
	keys := slices.Collect(maps.Keys(sum))
	slices.SortFunc(keys, func(a, b key) int {
		return strings.Compare(table[a.rule].Name+"\x00"+a.kind, table[b.rule].Name+"\x00"+b.kind)
	})
	series := func(k key) string {
		rr, ok := byName[table[k.rule].Name]
		if !ok {
			rr = verdict.RuleResult{Rule: table[k.rule].Name, Severity: table[k.rule].Severity}
		}
		return ruleSeries(rr) + fmt.Sprintf(`,kind="%s"`, labelValue(k.kind))
	}
	b.WriteString("# HELP lapilli_rule_firings_marked Firings judged that came during a marked window of the kind (a sale, a launch): in lapilli_rule_firings too. Deploys are lapilli_rule_firings_during_deploys.\n# TYPE lapilli_rule_firings_marked gauge\n")
	for _, k := range keys {
		fmt.Fprintf(b, "lapilli_rule_firings_marked{%s} %d\n", series(k), sum[k][0])
	}
	b.WriteString("# HELP lapilli_rule_caught_marked Of the firings during marked windows of the kind, those that caught an incident: in lapilli_rule_caught too.\n# TYPE lapilli_rule_caught_marked gauge\n")
	for _, k := range keys {
		fmt.Fprintf(b, "lapilli_rule_caught_marked{%s} %d\n", series(k), sum[k][1])
	}
	return nil
}

// markedOnMetrics are, apart from the pages' metrics, as they change with
// the time and not the ledger: each kind, 1 while an occurrence of it is
// on and 0 otherwise, for a dashboard to shade by; and when the marked
// file was last read.
func (s *Server) markedOnMetrics(ctx context.Context) []byte {
	var b bytes.Buffer
	kinds, err := s.l.MarkedKinds(ctx)
	if err == nil && len(kinds) > 0 {
		now := s.now()
		ms, err := s.l.MarkedMeeting(ctx, now, now)
		if err == nil {
			on := map[string]bool{}
			for _, m := range ms {
				if m.TeamID >= 0 && m.Covers(now) {
					on[m.Kind] = true
				}
			}
			b.WriteString("# HELP lapilli_marked_on Whether an occurrence of a marked window of the kind is on: 1 or 0.\n# TYPE lapilli_marked_on gauge\n")
			for _, k := range kinds {
				n := 0
				if on[k] {
					n = 1
				}
				fmt.Fprintf(&b, "lapilli_marked_on{kind=\"%s\"} %d\n", labelValue(k), n)
			}
		}
	}
	if s.cfg.Marked != "" {
		if at := s.markedRead.Load(); at > 0 {
			fmt.Fprintf(&b, "# HELP lapilli_serve_marked_read_timestamp_seconds When the marked file (--marked) was last read, and what it held kept, in Unix seconds.\n# TYPE lapilli_serve_marked_read_timestamp_seconds gauge\nlapilli_serve_marked_read_timestamp_seconds %d\n", at)
		}
	}
	return b.Bytes()
}

// ParseMarks reads --mark KIND=PROMQL and --mark-match KIND=LABEL,...
// as given, each kind once, eight expressions at most; a --mark-match of
// a kind no --mark names is refused.
func ParseMarks(marks, matches []string) ([]Mark, error) {
	var out []Mark
	for _, v := range marks {
		kind, expr, ok := strings.Cut(v, "=")
		kind, expr = strings.TrimSpace(kind), strings.TrimSpace(expr)
		if !ok || expr == "" {
			return nil, fmt.Errorf("--mark %q: want KIND=PROMQL", v)
		}
		if err := ledger.CheckKind(kind); err != nil {
			return nil, fmt.Errorf("--mark %q: %w", v, err)
		}
		if slices.ContainsFunc(out, func(m Mark) bool { return m.Kind == kind }) {
			return nil, fmt.Errorf("--mark: %s is given twice: one expression a kind", kind)
		}
		out = append(out, Mark{Kind: kind, Expr: expr})
	}
	if len(out) > ledger.MaxMarkExprs {
		return nil, fmt.Errorf("--mark is given %d times at most", ledger.MaxMarkExprs)
	}
	for _, v := range matches {
		kind, labels, ok := strings.Cut(v, "=")
		i := slices.IndexFunc(out, func(m Mark) bool { return m.Kind == strings.TrimSpace(kind) })
		if !ok || i < 0 {
			return nil, fmt.Errorf("--mark-match %q: want KIND=LABEL,..., KIND one --mark gives", v)
		}
		if out[i].Match != nil {
			return nil, fmt.Errorf("--mark-match: %s is given twice", out[i].Kind)
		}
		for _, l := range strings.Split(labels, ",") {
			if l = strings.TrimSpace(l); l != "" && !slices.Contains(out[i].Match, l) {
				if !labelName.MatchString(l) || len(l) > 128 {
					return nil, fmt.Errorf("--mark-match %q: %q isn't a label's name", v, l)
				}
				out[i].Match = append(out[i].Match, l)
			}
		}
		if len(out[i].Match) == 0 || len(out[i].Match) > 16 {
			return nil, fmt.Errorf("--mark-match %q: name from 1 to 16 labels", v)
		}
		slices.Sort(out[i].Match)
	}
	return out, nil
}
