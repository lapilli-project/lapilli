package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// --firings beside rules replayed: a cloud's own alarm, which can't be
// replayed, judged in the same report as the rules that are.

// recordedWorld is a rule replayed, ProbeDown, that fires over INC-1 on day
// 1; an alarm, StorageFull, recorded in a file, that fires over INC-2 on day
// 2, and over nothing on day 3; and the two incidents.
func recordedWorld(t *testing.T, firingsNames ...string) (signalHistory, []string) {
	t.Helper()
	h := signalHistory{series: map[string][]on{
		probeExpr: {{lbl("service", "checkout"), at(1, 10, 0), at(1, 10, 30)}},
	}}
	rulesFile := write(t, "rules.yaml", `groups:
  - name: probes
    rules:
      - alert: ProbeDown
        expr: `+probeExpr+`
        labels: {severity: critical}
`)
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-1, title: Checkout down, start: `+ts(at(1, 10, 5))+`, end: `+ts(at(1, 10, 30))+`}
  - {id: INC-2, title: Disk full, start: `+ts(at(2, 3, 5))+`, end: `+ts(at(2, 4, 0))+`}
`)
	name := "StorageFull"
	if len(firingsNames) > 0 {
		name = firingsNames[0]
	}
	firings := firingsFile(t, [][4]string{
		{name, `{"severity":"warning"}`, ts(at(2, 3, 10)), ts(at(2, 3, 40))},
		{name, `{"severity":"warning"}`, ts(at(3, 12, 0)), ts(at(3, 12, 10))},
	})
	return h, []string{"verdict", "--rules", rulesFile, "--firings", firings, "--incidents", incidents, "--prometheus", "http://p",
		"--from", ts(sigBase), "--to", ts(sigBase.Add(7 * 24 * time.Hour)), "--tz", "UTC"}
}

func runRecorded(t *testing.T, h signalHistory, args ...string) (int, string, string) {
	t.Helper()
	newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
	now = func() time.Time { return sigBase.Add(8 * 24 * time.Hour) }
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// An incident only a recorded firing caught is caught, not missed; the
// rule is the report's, marked as recorded, and judged as any is; it isn't
// one that couldn't be replayed.
func TestRecordedFiringsJudgedWithReplayed(t *testing.T) {
	h, args := recordedWorld(t)
	code, out, errs := runRecorded(t, h, append(args, "--json")...)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	var rep verdict.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	by := map[string]verdict.RuleResult{}
	for _, r := range rep.Rules {
		by[r.Rule] = r
	}
	probe, disk := by["ProbeDown"], by["StorageFull"]
	if !probe.Replayed || probe.Source != "" || probe.Firings != 1 || probe.White != 1 {
		t.Errorf("ProbeDown: %+v", probe)
	}
	if disk.Replayed || disk.Source != "firings" || disk.Unsupported == "" || disk.Firings != 2 || disk.White != 1 || disk.Black != 1 || disk.Incidents != 1 {
		t.Errorf("StorageFull: %+v", disk)
	}
	for _, in := range rep.Incidents {
		if len(in.CaughtBy) != 1 || in.NotKnown {
			t.Errorf("%s: caught by %v, not known %v", in.ID, in.CaughtBy, in.NotKnown)
		}
	}
	if rep.Incidents[1].CaughtBy[0].Rule != "StorageFull" {
		t.Errorf("INC-2 caught by %v", rep.Incidents[1].CaughtBy)
	}
	if len(rep.Unreplayed) != 0 {
		t.Errorf("unreplayed: %v", rep.Unreplayed)
	}

	_, text, _ := runRecorded(t, h, args...)
	for _, want := range []string{"Judged from what was recorded, not replayed, 1 rule", "StorageFull"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report doesn't say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Could not replay") {
		t.Errorf("a recorded rule is listed as one that couldn't be replayed:\n%s", text)
	}
	_, prom, _ := runRecorded(t, h, append(args, "--metrics")...)
	if !strings.Contains(prom, `lapilli_rule_firings{rule="StorageFull"`) {
		t.Errorf("--metrics leaves out the recorded rule:\n%s", prom)
	}
	_, csv, _ := runRecorded(t, h, append(args, "--csv")...)
	if !strings.Contains(csv, "StorageFull") {
		t.Errorf("--csv leaves out the recorded rule:\n%s", csv)
	}
	_, red, _ := runRecorded(t, h, append(args, "--redact", "--json")...)
	if strings.Contains(red, "StorageFull") || !strings.Contains(red, `"source"`) {
		t.Errorf("--redact: %s", red)
	}
}

// A name both a rule replayed and the firings have is refused, and
// --firings-prefix keeps them apart.
func TestRecordedFiringsSharedName(t *testing.T) {
	h, args := recordedWorld(t, "ProbeDown")
	code, out, errs := runRecorded(t, h, args...)
	if code != 2 || out != "" || !strings.Contains(errs, "--firings: ProbeDown is also a rule replayed: a rule's firings come from one place") {
		t.Fatalf("exit %d, stdout %q\n%s", code, out, errs)
	}
	code, out, errs = runRecorded(t, h, append(args, "--firings-prefix", "cloud/", "--json")...)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	var rep verdict.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, r := range rep.Rules {
		names[r.Rule] = r.Source
	}
	if len(names) != 2 || names["ProbeDown"] != "" || names["cloud/ProbeDown"] != "firings" {
		t.Errorf("rules: %v", names)
	}
}

// Many names: five, then how many more.
func TestRecordedFiringsClashSaid(t *testing.T) {
	got := clashSaid([]string{"A", "B", "C", "D", "E", "F", "G"})
	if !strings.HasPrefix(got, "--firings: A, B, C, D, E and 2 more are also rules replayed: ") {
		t.Errorf("%s", got)
	}
}

// What the combination doesn't take is said so.
func TestRecordedFiringsRefused(t *testing.T) {
	h, args := recordedWorld(t)
	inPlace := write(t, "before.yaml", "groups: []\n")
	for _, c := range []struct {
		extra []string
		want  string
	}{
		{[]string{"--compare", inPlace}, "leave out --firings"},
		{[]string{"--alerts"}, "--alerts and --firings"},
	} {
		code, _, errs := runRecorded(t, h, append(append([]string{}, args...), c.extra...)...)
		if code != 2 || !strings.Contains(errs, c.want) {
			t.Errorf("%v: exit %d\n%s", c.extra, code, errs)
		}
	}
	code, _, errs := runRecorded(t, h, "verdict", "--rules", args[2], "--firings-prefix", "x", "--incidents", args[6], "--prometheus", "http://p")
	if code != 2 || !strings.Contains(errs, "--firings-prefix") {
		t.Errorf("a prefix with no firings: exit %d\n%s", code, errs)
	}
	code, _, errs = runRecorded(t, h, "verdict", "--firings", args[4], "--firings-prefix", "x", "--incidents", args[6], "--prometheus", "http://p")
	if code != 2 || !strings.Contains(errs, "--firings-prefix goes with rules replayed beside --firings: alone, the firings' names can't clash") {
		t.Errorf("a prefix with firings alone: exit %d\n%s", code, errs)
	}
}

// Recorded rules aren't counted among those replayed: a run whose replays
// all failed exits 1 though recorded firings were judged (0 with
// --fail-nothing-replayed=false), and --fail-unreplayed counts only the
// rules replayed.
func TestRecordedFiringsAndFailedReplays(t *testing.T) {
	_, args := recordedWorld(t)
	h := signalHistory{} // nothing answers: the replay fails
	code, _, errs := runRecorded(t, h, args...)
	if code != 1 || !strings.Contains(errs, "no rule was replayed: the replays of 1 rules failed") || !strings.Contains(errs, "judges only what was recorded") {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	code, out, errs := runRecorded(t, h, append(args, "--json", "--fail-nothing-replayed=false")...)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	var rep verdict.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Unreplayed) != 1 || rep.Unreplayed[0].Rule != "ProbeDown" || !rep.Unreplayed[0].Failed {
		t.Fatalf("unreplayed: %+v", rep.Unreplayed)
	}
	code, _, errs = runRecorded(t, h, append(args, "--fail-nothing-replayed=false", "--fail-unreplayed")...)
	if code != 1 || !strings.Contains(errs, "1 of 1 rules couldn't be replayed") {
		t.Errorf("--fail-unreplayed: exit %d\n%s", code, errs)
	}
}

// --rule keeping only the rules recorded: nothing is replayed, and what was
// recorded is judged. Keeping none at all is still no rules to replay.
func TestRecordedFiringsOnlyKeptByRule(t *testing.T) {
	h, args := recordedWorld(t, "cloud/StorageFull")
	args = append(args, "--rule", "^cloud/", "--json")
	code, out, errs := runRecorded(t, h, args...)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	var rep verdict.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Rules) != 1 || rep.Rules[0].Rule != "cloud/StorageFull" || !rep.Rules[0].Recorded() || rep.Rules[0].Firings != 2 {
		t.Errorf("rules: %+v", rep.Rules)
	}
	code, _, errs = runRecorded(t, h, append(args, "--rule", "^nothing$")...)
	if code != 1 || !strings.Contains(errs, "no alert rules to replay") {
		t.Errorf("none kept: exit %d\n%s", code, errs)
	}
}

// With --firings-prefix, an incident's alerts name a recorded rule by the
// name the report gives it, the prefix too: the tool's own name for the
// alert (X) doesn't link to cloud/X.
func TestRecordedFiringsPrefixLinked(t *testing.T) {
	h, args := recordedWorld(t)
	args = append(args, "--firings-prefix", "cloud/", "--linked-only", "--json")
	for _, c := range []struct {
		alert  string
		caught bool
	}{{"cloud/StorageFull", true}, {"StorageFull", false}} {
		args[6] = write(t, "incidents.yaml", `incidents:
  - id: INC-2
    title: Disk full
    start: `+ts(at(2, 3, 5))+`
    end: `+ts(at(2, 4, 0))+`
    alerts: [{rule: `+c.alert+`}]
`)
		code, out, errs := runRecorded(t, h, args...)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", c.alert, code, errs)
		}
		var rep verdict.Report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatal(err)
		}
		if got := len(rep.Incidents[0].CaughtBy) == 1; got != c.caught {
			t.Errorf("alert %s: caught by %v", c.alert, rep.Incidents[0].CaughtBy)
		}
	}
}
