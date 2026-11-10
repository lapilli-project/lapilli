package serve

import (
	"context"
	"strings"
	"testing"
	"time"

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
	dog := run(nil, now.Add(-45*time.Hour), now.Add(-45*time.Hour+5*time.Minute))
	dog.Labels["alertname"] = "Watchdog"
	out = append(out, dog)
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
	if el, _ := s.l.CheckElsewhere(ctx, now.Add(-7*24*time.Hour), now); el["Watchdog"] != 1 {
		t.Errorf("elsewhere: %v", el)
	}
	// Again: nothing new to check, nothing counted twice.
	s.jobs(ctx, true)
	if again, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); again["Errors"] != c {
		t.Errorf("checked again: %+v, was %+v", again["Errors"], c)
	}
	if line := s.checkLine(cs); !strings.Contains(line, "73% alike, of 22 firings") || !strings.Contains(line, "Errors") {
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
