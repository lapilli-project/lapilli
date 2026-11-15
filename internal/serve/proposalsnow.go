package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// What a proposal's evidence says when it is read (docs/design/changes.md,
// "Read when shown"), whatever version of lapilli made it: whether its for
// is one one event can't outlast, as proposed (for_outlasts), worked out
// from the rule it was proposed against, which its hash names, with the
// for proposed, as carrying it out makes the rule; whether its backtest's
// period had no incident written down to judge its firings by (no_record),
// as the backtest kept it or, of one made before it kept it, as the ledger
// has the period now; and whether, waiting for approval, it is stale: made
// by another version of lapilli, or its period's record changed since,
// when it waits to be backtested again before anyone approves it; or the
// rule changed since, as the rules in effect last recorded have it, when
// it is refused once it is checked again, as it was before (Stale).

// staleWhy is why a proposal waiting for approval waits to be backtested
// again: a text to translate, and its values.
type staleWhy struct {
	Format string
	Args   []any
}

// Why a proposal is stale, in English: keys into the catalogs, and what
// JSON, the API and MCP give as stale (staleText).
const (
	staleRule      = "the rule changed since it was proposed"
	staleNoVersion = "it was backtested by an earlier lapilli, before backtests were numbered by how they replay and judge"
	staleVersion   = "it was backtested by lapilli %s, which replays or judges otherwise than this one"
	// The record is judged by whole weeks (verdict.Record): an incident
	// in a week the period has some of, or the two weeks before, keeps it.
	staleRecorded   = "an incident was written down in its period's weeks since it was backtested"
	staleUnrecorded = "no incident keeps the record of its period's weeks any more"
)

// staleNotice is what a stale proposal's page says at its top, with why.
const staleNotice = "It is stale, so it isn't approved until it is backtested again: %s."

// Text is why, in a language; HTML the same, each value set apart; Notice
// what the page says of it.
func (w staleWhy) Text(lang string) string        { return tr(lang, w.Format, w.Args...) }
func (w staleWhy) HTML(lang string) template.HTML { return trBDI(lang, w.Format, w.Args...) }
func (w staleWhy) Notice(lang string) template.HTML {
	return template.HTML(fmt.Sprintf(template.HTMLEscapeString(tr(lang, staleNotice)), w.HTML(lang)))
}

// versionOf is the version of lapilli running, as a backtest keeps it:
// Config.Version, or dev for none, as a build of its own has.
func (s *Server) versionOf() string {
	if s.cfg.Version == "" {
		return "dev"
	}
	return s.cfg.Version
}

// backtested reports whether a proposal's kind has a backtest: a for or a
// threshold, replayed.
func backtested(kind string) bool { return kind == ledger.KindFor || kind == ledger.KindThreshold }

// proposedFor is the for a proposal of a new for proposes: its edit's, or
// its evidence's.
func proposedFor(ev evidence) time.Duration {
	if ev.Edit != nil && ev.Edit.For != nil {
		return *ev.Edit.For
	}
	return ev.For
}

// proposedRule is a proposal's rule as proposed, made from r, the rule it
// is made against, as carrying it out makes it (checkEvidence): its for,
// or its threshold, as the evidence keeps them.
func proposedRule(p ledger.Proposal, ev evidence, r rules.Rule) (rules.Rule, error) {
	want := r
	switch p.Kind {
	case ledger.KindFor:
		want.For = proposedFor(ev)
	case ledger.KindThreshold:
		if ev.Bound == nil || ev.Value == nil {
			return want, refusal{"the proposal doesn't say which number to change"}
		}
		j := slices.IndexFunc(r.Bounds(), func(x rules.Bound) bool { return x.At == *ev.Bound })
		if j < 0 {
			return want, refusal{"the rule changed since it was proposed, or is gone: propose it again"}
		}
		want = r.WithBound(r.Bounds()[j], *ev.Value)
	}
	return want, nil
}

// outlastKey is a proposal as its for_outlasts is kept: by its id, what
// its rule hashed to, before and as applied, and its evidence, which says
// the for it proposes and is kept anew when it is backtested again.
type outlastKey struct {
	id            int64
	before, after string
	evidence      uint64
}

func outlastKeyOf(p ledger.Proposal) outlastKey {
	h := fnv.New64a()
	h.Write(p.Evidence)
	return outlastKey{p.ID, p.BeforeHash, p.AfterHash, h.Sum64()}
}

// outlasts are the for_outlasts worked out, by proposal: what the rule a
// proposal was made against hashes to doesn't change, nor does the for it
// proposes, so each is worked out once while serve runs, and a rule found
// only in an older version is read once.
type outlasts struct {
	sync.Mutex
	m map[outlastKey]*verdict.Outlast
}

// outlastsKept is how many outlasts keeps at most: one out, any one, past it.
const outlastsKept = 20000

func (o *outlasts) get(k outlastKey) (*verdict.Outlast, bool) {
	o.Lock()
	defer o.Unlock()
	v, ok := o.m[k]
	return v, ok
}

func (o *outlasts) put(k outlastKey, v *verdict.Outlast) {
	o.Lock()
	defer o.Unlock()
	if o.m == nil {
		o.m = map[outlastKey]*verdict.Outlast{}
	}
	if len(o.m) >= outlastsKept {
		for k := range o.m {
			delete(o.m, k)
			break
		}
	}
	o.m[k] = v
}

// reading is what proposalsNow reads once for the proposals it is given:
// the rules in effect.
type reading struct {
	inForce
}

// oldVersionsMost is how many versions older than the latest one read of
// the proposals parses for their for_outlasts, at most: past them, each
// says what it kept, and a later read works it out. One version of a few
// thousand rules parsed is some megabytes, until the rules matched are
// taken out of it.
const oldVersionsMost = 4

// flights are reads under way, by key, so that requests asking for the same
// at once wait for one (a version of the rules parsed, by its id).
type flights[T any] struct {
	sync.Mutex
	m map[int64]*flight[T]
}

type flight[T any] struct {
	done chan struct{}
	v    T
	err  error
}

// do is fn's value for key, worked out once for all who ask while it is.
func (f *flights[T]) do(ctx context.Context, key int64, fn func() (T, error)) (T, error) {
	f.Lock()
	if c, ok := f.m[key]; ok {
		f.Unlock()
		select {
		case <-c.done:
			return c.v, c.err
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
	}
	c := &flight[T]{done: make(chan struct{})}
	if f.m == nil {
		f.m = map[int64]*flight[T]{}
	}
	f.m[key] = c
	f.Unlock()
	c.v, c.err = fn()
	f.Lock()
	delete(f.m, key)
	f.Unlock()
	close(c.done)
	return c.v, c.err
}

// namedRules are a version's rules and the names the ledger judges them
// under (named).
type namedRules struct {
	all   []rules.Rule
	names []string
}

// outlastOf is what for_outlasts says of a proposal of a new for, made
// against r, as proposed.
func outlastOf(r rules.Rule, ev evidence) *verdict.Outlast {
	f := proposedFor(ev)
	if f <= 0 {
		return ev.Outlasts
	}
	want := r
	want.For = f
	if w, ok := want.ForOutlasts(); ok {
		return &verdict.Outlast{For: f, Window: w}
	}
	return nil
}

// outlastsNow sets each line's Outlasts, of a proposal of a new for: what
// for_outlasts says of the rule as proposed, worked out from the rule it
// was made against, which its hash names, with the for it proposes. A
// proposal finished with says what it kept, as do any other kind (none)
// and one whose rule isn't found. The rules in effect come first; the rest
// are found in the version in effect when each was made, each version
// parsed once for all of them, and by one of the requests asking for it at
// once, only the rules matched kept; oldVersionsMost of them at most.
func (s *Server) outlastsNow(ctx context.Context, ls []proposalLine, evs []evidence, rd *reading) error {
	older := map[int64][]int{}
	var order []int64
	for i := range ls {
		l, ev := &ls[i], evs[i]
		switch {
		case l.Kind != ledger.KindFor:
			continue
		case !l.Open():
			l.Outlasts = ev.Outlasts
			continue
		}
		k := outlastKeyOf(l.Proposal)
		if o, ok := s.outlasts.get(k); ok {
			l.Outlasts = o
			continue
		}
		if r, ok := rd.byHash[l.BeforeHash]; ok && l.BeforeHash != "" {
			l.Outlasts = outlastOf(r, ev)
			s.outlasts.put(k, l.Outlasts)
			continue
		}
		l.Outlasts = ev.Outlasts // unless its version is read below
		vs, err := s.l.Versions(ctx, l.Made, l.Made, false)
		if err != nil {
			return err
		}
		if len(vs) == 0 {
			s.outlasts.put(k, l.Outlasts)
			continue
		}
		id := vs[len(vs)-1].ID
		if older[id] == nil {
			order = append(order, id)
		}
		older[id] = append(older[id], i)
	}
	for n, id := range order {
		if n == oldVersionsMost {
			break // the rest say what they kept, this time
		}
		v, err := s.parsing.do(ctx, id, func() (namedRules, error) {
			all, names, err := s.named(ctx, id)
			return namedRules{all, names}, err
		})
		if err != nil {
			return err
		}
		for _, i := range older[id] {
			l := &ls[i]
			for j, x := range v.all {
				if v.names[j] == l.Rule && ruleHash(x) == l.BeforeHash {
					l.Outlasts = outlastOf(x, evs[i])
					break
				}
			}
			s.outlasts.put(outlastKeyOf(l.Proposal), l.Outlasts)
		}
	}
	return nil
}

// span is a backtest's period, as a map's key: its ends in seconds.
type span struct{ from, to int64 }

func spanOf(bt *backtest) span { return span{bt.From.Unix(), bt.To.Unix()} }

// proposalsNow works out, for each line, what its evidence says now: its
// for_outlasts, whether its backtest's period had a record, and, waiting
// for approval, why it is stale. rd has the rules in effect the lines were
// told stale (Stale) by. The period's record is read once for all of them:
// the incidents from two weeks before the first to the end of the last.
func (s *Server) proposalsNow(ctx context.Context, ls []proposalLine, rd *reading) error {
	evs := make([]evidence, len(ls))
	var spans []span
	for i := range ls {
		evs[i] = proposalEvidence(ls[i].Proposal)
		if bt := evs[i].Backtest; bt != nil && (bt.NoRecord == nil || ls[i].State == ledger.StateProposed) {
			spans = append(spans, spanOf(bt))
		}
	}
	kept := map[span]bool{}
	if len(spans) > 0 {
		from, to := spans[0].from, spans[0].to
		for _, x := range spans {
			from, to = min(from, x.from), max(to, x.to)
		}
		o, err := s.options(ctx)
		if err != nil {
			return err
		}
		rec, err := s.l.RecordOf(ctx, time.Unix(from, 0), time.Unix(to, 0), o, nil)
		if err != nil {
			return err
		}
		for _, x := range spans {
			if _, ok := kept[x]; ok || x.from >= x.to {
				continue
			}
			kept[x] = slices.ContainsFunc(s.l.SpanWeeks(time.Unix(x.from, 0), time.Unix(x.to, 0)), func(w int64) bool { return s.l.WeekKept(rec, w) })
		}
	}
	if err := s.outlastsNow(ctx, ls, evs, rd); err != nil {
		return err
	}
	for i := range ls {
		l, ev := &ls[i], evs[i]
		l.read = true
		bt := ev.Backtest
		now := false
		if bt != nil {
			now = !kept[spanOf(bt)]
			l.NoRecord = now && !bt.judgedSome()
			if bt.NoRecord != nil {
				l.NoRecord = *bt.NoRecord
			}
		}
		if l.State != ledger.StateProposed || bt == nil || !backtested(l.Kind) {
			continue
		}
		switch {
		case ev.Semantics == 0:
			l.StaleWhy = &staleWhy{Format: staleNoVersion}
		case ev.Semantics != verdict.Semantics:
			l.StaleWhy = &staleWhy{Format: staleVersion, Args: []any{cmpOr(ev.Version, "dev")}}
		case bt.NoRecord != nil && *bt.NoRecord && !now:
			l.StaleWhy = &staleWhy{Format: staleRecorded}
		case bt.NoRecord != nil && !*bt.NoRecord && now:
			l.StaleWhy = &staleWhy{Format: staleUnrecorded}
		}
	}
	return nil
}

// EarlierMissed are the incidents an earlier backtest said the change
// misses that its backtest now doesn't name: said on its page, in its pull
// request's description and through MCP, beside those it does.
func (ev evidence) EarlierMissed() []string {
	var out []string
	for _, id := range ev.EarlierLost {
		if ev.Backtest == nil || !slices.Contains(ev.Backtest.Lost, id) {
			out = append(out, id)
		}
	}
	return out
}

// earlierLost are the incidents ev's backtests, this one and those before
// it, said the change misses, each once, as a backtest run again keeps them.
func earlierLost(ev evidence) []string {
	out := slices.Clone(ev.EarlierLost)
	if ev.Backtest != nil {
		for _, id := range ev.Backtest.Lost {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// heldPromotable are the proposals --approvals lowered would approve that
// wait to be backtested again: held, as an approval of one is.
func (s *Server) heldPromotable(ctx context.Context) ([]int64, error) {
	ps, err := s.l.Promotable(ctx, s.approvals())
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	ls, err := s.linesNow(ctx, ps)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, l := range ls {
		if l.StaleWhy != nil {
			out = append(out, l.ID)
		}
	}
	return out, nil
}

// staleText is why a proposal is stale, in English, as JSON, the API and
// MCP give it: its rule changed since, of one not finished with (Stale), or
// why, waiting for approval, it waits to be backtested again; "" for
// neither.
func (l proposalLine) staleText() string {
	switch {
	case l.Stale:
		return staleRule
	case l.StaleWhy != nil:
		return l.StaleWhy.Text("en")
	}
	return ""
}

// staleNow reports whether a proposal waiting for approval is stale, for
// either reason: what the list's stale=1 picks.
func (l proposalLine) staleNow() bool {
	return l.State == ledger.StateProposed && (l.Stale || l.StaleWhy != nil)
}

// linesNow are proposals' lines with what their evidence says now
// (proposalsNow).
func (s *Server) linesNow(ctx context.Context, ps []ledger.Proposal) ([]proposalLine, error) {
	ls, rd, err := s.linesOf(ctx, ps)
	if err != nil {
		return nil, err
	}
	return ls, s.proposalsNow(ctx, ls, rd)
}

// judgedSome reports whether a backtest judged some of its firings: one
// caught an incident, or an incident was caught otherwise, missed or
// caught later. Its period had a record then, whatever the ledger says of
// it now (an incident since taken away): one made before it kept no_record
// isn't taken as having none.
func (bt *backtest) judgedSome() bool {
	return bt.Before.White > 0 || bt.After.White > 0 || len(bt.Lost)+len(bt.Gained)+len(bt.Later) > 0
}

// spanNoRecord reports whether a backtest's period had no record: as the
// backtest kept it, or, of one made before it kept it and judged none of
// its firings, as the ledger has the period now. An undo's page reads it
// of the change it undoes.
func (s *Server) spanNoRecord(ctx context.Context, bt *backtest) (bool, error) {
	if bt == nil {
		return false, nil
	}
	if bt.NoRecord != nil {
		return *bt.NoRecord, nil
	}
	if bt.judgedSome() {
		return false, nil
	}
	o, err := s.options(ctx)
	if err != nil {
		return false, err
	}
	kept, err := s.l.RecordKept(ctx, bt.From, bt.To, o)
	return !kept, err
}

// noRecordFewer says, after verdict.NoRecordCompared, what a period with
// no record means for a change's backtest: in English, in a pull
// request's description; a proposal's page says both in the reader's
// language.
const noRecordFewer = "Fewer firings aren't better, nor more worse: fewer may be events that happened, hidden."

// errFresh is a proposal backtested again that isn't stale: its backtest
// is this lapilli's, of its period's record as it is.
var errFresh = bad("it isn't stale: its backtest replays and judges as this lapilli does, of its period's record as it is, so there is nothing to backtest again")

// errNoBacktest is a proposal backtested again that has no backtest: a
// change to what a rule says, or retiring it.
var errNoBacktest = bad("it has no backtest to run again: only a new for or a new threshold is backtested")

// errRuleChanged is a proposal backtested again whose rule changed since
// it was made, as the rules in effect last recorded have it, or as they
// are read again: it no longer applies to the rule as it is.
var errRuleChanged = bad("the rule changed since it was proposed, so the proposal no longer applies to the rule as it is: propose it again from the rule's page")

// errHeld is an approval of a proposal that waits to be backtested again.
var errHeld = bad("it is stale, so it isn't approved until it is backtested again: backtest it again from its page, then approve it")

// backtestAgain runs a stale proposal's backtest again, with this lapilli,
// against the rule in effect, over the twelve weeks to now (as proposing
// it would), for who asks (agent, for an agent acting for them): its
// evidence, with the new backtest, this version and whether its period had
// a record, in place of the old, which its history keeps, and the
// approvals it had taken back. Asked as a proposal is (propose): the
// reader's tries an hour, their agents' apart, counted once the rule is
// found in effect, then the rules read again from where they are kept;
// refused when the rule changed since.
func (s *Server) backtestAgain(ctx context.Context, id int64, by string, agent bool) error {
	return s.backtestAgainOf(ctx, id, by, agent, false)
}

// backtestAgainOf is backtestAgain, wait saying to wait for a comparison
// running, as Lapilli's own work does; a person asking is told to try
// again.
func (s *Server) backtestAgainOf(ctx context.Context, id int64, by string, agent, wait bool) error {
	if !s.replaying() {
		return bad("this lapilli serve reads no rules of its own: there is nothing to change")
	}
	p, err := s.l.ProposalByID(ctx, id)
	if err != nil {
		return err
	}
	ls, err := s.linesNow(ctx, []ledger.Proposal{p})
	if err != nil {
		return err
	}
	l, ev := ls[0], proposalEvidence(p)
	switch {
	case p.State != ledger.StateProposed:
		return ledger.ErrMoved
	case !backtested(p.Kind) || ev.Backtest == nil:
		return errNoBacktest
	case l.StaleWhy == nil && l.Stale:
		return errRuleChanged
	case l.StaleWhy == nil:
		return errFresh
	}
	// Refused without waiting for the heavy turn: the hour's backtests
	// again, and the proposal's last, as the ledger keeps them; and the
	// reader's tries, as proposing counts them.
	if err := s.l.MayBacktestAgain(ctx, id, by, agent); err != nil {
		return err
	}
	if by != ledger.Proposer && !s.tries.may(by, agent, time.Now()) {
		return errTooManyTries
	}
	done, err := s.busy(ctx)
	if err != nil {
		return err
	}
	defer done()
	f, err := s.current(ctx, p.Rule, false)
	var b badRequest
	switch {
	case errors.As(err, &b):
		return errRuleChanged // no longer in effect
	case err != nil:
		return err
	}
	// The rule as it is kept now, read again as proposing reads it, is
	// what it was made against, or it no longer applies: the rules in
	// effect last recorded can lag a change Lapilli made through an API.
	if by != ledger.Proposer && !s.tries.take(by, agent, time.Now()) {
		return errTooManyTries
	}
	if f, err = s.reread(ctx, f); err != nil {
		return err
	}
	if hashOf(p, f.rule) != p.BeforeHash {
		return errRuleChanged
	}
	want, err := proposedRule(p, ev, f.rule)
	var no refusal
	if errors.As(err, &no) {
		return errRuleChanged
	}
	bt, err := s.backtest(ctx, f.name, f.rule, want, wait)
	if err != nil {
		return err
	}
	again := ev
	again.Backtest, again.Semantics, again.Version, again.Outlasts = bt, verdict.Semantics, s.versionOf(), nil
	again.EarlierLost = earlierLost(ev)
	if p.Kind == ledger.KindFor {
		if w, ok := want.ForOutlasts(); ok {
			again.Outlasts = &verdict.Outlast{For: want.For, Window: w}
		}
	}
	raw, err := json.Marshal(again)
	if err != nil {
		return err
	}
	_, _, err = s.l.BacktestAgain(ctx, p.ID, by, agent, p.Evidence, raw,
		map[string]any{"backtest": ev.Backtest, "version": ev.Version}, map[string]any{"backtest": bt, "version": again.Version})
	return err
}

// againARun is how many stale proposals Lapilli backtests again itself in
// a round of its work, at most, one at a time, each within --timeout: after
// verdict.Semantics changes, every one waiting is, a few each round.
const againARun = 3

// againRetry is how long Lapilli leaves a proposal it failed to backtest
// again itself before it tries again.
const againRetry = time.Hour

// againFailed are, by proposal, when Lapilli last failed to backtest one
// again itself (this replica's: the next leader tries again).
type againFailed struct {
	sync.Mutex
	m map[int64]time.Time
}

// backtestStale backtests again itself, as Lapilli (ledger.Proposer), the
// proposals waiting for approval that are stale, the longest unmoved
// first, againARun at most: those an upgrade that changes how a backtest
// replays or judges left waiting, Lapilli's own among them, which no one
// would otherwise ask for, and those whose period's record changed since.
// Each under the heavy turn, as a person's is, waiting for a comparison
// running; never one whose rule changed since (it is proposed again), one
// backtested again in the last ten minutes, or one it failed to in the
// last hour. Approvals withdrawn so are told, as any are.
func (s *Server) backtestStale(ctx context.Context) error {
	if !s.replaying() || s.cfg.ReadOnly {
		return nil
	}
	heads, err := s.l.ProposalList(ctx, ledger.ProposalPick{States: []string{ledger.StateProposed}}, staleMost)
	if err != nil || len(heads) == 0 {
		return err
	}
	ps := make([]ledger.Proposal, len(heads))
	for i, h := range heads {
		ps[i] = h.Proposal
	}
	ls, err := s.linesNow(ctx, ps)
	if err != nil {
		return err
	}
	now := time.Now()
	s.againFailed.Lock()
	ls = slices.DeleteFunc(ls, func(l proposalLine) bool {
		return l.StaleWhy == nil || l.Stale || now.Sub(s.againFailed.m[l.ID]) < againRetry
	})
	s.againFailed.Unlock()
	slices.SortFunc(ls, func(a, b proposalLine) int {
		if c := a.Updated.Compare(b.Updated); c != 0 {
			return c
		}
		return int(a.ID - b.ID)
	})
	n := 0
	for _, l := range ls {
		if n == againARun || !s.still(ctx) {
			break
		}
		err := s.backtestAgainOf(ctx, l.ID, ledger.Proposer, false, true)
		var b badRequest
		switch {
		case err == nil:
			n++
		case errors.Is(err, ledger.ErrBacktestedJustNow), errors.Is(err, ledger.ErrMoved), errors.Is(err, errFresh):
			// Done by someone else meanwhile, or moved on.
		case errors.Is(err, errBusy):
			return nil // a person's heavy request has the turn: the next round
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil:
			return err
		case errors.As(err, &b), errors.Is(err, errRuleChanged):
			// Its rule changed, or it can't be: left for a person.
			s.againFailed.Lock()
			if s.againFailed.m == nil {
				s.againFailed.m = map[int64]time.Time{}
			}
			s.againFailed.m[l.ID] = now
			s.againFailed.Unlock()
			n++
		default:
			s.againFailed.Lock()
			if s.againFailed.m == nil {
				s.againFailed.m = map[int64]time.Time{}
			}
			s.againFailed.m[l.ID] = now
			s.againFailed.Unlock()
			return err
		}
	}
	return nil
}

// postBacktestAgain is "Backtest again" on a stale proposal's page: an
// editor's, as proposing is (POST /proposals), from this site.
func (s *Server) postBacktestAgain(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	id := pathID(r)
	if err := s.backtestAgain(r.Context(), id, s.user(r), false); err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, fmt.Sprintf("/proposals/%d#backtest", id))
}

type backtestAgainIn struct {
	ID int64 `json:"id" jsonschema:"the proposal's number, as rule's proposals give it"`
}

type backtestAgainOut struct {
	ID   int64  `json:"id"`
	Page string `json:"page,omitempty" jsonschema:"its page, where a person approves it"`
}

// toolBacktestAgain is an agent backtesting a stale proposal again, for
// the person it acts for, as it proposes (toolPropose): counted against
// their agents' tries an hour, never their own.
func (s *Server) toolBacktestAgain(ctx context.Context, _ *mcp.CallToolRequest, in backtestAgainIn) (*mcp.CallToolResult, backtestAgainOut, error) {
	out := backtestAgainOut{ID: in.ID}
	by, ok := actsFor(ctx)
	switch {
	case !ok:
		return nil, out, errors.New("who this agent acts for isn't known: nothing is backtested")
	case by == "anonymous" && s.in != nil:
		return nil, out, errors.New("who this agent acts for isn't known: nothing is backtested")
	}
	if err := s.backtestAgain(ctx, in.ID, by, true); err != nil {
		return nil, out, err
	}
	if s.cfg.ExternalURL != "" {
		out.Page = fmt.Sprintf("%s/proposals/%d", strings.TrimRight(s.cfg.ExternalURL, "/"), in.ID)
	}
	return nil, out, nil
}

// proposalJSON is a proposal as the API gives it: as kept, with what its
// evidence says now (proposalsNow).
type proposalJSON struct {
	ledger.Proposal
	// Stale is why it is stale (staleText); "" when it isn't.
	Stale string `json:"stale,omitempty"`
	// Outlasts is, of a new for, the for one event can't outlast, as
	// proposed, and the range its condition counts events over.
	Outlasts *verdict.Outlast `json:"for_outlasts,omitempty"`
	// NoRecord says, of one with a backtest, that its period had no
	// incident written down to judge its firings by.
	NoRecord *bool `json:"no_record,omitempty"`
}

func proposalJSONOf(l proposalLine) proposalJSON {
	out := proposalJSON{Proposal: l.Proposal, Outlasts: l.Outlasts, Stale: l.staleText()}
	if proposalEvidence(l.Proposal).Backtest != nil {
		out.NoRecord = &l.NoRecord
	}
	return out
}
