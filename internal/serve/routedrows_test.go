package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// More pages than a view reads aren't counted short: the pages are read the
// earliest first, and from where they were cut the span is not known, while
// what lies before it is known and counted whole.
func TestPagesPastTheRowLimitAreNotKnown(t *testing.T) {
	parallel(t)
	am := routedFile(t, routedRepeatAM)
	s, _ := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
	ctx := context.Background()
	base := now.Add(-20 * 24 * time.Hour).Truncate(24 * time.Hour)
	end := base.Add(14 * 24 * time.Hour)
	f := firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "old"}, Start: base}
	if err := s.l.Record(ctx, ledger.Found{From: base, To: end, Run: ledger.Run{Kind: "test", Started: base, Finished: base, From: base, To: end}, Firings: []firing.Firing{f}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	for at := base; at.Before(end); at = at.Add(time.Hour) {
		s.workOutRouted(ctx, at, at.Add(time.Hour))
	}
	lo, hi := base.Add(7*24*time.Hour), base.Add(14*24*time.Hour)
	k, whole := pagesOver(t, s, lo, hi)
	if !k || whole < 20 {
		t.Fatalf("under the limit: known %v, %d pages", k, whole)
	}
	// Reading fewer pages than there are.
	const rows = 30
	s.routed.limits.pageRows = rows
	if k, _ := pagesOver(t, s, base, hi); k {
		t.Error("a span past the pages read is known")
	}
	if k, _ := pagesOver(t, s, lo, hi); k {
		t.Error("the later week, whose pages were cut, is known")
	}
	s.paged.Lock()
	s.paged.p = nil
	s.paged.Unlock()
	p, err := s.notified(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !p.routedMore || len(p.events) > rows {
		t.Fatalf("more %v, %d pages read of %d", p.routedMore, len(p.events), rows)
	}
	cut := p.routedCut
	if cut.IsZero() || !p.known(base, cut) {
		t.Errorf("the span before the cut %v is not known", cut)
	}
	if p.known(base, cut.Add(time.Hour)) {
		t.Errorf("a span reaching past the cut %v is known", cut)
	}
}

// A run whose look-back for the groups' last notifications begins in a
// stretch longer than the look-back that wasn't worked out says so, with what
// its counts assume, instead of counting the repeats from the stretch's start
// without a word.
func TestRoutedLongHoleBeforeTheHoursIsSaid(t *testing.T) {
	parallel(t)
	am := routedFile(t, routedRepeatAM)
	s, _ := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
	ctx := context.Background()
	base := now.Add(-20 * 24 * time.Hour).Truncate(24 * time.Hour)
	end := base.Add(14 * 24 * time.Hour)
	f := firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "old"}, Start: base}
	if err := s.l.Record(ctx, ledger.Found{From: base, To: end, Run: ledger.Run{Kind: "test", Started: base, Finished: base, From: base, To: end}, Firings: []firing.Firing{f}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	for at := base; at.Before(base.Add(20 * time.Hour)); at = at.Add(time.Hour) {
		s.workOutRouted(ctx, at, at.Add(time.Hour))
	}
	// Hours 20 to 60 aren't tried; the run of hour 60 begins its look-back in
	// them.
	at := base.Add(60 * time.Hour)
	s.workOutRouted(ctx, at, at.Add(time.Hour))
	c, err := s.l.RoutedCover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if why := c.Unknown(at, at.Add(time.Hour)); !strings.Contains(why, "longer than") || !strings.Contains(why, "on schedule") {
		t.Errorf("the caveat of a run after a long hole: %q", why)
	}
	// One right after hours worked out has none.
	s.workOutRouted(ctx, base.Add(20*time.Hour), base.Add(21*time.Hour))
	c, _ = s.l.RoutedCover(ctx)
	if why := c.Unknown(base.Add(20*time.Hour), base.Add(21*time.Hour)); why != "" {
		t.Errorf("a caveat with no hole before: %q", why)
	}
}
