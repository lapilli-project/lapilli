package response

import (
	"math"
	"time"
)

// Type is a firing's response, as the ledger keeps it (firings.response).
type Type int

// The responses, in the order the design tries them (docs/design/responses.md,
// "A firing's response"), with the numbers the ledger keeps.
const (
	None           Type = 0 // nothing is known
	Changed        Type = 1 // a change on its target, not a routine one
	ChangedRoutine Type = 2 // a change on its target that was likely anyway
	Acknowledged   Type = 3 // its page was acknowledged, or resolved by a person
	Unanswered     Type = 4 // its page was not acknowledged and its alert manager resolved it
)

func (t Type) String() string {
	switch t {
	case Changed:
		return "changed"
	case ChangedRoutine:
		return "changed routine"
	case Acknowledged:
		return "acknowledged"
	case Unanswered:
		return "unanswered"
	}
	return "none"
}

// DefaultRoutine is --routine's default.
const DefaultRoutine = 0.5

// Firing is what a response is looked for for: one firing of a rule.
type Firing struct {
	Rule   string
	Labels map[string]string
	Start  time.Time
	// End is when it ended; On says it is still on, and End is then not
	// read: now is its end.
	End time.Time
	On  bool
}

// Evidence is what responses are found among. Any part may be nil or empty.
type Evidence struct {
	Pages   *Index
	Changes *ChangeSet
	// Match are the labels --rollout-match names.
	Match []string
	// Routine is --routine: a change is routine at or above this chance of
	// one falling in a span as long as the firing by luck.
	Routine float64
}

// Response is a firing's response: its kind, the change it rests on, and
// the page, which is there whatever the kind (a firing with a change and a
// page is Changed, its page still shown).
type Response struct {
	Type Type
	// Change is the change a Changed rests on (the earliest not routine);
	// the earliest routine change, for ChangedRoutine and also when a page
	// decided the Type (Acknowledged, Unanswered), for the report to show.
	// Nil when there is none, or the page was none of these.
	Change *Change
	// Page is the firing's page, nil for none.
	Page *Page
	// Known says the changes of the firing's target are known at the
	// firing's start (ChangeSet.Known), which Useful needs.
	Known bool
}

// Respond is the firing's response at now, the first of these that holds:
// a change on its target, not a routine one, that started after the firing
// started and no later than its end; its page acknowledged by a person, or
// resolved by one; its page not acknowledged and resolved by its alert
// manager; only routine changes on its target; none. A routine change is
// chance, not evidence, so it doesn't stop the order at the first step; it
// stays on the Response, for the report to show.
//
// within is the window a notification is tied to a firing by, two
// evaluations of its rule: its page is the nearest of those sent between the
// firing's start and its end plus within.
func (e Evidence) Respond(f Firing, within time.Duration, now time.Time) Response {
	end := f.End
	if f.On || end.IsZero() {
		end = now
	}
	r := Response{}
	r.Page = e.Pages.Find(f.Rule, f.Labels, f.Start, end.Add(within))
	r.Known = e.Changes.Known(f.Labels, e.Match, f.Start)

	var first *Change
	var firstRoutine *Change
	for _, c := range e.Changes.Matching(f.Labels, e.Match, f.Start, end) {
		if e.routine(c, end.Sub(f.Start)) {
			if firstRoutine == nil {
				firstRoutine = &c
			}
			continue
		}
		first = &c
		break
	}
	switch {
	case first != nil:
		r.Type, r.Change = Changed, first
	case r.Page != nil && (!r.Page.Acknowledged.IsZero() || r.Page.ResolvedBy == ByPerson):
		r.Type, r.Change = Acknowledged, firstRoutine
	case r.Page != nil && r.Page.Acknowledged.IsZero() && r.Page.ResolvedBy == ByAlert:
		r.Type, r.Change = Unanswered, firstRoutine
	case firstRoutine != nil:
		r.Type, r.Change = ChangedRoutine, firstRoutine
	}
	return r
}

// routine reports whether a change is routine: its target changed so often
// in the period that one falling in a span as long as the firing's is likely
// anyway. With n changes of the target over the period T and the firing
// lasting s, 1 - e^(-(n-1)s/T) at or above the threshold.
func (e Evidence) routine(c Change, s time.Duration) bool {
	return Routine(e.Changes.Count(c), s, e.Changes.To.Sub(e.Changes.From), e.Routine)
}

// Routine is the routine test: n changes of a target over a period lasting
// T, a firing lasting s, the chance 1 - e^(-(n-1)s/T) of one falling in a
// span as long as the firing's by luck, at or above threshold. A single
// change (n of 1 or less) leaves no chance; no period gives none.
func Routine(n int, s, T time.Duration, threshold float64) bool {
	if n <= 1 || T <= 0 || s < 0 {
		return false
	}
	return 1-math.Exp(-float64(n-1)*s.Seconds()/T.Seconds()) >= threshold
}

// Answer is a person's word on whether a firing needed action (judging.md,
// "Did it need action?").
type Answer string

// The answers; NoAnswer is none given.
const (
	NoAnswer  Answer = ""
	AnswerYes Answer = "yes"
	AnswerNo  Answer = "no"
)

// Usefulness is whether a firing was useful.
type Usefulness int

// The values of Usefulness, as the ledger keeps them (firings.useful).
const (
	Unknown   Usefulness = -1
	NotUseful Usefulness = 0
	IsUseful  Usefulness = 1
)

func (u Usefulness) String() string {
	switch u {
	case IsUseful:
		return "useful"
	case NotUseful:
		return "not useful"
	}
	return "unknown"
}

// Useful is whether a firing was useful (docs/design/responses.md, "Useful
// firings"): white says it caught an incident; judged, that an incident
// record judged it; answer is a person's word; r its response, and known
// whether the changes of its target are known at its time (Response.Known).
//
// Useful when white, when changed and not routine, or when a person said it
// needed action. Not useful when a person said it didn't, when it is
// unanswered, when it is acknowledged and its target's changes are known
// (someone looked, nothing changed), or when it is black and judged and none
// of the useful things holds. Otherwise unknown.
//
// A person's word is taken over the rest, as it is the one said of this
// firing alone: a white stone a person says needed no action (a coincidence)
// is not useful. The design leaves the two sides' order unsaid.
func Useful(white, judged bool, answer Answer, r Response, known bool) Usefulness {
	switch {
	case answer == AnswerYes:
		return IsUseful
	case answer == AnswerNo:
		return NotUseful
	case white:
		return IsUseful
	case r.Type == Changed:
		return IsUseful
	case r.Type == Unanswered:
		return NotUseful
	case r.Type == Acknowledged && known:
		return NotUseful
	case judged:
		// Black and judged, and no useful thing above: a routine change, or
		// an acknowledged firing whose target's changes aren't known, is
		// judged by its stone, as the design's last not-useful line says.
		return NotUseful
	}
	return Unknown
}
