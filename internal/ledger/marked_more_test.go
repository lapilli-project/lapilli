package ledger

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// Two series of one expression, no --mark-match, both on at a read's
// start: each found window goes on from one found before, one each, so
// neither is lost.
func TestMarkedTwoSeriesGoOn(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	mon, tue, wed := t0, t0.Add(24*time.Hour), t0.Add(48*time.Hour)
	record(t, l, t0, t0.Add(3*24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: mon.Add(10 * time.Hour), End: mon.Add(10*time.Hour + time.Minute)},
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + time.Minute)},
	})
	step := time.Minute
	keep := func(since, to time.Time, ws ...Marked) {
		t.Helper()
		if _, err := l.KeepMarks(ctx, []MarkRead{{Kind: "sale", Key: "k", Since: since, To: to, Step: step, Windows: ws}}, opts); err != nil {
			t.Fatal(err)
		}
	}
	marked := func() int {
		t.Helper()
		fs, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(3 * 24 * time.Hour), Kind: "sale"})
		if err != nil {
			t.Fatal(err)
		}
		return len(fs)
	}
	keep(mon, wed, Marked{Start: mon, End: wed.Add(step)}, Marked{Start: tue, End: wed.Add(step)})
	since, to := wed.Add(-time.Hour), wed.Add(time.Hour)
	keep(since, to, Marked{Start: since, End: to.Add(step)}, Marked{Start: since, End: to.Add(step)})
	if n := marked(); n != 2 {
		t.Errorf("%d firings marked, want 2: a window found before was lost", n)
	}
	all, _ := l.AllMarked(ctx)
	if len(all) != 2 || !all[0].Start.Equal(mon) || !all[1].Start.Equal(tue) {
		t.Errorf("windows: %+v", all)
	}
}

// Two replicas export the same gauge from 00:00, read hour after hour:
// the windows keep their start, and every firing since is marked.
func TestMarkedTwoReplicasHourly(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	var fs []firing.Firing
	for h := 0; h < 10; h++ {
		s := t0.Add(time.Duration(h)*time.Hour + 30*time.Minute)
		fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: s, End: s.Add(time.Minute)})
	}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	for h := 1; h <= 10; h++ {
		to := t0.Add(time.Duration(h) * time.Hour)
		since := to.Add(-2 * time.Hour)
		if since.Before(t0) {
			since = t0
		}
		if _, err := l.KeepMarks(ctx, []MarkRead{{Kind: "sale", Key: "k", Since: since, To: to, Step: time.Minute,
			Windows: []Marked{{Start: since, End: to.Add(time.Minute)}, {Start: since, End: to.Add(time.Minute)}}}}, opts); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour), Kind: "sale"})
	if len(got) != 10 {
		t.Errorf("%d of 10 firings marked", len(got))
	}
}

// A window going on, hour after hour, is judged again from where it
// ended to where it ends now, not over all it covers.
func TestMarkedGoingOnCosts(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	record(t, l, t0, t0.Add(10*24*time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "x"}, Start: t0.Add(time.Minute), End: t0.Add(2 * time.Minute)}})
	var last [][2]int64
	keepMarksSpans = func(s [][2]int64) { last = s }
	defer func() { keepMarksSpans = nil }()
	for h := 1; h <= 7*24; h++ {
		to := t0.Add(time.Duration(h) * time.Hour)
		since := to.Add(-2 * time.Hour)
		w := Marked{Start: since, End: to.Add(time.Minute)}
		if since.Before(t0) {
			w.Start = t0
		}
		if _, err := l.KeepMarks(ctx, []MarkRead{{Kind: "sale", Key: "k", Since: since, To: to, Step: time.Minute, Windows: []Marked{w}}}, opts); err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	for _, s := range last {
		n += s[1] - s[0]
	}
	all, _ := l.AllMarked(ctx)
	if len(all) != 1 || !all[0].Start.Equal(t0) || n > 3600 {
		t.Errorf("%d windows, from %v; the last read judged %ds again, want an hour at most", len(all), all[0].Start, n)
	}
}

// An expression whose key changed is read again over the weeks before
// first: a read of the hour with the new key is skipped, its windows and
// key kept, until a read again keeps the key.
func TestMarkedNewKeyWaits(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(10 * time.Hour)
	var fs []firing.Firing
	var ws []Marked
	for d := 0; d < 5; d++ {
		s := at.Add(time.Duration(d) * 24 * time.Hour)
		fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: s.Add(5 * time.Minute), End: s.Add(6 * time.Minute)})
		ws = append(ws, Marked{Start: s, End: s.Add(time.Hour)})
	}
	record(t, l, t0, t0.Add(6*24*time.Hour), fs)
	if _, err := l.KeepMarks(ctx, []MarkRead{{Kind: "sale", Key: "k1", Since: t0, To: t0.Add(6 * 24 * time.Hour), Step: time.Minute, Windows: ws}}, opts); err != nil {
		t.Fatal(err)
	}
	hour := t0.Add(6 * 24 * time.Hour)
	said, err := l.KeepMarksSaid(ctx, []MarkRead{{Kind: "sale", Key: "k2", Since: hour.Add(-time.Hour), To: hour, Step: time.Minute}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := l.AllMarked(ctx)
	k, _ := l.MarkKeyOf(ctx, "sale")
	if len(all) != 5 || k != "k1" || len(said.Kinds) != 1 {
		t.Errorf("a read of the hour with a new key: %d windows, key %s, skipped %v; want 5, k1, the sale", len(all), k, said.Kinds)
	}
	if _, err := l.KeepMarks(ctx, []MarkRead{{Kind: "sale", Key: "k2", Again: true, Since: t0, To: hour, Step: time.Minute, Windows: ws[:2]}}, opts); err != nil {
		t.Fatal(err)
	}
	all, _ = l.AllMarked(ctx)
	if k, _ = l.MarkKeyOf(ctx, "sale"); len(all) != 2 || k != "k2" {
		t.Errorf("read again: %d windows, key %s; want 2, k2", len(all), k)
	}
}

// A team's editors keep their team's share of a week's occurrences, and
// every editor the UI's: the rest is the marked file's, --config's and
// the expressions'. A team's editors use the kinds there are, so the
// fifteen are taken by an editor or an admin, a file or an expression.
func TestMarkedShares(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	a, err := l.PutTeam(ctx, Team{Name: "a", Rules: "^A"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ed := WithTeams(ctx, []int64{a})
	start := time.Now().UTC().Truncate(24 * time.Hour)
	for i := range 3 {
		if _, err := l.PutMarked(ctx, Marked{Kind: fmt.Sprint("k", i), Title: "made", Start: start.Add(-72 * time.Hour), End: start.Add(-71 * time.Hour)}, "admin", opts); err != nil {
			t.Fatal(err)
		}
	}
	var inv ErrInvalid
	// Every six hours, an hour each: 28 a week, a team's 32 at most.
	six := Marked{Kind: "k0", Title: "x", Start: start, End: start.Add(time.Hour), Every: "cron", On: "0 */6 * * *", TeamID: a}
	if _, err := l.PutMarked(ed, six, "ed", opts); err != nil {
		t.Fatalf("a team's 28 a week: %v", err)
	}
	if _, err := l.PutMarked(ed, six, "ed", opts); !errors.As(err, &inv) {
		t.Errorf("a team's 56 a week: %v", err)
	}
	// Every three hours: 56 a week of a kind; the UI's 32 of a kind at
	// most.
	three := Marked{Kind: "k1", Title: "x", Start: start, End: start.Add(time.Hour), Every: "cron", On: "0 */3 * * *"}
	if _, err := l.PutMarked(ctx, three, "ed", opts); !errors.As(err, &inv) {
		t.Errorf("the UI's 56 of a kind a week: %v", err)
	}
	// Every six hours, of three kinds more: 112 a week; the UI's 128 at
	// most.
	for _, k := range []string{"k1", "k2", "k3", "k4"} {
		every := six
		every.Kind, every.TeamID = k, 0
		_, err := l.PutMarked(ctx, every, "ed", opts)
		switch {
		case k != "k4" && err != nil:
			t.Fatalf("the UI's %s every six hours: %v", k, err)
		case k == "k4" && !errors.As(err, &inv):
			t.Errorf("the UI's 140 a week: %v", err)
		}
	}
	// The file takes what is left, a week of 144, beside the UI's 112.
	var file []Marked
	next := start.Add(7 * 24 * time.Hour)
	for i := range 144 {
		s := next.Add(time.Duration(i) * time.Hour)
		file = append(file, Marked{Kind: fmt.Sprint("sale-", i%3), Title: fmt.Sprint("sale ", i), Start: s, End: s.Add(30 * time.Minute)})
	}
	if _, err := l.SyncMarked(ctx, file, opts, MarkedRead{Sum: "x"}); err != nil {
		t.Errorf("the marked file, beside the UI's: %v", err)
	}
	// Kinds: a team's editor makes none; the admin, the file and an
	// expression do.
	if _, err := l.PutMarked(ed, Marked{Kind: "launch", Title: "x", Start: start, End: start.Add(time.Hour), TeamID: a}, "ed", opts); !errors.As(err, &inv) {
		t.Errorf("a team's editor making a kind: %v", err)
	}
	if _, err := l.KeepMarks(ctx, []MarkRead{{Kind: "flag", Key: "k", Since: start.Add(-time.Hour), To: start, Step: time.Minute}}, opts); err != nil {
		t.Errorf("an expression of a new kind: %v", err)
	}
}

// Fifteen kinds, one an expression's alone: a --config window of a
// sixteenth is skipped, and said, the rest of the file kept; an
// expression of a sixteenth is skipped, the others read.
func TestMarkedSixteenthKind(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Now().UTC().Truncate(time.Hour)
	if _, err := l.KeepMarks(ctx, []MarkRead{{Kind: "flag", Key: "k", Since: start.Add(-time.Hour), To: start, Step: time.Minute}}, opts); err != nil {
		t.Fatal(err)
	}
	var file []Marked
	for i := range MaxKinds - 1 {
		file = append(file, Marked{Kind: fmt.Sprint("k", i), Title: "x", Start: start, End: start.Add(time.Hour)})
	}
	if _, err := l.SyncMarked(ctx, file, opts, MarkedRead{}); err != nil {
		t.Fatal(err)
	}
	d := declaredOf()
	d.Marked = []DeclaredMarked{{Marked: Marked{Kind: "sale", Title: "Autumn sale", Start: start, End: start.Add(time.Hour)}, Line: 30}}
	r, skipped, err := l.Reconcile(ctx, d, Leave{}, opts)
	if err != nil || len(skipped) != 1 || skipped[0].Kind != KindMarked || r.Made != 5 {
		t.Errorf("--config with a sixteenth kind: %+v %v %v", r, skipped, err)
	}
	said, err := l.KeepMarksSaid(ctx, []MarkRead{{Kind: "promo", Key: "p", Since: start.Add(-time.Hour), To: start, Step: time.Minute},
		{Kind: "flag", Key: "k", Since: start.Add(-time.Hour), To: start, Step: time.Minute, Windows: []Marked{{Start: start.Add(-time.Hour), End: start}}}}, opts)
	if err != nil || len(said.Kinds) != 1 || said.Kinds[0] != "promo" {
		t.Errorf("expressions with a sixteenth kind: %+v %v", said, err)
	}
	if ms, _ := l.AllMarked(ctx); len(ms) != MaxKinds {
		t.Errorf("%d windows: the flag's read was refused with the promo's", len(ms))
	}
}

// Marks agree with Covers around a change of the clocks, for repeating
// windows of each kind, in a zone with daylight saving; and each
// occurrence is covered from its start to its end.
func TestMarkedAgreesWithCoversDST(t *testing.T) {
	ctx := context.Background()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	l := open(t, loc)
	// 2026-11-01 02:00 EDT -> 01:00 EST.
	from := time.Date(2026, 10, 26, 0, 0, 0, 0, loc) // a Monday
	to := from.Add(14 * 24 * time.Hour)
	windows := []Marked{
		{Kind: "a", Title: "daily", Start: time.Date(2026, 10, 27, 1, 30, 0, 0, loc), End: time.Date(2026, 10, 27, 2, 30, 0, 0, loc), Every: "day"},
		{Kind: "b", Title: "weekly", Start: time.Date(2026, 10, 25, 23, 0, 0, 0, loc), End: time.Date(2026, 10, 26, 4, 0, 0, 0, loc), Every: "week"},
		{Kind: "c", Title: "cron", Start: time.Date(2026, 10, 26, 1, 0, 0, 0, loc), End: time.Date(2026, 10, 26, 1, 50, 0, 0, loc), Every: "cron", On: "0 1 * * *"},
		{Kind: "d", Title: "once", Start: time.Date(2026, 10, 31, 22, 0, 0, 0, loc), End: time.Date(2026, 11, 1, 3, 0, 0, 0, loc)},
		{Kind: "e", Title: "month", Start: time.Date(2026, 10, 1, 1, 0, 0, 0, loc), End: time.Date(2026, 10, 1, 1, 30, 0, 0, loc), Every: "month", On: "1"},
	}
	for _, m := range windows {
		if _, err := l.PutMarked(ctx, m, "ana", opts); err != nil {
			t.Fatal(m.Title, err)
		}
	}
	var fs []firing.Firing
	for at := from; at.Before(to); at = at.Add(10 * time.Minute) {
		fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at, End: at.Add(time.Minute)})
	}
	for d := from; d.Before(to); d = d.Add(24 * time.Hour) {
		var day []firing.Firing
		for _, f := range fs {
			if !f.Start.Before(d) && f.Start.Before(d.Add(24*time.Hour)) {
				day = append(day, f)
			}
		}
		record(t, l, d, d.Add(24*time.Hour-time.Second), day)
	}
	kept, err := l.AllMarked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Firings(ctx, FiringQuery{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, f := range got {
		var want []string
		for _, m := range kept {
			if m.Covers(f.Start) {
				want = append(want, m.Kind)
			}
		}
		if fmt.Sprint(want) != fmt.Sprint(f.Kinds) {
			if bad++; bad < 10 {
				t.Errorf("%s: marked %v, Covers says %v", f.Start.In(loc), f.Kinds, want)
			}
		}
	}
	for _, m := range kept {
		os, _ := m.Occurrences(from, to, 1000)
		for _, o := range os {
			if !m.Covers(o.Start) || !m.Covers(o.End) {
				t.Errorf("%s: occurrence %s → %s isn't covered", m.Title, o.Start.In(loc), o.End.In(loc))
			}
		}
	}
}

// An overnight window repeating each day, its firings judged by hourly
// runs: a run after midnight starts inside the occurrence that started
// the day before, and marks its firings.
func TestMarkedOvernightHourly(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.PutMarked(ctx, Marked{Kind: "sale", Title: "night", Start: t0.Add(23 * time.Hour), End: t0.Add(27 * time.Hour), Every: "day"}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	for h := 0; h < 72; h++ {
		from := t0.Add(time.Duration(h) * time.Hour)
		record(t, l, from, from.Add(time.Hour-time.Second), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"},
			Start: from.Add(30 * time.Minute), End: from.Add(31 * time.Minute)}})
	}
	// 23:30 on each of the three days, and 00:30 to 02:30 after the first
	// two: the run of 01:00 judges from 00:45, the day after the
	// occurrence began.
	if got, _ := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(72 * time.Hour), Kind: "sale"}); len(got) != 9 {
		t.Errorf("%d marked, want 9", len(got))
	}
}

// A firing at an occurrence's very end is marked, as Covers has it; one
// in the second week of an occurrence over two, judged by a run of that
// week alone, too.
func TestMarkedEdges(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	sat := t0.Add(5*24*time.Hour + 20*time.Hour) // Saturday 20:00
	if _, err := l.PutMarked(ctx, Marked{Kind: "sale", Title: "weekend", Start: sat, End: sat.Add(36 * time.Hour)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	mon := t0.Add(7 * 24 * time.Hour)
	record(t, l, sat.Add(-time.Hour), mon, []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "a"}, Start: sat.Add(time.Hour), End: sat.Add(2 * time.Hour)}})
	// Monday's run, alone: the occurrence's last hours and its very end.
	record(t, l, mon, mon.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "b"}, Start: mon.Add(2 * time.Hour), End: mon.Add(3 * time.Hour)},
		{Rule: "Errors", Labels: map[string]string{"service": "c"}, Start: sat.Add(36 * time.Hour), End: sat.Add(37 * time.Hour)},
		{Rule: "Errors", Labels: map[string]string{"service": "d"}, Start: sat.Add(36*time.Hour + time.Second), End: sat.Add(37 * time.Hour)},
	})
	got := markedFirings(t, l, mon)
	if got["Errors b 02:00"] != "sale" || got["Errors c 08:00"] != "sale" || got["Errors d 08:00"] != "" {
		t.Errorf("Monday's: %v", got)
	}
}

// A report over a week with no record counts each rule's marked firings,
// and of them, as unjudged, those that caught nothing.
func TestMarkedUnjudged(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(10 * time.Hour)
	if _, err := l.PutMarked(ctx, Marked{Kind: "sale", Title: "Sale", Start: t0, End: t0.Add(24 * time.Hour)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(time.Minute)},
		{Rule: "Errors", Labels: map[string]string{"service": "a"}, Start: at.Add(2 * time.Hour), End: at.Add(2*time.Hour + time.Minute)},
	})
	rep, err := l.Report(ctx, t0, t0.Add(7*24*time.Hour), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.NoRecord || len(rep.Rules) != 1 || rep.Rules[0].Marked["sale"].Unjudged != 2 || rep.Rules[0].Marked["sale"].Firings != 2 {
		t.Errorf("a week with no record: %+v", rep.Rules)
	}
}

// A team removed: its windows' team is negated, so they mark nothing,
// even when a team made after is given its id; the windows' pages say it
// was removed.
func TestMarkedTeamRemoved(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(10 * time.Hour)
	a, err := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	id, err := l.PutMarked(ctx, Marked{Kind: "launch", Title: "Launch", Start: t0, End: t0.Add(24 * time.Hour), TeamID: a}, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteTeam(ctx, a, "ana"); err != nil {
		t.Fatal(err)
	}
	if m, err := l.MarkedByID(ctx, id); err != nil || m.TeamID != -a {
		t.Fatalf("the window of a removed team: %+v %v", m, err)
	}
	b, err := l.PutTeam(ctx, Team{Name: "cart", Rules: "^Cart"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the team removed was %d, the next %d", a, b)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "CartSlow", Labels: map[string]string{"service": "cart"}, Start: at, End: at.Add(time.Minute)}})
	if got := markedFirings(t, l, t0); got["CartSlow cart 10:00"] != "" {
		t.Errorf("a removed team's window marked another team's rule: %v", got)
	}
}

// Pruning leaves a repeating window until its last occurrence is over:
// the firings it marked, after the cut, keep their kind.
func TestMarkedPruneKeepsLastRepeat(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	// Every week, five days long, the last starting on t0's Monday.
	start := t0.Add(-21 * 24 * time.Hour)
	if _, err := l.PutMarked(ctx, Marked{Kind: "sale", Title: "Weekly sale", Start: start, End: start.Add(5 * 24 * time.Hour), Every: "week", Until: t0}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(3 * 24 * time.Hour)
	record(t, l, t0, t0.Add(7*24*time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(time.Minute)}})
	if err := l.Prune(ctx, t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ms, _ := l.AllMarked(ctx); len(ms) != 1 {
		t.Errorf("%d windows after pruning past its until, before its last occurrence ended", len(ms))
	}
	if got := markedFirings(t, l, at.Truncate(24*time.Hour)); got["Errors a 00:00"] != "sale" {
		t.Errorf("after pruning: %v", got)
	}
}

// A window that would go on is dropped at the bounds: the one it would
// go on from is kept as it was, with the firings it marks, not deleted
// with nothing in its place.
func TestMarkedGoingOnDroppedKeepsOld(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	next := t0.Add(7 * 24 * time.Hour) // the Monday after
	record(t, l, t0, next.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: next.Add(-3 * time.Hour), End: next.Add(-3*time.Hour + time.Minute)}})
	// The week after holds the 64 occurrences of sale a week meets.
	var flash []Marked
	for i := 0; i < MarkedKindWeek; i++ {
		s := next.Add(time.Duration(i+2) * time.Hour)
		flash = append(flash, Marked{Kind: "sale", Title: fmt.Sprintf("Flash %d", i), Start: s, End: s.Add(30 * time.Minute)})
	}
	if _, err := l.SyncMarked(ctx, flash, opts, MarkedRead{}); err != nil {
		t.Fatal(err)
	}
	step := time.Minute
	old := Marked{Start: next.Add(-4 * time.Hour), End: next.Add(-30 * time.Minute)}
	if _, err := l.KeepMarks(ctx, []MarkRead{{Kind: "sale", Key: "k", Since: old.Start, To: old.End, Step: step, Windows: []Marked{old}}}, opts); err != nil {
		t.Fatal(err)
	}
	// On at the next read's start, and found going on into the week
	// after, which has no room for it; another series found in the week,
	// kept, so the read is written.
	since, to := next.Add(-time.Hour), next.Add(time.Hour)
	other := Marked{Start: since.Add(30 * time.Minute), End: since.Add(50 * time.Minute)}
	n, err := l.KeepMarks(ctx, []MarkRead{{Kind: "sale", Key: "k", Since: since, To: to, Step: step, Windows: []Marked{{Start: since, End: to.Add(step)}, other}}}, opts)
	if err != nil || n != 1 {
		t.Fatalf("%d dropped, %v; want 1", n, err)
	}
	all, err := l.AllMarked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var expr []Marked
	for _, m := range all {
		if m.Origin == OriginExpr {
			expr = append(expr, m)
		}
	}
	if len(expr) != 2 || !expr[0].Start.Equal(old.Start) || !expr[0].End.Equal(old.End) || !expr[1].Start.Equal(other.Start) {
		t.Errorf("the expression's windows: %+v, want the one before, as it was, and the other", expr)
	}
	fs, err := l.Firings(ctx, FiringQuery{From: t0, To: next, Kind: "sale"})
	if err != nil || len(fs) != 1 {
		t.Errorf("%d firings marked, %v; want the one in the window before", len(fs), err)
	}
}

// The windows made in the UI take MarkedUIKinds of the kinds at most,
// counting a kind no window names any more that isn't let go yet, whoever
// let go of it; a write that takes no more of them than they take is let
// be. The marked file and an expression have the rest of the kinds.
func TestMarkedUIKinds(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Now().UTC().Truncate(time.Hour)
	var ids []int64
	for i := range MarkedUIKinds - 1 {
		id, err := l.PutMarked(ctx, Marked{Kind: fmt.Sprint("ui-", i), Title: "x", Start: start, End: start.Add(time.Hour)}, "ed", opts)
		if err != nil {
			t.Fatal(i, err)
		}
		ids = append(ids, id)
	}
	// A kind nothing names, not let go yet.
	if err := l.write(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "INSERT INTO marked_kinds (id, name) VALUES (?, ?)", MaxKinds, "gone")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var inv ErrInvalid
	if _, err := l.PutMarked(ctx, Marked{Kind: "ui-new", Title: "x", Start: start, End: start.Add(time.Hour)}, "ed", opts); !errors.As(err, &inv) {
		t.Errorf("a kind past the UI's share, one not let go yet among them: %v", err)
	}
	if _, err := l.PutMarked(ctx, Marked{ID: ids[0], Kind: "ui-new", Title: "x", Start: start, End: start.Add(time.Hour)}, "ed", opts); !errors.As(err, &inv) {
		t.Errorf("a window changed to a new kind, its own not let go yet: %v", err)
	}
	if _, err := l.PutMarked(ctx, Marked{ID: ids[0], Kind: "ui-0", Title: "renamed", Start: start, End: start.Add(time.Hour)}, "ed", opts); err != nil {
		t.Errorf("a new title: %v", err)
	}
	if _, err := l.PutMarked(ctx, Marked{Kind: "ui-1", Title: "another", Start: start, End: start.Add(time.Hour)}, "ed", opts); err != nil {
		t.Errorf("another window of a kind the UI's take: %v", err)
	}
	var file []Marked
	for i := range MaxKinds - MarkedUIKinds - 1 {
		file = append(file, Marked{Kind: fmt.Sprint("file-", i), Title: "x", Start: start, End: start.Add(time.Hour)})
	}
	if _, err := l.SyncMarked(ctx, file, opts, MarkedRead{Sum: "f"}); err != nil {
		t.Errorf("the marked file, with the rest of the kinds but one: %v", err)
	}
	said, err := l.KeepMarksSaid(ctx, []MarkRead{{Kind: "flag", Key: "k", Since: start.Add(-time.Hour), To: start, Step: time.Minute,
		Windows: []Marked{{Start: start.Add(-time.Hour), End: start}}}}, opts)
	if err != nil || len(said.Kinds) != 0 || said.Dropped != 0 {
		t.Errorf("an expression of the last kind: %+v %v", said, err)
	}
}
