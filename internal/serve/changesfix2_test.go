package serve

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// The second review of changing rules, before v0.22.0: each test fails on
// what it found, and passes on its fix.

// What a rule says calls only the functions its template language has:
// Prometheus refuses a whole file for one it hasn't, a misspelt humanize as
// much as one that doesn't exist; Grafana's has its own beside them.
func TestSaysOnlyItsFunctions(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h := s.Handler()
	for text, ok := range map[string]bool{
		`{{ nosuchfunc $labels.instance }}`:                                                                 false,
		`{{ humanise $value }}`:                                                                             false,
		`Errors: {{ $value | humanise }}`:                                                                   false,
		`{{ removeLabels $labels "pod" }}`:                                                                  false, // Grafana's
		`{{ humanizePercentage $value }} on {{ $labels.instance | title | toUpper }}`:                       true,
		`{{ reReplaceAll "(.*):.*" "$1" $labels.instance }} {{ $externalURL }}`:                             true,
		`{{ $labels.instance | stripPort }} {{ index $labels "pod" }} {{ printf "%.1f" $value }}`:           true,
		`{{ humanizeDuration 3600 }} {{ toTime 1700000000 }} {{ $value | humanize1024 }} {{ .Labels.job }}`: true,
	} {
		id, code, body := proposeSays(t, h, url.Values{"annotation": {"summary"}, "text": {text}})
		if ok != (code == http.StatusSeeOther) {
			t.Errorf("%q: %d, want taken %v\n%s", text, code, ok, zzMsg(body))
		}
		if !ok && !strings.Contains(body, "not defined") {
			t.Errorf("%q: refused without naming the function: %s", text, zzMsg(body))
		}
		if id != 0 {
			s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
		}
	}
	// Grafana's own, in Grafana's language.
	g := grafanaServer(t, &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}})
	gh := g.Handler()
	if _, code, body := proposeSays(t, gh, url.Values{"annotation": {"summary"}, "text": {`{{ humanise $values.A.Value }}`}}); code != http.StatusBadRequest {
		t.Errorf("Grafana, misspelt: %d %s", code, zzMsg(body))
	}
	if _, code, body := proposeSays(t, gh, url.Values{"annotation": {"summary"}, "text": {`{{ $values.A.Value | humanize }} on {{ removeLabels $labels "pod" }}`}}); code != http.StatusSeeOther {
		t.Errorf("Grafana's own function: %d %s", code, zzMsg(body))
	}
}

// zzMsg is the message of an error page, or the start of a body.
func zzMsg(body string) string {
	if i := strings.Index(body, "</h1>"); i >= 0 {
		rest := body[i+5:]
		if j, k := strings.Index(rest, "<p>"), strings.Index(rest, "</p>"); j >= 0 && k > j {
			return rest[j+3 : k]
		}
	}
	return clip(body, 200)
}
