package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// --prometheus-tenant is the history's own tenant: sent as X-Scope-OrgID
// to --prometheus alone, in place of --ruler-tenant, which --ruler is
// still sent. Without it, --prometheus is sent --ruler-tenant, as before.
func TestPrometheusTenant(t *testing.T) {
	alone(t)
	newHistory = realHistory
	defer func() { newHistory = realHistory }()
	now = func() time.Time { return t0 }
	var mu sync.Mutex
	sent := map[string][]string{} // what each was sent as its tenant
	note := func(who string, r *http.Request) {
		mu.Lock()
		sent[who] = append(sent[who], r.Header.Get("X-Scope-OrgID"))
		mu.Unlock()
	}
	history := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		note("history", r)
		io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer history.Close()
	ruler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		note("ruler", r)
		io.WriteString(w, "shop:\n    - name: shop\n      rules:\n        - alert: CheckoutErrors\n          expr: errors > 5\n")
	}))
	defer ruler.Close()
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: Checkout down, start: 2026-11-01T21:00:00Z, end: 2026-11-01T22:00:00Z}\n")
	verdictWith := func(more ...string) map[string][]string {
		t.Helper()
		mu.Lock()
		clear(sent)
		mu.Unlock()
		var stdout, stderr bytes.Buffer
		args := append([]string{"verdict", "--ruler", ruler.URL + "/prometheus/config/v1/rules", "--ruler-tenant", "team-a",
			"--prometheus", history.URL, "--incidents", incidents, "--since", "1d", "--tz", "UTC"}, more...)
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
		}
		mu.Lock()
		defer mu.Unlock()
		out := map[string][]string{}
		for k, v := range sent {
			out[k] = slices.Compact(slices.Clone(v))
		}
		return out
	}
	if got := verdictWith("--prometheus-tenant", "history-b"); !slices.Equal(got["ruler"], []string{"team-a"}) || !slices.Equal(got["history"], []string{"history-b"}) {
		t.Errorf("with --prometheus-tenant: %v", got)
	}
	if got := verdictWith(); !slices.Equal(got["ruler"], []string{"team-a"}) || !slices.Equal(got["history"], []string{"team-a"}) {
		t.Errorf("without it: %v", got)
	}
	if got := verdictWith("--prometheus-tenant=history-b"); !slices.Equal(got["history"], []string{"history-b"}) {
		t.Errorf("--prometheus-tenant=ID: %v", got)
	}
}

// lapilli serve, lapilli preflight (which reads serve's flags) and lapilli
// impact suggest take it as verdict does; what a pull request's comment and
// a preflight print says it as [--prometheus-tenant].
func TestPrometheusTenantCommands(t *testing.T) {
	parallel(t)
	rules := write(t, "rules.yaml", "groups: []\n")
	st, err := readServe([]string{"--prometheus", "http://mimir:8080/prometheus", "--prometheus-tenant", "history-b",
		"--ruler", "http://mimir:8080/prometheus/config/v1/rules", "--ruler-tenant", "team-a"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if p := st.f.prometheus(); p.Tenant != "history-b" || st.cfg.RulerTenant != "team-a" {
		t.Errorf("serve: the history's %q, the ruler's %q", p.Tenant, st.cfg.RulerTenant)
	}
	if got := st.f.scrubber()("refused for history-b at mimir:8080"); got != "refused for [--prometheus-tenant] at [--prometheus]" {
		t.Errorf("redacted: %q", got)
	}
	f, err := parseSuggestFlags([]string{"--prometheus", "http://mimir:8080/prometheus", "--prometheus-tenant", "history-b"})
	if err != nil || f.prometheus().Tenant != "history-b" {
		t.Errorf("suggest: %v, %+v", err, f)
	}
	if _, err := parseSuggestFlags([]string{"--prometheus", "http://mimir:8080/prometheus", "--ruler-tenant", "team-a"}); err == nil {
		t.Error("suggest takes --ruler-tenant")
	}
	incidents := write(t, "incidents.yaml", "incidents: []\n")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a slash":      {[]string{"--prometheus", "http://p", "--prometheus-tenant", "a/b"}, "--prometheus-tenant \"a/b\": want a tenant's ID"},
		"empty":        {[]string{"--prometheus", "http://p", "--prometheus-tenant="}, "want a tenant's ID"},
		"dots":         {[]string{"--prometheus", "http://p", "--prometheus-tenant", ".."}, "want a tenant's ID"},
		"too long":     {[]string{"--prometheus", "http://p", "--prometheus-tenant", strings.Repeat("a", 151)}, "want a tenant's ID"},
		"a line break": {[]string{"--prometheus", "http://p", "--prometheus-tenant", "a\nb"}, "want a tenant's ID"},
		"Google's":     {[]string{"--gcp-project", "shop", "--prometheus-tenant", "a"}, "goes with --prometheus URL"},
	} {
		var out, errs strings.Builder
		if code := run(append([]string{"verdict", "--incidents", incidents, "--rules", rules}, c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("verdict, %s: %d, want 2 and %q\n%s", name, code, c.want, errs.String())
		}
	}
	if _, err := readServe([]string{"--rules", rules, "--gcp-project", "shop", "--prometheus-tenant", "a"}, true); err == nil || !strings.Contains(err.Error(), "goes with --prometheus URL") {
		t.Errorf("serve, Google's: %v", err)
	}
	var out, errs strings.Builder
	if code := run([]string{"impact", "suggest", "--gcp-project", "shop", "--prometheus-tenant", "a"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "goes with --prometheus URL") {
		t.Errorf("suggest, Google's: %d %s", code, errs.String())
	}
}

// The chart's prometheus.tenant: --prometheus-tenant, checked as lapilli
// checks it, with prometheus.url only; lapilli serve takes it.
func TestChartPrometheusTenant(t *testing.T) {
	parallel(t)
	m, _ := render(t, append(append([]string{}, serveValues...), "--set", "prometheus.tenant=history-b")...)
	args := m.args(t)
	if !slices.Contains(args, "--prometheus-tenant=history-b") {
		t.Fatalf("no --prometheus-tenant in %v", args)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "r.yaml"), []byte("groups: []\n"), 0o600)
	var flags []string
	for _, a := range args[1:] {
		flags = append(flags, strings.Replace(a, "/etc/lapilli/rules/0", dir, 1))
	}
	st, err := readServe(flags, true)
	if err != nil {
		t.Fatalf("serve refuses the chart's flags: %v\n%v", err, flags)
	}
	if p := st.f.prometheus(); p.Tenant != "history-b" {
		t.Errorf("as serve reads it: %+v", p)
	}
	if out, err := helmTemplate(t, "--set", "prometheus.url=http://prometheus:9090", "--set", "impact=up", "--set", "rules.configMaps={rules}",
		"--set", "prometheus.tenant=history-b"); err != nil ||
		!strings.Contains(out, `- "--prometheus-tenant=history-b"`) || !strings.Contains(out, "kind: CronJob") {
		t.Errorf("the cronjob: %v\n%s", err, out)
	}
	// A backslash, which --set would take as an escape, from a file.
	backslash := filepath.Join(t.TempDir(), "values.yaml")
	os.WriteFile(backslash, []byte("prometheus:\n  tenant: 'a\\b'\n"), 0o600)
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a slash":     {append(append([]string{}, serveValues...), "--set", "prometheus.tenant=a/b"), "prometheus.tenant \"a/b\": want a tenant's ID"},
		"a backslash": {append(append([]string{}, serveValues...), "-f", backslash), "want a tenant's ID"},
		"Google's": {[]string{"--set", "mode=serve", "--set", "gcpProject=shop", "--set", "rules.configMaps={rules}", "--set", "prometheus.tenant=a"},
			"it goes with prometheus.url"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
	}
}
