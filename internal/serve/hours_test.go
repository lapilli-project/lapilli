package serve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
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
	team := teamReport(rep, &owners{rule: map[string]int64{}}, ledger.Team{ID: 1, Name: "shop", Labels: incident.Labels{"service": {"checkout"}}})
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
	s.jobs(ctx, true)
	count := func() int {
		rs, err := s.l.Results(ctx, "near_misses", 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs)
	}
	near := func(id string) []verdict.NearMiss {
		n, err := s.nearMisses(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n[id]
	}
	// The digest's week, and the week under way, are worked out once.
	if n := count(); n != 2 {
		t.Fatalf("%d results after the first jobs", n)
	}
	s.jobs(ctx, false)
	if n := count(); n != 2 {
		t.Errorf("worked out again with nothing new: %d", n)
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
	rep, err := s.digestReport(ctx, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), o)
	if err != nil || len(rep.NearMisses) != 1 {
		t.Errorf("the week's report: %v %+v", err, rep.NearMisses)
	}
	before := count()
	s.jobs(ctx, false)
	if count() != before {
		t.Error("worked out again with nothing new")
	}

	// INC-4, in the week under way: Late nearly caught it too.
	write(inc2 + "  - {id: INC-4, title: Search down, start: 2026-11-02T03:00:00Z, end: 2026-11-02T03:30:00Z, labels: {service: search}}\n")
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
	// Worked out before the ledger's firings reached the incident's end, it
	// is worked out again once they have, and then no more.
	week := ledger.Week(s.l.Day(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)))
	key := "near:" + strconv.FormatInt(week, 10)
	v, _ := s.l.Meta(ctx, key)
	if v == "" {
		t.Fatal("no record of how far the week was worked out")
	}
	s.l.SetMeta(ctx, key, fmt.Sprintf(`{"updated": %d, "cursor": %d}`, time.Now().Unix()+3600, time.Date(2026, 11, 2, 3, 10, 0, 0, time.UTC).Unix()))
	before = count()
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if n := count(); n != before+1 {
		t.Errorf("%d results, want %d", n, before+1)
	}
	// Taken out of the file: its week is worked out again, without it.
	write("")
	s.jobs(ctx, false)
	if n := near("INC-4"); len(n) != 0 {
		t.Errorf("INC-4 gone, still: %+v", n)
	}
}

func TestNearByValueText(t *testing.T) {
	nm := verdict.NearMiss{Incident: "INC-1", Rule: "Latency", For: 5 * time.Minute, NewFor: 5 * time.Minute, Delay: 5 * time.Minute,
		Value: &verdict.ByValue{Op: ">", Bound: 0.9, Reached: 0.88, New: 0.87}, After: &verdict.RuleResult{Firings: 2, Black: 1}}
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
}
