package serve

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestTokens(t *testing.T) {
	got := tokens(`  err is:noisy "job:rate5m x" Label:team=pay severity: foo:bar group:shop 10:30 is:loud has:facts label:"a=b c" team:"a b" x:"y z" a"b`)
	want := []qword{{Value: "err"}, {Key: "is", Value: "noisy"}, {Value: "job:rate5m x", Quoted: true}, {Key: "label", Value: "team=pay"},
		{Key: "severity", Bad: "value"}, {Key: "foo", Value: "bar", Bad: "key"}, {Key: "group", Value: "shop"}, {Value: "10:30"},
		{Key: "is", Value: "loud", Bad: "value"}, {Key: "has", Value: "facts"}, {Key: "label", Value: "a=b c"}, {Key: "team", Value: "a b"},
		{Key: "x", Value: "y z", Bad: "key"}, {Value: `a"b`}}
	if len(got) != len(want) {
		t.Fatalf("%d tokens, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d: %+v, want %+v", i, got[i], want[i])
		}
		// As written, each is read again as it was.
		if again := tokens(got[i].String()); len(again) != 1 || again[0].Key != got[i].Key || again[0].Value != got[i].Value {
			t.Errorf("token %d, %s, read again: %+v", i, got[i], again)
		}
	}
}

func TestCanonical(t *testing.T) {
	teams := []string{"payments", "search"}
	for _, c := range []struct {
		q, want string
		changed bool
	}{
		{"q=err", "q=err", false},
		{"q=label:a=b+group:shop", "q=label%3Aa%3Db+group%3Ashop", false},
		{"q=err+is:noisy+severity:critical+team:payments", "perf=noisy&q=err&severity=critical&team=payments", true},
		{"q=is:differs+is:proposed+status:Tune+has:facts&perf=ok", "has=proposal&has=facts&perf=ok&status=tune&trust=differs", true},
		// What it doesn't understand stays, to say so; a team no one has
		// is one.
		{"q=is:loud+team:nope+severity:page&team=search", "q=is%3Aloud+team%3Anope&severity=page&team=search", true},
		{"q=severity:page&severity=page", "severity=page", true},
		// The q= of a URL are one search; a team named in quotes.
		{"q=err&q=is:noisy", "perf=noisy&q=err", true},
		{`q=team:"payments"+label:"a=b c"`, "q=label%3A%22a%3Db+c%22&team=payments", true},
	} {
		in, _ := url.ParseQuery(c.q)
		out, changed := canonical(in, teams)
		if out.Encode() != c.want || changed != c.changed {
			t.Errorf("%s: %s (%v), want %s (%v)", c.q, out.Encode(), changed, c.want, c.changed)
		}
		// Once canonical, it stays.
		if again, changed := canonical(out, teams); changed || again.Encode() != out.Encode() {
			t.Errorf("%s: canonical twice: %s", c.q, again.Encode())
		}
	}
	// A facet takes 20 values, each once; a search too long is left for
	// the page to say so.
	var many []string
	for i := 0; i < 30; i++ {
		many = append(many, fmt.Sprintf("severity:s%02d", i%25))
	}
	if out, _ := canonical(url.Values{"q": {strings.Join(many, " ")}}, teams); len(out["severity"]) != 20 || out.Has("q") {
		t.Errorf("25 severities, some twice: %d kept, q %q", len(out["severity"]), out.Get("q"))
	}
	long := url.Values{"q": {"is:noisy " + strings.Repeat("x", 500)}}
	if out, changed := canonical(long, teams); changed || out.Encode() != long.Encode() {
		t.Errorf("a search too long was moved: %s", out.Encode())
	}
}

// TestSearch: one found is opened, in the same context; more are listed by
// kind; a filter searches the rules; nothing found says so. Every redirect
// stays on this server.
func TestSearch(t *testing.T) {
	s := pagesServer(t)
	h := s.Handler()
	for _, c := range []struct{ q, to string }{
		{"q=Limited", "/rules/Limited"},
		{"q=limited&team=payments&weeks=4", "/rules/Limited?team=payments&weeks=4"},
		{"q=INC-1", "/incidents/INC-1"},
		{"q=paym&weeks=12", "/?team=payments&weeks=12"},
		{"q=is:noisy+err&team=payments", "/rules?perf=noisy&q=err&team=payments"},
		{"q=label:service%3Dsearch", "/rules?q=label%3Aservice%3Dsearch"},
		{"q=group:shop+is:loud", "/rules?q=group%3Ashop+is%3Aloud"},
		{"q=team:payments", "/rules?team=payments"},
	} {
		code, _, hd := get(t, h, "/search?"+c.q)
		if code != http.StatusSeeOther || hd.Get("Location") != c.to {
			t.Errorf("/search?%s: %d to %q, want %q", c.q, code, hd.Get("Location"), c.to)
		}
	}
	_, body, _ := get(t, h, "/search?q=i")
	for _, want := range []string{"<h2>Rules", "<h2>Incidents", `href="/rules/Limited"`, `href="/incidents/INC-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/search?q=i: no %s", want)
		}
	}
	if strings.Index(body, `href="/rules/Limited"`) > strings.Index(body, `href="/incidents/INC-1"`) {
		t.Error("/search?q=i: the incidents before the rules")
	}
	if _, body, _ := get(t, h, "/search?q=nothing-is-called-this"); !strings.Contains(body, `class="empty empty-notfound"`) || !strings.Contains(body, "Nothing found.") {
		t.Error("/search for nothing: no empty state")
	}
	if _, body, _ := get(t, h, "/search"); !strings.Contains(body, "is:noisy") {
		t.Error("/search: no help")
	}

	// Whatever is searched for, a redirect goes to a page here.
	r := rand.New(rand.NewPCG(1, 2))
	parts := []string{"//evil.example", `\\evil.example`, "https://evil.example/", "team:", "team://x", "is:", "label:", "@", "%2F%2F", " ", "Limited",
		"INC-1", "paym", "/..", "\r\n", "group:../", `"`, "is:noisy", "severity:/x"}
	for i := 0; i < 400; i++ {
		var q strings.Builder
		for j := r.IntN(4); j >= 0; j-- {
			q.WriteString(parts[r.IntN(len(parts))])
		}
		code, _, hd := get(t, h, "/search?q="+url.QueryEscape(q.String())+"&team="+url.QueryEscape(parts[r.IntN(len(parts))]))
		if code == http.StatusSeeOther {
			loc := hd.Get("Location")
			u, err := url.Parse(loc)
			if err != nil || u.Host != "" || u.Scheme != "" || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, `/\`) {
				t.Errorf("%q: off to %q", q.String(), loc)
			}
		} else if code != http.StatusOK {
			t.Errorf("%q: %d", q.String(), code)
		}
	}
}

// TestQueryTurnsNothingAround: what a search box was given comes back on
// the rules page set apart, with what turns text around read as U+FFFD,
// as names are: a link someone sends can't turn the page's text around.
func TestQueryTurnsNothingAround(t *testing.T) {
	s := pagesServer(t)
	h := s.Handler()
	q := "\u202eevil label:a=\u2067b foo:\u202ec"
	_, body, _ := get(t, h, "/rules?q="+url.QueryEscape(q))
	for _, want := range []string{"<bdi>\uFFFDevil</bdi>", "<bdi>a=\uFFFDb</bdi>", "foo:\uFFFDc"} {
		if !strings.Contains(body, want) {
			t.Errorf("rules page: no %q", want)
		}
	}
	// Only the forms keep what they were given, as typed, to send it again.
	outside := regexp.MustCompile(`value="[^"]*"`).ReplaceAllString(body, "")
	if strings.ContainsAny(outside, "\u202e\u2067") {
		t.Error("rules page: a direction override outside a form's value")
	}
}

// TestSearchBounded: a search is read within bounds, as the rules page
// reads one; nothing to look for is no search; a rule whose page a
// redirect would lose is listed, not opened; and the rules page given the
// words reads them as words, in every team, so that it shows what was
// counted.
func TestSearchBounded(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, nil)
	var b strings.Builder
	b.WriteString(ruleFile + "  - name: jobs\n    rules:\n      - {alert: \"..\", expr: up == 2}\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "      - {alert: \"job:x%02d\", expr: up == %d}\n", i, i+3)
	}
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(b.String()), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()

	for _, q := range []string{"..", "."} {
		if code, body, _ := get(t, h, "/search?q="+url.QueryEscape(q)); code != http.StatusOK || !strings.Contains(body, `href="/rules/.."`) {
			t.Errorf("/search?q=%s: %d, the rule .. not listed", q, code)
		}
	}

	_, body, _ := get(t, h, "/search?q=job:x")
	if !strings.Contains(body, "60 found for “<bdi>job:x</bdi>”.") {
		t.Error("/search?q=job:x: not 60 found, set apart")
	}
	more := regexp.MustCompile(`<a href="([^"]*)">All 60 on the rules page</a>`).FindStringSubmatch(body)
	if more == nil {
		t.Fatal("/search?q=job:x: no link to all 60")
	}
	u, _ := url.Parse(strings.ReplaceAll(more[1], "&amp;", "&"))
	if u.Path != "/rules" || u.Query().Get("q") != `"job:x"` || !u.Query().Has("team") || u.Query().Get("team") != "" {
		t.Errorf("all 60: %s", more[1])
	}
	if v := rulesFor(t, s, u.RawQuery); len(v.Rows)+len(v.Quiet) != 60 || len(v.Bad) != 0 {
		t.Errorf("all 60: the rules page shows %d and %d quiet, left out %q", len(v.Rows), len(v.Quiet), v.Bad)
	}

	// Twenty words, the rest left out, and said so.
	q := strings.Repeat("job:x ", 20) + "nothing-is-called-this"
	if _, body, _ := get(t, h, "/search?q="+url.QueryEscape(q)); !strings.Contains(body, "60 found for") ||
		!strings.Contains(body, "Left out: more than 20 words: <bdi>nothing-is-called-this</bdi>.") {
		t.Error("21 words: not 20 used, and the rest named")
	}
	// Too long, or nothing to look for: no search.
	for _, q := range []string{strings.Repeat("x", 501), `"`, `""  "`} {
		code, body, _ := get(t, h, "/search?q="+url.QueryEscape(q))
		if code != http.StatusOK || !strings.Contains(body, "Rules by name, incidents by id or title") || strings.Contains(body, "<h2>") || strings.Contains(body, "found for") {
			t.Errorf("/search?q=%.10s: %d, not the search's help alone", q, code)
		}
		if strings.Contains(body, `value="xxxxx`) {
			t.Error("a search too long is in the bar's box")
		}
		if long := len(q) > maxSearch; long != strings.Contains(body, "The search is too long: 500 characters at most.") {
			t.Errorf("/search?q=%.10s: says it is too long: %v", q, !long)
		}
	}
}
