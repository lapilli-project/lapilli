package ledger

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// mutedWithin is two minutes, as two evaluations a minute apart give it.
func mutedWithin(string) time.Duration { return 2 * time.Minute }

// A mute told of again, every few seconds, after a failure, or by another
// replica, is one row an hour: from the first of them to the last. One
// that comes back the next hour is a row of that hour.
func TestKeepMutesOnce(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(3*time.Hour + 10*time.Minute)
	m := Mute{At: at, Rule: "DiskFull", Labels: map[string]string{"alertname": "DiskFull", "instance": "db-2"}, Kind: MutedSilence, Ref: "c9839a90-d7ae-456f-b386-08e896968f39"}
	gen := l.Gen()
	added, errs, err := l.KeepMutes(ctx, []Mute{m})
	if err != nil || errs[0] != nil || added != 1 {
		t.Fatalf("kept %d: %v %v", added, errs, err)
	}
	if l.Gen() == gen {
		t.Error("a new mute isn't a page's: the pages kept stay")
	}
	gen = l.Gen()
	again := m
	again.At = at.Add(5 * time.Second)
	later := m
	later.At = at.Add(20 * time.Minute)
	if added, _, err := l.KeepMutes(ctx, []Mute{m, again, again, later}); err != nil || added != 0 {
		t.Fatalf("the same mute again: %d added, %v", added, err)
	}
	if l.Gen() != gen {
		t.Error("a mute made longer threw away the pages kept")
	}
	next := m
	next.At = at.Add(time.Hour)
	if added, _, err := l.KeepMutes(ctx, []Mute{next}); err != nil || added != 1 {
		t.Fatalf("the next hour: %d added, %v", added, err)
	}
	var rows int
	var first, last int64
	l.r.QueryRowContext(ctx, "SELECT count(*) FROM mutes").Scan(&rows)
	l.r.QueryRowContext(ctx, "SELECT first_at, last_at FROM mutes WHERE hour = ?", at.Unix()/3600).Scan(&first, &last)
	if rows != 2 || first != at.Unix() || last != at.Add(20*time.Minute).Unix() {
		t.Errorf("%d rows; the first hour's from %s to %s", rows, unix(first), unix(last))
	}
	if n, err := l.MutesSince(ctx, at.Add(30*time.Minute)); err != nil || n != 1 {
		t.Errorf("mutes since: %d %v", n, err)
	}
	if ok, err := l.HasMutes(ctx); !ok || err != nil {
		t.Errorf("has mutes: %v %v", ok, err)
	}
}

// What the ledger won't keep of a mute is refused, that one alone: bounded
// as a notification's alerts are.
func TestKeepMutesRefuses(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	good := Mute{At: t0, Rule: "Errors", Labels: map[string]string{"alertname": "Errors"}, Kind: MutedInhibition, Ref: "16118892496979754547"}
	bad := map[string]func(m *Mute){
		"no time":                     func(m *Mute) { m.At = time.Time{} },
		"no rule":                     func(m *Mute) { m.Rule = "" },
		"a kind it can't be":          func(m *Mute) { m.Kind = "routed" },
		"a ref too long":              func(m *Mute) { m.Ref = strings.Repeat("a", maxMuteRef+1) },
		"a ref that doesn't show":     func(m *Mute) { m.Ref = "a​b" },
		"labels over 4 KB":            func(m *Mute) { m.Labels = map[string]string{"note": strings.Repeat("x", 5000)} },
		"labels over 4 KB as kept":    func(m *Mute) { m.Labels = map[string]string{"alertname": "Errors", "note": strings.Repeat("<", 1000)} },
		"too many labels":             func(m *Mute) { m.Labels = manyLabels(maxAlertLabels + 1) },
		"a rule that doesn't show":    func(m *Mute) { m.Rule = "Err\nors" },
		"a last time in another hour": func(m *Mute) { m.Last = m.At.Add(time.Hour) },
	}
	ms := []Mute{good}
	var names []string
	for name, change := range bad {
		m := good
		m.Labels = map[string]string{"alertname": "Errors"}
		change(&m)
		ms = append(ms, m)
		names = append(names, name)
	}
	added, errs, err := l.KeepMutes(ctx, ms)
	if err != nil || added != 1 || errs[0] != nil {
		t.Fatalf("kept %d: %v %v", added, errs[0], err)
	}
	for i, name := range names {
		if !errors.Is(errs[i+1], ErrBadMute) {
			t.Errorf("%s: %v", name, errs[i+1])
		}
	}
}

func manyLabels(n int) map[string]string {
	m := map[string]string{}
	for i := range n {
		m["l"+strings.Repeat("x", i)] = "v"
	}
	return m
}

// A firing is muted by the mutes of its rule whose labels are all of its
// own and more, told of while it was on, or within two evaluations of it:
// a silence before an inhibition, and a notification's word that doesn't
// say which only where nothing else does.
func TestMutedOf(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := t0.Add(10 * time.Hour)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "DiskFull", Labels: map[string]string{"instance": "db-1"}, Start: start, End: start.Add(30 * time.Minute)},
		{Rule: "DiskFull", Labels: map[string]string{"instance": "db-2"}, Start: start, End: start.Add(30 * time.Minute)},
		{Rule: "DiskFull", Labels: map[string]string{"instance": "db-3"}, Start: start, End: start.Add(30 * time.Minute)},
		{Rule: "DiskFull", Labels: map[string]string{"instance": "db-4"}, Start: start.Add(3 * time.Hour), End: start.Add(4 * time.Hour)},
		{Rule: "Errors", Labels: map[string]string{"instance": "db-1"}, Start: start, End: start.Add(30 * time.Minute)},
	})
	labels := func(instance string) map[string]string {
		return map[string]string{"alertname": "DiskFull", "instance": instance, "cluster": "eu-1"}
	}
	in := start.Add(10 * time.Minute)
	ms := []Mute{
		// db-1: silenced, inhibited, and a notification that doesn't say.
		{At: in, Rule: "DiskFull", Labels: labels("db-1"), Kind: MutedInhibition, Ref: "42"},
		{At: in.Add(time.Minute), Rule: "DiskFull", Labels: labels("db-1"), Kind: MutedSilence, Ref: "s-1"},
		{At: in, Rule: "DiskFull", Labels: labels("db-1"), Kind: MutedEither},
		// db-2: only a notification's word.
		{At: in, Rule: "DiskFull", Labels: labels("db-2"), Kind: MutedEither},
		// db-3: muted an hour after it ended, of no use to it.
		{At: start.Add(90 * time.Minute), Rule: "DiskFull", Labels: labels("db-3"), Kind: MutedSilence, Ref: "s-3"},
		// db-4: told of a minute before it started, within two evaluations.
		{At: start.Add(3*time.Hour - time.Minute), Rule: "DiskFull", Labels: labels("db-4"), Kind: MutedSilence, Ref: "s-4"},
		// The same silence of db-1 on another cluster: one mute of it.
		{At: in.Add(-5 * time.Minute), Rule: "DiskFull", Labels: map[string]string{"alertname": "DiskFull", "instance": "db-1", "cluster": "eu-2"}, Kind: MutedSilence, Ref: "s-1"},
	}
	if _, _, err := l.KeepMutes(ctx, ms); err != nil {
		t.Fatal(err)
	}
	fs, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	if err != nil || len(fs) != 5 {
		t.Fatalf("%d firings: %v", len(fs), err)
	}
	of, err := l.MutedOf(ctx, fs, mutedWithin, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, f := range fs {
		for _, m := range of[f.ID] {
			got[f.Rule+" "+f.Labels["instance"]] = append(got[f.Rule+" "+f.Labels["instance"]], m.Kind+":"+m.Ref+" "+m.At.Format("15:04")+"-"+m.Last.Format("15:04"))
		}
	}
	want := map[string][]string{
		"DiskFull db-1": {"silence:s-1 10:05-10:11", "inhibition:42 10:10-10:10"},
		"DiskFull db-2": {"either: 10:10-10:10"},
		"DiskFull db-4": {"silence:s-4 12:59-12:59"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("muted:\n%v\nwant\n%v", got, want)
	}
	// A firing still on is muted up to now: what is told of about now.
	on := fs[slices.IndexFunc(fs, func(f Firing) bool { return f.Labels["instance"] == "db-3" })]
	on.End = time.Time{}
	if of, err := l.MutedOf(ctx, []Firing{on}, mutedWithin, start.Add(95*time.Minute)); err != nil || len(of[on.ID]) != 1 {
		t.Errorf("a firing still on: %+v %v", of, err)
	}
}

// Mutes are kept for --keep, as notifications are, and a firing's are
// found through mutes_rule, not by reading them all.
func TestMutesPrunedAndIndexed(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	old := Mute{At: t0, Rule: "Errors", Labels: map[string]string{"alertname": "Errors"}, Kind: MutedSilence, Ref: "s-1"}
	recent := old
	recent.At = t0.Add(48 * time.Hour)
	if _, _, err := l.KeepMutes(ctx, []Mute{old, recent}); err != nil {
		t.Fatal(err)
	}
	if err := l.Prune(ctx, t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	l.r.QueryRowContext(ctx, "SELECT count(*) FROM mutes").Scan(&n)
	if n != 1 {
		t.Errorf("%d mutes after pruning, want the recent one", n)
	}
	if p := plan(t, l, qMutesOf, "Errors", 1, 10); scansAll(l, p, "mutes") || !strings.Contains(p, "mutes_rule") {
		t.Errorf("a firing's mutes are read through: %s", p)
	}
}

// A rule's hour holds maxMutesPerRuleHour rows: one more is passed over,
// one kept goes on being made longer. A firing's silence is found among a
// thousand other alerts' mutes of its rule told of before it, and what is
// read for it is the hours about its start and its end, not those between.
func TestMutedOfBounded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := t0.Add(10 * time.Hour)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "DiskFull", Labels: map[string]string{"instance": "db-1"}, Start: start, End: start.Add(4 * time.Hour)}})
	real := Mute{At: start.Add(3 * time.Minute), Rule: "DiskFull", Labels: map[string]string{"alertname": "DiskFull", "instance": "db-1"}, Kind: MutedSilence, Ref: "s-1"}
	if _, _, err := l.KeepMutes(ctx, []Mute{real}); err != nil {
		t.Fatal(err)
	}
	junk := func(at time.Time, n int) []Mute {
		var ms []Mute
		for i := range n {
			ms = append(ms, Mute{At: at, Rule: "DiskFull", Labels: map[string]string{"alertname": "DiskFull", "instance": "junk-" + strconv.Itoa(i)}, Kind: MutedSilence, Ref: "s-9"})
		}
		return ms
	}
	// A thousand before it started, within two evaluations of its start,
	// in the hour before; a thousand in the hours between, and one more,
	// which its hour won't hold.
	before := junk(start.Add(-90*time.Second), maxMutesPerRuleHour)
	between := junk(start.Add(2*time.Hour+30*time.Minute), maxMutesPerRuleHour+1)
	added, errs, err := l.KeepMutes(ctx, append(before, between...))
	if err != nil || added != 2*maxMutesPerRuleHour {
		t.Fatalf("added %d: %v", added, err)
	}
	if last := errs[len(errs)-1]; !errors.Is(last, ErrMutesFull) || slices.ContainsFunc(errs[:len(errs)-1], func(e error) bool { return e != nil }) {
		t.Errorf("the hour's one more: %v", last)
	}
	// One kept goes on being made longer in a full hour.
	longer := between[0]
	longer.At = longer.At.Add(10 * time.Minute)
	if _, errs, err := l.KeepMutes(ctx, []Mute{longer}); err != nil || errs[0] != nil {
		t.Errorf("a row kept, made longer in a full hour: %v %v", errs[0], err)
	}
	fs, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	if err != nil || len(fs) != 1 {
		t.Fatalf("%d firings: %v", len(fs), err)
	}
	was := muteRowsRead.Load()
	of, err := l.MutedOf(ctx, fs, mutedWithin, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ms := of[fs[0].ID]; len(ms) != 1 || ms[0].Ref != "s-1" {
		t.Errorf("the firing's mutes: %+v", ms)
	}
	// The hour before its start (the thousand), its start's (its own), and
	// its end's: none of the thousand between.
	if read := muteRowsRead.Load() - was; read != maxMutesPerRuleHour+1 {
		t.Errorf("read %d rows of mutes, want %d", read, maxMutesPerRuleHour+1)
	}
}
