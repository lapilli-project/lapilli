package verdict

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A header names the zone its times are in, with its offset: never Go's
// bare "Local".
func TestZone(t *testing.T) {
	load := func(name string) *time.Location {
		t.Helper()
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("no zone %s here: %v", name, err)
		}
		return loc
	}
	winter := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	autumn := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		loc      *time.Location
		from, to time.Time
		want     string
	}{
		{time.UTC, winter, summer, "UTC"},
		{load("UTC"), winter, summer, "UTC"},
		// Debian's and Ubuntu's /etc/localtime, and UTC's other names.
		{load("Etc/UTC"), winter, summer, "UTC"},
		{load("Etc/UCT"), winter, summer, "UTC"},
		{load("Etc/Zulu"), winter, summer, "UTC"},
		{load("Asia/Seoul"), winter, summer, "Asia/Seoul (+09:00)"},
		{load("Asia/Kolkata"), winter, winter, "Asia/Kolkata (+05:30)"},
		{load("America/St_Johns"), winter, winter, "America/St_Johns (-03:30)"},
		// The clocks went back during the period; forward and back.
		{load("Europe/Berlin"), autumn, autumn.AddDate(0, 0, 14), "Europe/Berlin (+02:00 → +01:00)"},
		{load("Europe/Berlin"), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			"Europe/Berlin (+01:00 → +02:00 → +01:00)"},
		{load("Europe/Berlin"), winter, winter.AddDate(0, 0, 7), "Europe/Berlin (+01:00)"},
		{load("Europe/London"), winter, winter, "Europe/London (+00:00)"},
		{time.FixedZone("KST", 9*3600), winter, winter, "KST (+09:00)"},
		{time.FixedZone("", -5*3600), winter, winter, "-05:00"},
		{time.FixedZone("+09", 9*3600), winter, winter, "+09:00"},
	} {
		if got := Zone(c.loc, c.from, c.to); got != c.want {
			t.Errorf("%s: %q, want %q", c.loc, got, c.want)
		}
	}
	if got := Zone(time.Local, winter, summer); strings.Contains(got, "Local") || got == "" {
		t.Errorf("the machine's own zone: %q", got)
	}
	// A zone file copied, not linked: no name, each abbreviation with its
	// offset.
	if b, err := os.ReadFile("/usr/share/zoneinfo/Europe/Berlin"); err == nil {
		loc, err := time.LoadLocationFromTZData("", b)
		if err != nil {
			t.Fatal(err)
		}
		if got := Zone(loc, autumn, autumn.AddDate(0, 0, 14)); got != "CEST (+02:00) → CET (+01:00)" {
			t.Errorf("a copied zone file: %q", got)
		}
	}
}

// The machine's own zone is named as Go finds it: TZ, or the zone file
// /etc/localtime links to; a name that isn't that zone is no name.
func TestLocalZoneName(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip("no zone database here")
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	env := func(tz string, set bool) func(string) (string, bool) {
		return func(string) (string, bool) { return tz, set }
	}
	// link resolves /etc/localtime, or the path TZ names, to p, the file
	// at the end of its links.
	link := func(p string) func(string) (string, error) {
		return func(string) (string, error) {
			if p == "" {
				return "", errors.New("no such file")
			}
			return p, nil
		}
	}
	for _, c := range []struct {
		name  string
		env   func(string) (string, bool)
		link  func(string) (string, error)
		local *time.Location
		want  string
	}{
		{"a name", env("Asia/Seoul", true), link(""), seoul, "Asia/Seoul"},
		{"a name, with a colon", env(":Asia/Seoul", true), link(""), seoul, "Asia/Seoul"},
		{"empty", env("", true), link(""), time.UTC, "UTC"},
		{"a file", env("/usr/share/zoneinfo/Europe/Berlin", true), link(""), berlin, "Europe/Berlin"},
		{"Linux's link", env("", false), link("/usr/share/zoneinfo/Asia/Seoul"), seoul, "Asia/Seoul"},
		{"macOS's link", env("", false), link("/var/db/timezone/zoneinfo/Asia/Seoul"), seoul, "Asia/Seoul"},
		{"a relative link", env("", false), link("../usr/share/zoneinfo/Asia/Seoul"), seoul, "Asia/Seoul"},
		// /etc/localtime → /etc/alternatives/localtime → the zone file.
		{"a chain of links", env("", false), link("/usr/share/zoneinfo/Asia/Seoul"), seoul, "Asia/Seoul"},
		{"TZ the link", env("/etc/localtime", true), link("/usr/share/zoneinfo/Europe/Berlin"), berlin, "Europe/Berlin"},
		{"Debian's", env("", false), link("/usr/share/zoneinfo/Etc/UTC"), time.UTC, "Etc/UTC"},
		{"no link", env("", false), link(""), seoul, ""},
		{"a link elsewhere", env("", false), link("/etc/zones/seoul"), seoul, ""},
		{"no such zone", env("Nowhere/Land", true), link(""), seoul, ""},
		{"not the zone Go reads", env("Europe/Berlin", true), link(""), seoul, ""},
	} {
		if got := localZoneName(c.env, c.link, c.local); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
