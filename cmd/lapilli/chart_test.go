package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// helmTemplate renders the chart with the values given; it skips the test
// without helm.
func helmTemplate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := filepath.Join("..", "..", "charts", "lapilli")
	cmd := exec.Command("helm", append([]string{"template", "ops", chart}, args...)...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var serveValues = []string{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={rules}"}

// The chart listens on the pod's address, which every pod in the cluster
// reaches: with no roles, lapilli serve takes no changes there, unless
// serve.everyoneAdmin says everyone may make them. Off by default, and
// refused beside roles or serve.readOnly.
func TestChartEveryoneAdmin(t *testing.T) {
	out, err := helmTemplate(t, serveValues...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "--everyone-admin") || !strings.Contains(out, "- --listen=:8080") {
		t.Errorf("by default: %s", out)
	}
	out, err = helmTemplate(t, append(serveValues, "--set", "serve.everyoneAdmin=true")...)
	if err != nil || !strings.Contains(out, "- --everyone-admin\n") {
		t.Errorf("serve.everyoneAdmin: %v\n%s", err, out)
	}
	for _, more := range [][]string{
		{"--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.othersKeptOut=true", "--set", "serve.roles.admins={ana@example.com}"},
		{"--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.othersKeptOut=true", "--set", "serve.roles.editors={*}"},
		{"--set", "serve.readOnly=true"},
	} {
		if out, err := helmTemplate(t, append(append(serveValues, "--set", "serve.everyoneAdmin=true"), more...)...); err == nil {
			t.Errorf("serve.everyoneAdmin with %v: taken\n%s", more, out)
		}
	}
}

// The pods run the image by its digest when the chart has one: a release
// writes its own image's into image.digest and the chart's
// lapilli.dev/image-digest annotation. A tag alone can be moved. Another
// tag kept with the release's own digest would run the release's image
// under another version's name: refused.
func TestChartImageDigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("ab", 32)
	cron := []string{"--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={rules}", "--set", "impact=up"}
	for _, values := range [][]string{cron, serveValues} {
		out, err := helmTemplate(t, values...)
		if err != nil || !strings.Contains(out, `image: "ghcr.io/lapilli-project/lapilli:dev"`) {
			t.Errorf("no digest: %v\n%s", err, out)
		}
		out, err = helmTemplate(t, append(values, "--set", "image.digest="+d)...)
		if err != nil || !strings.Contains(out, `image: "ghcr.io/lapilli-project/lapilli:dev@`+d+`"`) {
			t.Errorf("image.digest: %v\n%s", err, out)
		}
		out, err = helmTemplate(t, append(values, "--set", "image.digest=sha256:abc")...)
		if err == nil || !strings.Contains(out, "image.digest: want sha256:") {
			t.Errorf("a digest that isn't one: %v\n%s", err, out)
		}
	}
	// A release's chart: its own digest, kept with another tag, is refused.
	chart := t.TempDir()
	if out, err := exec.Command("cp", "-R", filepath.Join("..", "..", "charts", "lapilli")+"/.", chart).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for file, from := range map[string]string{"Chart.yaml": `lapilli.dev/image-digest: ""`, "values.yaml": `  digest: ""`} {
		b, _ := os.ReadFile(filepath.Join(chart, file))
		if strings.Count(string(b), from+"\n") != 1 {
			t.Fatalf("%s has no line %q for the release to write the digest in", file, from)
		}
		b = []byte(strings.Replace(string(b), from, strings.Replace(from, `""`, `"`+d+`"`, 1), 1))
		b = []byte(strings.Replace(string(b), "appVersion: dev", "appVersion: v0.28.1", 1))
		os.WriteFile(filepath.Join(chart, file), b, 0o644)
	}
	run := func(more ...string) (string, error) {
		cmd := exec.Command("helm", append([]string{"template", "ops", chart}, append(append([]string{}, cron...), more...)...)...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(); err != nil || !strings.Contains(out, `image: "ghcr.io/lapilli-project/lapilli:v0.28.1@`+d+`"`) {
		t.Errorf("a release's chart: %v\n%s", err, out)
	}
	if out, err := run("--set", "image.tag=v0.29.0"); err == nil || !strings.Contains(out, "give the digest of v0.29.0's image too") {
		t.Errorf("another tag, the release's digest: %v\n%s", err, out)
	}
	if out, err := run("--set", "image.tag=v0.29.0", "--set", "image.digest="); err != nil || !strings.Contains(out, `image: "ghcr.io/lapilli-project/lapilli:v0.29.0"`) {
		t.Errorf("another tag, no digest: %v\n%s", err, out)
	}
}

// A cloud's identity reaches lapilli's container, not the rules history's
// init containers, which run git and RENDER on what a repository holds: the
// service account's token is mounted in lapilli's alone (OKE's workload
// identity reads it; the pod mounts it nowhere else), and EKS's and
// Azure's webhooks are told to pass the init containers over.
func TestChartIdentityOnlyLapilli(t *testing.T) {
	history := []string{"--set", "serve.rulesHistory.repository=https://git.example.com/ops/monitoring.git", "--set", "serve.rulesHistory.paths=alerts/",
		"--set", "serve.rulesHistory.render=helm template m charts/monitoring"}
	aws := append([]string{"--set", "mode=serve", "--set", "rules.configMaps={rules}", "--set", "prometheus.auth=aws",
		"--set", "prometheus.url=https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1"}, history...)
	oci := []string{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={rules}",
		"--set", "rules.alarms[0].source=oci-prod", "--set", "rules.alarms[0].url=https://telemetry.us-ashburn-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..x"}
	for name, c := range map[string]struct {
		args  []string
		token bool
		skip  bool
	}{
		"aws, the rules history":   {aws, true, true},
		"oci alarms":               {oci, true, false},
		"no cloud":                 {append(append([]string{}, serveValues...), history...), false, true},
		"following":                {append([]string{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090"}, history...), false, true},
		"no cloud, no history":     {serveValues, false, false},
		"the CronJob, google":      {[]string{"--set", "gcpProject=p", "--set", "rules.configMaps={rules}", "--set", "impact=up"}, true, false},
		"the CronJob, no identity": {[]string{"--set", "prometheus.url=http://p:9090", "--set", "rules.configMaps={rules}", "--set", "impact=up"}, false, false},
	} {
		out, err := helmTemplate(t, c.args...)
		if err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
			continue
		}
		if strings.Contains(out, "automountServiceAccountToken: true") {
			t.Errorf("%s: the token is mounted in every container", name)
		}
		var pod struct {
			Spec struct {
				Template struct {
					Metadata struct{ Annotations map[string]string }
					Spec     podSpec
				}
				JobTemplate struct {
					Spec struct{ Template struct{ Spec podSpec } }
				} `yaml:"jobTemplate"`
			}
		}
		for _, doc := range strings.Split(out, "\n---\n") {
			if strings.Contains(doc, "kind: Deployment") || strings.Contains(doc, "kind: CronJob") && !strings.Contains(doc, "component: trackers") {
				if err := yaml.Unmarshal([]byte(doc), &pod); err != nil {
					t.Fatal(err)
				}
			}
		}
		spec := pod.Spec.Template.Spec
		if spec.Containers == nil {
			spec = pod.Spec.JobTemplate.Spec.Template.Spec
		}
		for _, ct := range append(spec.InitContainers, spec.Containers...) {
			mounted := slices.ContainsFunc(ct.VolumeMounts, func(m mount) bool { return m.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount" })
			if want := c.token && ct.Name == "lapilli"; mounted != want {
				t.Errorf("%s: the token in %s: %v, want %v", name, ct.Name, mounted, want)
			}
		}
		a := pod.Spec.Template.Metadata.Annotations
		eks, azure := "rules-history-clone,rules-history", "rules-history-clone;rules-history"
		if name == "following" {
			eks += ",rules-history-fetch,rules-history-follow"
			azure += ";rules-history-fetch;rules-history-follow"
		}
		skips := a["eks.amazonaws.com/skip-containers"] == eks && a["azure.workload.identity/skip-containers"] == azure
		if skips != c.skip {
			t.Errorf("%s: init containers passed over: %v, want %v (%v)", name, skips, c.skip, a)
		}
	}
}

type mount struct {
	MountPath string `yaml:"mountPath"`
}

type podSpec struct {
	InitContainers []struct {
		Name         string
		VolumeMounts []mount `yaml:"volumeMounts"`
	} `yaml:"initContainers"`
	Containers []struct {
		Name         string
		VolumeMounts []mount `yaml:"volumeMounts"`
	}
}

// Where lapilli serve may connect can be limited too: a NetworkPolicy of
// its own, off by default, the cluster's DNS and what egress.to names. The
// ingress-only policy let the pod reach a cloud's metadata server and the
// Kubernetes API.
func TestChartEgress(t *testing.T) {
	out, err := helmTemplate(t, serveValues...)
	if err != nil || strings.Contains(out, "Egress") {
		t.Errorf("by default: %v\n%s", err, out)
	}
	if out, err := helmTemplate(t, append(serveValues, "--set", "serve.networkPolicy.egress.enabled=true")...); err == nil ||
		!strings.Contains(out, "serve.networkPolicy.egress.to") {
		t.Errorf("egress to nowhere: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, append(serveValues, "--set", "serve.networkPolicy.egress.enabled=true",
		"--set", "serve.networkPolicy.egress.to[0].to[0].ipBlock.cidr=0.0.0.0/0",
		"--set", "serve.networkPolicy.egress.to[0].to[0].ipBlock.except[0]=169.254.169.254/32",
		"--set", "serve.networkPolicy.egress.to[0].ports[0].port=443")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var np struct {
		Metadata struct{ Name string }
		Spec     struct {
			PodSelector struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			} `yaml:"podSelector"`
			PolicyTypes []string `yaml:"policyTypes"`
			Egress      []struct {
				To    []map[string]any
				Ports []struct {
					Port     int
					Protocol string
				}
			}
		}
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		if strings.Contains(doc, "name: ops-lapilli-egress") {
			if err := yaml.Unmarshal([]byte(doc), &np); err != nil {
				t.Fatal(err)
			}
		}
	}
	e := np.Spec.Egress
	if !slices.Equal(np.Spec.PolicyTypes, []string{"Egress"}) || np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "serve" || len(e) != 2 ||
		len(e[0].To) != 0 || len(e[0].Ports) != 2 || e[0].Ports[0].Port != 53 || len(e[1].To) != 1 || e[1].Ports[0].Port != 443 {
		t.Errorf("the egress policy: %+v", np)
	}
}

// The trackers' image is a digest, as the chart's documentation says: a
// tag could be moved to an image that reads both tokens.
func TestChartTrackersImageDigest(t *testing.T) {
	trackers := append(serveValues, "--set", "serve.trackers.tokenSecret.name=tasks", "--set", "serve.trackers.tracker=github",
		"--set", "serve.trackers.trackerTokenSecret.name=gh")
	for image, ok := range map[string]bool{
		"":                           false,
		"alpine:latest":              false,
		"example/curl-jq:1.0":        false,
		"example/curl-jq@sha256:abc": false,
		"example/curl-jq@sha256:" + strings.Repeat("0", 64) + "x": false,
		"example/curl-jq@sha256:" + strings.Repeat("0", 64):       true,
		"example/curl-jq:1.0@sha256:" + strings.Repeat("a", 64):   true,
	} {
		out, err := helmTemplate(t, append(trackers, "--set", "serve.trackers.image="+image)...)
		if (err == nil) != ok {
			t.Errorf("%q: %v\n%s", image, err, out)
		}
	}
}

// historyPod is the Deployment's pod as the rules history makes it.
type historyPod struct {
	InitContainers []historyContainer `yaml:"initContainers"`
	Containers     []historyContainer
}

type historyContainer struct {
	Name         string
	Image        string
	Args         []string
	Env          []struct{ Name, Value string }
	VolumeMounts []struct {
		Name      string
		MountPath string `yaml:"mountPath"`
		ReadOnly  bool   `yaml:"readOnly"`
	} `yaml:"volumeMounts"`
}

func (c historyContainer) env(name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// mounts says how a container mounts a volume: "", "rw" or "ro".
func (c historyContainer) mounts(volume string) string {
	for _, m := range c.VolumeMounts {
		if m.Name == volume {
			if m.ReadOnly {
				return "ro"
			}
			return "rw"
		}
	}
	return ""
}

func historyPodOf(t *testing.T, out string) map[string]historyContainer {
	t.Helper()
	var d struct {
		Spec struct {
			Template struct{ Spec historyPod }
		}
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		if strings.Contains(doc, "kind: Deployment") {
			if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
				t.Fatal(err)
			}
		}
	}
	cs := map[string]historyContainer{}
	for _, c := range append(d.Spec.Template.Spec.InitContainers, d.Spec.Template.Spec.Containers...) {
		cs[c.Name] = c
	}
	return cs
}

// With rules.configMaps, the rules history is theirs, written on each
// start. With none, the repository is the rules, followed: a container
// fetches the branch with the token, and another, without it, adds the
// versions of its new commits, which lapilli serve reads with
// --rules-history and no --rules. Those that run render have the clone
// read-only, so nothing render runs can change what the next fetch runs
// with; render runs in renderImage, pinned by digest.
func TestChartRulesHistory(t *testing.T) {
	base := []string{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090",
		"--set", "serve.rulesHistory.repository=https://git.example.com/ops/platform.git", "--set", "serve.rulesHistory.paths=charts/monitoring/",
		"--set", "serve.rulesHistory.tokenSecret.name=git"}
	rendered := append(append([]string{}, base...), "--set", "serve.rulesHistory.render=helm template m charts/monitoring",
		"--set", "serve.rulesHistory.renderPrepare=helm dependency build charts/monitoring", "--set", "serve.rulesHistory.unrendered=skip",
		"--set", "serve.rulesHistory.max=50", "--set", "serve.rulesHistory.every=120", "--set", "serve.rulesHistory.select=grafana_alert=1")
	out, err := helmTemplate(t, rendered...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cs := historyPodOf(t, out)
	lapilli := strings.Join(cs["lapilli"].Args, " ")
	if !strings.Contains(lapilli, "--rules-history=/var/lib/lapilli-history/versions --rules-select=grafana_alert=1") || strings.Contains(lapilli, "--rules=") {
		t.Errorf("lapilli's args: %s", lapilli)
	}
	values := chartValues(t)
	for name, want := range map[string]struct {
		image, repo, history, token string
	}{
		"rules-history-clone":  {values.image, "rw", "", "ro"},
		"rules-history":        {values.renderImage, "ro", "rw", ""},
		"rules-history-fetch":  {values.image, "rw", "", "ro"},
		"rules-history-follow": {values.renderImage, "ro", "rw", ""},
		"lapilli":              {"ghcr.io/lapilli-project/lapilli:dev", "", "ro", ""},
	} {
		c, ok := cs[name]
		if !ok {
			t.Errorf("no %s", name)
			continue
		}
		if c.Image != want.image || c.mounts("rules-history-repo") != want.repo || c.mounts("rules-history") != want.history ||
			c.mounts("rules-history-token") != want.token || (c.env("TOKEN_FILE") != "") != (want.token != "") {
			t.Errorf("%s: %s, the clone %q, the history %q, the token %q %q", name, c.Image, c.mounts("rules-history-repo"),
				c.mounts("rules-history"), c.mounts("rules-history-token"), c.env("TOKEN_FILE"))
		}
	}
	for _, name := range []string{"rules-history", "rules-history-follow"} {
		c := cs[name]
		if c.env("RENDER_PREPARE") != "helm dependency build charts/monitoring" || c.env("UNRENDERED") != "skip" || c.env("MAX") != "50" {
			t.Errorf("%s: %+v", name, c.Env)
		}
	}
	if cs["rules-history-fetch"].env("EVERY") != "120" || cs["rules-history-follow"].env("EVERY") != "120" || cs["rules-history-clone"].env("EVERY") != "0" {
		t.Error("every")
	}

	// Without render, the image with git writes the versions.
	out, err = helmTemplate(t, base...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if cs := historyPodOf(t, out); cs["rules-history"].Image != values.image || cs["rules-history-follow"].Image != values.image {
		t.Errorf("without render: %s %s", cs["rules-history"].Image, cs["rules-history-follow"].Image)
	}
	// The history of rules.configMaps: written on each start, not followed.
	out, err = helmTemplate(t, append(append([]string{}, rendered...), "--set", "rules.configMaps={rules}")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cs = historyPodOf(t, out)
	if _, ok := cs["rules-history-fetch"]; ok || !strings.Contains(strings.Join(cs["lapilli"].Args, " "), "--rules=/etc/lapilli/rules/0") {
		t.Errorf("with rules.configMaps: %v", cs)
	}

	for _, c := range []struct {
		more []string
		says string
	}{
		{[]string{"--set", "serve.rulesHistory.unrendered=maybe"}, "unrendered: fail or skip"},
		{[]string{"--set", "serve.rulesHistory.max=-1"}, "serve.rulesHistory.max"},
		{[]string{"--set", "serve.rulesHistory.every=10"}, "30 at least"},
		{[]string{"--set", "serve.rulesHistory.render=", "--set", "serve.rulesHistory.renderPrepare=true"}, "give render too"},
		{[]string{"--set", "serve.rulesHistory.renderImage=quay.io/argoproj/argocd:v3.5.6"}, "renderImage: NAME@sha256"},
		{[]string{"--set", "rules.loaded=true"}, "give one of"},
		{[]string{"--set", "serve.rulesHistory.repository=", "--set", "rules.configMaps={rules}"}, "go with serve.rulesHistory.repository"},
		{[]string{"--set", "mode=cronjob", "--set", "rules.configMaps={rules}", "--set", "impact=up"}, "serve.rulesHistory is for mode: serve"},
	} {
		if out, err := helmTemplate(t, append(append([]string{}, rendered...), c.more...)...); err == nil || !strings.Contains(out, c.says) {
			t.Errorf("%v: %v, want %q\n%s", c.more, err, c.says, out)
		}
	}
}

// chartValues are the rules history's images in values.yaml.
func chartValues(t *testing.T) (v struct{ image, renderImage string }) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "charts", "lapilli", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Serve struct {
			RulesHistory struct {
				Image       string
				RenderImage string `yaml:"renderImage"`
			} `yaml:"rulesHistory"`
		}
	}
	if err := yaml.Unmarshal(b, &values); err != nil {
		t.Fatal(err)
	}
	return struct{ image, renderImage string }{values.Serve.RulesHistory.Image, values.Serve.RulesHistory.RenderImage}
}

// The rules history's storage is bounded, and by default it never stops on
// a commit: its volumes have size limits, its containers ephemeral-storage
// requests and limits, a commit too large to read or that can't be
// rendered is passed over (unrendered: skip), and following, the versions
// from before the backfill and a day go. --changes has nothing to write to
// when the rules are a followed history alone.
func TestChartRulesHistoryBounded(t *testing.T) {
	base := []string{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090", "--set", "serve.backfill=10d",
		"--set", "serve.rulesHistory.repository=https://git.example.com/ops/platform.git", "--set", "serve.rulesHistory.paths=charts/monitoring/",
		"--set", "serve.rulesHistory.render=helm template m charts/monitoring"}
	out, err := helmTemplate(t, base...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name     string
						EmptyDir *struct {
							SizeLimit string `yaml:"sizeLimit"`
						} `yaml:"emptyDir"`
					}
					InitContainers []historyLimited `yaml:"initContainers"`
					Containers     []historyLimited
				}
			}
		}
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		if strings.Contains(doc, "kind: Deployment") {
			if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
				t.Fatal(err)
			}
		}
	}
	spec := d.Spec.Template.Spec
	sizes := map[string]string{}
	for _, v := range spec.Volumes {
		if v.EmptyDir != nil {
			sizes[v.Name] = v.EmptyDir.SizeLimit
		}
	}
	if sizes["rules-history"] != "1Gi" || sizes["rules-history-repo"] != "2Gi" || sizes["rules-history-work"] != "1Gi" {
		t.Errorf("size limits %v", sizes)
	}
	seen := 0
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		if !strings.HasPrefix(c.Name, "rules-history") {
			continue
		}
		seen++
		if c.Resources.Requests["ephemeral-storage"] == "" || c.Resources.Limits["ephemeral-storage"] == "" {
			t.Errorf("%s: resources %+v", c.Name, c.Resources)
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		if c.Name == "rules-history" || c.Name == "rules-history-follow" {
			if env["UNRENDERED"] != "skip" || env["MAX_TREE_FILES"] != "20000" || env["MAX_TREE_BYTES"] != "268435456" {
				t.Errorf("%s: env %v", c.Name, env)
			}
		}
		if c.Name == "rules-history-follow" && !strings.Contains(c.Args[0], `*d) keep=$(( ${BACKFILL%d} * 86400 + 86400 ))`) {
			t.Errorf("follow: no KEEP from the backfill:\n%s", c.Args[0])
		}
	}
	if seen != 4 {
		t.Errorf("%d rules history containers", seen)
	}
	// Left out of the values, as an older values file would: skip still.
	if out, err := helmTemplate(t, append(base, "--set", "serve.rulesHistory.unrendered=null")...); err != nil || !strings.Contains(out, "- name: UNRENDERED\n              value: \"skip\"") {
		t.Errorf("unrendered left out: %v", err)
	}
	if out, err := helmTemplate(t, append(base, "--set", "serve.changes.enabled=true", "--set", "serve.userHeader=X-Forwarded-Email",
		"--set", "serve.roles.admins={ana@example.com}", "--set", "serve.networkPolicy.othersKeptOut=true")...); err == nil ||
		!strings.Contains(out, "serve.changes writes a change to where the rules are kept") {
		t.Errorf("changes, following: %v\n%s", err, out)
	}
}

type historyLimited struct {
	Name      string
	Args      []string
	Env       []struct{ Name, Value string }
	Resources struct {
		Requests, Limits map[string]string
	}
}

// serve.ledger.postgres keeps the ledger in Postgres: the Secret holding
// its connection string is mounted and named by --ledger-url-file, no
// PersistentVolumeClaim is made, and the egress policy lets serve reach the
// database and nothing more. Still one replica. Beside existingClaim, or
// with egress and nowhere for the database, it is refused.
func TestChartPostgres(t *testing.T) {
	out, err := helmTemplate(t, serveValues...)
	if err != nil || !strings.Contains(out, "- --ledger=/var/lib/lapilli\n") || !strings.Contains(out, "kind: PersistentVolumeClaim") ||
		strings.Contains(out, "ledger-url-file") {
		t.Fatalf("by default: %v\n%s", err, out)
	}
	pg := append(serveValues, "--set", "serve.ledger.postgres.secret.name=ledger-db", "--set", "serve.ledger.postgres.secret.key=conn")
	out, err = helmTemplate(t, pg...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`- "--ledger-url-file=/var/run/lapilli/ledger/conn"`, "mountPath: /var/run/lapilli/ledger\n              readOnly: true",
		"secretName: ledger-db", "replicas: 1", "type: Recreate"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"kind: PersistentVolumeClaim", "--ledger=", "claimName:"} {
		if strings.Contains(out, not) {
			t.Errorf("%q with Postgres:\n%s", not, out)
		}
	}
	egress := append(pg, "--set", "serve.networkPolicy.egress.enabled=true", "--set", "serve.networkPolicy.egress.to[0].to[0].ipBlock.cidr=10.0.0.0/8")
	if out, err := helmTemplate(t, egress...); err == nil || !strings.Contains(out, "serve.ledger.postgres.to") {
		t.Errorf("egress, and nowhere for the database: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, append(egress, "--set", "serve.ledger.postgres.to[0].ipBlock.cidr=10.1.2.3/32", "--set", "serve.ledger.postgres.port=6432")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var np struct {
		Spec struct {
			Egress []struct {
				To    []map[string]any
				Ports []struct{ Port int }
			}
		}
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		if strings.Contains(doc, "name: ops-lapilli-egress") {
			if err := yaml.Unmarshal([]byte(doc), &np); err != nil {
				t.Fatal(err)
			}
		}
	}
	e := np.Spec.Egress
	if len(e) != 3 || len(e[2].To) != 1 || len(e[2].Ports) != 1 || e[2].Ports[0].Port != 6432 {
		t.Errorf("the egress policy: %+v", np)
	}
	if out, err := helmTemplate(t, append(pg, "--set", "serve.ledger.existingClaim=old")...); err == nil || !strings.Contains(out, "two places to keep the ledger") {
		t.Errorf("beside existingClaim: %v\n%s", err, out)
	}
}

// maxSamples is --max-samples, in both modes; none when 0, the default.
func TestChartMaxSamples(t *testing.T) {
	for _, mode := range [][]string{serveValues, {"--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={rules}", "--set", "impact=up == 0"}} {
		out, err := helmTemplate(t, mode...)
		if err != nil || strings.Contains(out, "--max-samples") {
			t.Errorf("by default: %v\n%s", err, out)
		}
		out, err = helmTemplate(t, append(mode, "--set", "maxSamples=40000000")...)
		if err != nil || !strings.Contains(out, `- "--max-samples=40000000"`) {
			t.Errorf("maxSamples: %v\n%s", err, out)
		}
	}
}
