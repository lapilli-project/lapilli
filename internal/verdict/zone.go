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
// "Europe/Berlin (+02:00 → +01:00)" when the clocks changed in it; UTC as
// "UTC". The machine's own zone, which Go calls Local, is named as TZ or
// /etc/localtime says; one whose name can't be known is named by its
// abbreviation, when it has one, and its offset: "KST (+09:00)", or
// "+09:00".
func Zone(loc *time.Location, from, to time.Time) string {
	if loc == nil {
		loc = time.UTC
	}
	name := loc.String()
	if loc == time.Local || name == "Local" {
		name = localName()
	}
	abbr, at := from.In(loc).Zone()
	_, until := to.In(loc).Zone()
	offset := offsetText(at)
	if until != at {
		offset += " → " + offsetText(until)
	}
	// A name with no letters, "+09", says no more than the offset.
	named := func(s string) bool { return strings.ContainsFunc(s, unicode.IsLetter) }
	if !named(name) {
		name = ""
	}
	switch {
	case at == 0 && until == 0 && (name == "UTC" || name == "" && abbr == "UTC"):
		return "UTC"
	case name != "":
		return name + " (" + offset + ")"
	case named(abbr):
		return abbr + " (" + offset + ")"
	}
	return offset
}

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
// leading colon or not), or else the zone file /etc/localtime links to.
// It is "" when no name can be known, or the name found isn't a zone Go
// reads as it reads the machine's.
var localName = sync.OnceValue(func() string {
	return localZoneName(os.LookupEnv, os.Readlink, time.Local)
})

func localZoneName(env func(string) (string, bool), readlink func(string) (string, error), local *time.Location) string {
	name := ""
	if tz, ok := env("TZ"); ok {
		tz = strings.TrimPrefix(tz, ":")
		switch {
		case tz == "" || tz == "UTC":
			return "UTC"
		case strings.HasPrefix(tz, "/"):
			name = zoneinfoName(tz)
		default:
			name = tz
		}
	} else if p, err := readlink("/etc/localtime"); err == nil {
		name = zoneinfoName(p)
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
