package firing

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// FromTransitions reads Grafana's changes of state as GRAFANA_ALERTS has
// them: Alerting and Recovering one firing, NoData one of its own, Error
// none, a reason after a state as the state, each alert by its labels.
func TestFromTransitions(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	pod := func(p string) map[string]string { return map[string]string{"pod": p} }
	ts := []Transition{
		// One that alerts, recovers (keep_firing_for) and resolves: one firing.
		{"Up", pod("a"), at(1), "Normal", "Pending"},
		{"Up", pod("a"), at(2), "Pending", "Alerting"},
		{"Up", pod("a"), at(5), "Alerting", "Recovering"},
		{"Up", pod("a"), at(6), "Recovering", "Alerting"},
		{"Up", pod("a"), at(7), "Alerting", "Normal (MissingSeries)"},
		// Another alert of the rule, its labels apart, firing since before
		// the read and still firing at its end.
		{"Up", pod("b"), at(3), "Alerting", "Recovering"},
		// No data: a firing of its own, marked so; Alerting (NoData) is
		// alerting.
		{"Up", map[string]string{"ref_id": "A"}, at(4), "Normal", "NoData"},
		{"Up", map[string]string{"ref_id": "A"}, at(8), "NoData", "Alerting (NoData)"},
		{"Up", map[string]string{"ref_id": "A"}, at(9), "Alerting (NoData)", "Normal"},
		// An error isn't a firing, and ends one.
		{"Errs", nil, at(1), "Normal", "Alerting"},
		{"Errs", nil, at(2), "Alerting", "Error"},
		{"Errs", nil, at(3), "Error", "Normal"},
		// Over by the start of the read: no firing of no length.
		{"Gone", nil, at(0), "Alerting", "Normal"},
	}
	got := FromTransitions(ts, at(0))
	var lines []string
	for _, f := range got {
		end := "on"
		if !f.End.IsZero() {
			end = f.End.Format("15:04")
		}
		lines = append(lines, fmt.Sprintf("%s %v %s-%s", f.Rule, f.Labels, f.Start.Format("15:04"), end))
	}
	want := []string{
		"Up map[pod:b] 07:00-on",
		"Errs map[] 07:01-07:02",
		"Up map[pod:a] 07:02-07:07",
		"Up map[grafana_alertstate:nodata ref_id:A] 07:04-07:08",
		"Up map[ref_id:A] 07:08-07:09",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	// The changes given aren't changed.
	if ts[6].Labels["grafana_alertstate"] != "" {
		t.Error("a change's labels were written to")
	}
}

func TestGrafanaState(t *testing.T) {
	for s, want := range map[string]bool{"Normal": true, "Normal (MissingSeries)": true, "Alerting (NoData, KeepLast)": true, "Pending (NoData)": true,
		"Error": true, "Recovering": true, "firing": false, "": false, "Alerting(x)": false} {
		if GrafanaState(s) != want {
			t.Errorf("%q: %v", s, !want)
		}
	}
}
