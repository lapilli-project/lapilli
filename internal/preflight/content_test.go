package preflight

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// amConfig is an Alertmanager configuration of two receivers, one sent
// with a template a file beside it defines, the files named by
// Alertmanager's own absolute path.
const amConfig = `route:
  receiver: team
  routes:
    - matchers: ['severity="page"']
      receiver: pager
receivers:
  - name: team
    slack_configs:
      - channel: '#ops'
        api_url_file: /etc/alertmanager/slack
        title: '{{ template "team.title" . }}'
  - name: pager
templates:
  - /etc/alertmanager/templates/*.tmpl
`

// --alertmanager-config as a file: read as serve reads it, with the
// template files beside it, and said by how many receivers and templates;
// what serve would pass over is a problem; one that can't be read too.
// Read with --offline as well: it is a file serve reads.
func TestAlertmanagerFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "alertmanager.yaml")
	os.WriteFile(file, []byte(amConfig), 0o600)
	os.WriteFile(filepath.Join(dir, "team.tmpl"), []byte(`{{ define "team.title" }}{{ .CommonAnnotations.summary }}{{ end }}{{ define "team.text" }}x{{ end }}`), 0o600)
	for _, offline := range []bool{false, true} {
		c := config()
		c.Serve.CheckContent, c.Serve.AlertmanagerConfig, c.Offline = true, file, offline
		p := guarded(t, c)
		p.alertmanager(context.Background())
		it := itemOf(p, "content.alertmanager")
		if it == nil || it.State != OK || !strings.Contains(it.What, "2 receivers; 1 template file, defining 2 templates") {
			t.Errorf("offline %v: %+v", offline, it)
		}
		if a := p.al.asked(); a.Requests != 0 {
			t.Errorf("a file asks nothing: %+v", a)
		}
	}

	// A glob that names no file beside it: serve passes it over, and says so.
	os.Remove(filepath.Join(dir, "team.tmpl"))
	c := config()
	c.Serve.CheckContent, c.Serve.AlertmanagerConfig = true, file
	p := guarded(t, c)
	p.alertmanager(context.Background())
	if it := itemOf(p, "content.alertmanager"); it == nil || it.State != Problem || !strings.Contains(it.Why, "names no file beside the configuration") ||
		!strings.Contains(it.Fix, "mount the template files beside") {
		t.Errorf("no template file: %+v", it)
	}
	// A template that doesn't parse: serve takes the rest.
	os.WriteFile(filepath.Join(dir, "team.tmpl"), []byte(`{{ define "team.title" }}{{ .Oops`), 0o600)
	p = guarded(t, c)
	p.alertmanager(context.Background())
	if it := itemOf(p, "content.alertmanager"); it == nil || it.State != Problem || !strings.Contains(it.Why, "doesn't parse") ||
		!strings.Contains(it.Fix, "keeps Alertmanager from loading") {
		t.Errorf("a template that doesn't parse: %+v", it)
	}
	// None there, or one with no route.
	for _, bad := range []string{filepath.Join(dir, "none.yaml"), fileOf(t, "am.yaml", "receivers: []\n")} {
		c.Serve.AlertmanagerConfig = bad
		p = guarded(t, c)
		p.alertmanager(context.Background())
		if it := itemOf(p, "content.alertmanager"); it == nil || it.State != Problem || !strings.Contains(it.What, "couldn't be read") {
			t.Errorf("%s: %+v", bad, it)
		}
	}
	// With neither the content check nor the notifications worked out, none.
	c.Serve.CheckContent = false
	p = guarded(t, c)
	p.alertmanager(context.Background())
	if it := itemOf(p, "content.alertmanager"); it != nil {
		t.Errorf("both off: %+v", it)
	}
	// With the content check off and the notifications worked out, serve
	// reads it all the same: one that can't be read is a problem, and says
	// what it costs.
	c.Serve.Notifications = true
	c.Serve.AlertmanagerConfig = filepath.Join(dir, "nonexistent.yaml")
	p = guarded(t, c)
	p.alertmanager(context.Background())
	if it := itemOf(p, "content.alertmanager"); it == nil || it.State != Problem || !strings.Contains(it.What, "who each firing reached isn't known") ||
		strings.Contains(it.What, "content check") {
		t.Errorf("the notifications alone, unreadable: %+v", it)
	}
	c.Serve.AlertmanagerConfig = file
	os.WriteFile(filepath.Join(dir, "team.tmpl"), []byte(`{{ define "team.title" }}x{{ end }}{{ define "team.text" }}x{{ end }}`), 0o600)
	p = guarded(t, c)
	p.alertmanager(context.Background())
	if it := itemOf(p, "content.alertmanager"); it == nil || it.State != OK {
		t.Errorf("the notifications alone, read: %+v", it)
	}
}

// With the content check off and the notifications worked out, serve asks
// Mimir's configuration API each hour all the same: the estimate counts it.
func TestAlertmanagerAPIEstimateByNotifications(t *testing.T) {
	for _, c := range []struct {
		content, notifications bool
		want                   int
	}{{true, false, 24}, {false, true, 24}, {true, true, 48}, {false, false, 0}} {
		cfg := config()
		cfg.Serve.CheckContent, cfg.Serve.Notifications = c.content, c.notifications
		cfg.Serve.AlertmanagerConfig = "http://mimir.example:8080/api/v1/alerts"
		p := guarded(t, cfg)
		p.beside(nil)
		got := 0
		if b := p.r.Estimates.Beside; b != nil {
			got = b.By["alertmanager_config"]
		}
		if got != c.want {
			t.Errorf("content %v, notifications %v: %d requests a day beside, want %d", c.content, c.notifications, got, c.want)
		}
	}
}

// --alertmanager-config as Mimir's API: asked once, as serve asks it, with
// the ruler's tenant and token, through the allowlist, that URL alone and
// never said; how often serve asks it is in the estimate. A 404 is a
// tenant with none, a 401 a token not let in; with --offline, unknown.
func TestAlertmanagerAPI(t *testing.T) {
	const raw = "http://mimir.example:8080/api/v1/alerts"
	answer := "template_files:\n  team.tmpl: '{{ define \"team.title\" }}{{ .CommonAnnotations.summary }}{{ end }}'\nalertmanager_config: |\n" +
		"  " + strings.ReplaceAll(strings.TrimSpace(amConfig), "\n", "\n  ") + "\n"
	code := http.StatusOK
	var tenant, token string
	h := standIns(t, map[string]http.Handler{"mimir.example:8080": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, token = r.Header.Get("X-Scope-OrgID"), r.Header.Get("Authorization")
		if code != http.StatusOK {
			http.Error(w, "the tenant's configuration isn't there", code)
			return
		}
		w.Write([]byte(answer))
	})})
	c := config()
	c.Serve.CheckContent, c.Serve.AlertmanagerConfig, c.Serve.RulerTenant = true, raw, "shop"
	c.Serve.RulerToken = fileOf(t, "token", "mimir-reads-0123456789\n")
	p := guarded(t, c)
	if !slices.Contains(p.hidden, [2]string{"--alertmanager-config", raw}) {
		t.Errorf("not hidden: %v", p.hidden)
	}
	p.alertmanager(context.Background())
	it := itemOf(p, "content.alertmanager")
	if it == nil || it.State != OK || !strings.Contains(it.What, "[--alertmanager-config] answered") ||
		!strings.Contains(it.What, "2 receivers; 1 template file, defining 1 template") || strings.Contains(it.What, "mimir.example") {
		t.Errorf("%+v", it)
	}
	if tenant != "shop" || token != "Bearer mimir-reads-0123456789" {
		t.Errorf("sent as %q, %q", tenant, token)
	}
	if a := p.al.asked(); a.Requests != 1 || a.By["alertmanager"] != 1 || a.Refused != 0 {
		t.Errorf("%+v", a)
	}
	// The allowlist holds it to that URL: a read beside it is refused.
	p.al.check(req(t, http.MethodGet, raw+"/x", ""))
	p.al.check(req(t, http.MethodPost, raw, ""))
	if a := p.al.asked(); a.Refused != 2 || len(h.all()) != 1 {
		t.Errorf("beside it: %+v, sent %v", a, h.all())
	}
	p.beside(nil)
	if b := p.r.Estimates.Beside; b == nil || b.By["alertmanager_config"] != 24 {
		t.Errorf("the estimate: %+v", b)
	}

	for _, x := range []struct {
		code int
		fix  string
	}{{http.StatusNotFound, "--ruler-tenant the tenant"}, {http.StatusUnauthorized, "--ruler-token-file a token"}, {http.StatusBadGateway, "reachable from where serve runs"}} {
		code = x.code
		p = guarded(t, c)
		p.alertmanager(context.Background())
		if it := itemOf(p, "content.alertmanager"); it == nil || it.State != Problem || !strings.Contains(it.Fix, x.fix) {
			t.Errorf("HTTP %d: %+v", x.code, it)
		}
	}

	c.Offline = true
	p = guarded(t, c)
	p.alertmanager(context.Background())
	if it := itemOf(p, "content.alertmanager"); it == nil || it.State != Unknown || p.al.asked().Requests != 0 {
		t.Errorf("offline: %+v", it)
	}
}

// With the notifications worked out and no Alertmanager configuration, serve
// reads Grafana's templates, contact points, policies and mute timings each
// hour for them: four requests, 96 a day, and none without them or with
// --alertmanager-config (whose routing it reads instead).
func TestGrafanaRoutingEstimate(t *testing.T) {
	for _, c := range []struct {
		notifications bool
		am            string
		want          int
	}{{true, "", 96}, {false, "", 0}, {true, "/etc/am.yaml", 0}} {
		cfg := config()
		cfg.Serve.Grafana, cfg.Serve.Notifications, cfg.Serve.AlertmanagerConfig = "http://grafana.example", c.notifications, c.am
		p := guarded(t, cfg)
		p.beside(nil)
		got := 0
		if b := p.r.Estimates.Beside; b != nil {
			got = b.By["grafana_routing"]
		}
		if got != c.want {
			t.Errorf("notifications %v, am %q: %d requests a day for Grafana's routing, want %d", c.notifications, c.am, got, c.want)
		}
	}
}

// With the notifications worked out, the routing is read as serve reads it:
// a route to a receiver that isn't there, or a matcher that doesn't parse, is
// a problem with its reason, with the content check on or off; with it off,
// the templates: globs aren't looked for (serve's routing ignores them).
func TestAlertmanagerRoutingProblem(t *testing.T) {
	dir := t.TempDir()
	for name, cfgText := range map[string]string{
		"undefined receiver": "route:\n  receiver: team\n  routes:\n    - matchers: ['a=\"b\"']\n      receiver: nowhere\nreceivers:\n  - name: team\n",
		"bad matcher":        "route:\n  receiver: team\n  matchers: [\"a=~(\"]\nreceivers:\n  - name: team\n",
	} {
		file := filepath.Join(dir, "am.yaml")
		os.WriteFile(file, []byte(cfgText), 0o600)
		for _, check := range []bool{false, true} {
			c := config()
			c.Serve.CheckContent, c.Serve.Notifications, c.Serve.AlertmanagerConfig = check, true, file
			p := guarded(t, c)
			p.alertmanager(context.Background())
			it := itemOf(p, "notifications.routing")
			if it == nil || it.State != Problem || it.Why == "" || !strings.Contains(it.What, "who each firing reached isn't known") {
				t.Errorf("%s, content check %v: %+v", name, check, it)
			}
		}
		// Not for the notifications: nothing says it.
		c := config()
		c.Serve.CheckContent, c.Serve.AlertmanagerConfig = true, file
		p := guarded(t, c)
		p.alertmanager(context.Background())
		if it := itemOf(p, "notifications.routing"); it != nil {
			t.Errorf("%s, no notifications: %+v", name, it)
		}
	}
	// A good routing is no problem; with the content check off a templates:
	// glob that names no file isn't one.
	file := filepath.Join(dir, "good.yaml")
	os.WriteFile(file, []byte(amConfig), 0o600)
	c := config()
	c.Serve.Notifications, c.Serve.AlertmanagerConfig = true, file
	p := guarded(t, c)
	p.alertmanager(context.Background())
	if it := itemOf(p, "notifications.routing"); it != nil {
		t.Errorf("a good routing: %+v", it)
	}
	if it := itemOf(p, "content.alertmanager"); it == nil || it.State != OK {
		t.Errorf("content check off, glob without files: %+v", it)
	}
	c.Serve.CheckContent = true
	p = guarded(t, c)
	p.alertmanager(context.Background())
	if it := itemOf(p, "content.alertmanager"); it == nil || it.State != Problem {
		t.Errorf("content check on, glob without files: %+v", it)
	}
}
