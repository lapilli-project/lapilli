package verdict

import (
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
)

func at(h int) time.Time { return time.Date(2026, 11, 2, h, 0, 0, 0, time.UTC) }

// A cloud's alarm history ties to the replayed alarm it is of: by its ID,
// a Google policy's project named either way and an Azure ID in either
// case; by name where the alarm's ID isn't known; never to an alarm it
// could be of twice, nor by name to one whose ID says it is another's.
func TestAlarmHistory(t *testing.T) {
	known := []rules.Rule{
		{Name: "cpu", Expr: "x > 1", Cloud: "cloudwatch", Namespace: "cw", Source: "arn:aws:cloudwatch:eu-west-1:1:alarm:cpu"},
		{Name: "latency", Group: "Checkout latency", Expr: "y > 1", Cloud: "google", Namespace: "gc", Source: "projects/shop-prod/alertPolicies/42"},
		{Name: "errors", Expr: "z > 1", Cloud: "azure", Namespace: "az", Source: "/subscriptions/S/resourceGroups/RG/providers/Microsoft.Insights/metricAlerts/errors"},
		{Name: "disk", Expr: "d > 1", Cloud: "cloudwatch"},                                             // from a file: no ID
		{Name: "twin", Expr: "a > 1", Cloud: "google"}, {Name: "twin", Expr: "b > 1", Cloud: "google"}, // a name two share
		{Name: "off", Expr: "o > 1", Cloud: "cloudwatch", Unsupported: rules.Disabled},
		{Name: "rule", Expr: "r > 1"}, // not a cloud's
	}
	recorded := []firing.Firing{
		{Rule: "cpu", Labels: map[string]string{"alarm_id": "arn:aws:cloudwatch:eu-west-1:1:alarm:cpu"}, Start: at(1)},
		{Rule: "Checkout latency", Labels: map[string]string{"alarm_id": "projects/123456/alertPolicies/42", "pod": "a"}, Start: at(2)},
		{Rule: "errors", Labels: map[string]string{"alarm_id": "/subscriptions/S/resourcegroups/rg/providers/microsoft.insights/metricalerts/errors"}, Start: at(3)},
		{Rule: "disk", Labels: map[string]string{"alarm_id": "arn:aws:cloudwatch:eu-west-1:1:alarm:disk"}, Start: at(4)},
		{Rule: "cpu", Labels: map[string]string{"alarm_id": "arn:aws:cloudwatch:us-east-1:2:alarm:cpu"}, Start: at(5)}, // another account's cpu
		{Rule: "twin", Start: at(6)},
		{Rule: "off", Start: at(7)},
		{Rule: "rule", Start: at(8)},
		{Rule: "Checkout latency", Start: at(9)}, // by the policy's name
	}
	got := AlarmHistory(known, recorded)
	want := map[time.Time]string{at(1): "cpu", at(2): "latency", at(3): "errors", at(4): "disk", at(9): "latency"}
	if len(got) != len(want) {
		t.Fatalf("tied %+v", got)
	}
	for _, f := range got {
		if want[f.Start] != f.Rule {
			t.Errorf("%s tied to %q, want %q", f.Start, f.Rule, want[f.Start])
		}
		if _, ok := f.Labels[AlarmIDLabel]; ok {
			t.Errorf("%s keeps its alarm_id", f.Start)
		}
	}
	if got[1].Labels["pod"] != "a" {
		t.Errorf("labels lost: %v", got[1].Labels)
	}
	if recorded[0].Labels["alarm_id"] == "" {
		t.Error("the recorded firings were changed")
	}
}

// CloudWatch keeps an alarm's history as a whole: its replayed firings are
// put together, one while any series fires.
func TestWhole(t *testing.T) {
	fs := []firing.Firing{
		{Rule: "cpu", Labels: map[string]string{"i": "a"}, Start: at(1), End: at(3)},
		{Rule: "cpu", Labels: map[string]string{"i": "b"}, Start: at(2), End: at(4)},
		{Rule: "cpu", Labels: map[string]string{"i": "c"}, Start: at(4), End: at(5)}, // touching: one
		{Rule: "cpu", Labels: map[string]string{"i": "d"}, Start: at(7), End: at(8)},
		{Rule: "cpu", Labels: map[string]string{"i": "e"}, Start: at(9)}, // still on
		{Rule: "cpu", Labels: map[string]string{"i": "f"}, Start: at(10), End: at(11)},
		{Rule: "other", Labels: map[string]string{"i": "a"}, Start: at(1), End: at(3)},
		{Rule: "other", Labels: map[string]string{"i": "b"}, Start: at(2), End: at(4)},
	}
	got := Whole(fs, map[string]bool{"cpu": true})
	var cpu []firing.Firing
	others := 0
	for _, f := range got {
		if f.Rule == "cpu" {
			cpu = append(cpu, f)
			if len(f.Labels) != 0 {
				t.Errorf("a whole alarm's firing with labels %v", f.Labels)
			}
		} else {
			others++
		}
	}
	want := [][2]time.Time{{at(1), at(5)}, {at(7), at(8)}, {at(9), {}}}
	if len(cpu) != len(want) || others != 2 {
		t.Fatalf("%+v", got)
	}
	for i, w := range want {
		if !cpu[i].Start.Equal(w[0]) || !cpu[i].End.Equal(w[1]) {
			t.Errorf("firing %d: %s to %s, want %s to %s", i, cpu[i].Start, cpu[i].End, w[0], w[1])
		}
	}
	if w := WholeAlarms([]rules.Rule{{Name: "cpu", Expr: "x", Cloud: "cloudwatch"}, {Name: "g", Expr: "y", Cloud: "google"}}); !w["cpu"] || w["g"] {
		t.Errorf("whole alarms %v", w)
	}
}
