package serve

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

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
		sum := map[int64]*verdict.RuleResult{}
		for _, d := range days {
			rr := sum[d.Rule]
			if rr == nil {
				rr = &verdict.RuleResult{Rule: table[d.Rule].Name, Severity: table[d.Rule].Severity}
				sum[d.Rule] = rr
			}
			rr.Firings += d.Firings
			rr.White += d.White
			rr.Black += d.Black()
			rr.Refired += d.Refired
		}
		rep := &verdict.Report{}
		noisyRule := map[string]bool{}
		for _, rr := range sum {
			rr.Noisy = noisy(rr.Firings, rr.White, o)
			noisyRule[rr.Rule] = rr.Noisy
			rep.Rules = append(rep.Rules, *rr)
		}
		ledger.SortRules(rep.Rules)
		incs, err := s.l.Incidents(ctx, s.l.Midnight(today-6), s.now().Add(time.Hour), false)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		for _, in := range incs {
			ir := verdict.IncidentResult{Incident: in.Incident, CaughtBy: slices.Clone(in.CaughtBy)}
			for i := range ir.CaughtBy {
				ir.CaughtBy[i].Noisy = noisyRule[ir.CaughtBy[i].Rule]
			}
			rep.Incidents = append(rep.Incidents, ir)
		}
		var last time.Time
		if runs, err := s.l.Runs(ctx, 1); err == nil && len(runs) > 0 {
			last = runs[0].Finished
		}
		var b bytes.Buffer
		if err := verdict.Prometheus(&b, rep, last); err != nil {
			s.oops(w, r, err)
			return
		}
		cursor, err := s.l.Cursor(ctx)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		fmt.Fprintf(&b, "# HELP lapilli_serve_cursor_timestamp_seconds How far the ledger has firings up to, in Unix seconds.\n# TYPE lapilli_serve_cursor_timestamp_seconds gauge\nlapilli_serve_cursor_timestamp_seconds %d\n", unixOr0(cursor))
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
		c = cached{"text/plain; version=0.0.4; charset=utf-8", b.Bytes()}
		s.cache.put(tag, "/metrics", c)
	}
	w.Header().Set("Content-Type", c.ctype)
	w.Write(c.body)
	if len(s.dests()) > 0 {
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

func unixOr0(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
