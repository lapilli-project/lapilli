package verdict

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/routing"
)

// pagerConfig sends everything to one PagerDuty receiver, repeating each 5
// minutes.
func pagerConfig(t *testing.T) *routing.Config {
	t.Helper()
	c, err := routing.NewConfig(&routing.Route{Receiver: "pager", GroupWait: ndur(10 * time.Second), GroupInterval: ndur(time.Minute), RepeatInterval: ndur(5 * time.Minute)},
		[]routing.Receiver{nrecv("pager", "pagerduty")}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// PagerDuty keeps one incident for a group: a firing an hour long with a
// repeat each five minutes pages once; the repeats are on a paging
// integration, and both numbers are said.
func TestPagerDutyPagesOnce(t *testing.T) {
	r := nreport(nfiring("A", 0, time.Hour, false))
	if err := r.WorkOutNotifications(NotificationOptions{Config: pagerConfig(t), Sent: true}); err != nil {
		t.Fatal(err)
	}
	n := r.Notifications
	if n.Paged != 1 || n.OnPaging < 10 || n.Notified != n.OnPaging {
		t.Fatalf("report: %+v", n)
	}
	f := r.Firings[0].Notifications
	if f.Paged != 1 || f.OnPaging != n.OnPaging || len(f.Sent) != n.OnPaging && len(f.Sent) != MaxSent {
		t.Fatalf("firing: %+v", f)
	}
	pages := 0
	for _, s := range f.Sent {
		if s.Page {
			pages++
			if s.Reason != "first" || !s.Paging || s.Kind != "pagerduty" {
				t.Errorf("%+v", s)
			}
		}
	}
	if pages != 1 {
		t.Errorf("%d pages listed", pages)
	}
	var b bytes.Buffer
	Text(&b, r, time.UTC)
	if !strings.Contains(b.String(), fmt.Sprintf("1 of them pages (%d on a paging integration)", n.OnPaging)) {
		t.Errorf("text lacks both numbers:\n%s", b.String())
	}
}

// What stopped short is never zero pages: the text, the report, the
// comparison's first line, summary and Markdown say not known, and a budget
// of pages is neither held nor broken.
func TestStoppedIsNotKnown(t *testing.T) {
	cfg := pagerConfig(t)
	var fs []Judged
	for i := range 200 {
		fs = append(fs, nfiring(fmt.Sprint("R", i), 0, 2*time.Hour, false))
	}
	stopped := func() *Report {
		r := nreport(fs...)
		if err := r.WorkOutNotifications(NotificationOptions{Config: cfg, Run: routing.Options{Budget: 1000}}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	whole := nreport(fs[:3]...)
	if err := whole.WorkOutNotifications(NotificationOptions{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	r := stopped()
	if n := r.Notifications; n.Known || !strings.Contains(n.Stopped, "budget of 1000") {
		t.Fatalf("report: %+v", n)
	}
	var b bytes.Buffer
	Text(&b, r, time.UTC)
	if !strings.Contains(b.String(), "not known: the work-out stopped: it spent its budget") || strings.Contains(b.String(), "of them pages (") {
		t.Errorf("text:\n%s", b.String())
	}
	b.Reset()
	JSON(&b, r)
	if !strings.Contains(b.String(), `"known": false`) || !strings.Contains(b.String(), `"stopped": "it spent`) {
		t.Errorf("json lacks it")
	}
	c := Compare(whole, r, []string{"R0"}, 0)
	c.From, c.To = nt0, nt0.Add(7*24*time.Hour)
	c.Summarize(false)
	if s := c.Summary.Notifications; s == nil || s.Known || !strings.Contains(s.Stopped, "budget") {
		t.Fatalf("summary: %+v", s)
	}
	b.Reset()
	CompareText(&b, c, time.UTC)
	if first := strings.Split(b.String(), "\n")[1]; !strings.HasPrefix(first, "Pages not known: the work-out stopped: it spent") {
		t.Errorf("text's first line: %q", first)
	}
	b.Reset()
	CompareMarkdown(&b, c, time.UTC)
	for _, want := range []string{"**Pages not known: the work-out stopped:", "| Pages, worked out | not known | not known |", `"known":false`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, b.String())
		}
	}
	// A budget of pages is not known on either side.
	c2 := Compare(whole, r, []string{"R0"}, 0)
	c2.From, c2.To = nt0, nt0.Add(7*24*time.Hour)
	k := check(t, c2, readExpect(t, "expectations:\n  - rule: R0\n    budget: {pages: 0}\n"), nil, nil)
	if x := k.Results[0]; x.BeforeState != ExpectUnknown || x.AfterState != ExpectUnknown {
		t.Errorf("a budget of pages beside a work-out that stopped: %+v", x)
	}
}

// An inhibited firing is one no flush of which got through, never notified;
// one held at the first flush and sent after isn't, and each firing weighs
// once in inhibited_by.
func TestInhibitedIsNeverNotified(t *testing.T) {
	cfg := nconfig(t) // Down inhibits B
	r := nreport(
		nfiring("Down", 0, 30*time.Minute, false),
		nfiring("B", time.Minute, 20*time.Minute, false, "n", "1"), // inhibited all through
		nfiring("B", time.Minute, 40*time.Minute, false, "n", "2"), // sent after Down resolved
	)
	if err := r.WorkOutNotifications(NotificationOptions{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	rb := nrule(r, "B").Notifications
	if rb.Inhibited != 1 || rb.InhibitedBy != "Down" {
		t.Errorf("rule B: %+v", rb)
	}
}

// The firings of a rule left alone take part, and are counted in nothing.
func TestAlsoFiringsAreUncounted(t *testing.T) {
	cfg := nconfig(t)
	r := nreport(nfiring("B", time.Minute, 20*time.Minute, false))
	also := nfiring("Down", 0, 30*time.Minute, false)
	if err := r.WorkOutNotifications(NotificationOptions{Config: cfg, Also: []firing.Firing{also.Firing}}); err != nil {
		t.Fatal(err)
	}
	if rb := nrule(r, "B").Notifications; rb.Inhibited != 1 || rb.InhibitedBy != "Down" {
		t.Errorf("B isn't inhibited by a rule left alone: %+v", rb)
	}
	if r.Notifications.Notified != 0 || len(r.Rules) != 1 {
		t.Errorf("the left alone rule is counted: %+v", r.Notifications)
	}
}

func TestParseBudgetOnePage(t *testing.T) {
	for _, s := range []string{"1 page", "1 pages", "1 page, 2/week"} {
		if b, err := ParseBudget(s); err != nil || b.Pages == nil || *b.Pages != 1 {
			t.Errorf("%q: %+v %v", s, b, err)
		}
	}
}
