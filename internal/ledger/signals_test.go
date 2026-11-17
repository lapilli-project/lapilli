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

// signalDefs are two named signals: errors, on the match in effect, and
// probe, on service, both keeping the record.
var signalDefs = []SignalDef{
	{Name: "errors", Expr: "http_requests_total > 1e9", For: 5 * time.Minute, Record: true},
	{Name: "probe", Expr: "probe_success == 0", For: 2 * time.Minute, Match: []string{"service"}, Record: true},
}

func seenOfValues(labels []string, values ...string) *verdict.Seen {
	s := verdict.NewSeen(labels)
	for _, v := range values {
		s.AddValue(labels[0], v)
	}
	return s
}

// Signals are kept as serve is started with them, each named once, a bit
// each, the lowest free; one no longer given is taken away and its bit
// given to the next; a round that changes nothing writes nothing.
func TestSyncSignals(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	got, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match)
	if err != nil || !got.Set || got.Entries {
		t.Fatalf("%+v %v", got, err)
	}
	gen := l.Gen()
	if got, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil || got.Set || got.Entries || l.Gen() != gen {
		t.Errorf("unchanged: %+v %v, gen %d → %d", got, err, gen, l.Gen())
	}
	entries := []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe"}, Match: []string{}}}
	if got, err := l.SyncSignals(ctx, signalDefs, entries, opts.Match); err != nil || got.Set || !got.Entries {
		t.Errorf("entries: %+v %v", got, err)
	}
	if es, err := l.RuleEntries(ctx); err != nil || len(es) != 1 || es[0].Match == nil || len(es[0].Match) != 0 {
		t.Errorf("entries kept: %+v %v", es, err)
	}
	defs := []SignalDef{signalDefs[1], {Name: "latency", Expr: "latency > 1", For: 10 * time.Minute, Record: true}}
	if got, err := l.SyncSignals(ctx, defs, entries, opts.Match); err != nil || !got.Set {
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
	if _, ok := bits["errors"]; ok || bits["probe"] != 1 || bits["latency"] != 0 {
		t.Errorf("bits: %v", bits)
	}
}

// Which signals an alert is tied to is worked out in the write that makes
// it, before its tallies: by what each signal has seen, and when nothing
// that keeps the record ties it, every one that does. A value seen later
// ties the alerts of that value alone: the alerts a look rewrites are
// those it could tie otherwise, not every one.
func TestSignalTies(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	const n = 1000
	var fs []firing.Firing
	for i := range n {
		at := t0.Add(time.Duration(i) * time.Minute)
		fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": fmt.Sprintf("svc-%04d", i)}, Start: at, End: at.Add(time.Minute)})
	}
	if err := l.write(ctx, func(tx *txn) error {
		return l.addSeen(tx, opts.Match, map[string]*verdict.Seen{"errors": seenOfValues([]string{"service"}, "svc-0001", "svc-0002")})
	}); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	bitsOf := func(service string) int64 {
		t.Helper()
		var b int64
		if err := l.r.QueryRowContext(ctx, "SELECT signals FROM series WHERE labels LIKE ?", `%"`+service+`"%`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if b := bitsOf("svc-0001"); b != 1 {
		t.Errorf("an alert errors saw: %d, want 1 (errors)", b)
	}
	if b := bitsOf("svc-0500"); b != 3|verdict.FellBackBit {
		t.Errorf("an alert nothing saw: %d, want every signal, fallen back", b)
	}
	var untied int
	l.r.QueryRowContext(ctx, "SELECT count(*) FROM series WHERE signals < 0").Scan(&untied)
	if untied != 0 {
		t.Errorf("%d alerts left untied", untied)
	}
	// The tallies are by them: a day's firings of Errors apart.
	days, err := l.DaysBy(ctx, l.Day(t0), l.Day(t0))
	if err != nil || len(days) != 2 {
		t.Fatalf("days by signals: %+v %v", days, err)
	}
	// The probe sees one service: the one alert of it is tied again, alone.
	written := -1
	retied = func(n int) { written = n }
	defer func() { retied = nil }()
	if err := l.write(ctx, func(tx *txn) error {
		return l.addSeen(tx, opts.Match, map[string]*verdict.Seen{"probe": seenOfValues([]string{"service"}, "svc-0500")})
	}); err != nil {
		t.Fatal(err)
	}
	if written != 1 || bitsOf("svc-0500") != 2 {
		t.Errorf("tied again %d alerts, svc-0500 to %d; want 1, the probe", written, bitsOf("svc-0500"))
	}
	if b := bitsOf("svc-0001"); b != 1 {
		t.Errorf("an alert the probe didn't see: %d", b)
	}
	// Seen again, nothing is new: nothing is written.
	written = -1
	if err := l.write(ctx, func(tx *txn) error {
		return l.addSeen(tx, opts.Match, map[string]*verdict.Seen{"probe": seenOfValues([]string{"service"}, "svc-0500")})
	}); err != nil || written != -1 {
		t.Errorf("seen again: %d written, %v", written, err)
	}
	v, _ := l.meta(ctx, l.r, "pending")
	if v == "" || v == "[]" {
		t.Error("the alert tied again: its firings not pended to be counted again")
	}
	if err := l.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	days, _ = l.DaysBy(ctx, l.Day(t0), l.Day(t0))
	sets := map[int64]int{}
	for _, d := range days {
		sets[d.Signals] += d.Firings
	}
	if sets[2] != 1 || sets[1] != 2 || !slices.Contains([]int{n - 3}, sets[3|verdict.FellBackBit]) {
		t.Errorf("days by signals after: %v", sets)
	}
}

// A run's read of a named signal the ledger doesn't keep (no row from
// SyncSignals, with its own bit) keeps nothing: no row made, at bit 0, a
// bit another signal has. And no two named signals can share a bit.
func TestSignalWithNoRowKeepsNothing(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(10 * time.Minute)
	f := Found{From: t0, To: t0.Add(time.Hour), Run: Run{Kind: "test", Started: t0, Finished: t0}, Signals: []SignalRead{{
		Name: "ghost", Criterion: "ghost > 1", Since: t0.Add(-time.Hour), From: t0,
		Windows: []incident.Incident{{ID: "impact-ghost-x", Signal: "ghost", Start: at, End: at.Add(5 * time.Minute)}},
	}}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	rows, err := l.SignalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "ghost" {
			t.Errorf("a row made for a signal the ledger doesn't keep, at bit %d", r.Bit)
		}
	}
	if ws, err := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour)); err != nil || len(ws) != 0 {
		t.Errorf("windows kept of a signal the ledger doesn't keep: %+v %v", ws, err)
	}
	// Nor does what keeps its record make one.
	if err := l.write(ctx, func(t *txn) error { return setSigMeta(t, "ghost", metaImpactSince, "1") }); err != nil {
		t.Fatal(err)
	}
	if v, err := sigMeta(ctx, l.r, "ghost", metaImpactSince); err != nil || v != "" {
		t.Errorf("ghost's since kept: %q %v", v, err)
	}
	err = l.write(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "INSERT INTO impact_signals (name, bit) VALUES ('twin', (SELECT bit FROM impact_signals WHERE name = 'errors'))")
		return err
	})
	if err == nil {
		t.Error("two named signals on one bit")
	}
}

// A read again of a named signal the ledger doesn't keep keeps nothing:
// no row made, no window.
func TestReadAgainWithNoRowKeepsNothing(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(time.Hour), nil)
	at := t0.Add(10 * time.Minute)
	err := l.ReadAgain(ctx, ImpactRead{Signal: "ghost", Criterion: "ghost > 1", From: t0, To: t0.Add(time.Hour),
		Windows: []incident.Incident{{ID: "impact-ghost-x", Signal: "ghost", Start: at, End: at.Add(5 * time.Minute)}}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if ws, err := l.ImpactWindows(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour)); err != nil || len(ws) != 0 {
		t.Errorf("windows kept of a signal the ledger doesn't keep: %+v %v", ws, err)
	}
	if v, err := sigMeta(ctx, l.r, "ghost", metaImpactSince); err != nil || v != "" {
		t.Errorf("ghost's since kept: %q %v", v, err)
	}
}

// A read catching up after a miss, a piece at a time, keeps its windows
// to where it stopped, and the signal missed from there; the read that
// gets to the run's end is caught up.
func TestSignalReadInPieces(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match); err != nil {
		t.Fatal(err)
	}
	run := func(from, to time.Time, r SignalRead) {
		t.Helper()
		f := Found{From: from, To: to, Run: Run{Kind: "test", Started: from, Finished: to}, Signals: []SignalRead{r}}
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatal(err)
		}
	}
	missedAt := func() time.Time {
		t.Helper()
		rows, err := l.SignalRows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Name == "probe" {
				return r.Missed
			}
		}
		return time.Time{}
	}
	run(t0, t0.Add(time.Hour), SignalRead{Name: "probe", Criterion: "c", From: t0, Since: t0.Add(-time.Hour), Missed: true})
	if !missedAt().Equal(t0) {
		t.Fatalf("missed at %s", missedAt())
	}
	at := t0.Add(20 * time.Minute)
	run(t0.Add(2*time.Hour), t0.Add(3*time.Hour), SignalRead{Name: "probe", Criterion: "c", From: t0, To: t0.Add(2 * time.Hour), Since: t0.Add(-time.Hour),
		Windows: []incident.Incident{{ID: "impact-probe-x", Signal: "probe", Start: at, End: at.Add(5 * time.Minute)}}})
	if got := missedAt(); !got.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("after a piece to %s, missed at %s", t0.Add(2*time.Hour), got)
	}
	if ws, err := l.ImpactWindows(ctx, t0, t0.Add(3*time.Hour)); err != nil || len(ws) != 1 {
		t.Errorf("the piece's windows: %+v %v", ws, err)
	}
	run(t0.Add(3*time.Hour), t0.Add(4*time.Hour), SignalRead{Name: "probe", Criterion: "c", From: t0.Add(2 * time.Hour), Since: t0.Add(time.Hour)})
	if got := missedAt(); !got.IsZero() {
		t.Errorf("caught up, missed at %s", got)
	}
}
