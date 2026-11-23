package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// amRules are rules Alertmanager is sent: one with no summary, one routed
// to a receiver whose title doesn't parse.
const amRules = `groups:
  - name: shop
    rules:
      - alert: ShopDown
        expr: up{job="shop"} == 0
        for: 5m
        labels: {severity: page, team: shop}
        annotations: {description: "Down.", runbook_url: "https://runbooks.example/shop"}
      - alert: WebDown
        expr: up{job="web"} == 0
        for: 5m
        labels: {severity: page, team: web}
        annotations: {summary: "Web down", runbook_url: "https://runbooks.example/web"}
`

const amConfig = `templates: [templates/*.tmpl]
route:
  receiver: shop
  routes:
    - matchers: ['team="web"']
      receiver: web
receivers:
  - name: shop
    slack_configs:
      - text: '{{ template "shop.text" . }}'
  - name: web
    slack_configs:
      - title: '{{ .CommonLabels.alertname }'
`

// lapilli verdict --check-content --alertmanager-config: the templates the
// rules are sent with checked beside them, from a file and the template
// files beside it, or from Mimir's API with the ruler's tenant and token.
func TestCheckContentAlertmanager(t *testing.T) {
	rulesFile := write(t, "rules.yaml", amRules)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "alertmanager.yml")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{cfg: amConfig,
		filepath.Join(dir, "templates", "shop.tmpl"): `{{ define "shop.text" }}{{ .CommonAnnotations.summary }}{{ end }}`} {
		if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{`ShopDown  fact  a notification template shows an annotation the rule hasn't  "shop.text": summary`,
		`WebDown   fact  a notification template that doesn't parse                   "web/slack_configs[0].title": line 1: unexpected "}" in operand`}
	check := func(args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"verdict", "--rules", rulesFile, "--check-content"}, args...), &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		for _, w := range want {
			if !strings.Contains(stdout.String(), w) {
				t.Errorf("no %q in:\n%s", w, stdout.String())
			}
		}
	}
	check("--alertmanager-config", cfg)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scope-OrgID") != "acme" || r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/api/v1/alerts" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write([]byte("template_files:\n  shop.tmpl: '{{ define \"shop.text\" }}{{ .CommonAnnotations.summary }}{{ end }}'\n" +
			"alertmanager_config: |\n" + indent(amConfig, "  ")))
	}))
	defer srv.Close()
	tok := write(t, "token", "tok\n")
	check("--alertmanager-config", srv.URL+"/api/v1/alerts", "--ruler-tenant", "acme", "--ruler-token-file", tok)

	// Refused: not read is an error, as it is read for this alone.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--rules", rulesFile, "--check-content", "--alertmanager-config", srv.URL + "/api/v1/alerts"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "HTTP 401") {
		t.Errorf("refused: exit %d: %s", code, stderr.String())
	}
	for _, args := range [][]string{
		{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--alertmanager-config", cfg},
		{"verdict", "--rules", rulesFile, "--check-content", "--alertmanager-config", "ftp://x/api/v1/alerts"},
	} {
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != 2 && code != 1 || !strings.Contains(stderr.String(), "--alertmanager-config") {
			t.Errorf("%q: exit %d: %s", args, code, stderr.String())
		}
	}
}
