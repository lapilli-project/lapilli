package verdict

import (
	"html"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/incident"
)

// Directions a dependency is followed in (--depends-direction).
const (
	// DependsDown lets a firing on a service an incident's service calls
	// catch it: a cause catching what it caused.
	DependsDown = "down"
	// DependsBoth also lets a firing on a caller catch an incident in a
	// service it calls.
	DependsBoth = "both"
)

// DependsDefaultDepth is how many calls a dependency is followed over when
// none is given, and DependsMaxDepth the most.
const (
	DependsDefaultDepth = 1
	DependsMaxDepth     = graph.MaxDepth
)

// DependsMaxEdges is the most edges an hour of the graph may have.
const DependsMaxEdges = graph.MaxEdgesPerHour

// DependsMaxNodes is the most services an hour of the graph may name.
const DependsMaxNodes = graph.MaxNodesPerHour

// dependencyWindow is how long before an incident starts the edges that
// held count (docs/design/dependencies.md).
const dependencyWindow = time.Hour

// Dependencies are the service graph a firing is judged by when its labels
// don't agree with an incident's (Options.Depends).
type Dependencies struct {
	Graph *graph.Graph
	// Label is the label of the match whose values are the graph's nodes.
	Label string
	// Depth is how many calls are followed, 1 to DependsMaxDepth.
	Depth int
	// Direction is DependsDown (the default) or DependsBoth.
	Direction string
	// Key says what the graph was read with, as lapilli serve keys it:
	// what a ledger judged by it is judged again when it changes. Not
	// used in judging.
	Key string
	// CountCatches lets a catch made only by the graph count where a
	// pull request's change is held to the incidents (--fail-missed) and
	// where a rule is called spare: by default it doesn't, as anyone who
	// can write a series that says who calls whom could make it
	// (--depends-count-missed).
	CountCatches bool
}

func (d *Dependencies) depth() int {
	if d.Depth < 1 {
		return DependsDefaultDepth
	}
	return min(d.Depth, DependsMaxDepth)
}

// Via is how an incident reaches a firing's service through the graph: the
// services on the way, in call order, from the caller to the callee. Of an
// incident with several values of the label, the shortest way from any;
// ties go to the earlier value, and to the incident's side calling. It is a
// lookup in what the graph reaches from the incident's values in the window
// (graph.Graph.Reach), walked once for them and not for each firing.
func (d *Dependencies) Via(fv string, values []string, w graph.Window) []string {
	return d.Graph.Reach(values, w, d.depth(), d.Direction == DependsBoth)[fv]
}

// Caught is whether a firing catches an incident, and how.
type Caught struct {
	// Caught is true when it does.
	Caught bool
	// Linked says it did as one of the alerts the incident lists.
	Linked bool
	// Lacked names the labels of the match in effect it lacks that the
	// incident has (Catch.Lacked).
	Lacked []string
	// Dependency is, of a firing that caught it by the service graph alone,
	// the way from the caller to the callee (Catch.Dependency).
	Dependency []string
}

// CatchesBy is CatchesLinked, and, of a firing that catches by neither a
// link nor its labels, whether it does by the service graph
// (Options.Depends): the value of its label reaches the incident's through
// calls that held from an hour before the incident to its end, and it
// agrees on every other label of the match. Precedence: linked, labels,
// dependency.
func CatchesBy(f firing.Firing, in incident.Incident, o Options) Caught {
	to := in.EndOr(o.UnknownLen)
	if o.Signals.NotJudgedRules() && o.NotJudged(f.Rule) {
		return Caught{}
	}
	if f.Start.After(to) || f.Start.Before(in.Start.Add(-o.MaxLead())) {
		return Caught{}
	}
	j := o.judgingOf(f.Rule, in)
	if f.Start.Before(in.Start.Add(-o.leadOf(j, in))) {
		return Caught{}
	}
	if in.Links(f.Rule, f.Labels) {
		return Caught{Caught: true, Linked: true}
	}
	if o.LinkedOnly && len(in.Alerts) > 0 {
		return Caught{}
	}
	if ok, lacked := o.agreesLoose(j, f.Labels, in); ok {
		return Caught{Caught: true, Lacked: lacked}
	}
	if path, lacked, ok := o.dependsOn(j, f.Labels, in); ok {
		return Caught{Caught: true, Lacked: lacked, Dependency: path}
	}
	return Caught{}
}

// dependsOn reports whether a firing with labels, whose labels don't agree
// with the incident's, is about it by the service graph: its value of the
// graph's label is reached from one of the incident's, or reaches it with
// both directions; it agrees on every other label of the match in effect.
// The way is from the caller to the callee.
func (o Options) dependsOn(j judging, labels map[string]string, in incident.Incident) (path, lacked []string, ok bool) {
	d := o.Depends
	if d == nil || d.Graph == nil || d.Label == "" {
		return nil, nil, false
	}
	fv := labels[d.Label]
	values := in.Labels[d.Label]
	if fv == "" || len(values) == 0 || slices.Contains(values, fv) {
		return nil, nil, false
	}
	match, ok := o.matchFor(j, labels, in)
	if !ok {
		return nil, nil, false
	}
	other := make([]string, 0, len(match))
	for _, m := range match {
		if m != d.Label {
			other = append(other, m)
		}
	}
	agrees, lacked := o.agreeOn(true, labels, in, other)
	if !agrees {
		return nil, nil, false
	}
	w := graph.Window{Start: in.Start.Add(-dependencyWindow), End: in.EndOr(o.UnknownLen)}
	path = d.Via(fv, values, w)
	return path, lacked, path != nil
}

// DependsText is a path as the report writes it: "checkout → payments-db".
// The graph keeps only names that hold neither "→" nor ",", nor a
// character that isn't printable (graph.ValidName), so each hop is one
// service and the text can't be forged to show another path.
func DependsText(path []string) string { return strings.Join(path, " → ") }

// DependsHops is a path as separate hops for a page: each service escaped
// for HTML and wrapped in its own <bdi>, so that no name's direction
// leaks into its neighbours, and joined by an arrow outside every <bdi>
// ("<bdi>checkout</bdi> → <bdi>payments-db</bdi>"). The result is HTML to
// write as it is.
func DependsHops(path []string) string {
	parts := make([]string, len(path))
	for i, v := range path {
		parts[i] = "<bdi>" + html.EscapeString(v) + "</bdi>"
	}
	return strings.Join(parts, " → ")
}

// DependsSettings are the service graph a report was judged by.
type DependsSettings struct {
	// Label is the label whose values the graph's services are.
	Label string `json:"label"`
	// Depth is how many calls were followed; Direction down or both.
	Depth     int    `json:"depth"`
	Direction string `json:"direction"`
	// Edges is how many pairs of services held an edge in the period read.
	Edges int `json:"edges"`
	// TooMany says the graph had more edges in an hour than are kept
	// (graph.MaxEdgesPerHour): it kept none, and no firing was judged by it.
	TooMany bool `json:"too_many,omitempty"`
	// Over says lapilli serve judged some of the period with no graph: its
	// incidents' windows held more edges than a judgement loads. No catch
	// there was by dependency.
	Over bool `json:"over,omitempty"`
	// Refused is how many names the graph was offered that aren't a
	// service's (graph.ValidName), left out of it.
	Refused int `json:"refused,omitempty"`
	// CountCatches says a catch made only by the graph counted where a
	// change is held to the incidents and for spare rules.
	CountCatches bool `json:"count_catches,omitempty"`
}

func (d *Dependencies) settings() *DependsSettings {
	if d == nil {
		return nil
	}
	return &DependsSettings{Label: d.Label, Depth: d.depth(), Direction: cmpDirection(d.Direction),
		Edges: d.Graph.Edges(), TooMany: d.Graph.TooMany(), Refused: d.Graph.Refused(), CountCatches: d.CountCatches}
}

func cmpDirection(s string) string {
	if s == DependsBoth {
		return DependsBoth
	}
	return DependsDown
}
