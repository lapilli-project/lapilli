package rules

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/sse"
)

// parsedGrafana is a Grafana rule in one form, from what decides when and
// how it fires as the replay reads it: a rule in a provisioning file and
// the same rule as Grafana's API gives it (its export, which adds the
// fields Grafana fills in on save) are one.
//
// It holds the rule's name, uid, group, folder, for, keepFiringFor, its
// group's interval and its labels as one map; and its Graph: the queries
// (a PromQL one through parsedExpr, a LogQL one as its text), the asks the
// replay makes of them, the steps (threshold, math, reduce, classic
// conditions) by value, the condition's ref, each query's end (Shift), its
// data source's uid, what Grafana does on no data and on an error, how
// many evaluations a gone series waits, and the queries on a data source
// the condition doesn't read. Beyond the Graph, what Grafana's evaluation
// has from a query's node that the Graph folds away or doesn't keep: its
// relativeTimeRange, instant and range, hide and format (and, of a query
// on a data source not replayed, its whole model); a reduce's mode and
// replacement; a threshold's unloadEvaluator. A field Grafana fills in
// where the file leaves it out is read as it is filled in (intervalMs,
// maxDataPoints, a reduce's mode, a query's range), and one that changes
// nothing in the evaluation (a model's datasource, refId, legendFormat,
// editorMode; an expression's intervalMs and maxDataPoints) isn't held.
//
// It leaves out what doesn't change when the rule fires: its annotations,
// its title where Name has it from the uid, its notification settings, the
// dashboard and panel it is on, ids, updated, provenance, the org, and what
// the replay is run with (Late, Scrape, the scrape intervals). Its form
// begins "parsed grafana", which no PromQL or Loki rule's does. ok is false
// for a rule with no Graph (not supported, paused, judged from Grafana's
// history), and for a Graph that can't be printed by value.
func (r Rule) parsedGrafana() (string, bool) {
	g := r.Graph
	if g == nil || g.src == nil || r.Unsupported != "" {
		return "", false
	}
	var o strings.Builder
	w := func(parts ...string) {
		for _, p := range parts {
			o.WriteString(p)
			o.WriteByte(' ')
		}
		o.WriteString("; ")
	}
	q := strconv.Quote
	dur := func(d time.Duration) string { return strconv.FormatInt(int64(d), 10) }
	flt := func(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
	o.WriteString("parsed grafana ")
	w("name", q(r.Name), "uid", q(r.UID), "group", q(r.Group), "folder", q(r.Folder))
	w("for", dur(r.For), "keep", dur(r.KeepFiringFor), "interval", dur(r.Interval))
	labels := r.AlertLabels()
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		w("label", q(k), q(labels[k]))
	}
	w("cond", q(g.Cond), "logs", q(g.Logs), "nodata", q(g.NoData), "execerr", q(g.ExecErr),
		"missing", strconv.Itoa(g.MissingToResolve))

	// The queries. A reduce folded into a query is in Asks, which is what
	// Queries' text of it is made from.
	for _, ref := range slices.Sorted(maps.Keys(g.Queries)) {
		if _, ask := g.Asks[ref]; ask {
			continue
		}
		text := g.Queries[ref]
		if _, aside := g.Aside[ref]; g.Logs == "" && !aside {
			var ok bool
			if text, ok = parsedExpr(text); !ok {
				return "", false
			}
		}
		w("query", q(ref), q(text))
	}
	for _, ref := range slices.Sorted(maps.Keys(g.Asks)) {
		a := g.Asks[ref]
		text := a.Expr
		if !a.Logs {
			var ok bool
			if text, ok = parsedExpr(text); !ok {
				return "", false
			}
		}
		w("ask", q(ref), q(a.Fn), q(text), strconv.FormatBool(a.Numbers), strconv.FormatBool(a.Counts),
			dur(a.Window), dur(a.Res), strconv.FormatBool(a.Logs), strconv.FormatBool(a.Classic))
	}
	for _, ref := range slices.Sorted(maps.Keys(g.Shift)) {
		w("shift", q(ref), dur(g.Shift[ref]))
	}
	for _, ref := range slices.Sorted(maps.Keys(g.Data)) {
		w("data", q(ref), q(g.Data[ref].Present), q(g.Data[ref].UID))
	}
	for _, ref := range slices.Sorted(maps.Keys(g.Other)) {
		w("other", q(ref), q(g.Other[ref]))
	}
	for _, ref := range slices.Sorted(maps.Keys(g.Aside)) {
		w("aside", q(ref), q(g.Aside[ref]))
	}

	for _, ref := range slices.Sorted(maps.Keys(g.Steps)) {
		s := g.Steps[ref]
		switch {
		case s.Threshold != nil:
			t := s.Threshold
			w("threshold", q(ref), q(t.Input), q(t.Type), flt(t.A), flt(t.B))
		case s.Math != nil:
			m, ok := sse.Canon(s.Math)
			if !ok {
				return "", false
			}
			w("math", q(ref), q(m))
		case s.Reduce != nil:
			x := s.Reduce
			w("reduce", q(ref), q(x.Input), q(x.Fn), q(x.Mode), dur(x.Window))
		case s.Classic != nil:
			c := s.Classic
			w("classic", q(ref), strconv.Itoa(c.Of))
			for _, cd := range c.Conds {
				text := cd.Expr
				if g.Logs == "" {
					var ok bool
					if text, ok = parsedExpr(text); !ok {
						return "", false
					}
				}
				params := make([]string, len(cd.Params))
				for i, p := range cd.Params {
					params[i] = flt(p)
				}
				w("cond", q(cd.Query), q(cd.Reducer), q(cd.Type), strings.Join(params, ","), q(cd.Op),
					strconv.FormatBool(cd.Instant), q(cd.Present), q(cd.Value), q(cd.Oldest), q(cd.Points), q(text),
					dur(cd.Window), dur(cd.Res), strconv.FormatBool(cd.Reduced))
			}
		default:
			return "", false
		}
	}

	// What the rule's nodes have that the Graph doesn't keep.
	seen := map[string]bool{}
	var nodes []map[string]any
	for _, d := range list(g.src["data"]) {
		dm, ok := d.(map[string]any)
		if !ok {
			return "", false
		}
		if ref := str(dm["refId"]); seen[ref] {
			return "", false
		} else {
			seen[ref] = true
		}
		nodes = append(nodes, dm)
	}
	slices.SortFunc(nodes, func(a, b map[string]any) int { return strings.Compare(str(a["refId"]), str(b["refId"])) })
	for _, dm := range nodes {
		ref := str(dm["refId"])
		model, _ := dm["model"].(map[string]any)
		if str(dm["datasourceUid"]) == "__expr__" {
			if _, ok := g.Steps[ref]; !ok {
				if _, ok := g.Queries[ref]; !ok {
					continue // not worked out by the replay
				}
			}
			switch str(model["type"]) {
			case "reduce":
				set, _ := model["settings"].(map[string]any)
				switch mode := str(set["mode"]); mode {
				case "", "strict", "dropNN":
					w("reduce", q(ref), q(str(model["reducer"])), q(ifEmpty(mode, "strict")))
				case "replaceNN":
					v, ok := toFloat(set["replaceWith"])
					if _, has := set["replaceWith"]; has && !ok {
						return "", false
					}
					w("reduce", q(ref), q(str(model["reducer"])), q(mode), flt(v))
				default:
					return "", false
				}
			case "threshold":
				if conds := list(model["conditions"]); len(conds) == 1 {
					cm, _ := conds[0].(map[string]any)
					if u, has := cm["unloadEvaluator"]; has && u != nil {
						b, err := json.Marshal(u)
						if err != nil {
							return "", false
						}
						w("unload", q(ref), q(string(b)))
					}
				}
			}
			continue
		}
		_, replayed := g.Data[ref]
		_, other := g.Other[ref]
		if !replayed && !other {
			return "", false // a query the Graph has no place for
		}
		rt := "none"
		if v, has := dm["relativeTimeRange"]; has && v != nil {
			rm, _ := v.(map[string]any)
			from, fok := toFloat(rm["from"])
			to, tok := toFloat(rm["to"])
			if !fok || !tok {
				return "", false
			}
			rt = flt(from) + "," + flt(to)
		}
		w("node", q(ref), q(str(dm["datasourceUid"])), "rt", rt)
		if other {
			// A data source the replay doesn't read: all of its model, but
			// what Grafana fills in or only names, as it is filled in.
			m := maps.Clone(model)
			delete(m, "refId")
			delete(m, "datasource")
			if h, has := m["hide"]; has && !truthy(h) {
				delete(m, "hide")
			}
			if _, has := m["intervalMs"]; !has {
				m["intervalMs"] = 1000
			}
			if _, has := m["maxDataPoints"]; !has {
				m["maxDataPoints"] = 43200
			}
			b, err := json.Marshal(m)
			if err != nil {
				return "", false
			}
			w("model", q(ref), q(string(b)))
			continue
		}
		_, aside := g.Aside[ref]
		if g.Logs != "" || aside {
			// Grafana's Loki data source reads queryType alone (lokiQuery),
			// range when it has none.
			w("shape", q(ref), "loki", q(ifEmpty(str(model["queryType"]), "range")),
				"hide", strconv.FormatBool(truthy(model["hide"])))
			continue
		}
		instant := truthy(model["instant"])
		// Grafana's Prometheus takes a query with neither as a range one.
		rng := !instant
		if v, has := model["range"]; has {
			rng = truthy(v)
		}
		w("shape", q(ref), "instant", strconv.FormatBool(instant), "range", strconv.FormatBool(rng),
			"hide", strconv.FormatBool(truthy(model["hide"])), "format", q(ifEmpty(str(model["format"]), "time_series")))
	}
	return o.String(), true
}

// ifEmpty is v, or def when v is empty.
func ifEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
