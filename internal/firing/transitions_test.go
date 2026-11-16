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
	ch := func(rule string, l map[string]string, at time.Time, from, to string) Transition {
		return Transition{Rule: rule, Labels: l, At: at, From: from, To: to}
	}
	ts := []Transition{
		// One that alerts, recovers (keep_firing_for) and resolves: one firing.
		ch("Up", pod("a"), at(1), "Normal", "Pending"),
		ch("Up", pod("a"), at(2), "Pending", "Alerting"),
		ch("Up", pod("a"), at(5), "Alerting", "Recovering"),
		ch("Up", pod("a"), at(6), "Recovering", "Alerting"),
		ch("Up", pod("a"), at(7), "Alerting", "Normal (MissingSeries)"),
		// Another alert of the rule, its labels apart, firing since before
		// the read and still firing at its end.
		ch("Up", pod("b"), at(3), "Alerting", "Recovering"),
		// No data: a firing of its own, marked so; Alerting (NoData) is
		// alerting.
		ch("Up", map[string]string{"ref_id": "A"}, at(4), "Normal", "NoData"),
		ch("Up", map[string]string{"ref_id": "A"}, at(8), "NoData", "Alerting (NoData)"),
		ch("Up", map[string]string{"ref_id": "A"}, at(9), "Alerting (NoData)", "Normal"),
		// An error isn't a firing, and ends one.
		ch("Errs", nil, at(1), "Normal", "Alerting"),
		ch("Errs", nil, at(2), "Alerting", "Error"),
		ch("Errs", nil, at(3), "Error", "Normal"),
		// Over by the start of the read: no firing of no length.
		ch("Gone", nil, at(0), "Alerting", "Normal"),
	}
	got, unsure := FromTransitions(ts, at(0))
	if len(unsure) != 0 {
		t.Errorf("unsure: %v", unsure)
	}
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

// Changes that don't follow on from each other, from a state the one
// before didn't leave, say the history is missing some, or tells two
// alerts apart by nothing it keeps: the rule is unsure. Those Grafana
// leaves out don't change whether an alert fires, and aren't taken so.
func TestFromTransitionsUnsure(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	ch := func(rule string, at time.Time, from, to string) Transition {
		return Transition{Rule: rule, Labels: map[string]string{"pod": "a"}, At: at, From: from, To: to}
	}
	ts := []Transition{
		// Two pods with one label set: each Normal to Alerting at once.
		ch("Merged", at(1), "Normal", "Alerting"),
		ch("Merged", at(1), "Normal", "Alerting"),
		// A change missing: Normal to Alerting, then Pending to Normal.
		ch("Gap", at(1), "Normal", "Alerting"),
		ch("Gap", at(3), "Pending", "Normal"),
		// What Grafana leaves out: Normal (NoData) to Normal, and into
		// KeepLast.
		ch("Fine", at(1), "Normal (NoData)", "Pending"),
		ch("Fine", at(2), "Pending", "Alerting"),
		ch("Fine", at(3), "Alerting (KeepLast)", "Recovering"),
		ch("Fine", at(4), "Recovering", "Normal"),
	}
	fs, unsure := FromTransitions(ts, at(0))
	if strings.Join(unsure, ",") != "Gap,Merged" {
		t.Errorf("unsure: %v", unsure)
	}
	// Firings of one alert share its labels.
	var fine []Firing
	for _, f := range fs {
		if f.Rule == "Fine" {
			fine = append(fine, f)
		}
	}
	if len(fine) != 1 || !fine[0].Start.Equal(at(2)) || !fine[0].End.Equal(at(4)) {
		t.Errorf("Fine: %+v", fine)
	}
	// Keyed alerts apart, whatever their labels.
	k := []Transition{{Rule: "K", Key: "1", At: at(1), From: "Normal", To: "Alerting"}, {Rule: "K", Key: "2", At: at(1), From: "Normal", To: "Alerting"}}
	if fs, unsure := FromTransitions(k, at(0)); len(fs) != 2 || len(unsure) != 0 {
		t.Errorf("keyed: %+v %v", fs, unsure)
	}
}
