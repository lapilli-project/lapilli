package replay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// The freeze of the stand-in case: what an age in a table is counted to.
var frozenAt = time.Date(2026, 10, 7, 9, 17, 26, 0, time.UTC)

// takenUp is when the stand-in snapshot server read the logs it serves: after the case began to be
// served, which is some time after the freeze.
var takenUp = frozenAt.Add(72*time.Hour + 795458*time.Microsecond)

// The minor version the stand-in says its cluster is, and how often it was asked to send a caller
// somewhere else and the caller went.
var (
	standInMinor = "37"
	elsewhere    = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed++ }))
	followed     int
)

// A stand-in for the snapshot server with its one relevant habit: it ignores fieldSelector. It answers
// a list as objects or as a table, as asked, and records what it was asked.
func snapshotServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	type pod struct{ name, node, phase string }
	pods := []pod{{"cache-1", "worker", "Running"}, {"api-1", "worker2", "Running"}, {"api-2", "worker2", "Pending"}, {"job-1", "", "Pending"}}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/kubernetes/version" {
			io.WriteString(w, `{"major":"1","minor":"`+standInMinor+`","gitVersion":"v1.`+standInMinor+`.0"}`)
			return
		}
		asked = append(asked, r.Method+" "+r.URL.RequestURI()+" ["+r.Header.Get("Accept")+"]")
		w.Header().Set("Content-Type", "application/json")
		const roles = "/kubernetes/apis/rbac.authorization.k8s.io/v1/namespaces/shop/roles"
		switch {
		// The snapshot server's habits that the front makes up for, beyond the selector: it cannot find
		// an object whose name has a colon in it, though it lists it; and it serves a Secret as `freeze`
		// left it, with a value that is not base64.
		case strings.HasPrefix(r.URL.Path, roles+"/"), strings.HasPrefix(r.URL.Path, shopPods+"/ghost"), r.URL.Path == "/kubernetes/api/v1/pods/cache-1":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"kind":"Status","status":"Failure","code":404,"message":"the server could not find the requested resource"}`)
		// More of its habits, each met once: a name that is not the one asked for, an answer that is
		// "go and ask there", a list with a hole in it, and a kind of its own that shares a name with
		// one of Kubernetes's.
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/configmaps/zeta-alias":
			io.WriteString(w, `{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"zeta","namespace":"shop"}}`)
		case r.URL.Path == shopPods+"/moved":
			http.Redirect(w, r, elsewhere.URL+"/kubernetes/api/v1/namespaces/shop/pods/moved", http.StatusFound)
		case r.URL.Path == "/kubernetes/api/v1/pods", r.URL.Path == "/kubernetes/api/v1/namespaces/odd/pods":
			io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[null,`+
				`{"metadata":{"name":"cache-1","namespace":"shop-db"},"spec":{"containers":[{"name":"app"}]}},{"metadata":{"name":"cache-1","namespace":"shop"},"spec":{"containers":[{"name":"app"}]}}]}`)
		case r.URL.Path == "/kubernetes/apis/serving.knative.dev/v1/namespaces/shop/services" && strings.Contains(r.Header.Get("Accept"), "as=Table"):
			io.WriteString(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Name"},{"name":"URL"}],"rows":[{"cells":["hello","http://hello.shop"],"object":{"metadata":{"name":"hello","namespace":"shop"}}}]}`)
		case r.URL.Path == "/kubernetes/apis/serving.knative.dev/v1/namespaces/shop/services/hello":
			io.WriteString(w, `{"kind":"Service","apiVersion":"serving.knative.dev/v1","metadata":{"name":"hello","namespace":"shop"},"spec":{"count":3},`+
				`"status":{"url":"http://hello.shop","latest":"2026-10-07T09:07:26Z","conditions":[{"type":"Routes","status":"False"},{"type":"Ready","status":"True"}]}}`)
		case r.URL.Path == "/kubernetes/apis/serving.knative.dev/v1/namespaces/shop/services":
			io.WriteString(w, `{"kind":"ServiceList","apiVersion":"serving.knative.dev/v1","metadata":{},"items":[{"metadata":{"name":"hello","namespace":"shop"},"spec":{"count":3},`+
				`"status":{"url":"http://hello.shop","latest":"2026-10-07T09:07:26Z","conditions":[{"type":"Routes","status":"False"},{"type":"Ready","status":"True"}]}}]}`)
		case r.URL.Path == "/kubernetes/apis/apiextensions.k8s.io/v1/customresourcedefinitions":
			io.WriteString(w, `{"kind":"CustomResourceDefinitionList","apiVersion":"apiextensions.k8s.io/v1","items":[{"metadata":{"name":"services.serving.knative.dev"},`+
				`"spec":{"group":"serving.knative.dev","names":{"kind":"Service"},"versions":[{"name":"v1alpha1","additionalPrinterColumns":[{"name":"Old","type":"string","jsonPath":".x"}]},`+
				`{"name":"v1","additionalPrinterColumns":[{"name":"URL","type":"string","jsonPath":".status.url"},{"name":"Ready","type":"string","jsonPath":".status.conditions[?(@.type==\"Ready\")].status"},`+
				`{"name":"Latest","type":"date","jsonPath":".status.latest"},{"name":"Count","type":"integer","jsonPath":".spec.count","priority":1},{"name":"Missing","type":"string","jsonPath":".spec.nothing"}]}]}},`+
				`{"metadata":{"name":"widgets.example.dev"},"spec":{"group":"example.dev","names":{"kind":"Widget"},"versions":[{"name":"v1","additionalPrinterColumns":[`+
				`{"name":"Tier","type":"string","jsonPath":".metadata.labels.example\\.dev/tier"},{"name":"Since","type":"date","jsonPath":".spec.since"},{"name":"Last","type":"string","jsonPath":".spec.parts[-1].name"}]}]}}]}`)
		// A custom resource of a name all its own. The snapshot server prints its columns too, with a
		// JSONPath of its own and a date as a date.
		case r.URL.Path == "/kubernetes/apis/example.dev/v1/namespaces/shop/widgets" && strings.Contains(r.Header.Get("Accept"), "as=Table"):
			io.WriteString(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Name"},{"name":"Tier"},{"name":"Since","type":"date"},{"name":"Last"}],`+
				`"rows":[{"cells":["left","","2026-10-07T09:07:26Z",""],"object":{"metadata":{"name":"left","namespace":"shop"}}}]}`)
		case r.URL.Path == "/kubernetes/apis/example.dev/v1/namespaces/shop/widgets":
			io.WriteString(w, `{"kind":"WidgetList","apiVersion":"example.dev/v1","metadata":{},"items":[{"metadata":{"name":"left","namespace":"shop","labels":{"example.dev/tier":"gold"}},`+
				`"spec":{"since":"2026-10-07T09:07:26Z","parts":[{"name":"handle"},{"name":"blade"}]}}]}`)
		// The same events under the other group that serves them, with its names for the same things.
		case r.URL.Path == "/kubernetes/apis/events.k8s.io/v1/namespaces/shop/events" && strings.Contains(r.Header.Get("Accept"), "as=Table"):
			io.WriteString(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Type"},{"name":"Reason"}],"rows":[{"cells":["Warning","BackOff"],"object":{"metadata":{"name":"web.1","namespace":"shop"}}}]}`)
		case r.URL.Path == "/kubernetes/apis/events.k8s.io/v1/namespaces/shop/events":
			io.WriteString(w, `{"kind":"EventList","apiVersion":"events.k8s.io/v1","metadata":{},"items":[{"metadata":{"name":"web.1","namespace":"shop"},"regarding":{"kind":"Pod","name":"web","fieldPath":"spec.containers{app}"},`+
				`"note":"Back-off restarting failed container","reason":"BackOff","type":"Warning","deprecatedFirstTimestamp":"2026-10-07T09:07:26Z","deprecatedLastTimestamp":"2026-10-07T09:16:26Z","deprecatedCount":7,`+
				`"deprecatedSource":{"component":"kubelet","host":"w1"}}]}`)
		// A kind named like one of Kubernetes's, in a group that is no custom resource's: an aggregated
		// API's. Nothing here knows how a cluster prints it, and the snapshot server's table stands.
		case r.URL.Path == "/kubernetes/apis/metrics.example/v1/namespaces/shop/pods" && strings.Contains(r.Header.Get("Accept"), "as=Table"):
			io.WriteString(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Name"},{"name":"CPU"}],"rows":[{"cells":["web","250m"],"object":{"metadata":{"name":"web","namespace":"shop"}}}]}`)
		case r.URL.Path == "/kubernetes/apis/metrics.example/v1/namespaces/shop/pods":
			io.WriteString(w, `{"kind":"PodList","apiVersion":"metrics.example/v1","metadata":{},"items":[{"metadata":{"name":"web","namespace":"shop"},"usage":{"cpu":"250m"}}]}`)
		// A log the kubelet could not give: what it says in its place, which has no time on it.
		case strings.HasSuffix(r.URL.Path, "/gone/log"):
			w.Header().Set("Content-Type", "text/plain")
			if r.URL.Query().Get("timestamps") == "true" {
				io.WriteString(w, takenUp.Format("2006-01-02T15:04:05.000000000Z07:00")+" ")
			} else {
				io.WriteString(w, "to retrieve container logs for containerd://abc") // its first word taken for a time
				return
			}
			io.WriteString(w, "unable to retrieve container logs for containerd://abc")
		// A log as a snapshot holds it: every line with the time the kubelet stamped on it, given back
		// with those times or without, as asked — and one line the collector wrote in place of a log,
		// which has none and is given the time the snapshot server took the file up.
		case strings.HasSuffix(r.URL.Path, "/stamped/log"):
			w.Header().Set("Content-Type", "text/plain")
			for _, line := range []string{"2026-10-07T09:05:26.123456789Z one", "2026-10-07T09:10:26.000000001Z two", "2026-10-07T09:16:26.5Z three", "2026-10-07T09:17:25.999999999Z ", "2026-10-07T09:17:27.000001000Z late"} {
				if r.URL.Query().Get("timestamps") != "true" {
					_, line, _ = strings.Cut(line, " ")
				}
				io.WriteString(w, line+"\n")
			}
			if r.URL.Query().Get("timestamps") == "true" {
				io.WriteString(w, takenUp.Format("2006-01-02T15:04:05.000000000Z07:00")+" ")
			}
			io.WriteString(w, "unable to retrieve container logs\n")
		case strings.HasSuffix(r.URL.Path, "/waiting/log"):
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"kind":"Status","status":"Failure","code":400,"message":"the snapshot server's own words"}`)
		case r.URL.Path == shopPods+"/waiting":
			io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"waiting","namespace":"shop"},"spec":{"initContainers":[{"name":"migrate"}],"containers":[{"name":"app"},{"name":"puller"},{"name":"slow"}],"ephemeralContainers":[{"name":"debugger"}]},`+
				`"status":{"initContainerStatuses":[{"name":"migrate","state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":1}}}],`+
				`"ephemeralContainerStatuses":[{"name":"debugger","state":{"waiting":{"reason":"ContainerCreating"}}}],`+
				`"containerStatuses":[{"name":"app","state":{"waiting":{"reason":"PodInitializing"}}},{"name":"puller","state":{"waiting":{"reason":"ImagePullBackOff"}}},{"name":"slow","state":{"waiting":{"reason":"ErrImagePull"}}}]}}`)
		case r.URL.Path == "/kubernetes/apis/storage.k8s.io/v1/storageclasses/standard":
			io.WriteString(w, `{"kind":"StorageClass","apiVersion":"storage.k8s.io/v1","metadata":{"name":"standard","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},"provisioner":"p"}`)
		case r.URL.Path == "/kubernetes/apis/storage.k8s.io/v1/storageclasses":
			io.WriteString(w, `{"kind":"StorageClassList","apiVersion":"storage.k8s.io/v1","metadata":{},"items":[{"metadata":{"name":"standard","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},"provisioner":"p"}]}`)
		case r.URL.Path == "/kubernetes/apis/flowcontrol.apiserver.k8s.io/v1/flowschemas":
			io.WriteString(w, `{"kind":"FlowSchemaList","apiVersion":"flowcontrol.apiserver.k8s.io/v1","metadata":{},"items":[`+
				`{"metadata":{"name":"a-last"},"spec":{"matchingPrecedence":9000}},{"metadata":{"name":"z-first"},"spec":{"matchingPrecedence":1}}]}`)
		case r.URL.Path == shopPods+"/crashy":
			io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"crashy","namespace":"shop"},"spec":{"containers":[{"name":"app"}]},`+
				`"status":{"containerStatuses":[{"name":"app","restartCount":3,"lastState":{"terminated":{"exitCode":1}}}]}}`)
		case strings.HasSuffix(r.URL.Path, "/crashy/log"), strings.HasSuffix(r.URL.Path, "/cache-1/log") && r.URL.Query().Get("container") == "nosuch":
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"kind":"Status","status":"Failure","code":400,"message":"the snapshot server's own words"}`)
		case r.URL.Path == roles:
			io.WriteString(w, `{"kind":"RoleList","apiVersion":"rbac.authorization.k8s.io/v1","metadata":{"resourceVersion":"1"},"items":[`+
				`{"metadata":{"name":"system:reader","namespace":"shop","creationTimestamp":"2026-10-07T09:04:26Z"},"rules":[{"verbs":["get"]}]}]}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/secrets/token":
			io.WriteString(w, `{"kind":"Secret","apiVersion":"v1","metadata":{"name":"token","namespace":"shop"},"type":"Opaque","data":{"password":"REDACTED-BY-LAPILLI"}}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/secrets" && strings.Contains(r.Header.Get("Accept"), "as=Table"):
			io.WriteString(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Name","type":"string"},{"name":"Type","type":"string"},{"name":"Data","type":"string"},{"name":"Age","type":"string"}],"rows":[`+
				`{"cells":["token","Opaque","2","1h"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"token","namespace":"shop"}}}]}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/secrets":
			io.WriteString(w, `{"kind":"SecretList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[{"metadata":{"name":"token","namespace":"shop"},"type":"Opaque","data":{"password":"REDACTED-BY-LAPILLI","note":"aGVsbG8="}}]}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/pods/cache-1":
			io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"cache-1","namespace":"shop","resourceVersion":"7","creationTimestamp":"2026-10-07T09:04:26Z"},"spec":{"nodeName":"worker","containers":[{"name":"app"}]},`+
				`"status":{"phase":"Running","podIPs":[{"ip":"10.244.3.2"}],"containerStatuses":[{"name":"app","ready":true,"restartCount":0,"state":{"running":{}}}]}}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/configmaps/settings", r.URL.Path == "/kubernetes/api/v1/namespaces/shop/configmaps/missing":
			if strings.HasSuffix(r.URL.Path, "missing") {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"kind":"Status","status":"Failure","code":404}`)
				return
			}
			io.WriteString(w, `{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"settings","namespace":"shop","creationTimestamp":"2026-10-07T07:17:26Z"},"data":{"MODE":"per-task"}}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/configmaps" && strings.Contains(r.Header.Get("Accept"), "as=Table"):
			// The snapshot server's own table: its columns, its idea of an age, metadata in the rows.
			io.WriteString(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Name","type":"string"},{"name":"Data","type":"string"},{"name":"Age","type":"string"}],"rows":[`+
				`{"cells":["zeta","0","1h"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"zeta","namespace":"shop"}}},`+
				`{"cells":["settings","1","1h"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"settings","namespace":"shop"}}}]}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/configmaps":
			io.WriteString(w, `{"kind":"ConfigMapList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[`+
				`{"metadata":{"name":"zeta","namespace":"shop","creationTimestamp":"2026-10-07T09:15:26Z"}},`+
				`{"metadata":{"name":"settings","namespace":"shop","creationTimestamp":"2026-10-07T07:17:26Z"},"data":{"MODE":"per-task"}}]}`)
		case strings.HasPrefix(r.URL.Path, "/kubernetes/api/v1/namespaces/gone/"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"kind":"Status","status":"Failure","code":404}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/events":
			io.WriteString(w, `{"kind":"EventList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[`+
				`{"metadata":{"name":"e1","namespace":"shop"},"involvedObject":{"kind":"Pod","name":"cache-1","fieldPath":"spec.containers{app}"},"reason":"Pulled","count":3,"type":"Normal","message":" Container image already present \n",`+
				`"firstTimestamp":"2026-10-07T09:05:26Z","lastTimestamp":"2026-10-07T09:16:56Z","source":{"component":"kubelet","host":"worker"}},`+
				`{"metadata":{"name":"e2","namespace":"shop"},"involvedObject":{"kind":"Pod","name":"api-1"},"reason":"Pulled","count":1,"lastTimestamp":"2026-10-07T09:10:26Z"},`+
				`{"metadata":{"name":"e3","namespace":"shop"},"involvedObject":{"kind":"ReplicaSet","name":"api"},"reason":"SuccessfulCreate","eventTime":"2026-10-07T09:07:26.000000Z","reportingComponent":"replicaset-controller"}]}`)
		case strings.HasSuffix(r.URL.Path, "/api-1/log"):
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "one\ntwo\nthree\nfour\nfive, with no line ending")
		case strings.HasSuffix(r.URL.Path, "/cache-1/log") && r.URL.Query().Get("previous") == "true":
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"kind":"Status","status":"Failure","code":400}`)
		case strings.HasSuffix(r.URL.Path, "/nope/log"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"kind":"Status","status":"Failure","code":404,"message":"line one\nline two\nline three"}`)
		case r.URL.Path != "/kubernetes/api/v1/namespaces/shop/pods":
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "a stream of log lines\n")
		case strings.Contains(r.Header.Get("Accept"), "as=Table"):
			rows := []string{}
			for _, p := range pods {
				rows = append(rows, `{"cells":["`+p.name+`","1/1","`+p.phase+`"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"`+p.name+`","namespace":"shop"}}}`)
			}
			io.WriteString(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Name"}],"rows":[`+strings.Join(rows, ",")+`]}`)
		default:
			items := []string{}
			for _, p := range pods {
				node := `"nodeName":"` + p.node + `",`
				if p.node == "" {
					node = ""
				}
				items = append(items, `{"metadata":{"name":"`+p.name+`","namespace":"shop","creationTimestamp":"2026-10-07T09:04:26Z"},"spec":{`+node+`"hostNetwork":false,"containers":[{"name":"app"}]},"status":{"phase":"`+p.phase+`"}}`)
			}
			io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[`+strings.Join(items, ",")+`]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func ask(t *testing.T, front *httptest.Server, method, path, accept string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, front.URL+path, nil)
	req.Header.Set("Accept", accept)
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func names(t *testing.T, body, list string) []string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	var entries []json.RawMessage
	json.Unmarshal(doc[list], &entries)
	out := []string{}
	for _, raw := range entries {
		var e any
		json.Unmarshal(raw, &e)
		if list == "rows" {
			e = e.(map[string]any)["object"]
		}
		out = append(out, field(e, []string{"metadata", "name"}))
	}
	return out
}

const (
	asObjects = "application/json"
	asTable   = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json"
	shopPods  = "/kubernetes/api/v1/namespaces/shop/pods"
)

func TestAFieldSelectorFiltersWhatTheSnapshotServerWouldNot(t *testing.T) {
	upstream, asked := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target, frozenAt))
	defer front.Close()

	for selector, want := range map[string][]string{
		"spec.nodeName=worker2":                       {"api-1", "api-2"},
		"spec.nodeName==worker2":                      {"api-1", "api-2"},
		"spec.nodeName!=worker2":                      {"cache-1", "job-1"},
		"status.phase!=Running":                       {"api-2", "job-1"},
		"spec.nodeName=worker2,status.phase=Running":  {"api-1"},
		"spec.nodeName=":                              {"job-1"}, // never scheduled: the field is not there
		"metadata.name=cache-1":                       {"cache-1"},
		"metadata.namespace=shop":                     {"cache-1", "api-1", "api-2", "job-1"},
		"spec.hostNetwork=false,status.phase=Pending": {"api-2", "job-1"},
		"spec.nodeName=nowhere":                       {},
	} {
		path := shopPods + "?limit=500&labelSelector=app%3Dx&fieldSelector=" + url.QueryEscape(selector)
		for _, shape := range []struct{ accept, list, kind string }{{asObjects, "items", `"kind":"PodList"`}, {asTable, "rows", `"kind":"Table"`}} {
			code, body := ask(t, front, http.MethodGet, path, shape.accept)
			want := append([]string{}, want...)
			sort.Strings(want) // the order a cluster lists in, not the snapshot server's
			if got := names(t, body, shape.list); code != http.StatusOK || !reflect.DeepEqual(got, want) || !strings.Contains(body, shape.kind) {
				t.Errorf("%s as %s: %d %v, want %v", selector, shape.list, code, got, want)
			}
		}
	}
	// The filtered table is a cluster's table of what is left.
	if _, body := ask(t, front, http.MethodGet, shopPods+"?fieldSelector=spec.nodeName%3Dworker", asTable); !strings.Contains(body, `{"name":"Node","type":"string","format":"","priority":1}`) || !strings.Contains(body, `"cells":["cache-1","0/1","Running","0","13m","\u003cnone\u003e","worker",`) {
		t.Errorf("filtered table: %s", body)
	}
	// What `kubectl describe pod` asks: this object's events and nobody else's.
	if _, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/events?fieldSelector="+url.QueryEscape("involvedObject.name=cache-1,involvedObject.kind=Pod"), asObjects); !reflect.DeepEqual(names(t, body, "items"), []string{"e1"}) || !strings.Contains(body, `"resourceVersion":"1"`) {
		t.Errorf("events of one pod: %s", body)
	}
	if _, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/events?fieldSelector=count%3D3", asObjects); !reflect.DeepEqual(names(t, body, "items"), []string{"e1"}) {
		t.Errorf("a numeric field: %s", body)
	}

	// The unfiltered list is fetched whole, with the label selector and without the paging.
	for _, a := range *asked {
		if strings.Contains(a, "fieldSelector") || strings.Contains(a, "limit=") {
			t.Errorf("the snapshot server was asked %s", a)
		}
	}
	if first := (*asked)[0]; !strings.Contains(first, "labelSelector=app%3Dx") || !strings.HasSuffix(first, "["+asObjects+"]") {
		t.Errorf("the first request upstream was %s", first)
	}
}

func TestWhatIsLeftToTheSnapshotServer(t *testing.T) {
	upstream, asked := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target, frozenAt))
	defer front.Close()

	// A list with no selector keeps everything it had, in the order a cluster lists it — the stand-in
	// has cache-1 first — and is asked for whole.
	code, through := ask(t, front, http.MethodGet, shopPods+"?limit=500", asObjects)
	if got := names(t, through, "items"); code != http.StatusOK || !reflect.DeepEqual(got, []string{"api-1", "api-2", "cache-1", "job-1"}) || !strings.Contains(through, `"kind":"PodList"`) || !strings.Contains(through, `"hostNetwork":false`) {
		t.Errorf("a list with no selector: %d %s", code, through)
	}
	if last := (*asked)[len(*asked)-1]; last != "GET "+shopPods+" ["+asObjects+"]" {
		t.Errorf("upstream was asked %s", last)
	}
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/pods/cache-1/log?follow=true", "*/*"); code != http.StatusOK || body != "a stream of log lines\n" {
		t.Errorf("a log: %d %q", code, body)
	}
	// A watch with no selector is a stream, and is the snapshot server's to answer however it does.
	// (One with a selector is answered here: TestAWatchThatNamesItsObjectIsOfThatObject.)
	if code, body := ask(t, front, http.MethodGet, shopPods+"?watch=true", asObjects); code != http.StatusOK || len(names(t, body, "items")) != 4 {
		t.Errorf("a watch was changed: %d %s", code, body)
	}
	// A selector on one object, or on a list that is not there, has nothing to filter.
	if code, body := ask(t, front, http.MethodGet, shopPods+"/cache-1?fieldSelector=spec.nodeName%3Dworker2", asObjects); code != http.StatusOK || !strings.Contains(body, `"kind":"Pod"`) {
		t.Errorf("a single object: %d %s", code, body)
	}
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/gone/pods?fieldSelector=a%3Db", asObjects); code != http.StatusNotFound || !strings.Contains(body, `"code":404`) {
		t.Errorf("an upstream error: %d %s", code, body)
	}
	// A selector that cannot be read is refused in the API's own shape.
	for _, bad := range []string{"spec.nodeName", "=worker2", "a~b"} {
		code, body := ask(t, front, http.MethodGet, shopPods+"?fieldSelector="+url.QueryEscape(bad), asObjects)
		if code != http.StatusBadRequest || !strings.Contains(body, `"kind":"Status"`) || !strings.Contains(body, `"reason":"BadRequest"`) {
			t.Errorf("selector %q: %d %s", bad, code, body)
		}
	}
}

// `kubectl logs --tail=N`: the snapshot server returns the whole log; the last N lines of a frozen log
// are what a live cluster would have returned at the freeze.
func TestTheTailOfALogIsItsLastLines(t *testing.T) {
	upstream, asked := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target, frozenAt))
	defer front.Close()
	log := "/kubernetes/api/v1/namespaces/shop/pods/api-1/log"

	for query, want := range map[string]string{
		"?tailLines=2":                "four\nfive, with no line ending",
		"?tailLines=1&container=main": "five, with no line ending",
		"?tailLines=4":                "two\nthree\nfour\nfive, with no line ending",
		"?tailLines=5":                "one\ntwo\nthree\nfour\nfive, with no line ending",
		"?tailLines=500":              "one\ntwo\nthree\nfour\nfive, with no line ending",
		"?tailLines=0":                "",
		"":                            "one\ntwo\nthree\nfour\nfive, with no line ending",
		"?tailLines=many":             "one\ntwo\nthree\nfour\nfive, with no line ending", // not a number: not obeyed
		"?tailLines=2&follow=true":    "four\nfive, with no line ending",                  // `logs -f`: the same lines, and then the end
		"?sinceSeconds=300":           "one\ntwo\nthree\nfour\nfive, with no line ending", // lines with no time on them are none of them too old
	} {
		if code, body := ask(t, front, http.MethodGet, log+query, "*/*"); code != http.StatusOK || body != want {
			t.Errorf("logs%s: %d %q, want %q", query, code, body, want)
		}
	}
	// The snapshot server is asked for the whole log; a tail it would ignore is not sent to it.
	for _, a := range *asked {
		if strings.Contains(a, "/log") && (strings.Contains(a, "tailLines=") || strings.Contains(a, "follow=")) {
			t.Errorf("the snapshot server was asked %s", a)
		}
	}
	// An error is not a log: it is passed on whole, not cut to its last line.
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/pods/nope/log?tailLines=1", "*/*"); code != http.StatusNotFound || !strings.Contains(body, "line one") {
		t.Errorf("a log that is not there: %d %s", code, body)
	}
}
