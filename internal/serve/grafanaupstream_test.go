package serve

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// What a Grafana lapilli serve reads can do to it (0.34.1).

// hungGrafana is grafanaStandIn, but once hang is set, its rules' export
// is never answered: a Grafana whose database is locked, or one holding the
// connection open on purpose. It answers the headers' wait with nothing.
func hungGrafana(t *testing.T, more func(*Config)) (*Server, *atomic.Bool, *atomic.Int32, func()) {
	t.Helper()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	var hang atomic.Bool
	var asked atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() && r.URL.Path == "/api/v1/provisioning/alert-rules/export" {
			asked.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		g.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(free) // before ts.Close, which waits for the handler
	s, _ := changesServer(t, func(c *Config) {
		c.RulesPath, c.Grafana = "", ts.URL
		c.ChangesGrafanaToken = tokenFile(t, "grafana-writes-0123456789")
		if more != nil {
			more(c)
		}
	})
	return s, &hang, &asked, free
}

// With --changes and --grafana, the weekly proposals (proposeWeekly ->
// proposeFound -> propose -> current(fresh) -> readRules -> source.Grafana)
// read Grafana's export with the work loop's own context, which has no
// deadline. A Grafana that takes the request and never answers would hold
// the work loop, and with it every hourly run, digest, change and report:
// the read is given the minute the rules' own reads get (look), here a
// second.
func TestHungGrafanaLetsTheWorkLoopGo(t *testing.T) {
	was := rulesReadTime
	t.Cleanup(func() { rulesReadTime = was })
	rulesReadTime = time.Second
	s, hang, asked, free := hungGrafana(t, func(c *Config) { c.Timeout = 2 * time.Second })
	ctx := context.Background() // as work(ctx) runs: Run's context, no deadline
	if err := s.l.SetMeta(ctx, foundKey, ""); err != nil {
		t.Fatal(err)
	}
	hang.Store(true)
	done := make(chan struct{})
	start := time.Now()
	go func() { s.jobs(ctx, true); close(done) }()
	const bound = 10 * time.Second
	select {
	case <-done:
		t.Logf("jobs returned after %s; the export was asked %d times", time.Since(start), asked.Load())
	case <-time.After(bound):
		where := ""
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "source.Grafana(") {
				for _, l := range strings.Split(g, "\n") {
					if strings.Contains(l, "lapilli.dev/internal/") && !strings.HasPrefix(strings.TrimSpace(l), "/") {
						where += "\n  " + funcName(strings.TrimSpace(l))
					}
				}
			}
		}
		t.Errorf("lapilli serve's work loop is still held by a Grafana that doesn't answer after %s (the read is given %s); "+
			"the export was asked %d time(s); held at:%s", time.Since(start).Round(time.Second), rulesReadTime, asked.Load(), where)
	}
	free()
	<-done
}

// funcName is a stack line's function, without its arguments.
func funcName(l string) string {
	cut := len(l)
	for _, a := range []string{"(0x", "({", "(_", "()", "(..."} {
		if i := strings.Index(l, a); i >= 0 && i < cut {
			cut = i
		}
	}
	return l[:cut]
}

// A Grafana (or a proxy before it) that echoes the token it refused: the
// error lapilli serve fails reading the rules with is the status line of
// every page, for anyone who may read a page, and holds no token.
func TestGrafanaEchoedTokenNotOnPages(t *testing.T) {
	const token = "glsa_s3cretTokenValue_0123456789abcdef"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized: refused header Authorization: "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	defer ts.Close()
	s, _ := server(t, func(c *Config) {
		c.RulesPath, c.Grafana, c.GrafanaToken = "", ts.URL, tokenFile(t, token)
	})
	raw := ""
	err := s.look(context.Background(), &raw)
	if err == nil {
		t.Fatal("the rules read")
	}
	s.fail("reading the rules", err) // as watch does
	s.ready.Store(true)
	code, body, _ := get(t, s.Handler(), "/")
	t.Logf("GET / (no role asked for): %d, the status line says the token: %v", code, strings.Contains(body, token))
	if strings.Contains(body, token) {
		t.Errorf("the page shows Grafana's token: %d bytes, %q", len(body), body[max(0, strings.Index(body, token)-80):strings.Index(body, token)+len(token)])
	}
	if !strings.Contains(body, "HTTP 401: unauthorized: refused header Authorization: [redacted]") {
		i := strings.Index(body, "HTTP 401")
		t.Errorf("the page doesn't say why the rules weren't read: %q", body[max(i, 0):min(len(body), max(i, 0)+200)])
	}
}

// A change read back from Grafana, refused by a Grafana that echoes the
// write token bare: the reason, kept and sent to Slack, doesn't hold it.
func TestGrafanaWriteEchoedToken(t *testing.T) {
	const token = "glsa_writer-0123456789abcdef"
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/provisioning/alert-rules/abc" {
			http.Error(w, "no rights for "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), http.StatusForbidden)
			return
		}
		g.ServeHTTP(w, r)
	}))
	defer ts.Close()
	s, _ := changesServer(t, func(c *Config) {
		c.RulesPath, c.Grafana = "", ts.URL
		c.ChangesGrafanaToken = tokenFile(t, token)
	})
	_, _, _, err := s.grafanaRule(context.Background(), "abc")
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "no rights for") {
		t.Errorf("Grafana's refusal: %v", err)
	}
}

// Grafana's error, said in a change's reason, holds no bearer token, nor
// what doesn't show.
func TestGrafanaFailedText(t *testing.T) {
	err := grafanaFailed(401, []byte("refused Authorization: Bearer glsa_writer-0123456789abcdef \x1b[2J"))
	if m := err.Error(); strings.Contains(m, "glsa_") || strings.ContainsRune(m, 0x1b) || !strings.Contains(m, "Bearer [redacted]") {
		t.Errorf("%q", m)
	}
}

// bigConfig is a history whose Prometheus's configuration is larger than
// is read of it.
type bigConfig struct{ *history }

func (*bigConfig) StatusConfig(context.Context) (string, error) {
	return "", fmt.Errorf("the answer is over 2 MiB: %w", prom.ErrTooLarge)
}

// A configuration too large to read turns the range check off, and says
// so in the log, as a warning: whoever runs lapilli can give
// --scrape-interval.
func TestConfigTooLargeSaid(t *testing.T) {
	var logs bytes.Buffer
	s, _ := server(t, nil)
	s.h, s.log = &bigConfig{&history{}}, slog.New(slog.NewTextHandler(&logs, nil))
	s.askScrape(context.Background())
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "--scrape-interval") {
		t.Errorf("not said: %s", logs.String())
	}
}
