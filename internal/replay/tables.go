package replay

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/util/jsonpath"
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
	before  func(a, b obj) bool // the order of the rows, where a cluster's is not by name
	// inList is what a cluster writes for a row of a list and not for one object; all is the list.
	inList func(o obj, cells []any, all []obj)
	// as is this printer as an earlier version of Kubernetes had it, where it is known to have had
	// another: a table is written as v1.37 writes it, and a case may be frozen from an older cluster.
	as func(minor int, p printer) printer
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
	shown := func(o obj) obj { return o } // the object as its printer reads it
	if as, ok := sameKind[group+"/"+kind]; ok && !printed {
		p, printed, shown = printers[kind], true, as
	}
	if printed && p.as != nil {
		if minor := f.clusterMinor(); minor > 0 { // a case that does not say is printed as the newest
			p = p.as(minor, p)
		}
	}
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
	write := func(columns any, cells func(it obj) []any) {
		rows := make([]map[string]any, 0, len(items))
		for _, it := range items {
			row := map[string]any{"cells": cells(it)}
			if o := rowObject(it); o != nil {
				row["object"] = o
			}
			rows = append(rows, row)
		}
		table["columnDefinitions"], table["rows"] = columns, rows
		writeJSON(w, table)
	}

	if printed {
		write(p.columns, func(it obj) []any {
			cells := p.cells(shown(it), f.now)
			if p.inList != nil && !one {
				p.inList(shown(it), cells, items)
			}
			return cells
		})
		return
	}

	// A kind of someone's own is printed as its definition says, whatever it is called: the snapshot
	// server prints one named like a kind of Kubernetes's as that kind, and reads the paths of the
	// others with a JSONPath of its own. Here they are read with the API server's.
	if definition := f.definition(r, group, kind); definition != nil {
		columns, paths := definedColumns(definition, head.APIVersion[strings.LastIndex(head.APIVersion, "/")+1:])
		write(columns, func(it obj) []any {
			cells, whole := []any{it.name()}, asStored(map[string]any(it))
			for i, path := range paths {
				cells = append(cells, definedCell(whole, path, columns[i+1].Type, f.now))
			}
			return cells
		})
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

// sameKind is a kind that another API group serves too, under other names for the same things: the
// API server prints both with one printer, having read both into one form.
var sameKind = map[string]func(obj) obj{
	"events.k8s.io/Event": func(o obj) obj {
		core := obj{}
		for k, v := range o {
			core[k] = v
		}
		for theirs, ours := range map[string]string{"regarding": "involvedObject", "note": "message", "deprecatedSource": "source", "deprecatedCount": "count",
			"deprecatedFirstTimestamp": "firstTimestamp", "deprecatedLastTimestamp": "lastTimestamp", "reportingController": "reportingComponent"} {
			if v, ok := o[theirs]; ok {
				core[ours] = v
			}
		}
		return core
	},
}

// definition is the CustomResourceDefinition of a kind, if the snapshot has one.
func (f *front) definition(r *http.Request, group, kind string) obj {
	crds := r.Clone(r.Context())
	crds.URL.Path = f.prefix + "/apis/apiextensions.k8s.io/v1/customresourcedefinitions"
	var list struct {
		Items []obj `json:"items"`
	}
	if body, resp, err := f.get(crds, url.Values{}, "application/json"); err == nil && resp.StatusCode == http.StatusOK {
		json.Unmarshal(body, &list)
	}
	for _, crd := range list.Items {
		if group != "" && crd.str("spec", "group") == group && crd.str("spec", "names", "kind") == kind {
			return crd
		}
	}
	return nil
}

// definedColumns is what a custom resource's definition says to print for one of its versions: its
// name, and then either the columns it lists or, if it lists none, its age. The paths are theirs,
// one a column after the first.
func definedColumns(definition obj, version string) (columns []column, paths []string) {
	columns = []column{nameColumn}
	for _, v := range definition.list("spec", "versions") {
		if v.str("name") != version {
			continue
		}
		for _, c := range v.list("additionalPrinterColumns") {
			columns = append(columns, column{Name: c.str("name"), Type: c.str("type"), Format: c.str("format"), Priority: int(c.int("priority"))})
			paths = append(paths, c.str("jsonPath"))
		}
	}
	if len(paths) == 0 {
		columns, paths = append(columns, column{Name: "Age", Type: "date"}), []string{".metadata.creationTimestamp"}
	}
	return columns, paths
}

// asStored is a decoded object with its whole numbers as integers, which is how the API server holds
// a custom resource, and how a path that prints one writes it: 1000000 and not 1e+06.
func asStored(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = asStored(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = asStored(e)
		}
		return out
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t)
		}
	}
	return v
}

// definedCell is one cell of a custom resource, as the API server's table convertor for custom
// resources makes it (apiextensions-apiserver, registry/customresource/tableconvertor): the first
// thing the path finds; a string column printed as JSONPath prints; any other column only if the
// value is of the column's type; a date as how long ago. Nothing where nothing is found.
func definedCell(whole any, path, kind string, now time.Time) any {
	read := jsonpath.New("column")
	if read.Parse("{"+path+"}") != nil {
		return nil // the API server would not have taken the definition
	}
	read.AllowMissingKeys(true)
	results, err := read.FindResults(whole)
	if err != nil || len(results) == 0 || len(results[0]) == 0 {
		return nil
	}
	value := results[0][0].Interface()
	if kind == "string" {
		var text bytes.Buffer
		if read.PrintResults(&text, []reflect.Value{reflect.ValueOf(value)}) != nil {
			return nil
		}
		return text.String()
	}
	switch typed := value.(type) {
	case int64:
		switch kind {
		case "integer":
			return typed
		case "number":
			return float64(typed)
		}
	case float64:
		switch kind {
		case "integer":
			return int64(typed)
		case "number":
			return typed
		}
	case bool:
		if kind == "boolean" {
			return typed
		}
	case string:
		if kind == "date" {
			if typed == "" || typed == "null" {
				return "<unknown>"
			}
			at, err := time.Parse(time.RFC3339, typed)
			if err != nil {
				return "<invalid>"
			}
			return since(at, now)
		}
	}
	return nil
}
