package serve

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// quietAfter is how long a source may go without firings copied in before
// it is told of.
func (s *Server) quietAfter() time.Duration {
	if s.cfg.SourceQuiet > 0 {
		return s.cfg.SourceQuiet
	}
	return sourceQuiet
}

// maxIncidentSeries is how many incidents, the latest, /metrics gives a
// series each, and how many series say which teams own them.
const maxIncidentSeries = 500

// ruleSeries are a rule's labels in /metrics: its name, severity and team.
func ruleSeries(rr verdict.RuleResult) string {
	l := fmt.Sprintf(`rule="%s"`, labelValue(rr.Rule))
	if rr.Severity != "" {
		l += fmt.Sprintf(`,severity="%s"`, labelValue(rr.Severity))
	}
	if rr.Team != "" {
		l += fmt.Sprintf(`,team="%s"`, labelValue(rr.Team))
	}
	return l
}

// nocMetrics are what an operations team asks of its alerts
// (docs/design/noc.md), beside the rule metrics in rep, the last seven
// days': firings during deploys, the weeks with a re-fire, each incident's
// state and lead by the teams that own it, and when each source last
// sent.
func (s *Server) nocMetrics(ctx context.Context, b *bytes.Buffer, rep *verdict.Report, own *owners, today int64) error {
	family := func(name, help string) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	deploys, err := s.l.HasDeploys(ctx)
	if err != nil {
		return err
	}
	if deploys {
		family("lapilli_rule_firings_during_deploys", "Firings judged that came during a deploy of what they are about, or within --deploy-after of its end: in lapilli_rule_firings too.")
		for _, rr := range rep.Rules {
			fmt.Fprintf(b, "lapilli_rule_firings_during_deploys{%s} %d\n", ruleSeries(rr), rr.DuringDeploys)
		}
		family("lapilli_rule_caught_during_deploys", "Of the firings during deploys, those that caught an incident: in lapilli_rule_caught too.")
		for _, rr := range rep.Rules {
			fmt.Fprintf(b, "lapilli_rule_caught_during_deploys{%s} %d\n", ruleSeries(rr), rr.DeploysCaught)
		}
		if last, err := s.l.LastDeploy(ctx, s.now()); err != nil {
			return err
		} else if !last.IsZero() {
			family("lapilli_serve_deploys_last_timestamp_seconds", "When the latest deploy the deploys file holds that has started started, in Unix seconds.")
			fmt.Fprintf(b, "lapilli_serve_deploys_last_timestamp_seconds %d\n", last.Unix())
		}
	}

	// The weeks with a re-fire, of the twelve whole weeks before this one
	// (tallies.whole).
	t, err := s.tallies(ctx, weeks+1)
	if err != nil {
		return err
	}
	everyone, counted := t.refireWeeks(t.weeks, func(int64) bool { return true })
	family("lapilli_weeks_counted", "Whole weeks, of the twelve before this one, the ledger has all of (not the one its first firing fell in): what lapilli_refired_weeks and lapilli_rule_refired_weeks count of.")
	fmt.Fprintf(b, "lapilli_weeks_counted %d\n", counted)
	family("lapilli_refired_weeks", "Whole weeks, of lapilli_weeks_counted, in which a firing re-fired: everyone's without a team label, and each team's rules'.")
	fmt.Fprintf(b, "lapilli_refired_weeks %d\n", everyone)
	for _, tm := range own.teams {
		n, _ := t.refireWeeks(t.weeks, func(id int64) bool { return own.rule[t.rules[id].Name] == tm.ID })
		fmt.Fprintf(b, "lapilli_refired_weeks{team=\"%s\"} %d\n", labelValue(tm.Name), n)
	}
	family("lapilli_rule_refired_weeks", "Whole weeks, of lapilli_weeks_counted, in which a firing of the rule re-fired.")
	ids := slices.Collect(maps.Keys(t.rules))
	slices.SortFunc(ids, func(a, b int64) int { return strings.Compare(t.rules[a].Name, t.rules[b].Name) })
	for _, id := range ids {
		if n, _ := t.refireWeeks(t.weeks, func(rid int64) bool { return rid == id }); n > 0 {
			r := t.rules[id]
			fmt.Fprintf(b, "lapilli_rule_refired_weeks{%s} %d\n", ruleSeries(verdict.RuleResult{Rule: r.Name, Severity: r.Severity, Team: own.teamOf(r.Name)}), n)
		}
	}

	// Each incident of the last seven days, a series each; which teams own
	// it, in a series of its own for each: a dashboard picks a team's by
	// joining on incident, and counts one two teams own once. Both are
	// bounded, the latest incidents kept.
	incs := rep.Incidents
	if len(incs) > maxIncidentSeries {
		incs = incs[len(incs)-maxIncidentSeries:]
	}
	var states, leads, teams []string
	for i := len(incs) - 1; i >= 0; i-- { // the latest first, so the bound keeps them
		in := incs[i]
		src := "written"
		if in.Candidate {
			src = "impact"
		}
		state := "caught"
		switch {
		case in.Missed():
			state = "missed"
		case in.OnlyNoisy():
			state = "noisy_only"
		}
		l := fmt.Sprintf(`incident="%s",source="%s"`, labelValue(in.ID), src)
		states = append(states, fmt.Sprintf(`lapilli_incident_state{%s,state="%s"} 1`, l, state))
		if c, caught := in.First(); caught {
			leads = append(leads, fmt.Sprintf("lapilli_incident_lead_seconds{%s} %g", l, -c.Delay.Seconds()))
		}
		owned := false
		for _, tm := range own.teams {
			if len(teams) == maxIncidentSeries {
				break
			}
			if own.ownsIncident(tm, ledger.Incident{Incident: in.Incident, CaughtBy: in.CaughtBy}) {
				teams = append(teams, fmt.Sprintf(`lapilli_incident_team{incident="%s",team="%s"} 1`, labelValue(in.ID), labelValue(tm.Name)))
				owned = true
			}
		}
		if !owned && len(teams) < maxIncidentSeries {
			teams = append(teams, fmt.Sprintf(`lapilli_incident_team{incident="%s"} 1`, labelValue(in.ID)))
		}
	}
	family("lapilli_incident_state", "Each incident of the last seven days, written down or an impact window found by --impact, as caught, caught only by noisy rules, or missed, by the rules' stones in its week: 1. The latest 500 at most.")
	for _, x := range states {
		b.WriteString(x + "\n")
	}
	family("lapilli_incident_lead_seconds", "How long before each caught incident's start the first alert from a rule that isn't noisy came: negative when it came after.")
	for _, x := range leads {
		b.WriteString(x + "\n")
	}
	family("lapilli_incident_team", "Each team that owns an incident of lapilli_incident_state (one of its rules caught it, or it has a value of a label the team owns by): 1, without a team label for an incident no team owns. 500 series at most, the latest incidents first.")
	for _, x := range teams {
		b.WriteString(x + "\n")
	}

	// When each way firings and notifications come in last brought one.
	last, err := s.l.LastNotified(ctx)
	if err != nil {
		return err
	}
	if len(last) > 0 {
		family("lapilli_serve_notifications_last_timestamp_seconds", "When the latest notification taken came, by where it came from, in Unix seconds.")
		for _, src := range slices.Sorted(maps.Keys(last)) {
			fmt.Fprintf(b, "lapilli_serve_notifications_last_timestamp_seconds{source=\"%s\"} %d\n", labelValue(src), last[src].Unix())
		}
	}
	return nil
}
