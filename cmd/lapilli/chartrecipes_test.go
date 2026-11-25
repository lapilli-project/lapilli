package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The chart mounts the recipes from its own copies, so that they travel in
// the packaged chart: charts/lapilli/files/recipes/{incidents,changes,responses} must
// hold what recipes/incidents, recipes/changes and recipes/responses do, their READMEs and
// testdata aside, and nothing else.
func TestChartRecipesAreTheRepositorys(t *testing.T) {
	list := func(dir string) map[string]string {
		got := map[string]string{}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || e.Name() == "README.md" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			got[e.Name()] = string(b)
		}
		return got
	}
	const fix = "run charts/sync-recipes.sh and commit what it changes"
	for _, kind := range []string{"incidents", "changes", "responses"} {
		want := list(filepath.Join("..", "..", "recipes", kind))
		got := list(filepath.Join("..", "..", "charts", "lapilli", "files", "recipes", kind))
		if len(want) == 0 {
			t.Fatalf("recipes/%s holds nothing", kind)
		}
		for name, w := range want {
			if g, ok := got[name]; !ok {
				t.Errorf("charts/lapilli/files/recipes/%s/%s is missing: %s", kind, name, fix)
			} else if g != w {
				t.Errorf("charts/lapilli/files/recipes/%s/%s differs from recipes/%s/%s: %s", kind, name, kind, name, fix)
			}
		}
		for name := range got {
			if _, ok := want[name]; !ok {
				t.Errorf("charts/lapilli/files/recipes/%s/%s is not in recipes/%s: %s", kind, name, kind, fix)
			}
		}
		core := map[string]string{"incidents": "put.sh", "changes": "lib.sh", "responses": "put.sh"}[kind]
		if _, ok := got[core]; !ok {
			t.Errorf("charts/lapilli/files/recipes/%s has no %s", kind, core)
		}
	}
}

var recipesCopyValues = []string{"--set", "serve.incidentsAPI.tokenSecret.name=lapilli-incidents",
	"--set", "serve.incidentsAPI.copy.enabled=true", "--set", "serve.incidentsAPI.copy.image=example.com/tools/recipes:1.0",
	"--set", "serve.incidentsAPI.copy.jobs[0].name=pagerduty", "--set", "serve.incidentsAPI.copy.jobs[0].recipe=pagerduty",
	"--set", "serve.incidentsAPI.copy.jobs[0].env.SINCE=2026-10-01",
	"--set", "serve.incidentsAPI.copy.jobs[0].tokenFiles.PAGERDUTY_TOKEN_FILE.name=pagerduty-read"}

var recipesPRValues = []string{"--set", "serve.changes.enabled=true", "--set", "serve.userHeader=X-Forwarded-Email",
	"--set", "serve.roles.admins={ana@example.com}", "--set", "serve.networkPolicy.enabled=true", "--set", "serve.networkPolicy.from[0].podSelector.matchLabels.app=proxy",
	"--set", "serve.changes.tokenSecret.name=lapilli-changes",
	"--set", "serve.changes.pullRequests.enabled=true", "--set", "serve.changes.pullRequests.host=github",
	"--set", "serve.changes.pullRequests.image=example.com/tools/recipes:1.0",
	"--set", "serve.changes.pullRequests.tokenSecret.name=github-pr", "--set", "serve.changes.pullRequests.gitCredentialsSecret.name=github-git",
	"--set", "serve.changes.pullRequests.env.CLONE=https://github.com/example/monitoring.git",
	"--set", "serve.changes.pullRequests.env.RULES_PATHS=alerts", "--set", "serve.changes.pullRequests.env.GITHUB_REPO=example/monitoring"}

// serve.incidentsAPI.copy and serve.changes.pullRequests are off by
// default and add nothing; on, each runs its recipe from a ConfigMap of the
// chart's files, as a non-root pod with a read-only root, the tokens as
// read-only files, and the NetworkPolicy lets only these pods reach the
// incidents port.
func TestChartRecipeJobs(t *testing.T) {
	m, _ := render(t, serveValues...)
	for k := range m {
		if strings.Contains(k, "recipes") || strings.Contains(k, "incidents-") || strings.Contains(k, "changes-pr") {
			t.Errorf("by default: %s", k)
		}
	}
	m, _ = render(t, slices.Concat(serveValues, recipesCopyValues, []string{"--set", "serve.networkPolicy.enabled=true", "--set", "serve.networkPolicy.from[0].podSelector.matchLabels.app=proxy",
		"--set", "serve.userHeader=X-Forwarded-Email"})...)
	if m["ConfigMap/ops-lapilli-recipes-incidents"] == nil || m["ConfigMap/ops-lapilli-recipes-changes"] != nil || m["CronJob/ops-lapilli-changes-pr"] != nil {
		t.Errorf("the incidents' alone: %v", m)
	}
	data, _ := dig(m["ConfigMap/ops-lapilli-recipes-incidents"], "data").(map[string]any)
	for _, f := range []string{"pagerduty.sh", "pagerduty.jq", "put.sh"} {
		if data[f] == nil {
			t.Errorf("the ConfigMap has no %s", f)
		}
	}
	job := m["CronJob/ops-lapilli-incidents-pagerduty"]
	pod := func(job map[string]any) any { return dig(job, "spec", "jobTemplate", "spec", "template", "spec") }
	c := dig(pod(job), "containers", 0)
	if dig(job, "spec", "schedule") != "*/15 * * * *" || dig(job, "spec", "concurrencyPolicy") != "Forbid" || dig(c, "image") != "example.com/tools/recipes:1.0" ||
		dig(pod(job), "securityContext", "runAsNonRoot") != true || dig(c, "securityContext", "readOnlyRootFilesystem") != true ||
		dig(pod(job), "automountServiceAccountToken") != false || dig(c, "securityContext", "capabilities", "drop", 0) != "ALL" ||
		dig(pod(job), "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
		t.Errorf("the CronJob: %v", job)
	}
	if cmd, _ := dig(c, "command", 2).(string); !strings.Contains(cmd, "sh /recipes/incidents/pagerduty.sh") || !strings.Contains(cmd, "sh /recipes/incidents/put.sh") {
		t.Errorf("the command: %v", dig(c, "command"))
	}
	env := map[string]string{}
	for _, e := range dig(c, "env").([]any) {
		env[dig(e, "name").(string)], _ = dig(e, "value").(string)
	}
	for k, v := range map[string]string{"LAPILLI_URL": "http://ops-lapilli:8082", "LAPILLI_TOKEN_FILE": "/var/run/lapilli/incidents-token/token", "SINCE": "2026-10-01",
		"PAGERDUTY_TOKEN_FILE": "/var/run/lapilli/tool-token/pagerduty_token_file/token", "TMPDIR": "/tmp"} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	mounts := map[string]bool{}
	for _, mt := range dig(c, "volumeMounts").([]any) {
		mounts[dig(mt, "mountPath").(string)] = dig(mt, "readOnly") == true
	}
	for _, p := range []string{"/recipes/incidents", "/var/run/lapilli/incidents-token", "/var/run/lapilli/tool-token/pagerduty_token_file"} {
		if !mounts[p] {
			t.Errorf("%s isn't mounted read-only: %v", p, mounts)
		}
	}
	np, _ := yaml.Marshal(m["NetworkPolicy/ops-lapilli"])
	if !strings.Contains(string(np), "app.kubernetes.io/component: incidents-copy") {
		t.Errorf("the NetworkPolicy lets the job in?\n%s", np)
	}

	m, _ = render(t, slices.Concat(serveValues, recipesPRValues)...)
	job = m["CronJob/ops-lapilli-changes-pr"]
	c = dig(pod(job), "containers", 0)
	if dig(job, "spec", "schedule") != "*/5 * * * *" || dig(c, "command", 1) != "/recipes/changes/github.sh" || dig(c, "securityContext", "readOnlyRootFilesystem") != true ||
		dig(pod(job), "securityContext", "runAsNonRoot") != true {
		t.Errorf("the pull requests' CronJob: %v", job)
	}
	env = map[string]string{}
	for _, e := range dig(c, "env").([]any) {
		env[dig(e, "name").(string)], _ = dig(e, "value").(string)
	}
	for k, v := range map[string]string{"LAPILLI_URL": "http://ops-lapilli:8082", "LAPILLI_TOKEN_FILE": "/var/run/lapilli/changes-token/token", "GITHUB_TOKEN_FILE": "/var/run/lapilli/host-token/token",
		"CLONE": "https://github.com/example/monitoring.git", "RULES_PATHS": "alerts", "GITHUB_REPO": "example/monitoring", "GIT_CONFIG_VALUE_0": `!f() { test "$1" = get || exit 0; exec git credential-store --file=/var/run/lapilli/git-credentials/credentials get; }; f`} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	data, _ = dig(m["ConfigMap/ops-lapilli-recipes-changes"], "data").(map[string]any)
	if data["github.sh"] == nil || data["lib.sh"] == nil {
		t.Errorf("the changes' ConfigMap: %v", data)
	}
	np, _ = yaml.Marshal(m["NetworkPolicy/ops-lapilli"])
	if !strings.Contains(string(np), "app.kubernetes.io/component: changes-pr") {
		t.Errorf("the NetworkPolicy lets the job in?\n%s", np)
	}

	for name, c := range map[string]struct {
		args []string
		says string
	}{
		"no image": {slices.Concat(serveValues, recipesCopyValues, []string{"--set", "serve.incidentsAPI.copy.image="}),
			"serve.incidentsAPI.copy.image is required: an image with sh, curl and jq"},
		"an unknown recipe": {slices.Concat(serveValues, recipesCopyValues, []string{"--set", "serve.incidentsAPI.copy.jobs[0].recipe=opsgenie"}), `recipe "opsgenie" is not one of`},
		"put as a recipe":   {slices.Concat(serveValues, recipesCopyValues, []string{"--set", "serve.incidentsAPI.copy.jobs[0].recipe=put"}), `recipe "put" is not one of`},
		"a token set by hand": {slices.Concat(serveValues, recipesCopyValues, []string{"--set", "serve.incidentsAPI.copy.jobs[0].env.PAGERDUTY_TOKEN_FILE=/x"}),
			"is set by the chart"},
		"no jobs":         {slices.Concat(serveValues, []string{"--set", "serve.incidentsAPI.tokenSecret.name=t", "--set", "serve.incidentsAPI.copy.enabled=true", "--set", "serve.incidentsAPI.copy.image=x"}), "name at least one job"},
		"no token":        {slices.Concat(serveValues, []string{"--set", "serve.incidentsAPI.copy.enabled=true", "--set", "serve.incidentsAPI.copy.image=x"}), "needs serve.incidentsAPI.tokenSecret"},
		"no PR image":     {slices.Concat(serveValues, recipesPRValues, []string{"--set", "serve.changes.pullRequests.image="}), "serve.changes.pullRequests.image is required: an image with sh, git, curl and jq"},
		"an unknown host": {slices.Concat(serveValues, recipesPRValues, []string{"--set", "serve.changes.pullRequests.host=svn"}), `host: "svn" is not one of`},
		"no CLONE":        {slices.Concat(serveValues, recipesPRValues, []string{"--set", "serve.changes.pullRequests.env.CLONE="}), "env.CLONE is required"},
		"no changes":      {slices.Concat(serveValues, recipesPRValues, []string{"--set", "serve.changes.enabled=false"}), "needs serve.changes.enabled"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.says) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
}

// A job's Secret volumes project the one key the job reads, so a pod that
// holds one token can't read the others kept in the same Secret, and the
// files are group-readable (the pod's fsGroup) and no wider.
func TestChartJobSecretsProjectOneKey(t *testing.T) {
	copyVals := slices.Concat(recipesCopyValues, []string{"--set", "serve.incidentsAPI.copy.jobs[0].tokenFiles.PAGERDUTY_TOKEN_FILE.key=pd"})
	m, _ := render(t, slices.Concat(serveValues, copyVals, recipesPRValues, trackersValues,
		[]string{"--set", "serve.incidentsAPI.tokenSecret.key=incidents", "--set", "serve.changes.tokenSecret.key=changes"})...)
	check := func(job string, want map[string]string) {
		t.Helper()
		vols, _ := dig(m[job], "spec", "jobTemplate", "spec", "template", "spec", "volumes").([]any)
		got := map[string]string{}
		for _, v := range vols {
			s, ok := dig(v, "secret").(map[string]any)
			if !ok {
				continue
			}
			items, _ := s["items"].([]any)
			if len(items) != 1 || dig(items[0], "key") != dig(items[0], "path") || s["defaultMode"] != 288 {
				t.Errorf("%s: %v: want one item and mode 0440: %v", job, dig(v, "name"), s)
				continue
			}
			got[s["secretName"].(string)] = dig(items[0], "key").(string)
		}
		for name, key := range want {
			if got[name] != key {
				t.Errorf("%s: Secret %s projects %q, want %q (all: %v)", job, name, got[name], key, got)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: secrets %v, want %v", job, got, want)
		}
	}
	check("CronJob/ops-lapilli-incidents-pagerduty", map[string]string{"lapilli-incidents": "incidents", "pagerduty-read": "pd"})
	check("CronJob/ops-lapilli-changes-pr", map[string]string{"lapilli-changes": "changes", "github-pr": "token", "github-git": "credentials"})
	check("CronJob/ops-lapilli-trackers", map[string]string{"tasks": "token", "gh": "token"})
}

// The jobs' /tmp is an emptyDir with a size limit, from a value: 256Mi for
// the copy jobs and 2Gi for the pull requests', which holds a clone; and a
// git credentials key of . or .. is refused.
func TestChartJobTmpSizeLimit(t *testing.T) {
	limit := func(m manifest, job string) any {
		vols, _ := dig(m[job], "spec", "jobTemplate", "spec", "template", "spec", "volumes").([]any)
		for _, v := range vols {
			if dig(v, "name") == "tmp" {
				return dig(v, "emptyDir", "sizeLimit")
			}
		}
		return nil
	}
	m, _ := render(t, slices.Concat(serveValues, recipesCopyValues, recipesPRValues)...)
	if got := limit(m, "CronJob/ops-lapilli-incidents-pagerduty"); got != "256Mi" {
		t.Errorf("copy job's /tmp limit %v, want 256Mi", got)
	}
	if got := limit(m, "CronJob/ops-lapilli-changes-pr"); got != "2Gi" {
		t.Errorf("pull requests' /tmp limit %v, want 2Gi", got)
	}
	m, _ = render(t, slices.Concat(serveValues, recipesCopyValues, recipesPRValues,
		[]string{"--set", "serve.incidentsAPI.copy.tmpSizeLimit=64Mi", "--set", "serve.changes.pullRequests.tmpSizeLimit=5Gi"})...)
	if a, b := limit(m, "CronJob/ops-lapilli-incidents-pagerduty"), limit(m, "CronJob/ops-lapilli-changes-pr"); a != "64Mi" || b != "5Gi" {
		t.Errorf("set limits %v and %v, want 64Mi and 5Gi", a, b)
	}
	for _, key := range []string{".", ".."} {
		out, err := helmTemplate(t, slices.Concat(serveValues, recipesPRValues, []string{"--set", "serve.changes.pullRequests.gitCredentialsSecret.key=" + key})...)
		if err == nil || !strings.Contains(out, "gitCredentialsSecret.key must be a Secret's key") {
			t.Errorf("key %q: %v\n%s", key, err, out)
		}
	}
}

// serve.responses: the token is passed and mounted as its own file and
// refused when it is another port's; the file is mounted and passed;
// rollouts, rolloutMatch, routine and noisyBy are the flags; a job running a
// recipe from responses/ sends with the responses token, with both recipe
// directories mounted side by side, as the recipes need ../incidents.
func TestChartResponses(t *testing.T) {
	m, _ := render(t, serveValues...)
	args := func(m manifest) []string {
		var out []string
		for _, a := range dig(m["Deployment/ops-lapilli"], "spec", "template", "spec", "containers", 0, "args").([]any) {
			out = append(out, a.(string))
		}
		return out
	}
	for _, a := range args(m) {
		for _, f := range []string{"--responses", "--rollout", "--routine", "--noisy-by"} {
			if strings.HasPrefix(a, f) {
				t.Errorf("by default: %s", a)
			}
		}
	}
	vals := slices.Concat(serveValues, []string{"--set", "serve.responses.tokenSecret.name=lapilli-responses", "--set", "serve.responses.configMap.name=pages",
		"--set", "serve.networkPolicy.enabled=true", "--set", "serve.networkPolicy.from[0].podSelector.matchLabels.app=proxy", "--set", "serve.incidentsAPI.from[0].podSelector.matchLabels.app=job",
		"--set", "serve.userHeader=X-Forwarded-Email",
		"--set", "rollouts=true", "--set", "rolloutMatch={service,app}", "--set", "routine=0.7", "--set", "noisyBy=useful"})
	m, _ = render(t, vals...)
	got := args(m)
	for _, want := range []string{"--responses-token-file=/var/run/lapilli/responses-token/token", "--responses=/etc/lapilli/responses/responses.yaml", "--rollouts",
		"--rollout-match=service", "--rollout-match=app", "--routine=0.7", "--noisy-by=useful", "--incidents-listen=:8082"} {
		if !slices.Contains(got, want) {
			t.Errorf("no %s in %v", want, got)
		}
	}
	// Its own token.
	if out, err := helmTemplate(t, slices.Concat(vals, []string{"--set", "serve.incidentsAPI.tokenSecret.name=lapilli-responses"})...); err == nil || !strings.Contains(out, "another port's token") {
		t.Errorf("the incidents token as the responses token: %v\n%s", err, out)
	}
	if out, err := helmTemplate(t, slices.Concat(serveValues, []string{"--set", "noisyBy=useful"})...); err == nil || !strings.Contains(out, "noisyBy: useful") {
		t.Errorf("noisyBy useful with nothing to find it by: %v\n%s", err, out)
	}
	// A job of the pages' recipes.
	jobs := slices.Concat(vals, []string{"--set", "serve.incidentsAPI.copy.enabled=true", "--set", "serve.incidentsAPI.copy.image=example.com/tools/recipes:1.0",
		"--set", "serve.incidentsAPI.copy.jobs[0].name=pd-pages", "--set", "serve.incidentsAPI.copy.jobs[0].recipe=responses/pagerduty",
		"--set", "serve.incidentsAPI.copy.jobs[0].source=pagerduty-prod", "--set", "serve.incidentsAPI.copy.jobs[0].sinceDays=2",
		"--set", "serve.incidentsAPI.copy.jobs[0].tokenFiles.PAGERDUTY_TOKEN_FILE.name=pagerduty-read"})
	m, _ = render(t, jobs...)
	job := m["CronJob/ops-lapilli-incidents-pd-pages"]
	c := dig(job, "spec", "jobTemplate", "spec", "template", "spec", "containers", 0)
	cmd, _ := dig(c, "command", 2).(string)
	if !strings.Contains(cmd, "sh /recipes/responses/pagerduty.sh") || !strings.Contains(cmd, "sh /recipes/responses/put.sh \"$LAPILLI_URL\" pagerduty-prod") || !strings.Contains(cmd, "2 * 86400") {
		t.Errorf("the command: %s", cmd)
	}
	mounts := map[string]bool{}
	for _, mt := range dig(c, "volumeMounts").([]any) {
		mounts[dig(mt, "mountPath").(string)] = dig(mt, "readOnly") == true
	}
	for _, p := range []string{"/recipes/incidents", "/recipes/responses"} {
		if !mounts[p] {
			t.Errorf("%s isn't mounted read-only: %v", p, mounts)
		}
	}
	data, _ := dig(m["ConfigMap/ops-lapilli-recipes-responses"], "data").(map[string]any)
	for _, f := range []string{"pagerduty.sh", "pagerduty.jq", "put.sh", "responses.jq"} {
		if data[f] == nil {
			t.Errorf("the responses ConfigMap has no %s", f)
		}
	}
	if out, err := helmTemplate(t, slices.Concat(jobs, []string{"--set", "serve.incidentsAPI.copy.jobs[0].source="})...); err == nil || !strings.Contains(out, "needs source") {
		t.Errorf("no source: %v\n%s", err, out)
	}
}
