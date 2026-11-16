package verdict

import (
	"regexp"
	"slices"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/incident"
)

// Named impact signals (docs/design/signals.md): several expressions that
// each return something while users are hurt in one way, each with its own
// "for", the labels its windows are matched on, and from when it keeps the
// record; and rule entries, which say which rules are judged against which
// signals' windows, and on which labels. With none, an impact signal has no
// name, and judges as it always has (Options.ImpactFrom).

// The bounds of what judges by signals: eight signals, each matched on 16
// labels at most; 64 rule entries, each with 16 labels and 8 signals at
// most; and, of what a signal has returned (Seen), 1,000 values a label and
// 256 KB a signal, past which it is taken to have returned every value of
// that label.
const (
	MaxSignals        = 8
	MaxSignalMatch    = 16
	MaxRuleEntries    = 64
	MaxEntryLabels    = 16
	MaxEntrySignals   = 8
	MaxSeenValues     = 1000
	MaxSeenBytes      = 256 << 10
	MaxSignalNameSize = 32
)

// SignalName is what a signal may be named: a lowercase letter, then
// lowercase letters, digits, - and _, 32 bytes at most; safe in an
// incident's id, a URL and a label's value.
var SignalName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Signal is a named impact signal, as it judges and as a report says it
// (Settings.ImpactSignals).
type Signal struct {
	Name string `json:"name"`
	// For is how long a series returns something before it is a window.
	For time.Duration `json:"for_ns"`
	// Match are the labels a firing must agree with its windows on: nil
	// for the match in effect (Options.Match), empty for none.
	Match []string `json:"match"`
	// Record says whether reading it all along keeps the record: false for
	// one that says only that users weren't hurt in one way, such as
	// payments failing, not that nothing was.
	Record bool `json:"record"`
	// Start is from when it keeps the record: where it was first read,
	// with data, or later, where the data of every selector it reads
	// begins. Zero for never: a selector it reads has no data.
	Start time.Time `json:"start,omitzero"`
	// Seen is what its series returned, for the labels it is matched on.
	Seen *Seen `json:"-"`
}

// matchOr is the labels it is matched on: its own, or else those in effect.
func (s Signal) matchOr(inEffect []string) []string {
	if s.Match != nil {
		return s.Match
	}
	return inEffect
}

// MatchOr is the labels a signal's windows are matched on: its own, or
// else those in effect.
func (s Signal) MatchOr(inEffect []string) []string { return s.matchOr(inEffect) }

// Seen is what a signal's series have returned, for each label its windows
// are matched on: the values each label had, or every value, once past
// MaxSeenValues or MaxSeenBytes, or once a series returned something
// without that label, which, as Agree has it, any firing's value agrees
// with.
type Seen struct {
	values map[string]map[string]bool
	all    map[string]bool
	bytes  int
}

// NewSeen is what a signal has returned before any series: nothing, of the
// labels named.
func NewSeen(labels []string) *Seen {
	s := &Seen{values: map[string]map[string]bool{}, all: map[string]bool{}}
	for _, l := range labels {
		s.values[l] = map[string]bool{}
	}
	return s
}

// Add keeps what a series that returned something had, of the labels
// named.
func (s *Seen) Add(labels map[string]string) {
	for l, vs := range s.values {
		if s.all[l] {
			continue
		}
		v, ok := labels[l]
		switch {
		case vs[v]:
		case !ok, len(vs) >= MaxSeenValues || s.bytes+len(l)+len(v) > MaxSeenBytes:
			s.every(l)
		default:
			vs[v] = true
			s.bytes += len(l) + len(v)
		}
	}
}

// every takes the signal to have returned every value of a label, and lets
// the values kept of it go.
func (s *Seen) every(l string) {
	s.all[l] = true
	for x := range s.values[l] {
		s.bytes -= len(l) + len(x)
	}
	s.values[l] = nil
}

// Ties reports whether a firing with labels could have been seen by the
// signal, by the labels named: for each that it has, its value is one a
// series had, or the signal is taken to have returned every value. A label
// the firing hasn't ties it, as in Agree. Nil, nothing was kept of what it
// returned, and it is taken to have returned everything.
func (s *Seen) Ties(labels map[string]string, names []string) bool {
	if s == nil {
		return true
	}
	for _, l := range names {
		v, ok := labels[l]
		if !ok || s.all[l] {
			continue
		}
		vs, kept := s.values[l]
		if !kept {
			continue // not a label it kept: nothing known of it
		}
		if !vs[v] {
			return false
		}
	}
	return true
}

// Bytes is how much of what it returned is kept, as MaxSeenBytes counts it.
func (s *Seen) Bytes() int { return s.bytes }

// RuleEntry says which signals' windows the rules it names are judged
// against, and on which labels (docs/design/signals.md, "Which rules a
// signal judges"): the rules whose names match Rules, or else whose labels
// have the values in Labels.
type RuleEntry struct {
	Rules  string          `json:"rules,omitempty"`
	Labels incident.Labels `json:"labels,omitempty"`
	// Signals are the signals whose windows its rules are judged against:
	// a window of another signal is, to them, as if it weren't there.
	Signals []string `json:"signals"`
	// Match, when not nil, are the labels its rules' firings are matched
	// on against those windows, in place of each signal's; empty for none.
	Match []string `json:"match"`
	re    *regexp.Regexp
	idx   []int
}

// Signals are the named signals a report's firings are judged against, and
// the rule entries, in order.
type Signals struct {
	List    []Signal
	Entries []RuleEntry
	// entryOf is each rule's entry, an index in Entries, -1 for none: found
	// once a rule, as Index found it.
	entryOf map[string]int
	// looked counts the entries looked for, a rule each: what judging with
	// entries costs.
	looked *atomic.Int64
}

// NewSignals are signals and rule entries to judge by, the entries' rule
// patterns compiled: an entry naming a signal not among them names none.
// Their rules' entries are looked for once Index says the rules' labels.
func NewSignals(list []Signal, entries []RuleEntry) *Signals {
	s := &Signals{List: list, Entries: slices.Clone(entries), looked: &atomic.Int64{}}
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.Rules != "" {
			e.re, _ = regexp.Compile(e.Rules)
		}
		e.idx = nil
		for _, name := range e.Signals {
			if j := s.signal(name); j >= 0 {
				e.idx = append(e.idx, j)
			}
		}
	}
	return s
}

// Named reports whether there are named signals to judge by.
func (s *Signals) Named() bool { return s != nil && len(s.List) > 0 }

// Looked is how many times a rule's entry was looked for.
func (s *Signals) Looked() int64 {
	if s == nil || s.looked == nil {
		return 0
	}
	return s.looked.Load()
}

func (s *Signals) signal(name string) int {
	return slices.IndexFunc(s.List, func(x Signal) bool { return x.Name == name })
}

// Index is s with each rule's entry found once, by the rules' labels: the
// first entry, in order, whose pattern matches the rule's name; else the
// first whose labels the rule has. The name decides before any label, as
// it does for a team.
func (s *Signals) Index(labels map[string]map[string]string) *Signals {
	if !s.Named() {
		return s
	}
	out := *s
	out.entryOf = make(map[string]int, len(labels))
	for rule, l := range labels {
		out.entryOf[rule] = s.find(rule, l)
	}
	return &out
}

// find is the rule's entry, an index in Entries, -1 for none.
func (s *Signals) find(rule string, labels map[string]string) int {
	if s.looked != nil {
		s.looked.Add(1)
	}
	for i, e := range s.Entries {
		if e.re != nil && e.re.MatchString(rule) {
			return i
		}
	}
	for i, e := range s.Entries {
		if len(e.Labels) > 0 && hasValues(labels, e.Labels) {
			return i
		}
	}
	return -1
}

// hasValues reports whether labels have, for each label named, one of its
// values.
func hasValues(labels map[string]string, want incident.Labels) bool {
	for k, vs := range want {
		if v, ok := labels[k]; !ok || !slices.Contains(vs, v) {
			return false
		}
	}
	return true
}

// Entry is the rule's entry, nil for none: as Index found it, or, of a
// rule it wasn't told of, by its name alone.
func (s *Signals) Entry(rule string) *RuleEntry {
	if i := s.entryIndex(rule); i >= 0 {
		return &s.Entries[i]
	}
	return nil
}

// EntryNumber is the rule's entry's place in Entries, from 1; 0 for none.
func (s *Signals) EntryNumber(rule string) int { return s.entryIndex(rule) + 1 }

func (s *Signals) entryIndex(rule string) int {
	if !s.Named() || len(s.Entries) == 0 {
		return -1
	}
	i, ok := s.entryOf[rule]
	if !ok {
		i = s.find(rule, nil)
	}
	return i
}

// Ties are the signals, as indexes in List, that could have seen the
// incident a firing of rule with labels would catch (docs/design/signals.md,
// "The record, signal by signal"): its rule entry's signals, and no others;
// else each signal for which each label it is matched on (its own, or
// inEffect) that the firing has, has a value the signal returned; and,
// when none of those keeps the record, every signal that does too: a
// firing whose values no signal that keeps the record has had waits for
// the latest start of them all, whatever a signal matched on no labels
// ties it to.
func (s *Signals) Ties(rule string, labels map[string]string, inEffect []string) []int {
	if !s.Named() {
		return nil
	}
	if e := s.Entry(rule); e != nil {
		return e.idx
	}
	var out []int
	keeps := false
	for i, x := range s.List {
		if x.Seen.Ties(labels, x.matchOr(inEffect)) {
			out = append(out, i)
			keeps = keeps || x.Record
		}
	}
	if keeps {
		return out
	}
	for i, x := range s.List {
		if x.Record && !slices.Contains(out, i) {
			out = append(out, i)
		}
	}
	slices.Sort(out)
	return out
}

// Names are the names of the signals at the indexes given.
func (s *Signals) Names(idx []int) []string {
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, s.List[i].Name)
	}
	return out
}

// Agrees reports whether a firing of rule with labels agrees with an
// incident on the labels it is matched on, and may catch it at all
// (docs/design/signals.md, "Judged by its own match"): of a signal's
// window, a rule entry for rule must name that signal, and its match, when
// it gives one, is the one; else the incident's own (Incident.Match); else
// the match in effect. An incident written down has no signal, and is
// matched on the match in effect, for every rule alike.
func (o Options) Agrees(rule string, labels map[string]string, in incident.Incident) bool {
	match := o.Match
	if in.Match != nil {
		match = in.Match
	}
	if in.Signal != "" && o.Signals.Named() {
		if e := o.Signals.Entry(rule); e != nil {
			if !slices.Contains(e.Signals, in.Signal) {
				return false
			}
			if e.Match != nil {
				match = e.Match
			}
		}
	}
	return in.Labels.Agree(labels, match)
}
