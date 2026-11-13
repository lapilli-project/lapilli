package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// The digest's header names the zone serve counts days in, with each
// offset its period took: a week the clocks went back in, in Berlin.
func TestDigestZone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no zone database here")
	}
	s, _ := server(t, func(c *Config) { c.Loc = berlin })
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, berlin)
	to := from.AddDate(0, 0, 14)
	text, err := s.digestText(context.Background(), &verdict.Report{From: from, To: to}, nil, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Lapilli  2026-10-19 00:00 → 2026-11-02 00:00 Europe/Berlin (+02:00 → +01:00)\n"; !strings.HasPrefix(text, want) {
		t.Errorf("the digest's header:\n%s\nwant %q", text, want)
	}
}
