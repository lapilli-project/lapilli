package source

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The history's API follows no redirect, as every other client Lapilli
// has: a Prometheus, or whatever answers in its name, can't send the query,
// the bearer token, or AWS's session token to another server. Go would
// send the token on to the same host's other ports (both servers here are
// 127.0.0.1), the session token to any host, and keep a 307's body.
func TestPrometheusFollowsNoRedirect(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "session-token-0123456789")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("prom-bearer-0123456789\n"), 0o600)

	var mu sync.Mutex
	var elsewhere []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		elsewhere = append(elsewhere, r.Header.Get("Authorization")+" "+r.Header.Get("X-Amz-Security-Token")+" "+string(b))
		mu.Unlock()
		io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer other.Close()
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusFound} {
		prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/api/v1/query_range", code)
		}))
		for _, p := range []Prometheus{
			{URL: prom.URL, TokenFile: tok},
			{URL: prom.URL, Auth: "aws", AWSRegion: "us-east-1"},
		} {
			h, err := p.History(context.Background(), "lapilli/test")
			if err != nil {
				t.Fatal(err)
			}
			end := time.Unix(1_800_000_000, 0)
			if _, err := h.QueryRange(context.Background(), `rate(errors_total{customer="acme"}[5m]) > 0.1`, end.Add(-time.Hour), end, time.Minute); err == nil {
				t.Errorf("%d, auth %q: a redirect was taken for an answer", code, p.Auth)
			}
		}
		prom.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(elsewhere) > 0 {
		t.Errorf("a redirect was followed, to another server: %q", elsewhere)
	}
}
