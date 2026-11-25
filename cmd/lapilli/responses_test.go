package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// ksm is a stand-in Prometheus that holds kube-state-metrics: a Deployment
// web in shop, rolled out at day 1 10:05 (its ReplicaSet web-7d4 created
// then), with a pod web-7d4-abc, and replicas read all week. Nothing else
// is there: no autoscaler, no StatefulSet, no DaemonSet. It answers what
// internal/response asks of it by the metric each query names.
type ksm struct {
	mu    sync.Mutex
	asked []string
}

func (k *ksm) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	k.mu.Lock()
	k.asked = append(k.asked, expr)
	k.mu.Unlock()
	one := func(v float64, kv ...string) []prom.Series {
		return []prom.Series{{Labels: lbl(kv...), Samples: []prom.Sample{{T: end, V: v}}}}
	}
	switch {
	case strings.Contains(expr, "kube_replicaset_owner"):
		return one(1, "namespace", "shop", "replicaset", "web-7d4", "owner_kind", "Deployment", "owner_name", "web", "owner_is_controller", "true"), nil
	case strings.Contains(expr, "kube_replicaset_created"):
		return one(float64(at(1, 10, 5).Unix()), "namespace", "shop", "replicaset", "web-7d4"), nil
	case strings.Contains(expr, "kube_pod_owner"):
		return one(1, "namespace", "shop", "pod", "web-7d4-abc", "owner_kind", "ReplicaSet", "owner_name", "web-7d4", "owner_is_controller", "true"), nil
	case strings.HasPrefix(expr, "kube_deployment_spec_replicas"):
		s := prom.Series{Labels: lbl("namespace", "shop", "deployment", "web")}
		for t := start; !t.After(end); t = t.Add(24 * time.Hour) {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: 2})
		}
		return []prom.Series{s}, nil
	}
	return nil, nil
}

// responsesWeek is a week of firings with what was done about them: a
// rollout, a page nobody answered, a page acknowledged and nothing
// changed, a deploy, and a firing nothing is known of.
func responsesWeek(t *testing.T) []string {
	t.Helper()
	responses := write(t, "responses.yaml", `responses:
  - id: PD-2
    alerts: [{rule: ApiErrors, labels: {namespace: shop, deployment: web}}]
    sent: `+ts(at(2, 9, 0).Add(30*time.Second))+`
    resolved: `+ts(at(2, 9, 3))+`
    resolved_by: alert
  - id: PD-3
    alerts: [{rule: DiskFull, labels: {namespace: shop, pod: web-7d4-abc}}]
    sent: `+ts(at(3, 12, 1))+`
    acknowledged: `+ts(at(3, 12, 5))+`
    resolved: `+ts(at(3, 12, 30))+`
    resolved_by: person
  - id: PD-9
    alerts: [{rule: RenamedAlert, labels: {namespace: shop}}]
    sent: `+ts(at(3, 9, 0))+`
`)
	deploys := write(t, "deploys.yaml", `deploys:
  - labels: {service: cart}
    start: `+ts(at(4, 5, 10))+`
    end: `+ts(at(4, 5, 20))+`
    revision: 4f2a9c1
`)
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-1, title: Errors, start: `+ts(at(1, 10, 0))+`, end: `+ts(at(1, 10, 30))+`, labels: {namespace: shop}}
`)
	web := `{"namespace":"shop","deployment":"web","severity":"critical"}`
	firings := firingsFile(t, [][4]string{
		{"ApiErrors", web, ts(at(1, 10, 0)), ts(at(1, 10, 20))},
		{"ApiErrors", web, ts(at(2, 9, 0)), ts(at(2, 9, 3))},
		{"DiskFull", `{"namespace":"shop","pod":"web-7d4-abc"}`, ts(at(3, 12, 0)), ts(at(3, 12, 30))},
		{"QueueDepth", `{"service":"cart"}`, ts(at(4, 5, 0)), ts(at(4, 5, 30))},
		{"Latency", `{"service":"other"}`, ts(at(5, 2, 0)), ts(at(5, 2, 10))},
	})
	return []string{"verdict", "--firings", firings, "--incidents", incidents, "--prometheus", "http://p", "--responses", responses, "--rollouts", "--deploys", deploys,
		"--from", ts(sigBase), "--to", ts(sigBase.Add(7 * 24 * time.Hour)), "--match", "namespace", "--tz", "UTC"}
}

// The report with responses, from a responses file, kube-state-metrics and
// a deploys file: the RESPONSES table, the fields of --json (golden, with
// LAPILLI_GOLDEN=update), and the CSV and the metrics.
func TestVerdictResponses(t *testing.T) {
	alone(t)
	h := &ksm{}
	newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
	now = func() time.Time { return sigBase.Add(8 * 24 * time.Hour) }
	args := responsesWeek(t)
	for _, out := range []struct{ file, flag string }{{"report.txt", ""}, {"report.json", "--json"}, {"report.csv", "--csv"}, {"report.prom", "--metrics"}} {
		a := args
		if out.flag != "" {
			a = append(append([]string{}, args...), out.flag)
		}
		var stdout, stderr bytes.Buffer
		if code := run(a, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d\n%s", out.file, code, stderr.String())
		}
		path := filepath.Join("testdata", "responses", out.file)
		if os.Getenv("LAPILLI_GOLDEN") == "update" {
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, stdout.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := stdout.String(); got != string(want) {
			t.Errorf("%s differs:\n%s\nwant\n%s", out.file, got, want)
		}
	}
	// Names go into the queries quoted, and the namespace and the pod the
	// firings carry are asked about.
	var ns, pod bool
	for _, q := range h.asked {
		ns = ns || strings.Contains(q, `namespace=~"shop"`)
		pod = pod || strings.Contains(q, `pod=~"web-7d4-abc"`)
	}
	if !ns || !pod {
		t.Errorf("asked %q: want the namespace and the pod the firings carry", h.asked)
	}
}

// The text says what each firing came to: the rollout made the first
// firing useful, the page nobody answered the second not, and so on.
func TestVerdictResponsesRead(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return &ksm{}, nil }
	var stdout, stderr bytes.Buffer
	if code := run(append(responsesWeek(t), "--json"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		`"noisy_by": "useful"`, `"routine": 0.5`, `"pages": 3`, `"pages_unmatched": 1`, `"autoscalers_known": false`,
		`"kind": "changed"`, `"kind": "unanswered"`, `"kind": "acknowledged"`, `"kind": "deploy"`, `"id": "PD-2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--json lacks %s:\n%s", want, out)
		}
	}
	// --redact gives every page an alias.
	stdout.Reset()
	if code := run(append(responsesWeek(t), "--json", "--redact"), &stdout, &stderr); code != 0 {
		t.Fatalf("redact: exit %d\n%s", code, stderr.String())
	}
	for _, name := range []string{"PD-2", "PD-3", "web-7d4-abc", "cart"} {
		if strings.Contains(stdout.String(), name) {
			t.Errorf("--redact left %s in the report", name)
		}
	}
	if !strings.Contains(stdout.String(), `"id": "page-`) {
		t.Errorf("--redact: no page alias in\n%s", stdout.String())
	}
}

// With none of --responses, --rollouts and --deploys, a report has none of
// what they add, and is noisy by what it caught.
func TestVerdictWithoutResponses(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return &ksm{}, nil }
	var args []string
	all := responsesWeek(t)
	for i := 0; i < len(all); i++ {
		switch all[i] {
		case "--responses", "--deploys", "--prometheus":
			i++
		case "--rollouts":
		default:
			args = append(args, all[i])
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(append(args, "--json"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	for _, not := range []string{`"responses"`, `"response"`, `"noisy_by"`, `"routine"`} {
		if strings.Contains(stdout.String(), not) {
			t.Errorf("a report with no responses has %s", not)
		}
	}
	stdout.Reset()
	if code := run(append(args, "--csv"), &stdout, &stderr); code != 0 {
		t.Fatalf("csv: exit %d\n%s", code, stderr.String())
	}
	if first, _, _ := strings.Cut(stdout.String(), "\n"); strings.Contains(first, "response") {
		t.Errorf("the CSV's header with no responses: %s", first)
	}
}

// The flags: what they take, and what they go with.
func TestResponsesFlags(t *testing.T) {
	parallel(t)
	base := []string{"--firings", "f.jsonl", "--incidents", "i.yaml"}
	for _, c := range []struct {
		args []string
		want string // "" for accepted
	}{
		{[]string{"--noisy-by", "caught"}, ""},
		{[]string{"--noisy-by", "useful"}, "--noisy-by useful finds a rule noisy by what was done"},
		{[]string{"--noisy-by", "often"}, "want caught or useful"},
		{[]string{"--responses", "r.yaml", "--noisy-by", "useful"}, ""},
		{[]string{"--responses", "r.yaml", "--routine", "0"}, "want a chance above 0"},
		{[]string{"--responses", "r.yaml", "--routine", "1.5"}, "want a chance above 0"},
		{[]string{"--responses", "r.yaml", "--routine", "0.8"}, ""},
		{[]string{"--routine", "0.8"}, "goes with --responses"},
		{[]string{"--responses", "r.yaml", "--rollout-match", "service"}, "goes with --rollouts"},
		{[]string{"--rollouts", "--rollout-match", "service"}, "--rollouts reads kube-state-metrics from the history"},
		{[]string{"--rollouts", "--prometheus", "http://p", "--rollout-match", "service", "--rollout-match", "app"}, ""},
		{[]string{"--rollout-match", "not a label", "--rollouts", "--prometheus", "http://p"}, "want a label's name"},
		{[]string{"--deploys", "d.yaml", "--rollouts=false"}, ""},
	} {
		_, err := parseVerdictFlags(append(append([]string{}, base...), c.args...))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%v: %v", c.args, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
	// Alone, the three inputs need no incidents: a rule is judged by them.
	if _, err := parseVerdictFlags([]string{"--firings", "f.jsonl", "--deploys", "d.yaml"}); err != nil {
		t.Errorf("responses with no incidents: %v", err)
	}
	if _, err := parseVerdictFlags([]string{"--firings", "f.jsonl"}); err == nil || !strings.Contains(fmt.Sprint(err), "say what went wrong") {
		t.Errorf("no incidents and no responses: %v", err)
	}
	f, err := parseVerdictFlags(append(base, "--responses", "r.yaml"))
	if err != nil || f.noisyByOf() != "useful" {
		t.Errorf("noisy by with responses: %v %v", f.noisyByOf(), err)
	}
	f, _ = parseVerdictFlags(append(base, "--responses", "r.yaml", "--noisy-by", "caught"))
	if f.noisyByOf() != "caught" {
		t.Errorf("--noisy-by caught: %v", f.noisyByOf())
	}
	f, _ = parseVerdictFlags(base)
	if f.noisyByOf() != "caught" {
		t.Errorf("noisy by without responses: %v", f.noisyByOf())
	}
}
