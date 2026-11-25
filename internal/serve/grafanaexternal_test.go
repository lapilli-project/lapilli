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

// With --grafana and --alertmanager-config, a Grafana that sends its
// alerts to an external Alertmanager has its rules checked against that
// one's templates, asked once an hour; to its own alone, as before; one
// that can't be asked, as before, and the log says why once.
func TestContentGrafanaToAlertmanager(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	choice, refuse := "external", false
	base, _ := fakeGrafana(t, nil)
	asked := 0
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ngalert/admin_config":
			asked++
			if refuse {
				http.Error(w, `{"message":"Access denied"}`, http.StatusForbidden)
				return
			}
			w.Write([]byte(`{"alertmanagersChoice":"` + choice + `"}`))
		case "/api/v1/ngalert/alertmanagers":
			w.Write([]byte(`{"status":"success","data":{"activeAlertManagers":[{"url":"http://am.example:9093/api/v2/alerts"}],"droppedAlertManagers":[]}}`))
		case "/api/datasources":
			w.Write([]byte(`[]`))
		default:
			// Everything else is the Grafana that gives rules and templates.
			req, _ := http.NewRequestWithContext(r.Context(), r.Method, base.URL+r.URL.Path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			w.WriteHeader(resp.StatusCode)
			var b bytes.Buffer
			b.ReadFrom(resp.Body)
			w.Write(b.Bytes())
		}
	}))
	defer g.Close()
	cfg := filepath.Join(t.TempDir(), "alertmanager.yml")
	os.WriteFile(cfg, []byte("route: {receiver: r}\nreceivers:\n  - name: r\n    slack_configs:\n      - text: '{{ .CommonAnnotations.summary }}'\n"), 0o644)
	s, _ := server(t, func(c *Config) { c.RulesPath, c.Grafana, c.CheckContent, c.AlertmanagerConfig = "", g.URL, true, cfg })
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	find := func(rule, detail string) bool {
		t.Helper()
		cf, err := s.contentFindings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(cf[rule], func(f contentFinding) bool { return f.Kind == content.NotifyNoText && f.Detail == detail })
	}
	const own, ext = `"shop.message": summary`, `"r/slack_configs[0].text": summary`
	// External alone: Alertmanager's templates, and not Grafana's own.
	if !find("PayLatency", ext) || find("PayLatency", own) || find("ShopErrors", ext) {
		t.Errorf("external: %+v", s.saidRules.found.findings["PayLatency"])
	}
	next := func() {
		s.now = func() time.Time { return s.saidRules.sendsAt.Add(time.Hour) }
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
	}
	// Both, an hour later.
	choice = "all"
	next()
	if !find("PayLatency", ext) || !find("PayLatency", own) {
		t.Errorf("all: %+v", s.saidRules.found.findings["PayLatency"])
	}
	// Its own alone: as before.
	choice = "internal"
	next()
	if find("PayLatency", ext) || !find("PayLatency", own) {
		t.Errorf("internal: %+v", s.saidRules.found.findings["PayLatency"])
	}
	// Refused: as before, said once.
	refuse = true
	next()
	next()
	if find("PayLatency", ext) || !find("PayLatency", own) {
		t.Errorf("refused: %+v", s.saidRules.found.findings["PayLatency"])
	}
	if n := strings.Count(logged.String(), "where Grafana sends its alerts can't be read"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, logged.String())
	}
	if asked != 5 {
		t.Errorf("asked %d times, want once an hour", asked)
	}
}
