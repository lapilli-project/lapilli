package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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
