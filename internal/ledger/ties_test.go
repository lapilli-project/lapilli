package ledger

import (
	"context"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
)

// An alarm Lapilli doesn't replay, read through --alarms, and its history
// copied in under its source are one rule (docs/design/alarms.md).

const cwARN = "arn:aws:cloudwatch:eu-west-1:111122223333:alarm:"

// alarmVersion is a version holding a replayed rule file and, read under
// the source cw, CloudWatch alarms that aren't PromQL, by name, each with
// the name the version judges it under.
func alarmVersion(t *testing.T, l *Ledger, since time.Time, alarms map[string]string) {
	t.Helper()
	docs := []rules.Doc{{Source: "rules.yaml", Data: []byte("groups:\n  - name: g\n    rules:\n      - alert: Replayed\n        expr: up == 0\n")}}
	names := []string{"Replayed"}
	for _, a := range slices.Sorted(func(yield func(string) bool) {
		for k := range alarms {
			if !yield(k) {
				return
			}
		}
	}) {
		docs = append(docs, rules.Doc{Source: cwARN + a, Namespace: "cw",
			Data: []byte(`{"MetricAlarms": [{"AlarmName": "` + a + `", "AlarmArn": "` + cwARN + a + `", "Namespace": "AWS/EC2", "MetricName": "CPUUtilization"}]}`)})
		names = append(names, alarms[a])
	}
	if _, err := l.AddVersion(context.Background(), Version{Since: since, Digest: since.String(), Docs: docs, Names: names}); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, l *Ledger) []string {
	t.Helper()
	table, err := l.RuleTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range table {
		out = append(out, r.Name+" <"+r.Source+">")
	}
	slices.Sort(out)
	return out
}

func TestTiedByID(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	// The alarm's name is taken by a replayed rule, so it is judged as
	// cpu-high [cw]; its history, copied in, comes under that name, by its
	// ID, though the recipe names it cpu-high.
	if err := l.PutNote(ctx, Note{Rule: "cpu-high [cw]", Note: "the alarm's"}, "ana"); err != nil {
		t.Fatal(err)
	}
	alarmVersion(t, l, t0, map[string]string{"cpu-high": "cpu-high [cw]"})
	record(t, l, t0, t0.Add(3*time.Hour), []firing.Firing{{Rule: "Replayed", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	id := map[string]string{AlarmID: cwARN + "cpu-high", "InstanceId": "i-1"}
	putFirings(t, l, "cw", t0, t0.Add(3*time.Hour), []firing.Firing{
		{Rule: "cpu-high", Labels: id, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	want := []string{"Replayed <>", "cpu-high [cw] <cw>"}
	if got := names(t, l); !slices.Equal(got, want) {
		t.Errorf("names\n got %q\nwant %q", got, want)
	}
	// A run judging the alarm under its name, unreplayed, leaves it the
	// history's, its severity the source's.
	f := Found{From: t0.Add(3 * time.Hour), To: t0.Add(6 * time.Hour), Rules: []Rule{{Name: "cpu-high [cw]", Severity: "page", Unsupported: "a CloudWatch metric alarm isn't PromQL"},
		{Name: "Replayed"}}, Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0.Add(3 * time.Hour), To: t0.Add(6 * time.Hour)}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if got := names(t, l); !slices.Equal(got, want) {
		t.Errorf("after a run\n got %q\nwant %q", got, want)
	}
	// The next copy finds its rule, of the source's own severity.
	putFirings(t, l, "cw", t0, t0.Add(6*time.Hour), []firing.Firing{
		{Rule: "cpu-high", Labels: id, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)},
		{Rule: "cpu-high", Labels: id, Start: t0.Add(4 * time.Hour), End: t0.Add(5 * time.Hour)}})
	fs, _ := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(9 * time.Hour), Rule: "cpu-high [cw]"})
	if len(fs) != 2 || !slices.Equal(names(t, l), want) {
		t.Errorf("firings %d, names %q", len(fs), names(t, l))
	}
	ties, err := l.Ties(ctx)
	if err != nil || ties["cpu-high [cw]"].Source != "cw" || ties["cpu-high [cw]"].Since != t0.Add(time.Hour).Unix() {
		t.Errorf("ties: %+v %v", ties, err)
	}
	if n, _ := l.Notes(ctx); n["cpu-high [cw]"].Note != "the alarm's" {
		t.Errorf("the alarm's note: %+v", n)
	}
}

// A history with no alarm ID ties by name; copied in before the alarm is
// read, it is named as its source names it, then as the alarm is judged.
// A name two alarms share ties neither; an alarm with no history has none.
func TestTiedByName(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	putFirings(t, l, "cw", t0, t0.Add(3*time.Hour), []firing.Firing{
		{Rule: "disk-full", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)},
		{Rule: "Replayed", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	alarmVersion(t, l, t0, map[string]string{"disk-full": "disk-full", "quiet": "quiet"})
	want := []string{"Replayed [cw] <cw>", "disk-full <cw>"}
	if got := names(t, l); !slices.Equal(got, want) {
		t.Errorf("names\n got %q\nwant %q", got, want)
	}
	ties, _ := l.Ties(ctx)
	if ties["disk-full"].Source != "cw" || ties["quiet"].Why != UntiedNone || ties["Replayed"] != (TieState{}) {
		t.Errorf("ties: %+v", ties)
	}
	// The same name under a source twice: neither is tied by name.
	list, untied := TiesOf([]rules.Rule{
		{Name: "a", Cloud: "oci", Namespace: "oci", Source: "ocid1.alarm.oc1..1"},
		{Name: "a", Cloud: "oci", Namespace: "oci", Source: "ocid1.alarm.oc1..2"},
		{Name: "b", Cloud: "oci", Namespace: "oci", Source: "ocid1.alarm.oc1..3"},
		{Name: "c", Cloud: "oci", Namespace: "oci", Source: "ocid1.alarm.oc1..4", Expr: "up == 0"},
		{Name: "d", Cloud: "oci", Source: "file.json"},
		{Name: "p: x", Group: "p", Cloud: "google", Namespace: "g", Source: "projects/p/alertPolicies/1"},
		{Name: "p: y", Group: "p", Cloud: "google", Namespace: "g", Source: "projects/p/alertPolicies/1"},
	}, []string{"a [1]", "a [2]", "b", "c", "d", "p: x", "p: y"})
	if len(list) != 3 || list[0].Name != "" || list[1].Name != "" || list[2].Name != "b" || untied["p: x"] != UntiedPolicy {
		t.Errorf("ties %+v, untied %+v", list, untied)
	}
}

// A history whose alerts carry two alarms' IDs is neither's; an alarm two
// of its source's rules tie to has neither, unless one is under its
// name.
func TestTiedNeither(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	alarmVersion(t, l, t0, map[string]string{"cpu-high": "cpu-high", "disk": "disk"})
	putFirings(t, l, "cw", t0, t0.Add(3*time.Hour), []firing.Firing{
		{Rule: "cpu-high", Labels: map[string]string{AlarmID: cwARN + "cpu-high", "i": "1"}, Start: t0.Add(time.Hour)},
		{Rule: "cpu-high", Labels: map[string]string{AlarmID: cwARN + "disk", "i": "2"}, Start: t0.Add(time.Hour)},
		{Rule: "disk-old", Labels: map[string]string{AlarmID: cwARN + "disk"}, Start: t0.Add(time.Hour)},
		{Rule: "disk", Labels: map[string]string{AlarmID: cwARN + "disk"}, Start: t0.Add(2 * time.Hour)}})
	ties, _ := l.Ties(ctx)
	if ties["cpu-high"].Source != "" || ties["disk"].Source != "cw" {
		t.Errorf("ties: %+v", ties)
	}
	want := []string{"cpu-high [cw] <cw>", "disk <cw>", "disk-old <cw>"}
	if got := names(t, l); !slices.Equal(got, want) {
		t.Errorf("names\n got %q\nwant %q", got, want)
	}
}

// An alarm renamed in the console (OCI's and Google's display names can
// be) keeps its ID: its history, copied in under its old name, is tied to
// it by the ID its alerts carry, not by name.
func TestTiedRenamed(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	const id = "ocid1.alarm.oc1.ap-seoul-1.a1"
	putFirings(t, l, "oci", t0, t0.Add(3*time.Hour), []firing.Firing{
		{Rule: "cpu-old", Labels: map[string]string{AlarmID: id}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	docs := []rules.Doc{{Source: id, Namespace: "oci",
		Data: []byte(`{"data": [{"id": "` + id + `", "displayName": "cpu", "namespace": "oci_computeagent", "isEnabled": true, "body": ""}]}`)}}
	if _, err := l.AddVersion(ctx, Version{Since: t0, Digest: "d", Docs: docs, Names: []string{"cpu"}}); err != nil {
		t.Fatal(err)
	}
	if got := names(t, l); !slices.Equal(got, []string{"cpu <oci>"}) {
		t.Errorf("names %q", got)
	}
	if ties, _ := l.Ties(ctx); ties["cpu"].Source != "oci" {
		t.Errorf("ties: %+v", ties)
	}
}

// An Azure alert rule whose severity changed: its history is two recorded
// rules of its ID (a recorded rule is a name and a severity). The one at
// its severity now is its.
func TestTiedSeverityChanged(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	const id = "/subscriptions/s1/resourceGroups/shop/providers/Microsoft.Insights/metricAlerts/disk-full"
	docs := []rules.Doc{{Source: id, Namespace: "az",
		Data: []byte(`{"id": "` + id + `", "name": "disk-full", "type": "Microsoft.Insights/metricAlerts", "location": "global",
		 "properties": {"severity": 2, "enabled": true, "description": "", "criteria": {"odata.type": "Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria", "allOf": []}}}`)}}
	if _, err := l.AddVersion(ctx, Version{Since: t0, Digest: "d", Docs: docs, Names: []string{"disk-full"}}); err != nil {
		t.Fatal(err)
	}
	putFirings(t, l, "az", t0, t0.Add(6*time.Hour), []firing.Firing{
		{Rule: "disk-full", Labels: map[string]string{AlarmID: id, "severity": "Sev3"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)},
		{Rule: "disk-full", Labels: map[string]string{AlarmID: id, "severity": "Sev2"}, Start: t0.Add(4 * time.Hour), End: t0.Add(5 * time.Hour)}})
	ties, err := l.Ties(ctx)
	if err != nil || ties["disk-full"].Source != "az" {
		t.Fatalf("ties: %+v %v", ties, err)
	}
	fs, _ := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(6 * time.Hour), Rule: "disk-full"})
	if len(fs) != 1 || !fs[0].Start.Equal(t0.Add(4*time.Hour)) {
		t.Errorf("its history at Sev2: %+v", fs)
	}
}

// A Google policy of two conditions, one PromQL (replayed) and one not:
// the policy's alerts don't say which opened them, so the other isn't
// tied to them.
func TestTiedMixedPolicy(t *testing.T) {
	list, untied := TiesOf([]rules.Rule{
		{Name: "p: x", Group: "p", Cloud: "google", Namespace: "g", Source: "projects/p/alertPolicies/1", Expr: "up == 0"},
		{Name: "p: y", Group: "p", Cloud: "google", Namespace: "g", Source: "projects/p/alertPolicies/1"},
	}, []string{"p: x", "p: y"})
	if len(list) != 0 || untied["p: y"] != UntiedPolicy {
		t.Errorf("ties %+v, untied %+v", list, untied)
	}
}

// A Google policy's name names its project by ID in one place and by
// number in another (alerts, incidents): its history, its open alerts and
// its notifications are its by the policy alone, the project being the one
// --alarms names.
func TestTiedGoogleProjectNumber(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	const id, num = "projects/shop/alertPolicies/9", "projects/123456789012/alertPolicies/9"
	docs := []rules.Doc{{Source: id, Namespace: "gcp", Data: []byte(`{"name":"` + id + `","displayName":"Disk","enabled":true,"combiner":"OR",
	 "conditions":[{"name":"` + id + `/conditions/1","displayName":"Disk","conditionThreshold":{"filter":"metric.type=\"x\"","comparison":"COMPARISON_GT",
	 "thresholdValue":90,"duration":"300s"}}]}`)}}
	if _, err := l.AddVersion(ctx, Version{Since: t0, Digest: "d", Docs: docs, Names: []string{"Disk"}}); err != nil {
		t.Fatal(err)
	}
	putFirings(t, l, "gcp", t0, t0.Add(6*time.Hour), []firing.Firing{
		{Rule: "Disk usage", Labels: map[string]string{AlarmID: num}, Start: t0.Add(time.Hour)}})
	if ties, err := l.Ties(ctx); err != nil || ties["Disk"].Source != "gcp" {
		t.Errorf("tied: %+v %v", ties, err)
	}
	if on, err := l.FiringAlarm(ctx, "gcp", id, false); err != nil || !on {
		t.Errorf("firing, by its project's ID: %v %v", on, err)
	}
	if on, _ := l.FiringAlarm(ctx, "gcp", "projects/shop/alertPolicies/10", false); on {
		t.Error("another policy firing")
	}
	if !among(map[string]string{AlarmID: id}, map[string]string{AlarmID: num}) || among(map[string]string{AlarmID: id}, map[string]string{AlarmID: "projects/1/alertPolicies/10"}) {
		t.Error("a notification naming the policy's project by number")
	}
}
