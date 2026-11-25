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
//   - its rollback: a ReplicaSet created before the period whose spec replicas
//     go from 0 to more, at the step they do; those that were both 0 and above
//     it in the period are found in one read, and only they are read by step;
//   - a StatefulSet's rollout: its update revision changing; a DaemonSet's:
//     its generation going up; at the first step it shows;
//   - a scale of a Deployment or StatefulSet no HorizontalPodAutoscaler
//     targets: its replicas changing.
//
// The namespaces the pods are in are read too. Names go into a regular
// expression quoted, MaxNames to a query. What a ReplicaSet, a pod, or an
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
		rsOwner: map[rsKey]string{}, hpa: map[Workload]bool{}}

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
	created, err := r.instant(ctx, "ReplicaSets created", r.over("kube_replicaset_created", sel)+" >= "+strconv.FormatInt(r.from.Unix(), 10))
	if err != nil {
		return err
	}
	newRS := map[rsKey]bool{}
	for _, s := range created {
		v, ok := lastValue(s)
		if !ok || v > float64(r.to.Unix()) {
			continue
		}
		k := rsKey{s.Labels["namespace"], s.Labels["replicaset"]}
		newRS[k] = true
		if d, ok := r.rsOwner[k]; ok {
			r.out.Changes = append(r.out.Changes, Change{Kind: Rollout, At: time.Unix(int64(v), 0).UTC(),
				Namespace: k.ns, WorkloadKind: KindDeployment, Workload: d, Revision: k.rs})
		}
	}
	// Created before the period: back to life by a rollback. Those whose
	// replicas were both 0 and above it within the period are found in one
	// read; only they are read step by step.
	both := "kube_replicaset_spec_replicas{" + sel + "}[" + r.window + "]"
	cand, err := r.instant(ctx, "ReplicaSets that came back", "(max_over_time("+both+") > 0) and (min_over_time("+both+") == 0)")
	if err != nil {
		return err
	}
	byNS := map[string][]string{}
	for _, s := range cand {
		k := rsKey{s.Labels["namespace"], s.Labels["replicaset"]}
		if _, ok := r.rsOwner[k]; ok && !newRS[k] {
			byNS[k.ns] = append(byNS[k.ns], k.rs)
		}
	}
	for _, ns := range slices.Sorted(mapKeys(byNS)) {
		names := byNS[ns]
		slices.Sort(names)
		for _, part := range chunks(names) {
			spec, err := r.rangeOf(ctx, "ReplicaSets' replicas", "kube_replicaset_spec_replicas{namespace="+strconv.Quote(regexp.QuoteMeta(ns))+",replicaset=~"+matcher(part)+"}")
			if err != nil {
				return err
			}
			for _, s := range spec {
				k := rsKey{s.Labels["namespace"], s.Labels["replicaset"]}
				for i := 1; i < len(s.Samples); i++ {
					if s.Samples[i-1].V == 0 && s.Samples[i].V > 0 {
						r.out.Changes = append(r.out.Changes, Change{Kind: Rollback, At: s.Samples[i].T.UTC(),
							Namespace: k.ns, WorkloadKind: KindDeployment, Workload: r.rsOwner[k], Revision: k.rs})
					}
				}
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

// scales reads the Deployments', StatefulSets' and DaemonSets' series of a
// chunk of namespaces: their scales and rollouts, and where each is known
// from. The autoscalers are all read before, by namespaces.
func (r *reader) scales(ctx context.Context, sel string) error {
	known := func(w Workload, first time.Time) {
		r.out.Coverage[w] = append(r.out.Coverage[w], Span{From: first, To: r.to})
	}
	// The workloads' series, merged by workload and put in order.
	merged := func(series []prom.Series, kind, label string) map[Workload][]prom.Sample {
		m := map[Workload][]prom.Sample{}
		for _, s := range series {
			w := Workload{s.Labels["namespace"], kind, s.Labels[label]}
			m[w] = append(m[w], s.Samples...)
		}
		for w := range m {
			slices.SortStableFunc(m[w], func(a, b prom.Sample) int { return a.T.Compare(b.T) })
		}
		return m
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
		for w, ss := range merged(series, x.kind, x.label) {
			if len(ss) == 0 {
				continue
			}
			known(w, ss[0].T)
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
	// A StatefulSet's update revision: a series for each revision.
	series, err := r.rangeOf(ctx, "StatefulSets' revisions", "kube_statefulset_status_update_revision{"+sel+"}")
	if err != nil {
		return err
	}
	type at struct {
		t   time.Time
		rev string
	}
	revs := map[Workload][]at{}
	for _, s := range series {
		w := Workload{s.Labels["namespace"], KindStatefulSet, s.Labels["statefulset"]}
		for _, p := range s.Samples {
			revs[w] = append(revs[w], at{p.T, s.Labels["revision"]})
		}
	}
	for w, l := range revs {
		slices.SortStableFunc(l, func(a, b at) int { return a.t.Compare(b.t) })
		cur := ""
		for i := 0; i < len(l); {
			// Of the revisions at one step, the one that isn't the last
			// seen: the new one, as the old series ends as it begins.
			j := i
			pick := l[i].rev
			for ; j < len(l) && l[j].t.Equal(l[i].t); j++ {
				if l[j].rev != cur {
					pick = l[j].rev
				}
			}
			if cur != "" && pick != cur {
				r.out.Changes = append(r.out.Changes, Change{Kind: Rollout, At: l[i].t.UTC(),
					Namespace: w.Namespace, WorkloadKind: w.Kind, Workload: w.Name, Revision: pick})
			}
			cur = pick
			i = j
		}
	}
	// A DaemonSet's generation going up.
	series, err = r.rangeOf(ctx, "DaemonSets' generations", "kube_daemonset_metadata_generation{"+sel+"}")
	if err != nil {
		return err
	}
	for w, ss := range merged(series, KindDaemonSet, "daemonset") {
		if len(ss) == 0 {
			continue
		}
		known(w, ss[0].T)
		for i := 1; i < len(ss); i++ {
			if ss[i].V > ss[i-1].V {
				r.out.Changes = append(r.out.Changes, Change{Kind: Rollout, At: ss[i].T.UTC(),
					Namespace: w.Namespace, WorkloadKind: w.Kind, Workload: w.Name,
					Revision: strconv.FormatFloat(ss[i].V, 'f', -1, 64)})
			}
		}
	}
	// A StatefulSet's rollouts are known from where its replicas begin
	// (above).
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
