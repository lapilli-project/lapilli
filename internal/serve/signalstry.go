package serve

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// The try of a change to the named impact signals (docs/design/signals.md,
// "Where it shows"): what a signal's form, or the rule entries', would do
// over the last four weeks, written nowhere. It reads the signals the
// change gives a new criterion (added, or another expression or "for"),
// and what those given a new match read by its labels, as saving would
// have them read; shows each signal's windows apart, in effect and with
// it; and the verdict in effect and with all of them, judged in memory as
// a backtest is, each firing tied by what each signal has seen.

// signalsTry is a try's result: the period, why it couldn't be tried
// (Err, in the reader's language), whether it is what is in effect
// (Same), each signal with the change (Signals), the verdict in effect and
// with it (Rows), and the proposals waiting it would hold.
type signalsTry struct {
	From, To  time.Time
	Err       string
	Same      bool
	Signals   []signalTried
	Rows      []triedRow
	Stale     []triedProposal
	StaleMore int
	StaleN    int
}

// signalTried is a signal with a change tried: its name, and the name it
// had (Was, of one renamed: "" of the one with no name); whether it is
// added; read over the period (Read: a new criterion), its windows found
// (the first triedWindows, Found of them, Counted of them would count) and
// where the data of its selectors begins; or what it reads looked at by
// its labels (Seen: a new match). Now and With are its windows in the
// period in effect and with the change; StartNow and StartWith, from when
// it keeps the record.
type signalTried struct {
	Name, Was            string
	Renamed, Added       bool
	Read, Seen           bool
	Now, With            int
	Found, MoreWindows   int
	Windows              []triedWindow
	Data                 *dataView
	StartNow, StartWith  time.Time
	NoStartNow, NoRecord bool
}

// keepForm keeps a form tried as typed on the page: the form of the
// signal it changes, open, or the one that adds one.
func keepForm(v *criteriaView, f signalForm) {
	f.InEffect, f.LeadAbove = v.Add.InEffect, v.Add.LeadAbove // what a form posted doesn't give
	if f.Was == "" {
		v.Add, v.AddOpen = f, true
		return
	}
	for i := range v.Named {
		if v.Named[i].Name == f.Was {
			v.Named[i].Form, v.Named[i].Open = f, true
		}
	}
}

// trySignals tries the change the Settings page's form gives, an admin's,
// from this site: a signal's form (signal-try), or the rule entries'
// (rules-try). Checked as saving it is, then read over the last four
// weeks, within tryWithin and --max-samples, one heavy request at a time
// and impactTriesAnHour an admin; what can't be tried is said in its
// result, the form kept as typed.
func (s *Server) trySignals(r *http.Request, v *criteriaView) error {
	ctx := r.Context()
	if s.role(r) != admin {
		return s.forbidden(admin)
	}
	if !fromHere(r) {
		return errNotFromHere
	}
	q := r.URL.Query()
	l := lang(r)
	try := &signalsTry{}
	v.SignalsTried = try
	set, err := s.l.Settings(ctx)
	if err != nil {
		return err
	}
	who, at := s.user(r), time.Now().UTC().Truncate(time.Second)
	var p signalPlan
	if q.Get("rules-try") != "" {
		text := q.Get("rules")
		if v.Entries != nil {
			v.Entries.Text, v.Entries.Open = text, true
		}
		p, err = s.planEntries(set, text, false, who, at)
	} else {
		f := signalFormOf(q.Get)
		keepForm(v, f)
		p, err = s.planSignal(ctx, set, f, "save", who, at)
	}
	if err != nil {
		try.Err = message(l, err)
		return nil
	}
	return s.judgeSignalsTried(ctx, l, who, try, s.signalsIn(set), p)
}

// sameSignals reports whether two sets of named signals say the same.
func sameSignals(a, b signalsNow) bool {
	return len(a.Defs) == len(b.Defs) && sameEntries(a.Entries, b.Entries) &&
		!slices.ContainsFunc(a.Defs, func(d ledger.SignalDef) bool {
			x, ok := b.def(d.Name)
			return !ok || !sameSignal(d, x)
		})
}

// judgeSignalsTried works out a try of p, a change to n: each signal with
// it, the verdict over the period in effect and with it, and the
// proposals it would hold.
func (s *Server) judgeSignalsTried(ctx context.Context, l, who string, try *signalsTry, n signalsNow, p signalPlan) error {
	o, err := s.options(ctx)
	if err != nil {
		return err
	}
	if try.From, try.To, err = s.trySpan(ctx); err != nil {
		return err
	}
	from, to := try.From, try.To
	o.From, o.To = from, to
	if k, err := s.l.CountFirings(ctx, from, to); err != nil {
		return err
	} else if k > reportFirings {
		return bad(tooManyToTry, k, reportFirings)
	}
	// Each signal by the name its windows have in the ledger: one renamed,
	// by the one it had.
	was := func(name string) string {
		if from, ok := p.Renamed[name]; ok {
			return from
		}
		return name
	}
	renamedTo := map[string]string{}
	for to, from := range p.Renamed {
		renamedTo[from] = to
	}
	read := map[string]ledger.SignalDef{}
	for _, d := range p.Read {
		read[d.Name] = d
	}
	inEffect := map[string]verdict.Signal{}
	if o.Signals.Named() {
		for _, x := range o.Signals.List {
			inEffect[x.Name] = x
		}
	}
	// What it reads by its labels, of a signal whose match is new, or the
	// one with no name, which has seen nothing by any.
	seenAgain := func(d ledger.SignalDef) bool {
		if _, ok := read[d.Name]; ok {
			return false
		}
		prev, had := inEffect[was(d.Name)]
		return !had || !slices.Equal(prev.MatchOr(o.Match), d.MatchOr(o.Match))
	}
	// Every try is counted among the admin's reads an hour, whether it
	// reads Prometheus or judges the four weeks alone (the entries'): each
	// takes the heavy turn.
	if reads := len(read) > 0 || slices.ContainsFunc(p.After.Defs, seenAgain); reads && s.h == nil {
		try.Err = message(l, errNoHistory)
		return nil
	}
	if !s.takeTry(ctx, who) {
		return errImpactTries
	}
	done, err := s.busy(ctx)
	if err != nil {
		return err
	}
	defer done()

	wf, wt := s.l.UnitsOf(o, from, to)
	around, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(wf, wt), wt, o.UnknownLen)
	if err != nil {
		return err
	}
	var written []incident.Incident
	windowsOf := map[string][]incident.Incident{} // the ledger's in the period, by signal
	for _, in := range around {
		switch {
		case in.Origin != "impact":
			written = append(written, in.Incident)
		case !in.Start.Before(from) && in.Start.Before(to):
			windowsOf[in.Signal] = append(windowsOf[in.Signal], in.Incident)
		}
	}
	unnamed, err := s.l.ImpactKept(ctx)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, min(s.cfg.Timeout, tryWithin))
	defer cancel()
	held := map[string]string{}
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		held[r.Name] = r.Kept.Criterion
		if !r.Kept.Floor.IsZero() {
			held[r.Name] = heldPending(r.Kept)
		}
	}
	heldWith := map[string]string{}
	list := make([]verdict.Signal, 0, len(p.After.Defs))
	counting := map[string][]incident.Incident{}
	failed := func(name string, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		try.Err = tr(l, "Prometheus couldn't read %s within %s and --max-samples: %s", name, verdict.Duration(min(s.cfg.Timeout, tryWithin)), promText(err))
		return nil
	}
	for _, d := range p.After.Defs {
		w := was(d.Name)
		x := verdict.Signal{Name: d.Name, For: d.For, Match: d.Match, Record: d.Record, Lead: d.Lead, Bit: -1}
		t := signalTried{Name: d.Name, Now: len(windowsOf[w])}
		prev, had := inEffect[w]
		switch _, renamed := p.Renamed[d.Name]; {
		case renamed:
			t.Was, t.Renamed = w, true
		case !had:
			t.Added = true
		}
		if had {
			x.Start, x.Seen, x.Bit = prev.Start, prev.Seen, prev.Bit
			t.StartNow = prev.Start
		} else if t.Renamed && w == "" {
			x.Start = unnamed.From()
			t.StartNow = x.Start
		}
		t.NoStartNow = t.StartNow.IsZero()
		heldWith[d.Name] = held[w]
		switch {
		case read[d.Name].Name != "":
			// Read as the criterion with no name's try reads it (readSpan),
			// within the same bounds, and as saving it reads it first: read
			// in full, it stands for saving it within triedFor.
			c := signalCriterion(d)
			series, data, err := s.readSpan(rctx, c, from, to)
			if err != nil {
				return failed(d.Name, err)
			}
			s.l.Floor(&data)
			t.Read, t.Data = true, dataViewOf(&data)
			x.Start = time.Time{}
			if !data.NoData {
				x.Start = later(from, data.Floor)
			}
			seen := verdict.NewSeen(d.MatchOr(o.Match))
			for _, se := range series {
				if len(se.Samples) > 0 {
					seen.Add(se.Labels)
				}
			}
			if more, _, err := source.SeenFrom(rctx, s.h, d.Expr, d.MatchOr(o.Match), from, to); err == nil {
				seen.Merge(more)
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
			x.Seen = seen
			found, err := s.l.DecidedSignal(ctx, w, from)
			if err != nil {
				return err
			}
			ws, err := incident.FromSignal(rctx, series, s.cfg.Step, d.For, d.Name, d.Match)
			if err != nil {
				return failed(d.Name, err)
			}
			// What a save couldn't keep, a try doesn't show: past the
			// windows a read again keeps in a week, it is said.
			if most := maxSignalWeek * (int(to.Sub(from)/(7*24*time.Hour)) + 1); len(ws) > most {
				try.Err = tr(l, "%s finds more than %d windows a week: an expression by pod, not by service, finds too many", d.Name, maxSignalWeek)
				return nil
			}
			t.Found = len(ws)
			if t.Windows, t.MoreWindows, counting[d.Name], err = windowsTried(ctx, ws, written, found, from, o); err != nil {
				return err
			}
			t.With = len(counting[d.Name])
			heldWith[d.Name] = c.key()
			s.tried.put(c.key(), time.Now())
		case seenAgain(d):
			seen, _, err := source.SeenFrom(rctx, s.h, d.Expr, d.MatchOr(o.Match), from, to)
			if err != nil {
				return failed(d.Name, err)
			}
			for _, in := range windowsOf[w] {
				for k, vs := range in.Labels {
					for _, v := range vs {
						seen.Add(map[string]string{k: v})
					}
				}
			}
			x.Seen, t.Seen, t.With = seen, true, t.Now
		default:
			t.With = t.Now
		}
		t.StartWith, t.NoRecord = x.Start, !d.Record
		list = append(list, x)
		try.Signals = append(try.Signals, t)
	}
	with := o
	with.ImpactFrom = time.Time{}
	with.Signals = verdict.NewSignals(list, p.After.Entries)
	if len(p.After.Entries) > 0 {
		own, err := s.owners(ctx)
		if err != nil {
			return err
		}
		with.Signals = with.Signals.Index(own.ruleLabels)
	}
	// The incidents with it: a window of a signal given a new criterion
	// gives way, from the period's start, to those it would count; one
	// renamed is its new name's.
	withIncs := make([]incident.Incident, 0, len(around))
	nowIncs := make([]incident.Incident, 0, len(around))
	matchAfter := map[string][]string{}
	for _, d := range p.After.Defs {
		matchAfter[d.Name] = d.Match
	}
	for _, in := range around {
		nowIncs = append(nowIncs, in.Incident)
		x := in.Incident
		if in.Origin == "impact" {
			if to, ok := renamedTo[x.Signal]; ok {
				x.Signal = to
			}
			// Matched on its signal's labels with the change, as saving it
			// matches them (ledger.SyncSignalSet).
			if m, ok := matchAfter[x.Signal]; ok && x.Signal != "" {
				x.Match = m
			}
			if _, ok := read[x.Signal]; ok {
				if !x.Start.Before(from) {
					continue
				}
				if x.End.After(from) {
					x.End = from
				}
			}
		}
		withIncs = append(withIncs, x)
	}
	for _, d := range p.After.Defs {
		withIncs = append(withIncs, counting[d.Name]...)
	}
	if sameSignals(n, p.After) {
		try.Same = true
		with, withIncs = o, nowIncs
	}

	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
	if err != nil {
		return err
	}
	plain := make([]firing.Firing, 0, len(fs))
	live := make([]ledger.Firing, 0, len(fs))
	for _, f := range fs {
		if !f.Excused {
			plain = append(plain, f.Firing)
			live = append(live, f)
		}
	}
	// With rules noisy by what was done about their firings, the preview
	// asks the same test as every surface, so it is given the responses.
	if rsp, err := s.previewResponses(ctx, live, o); err != nil {
		return err
	} else if rsp != nil {
		o.Responder, with.Responder = rsp, rsp
	}
	// People's verdicts, on both sides (docs/design/judging.md, item 3).
	said, err := s.l.ReplayVerdicts(ctx, from, to)
	if err != nil {
		return err
	}
	o.Verdicts, with.Verdicts = said, said
	o, with = s.graphed(ctx, o, nowIncs), s.graphed(ctx, with, withIncs)
	nowRep := verdict.Judge(nil, plain, nowIncs, o)
	withRep := verdict.Judge(nil, plain, withIncs, with)
	units := s.l.SpanUnits(o, from, to)
	a := triedSumOf(nowRep, verdict.RecordOf(nowIncs, o), o, units, s.l.Midnight, from, to)
	b := triedSumOf(withRep, verdict.RecordOf(withIncs, with), with, units, s.l.Midnight, from, to)
	row := func(label, x, y string) {
		try.Rows = append(try.Rows, triedRow{Label: label, Now: x, With: y, Changed: x != y})
	}
	of := func(n, m int) string { return tr(l, "%d of %d", n, m) }
	row("Incidents judged against", fmt.Sprint(a.incidents), fmt.Sprint(b.incidents))
	row("Of them, impact windows", fmt.Sprint(a.impact), fmt.Sprint(b.impact))
	row("Firings judged", of(a.judged, a.firings), of(b.judged, b.firings))
	row("Firings with no record", fmt.Sprint(a.firings-a.judged), fmt.Sprint(b.firings-b.judged))
	row("Precision", pct(a.white, a.judged), pct(b.white, b.judged))
	row("Noisy rules", fmt.Sprint(a.noisy), fmt.Sprint(b.noisy))
	row("Incidents missed", fmt.Sprint(a.missed), fmt.Sprint(b.missed))
	if try.Same {
		return nil
	}
	key := heldDigest(p.After.Defs, p.After.Entries, heldWith, with.Match)
	try.StaleN, try.Stale, try.StaleMore, err = s.staleBy(ctx, key, with, func(around []ledger.Incident) []incident.Incident {
		out := make([]incident.Incident, 0, len(around))
		for _, in := range around {
			x := in.Incident
			if in.Origin == "impact" {
				if to, ok := renamedTo[x.Signal]; ok {
					x.Signal = to
				}
				if _, ok := read[x.Signal]; ok && !x.Start.Before(from) {
					continue
				}
			}
			out = append(out, x)
		}
		for _, d := range p.After.Defs {
			out = append(out, counting[d.Name]...)
		}
		return out
	})
	return err
}
