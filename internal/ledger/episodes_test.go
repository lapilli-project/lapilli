package ledger

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/verdict"
)

// episodeWorld is three weeks of a busy set of rules, each with its alerts
// on a few pods of one of a few services, flapping, overlapping, and
// crossing midnight, with incidents that catch some; and a quiet window.
func episodeWorld(seed uint64) ([]firing.Firing, []incident.Incident) {
	fs, incs := world(seed, 300)
	// Some on pods, overlapping, and some across midnight.
	for d := range 7 {
		day := t0.Add(time.Duration(d) * 24 * time.Hour)
		for pod := range 4 {
			fs = append(fs, firing.Firing{Rule: "PodRestarting", Labels: map[string]string{"service": "search", "pod": fmt.Sprint(pod)},
				Start: day.Add(23*time.Hour + 50*time.Minute + time.Duration(pod)*time.Minute), End: day.Add(24*time.Hour + 20*time.Minute)})
		}
	}
	return fs, incs
}

// The ledger's episodes are verdict.Judge's: each firing's, numbered in the
// report, and each rule's count, over a week recorded in two runs and
// judged again by an incident written down and a change of the match.
func TestEpisodesSameAsJudge(t *testing.T) {
	ctx := context.Background()
	for seed := uint64(1); seed <= 3; seed++ {
		l := open(t, time.UTC)
		fs, incs := episodeWorld(seed)
		incs = append([]incident.Incident{keepAll}, incs...)
		if _, err := l.SyncFile(ctx, incs, opts); err != nil {
			t.Fatal(err)
		}
		mid := t0.Add(80 * time.Hour)
		var first []firing.Firing
		for _, f := range fs {
			if f.Start.Before(mid) {
				g := f
				if !g.End.IsZero() && g.End.After(mid) {
					g.End = time.Time{}
				}
				first = append(first, g)
			}
		}
		record(t, l, t0, mid, first)
		record(t, l, mid.Add(-2*time.Hour), t0.Add(8*24*time.Hour), fs)
		o := opts
		o.From, o.To = t0, t0.Add(7*24*time.Hour)
		check := func(what string, o verdict.Options) {
			t.Helper()
			var in []firing.Firing
			for _, f := range fs {
				if !f.Start.Before(o.From) && f.Start.Before(o.To) {
					in = append(in, f)
				}
			}
			want := verdict.Judge(nil, slices.Clone(in), incs, o)
			got, err := l.Report(ctx, o.From, o.To, o, true)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(norm(got.Rules), norm(want.Rules)) {
				t.Errorf("seed %d, %s: rules\n got %+v\nwant %+v", seed, what, got.Rules, want.Rules)
			}
			if len(got.Firings) != len(want.Firings) {
				t.Fatalf("seed %d, %s: %d firings, want %d", seed, what, len(got.Firings), len(want.Firings))
			}
			// Which firings make each episode: of each firing, the start
			// of its episode's first, and its day.
			if g, w := episodeParts(got.Firings), episodeParts(want.Firings); !reflect.DeepEqual(g, w) {
				for k := range w {
					if g[k] != w[k] {
						t.Errorf("seed %d, %s: %s is of an episode from %s, Judge's from %s", seed, what, k, g[k], w[k])
						break
					}
				}
			}
		}
		check("recorded", o)
		// An incident written down, judged again over its span.
		if _, err := l.Put(ctx, incident.Incident{ID: "LATE", Start: t0.Add(50 * time.Hour), End: t0.Add(51 * time.Hour),
			Labels: incident.Labels{"service": {"search"}}}, "ana", o); err != nil {
			t.Fatal(err)
		}
		incs = append(incs, incident.Incident{ID: "LATE", Start: t0.Add(50 * time.Hour), End: t0.Add(51 * time.Hour), Labels: incident.Labels{"service": {"search"}}})
		check("an incident written down", o)
		// Matched on the pod too: the pods' alerts of one service, one
		// episode before, are one each.
		o.Match = []string{"service", "pod"}
		if err := l.JudgeAll(ctx, o, nil); err != nil {
			t.Fatal(err)
		}
		check("matched on the pod too", o)
	}
}

// episodeParts is, of each firing, by its rule, labels and start, the start
// of its episode's first firing and its day: which firings make each
// episode, whatever their order at one time.
func episodeParts(fs []verdict.Judged) map[string]string {
	first := map[int]time.Time{}
	for _, j := range fs {
		if t, ok := first[j.Episode]; !ok || j.Start.Before(t) {
			first[j.Episode] = j.Start
		}
	}
	out := map[string]string{}
	for _, j := range fs {
		out[fmt.Sprint(j.Rule, j.Labels, j.Start.Unix())] = first[j.Episode].Format(time.RFC3339) + " " + j.Day
	}
	return out
}

// withoutEpisodes is a report's rules without their episodes.
func withoutEpisodes(rs []verdict.RuleResult) []verdict.RuleResult {
	out := slices.Clone(rs)
	for i := range out {
		out[i].Episodes = verdict.EpisodeCount{}
	}
	return out
}

// A quiet window's firing takes no part in an episode: it neither starts
// nor continues one, and the episodes before and after it are two.
func TestEpisodesExcused(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncFile(ctx, []incident.Incident{keepAll}, opts); err != nil {
		t.Fatal(err)
	}
	svc := map[string]string{"service": "search"}
	fs := []firing.Firing{
		{Rule: "Flap", Labels: svc, Start: t0.Add(10 * time.Hour), End: t0.Add(10*time.Hour + 5*time.Minute)},
		{Rule: "Flap", Labels: svc, Start: t0.Add(11 * time.Hour), End: t0.Add(11*time.Hour + 5*time.Minute)},
		{Rule: "Flap", Labels: svc, Start: t0.Add(12 * time.Hour), End: t0.Add(12*time.Hour + 5*time.Minute)},
	}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	rep, err := l.Report(ctx, t0, t0.Add(24*time.Hour), opts, true)
	if err != nil || len(rep.Rules) != 1 || rep.Rules[0].Episodes.Total != 1 {
		t.Fatalf("three flaps an hour apart: %+v %v", rep.Rules, err)
	}
	if _, err := l.PutWindow(ctx, Window{Title: "deploy", Start: t0.Add(11*time.Hour - time.Minute), End: t0.Add(11*time.Hour + 10*time.Minute)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	rep, err = l.Report(ctx, t0, t0.Add(24*time.Hour), opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if r := rep.Rules[0]; r.Excused != 1 || r.Firings != 2 || r.Episodes.Total != 2 {
		t.Errorf("the middle one excused: %+v", r)
	}
	if got := []int{rep.Firings[0].Episode, rep.Firings[1].Episode}; !slices.Equal(got, []int{1, 2}) {
		t.Errorf("episodes %v, want two", got)
	}
}

// An episode never crosses midnight: a write that judges one day walks
// the firings of that day alone, and those of the day after, which
// overlap them, keep their own episode.
func TestEpisodesMidnight(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	svc := map[string]string{"service": "search"}
	fs := []firing.Firing{
		{Rule: "Flap", Labels: svc, Start: t0.Add(23*time.Hour + 50*time.Minute), End: t0.Add(24*time.Hour + 30*time.Minute)},
		{Rule: "Flap", Labels: map[string]string{"service": "search", "pod": "b"}, Start: t0.Add(24*time.Hour + 5*time.Minute), End: t0.Add(24*time.Hour + 10*time.Minute)},
	}
	walked := 0
	episodesWalked = func(n int) { walked += n }
	defer func() { episodesWalked = nil }()
	record(t, l, t0, t0.Add(48*time.Hour), fs)
	rep, err := l.Report(ctx, t0, t0.Add(48*time.Hour), opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := []int{rep.Firings[0].Episode, rep.Firings[1].Episode}; !slices.Equal(got, []int{1, 2}) {
		t.Errorf("across midnight: %v, want two", got)
	}
	if rep.Rules[0].Episodes.Total != 2 {
		t.Errorf("counted %+v", rep.Rules[0].Episodes)
	}
	// An incident written down on the second day: its write walks that day
	// and the one before (its lead's), never further.
	walked = 0
	if _, err := l.Put(ctx, incident.Incident{ID: "INC", Start: t0.Add(24*time.Hour + 6*time.Minute), End: t0.Add(25 * time.Hour), Labels: incident.Labels{"service": {"search"}}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if walked != 2 {
		t.Errorf("an incident's write walked %d firings, want the two of its days", walked)
	}
	rep, _ = l.Report(ctx, t0, t0.Add(48*time.Hour), opts, true)
	if e := rep.Rules[0].Episodes; e.Total != 2 || e.White != 1 {
		t.Errorf("one white of two: %+v", e)
	}
}

// Opened in another zone, the ledger takes every episode back, says the
// weeks' episodes aren't counted, and counts them again a week a write,
// from the newest back, once; then they are as a ledger kept in that zone
// from the first counts them, and no other number moved.
func TestEpisodesZone(t *testing.T) {
	ctx := context.Background()
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip(err)
	}
	fs, incs := episodeWorld(4)
	incs = append([]incident.Incident{keepAll}, incs...)
	fill := func(l *Ledger) {
		if _, err := l.SyncFile(ctx, incs, opts); err != nil {
			t.Fatal(err)
		}
		// Two weeks of them, the second the first again a week on.
		var more []firing.Firing
		for _, f := range fs {
			g := f
			g.Start, g.End = f.Start.Add(7*24*time.Hour), f.End
			if !g.End.IsZero() {
				g.End = g.End.Add(7 * 24 * time.Hour)
			}
			more = append(more, g)
		}
		record(t, l, t0, t0.Add(15*24*time.Hour), append(slices.Clone(fs), more...))
	}
	path := place(t)
	l, err := openAt(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	fill(l)
	before, err := l.WeeksBy(ctx, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	l, err = openAt(ctx, path, seoul)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	o := opts
	o.Zone = seoul
	if w, ok, err := l.EpisodesFrom(ctx); err != nil || !ok {
		t.Fatalf("opened in Seoul, the episodes are counted: %v %v %v", w, ok, err)
	}
	rep, err := l.Report(ctx, t0, t0.Add(7*24*time.Hour), o, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.EpisodesNotCounted || slices.ContainsFunc(rep.Rules, func(r verdict.RuleResult) bool { return r.Episodes.Total > 0 }) ||
		slices.ContainsFunc(rep.Firings, func(j verdict.Judged) bool { return j.Episode != 0 }) {
		t.Errorf("before the walk, the report gives episodes")
	}
	// Walked from the newest week back, a week a write: each week in
	// Seoul with a firing that started in it.
	has := map[int64]bool{}
	for _, f := range fs {
		has[Week(l.Day(f.Start))], has[Week(l.Day(f.Start.Add(7*24*time.Hour)))] = true, true
	}
	var weeks []int64
	walks := 0
	episodesWalked = func(int) {
		walks++
		w, _, _ := l.episodesFrom(ctx, l.r) // as the last write left it
		weeks = append(weeks, w)
	}
	n, err := l.CountEpisodes(ctx, o, nil)
	episodesWalked = nil
	if err != nil || n != len(has) || walks != len(has) || !slices.IsSortedFunc(weeks, func(a, b int64) int { return int(b - a) }) {
		t.Errorf("counted %d weeks of %d in %d walks, from %v: %v", n, len(has), walks, weeks, err)
	}
	if _, ok, _ := l.EpisodesFrom(ctx); ok {
		t.Error("walked through, the weeks are still said not counted")
	}
	if n, err := l.CountEpisodes(ctx, o, nil); err != nil || n != 0 {
		t.Errorf("again: %d weeks: %v", n, err)
	}
	// As a ledger kept in Seoul from the first.
	fresh := open(t, seoul)
	fill(fresh)
	for _, w := range []time.Time{t0, t0.Add(7 * 24 * time.Hour)} {
		a, err := l.Report(ctx, w, w.Add(7*24*time.Hour), o, true)
		if err != nil {
			t.Fatal(err)
		}
		b, err := fresh.Report(ctx, w, w.Add(7*24*time.Hour), o, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a.Rules, b.Rules) || !reflect.DeepEqual(a.Firings, b.Firings) {
			t.Errorf("the week of %s, walked again, isn't as kept in Seoul from the first", w.Format(time.DateOnly))
		}
	}
	// No count of firings moved: the weeks' are as before but their
	// episodes, which Seoul's days part otherwise.
	after, err := l.WeeksBy(ctx, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	strip := func(ts []Tally) map[[3]int64]Tally {
		m := map[[3]int64]Tally{}
		for _, x := range ts {
			k := [3]int64{0, x.Rule, x.Signals} // the weeks are Seoul's now: by rule
			y := m[k].Add(x)
			y.Episodes, y.EpisodesWhite, y.At = 0, 0, 0
			y.Rule, y.Signals = x.Rule, x.Signals
			m[k] = y
		}
		return m
	}
	if !reflect.DeepEqual(strip(before), strip(after)) {
		t.Errorf("the firings' counts moved:\n%+v\n%+v", strip(before), strip(after))
	}
}

// After an upgrade to schema 025, the episodes are counted once, a week a
// write, whichever replica walks, or two at once; until then no stone, no
// count of firings and no report but its episodes differs.
func TestEpisodesUpgradedOnce(t *testing.T) {
	ctx := context.Background()
	path := place(t)
	l, err := openAt(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	fs, incs := episodeWorld(5)
	incs = append([]incident.Incident{keepAll}, incs...)
	if _, err := l.SyncFile(ctx, incs, opts); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(8*24*time.Hour), fs)
	o := opts
	o.From, o.To = t0, t0.Add(7*24*time.Hour)
	want, err := l.Report(ctx, o.From, o.To, o, true)
	if err != nil {
		t.Fatal(err)
	}
	daysBefore, _ := l.DaysBy(ctx, 0, 1<<30)
	// Taken back to 024, as the release before left it.
	undo, version := slices.Concat(ledgertest.Undo029, ledgertest.Undo028, ledgertest.Undo027, ledgertest.Undo026, ledgertest.Undo025), "PRAGMA user_version = 24"
	if l.d == postgres {
		version = "UPDATE schema_version SET version = 24"
	}
	for _, q := range append(slices.Clone(undo), version) {
		if _, err := l.w.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	l.Close()
	// Opened twice, as two replicas of a Postgres ledger, or a restart of
	// a SQLite one.
	a, err := openAt(ctx, path, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 025: %v", err)
	}
	defer a.Close()
	has := map[int64]bool{}
	newest := int64(0)
	for _, f := range fs {
		has[Week(a.Day(f.Start))] = true
		newest = max(newest, a.Day(f.Start))
	}
	if w, ok, err := a.EpisodesFrom(ctx); err != nil || !ok || w != Week(newest)+7 {
		t.Errorf("upgraded, the episodes are counted from %v %v, want the week after the newest firing's, %d: %v", w, ok, Week(newest)+7, err)
	}
	got, err := a.Report(ctx, o.From, o.To, o, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.EpisodesNotCounted || !reflect.DeepEqual(withoutEpisodes(got.Rules), withoutEpisodes(want.Rules)) {
		t.Errorf("upgraded, not walked: not counted %v, the rules as before but their episodes: %v", got.EpisodesNotCounted,
			reflect.DeepEqual(withoutEpisodes(got.Rules), withoutEpisodes(want.Rules)))
	}
	b := a
	if a.d == postgres {
		if b, err = openAt(ctx, path, time.UTC); err != nil {
			t.Fatal(err)
		}
		defer b.Close()
	}
	var wg sync.WaitGroup
	counted := make([]int, 2)
	for i, x := range []*Ledger{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := x.CountEpisodes(ctx, o, nil)
			if err != nil {
				t.Error(err)
			}
			counted[i] = n
		}()
	}
	wg.Wait()
	// Each week with a firing that started in it, once.
	if counted[0]+counted[1] != len(has) {
		t.Errorf("two walking at once counted %v weeks, want each of the %d once", counted, len(has))
	}
	got, err = a.Report(ctx, o.From, o.To, o, true)
	if err != nil || got.EpisodesNotCounted || !reflect.DeepEqual(got.Rules, want.Rules) || !reflect.DeepEqual(got.Firings, want.Firings) {
		t.Errorf("walked, the report isn't as before the upgrade: %v", err)
	}
	if days, _ := a.DaysBy(ctx, 0, 1<<30); !reflect.DeepEqual(days, daysBefore) {
		t.Errorf("walked, the days' tallies aren't as before the upgrade")
	}
}

// The walk of the weeks not counted yet reads the latest firing before a
// week along an index, in either database's plan: no scan of every firing
// while the ledger's one writer is held.
func TestEpisodesWalkIndexed(t *testing.T) {
	l := open(t, time.UTC)
	p := plan(t, l, newestBefore, 0)
	if p == "" || scansAll(l, p, "firings") || !strings.Contains(p, "firings_start") {
		t.Errorf("plan: %q", p)
	}
}
