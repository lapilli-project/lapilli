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
		skips := a["eks.amazonaws.com/skip-containers"] == "rules-history-clone,rules-history" &&
			a["azure.workload.identity/skip-containers"] == "rules-history-clone;rules-history"
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
