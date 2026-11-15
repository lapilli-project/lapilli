package serve

import (
	"context"
	"strings"
	"testing"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// TestOwnedByGroupLabels: a rule whose group gives it a team's label is
// that team's, as its alerts carry the label, its row's chips show it and
// label: picks it by it; its page says so. The rule's own label, where it
// sets one, is the one taken.
func TestOwnedByGroupLabels(t *testing.T) {
	ctx := context.Background()
	s := labelledServer(t, `groups:
  - name: payments
    labels: {team: payments}
    rules:
      - {alert: Charged, expr: charged > 1}
      - {alert: Moved, expr: moved > 1, labels: {team: search}}
`, nil)
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Labels: incident.Labels{"team": {"payments"}}}, "ana"); err != nil {
		t.Fatal(err)
	}
	o, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o.teamOf("Charged") != "payments" || o.teamOf("Moved") != "" {
		t.Errorf("Charged is %q's, Moved %q's; want payments's, and no one's", o.teamOf("Charged"), o.teamOf("Moved"))
	}
	h := s.Handler()
	if _, body, _ := get(t, h, "/rules/Charged"); !strings.Contains(body, "Its labels have <bdi>team=payments</bdi>, which <bdi>payments</bdi> owns by.") {
		t.Errorf("a rule its group's label makes a team's doesn't say so:\n%s", body)
	}
	if _, body, _ := get(t, h, "/rules?team=payments&weeks=12&perf=silent"); !strings.Contains(body, `<bdi>Charged</bdi>`) {
		t.Errorf("the team's rules don't list it:\n%s", body)
	}
	// The suggestions read them so too: search, from Moved's own label.
	var got []string
	for _, l := range s.suggestTeams(o).Lines {
		got = append(got, l.Key+"="+l.Value)
	}
	if strings.Join(got, " ") != "team=search" {
		t.Errorf("suggested %v, want team=search", got)
	}
}
