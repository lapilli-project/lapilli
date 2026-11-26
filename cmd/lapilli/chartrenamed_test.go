package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestChartRenamedValues: a value set under its old key is read as the new,
// and NOTES.txt says so; the table kept (templates/_renamed.tpl) renames
// nothing yet, so here it is a test's: the chart is copied with its own.
func TestChartRenamedValues(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := t.TempDir()
	if out, err := exec.Command("cp", "-R", filepath.Join("..", "..", "charts", "lapilli")+"/.", chart).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(chart, "templates", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("_renamed.tpl", `{{- define "lapilli.renamedValues" -}}
serve.oldBackfill:
  to: serve.backfill
oldTimeZone:
  to: timeZone
{{- end -}}
`)
	// NOTES.txt is not rendered by helm template: its text, in a ConfigMap.
	notes, err := os.ReadFile(filepath.Join(chart, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	write("_notes.tpl", `{{ define "t.notes" }}`+string(notes)+`{{ end }}`)
	write("notes-test.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes\ndata:\n  n: {{ include \"t.notes\" . | toJson }}\n")
	render := func(set ...string) (string, error) {
		args := []string{"template", "ops", chart}
		args = append(args, append(append([]string{}, serveValues...), set...)...)
		cmd := exec.Command("helm", args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := render("--set", "serve.oldBackfill=9d", "--set", "oldTimeZone=Asia/Seoul")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`"--backfill=9d"`, "serve.oldBackfill is serve.backfill now", "oldTimeZone is timeZone now",
		"until the next major release"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Asia/Seoul") {
		t.Errorf("the old key's value is not the new's:\n%s", out)
	}
	// By the new key: no note, the same value.
	out, err = render("--set", "serve.backfill=9d")
	if err != nil || !strings.Contains(out, `"--backfill=9d"`) || strings.Contains(out, "Renamed values") {
		t.Errorf("the new key: %v\n%s", err, out)
	}
}

// The values.yaml of 0.71.0, the last release before the renames, as a person
// keeps it from helm show values, or as --reuse-values gives it: every old key
// at its old default.
const values071 = "testdata/values-0.71.0.yaml"

// renamedChart copies the chart with a table of its own (the real one when
// table is empty), and a template that gives NOTES.txt's text and the values
// the templates read after the renames, in ConfigMaps.
func renamedChart(t *testing.T, table string) string {
	t.Helper()
	chart := t.TempDir()
	if out, err := exec.Command("cp", "-R", filepath.Join("..", "..", "charts", "lapilli")+"/.", chart).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(chart, "templates", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if table != "" {
		write("_renamed.tpl", `{{- define "lapilli.renamedValues" -}}
`+table+`
{{- end -}}
`)
	}
	notes, err := os.ReadFile(filepath.Join(chart, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	write("_notes.tpl", `{{ define "t.notes" }}`+string(notes)+`{{ end }}`)
	write("notes-test.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes\ndata:\n  n: {{ include \"t.notes\" . | toJson }}\n  service: {{ .Values.serve.service | toJson | toJson }}\n")
	return chart
}

func renderChart(t *testing.T, chart string, set ...string) (string, error) {
	t.Helper()
	args := append([]string{"template", "ops", chart}, set...)
	cmd := exec.Command("helm", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestChartRenamedOldDefaults: values kept from 0.71.0 carry each old key at
// its old default, which is not the person's choice: the new key given beside
// them is what the chart reads, an old key changed from its default still
// moves, and both given and differing fail.
func TestChartRenamedOldDefaults(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := renamedChart(t, "")
	signed := []string{"--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.othersKeptOut=true"}
	old := []string{"-f", values071, "--set", "mode=serve", "--set", "rules.configMaps={r}"}
	prom := []string{"--set", "prometheus.url=http://prometheus:9090"}
	cat := func(parts ...[]string) []string {
		var all []string
		for _, p := range parts {
			all = append(all, p...)
		}
		return all
	}
	for name, c := range map[string]struct {
		set  []string
		want string
	}{
		"Alertmanager's secret": {cat(prom, []string{"--set", "alertmanager.secret.name=am"}), `"--alertmanager-config=/etc/lapilli/alertmanager/alertmanager.yaml"`},
		"the scrape interval":   {cat(prom, []string{"--set", "scrapeInterval=30s"}), `"--scrape-interval=30s"`},
		"the groups' header":    {cat(prom, signed, []string{"--set", "serve.groupsHeader=X-G"}), `"--groups-header=X-G"`},
		"Google's project":      {[]string{"--set", "prometheus.googleProject=shop"}, `"--google-project=shop"`},
		// An old key changed from its default is the person's.
		"the old scrape interval":     {cat(prom, []string{"--set", "serve.checkContent.scrapeInterval=15s"}), `"--scrape-interval=15s"`},
		"the old Google project":      {[]string{"--set", "gcpProject=shop"}, `"--google-project=shop"`},
		"the old Alertmanager secret": {cat(prom, []string{"--set", "serve.checkContent.alertmanager.secret.name=am"}), `"--alertmanager-config=/etc/lapilli/alertmanager/alertmanager.yaml"`},
	} {
		out, err := renderChart(t, chart, cat(old, c.set)...)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
	}

	// The old key moved is the person's; one at its default is not listed.
	out, err := renderChart(t, chart, cat(old, prom, []string{"--set", "serve.checkContent.scrapeInterval=15s", "--set", "alertmanager.secret.name=am"})...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "serve.checkContent.scrapeInterval is scrapeInterval now") {
		t.Errorf("the moved key is not in NOTES:\n%s", out)
	}
	for _, not := range []string{"gcpProject is", "serve.checkContent.alertmanager is", "serve.roles.groupsHeader is", "serve.incidentsAPI", "serve.changes.tokenSecret is"} {
		if strings.Contains(out, not) {
			t.Errorf("an old key at its old default is in NOTES (%q):\n%s", not, out)
		}
	}

	// Both set, and differing: an error naming both.
	out, err = renderChart(t, chart, cat(old, prom, []string{"--set", "serve.checkContent.scrapeInterval=15s", "--set", "scrapeInterval=30s"})...)
	if err == nil || !strings.Contains(out, "serve.checkContent.scrapeInterval and scrapeInterval are both set") || !strings.Contains(out, "keep scrapeInterval") {
		t.Errorf("both set and differing: %v\n%s", err, out)
	}
	// Both set to the same: nothing to choose.
	out, err = renderChart(t, chart, cat(old, prom, []string{"--set", "serve.checkContent.scrapeInterval=30s", "--set", "scrapeInterval=30s"})...)
	if err != nil || !strings.Contains(out, `"--scrape-interval=30s"`) {
		t.Errorf("both set, the same: %v\n%s", err, out)
	}
}

// TestChartRenamedBlock: a block under its old key is laid over the new
// key's default block, key by key: a key it leaves out keeps the new default,
// and an empty value in it is kept as given.
func TestChartRenamedBlock(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := renamedChart(t, "serve.oldService:\n  to: serve.service")
	out, err := renderChart(t, chart, append(append([]string{}, serveValues...),
		"--set", "serve.oldService.type=", "--set", "serve.oldService.port=8080", "--set", "serve.oldService.annotations.a=b")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var got string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "service: "); ok {
			if err := json.Unmarshal([]byte(v), &got); err != nil {
				t.Fatalf("%v: %s", err, v)
			}
		}
	}
	var svc map[string]any
	if err := json.Unmarshal([]byte(got), &svc); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if v, ok := svc["type"]; !ok || v != "" {
		t.Errorf("an empty value given is not kept: type %v", v)
	}
	if svc["port"] != float64(8080) {
		t.Errorf("port %v, want 8080", svc["port"])
	}
	if a, _ := svc["annotations"].(map[string]any); a["a"] != "b" {
		t.Errorf("annotations %v", svc["annotations"])
	}
	if svc["externalTrafficPolicy"] != "" || svc["sourceRangesAreTheProxy"] != false {
		t.Errorf("a key left out has not the new default: %v", svc)
	}
}

// TestChartReuseValuesFromOld: helm upgrade --reuse-values gives the old
// release's values as the chart's own (the new chart's values.yaml is not
// read), so every key added since is missing and so is the marker; the chart
// fills each from files/values.yaml. Here the chart copy's values.yaml is
// 0.71.0's, the templates and files/ the new.
func TestChartReuseValuesFromOld(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := renamedChart(t, "")
	old, err := os.ReadFile(values071)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "values.yaml"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	base := []string{"--set", "mode=serve", "--set", "rules.configMaps={r}", "--set", "prometheus.url=http://prometheus:9090"}
	out, err := renderChart(t, chart, base...)
	if err != nil || !strings.Contains(out, "kind: Deployment") {
		t.Fatalf("%v\n%s", err, out)
	}
	// The key given beside them is read, over the filled default.
	out, err = renderChart(t, chart, append(base, "--set", "alertmanager.secret.name=am", "--set", "scrapeInterval=30s")...)
	if err != nil || !strings.Contains(out, `"--scrape-interval=30s"`) || !strings.Contains(out, `"--alertmanager-config=`) {
		t.Fatalf("%v\n%s", err, out)
	}
}

// TestChartNullRemovesDefault: with the chart's own values (the marker is
// there) nothing is filled, so null removes a default as it always did.
func TestChartNullRemovesDefault(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	out, err := helmTemplate(t, "--set", "mode=serve", "--set", "rules.configMaps={r}", "--set", "prometheus.url=http://prometheus:9090")
	if err != nil || !strings.Contains(out, "limits:") {
		t.Fatalf("the default limits: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, "--set", "mode=serve", "--set", "rules.configMaps={r}", "--set", "prometheus.url=http://prometheus:9090", "--set", "resources.limits=null")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "limits:") {
		t.Errorf("resources.limits=null left a limit:\n%s", out)
	}
}

// files/values.yaml, the defaults the chart fills missing keys from (a
// template can't read values.yaml itself), is values.yaml.
func TestChartValuesCopy(t *testing.T) {
	parallel(t)
	a, err := os.ReadFile(filepath.Join("..", "..", "charts", "lapilli", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "charts", "lapilli", "files", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("charts/lapilli/files/values.yaml differs from charts/lapilli/values.yaml: cp charts/lapilli/values.yaml charts/lapilli/files/values.yaml")
	}
}

// The table kept is a map of dotted paths; here, none.
func TestChartRenamedValuesKept(t *testing.T) {
	parallel(t)
	b, err := os.ReadFile(filepath.Join("..", "..", "charts", "lapilli", "templates", "_renamed.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `define "lapilli.renamedValues"`) {
		t.Error("no table")
	}
}

// TestChartRenamedKeysTaken: each value renamed before this release is taken
// under its old key, and the flag it makes is the new key's.
func TestChartRenamedKeysTaken(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	signed := []string{"--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.roles.admins={ana@example.com}", "--set", "serve.networkPolicy.othersKeptOut=true"}
	for name, c := range map[string]struct {
		set  []string
		want string
	}{
		"the jobs' port":     {[]string{"--set", "serve.incidentsAPI.tokenSecret.name=t", "--set", "serve.incidentsAPI.port=9191"}, `"--jobs-listen=:9191"`},
		"Google's project":   {[]string{"--set", "prometheus.url=", "--set", "gcpProject=shop", "--set", "mode=serve", "--set", "rules.configMaps={r}"}, `"--google-project=shop"`},
		"the recipes' token": {append([]string{"--set", "serve.changes.enabled=true", "--set", "serve.changes.tokenSecret.name=rt", "--set", "serve.incidentsAPI.tokenSecret.name=it"}, signed...), "--changes-recipe-token-file=/var/run/lapilli/changes-token/token"},
		"the groups' header": {append([]string{"--set", "serve.roles.groupsHeader=X-Forwarded-Groups"}, signed...), `"--groups-header=X-Forwarded-Groups"`},
	} {
		args := append(append([]string{}, serveValues...), c.set...)
		out, err := helmTemplate(t, args...)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
	}
}

// TestChartImageDigestOfAnotherRelease: --reuse-values (or saved values) from
// 0.71.0 keeps its image.digest, and a released chart (it carries its own in
// the annotation) would run that image under this version's name: refused,
// unless the tag is given too or the digest is this chart's own or empty.
func TestChartImageDigestOfAnotherRelease(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	own := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	released := renamedChart(t, "")
	path := filepath.Join(released, "Chart.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "  lapilli.dev/image-digest: \"\"\n") {
		t.Fatal("no annotation to set")
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), "  lapilli.dev/image-digest: \"\"\n", "  lapilli.dev/image-digest: \""+own+"\"\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	git := renamedChart(t, "")
	base := []string{"--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={r}", "--set", "impact=up==0"}
	out, err := renderChart(t, released, append(base, "--set", "image.digest="+other)...)
	if err == nil || !strings.Contains(out, "another release's") || !strings.Contains(out, `image.digest=""`) {
		t.Errorf("another release's digest, no tag: %v\n%s", err, out)
	}
	for name, c := range map[string]struct {
		chart string
		set   []string
		want  string
	}{
		"its own":          {released, []string{"image.digest=" + own}, "@" + own},
		"emptied":          {released, []string{`image.digest=`}, ""},
		"with a tag":       {released, []string{"image.digest=" + other, "image.tag=v0.0.1"}, "v0.0.1@" + other},
		"a chart from git": {git, []string{"image.digest=" + other}, "@" + other},
	} {
		var set []string
		for _, s := range c.set {
			set = append(set, "--set", s)
		}
		out, err := renderChart(t, c.chart, append(base, set...)...)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
}

// TestChartServeOnlyOldKeys: serve.checkContent.alertmanager and
// serve.checkContent.scrapeInterval were serve's alone in 0.71.0: moved in
// serve mode, ignored (and said so) in cronjob mode, where they were. The
// same block under both keys is not a difference.
func TestChartServeOnlyOldKeys(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := renamedChart(t, "")
	base := []string{"--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={r}", "--set", "impact=up==0"}
	old := []string{"--set", "serve.checkContent.alertmanager.secret.name=am", "--set", "serve.checkContent.scrapeInterval=30s"}
	out, err := renderChart(t, chart, append(append([]string{}, base...), old...)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, no := range []string{"--alertmanager-config", "--scrape-interval"} {
		if strings.Contains(out, no) {
			t.Errorf("cronjob mode passed %s:\n%s", no, out)
		}
	}
	for _, want := range []string{"serve.checkContent.alertmanager is ignored in cronjob mode", "serve.checkContent.scrapeInterval is ignored in cronjob mode"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	out, err = renderChart(t, chart, append(append([]string{"--set", "mode=serve"}, base...), old...)...)
	if err != nil || !strings.Contains(out, `"--alertmanager-config=/etc/lapilli/alertmanager/alertmanager.yaml"`) || !strings.Contains(out, `"--scrape-interval=30s"`) {
		t.Errorf("serve mode: %v\n%s", err, out)
	}
	// The same block under both keys: not a difference; a different one is.
	serve := append([]string{"--set", "mode=serve"}, base...)
	out, err = renderChart(t, chart, append(append([]string{}, serve...), "--set", "serve.checkContent.alertmanager.secret.name=am", "--set", "alertmanager.secret.name=am")...)
	if err != nil {
		t.Errorf("the same block under both keys: %v\n%s", err, out)
	}
	out, err = renderChart(t, chart, append(append([]string{}, serve...), "--set", "serve.checkContent.alertmanager.secret.name=am", "--set", "alertmanager.secret.name=other")...)
	if err == nil || !strings.Contains(out, "both set, and differ") {
		t.Errorf("a different block under both keys: %v\n%s", err, out)
	}
}
