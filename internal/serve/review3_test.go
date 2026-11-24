package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// TestTryComparesLikeWithLike: the try page, given the settings in effect,
// changes nothing: what it replays is judged by the weeks the ledger's
// report is, though the seven days are of two weeks, one with no record.
func TestTryComparesLikeWithLike(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := noSource(t, nil)
	o, err := s.options(ctx)
	if err != nil || o.RecordWeek == nil {
		t.Fatalf("judged by week: %v", err)
	}
	v, err := s.try(ctx, o, o)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Same {
		for _, rc := range v.Rules {
			t.Errorf("%s: %+v → %+v", rc.Rule, rc.Before, rc.After)
		}
		t.Errorf("incidents %+v", v.Incs)
	}
}

// TestReplayJudgedAsKept: a span's firings judged again as the digest's
// replays are (replayInput), whole weeks or not, say what the ledger's
// report of it says, rule by rule: by the same weeks, given the incidents
// that keep them.
func TestReplayJudgedAsKept(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := noSource(t, nil)
	o, _ := s.options(ctx)
	to := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	for _, from := range []time.Time{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)} {
		rep, err := s.l.Report(ctx, from, to, o, false)
		if err != nil {
			t.Fatal(err)
		}
		in, err := s.replayInput(ctx, rep, from, to, o)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
		if err != nil {
			t.Fatal(err)
		}
		var plain []firing.Firing
		for _, f := range fs {
			if !f.Excused {
				plain = append(plain, f.Firing)
			}
		}
		again := verdict.Judge(nil, plain, in.incs, in.o)
		if again.NoRecord != rep.NoRecord || again.Unjudged != rep.Unjudged {
			t.Errorf("from %s: no record %v, %d unjudged; kept %v, %d", from.Format("Jan 2 15h"), again.NoRecord, again.Unjudged, rep.NoRecord, rep.Unjudged)
		}
		for _, want := range rep.Rules {
			for _, got := range again.Rules {
				if got.Rule == want.Rule && (got.Unjudged != want.Unjudged || got.Noisy != want.Noisy) {
					t.Errorf("from %s, %s: judged again %d unjudged, noisy %v; kept %d, %v", from.Format("Jan 2 15h"), want.Rule, got.Unjudged, got.Noisy, want.Unjudged, want.Noisy)
				}
			}
		}
	}
}

// TestPrecisionOfJudged: the week before has no record, this week has. The
// rules list's sum, MCP's rule, the deploys line, a rule's span before its
// period and the outcomes' then and now are of the firings judged alone.
func TestPrecisionOfJudged(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := noSource(t, nil)
	v := rulesFor(t, s, "weeks=4")
	judged := v.Sum.Firings - v.Sum.Unjudged
	if v.Sum.Unjudged == 0 || v.Sum.Precision != pct(v.Sum.White, judged) {
		t.Errorf("rules list's sum: %+v", v.Sum)
	}
	_, out, err := s.toolRules(ctx, nil, rulesIn{Weeks: 4})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Rules {
		if r.Name == "Errors" && (r.Precision == nil || *r.Precision != 1.0/7) {
			t.Errorf("MCP Errors' precision: %v", r.Precision)
		}
	}
	o, _ := s.options(ctx)
	from, to := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	rep, err := s.l.Report(ctx, from, to, o, false)
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.refiresOf(ctx, rep, from, to)
	if err != nil {
		t.Fatal(err)
	}
	white, n := 0, 0
	for _, r := range rep.Rules {
		white, n = white+r.White, n+r.Firings-r.Unjudged
	}
	if rep.Unjudged == 0 || x.Precision == nil || *x.Precision != float64(white)/float64(n) {
		t.Errorf("refires' precision %v, want %d of %d", x.Precision, white, n)
	}
	// Of the span before: none judged, no precision to set beside.
	if p, _, _ := besides(ruleLine{Firings: 7, White: 1}, ledger.Span{Firings: 14, Unjudged: 14}); p.On {
		t.Errorf("a span before with none judged: %+v", p)
	}
	if p, _, _ := besides(ruleLine{Firings: 7, White: 1, Unjudged: 2}, ledger.Span{Firings: 14, White: 2, Unjudged: 10}); !p.On || p.By != 20-50 {
		t.Errorf("of those judged: %+v", p)
	}
	// The outcomes.
	tiles := thenNow("en", period{Firings: 20, White: 2, Unjudged: 16}, period{Firings: 10, White: 5})
	if tiles[0].Value != "50%" || !strings.Contains(tiles[0].Of, "50%") {
		t.Errorf("outcomes' precision: %+v", tiles[0])
	}
	if w := (weekView{Firings: 4, White: 1, NoRecord: true}); w.Without() != "–" {
		t.Errorf("a week with no record, without deploys: %s", w.Without())
	}
}

// TestTeamWithNoLabels: a team that owns its rules by name alone owns no
// incident by labels, only those its rules caught: its pages and digest
// are judged by everyone's record, which INC-0 keeps (TestTeamRecord has a
// team with labels, judged by what it owns).
func TestTeamWithNoLabels(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := noSource(t, keepRecord)
	team := ledger.Team{Name: "payments", Rules: "^Errors$"}
	id, err := s.l.PutTeam(ctx, team, "ana")
	if err != nil {
		t.Fatal(err)
	}
	team.ID = id
	row := func(q string) ruleRow {
		t.Helper()
		for _, r := range rulesFor(t, s, q).Rows {
			if r.Name == "Errors" {
				return r
			}
		}
		t.Fatalf("no Errors in %s", q)
		return ruleRow{}
	}
	all, mine := row("weeks=12"), row("weeks=12&team=payments")
	if !mine.Noisy || mine.Unjudged != 0 || mine.Unjudged != all.Unjudged || mine.Precision != all.Precision {
		t.Errorf("payments' Errors %+v, everyone's %+v", mine.ruleLine, all.ruleLine)
	}
	o, _ := s.options(ctx)
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	rep, err := s.digestReportFor(ctx, week, week.Add(7*24*time.Hour), o, &team)
	if err != nil || rep.NoRecord {
		t.Errorf("payments' week: %v %v", rep.NoRecord, err)
	}
	if kept, err := s.spanKept(ctx, week, week.Add(7*24*time.Hour), &team); err != nil || !kept {
		t.Errorf("payments' week kept: %v %v", kept, err)
	}
}

// TestHeatPicksJudged: where the noise is shows the rules with the most
// black stones judged, though others had more, with no record.
func TestHeatPicksJudged(t *testing.T) {
	parallel(t)
	s := noSource(t, nil)
	w1, w2 := int64(20387), int64(20394)
	tl := &tallies{weeks: []int64{w1, w2}, kept: map[int64]bool{w1: false, w2: true}, rules: map[int64]ledger.RuleRow{},
		by: map[int64]map[int64]ledger.Tally{w1: {}, w2: {}}}
	var lines []ruleLine
	// Twenty rules with many firings in the week with no record, and few
	// in the one with; one, Judged, with more in the one with.
	for i := range 20 {
		id := int64(i + 1)
		name := fmt.Sprintf("R%02d", i)
		tl.rules[id] = ledger.RuleRow{Rule: ledger.Rule{Name: name}}
		tl.by[w1][id], tl.by[w2][id] = ledger.Tally{Firings: 100}, ledger.Tally{Firings: 1}
		lines = append(lines, ruleLine{Name: name, Firings: 101, Black: 101, Unjudged: 100})
	}
	tl.rules[99] = ledger.RuleRow{Rule: ledger.Rule{Name: "Judged"}}
	tl.by[w2][99] = ledger.Tally{Firings: 9}
	lines = append(lines, ruleLine{Name: "Judged", Firings: 9, Black: 9})
	rows, _, left := s.heat("en", tl, lines)
	if len(rows) != heatRows || rows[0].Name != "Judged" || rows[0].Black != 9 || !left {
		t.Errorf("%d rows, first %+v, left %v", len(rows), rows[0], left)
	}
}

// TestFiredOnSaysWhatWasntRead: past the alerts read, a rule's firings
// aren't counted by team, and its result says how many weren't read.
func TestFiredOnSaysWhatWasntRead(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Backfill = 10 * 24 * time.Hour; c.Impact = "" })
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "web", Labels: incident.Labels{"service": {"search"}}}, "ana"); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	was := firedOnRead
	firedOnRead = 1
	defer func() { firedOnRead = was }()
	// This week Errors fired on search six times, on checkout once: the
	// one alert read is search's.
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	on, unread, err := s.firedOnOf(ctx, from, from.Add(7*24*time.Hour))
	if err != nil || unread != 1 || len(on) != 1 || on[0] != (firedOn{Rule: "Errors", Team: "web", Firings: 6, Of: 7, Unread: 1}) {
		t.Fatalf("%+v %d %v", on, unread, err)
	}
	// Out of time: an error, which the digest logs and keeps nothing of.
	done, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.firedOnOf(done, from, from.Add(7*24*time.Hour)); err == nil {
		t.Error("read with no time left")
	}
}

// TestTooManyToTell: what a rule's firings were, with more than are read to
// tell how many happened, says so on its page, in each language.
func TestTooManyToTell(t *testing.T) {
	ctx := context.Background()
	s := noSource(t, nil)
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	us := []verdict.Unrecorded{{Rule: "Errors", Firings: 14, Bound: "> 5", TooMany: true, Alerts: 1, Top: []verdict.AlertCount{{Labels: map[string]string{"service": "a"}, Firings: 14}}}}
	if err := s.l.ReplaceResult(ctx, "unrecorded", now, week, week.Add(7*24*time.Hour), us); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	if _, body, _ := get(t, h, "/rules/Errors"); !strings.Contains(body, "Too many firings to tell how many happened.") {
		t.Error("the rule's page doesn't say why")
	}
	if !strings.Contains(us[0].Text(), "Too many firings to tell how many happened.") {
		t.Errorf("%q", us[0].Text())
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	pageA11y(t, h, "", "/rules/Errors")
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
	_ = http.StatusOK
}

// TestReportCountsJudged: a report's precision, now and the span before, is
// of the firings judged: those in weeks with no record aren't, and with
// none judged there is none.
func TestReportCountsJudged(t *testing.T) {
	parallel(t)
	rep := &verdict.Report{Rules: []verdict.RuleResult{{Rule: "A", Firings: 20, White: 2, Unjudged: 14}, {Rule: "B", Firings: 4, White: 1}}}
	c := count(rep)
	if c.Firings != 24 || c.Caught != 3 || c.Nothing != 21 || c.Precision == nil || *c.Precision != 0.3 {
		t.Errorf("%+v %v", c, c.Precision)
	}
	if c := count(&verdict.Report{Rules: []verdict.RuleResult{{Rule: "A", Firings: 14, Unjudged: 14}}}); c.Precision != nil || c.pct() != "–" {
		t.Errorf("none judged: %+v", c)
	}
}

// TestSpanBeforeByWeek: over twelve weeks, the span before a rule's period
// is the twelve before them, of which an incident 20 weeks ago keeps three:
// its precision is of those three weeks' firings, as the period's is of
// the three an incident six weeks ago keeps, each week by its own record.
func TestSpanBeforeByWeek(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Backfill, c.Impact = 2*24*time.Hour, "" })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	mon := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	from := mon.Add(-25 * weekLen)
	var fs []firing.Firing
	for at := from.Add(12 * time.Hour); at.Before(now); at = at.Add(24 * time.Hour) {
		fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "search", "severity": "page"}, Start: at, End: at.Add(10 * time.Minute)})
	}
	if err := s.l.Record(ctx, ledger.Found{From: from, To: now, Firings: fs, Run: ledger.Run{Kind: "backfill", Started: from, Finished: from, From: from, To: now}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfg.Incidents, []byte("incidents: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, weeksAgo := range []int{20, 6} {
		at := mon.Add(-time.Duration(weeksAgo)*weekLen + 11*time.Hour + 55*time.Minute)
		in := incident.Incident{ID: fmt.Sprintf("INC-%d", i), Title: "Search down", Start: at, End: at.Add(30 * time.Minute), Labels: incident.Labels{"service": {"search"}}}
		if _, err := s.l.Put(ctx, in, "ana", s.cfg.Options); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/rules/Errors?weeks=12", nil)
	r.SetPathValue("name", "Errors")
	page, err := s.rulePage(r)
	if err != nil {
		t.Fatal(err)
	}
	v := page.(ruleView)
	if v.Before.IsZero() || v.Before.After(mon.Add(-20*weekLen)) {
		t.Fatalf("the span before from %s", v.Before)
	}
	// Now: of the 21 judged, one caught INC-1; then: of the 21, INC-0.
	if now, ok := sharePct(v.White, v.Firings-v.Unjudged); !ok || v.Firings-v.Unjudged != 21 || v.White != 1 {
		t.Fatalf("the period: %d of %d judged, %d%%", v.White, v.Firings-v.Unjudged, now)
	}
	if got := v.Panel.PrecisionBy; !got.On || got.By != 0 {
		t.Errorf("precision beside the span before: %+v", got)
	}
}
