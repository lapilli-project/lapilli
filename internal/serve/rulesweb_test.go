package serve

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// facetServer is a server with rules of every kind the facets tell apart:
// noisy and not, in three groups, of three severities and none, some
// owned by a team, some with a note, two with a proposal, one recorded
// elsewhere, some silent, and two that fired only before the last four
// weeks.
func facetServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	s, rules := server(t, nil)
	var b strings.Builder
	b.WriteString("groups:\n")
	for g := 0; g < 3; g++ {
		fmt.Fprintf(&b, "  - name: g%d\n    rules:\n", g)
		for i := g; i < 24; i += 3 {
			sev := []string{"critical", "warning", "info", ""}[i%4]
			labels := ""
			if sev != "" {
				labels = fmt.Sprintf(", labels: {severity: %s, tier: t%d}", sev, i%2)
			}
			fmt.Fprintf(&b, "      - {alert: R%02d, expr: up == %d%s}\n", i, i, labels)
		}
	}
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(b.String()), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	o := s.cfg.Options
	record := func(at time.Time, fs []firing.Firing) {
		if err := s.l.Record(ctx, ledger.Found{From: at, To: at.Add(time.Hour), Firings: fs, Run: ledger.Run{Kind: "backfill", Started: at, Finished: at}}, o); err != nil {
			t.Fatal(err)
		}
	}
	day := now.Add(-26 * time.Hour)
	var fs []firing.Firing
	for i := 0; i < 16; i++ {
		n := 2
		if i < 6 {
			n = 12 // noisy: ten firings or more, none catching anything
		}
		for k := 0; k < n; k++ {
			at := day.Add(time.Duration(k) * 2 * time.Minute)
			fs = append(fs, firing.Firing{Rule: fmt.Sprintf("R%02d", i), Labels: map[string]string{"service": "search", "k": fmt.Sprint(k)}, Start: at, End: at.Add(time.Minute)})
		}
	}
	record(day, fs)
	old := now.Add(-50 * 24 * time.Hour)
	record(old, []firing.Firing{{Rule: "R16", Start: old, End: old.Add(time.Minute)}, {Rule: "R17", Start: old, End: old.Add(time.Minute)}})
	if _, err := s.l.PutFirings(ctx, "cloud", day, day.Add(24*time.Hour), []firing.Firing{{Rule: "Copied", Labels: map[string]string{"severity": "critical"},
		Start: day, End: day.Add(time.Minute)}}, "firings-token", o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^R0"}, "ana"); err != nil {
		t.Fatal(err)
	}
	for i, st := range []string{"keep", "tune", "retire", "tune", "", "keep"} {
		if st != "" {
			if err := s.l.PutNote(ctx, ledger.Note{Rule: fmt.Sprintf("R%02d", i*3), Status: st}, "ana"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, r := range []string{"R01", "R20"} {
		if _, err := s.propose(ctx, ask{Rule: r, Kind: ledger.KindFor, For: 15 * time.Minute}, "ana", false, true); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// rulesFor is the rules list for a query, worked out as the page is.
func rulesFor(t *testing.T, s *Server, query string) rulesList {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/rules?"+query, nil)
	r, vr := s.withContext(r, "rules")
	_ = r
	v, err := s.rulesList(vr)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v.(rulesList)
}

func rowNames(rows []ruleRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// TestFacetCounts: for filters picked at random, each option's count is how
// many rules the list shows with that option picked alone in its facet,
// and what the others pick; and the rules shown are those each filter
// lets through.
func TestFacetCounts(t *testing.T) {
	s := facetServer(t)
	all := rulesFor(t, s, "weeks=4")
	if all.Total != 25 {
		t.Fatalf("%d rules, want 25: %q, quiet %q", all.Total, rowNames(all.Rows), rowNames(all.Quiet))
	}
	perf := map[string]int{}
	for _, o := range all.Facets[0].Options {
		perf[o.Value] = o.Count
	}
	if want := map[string]int{"noisy": 6, "ok": 11, "silent": 8, "spare": 0}; !reflect.DeepEqual(perf, want) {
		t.Errorf("performance: %v, want %v", perf, want)
	}
	if all.Shown != 17 || len(all.Quiet) != 8 || !all.QuietOlder {
		t.Errorf("shown %d, quiet %d (%v), want 17 and 8, some older", all.Shown, len(all.Quiet), all.QuietOlder)
	}
	r := rand.New(rand.NewPCG(5, 6))
	texts := []string{"", "r0", "label:tier=t1", "group:g2", "r1 group:g1", "label:severity=critical"}
	checked := 0
	for i := 0; i < 40; i++ {
		q := url.Values{}
		if x := texts[r.IntN(len(texts))]; x != "" {
			q.Set("q", x)
		}
		if r.IntN(3) == 0 {
			q.Set("team", "payments")
		}
		for _, f := range all.Facets {
			for _, o := range f.Options {
				if r.IntN(4) == 0 {
					q.Add(f.Param, o.Value)
				}
			}
		}
		v := rulesFor(t, s, q.Encode())
		for _, f := range v.Facets {
			for _, o := range f.Options {
				alone := url.Values{}
				for k, vs := range q {
					alone[k] = vs
				}
				alone[f.Param] = []string{o.Value}
				if got := rulesFor(t, s, alone.Encode()).Shown; got != o.Count {
					t.Errorf("%s: %s=%s counts %d, and shows %d", q.Encode(), f.Param, o.Value, o.Count, got)
				}
				checked++
			}
		}
		// Each rule shown passes each filter.
		for _, row := range v.Rows {
			for _, f := range facetsOf {
				if picked := q[f.param]; len(picked) > 0 && !slices.ContainsFunc(row.values(f.param), func(x string) bool { return slices.Contains(picked, x) }) {
					t.Errorf("%s: %s shown, its %s %q", q.Encode(), row.Name, f.param, row.values(f.param))
				}
			}
			if q.Get("team") != "" && !strings.HasPrefix(row.Name, "R0") {
				t.Errorf("%s: %s isn't payments'", q.Encode(), row.Name)
			}
		}
	}
	if checked < 400 {
		t.Errorf("only %d counts checked", checked)
	}
}

// TestRulesFilters: what the URL asks for, and how the page says it.
func TestRulesFilters(t *testing.T) {
	s := facetServer(t)
	h := s.Handler()

	// The search box's filters for a facet become parameters, by a
	// redirect to a page here.
	for q, to := range map[string]string{
		"q=is:noisy+r0": "/rules?perf=noisy&q=r0",
		"q=severity:critical+team:payments&weeks=12": "/rules?q=&severity=critical&team=payments&weeks=12",
		"q=status:tune+has:proposal+label:tier=t1":   "/rules?has=proposal&q=label%3Atier%3Dt1&status=tune",
	} {
		code, _, hd := get(t, h, "/rules?"+q)
		want, _ := url.Parse(to)
		got, _ := url.Parse(hd.Get("Location"))
		if code != http.StatusSeeOther || got.Path != "/rules" || got.Host != "" || got.Query().Encode() != strings.ReplaceAll(want.Query().Encode(), "q=&", "") {
			t.Errorf("/rules?%s: %d to %q, want %q", q, code, hd.Get("Location"), to)
		}
	}
	r := rand.New(rand.NewPCG(7, 8))
	parts := []string{"is:noisy", "team:payments", "team://evil.example", "//evil.example", `\evil`, "severity:/../x", "label:a=b", "x:y", " ", "https://e.example"}
	for i := 0; i < 200; i++ {
		var q []string
		for j := r.IntN(4); j >= 0; j-- {
			q = append(q, parts[r.IntN(len(parts))])
		}
		code, _, hd := get(t, h, "/rules?q="+url.QueryEscape(strings.Join(q, " ")))
		if loc := hd.Get("Location"); code == http.StatusSeeOther && (!strings.HasPrefix(loc, "/rules?") || strings.Contains(loc, "//")) {
			t.Errorf("%q: to %q", q, loc)
		} else if code != http.StatusSeeOther && code != http.StatusOK {
			t.Errorf("%q: %d", q, code)
		}
	}

	// What it doesn't understand is said, and left out.
	code, body, _ := get(t, h, "/rules?q=foo:bar+is:loud+r0")
	if code != http.StatusOK || !strings.Contains(body, "Left out, as it isn&#39;t understood: <bdi>foo:bar, is:loud</bdi>.") || !strings.Contains(body, "label:key=value") {
		t.Errorf("unknown keys: %d", code)
	}
	if v := rulesFor(t, s, "q=foo:bar+is:loud+r0"); v.Shown == 0 || slices.ContainsFunc(v.Rows, func(r ruleRow) bool { return !strings.Contains(r.Name, "R0") }) {
		t.Errorf("unknown keys aren't left out: %q", rowNames(v.Rows))
	}

	// A chip for each filter, each the same page without it; clearing keeps
	// the team, the weeks and the order.
	_, body, _ = get(t, h, "/rules?perf=noisy&severity=critical&q=r0+label:tier=t1&team=payments&weeks=12&sort=refired")
	for _, want := range []string{
		`<a class="chip" href="/rules?perf=noisy&amp;q=r0&#43;label%3Atier%3Dt1&amp;severity=critical&amp;sort=refired&amp;team=&amp;weeks=12"><span class="vh">Remove filter: </span>Team: <bdi>payments</bdi>`,
		`<a class="chip" href="/rules?q=r0&#43;label%3Atier%3Dt1&amp;severity=critical&amp;sort=refired&amp;team=payments&amp;weeks=12"><span class="vh">Remove filter: </span>Performance: Noisy`,
		`<a class="chip" href="/rules?perf=noisy&amp;q=r0&#43;label%3Atier%3Dt1&amp;sort=refired&amp;team=payments&amp;weeks=12"><span class="vh">Remove filter: </span>Severity: <bdi>critical</bdi>`,
		`<a class="chip" href="/rules?perf=noisy&amp;q=label%3Atier%3Dt1&amp;severity=critical&amp;sort=refired&amp;team=payments&amp;weeks=12"><span class="vh">Remove filter: </span>Name has: <bdi>r0</bdi>`,
		`<a class="chip" href="/rules?perf=noisy&amp;q=r0&amp;severity=critical&amp;sort=refired&amp;team=payments&amp;weeks=12"><span class="vh">Remove filter: </span>Label: <bdi>tier=t1</bdi>`,
		`<a href="/rules?sort=refired&amp;team=payments&amp;weeks=12">Clear filters</a>`,
		`<details class="facets-menu" open>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}

	// Silent rules: listed apart, or as rows when picked.
	v := rulesFor(t, s, "perf=silent")
	if len(v.Quiet) != 0 || len(v.Rows) != 8 || slices.ContainsFunc(v.Rows, func(r ruleRow) bool { return r.Perf != "silent" || r.Firings != 0 }) {
		t.Errorf("perf=silent: rows %q, quiet %d", rowNames(v.Rows), len(v.Quiet))
	}
	if !slices.Contains(rowNames(v.Rows), "R16") || len(rulesFor(t, s, "perf=silent&weeks=12").Rows) != 6 {
		t.Error("a rule that fired before the weeks shown, and not in them, isn't silent in them")
	}

	// Nothing matching: the ways out, and the same in all teams.
	_, body, _ = get(t, h, "/rules?team=payments&q=nothing-is-called-this")
	for _, want := range []string{`class="empty empty-notfound"`, "No rule matches these filters.", `<a href="/rules?team=payments">Clear filters</a>`,
		`<a href="/rules?q=nothing-is-called-this&amp;team=">The same filters, in all teams</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("nothing found: no %s", want)
		}
	}
	// Nothing judged yet.
	fresh, _ := server(t, nil)
	if _, body, _ := get(t, fresh.Handler(), "/rules"); !strings.Contains(body, `class="empty empty-first"`) {
		t.Error("/rules before the first run: no empty state")
	}
	// The badges: a sign for the eye, a name for everyone.
	_, body, _ = get(t, h, "/rules?weeks=12")
	for _, want := range []string{`<span aria-hidden="true">●</span> Noisy`, `<span aria-hidden="true">○</span> OK`, `<span aria-hidden="true">⇣</span> Recorded only`,
		`<span class="badge pill tune"><span aria-hidden="true">±</span> tune</span>`, `href="/rules/R01?weeks=12#proposals"><span aria-hidden="true">✎</span> Proposed</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("/rules?weeks=12: no %s", want)
		}
	}
}

// TestRulesSameData: the URLs the rules page took before show the rules
// they showed, in the same order, with the same numbers.
func TestRulesSameData(t *testing.T) {
	for name, s := range map[string]*Server{"pages": pagesServer(t), "facets": facetServer(t)} {
		for _, q := range []string{"", "weeks=1", "weeks=12", "team=payments", "weeks=12&team=payments&sort=refired", "sort=deploys&weeks=12", "sort=refired"} {
			r := httptest.NewRequest(http.MethodGet, "/rules?"+q, nil)
			d, err := s.rulesPage(r)
			if err != nil {
				t.Fatal(err)
			}
			rv := d.(rulesView)
			v := rulesFor(t, s, q)
			var lines []ruleLine
			for _, row := range v.Rows {
				lines = append(lines, row.ruleLine)
			}
			if !reflect.DeepEqual(lines, rv.Lines) {
				t.Errorf("%s /rules?%s: rows %q, the API's %d lines", name, q, rowNames(v.Rows), len(rv.Lines))
			}
			for _, n := range rv.Silent {
				if !slices.ContainsFunc(v.Quiet, func(r ruleRow) bool { return r.Name == n }) {
					t.Errorf("%s /rules?%s: %s never fired, and isn't listed", name, q, n)
				}
			}
		}
	}
}

// TestRulesListScale: the rules list over two thousand rules, twelve weeks
// of firings, with filters, each worked out afresh, holds to a budget.
func TestRulesListScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	s, rules := server(t, nil)
	const n = 2000
	var b strings.Builder
	b.WriteString("groups:\n")
	for g := 0; g < 20; g++ {
		fmt.Fprintf(&b, "  - name: g%d\n    rules:\n", g)
		for i := g; i < n; i += 20 {
			fmt.Fprintf(&b, "      - {alert: Rule%04d, expr: up == %d, labels: {severity: %s, tier: t%d}}\n", i, i, []string{"critical", "warning", "info"}[i%3], i%4)
		}
	}
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(b.String()), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	o := s.cfg.Options
	r := rand.New(rand.NewPCG(9, 10))
	start := now.Add(-weeks * 7 * 24 * time.Hour)
	for d := start; d.Before(now); d = d.Add(24 * time.Hour) {
		var fs []firing.Firing
		for i := 0; i < 3000; i++ {
			at := d.Add(time.Duration(r.IntN(24*60)) * time.Minute)
			fs = append(fs, firing.Firing{Rule: fmt.Sprintf("Rule%04d", r.IntN(n*9/10)), Labels: map[string]string{"service": "s"}, Start: at, End: at.Add(time.Minute)})
		}
		if err := s.l.Record(ctx, ledger.Found{From: d, To: d.Add(24*time.Hour - time.Second), Firings: fs, Run: ledger.Run{Kind: "backfill", Started: d, Finished: d}}, o); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := s.l.PutTeam(ctx, ledger.Team{Name: fmt.Sprintf("team%d", i), Rules: fmt.Sprintf("^Rule%d", i)}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Handler()
	for _, path := range []string{"/api/v1/rules", "/api/v1/rules?weeks=12", "/rules", "/rules?weeks=12", "/rules?perf=noisy&severity=critical&q=rule1+label:tier=t1",
		"/rules?perf=silent&weeks=1", "/rules?team=team3&status=none&trust=differs&trust=unreplayed", "/search?q=rule12",
		"/search?q=r+u+l+e+0+1+2+3+4+5+6+7+8+9+ru+ul+le+e0+e1+e2"} {
		s.cache.Lock()
		s.cache.gen = ""
		s.cache.Unlock()
		began := time.Now()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		took := time.Since(began)
		t.Logf("%-70s %d %8d bytes  %s", path, w.Code, w.Body.Len(), took.Round(time.Microsecond))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
		}
		if budget := time.Duration(float64(time.Second) * slow); took > budget {
			t.Errorf("%s took %s, over %s", path, took, budget)
		}
	}
}

// TestRulesAskBounded: what the rules page is asked is read within bounds,
// once. A search over 500 characters is left out whole, and said so; 20
// of its words are used, and the rest named; a facet takes 20 values,
// each once; and the links the page makes to itself hold what it read,
// not what else the URL had.
func TestRulesAskBounded(t *testing.T) {
	s := facetServer(t)
	h := s.Handler()
	all := rulesFor(t, s, "")

	// Too long: nothing of it used, not even a filter to move.
	long := "is:noisy " + strings.Repeat("x", 500)
	code, body, _ := get(t, h, "/rules?q="+url.QueryEscape(long))
	if code != http.StatusOK || !strings.Contains(body, "The search is too long: 500 characters at most.") || strings.Count(body, `<input type="search" name="q" value="" maxlength="500"`) != 3 {
		t.Errorf("a search too long: %d", code)
	}
	if v := rulesFor(t, s, "q="+url.QueryEscape(long)); !v.TooLong || v.Shown != all.Shown || v.Q != "" || len(v.Chips) != 0 {
		t.Errorf("a search too long: shown %d of %d, q %q, chips %d", v.Shown, all.Shown, v.Q, len(v.Chips))
	}
	if v := rulesFor(t, s, "q="+url.QueryEscape(strings.Repeat("é", 500))); v.TooLong {
		t.Error("500 characters are too long")
	}

	// Twenty words used, the rest named and left out of what is matched,
	// and of the links.
	var words []string
	for i := 0; i < 19; i++ {
		words = append(words, "r")
	}
	words = append(words, "r0", "nothing-is-called-this", "w22")
	v := rulesFor(t, s, "q="+url.QueryEscape(strings.Join(words, " ")))
	if v.Shown == 0 || slices.ContainsFunc(v.Rows, func(r ruleRow) bool { return !strings.HasPrefix(r.Name, "R0") }) {
		t.Errorf("20 words: %q", rowNames(v.Rows))
	}
	if !slices.Equal(v.Over, []string{"nothing-is-called-this", "w22"}) || strings.Contains(v.Q, "w22") {
		t.Errorf("20 words: over %q, q %q", v.Over, v.Q)
	}
	for _, l := range v.SortLinks {
		if !strings.Contains(l.Href, "r0") || strings.Contains(l.Href, "w22") {
			t.Errorf("an order: %s, the words used, and none left out", l.Href)
		}
	}
	_, body, _ = get(t, h, "/rules?q="+url.QueryEscape(strings.Join(words, " ")))
	if !strings.Contains(body, "Left out: more than 20 words: <bdi>nothing-is-called-this w22</bdi>.") {
		t.Error("20 words: the rest aren't named")
	}

	// A facet takes 20 values, a value repeated once.
	q := url.Values{}
	for i := 0; i < 30; i++ {
		q.Add("severity", "critical")
	}
	for i := 0; i < 30; i++ {
		q.Add("severity", fmt.Sprintf("s%02d", i))
	}
	v = rulesFor(t, s, q.Encode())
	picked := 0
	for _, o := range v.Facets[1].Options {
		if o.Checked {
			picked++
		}
	}
	if picked != 20 || len(v.Chips) != 20 || v.Shown != rulesFor(t, s, "severity=critical").Shown {
		t.Errorf("60 severities, 31 of them other: %d picked, %d chips, %d shown", picked, len(v.Chips), v.Shown)
	}

	// The q= of a URL are one search; a chip, an order, the same page
	// without what it doesn't take.
	v = rulesFor(t, s, "q=r0&q=label:tier=t1&perf=noisy&perf=loud&foo=bar&weeks=7&sort=refired")
	if v.Shown == 0 || slices.ContainsFunc(v.Rows, func(r ruleRow) bool { return !strings.HasPrefix(r.Name, "R0") || r.labels["tier"] != "t1" }) {
		t.Errorf("q= twice: %q", rowNames(v.Rows))
	}
	var hrefs []string
	for _, c := range v.Chips {
		hrefs = append(hrefs, c.Href)
	}
	for _, l := range v.SortLinks {
		hrefs = append(hrefs, l.Href)
	}
	for _, href := range hrefs {
		if strings.Contains(href, "foo") || strings.Contains(href, "loud") || strings.Contains(href, "weeks") {
			t.Errorf("a link the page makes to itself has what it doesn't take: %s", href)
		}
	}
}

// TestRulesSortLinks: an order is the same page, every filter and the
// search kept, in that order; the one in effect says so.
func TestRulesSortLinks(t *testing.T) {
	s := facetServer(t)
	_, body, _ := get(t, s.Handler(), "/rules?perf=noisy&severity=critical&q=r0+label:tier=t1&team=payments&weeks=12&sort=refired")
	for _, want := range []string{
		`<a href="/rules?perf=noisy&amp;q=r0&#43;label%3Atier%3Dt1&amp;severity=critical&amp;team=payments&amp;weeks=12">caught nothing, the most first</a>`,
		`<a href="/rules?perf=noisy&amp;q=r0&#43;label%3Atier%3Dt1&amp;severity=critical&amp;sort=refired&amp;team=payments&amp;weeks=12" aria-current="page">re-fired, the most first</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}
	if n := strings.Count(body, `aria-current="page">re-fired`) + strings.Count(body, `aria-current="page">caught nothing`); n != 1 {
		t.Errorf("%d orders in effect", n)
	}
}

// TestSeveritySpelled: a severity is the rules' own spelling, however the
// URL or the search box writes it, so that one is one option and one chip;
// and it is shown as a name is.
func TestSeveritySpelled(t *testing.T) {
	s := facetServer(t)
	h := s.Handler()
	want := rulesFor(t, s, "severity=critical")
	for _, q := range []string{"severity=Critical", "severity=CRITICAL&severity=critical"} {
		v := rulesFor(t, s, q)
		var sevs []string
		for _, o := range v.Facets[1].Options {
			sevs = append(sevs, o.Value)
		}
		if v.Shown != want.Shown || len(v.Chips) != 1 || v.Chips[0].Value != "critical" || slices.Contains(sevs, "Critical") || slices.Contains(sevs, "CRITICAL") {
			t.Errorf("%s: shown %d, chips %+v, options %q", q, v.Shown, v.Chips, sevs)
		}
	}
	_, _, hd := get(t, h, "/rules?q=severity:Critical")
	_, body, _ := get(t, h, hd.Get("Location"))
	if strings.Count(body, `<input type="checkbox" name="severity" value="critical" checked>`) != 2 || strings.Contains(body, `type="checkbox" name="severity" value="Critical"`) {
		t.Errorf("severity:Critical: not the rules' critical, in both of the facets' forms")
	}
	_, body, _ = get(t, h, "/rules?severity="+url.QueryEscape("x‮y"))
	if !strings.Contains(body, "<span><bdi>x�y</bdi></span>") || strings.Contains(body, "<span>x‮y") {
		t.Error("a severity from the URL isn't shown as a name is")
	}
}

// TestLongQueryRefused: a page asked with a query past 4096 bytes is
// refused, 414, with a page of its own size, in the reader's language;
// one of 4096 is a page.
func TestLongQueryRefused(t *testing.T) {
	s := facetServer(t)
	h := s.Handler()
	for _, path := range []string{"/", "/rules", "/rules/R01", "/search", "/tasks", "/incidents"} {
		q := "q=" + strings.Repeat("a", maxQuery-2)
		if code, _, _ := get(t, h, path+"?"+q); code == http.StatusRequestURITooLong {
			t.Errorf("%s, a query of 4096 bytes: %d", path, code)
		}
		code, body, _ := get(t, h, path+"?"+q+"a")
		if code != http.StatusRequestURITooLong || len(body) > 8000 || strings.Contains(body, "aaaaaaaa") || strings.Contains(body, `class="count"`) ||
			!strings.Contains(body, "this address asks too much: a page&#39;s query is 4096 bytes at most") {
			t.Errorf("%s, a query of 4097 bytes: %d, %d bytes", path, code, len(body))
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/rules?q="+strings.Repeat("group:a+", 7900), nil)
	r.Header.Set("Accept-Language", "ko")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusRequestURITooLong || !strings.Contains(w.Body.String(), "주소가 너무 길어요") {
		t.Errorf("63 KB of group:a, in Korean: %d", w.Code)
	}
}
