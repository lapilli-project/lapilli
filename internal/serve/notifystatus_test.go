package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// TestNotifyStatusNowhere: with nowhere to send anything, the Settings page
// and the Manage page's card say so, and that the weekly digest, worked
// out every week, goes nowhere, in words.
func TestNotifyStatusNowhere(t *testing.T) {
	s, _ := server(t, nil)
	h := s.Handler()
	nowhere := "The weekly digest is worked out Mondays 09:00 and goes nowhere: give lapilli serve --slack-webhook-file, --notify-webhook-file or --team-digest, or set it on the Reports page"
	_, body, _ := get(t, h, "/settings")
	for _, x := range []string{`<h2 id="notifications">Notifications</h2>`, nowhere, "What happens isn&#39;t told anywhere",
		"Nothing goes out: no webhook, Slack or place for reports is given.", "The weekly digest: Mondays 09:00, to 0 places, as the flags set it."} {
		if !containsPlain(body, x) {
			t.Errorf("/settings lacks %q", x)
		}
	}
	_, body, _ = get(t, h, "/manage")
	for _, x := range []string{`href="/settings#notifications">Notifications</a>`, "The weekly digest goes nowhere.", "Nothing goes out"} {
		if !strings.Contains(body, x) {
			t.Errorf("/manage lacks %q", x)
		}
	}
	// The settings' API answers as it did.
	if _, body, _ := get(t, h, "/api/v1/settings"); strings.Contains(body, "Notify") || strings.Contains(body, "notif") {
		t.Errorf("/api/v1/settings says more than it did: %s", body)
	}
}

// TestNotifyStatusSent: with places to send to, the pages count them, never
// show a URL, say when the outbox last sent and how many of the last day's
// notices failed, as it is now, though sending empties no page kept; and a
// digest or a report on with nowhere to go is said so.
func TestNotifyStatusSent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook")
	os.WriteFile(hook, []byte("https://hooks.example.com/secret-path"), 0o600)
	s, _ := server(t, func(c *Config) {
		c.NotifyWebhook, c.ReportDests, c.TeamDigests = hook, map[string]string{"ops": hook, "sre": hook}, map[string]string{"payments": hook}
	})
	h := s.Handler()
	for i := range 4 {
		if _, err := s.l.PutTask(ctx, ledger.Task{Title: "t" + itoa(int64(i)), Kind: "tune"}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	_, body, _ := get(t, h, "/manage")
	if !strings.Contains(body, "Nothing sent from the outbox yet.") {
		t.Errorf("/manage doesn't say nothing was sent yet:\n%s", body)
	}
	due, err := s.l.Due(ctx, "webhook", time.Now().Add(time.Minute), 10)
	if err != nil || len(due) != 4 {
		t.Fatalf("%d due: %v", len(due), err)
	}
	if err := s.l.Sent(ctx, due[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, n := range due[1:3] {
		if err := s.l.Failed(ctx, n.ID, "503"); err != nil {
			t.Fatal(err)
		}
	}
	// One given up on, after ten tries: said apart.
	for range ledger.GiveUpAfter {
		if err := s.l.Failed(ctx, due[3].ID, "503"); err != nil {
			t.Fatal(err)
		}
	}
	// Sending is written aside, and empties no page kept: these are
	// worked out again.
	for p, failed := range map[string][2]string{"/manage": {"2 notices failed in the last day.", "1 notice was given up on in the last day."},
		"/settings": {"2 notices made in the last day weren&#39;t sent: their last try failed, and the log says why.",
			"1 notice made in the last day was given up on, after ten tries: the log says why."}} {
		_, body, hd := get(t, h, p)
		for _, x := range []string{"Last sent from the outbox: ", failed[0], failed[1]} {
			if !strings.Contains(body, x) {
				t.Errorf("%s lacks %q:\n%s", p, x, body)
			}
		}
		if strings.Contains(body, "hooks.example.com") || strings.Contains(body, hook) {
			t.Errorf("%s shows where the webhook is", p)
		}
		if hd.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", p, hd.Get("Cache-Control"))
		}
	}
	_, body, _ = get(t, h, "/settings")
	for _, x := range []string{"What happens is told to the webhook.", "Places reports can go to by name (--report-dest): 2. Teams with a digest of their own (--team-digest): 1.",
		"The weekly digest: Mondays 09:00, to 2 places, as the flags set it."} {
		if !containsPlain(body, x) {
			t.Errorf("/settings lacks %q", x)
		}
	}
	if strings.Contains(body, "goes nowhere") {
		t.Error("/settings says the digest goes nowhere, with places to go")
	}

	// The digest set to a place gone, and a report on to one: said so.
	if err := s.l.PutDigest(ctx, ledger.DigestSet{Schedule: "weekly tue@10:00", Dests: []string{"gone"}, Enabled: true}, "ana", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "daily", Schedule: "0 9 * * *", Span: "24h", Zone: "ledger", Sections: ledger.ReportSections,
		Dests: []string{"old"}, Enabled: true}, "ana", now); err != nil {
		t.Fatal(err)
	}
	_, body, _ = get(t, h, "/settings")
	for _, x := range []string{"The weekly digest is worked out Tuesdays 10:00 and goes nowhere: none of the places set for it on the Reports page is there now.",
		"1 report is on with nowhere to go: the places it names are gone.",
		"The weekly digest: Tuesdays 10:00, to 0 of the places it names, as set on the Reports page.", "Reports on: 1."} {
		if !strings.Contains(body, x) {
			t.Errorf("/settings lacks %q:\n%s", x, body)
		}
	}
	// Another page is kept as it was.
	if _, _, hd := get(t, h, "/teams"); hd.Get("Cache-Control") != "no-cache" || hd.Get("ETag") == "" {
		t.Errorf("/teams isn't kept: %v", hd)
	}
}

// TestNotifyStatusInEachLanguage: what the pages say of notices is in
// every language.
func TestNotifyStatusInEachLanguage(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	inEachLanguage(t, s.Handler(), "", "/settings", "/manage")
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example.com/x"), 0o600)
	s, _ = server(t, func(c *Config) { c.NotifyWebhook, c.NotifySlack, c.Slack = hook, true, hook })
	for i := range 2 {
		if _, err := s.l.PutTask(ctx, ledger.Task{Title: "t" + itoa(int64(i)), Kind: "tune"}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	due, _ := s.l.Due(ctx, "webhook", time.Now().Add(time.Minute), 10)
	s.l.Failed(ctx, due[0].ID, "503")
	inEachLanguage(t, s.Handler(), "", "/settings", "/manage")
	for _, n := range due {
		s.l.Failed(ctx, n.ID, "503")
	}
	s.l.Sent(ctx, due[1].ID)
	s.l.PutScheduled(ctx, ledger.Scheduled{Name: "a", Schedule: "0 9 * * *", Span: "24h", Zone: "ledger", Sections: ledger.ReportSections, Dests: []string{"old"}, Enabled: true}, "ana", now)
	s.l.PutScheduled(ctx, ledger.Scheduled{Name: "b", Schedule: "0 9 * * *", Span: "24h", Zone: "ledger", Sections: ledger.ReportSections, Dests: []string{"old"}, Enabled: true}, "ana", now)
	s.l.PutDigest(ctx, ledger.DigestSet{Schedule: "weekly tue@10:00", Dests: []string{"slack"}}, "ana", now)
	inEachLanguage(t, s.Handler(), "", "/settings", "/manage")
	s, _ = server(t, func(c *Config) { c.NotifySlack, c.Slack = true, hook })
	inEachLanguage(t, s.Handler(), "", "/settings")
}

// TestNotifyStatusSlackPost: the weekly digest is posted to Slack itself,
// not through the outbox: how its posts went is read from the ledger, as
// every replica, and a process restarted, reads it: the last week posted
// (digest-posted), a week passed over said as one done with, not posted
// (digest-sent alone), and a later week's digest that failed (a run's
// error), as a warning.
func TestNotifyStatusSlackPost(t *testing.T) {
	ctx := context.Background()
	posts := 0
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts++ }))
	defer slack.Close()
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("http://127.0.0.1:1/x"), 0o600)
	s, _ := server(t, func(c *Config) { c.Slack = hook })
	h := s.Handler()
	if _, body, _ := get(t, h, "/settings"); !strings.Contains(body, "No weekly digest has been posted to Slack yet.") {
		t.Errorf("/settings doesn't say the digest wasn't posted yet:\n%s", body)
	}
	week := ledger.Week(s.l.Day(now))
	date := func(w int64) string { return s.l.Midnight(w).Format("2006-01-02") }
	// The week before passed over, as when the digest is off: done with,
	// not posted.
	if err := s.l.SetMeta(ctx, "digest-sent", itoa(week-7)); err != nil {
		t.Fatal(err)
	}
	_, body, _ := get(t, h, "/settings")
	if !strings.Contains(body, "Up to the week to "+date(week-7)+", the weekly digest went to Slack or was passed over.") || strings.Contains(body, "Last posted") {
		t.Errorf("/settings says a week passed over was posted:\n%s", body)
	}
	// Posted where nothing answers, its post fails, kept as a run's.
	post := func() error {
		s.slackTried = time.Time{}
		return s.postDigest(ctx, digestIn{On: true, Dests: []string{"slack"}}, itoa(week), s.l.Midnight(week-7), s.l.Midnight(week), s.cfg.Options)
	}
	if err := post(); err == nil {
		t.Fatal("posting the digest where nothing answers worked")
	}
	failed := "The weekly digest for the week to " + date(week) + " failed, at "
	_, body, _ = get(t, h, "/settings")
	if !strings.Contains(body, failed) || !strings.Contains(body, "the Run history page says why.") || strings.Contains(body, "127.0.0.1") {
		t.Errorf("/settings doesn't say the digest failed, or says where:\n%s", body)
	}
	if _, body, _ := get(t, h, "/manage"); !strings.Contains(body, "The weekly digest failed.") {
		t.Errorf("/manage doesn't say the digest failed:\n%s", body)
	}
	// Posted where Slack answers: posted, not failed.
	os.WriteFile(hook, []byte(slack.URL+"/x"), 0o600)
	if err := post(); err != nil || posts != 1 {
		t.Fatalf("posting the digest: %v, %d posts", err, posts)
	}
	_, body, _ = get(t, h, "/settings")
	if !strings.Contains(body, "Last posted to Slack for the week to "+date(week)+".") || strings.Contains(body, failed) {
		t.Errorf("/settings doesn't say the digest was posted, or says it failed:\n%s", body)
	}
	// Not going to Slack, nothing is said of it.
	s, _ = server(t, nil)
	if _, body, _ := get(t, s.Handler(), "/settings"); strings.Contains(body, "to Slack") {
		t.Error("/settings speaks of Slack, with no Slack")
	}
}

// TestNotifyStatusTexts: every way the notifications are said, past the
// failures counted and with the digest's post failed, on every day of the
// week, is in every language.
func TestNotifyStatusTexts(t *testing.T) {
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, n := range names {
		for day := range 7 {
			for _, st := range []notifyStatus{
				{Sends: true, Failed: 3, Backlog: true, GivenUp: 1000, GivenUpMore: true, Counted: 1000, DigestOn: true, DigestNowhere: true, Posted: &digestPosted{At: now, FailedTo: now, Failed: true}, Loc: time.UTC},
				{Sends: true, Failed: 1, GivenUp: 1, ReportsNowhere: 1, DigestOn: true, DigestSet: true, DigestNowhere: true, Posted: &digestPosted{}, Loc: time.UTC},
				{Sends: true, LastSent: now, Failed: 2, GivenUp: 2, ReportsNowhere: 2, DigestOn: true, Posted: &digestPosted{Posted: now, Done: now}, Loc: time.UTC},
				{Sends: true, DigestOn: true, Posted: &digestPosted{Done: now}, Loc: time.UTC},
			} {
				st.DigestWhen = weeklyText(n.Code, Weekly{time.Weekday(day), 9, 0})
				for _, tpl := range []string{"notifystatus", "notifycard"} {
					var b strings.Builder
					if err := pagesIn[n.Code].ExecuteTemplate(&b, tpl, struct {
						P page
						V any
					}{page{Loc: time.UTC, Lang: n.Code}, &st}); err != nil {
						t.Fatal(err)
					}
					if tpl == "notifystatus" && (strings.Contains(b.String(), "@09") || !strings.Contains(b.String(), "09:00")) {
						t.Errorf("%s %s: the digest's time isn't as a person reads it:\n%s", n.Code, tpl, b.String())
					}
				}
			}
		}
	}
	missing.Lock()
	defer missing.Unlock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
}
