package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// The Settings page shows the routes in force, read-only, as --config
// declares them, said to be changed there; each destination an event goes
// to, when it was last sent to, what waits and failed; why the latest still
// unsent failed to an admin alone; no URL. Its words in every language, and
// its parts where assistive technology looks for them.
func TestRoutesShown(t *testing.T) {
	ctx := context.Background()
	s, hs, _ := routedServer(t, routesConfig, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{ana}, []string{"*"}
	})
	hs.ops.fail = 1
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "tune Errors", Kind: "tune"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.sendDue(ctx, "route:ops", time.Now()); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	page := func(who string) string {
		r := httptest.NewRequest(http.MethodGet, "/settings", nil)
		r.Header.Set("X-Forwarded-Email", who)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		b, _ := io.ReadAll(w.Body)
		return string(b)
	}
	admin, editor := page(ana), page(cy)
	for _, body := range []string{admin, editor} {
		for _, want := range []string{`<h3 id="routes">Routes</h3>`, "What happens is told where the routes below send it.",
			"Declared in <code><bdi>--config</bdi></code>, under notify:, and changed there, not here.",
			"<td><code>task.*</code></td><td><bdi>payments</bdi></td><td>any</td><td><code>payments-hook</code>, <code>team:payments</code></td><td>goes on to the next</td>",
			"what no route takes (the default)", "<code>ops</code> <span class=\"quiet\">(<code>--report-dest</code>)</span><br><span class=\"quiet\">in words of its own</span>",
			"<code>payments-hook</code> <span class=\"quiet\">(<code>--webhook-dest</code>)</span>", "a line, Slack&#39;s way"} {
			if !strings.Contains(body, want) {
				t.Errorf("/settings lacks %q", want)
			}
		}
		for _, url := range []string{hs.ops.file, "127.0.0.1"} {
			if strings.Contains(body, url) {
				t.Errorf("/settings shows %s", url)
			}
		}
		if strings.Contains(body, "<form") && strings.Contains(body, "notify") && strings.Contains(body, `name="route`) {
			t.Error("/settings has a form for routes")
		}
	}
	why := "sending to Slack: HTTP 500: down"
	if !strings.Contains(admin, why) || strings.Contains(editor, why) || !strings.Contains(editor, "failed: an admin sees why") {
		t.Errorf("why a notice failed: to the admin %v, to an editor %v", strings.Contains(admin, why), strings.Contains(editor, why))
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	pageA11y(t, h, ana, "/settings", "/manage")
	pageA11y(t, h, cy, "/settings")
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}

// Routes that can't be used are said, with their line, to an admin, and
// the line alone to anyone else; with none read since the start, that what
// happens is told as without them; and the events no route sent anywhere,
// on the page and in /metrics, beside each routed place's sent and failed
// at 0 from the start.
func TestRoutesBrokenShown(t *testing.T) {
	s, _, _ := routedServer(t, "notify:\n  routes:\n    - {to: [nowhere]}\n  default: [slack]\n", func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{ana}, []string{"*"}
	})
	if _, err := s.l.PutTask(context.Background(), ledger.Task{Title: "t", Kind: "tune"}, "ana"); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	r := httptest.NewRequest(http.MethodGet, "/settings", nil)
	r.Header.Set("X-Forwarded-Email", ana)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	admin := w.Body.String()
	r.Header.Set("X-Forwarded-Email", cy)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	editor := w.Body.String()
	said := `there is no destination called &#34;nowhere&#34;`
	if !strings.Contains(admin, "line 3: "+said) || strings.Contains(editor, said) || !strings.Contains(editor, "Line 3 of --config&#39;s notify: can&#39;t be used: an admin sees why.") {
		t.Errorf("why: admin %v, editor %v", strings.Contains(admin, said), strings.Contains(editor, said))
	}
	for _, want := range []string{"No routes have been read since lapilli serve started: what happens is told as without them.",
		"1 event since lapilli serve started went where no route sent it"} {
		if !strings.Contains(admin, want) {
			t.Errorf("/settings lacks %q", want)
		}
	}
	_, card, _ := get(t, h, "/manage")
	if !strings.Contains(card, "--config&#39;s notify: can&#39;t be used.") || !strings.Contains(card, "1 event went where no route sent it.") {
		t.Error("/manage doesn't say the routes can't be used")
	}
	_, metrics, _ := get(t, s.MetricsHandler(), "/metrics")
	for _, want := range []string{"lapilli_serve_notices_unrouted_total 1", `lapilli_serve_notices_total{to="route:payments-hook",result="sent"} 0`,
		`lapilli_serve_notices_total{to="route:team:payments",result="failed"} 0`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	// The webhook's said as before: once it sent.
	if strings.Contains(metrics, `to="webhook"`) {
		t.Error("/metrics says the webhook before it sent")
	}
}
