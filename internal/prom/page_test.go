package prom

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A managed service's frontend answers a path it doesn't serve with its
// own error page: markup, a style sheet, the path asked.
const frontend404 = `<!DOCTYPE html>
<html lang=en>
  <meta charset=utf-8>
  <meta name=viewport content="initial-scale=1, minimum-scale=1, width=device-width">
  <title>Error 404 (Not Found)!!1</title>
  <style>
    *{margin:0;padding:0}html,code{font:15px/22px arial,sans-serif}html{background:#fff;color:#222;padding:15px}
  </style>
  <a href=//www.example.com/><span id=logo aria-label=Example></span></a>
  <p><b>404.</b> <ins>That’s an error.</ins>
  <p>The requested URL <code>/api/v1/status/config</code> was not found on this server.  <ins>That’s all we know.</ins>
`

const secret = "s3cret-token-0123456789"

// leaks reports whether s holds secret, or a start of it four bytes long
// or more, or its end.
func leaks(s string) bool {
	return strings.Contains(s, secret[:4]) || strings.Contains(s, secret[1:5]) || strings.Contains(s, secret[len(secret)-8:])
}

// signed is a request that carried secret to sign in.
func signed() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://prom.example/api/v1/targets", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	return req
}

// An HTML page is said by its title, or with none by its text, never by
// its markup, its scripts or its styles; what isn't a page isn't one.
func TestPageSaid(t *testing.T) {
	for text, want := range map[string]string{
		frontend404: `an HTML page, "Error 404 (Not Found)!!1"`,
		"<html><body><h1>502 Bad Gateway</h1></body></html>":                                                                  `an HTML page, "502 Bad Gateway"`,
		`<html><head><script>var said = "a script";</script><style>body{color:red}</style></head><body>Sign in</body></html>`: `an HTML page, "Sign in"`,
		"<html></html>":                                     "an HTML page",
		"<HTML><TITLE>A &amp; B</TITLE></HTML>":             `an HTML page, "A & B"`,
		"<?xml version=\"1.0\"?>\n<html><title>x</title>":   `an HTML page, "x"`,
		"  <!doctype html><title>\n  two\n  lines </title>": `an HTML page, "two lines"`,
		"<html><body>\x1b[31mred\u202e</body></html>":       `an HTML page, "[31mred"`,
		"\ufeff<!DOCTYPE html><title>with a BOM</title>":    `an HTML page, "with a BOM"`,
		"<html><body>said<plaintext><b>not said</b></body>": `an HTML page, "said"`,
	} {
		if got, ok := pageSaid(text, false, nil); !ok || got != want {
			t.Errorf("%.40q: %q %v, want %q", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "plain text", `{"status":"error","error":"<html>"}`, "<notapage>", "a <html> later",
		"<error><header>x</header>quota exceeded</error>", "<htmlish>x</htmlish>"} {
		if got, ok := pageSaid(text, false, nil); ok {
			t.Errorf("%q: a page, %q", text, got)
		}
	}
	got, _ := pageSaid("<title>"+strings.Repeat("ab ", 200)+"</title>", false, nil)
	if len(got) > len(`an HTML page, ""`)+pageSaidMost || !strings.HasSuffix(got, `…"`) {
		t.Errorf("a long title: %d bytes, %q", len(got), got)
	}
}

// What the request carried to sign in is taken out of a page's text after
// its entities are read and before it is cut, though white space or a
// character that doesn't show split it in two; and of a page cut short,
// the text it ends in, where a secret's start would be, isn't said.
func TestPageSaidSecrets(t *testing.T) {
	half := secret[:13] // "s3cret-token-"
	for _, c := range []struct {
		text string
		cut  bool
	}{
		{"<html><title>" + secret + "</title></html>", false},
		{"<html><title>&#115;3cret-token-0123456789</title></html>", false},
		{"<html><body>signed in as " + secret + "</body></html>", false},
		{"<html><title>" + strings.Repeat("a", pageSaidMost-10) + " " + secret + "</title></html>", false},
		{"<html><title>" + half + "\n" + secret[13:] + "</title></html>", false},
		{"<html><title>" + half + "\t" + secret[13:] + "</title></html>", false},
		{"<html><title>" + half + "&#10;" + secret[13:] + "</title></html>", false},
		{"<html><body>signed in as " + half + "\r\n" + secret[13:] + "</body></html>", false},
		{"<html><body>signed in as " + half + "\u00ad" + secret[13:] + "</body></html>", false},
		{"<html><body>signed in as " + secret[:10], true},
		{"<html><body>signed in as &#115;3cret-tok", true},
		{"<html><body>signed in as " + secret[:10] + "\nen", true},
		{"<html><title>signed in as " + secret[:10], true},
	} {
		got, ok := pageSaid(c.text, c.cut, signed())
		if !ok || leaks(got) {
			t.Errorf("%.70q, cut %v: %q", c.text, c.cut, got)
		}
	}
	// Cut, the text before the one it ends in is said.
	if got, _ := pageSaid("<html><title>Sign in</title><body>as "+secret[:10], true, signed()); got != `an HTML page, "Sign in"` {
		t.Errorf("cut after the title: %q", got)
	}
}

// A path a managed service doesn't serve, its frontend's 404 page: the
// error says the status and the page's title, not its markup; and a page
// in place of a range answer is not the API's, said so, as before.
func TestPageErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		if r.URL.Path == "/api/v1/query_range" {
			w.Write([]byte(frontend404))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(frontend404))
	}))
	defer srv.Close()
	c := New(srv.URL, "test")
	ctx := context.Background()
	_, err1 := c.StatusConfig(ctx)
	_, err2 := c.Targets(ctx)
	for _, err := range []error{err1, err2} {
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != 404 || err.Error() != `HTTP 404: an HTML page, "Error 404 (Not Found)!!1"` {
			t.Errorf("%v", err)
		}
	}
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_, err := c.QueryRange(ctx, "up", t0, t0.Add(time.Hour), time.Minute)
	var ae *APIError
	if !errors.As(err, &ae) || !ae.page || Refused(err) || strings.Contains(err.Error(), "<") ||
		!strings.HasSuffix(err.Error(), `not a Prometheus API response: an HTML page, "Error 404 (Not Found)!!1"`) {
		t.Errorf("%v", err)
	}
}

// A page longer than is read of it, its text cut in a secret: at the cut
// pageSaid makes of a 403's page, and at the head a range answer keeps,
// the secret written as entities. No start of it is said.
func TestPageCutSecrets(t *testing.T) {
	// The style fills what is read but for the text, so that the text is
	// the page's first, and the cut falls ten bytes into the secret.
	page := func(read int, text string) string {
		start := "<html><style>"
		end := "</style><body>signed in as "
		return start + strings.Repeat("a", read-len(start)-len(end)-10) + end + text + "</body></html>"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query_range" {
			w.Write([]byte(page(headKept, "&#115;3cret-token-0123456789")))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(page(pageRead, secret)))
	}))
	defer srv.Close()
	c := New(srv.URL, "t", WithToken(func(context.Context) (string, error) { return secret, nil }))
	_, err := c.Targets(context.Background())
	if err == nil || leaks(err.Error()) || !strings.HasPrefix(err.Error(), "HTTP 403: an HTML page") {
		t.Errorf("cut at what is read: %v", err)
	}
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_, err = c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
	if err == nil || leaks(err.Error()) || !strings.Contains(err.Error(), "not a Prometheus API response: an HTML page") {
		t.Errorf("cut at the head kept: %v", err)
	}
}

// Whether a page says the answer was too large is read from the start of
// it, as before, and from its title: a title that says so says so.
func TestPageTooBig(t *testing.T) {
	for body, status := range map[string]int{
		"<html><title>413</title><body>response too large</body></html>": http.StatusRequestEntityTooLarge,
		"<html><title>response too large</title></html>":                 http.StatusServiceUnavailable,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		_, err := New(srv.URL, "test").Targets(context.Background())
		srv.Close()
		var ae *APIError
		if !errors.As(err, &ae) || !ae.tooBig() || !strings.HasPrefix(err.Error(), "HTTP ") || strings.Contains(err.Error(), "<") {
			t.Errorf("%d: %v", status, err)
		}
	}
}

// Loki's plain-text error is Loki's when its first 200 bytes have no
// markup, as before: what follows them isn't looked at.
func TestLokiTextLong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(strings.Repeat("parse error ", 30) + "near <x>"))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "test", Loki()).QueryRange(context.Background(), `count_over_time({app="x"}[1m])`, time.Unix(0, 0), time.Unix(60, 0), time.Minute)
	var ae *APIError
	if !errors.As(err, &ae) || ae.page || !Refused(err) || !strings.HasPrefix(err.Error(), "HTTP 400: parse error") {
		t.Errorf("%v", err)
	}
}
