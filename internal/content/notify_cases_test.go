package content

import (
	"fmt"
	"strings"
	"testing"
)

const caseTemplates = `[{"name": "s", "template": "{{ define \"s.msg\" }}on {{ .CommonLabels.cluster }}{{ end }}"}]`
const caseContactPoints = `[{"name": "shop", "settings": {"message": "{{ template \"s.msg\" . }}"}}, {"name": "root", "settings": {"message": "{{ len .Alerts }}"}}]`

// usedWith are the templates a Grafana rule of these labels is sent with,
// under these policies: a template on the shop contact point, none on root.
func usedWith(t *testing.T, policies, labels string) string {
	t.Helper()
	n, _, err := ReadNotifications([]byte(caseTemplates), []byte(caseContactPoints), []byte(policies))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(usedBy(n, grafanaRule(t, labels)), " ")
}

// Alertmanager's older matchers, case by case: match and match_re as = and
// =~, anchored, with object_matchers too; on a label the rule doesn't set,
// or a match_re that doesn't compile, either way (a verification's cases).
func TestLegacyMatcherCases(t *testing.T) {
	for _, c := range []struct{ policies, labels, want string }{
		{`{"receiver": "root", "routes": [{"receiver": "shop", "match": {"team": "shop"}}]}`, "          team: billing\n", ""},
		{`{"receiver": "root", "routes": [{"receiver": "shop", "match": {"team": "shop"}}]}`, "          team: shop\n", "s.msg"},
		{`{"receiver": "root", "routes": [{"receiver": "shop", "match_re": {"service": "pay.*"}}]}`, "          service: xpayments\n", ""},
		{`{"receiver": "root", "routes": [{"receiver": "shop", "match_re": {"service": "^pay.*$"}}]}`, "          service: payments\n", "s.msg"},
		{`{"receiver": "root", "routes": [{"receiver": "shop", "match_re": {"service": "pay"}}]}`, "          service: payments\n", ""},
		// ANDed: match holds, object_matchers doesn't.
		{`{"receiver": "root", "routes": [{"receiver": "shop", "match": {"team": "shop"}, "object_matchers": [["env", "=", "prod"]]}]}`, "          team: shop\n          env: dev\n", ""},
		// match on a label the rule doesn't set: either way, no template certain.
		{`{"receiver": "root", "routes": [{"receiver": "shop", "match": {"team": "shop"}}]}`, "          severity: page\n", ""},
		// A match_re that doesn't compile: either way.
		{`{"receiver": "root", "routes": [{"receiver": "shop", "match_re": {"team": "("}}]}`, "          team: shop\n", ""},
	} {
		if got := usedWith(t, c.policies, c.labels); got != c.want {
			t.Errorf("%s %q: %q, want %q", c.policies, c.labels, got, c.want)
		}
	}
}

// A rule's label written as a template, in any of the ways a template is
// written, is routed either way; one written plainly, by its value (a
// verification's cases).
func TestTemplatedLabelCases(t *testing.T) {
	p := `{"receiver": "root", "routes": [{"receiver": "shop", "object_matchers": [["team", "=", "billing"]]}]}`
	tmpl := `[{"name": "r", "template": "{{ define \"r.msg\" }}{{ .CommonLabels.cluster }}{{ end }}"}]`
	cps := `[{"name": "shop", "settings": {"message": "{{ len .Alerts }}"}}, {"name": "root", "settings": {"message": "{{ template \"r.msg\" . }}"}}]`
	n, _, err := ReadNotifications([]byte(tmpl), []byte(cps), []byte(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"'{{ $labels.team }}'", "'{{$labels.team}}'", "'{{- $labels.team -}}'", "'{{ index $labels \"team\" }}'", "'x-{{ $labels.team }}'"} {
		got := strings.Join(usedBy(n, grafanaRule(t, "          team: "+v+"\n")), " ")
		if got != "" {
			t.Errorf("%s: routed as text: %q", v, got)
		}
	}
	// The literal: shop isn't billing, root's template certain.
	if got := strings.Join(usedBy(n, grafanaRule(t, "          team: shop\n")), " "); got != "r.msg" {
		t.Errorf("literal: %q", got)
	}
}

// Past the policies read, a rule that may reach them isn't routed; one
// whose ways all stop before them, or that can't enter the subtree cut, is
// (a verification's cases).
func TestPolicyCutCases(t *testing.T) {
	var wide strings.Builder
	wide.WriteString(`{"receiver": "root", "routes": [{"receiver": "shop", "object_matchers": [["team", "=", "shop"]]}`)
	for i := range 2100 {
		fmt.Fprintf(&wide, `,{"receiver": "root", "object_matchers": [["n", "=", "%d"]]}`, i)
	}
	wide.WriteString(`]}`)
	// team=shop stops at the first policy, before the cut: routed to shop.
	if got := usedWith(t, wide.String(), "          team: shop\n"); got != "s.msg" {
		t.Errorf("stopped before the cut: %q", got)
	}
	// team=x and n unset: may reach the cut: not routed.
	if got := usedWith(t, wide.String(), "          team: x\n"); got != "" {
		t.Errorf("may reach the cut: %q", got)
	}
	// The cut deep in a subtree the rule can't enter: routed.
	var deep strings.Builder
	deep.WriteString(`{"receiver": "root", "routes": [`)
	for i := range 34 {
		fmt.Fprintf(&deep, `{"receiver": "shop", "object_matchers": [["severity", "=", "page"]], "routes": [`)
		_ = i
	}
	deep.WriteString(strings.Repeat(`]}`, 34) + `,{"receiver": "shop", "object_matchers": [["severity", "=", "info"]]}]}`)
	if got := usedWith(t, deep.String(), "          severity: info\n"); got != "s.msg" {
		t.Errorf("cut in a subtree it can't enter: %q", got)
	}
	if got := usedWith(t, deep.String(), "          severity: page\n"); got != "" {
		t.Errorf("past 32 levels: %q", got)
	}
}

// A child with continue that matches for certain: the parent's contact point
// isn't used (Alertmanager uses a policy's own only when no child matched);
// one that may match goes to it, or to the parent.
func TestContinueMatchedForCertain(t *testing.T) {
	tmpl := `[{"name": "s", "template": "{{ define \"s.msg\" }}{{ .CommonLabels.cluster }}{{ end }}{{ define \"r.msg\" }}{{ .CommonLabels.pod }}{{ end }}"}]`
	cps := `[{"name": "shop", "settings": {"message": "{{ template \"s.msg\" . }}"}}, {"name": "root", "settings": {"message": "{{ template \"r.msg\" . }}"}}]`
	for p, want := range map[string]string{
		`{"receiver": "root", "routes": [{"receiver": "shop", "object_matchers": [["team", "=", "shop"]], "continue": true}]}`:                                                                  "s.msg",
		`{"receiver": "root", "routes": [{"receiver": "shop", "object_matchers": [["unset", "=", "x"]], "continue": true}]}`:                                                                    "", // shop, or root: no template both
		`{"receiver": "root", "routes": [{"receiver": "shop", "object_matchers": [["team", "=", "shop"]], "continue": true}, {"receiver": "root", "object_matchers": [["team", "=~", ".+"]]}]}`: "r.msg s.msg",
	} {
		n, _, err := ReadNotifications([]byte(tmpl), []byte(cps), []byte(p))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(usedBy(n, grafanaRule(t, "          team: shop\n")), " "); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
}
