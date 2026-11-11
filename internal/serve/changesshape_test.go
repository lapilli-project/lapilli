package serve

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lapilli.dev/internal/rules"
)

// What a says change writes is worked out where the rule is kept each time
// the alert fires: a text that parses and calls no query, but repeats,
// sets a variable doubled again and again, or applies a function that
// grows what it is given, can stall Prometheus or Grafana, taking the
// alerts down. Such a text is refused, whoever proposes it.
func TestSaysTemplateCantGrow(t *testing.T) {
	bomb := strings.Repeat("{{range $labels}}", 22) + "x" + strings.Repeat("{{end}}", 22)
	for text, want := range map[string]string{
		bomb:                                     "repeats",
		`{{range 1000000000000}}x{{end}}`:        "repeats",
		`{{define "x"}}a{{end}}{{template "x"}}`: "repeats",
		`{{block "x" .}}a{{end}}`:                "repeats",
		`{{$a := "xx"}}{{$a = printf "%s%s" $a $a}}{{$a}}`:                    "sets a variable",
		`{{with $x := $labels.job}}{{$x}}{{end}}`:                             "sets a variable",
		`{{ $labels.job | js | js | js }}`:                                    "applies js",
		`{{ reReplaceAll "a" "aaaa" (reReplaceAll "a" "aaaa" $labels.job) }}`: "applies reReplaceAll",
		`{{ $labels.job | toUpper | reReplaceAll "A" "AAAA" }}`:               "applies reReplaceAll",
		`{{ $labels.job | html | graphLink }}`:                                "applies graphLink",
		`{{ call $labels.f }}`:                                                "calls call",
		`{{ printf "%0999d" 1 }}`:                                             "printf",
		`{{ printf "%.999f" $value }}`:                                        "printf",
		`{{ printf "%*d" 9999 1 }}`:                                           "printf",
		`{{ printf "%[1]*d" 9999 1 }}`:                                        "printf",
		`{{ printf $labels.format 1 }}`:                                       "printf",
		`{{ "%0999d" | printf }}`:                                             "printf",
	} {
		err := templateOK(text, rules.TemplatePrometheus)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.60s: %v, want %q", text, err, want)
		}
	}
	for _, text := range []string{
		`{{ $labels.instance }} at {{ printf "%.2f" $value }} ({{ $value | humanizePercentage }})`,
		`{{ if gt $value 0.9 }}high{{ else if gt $value 0.5 }}raised{{ else }}low{{ end }} on {{ with $labels.job }}{{ . }}{{ end }}`,
		`100%% of {{ printf "%5.1f%%" $value }} {{ index $labels "pod" | toUpper | title }}`,
		`{{ humanizeDuration $value }} since {{ $externalURL }}, {{ len $labels }} labels {{ print $labels }}`,
		"No template at all, 100% plain.",
		`{{ reReplaceAll "(.*):.*" "$1" $labels.instance }} {{ $labels.instance | reReplaceAll ":.*" "" | toUpper }}`,
		`{{ graphLink "up == 0" }} {{ $labels.job | html }}`,
	} {
		if err := templateOK(text, rules.TemplatePrometheus); err != nil {
			t.Errorf("%s: %v", text, err)
		}
	}
	if err := templateOK(`{{ $values.A.Value }} on {{ index $labels "pod" }}`, rules.TemplateGrafana); err != nil {
		t.Errorf("Grafana's: %v", err)
	}

	// Through the form, as the reviewer's 571-byte text came.
	s, _ := changesServer(t, nil)
	h := s.Handler()
	if _, code, body := proposeSays(t, h, url.Values{"annotation": {"description"}, "text": {bomb}}); code != http.StatusBadRequest || !strings.Contains(body, "repeats") {
		t.Errorf("the bomb proposed: %d %s", code, body)
	}
}
