package scenario

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/scenario/scenariotest"
	"lapilli.dev/internal/verdict"
)

// What holds promtool's memory, and its children: a cap set where one can
// be (Linux), through Lapilli started again, which keeps every other guard
// (no shell, the same arguments, the emptied environment); a process group
// killed whole at the time limit.

// Guard: capped, promtool is started through the memory cap's wrapper,
// which sets RLIMIT_DATA to the cap, and RLIMIT_FSIZE to MaxFile, and
// gives promtool what it is given
// uncapped, nothing more: test rules test.yaml, TMPDIR alone. Past the cap,
// a run that takes memory stops, and is unrun, saying so. Uncapped, no
// limit is set. Skipped where no cap can be set (macOS). The stand-in is
// this test binary, built with cgo: each of its threads maps 8 MiB of
// stack, which RLIMIT_DATA counts, so it starts under no less than about
// 160 MiB (on four CPUs), where promtool's release, built without, starts
// under 64 MiB.
func TestRunMemoryCap(t *testing.T) {
	if !CanCap {
		t.Skip("no memory cap on " + runtime.GOOS)
	}
	t.Setenv("LAPILLI_TEST_SECRET", "s3cret-value")
	link := scenariotest.Install(t)
	const cap = 512 << 20
	p := Promtool{Path: link, Memory: cap}
	r := run(t, p, "see")
	if r.State != Failed || !strings.Contains(r.Said, `args ["test" "rules" "test.yaml"]`) || !strings.Contains(r.Said, `env ["TMPDIR=`) ||
		strings.Contains(r.Said, "s3cret") || strings.Contains(r.Said, capPath) || !strings.Contains(r.Said, "rlimit_data "+strconv.Itoa(cap)+"\n") ||
		!strings.Contains(r.Said, "rlimit_fsize "+strconv.Itoa(MaxFile)+"\n") {
		t.Errorf("capped, promtool was given:\n%s", r.Said)
	}
	if r := run(t, Promtool{Path: link}, "see"); strings.Contains(r.Said, "rlimit_data "+strconv.Itoa(cap)+"\n") {
		t.Errorf("uncapped, a cap set:\n%s", r.Said)
	}
	if r := run(t, p, "hog"); r.State != Unrun || r.Why != "promtool took more memory than it may (512 MiB)" {
		t.Errorf("past the cap: %+v", r)
	}
	if r := run(t, p, "pass"); r.State != Passed {
		t.Errorf("under the cap: %+v", r)
	}
	if r := run(t, Promtool{Path: filepath.Join(t.TempDir(), "none"), Memory: cap}, "pass"); r.State != Unrun || r.Why != "promtool couldn't be started" {
		t.Errorf("none, capped: %+v", r)
	}
}

// With a real promtool on PATH, a rule whose summary's template expands
// to gigabytes at each evaluation, beside the rule a scenario tests in its
// group (absent(up) fires in promtool's empty storage, never in
// production), is stopped at the cap, unrun, and Lapilli's own memory
// doesn't grow with it. Skipped without promtool, or where no cap can be
// set.
func TestRealPromtoolMemoryCap(t *testing.T) {
	if !CanCap {
		t.Skip("no memory cap on " + runtime.GOOS)
	}
	path, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("no promtool on PATH")
	}
	tpl := `{{define "l0"}}` + strings.Repeat("x", 64) + `{{end}}`
	for i := 1; i <= 7; i++ {
		tpl += fmt.Sprintf(`{{define "l%d"}}`, i) + strings.Repeat(fmt.Sprintf(`{{template "l%d"}}`, i-1), 10) + `{{end}}`
	}
	tpl += `{{template "l7"}}`
	alerts := []rules.Rule{
		{Name: "Victim", Group: "g", Source: "alerts/g.yaml", Expr: "foo > 0", Prometheus: true},
		{Name: "Bomb", Group: "g", Source: "alerts/g.yaml", Expr: "foo > 0 and on() absent(up)", Prometheus: true, Says: rules.Says{Summary: tpl}},
	}
	g, err := Group(alerts, nil)
	if err != nil {
		t.Fatal(err)
	}
	test, err := Rewrite([]byte("tests:\n- interval: 1m\n  input_series:\n  - series: foo\n    values: 1x5\n  alert_rule_test:\n  - eval_time: 2m\n    alertname: Victim\n    exp_alerts:\n    - {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := Promtool{Path: path, Memory: 256 << 20}.Run(context.Background(), g, test)
	runtime.GC()
	runtime.ReadMemStats(&after)
	if r.State != Unrun || !strings.Contains(r.Why, "more memory than it may") {
		t.Errorf("the template: %+v", r)
	}
	if grew := int64(after.HeapInuse) - int64(before.HeapInuse); grew > 16<<20 {
		t.Errorf("Lapilli's own heap grew by %d bytes", grew)
	}
}

// Guard: a run is a process group of its own, killed whole when it is
// stopped (at its time limit, or as the round's time ends): a --promtool
// that is a wrapper starting a child leaves none running.
func TestRunKillsItsGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "promtool")
	os.WriteFile(script, []byte("#!/bin/sh\n/bin/sleep 600 &\necho $! > '"+pidFile+".new'\nmv '"+pidFile+".new' '"+pidFile+"'\nwait\n"), 0o755)
	alerts, recs := shop()
	g, _ := Group(alerts, recs)
	test, _ := Rewrite([]byte(scenariotest.Test("pass")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- Promtool{Path: script}.Run(ctx, g, test) }()
	// How long these take isn't the test's; a deadline only ends one that
	// would never.
	deadline := time.Now().Add(time.Minute)
	var pid string
	for pid == "" {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid = strings.TrimSpace(string(b))
		} else if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the wrapper never started its child: %+v", <-done)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if r := <-done; r.State != Unrun {
		t.Fatalf("%+v", r)
	}
	// Its child goes with it: gone, or a zombie its new parent reaps.
	for running(pid) {
		if time.Now().After(deadline) {
			exec.Command("kill", "-9", pid).Run()
			t.Fatalf("the wrapper's child %s outlived the run", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// running says a process runs: not gone, nor a zombie. /proc's word where
// there is one (Linux), else ps's.
func running(pid string) bool {
	if b, err := os.ReadFile("/proc/" + pid + "/stat"); err == nil {
		_, rest, _ := strings.Cut(string(b), ") ")
		return !strings.HasPrefix(rest, "Z") && !strings.HasPrefix(rest, "X")
	} else if _, err := os.Stat("/proc/self/stat"); err == nil {
		return false // /proc has no such process
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	st := strings.TrimSpace(string(out))
	return err == nil && st != "" && !strings.HasPrefix(st, "Z")
}

// A scenario run before a change and not known after it, the change having
// moved its rule out of Prometheus's format (a rule file made a Grafana
// rule, the same name), counts as unrun: the gate fails closed.
func TestSidesMovedOutOfFormat(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "t.yaml"), []byte(scenariotest.Test("pass")), 0o600)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := []rules.Rule{{Name: "CheckoutErrors", Group: "shop", Source: "a.yaml", Expr: "errors > 5", Prometheus: true}}
	after := []rules.Rule{{Name: "CheckoutErrors", Group: "shop", Source: "a.json", Expr: "errors > 5", Graph: &rules.Graph{}}}
	s := &Sides{Promtool: Promtool{Path: scenariotest.Install(t)}, Rules: [2][]rules.Rule{before, after}, After: root}
	defer s.Close()
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	b, a := &verdict.Report{Rules: []verdict.RuleResult{{Rule: "CheckoutErrors"}}}, &verdict.Report{Rules: []verdict.RuleResult{{Rule: "CheckoutErrors"}}}
	c := &verdict.Comparison{From: to.Add(-7 * 24 * time.Hour), To: to, Before: b, After: a, Scenarios: s,
		Rules: []verdict.RuleChange{{Rule: "CheckoutErrors", Before: &b.Rules[0], After: &a.Rules[0]}}}
	c.Summarize(false)
	e, err := verdict.ReadExpectations(strings.NewReader("expectations:\n  - rule: CheckoutErrors\n    scenarios: [t.yaml]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Expect(e, nil, nil); err != nil {
		t.Fatal(err)
	}
	if r := c.Expectations.Results[0]; r.BeforeState != verdict.ExpectMet || r.AfterState != verdict.ExpectUnknown || !r.Unrun || c.Summary.ScenariosUnrun != 1 {
		t.Errorf("moved out of Prometheus's format: %+v, %d unrun", r, c.Summary.ScenariosUnrun)
	}
}
