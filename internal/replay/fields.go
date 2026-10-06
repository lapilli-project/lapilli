package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// front stands in front of the snapshot server and does two things it does not: it filters a list by
// a field selector, and it honours `kubectl logs --tail`.
//
// The snapshot server ignores `fieldSelector`. Asked for the pods on one node it returns every pod, and
// `kubectl describe`, which asks for an object's events that way, lists every event in the namespace —
// four times the output a live cluster gives, in the frozen condition only. That is a difference an
// agent can be misled by, so it is closed here rather than documented: the list is fetched whole,
// filtered, and returned in the shape that was asked for. Everything else goes through untouched.
//
// Any dotted path into an object is accepted as a field, which is more than a real API server allows
// (it knows a short list per resource and refuses the rest). A selector that works here and not on a
// live cluster is possible; one that works on a live cluster and not here should not be.
//
// It also ignores `tailLines`, and answers `kubectl logs --tail=20` with the whole log. A frozen log is
// a file, so the last lines of it are exactly what a live cluster would have returned at the freeze.
// `--since` is still not honoured: that needs a time for every line, and a snapshot has only the text.
type front struct {
	upstream *url.URL // scheme and host of the snapshot server
	proxy    *httputil.ReverseProxy
	client   *http.Client
}

func newFront(upstream *url.URL) *front {
	target := &url.URL{Scheme: upstream.Scheme, Host: upstream.Host}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // `logs -f` and watches are streams
	return &front{upstream: target, proxy: proxy, client: &http.Client{}}
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
		default:
			return nil, fmt.Errorf("invalid field selector %q: expected field=value, field==value or field!=value", part)
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

func (f *front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if n, err := strconv.Atoi(q.Get("tailLines")); err == nil && n >= 0 && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/log") && q.Get("follow") != "true" {
		q.Del("tailLines")
		body, resp, err := f.get(r, q, r.Header.Get("Accept"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if resp.StatusCode == http.StatusOK {
			body = lastLines(body, n)
		}
		relay(w, resp, body)
		return
	}
	selector := q.Get("fieldSelector")
	if selector == "" || r.Method != http.MethodGet || q.Get("watch") == "true" || q.Get("watch") == "1" {
		f.proxy.ServeHTTP(w, r)
		return
	}
	terms, err := parseFieldSelector(selector)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "BadRequest", "code": http.StatusBadRequest, "message": err.Error()})
		return
	}
	// The whole list, as objects: the fields a selector names are not in a table's rows.
	q.Del("fieldSelector")
	q.Del("limit") // a page of the unfiltered list is not a page of the filtered one
	q.Del("continue")
	body, resp, err := f.get(r, q, "application/json")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var list map[string]json.RawMessage
	var items []json.RawMessage
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &list) != nil || json.Unmarshal(list["items"], &items) != nil || list["items"] == nil {
		relay(w, resp, body) // an error, or not a list: a selector has nothing to filter
		return
	}
	kept, keep := make([]json.RawMessage, 0, len(items)), map[string]bool{}
	for _, raw := range items {
		var obj any
		if json.Unmarshal(raw, &obj) == nil && matchesFields(obj, terms) {
			kept, keep[identity(obj)] = append(kept, raw), true
		}
	}

	if accept := r.Header.Get("Accept"); strings.Contains(accept, "as=Table") {
		body, resp, err = f.get(r, q, accept)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		var table map[string]json.RawMessage
		var rows []json.RawMessage
		if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &table) != nil || json.Unmarshal(table["rows"], &rows) != nil || table["rows"] == nil {
			relay(w, resp, body)
			return
		}
		keptRows := make([]json.RawMessage, 0, len(rows))
		for _, raw := range rows {
			var row struct {
				Object any `json:"object"`
			}
			if json.Unmarshal(raw, &row) == nil && keep[identity(row.Object)] {
				keptRows = append(keptRows, raw)
			}
		}
		table["rows"], _ = json.Marshal(keptRows)
		writeJSON(w, table)
		return
	}
	list["items"], _ = json.Marshal(kept)
	writeJSON(w, list)
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

// lastLines returns the last n lines of a log, each with the line ending it had.
func lastLines(log []byte, n int) []byte {
	lines := bytes.SplitAfter(log, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return bytes.Join(lines, nil)
}

func relay(w http.ResponseWriter, resp *http.Response, body []byte) {
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
