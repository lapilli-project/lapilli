package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/verdict"
)

var t0 = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) // a Monday

func open(t testing.TB, loc *time.Location) *Ledger {
	t.Helper()
	l, err := openAt(context.Background(), place(t), loc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// place is where a test's ledger is kept: a file of its own, or, with
// LAPILLI_TEST_POSTGRES, a schema of its own in that database.
func place(t testing.TB) string {
	t.Helper()
	if u := ledgertest.URL(t); u != "" {
		return u
	}
	return filepath.Join(t.TempDir(), "ledger.db")
}

// openAt opens the ledger a place names.
func openAt(ctx context.Context, where string, loc *time.Location) (*Ledger, error) {
	if strings.HasPrefix(where, "postgres") {
		return OpenPostgres(ctx, where, loc)
	}
	return Open(ctx, where, loc)
}

var opts = verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}}

// world makes firings and incidents at random over a week, none of them
// near its edges, so that the ledger and verdict.Judge see the same.
func world(seed uint64, n int) ([]firing.Firing, []incident.Incident) {
	r := rand.New(rand.NewPCG(seed, 1))
	services := []string{"checkout", "search", "cart"}
	var fs []firing.Firing
	for i := 0; i < n; i++ {
		start := t0.Add(2*time.Hour + time.Duration(r.IntN(6*24*60))*time.Minute)
		f := firing.Firing{
			Rule:   fmt.Sprintf("Rule%d", r.IntN(12)),
			Labels: map[string]string{"service": services[r.IntN(3)], "severity": "page"},
			Start:  start,
		}
		if r.IntN(10) > 0 {
			f.End = start.Add(time.Duration(1+r.IntN(90)) * time.Minute)
		}
		fs = append(fs, f)
	}
	var incs []incident.Incident
	for i := 0; i < n/20+1; i++ {
		start := t0.Add(3*time.Hour + time.Duration(r.IntN(6*24*60))*time.Minute)
		in := incident.Incident{ID: fmt.Sprintf("INC-%d", i), Title: "broken", Start: start,
			Labels: incident.Labels{"service": {services[r.IntN(3)]}}}
		if r.IntN(3) > 0 {
			in.End = start.Add(time.Duration(5+r.IntN(120)) * time.Minute)
		}
		incs = append(incs, in)
	}
	slices.SortStableFunc(incs, func(a, b incident.Incident) int { return a.Start.Compare(b.Start) })
	return fs, incs
}

func record(t testing.TB, l *Ledger, from, to time.Time, fs []firing.Firing) {
	t.Helper()
	f := Found{From: from, To: to, Firings: fs, Run: Run{Kind: "test", Started: to, Finished: to, From: from, To: to}}
	if err := l.Record(context.Background(), f, opts); err != nil {
		t.Fatal(err)
	}
}

// The ledger's verdict on a period is verdict.Judge's.
func TestSameAsJudge(t *testing.T) {
	ctx := context.Background()
	for seed := uint64(1); seed <= 5; seed++ {
		l := open(t, time.UTC)
		fs, incs := world(seed, 400)
		if _, err := l.SyncFile(ctx, incs, opts); err != nil {
			t.Fatal(err)
		}
		// Recorded in two runs, the second taking up the first's open
		// firings.
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
		record(t, l, mid.Add(-2*time.Hour), t0.Add(7*24*time.Hour), fs)

		o := opts
		o.From, o.To = t0, t0.Add(7*24*time.Hour)
		want := verdict.Judge(nil, slices.Clone(fs), incs, o)
		got, err := l.Report(ctx, o.From, o.To, o, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(norm(got.Rules), norm(want.Rules)) {
			t.Errorf("seed %d rules\n got %+v\nwant %+v", seed, got.Rules, want.Rules)
		}
		if !reflect.DeepEqual(got.Incidents, want.Incidents) {
			t.Errorf("seed %d incidents\n got %+v\nwant %+v", seed, got.Incidents, want.Incidents)
		}
		if len(got.Firings) != len(want.Firings) {
			t.Fatalf("seed %d: %d firings, want %d", seed, len(got.Firings), len(want.Firings))
		}
		whites := func(js []verdict.Judged) []string {
			var out []string
			for _, j := range js {
				out = append(out, fmt.Sprintf("%s %v %s %v %v", j.Rule, j.Labels, j.Start, j.White, j.Incidents))
			}
			slices.Sort(out)
			return out
		}
		if g, w := whites(got.Firings), whites(want.Firings); !slices.Equal(g, w) {
			t.Errorf("seed %d stones differ", seed)
		}
		// The weekly tally says the same as the report.
		weeks, err := l.Weeks(ctx, l.Day(t0), l.Day(t0))
		if err != nil {
			t.Fatal(err)
		}
		table, _ := l.RuleTable(ctx)
		for _, w := range weeks {
			i := slices.IndexFunc(want.Rules, func(r verdict.RuleResult) bool { return r.Rule == table[w.Rule].Name })
			if r := want.Rules[i]; w.Firings != r.Firings || w.White != r.White || w.Refired != r.Refired {
				t.Errorf("seed %d week of %s: %+v, report %+v", seed, r.Rule, w, r)
			}
		}
	}
}

// norm leaves out what Judge knows only from rules replayed.
func norm(rs []verdict.RuleResult) []verdict.RuleResult {
	out := slices.Clone(rs)
	for i := range out {
		out[i].Severity, out[i].Group = "", ""
	}
	return out
}

// Recording the same thing twice changes nothing.
func TestRecordTwice(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	fs, incs := world(7, 300)
	if _, err := l.SyncFile(ctx, incs, opts); err != nil {
		t.Fatal(err)
	}
	end := t0.Add(7 * 24 * time.Hour)
	record(t, l, t0, end, fs)
	before := dump(t, l)
	record(t, l, t0.Add(24*time.Hour), end, fs)
	if after := dump(t, l); before != after {
		t.Errorf("a second run over the same firings changed the ledger:\n%s", diff(before, after))
	}
}

func dump(t testing.TB, l *Ledger) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		"SELECT r.name, s.labels, f.start_at, coalesce(f.end_at, -1), f.day, f.white, f.refired FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id ORDER BY 1, 2, 3",
		"SELECT i.key, r.name, c.delay FROM catches c JOIN incidents i ON i.id = c.incident_id JOIN rules r ON r.id = c.rule_id ORDER BY 1, 2",
		"SELECT d.day, r.name, d.firings, d.white, d.refired FROM days d JOIN rules r ON r.id = d.rule_id ORDER BY 1, 2",
		"SELECT w.week, r.name, w.firings, w.white, w.refired FROM weeks w JOIN rules r ON r.id = w.rule_id ORDER BY 1, 2",
		"SELECT key, origin, state, counts, start_at, coalesce(end_at, -1) FROM incidents ORDER BY 1",
	} {
		rows, err := l.r.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

func diff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for _, l := range x {
		if !slices.Contains(y, l) {
			out = append(out, "- "+l)
		}
	}
	for _, l := range y {
		if !slices.Contains(x, l) {
			out = append(out, "+ "+l)
		}
	}
	return strings.Join(out, "\n")
}

// A firing still on at the end of one run is ended by the next, not found
// twice; one the next run no longer sees ends where its replay started.
func TestContinued(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	svc := map[string]string{"service": "checkout"}
	open1 := firing.Firing{Rule: "Errors", Labels: svc, Start: t0.Add(25 * time.Minute)}
	gone := firing.Firing{Rule: "Disk", Labels: svc, Start: t0.Add(30 * time.Minute)}
	record(t, l, t0, t0.Add(time.Hour), []firing.Firing{open1, gone})

	// The next run replays from 40m on: Errors, already firing then, seems
	// to start once its 5m "for" is up, at 45m, and ends at 1h30.
	seen := open1
	seen.Start, seen.End = t0.Add(45*time.Minute), t0.Add(90*time.Minute)
	f := Found{From: t0.Add(time.Hour), To: t0.Add(2 * time.Hour), Since: map[string]time.Time{"Errors": t0.Add(40 * time.Minute), "Disk": t0.Add(40 * time.Minute)},
		Firings: []firing.Firing{seen}, Run: Run{Kind: "test", Started: t0, Finished: t0}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	got, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range got {
		lines = append(lines, fmt.Sprintf("%s %s→%s", f.Rule, f.Start.Sub(t0), f.End.Sub(t0)))
	}
	want := []string{"Disk 30m0s→40m0s", "Errors 25m0s→1h30m0s"}
	if !slices.Equal(lines, want) {
		t.Errorf("firings %q, want %q", lines, want)
	}
	if c, _ := l.Cursor(ctx); !c.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("cursor %s", c)
	}
}

// An incident written down later turns the firings that caught it white,
// in the firings, the catches and the tallies.
func TestIncidentLater(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	svc := map[string]string{"service": "checkout"}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: svc, Start: t0.Add(10 * time.Hour), End: t0.Add(11 * time.Hour)},
		{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: t0.Add(10 * time.Hour), End: t0.Add(11 * time.Hour)},
	})
	weekWhite := func() int {
		ws, err := l.Weeks(ctx, l.Day(t0), l.Day(t0))
		if err != nil || len(ws) != 1 {
			t.Fatalf("weeks %v %v", ws, err)
		}
		return ws[0].White
	}
	if w := weekWhite(); w != 0 {
		t.Fatalf("white %d before any incident", w)
	}
	gen := l.Gen()
	id, err := l.Put(ctx, incident.Incident{Title: "Checkout down", Start: t0.Add(10*time.Hour + 5*time.Minute),
		Labels: incident.Labels{"service": {"checkout"}}}, "ana@example.com", opts)
	if err != nil {
		t.Fatal(err)
	}
	if id != "L-1" || l.Gen() == gen {
		t.Errorf("id %q, gen %d → %d", id, gen, l.Gen())
	}
	if w := weekWhite(); w != 1 {
		t.Errorf("white %d after the incident, want 1: only checkout's firing caught it", w)
	}
	in, err := l.IncidentByID(ctx, "L-1")
	if err != nil || len(in.CaughtBy) != 1 || in.CaughtBy[0].Delay != -5*time.Minute || in.By != "ana@example.com" {
		t.Errorf("incident %+v %v", in, err)
	}
	// Moved away, it catches nothing.
	in.Incident.Start = t0.Add(20 * time.Hour)
	if _, err := l.Put(ctx, in.Incident, "ana@example.com", opts); err != nil {
		t.Fatal(err)
	}
	if w := weekWhite(); w != 0 {
		t.Errorf("white %d after moving the incident", w)
	}
	if err := l.Delete(ctx, "L-1", "ana@example.com", opts); err != nil {
		t.Fatal(err)
	}
	cs, _ := l.Changes(ctx, 10)
	if len(cs) != 3 || cs[0].What != "incident L-1 deleted" {
		t.Errorf("changes %+v", cs)
	}
	// One from the file isn't the UI's to change.
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Start: t0}}, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Put(ctx, incident.Incident{ID: "INC-1", Start: t0}, "x", opts); err != ErrNotYours {
		t.Errorf("put over the file's: %v", err)
	}
}

// An impact window covered by an incident written down doesn't count;
// dismissed, it doesn't either; confirmed, it is written down.
func TestImpact(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	svc := incident.Labels{"service": {"checkout"}}
	window := incident.Incident{ID: "impact-1", Title: "user impact", Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour), Labels: svc, Candidate: true}
	other := incident.Incident{ID: "impact-2", Title: "user impact", Start: t0.Add(5 * time.Hour), End: t0.Add(6 * time.Hour), Labels: svc, Candidate: true}
	f := Found{From: t0, To: t0.Add(8 * time.Hour), ImpactOn: true, ImpactSince: t0,
		Impact:  []incident.Incident{window, other},
		Firings: []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(5*time.Hour + time.Minute)}},
		Run:     Run{Kind: "test", Started: t0, Finished: t0}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	counts := func() []string {
		incs, err := l.Incidents(ctx, t0, t0.Add(24*time.Hour), true)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, in := range incs {
			out = append(out, fmt.Sprintf("%s %s %v %d", in.ID, in.State, in.Counts, len(in.CaughtBy)))
		}
		return out
	}
	if got := counts(); !slices.Equal(got, []string{"impact-20261102T0200Z  true 0", "impact-20261102T0500Z  true 1"}) {
		t.Errorf("found %q", got)
	}
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-9", Start: t0.Add(2*time.Hour + 10*time.Minute), Labels: svc}}, opts); err != nil {
		t.Fatal(err)
	}
	if got := counts(); !slices.Equal(got, []string{"impact-20261102T0200Z  false 0", "INC-9  true 0", "impact-20261102T0500Z  true 1"}) {
		t.Errorf("covered: %q", got)
	}
	made, err := l.Decide(ctx, "impact-20261102T0500Z", "confirmed", "Checkout slow", "ana", opts)
	if err != nil || made != "L-1" {
		t.Fatalf("confirm: %q %v", made, err)
	}
	if got := counts(); !slices.Equal(got, []string{"impact-20261102T0200Z  false 0", "INC-9  true 0", "L-1  true 1", "impact-20261102T0500Z confirmed false 0"}) {
		t.Errorf("confirmed: %q", got)
	}
	// A run over the same hours keeps what was decided.
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if got := counts(); !slices.Equal(got, []string{"impact-20261102T0200Z  false 0", "INC-9  true 0", "L-1  true 1", "impact-20261102T0500Z confirmed false 0"}) {
		t.Errorf("after another run: %q", got)
	}
}

// Opened in another zone, the ledger counts its days again.
func TestZone(t *testing.T) {
	ctx := context.Background()
	path := place(t)
	l, err := openAt(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Errors", Start: t0.Add(20 * time.Hour), End: t0.Add(21 * time.Hour)}})
	l.Close()
	seoul, _ := time.LoadLocation("Asia/Seoul")
	l, err = openAt(ctx, path, seoul)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	days, err := l.Days(ctx, 0, 1<<30)
	if err != nil || len(days) != 1 || days[0].At != l.Day(t0)+1 {
		t.Errorf("20:00 UTC is the next day in Seoul: %+v %v", days, err)
	}
}

func TestVersions(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for i, since := range []time.Time{{}, t0.Add(time.Hour), t0.Add(5 * time.Hour)} {
		v := Version{Since: since, Digest: fmt.Sprint(i), Names: []string{fmt.Sprint("R", i)},
			Docs: nil}
		if _, err := l.AddVersion(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := l.Versions(ctx, t0.Add(2*time.Hour), t0.Add(6*time.Hour), true)
	if err != nil || len(vs) != 2 || vs[0].Digest != "1" || vs[1].Digest != "2" {
		t.Errorf("versions %+v %v", vs, err)
	}
	vs, _ = l.Versions(ctx, t0.Add(-time.Hour), t0, false)
	if len(vs) != 1 || vs[0].Digest != "0" || !vs[0].Since.IsZero() {
		t.Errorf("before the first change: %+v", vs)
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	fs, _ := world(3, 200)
	record(t, l, t0, t0.Add(7*24*time.Hour), fs)
	if err := l.Prune(ctx, t0.Add(4*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	lo, _, err := l.Bounds(ctx)
	if err != nil || lo.Before(t0.Add(4*24*time.Hour)) {
		t.Errorf("earliest %s %v", lo, err)
	}
	days, _ := l.Days(ctx, 0, l.Day(t0.Add(4*24*time.Hour))-1)
	if len(days) != 0 {
		t.Errorf("days before kept: %v", days)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if err := l.SetSettings(ctx, map[string]string{"lead": "10m"}, "ana"); err != nil {
		t.Fatal(err)
	}
	s, _ := l.Settings(ctx)
	if s["lead"].Value != "10m" || s["lead"].By != "ana" {
		t.Errorf("settings %+v", s)
	}
	if err := l.SetSettings(ctx, map[string]string{"lead": ""}, "ana"); err != nil {
		t.Fatal(err)
	}
	if s, _ := l.Settings(ctx); len(s) != 0 {
		t.Errorf("reset: %+v", s)
	}
	if err := l.AddResult(ctx, "suggestions", t0, t0, t0.Add(time.Hour), []verdict.Suggestion{{Rule: "X", NewFor: time.Hour}}); err != nil {
		t.Fatal(err)
	}
	rs, _ := l.Results(ctx, "suggestions", 1)
	var got []verdict.Suggestion
	if len(rs) != 1 || rs[0].Decode(&got) != nil || got[0].Rule != "X" {
		t.Errorf("results %+v %+v", rs, got)
	}
	_ = json.Marshal
}

// An incidents file written in another zone is the same incidents, read
// again; nothing is judged again.
func TestSyncFileZones(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	seoul := time.FixedZone("KST", 9*3600)
	incs := []incident.Incident{{ID: "INC-1", Start: t0.In(seoul), End: t0.Add(time.Hour).In(seoul), Labels: incident.Labels{"service": {"checkout"}}}}
	if changed, err := l.SyncFile(ctx, incs, opts); err != nil || !changed {
		t.Fatalf("first read: %v %v", changed, err)
	}
	gen := l.Gen()
	if changed, err := l.SyncFile(ctx, incs, opts); err != nil || changed {
		t.Errorf("read again: changed %v %v", changed, err)
	}
	if l.Gen() != gen+1 { // the write happens, and changes nothing but the count
		t.Logf("gen %d → %d", gen, l.Gen())
	}
}

// A window confirmed twice is written down once.
func TestDecideTwice(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	f := Found{From: t0, To: t0.Add(4 * time.Hour), ImpactOn: true, ImpactSince: t0,
		Impact: []incident.Incident{{Title: "user impact", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour), Candidate: true}},
		Run:    Run{Kind: "test", Started: t0, Finished: t0}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	key := "impact-20261102T0100Z"
	for range 2 {
		if _, err := l.Decide(ctx, key, "confirmed", "", "ana", opts); err != nil {
			t.Fatal(err)
		}
	}
	incs, _ := l.Incidents(ctx, t0, t0.Add(24*time.Hour), true)
	if len(incs) != 2 {
		t.Errorf("want the window and one incident: %+v", incs)
	}
	// A run over the same hours, the window grown another service, keeps
	// the decision and who made it.
	f.Impact[0].Labels = incident.Labels{"service": {"cart", "checkout"}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	in, err := l.IncidentByID(ctx, key)
	if err != nil || in.State != "confirmed" || in.By != "ana" {
		t.Errorf("after another run: %+v %v", in, err)
	}
}

// A rule a run couldn't replay is replayed by the next from where it was
// missed: no firing is lost, and none is folded into another.
func TestMissed(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	svc := map[string]string{"service": "checkout"}
	a := firing.Firing{Rule: "Errors", Labels: svc, Start: t0.Add(10 * time.Minute)}
	record(t, l, t0, t0.Add(time.Hour), []firing.Firing{a})
	// The next run, [1h, 3h], can't replay Errors.
	f := Found{From: t0.Add(time.Hour), To: t0.Add(3 * time.Hour), Keep: map[string]bool{"Errors": true},
		Rules: []Rule{{Name: "Errors"}}, Run: Run{Kind: "test", Started: t0, Finished: t0}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	missed, err := l.Missed(ctx)
	if err != nil || !missed["Errors"].Equal(t0.Add(time.Hour)) {
		t.Fatalf("missed %v %v", missed, err)
	}
	// The one after, [2h, 4h], replays Errors from 1h less its margin: A
	// ended at 1:10, B ran 1:20 to 1:30, D 1:50 to 1:55, C from 2:20.
	ended := a
	ended.End = t0.Add(70 * time.Minute)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	f = Found{From: at(120), To: at(240), Missed: missed,
		Since: map[string]time.Time{"Errors": at(45)}, Fresh: map[string]time.Time{"Errors": at(50)},
		Rules: []Rule{{Name: "Errors"}}, Run: Run{Kind: "test", Started: t0, Finished: t0},
		Firings: []firing.Firing{
			{Rule: "Errors", Labels: svc, Start: at(50), End: at(70)}, // A, seen from 45m: on already
			{Rule: "Errors", Labels: svc, Start: at(80), End: at(90)},
			{Rule: "Errors", Labels: svc, Start: at(110), End: at(115)},
			{Rule: "Errors", Labels: svc, Start: at(140)},
		}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	got, err := l.Firings(ctx, FiringQuery{From: t0, To: at(300)})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, x := range got {
		end := "on"
		if !x.End.IsZero() {
			end = x.End.Sub(t0).String()
		}
		lines = append(lines, fmt.Sprintf("%s→%s", x.Start.Sub(t0), end))
	}
	want := []string{"2h20m0s→on", "1h50m0s→1h55m0s", "1h20m0s→1h30m0s", "10m0s→1h10m0s"}
	if !slices.Equal(lines, want) {
		t.Errorf("firings %q, want %q", lines, want)
	}
	if m, _ := l.Missed(ctx); len(m) != 0 {
		t.Errorf("still missed: %v", m)
	}
}

// A relative path, as --ledger's default (lapilli-ledger) is, opens a
// ledger there: written as a URI, its first directory was read as the host
// (file://lapilli-ledger/ledger.db), and SQLite refused it, so lapilli serve
// didn't start without an absolute --ledger.
func TestOpenRelative(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Chdir(dir)
	for _, path := range []string{filepath.Join("lapilli-ledger", "ledger.db"), "ledger.db", filepath.Join(".", "a#1", "ledger.db")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		l, err := Open(ctx, path, time.UTC)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		l.Close()
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Errorf("%s: no ledger where it was asked for: %v", path, err)
		}
	}
}

// A ledger's path is its own whatever its directory is named: a # or a ?
// in it used to end the path there, so two ledgers named alike before it
// were one file, and a % or a space was read as something else.
func TestOpenPathNamed(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"a#1", "a#2", "b?x=1", "c%20d", "e f", "g%"} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "ledger.db")
		l, err := Open(ctx, path, time.UTC)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := l.SetMeta(ctx, "name", name); err != nil {
			t.Fatal(err)
		}
		l.Close()
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: no ledger where it was asked for: %v", name, err)
		}
		entries, _ := os.ReadDir(filepath.Dir(dir))
		if len(entries) != 1 {
			t.Errorf("%s: %d files beside its directory, want none", name, len(entries)-1)
		}
		l, err = Open(ctx, path, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := l.Meta(ctx, "name"); got != name {
			t.Errorf("%s: opened again, it holds %q", name, got)
		}
		l.Close()
	}
}

// A path is written as a file URI with its drive, on Windows, in the path,
// not as the URI's host, and what would end the path escaped.
func TestFileURI(t *testing.T) {
	for in, want := range map[string]string{
		"/var/lib/lapilli/ledger": "file:///var/lib/lapilli/ledger",
		"/a b/#c?d%e":             "file:///a%20b/%23c%3Fd%25e",
		"C:/ledgers/lapilli":      "file:///C:/ledgers/lapilli",
	} {
		u := fileURI(in)
		if got := u.String(); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
