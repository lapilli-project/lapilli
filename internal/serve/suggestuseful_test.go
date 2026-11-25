package serve

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// With rules found noisy by what was done about their firings, the digest's
// longer "for" keeps every useful firing: with Errors noisy and one of its
// firings, a short one, said by a person to have needed action, the 15m for
// that would drop all of them (and was suggested while the digest was given
// no firings) is not suggested.
func TestDigestSuggestsKeepingUsefulFirings(t *testing.T) {
	ctx := context.Background()
	suggested := func(useful bool) []verdict.Suggestion {
		s, _ := server(t, func(c *Config) { c.Backfill = 10 * 24 * time.Hour; c.Impact = ""; keepRecord(c); c.Options.NoisyBy = verdict.NoisyByUseful })
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		from := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
		if useful {
			// The runs have to have found the firings first.
			s.jobs(ctx, true)
			fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: from.Add(7 * 24 * time.Hour)})
			if err != nil || len(fs) == 0 {
				t.Fatalf("firings: %d %v", len(fs), err)
			}
			yes := "yes"
			o, _ := s.options(ctx)
			if _, err := s.l.SetVerdict(ctx, ledger.FiringKey{ID: fs[0].ID, Series: fs[0].Series, Start: fs[0].Start}, ledger.VerdictChange{Action: &yes},
				ledger.VerdictBy{By: "ana", Admin: true, Now: time.Now()}, o); err != nil {
				t.Fatal(err)
			}
			// Worked out again with it.
			if err := s.l.SetMeta(ctx, "digest", "0"); err != nil {
				t.Fatal(err)
			}
			o, _ = s.options(ctx)
			if _, err := s.digest(ctx, o); err != nil {
				t.Fatal(err)
			}
		} else {
			s.jobs(ctx, true)
		}
		r, ok, err := s.l.ForPeriod(ctx, "suggestions", from)
		if err != nil || !ok {
			t.Fatalf("no suggestions kept: %v", err)
		}
		var sg []verdict.Suggestion
		if err := r.Decode(&sg); err != nil {
			t.Fatal(err)
		}
		return sg
	}
	if sg := suggested(false); len(sg) != 1 || sg[0].NewFor != 15*time.Minute {
		t.Fatalf("with none useful: %+v, want a for of 15m", sg)
	}
	if sg := suggested(true); len(sg) != 0 {
		t.Errorf("with a useful firing the 15m for drops: %+v", sg)
	}
}
