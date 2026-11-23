package oncall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A feed's own zones (RFC 5545, 3.6.5): each VTIMEZONE's STANDARD and
// DAYLIGHT parts say when its offset changes, by DTSTART and TZOFFSETFROM
// (the onset on the clock before it), a rule or RDATEs, and to what
// (TZOFFSETTO). The offset at an instant is that of the part whose onset
// is the latest at or before it.

// Bounds on a feed's own zones: they are outside input too.
const (
	// maxVTimezones is how many VTIMEZONEs a feed may hold.
	maxVTimezones = 100
	// maxObservances is how many STANDARD and DAYLIGHT parts one may hold.
	maxObservances = 64
	// maxRdates is how many RDATEs one may hold, in all its parts.
	maxRdates = 1000
	// maxOnsets is how many onsets its DTSTARTs, RDATEs and rules with
	// COUNT may make in all.
	maxOnsets = 2000
	// maxCounted is how many periods a rule with COUNT is stepped through
	// from its DTSTART.
	maxCounted = 10_000
	// maxYearChanges is how many times its offset may change in a year: a
	// real zone's changes two to four times.
	maxYearChanges = 16
	// lookBack is how many periods a part's rule is looked back through for
	// its last onset before a year: a zone's yearly rule has one each year.
	lookBack = 8
	// firstYear and lastYear bound the years a zone is worked out for.
	firstYear, lastYear = 1800, 2300
)

// vzone is a VTIMEZONE. Its rules are worked out only when a time is read
// in it, a year at a time, for the years asked: a rule from 1601, as
// Outlook writes, is brought to the year asked without a step a year.
// Working them out takes steps from the feed's (maxFeedSteps).
type vzone struct {
	name  string
	obs   []*observance
	err   error // why it can't be read; kept once found
	steps *int
	// rdates is how many RDATEs its parts hold.
	rdates int
	// Worked out when first used.
	ready  bool
	onsets []change      // of DTSTART, RDATE and rules with COUNT, in order
	open   []*observance // parts with a rule without COUNT: by the year
	first  int           // the offset before its first onset
	years  map[int]*zoneYear
	buf    []time.Time
}

// change is an onset: from at on, the offset is off.
type change struct {
	at  time.Time
	off int
}

// zoneYear is a zone's offset over a year (in UTC): before, as it starts,
// and changes, its onsets in it, in order.
type zoneYear struct {
	before  int
	changes []change
}

// observance is a STANDARD or DAYLIGHT part.
type observance struct {
	start    time.Time // DTSTART, a wall clock time
	from, to int       // TZOFFSETFROM and TZOFFSETTO, in seconds east of UTC
	rule     *rule
	until    time.Time // the rule's UNTIL, an instant: zero for none
	rdates   []time.Time
	has      uint8 // DTSTART, TZOFFSETFROM and TZOFFSETTO, a bit each
	err      error
}

// readZones reads a feed's VTIMEZONEs, before its events: an event may
// come before the VTIMEZONE its TZID names. Each is read as far as its
// parts; their rules are worked out when a time is read in it. Steps the
// zones take are taken from steps.
func readZones(ctx context.Context, data []byte, steps *int) (map[string]*vzone, error) {
	zones := map[string]*vzone{}
	var cur *vzone
	var ob *observance
	depth, n, lines := 0, 0, 0
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if lines++; lines%1024 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Outside a VTIMEZONE only its start is looked at; a line too
		// long is refused when the events are read.
		if len(line) == 0 || len(line) > maxLine || cur == nil && !hasPrefixFold(line, "BEGIN:VTIMEZONE") {
			continue
		}
		name, params, value, ok := contentLine(string(line))
		if !ok {
			continue
		}
		part := strings.EqualFold(value, "STANDARD") || strings.EqualFold(value, "DAYLIGHT")
		switch {
		case cur == nil:
			if name != "BEGIN" || !strings.EqualFold(value, "VTIMEZONE") {
				continue
			}
			if n++; n > maxVTimezones {
				return nil, fmt.Errorf("the feed holds more than %d VTIMEZONEs", maxVTimezones)
			}
			cur, depth = &vzone{steps: steps}, 0
		case name == "BEGIN" && depth == 0 && ob == nil && part:
			ob = &observance{}
		case name == "END" && depth == 0 && ob != nil && part:
			cur.add(ob)
			ob = nil
		case name == "END" && depth == 0 && strings.EqualFold(value, "VTIMEZONE"):
			if ob != nil {
				cur.add(ob)
				ob = nil
			}
			if len(cur.obs) == 0 && cur.err == nil {
				cur.err = fmt.Errorf("it has no STANDARD or DAYLIGHT part")
			}
			// Two of one name: the first holds.
			if cur.name != "" && zones[cur.name] == nil {
				zones[cur.name] = cur
			}
			cur = nil
		case name == "BEGIN":
			depth++ // a part of a part, or of a kind not read: passed over
		case name == "END":
			depth = max(depth-1, 0)
		case depth > 0:
		case ob == nil:
			if name == "TZID" && len(value) <= maxTZID {
				cur.name = strings.Clone(unescape(value))
			}
		default:
			ob.read(cur, name, params, value)
		}
	}
	return zones, nil
}

func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && bytes.EqualFold(b[:len(prefix)], []byte(prefix))
}

// add keeps a part, maxObservances of them; one that can't be read makes
// the zone one that can't.
func (z *vzone) add(o *observance) {
	switch {
	case len(z.obs) >= maxObservances:
		o.err = fmt.Errorf("more than %d STANDARD and DAYLIGHT parts", maxObservances)
	case o.err == nil && o.has != 7:
		o.err = fmt.Errorf("a STANDARD or DAYLIGHT part without its DTSTART, TZOFFSETFROM and TZOFFSETTO")
	default:
		z.obs = append(z.obs, o)
	}
	if o.err != nil && z.err == nil {
		z.err = o.err
	}
}

// read reads a property of a part.
func (o *observance) read(z *vzone, name string, params map[string]string, value string) {
	var err error
	switch name {
	case "DTSTART":
		o.start, err = localTime(value)
		o.has |= 1
	case "TZOFFSETFROM":
		o.from, err = parseOffset(value)
		o.has |= 2
	case "TZOFFSETTO":
		o.to, err = parseOffset(value)
		o.has |= 4
	case "RRULE":
		if o.rule != nil {
			err = fmt.Errorf("a part with two RRULEs")
			break
		}
		o.rule, err = parseRule(value)
	case "RDATE":
		if strings.EqualFold(params["VALUE"], "PERIOD") {
			err = fmt.Errorf("an RDATE of periods")
			break
		}
		for v := range strings.SplitSeq(value, ",") {
			if z.rdates++; z.rdates > maxRdates {
				err = fmt.Errorf("more than %d RDATEs", maxRdates)
				break
			}
			var w time.Time
			if w, err = localTime(v); err != nil {
				break
			}
			o.rdates = append(o.rdates, w)
		}
	}
	if err != nil && o.err == nil {
		o.err = err
	}
}

// localTime reads a part's DTSTART or RDATE: a time on the clock, as RFC
// 5545 has them, or a day, from its midnight.
func localTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	layout := "20060102T150405"
	if len(v) == 8 {
		layout = "20060102"
	}
	t, err := time.Parse(layout, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q isn't a time on the clock, as a zone's onset is", clip(v))
	}
	return t, nil
}

// parseOffset reads a UTC-OFFSET (RFC 5545, 3.3.14): +HHMM or -HHMM, with
// its seconds or not; 18 hours at most, as no zone is more than 14 from
// UTC.
func parseOffset(v string) (int, error) {
	v = strings.TrimSpace(v)
	bad := fmt.Errorf("%q isn't an offset from UTC, such as -0800", clip(v))
	if len(v) != 5 && len(v) != 7 || v[0] != '+' && v[0] != '-' {
		return 0, bad
	}
	for i := 1; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, bad
		}
	}
	two := func(i int) int { return int(v[i]-'0')*10 + int(v[i+1]-'0') }
	h, m, s := two(1), two(3), 0
	if len(v) == 7 {
		s = two(5)
	}
	if h > 23 || m > 59 || s > 59 {
		return 0, bad
	}
	off := h*3600 + m*60 + s
	if off > maxOffset {
		return 0, fmt.Errorf("an offset from UTC of %s, more than any zone's", clip(v))
	}
	if v[0] == '-' {
		off = -off
	}
	return off, nil
}

func (z *vzone) charge(n int) error {
	if *z.steps -= n; *z.steps < 0 {
		return errTooManySteps
	}
	return nil
}

// prepare works out what doesn't change with the year asked: the onsets
// of DTSTART, RDATE and rules with COUNT, which are counted from their
// DTSTART; and the offset before the first of them.
func (z *vzone) prepare() error {
	if z.ready || z.err != nil {
		return z.err
	}
	z.ready = true
	z.years = map[int]*zoneYear{}
	var firstAt time.Time
	for _, o := range z.obs {
		at := o.start.Add(-secs(o.from))
		z.onsets = append(z.onsets, change{at, o.to})
		if firstAt.IsZero() || at.Before(firstAt) {
			firstAt, z.first = at, o.from
		}
		for _, w := range o.rdates {
			z.onsets = append(z.onsets, change{w.Add(-secs(o.from)), o.to})
		}
		if o.rule == nil {
			continue
		}
		if o.rule.until != "" {
			u, err := o.untilAt()
			if err != nil {
				z.err = err
				return err
			}
			o.until = u
		}
		if o.rule.count < 0 {
			z.open = append(z.open, o)
			continue
		}
		if err := z.counted(o); err != nil {
			z.err = err
			return err
		}
	}
	if len(z.onsets) > maxOnsets {
		z.err = fmt.Errorf("its onsets are more than %d", maxOnsets)
		return z.err
	}
	slices.SortStableFunc(z.onsets, func(a, b change) int { return a.at.Compare(b.at) })
	return nil
}

// untilAt is a part's UNTIL as an instant: in UTC, as RFC 5545 has it, or
// on the clock before the onset.
func (o *observance) untilAt() (time.Time, error) {
	v := strings.TrimSpace(o.rule.until)
	if strings.HasSuffix(v, "Z") {
		t, err := time.Parse("20060102T150405Z", v)
		if err != nil {
			return time.Time{}, fmt.Errorf("a rule's UNTIL %q isn't a time", clip(v))
		}
		return t, nil
	}
	w, err := localTime(v)
	if err != nil {
		return time.Time{}, err
	}
	if len(v) == 8 {
		w = w.AddDate(0, 0, 1).Add(-time.Second)
	}
	return w.Add(-secs(o.from)), nil
}

// counted adds the onsets of a rule with COUNT: stepped through from its
// DTSTART, as COUNT counts from there, maxCounted periods at most.
func (z *vzone) counted(o *observance) error {
	r, from := o.rule, secs(o.from)
	n := 0
	for p := 0; ; p++ {
		if p >= maxCounted {
			return fmt.Errorf("a rule with COUNT that takes more than %d steps", maxCounted)
		}
		if r.begin(o.start, p).Year() > lastYear {
			return nil
		}
		var work int
		z.buf, work = r.days(o.start, p, z.buf)
		if err := z.charge(work); err != nil {
			return err
		}
		for _, w := range z.buf {
			if w.Before(o.start) {
				continue
			}
			u := w.Add(-from)
			if n++; n > r.count || !o.until.IsZero() && u.After(o.until) {
				return nil
			}
			if z.onsets = append(z.onsets, change{u, o.to}); len(z.onsets) > maxOnsets {
				return fmt.Errorf("its onsets are more than %d", maxOnsets)
			}
		}
	}
}

// offset is the zone's offset at u: that of the part whose onset is the
// latest at or before it. What keeps it from being worked out names the
// zone, as what keeps it from being read does.
func (z *vzone) offset(u time.Time) (int, error) {
	off, err := z.offsetAt(u)
	if err != nil && !errors.Is(err, errTooManySteps) {
		err = fmt.Errorf("the feed's VTIMEZONE %q isn't read: %w", clip(z.name), err)
	}
	return off, err
}

func (z *vzone) offsetAt(u time.Time) (int, error) {
	if err := z.prepare(); err != nil {
		return 0, err
	}
	u = u.UTC()
	zy, err := z.year(u.Year())
	if err != nil {
		return 0, err
	}
	i, _ := slices.BinarySearchFunc(zy.changes, u, func(c change, t time.Time) int {
		if c.at.After(t) {
			return 1
		}
		return -1
	})
	if i == 0 {
		return zy.before, nil
	}
	return zy.changes[i-1].off, nil
}

// year works out a year's offsets, once: the onsets in it, and the offset
// as it starts, which is the last change of the year before when that is
// known, else of the onset latest before it.
func (z *vzone) year(y int) (*zoneYear, error) {
	if zy := z.years[y]; zy != nil {
		return zy, nil
	}
	if z.err != nil {
		return nil, z.err
	}
	if y < firstYear || y > lastYear {
		return nil, fmt.Errorf("a time in %d", y)
	}
	start := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC)
	zy := &zoneYear{}
	i, _ := slices.BinarySearchFunc(z.onsets, start, func(c change, t time.Time) int {
		if c.at.Before(t) {
			return -1
		}
		return 1
	})
	for j := i; j < len(z.onsets) && z.onsets[j].at.Before(end); j++ {
		zy.changes = append(zy.changes, z.onsets[j])
	}
	for _, o := range z.open {
		if len(zy.changes) > maxYearChanges {
			break
		}
		if err := z.openIn(o, start, end, &zy.changes); err != nil {
			z.err = err
			return nil, err
		}
	}
	if len(zy.changes) > maxYearChanges {
		z.err = fmt.Errorf("its offset changes more than %d times in %d", maxYearChanges, y)
		return nil, z.err
	}
	slices.SortStableFunc(zy.changes, func(a, b change) int { return a.at.Compare(b.at) })
	if prev := z.years[y-1]; prev != nil {
		zy.before = prev.before
		if n := len(prev.changes); n > 0 {
			zy.before = prev.changes[n-1].off
		}
	} else {
		zy.before = z.first
		var best time.Time
		if i > 0 {
			best, zy.before = z.onsets[i-1].at, z.onsets[i-1].off
		}
		for _, o := range z.open {
			u, ok, err := z.lastBefore(o, start)
			if err != nil {
				z.err = err
				return nil, err
			}
			if ok && (best.IsZero() || u.After(best)) {
				best, zy.before = u, o.to
			}
		}
	}
	z.years[y] = zy
	return zy, nil
}

// openIn adds the onsets in [start, end) of a part's rule without COUNT:
// the steps before them are skipped, not taken.
func (z *vzone) openIn(o *observance, start, end time.Time, out *[]change) error {
	r, from := o.rule, secs(o.from)
	for p := max(r.periodOf(o.start, start.Add(from))-1, 0); ; p++ {
		b := r.begin(o.start, p).Add(-from)
		if !b.Before(end) || !o.until.IsZero() && b.After(o.until) {
			return nil
		}
		var work int
		z.buf, work = r.days(o.start, p, z.buf)
		if err := z.charge(work); err != nil {
			return err
		}
		for _, w := range z.buf {
			if w.Before(o.start) {
				continue
			}
			u := w.Add(-from)
			if !o.until.IsZero() && u.After(o.until) || !u.Before(end) {
				return nil
			}
			if u.Before(start) {
				continue
			}
			if *out = append(*out, change{u, o.to}); len(*out) > maxYearChanges {
				return fmt.Errorf("its offset changes more than %d times in %d", maxYearChanges, start.Year())
			}
		}
	}
}

// lastBefore is the last onset before limit of a part's rule without
// COUNT, looked for lookBack periods back at most: a rule that holds no
// day so near is taken to have none, and the part's DTSTART, among the
// onsets, stands.
func (z *vzone) lastBefore(o *observance, limit time.Time) (time.Time, bool, error) {
	r, from := o.rule, secs(o.from)
	if !o.until.IsZero() && o.until.Before(limit) {
		limit = o.until.Add(time.Nanosecond)
	}
	p := r.periodOf(o.start, limit.Add(from))
	for k := 0; k < lookBack && p-k >= 0; k++ {
		var work int
		z.buf, work = r.days(o.start, p-k, z.buf)
		if err := z.charge(work); err != nil {
			return time.Time{}, false, err
		}
		for i := len(z.buf) - 1; i >= 0; i-- {
			w := z.buf[i]
			if w.Before(o.start) {
				break
			}
			if u := w.Add(-from); u.Before(limit) {
				return u, true, nil
			}
		}
	}
	return time.Time{}, false, nil
}
