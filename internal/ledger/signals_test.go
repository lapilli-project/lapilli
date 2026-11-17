package ledger

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
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
