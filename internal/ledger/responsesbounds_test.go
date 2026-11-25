package ledger

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/response"
)

func pageFor(id, rule string, sent time.Time) response.Page {
	return response.Page{ID: id, Sent: sent, Alerts: []incident.Alert{{Rule: rule, Labels: map[string]string{"name": id}}}}
}

// A write of pages is held to the firings PUT's bounds: a period of 31 days
// at most, pages sent no later than an hour from now, 50 sources, and pages
// per source and per week.
func TestPutResponsesBounds(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	refused := func(what, want string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error saying %q", what, err, want)
		}
	}
	_, err := l.PutResponses(ctx, "pd", at, at.Add(32*24*time.Hour), nil, o)
	refused("a period over 31 days", "31 days", err)
	soon := time.Now().Add(3 * time.Hour)
	_, err = l.PutResponses(ctx, "pd", time.Now().Add(-time.Hour), soon.Add(time.Hour), []response.Page{pageFor("future", "R", soon)}, o)
	refused("a page sent 3 hours ahead", "sent", err)
	_, err = l.PutResponses(ctx, "pd", soon, soon.Add(time.Hour), nil, o)
	refused("a period from the future", "future", err)
	_, err = l.SyncResponses(ctx, ResponsesFile, "", []response.Page{pageFor("future", "R", soon)}, o)
	refused("a file's page sent ahead", "sent", err)
	if n, _ := l.CountPages(ctx, at, soon.Add(24*time.Hour)); n != 0 {
		t.Errorf("a refused write left %d pages", n)
	}

	// Pages per source and per week, made small.
	defer func(a, b int) { maxSourcePages, maxWeekPages = a, b }(maxSourcePages, maxWeekPages)
	maxSourcePages, maxWeekPages = 3, 5
	var pages []response.Page
	for i := 0; i < 4; i++ {
		pages = append(pages, pageFor(fmt.Sprint("p", i), "R", at.Add(time.Duration(i)*time.Minute)))
	}
	_, err = l.PutResponses(ctx, "pd", at, at.Add(time.Hour), pages, o)
	refused("a source's fourth page", "3 pages at most", err)
	if _, err := l.PutResponses(ctx, "pd", at, at.Add(time.Hour), pages[:3], o); err != nil {
		t.Fatal(err)
	}
	_, err = l.PutResponses(ctx, "pd", at.Add(time.Hour), at.Add(2*time.Hour), []response.Page{pageFor("q", "R", at.Add(time.Hour))}, o)
	refused("a source's page over what it keeps", "3 pages at most", err)
	// Another source's pages in the same week: two more are 5, the third 6.
	if _, err := l.PutResponses(ctx, "ops", at, at.Add(time.Hour), []response.Page{pageFor("a", "R", at), pageFor("b", "R", at)}, o); err != nil {
		t.Fatal(err)
	}
	_, err = l.PutResponses(ctx, "other", at, at.Add(time.Hour), []response.Page{pageFor("c", "R", at)}, o)
	refused("a week's sixth page", "a week keeps 5", err)
	// Taken back, the write left nothing of its own: not its source either.
	var n int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM response_sources WHERE source = 'other'").Scan(&n); err != nil || n != 0 {
		t.Errorf("a refused write's source was kept: %d %v", n, err)
	}
	maxSourcePages, maxWeekPages = 500_000, 200_000
	// 50 sources, not a 51st; a source with pages already can be written.
	for i := 0; i < MaxSources; i++ {
		if _, err := l.PutResponses(ctx, fmt.Sprint("s", i), at, at.Add(time.Hour), []response.Page{pageFor("x", "R", at)}, o); err != nil && i >= MaxSources-2 {
			// "pd" and "ops" made two already.
			break
		}
	}
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM response_sources").Scan(&n); err != nil || n != MaxSources {
		t.Fatalf("sources: %d %v", n, err)
	}
	_, err = l.PutResponses(ctx, "one-more", at, at.Add(time.Hour), []response.Page{pageFor("x", "R", at)}, o)
	refused("a 51st source", "50 sources at most", err)
	if _, err := l.PutResponses(ctx, "pd", at, at.Add(time.Hour), pages[:2], o); err != nil {
		t.Errorf("a source the ledger keeps: %v", err)
	}
}

// Pages are read by the rules of the firings judged, and at the times they
// may answer them. In a crash loop (50 sources paging 400 times each in an
// hour, for 50 rules), judging a firing of one rule reads that rule's pages
// and not the 20 000, and allocates under 4 MiB doing it (counted in bytes
// allocated, not in time).
func TestResponsesReadByRule(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	for s := 0; s < 50; s++ {
		var pages []response.Page
		for i := 0; i < 400; i++ {
			pages = append(pages, pageFor(fmt.Sprintf("p%03d", i), fmt.Sprintf("Crash%d", i%50), at.Add(time.Duration(i)*time.Second)))
		}
		if _, err := l.PutResponses(ctx, fmt.Sprintf("src-%d", s), at, at.Add(time.Hour), pages, o); err != nil {
			t.Fatal(err)
		}
	}
	// 50 sources' pages of Crash7: 8 each.
	f := respFiring{ID: 1, Rule: "Crash7", Labels: map[string]string{"name": "p007"}, Start: at, End: at.Add(10 * time.Minute)}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r, err := l.newResponder(ctx, l.r, l.r, o, []respFiring{f}, at.Add(time.Hour))
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.pages.Len(); got != 50*8 {
		t.Errorf("pages read: %d, want %d, those naming Crash7", got, 50*8)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 4<<20 {
		t.Errorf("reading the pages of one rule allocated %d bytes, more than 4 MiB", got)
	}
	// The page this firing's labels name is found, from whichever source:
	// the nearest by sent time, the source before another as far.
	resp, _, err := r.respond(f)
	if err != nil || resp.Page == nil || resp.Page.ID != "p007" || resp.Page.Source != "src-0" {
		t.Errorf("the firing's page: %+v %v", resp.Page, err)
	}
}

// What leaves with --keep leaves with the page's rules, and a page dated past
// what a write may send, which a write would refuse, is dropped as well,
// whatever --keep says.
func TestPruneDropsFuturePages(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	now := time.Now()
	if _, err := l.PutResponses(ctx, "pd", now.Add(-48*time.Hour), now, []response.Page{pageFor("now", "R", now.Add(-time.Hour)), pageFor("old", "R", now.Add(-40*time.Hour))}, o); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		fmt.Sprintf("INSERT INTO responses (source, id, sent_at, alerts) VALUES ('pd', 'ahead', %d, '[]')", now.Add(5*time.Hour).Unix()),
		fmt.Sprintf("INSERT INTO response_rules (rule, sent_at, source, id) VALUES ('R', %d, 'pd', 'ahead')", now.Add(5*time.Hour).Unix()),
	} {
		if err := l.write(ctx, func(t *txn) error { _, err := t.ExecContext(t.ctx, q); return err }); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Prune(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var ids, rules int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM responses").Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM response_rules").Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if ids != 1 || rules != 1 {
		t.Errorf("after pruning: %d pages, %d rows of their rules, want 1 and 1 (the old and the future gone)", ids, rules)
	}
}

// When several sources have a page for a firing, the --responses file's is
// its page, even from a source that sorts before it and sent it sooner;
// among API sources, the nearest, as far apart the lesser source.
func TestRespondFilePageWins(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	o.Within = func(string) time.Duration { return 10 * time.Minute }
	at := t0.Add(24 * time.Hour)
	if err := l.Record(ctx, Found{From: t0, To: t0.Add(7 * 24 * time.Hour), Firings: []firing.Firing{firingOf("a", "shop", nil, at, 20)},
		Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(7 * 24 * time.Hour)}}, o); err != nil {
		t.Fatal(err)
	}
	// Acknowledged by an API source sorting before "file", nearer; the file's
	// page, later, was resolved by the alert manager: unanswered.
	if _, err := l.PutResponses(ctx, "aaa-pd", at, at.Add(time.Hour), []response.Page{page("A", "R", "a", at.Add(time.Minute), at.Add(2*time.Minute), time.Time{}, "")}, o); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SyncResponses(ctx, ResponsesFile, "x", []response.Page{page("F", "R", "a", at.Add(5*time.Minute), time.Time{}, at.Add(6*time.Minute), response.ByAlert)}, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["a"]; k.response != int(response.Unanswered) {
		t.Errorf("a nearer API page overrode the file's: %+v", k)
	}
	// Without the file's page the API's is the firing's.
	if _, err := l.SyncResponses(ctx, ResponsesFile, "y", nil, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["a"]; k.response != int(response.Acknowledged) {
		t.Errorf("the API source's page: %+v", k)
	}
	// Two API sources as far apart: the lesser source's, whatever order they were written in.
	for _, src := range []string{"zz-ops", "mm-ops"} {
		if _, err := l.PutResponses(ctx, src, at, at.Add(time.Hour), []response.Page{page("B", "R", "a", at.Add(time.Minute), time.Time{}, at.Add(2*time.Minute), response.ByAlert)}, o); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := l.ResponsesOf(ctx, []Firing{{ID: 1, Firing: firing.Firing{Rule: "R", Labels: map[string]string{"name": "a", "namespace": "shop"}, Start: at, End: at.Add(20 * time.Minute)}}}, o, at.Add(time.Hour))
	if err != nil || rs[1].Page == nil || rs[1].Page.Source != "aaa-pd" {
		t.Errorf("as far apart, the lesser source: %+v %v", rs[1].Page, err)
	}
}

// A deploy that starts after a firing began, with no time before deploys
// (--deploy-before 0), is a change in the firing: its response is judged
// again when the deploy arrives, though the deploy marks no firing before it.
// And a deploy taken away puts the firing back as it was.
func TestRespondDeployAfterFiringStart(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	f := firingOf("slow", "shop", map[string]string{"service": "cart"}, at, 60)
	if err := l.Record(ctx, Found{From: t0, To: t0.Add(7 * 24 * time.Hour), Firings: []firing.Firing{f},
		Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(7 * 24 * time.Hour)}}, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["slow"]; k.response != int(response.None) {
		t.Fatalf("before the deploy: %+v", k)
	}
	d := []Deploy{{Start: at.Add(10 * time.Minute), Labels: map[string]string{"service": "cart"}, Revision: "r2"}}
	if _, err := l.SyncDeploys(ctx, d, o, DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["slow"]; k.response != int(response.Changed) || k.useful != 1 {
		t.Errorf("a deploy that started after the firing did: %+v, want changed and useful", k)
	}
	if _, err := l.SyncDeploys(ctx, nil, o, DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["slow"]; k.response != int(response.None) {
		t.Errorf("the deploy taken away: %+v", k)
	}
}
