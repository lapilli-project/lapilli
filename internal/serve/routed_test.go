package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// An Alertmanager configuration for the test server's rule (Errors,
// severity page): it goes to the pager, a PagerDuty and a Slack, grouped by
// alertname; everything else to a channel.
const routedAM = `route:
  receiver: channel
  group_by: [alertname]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 1h
  routes:
    - matchers: ['severity="page"']
      receiver: oncall
receivers:
  - name: channel
    slack_configs: [{channel: '#ops'}]
  - name: oncall
    pagerduty_configs: [{routing_key: k}]
    slack_configs: [{channel: '#oncall'}]
`

func routedFile(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "alertmanager.yml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// workedServer is a server that works the notifications out, with its runs
// done: the test history's firings of Errors, which the configuration sends
// to the pager.
func workedServer(t *testing.T, change func(*Config)) (*Server, string) {
	t.Helper()
	am := routedFile(t, routedAM)
	s, _ := server(t, func(c *Config) {
		c.Notifications, c.AlertmanagerConfig = true, am
		c.Options.Hours = nil
		if change != nil {
			change(c)
		}
	})
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), true)
	return s, am
}

// workedOf reads what was worked out for the rule's firings, by start.
func workedOf(t *testing.T, s *Server, rule string) map[time.Time]ledger.RoutedFiring {
	t.Helper()
	return workedOfUpTo(t, s, rule, now)
}

// workedOfUpTo is workedOf for a ledger whose hours go up to a time.
func workedOfUpTo(t *testing.T, s *Server, rule string, upTo time.Time) map[time.Time]ledger.RoutedFiring {
	t.Helper()
	ctx := context.Background()
	fs, _, err := s.l.FiringsOverlapping(ctx, now.Add(-4*24*time.Hour), now, now.Add(-4*24*time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	byID := map[int64]time.Time{}
	for _, f := range fs {
		if f.Rule == rule {
			ids = append(ids, f.ID)
			byID[f.ID] = f.Start
		}
	}
	got, err := s.l.RoutedOf(ctx, ids, upTo)
	if err != nil {
		t.Fatal(err)
	}
	out := map[time.Time]ledger.RoutedFiring{}
	for id, r := range got {
		out[byID[id]] = r
	}
	if len(ids) == 0 {
		t.Fatalf("no firing of %s", rule)
	}
	return out
}

// A run works out the notifications of the hours it judges, and keeps them
// per firing: Errors goes to the pager, whose PagerDuty pages and Slack
// messages.
func TestRoutedKept(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	from, to, ok, err := routedSpan(s.l, ctx)
	if err != nil || !ok || !from.Before(to) {
		t.Fatalf("span: %v %v %v %v", from, to, ok, err)
	}
	got := workedOf(t, s, "Errors")
	if len(got) < 5 {
		t.Fatalf("%d firings worked out", len(got))
	}
	paged := 0
	for start, r := range got {
		if r.Notified == 0 || r.Paged == 0 || r.FirstPage.Before(start) || len(r.PageAts) != r.Paged {
			t.Errorf("firing at %v: %+v", start, r)
		}
		var kinds []string
		for _, rc := range r.Receivers {
			if rc.Receiver != "oncall" {
				t.Errorf("reached %q", rc.Receiver)
			}
			kinds = append(kinds, rc.Kind)
		}
		slices.Sort(kinds)
		if !slices.Equal(kinds, []string{"pagerduty", "slack"}) {
			t.Errorf("kinds %v", kinds)
		}
		paged += r.Paged
	}
	if paged == 0 {
		t.Error("no page")
	}
	by, err := s.l.RoutedByReceiver(ctx, from, to)
	if err != nil || by["oncall"].Notifications == 0 || by["oncall"].Pages == 0 || by["oncall"].Pages >= by["oncall"].Notifications || by["channel"].Notifications != 0 {
		t.Errorf("by receiver: %v %v", by, err)
	}
}

// Judging again works the notifications out again from the ledger alone: a
// configuration that now sends Errors to a channel puts no page on any
// firing, with no history asked.
func TestRoutedAgain(t *testing.T) {
	parallel(t)
	s, am := workedServer(t, nil)
	ctx := context.Background()
	h := s.h.(*history)
	asked := func() int { h.Lock(); defer h.Unlock(); return h.asked }
	from, to, _, _ := routedSpan(s.l, ctx)
	before := asked()
	if err := os.WriteFile(am, []byte(strings.Replace(routedAM, "severity=\"page\"", "severity=\"nothing\"", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	s.routed.Lock()
	s.routed.at = time.Time{} // read again, as an hour later
	s.routed.Unlock()
	s.workOutRouted(ctx, from, to)
	if asked() != before {
		t.Errorf("the history was asked %d times more", asked()-before)
	}
	for start, r := range workedOf(t, s, "Errors") {
		if r.Paged != 0 || len(r.Receivers) != 1 || r.Receivers[0].Receiver != "channel" || r.Receivers[0].Kind != "slack" {
			t.Errorf("firing at %v: %+v", start, r)
		}
	}
	by, _ := s.l.RoutedByReceiver(ctx, from, to)
	if by["oncall"].Notifications != 0 || by["channel"].Notifications == 0 {
		t.Errorf("by receiver after: %v", by)
	}
}

// A configuration that can't be read leaves the notifications as they
// were, and the page says so.
func TestRoutedConfigUnreadable(t *testing.T) {
	parallel(t)
	s, am := workedServer(t, nil)
	ctx := context.Background()
	from, to, _, _ := routedSpan(s.l, ctx)
	if err := os.WriteFile(am, []byte("route: {"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.routed.Lock()
	s.routed.at = time.Time{}
	s.routed.Unlock()
	s.workOutRouted(ctx, from, to)
	if got := workedOf(t, s, "Errors"); len(got) == 0 {
		t.Fatal("none kept")
	}
	h := s.Handler()
	code, body, _ := get(t, h, "/notifications")
	if code != http.StatusOK || !strings.Contains(body, "What was worked out before stays") {
		t.Errorf("/notifications: %d %s", code, body)
	}
}

// With --notifications off there is no page, no sidebar entry and no number.
func TestRoutedOff(t *testing.T) {
	parallel(t)
	s, _ := server(t, nil)
	s.jobs(context.Background(), true)
	h := s.Handler()
	if _, body, _ := get(t, h, "/"); strings.Contains(body, "/notifications") {
		t.Error("a sidebar entry with no notifications worked out")
	}
	if _, body, _ := get(t, h, "/metrics"); strings.Contains(body, "lapilli_notifications_agreement") || strings.Contains(body, "lapilli_rule_pages") {
		t.Error("metrics of pages with none worked out")
	}
	if _, _, ok, err := routedSpan(s.l, context.Background()); ok || err != nil {
		t.Errorf("a span with none worked out: %v %v", ok, err)
	}
	code, body, _ := get(t, h, "/api/v1/report")
	var rep struct {
		Notifications json.RawMessage `json:"notifications"`
	}
	if json.Unmarshal([]byte(body), &rep); code != 200 || len(rep.Notifications) != 0 {
		t.Errorf("report: %d notifications %s", code, rep.Notifications)
	}
}

// What the pages say of the worked-out pages: each shows them, saying that
// they are worked out.
func TestRoutedSurfaces(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, func(c *Config) { c.Options.Hours = nil })
	h := s.Handler()
	// The rule page's pages section.
	code, body, _ := get(t, h, "/rules/Errors")
	if code != 200 || !strings.Contains(body, `id="pages"`) || !strings.Contains(body, "worked out") {
		t.Errorf("the rule page: %d, no pages section saying worked out", code)
	}
	if !strings.Contains(body, "pages, worked out") {
		t.Errorf("the rule page doesn't say its pages are worked out")
	}
	// The rules list.
	_, body, _ = get(t, h, "/rules?weeks=1")
	if !strings.Contains(body, "pages, worked out") {
		t.Errorf("the rules list doesn't show pages, worked out")
	}
	// A firing's own line says worked out.
	_, body, _ = get(t, h, "/rules/Errors?tab=firings")
	if !strings.Contains(body, "(worked out") {
		t.Errorf("a firing's line doesn't say worked out")
	}
	// The Notifications page lists the receivers, none recorded.
	code, body, _ = get(t, h, "/notifications")
	if code != 200 || !strings.Contains(body, "<code>oncall</code>") || !strings.Contains(body, "none recorded") || !strings.Contains(body, "No notification was recorded") {
		t.Errorf("the notifications page: %d\n%s", code, body)
	}
	if _, body, _ = get(t, h, "/"); !strings.Contains(body, `href="/notifications"`) {
		t.Errorf("no sidebar entry")
	}
	// Metrics: pages by basis; no agreement with nothing recorded.
	_, body, _ = get(t, h, "/metrics")
	if !strings.Contains(body, `lapilli_rule_pages{rule="Errors",basis="worked out"}`) || strings.Contains(body, "lapilli_notifications_agreement") {
		t.Errorf("metrics: %s", grepLines(body, "pages", "agreement"))
	}
	// The report: verdict's fields, marked worked_out.
	code, body, _ = get(t, h, "/api/v1/report?from=2026-10-30T00:00:00Z&to=2026-11-03T12:00:00Z")
	var rep struct {
		Notifications struct {
			WorkedOut bool `json:"worked_out"`
			Notified  int  `json:"notified"`
			Paged     int  `json:"paged"`
			Receivers []struct {
				Name   string `json:"name"`
				Paging bool   `json:"paging"`
			} `json:"receivers"`
		} `json:"notifications"`
		Rules []struct {
			Rule          string `json:"rule"`
			Notifications struct {
				Notified int `json:"notified"`
				Paged    int `json:"paged"`
			} `json:"notifications"`
		} `json:"rules"`
		Firings []struct {
			Rule          string `json:"rule"`
			Notifications *struct {
				Paged     int      `json:"paged"`
				Receivers []string `json:"receivers"`
			} `json:"notifications"`
		} `json:"firings"`
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil || code != 200 {
		t.Fatalf("report: %d %v\n%s", code, err, body)
	}
	if !rep.Notifications.WorkedOut || rep.Notifications.Paged == 0 || rep.Notifications.Notified <= rep.Notifications.Paged {
		t.Errorf("report notifications: %+v", rep.Notifications)
	}
	paging := ""
	for _, r := range rep.Notifications.Receivers {
		if r.Paging {
			paging = r.Name
		}
	}
	if paging != "oncall" {
		t.Errorf("paging receivers %+v", rep.Notifications.Receivers)
	}
	found := false
	for _, r := range rep.Rules {
		if r.Rule == "Errors" && r.Notifications.Paged > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("rules: %+v", rep.Rules)
	}
	withPages := 0
	for _, f := range rep.Firings {
		if f.Rule == "Errors" && f.Notifications != nil && f.Notifications.Paged > 0 && slices.Contains(f.Notifications.Receivers, "oncall") {
			withPages++
		}
	}
	if withPages == 0 {
		t.Errorf("no firing with its pages in the report")
	}
	// As CSV, with the columns verdict gives.
	_, body, _ = get(t, h, "/api/v1/report?format=csv&from=2026-10-30T00:00:00Z&to=2026-11-03T12:00:00Z")
	if !strings.Contains(body, "worked_out_paged") {
		t.Errorf("csv: %.200s", body)
	}
	// The summary's rows.
	_, body, _ = get(t, h, "/api/v1/summary")
	var sum struct {
		Rows []struct {
			Paged *struct {
				Pages     int    `json:"pages"`
				WorkedOut int    `json:"worked_out"`
				Recorded  int    `json:"recorded"`
				Basis     string `json:"basis"`
			} `json:"paged"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &sum); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, r := range sum.Rows {
		if r.Paged != nil {
			total += r.Paged.Pages
			if r.Paged.Pages > 0 && (r.Paged.Basis != "worked out" || r.Paged.Recorded != 0 || r.Paged.WorkedOut != r.Paged.Pages) {
				t.Errorf("a week's pages: %+v", r.Paged)
			}
		}
	}
	if total == 0 {
		t.Errorf("no page in the summary: %s", body)
	}
}

func grepLines(text string, subs ...string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		for _, s := range subs {
			if strings.Contains(l, s) {
				out = append(out, l)
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// What a run records is worked out by the run's hours, and a firing a run
// replaced has its own row: recorded by hand over worked-out hours, a firing
// has none until the hours are worked out again.
func TestRoutedFiringsReplaced(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	from, to, _, _ := routedSpan(s.l, ctx)
	at := to.Add(-3 * time.Hour)
	lo := to.Add(-4 * time.Hour)
	if err := s.l.Record(ctx, ledger.Found{From: lo, To: to, Run: ledger.Run{Kind: "test", Started: at, Finished: at, From: lo, To: to},
		Firings: []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "late", "severity": "page"}, Start: at, End: at.Add(10 * time.Minute)}}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	got := workedOf(t, s, "Errors")
	if _, ok := got[at]; ok {
		t.Fatal("worked out before a run did")
	}
	s.workOutRouted(ctx, lo, to)
	got = workedOf(t, s, "Errors")
	if r, ok := got[at]; !ok || r.Paged == 0 {
		t.Errorf("not worked out after: %+v %v", r, ok)
	}
	if f2, to2, _, _ := routedSpan(s.l, ctx); !f2.Equal(from) || !to2.Equal(to) {
		t.Errorf("span: %v %v", f2, to2)
	}
}

// workedAndRecorded is a server with some pages recorded, from the
// middle of the span on, and the rest worked out: both bases at once.
func workedAndRecorded(t *testing.T) *Server {
	t.Helper()
	s, _ := workedServer(t, func(c *Config) { c.Options.Hours = nil })
	ctx := context.Background()
	fs, _, err := s.l.FiringsOverlapping(ctx, now.Add(-4*24*time.Hour), now, now.Add(-4*24*time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range fs {
		if f.Rule != "Errors" || f.Start.Before(now.Add(-40*time.Hour)) {
			continue
		}
		n++
		for _, recv := range []string{"oncall/pagerduty", "oncall/slack"} {
			_, err := s.l.Notify(ctx, ledger.Notification{At: f.Start.Add(31 * time.Second), Source: "alertmanager-events", Receiver: recv, Group: "{}:{alertname=\"Errors\"}", Reason: ledger.ReasonFirst,
				Alerts: []ledger.Alert{{Rule: "Errors", Labels: map[string]string{"alertname": "Errors", "service": f.Labels["service"], "severity": "page"}, Start: f.Start}}})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if n == 0 {
		t.Fatal("no firing to record a notification for")
	}
	return s
}

// Every text the pages of the worked-out notifications say is in each
// language.
func TestRoutedLanguages(t *testing.T) {
	s := workedAndRecorded(t)
	h := s.Handler()
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	paths := []string{"/", "/rules", "/rules?weeks=12", "/rules/Errors", "/rules/Errors?tab=firings", "/notifications", "/firings?day=2026-11-02", "/settings",
		"/rules?perf=silent&weeks=12", "/goals", "/outcomes", "/reports"}
	for _, n := range names {
		for _, p := range paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Sec-Fetch-Site", "none")
			r.Header.Set("Accept-Language", n.Code+";q=0.9, de;q=0.5")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !strings.Contains(w.Body.String(), `<html lang="`+n.Code+`">`) {
				t.Errorf("%s %s: %d, not in %s", n.Code, p, w.Code, n.Code)
			}
		}
		// The digest's line.
		rep, err := s.ledgerReport(context.Background(), now.Add(-7*24*time.Hour), now, s.cfg.Options, false)
		if err != nil {
			t.Fatal(err)
		}
		sent, err := s.sent(context.Background(), rep, now.Add(-7*24*time.Hour), now)
		if err != nil || sent == nil || sent.Pages == nil {
			t.Fatalf("the digest's pages: %+v %v", sent, err)
		}
		if line := sent.text(n.Code); !strings.Contains(line, strings.TrimSpace(tr(n.Code, sent.Pages.Basis))) {
			t.Errorf("%s: the digest's line %q doesn't say %q", n.Code, line, sent.Pages.Basis)
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}

// routedSpan is from where to where the ledger's hours were worked out, as
// the cover says, and whether any was.
func routedSpan(l *ledger.Ledger, ctx context.Context) (from, to time.Time, ok bool, err error) {
	c, err := l.RoutedCover(ctx)
	if err != nil {
		return
	}
	from, ok = c.First()
	to, _ = c.Last()
	return
}
