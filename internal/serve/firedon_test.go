package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// TestFiredOn: Errors is payments' rule, by its name, and every one of its
// firings in the week before was on the search service, which web owns by
// its labels: the digest keeps it, and the rule's page, web's rules list,
// web's digest, what its firings were with no record, and MCP say so; read
// from what the digest kept.
func TestFiredOn(t *testing.T) {
	ctx := context.Background()
	// No impact signal: the week before has no record, everyone's or web's.
	s, _ := server(t, func(c *Config) { c.Backfill = 10 * 24 * time.Hour; c.Impact = "" })
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "web", Labels: incident.Labels{"service": {"search"}}}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Errors$"}, "ana"); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	from := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	on, err := s.firedOnFor(ctx, from)
	if err != nil || len(on) != 1 || on["Errors"] != (firedOn{Rule: "Errors", Owner: "payments", Team: "web", Firings: 14, Of: 14}) {
		t.Fatalf("%+v %v", on, err)
	}
	h := s.Handler()
	if _, body, _ := get(t, h, "/rules/Errors"); !strings.Contains(body, "Its alerts go to <bdi>payments</bdi>; most came from what <bdi>web</bdi> owns (14 of 14), in the week of <bdi>2026-10-26</bdi>.") {
		t.Error("/rules/Errors doesn't say where its firings came from")
	}
	if _, body, _ := get(t, h, "/rules?team=web"); !strings.Contains(body, `Fired on what <bdi>web</bdi> owns`) || !strings.Contains(body, `<a href="/rules/Errors?team=web&amp;weeks=4"><bdi>Errors</bdi></a> <span class="quiet">(owned by <bdi>payments</bdi>)</span>`) ||
		!strings.Contains(body, "14 of 14 firings") {
		t.Error("/rules?team=web doesn't list Errors")
	}
	if _, body, _ := get(t, h, "/rules?team=payments"); strings.Contains(body, `id="fired-on"`) {
		t.Error("payments' own rule isn't another team's")
	}
	_, out, err := s.toolRule(ctx, nil, ruleIn{Name: "Errors"})
	if err != nil || out.Rule.FiredOn == nil || out.Rule.FiredOn.Team != "web" || out.Rule.FiredOn.Owner != "payments" {
		t.Errorf("MCP rule: %+v %v", out.Rule.FiredOn, err)
	}
	// What its firings were, with no record: each alert with its team.
	r, ok, err := s.l.ForPeriod(ctx, "unrecorded", from)
	var us []verdict.Unrecorded
	if err != nil || !ok || r.Decode(&us) != nil || len(us) != 1 || us[0].Team != "payments" || len(us[0].Top) != 1 || us[0].Top[0].Team != "web" {
		t.Errorf("unrecorded: %+v %v", us, err)
	}
	// web's digest says it, and has the rule's firings with no record.
	o, _ := s.options(ctx)
	own, _ := s.owners(ctx)
	web, _ := own.team("web")
	rep, err := s.digestReport(ctx, from, from.Add(7*24*time.Hour), o)
	if err != nil {
		t.Fatal(err)
	}
	part := teamReport(rep, own, web, o.Hours)
	text, err := s.digestText(ctx, part, nil, from, from.Add(7*24*time.Hour), &web)
	if err != nil || !strings.Contains(text, "Rules of other teams, or of no team, that fired mostly on what web owns: Errors (14 of 14 firings).") ||
		!strings.Contains(text, "Errors: Fired 14 times with no incident written down") {
		t.Errorf("web's digest: %v\n%s", err, text)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	pageA11y(t, h, "", "/rules/Errors", "/rules?team=web")
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}

// With no team that owns by labels, the digest keeps no rule as fired on
// another team's.
func TestFiredOnNoLabels(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Errors$"}, "ana"); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if xs, err := s.firedOnOf(ctx, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), now); err != nil || xs != nil {
		t.Errorf("%+v %v", xs, err)
	}
}
