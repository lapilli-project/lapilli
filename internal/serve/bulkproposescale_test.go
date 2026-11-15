package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// TestBulkProposeAtLimitScale: at four thousand rules, a proposal for twenty
// rules by a reader who has made the hour's proposals already reads none of
// them: it is refused at once, each counted, without the rules parsed for
// each or read again.
func TestBulkProposeAtLimitScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	s, _ := changesServer(t, func(c *Config) { c.Editors = []string{bo} })
	const n = 4000
	var b strings.Builder
	b.WriteString("groups:\n")
	for g := 0; g < 20; g++ {
		fmt.Fprintf(&b, "  - name: g%d\n    rules:\n", g)
		for i := g; i < n; i += 20 {
			fmt.Fprintf(&b, "      - {alert: Rule%04d, expr: up == %d, for: 2m, labels: {severity: warning, tier: t%d}}\n", i, i, i%4)
		}
	}
	os.WriteFile(filepath.Join(s.cfg.RulesPath, "many.yaml"), []byte(b.String()), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	for i := range ledger.ProposalsAnHour {
		if _, err := s.l.Propose(ctx, ledger.Proposal{Rule: fmt.Sprint("Other", i), Kind: ledger.KindFor, Path: ledger.PathNone, MadeBy: bo}); err != nil {
			t.Fatal(err)
		}
	}
	var picks []string
	for i := range maxPropose {
		picks = append(picks, fmt.Sprintf("Rule%04d", i*97))
	}
	form := url.Values{"do": {"propose"}, "propose": {"retire"}, "rule": picks, "back": {"/rules"}}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	began := time.Now()
	code, _, hd := postAs(t, s.Handler(), "/rules/bulk", form, bo, "same-origin")
	took := time.Since(began)
	runtime.ReadMemStats(&m1)
	alloc := m1.TotalAlloc - m0.TotalAlloc
	t.Logf("a bulk retire of %d at %d rules, at the hour's limit: %d %s, %s, %d MB allocated", len(picks), n, code, hd.Get("Location"), took.Round(time.Millisecond), alloc>>20)
	if code != http.StatusSeeOther || !strings.Contains(hd.Get("Location"), fmt.Sprintf("limit=%d", maxPropose)) {
		t.Errorf("%d %s", code, hd.Get("Location"))
	}
	if budget := time.Duration(float64(50*time.Millisecond) * slow); took > budget || alloc > 32<<20 {
		t.Errorf("took %s and %d MB, over %s and 32 MB", took, alloc>>20, budget)
	}
	// Twenty names no rule in effect has, by an editor with every try left:
	// the rules in effect are read once for the request, not once a name.
	var absent []string
	for i := range maxPropose {
		absent = append(absent, fmt.Sprint("Gone", i))
	}
	form = url.Values{"do": {"propose"}, "propose": {"retire"}, "rule": absent, "back": {"/rules"}}
	runtime.GC()
	runtime.ReadMemStats(&m0)
	began = time.Now()
	code, _, hd = postAs(t, s.Handler(), "/rules/bulk", form, ana, "same-origin")
	took = time.Since(began)
	runtime.ReadMemStats(&m1)
	alloc = m1.TotalAlloc - m0.TotalAlloc
	t.Logf("a bulk retire of %d names not in effect at %d rules: %d %s, %s, %d MB allocated", len(absent), n, code, hd.Get("Location"), took.Round(time.Millisecond), alloc>>20)
	if code != http.StatusSeeOther || !strings.Contains(hd.Get("Location"), fmt.Sprintf("absent=%d", maxPropose)) {
		t.Errorf("%d %s", code, hd.Get("Location"))
	}
	if budget := time.Duration(float64(150*time.Millisecond) * slow); took > budget || alloc > 96<<20 {
		t.Errorf("took %s and %d MB, over %s and 96 MB", took, alloc>>20, budget)
	}
}
