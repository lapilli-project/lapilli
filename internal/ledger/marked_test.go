package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// markedFirings are a day's firings by rule, service and start, each with
// the kinds it is marked with.
func markedFirings(t *testing.T, l *Ledger, from time.Time) map[string]string {
	t.Helper()
	got, err := l.Firings(context.Background(), FiringQuery{From: from, To: from.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range got {
		out[f.Rule+" "+f.Labels["service"]+" "+f.Start.Format("15:04")] = strings.Join(f.Kinds, ",")
	}
	return out
}

// A firing judged that started during an occurrence of a marked window and
// agrees with it, as a firing agrees with a deploy, is marked with its
// kind, beside its stones, which don't change; one a quiet window excused
// isn't; a team's window marks its team's rules alone. Counted by day and
// week and kind, and in a report, by rule and kind, with the firings.
func TestMarkedMarks(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(10 * time.Hour)
	in := incident.Incident{ID: "INC-1", Start: at.Add(time.Minute), End: at.Add(10 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}}
	if _, err := l.SyncFile(ctx, []incident.Incident{keepAll, in}, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search"}, "ana"); err != nil {
		t.Fatal(err)
	}
	fs := []firing.Firing{
		// During the sale, catching the incident.
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(2 * time.Minute), End: at.Add(4 * time.Minute)},
		// During the sale and the launch of search, catching nothing.
		{Rule: "Latency", Labels: map[string]string{"service": "search"}, Start: at.Add(12 * time.Minute), End: at.Add(20 * time.Minute)},
		// During the sale; not the launch's service.
		{Rule: "Latency", Labels: map[string]string{"service": "cart"}, Start: at.Add(13 * time.Minute), End: at.Add(20 * time.Minute)},
		// After every window.
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(5 * time.Hour), End: at.Add(5*time.Hour + time.Minute)},
		// During the sale, but a quiet window excused it.
		{Rule: "Disk", Labels: map[string]string{"service": "cart"}, Start: at.Add(30 * time.Minute), End: at.Add(31 * time.Minute)},
		// The search team's rule, during its team's window.
		{Rule: "SearchSlow", Labels: map[string]string{"service": "search"}, Start: at.Add(40 * time.Minute), End: at.Add(41 * time.Minute)},
	}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	stones := func() map[string][3]bool {
		got, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][3]bool{}
		for _, f := range got {
			out[f.Rule+" "+f.Start.Format("15:04")] = [3]bool{f.White, f.Refired, f.Excused}
		}
		return out
	}
	if _, err := l.PutWindow(ctx, Window{Title: "disk work", Start: at.Add(29 * time.Minute), End: at.Add(32 * time.Minute), Rules: "^Disk$"}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	before := stones()
	teams, err := l.Teams(ctx)
	if err != nil || len(teams) != 1 {
		t.Fatal(teams, err)
	}
	for _, m := range []Marked{
		{Kind: "sale", Title: "Autumn sale", Start: at, End: at.Add(time.Hour)},
		{Kind: "launch", Title: "Search, new ranking", Start: at.Add(10 * time.Minute), End: at.Add(20 * time.Minute), Labels: map[string]string{"service": "search"}},
		{Kind: "campaign", Title: "Search's campaign", Start: at, End: at.Add(time.Hour), TeamID: teams[0].ID},
	} {
		if _, err := l.PutMarked(ctx, m, "ana", opts); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{"Errors checkout 10:02": "sale", "Latency search 10:12": "launch,sale", "Latency cart 10:13": "sale",
		"Errors checkout 15:00": "", "Disk cart 10:30": "", "SearchSlow search 10:40": "campaign,sale"}
	if got := markedFirings(t, l, t0); !equalMarkedFirings(got, want) {
		t.Errorf("marks: %v, want %v", got, want)
	}
	if after := stones(); !equalStones(before, after) {
		t.Errorf("stones changed with the marks: %v, before %v", after, before)
	}
	// By day and week and kind.
	days, err := l.MarkedDays(ctx, l.Day(t0), l.Day(t0))
	if err != nil {
		t.Fatal(err)
	}
	sum := map[string][3]int{}
	for _, d := range days {
		c := sum[d.Kind]
		sum[d.Kind] = [3]int{c[0] + d.Firings, c[1] + d.White, c[2] + d.Refired}
	}
	if sum["sale"] != [3]int{4, 1, 0} || sum["launch"] != [3]int{1, 0, 0} || sum["campaign"] != [3]int{1, 0, 0} {
		t.Errorf("marked days: %v", sum)
	}
	weeks, err := l.MarkedWeeks(ctx, Week(l.Day(t0)), Week(l.Day(t0)))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, w := range weeks {
		if w.Kind == "sale" {
			n += w.Firings
		}
	}
	if n != 4 {
		t.Errorf("marked weeks: %d of the sale, want 4", n)
	}
	// In a report, by rule and kind, over whole days and over an hour.
	for _, span := range [][2]time.Time{{t0, t0.Add(24 * time.Hour)}, {at, at.Add(time.Hour)}} {
		rep, err := l.Report(ctx, span[0], span[1], opts, true)
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]verdict.MarkedCount{}
		for _, r := range rep.Rules {
			for k, c := range r.Marked {
				by[r.Rule+" "+k] = c
			}
		}
		if by["Errors sale"] != (verdict.MarkedCount{Firings: 1, Caught: 1}) || by["Latency sale"] != (verdict.MarkedCount{Firings: 2}) ||
			by["Latency launch"] != (verdict.MarkedCount{Firings: 1}) || len(by) != 5 {
			t.Errorf("report over %s: %+v", span, by)
		}
		marked := 0
		for _, j := range rep.Firings {
			if slices.Contains(j.Marked, "sale") {
				marked++
			}
		}
		if marked != 4 {
			t.Errorf("report over %s: %d firings marked with the sale, want 4", span, marked)
		}
	}
	// Picked by kind.
	if got, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour), Kind: "launch"}); err != nil || len(got) != 1 || got[0].Rule != "Latency" {
		t.Errorf("firings of the launch: %+v %v", got, err)
	}
	if got, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour), Kind: "nothing"}); err != nil || len(got) != 0 {
		t.Errorf("firings of a kind there isn't: %+v %v", got, err)
	}
	// The team removed: its window marks nothing.
	if err := l.DeleteTeam(ctx, teams[0].ID, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	want["SearchSlow search 10:40"] = "sale"
	if got := markedFirings(t, l, t0); !equalMarkedFirings(got, want) {
		t.Errorf("marks with the team removed: %v, want %v", got, want)
	}
}

func equalMarkedFirings(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func equalStones(a, b map[string][3]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Without a marked window nothing changes: a world judged in a ledger that
// never kept one, and in one where a window marked its firings and was
// deleted, gives the same stones, tallies and report, byte for byte, and
// no tally of a kind.
func TestMarkedNothingChanges(t *testing.T) {
	ctx := context.Background()
	fs, incs := world(7, 300)
	incs = append([]incident.Incident{keepAll}, incs...)
	state := func(l *Ledger) string {
		t.Helper()
		rep, err := l.Report(ctx, t0, t0.Add(7*24*time.Hour), opts, true)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := verdict.JSON(&b, rep); err != nil {
			t.Fatal(err)
		}
		days, err := l.Days(ctx, l.Day(t0), l.Day(t0)+7)
		if err != nil {
			t.Fatal(err)
		}
		weeks, err := l.Weeks(ctx, Week(l.Day(t0)), Week(l.Day(t0)))
		if err != nil {
			t.Fatal(err)
		}
		d, _ := json.Marshal(days)
		w, _ := json.Marshal(weeks)
		return b.String() + string(d) + string(w)
	}
	plain := open(t, time.UTC)
	if _, err := plain.SyncFile(ctx, incs, opts); err != nil {
		t.Fatal(err)
	}
	record(t, plain, t0, t0.Add(7*24*time.Hour), fs)
	want := state(plain)
	if strings.Contains(want, `"marked"`) {
		t.Errorf("a report with no marked window says marked: %s", want)
	}
	var tallied int
	plain.r.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM marked_days) + (SELECT count(*) FROM marked_weeks) + (SELECT count(*) FROM firings WHERE marked != 0)").Scan(&tallied)
	if tallied != 0 {
		t.Errorf("%d rows of marks with no marked window", tallied)
	}

	l := open(t, time.UTC)
	if _, err := l.SyncFile(ctx, incs, opts); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(7*24*time.Hour), fs)
	id, err := l.PutMarked(ctx, Marked{Kind: "sale", Title: "Sale", Start: t0, End: t0.Add(7 * 24 * time.Hour)}, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := state(l); got == want {
		t.Fatal("a window over the week marked nothing")
	}
	if err := l.DeleteMarked(ctx, id, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got := state(l); got != want {
		t.Errorf("after the window was deleted:\n%s\nwant\n%s", got, want)
	}
	if ks, err := l.MarkedKinds(ctx); err != nil || len(ks) != 0 {
		t.Errorf("the kind nothing names was kept: %v %v", ks, err)
	}
	l.r.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM marked_days) + (SELECT count(*) FROM marked_weeks) + (SELECT count(*) FROM firings WHERE marked != 0)").Scan(&tallied)
	if tallied != 0 {
		t.Errorf("%d rows of marks after the window was deleted", tallied)
	}
}

// Each occurrence of a repeating window is one, as a quiet window's repeat
// is: a firing is marked in each, by the same code that says when a quiet
// window covers a time, and not between them.
func TestMarkedRepeats(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	// A Friday flash sale, 20:00 to 22:00 each week, from 2026-11-06.
	fri := time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)
	m := Marked{Kind: "sale", Title: "Friday flash sale", Start: fri, End: fri.Add(2 * time.Hour), Every: "week", Until: time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)}
	var fs []firing.Firing
	for d := 0; d < 21; d++ {
		for _, h := range []int{19, 21} {
			s := fri.AddDate(0, 0, d).Add(time.Duration(h-20) * time.Hour)
			fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: s, End: s.Add(time.Minute)})
		}
	}
	// Kept ahead, as a sale is planned; the runs mark what comes.
	if _, err := l.PutMarked(ctx, m, "ana", opts); err != nil {
		t.Fatal(err)
	}
	record(t, l, fri.Add(-24*time.Hour), fri.AddDate(0, 0, 22), fs)
	got, err := l.Firings(ctx, FiringQuery{From: fri.Add(-24 * time.Hour), To: fri.AddDate(0, 0, 22)})
	if err != nil {
		t.Fatal(err)
	}
	w := Window{Start: m.Start, End: m.End, Every: "week", Until: m.Until, loc: time.UTC}
	n := 0
	for _, f := range got {
		marked := slices.Contains(f.Kinds, "sale")
		if marked != w.Covers(f.Start) {
			t.Errorf("%s: marked %v, covered %v", f.Start, marked, w.Covers(f.Start))
		}
		if marked {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d firings marked, want 3: one in each Friday's", n)
	}
	kept, err := l.MarkedByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	occ, over := kept.Occurrences(fri, fri.AddDate(0, 0, 21).Add(-time.Second), 64)
	if over || len(occ) != 3 || !occ[1].Start.Equal(fri.AddDate(0, 0, 7)) || OccurrenceKey(kept.ID, occ[0].Start) != "1@2026-11-06T20:00Z" {
		t.Errorf("occurrences: %v %v", occ, over)
	}
}

// What a window may be: its kind a name not taken by deploys or goals, of
// [a-z][a-z0-9-], 15 kinds at most; it lasts 31 days at most and starts no
// later than a year ahead; 64 occurrences of a kind meet a week at most,
// and 256 of all; what breaks one is refused where it is kept, the file
// whole.
func TestMarkedBounds(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := time.Now().Truncate(time.Hour)
	put := func(m Marked) error {
		_, err := l.PutMarked(ctx, m, "ana", opts)
		return err
	}
	for _, c := range []struct {
		name string
		m    Marked
	}{
		{"deploy is the deploys'", Marked{Kind: "deploy", Title: "x", Start: at, End: at.Add(time.Hour)}},
		{"week is a goal's", Marked{Kind: "week", Title: "x", Start: at, End: at.Add(time.Hour)}},
		{"an upper-case kind", Marked{Kind: "Sale", Title: "x", Start: at, End: at.Add(time.Hour)}},
		{"a kind past 32 bytes", Marked{Kind: strings.Repeat("a", 33), Title: "x", Start: at, End: at.Add(time.Hour)}},
		{"no title", Marked{Kind: "sale", Start: at, End: at.Add(time.Hour)}},
		{"a title of two lines", Marked{Kind: "sale", Title: "a\nb", Start: at, End: at.Add(time.Hour)}},
		{"a title turned around", Marked{Kind: "sale", Title: "sale‮", Start: at, End: at.Add(time.Hour)}},
		{"longer than 31 days", Marked{Kind: "sale", Title: "x", Start: at, End: at.Add(32 * 24 * time.Hour)}},
		{"more than a year ahead", Marked{Kind: "sale", Title: "x", Start: at.Add(400 * 24 * time.Hour), End: at.Add(401 * 24 * time.Hour)}},
		{"seventeen labels", Marked{Kind: "sale", Title: "x", Start: at, End: at.Add(time.Hour), Labels: func() map[string]string {
			m := map[string]string{}
			for i := range 17 {
				m[fmt.Sprint("l", i)] = "v"
			}
			return m
		}()}},
		{"every day for a day", Marked{Kind: "sale", Title: "x", Start: at, End: at.Add(24 * time.Hour), Every: "day"}},
		{"every two hours, 84 a week", Marked{Kind: "sale", Title: "x", Start: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC), Every: "cron", On: "0 */2 * * *"}},
	} {
		var inv ErrInvalid
		if err := put(c.m); !errors.As(err, &inv) {
			t.Errorf("%s: %v, want it refused", c.name, err)
		}
	}
	// 64 of a kind in a week, and no more.
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var ms []Marked
	for i := range MarkedKindWeek {
		s := start.Add(time.Duration(i) * 2 * time.Hour)
		ms = append(ms, Marked{Kind: "sale", Title: fmt.Sprint("sale ", i), Start: s, End: s.Add(time.Hour)})
	}
	if _, err := l.SyncMarked(ctx, ms, opts, MarkedRead{}); err != nil {
		t.Fatalf("64 of a kind in a week: %v", err)
	}
	extra := Marked{Kind: "sale", Title: "one more", Start: start.Add(150 * time.Hour), End: start.Add(151 * time.Hour)}
	var inv ErrInvalid
	if err := put(extra); !errors.As(err, &inv) {
		t.Errorf("a 65th of a kind in a week: %v", err)
	}
	// The file whole: one more refuses it, and the ledger keeps what it had.
	if _, err := l.SyncMarked(ctx, append(slices.Clone(ms), extra), opts, MarkedRead{}); !errors.As(err, &inv) {
		t.Errorf("a file with a 65th of a kind in a week: %v", err)
	}
	if all, err := l.AllMarked(ctx); err != nil || len(all) != MarkedKindWeek {
		t.Errorf("after a file refused: %d windows, %v", len(all), err)
	}
	// Another week takes it.
	extra.Start, extra.End = extra.Start.Add(7*24*time.Hour), extra.End.Add(7*24*time.Hour)
	if err := put(extra); err != nil {
		t.Errorf("one in another week: %v", err)
	}
	// 256 of all in a week.
	l2 := open(t, time.UTC)
	ms = nil
	for i := range MarkedWeek {
		s := start.Add(time.Duration(i) * 30 * time.Minute)
		ms = append(ms, Marked{Kind: fmt.Sprint("k", i%4), Title: fmt.Sprint("w ", i), Start: s, End: s.Add(20 * time.Minute)})
	}
	if _, err := l2.SyncMarked(ctx, ms, opts, MarkedRead{}); err != nil {
		t.Fatalf("256 in a week: %v", err)
	}
	if _, err := l2.PutMarked(ctx, Marked{Kind: "k9", Title: "x", Start: start.Add(160 * time.Hour), End: start.Add(161 * time.Hour)}, "ana", opts); !errors.As(err, &inv) {
		t.Errorf("a 257th in a week: %v", err)
	}
	// Fifteen kinds, and no sixteenth.
	l3 := open(t, time.UTC)
	for i := range MaxKinds {
		s := start.Add(time.Duration(i) * 24 * time.Hour * 7)
		if _, err := l3.PutMarked(ctx, Marked{Kind: fmt.Sprint("kind-", i), Title: "x", Start: s, End: s.Add(time.Hour)}, "ana", opts); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := l3.PutMarked(ctx, Marked{Kind: "kind-15", Title: "x", Start: start, End: start.Add(time.Hour)}, "ana", opts); !errors.As(err, &inv) {
		t.Errorf("a sixteenth kind: %v", err)
	}
}

// The marked file owns its windows: the same file again changes nothing;
// one it no longer holds is dropped, and its firings marked no more; the
// UI changes none of them, nor the file the UI's.
func TestSyncMarked(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(10 * time.Hour)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(5 * time.Minute), End: at.Add(6 * time.Minute)},
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(3 * time.Hour), End: at.Add(3*time.Hour + time.Minute)},
	})
	file := []Marked{
		{Kind: "sale", Title: "Morning sale", Start: at, End: at.Add(time.Hour)},
		{Kind: "launch", Title: "Afternoon launch", Start: at.Add(150 * time.Minute), End: at.Add(4 * time.Hour)},
	}
	if changed, err := l.SyncMarked(ctx, file, opts, MarkedRead{Sum: "a"}); err != nil || !changed {
		t.Fatalf("SyncMarked: %v %v", changed, err)
	}
	if got := markedFirings(t, l, t0); got["Errors checkout 10:05"] != "sale" || got["Errors checkout 13:00"] != "launch" {
		t.Errorf("marks: %v", got)
	}
	if same, err := l.MarkedUnchanged(ctx, MarkedRead{Sum: "a"}); err != nil || !same {
		t.Errorf("the same read: unchanged %v, %v", same, err)
	}
	if changed, err := l.SyncMarked(ctx, file, opts, MarkedRead{Sum: "b"}); err != nil || changed {
		t.Errorf("the same windows again: changed %v, %v", changed, err)
	}
	ui, err := l.PutMarked(ctx, Marked{Kind: "campaign", Title: "Newsletter", Start: at, End: at.Add(10 * time.Minute)}, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.SyncMarked(ctx, file[1:], opts, MarkedRead{Sum: "c"}); err != nil {
		t.Fatal(err)
	}
	if got := markedFirings(t, l, t0); got["Errors checkout 10:05"] != "campaign" || got["Errors checkout 13:00"] != "launch" {
		t.Errorf("marks with the sale taken out of the file: %v", got)
	}
	all, err := l.AllMarked(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("windows: %+v %v", all, err)
	}
	for _, m := range all {
		if m.Origin == OriginFile {
			if _, err := l.PutMarked(ctx, Marked{ID: m.ID, Kind: "sale", Title: "x", Start: at, End: at.Add(time.Hour)}, "ana", opts); !errors.Is(err, ErrNotTheUIs) {
				t.Errorf("the UI changing the file's: %v", err)
			}
			if err := l.DeleteMarked(ctx, m.ID, "ana", opts); !errors.Is(err, ErrNotTheUIs) {
				t.Errorf("the UI deleting the file's: %v", err)
			}
		}
	}
	if m, err := l.MarkedByID(ctx, ui); err != nil || m.Origin != OriginUI {
		t.Errorf("the UI's after the file was read: %+v %v", m, err)
	}
}

// An expression's windows are kept from a read's start on in place of
// those it found before; one on at the start goes on from it; read again
// with a new key, they are replaced; those past the bounds are dropped,
// and said.
func TestKeepMarks(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(10 * time.Hour)
	var fs []firing.Firing
	for m := 0; m < 240; m += 10 {
		s := at.Add(time.Duration(m) * time.Minute)
		fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout", "region": "kr"}, Start: s, End: s.Add(time.Minute)})
	}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	step := time.Minute
	keep := func(since, to time.Time, again bool, key string, ws ...Marked) int {
		t.Helper()
		n, err := l.KeepMarks(ctx, []MarkRead{{Kind: "sale", Key: key, Since: since, To: to, Step: step, Again: again, Windows: ws}}, opts)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The first read: on from 10:00 to 11:00.
	keep(at.Add(-time.Hour), at.Add(time.Hour), false, "k1", Marked{Start: at, End: at.Add(time.Hour)})
	// The next, from 11:00: still on until 11:30, going on from the one before.
	keep(at.Add(time.Hour), at.Add(2*time.Hour), false, "k1", Marked{Start: at.Add(time.Hour), End: at.Add(90 * time.Minute)})
	all, err := l.AllMarked(ctx)
	if err != nil || len(all) != 1 || !all[0].Start.Equal(at) || !all[0].End.Equal(at.Add(90*time.Minute)) || all[0].Origin != OriginExpr || all[0].Title != "sale" {
		t.Fatalf("after two reads: %+v %v", all, err)
	}
	if got := markedFirings(t, l, t0); got["Errors checkout 11:20"] != "sale" || got["Errors checkout 11:40"] != "" {
		t.Errorf("marks after two reads: %v", got)
	}
	// Read again with another key, over the weeks before: replaced whole.
	keep(t0.Add(-28*24*time.Hour), at.Add(4*time.Hour), true, "k2", Marked{Start: at.Add(3 * time.Hour), End: at.Add(4 * time.Hour), Labels: map[string]string{"region": "kr"}})
	if got := markedFirings(t, l, t0); got["Errors checkout 11:20"] != "" || got["Errors checkout 13:10"] != "sale" {
		t.Errorf("marks after a read again: %v", got)
	}
	if k, err := l.MarkKeyOf(ctx, "sale"); err != nil || k != "k2" {
		t.Errorf("key: %q %v", k, err)
	}
	// Past the bounds: 65 windows in a week, the last dropped.
	var ws []Marked
	for i := range 65 {
		s := t0.Add(time.Duration(i) * 2 * time.Hour)
		ws = append(ws, Marked{Start: s, End: s.Add(time.Hour)})
	}
	if n := keep(t0.Add(-time.Hour), t0.Add(7*24*time.Hour), true, "k3", ws...); n != 1 {
		t.Errorf("%d dropped, want 1", n)
	}
	// No longer given: its key forgotten, its windows kept.
	if err := l.ForgetMarks(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if k, err := l.MarkKeyOf(ctx, "sale"); err != nil || k != "" {
		t.Errorf("key after it was forgotten: %q %v", k, err)
	}
	if all, err := l.AllMarked(ctx); err != nil || len(all) != 64 {
		t.Errorf("windows after it was forgotten: %d %v", len(all), err)
	}
}

// A kind no window names any more is let go once its windows' spans are
// judged again, and its id given to another: a firing marked before is
// marked with the new kind only where the new kind's window is.
func TestMarkedKindsLetGo(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(10 * time.Hour)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(5 * time.Minute), End: at.Add(6 * time.Minute)},
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(3 * time.Hour), End: at.Add(3*time.Hour + time.Minute)},
	})
	id, err := l.PutMarked(ctx, Marked{Kind: "sale", Title: "Sale", Start: at, End: at.Add(time.Hour)}, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteMarked(ctx, id, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutMarked(ctx, Marked{Kind: "launch", Title: "Launch", Start: at.Add(2 * time.Hour), End: at.Add(4 * time.Hour)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	var sale int
	l.r.QueryRowContext(ctx, "SELECT count(*) FROM marked_kinds WHERE name = 'sale'").Scan(&sale)
	if sale != 0 {
		t.Error("the sale, which nothing names, was kept")
	}
	if got := markedFirings(t, l, t0); got["Errors checkout 10:05"] != "" || got["Errors checkout 13:00"] != "launch" {
		t.Errorf("marks: %v", got)
	}
}

// A team's editors keep 20 of their team's marked windows at most, of the
// kinds there are, and none of another team's.
func TestMarkedTeamEditors(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	a, err := l.PutTeam(ctx, Team{Name: "a", Rules: "^A"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.PutTeam(ctx, Team{Name: "b", Rules: "^B"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ed := WithTeams(ctx, []int64{a})
	start := time.Now().Truncate(time.Hour)
	// A kind there isn't: an editor's or an admin's to make.
	var inv ErrInvalid
	if _, err := l.PutMarked(ed, Marked{Kind: "launch", Title: "x", Start: start, End: start.Add(time.Hour), TeamID: a}, "ed", opts); !errors.As(err, &inv) {
		t.Errorf("a team's editor making a kind: %v", err)
	}
	if _, err := l.PutMarked(ctx, Marked{Kind: "launch", Title: "everyone's", Start: start.Add(-48 * time.Hour), End: start.Add(-47 * time.Hour)}, "admin", opts); err != nil {
		t.Fatal(err)
	}
	for i := range MaxTeamMarked {
		s := start.Add(time.Duration(i) * 24 * time.Hour)
		if _, err := l.PutMarked(ed, Marked{Kind: "launch", Title: "x", Start: s, End: s.Add(time.Hour), TeamID: a}, "ed", opts); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := l.PutMarked(ed, Marked{Kind: "launch", Title: "x", Start: start, End: start.Add(time.Hour), TeamID: a}, "ed", opts); !errors.As(err, &inv) {
		t.Errorf("a 21st of the team's: %v", err)
	}
	if _, err := l.PutMarked(ed, Marked{Kind: "launch", Title: "x", Start: start, End: start.Add(time.Hour), TeamID: b}, "ed", opts); err == nil {
		t.Error("another team's window made by a team's editor")
	}
	if _, err := l.PutMarked(ed, Marked{Kind: "launch", Title: "x", Start: start, End: start.Add(time.Hour)}, "ed", opts); err == nil {
		t.Error("everyone's window made by a team's editor")
	}
}

// What --config declares of marked windows is kept declared, the UI
// changing none of it; the same file again writes nothing; one the file
// no longer names is the UI's again, or deleted with --config-prune; one
// past the bounds is skipped, and said, the rest kept.
func TestReconcileMarked(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	d := declaredOf()
	d.Marked = []DeclaredMarked{
		{Marked: Marked{Kind: "sale", Title: "Autumn sale", Start: start, End: start.Add(72 * time.Hour)}, Line: 30},
		{Marked: Marked{Kind: "launch", Title: "Payments, new checkout", Start: start, End: start.Add(time.Hour), Labels: map[string]string{"service": "checkout"}},
			Team: "payments", Line: 34},
	}
	r, skipped := reconcile(t, l, d, Leave{})
	if r.Made != 7 || len(skipped) != 0 {
		t.Fatalf("reconciled: %+v %v", r, skipped)
	}
	ms, err := l.AllMarked(ctx)
	if err != nil || len(ms) != 2 || !ms[0].Declared || ms[0].Origin != OriginConfig || ms[1].TeamID == 0 {
		t.Fatalf("declared: %+v %v", ms, err)
	}
	if _, err := l.PutMarked(ctx, Marked{ID: ms[0].ID, Kind: "sale", Title: "x", Start: start, End: start.Add(time.Hour)}, "ana", opts); !errors.Is(err, ErrDeclared) {
		t.Errorf("the UI changing a declared one: %v", err)
	}
	if err := l.DeleteMarked(ctx, ms[0].ID, "ana", opts); !errors.Is(err, ErrDeclared) {
		t.Errorf("the UI deleting a declared one: %v", err)
	}
	if r, _ := reconcile(t, l, d, Leave{}); r.Any() {
		t.Errorf("the same file again: %+v", r)
	}
	// Exported, as the UI's and the config's, not the file's.
	if _, err := l.SyncMarked(ctx, []Marked{{Kind: "sale", Title: "the file's", Start: start, End: start.Add(time.Hour)}}, opts, MarkedRead{}); err != nil {
		t.Fatal(err)
	}
	dd, err := l.AllDeclarable(ctx)
	if err != nil || len(dd.Marked) != 2 {
		t.Errorf("declarable: %+v %v", dd.Marked, err)
	}
	// The sale taken out: the UI's, which it may change now.
	d.Marked = d.Marked[1:]
	if r, _ := reconcile(t, l, d, Leave{}); r.Undeclared != 1 {
		t.Errorf("one taken out: %+v", r)
	}
	if m, err := l.MarkedByID(ctx, ms[0].ID); err != nil || m.Declared || m.Origin != OriginUI {
		t.Errorf("the one taken out: %+v %v", m, err)
	}
	if _, err := l.PutMarked(ctx, Marked{ID: ms[0].ID, Kind: "sale", Title: "Autumn sale, longer", Start: start, End: start.Add(96 * time.Hour)}, "ana", opts); err != nil {
		t.Errorf("the UI changing one given back: %v", err)
	}
	// 65 of a kind in a week, from a Monday: the last skipped, and said;
	// the others kept.
	d.Marked = nil
	mon := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	for i := range 65 {
		s := mon.Add(time.Duration(i) * 2 * time.Hour)
		d.Marked = append(d.Marked, DeclaredMarked{Marked: Marked{Kind: "flash", Title: fmt.Sprint("flash ", i), Start: s, End: s.Add(time.Hour)}, Line: 40 + i})
	}
	r, skipped = reconcile(t, l, d, Leave{Prune: true})
	if len(skipped) != 1 || skipped[0].Kind != KindMarked || r.Made != 64 {
		t.Errorf("past the bounds: %+v %v", r, skipped)
	}
}
