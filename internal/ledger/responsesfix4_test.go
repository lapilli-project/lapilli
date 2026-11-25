package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/response"
)

// The pages a judgement loads are those Index.Find, over every page, would
// choose among: the cut of a rule's pages falls where Find's does (sent time,
// id, then source), and the file's page is always read. 999 pages of one
// source, then two sent in the same second, an API source's "z" and the
// file's "a", both listing the firing: Find over all of them takes the file's
// (unanswered), and so does the judgement.
func TestPageCutIsFinds(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	var pd []response.Page
	for i := 0; i < 999; i++ {
		pd = append(pd, page(fmt.Sprintf("p%04d", i), "R", "other", at.Add(time.Duration(i)*time.Second), time.Time{}, time.Time{}, ""))
	}
	if _, err := l.PutResponses(ctx, "pd", at, at.Add(time.Hour), pd, o); err != nil {
		t.Fatal(err)
	}
	same := at.Add(999 * time.Second)
	if _, err := l.PutResponses(ctx, "am", at, at.Add(time.Hour), []response.Page{page("z", "R", "x", same, same.Add(time.Minute), time.Time{}, "")}, o); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutResponses(ctx, ResponsesFile, at, at.Add(time.Hour), []response.Page{page("a", "R", "x", same, time.Time{}, same.Add(time.Minute), response.ByAlert)}, o); err != nil {
		t.Fatal(err)
	}
	fs := []Firing{{ID: 1, Firing: firing.Firing{Rule: "R", Labels: map[string]string{"name": "x"}, Start: at, End: at.Add(20 * time.Minute)}}}
	got, err := l.ResponsesOf(ctx, fs, o, at.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if p := got[1].Page; p == nil || p.Source != ResponsesFile || got[1].Type != response.Unanswered {
		t.Errorf("response %+v page %+v, want the file's page, unanswered", got[1].Type, p)
	}
}

// A week of firings with windows apart is asked for in a few statements, not
// one for each window: 300 firings of one rule, hours apart, take at most 6
// (50 windows a statement), not 300.
func TestPageCutsAreBatched(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	var ws []pageWindow
	for i := 0; i < 300; i++ {
		s := t0.Add(time.Duration(i) * 3 * time.Hour).Unix()
		ws = append(ws, pageWindow{"R", s, s + 600})
	}
	before := l.pageCuts.Load()
	if _, err := l.pageKeys(ctx, l.r, ws); err != nil {
		t.Fatal(err)
	}
	if n := l.pageCuts.Load() - before; n > 6 {
		t.Errorf("%d statements for 300 windows, want at most 6", n)
	}
}

// pendedBy is the spans a run of pendTargets put back for ts, in a write of its
// own, and how many statements it took.
func pendedBy(t *testing.T, l *Ledger, path string, ts []changeTarget, hi int64) ([][2]int64, int64) {
	t.Helper()
	var got [][2]int64
	w := doing(t, l, path, func() {
		err := l.write(context.Background(), func(tx *txn) error {
			if err := l.pendTargets(tx, ts, hi); err != nil {
				return err
			}
			got = append(got, tx.pended...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	return got, w.statements
}

func recordFirings(t *testing.T, l *Ledger, fs ...firing.Firing) {
	t.Helper()
	to := t0.Add(48 * time.Hour)
	if err := l.Record(context.Background(), Found{From: t0, To: to, Firings: fs, Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: to}}, opts); err != nil {
		t.Fatal(err)
	}
}

// A deploy that lists no label is no firing's: its change puts back none of
// the week's firings (it would otherwise put back every firing of the week),
// while a deploy that lists a label puts back the firings that carry it. The
// week is read in a few statements: its series once each, then the starts of
// the series the change is made to.
func TestLabelessDeployPutsNothingBack(t *testing.T) {
	l, path := openWatched(t)
	at := t0.Add(24 * time.Hour)
	var fs []firing.Firing
	for i := 0; i < 20; i++ {
		fs = append(fs, firingOf(fmt.Sprint("n", i), "shop", nil, at.Add(time.Duration(i)*time.Hour), 5))
	}
	fs = append(fs, firingOf("other", "billing", nil, at, 5))
	recordFirings(t, l, fs...)
	hi := at.Add(36 * time.Hour).Unix()
	if got, _ := pendedBy(t, l, path, []changeTarget{{at: at.Unix(), deploy: true}}, hi); len(got) != 0 {
		t.Errorf("a deploy with no labels put back %d spans, want 0", len(got))
	}
	got, n := pendedBy(t, l, path, []changeTarget{{at: at.Unix(), deploy: true, labels: map[string]string{"namespace": "shop"}}}, hi)
	if len(got) == 0 || len(got) > 20 {
		t.Errorf("a deploy for the namespace put back %d spans, want those of its 20 firings (joined, at most 20)", len(got))
	}
	if n > 8 {
		t.Errorf("%d statements for a week's change, want a few", n)
	}
}

// A run that extends what is known of a workload's changes puts back the
// firings whose changes were unknown and now are, and not those that were
// known already: an ongoing firing is not judged again by every run.
func TestExtendedCoverPutsBackWhatBecameKnown(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(24 * time.Hour)
	recordFirings(t, l,
		firingOf("early", "shop", map[string]string{"deployment": "cart"}, at, 20),
		firingOf("late", "shop", map[string]string{"deployment": "cart"}, at.Add(3*time.Hour), 20))
	w := response.Workload{Namespace: "shop", Kind: "Deployment", Name: "cart"}
	keep := func(from, to time.Time) int {
		var n int
		err := l.write(ctx, func(tx *txn) error {
			r := &response.Rollouts{Coverage: map[response.Workload][]response.Span{w: {{From: from, To: to}}}}
			if err := l.keepRollouts(tx, r, to.Unix(), at.Add(10*time.Hour).Unix()); err != nil {
				return err
			}
			n = len(tx.pended)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// First known over the first hours: the early firing is put back.
	if n := keep(at.Add(-time.Hour), at.Add(time.Hour)); n == 0 {
		t.Errorf("a first read put back nothing")
	}
	// Extended to cover the late one too: that one only (the early one was known).
	got := func() [][2]int64 {
		var s [][2]int64
		err := l.write(ctx, func(tx *txn) error {
			r := &response.Rollouts{Coverage: map[response.Workload][]response.Span{w: {{From: at.Add(-time.Hour), To: at.Add(4 * time.Hour)}}}}
			if err := l.keepRollouts(tx, r, at.Add(4*time.Hour).Unix(), at.Add(10*time.Hour).Unix()); err != nil {
				return err
			}
			s = append(s, tx.pended...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}()
	if len(got) != 1 || got[0][0] != at.Add(3*time.Hour).Unix() {
		t.Errorf("an extension put back %v, want the late firing's start only", got)
	}
	// And a longer read again that adds nothing about either: nothing.
	if n := keep(at.Add(-time.Hour), at.Add(5*time.Hour)); n != 0 {
		t.Errorf("an extension that made no firing known put back %d spans", n)
	}
}
