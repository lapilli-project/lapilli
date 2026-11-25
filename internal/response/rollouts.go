package response

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/prom"
)

// MaxNames is how many namespaces or pods a query's regular expression
// names at most: more are asked for in more queries.
const MaxNames = 200

// History is the part of a Prometheus-compatible API the rollouts reader
// uses, as replay.History is: a *prom.Client, with its limits
// (--max-samples, the limiter, retries) applied to every query.
type History interface {
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error)
}

// ReadRollouts reads, from kube-state-metrics' history over [from, to] at
// step, the changes made to the workloads of namespaces (docs/design/responses.md,
// "What comes in"), and which workloads the pods own.
//
//   - a Deployment's rollout: a ReplicaSet created in the period, owned by
//     the Deployment, at the second it was created;
//   - its rollback: a ReplicaSet whose spec replicas go from 0 to more, at the
//     step they do (one created in the period, only after it was above 0 and
//     then 0), unless the Deployment's own replicas go from 0 to more then
//     (that is a scale from zero);
//   - a StatefulSet's rollout: its update revision changing; a DaemonSet's:
//     its generation going up; at the first step it shows;
//   - a scale of a Deployment or StatefulSet no HorizontalPodAutoscaler
//     targets: its replicas changing.
//
// The namespaces the pods are in are read too. Names go into a regular
// expression quoted, MaxNames to a query. Series that differ only by the
// kube-state-metrics replica they came from are one: of those at one step
// the greatest value is taken, and a revision seen on any is seen. What a
// ReplicaSet, a pod, or an
// autoscaler is is asked once, as of the period's end, over the whole period
// (last_over_time), so those that were deleted before it are found too; the
// replica, revision and generation series are asked at step.
func ReadRollouts(ctx context.Context, h History, from, to time.Time, step time.Duration, namespaces []string, pods []Pod) (*Rollouts, error) {
	out := &Rollouts{Coverage: map[Workload][]Span{}, Owners: map[Pod]Workload{}}
	if step <= 0 {
		return nil, fmt.Errorf("reading rollouts: step must be positive")
	}
	nsSet := map[string]bool{}
	for _, n := range namespaces {
		nsSet[n] = true
	}
	podSet := map[Pod]bool{}
	for _, p := range pods {
		podSet[p] = true
		nsSet[p.Namespace] = true
	}
	delete(nsSet, "")
	if len(nsSet) == 0 {
		return out, nil
	}
	r := &reader{h: h, from: from, to: to, step: step, out: out,
		window:  strconv.FormatInt(int64((to.Sub(from)+step)/time.Second), 10) + "s",
		rsOwner: map[rsKey]string{}, hpa: map[Workload]bool{},
		rsCover: map[Workload][]Span{}, depUp: map[Workload]map[time.Time]bool{},
		stsReplicas: map[Workload][]Span{}, stsRevisions: map[Workload][]Span{}}

	nss := slices.Sorted(mapKeys(nsSet))
	for _, chunk := range chunks(nss) {
		if err := r.namespaces(ctx, "namespace=~"+matcher(chunk)); err != nil {
			return nil, err
		}
	}
	// Looked for beyond the namespaces too: an autoscaler elsewhere says the
	// metric is exported, though none is in these.
	if !r.hpaInfo {
		any, err := r.anyHPA(ctx)
		if err != nil {
			return nil, err
		}
		r.hpaInfo = any
	}
	out.AutoscalersKnown = r.hpaInfo
	for _, chunk := range chunks(nss) {
		if err := r.scales(ctx, "namespace=~"+matcher(chunk)); err != nil {
			return nil, err
		}
	}
	r.flushRollbacks()
	if len(podSet) > 0 {
		ps := make([]Pod, 0, len(podSet))
		for p := range podSet {
			ps = append(ps, p)
		}
		slices.SortFunc(ps, func(a, b Pod) int {
			if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
				return c
			}
			return strings.Compare(a.Name, b.Name)
		})
		for i := 0; i < len(ps); i += MaxNames {
			part := ps[i:min(i+MaxNames, len(ps))]
			var pn, pp []string
			seen := map[string]bool{}
			for _, p := range part {
				pp = append(pp, p.Name)
				if !seen[p.Namespace] {
					seen[p.Namespace] = true
					pn = append(pn, p.Namespace)
				}
			}
			if err := r.pods(ctx, "namespace=~"+matcher(pn)+",pod=~"+matcher(pp)); err != nil {
				return nil, err
			}
		}
	}
	out.Changes = dedupe(out.Changes)
	sort.SliceStable(out.Changes, func(i, j int) bool {
		a, b := out.Changes[i], out.Changes[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.Target() < b.Target()
	})
	return out, nil
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// chunks splits names into lists of MaxNames at most.
func chunks(names []string) [][]string {
	var out [][]string
	for i := 0; i < len(names); i += MaxNames {
		out = append(out, names[i:min(i+MaxNames, len(names))])
	}
	return out
}

// matcher is a PromQL string holding a regular expression that matches the
// names, each quoted as a literal (regexp.QuoteMeta) and the whole quoted
// as a string.
func matcher(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = regexp.QuoteMeta(n)
	}
	return strconv.Quote(strings.Join(q, "|"))
}

type rsKey struct{ ns, rs string }

type reader struct {
	h        History
	from, to time.Time
	step     time.Duration
	window   string
	out      *Rollouts
	rsOwner  map[rsKey]string  // a ReplicaSet's Deployment
	hpa      map[Workload]bool // the workloads an autoscaler targets
	hpaInfo  bool
	// rsCover is where the ReplicaSet series of a Deployment (owner, created
	// and spec replicas) are, which its changes are known over.
	rsCover map[Workload][]Span
	// rollbacks are those found among the ReplicaSets' replicas, held until
	// the Deployments' own are read.
	rollbacks []Change
	// depUp is, of a Deployment, the steps its own replicas went from 0 to more.
	depUp map[Workload]map[time.Time]bool
	// A StatefulSet's replicas' and revisions' presence, within a chunk.
	stsReplicas, stsRevisions map[Workload][]Span
}

// instant is what expr holds as of the period's end: one sample a series.
func (r *reader) instant(ctx context.Context, what, expr string) ([]prom.Series, error) {
	s, err := r.h.QueryRange(ctx, expr, r.to, r.to, r.step)
	if err != nil {
		return nil, fmt.Errorf("reading %s from kube-state-metrics: %w", what, err)
	}
	return s, nil
}

func (r *reader) rangeOf(ctx context.Context, what, expr string) ([]prom.Series, error) {
	s, err := r.h.QueryRange(ctx, expr, r.from, r.to, r.step)
	if err != nil {
		return nil, fmt.Errorf("reading %s from kube-state-metrics: %w", what, err)
	}
	return s, nil
}

func (r *reader) over(metric, sel string) string {
	return "last_over_time(" + metric + "{" + sel + "}[" + r.window + "])"
}

func lastValue(s prom.Series) (float64, bool) {
	if len(s.Samples) == 0 {
		return 0, false
	}
	return s.Samples[len(s.Samples)-1].V, true
}

func controller(l map[string]string) bool {
	v, ok := l["owner_is_controller"]
	return !ok || v != "false"
}

// anyHPA says whether any autoscaler's info is in the history at all.
func (r *reader) anyHPA(ctx context.Context) (bool, error) {
	s, err := r.instant(ctx, "autoscalers", "count("+r.over("kube_horizontalpodautoscaler_info", "")+")")
	if err != nil {
		return false, err
	}
	for _, x := range s {
		if len(x.Samples) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// namespaces reads the ReplicaSets, their owners and the autoscalers of a
// chunk of namespaces, and the rollouts and rollbacks they tell of.
func (r *reader) namespaces(ctx context.Context, sel string) error {
	owners, err := r.instant(ctx, "ReplicaSets' owners", r.over("kube_replicaset_owner", sel+`,owner_kind="Deployment"`))
	if err != nil {
		return err
	}
	for _, s := range owners {
		if controller(s.Labels) {
			r.rsOwner[rsKey{s.Labels["namespace"], s.Labels["replicaset"]}] = s.Labels["owner_name"]
		}
	}
	created, err := r.instant(ctx, "ReplicaSets created", r.over("kube_replicaset_created", sel))
	if err != nil {
		return err
	}
	hasCreated := map[rsKey]bool{}
	newRS := map[rsKey]bool{}
	for _, s := range created {
		v, ok := lastValue(s)
		if !ok {
			continue
		}
		k := rsKey{s.Labels["namespace"], s.Labels["replicaset"]}
		hasCreated[k] = true
		if v < float64(r.from.Unix()) || v > float64(r.to.Unix()) || newRS[k] {
			continue
		}
		newRS[k] = true
		if d, ok := r.rsOwner[k]; ok {
			r.out.Changes = append(r.out.Changes, Change{Kind: Rollout, At: time.Unix(int64(v), 0).UTC(),
				Namespace: k.ns, WorkloadKind: KindDeployment, Workload: d, Revision: k.rs})
		}
	}
	// The ReplicaSets' replicas by step: a rollback is a ReplicaSet back to
	// life, 0 to more. One created in the period is counted only once it has
	// been above 0 and then 0, or its first scale up is its rollout's.
	spec, err := r.rangeOf(ctx, "ReplicaSets' replicas", "kube_replicaset_spec_replicas{"+sel+"}")
	if err != nil {
		return err
	}
	for k, ss := range mergeMax(spec, func(l map[string]string) rsKey { return rsKey{l["namespace"], l["replicaset"]} }) {
		d, owned := r.rsOwner[k]
		if !owned {
			continue
		}
		w := Workload{k.ns, KindDeployment, d}
		if hasCreated[k] {
			r.rsCover[w] = MergeSpans(append(r.rsCover[w], r.presence(ss)...))
		}
		sawUp := !newRS[k]
		for i, p := range ss {
			if p.V > 0 {
				if i > 0 && ss[i-1].V == 0 && sawUp {
					r.rollbacks = append(r.rollbacks, Change{Kind: Rollback, At: p.T.UTC(),
						Namespace: k.ns, WorkloadKind: KindDeployment, Workload: d, Revision: k.rs})
				}
				sawUp = true
			}
		}
	}
	hpas, err := r.instant(ctx, "autoscalers", r.over("kube_horizontalpodautoscaler_info", sel))
	if err != nil {
		return err
	}
	for _, s := range hpas {
		r.hpaInfo = true
		l := s.Labels
		r.hpa[Workload{l["namespace"], l["scaletargetref_kind"], l["scaletargetref_name"]}] = true
	}
	return nil
}

// flushRollbacks adds the rollbacks found, but those at a step the
// Deployment itself went from 0 replicas to more, or the step before (its
// ReplicaSet follows it): a scale from zero.
func (r *reader) flushRollbacks() {
	for _, c := range r.rollbacks {
		w := Workload{c.Namespace, c.WorkloadKind, c.Workload}
		if up := r.depUp[w]; up[c.At] || up[c.At.Add(-r.step)] {
			continue
		}
		r.out.Changes = append(r.out.Changes, c)
	}
	r.rollbacks = nil
}

// presence is where a series is, from its first sample to its last and a
// step past it (not past the period's end); a gap of more than a sample
// missing breaks it.
func (r *reader) presence(ss []prom.Sample) []Span {
	var out []Span
	for _, s := range ss {
		end := s.T.Add(r.step)
		if end.After(r.to) {
			end = r.to
		}
		if n := len(out); n > 0 && !s.T.After(out[n-1].To.Add(r.step)) {
			if end.After(out[n-1].To) {
				out[n-1].To = end
			}
			continue
		}
		out = append(out, Span{s.T, end})
	}
	return out
}

// mergeMax puts the series that key says are one together: each step has the
// greatest value any had, and the steps are in order.
func mergeMax[K comparable](series []prom.Series, key func(map[string]string) K) map[K][]prom.Sample {
	byStep := map[K]map[time.Time]float64{}
	for _, s := range series {
		k := key(s.Labels)
		m := byStep[k]
		if m == nil {
			m = map[time.Time]float64{}
			byStep[k] = m
		}
		for _, p := range s.Samples {
			if v, ok := m[p.T]; !ok || p.V > v {
				m[p.T] = p.V
			}
		}
	}
	out := make(map[K][]prom.Sample, len(byStep))
	for k, m := range byStep {
		l := make([]prom.Sample, 0, len(m))
		for t, v := range m {
			l = append(l, prom.Sample{T: t, V: v})
		}
		slices.SortFunc(l, func(a, b prom.Sample) int { return a.T.Compare(b.T) })
		out[k] = l
	}
	return out
}

// dedupe keeps each change once.
func dedupe(cs []Change) []Change {
	type key struct {
		k        Kind
		at       time.Time
		target   string
		revision string
	}
	seen := make(map[key]bool, len(cs))
	out := cs[:0]
	for _, c := range cs {
		k := key{c.Kind, c.At, c.Target(), c.Revision}
		if !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	return out
}

// scales reads the Deployments', StatefulSets' and DaemonSets' series of a
// chunk of namespaces: their scales and rollouts, and where each is known:
// where every series its changes are read from is. The autoscalers are all
// read before, by namespaces.
func (r *reader) scales(ctx context.Context, sel string) error {
	cover := func(w Workload, spans []Span) {
		if len(spans) > 0 {
			r.out.Coverage[w] = MergeSpans(append(r.out.Coverage[w], spans...))
		}
	}
	for _, x := range []struct {
		metric, kind, label string
	}{
		{"kube_deployment_spec_replicas", KindDeployment, "deployment"},
		{"kube_statefulset_replicas", KindStatefulSet, "statefulset"},
	} {
		series, err := r.rangeOf(ctx, x.metric, x.metric+"{"+sel+"}")
		if err != nil {
			return err
		}
		for w, ss := range mergeMax(series, func(l map[string]string) Workload { return Workload{l["namespace"], x.kind, l[x.label]} }) {
			own := r.presence(ss)
			if x.kind == KindDeployment {
				// Known where its ReplicaSets' series are too.
				own = IntersectSpans(own, r.rsCover[w])
			}
			if x.kind == KindDeployment {
				for i := 1; i < len(ss); i++ {
					if ss[i-1].V == 0 && ss[i].V > 0 {
						if r.depUp[w] == nil {
							r.depUp[w] = map[time.Time]bool{}
						}
						r.depUp[w][ss[i].T] = true
					}
				}
			}
			if x.kind == KindStatefulSet {
				// Known where its update revision is read as well.
				r.stsReplicas[w] = own
			} else {
				cover(w, own)
			}
			if r.hpa[w] {
				continue
			}
			for i := 1; i < len(ss); i++ {
				if ss[i].V != ss[i-1].V {
					r.out.Changes = append(r.out.Changes, Change{Kind: Scale, At: ss[i].T.UTC(),
						Namespace: w.Namespace, WorkloadKind: w.Kind, Workload: w.Name})
				}
			}
		}
	}
	// A StatefulSet's update revision: a series for each revision. At each
	// step the revisions seen on any replica; one that wasn't seen at the
	// step before is a rollout.
	series, err := r.rangeOf(ctx, "StatefulSets' revisions", "kube_statefulset_status_update_revision{"+sel+"}")
	if err != nil {
		return err
	}
	revs := map[Workload]map[time.Time]map[string]bool{}
	for _, s := range series {
		w := Workload{s.Labels["namespace"], KindStatefulSet, s.Labels["statefulset"]}
		if revs[w] == nil {
			revs[w] = map[time.Time]map[string]bool{}
		}
		for _, p := range s.Samples {
			if revs[w][p.T] == nil {
				revs[w][p.T] = map[string]bool{}
			}
			revs[w][p.T][s.Labels["revision"]] = true
		}
	}
	for w, byT := range revs {
		ts := make([]time.Time, 0, len(byT))
		samples := make([]prom.Sample, 0, len(byT))
		for t := range byT {
			ts = append(ts, t)
		}
		slices.SortFunc(ts, func(a, b time.Time) int { return a.Compare(b) })
		var prev map[string]bool
		for _, t := range ts {
			samples = append(samples, prom.Sample{T: t})
			if prev != nil {
				var fresh []string
				for rev := range byT[t] {
					if !prev[rev] {
						fresh = append(fresh, rev)
					}
				}
				if len(fresh) > 0 {
					// One change a step.
					slices.Sort(fresh)
					r.out.Changes = append(r.out.Changes, Change{Kind: Rollout, At: t.UTC(),
						Namespace: w.Namespace, WorkloadKind: w.Kind, Workload: w.Name, Revision: fresh[0]})
				}
			}
			prev = byT[t]
		}
		r.stsRevisions[w] = r.presence(samples)
	}
	for w, own := range r.stsReplicas {
		cover(w, IntersectSpans(own, r.stsRevisions[w]))
	}
	r.stsReplicas, r.stsRevisions = map[Workload][]Span{}, map[Workload][]Span{}
	// A DaemonSet's generation going up.
	series, err = r.rangeOf(ctx, "DaemonSets' generations", "kube_daemonset_metadata_generation{"+sel+"}")
	if err != nil {
		return err
	}
	for w, ss := range mergeMax(series, func(l map[string]string) Workload { return Workload{l["namespace"], KindDaemonSet, l["daemonset"]} }) {
		cover(w, r.presence(ss))
		for i := 1; i < len(ss); i++ {
			if ss[i].V > ss[i-1].V {
				r.out.Changes = append(r.out.Changes, Change{Kind: Rollout, At: ss[i].T.UTC(),
					Namespace: w.Namespace, WorkloadKind: w.Kind, Workload: w.Name,
					Revision: strconv.FormatFloat(ss[i].V, 'f', -1, 64)})
			}
		}
	}
	return nil
}

// pods reads which workloads a chunk of pods belong to.
func (r *reader) pods(ctx context.Context, sel string) error {
	series, err := r.instant(ctx, "pods' owners", r.over("kube_pod_owner", sel))
	if err != nil {
		return err
	}
	for _, s := range series {
		l := s.Labels
		if !controller(l) {
			continue
		}
		p := Pod{l["namespace"], l["pod"]}
		switch l["owner_kind"] {
		case "ReplicaSet":
			if d, ok := r.rsOwner[rsKey{p.Namespace, l["owner_name"]}]; ok {
				r.out.Owners[p] = Workload{p.Namespace, KindDeployment, d}
			}
		case KindStatefulSet, KindDaemonSet:
			r.out.Owners[p] = Workload{p.Namespace, l["owner_kind"], l["owner_name"]}
		}
	}
	return nil
}
