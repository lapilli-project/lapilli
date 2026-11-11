package serve

import (
	"context"
	"slices"
	"sync"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/rules"
)

// What each rule says when it fires, checked (docs/design/content.md): of
// the rules as serve read them last. A change to a summary decides nothing
// about when a rule fires, so it makes no new version: the versions don't
// have what the rules say now.

// contentFinding is a problem with what a rule says, or how it is written.
type contentFinding struct {
	// Kind is the check: one of content.Checks, such as no_runbook.
	Kind string `json:"check"`
	// Class is content.Fact, content.Measured or content.Heuristic.
	Class string `json:"class"`
	// Key is what it says, in English: a key into the catalogs.
	Key string `json:"message"`
	// Detail is what in particular: a label, a severity, another rule, a
	// range, a template's error. From the rules: data, bounded.
	Detail string `json:"detail,omitempty"`
}

// Why is why a heuristic is only a guess, in English, a key into the
// catalogs too; "" for a fact or a measured check.
func (f contentFinding) Why() string { return content.Why(f.Kind) }

// saidRules are the rules as serve read them last, and what was found in
// them.
type saidRules struct {
	sync.Mutex
	rules []rules.Rule // --rule kept
	gen   int64        // counts the reads that found them changed
	// scrape is the scrape intervals Prometheus gave, the global and each
	// job's, asked once a day; a zero global when it gave none.
	scrape   content.Scrapes
	scrapeAt time.Time
	// What was found, and what it was found from.
	at    contentKey
	found *contentState
	// work is held while the check runs, so it runs once at a time: one
	// that waited takes what the one before it found.
	work sync.Mutex
	// checked counts the times the check ran.
	checked int
	// failed are the rules whose check failed, logged once each.
	failed map[string]bool
}

// contentKey is what the content check reads, so that what it found stands
// until one of them changes, and not on every write to the ledger (each
// notification kept is one): the rules as read last and the scrape
// interval; the names the latest version gives them; the firings, and how
// many make a rule noisy; and the hour, as its twelve weeks move on.
type contentKey struct {
	said, version, firings int64
	scrape                 string // content.Scrapes, as a line
	noisyN                 int
	noisyP                 float64
	hour                   int64
}

type contentState struct {
	findings map[string][]contentFinding // by the rule's name in the ledger
	says     map[string]rules.Says
}

// said keeps the rules as read now, for what they say.
func (s *Server) said(rs []rules.Rule) {
	s.saidRules.Lock()
	s.saidRules.rules = rs
	s.saidRules.gen++
	s.saidRules.Unlock()
}

// saidGen changes when what the rules say does: pages show it.
func (s *Server) saidGen() int64 {
	s.saidRules.Lock()
	defer s.saidRules.Unlock()
	return s.saidRules.gen
}

// statusConfig is a history that serves Prometheus's configuration.
type statusConfig interface {
	StatusConfig(ctx context.Context) (string, error)
}

// targetsAPI is a history that serves Prometheus's scrape targets.
type targetsAPI interface {
	Targets(ctx context.Context) ([]byte, error)
}

// askScrape asks Prometheus for its scrape intervals once a day, the
// global and each job's, unless --scrape-interval gives one; where that
// isn't served, the range check is off. Its targets, where they are
// served, say the job label each job's ends up with.
func (s *Server) askScrape(ctx context.Context) {
	sc, ok := s.h.(statusConfig)
	if !s.cfg.CheckContent || s.cfg.ScrapeInterval > 0 || !ok {
		return
	}
	s.saidRules.Lock()
	fresh := !s.saidRules.scrapeAt.IsZero() && s.now().Sub(s.saidRules.scrapeAt) < 24*time.Hour
	s.saidRules.Unlock()
	if fresh {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var got content.Scrapes
	if cfg, err := sc.StatusConfig(ctx); err == nil {
		got, _ = content.ScrapeConfig(cfg)
	} else {
		s.log.Debug("no scrape interval from Prometheus: the range check is off", "err", err)
	}
	if ta, ok := s.h.(targetsAPI); ok && got.Global > 0 {
		if b, err := ta.Targets(ctx); err == nil {
			got.Targets(b)
		} else {
			s.log.Debug("no targets from Prometheus: a job is known by its name", "err", err)
		}
	}
	s.saidRules.Lock()
	if got.String() != s.saidRules.scrape.String() {
		s.saidRules.gen++ // what the pages show changes
	}
	s.saidRules.scrape, s.saidRules.scrapeAt = got, s.now()
	s.saidRules.Unlock()
}

// scrapes are the intervals the range check counts with; a zero global
// when not known.
func (s *Server) scrapes() content.Scrapes {
	if s.cfg.ScrapeInterval > 0 {
		return content.Scrapes{Global: s.cfg.ScrapeInterval}
	}
	s.saidRules.Lock()
	defer s.saidRules.Unlock()
	return s.saidRules.scrape
}

// contentFindings are the problems with what each rule says, or how it is
// written, by the rule's name in the ledger, facts first; empty with
// --check-content=false.
func (s *Server) contentFindings(ctx context.Context) (map[string][]contentFinding, error) {
	st, err := s.content(ctx)
	if err != nil {
		return nil, err
	}
	return st.findings, nil
}

func (s *Server) content(ctx context.Context) (*contentState, error) {
	none := &contentState{findings: map[string][]contentFinding{}, says: map[string]rules.Says{}}
	if !s.cfg.CheckContent {
		return none, nil
	}
	key, rs, st, err := s.contentCached(ctx)
	if err != nil || st != nil {
		return st, err
	}
	if len(rs) == 0 {
		return none, nil
	}
	s.saidRules.work.Lock()
	defer s.saidRules.work.Unlock()
	if key, rs, st, err = s.contentCached(ctx); err != nil || st != nil {
		return st, err
	}
	if len(rs) == 0 {
		return none, nil
	}
	if st, err = s.checkContent(ctx, rs, s.scrapes()); err != nil {
		return nil, err
	}
	s.saidRules.Lock()
	s.saidRules.at, s.saidRules.found = key, st
	s.saidRules.checked++
	s.saidRules.Unlock()
	return st, nil
}

// contentCached is what the content check reads now, the rules as read
// last, and what it found, when it found it from the same.
func (s *Server) contentCached(ctx context.Context) (contentKey, []rules.Rule, *contentState, error) {
	scrape := s.scrapes()
	o, err := s.options(ctx)
	if err != nil {
		return contentKey{}, nil, nil, err
	}
	version, err := s.l.LatestID(ctx)
	if err != nil {
		return contentKey{}, nil, nil, err
	}
	key := contentKey{version: version, firings: s.l.ChangedAny(), scrape: scrape.String(), noisyN: o.NoisyFirings, noisyP: o.NoisyPrecision,
		hour: s.now().Unix() / 3600}
	s.saidRules.Lock()
	defer s.saidRules.Unlock()
	key.said = s.saidRules.gen
	if s.saidRules.found != nil && s.saidRules.at == key {
		return key, nil, s.saidRules.found, nil
	}
	return key, s.saidRules.rules, nil, nil
}

// checkContent checks the rules as read, under the names the ledger
// judges them by, with what the ledger measured.
func (s *Server) checkContent(ctx context.Context, rs []rules.Rule, scrape content.Scrapes) (*contentState, error) {
	named, err := s.sayNames(ctx, rs)
	if err != nil {
		return nil, err
	}
	// What was judged: the labels the alerts had over twelve weeks, and
	// which rules were noisy over four.
	seen, err := s.l.LabelNames(ctx, s.now().AddDate(0, 0, -7*weeks))
	if err != nil {
		return nil, err
	}
	t, err := s.tallies(ctx, 4)
	if err != nil {
		return nil, err
	}
	sums := map[int64][2]int{}
	for _, w := range t.weeks {
		for id, x := range t.by[w] {
			c := sums[id]
			sums[id] = [2]int{c[0] + x.Firings, c[1] + x.White}
		}
	}
	noisyRule := map[string]bool{}
	for id, c := range sums {
		if row, ok := t.rules[id]; ok && row.Source == "" && noisy(c[0], c[1], t.o) {
			noisyRule[row.Name] = true
		}
	}
	fs := content.Check(named, content.Options{Severities: s.cfg.Severities, Scrape: scrape.Global, Jobs: scrape.Jobs,
		Seen: func(rule string) (map[string]bool, bool) {
			m, ok := seen[rule]
			return m, ok
		},
		Noisy:  func(rule string) bool { return noisyRule[rule] },
		Failed: s.contentFailed})
	st := &contentState{findings: map[string][]contentFinding{}, says: map[string]rules.Says{}}
	for i, r := range named {
		st.says[r.Name] = r.Says
		for _, f := range fs[i] {
			cf := contentFinding{Kind: f.Check, Class: f.Class, Key: content.Message(f.Check), Detail: f.Detail}
			if !slices.Contains(st.findings[r.Name], cf) { // a rule read twice, from two files
				st.findings[r.Name] = append(st.findings[r.Name], cf)
			}
		}
	}
	return st, nil
}

// contentFailed logs, once for each rule, that the content check failed on
// it: a fault of the check's, which leaves the rule with no findings.
func (s *Server) contentFailed(rule string, err error) {
	s.saidRules.Lock()
	first := !s.saidRules.failed[rule]
	if first {
		if s.saidRules.failed == nil {
			s.saidRules.failed = map[string]bool{}
		}
		s.saidRules.failed[rule] = true
	}
	s.saidRules.Unlock()
	if first {
		s.log.Error("the content check failed on a rule: it has no findings", "rule", rule, "err", err)
	}
}

// sayNames gives the rules read the names the ledger judges them under, as
// the latest version has them: a rule keeps its name by its group, name,
// labels and uid, which a change to what it says leaves alone. One the
// ledger has no version with yet keeps its own.
func (s *Server) sayNames(ctx context.Context, rs []rules.Rule) ([]rules.Rule, error) {
	latest, ok, err := s.l.Latest(ctx)
	if err != nil || !ok {
		return rs, err
	}
	all, names, err := s.named(ctx, latest.ID)
	if err != nil {
		return nil, err
	}
	had := map[string]string{}
	for i, r := range all {
		if names[i] != "" {
			had[identity(r)] = names[i]
		}
	}
	out := make([]rules.Rule, len(rs))
	for i, r := range rs {
		if n, ok := had[identity(r)]; ok && n != r.Name {
			r.Alert, r.Name = r.AlertName(), n
		}
		out[i] = r
	}
	return out, nil
}

// contentCount is how many problems of each kind a rule has.
type contentCount struct {
	Facts, Measured, Guesses int
}

func countOf(fs []contentFinding) contentCount {
	var c contentCount
	for _, f := range fs {
		switch f.Class {
		case content.Fact:
			c.Facts++
		case content.Measured:
			c.Measured++
		default:
			c.Guesses++
		}
	}
	return c
}

// saysView is what a rule says, as its page shows it.
type saysView struct {
	rules.Says
	// RunbookLink is the runbook when it is a link to follow: an http or
	// https URL with no template in it.
	RunbookLink string
	// Problems are facts and what was measured; Guesses, heuristics.
	Problems, Guesses []contentFinding
}

func saysViewOf(says rules.Says, fs []contentFinding) *saysView {
	v := &saysView{Says: says, RunbookLink: content.Link(says)}
	for _, f := range fs {
		if f.Class == content.Heuristic {
			v.Guesses = append(v.Guesses, f)
		} else {
			v.Problems = append(v.Problems, f)
		}
	}
	return v
}
