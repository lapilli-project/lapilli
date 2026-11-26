package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

const impactUsage = `lapilli impact suggest says which of the stacks in front of services that
docs/impact.md lists have their metrics in the history, and how many
windows of user impact each one's --impact finds over the period: an
--impact to choose from, for a team with no incidents written down. It
chooses none, and writes and sends nothing: it reads
(docs/design/suggest.md).

Usage:
  lapilli impact suggest --prometheus URL [flags]

Where the history is, as lapilli verdict takes it:
  --prometheus URL    a Prometheus-compatible API: Prometheus, Thanos, Mimir,
                      VictoriaMetrics, or a cloud's managed Prometheus
  --auth CLOUD        send the credentials that cloud's managed Prometheus
                      wants (aws, azure or google), from those the machine
                      or pod already has
  --aws-region REGION the region to sign for, when the URL doesn't say
  --google-project ID    the same as --prometheus with Google Cloud Managed
                      Service for Prometheus's URL for this project and
                      --auth google
  --prometheus-token-file FILE
                      send this token with each query
  --prometheus-tenant ID
                      the tenant of a Mimir or a Cortex with tenants and no
                      gateway to set it, sent as X-Scope-OrgID
  --max-rate N        ask the API N requests a second at most, from 0.001
  --max-samples N     read the answers to one query to N samples in all at
                      most (default 10000000)

When, as lapilli verdict takes it:
  --since DUR         the last DUR (default 7d)
  --from TIME, --to TIME
                      an RFC 3339 period instead
  --step DUR          the step the windows are found at (default 1m)
  --tz ZONE           show the period in this zone

Output:
  --json              the same, as JSON
  --timeout DUR       give up after this long (default 10m)

For each stack, one request counts its metric's series, as its --impact
reads every request (not only those that failed), at an hour's step, or
coarser over a long period: the most at once. For a stack found, its --impact is read over the period, a day a request, and
its windows found as lapilli verdict --impact finds them, with
--impact-for 5m. The stacks are listed in docs/impact.md's order,
whatever they found. A stack that couldn't be asked is not known, with
why: a query the history refused as written, that stack alone; a history
not reached, not let in or turned away, or the timeout, every stack from
it on, unasked. It exits 0 when every stack was asked, 1 when one wasn't,
and 2 when the flags are wrong.
`

// impactStack is a stack in front of services, as docs/impact.md's table
// lists it: its name there, the metrics its --impact reads, and the
// --impact. TestImpactStacksDocumented holds the table and impactStacks
// alike.
type impactStack struct {
	// ID names it in --json, and stays.
	ID      string
	Name    string
	Metrics []string
	Impact  string
}

var impactStacks = []impactStack{
	{"istio", "Istio", []string{"istio_requests_total"},
		`sum by (destination_service_name) (rate(istio_requests_total{reporter="destination", response_code=~"5.."}[5m])) / sum by (destination_service_name) (rate(istio_requests_total{reporter="destination"}[5m])) > 0.01`},
	{"linkerd", "Linkerd", []string{"response_total"},
		`sum by (deployment) (rate(response_total{direction="inbound", classification="failure"}[5m])) / sum by (deployment) (rate(response_total{direction="inbound"}[5m])) > 0.01`},
	{"envoy", "Envoy or Envoy Gateway", []string{"envoy_cluster_upstream_rq_xx"},
		`sum by (envoy_cluster_name) (rate(envoy_cluster_upstream_rq_xx{envoy_response_code_class="5"}[5m])) / sum by (envoy_cluster_name) (rate(envoy_cluster_upstream_rq_xx[5m])) > 0.01`},
	{"ingress-nginx", "ingress-nginx", []string{"nginx_ingress_controller_requests"},
		`sum by (ingress) (rate(nginx_ingress_controller_requests{status=~"5.."}[5m])) / sum by (ingress) (rate(nginx_ingress_controller_requests[5m])) > 0.01`},
	{"traefik", "Traefik", []string{"traefik_service_requests_total"},
		`sum by (service) (rate(traefik_service_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(traefik_service_requests_total[5m])) > 0.01`},
	{"otel-http", "OpenTelemetry HTTP server metrics", []string{"http_server_request_duration_seconds_count"},
		`sum by (service_name) (rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m])) / sum by (service_name) (rate(http_server_request_duration_seconds_count[5m])) > 0.01`},
}

// counted is the selector of metric that s's --impact reads every request
// by: of those naming it, the shortest, which the others narrow with a
// matcher of failures more (TestImpactStacksDocumented).
func (s impactStack) counted(metric string) (string, error) {
	sels, err := rules.Selectors(s.Impact)
	if err != nil {
		return "", err
	}
	best := ""
	for _, sel := range sels {
		if name, _, _ := strings.Cut(sel, "{"); name == metric && (best == "" || len(sel) < len(best)) {
			best = sel
		}
	}
	if best == "" {
		return "", fmt.Errorf("%s's --impact reads no %s", s.Name, metric)
	}
	return best, nil
}

// dependsStack is a stack that counts calls between services, as
// docs/dependencies.md's table lists it: its name there, the metric its
// --depends reads and the labels it names the caller and the callee by.
// Istio's and Linkerd's name a service namespace/workload, from
// label_join, as the series' own labels are the workload's alone: two
// namespaces' same-named workloads would be one node, and a workload in
// another namespace could be named as the one an incident's service calls.
// TestDependsStacksDocumented holds the table and dependsStacks alike.
type dependsStack struct {
	// ID names it in --json, and stays.
	ID      string
	Name    string
	Metric  string
	Depends string
	Labels  string
}

var dependsStacks = []dependsStack{
	{"istio", "Istio", "istio_requests_total",
		`sum by (caller, callee) (label_join(label_join(rate(istio_requests_total{reporter="source"}[1h]), "caller", "/", "source_workload_namespace", "source_workload"), "callee", "/", "destination_workload_namespace", "destination_workload")) > 0`, "caller,callee"},
	{"linkerd", "Linkerd", "response_total",
		`sum by (caller, callee) (label_join(label_join(rate(response_total{direction="outbound"}[1h]), "caller", "/", "namespace", "deployment"), "callee", "/", "dst_namespace", "dst_deployment")) > 0`, "caller,callee"},
	{"otel-service-graph", "An OpenTelemetry or Tempo service graph", "traces_service_graph_request_total",
		`sum by (client, server) (rate(traces_service_graph_request_total[1h])) > 0`, "client,server"},
}

// counted is the selector of the metric that s's --depends reads, which
// its series are counted by: the shortest naming it.
func (s dependsStack) counted() (string, error) {
	sels, err := rules.Selectors(s.Depends)
	if err != nil {
		return "", err
	}
	best := ""
	for _, sel := range sels {
		if name, _, _ := strings.Cut(sel, "{"); name == s.Metric && (best == "" || len(sel) < len(best)) {
			best = sel
		}
	}
	if best == "" {
		return "", fmt.Errorf("%s's --depends reads no %s", s.Name, s.Metric)
	}
	return best, nil
}

// argument is the flags to give lapilli verdict for s, quoted for a POSIX
// shell.
func (s dependsStack) argument() string {
	return "--depends '" + strings.ReplaceAll(s.Depends, "'", `'\''`) + "' --depends-labels " + s.Labels
}

// suggestDepends is what lapilli impact suggest found of the stacks that
// count calls between services.
type suggestDepends struct {
	// Found are the stacks whose metric has series, NotFound those with
	// none, and NotKnown those that couldn't be asked, with why; each in
	// dependsStacks' order.
	Found    []suggestGraph `json:"found"`
	NotFound []suggestGraph `json:"not_found"`
	NotKnown []suggestGraph `json:"not_known"`
}

type suggestGraph struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Selector string `json:"selector"`
	// Series are the metric's, the most at once.
	Series   int    `json:"series"`
	Depends  string `json:"depends"`
	Labels   string `json:"labels"`
	Argument string `json:"argument"`
	Error    string `json:"error,omitempty"`
}

// suggestReport is what lapilli impact suggest found, as --json writes it.
type suggestReport struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Step      string    `json:"step"`
	ImpactFor string    `json:"impact_for"`
	// Found are the stacks whose metrics have series, NotFound those with
	// none, and NotKnown those that couldn't be asked, with why; each in
	// impactStacks' order.
	Found    []suggestStack `json:"found"`
	NotFound []suggestStack `json:"not_found"`
	NotKnown []suggestStack `json:"not_known"`
	// Dependencies are the stacks that count calls between services, for
	// --depends.
	Dependencies suggestDepends `json:"dependencies"`
}

type suggestStack struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Metrics []suggestMetric `json:"metrics"`
	// Series are its metrics', added up.
	Series int `json:"series"`
	// Windows are those its --impact finds over the period: for a stack
	// found, none when they couldn't be read, Error saying why.
	Windows  *int   `json:"windows,omitempty"`
	Impact   string `json:"impact"`
	Argument string `json:"argument"`
	Error    string `json:"error,omitempty"`
}

type suggestMetric struct {
	Name     string `json:"name"`
	Selector string `json:"selector"`
	Series   int    `json:"series"`
}

// suggestTakes are the flags of lapilli verdict that lapilli impact
// suggest takes, each read as verdict reads it (readFlags): true for one
// with a value.
var suggestTakes = map[string]bool{
	"--prometheus": true, "--google-project": true, "--auth": true, "--aws-region": true, "--prometheus-token-file": true, "--prometheus-tenant": true,
	"--max-rate": true, "--max-samples": true,
	"--since": true, "--from": true, "--to": true, "--step": true, "--tz": true,
	"--timeout": true, "--json": false,
}

func runImpact(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, impactUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "suggest" {
		fmt.Fprintf(stderr, "lapilli impact: unknown command %q\nRun \"lapilli impact -h\" for the options.\n", args[0])
		return 2
	}
	for _, a := range args[1:] {
		if a == "-h" || a == "--help" {
			fmt.Fprint(stdout, impactUsage)
			return 0
		}
	}
	f, err := parseSuggestFlagsAs(newFlagNames("impact", renamedFlags, stderr), args[1:])
	if err != nil {
		fmt.Fprintf(stderr, "lapilli impact suggest: %s\nRun \"lapilli impact -h\" for the options.\n", verdict.Clean(err.Error()))
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	from, to := f.from, f.to
	if from.IsZero() {
		to = now().Truncate(f.step)
		from = to.Add(-f.since)
	}
	h, err := newHistory(ctx, f)
	if err != nil {
		fmt.Fprintf(stderr, "lapilli impact suggest: %s\n", verdict.Clean(err.Error()))
		return 1
	}
	rep := suggest(ctx, h, from, to, f.step, f.impactFor)
	if f.json {
		err = verdict.JSON(stdout, rep)
	} else {
		err = rep.text(stdout, f.loc)
	}
	if err != nil {
		fmt.Fprintf(stderr, "lapilli impact suggest: %v\n", err)
		return 1
	}
	if len(rep.NotKnown) > 0 || ctx.Err() != nil {
		return 1
	}
	for _, s := range rep.Found {
		if s.Error != "" {
			return 1
		}
	}
	return 0
}

// parseSuggestFlags reads lapilli impact suggest's flags, each as lapilli
// verdict reads it, and refuses verdict's others.
func parseSuggestFlags(args []string) (*verdictFlags, error) {
	return parseSuggestFlagsAs(nil, args)
}

// parseSuggestFlagsAs is parseSuggestFlags, a flag by an old name taken as
// rn says.
func parseSuggestFlagsAs(rn *flagNames, args []string) (*verdictFlags, error) {
	for i := 0; i < len(args); i++ {
		name, _, hasVal := strings.Cut(args[i], "=")
		name, err := rn.name(name)
		if err != nil {
			return nil, err
		}
		value, ok := suggestTakes[name]
		if !ok {
			return nil, fmt.Errorf("unknown flag %s", name)
		}
		if value && !hasVal {
			i++
		}
	}
	f, err := readFlagsAs(rn, args, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case f.promURL == "" && f.gcpProj == "":
		return nil, errors.New("say where the history is: " + historyAt)
	case f.from.IsZero() != f.to.IsZero():
		return nil, errors.New("--from and --to go together")
	case !f.from.IsZero() && !f.to.After(f.from):
		return nil, errors.New("--to must be after --from")
	}
	return f, validHistory(f)
}

// suggest asks the history of each stack: its metrics' series, one request
// each, and, for a stack found, the windows its --impact finds in [from,
// to], as lapilli verdict --impact finds them. A query the history
// refuses as written (prom.Refused) leaves its stack not known; any other
// failure, of the history itself (not reached, not let in, turned away,
// the timeout), leaves every stack after it not known too, unasked.
func suggest(ctx context.Context, h history, from, to time.Time, step, impactFor time.Duration) suggestReport {
	rep := suggestReport{From: from, To: to, Step: verdict.Duration(step), ImpactFor: verdict.Duration(impactFor),
		Found: []suggestStack{}, NotFound: []suggestStack{}, NotKnown: []suggestStack{},
		Dependencies: suggestDepends{Found: []suggestGraph{}, NotFound: []suggestGraph{}, NotKnown: []suggestGraph{}}}
	stopped := ""
	why := func(err error) string {
		said := verdict.Clean(err.Error())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			said = "--timeout cut it short"
		}
		if !prom.Refused(err) {
			stopped = said
		}
		return said
	}
	for _, st := range impactStacks {
		s := suggestStack{ID: st.ID, Name: st.Name, Metrics: []suggestMetric{}, Impact: st.Impact, Argument: impactArgument(st.Impact)}
		if stopped != "" {
			s.Error = stopped
			rep.NotKnown = append(rep.NotKnown, s)
			continue
		}
		found := true
		for _, m := range st.Metrics {
			sel, err := st.counted(m)
			n := 0
			if err == nil {
				n, err = source.SeriesCount(ctx, h, sel, from, to)
			}
			if err != nil {
				s.Error = why(err)
				break
			}
			s.Metrics = append(s.Metrics, suggestMetric{Name: m, Selector: sel, Series: n})
			s.Series += n
			found = found && n > 0
		}
		switch {
		case s.Error != "":
			rep.NotKnown = append(rep.NotKnown, s)
			continue
		case !found:
			rep.NotFound = append(rep.NotFound, s)
			continue
		}
		series, err := h.QueryRange(ctx, st.Impact, from, to, step)
		if err == nil {
			var ws []incident.Incident
			if ws, err = incident.FromImpact(ctx, series, step, impactFor); err == nil {
				n := len(ws)
				s.Windows = &n
			}
		}
		if err != nil {
			s.Error = why(err)
		}
		rep.Found = append(rep.Found, s)
	}
	// The stacks that count calls: one request each, asked as the others
	// are, and none once the history failed.
	for _, st := range dependsStacks {
		g := suggestGraph{ID: st.ID, Name: st.Name, Depends: st.Depends, Labels: st.Labels, Argument: st.argument()}
		if stopped != "" {
			g.Error = stopped
			rep.Dependencies.NotKnown = append(rep.Dependencies.NotKnown, g)
			continue
		}
		sel, err := st.counted()
		if err == nil {
			g.Selector = sel
			g.Series, err = source.SeriesCount(ctx, h, sel, from, to)
		}
		switch {
		case err != nil:
			g.Error = why(err)
			rep.Dependencies.NotKnown = append(rep.Dependencies.NotKnown, g)
		case g.Series == 0:
			rep.Dependencies.NotFound = append(rep.Dependencies.NotFound, g)
		default:
			rep.Dependencies.Found = append(rep.Dependencies.Found, g)
		}
	}
	return rep
}

// impactArgument is --impact with expr, quoted for a POSIX shell.
func impactArgument(expr string) string {
	return "--impact '" + strings.ReplaceAll(expr, "'", `'\''`) + "'"
}

// text writes r for a person: the stacks found, with their series and
// windows, those not found on one line, those not known with why, then
// each found stack's --impact, ready to give.
func (r suggestReport) text(w io.Writer, loc *time.Location) error {
	const stamp = "2006-01-02 15:04"
	fmt.Fprintf(w, "Lapilli impact suggest  %s → %s %s\n", r.From.In(loc).Format(stamp), r.To.In(loc).Format(stamp), verdict.Zone(loc, r.From, r.To))
	all := len(r.Found) + len(r.NotFound) + len(r.NotKnown)
	switch {
	case len(r.Found) > 0:
		fmt.Fprintf(w, "%d of the %d stacks have their metrics in the history: SERIES, the most at once, and WINDOWS,\n", len(r.Found), all)
		fmt.Fprintf(w, "those its --impact finds, %s or longer, each an incident nobody wrote down.\n\n", r.ImpactFor)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "STACK\tSERIES\tWINDOWS")
		for _, s := range r.Found {
			n := "?"
			if s.Windows != nil {
				n = fmt.Sprint(*s.Windows)
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\n", s.Name, s.Series, n)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	case len(r.NotKnown) == 0:
		fmt.Fprintf(w, "None of the %d stacks have their metrics in the history.\n", all)
	}
	if len(r.NotFound) > 0 || len(r.NotKnown) > 0 {
		fmt.Fprintln(w)
	}
	if len(r.NotFound) > 0 {
		names := make([]string, 0, len(r.NotFound))
		for _, s := range r.NotFound {
			names = append(names, s.Name)
		}
		fmt.Fprintf(w, "Not found: %s.\n", strings.Join(names, ", "))
	}
	// Those not known for one reason together, as after the history failed.
	for i := 0; i < len(r.NotKnown); {
		names := []string{r.NotKnown[i].Name}
		j := i + 1
		for ; j < len(r.NotKnown) && r.NotKnown[j].Error == r.NotKnown[i].Error; j++ {
			names = append(names, r.NotKnown[j].Name)
		}
		fmt.Fprintf(w, "Not known: %s, which couldn't be asked: %s\n", strings.Join(names, ", "), r.NotKnown[i].Error)
		i = j
	}
	for _, s := range r.Found {
		fmt.Fprintf(w, "\n%s:\n  %s\n", s.Name, s.Argument)
		if s.Error != "" {
			fmt.Fprintf(w, "  its windows couldn't be read: %s\n", s.Error)
		}
	}
	r.dependsText(w)
	fmt.Fprintln(w)
	if len(r.Found) > 0 {
		fmt.Fprintln(w, "Label names depend on how each is scraped, and the threshold on what your users notice:")
		fmt.Fprintln(w, "check both before giving one to lapilli verdict (lapilli.dev/docs/impact).")
	} else {
		fmt.Fprintln(w, "Write an --impact of your own, from a signal your users would notice (lapilli.dev/docs/impact).")
	}
	return nil
}

// dependsText writes the stacks that count calls between services: those
// found with the flags to give, then those not found, and those not known
// with why.
func (r suggestReport) dependsText(w io.Writer) {
	d := r.Dependencies
	all := len(d.Found) + len(d.NotFound) + len(d.NotKnown)
	if all == 0 {
		return
	}
	fmt.Fprintf(w, "\nWho calls whom, for --depends (docs/dependencies.md): %d of the %d stacks have their metric in the history.\n", len(d.Found), all)
	for _, g := range d.Found {
		fmt.Fprintf(w, "\n%s (%s, %d series at most at once):\n  %s\n", g.Name, g.Selector, g.Series, g.Argument)
	}
	if len(d.NotFound) > 0 {
		names := make([]string, 0, len(d.NotFound))
		for _, g := range d.NotFound {
			names = append(names, g.Name)
		}
		fmt.Fprintf(w, "\nNot found: %s.\n", strings.Join(names, ", "))
	}
	for i := 0; i < len(d.NotKnown); {
		names := []string{d.NotKnown[i].Name}
		j := i + 1
		for ; j < len(d.NotKnown) && d.NotKnown[j].Error == d.NotKnown[i].Error; j++ {
			names = append(names, d.NotKnown[j].Name)
		}
		fmt.Fprintf(w, "Not known: %s, which couldn't be asked: %s\n", strings.Join(names, ", "), d.NotKnown[i].Error)
		i = j
	}
	if len(d.Found) > 0 {
		fmt.Fprintln(w, "\nThe services on its edges must be named as your firings and incidents name them, by the --match label. Istio's and Linkerd's are")
		fmt.Fprintln(w, "namespace/workload, so that same-named workloads of two namespaces don't merge: give that form to the label (docs/dependencies.md says how),")
		fmt.Fprintln(w, "or the plain form it also lists. Give the label to --depends-match when --match has several.")
	}
}
