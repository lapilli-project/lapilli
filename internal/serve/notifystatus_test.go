package serve

import (
	"context"
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
	nowhere := "The weekly digest is worked out every mon@09:00 and goes nowhere: give it a place to go"
	_, body, _ := get(t, h, "/settings")
	for _, x := range []string{`<h2 id="notifications">Notifications</h2>`, nowhere, "What happens isn&#39;t told anywhere",
		"Nothing goes out: no webhook, Slack or place for reports is given.", "The weekly digest: every mon@09:00, to 0 places, as the flags set it."} {
		if !strings.Contains(body, x) {
			t.Errorf("/settings lacks %q", x)
		}
	}
	_, body, _ = get(t, h, "/manage")
	for _, x := range []string{`href="/settings#notifications">Notifications</a>`, nowhere, "Nothing goes out"} {
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
	for i := range 3 {
		if _, err := s.l.PutTask(ctx, ledger.Task{Title: "t" + itoa(int64(i)), Kind: "tune"}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	_, body, _ := get(t, h, "/manage")
	if !strings.Contains(body, "Nothing sent from the outbox yet.") {
		t.Errorf("/manage doesn't say nothing was sent yet:\n%s", body)
	}
	due, err := s.l.Due(ctx, "webhook", time.Now().Add(time.Minute), 10)
	if err != nil || len(due) != 3 {
		t.Fatalf("%d due: %v", len(due), err)
	}
	if err := s.l.Sent(ctx, due[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, n := range due[1:] {
		if err := s.l.Failed(ctx, n.ID, "503"); err != nil {
			t.Fatal(err)
		}
	}
	// Sending is written aside, and empties no page kept: these are
	// worked out again.
	for _, p := range []string{"/manage", "/settings"} {
		_, body, hd := get(t, h, p)
		for _, x := range []string{"Last sent from the outbox: ", "2 notices made in the last day weren&#39;t sent: their last try failed, and the log says why."} {
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
		"The weekly digest: every mon@09:00, to 2 places, as the flags set it."} {
		if !strings.Contains(body, x) {
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
	for _, x := range []string{"The weekly digest is worked out every tue@10:00 and goes nowhere", "1 report is on with nowhere to go: the places it names are gone.",
		"The weekly digest: every tue@10:00, to 0 of the places it names, as set on the Reports page.", "Reports on: 1."} {
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
