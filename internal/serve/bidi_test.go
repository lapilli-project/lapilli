package serve

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// turningChars are the characters ledger.textShows refuses in a comment
// from a tracker because they turn the text around them: the bidi marks,
// embeddings, overrides and isolates.
var turningChars = []rune{0x061C, 0x200E, 0x200F, 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069}

// A rule's name comes from whoever can write a rule where lapilli reads
// them (a rule file, a ruler's tenant, a Grafana editor). A name ending in
// U+202E RIGHT-TO-LEFT OVERRIDE would reach the pages raw, inside an inline
// element of a sentence, and turn around the text after it in the same
// paragraph: on the overview, the numbers next to the rule ("fired 21 → 3")
// would read reversed. Rules are read with such characters as U+FFFD
// (rules.Shown), as the ledger takes a tracker's comment (textShows), and
// the pages set names apart in <bdi>.
func TestRuleNamesDontTurnPages(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, nil)
	// The rule file is the one source here; a ruler or Grafana gives the
	// same name through rules.FromAPI or the export.
	name := "Errors‮"
	file := strings.Replace(ruleFile, "alert: Errors", "alert: \""+name+"\"", 1)
	file = strings.Replace(file, "labels: {severity: page}", "labels: {severity: page}\n        annotations: {summary: \"disk ‮full\"}", 1)
	if err := os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.ready.Store(true)
	h := s.Handler()

	leaked := map[string]int{}
	for _, path := range []string{"/", "/rules", "/rules?weeks=12", "/rules/Errors%EF%BF%BD", "/incidents", "/incidents/INC-1", "/firings?day=2026-11-02"} {
		code, body, _ := get(t, h, path)
		if code != 200 {
			t.Errorf("%s: %d", path, code)
			continue
		}
		for _, r := range turningChars {
			if n := strings.Count(body, string(r)); n > 0 {
				leaked[path] += n
			}
		}
	}
	for path, n := range leaked {
		t.Errorf("%s: %d raw bidi override characters from a rule's name or annotation reach the page", path, n)
	}
	// The name shows, set apart.
	for path, want := range map[string]string{"/": "<bdi>Quiet Errors\uFFFD</bdi>", "/rules": "<bdi>Errors\uFFFD</bdi>", "/rules/Errors%EF%BF%BD": "<h1><bdi>Errors\uFFFD</bdi></h1>",
		"/rules/Errors%EF%BF%BD?": `<span class="k">Group</span> <bdi>shop</bdi>`,
		"/firings?day=2026-11-02": "<bdi>Errors\uFFFD</bdi>"} {
		if _, body, _ := get(t, h, path); !strings.Contains(body, want) {
			i := strings.Index(body, "Errors\uFFFD")
			t.Errorf("%s: no %q in %q", path, want, body[max(0, i-80):min(len(body), max(i, 0)+80)])
		}
	}
}

// A sentence with names in it sets each apart in <bdi>, escaped, in every
// language; and an expression, which a page can't change, is shown with
// what turns text around as U+FFFD.
func TestNamesInSentencesSetApart(t *testing.T) {
	if got := string(trBDI("en", "Group %s.", "a<b>\u202e")); got != "Group <bdi>a&lt;b&gt;\u202e</bdi>." {
		t.Errorf("%q", got)
	}
	if got := string(trBDI("en", "In the ruler: namespace %s, group %s.", "ns", "g")); got != "In the ruler: namespace <bdi>ns</bdi>, group <bdi>g</bdi>." {
		t.Errorf("%q", got)
	}
	for _, l := range names {
		if got := string(trBDI(l.Code, "Group %s.", "g")); !strings.Contains(got, "<bdi>g</bdi>") {
			t.Errorf("%s: %q", l.Code, got)
		}
		// A count beside a name is a number, as its verb has it, in a
		// translation that puts them in another order too.
		if got := string(trBDI(l.Code, "%d found for “%s”.", 3, "g")); !strings.Contains(got, "3") || !strings.Contains(got, "<bdi>g</bdi>") || strings.Contains(got, "%!") {
			t.Errorf("%s: %q", l.Code, got)
		}
	}
	if got := funcs["shown"].(func(string) string)("up{x=\"\u202e\"}"); got != "up{x=\"\uFFFD\"}" {
		t.Errorf("an expression shown: %q", got)
	}
}

// Every link to a rule sets its name apart in <bdi>, and an expression is
// shown as what shows: a template that doesn't would let a name from
// another source than rules.Shown takes turn the text after it around.
func TestTemplatesSetNamesApart(t *testing.T) {
	link := regexp.MustCompile(`<a href="/rules/\{\{path [^}]*\}\}"[^>]*>(.{0,6})`)
	files, _ := filepath.Glob("templates/*.html")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range link.FindAllSubmatch(b, -1) {
			if !strings.HasPrefix(string(m[1]), "<bdi>") {
				t.Errorf("%s: %s", f, m[0])
			}
		}
		if strings.Contains(string(b), "{{.Expr}}") {
			t.Errorf("%s: an expression shown as it is", f)
		}
	}
}
