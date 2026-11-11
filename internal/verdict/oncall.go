package verdict

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Shifts read from an on-call schedule: a turn names a person, not hours,
// and the person has hours of their own (docs/design/inputs.md). Shifts
// written on-call=NAME take turns by the schedule NAME, whose turns are
// read by package oncall and set here as they change.

// RotaTurn is someone's turn on call: From up to, not including, To.
type RotaTurn struct {
	From, To time.Time
	Who      string
}

// PersonHours are a person's own hours, as --tz, --work-hours and
// --sleep-hours take them, each "" for the team's.
type PersonHours struct {
	TZ, Work, Sleep string
}

// CheckPersonHours checks a person's hours.
func CheckPersonHours(p PersonHours) error {
	h := DefaultHours(time.UTC)
	_, err := p.apply(h)
	return err
}

func (p PersonHours) apply(base *Hours) (*Hours, error) {
	h := *base
	h.Shifts = nil
	if p.TZ != "" {
		loc, err := time.LoadLocation(p.TZ)
		if err != nil || p.TZ == "Local" || len(p.TZ) > 64 || strings.Contains(p.TZ, "..") {
			return nil, fmt.Errorf("tz %q isn't a time zone", p.TZ)
		}
		h.Loc = loc
	}
	if p.Work != "" {
		if err := h.ParseWork(p.Work); err != nil {
			return nil, fmt.Errorf("work: %w", err)
		}
	}
	if p.Sleep != "" {
		if err := h.ParseSleep(p.Sleep); err != nil {
			return nil, fmt.Errorf("sleep: %w", err)
		}
	}
	return &h, nil
}

// Rota is a schedule's turns, flattened, the earliest first, and the
// hours of the people they name.
type Rota struct {
	turns  []RotaTurn
	people map[string]PersonHours
	key    string
	// hours are each person's, over each base hours, worked out once.
	mu    sync.Mutex
	hours map[*Hours]map[string]*Hours
}

// NewRota is a schedule's rota: turns that don't overlap, as package
// oncall flattens them, and each person's hours by the name turns give.
func NewRota(turns []RotaTurn, people map[string]PersonHours) *Rota {
	r := &Rota{turns: slices.Clone(turns), people: people, hours: map[*Hours]map[string]*Hours{}}
	sort.SliceStable(r.turns, func(i, j int) bool { return r.turns[i].From.Before(r.turns[j].From) })
	h := sha256.New()
	for _, t := range r.turns {
		fmt.Fprintf(h, "%d %d %s\n", t.From.Unix(), t.To.Unix(), t.Who)
	}
	names := make([]string, 0, len(people))
	for n := range people {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := people[n]
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\n", n, p.TZ, p.Work, p.Sleep)
	}
	r.key = hex.EncodeToString(h.Sum(nil)[:8])
	return r
}

// Turns are the rota's turns.
func (r *Rota) Turns() []RotaTurn { return r.turns }

// Known reports whether a turn's person has hours of their own.
func (r *Rota) Known(who string) bool {
	_, ok := r.people[who]
	return ok
}

// at is the hours at t, and when they next may change: the hours of the
// person whose turn it is, or base's between turns or for someone with
// none.
func (r *Rota) at(base *Hours, t time.Time) (*Hours, time.Time) {
	i := sort.Search(len(r.turns), func(i int) bool { return r.turns[i].From.After(t) }) // the first after t
	next := time.Time{}
	if i < len(r.turns) {
		next = r.turns[i].From
	}
	if i > 0 && r.turns[i-1].To.After(t) {
		turn := r.turns[i-1]
		return r.personHours(base, turn.Who), turn.To
	}
	if next.IsZero() {
		next = t.Add(366 * 24 * time.Hour)
	}
	return base, next
}

func (r *Rota) personHours(base *Hours, who string) *Hours {
	p, ok := r.people[who]
	if !ok {
		return base
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	by := r.hours[base]
	if by == nil {
		by = map[string]*Hours{}
		r.hours[base] = by
	}
	if h := by[who]; h != nil {
		return h
	}
	h, err := p.apply(base)
	if err != nil {
		h = base // checked when the file was read
	}
	by[who] = h
	return h
}

// OnCall is a schedule named in shifts as on-call=NAME, and its rota as
// last read; none read yet, its turns are the hours shifts were given
// with.
type OnCall struct {
	Name string
	rota atomic.Pointer[Rota]
}

// Set sets the schedule's rota.
func (o *OnCall) Set(r *Rota) { o.rota.Store(r) }

// Rota is the schedule's rota; nil before one is read.
func (o *OnCall) Rota() *Rota { return o.rota.Load() }

var onCalls sync.Map // name → *OnCall

// OnCallNamed is the schedule of a name, the same for every shifts that
// name it.
func OnCallNamed(name string) *OnCall {
	o, _ := onCalls.LoadOrStore(name, &OnCall{Name: name})
	return o.(*OnCall)
}

var onCallName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// OnCallShifts reads shifts written on-call=NAME: the schedule's name, and
// whether they are.
func OnCallShifts(s string) (string, bool) {
	name, ok := strings.CutPrefix(strings.TrimSpace(s), "on-call=")
	return name, ok
}

// onCallShifts are shifts by a schedule's turns, over base's hours.
func onCallShifts(text, name string, base *Hours) (*Shifts, error) {
	if !onCallName.MatchString(name) {
		return nil, &ShiftsError{text, "a schedule's name is lowercase letters, digits and dashes, 40 at most, as --on-call names it"}
	}
	b := *base
	b.Shifts = nil
	return &Shifts{OnCall: OnCallNamed(name), base: &b, text: "on-call=" + name}, nil
}

// Key tells shifts apart where String doesn't: a schedule's turns and
// people as last read.
func (s *Shifts) Key() string {
	if s.OnCall == nil {
		return s.text
	}
	if r := s.OnCall.Rota(); r != nil {
		return s.text + "@" + r.key
	}
	return s.text + "@none"
}
