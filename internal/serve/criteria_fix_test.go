package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// What review of the incident criteria found, held to (docs/design/serve.md,
// "The impact criterion").

func hist(s *Server) *history { return s.h.(*history) }

// asBefore049 leaves a server's ledger as a lapilli before 0.49 left it:
// impact_since alone, at since.
func asBefore049(t *testing.T, s *Server, since time.Time) {
	t.Helper()
	ledgerExec(t, s, "DELETE FROM meta WHERE key IN ('impact_criterion', 'impact_again', 'impact_data')")
	ledgerExec(t, s, "UPDATE meta SET value = ? WHERE key = 'impact_since'", strconv.FormatInt(since.Unix(), 10))
}

// a run due: an hour on, the cursor an hour behind it.
func runDue(s *Server, hours int) {
	s.now = func() time.Time { return now.Add(time.Duration(hours) * time.Hour) }
}

func options(t *testing.T, s *Server) time.Time {
	t.Helper()
	o, err := s.options(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return o.ImpactFrom
}

// On a first start, a selector whose data begins within the runs' span
// keeps the weeks before its first whole week out of the record.
func TestImpactDataFirstStart(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.Incidents = "" })
	hist(s).begins = map[string][]time.Time{"impact": {now.Add(-30 * time.Hour)}}
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), true)
	monday := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) // after Monday 2 06:00
	if from := options(t, s); !from.Equal(monday) {
		t.Errorf("kept from %s, not %s", from, monday)
	}
}

// An existing ledger, kept by a lapilli before 0.49, whose impact_since is
// before its selector's data: moved forward, at the first round that runs.
func TestImpactDataUpgrade(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	old := now.Add(-20 * 24 * time.Hour)
	asBefore049(t, s, old)
	hist(s).begins = map[string][]time.Time{"impact": {now.Add(-10 * 24 * time.Hour)}} // Saturday 24
	runDue(s, 2)
	s.jobs(ctx, true)
	monday := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if from := options(t, s); !from.Equal(monday) {
		t.Errorf("kept from %s, not %s", from, monday)
	}
	if k, err := s.l.ImpactKept(ctx); err != nil || !k.Since.Equal(monday) {
		t.Errorf("impact_since %s %v", k.Since, err)
	}
}

// Of an expression of two selectors, the one whose data begins later holds
// the record back, as only from there did both have data; of a selector, a
// series added late moves nothing; one with no data at all keeps no record,
// and the pages say so.
func TestImpactDataSelectors(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		begins map[string][]time.Time
		want   time.Time // zero: no record
		late   string
	}{
		{"a or b, b later", map[string][]time.Time{"probe_failed": {now.Add(-30 * time.Hour)}}, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), "probe_failed"},
		{"a series added late", map[string][]time.Time{"probe_failed": {now.Add(-30 * time.Hour), now.Add(-80 * time.Hour)}}, time.Time{}, ""},
		{"no data", map[string][]time.Time{"probe_failed": {}}, time.Time{}, ""},
	} {
		s := criteriaServer(t, false, nil)
		hist(s).begins = c.begins
		code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {"impact or probe_failed == 1"}}, "ana@example.com", "same-origin")
		if code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		s.jobs(ctx, false)
		k, err := s.l.ImpactKept(ctx)
		if err != nil || !k.AgainDone("5m impact or probe_failed == 1") {
			t.Fatalf("%s: %+v %v", c.name, k, err)
		}
		from := options(t, s)
		switch c.name {
		case "a series added late":
			// The earlier series: from where the read again begins.
			if began, _ := s.l.RunsFrom(ctx); !from.Equal(began) {
				t.Errorf("%s: kept from %s, not %s", c.name, from, began)
			}
		default:
			if !from.Equal(c.want) || k.Data.Late() != c.late {
				t.Errorf("%s: kept from %s, late %q; not %s, %q", c.name, from, k.Data.Late(), c.want, c.late)
			}
		}
		_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
		switch c.name {
		case "a or b, b later":
			if !strings.Contains(page, "probe_failed</bdi></code>: data from 2026-11-02") || !strings.Contains(page, "holds the record back") {
				t.Errorf("%s: the page doesn't say which:\n%s", c.name, page)
			}
		case "no data":
			if !strings.Contains(page, "keeps no record: a selector it reads has no data yet") || !strings.Contains(sourceRowOf(page, "Impact signal"), "no data") {
				t.Errorf("%s: the page doesn't say so:\n%s", c.name, page)
			}
		}
	}
}

// A selector with no data that only narrows one with data had nothing to
// match: the 5xx of a service that served none, beside all its requests.
// It holds nothing back, and the page says why, not that there is no
// record.
func TestImpactDataNarrowed(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).begins = map[string][]time.Time{`probe_failed{code="5"}`: {}}
	expr := `sum(probe_failed{code="5"}) / sum(probe_failed) > 0.5`
	code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {expr}}, "ana@example.com", "same-origin")
	if code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	k, err := s.l.ImpactKept(ctx)
	if err != nil || !k.AgainDone("5m "+expr) || k.Data == nil || k.Data.NoData {
		t.Fatalf("%+v %v", k, err)
	}
	if began, _ := s.l.RunsFrom(ctx); !options(t, s).Equal(began) {
		t.Errorf("kept from %s, not %s", options(t, s), began)
	}
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(page, "nothing matched since") || !strings.Contains(page, "<bdi>probe_failed{code!=&#34;&#34;}</bdi> has data") || strings.Contains(page, "keeps no record") {
		t.Errorf("the page doesn't say why:\n%s", page)
	}
}

// A selector that narrows another by a label the series got later (a
// rename, a relabelling) had nothing it could match before: its stand-in,
// the series with that label, holds the record back to where it begins.
func TestImpactDataLabelLater(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).begins = map[string][]time.Time{`probe_failed{code="5"}`: {}, `probe_failed{code!=""}`: {now.Add(-30 * time.Hour)}}
	expr := `sum(probe_failed{code="5"}) / sum(probe_failed) > 0.5`
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {expr}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	k, err := s.l.ImpactKept(ctx)
	if err != nil || k.Data == nil || k.Data.NoData || k.Data.Late() != `probe_failed{code="5"}` {
		t.Fatalf("%+v %v", k.Data, err)
	}
	if want := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC); !options(t, s).Equal(want) {
		t.Errorf("kept from %s, not %s", options(t, s), want)
	}
}

// A ledger whose impact data a lapilli before stand-ins kept, no record by
// a selector that only narrows another: at the next round its stand-in is
// looked for, and the record is kept from impact_since; the sources panel
// no longer says no data.
func TestImpactDataStandInUpgrade(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).begins = map[string][]time.Time{`probe_failed{code="5"}`: {}}
	expr := `sum(probe_failed{code="5"}) / sum(probe_failed) > 0.5`
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {expr}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	k, err := s.l.ImpactKept(ctx)
	if err != nil || k.Data == nil {
		t.Fatal(k, err)
	}
	// As the release before kept it: the selectors read, no stand-in.
	old := *k.Data
	old.Selectors = slices.DeleteFunc(slices.Clone(old.Selectors), func(x ledger.SelectorData) bool { return x.For != "" })
	if err := s.l.SetImpactData(ctx, old); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.l.ImpactKept(ctx); !k.Data.NoData || !k.From().IsZero() {
		t.Fatalf("as before: no data %v, from %s", k.Data.NoData, k.From())
	}
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(sourceRowOf(page, "Impact signal"), "no data") {
		t.Fatalf("as before, the sources row:\n%s", sourceRowOf(page, "Impact signal"))
	}
	runDue(s, 2)
	s.jobs(ctx, true)
	k, err = s.l.ImpactKept(ctx)
	if err != nil || k.Data.NoData || !k.From().Equal(k.Since) {
		t.Fatalf("at the next round: no data %v, from %s, since %s (%v)", k.Data.NoData, k.From(), k.Since, err)
	}
	_, page = askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if strings.Contains(sourceRowOf(page, "Impact signal"), "no data") {
		t.Errorf("the sources row still says no data:\n%s", sourceRowOf(page, "Impact signal"))
	}
}

// A ledger a lapilli before stand-ins wrote back, a stand-in's "for" and
// "of" let go: given back at the next round, not taken for a selector the
// expression reads, nor looked for twice.
func TestImpactDataStandInDowngraded(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).begins = map[string][]time.Time{`probe_failed{code="5"}`: {}}
	expr := `sum(probe_failed{code="5"}) / sum(probe_failed) > 0.5`
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {expr}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	k, err := s.l.ImpactKept(ctx)
	if err != nil || k.Data == nil {
		t.Fatal(k, err)
	}
	old := *k.Data
	old.Selectors = slices.Clone(old.Selectors)
	for i := range old.Selectors {
		old.Selectors[i].For, old.Selectors[i].Of = "", ""
	}
	if err := s.l.SetImpactData(ctx, old); err != nil {
		t.Fatal(err)
	}
	runDue(s, 2)
	s.jobs(ctx, true)
	k, err = s.l.ImpactKept(ctx)
	if err != nil || k.Data.NoData || len(k.Data.Selectors) != 3 || k.Data.Selectors[2].For != `probe_failed{code="5"}` || k.Data.Selectors[2].Of != "probe_failed" {
		t.Fatalf("at the next round: %+v %v", k.Data, err)
	}
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if strings.Contains(page, "<bdi>probe_failed{code!=&#34;&#34;}</bdi></code>:") {
		t.Errorf("the stand-in listed as a selector:\n%s", page)
	}
}

// A selector whose own data begins later than its stand-in's, a first 5xx,
// begins by its stand-in: the page says from when, and by what.
func TestImpactDataStandInEarlier(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).begins = map[string][]time.Time{`probe_failed{code="5"}`: {now.Add(-30 * time.Hour)}}
	expr := `sum(probe_failed{code="5"}) / sum(probe_failed) > 0.5`
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {expr}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	if began, _ := s.l.RunsFrom(ctx); !options(t, s).Equal(began) {
		t.Errorf("kept from %s, not %s", options(t, s), began)
	}
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(page, "kept from") || !strings.Contains(page, "<bdi>probe_failed{code!=&#34;&#34;}</bdi> begins") {
		t.Errorf("the page doesn't say by what:\n%s", page)
	}
}

// A record a stand-in kept stays when the selector's own first sample
// comes: a first 5xx moves impact_since nowhere.
func TestImpactDataFirstMatchKeeps(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).begins = map[string][]time.Time{`probe_failed{code="5"}`: {}}
	expr := `sum(probe_failed{code="5"}) / sum(probe_failed) > 0.5`
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {expr}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	k, err := s.l.ImpactKept(ctx)
	if err != nil || k.From().IsZero() {
		t.Fatalf("kept from %s (%v)", k.From(), err)
	}
	since, from := k.Since, k.From()
	hist(s).begins = map[string][]time.Time{`probe_failed{code="5"}`: {now.Add(time.Hour)}}
	runDue(s, 3)
	s.jobs(ctx, true)
	if k, err = s.l.ImpactKept(ctx); err != nil || !k.Since.Equal(since) || !k.From().Equal(from) || k.Data.Selectors[0].First.IsZero() {
		t.Errorf("after the first 5xx: since %s, from %s, its first %s; not %s, %s (%v)", k.Since, k.From(), k.Data.Selectors[0].First, since, from, err)
	}
}

// A ledger before 0.49, with no criterion kept, and a criterion saved
// before the leader's first round that isn't the command line's: read
// again over the last four weeks, not taken as read all along.
func TestImpactUpgradeSavedFirst(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	began, _ := s.l.RunsFrom(ctx)
	asBefore049(t, s, began.Add(-time.Hour))
	if err := s.l.SetSettings(ctx, map[string]string{"impact": "impact > 1"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	if plan, _, err := s.againPlan(ctx, criterion{"impact > 1", 5 * time.Minute}); err != nil || plan != againRead {
		t.Fatalf("plan %d %v", plan, err)
	}
	s.jobs(ctx, false)
	if k, err := s.l.ImpactKept(ctx); err != nil || !k.AgainDone("5m impact > 1") || !k.Since.Equal(began) || hist(s).exprs["impact > 1"] != 1 {
		t.Errorf("%+v %v, read %d", k, err, hist(s).exprs["impact > 1"])
	}
}

// The same, the read again failing, and the runs reading the new criterion
// meanwhile: they take the ledger's for the command line's, which is kept,
// the new one read from a floor on, and the read again is still due.
func TestImpactUpgradeRunFirst(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	began, _ := s.l.RunsFrom(ctx)
	asBefore049(t, s, began.Add(-time.Hour))
	hist(s).failLong = map[string]bool{"impact > 1": true}
	if err := s.l.SetSettings(ctx, map[string]string{"impact": "impact > 1"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	runDue(s, 2)
	s.jobs(ctx, true)
	k, err := s.l.ImpactKept(ctx)
	if err != nil || k.Criterion != "5m impact" || k.Read != "5m impact > 1" || k.Floor.IsZero() || !k.From().IsZero() {
		t.Fatalf("%+v %v", k, err)
	}
	if plan, _, err := s.againPlan(ctx, criterion{"impact > 1", 5 * time.Minute}); err != nil || plan != againRead {
		t.Errorf("plan %d %v", plan, err)
	}
}

// Of a ledger before 0.49 whose runs began to read the signal within the
// last four weeks, the weeks before are read again, not taken as read.
func TestImpactUpgradeLateStart(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	asBefore049(t, s, now.Add(-2*time.Hour))
	if plan, _, err := s.againPlan(ctx, criterion{"impact", 5 * time.Minute}); err != nil || plan != againRead {
		t.Errorf("plan %d %v", plan, err)
	}
}

// A new criterion is saved once read in full over the last four weeks: one
// that can't be is refused, nothing saved; one a try read lately is saved
// with no read more.
func TestImpactSaveReadsFirst(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	h := s.Handler()
	hist(s).failLong = map[string]bool{"impact > 1": true}
	code, body := askAs(t, h, http.MethodPost, "/settings/impact", url.Values{"impact": {"impact > 1"}}, "ana@example.com", "same-origin")
	if code != 400 || !strings.Contains(body, "couldn&#39;t be read over the last four weeks") {
		t.Errorf("%d\n%s", code, body)
	}
	if set, _ := s.l.Settings(ctx); len(set) != 0 {
		t.Errorf("saved: %v", set)
	}
	hist(s).failLong = nil
	if code, _ := askAs(t, h, http.MethodGet, "/settings?impact-try=1&impact=impact+%3E+1&impact-for=5m", nil, "ana@example.com", "same-origin"); code != 200 {
		t.Fatal(code)
	}
	read := hist(s).exprs["impact > 1"]
	if code, _ := askAs(t, h, http.MethodPost, "/settings/impact", url.Values{"impact": {"impact > 1"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if n := hist(s).exprs["impact > 1"]; n != read {
		t.Errorf("read %d more to save", n-read)
	}
}

// impactProposed makes a proposal waiting for approval, backtested against the
// impact windows of criterion over the last twelve weeks, with a record.
func impactProposed(t *testing.T, s *Server, criterion string) int64 {
	t.Helper()
	no := false
	five := 5 * time.Minute
	ev, _ := json.Marshal(evidence{From: "person", Edit: &rules.Edit{Name: "Errors", For: &five}, Semantics: semantics, Version: "test", Impact: criterion,
		Backtest: &backtest{From: now.Add(-12 * weekLen), To: now, Before: side{Firings: 21, White: 1}, After: side{Firings: 2, White: 1}, NoRecord: &no}})
	id, err := s.l.Propose(context.Background(), ledger.Proposal{Rule: "Errors", Kind: ledger.KindFor, Path: ledger.PathGit, MadeBy: "bo",
		Before: "Errors ()\nfor: 2m\n", After: "Errors ()\nfor: 5m\n", Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func staleOf(t *testing.T, s *Server, id int64) *staleWhy {
	t.Helper()
	p, err := s.l.ProposalByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := s.linesNow(context.Background(), []ledger.Proposal{p})
	if err != nil {
		t.Fatal(err)
	}
	return ls[0].StaleWhy
}

// A proposal backtested against the windows of the criterion read before
// is stale once another is read again in its place, though its weeks keep
// their record (staleImpact); the try said it would be, the same count.
func TestImpactStaleImpact(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, true, nil) // INC-1 keeps the record
	id := impactProposed(t, s, "5m impact")
	if w := staleOf(t, s, id); w != nil {
		t.Fatalf("stale before: %+v", w)
	}
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings?impact-try=1&impact=impact+%3E+1", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "1 waiting proposals would need a new backtest") {
		t.Errorf("the try doesn't say so:\n%s", body)
	}
	if code, _ := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {"impact > 1"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	s.jobs(ctx, false)
	if w := staleOf(t, s, id); w == nil || w.Format != staleImpact {
		t.Errorf("after: %+v", w)
	}
}

// Tried, the criterion in effect, read already, changes nothing: no
// measure changes, and no proposal would be held.
func TestImpactTrySame(t *testing.T) {
	s := criteriaServer(t, false, nil)
	impactProposed(t, s, "5m impact")
	// Kept from a Monday three weeks before the runs began.
	ledgerExec(t, s, "UPDATE meta SET value = ? WHERE key = 'impact_since'", strconv.FormatInt(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC).Unix(), 10))
	ledgerExec(t, s, "DELETE FROM meta WHERE key = 'impact_data'")
	code, body := askAs(t, s.Handler(), http.MethodGet, "/settings?impact-try=1&impact=impact&impact-for=5m", nil, "ana@example.com", "same-origin")
	if code != 200 || !strings.Contains(body, "nothing would change") || strings.Contains(body, "<strong>") || strings.Contains(body, "waiting proposals would need") {
		t.Errorf("%d\n%s", code, body)
	}
}

// A try says what it would cost the weeks before its span the record is
// kept of now.
func TestImpactTryEarlierWeeks(t *testing.T) {
	s := criteriaServer(t, false, nil)
	monday := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) // three weeks before
	ledgerExec(t, s, "UPDATE meta SET value = ? WHERE key = 'impact_since'", strconv.FormatInt(monday.Unix(), 10))
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings?impact-try=1&impact=impact+%3E+1", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, `<th scope="row">Weeks before them with a record</th><td>2 of 2</td><td><strong>0 of 2</strong></td>`) {
		t.Errorf("no weeks before:\n%s", body)
	}
}

// A try's reads are bounded by tryWithin, whatever --timeout is.
func TestImpactTryWithin(t *testing.T) {
	s := criteriaServer(t, false, func(c *Config) { c.Timeout = time.Hour })
	hist(s).within = nil
	if code, _ := askAs(t, s.Handler(), http.MethodGet, "/settings?impact-try=1&impact=impact+%3E+1", nil, "ana@example.com", "same-origin"); code != 200 {
		t.Fatal(code)
	}
	if len(hist(s).within) < 2 {
		t.Fatalf("%d reads with a deadline", len(hist(s).within))
	}
	for _, d := range hist(s).within {
		if d > tryWithin {
			t.Errorf("a read had %s", d)
		}
	}
}

// A try is from this site's page, or typed: a request that says neither
// (no Sec-Fetch-Site) is refused.
func TestImpactTryFromHere(t *testing.T) {
	s := criteriaServer(t, false, nil)
	path := "/settings?impact-try=1&impact=impact+%3E+1"
	if code, _ := askAs(t, s.Handler(), http.MethodGet, path, nil, "ana@example.com", ""); code != 400 {
		t.Errorf("said nothing: %d", code)
	}
	if code, _ := askAs(t, s.Handler(), http.MethodGet, path, nil, "ana@example.com", "none"); code != 200 {
		t.Errorf("typed: %d", code)
	}
}

// A save of --impact-for alone reads the criterion, as one of a new
// expression does, and counts among the tries.
func TestImpactForSaveCounts(t *testing.T) {
	s := criteriaServer(t, false, nil)
	h := s.Handler()
	for i := range impactTriesAnHour {
		path := fmt.Sprintf("/settings?impact-try=1&impact-for=5m&impact=impact+%%3E+%d", i+10)
		if code, _ := askAs(t, h, http.MethodGet, path, nil, "ana@example.com", "same-origin"); code != 200 {
			t.Fatal(code)
		}
	}
	code, body := askAs(t, h, http.MethodPost, "/settings/impact", url.Values{"impact": {"impact"}, "impact-for": {"10m"}}, "ana@example.com", "same-origin")
	if code != 400 || !strings.Contains(body, "as many incident criteria") {
		t.Errorf("%d\n%s", code, body)
	}
}

// A save's read takes the heavy turn: while another holds it, it waits,
// and is refused when it can't have it.
func TestImpactSaveHeavyTurn(t *testing.T) {
	s := criteriaServer(t, false, nil)
	defer func(d time.Duration) { busyWait = d }(busyWait)
	busyWait = time.Millisecond
	s.heavy <- struct{}{}
	defer func() { <-s.heavy }()
	code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {"impact > 1"}}, "ana@example.com", "same-origin")
	if code == http.StatusSeeOther || !strings.Contains(body, "busy") {
		t.Errorf("%d\n%s", code, body)
	}
}

// An expression is 4,096 characters at most, as its field counts them;
// a range or a subquery over a day is refused.
func TestImpactChecks(t *testing.T) {
	long := strings.Repeat("가", maxImpactExpr-10) + " > 1"
	for expr, ok := range map[string]bool{
		long:                           true,
		long + "1":                     true,
		long + strings.Repeat("1", 10): false,
		`rate(x[1d]) > 1`:              true,
		`rate(x[25h]) > 1`:             false,
		`max_over_time(x[5m:1m]) > 1`:  true,
		`max_over_time(x[2d:1h]) > 1`:  false,
		`max_over_time(x[1h:]) > 1`:    true,
		// Past a day however it is written, or stepping by less than a
		// minute.
		`max_over_time(up[201y]) > 1`:  false,
		`rate(up[90000]) > 1`:          false,
		`max_over_time(up[1d:1s]) > 1`: false,
		manySelectors(33):              false,
		manySelectors(32):              true,
	} {
		if err := checkImpact(expr); (err == nil) != ok {
			t.Errorf("%.30q: %v", expr, err)
		}
	}
}

// manySelectors is an expression that reads n selectors.
func manySelectors(n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteString(" + ")
		}
		fmt.Fprintf(&b, "m%d", i)
	}
	return b.String() + " > 1"
}

// A try's query may carry an expression its field takes.
func TestImpactTryLongQuery(t *testing.T) {
	s := criteriaServer(t, false, nil)
	// Some 9 KB escaped, past maxQuery.
	expr := "impact > " + strings.Repeat("(", 1400) + "1" + strings.Repeat(")", 1400)
	code, body := askAs(t, s.Handler(), http.MethodGet, "/settings?impact-try=1&impact="+url.QueryEscape(expr), nil, "ana@example.com", "same-origin")
	if code != 200 {
		t.Errorf("%d %.300s", code, body)
	}
}

// A try over more firings than are judged at once is refused in its own
// words.
func TestImpactTryTooMany(t *testing.T) {
	s := criteriaServer(t, false, nil)
	defer func(n int) { reportFirings = n }(reportFirings)
	reportFirings = 1
	code, body := askAs(t, s.Handler(), http.MethodGet, "/settings?impact-try=1&impact=impact+%3E+1", nil, "ana@example.com", "same-origin")
	if code != 400 || !strings.Contains(body, "the most a try of the incident criteria judges at once") || strings.Contains(body, "firings=false") {
		t.Errorf("%d\n%s", code, body)
	}
}

// "Waiting to be read again" is said only when it will be: not of a new
// ledger whose runs read the signal from their first.
func TestImpactNoWaiting(t *testing.T) {
	s := criteriaServer(t, false, nil)
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if strings.Contains(body, "Waiting to be read again") {
		t.Errorf("says it waits:\n%s", body)
	}
}

// The impact row waits once another criterion is saved, until it is read
// with it; and is quiet when not read for longer than two runs.
func TestSourcesImpactRead(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	if err := s.l.SetSettings(ctx, map[string]string{"impact": "impact > 1"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if row := sourceRowOf(body, "Impact signal"); !strings.Contains(row, "↻ waiting") {
		t.Errorf("saved:\n%s", row)
	}
	s.jobs(ctx, false)
	runDue(s, 12)
	_, body = askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if row := sourceRowOf(body, "Impact signal"); !strings.Contains(row, "quiet") || !strings.Contains(row, "are the runs running?") {
		t.Errorf("read long ago:\n%s", row)
	}
}

// The quiet sources are listed first, never among those past the list.
func TestSourcesQuietFirst(t *testing.T) {
	ctx := context.Background()
	token := "an-incidents-token-0123"
	s := criteriaServer(t, false, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", tokenFile(t, token) })
	if err := s.l.NoteReceived(ctx, "OLD", now.Add(-5*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := range receivedSources + 2 {
		if err := s.l.NoteReceived(ctx, fmt.Sprintf("S%02d", i), now.Add(-time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if row := sourceRowOf(body, "OLD-"); !strings.Contains(row, "quiet") {
		t.Errorf("the quiet one isn't listed:\n%s", body)
	}
}

// An incidents file's error is said without its path to a reader who isn't
// an admin.
func TestSourcesFileErrorPath(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, true, nil)
	s.cfg.Incidents += ".gone"
	runDue(s, 2)
	s.jobs(ctx, true)
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "bo@example.com", "same-origin")
	row := sourceRowOf(body, "Incidents file")
	if !strings.Contains(row, "failing") || strings.Contains(row, s.cfg.Incidents) || !strings.Contains(row, "the incidents file") {
		t.Errorf("%s", row)
	}
	// Nor in the status banner of every page.
	for _, p := range []string{"/settings", "/"} {
		if _, body := askAs(t, s.Handler(), http.MethodGet, p, nil, "bo@example.com", "same-origin"); strings.Contains(body, s.cfg.Incidents) {
			i := strings.Index(body, s.cfg.Incidents)
			t.Errorf("%s: the path is on the page: %s", p, body[max(0, i-200):min(len(body), i+120)])
		}
	}
	if _, body := askAs(t, s.Handler(), http.MethodGet, "/", nil, "ana@example.com", "same-origin"); !strings.Contains(body, s.cfg.Incidents) {
		t.Error("an admin doesn't read the path")
	}
}

// The Cleanup page lists the tasks kept on an impact window read again
// that nothing found covers now.
func TestOrphansImpactNotes(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	key := "impact-20261103T0600Z"
	id, err := s.l.PutTask(ctx, ledger.Task{Title: "look", Kind: "decide", Incident: key}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	// Read again with a criterion that finds nothing.
	if err := s.l.SetSettings(ctx, map[string]string{"impact": "impact > 9"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, false)
	_, body := askAs(t, s.Handler(), http.MethodGet, "/orphans", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, key) || !strings.Contains(body, fmt.Sprintf("/tasks/%d", id)) {
		t.Errorf("not listed:\n%s", body)
	}
}

// saveFailing saves a criterion whose read again fails, as the replica
// that leads reads it: "" saves the command line's back.
func saveFailing(t *testing.T, s *Server, expr string) {
	t.Helper()
	if err := s.l.SetSettings(context.Background(), map[string]string{"impact": expr}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	if expr == "" {
		expr = s.cfg.Impact
	}
	h := hist(s)
	h.Lock()
	if h.failLong == nil {
		h.failLong = map[string]bool{}
	}
	h.failLong[expr] = true
	h.Unlock()
}

// readsAgain lets each read again of an expression through.
func readsAgain(s *Server) {
	h := hist(s)
	h.Lock()
	h.failLong = nil
	h.Unlock()
}

// A criterion saved, its read again failing, a run reading it: a floor,
// no record at all, though the one kept had data all along; its selector
// with no data says nothing more.
func TestImpactPendingNoData(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).begins = map[string][]time.Time{"gone": {}}
	if from := options(t, s); from.IsZero() {
		t.Fatal("the one kept, with data, keeps no record")
	}
	saveFailing(t, s, "gone > 1")
	runDue(s, 2)
	s.jobs(ctx, true)
	if from := options(t, s); !from.IsZero() {
		t.Errorf("a floor, its selector with no data: kept from %s", from)
	}
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(page, "the impact signal keeps no record until the criterion in effect is read again") || strings.Contains(page, "The record is kept by the impact signal from") {
		t.Errorf("the page doesn't say so:\n%s", page)
	}
	if row := sourceRowOf(page, "Impact signal"); !strings.Contains(row, `status-warn">⚠&#xFE0E; no record`) || !strings.Contains(row, "waiting to be read again") {
		t.Errorf("the sources row:\n%s", row)
	}
	if !strings.Contains(page, "raise --timeout or --max-samples") {
		t.Errorf("no way past a failing read again:\n%s", page)
	}
}

// Two criteria saved one after the other, each read again failing, each
// read by a run: no record from the first one's first read on, the second
// with data all along, the first with none.
func TestImpactTwoSavesPending(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).begins = map[string][]time.Time{"gone": {}}
	saveFailing(t, s, "gone > 1")
	runDue(s, 2)
	s.jobs(ctx, true)
	saveFailing(t, s, "impact > 1")
	runDue(s, 3)
	s.jobs(ctx, true)
	if hist(s).exprs["impact > 1"] < 2 {
		t.Fatalf("the second isn't read: %v", hist(s).exprs)
	}
	if from := options(t, s); !from.IsZero() {
		t.Errorf("after two saves: kept from %s", from)
	}
}

// A criterion saved and read by runs, then the one kept saved back, its
// read again failing: its window isn't split at each run's start, and no
// record is kept until it is read again.
func TestImpactRevertPendingNoSplit(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, func(c *Config) { c.Impact = "always" })
	saveFailing(t, s, "impact > 1")
	runDue(s, 2)
	s.jobs(ctx, true)
	saveFailing(t, s, "")
	for hours := 3; hours <= 6; hours++ {
		runDue(s, hours)
		s.jobs(ctx, true)
		if from := options(t, s); !from.IsZero() {
			t.Errorf("saved back, %dh: kept from %s", hours, from)
		}
	}
	cursor, _ := s.l.Cursor(ctx)
	ws, err := s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), cursor)
	if err != nil || len(ws) != 2 || ws[1].End.Before(cursor) {
		t.Errorf("%d windows, not the one kept's and its own on to %s: %+v %v", len(ws), cursor, ws, err)
	}
	readsAgain(s)
	runDue(s, 24)
	s.jobs(ctx, true)
	if k, _ := s.l.ImpactKept(ctx); !k.Floor.IsZero() || options(t, s).IsZero() {
		t.Errorf("read again: %+v, kept from %s", k, options(t, s))
	}
}

// The one kept with no data, another with data read by a run, then the
// one kept saved back and read by a run: no record, the data of the other
// saying nothing of it.
func TestImpactRevertPendingNoData(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Incidents = "" })
	hist(s).begins = map[string][]time.Time{"impact": {}}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if from := options(t, s); !from.IsZero() {
		t.Fatalf("no data: kept from %s", from)
	}
	saveFailing(t, s, "probe == 1")
	runDue(s, 2)
	s.jobs(ctx, true)
	saveFailing(t, s, "")
	runDue(s, 3)
	s.jobs(ctx, true)
	if from := options(t, s); !from.IsZero() {
		t.Errorf("saved back: kept from %s", from)
	}
}

// A floor that an older leader left with impact_again saying the one kept
// needs no read again, and the runs reading it: read again all the same,
// which lifts it.
func TestImpactFloorLeft(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	s.jobs(ctx, false) // marked: read from the runs' first
	k, err := s.l.ImpactKept(ctx)
	if err != nil || !k.AgainDone("5m impact") {
		t.Fatalf("%+v %v", k, err)
	}
	cursor, _ := s.l.Cursor(ctx)
	ledgerExec(t, s, "INSERT INTO meta (key, value) VALUES ('impact_floor', ?)", strconv.FormatInt(cursor.Add(-time.Hour).Unix(), 10))
	ledgerExec(t, s, "INSERT INTO meta (key, value) VALUES ('impact_read', '5m impact') ON CONFLICT (key) DO UPDATE SET value = excluded.value")
	if plan, _, err := s.againPlan(ctx, criterion{"impact", 5 * time.Minute}); err != nil || plan != againRead {
		t.Fatalf("plan %d %v", plan, err)
	}
	s.jobs(ctx, false)
	if k, err := s.l.ImpactKept(ctx); err != nil || !k.Floor.IsZero() || options(t, s).IsZero() {
		t.Errorf("not lifted: %+v %v", k, err)
	}
}

// Of an expression's selectors, one whose look fails doesn't keep the
// others from being looked for.
func TestImpactLookForPassesFailing(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).failLong = map[string]bool{"count(a)": true}
	cursor, _ := s.l.Cursor(ctx)
	d := &ledger.ImpactData{Criterion: "5m a == 1 or b == 1", From: cursor.Add(-72 * time.Hour), Selectors: []ledger.SelectorData{{Sel: "a"}, {Sel: "b"}}}
	if err := s.lookFor(ctx, d, cursor); err == nil {
		t.Error("a's failure isn't said")
	}
	if b := d.Selectors[1]; !b.To.Equal(cursor) || b.First.IsZero() {
		t.Errorf("b isn't looked for: %+v", b)
	}
}

// A proposal backtested while a floor stands is labeled with it, a label
// no criterion kept is: held once the criterion saved is read again.
func TestImpactHeldInGap(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, true, nil)
	saveFailing(t, s, "impact > 1")
	runDue(s, 2)
	s.jobs(ctx, true)
	held, err := s.impactHeld(ctx)
	if err != nil || !strings.HasPrefix(held, "pending ") {
		t.Fatalf("held %q %v", held, err)
	}
	id := impactProposed(t, s, held)
	readsAgain(s)
	runDue(s, 4) // past the hour the failed read again waits
	s.jobs(ctx, false)
	if w := staleOf(t, s, id); w == nil || w.Format != staleImpact {
		t.Errorf("not held once read again: %+v", w)
	}
}

// The same, the one kept saved back: held once it is read again.
func TestImpactHeldAfterRevert(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, true, nil)
	saveFailing(t, s, "impact > 1")
	runDue(s, 2)
	s.jobs(ctx, true)
	saveFailing(t, s, "")
	runDue(s, 3)
	s.jobs(ctx, true)
	held, err := s.impactHeld(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := impactProposed(t, s, held)
	readsAgain(s)
	runDue(s, 24)
	s.jobs(ctx, false)
	if k, _ := s.l.ImpactKept(ctx); !k.Floor.IsZero() {
		t.Fatalf("not read again: %+v", k)
	}
	if w := staleOf(t, s, id); w == nil || w.Format != staleImpact {
		t.Errorf("not held once read again: %+v", w)
	}
}

// An offset moves where a selector's data is read: its data begins an
// offset after its metric's.
func TestImpactDataOffset(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	hist(s).begins = map[string][]time.Time{"x": {now.Add(-80 * time.Hour)}, "x offset 1w": {now.Add(-30 * time.Hour)}}
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {"rate(x[5m]) > 2 * rate(x[5m] offset 1w)"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	monday := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	if from := options(t, s); !from.Equal(monday) {
		t.Errorf("kept from %s, not %s", from, monday)
	}
}

// Saving a criterion counts the replica that leads reading it again among
// the saver's reads, a try lately or not: saving back and forth is held to
// the same twelve an hour.
func TestImpactSavesCounted(t *testing.T) {
	s := criteriaServer(t, false, nil)
	h := s.Handler()
	for _, e := range []string{"impact+%3E+1", "impact+%3E+2"} {
		if code, _ := askAs(t, h, http.MethodGet, "/settings?impact-try=1&impact="+e, nil, "ana@example.com", "same-origin"); code != 200 {
			t.Fatal(code)
		}
	}
	refused := false
	for i := range impactTriesAnHour {
		e := []string{"impact > 1", "impact > 2"}[i%2]
		code, body := askAs(t, h, http.MethodPost, "/settings/impact", url.Values{"impact": {e}}, "ana@example.com", "same-origin")
		if code != http.StatusSeeOther {
			refused = code == 400 && strings.Contains(body, "as many incident criteria")
			break
		}
	}
	if !refused {
		t.Error("saved back and forth past the hour's reads")
	}
}

// A try holds what was decided about a window found before that a read
// again would hold, one on from before the span however long before it
// started.
func TestImpactTryLongDecided(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	began, _ := s.l.RunsFrom(ctx)
	// The runs' window, from 30 hours before the span to 6 hours into it,
	// dismissed.
	start := began.Add(-30 * time.Hour)
	if err := s.l.Record(ctx, ledger.Found{From: start, To: began.Add(6 * time.Hour), ImpactOn: true, ImpactSince: start, ImpactCriterion: "5m impact",
		Impact: []incident.Incident{{ID: "w", Start: start, End: began.Add(6 * time.Hour), Candidate: true}},
		Run:    ledger.Run{Kind: "test", Started: start, Finished: start, From: start, To: began.Add(6 * time.Hour)}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Decide(ctx, "impact-"+start.Format("20060102T1504Z"), "dismissed", "", "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	hist(s).begins = nil
	s.h = &spanHistory{history: hist(s), at: began.Add(time.Hour), long: 4 * time.Hour}
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings?impact-try=1&impact=span", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "no: dismissed before") {
		t.Errorf("the decision isn't held:\n%s", body)
	}
}

// spanHistory answers "span" with one series true from at for long.
type spanHistory struct {
	*history
	at   time.Time
	long time.Duration
}

func (h *spanHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr != "span" {
		return h.history.QueryRange(ctx, expr, start, end, step)
	}
	se := prom.Series{Labels: map[string]string{}}
	for t := h.at; t.Before(h.at.Add(h.long)); t = t.Add(step) {
		if !t.Before(start) && !t.After(end) {
			se.Samples = append(se.Samples, prom.Sample{T: t, V: 1})
		}
	}
	return []prom.Series{se}, nil
}
