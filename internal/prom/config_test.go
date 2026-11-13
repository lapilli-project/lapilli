package prom

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Prometheus's configuration is read as /api/v1/status/config serves it;
// a store that doesn't serve it says so.
func TestStatusConfig(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prom/api/v1/status/config":
			w.Write([]byte(`{"status": "success", "data": {"yaml": "global:\n  scrape_interval: 30s\n"}}`))
		case "/odd/api/v1/status/config":
			w.Write([]byte(`<html>a sign-in page</html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	got, err := New(ts.URL+"/prom", "test").StatusConfig(context.Background())
	if err != nil || got != "global:\n  scrape_interval: 30s\n" {
		t.Errorf("%q %v", got, err)
	}
	for _, base := range []string{"/managed", "/odd"} {
		if _, err := New(ts.URL+base, "test").StatusConfig(context.Background()); err == nil {
			t.Errorf("%s: no error", base)
		}
	}
}

// A configuration over 2 MiB, or targets over 32 MiB, are refused before
// they are parsed: what is read is parsed whole, and they are reads lapilli
// does without. A configuration of hundreds of jobs is a few hundred KB.
func TestStatusConfigCapped(t *testing.T) {
	big := strings.Repeat("0,", maxConfig/2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status/config":
			fmt.Fprintf(w, `{"status": "success", "data": {"yaml": "global:\n  scrape_interval: 30s\npad: [%s]\n"}}`, big)
		case "/api/v1/targets":
			w.Write([]byte(`{"status":"success","data":{"activeTargets":[`))
			for range maxTargets / 64 {
				w.Write([]byte(`{"labels":{"job":"a"},"scrapeInterval":"30s","scrapePool":"a"}, `))
			}
		}
	}))
	defer ts.Close()
	c := New(ts.URL, "test")
	if got, err := c.StatusConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "over 2 MiB") {
		t.Errorf("a configuration of %d bytes: %d bytes, %v", len(big), len(got), err)
	}
	if got, err := c.Targets(context.Background()); err == nil || !strings.Contains(err.Error(), "over 32 MiB") {
		t.Errorf("targets over 32 MiB: %d bytes, %v", len(got), err)
	}
}
