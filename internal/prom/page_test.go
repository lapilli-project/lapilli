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
		"<html><body>\x1b[31mred‮</body></html>":            `an HTML page, "[31mred"`,
	} {
		if got, ok := pageSaid(text, nil); !ok || got != want {
			t.Errorf("%.40q: %q %v, want %q", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "plain text", `{"status":"error","error":"<html>"}`, "<notapage>", "a <html> later"} {
		if got, ok := pageSaid(text, nil); ok {
			t.Errorf("%q: a page, %q", text, got)
		}
	}
	got, _ := pageSaid("<title>"+strings.Repeat("ab ", 200)+"</title>", nil)
	if len(got) > len(`an HTML page, ""`)+pageSaidMost || !strings.HasSuffix(got, `…"`) {
		t.Errorf("a long title: %d bytes, %q", len(got), got)
	}
}

// What the request carried to sign in is taken out of a page's text after
// its entities are read and before it is cut: no token, whole or begun, is
// said.
func TestPageSaidSecrets(t *testing.T) {
	const token = "s3cret-token-0123456789"
	req := httptest.NewRequest(http.MethodGet, "http://prom.example/api/v1/targets", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for _, text := range []string{
		"<html><title>" + token + "</title></html>",
		"<html><title>&#115;3cret-token-0123456789</title></html>",
		"<html><body>signed in as " + token + "</body></html>",
		"<html><title>" + strings.Repeat("a", pageSaidMost-10) + " " + token + "</title></html>",
	} {
		got, ok := pageSaid(text, req)
		if !ok || strings.Contains(got, "s3cr") || strings.Contains(got, "3cret") {
			t.Errorf("%.60q: %q", text, got)
		}
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

// A page that says the answer was too large is read so as before: the
// words, not only the title, say it.
func TestPageTooBig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		w.Write([]byte("<html><title>413</title><body>response too large</body></html>"))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "test").Targets(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || !ae.tooBig() || err.Error() != `HTTP 413: an HTML page, "413"` {
		t.Errorf("%v", err)
	}
}
