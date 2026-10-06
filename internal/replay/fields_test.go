package replay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// A stand-in for the snapshot server with its one relevant habit: it ignores fieldSelector. It answers
// a list as objects or as a table, as asked, and records what it was asked.
func snapshotServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	type pod struct{ name, node, phase string }
	pods := []pod{{"cache-1", "worker", "Running"}, {"api-1", "worker2", "Running"}, {"api-2", "worker2", "Pending"}, {"job-1", "", "Pending"}}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.RequestURI()+" ["+r.Header.Get("Accept")+"]")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/pods/cache-1":
			io.WriteString(w, `{"kind":"Pod","metadata":{"name":"cache-1","namespace":"shop"},"spec":{"nodeName":"worker"}}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/gone/pods":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"kind":"Status","status":"Failure","code":404}`)
		case r.URL.Path == "/kubernetes/api/v1/namespaces/shop/events":
			io.WriteString(w, `{"kind":"EventList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[`+
				`{"metadata":{"name":"e1","namespace":"shop"},"involvedObject":{"kind":"Pod","name":"cache-1"},"reason":"Pulled","count":3},`+
				`{"metadata":{"name":"e2","namespace":"shop"},"involvedObject":{"kind":"Pod","name":"api-1"},"reason":"Pulled","count":1},`+
				`{"metadata":{"name":"e3","namespace":"shop"},"involvedObject":{"kind":"ReplicaSet","name":"api"},"reason":"SuccessfulCreate","count":1}]}`)
		case strings.HasSuffix(r.URL.Path, "/api-1/log"):
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "one\ntwo\nthree\nfour\nfive, with no line ending")
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
				items = append(items, `{"metadata":{"name":"`+p.name+`","namespace":"shop"},"spec":{`+node+`"hostNetwork":false},"status":{"phase":"`+p.phase+`"}}`)
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
	front := httptest.NewServer(newFront(target))
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
			if got := names(t, body, shape.list); code != http.StatusOK || !reflect.DeepEqual(got, want) || !strings.Contains(body, shape.kind) {
				t.Errorf("%s as %s: %d %v, want %v", selector, shape.list, code, got, want)
			}
		}
	}
	// The table keeps its columns, and the list its envelope.
	if _, body := ask(t, front, http.MethodGet, shopPods+"?fieldSelector=spec.nodeName%3Dworker", asTable); !strings.Contains(body, `"columnDefinitions":[{"name":"Name"}]`) || !strings.Contains(body, `"cells":["cache-1","1/1","Running"]`) {
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

func TestWhatHasNoFieldSelectorGoesThroughUntouched(t *testing.T) {
	upstream, asked := snapshotServer(t)
	target, _ := url.Parse(upstream.URL + "/kubernetes")
	front := httptest.NewServer(newFront(target))
	defer front.Close()

	_, direct := ask(t, upstream, http.MethodGet, shopPods+"?limit=500", asTable)
	if code, through := ask(t, front, http.MethodGet, shopPods+"?limit=500", asTable); code != http.StatusOK || through != direct {
		t.Errorf("a list with no selector was changed:\n%s\n%s", through, direct)
	}
	if !strings.HasPrefix((*asked)[len(*asked)-1], "GET "+shopPods+"?limit=500 [") {
		t.Errorf("upstream was asked %s", (*asked)[len(*asked)-1])
	}
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/pods/cache-1/log?follow=true", "*/*"); code != http.StatusOK || body != "a stream of log lines\n" {
		t.Errorf("a log: %d %q", code, body)
	}
	// A watch is a stream, and is the snapshot server's to answer however it does.
	if code, body := ask(t, front, http.MethodGet, shopPods+"?watch=true&fieldSelector=spec.nodeName%3Dworker2", asObjects); code != http.StatusOK || len(names(t, body, "items")) != 4 {
		t.Errorf("a watch was filtered: %d %s", code, body)
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
	front := httptest.NewServer(newFront(target))
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
		"?tailLines=many":             "one\ntwo\nthree\nfour\nfive, with no line ending", // not a number: the snapshot server's to answer
		"?tailLines=2&follow=true":    "one\ntwo\nthree\nfour\nfive, with no line ending", // a stream is passed through
		"?sinceSeconds=300":           "one\ntwo\nthree\nfour\nfive, with no line ending", // still not honoured, and documented
	} {
		if code, body := ask(t, front, http.MethodGet, log+query, "*/*"); code != http.StatusOK || body != want {
			t.Errorf("logs%s: %d %q, want %q", query, code, body, want)
		}
	}
	// The snapshot server is asked for the whole log; a tail it would ignore is not sent to it.
	for _, a := range *asked {
		if strings.Contains(a, "tailLines=") && !strings.Contains(a, "follow=true") && !strings.Contains(a, "tailLines=many") {
			t.Errorf("the snapshot server was asked %s", a)
		}
	}
	// An error is not a log: it is passed on whole, not cut to its last line.
	if code, body := ask(t, front, http.MethodGet, "/kubernetes/api/v1/namespaces/shop/pods/nope/log?tailLines=1", "*/*"); code != http.StatusNotFound || !strings.Contains(body, "line one") {
		t.Errorf("a log that is not there: %d %s", code, body)
	}
	for in, want := range map[string]string{"a\nb\n": "b\n", "a\nb": "b", "\n\n": "\n", "": "", "a": "a"} {
		if got := string(lastLines([]byte(in), 1)); got != want {
			t.Errorf("lastLines(%q, 1) = %q, want %q", in, got, want)
		}
	}
}
