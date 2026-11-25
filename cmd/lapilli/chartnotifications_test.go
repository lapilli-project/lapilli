package main

import (
	"slices"
	"strings"
	"testing"
)

// The chart's notifications values (docs/design/routing.md, "In lapilli
// serve"): external labels, the receivers taken as paging and the silences
// are flags with the alert manager's configuration, or Grafana's policies,
// in both modes; lapilli serve takes the flags and works the notifications
// out for each run's hours.
func TestChartNotifications(t *testing.T) {
	parallel(t)
	set := func(base []string, kv ...string) []string {
		out := slices.Clone(base)
		for _, x := range kv {
			out = append(out, "--set", x)
		}
		return out
	}
	am := set(serveValues, "serve.checkContent.alertmanager.secret.name=alertmanager-main",
		"notifications.externalLabels.cluster=prod", "notifications.externalLabels.env=live",
		"notifications.pagingReceivers=^oncall", "notifications.silences.configMap.name=silences")
	m, _ := render(t, am...)
	args := m.args(t)
	for _, want := range []string{"--external-labels=cluster=prod", "--external-labels=env=live", "--paging-receivers=^oncall", "--silences=/etc/lapilli/silences/silences.json"} {
		if !slices.Contains(args, want) {
			t.Errorf("no %s in %v", want, args)
		}
	}
	if slices.Contains(args, "--notifications=false") {
		t.Errorf("on, and passed off: %v", args)
	}
	if mounts, vols := podFiles(m); mounts["silences"] != "/etc/lapilli/silences" || vols["silences"] != "configMap silences" {
		t.Errorf("the silences: %v %v", mounts, vols)
	}
	// serve takes the chart's flags.
	flags := slices.Clone(args[1:])
	st, err := readServe(flags, true)
	if err != nil {
		t.Fatalf("serve refuses the chart's flags: %v\n%v", err, flags)
	}
	c := st.cfg
	if !c.Notifications || c.ExternalLabels["cluster"] != "prod" || c.ExternalLabels["env"] != "live" || c.PagingReceivers == nil || c.Silences != "/etc/lapilli/silences/silences.json" {
		t.Errorf("as serve reads them: %+v", c)
	}
	// A Secret, under a key of its own.
	m, _ = render(t, set(am[:len(am)-2], "notifications.silences.secret.name=sil", "notifications.silences.secret.key=s.json")...)
	if args := m.args(t); !slices.Contains(args, "--silences=/etc/lapilli/silences/s.json") {
		t.Errorf("a Secret: %v", args)
	}
	if _, vols := podFiles(m); vols["silences"] != "secret sil" {
		t.Errorf("a Secret: %v", vols)
	}
	// Off.
	m, _ = render(t, set(serveValues, "serve.checkContent.alertmanager.secret.name=alertmanager-main", "notifications.enabled=false")...)
	args = m.args(t)
	if !slices.Contains(args, "--notifications=false") {
		t.Errorf("off, and not passed: %v", args)
	}
	if _, err := readServe(args[1:], true); err != nil {
		t.Errorf("serve refuses --notifications=false: %v", err)
	}
	// With Grafana's policies, and in cronjob mode.
	grafServe := []string{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090"}
	graf := set(grafServe, "rules.grafana.url=http://grafana:3000", "notifications.pagingReceivers=^oncall")
	if m, _ := render(t, graf...); !slices.Contains(m.args(t), "--paging-receivers=^oncall") {
		t.Errorf("with Grafana: %v", m.args(t))
	}
	out, err := helmTemplate(t, "--set", "prometheus.url=http://prometheus:9090", "--set", "impact=up", "--set", "rules.grafana.url=http://grafana:3000",
		"--set", "notifications.externalLabels.cluster=prod", "--set", "notifications.silences.secret.name=sil")
	if err != nil || !strings.Contains(out, "--external-labels=cluster=prod") || !strings.Contains(out, "--silences=/etc/lapilli/silences/silences.json") {
		t.Errorf("cronjob mode: %v\n%s", err, out)
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no configuration": {set(serveValues, "notifications.externalLabels.cluster=prod"), "go with the alert manager's configuration"},
		"off with them":    {set(am, "notifications.enabled=false"), "leaves out who a firing reached"},
		"two silences":     {set(am, "notifications.silences.secret.name=sil"), "give one of secret and configMap"},
		"a long pattern":   {set(grafServe, "rules.grafana.url=http://grafana:3000", "notifications.pagingReceivers="+strings.Repeat("a", 501)), "at most 500 characters"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
	}
}

// lapilli serve takes the flags of the notifications, with the alert
// manager's configuration or Grafana's policies to work them out from, and
// works them out unless --notifications=false.
func TestServeNotificationsFlags(t *testing.T) {
	parallel(t)
	base := []string{"--rules", t.TempDir(), "--prometheus", "http://p:9090", "--incidents", "i.yaml"}
	am := "--alertmanager-config=/etc/alertmanager/alertmanager.yml"
	for _, c := range []struct {
		args []string
		want string // "" for accepted
		on   bool
	}{
		{[]string{am}, "", true},
		{[]string{am, "--notifications=false"}, "", false},
		{[]string{am, "--external-labels", "cluster=prod", "--silences", "/s.json", "--paging-receivers", "^oncall"}, "", true},
		{[]string{}, "", false},
		{[]string{"--notifications"}, "give --alertmanager-config, or --grafana", false},
		{[]string{"--external-labels", "cluster=prod"}, "goes with --alertmanager-config, or --grafana", false},
		{[]string{"--silences", "/s.json"}, "goes with --alertmanager-config, or --grafana", false},
		{[]string{"--paging-receivers", "^oncall"}, "goes with --alertmanager-config, or --grafana", false},
		{[]string{am, "--notifications=false", "--paging-receivers", "^oncall"}, "turns off", false},
		{[]string{am, "--external-labels", "cluster"}, "want NAME=VALUE", false},
	} {
		st, err := readServe(append(slices.Clone(base), c.args...), true)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%v: %v", c.args, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		case c.want == "" && st.cfg.Notifications != c.on:
			t.Errorf("%v: notifications %v, want %v", c.args, st.cfg.Notifications, c.on)
		}
	}
}
