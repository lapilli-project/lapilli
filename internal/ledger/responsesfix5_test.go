package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/response"
)

// A week whose pages are more than a judgement holds at once is judged in
// batches, and every firing gets the page it would with no cap: none is left
// without for the rule it is of (the cap used to skip windows in rule-name
// order and save the firings as unknown). 6 rules of 8 firings, hours apart,
// each with 10 pages near it (480 pages), judged under a cap of 25.
func TestBatchedJudgementMatchesUncapped(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	var pages []response.Page
	var fs []Firing
	id := int64(0)
	for r := 0; r < 6; r++ {
		rule := fmt.Sprintf("Rule%d", r)
		for i := 0; i < 8; i++ {
			start := at.Add(time.Duration(i) * 3 * time.Hour)
			name := fmt.Sprintf("%s-%d", rule, i)
			for k := 0; k < 10; k++ {
				n := fmt.Sprintf("%s-decoy%d", name, k)
				if k == 4 {
					n = name
				}
				pages = append(pages, page(fmt.Sprintf("%s-p%d", name, k), rule, n, start.Add(time.Duration(k)*10*time.Second), start.Add(time.Minute), time.Time{}, ""))
			}
			id++
			fs = append(fs, Firing{ID: id, Firing: firing.Firing{Rule: rule, Labels: map[string]string{"name": name}, Start: start, End: start.Add(20 * time.Minute)}})
		}
	}
	if _, err := l.PutResponses(ctx, "pd", at, at.Add(30*time.Hour), pages, o); err != nil {
		t.Fatal(err)
	}
	now := at.Add(31 * time.Hour)
	want, err := l.ResponsesOf(ctx, fs, o, now)
	if err != nil {
		t.Fatal(err)
	}
	old := judgePagesCap
	judgePagesCap = 25
	defer func() { judgePagesCap = old }()
	before := l.pagesRead.Load()
	got, err := l.ResponsesOf(ctx, fs, o, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		w, g := want[f.ID], got[f.ID]
		if w.Page == nil {
			t.Fatalf("firing %d has no page uncapped", f.ID)
		}
		if g.Page == nil || g.Page.ID != w.Page.ID || g.Type != w.Type {
			t.Errorf("firing %d (%s) capped: %+v %v, uncapped: %s %v", f.ID, f.Rule, g.Page, g.Type, w.Page.ID, w.Type)
		}
	}
	// No more than a batch's pages at once: 480 pages in all are read, a
	// firing's 10 at a time at most, never twice over the cap.
	if n := l.pagesRead.Load() - before; n < 480 || n > 480*2 {
		t.Errorf("%d pages read in batches, want the 480 once or a little over", n)
	}
}

// A cut reads the first MaxFindScan pages of a rule from a firing's start, as
// Index.Find does, whatever their source: 1,100 pages of an API source and
// then 1,100 of the file's in a window have a cut of MaxFindScan+1 entries,
// none of the file's among them, and the page only a file's later one lists is
// not found, as Find over every page does not find it.
func TestCutReadsOnlyWhatFindScans(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	var api, file, all []response.Page
	for i := 0; i < 1100; i++ {
		api = append(api, page(fmt.Sprintf("a%04d", i), "R", fmt.Sprintf("x%d", i), at.Add(time.Duration(i)*time.Second), time.Time{}, time.Time{}, ""))
		file = append(file, page(fmt.Sprintf("f%04d", i), "R", fmt.Sprintf("y%d", i), at.Add(time.Duration(1100+i)*time.Second), time.Time{}, time.Time{}, ""))
	}
	for _, p := range []struct {
		src   string
		pages []response.Page
	}{{"pd", api}, {ResponsesFile, file}} {
		if _, err := l.PutResponses(ctx, p.src, at, at.Add(time.Hour), p.pages, o); err != nil {
			t.Fatal(err)
		}
		for _, pg := range p.pages {
			pg.Source = p.src
			all = append(all, pg)
		}
	}
	cut := pageWindow{"R", at.Unix(), at.Add(time.Hour).Unix()}
	var ks []pageKey
	if err := l.eachCut(ctx, l.r, []pageWindow{cut}, response.MaxFindScan+1, func(_ int, k []pageKey) { ks = k }); err != nil {
		t.Fatal(err)
	}
	if len(ks) != response.MaxFindScan+1 {
		t.Fatalf("a cut of %d entries, want %d", len(ks), response.MaxFindScan+1)
	}
	for i, k := range ks {
		if k.source != "pd" || k.id != fmt.Sprintf("a%04d", i) {
			t.Fatalf("entry %d is %+v, want the API's pages in Find's order", i, k)
		}
	}
	ix := response.NewIndex(all)
	fs := []Firing{
		{ID: 1, Firing: firing.Firing{Rule: "R", Labels: map[string]string{"name": "y5"}, Start: at, End: at.Add(40 * time.Minute)}},
		{ID: 2, Firing: firing.Firing{Rule: "R", Labels: map[string]string{"name": "x7"}, Start: at, End: at.Add(40 * time.Minute)}},
	}
	got, err := l.ResponsesOf(ctx, fs, o, at.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		w := ix.Find("R", f.Labels, f.Start, f.End.Add(2*time.Minute))
		g := got[f.ID].Page
		if (w == nil) != (g == nil) || (w != nil && w.Key() != g.Key()) {
			t.Errorf("firing %d: the ledger found %+v, Find over every page %+v", f.ID, g, w)
		}
	}
	if got[1].Page != nil {
		t.Errorf("a file's page past the first %d was found: %+v", response.MaxFindScan, got[1].Page)
	}
}
