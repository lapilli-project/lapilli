package serve

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// digestWorld is a server whose digest the flags send to Slack, the webhook
// and shop's own, with a named destination, ops, it can be sent to too; and
// what each received, by its path.
type digestWorld struct {
	s   *Server
	mu  sync.Mutex
	got map[string][]string
}

func newDigestWorld(t *testing.T) *digestWorld {
	t.Helper()
	w := &digestWorld{got: map[string][]string{}}
	recv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.got[r.URL.Path] = append(w.got[r.URL.Path], string(b))
		w.mu.Unlock()
	}))
	t.Cleanup(recv.Close)
	dir := t.TempDir()
	file := func(name string) string {
		f := filepath.Join(dir, name)
		os.WriteFile(f, []byte(recv.URL+"/"+name), 0o600)
		return f
	}
	w.s, _ = server(t, func(c *Config) {
		c.Backfill = 10 * 24 * time.Hour
		c.Slack, c.NotifyWebhook = file("slack"), file("hook")
		c.TeamDigests = map[string]string{"shop": file("shop")}
		c.ReportDests = map[string]string{"ops": file("ops")}
	})
	if _, err := w.s.l.PutTeam(context.Background(), ledgerTeam("shop", "^Errors$"), "ana"); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := w.s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	return w
}

// round does the jobs at a time, and sends what the outbox holds.
func (w *digestWorld) round(t *testing.T, at time.Time) {
	t.Helper()
	ctx := context.Background()
	w.s.now = func() time.Time { return at }
	w.s.slackTried = time.Time{}
	w.s.jobs(ctx, true)
	for _, d := range w.s.sendsTo() {
		if err := w.s.sendDue(ctx, d, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
}

// took is how many each received since the last time it was asked, as
// slack=1 hook=2 ops=0 shop=1.
func (w *digestWorld) took() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, p := range []string{"/slack", "/hook", "/ops", "/shop"} {
		n := 0
		for _, b := range w.got[p] {
			if strings.Contains(b, "Lapilli  ") || strings.Contains(b, "digest.ready") {
				n++
			}
		}
		out = append(out, p[1:]+"="+itoa(int64(n)))
	}
	w.got = map[string][]string{}
	return strings.Join(out, " ")
}

// Set in the UI, the digest is the ledger's: when, where to, on or off;
// the flags only name where it can go. A week sent isn't sent again, to
// old destinations or new; one due and unsent goes once, where it is set
// to go; off, nothing goes but the week is still worked out; it never goes
// back to an older week; and it can be given back to the flags.
func TestDigestSet(t *testing.T) {
	ctx := context.Background()
	tuesday := now                                          // 2026-11-03 12:00, the week to 11-02 due
	monday := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) // the next week's

	t.Run("an upgrade, sent by the flags, then set", func(t *testing.T) {
		w := newDigestWorld(t)
		h := w.s.Handler()
		w.round(t, tuesday)
		if got := w.took(); got != "slack=1 hook=2 ops=0 shop=1" {
			t.Errorf("by the flags: %s", got)
		}
		code, body := api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["ops", "slack"]}`)
		if code != http.StatusOK || !strings.Contains(body, `"set_by": "ledger"`) || !strings.Contains(body, `"kind": "digest"`) {
			t.Fatalf("set: %d %s", code, body)
		}
		w.round(t, tuesday.Add(time.Hour))
		if got := w.took(); got != "slack=0 hook=0 ops=0 shop=0" {
			t.Errorf("the week sent, sent again: %s", got)
		}
		w.round(t, monday)
		if got := w.took(); got != "slack=1 hook=0 ops=1 shop=0" {
			t.Errorf("the next week, where it is set to go: %s", got)
		}
		w.round(t, monday.Add(time.Hour))
		if got := w.took(); got != "slack=0 hook=0 ops=0 shop=0" {
			t.Errorf("twice: %s", got)
		}
		// Given back to the flags.
		if code, _ := api(t, h, "DELETE", "/api/v1/reports/digest", ""); code != http.StatusNoContent {
			t.Fatalf("back: %d", code)
		}
		if _, body := api(t, h, "GET", "/api/v1/reports", ""); !strings.Contains(body, `"set_by": "flags"`) {
			t.Errorf("back to the flags: %s", body)
		}
	})

	t.Run("set before its week is sent", func(t *testing.T) {
		w := newDigestWorld(t)
		if code, body := api(t, w.s.Handler(), "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["ops"]}`); code != http.StatusOK {
			t.Fatalf("set: %d %s", code, body)
		}
		w.round(t, tuesday)
		if got := w.took(); got != "slack=0 hook=0 ops=1 shop=0" {
			t.Errorf("due and unsent, set: %s", got)
		}
	})

	t.Run("off, then on", func(t *testing.T) {
		w := newDigestWorld(t)
		h := w.s.Handler()
		if code, body := api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["slack", "webhook"], "enabled": false}`); code != http.StatusOK {
			t.Fatalf("off: %d %s", code, body)
		}
		w.round(t, tuesday)
		if got := w.took(); got != "slack=0 hook=0 ops=0 shop=0" {
			t.Errorf("off: %s", got)
		}
		if done, _ := w.s.l.Meta(ctx, "digest"); done == "" {
			t.Error("off, the week wasn't worked out")
		}
		if _, body := api(t, h, "GET", "/api/v1/reports/digest", ""); !strings.Contains(body, `"stopped": "off"`) || !strings.Contains(body, `"enabled": false`) {
			t.Errorf("off: %s", body)
		}
		api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["slack", "webhook"]}`)
		w.round(t, tuesday.Add(time.Hour))
		if got := w.took(); got != "slack=0 hook=0 ops=0 shop=0" {
			t.Errorf("on again, the week passed over while off: %s", got)
		}
		w.round(t, monday)
		if got := w.took(); got != "slack=1 hook=2 ops=0 shop=0" { // everyone's, and shop's, to the webhook
			t.Errorf("on, the next week: %s", got)
		}
	})

	t.Run("its day moved back across a week's start", func(t *testing.T) {
		w := newDigestWorld(t)
		w.round(t, tuesday)
		w.took()
		was, _ := w.s.l.Meta(ctx, "digest")
		// Sunday 20:00 last fell due on 11-01, in the week before the one
		// just sent.
		if code, body := api(t, w.s.Handler(), "PUT", "/api/v1/reports/digest", `{"schedule": "weekly sun@20:00", "dests": ["slack", "ops"]}`); code != http.StatusOK {
			t.Fatalf("set: %d %s", code, body)
		}
		w.round(t, tuesday.Add(time.Hour))
		if got := w.took(); got != "slack=0 hook=0 ops=0 shop=0" {
			t.Errorf("an older week sent: %s", got)
		}
		if now, _ := w.s.l.Meta(ctx, "digest"); now != was {
			t.Errorf("worked out the week %s again, after %s", now, was)
		}
		// A Sunday's digest is of the week whose Monday is before it: on
		// 11-08, the week already sent; on 11-15, the next.
		w.round(t, time.Date(2026, 11, 8, 21, 0, 0, 0, time.UTC))
		if got := w.took(); got != "slack=0 hook=0 ops=0 shop=0" {
			t.Errorf("its next Sunday: %s", got)
		}
		w.round(t, time.Date(2026, 11, 15, 21, 0, 0, 0, time.UTC))
		if got := w.took(); got != "slack=1 hook=0 ops=1 shop=0" {
			t.Errorf("the Sunday after: %s", got)
		}
	})

	t.Run("refused", func(t *testing.T) {
		w := newDigestWorld(t)
		h := w.s.Handler()
		for _, body := range []string{
			`{"schedule": "daily@09:00", "dests": ["slack"]}`,
			`{"schedule": "0 9 * * 1", "dests": ["slack"]}`,
			`{"schedule": "weekly mon@25:00", "dests": ["slack"]}`,
			`{"schedule": "weekly mon@09:00", "dests": ["nowhere"]}`,
			`{"schedule": "weekly mon@09:00", "dests": []}`,
		} {
			if code, _ := api(t, h, "PUT", "/api/v1/reports/digest", body); code != http.StatusBadRequest {
				t.Errorf("%s: %d", body, code)
			}
		}
	})
}

func ledgerTeam(name, rules string) ledger.Team { return ledger.Team{Name: name, Rules: rules} }

// A week the digest went to before a destination was added isn't sent to
// it, whichever way each goes (Slack's post, the outbox); one that went
// nowhere goes once where the digest is first set to go; and a week told
// isn't worked out again each round (what reviews found).
func TestDigestSetReview(t *testing.T) {
	tuesday := now
	monday := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name          string
		first, second string
	}{
		{"sent to slack, then ops added", `["slack"]`, `["slack", "ops"]`},
		{"told to ops, then slack added", `["ops"]`, `["ops", "slack"]`},
		{"told to the webhook, then shop's added", `["webhook"]`, `["webhook", "team:shop"]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newDigestWorld(t)
			h := w.s.Handler()
			w.round(t, tuesday)
			w.took()
			api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": `+c.first+`}`)
			w.round(t, monday)
			w.took()
			api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": `+c.second+`}`)
			w.round(t, monday.Add(time.Hour))
			if got := w.took(); got != "slack=0 hook=0 ops=0 shop=0" {
				t.Errorf("the week sent, sent again to a destination added: %s", got)
			}
		})
	}
	t.Run("sent to slack, off, on with the webhook", func(t *testing.T) {
		w := newDigestWorld(t)
		h := w.s.Handler()
		w.round(t, tuesday)
		api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["slack"]}`)
		w.round(t, monday)
		w.took()
		api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["slack"], "enabled": false}`)
		w.round(t, monday.Add(30*time.Minute))
		api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["slack", "webhook"]}`)
		w.round(t, monday.Add(time.Hour))
		if got := w.took(); got != "slack=0 hook=0 ops=0 shop=0" {
			t.Errorf("sent again: %s", got)
		}
	})
	t.Run("Slack failed, ops added: Slack's is still tried", func(t *testing.T) {
		w := newDigestWorld(t)
		h := w.s.Handler()
		w.round(t, tuesday)
		w.took()
		api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["slack", "ops"]}`)
		slack := w.s.cfg.Slack
		w.s.cfg.Slack = filepath.Join(t.TempDir(), "missing") // fails to read
		w.round(t, monday)
		w.took()
		w.s.cfg.Slack = slack
		api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["slack", "ops", "webhook"]}`)
		w.round(t, monday.Add(2*time.Hour))
		if got := w.took(); got != "slack=1 hook=0 ops=0 shop=0" {
			t.Errorf("after Slack failed: %s", got)
		}
	})
}

// The flags send the digest nowhere, but a --report-dest names where it
// could go: the Reports page offers to set it, and set to ops the day after
// its week fell due, that week goes to ops, once.
func TestDigestFirstSet(t *testing.T) {
	tuesday := now
	monday := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	w := &digestWorld{got: map[string][]string{}}
	recv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.got[r.URL.Path] = append(w.got[r.URL.Path], string(b))
		w.mu.Unlock()
	}))
	t.Cleanup(recv.Close)
	f := filepath.Join(t.TempDir(), "ops")
	os.WriteFile(f, []byte(recv.URL+"/ops"), 0o600)
	w.s, _ = server(t, func(c *Config) {
		c.Backfill = 10 * 24 * time.Hour
		c.ReportDests = map[string]string{"ops": f}
	})
	raw := ""
	if err := w.s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	h := w.s.Handler()
	if _, page, _ := get(t, h, "/reports"); !strings.Contains(page, "/reports?edit=digest") {
		t.Errorf("the Reports page doesn't offer to set the digest:\n%s", page)
	}
	w.round(t, tuesday)
	if got := w.took(); got != "slack=0 hook=0 ops=0 shop=0" {
		t.Fatalf("by the flags, nowhere: %s", got)
	}
	if code, body := api(t, h, "PUT", "/api/v1/reports/digest", `{"schedule": "weekly mon@09:00", "dests": ["ops"]}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	w.round(t, tuesday.Add(time.Hour))
	if got := w.took(); got != "slack=0 hook=0 ops=1 shop=0" {
		t.Errorf("the week due and sent nowhere, set to ops: %s", got)
	}
	w.round(t, tuesday.Add(2*time.Hour))
	w.round(t, monday)
	if got := w.took(); got != "slack=0 hook=0 ops=1 shop=0" {
		t.Errorf("the next week: %s", got)
	}
}

// A week told isn't worked out again each round: the guard reads what
// TellOnce keeps. A --team-digest naming a team the ledger hasn't logs
// once each time the digest is worked out for telling: count them.
func TestDigestNotRebuiltEachRound(t *testing.T) {
	monday := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	w := newDigestWorld(t)
	ghost := filepath.Join(t.TempDir(), "ghost")
	os.WriteFile(ghost, []byte("http://127.0.0.1:1/ghost"), 0o600)
	w.s.cfg.TeamDigests["ghost"] = ghost
	var buf bytes.Buffer
	w.s.log = slog.New(slog.NewTextHandler(&buf, nil))
	builds := func() int {
		n := strings.Count(buf.String(), "names a team the ledger doesn't have")
		buf.Reset()
		return n
	}
	// As 0.23's guard left it, when a webhook was first added late: a
	// week long gone.
	if err := w.s.l.SetMeta(context.Background(), "digest-told", "20000"); err != nil {
		t.Fatal(err)
	}
	w.round(t, now)
	if n := builds(); n != 1 {
		t.Errorf("the first week worked out %d times", n)
	}
	w.round(t, monday)
	builds()
	for i := 1; i <= 5; i++ {
		w.round(t, monday.Add(time.Duration(i)*time.Minute))
	}
	if n := builds(); n != 0 {
		t.Errorf("the second week, told, worked out again %d times in five rounds", n)
	}
}
