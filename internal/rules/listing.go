package rules

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Listing is the groups a rules API lists (Prometheus's /api/v1/rules, or a
// Loki ruler's), to place a group read from a file where the server runs
// the group of that name: a file on Lapilli's disk doesn't say where its
// group is placed, and Prometheus places it by a hash of its name and the
// path it loaded the file from, which only the API says
// (docs/design/states.md, section 5).
type Listing struct {
	groups []listed
	// layout is how the server hashes a group's name and file, as the
	// groups' own last evaluations say, or -1 when none of the layouts
	// known fits them.
	layout int
	tag    string
}

type listed struct {
	name, file string
	interval   time.Duration
	last       time.Time
	vmalert    bool
	// phase is where the group's last evaluation says it is placed, to the
	// second; ran is whether it has run.
	phase time.Duration
	ran   bool
}

// ReadListing reads the groups a Prometheus-compatible rules API answers
// with.
func ReadListing(b []byte) (*Listing, error) { return readListing(b, true) }

// ReadLokiListing reads the groups a Loki ruler's rules API answers with:
// the file it gives is the namespace, not the path the ruler hashes, so no
// hash is checked against them, and a group is placed by where it last ran.
func ReadLokiListing(b []byte) (*Listing, error) { return readListing(b, false) }

func readListing(b []byte, hashed bool) (*Listing, error) {
	var r struct {
		Status string `json:"status"`
		Data   struct {
			Groups []struct {
				Name           string    `json:"name"`
				File           string    `json:"file"`
				Interval       float64   `json:"interval"`
				LastEvaluation time.Time `json:"lastEvaluation"`
				Concurrency    *float64  `json:"concurrency"`
				EvalOffset     float64   `json:"eval_offset"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("not a rules API answer: %w", err)
	}
	if r.Status != "success" {
		return nil, errors.New("the rules API didn't answer with success")
	}
	l := &Listing{layout: -1}
	for _, g := range r.Data.Groups {
		var iv time.Duration
		if g.Interval >= 1 && g.Interval < never.Seconds() {
			iv = time.Duration(g.Interval * float64(time.Second))
		}
		x := listed{name: g.Name, file: g.File, interval: iv, last: g.LastEvaluation, vmalert: g.Concurrency != nil}
		x.phase = phaseOf(iv, g.LastEvaluation, x.vmalert, g.EvalOffset)
		x.ran = iv > 0 && (x.vmalert || g.LastEvaluation.Year() > 1970)
		l.groups = append(l.groups, x)
	}
	if hashed {
		l.layout = l.hashLayout()
	}
	// What a group is placed by, not what it last did: the answer changes
	// at every evaluation, and what is kept under the tag with it.
	h := sha256.New()
	fmt.Fprintf(h, "%d", l.layout)
	for _, g := range l.groups {
		fmt.Fprintf(h, "\x00%q\x00%q\x00%d\x00%d\x00%t\x00%t", g.name, g.file, g.interval, g.phase, g.ran, g.vmalert)
	}
	l.tag = hex.EncodeToString(h.Sum(nil)[:8])
	return l, nil
}

// Tag tells one listing from another, by where its groups are placed, "" for none: what rules placed by it
// are kept under.
func (l *Listing) Tag() string {
	if l == nil {
		return ""
	}
	return l.tag
}

// The hashes of a label set of a group's file and name (name and file, in
// order of the labels' names), as a Prometheus build gives them.
const (
	hashStrings = iota // Prometheus 3's default build (stringlabels): each as a length and the bytes
	hashUvarint        // Prometheus 2's stringlabels build: the same, the length a uvarint
	hashSlice          // the slice of labels': each name and value, then 0xff
	hashLayouts
)

// hash is the hash of a group of the name and file as a Prometheus built
// with layout takes it.
func hash(layout int, name, file string) uint64 {
	var b []byte
	for _, s := range []string{"file", file, "name", name} {
		switch layout {
		case hashStrings:
			// One byte below 255, else 0xFF and three bytes, little endian
			// (model/labels, encodeSize): the same as a uvarint below 128.
			if n := len(s); n < 255 {
				b = append(b, byte(n))
			} else {
				b = append(b, 0xff, byte(n), byte(n>>8), byte(n>>16))
			}
			b = append(b, s...)
		case hashUvarint:
			b = binary.AppendUvarint(b, uint64(len(s)))
			b = append(b, s...)
		default:
			b = append(b, s...)
			b = append(b, 0xff)
		}
	}
	return xxhash64(b)
}

// offset is where a group of an interval is placed within it by layout's
// hash, to the second below: Prometheus's offset is the hash modulo the
// interval, in nanoseconds.
func offset(layout int, name, file string, interval time.Duration) time.Duration {
	ns := time.Duration(hash(layout, name, file) % uint64(interval))
	return ns / time.Second * time.Second
}

// hashLayout is the layout whose hashes the groups' last evaluations
// agree with, for every group that has run, and at least three of them: a
// server that places its groups by a hash of its own, or at random, agrees
// with none, and one or two groups fit a layout by chance (about two in
// the interval, for each).
func (l *Listing) hashLayout() int {
	ran := 0
	var fits [hashLayouts]int
	for _, g := range l.groups {
		if !g.ran || g.vmalert {
			continue
		}
		ran++
		for k := range fits {
			if g.agrees(offset(k, g.name, g.file, g.interval)) {
				fits[k]++
			}
		}
	}
	for k, n := range fits {
		if n >= 3 && n == ran {
			return k
		}
	}
	return -1
}

// agrees reports whether where a hash places the group is where it last
// ran, to the second: the last evaluation is when it started, a moment after
// the time it was placed at, which may be in the next second.
func (g listed) agrees(placed time.Duration) bool {
	return (g.phase-placed+g.interval)%g.interval <= time.Second
}

// Place is where the group of the name, as a file gives it with the
// interval, is placed within it: where the server's hash of the group's
// name and the file it loaded puts it, or, where Lapilli can't check that
// hash against the groups' last evaluations, where the group of that name
// last ran, if that is at the same interval. why says what stopped it
// placing the group, when it doesn't: no group of that name, more than one,
// one that hasn't run, or placed in a way Lapilli can't tell at another
// interval.
func (l *Listing) Place(name string, interval time.Duration) (phase, every time.Duration, why string) {
	if l == nil {
		return 0, 0, "the rules API wasn't read"
	}
	var of []listed
	for _, g := range l.groups {
		if g.name == name {
			of = append(of, g)
		}
	}
	switch {
	case len(of) == 0:
		return 0, 0, "the rules API lists no group of that name"
	case len(of) > 1:
		return 0, 0, fmt.Sprintf("the rules API lists %d groups of that name", len(of))
	}
	g := of[0]
	if interval <= 0 {
		// The file gives none: the group runs at the server's own, which
		// the API says.
		interval = g.interval
	}
	if interval < time.Second {
		return 0, 0, "its interval isn't known"
	}
	if l.layout >= 0 && !g.vmalert {
		d := offset(l.layout, g.name, g.file, interval)
		if g.ran && g.interval == interval && !g.agrees(d) {
			return 0, 0, "the hash of its name and file doesn't place it where the rules API says it last ran"
		}
		return d, interval, ""
	}
	switch {
	case !g.ran:
		return 0, 0, "the group hasn't run yet"
	case g.interval != interval:
		return 0, 0, "the server places a group in a way Lapilli can't work out for another interval than the one it runs it at"
	}
	return g.phase, interval, ""
}

// Unplaced collects the groups that Replaying.Apply couldn't place from a
// listing, and why, and how many it placed: said by the run, once.
type Unplaced struct {
	mu     sync.Mutex
	placed map[string]bool
	why    map[string]map[string]bool // reason to groups
}

// Said is what was collected, as lines: how many groups were placed, and
// which weren't, by reason. nil when none were looked at.
func (u *Unplaced) Said(by string) []string {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.placed)+len(u.why) == 0 {
		return nil
	}
	var out []string
	if n := len(u.placed); n > 0 {
		groups := "groups"
		if n == 1 {
			groups = "group"
		}
		out = append(out, fmt.Sprintf("%d %s placed as %s places them", n, groups, by))
	}
	for _, why := range slices.Sorted(maps.Keys(u.why)) {
		names := slices.Sorted(maps.Keys(u.why[why]))
		more := ""
		if len(names) > 5 {
			more = fmt.Sprintf(" and %d more", len(names)-5)
			names = names[:5]
		}
		for i, n := range names {
			names[i] = fmt.Sprintf("%q", n)
		}
		out = append(out, fmt.Sprintf("not placed, %s: %s%s", why, strings.Join(names, ", "), more))
	}
	return out
}

func (u *Unplaced) add(group, why string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if why == "" {
		if u.placed == nil {
			u.placed = map[string]bool{}
		}
		u.placed[group] = true
		return
	}
	if u.why == nil {
		u.why = map[string]map[string]bool{}
	}
	if u.why[why] == nil {
		u.why[why] = map[string]bool{}
	}
	u.why[why][group] = true
}
