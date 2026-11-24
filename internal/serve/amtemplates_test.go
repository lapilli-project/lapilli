package serve

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/content"
)

// With --alertmanager-config, the templates Alertmanager sends a rule
// with are checked beside it, read once an hour: from a file and the
// template files beside it, or from Mimir's API with the ruler's tenant.
// Not read, the check goes on as it was, and the log says why once.
func TestContentAlertmanager(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "alertmanager.yml")
	os.WriteFile(cfg, []byte("templates: ['*.tmpl']\nroute: {receiver: r}\nreceivers:\n  - name: r\n    slack_configs:\n      - text: '{{ template \"t\" . }}'\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "t.tmpl"), []byte(`{{ define "t" }}{{ .CommonAnnotations.summary }} in {{ .CommonLabels.cluster }}{{ end }}`), 0o644)
	s, _ := server(t, func(c *Config) { c.CheckContent, c.AlertmanagerConfig = true, cfg })
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	found, err := s.contentFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	has := func(fs []contentFinding, kind, detail string) bool {
		return slices.ContainsFunc(fs, func(f contentFinding) bool { return f.Kind == kind && f.Detail == detail })
	}
	if !has(found["Errors"], content.NotifyNoText, `"t": summary`) {
		t.Errorf("Errors: %+v", found["Errors"])
	}
	_, body, _ := get(t, s.Handler(), "/rules/Errors")
	if !strings.Contains(body, "a notification template shows an annotation the rule hasn&#39;t") {
		t.Errorf("the rule's page has no finding:\n%s", body)
	}

	// The file changed, read again the hour after: what it says now.
	os.WriteFile(filepath.Join(dir, "t.tmpl"), []byte(`{{ define "t" }}{{ .CommonLabels.cluster }{{ end }}`), 0o644)
	s.saidRules.Lock()
	s.saidRules.amAt = s.saidRules.amAt.Add(-time.Hour)
	s.saidRules.Unlock()
	s.askAlertmanager(ctx)
	if found, _ = s.contentFindings(ctx); !slices.ContainsFunc(found["Errors"], func(f contentFinding) bool { return f.Kind == content.NotifyTemplateError }) {
		t.Errorf("a template that doesn't parse: %+v", found["Errors"])
	}
	if n := strings.Count(logged.String(), "won't load the configuration"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, logged.String())
	}
	// Asked again, the same: not logged again.
	s.saidRules.Lock()
	s.saidRules.amAt = s.saidRules.amAt.Add(-time.Hour)
	s.saidRules.Unlock()
	s.askAlertmanager(ctx)
	if n := strings.Count(logged.String(), "won't load the configuration"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, logged.String())
	}

	// Mimir's API, with the ruler's tenant and token.
	tok := filepath.Join(dir, "token")
	os.WriteFile(tok, []byte("tok\n"), 0o600)
	asked := 0
	m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.Header.Get("X-Scope-OrgID") != "acme" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unable to determine the OrgID", http.StatusUnauthorized)
			return
		}
		w.Write([]byte("template_files:\n  t.tmpl: '{{ define \"t\" }}{{ .CommonAnnotations.description }}{{ end }}'\n" +
			"alertmanager_config: |\n  route: {receiver: r}\n  receivers:\n    - name: r\n      slack_configs:\n        - text: '{{ template \"t\" . }}'\n"))
	}))
	defer m.Close()
	s2, _ := server(t, func(c *Config) {
		c.CheckContent, c.AlertmanagerConfig, c.RulerTenant, c.RulerToken = true, m.URL+"/api/v1/alerts", "acme", tok
	})
	if err := s2.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if found, _ = s2.contentFindings(ctx); !has(found["Errors"], content.NotifyNoText, `"t": description`) || asked != 1 {
		t.Errorf("from Mimir, asked %d: %+v", asked, found["Errors"])
	}

	// Refused: the check goes on without them, said once.
	s3, _ := server(t, func(c *Config) { c.CheckContent, c.AlertmanagerConfig = true, m.URL+"/api/v1/alerts" })
	logged.Reset()
	s3.log = slog.New(slog.NewTextHandler(&logged, nil))
	for range 2 {
		s3.saidRules.Lock()
		s3.saidRules.amAt = time.Time{}
		s3.saidRules.Unlock()
		s3.askAlertmanager(ctx)
	}
	if found, err = s3.contentFindings(ctx); err != nil || slices.ContainsFunc(found["Errors"], func(f contentFinding) bool { return strings.HasPrefix(f.Kind, "notify_") }) {
		t.Errorf("refused: %+v %v", found["Errors"], err)
	}
	if n := strings.Count(logged.String(), "can't be read"); n != 1 || !strings.Contains(logged.String(), "HTTP 401") {
		t.Errorf("logged %d times:\n%s", n, logged.String())
	}
}
