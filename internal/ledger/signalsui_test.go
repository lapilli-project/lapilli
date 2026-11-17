package ledger

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// Named impact signals set on the Settings page (docs/design/signals.md,
// step 3).

// probeWindows records two windows of the probe, decides the first, and
// keeps a task and a comment on it: its key.
func probeWindows(t *testing.T, l *Ledger) string {
	t.Helper()
	ctx := context.Background()
	at := t0.Add(10 * time.Minute)
	f := Found{From: t0, To: t0.Add(time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, Signals: []SignalRead{{
		Name: "probe", Criterion: "2m probe_success == 0", Since: t0.Add(-time.Hour), From: t0, Match: []string{"service"},
		Windows: []incident.Incident{
			{ID: "impact-probe-1", Signal: "probe", Match: []string{"service"}, Start: at, End: at.Add(5 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}},
			{ID: "impact-probe-2", Signal: "probe", Match: []string{"service"}, Start: at.Add(30 * time.Minute), End: at.Add(35 * time.Minute), Labels: incident.Labels{"service": {"cart"}}},
		},
	}}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	ws, err := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour))
	if err != nil || len(ws) != 2 {
		t.Fatalf("windows: %+v %v", ws, err)
	}
	key := ws[0].ID
	if _, err := l.Decide(ctx, key, "confirmed", "", "ana", opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutTask(ctx, Task{Title: "look", Kind: "decide", Incident: key, Status: "open"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutComment(ctx, Comment{On: "incident", Key: key, Body: "a deploy"}, "ana"); err != nil {
		t.Fatal(err)
	}
	return key
}

// A signal renamed in place keeps its row, bit and what keeps its record;
// its windows are renamed, keys and all, with what was decided of them and
// the tasks and comments on them; and the incident a confirmation of one
// wrote carries the new name. The same write keeps the settings it sets
// and notes the activity log, and nothing is judged again: the set's
// match, record and entries are as they were.
func TestRenameSignal(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	key := probeWindows(t, l)
	before, err := l.SignalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(time.Now().Unix(), 0).UTC()
	defs := []SignalDef{signalDefs[0], {Name: "synthetic", Expr: signalDefs[1].Expr, For: signalDefs[1].For, Match: signalDefs[1].Match, Record: true, SetBy: "ana", SetAt: at}}
	got, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "probe", To: "synthetic",
		Settings: map[string]string{"impact-signal/synthetic": "{}", "impact-signal/probe": ""},
		Log:      []Logged{{What: "setting impact signal synthetic", Before: map[string]string{"name": "probe"}, After: map[string]string{"name": "synthetic"}}}}, fixed(SignalSet{Defs: defs}, opts.Match), "ana")
	if err != nil {
		t.Fatal(err)
	}
	if got.Set || got.Entries {
		t.Errorf("a rename judged again: %+v", got)
	}
	rows, err := l.SignalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bit := func(rows []SignalRow, name string) (SignalRow, bool) {
		i := slices.IndexFunc(rows, func(r SignalRow) bool { return r.Name == name })
		if i < 0 {
			return SignalRow{}, false
		}
		return rows[i], true
	}
	was, _ := bit(before, "probe")
	now, ok := bit(rows, "synthetic")
	if _, still := bit(rows, "probe"); !ok || still || now.Bit != was.Bit || now.Kept.Criterion != was.Kept.Criterion || now.SetBy != "ana" || !now.SetAt.Equal(at) {
		t.Fatalf("rows: %+v", rows)
	}
	ws, err := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour))
	if err != nil || len(ws) != 2 {
		t.Fatal(ws, err)
	}
	renamed := "impact-synthetic-" + key[len("impact-probe-"):]
	for _, w := range ws {
		if w.Signal != "synthetic" || len(w.ID) < len("impact-synthetic-") || w.ID[:len("impact-synthetic-")] != "impact-synthetic-" {
			t.Errorf("window %s of %q", w.ID, w.Signal)
		}
		if w.ID == renamed && w.State != "confirmed" {
			t.Errorf("what was decided of %s: %q", w.ID, w.State)
		}
	}
	if ts, err := l.Tasks(ctx, TaskQuery{Incident: renamed}); err != nil || len(ts) != 1 {
		t.Errorf("the task on it: %+v %v", ts, err)
	}
	if cs, err := l.Comments(ctx, "incident", renamed); err != nil || len(cs) != 1 {
		t.Errorf("the comment on it: %+v %v", cs, err)
	}
	all, err := l.Incidents(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour), true)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := slices.IndexFunc(all, func(in Incident) bool { return in.Origin == "ui" })
	if confirmed < 0 || all[confirmed].Signal != "synthetic" {
		t.Errorf("the confirmation's incident: %+v", all)
	}
	if orphans, err := l.OrphanedImpact(ctx); err != nil || len(orphans) != 0 {
		t.Errorf("left behind: %+v %v", orphans, err)
	}
	set, err := l.Settings(ctx)
	if _, gone := set["impact-signal/probe"]; err != nil || set["impact-signal/synthetic"].By != "ana" || gone {
		t.Errorf("settings: %+v %v", set, err)
	}
	cs, err := l.Changes(ctx, 10)
	if err != nil || len(cs) == 0 || cs[0].What != "setting impact signal synthetic" || cs[0].Before == "" || cs[0].After == "" {
		t.Errorf("activity: %+v %v", cs, err)
	}
	// Renamed back to probe, or to errors, which has a row: errors is
	// refused, and nothing is written.
	gen := l.Gen()
	_, err = l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "synthetic", To: "errors", Settings: map[string]string{"impact-signal/x": "{}"}}, fixed(SignalSet{Defs: defs}, opts.Match), "ana")
	var inv ErrInvalid
	if !errors.As(err, &inv) {
		t.Errorf("renamed to a name kept: %v", err)
	}
	if set, _ := l.Settings(ctx); set["impact-signal/x"].Value != "" || l.Gen() != gen {
		t.Errorf("a refused rename wrote: %+v", set)
	}
}

// A signal can't be renamed to the name of one taken away whose windows
// the ledger keeps: its windows and the renamed one's would be one
// signal's.
func TestRenameSignalToKeptWindows(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	probeWindows(t, l)
	if _, err := l.SyncSignals(ctx, signalDefs[:1], nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	if kept, err := l.SignalNamesKept(ctx); err != nil || !kept["probe"] || !kept["errors"] {
		t.Fatalf("kept: %v %v", kept, err)
	}
	_, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "errors", To: "probe"}, fixed(SignalSet{Defs: []SignalDef{{Name: "probe", Expr: "x", For: time.Minute, Record: true}}}, opts.Match), "ana")
	var inv ErrInvalid
	if !errors.As(err, &inv) {
		t.Errorf("renamed onto a signal's windows: %v", err)
	}
}

// The impact signal with no name, made a named one by the first named
// signal saved in the UI: its row given the lowest bit free, no other
// signal's; its windows keep their keys, and a read again of it knows them
// by them still.
func TestRenameUnnamedSignal(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(10 * time.Minute)
	w := incident.Incident{ID: "impact-1", Candidate: true, Start: at, End: at.Add(10 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}}
	f := Found{From: t0, To: t0.Add(time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, ImpactOn: true, ImpactSince: t0.Add(-time.Hour),
		ImpactCriterion: "5m impact > 1", Impact: []incident.Incident{w}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	ws, err := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour))
	if err != nil || len(ws) != 1 || ws[0].Signal != "" {
		t.Fatalf("%+v %v", ws, err)
	}
	key := ws[0].ID
	defs := []SignalDef{{Name: "impact", Expr: "impact > 1", For: 5 * time.Minute, Record: true, SetBy: "ana"}, {Name: "probe", Expr: "probe_success == 0", For: 2 * time.Minute, Record: true, SetBy: "ana"}}
	got, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, From: "", To: "impact"}, fixed(SignalSet{Defs: defs}, opts.Match), "ana")
	if err != nil || !got.Set {
		t.Fatalf("%+v %v", got, err)
	}
	rows, err := l.SignalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bits := map[string]int{}
	for _, r := range rows {
		bits[r.Name] = r.Bit
	}
	if _, ok := bits[""]; ok || len(bits) != 2 || bits["impact"] == bits["probe"] {
		t.Fatalf("rows: %+v", rows)
	}
	if k, err := l.SignalKept(ctx, "impact"); err != nil || k.Criterion != "5m impact > 1" {
		t.Errorf("what keeps its record: %+v %v", k, err)
	}
	ws, err = l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour))
	if err != nil || len(ws) != 1 || ws[0].ID != key || ws[0].Signal != "impact" {
		t.Fatalf("its window: %+v %v", ws, err)
	}
	// Read again, the same window keeps its key.
	err = l.ReadAgain(ctx, ImpactRead{From: t0, To: t0.Add(time.Hour), Criterion: "5m impact > 1", Signal: "impact",
		Windows: []incident.Incident{{ID: "impact-impact-1", Signal: "impact", Candidate: true, Start: w.Start, End: w.End, Labels: w.Labels}}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if ws, err := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour)); err != nil || len(ws) != 1 || ws[0].ID != key {
		t.Errorf("read again: %+v %v", ws, err)
	}
}

// Who set a signal in the UI, and whether the entries are the UI's, are
// kept beside them, and judge nothing again.
func TestSignalSetUI(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	entries := []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe"}}}
	if _, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: signalDefs, Entries: entries}, opts.Match)); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(time.Now().Unix(), 0).UTC()
	defs := slices.Clone(signalDefs)
	defs[1].SetBy, defs[1].SetAt = "ana", at
	got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: defs, Entries: entries, EntriesUI: true}, opts.Match))
	if err != nil || got.Set || got.Entries {
		t.Fatalf("%+v %v", got, err)
	}
	gen := l.Gen()
	if _, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: defs, Entries: entries, EntriesUI: true}, opts.Match)); err != nil || l.Gen() != gen {
		t.Errorf("unchanged, written: %v", err)
	}
	rows, _ := l.SignalRows(ctx)
	i := slices.IndexFunc(rows, func(r SignalRow) bool { return r.Name == "probe" })
	if i < 0 || rows[i].SetBy != "ana" || !rows[i].SetAt.Equal(at) {
		t.Errorf("rows: %+v", rows)
	}
	var ui int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM impact_rules WHERE ui != 0").Scan(&ui); err != nil || ui != 1 {
		t.Errorf("entries the UI's: %d %v", ui, err)
	}
}

// Each signal's windows since a time are counted apart.
func TestSignalWindowsSince(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	probeWindows(t, l)
	if got, err := l.SignalWindowsSince(ctx, t0); err != nil || got["probe"] != 2 || got["errors"] != 0 {
		t.Errorf("%v %v", got, err)
	}
	if got, err := l.DecidedSignal(ctx, "probe", t0); err != nil || len(got) != 1 {
		t.Errorf("decided: %+v %v", got, err)
	}
}

// The signal with no name made a named one takes a bit no named signal
// has: one beside it, kept already, keeps its own.
func TestRenameUnnamedBesideNamed(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs[:1], nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	if err := l.write(ctx, func(t *txn) error { return setSigMeta(t, "", metaImpactCriterion, "5m impact > 1") }); err != nil {
		t.Fatal(err)
	}
	defs := []SignalDef{signalDefs[0], {Name: "impact", Expr: "impact > 1", For: 5 * time.Minute, Record: true, SetBy: "ana"}}
	if _, err := l.ChangeSignals(ctx, SignalsChange{Rename: true, To: "impact"}, fixed(SignalSet{Defs: defs}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	rows, err := l.SignalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bits := map[int]string{}
	for _, r := range rows {
		if other, ok := bits[r.Bit]; ok {
			t.Errorf("%s and %s share bit %d", r.Name, other, r.Bit)
		}
		bits[r.Bit] = r.Name
	}
	if len(rows) != 2 {
		t.Errorf("rows: %+v", rows)
	}
}

// fixed is a set of signals as given, whatever the settings say: the
// command line's, with nothing set in the UI.
func fixed(set SignalSet, match []string) SignalsFrom {
	return func(map[string]Setting) (SignalSet, []string) { return set, match }
}
