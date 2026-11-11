package oncall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// Bounds on a feed: what a schedule's editor writes in it is outside
// input.
const (
	// MaxFeed is the largest feed read.
	MaxFeed = 8 << 20
	// maxLine is the longest line, unfolded.
	maxLine = 64 << 10
	// maxEvents is how many events a feed may hold.
	maxEvents = 20_000
	// maxTurns is how many turns a feed's events, repeats expanded, may
	// make: as many as the ledger keeps.
	maxTurns = ledger.MaxTurns
	// maxSteps is how many steps one repeat may take to reach its turns.
	maxSteps = 200_000
	// maxNotes is how many things passed over are said.
	maxNotes = 20
	// maxExdates is how many dates one event may leave out.
	maxExdates = 1000
	// maxFeedSteps is how many steps every repeat of a feed may take in
	// all, to reach the span it is read over and through it.
	maxFeedSteps = 2_000_000
)

// The span repeats are expanded over, around now.
const (
	Back  = 400 * 24 * time.Hour
	Ahead = 60 * 24 * time.Hour
)

// event is a VEVENT, as far as a turn needs.
type event struct {
	uid, summary, status string
	start, end           stamp
	duration             string
	rrule                string
	exdates              []stamp
	recurrence           *stamp
	attendees            []string
	// names are the attendees' common names (CN).
	names []string
	// err is why the event can't be read: it is passed over, and said.
	err error
}

// stamp is a DTSTART, DTEND, EXDATE or RECURRENCE-ID: its value and the
// zone it was written in.
type stamp struct {
	t    time.Time
	date bool // VALUE=DATE: a day, not a time
	set  bool
}

// Feed is what a feed said: its turns, flattened, the earliest first, and
// what was passed over, said.
type Feed struct {
	Turns []verdict.RotaTurn
	Notes []string
	// Events is how many events it held.
	Events int
	// From is the earliest any of its events in the span read starts, those
	// cancelled too, not those passed over: the feed speaks for what it
	// holds from there.
	From time.Time
	// Held are the events passed over that name someone (Hold).
	Held []Held
}

// Held is an event passed over: what it would make isn't known, so the
// turns kept of the person it names from its start on are kept as they
// were (Hold), to its end for one that holds one turn; To is zero for a
// repeat.
type Held struct {
	Who      string
	From, To time.Time
}

// Parse reads a feed's turns: each event a turn of the person it names,
// over [now-Back, now+Ahead], repeats expanded within it; turns that
// overlap flattened, the one that started last holding from its start. It
// stops when ctx is done.
func Parse(ctx context.Context, data []byte, s *Schedule, c *Config, now time.Time) (*Feed, error) {
	if len(data) > MaxFeed {
		return nil, fmt.Errorf("the feed is over %d MB", MaxFeed>>20)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("BEGIN:VCALENDAR")) {
		return nil, errors.New("not an iCalendar feed: it doesn't start with BEGIN:VCALENDAR (a sign-in page, or an error?)")
	}
	// Unfold: a line break followed by a space or a tab goes on the line
	// before. Unfolded as bytes, as a fold can cut a character in two.
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\n "), nil)
	data = bytes.ReplaceAll(data, []byte("\n\t"), nil)
	if !utf8.Valid(data) {
		return nil, errors.New("the feed isn't UTF-8")
	}
	f := &Feed{}
	note := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if len(f.Notes) < maxNotes && !slices.Contains(f.Notes, msg) {
			f.Notes = append(f.Notes, msg)
		}
	}
	var events []*event
	var cur *event
	depth := 0 // within the event: a VALARM in it is passed over
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(line) > maxLine {
			return nil, fmt.Errorf("a line of the feed is over %d KB", maxLine>>10)
		}
		if len(line) == 0 {
			continue
		}
		name, params, value, ok := contentLine(string(line))
		if !ok {
			continue
		}
		switch {
		case name == "BEGIN" && strings.EqualFold(value, "VEVENT") && cur == nil:
			if len(events) >= maxEvents {
				return nil, fmt.Errorf("the feed holds more than %d events", maxEvents)
			}
			cur = &event{}
			depth = 0
			continue
		case name == "END" && strings.EqualFold(value, "VEVENT") && cur != nil && depth == 0:
			events = append(events, cur)
			cur = nil
			continue
		case cur == nil:
			continue
		case name == "BEGIN":
			depth++
			continue
		case name == "END":
			depth--
			continue
		case depth > 0:
			continue
		}
		var err error
		switch name {
		case "UID":
			cur.uid = value
		case "SUMMARY":
			cur.summary = unescape(value)
		case "STATUS":
			cur.status = strings.ToUpper(value)
		case "DTSTART":
			cur.start, err = parseStamp(params, value, s.Loc)
		case "DTEND":
			cur.end, err = parseStamp(params, value, s.Loc)
		case "DURATION":
			cur.duration = value
		case "RRULE":
			cur.rrule = value
		case "EXDATE":
			for v := range strings.SplitSeq(value, ",") {
				if len(cur.exdates) >= maxExdates {
					err = fmt.Errorf("more than %d dates left out of a repeat", maxExdates)
					break
				}
				x, e := parseStamp(params, v, s.Loc)
				if e != nil {
					err = e
					break
				}
				cur.exdates = append(cur.exdates, x)
			}
		case "RECURRENCE-ID":
			var x stamp
			x, err = parseStamp(params, value, s.Loc)
			cur.recurrence = &x
		case "ATTENDEE":
			v := value
			if strings.HasPrefix(strings.ToLower(v), "mailto:") {
				v = v[len("mailto:"):]
			}
			cur.attendees = append(cur.attendees, v)
			if cn := params["CN"]; cn != "" {
				cur.names = append(cur.names, cn)
			}
		}
		if err != nil && cur.err == nil {
			cur.err = err
		}
	}
	f.Events = len(events)
	// RECURRENCE-ID: an occurrence of a repeat replaced by an event of its
	// own, by the repeat's UID.
	replaced := map[string][]time.Time{}
	for _, e := range events {
		if e.recurrence != nil && e.recurrence.set && e.uid != "" {
			replaced[e.uid] = append(replaced[e.uid], e.recurrence.t)
		}
	}
	from, to := now.Add(-Back), now.Add(Ahead)
	var turns []verdict.RotaTurn
	steps := maxFeedSteps
	for k, e := range events {
		if k%256 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// speaks: the feed speaks for what it holds from the event's start,
		// a cancelled one's too (the feed says the turn is gone). An event
		// passed over doesn't: it says nothing of its turns.
		speaks := func() {
			if e.start.set && e.start.t.Before(to) && (f.From.IsZero() || e.start.t.Before(f.From)) && !e.start.t.Before(from) {
				f.From = e.start.t
			}
		}
		if e.status == "CANCELLED" {
			speaks()
			continue
		}
		if !e.start.set {
			if e.err != nil {
				note("%q: %v", clip(e.summary), e.err)
			}
			continue
		}
		who, said := c.who(s, e)
		// passOver says why an event is passed over; the turns kept of
		// whoever it names, from its start, are kept as they were (Held).
		passOver := func(why error, end time.Time) {
			note("%q: %v", clip(e.summary), why)
			if who != "" {
				f.Held = append(f.Held, Held{Who: who, From: e.start.t, To: end})
			}
		}
		// once is an event that holds one turn, not a repeat: it is held
		// only over that turn (from its start, to its end when known).
		once := e.rrule == "" || e.recurrence != nil
		if e.err != nil {
			passOver(e.err, onceEnd(once, e.start.t, nil))
			continue
		}
		length, err := e.length(s.Loc)
		if err != nil {
			passOver(err, onceEnd(once, e.start.t, nil))
			continue
		}
		if who == "" {
			note("%q: no one named", clip(e.summary))
			continue
		}
		if said != "" {
			note("%q: %s", clip(e.summary), said)
		}
		starts := []time.Time{e.start.t}
		if !once {
			starts, err = expand(ctx, e, from, to, maxTurns-len(turns), &steps)
			switch {
			case errors.Is(err, errTooManyTurns), errors.Is(err, errTooManySteps), errors.Is(err, ctx.Err()) && ctx.Err() != nil:
				return nil, err
			case err != nil:
				passOver(err, time.Time{})
				continue
			}
		}
		speaks()
		for _, st := range starts {
			if slices.ContainsFunc(replaced[e.uid], st.Equal) && e.recurrence == nil {
				continue
			}
			end := length(st)
			// Within the span read: an event from long ago, or far ahead,
			// is no turn of it, and can't push kept turns out.
			if !end.After(st) || !end.After(from) || !st.Before(to) {
				continue
			}
			if len(turns) >= maxTurns {
				return nil, errTooManyTurns
			}
			turns = append(turns, verdict.RotaTurn{From: st, To: end, Who: who})
		}
	}
	// Two turns that start together: the one later in the feed holds
	// (Flatten), said once for each two people.
	slices.SortStableFunc(turns, func(a, b verdict.RotaTurn) int { return a.From.Compare(b.From) })
	both := map[[2]string]bool{}
	for i := 1; i < len(turns); i++ {
		a, b := turns[i-1], turns[i]
		if a.From.Equal(b.From) && a.Who != b.Who && !both[[2]string{a.Who, b.Who}] {
			both[[2]string{a.Who, b.Who}] = true
			note("%q and %q start at the same time (%s first): the later in the feed, %q, holds", clip(a.Who), clip(b.Who), a.From.UTC().Format(time.RFC3339), clip(b.Who))
		}
	}
	f.Turns = Flatten(turns)
	return f, nil
}

// Errors that stop a feed's read: what it would make is past the bounds.
var (
	errTooManyTurns = fmt.Errorf("the feed's events make more than %d turns", maxTurns)
	errTooManySteps = fmt.Errorf("the feed's repeats take more than %d steps to expand", maxFeedSteps)
)

// who is the person an event names, and what is to be said of it: an
// attendee who is someone in the file (the first, said, when more are);
// else what Who takes out of the summary, as it is, or the summary when it
// is someone in the file; else, of an event with one attendee, that
// attendee (an email, which a person added to the file later is found
// by); else the summary. Who and the summary come before an attendee the
// file doesn't know: PagerDuty lists one attendee on every turn, and the
// file may name its people by the summary alone. A name that is someone's
// is theirs as the file writes it; one longer than the ledger keeps is
// cut to fit.
func (c *Config) who(s *Schedule, e *event) (string, string) {
	var known []string
	for _, a := range append(slices.Clone(e.attendees), e.names...) {
		if p := c.Find(a); p != nil && !slices.Contains(known, p.Name) {
			known = append(known, p.Name)
		}
	}
	switch {
	case len(known) > 1:
		return known[0], fmt.Sprintf("names %d people in the file, %q first: %q's turn it is", len(known), clip(known[0]), clip(known[0]))
	case len(known) == 1:
		return known[0], ""
	}
	name := strings.TrimSpace(e.summary)
	took := false
	if s.Who != nil {
		if m := s.Who.FindStringSubmatch(name); m != nil {
			name, took = strings.TrimSpace(m[1]), true
		}
	}
	if p := c.Find(name); p != nil {
		return p.Name, ""
	}
	if !took && len(e.attendees) == 1 {
		if a := strings.TrimSpace(e.attendees[0]); a != "" && shows(a) && len(a) <= maxWho {
			return a, ""
		}
	}
	if !shows(name) {
		return "", ""
	}
	return clipName(name), ""
}

// onceEnd is the end of an event's one turn, when it holds one and its
// length is known: a turn that starts at its start and no other is held
// otherwise. A repeat's is zero: held on.
func onceEnd(once bool, start time.Time, length func(time.Time) time.Time) time.Time {
	switch {
	case !once:
		return time.Time{}
	case length != nil && length(start).After(start):
		return length(start)
	}
	return start.Add(time.Nanosecond)
}

// length gives an occurrence's end from its start: DTEND less DTSTART, or
// DURATION; a day for a day with neither.
func (e *event) length(loc *time.Location) (func(time.Time) time.Time, error) {
	switch {
	case e.end.set && e.duration != "":
		return nil, errors.New("both DTEND and DURATION")
	case e.end.set:
		if e.start.date && e.end.date {
			days := int(e.end.t.Sub(e.start.t).Hours()/24 + 0.5)
			return func(t time.Time) time.Time { return t.AddDate(0, 0, days) }, nil
		}
		d := e.end.t.Sub(e.start.t)
		return func(t time.Time) time.Time { return t.Add(d) }, nil
	case e.duration != "":
		days, d, err := parseDuration(e.duration)
		if err != nil {
			return nil, err
		}
		return func(t time.Time) time.Time { return t.AddDate(0, 0, days).Add(d) }, nil
	case e.start.date:
		return func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }, nil
	}
	return func(t time.Time) time.Time { return t }, nil // no length: passed over
}

// contentLine splits a content line into its name, its parameters and its
// value; a colon in a quoted parameter is the parameter's.
func contentLine(line string) (name string, params map[string]string, value string, ok bool) {
	quoted := false
	colon := -1
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			quoted = !quoted
		case ':':
			if !quoted {
				colon = i
			}
		}
		if colon >= 0 {
			break
		}
	}
	if colon < 0 {
		return "", nil, "", false
	}
	head, value := line[:colon], line[colon+1:]
	parts := splitParams(head)
	name = strings.ToUpper(parts[0])
	params = map[string]string{}
	for _, p := range parts[1:] {
		k, v, _ := strings.Cut(p, "=")
		params[strings.ToUpper(k)] = strings.Trim(v, `"`)
	}
	return name, params, value, true
}

// splitParams splits NAME;A=B;C="D;E" at the semicolons outside quotes.
func splitParams(s string) []string {
	var out []string
	quoted, from := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case ';':
			if !quoted {
				out = append(out, s[from:i])
				from = i + 1
			}
		}
	}
	return append(out, s[from:])
}

// unescape reads a TEXT value: \n, \, \; and \\.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n', 'N':
				b.WriteByte(' ')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseStamp reads a date or a time: in UTC (Z), in the zone TZID names
// (an IANA name only: a feed's own VTIMEZONE isn't read), or floating, in
// loc.
func parseStamp(params map[string]string, v string, loc *time.Location) (stamp, error) {
	v = strings.TrimSpace(v)
	if params["VALUE"] == "DATE" || len(v) == 8 {
		t, err := time.ParseInLocation("20060102", v, loc)
		if err != nil {
			return stamp{}, fmt.Errorf("%q isn't a date", clip(v))
		}
		return stamp{t: t, date: true, set: true}, sane(t)
	}
	if strings.HasSuffix(v, "Z") {
		t, err := time.Parse("20060102T150405Z", v)
		if err != nil {
			return stamp{}, fmt.Errorf("%q isn't a time", clip(v))
		}
		return stamp{t: t, set: true}, sane(t)
	}
	if tz := params["TZID"]; tz != "" {
		l, err := LoadZone(strings.TrimPrefix(tz, "/"))
		if err != nil {
			return stamp{}, fmt.Errorf("a zone that isn't an IANA name, %q: its own VTIMEZONE isn't read", clip(tz))
		}
		loc = l
	}
	t, err := time.ParseInLocation("20060102T150405", v, loc)
	if err != nil {
		return stamp{}, fmt.Errorf("%q isn't a time", clip(v))
	}
	return stamp{t: t, set: true}, sane(t)
}

func sane(t time.Time) error {
	if t.Year() < 1970 || t.Year() > 2200 {
		return fmt.Errorf("a time in %d", t.Year())
	}
	return nil
}

// parseDuration reads an iCalendar DURATION, such as PT8H or P1W: its days
// (nominal, a day on the clock) and the rest (exact).
func parseDuration(s string) (int, time.Duration, error) {
	bad := fmt.Errorf("%q isn't a duration", clip(s))
	if !strings.HasPrefix(s, "P") && !strings.HasPrefix(s, "+P") {
		return 0, 0, bad
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "+"), "P")
	days := 0
	var d time.Duration
	inTime := false
	num := ""
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			num += string(r)
			if len(num) > 6 {
				return 0, 0, bad
			}
		case r == 'T':
			if inTime || num != "" {
				return 0, 0, bad
			}
			inTime = true
		default:
			n, err := strconv.Atoi(num)
			if err != nil {
				return 0, 0, bad
			}
			num = ""
			switch {
			case !inTime && r == 'W':
				days += 7 * n
			case !inTime && r == 'D':
				days += n
			case inTime && r == 'H':
				d += time.Duration(n) * time.Hour
			case inTime && r == 'M':
				d += time.Duration(n) * time.Minute
			case inTime && r == 'S':
				d += time.Duration(n) * time.Second
			default:
				return 0, 0, bad
			}
		}
	}
	if num != "" || days > 366 || d > 366*24*time.Hour {
		return 0, 0, bad
	}
	return days, d, nil
}

// expand gives the starts of a repeating event's occurrences that may meet
// [from, to], at most most of them: FREQ=DAILY or WEEKLY, with INTERVAL,
// COUNT or UNTIL, and BYDAY for weekly; EXDATE taken out. Other repeats
// are refused.
func expand(ctx context.Context, e *event, from, to time.Time, most int, steps *int) ([]time.Time, error) {
	freq, interval, count := "", 1, -1
	wkst := time.Monday
	for part := range strings.SplitSeq(e.rrule, ";") {
		if k, v, _ := strings.Cut(part, "="); strings.EqualFold(k, "FREQ") {
			freq = strings.ToUpper(v)
		}
	}
	if freq != "DAILY" && freq != "WEEKLY" {
		return nil, fmt.Errorf("a repeat FREQ=%s, which this doesn't expand: only DAILY and WEEKLY", clip(freq))
	}
	var until time.Time
	var byday []time.Weekday
	for part := range strings.SplitSeq(e.rrule, ";") {
		k, v, _ := strings.Cut(part, "=")
		switch strings.ToUpper(k) {
		case "FREQ":
		case "INTERVAL":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1000 {
				return nil, fmt.Errorf("a repeat's INTERVAL %q", clip(v))
			}
			interval = n
		case "COUNT":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxSteps {
				return nil, fmt.Errorf("a repeat's COUNT %q", clip(v))
			}
			count = n
		case "UNTIL":
			st, err := parseStamp(nil, v, e.start.t.Location())
			if err != nil {
				return nil, err
			}
			until = st.t
			if st.date {
				until = until.AddDate(0, 0, 1).Add(-time.Second)
			}
		case "BYDAY":
			for d := range strings.SplitSeq(v, ",") {
				i := slices.Index([]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}, strings.ToUpper(d))
				if i < 0 {
					return nil, fmt.Errorf("a repeat by day %q, which this doesn't expand", clip(d))
				}
				byday = append(byday, time.Weekday(i))
			}
		case "WKST":
			i := slices.Index([]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}, strings.ToUpper(v))
			if i < 0 {
				return nil, fmt.Errorf("a repeat's week starting %q", clip(v))
			}
			wkst = time.Weekday(i)
		default:
			return nil, fmt.Errorf("a repeat by %s, which this doesn't expand", clip(k))
		}
	}
	if freq == "DAILY" && len(byday) > 0 {
		return nil, errors.New("a daily repeat by day, which this doesn't expand")
	}
	// Dates left out, looked up rather than searched each step.
	exTimes, exDates := map[int64]bool{}, map[[3]int]bool{}
	for _, x := range e.exdates {
		if x.date {
			y, m, d := x.t.Date()
			exDates[[3]int{y, int(m), d}] = true
		} else {
			exTimes[x.t.Unix()] = true
		}
	}
	excluded := func(t time.Time) bool {
		y, m, d := t.Date()
		return exTimes[t.Unix()] || exDates[[3]int{y, int(m), d}]
	}
	start := e.start.t
	var out []time.Time
	n := 0 // occurrences so far, for COUNT
	full := false
	take := func(t time.Time) bool {
		if !until.IsZero() && t.After(until) || t.After(to) {
			return false
		}
		n++
		if count >= 0 && n > count {
			return false
		}
		if !t.Before(from.Add(-Ahead)) && !excluded(t) {
			if len(out) >= most {
				full = true
				return false
			}
			out = append(out, t)
		}
		return true
	}
	// Without COUNT, the steps before the span read are skipped, not taken:
	// a repeat from 1970 starts near now. With COUNT they are counted, and
	// COUNT bounds them.
	first := 0
	if count < 0 {
		days := 7 * interval
		if freq == "DAILY" {
			days = interval
		}
		if gap := int(from.Add(-Ahead).Sub(start).Hours() / 24 / float64(days)); gap > 1 {
			first = gap - 1
		}
	}
	for step := first; step < first+maxSteps; step++ {
		*steps--
		if *steps < 0 {
			return nil, errTooManySteps
		}
		if step%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		more := true
		switch {
		case freq == "DAILY":
			more = take(start.AddDate(0, 0, step*interval))
		case len(byday) == 0:
			more = take(start.AddDate(0, 0, 7*step*interval))
		default:
			// WEEKLY by day: the week of the start, as WKST begins weeks,
			// then every interval weeks, each day it names at the start's
			// time of day.
			week := start.AddDate(0, 0, 7*step*interval)
			begin := week.AddDate(0, 0, -((int(week.Weekday()) - int(wkst) + 7) % 7))
			for i := range 7 {
				d := begin.AddDate(0, 0, i)
				if !slices.Contains(byday, d.Weekday()) || d.Before(start) {
					continue
				}
				if more = take(d); !more {
					break
				}
			}
		}
		if full {
			return nil, errTooManyTurns
		}
		if !more {
			return out, nil
		}
	}
	return nil, errors.New("a repeat that takes too many steps to reach now")
}

func sameDate(a, b time.Time) bool {
	y1, m1, d1 := a.Date()
	y2, m2, d2 := b.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// Flatten makes turns that overlap into turns that don't: at each time, the
// turn that started last holds; when it ends, the one under it resumes.
// Turns of one person that meet are one.
func Flatten(turns []verdict.RotaTurn) []verdict.RotaTurn {
	if len(turns) == 0 {
		return nil
	}
	edges := make([]time.Time, 0, 2*len(turns))
	for _, t := range turns {
		edges = append(edges, t.From, t.To)
	}
	slices.SortFunc(edges, func(a, b time.Time) int { return a.Compare(b) })
	edges = slices.CompactFunc(edges, func(a, b time.Time) bool { return a.Equal(b) })
	byStart := slices.Clone(turns)
	slices.SortStableFunc(byStart, func(a, b verdict.RotaTurn) int { return a.From.Compare(b.From) })
	// active holds the turns begun and not yet ended, the latest begun
	// last: a stack the latest of which is on top, once those ended are
	// let go from the top.
	var active []verdict.RotaTurn
	var out []verdict.RotaTurn
	next := 0
	for i := 0; i+1 < len(edges); i++ {
		a, b := edges[i], edges[i+1]
		for next < len(byStart) && !byStart[next].From.After(a) {
			active = append(active, byStart[next])
			next++
		}
		for len(active) > 0 && !active[len(active)-1].To.After(a) {
			active = active[:len(active)-1]
		}
		if len(active) == 0 {
			continue
		}
		who := active[len(active)-1].Who
		if n := len(out); n > 0 && out[n-1].Who == who && out[n-1].To.Equal(a) {
			out[n-1].To = b
			continue
		}
		out = append(out, verdict.RotaTurn{From: a, To: b, Who: who})
	}
	return out
}

// Hold is a feed's turns with the turns kept of the events it passed over
// (Held): each kept turn of the person such an event names, from the
// event's start on (over its one turn, for one that holds one), where the
// feed's turns aren't, from from on. turns and kept are each in order and
// don't overlap; so is what Hold gives.
func Hold(kept, turns []verdict.RotaTurn, held []Held, from time.Time) []verdict.RotaTurn {
	if len(held) == 0 || len(turns) == 0 {
		return turns
	}
	// By who: from when a repeat of theirs is held on, and the spans of
	// their single turns held, joined where they meet.
	type holds struct {
		on    time.Time
		spans [][2]time.Time
	}
	by := map[string]*holds{}
	for _, h := range held {
		x := by[h.Who]
		if x == nil {
			x = &holds{}
			by[h.Who] = x
		}
		if h.To.IsZero() {
			if x.on.IsZero() || h.From.Before(x.on) {
				x.on = h.From
			}
			continue
		}
		x.spans = append(x.spans, [2]time.Time{h.From, h.To})
	}
	for _, x := range by {
		slices.SortFunc(x.spans, func(a, b [2]time.Time) int { return a[0].Compare(b[0]) })
		var joined [][2]time.Time
		for _, sp := range x.spans {
			if n := len(joined); n > 0 && !sp[0].After(joined[n-1][1]) {
				if sp[1].After(joined[n-1][1]) {
					joined[n-1][1] = sp[1]
				}
				continue
			}
			joined = append(joined, sp)
		}
		x.spans = joined
	}
	isHeld := func(k verdict.RotaTurn) bool {
		x := by[k.Who]
		switch {
		case x == nil:
			return false
		case !x.on.IsZero() && !k.From.Before(x.on):
			return true
		}
		i, _ := slices.BinarySearchFunc(x.spans, k.From, func(sp [2]time.Time, t time.Time) int {
			if sp[1].After(t) {
				return 1
			}
			return -1
		})
		return i < len(x.spans) && !k.From.Before(x.spans[i][0])
	}
	out := slices.Clone(turns)
	for _, k := range kept {
		if !k.To.After(from) || !isHeld(k) {
			continue
		}
		if k.From.Before(from) {
			k.From = from
		}
		// The first of the feed's turns that ends after k starts: k is
		// kept only where the feed has nothing.
		i, _ := slices.BinarySearchFunc(turns, k.From, func(x verdict.RotaTurn, t time.Time) int {
			if x.To.After(t) {
				return 1
			}
			return -1
		})
		if i < len(turns) && turns[i].From.Before(k.To) {
			continue
		}
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b verdict.RotaTurn) int { return a.From.Compare(b.From) })
	return out
}

// shows reports whether a name shows as the ledger keeps names (its
// Hidden: no control, nothing that doesn't show but a joiner between two
// letters of one script or two emoji). A turn whose name doesn't is passed
// over, not the whole feed.
func shows(s string) bool {
	return utf8.ValidString(s) && !ledger.Hidden(s)
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= 80 {
		return s
	}
	return string([]rune(s)[:80]) + "…"
}

// clipName is a name that shows cut to the bytes the ledger keeps (maxWho),
// with … where it was cut: at a character's end, and before a joiner or a
// variation selector left with nothing after it, so it still shows.
func clipName(s string) string {
	if len(s) <= maxWho {
		return s
	}
	cut := maxWho - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	s = s[:cut]
	for {
		r, n := utf8.DecodeLastRuneInString(s)
		if n == 0 || r != 0x200C && r != 0x200D && (r < 0xFE00 || r > 0xFE0F) {
			break
		}
		s = s[:len(s)-n]
	}
	return s + "…"
}
