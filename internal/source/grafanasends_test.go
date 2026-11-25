package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// Where a Grafana sends its alerts is read from its alerting API's
// answers, as its API reference documents them: the admin configuration,
// the active Alertmanagers, and the Alertmanager data sources.
func TestGrafanaAlertmanagers(t *testing.T) {
	answers := map[string]string{}
	codes := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := codes[r.URL.Path]; c != 0 {
			http.Error(w, "no", c)
			return
		}
		a, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(a))
	}))
	defer srv.Close()
	ask := func() (GrafanaSends, error) {
		return GrafanaAlertmanagers(context.Background(), srv.URL, "", "lapilli/test")
	}
	reset := func() {
		clear(answers)
		clear(codes)
	}
	const ds = `[{"name":"Mimir AM","type":"alertmanager","jsonData":{"handleGrafanaManagedAlerts":true,"implementation":"mimir"}},
	 {"name":"Other","type":"alertmanager","jsonData":{"handleGrafanaManagedAlerts":false}},
	 {"name":"Prom","type":"prometheus","jsonData":{"handleGrafanaManagedAlerts":true}}]`
	const active = `{"status":"success","data":{"activeAlertManagers":[{"url":"http://am.example:9093/api/v2/alerts"}],"droppedAlertManagers":[]}}`

	// No configuration (404): Grafana sends to its own.
	reset()
	if g, err := ask(); err != nil || g.Choice != "internal" || g.External() || !g.Internal() {
		t.Errorf("no configuration: %+v %v", g, err)
	}
	// Internal: nothing more is asked.
	answers["/api/v1/ngalert/admin_config"] = `{"alertmanagersChoice":"internal"}`
	if g, err := ask(); err != nil || g.External() || !g.Internal() {
		t.Errorf("internal: %+v %v", g, err)
	}
	// External and all.
	answers["/api/v1/ngalert/alertmanagers"] = active
	answers["/api/datasources"] = ds
	answers["/api/v1/ngalert/admin_config"] = `{"alertmanagersChoice":"external"}`
	g, err := ask()
	if err != nil || !g.External() || g.Internal() || g.Active != 1 || !g.SourcesRead || !slices.Equal(g.Sources, []string{"Mimir AM"}) {
		t.Errorf("external: %+v %v", g, err)
	}
	answers["/api/v1/ngalert/admin_config"] = `{"alertmanagersChoice":"all"}`
	if g, err := ask(); err != nil || !g.External() || !g.Internal() {
		t.Errorf("all: %+v %v", g, err)
	}
	// A choice not sent is "all", as the API says.
	answers["/api/v1/ngalert/admin_config"] = `{}`
	if g, err := ask(); err != nil || g.Choice != "all" || !g.External() {
		t.Errorf("unset: %+v %v", g, err)
	}
	// "external" with none active sends nowhere: checked as before.
	answers["/api/v1/ngalert/admin_config"] = `{"alertmanagersChoice":"external"}`
	answers["/api/v1/ngalert/alertmanagers"] = `{"status":"success","data":{"activeAlertManagers":[],"droppedAlertManagers":[]}}`
	if g, err := ask(); err != nil || g.External() || !g.Internal() {
		t.Errorf("none active: %+v %v", g, err)
	}
	// Data sources not readable isn't an error.
	answers["/api/v1/ngalert/alertmanagers"] = active
	codes["/api/datasources"] = http.StatusForbidden
	if g, err := ask(); err != nil || !g.External() || g.SourcesRead {
		t.Errorf("data sources refused: %+v %v", g, err)
	}
	// A Grafana that answers otherwise, or refuses: an error.
	for name, f := range map[string]func(){
		"refused":        func() { codes["/api/v1/ngalert/admin_config"] = http.StatusForbidden },
		"not json":       func() { answers["/api/v1/ngalert/admin_config"] = `<html>` },
		"unknown choice": func() { answers["/api/v1/ngalert/admin_config"] = `{"alertmanagersChoice":"both"}` },
		"no list":        func() { delete(answers, "/api/v1/ngalert/alertmanagers") },
	} {
		reset()
		answers["/api/v1/ngalert/admin_config"] = `{"alertmanagersChoice":"all"}`
		answers["/api/v1/ngalert/alertmanagers"] = active
		f()
		if _, err := ask(); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// Grafana sending to several Alertmanagers is said, naming the data
// sources that take its alerts, so that a wrong match of --alertmanager-config
// is seen; one or none says nothing.
func TestGrafanaSendsSeveral(t *testing.T) {
	g := GrafanaSends{Choice: "all", Active: 2, Sources: []string{"AM east", "AM west"}, SourcesRead: true}
	got := g.Several()
	if !strings.Contains(got, "2 Alertmanagers") || !strings.Contains(got, "AM east, AM west") || !strings.Contains(got, "--alertmanager-config") {
		t.Errorf("%q", got)
	}
	for _, g := range []GrafanaSends{{Choice: "all", Active: 1, Sources: []string{"AM"}}, {Choice: "internal"}, {Choice: "external", Active: 0, Sources: []string{"a", "b"}}} {
		if s := g.Several(); s != "" {
			t.Errorf("%+v: %q", g, s)
		}
	}
}
