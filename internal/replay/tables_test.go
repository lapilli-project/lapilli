package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func decode(t *testing.T, s string) obj {
	t.Helper()
	var o obj
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return o
}

// What a table is asked for is what kubectl prints, so these are the lines an agent reads. Each row
// here is a state a pod is found in during an incident, and the STATUS a cluster prints for it.
func TestAPodsRowIsWhatAClusterPrints(t *testing.T) {
	const meta = `"metadata":{"name":"api-1","namespace":"shop","creationTimestamp":"2026-10-07T09:04:26Z"}`
	for name, c := range map[string]struct {
		pod  string
		want []any
	}{
		"running": {`{` + meta + `,"spec":{"nodeName":"worker2","containers":[{"name":"app"},{"name":"proxy"}]},
			"status":{"phase":"Running","podIPs":[{"ip":"10.244.1.3"}],"containerStatuses":[{"ready":true,"restartCount":0,"state":{"running":{}}},{"ready":false,"restartCount":0,"state":{"running":{}}}]}}`,
			[]any{"api-1", "1/2", "Running", "0", "13m", "10.244.1.3", "worker2", "<none>", "<none>"}},
		"crash loop": {`{` + meta + `,"spec":{"nodeName":"worker2","containers":[{"name":"app"}]},
			"status":{"phase":"Running","containerStatuses":[{"ready":false,"restartCount":6,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":1,"reason":"Error","finishedAt":"2026-10-07T09:15:06Z"}}}]}}`,
			[]any{"api-1", "0/1", "CrashLoopBackOff", "6 (2m20s ago)", "13m", "<none>", "worker2", "<none>", "<none>"}},
		"killed for memory": {`{` + meta + `,"spec":{"containers":[{"name":"app"}]},
			"status":{"phase":"Running","containerStatuses":[{"ready":false,"restartCount":1,"state":{"terminated":{"exitCode":137,"reason":"OOMKilled"}}}]}}`,
			[]any{"api-1", "0/1", "OOMKilled", "1", "13m", "<none>", "<none>", "<none>", "<none>"}},
		"exited with no reason given": {`{` + meta + `,"spec":{"containers":[{"name":"app"}]},
			"status":{"phase":"Failed","containerStatuses":[{"ready":false,"restartCount":0,"state":{"terminated":{"exitCode":3}}}]}}`,
			[]any{"api-1", "0/1", "ExitCode:3", "0", "13m", "<none>", "<none>", "<none>", "<none>"}},
		"not scheduled": {`{` + meta + `,"spec":{"containers":[{"name":"app"}]},"status":{"phase":"Pending","nominatedNodeName":"worker"}}`,
			[]any{"api-1", "0/1", "Pending", "0", "13m", "<none>", "<none>", "worker", "<none>"}},
		"an init container failing": {`{` + meta + `,"spec":{"initContainers":[{"name":"migrate"},{"name":"warm"}],"containers":[{"name":"app"}]},
			"status":{"phase":"Pending","initContainerStatuses":[{"name":"migrate","restartCount":2,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":1,"finishedAt":"2026-10-07T09:17:00Z"}}}],
			"containerStatuses":[{"ready":false,"restartCount":0,"state":{"waiting":{"reason":"PodInitializing"}}}]}}`,
			[]any{"api-1", "0/1", "Init:CrashLoopBackOff", "2 (26s ago)", "13m", "<none>", "<none>", "<none>", "<none>"}},
		"init containers still running": {`{` + meta + `,"spec":{"initContainers":[{"name":"migrate"},{"name":"warm"}],"containers":[{"name":"app"}]},
			"status":{"phase":"Pending","initContainerStatuses":[{"name":"migrate","state":{"terminated":{"exitCode":0}}},{"name":"warm","state":{"running":{}}}]}}`,
			[]any{"api-1", "0/1", "Init:1/2", "0", "13m", "<none>", "<none>", "<none>", "<none>"}},
		"a sidecar counts as a container": {`{` + meta + `,"spec":{"initContainers":[{"name":"mesh","restartPolicy":"Always"}],"containers":[{"name":"app"}]},
			"status":{"phase":"Running","conditions":[{"type":"Initialized","status":"True"}],"initContainerStatuses":[{"name":"mesh","started":true,"ready":true,"restartCount":1,"state":{"running":{}}}],
			"containerStatuses":[{"ready":true,"restartCount":0,"state":{"running":{}}}]}}`,
			[]any{"api-1", "2/2", "Running", "1", "13m", "<none>", "<none>", "<none>", "<none>"}},
		"being deleted": {`{"metadata":{"name":"api-1","creationTimestamp":"2026-10-07T09:04:26Z","deletionTimestamp":"2026-10-07T09:17:00Z"},"spec":{"containers":[{"name":"app"}]},
			"status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":0,"state":{"running":{}}}]}}`,
			[]any{"api-1", "1/1", "Terminating", "0", "13m", "<none>", "<none>", "<none>", "<none>"}},
		"a job's pod, done": {`{` + meta + `,"spec":{"containers":[{"name":"app"}],"readinessGates":[{"conditionType":"lb"},{"conditionType":"dns"}]},
			"status":{"phase":"Succeeded","conditions":[{"type":"lb","status":"True"},{"type":"dns","status":"False"}],"containerStatuses":[{"ready":false,"restartCount":0,"state":{"terminated":{"exitCode":0,"reason":"Completed"}}}]}}`,
			[]any{"api-1", "0/1", "Completed", "0", "13m", "<none>", "<none>", "<none>", "1/2"}},
		"evicted": {`{` + meta + `,"spec":{"containers":[{"name":"app"}]},"status":{"phase":"Failed","reason":"Evicted"}}`,
			[]any{"api-1", "0/1", "Evicted", "0", "13m", "<none>", "<none>", "<none>", "<none>"}},
	} {
		if got := podCells(decode(t, c.pod), frozenAt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
	if n := len(printers["Pod"].columns); n != 9 {
		t.Errorf("%d columns for 9 cells", n)
	}
}

func TestTheOtherKindsRows(t *testing.T) {
	const tmpl = `"template":{"spec":{"nodeSelector":{"kubernetes.io/os":"linux","disk":"ssd"},"containers":[{"name":"app","image":"python:3.12-alpine"},{"name":"proxy","image":"envoy:1"}]}}`
	const sel = `"selector":{"matchLabels":{"app":"cache","tier":"a"},"matchExpressions":[{"key":"zone","operator":"In","values":["b","a"]},{"key":"canary","operator":"DoesNotExist"},{"key":"env","operator":"NotIn","values":["dev"]},{"key":"owner","operator":"Exists"}]}`
	const selText = "app=cache,!canary,env notin (dev),owner,tier=a,zone in (a,b)" // in the order of the keys
	const meta = `"metadata":{"name":"cache","creationTimestamp":"2026-10-06T06:17:26Z"}`
	for kind, c := range map[string]struct {
		object string
		want   []any
	}{
		"Deployment": {`{` + meta + `,"spec":{"replicas":3,` + sel + `,` + tmpl + `},"status":{"readyReplicas":2,"updatedReplicas":3,"availableReplicas":2}}`,
			[]any{"cache", "2/3", int64(3), int64(2), "27h", "app,proxy", "python:3.12-alpine,envoy:1", selText}},
		"ReplicaSet": {`{` + meta + `,"spec":{"replicas":3,` + sel + `,` + tmpl + `},"status":{"replicas":3,"readyReplicas":2}}`,
			[]any{"cache", int64(3), int64(3), int64(2), "27h", "app,proxy", "python:3.12-alpine,envoy:1", selText}},
		"DaemonSet": {`{` + meta + `,"spec":{` + sel + `,` + tmpl + `},"status":{"desiredNumberScheduled":3,"currentNumberScheduled":3,"numberReady":2,"updatedNumberScheduled":3,"numberAvailable":2}}`,
			[]any{"cache", int64(3), int64(3), int64(2), int64(3), int64(2), "disk=ssd,kubernetes.io/os=linux", "27h", "app,proxy", "python:3.12-alpine,envoy:1", selText}},
		"Service": {`{` + meta + `,"spec":{"type":"NodePort","clusterIPs":["10.96.204.9"],"selector":{"app":"cache"},"ports":[{"port":6379,"protocol":"TCP"},{"port":80,"nodePort":30080,"protocol":"TCP"}]}}`,
			[]any{"cache", "NodePort", "10.96.204.9", "<none>", "6379/TCP,80:30080/TCP", "27h", "app=cache"}},
		"Endpoints": {`{` + meta + `,"subsets":[{"addresses":[{"ip":"10.244.3.2"},{"ip":"10.244.3.3"}],"ports":[{"port":6379},{"port":9100}]}]}`,
			[]any{"cache", "10.244.3.2:6379,10.244.3.3:6379,10.244.3.2:9100 + 1 more...", "27h"}},
		"EndpointSlice": {`{` + meta + `,"addressType":"IPv4","ports":[{"port":6379},{"name":"metrics"}],"endpoints":[{"addresses":["10.244.3.2"]},{"addresses":["10.244.3.3","10.244.3.4","10.244.3.5"]}]}`,
			[]any{"cache", "IPv4", "6379,metrics", "10.244.3.2,10.244.3.3,10.244.3.4 + 1 more...", "27h"}},
	} {
		p := printers[kind]
		if got := p.cells(decode(t, c.object), frozenAt); !reflect.DeepEqual(got, c.want) || len(got) != len(p.columns) {
			t.Errorf("%s (%d columns):\n got %q\nwant %q", kind, len(p.columns), got, c.want)
		}
	}
	if got := serviceCells(decode(t, `{"metadata":{"name":"lb"},"spec":{"type":"LoadBalancer","ports":[]}}`), frozenAt); got[2] != "<none>" || got[3] != "<pending>" || got[4] != "<none>" || got[5] != "<unknown>" || got[6] != "<none>" {
		t.Errorf("a load balancer with nothing yet: %q", got)
	}
	if got := endpoints(decode(t, `{}`)); got != "<none>" {
		t.Errorf("no endpoints: %q", got)
	}
}

// The age a cluster prints: two units while the larger one is small, one after that.
func TestAnAgeIsWrittenAsAClusterWritesIt(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-5 * time.Second: "<invalid>", 0: "0s", 119 * time.Second: "119s", 2 * time.Minute: "2m", 9*time.Minute + 59*time.Second: "9m59s", 10*time.Minute + 30*time.Second: "10m",
		179 * time.Minute: "179m", 3 * time.Hour: "3h", 7*time.Hour + 59*time.Minute: "7h59m", 8*time.Hour + 30*time.Minute: "8h", 47 * time.Hour: "47h", 48 * time.Hour: "2d",
		7*24*time.Hour + 5*time.Hour: "7d5h", 8 * 24 * time.Hour: "8d", 729 * 24 * time.Hour: "729d", 3*365*24*time.Hour + 4*24*time.Hour: "3y4d", 9 * 365 * 24 * time.Hour: "9y",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func tableOf(t *testing.T, body string) (columns []column, cells [][]any, objects []obj) {
	t.Helper()
	var table struct {
		Kind    string   `json:"kind"`
		Columns []column `json:"columnDefinitions"`
		Rows    []struct {
			Cells  []any `json:"cells"`
			Object obj   `json:"object"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &table); err != nil || table.Kind != "Table" {
		t.Fatalf("not a table (%v): %s", err, body)
	}
	for _, r := range table.Rows {
		cells, objects = append(cells, r.Cells), append(objects, r.Object)
	}
	return table.Columns, cells, objects
}

func TestATableIsTheClustersNotTheSnapshotServers(t *testing.T) {
	upstream, asked := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target, frozenAt))
	defer front.Close()

	// A list: the wide columns are there, marked so that kubectl shows them only for -o wide, the rows
	// are in the order a cluster lists, and each carries its metadata.
	_, body := ask(t, front, http.MethodGet, shopPods+"?limit=500", asTable)
	columns, cells, objects := tableOf(t, body)
	var plain, extra []string
	for _, c := range columns {
		if c.Priority == 0 {
			plain = append(plain, c.Name)
		} else {
			extra = append(extra, c.Name)
		}
	}
	if !reflect.DeepEqual(plain, []string{"Name", "Ready", "Status", "Restarts", "Age"}) || !reflect.DeepEqual(extra, []string{"IP", "Node", "Nominated Node", "Readiness Gates"}) {
		t.Errorf("columns %v and, for -o wide, %v", plain, extra)
	}
	if len(cells) != 4 || !reflect.DeepEqual(cells[0], []any{"api-1", "0/1", "Running", "0", "13m", "<none>", "worker2", "<none>", "<none>"}) || cells[3][0] != "job-1" || cells[3][6] != "<none>" {
		t.Errorf("rows: %q", cells)
	}
	if o := objects[0]; o.str("kind") != "PartialObjectMetadata" || o.name() != "api-1" || o.has("spec") {
		t.Errorf("a row's object by default: %v", o)
	}

	// `kubectl get --sort-by` asks for the whole object in each row and sorts by a path into it. With
	// metadata only, a sort on anything else finds no such field and prints "No resources found".
	_, body = ask(t, front, http.MethodGet, shopPods+"?includeObject=Object", asTable)
	if _, _, objects = tableOf(t, body); objects[0].str("kind") != "Pod" || objects[0].str("apiVersion") != "v1" || objects[0].str("spec", "nodeName") != "worker2" || objects[0].str("status", "phase") != "Running" {
		t.Errorf("a row's object when the whole of it is asked for: %v", objects[0])
	}
	_, body = ask(t, front, http.MethodGet, shopPods+"?includeObject=None", asTable)
	if _, _, objects = tableOf(t, body); objects[0] != nil {
		t.Errorf("a row's object when none is asked for: %v", objects[0])
	}

	// One object by name: a table of one row with its kind's columns, where the snapshot server sends
	// the object and kubectl prints NAME and AGE.
	_, body = ask(t, front, http.MethodGet, shopPods+"/cache-1", asTable)
	if columns, cells, _ = tableOf(t, body); len(columns) != 9 || !reflect.DeepEqual(cells, [][]any{{"cache-1", "1/1", "Running", "0", "13m", "10.244.3.2", "worker", "<none>", "<none>"}}) || !strings.Contains(body, `"resourceVersion":"7"`) {
		t.Errorf("one pod: %s", body)
	}

	// Events: a cluster's heading, the object the event is about, and the times counted to the freeze.
	_, body = ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/events?includeObject=Object", asTable)
	columns, cells, objects = tableOf(t, body)
	if columns[0].Name != "Last Seen" || columns[3].Name != "Object" || columns[6].Name != "Message" || columns[4].Priority != 1 {
		t.Errorf("event columns: %+v", columns)
	}
	if want := []any{"30s", "Normal", "Pulled", "pod/cache-1", "spec.containers{app}", "kubelet, worker", "Container image already present", "12m", float64(3), "e1"}; !reflect.DeepEqual(cells[0], want) {
		t.Errorf("an event:\n got %q\nwant %q", cells[0], want)
	}
	if want := []any{"10m", "", "SuccessfulCreate", "replicaset/api", "", "replicaset-controller", "", "10m", float64(1), "e3"}; !reflect.DeepEqual(cells[2], want) {
		t.Errorf("an event with only an event time:\n got %q\nwant %q", cells[2], want)
	}
	if objects[0].str("lastTimestamp") == "" {
		t.Error("an event's row does not carry what --sort-by=.lastTimestamp sorts by")
	}

	// A kind with no printer here keeps the snapshot server's columns, in a cluster's order, with the
	// age written as a cluster writes it; and one object of it is its row, not NAME and AGE.
	_, body = ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/configmaps?includeObject=Object", asTable)
	columns, cells, objects = tableOf(t, body)
	if len(columns) != 3 || !reflect.DeepEqual(cells, [][]any{{"settings", "1", "120m"}, {"zeta", "0", "2m"}}) || objects[0].str("data", "MODE") != "per-task" || objects[0].str("kind") != "ConfigMap" {
		t.Errorf("a kind without a printer: %s", body)
	}
	if columns[0].Format != "name" || columns[0].Type != "string" || columns[1].Type != "string" {
		t.Errorf("the name column is not marked as one, so kubectl will not write configmap/settings beside other kinds: %+v", columns)
	}
	_, body = ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/configmaps/settings", asTable)
	if _, cells, _ = tableOf(t, body); !reflect.DeepEqual(cells, [][]any{{"settings", "1", "120m"}}) {
		t.Errorf("one object of a kind without a printer: %s", body)
	}

	// What is not there is the snapshot server's to say, in its words.
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/configmaps/missing", asTable); code != http.StatusNotFound || !strings.Contains(body, `"code":404`) {
		t.Errorf("an object that is not there: %d %s", code, body)
	}
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/gone/pods", asTable); code != http.StatusNotFound || !strings.Contains(body, `"code":404`) {
		t.Errorf("a list that is not there: %d %s", code, body)
	}
	for _, a := range *asked {
		if strings.Contains(a, "includeObject") || strings.Contains(a, "limit=") {
			t.Errorf("the snapshot server was asked %s", a)
		}
	}
}

func TestWhatIsUnderAnObjectIsNotATable(t *testing.T) {
	for path, want := range map[string]bool{
		"/kubernetes/api/v1/pods":                                          false,
		"/kubernetes/api/v1/nodes/worker":                                  false,
		"/kubernetes/api/v1/namespaces":                                    false,
		"/kubernetes/api/v1/namespaces/shop":                               false,
		"/kubernetes/api/v1/namespaces/shop/status":                        true,
		"/kubernetes/api/v1/namespaces/shop/pods":                          false,
		"/kubernetes/api/v1/namespaces/shop/pods/api-1":                    false,
		"/kubernetes/api/v1/namespaces/shop/pods/api-1/log":                true,
		"/kubernetes/api/v1/namespaces/status/pods/log":                    false, // a namespace called status, a pod called log
		"/kubernetes/api/v1/nodes/worker/proxy/metrics":                    true,
		"/kubernetes/apis/apps/v1/deployments":                             false,
		"/kubernetes/apis/apps/v1/namespaces/shop/deployments/cache":       false,
		"/kubernetes/apis/apps/v1/namespaces/shop/deployments/cache/scale": true,
		"/apis/apps/v1/namespaces/shop/deployments/cache/status":           true,
		"/kubernetes/version":                                              false,
	} {
		if got := isSubresource(path); got != want {
			t.Errorf("isSubresource(%s) = %v", path, got)
		}
	}
	upstream, _ := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target, frozenAt))
	defer front.Close()
	// A log asked for by a client that also accepts tables is still a log, and a watch is still a stream.
	if code, body := ask(t, front, http.MethodGet, shopPods+"/cache-1/log", asTable); code != http.StatusOK || body != "a stream of log lines\n" {
		t.Errorf("a log: %d %q", code, body)
	}
	if _, body := ask(t, front, http.MethodGet, shopPods+"?watch=true", asTable); !strings.Contains(body, `"columnDefinitions":[{"name":"Name"}]`) {
		t.Errorf("a watch was answered here: %s", body)
	}
}

func TestNodesAndWhatAClusterIsMadeOf(t *testing.T) {
	node := `{"metadata":{"name":"worker2","creationTimestamp":"2026-10-07T09:04:26Z","labels":{"node-role.kubernetes.io/edge":"","kubernetes.io/role":"gpu","kubernetes.io/os":"linux"}},"spec":{"unschedulable":true},
		"status":{"conditions":[{"type":"MemoryPressure","status":"False"},{"type":"Ready","status":"False"}],"addresses":[{"type":"InternalIP","address":"172.20.0.3"},{"type":"Hostname","address":"worker2"}],
		"nodeInfo":{"kubeletVersion":"v1.37.0","osImage":"Debian GNU/Linux 13 (trixie)","kernelVersion":"6.10.14-linuxkit","architecture":"arm64","containerRuntimeVersion":"containerd://2.3.4"}}}`
	if got, want := nodeCells(decode(t, node), frozenAt), []any{"worker2", "NotReady,SchedulingDisabled", "edge,gpu", "13m", "v1.37.0", "172.20.0.3", "<none>", "Debian GNU/Linux 13 (trixie)", "6.10.14-linuxkit (arm64)", "containerd://2.3.4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a node:\n got %q\nwant %q", got, want)
	}
	if got := nodeCells(decode(t, `{"metadata":{"name":"new"}}`), frozenAt); got[1] != "Unknown" || got[2] != "<none>" || got[3] != "<unknown>" || got[8] != "<unknown>" {
		t.Errorf("a node that has reported nothing: %q", got)
	}
	binding := `{"metadata":{"name":"readers","creationTimestamp":"2026-10-07T09:04:26Z"},"roleRef":{"kind":"ClusterRole","name":"view"},
		"subjects":[{"kind":"User","name":"ana"},{"kind":"Group","name":"system:masters"},{"kind":"ServiceAccount","namespace":"shop","name":"default"},{"kind":"User","name":"bo"}]}`
	if got, want := bindingCells(decode(t, binding), frozenAt), []any{"readers", "ClusterRole/view", "13m", "ana, bo", "system:masters", "shop/default"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a binding:\n got %q\nwant %q", got, want)
	}
	if got := createdAt(decode(t, binding), frozenAt); !reflect.DeepEqual(got, []any{"readers", "2026-10-07T09:04:26Z"}) {
		t.Errorf("a role: %q", got)
	}
	// Every kind's cells are as many as its columns, whatever the object lacks.
	for kind, p := range printers {
		if got := p.cells(decode(t, `{"metadata":{"name":"x"}}`), frozenAt); len(got) != len(p.columns) {
			t.Errorf("%s: %d cells for %d columns", kind, len(got), len(p.columns))
		}
	}
	// A list is in the order a cluster gives it, which for one kind is not by name; and the default
	// storage class is marked in a list and not when it is asked for by name.
	a, b := decode(t, `{"metadata":{"name":"a"},"spec":{"matchingPrecedence":9000}}`), decode(t, `{"metadata":{"name":"b"},"spec":{"matchingPrecedence":1}}`)
	if less := printers["FlowSchema"].before; !less(b, a) || less(a, b) {
		t.Error("flow schemas are not in the order they are applied")
	}
	class := decode(t, `{"metadata":{"name":"standard","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},"provisioner":"rancher.io/local-path"}`)
	cells := printers["StorageClass"].cells(class, frozenAt)
	if cells[0] != "standard" || cells[2] != "Delete" || cells[3] != "Immediate" || cells[4] != false {
		t.Errorf("a storage class by name: %q", cells)
	}
	if printers["StorageClass"].inList(class, cells); cells[0] != "standard (default)" {
		t.Errorf("the default storage class in a list: %q", cells)
	}
}

// Three more things the snapshot server does not do as a cluster does, found by asking a cluster and
// its frozen copy the same commands (test/replay-diff).
func TestWhatIsNotThereAndWhatWasBlanked(t *testing.T) {
	upstream, _ := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target, frozenAt))
	defer front.Close()
	const roles = "/kubernetes/apis/rbac.authorization.k8s.io/v1/namespaces/shop/roles"

	// An object the snapshot server lists and cannot find by name: every `system:` role and binding.
	code, body := ask(t, front, http.MethodGet, roles+"/system:reader", asObjects)
	if o := decode(t, body); code != http.StatusOK || o.str("kind") != "Role" || o.str("apiVersion") != "rbac.authorization.k8s.io/v1" || o.name() != "system:reader" || len(o.list("rules")) != 1 {
		t.Errorf("an object with a colon in its name: %d %s", code, body)
	}
	_, body = ask(t, front, http.MethodGet, roles+"/system:reader", asTable)
	if columns, cells, _ := tableOf(t, body); len(columns) != 2 || !reflect.DeepEqual(cells, [][]any{{"system:reader", "2026-10-07T09:04:26Z"}}) {
		t.Errorf("its table: %s", body)
	}

	// What is not there is refused in a cluster's words, as an object, as a table, and as a log.
	for path, accept := range map[string]string{roles + "/nobody": asObjects, roles + "/nobody?x=1": asTable} {
		code, body := ask(t, front, http.MethodGet, path, accept)
		if o := decode(t, body); code != http.StatusNotFound || o.str("reason") != "NotFound" || o.str("message") != `roles.rbac.authorization.k8s.io "nobody" not found` || o.str("details", "group") != "rbac.authorization.k8s.io" || o.str("details", "kind") != "roles" {
			t.Errorf("a role that is not there: %d %s", code, body)
		}
	}
	for _, path := range []string{shopPods + "/ghost", shopPods + "/ghost/log?tailLines=5", shopPods + "/ghost/log"} {
		code, body := ask(t, front, http.MethodGet, path, asObjects)
		if o := decode(t, body); code != http.StatusNotFound || o.str("message") != `pods "ghost" not found` || o.has("details", "group") {
			t.Errorf("%s: %d %s", path, code, body)
		}
	}
	// `kubectl logs -p` on a container that has not restarted: a cluster says so.
	code, body = ask(t, front, http.MethodGet, shopPods+"/cache-1/log?previous=true", asObjects)
	if o := decode(t, body); code != http.StatusBadRequest || o.str("reason") != "BadRequest" || o.str("message") != `previous terminated container "app" in pod "cache-1" not found` {
		t.Errorf("the previous log of a container that has run once: %d %s", code, body)
	}
	// …but only when the list was read and the object is not in it. A kind the case does not have is
	// the snapshot server's to refuse.
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/gone/pods/x", asObjects); code != http.StatusNotFound || strings.Contains(body, "not found") {
		t.Errorf("an object of a list that is not there: %d %s", code, body)
	}

	// A Secret that `freeze` blanked decodes: its value is sent as the base64 of the marker.
	const marker = "UkVEQUNURUQtQlktTEFQSUxMSQ==" // base64 of REDACTED-BY-LAPILLI
	_, body = ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/secrets/token", asObjects)
	if o := decode(t, body); o.str("data", "password") != marker || o.str("type") != "Opaque" {
		t.Errorf("one secret: %s", body)
	}
	_, body = ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/secrets?limit=500", asObjects)
	if o := decode(t, body); o.str("kind") != "SecretList" || len(o.list("items")) != 1 || o.list("items")[0].str("data", "password") != marker || o.list("items")[0].str("data", "note") != "aGVsbG8=" {
		t.Errorf("a list of secrets: %s", body)
	}
	_, body = ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/secrets?includeObject=Object", asTable)
	if strings.Contains(body, `"REDACTED-BY-LAPILLI"`) || !strings.Contains(body, marker) {
		t.Errorf("a table of secrets with the objects in it: %s", body)
	}
}

// A cluster of v1.33 or later sends a deprecation warning with every answer about Endpoints, and
// kubectl prints it on every such command.
func TestEndpointsCarryTheClustersWarning(t *testing.T) {
	upstream, _ := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target, frozenAt))
	defer front.Close()
	for path, want := range map[string]bool{"/kubernetes/api/v1/namespaces/shop/endpoints": true, "/kubernetes/api/v1/endpoints": true, shopPods: false,
		"/kubernetes/apis/discovery.k8s.io/v1/namespaces/shop/endpointslices": false} {
		req, _ := http.NewRequest(http.MethodGet, front.URL+path, nil)
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := strings.Contains(resp.Header.Get("Warning"), "v1 Endpoints is deprecated in v1.33+"); got != want {
			t.Errorf("%s: warning %v, want %v", path, got, want)
		}
	}
}

// `kubectl rollout status deployment/x` watches one Deployment by name. Unfiltered, the watch sends
// whichever object comes first, and the rollout reported is another Deployment's.
func TestAWatchThatNamesItsObjectIsOfThatObject(t *testing.T) {
	upstream, _ := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target, frozenAt))
	defer front.Close()

	events := func(query string) (types, names []string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+shopPods+"?watch=true&"+query, nil)
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		lines := bufio.NewScanner(resp.Body)
		lines.Buffer(make([]byte, 1<<20), 1<<20)
		for lines.Scan() { // ends when the watch's time is up
			var e struct {
				Type   string `json:"type"`
				Object obj    `json:"object"`
			}
			if err := json.Unmarshal(lines.Bytes(), &e); err != nil {
				t.Fatalf("%v: %s", err, lines.Text())
			}
			if e.Type == "BOOKMARK" && e.Object.str("metadata", "annotations", "k8s.io/initial-events-end") != "true" {
				t.Errorf("a bookmark that does not end the initial events: %s", lines.Text())
			}
			if e.Type == "ADDED" && (e.Object.str("kind") != "Pod" || e.Object.str("apiVersion") != "v1") {
				t.Errorf("an event whose object does not say what it is: %s", lines.Text())
			}
			types, names = append(types, e.Type), append(names, e.Object.name())
		}
		return types, names
	}
	// What kubectl sends first: the state, and a mark that it is all of it.
	types, names := events("fieldSelector=metadata.name%3Dapi-2&sendInitialEvents=true&resourceVersionMatch=NotOlderThan&allowWatchBookmarks=true&timeoutSeconds=1")
	if !reflect.DeepEqual(types, []string{"ADDED", "BOOKMARK"}) || names[0] != "api-2" {
		t.Errorf("a watch of one pod, from its state: %v %v", types, names)
	}
	// And then: from that state on, where a frozen cluster has nothing more to say.
	if types, _ := events("fieldSelector=metadata.name%3Dapi-2&resourceVersion=1&timeoutSeconds=1"); len(types) != 0 {
		t.Errorf("a watch from a version on sent %v", types)
	}
	if types, names := events("fieldSelector=spec.nodeName%3Dworker2&timeoutSeconds=1"); !reflect.DeepEqual(types, []string{"ADDED", "ADDED"}) || !reflect.DeepEqual(names, []string{"api-1", "api-2"}) {
		t.Errorf("a watch from no version: %v %v", types, names)
	}
}
