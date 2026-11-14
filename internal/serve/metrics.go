package serve

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// metrics are lapilli verdict --metrics over the last seven days, from the
// ledger's daily tallies, and lapilli serve's own: a Prometheus that
// scrapes them can graph the stones week by week without a Pushgateway.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tag := s.tag()
	c, ok := s.cache.get(tag, "/metrics")
	if !ok {
		o, err := s.options(ctx)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		today := s.l.Day(s.now())
		days, err := s.l.Days(ctx, today-6, today)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		table, err := s.l.RuleTable(ctx)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		// Each rule's team, a label of its series, so a dashboard can
		// show one team's.
		own, err := s.owners(ctx)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		sum := map[int64]*verdict.RuleResult{}
		for _, d := range days {
			rr := sum[d.Rule]
			if rr == nil {
				name := table[d.Rule].Name
				rr = &verdict.RuleResult{Rule: name, Severity: table[d.Rule].Severity, Team: own.teamOf(name)}
				sum[d.Rule] = rr
			}
			rr.Firings += d.Firings
			rr.White += d.White
			rr.Black += d.Black()
			rr.Refired += d.Refired
			rr.DuringDeploys += d.Deploy
			rr.DeploysCaught += d.DeployWhite
		}
		rep := &verdict.Report{}
		for _, rr := range sum {
			rr.Noisy = noisy(rr.Firings, rr.White, o)
			rep.Rules = append(rep.Rules, *rr)
		}
		// Each rule's by its team's hours, or the server's.
		if sc := own.schedules(o.Hours); !sc.none() {
			if err := s.l.CountHours(ctx, rep.Rules, s.l.Midnight(today-6), s.l.Midnight(today+1), sc.of); err != nil {
				s.oops(w, r, err)
				return
			}
			rep.Hours = sc.used(rep.Rules, nil)
		}
		ledger.SortRules(rep.Rules)
		incs, err := s.l.Incidents(ctx, s.l.Midnight(today-6), s.now().Add(time.Hour), false)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		// A catch by a rule noisy in the incident's week, as /incidents,
		// the overview and the summary judge it: not by the seven days to
		// now, so an incident is in the same state everywhere.
		t, err := s.tallies(ctx, weeks)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		for _, in := range incs {
			ir := verdict.IncidentResult{Incident: in.Incident, CaughtBy: slices.Clone(in.CaughtBy)}
			week := ledger.Week(s.l.Day(in.Start))
			for i := range ir.CaughtBy {
				ir.CaughtBy[i].Noisy = t.noisy(week, t.id(ir.CaughtBy[i].Rule))
			}
			rep.Incidents = append(rep.Incidents, ir)
		}
		rep.SomeRules = o.SomeRules
		var inEffect func(string) bool
		if latest, ok, err := s.l.Latest(ctx); err == nil && ok {
			inEffect = func(rule string) bool { return slices.Contains(latest.Names, rule) }
		}
		rep.Credit(inEffect)
		var last time.Time
		if runs, err := s.l.Runs(ctx, 1); err == nil && len(runs) > 0 {
			last = runs[0].Finished
		}
		var b bytes.Buffer
		if err := verdict.Prometheus(&b, rep, last); err != nil {
			s.oops(w, r, err)
			return
		}
		if err := s.nocMetrics(ctx, &b, rep, own, today); err != nil {
			s.oops(w, r, err)
			return
		}
		cursor, err := s.l.Cursor(ctx)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		if !cursor.IsZero() {
			fmt.Fprintf(&b, "# HELP lapilli_serve_cursor_timestamp_seconds How far the ledger has firings up to, in Unix seconds.\n# TYPE lapilli_serve_cursor_timestamp_seconds gauge\nlapilli_serve_cursor_timestamp_seconds %d\n", cursor.Unix())
		}
		s.status.Lock()
		took := s.status.took
		runs := map[string]int{}
		for k, v := range s.status.runs {
			runs[k] = v
		}
		s.status.Unlock()
		fmt.Fprintf(&b, "# HELP lapilli_serve_run_duration_seconds How long the last run took.\n# TYPE lapilli_serve_run_duration_seconds gauge\nlapilli_serve_run_duration_seconds %g\n", took.Seconds())
		b.WriteString("# HELP lapilli_serve_runs_total Runs since lapilli serve started, by kind and result.\n# TYPE lapilli_serve_runs_total counter\n")
		for _, k := range slices.Sorted(maps.Keys(runs)) {
			kind, result, _ := strings.Cut(k, " ")
			fmt.Fprintf(&b, "lapilli_serve_runs_total{kind=%q,result=%q} %d\n", kind, result, runs[k])
		}
		// What Lapilli itself came to, as on the outcomes page.
		out, err := s.outcomes(ctx)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		e := out.Effect
		fmt.Fprintf(&b, "# HELP lapilli_serve_firings_avoided How many fewer times the changed rules fired since each reviewed change than at their rates before it.\n# TYPE lapilli_serve_firings_avoided gauge\nlapilli_serve_firings_avoided %d\n", e.Fewer)
		fmt.Fprintf(&b, "# HELP lapilli_serve_nothing_avoided How many fewer firings with no incident (black stones) the changed rules made since each reviewed change than at their rates before it.\n# TYPE lapilli_serve_nothing_avoided gauge\nlapilli_serve_nothing_avoided %d\n", e.FewerBlack)
		b.WriteString("# HELP lapilli_serve_rule_changes Reviewed changes to rules, by how they came out against the prediction.\n# TYPE lapilli_serve_rule_changes gauge\n")
		fmt.Fprintf(&b, "lapilli_serve_rule_changes{outcome=\"as_predicted\"} %d\nlapilli_serve_rule_changes{outcome=\"otherwise\"} %d\nlapilli_serve_rule_changes{outcome=\"measuring\"} %d\n",
			e.AsPredicted, e.Measured-e.AsPredicted, e.Measuring)
		fmt.Fprintf(&b, "# HELP lapilli_serve_incident_last_timestamp_seconds When the latest incident written down started (from the incidents file, the UI or the API, impact windows confirmed among them), in Unix seconds; 0 for none.\n# TYPE lapilli_serve_incident_last_timestamp_seconds gauge\nlapilli_serve_incident_last_timestamp_seconds %d\n", unixOr0(out.Record.LastIncident))
		fmt.Fprintf(&b, "# HELP lapilli_serve_edits Edits made in the UI or the API in the last four weeks.\n# TYPE lapilli_serve_edits gauge\nlapilli_serve_edits %d\n", out.Use.Edits)
		// Where firings are copied in from, so an alert can say a job
		// stopped copying them.
		srcs, err := s.l.Sources(ctx)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		if len(srcs) > 0 || s.cfg.FiringsToken != "" {
			b.WriteString("# HELP lapilli_serve_source_last_timestamp_seconds When firings were last copied in from each source, in Unix seconds.\n# TYPE lapilli_serve_source_last_timestamp_seconds gauge\n")
			for _, src := range srcs {
				fmt.Fprintf(&b, "lapilli_serve_source_last_timestamp_seconds{source=%q} %d\n", src.Name, src.Written.Unix())
			}
			b.WriteString("# HELP lapilli_serve_source_firings Firings in the period last copied in from each source.\n# TYPE lapilli_serve_source_firings gauge\n")
			for _, src := range srcs {
				fmt.Fprintf(&b, "lapilli_serve_source_firings{source=%q} %d\n", src.Name, src.Firings)
			}
			fmt.Fprintf(&b, "# HELP lapilli_serve_source_quiet_seconds How long a source may go without firings copied in before it is told of (--source-quiet).\n# TYPE lapilli_serve_source_quiet_seconds gauge\nlapilli_serve_source_quiet_seconds %g\n", s.quietAfter().Seconds())
		}
		// Notifications as they were sent, once there are any.
		if p, err := s.notified(ctx); err != nil {
			s.oops(w, r, err)
			return
		} else if !p.first.IsZero() {
			// Only the ledger's rules, and a rule's busiest receivers: an alert's
			// name and a receiver's come from outside, and each new one would
			// be a series more for the Prometheus that scrapes this.
			table, err := s.l.RuleTable(ctx)
			if err != nil {
				s.oops(w, r, err)
				return
			}
			known := make(map[string]bool, len(table))
			for _, rr := range table {
				known[rr.Name] = true
			}
			type key struct{ rule, receiver, reason string }
			by := map[key]int{}
			from, to := s.l.Midnight(today-6), s.l.Midnight(today+1)
			receivers := map[string]map[string]int{}
			for _, pg := range p.pages {
				if known[pg.Rule] && !pg.At.Before(from) && pg.At.Before(to) {
					if receivers[pg.Rule] == nil {
						receivers[pg.Rule] = map[string]int{}
					}
					receivers[pg.Rule][pg.Receiver]++
				}
			}
			busiest := map[string]map[string]bool{}
			for rule, rs := range receivers {
				names := slices.Collect(maps.Keys(rs))
				slices.SortFunc(names, func(a, b string) int { return cmp.Or(cmp.Compare(rs[b], rs[a]), strings.Compare(a, b)) })
				busiest[rule] = map[string]bool{}
				for _, n := range names[:min(len(names), maxReceiverSeries)] {
					busiest[rule][n] = true
				}
			}
			for _, pg := range p.pages {
				if known[pg.Rule] && !pg.At.Before(from) && pg.At.Before(to) {
					recv := pg.Receiver
					if !busiest[pg.Rule][recv] {
						recv = "other"
					}
					by[key{pg.Rule, recv, pg.Reason}]++
				}
			}
			keys := slices.Collect(maps.Keys(by))
			slices.SortFunc(keys, func(a, b key) int {
				return strings.Compare(a.rule+"\x00"+a.receiver+"\x00"+a.reason, b.rule+"\x00"+b.receiver+"\x00"+b.reason)
			})
			b.WriteString("# HELP lapilli_rule_notifications Notifications sent for each rule over the last seven days, as the alert manager sent them, by receiver and why: first, new, resolved, repeat or unknown.\n# TYPE lapilli_rule_notifications gauge\n")
			for _, k := range keys {
				fmt.Fprintf(&b, "lapilli_rule_notifications{rule=\"%s\",receiver=\"%s\",reason=\"%s\"} %d\n", labelValue(k.rule), labelValue(k.receiver), k.reason, by[k])
			}
			fmt.Fprintf(&b, "# HELP lapilli_serve_notifications_since_timestamp_seconds When the first notification kept came, in Unix seconds: notifications are counted from then.\n# TYPE lapilli_serve_notifications_since_timestamp_seconds gauge\nlapilli_serve_notifications_since_timestamp_seconds %d\n", p.first.Unix())
		}
		if s.checking() {
			from, to := s.lastWeek()
			checks, err := s.l.Checks(ctx, from, to)
			if err != nil {
				s.oops(w, r, err)
				return
			}
			if len(checks) > 0 {
				b.WriteString("# HELP lapilli_rule_replay_check The replay beside what really fired over the seven whole days before today, by rule: firings alike, only in the replay, and only in what really fired. Rules that share an alert name, which what really fired doesn't tell apart, each carry the counts of them all.\n# TYPE lapilli_rule_replay_check gauge\n")
				for _, name := range slices.Sorted(maps.Keys(checks)) {
					c := checks[name]
					for _, x := range []struct {
						way string
						n   int
					}{{"alike", c.Alike}, {"replayed", c.Replayed - c.Alike}, {"real", c.Real - c.Alike}} {
						fmt.Fprintf(&b, "lapilli_rule_replay_check{rule=\"%s\",way=\"%s\"} %d\n", labelValue(name), x.way, x.n)
					}
				}
			}
			done, err := s.l.CheckedTo(ctx)
			if err != nil {
				s.oops(w, r, err)
				return
			}
			if !done.IsZero() {
				fmt.Fprintf(&b, "# HELP lapilli_serve_replay_checked_to_timestamp_seconds How far the replay has been set beside what really fired, in Unix seconds.\n# TYPE lapilli_serve_replay_checked_to_timestamp_seconds gauge\nlapilli_serve_replay_checked_to_timestamp_seconds %d\n", done.Unix())
			}
		}
		cf, err := s.contentFindings(ctx)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		if problems := contentProblems(cf); len(problems) > 0 {
			b.WriteString("# HELP lapilli_rule_content_problems Problems with what each rule says when it fires, or how it is written, by check: facts and what was measured, not guesses (docs/design/content.md).\n# TYPE lapilli_rule_content_problems gauge\n")
			for _, p := range problems {
				fmt.Fprintf(&b, "lapilli_rule_content_problems{rule=\"%s\",check=\"%s\"} %d\n", labelValue(p.rule), p.check, p.n)
			}
		}
		c = cached{"text/plain; version=0.0.4; charset=utf-8", b.Bytes()}
		s.cache.put(tag, "/metrics", c)
	}
	w.Header().Set("Content-Type", c.ctype)
	w.Write(c.body)
	// When the deploys file was last read and kept: what a dashboard says
	// a job that writes it stopped by. Not a page's: it changes each run.
	if s.cfg.Deploys != "" {
		if at := s.deploysRead.Load(); at > 0 {
			fmt.Fprintf(w, "# HELP lapilli_serve_deploys_read_timestamp_seconds When the deploys file (--deploys) was last read, and what it held kept, in Unix seconds.\n# TYPE lapilli_serve_deploys_read_timestamp_seconds gauge\nlapilli_serve_deploys_read_timestamp_seconds %d\n", at)
		}
	}
	// Which replica leads: 1 for this one's pod, 0 for the others'. What
	// the leader's work counts (runs, notices sent, the files read) is in
	// its pod's metrics alone.
	if s.l.Shared() {
		lead := 0
		if s.l.Leading() {
			lead = 1
		}
		fmt.Fprintf(w, "# HELP lapilli_serve_leader Whether this replica leads, running what is run once among the replicas sharing the ledger: 1 or 0.\n# TYPE lapilli_serve_leader gauge\nlapilli_serve_leader %d\n", lead)
		fmt.Fprintf(w, "# HELP lapilli_serve_replicas The replicas of lapilli serve that share the ledger and live, this one among them, as last counted.\n# TYPE lapilli_serve_replicas gauge\nlapilli_serve_replicas %d\n", s.l.Replicas())
		waited := 0
		if s.l.WritesWaited() {
			waited = 1
		}
		fmt.Fprintf(w, "# HELP lapilli_serve_ledger_writes_waiting Whether this replica's last write gave up waiting for the ledger's writers' turn, which another session holds, and none has taken it since: 1 or 0.\n# TYPE lapilli_serve_ledger_writes_waiting gauge\nlapilli_serve_ledger_writes_waiting %d\n", waited)
		// How the leader's work goes: a leader whose step runs past
		// stepWithin gives its lease up.
		if at := s.rounded.Load(); lead == 1 && at > 0 {
			fmt.Fprintf(w, "# HELP lapilli_serve_jobs_round_timestamp_seconds When the leader last finished a round of its jobs, in Unix seconds.\n# TYPE lapilli_serve_jobs_round_timestamp_seconds gauge\nlapilli_serve_jobs_round_timestamp_seconds %d\n", at)
		}
	}
	// Read with the file each run, not kept with the ledger's pages: by
	// the replica that reads it.
	if s.cfg.Incidents != "" && (!s.l.Shared() || s.incidentsRead.Load()) {
		fmt.Fprintf(w, "# HELP lapilli_serve_incidents_passed_over Incidents in the incidents file whose ids the ledger holds from people, and so not read from it.\n# TYPE lapilli_serve_incidents_passed_over gauge\nlapilli_serve_incidents_passed_over %d\n", s.passedOver.Load())
	}
	if s.cfg.NotifyListen != "" && s.port != nil {
		// Taking a notification changes no page until it is kept: counted
		// here, not with them. Each source's, so its series is there at 0.
		s.status.Lock()
		refused := maps.Clone(s.status.refused)
		s.status.Unlock()
		var b bytes.Buffer
		b.WriteString("# HELP lapilli_serve_notifications_refused_total Notifications the notifications port was sent and couldn't keep, by source, since lapilli serve started: the rest of a request is kept, and why is logged.\n# TYPE lapilli_serve_notifications_refused_total counter\n")
		for _, src := range slices.Sorted(maps.Keys(s.port.rates)) {
			fmt.Fprintf(&b, "lapilli_serve_notifications_refused_total{source=%q} %d\n", src, refused[src])
		}
		w.Write(b.Bytes())
	}
	if len(s.sendsTo()) > 0 {
		// Sending a notice changes no page, so these aren't kept with them.
		unsent, err := s.l.Unsent(ctx)
		if err != nil {
			return
		}
		s.status.Lock()
		notices := maps.Clone(s.status.notices)
		s.status.Unlock()
		var b bytes.Buffer
		b.WriteString("# HELP lapilli_serve_notices_total Notices since lapilli serve started, by where to and whether they were sent.\n# TYPE lapilli_serve_notices_total counter\n")
		for _, k := range slices.Sorted(maps.Keys(notices)) {
			to, result, _ := strings.Cut(k, " ")
			fmt.Fprintf(&b, "lapilli_serve_notices_total{to=%q,result=%q} %d\n", to, result, notices[k])
		}
		fmt.Fprintf(&b, "# HELP lapilli_serve_notices_unsent Notices not sent yet, or given up on.\n# TYPE lapilli_serve_notices_unsent gauge\nlapilli_serve_notices_unsent %d\n", unsent)
		w.Write(b.Bytes())
	}
}

// maxReceiverSeries is how many receivers a rule's notifications are told
// apart by in /metrics; the rest are receiver="other".
const maxReceiverSeries = 10

func unixOr0(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// labelValue escapes a label's value as Prometheus's text format does:
// backslash, double quote and line feed; and a control character or a
// direction mark, embedding, override or isolate, which a rule's name from
// an API may hold, as the pages and lapilli verdict say it
// (verdict.LabelValue). The joiners names are written with are as they
// are.
func labelValue(v string) string { return verdict.LabelValue(v) }

type contentProblem struct {
	rule, check string
	n           int
}

// contentProblems are how many problems of each check each rule has, the
// guesses left out, in order of rule and check.
func contentProblems(cf map[string][]contentFinding) []contentProblem {
	var out []contentProblem
	for _, name := range slices.Sorted(maps.Keys(cf)) {
		by := map[string]int{}
		for _, f := range cf[name] {
			if f.Class != content.Heuristic {
				by[f.Kind]++
			}
		}
		for _, check := range content.Checks {
			if by[check] > 0 {
				out = append(out, contentProblem{name, check, by[check]})
			}
		}
	}
	return out
}
