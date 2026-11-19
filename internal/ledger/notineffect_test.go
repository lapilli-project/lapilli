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

	"lapilli.dev/internal/verdict"
)

// On the upgrade to a release in which a window of a signal not in effect
// keeps no rule's record and only the rules with no entry catch it
// (docs/design/judging.md, item 5), the spans of those the ledger kept
// from before are pended to be judged again, once (PendNotInEffect).

// goneBefore is a ledger as a release before this one left it with the
// signal impact taken away: its windows kept, with its name; and Errors,
// which an entry ties to payments alone, white for impact's checkout
// window, as any rule caught such a window then. It gives the windows, and
// the options of the signals in effect after (payments alone).
func goneBefore(t *testing.T, l *Ledger) ([]Incident, verdict.Options) {
	t.Helper()
	ctx := context.Background()
	ws := recordUnnamed(t, l)
	startedBefore(t, l, []SignalDef{tieImpact, tiePayments}, paymentsOnly)
	if _, err := l.SyncSignalSet(ctx, fixed(SignalSet{Defs: []SignalDef{tieImpact, tiePayments}, Entries: paymentsOnly, Lead: opts.Lead}, opts.Match)); err != nil {
		t.Fatal(err)
	}
	startedBefore(t, l, []SignalDef{tiePayments}, paymentsOnly) // impact taken away
	for _, q := range []string{
		"UPDATE firings SET white = 1 WHERE start_at = " + strconv.FormatInt(ws[0].Start.Add(2*time.Minute).Unix(), 10),
		"INSERT INTO catches (incident_id, rule_id, delay) SELECT i.id, r.id, 120 FROM incidents i, rules r WHERE i.key = '" + ws[0].ID + "' AND r.name = 'Errors'",
	} {
		if _, err := l.w.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	o := namedOpts(t, l, paymentsOnly)
	if fs := judged(t, l, o); !fs[0].White {
		t.Fatalf("as the release before left it: %+v", fs)
	}
	return ws, o
}

// The windows of impact, taken away, are pended once, from the longest
// lead before each to its end; judged again, Errors no longer catches
// impact's window, as it has an entry. Asked again, nothing is pended,
// nothing written.
func TestPendNotInEffectOnce(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	ws, o := goneBefore(t, l)
	longer := o
	longer.Lead = 20 * time.Minute // the longest lead in effect
	n, err := l.PendNotInEffect(ctx, longer)
	if err != nil || n != 2 {
		t.Fatalf("pended %d: %v", n, err)
	}
	first := ws[0].Start.Add(2 * time.Minute).Unix() // not before the first firing kept
	want := [][2]int64{{max(ws[0].Start.Add(-20*time.Minute).Unix(), first), ws[0].End.Unix()}, {ws[1].Start.Add(-20 * time.Minute).Unix(), ws[1].End.Unix()}}
	v, err := l.meta(ctx, l.r, "pending")
	var pending [][2]int64
	if err != nil || json.Unmarshal([]byte(v), &pending) != nil || !slices.Equal(pending, want) {
		t.Errorf("pended %s, want %v", v, want)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	for _, f := range judged(t, l, o) {
		if f.White || !f.NoRecord {
			t.Errorf("the firing at %v: white %v, no record %v; want unjudged", f.Start, f.White, f.NoRecord)
		}
	}
	if in, err := l.IncidentByID(ctx, ws[0].ID); err != nil || len(in.CaughtBy) != 0 {
		t.Errorf("impact's window, caught by %+v %v", in.CaughtBy, err)
	}
	gen := l.Gen()
	if n, err := l.PendNotInEffect(ctx, o); err != nil || n != 0 || l.Gen() != gen {
		t.Errorf("again: %d %v, gen %d → %d", n, err, gen, l.Gen())
	}
	if v, _ := l.meta(ctx, l.r, "pending"); v != "" && v != "[]" && v != "null" {
		t.Errorf("again, pended %s", v)
	}
}

// From two ledgers on the same database at once (two replicas'), or two
// writers of one: one pends, the other finds it done.
func TestPendNotInEffectOneReplica(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, o := goneBefore(t, l)
	other := l
	if strings.HasPrefix(where, "postgres") {
		if other, err = openAt(ctx, where, time.UTC); err != nil {
			t.Fatal(err)
		}
		defer other.Close()
	}
	var wg sync.WaitGroup
	ns, errs := make([]int, 2), make([]error, 2)
	for i, led := range []*Ledger{l, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ns[i], errs[i] = led.PendNotInEffect(ctx, o)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || ns[0]+ns[1] != 2 || ns[0]*ns[1] != 0 {
		t.Errorf("pended %v: %v", ns, errs)
	}
}

// A ledger with no window of a signal not in effect notes it, in a write
// that changes no page, and looks no more. One whose signals are all in
// effect pends nothing either.
func TestPendNotInEffectNone(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	recordUnnamed(t, l)
	gen := l.Gen()
	if n, err := l.PendNotInEffect(ctx, opts); err != nil || n != 0 || l.Gen() != gen {
		t.Errorf("the windows of the signal with no name alone: %d %v, gen %d → %d", n, err, gen, l.Gen())
	}
	if v, _ := l.meta(ctx, l.r, metaNotInEffectPended); v != "1" {
		t.Errorf("not noted: %q", v)
	}
	named := open(t, time.UTC)
	recordUnnamed(t, named)
	if _, err := named.SyncSignalSet(ctx, fixed(SignalSet{Defs: []SignalDef{tieImpact, tiePayments}, Entries: paymentsOnly, Lead: opts.Lead}, opts.Match)); err != nil {
		t.Fatal(err)
	}
	if err := named.JudgePending(ctx, namedOpts(t, named, paymentsOnly)); err != nil {
		t.Fatal(err)
	}
	if n, err := named.PendNotInEffect(ctx, namedOpts(t, named, paymentsOnly)); err != nil || n != 0 {
		t.Errorf("impact in effect: %d %v", n, err)
	}
}
