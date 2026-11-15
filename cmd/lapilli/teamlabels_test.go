package main

import (
	"strings"
	"testing"
)

// --team-labels names the labels whose values the Teams page suggests as
// teams: label names, eight at most; anything else is refused at start.
func TestServeTeamLabelsFlag(t *testing.T) {
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	for _, bad := range []string{"", ",", "team=x", "9team", "a,b,c,d,e,f,g,h,i", "team owner"} {
		var errs strings.Builder
		if code := run([]string{"serve", "--rules", rulesFile, "--prometheus", "http://127.0.0.1:1", "--team-labels", bad}, &strings.Builder{}, &errs); code != 2 ||
			!strings.Contains(errs.String(), "--team-labels") {
			t.Errorf("--team-labels %q: exit %d\n%s", bad, code, errs.String())
		}
	}
	var errs strings.Builder
	run([]string{"serve", "--rules", rulesFile, "--prometheus", "http://127.0.0.1:1", "--team-labels", "team, service_owner",
		"--listen", "256.0.0.1:1", "--ledger", t.TempDir()}, &strings.Builder{}, &errs)
	if strings.Contains(errs.String(), "--team-labels") || !strings.Contains(errs.String(), "--listen") {
		t.Errorf("--team-labels team,service_owner: %s", errs.String())
	}
}
