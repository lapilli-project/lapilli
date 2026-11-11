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
	// scrape is the scrape interval Prometheus gave, asked once a day;
	// 0 when it gave none.
	scrape   time.Duration
	scrapeAt time.Time
	// What was found, at a generation of the rules, of the ledger, and a
	// scrape interval.
	at    [3]int64
	found *contentState
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

// askScrape asks Prometheus for its scrape interval once a day, unless
// --scrape-interval gives it; where that isn't served, the range check is
// off.
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
	var d time.Duration
	if cfg, err := sc.StatusConfig(ctx); err == nil {
		d, _ = content.ScrapeInterval(cfg)
	} else {
		s.log.Debug("no scrape interval from Prometheus: the range check is off", "err", err)
	}
	s.saidRules.Lock()
	if d != s.saidRules.scrape {
		s.saidRules.gen++ // what the pages show changes
	}
	s.saidRules.scrape, s.saidRules.scrapeAt = d, s.now()
	s.saidRules.Unlock()
}

// scrapeInterval is the one the range check counts with; 0 when not known.
func (s *Server) scrapeInterval() time.Duration {
	if s.cfg.ScrapeInterval > 0 {
		return s.cfg.ScrapeInterval
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
	scrape := s.scrapeInterval()
	s.saidRules.Lock()
	rs, at := s.saidRules.rules, [3]int64{s.saidRules.gen, s.l.Gen(), int64(scrape)}
	if s.saidRules.found != nil && s.saidRules.at == at {
		st := s.saidRules.found
		s.saidRules.Unlock()
		return st, nil
	}
	s.saidRules.Unlock()
	if len(rs) == 0 {
		return none, nil
	}
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
	fs := content.Check(named, content.Options{Severities: s.cfg.Severities, Scrape: scrape,
		Seen: func(rule string) (map[string]bool, bool) {
			m, ok := seen[rule]
			return m, ok
		},
		Noisy: func(rule string) bool { return noisyRule[rule] }})
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
	s.saidRules.Lock()
	s.saidRules.at, s.saidRules.found = at, st
	s.saidRules.Unlock()
	return st, nil
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
		if n, ok := had[identity(r)]; ok {
			r.Name = n
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
