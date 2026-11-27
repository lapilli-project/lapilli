package serve

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// TestForOutlastsShown: a rule whose for one event can't outlast says so
// among the problems on its page and through MCP's rule; its proposals tab
// says how long its condition counts events over; and a proposal of such a
// for says what it means on its page, in its pull request's description,
// and through MCP, and is made all the same.
func TestForOutlastsShown(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, func(c *Config) { c.CheckContent = true })
	if err := os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(`groups:
  - name: pods
    rules:
      - alert: Restarts
        expr: increase(restarts[30m]) > 0.5
        for: 1h
        labels: {severity: page}
        annotations: {summary: "{{ $labels.pod }} restarted", runbook_url: "https://runbooks.example/restarts"}
      - alert: RestartsNow
        expr: increase(restarts[30m]) > 0.5
        for: 1m
        labels: {severity: page}
        annotations: {summary: "{{ $labels.pod }} restarted", runbook_url: "https://runbooks.example/restarts"}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	said := "Its for (1h) is as long as the 30m its condition counts over, or longer: one event alone can never fire it; only events that keep coming for 1h will."
	_, body, _ := get(t, h, "/rules/Restarts")
	if !strings.Contains(html.UnescapeString(body), said) {
		t.Error("/rules/Restarts: its for isn't said to outlast one event")
	}
	if _, body, _ := get(t, h, "/rules/RestartsNow"); strings.Contains(html.UnescapeString(body), "one event alone can never fire it") {
		t.Error("/rules/RestartsNow: a for of 1m outlasts nothing")
	}
	_, out, err := s.toolRule(ctx, nil, ruleIn{Name: "Restarts"})
	if err != nil || out.Says == nil || len(out.Says.Problems) != 1 || out.Says.Problems[0].Check != "for_outlasts" || out.Says.Problems[0].Message != said {
		t.Errorf("MCP rule: %+v %v", out.Says, err)
	}
	if _, body, _ := ui(t, h, "GET", "/rules/RestartsNow?tab=proposals", ana, "same-origin", nil); !strings.Contains(body, "Its condition counts events over 30m: with a &#34;for&#34; that long or longer, one event alone can never fire it.") {
		t.Error("the proposals tab doesn't say the range its condition counts over")
	}

	id, err := s.propose(ctx, ask{Rule: "RestartsNow", Kind: ledger.KindFor, For: time.Hour, From: "person"}, "ana", false, true)
	if err != nil {
		t.Fatal(err)
	}
	// In each language, and accessible.
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	paths := []string{"/rules/Restarts", "/rules/RestartsNow?tab=proposals", fmt.Sprintf("/proposals/%d", id)}
	pageA11y(t, h, ana, paths...)
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
	proposed := "As proposed, its for (1h) is as long as the 30m its condition counts over, or longer: one event alone could never fire it; only events that keep coming for 1h would."
	if _, body, _ := get(t, h, fmt.Sprintf("/proposals/%d", id)); !strings.Contains(html.UnescapeString(body), proposed) {
		t.Error("the proposal's page doesn't warn")
	}
	p, err := s.l.ProposalByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodyOf(t, s, p), proposed) || !strings.Contains(string(p.Evidence), `"for_outlasts":{"for_ns":3600000000000,"window_ns":1800000000000}`) {
		t.Errorf("its description and evidence: %s\n%s", bodyOf(t, s, p), p.Evidence)
	}
	_, out, err = s.toolRule(ctx, nil, ruleIn{Name: "RestartsNow"})
	if err != nil || len(out.Proposals) != 1 || out.Proposals[0].Outlasts != proposed {
		t.Errorf("MCP rule's proposals: %+v %v", out.Proposals, err)
	}
	// A for under the range says nothing.
	ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "RestartsNow"})
	for _, x := range ps {
		s.l.Move(ctx, x.ID, []string{ledger.StateProposed}, ledger.StateDropped, "ana", ledger.Step{})
	}
	id, err = s.propose(ctx, ask{Rule: "RestartsNow", Kind: ledger.KindFor, For: 10 * time.Minute, From: "person"}, "ana", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := s.l.ProposalByID(ctx, id); strings.Contains(string(p.Evidence), "for_outlasts") {
		t.Errorf("10m: %s", p.Evidence)
	}
}

// The same, of the restart rule written in Grafana: a query, a reduce
// (last) and a threshold (> 0.5).
func TestForOutlastsGrafana(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, dir := changesServer(t, func(c *Config) { c.CheckContent = true })
	rule := func(title, for_ string) string {
		return `
      - uid: ` + strings.ToLower(title) + `
        title: ` + title + `
        condition: C
        for: ` + for_ + `
        labels: {severity: page}
        annotations: {summary: "{{ $labels.pod }} restarted", runbook_url: "https://runbooks.example/restarts"}
        data:
          - refId: A
            relativeTimeRange: {from: 1800, to: 0}
            datasourceUid: prom
            model: {expr: 'sum by (pod) (increase(restarts[30m]))', instant: true, refId: A}
          - refId: B
            datasourceUid: __expr__
            model: {type: reduce, reducer: last, expression: A, refId: B}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: B, refId: C, conditions: [{evaluator: {type: gt, params: [0.5]}}]}`
	}
	if err := os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(`apiVersion: 1
groups:
  - orgId: 1
    name: pods
    folder: Ops
    interval: 1m
    rules:`+rule("Restarts", "1h")+rule("RestartsNow", "1m")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	said := "Its for (1h) is as long as the 30m its condition counts over, or longer: one event alone can never fire it; only events that keep coming for 1h will."
	if _, body, _ := get(t, h, "/rules/Restarts"); !strings.Contains(html.UnescapeString(body), said) {
		t.Error("/rules/Restarts: its for isn't said to outlast one event")
	}
	if _, body, _ := get(t, h, "/rules/RestartsNow"); strings.Contains(html.UnescapeString(body), "one event alone can never fire it") {
		t.Error("/rules/RestartsNow: a for of 1m outlasts nothing")
	}
	if _, body, _ := ui(t, h, "GET", "/rules/RestartsNow?tab=proposals", ana, "same-origin", nil); !strings.Contains(body, "Its condition counts events over 30m: with a &#34;for&#34; that long or longer, one event alone can never fire it.") {
		t.Error("the proposals tab doesn't say the range its condition counts over")
	}
	id, err := s.propose(ctx, ask{Rule: "RestartsNow", Kind: ledger.KindFor, For: time.Hour, From: "person"}, "ana", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.l.ProposalByID(ctx, id); err != nil || !strings.Contains(string(p.Evidence), `"for_outlasts":{"for_ns":3600000000000,"window_ns":1800000000000}`) {
		t.Errorf("its evidence: %v %s", err, p.Evidence)
	}
}
