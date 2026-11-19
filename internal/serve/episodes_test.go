package serve

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// Episodes beside firings (docs/design/judging.md, item 2), where they
// show, on nocServer's week: Flap's eight firings a half hour apart, seven
// re-fires, are one episode, white by the impact window one of them caught;
// Noise's twelve, two hours apart, are twelve, none caught, noisy by its
// episodes as by its firings. The summary, the marked kinds (by each
// episode's first firing), the goals, the rules list and its facet, a
// rule's page, the overview, the digest, MCP, the CSV and /metrics.
func TestEpisodesShown(t *testing.T) {
	ctx := context.Background()
	s := nocServer(t)
	declare(t, s, "goals:\n  - {name: few-episodes, measure: episodes.total, at_most: 5}\n"+
		"  - {name: episodes-caught, measure: episodes.precision, at_least: 0.05}\n"+
		"  - {name: sale-episodes, each: sale, measure: episodes.total, at_most: 1}\n")
	h := s.Handler()
	day := s.l.Midnight(ledger.Week(s.l.Day(nocBase))).In(s.cfg.Loc).Format("2006-01-02")

	// The summary's row of the week, and its marked kinds: the sale is on
	// over the first two flaps and a noise, two first firings; the launch
	// over the third flap alone, of no episode's first.
	var sum struct {
		Rows []struct {
			Team     string
			Firings  int
			Episodes *struct {
				Total, Caught, Unjudged int
				Precision               *float64
				NoisyRules              int `json:"noisy_rules"`
			}
			Marked map[string]struct {
				Firings           int
				Episodes          *int
				EpisodesCaught    *int     `json:"episodes_caught"`
				EpisodesPrecision *float64 `json:"episodes_precision"`
			}
			Goals []struct {
				Name, Result, Why string
				Value             *float64
			}
		}
	}
	summary := func() {
		t.Helper()
		code, body, _ := get(t, h, "/api/v1/summary?from="+day+"&to="+day)
		if code != 200 {
			t.Fatalf("summary: %d %s", code, body)
		}
		sum.Rows = nil
		if err := json.Unmarshal([]byte(body), &sum); err != nil || len(sum.Rows) != 1 {
			t.Fatalf("summary: %v %s", err, body)
		}
	}
	summary()
	row := sum.Rows[0]
	if e := row.Episodes; e == nil || e.Total != 13 || e.Caught != 1 || e.Unjudged != 0 || e.NoisyRules != 1 || e.Precision == nil || *e.Precision != 1.0/13 {
		t.Errorf("the week's episodes: %+v", e)
	}
	if m := row.Marked["sale"]; m.Episodes == nil || *m.Episodes != 2 || *m.EpisodesCaught != 1 {
		t.Errorf("the sale's episodes: %+v", m)
	}
	if m := row.Marked["launch"]; m.Firings != 1 || m.Episodes == nil || *m.Episodes != 0 {
		t.Errorf("the launch's: a firing of it, and no episode's first: %+v", m)
	}
	goals := map[string]string{}
	for _, g := range row.Goals {
		goals[g.Name] = fmt.Sprintf("%s %v", g.Result, valueOf(g.Value))
	}
	if goals["few-episodes"] != "missed 13" || goals["episodes-caught"] != fmt.Sprintf("met %v", 1.0/13) {
		t.Errorf("the week's goals on episodes: %v", goals)
	}

	// The rules list: its column, and noisy:episodes.
	code, _, hd := get(t, h, "/rules?q=noisy:episodes")
	if code != http.StatusSeeOther || hd.Get("Location") != "/rules?perf=noisy-episodes" {
		t.Errorf("noisy:episodes: %d %s", code, hd.Get("Location"))
	}
	v := rulesFor(t, s, "weeks=4&perf=noisy-episodes")
	if names := rowNames(v.Rows); !slices.Equal(names, []string{"Noise"}) {
		t.Errorf("noisy by episodes: %v", names)
	}
	all := rulesFor(t, s, "weeks=4")
	for _, r := range all.Rows {
		if want := map[string]int{"Flap": 1, "Noise": 13}[r.Name]; want != 0 && (r.Episodes != want || !r.EpisodesCounted) {
			t.Errorf("%s: %d episodes, want %d", r.Name, r.Episodes, want)
		}
	}
	_, page, _ := get(t, h, "/rules?weeks=4")
	if !strings.Contains(page, `<th scope="col" class="n">Episodes</th>`) {
		t.Error("the rules list has no Episodes column")
	}

	// A rule's page: Flap's one episode, which held its eight firings.
	_, page, _ = get(t, h, "/rules/Flap?tab=firings")
	if !strings.Contains(page, "Episodes: 1, each one problem") || !strings.Contains(page, "of an episode of 8 firings from") {
		t.Errorf("Flap's page doesn't say its episode")
	}

	// MCP: each rule's, and each firing's.
	ts := httptest.NewServer(h)
	defer ts.Close()
	cs := connect(t, ts.URL, "viewer@example.com")
	var rules struct {
		Rules []struct {
			Name          string
			Episodes      *struct{ Total, White int }
			NoisyEpisodes *bool `json:"noisy_episodes"`
		}
	}
	if failed, text := call(t, cs, "rules", map[string]any{"weeks": 4}, &rules); failed {
		t.Fatal(text)
	}
	seen := 0
	for _, r := range rules.Rules {
		switch r.Name {
		case "Noise":
			if r.Episodes == nil || r.Episodes.Total != 13 || r.NoisyEpisodes == nil || !*r.NoisyEpisodes {
				t.Errorf("MCP's Noise: %+v", r)
			}
			seen++
		case "Flap":
			if r.Episodes == nil || r.Episodes.Total != 1 || r.Episodes.White != 1 || r.NoisyEpisodes == nil || *r.NoisyEpisodes {
				t.Errorf("MCP's Flap: %+v", r)
			}
			seen++
		}
	}
	if seen != 2 {
		t.Errorf("MCP's rules: %+v", rules)
	}
	var fired struct {
		Firings []struct {
			Rule    string
			Start   time.Time
			Episode *struct {
				Start   time.Time
				Firings int
			}
		}
	}
	if failed, text := call(t, cs, "firings", map[string]any{"rule": "Flap", "days": 7}, &fired); failed {
		t.Fatal(text)
	}
	if len(fired.Firings) != 8 {
		t.Fatalf("MCP's Flap firings: %d", len(fired.Firings))
	}
	for _, f := range fired.Firings {
		if f.Episode == nil || f.Episode.Firings != 8 || !f.Episode.Start.Equal(nocBase) {
			t.Errorf("MCP's firing at %s: episode %+v", f.Start, f.Episode)
		}
	}

	// The CSV: the episode each is of, at the end.
	from, to := s.l.Midnight(ledger.Week(s.l.Day(nocBase))), s.l.Midnight(ledger.Week(s.l.Day(nocBase))+7)
	span := "from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(to.Format(time.RFC3339))
	_, body, _ := get(t, h, "/api/v1/report?format=csv&"+span)
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || rows[0][13] != "episode" {
		t.Fatalf("the CSV's columns: %v %v", rows[0], err)
	}
	flaps := map[string]bool{}
	for _, r := range rows[1:] {
		if r[0] == "Flap" {
			flaps[r[13]] = true
		}
	}
	if len(flaps) != 1 || flaps[""] {
		t.Errorf("Flap's firings in the CSV are of episodes %v, want one", flaps)
	}

	// /metrics, over the seven days.
	_, metrics, _ := get(t, h, "/metrics")
	for _, want := range []string{`lapilli_rule_episodes{rule="Flap",severity="critical",team="search"} 1`, `lapilli_rule_episodes_caught{rule="Flap",severity="critical",team="search"} 1`,
		`lapilli_rule_episodes{rule="Noise",severity="warning"} 12`} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("/metrics: no %s", want)
		}
	}

	// The digest's line of the noisy rules' episodes.
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.digestReportFor(ctx, from, to, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if line := episodesLine("en", rep); line != "The noisy rules' episodes, one problem each however many alerts: Noise, 12 firings in 12 episodes, 0 caught." {
		t.Errorf("the digest's line: %q", line)
	}

	// The overview's noisiest rules.
	_, page, _ = get(t, h, "/")
	if !strings.Contains(page, `<th scope="col" class="n">Episodes</th>`) {
		t.Error("the overview's noisiest rules have no episodes")
	}

	// Not counted yet, as after an upgrade: none of it is said, and the
	// goals on episodes aren't known; counted again, as before.
	ledgerExec(t, s, "INSERT INTO meta (key, value) VALUES ('episodes_from', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		fmt.Sprint(ledger.Week(s.l.Day(now))+7))
	afreshAll(s)
	summary()
	row = sum.Rows[0]
	if row.Episodes != nil || row.Marked["sale"].Episodes != nil || row.Firings != 20 {
		t.Errorf("not counted: %+v", row)
	}
	for _, g := range row.Goals {
		if g.Name != "sale-episodes" && (g.Result != goalUnknown || g.Why != whyNoEpisodes) {
			t.Errorf("not counted, %s: %s %s", g.Name, g.Result, g.Why)
		}
	}
	_, page, _ = get(t, h, "/rules/Flap")
	if !strings.Contains(page, "Its episodes aren&#39;t counted yet") {
		t.Error("not counted, Flap's page doesn't say so")
	}
	_, metrics, _ = get(t, h, "/metrics")
	if strings.Contains(metrics, "lapilli_rule_episodes{") || !strings.Contains(metrics, "# TYPE lapilli_rule_episodes gauge") {
		t.Error("not counted, /metrics gives episodes")
	}
	_, body, _ = get(t, h, "/api/v1/report?"+span)
	var r verdict.Report
	if err := json.Unmarshal([]byte(body), &r); err != nil || slices.ContainsFunc(r.Rules, func(x verdict.RuleResult) bool { return x.Episodes.Total > 0 }) ||
		slices.ContainsFunc(r.Firings, func(j verdict.Judged) bool { return j.Episode != 0 }) {
		t.Errorf("not counted, the report gives episodes: %v", err)
	}
	if n, err := s.l.CountEpisodes(ctx, o, nil); err != nil || n == 0 {
		t.Fatalf("counted again: %d %v", n, err)
	}
	afreshAll(s)
	summary()
	if e := sum.Rows[0].Episodes; e == nil || e.Total != 13 {
		t.Errorf("counted again: %+v", e)
	}
}

func valueOf(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// afreshAll has every page and summary worked out afresh: a test that
// writes the ledger from outside lapilli, which changes no generation.
func afreshAll(s *Server) {
	s.cache.Lock()
	s.cache.gen = ""
	s.cache.Unlock()
	s.goalRows.Lock()
	s.goalRows.by = nil
	s.goalRows.Unlock()
	s.goalsK.Lock()
	s.goalsK.key = ""
	s.goalsK.Unlock()
}
