package serve

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
)

// readOf is an error of reading a history at address, as a client
// returns it.
func readOf(at string, err error) error {
	return fmt.Errorf("reading ALERTS{alertstate=\"firing\"}: %w", &url.Error{Op: "Post", URL: at + "/api/v1/query_range", Err: err})
}

func x509Unknown() error { return x509.UnknownAuthorityError{} }

const checkWhat = "setting the replay beside what really fired"

// A failure to read the history is said first in plain words, with the
// address as given and the flag to check, by what failed; the error stays
// behind "The error", and the link to Sources stays.
func TestBannerNamesTheSource(t *testing.T) {
	parallel(t)
	const at = "http://127.0.0.1:9"
	refused := readOf(at, &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}})
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"refused", refused, "Nothing answered at <code><bdi>http://127.0.0.1:9</bdi></code>: the connection was refused."},
		{"host", readOf(at, &net.DNSError{Err: "no such host", Name: "prom.invalid", IsNotFound: true}), "The address <code><bdi>http://127.0.0.1:9</bdi></code> couldn&#39;t be found."},
		{"timeout", fmt.Errorf("reading ALERTS: %w", context.DeadlineExceeded), "<code><bdi>http://127.0.0.1:9</bdi></code> didn&#39;t answer in time."},
		{"denied", fmt.Errorf("reading ALERTS: %w", &prom.APIError{Status: 401, Msg: "no"}), "turned Lapilli away (HTTP 401)."},
		{"forbidden", fmt.Errorf("reading ALERTS: %w", &prom.APIError{Status: 403, Msg: "no"}), "turned Lapilli away (HTTP 403)."},
		{"tls", readOf(at, x509Unknown()), "Lapilli couldn&#39;t trust the secure connection to"},
		{"other", fmt.Errorf("reading ALERTS: %w", &prom.APIError{Status: 500, Msg: "boom"}), "Lapilli couldn&#39;t read <code><bdi>http://127.0.0.1:9</bdi></code>."},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := server(t, func(c *Config) { c.Prometheus = "http://user:secret@127.0.0.1:9/?x=1" })
			s.cfg.Prometheus = at
			s.fail(checkWhat, c.err)
			_, body, _ := get(t, s.Handler(), "/")
			for _, want := range []string{c.want, "<code><bdi>--prometheus</bdi></code>", `<details class="why"><summary>The error</summary><p>` + html.EscapeString(checkWhat+": "+c.err.Error()), `href="/sources"`} {
				if !strings.Contains(body, want) {
					t.Errorf("no %q in the banner", want)
				}
			}
			if i := strings.Index(body, "The error</summary>"); i < strings.Index(body, c.want[:20]) {
				t.Error("the error comes before what to check")
			}
		})
	}
	t.Run("credentials", func(t *testing.T) {
		s, _ := server(t, func(c *Config) { c.Prometheus = "http://user:secret@prom.example:9090/?token=abc" })
		s.fail(checkWhat, refused)
		_, body, _ := get(t, s.Handler(), "/")
		if strings.Contains(body, "secret") || strings.Contains(body, "token=abc") {
			t.Error("the address is said with its password or its query")
		}
		if !strings.Contains(body, "http://prom.example:9090/") {
			t.Error("the address is not said")
		}
	})
	t.Run("not a source", func(t *testing.T) {
		s, _ := server(t, func(c *Config) { c.Prometheus = at })
		s.fail("pruning", errors.New("disk full"))
		_, body, _ := get(t, s.Handler(), "/")
		if strings.Contains(body, "The error</summary>") || !strings.Contains(body, "pruning: disk full") {
			t.Error("a failure that isn't a read of a source is said as it was")
		}
	})
	t.Run("in each language", func(t *testing.T) {
		for _, l := range []string{"es", "ja", "ko"} {
			for _, k := range []string{"refused", "host", "timeout", "denied", "tls", "read"} {
				got := faultLead(l, errClass{kind: k, at: at, flag: "--prometheus", code: 401})
				if en := faultLead("en", errClass{kind: k, at: at, flag: "--prometheus", code: 401}); got == en || strings.Contains(string(got), "%!") {
					t.Errorf("%s %s: %q", l, k, got)
				}
			}
		}
	})
}

// replayServer is a server with rules and its backfill run, over the days
// replayed from.
func replayServer(t *testing.T, days int, more string) *Server {
	t.Helper()
	ctx := context.Background()
	s, rules := server(t, func(c *Config) { c.Backfill = time.Duration(days) * 24 * time.Hour })
	if more != "" {
		os.WriteFile(filepath.Join(rules, "more.yaml"), []byte(more), 0o644)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	return s
}

const idleRule = "groups:\n  - name: idle\n    rules:\n      - {alert: Idle, expr: idle > 1}\n"
const idleRules = idleRule + "      - {alert: Idle2, expr: idle2 > 1}\n"

func failRun(t *testing.T, s *Server, failed ...string) {
	t.Helper()
	why := map[string]string{}
	for _, n := range failed {
		why[n] = "reading errors: Post \"http://127.0.0.1:9/api/v1/query_range\": connection refused"
	}
	if err := s.l.AddRun(context.Background(), ledger.Run{Kind: "hourly", Started: now, Finished: now.Add(time.Minute), From: now.Add(-time.Hour), To: now, Failed: why}); err != nil {
		t.Fatal(err)
	}
}

// The overview's line about the last run says when it replayed none, or
// some, of the rules, with where to see why: in the status that says so, bad
// or warn, never as a run that went well.
func TestLedeSaysWhatTheLastRunReplayed(t *testing.T) {
	parallel(t)
	t.Run("none, one rule", func(t *testing.T) {
		s := replayServer(t, 3, "")
		failRun(t, s, "Errors")
		_, body, _ := get(t, s.Handler(), "/")
		if !strings.Contains(body, `<span class="status-bad"><span aria-hidden="true">✕</span> Last run 2026-11-03 12:01 couldn&#39;t replay its one rule: reading errors:`) ||
			!strings.Contains(body, `<a href="/runs">Run history</a>`) {
			t.Errorf("the lede of a run that replayed its one rule not:\n%s", cutOf(body, `<p class="lede">`, "</p>"))
		}
		if strings.Contains(cutOf(body, `<p class="lede">`, "</p>"), "Last run 2026-11-03 12:01.") {
			t.Error("a run that replayed nothing is said to have run")
		}
	})
	t.Run("none, several", func(t *testing.T) {
		s := replayServer(t, 3, idleRules)
		failRun(t, s, "Errors", "Idle", "Idle2")
		_, body, _ := get(t, s.Handler(), "/")
		if !strings.Contains(body, `<span class="status-bad"><span aria-hidden="true">✕</span> Last run 2026-11-03 12:01 replayed none of the 3 rules: reading errors:`) {
			t.Errorf("the lede:\n%s", cutOf(body, `<p class="lede">`, "</p>"))
		}
	})
	t.Run("some", func(t *testing.T) {
		s := replayServer(t, 3, idleRules)
		failRun(t, s, "Idle")
		_, body, _ := get(t, s.Handler(), "/")
		if !strings.Contains(body, `<span class="status-warn"><span aria-hidden="true">⚠&#xFE0E;</span> Last run 2026-11-03 12:01: 1 of 3 rules couldn&#39;t be replayed.</span> <a href="/runs">Run history</a>`) {
			t.Errorf("the lede:\n%s", cutOf(body, `<p class="lede">`, "</p>"))
		}
	})
	t.Run("all replayed", func(t *testing.T) {
		s := replayServer(t, 3, "")
		_, body, _ := get(t, s.Handler(), "/")
		lede := cutOf(body, `<p class="lede">`, "</p>")
		if strings.Contains(lede, "replay") && strings.Contains(lede, "couldn") || !strings.Contains(lede, "Last run ") {
			t.Errorf("the lede of a run that went well:\n%s", lede)
		}
	})
}

// between is the text of body from start to the next end.
func cutOf(body, start, end string) string {
	i := strings.Index(body, start)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], end)
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j+len(end)]
}

// A week no run replayed is not zero: its tiles are "–", one notice says
// from when Lapilli replays, the charts mark it and the table tags it,
// each with no number of it.
func TestWeekNoRunReplayedIsNotZero(t *testing.T) {
	parallel(t)
	t.Run("before", func(t *testing.T) {
		s := replayServer(t, 1, "") // from 2026-11-02 12:00; the last whole week ended at 00:00
		// Firings copied in from before it, in the week of 2026-10-12: the
		// weeks after it, with none, are not known either.
		at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
		if err := s.l.Record(context.Background(), ledger.Found{From: at.Add(-time.Hour), To: at.Add(time.Hour),
			Firings: []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at, End: at.Add(10 * time.Minute)}}}, s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		_, body, _ := get(t, s.Handler(), "/")
		notice := "Lapilli replays from 2026-11-02: the week of 2026-10-26 was before it, so nothing in it is known."
		if strings.Count(body, notice) != 1 {
			t.Fatalf("the notice, once: %d", strings.Count(body, notice))
		}
		if strings.Index(body, notice) > strings.Index(body, `<section class="tiles`) {
			t.Error("the notice comes after the tiles")
		}
		tiles := cutOf(body, `<section class="tiles`, "</section>")
		if n := strings.Count(tiles, `<div class="tile-value">–</div>`); n != strings.Count(tiles, `class="tile-value"`) || n < 6 {
			t.Errorf("%d tiles of %d say –", n, strings.Count(tiles, `class="tile-value"`))
		}
		if strings.Contains(tiles, `<div class="tile-value">0</div>`) {
			t.Error("a tile says 0 of a week that wasn't replayed")
		}
		if !strings.Contains(body, "<title>not replayed</title>–</text>") {
			t.Error("a chart doesn't mark a week not replayed")
		}
		row := cutOf(body, `<td class="t"><span class="d">2026-10-19</span>`, "</tr>")
		if !strings.Contains(row, `<span class="tag status-unknown">not replayed</span>`) || !strings.Contains(row, `>–</td>`) || regexp.MustCompile(`<td class="n">0</td>`).MatchString(row) {
			t.Errorf("the table's row of a week not replayed: %s", row)
		}
		// The unknown kind, with its sign, never a verdict.
		if i := strings.Index(body, notice); !strings.Contains(body[i-150:i], `status-unknown`) {
			t.Error("the notice isn't of the unknown kind")
		}
	})
	t.Run("in part", func(t *testing.T) {
		s := replayServer(t, 3, "") // from 2026-10-31 12:00, in the last whole week
		_, body, _ := get(t, s.Handler(), "/")
		if !strings.Contains(body, "Lapilli replays from 2026-10-31 12:00: of the week of 2026-10-26, only what came after is known.") {
			t.Error("a week replayed in part says so")
		}
		if strings.Contains(cutOf(body, `<section class="tiles`, "</section>"), "not replayed") {
			t.Error("a week replayed in part is not replayed")
		}
	})
	t.Run("none", func(t *testing.T) {
		ctx := context.Background()
		s, _ := server(t, nil)
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		// A ledger some run has moved on, and none replayed.
		if err := s.l.Record(ctx, ledger.Found{From: now.Add(-time.Hour), To: now}, s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		_, body, _ := get(t, s.Handler(), "/")
		if !strings.Contains(body, "No run has replayed any rule yet: nothing in the week of 2026-10-26 is known.") {
			t.Error("no notice of a ledger no run replayed")
		}
	})
}

// "Never fired" is said over the time replayed: since the day replayed
// from when that is later than the twelve weeks begin, in singular for one,
// and not at all with under four weeks of it, or when the latest run
// replayed none of the rules, or for a rule it couldn't replay.
func TestNeverFiredOverTheTimeReplayed(t *testing.T) {
	parallel(t)
	look := func(t *testing.T, s *Server) string {
		t.Helper()
		_, body, _ := get(t, s.Handler(), "/")
		return cutOf(body, `id="look-h"`, "</section>")
	}
	t.Run("since", func(t *testing.T) {
		s := replayServer(t, 42, idleRule)
		if got := look(t, s); !strings.Contains(got, "1 rule never fired since 2026-09-22") || strings.Contains(got, "twelve weeks") {
			t.Errorf("one rule, since:\n%s", got)
		}
		_, rules, _ := get(t, s.Handler(), "/rules?weeks=12")
		if !strings.Contains(rules, "1 rule never fired since 2026-09-22") {
			t.Error("the rules' list doesn't follow the span")
		}
	})
	t.Run("several", func(t *testing.T) {
		s := replayServer(t, 42, idleRules)
		if got := look(t, s); !strings.Contains(got, "2 rules never fired since 2026-09-22") {
			t.Errorf("two rules, since:\n%s", got)
		}
	})
	t.Run("twelve weeks", func(t *testing.T) {
		s := replayServer(t, 100, idleRule)
		if got := look(t, s); !strings.Contains(got, "1 rule never fired in twelve weeks") {
			t.Errorf("one rule, twelve weeks:\n%s", got)
		}
		_, rules, _ := get(t, s.Handler(), "/rules?weeks=12")
		if !strings.Contains(rules, "1 rule never fired in the last twelve weeks") {
			t.Error("the rules' list says 1 rules")
		}
	})
	t.Run("too short", func(t *testing.T) {
		s := replayServer(t, 20, idleRule)
		if got := look(t, s); strings.Contains(got, "never fired") {
			t.Errorf("under four weeks:\n%s", got)
		}
	})
	t.Run("none replayed", func(t *testing.T) {
		s := replayServer(t, 42, idleRule)
		failRun(t, s, "Errors", "Idle")
		if got := look(t, s); strings.Contains(got, "never fired") {
			t.Errorf("the latest run replayed none:\n%s", got)
		}
	})
	t.Run("one failed", func(t *testing.T) {
		s := replayServer(t, 42, idleRules)
		failRun(t, s, "Idle")
		if got := look(t, s); !strings.Contains(got, "1 rule never fired since") {
			t.Errorf("a rule the run couldn't replay is counted:\n%s", got)
		}
	})
}

// dashOf is the overview as the page is made of it.
func dashOf(t *testing.T, s *Server) dashboard {
	t.Helper()
	d, err := s.overview(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	return d.(dashboard)
}

// The overview's hierarchy: a headline of the week in a sentence, four
// tiles then the rest, the level of a precision under the noisy line, What
// to look at by rule, a selector in code, and no hole in the cards.
func TestOverviewHierarchy(t *testing.T) {
	s := demoServer(t)
	h := s.Handler()
	d := dashOf(t, s)
	_, body, _ := get(t, h, "/")
	last := d.Weeks[1]
	t.Run("headline", func(t *testing.T) {
		// The noisy rules' firings, as the rules list reads them.
		rl := viewOf(t, s, "rules", "/rules?perf=noisy&week=2026-10-26", s.rulesList).(rulesList)
		noisy, fired := rl.Shown, rl.Sum.Firings
		if noisy == 0 || noisy != last.Noisy || fired != last.NoisyFirings {
			t.Fatalf("the noisy rules: %d firing %d, the week has %d firing %d", noisy, fired, last.Noisy, last.NoisyFirings)
		}
		want := fmt.Sprintf("<strong>%d</strong> noisy rules made <strong>%d</strong> of the week&#39;s <strong>%d</strong> firings", noisy, fired, last.Firings)
		if !strings.Contains(body, `<p class="headline">`+strings.ReplaceAll(want, "&#39;", "'")) && !strings.Contains(body, want) {
			t.Errorf("no headline %q:\n%s", want, cutOf(body, `<p class="headline">`, "</p>"))
		}
		if last.Missed > 0 && !strings.Contains(body, fmt.Sprintf("; <strong>%d</strong> incident", last.Missed)) {
			t.Errorf("the headline leaves the incidents missed out: %s", cutOf(body, `<p class="headline">`, "</p>"))
		}
		if strings.Contains(cutOf(body, `<p class="headline">`, "</p>"), "status-") {
			t.Error("the headline is coloured")
		}
	})
	t.Run("singular", func(t *testing.T) {
		one := &weekView{Firings: 9, Noisy: 1, NoisyFirings: 7, Missed: 0}
		if got := string(headline("en", one)); got != "<strong>1</strong> noisy rule made <strong>7</strong> of the week's <strong>9</strong> firings." {
			t.Error(got)
		}
		one.Missed = 1
		if got := string(headline("en", one)); !strings.HasSuffix(got, "; <strong>1</strong> incident missed.") {
			t.Error(got)
		}
		one.Missed = 3
		if got := string(headline("en", one)); !strings.HasSuffix(got, "; <strong>3</strong> incidents missed.") {
			t.Error(got)
		}
	})
	t.Run("tiers", func(t *testing.T) {
		first, rest := cutOf(body, `<div class="tier tier-first">`, "</section>"), ""
		if i := strings.Index(first, `<div class="tier tier-rest">`); i > 0 {
			first, rest = first[:i], first[i:]
		}
		if n := strings.Count(first, `class="tile"`); n != 4 {
			t.Errorf("%d tiles lead", n)
		}
		for _, l := range []string{"Precision", "Incidents missed", "Noisy rules"} {
			if !strings.Contains(first, `<div class="tile-label">`+l+`</div>`) {
				t.Errorf("%s doesn't lead", l)
			}
		}
		if len(d.TilesFirst)+len(d.TilesRest) != len(d.Tiles) || strings.Count(rest, `class="tile"`) != len(d.TilesRest) || len(d.TilesRest) == 0 {
			t.Errorf("%d + %d tiles of %d", len(d.TilesFirst), len(d.TilesRest), len(d.Tiles))
		}
		for _, x := range d.TilesRest {
			if x.Href == "" && drillsOf[x.key] != [2]string{} {
				t.Errorf("%s has lost its link", x.key)
			}
		}
	})
	t.Run("precision level", func(t *testing.T) {
		p, ok := last.precision()
		if !ok || p >= noisyAt(s.cfg.Options).Precision {
			t.Fatalf("the demo's precision is %v, not under the line", p)
		}
		if !strings.Contains(body, `<div class="tile-under status-warn"><span aria-hidden="true">⚠&#xFE0E;</span> under the noisy line, `+percentOf(noisyAt(s.cfg.Options).Precision)+`</div>`) {
			t.Error("the precision tile doesn't say it is under the noisy line")
		}
	})
	t.Run("by rule", func(t *testing.T) {
		look := cutOf(body, `id="look-h"`, "</section>")
		items := regexp.MustCompile(`(?s)<ol class="actions">(.*?)</ol>`).FindStringSubmatch(look)
		if items == nil {
			t.Fatal("no list")
		}
		lis := strings.Split(items[1], `<li><span class="look-what`)
		grouped := 0
		seen := map[string]bool{}
		for _, li := range lis[1:] {
			if strings.Contains(li, "look-rule") {
				grouped++
				name := regexp.MustCompile(`<a href="/rules/([^"?]+)`).FindStringSubmatch(li)
				if name == nil || seen[name[1]] {
					t.Errorf("an item of a rule shown twice or with no link: %v", name)
				} else {
					seen[name[1]] = true
				}
				if n := strings.Count(li, "<form"); n > 1 {
					t.Errorf("%d task controls in one item", n)
				}
				if strings.Count(li, `<li>`) < 2 {
					t.Errorf("an item of a rule with one line: %s", li)
				}
			}
		}
		if grouped == 0 {
			t.Fatalf("no rule has two things to look at in the demo:\n%s", look)
		}
		for _, a := range d.Actions {
			if a.Rule != "" && seen[a.Rule] && !strings.Contains(look, html.EscapeString(a.Text)) {
				t.Errorf("a line of %s is gone: %q", a.Rule, a.Text)
			}
		}
		if n := strings.Count(look, "<li><span class=\"look-what"); n > actionsShown || len(d.Items) > actionsShown {
			t.Errorf("%d items", n)
		}
	})
	t.Run("selector in code", func(t *testing.T) {
		if !regexp.MustCompile(`held by <code><bdi>rule: \w+</bdi></code>, in`).MatchString(body) {
			t.Errorf("the selector an expectation is held by isn't in code:\n%s", cutOf(body, "held by", "</span>"))
		}
	})
}

// Grouping: actions of one rule are one item at the place of its first; those
// with no rule stay each their own.
func TestGroupActions(t *testing.T) {
	as := []action{{Text: "a", Rule: "R1"}, {Text: "i"}, {Text: "b", Rule: "R2"}, {Text: "c", Rule: "R1"}, {Text: "g", Goal: "G", Rule: "R1"}, {Text: "d", Rule: "R1"}}
	got := groupActions(as)
	if len(got) != 4 || got[0].Rule != "R1" || len(got[0].Lines) != 3 || got[1].Rule != "" || got[2].Rule != "" || got[3].Lines[0].Text != "g" {
		t.Errorf("%+v", got)
	}
	// One task control to an item: the first that may; the rest wait.
	as = []action{{Rule: "R", Kind: "tune", May: true}, {Rule: "R", Kind: "retire", May: true}, {Rule: "R", Kind: "tune", Task: 7}}
	it := groupActions(as)[0]
	if f := it.Form(); f == nil || f.Kind != "tune" || it.Off() != nil {
		t.Errorf("%+v %+v", f, it.Off())
	}
	it = groupActions([]action{{Rule: "R", Kind: "tune", Why: "Editors only"}, {Rule: "R", Kind: "retire", Why: "Editors only"}})[0]
	if it.Form() != nil || it.Off() == nil || it.Off().Kind != "tune" {
		t.Errorf("%+v", it.Off())
	}
}

// An odd number of cards leaves no hole: the last is as wide as the grid.
func TestCardsLeaveNoHole(t *testing.T) {
	css, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".cards > .card:last-child:nth-child(odd) { grid-column: 1 / -1; }") {
		t.Error("the last of an odd number of cards doesn't span the grid")
	}
}
