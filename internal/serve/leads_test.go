package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// A lead of a signal's or a rule entry's own, and require: on an entry
// (docs/design/judging.md, item 1), in lapilli serve.

// leadsServer reads errors and probe, the probe started with a lead of
// 45m: an errors window over checkout 40 minutes after Errors' checkout
// firing (06:02 on Monday), and a probe window over search 30 minutes
// after its first search firing (10:02 on Sunday).
func leadsServer(t *testing.T) *Server {
	t.Helper()
	h := shopHistory()
	errorsAt, probeAt := time.Date(2026, 11, 2, 6, 42, 0, 0, time.UTC), time.Date(2026, 11, 1, 10, 32, 0, 0, time.UTC)
	h.windows = map[string][]prom.Series{
		"http_requests_total > 1e9": {minutes(map[string]string{"service": "checkout"}, errorsAt, errorsAt.Add(10*time.Minute))},
		"probe_success == 0":        {minutes(map[string]string{"service": "search", "instance": "https://search.example.com"}, probeAt, probeAt.Add(8*time.Minute))},
	}
	return signalServer(t, h, func(c *Config) {
		lead := 45 * time.Minute
		c.Signals[1].Lead = &lead
	})
}

// leadsReport is the ledger's report of every firing kept, and what
// verdict.Judge gives the same firings and incidents with the same options:
// a firing each, its rule, labels, start and what it caught.
func leadsReport(t *testing.T, s *Server) (ledgerSays, judgeSays []string) {
	t.Helper()
	from, to := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), now.Add(time.Hour)
	code, body, _ := get(t, s.Handler(), "/api/v1/report?from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339))
	if code != http.StatusOK {
		t.Fatalf("report: %d %s", code, body)
	}
	var rep verdict.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatal(err)
	}
	o, err := s.options(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o.From, o.To = from, to
	var fs []firing.Firing
	for _, f := range rep.Firings {
		fs = append(fs, f.Firing)
	}
	var incs []incident.Incident
	for _, in := range rep.Incidents {
		incs = append(incs, in.Incident)
	}
	say := func(js []verdict.Judged) []string {
		var out []string
		for _, j := range js {
			out = append(out, fmt.Sprintf("%s %s %s %v", j.Rule, j.Labels["service"], j.Start.UTC().Format("01-02 15:04"), j.Incidents))
		}
		slices.Sort(out)
		return out
	}
	return say(rep.Firings), say(verdict.Judge(nil, fs, incs, o).Firings)
}

// caught is, of stones as leadsReport says them, those that caught
// something.
func caught(stones []string) []string {
	return slices.DeleteFunc(slices.Clone(stones), func(s string) bool { return strings.HasSuffix(s, "[]") })
}

// Started with a signal's lead, and given an entry's on the Settings page,
// serve's ledger judges each firing as lapilli verdict's verdict.Judge does
// with the same options: the probe's 45m catches a firing 30 minutes before
// its window, --lead not one 40 minutes before errors'; the entry's 1h
// both. The summary, /settings.yaml and the page say each lead.
func TestLeadsServe(t *testing.T) {
	s := leadsServer(t)
	got, want := leadsReport(t, s)
	if !slices.Equal(got, want) {
		t.Fatalf("serve and verdict differ:\n serve %q\nverdict %q", got, want)
	}
	if c := caught(got); len(c) != 1 || !strings.HasPrefix(c[0], "Errors search 11-01 10:02 [impact-probe-") {
		t.Errorf("caught by the probe's lead alone: %q", c)
	}
	// An entry with a lead, saved on the Settings page: judged again.
	text := "- {rules: ^Errors$, signals: [errors, probe], lead: 1h}\n"
	if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {text}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(context.Background(), true)
	got, want = leadsReport(t, s)
	if !slices.Equal(got, want) {
		t.Fatalf("serve and verdict differ with the entry's lead:\n serve %q\nverdict %q", got, want)
	}
	if c := caught(got); len(c) != 2 || !strings.HasPrefix(c[0], "Errors checkout 11-02 06:02 [impact-errors-") {
		t.Errorf("caught by the entry's lead: %q", c)
	}
	var sum summary
	if code, body, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-11-02&to=2026-11-02"); code != http.StatusOK || json.Unmarshal([]byte(body), &sum) != nil {
		t.Fatalf("summary: %d %s", code, body)
	}
	if ss, es := sum.Settings.ImpactSignals, sum.Settings.ImpactRules; len(ss) != 2 || ss[0].Lead != "" || ss[1].Lead != "45m" || len(es) != 1 || es[0].Lead != "1h" || sum.Settings.Lead != "15m" {
		t.Errorf("the summary's settings: %+v %+v", ss, es)
	}
	_, y, _ := get(t, s.Handler(), "/settings.yaml")
	if !strings.Contains(y, "lead: 45m") || !strings.Contains(y, "lead: 1h") {
		t.Errorf("settings.yaml:\n%s", y)
	}
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(page, `name="lead" value="45m"`) || !strings.Contains(page, `placeholder="15m, the lead above"`) || !strings.Contains(page, "lead 1h") {
		t.Error("the Settings page says no lead")
	}
}

// A lead on the Settings page is from 0 to a day; empty follows the lead
// above, and is kept as nothing.
func TestLeadsSetInUI(t *testing.T) {
	s := signalServer(t, shopHistory(), nil)
	post := func(lead string) (int, string) {
		v := signalPost("probe", "probe", "probe_success == 0", "2m", "service", true)
		v.Set("lead", lead)
		return adminPost(t, s, "/settings/signals", v, "ana@example.com")
	}
	for _, bad := range []string{"24h1s", "2d", "-1m", "soon"} {
		if code, body := post(bad); code != http.StatusBadRequest || !strings.Contains(body, "want a duration from 0 to 24h") {
			t.Errorf("lead %s: %d", bad, code)
		}
	}
	if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {"- {rules: ^Errors$, signals: [probe], lead: 25h}\n"}}, "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "rule entry 1") {
		t.Errorf("an entry's lead of 25h: %d", code)
	}
	if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {"- {rules: ^Errors$, signals: [probe], require: [9x]}\n"}}, "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "require") {
		t.Errorf("an entry requiring a name that isn't a label's: %d", code)
	}
	for _, c := range []struct {
		lead string
		want *time.Duration
	}{{"24h", dur(24 * time.Hour)}, {"0", dur(0)}, {"", nil}} {
		if code, body := post(c.lead); code != http.StatusSeeOther {
			t.Fatalf("lead %q: %d %s", c.lead, code, body)
		}
		d, ok := inEffect(t, s).def("probe")
		if r, _ := signalRow(t, s, "probe"); !ok || !sameLeadOf(d.Lead, c.want) {
			t.Errorf("lead %q: in effect %v, kept %v", c.lead, d.Lead, r.Lead)
		}
	}
	set, _ := s.l.Settings(context.Background())
	if v := set["impact-signal/probe"].Value; strings.Contains(v, "lead") {
		t.Errorf("a lead left empty, kept as %s", v)
	}
}

func dur(d time.Duration) *time.Duration { return &d }

func sameLeadOf(a, b *time.Duration) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// What a week's goals and the digest wait for past its end: the longest
// lead and a signal's for, and --settle after that; with the defaults and
// an impact signal, an hour and 20 minutes; with written incidents alone,
// --lead and --settle.
func TestWholeAfter(t *testing.T) {
	s := signalServer(t, shopHistory(), nil)
	o := verdict.Options{Lead: 15 * time.Minute}
	if got := s.wholeAfter(o, nil); got != time.Hour+15*time.Minute {
		t.Errorf("written incidents alone: %s", got)
	}
	s.cfg.Impact, s.cfg.ImpactFor = "impact", 5*time.Minute
	s.cfg.Signals = nil
	if got := s.wholeAfter(o, nil); got != time.Hour+20*time.Minute {
		t.Errorf("the defaults, an impact signal with no name: %s", got)
	}
	o.Signals = verdict.NewSignals([]verdict.Signal{{Name: "errors", For: 5 * time.Minute}, {Name: "probe", For: 2 * time.Minute, Lead: dur(45 * time.Minute)}},
		[]verdict.RuleEntry{{Rules: "^Mem", Signals: []string{"errors"}, Lead: dur(time.Hour)}})
	if got := s.wholeAfter(o, nil); got != 2*time.Hour+5*time.Minute {
		t.Errorf("an entry's 1h against errors, for 5m: %s", got)
	}
}

// A whole week's goals stay open until the runs are past its end by what
// it waits for (wholeAfter), though the summary holds the week whole:
// goal.missed, the goals' tasks and their /metrics follow its results.
func TestGoalsWaitForTheReach(t *testing.T) {
	s := goalServer(t, nil)
	declare(t, s, theGoals)
	// The runs are 35 hours past the week of 10-26's end: past the wait
	// of --lead and --settle.
	if g := goalIn(t, summaryOfGoals(t, s), "2026-10-26", "", "none-missed"); g.Result != "missed" {
		t.Fatalf("the last whole week: %+v", g)
	}
	// Asked again by another span, so that no page kept answers it.
	s.cfg.Settle = 36 * time.Hour
	code, body, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-10-19&to=2026-11-02&team=*")
	var sum summary
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sum) != nil {
		t.Fatalf("%d %s", code, body)
	}
	if g := goalIn(t, sum, "2026-10-26", "", "none-missed"); g.Result != "open" {
		t.Errorf("within --lead and --settle of the week's end: %+v", g)
	}
	for _, r := range sum.Rows {
		if r.From.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)) && r.Partial {
			t.Error("the week is partial: only its goals wait")
		}
	}
}
