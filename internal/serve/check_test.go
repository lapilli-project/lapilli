package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/alarmtest"
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

// A CloudWatch PromQL alarm, read with --alarms and replayed, keeps no
// series of what it fired: its history, copied in under its source, stands
// in, its replay first put together as CloudWatch keeps the alarm, as a
// whole. The check waits for a source whose copies are behind, two days
// at most; one never copied in holds nothing up.
func TestCheckReplayCloudAlarm(t *testing.T) {
	ctx := context.Background()
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "errors-promql", "EvaluationInterval": 60,
		"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": "errors > 1", "PendingPeriod": 0}}}, "OK", nil)
	cw.Add(map[string]any{"AlarmName": "busy-promql", "EvaluationInterval": 60,
		"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": "busy > 1", "PendingPeriod": 0}}}, "OK", nil)
	s := alarmsServer(t, "cloudwatch-prod", "https://monitoring.eu-west-1.amazonaws.com", cw, nil)
	arn := cw.ARN("errors-promql")
	// As the alarm really fired, the alarm as a whole: twenty of the
	// replay's twenty-one, a minute later.
	var lines []string
	whole := func(from, to time.Time) {
		lines = append(lines, `{"rule":"errors-promql","labels":{"alarm_id":"`+arn+`"},"start":"`+from.Add(time.Minute).Format(time.RFC3339)+`","end":"`+to.Format(time.RFC3339)+`"}`)
	}
	whole(now.Add(-30*time.Hour), now.Add(-29*time.Hour))
	for k := 1; k < 20; k++ {
		at := now.Add(-50*time.Hour + time.Duration(k)*time.Hour)
		whole(at, at.Add(10*time.Minute))
	}
	// Copied in up to midnight of 2 November: the check of 1 November
	// waits for the rest, as the job copying it is behind.
	early := []string{}
	for _, l := range lines {
		if strings.Contains(l, `"start":"2026-11-01`) {
			early = append(early, l)
		}
	}
	copyFirings(t, s, "cloudwatch-prod", "2026-10-31T00:00:00Z", "2026-11-02T00:00:00Z", strings.Join(early, "\n")+"\n")
	s.cfg.CheckReplay = true
	s.jobs(ctx, true)
	if to, err := s.l.CheckedTo(ctx); err != nil || !to.IsZero() {
		t.Fatalf("checked to %s with the source behind: %v", to, err)
	}
	// The other alarm's two series, one firing as CloudWatch keeps it.
	lines = append(lines, `{"rule":"busy-promql","labels":{"alarm_id":"`+cw.ARN("busy-promql")+`"},"start":"`+
		now.Add(-30*time.Hour+time.Minute).Format(time.RFC3339)+`","end":"`+now.Add(-28*time.Hour).Format(time.RFC3339)+`"}`)
	copyFirings(t, s, "cloudwatch-prod", "2026-10-31T00:00:00Z", "2026-11-03T12:00:00Z", strings.Join(lines, "\n")+"\n")
	s.jobs(ctx, true)
	to, err := s.l.CheckedTo(ctx)
	if err != nil || !to.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("checked to %s %v", to, err)
	}
	cs, err := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if c := cs["errors-promql"]; c.Replayed != 21 || c.Real != 20 || c.Alike != 20 {
		t.Errorf("the alarm: %+v; want 21 replayed, 20 real, 20 alike", c)
	}
	if c := cs["busy-promql"]; c.Replayed != 1 || c.Real != 1 || c.Alike != 1 {
		t.Errorf("the alarm of two series: %+v; want one firing, alike", c)
	}
	if el, _ := s.l.CheckElsewhere(ctx, now.Add(-7*24*time.Hour), now); len(el) != 0 {
		t.Errorf("its history taken for alerts no rule is named: %v", el)
	}
	for _, e := range cw.Errs {
		t.Error(e)
	}
}

// A day the copies of a source don't cover isn't checked for its alarms:
// what really fired then isn't known, so the replay's firings aren't taken
// as only the replay's.
func TestCheckReplayCloudAlarmGap(t *testing.T) {
	ctx := context.Background()
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "errors-promql", "EvaluationInterval": 60,
		"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": "errors > 1", "PendingPeriod": 0}}}, "OK", nil)
	s := alarmsServer(t, "cloudwatch-prod", "https://monitoring.eu-west-1.amazonaws.com", cw, nil)
	copyFirings(t, s, "cloudwatch-prod", "2026-11-02T12:00:00Z", "2026-11-03T12:00:00Z", "")
	s.cfg.CheckReplay = true
	s.jobs(ctx, true)
	if to, err := s.l.CheckedTo(ctx); err != nil || !to.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("checked to %s %v", to, err)
	}
	cs, err := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := cs["errors-promql"]; ok && c.Replayed+c.Real > 0 {
		t.Errorf("checked over days its copies don't cover: %+v", c)
	}
}

// A day whose check waited for a cloud's alarms' history to be copied in
// is told of when it is checked, as a day just checked is: the wait
// (two days) is longer than how old a day told of may be, and a source
// behind would otherwise stop every rule's notices of differing (M12).
func TestCheckReplayHeldDayTold(t *testing.T) {
	ctx := context.Background()
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "errors-promql", "EvaluationInterval": 60,
		"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": "errors > 1", "PendingPeriod": 0}}}, "OK", nil)
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example.com/lapilli"), 0o600)
	s := alarmsServer(t, "cloudwatch-prod", "https://monitoring.eu-west-1.amazonaws.com", cw, func(c *Config) { c.NotifyWebhook = hook })
	// Copied in up to midnight of 1 November only: the check of 1
	// November waits.
	copyFirings(t, s, "cloudwatch-prod", "2026-10-31T00:00:00Z", "2026-11-01T00:00:00Z", "")
	s.cfg.CheckReplay = true
	s.jobs(ctx, true)
	if to, _ := s.l.CheckedTo(ctx); !to.IsZero() {
		t.Fatalf("checked to %s with the source behind", to)
	}
	// The rest copied in, with three of the alarm's fourteen firings of
	// 1 November: it differs. Two days and a hour later, the day is checked.
	arn := cw.ARN("errors-promql")
	var lines []string
	for k := range 3 {
		at := now.Add(-50*time.Hour + time.Duration(k)*time.Hour)
		lines = append(lines, `{"rule":"errors-promql","labels":{"alarm_id":"`+arn+`"},"start":"`+at.Format(time.RFC3339)+`","end":"`+at.Add(10*time.Minute).Format(time.RFC3339)+`"}`)
	}
	copyFirings(t, s, "cloudwatch-prod", "2026-10-31T00:00:00Z", "2026-11-03T12:00:00Z", strings.Join(lines, "\n")+"\n")
	s.now = func() time.Time { return time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC) }
	s.jobs(ctx, true)
	ns, err := s.l.Due(ctx, "webhook", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 1000)
	if err != nil {
		t.Fatal(err)
	}
	told := false
	for _, n := range ns {
		var b struct{ To time.Time }
		json.Unmarshal(n.Body, &b)
		if n.Event == "replay.differs" && strings.Contains(string(n.Body), "errors-promql") && b.To.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
			told = true
		}
	}
	if !told {
		t.Errorf("the day held for the source wasn't told of: %d notices", len(ns))
		for _, n := range ns {
			t.Log(n.Event, string(n.Body))
		}
	}
}
