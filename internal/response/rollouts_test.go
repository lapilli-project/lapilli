package response

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// fake is a kube-state-metrics series the stand-in Prometheus holds: for an
// instant read (last_over_time) its value is the last of vals; for a range,
// vals are the values from the period's start, a step apart, the last
// repeated to the end unless it stops.
type fake struct {
	metric string
	labels map[string]string
	vals   []float64
	// from is the step the series begins at (0 for the period's start).
	from int
	// stop ends the series with its last value.
	stop bool
}

type standIn struct {
	t      *testing.T
	series []fake
	mu     sync.Mutex
	asked  []string
	// ranged are the ReplicaSets whose replicas were read by step.
	ranged []string
}

var (
	nsMatcher  = regexp.MustCompile(`namespace=~("(?:[^"\\]|\\.)*")`)
	podMatcher = regexp.MustCompile(`pod=~("(?:[^"\\]|\\.)*")`)
	rsMatcher  = regexp.MustCompile(`replicaset=~("(?:[^"\\\\]|\\\\.)*")`)
	geMatcher  = regexp.MustCompile(` >= (\d+)$`)
)

func (s *standIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	expr := r.Form.Get("query")
	s.mu.Lock()
	s.asked = append(s.asked, expr)
	s.mu.Unlock()
	start, _ := strconv.ParseInt(r.Form.Get("start"), 10, 64)
	end, _ := strconv.ParseInt(r.Form.Get("end"), 10, 64)
	step, _ := strconv.ParseFloat(r.Form.Get("step"), 64)

	filter := func(re *regexp.Regexp, label string) func(map[string]string) bool {
		m := re.FindStringSubmatch(expr)
		if m == nil {
			return func(map[string]string) bool { return true }
		}
		text, err := strconv.Unquote(m[1])
		if err != nil {
			s.t.Errorf("not a quoted string: %s", m[1])
		}
		rx := regexp.MustCompile("^(?:" + text + ")$")
		return func(l map[string]string) bool { return rx.MatchString(l[label]) }
	}
	inNS, inPod, inRS := filter(nsMatcher, "namespace"), filter(podMatcher, "pod"), filter(rsMatcher, "replicaset")
	cameBack := strings.HasPrefix(expr, "(max_over_time(")
	instant := strings.HasPrefix(expr, "last_over_time(") || strings.HasPrefix(expr, "count(") || cameBack
	min := -1.0
	if m := geMatcher.FindStringSubmatch(expr); m != nil {
		min, _ = strconv.ParseFloat(m[1], 64)
	}
	var out []string
	count := 0
	for _, f := range s.series {
		if !strings.Contains(expr, f.metric) || !inNS(f.labels) || !inPod(f.labels) || !inRS(f.labels) {
			continue
		}
		if len(f.vals) == 0 {
			continue
		}
		if !instant && f.metric == "kube_replicaset_spec_replicas" {
			s.mu.Lock()
			s.ranged = append(s.ranged, f.labels["replicaset"])
			s.mu.Unlock()
		}
		if cameBack {
			lo, hi := f.vals[0], f.vals[0]
			for _, v := range f.vals {
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
			if !(lo == 0 && hi > 0) {
				continue
			}
		}
		var vals []string
		if instant {
			v := f.vals[len(f.vals)-1]
			if cameBack {
				v = 1
			}
			if v < min {
				continue
			}
			vals = append(vals, fmt.Sprintf("[%d,%q]", end, strconv.FormatFloat(v, 'f', -1, 64)))
			count++
		} else {
			for i := f.from; ; i++ {
				at := start + int64(float64(i)*step)
				if at > end {
					break
				}
				if f.stop && i-f.from >= len(f.vals) {
					break
				}
				v := f.vals[len(f.vals)-1]
				if i-f.from < len(f.vals) {
					v = f.vals[i-f.from]
				}
				vals = append(vals, fmt.Sprintf("[%d,%q]", at, strconv.FormatFloat(v, 'f', -1, 64)))
			}
		}
		var ls []string
		for _, k := range sortedKeys(f.labels) {
			ls = append(ls, fmt.Sprintf("%q:%q", k, f.labels[k]))
		}
		out = append(out, fmt.Sprintf(`{"metric":{%s},"values":[%s]}`, strings.Join(ls, ","), strings.Join(vals, ",")))
	}
	if strings.HasPrefix(expr, "count(") {
		out = nil
		if count > 0 {
			out = []string{fmt.Sprintf(`{"metric":{},"values":[[%d,"%d"]]}`, end, count)}
		}
	}
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[%s]}}`, strings.Join(out, ","))
}

func sortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func (s *standIn) client() History {
	srv := httptest.NewServer(s)
	s.t.Cleanup(srv.Close)
	return prom.New(srv.URL, "test")
}

func (s *standIn) queries(metric string) []string {
	var out []string
	for _, q := range s.asked {
		if strings.Contains(q, metric) {
			out = append(out, q)
		}
	}
	return out
}

func lb(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// steps is a step number as the time of that step.
func stepAt(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

func unix(t time.Time) float64 { return float64(t.Unix()) }

func find(r *Rollouts, k Kind, w Workload) []Change {
	var out []Change
	for _, c := range r.Changes {
		if c.Kind == k && c.Namespace == w.Namespace && c.WorkloadKind == w.Kind && c.Workload == w.Name {
			out = append(out, c)
		}
	}
	return out
}

func TestReadRollouts(t *testing.T) {
	to := t0.Add(6 * time.Hour)
	created := stepAt(120)
	s := &standIn{t: t, series: []fake{
		// A rollout of web at step 120, which owns RS web-7d4; web-6c1 is older
		// and comes back at step 180 (a rollback).
		{metric: "kube_replicaset_owner", labels: lb("namespace", "shop", "replicaset", "web-7d4", "owner_kind", "Deployment", "owner_name", "web", "owner_is_controller", "true"), vals: []float64{1}},
		{metric: "kube_replicaset_owner", labels: lb("namespace", "shop", "replicaset", "web-6c1", "owner_kind", "Deployment", "owner_name", "web", "owner_is_controller", "true"), vals: []float64{1}},
		{metric: "kube_replicaset_created", labels: lb("namespace", "shop", "replicaset", "web-7d4"), vals: []float64{unix(created)}},
		{metric: "kube_replicaset_created", labels: lb("namespace", "shop", "replicaset", "web-6c1"), vals: []float64{unix(t0.Add(-5 * 24 * time.Hour))}},
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-6c1"), vals: append(zeros(181), 3)},
		// One that stayed at 0, and one that stayed above 0, are never read by step.
		{metric: "kube_replicaset_owner", labels: lb("namespace", "shop", "replicaset", "web-old0", "owner_kind", "Deployment", "owner_name", "web", "owner_is_controller", "true"), vals: []float64{1}},
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-old0"), vals: []float64{0}},
		{metric: "kube_replicaset_owner", labels: lb("namespace", "shop", "replicaset", "web-up", "owner_kind", "Deployment", "owner_name", "web", "owner_is_controller", "true"), vals: []float64{1}},
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-up"), vals: []float64{2}},
		// A new ReplicaSet's replicas go 0 to more: no rollback.
		{metric: "kube_replicaset_spec_replicas", labels: lb("namespace", "shop", "replicaset", "web-7d4"), vals: append(zeros(121), 3)},
		// Another namespace, whose name a dot would match: never asked about.
		{metric: "kube_replicaset_owner", labels: lb("namespace", "shopxyz", "replicaset", "z-1", "owner_kind", "Deployment", "owner_name", "z", "owner_is_controller", "true"), vals: []float64{1}},
		{metric: "kube_replicaset_created", labels: lb("namespace", "shopxyz", "replicaset", "z-1"), vals: []float64{unix(created)}},
		// Scales: api 2 to 4 at step 60; hpa-d is autoscaled; db (StatefulSet) 3 to 5 at step 90.
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "api"), vals: append(repeat(2, 60), 4)},
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "hpa-d"), vals: append(repeat(2, 70), 6)},
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "late"), vals: []float64{2}, from: 240},
		{metric: "kube_statefulset_replicas", labels: lb("namespace", "shop", "statefulset", "db"), vals: append(repeat(3, 90), 5)},
		{metric: "kube_horizontalpodautoscaler_info", labels: lb("namespace", "shop", "horizontalpodautoscaler", "h", "scaletargetref_kind", "Deployment", "scaletargetref_name", "hpa-d"), vals: []float64{1}},
		// db's revision changes at step 150; the old series ends as the new begins.
		{metric: "kube_statefulset_status_update_revision", labels: lb("namespace", "shop", "statefulset", "db", "revision", "db-aaa"), vals: repeat(1, 150), stop: true},
		{metric: "kube_statefulset_status_update_revision", labels: lb("namespace", "shop", "statefulset", "db", "revision", "db-bbb"), vals: []float64{1}, from: 150},
		{metric: "kube_daemonset_metadata_generation", labels: lb("namespace", "shop", "daemonset", "agent"), vals: append(repeat(1, 200), 2)},
		{metric: "kube_pod_owner", labels: lb("namespace", "shop", "pod", "web-7d4-abc", "owner_kind", "ReplicaSet", "owner_name", "web-7d4", "owner_is_controller", "true", "uid", "u1"), vals: []float64{1}},
		{metric: "kube_pod_owner", labels: lb("namespace", "shop", "pod", "db-0", "owner_kind", "StatefulSet", "owner_name", "db", "owner_is_controller", "true", "uid", "u2"), vals: []float64{1}},
		{metric: "kube_pod_owner", labels: lb("namespace", "shop", "pod", "other-1", "owner_kind", "Job", "owner_name", "j", "owner_is_controller", "true", "uid", "u3"), vals: []float64{1}},
		{metric: "kube_pod_owner", labels: lb("namespace", "shop", "pod", "unasked", "owner_kind", "StatefulSet", "owner_name", "db", "owner_is_controller", "true", "uid", "u4"), vals: []float64{1}},
	}}
	pods := []Pod{{"shop", "web-7d4-abc"}, {"shop", "db-0"}, {"shop", "other-1"}}
	r, err := ReadRollouts(context.Background(), s.client(), t0, to, time.Minute, []string{"shop"}, pods)
	if err != nil {
		t.Fatal(err)
	}
	web := Workload{"shop", KindDeployment, "web"}
	if !slices.Equal(s.ranged, []string{"web-6c1"}) {
		t.Errorf("ReplicaSets read by step: %v, want web-6c1 alone", s.ranged)
	}
	if c := find(r, Rollout, web); len(c) != 1 || !c[0].At.Equal(created) || c[0].Revision != "web-7d4" {
		t.Errorf("web's rollout at its creation second: %+v", c)
	}
	if c := find(r, Rollback, web); len(c) != 1 || !c[0].At.Equal(stepAt(181)) || c[0].Revision != "web-6c1" {
		t.Errorf("web's rollback at the first step it comes back: %+v", c)
	}
	if n := len(find(r, Rollout, Workload{"shopxyz", KindDeployment, "z"})); n != 0 {
		t.Error("a namespace a dot would match was read")
	}
	if c := find(r, Scale, Workload{"shop", KindDeployment, "api"}); len(c) != 1 || !c[0].At.Equal(stepAt(60)) {
		t.Errorf("api's scale: %+v", c)
	}
	if c := find(r, Scale, Workload{"shop", KindDeployment, "hpa-d"}); len(c) != 0 {
		t.Errorf("an autoscaled Deployment's scale was counted: %+v", c)
	}
	if c := find(r, Scale, Workload{"shop", KindStatefulSet, "db"}); len(c) != 1 || !c[0].At.Equal(stepAt(90)) {
		t.Errorf("db's scale: %+v", c)
	}
	db := Workload{"shop", KindStatefulSet, "db"}
	if c := find(r, Rollout, db); len(c) != 1 || !c[0].At.Equal(stepAt(150)) || c[0].Revision != "db-bbb" {
		t.Errorf("db's rollout at the first step its new revision shows: %+v", c)
	}
	if c := find(r, Rollout, Workload{"shop", KindDaemonSet, "agent"}); len(c) != 1 || !c[0].At.Equal(stepAt(200)) || c[0].Revision != "2" {
		t.Errorf("agent's rollout: %+v", c)
	}
	if !r.AutoscalersKnown {
		t.Error("autoscalers' info was there and wasn't found")
	}
	if len(r.Changes) != 6 {
		t.Errorf("%d changes, want 6: %+v", len(r.Changes), r.Changes)
	}
	if !slices.IsSortedFunc(r.Changes, func(a, b Change) int { return a.At.Compare(b.At) }) {
		t.Error("changes aren't in order of time")
	}
	// Coverage begins where a workload's series do.
	for w, from := range map[Workload]time.Time{
		{"shop", KindDeployment, "api"}:  t0,
		{"shop", KindDeployment, "late"}: stepAt(240),
		db:                               t0,
		{"shop", KindDaemonSet, "agent"}: t0,
	} {
		sp := r.Coverage[w]
		if len(sp) != 1 || !sp[0].From.Equal(from) || !sp[0].To.Equal(to) {
			t.Errorf("%v coverage %+v, want from %v to %v", w, sp, from, to)
		}
	}
	if _, ok := r.Coverage[Workload{"shop", KindDeployment, "none"}]; ok {
		t.Error("coverage of a workload that isn't there")
	}
	// Pod owners: a Deployment's through its ReplicaSet, a StatefulSet's
	// direct, a Job's none; a pod not named is not read.
	if got := r.Owners[Pod{"shop", "web-7d4-abc"}]; got != web {
		t.Errorf("pod owner %+v", got)
	}
	if got := r.Owners[Pod{"shop", "db-0"}]; got != db {
		t.Errorf("pod owner %+v", got)
	}
	if len(r.Owners) != 2 {
		t.Errorf("owners %+v", r.Owners)
	}

	// Through a ChangeSet, as a firing finds them.
	cs := NewChangeSet(t0, to)
	cs.AddRollouts(r)
	if !cs.Known(lb("namespace", "shop", "pod", "db-0"), nil, stepAt(5)) || cs.Known(lb("namespace", "shop", "deployment", "late"), nil, stepAt(239)) {
		t.Error("coverage as the set sees it")
	}
	got := cs.Matching(lb("namespace", "shop", "pod", "web-7d4-abc"), nil, t0, to)
	if len(got) != 2 {
		t.Errorf("a pod's workload's changes: %+v", got)
	}
}

func zeros(n int) []float64 { return repeat(0, n) }

func repeat(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestReadRolloutsNoAutoscalerInfo(t *testing.T) {
	s := &standIn{t: t, series: []fake{
		{metric: "kube_deployment_spec_replicas", labels: lb("namespace", "shop", "deployment", "hpa-d"), vals: append(repeat(2, 70), 6)},
	}}
	r, err := ReadRollouts(context.Background(), s.client(), t0, t0.Add(2*time.Hour), time.Minute, []string{"shop"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.AutoscalersKnown {
		t.Error("autoscalers known with none in the history")
	}
	if c := find(r, Scale, Workload{"shop", KindDeployment, "hpa-d"}); len(c) != 1 {
		t.Errorf("without info every scale is counted: %+v", c)
	}
	// An autoscaler in another namespace says the metric is exported: the
	// scales of these are still all there is, and it is known.
	s.series = append(s.series, fake{metric: "kube_horizontalpodautoscaler_info", labels: lb("namespace", "elsewhere", "horizontalpodautoscaler", "h", "scaletargetref_kind", "Deployment", "scaletargetref_name", "x"), vals: []float64{1}})
	r, err = ReadRollouts(context.Background(), s.client(), t0, t0.Add(2*time.Hour), time.Minute, []string{"shop"}, nil)
	if err != nil || !r.AutoscalersKnown {
		t.Errorf("known: %v %v", r.AutoscalersKnown, err)
	}
}

func TestReadRolloutsNothingToRead(t *testing.T) {
	s := &standIn{t: t}
	r, err := ReadRollouts(context.Background(), s.client(), t0, t0.Add(time.Hour), time.Minute, nil, nil)
	if err != nil || len(r.Changes) != 0 || len(s.asked) != 0 {
		t.Errorf("%+v %v %v", r, err, s.asked)
	}
	if _, err := ReadRollouts(context.Background(), s.client(), t0, t0.Add(time.Hour), 0, []string{"a"}, nil); err == nil {
		t.Error("no step was taken")
	}
}

// Names go into the expression quoted, 200 to a query.
func TestReadRolloutsChunks(t *testing.T) {
	var nss []string
	var pods []Pod
	for i := 0; i < 450; i++ {
		nss = append(nss, fmt.Sprintf("ns.%d", i))
	}
	nss = append(nss, "shop") // 451
	for i := 0; i < 250; i++ {
		pods = append(pods, Pod{"shop", fmt.Sprintf("p-%d.x", i)})
	}
	s := &standIn{t: t, series: []fake{
		{metric: "kube_replicaset_owner", labels: lb("namespace", "ns.449", "replicaset", "r1", "owner_kind", "Deployment", "owner_name", "d", "owner_is_controller", "true"), vals: []float64{1}},
		{metric: "kube_replicaset_created", labels: lb("namespace", "ns.449", "replicaset", "r1"), vals: []float64{unix(stepAt(10))}},
		// "." quoted: ns-449 is not ns.449.
		{metric: "kube_replicaset_owner", labels: lb("namespace", "ns-449", "replicaset", "r2", "owner_kind", "Deployment", "owner_name", "e", "owner_is_controller", "true"), vals: []float64{1}},
		{metric: "kube_replicaset_created", labels: lb("namespace", "ns-449", "replicaset", "r2"), vals: []float64{unix(stepAt(10))}},
		{metric: "kube_pod_owner", labels: lb("namespace", "shop", "pod", "p-249.x", "owner_kind", "StatefulSet", "owner_name", "db", "owner_is_controller", "true"), vals: []float64{1}},
		{metric: "kube_pod_owner", labels: lb("namespace", "shop", "pod", "p-249-x", "owner_kind", "StatefulSet", "owner_name", "db", "owner_is_controller", "true"), vals: []float64{1}},
	}}
	r, err := ReadRollouts(context.Background(), s.client(), t0, t0.Add(time.Hour), time.Minute, nss, pods)
	if err != nil {
		t.Fatal(err)
	}
	// 451 namespaces in 3 queries a metric; 250 pods in 2.
	for _, m := range []string{"kube_replicaset_owner", "kube_replicaset_created", "kube_replicaset_spec_replicas", "kube_deployment_spec_replicas", "kube_statefulset_replicas", "kube_statefulset_status_update_revision", "kube_daemonset_metadata_generation"} {
		qs := s.queries(m)
		if m == "kube_replicaset_owner" {
			qs = qs[:3] // the pods' owners are asked as kube_pod_owner
		}
		if len(qs) != 3 {
			t.Errorf("%s asked %d times, want 3", m, len(qs))
		}
		for _, q := range qs {
			m := nsMatcher.FindStringSubmatch(q)
			text, _ := strconv.Unquote(m[1])
			if n := strings.Count(text, "|") + 1; n > MaxNames {
				t.Errorf("%d names in a query", n)
			}
			if strings.Contains(text, "ns.4") {
				t.Errorf("a dot not quoted: %s", text[:60])
			}
		}
	}
	if qs := s.queries("kube_pod_owner"); len(qs) != 2 {
		t.Errorf("pods asked %d times, want 2", len(qs))
	} else {
		for _, q := range qs {
			m := podMatcher.FindStringSubmatch(q)
			text, _ := strconv.Unquote(m[1])
			if n := strings.Count(text, "|") + 1; n > MaxNames || !strings.Contains(text, `\.x`) {
				t.Errorf("%d pods in a query: %s", n, text[:40])
			}
		}
	}
	if c := find(r, Rollout, Workload{"ns.449", KindDeployment, "d"}); len(c) != 1 {
		t.Errorf("the last chunk's rollout: %+v", c)
	}
	if len(find(r, Rollout, Workload{"ns-449", KindDeployment, "e"})) != 0 {
		t.Error("ns-449 matched ns.449's expression")
	}
	if len(r.Owners) != 1 || r.Owners[Pod{"shop", "p-249.x"}].Name != "db" {
		t.Errorf("owners %+v", r.Owners)
	}
}

func TestReadRolloutsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	}))
	defer srv.Close()
	_, err := ReadRollouts(context.Background(), prom.New(srv.URL, "test"), t0, t0.Add(time.Hour), time.Minute, []string{"a"}, nil)
	if err == nil || !strings.Contains(err.Error(), "kube-state-metrics") || !strings.Contains(err.Error(), "parse error") {
		t.Errorf("%v", err)
	}
}

func TestMatcherQuotes(t *testing.T) {
	got := matcher([]string{`a.b`, `c"d`, `e\f`, `g|h`})
	text, err := strconv.Unquote(got)
	if err != nil {
		t.Fatal(err)
	}
	rx := regexp.MustCompile("^(?:" + text + ")$")
	for _, n := range []string{`a.b`, `c"d`, `e\f`, `g|h`} {
		if !rx.MatchString(n) {
			t.Errorf("%q not matched by %s", n, got)
		}
	}
	for _, n := range []string{`axb`, `g`, `h`} {
		if rx.MatchString(n) {
			t.Errorf("%q matched by %s", n, got)
		}
	}
}
