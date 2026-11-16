package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// TestCountedByAPI: what Grafana's data source proxy is asked is counted
// as Grafana's, --loki's as Loki's, and --prometheus's as Prometheus's,
// never by a URL or a data source's name.
func TestCountedByAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()
	var n prom.Counts
	ctx := prom.Counting(context.Background(), &n)
	t0 := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	p, err := Prometheus{URL: srv.URL}.History(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []interface {
		QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error)
	}{GrafanaLoki(srv.URL, "", "test", "lk", nil), Loki{URL: srv.URL}.History("test"), p} {
		if _, err := h.QueryRange(ctx, `count_over_time({app="shop"}[5m])`, t0, t0.Add(time.Hour), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	for _, api := range prom.APIs {
		if got := n.Of(api).Answers["2xx"]; got != 1 {
			t.Errorf("%s: %d", api, got)
		}
	}
}
