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

// matchStrict is --match-strict, in both modes; none by default.
func TestChartMatchStrict(t *testing.T) {
	for _, mode := range [][]string{serveValues, {"--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={rules}", "--set", "impact=up == 0"}} {
		out, err := helmTemplate(t, mode...)
		if err != nil || strings.Contains(out, "--match-strict") {
			t.Errorf("by default: %v\n%s", err, out)
		}
		out, err = helmTemplate(t, append(mode, "--set", "matchStrict=true")...)
		if err != nil || strings.Count(out, "- --match-strict\n") != 1 {
			t.Errorf("matchStrict: %v\n%s", err, out)
		}
		out, err = helmTemplate(t, append(mode, "--set-string", "matchStrict=false")...)
		if err != nil || strings.Contains(out, "--match-strict") {
			t.Errorf(`matchStrict: "false": %v\n%s`, err, out)
		}
	}
}

// linkedOnly is --linked-only, in both modes; none by default.
func TestChartLinkedOnly(t *testing.T) {
	for _, mode := range [][]string{serveValues, {"--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={rules}", "--set", "impact=up == 0"}} {
		out, err := helmTemplate(t, mode...)
		if err != nil || strings.Contains(out, "--linked-only") {
			t.Errorf("by default: %v\n%s", err, out)
		}
		out, err = helmTemplate(t, append(mode, "--set", "linkedOnly=true")...)
		if err != nil || strings.Count(out, "- --linked-only\n") != 1 {
			t.Errorf("linkedOnly: %v\n%s", err, out)
		}
		// Quoted, as a values file may give it: "false" is no flag, which a
		// template's if read as true.
		out, err = helmTemplate(t, append(mode, "--set-string", "linkedOnly=false")...)
		if err != nil || strings.Contains(out, "--linked-only") {
			t.Errorf(`linkedOnly: "false": %v\n%s`, err, out)
		}
		out, err = helmTemplate(t, append(mode, "--set-string", "linkedOnly=True")...)
		if err != nil || strings.Count(out, "- --linked-only\n") != 1 {
			t.Errorf(`linkedOnly: "True": %v\n%s`, err, out)
		}
		if out, err = helmTemplate(t, append(mode, "--set-string", "linkedOnly=no")...); err == nil || !strings.Contains(out, `linkedOnly: true or false, not "no"`) {
			t.Errorf(`linkedOnly: "no": %v\n%s`, err, out)
		}
	}
	// So are the two that let anyone in: "false" is neither.
	out, err := helmTemplate(t, append(serveValues, "--set-string", "serve.everyoneAdmin=false", "--set", "serve.service.type=LoadBalancer",
		"--set-string", "serve.exposeWithoutSignIn=false")...)
	if err == nil || strings.Contains(out, "--everyone-admin") || !strings.Contains(out, "say it is meant with serve.exposeWithoutSignIn") {
		t.Errorf(`everyoneAdmin and exposeWithoutSignIn: "false": %v\n%s`, err, out)
	}
	out, err = helmTemplate(t, append(serveValues, "--set-string", "serve.everyoneAdmin=false")...)
	if err != nil || strings.Contains(out, "--everyone-admin") {
		t.Errorf(`everyoneAdmin: "false": %v\n%s`, err, out)
	}
}

// TestChartServeRuleConfigMaps: lapilli serve reads one directory of rules,
// so with several ConfigMaps it has them all in one, a projected volume of
// each one's keys side by side, and one --rules; the CronJob, which takes a
// --rules a ConfigMap, keeps a volume each.
func TestChartServeRuleConfigMaps(t *testing.T) {
	out, err := helmTemplate(t, "--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={main,shared,noc}")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Count(out, "--rules=") != 1 || !strings.Contains(out, `- "--rules=/etc/lapilli/rules/0"`) || strings.Contains(out, "/etc/lapilli/rules/1") {
		t.Errorf("serve with three ConfigMaps: not one directory of rules\n%s", out)
	}
	if !strings.Contains(out, "projected:\n            sources:\n              - configMap:\n                  name: main\n              - configMap:\n                  name: shared\n              - configMap:\n                  name: noc\n") {
		t.Errorf("serve with three ConfigMaps: not one projected volume of the three\n%s", out)
	}
	out, err = helmTemplate(t, "--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={main,shared}", "--set", "impact=up")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, `- "--rules=/etc/lapilli/rules/0"`) || !strings.Contains(out, `- "--rules=/etc/lapilli/rules/1"`) || strings.Contains(out, "projected:") {
		t.Errorf("the CronJob with two ConfigMaps: not a volume and a --rules each\n%s", out)
	}
}

// A default the API server writes inside an atomic field (a
// resourceFieldRef's divisor, a fieldRef's apiVersion in a projected
// volume's sources) is not in the applied object, so a GitOps tool diffing
// what it applied (Argo CD's server-side apply and diff) sees the pod spec
// as changed for as long as it runs. The chart says each.
func TestChartSaysAtomicDefaults(t *testing.T) {
	for _, args := range [][]string{
		{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={main}"},
		{"--set", "mode=serve", "--set", "gcpProject=my-project", "--set", "rules.configMaps={main}"},
		{"--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={main}", "--set", "impact=up"},
		{"--set", "gcpProject=my-project", "--set", "rules.configMaps={main}", "--set", "impact=up"},
	} {
		out, err := helmTemplate(t, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		refs := 0
		dec := yaml.NewDecoder(strings.NewReader(out))
		for {
			var doc any
			if err := dec.Decode(&doc); err != nil {
				break
			}
			var walk func(v any)
			walk = func(v any) {
				switch v := v.(type) {
				case map[string]any:
					if r, ok := v["resourceFieldRef"].(map[string]any); ok {
						refs++
						if r["divisor"] != "1" {
							t.Errorf("%v: a resourceFieldRef without its divisor: %v", args, r)
						}
					}
					if r, ok := v["fieldRef"].(map[string]any); ok {
						refs++
						if r["apiVersion"] != "v1" {
							t.Errorf("%v: a fieldRef without its apiVersion: %v", args, r)
						}
					}
					for _, x := range v {
						walk(x)
					}
				case []any:
					for _, x := range v {
						walk(x)
					}
				}
			}
			walk(doc)
		}
		if refs == 0 {
			t.Errorf("%v: no resourceFieldRef or fieldRef rendered", args)
		}
	}
}

// serve.marked names the ConfigMap --marked's file is mounted from, and
// serve.mark the expressions, each with the labels its windows keep: the
// flags as the command takes them, and none by default.
func TestChartMarked(t *testing.T) {
	out, err := helmTemplate(t, serveValues...)
	if err != nil || strings.Contains(out, "--mark") {
		t.Fatalf("by default: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, append(serveValues, "--set", "serve.marked.configMap.name=sales", "--set", "serve.mark[0].kind=sale",
		"--set", "serve.mark[0].expr=shop_sale_active == 1", "--set", "serve.mark[0].match={region,country}", "--set", "serve.mark[1].kind=launch",
		"--set", "serve.mark[1].expr=flag_on > 0")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`- "--marked=/etc/lapilli/marked/marked.yaml"`, `- "--mark=sale=shop_sale_active == 1"`, `- "--mark-match=sale=region,country"`,
		`- "--mark=launch=flag_on > 0"`, "mountPath: /etc/lapilli/marked", "name: sales"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "--mark-match=launch") {
		t.Error("a --mark-match for an expression that keeps no labels")
	}
}

// impact as a string is --impact, as it was; as a mapping, the named
// signals and the rule entries, written to a ConfigMap as --impact-file
// reads them (docs/design/signals.md), which serve and verdict take.
func TestChartImpactSignals(t *testing.T) {
	out, err := helmTemplate(t, append(serveValues, "--set", "impact=up == 0")...)
	if err != nil || !strings.Contains(out, `- "--impact=up == 0"`) || strings.Contains(out, "--impact-file") {
		t.Fatalf("a string: %v\n%s", err, out)
	}
	values := filepath.Join(t.TempDir(), "values.yaml")
	os.WriteFile(values, []byte(`impact:
  signals:
    - {name: errors, expr: "sum by (service) (rate(http_requests_total{code=~\"5..\"}[5m])) > 1", for: 5m}
    - {name: probe, expr: "probe_success == 0", for: 2m, match: [service]}
  rules:
    - {rules: "^BackendMissing$", signals: [errors, probe], match: [backend]}
`), 0o644)
	out, err = helmTemplate(t, append(serveValues, "-f", values)...)
	if err != nil || strings.Contains(out, "warning") {
		t.Fatalf("a mapping: %v\n%s", err, out)
	}
	for _, want := range []string{"- --impact-file=/etc/lapilli/impact/impact.yaml", "mountPath: /etc/lapilli/impact", "name: ops-lapilli-impact", "impact.yaml: |"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, `"--impact=`) {
		t.Error("--impact beside --impact-file")
	}
	// The file is read as serve starts: a change to the mapping changes the
	// pod's checksum, which starts it again with the file.
	checksum := func(out string) string {
		_, after, _ := strings.Cut(out, "checksum/impact: ")
		v, _, _ := strings.Cut(after, "\n")
		return v
	}
	sum := checksum(out)
	changed := filepath.Join(t.TempDir(), "values.yaml")
	b, _ := os.ReadFile(values)
	os.WriteFile(changed, []byte(strings.Replace(string(b), "for: 2m", "for: 3m", 1)), 0o644)
	again, err := helmTemplate(t, append(serveValues, "-f", changed)...)
	if err != nil || sum == "" || checksum(again) == "" || checksum(again) == sum {
		t.Errorf("the pod's impact checksum: %q, then %q (%v)", sum, checksum(again), err)
	}
	// What the ConfigMap holds is a file --impact-file reads.
	_, data, _ := strings.Cut(out, "impact.yaml: |\n")
	var file []string
	for _, l := range strings.Split(data, "\n") {
		if !strings.HasPrefix(l, "    ") {
			break
		}
		file = append(file, strings.TrimPrefix(l, "    "))
	}
	p := filepath.Join(t.TempDir(), "impact.yaml")
	os.WriteFile(p, []byte(strings.Join(file, "\n")), 0o644)
	signals, entries, err := readImpactFile(p)
	if err != nil || len(signals) != 2 || len(entries) != 1 || entries[0].Match[0] != "backend" {
		t.Errorf("the file: %+v %+v %v\n%s", signals, entries, err, strings.Join(file, "\n"))
	}
}

// serve.notify.webhookSecret.signingKey, a key of the webhook's Secret,
// signs the webhook's posts with what it holds: passed as
// --notify-webhook-secret-file from the Secret mounted already. None
// without it, and refused without the Secret, or as the URL's own key.
func TestChartNotifySigningKey(t *testing.T) {
	hook := append(slices.Clone(serveValues), "--set", "serve.notify.webhookSecret.name=lapilli-hook")
	out, err := helmTemplate(t, hook...)
	if err != nil || !strings.Contains(out, `- "--notify-webhook-file=/var/run/lapilli/notify/url"`) || strings.Contains(out, "--notify-webhook-secret-file") {
		t.Fatalf("without a signing key: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, append(slices.Clone(hook), "--set", "serve.notify.webhookSecret.signingKey=signing-key")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`- "--notify-webhook-file=/var/run/lapilli/notify/url"`, `- "--notify-webhook-secret-file=/var/run/lapilli/notify/signing-key"`,
		"mountPath: /var/run/lapilli/notify", "secretName: lapilli-hook"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s in\n%s", want, out)
		}
	}
	for _, c := range []struct {
		args []string
		says string
	}{
		{append(slices.Clone(serveValues), "--set", "serve.notify.webhookSecret.signingKey=signing-key"), "give the Secret's name"},
		{append(slices.Clone(hook), "--set", "serve.notify.webhookSecret.signingKey=url"), "a key of its own"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.says) {
			t.Errorf("%v: %v\n%s", c.args, err, out)
		}
	}
}

// TestChartVerdictExit: the CronJob passes each of the exit options only
// when it differs from the CLI's default.
func TestChartVerdictExit(t *testing.T) {
	cron := []string{"--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={rules}", "--set", "impact=up"}
	for _, c := range []struct {
		name string
		set  []string
		want []string
		not  []string
	}{
		{"default", nil, nil, []string{"--fail-unreplayed", "--fail-nothing-replayed", "--check-no-data"}},
		{"all three", []string{"--set", "failUnreplayed=true", "--set", "failNothingReplayed=false", "--set", "checkNoData=false"},
			[]string{`"--fail-unreplayed"`, `"--fail-nothing-replayed=false"`, `"--check-no-data=false"`}, nil},
		{"the defaults said", []string{"--set", "failUnreplayed=false", "--set", "failNothingReplayed=true", "--set", "checkNoData=true"},
			nil, []string{"--fail-unreplayed", "--fail-nothing-replayed", "--check-no-data"}},
	} {
		out, err := helmTemplate(t, append(append([]string{}, cron...), c.set...)...)
		if err != nil {
			t.Errorf("%s: %v\n%s", c.name, err, out)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: no %s in\n%s", c.name, w, out)
			}
		}
		for _, n := range c.not {
			if strings.Contains(out, n) {
				t.Errorf("%s: %s in\n%s", c.name, n, out)
			}
		}
	}
	// Serve has no exit status to set: its Deployment takes none of them.
	out, err := helmTemplate(t, append(append([]string{}, serveValues...), "--set", "failUnreplayed=true", "--set", "checkNoData=false")...)
	if err != nil || strings.Contains(out, "--fail-unreplayed") || strings.Contains(out, "--check-no-data") {
		t.Errorf("serve: %v\n%s", err, out)
	}
}
