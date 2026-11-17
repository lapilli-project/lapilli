package ledger

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// Named impact signals changed while the ledger's other writers work
// (docs/design/signals.md, "Step 3, as built"): what a run, a read again
// or a judge worked out before a change, written after it, undoes nothing
// of it, and is judged again by options read after it.

// noneNamedThenNamed records a window of the impact signal with no name,
// on at the run's end, then saves the first named signal: the one with no
// name made the signal impact, probe added, as the Settings page does.
func noneNamedThenNamed(t *testing.T, l *Ledger) incident.Incident {
	t.Helper()
	ctx := context.Background()
	at := t0.Add(50 * time.Minute)
	w := incident.Incident{ID: "impact-1", Candidate: true, Start: at, End: t0.Add(time.Hour), Labels: incident.Labels{"service": {"checkout"}}}
	f := Found{From: t0, To: t0.Add(time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, ImpactOn: true, ImpactSince: t0.Add(-time.Hour),
		ImpactCriterion: "5m impact > 1", Impact: []incident.Incident{w}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	defs := []SignalDef{{Name: "impact", Expr: "impact > 1", For: 5 * time.Minute, Record: true, SetBy: "ana"},
		{Name: "probe", Expr: "probe_success == 0", For: 2 * time.Minute, Record: true, SetBy: "ana"}}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "", To: "impact"}, fixed(SignalSet{Defs: defs}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	return w
}

func windowsBySignal(t *testing.T, l *Ledger) map[string][]Incident {
	t.Helper()
	ws, err := l.ImpactWindows(context.Background(), t0.Add(-30*24*time.Hour), t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]Incident{}
	for _, w := range ws {
		out[w.Signal] = append(out[w.Signal], w)
	}
	return out
}

func rowOf(t *testing.T, l *Ledger, name string) (SignalRow, bool) {
	t.Helper()
	rows, err := l.SignalRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(rows, func(r SignalRow) bool { return r.Name == name })
	if i < 0 {
		return SignalRow{}, false
	}
	return rows[i], true
}

// A run begun before the first named signal was saved, which read the one
// with no name, written after: it keeps nothing of it (no window of the
// signal with no name beside impact's, the row with no name not kept
// again), and impact, made of it, is read from where the runs had got to,
// as one missed, so that it catches up the hours that run read.
func TestSignalsUnnamedRunAfterNamedKeepsNothing(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	w := noneNamedThenNamed(t, l)
	if r, ok := rowOf(t, l, "impact"); !ok || !r.Missed.Equal(t0.Add(time.Hour)) {
		t.Errorf("impact, made of it, missed from %v: %+v", t0.Add(time.Hour), r)
	}
	f := Found{From: t0.Add(time.Hour), To: t0.Add(2 * time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, ImpactOn: true, ImpactSince: t0.Add(-time.Hour),
		ImpactCriterion: "5m impact > 1", Impact: []incident.Incident{{ID: "impact-1", Candidate: true, Start: w.Start, End: t0.Add(80 * time.Minute), Labels: w.Labels}}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if got := windowsBySignal(t, l); len(got[""]) != 0 || len(got["impact"]) != 1 {
		t.Errorf("windows: %d of the signal with no name, %d of impact", len(got[""]), len(got["impact"]))
	}
	k, err := l.SignalKept(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.ParseInt(k.Criterion, 10, 64); k.Criterion != "" || err == nil {
		t.Errorf("the row with no name keeps a record again: %+v", k)
	}
}

// The same of a read again of the one with no name under way: it keeps
// nothing, not four weeks of windows beside impact's.
func TestSignalsUnnamedReadAgainAfterNamedKeepsNothing(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	w := noneNamedThenNamed(t, l)
	err := l.ReadAgain(ctx, ImpactRead{From: t0.Add(-3 * 24 * time.Hour), To: t0.Add(time.Hour), Criterion: "5m impact > 1",
		Windows: []incident.Incident{{ID: "impact-1", Candidate: true, Start: w.Start, End: w.End, Labels: w.Labels}}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := windowsBySignal(t, l); len(got[""]) != 0 {
		t.Errorf("windows of the signal with no name: %d", len(got[""]))
	}
}

// The signals a write keeps are worked out from the settings as it reads
// them: the leader's round, which read the settings before a rename saved,
// keeps the rename; a run that read the old name, written after, keeps
// nothing of it, and the renamed one catches up from where it read.
func TestSignalsLeaderKeepsRename(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	probeWindows(t, l)
	before, _ := l.SignalKept(ctx, "probe")
	renamed := []SignalDef{signalDefs[0], {Name: "synthetic", Expr: signalDefs[1].Expr, For: signalDefs[1].For, Match: signalDefs[1].Match, Record: true, SetBy: "ana"}}
	// As lapilli serve works them out: the settings say probe is renamed.
	from := func(set map[string]Setting) (SignalSet, []string) {
		if _, ok := set["impact-signal/synthetic"]; ok {
			return SignalSet{Defs: renamed}, opts.Match
		}
		return SignalSet{Defs: signalDefs}, opts.Match
	}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "probe", To: "synthetic",
		Settings: map[string]string{"impact-signal/synthetic": "{}", "impact-signal/probe": "off"}}, from, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SyncSignalSet(ctx, from); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(90 * time.Minute)
	f := Found{From: t0.Add(time.Hour), To: t0.Add(2 * time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, Signals: []SignalRead{{
		Name: "probe", Criterion: "2m probe_success == 0", Since: t0, From: t0.Add(time.Hour), Match: []string{"service"},
		Windows: []incident.Incident{{ID: "x", Signal: "probe", Match: []string{"service"}, Start: at, End: at.Add(5 * time.Minute), Labels: incident.Labels{"service": {"cart"}}}}}}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SyncSignalSet(ctx, from); err != nil {
		t.Fatal(err)
	}
	after, _ := l.SignalKept(ctx, "synthetic")
	if !after.Since.Equal(before.Since) || after.Criterion != before.Criterion {
		t.Errorf("the renamed signal's record: %+v, was %+v", after, before)
	}
	if _, ok := rowOf(t, l, "probe"); ok {
		t.Error("probe's row made again")
	}
	if got := windowsBySignal(t, l); len(got["probe"]) != 0 {
		t.Errorf("windows of the old name: %+v", got["probe"])
	}
	// The run under way read it by its old name: the renamed one is read
	// from where the runs had got to when it was renamed.
	if r, ok := rowOf(t, l, "synthetic"); !ok || !r.Missed.Equal(t0.Add(time.Hour)) {
		t.Errorf("synthetic missed from %v: %+v", t0.Add(time.Hour), r.Missed)
	}
}

// A change worked out from settings changed since (two admins at once)
// is refused, and nothing of it written.
func TestSignalsChangePlannedOnStaleSettings(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	if err := l.SetSettings(ctx, map[string]string{"impact-signal/a": "{}"}, "bo"); err != nil {
		t.Fatal(err)
	}
	gen := l.Gen()
	_, err := l.ChangeSignals(ctx, SignalsChange{Settings: map[string]string{"impact-signal/b": "{}"},
		Planned: func(set map[string]Setting) bool { _, ok := set["impact-signal/a"]; return !ok }}, fixed(SignalSet{Defs: signalDefs}, opts.Match), "ana")
	if !errors.Is(err, ErrSignalsChanged) {
		t.Errorf("planned on settings changed since: %v", err)
	}
	if set, _ := l.Settings(ctx); set["impact-signal/b"].Value != "" || l.Gen() != gen {
		t.Errorf("written: %+v", set)
	}
}

// probeOverSearch records a probe window (checkout) and a firing of
// Errors on search during it, the probe matched on service: the firing
// catches nothing.
func probeOverSearch(t *testing.T, l *Ledger, o verdict.Options, match []string) {
	t.Helper()
	ctx := context.Background()
	at := t0.Add(10 * time.Minute)
	f := Found{From: t0, To: t0.Add(time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0},
		Firings: []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: at.Add(time.Minute), End: at.Add(2 * time.Minute)}},
		Signals: []SignalRead{{Name: "probe", Criterion: "2m probe_success == 0", Since: t0.Add(-time.Hour), From: t0, Match: match,
			Windows: []incident.Incident{{ID: "impact-probe-1", Signal: "probe", Match: match, Start: at, End: at.Add(10 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}}}}}}
	if err := l.Record(ctx, f, o); err != nil {
		t.Fatal(err)
	}
}

func searchWhite(t *testing.T, l *Ledger) bool {
	t.Helper()
	fs, err := l.Firings(context.Background(), FiringQuery{From: t0, To: t0.Add(time.Hour)})
	if err != nil || len(fs) != 1 {
		t.Fatalf("%+v %v", fs, err)
	}
	return fs[0].White
}

// A signal's match changed judges its windows on it: kept on each window
// (incidents.match_labels), it is changed with it, in the same write, so
// that the firings judged again are judged by it.
func TestSignalMatchChangeJudgesWindows(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	probeOverSearch(t, l, opts, []string{"service"})
	if searchWhite(t, l) {
		t.Fatal("caught before")
	}
	defs := []SignalDef{signalDefs[0], {Name: "probe", Expr: signalDefs[1].Expr, For: signalDefs[1].For, Match: []string{}, Record: true}}
	if got, err := l.ChangeSignals(ctx, SignalsChange{Settings: map[string]string{"impact-signal/probe": `{"match":[]}`}}, fixed(SignalSet{Defs: defs}, opts.Match), "ana"); err != nil || !got.Set {
		t.Fatalf("%+v %v", got, err)
	}
	o := opts
	o.Signals = verdict.NewSignals([]verdict.Signal{{Name: "errors", Bit: 0, Record: true}, {Name: "probe", Bit: 1, Match: []string{}, Record: true}}, nil)
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	if !searchWhite(t, l) {
		t.Error("the probe matched on no label: the search firing during its window catches it")
	}
	ws, _ := l.ImpactWindows(ctx, t0, t0.Add(time.Hour))
	for _, w := range ws {
		if w.Match == nil || len(w.Match) != 0 {
			t.Errorf("window %s matched on %v", w.ID, w.Match)
		}
	}
}

// Options read before the rule entries changed (the leader's round, the
// entries saved on another replica meanwhile) judge nothing the change
// pended: it is left, and options read after judge it.
func TestSignalsStaleOptionsLeavePending(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, []string{}); err != nil {
		t.Fatal(err)
	}
	o := opts
	o.Match = []string{}
	o.Signals = verdict.NewSignals([]verdict.Signal{{Name: "errors", Bit: 0, Record: true}, {Name: "probe", Bit: 1, Match: []string{"service"}, Record: true}}, nil)
	probeOverSearch(t, l, o, nil)
	if !searchWhite(t, l) {
		t.Fatal("not caught before")
	}
	entries := []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"errors"}}}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Settings: map[string]string{"impact-rules": `[{"rules":"^Errors$","signals":["errors"]}]`}},
		fixed(SignalSet{Defs: signalDefs, Entries: entries, EntriesUI: true}, []string{}), "ana"); err != nil {
		t.Fatal(err)
	}
	// Options read before, held to what the ledger keeps.
	if err := l.JudgePending(ctx, o); !errors.Is(err, ErrSignalsMoved) {
		t.Errorf("judged with options read before: %v", err)
	}
	// And by their generation, as lapilli serve reads them.
	byGen := o
	byGen.SignalsGen = 1
	if err := l.JudgePending(ctx, byGen); !errors.Is(err, ErrSignalsMoved) {
		t.Errorf("judged with options of an old generation: %v", err)
	}
	if v, _ := l.meta(ctx, l.r, "pending"); v == "" || v == "[]" {
		t.Fatal("what the change pended is gone")
	}
	fresh := o
	fresh.Signals = verdict.NewSignals(o.Signals.List, entries)
	if fresh.SignalsGen, _ = l.SignalsGen(ctx); fresh.SignalsGen <= 1 {
		t.Fatalf("generation %d", fresh.SignalsGen)
	}
	if err := l.JudgePending(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if searchWhite(t, l) {
		t.Error("Errors, judged against errors alone, catches the probe's window")
	}
}

// A rename while a run that read the old name is under way: the renamed
// signal is read from where the runs had got to, so that the window that
// run found by the old name, which it keeps nothing of, is found again.
func TestSignalRenameDuringRunMissed(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	probeWindows(t, l) // a run to t0+1h
	renamed := []SignalDef{signalDefs[0], {Name: "synthetic", Expr: signalDefs[1].Expr, For: signalDefs[1].For, Match: signalDefs[1].Match, Record: true, SetBy: "ana"}}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "probe", To: "synthetic"}, fixed(SignalSet{Defs: renamed}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(90 * time.Minute)
	f := Found{From: t0.Add(time.Hour), To: t0.Add(4 * time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, Signals: []SignalRead{{
		Name: "probe", Criterion: "2m probe_success == 0", Since: t0.Add(-time.Hour), From: t0.Add(time.Hour), Match: []string{"service"},
		Windows: []incident.Incident{{ID: "x", Signal: "probe", Match: []string{"service"}, Start: at, End: at.Add(5 * time.Minute), Labels: incident.Labels{"service": {"cart"}}}}}}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if r, ok := rowOf(t, l, "synthetic"); !ok || r.Missed.IsZero() || r.Missed.After(f.From) {
		t.Errorf("synthetic missed from %v: %+v", r.Missed, r)
	}
}

// The last named signal taken away, of the criterion of the one with no
// name in effect again (the one it was made, given back): its row and
// windows are the one with no name's again, keys and all, its record
// standing, every alert tied to none.
func TestSignalUnnamedGivenBack(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	noneNamedThenNamed(t, l)
	was, _ := l.SignalKept(ctx, "impact")
	keys := windowsBySignal(t, l)["impact"]
	if len(keys) != 1 {
		t.Fatalf("impact's windows: %+v", keys)
	}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "impact", To: ""}, fixed(SignalSet{}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	k, err := l.SignalKept(ctx, "")
	if err != nil || !k.Since.Equal(was.Since) || k.Criterion != was.Criterion {
		t.Errorf("the one with no name's record: %+v, was %+v (%v)", k, was, err)
	}
	got := windowsBySignal(t, l)
	if len(got[""]) != 1 || got[""][0].ID != keys[0].ID || len(got["impact"]) != 0 {
		t.Errorf("windows: %+v", got)
	}
	if rows, _ := l.SignalRows(ctx); slices.ContainsFunc(rows, func(r SignalRow) bool { return r.Name != "" }) {
		t.Errorf("named rows left: %+v", rows)
	}
	var tied int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM series WHERE signals != 0").Scan(&tied); err != nil || tied != 0 {
		t.Errorf("alerts tied: %d %v", tied, err)
	}
}

// A run written with options read before a change of the signals (one
// under way when it was saved) judges its own span with them, and pends
// it: options read after judge it again.
func TestSignalsStaleRunJudgedAgain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, []string{}); err != nil {
		t.Fatal(err)
	}
	stale := opts
	stale.Match = []string{}
	stale.Signals = verdict.NewSignals([]verdict.Signal{{Name: "errors", Bit: 0, Record: true}, {Name: "probe", Bit: 1, Match: []string{"service"}, Record: true}}, nil)
	if stale.SignalsGen, _ = l.SignalsGen(ctx); stale.SignalsGen == 0 {
		t.Fatal("no generation")
	}
	entries := []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"errors"}}}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Settings: map[string]string{"impact-rules": "[]"}},
		fixed(SignalSet{Defs: signalDefs, Entries: entries, EntriesUI: true}, []string{}), "ana"); err != nil {
		t.Fatal(err)
	}
	fresh := stale
	fresh.Signals = verdict.NewSignals(stale.Signals.List, entries)
	fresh.SignalsGen, _ = l.SignalsGen(ctx)
	if err := l.JudgePending(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	// The run under way, written now, with the options it read before.
	probeOverSearch(t, l, stale, nil)
	if !searchWhite(t, l) {
		t.Fatal("judged, as the run read them, by the options before: caught")
	}
	if err := l.JudgePending(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if searchWhite(t, l) {
		t.Error("judged again by the options after: Errors, against errors alone, catches the probe's window")
	}
}

// What SyncSignalSet keeps is worked out in its write: settings changed
// between its first read and its write (a rename saved meanwhile) are
// those it keeps, not those it read first.
func TestSignalsSyncWorkedOutInWrite(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	probeWindows(t, l)
	renamed := []SignalDef{signalDefs[0], {Name: "synthetic", Expr: signalDefs[1].Expr, For: signalDefs[1].For, Match: signalDefs[1].Match, Record: true}}
	of := func(set map[string]Setting) (SignalSet, []string) {
		if _, ok := set["impact-signal/synthetic"]; ok {
			return SignalSet{Defs: renamed}, opts.Match
		}
		// Not yet renamed, and a change of its own to keep (another
		// signal), which makes the round write.
		return SignalSet{Defs: append(slices.Clone(signalDefs), SignalDef{Name: "latency", Expr: "latency > 1", For: time.Minute, Record: true})}, opts.Match
	}
	first := true
	from := func(set map[string]Setting) (SignalSet, []string) {
		if first {
			first = false
			// The rename saved meanwhile, after the round read the settings.
			if _, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "probe", To: "synthetic",
				Settings: map[string]string{"impact-signal/synthetic": "{}"}}, of, "ana"); err != nil {
				t.Fatal(err)
			}
		}
		return of(set)
	}
	if _, err := l.SyncSignalSet(ctx, from); err != nil {
		t.Fatal(err)
	}
	if _, ok := rowOf(t, l, "synthetic"); !ok {
		t.Error("the rename undone")
	}
	if _, ok := rowOf(t, l, "probe"); ok {
		t.Error("probe made again")
	}
}

// A team's and a quiet window's labels are text, as a rule entry's are:
// a value that isn't valid UTF-8, or has a control character, is refused.
func TestLabelsText(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for _, v := range []string{"\xff\xfe", "a\x07b", "line\nbreak"} {
		_, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "Errors", Labels: incident.Labels{"service": {v}}}, "ana")
		var inv ErrInvalid
		if !errors.As(err, &inv) {
			t.Errorf("a team's label %q: %v", v, err)
		}
	}
	if _, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "Errors", Labels: incident.Labels{"service": {"checkout-ü"}}}, "ana"); err != nil {
		t.Errorf("text: %v", err)
	}
}

// The last named signal given back to the one with no name while a run of
// the named ones is under way: its read of the one given back, by name,
// written after, is the one with no name's, its windows and its record,
// not a run that didn't read it, which would turn it off.
func TestSignalUnnameDuringNamedRun(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	noneNamedThenNamed(t, l)
	if _, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "impact", To: ""}, fixed(SignalSet{}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	given, _ := l.SignalKept(ctx, "")
	at := t0.Add(80 * time.Minute)
	f := Found{From: t0.Add(time.Hour), To: t0.Add(2 * time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, Signals: []SignalRead{
		{Name: "impact", Criterion: "5m impact > 1", Since: t0, From: t0.Add(time.Hour),
			Windows: []incident.Incident{{ID: "x", Signal: "impact", Start: at, End: at.Add(10 * time.Minute), Labels: incident.Labels{"service": {"cart"}}}}},
		{Name: "probe", Criterion: "2m probe_success == 0", Since: t0.Add(time.Hour), From: t0.Add(time.Hour)}}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	after, _ := l.SignalKept(ctx, "")
	if !after.Since.Equal(given.Since) || after.Criterion != given.Criterion || !after.Floor.IsZero() || after.From().IsZero() != given.From().IsZero() {
		t.Errorf("the record given back undone by a run under way: %+v, was %+v", after, given)
	}
	got := windowsBySignal(t, l)
	if len(got[""]) != 2 || len(got["impact"]) != 0 || len(got["probe"]) != 0 {
		t.Errorf("windows: %+v", got)
	}
}

// Given back while it was missed, behind: the one with no name keeps no
// record from where it was missed until it is read again.
func TestSignalUnnameWhileMissed(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	noneNamedThenNamed(t, l)
	f := Found{From: t0.Add(time.Hour), To: t0.Add(3 * time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, Signals: []SignalRead{
		{Name: "impact", Criterion: "5m impact > 1", Since: t0.Add(-time.Hour), From: t0.Add(time.Hour), Missed: true}}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	r, _ := rowOf(t, l, "impact")
	if r.Missed.IsZero() {
		t.Fatal("not missed")
	}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "impact", To: ""}, fixed(SignalSet{}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	if k, _ := l.SignalKept(ctx, ""); !k.Floor.Equal(r.Missed) || !k.From().IsZero() {
		t.Errorf("the one with no name keeps the record over hours no run read: floor %v, from %v, missed %v", k.Floor, k.From(), r.Missed)
	}
}

// Given back, the windows found while it was named are keyed as the one
// with no name keys them, their comments with them, so that a read again
// knows them; a confirmation's incident keeps the labels it was
// confirmed with.
func TestSignalUnnameKeysAndConfirmations(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	noneNamedThenNamed(t, l)
	at := t0.Add(80 * time.Minute)
	f := Found{From: t0.Add(time.Hour), To: t0.Add(2 * time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, Signals: []SignalRead{
		{Name: "impact", Criterion: "5m impact > 1", Since: t0, From: t0.Add(time.Hour), Match: []string{"service"},
			Windows: []incident.Incident{{ID: "x", Signal: "impact", Match: []string{"service"}, Start: at, End: at.Add(10 * time.Minute), Labels: incident.Labels{"service": {"cart"}}}}}}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	var key string
	for _, w := range windowsBySignal(t, l)["impact"] {
		if strings.HasPrefix(w.ID, "impact-impact-") {
			key = w.ID
		}
	}
	if key == "" {
		t.Fatal("no window keyed by its name")
	}
	if _, err := l.PutComment(ctx, Comment{On: "incident", Key: key, Body: "c"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Decide(ctx, key, "confirmed", "", "ana", opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "impact", To: ""}, fixed(SignalSet{}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	plain := strings.Replace(key, "impact-impact-", "impact-", 1)
	if cs, err := l.Comments(ctx, "incident", plain); err != nil || len(cs) != 1 {
		t.Errorf("the comment, on %s: %+v %v", plain, cs, err)
	}
	all, _ := l.Incidents(ctx, t0.Add(-time.Hour), t0.Add(3*time.Hour), true)
	for _, in := range all {
		if in.Origin == "ui" && (in.Signal != "" || in.Match == nil) {
			t.Errorf("the confirmation's incident: signal %q, match %v", in.Signal, in.Match)
		}
	}
}

// Options of the generation in effect, whose signals were kept from
// before it (a cache), are stale too: the ledger holds them to the entries
// it keeps, and judges nothing with them.
func TestSignalsStaleSignalsOfTheGeneration(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	entries := []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"errors"}}}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Settings: map[string]string{"impact-rules": "[]"}},
		fixed(SignalSet{Defs: signalDefs, Entries: entries, EntriesUI: true}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	o := opts
	o.Signals = verdict.NewSignals([]verdict.Signal{{Name: "errors", Bit: 0, Record: true}, {Name: "probe", Bit: 1, Match: []string{"service"}, Record: true}}, nil)
	o.SignalsGen, _ = l.SignalsGen(ctx)
	if err := l.JudgePending(ctx, o); !errors.Is(err, ErrSignalsMoved) {
		t.Errorf("judged with the entries before, of the generation after: %v", err)
	}
}
