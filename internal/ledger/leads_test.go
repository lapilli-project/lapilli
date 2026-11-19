package ledger

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// A lead of a signal's or a rule entry's own (docs/design/judging.md, item
// 1): judged by the ledger as verdict.Judge judges it, and a change to a
// lead alone judging again the spans of the windows it touches, no more.

func lead(d time.Duration) *time.Duration { return &d }

// leadsWorld records a day: a window of errors over checkout at 10:00 and
// one of the probe over search at 16:00, an incident written down over
// cart at 20:00, and a firing 40 minutes before each, of MemoryPressure
// (a cause), and Search and Cart; and one of MemoryPressure with no
// service, 20 minutes before errors' window.
func leadsWorld(t *testing.T, l *Ledger) []firing.Firing {
	t.Helper()
	ctx := context.Background()
	errorsAt, probeAt, cartAt := t0.Add(10*time.Hour), t0.Add(16*time.Hour), t0.Add(20*time.Hour)
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Start: cartAt, End: cartAt.Add(30 * time.Minute), Labels: incident.Labels{"service": {"cart"}}}}, opts); err != nil {
		t.Fatal(err)
	}
	read := func(name string, match []string, at time.Time, service string) SignalRead {
		return SignalRead{Name: name, Criterion: name, Since: t0.Add(-time.Hour), From: t0, Match: match,
			Windows: []incident.Incident{{ID: "impact-" + name + "-1", Signal: name, Match: match, Start: at, End: at.Add(20 * time.Minute), Labels: incident.Labels{"service": {service}}}}}
	}
	fs := []firing.Firing{
		{Rule: "MemoryPressure", Labels: map[string]string{"service": "checkout"}, Start: errorsAt.Add(-40 * time.Minute), End: errorsAt.Add(-30 * time.Minute)},
		{Rule: "MemoryPressure", Labels: map[string]string{"node": "n1"}, Start: errorsAt.Add(-20 * time.Minute), End: errorsAt.Add(-10 * time.Minute)},
		{Rule: "Search", Labels: map[string]string{"service": "search"}, Start: probeAt.Add(-40 * time.Minute), End: probeAt.Add(-30 * time.Minute)},
		{Rule: "Cart", Labels: map[string]string{"service": "cart"}, Start: cartAt.Add(-40 * time.Minute), End: cartAt.Add(-30 * time.Minute)},
	}
	f := Found{From: t0, To: t0.Add(24 * time.Hour), Firings: fs, Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(24 * time.Hour)},
		Signals: []SignalRead{read("errors", nil, errorsAt, "checkout"), read("probe", []string{"service"}, probeAt, "search")}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	return fs
}

// leadsOpts are the options of the signals and entries the ledger keeps,
// as lapilli serve reads them: of its generation, each with its bit.
func leadsOpts(t *testing.T, l *Ledger, defs []SignalDef, entries []verdict.RuleEntry) verdict.Options {
	t.Helper()
	o := opts
	var list []verdict.Signal
	for i, d := range defs {
		list = append(list, verdict.Signal{Name: d.Name, For: d.For, Match: d.Match, Record: d.Record, Lead: d.Lead, Bit: i})
	}
	o.Signals = verdict.NewSignals(list, entries)
	var err error
	if o.SignalsGen, err = l.SignalsGen(context.Background()); err != nil {
		t.Fatal(err)
	}
	return o
}

// stonesOf are the ledger's stones of the day, a firing each: its rule,
// labels, and the incidents it caught.
func stonesOf(t *testing.T, l *Ledger, o verdict.Options) []string {
	t.Helper()
	rep, err := l.Report(context.Background(), t0, t0.Add(24*time.Hour), o, true)
	if err != nil {
		t.Fatal(err)
	}
	return judgedStones(rep.Firings)
}

func judgedStones(js []verdict.Judged) []string {
	var out []string
	for _, j := range js {
		out = append(out, fmt.Sprintf("%s %v %v", j.Rule, j.Labels, j.Incidents))
	}
	slices.Sort(out)
	return out
}

// A change to a lead alone pends the spans of the windows it touches, from
// the longer lead before each to its end, in the same write that keeps it,
// counting a generation of the signals: an entry's lead, its signals'
// windows; a signal's, its own; and nothing of the other signal's windows
// or of the incident written down. Judged again, the stones are those
// verdict.Judge gives the same firings and incidents with the same
// options; and written incidents keep --lead.
func TestLeadChangePendsItsWindows(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	entries := []verdict.RuleEntry{{Rules: "^MemoryPressure$", Signals: []string{"errors"}}}
	if _, err := l.SyncSignals(ctx, signalDefs, entries, opts.Match, opts.Lead); err != nil {
		t.Fatal(err)
	}
	fs := leadsWorld(t, l)
	o := leadsOpts(t, l, signalDefs, entries)
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	want := []string{"Cart map[service:cart] []", "MemoryPressure map[node:n1] []", "MemoryPressure map[service:checkout] []", "Search map[service:search] []"}
	if got := stonesOf(t, l, o); !slices.Equal(got, want) {
		t.Fatalf("before any lead: %q", got)
	}
	var spans [][2]int64
	pendedLeads = func(s [][2]int64) { spans = s }
	defer func() { pendedLeads = nil }()
	gen := o.SignalsGen

	// An entry's lead: errors' window alone, from an hour before it.
	withLead := []verdict.RuleEntry{{Rules: "^MemoryPressure$", Signals: []string{"errors"}, Lead: lead(time.Hour)}}
	got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: signalDefs, Entries: withLead, Lead: opts.Lead}, opts.Match))
	if err != nil {
		t.Fatal(err)
	}
	errorsAt := t0.Add(10 * time.Hour)
	if got != (SignalsSynced{Leads: true}) || !slices.Equal(spans, [][2]int64{{errorsAt.Add(-time.Hour).Unix(), errorsAt.Add(20 * time.Minute).Unix()}}) {
		t.Fatalf("an entry's lead changed: %+v, pended %v", got, spans)
	}
	if es, err := l.RuleEntries(ctx); err != nil || len(es) != 1 || es[0].Lead == nil || *es[0].Lead != time.Hour {
		t.Fatalf("the entry kept: %+v %v", es, err)
	}
	if err := l.JudgePending(ctx, o); err == nil {
		t.Error("judged with options read before the lead changed")
	}
	o = leadsOpts(t, l, signalDefs, withLead)
	if o.SignalsGen <= gen {
		t.Errorf("generation %d, was %d", o.SignalsGen, gen)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	// The cause 40 minutes before errors' window is white now; the one
	// with no service too, as no entry requires it; Cart, 40 minutes
	// before an incident written down, stays black: --lead.
	want = []string{"Cart map[service:cart] []", "MemoryPressure map[node:n1] [impact-errors-20261102T1000Z]", "MemoryPressure map[service:checkout] [impact-errors-20261102T1000Z]", "Search map[service:search] []"}
	if got := stonesOf(t, l, o); !slices.Equal(got, want) {
		t.Errorf("with the entry's lead: %q", got)
	}
	incs, err := l.RecordIncidents(ctx, t0.Add(-time.Hour), t0.Add(25*time.Hour), opts.UnknownLen)
	if err != nil {
		t.Fatal(err)
	}
	var plain []incident.Incident
	for _, in := range incs {
		plain = append(plain, in.Incident)
	}
	vo := o
	vo.From, vo.To = t0, t0.Add(24*time.Hour)
	if v := judgedStones(verdict.Judge(nil, slices.Clone(fs), plain, vo).Firings); !slices.Equal(v, want) {
		t.Errorf("verdict.Judge: %q, the ledger %q", v, want)
	}

	// A signal's lead: the probe's window alone, from the longer of the
	// two, its own 1h now, before it.
	defs := slices.Clone(signalDefs)
	defs[1].Lead = lead(time.Hour)
	spans = nil
	if got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: defs, Entries: withLead, Lead: opts.Lead}, opts.Match)); err != nil || got != (SignalsSynced{Leads: true}) {
		t.Fatalf("a signal's lead changed: %+v %v", got, err)
	}
	probeAt := t0.Add(16 * time.Hour)
	if !slices.Equal(spans, [][2]int64{{probeAt.Add(-time.Hour).Unix(), probeAt.Add(20 * time.Minute).Unix()}}) {
		t.Errorf("pended %v, want the probe's window alone", spans)
	}
	o = leadsOpts(t, l, defs, withLead)
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	want[3] = "Search map[service:search] [impact-probe-20261102T1600Z]"
	if got := stonesOf(t, l, o); !slices.Equal(got, want) {
		t.Errorf("with the probe's lead: %q", got)
	}

	// require: on the entry is a change to it, as its match is: every
	// firing judged again. The alert with no service catches nothing.
	required := []verdict.RuleEntry{{Rules: "^MemoryPressure$", Signals: []string{"errors"}, Lead: lead(time.Hour), Require: []string{"service"}}}
	if got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: defs, Entries: required, Lead: opts.Lead}, opts.Match)); err != nil || !got.Entries {
		t.Fatalf("require added: %+v %v", got, err)
	}
	if es, err := l.RuleEntries(ctx); err != nil || len(es) != 1 || !slices.Equal(es[0].Require, []string{"service"}) {
		t.Fatalf("the entry kept: %+v %v", es, err)
	}
	o = leadsOpts(t, l, defs, required)
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	want[1] = "MemoryPressure map[node:n1] []"
	if got := stonesOf(t, l, o); !slices.Equal(got, want) {
		t.Errorf("with require: %q", got)
	}
	// Kept as they are, a round changes nothing.
	if got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: defs, Entries: required, Lead: opts.Lead}, opts.Match)); err != nil || got != (SignalsSynced{}) {
		t.Errorf("the same again: %+v %v", got, err)
	}
}
