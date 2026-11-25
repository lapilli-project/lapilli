package response

import (
	"context"
	"testing"
	"time"
)

func owned(ns, rs, dep string) fake {
	return fake{metric: "kube_replicaset_owner", labels: lb("namespace", ns, "replicaset", rs, "owner_kind", "Deployment", "owner_name", dep, "owner_is_controller", "true"), vals: []float64{1}}
}

func createdAt(ns, rs string, at time.Time) fake {
	return fake{metric: "kube_replicaset_created", labels: lb("namespace", ns, "replicaset", rs), vals: []float64{unix(at)}}
}

func read(t *testing.T, s *standIn, hours int) (*Rollouts, time.Time) {
	t.Helper()
	to := t0.Add(time.Duration(hours) * time.Hour)
	r, err := ReadRollouts(context.Background(), s.client(), t0, to, time.Minute, []string{"shop"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r, to
}

// A firing's target's changes are known only where one span holds all of the
// firing, from its start to its end (now, for one still on).
func TestRespondKnownOverTheWholeFiring(t *testing.T) {
	cs := NewChangeSet(t0, t0.Add(period))
	cs.AddRollouts(&Rollouts{Coverage: map[Workload][]Span{web: {{t0, t0.Add(4 * time.Hour)}}}})
	ev := Evidence{Changes: cs}
	l := map[string]string{"namespace": "shop", "deployment": "web"}
	for _, c := range []struct {
		name       string
		start, end time.Duration
		on         bool
		now        time.Duration
		want       bool
	}{
		{"inside", time.Hour, 2 * time.Hour, false, 5 * time.Hour, true},
		{"started inside, ended past the span", 3 * time.Hour, 6 * time.Hour, false, 7 * time.Hour, false},
		{"still on, now inside", time.Hour, 0, true, 3 * time.Hour, true},
		{"still on, now past the span", time.Hour, 0, true, 5 * time.Hour, false},
	} {
		f := Firing{Rule: "R", Labels: l, Start: t0.Add(c.start), End: t0.Add(c.end), On: c.on}
		if r := ev.Respond(f, 0, t0.Add(c.now)); r.Known != c.want {
			t.Errorf("%s: known %v, want %v", c.name, r.Known, c.want)
		}
	}
}

// Coverage comes from every series the detections use.
func TestReadRolloutsCoverageFromEachSeries(t *testing.T) {
	s := &standIn{t: t, series: []fake{
		// full: its own replicas and its ReplicaSets' from the start.
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "full"), vals: []float64{2}},
		owned("shop", "full-1", "full"), createdAt("shop", "full-1", t0.Add(-time.Hour)),
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "full-1"), vals: []float64{2}},
		// norsm: replicas only.
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "nors"), vals: []float64{2}},
		// rslate: its ReplicaSets' series begin at step 100.
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "rslate"), vals: []float64{2}},
		owned("shop", "rslate-1", "rslate"), createdAt("shop", "rslate-1", t0.Add(-time.Hour)),
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "rslate-1"), vals: []float64{2}, from: 100},
		// no created series for its ReplicaSet.
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "nocreated"), vals: []float64{2}},
		owned("shop", "nocreated-1", "nocreated"),
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "nocreated-1"), vals: []float64{2}},
		// A StatefulSet with replicas and no revision; one whose revision begins at 50; a DaemonSet.
		{metric: "kube_statefulset_replicas", labels: lb("namespace", "shop", "statefulset", "norev"), vals: []float64{3}},
		{metric: "kube_statefulset_replicas", labels: lb("namespace", "shop", "statefulset", "revlate"), vals: []float64{3}},
		{metric: "kube_statefulset_status_update_revision", labels: lb("namespace", "shop", "statefulset", "revlate", "revision", "a"), vals: []float64{1}, from: 50},
		{metric: "kube_statefulset_status_update_revision", labels: lb("namespace", "shop", "statefulset", "onlyrev", "revision", "a"), vals: []float64{1}},
		{metric: "kube_daemonset_metadata_generation", labels: lb("namespace", "shop", "daemonset", "ds"), vals: []float64{1}, from: 20},
		// ended early: its replicas stop at step 200.
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "gone"), vals: repeat(2, 200), stop: true},
		owned("shop", "gone-1", "gone"), createdAt("shop", "gone-1", t0.Add(-time.Hour)),
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "gone-1"), vals: []float64{2}},
	}}
	r, to := read(t, s, 6)
	cov := func(kind, name string) []Span { return r.Coverage[Workload{"shop", kind, name}] }
	if sp := cov(KindDeployment, "full"); len(sp) != 1 || !sp[0].From.Equal(t0) || !sp[0].To.Equal(to) {
		t.Errorf("full: %+v", sp)
	}
	if sp := cov(KindDeployment, "nors"); len(sp) != 0 {
		t.Errorf("a Deployment with no ReplicaSet series is covered: %+v", sp)
	}
	if sp := cov(KindDeployment, "rslate"); len(sp) != 1 || !sp[0].From.Equal(stepAt(100)) {
		t.Errorf("rslate known from its ReplicaSets': %+v", sp)
	}
	if sp := cov(KindDeployment, "nocreated"); len(sp) != 0 {
		t.Errorf("a ReplicaSet with no created series covers: %+v", sp)
	}
	if sp := cov(KindStatefulSet, "norev"); len(sp) != 0 {
		t.Errorf("a StatefulSet with no revision series is covered: %+v", sp)
	}
	if sp := cov(KindStatefulSet, "revlate"); len(sp) != 1 || !sp[0].From.Equal(stepAt(50)) || !sp[0].To.Equal(to) {
		t.Errorf("revlate: %+v", sp)
	}
	if sp := cov(KindStatefulSet, "onlyrev"); len(sp) != 0 {
		t.Errorf("a StatefulSet with no replicas series is covered: %+v", sp)
	}
	if sp := cov(KindDaemonSet, "ds"); len(sp) != 1 || !sp[0].From.Equal(stepAt(20)) {
		t.Errorf("ds: %+v", sp)
	}
	if sp := cov(KindDeployment, "gone"); len(sp) != 1 || !sp[0].To.Before(to) || sp[0].To.Before(stepAt(199)) {
		t.Errorf("gone is known to where its series end: %+v", sp)
	}
}

// Two kube-state-metrics replicas, differing by instance, make one change
// each time, though they see it a step apart.
func TestReadRolloutsDuplicateSeries(t *testing.T) {
	rs := func(inst string, vals []float64) fake {
		return fake{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-old", "instance", inst), vals: vals}
	}
	s := &standIn{t: t, series: []fake{
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "api", "instance", "a"), vals: append(repeat(2, 60), 4)},
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "api", "instance", "b"), vals: append(repeat(2, 61), 4)},
		{metric: "kube_statefulset_replicas", labels: lb("namespace", "shop", "statefulset", "db", "instance", "a"), vals: append(repeat(3, 70), 5)},
		{metric: "kube_statefulset_replicas", labels: lb("namespace", "shop", "statefulset", "db", "instance", "b"), vals: append(repeat(3, 72), 5)},
		{metric: "kube_statefulset_status_update_revision", labels: lb("namespace", "shop", "statefulset", "db", "revision", "aaa", "instance", "a"), vals: repeat(1, 150), stop: true},
		{metric: "kube_statefulset_status_update_revision", labels: lb("namespace", "shop", "statefulset", "db", "revision", "bbb", "instance", "a"), vals: []float64{1}, from: 150},
		{metric: "kube_statefulset_status_update_revision", labels: lb("namespace", "shop", "statefulset", "db", "revision", "aaa", "instance", "b"), vals: repeat(1, 153), stop: true},
		{metric: "kube_statefulset_status_update_revision", labels: lb("namespace", "shop", "statefulset", "db", "revision", "bbb", "instance", "b"), vals: []float64{1}, from: 153},
		{metric: "kube_daemonset_metadata_generation", labels: lb("namespace", "shop", "daemonset", "ds", "instance", "a"), vals: append(repeat(1, 200), 2)},
		{metric: "kube_daemonset_metadata_generation", labels: lb("namespace", "shop", "daemonset", "ds", "instance", "b"), vals: append(repeat(1, 201), 2)},
		owned("shop", "web-new", "web"), owned("shop", "web-old", "web"),
		{metric: "kube_replicaset_owner", labels: lb("namespace", "shop", "replicaset", "web-new", "owner_kind", "Deployment", "owner_name", "web", "owner_is_controller", "true", "instance", "b"), vals: []float64{1}},
		createdAt("shop", "web-new", stepAt(120)),
		{metric: "kube_replicaset_created", labels: lb("namespace", "shop", "replicaset", "web-new", "instance", "b"), vals: []float64{unix(stepAt(120))}},
		createdAt("shop", "web-old", t0.Add(-time.Hour)),
		rs("a", append(zeros(180), 3)), rs("b", append(zeros(181), 3)),
	}}
	r, _ := read(t, s, 6)
	want := map[string]int{"api scale": 1, "db scale": 1, "db rollout": 1, "ds rollout": 1, "web rollout": 1, "web rollback": 1}
	got := map[string]int{}
	for _, c := range r.Changes {
		got[c.Workload+" "+string(c.Kind)]++
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: %d changes, want %d (%v)", k, got[k], n, got)
		}
	}
	if len(r.Changes) != 6 {
		t.Errorf("%d changes, want 6: %+v", len(r.Changes), r.Changes)
	}
}

func TestReadRolloutsScaleFromZeroIsNotARollback(t *testing.T) {
	for _, c := range []struct {
		name      string
		rsStep    int // the ReplicaSet's step up
		hpa       bool
		scales    int
		rollbacks int
	}{
		{"the same step", 100, false, 1, 0},
		{"the ReplicaSet a step after", 101, false, 1, 0},
		{"autoscaled", 100, true, 0, 0},
		{"the Deployment didn't go up then", 140, false, 1, 1},
	} {
		series := []fake{
			{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "web"), vals: append(zeros(100), 3)},
			owned("shop", "web-1", "web"), createdAt("shop", "web-1", t0.Add(-time.Hour)),
			{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-1"), vals: append(zeros(c.rsStep), 3)},
		}
		if c.hpa {
			series = append(series, fake{metric: "kube_horizontalpodautoscaler_info", labels: lb("namespace", "shop", "horizontalpodautoscaler", "h", "scaletargetref_kind", "Deployment", "scaletargetref_name", "web"), vals: []float64{1}})
		}
		r, _ := read(t, &standIn{t: t, series: series}, 6)
		w := Workload{"shop", KindDeployment, "web"}
		if n := len(find(r, Scale, w)); n != c.scales {
			t.Errorf("%s: %d scales, want %d", c.name, n, c.scales)
		}
		if n := len(find(r, Rollback, w)); n != c.rollbacks {
			t.Errorf("%s: %d rollbacks, want %d", c.name, n, c.rollbacks)
		}
	}
}

// A ReplicaSet created in the period and rolled back to later is a rollback;
// its own first scale up is not.
func TestReadRolloutsRollbackToNewReplicaSet(t *testing.T) {
	s := &standIn{t: t, series: []fake{
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "web"), vals: []float64{3}},
		owned("shop", "web-new", "web"), createdAt("shop", "web-new", stepAt(50)),
		// 0 at first (its own scale up at 60 is no rollback), up, down to 0 at 100, back at 150.
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-new"),
			vals: append(append(append(zeros(60), repeat(3, 40)...), zeros(50)...), 3)},
		owned("shop", "web-fresh", "web"), createdAt("shop", "web-fresh", stepAt(70)),
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-fresh"), from: 70, vals: append(zeros(5), 3)},
	}}
	r, _ := read(t, s, 6)
	w := Workload{"shop", KindDeployment, "web"}
	rb := find(r, Rollback, w)
	if len(rb) != 1 || !rb[0].At.Equal(stepAt(150)) || rb[0].Revision != "web-new" {
		t.Errorf("rollbacks: %+v", rb)
	}
	if n := len(find(r, Rollout, w)); n != 2 {
		t.Errorf("%d rollouts, want 2", n)
	}
}
