package rules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kubeStack is a rendered chart's rules as large as kube-prometheus-stack's
// with its mixins, times scale: 35 PrometheusRules a scale, each of four
// groups of six alert rules and three recording rules, each group with four
// labels of its own (Prometheus 3's), each alert rule's expression some 700
// bytes of PromQL, its description some 300 bytes, every tenth's 4 KiB
// written out in full, a runbook, and a severity; each recording rule's
// expression some 300 bytes. Documents are cut as helm template cuts them.
func kubeStack(scale int, groupLabels bool) string {
	var b strings.Builder
	long := strings.Repeat("The pod has been restarting and its container is waiting; check its events, its logs and its node's pressure before scaling. ", 34)[:4096]
	n := 0
	for o := range 35 * scale {
		fmt.Fprintf(&b, "---\n# Source: kube-prometheus-stack/templates/prometheus/rules-1.14/r%d.yaml\napiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\n"+
			"metadata:\n  name: kube-prometheus-stack-r%d\n  namespace: monitoring\n  labels:\n    app: kube-prometheus-stack\n    release: kube-prometheus-stack\nspec:\n  groups:\n", o, o)
		for g := range 4 {
			fmt.Fprintf(&b, "  - name: kubernetes-apps-%d-%d\n", o, g)
			if groupLabels {
				fmt.Fprintf(&b, "    labels:\n      team: platform-observability-%d\n      environment: production\n      cluster: eu-west-1-primary\n      source: kube-prometheus-stack\n", o%7)
			}
			b.WriteString("    rules:\n")
			for range 6 {
				n++
				desc := fmt.Sprintf("Pod {{ $labels.namespace }}/{{ $labels.pod }} ({{ $labels.container }}) is in waiting state (reason: \"CrashLoopBackOff\") for longer than 15 minutes on rule %d. ", n)
				desc = strings.Repeat(desc, 2)
				if n%10 == 0 {
					desc = long
				}
				fmt.Fprintf(&b, "    - alert: KubeContainerWaiting%d\n      annotations:\n        description: %q\n        runbook_url: https://runbooks.prometheus-operator.dev/runbooks/kubernetes/kubecontainerwaiting\n"+
					"        summary: Pod container waiting longer than 1 hour\n      expr: |-\n"+
					"        sum by (namespace, pod, container, cluster) (kube_pod_container_status_waiting_reason{reason!=\"CrashLoopBackOff\", job=\"kube-state-metrics\", namespace=~\".*\"}) > 0\n"+
					"        and on (namespace, pod, cluster) (\n          max by (namespace, pod, cluster) (kube_pod_owner{job=\"kube-state-metrics\", owner_kind!=\"Job\"})\n"+
					"          * on (namespace, pod, cluster) group_left (node) max by (namespace, pod, node, cluster) (kube_pod_info{job=\"kube-state-metrics\"})\n"+
					"        ) unless on (namespace, pod, cluster) (\n          sum by (namespace, pod, cluster) (rate(container_cpu_usage_seconds_total{job=\"kubelet\", metrics_path=\"/metrics/cadvisor\", image!=\"\"}[5m])) > %d\n        )\n"+
					"      for: 15m\n      labels:\n        severity: %s\n", n, desc, n, []string{"warning", "critical"}[n%2])
			}
			for range 3 {
				n++
				fmt.Fprintf(&b, "    - record: node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate_%d\n      expr: |-\n"+
					"        sum by (cluster, namespace, pod, container) (\n          irate(container_cpu_usage_seconds_total{job=\"kubelet\", metrics_path=\"/metrics/cadvisor\", image!=\"\"}[5m])\n"+
					"        ) * on (cluster, namespace, pod) group_left(node) topk by (cluster, namespace, pod) (\n          1, max by (cluster, namespace, pod, node) (kube_pod_info{node!=\"\"})\n        )\n"+
					"      labels:\n        quantile: \"0.99\"\n", n)
			}
		}
	}
	return b.String()
}

// realistic is a large rule set as a real one can be: n rules in groups of
// 50, each group's labels some 300 bytes, and annotations of some 800
// shared by an alias, with their own labels and expressions.
func realistic(n int) string {
	var b strings.Builder
	b.WriteString("groups:\n")
	desc := strings.Repeat("The error budget burns faster than it should; see the runbook. ", 12)
	for i := range n {
		if i%50 == 0 {
			fmt.Fprintf(&b, "  - name: group-%d\n    labels: {team: payments-platform-%d, tier: customer-facing-critical-path, environment: production-primary-region, "+
				"owner: platform-reliability-on-call-rotation, cost_center: engineering-operations-%d, service: settlement-and-reconciliation-%d}\n    rules:\n", i/50, i/50, i/50, i/50)
		}
		if i == 0 {
			fmt.Fprintf(&b, "      - alert: R0\n        expr: sum(rate(errors_total{job=\"api-0\"}[5m])) / sum(rate(requests_total{job=\"api-0\"}[5m])) > 0.05\n        for: 5m\n"+
				"        labels: {severity: page}\n        annotations: &said {summary: \"Errors are high\", description: %q, runbook_url: \"https://runbooks.example.com/errors\"}\n", desc)
			continue
		}
		fmt.Fprintf(&b, "      - alert: R%d\n        expr: sum(rate(errors_total{job=\"api-%d\"}[5m])) / sum(rate(requests_total{job=\"api-%d\"}[5m])) > 0.05\n        for: 5m\n"+
			"        labels: {severity: page}\n        annotations: *said\n", i, i, i)
	}
	return b.String()
}

// terse is n rules as tersely as a real file writes them, in flow style,
// each its severity and no more, in groups of 50 with three labels each.
func terse(n int) string {
	var b strings.Builder
	b.WriteString("groups:\n")
	for i := range n {
		if i%50 == 0 {
			fmt.Fprintf(&b, "- name: g%d\n  labels: {team: t%d, env: prod, tier: one}\n  rules:\n", i/50, i/50)
		}
		fmt.Fprintf(&b, "  - {alert: Down%05d, expr: 'up{job=\"j%05d\"} == 0', labels: {severity: page}}\n", i, i)
	}
	return b.String()
}

// TestRealSetsRead: real rule sets are never refused for what they make:
// kube-prometheus-stack's rules with its mixins, four times over (5,040
// rules, each group with labels of its own, long PromQL, 4 KiB
// descriptions written out in full), as a stream, a file read with its
// recording rules, a directory of its documents and a ConfigMap; 5,000
// rules sharing a group's 300 bytes of labels and 800 of annotations by an
// alias; 10,000 written tersely, each group's three labels on each; and
// labels past 16 KiB, which Prometheus takes, in a file and from its API,
// as a version of the rules kept in the ledger before this bound holds
// them.
func TestRealSetsRead(t *testing.T) {
	read := func(what, doc string, want int) {
		t.Helper()
		var rs []Rule
		var err error
		ctx := OneInput(t.Context())
		bytes, took := allocated(func() { rs, err = parseContext(ctx, []byte(doc), nil) })
		in := inputOf(ctx)
		t.Logf("%s: %d KB, %d rules, %d MB allocated, %v; made %d KB of the %d KB it may", what, len(doc)>>10, len(rs), bytes>>20, took,
			in.made>>10, madeLimit(in.size)>>10)
		if err != nil || len(rs) != want {
			t.Errorf("%s: %d rules, %v", what, len(rs), err)
		}
	}
	stack := kubeStack(4, true)
	read("kube-prometheus-stack, four times over, group labels", stack, 140*4*6)
	read("kube-prometheus-stack, four times over", kubeStack(4, false), 140*4*6)
	read("5,000 rules, a group's labels and annotations shared", realistic(5000), 5000)
	read("10,000 rules written tersely, a group's labels on each", terse(10000), 10000)

	rs, rec, err := readWithRecordings(t, stack)
	if err != nil || len(rs) != 140*4*6 || len(rec) != 140*4*3 || rec[0].Labels["team"] != "platform-observability-0" {
		t.Errorf("read with its recording rules: %d, %d, %v", len(rs), len(rec), err)
	}
	dir := t.TempDir()
	for i, d := range strings.Split(stack, "---\n")[1:] {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("r%03d.yaml", i)), []byte(d), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if rs, docs, _, err := ReadPicked(dir, Picking{}); err != nil || len(rs) != 140*4*6 {
		t.Errorf("a directory of its documents: %d, %v", len(rs), err)
	} else if again, err := FromDocs(docs); err != nil || len(again) != len(rs) {
		t.Errorf("its documents read again, as from the ledger: %d, %v", len(again), err)
	}
	var files []string
	for i, d := range strings.Split(kubeStack(1, true), "---\n")[1:] {
		if i < 10 {
			// A rule file's groups, as a ConfigMap mounted in Prometheus holds them.
			_, spec, _ := strings.Cut(d, "spec:\n")
			lines := strings.Split(spec, "\n")
			for j, l := range lines {
				lines[j] = strings.TrimPrefix(l, "  ")
			}
			files = append(files, strings.Join(lines, "\n"))
		}
	}
	if rs, err := FromDocs([]Doc{{Source: "cm", Data: []byte(configMap(files...))}}); err != nil || len(rs) != 10*4*6 {
		t.Errorf("a ConfigMap of ten rule files: %d, %v", len(rs), err)
	}

	big := strings.Repeat("v", 20<<10)
	file := fmt.Sprintf("groups:\n- name: g\n  labels: {owner: %q}\n  rules:\n  - alert: A\n    expr: up == 0\n    labels:\n      ? %s\n      : %q\n  - {alert: B, expr: up == 0}\n",
		big, "k"+big, big)
	if rs, err := FromDocs([]Doc{{Source: "v", Data: []byte(file)}}); err != nil || len(rs) != 2 || len(rs[0].Labels["k"+big]) != 20<<10 {
		t.Errorf("labels of 20 KiB: %d, %v", len(rs), err)
	}
	api, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"groups": []any{map[string]any{"name": "g", "rules": []any{
		map[string]any{"type": "alerting", "name": "A", "query": "up == 0", "labels": map[string]string{"owner": big, "k" + big: big}}}}}}})
	if rs, err := FromDocs([]Doc{{Source: "api", API: true, Data: api}}); err != nil || len(rs) != 1 {
		t.Errorf("labels of 20 KiB from the API: %d, %v", len(rs), err)
	}
}
