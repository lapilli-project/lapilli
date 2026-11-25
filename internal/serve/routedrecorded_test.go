package serve

import (
	"context"
	"regexp"
	"testing"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/routing"
)

// The recorded rule for a page is the model's: for every kind, reason and
// whether --paging-receivers matched the receiver, a recorded notification is
// a page exactly where the model says its worked-out copy is.
func TestRecordedPageRuleIsTheModels(t *testing.T) {
	parallel(t)
	const am = `route: {receiver: r}
receivers:
  - name: pd
    pagerduty_configs: [{routing_key: k}]
  - name: po
    pushover_configs: [{user_key: u, token: t}]
  - name: sl
    slack_configs: [{channel: '#x'}]
  - name: wh
    webhook_configs: [{url: 'http://x'}]
  - name: ops
    opsgenie_configs: [{api_key: k}]
  - name: r
`
	cfg, err := content.ReadAlertmanagerRouting([]byte(am))
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []*regexp.Regexp{nil, regexp.MustCompile(`^(pd|sl|wh|ops)$`)} {
		pages := pagingOfRecorded(cfg, re, false)
		for _, recv := range []struct{ name, kind string }{{"pd", "pagerduty"}, {"po", "pushover"}, {"sl", "slack"}, {"wh", "webhook"}, {"ops", "opsgenie"}} {
			for _, reason := range []routing.Reason{routing.ReasonFirst, routing.ReasonNew, routing.ReasonRepeat, routing.ReasonResolved} {
				byName := re != nil && re.MatchString(recv.name)
				_, want := routing.PageRule(recv.kind, false, byName, reason != routing.ReasonResolved, reason)
				// By the receiver's name alone, and by its integration.
				for _, name := range []string{recv.name, recv.name + "/" + recv.kind, recv.name + "/" + recv.kind + "[0]"} {
					if got := pages(name, string(reason), reason == routing.ReasonResolved); got != want {
						t.Errorf("%s %s (paging-receivers %v): recorded says %v, the model %v", name, reason, re, got, want)
					}
				}
			}
		}
	}
	// The case that differed: a repeat to PagerDuty rings where
	// --paging-receivers says its receiver pages for each notification.
	re := regexp.MustCompile(`^pd$`)
	if !pagingOfRecorded(cfg, re, false)("pd/pagerduty", ledger.ReasonRepeat, false) || pagingOfRecorded(cfg, nil, false)("pd/pagerduty", ledger.ReasonRepeat, false) {
		t.Error("a repeat to a dedup kind: a page by --paging-receivers alone")
	}
}

// Notifications recorded since before the first hour worked out are counted
// as recorded: those hours are known, and an hour after the first one tried
// that wasn't worked out and has no notification recorded is not.
func TestKnownRecordedBeforeCover(t *testing.T) {
	h := func(n int) time.Time {
		return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)
	}
	p := &paged{
		routed: true, first: h(0), routedFrom: h(48), upTo: h(100),
		cover:         ledger.RoutedCover{{From: h(48), To: h(60), Worked: true}, {From: h(60), To: h(62), Worked: false, Why: "stopped"}, {From: h(62), To: h(100), Worked: true}},
		recordedHours: map[int64]bool{h(10).Unix(): true},
	}
	if !p.known(h(0), h(48)) {
		t.Error("the hours of the recording before the cover began read not known")
	}
	if !p.known(h(0), h(60)) || !p.known(h(62), h(100)) {
		t.Error("recorded hours before the cover, and worked out hours, read not known")
	}
	if p.known(h(48), h(100)) {
		t.Error("an hour that wasn't worked out, with nothing recorded in it, is known")
	}
	// A notification recorded in each of its hours makes it known.
	p.recordedHours[h(60).Unix()], p.recordedHours[h(61).Unix()] = true, true
	if !p.known(h(0), h(100)) {
		t.Error("an hour not worked out but recorded in reads not known")
	}
	// With none recorded, the cover is all there is: counted from its start,
	// a gap in it is not known.
	q := &paged{routed: true, routedFrom: h(48), upTo: h(100), cover: p.cover}
	if q.known(h(0), h(100)) {
		t.Error("a gap with no recording is known")
	}
	if !q.known(h(0), h(60)) {
		t.Error("the hours before it are not counted, so they are not in question")
	}
}

// The hours recorded are read for the pages: a server whose recorder began
// before its first work-out reads them as known.
func TestKnownRecordedHoursLoaded(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	fs, _, _ := s.l.FiringsOverlapping(ctx, now.Add(-4*24*time.Hour), now, now.Add(-4*24*time.Hour), 1000)
	var f ledger.Firing
	for _, x := range fs {
		if x.Rule == "Errors" {
			f = x
			break
		}
	}
	h := f.Start.Truncate(time.Hour)
	if err := s.l.MarkRoutedUnknown(ctx, h, h.Add(time.Hour), "a test's failure"); err != nil {
		t.Fatal(err)
	}
	p, err := s.notified(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.known(h, h.Add(time.Hour)) {
		t.Fatal("an hour not worked out, none recorded, reads known")
	}
	if _, err := s.l.Notify(ctx, ledger.Notification{At: h.Add(time.Minute), Source: "alertmanager-events", Receiver: "oncall/pagerduty", Group: "g", Reason: ledger.ReasonFirst,
		Alerts: []ledger.Alert{{Rule: "Errors", Labels: map[string]string{"alertname": "Errors", "service": f.Labels["service"], "severity": "page"}, Start: f.Start}}}); err != nil {
		t.Fatal(err)
	}
	if p, err = s.notified(ctx); err != nil || !p.known(h, h.Add(time.Hour)) {
		t.Errorf("an hour not worked out with a notification recorded in it reads not known: %v", err)
	}
}
