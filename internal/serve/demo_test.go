package serve

// A demo ledger for the README's screenshots (test/screenshots): twelve
// weeks of three teams' alerts, with names anyone might give them, made
// the same each time from a fixed clock and seed. TestDemoServe serves it.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The demo's names, every one: its teams (each a group of its rules,
// and the label team its alerts have), the people who keep its notes,
// tasks, comments and proposals, and the pods its platform's alerts name.
// With demoRules' names, services and severities, and demoIncidents'
// services, they are all a page of the demo can show: TestDemoLedger
// holds the ledger to them.
var (
	demoTeams  = []string{"checkout", "platform", "search"}
	demoPeople = []string{"ana", "sam", "kim"}
	demoPods   = []string{"api-7c9f", "worker-5d2b", "web-9a41", "cron-1e07"}
)

// demoRule is a rule of the demo: its group (each a team's), what it is
// about, how often it fires for nothing a day (from a day on, and as often
// before it, Before), and what it says.
type demoRule struct {
	name, group, service, severity string
	expr, forDur                   string
	noise, before                  float64
	from                           int
	summary, runbook               bool
}

// demoRules are the demo's rules: some noisy, some precise, one spare,
// one whose replay differs, some that never fire.
var demoRules = []demoRule{
	{"CheckoutLatencyP99", "checkout", "checkout", "warning", `histogram_quantile(0.99, sum by (le, service) (rate(http_request_duration_seconds_bucket{service="checkout"}[5m]))) > %s`, "%s", 5, 0.4, 44, true, true},
	{"CheckoutErrors", "checkout", "checkout", "critical", `sum by (service) (rate(http_requests_total{service="checkout",code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total{service="checkout"}[5m])) > 0.02`, "2m", 0, 0, 0, true, true},
	{"CheckoutErrorsBudgetBurn", "checkout", "checkout", "critical", `slo:error_budget_burn_rate:1h{service="checkout"} > 14.4`, "5m", 0, 0, 0, true, false},
	{"CheckoutQueueBacklog", "checkout", "checkout", "warning", `sum(queue_depth{queue="orders"}) > 500`, "10m", 0.8, 0.8, 0, true, false},
	{"CartLatencyHigh", "checkout", "cart", "critical", `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="cart"}[5m]))) > 1.5`, "5m", 0, 0, 0, true, true},
	{"CartRedisMemory", "checkout", "cart", "warning", `redis_memory_used_bytes{service="cart"} / redis_memory_max_bytes > 0.9`, "15m", 0, 0, 0, true, true},
	{"PaymentsDown", "checkout", "payments", "critical", `up{job="payments"} == 0`, "1m", 0, 0, 0, true, true},
	{"PaymentsLatency", "checkout", "payments", "warning", `histogram_quantile(0.99, sum by (le) (rate(payment_request_seconds_bucket[5m]))) > 3`, "10m", 0.1, 0.1, 0, true, true},
	{"PromoServiceErrors", "checkout", "promo", "warning", `sum(rate(http_requests_total{service="promo",code=~"5.."}[5m])) > 1`, "5m", 0.15, 0.15, 0, true, true},
	{"CheckoutTrafficDrop", "checkout", "checkout", "critical", `sum(rate(http_requests_total{service="checkout"}[10m])) < 5`, "10m", 0, 0, 0, true, true},
	{"PodCrashLooping", "platform", "kubernetes", "warning", `increase(kube_pod_container_status_restarts_total[15m]) > 3`, "5m", 4, 4, 0, true, true},
	{"NodeNotReady", "platform", "kubernetes", "critical", `kube_node_status_condition{condition="Ready",status="true"} == 0`, "5m", 0, 0, 0, true, true},
	{"DiskWillFill", "platform", "database", "warning", `predict_linear(node_filesystem_avail_bytes[6h], 24 * 3600) < 0`, "%s", 0.05, 0.05, 0, true, true},
	{"HighCPU", "platform", "kubernetes", "warning", `avg by (instance) (rate(node_cpu_seconds_total{mode!="idle"}[5m])) > 0.9`, "10m", 3, 3, 0, false, true},
	{"HighMemory", "platform", "kubernetes", "warning", `node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes < 0.1`, "10m", 0.7, 0.7, 0, true, true},
	{"ContainerOOMKilled", "platform", "kubernetes", "warning", `increase(kube_pod_container_status_restarts_total{reason="OOMKilled"}[10m]) > 0`, "0s", 2, 2, 0, true, true},
	{"DeploymentReplicasMismatch", "platform", "kubernetes", "warning", `kube_deployment_spec_replicas != kube_deployment_status_replicas_available`, "15m", 1.2, 1.2, 0, true, true},
	{"KubeAPIServerLatency", "platform", "kubernetes", "warning", `histogram_quantile(0.99, sum by (le) (rate(apiserver_request_duration_seconds_bucket[5m]))) > 1`, "10m", 0.2, 0.2, 0, true, true},
	{"EtcdNoLeader", "platform", "kubernetes", "critical", `etcd_server_has_leader == 0`, "1m", 0, 0, 0, true, true},
	{"CertificateExpiring", "platform", "kubernetes", "warning", `certmanager_certificate_expiration_timestamp_seconds - time() < 7 * 86400`, "1h", 0, 0, 0, true, true},
	{"NodeClockSkew", "platform", "kubernetes", "warning", `abs(node_timex_offset_seconds) > 0.05`, "10m", 0.1, 0.1, 0, true, true},
	{"PersistentVolumeFull", "platform", "database", "warning", `kubelet_volume_stats_available_bytes / kubelet_volume_stats_capacity_bytes < 0.05`, "5m", 0.05, 0.05, 0, true, true},
	{"JobFailed", "platform", "kubernetes", "warning", `kube_job_status_failed > 0`, "0s", 0.5, 0.5, 0, true, true},
	{"APIGateway5xx", "platform", "api-gateway", "critical", `sum(rate(gateway_requests_total{code=~"5.."}[5m])) / sum(rate(gateway_requests_total[5m])) > 0.05`, "2m", 0, 0, 0, true, true},
	{"APIGatewayLatency", "platform", "api-gateway", "warning", `histogram_quantile(0.99, sum by (le) (rate(gateway_request_seconds_bucket[5m]))) > 0.75`, "2m", 2.5, 2.5, 0, true, true},
	{"DNSErrors", "platform", "dns", "critical", `sum(rate(coredns_dns_responses_total{rcode="SERVFAIL"}[5m])) > 10`, "5m", 0, 0, 0, true, true},
	{"SearchIndexLag", "search", "search", "warning", `max(search_index_lag_seconds) > 300`, "5m", 2, 2, 0, true, true},
	{"SearchErrors", "search", "search", "critical", `sum(rate(search_requests_total{code=~"5.."}[5m])) / sum(rate(search_requests_total[5m])) > 0.05`, "5m", 0, 0, 0, true, true},
	{"SearchLatencyP95", "search", "search", "warning", `histogram_quantile(0.95, sum by (le) (rate(search_request_seconds_bucket[5m]))) > 0.8`, "10m", 0.2, 0.2, 0, true, true},
	{"SearchNodeDown", "search", "search", "critical", `up{job="search-node"} == 0`, "2m", 0, 0, 0, true, true},
	{"IndexerQueueBacklog", "search", "indexer", "warning", `sum(indexer_queue_depth) > 10000`, "15m", 0.1, 0.1, 0, true, true},
	{"IndexerCrashLooping", "search", "indexer", "warning", `increase(kube_pod_container_status_restarts_total{container="indexer"}[15m]) > 3`, "5m", 1.5, 1.5, 0, true, true},
}

// demoIncident is an incident of the demo: its day of the strip, hour and
// minute, what it was about and its title; and the rules that caught it,
// each so many minutes from its start.
type demoIncident struct {
	day, hour, minute int
	service, title    string
	caught            map[string]int
}

var demoIncidents = []demoIncident{
	{9, 14, 10, "checkout", "Checkout errors after a release", map[string]int{"CheckoutErrors": 2, "CheckoutErrorsBudgetBurn": 6}},
	{20, 3, 40, "payments", "Payment provider timeouts", map[string]int{"PaymentsDown": 3}},
	{27, 10, 5, "search", "Stale search results", map[string]int{"SearchIndexLag": 12}},
	{33, 16, 20, "kubernetes", "A node pool lost a zone", map[string]int{"NodeNotReady": 4}},
	{41, 22, 30, "dns", "DNS resolution failures", nil},
	{48, 13, 0, "cart", "Cart pages slow", map[string]int{"CartLatencyHigh": 4, "CheckoutLatencyP99": 1}},
	{56, 9, 15, "checkout", "Checkout errors", map[string]int{"CheckoutErrors": 1, "CheckoutErrorsBudgetBurn": 5}},
	{63, 15, 45, "api-gateway", "Gateway 5xx spike", map[string]int{"APIGateway5xx": 2}},
	{70, 4, 20, "database", "Database disk full", map[string]int{"DiskWillFill": -8}},
	{77, 11, 30, "search", "Search unavailable", nil},
	{81, 8, 10, "checkout", "Checkout latency", map[string]int{"CheckoutLatencyP99": 2}},
}

// demoFile is the demo's rules as they stood: version 0 at first, 1 from
// the day CheckoutLatencyP99's threshold was lowered, 2 from the day
// DiskWillFill was given longer.
func demoFile(version int) string {
	var b strings.Builder
	b.WriteString("groups:\n")
	for _, g := range demoTeams {
		fmt.Fprintf(&b, "  - name: %s-alerts\n    labels: {team: %s}\n    rules:\n", g, g)
		for _, r := range demoRules {
			if r.group != g {
				continue
			}
			expr, forDur := r.expr, r.forDur
			switch r.name {
			case "CheckoutLatencyP99":
				expr, forDur = fmt.Sprintf(expr, map[bool]string{true: "0.5", false: "0.8"}[version >= 1]), map[bool]string{true: "2m", false: "5m"}[version >= 1]
			case "DiskWillFill":
				forDur = map[bool]string{true: "30m", false: "10m"}[version >= 2]
			}
			fmt.Fprintf(&b, "      - alert: %s\n        expr: %q\n        for: %s\n        labels: {severity: %s}\n", r.name, expr, forDur, r.severity)
			if r.summary || r.runbook {
				b.WriteString("        annotations:\n")
				if r.summary {
					fmt.Fprintf(&b, "          summary: %q\n", "{{ $labels.service }}: "+strings.ToLower(splitName(r.name)))
				}
				if r.runbook {
					fmt.Fprintf(&b, "          runbook_url: https://runbooks.example.com/%s/%s\n", r.group, strings.ToLower(r.name))
				}
			}
		}
	}
	return b.String()
}

// splitName is a rule's name in words: CheckoutLatencyP99, "Checkout
// latency p99".
func splitName(name string) string {
	var out []rune
	for i, c := range name {
		if i > 0 && c >= 'A' && c <= 'Z' && (name[i-1] < 'A' || name[i-1] > 'Z') {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	return string(out)
}

// demoServer is a server with the demo ledger.
func demoServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	s, dir := server(t, func(c *Config) {
		c.CheckReplay, c.CheckContent, c.Version = true, true, ""
		c.Options.Hours = verdict.DefaultHours(time.UTC)
		c.Options.DeployAfter = 10 * time.Minute
	})
	s.h = &alertsHistory{}
	r := rand.New(rand.NewPCG(2026, 1103))
	today := s.l.Day(now)
	first := s.l.Midnight(today - stripDays + 1)
	day := func(d int) time.Time { return first.Add(time.Duration(d) * 24 * time.Hour) }

	// The rules, as they stood, each version from its day.
	for v, at := range []time.Time{first.Add(-24 * time.Hour), day(44).Add(11 * time.Hour), day(70).Add(15 * time.Hour)} {
		os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(demoFile(v)), 0o644)
		s.now = func() time.Time { return at }
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
	}
	s.now = func() time.Time { return now }
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	o := s.cfg.Options

	// The teams, each owning its group's rules.
	team := map[string]int64{}
	for i, rules := range []string{"^(Checkout|Cart|Payments|Promo)", "^(Pod|Node|Disk|High|Container|Deployment|Kube|Etcd|Certificate|Persistent|Job|APIGateway|DNS)",
		"^(Search|Indexer)"} {
		tm := ledger.Team{Name: demoTeams[i], Rules: rules}
		id, err := s.l.PutTeam(ctx, tm, "ana")
		if err != nil {
			t.Fatal(err)
		}
		team[tm.Name] = id
	}

	// The incidents, and the firings that caught them.
	var incs []incident.Incident
	caught := map[int][]firing.Firing{}
	for i, x := range demoIncidents {
		start := day(x.day).Add(time.Duration(x.hour)*time.Hour + time.Duration(x.minute)*time.Minute)
		incs = append(incs, incident.Incident{ID: fmt.Sprint("INC-", 101+i), Title: x.title, Start: start, End: start.Add(40 * time.Minute),
			Labels: incident.Labels{"service": {x.service}}})
		for rule, after := range x.caught {
			at := start.Add(time.Duration(after) * time.Minute)
			caught[x.day] = append(caught[x.day], firing.Firing{Rule: rule, Labels: map[string]string{"service": x.service}, Start: at, End: at.Add(25 * time.Minute)})
		}
	}
	if _, err := s.l.SyncFile(ctx, incs, o); err != nil {
		t.Fatal(err)
	}

	// Each day's firings: the noise, those that caught the incidents, and,
	// on Saturdays, the platform's during its weekly maintenance.
	pods := demoPods
	var latency []firing.Firing // CheckoutLatencyP99's, to notify
	for d := 0; d < stripDays; d++ {
		var fs []firing.Firing
		for _, rl := range demoRules {
			rate := rl.noise
			if d < rl.from {
				rate = rl.before
			}
			n := int(rate)
			if r.Float64() < rate-float64(n) {
				n++
			}
			for k := 0; k < n; k++ {
				at := day(d).Add(time.Duration(r.IntN(24*60)) * time.Minute)
				labels := map[string]string{"service": rl.service}
				if rl.service == "kubernetes" {
					labels["pod"] = pods[r.IntN(len(pods))]
				}
				f := firing.Firing{Rule: rl.name, Labels: labels, Start: at, End: at.Add(time.Duration(3+r.IntN(22)) * time.Minute)}
				fs = append(fs, f)
				if rl.name == "CheckoutLatencyP99" {
					latency = append(latency, f)
				}
			}
		}
		// The checkout deploys, every third day at 13:30, which its latency
		// alert fires during once it was lowered.
		if d%3 == 0 && d >= 44 {
			at := day(d).Add(13*time.Hour + 34*time.Minute)
			fs = append(fs, firing.Firing{Rule: "CheckoutLatencyP99", Labels: map[string]string{"service": "checkout"}, Start: at, End: at.Add(9 * time.Minute)})
		}
		if day(d).Weekday() == time.Saturday {
			for i, rule := range []string{"PodCrashLooping", "HighCPU", "ContainerOOMKilled"} {
				at := day(d).Add(time.Hour + time.Duration(10+20*i)*time.Minute)
				fs = append(fs, firing.Firing{Rule: rule, Labels: map[string]string{"service": "kubernetes", "pod": pods[i]}, Start: at, End: at.Add(12 * time.Minute)})
			}
		}
		// NodeNotReady, the last six days, once a day.
		if d >= stripDays-6 {
			at := day(d).Add(time.Duration(6+r.IntN(12)) * time.Hour)
			fs = append(fs, firing.Firing{Rule: "NodeNotReady", Labels: map[string]string{"service": "kubernetes", "pod": pods[r.IntN(len(pods))]}, Start: at, End: at.Add(7 * time.Minute)})
		}
		fs = append(fs, caught[d]...)
		end := day(d).Add(24*time.Hour - time.Second)
		if d == stripDays-1 {
			end = now
			fs = slicesBefore(fs, now)
		}
		if err := s.l.Record(ctx, ledger.Found{From: day(d), To: end, Firings: fs,
			Run: ledger.Run{Kind: "backfill", Started: day(d), Finished: day(d), From: day(d), To: end}}, o); err != nil {
			t.Fatal(err)
		}
	}

	// Deploys of checkout every third day, search's every fourth, and the
	// platform's every week.
	var ds []ledger.Deploy
	for d := 0; d < stripDays; d++ {
		if d%3 == 0 {
			ds = append(ds, ledger.Deploy{Start: day(d).Add(13*time.Hour + 30*time.Minute), End: day(d).Add(13*time.Hour + 40*time.Minute),
				Labels: map[string]string{"service": "checkout"}, Revision: fmt.Sprintf("%07x", r.Uint32()>>4)})
		}
		if d%4 == 1 {
			ds = append(ds, ledger.Deploy{Start: day(d).Add(11 * time.Hour), End: day(d).Add(11*time.Hour + 8*time.Minute), Labels: map[string]string{"service": "search"}})
		}
		if day(d).Weekday() == time.Wednesday {
			ds = append(ds, ledger.Deploy{Start: day(d).Add(10 * time.Hour), End: day(d).Add(10*time.Hour + 20*time.Minute), Labels: map[string]string{"service": "kubernetes"}})
		}
	}
	if _, err := s.l.SyncDeploys(ctx, ds, o, ledger.DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	// The platform's weekly maintenance, Saturdays 01:00 to 03:00.
	sat := first.Add(-7 * 24 * time.Hour)
	for sat.Weekday() != time.Saturday {
		sat = sat.Add(24 * time.Hour)
	}
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "Weekly node maintenance", Start: sat.Add(time.Hour), End: sat.Add(3 * time.Hour), Every: "week",
		TeamID: team["platform"]}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}

	// The notifications CheckoutLatencyP99's firings sent the last three
	// weeks, each once, and some again four hours on.
	for i, f := range latency {
		if f.Start.Before(now.Add(-21*24*time.Hour)) || f.Start.After(now) {
			continue
		}
		for k, at := range []time.Time{f.Start.Add(30 * time.Second), f.Start.Add(4 * time.Hour)} {
			if k == 1 && i%3 != 0 || at.After(now) {
				continue
			}
			if _, err := s.l.Notify(ctx, ledger.Notification{Source: "alertmanager", Receiver: "checkout-oncall", Group: fmt.Sprint("checkout-", i), At: at,
				Alerts: []ledger.Alert{{Rule: f.Rule, Labels: f.Labels, Start: f.Start}}}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The replay beside what really fired, the last two weeks: alike, but
	// for NodeNotReady, which really fired more than its replay did.
	_, seq, err := s.l.AgainFrom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for d := stripDays - 14; d < stripDays-1; d++ {
		fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: day(d), To: day(d + 1), Limit: 10000})
		if err != nil {
			t.Fatal(err)
		}
		c := &verdict.SpanCheck{Rules: map[string]*verdict.SpanRule{}}
		for _, rl := range demoRules {
			c.Rules[rl.name] = &verdict.SpanRule{}
		}
		for _, f := range fs {
			x := c.Rules[f.Rule]
			x.Replayed++
			x.Real++
			x.Alike++
			x.OffSum += 20 * time.Second
		}
		if x := c.Rules["NodeNotReady"]; d >= stripDays-6 {
			x.Real += 2
		}
		if err := s.l.PutCheck(ctx, day(d), c, nil, day(d+1), seq); err != nil {
			t.Fatal(err)
		}
	}

	// What the digest worked out: a longer for for the noisy ones, and a
	// rule that nearly caught an incident nothing caught.
	week := s.l.Midnight(ledger.Week(today))
	if err := s.l.AddResult(ctx, "suggestions", now, week.Add(-7*24*time.Hour), week, []verdict.Suggestion{
		{Rule: "CheckoutLatencyP99", For: 2 * time.Minute, NewFor: 30 * time.Minute, Before: verdict.RuleResult{Firings: 41}, After: verdict.RuleResult{Firings: 9}, Incidents: 2},
		{Rule: "PodCrashLooping", For: 5 * time.Minute, NewFor: 15 * time.Minute, Before: verdict.RuleResult{Firings: 30}, After: verdict.RuleResult{Firings: 12}},
	}); err != nil {
		t.Fatal(err)
	}
	missed := incs[9]
	mw := s.l.Midnight(ledger.Week(s.l.Day(missed.Start)))
	if err := s.l.ReplaceResult(ctx, "near_misses", now, mw, mw.Add(7*24*time.Hour), []verdict.NearMiss{{Incident: missed.ID, Rule: "SearchErrors",
		Labels: map[string]string{"service": "search"}, From: 2 * time.Minute, Held: 4 * time.Minute, For: 5 * time.Minute, NewFor: 3 * time.Minute, Delay: 5 * time.Minute}}); err != nil {
		t.Fatal(err)
	}

	// What people keep: notes, a proposal waiting for its approval, tasks,
	// comments.
	for _, n := range []ledger.Note{
		{Rule: "CheckoutLatencyP99", Status: "tune", Note: "Its threshold went from 0.8 s to 0.5 s in September; it has paged most nights since. Look at the for first."},
		{Rule: "NodeNotReady", Status: "keep"}, {Rule: "CheckoutErrors", Status: "keep"},
		{Rule: "CheckoutErrorsBudgetBurn", Status: "retire", Note: "CheckoutErrors catches the same, sooner."},
		{Rule: "PodCrashLooping", Status: "tune"},
	} {
		if err := s.l.PutNote(ctx, n, "sam"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.propose(ctx, ask{Rule: "PodCrashLooping", Kind: ledger.KindFor, For: 15 * time.Minute, From: "suggestion"}, "sam", false, true); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []ledger.Task{
		{Title: `Quiet CheckoutLatencyP99: "for" 2m → 30m`, Kind: "tune", Rule: "CheckoutLatencyP99", TeamID: team["checkout"], Assignee: "sam", Due: now.Add(4 * 24 * time.Hour)},
		{Title: "Write a runbook for CheckoutQueueBacklog", Kind: "write", Rule: "CheckoutQueueBacklog", TeamID: team["checkout"], Due: now.Add(9 * 24 * time.Hour)},
		{Title: "Decide on SearchIndexLag", Kind: "decide", Rule: "SearchIndexLag", TeamID: team["search"], Assignee: "kim", Status: "doing"},
		{Title: "Why did nothing catch the DNS failures?", Kind: "write", Incident: "INC-105", TeamID: team["platform"], Status: "open"},
	} {
		if _, err := s.l.PutTask(ctx, tk, "sam"); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []ledger.Comment{
		{On: "rule", Key: "CheckoutLatencyP99", Body: "Most of these come during the 13:30 deploys. A longer for keeps the cart incident."},
		{On: "rule", Key: "CheckoutLatencyP99", Body: "Agreed: propose 30m, and look again in two weeks."},
		{On: "task", Key: "1", Body: "The backtest says 41 → 9 firings, both incidents still caught."},
	} {
		if _, err := s.l.PutComment(ctx, c, "kim"); err != nil {
			t.Fatal(err)
		}
	}
	// What people kept is dated by the clock it was written at: dated in
	// the demo's own weeks instead, in the order it was written, its pages
	// are the same however late they are made.
	at := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	for _, x := range []struct {
		q    string
		args []any
	}{
		{"UPDATE teams SET updated = ?", []any{first.Add(-7 * 24 * time.Hour).Unix()}},
		{"UPDATE windows SET updated = ?", []any{first.Add(-7 * 24 * time.Hour).Unix()}},
		{"UPDATE notes SET updated = ?", []any{at(5 * 24 * time.Hour)}},
		{"UPDATE proposals SET made = ?, updated = ?", []any{at(30 * time.Hour), at(30 * time.Hour)}},
		{"UPDATE tasks SET created = ? + id * 3600, updated = ? + id * 3600", []any{at(50 * time.Hour), at(50 * time.Hour)}},
		{"UPDATE comments SET created = ? + id * 3600, updated = ? + id * 3600", []any{at(26 * time.Hour), at(26 * time.Hour)}},
		{"UPDATE changes SET at = ? + id WHERE at > ?", []any{at(72 * time.Hour), now.Unix()}},
	} {
		ledgerExec(t, s, x.q, x.args...)
	}
	// Read again, as after any change.
	if err := s.l.SetMeta(ctx, "demo-dated", "1"); err != nil {
		t.Fatal(err)
	}
	return s
}

// slicesBefore are the firings that started before t.
func slicesBefore(fs []firing.Firing, t time.Time) []firing.Firing {
	var out []firing.Firing
	for _, f := range fs {
		if f.Start.Before(t) {
			if f.End.After(t) {
				f.End = time.Time{}
			}
			out = append(out, f)
		}
	}
	return out
}

// TestDemoServe serves the demo ledger at LAPILLI_DEMO_SERVE, on loopback,
// until it is stopped, for test/screenshots/shots.sh, which waits for
// /demo-ready: only the demo answers it, with demoReady.
func TestDemoServe(t *testing.T) {
	addr := serveAddr(t, "LAPILLI_DEMO_SERVE")
	s := demoServer(t)
	s.cfg.AllowedHosts = append(s.cfg.AllowedHosts, "host.docker.internal")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /demo-ready", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, demoReady) })
	mux.Handle("/", s.Handler())
	serveTest(t, s, addr, mux)
}

// demoReady is what the demo answers at /demo-ready.
const demoReady = "lapilli demo ledger, ready"

// TestDemoLedger: the demo has what its screenshots are to show: a noisy
// rule, precise ones, a spare one, one whose replay differs, silent ones;
// incidents caught, missed and caught only by noisy rules; a proposal to
// approve. Every team, rule, label value and person in it is one the demo
// declares; and what people kept is dated in its own weeks.
func TestDemoLedger(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	s := demoServer(t)
	v := rulesFor(t, s, "weeks=12")
	perf := map[string]string{}
	for _, row := range append(v.Rows, v.Quiet...) {
		perf[row.Name] = row.Perf
		if row.Name == "NodeNotReady" && !slicesHas(row.Trust, "differs") {
			t.Errorf("NodeNotReady doesn't differ: %q", row.Trust)
		}
	}
	for rule, want := range map[string]string{"CheckoutLatencyP99": "noisy", "PodCrashLooping": "noisy", "CheckoutErrors": "ok",
		"CheckoutErrorsBudgetBurn": "spare", "EtcdNoLeader": "silent", "NodeNotReady": "ok"} {
		if perf[rule] != want {
			t.Errorf("%s: %s, want %s", rule, perf[rule], want)
		}
	}
	if len(perf) < 30 || len(perf) > 60 {
		t.Errorf("%d rules", len(perf))
	}
	_, body, _ := get(t, s.Handler(), "/incidents?weeks=12")
	for _, want := range []string{"✗ nothing fired", "◐ SearchIndexLag", ", noisy", "DNS resolution failures", "nearly caught by SearchErrors"} {
		if !strings.Contains(body, want) {
			t.Errorf("/incidents: no %q", want)
		}
	}
	_, body, _ = get(t, s.Handler(), "/rules/CheckoutLatencyP99?weeks=4")
	for _, want := range []string{"Noisy, ", "Beside each number", "the rule changed", "fired during a deploy of what it is about", `class="delta`} {
		if !strings.Contains(body, want) {
			t.Errorf("/rules/CheckoutLatencyP99: no %q", want)
		}
	}
	if _, approve := s.waiting(context.Background()); approve != 1 {
		t.Errorf("%d proposals to approve", approve)
	}
	demoClosed(t, s)
}

// demoClosed: every team, rule, label value and person in the demo's
// ledger is one it declares, and every time people wrote something in it
// is in its own weeks, by its clock, not the one it was made at.
func demoClosed(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	known := map[string]map[string]bool{"team": {}, "rule": {}, "label value": {}, "person": {}}
	for _, x := range demoTeams {
		known["team"][x], known["label value"][x] = true, true
	}
	for _, x := range demoPeople {
		known["person"][x] = true
	}
	for _, x := range demoPods {
		known["label value"][x] = true
	}
	for _, r := range demoRules {
		known["rule"][r.name], known["label value"][r.service], known["label value"][r.severity] = true, true, true
	}
	for _, in := range demoIncidents {
		known["label value"][in.service] = true
	}
	db := ledgerDB(t, s)
	defer db.Close()
	each := func(what, q string, f func(string) []string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			for _, x := range f(v) {
				n++
				if !known[what][x] {
					t.Errorf("%s: %s %q isn't one the demo declares", q, what, x)
				}
			}
		}
		if n == 0 {
			t.Errorf("%s: none", q)
		}
	}
	one := func(v string) []string { return []string{v} }
	values := func(v string) []string {
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		var out []string
		for _, x := range m {
			switch x := x.(type) {
			case string:
				out = append(out, x)
			case []any:
				for _, y := range x {
					out = append(out, fmt.Sprint(y))
				}
			}
		}
		return out
	}
	each("team", "SELECT name FROM teams", one)
	each("rule", "SELECT name FROM rules", one)
	for _, q := range []string{"SELECT labels FROM series", "SELECT labels FROM incidents", "SELECT labels FROM deploy_labels"} {
		each("label value", q, values)
	}
	for _, q := range []string{"SELECT DISTINCT by FROM changes", "SELECT updated_by FROM notes", "SELECT created_by FROM tasks",
		"SELECT assignee FROM tasks WHERE assignee != ''", "SELECT created_by FROM comments", "SELECT made_by FROM proposals",
		"SELECT updated_by FROM windows", "SELECT updated_by FROM teams"} {
		each("person", q, one)
	}
	// The rules' own labels, as the rules in effect have them.
	latest, ok, err := s.l.Latest(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	rs, err := s.parse(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		for k, v := range r.AlertLabels() {
			if !known["label value"][v] {
				t.Errorf("%s: its label %s=%q isn't one the demo declares", r.Name, k, v)
			}
		}
	}
	for _, q := range []string{"SELECT count(*) FROM notes WHERE updated > ?", "SELECT count(*) FROM tasks WHERE created > ? OR updated > ?",
		"SELECT count(*) FROM comments WHERE created > ? OR updated > ?", "SELECT count(*) FROM proposals WHERE made > ? OR updated > ?",
		"SELECT count(*) FROM teams WHERE updated > ?", "SELECT count(*) FROM windows WHERE updated > ?", "SELECT count(*) FROM changes WHERE at > ?"} {
		args := []any{now.Unix()}
		if strings.Contains(q, " OR ") {
			args = append(args, now.Unix())
		}
		if s.cfg.LedgerURLFile != "" {
			k := 0
			q = replaceEach(q, "?", func() string { k++; return fmt.Sprint("$", k) })
		}
		var n int
		if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d after the demo's now (%v)", q, n, err)
		}
	}
}

func slicesHas(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
