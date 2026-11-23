package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The template files the configuration names, resolved against its
// directory, and read only within it: an absolute glob, as Alertmanager's
// own path, looked for beside it; a link out of it, or a glob that leaves
// it, passed over and said, by a name relative to it.
func TestAlertmanagerFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "am")
	writeFile(t, filepath.Join(root, "secret.tmpl"), `{{ define "secret" }}{{ end }}`)
	writeFile(t, filepath.Join(dir, "templates", "a.tmpl"), `{{ define "a" }}{{ end }}`)
	writeFile(t, filepath.Join(dir, "templates", "b.tmpl"), `{{ define "b" }}{{ end }}`)
	writeFile(t, filepath.Join(dir, "mounted.tmpl"), `{{ define "m" }}{{ end }}`)
	if err := os.Symlink(filepath.Join(root, "secret.tmpl"), filepath.Join(dir, "templates", "link.tmpl")); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	writeFile(t, filepath.Join(dir, "alertmanager.yml"), `templates:
  - templates/*.tmpl
  - /etc/alertmanager/config/*.tmpl
  - ../*.tmpl
  - "[bad"
route: {receiver: r}
receivers: [{name: r}]
`)
	cfg, files, passed, err := AlertmanagerFiles(filepath.Join(dir, "alertmanager.yml"))
	if err != nil || !strings.Contains(string(cfg), "receiver: r") {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"templates/a.tmpl", "templates/b.tmpl", "mounted.tmpl"}) {
		t.Errorf("read %q", names)
	}
	joined := strings.Join(passed, "\n")
	for _, want := range []string{`"templates/link.tmpl" is outside`, `"../secret.tmpl" is outside`, `"[bad" isn't a glob`} {
		if !strings.Contains(joined, want) {
			t.Errorf("passed over: %q, want %s", passed, want)
		}
	}
	if strings.Contains(joined, root) {
		t.Errorf("a path on the disk said: %q", passed)
	}
	// Mimir's format, in a file, as Amazon's definition is written.
	writeFile(t, filepath.Join(dir, "definition.yml"), "template_files:\n  x.tmpl: '{{ define \"x\" }}{{ end }}'\nalertmanager_config: |\n  route: {receiver: r}\n")
	cfg, files, _, err = AlertmanagerFiles(filepath.Join(dir, "definition.yml"))
	if err != nil || len(files) != 1 || files[0].Name != "x.tmpl" || !strings.Contains(string(cfg), "route:") {
		t.Errorf("a definition: %q %+v %v", cfg, files, err)
	}
	// Too large, and a directory: not read.
	writeFile(t, filepath.Join(dir, "big.yml"), strings.Repeat("#", MaxAlertmanagerConfig+1))
	for _, p := range []string{filepath.Join(dir, "big.yml"), dir, filepath.Join(dir, "none.yml")} {
		if _, _, _, err := AlertmanagerFiles(p); err == nil {
			t.Errorf("%s read", filepath.Base(p))
		}
	}
}

// Mimir's configuration API: the tenant and the token sent, 404 as none,
// and an answer that isn't its own, or too large, or a redirect, refused.
func TestAlertmanagerAPI(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	writeFile(t, tok, "s3cret\n")
	answer := "template_files:\n  a.tmpl: '{{ define \"a\" }}{{ end }}'\nalertmanager_config: |\n  route: {receiver: r}\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scope-OrgID") != "acme" || r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "unable to determine the OrgID", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/alerts":
			w.Header().Set("Content-Type", "application/yaml")
			w.Write([]byte(answer))
		case "/none/api/v1/alerts":
			http.Error(w, "alertmanager storage object not found", http.StatusNotFound)
		case "/moved/api/v1/alerts":
			http.Redirect(w, r, "http://example.com/", http.StatusFound)
		case "/other/api/v1/alerts":
			w.Write([]byte("route: {receiver: r}\n"))
		case "/big/api/v1/alerts":
			w.Write([]byte(strings.Repeat("#", MaxAlertmanagerConfig+1)))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := Creds{TokenFile: tok}
	cfg, files, found, err := AlertmanagerAPI(ctx, srv.URL+"/api/v1/alerts", "acme", c, "lapilli/test", nil)
	if err != nil || !found || len(files) != 1 || !strings.Contains(string(cfg), "route:") {
		t.Fatalf("%q %+v %v %v", cfg, files, found, err)
	}
	if _, _, found, err := AlertmanagerAPI(ctx, srv.URL+"/none/api/v1/alerts", "acme", c, "lapilli/test", nil); found || err != nil {
		t.Errorf("none: %v %v", found, err)
	}
	for _, p := range []string{"/moved", "/other", "/big"} {
		if _, _, _, err := AlertmanagerAPI(ctx, srv.URL+p+"/api/v1/alerts", "acme", c, "lapilli/test", nil); err == nil {
			t.Errorf("%s: read", p)
		}
	}
	_, _, _, err = AlertmanagerAPI(ctx, srv.URL+"/api/v1/alerts", "other", c, "lapilli/test", nil)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("refused: %v", err)
	}
	for _, bad := range []string{"ftp://x/api/v1/alerts", "http://user@x/api/v1/alerts", "http://x/api/v1/alerts?q=1", "http:///api"} {
		if CheckAlertmanagerURL(bad) == nil {
			t.Errorf("%s taken", bad)
		}
	}
}

// Mimir's, Cortex's and Amazon's configuration: alertmanager_config, and
// every template file beside it.
func TestSplitDefinition(t *testing.T) {
	cfg, files, ok, err := SplitDefinition([]byte(`template_files:
  b.tmpl: '{{ define "b" }}{{ end }}'
  a.tmpl: |
    {{ define "a" }}{{ .CommonLabels.x }}{{ end }}
alertmanager_config: |
  route:
    receiver: r
  receivers:
    - name: r
`))
	if err != nil || !ok || !strings.Contains(string(cfg), "receiver: r") || len(files) != 2 || files[0].Name != "a.tmpl" {
		t.Fatalf("%q %+v %v %v", cfg, files, ok, err)
	}
	if _, _, ok, err := SplitDefinition([]byte("route: {receiver: r}\n")); ok || err != nil {
		t.Errorf("alertmanager.yml taken for a definition: %v %v", ok, err)
	}
	if _, _, _, err := SplitDefinition([]byte("alertmanager_config: [1]\n")); err == nil {
		t.Error("alertmanager_config that isn't a text")
	}
	globs, err := TemplateGlobs([]byte("templates: ['/etc/alertmanager/*.tmpl', 'more/*.tmpl', 3]\nroute: {receiver: r}\n"))
	if err != nil || !slices.Equal(globs, []string{"/etc/alertmanager/*.tmpl", "more/*.tmpl"}) {
		t.Errorf("%q %v", globs, err)
	}
}
