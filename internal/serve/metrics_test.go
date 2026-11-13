package serve

import "testing"

// A label's value in /metrics, as Prometheus's text format reads it back:
// a rule's name from an API, holding what acts on a terminal or turns text
// around, is said as the pages say it.
func TestMetricsLabelValueShows(t *testing.T) {
	for in, want := range map[string]string{
		"Errors":                "Errors",
		`a "b" \c` + "\nd":      `a \"b\" \\c\\u{a}d`,
		"Up\x1b]52;c;eA==\x07":  `Up\\u{1b}]52;c;eA==\\u{7}`,
		"Errors\u202e1 \u2066x": `Errors\\u{202e}1 \\u{2066}x`,
		"👩\u200d💻":              "👩\u200d💻",
	} {
		if got := labelValue(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}
