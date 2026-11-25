package preflight

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With --grafana and --alertmanager-config, where Grafana sends its alerts
// is asked as serve asks it (three reads, through the allowlist) and said:
// to an external Alertmanager its rules are checked against the
// configuration; one that can't be asked, or has none active, is a problem.
func TestGrafanaAlertmanagers(t *testing.T) {
	choice, active, code := "external", 1, http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code != http.StatusOK {
			http.Error(w, "no", code)
			return
		}
		switch r.URL.Path {
		case "/api/v1/ngalert/admin_config":
			w.Write([]byte(`{"alertmanagersChoice":"` + choice + `"}`))
		case "/api/v1/ngalert/alertmanagers":
			list := strings.TrimSuffix(strings.Repeat(`{"url":"http://am.example:9093/api/v2/alerts"},`, active), ",")
			w.Write([]byte(`{"status":"success","data":{"activeAlertManagers":[` + list + `],"droppedAlertManagers":[]}}`))
		case "/api/datasources":
			w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := config()
	c.Serve.CheckContent, c.Serve.Grafana = true, srv.URL
	c.Serve.AlertmanagerConfig = fileOf(t, "am.yaml", amConfig)
	item := func() *Item {
		p := guarded(t, c)
		p.grafanaSends(context.Background())
		return itemOf(p, "grafana.alertmanagers")
	}
	if it := item(); it == nil || it.State != OK || !strings.Contains(it.What, "1 external Alertmanager") || strings.Contains(it.What, "own templates") {
		t.Errorf("external: %+v", it)
	}
	active = 2
	if it := item(); it == nil || it.State != OK || !strings.Contains(it.Why, "2 Alertmanagers") {
		t.Errorf("two Alertmanagers: %+v", it)
	}
	active = 1
	choice = "all"
	if it := item(); it == nil || it.State != OK || !strings.Contains(it.What, "and against its own templates") {
		t.Errorf("all: %+v", it)
	}
	choice = "internal"
	if it := item(); it == nil || it.State != OK || !strings.Contains(it.What, "its own Alertmanager") {
		t.Errorf("internal: %+v", it)
	}
	choice, active = "external", 0
	if it := item(); it == nil || it.State != Problem || !strings.Contains(it.Fix, "Receive Grafana Alerts") {
		t.Errorf("none active: %+v", it)
	}
	code = http.StatusForbidden
	if it := item(); it == nil || it.State != Problem || !strings.Contains(it.Fix, "alerting settings") {
		t.Errorf("refused: %+v", it)
	}
	c.Offline = true
	if it := item(); it == nil || it.State != Unknown {
		t.Errorf("offline: %+v", it)
	}
	// Without --alertmanager-config, nothing is asked.
	c.Offline, c.Serve.AlertmanagerConfig = false, ""
	if it := item(); it != nil {
		t.Errorf("no configuration: %+v", it)
	}
	c.Serve.AlertmanagerConfig = fileOf(t, "am.yaml", amConfig)
	p := guarded(t, c)
	p.beside(nil)
	if b := p.r.Estimates.Beside; b == nil || b.By["grafana_alertmanagers"] != 72 {
		t.Errorf("the estimate: %+v", b)
	}
}
