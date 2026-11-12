package serve

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"
)

// What the change recipes write, push and say, against a Lapilli someone
// took over: it controls the document, its hash, the ids and what it
// answers. docs/design/changes.md assumes it.

// standIn is a Lapilli handing out changes, keeping what it was told.
type standIn struct {
	*httptest.Server
	mu   sync.Mutex
	told []string
}

func newStandIn(t *testing.T, changes []map[string]any) *standIn {
	t.Helper()
	l := &standIn{}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer "+changesToken:
			http.Error(w, "no", http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/changes":
			json.NewEncoder(w).Encode(map[string]any{"changes": changes})
		case r.Method == http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			l.mu.Lock()
			l.told = append(l.told, strings.TrimPrefix(r.URL.Path, "/api/v1/changes/")+" "+string(b))
			l.mu.Unlock()
			io.WriteString(w, "{}")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(l.Close)
	return l
}

func (l *standIn) said() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.told, "\n")
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// repo is a bare repository holding files on main, those named in
// executable with mode 100755, and the branches given, each main with one
// file written.
func repo(t *testing.T, env []string, files map[string]string, executable []string, branches map[string][2]string) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	gitIn(t, env, dir, "init", "--quiet", "--bare", "--initial-branch=main", bare)
	work := filepath.Join(dir, "work")
	gitIn(t, env, dir, "clone", "--quiet", bare, work)
	gitIn(t, env, work, "checkout", "--quiet", "-b", "main")
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		mode := os.FileMode(0o644)
		if slices.Contains(executable, name) {
			mode = 0o755
		}
		os.WriteFile(filepath.Join(work, name), []byte(body), mode)
	}
	commit := func(msg string) {
		gitIn(t, env, work, "add", ".")
		gitIn(t, env, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", msg)
	}
	commit("rules")
	gitIn(t, env, work, "push", "--quiet", "origin", "HEAD:main")
	for branch, f := range branches {
		gitIn(t, env, work, "checkout", "--quiet", "-B", "b", "main")
		os.WriteFile(filepath.Join(work, f[0]), []byte(f[1]), 0o644)
		commit("reviewed")
		gitIn(t, env, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	}
	return bare
}

// runChangeRecipe runs github.sh against a stand-in Lapilli and GitHub, with more
// of the environment, and gives what it said, and its error.
func runChangeRecipe(t *testing.T, env []string, lapilli, host, bare, paths string, more ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", changeRecipes+"github.sh")
	cmd.Env = append(append(append(env, "LAPILLI_URL="+lapilli, "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE="+bare,
		"RULES_PATHS="+paths), recipeHosts[0].env(host, tokenFile(t, "host-token-0123456789"))...), more...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func branches(t *testing.T, env []string, bare string) []string {
	t.Helper()
	out := strings.Fields(gitIn(t, env, bare, "for-each-ref", "--format=%(refname:short)", "refs/heads/lapilli/"))
	slices.Sort(out)
	return out
}

func githubStandIn(t *testing.T) (*prHost, string) {
	t.Helper()
	h := recipeHosts[0]
	host := &prHost{t: t, kind: h.kind, auth: h.auth}
	hs := httptest.NewServer(host)
	t.Cleanup(hs.Close)
	host.url = hs.URL
	return host, hs.URL
}

// A change is written only to a rule file: the file the document is must
// be one, as must the document. A Lapilli someone took over, handing out
// the hash of a script, a Makefile, a GitLab include, a build's
// configuration or any other file a push makes a CI system run, under
// RULES_PATHS or not, in any letter case, had the recipe push it, executable
// still; or wrote a CI system's file over a rule file. A push runs those
// with the repository's secrets before anyone reviews them.
func TestChangeRecipeWritesOnlyRuleFiles(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	steal := "groups: []\n" // looks like rules, to pass whatever reads the document alone
	files := map[string]string{
		"alerts/shop.yaml":              ruleFile,
		"alerts/test.sh":                "#!/bin/sh\npromtool check rules alerts/*.yaml\n",
		"alerts/Makefile":               "test:\n\tpromtool check rules *.yaml\n",
		"alerts/ci/rules-check.yml":     "rules-check:\n  script: promtool check rules alerts/*.yaml\n",
		"alerts/pipelines/validate.yml": "steps:\n  - script: promtool check rules alerts/*.yaml\n",
		"alerts/cloudbuild.yaml":        "steps:\n  - name: prom/prometheus\n    args: [check, rules, alerts/shop.yaml]\n",
		"alerts/.GITLAB-CI.YML":         "check:\n  script: promtool check rules alerts/*.yaml\n",
		"alerts/.tekton/push.yaml":      "apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata: {name: check}\n",
		"alerts/buildspec.yml":          "version: 0.2\nphases:\n  build:\n    commands: [promtool check rules alerts/shop.yaml]\n",
		"alerts/run.yaml":               ruleFile + "# runs\n",
		"alerts/groups-job.yml":         "groups:\n  script: promtool check rules alerts/*.yaml\n",
	}
	exe := []string{"alerts/test.sh", "alerts/run.yaml"}
	for _, c := range []struct {
		target, document, want string
	}{
		{"alerts/test.sh", steal, "is not one a change is written to"},
		{"alerts/Makefile", steal, "is not one a change is written to"},
		{"alerts/ci/rules-check.yml", steal, "is not one a change is written to"},
		{"alerts/pipelines/validate.yml", steal, "is not one a change is written to"},
		{"alerts/cloudbuild.yaml", steal, "is not one a change is written to"},
		{"alerts/.GITLAB-CI.YML", steal, "is not one a change is written to"},
		{"alerts/.tekton/push.yaml", steal, "is not one a change is written to"},
		{"alerts/buildspec.yml", steal, "is not one a change is written to"},
		{"alerts/run.yaml", steal, "is not one a change is written to"},
		{"alerts/groups-job.yml", steal, "is not one a change is written to"},
		// A rule file, written as a GitLab job named groups, or a workflow.
		{"alerts/shop.yaml", "groups:\n  script: env\n", "the change Lapilli wrote is not a rule file"},
		{"alerts/shop.yaml", "on: [push]\njobs: {steal: {runs-on: x, steps: [{run: env}]}}\n", "the change Lapilli wrote is not a rule file"},
		{"alerts/shop.yaml", "groups: []\njobs:\n  steal: {runs-on: x, steps: [{run: env}]}\n", "the change Lapilli wrote is not a rule file"},
		{"alerts/shop.yaml", "groups: []\r\"jobs\": {steal: {script: env}}\n", "the change Lapilli wrote is not a rule file"},
		// And a rule file written as one: pushed.
		{"alerts/shop.yaml", strings.Replace(ruleFile, "for: 2m", "for: 15m", 1), "opened"},
	} {
		t.Run(c.target+", "+strings.SplitN(c.document, "\n", 2)[0], func(t *testing.T) {
			env := gitEnv(t)
			bare := repo(t, env, files, exe, nil)
			lapilli := newStandIn(t, []map[string]any{{"id": 1, "ledger": "0123456789ab", "rule": "Errors", "kind": "for", "state": "ready",
				"doc_hash": sum(files[c.target]), "document": c.document, "title": "Lapilli: a new for, Errors", "body": "Backtest"}})
			host, hostURL := githubStandIn(t)
			out, _ := runChangeRecipe(t, env, lapilli.URL, hostURL, bare, "alerts")
			said := out + lapilli.said()
			if !strings.Contains(said, c.want) {
				t.Errorf("want %q, said:\n%s", c.want, said)
			}
			pushed := branches(t, env, bare)
			if c.want == "opened" {
				if len(pushed) != 1 || len(host.opened) != 1 {
					t.Errorf("a rule file, written as one: pushed %v, opened %v", pushed, host.opened)
				}
				return
			}
			if len(pushed) != 0 || len(host.opened) != 0 {
				t.Errorf("pushed %v, opened %v", pushed, host.opened)
			}
		})
	}
	// RULES_PATHS can't name what a CI system runs either, in any case.
	for _, paths := range []string{".travis.yml", "cloudbuild.yaml", "alerts/.GITLAB-CI.YML", ".Github", "deploy/Jenkinsfile", "alerts .tekton"} {
		env := gitEnv(t)
		bare := repo(t, env, files, exe, nil)
		lapilli := newStandIn(t, nil)
		_, hostURL := githubStandIn(t)
		if out, err := runChangeRecipe(t, env, lapilli.URL, hostURL, bare, paths); err == nil || !strings.Contains(out, "it is what a CI system runs") {
			t.Errorf("RULES_PATHS %s: %v %s", paths, err, out)
		}
	}
}

// ruleFileCases are documents rule_file reads: true, alert rules and nothing
// else; false, anything else, or what YAML could read in more than one way.
var ruleFileCases = []struct {
	name, body string
	ok         bool
}{
	{"x.yaml", ruleFile, true},
	{"x.yml", "# shop\n---\ngroups:\n- name: shop\n  rules:\n  - alert: A\n    expr: up == 0\n", true},
	{"x.yaml", "groups: []\n", true},
	{"x.rules", "groups:\n  # first\n\n  - name: g\n    rules: []\n...\n", true},
	{"x.yaml", "groups:\r\n  - name: g\r\n    rules: []\r\n", true},
	{"x.yaml", "namespace: shop\ngroups:\n  - name: g\n    rules: []\n", true},
	{"x.yaml", "apiVersion: 1\ngroups:\n  - orgId: 1\n    name: g\n    folder: shop\n    rules: []\ndeleteRules:\n- orgId: 1\n  uid: x\n", true},
	{"x.yaml", "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: shop\nspec:\n  groups:\n    - name: g\n      rules: []\n", true},
	{"x.yaml", "apiVersion: monitoring.googleapis.com/v1\nkind: 'Rules'  # GMP\nmetadata: {name: shop}\nspec: {groups: []}\n---\napiVersion: monitoring.coreos.com/v1\nkind: \"PrometheusRule\"\nmetadata: {name: b}\nspec: {groups: []}\n", true},
	{"x.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rules\ndata:\n  shop.yaml: |\n    groups: []\n", true},
	{"x.json", `{"groups": [{"name": "g", "rules": []}]}`, true},
	{"X.JSON", `{"apiVersion": "monitoring.coreos.com/v1", "kind": "PrometheusRule", "metadata": {}, "spec": {"groups": []}}`, true},

	{"x.yaml", "", false},
	{"x.yaml", "# nothing\n", false},
	{"x.yaml", "on: [push]\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps: [{run: env}]\n", false},
	{"x.yaml", "groups:\n  script: env\n", false},
	{"x.yaml", "groups:\nsteal:\n  script: env\n", false},
	{"x.yaml", "groups: []\nsteal:\n  script: env\n", false},
	{"x.yaml", "groups: []\ninclude:\n  - local: x.yml\n", false},
	{"x.yaml", "groups: 1\n", false},
	{"x.yaml", "groups:\n", false},
	{"x.yaml", "{groups: [], jobs: {steal: {script: env}}}\n", false},
	{"x.yaml", "groups: []\n\"jobs\": {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\n'jobs': {}\n", false},
	{"x.yaml", "groups: []\n? jobs\n: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\n<<: {jobs: {steal: {script: env}}}\n", false},
	{"x.yaml", "groups: &g []\n", false},
	{"x.yaml", "groups: !!seq []\n", false},
	{"x.yaml", "--- !!map\ngroups: []\n", false},
	{"x.yaml", "%YAML 1.1\n---\ngroups: []\n", false},
	{"x.yaml", "--- |\n  groups: []\n", false},
	{"x.yaml", "- groups: []\n", false},
	{"x.yaml", "groups: []\ngroups: []\n", false},
	{"x.yaml", "groups: []\n---\non: push\njobs: {}\n", false},
	{"x.yaml", "apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata: {}\nspec: {}\n", false},
	{"x.yaml", "kind: pipeline\ntype: docker\nsteps: []\n", false},
	{"x.yaml", "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata: {}\nspec: {}\njobs: {}\n", false},
	{"x.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n", false},
	{"x.yaml", "groups: []\rjobs: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\u0085jobs: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\u2028jobs: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\u2029jobs: {steal: {script: env}}\n", false},
	{"x.yaml", "groups: []\x00\njobs: {}\n", false},
	{"x.yaml", "\ufeffgroups: []\n", false},
	{"x.yaml", "\tjobs: {}\ngroups: []\n", false},
	{"x.json", `{"groups": [], "jobs": {}}`, false},
	{"x.json", `{"groups": {}}`, false},
	{"x.json", `[{"groups": []}]`, false},
	{"x.json", `{"groups": []} {"jobs": {}}`, false},
	{"x.json", `{"kind": "Pipeline", "spec": {}}`, false},
}

// ruleFile asks lib.sh's rule_file of each document, in one shell.
func ruleFileSays(t *testing.T, cases []struct {
	name, body string
	ok         bool
}) []bool {
	t.Helper()
	dir := t.TempDir()
	var script strings.Builder
	script.WriteString(`. ` + changeRecipes + "lib.sh\n")
	for i, c := range cases {
		f := filepath.Join(dir, fmt.Sprint(i))
		os.WriteFile(f, []byte(c.body), 0o644)
		fmt.Fprintf(&script, "if rule_file %q %q; then echo 1; else echo 0; fi\n", f, c.name)
	}
	cmd := exec.Command("sh", "-c", script.String())
	cmd.Env = append(os.Environ(), "LAPILLI_URL=http://x", "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE=x", "RULES_PATHS=alerts")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var says []bool
	for _, l := range strings.Fields(string(out)) {
		says = append(says, l == "1")
	}
	if len(says) != len(cases) {
		t.Fatalf("%d answers for %d documents: %q", len(says), len(cases), out)
	}
	return says
}

// topKeys are the keys at the top of each document a YAML parser reads,
// and whether each groups is a list.
func topKeys(body string) (keys [][]string, lists bool, err error) {
	dec := yaml.NewDecoder(strings.NewReader(body))
	lists = true
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err == io.EOF {
			return keys, lists, nil
		} else if err != nil {
			return nil, false, err
		}
		var ks []string
		for k := range doc {
			ks = append(ks, k)
		}
		if g, ok := doc["groups"]; ok {
			_, isList := g.([]any)
			lists = lists && isList
		}
		keys = append(keys, ks)
	}
}

var ruleKeys = []string{"groups", "namespace", "apiVersion", "deleteRules", "contactPoints", "deleteContactPoints", "policies",
	"resetPolicies", "templates", "deleteTemplates", "muteTimes", "deleteMuteTimes", "kind", "metadata", "spec", "data"}

// rule_file takes alert rules, and refuses anything else. Whatever it takes,
// a YAML parser, as a CI system's is, reads as keys of rule files alone,
// each groups a list: documents made at random from pieces of rule files
// and of CI systems' files are held to that, beside those above.
func TestRuleFile(t *testing.T) {
	needTools(t)
	says := ruleFileSays(t, ruleFileCases)
	for i, c := range ruleFileCases {
		if says[i] != c.ok {
			t.Errorf("%s %q: rule_file says %v", c.name, c.body, says[i])
		}
	}
	pieces := []string{"groups:\n", "groups: []\n", "  - name: g\n", "- name: g\n", "    rules: []\n", "jobs:\n", "  steal: {script: env}\n",
		"steps: [{run: env}]\n", "---\n", "# c\n", "\n", "apiVersion: 1\n", "kind: PrometheusRule\n", "kind: ConfigMap\n", "spec: {}\n",
		"data: {}\n", "metadata: {}\n", "\"jobs\": {}\n", "  script: env\n", "groups: [\n", "]\n", "script: env\n", "\r", "\u2028",
		"\u0085", "\t", "templates:\n", "policies: x\n", "? jobs\n", ": {}\n", "<<: {jobs: {}}\n", "groups: &a []\n", "x: *a\n", "...\n",
		"%YAML 1.1\n", "--- |\n", "namespace: n\n", "groups: |\n", "  jobs: {}\n"}
	r := rand.New(rand.NewPCG(11, 12))
	var random []struct {
		name, body string
		ok         bool
	}
	for range 600 {
		var b bytes.Buffer
		for range 1 + r.IntN(8) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		random = append(random, struct {
			name, body string
			ok         bool
		}{"x.yaml", b.String(), false})
	}
	took := 0
	for i, ok := range ruleFileSays(t, random) {
		if !ok {
			continue
		}
		took++
		keys, lists, err := topKeys(random[i].body)
		if err != nil {
			continue // no parser runs it either
		}
		for _, ks := range keys {
			for _, k := range ks {
				if !slices.Contains(ruleKeys, k) {
					t.Errorf("rule_file took %q, which YAML reads with the key %q", random[i].body, k)
				}
			}
		}
		if !lists {
			t.Errorf("rule_file took %q, whose groups YAML reads as no list", random[i].body)
		}
	}
	t.Logf("%d of %d documents made at random taken", took, len(random))
	// And what it refuses for its line breaks, a YAML parser reads as more.
	for _, c := range ruleFileCases {
		if c.ok || !strings.ContainsAny(c.body, "\r\u0085\u2028\u2029") || strings.HasSuffix(c.body, "\r\n") {
			continue
		}
		if keys, _, err := topKeys(c.body); err == nil && len(keys) > 0 && !slices.Contains(keys[0], "jobs") {
			t.Logf("%q: read as %v", c.body, keys)
		}
	}
}

// A branch is never pushed over: a Lapilli someone took over, handing out
// again as ready a change whose pull request people reviewed, or two
// ledgers proposing to one repository under the same id, had the recipe
// force-push the branch, keeping the pull request's approvals and
// auto-merge. A branch holding this very change is a run's that stopped,
// and its pull request is opened or found; one holding another change is
// left, and the change fails, saying why. A branch a recipe before 0.28.1
// pushed, lapilli/proposal-ID, is taken when it holds this change, and
// left as it is when not.
func TestChangeRecipeNeverPushesOver(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	proposed := strings.Replace(ruleFile, "for: 2m", "for: 15m", 1)
	reviewed := strings.Replace(ruleFile, "for: 2m", "for: 10m", 1)
	const ledger = "0123456789ab"
	ours, legacy := "lapilli/"+ledger+"/proposal-7", "lapilli/proposal-7"
	for _, c := range []struct {
		name      string
		ledger    string
		branches  map[string][2]string
		want      string
		pushed    []string
		opened    int
		unchanged []string
	}{
		{"none there", ledger, nil, `"state":"opened"`, []string{ours}, 1, nil},
		{"another change there", ledger, map[string][2]string{ours: {"alerts/shop.yaml", reviewed}},
			"holds another change already", []string{ours}, 0, []string{ours}},
		{"this change there", ledger, map[string][2]string{ours: {"alerts/shop.yaml", proposed}}, `"state":"opened"`, []string{ours}, 1, []string{ours}},
		{"a recipe's before 0.28.1, this change", ledger, map[string][2]string{legacy: {"alerts/shop.yaml", proposed}},
			`"state":"opened"`, []string{legacy}, 1, []string{legacy}},
		{"a recipe's before 0.28.1, another change", ledger, map[string][2]string{legacy: {"alerts/shop.yaml", reviewed}},
			"is left as it is", []string{ours, legacy}, 1, []string{legacy}},
		{"a Lapilli before 0.28.1, another change there", "", map[string][2]string{legacy: {"alerts/shop.yaml", reviewed}},
			"holds another change already", []string{legacy}, 0, []string{legacy}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := gitEnv(t)
			bare := repo(t, env, map[string]string{"alerts/shop.yaml": ruleFile}, nil, c.branches)
			before := map[string]string{}
			for b := range c.branches {
				before[b] = strings.TrimSpace(gitIn(t, env, bare, "rev-parse", b))
			}
			change := map[string]any{"id": 7, "rule": "Errors", "kind": "for", "state": "ready", "doc_hash": sum(ruleFile),
				"document": proposed, "title": "Lapilli: a new for, Errors", "body": "Backtest"}
			if c.ledger != "" {
				change["ledger"] = c.ledger
			}
			lapilli := newStandIn(t, []map[string]any{change})
			host, hostURL := githubStandIn(t)
			out, _ := runChangeRecipe(t, env, lapilli.URL, hostURL, bare, "alerts")
			if said := out + lapilli.said(); !strings.Contains(said, c.want) {
				t.Errorf("want %q, said:\n%s", c.want, said)
			}
			if got := branches(t, env, bare); !slices.Equal(got, c.pushed) {
				t.Errorf("branches %v, want %v", got, c.pushed)
			}
			if len(host.opened) != c.opened {
				t.Errorf("opened %v, want %d", host.opened, c.opened)
			}
			for _, b := range c.unchanged {
				if now := strings.TrimSpace(gitIn(t, env, bare, "rev-parse", b)); now != before[b] {
					t.Errorf("%s pushed over: %s, was %s", b, now, before[b])
				}
			}
		})
	}
}

// A run pushes MAX_CHANGES branches at most, ten unless given: a Lapilli
// someone took over, handing out fifty changes, had one run push fifty
// branches and open fifty pull requests, each push setting off the
// repository's CI. The rest wait for the next run, and the run says so.
func TestChangeRecipeMaxChanges(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	var changes []map[string]any
	for i := range 15 {
		changes = append(changes, map[string]any{"id": i + 1, "ledger": "0123456789ab", "rule": "Errors", "kind": "for", "state": "ready",
			"doc_hash": sum(ruleFile), "document": strings.Replace(ruleFile, "for: 2m", fmt.Sprintf("for: %dm", 10+i), 1),
			"title": "Lapilli: a new for, Errors", "body": "Backtest"})
	}
	for _, c := range []struct {
		max  string
		want int
	}{{"", 10}, {"3", 3}} {
		env := gitEnv(t)
		bare := repo(t, env, map[string]string{"alerts/shop.yaml": ruleFile}, nil, nil)
		lapilli := newStandIn(t, changes)
		host, hostURL := githubStandIn(t)
		var more []string
		if c.max != "" {
			more = append(more, "MAX_CHANGES="+c.max)
		}
		out, err := runChangeRecipe(t, env, lapilli.URL, hostURL, bare, "alerts", more...)
		if err != nil {
			t.Errorf("MAX_CHANGES %q: %v %s", c.max, err, out)
		}
		if got := branches(t, env, bare); len(got) != c.want || len(host.opened) != c.want {
			t.Errorf("MAX_CHANGES %q: %d branches, %d pull requests, want %d", c.max, len(got), len(host.opened), c.want)
		}
		if want := fmt.Sprintf("%d changes ready are left for the next run", 15-c.want); !strings.Contains(out, want) {
			t.Errorf("MAX_CHANGES %q: no %q in %s", c.max, want, out)
		}
	}
	for _, bad := range []string{"0", "ten", "-1", "07"} {
		env := gitEnv(t)
		lapilli := newStandIn(t, changes)
		_, hostURL := githubStandIn(t)
		if out, err := runChangeRecipe(t, env, lapilli.URL, hostURL, "x", "alerts", "MAX_CHANGES="+bad); err == nil || !strings.Contains(out, "MAX_CHANGES") {
			t.Errorf("MAX_CHANGES %s: %v %s", bad, err, out)
		}
	}
}

// What Lapilli or the host answers reaches the job's log as JSON: escape
// sequences in it (a window's title, a cleared screen, a line made to look
// like the recipe's own) were written to the terminal or CI log as they
// came.
func TestChangeRecipeShowsNoEscapes(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	evil := "\x1b]0;owned\x07\x1b[2J\x1b[Hgithub.sh: all changes opened\x1b[0m"
	lapilli := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, evil)
	}))
	defer lapilli.Close()
	env := gitEnv(t)
	bare := repo(t, env, map[string]string{"alerts/shop.yaml": ruleFile}, nil, nil)
	_, hostURL := githubStandIn(t)
	out, _ := runChangeRecipe(t, env, lapilli.URL, hostURL, bare, "alerts")
	if strings.ContainsRune(out, '\x1b') || !strings.Contains(out, `\u001b]0;owned\u0007`) {
		t.Errorf("Lapilli's answer, said as: %q", out)
	}
	// The host's, refusing a pull request.
	ready := newStandIn(t, []map[string]any{{"id": 1, "ledger": "0123456789ab", "rule": "Errors", "kind": "for", "state": "ready",
		"doc_hash": sum(ruleFile), "document": strings.Replace(ruleFile, "for: 2m", "for: 15m", 1), "title": "t", "body": "b"}})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login": "lapilli-bot"}`)
		case r.Method == http.MethodGet:
			io.WriteString(w, "[]")
		default:
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, evil)
		}
	}))
	defer host.Close()
	out, _ = runChangeRecipe(t, env, ready.URL, host.URL, bare, "alerts")
	if strings.ContainsRune(out, '\x1b') || !strings.Contains(out, `GitHub didn't open a pull request (422): "\u001b]0;owned`) {
		t.Errorf("the host's answer, said as: %q", out)
	}
}

// A token in CLONE is refused wherever it is in the URL's authority: as
// the user alone (https://TOKEN@host/...), a form GitHub and others take,
// it went on git's command line, which ps shows, and into the clone's
// .git/config. An ssh URL's user is no secret.
func TestChangeRecipeNoTokenInClone(t *testing.T) {
	needTools(t)
	bin := t.TempDir()
	argv := filepath.Join(bin, "argv")
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+argv+"\nexit 1\n"), 0o755)
	lapilli := newStandIn(t, []map[string]any{{"id": 1, "ledger": "0123456789ab", "rule": "Errors", "kind": "for", "state": "ready",
		"doc_hash": sum(ruleFile), "document": ruleFile, "title": "t", "body": "b"}})
	_, hostURL := githubStandIn(t)
	path := "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")
	for clone, refused := range map[string]bool{
		"https://ghp_SECRET0123456789@github.com/o/r.git":             true,
		"HTTPS://ghp_SECRET0123456789@github.com/o/r.git":             true,
		"http://ghp_SECRET0123456789@git.example.com:3000/o/r.git":    true,
		"https://x-access-token:ghp_SECRET0123456789@github.com/o/r":  true,
		"ssh://u:ghp_SECRET0123456789@git.example.com/o/r.git":        true,
		"ssh://git@git.example.com/o/r.git":                           false,
		"git@git.example.com:o/r.git":                                 false,
		"https://git.example.com/o/r.git":                             false,
		"https://git.example.com/o/r@main.git":                        false,
		"https://git.example.com:8443/scm/ghp_SECRET0123456789/r.git": false,
	} {
		os.Remove(argv)
		out, _ := runChangeRecipe(t, nil, lapilli.URL, hostURL, clone, "alerts", path)
		said := strings.Contains(out, "give git its credentials as your CI does")
		ran, _ := os.ReadFile(argv)
		if said != refused || refused && len(ran) > 0 {
			t.Errorf("CLONE %s: refused %v, want %v; git ran with %q\n%s", clone, said, refused, ran, out)
		}
	}
}

// The ledger's name, in the recipes' branches, is made once, at random,
// and kept: a restart doesn't move a change to another branch.
func TestLedgerIDKept(t *testing.T) {
	s, _ := server(t, nil)
	if len(s.ledgerID) != 12 || strings.Trim(s.ledgerID, "0123456789abcdef") != "" {
		t.Fatalf("ledger id %q", s.ledgerID)
	}
	again, err := New(t.Context(), s.cfg, &history{}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.ledgerID != s.ledgerID {
		t.Errorf("opened again: %q, was %q", again.ledgerID, s.ledgerID)
	}
	other, _ := server(t, nil)
	if other.ledgerID == s.ledgerID {
		t.Errorf("two ledgers, one name: %q", s.ledgerID)
	}
}
