package response

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/deploys"
)

// Kind is what a change did to its target.
type Kind string

// The kinds of change (docs/design/responses.md, "What comes in").
const (
	Rollout  Kind = "rollout"
	Rollback Kind = "rollback"
	Scale    Kind = "scale"
	Deploy   Kind = "deploy"
)

// The kinds of workload kube-state-metrics tells of, as it names them.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
)

var workloadKinds = []string{KindDeployment, KindStatefulSet, KindDaemonSet}

// Change is one change made to the target of an alert.
type Change struct {
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at"`
	// Namespace, WorkloadKind and Workload are the target of a rollout,
	// rollback or scale: a Deployment, StatefulSet or DaemonSet.
	Namespace    string `json:"namespace,omitempty"`
	WorkloadKind string `json:"workload_kind,omitempty"`
	Workload     string `json:"workload,omitempty"`
	// Revision is what it rolled out to: a ReplicaSet's name for a
	// Deployment, a StatefulSet's update revision, a DaemonSet's
	// generation; a deploy's revision.
	Revision string `json:"revision,omitempty"`
	// Labels are a deploy's, which tie it to the firings it may concern.
	Labels map[string]string `json:"labels,omitempty"`
}

// Target is the thing a change was made to, as a name that counts its
// changes: a workload's namespace, kind and name; a deploy's labels.
func (c Change) Target() string {
	if c.Kind == Deploy {
		b, _ := json.Marshal(c.Labels) // keys sorted
		return "deploy\x00" + string(b)
	}
	return c.Namespace + "\x00" + c.WorkloadKind + "\x00" + c.Workload
}

// Workload names a workload.
type Workload struct{ Namespace, Kind, Name string }

// Pod names a pod.
type Pod struct{ Namespace, Name string }

// Span is a stretch of time; To is zero for one that goes on.
type Span struct{ From, To time.Time }

// Contains reports whether t is in the span.
func (s Span) Contains(t time.Time) bool {
	return !t.Before(s.From) && (s.To.IsZero() || !t.After(s.To))
}

// Rollouts is what was read from kube-state-metrics' history (ReadRollouts).
type Rollouts struct {
	// Changes are the rollouts, rollbacks and scales found, in order of time.
	Changes []Change
	// Coverage is, of each workload, the spans its changes are known over:
	// from where its series begin to the end of the period. A workload not
	// here, in a namespace not read, has no change known, not "no change".
	Coverage map[Workload][]Span
	// Owners are the workloads the pods named own, by the pod: through
	// kube_pod_owner, and, for a Deployment's, kube_replicaset_owner.
	Owners map[Pod]Workload
	// AutoscalersKnown says kube_horizontalpodautoscaler_info was found; when
	// it wasn't, every scale is counted, an autoscaler's too.
	AutoscalersKnown bool
}

// ChangeSet is the changes of a period, from the history and from the
// deploys file, and the spans over which they are known: what a firing's
// response is looked for among.
type ChangeSet struct {
	// From and To are the period, which the routine test counts a target's
	// changes over.
	From, To time.Time
	// AutoscalersKnown is as Rollouts's; Rolled says the history was read
	// for rollouts.
	AutoscalersKnown, Rolled bool

	byWorkload  map[Workload][]Change // in order of time
	deploys     []Change              // in order of time
	counts      map[string]int        // changes of each target, within the period
	coverage    map[Workload][]Span
	deployCover []deployCover
	coverByKey  map[string]int   // deployCover's index, by its labels
	coverByPair map[string][]int // deployCover's indexes, by its first label's name and value
	owners      map[Pod]Workload
}

type deployCover struct {
	labels map[string]string
	from   time.Time
}

// NewChangeSet is an empty set of changes for the period from to to.
func NewChangeSet(from, to time.Time) *ChangeSet {
	return &ChangeSet{
		From: from, To: to,
		byWorkload: map[Workload][]Change{},
		counts:     map[string]int{},
		coverage:   map[Workload][]Span{},
		owners:     map[Pod]Workload{},

		coverByKey:  map[string]int{},
		coverByPair: map[string][]int{},
	}
}

// AddRollouts adds what was read from the history.
func (cs *ChangeSet) AddRollouts(r *Rollouts) {
	if r == nil {
		return
	}
	cs.Rolled = true
	cs.AutoscalersKnown = cs.AutoscalersKnown || r.AutoscalersKnown
	for _, c := range r.Changes {
		w := Workload{c.Namespace, c.WorkloadKind, c.Workload}
		cs.byWorkload[w] = append(cs.byWorkload[w], c)
		cs.count(c)
	}
	for w := range cs.byWorkload {
		slices.SortStableFunc(cs.byWorkload[w], func(a, b Change) int { return a.At.Compare(b.At) })
	}
	for w, spans := range r.Coverage {
		cs.coverage[w] = MergeSpans(append(cs.coverage[w], spans...))
	}
	for p, w := range r.Owners {
		cs.owners[p] = w
	}
}

// AddDeploys adds the deploys of the deploys file, each a change at its
// start. A target the file lists has its changes known from its earliest
// deploy listed on.
func (cs *ChangeSet) AddDeploys(ds []deploys.Deploy) {
	for _, d := range ds {
		c := Change{Kind: Deploy, At: d.Start, Labels: d.Labels, Revision: d.Revision}
		cs.deploys = append(cs.deploys, c)
		cs.count(c)
		cs.AddDeployCoverage(d.Labels, d.Start)
	}
	slices.SortStableFunc(cs.deploys, func(a, b Change) int { return a.At.Compare(b.At) })
}

// AddDeployCoverage says the changes of the target the deploys with labels
// name are known from from on: a deploy listed for it at from, or earlier.
// A ledger that keeps only the deploys of a period adds where each target's
// begin, beside them. A deploy that lists no label names no target, and
// covers nothing.
func (cs *ChangeSet) AddDeployCoverage(labels map[string]string, from time.Time) {
	if len(labels) == 0 {
		return
	}
	b, _ := json.Marshal(labels) // keys sorted
	key := string(b)
	if i, ok := cs.coverByKey[key]; ok {
		if from.Before(cs.deployCover[i].from) {
			cs.deployCover[i].from = from
		}
		return
	}
	i := len(cs.deployCover)
	cs.deployCover = append(cs.deployCover, deployCover{labels, from})
	cs.coverByKey[key] = i
	// Indexed by its least label, which a firing it covers carries too.
	pair := leastPair(labels)
	cs.coverByPair[pair] = append(cs.coverByPair[pair], i)
}

func leastPair(labels map[string]string) string {
	best, bv := "", ""
	first := true
	for k, v := range labels {
		if first || k < best {
			best, bv, first = k, v, false
		}
	}
	return best + "\x00" + bv
}

// MergeSpans puts spans in order and joins those that overlap or touch; a
// span that goes on (To zero) goes on past any after it.
func MergeSpans(spans []Span) []Span {
	if len(spans) < 2 {
		return spans
	}
	spans = slices.Clone(spans)
	slices.SortFunc(spans, func(a, b Span) int { return a.From.Compare(b.From) })
	out := spans[:1]
	for _, s := range spans[1:] {
		last := &out[len(out)-1]
		switch {
		case last.To.IsZero():
		case s.From.After(last.To):
			out = append(out, s)
		case s.To.IsZero() || s.To.After(last.To):
			last.To = s.To
		}
	}
	return out
}

// IntersectSpans are the stretches of time both lists of spans (each in
// order, joined: MergeSpans) are over.
func IntersectSpans(a, b []Span) []Span {
	forever := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	end := func(s Span) time.Time {
		if s.To.IsZero() {
			return forever
		}
		return s.To
	}
	var out []Span
	for i, j := 0, 0; i < len(a) && j < len(b); {
		from := a[i].From
		if b[j].From.After(from) {
			from = b[j].From
		}
		to := end(a[i])
		if e := end(b[j]); e.Before(to) {
			to = e
		}
		if !to.Before(from) {
			if to.Equal(forever) {
				to = time.Time{}
			}
			out = append(out, Span{from, to})
		}
		if end(a[i]).Before(end(b[j])) {
			i++
		} else {
			j++
		}
	}
	return out
}

// count notes a change of its target when it falls within the period.
func (cs *ChangeSet) count(c Change) {
	if !c.At.Before(cs.From) && !c.At.After(cs.To) {
		cs.counts[c.Target()]++
	}
}

// Count is how many changes the target of c had over the period.
func (cs *ChangeSet) Count(c Change) int { return cs.counts[c.Target()] }

// Changes are all the changes in the set, in order of time.
func (cs *ChangeSet) Changes() []Change {
	var out []Change
	for _, l := range cs.byWorkload {
		out = append(out, l...)
	}
	out = append(out, cs.deploys...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Target() < out[j].Target()
	})
	return out
}

// workloads are the workloads a firing with labels may be of: the firing
// carries a namespace, and names the workload by a label of the kind
// (deployment, statefulset, daemonset), by a pod the workload owns, or by a
// label in match whose value is the workload's name, of any kind.
func (cs *ChangeSet) workloads(labels map[string]string, match []string) []Workload {
	ns := labels["namespace"]
	if ns == "" || cs == nil {
		return nil
	}
	var out []Workload
	add := func(w Workload) {
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	for _, k := range workloadKinds {
		if name := labels[strings.ToLower(k)]; name != "" {
			add(Workload{ns, k, name})
		}
	}
	if pod := labels["pod"]; pod != "" {
		if w, ok := cs.owners[Pod{ns, pod}]; ok {
			add(w)
		}
	}
	for _, m := range match {
		if name := labels[m]; name != "" {
			for _, k := range workloadKinds {
				add(Workload{ns, k, name})
			}
		}
	}
	return out
}

// Matching are the changes that started after from and no later than to,
// that are on the target of a firing with labels, in order of time. match
// are the labels --rollout-match names.
func (cs *ChangeSet) Matching(labels map[string]string, match []string, from, to time.Time) []Change {
	if cs == nil {
		return nil
	}
	var out []Change
	for _, w := range cs.workloads(labels, match) {
		out = append(out, within(cs.byWorkload[w], from, to, nil)...)
	}
	// A deploy is tied as deploys.Deploy.During ties one, by its labels.
	out = append(out, within(cs.deploys, from, to, func(c Change) bool {
		return deploys.Deploy{Labels: c.Labels}.Tied(labels)
	})...)
	slices.SortStableFunc(out, func(a, b Change) int { return a.At.Compare(b.At) })
	return out
}

// within are the changes of list (in order of time) after from and no later
// than to, that ok holds (nil for any).
func within(list []Change, from, to time.Time, ok func(Change) bool) []Change {
	i := sort.Search(len(list), func(i int) bool { return list[i].At.After(from) })
	var out []Change
	for ; i < len(list) && !list[i].At.After(to); i++ {
		if ok == nil || ok(list[i]) {
			out = append(out, list[i])
		}
	}
	return out
}

// Known reports whether the changes of the target of a firing with labels
// are known from from to to, the whole of it: one span of a workload it names
// holds all of [from, to] (the changes of a firing are looked for over the
// firing's whole length, now for one still on), or a deploy the file lists
// for it, every label of which the firing carries, was made by from. A
// firing that names no target has none known.
func (cs *ChangeSet) Known(labels map[string]string, match []string, from, to time.Time) bool {
	if cs == nil {
		return false
	}
	if to.Before(from) {
		to = from
	}
	for _, w := range cs.workloads(labels, match) {
		for _, s := range cs.coverage[w] {
			if !from.Before(s.From) && (s.To.IsZero() || !to.After(s.To)) {
				return true
			}
		}
	}
	for k, v := range labels {
		for _, i := range cs.coverByPair[k+"\x00"+v] {
			d := cs.deployCover[i]
			if !from.Before(d.from) && carries(labels, d.labels) {
				return true
			}
		}
	}
	return false
}

// carries reports whether labels hold every label of want, with its value.
func carries(labels, want map[string]string) bool {
	for k, v := range want {
		if have, ok := labels[k]; !ok || have != v {
			return false
		}
	}
	return true
}
