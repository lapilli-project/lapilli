package main

import (
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/scenario"
	"lapilli.dev/internal/scenario/scenariotest"
)

// The stand-in for promtool is this test binary, built with cgo: it
// starts under no less than about 160 MiB of RLIMIT_DATA, so the tests
// cap it at 512 MiB (internal/scenario's TestRunMemoryCap); promtool's
// release starts under 64 MiB.
var standInMemory = []string{"--promtool-memory", "512Mi"}

// --promtool-memory caps each scenario lapilli verdict runs, as serve's
// does: 256Mi unless given, 64Mi at least, only with --promtool; on Linux,
// promtool's RLIMIT_DATA, through the lapilli binary started again
// (scenario's wrapper, linked into it); elsewhere, said to be no cap.
func TestVerdictPromtoolMemory(t *testing.T) {
	alone(t)
	rulesFile := write(t, "rules.yaml", shopRules("5m"))
	expect := write(t, "expectations.yaml", "expectations:\n  - rule: CheckoutErrors\n    scenarios: [t.yaml]\n")
	promtool := scenariotest.Install(t)
	base := []string{"--prometheus", "http://p", "--rules", rulesFile, "--compare", rulesFile, "--expect", expect, "--promtool", promtool}
	f, err := parseVerdictFlags(base)
	if err != nil || f.promtoolMemory != 256<<20 {
		t.Fatalf("by default: %d %v", f.promtoolMemory, err)
	}
	if f, err := parseVerdictFlags(append(slices.Clone(base), "--promtool-memory", "1Gi")); err != nil || f.promtoolMemory != 1<<30 {
		t.Errorf("1Gi: %d %v", f.promtoolMemory, err)
	}
	if _, err := parseVerdictFlags(append(slices.Clone(base), "--promtool-memory", "32Mi")); err == nil || !strings.Contains(err.Error(), "64Mi at least") {
		t.Errorf("32Mi: %v", err)
	}
	if _, err := parseVerdictFlags([]string{"--prometheus", "http://p", "--rules", rulesFile, "--compare", rulesFile, "--expect", expect,
		"--promtool-memory", "1Gi"}); err == nil || !strings.Contains(err.Error(), "give --promtool") {
		t.Errorf("without --promtool: %v", err)
	}

	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	exp := t.TempDir()
	writeIn(t, exp, map[string]string{
		"expectations.yaml": "expectations:\n  - rule: CheckoutErrors\n    scenarios: [tests/shop.yaml]\n",
		"tests/shop.yaml":   scenariotest.Test("see"),
	})
	pr := write(t, "pr.yaml", shopRules("2h"))
	var out, errs strings.Builder
	code := run(slices.Concat([]string{"verdict", "--prometheus", "http://p", "--tz", "UTC", "--rules", pr, "--compare", rulesFile,
		"--expect", filepath.Join(exp, "expectations.yaml"), "--promtool", promtool, "--json"}, standInMemory), &out, &errs)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs.String())
	}
	capped := strings.Contains(out.String(), `rlimit_data `+strconv.Itoa(512<<20)+`\n`)
	switch {
	case scenario.CanCap && !capped:
		t.Errorf("on %s, not capped:\n%s", runtime.GOOS, out.String())
	case !scenario.CanCap && (capped || !strings.Contains(errs.String(), "promtool runs without a memory cap here ("+runtime.GOOS+")")):
		t.Errorf("on %s, not said to be no cap:\n%s", runtime.GOOS, errs.String())
	}
	if scenario.CanCap && strings.Contains(errs.String(), "without a memory cap") {
		t.Errorf("on %s, said to be no cap:\n%s", runtime.GOOS, errs.String())
	}
}
