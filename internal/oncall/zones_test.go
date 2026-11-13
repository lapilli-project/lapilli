package oncall

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The zones a feed's TZIDs name are kept loaded, maxZones of them (64
// here): a feed that named 64 zones first, once each, and then dates in a
// 65th, had that zone loaded from disk again for each date
// (TestFeedZoneEachDate's feed, a fifth of it, behind 64 decoys), some 2
// seconds, the whole 8 MB over 11. A zone past maxZones is refused.
func TestFeedZoneCacheFull(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	s := cfg.Schedules[0]
	var zones []string
	for i := 0; i <= 12; i++ {
		zones = append(zones, fmt.Sprintf("Etc/GMT+%d", i))
	}
	for i := 0; i <= 14; i++ {
		zones = append(zones, fmt.Sprintf("Etc/GMT-%d", i))
	}
	for _, z := range []string{"Asia/Seoul", "Asia/Tokyo", "Asia/Shanghai", "Asia/Singapore", "Asia/Kolkata", "Asia/Dubai",
		"Asia/Jakarta", "Asia/Manila", "Asia/Bangkok", "Asia/Taipei", "Asia/Hong_Kong", "Asia/Karachi", "Europe/London",
		"Europe/Paris", "Europe/Berlin", "Europe/Madrid", "Europe/Rome", "Europe/Amsterdam", "Europe/Stockholm",
		"Europe/Warsaw", "Europe/Kyiv", "Europe/Istanbul", "Europe/Lisbon", "Europe/Dublin", "America/Chicago",
		"America/Denver", "America/Los_Angeles", "America/Toronto", "America/Sao_Paulo", "America/Mexico_City",
		"America/Bogota", "America/Lima", "Australia/Sydney", "Australia/Perth", "Pacific/Auckland", "Africa/Cairo"} {
		zones = append(zones, z)
	}
	// The cache made as small as the zones here can fill it.
	was := maxZones
	t.Cleanup(func() { maxZones = was })
	maxZones = 64
	if len(zones) < maxZones {
		t.Fatalf("%d zones", len(zones))
	}
	zones = zones[:maxZones]
	var b strings.Builder
	for i, z := range zones {
		if _, err := LoadZone(z); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:d%d\r\nDTSTART;TZID=%s:20250101T000000\r\nDURATION:PT1H\r\nSUMMARY:nobody\r\nEND:VEVENT\r\n", i, z)
	}
	decoys := b.String()
	feed := zonedExdates(100, 1000) // America/New_York, the 65th
	data := append([]byte("BEGIN:VCALENDAR\r\n"+decoys), feed[len("BEGIN:VCALENDAR\r\n"):]...)
	if len(data) > MaxFeed {
		t.Fatalf("%d bytes, over MaxFeed", len(data))
	}
	const bound = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 3*bound/2)
	defer cancel()
	began := time.Now()
	_, err := Parse(ctx, data, s, cfg, now)
	took := time.Since(began)
	t.Logf("%d bytes, 64 zones then 100 events x 1,000 EXDATEs in a 65th: %s, err %v", len(data), took, err)
	if took > bound || err != nil {
		t.Errorf("1.6 MB of zoned dates behind 64 zones isn't read within %s: %s, %v", bound, took, err)
	}
}
