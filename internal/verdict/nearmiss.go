package verdict

import (
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// NearMiss is a rule that nearly caught an incident: its condition held from
// From, counted from the incident's start, but either not for its "for", so
// it never fired, or only so that it fired after the incident was over
// (Late).
type NearMiss struct {
	Incident string            `json:"incident"`
	Rule     string            `json:"rule"`
	Labels   map[string]string `json:"labels,omitempty"` // the alert's, while it held
	From     time.Duration     `json:"from_ns"`
	// Held is how long the condition held, when that was under its "for";
	// when Late, Fired is when the rule did fire, from the incident's start.
	Held  time.Duration `json:"held_ns,omitempty"`
	Late  bool          `json:"late,omitempty"`
	Fired time.Duration `json:"fired_ns,omitempty"`
	For   time.Duration `json:"for_ns"`
	// NewFor is the longest "for" that would have caught the incident; the
	// rule would then have fired Delay from its start.
	NewFor time.Duration `json:"new_for_ns"`
	Delay  time.Duration `json:"delay_ns"`
	// Before and After are the rule's stones over the period with its "for"
	// and with NewFor; After is nil when that wasn't worked out.
	Before RuleResult  `json:"before"`
	After  *RuleResult `json:"after,omitempty"`
}

// Nearly finds, of the stretches a rule's condition held (firings with no
// "for", from the first evaluation that held to after the last), the one
// that would have caught the incident with the longest "for" short of the
// rule's own. every is how often the rule was evaluated. ok is false when no
// shorter "for" would have caught it.
func Nearly(in incident.Incident, held []firing.Firing, ruleFor, every time.Duration, o Options) (NearMiss, bool) {
	var best NearMiss
	found := false
	from, to := in.Start.Add(-o.Lead), in.EndOr(o.UnknownLen)
	// The longest "for" short of the rule's own, in whole evaluations.
	longest := (ruleFor - 1) / every * every
	for _, f := range held {
		if f.End.IsZero() || !in.Labels.Agree(f.Labels, o.Match) {
			continue
		}
		// With a "for" of d it fires at Start+d, if it still held then, and
		// catches the incident if that is in [from, to].
		length := f.End.Sub(f.Start)
		d := min(length-every, to.Sub(f.Start), longest)
		if d < 0 {
			continue
		}
		d = d / every * every
		at := f.Start.Add(d)
		if at.Before(from) {
			continue
		}
		delay := at.Sub(in.Start)
		if found && (d < best.NewFor || d == best.NewFor && delay >= best.Delay) {
			continue
		}
		best = NearMiss{Incident: in.ID, Labels: f.Labels, From: f.Start.Sub(in.Start), For: ruleFor, NewFor: d, Delay: delay}
		if fires := (ruleFor + every - 1) / every * every; length-every >= fires {
			// It held long enough: it fired, once its "for" was up, too late.
			best.Late = true
			best.Fired = f.Start.Add(fires).Sub(in.Start)
		} else {
			best.Held = length
		}
		found = true
	}
	return best, found
}
