package serve

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// withRulesAPI is the test history with a rules API beside it.
type withRulesAPI struct {
	*history
	answer string
	err    error
	asked  *atomic.Int64
}

func (h withRulesAPI) Rules(context.Context) ([]byte, error) {
	h.asked.Add(1)
	return []byte(h.answer), h.err
}

// lapilli serve places the groups it reads from rule files where the
// history's rules API runs the group of their name, with one request of
// the API a read of the rules; one that can't be read leaves the groups
// as placed before, or on the grid.
func TestServePlacesRuleFiles(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, dir := server(t, nil)
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte("groups:\n  - name: shop\n    interval: 5m\n    rules:\n      - alert: Errors\n        expr: errors > 5\n"), 0o644)
	var asked atomic.Int64
	h := withRulesAPI{history: &history{}, asked: &asked,
		answer: `{"status":"success","data":{"groups":[{"name":"shop","file":"/etc/rules/shop.yaml","interval":300,"lastEvaluation":"2026-11-02T06:02:30.2Z","rules":[]}]}}`}
	s.h = h
	phase := func() time.Duration {
		vs, err := s.versions(ctx, now.Add(-time.Hour), now)
		if err != nil || len(vs) == 0 || len(vs[len(vs)-1].Rules) == 0 {
			t.Fatalf("%v %v", vs, err)
		}
		return vs[len(vs)-1].Rules[0].Phase
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if got := phase(); got != 2*time.Minute+30*time.Second {
		t.Errorf("placed at %s, want 2m30s", got)
	}
	if asked.Load() != 1 {
		t.Errorf("the rules API was asked %d times, want once a read", asked.Load())
	}
	// The API can't be read: kept as it was.
	h.err = errors.New("HTTP 503")
	s.h = h
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if got := phase(); got != 2*time.Minute+30*time.Second {
		t.Errorf("after a failed read: placed at %s, want as last read", got)
	}
}

// The rules API's answer changes at every evaluation, and what groups are
// placed by doesn't: a read of the rules that finds the groups where they
// were reads no version of them from the ledger again.
func TestServeListingChangeOnlyWhenPlacesDo(t *testing.T) {
	// Not parallel: it counts namedReads, which every test adds to.
	ctx := context.Background()
	s, dir := server(t, nil)
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte("groups:\n  - name: shop\n    interval: 5m\n    rules:\n      - alert: Errors\n        expr: errors > 5\n"), 0o644)
	var asked atomic.Int64
	answer := func(last string) withRulesAPI {
		return withRulesAPI{history: &history{}, asked: &asked,
			answer: `{"status":"success","data":{"groups":[{"name":"shop","file":"/etc/rules/shop.yaml","interval":300,"lastEvaluation":"` + last + `","evaluationTime":0.001,"rules":[]}]}}`}
	}
	raw := ""
	s.h = answer("2026-11-02T06:02:30.2Z")
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	reads := func() int64 {
		before := namedReads.Load()
		if _, err := s.versions(ctx, now.Add(-time.Hour), now); err != nil {
			t.Fatal(err)
		}
		return namedReads.Load() - before
	}
	if n := reads(); n == 0 {
		t.Fatal("the first parse read nothing")
	}
	// The group ran again, five minutes later: placed where it was.
	s.h = answer("2026-11-02T06:07:30.3Z")
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if n := reads(); n != 0 {
		t.Errorf("%d versions read again though no group moved", n)
	}
	// It moved: parsed again.
	s.h = answer("2026-11-02T06:07:31Z")
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if n := reads(); n == 0 {
		t.Error("a group moved and nothing was parsed again")
	}
}

// A listing read that wasn't sent, its turn by --max-rate coming past the
// deadline, is said at info, once, as not asked; one that failed is said at
// warn. Each is said once until a read answers.
func TestSetListingSaysWhyItWasNotAsked(t *testing.T) {
	parallel(t)
	// The error a real client gives: the first request takes the rate's
	// turn, and the next one's would come past its 30 seconds.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
	}))
	defer api.Close()
	c := prom.New(api.URL, "test", prom.WithLimiter(prom.NewLimiter(0.01)))
	if _, err := c.RulesOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, notSent := c.RulesOnce(context.Background())
	if !prom.TurnPastDeadline(notSent) {
		t.Fatalf("a second request at 0.01 a second: %v", notSent)
	}
	cases := []struct {
		name  string
		err   error
		level string
		text  string
	}{
		{"turn past", notSent, "level=INFO", "wasn't asked"},
		{"failed", errors.New("connection refused"), "level=WARN", "couldn't be read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := signalServer(t, shopHistory(), nil)
			var buf strings.Builder
			s.log = slog.New(slog.NewTextHandler(&buf, nil))
			var at *rules.Listing
			bad := false
			s.setListing(&at, &bad, "the history's rules API", nil, c.err)
			s.setListing(&at, &bad, "the history's rules API", nil, c.err)
			out := buf.String()
			if strings.Count(out, "\n") != 1 || !strings.Contains(out, c.level) || !strings.Contains(out, c.text) || !bad {
				t.Fatalf("logged %q, bad %v; want one %s line with %q", out, bad, c.level, c.text)
			}
		})
	}
}
