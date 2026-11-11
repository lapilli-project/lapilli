package prom

import (
	"context"
	"net/http"
	"net/http/httptest"
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
