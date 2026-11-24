package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grafanaStandIn answers as Grafana's APIs do: one rule with no summary,
// no notification templates of its own, and the alerting API's choice of
// Alertmanagers, which a test sets.
func grafanaStandIn(t *testing.T, choice *string, refuse bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/provisioning/alert-rules/export":
			io.WriteString(w, `apiVersion: 1
groups:
  - orgId: 1
    name: shop
    folder: prod
    interval: 1m
    rules:
      - uid: c1
        title: CheckoutErrors
        condition: A
        for: 5m
        labels: {severity: page, team: shop}
        annotations: {runbook_url: "https://runbooks.example/c"}
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: errors > 5}
`)
		case "/api/v1/provisioning/templates", "/api/v1/provisioning/contact-points":
			io.WriteString(w, `[]`)
		case "/api/v1/provisioning/policies":
			io.WriteString(w, `{"receiver":"default"}`)
		case "/api/v1/ngalert/admin_config":
			if refuse {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			io.WriteString(w, `{"alertmanagersChoice":"`+*choice+`"}`)
		case "/api/v1/ngalert/alertmanagers":
			io.WriteString(w, `{"status":"success","data":{"activeAlertManagers":[{"url":"http://am.example:9093/api/v2/alerts"}],"droppedAlertManagers":[]}}`)
		case "/api/datasources":
			io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// lapilli verdict --check-content --grafana --alertmanager-config: a
// Grafana that sends to an external Alertmanager has its rules checked
// against that one's templates; one that sends to its own, or can't be
// asked, as before.
func TestCheckContentGrafanaToAlertmanager(t *testing.T) {
	parallel(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "alertmanager.yml")
	if err := os.WriteFile(cfg, []byte(`route: {receiver: r}
receivers:
  - name: r
    slack_configs:
      - text: '{{ .CommonAnnotations.summary }}'
`), 0o600); err != nil {
		t.Fatal(err)
	}
	const finding = `a notification template shows an annotation the rule hasn't`
	for _, c := range []struct {
		choice  string
		refuse  bool
		finds   bool
		stderrs string
	}{
		{"external", false, true, ""},
		{"all", false, true, ""},
		{"internal", false, false, ""},
		{"", true, false, "where Grafana sends its alerts wasn't read"},
	} {
		choice := c.choice
		srv := grafanaStandIn(t, &choice, c.refuse)
		var stdout, stderr bytes.Buffer
		if code := run([]string{"verdict", "--check-content", "--grafana", srv.URL, "--alertmanager-config", cfg}, &stdout, &stderr); code != 0 {
			t.Fatalf("%q: exit %d: %s", c.choice, code, stderr.String())
		}
		if got := strings.Contains(stdout.String(), finding); got != c.finds {
			t.Errorf("%q (refused %v): found %v, want %v:\n%s", c.choice, c.refuse, got, c.finds, stdout.String())
		}
		if c.stderrs != "" && !strings.Contains(stderr.String(), c.stderrs) {
			t.Errorf("%q: stderr %q", c.choice, stderr.String())
		}
	}
}
