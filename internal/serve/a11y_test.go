package serve

// The UI against WCAG 2.2 AA, as far as a test can tell from the HTML and
// the CSS: every page in every language. test/a11y/axe.sh checks the same
// pages in a browser, with axe-core.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// a11yServer is pagesServer with a proposal, a rule that caught nothing
// (for the noise's heatmap), a task with an issue in a tracker, people's
// verdicts and the episodes counted (their forms, words and columns, which
// test/a11y/axe.mjs reads too), and the pages to read: those
// TestPagesInEachLanguage renders, the proposals, and a page not found.
func a11yServer(t *testing.T) (*Server, []string) {
	t.Helper()
	ctx := context.Background()
	s := pagesServer(t)
	peopleSay(t, s)
	if _, err := s.l.CountEpisodes(ctx, s.cfg.Options, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.propose(ctx, ask{Rule: "Errors", Kind: ledger.KindFor, For: 15 * time.Minute}, "ana", false, true); err != nil {
		t.Fatal(err)
	}
	from := now.Add(-20 * 24 * time.Hour)
	if err := s.l.Record(ctx, ledger.Found{From: from, To: from.Add(time.Hour), Run: ledger.Run{Kind: "backfill", Started: from, Finished: from},
		Firings: []firing.Firing{{Rule: "Noise", Labels: map[string]string{"service": "search"}, Start: from, End: from.Add(10 * time.Minute)}}},
		s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "linked", Kind: "tune", Rule: "Noise", Link: "https://tracker.example/1"}, "ana"); err != nil {
		t.Fatal(err)
	}
	// A change an agent proposed, applied, that waited while its rule
	// fired, and its undo: their pages, the history's steps, the backtest
	// read the other way.
	five := 5 * time.Minute
	ev, _ := json.Marshal(evidence{From: "agent", Reason: "fewer pages", Edit: &rules.Edit{Name: "Noise", For: &five},
		Backtest: &backtest{From: now.Add(-weekLen), To: now, Before: side{Firings: 9, White: 1}, After: side{Firings: 2, White: 1}, Gained: []string{"INC-1"}, Later: []string{"INC-1"}}})
	id, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Noise", Kind: ledger.KindFor, Path: ledger.PathGrafana, MadeBy: "bo", Agent: true,
		Before: "Noise ()\nfor: 0s\n", After: "Noise ()\nfor: 5m\n", Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ from, to, reason string }{{ledger.StateProposed, ledger.StateApproved, ""}, {ledger.StateApproved, ledger.StateApproved, firesNote},
		{ledger.StateApproved, ledger.StateApplying, firedNote}, {ledger.StateApplying, ledger.StateApplied, ""}} {
		if _, err := s.l.Move(ctx, id, []string{m.from}, m.to, ledger.Proposer, ledger.Step{Reason: m.reason}); err != nil {
			t.Fatal(err)
		}
	}
	uev, _ := json.Marshal(undoEvidence(proposalEvidence(state(t, s, id))))
	undo, err := s.l.Undo(ctx, ledger.Proposal{Rule: "Noise", Kind: ledger.KindUndo, Path: ledger.PathGrafana, Follows: id, Before: "Noise ()\nfor: 5m\n",
		After: "Noise ()\nfor: 0s\n", Evidence: uev}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	return s, append(append(slices.Clone(pagePaths), "/proposals", "/proposals?state=all", "/proposals/1", "/rules/Nothing",
		"/proposals/"+strconv.FormatInt(id, 10), "/proposals/"+strconv.FormatInt(undo, 10)), declaredPages(t, s)...)
}

// longNames are rules named as a real set's can be: long, in one word,
// with long label values and more labels than a row of the rules list
// shows.
var longNames = []string{
	"KubernetesPersistentVolumeFillingUpPredictedWithinFourDaysOnTheLargestClusters",
	"ServiceLevelObjectiveErrorBudgetBurnRateTooHighForTheAuthorizationGatewayAPI",
	"ObjectStorageReplicationLagAboveThresholdBetweenPrimaryAndSecondaryRegions",
	"MessageQueueConsumerGroupLagGrowingForTheSettlementEventsPipelineWorkers",
	"DatabaseConnectionPoolSaturatedForReadReplicasBehindTheInternalLoadBalancer",
	"CertificateExpiringWithinFourteenDaysForTheCustomerFacingIngressHostnames",
}

// longNamed gives s the rules longNames names, owned by a team with a long
// name, each fired over the twelve weeks: the rules list as a real
// deployment's can be, which test/a11y/axe.mjs holds to fit a laptop's
// screen.
func longNamed(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	b.WriteString("groups:\n  - name: observability-and-site-reliability-engineering-platform\n    labels: {environment: production-primary-region-north-east}\n    rules:\n")
	for i, n := range longNames {
		fmt.Fprintf(&b, "      - {alert: %s, expr: up == %d, for: 5m, labels: {severity: %s, component: ingress-controller-public-edge-load-balancer-fleet-%d, "+
			"tier: customer-facing-critical-path, service: settlement-and-reconciliation-batch-processing, runbook_section: persistent-volumes-and-storage-classes, "+
			"owner: platform-reliability-on-call-rotation-secondary}}\n", n, 100+i, []string{"critical", "warning"}[i%2], i)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.RulesPath, "long.yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	var fs []firing.Firing
	for w := 0; w < weeks; w++ {
		for i, n := range longNames {
			at := now.Add(-time.Duration(w*7*24+i+1) * time.Hour)
			fs = append(fs, firing.Firing{Rule: n, Labels: map[string]string{"service": "settlement-and-reconciliation-batch-processing"}, Start: at, End: at.Add(20 * time.Minute)})
		}
	}
	from := now.Add(-weeks * weekLen)
	if err := s.l.Record(ctx, ledger.Found{From: from, To: now, Firings: fs, Run: ledger.Run{Kind: "backfill", Started: from, Finished: from}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "observability-and-site-reliability-engineering", Rules: "^(Kubernetes|ServiceLevel|ObjectStorage)"}, "ana"); err != nil {
		t.Fatal(err)
	}
}

// TestA11yServe serves the pages at LAPILLI_A11Y_SERVE until it is
// stopped, for test/a11y/axe.sh; /a11y-paths lists them.
func TestA11yServe(t *testing.T) {
	addr := serveAddr(t, "LAPILLI_A11Y_SERVE")
	s, mux, paths := a11yServed(t)
	mux.HandleFunc("GET /a11y-paths", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, strings.Join(paths, "\n"))
	})
	t.Logf("serving %d pages", len(paths))
	serveTest(t, s, addr, mux)
}

// a11yBacktests is where TestA11yServe serves backtestsServer's pages.
const a11yBacktests = "/pr-backtests"

// a11yServed are the pages TestA11yServe serves, on mux, and their paths:
// a11yServer's, and other deployments' under a path each.
func a11yServed(t *testing.T) (*Server, *http.ServeMux, []string) {
	t.Helper()
	s, paths := a11yServer(t)
	longNamed(t, s)
	more, _ := statusStates(t, s) // each status kind drawn
	paths = append(paths, more...)
	paths = append(paths, manyLabelled(t, s)) // an incident's labels summed up, behind a details
	s.cfg.AllowedHosts = append(s.cfg.AllowedHosts, "host.docker.internal")
	mux := http.NewServeMux()
	paths = append(paths, a11yPreviews(mux, s)...)
	// A first-time user's deployment, under /first.
	first, firstPaths := firstUseServer(t)
	first.cfg.AllowedHosts = append(first.cfg.AllowedHosts, "host.docker.internal")
	mux.Handle("/first/", http.StripPrefix("/first", first.Handler()))
	for _, p := range firstPaths {
		paths = append(paths, "/first"+p)
	}
	// The incident sources' other states (sourcesStates).
	for prefix, src := range sourcesStates(t) {
		src.cfg.AllowedHosts = append(src.cfg.AllowedHosts, "host.docker.internal")
		mux.Handle(prefix+"/", http.StripPrefix(prefix, src.Handler()))
		paths = append(paths, prefix+"/settings")
	}
	// The backtest port on, a backtest in each state on the Run history.
	bt := backtestsServer(t)
	bt.cfg.AllowedHosts = append(bt.cfg.AllowedHosts, "host.docker.internal")
	mux.Handle(a11yBacktests+"/", http.StripPrefix(a11yBacktests, bt.Handler()))
	paths = append(paths, a11yBacktests+"/runs")
	mux.Handle("/", s.Handler())
	return s, mux, paths
}

// backtestsServer is serve with the backtest port on (btServer) and a pull
// request backtest of each state kept, as the leader's lane leaves them:
// done and failed within the second they started, done in 2m5s, failed in
// 12s, of a plain token with no repository, one cancelled as it ran,
// within the second, one superseded by a newer commit, one cancelled
// while it waited, one running and one waiting. Each says which it is by
// its rules that differ, 1 to 9 in that order.
func backtestsServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	s := btServer(t, nil, "", nil).s
	at := now.Add(-3 * time.Hour)
	n := 0
	// Each of a pull request its token's ref names, as a CI's ID token's.
	ask := func(trust, repo string, pr int64) string {
		t.Helper()
		n++
		id := fmt.Sprintf("bt-%d", n)
		if _, _, _, err := s.l.AskBacktest(ctx, ledger.BacktestAsk{Backtest: ledger.Backtest{ID: id, Trust: trust, Repository: repo, PullRequest: pr,
			Commit: "0123abcd", Rules: n, Requests: 40, KeyHash: "k-" + id}, Body: []byte(`{"after":[]}`), BodyHash: "h-" + id,
			Until: at.Add(2 * time.Hour), Queue: 20, PerTrust: 10, Supersede: pr > 0}, at); err != nil {
			t.Fatal(err)
		}
		at = at.Add(10 * time.Minute)
		return id
	}
	run := func(state string, took time.Duration, made int) {
		t.Helper()
		b, _, ok, err := s.l.StartBacktest(ctx, at)
		if err != nil || !ok {
			t.Fatalf("nothing to start: %t %v", ok, err)
		}
		why := ""
		if state == ledger.BacktestFailed {
			why = "the history didn't answer"
		}
		if err := s.l.EndBacktest(ctx, b.ID, state, why, made, nil, at.Add(took)); err != nil {
			t.Fatal(err)
		}
	}
	ask("alerts", "example/alerts", 1)
	run(ledger.BacktestDone, 0, 37)
	ask("monitoring-gitlab", "example/monitoring", 2)
	run(ledger.BacktestFailed, 0, 3)
	ask("forgejo", "example/forgejo-rules", 3)
	run(ledger.BacktestDone, 2*time.Minute+5*time.Second, 80)
	ask("plain", "", 0)
	run(ledger.BacktestFailed, 12*time.Second, 9)
	ask("forgejo", "example/forgejo-rules", 4)
	run(ledger.BacktestCancelled, 0, 2)
	ask("alerts", "example/alerts", 6)
	ask("alerts", "example/alerts", 6) // the one before superseded
	cancelled := ask("monitoring-gitlab", "example/monitoring", 8)
	if ok, err := s.l.CancelBacktest(ctx, cancelled, at); err != nil || !ok {
		t.Fatalf("cancelled: %t %v", ok, err)
	}
	if _, _, ok, err := s.l.StartBacktest(ctx, at); err != nil || !ok { // the newer commit's
		t.Fatalf("nothing to start: %t %v", ok, err)
	}
	ask("forgejo", "example/forgejo-rules", 9)
	return s
}

// backtestsShownIn are, by language, the Run history's backtests' column
// heads, and what a backtest that ran and ended within the second it
// started took.
var backtestsShownIn = map[string]struct {
	heads []string
	under string
}{
	"en": {[]string{"Asked", "Trust entry", "Repository", "Rules that differ", "Queries", "Took", "How it ended"}, "under a second"},
	"es": {[]string{"Pedido", "Entrada de confianza", "Repositorio", "Reglas que difieren", "Consultas", "Duración", "Cómo terminó"}, "menos de un segundo"},
	"ja": {[]string{"依頼", "信頼エントリ", "リポジトリ", "違うルール", "クエリ数", "所要時間", "結果"}, "1秒未満"},
	"ko": {[]string{"요청 시각", "신뢰 항목", "저장소", "달라진 규칙", "쿼리 수", "걸린 시간", "결과"}, "1초 미만"},
}

// backtestsTable is the Run history's backtests, as a page has them: its
// column heads, and each row's cells by its rules that differ.
func backtestsTable(t *testing.T, body string) (heads []string, rows map[string][]string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var table *html.Node
	seen := false
	for _, n := range allNodes(doc) {
		if nodeAttr(n, "id") == "backtests" {
			seen = true
		} else if seen && n.DataAtom == atom.Table {
			table = n
			break
		}
	}
	if table == nil {
		return nil, nil
	}
	rows = map[string][]string{}
	for _, n := range allNodes(table) {
		switch {
		case n.DataAtom == atom.Th && nodeWithin(n, atom.Thead):
			heads = append(heads, nodeText(n))
		case n.DataAtom == atom.Tr && nodeWithin(n, atom.Tbody):
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.DataAtom == atom.Td {
					cells = append(cells, nodeText(c))
				}
			}
			if len(cells) == 7 {
				rows[cells[3]] = cells
			}
		}
	}
	return heads, rows
}

// The Run history's pull request backtests, in every language: their
// parts where assistive technology looks for them, as every page's, and
// each text translated; the trust entry and the queries asked of the
// history so named; and the time taken of one that ran and ended within
// the second it started (done, failed, or cancelled as it ran) said as
// under a second, of one waiting, running, superseded or cancelled before
// it ran, nothing.
func TestBacktestsAccessible(t *testing.T) {
	s := backtestsServer(t)
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	pageA11y(t, s.Handler(), "", "/runs")
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
	for _, n := range names {
		want := backtestsShownIn[n.Code]
		r := httptest.NewRequest(http.MethodGet, "/runs", nil)
		r.Header.Set("Accept-Language", n.Code)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		heads, rows := backtestsTable(t, w.Body.String())
		if !slices.Equal(heads, want.heads) {
			t.Errorf("%s: the column heads are %q, not %q", n.Code, heads, want.heads)
		}
		for rules, took := range map[string]string{"1": want.under, "2": want.under, "3": "2m5s", "4": "12s", "5": want.under, "6": "", "7": "", "8": "", "9": ""} {
			if row := rows[rules]; row == nil {
				t.Errorf("%s: no backtest with %s rules that differ: %q", n.Code, rules, rows)
			} else if row[5] != took {
				t.Errorf("%s: the backtest with %s rules that differ took %q, not %q: %q", n.Code, rules, row[5], took, row)
			}
		}
		if row := rows["1"]; row != nil && (row[1] != "alerts" || row[2] != "example/alerts" || row[4] != "37") {
			t.Errorf("%s: the first backtest's entry, repository and queries: %q", n.Code, row)
		}
	}
}

// What test/a11y/axe.sh checks has the Run history's pull request
// backtests, a backtest of each state on it.
func TestA11yServedBacktests(t *testing.T) {
	_, mux, paths := a11yServed(t)
	if !slices.Contains(paths, a11yBacktests+"/runs") {
		t.Fatalf("TestA11yServe doesn't list %s/runs: %q", a11yBacktests, paths)
	}
	_, body, _ := get(t, mux, a11yBacktests+"/runs")
	_, rows := backtestsTable(t, body)
	states := map[string]bool{}
	for _, row := range rows {
		states[row[6]] = true
	}
	for _, want := range []string{"✓ done", "✕ failed", "waiting", "↻ running", "superseded by a newer commit", "cancelled"} {
		if !states[want] {
			t.Errorf("%s/runs has no backtest %s: %q", a11yBacktests, want, rows)
		}
	}
}

// Every page, in every language, has its parts where assistive technology
// looks for them, and names every control, link, image and chart.
func TestPagesAccessible(t *testing.T) {
	s, paths := a11yServer(t)
	h := s.Handler()
	for _, n := range names {
		for _, p := range paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Sec-Fetch-Site", "none") // a page asked for in a browser, as axe does
			r.Header.Set("Accept-Language", n.Code)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if want := http.StatusOK; w.Code != want && !strings.HasSuffix(p, "/none") && !strings.HasSuffix(p, "/Nothing") {
				t.Errorf("%s %s: %d, not %d", n.Code, p, w.Code, want)
			}
			doc, err := html.Parse(w.Body)
			if err != nil {
				t.Fatalf("%s %s: %v", n.Code, p, err)
			}
			for _, problem := range a11yProblems(doc, n.Code) {
				t.Errorf("%s %s: %s", n.Code, p, problem)
			}
		}
	}
}

// The charts say their numbers are in the table below: the table there has
// a column for each measure they draw, and a row for each week they draw
// something for.
func TestChartsInTables(t *testing.T) {
	s, _ := a11yServer(t)
	h := s.Handler()
	date := regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	for p, measures := range map[string][]string{
		"/":         {"Fired", "○ Caught", "● Nothing", "Precision", "Incidents", "✓ Caught", "◐ Noisy only", "✗ Missed"},
		"/outcomes": {"From the file", "Written in Lapilli", "Impact windows to decide", "Edits"},
	} {
		_, body, _ := get(t, h, p)
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var table *html.Node // the last on the page, below the charts
		drawn := map[string]bool{}
		for _, n := range allNodes(doc) {
			switch {
			case n.DataAtom == atom.Table:
				table = n
			case n.Data == "title" && nodeWithin(n, atom.Svg):
				for _, d := range date.FindAllString(nodeText(n), -1) {
					drawn[d] = true
				}
			}
		}
		if len(drawn) == 0 || table == nil {
			t.Fatalf("%s: charts drawing %d weeks, and a table: %v", p, len(drawn), table != nil)
		}
		var heads []string
		weeks := map[string]bool{}
		for _, n := range allNodes(table) {
			switch {
			case n.DataAtom == atom.Th && nodeWithin(n, atom.Thead):
				heads = append(heads, nodeText(n))
			case n.DataAtom == atom.Tr && nodeWithin(n, atom.Tbody):
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.ElementNode {
						weeks[date.FindString(nodeText(c))] = true
						break
					}
				}
			}
		}
		for _, m := range measures {
			if !slices.Contains(heads, m) {
				t.Errorf("%s: the table below the charts has no %q: %q", p, m, heads)
			}
		}
		for d := range drawn {
			if !weeks[d] {
				t.Errorf("%s: a chart draws the week of %s, the table below hasn't it", p, d)
			}
		}
		// And each number a chart draws is the one the table has, in
		// that week's row and the measure's column.
		cell := map[string]map[string]string{} // week, column, text
		for _, n := range allNodes(table) {
			if n.DataAtom != atom.Tr || !nodeWithin(n, atom.Tbody) {
				continue
			}
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode {
					cells = append(cells, strings.TrimSpace(nodeText(c)))
				}
			}
			row := map[string]string{}
			for i, h := range heads {
				if i < len(cells) {
					row[h] = cells[i]
				}
			}
			cell[date.FindString(cells[0])] = row
		}
		checked := 0
		for _, n := range allNodes(doc) {
			if n.Data != "title" || !nodeWithin(n, atom.Svg) {
				continue
			}
			m := chartTitle.FindStringSubmatch(nodeText(n))
			if m == nil {
				continue
			}
			want := map[string]string{}
			if pm := chartPrecision.FindStringSubmatch(m[2]); pm != nil {
				want["Precision"], want["Fired"] = pm[1]+"%", pm[2]
			} else if cm := chartCount.FindStringSubmatch(m[2]); cm != nil {
				col, ok := chartColumns[cm[2]]
				if !ok {
					t.Errorf("%s: a chart draws %q, which no column is known for", p, m[2])
					continue
				}
				want[col] = cm[1]
			}
			for col, v := range want {
				checked++
				if got := cell[m[1]][col]; got != v {
					t.Errorf("%s: the chart draws %q for the week of %s; the table's %s is %q", p, v, m[1], col, got)
				}
			}
		}
		if checked == 0 {
			t.Errorf("%s: no number a chart draws was checked", p)
		}
	}
}

// What a chart's titles say, in English, and the column of the table
// below each measure is in.
var (
	chartTitle     = regexp.MustCompile(`^week of (\d{4}-\d{2}-\d{2}): (.*)$`)
	chartPrecision = regexp.MustCompile(`^(\d+)% of (\d+) firings caught something`)
	chartCount     = regexp.MustCompile(`^(\d+) (.+)$`)
	chartColumns   = map[string]string{
		"caught something": "○ Caught", "caught nothing": "● Nothing", "not judged: no record": "? Not judged",
		"caught": "✓ Caught", "caught only by noisy rules": "◐ Noisy only", "missed": "✗ Missed",
		"from the incidents file or a tool": "From the file", "written in Lapilli": "Written in Lapilli",
		"impact windows to decide": "Impact windows to decide", "edits": "Edits",
	}
)

// TestCSSAccessible: what the stylesheet does for accessibility, as
// docs/design/access.md says: a focus ring for everything, of the accent
// colour TestContrast holds at 3:1; a skip link hidden until it has the
// focus, then shown; tabs not chosen underlined, as links are; and no
// motion, so nothing for prefers-reduced-motion to stop.
func TestCSSAccessible(t *testing.T) {
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	rule := func(sel string) string {
		i := strings.Index(css, "\n"+sel+" {")
		if i < 0 {
			return ""
		}
		j := strings.Index(css[i:], "}")
		return css[i : i+j]
	}
	if r := rule(":focus-visible"); !regexp.MustCompile(`outline: ?[2-9]px solid var\(--accent\)`).MatchString(r) {
		t.Errorf("no focus ring of 2px or more in --accent: %q", r)
	}
	if strings.Contains(css, "outline: none") || strings.Contains(css, "outline: 0") {
		t.Error("an outline taken away")
	}
	skip, shown := rule(".skip"), rule(".skip:focus")
	if !strings.Contains(skip, "top: -") || !regexp.MustCompile(`top: ?\d`).MatchString(shown) {
		t.Errorf("the skip link isn't hidden until focused, then shown: %q, %q", skip, shown)
	}
	if tabs := rule(".tabs a"); tabs == "" || strings.Contains(tabs, "text-decoration: none") ||
		strings.Contains(rule("a"), "text-decoration: none") {
		t.Errorf("tabs not chosen aren't underlined: %q", tabs)
	}
	if regexp.MustCompile(`(?m)^\s*(transition|animation)`).MatchString(css) || strings.Contains(css, "transition:") || strings.Contains(css, "animation:") {
		t.Error("the stylesheet moves things; prefers-reduced-motion should stop it")
	}
}

// a11yProblems are what a page, in a language, gets wrong.
func a11yProblems(doc *html.Node, lang string) []string {
	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	all := allNodes(doc)
	ids := map[string]*html.Node{}
	for _, n := range all {
		if id := nodeAttr(n, "id"); id != "" {
			if ids[id] != nil {
				bad("id %q is given twice", id)
			}
			ids[id] = n
		}
	}
	labelled := map[string]bool{} // ids a label is for
	for _, n := range all {
		if n.DataAtom == atom.Label && nodeAttr(n, "for") != "" {
			labelled[nodeAttr(n, "for")] = true
		}
	}
	// name is what assistive technology reads for an element named by its
	// attributes, or "".
	name := func(n *html.Node) string {
		if v := strings.TrimSpace(nodeAttr(n, "aria-label")); v != "" {
			return v
		}
		var parts []string
		for _, id := range strings.Fields(nodeAttr(n, "aria-labelledby")) {
			if x := ids[id]; x != nil {
				parts = append(parts, nodeText(x))
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}

	// The language, the title, the landmarks, and a way past them.
	var root, body, title, h1, mainEl *html.Node
	count := map[atom.Atom]int{}
	for _, n := range all {
		if n.Type != html.ElementNode {
			continue
		}
		count[n.DataAtom]++
		switch n.DataAtom {
		case atom.Html:
			root = n
		case atom.Body:
			body = n
		case atom.Title:
			if nodeWithin(n, atom.Head) {
				title = n
			}
		case atom.H1:
			h1 = n
		case atom.Main:
			mainEl = n
		}
	}
	if root == nil || nodeAttr(root, "lang") != lang {
		bad("html isn't lang=%q", lang)
	}
	for _, a := range []atom.Atom{atom.Header, atom.Nav, atom.Main, atom.Footer} {
		if count[a] == 0 {
			bad("no %s", a)
		}
	}
	if count[atom.Main] > 1 || count[atom.Header] > 1 || count[atom.Footer] > 1 {
		bad("more than one main, header or footer")
	}
	if count[atom.H1] != 1 {
		bad("%d h1, not one", count[atom.H1])
	}
	if title == nil || h1 == nil || !strings.Contains(squashed(nodeText(title)), squashed(nodeText(h1))) || squashed(nodeText(title)) == squashed(nodeText(h1)) {
		bad("the title doesn't name the page and the site: %q, h1 %q", nodeText(title), nodeText(h1))
	}
	if body != nil {
		first := body.FirstChild
		for first != nil && first.Type != html.ElementNode {
			first = first.NextSibling
		}
		if first == nil || first.DataAtom != atom.A || mainEl == nil || nodeAttr(first, "href") != "#"+nodeAttr(mainEl, "id") || nodeAttr(mainEl, "id") == "" {
			bad("the body doesn't start with a link to main")
		}
	}
	// Each landmark of a kind that comes more than once has a name of its
	// own.
	navs := map[string]bool{}
	for _, n := range all {
		if n.DataAtom == atom.Nav && count[atom.Nav] > 1 {
			if l := name(n); l == "" || navs[l] {
				bad("a nav without a name of its own: %q", l)
			} else {
				navs[l] = true
			}
		}
	}

	// Each language is offered in a nav, named in its own language.
	for _, l := range names {
		if !slices.ContainsFunc(all, func(n *html.Node) bool {
			return nodeAttr(n, "lang") == l.Code && nodeWithin(n, atom.Nav) && nodeText(n) == l.Name
		}) {
			bad("no %s in a nav, in lang=%q", l.Name, l.Code)
		}
	}

	// Headings go down a level at a time.
	level := 0
	for _, n := range all {
		if n.Type == html.ElementNode && len(n.Data) == 2 && n.Data[0] == 'h' && n.Data[1] >= '1' && n.Data[1] <= '6' {
			l := int(n.Data[1] - '0')
			if l > level+1 {
				bad("%s %q after h%d", n.Data, nodeText(n), level)
			}
			level = l
		}
	}

	for _, n := range all {
		if n.Type != html.ElementNode {
			continue
		}
		if v, err := strconv.Atoi(nodeAttr(n, "tabindex")); err == nil && v > 0 {
			bad("tabindex=%d on %s", v, n.Data)
		}
		if l := nodeAttr(n, "lang"); n != root && l != "" && !known(l) {
			bad("lang=%q on %s", l, n.Data)
		}
		switch n.DataAtom {
		case atom.Input, atom.Select, atom.Textarea:
			if n.DataAtom == atom.Input && slices.Contains([]string{"hidden", "submit", "button", "reset", "image"}, nodeAttr(n, "type")) {
				continue
			}
			id := nodeAttr(n, "id")
			if name(n) == "" && !(id != "" && labelled[id]) && !nodeWithin(n, atom.Label) {
				bad("%s name=%q has no label", n.Data, nodeAttr(n, "name"))
			}
		case atom.A, atom.Button:
			if n.DataAtom == atom.A && !hasNodeAttr(n, "href") || hasNodeAttr(n, "hidden") {
				continue // no link, or not shown nor said
			}
			if l := firstOf(name(n), nodeText(n)); !strings.ContainsFunc(l, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
				bad("%s %q has no name in words", n.Data, l)
			}
		case atom.Img:
			if !hasNodeAttr(n, "alt") {
				bad("img %q has no alt", nodeAttr(n, "src"))
			}
		case atom.Svg:
			if nodeAttr(n, "aria-hidden") == "true" {
				continue
			}
			l := name(n)
			for c := n.FirstChild; c != nil && l == ""; c = c.NextSibling {
				if c.Type == html.ElementNode && c.Data == "title" {
					l = nodeText(c)
				}
			}
			if nodeAttr(n, "role") != "img" || l == "" {
				bad("svg %q isn't an image with a name, nor hidden", nodeAttr(n, "class"))
			}
		case atom.Table:
			named := name(n) != ""
			ths := 0
			for _, c := range allNodes(n) {
				switch c.DataAtom {
				case atom.Caption:
					named = true
				case atom.Th:
					ths++
					if nodeAttr(c, "scope") == "" && !named {
						bad("a th %q without a scope, in a table without a caption", nodeText(c))
					}
					if strings.TrimSpace(nodeText(c)) == "" {
						bad("an empty th")
					}
				}
			}
			if ths == 0 {
				bad("a table without th")
			}
		}
	}
	return out
}

func allNodes(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
		out = append(out, allNodes(c)...)
	}
	return out
}

func nodeAttr(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasNodeAttr(n *html.Node, key string) bool {
	return slices.ContainsFunc(n.Attr, func(a html.Attribute) bool { return a.Key == key })
}

// nodeText is what an element says: its text, and its images' alt.
func nodeText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range allNodes(n) {
		switch {
		case c.Type == html.TextNode:
			b.WriteString(c.Data)
		case c.DataAtom == atom.Img:
			b.WriteString(nodeAttr(c, "alt"))
		}
	}
	return strings.TrimSpace(b.String())
}

func nodeWithin(n *html.Node, a atom.Atom) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.DataAtom == a {
			return true
		}
	}
	return false
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func squashed(s string) string { return strings.Join(strings.Fields(s), " ") }

// Text is 4.5:1 or more on each surface it sits on, and what shows a
// control or where the focus is, 3:1, in light and in dark; and each status
// kind (docs/design/ui.md, "Status colours"): its text on its wash, the
// card, the background and the panel, and ink, dim and the accent (a
// notice's words, quiet words and links in it) on its wash, 4.5:1; its
// line, a badge's edge and a notice's left, and a link's underline on its
// wash, 3:1 on the card, the background and its wash; and the incidents
// chart's fills of the kinds, 3:1 on the card.
func TestContrast(t *testing.T) {
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	light, dark := cssTokens(string(b))
	texts := map[string][]string{
		"ink":       {"bg", "card", "panel", "accent-soft", "accent-wash"},
		"dim":       {"bg", "card", "panel", "accent-soft"},
		"accent":    {"bg", "card", "panel", "accent-soft"},
		"better":    {"card"},
		"worse":     {"card"},
		"viz-muted": {"card"},
		"on-accent": {"accent"},
		"on-blue":   {"tint-blue"},
	}
	ui := map[string][]string{
		"accent":   {"bg", "card", "panel"}, // the focus ring
		"line-ui":  {"bg", "card", "panel"}, // a field's edge, a link's underline
		"viz-good": {"card"}, "viz-warn": {"card"}, "viz-bad": {"card"},
	}
	for _, k := range statusKindNames {
		texts[k] = []string{k + "-wash", "bg", "card", "panel"}
		for _, on := range []string{"ink", "dim", "accent"} {
			texts[on] = append(texts[on], k+"-wash")
		}
		ui[k+"-line"] = []string{"bg", "card", k + "-wash"}
		ui["line-ui"] = append(ui["line-ui"], k+"-wash")
	}
	for mode, tokens := range map[string]map[string]string{"light": light, "dark": dark} {
		for least, pairs := range map[float64]map[string][]string{4.5: texts, 3: ui} {
			for fg, bgs := range pairs {
				for _, bg := range bgs {
					r, err := tokenContrast(tokens, fg, bg)
					if err != nil {
						t.Errorf("%s: %v", mode, err)
					} else if r < least {
						t.Errorf("%s: --%s on --%s is %.2f:1, under %.1f:1", mode, fg, bg, r, least)
					}
				}
			}
		}
	}
}

var (
	cssRootRe  = regexp.MustCompile(`(@media \(prefers-color-scheme: dark\) \{\s*)?:root \{([^}]*)\}`)
	cssTokenRe = regexp.MustCompile(`--([\w-]+):\s*([^;]+);`)
)

// statusKindNames are the status kinds, each a token of the stylesheet
// with its -wash and -line.
var statusKindNames = []string{"ok", "warn", "bad", "info", "unknown"}

// cssTokens are the colour tokens of the stylesheet, in light and in dark,
// dark falling back on light; a token that is another, var(--x), is that
// one's colour in the same mode.
func cssTokens(css string) (light, dark map[string]string) {
	light, dark = map[string]string{}, map[string]string{}
	for _, m := range cssRootRe.FindAllStringSubmatch(css, -1) {
		to := light
		if m[1] != "" {
			to = dark
		}
		for _, x := range cssTokenRe.FindAllStringSubmatch(m[2], -1) {
			to[x[1]] = strings.TrimSpace(x[2])
		}
	}
	for k, v := range light {
		if _, ok := dark[k]; !ok {
			dark[k] = v
		}
	}
	for _, m := range []map[string]string{light, dark} {
		for k := range m {
			v := m[k]
			for i := 0; i < 10 && strings.HasPrefix(v, "var(--") && strings.HasSuffix(v, ")"); i++ {
				v = m[strings.TrimSuffix(strings.TrimPrefix(v, "var(--"), ")")]
			}
			m[k] = v
		}
	}
	return light, dark
}

// contrast is WCAG's contrast ratio of a token on another, a colour with
// alpha laid over the one under it.
func tokenContrast(tokens map[string]string, fg, bg string) (float64, error) {
	b, err := cssRGBA(tokens[bg])
	if err != nil {
		return 0, fmt.Errorf("--%s: %v", bg, err)
	}
	f, err := cssRGBA(tokens[fg])
	if err != nil {
		return 0, fmt.Errorf("--%s: %v", fg, err)
	}
	for i := range 3 {
		f[i] = f[i]*f[3] + b[i]*(1-f[3])
	}
	lf, lb := relLuminance(f), relLuminance(b)
	return (max(lf, lb) + 0.05) / (min(lf, lb) + 0.05), nil
}

func cssRGBA(hex string) ([4]float64, error) {
	h := strings.TrimPrefix(hex, "#")
	if len(h) == 6 {
		h += "ff"
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if len(h) != 8 || err != nil {
		return [4]float64{}, fmt.Errorf("not a colour: %q", hex)
	}
	return [4]float64{float64(v>>24&255) / 255, float64(v>>16&255) / 255, float64(v>>8&255) / 255, float64(v&255) / 255}, nil
}

func relLuminance(c [4]float64) float64 {
	lin := func(x float64) float64 {
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c[0]) + 0.7152*lin(c[1]) + 0.0722*lin(c[2])
}

// pageA11y runs a11yProblems over pages that must answer 200.
func pageA11y(t *testing.T, h http.Handler, who string, paths ...string) {
	t.Helper()
	for _, n := range names {
		for _, p := range paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Sec-Fetch-Site", "none") // a page asked for in a browser, as axe does
			r.Header.Set("Accept-Language", n.Code)
			if who != "" {
				r.Header.Set("X-Forwarded-Email", who)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("%s %s: %d", n.Code, p, w.Code)
				continue
			}
			doc, err := html.Parse(strings.NewReader(w.Body.String()))
			if err != nil {
				t.Fatal(err)
			}
			for _, pr := range a11yProblems(doc, n.Code) {
				t.Errorf("%s %s (%s): %s", n.Code, p, who, pr)
			}
		}
	}
}

// TestPagesAccessibleInMoreStates: pages in states TestPagesAccessible
// doesn't render pass the same check: a task with its tracker link,
// read-only, nothing judged yet, and a proposal with changes on, as an
// approver and as an admin sees it.
func TestPagesAccessibleInMoreStates(t *testing.T) {
	t.Run("task with a tracker link", func(t *testing.T) {
		s, _ := a11yServer(t)
		pageA11y(t, s.Handler(), "", "/tasks/2")
	})
	t.Run("read-only", func(t *testing.T) {
		s, _ := a11yServer(t)
		s.cfg.ReadOnly = true
		pageA11y(t, s.Handler(), "", "/", "/rules/Errors", "/tasks/1", "/tasks/2", "/windows/1", "/settings", "/reports", "/proposals/1")
	})
	t.Run("nothing judged yet", func(t *testing.T) {
		s, _ := server(t, nil)
		pageA11y(t, s.Handler(), "", "/", "/outcomes")
	})
	t.Run("changes on", func(t *testing.T) {
		s, _ := changesServer(t, nil)
		if _, err := s.propose(context.Background(), ask{Rule: "Errors", Kind: ledger.KindFor, For: 15 * time.Minute}, ana, false, true); err != nil {
			t.Fatal(err)
		}
		h := s.Handler()
		pageA11y(t, h, cy, "/proposals", "/proposals/1", "/rules/Errors")
		pageA11y(t, h, ana, "/proposals/1")
	})
}

// TestCheckboxOnItsLine: a label holding a checkbox draws it on the first
// line of its words, before them, on their baseline, in any form (the
// stylesheet's rule): as a field's label it stood on a line of its own,
// centred over its words, as "take it out instead" did on a rule's
// proposals. So every checkbox a page draws in a label is the label's
// own, before its words, which the rule asks; test/a11y/axe.mjs measures
// it in a browser (checkbox-line).
func TestCheckboxOnItsLine(t *testing.T) {
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\nlabel:has\(> input\[type=checkbox\]\) \{([^}]*)\}`).FindSubmatch(b)
	if m == nil || !strings.Contains(string(m[1]), "display: flex") || !strings.Contains(string(m[1]), "align-items: baseline") {
		t.Fatalf("no rule draws a checkbox's label on one line: %q", m)
	}
	h := pagesServer(t).Handler()
	seen := 0
	for _, p := range pagePaths {
		_, body, _ := get(t, h, p)
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range allNodes(doc) {
			if n.DataAtom != atom.Input || nodeAttr(n, "type") != "checkbox" || !nodeWithin(n, atom.Label) {
				continue
			}
			seen++
			if n.Parent.DataAtom != atom.Label || strings.TrimSpace(nodeTextBefore(n)) != "" {
				t.Errorf("%s: the checkbox %s isn't its label's own, before its words: %q", p, nodeAttr(n, "name"), nodeText(n.Parent))
			}
		}
	}
	if seen == 0 {
		t.Error("no page drew a checkbox in a label")
	}
}

// nodeTextBefore is what an element's parent says before it.
func nodeTextBefore(n *html.Node) string {
	var b strings.Builder
	for c := n.Parent.FirstChild; c != nil && c != n; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		} else {
			b.WriteString(nodeText(c))
		}
	}
	return b.String()
}

// TestDisabledFieldsLookIt: a field of a form no one may send now (a
// rule's, while one of its proposals waits) is drawn as a disabled button
// is: dim words on the panel, a dashed edge; not as a field to fill. The
// stylesheet says so here; test/a11y/axe.mjs reads each disabled field's
// computed style in a browser (disabled-look), which a more specific rule
// can't pass by.
func TestDisabledFieldsLookIt(t *testing.T) {
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\ninput:disabled, select:disabled, textarea:disabled \{([^}]*)\}`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no rule for a disabled field")
	}
	for _, want := range []string{"color: var(--dim)", "background: var(--panel)", "border: 1px dashed var(--line-ui)"} {
		if !strings.Contains(string(m[1]), want) {
			t.Errorf("a disabled field isn't drawn with %s: %q", want, m[1])
		}
	}
}
