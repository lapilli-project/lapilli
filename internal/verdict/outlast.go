package verdict

import (
	"fmt"
	"time"
)

// Outlast is a rule's "for" as long as the range its condition counts
// events over, or longer (rules.Rule.ForOutlasts): after one event the
// condition holds for that range at most, so one event alone can never fire
// it, only events that keep coming for the whole of its "for". The rule
// written to catch an event catches a run of them.
type Outlast struct {
	For    time.Duration `json:"for_ns"`
	Window time.Duration `json:"window_ns"`
}

// Text says it, in English.
func (o Outlast) Text() string {
	return fmt.Sprintf("Its for (%s) is as long as the %s its condition counts over, or longer: one event alone can never fire it; only events that keep coming for %s will.",
		Duration(o.For), Duration(o.Window), Duration(o.For))
}

// Proposed says it of a "for" proposed, in English.
func (o Outlast) Proposed() string {
	return fmt.Sprintf("As proposed, its for (%s) is as long as the %s its condition counts over, or longer: one event alone could never fire it; only events that keep coming for %s would.",
		Duration(o.For), Duration(o.Window), Duration(o.For))
}
