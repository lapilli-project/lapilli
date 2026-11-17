package serve

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// Named impact signals in lapilli serve (docs/design/signals.md, step 2):
// each read on its own by the runs, missed on its own when it can't be,
// read again alone when its expression or "for" changes, with its own
// start, where the data of the selectors it reads begins, and what it has
// seen, from what it returned and what it reads; each alert tied to the
// signals that could have seen it, and the tallies kept by them. With
// none, the impact signal has no name, and everything is as it was.

// maxSignalWindows is how many windows one read of a signal keeps at
// most: past it, the signal is missed for the run, as an expression by
// pod, not by service, finds too many.
const maxSignalWindows = 1000

// maxSignalWeek is how many windows a signal keeps in a week at most: past
// it, a run's read of it is missed. A variable for tests.
var maxSignalWeek = 2000

// errTooManyWindows is a signal's read that found more than
// maxSignalWindows windows, or past maxSignalWeek in its week.
var errTooManyWindows = fmt.Errorf("more than %d windows in one read: an expression by pod, not by service, finds too many", maxSignalWindows)

// namedSignals says serve reads named impact signals.
func (s *Server) namedSignals() bool { return len(s.cfg.Signals) > 0 }

// signalCriterion is what makes a named signal's windows: its expression
// and its "for", keyed as the unnamed signal's criterion is.
func signalCriterion(d ledger.SignalDef) criterion { return criterion{d.Expr, d.For} }

// signalsCache keeps the signals worked out until the ledger changes: each
// page reads them, with what each signal has seen.
type signalsCache struct {
	sync.Mutex
	gen   int64
	match string
	sig   *verdict.Signals
}

// signalsOf are the named signals in effect, as they judge (o.Signals):
// each with its bit, its start and what it has seen, on match (the match
// in effect), as the ledger keeps them, and the rule entries, each rule's
// found once by its name or labels (owners); what replays judge in memory
// ties a firing as the ledger ties its alert. Nil with none named.
func (s *Server) signalsOf(ctx context.Context, match []string) (*verdict.Signals, error) {
	if !s.namedSignals() {
		return nil, nil
	}
	gen, key := s.l.Gen(), strings.Join(match, "\x00")
	s.sigs.Lock()
	if s.sigs.sig != nil && s.sigs.gen == gen && s.sigs.match == key {
		sig := s.sigs.sig
		s.sigs.Unlock()
		return sig, nil
	}
	s.sigs.Unlock()
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		return nil, err
	}
	seen, err := s.l.SeenOf(ctx, match)
	if err != nil {
		return nil, err
	}
	list := make([]verdict.Signal, 0, len(s.cfg.Signals))
	for _, d := range s.cfg.Signals {
		x := verdict.Signal{Name: d.Name, For: d.For, Match: d.Match, Record: d.Record, Bit: -1}
		if i := slices.IndexFunc(rows, func(r ledger.SignalRow) bool { return r.Name == d.Name }); i >= 0 {
			x.Bit, x.Start = rows[i].Bit, rows[i].Kept.From()
		}
		if st, ok := seen[d.Name]; ok {
			x.Seen = st.Seen
		} else {
			x.Seen = verdict.NewSeen(d.MatchOr(match))
		}
		list = append(list, x)
	}
	sig := verdict.NewSignals(list, s.cfg.ImpactRules)
	if len(s.cfg.ImpactRules) > 0 {
		own, err := s.owners(ctx)
		if err != nil {
			return nil, err
		}
		sig = sig.Index(own.ruleLabels)
	}
	s.sigs.Lock()
	s.sigs.gen, s.sigs.match, s.sigs.sig = gen, key, sig
	s.sigs.Unlock()
	return sig, nil
}

// syncSignals keeps the named signals serve was started with, and the rule
// entries, in the ledger (ledger.SyncSignals): when a signal was added or
// taken away, or its match or record changed, or the entries, every firing
// is judged again, as a change to --match judges it.
func (s *Server) syncSignals(ctx context.Context) (bool, error) {
	set, err := s.l.Settings(ctx)
	if err != nil {
		return false, err
	}
	o := s.cfg.Options
	if v, ok := set["match"]; ok {
		if err := apply(&o, "match", v.Value); err != nil {
			o.Match = s.cfg.Options.Match
		}
	}
	synced, err := s.l.SyncSignals(ctx, s.cfg.Signals, s.cfg.ImpactRules, o.Match)
	return synced.Set || synced.Entries, err
}

// signalsRead reads each named signal for a run over [from, to] (Found's
// Signals): from the hour before from, or from where a run before missed
// it, within a week, each in turn within --timeout (inTurn), apart from
// the replay's. One that fails, finds more than
// maxSignalWindows windows, or is left unread when the time runs out, is
// missed for the run, which records the rest; one Prometheus turned away
// (a 429 yielded to) holds the run, as the unnamed signal's does.
func (s *Server) signalsRead(parent context.Context, from, to time.Time, f *ledger.Found) error {
	rows, err := s.l.SignalRows(parent)
	if err != nil {
		return err
	}
	match := s.cfg.Options.Match
	if o, err := s.options(parent); err == nil {
		match = o.Match
	}
	missed := map[string]time.Time{}
	for _, r := range rows {
		if !r.Missed.IsZero() && r.Missed.Before(from) && to.Sub(r.Missed) <= 7*24*time.Hour {
			missed[r.Name] = r.Missed
		}
	}
	// A signal the ledger doesn't keep yet (its row, with its own bit, is
	// SyncSignals'), or no longer, isn't read: nothing of it would be kept.
	order := slices.DeleteFunc(slices.Clone(s.cfg.Signals), func(d ledger.SignalDef) bool {
		return !slices.ContainsFunc(rows, func(r ledger.SignalRow) bool { return r.Name == d.Name })
	})
	slices.SortStableFunc(order, func(a, b ledger.SignalDef) int {
		x, okx := missed[a.Name]
		y, oky := missed[b.Name]
		switch {
		case okx && oky:
			return x.Compare(y)
		case okx:
			return -1
		case oky:
			return 1
		}
		return 0
	})
	// Each in turn, within --timeout (inTurn): one that hangs runs out its
	// own share, and leaves the rest theirs; one that needs more than its
	// share is read on its turn.
	reads := make([]ledger.SignalRead, len(order))
	asks := make([]turnAsk, len(order))
	var fatal error
	for i, d := range order {
		start := from
		at, behind := missed[d.Name]
		if behind {
			start = at
		}
		reads[i] = ledger.SignalRead{Name: d.Name, Criterion: signalCriterion(d).key(), Match: d.Match, Since: start.Add(-time.Hour), From: start}
		asks[i] = turnAsk{name: d.Name, behind: behind, ask: func(ctx context.Context) error {
			r := &reads[i]
			series, err := s.h.QueryRange(ctx, d.Expr, r.Since, to, s.cfg.Step)
			if err != nil {
				return err
			}
			r.Windows = incident.FromSignal(series, s.cfg.Step, d.For, d.Name, d.Match)
			// Those of its week kept before its read, which it doesn't put
			// in place of.
			week, _ := s.l.WeekOf(r.From)
			before, err := s.l.SignalWindowsBetween(parent, d.Name, week, r.From)
			if err != nil {
				fatal = err
				return err
			}
			// Of its own, those it puts in place from where it reads: the
			// hour before is read to carry on a window, not counted again.
			fresh := 0
			for _, w := range r.Windows {
				if !w.Start.Before(r.From) {
					fresh++
				}
			}
			if len(r.Windows) > maxSignalWindows || before+fresh > maxSignalWeek {
				return errTooManyWindows
			}
			r.Seen = verdict.NewSeen(d.MatchOr(match))
			for _, se := range series {
				if len(se.Samples) > 0 {
					r.Seen.Add(se.Labels)
				}
			}
			return nil
		}}
	}
	errs := s.inTurn(parent, "read", asks, func(err error) bool { return prom.IsYielded(err) || fatal != nil })
	if fatal != nil {
		return fatal
	}
	for i, err := range errs {
		r := reads[i]
		switch {
		case err != nil && prom.IsYielded(err):
			return fmt.Errorf("--impact %s: %w", r.Name, err)
		case err != nil:
			r.Windows, r.Seen, r.Missed = nil, nil, true
			s.readSignal(r.Name, err)
		default:
			s.readSignal(r.Name, nil)
		}
		f.Signals = append(f.Signals, r)
	}
	return nil
}

// signalSeen is how the last read of a named signal went, as the sources
// panel will say it: when it was last read, and when it last failed, why.
type signalSeen struct {
	Read   time.Time `json:"read,omitzero"`
	Failed time.Time `json:"failed,omitzero"`
	Err    string    `json:"error,omitempty"`
}

// readSignal notes how a read of a named signal went.
func (s *Server) readSignal(name string, err error) {
	s.sources.Lock()
	defer s.sources.Unlock()
	if s.sources.signals == nil {
		s.sources.signals = map[string]signalSeen{}
	}
	x := s.sources.signals[name]
	if err != nil {
		x.Failed, x.Err = s.now(), promText(err)
		s.log.Warn("an impact signal couldn't be read: missed for the run, read from there by a later one", "signal", name, "err", x.Err)
	} else {
		x.Read, x.Err = s.now(), ""
	}
	s.sources.signals[name] = x
}

// signalsData looks, of each named signal, for where the data of the
// selectors of the criterion it keeps the record by begins, as impactData
// does of the unnamed one, each in turn within --timeout (inTurn): one
// that fails passing to the next, the last failure returned.
func (s *Server) signalsData(ctx context.Context) error {
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		return err
	}
	// Each in turn, within --timeout (inTurn), as a run reads them.
	var asks []turnAsk
	for _, r := range rows {
		if r.Name == "" || !slices.ContainsFunc(s.cfg.Signals, func(d ledger.SignalDef) bool { return d.Name == r.Name }) {
			continue
		}
		asks = append(asks, turnAsk{name: r.Name, ask: func(ctx context.Context) error {
			k := r.Kept
			return s.impactDataOf(ctx, r.Name, &k)
		}})
	}
	var failed error
	for i, err := range s.inTurn(ctx, "data", asks, nil) {
		if err != nil {
			failed = fmt.Errorf("%s: %w", asks[i].name, err)
		}
	}
	return failed
}

// signalsAgain reads again, over the last four weeks, each named signal
// whose expression or "for" the runs read since what keeps its record, or
// that was added since they began (againPlanOf), alone, as impactAgain
// reads the unnamed one: its windows in place of its own from then on,
// where its data begins, and what it returned. One that fails is tried
// again later, an hour, then two, four, up to a day, while the others go
// on. It reports whether it read one again.
func (s *Server) signalsAgain(ctx context.Context, o verdict.Options) (bool, error) {
	var any bool
	var failed error
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		return false, err
	}
	for _, d := range s.cfg.Signals {
		if !slices.ContainsFunc(rows, func(r ledger.SignalRow) bool { return r.Name == d.Name }) {
			continue // not kept by the ledger: nothing of it would be
		}
		c := signalCriterion(d)
		plan, _, err := s.againPlanOf(ctx, d.Name, c, "")
		switch {
		case err != nil:
			return any, err
		case plan == againNone:
			continue
		case plan == againMark:
			if err := s.l.MarkAgainOf(ctx, d.Name, c.key()); err != nil {
				return any, err
			}
			continue
		}
		now := s.now()
		s.sources.Lock()
		a := s.sources.againBy[d.Name]
		s.sources.Unlock()
		if a.Key == c.key() && now.Before(a.Next) {
			continue
		}
		read, err := s.signalAgain(ctx, d, c, o)
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return any, err
		}
		s.sources.Lock()
		if s.sources.againBy == nil {
			s.sources.againBy = map[string]againState{}
		}
		if err != nil {
			g := s.sources.againBy[d.Name]
			if g.Key != c.key() {
				g = againState{Key: c.key()}
			}
			g.Tries++
			g.Failed, g.Err = now, promText(err)
			g.Next = now.Add(min(time.Hour<<min(g.Tries-1, 5), 24*time.Hour))
			s.sources.againBy[d.Name] = g
			failed = fmt.Errorf("--impact %s read again over the last four weeks: %w", d.Name, err)
		} else {
			delete(s.sources.againBy, d.Name)
		}
		s.sources.Unlock()
		any = any || read
	}
	return any, failed
}

// signalAgain reads one named signal again over the last four weeks, and
// puts what it found in the ledger (ledger.ReadAgain), within --timeout.
func (s *Server) signalAgain(ctx context.Context, d ledger.SignalDef, c criterion, o verdict.Options) (bool, error) {
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		return false, err
	}
	from, err := s.againFrom(ctx, cursor)
	if err != nil {
		return false, err
	}
	s.status.set("reading the impact signal %s again over the last four weeks", d.Name)
	rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	series, err := s.h.QueryRange(rctx, d.Expr, from, cursor, s.cfg.Step)
	var sels []ledger.SelectorData
	if err == nil {
		sels, err = s.firstSamples(rctx, d.Expr, from, cursor, true)
	}
	if err != nil {
		s.readSignal(d.Name, err)
		return false, err
	}
	windows := incident.FromSignal(series, s.cfg.Step, d.For, d.Name, d.Match)
	if most := maxSignalWeek * (int(cursor.Sub(from)/(7*24*time.Hour)) + 1); len(windows) > most {
		err := fmt.Errorf("more than %d windows a week: an expression by pod, not by service, finds too many", maxSignalWeek)
		s.readSignal(d.Name, err)
		return false, err
	}
	seen := verdict.NewSeen(d.MatchOr(o.Match))
	for _, se := range series {
		if len(se.Samples) > 0 {
			seen.Add(se.Labels)
		}
	}
	err = s.l.ReadAgain(ctx, ledger.ImpactRead{From: from, To: cursor, Criterion: c.key(), Windows: windows, Signal: d.Name, Match: d.Match, Seen: seen,
		Data: ledger.ImpactData{From: from, CheckedTo: cursor, Selectors: sels}}, o)
	if errors.Is(err, ledger.ErrRunsMoved) {
		return false, nil // read again at the next round, to where they are
	}
	if err != nil {
		return false, err
	}
	s.readSignal(d.Name, nil)
	return true, s.l.JudgePending(ctx, o)
}

// errLookCut is a look at what a signal reads that some of its selectors
// didn't answer.
var errLookCut = errors.New("some of the selectors it reads didn't answer")

// signalsSeen reads, for each named signal, what it reads, by the labels
// it is matched on (docs/design/signals.md, "The record, signal by
// signal"), from how far it was read for it to where the runs have got
// to, each in turn within --timeout (inTurn): each selector of its
// expression counted by those labels, one request a selector (seenFrom),
// and kept beside what it returned (ledger.AddSeen), which has the alerts
// it may tie otherwise worked out again. One that fails passes to the
// next, the last failure returned.
func (s *Server) signalsSeen(ctx context.Context, o verdict.Options) error {
	if !s.namedSignals() || s.h == nil {
		return nil
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.IsZero() {
		return err
	}
	states, err := s.l.SeenOf(ctx, o.Match)
	if err != nil {
		return err
	}
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		return err
	}
	more, to := map[string]*verdict.Seen{}, map[string]time.Time{}
	var mu sync.Mutex
	var asks []turnAsk
	for _, d := range s.cfg.Signals {
		i := slices.IndexFunc(rows, func(r ledger.SignalRow) bool { return r.Name == d.Name })
		if i < 0 || rows[i].Kept.Since.IsZero() {
			continue // not read yet
		}
		// From how far it was read for it, from its first read; begun
		// again (of other labels), from where its data was looked for, or
		// the record by it begins.
		lo := states[d.Name].To
		if lo.IsZero() {
			lo = rows[i].Kept.Since
			if dd := rows[i].Kept.Data; dd != nil && dd.From.Before(lo) {
				lo = dd.From
			}
		}
		if !cursor.After(lo) {
			continue
		}
		// Each in turn, within --timeout (inTurn): one whose count hangs
		// leaves the others' looks theirs.
		asks = append(asks, turnAsk{name: d.Name, ask: func(ctx context.Context) error {
			seen, complete, err := source.SeenFrom(ctx, s.h, d.Expr, d.MatchOr(o.Match), lo, cursor)
			mu.Lock()
			defer mu.Unlock()
			if seen != nil {
				more[d.Name] = seen // what the selectors that answered read
			}
			if err == nil && !complete {
				err = errLookCut
			}
			if err == nil {
				to[d.Name] = cursor
			}
			return err
		}})
	}
	var failed error
	for i, err := range s.inTurn(ctx, "seen", asks, nil) {
		if err != nil {
			// Looked at again from the same place next round.
			failed = fmt.Errorf("%s: %w", asks[i].name, err)
		}
	}
	if len(more) > 0 || len(to) > 0 {
		if err := s.l.AddSeen(context.WithoutCancel(ctx), o.Match, more, to); err != nil {
			return err
		}
	}
	return failed
}

// signalsHeld is, with named signals, what the windows the ledger holds
// were read with, as a backtest is judged against them (evidence.Impact,
// staleImpact): one digest of each signal's criterion held (or the label
// of a floor standing, heldPending), its match and whether it keeps the
// record, the rule entries, and the match in effect, so that a change to
// any of them holds the proposals it could change.
func (s *Server) signalsHeld(ctx context.Context) (string, error) {
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		return "", err
	}
	o, err := s.options(ctx)
	if err != nil {
		return "", err
	}
	type held struct {
		Name, Held string
		Match      []string
		Record     bool
	}
	var hs []held
	for _, d := range s.cfg.Signals {
		h := held{Name: d.Name, Match: d.Match, Record: d.Record}
		if i := slices.IndexFunc(rows, func(r ledger.SignalRow) bool { return r.Name == d.Name }); i >= 0 {
			k := rows[i].Kept
			h.Held = k.Criterion
			if !k.Floor.IsZero() {
				h.Held = heldPending(k)
			}
		}
		hs = append(hs, h)
	}
	slices.SortFunc(hs, func(a, b held) int { return cmp.Compare(a.Name, b.Name) })
	b, err := json.Marshal(struct {
		Signals []held
		Rules   []verdict.RuleEntry
		Match   []string
	}{hs, s.cfg.ImpactRules, o.Match})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "signals " + hex.EncodeToString(sum[:12]), nil
}

// ruleSignals is what a rule is judged against, with named signals
// (docs/design/signals.md, "Where it shows"): the place of the rule entry
// that names it, from 1, 0 for none; the signals whose windows it is
// judged against, each with the labels it is matched on; and the signals
// its record waits for, those that keep the record of the signals its
// alerts are tied to, with the latest start among them: no record by
// reading before it, and none at all when one has no start (Never).
type ruleSignals struct {
	Entry   int            `json:"rule_entry,omitempty"`
	Against []ruleSignalOn `json:"judged_against"`
	Waits   []string       `json:"waits_for"`
	// NoAlert says no rule entry names it and it has no alert yet: which
	// signals its record waits for comes with each alert, by its labels.
	NoAlert bool       `json:"no_alert_yet,omitempty"`
	From    *time.Time `json:"no_record_before,omitempty"`
	Never   bool       `json:"never,omitempty"`
}

type ruleSignalOn struct {
	Signal string   `json:"signal"`
	Match  []string `json:"matched_on"`
}

// ruleSignalsOf is ruleSignals of a rule, by o's named signals: nil with
// none named.
func (s *Server) ruleSignalsOf(ctx context.Context, o verdict.Options, rule string) (*ruleSignals, error) {
	sig := o.Signals
	if !sig.Named() {
		return nil, nil
	}
	out := &ruleSignals{Entry: sig.EntryNumber(rule), Against: []ruleSignalOn{}, Waits: []string{}}
	e := sig.Entry(rule)
	for i, x := range sig.List {
		if e != nil && !slices.Contains(e.Signals, x.Name) {
			continue
		}
		m := x.MatchOr(o.Match)
		if e != nil && e.Match != nil {
			m = e.Match
		}
		out.Against = append(out.Against, ruleSignalOn{Signal: sig.List[i].Name, Match: append([]string{}, m...)})
	}
	sets, err := s.l.RuleSignals(ctx, rule)
	if err != nil {
		return nil, err
	}
	if e != nil {
		sets = []int64{0}
	}
	out.NoAlert = len(sets) == 0
	var waits []int
	for _, b := range sets {
		for _, i := range sig.TiedOf(rule, b) {
			if sig.List[i].Record && !slices.Contains(waits, i) {
				waits = append(waits, i)
			}
		}
	}
	slices.Sort(waits)
	var latest time.Time
	for _, i := range waits {
		x := sig.List[i]
		out.Waits = append(out.Waits, x.Name)
		if x.Start.IsZero() {
			out.Never = true
		} else if x.Start.After(latest) {
			latest = x.Start
		}
	}
	if !out.Never && !latest.IsZero() {
		at := latest.UTC()
		out.From = &at
	}
	return out, nil
}
