package serve

import (
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lapilli.dev/internal/scenario/scenariotest"
)

// The reasons Lapilli writes itself beside a check or a draft's item,
// kept in English in the ledger, each has a translation in every language
// it is shown in, matched by its words, a number or a name in it put in;
// any other text is shown as it is.
func TestReasonTextTranslated(t *testing.T) {
	samples := []string{"promtool took longer than 30s", "promtool took more memory than it may (512 MiB)",
		"promtool exited 3, saying what it doesn't say of a test", "more than 1048576 bytes",
		"line 4: a key promtool's test files don't have: want rule_files, tests", "line 2: tests is a list of tests, one at least",
		"it couldn't be replayed: no data"}
	for k := range expectSaid {
		samples = append(samples, k)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, n := range names {
		for _, s := range samples {
			if got := reasonText(n.Code, s); n.Code != "en" && got == s {
				t.Errorf("%s: %q as it is", n.Code, s)
			}
		}
		for _, v := range []string{"passes", "fails"} {
			expectValueText(n.Code, v)
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
	if got := reasonText("ko", "promtool took longer than 30s"); got != "promtool 이 30s 보다 오래 걸렸어요" {
		t.Errorf("a time put in: %q", got)
	}
	for _, s := range []string{"yaml: line 3: did not find expected key", "promtool took longer than 30s, and then some"} {
		if got := reasonText("ko", s); got != s {
			t.Errorf("%q, not Lapilli's: %q", s, got)
		}
	}
}

// A rule's Expectations tab says, in the reader's language, why a
// scenario is not known in Lapilli's words (its test file not there);
// what promtool said of one failing is shown as promtool said it.
func TestExpectationReasonsInLanguage(t *testing.T) {
	expect := "expectations:\n  - rule: Errors\n    scenarios: [tests/none.yaml, tests/errors.yaml]\n"
	s, _, _, _ := scenarioServer(t, expect, map[string]string{"tests/errors.yaml": scenariotest.Test("fail")}, true)
	checkAgain(t, s)
	page := func(l string) string {
		r := httptest.NewRequest(http.MethodGet, "/rules/Errors?tab=expectations", nil)
		r.Header.Set("Sec-Fetch-Site", "none")
		r.Header.Set("Accept-Language", l)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return html.UnescapeString(w.Body.String())
	}
	c := checksOf(t, s, "Errors")
	if c["scenario tests/none.yaml"].Detail != "no such file" || c["scenario tests/errors.yaml"].Detail == "" {
		t.Fatalf("checks: %+v", c)
	}
	said := c["scenario tests/errors.yaml"].Detail
	for l, want := range map[string]string{"en": "no such file", "ko": "그런 파일이 없어요", "ja": "そのファイルはありません", "es": "no existe el archivo"} {
		body := page(l)
		if !strings.Contains(body, "<code>"+want+"</code>") || !strings.Contains(body, said) {
			t.Errorf("%s: no %q, or promtool's words changed", l, want)
		}
		if l != "en" && (strings.Contains(body, "<code>no such file</code>") || strings.Contains(body, ">fails<") || strings.Contains(body, ", fails")) {
			t.Errorf("%s: Lapilli's words in English", l)
		}
	}
}
