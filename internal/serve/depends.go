package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
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
// step, the labels and a digest of the expression, written in one form
// (normalExpr), so that its spaces and the order of its matchers don't. Not the depth or the
// direction, which judge what is read and read nothing. A key that changes,
// as another expression, labels or step does, discards the edges the ledger
// holds, which are read again over the last four weeks.
func (c dependsConf) graphKey() string {
	if !c.on() {
		return ""
	}
	h := sha256.Sum256([]byte(normalExpr(c.Expr)))
	return verdict.Duration(c.Step) + " " + c.From + "," + c.To + " #" + hex.EncodeToString(h[:8])
}

// normalExpr is an expression as Lapilli reads PromQL and writes it in one
// form (rules.Rule.Parsed): spaces, comments, the order of matchers and the
// like are not told apart. One that isn't read so is as it was typed, its
// space trimmed.
func normalExpr(expr string) string {
	if p, ok := (rules.Rule{Name: "depends", Expr: expr}).Parsed(); ok {
		return p
	}
	return strings.TrimSpace(expr)
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
	// What the round reads the graph with, by its key (roundDepends).
	s.depends.Lock()
	s.depends.round, s.depends.key = c, c.graphKey()
	s.depends.Unlock()
	return &verdict.Dependencies{Label: label, Depth: c.Depth, Direction: c.Direction, Key: c.graphKey()}, nil
}

// roundDepends is the service graph a round judges by, as its options had it
// (dependsFor), when its key is key: ok false when it isn't, because the
// graph was changed since, which the next round reads again. What a run
// reads and keeps is of the round's graph, never of another a save made
// meanwhile.
func (s *Server) roundDepends(key string) (dependsConf, bool) {
	s.depends.Lock()
	defer s.depends.Unlock()
	return s.depends.round, key != "" && s.depends.key == key
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
// from, as the verdict side reads it, within the time given.
func (s *Server) readGraph(parent context.Context, c dependsConf, from, to time.Time, lead, within time.Duration) (*ledger.EdgesRead, error) {
	ctx, cancel := context.WithTimeout(parent, within)
	defer cancel()
	from = from.Add(-max(lead, time.Hour))
	g, err := graph.Read(ctx, s.h, c.query(), from, to)
	if err != nil {
		return nil, fmt.Errorf("--depends: %w", err)
	}
	if g.TooMany() {
		return nil, errDependsTooMany
	}
	return &ledger.EdgesRead{Graph: g, From: from, To: to, Key: c.graphKey()}, nil
}

// edgesFailed keeps in the ledger that reading the graph failed, and why, for
// the Settings page of any replica, until a read goes well (edgesWell). The
// backoff of a read over the last four weeks it had is kept.
func (s *Server) edgesFailed(ctx context.Context, key string, err error) {
	s.fail("depends", err)
	st, _ := s.l.EdgesStatus(ctx)
	if st.Key != key {
		st = ledger.EdgesStatus{Key: key}
	}
	st.Error, st.At = promText(err), s.now()
	if err := s.l.SetEdgesStatus(context.WithoutCancel(ctx), st); err != nil {
		s.log.Warn("why the service graph couldn't be read wasn't kept", "err", err)
	}
}

// edgesWell clears what was kept of a failed read of the graph with that key:
// it was read. What was kept of another's, a change not read yet, stays.
func (s *Server) edgesWell(ctx context.Context, key string) {
	if st, err := s.l.EdgesStatus(ctx); err == nil && st.Key != "" && st.Key != key {
		return
	}
	s.status.Lock()
	if s.status.errWhat == "depends" {
		s.status.err, s.status.errWhat = "", ""
	}
	s.status.Unlock()
	if err := s.l.SetEdgesStatus(context.WithoutCancel(ctx), ledger.EdgesStatus{}); err != nil {
		s.log.Warn("the service graph was read, and what was kept of a failed read stays", "err", err)
	}
}

// readEdges is the graph a run reads, over its hours, of the graph the
// round judges by (roundDepends): a read that fails is kept as a span the next
// run reads once more (retryEdges), and until then the incidents in it are
// judged by what the ledger holds. Not read while the ledger holds as many
// edges as it keeps (ledger.MaxEdgesKept), and not kept when a day of it has
// more than ledger.MaxEdgesPerDay: the status says so, and the hours are a
// gap in the spans read.
func (s *Server) readEdges(ctx context.Context, o verdict.Options, from, to time.Time) *ledger.EdgesRead {
	if o.Depends == nil {
		return nil
	}
	c, ok := s.roundDepends(o.Depends.Key)
	if !ok || !c.on() {
		return nil
	}
	if have, _, err := s.l.EdgesKept(ctx); err == nil && have >= ledger.MaxEdgesKept {
		s.edgesFailed(ctx, o.Depends.Key, ledger.ErrEdgesKept)
		return nil
	}
	e, err := s.readGraph(ctx, c, from, to, o.MaxLead(), s.cfg.Timeout)
	if err == nil {
		err = s.l.EdgesFit(ctx, e, false)
	}
	if err != nil {
		s.edgesFailed(ctx, o.Depends.Key, err)
		if !errors.Is(err, errDependsTooMany) && !errors.Is(err, ledger.ErrEdgesPerDay) && !errors.Is(err, ledger.ErrEdgesKept) {
			start := from.Add(-max(o.MaxLead(), time.Hour))
			if err := s.l.EdgeFailed(context.WithoutCancel(ctx), start, to); err != nil {
				s.log.Warn("a span whose service graph couldn't be read wasn't kept to be read again", "from", start, "to", to, "err", err)
			}
		}
		return nil
	}
	s.edgesWell(ctx, o.Depends.Key)
	return e
}

// retryEdges reads once more the spans earlier runs couldn't read the graph
// of, and keeps what it finds (judging them again), of the graph the round
// judges by. A span that fails again is kept as a gap, a known hole in the
// graph the Settings page shows, and isn't tried again. Each read is within
// tryWithin, not --timeout, so that a history that fails holds the jobs of
// the replica that leads for as little as it may.
func (s *Server) retryEdges(ctx context.Context, o verdict.Options) {
	if o.Depends == nil {
		return
	}
	spans, err := s.l.EdgeRetries(ctx)
	if err != nil {
		s.log.Warn("the spans whose service graph wasn't read couldn't be listed", "err", err)
		return
	}
	c, ok := s.roundDepends(o.Depends.Key)
	if !ok || !c.on() {
		return
	}
	for _, sp := range spans {
		e, err := s.readGraph(ctx, c, sp[0], sp[1], 0, min(s.cfg.Timeout, tryWithin))
		if err == nil {
			// readGraph reaches an hour back from the span's start, which
			// the span already did.
			e.From = sp[0]
			if err = s.l.EdgesFit(ctx, e, false); err == nil {
				err = s.l.KeepEdges(context.WithoutCancel(ctx), e, o)
			}
		}
		if err != nil {
			s.log.Warn("the service graph read again for a span a run couldn't read it for failed too: it is a known gap, and isn't tried again", "from", sp[0], "to", sp[1], "err", err)
			if err := s.l.EdgeGaveUp(context.WithoutCancel(ctx), sp[0]); err != nil {
				s.log.Warn("a span whose service graph failed to be read again wasn't kept as a gap", "from", sp[0], "err", err)
			}
			continue
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

// dependsAgainPiece is how much of the last four weeks a read reads at once
// (a variable for tests): a week, so that what a read couldn't reach is the weeks it failed over,
// not all four.
var dependsAgainPiece = 7 * 24 * time.Hour

// dependsAgainBackoff is how long after its tries a read over the last four
// weeks that failed isn't tried again: an hour, then two, four, up to a day,
// as impactAgain's.
func dependsAgainBackoff(tries int) time.Duration {
	return min(time.Hour<<min(max(tries-1, 0), 5), 24*time.Hour)
}

// dependsAgain does what a change of the graph asks, at the leader's round,
// before the options are read: the edges the ledger holds were read with
// another expression, labels or step than the one in effect (started with,
// or saved), so they are dropped and the graph is read over the last four
// weeks, a week at a time within tryWithin each, and the ledger judged again
// with it (its key changes what the stones are judged by, linkedText);
// turned off, they are dropped. The weeks that read are kept and those that
// didn't are read once more by the next run, then a known gap. A read that
// reaches none leaves the ledger as it was, judged with no graph, and is
// tried again an hour later, then two, up to a day (ledger.EdgesStatus), so
// that a history that fails isn't asked every round. Before the first run
// there is nothing to read: the runs read it from their start.
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
	var cursor time.Time
	if want != "" {
		if cursor, err = s.l.Cursor(ctx); err != nil {
			s.fail("the ledger", err)
			return
		}
	}
	if cursor.IsZero() {
		if err := s.l.ReplaceEdges(ctx, want, nil); err != nil {
			s.fail("depends", err)
			return
		}
		s.edgesWell(ctx, want)
		return
	}
	now := s.now()
	st, err := s.l.EdgesStatus(ctx)
	if err != nil {
		s.fail("the ledger", err)
		return
	}
	if st.Key == want && now.Before(st.Next) {
		return
	}
	from, err := s.againFrom(ctx, cursor)
	if err != nil {
		s.fail("the ledger", err)
		return
	}
	s.status.set("reading the service graph over the last four weeks")
	within := min(s.cfg.Timeout, tryWithin)
	replaced := false
	var failed [][2]time.Time
	var firstErr error
	pieces := 0
	for a := from; a.Before(cursor); a = a.Add(dependsAgainPiece) {
		b := a.Add(dependsAgainPiece)
		if b.After(cursor) {
			b = cursor
		}
		lead := time.Duration(0)
		if a.Equal(from) {
			lead = s.cfg.Options.MaxLead()
		}
		pieces++
		e, err := s.readGraph(ctx, c, a, b, lead, within)
		if err == nil {
			err = s.l.EdgesFit(ctx, e, !replaced)
		}
		if err == nil && !replaced {
			if err = s.l.ReplaceEdges(ctx, want, e); err == nil {
				replaced = true
			}
		} else if err == nil {
			err = s.l.AddEdges(ctx, e)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if firstErr == nil {
				firstErr = err
			}
			failed = append(failed, [2]time.Time{a, b})
		}
	}
	if !replaced {
		// None read: as it was, and not again before a while.
		tries := 1
		if st.Key == want {
			tries = st.Tries + 1
		}
		s.fail("depends", fmt.Errorf("read again over the last four weeks, and tried again in %s: %w", verdict.Duration(dependsAgainBackoff(tries)), firstErr))
		next := ledger.EdgesStatus{Key: want, Error: promText(firstErr), At: now, Tries: tries, Next: now.Add(dependsAgainBackoff(tries))}
		if err := s.l.SetEdgesStatus(context.WithoutCancel(ctx), next); err != nil {
			s.log.Warn("why the service graph couldn't be read wasn't kept", "err", err)
		}
		return
	}
	for _, sp := range failed {
		if err := s.l.EdgeFailed(context.WithoutCancel(ctx), sp[0], sp[1]); err != nil {
			s.log.Warn("a span whose service graph couldn't be read wasn't kept to be read again", "from", sp[0], "to", sp[1], "err", err)
		}
	}
	if len(failed) > 0 {
		s.edgesFailed(ctx, want, fmt.Errorf("%d of %d parts of the last four weeks couldn't be read, and are read once more by the next run: %w", len(failed), pieces, firstErr))
		return
	}
	s.edgesWell(ctx, want)
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

// noteGraphOver says in the log, once, when a judgement of the run's hours
// was made with no graph, its incidents' windows having more edges than are
// loaded (ledger.MaxJudgeEdges), and once more when it no longer is.
func (s *Server) noteGraphOver(ctx context.Context, from, to time.Time) {
	over, err := s.l.EdgesOver(ctx, from, to)
	if err != nil {
		return
	}
	s.depends.Lock()
	was := s.depends.over
	s.depends.over = over
	s.depends.Unlock()
	switch {
	case over && !was:
		s.log.Warn("the service graph of the incidents' windows has more edges than a judgement loads: the firings of these hours are judged by none, and no catch is by dependency",
			"max", ledger.MaxJudgeEdges, "from", from, "to", to)
	case !over && was:
		s.log.Info("the service graph of the incidents' windows is loaded for a judgement again")
	}
}
