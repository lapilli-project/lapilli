package ledger

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// The windows of the impact signal with no name, kept from before named
// signals were in effect, tied to the signal impact by the leader's round
// (tieUnnamed), as the Settings page ties them when the first named one is
// saved there (docs/design/signals.md, "Step 3, as built").

var (
	// tieImpact is the signal impact, of the criterion the one with no
	// name was read with, matched on service; tiePayments keeps no record.
	tieImpact   = SignalDef{Name: "impact", Expr: "impact > 1", For: 5 * time.Minute, Match: []string{"service"}, Record: true}
	tiePayments = SignalDef{Name: "payments", Expr: "payments_failed_total > 0", For: time.Minute, Record: false}
	tieLatency  = SignalDef{Name: "latency", Expr: "latency_seconds > 1", For: 10 * time.Minute, Record: true}
	// paymentsOnly ties Errors to payments alone: its firings are left
	// unjudged.
	paymentsOnly = []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"payments"}}}
)

// recordUnnamed records two windows of the impact signal with no name, as
// a lapilli serve with --impact PROMQL keeps them: checkout's on Monday,
// which Errors' checkout firing catches, and search's the Tuesday after;
// beside the firings of Errors, checkout's at 00:12 and search's at 03:00
// on Monday. It returns the windows as the ledger keeps them.
func recordUnnamed(t *testing.T, l *Ledger) []Incident {
	t.Helper()
	ws := []incident.Incident{
		{ID: "impact-1", Candidate: true, Start: t0.Add(10 * time.Minute), End: t0.Add(30 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}},
		{ID: "impact-2", Candidate: true, Start: t0.Add(8 * 24 * time.Hour), End: t0.Add(8*24*time.Hour + 20*time.Minute), Labels: incident.Labels{"service": {"search"}}},
	}
	fs := []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(12 * time.Minute), End: t0.Add(40 * time.Minute)},
		{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: t0.Add(3 * time.Hour), End: t0.Add(3*time.Hour + 10*time.Minute)},
	}
	to := t0.Add(10 * 24 * time.Hour)
	f := Found{From: t0, To: to, Firings: fs, Run: Run{Kind: "test", Started: to, Finished: to, From: t0, To: to},
		ImpactOn: true, ImpactSince: t0.Add(-time.Hour), ImpactCriterion: "5m impact > 1", Impact: ws}
	if err := l.Record(context.Background(), f, opts); err != nil {
		t.Fatal(err)
	}
	got, err := l.ImpactWindows(context.Background(), t0.Add(-time.Hour), to)
	if err != nil || len(got) != 2 || got[0].Signal != "" || got[1].Signal != "" {
		t.Fatalf("the windows of the signal with no name: %+v %v", got, err)
	}
	return got
}

// startedBefore keeps defs as the release before this one kept them as
// serve started with them, tying nothing, and judges what that pended.
func startedBefore(t *testing.T, l *Ledger, defs []SignalDef, entries []verdict.RuleEntry) {
	t.Helper()
	ctx := context.Background()
	if _, err := l.ChangeSignals(ctx, SignalsChange{}, fixed(SignalSet{Defs: defs, Entries: entries}, opts.Match), ""); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, namedOpts(t, l, entries)); err != nil {
		t.Fatal(err)
	}
	if v, _ := l.meta(ctx, l.r, "pending"); v != "" && v != "[]" && v != "null" {
		t.Fatalf("left pending: %s", v)
	}
}

// namedOpts are opts with the named signals the ledger keeps, as lapilli
// serve reads them, and entries.
func namedOpts(t *testing.T, l *Ledger, entries []verdict.RuleEntry) verdict.Options {
	t.Helper()
	ctx := context.Background()
	o := opts
	gen, err := l.SignalsGen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := l.SignalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var list []verdict.Signal
	for _, r := range rows {
		if r.Name != "" {
			list = append(list, verdict.Signal{Name: r.Name, For: r.For, Match: r.Match, Record: r.Record, Bit: r.Bit, Start: r.Kept.From(), Seen: verdict.NewSeen(r.MatchOr(o.Match))})
		}
	}
	o.SignalsGen, o.RecordUnit, o.Signals = gen, l.WeekOf, verdict.NewSignals(list, entries)
	return o
}

// judged are the firings of the two weeks from t0 as the ledger judges
// them by o, by start.
func judged(t *testing.T, l *Ledger, o verdict.Options) []verdict.Judged {
	t.Helper()
	rep, err := l.Report(context.Background(), t0, t0.Add(14*24*time.Hour), o, true)
	if err != nil {
		t.Fatal(err)
	}
	return rep.Firings
}

func tiedNotes(t *testing.T, l *Ledger) []Change {
	t.Helper()
	cs, err := l.Changes(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(cs, func(c Change) bool { return c.What != TiedWhat })
}

// Of a ledger a release before this one kept named signals in, impact
// among them, the windows of the signal with no name become impact's,
// their keys kept, matched on impact's labels; what changes with them is
// pended, exactly: the firings from --lead before each to its end. Judged
// again, Errors, tied to payments alone, is unjudged, its checkout firing
// no longer white for a window it may not catch, as lapilli verdict says
// with the same signals and no window from before. The activity log notes
// it, once: the next round writes nothing.
func TestTieUnnamedIntoImpactInEffect(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	ws := recordUnnamed(t, l)
	defs := []SignalDef{tieImpact, tiePayments}
	startedBefore(t, l, defs, paymentsOnly)
	o := namedOpts(t, l, paymentsOnly)
	if fs := judged(t, l, o); len(fs) != 2 || !fs[0].White || fs[1].NoRecord {
		t.Fatalf("before: %+v", fs)
	}
	lead := 20 * time.Minute
	got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: defs, Entries: paymentsOnly, Lead: lead}, opts.Match))
	if err != nil || got.Tied != 2 || !got.TiedRead || got.Set || got.Entries {
		t.Fatalf("%+v %v", got, err)
	}
	after, err := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("windows: %+v", after)
	}
	for i, w := range after {
		if w.ID != ws[i].ID || w.Signal != "impact" || !slices.Equal(w.Match, []string{"service"}) {
			t.Errorf("window %s (was %s): of %q, matched on %v", w.ID, ws[i].ID, w.Signal, w.Match)
		}
	}
	// From --lead before each, but not before the first firing kept.
	first := t0.Add(12 * time.Minute).Unix()
	want := [][2]int64{{max(ws[0].Start.Add(-lead).Unix(), first), ws[0].End.Unix()}, {ws[1].Start.Add(-lead).Unix(), ws[1].End.Unix()}}
	v, err := l.meta(ctx, l.r, "pending")
	var pending [][2]int64
	if err != nil || json.Unmarshal([]byte(v), &pending) != nil || !slices.Equal(pending, want) {
		t.Errorf("pended %s, want %v", v, want)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	fs := judged(t, l, o)
	for _, f := range fs {
		if f.White || !f.NoRecord {
			t.Errorf("the firing at %v: white %v, no record %v; want unjudged", f.Start, f.White, f.NoRecord)
		}
	}
	var plain []firing.Firing
	for _, f := range fs {
		plain = append(plain, f.Firing)
	}
	o.From, o.To = t0, t0.Add(14*24*time.Hour)
	if r := verdict.Judge(nil, plain, nil, o); !r.NoRecord {
		t.Errorf("verdict.Judge: %d unjudged of %d", r.Unjudged, len(r.Firings))
	}
	notes := tiedNotes(t, l)
	if len(notes) != 1 || notes[0].By != TiedBy || !strings.Contains(notes[0].After, `"windows":2`) {
		t.Errorf("the activity log: %+v", notes)
	}
	// What the row with no name kept stands: impact has its own.
	if _, ok := rowOf(t, l, ""); !ok {
		t.Error("the row with no name went")
	}
	gen := l.Gen()
	if got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: defs, Entries: paymentsOnly, Lead: lead}, opts.Match)); err != nil || got.Tied != 0 || l.Gen() != gen {
		t.Errorf("again: %+v %v, gen %d → %d", got, err, gen, l.Gen())
	}
	if n := len(tiedNotes(t, l)); n != 1 {
		t.Errorf("again, %d notes", n)
	}
}

// Of a ledger that keeps payments, impact added beside it: impact is made
// of the signal with no name, as the Settings page makes it (its row, the
// bit free, what keeps its record and its windows' keys kept), and, being
// one no alert is tied to, every alert is tied again, to it too, and every
// firing judged again.
func TestTieUnnamedMakesImpact(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	ws := recordUnnamed(t, l)
	startedBefore(t, l, []SignalDef{tiePayments}, paymentsOnly)
	impact := tieImpact
	impact.Match = nil // the match in effect, as the row with no name has it
	got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: []SignalDef{impact, tiePayments}, Entries: paymentsOnly}, opts.Match))
	if err != nil || got.Tied != 2 || !got.TiedRead || !got.Set {
		t.Fatalf("%+v %v", got, err)
	}
	r, ok := rowOf(t, l, "impact")
	p, _ := rowOf(t, l, "payments")
	if _, still := rowOf(t, l, ""); !ok || still || r.Kept.Criterion != "5m impact > 1" || r.Kept.Since.IsZero() || r.Bit == p.Bit {
		t.Fatalf("impact's row %+v, payments' %+v", r, p)
	}
	after, err := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour))
	if err != nil || len(after) != 2 {
		t.Fatal(after, err)
	}
	for i, w := range after {
		if w.ID != ws[i].ID || w.Signal != "impact" {
			t.Errorf("window %s (was %s) of %q", w.ID, ws[i].ID, w.Signal)
		}
	}
	var untied int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM series WHERE signals < 0 OR (signals & CAST(? AS BIGINT)) = 0", int64(1)<<r.Bit).Scan(&untied); err != nil || untied != 0 {
		t.Errorf("%d alerts not tied to impact: %v", untied, err)
	}
	v, _ := l.meta(ctx, l.r, "pending")
	var pending [][2]int64
	if json.Unmarshal([]byte(v), &pending) != nil || len(pending) != 1 || pending[0][0] > t0.Add(12*time.Minute).Unix() || pending[0][1] < ws[1].Start.Unix() {
		t.Errorf("pended %s: want every firing", v)
	}
	if n := len(tiedNotes(t, l)); n != 1 {
		t.Errorf("%d notes", n)
	}
}

// With no impact among the signals in effect, the windows are impact's, a
// signal no longer read: as the Settings page leaves them when the impact
// it made of the one with no name is taken away, judged alike (incidents
// with its name, as any signal's taken away).
func TestTieUnnamedNoneRead(t *testing.T) {
	ctx := context.Background()
	defs := []SignalDef{tieLatency, tiePayments}
	cli := open(t, time.UTC)
	recordUnnamed(t, cli)
	got, err := cli.SyncSignalSet(ctx, fixed(SignalSet{Defs: defs, Entries: paymentsOnly, Lead: opts.Lead}, opts.Match))
	if err != nil || got.Tied != 2 || got.TiedRead {
		t.Fatalf("%+v %v", got, err)
	}
	if _, ok := rowOf(t, cli, "impact"); ok {
		t.Error("impact has a row, as a signal in effect")
	}
	if err := cli.JudgePending(ctx, namedOpts(t, cli, paymentsOnly)); err != nil {
		t.Fatal(err)
	}
	// The Settings page: the first named signal saved makes the one with no
	// name impact, on the match in effect; then impact is taken away.
	ui := open(t, time.UTC)
	recordUnnamed(t, ui)
	made := tieImpact
	made.Match = nil
	if _, err := ui.ChangeSignals(ctx, SignalsChange{Rename: true, From: "", To: "impact"}, fixed(SignalSet{Defs: append([]SignalDef{made}, defs...), Entries: paymentsOnly}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := ui.ChangeSignals(ctx, SignalsChange{}, fixed(SignalSet{Defs: defs, Entries: paymentsOnly}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	if err := ui.JudgePending(ctx, namedOpts(t, ui, paymentsOnly)); err != nil {
		t.Fatal(err)
	}
	window := func(w Incident) string { return w.ID + " " + w.Signal + " " + strings.Join(w.Match, ",") }
	a, _ := cli.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour))
	b, _ := ui.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour))
	if len(a) != 2 || !slices.Equal(slicesMap(a, window), slicesMap(b, window)) {
		t.Errorf("windows: %v, the Settings page's %v", slicesMap(a, window), slicesMap(b, window))
	}
	firings := func(f verdict.Judged) string {
		return f.Start.String() + map[bool]string{true: " white", false: ""}[f.White] + map[bool]string{true: " no record", false: ""}[f.NoRecord]
	}
	x, y := slicesMap(judged(t, cli, namedOpts(t, cli, paymentsOnly)), firings), slicesMap(judged(t, ui, namedOpts(t, ui, paymentsOnly)), firings)
	if len(x) != 2 || !slices.Equal(x, y) {
		t.Errorf("judged %v, by the Settings page's %v", x, y)
	}
}

func slicesMap[T any](xs []T, f func(T) string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

// With no named signal (the one with no name still given), or with named
// ones and no window of the one with no name kept, nothing is tied, and
// nothing written.
func TestTieUnnamedNothingToTie(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	recordUnnamed(t, l)
	gen := l.Gen()
	if got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Lead: opts.Lead}, opts.Match)); err != nil || got.Tied != 0 || l.Gen() != gen {
		t.Errorf("no named signal: %+v %v, gen %d → %d", got, err, gen, l.Gen())
	}
	if ws, _ := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour)); len(ws) != 2 || ws[0].Signal != "" || ws[1].Signal != "" {
		t.Errorf("windows: %+v", ws)
	}
	// Named signals a release before this one kept, then the one with no
	// name given again: its windows are its own still.
	startedBefore(t, l, []SignalDef{tieImpact, tiePayments}, paymentsOnly)
	if got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Lead: opts.Lead}, opts.Match)); err != nil || got.Tied != 0 || !got.Set {
		t.Errorf("given again with no name: %+v %v", got, err)
	}
	if ws, _ := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour)); len(ws) != 2 || ws[0].Signal != "" || ws[1].Signal != "" {
		t.Errorf("given again with no name, windows: %+v", ws)
	}
	fresh := open(t, time.UTC)
	set := fixed(SignalSet{Defs: []SignalDef{tieImpact, tiePayments}, Entries: paymentsOnly, Lead: opts.Lead}, opts.Match)
	if got, err := fresh.SyncSignalSet(ctx, set); err != nil || got.Tied != 0 || !got.Set {
		t.Errorf("named, nothing kept: %+v %v", got, err)
	}
	gen = fresh.Gen()
	if got, err := fresh.SyncSignalSet(ctx, set); err != nil || got.Tied != 0 || fresh.Gen() != gen {
		t.Errorf("again: %+v %v", got, err)
	}
	if n := len(tiedNotes(t, l)) + len(tiedNotes(t, fresh)); n != 0 {
		t.Errorf("%d notes", n)
	}
}

// Two replicas' rounds at once, on one ledger (two writers of one, on
// SQLite): one ties the windows, under the writers' turn, and the other
// finds none left.
func TestTieUnnamedOnce(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	recordUnnamed(t, l)
	other := l
	if strings.HasPrefix(where, "postgres") {
		if other, err = openAt(ctx, where, time.UTC); err != nil {
			t.Fatal(err)
		}
		defer other.Close()
	}
	set := fixed(SignalSet{Defs: []SignalDef{tieImpact, tiePayments}, Entries: paymentsOnly, Lead: opts.Lead}, opts.Match)
	var wg sync.WaitGroup
	got := make([]SignalsSynced, 2)
	errs := make([]error, 2)
	for i, led := range []*Ledger{l, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = led.SyncSignalSet(ctx, set)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || got[0].Tied+got[1].Tied != 2 || got[0].Tied != 0 && got[1].Tied != 0 {
		t.Fatalf("%+v %v", got, errs)
	}
	if n := len(tiedNotes(t, l)); n != 1 {
		t.Errorf("%d notes", n)
	}
	if ws, _ := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour)); slices.ContainsFunc(ws, func(w Incident) bool { return w.Signal != "impact" }) {
		t.Errorf("windows: %+v", ws)
	}
}

// eightNamed are eight named signals, s0 to s7, as many as there may be.
func eightNamed() []SignalDef {
	var out []SignalDef
	for i := range verdict.MaxSignals {
		out = append(out, SignalDef{Name: "s" + strconv.Itoa(i), Expr: "s" + strconv.Itoa(i) + "_total > 0", For: time.Minute, Record: true})
	}
	return out
}

// Of a ledger a release before this one kept eight named signals in, s0 to
// s7, beside the windows of the one with no name, started with impact in
// place of s7: s7 is taken away first, so that its bit is free, and impact
// is made of the one with no name with it, as the Settings page would. The
// round keeps the signals given, and fails no more than ChangeSignals does
// of the same set.
func TestTieUnnamedBesideEight(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	ws := recordUnnamed(t, l)
	eight := eightNamed()
	startedBefore(t, l, eight, nil)
	s7, _ := rowOf(t, l, "s7")
	impact := tieImpact
	impact.Match = nil
	defs := append(slices.Clone(eight[:7]), impact)
	got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: defs, Lead: opts.Lead}, opts.Match))
	if err != nil || got.TieErr != nil || got.Tied != 2 || !got.Set {
		t.Fatalf("%+v %v", got, err)
	}
	r, ok := rowOf(t, l, "impact")
	if _, still := rowOf(t, l, "s7"); !ok || still || r.Bit != s7.Bit || r.Kept.Criterion != "5m impact > 1" {
		t.Errorf("impact's row %+v, in place of s7 (bit %d)", r, s7.Bit)
	}
	if after, _ := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour)); len(after) != 2 || after[0].ID != ws[0].ID || after[0].Signal != "impact" || after[1].Signal != "impact" {
		t.Errorf("windows: %+v", after)
	}
	if err := l.JudgePending(ctx, namedOpts(t, l, nil)); err != nil {
		t.Errorf("judged by options of the signals given: %v", err)
	}
}

// refuseTie makes the ledger refuse to give a window of the signal with no
// name a signal's name, as a write that fails would.
func refuseTie(t *testing.T, l *Ledger) {
	t.Helper()
	qs := []string{`CREATE TRIGGER refuse_tie BEFORE UPDATE OF signal ON incidents
		WHEN OLD.signal = '' AND NEW.signal != '' BEGIN SELECT RAISE(ABORT, 'refused'); END`}
	if l.pg != nil {
		qs = []string{`CREATE FUNCTION refuse_tie() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$`,
			`CREATE TRIGGER refuse_tie BEFORE UPDATE OF signal ON incidents FOR EACH ROW
			WHEN (OLD.signal = '' AND NEW.signal <> '') EXECUTE FUNCTION refuse_tie()`}
	}
	for _, q := range qs {
		if _, err := l.w.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}

// A tie that fails leaves the windows as they are, and keeps the signals
// given all the same, in a write of their own: options read after it are
// of the signals the ledger keeps, and judge (no ErrSignalsMoved, which
// would wake the next round at once). The next round tries again.
func TestTieUnnamedFails(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	recordUnnamed(t, l)
	refuseTie(t, l)
	defs := []SignalDef{tieImpact, tiePayments}
	set := fixed(SignalSet{Defs: defs, Entries: paymentsOnly, Lead: opts.Lead}, opts.Match)
	got, err := l.SyncSignalSet(ctx, set)
	if err != nil || got.TieErr == nil || got.Tied != 0 || !got.Set || !got.Entries {
		t.Fatalf("%+v %v", got, err)
	}
	for _, name := range []string{"impact", "payments"} {
		if _, ok := rowOf(t, l, name); !ok {
			t.Errorf("no row of %s", name)
		}
	}
	if ws, _ := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour)); len(ws) != 2 || ws[0].Signal != "" || ws[1].Signal != "" {
		t.Errorf("windows: %+v", ws)
	}
	if err := l.JudgePending(ctx, namedOpts(t, l, paymentsOnly)); err != nil {
		t.Errorf("judged: %v", err)
	}
	if n := len(tiedNotes(t, l)); n != 0 {
		t.Errorf("%d notes of a tie that failed", n)
	}
	// Again, refused still: said again, the signals as they are.
	if got, err := l.SyncSignalSet(ctx, set); err != nil || got.TieErr == nil || got.Set {
		t.Errorf("again: %+v %v", got, err)
	}
	q := "DROP TRIGGER refuse_tie"
	if l.pg != nil {
		q += " ON incidents"
	}
	if _, err := l.w.ExecContext(ctx, q); err != nil {
		t.Fatal(err)
	}
	if got, err := l.SyncSignalSet(ctx, set); err != nil || got.TieErr != nil || got.Tied != 2 {
		t.Errorf("no longer refused: %+v %v", got, err)
	}
}

// Started first with no impact, the windows tied to impact, a signal no
// longer read; then with impact added: its windows are its own, matched on
// its labels, and judged by them.
func TestTieUnnamedThenImpactAdded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	recordUnnamed(t, l)
	if got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: []SignalDef{tiePayments}, Entries: paymentsOnly, Lead: opts.Lead}, opts.Match)); err != nil || got.Tied != 2 || got.TiedRead {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: []SignalDef{tieImpact, tiePayments}, Entries: paymentsOnly, Lead: opts.Lead}, opts.Match)); err != nil || !got.Set || got.Tied != 0 {
		t.Fatalf("impact added: %+v %v", got, err)
	}
	ws, _ := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(30*24*time.Hour))
	for _, w := range ws {
		if w.Signal != "impact" || !slices.Equal(w.Match, tieImpact.Match) {
			t.Errorf("window %s: of %q, matched on %v", w.ID, w.Signal, w.Match)
		}
	}
	o := namedOpts(t, l, paymentsOnly)
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	for _, f := range judged(t, l, o) {
		if f.White || !f.NoRecord {
			t.Errorf("the firing at %v: white %v, no record %v; want unjudged", f.Start, f.White, f.NoRecord)
		}
	}
}

// Once a round found no window of the signal with no name left, named
// signals in effect, the rounds after don't look for one again; with no
// named signal, they may be found again, and are looked for when signals
// are named again.
func TestTieUnnamedLooksOnce(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	ws := recordUnnamed(t, l)
	named := fixed(SignalSet{Defs: []SignalDef{tieImpact, tiePayments}, Entries: paymentsOnly, Lead: opts.Lead}, opts.Match)
	if got, err := l.SyncSignalSet(ctx, named); err != nil || got.Tied != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	// One with no name again, which no round of named signals could keep:
	// not looked for.
	if _, err := l.w.ExecContext(ctx, "UPDATE incidents SET signal = '' WHERE key = ?", ws[0].ID); err != nil {
		t.Fatal(err)
	}
	if got, err := l.SyncSignalSet(ctx, named); err != nil || got.Tied != 0 {
		t.Errorf("looked again: %+v %v", got, err)
	}
	// No signal named (the Settings page took the last away), then named
	// again there, which ties nothing: the round looks again, and ties it.
	if _, err := l.ChangeSignals(ctx, SignalsChange{}, fixed(SignalSet{Lead: opts.Lead}, opts.Match), "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ChangeSignals(ctx, SignalsChange{}, named, "ana"); err != nil {
		t.Fatal(err)
	}
	if got, err := l.SyncSignalSet(ctx, named); err != nil || got.Tied != 1 {
		t.Errorf("named again: %+v %v", got, err)
	}
	// A ledger a release before this one kept named signals in, with no
	// window of the one with no name: looked for once, in a write that
	// changes no generation, and not again.
	fresh := open(t, time.UTC)
	startedBefore(t, fresh, []SignalDef{tieImpact, tiePayments}, paymentsOnly)
	gen := fresh.Gen()
	if got, err := fresh.SyncSignalSet(ctx, named); err != nil || got.Tied != 0 || fresh.Gen() != gen {
		t.Errorf("%+v %v, gen %d → %d", got, err, gen, fresh.Gen())
	}
	if v, err := fresh.meta(ctx, fresh.r, metaUnnamedTied); err != nil || v == "" {
		t.Errorf("not noted: %q %v", v, err)
	}
}

// SyncSignals ties with the lead it is given: the firings from it before
// each window are judged again.
func TestSyncSignalsLead(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	ws := recordUnnamed(t, l)
	defs := []SignalDef{tieImpact, tiePayments}
	startedBefore(t, l, defs, paymentsOnly)
	lead := 40 * time.Minute
	if got, err := l.SyncSignals(ctx, defs, paymentsOnly, opts.Match, lead); err != nil || got.Tied != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	v, _ := l.meta(ctx, l.r, "pending")
	var pending [][2]int64
	if json.Unmarshal([]byte(v), &pending) != nil || len(pending) != 2 || pending[1][0] != ws[1].Start.Add(-lead).Unix() {
		t.Errorf("pended %s: want the second from %v", v, ws[1].Start.Add(-lead))
	}
}
