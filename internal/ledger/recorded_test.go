package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

func putFirings(t testing.TB, l *Ledger, source string, from, to time.Time, fs []firing.Firing) Recorded {
	t.Helper()
	r, err := l.PutFirings(context.Background(), source, from, to, fs, FiringsCopier, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// in is the firings that start in [from, to).
func in(fs []firing.Firing, from, to time.Time) []firing.Firing {
	var out []firing.Firing
	for _, f := range fs {
		if !f.Start.Before(from) && f.Start.Before(to) {
			out = append(out, f)
		}
	}
	return out
}

// Firings recorded elsewhere are judged as verdict.Judge judges them, beside
// replayed ones, and a run replaying the rules leaves them as they are.
func TestRecordedSameAsJudge(t *testing.T) {
	ctx := context.Background()
	for seed := uint64(1); seed <= 4; seed++ {
		l := open(t, time.UTC)
		all, incs := world(seed, 400)
		if _, err := l.SyncFile(ctx, incs, opts); err != nil {
			t.Fatal(err)
		}
		// An alert that starts twice at once is one: a source sending it
		// twice.
		var fs []firing.Firing
		for _, f := range all {
			if !slices.ContainsFunc(fs, func(g firing.Firing) bool {
				return g.Rule == f.Rule && g.Start.Equal(f.Start) && reflect.DeepEqual(g.Labels, f.Labels)
			}) {
				fs = append(fs, f)
			}
		}
		// Half the rules' firings come from a cloud, in two writes; the
		// others are replayed, after and over them.
		var recorded, replayed []firing.Firing
		for _, f := range fs {
			if n := f.Rule[len(f.Rule)-1]; n%2 == 0 {
				recorded = append(recorded, f)
			} else {
				replayed = append(replayed, f)
			}
		}
		mid, end := t0.Add(80*time.Hour), t0.Add(7*24*time.Hour)
		putFirings(t, l, "cloud", t0, mid, in(recorded, t0, mid))
		putFirings(t, l, "cloud", mid, end, in(recorded, mid, end))
		record(t, l, t0, end, replayed)

		o := opts
		o.From, o.To = t0, end
		want := verdict.Judge(nil, slices.Clone(fs), incs, o)
		got, err := l.Report(ctx, o.From, o.To, o, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range got.Rules {
			if n := r.Rule[len(r.Rule)-1]; (n%2 == 0) != (r.Source == "cloud") {
				t.Errorf("seed %d: %s has source %q", seed, r.Rule, r.Source)
			}
		}
		sourceless := norm(got.Rules)
		for i := range sourceless {
			sourceless[i].Source = ""
		}
		if !reflect.DeepEqual(sourceless, norm(want.Rules)) {
			t.Errorf("seed %d rules\n got %+v\nwant %+v", seed, got.Rules, want.Rules)
		}
		if !reflect.DeepEqual(got.Incidents, want.Incidents) {
			t.Errorf("seed %d incidents\n got %+v\nwant %+v", seed, got.Incidents, want.Incidents)
		}
		if len(got.Firings) != len(want.Firings) {
			t.Errorf("seed %d: %d firings, want %d", seed, len(got.Firings), len(want.Firings))
		}
	}
}

// Sent again as it is, a period changes nothing and judges nothing again;
// only when the source was last written moves.
func TestRecordedTwice(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	fs, incs := world(9, 300)
	if _, err := l.SyncFile(ctx, incs, opts); err != nil {
		t.Fatal(err)
	}
	end := t0.Add(7 * 24 * time.Hour)
	first := putFirings(t, l, "cloud", t0, end, fs)
	if first.Added != len(fs) || first.Removed != 0 {
		t.Errorf("first write: %+v", first)
	}
	before, gen := dump(t, l), l.Gen()
	srcs, _ := l.Sources(ctx)
	shuffled := slices.Clone(fs)
	rand.New(rand.NewPCG(1, 1)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	time.Sleep(1100 * time.Millisecond) // for written to move
	again := putFirings(t, l, "cloud", t0, end, shuffled)
	if again.Changed() {
		t.Errorf("sent again: %+v", again)
	}
	if after := dump(t, l); after != before {
		t.Errorf("sent again, the ledger changed:\n%s", diff(before, after))
	}
	later, _ := l.Sources(ctx)
	if len(later) != 1 || !later[0].Written.After(srcs[0].Written) || !later[0].Changed.Equal(srcs[0].Changed) || later[0].Firings != len(fs) {
		t.Errorf("source before %+v, after %+v", srcs, later)
	}
	if l.Gen() == gen {
		t.Errorf("the source's last write isn't shown")
	}
}

// A period replaces what the ledger has of the source in it, and only that:
// firings taken away, ends changed, a firing still on that has ended.
func TestRecordedReplace(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	f := func(rule, svc string, start, end int) firing.Firing {
		x := firing.Firing{Rule: rule, Labels: map[string]string{"service": svc}, Start: at(start)}
		if end > 0 {
			x.End = at(end)
		}
		return x
	}
	putFirings(t, l, "cloud", at(0), at(24), []firing.Firing{f("A", "checkout", 1, 2), f("A", "checkout", 5, 0), f("B", "search", 7, 8)})
	putFirings(t, l, "cloud", at(24), at(48), []firing.Firing{f("A", "checkout", 30, 31)})
	putFirings(t, l, "other", at(0), at(24), []firing.Firing{f("A", "checkout", 1, 2)})
	// The first day again: A on at 5 has ended, B's firing is gone, and a
	// new one came.
	r := putFirings(t, l, "cloud", at(0), at(24), []firing.Firing{f("A", "checkout", 1, 2), f("A", "checkout", 5, 6), f("C", "cart", 9, 0)})
	if r.Added != 2 || r.Removed != 2 {
		t.Errorf("replaced: %+v", r)
	}
	got, err := l.Firings(ctx, FiringQuery{From: at(0), To: at(48)})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, x := range got {
		end := "on"
		if !x.End.IsZero() {
			end = x.End.Sub(t0).String()
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", x.Rule, x.Start.Sub(t0), end))
	}
	slices.Sort(lines)
	want := []string{"A 1h0m0s 2h0m0s", "A 30h0m0s 31h0m0s", "A 5h0m0s 6h0m0s", "A [other] 1h0m0s 2h0m0s", "C 9h0m0s on"}
	if !slices.Equal(lines, want) {
		t.Errorf("firings\n got %q\nwant %q", lines, want)
	}
	// A rule's tallies say the same.
	rep, err := l.Report(ctx, at(0), at(48), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range rep.Rules {
		if rr.Rule == "B" && rr.Firings != 0 {
			t.Errorf("B still counted: %+v", rr)
		}
	}
}

// What is sent is checked first, and a write refused changes nothing.
func TestRecordedRefused(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	ok := []firing.Firing{{Rule: "A", Start: t0.Add(time.Hour)}}
	for _, c := range []struct {
		source   string
		from, to time.Time
		fs       []firing.Firing
		says     string
	}{
		{"Cloud", t0, t0.Add(time.Hour * 2), ok, "lowercase"},
		{"-cloud", t0, t0.Add(time.Hour * 2), ok, "lowercase"},
		{"cloud/x", t0, t0.Add(time.Hour * 2), ok, "lowercase"},
		{strings.Repeat("a", 41), t0, t0.Add(time.Hour * 2), ok, "lowercase"},
		{"cloud", time.Time{}, t0, ok, "which period"},
		{"cloud", t0, t0, nil, "after"},
		{"cloud", t0, t0.Add(32 * 24 * time.Hour), ok, "31 days"},
		{"cloud", t0.Add(2 * time.Hour), t0.Add(3 * time.Hour), ok, "outside the period"},
		{"cloud", t0, t0.Add(time.Hour), ok, "outside the period"}, // to is outside
		{"cloud", t0, t0.Add(2 * time.Hour), []firing.Firing{{Rule: "A", Start: t0.Add(time.Hour), End: t0}}, "ends before"},
		{"cloud", t0, t0.Add(2 * time.Hour), []firing.Firing{{Rule: strings.Repeat("x", 513), Start: t0}}, "512 characters"},
		{"cloud", t0, t0.Add(2 * time.Hour), []firing.Firing{{Rule: "A", Start: t0, Labels: map[string]string{"x": strings.Repeat("y", 5000)}}}, "4 KB"},
	} {
		_, err := l.PutFirings(ctx, c.source, c.from, c.to, c.fs, FiringsCopier, opts)
		var inv ErrInvalid
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%q %s %s: %v, want one saying %q", c.source, c.from, c.to, err, c.says)
		}
	}
	if fs, _ := l.Firings(ctx, FiringQuery{From: t0.Add(-time.Hour), To: t0.Add(40 * 24 * time.Hour)}); len(fs) != 0 {
		t.Errorf("refused writes kept %d firings", len(fs))
	}
	// Fifty sources at most; a source written before can be written again.
	for i := range MaxSources {
		putFirings(t, l, fmt.Sprintf("s%d", i), t0, t0.Add(2*time.Hour), ok)
	}
	if _, err := l.PutFirings(ctx, "one-more", t0, t0.Add(2*time.Hour), ok, FiringsCopier, opts); err == nil || !strings.Contains(err.Error(), "50 sources") {
		t.Errorf("a 51st source: %v", err)
	}
	putFirings(t, l, "s7", t0, t0.Add(2*time.Hour), nil)
}

// A recorded rule is named as its source names it, told apart from a
// replayed rule and from other sources' when it has to be; a replayed rule
// coming later takes the name, and what people kept about the recorded one
// goes with it.
func TestRecordedNames(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	one := func(rule string, labels map[string]string) []firing.Firing {
		return []firing.Firing{{Rule: rule, Labels: labels, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}}
	}
	record(t, l, t0, t0.Add(3*time.Hour), one("Replayed", nil))
	putFirings(t, l, "cw", t0, t0.Add(3*time.Hour), one("Replayed", nil))
	putFirings(t, l, "cw", t0, t0.Add(3*time.Hour), one("Errors", map[string]string{"severity": "warning"}))
	putFirings(t, l, "az", t0, t0.Add(3*time.Hour), one("Errors", map[string]string{"severity": "warning"}))
	putFirings(t, l, "cw", t0.Add(3*time.Hour), t0.Add(6*time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"severity": "critical"}, Start: t0.Add(4 * time.Hour)}})
	if err := l.PutNote(ctx, Note{Rule: "Errors", Note: "the cloud's"}, "ana"); err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		table, err := l.RuleTable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range table {
			out = append(out, r.Name+" <"+r.Source+">")
		}
		slices.Sort(out)
		return out
	}
	want := []string{"Errors <cw>", "Errors [az] <az>", "Errors [severity=critical] <cw>", "Replayed <>", "Replayed [cw] <cw>"}
	if got := names(); !slices.Equal(got, want) {
		t.Errorf("names\n got %q\nwant %q", got, want)
	}
	// A rule called Errors comes to be replayed: the cloud's, which it has
	// at two severities, is told apart by its severity.
	record(t, l, t0.Add(6*time.Hour), t0.Add(9*time.Hour), one("Errors", nil))
	want = []string{"Errors <>", "Errors [az] <az>", "Errors [severity=critical] <cw>", "Errors [severity=warning] <cw>", "Replayed <>", "Replayed [cw] <cw>"}
	if got := names(); !slices.Equal(got, want) {
		t.Errorf("after a replayed Errors\n got %q\nwant %q", got, want)
	}
	if notes, err := l.Notes(ctx); err != nil || notes["Errors [severity=warning]"].Note != "the cloud's" || notes["Errors"].Note != "" {
		t.Errorf("the note went to %+v %v", notes, err)
	}
	// Each keeps its own firings, and the cloud's next write finds its rule.
	putFirings(t, l, "cw", t0, t0.Add(3*time.Hour), one("Errors", map[string]string{"severity": "warning"}))
	fs, _ := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(9 * time.Hour)})
	count := map[string]int{}
	for _, f := range fs {
		count[f.Rule]++
	}
	if count["Errors"] != 1 || count["Errors [severity=warning]"] != 1 || count["Errors [az]"] != 1 {
		t.Errorf("firings by rule: %v", count)
	}
}

// A run's ending of alerts it no longer sees on leaves a recorded firing
// still on alone: its source says when it ends.
func TestRecordedStillOn(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	putFirings(t, l, "cw", t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Disk", Start: t0.Add(time.Hour)}})
	record(t, l, t0, t0.Add(30*time.Hour), nil)
	record(t, l, t0.Add(30*time.Hour), t0.Add(40*time.Hour), nil)
	fs, _ := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(40 * time.Hour)})
	if len(fs) != 1 || !fs[0].End.IsZero() {
		t.Fatalf("got %+v", fs)
	}
	putFirings(t, l, "cw", t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Disk", Start: t0.Add(time.Hour), End: t0.Add(26 * time.Hour)}})
	fs, _ = l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(40 * time.Hour)})
	if len(fs) != 1 || !fs[0].End.Equal(t0.Add(26*time.Hour)) {
		t.Errorf("got %+v", fs)
	}
}

// An incident written down after the firings were copied in is judged
// against them.
func TestRecordedThenIncident(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	putFirings(t, l, "cw", t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "5xx", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	if _, err := l.Put(ctx, incident.Incident{ID: "INC-1", Start: t0.Add(time.Hour + 5*time.Minute), Labels: incident.Labels{"service": {"checkout"}}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Report(ctx, t0, t0.Add(24*time.Hour), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rules) != 1 || rep.Rules[0].White != 1 || rep.Rules[0].Source != "cw" || len(rep.Incidents) != 1 || len(rep.Incidents[0].CaughtBy) != 1 {
		t.Errorf("rules %+v incidents %+v", rep.Rules, rep.Incidents)
	}
}

// Pruning drops recorded firings as it drops replayed ones, and forgets a
// source nothing was written to since.
func TestRecordedPrune(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	putFirings(t, l, "cw", t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "A", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	if err := l.Prune(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := l.Sources(ctx); len(srcs) != 1 {
		t.Errorf("a source written now was forgotten: %+v", srcs)
	}
	if err := l.Prune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	fs, _ := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	srcs, _ := l.Sources(ctx)
	if len(fs) != 0 || len(srcs) != 0 {
		t.Errorf("kept %d firings, sources %+v", len(fs), srcs)
	}
}

// A recorded rule's labels, for teams to own it by, are those all its
// alerts have alike; they follow what is written.
func TestRecordedSharedLabels(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	f := func(svc, inst string, h int) firing.Firing {
		return firing.Firing{Rule: "cpu", Labels: map[string]string{"service": svc, "instance": inst}, Start: t0.Add(time.Duration(h) * time.Hour)}
	}
	putFirings(t, l, "cw", t0, t0.Add(24*time.Hour), []firing.Firing{f("checkout", "a", 1), f("checkout", "b", 2)})
	got, err := l.SharedLabels(ctx)
	if err != nil || !reflect.DeepEqual(got, map[string]map[string]string{"cpu": {"service": "checkout"}}) {
		t.Errorf("shared: %v %v", got, err)
	}
	putFirings(t, l, "cw", t0.Add(24*time.Hour), t0.Add(48*time.Hour), []firing.Firing{f("search", "c", 30)})
	if got, _ := l.SharedLabels(ctx); len(got["cpu"]) != 0 {
		t.Errorf("shared after another service: %v", got)
	}
}

// A period written again, whose firing now ends later, past the period's
// end, gives the next firing of its alert, in the period after, its stones
// again: it refired after it.
func TestRecordedRefireAcrossPeriods(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := func(h, m int) time.Time { return t0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	lb := map[string]string{"service": "checkout"}
	putFirings(t, l, "cw", at(24, 0), at(48, 0), []firing.Firing{{Rule: "A", Labels: lb, Start: at(30, 0), End: at(31, 0)}})
	putFirings(t, l, "cw", at(0, 0), at(24, 0), []firing.Firing{{Rule: "A", Labels: lb, Start: at(23, 0)}})
	putFirings(t, l, "cw", at(0, 0), at(24, 0), []firing.Firing{{Rule: "A", Labels: lb, Start: at(23, 0), End: at(29, 50)}})
	rep, err := l.Report(ctx, at(0, 0), at(48, 0), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rules) != 1 || rep.Rules[0].Refired != 1 {
		t.Errorf("rules %+v, want one refired", rep.Rules)
	}
}

// A write names new rules the same way whatever order it lists them in.
func TestRecordedNamesInAnyOrder(t *testing.T) {
	ctx := context.Background()
	var first []string
	for i := range 12 {
		l := open(t, time.UTC)
		fs := []firing.Firing{
			{Rule: "X", Start: t0.Add(time.Hour)},
			{Rule: "X", Labels: map[string]string{"severity": "warning"}, Start: t0.Add(2 * time.Hour)},
			{Rule: "X", Labels: map[string]string{"severity": "critical"}, Start: t0.Add(3 * time.Hour)},
		}
		rand.New(rand.NewPCG(uint64(i), 9)).Shuffle(len(fs), func(a, b int) { fs[a], fs[b] = fs[b], fs[a] })
		putFirings(t, l, "cw", t0, t0.Add(24*time.Hour), fs)
		table, _ := l.RuleTable(ctx)
		var names []string
		for _, r := range table {
			names = append(names, r.Name+"|"+r.Severity)
		}
		slices.Sort(names)
		if first == nil {
			first = names
		} else if !slices.Equal(names, first) {
			t.Fatalf("order %d named them %q; the first, %q", i, names, first)
		}
	}
}

// The ledger keeps seconds: a period is of whole seconds, so periods that
// meet share none; firings given finer are kept to the second, and sent
// again they are found, not added again.
func TestRecordedFractions(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.PutFirings(ctx, "cw", t0, t0.Add(time.Hour+500*time.Millisecond), nil, FiringsCopier, opts); err == nil || !strings.Contains(err.Error(), "whole seconds") {
		t.Errorf("a period ending within a second: %v", err)
	}
	fs := []firing.Firing{{Rule: "A", Start: t0.Add(time.Hour + 200*time.Millisecond), End: t0.Add(time.Hour + 300*time.Millisecond)}}
	for range 3 {
		putFirings(t, l, "cw", t0, t0.Add(2*time.Hour), fs)
	}
	putFirings(t, l, "cw", t0.Add(2*time.Hour), t0.Add(3*time.Hour), []firing.Firing{{Rule: "A", Start: t0.Add(2*time.Hour + 700*time.Millisecond)}})
	got, _ := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(3 * time.Hour)})
	if len(got) != 2 {
		t.Errorf("%d firings, want 2: %+v", len(got), got)
	}
}

// What a team owns a recorded rule by is worked out in time that grows with
// its alerts, not their square.
func TestRecordedSharedLabelsScale(t *testing.T) {
	if testing.Short() || race {
		t.Skip("short, or the race detector")
	}
	ctx := context.Background()
	l := open(t, time.UTC)
	var fs []firing.Firing
	for i := range 30000 {
		fs = append(fs, firing.Firing{Rule: "cpu", Labels: map[string]string{"service": "checkout", "instance": fmt.Sprintf("i-%05d", i)},
			Start: t0.Add(time.Duration(i) * time.Second)})
	}
	putFirings(t, l, "cw", t0, t0.Add(24*time.Hour), fs)
	began := time.Now()
	got, err := l.SharedLabels(ctx)
	took := time.Since(began)
	if err != nil || !reflect.DeepEqual(got["cpu"], map[string]string{"service": "checkout"}) {
		t.Errorf("shared: %v %v", got, err)
	}
	if took > 2*time.Second {
		t.Errorf("30,000 alerts took %s", took)
	}
	t.Logf("30,000 alerts: %s", took.Round(time.Millisecond))
}

// What a source's copies cover without a gap: periods that meet or overlap
// run together; one after a gap starts it again, until the gap is copied
// in too.
func TestCoverage(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	day := 24 * time.Hour
	put := func(from, to time.Duration) {
		putFirings(t, l, "cw", t0.Add(from), t0.Add(to), nil)
	}
	covers := func(what string, from, to time.Duration) {
		t.Helper()
		f, u, err := l.Coverage(ctx, "cw")
		if err != nil || !f.Equal(t0.Add(from)) || !u.Equal(t0.Add(to)) {
			t.Errorf("%s: %v to %v, %v", what, f.Sub(t0), u.Sub(t0), err)
		}
	}
	if f, _, err := l.Coverage(ctx, "cw"); err != nil || !f.IsZero() {
		t.Errorf("never written: %v %v", f, err)
	}
	put(0, day)
	put(day, 2*day)
	put(12*time.Hour, 36*time.Hour)
	covers("met and overlapping", 0, 2*day)
	put(3*day, 4*day)
	covers("after a gap", 3*day, 4*day)
	put(2*day, 3*day)
	covers("the gap copied in", 0, 4*day)
	// Each a gap apart: the latest maxSpans kept.
	for i := range maxSpans + 2 {
		at := -30*day + time.Duration(2*i)*time.Hour
		put(at, at+time.Hour)
	}
	if spans, err := l.spans(ctx, l.r, "cw"); err != nil || len(spans) != maxSpans || spans[0][0] != t0.Add(-30*day+6*time.Hour).Unix() {
		t.Errorf("the latest %d spans: %d %v", maxSpans, len(spans), err)
	}
	covers("the earlier spans let go", 0, 4*day)
	putFirings(t, l, "oci", t0, t0.Add(day), nil)
	// A source forgotten, what its copies covered goes with it.
	if err := l.Prune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if v, err := l.meta(ctx, l.r, "copied.cw"); err != nil || v != "" {
		t.Errorf("forgotten: %q %v", v, err)
	}
}
