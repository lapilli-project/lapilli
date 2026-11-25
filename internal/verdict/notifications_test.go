package verdict

import (
	"bytes"
	"encoding/csv"
	"regexp"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/routing"
	"lapilli.dev/internal/rules"
)

var nt0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC) // a Monday, at work

func nmatcher(t *testing.T, name, op, value string) routing.Matcher {
	t.Helper()
	m, err := routing.NewMatcher(name, op, value)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ndur(d time.Duration) *time.Duration { return &d }

func nrecv(name string, kinds ...string) routing.Receiver {
	r := routing.Receiver{Name: name}
	for i, k := range kinds {
		r.Integrations = append(r.Integrations, routing.Integration{Kind: k, Index: i, SendResolved: routing.DefaultSendResolved(k)})
	}
	return r
}

// A configuration: A and B go to the pager (pagerduty), grouped together;
// everything else to a channel (slack); Down inhibits B.
func nconfig(t *testing.T) *routing.Config {
	t.Helper()
	c, err := routing.NewConfig(&routing.Route{
		Receiver: "channel", GroupBy: nil, GroupBySet: true, GroupWait: ndur(10 * time.Second), GroupInterval: ndur(time.Minute), RepeatInterval: ndur(5 * time.Minute),
		Routes: []*routing.Route{{Receiver: "pager", Matchers: []routing.Matcher{nmatcher(t, "alertname", "=~", "A|B")}}},
	}, []routing.Receiver{nrecv("channel", "slack"), nrecv("pager", "pagerduty")}, nil,
		[]routing.InhibitRule{{Name: "down-hides-b", SourceMatchers: []routing.Matcher{nmatcher(t, "alertname", "=", "Down")}, TargetMatchers: []routing.Matcher{nmatcher(t, "alertname", "=", "B")}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustRE(t *testing.T, s string) *regexp.Regexp {
	t.Helper()
	return regexp.MustCompile(s)
}

func nfiring(rule string, start, end time.Duration, white bool, kv ...string) Judged {
	f := firing.Firing{Rule: rule, Labels: map[string]string{}, Start: nt0.Add(start)}
	if end != 0 {
		f.End = nt0.Add(end)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		f.Labels[kv[i]] = kv[i+1]
	}
	return Judged{Firing: f, White: white}
}

func nreport(fs ...Judged) *Report {
	r := &Report{From: nt0.Add(-time.Hour), To: nt0.Add(24 * time.Hour), Firings: fs, Rules: []RuleResult{}}
	seen := map[string]int{}
	for _, f := range fs {
		i, ok := seen[f.Rule]
		if !ok {
			i = len(r.Rules)
			seen[f.Rule] = i
			r.Rules = append(r.Rules, RuleResult{Rule: f.Rule, Replayed: true})
		}
		r.Rules[i].Firings++
		if f.White {
			r.Rules[i].White++
		} else {
			r.Rules[i].Black++
		}
	}
	return r
}

func nrule(r *Report, name string) RuleResult {
	for _, rr := range r.Rules {
		if rr.Rule == name {
			return rr
		}
	}
	return RuleResult{}
}

// The alerts are the firings with alertname, the rule's name as written
// (not the one that told it apart from another), a Grafana rule's folder,
// and their ends: zero for one still firing.
func TestNotificationAlerts(t *testing.T) {
	fs := []Judged{
		nfiring("Errors [shop]", 0, time.Minute, true, "service", "shop"),
		nfiring("Disk", time.Minute, 0, false, "alertname", "DiskFull"),
		nfiring("Folded", 2*time.Minute, time.Hour, false),
		nfiring("Unknown", 0, 0, false),
	}
	rs := []rules.Rule{{Name: "Errors [shop]", Alert: "Errors"}, {Name: "Folded", Folder: "Team A"}, {Name: "Disk"}}
	got := NotificationAlerts(fs, rs)
	if len(got) != 4 {
		t.Fatalf("%d alerts", len(got))
	}
	for i, want := range []map[string]string{
		{"alertname": "Errors", "service": "shop"},
		{"alertname": "DiskFull"},
		{"alertname": "Folded", "grafana_folder": "Team A"},
		{"alertname": "Unknown"},
	} {
		if len(got[i].Labels) != len(want) {
			t.Errorf("alert %d: labels %v, want %v", i, got[i].Labels, want)
		}
		for k, v := range want {
			if got[i].Labels[k] != v {
				t.Errorf("alert %d: %s = %q, want %q", i, k, got[i].Labels[k], v)
			}
		}
	}
	if !got[1].End.IsZero() || !got[0].End.Equal(nt0.Add(time.Minute)) || !got[0].Start.Equal(nt0) {
		t.Errorf("times: %v %v", got[0], got[1])
	}
	// The firings' own labels aren't changed.
	if _, ok := fs[0].Labels["alertname"]; ok {
		t.Error("the firing's labels were written to")
	}
}

// Per firing and per rule: what each reached and what muted it; B is
// inhibited by Down while it fires, then sent, in a notification with A's
// alert; pages for nothing count the black firings' pages, those while people
// slept and off hours apart.
func TestWorkOutNotifications(t *testing.T) {
	cfg := nconfig(t)
	r := nreport(
		nfiring("A", 0, 10*time.Minute, true),
		nfiring("B", time.Minute, 10*time.Minute, false),
		nfiring("Down", 0, 3*time.Minute, false),
		nfiring("A", 17*time.Hour, 17*time.Hour+time.Minute, false), // 03:00 the next day: asleep
	)
	hours := DefaultHours(time.UTC)
	if err := r.WorkOutNotifications(NotificationOptions{Config: cfg, Hours: hours}); err != nil {
		t.Fatal(err)
	}
	n := r.Notifications
	if n == nil || !n.WorkedOut || n.Groups != 2 {
		t.Fatalf("report: %+v", n)
	}
	// Pager: A and B in a group, then A at 03:00 (its own group, the first
	// is gone); channel: Down.
	if n.Paged < 3 || n.Notified <= n.Paged {
		t.Errorf("notified %d, paged %d", n.Notified, n.Paged)
	}
	if len(n.Receivers) != 2 || n.Receivers[1].Name != "pager" || !n.Receivers[1].Paging || n.Receivers[1].Why != "pages by its pagerduty" ||
		n.Receivers[0].Paging || n.Receivers[0].Why != "messages only (slack)" {
		t.Errorf("receivers: %+v", n.Receivers)
	}
	b := r.Firings[1].Notifications
	if b == nil || len(b.Muted) != 1 || b.Muted[0].Kind != "inhibition" || b.Muted[0].By != "down-hides-b" || b.Muted[0].SourceRule != "Down" {
		t.Fatalf("B: %+v", b)
	}
	if b.Notified == 0 || b.Paged != b.Notified || b.FirstPage.Before(nt0.Add(3*time.Minute)) {
		t.Errorf("B sent after Down resolved: %+v", b)
	}
	rb := nrule(r, "B").Notifications
	if rb.Inhibited != 1 || rb.InhibitedBy != "Down" || rb.Grouped != 1 || rb.PagedForNothing != rb.Paged {
		t.Errorf("rule B: %+v", rb)
	}
	ra := nrule(r, "A").Notifications
	if ra.Grouped != 1 || ra.Inhibited != 0 {
		t.Errorf("rule A grouped with B's alert once: %+v", ra)
	}
	// A's second firing, at 03:00, is black and asleep: its one page is for
	// nothing and asleep; the first firing's pages were white, at work.
	if second := r.Firings[3].Notifications; second.Paged != 1 || ra.Paged != r.Firings[0].Notifications.Paged+1 ||
		ra.PagedForNothing != 1 || ra.PagedForNothingAsleep != 1 || ra.PagedAsleep != 1 || ra.PagedOffHours != 0 {
		t.Errorf("rule A: %+v", ra)
	}
	rd := nrule(r, "Down").Notifications
	if rd.Notified != 1 || rd.Paged != 0 || rd.PagedForNothing != 0 {
		t.Errorf("rule Down (a message): %+v", rd)
	}
	// No hours: the pages are counted, not by when.
	r2 := nreport(nfiring("A", 17*time.Hour, 17*time.Hour+time.Minute, false))
	if err := r2.WorkOutNotifications(NotificationOptions{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if x := nrule(r2, "A").Notifications; x.Paged != 1 || x.PagedForNothing != 1 || x.PagedAsleep != 0 || x.PagedForNothingAsleep != 0 {
		t.Errorf("no hours: %+v", x)
	}
}

// With responses read, a page for nothing is one of a firing that wasn't
// useful, whatever it caught; a firing whose use isn't known counts for
// nothing.
func TestPagesForNothingByResponses(t *testing.T) {
	cfg := nconfig(t)
	yes, no := true, false
	a := nfiring("A", 0, time.Minute, false)
	a.Response = &FiringResponse{Kind: ResponseChanged, Useful: &yes}
	b := nfiring("A", 2*time.Hour, 2*time.Hour+time.Minute, true)
	b.Response = &FiringResponse{Kind: ResponseUnanswered, Useful: &no}
	c := nfiring("A", 4*time.Hour, 4*time.Hour+time.Minute, false)
	c.Response = &FiringResponse{Kind: ResponseNone}
	r := nreport(a, b, c)
	r.Responses = &ResponseTotals{}
	if err := r.WorkOutNotifications(NotificationOptions{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if x := nrule(r, "A").Notifications; x.Paged != 3 || x.PagedForNothing != 1 {
		t.Errorf("by responses: %+v", x)
	}
	// Without them, the black ones.
	r.Responses = nil
	if err := r.WorkOutNotifications(NotificationOptions{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if x := nrule(r, "A").Notifications; x.PagedForNothing != 2 {
		t.Errorf("by stones: %+v", x)
	}
}

// --paging-receivers makes a message a page, and the report says why.
func TestPagingReceiversPattern(t *testing.T) {
	cfg := nconfig(t)
	r := nreport(nfiring("Down", 0, time.Minute, false))
	err := r.WorkOutNotifications(NotificationOptions{Config: cfg, Run: routing.Options{PagingReceivers: mustRE(t, "^chan")}})
	if err != nil {
		t.Fatal(err)
	}
	if x := nrule(r, "Down").Notifications; x.Paged != 1 {
		t.Errorf("paged %d", x.Paged)
	}
	for _, rc := range r.Notifications.Receivers {
		if rc.Name == "channel" && (!rc.Paging || rc.Why != "pages by --paging-receivers") {
			t.Errorf("%+v", rc)
		}
	}
}

// The text: the table, what it says it is, the receivers taken as paging,
// and the woken-for-nothing line counted from pages.
func TestNotificationsText(t *testing.T) {
	cfg := nconfig(t)
	r := nreport(nfiring("A", 17*time.Hour, 17*time.Hour+time.Minute, false), nfiring("Down", 0, time.Minute, false))
	r.Hours = DefaultHours(time.UTC).Used()
	if err := r.WorkOutNotifications(NotificationOptions{Config: cfg, Hours: DefaultHours(time.UTC)}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"PAGED FOR NOTHING", NotificationsSaid, "Taken as paging: pager (pages by its pagerduty).",
		"of the pages of firings that caught nothing (worked out, not recorded), 1 went while people slept, and 0 more off hours.", "PAGES ASLEEP"} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	// Without them, nothing of it.
	r.Notifications = nil
	for i := range r.Rules {
		r.Rules[i].Notifications = nil
	}
	b.Reset()
	Text(&b, r, time.UTC)
	if strings.Contains(b.String(), "worked out") || strings.Contains(b.String(), "NOTIFIED") {
		t.Errorf("text of a report without notifications:\n%s", b.String())
	}
}

// The CSV gets its columns after the others, and the metrics their two.
func TestNotificationsCSVAndMetrics(t *testing.T) {
	cfg := nconfig(t)
	r := nreport(nfiring("A", 0, time.Minute, true))
	if err := r.WorkOutNotifications(NotificationOptions{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := CSV(&b, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&b).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %v", rows, err)
	}
	if got := strings.Join(rows[0][len(CSVColumns):], ","); got != strings.Join(NotificationCSVColumns, ",") {
		t.Errorf("columns %s", got)
	}
	if got := rows[1][len(CSVColumns):]; got[0] != "1" || got[1] != "1" || got[2] != "2026-11-02T10:00:10Z" || got[3] != `["pager"]` || got[4] != "[]" {
		t.Errorf("cells %q", got)
	}
	b.Reset()
	if err := Prometheus(&b, r, time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`lapilli_rule_notifications_worked_out{rule="A"} 1`, `lapilli_rule_pages_worked_out{rule="A"} 1`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %s:\n%s", want, b.String())
		}
	}
	// Without notifications, neither.
	r.Notifications = nil
	b.Reset()
	Prometheus(&b, r, time.Time{})
	if strings.Contains(b.String(), "worked_out") {
		t.Error("metrics of a report without notifications name worked_out")
	}
}

// Before and after: PAGED for each rule and in all, as the first line of the
// summary, in the text and the Markdown, and on the summary's JSON.
func TestCompareNotifications(t *testing.T) {
	cfg := nconfig(t)
	side := func(extra ...Judged) *Report {
		r := nreport(append([]Judged{nfiring("A", 0, time.Minute, true), nfiring("Down", 0, time.Minute, false)}, extra...)...)
		if err := r.WorkOutNotifications(NotificationOptions{Config: cfg}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	before, after := side(), side(nfiring("A", 3*time.Hour, 3*time.Hour+time.Minute, false), nfiring("A", 6*time.Hour, 6*time.Hour+time.Minute, false))
	c := Compare(before, after, []string{"A"}, 1)
	c.Summarize(false)
	s := c.Summary.Notifications
	if s == nil || !s.WorkedOut || s.PagedBefore != 1 || s.PagedAfter != 3 {
		t.Fatalf("summary: %+v", s)
	}
	var b bytes.Buffer
	if err := CompareText(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(b.String(), "\n")
	if lines[1] != "Pages 1 → 3 (worked out from the alert manager's configuration, not recorded)" || !strings.Contains(b.String(), "PAGED") {
		t.Errorf("text:\n%s", b.String())
	}
	for _, row := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(row, "A ") && !strings.Contains(row, "1 → 3") {
			t.Errorf("A's row lacks 1 → 3: %q", row)
		}
	}
	b.Reset()
	if err := CompareMarkdown(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"notifications":{"worked_out":true,"paged_before":1,"paged_after":3,`, "**Pages 1 → 3 (worked out", "| Pages, worked out | 1 | 3 |", "| Paged |"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, b.String())
		}
	}
	// Not worked out: none of it.
	plain := Compare(nreport(nfiring("A", 0, time.Minute, true)), nreport(nfiring("A", 0, time.Minute, true), nfiring("A", time.Hour, 2*time.Hour, true)), []string{"A"}, 0)
	plain.Summarize(false)
	b.Reset()
	CompareText(&b, plain, time.UTC)
	CompareMarkdown(&b, plain, time.UTC)
	if plain.Summary.Notifications != nil || strings.Contains(b.String(), "Pages") || strings.Contains(b.String(), "PAGED") || strings.Contains(b.String(), "notifications") {
		t.Errorf("a comparison without notifications:\n%s", b.String())
	}
}

// A budget of pages: held to the most a week sends, on each side, failing
// as a budget of firings does; not known where the notifications weren't
// worked out.
func TestExpectationsPages(t *testing.T) {
	cfg := nconfig(t)
	side := func(n int) *Report {
		var fs []Judged
		for i := range n {
			fs = append(fs, nfiring("A", time.Duration(i)*time.Hour, time.Duration(i)*time.Hour+time.Minute, true))
		}
		r := nreport(fs...)
		if err := r.WorkOutNotifications(NotificationOptions{Config: cfg}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	c := Compare(side(1), side(3), []string{"A"}, 0)
	c.From, c.To = nt0, nt0.Add(7*24*time.Hour)
	k := check(t, c, readExpect(t, "expectations:\n  - rule: A\n    budget: {pages: 2}\n"), nil, nil)
	if len(k.Results) != 1 {
		t.Fatalf("%+v", k.Results)
	}
	r := k.Results[0]
	if r.What != "budget pages" || r.Limit != "2" || r.Before != "1" || r.BeforeState != ExpectMet || r.After != "3" || r.AfterState != ExpectBroken || !r.BrokenByChange() {
		t.Errorf("%+v", r)
	}
	if got := r.heldTo(func(s string) string { return s }); !strings.Contains(got, "at most 2 pages a week") {
		t.Errorf("held to %q", got)
	}
	// No configuration: not known.
	plain := Compare(nreport(nfiring("A", 0, time.Minute, true)), nreport(nfiring("A", 0, time.Minute, true)), []string{"A"}, 0)
	plain.From, plain.To = nt0, nt0.Add(7*24*time.Hour)
	k = check(t, plain, readExpect(t, "expectations:\n  - rule: A\n    budget: {pages: 2}\n"), nil, nil)
	if r := k.Results[0]; r.BeforeState != ExpectUnknown || r.AfterState != ExpectUnknown || r.After != "–" {
		t.Errorf("not worked out: %+v", r)
	}
	// And the annotation takes pages.
	b, err := ParseBudget("10/week, 2 asleep, 3 refires, 1 pages")
	if err != nil || b.Pages == nil || *b.Pages != 1 || *b.Week != 10 {
		t.Errorf("%+v %v", b, err)
	}
	if _, err := ParseBudget("1 pages, 2 pages"); err == nil {
		t.Error("pages twice read")
	}
	if _, err := ReadExpectations(strings.NewReader("expectations:\n  - rule: A\n    budget: {pages: -1}\n")); err == nil {
		t.Error("a budget of pages below nothing read")
	}
	if _, err := ReadExpectations(strings.NewReader("expectations:\n  - rule: A\n    budget: {pages: 1}\n  - rule: A\n    budget: {pages: 2}\n")); err == nil {
		t.Error("pages given twice read")
	}
}

// --redact gives each receiver, inhibition rule, silence and time interval
// an alias, and the rule that inhibited another.
func TestRedactNotifications(t *testing.T) {
	cfg := nconfig(t)
	r := nreport(nfiring("A", 0, 10*time.Minute, true), nfiring("B", time.Minute, 10*time.Minute, false), nfiring("Down", 0, 3*time.Minute, false))
	if err := r.WorkOutNotifications(NotificationOptions{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	red := NewRedactor(nil).Report(r)
	var b bytes.Buffer
	JSON(&b, red)
	CSV(&b, red, nil, nil)
	Text(&b, red, time.UTC)
	for _, name := range []string{`"pager"`, `"channel"`, "down-hides-b", `"Down"`, "by Down"} {
		if strings.Contains(b.String(), name) {
			t.Errorf("redacted report holds %q:\n%s", name, b.String())
		}
	}
	for _, want := range []string{"receiver-", "inhibition-", `"paging": true`, "worked_out"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("redacted report lacks %q", want)
		}
	}
	// The report it was made from is as it was.
	if r.Notifications.Receivers[0].Name != "channel" || r.Firings[1].Notifications.Muted[0].By != "down-hides-b" {
		t.Error("the redaction changed the report")
	}
}

// A Grafana rule that names its contact point goes to it, past the policies
// (nconfig sends A to the pager), with the settings it gives.
func TestOwnContactPointRoute(t *testing.T) {
	cfg := nconfig(t)
	r := nreport(nfiring("A", 0, 10*time.Minute, true), nfiring("C", 0, 10*time.Minute, true))
	known := []rules.Rule{
		{Name: "A", Receiver: "channel", Notify: &rules.NotifySettings{GroupWait: 20 * time.Second}},
		{Name: "C", Receiver: "pager", Notify: &rules.NotifySettings{}},
	}
	if err := r.WorkOutNotifications(NotificationOptions{Config: cfg, Rules: known}); err != nil {
		t.Fatal(err)
	}
	a, c := r.Firings[0].Notifications, r.Firings[1].Notifications
	if len(a.Receivers) != 1 || a.Receivers[0] != "channel" || a.Paged != 0 || a.Notified == 0 || !r.Firings[0].Notifications.FirstPage.IsZero() {
		t.Errorf("A named the channel: %+v", a)
	}
	if len(c.Receivers) != 1 || c.Receivers[0] != "pager" || c.Paged == 0 || !c.FirstPage.Equal(nt0.Add(10*time.Second)) {
		t.Errorf("C named the pager, past the policies that send it to the channel: %+v", c)
	}
	if r.Notifications.Groups != 2 {
		t.Errorf("groups: %d", r.Notifications.Groups)
	}
}
