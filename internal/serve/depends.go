package serve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The service graph (docs/design/dependencies.md, "In lapilli serve"): an
// expression whose series each name a caller and a callee, read by each run
// for the hours it judges and the lead before them, and kept in the ledger
// (edges), so that judging again asks the history nothing. A firing that
// doesn't catch an incident by its labels catches it by dependency when the
// incident's service calls the firing's. Set where serve is started
// (--depends and what goes with it), or by an admin on the Settings page,
// and tried on the last four weeks first, as --impact is.

// DependsConfig is the service graph serve was started with: the
// expression, the labels of its series that name the caller and the callee,
// the label of the match whose values the services are ("" for the first
// label of the match in effect), how many calls are followed (0 for 1),
// down or both ("" for down), and the step it is read at (0 for an hour).
type DependsConfig struct {
	Expr, From, To, Match string
	Depth                 int
	Direction             string
	Step                  time.Duration
}

// The settings the service graph is, by name, with the flag each stands for.
var dependsSettable = []struct{ key, flag string }{
	{"depends", "--depends"}, {"depends-labels", "--depends-labels"},
	{"depends-depth", "--depends-depth"}, {"depends-direction", "--depends-direction"},
}

// dependsSetting says a setting is one the service graph's.
func dependsSetting(key string) bool {
	return slices.ContainsFunc(dependsSettable, func(c struct{ key, flag string }) bool { return c.key == key })
}

// The bounds of what the UI takes of an expression: those of an impact one.
// The labels are two names of a label each, 128 bytes at most.
const maxDependsLabel = 128

// dependsConf is the service graph in effect: the command line's, with what
// an admin changed on the Settings page.
type dependsConf struct {
	Expr, From, To, Match string
	Depth                 int
	Direction             string
	Step                  time.Duration
}

func (c dependsConf) on() bool { return c.Expr != "" && c.From != "" && c.To != "" }

// graphKey tells apart what the edges the ledger holds were read with: the
// step, the labels and the expression. Not the depth or the direction,
// which judge what is read and read nothing.
func (c dependsConf) graphKey() string {
	if !c.on() {
		return ""
	}
	return verdict.Duration(c.Step) + " " + c.From + "," + c.To + " " + c.Expr
}

// labelOf is the label of the match whose values are the graph's services:
// --depends-match, or the first of the match in effect; "" when that isn't
// among it (the match was changed since), which leaves the graph unused.
func (c dependsConf) labelOf(match []string) string {
	switch {
	case c.Match != "" && slices.Contains(match, c.Match):
		return c.Match
	case c.Match == "" && len(match) > 0:
		return match[0]
	}
	return ""
}

// dependsOf is the service graph in effect, of the settings set. With no
// history to read it from, none.
func (s *Server) dependsOf(set map[string]ledger.Setting) dependsConf {
	c := dependsConf(s.cfg.Depends)
	if v, ok := set["depends"]; ok {
		c.Expr = v.Value
		if v.Value == impactOff {
			c.Expr = ""
		}
	}
	if v, ok := set["depends-labels"]; ok {
		if from, to, ok := splitDependsLabels(v.Value); ok {
			c.From, c.To = from, to
		}
	}
	if v, ok := set["depends-depth"]; ok {
		if n, err := strconv.Atoi(v.Value); err == nil && n >= 1 && n <= graph.MaxDepth {
			c.Depth = n
		}
	}
	if v, ok := set["depends-direction"]; ok && (v.Value == verdict.DependsDown || v.Value == verdict.DependsBoth) {
		c.Direction = v.Value
	}
	if c.Depth < 1 {
		c.Depth = verdict.DependsDefaultDepth
	}
	if c.Direction == "" {
		c.Direction = verdict.DependsDown
	}
	if c.Step < graph.MinStep {
		c.Step = graph.DefaultStep
	}
	if s.h == nil {
		c.Expr = ""
	}
	return c
}

// splitDependsLabels reads "FROM,TO": two different labels.
func splitDependsLabels(v string) (from, to string, ok bool) {
	from, to, ok = strings.Cut(v, ",")
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if !ok || from == "" || to == "" || from == to || strings.Contains(to, ",") || len(from) > maxDependsLabel || len(to) > maxDependsLabel {
		return "", "", false
	}
	return from, to, true
}

// query is how the graph is read.
func (c dependsConf) query() graph.Query {
	return graph.Query{Expr: c.Expr, From: c.From, To: c.To, Step: c.Step}
}

// dependsFor gives the options' service graph, for the settings set and the
// match in effect: none while the graph is off, no label of the match is its
// services', or the ledger holds the edges of another (not yet read again:
// dependsAgain). The graph itself is the ledger's, loaded for the incidents
// judged against (ledger.MaxJudgeEdges).
func (s *Server) dependsFor(ctx context.Context, set map[string]ledger.Setting, match []string) (*verdict.Dependencies, error) {
	c := s.dependsOf(set)
	label := c.labelOf(match)
	if !c.on() || label == "" {
		return nil, nil
	}
	held, err := s.l.EdgesKey(ctx)
	if err != nil || held != c.graphKey() {
		return nil, err
	}
	return &verdict.Dependencies{Label: label, Depth: c.Depth, Direction: c.Direction, Key: c.graphKey()}, nil
}

// dependsKey is what the ledger's stones are judged by of the service graph:
// "" with none, else "~" and a digest in capital letters, which neither "1",
// "m" nor "R" is among, of what it was read with, its label, depth and
// direction. A ledger judged with another judges every firing again.
func dependsKey(o verdict.Options) string {
	d := o.Depends
	if d == nil {
		return ""
	}
	h := sha256.Sum256([]byte(strings.Join([]string{d.Key, d.Label, strconv.Itoa(d.Depth), d.Direction}, "\x00")))
	b := make([]byte, 8)
	for i := range b {
		b[i] = 'A' + h[i]%16
	}
	return "~" + string(b)
}

// errDependsTooMany is a graph with more edges in an hour than are kept.
var errDependsTooMany = fmt.Errorf("the service graph has more than %d edges in an hour: none of it is kept, and no firing is judged by it. Narrow --depends to the calls between services (sum by the two labels)", graph.MaxEdgesPerHour)

// readGraph reads the graph c says over [from, to], with the lead before
// from, as the verdict side reads it, within --timeout.
func (s *Server) readGraph(parent context.Context, c dependsConf, from, to time.Time, lead time.Duration) (*ledger.EdgesRead, error) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
	defer cancel()
	from = from.Add(-max(lead, time.Hour))
	g, err := graph.Read(ctx, s.h, c.query(), from, to)
	if err != nil {
		return nil, fmt.Errorf("--depends: %w", err)
	}
	if g.TooMany() {
		return nil, errDependsTooMany
	}
	return &ledger.EdgesRead{Graph: g, From: from, To: to}, nil
}

// readEdges is the graph a run reads, over its hours: a read that fails is
// kept as a span the next run reads once more (retryEdges), and until then
// the incidents in it are judged by what the ledger holds.
func (s *Server) readEdges(ctx context.Context, o verdict.Options, from, to time.Time) *ledger.EdgesRead {
	c, err := s.currentDepends(ctx)
	if err != nil || !c.on() || o.Depends == nil {
		return nil
	}
	e, err := s.readGraph(ctx, c, from, to, o.MaxLead())
	if err != nil {
		s.fail("depends", err)
		if !errors.Is(err, errDependsTooMany) {
			start := from.Add(-max(o.MaxLead(), time.Hour))
			if err := s.l.EdgeFailed(context.WithoutCancel(ctx), start, to); err != nil {
				s.log.Warn("a span whose service graph couldn't be read wasn't kept to be read again", "from", start, "to", to, "err", err)
			}
		}
		return nil
	}
	return e
}

// retryEdges reads once more the spans earlier runs couldn't read the graph
// of, and keeps what it finds (judging them again). A span that fails again
// is let go: it is tried once.
func (s *Server) retryEdges(ctx context.Context, o verdict.Options) {
	if o.Depends == nil {
		return
	}
	spans, err := s.l.EdgeRetries(ctx)
	if err != nil {
		s.log.Warn("the spans whose service graph wasn't read couldn't be listed", "err", err)
		return
	}
	c, err := s.currentDepends(ctx)
	if err != nil || !c.on() {
		return
	}
	for _, sp := range spans {
		e, err := s.readGraph(ctx, c, sp[0], sp[1], 0)
		if err == nil {
			// readGraph reaches an hour back from the span's start, which
			// the span already did.
			e.From = sp[0]
			err = s.l.KeepEdges(context.WithoutCancel(ctx), e, o)
		}
		if err != nil {
			s.log.Warn("the service graph read again for a span a run couldn't read it for failed too, and isn't tried again", "from", sp[0], "to", sp[1], "err", err)
		}
		if err := s.l.EdgeRetried(context.WithoutCancel(ctx), sp[0]); err != nil {
			s.log.Warn("a span whose service graph was read again wasn't dropped", "from", sp[0], "err", err)
		}
	}
}

// currentDepends is the service graph in effect now.
func (s *Server) currentDepends(ctx context.Context) (dependsConf, error) {
	set, err := s.l.Settings(ctx)
	if err != nil {
		return dependsConf{}, err
	}
	return s.dependsOf(set), nil
}

// dependsAgain does what a change of the graph asks, at the leader's round,
// before the options are read: the edges the ledger holds were read with
// another expression, labels or step than the one in effect (started with,
// or saved), so they are dropped and the graph is read over the last four
// weeks, and the ledger judged again with it (its key changes what the
// stones are judged by, linkedText); turned off, they are dropped. A read
// that fails leaves the ledger as it was, judged with no graph, and is tried
// again at the next round. Before the first run there is nothing to read:
// the runs read it from their start.
func (s *Server) dependsAgain(ctx context.Context) {
	c, err := s.currentDepends(ctx)
	if err != nil {
		s.fail("settings", err)
		return
	}
	want := c.graphKey()
	held, err := s.l.EdgesKey(ctx)
	if err != nil {
		s.fail("the ledger", err)
		return
	}
	if held == want {
		return
	}
	var e *ledger.EdgesRead
	if want != "" {
		cursor, err := s.l.Cursor(ctx)
		if err != nil {
			s.fail("the ledger", err)
			return
		}
		if !cursor.IsZero() {
			from, err := s.againFrom(ctx, cursor)
			if err != nil {
				s.fail("the ledger", err)
				return
			}
			s.status.set("reading the service graph over the last four weeks")
			if e, err = s.readGraph(ctx, c, from, cursor, s.cfg.Options.MaxLead()); err != nil {
				s.fail("depends", fmt.Errorf("read again over the last four weeks, and tried again at the next round: %w", err))
				return
			}
		}
	}
	if err := s.l.ReplaceEdges(ctx, want, e); err != nil {
		s.fail("depends", err)
	}
}

// dependsChanged says the Settings page changed any of the service graph's
// settings.
func dependsChanged(set map[string]ledger.Setting) bool {
	for k := range set {
		if dependsSetting(k) {
			return true
		}
	}
	return false
}

// graphed is the options with the graph the ledger holds for the incidents
// to be judged against, for a judgement made here with verdict.Judge: those
// the ledger judges have it from the ledger. When the graph can't be loaded,
// the options as they were: no catch by dependency.
func (s *Server) graphed(ctx context.Context, o verdict.Options, incs []incident.Incident) verdict.Options {
	if o.Depends == nil || o.Depends.Graph != nil {
		return o
	}
	g, err := s.l.WithGraph(ctx, o, incs)
	if err != nil {
		s.log.Warn("the service graph couldn't be loaded for a judgement: it is made with none", "err", err)
		return o
	}
	return g
}
