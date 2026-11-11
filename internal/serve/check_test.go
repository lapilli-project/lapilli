package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
)

// alertsHistory is history, and ALERTS as Prometheus would have kept it for
// the rule Errors (for 2m): the checkout alert as replayed, fifteen of the
// twenty search alerts, one for cart the replay doesn't have, and a
// Watchdog no rule given is named.
type alertsHistory struct {
	history
	none bool // ALERTS holds nothing
}

func (h *alertsHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr != alertsSeries {
		return h.history.QueryRange(ctx, expr, start, end, step)
	}
	if h.none {
		return nil, nil
	}
	run := func(labels map[string]string, from, to time.Time) prom.Series {
		s := prom.Series{Labels: map[string]string{"alertname": "Errors", "alertstate": "firing", "severity": "page"}}
		for k, v := range labels {
			s.Labels[k] = v
		}
		for t := from; !t.After(to); t = t.Add(step) {
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		return s
	}
	var out []prom.Series
	out = append(out, run(map[string]string{"service": "checkout"}, now.Add(-30*time.Hour+2*time.Minute), now.Add(-29*time.Hour)))
	for k := 0; k < 15; k++ {
		at := now.Add(-50*time.Hour + time.Duration(k)*time.Hour)
		out = append(out, run(map[string]string{"service": "search"}, at.Add(2*time.Minute), at.Add(10*time.Minute)))
	}
	out = append(out, run(map[string]string{"service": "cart"}, now.Add(-40*time.Hour), now.Add(-39*time.Hour)))
	// Before the backfill began, at noon three days ago: a day the runs
	// replayed only part of isn't checked, so this isn't counted.
	out = append(out, run(map[string]string{"service": "cart"}, now.Add(-76*time.Hour), now.Add(-75*time.Hour)))
	dog := run(nil, now.Add(-45*time.Hour), now.Add(-45*time.Hour+5*time.Minute))
	dog.Labels["alertname"] = "Watchdog"
	out = append(out, dog)
	// A name that doesn't show, as any store could hold.
	bad := run(nil, now.Add(-44*time.Hour), now.Add(-44*time.Hour+5*time.Minute))
	bad.Labels["alertname"] = "Watch\u202edog"
	out = append(out, bad)
	var kept []prom.Series
	for _, s := range out {
		if len(s.Samples) > 0 {
			kept = append(kept, s)
		}
	}
	return kept, nil
}

// The replay is set beside what really fired after the runs, a day at a
// time, over the days the runs won't replay again, and kept: each firing
// once, what differs by rule, and alerts no rule is named.
func TestCheckReplay(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.CheckReplay = true })
	s.h = &alertsHistory{}
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	to, err := s.l.CheckedTo(ctx)
	if err != nil || !to.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("checked to %s %v; want the start of today, the runs replaying the last hour again", to, err)
	}
	cs, err := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	c := cs["Errors"]
	if c.Replayed != 21 || c.Real != 17 || c.Alike != 16 || c.Off != 0 {
		t.Errorf("Errors: %+v; want 21 replayed, 17 real, 16 alike, no time apart", c)
	}
	misses, err := s.l.CheckMisses(ctx, "Errors")
	if err != nil || len(misses) != 6 {
		t.Fatalf("misses: %+v %v", misses, err)
	}
	ways := map[string]int{}
	for _, m := range misses {
		ways[m.Way+" "+m.Labels["service"]]++
	}
	if ways["replayed search"] != 5 || ways["real cart"] != 1 {
		t.Errorf("misses by way: %v", ways)
	}
	if el, _ := s.l.CheckElsewhere(ctx, now.Add(-7*24*time.Hour), now); el["Watchdog"] != 1 || len(el) != 1 {
		t.Errorf("elsewhere: %v", el)
	}
	// From the first whole day the runs replayed: the backfill began at noon.
	if first, _ := s.l.Checks(ctx, now.Add(-3*24*time.Hour), now.Add(-2*24*time.Hour)); len(first) != 0 {
		t.Errorf("a day replayed in part was checked: %+v", first)
	}
	// Again: nothing new to check, nothing counted twice.
	s.jobs(ctx, true)
	if again, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); again["Errors"] != c {
		t.Errorf("checked again: %+v, was %+v", again["Errors"], c)
	}
	if line, err := s.checkLine(ctx, now.Add(-7*24*time.Hour), now); err != nil || !strings.Contains(line, "73% alike, of 22 firings") || !strings.Contains(line, "Errors") {
		t.Errorf("the digest's line: %q", line)
	}
	// The pages say so.
	h := s.Handler()
	for path, want := range map[string][]string{
		"/rules/Errors": {"Over the last week its replay differed", "73% alike, of 22 firings; 5 only in the replay, 1 only in what really fired", "only in what really fired", "service=cart"},
		"/rules":        {"replayed differently from how it really fired"},
		"/runs":         {"Set beside ALERTS", "with no rule given by that name: Watchdog"},
		"/":             {"That week, the replay beside what really fired: 93% alike, of 15 firings."},
	} {
		_, body, _ := get(t, h, path)
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s doesn't say %q", path, w)
			}
		}
	}
}

// A store that keeps no ALERTS checks nothing, and says so, rather than
// every replayed firing looking as if it never really happened.
func TestCheckReplayNoSeries(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.CheckReplay = true })
	s.h = &alertsHistory{none: true}
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if cs, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); len(cs) != 0 {
		t.Errorf("checked against nothing: %+v", cs)
	}
	s.status.Lock()
	empty := s.status.checkEmpty
	s.status.Unlock()
	if len(empty) != 1 || empty[0] != alertsSeries {
		t.Errorf("empty series: %v", empty)
	}
}

// Rules that share an alert name, named apart in the ledger, are checked
// together under it: what really fired doesn't tell them apart, and their
// firings aren't taken for an alert no rule is named.
func TestCheckReplayShared(t *testing.T) {
	s, rulesDir := server(t, func(c *Config) { c.CheckReplay = true })
	s.h = &alertsHistory{}
	two := ruleFile + "  - name: web\n    rules:\n      - alert: Errors\n        expr: errors > 5\n        for: 2m\n        labels: {severity: page}\n"
	if err := os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	cs, err := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("checks: %+v", cs)
	}
	for name, c := range cs {
		if !c.Shared || c.Alert != "Errors" || c.Real != 17 || c.Alike != 16 {
			t.Errorf("%s: %+v", name, c)
		}
	}
	if el, _ := s.l.CheckElsewhere(ctx, now.Add(-7*24*time.Hour), now); el["Errors"] != 0 {
		t.Errorf("Errors taken for an alert no rule is named: %v", el)
	}
}

// A rule a run couldn't replay isn't checked over the days of that run, and
// what really fired of it isn't taken for an alert no rule is named.
func TestCheckReplayFailed(t *testing.T) {
	s, _ := server(t, nil)
	s.h = &alertsHistory{}
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := s.l.AddRun(ctx, ledger.Run{Kind: "hourly", Started: now, Finished: now, From: day.Add(6 * time.Hour), To: day.Add(7 * time.Hour),
		Failed: map[string]string{"Errors": "boom"}}); err != nil {
		t.Fatal(err)
	}
	s.cfg.CheckReplay = true
	if err := s.checkReplay(ctx); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s.l.Checks(ctx, day, day.Add(24*time.Hour)); len(cs) != 0 {
		t.Errorf("checked over a day it failed: %+v", cs)
	}
	if el, _ := s.l.CheckElsewhere(ctx, day, day.Add(24*time.Hour)); el["Errors"] != 0 {
		t.Errorf("Errors taken for an alert no rule is named: %v", el)
	}
	if cs, _ := s.l.Checks(ctx, day.Add(24*time.Hour), day.Add(48*time.Hour)); cs["Errors"].Either() == 0 {
		t.Errorf("not checked the day after: %+v", cs)
	}
}
