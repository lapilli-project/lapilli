package main

import (
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
serve.oldBackfill: serve.backfill
oldTimeZone: timeZone
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
