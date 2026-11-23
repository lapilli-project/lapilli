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

// Of a span's firings no notification told of, those muted are counted by
// what their pages say muted them first, and those nothing was told of
// muting, of rules notifications were counted for by then; those told of,
// of rules not asked for, or started outside the span, are in none.
func TestUnpaged(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := t0.Add(10 * time.Hour)
	disk := func(instance string, at time.Time) firing.Firing {
		return firing.Firing{Rule: "DiskFull", Labels: map[string]string{"instance": instance}, Start: at, End: at.Add(30 * time.Minute)}
	}
	record(t, l, t0, t0.Add(48*time.Hour), []firing.Firing{
		disk("db-1", start), disk("db-2", start), disk("db-3", start), disk("db-4", start), disk("db-5", start),
		disk("db-6", t0.Add(-time.Hour+time.Second).Add(25*time.Hour)), // the next day: outside the span
		{Rule: "Errors", Labels: map[string]string{"instance": "web-1"}, Start: start, End: start.Add(time.Hour)},
		{Rule: "Quiet", Labels: map[string]string{"instance": "web-1"}, Start: start, End: start.Add(time.Hour)},
		{Rule: "Elsewhere", Labels: map[string]string{"instance": "web-1"}, Start: start, End: start.Add(time.Hour)},
	})
	labels := func(rule, instance string) map[string]string {
		return map[string]string{"alertname": rule, "instance": instance}
	}
	in := start.Add(5 * time.Minute)
	if _, _, err := l.KeepMutes(ctx, []Mute{
		{At: in, Rule: "DiskFull", Labels: labels("DiskFull", "db-1"), Kind: MutedSilence, Ref: "s-1"},
		{At: in, Rule: "DiskFull", Labels: labels("DiskFull", "db-1"), Kind: MutedEither},
		{At: in, Rule: "DiskFull", Labels: labels("DiskFull", "db-2"), Kind: MutedInhibition, Ref: "42"},
		{At: in, Rule: "DiskFull", Labels: labels("DiskFull", "db-3"), Kind: MutedEither},
		// db-5 was told of: muted or not, it paged someone.
		{At: in, Rule: "DiskFull", Labels: labels("DiskFull", "db-5"), Kind: MutedSilence, Ref: "s-1"},
		{At: in, Rule: "DiskFull", Labels: labels("DiskFull", "db-6"), Kind: MutedSilence, Ref: "s-1"},
		{At: in, Rule: "Elsewhere", Labels: labels("Elsewhere", "web-1"), Kind: MutedSilence, Ref: "s-2"},
	}); err != nil {
		t.Fatal(err)
	}
	fs, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var told int64
	for _, f := range fs {
		if f.Labels["instance"] == "db-5" {
			told = f.ID
		}
	}
	got, err := l.Unpaged(ctx, t0, t0.Add(24*time.Hour), func(rule string) bool { return rule != "Elsewhere" },
		func(id int64) bool { return id == told }, func(rule string, _ time.Time) bool { return rule != "Quiet" },
		mutedWithin, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// db-1 by its silence; db-2 by its inhibition; db-3 by either; db-4
	// and Errors by nothing; Quiet's isn't counted, Elsewhere's isn't
	// asked for.
	if want := (Unpaged{Silence: 1, Inhibition: 1, Either: 1, Nothing: 2}); got != want || got.Total() != 5 {
		t.Errorf("unpaged: %+v, want %+v", got, want)
	}
}

// What is read to count the firings that paged no one is what MutedOf
// reads for those of rules with a mute about them, the hours about their
// starts and ends: not a rule's mutes with no such firing, nor its hours
// between, however many. Past UnpagedMost rows, of firings read with their
// labels and of mutes looked at, the rest aren't read, and the count says
// so: a forged mute beside many firings reads no more of them.
func TestUnpagedBounded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := t0.Add(10 * time.Hour)
	later := start.Add(8 * time.Hour)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "DiskFull", Labels: map[string]string{"instance": "db-1"}, Start: start, End: start.Add(4 * time.Hour)},
		{Rule: "DiskFull", Labels: map[string]string{"instance": "db-2"}, Start: later, End: later.Add(4 * time.Hour)},
		{Rule: "Errors", Labels: map[string]string{"instance": "web-1"}, Start: start, End: start.Add(4 * time.Hour)},
	})
	junk := func(rule string, at time.Time, n int) []Mute {
		var ms []Mute
		for i := range n {
			ms = append(ms, Mute{At: at, Rule: rule, Labels: map[string]string{"alertname": rule, "instance": "junk-" + strconv.Itoa(i)}, Kind: MutedSilence, Ref: "s-9"})
		}
		return ms
	}
	real := []Mute{
		{At: start.Add(3 * time.Minute), Rule: "DiskFull", Labels: map[string]string{"alertname": "DiskFull", "instance": "db-1"}, Kind: MutedSilence, Ref: "s-1"},
		{At: later.Add(3 * time.Minute), Rule: "DiskFull", Labels: map[string]string{"alertname": "DiskFull", "instance": "db-2"}, Kind: MutedSilence, Ref: "s-1"},
	}
	var ms []Mute
	// A thousand in the hour before db-1 started, within two evaluations
	// of it; a thousand in an hour between its start and its end; and a
	// thousand of Errors, told of, in each hour of the day.
	ms = append(ms, junk("DiskFull", start.Add(-90*time.Second), maxMutesPerRuleHour)...)
	ms = append(ms, junk("DiskFull", start.Add(2*time.Hour+30*time.Minute), maxMutesPerRuleHour)...)
	for h := range 24 {
		ms = append(ms, junk("Errors", t0.Add(time.Duration(h)*time.Hour), maxMutesPerRuleHour)...)
	}
	if _, _, err := l.KeepMutes(ctx, append(real, ms...)); err != nil {
		t.Fatal(err)
	}
	fs, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour), Rule: "Errors"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("%d firings: %v", len(fs), err)
	}
	told := fs[0].ID
	count := func() (Unpaged, int64) {
		t.Helper()
		was := muteRowsRead.Load()
		u, err := l.Unpaged(ctx, t0, t0.Add(24*time.Hour), func(string) bool { return true }, func(id int64) bool { return id == told },
			func(string, time.Time) bool { return true }, mutedWithin, t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return u, muteRowsRead.Load() - was
	}
	// db-1's hour before its start (the thousand) and its start's (its
	// own); db-2's start's (its own); the ends' hours, none.
	if u, read := count(); u != (Unpaged{Silence: 2}) || read != maxMutesPerRuleHour+2 {
		t.Errorf("%+v, %d rows of mutes read, want %d", u, read, maxMutesPerRuleHour+2)
	}
	// Room for DiskFull's two firings and db-1's four hours, each as the
	// most it can hold, and not db-2's four after them: db-2 isn't counted.
	was := UnpagedMost
	t.Cleanup(func() { UnpagedMost = was })
	UnpagedMost = 2 + 4*2*maxMutesPerRuleHour
	if u, read := count(); u != (Unpaged{Silence: 1, Partial: true}) || read != maxMutesPerRuleHour+1 {
		t.Errorf("past the most: %+v, %d rows read", u, read)
	}
	// Room for none: nothing read, nothing counted.
	UnpagedMost = 1
	if u, read := count(); u != (Unpaged{Partial: true}) || read != 0 {
		t.Errorf("no room: %+v, %d rows read", u, read)
	}
	if p := plan(t, l, qMutedAny, "Errors", 1, 10); scansAll(l, p, "mutes") || !strings.Contains(p, "mutes_rule") {
		t.Errorf("whether a rule has mutes is looked up through: %s", p)
	}

	// Many firings of a rule, all but one told of, and one mute beside
	// them: no more of them are read with their labels than there is room
	// for, and one more to say there were more.
	l = open(t, time.UTC)
	var many []firing.Firing
	for i := range 300 {
		at := t0.Add(time.Duration(i) * time.Minute)
		many = append(many, firing.Firing{Rule: "Flapping", Labels: map[string]string{"instance": "web-" + strconv.Itoa(i)}, Start: at, End: at.Add(30 * time.Second)})
	}
	record(t, l, t0, t0.Add(24*time.Hour), many)
	if _, _, err := l.KeepMutes(ctx, []Mute{{At: t0.Add(time.Minute), Rule: "Flapping", Labels: map[string]string{"alertname": "Flapping", "instance": "web-0"}, Kind: MutedSilence, Ref: "s-1"}}); err != nil {
		t.Fatal(err)
	}
	fs, err = l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	if err != nil || len(fs) != 300 {
		t.Fatalf("%d firings: %v", len(fs), err)
	}
	var untold int64
	for _, f := range fs {
		if f.Labels["instance"] == "web-0" {
			untold = f.ID
		}
	}
	flapping := func() (Unpaged, int64, int64) {
		t.Helper()
		firings, mutes := unpagedFiringsRead.Load(), muteRowsRead.Load()
		u, err := l.Unpaged(ctx, t0, t0.Add(24*time.Hour), func(string) bool { return true }, func(id int64) bool { return id != untold },
			func(string, time.Time) bool { return true }, mutedWithin, t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return u, unpagedFiringsRead.Load() - firings, muteRowsRead.Load() - mutes
	}
	UnpagedMost = was
	if u, firings, mutes := flapping(); u != (Unpaged{Silence: 1}) || firings != 300 || mutes != 1 {
		t.Errorf("with room: %+v, %d rows of firings read, %d of mutes", u, firings, mutes)
	}
	UnpagedMost = 50
	if u, firings, mutes := flapping(); u != (Unpaged{Partial: true}) || firings != 51 || mutes != 0 {
		t.Errorf("past the most: %+v, %d rows of firings read, %d of mutes", u, firings, mutes)
	}
}

// The span's firings are read without their labels a page a statement,
// each once, those that started in one second among them, and
// UnpagedFirings at most, in the order they started: past it the rest
// aren't read, are in none of the counts, and the count says so.
func TestUnpagedSpanBounded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	wasPage, wasMost := unpagedPage, UnpagedFirings
	t.Cleanup(func() { unpagedPage, UnpagedFirings = wasPage, wasMost })
	var many []firing.Firing
	for i := range 300 {
		at := t0.Add(time.Duration(i) * time.Minute)
		many = append(many, firing.Firing{Rule: "Flapping", Labels: map[string]string{"instance": "web-" + strconv.Itoa(i)}, Start: at, End: at.Add(30 * time.Second)})
	}
	// And 150 of another rule's that started in one second.
	for i := range 150 {
		many = append(many, firing.Firing{Rule: "Burst", Labels: map[string]string{"instance": "db-" + strconv.Itoa(i)}, Start: t0.Add(2*time.Hour + 30*time.Second), End: t0.Add(3 * time.Hour)})
	}
	record(t, l, t0, t0.Add(24*time.Hour), many)
	if _, _, err := l.KeepMutes(ctx, []Mute{{At: t0.Add(time.Minute), Rule: "Flapping", Labels: map[string]string{"alertname": "Flapping", "instance": "web-0"}, Kind: MutedSilence, Ref: "s-1"}}); err != nil {
		t.Fatal(err)
	}
	fs, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour), Limit: 1000})
	if err != nil || len(fs) != 450 {
		t.Fatalf("%d firings: %v", len(fs), err)
	}
	// No notification told of web-0, muted, of web-200, nor of Burst's.
	untold := map[int64]bool{}
	for _, f := range fs {
		if f.Rule == "Burst" || f.Labels["instance"] == "web-0" || f.Labels["instance"] == "web-200" {
			untold[f.ID] = true
		}
	}
	count := func() (Unpaged, int64, int64) {
		t.Helper()
		rows, pages := unpagedSpanRead.Load(), unpagedSpanPages.Load()
		u, err := l.Unpaged(ctx, t0, t0.Add(24*time.Hour), func(string) bool { return true }, func(id int64) bool { return !untold[id] },
			func(string, time.Time) bool { return true }, mutedWithin, t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return u, unpagedSpanRead.Load() - rows, unpagedSpanPages.Load() - pages
	}
	whole := Unpaged{Silence: 1, Nothing: 151}
	if u, rows, pages := count(); u != whole || rows != 450 || pages != 1 {
		t.Errorf("in one statement: %+v, %d rows in %d statements", u, rows, pages)
	}
	// A page of 64: eight statements, the second in which Burst's began
	// across three of them, and each firing read once.
	unpagedPage = 64
	if u, rows, pages := count(); u != whole || rows != 450 || pages != 8 {
		t.Errorf("a page of 64: %+v, %d rows in %d statements", u, rows, pages)
	}
	// 200 at most: Flapping's first 121 and 79 of Burst's, which started
	// two hours in. web-200 and the rest of Burst's aren't read, nor
	// counted.
	UnpagedFirings = 200
	if u, rows, pages := count(); u != (Unpaged{Silence: 1, Nothing: 79, Partial: true}) || rows != 200 || pages != 4 {
		t.Errorf("200 at most: %+v, %d rows in %d statements", u, rows, pages)
	}
	// As many as there are: all read, and not said cut.
	UnpagedFirings = 450
	if u, rows, _ := count(); u != whole || rows != 450 {
		t.Errorf("as many as there are: %+v, %d rows", u, rows)
	}
	if p := plan(t, l, qUnpagedSpan, t0.Unix(), -1, t0.Add(24*time.Hour).Unix(), 10); scansAll(l, p, "firings") || !strings.Contains(p, "firings_start") {
		t.Errorf("the span's firings are read through: %s", p)
	}
}
