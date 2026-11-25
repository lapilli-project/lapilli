package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/response"
)

func bigPage(id, rule string, sent time.Time, pad int) response.Page {
	return response.Page{ID: id, Sent: sent, Alerts: []incident.Alert{{Rule: rule, Labels: map[string]string{"name": id, "note": strings.Repeat("x", pad)}}}}
}

// A judgement reads the pages near its firings, not every page of their rules
// over the days they span. With a page a minute for 14 days (20,160 pages of
// 1 KiB of alerts) and two firings 12 days apart, judging both reads the
// pages in their two windows (11 each, a 2-minute "within" past a 10-minute
// firing) and allocates under 2 MiB, where it read the whole span. And with
// 5,000 pages in one firing's window, no more than MaxFindScan of them are
// read for it.
func TestPagesReadNearTheirFirings(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	day := 24 * time.Hour
	var pages []response.Page
	for i := 0; i < 14*24*60; i++ {
		pages = append(pages, bigPage(fmt.Sprint("m", i), "R", t0.Add(time.Duration(i)*time.Minute), 1024))
		if len(pages) == 5000 || i == 14*24*60-1 {
			from := pages[0].Sent
			if _, err := l.PutResponses(ctx, "pd", from, pages[len(pages)-1].Sent.Add(time.Minute), pages, o); err != nil {
				t.Fatal(err)
			}
			pages = nil
		}
	}
	a, b := t0.Add(day+3*time.Hour), t0.Add(13*day+3*time.Hour)
	fs := []respFiring{
		{ID: 1, Rule: "R", Labels: map[string]string{"name": "m1"}, Start: a, End: a.Add(10 * time.Minute)},
		{ID: 2, Rule: "R", Labels: map[string]string{"name": "m2"}, Start: b, End: b.Add(10 * time.Minute)},
	}
	measure := func(fs []respFiring) (uint64, int64, *responder) {
		before := l.pagesRead.Load()
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		r, err := l.newResponder(ctx, l.r, l.r, o, fs, b.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		err = r.eachResponse(fs, func(respFiring, response.Response, response.Usefulness) error { return nil })
		runtime.ReadMemStats(&m1)
		if err != nil {
			t.Fatal(err)
		}
		return m1.TotalAlloc - m0.TotalAlloc, l.pagesRead.Load() - before, r
	}
	alloc, n, _ := measure(fs)
	if n > 40 || alloc > 2<<20 {
		t.Errorf("two firings 12 days apart read %d pages and allocated %d bytes, want at most 40 and 2 MiB", n, alloc)
	}

	// Dense: 5,000 pages in the window of one firing.
	var dense []response.Page
	c := t0.Add(20 * day)
	for i := 0; i < 5000; i++ {
		dense = append(dense, bigPage(fmt.Sprint("d", i), "Dense", c.Add(time.Duration(i)*100*time.Millisecond), 16))
	}
	// Sent seconds apart: a page a second for 5,000 seconds, in the firing's 2 hours.
	for i := range dense {
		dense[i].Sent = c.Add(time.Duration(i) * time.Second)
	}
	if _, err := l.PutResponses(ctx, "pd", c, c.Add(2*time.Hour), dense, o); err != nil {
		t.Fatal(err)
	}
	f := []respFiring{{ID: 3, Rule: "Dense", Labels: map[string]string{"name": "d1"}, Start: c, End: c.Add(2 * time.Hour)}}
	if _, n, _ := measure(f); n > response.MaxFindScan {
		t.Errorf("5,000 pages in one window: %d read, want at most %d", n, response.MaxFindScan)
	}
}

// A firing a quiet window excused has no response and asks for no page: the
// pages of its rule are not read for it.
func TestResponsesOfReadsNothingForExcused(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	var pages []response.Page
	for i := 0; i < 50; i++ {
		pages = append(pages, pageFor(fmt.Sprint("p", i), "R", at.Add(time.Duration(i)*time.Second)))
	}
	if _, err := l.PutResponses(ctx, "pd", at, at.Add(time.Hour), pages, o); err != nil {
		t.Fatal(err)
	}
	fs := []Firing{{ID: 1, Excused: true, Firing: firing.Firing{Rule: "R", Labels: map[string]string{"name": "p1"}, Start: at, End: at.Add(time.Minute)}}}
	before := l.pagesRead.Load()
	got, err := l.ResponsesOf(ctx, fs, o, at.Add(time.Hour))
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if n := l.pagesRead.Load() - before; n != 0 {
		t.Errorf("%d pages read for an excused firing", n)
	}
}

// A span read again, found to have no change, makes the firings in it whose
// targets it covers judged again: an acknowledged firing whose changes were
// not known (the read failed) is not useful once they are known to be none.
func TestRetryJudgesFiringsAgain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	f := firingOf("a", "shop", map[string]string{"deployment": "cart"}, at, 20)
	if err := l.Record(ctx, Found{From: t0, To: at.Add(time.Hour), Firings: []firing.Firing{f},
		Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: at.Add(time.Hour)}}, o); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutResponses(ctx, "pd", at, at.Add(time.Hour), []response.Page{page("P", "R", "a", at.Add(time.Minute), at.Add(2*time.Minute), time.Time{}, "")}, o); err != nil {
		t.Fatal(err)
	}
	if err := l.RolloutFailed(ctx, at.Add(-time.Hour), at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["a"]; k.response != int(response.Acknowledged) || k.useful != -1 {
		t.Fatalf("with the read failed: %+v, want acknowledged and unknown", k)
	}
	w := response.Workload{Namespace: "shop", Kind: "Deployment", Name: "cart"}
	read := &response.Rollouts{Coverage: map[response.Workload][]response.Span{w: {{From: at.Add(-time.Hour), To: at.Add(2 * time.Hour)}}}}
	if err := l.KeepRollouts(ctx, read, at.Add(2*time.Hour), o); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["a"]; k.response != int(response.Acknowledged) || k.useful != 0 {
		t.Errorf("after the retry found no change: %+v, want acknowledged and not useful", k)
	}
}

// A new rollout puts back the firings of its workload and no others': in
// a namespace of 200 workloads with firings in a week, the rollout of one
// (whose firings are a day 1 and a day 5) leaves pending those two and not
// the third day of everyone's.
func TestNewRolloutPendsItsWorkloadsFiringsOnly(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	day := 24 * time.Hour
	var fs []firing.Firing
	for i := 0; i < 200; i++ {
		name := fmt.Sprint("d", i)
		lb := map[string]string{"name": name, "namespace": "shop", "deployment": name}
		if i == 7 {
			fs = append(fs, firing.Firing{Rule: "R", Labels: lb, Start: t0.Add(day + 3*time.Hour), End: t0.Add(day + 4*time.Hour)},
				firing.Firing{Rule: "R", Labels: lb, Start: t0.Add(5*day + 3*time.Hour), End: t0.Add(5*day + 6*time.Hour)})
			continue
		}
		fs = append(fs, firing.Firing{Rule: "R", Labels: lb, Start: t0.Add(3*day + 3*time.Hour), End: t0.Add(3*day + 4*time.Hour)})
	}
	if err := l.Record(ctx, Found{From: t0, To: t0.Add(6 * day), Firings: fs, Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(6 * day)}}, o); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	r := &response.Rollouts{Changes: []response.Change{{Kind: response.Rollout, At: t0.Add(5*day + 5*time.Hour), Namespace: "shop", WorkloadKind: "Deployment", Workload: "d7"}}}
	seen := t0.Add(6 * day).Unix()
	if err := l.write(ctx, func(t *txn) error { return l.keepRollouts(t, r, seen, seen) }); err != nil {
		t.Fatal(err)
	}
	v, err := l.meta(ctx, l.r, "pending")
	if err != nil {
		t.Fatal(err)
	}
	var spans [][2]int64
	if err := json.Unmarshal([]byte(v), &spans); err != nil {
		t.Fatalf("pending %q: %v", v, err)
	}
	covers := func(at time.Time) bool {
		for _, s := range spans {
			if s[0] <= at.Unix() && at.Unix() <= s[1] {
				return true
			}
		}
		return false
	}
	if !covers(t0.Add(5*day+3*time.Hour)) || !covers(t0.Add(day+3*time.Hour)) || covers(t0.Add(3*day+3*time.Hour)) {
		t.Errorf("pending %v: want d7's two firings and not everyone's on day 3", spans)
	}
}
