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
	// Without the content check, none.
	c.Serve.CheckContent = false
	p = guarded(t, c)
	p.alertmanager(context.Background())
	if it := itemOf(p, "content.alertmanager"); it != nil {
		t.Errorf("the check off: %+v", it)
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
