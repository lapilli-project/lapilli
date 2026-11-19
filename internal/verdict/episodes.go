package verdict

import (
	"strings"
	"time"
)

// An episode is the firings of one rule with the same values of its match
// labels (the match in effect, Options.Match), each starting while another
// of them is on, or within Options.RefireWithin of the latest end among
// them so far; the first such firing of a day, in the zone days are
// counted in, starts a new one (docs/design/judging.md, item 2). It is
// counted beside the firings and changes none of their numbers.
//
// An episode is white when any of its firings is white; else judged, and
// black, when its first firing was judged, and not judged when it wasn't.
// Its day, week, marks and record are its first firing's.

// EpisodeCount is a rule's episodes: Total, White those with a white
// firing, Unjudged those not judged (no white firing, and its first
// firing had no record), and Noisy whether the rule is noisy by its
// episodes, by the same thresholds as by its firings (NoisyAt). Noisy by
// episodes is shown, and decides nothing.
type EpisodeCount struct {
	Total    int  `json:"total"`
	White    int  `json:"white"`
	Unjudged int  `json:"unjudged,omitempty"`
	Noisy    bool `json:"noisy"`
}

// Judged is how many episodes were judged.
func (e EpisodeCount) Judged() int { return e.Total - e.Unjudged }

// Precision is the share of the episodes judged with a white firing; ok
// is false when none was judged.
func (e EpisodeCount) Precision() (float64, bool) {
	if n := e.Judged(); n > 0 {
		return float64(e.White) / float64(n), true
	}
	return 0, false
}

// EpisodeKey is what the firings of one episode share: their rule, and of
// each label of match, the firing's value, or none for a label it doesn't
// carry. The labels that tell its alerts apart and aren't matched on (pod,
// instance) don't part them; with no match, every firing of a rule shares
// one.
func EpisodeKey(rule string, labels map[string]string, match []string) string {
	var b strings.Builder
	b.WriteString(rule)
	for _, m := range match {
		b.WriteByte(0)
		if v, ok := labels[m]; ok {
			b.WriteByte('=')
			b.WriteString(v)
		}
	}
	return b.String()
}

// EpisodeWalk follows the firings of one episode key (EpisodeKey) in
// order of their start, an episode at a time.
type EpisodeWalk struct {
	begun bool
	day   int64
	// until is the latest end of the episode's firings so far; on, that
	// one of them is still on.
	until time.Time
	on    bool
}

// Starts reports whether a firing that started at start, on day (DayIn),
// and ended at end (zero while it is on) starts a new episode: it is the
// key's first, or the first of its day, or it started more than refire
// after every firing of the episode so far ended. It is taken in either
// way.
func (w *EpisodeWalk) Starts(day int64, start, end time.Time, refire time.Duration) bool {
	if !w.begun || day != w.day || !w.on && start.Sub(w.until) > refire {
		w.begun, w.day, w.until, w.on = true, day, end, end.IsZero()
		return true
	}
	if end.IsZero() {
		w.on = true
	} else if end.After(w.until) {
		w.until = end
	}
	return false
}

// DayIn is the day t falls on in loc, as the days since 1970-01-01: as
// lapilli serve's ledger numbers them.
func DayIn(t time.Time, loc *time.Location) int64 {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// zone is the zone days are counted in: the machine's when none is given.
func (o Options) zone() *time.Location {
	if o.Zone != nil {
		return o.Zone
	}
	return time.Local
}

// countEpisodes gives each of the report's firings, in the order they
// have, the episode it is of (Judged.Episode, numbered from 1 in order of
// their first firing) and the day its start falls on (Judged.Day), and
// each rule its episodes (RuleResult.Episodes).
func (r *Report) countEpisodes(o Options, at NoisyAt) {
	loc := o.zone()
	type walk struct {
		EpisodeWalk
		n int
	}
	walks := map[string]*walk{}
	var first []int  // each episode's first firing
	var white []bool // whether any of its firings is white
	for i := range r.Firings {
		j := &r.Firings[i]
		j.Day = j.Start.In(loc).Format(time.DateOnly)
		k := EpisodeKey(j.Rule, j.Labels, o.Match)
		w := walks[k]
		if w == nil {
			w = &walk{}
			walks[k] = w
		}
		if w.Starts(DayIn(j.Start, loc), j.Start, j.End, o.RefireWithin) {
			first, white = append(first, i), append(white, false)
			w.n = len(first)
		}
		j.Episode = w.n
		white[w.n-1] = white[w.n-1] || j.White
	}
	byRule := map[string]*EpisodeCount{}
	for e, i := range first {
		f := r.Firings[i]
		c := byRule[f.Rule]
		if c == nil {
			c = &EpisodeCount{}
			byRule[f.Rule] = c
		}
		c.Total++
		switch {
		case white[e]:
			c.White++
		case f.NoRecord:
			c.Unjudged++
		}
	}
	for i := range r.Rules {
		if c := byRule[r.Rules[i].Rule]; c != nil {
			c.Noisy = IsNoisy(c.Judged(), c.White, at)
			r.Rules[i].Episodes = *c
		}
	}
}
