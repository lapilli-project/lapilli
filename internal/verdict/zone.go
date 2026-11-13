package verdict

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Zone names a time zone for a person, as a report's header does: its IANA
// name and its offset from UTC over from–to, as "Asia/Seoul (+09:00)", or
// "Europe/Berlin (+01:00 → +02:00 → +01:00)" for each offset the clocks
// took in it; UTC, by any of its names, as "UTC". The machine's own zone,
// which Go calls Local, is named as TZ or /etc/localtime says; one whose
// name can't be known is named by its abbreviations, when it has them,
// with their offsets: "CEST (+02:00) → CET (+01:00)", or "+09:00".
func Zone(loc *time.Location, from, to time.Time) string {
	if loc == nil {
		loc = time.UTC
	}
	name := loc.String()
	if loc == time.Local || name == "Local" {
		name = localName()
	}
	// A name with no letters, "+09", says no more than the offset.
	named := func(s string) bool { return strings.ContainsFunc(s, unicode.IsLetter) }
	if !named(name) {
		name = ""
	}
	type zone struct {
		abbr   string
		offset int
	}
	// Each zone the period passes through, in order: a period of years has
	// a few dozen at most.
	var zones []zone
	utc := true
	for t, n := from, 0; n < 200; n++ {
		abbr, offset := t.In(loc).Zone()
		if len(zones) == 0 || zones[len(zones)-1] != (zone{abbr, offset}) {
			zones = append(zones, zone{abbr, offset})
		}
		utc = utc && offset == 0
		_, end := t.In(loc).ZoneBounds()
		if end.IsZero() || !end.Before(to) || !end.After(t) {
			break
		}
		t = end
	}
	if utc && (utcNames[name] || name == "" && zones[0].abbr == "UTC") {
		return "UTC"
	}
	var said []string
	if name != "" {
		for _, z := range zones {
			if o := offsetText(z.offset); len(said) == 0 || said[len(said)-1] != o {
				said = append(said, o)
			}
		}
		return name + " (" + strings.Join(said, " → ") + ")"
	}
	for _, z := range zones {
		o := offsetText(z.offset)
		if named(z.abbr) {
			o = z.abbr + " (" + o + ")"
		}
		if len(said) == 0 || said[len(said)-1] != o {
			said = append(said, o)
		}
	}
	return strings.Join(said, " → ")
}

// utcNames are the names the zone database gives UTC.
var utcNames = map[string]bool{"UTC": true, "UCT": true, "Zulu": true, "Universal": true,
	"Etc/UTC": true, "Etc/UCT": true, "Etc/Zulu": true, "Etc/Universal": true}

// offsetText is an offset from UTC, in seconds, as +09:00 or -03:30.
func offsetText(s int) string {
	sign := '+'
	if s < 0 {
		sign, s = '-', -s
	}
	return fmt.Sprintf("%c%02d:%02d", sign, s/3600, s%3600/60)
}

// localName is the IANA name of the machine's own zone, as Go finds it:
// TZ when set ("" for UTC; a name, or the path of a zone file, with a
// leading colon or not), or else the zone file /etc/localtime is, through
// every link to it. It is "" when no name can be known, or the name found
// isn't a zone Go reads as it reads the machine's.
var localName = sync.OnceValue(func() string {
	return localZoneName(os.LookupEnv, filepath.EvalSymlinks, time.Local)
})

// localZoneName is localName, with what it reads given: resolve follows
// links to the file at their end.
func localZoneName(env func(string) (string, bool), resolve func(string) (string, error), local *time.Location) string {
	file := func(p string) string {
		if r, err := resolve(p); err == nil {
			if n := zoneinfoName(r); n != "" {
				return n
			}
		}
		return zoneinfoName(p)
	}
	name := ""
	if tz, ok := env("TZ"); ok {
		tz = strings.TrimPrefix(tz, ":")
		switch {
		case tz == "" || tz == "UTC":
			return "UTC"
		case strings.HasPrefix(tz, "/"):
			name = file(tz)
		default:
			name = tz
		}
	} else {
		name = file("/etc/localtime")
	}
	if name == "" {
		return ""
	}
	loc, err := time.LoadLocation(name)
	if err != nil || loc.String() != name {
		return ""
	}
	// The name must be the zone Go reads as Local: the same offsets, now
	// and half a year off.
	now := time.Now()
	for _, t := range []time.Time{now, now.AddDate(0, 6, 0)} {
		_, a := t.In(loc).Zone()
		_, b := t.In(local).Zone()
		if a != b {
			return ""
		}
	}
	return name
}

// zoneinfoName is the zone a zone file's path names: what follows its
// zoneinfo directory, /usr/share/zoneinfo/Asia/Seoul or macOS's
// /var/db/timezone/zoneinfo/Asia/Seoul.
func zoneinfoName(path string) string {
	path = filepath.ToSlash(path)
	i := strings.LastIndex(path, "zoneinfo/")
	if i < 0 {
		return ""
	}
	return path[i+len("zoneinfo/"):]
}
