package content

import (
	"fmt"
	"strings"
	"testing"

	"lapilli.dev/internal/routing"
)

// incident.io is Alertmanager's own integration: it pages, and the tool keeps
// one incident for a group.
func TestIncidentioIsAPagingKind(t *testing.T) {
	cfg, err := ReadAlertmanagerRouting([]byte(`
route: {receiver: r}
receivers:
  - name: r
    incidentio_configs: [{url: "http://x", alert_source_token: "t"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.Receiver("r")
	if len(r.Integrations) != 1 || r.Integrations[0].Kind != "incidentio" || !routing.PagingKind("incidentio", false) || !routing.DedupsByGroup("incidentio") {
		t.Fatalf("%+v", r)
	}
}

// Alertmanager accepts a timing of a millisecond; the model doesn't follow
// it, and says so.
func TestAlertmanagerSubSecondTimingRefused(t *testing.T) {
	_, err := ReadAlertmanagerRouting([]byte("route: {receiver: r, group_interval: 100ms}\nreceivers: [{name: r}]\n"))
	if err == nil || !strings.Contains(err.Error(), "doesn't model timings under") {
		t.Fatalf("%v", err)
	}
}

func TestRegexBudgetOfAConfiguration(t *testing.T) {
	var b strings.Builder
	b.WriteString("route:\n  receiver: r\n  routes:\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "    - receiver: r\n      matchers: ['a=~\"(ab|cd|ef|gh|ij|kl|mn|op|qr|st){90}%d\"']\n", i)
	}
	b.WriteString("receivers: [{name: r}]\n")
	_, err := ReadAlertmanagerRouting([]byte(b.String()))
	if err == nil || !strings.Contains(err.Error(), "instructions") {
		t.Fatalf("%v", err)
	}
	// One far over a program's size, at the first matcher.
	_, err = ReadAlertmanagerRouting([]byte("route: {receiver: r, matchers: ['a=~\"(ab|cd|ef|gh|ij|kl|mn|op|qr|st){1000}\"']}\nreceivers: [{name: r}]\n"))
	if err == nil || !strings.Contains(err.Error(), "instructions") {
		t.Fatalf("%v", err)
	}
}

// Grafana accepts a timing of a nanosecond; the model reads it as a second
// and says so, and the budget of regular expressions holds in its policies.
func TestGrafanaRoutingBounds(t *testing.T) {
	cps := []byte(`[{"name":"r","type":"slack"}]`)
	cfg, err := ReadGrafanaRouting(cps, []byte(`{"receiver":"r","group_wait":"0s","group_interval":"500ms","repeat_interval":"1h"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "group_interval") || !strings.Contains(w[0], "1s") {
		t.Fatalf("warnings %v", w)
	}
	if rs := cfg.Routes(); rs[0].GroupInterval != routing.MinInterval || rs[0].GroupWait != 0 {
		t.Fatalf("%+v", rs[0])
	}
	var b strings.Builder
	b.WriteString(`{"receiver":"r","routes":[`)
	for i := 0; i < 400; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"receiver":"r","object_matchers":[["a","=~","(ab|cd|ef|gh|ij|kl|mn|op|qr|st){90}%d"]]}`, i)
	}
	b.WriteString("]}")
	if _, err := ReadGrafanaRouting(cps, []byte(b.String()), nil); err == nil || !strings.Contains(err.Error(), "instructions") {
		t.Fatalf("%v", err)
	}
}
