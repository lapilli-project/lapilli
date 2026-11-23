package serve

import (
	"context"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/verdict"
)

// pastRange is the test history, with Errors written as a range, errors > 5
// and errors < 50, held as errors > 5 is, and so with each end a fifth
// further, errors > 6 and errors < 40: each firing went well past both.
type pastRange struct{ replay.History }

func (p pastRange) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr == "errors > 5 and errors < 50" || expr == "errors > 6 and errors < 40" {
		expr = "errors > 5"
	}
	return p.History.QueryRange(ctx, expr, start, end, step)
}

// A rule written as a range, with no record in the week before: the digest
// says how many of its firings went well past both its ends, and the rule's
// page and MCP say it of the bounds that decided them, in each language.
func TestDigestUnrecordedRange(t *testing.T) {
	s, dir := server(t, func(c *Config) { c.Backfill = 10 * 24 * time.Hour; c.Impact = "" })
	range_ := strings.Replace(ruleFile, "expr: errors > 5", "expr: errors > 5 and errors < 50", 1)
	if err := os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(range_), 0o644); err != nil {
		t.Fatal(err)
	}
	s.h = pastRange{s.h}
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	r, ok, err := s.l.ForPeriod(ctx, "unrecorded", time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
	if err != nil || !ok {
		t.Fatalf("kept %v %v", ok, err)
	}
	var us []verdict.Unrecorded
	if err := r.Decode(&us); err != nil || len(us) != 1 {
		t.Fatalf("%+v %v", us, err)
	}
	if u := us[0]; u.Rule != "Errors" || u.Firings != 14 || u.Clear == nil || *u.Clear != 14 || u.Bound != "errors > 5, errors < 50" || u.Bounds != 2 {
		t.Errorf("%+v", u)
	}
	h := s.Handler()
	_, body, _ := get(t, h, "/rules/Errors")
	if want := "All 14 went well past the bounds that decided them (errors > 5, errors < 50), not a flicker at an edge: these happened."; !strings.Contains(html.UnescapeString(body), want) {
		t.Errorf("/rules/Errors: no %q", want)
	}
	_, out, err := s.toolRule(ctx, nil, ruleIn{Name: "Errors"})
	if u := out.Rule.Unrecorded; err != nil || u == nil || u.Clear == nil || *u.Clear != 14 || u.Bounds != 2 || !strings.Contains(u.Text, "the bounds that decided them") {
		t.Errorf("MCP: %+v %v", u, err)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	pageA11y(t, h, "", "/rules/Errors")
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}
