package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// A rule only copied in is known in every span of its copies the ledger
// keeps, not only the latest: a week inside the earlier span is judged,
// and so is a catch of an incident there; one in the gap isn't
// (TestExpectCopiedOutsideSpan).
func TestExpectCopiedEarlierSpan(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.RulesPath, c.Impact = "", "" })
	early := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if _, err := s.l.PutFirings(ctx, "other", early, early.Add(7*24*time.Hour),
		[]firing.Firing{{Rule: "Else", Start: early.Add(time.Hour), End: early.Add(2 * time.Hour)}}, ledger.FiringsCopier, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	// Two spans: Aug 31 to Sep 14, and Oct 5 to 19.
	for _, from := range []time.Time{time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)} {
		f := []firing.Firing{{Rule: "Copied", Labels: map[string]string{"service": "x"}, Start: from.Add(26 * time.Hour), End: from.Add(27 * time.Hour)}}
		if _, err := s.l.PutFirings(ctx, "pager", from, from.Add(14*24*time.Hour), f, ledger.FiringsCopier, s.cfg.Options); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(t.TempDir(), "expect.yaml")
	os.WriteFile(file, []byte("expectations:\n  - rule: Copied\n    budget: {week: 5}\n    catches: [{incident: INC-8, within: 5m}]\n"), 0o600)
	s.cfg.Expect = file
	inc := filepath.Join(t.TempDir(), "incidents.yaml")
	os.WriteFile(inc, []byte("incidents:\n  - {id: INC-8, title: In the earlier span, start: 2026-09-09T06:01:00Z, end: 2026-09-09T06:40:00Z, labels: {service: x}}\n"), 0o644)
	s.cfg.Incidents = inc
	s.jobs(ctx, true)
	expectReadTwice(t, s)
	checkAgain(t, s)
	cs := checksOf(t, s, "Copied")
	for _, w := range []string{"2026-08-31", "2026-09-07", "2026-10-05", "2026-10-12"} {
		if c := cs["budget week "+w]; c.Result != ledger.ExpectMet {
			t.Errorf("week %s, inside a span: %+v", w, c)
		}
	}
	if c := cs["catches INC-8"]; c.Result != ledger.ExpectBroken || c.Value != "✗" {
		t.Errorf("a catch of an incident inside the earlier span, not caught: %+v", c)
	}
	for _, w := range []string{"2026-08-24", "2026-09-14", "2026-09-28", "2026-10-19"} {
		if c := cs["budget week "+w]; c.Result != ledger.ExpectUnknown || c.Why != whyNotCopied {
			t.Errorf("week %s, outside the spans: %+v", w, c)
		}
	}
}
