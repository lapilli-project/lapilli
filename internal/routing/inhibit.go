package routing

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// An inhibitor is the inhibition rules' view of the alerts: for each rule
// and each value of its equal labels, the alerts that match its source
// (inhibit.InhibitRule's cache), ready to be asked whether one fires.
type inhibitor struct {
	r     *runner
	rules []InhibitRule
	// match remembers whether a label set matches a rule's source and
	// target matchers: 1 for the source, 2 for the target. It holds
	// maxMatchCache at most; a pair past it is worked out again.
	match map[[2]int]int
	// all are the alerts matching a rule's source, by the rule and the
	// values of its equal labels; onlyS those among them that don't match its
	// target too, which are what an alert that matches both is inhibited by.
	all, onlyS []map[string]*sources
}

// sources are alerts sorted by when they started, with what asking whether
// one is firing at a time takes.
type sources struct {
	at      []int       // positions in the alerts given
	start   []time.Time // when each started
	end     []time.Time // when each ends
	maxEnd  []time.Time // the latest any up to it ends
	changes []time.Time // each time one starts or ends, sorted, once each
}

// maxMatchCache is how many (rule, label set) pairs the inhibitor
// remembers the matching of: 2 million is some 100 MB.
const maxMatchCache = 2_000_000

func newInhibitor(r *runner) *inhibitor {
	h := &inhibitor{r: r, rules: r.c.inhibit, match: map[[2]int]int{}}
	h.all = make([]map[string]*sources, len(h.rules))
	h.onlyS = make([]map[string]*sources, len(h.rules))
	for k := range h.rules {
		all := map[string][]int{}
		only := map[string][]int{}
		if !r.spend(len(r.in)) {
			return nil
		}
		for i, x := range r.in {
			m := h.matches(k, x.fp)
			if m&1 == 0 {
				continue
			}
			eq := h.eqKey(k, x.labels)
			all[eq] = append(all[eq], i)
			if m&2 == 0 {
				only[eq] = append(only[eq], i)
			}
		}
		h.all[k] = h.build(all)
		h.onlyS[k] = h.build(only)
	}
	return h
}

func (h *inhibitor) build(by map[string][]int) map[string]*sources {
	out := make(map[string]*sources, len(by))
	for eq, at := range by {
		s := &sources{at: at}
		sort.SliceStable(s.at, func(a, b int) bool {
			x, y := h.r.in[s.at[a]].start, h.r.in[s.at[b]].start
			if !x.Equal(y) {
				return x.Before(y)
			}
			return s.at[a] < s.at[b]
		})
		var latest time.Time
		for _, i := range s.at {
			x := h.r.in[i]
			s.start = append(s.start, x.start)
			s.end = append(s.end, x.end)
			if x.end.After(latest) {
				latest = x.end
			}
			s.maxEnd = append(s.maxEnd, latest)
			s.changes = append(s.changes, x.start)
			if x.end != never {
				s.changes = append(s.changes, x.end)
			}
		}
		slices.SortFunc(s.changes, func(a, b time.Time) int { return a.Compare(b) })
		s.changes = slices.CompactFunc(s.changes, func(a, b time.Time) bool { return a.Equal(b) })
		out[eq] = s
	}
	return out
}

// matches is whether a label set matches rule k's source (bit 1) and its
// target (bit 2).
func (h *inhibitor) matches(k, fp int) int {
	key := [2]int{k, fp}
	if m, ok := h.match[key]; ok {
		return m
	}
	h.r.spend(1)
	m := 0
	if matchAll(h.rules[k].SourceMatchers, h.r.fpLab[fp]) {
		m |= 1
	}
	if matchAll(h.rules[k].TargetMatchers, h.r.fpLab[fp]) {
		m |= 2
	}
	if len(h.match) < maxMatchCache {
		h.match[key] = m
	}
	return m
}

// eqKey is a label set's values of a rule's equal labels: an absent one
// agrees with an absent one, and with an empty one, as Alertmanager's
// fingerprint of them does.
func (h *inhibitor) eqKey(k int, labels map[string]string) string {
	var b strings.Builder
	for _, ln := range h.rules[k].Equal {
		b.WriteString(labels[ln])
		b.WriteByte(0xff)
	}
	return b.String()
}

// firing is the first of the alerts that fires at a time: one that has come
// and hasn't resolved.
func (s *sources) firing(at time.Time) (int, bool) {
	pos := sort.Search(len(s.start), func(k int) bool { return s.start[k].After(at) }) - 1
	if pos < 0 || !s.maxEnd[pos].After(at) {
		return 0, false
	}
	for k := pos; k >= 0; k-- {
		if s.end[k].After(at) {
			return s.at[k], true
		}
	}
	return 0, false
}

// inhibited reports the first rule that inhibits an alert at a time, and the
// first alert that does: its target matchers match the alert, and an alert
// fires that matches its source matchers and agrees with the alert on its
// equal labels. An alert that matches both sides is only inhibited by
// alerts that don't match the target, so it never inhibits itself
// (inhibit.Inhibitor.Mutes, InhibitRule.hasEqual).
func (r *runner) inhibited(i int, at time.Time) (rule, source int, ok bool) {
	if r.inh == nil {
		return 0, 0, false
	}
	h := r.inh
	x := r.in[i]
	if !r.spend(len(h.rules)) {
		return 0, 0, false
	}
	for k := range h.rules {
		m := h.matches(k, x.fp)
		if m&2 == 0 {
			continue
		}
		lists := h.all[k]
		if m&1 != 0 {
			lists = h.onlyS[k]
		}
		if s := lists[h.eqKey(k, x.labels)]; s != nil {
			if src, ok := s.firing(at); ok {
				return k, src, true
			}
		}
	}
	return 0, 0, false
}

// change is the first time after at when an alert that could inhibit alert i
// starts or ends; never when none does.
func (h *inhibitor) change(i int, at time.Time) time.Time {
	x := h.r.in[i]
	t := never
	for k := range h.rules {
		if h.matches(k, x.fp)&2 == 0 {
			continue
		}
		s := h.all[k][h.eqKey(k, x.labels)]
		if s == nil {
			continue
		}
		if p := sort.Search(len(s.changes), func(j int) bool { return s.changes[j].After(at) }); p < len(s.changes) && s.changes[p].Before(t) {
			t = s.changes[p]
		}
	}
	return t
}
