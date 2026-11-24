package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Terraform's JSON of a plan, or of a state, is given to --rules as any
// rules are: its rules are replayed, with no flag to say what it is.
func TestVerdictRulesTerraformJSON(t *testing.T) {
	now = func() time.Time { return t0 }
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-1, title: Errors, start: 2026-11-01T22:30:00Z, end: 2026-11-01T22:40:00Z}\n")
	for _, f := range []string{"terraform-plan.json", "terraform-state.json"} {
		var mu sync.Mutex
		var asked []string
		h := scrapedHistory{&mu, &asked}
		newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
		var stdout, stderr bytes.Buffer
		run([]string{"verdict", "--rules", "../../internal/rules/testdata/" + f, "--prometheus", "http://p", "--since", "1h", "--incidents", incidents}, &stdout, &stderr)
		joined := strings.Join(asked, "\n")
		for _, want := range []string{"checkout_errors_total", "checkout_requests_total", `up{job="edge"}`} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: asked %q, want a query with %q\n%s", f, asked, want, stderr.String())
			}
		}
		if strings.Contains(joined, "checkout_seconds_bucket") {
			t.Errorf("%s: the paused rule was replayed: %q", f, asked)
		}
	}
}
