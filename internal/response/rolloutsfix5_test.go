package response

import (
	"context"
	"regexp"
	"testing"
	"time"
)

var subqueryRe = regexp.MustCompile(`\[[^\]]*:[^\]]*\]`)

// The test of the ReplicaSets that came back is made of raw ranges, no
// subquery: a subquery holds its whole matrix inside Prometheus's
// --query.max-samples (about 2 x the ReplicaSets x the points), where the raw
// range holds none; and one that had two series over the period (a break
// between them) is found as well, by its count of series.
func TestReadRolloutsAsksNoSubquery(t *testing.T) {
	s := &standIn{t: t, series: []fake{
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "web"), vals: []float64{3}},
		owned("shop", "web-1", "web"), createdAt("shop", "web-1", t0.Add(-48*time.Hour)),
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-1", "instance", "a"), vals: zeros(100), stop: true},
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-1", "instance", "b"), vals: []float64{3}, from: 101},
	}}
	r, _ := read(t, s, 6)
	if c := find(r, Rollback, Workload{"shop", KindDeployment, "web"}); len(c) != 1 {
		t.Errorf("a rollback across a break of two series: %+v", c)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.asked {
		if subqueryRe.MatchString(q) {
			t.Errorf("a subquery asked: %s", q)
		}
	}
}

// A chunk of ReplicaSets read by step is sized from the client's own sample
// cap (--max-samples), and from the most series any of a ReplicaSet's series
// (owner, spec replicas) has.
func TestReadRolloutsChunkFollowsTheClientsCapAndSpecSeries(t *testing.T) {
	to := t0.Add(28 * 24 * time.Hour) // 40,321 points at 1m
	for _, c := range []struct {
		name  string
		h     *capHistory
		limit int
	}{
		{"a cap a tenth of the default", &capHistory{n: 400, dup: 1, limit: 1000000}, 1000000},
		{"three series of spec replicas, one of owner", &capHistory{n: 400, dup: 1, specDup: 3}, 10000000},
	} {
		if _, err := ReadRollouts(context.Background(), c.h, t0, to, time.Minute, []string{"big"}, nil); err != nil {
			t.Fatal(err)
		}
		if c.h.most > c.limit {
			t.Errorf("%s: a query held %d samples, past the cap of %d", c.name, c.h.most, c.limit)
		}
	}
	// And the cap's effect is seen: the smaller cap takes more queries.
	small, big := &capHistory{n: 400, dup: 1, limit: 1000000}, &capHistory{n: 400, dup: 1}
	for _, h := range []*capHistory{small, big} {
		if _, err := ReadRollouts(context.Background(), h, t0, to, time.Minute, []string{"big"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if small.queries <= big.queries {
		t.Errorf("%d queries under a cap a tenth the size, %d under the default: want more", small.queries, big.queries)
	}
}
