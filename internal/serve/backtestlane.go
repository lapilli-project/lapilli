package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// lane runs the backtests queued, the oldest first, one at a time, while
// this replica leads: beside the worker and not in it, so a run waits for
// no backtest, and a backtest for no run. What it asks of the history is
// counted apart from the runs' (asked), and goes through its own Limiter,
// at its share of --max-rate.
func (s *Server) lane(ctx context.Context) {
	p := s.btPort
	ctx = replay.Tracking(prom.Counting(ctx, &p.asked), &p.progress)
	t := time.NewTicker(backtestPoll)
	defer t.Stop()
	for ctx.Err() == nil {
		b, body, ok, err := s.l.StartBacktest(ctx, s.now())
		switch {
		case ok && err != nil:
			s.log.Error("backtest lane: reading a backtest's request", "id", b.ID, "err", err)
			s.endBacktest(ctx, b, ledger.BacktestFailed, "its request couldn't be read back", 0, nil, s.now())
			continue
		case ok:
			s.runBacktest(ctx, b, body)
			continue
		case err != nil && ctx.Err() == nil && !errors.Is(err, ledger.ErrNotLeading):
			s.log.Error("backtest lane: taking a backtest", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.wake:
		}
	}
}

// laneHook is called as the lane runs each backtest: nil but in tests.
var laneHook func(ledger.Backtest)

// trustNamed is the entry of --backtest-trust named so, nil for none.
func (s *Server) trustNamed(name string) *trusted {
	for _, t := range s.btPort.trust {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// runBacktest runs a backtest taken: its request read again, replayed
// over its period with serve's access, judged by serve's record, and kept
// as the Markdown and JSON lapilli verdict --compare --changed-only would
// write, made fit to post where anyone may read it. One that the job
// cancelled, or that ran out of --backtest-timeout, ends so; one this
// replica stopped leading during is left to the next leader, which runs it
// again from its start.
func (s *Server) runBacktest(ctx context.Context, b ledger.Backtest, body []byte) {
	p := s.btPort
	p.busy.Store(true)
	defer p.busy.Store(false)
	start := s.now()
	base := p.asked.Requests.Load()
	made := func() int { return int(p.asked.Requests.Load() - base) }
	fail := func(why string) { s.endBacktest(ctx, b, ledger.BacktestFailed, why, made(), nil, start) }
	// What a request makes the lane do that it didn't foresee ends that
	// backtest, not the leader.
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("backtest lane: a backtest stopped", "id", b.ID, "panic", fmt.Sprintf("%T", r))
			fail("lapilli serve couldn't run it")
		}
	}()
	if laneHook != nil {
		laneHook(b)
	}
	var req backtestRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fail("its request couldn't be read back")
		return
	}
	t := s.trustNamed(b.Trust)
	if t == nil {
		fail("its entry is no longer in --backtest-trust")
		return
	}
	// Held to what the rules in effect read now, which may have changed
	// since it was queued.
	eff, err := s.readsInEffect(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		return // no longer leading: the next leader runs it again
	case err != nil:
		s.log.Error("backtest lane: reading the rules in effect", "err", err)
		fail("lapilli serve's rules in effect couldn't be read: try again")
		return
	}
	pr, why := s.prepare(req, t.Trust, eff)
	if why != "" {
		fail(why)
		return
	}
	// The Lokis it asks keep the lane's share while it runs, as
	// --prometheus does.
	asking := p.pacesOf(pr.gone, pr.came)
	p.asking.Store(&asking)
	defer p.asking.Store(nil)
	o, err := s.options(ctx)
	if err != nil {
		s.log.Error("backtest lane: reading the settings", "err", err)
		fail("lapilli serve's settings couldn't be read")
		return
	}
	from, to := s.backtestPeriod(pr.days)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var cancelled atomic.Bool
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		poll := time.NewTicker(backtestPoll)
		defer poll.Stop()
		told := s.now()
		for {
			select {
			case <-rctx.Done():
				return
			case <-poll.C:
			}
			// What it asked, kept every backtestTold, and whether its job
			// cancelled it, looked at each poll.
			n := -1
			if s.now().Sub(told) >= backtestTold {
				n, told = made(), s.now()
			}
			c, err := s.backtestLooked(ctx, b.ID, n)
			if err == nil && c {
				cancelled.Store(true)
				cancel()
				return
			}
		}
	}()
	cmp, err := s.backtestCompared(rctx, pr, from, to, o)
	// Held to the incidents, the record of the rules left alone read, and
	// those it can't settle replayed, while the job can still cancel it.
	var held *leftAloneHeld
	if err == nil && pr.failMissed {
		// Within what is left of --backtest-timeout, not another of it.
		held, err = s.holdPrepared(rctx, cmp, pr, from, to, o, s.backtestTimeout()-s.now().Sub(start))
	}
	cancel()
	<-watching
	switch {
	case cancelled.Load():
		s.endBacktest(ctx, b, ledger.BacktestCancelled, "", made(), nil, start)
		return
	case ctx.Err() != nil:
		return // no longer leading: the next leader runs it again
	case err != nil:
		s.log.Error("backtest lane: replaying", "id", b.ID, "err", err)
		fail("lapilli serve couldn't read its record: try again")
		return
	}
	for _, rc := range cmp.Rules {
		for _, r := range []*verdict.RuleResult{rc.Before, rc.After} {
			if r != nil && r.Unsupported == rules.Unfinished {
				fail(fmt.Sprintf("it didn't finish within --backtest-timeout %s: give fewer days, or fewer rules", verdict.Duration(s.backtestTimeout())))
				return
			}
		}
	}
	result, why := s.backtestResult(ctx, cmp, pr, held, o, t.IncidentTitles)
	switch {
	case why != "" && ctx.Err() != nil:
		return // no longer leading: the next leader runs it again
	case why != "":
		fail(why)
		return
	}
	s.endBacktest(ctx, b, ledger.BacktestDone, "", made(), result, start)
}

// backtestLooked says what a backtest running has asked (n, -1 for not
// now), and whether its job cancelled it.
func (s *Server) backtestLooked(ctx context.Context, id string, n int) (bool, error) {
	if n < 0 {
		b, ok, err := s.l.Backtest(ctx, id)
		return err == nil && (!ok || b.Cancel), err
	}
	return s.l.BacktestMade(ctx, id, n)
}

// backtestCompared replays a backtest's rules over [from, to), with serve's
// access through the lane, judged by serve's record, its firings kept for
// the Markdown's counts.
func (s *Server) backtestCompared(ctx context.Context, pr *prepared, from, to time.Time, o verdict.Options) (*verdict.Comparison, error) {
	if len(pr.gone)+len(pr.came) == 0 && len(pr.recording) == 0 {
		return &verdict.Comparison{From: from, To: to, Same: len(pr.same), ChangedOnly: true,
			Rules: []verdict.RuleChange{}, Incidents: []verdict.IncidentChange{}}, nil
	}
	return s.replayCompared(ctx, s.btPort.lane, s.btPort.parallel, pr.gone, pr.came, len(pr.same), from, to, s.backtestTimeout(), o)
}

// unreadByServe is, in a backtest's comment, why a rule whose query the
// history didn't answer couldn't be replayed: nothing of where the history
// is, nor what it said.
const unreadByServe = "query failed: the history couldn't be read by lapilli serve"

// scrubFor is what a reason a rule couldn't be replayed is said as in a
// backtest's comment: the request's own words as they are (a rule that
// doesn't parse), and every other in Lapilli's words alone, of a fixed few,
// as what the history answered may hold what it holds (a label's values,
// where it is).
func scrubFor(own map[string]bool) func(string) string {
	return func(why string) string {
		switch {
		case own[why]:
			return why
		case strings.HasPrefix(why, rules.TooWide):
			return strings.TrimSuffix(rules.TooWide, ": ")
		case strings.HasPrefix(why, rules.QueryFailed):
			return "query failed: the history refused it as written"
		}
		return "it couldn't be replayed"
	}
}

// backtestResult is what a backtest came to, kept and given back to its
// job: the Markdown lapilli verdict --compare --changed-only --markdown
// writes, its summary line among it, judged by serve's record and saying
// so, and its JSON. What either says is the request's own (its rules'
// names, labels and severities, its expectations), numbers, the
// incidents' ids and titles, and Lapilli's words: no label of a series a
// rule read (an alert's labels), which a rule could make of whatever
// serve's access reads; no reason of the history's; nothing of serve's
// settings. Held to the incidents (fail_missed), it says which incidents
// the change leaves caught by no rule, as lapilli verdict --fail-missed
// does, the rules left alone judged by serve's record, and replayed where it
// can't settle them (holdPrepared, holdMissed).
func (s *Server) backtestResult(ctx context.Context, cmp *verdict.Comparison, pr *prepared, held *leftAloneHeld, o verdict.Options, titles bool) ([]byte, string) {
	cmp.Recording, cmp.JudgedBy, cmp.ChangedOnly = pr.recording, verdict.JudgedByServe, true
	if len(pr.passedOver) > 0 {
		cmp.PassedOver = pr.passedOver
	}
	cmp.Summarize(true)
	// With nothing that differs, nothing is held, as lapilli verdict holds
	// nothing then: its summary says nothing changed.
	if pr.expect != nil && cmp.Before != nil && cmp.After != nil {
		// While people slept by the hours of the rule's team, where it has
		// its own, as serve's checks count it (docs/design/expectations.md).
		hours := func(string) *verdict.Hours { return o.Hours }
		if own, err := s.owners(ctx); err == nil {
			hours = own.schedules(o.Hours).of
		}
		// The scenarios a request names are the pull request's files,
		// which serve isn't sent and never reads, nor runs: each is said
		// as not run through serve, whatever --promtool serve has
		// (docs/design/expectations.md, step 4).
		cmp.Scenarios = verdict.NotThroughServe
		if err := cmp.ExpectBy(pr.expect, pr.before, hours); err != nil {
			return nil, "expect: " + clip(verdict.Clean(err.Error()), 600)
		}
	}
	if pr.failMissed {
		s.holdMissed(cmp, held)
	}
	verdict.ForComment(cmp, unreadByServe, scrubFor(pr.reasons), nil)
	cmp.Unreplayed = cmp.UnreplayedRules()
	severity := map[string]string{}
	for _, r := range slices.Concat(pr.gone, pr.came) {
		if v, ok := r.Labels["severity"]; ok {
			severity[r.Name] = v
		}
	}
	for _, rep := range []*verdict.Report{cmp.Before, cmp.After} {
		if rep == nil {
			continue
		}
		for i := range rep.Firings {
			f := &rep.Firings[i]
			f.Labels = nil
			if v, ok := severity[f.Rule]; ok {
				f.Labels = map[string]string{"severity": v}
			}
		}
	}
	// Of the incidents, those caught differently, and those caught by no
	// rule after the change, by their ids: a title written in a tracker is
	// said only where the entry says so.
	for _, list := range [][]verdict.IncidentChange{cmp.Incidents, cmp.Missed} {
		for i := range list {
			if !titles {
				list[i].Title = ""
			}
			list[i].Signal = ""
		}
	}
	var md bytes.Buffer
	if err := verdict.CompareMarkdown(&md, cmp, pr.loc); err != nil {
		return nil, "its comment couldn't be written"
	}
	js, err := reportForPullRequest(cmp)
	if err != nil {
		return nil, "its report couldn't be written"
	}
	out, err := json.Marshal(map[string]any{"state": ledger.BacktestDone, "markdown": md.String(), "report": json.RawMessage(js)})
	if err == nil && len(out) > ledger.MaxBacktestResult {
		out, err = json.Marshal(map[string]any{"state": ledger.BacktestDone, "markdown": md.String(), "report": nil,
			"note": fmt.Sprintf("the report is past %d MiB, and is left out: the Markdown alone", ledger.MaxBacktestResult>>20)})
	}
	if err != nil || len(out) > ledger.MaxBacktestResult {
		return nil, "its result is past 1 MiB"
	}
	return out, ""
}

// reportForPullRequest is a comparison's JSON as verdict.JSON writes it,
// less what a job has no use for and serve's record or settings would say:
// each side's firings (the counts stay) and incidents (the comparison's
// own, caught differently, and those caught by no rule after the change,
// stay, each its id, start and catches, and its title where
// CompareMarkdown said it), the thresholds and hours it was judged by, the
// impact signals, and what only lapilli verdict works out.
func reportForPullRequest(c *verdict.Comparison) ([]byte, error) {
	var b bytes.Buffer
	if err := verdict.JSON(&b, c); err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b.Bytes(), &m); err != nil {
		return nil, err
	}
	for _, side := range []string{"before", "after"} {
		if rep, ok := m[side].(map[string]any); ok {
			for _, k := range []string{"firings", "incidents", "settings", "noisy_at", "hours", "impact_short", "near_misses", "near_misses_cut",
				"unrecorded", "suggestions", "extras", "rules_changed"} {
				delete(rep, k)
			}
		}
	}
	for _, list := range []string{"incidents", "missed"} {
		incs, _ := m[list].([]any)
		for _, x := range incs {
			if in, ok := x.(map[string]any); ok {
				for k := range in {
					switch k {
					case "id", "title", "start", "before", "after":
					default:
						delete(in, k)
					}
				}
				if in["title"] == "" {
					delete(in, "title")
				}
			}
		}
	}
	return json.Marshal(m)
}

// endBacktest keeps how a backtest ended, logs its one line, and counts
// it.
func (s *Server) endBacktest(ctx context.Context, b ledger.Backtest, state, why string, made int, result []byte, start time.Time) {
	if err := s.l.EndBacktest(ctx, b.ID, state, why, made, result, s.now()); err != nil {
		if ctx.Err() == nil && !errors.Is(err, ledger.ErrNotLeading) {
			s.log.Error("backtest lane: keeping a backtest's end", "id", b.ID, "err", err)
		}
		return
	}
	s.backtestEnded(b.Trust, state)
	s.backtestLog(b, state, made, s.now().Sub(start))
}

// backtestResults are how a backtest ends, or is refused, as /metrics
// counts them.
var backtestResults = []string{ledger.BacktestDone, ledger.BacktestFailed, ledger.BacktestCancelled, ledger.BacktestSuperseded, "refused"}

// backtestMetrics writes the backtests ended since start, by entry and
// how, and those waiting now, by entry: never a repository, a branch or a
// rule.
func (s *Server) backtestMetrics(ctx context.Context, w io.Writer) {
	p := s.btPort
	if p == nil {
		return
	}
	p.mu.Lock()
	ended := maps.Clone(p.ended)
	p.mu.Unlock()
	fmt.Fprint(w, "# HELP lapilli_serve_backtests_total Pull request backtests since lapilli serve started, by entry of --backtest-trust and how they ended: done, failed, cancelled, superseded, or refused before they were queued.\n# TYPE lapilli_serve_backtests_total counter\n")
	for _, t := range p.trust {
		for _, how := range backtestResults {
			fmt.Fprintf(w, "lapilli_serve_backtests_total{trust=%q,result=%q} %d\n", t.Name, how, ended[[2]string{t.Name, how}])
		}
	}
	waiting, err := s.l.BacktestsWaiting(ctx)
	if err != nil {
		return
	}
	fmt.Fprint(w, "# HELP lapilli_serve_backtests_waiting Pull request backtests waiting, by entry of --backtest-trust.\n# TYPE lapilli_serve_backtests_waiting gauge\n")
	for _, t := range p.trust {
		fmt.Fprintf(w, "lapilli_serve_backtests_waiting{trust=%q} %d\n", t.Name, waiting[t.Name])
	}
}

// discoverIssuers asks each issuer --backtest-trust names for its keys at
// start, so that one set up wrong is in the log at once.
func (s *Server) discoverIssuers(ctx context.Context) {
	done := map[string]bool{}
	for _, t := range s.btPort.trust {
		if t.ci == nil || done[t.Issuer+"\x00"+t.JWKSURL] {
			continue
		}
		done[t.Issuer+"\x00"+t.JWKSURL] = true
		if err := t.ci.Discover(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("backtest port: an issuer --backtest-trust names couldn't be read", "trust", t.Name, "err", err)
		}
	}
}

// backtestServer is the backtest port's server, as the other ports' are.
func (s *Server) backtestServer(ctx context.Context) *http.Server {
	return &http.Server{Handler: s.BacktestHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
}
