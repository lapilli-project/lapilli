package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// The search firings come from 10:00 on Sunday 1 November to 05:00 on the
// Monday, an hour apart, and catch nothing: twelve off hours, two asleep
// that Sunday night and six more after midnight.
func TestHours(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()

	code, body, _ := get(t, h, "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false")
	if code != 200 || !strings.Contains(body, `"asleep_black": 8`) || !strings.Contains(body, `"off_hours_black": 12`) ||
		!strings.Contains(body, `"sleep": "22:00-08:00"`) {
		t.Errorf("report: %d %s", code, body)
	}
	// Over more than 31 days, a report doesn't read every firing again to
	// count them by the hours.
	if _, body, _ := get(t, h, "/api/v1/report?from=2026-09-01T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false"); strings.Contains(body, `"hours"`) {
		t.Errorf("hours over two months: %s", body)
	}
	if _, body, _ := get(t, h, "/rules"); !strings.Contains(body, "8 asleep, 12 off hours") {
		t.Errorf("rules: %s", body)
	}
	if _, body, _ := get(t, h, "/rules/Errors"); !strings.Contains(body, "8 came while people slept, and 12 more off hours") {
		t.Errorf("rule: %s", body)
	}
	// The last whole week has the two on Sunday night.
	_, body, _ = get(t, h, "/")
	i := strings.Index(body, "Woken for nothing")
	if i < 0 || !strings.Contains(body[i:min(len(body), i+400)], ">2<") || !strings.Contains(body, "firings that caught nothing, 22:00–08:00") {
		t.Errorf("overview: %s", body)
	}
	// An incident written down at 22:00 that Sunday turns the firing then
	// white, and the week is counted again.
	if code, body := api(t, h, "PUT", "/api/v1/incidents/INC-9", `{"title": "Search down", "start": "2026-11-01T22:00:00Z", "end": "2026-11-01T22:30:00Z", "labels": {"service": ["search"]}}`); code/100 != 2 {
		t.Fatalf("incident: %d %s", code, body)
	}
	if _, body, _ := get(t, h, "/rules"); !strings.Contains(body, "7 asleep, 12 off hours") {
		t.Errorf("rules after an incident: %s", body)
	}
	if _, body, _ := get(t, s.MetricsHandler(), "/metrics"); !strings.Contains(body, `lapilli_rule_nothing_by_hours{rule="Errors",severity="page",hours="asleep"}`) {
		t.Errorf("metrics: %s", body)
	}

	// Without hours, none of it.
	s2, _ := server(t, nil)
	if err := s2.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s2.jobs(ctx, true)
	if _, body, _ := get(t, s2.Handler(), "/"); strings.Contains(body, "Woken for nothing") {
		t.Error("a tile for hours not given")
	}
}

// An incident nothing caught: the search errors held ten minutes then, but
// Late waits half an hour. The digest works out that ten minutes would have
// caught it, and what that would have cost.
func TestNearMisses(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, nil)
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(`groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
        labels: {severity: page, service: checkout}
      - alert: Late
        expr: errors > 1
        for: 30m
        labels: {severity: page}
`), 0o644)
	os.WriteFile(s.cfg.Incidents, []byte("incidents:\n"+
		"  - {id: INC-1, title: Checkout down, start: 2026-11-02T06:01:00Z, end: 2026-11-02T06:40:00Z, labels: {service: checkout}}\n"+
		"  - {id: INC-2, title: Search slow, start: 2026-11-01T20:00:00Z, end: 2026-11-01T20:30:00Z, labels: {service: search}}\n"+
		// Still open, as some tools mark it: looked at within the week.
		"  - {id: INC-3, title: Search open, start: 2026-11-01T23:00:00Z, end: 9999-12-31T00:00:00Z, labels: {service: search}}\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	near, err := s.nearMisses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := near["INC-3"]; len(n) != 1 || n[0].Rule != "Late" {
		t.Errorf("INC-3: %+v", n)
	}
	nms := near["INC-2"]
	if len(nms) != 1 || nms[0].Rule != "Late" || nms[0].NewFor != 10*time.Minute || nms[0].Delay != 10*time.Minute || nms[0].After == nil {
		t.Fatalf("near misses: %+v", near)
	}
	// Fourteen of the search stretches are in the week: with ten minutes,
	// each fires, and two catch INC-2 and INC-3.
	if a := nms[0].After; a.Firings != 14 || a.White != 2 || nms[0].Before.Firings != 0 {
		t.Errorf("cost: before %+v, after %+v", nms[0].Before, *a)
	}
	h := s.Handler()
	_, body, _ := get(t, h, "/incidents/INC-2")
	if !strings.Contains(body, "Nearly caught by") || !strings.Contains(body, `under its &#34;for&#34; of 30m: with 10m it would have fired at &#43;10m`) ||
		!strings.Contains(body, "fired 14 times instead of 0, 12 of them catching nothing") {
		t.Errorf("incident: %s", body)
	}
	if l := nearText("ko", nms[0]); !strings.Contains(l.Text, "조건이 +0s부터 11m 동안") {
		t.Errorf("in Korean: %s", l.Text)
	}
	if _, body, _ := get(t, h, "/incidents/INC-1"); strings.Contains(body, "Nearly caught by") {
		t.Errorf("a caught incident shows near misses: %s", body)
	}
	// The digest has them.
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.digestReport(ctx, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), o)
	if err != nil {
		t.Fatal(err)
	}
	// A team that owns checkout's incidents hears nothing of search's.
	team := teamReport(rep, &owners{rule: map[string]int64{}}, ledger.Team{ID: 1, Name: "shop", Labels: incident.Labels{"service": {"checkout"}}}, nil)
	if len(team.NearMisses) != 0 {
		t.Errorf("another team's near misses: %+v", team.NearMisses)
	}
	var b strings.Builder
	verdict.Text(&b, rep, time.UTC)
	if !strings.Contains(b.String(), "Rules that nearly caught them") || !strings.Contains(b.String(), "INC-2     Late  11m from +0s  for 30m → 10m  +10m        0 → 14  0 → 12") {
		t.Errorf("digest:\n%s", b.String())
	}
}

// An incident written down after the week's digest, or in the week under
// way, gets its near misses then, not at the next digest.
func TestNearMissesLater(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, nil)
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(`groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
        labels: {severity: page, service: checkout}
      - alert: Late
        expr: errors > 1
        for: 30m
        labels: {severity: page}
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	digestWeek, thisWeek := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	// kept says whether a week has near misses kept; asked how many
	// questions the history has had, more when one is worked out again.
	kept := func(week time.Time) bool {
		_, ok, err := s.l.ForPeriod(ctx, "near_misses", week)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	hist := s.h.(*history)
	asked := func() int {
		hist.Lock()
		defer hist.Unlock()
		return hist.asked
	}
	near := func(id string) []verdict.NearMiss {
		n, err := s.nearMisses(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n[id]
	}
	// The digest's week; the week under way the round after, not with the
	// digest; then neither again with nothing new.
	s.jobs(ctx, true)
	if !kept(digestWeek) || kept(thisWeek) {
		t.Fatalf("after the digest: %v %v", kept(digestWeek), kept(thisWeek))
	}
	s.jobs(ctx, false)
	if !kept(thisWeek) {
		t.Fatal("the week under way wasn't worked out")
	}
	n := asked()
	s.jobs(ctx, false)
	if asked() != n {
		t.Error("worked out again with nothing new")
	}

	// INC-2, in the digest's week, written down after it.
	write := func(more string) {
		os.WriteFile(s.cfg.Incidents, []byte("incidents:\n"+
			"  - {id: INC-1, title: Checkout down, start: 2026-11-02T06:01:00Z, end: 2026-11-02T06:40:00Z, labels: {service: checkout}}\n"+more), 0o644)
	}
	inc2 := "  - {id: INC-2, title: Search slow, start: 2026-11-01T20:00:00Z, end: 2026-11-01T20:30:00Z, labels: {service: search}}\n"
	write(inc2)
	s.jobs(ctx, false)
	if n := near("INC-2"); len(n) != 1 || n[0].Rule != "Late" || n[0].NewFor != 10*time.Minute || n[0].After == nil {
		t.Fatalf("INC-2: %+v", n)
	}
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.digestReport(ctx, digestWeek, thisWeek, o)
	if err != nil || len(rep.NearMisses) != 1 {
		t.Errorf("the week's report: %v %+v", err, rep.NearMisses)
	}
	n = asked()
	s.jobs(ctx, false)
	if asked() != n {
		t.Error("worked out again with nothing new")
	}

	// INC-4, in the week under way: Late nearly caught it too.
	inc4 := "  - {id: INC-4, title: Search down, start: 2026-11-02T03:00:00Z, end: 2026-11-02T03:30:00Z, labels: {service: search}}\n"
	write(inc2 + inc4)
	s.jobs(ctx, false)
	if n := near("INC-4"); len(n) != 1 || n[0].Rule != "Late" || n[0].Delay != 10*time.Minute {
		t.Fatalf("INC-4: %+v", n)
	}
	if _, body, _ := get(t, s.Handler(), "/incidents/INC-4"); !strings.Contains(body, "Nearly caught by") {
		t.Errorf("INC-4's page: %s", body)
	}
	if n := near("INC-2"); len(n) != 1 {
		t.Errorf("INC-2 lost: %+v", n)
	}

	// Worked out before the ledger's firings reached the incident's end and
	// --settle after, it is worked out again once they have, and then no more.
	week := ledger.Week(s.l.Day(thisWeek))
	x, ok, err := s.nearSeen(ctx, week)
	if err != nil || !ok || x.Incidents["INC-4"] == 0 {
		t.Fatalf("how far the week was worked out: %v %v %+v", err, ok, x)
	}
	x.Cursor = time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC).Unix() // before 03:30 and an hour
	s.setNearSeen(ctx, week, x)
	n = asked()
	s.jobs(ctx, false)
	if asked() == n {
		t.Error("not worked out again once the firings reached INC-4's end")
	}
	n = asked()
	s.jobs(ctx, false)
	if asked() != n {
		t.Error("worked out again and again")
	}

	// The history failing: what was kept stays, and it isn't asked again
	// until an hour on, unless the incidents change.
	failing := &refusing{}
	s.h = failing
	write(inc2 + inc4 + "  - {id: INC-5, title: Search again, start: 2026-11-02T04:00:00Z, end: 2026-11-02T04:30:00Z, labels: {service: search}}\n")
	s.jobs(ctx, false)
	refused := failing.asked.Load()
	if refused == 0 || len(near("INC-4")) != 1 || len(near("INC-5")) != 0 {
		t.Fatalf("failing: asked %d, INC-4 %+v", refused, near("INC-4"))
	}
	s.jobs(ctx, false)
	if failing.asked.Load() != refused {
		t.Error("asked again at once")
	}
	s.h = hist
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	s.jobs(ctx, false)
	if len(near("INC-5")) != 1 || len(near("INC-4")) != 1 {
		t.Errorf("an hour on: INC-5 %+v, INC-4 %+v", near("INC-5"), near("INC-4"))
	}

	// Taken out of the file: its near misses go, the others' stay.
	write(inc2)
	s.jobs(ctx, false)
	if len(near("INC-4")) != 0 || len(near("INC-5")) != 0 || len(near("INC-2")) != 1 {
		t.Errorf("INC-4 and INC-5 gone: %+v %+v %+v", near("INC-4"), near("INC-5"), near("INC-2"))
	}
}

// A digest whose questions fail keeps what was kept for the week, and the
// week is worked out again once the history answers.
func TestNearMissesDigestFails(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, nil)
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(`groups:
  - name: shop
    rules:
      - alert: Late
        expr: errors > 1
        for: 30m
        labels: {severity: page}
`), 0o644)
	os.WriteFile(s.cfg.Incidents, []byte("incidents:\n"+
		"  - {id: INC-1, title: Checkout down, start: 2026-11-02T06:01:00Z, end: 2026-11-02T06:40:00Z, labels: {service: checkout}}\n"+
		"  - {id: INC-2, title: Search slow, start: 2026-11-01T20:00:00Z, end: 2026-11-01T20:30:00Z, labels: {service: search}}\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	near := func(id string) int {
		n, err := s.nearMisses(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(n[id])
	}
	if near("INC-2") != 1 {
		t.Fatal("no near miss for INC-2")
	}
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	hist := s.h
	s.h = &refusing{}
	s.l.SetMeta(ctx, "digest", "") // worked out again
	s.jobs(ctx, false)
	if near("INC-2") != 1 {
		t.Error("a failing digest lost INC-2's near miss")
	}
	x, _, err := s.nearSeen(ctx, ledger.Week(s.l.Day(week)))
	if err != nil || len(x.Incidents) != 0 {
		t.Errorf("the week isn't to be worked out again: %v %+v", err, x)
	}
	s.h = hist
	s.jobs(ctx, false)
	if x, _, _ := s.nearSeen(ctx, ledger.Week(s.l.Day(week))); x.Incidents["INC-2"] == 0 || near("INC-2") != 1 {
		t.Errorf("worked out again: %+v, %d", x, near("INC-2"))
	}
	// The digest tells only of incidents still not caught.
	r, _, _ := s.l.ForPeriod(ctx, "near_misses", week)
	var nms []verdict.NearMiss
	r.Decode(&nms)
	s.l.ReplaceResult(ctx, "near_misses", s.now(), week, week.Add(7*24*time.Hour),
		append(nms, verdict.NearMiss{Incident: "INC-1", Rule: "Late"}))
	o, _ := s.options(ctx)
	rep, err := s.digestReport(ctx, week, week.Add(7*24*time.Hour), o)
	if err != nil || len(rep.NearMisses) != 1 || rep.NearMisses[0].Incident != "INC-2" {
		t.Errorf("the digest's: %v %+v", err, rep.NearMisses)
	}
}

// refusing is a history that refuses every question.
type refusing struct{ asked atomic.Int64 }

func (h *refusing) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.asked.Add(1)
	return nil, errors.New("unavailable")
}

func TestNearByValueText(t *testing.T) {
	nm := verdict.NearMiss{Incident: "INC-1", Rule: "Latency", For: 5 * time.Minute, NewFor: 5 * time.Minute, Delay: 5 * time.Minute,
		Value: &verdict.ByValue{Op: ">", Bound: 0.9, Reached: new(0.88), New: 0.87}, After: &verdict.RuleResult{Firings: 2, Black: 1}}
	if l := nearText("en", nm); l.Text != "its value came to 0.88, short of its threshold of > 0.9: with > 0.87 it would have fired at +5m" ||
		!strings.Contains(l.Cost, "fired 2 times instead of 0, 1 of them") {
		t.Errorf("in English: %+v", l)
	}
	if l := nearText("ko", nm); l.Text != "값이 0.88까지 갔지만 임계값 > 0.9에는 못 미쳤어요. > 0.87였다면 +5m에 울렸을 거예요" {
		t.Errorf("in Korean: %s", l.Text)
	}
	x := nearOutOf(nm)
	if x.Op != ">" || x.Threshold == nil || *x.Threshold != 0.9 || *x.Reached != 0.88 || *x.NewThreshold != 0.87 || x.NewFor != 300 {
		t.Errorf("MCP: %+v", x)
	}
	if x := nearOutOf(verdict.NearMiss{For: time.Hour, NewFor: time.Minute}); x.Threshold != nil || x.Op != "" {
		t.Errorf("MCP by for: %+v", x)
	}
	// One comparison of several: by its expression, read or replayed.
	nm.Value = &verdict.ByValue{Op: ">", Bound: 0.9, Of: "errors", Reached: new(0.88), New: 0.87}
	if l := nearText("en", nm); l.Text != "the value of errors came to 0.88, short of errors > 0.9: with errors > 0.87 it would have fired at +5m" {
		t.Errorf("of several, read: %s", l.Text)
	}
	nm.Value = &verdict.ByValue{Op: "<", Bound: 10, Of: "requests", Unless: true, New: 9}
	if l := nearText("en", nm); l.Text != "in place of unless requests < 10, with unless requests < 9 it would have fired at +5m" {
		t.Errorf("of several, replayed: %s", l.Text)
	}
	if l := nearText("ko", nm); l.Text != "unless requests < 10 대신 unless requests < 9였다면 +5m에 울렸을 거예요" {
		t.Errorf("of several, replayed, in Korean: %s", l.Text)
	}
	if x := nearOutOf(nm); x.Of != "requests" || !x.Unless || x.Reached != nil || *x.NewThreshold != 9 {
		t.Errorf("MCP of several: %+v", x)
	}
}
