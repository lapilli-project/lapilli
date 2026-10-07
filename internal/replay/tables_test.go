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

	// An object that is not in its list is refused here, in a cluster's words; a list that is not
	// there is the snapshot server's to refuse, in its own.
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/configmaps/missing", asTable); code != http.StatusNotFound || !strings.Contains(body, `configmaps \"missing\" not found`) {
		t.Errorf("an object that is not there: %d %s", code, body)
	}
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/gone/pods", asTable); code != http.StatusNotFound || body != `{"kind":"Status","status":"Failure","code":404}` {
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
		if rp, ok := parseResourcePath(path); (ok && rp.sub != "") != want {
			t.Errorf("%s: under an object %v, read as %+v", path, !want, rp)
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
	if printers["StorageClass"].inList(class, cells, []obj{class}); cells[0] != "standard (default)" {
		t.Errorf("the default storage class in a list: %q", cells)
	}
}

// A table is written as v1.37 writes it. Where an earlier version wrote another — each of these was
// seen on a cluster of the versions named — a case frozen from that version is given that.
func TestATableAsAnOlderClusterWroteIt(t *testing.T) {
	at := func(kind string, minor int) printer { p := printers[kind]; return p.as(minor, p) }

	node := decode(t, `{"metadata":{"name":"w"},"status":{"nodeInfo":{"kernelVersion":"6.10.14-linuxkit","architecture":"arm64"}}}`)
	for minor, want := range map[int]string{33: "6.10.14-linuxkit", 35: "6.10.14-linuxkit", 36: "6.10.14-linuxkit (arm64)", 37: "6.10.14-linuxkit (arm64)"} {
		if got := at("Node", minor).cells(node, frozenAt)[8]; got != want {
			t.Errorf("a node's kernel on v1.%d: %q, want %q", minor, got, want)
		}
	}
	if got := at("Node", 33).cells(decode(t, `{"metadata":{"name":"w"}}`), frozenAt)[8]; got != "<unknown>" {
		t.Errorf("a node that does not say its kernel, on v1.33: %q", got)
	}

	account := decode(t, `{"metadata":{"name":"default"},"secrets":[{"name":"a"},{"name":"b"}]}`)
	if p := at("ServiceAccount", 34); len(p.columns) != 3 || p.columns[1].Name != "Secrets" || !reflect.DeepEqual(p.cells(account, frozenAt)[:2], []any{"default", int64(2)}) {
		t.Errorf("a service account on v1.34: %v %v", p.columns, p.cells(account, frozenAt))
	}
	if p := at("ServiceAccount", 35); len(p.columns) != 2 || len(p.cells(account, frozenAt)) != 2 {
		t.Errorf("a service account on v1.35: %v %v", p.columns, p.cells(account, frozenAt))
	}

	class := decode(t, `{"metadata":{"name":"standard","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},"provisioner":"p"}`)
	if p := at("StorageClass", 36); p.cells(class, frozenAt)[0] != "standard (default)" || p.inList != nil {
		t.Errorf("the default storage class by name on v1.36: %v", p.cells(class, frozenAt))
	}
	if p := at("StorageClass", 37); p.cells(class, frozenAt)[0] != "standard" || p.inList == nil {
		t.Errorf("the default storage class by name on v1.37: %v", p.cells(class, frozenAt))
	}
	definition := decode(t, `{"metadata":{"name":"widgets.example.dev","creationTimestamp":"2026-10-07T09:07:26Z"},"spec":{"scope":"Namespaced","versions":[{"name":"v1","served":true,"storage":true}]}}`)
	if p := at("CustomResourceDefinition", 36); len(p.columns) != 2 || !reflect.DeepEqual(p.cells(definition, frozenAt), []any{"widgets.example.dev", "2026-10-07T09:07:26Z"}) {
		t.Errorf("a custom resource definition on v1.36: %v %v", p.columns, p.cells(definition, frozenAt))
	}
	if p := at("CustomResourceDefinition", 37); len(p.columns) != 8 || p.cells(definition, frozenAt)[2] != "v1(storage)" {
		t.Errorf("a custom resource definition on v1.37: %v %v", p.columns, p.cells(definition, frozenAt))
	}
	priority := decode(t, `{"metadata":{"name":"high"},"value":1000,"preemptionPolicy":"Never"}`)
	if p := at("PriorityClass", 31); len(p.columns) != 4 || len(p.cells(priority, frozenAt)) != 4 {
		t.Errorf("a priority class on v1.31: %v %v", p.columns, p.cells(priority, frozenAt))
	}
	if p := at("PriorityClass", 32); len(p.columns) != 5 || p.cells(priority, frozenAt)[4] != "Never" {
		t.Errorf("a priority class on v1.32: %v %v", p.columns, p.cells(priority, frozenAt))
	}
	quota := decode(t, `{"metadata":{"name":"counts","creationTimestamp":"2026-10-07T09:07:26Z"},"status":{"hard":{"pods":"10","limits.cpu":"4"},"used":{"pods":"3"}}}`)
	if p := at("ResourceQuota", 32); p.columns[1].Name != "Age" || !reflect.DeepEqual(p.cells(quota, frozenAt), []any{"counts", "10m", "pods: 3/10", "limits.cpu: 0/4"}) {
		t.Errorf("a quota on v1.32: %v %v", p.columns, p.cells(quota, frozenAt))
	}
	if p := at("ResourceQuota", 33); p.columns[3].Name != "Age" || !reflect.DeepEqual(p.cells(quota, frozenAt), []any{"counts", "pods: 3/10", "limits.cpu: 0/4", "10m"}) {
		t.Errorf("a quota on v1.33: %v %v", p.columns, p.cells(quota, frozenAt))
	}
	if len(printers["PriorityClass"].columns) != 5 {
		t.Error("the priority class printer was shortened by being asked for an older version")
	}
	// Asking for an older printer does not change the one that is kept.
	if printers["StorageClass"].inList == nil || len(printers["ServiceAccount"].columns) != 2 || printers["StorageClass"].cells(class, frozenAt)[0] != "standard" {
		t.Error("the printers were changed by being asked for an older version")
	}

	// And through a request: the version is the one the snapshot says its cluster had.
	upstream, _ := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	standInMinor = "36"
	defer func() { standInMinor = "37" }()
	older := httptest.NewServer(newFront(target, frozenAt))
	defer older.Close()
	_, body := ask(t, older, http.MethodGet, "/kubernetes/apis/storage.k8s.io/v1/storageclasses/standard", asTable)
	if _, cells, _ := tableOf(t, body); len(cells) != 1 || cells[0][0] != "standard (default)" {
		t.Errorf("the default storage class by name, from a v1.36 cluster: %s", body)
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
	// And then: from that state on, where a frozen cluster has nothing more to say, for as long as it
	// was asked to wait and no longer.
	started := time.Now()
	if types, _ := events("fieldSelector=metadata.name%3Dapi-2&resourceVersion=1&timeoutSeconds=1"); len(types) != 0 {
		t.Errorf("a watch from a version on sent %v", types)
	}
	if waited := time.Since(started); waited < 900*time.Millisecond || waited > 3*time.Second {
		t.Errorf("a watch asked to wait one second waited %v", waited)
	}
	if types, names := events("fieldSelector=spec.nodeName%3Dworker2&timeoutSeconds=1"); !reflect.DeepEqual(types, []string{"ADDED", "ADDED"}) || !reflect.DeepEqual(names, []string{"api-1", "api-2"}) {
		t.Errorf("a watch from no version: %v %v", types, names)
	}
}

// What an independent reading of this code found that the comparison with a cluster could not: the
// states no scenario has.
func TestWhatNoScenarioHas(t *testing.T) {
	upstream, _ := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target, frozenAt))
	defer front.Close()

	// An age is counted to the instant `freeze` noted before it began to read. What was stamped a few
	// seconds after that is in the case too, and is not "<invalid>".
	if got := since(frozenAt.Add(5*time.Second), frozenAt); got != "0s" {
		t.Errorf("the age of something stamped after the freeze: %q", got)
	}
	// A cluster's order is that of its keys, namespace/name: a hyphen sorts before the slash.
	items := []obj{decode(t, `{"metadata":{"name":"a","namespace":"shop"}}`), decode(t, `{"metadata":{"name":"z","namespace":"shop-db"}}`), decode(t, `{"metadata":{"name":"node-b"}}`), decode(t, `{"metadata":{"name":"node-a"}}`)}
	inKeyOrder(items)
	if got := []string{items[0].name(), items[1].name(), items[2].name(), items[3].name()}; !reflect.DeepEqual(got, []string{"node-a", "node-b", "z", "a"}) {
		t.Errorf("the order of a list: %v", got)
	}

	// A printer is for a kind of one API group. The snapshot server prints a custom resource called
	// Service as a Service. Here it is printed from what its own definition says to print, for the
	// version asked for: a path, a filter, a date as how long ago, a number as a number, and nothing
	// where there is nothing.
	_, body := ask(t, front, http.MethodGet, "/kubernetes/apis/serving.knative.dev/v1/namespaces/shop/services", asTable)
	columns, cells, _ := tableOf(t, body)
	var heads []string
	for _, c := range columns {
		heads = append(heads, c.Name)
	}
	if !reflect.DeepEqual(heads, []string{"Name", "URL", "Ready", "Latest", "Count", "Missing"}) || columns[4].Priority != 1 ||
		!reflect.DeepEqual(cells, [][]any{{"hello", "http://hello.shop", "True", "10m", float64(3), nil}}) {
		t.Errorf("a custom resource named like a built-in kind: %s", body)
	}
	_, body = ask(t, front, http.MethodGet, "/kubernetes/apis/serving.knative.dev/v1/namespaces/shop/services/hello", asTable)
	if _, one, _ := tableOf(t, body); !reflect.DeepEqual(one, cells) {
		t.Errorf("the same one asked for by name: %s", body)
	}
	if p := printers["Event"]; p.group != "" || printers["Deployment"].group != "apps" || printers["EndpointSlice"].group != "discovery.k8s.io" {
		t.Error("a printer without its API group")
	}
	// So is every other custom resource, with the API server's JSONPath and not the snapshot server's:
	// an escaped dot in a label's name, the last of a list, a date as how long ago.
	_, body = ask(t, front, http.MethodGet, "/kubernetes/apis/example.dev/v1/namespaces/shop/widgets", asTable)
	if columns, cells, _ := tableOf(t, body); len(columns) != 4 || columns[2].Type != "date" || !reflect.DeepEqual(cells, [][]any{{"left", "gold", "10m", "blade"}}) {
		t.Errorf("a custom resource: %s", body)
	}
	// An Event of events.k8s.io is the Event of the core group under other names, and the API server
	// prints both with one printer.
	_, body = ask(t, front, http.MethodGet, "/kubernetes/apis/events.k8s.io/v1/namespaces/shop/events?includeObject=Object", asTable)
	columns, cells, objects := tableOf(t, body)
	if len(columns) != 10 || columns[0].Name != "Last Seen" || len(cells) != 1 ||
		!reflect.DeepEqual(cells[0], []any{"60s", "Warning", "BackOff", "pod/web", "spec.containers{app}", "kubelet, w1", "Back-off restarting failed container", "10m", float64(7), "web.1"}) {
		t.Errorf("an Event of events.k8s.io: %s", body)
	}
	if len(objects) != 1 || objects[0].str("regarding", "name") != "web" || objects[0].str("apiVersion") != "events.k8s.io/v1" || objects[0].has("involvedObject") {
		t.Errorf("the object in its row is the one that was asked for, in its own words: %s", body)
	}
	// A kind named like one of Kubernetes's that is no custom resource and no kind known here keeps
	// the snapshot server's table: nothing better is known.
	_, body = ask(t, front, http.MethodGet, "/kubernetes/apis/metrics.example/v1/namespaces/shop/pods", asTable)
	if columns, cells, _ := tableOf(t, body); len(columns) != 2 || columns[1].Name != "CPU" || !reflect.DeepEqual(cells, [][]any{{"web", "250m"}}) {
		t.Errorf("a kind of an aggregated API named like a built-in one: %s", body)
	}

	// What a cluster does for a row of a list and not for one object, and its own order for one kind,
	// through a request and not only as functions.
	_, body = ask(t, front, http.MethodGet, "/kubernetes/apis/storage.k8s.io/v1/storageclasses", asTable)
	if _, cells, _ := tableOf(t, body); len(cells) != 1 || cells[0][0] != "standard (default)" {
		t.Errorf("the default storage class in a list: %s", body)
	}
	_, body = ask(t, front, http.MethodGet, "/kubernetes/apis/storage.k8s.io/v1/storageclasses/standard", asTable)
	if _, cells, _ := tableOf(t, body); len(cells) != 1 || cells[0][0] != "standard" {
		t.Errorf("the default storage class by name: %s", body)
	}
	_, body = ask(t, front, http.MethodGet, "/kubernetes/apis/flowcontrol.apiserver.k8s.io/v1/flowschemas", asTable)
	if _, cells, _ := tableOf(t, body); len(cells) != 2 || cells[0][0] != "z-first" || cells[1][0] != "a-last" {
		t.Errorf("flow schemas in the order they are applied: %s", body)
	}

	// A hole in a list is skipped, as a table and as objects, and is not the end of the connection.
	_, body = ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/odd/pods", asTable)
	if _, cells, _ := tableOf(t, body); len(cells) != 2 || cells[0][0] != "cache-1" {
		t.Errorf("a list with a null in it, as a table: %s", body)
	}
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/odd/pods", asObjects); code != http.StatusOK || len(names(t, body, "items")) != 2 {
		t.Errorf("a list with a null in it: %d %s", code, body)
	}

	// One object by name is that object: not one of another name, not one of another namespace, and
	// not whatever is at an address the snapshot server points to.
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/configmaps/zeta-alias", asObjects); code != http.StatusNotFound || !strings.Contains(body, `configmaps \"zeta-alias\" not found`) {
		t.Errorf("an object answered under another name: %d %s", code, body)
	}
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/pods/cache-1", asObjects); code != http.StatusNotFound || !strings.Contains(body, `pods \"cache-1\" not found`) {
		t.Errorf("a namespaced object asked for with no namespace: %d %s", code, body)
	}
	for _, accept := range []string{asObjects, asTable} {
		if code, _ := ask(t, front, http.MethodGet, shopPods+"/moved", accept); code != http.StatusBadGateway || followed != 0 {
			t.Errorf("a redirect from the snapshot server: answered %d, followed %d times", code, followed)
		}
	}

	// `kubectl logs -p`: a cluster's words only where they are known to be the right ones. A container
	// that has restarted, or one the pod does not have, is the snapshot server's to answer.
	for _, path := range []string{shopPods + "/crashy/log?previous=true", shopPods + "/cache-1/log?previous=true&container=nosuch"} {
		if code, body := ask(t, front, http.MethodGet, path, asObjects); code != http.StatusBadRequest || !strings.Contains(body, "the snapshot server's own words") {
			t.Errorf("%s: %d %s", path, code, body)
		}
	}

	// The warning about Endpoints is a cluster's from v1.33 on, and not before.
	standInMinor = "32"
	defer func() { standInMinor = "37" }()
	older := httptest.NewServer(newFront(target, frozenAt))
	defer older.Close()
	resp, err := older.Client().Get(older.URL + "/kubernetes/api/v1/namespaces/shop/endpoints")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if w := resp.Header.Get("Warning"); w != "" {
		t.Errorf("a case from v1.32 warns %q", w)
	}
}

func TestTheSmallerCells(t *testing.T) {
	lb := `{"metadata":{"name":"lb"},"spec":{"type":"LoadBalancer","externalIPs":["203.0.113.9"]},"status":{"loadBalancer":{"ingress":[{"hostname":"b.example"},{"ip":"198.51.100.7"},{"hostname":"b.example"}]}}}`
	if got := serviceCells(decode(t, lb), frozenAt)[3]; got != "198.51.100.7,b.example,203.0.113.9" {
		t.Errorf("a load balancer's addresses, each once and in order: %q", got)
	}
	slice := printers["EndpointSlice"].cells(decode(t, `{"metadata":{"name":"s"},"ports":[{"name":""},{"port":80},{}]}`), frozenAt)
	if slice[2] != ",80,*" {
		t.Errorf("an endpoint slice's ports: %q", slice[2])
	}
	for ref, want := range map[string]string{
		`{"resource":"services","namespace":"default","name":"kubernetes"}`:                              "services/default/kubernetes",
		`{"group":"gateway.networking.k8s.io","resource":"Gateways","namespace":"edge","name":"public"}`: "gateways.gateway.networking.k8s.io/edge/public",
		`{"resource":"nodes","name":"worker"}`:                                                           "nodes/worker",
	} {
		if got := printers["IPAddress"].cells(decode(t, `{"metadata":{"name":"10.0.0.1"},"spec":{"parentRef":`+ref+`}}`), frozenAt)[1]; got != want {
			t.Errorf("the parent of an address: %q, want %q", got, want)
		}
	}
}

// A column of a custom resource is a JSONPath and a type, and the API server reads both in its own
// way. Each row here is what the apiextensions table convertor of v1.37 makes of one object.
func TestACustomResourcesCellsAreTheAPIServers(t *testing.T) {
	whole := asStored(map[string]any(decode(t, `{"metadata":{"labels":{"app":"web","node.kubernetes.io/instance-type":"m5.large"}},
		"spec":{"size":3,"big":1000000,"ratio":0.5,"on":true,"word":"True","five":"5","none":null,"parts":[{"weight":1},{"name":"blade","image":"i:1"}],"empty":[]},
		"status":{"at":"2026-10-07T09:07:26Z","soon":"2026-10-07T09:17:56Z","later":"2027-01-01T00:00:00Z","blank":"","conditions":[{"type":"A","status":"False"},{"type":"Ready","status":"True","n":2}]}}`)))
	for _, c := range []struct {
		path, kind string
		want       any
	}{
		{".spec.size", "integer", int64(3)}, {".spec.size", "number", float64(3)}, {".spec.size", "string", "3"}, {".spec.big", "string", "1000000"},
		{".spec.ratio", "number", 0.5}, {".spec.ratio", "integer", int64(0)}, {".spec.on", "boolean", true}, {".spec.on", "string", "true"},
		{".spec.parts[1].name", "string", "blade"}, {"$.spec.parts[1].name", "string", "blade"},
		{`.status.conditions[?(@.type=="Ready")].status`, "string", "True"}, {`.status.conditions[?(@.type!="A")].status`, "string", "True"}, {`.status.conditions[?(@.n==2)].type`, "string", "Ready"},
		// the forms a path of one's own writing did not read
		{`.metadata.labels.node\.kubernetes\.io/instance-type`, "string", "m5.large"}, {".metadata.labels['app']", "string", "web"},
		{".status.conditions[-1].type", "string", "Ready"}, {".status.conditions[-1:].type", "string", "Ready"}, {".status.conditions[0:1].type", "string", "A"},
		{".spec.parts[*].name", "string", "blade"}, {".spec..image", "string", "i:1"},
		// a value that is not of the column's type is no cell at all
		{".spec.word", "boolean", nil}, {".spec.five", "number", nil}, {".spec.five", "integer", nil}, {".spec.size", "date", nil}, {".spec.size", "boolean", nil},
		{".spec.nothing", "string", nil}, {".spec.parts[5].name", "string", nil}, {".spec.empty[*]", "string", nil}, {`.status.conditions[?(@.type=="Gone")].status`, "string", nil},
		// dates: how long ago, and what is said of one that is none, is empty, or has not come
		{".status.at", "date", "10m"}, {".status.soon", "date", "0s"}, {".status.later", "date", "<invalid>"}, {".status.blank", "date", "<unknown>"}, {".spec.word", "date", "<invalid>"},
		{".spec.unclosed[", "string", nil},
	} {
		if got := definedCell(whole, c.path, c.kind, frozenAt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s as %s: %#v, want %#v", c.path, c.kind, got, c.want)
		}
	}

	// The columns are those of the version asked for, and an age where it names none.
	definition := decode(t, `{"spec":{"versions":[{"name":"v1alpha1","additionalPrinterColumns":[{"name":"Old","type":"string","jsonPath":".x"}]},{"name":"v1"},
		{"name":"v2","additionalPrinterColumns":[{"name":"Size","type":"integer","jsonPath":".spec.size","priority":1,"format":"int32"}]}]}}`)
	if columns, paths := definedColumns(definition, "v2"); len(columns) != 2 || columns[1] != (column{Name: "Size", Type: "integer", Format: "int32", Priority: 1}) || !reflect.DeepEqual(paths, []string{".spec.size"}) {
		t.Errorf("the columns of v2: %v %v", columns, paths)
	}
	if columns, paths := definedColumns(definition, "v1"); len(columns) != 2 || columns[1].Name != "Age" || columns[1].Type != "date" || !reflect.DeepEqual(paths, []string{".metadata.creationTimestamp"}) {
		t.Errorf("the columns of a version that names none: %v %v", columns, paths)
	}
}

// What the review of this change found the new printers getting wrong, each beside what the API
// server's own code does with the same object.
func TestWhatAReviewFoundInTheNewPrinters(t *testing.T) {
	hosts := func(names ...string) []obj {
		var rules []obj
		for _, n := range names {
			rules = append(rules, obj{"host": n})
		}
		return rules
	}
	for want, rules := range map[string][]obj{
		"*": hosts(""), "a": hosts("a"), "a,b,c": hosts("a", "b", "c"), "a,b,c + 1 more...": hosts("a", "b", "c", ""), "a,b,c + 2 more...": hosts("a", "", "b", "c", "d"), "a,b": hosts("", "a", "", "b"),
	} {
		if got := ingressHosts(rules); got != want {
			t.Errorf("the hosts of %v: %q, want %q", rules, got, want)
		}
	}
	if got := ingressHosts(nil); got != "*" {
		t.Errorf("an Ingress with no rule: %q", got)
	}

	crd := decode(t, `{"metadata":{"name":"w.example.dev","creationTimestamp":"2026-10-07T09:07:26Z"},"spec":{"scope":"Namespaced","versions":[
		{"name":"v1alpha1","served":false},{"name":"v1beta1","served":true},{"name":"v1","served":true,"storage":true},{"name":"v2","served":true},{"name":"v10","served":true}]}}`)
	if got := printers["CustomResourceDefinition"].cells(crd, frozenAt)[2]; got != "v1(storage),v10,v1beta1,v2" {
		t.Errorf("the versions a definition serves, as letters: %q", got)
	}

	class := `{"metadata":{"name":"nginx","annotations":{"ingressclass.kubernetes.io/is-default-class":"true"}},"spec":{"controller":"c","parameters":{"kind":"P","name":"n"}}}`
	p := printers["IngressClass"]
	if got := p.cells(decode(t, class), frozenAt); got[0] != "nginx (default)" || got[2] != "P/n" {
		t.Errorf("the default ingress class: %v", got)
	}
	if got := p.as(35, p).cells(decode(t, class), frozenAt); got[0] != "nginx" {
		t.Errorf("the default ingress class on v1.35: %v", got)
	}
	if got := p.as(36, p).cells(decode(t, class), frozenAt); got[0] != "nginx (default)" {
		t.Errorf("the default ingress class on v1.36: %v", got)
	}
	if got := p.cells(decode(t, `{"metadata":{"name":"x"},"spec":{"parameters":{"kind":"P","apiGroup":"g.io","name":"n"}}}`), frozenAt); got[0] != "x" || got[2] != "P.g.io/n" {
		t.Errorf("an ingress class with parameters of a group: %v", got)
	}

	// Of several storage classes that say they are the default, v1.37 marks the one made last — the
	// one that is — and of two made in one second the one whose name comes first. Before it, each.
	classes := []obj{}
	for _, c := range []string{`"name":"old","creationTimestamp":"2026-10-01T00:00:00Z"`, `"name":"new-b","creationTimestamp":"2026-10-05T00:00:00Z"`, `"name":"new-a","creationTimestamp":"2026-10-05T00:00:00Z"`} {
		classes = append(classes, decode(t, `{"metadata":{`+c+`,"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},"provisioner":"p"}`))
	}
	classes = append(classes, decode(t, `{"metadata":{"name":"plain","creationTimestamp":"2026-10-06T00:00:00Z"},"provisioner":"p"}`))
	sc := printers["StorageClass"]
	var marked, before []string
	for _, c := range classes {
		cells := sc.cells(c, frozenAt)
		sc.inList(c, cells, classes)
		marked = append(marked, cells[0].(string))
		before = append(before, sc.as(36, sc).cells(c, frozenAt)[0].(string))
	}
	if !reflect.DeepEqual(marked, []string{"old", "new-b", "new-a (default)", "plain"}) || !reflect.DeepEqual(before, []string{"old (default)", "new-b (default)", "new-a (default)", "plain"}) {
		t.Errorf("several default storage classes: v1.37 %v, v1.36 %v", marked, before)
	}

	// An autoscaler's targets, metric by metric: each kind in its own words, the status at the same
	// place in the list, two of them and a count of the rest.
	for want, hpa := range map[string]string{
		"<none>":             `{}`,
		"cpu: <unknown>/80%": `{"spec":{"metrics":[{"type":"Resource","resource":{"name":"cpu","target":{"averageUtilization":80}}}]}}`,
		"cpu: 40%/80%":       `{"spec":{"metrics":[{"type":"Resource","resource":{"name":"cpu","target":{"averageUtilization":80}}}]},"status":{"currentMetrics":[{"type":"Resource","resource":{"name":"cpu","current":{"averageUtilization":40,"averageValue":"40m"}}}]}}`,
		"memory: 12Mi/64Mi":  `{"spec":{"metrics":[{"type":"Resource","resource":{"name":"memory","target":{"averageValue":"64Mi"}}}]},"status":{"currentMetrics":[{"type":"Resource","resource":{"name":"memory","current":{"averageValue":"12Mi"}}}]}}`,
		"7/10, 300m/1 (avg) + 2 more...": `{"spec":{"metrics":[{"type":"Pods","pods":{"target":{"averageValue":"10"}}},{"type":"External","external":{"target":{"averageValue":"1"}}},
			{"type":"Object","object":{"target":{"value":"5"}}},{"type":"Whatever"}]},
			"status":{"currentMetrics":[{"type":"Pods","pods":{"current":{"averageValue":"7"}}},{"type":"External","external":{"current":{"averageValue":"300m"}}}]}}`,
		"<unknown>/5, <nil>/10": `{"spec":{"metrics":[{"type":"Object","object":{"target":{"value":"5"}}},{"type":"Pods","pods":{"target":{"averageValue":"10"}}}]},
			"status":{"currentMetrics":[{"type":"Resource","resource":{"current":{}}},{"type":"Pods","pods":{"current":{}}}]}}`,
		"cpu: <unknown>/<auto>, <unknown type>": `{"spec":{"metrics":[{"type":"ContainerResource","containerResource":{"name":"cpu","target":{}}},{"type":"Other"}]}}`,
	} {
		if got := autoscalerTargets(decode(t, hpa)); got != want {
			t.Errorf("an autoscaler's targets: %q, want %q, for %s", got, want, hpa)
		}
	}

	// What is being deleted says so, whatever else it was.
	for kind, at := range map[string]int{"PersistentVolumeClaim": 1, "PersistentVolume": 4, "Job": 1} {
		if cells := printers[kind].cells(decode(t, `{"metadata":{"name":"x","deletionTimestamp":"2026-10-07T09:17:00Z"},"status":{"phase":"Bound"}}`), frozenAt); cells[at] != "Terminating" {
			t.Errorf("a %s that is being deleted: %v", kind, cells)
		}
	}
	if cells := printers["Job"].cells(decode(t, `{"metadata":{"name":"x","deletionTimestamp":"2026-10-07T09:17:00Z"},"status":{"conditions":[{"type":"Complete","status":"True"}]}}`), frozenAt); cells[1] != "Complete" {
		t.Errorf("a finished Job that is being deleted is still finished: %v", cells)
	}

	claim := printers["PersistentVolumeClaim"].cells(decode(t, `{"metadata":{"name":"c"},"spec":{"volumeName":"pv-1"},"status":{"phase":"Bound"}}`), frozenAt)
	if claim[3] != "0" {
		t.Errorf("a bound claim that does not say how much it got: %q", claim[3])
	}
	if unbound := printers["PersistentVolumeClaim"].cells(decode(t, `{"metadata":{"name":"c"},"status":{"phase":"Pending"}}`), frozenAt); unbound[3] != "" {
		t.Errorf("a claim that is not bound: %q", unbound[3])
	}
}

// Every kind this change prints: its API group and its headings, wide ones marked, as a v1.37 cluster
// printed them for test/replay-diff/kinds.
func TestTheHeadingsOfTheNewKinds(t *testing.T) {
	for kind, want := range map[string]string{
		"StatefulSet":              "apps: NAME READY AGE +CONTAINERS +IMAGES",
		"ReplicationController":    ": NAME DESIRED CURRENT READY AGE +CONTAINERS +IMAGES +SELECTOR",
		"Job":                      "batch: NAME STATUS COMPLETIONS DURATION AGE +CONTAINERS +IMAGES +SELECTOR",
		"CronJob":                  "batch: NAME SCHEDULE TIMEZONE SUSPEND ACTIVE LAST SCHEDULE AGE +CONTAINERS +IMAGES +SELECTOR",
		"PersistentVolumeClaim":    ": NAME STATUS VOLUME CAPACITY ACCESS MODES STORAGECLASS VOLUMEATTRIBUTESCLASS AGE +VOLUMEMODE",
		"PersistentVolume":         ": NAME CAPACITY ACCESS MODES RECLAIM POLICY STATUS CLAIM STORAGECLASS VOLUMEATTRIBUTESCLASS REASON AGE +VOLUMEMODE",
		"HorizontalPodAutoscaler":  "autoscaling: NAME REFERENCE TARGETS MINPODS MAXPODS REPLICAS AGE",
		"Ingress":                  "networking.k8s.io: NAME CLASS HOSTS ADDRESS PORTS AGE",
		"IngressClass":             "networking.k8s.io: NAME CONTROLLER PARAMETERS AGE",
		"ResourceQuota":            ": NAME REQUEST LIMIT AGE",
		"LimitRange":               ": NAME CREATED AT",
		"RuntimeClass":             "node.k8s.io: NAME HANDLER AGE",
		"CustomResourceDefinition": "apiextensions.k8s.io: NAME SCOPE VERSIONS CREATED AT +GROUP +KIND +SHORTNAMES +ESTABLISHED",
		"ServiceAccount":           ": NAME AGE",
	} {
		p := printers[kind]
		heads := []string{}
		for _, c := range p.columns {
			heads = append(heads, strings.Repeat("+", c.Priority)+strings.ToUpper(c.Name))
		}
		if got := p.group + ": " + strings.Join(heads, " "); got != want {
			t.Errorf("%s\n got %s\nwant %s", kind, got, want)
		}
		if cells := p.cells(decode(t, `{"metadata":{"name":"x"}}`), frozenAt); len(cells) != len(p.columns) {
			t.Errorf("%s: %d cells for %d columns", kind, len(cells), len(p.columns))
		}
	}
}

// The rows a v1.37 cluster printed for test/replay-diff/kinds, kind by kind.
func TestWhatHoldsStateRunsToAnEndStoresAndRoutes(t *testing.T) {
	const meta = `"metadata":{"name":"x","creationTimestamp":"2026-10-07T09:16:11Z"}`
	const tmpl = `"template":{"spec":{"containers":[{"name":"a","image":"i:1"},{"name":"b","image":"i:2"}]}}`
	for kind, c := range map[string]struct {
		object string
		want   []any
	}{
		"StatefulSet":           {`{` + meta + `,"spec":{"replicas":2,` + tmpl + `},"status":{"readyReplicas":1}}`, []any{"x", "1/2", "75s", "a,b", "i:1,i:2"}},
		"ReplicationController": {`{` + meta + `,"spec":{"replicas":1,"selector":{"app":"legacy"},` + tmpl + `},"status":{"replicas":1,"readyReplicas":1}}`, []any{"x", int64(1), int64(1), int64(1), "75s", "a,b", "i:1,i:2", "app=legacy"}},
		"CronJob": {`{` + meta + `,"spec":{"schedule":"0 3 1 1 *","suspend":false,"jobTemplate":{"spec":{` + tmpl + `}}},"status":{}}`,
			[]any{"x", "0 3 1 1 *", "<none>", "False", int64(0), "<none>", "75s", "a,b", "i:1,i:2", "<none>"}},
		"PersistentVolumeClaim": {`{` + meta + `,"spec":{"volumeName":"pvc-1","storageClassName":"standard","volumeMode":"Filesystem","accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"16Mi"}}},
			"status":{"phase":"Bound","capacity":{"storage":"16Mi"},"accessModes":["ReadOnlyMany","ReadWriteOnce"]}}`,
			[]any{"x", "Bound", "pvc-1", "16Mi", "RWO,ROX", "standard", "<unset>", "75s", "Filesystem"}},
		"PersistentVolume": {`{` + meta + `,"spec":{"capacity":{"storage":"32Mi"},"accessModes":["ReadWriteOnce","ReadOnlyMany"],"persistentVolumeReclaimPolicy":"Retain","storageClassName":"manual","volumeMode":"Filesystem",
			"claimRef":{"namespace":"kinds","name":"data-db-0"}},"status":{"phase":"Bound"}}`,
			[]any{"x", "32Mi", "RWO,ROX", "Retain", "Bound", "kinds/data-db-0", "manual", "<unset>", "", "75s", "Filesystem"}},
		"HorizontalPodAutoscaler": {`{` + meta + `,"spec":{"scaleTargetRef":{"kind":"Deployment","name":"web"},"minReplicas":2,"maxReplicas":5,"metrics":[
			{"type":"Resource","resource":{"name":"cpu","target":{"type":"Utilization","averageUtilization":80}}},{"type":"Resource","resource":{"name":"memory","target":{"type":"AverageValue","averageValue":"100Mi"}}}]},
			"status":{"currentReplicas":2}}`, []any{"x", "Deployment/web", "cpu: <unknown>/80%, memory: <unknown>/100Mi", "2", int64(5), int64(2), "75s"}},
		"Ingress": {`{` + meta + `,"spec":{"ingressClassName":"fixture","tls":[{"hosts":["a"]}],"rules":[{"host":"web.example.test"},{"host":"api.example.test"}]}}`,
			[]any{"x", "fixture", "web.example.test,api.example.test", "", "80, 443", "75s"}},
		"IngressClass":  {`{` + meta + `,"spec":{"controller":"c/none","parameters":{"apiGroup":"k8s.example.test","kind":"Params","name":"p"}}}`, []any{"x", "c/none", "Params.k8s.example.test/p", "75s"}},
		"ResourceQuota": {`{` + meta + `,"status":{"hard":{"pods":"50","configmaps":"20","limits.memory":"1Gi"},"used":{"pods":"16","configmaps":"2","limits.memory":"0"}}}`, []any{"x", "configmaps: 2/20, pods: 16/50", "limits.memory: 0/1Gi", "75s"}},
		"RuntimeClass":  {`{` + meta + `,"handler":"runc"}`, []any{"x", "runc", "75s"}},
		"CustomResourceDefinition": {`{` + meta + `,"spec":{"group":"g.example.test","scope":"Namespaced","names":{"kind":"Widget","shortNames":["wd","w"]},"versions":[{"name":"v1beta1","served":true},{"name":"v1","served":true,"storage":true},{"name":"v1alpha1"}]},
			"status":{"conditions":[{"type":"Established","status":"True"}]}}`, []any{"x", "Namespaced", "v1(storage),v1beta1", "2026-10-07T09:16:11Z", "g.example.test", "Widget", "wd,w", true}},
	} {
		p := printers[kind]
		if got := p.cells(decode(t, c.object), frozenAt); !reflect.DeepEqual(got, c.want) || len(got) != len(p.columns) {
			t.Errorf("%s (%d columns):\n got %q\nwant %q", kind, len(p.columns), got, c.want)
		}
	}
	// A job: what it came to, how much of it is done, and how long it took — or has taken, if it has
	// not ended, which is the whole of a failed job's life.
	for name, c := range map[string]struct {
		job  string
		want []any
	}{
		"complete": {`"spec":{"completions":3,"selector":{"matchLabels":{"uid":"1"}},` + tmpl + `},"status":{"succeeded":3,"startTime":"2026-10-07T09:16:11Z","completionTime":"2026-10-07T09:16:37Z","conditions":[{"type":"Complete","status":"True"}]}`,
			[]any{"x", "Complete", "3/3", "26s", "75s", "a,b", "i:1,i:2", "uid=1"}},
		"failed":    {`"spec":{"completions":1,` + tmpl + `},"status":{"startTime":"2026-10-07T09:16:11Z","conditions":[{"type":"Failed","status":"True"}]}`, []any{"x", "Failed", "0/1", "75s", "75s", "a,b", "i:1,i:2", ""}},
		"a queue":   {`"spec":{"parallelism":4,` + tmpl + `},"status":{"succeeded":2,"conditions":[{"type":"Complete","status":"False"}]}`, []any{"x", "Running", "2/1 of 4", "", "75s", "a,b", "i:1,i:2", ""}},
		"suspended": {`"spec":{` + tmpl + `},"status":{"conditions":[{"type":"Suspended","status":"True"}]}`, []any{"x", "Suspended", "0/1", "", "75s", "a,b", "i:1,i:2", ""}},
	} {
		if got := jobCells(decode(t, `{`+meta+`,`+c.job+`}`), frozenAt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("a job, %s:\n got %q\nwant %q", name, got, c.want)
		}
	}
	// A claim that has no volume yet says nothing of what it has not got.
	if got := printers["PersistentVolumeClaim"].cells(decode(t, `{`+meta+`,"spec":{"storageClassName":"standard"},"status":{"phase":"Pending"}}`), frozenAt); got[2] != "" || got[3] != "" || got[4] != "" || got[8] != "<unset>" {
		t.Errorf("a claim that is pending: %q", got)
	}
}

// A snapshot holds each line of a log with the time the kubelet stamped on it. What a cluster does
// with those times — and what the snapshot server does not — is done by the front.
func TestALogIsReadByItsTimes(t *testing.T) {
	upstream, asked := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	answers := newFront(target, frozenAt)
	answers.served = takenUp.Add(-time.Second)
	front := httptest.NewServer(answers)
	defer front.Close()
	log := shopPods + "/stamped/log?container=app"

	for query, want := range map[string]string{
		"":                 "one\ntwo\nthree\n\nlate\nunable to retrieve container logs\n",
		"&timestamps=true": "2026-10-07T09:05:26.123456789Z one\n2026-10-07T09:10:26.000000001Z two\n2026-10-07T09:16:26.5Z three\n2026-10-07T09:17:25.999999999Z \n2026-10-07T09:17:27.000001000Z late\nunable to retrieve container logs\n",
		// since a time: the lines stamped at it or after it, and the one that has no time
		"&sinceTime=2026-10-07T09:10:26Z": "two\nthree\n\nlate\nunable to retrieve container logs\n",
		"&sinceTime=2026-10-07T09:10:27Z": "three\n\nlate\nunable to retrieve container logs\n",
		// since so many seconds: counted back from the freeze, which is the case's now
		"&sinceSeconds=120":                  "three\n\nlate\nunable to retrieve container logs\n",
		"&sinceSeconds=600":                  "two\nthree\n\nlate\nunable to retrieve container logs\n",
		"&sinceSeconds=600&tailLines=3":      "\nlate\nunable to retrieve container logs\n",
		"&tailLines=4&timestamps=true":       "2026-10-07T09:16:26.5Z three\n2026-10-07T09:17:25.999999999Z \n2026-10-07T09:17:27.000001000Z late\nunable to retrieve container logs\n",
		"&limitBytes=7":                      "one\ntwo",
		"&tailLines=2&limitBytes=3":          "lat", // the last lines, and of those the first bytes
		"&tailLines=0":                       "",
		"&sinceTime=yesterday&tailLines=two": "one\ntwo\nthree\n\nlate\nunable to retrieve container logs\n", // what cannot be read is not obeyed
	} {
		if code, body := ask(t, front, http.MethodGet, log+query, "*/*"); code != http.StatusOK || body != want {
			t.Errorf("logs%s: %d\n got %q\nwant %q", query, code, body, want)
		}
	}
	// What the kubelet says where it has no log to give is said whole, whatever was asked for: it is
	// no line of a log, to be counted or cut — and it keeps its first word, which is no time.
	for _, query := range []string{"", "&tailLines=0", "&limitBytes=7", "&timestamps=true", "&sinceSeconds=5", "&follow=true&tailLines=0"} {
		if code, body := ask(t, front, http.MethodGet, shopPods+"/gone/log?container=app"+query, "*/*"); code != http.StatusOK || body != "unable to retrieve container logs for containerd://abc" {
			t.Errorf("logs%s of a container whose log is gone: %d %q", query, code, body)
		}
	}
	// `logs -f` is read by its times as well, and ends.
	if code, body := ask(t, front, http.MethodGet, log+"&follow=true&tailLines=2&timestamps=true", "*/*"); code != http.StatusOK || body != "2026-10-07T09:17:27.000001000Z late\nunable to retrieve container logs\n" {
		t.Errorf("logs -f --tail=2 --timestamps: %d %q", code, body)
	}
	for _, a := range *asked {
		if strings.Contains(a, "/stamped/log") && (!strings.Contains(a, "timestamps=true") || strings.Contains(a, "since") || strings.Contains(a, "tailLines") || strings.Contains(a, "limitBytes")) {
			t.Errorf("the snapshot server was asked %s", a)
		}
	}

	// A container that has not started has no log, and a cluster says what it is waiting for.
	for container, want := range map[string]string{
		"app":    `container "app" in pod "waiting" is waiting to start: PodInitializing`,
		"puller": `container "puller" in pod "waiting" is waiting to start: trying and failing to pull image`,
		"slow":   `container "slow" in pod "waiting" is waiting to start: image can't be pulled`,
		// one that was added to the pod to look at it, and has not started either
		"debugger": `container "debugger" in pod "waiting" is waiting to start: ContainerCreating`,
	} {
		code, body := ask(t, front, http.MethodGet, shopPods+"/waiting/log?container="+container, asObjects)
		if o := decode(t, body); code != http.StatusBadRequest || o.str("reason") != "BadRequest" || o.str("message") != want {
			t.Errorf("the log of %s: %d %s", container, code, body)
		}
	}
	// One that waits to be restarted has run before: whatever the snapshot server has of it is its to give.
	for _, query := range []string{"?container=migrate", "?container=migrate&previous=true", "?container=nosuch"} {
		if code, body := ask(t, front, http.MethodGet, shopPods+"/waiting/log"+query, asObjects); code != http.StatusBadRequest || !strings.Contains(body, "the snapshot server's own words") {
			t.Errorf("logs%s: %d %s", query, code, body)
		}
	}
}
