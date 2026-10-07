package replay

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// A third thing the snapshot server does not do as a cluster does, after field selectors and log
// tails: its tables.
//
// `kubectl get` prints what the server sends — the columns and the cells are the API server's, not
// kubectl's. The snapshot server's tables are its own: no wide columns for pods or Deployments, a
// name and an age for ReplicaSets, Endpoints and EndpointSlices, its own heading and its own OBJECT
// column for events, an age in whole hours. A request for one object by name is answered with the
// object, which kubectl prints as NAME and AGE whatever it is. And a table asked for with the whole
// object in each row (`includeObject=Object`, which is how kubectl sorts) comes back with metadata
// only, so `--sort-by` on anything outside metadata finds no such field and prints "No resources
// found". Round 38 found these in 33 steps of 19 of its 36 frozen runs and in none of the live ones.
//
// So a table request is answered here: the objects are fetched, and the table is built from them the
// way the API server builds it, for the kinds below. For any other kind the snapshot server's table
// is kept, with what can be repaired without knowing the kind: the row for one object picked out of
// the list, the whole object put in each row when asked, and the age written the way a cluster
// writes it.
//
// An age is counted to the freeze, not to now: a table is the server's answer, and the case's server
// stopped at the freeze. (`kubectl describe` counts its own ages from the wall clock, and nothing here
// can change that.)

type column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Format   string `json:"format"`
	Priority int    `json:"priority"`
}

func col(name string) column  { return column{Name: name, Type: "string"} }
func wide(name string) column { return column{Name: name, Type: "string", Priority: 1} }

// A printer is the columns of one kind and the cells of one object of it. now is the freeze.
type printer struct {
	group   string // the API group of the kind: "" is core
	columns []column
	cells   func(o obj, now time.Time) []any
	before  func(a, b obj) bool      // the order of the rows, where a cluster's is not by name
	inList  func(o obj, cells []any) // what a cluster writes for a row of a list and not for one object
}

var nameColumn = column{Name: "Name", Type: "string", Format: "name"}

func named(rest ...column) []column { return append([]column{nameColumn}, rest...) }

// obj is a decoded JSON object, read by path. A field that is not there reads as its zero value,
// which is what the API server's printers see for a field that was never set.
type obj map[string]any

func (o obj) get(keys ...string) any {
	var v any = map[string]any(o)
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func (o obj) at(keys ...string) obj {
	m, _ := o.get(keys...).(map[string]any)
	return m
}

func (o obj) has(keys ...string) bool { return o.get(keys...) != nil }

func (o obj) str(keys ...string) string {
	s, _ := o.get(keys...).(string)
	return s
}

func (o obj) int(keys ...string) int64 {
	n, _ := o.get(keys...).(float64)
	return int64(n)
}

func (o obj) bool(keys ...string) bool {
	b, _ := o.get(keys...).(bool)
	return b
}

func (o obj) list(keys ...string) []obj {
	raw, _ := o.get(keys...).([]any)
	out := make([]obj, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		out = append(out, m)
	}
	return out
}

func (o obj) strings(keys ...string) []string {
	raw, _ := o.get(keys...).([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (o obj) name() string { return o.str("metadata", "name") }

func (o obj) time(keys ...string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, o.str(keys...))
	return t
}

func (o obj) age(now time.Time) string { return since(o.time("metadata", "creationTimestamp"), now) }

// serveTable answers a table request from the objects themselves.
func (f *front) serveTable(w http.ResponseWriter, r *http.Request, q url.Values, rp resourcePath) {
	terms, err := parseFieldSelector(q.Get("fieldSelector"))
	if err != nil {
		badRequest(w, err)
		return
	}
	include := q.Get("includeObject")
	for _, k := range []string{"fieldSelector", "limit", "continue", "includeObject"} {
		q.Del(k)
	}
	var body []byte
	var resp *http.Response
	if rp.name != "" { // one object: found in the list if the snapshot server cannot find it by name
		o, missing := f.object(r, q, rp)
		if missing {
			writeNotFound(w, rp)
			return
		}
		if o == nil {
			f.proxy.ServeHTTP(w, r)
			return
		}
		body, _ = json.Marshal(o)
	} else if body, resp, err = f.get(r, q, "application/json"); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	} else if resp.StatusCode != http.StatusOK {
		relay(w, resp, body) // an error is the snapshot server's to word
		return
	}
	var doc map[string]json.RawMessage
	var head struct {
		Kind       string         `json:"kind"`
		APIVersion string         `json:"apiVersion"`
		Metadata   map[string]any `json:"metadata"`
	}
	if json.Unmarshal(body, &doc) != nil || json.Unmarshal(body, &head) != nil || head.Kind == "" || head.Kind == "Status" {
		f.proxy.ServeHTTP(w, r)
		return
	}
	kind, one := strings.TrimSuffix(head.Kind, "List"), rp.name != ""
	var raws []json.RawMessage
	if one {
		raws = []json.RawMessage{body}
	} else if json.Unmarshal(doc["items"], &raws) != nil {
		f.proxy.ServeHTTP(w, r)
		return
	}
	items := make([]obj, 0, len(raws))
	for _, raw := range raws {
		var o obj
		if json.Unmarshal(raw, &o) != nil || o == nil {
			continue
		}
		if one || matchesFields(map[string]any(o), terms) {
			o["kind"] = kind // an item of a list does not say what it is
			items = append(items, redacted(o))
		}
	}
	// A cluster lists in the order of its keys; the snapshot server in whatever order it read its
	// files, which is not the same twice.
	inKeyOrder(items)
	// A printer is for a kind of one API group: a custom resource called Service is not a Service.
	group, _, grouped := strings.Cut(head.APIVersion, "/")
	if !grouped {
		group = ""
	}
	p, printed := printers[kind]
	printed = printed && p.group == group
	if printed && p.before != nil {
		sort.SliceStable(items, func(i, j int) bool { return p.before(items[i], items[j]) })
	}

	table := map[string]any{"kind": "Table", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{"resourceVersion": head.Metadata["resourceVersion"]}}
	rowObject := func(it obj) any {
		switch include {
		case "None":
			return nil
		case "Object": // the whole object, saying what it is: an item of a list does not
			whole := map[string]any{}
			for k, v := range it {
				whole[k] = v
			}
			whole["kind"], whole["apiVersion"] = kind, head.APIVersion
			return whole
		}
		return map[string]any{"kind": "PartialObjectMetadata", "apiVersion": "meta.k8s.io/v1", "metadata": it["metadata"]}
	}

	if printed {
		rows := make([]map[string]any, 0, len(items))
		for _, it := range items {
			cells := p.cells(it, f.now)
			if p.inList != nil && !one {
				p.inList(it, cells)
			}
			row := map[string]any{"cells": cells}
			if o := rowObject(it); o != nil {
				row["object"] = o
			}
			rows = append(rows, row)
		}
		table["columnDefinitions"], table["rows"] = p.columns, rows
		writeJSON(w, table)
		return
	}

	// A kind with no printer here: the snapshot server's own table of the list, its rows matched to
	// the objects by namespace and name.
	listRequest := r.Clone(r.Context())
	listRequest.URL.Path = rp.collection
	body, resp, err = f.get(listRequest, q, r.Header.Get("Accept"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var theirs struct {
		Columns []map[string]any  `json:"columnDefinitions"`
		Rows    []json.RawMessage `json:"rows"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &theirs) != nil || theirs.Columns == nil {
		f.proxy.ServeHTTP(w, r) // no table of it either: as it was
		return
	}
	age := -1
	for i, c := range theirs.Columns {
		name, _ := c["name"].(string)
		if strings.EqualFold(name, "age") {
			age = i
		}
		// kubectl writes a name as kind/name, when several kinds are listed together, in the column a
		// cluster marks as the name. The snapshot server marks none.
		if c["type"] == nil || c["type"] == "" {
			c["type"] = "string"
		}
		if i == 0 && strings.EqualFold(name, "name") {
			c["format"] = "name"
		}
	}
	theirRow := map[string][]any{}
	for _, raw := range theirs.Rows {
		var row struct {
			Cells  []any `json:"cells"`
			Object any   `json:"object"`
		}
		if json.Unmarshal(raw, &row) == nil {
			theirRow[identity(row.Object)] = row.Cells
		}
	}
	rows := make([]map[string]any, 0, len(items))
	for _, it := range items {
		cells, ok := theirRow[identity(map[string]any(it))]
		if !ok {
			continue
		}
		if age >= 0 && age < len(cells) {
			cells[age] = it.age(f.now)
		}
		row := map[string]any{"cells": cells}
		if o := rowObject(it); o != nil {
			row["object"] = o
		}
		rows = append(rows, row)
	}
	if one && len(rows) == 0 {
		f.proxy.ServeHTTP(w, r)
		return
	}
	table["columnDefinitions"], table["rows"] = theirs.Columns, rows
	writeJSON(w, table)
}

func badRequest(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "BadRequest", "code": http.StatusBadRequest, "message": err.Error()})
}
