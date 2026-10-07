package replay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lapilli-project/lapilli/internal/freeze"
)

// front stands in front of the snapshot server and answers what it does not answer as a cluster
// does. The snapshot server accepts requests it does not implement and answers them well formed and
// wrong, which is a difference an agent can be misled by, so each is closed here rather than
// documented:
//
//   - a field selector, which it ignores: asked for the pods on one node it returns every pod, and
//     `kubectl describe`, which asks for an object's events that way, listed every event in the
//     namespace. The list is fetched whole and filtered; so is a watch that names what it watches.
//   - what a log is asked by — its last lines, the lines since a time, their times — all of which
//     it ignores or half does: a frozen log is a file with the kubelet's time on every line, and
//     what a cluster does with those times is done here (serveLog).
//   - a table, which is what `kubectl get` prints (tables.go, printers.go).
//   - a list, which it returns in the order it read its files: here in a cluster's order.
//   - one object by name, where it cannot find a name it lists, and "not found" in a cluster's words.
//   - a Secret that `freeze` blanked, sent so that it decodes.
//
// What is left to it untouched: a write, a stream, a watch of a whole list, discovery, and anything
// under an object but a pod's log.
//
// Any dotted path into an object is accepted as a field, which is more than a real API server allows
// (it knows a short list per resource and refuses the rest). A selector that works here and not on a
// live cluster is possible; one that works on a live cluster and not here should not be.
type front struct {
	upstream *url.URL // scheme and host of the snapshot server
	prefix   string   // the path its API is under
	proxy    *httputil.ReverseProxy
	client   *http.Client
	now      time.Time // the freeze: what an age in a table is counted to
	served   time.Time // when the case began to be served: no line of a log was stamped after it

	versionOnce sync.Once
	minor       int // the minor version of the cluster the case was frozen from; 0 if unknown
}

func newFront(upstream *url.URL, freeze time.Time) *front {
	target := &url.URL{Scheme: upstream.Scheme, Host: upstream.Host}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // `logs -f` and watches are streams
	// The snapshot server is the only place a request goes, whatever it answers: a redirect is neither
	// followed here nor handed to kubectl to follow.
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return fmt.Errorf("the snapshot server answered %s", resp.Status)
		}
		return nil
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &front{upstream: target, prefix: strings.TrimSuffix(upstream.Path, "/"), proxy: proxy, client: client, now: freeze, served: time.Now()}
}

// clusterMinor is the minor version of the cluster the case was frozen from, asked of the snapshot
// server once.
func (f *front) clusterMinor() int {
	f.versionOnce.Do(func() {
		u := *f.upstream
		u.Path = f.prefix + "/version"
		resp, err := f.client.Get(u.String())
		if err != nil {
			return
		}
		defer resp.Body.Close()
		var v struct {
			Minor string `json:"minor"`
		}
		if json.NewDecoder(resp.Body).Decode(&v) == nil {
			f.minor, _ = strconv.Atoi(strings.TrimRight(v.Minor, "+"))
		}
	})
	return f.minor
}

type fieldTerm struct {
	path   []string
	negate bool
	value  string
}

// parseFieldSelector reads `a.b=x,c!=y`. `==` is `=`.
func parseFieldSelector(s string) ([]fieldTerm, error) {
	var terms []fieldTerm
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		var t fieldTerm
		var lhs, rhs string
		switch {
		case strings.Contains(part, "!="):
			lhs, rhs, _ = strings.Cut(part, "!=")
			t.negate = true
		case strings.Contains(part, "=="):
			lhs, rhs, _ = strings.Cut(part, "==")
		case strings.Contains(part, "="):
			lhs, rhs, _ = strings.Cut(part, "=")
		default: // a cluster's words for it
			return nil, fmt.Errorf("invalid selector: '%s'; can't understand '%s'", s, part)
		}
		if lhs = strings.TrimSpace(lhs); lhs == "" {
			return nil, fmt.Errorf("invalid field selector %q: no field", part)
		}
		t.path, t.value = strings.Split(lhs, "."), strings.TrimSpace(rhs)
		terms = append(terms, t)
	}
	return terms, nil
}

// field reads a dotted path out of a decoded object. A field that is not there is the empty string,
// which is what `spec.nodeName=` relies on to find pods that were never scheduled.
func field(obj any, path []string) string {
	for _, key := range path {
		m, ok := obj.(map[string]any)
		if !ok {
			return ""
		}
		obj = m[key]
	}
	switch v := obj.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func matchesFields(obj any, terms []fieldTerm) bool {
	for _, t := range terms {
		if (field(obj, t.path) == t.value) == t.negate {
			return false
		}
	}
	return true
}

func identity(obj any) string {
	return field(obj, []string{"metadata", "namespace"}) + "/" + field(obj, []string{"metadata", "name"})
}

// resourcePath is what a request path names: …/apis/apps/v1/namespaces/shop/deployments/cache/scale
// is the subresource scale of the object cache in the collection …/namespaces/shop/deployments.
type resourcePath struct {
	collection                            string // the path of the list the object is in
	group, resource, namespace, name, sub string
}

// parseResourcePath reads a path of the Kubernetes API. "namespaces" is both a resource and what
// scopes one, so /namespaces/shop is an object and /namespaces/shop/pods a collection.
func parseResourcePath(p string) (rp resourcePath, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range parts {
		base := 0
		switch {
		case s == "api" && i+2 < len(parts): // api/<version>/…
			base = i + 2
		case s == "apis" && i+3 < len(parts): // apis/<group>/<version>/…
			rp.group, base = parts[i+1], i+3
		default:
			continue
		}
		rest := parts[base:]
		if rest[0] == "namespaces" && len(rest) >= 3 && !(len(rest) == 3 && (rest[2] == "status" || rest[2] == "finalize")) {
			rp.namespace, rest, base = rest[1], rest[2:], base+2
		}
		rp.resource, rp.collection = rest[0], "/"+strings.Join(parts[:base+1], "/")
		if len(rest) > 1 {
			rp.name = rest[1]
		}
		rp.sub = strings.Join(rest[min(2, len(rest)):], "/")
		return rp, true
	}
	return rp, false
}

func (f *front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rp, ok := parseResourcePath(r.URL.Path)
	watch := q.Get("watch") == "true" || q.Get("watch") == "1"
	if ok && watch && r.Method == http.MethodGet && rp.sub == "" && q.Get("fieldSelector") != "" && !strings.Contains(r.Header.Get("Accept"), "as=Table") {
		f.serveWatch(w, r, q)
		return
	}
	if !ok || r.Method != http.MethodGet || watch {
		f.proxy.ServeHTTP(w, r) // a stream, a write, or not a resource: the snapshot server's to answer
		return
	}
	// A cluster of v1.33 or later says this with every answer about Endpoints, and kubectl prints it.
	if rp.group == "" && rp.resource == "endpoints" && f.clusterMinor() >= 33 {
		w.Header().Add("Warning", `299 - "v1 Endpoints is deprecated in v1.33+; use discovery.k8s.io/v1 EndpointSlice"`)
	}
	switch {
	case rp.resource == "pods" && rp.sub == "log":
		f.serveLog(w, r, q, rp) // and `logs -f` too: what there is, and then the end, since nothing more is written in a case
	case rp.sub != "":
		f.proxy.ServeHTTP(w, r)
	case strings.Contains(r.Header.Get("Accept"), "as=Table"):
		f.serveTable(w, r, q, rp) // with its field selector, if it has one
	case rp.name != "":
		f.serveObject(w, r, q, rp)
	default:
		f.serveList(w, r, q)
	}
}

// inKeyOrder puts objects in the order a cluster lists them: by the key they are stored under,
// which is namespace/name. So `shop-db` comes before `shop`, a hyphen sorting before a slash.
func inKeyOrder(items []obj) {
	key := func(o obj) string {
		if ns := o.str("metadata", "namespace"); ns != "" {
			return ns + "/" + o.name()
		}
		return o.name()
	}
	sort.SliceStable(items, func(i, j int) bool { return key(items[i]) < key(items[j]) })
}

// serveLog answers a request for a pod's log. A snapshot keeps each line with the time the kubelet
// stamped on it, and the snapshot server gives those times back or drops them but does not read
// them. So the log is asked for with its times, and what a cluster does with them is done here:
// `--since` and `--since-time`, the first counted back from the freeze; `--tail`; `--limit-bytes`;
// and `--timestamps` itself. Where there is no log, why not is said in a cluster's words.
func (f *front) serveLog(w http.ResponseWriter, r *http.Request, q url.Values, rp resourcePath) {
	number := func(key string) int {
		if n, err := strconv.Atoi(q.Get(key)); err == nil && n >= 0 {
			return n
		}
		return -1
	}
	tail, limit, stamped := number("tailLines"), number("limitBytes"), q.Get("timestamps") == "true"
	var from time.Time
	if s := number("sinceSeconds"); s > 0 {
		from = f.now.Add(-time.Duration(s) * time.Second)
	}
	if t, err := time.Parse(time.RFC3339, q.Get("sinceTime")); err == nil {
		from = t
	}
	for _, k := range []string{"tailLines", "limitBytes", "sinceSeconds", "sinceTime", "follow"} {
		q.Del(k)
	}
	q.Set("timestamps", "true")
	body, resp, err := f.get(r, q, r.Header.Get("Accept"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode == http.StatusOK {
		lines := bytes.SplitAfter(body, []byte("\n"))
		if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
			lines = lines[:len(lines)-1]
		}
		// Where the kubelet has no log to give, it says so in the log's place, and says all of it
		// whatever was asked for: that is no line of a log to count or to cut.
		if len(lines) == 1 {
			if at, text := lineTime(lines[0], f.served); at.IsZero() && bytes.HasPrefix(text, []byte("unable to retrieve container logs for ")) {
				w.Header().Set("Content-Type", "text/plain")
				w.Write(text)
				return
			}
		}
		kept := lines[:0]
		for _, line := range lines {
			at, text := lineTime(line, f.served)
			switch {
			case !from.IsZero() && !at.IsZero() && at.Before(from):
			case stamped && !at.IsZero():
				kept = append(kept, line)
			default:
				kept = append(kept, text)
			}
		}
		if tail >= 0 && len(kept) > tail {
			kept = kept[len(kept)-tail:]
		}
		out := bytes.Join(kept, nil)
		if limit >= 0 && len(out) > limit {
			out = out[:limit]
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write(out)
		return
	}

	// No log. Why not is something a cluster says and the snapshot server does not.
	pod := r.Clone(r.Context())
	pod.URL.Path = rp.collection + "/" + rp.name
	o, missing := f.object(pod, url.Values{}, rp)
	if missing {
		writeNotFound(w, rp)
		return
	}
	if o != nil {
		container := q.Get("container")
		if container == "" && len(o.list("spec", "containers")) > 0 {
			container = o.list("spec", "containers")[0].str("name")
		}
		var status obj
		for _, c := range append(append(o.list("status", "containerStatuses"), o.list("status", "initContainerStatuses")...), o.list("status", "ephemeralContainerStatuses")...) {
			if c.str("name") == container {
				status = c
			}
		}
		known := false
		for _, c := range append(append(o.list("spec", "containers"), o.list("spec", "initContainers")...), o.list("spec", "ephemeralContainers")...) {
			known = known || c.str("name") == container
		}
		switch waiting := status.at("state", "waiting"); {
		case !known:
		case q.Get("previous") == "true":
			if !status.has("lastState", "terminated") { // `kubectl logs -p` on a container that has run once, or never
				badRequest(w, fmt.Errorf("previous terminated container %q in pod %q not found", container, rp.name))
				return
			}
		case waiting != nil && !status.has("lastState", "terminated"): // it has never run, and the kubelet says what it is waiting for
			why := waiting.str("reason")
			switch why {
			case "ErrImagePull":
				why = "image can't be pulled"
			case "ImagePullBackOff":
				why = "trying and failing to pull image"
			}
			badRequest(w, fmt.Errorf("container %q in pod %q is waiting to start: %s", container, rp.name, why))
			return
		}
	}
	relay(w, resp, body)
}

// lineTime splits a line of a log into the time the kubelet stamped on it and the line itself. A
// line the snapshot holds without a time — where the collector wrote down an error in place of a log
// — is given one by the snapshot server: the time it took the file up, the same at every asking. No
// line of the cluster's was stamped after its case began to be served, so a time that late is the
// snapshot server's and not the line's, and is taken off. (A node whose clock ran ahead of this
// machine's by more than the time between the freeze and the serving could have a last line read so;
// it would lose its time under --timestamps and nothing else.)
func lineTime(line []byte, served time.Time) (time.Time, []byte) {
	stamp, text, found := bytes.Cut(line, []byte(" "))
	if !found {
		stamp, text = bytes.TrimRight(line, "\n"), line[len(bytes.TrimRight(line, "\n")):]
	}
	at, err := time.Parse(time.RFC3339Nano, string(stamp))
	if err != nil {
		return time.Time{}, line
	}
	if !at.Before(served) {
		return time.Time{}, text
	}
	return at, text
}

// object fetches one object as JSON. The snapshot server cannot find an object whose name has a
// colon in it — every `system:` role and binding — though it lists it; so one it does not find is
// looked for in the list. missing is true only when the list was read and the object is not in it.
func (f *front) object(r *http.Request, q url.Values, rp resourcePath) (o obj, missing bool) {
	body, resp, err := f.get(r, q, "application/json")
	if err != nil {
		return nil, false
	}
	found := resp.StatusCode == http.StatusOK && json.Unmarshal(body, &o) == nil && o.str("kind") != "" && o.str("kind") != "Status"
	if found && o.name() == rp.name { // and not an object stored under a name like it
		return redacted(o), false
	}
	if !found && resp.StatusCode != http.StatusNotFound {
		return nil, false
	}
	list := r.Clone(r.Context())
	list.URL.Path = rp.collection
	body, resp, err = f.get(list, url.Values{}, "application/json")
	var doc struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
		Items      []obj  `json:"items"`
	}
	if err != nil || resp.StatusCode != http.StatusOK || json.Unmarshal(body, &doc) != nil || !strings.HasSuffix(doc.Kind, "List") {
		return nil, false // no such list either: nothing is known about the object
	}
	for _, item := range doc.Items {
		if item != nil && item.name() == rp.name && item.str("metadata", "namespace") == rp.namespace {
			item["kind"], item["apiVersion"] = strings.TrimSuffix(doc.Kind, "List"), doc.APIVersion
			return redacted(item), false
		}
	}
	return nil, true
}

// serveObject answers a request for one object, as an object.
func (f *front) serveObject(w http.ResponseWriter, r *http.Request, q url.Values, rp resourcePath) {
	o, missing := f.object(r, q, rp)
	switch {
	case missing:
		writeNotFound(w, rp)
	case o == nil:
		f.proxy.ServeHTTP(w, r) // whatever the snapshot server says, in its words
	default:
		writeJSON(w, o)
	}
}

// redacted makes a Secret that `freeze` blanked readable as a Secret again. A Secret's values are
// base64 on the wire and the marker `freeze` writes is not, so `kubectl describe secret` failed to
// decode it; here the marker is sent as the base64 of itself, which is what a cluster would send for
// a secret whose value was that text.
func redacted(o obj) obj {
	if o.str("kind") == "Secret" {
		for k, v := range o.at("data") {
			if v == freeze.Redacted {
				o.at("data")[k] = base64.StdEncoding.EncodeToString([]byte(freeze.Redacted))
			}
		}
	}
	return o
}

// writeNotFound says that an object is not there in a cluster's words: `pods "x" not found`.
func writeNotFound(w http.ResponseWriter, rp resourcePath) {
	qualified, details := rp.resource, map[string]any{"name": rp.name, "kind": rp.resource}
	if rp.group != "" {
		qualified, details["group"] = rp.resource+"."+rp.group, rp.group
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure", "reason": "NotFound", "code": http.StatusNotFound,
		"message": fmt.Sprintf("%s %q not found", qualified, rp.name), "details": details})
}

// selected fetches a list whole, as objects, and keeps what a field selector selects. ok is false
// when there was no list to filter, and the snapshot server's answer has been passed on.
func (f *front) selected(w http.ResponseWriter, r *http.Request, q url.Values) (list map[string]json.RawMessage, kept []obj, ok bool) {
	terms, err := parseFieldSelector(q.Get("fieldSelector"))
	if err != nil {
		badRequest(w, err)
		return nil, nil, false
	}
	only := url.Values{}
	if q.Get("labelSelector") != "" {
		only.Set("labelSelector", q.Get("labelSelector")) // not the paging: a page of the unfiltered list is not a page of the filtered one
	}
	body, resp, err := f.get(r, only, "application/json")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return nil, nil, false
	}
	var items []obj
	var kind string
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &list) != nil || list["items"] == nil || json.Unmarshal(list["items"], &items) != nil {
		relay(w, resp, body) // an error, or not a list: a selector has nothing to filter
		return nil, nil, false
	}
	json.Unmarshal(list["kind"], &kind)
	kept = make([]obj, 0, len(items))
	for _, o := range items {
		if o != nil && matchesFields(map[string]any(o), terms) {
			if kind == "SecretList" { // so that redacted knows it; both, or kubectl takes it for neither
				o["kind"], o["apiVersion"] = "Secret", "v1"
			}
			kept = append(kept, redacted(o))
		}
	}
	inKeyOrder(kept)
	return list, kept, true
}

// serveList answers a list of objects: in a cluster's order, filtered by its field selector if it
// has one, its Secrets readable.
func (f *front) serveList(w http.ResponseWriter, r *http.Request, q url.Values) {
	list, kept, ok := f.selected(w, r, q)
	if !ok {
		return
	}
	list["items"], _ = json.Marshal(kept)
	writeJSON(w, list)
}

// serveWatch answers a watch that names what it watches. `kubectl rollout status` watches one
// Deployment by a field selector on its name; the snapshot server ignores the selector and sends the
// first object it has, so the rollout reported was another Deployment's. A frozen cluster has one
// state and no events after it: what is selected is sent as it stands, where the request asks for
// the state to begin with, and then nothing until the client leaves or its time is up.
func (f *front) serveWatch(w http.ResponseWriter, r *http.Request, q url.Values) {
	list, kept, ok := f.selected(w, r, q)
	if !ok {
		return
	}
	var kind, version string
	var meta obj
	json.Unmarshal(list["kind"], &kind)
	json.Unmarshal(list["apiVersion"], &version)
	json.Unmarshal(list["metadata"], &meta)
	kind = strings.TrimSuffix(kind, "List")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	send := json.NewEncoder(w)
	initial := q.Get("sendInitialEvents") == "true"
	if rv := q.Get("resourceVersion"); initial || rv == "" || rv == "0" {
		for _, o := range kept {
			o["kind"], o["apiVersion"] = kind, version
			send.Encode(map[string]any{"type": "ADDED", "object": o})
		}
	}
	if initial { // the mark a client that asked for the state waits for before it believes it has it
		send.Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{"kind": kind, "apiVersion": version,
			"metadata": map[string]any{"resourceVersion": meta.str("resourceVersion"), "annotations": map[string]string{"k8s.io/initial-events-end": "true"}}}})
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	limit := 30 * time.Minute
	if n, err := strconv.Atoi(q.Get("timeoutSeconds")); err == nil && n > 0 {
		limit = time.Duration(n) * time.Second
	}
	select {
	case <-r.Context().Done():
	case <-time.After(limit):
	}
}

func (f *front) get(r *http.Request, q url.Values, accept string) ([]byte, *http.Response, error) {
	u := *f.upstream
	u.Path, u.RawQuery = r.URL.Path, q.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", accept)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp, err
}

func relay(w http.ResponseWriter, resp *http.Response, body []byte) {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 { // see newFront
		http.Error(w, "the snapshot server answered "+resp.Status, http.StatusBadGateway)
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
