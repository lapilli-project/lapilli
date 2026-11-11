// Package oncall reads on-call schedules from the iCalendar feeds on-call
// tools export (PagerDuty, Opsgenie, Jira Service Management, Grafana
// OnCall and IRM, a shared Google Calendar): who was on call when. A turn
// names a person; --on-call's file says each person's hours, so a firing
// can be weighed by the hours of whoever's turn it came in
// (docs/design/inputs.md).
package oncall

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/verdict"
)

// Config is what --on-call reads: the schedules, and the people in them.
type Config struct {
	// Every is how often lapilli serve reads each feed.
	Every     time.Duration
	Schedules []*Schedule
	People    []Person
	// byName finds a person by any of their names, in lower case.
	byName map[string]*Person
}

// Schedule is one schedule's feed.
type Schedule struct {
	Name string
	// URLFile is a file holding the feed's URL, its secret in it; URL a
	// URL with no secret in it, TokenFile one sent with it, as Auth says;
	// File a file holding the feed.
	URLFile, URL, TokenFile, Auth, User, File string
	// Who takes the person out of an event's summary: its first group.
	Who *regexp.Regexp
	// Loc is the zone of times written without one.
	Loc *time.Location
}

// Person is someone on call: their names, as the feeds write them, and
// their hours, each left out the team's.
type Person struct {
	Name  string
	Also  []string
	Hours verdict.PersonHours
}

// Bounds on what the file may hold.
const (
	maxSchedules = 20
	maxPeople    = 2000
	maxWho       = 200
	minEvery     = 5 * time.Minute
	defaultEvery = 30 * time.Minute
)

var scheduleName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// ValidName reports whether a schedule's name is one: lowercase letters,
// digits and dashes, 40 at most.
func ValidName(s string) bool { return scheduleName.MatchString(s) }

type fileConfig struct {
	Every     string `yaml:"every"`
	Schedules []struct {
		Name      string `yaml:"name"`
		URLFile   string `yaml:"url_file"`
		URL       string `yaml:"url"`
		TokenFile string `yaml:"token_file"`
		Auth      string `yaml:"auth"`
		User      string `yaml:"user"`
		File      string `yaml:"file"`
		Who       string `yaml:"who"`
		TZ        string `yaml:"tz"`
	} `yaml:"schedules"`
	People []struct {
		Name  string   `yaml:"name"`
		Also  []string `yaml:"also"`
		TZ    string   `yaml:"tz"`
		Work  string   `yaml:"work"`
		Sleep string   `yaml:"sleep"`
	} `yaml:"people"`
}

// Load reads --on-call's file, and checks everything in it.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("the file is over 1 MB")
	}
	var f fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("not what --on-call reads: %w", err)
	}
	c := &Config{Every: defaultEvery, byName: map[string]*Person{}}
	if f.Every != "" {
		d, err := time.ParseDuration(f.Every)
		if err != nil || d < minEvery || d > 24*time.Hour {
			return nil, fmt.Errorf("every %q: want how often to read each feed, 5m to 24h", f.Every)
		}
		c.Every = d
	}
	if len(f.Schedules) == 0 || len(f.Schedules) > maxSchedules {
		return nil, fmt.Errorf("want 1 to %d schedules", maxSchedules)
	}
	seen := map[string]bool{}
	for i, fs := range f.Schedules {
		s := &Schedule{Name: fs.Name, URLFile: fs.URLFile, URL: fs.URL, TokenFile: fs.TokenFile, Auth: fs.Auth, User: fs.User, File: fs.File, Loc: time.UTC}
		where := fmt.Sprintf("schedule %d", i+1)
		if !ValidName(s.Name) {
			return nil, fmt.Errorf("%s: a name is lowercase letters, digits and dashes, 40 at most, such as payments", where)
		}
		where = "schedule " + s.Name
		if seen[s.Name] {
			return nil, fmt.Errorf("%s: named twice", where)
		}
		seen[s.Name] = true
		n := 0
		for _, v := range []string{s.URLFile, s.URL, s.File} {
			if v != "" {
				n++
			}
		}
		if n != 1 {
			return nil, fmt.Errorf("%s: give one of url_file (a file holding the feed's URL), url, or file", where)
		}
		if s.URL != "" {
			if _, err := FeedURL(s.URL); err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
		}
		switch s.Auth {
		case "":
			if s.TokenFile != "" {
				s.Auth = "bearer"
			}
		case "bearer", "genie", "basic":
			if s.TokenFile == "" {
				return nil, fmt.Errorf("%s: auth %s needs token_file", where, s.Auth)
			}
		default:
			return nil, fmt.Errorf("%s: auth %q: want bearer, genie or basic", where, s.Auth)
		}
		if s.TokenFile != "" && s.File != "" {
			return nil, fmt.Errorf("%s: a token is for a URL, not a file", where)
		}
		if (s.Auth == "basic") != (s.User != "") {
			return nil, fmt.Errorf("%s: user goes with auth basic, and basic needs a user", where)
		}
		if strings.ContainsAny(s.User, ":\r\n") || len(s.User) > 200 {
			return nil, fmt.Errorf("%s: user %q isn't one", where, s.User)
		}
		if fs.Who != "" {
			re, err := regexp.Compile(fs.Who)
			if err != nil || len(fs.Who) > 500 {
				return nil, fmt.Errorf("%s: who %q isn't a regular expression: %v", where, fs.Who, err)
			}
			if re.NumSubexp() < 1 {
				return nil, fmt.Errorf("%s: who %q: want a group, (...), around the person", where, fs.Who)
			}
			s.Who = re
		}
		if fs.TZ != "" {
			loc, err := LoadZone(fs.TZ)
			if err != nil {
				return nil, fmt.Errorf("%s: tz %q isn't a time zone", where, fs.TZ)
			}
			s.Loc = loc
		}
		c.Schedules = append(c.Schedules, s)
	}
	if len(f.People) > maxPeople {
		return nil, fmt.Errorf("more than %d people", maxPeople)
	}
	for i, fp := range f.People {
		p := Person{Name: strings.TrimSpace(fp.Name), Also: fp.Also, Hours: verdict.PersonHours{TZ: fp.TZ, Work: fp.Work, Sleep: fp.Sleep}}
		where := fmt.Sprintf("person %d", i+1)
		if p.Name == "" {
			return nil, fmt.Errorf("%s: no name", where)
		}
		where = "person " + p.Name
		if err := verdict.CheckPersonHours(p.Hours); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		for _, n := range append([]string{p.Name}, p.Also...) {
			n = strings.TrimSpace(n)
			if n == "" || !plain(n) || utf8.RuneCountInString(n) > maxWho {
				return nil, fmt.Errorf("%s: a name %q that doesn't show, or is over %d characters", where, n, maxWho)
			}
			k := strings.ToLower(n)
			if other, ok := c.byName[k]; ok && other.Name != p.Name {
				return nil, fmt.Errorf("%s: %q is also %s's", where, n, other.Name)
			}
		}
		c.People = append(c.People, p)
		pp := &c.People[len(c.People)-1]
		for _, n := range append([]string{p.Name}, p.Also...) {
			c.byName[strings.ToLower(strings.TrimSpace(n))] = pp
		}
	}
	// The map points into People: rebuilt now it won't grow again.
	c.byName = map[string]*Person{}
	for i := range c.People {
		for _, n := range append([]string{c.People[i].Name}, c.People[i].Also...) {
			c.byName[strings.ToLower(strings.TrimSpace(n))] = &c.People[i]
		}
	}
	return c, nil
}

// Find is the person a name is, by any of their names, without case.
func (c *Config) Find(name string) *Person {
	return c.byName[strings.ToLower(strings.TrimSpace(name))]
}

// Schedule is the schedule of a name; nil for none.
func (c *Config) Schedule(name string) *Schedule {
	for _, s := range c.Schedules {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// HoursOf are each person's hours, by their name, as a turn names them.
func (c *Config) HoursOf() map[string]verdict.PersonHours {
	out := map[string]verdict.PersonHours{}
	for _, p := range c.People {
		out[p.Name] = p.Hours
	}
	return out
}

// zoneName is an IANA zone's name, as Go's zone database has them: no
// path, nothing that isn't a letter, digit, _, + or -.
var zoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+){0,2}$`)

// LoadZone loads a zone by its IANA name, and only by one.
func LoadZone(name string) (*time.Location, error) {
	if !zoneName.MatchString(name) || len(name) > 64 || name == "Local" {
		return nil, fmt.Errorf("%q isn't a time zone", name)
	}
	return time.LoadLocation(name)
}

// FeedURL checks a feed's URL: https, or webcal (read as https), with a
// host and no user in it.
func FeedURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("the feed's URL isn't one")
	}
	switch strings.ToLower(u.Scheme) {
	case "webcal", "webcals":
		u.Scheme = "https"
	case "https":
	default:
		return nil, errors.New("the feed's URL must be https (or webcal, read as https)")
	}
	if u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("the feed's URL needs a host, and no user name in it")
	}
	return u, nil
}

// plain reports whether text shows: no control characters, no
// direction marks.
func plain(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 || r >= 0x200b && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0xfeff {
			return false
		}
	}
	return true
}
