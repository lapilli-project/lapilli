package response

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// A kube-state-metrics --metric-allowlist that keeps a ReplicaSet's owner and
// created series and drops its spec replicas leaves its Deployment's
// rollbacks unseen: the Deployment is not known.
func TestReadRolloutsAllowlistWithoutSpecReplicas(t *testing.T) {
	s := &standIn{t: t, series: []fake{
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "al"), vals: []float64{2}},
		owned("shop", "al-1", "al"), createdAt("shop", "al-1", t0.Add(-time.Hour)),
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "ok"), vals: []float64{2}},
		owned("shop", "ok-1", "ok"), createdAt("shop", "ok-1", t0.Add(-time.Hour)),
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "ok-1"), vals: []float64{2}},
	}}
	r, _ := read(t, s, 6)
	if sp := r.Coverage[Workload{"shop", KindDeployment, "al"}]; len(sp) != 0 {
		t.Errorf("a Deployment whose ReplicaSets' replicas are not exported is known: %+v", sp)
	}
	if sp := r.Coverage[Workload{"shop", KindDeployment, "ok"}]; len(sp) != 1 {
		t.Errorf("a Deployment with them is not known: %+v", sp)
	}
}

// A ReplicaSet's replicas going from 0 to more where one series ends and its
// twin under another instance begins (kube-state-metrics restarted, its
// instance label changed) is a rollback: the first read puts the series
// together before it tests them.
func TestReadRolloutsRollbackAcrossASeriesBreak(t *testing.T) {
	s := &standIn{t: t, series: []fake{
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "web"), vals: []float64{3}},
		owned("shop", "web-1", "web"), createdAt("shop", "web-1", t0.Add(-48*time.Hour)),
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-1", "instance", "a"), vals: zeros(100), stop: true},
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-1", "instance", "b"), vals: []float64{3}, from: 101},
	}}
	r, _ := read(t, s, 6)
	if c := find(r, Rollback, Workload{"shop", KindDeployment, "web"}); len(c) != 1 || !c[0].At.Equal(stepAt(101)) {
		t.Errorf("rollbacks across a series break: %+v", c)
	}
}

// A namespace's own name goes into an equality matcher as it is: quoted as a
// string, not as a regular expression (a dot is not escaped there).
func TestReadRolloutsEqualityMatcherIsNotRegexpQuoted(t *testing.T) {
	s := &standIn{t: t, series: []fake{
		owned("my.shop", "web-1", "web"), createdAt("my.shop", "web-1", t0.Add(-48*time.Hour)),
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "my.shop", "replicaset", "web-1"), vals: append(zeros(10), 2)},
	}}
	if _, err := ReadRollouts(context.Background(), s.client(), t0, t0.Add(time.Hour), time.Minute, []string{"my.shop"}, nil); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, q := range s.queries("kube_replicaset_spec_replicas{namespace=") {
		if !strings.HasPrefix(q, "kube_replicaset_spec_replicas{") {
			continue
		}
		found = true
		if !strings.Contains(q, `namespace="my.shop"`) {
			t.Errorf("an equality matcher regexp-quoted: %s", q)
		}
	}
	if !found {
		t.Error("the ReplicaSet that came back was not read by step")
	}
}

// capHistory answers the reads of ReplicaSets that all came back, counting the
// samples a read by step would hold without building them.
type capHistory struct {
	n, dup int
	// specDup is how many series each ReplicaSet's spec replicas have (dup
	// is its owner's), limit the client's sample cap (0: the default).
	specDup, limit int
	step           time.Duration
	most           int
	queries        int
}

func (h *capHistory) MaxSamples() int {
	if h.limit > 0 {
		return h.limit
	}
	return prom.DefaultMaxSamples
}

func (h *capHistory) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	var out []prom.Series
	switch {
	case strings.HasPrefix(expr, "last_over_time(kube_replicaset_owner"):
		for i := 0; i < h.n; i++ {
			for d := 0; d < h.dup; d++ {
				out = append(out, prom.Series{Labels: lb("namespace", "big", "replicaset", fmt.Sprint("rs", i), "owner_kind", "Deployment", "owner_name", fmt.Sprint("d", i%7), "owner_is_controller", "true", "instance", fmt.Sprint("i", d)),
					Samples: []prom.Sample{{T: end, V: 1}}})
			}
		}
	case strings.HasPrefix(expr, "(max_over_time("):
		for i := 0; i < h.n; i++ {
			out = append(out, prom.Series{Labels: lb("namespace", "big", "replicaset", fmt.Sprint("rs", i)), Samples: []prom.Sample{{T: end, V: 1}}})
		}
	case strings.HasPrefix(expr, "count by (namespace, replicaset) ("):
		for i := 0; i < h.n && h.specDup > 0; i++ {
			out = append(out, prom.Series{Labels: lb("namespace", "big", "replicaset", fmt.Sprint("rs", i)), Samples: []prom.Sample{{T: end, V: float64(h.specDup)}}})
		}
	case strings.HasPrefix(expr, "kube_replicaset_spec_replicas{namespace="):
		m := rsMatcher.FindStringSubmatch(expr)
		text, _ := strconv.Unquote(m[1])
		points := int(end.Sub(start)/step) + 1
		samples := (strings.Count(text, "|") + 1) * points * max(h.dup, h.specDup)
		h.queries++
		h.most = max(h.most, samples)
	}
	return out, nil
}

// A chunk of ReplicaSets read by step is sized from the sample cap, the
// points in the period and the series each has: at 28 days, 1m and two
// kube-state-metrics replicas, 200 names would hold 16 million samples, past
// the 10 million a query may.
func TestReadRolloutsChunkIsSizedFromTheSampleCap(t *testing.T) {
	h := &capHistory{n: 500, dup: 2}
	to := t0.Add(28 * 24 * time.Hour)
	if _, err := ReadRollouts(context.Background(), h, t0, to, time.Minute, []string{"big"}, nil); err != nil {
		t.Fatal(err)
	}
	if h.queries == 0 || h.most > prom.DefaultMaxSamples {
		t.Errorf("%d queries, the largest holding %d samples, want at most %d", h.queries, h.most, prom.DefaultMaxSamples)
	}
	if h.queries < 6 {
		t.Errorf("%d queries for 500 ReplicaSets at 80,642 samples each, want at least 6", h.queries)
	}
	// A short period still holds 200 names a query.
	h = &capHistory{n: 500, dup: 1}
	if _, err := ReadRollouts(context.Background(), h, t0, t0.Add(2*time.Hour), time.Minute, []string{"big"}, nil); err != nil {
		t.Fatal(err)
	}
	if h.queries != 3 {
		t.Errorf("%d queries for 500 names over 2 hours, want 3", h.queries)
	}
}
