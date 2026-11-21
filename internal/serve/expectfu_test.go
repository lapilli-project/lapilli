package serve

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

// Why --expect can't be read is said on the status line to an admin; to
// anyone else, only that the file, by its path as they read it, can't be,
// as a --config file's problem is said: what it holds, or where it is,
// isn't theirs to read. So is what it and --config's expectations can't
// hold together, which names what --config declares.
func TestExpectUnreadToReaders(t *testing.T) {
	s := expectServer(t, theExpectations, false)
	os.WriteFile(s.cfg.Expect, []byte("expectations:\n  - rule: Paged\n    budjet: {week: 1}\n"), 0o600)
	s.readExpect()
	if err := s.expectAfter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := s.statusView("en", true); !strings.Contains(st.Err, "budjet") || !strings.Contains(st.Err, s.cfg.Expect) {
		t.Errorf("to an admin: %q", st.Err)
	}
	for l, want := range map[string]string{
		"en": "reading the expectations: the path given to --expect can't be read as an expectations file; an admin sees why",
		"ko": "기대치 읽기: 기대치 파일(--expect 에 준 경로)을 읽을 수 없어요. 이유는 관리자가 볼 수 있어요",
	} {
		if st := s.statusView(l, false); st.Err != want {
			t.Errorf("%s, to a viewer: %q", l, st.Err)
		}
	}
	// Read again, and held together with --config's, with the same budget
	// given twice.
	os.WriteFile(s.cfg.Expect, []byte(theExpectations), 0o600)
	expectReadTwice(t, s)
	cfg, err := ParseConfig([]byte("expectations:\n  - rule: Noise\n    budget: {refires: 5}\n"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	s.keptConfig(cfg.Expectations)
	if err := s.expectAfter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := s.statusView("en", true); !strings.Contains(st.Err, "a budget of re-fires given twice") {
		t.Errorf("to an admin: %q", st.Err)
	}
	if st := s.statusView("en", false); st.Err != "reading the expectations: the expectations of --expect and --config can't be held together; an admin sees why" {
		t.Errorf("to a viewer: %q", st.Err)
	}
}
