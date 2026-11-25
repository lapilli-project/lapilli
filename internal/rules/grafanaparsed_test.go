package rules

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// provFile is a rule as a person writes it in a provisioning file: what
// Grafana fills in on save is left out. provAPI is the same rule as
// Grafana's export gives it (grafana-13.3.1-export), those fields filled in.
func provFile() map[string]any {
	return map[string]any{
		"apiVersion": 1,
		"groups": []any{map[string]any{
			"name": "shop", "folder": "alerts", "interval": "1m",
			"rules": []any{map[string]any{
				"uid": "errs", "title": "Errors", "condition": "C",
				"data": []any{
					map[string]any{"refId": "A", "datasourceUid": "prom",
						"relativeTimeRange": map[string]any{"from": 600, "to": 0},
						"model":             map[string]any{"expr": `sum(rate(errors_total{job="shop"}[5m]))`, "instant": true}},
					map[string]any{"refId": "B", "datasourceUid": "__expr__",
						"model": map[string]any{"type": "reduce", "expression": "A", "reducer": "last"}},
					map[string]any{"refId": "C", "datasourceUid": "__expr__",
						"model": map[string]any{"type": "threshold", "expression": "B",
							"conditions": []any{map[string]any{"evaluator": map[string]any{"type": "gt", "params": []any{5}}}}}},
				},
				"noDataState": "OK", "execErrState": "Error", "for": "5m",
				"labels": map[string]any{"severity": "page", "team": "shop"},
			}},
		}},
	}
}

// rule0 is the first rule of a document, and node one of its data by refId.
func rule0(doc map[string]any) map[string]any {
	return doc["groups"].([]any)[0].(map[string]any)["rules"].([]any)[0].(map[string]any)
}

func node(doc map[string]any, ref string) map[string]any {
	for _, d := range rule0(doc)["data"].([]any) {
		if dm := d.(map[string]any); dm["refId"] == ref {
			return dm
		}
	}
	panic("no node " + ref)
}

func provAPI() map[string]any {
	doc := provFile()
	g := doc["groups"].([]any)[0].(map[string]any)
	g["orgId"] = 1
	r := rule0(doc)
	r["dashboardUid"], r["panelId"], r["isPaused"] = "", 0, false
	r["annotations"] = map[string]any{"summary": "Errors"}
	for _, d := range r["data"].([]any) {
		dm := d.(map[string]any)
		m := dm["model"].(map[string]any)
		m["refId"], m["intervalMs"], m["maxDataPoints"] = dm["refId"], 1000, 43200
		if dm["datasourceUid"] == "prom" {
			m["range"], m["hide"] = false, false
			m["datasource"] = map[string]any{"type": "prometheus", "uid": "prom"}
		} else {
			m["datasource"] = map[string]any{"type": "__expr__", "uid": "__expr__"}
			if m["type"] == "reduce" {
				m["settings"] = map[string]any{"mode": ""}
			}
		}
	}
	return doc
}

func provOne(t *testing.T, doc map[string]any) Rule {
	t.Helper()
	b, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := Parse(b)
	if err != nil || len(rs) != 1 {
		t.Fatalf("%d rules, %v:\n%s", len(rs), err, b)
	}
	return rs[0]
}

func provParsed(t *testing.T, doc map[string]any) string {
	t.Helper()
	r := provOne(t, doc)
	if r.Graph == nil {
		t.Fatalf("not read: %q", r.Unsupported)
	}
	s, ok := r.Parsed()
	if !ok {
		t.Fatalf("no Parsed: %+v", r)
	}
	return s
}

// A rule as a file writes it and as Grafana's API gives it are one Parsed,
// where their Whole aren't; and what changes when it fires makes another.
func TestGrafanaParsed(t *testing.T) {
	file, api := provFile(), provAPI()
	if provOne(t, file).Whole() == provOne(t, api).Whole() {
		t.Fatal("the file and the API write it alike")
	}
	want := provParsed(t, file)
	if !strings.HasPrefix(want, "parsed grafana ") {
		t.Errorf("not marked as Grafana's: %s", want)
	}
	if got := provParsed(t, api); got != want {
		t.Errorf("the API's rule is another:\n%s\n%s", got, want)
	}

	type edit = func(doc map[string]any)
	prom := func(doc map[string]any) map[string]any { return node(doc, "A")["model"].(map[string]any) }
	for name, change := range map[string]edit{
		"another threshold": func(d map[string]any) {
			node(d, "C")["model"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["evaluator"].(map[string]any)["params"] = []any{6}
		},
		"another threshold type": func(d map[string]any) {
			node(d, "C")["model"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["evaluator"].(map[string]any)["type"] = "gte"
		},
		"an unload evaluator": func(d map[string]any) {
			node(d, "C")["model"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["unloadEvaluator"] = map[string]any{"type": "lt", "params": []any{3}}
		},
		"another expression":   func(d map[string]any) { prom(d)["expr"] = `sum(rate(errors_total{job="cart"}[5m]))` },
		"another window":       func(d map[string]any) { prom(d)["expr"] = `sum(rate(errors_total{job="shop"}[10m]))` },
		"another data source":  func(d map[string]any) { node(d, "A")["datasourceUid"] = "prom2" },
		"another for":          func(d map[string]any) { rule0(d)["for"] = "10m" },
		"a keepFiringFor":      func(d map[string]any) { rule0(d)["keepFiringFor"] = "5m" },
		"another noDataState":  func(d map[string]any) { rule0(d)["noDataState"] = "Alerting" },
		"another execErrState": func(d map[string]any) { rule0(d)["execErrState"] = "OK" },
		"missing evals":        func(d map[string]any) { rule0(d)["missing_series_evals_to_resolve"] = 5 },
		"another label":        func(d map[string]any) { rule0(d)["labels"].(map[string]any)["severity"] = "ticket" },
		"a label more":         func(d map[string]any) { rule0(d)["labels"].(map[string]any)["extra"] = "x" },
		"another condition": func(d map[string]any) {
			rule0(d)["condition"] = "B"
		},
		"another time range from": func(d map[string]any) { node(d, "A")["relativeTimeRange"].(map[string]any)["from"] = 1200 },
		"another time range to":   func(d map[string]any) { node(d, "A")["relativeTimeRange"].(map[string]any)["to"] = 60 },
		"no time range":           func(d map[string]any) { delete(node(d, "A"), "relativeTimeRange") },
		"a range query":           func(d map[string]any) { prom(d)["instant"] = false },
		"a hidden query":          func(d map[string]any) { prom(d)["hide"] = true },
		"another format":          func(d map[string]any) { prom(d)["format"] = "table" },
		"another reducer":         func(d map[string]any) { node(d, "B")["model"].(map[string]any)["reducer"] = "max" },
		"a reduce's mode": func(d map[string]any) {
			node(d, "B")["model"].(map[string]any)["settings"] = map[string]any{"mode": "dropNN"}
		},
		"a reduce's replacement": func(d map[string]any) {
			node(d, "B")["model"].(map[string]any)["settings"] = map[string]any{"mode": "replaceNN", "replaceWithValue": 0}
		},
		"an interval factor": func(d map[string]any) { prom(d)["intervalFactor"] = 2 },
		"another group":      func(d map[string]any) { d["groups"].([]any)[0].(map[string]any)["name"] = "cart" },
		"another folder":     func(d map[string]any) { d["groups"].([]any)[0].(map[string]any)["folder"] = "other" },
		"another interval":   func(d map[string]any) { d["groups"].([]any)[0].(map[string]any)["interval"] = "30s" },
		"another uid":        func(d map[string]any) { rule0(d)["uid"] = "errs2" },
		"another name":       func(d map[string]any) { rule0(d)["title"] = "Errors 2" },
		"a title's space":    func(d map[string]any) { rule0(d)["title"] = "Errors " },
	} {
		for shape, base := range map[string]func() map[string]any{"file": provFile, "API": provAPI} {
			d := base()
			change(d)
			if got := provParsed(t, d); got == want {
				t.Errorf("%s (%s): the same Parsed", name, shape)
			}
		}
	}

	for name, change := range map[string]edit{
		"an annotation": func(d map[string]any) {
			rule0(d)["annotations"] = map[string]any{"summary": "Errors are high", "runbook": "https://example.com"}
		},
		"ids and provenance": func(d map[string]any) {
			rule0(d)["id"], rule0(d)["updated"], rule0(d)["provenance"] = 12, "2026-01-01T00:00:00Z", "file"
			rule0(d)["orgID"], rule0(d)["dashboardUid"], rule0(d)["panelId"] = 1, "dash", 7
		},
		"notification settings": func(d map[string]any) { rule0(d)["notification_settings"] = map[string]any{"receiver": "slack"} },
		"intervalMs":            func(d map[string]any) { prom(d)["intervalMs"] = 15000 },
		"maxDataPoints":         func(d map[string]any) { prom(d)["maxDataPoints"] = 100 },
		"a legend":              func(d map[string]any) { prom(d)["legendFormat"], prom(d)["editorMode"] = "{{pod}}", "code" },
		"the model's datasource": func(d map[string]any) {
			prom(d)["datasource"] = map[string]any{"type": "prometheus", "uid": "prom"}
		},
		"the expression's model": func(d map[string]any) {
			m := node(d, "C")["model"].(map[string]any)
			m["intervalMs"], m["maxDataPoints"], m["refId"] = 1000, 43200, "C"
		},
		"a range written": func(d map[string]any) { prom(d)["range"] = false },
		"the expression spelt": func(d map[string]any) {
			prom(d)["expr"] = "sum ( rate ( errors_total { job = \"shop\" } [ 5m ] ) ) # note"
		},
		"a threshold as a float": func(d map[string]any) {
			node(d, "C")["model"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["evaluator"].(map[string]any)["params"] = []any{5.0}
		},
		"the default state written": func(d map[string]any) { rule0(d)["missing_series_evals_to_resolve"] = 2 },
		"a duration written":        func(d map[string]any) { rule0(d)["for"] = "300s" },
		"an interval factor of 0":   func(d map[string]any) { prom(d)["intervalFactor"] = 0 },
		"an interval factor of 1":   func(d map[string]any) { prom(d)["intervalFactor"] = 1.0 },
		"no mode, written": func(d map[string]any) {
			node(d, "B")["model"].(map[string]any)["settings"] = map[string]any{"mode": ""}
		},
	} {
		for shape, base := range map[string]func() map[string]any{"file": provFile, "API": provAPI} {
			d := base()
			change(d)
			if got := provParsed(t, d); got != want {
				t.Errorf("%s (%s): another Parsed:\n%s\n%s", name, shape, got, want)
			}
		}
	}

	// The order the keys are written in, and the rules' labels written apart
	// or together, change nothing: the same document, built again.
	for i := 0; i < 20; i++ {
		if got := provParsed(t, provAPI()); got != want {
			t.Fatalf("read twice otherwise:\n%s\n%s", got, want)
		}
	}
}

// A rule over a math expression, a classic condition, a range query with a
// reduce: each by value, and its spelling not.
func TestGrafanaParsedSteps(t *testing.T) {
	two := func(expr string) map[string]any {
		d := provFile()
		r := rule0(d)
		r["data"] = []any{
			map[string]any{"refId": "A", "datasourceUid": "prom", "relativeTimeRange": map[string]any{"from": 600, "to": 0},
				"model": map[string]any{"expr": "x", "instant": true}},
			map[string]any{"refId": "B", "datasourceUid": "prom", "relativeTimeRange": map[string]any{"from": 600, "to": 0},
				"model": map[string]any{"expr": "y", "instant": true}},
			map[string]any{"refId": "C", "datasourceUid": "__expr__", "model": map[string]any{"type": "math", "expression": expr}},
		}
		return d
	}
	want := provParsed(t, two("$A / $B > 0.5"))
	for _, same := range []string{"${A}/${B}>.5", "($A / $B) > 0.50", "$A  /  $B  >  5e-1"} {
		if got := provParsed(t, two(same)); got != want {
			t.Errorf("%q: another Parsed", same)
		}
	}
	for _, other := range []string{"$A / $B > 0.6", "$A * $B > 0.5", "$B / $A > 0.5", "$A / ($B > 0.5)"} {
		if got := provParsed(t, two(other)); got == want {
			t.Errorf("%q: the same Parsed", other)
		}
	}

	// Grafana's math lexer refuses these, so the rule is no rule it loads.
	for _, bad := range []string{"$A>-1", "$A--$B", "$A / $B ^ 2 > 0.5"} {
		if r := provOne(t, two(bad)); r.Graph != nil {
			if s, ok := r.Parsed(); ok {
				t.Errorf("%q: a Parsed for what Grafana's lexer refuses: %s", bad, s)
			}
		}
	}

	classic := func(reducer string, value int) map[string]any {
		d := provFile()
		r := rule0(d)
		r["data"] = []any{
			map[string]any{"refId": "A", "datasourceUid": "prom", "relativeTimeRange": map[string]any{"from": 600, "to": 0},
				"model": map[string]any{"expr": "x"}},
			map[string]any{"refId": "C", "datasourceUid": "__expr__", "model": map[string]any{"type": "classic_conditions",
				"conditions": []any{map[string]any{
					"evaluator": map[string]any{"type": "gt", "params": []any{value}},
					"operator":  map[string]any{"type": "and"},
					"query":     map[string]any{"params": []any{"A"}},
					"reducer":   map[string]any{"type": reducer},
				}}}},
		}
		return d
	}
	want = provParsed(t, classic("avg", 5))
	if provParsed(t, classic("avg", 5)) != want || provParsed(t, classic("max", 5)) == want || provParsed(t, classic("avg", 6)) == want {
		t.Error("a classic condition isn't held by value")
	}
}

// What the recorded answers are: the export of Grafana 13.3.1 with the
// fields it fills in taken out, as a file would leave them, is one rule
// each; and the two measured edits (a threshold, an annotation) are told.
func TestGrafanaParsedRecorded(t *testing.T) {
	for _, path := range []string{"../replay/testdata/grafana-13.3.1-export/rules.yaml", "../replay/testdata/grafana-13.3.1-change/rules-before.yaml",
		"../replay/testdata/grafana-13.3.1-scraped/rules.yaml", "../replay/testdata/grafana-13.3.1-loki/rules.yaml"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		api, err := Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		for _, g := range doc["groups"].([]any) {
			delete(g.(map[string]any), "orgId")
			for _, r := range g.(map[string]any)["rules"].([]any) {
				rm := r.(map[string]any)
				delete(rm, "dashboardUid")
				delete(rm, "panelId")
				delete(rm, "isPaused")
				delete(rm, "annotations")
				for _, d := range rm["data"].([]any) {
					m := d.(map[string]any)["model"].(map[string]any)
					delete(m, "refId")
					// Grafana fills in 1000 and 43200; a query that says another
					// means it.
					if v, _ := toFloat(m["intervalMs"]); v == 1000 {
						delete(m, "intervalMs")
					}
					if v, _ := toFloat(m["maxDataPoints"]); v == 43200 {
						delete(m, "maxDataPoints")
					}
					delete(m, "range")
				}
			}
		}
		out, _ := yaml.Marshal(doc)
		file, err := Parse(out)
		if err != nil || len(file) != len(api) {
			t.Fatalf("%s: %d rules, %v", path, len(file), err)
		}
		read := 0
		for i := range api {
			ap, aok := api[i].Parsed()
			fp, fok := file[i].Parsed()
			if aok != fok || ap != fp {
				t.Errorf("%s: %s: the file's and the API's are %t, %t, one %t", path, api[i].Name, fok, aok, ap == fp)
			}
			if aok {
				read++
			}
		}
		if read == 0 {
			t.Errorf("%s: none read", path)
		}
	}

	read := func(name string) map[string]Rule {
		b, err := os.ReadFile("../replay/testdata/grafana-13.3.1-change/" + name)
		if err != nil {
			t.Fatal(err)
		}
		rs, err := Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Rule{}
		for _, r := range rs {
			out[r.UID] = r
		}
		return out
	}
	before, after := read("rules-before.yaml"), read("rules-after.yaml")
	differ, annotated := 0, 0
	for _, uid := range slices.Sorted(maps.Keys(before)) {
		b, a := before[uid], after[uid]
		bp, bok := b.Parsed()
		ap, aok := a.Parsed()
		if !bok || !aok {
			continue
		}
		if bp != ap {
			differ++
		} else if b.Digest != a.Digest {
			annotated++
		}
	}
	if differ != 1 || annotated != 1 {
		t.Errorf("rules changed in what fires: %d, in what it says alone: %d: want 1 and 1", differ, annotated)
	}
}

// A Grafana rule is never one with a PromQL or a Loki rule's, and what the
// replay can't read is refused.
func TestGrafanaParsedRefused(t *testing.T) {
	g := provOne(t, provFile())
	gp, _ := g.Parsed()
	prom := Rule{Name: "Errors", Group: "shop", Expr: `sum(rate(errors_total{job="shop"}[5m])) > 5`, For: 5 * time.Minute, Interval: time.Minute}
	pp, ok := prom.Parsed()
	if !ok || pp == gp {
		t.Errorf("a PromQL rule: %t, %q", ok, pp)
	}
	loki := prom
	loki.Logs = LokiRuler
	if lp, ok := loki.Parsed(); ok && lp == gp {
		t.Errorf("a Loki rule: %q", lp)
	}

	paused := provFile()
	rule0(paused)["isPaused"] = true
	unsupported := provFile()
	node(unsupported, "B")["model"].(map[string]any)["type"] = "resample"
	templated := provFile()
	node(templated, "A")["model"].(map[string]any)["expr"] = `up > $threshold`
	unreadable := provFile()
	node(unreadable, "A")["model"].(map[string]any)["expr"] = "x > 1_000"
	badMode := provFile()
	node(badMode, "B")["model"].(map[string]any)["settings"] = map[string]any{"mode": "other"}
	badRange := provFile()
	node(badRange, "A")["relativeTimeRange"] = map[string]any{"from": "a", "to": 0}
	dup := provFile()
	rule0(dup)["data"] = append(rule0(dup)["data"].([]any), node(dup, "A"))
	for name, d := range map[string]map[string]any{"paused": paused, "unsupported": unsupported, "templated": templated,
		"unreadable": unreadable, "a mode": badMode, "a time range": badRange, "a ref twice": dup} {
		b, _ := yaml.Marshal(d)
		rs, err := Parse(b)
		if err != nil || len(rs) != 1 {
			t.Fatalf("%s: %d rules, %v", name, len(rs), err)
		}
		if s, ok := rs[0].Parsed(); ok {
			t.Errorf("%s: %s", name, s)
		}
	}
	if s, ok := (Rule{Name: "G", Graph: &Graph{}}).Parsed(); ok {
		t.Errorf("a Graph read from nothing: %s", s)
	}
}

// A Grafana rule on Loki: its LogQL written with other spacing is the same
// rule, as parsedLogQL reads it, where its text is another.
func TestGrafanaParsedLogQL(t *testing.T) {
	b, err := os.ReadFile("../replay/testdata/grafana-13.3.1-loki/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	spaced := 0
	for _, g := range doc["groups"].([]any) {
		for _, r := range g.(map[string]any)["rules"].([]any) {
			for _, d := range r.(map[string]any)["data"].([]any) {
				m := d.(map[string]any)["model"].(map[string]any)
				if e, ok := m["expr"].(string); ok && strings.Contains(e, "{") {
					m["expr"] = "  " + strings.ReplaceAll(e, ", ", " ,  ") + "\n"
					spaced++
				}
			}
		}
	}
	if spaced == 0 {
		t.Fatal("no LogQL query to write otherwise")
	}
	out, _ := yaml.Marshal(doc)
	was, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	now, err := Parse(out)
	if err != nil || len(now) != len(was) {
		t.Fatalf("%d rules, %v", len(now), err)
	}
	one, other := 0, 0
	for i := range was {
		if was[i].Graph == nil || was[i].Graph.Logs == "" {
			continue
		}
		wp, wok := was[i].Parsed()
		np, nok := now[i].Parsed()
		if wok != nok || wp != np {
			t.Errorf("%s: parsed %t, %t, one %t", was[i].Name, wok, nok, wp == np)
		}
		if wok {
			one++
		}
		if was[i].Whole() != now[i].Whole() {
			other++
		}
	}
	if one == 0 || other == 0 {
		t.Errorf("%d rules on Loki one as parsed, %d of them another by text", one, other)
	}
}

// Grafana 13.3 (pkg/expr UnmarshalReduceCommand) reads a reduce's
// replacement as settings.replaceWithValue, a number, with mode replaceNN:
// the value is held, and what Grafana refuses (a mode it doesn't know,
// strict among them; replaceNN with no value or a string; settings that
// isn't a map; an interval factor that isn't an integer) has no Parsed.
func TestGrafanaParsedReduceAsGrafanaReadsIt(t *testing.T) {
	with := func(change func(d map[string]any)) map[string]any {
		d := provFile()
		change(d)
		return d
	}
	reduce := func(settings any) map[string]any {
		return with(func(d map[string]any) { node(d, "B")["model"].(map[string]any)["settings"] = settings })
	}
	replace := func(v any) map[string]any {
		return reduce(map[string]any{"mode": "replaceNN", "replaceWithValue": v})
	}
	if provParsed(t, replace(0)) == provParsed(t, replace(1)) {
		t.Error("two replacements are one")
	}
	if provParsed(t, reduce(map[string]any{"mode": "dropNN", "replaceWithValue": 3})) != provParsed(t, reduce(map[string]any{"mode": "dropNN"})) {
		t.Error("a replacement Grafana doesn't use is held")
	}
	if provParsed(t, replace(1)) != provParsed(t, replace(1.0)) {
		t.Error("1 and 1.0 aren't one")
	}
	for name, d := range map[string]map[string]any{
		"strict":                  reduce(map[string]any{"mode": "strict"}),
		"another mode":            reduce(map[string]any{"mode": "dropnn"}),
		"a mode not a string":     reduce(map[string]any{"mode": 1}),
		"a null mode":             reduce(map[string]any{"mode": nil}),
		"replaceNN with no value": reduce(map[string]any{"mode": "replaceNN"}),
		"the old key":             reduce(map[string]any{"mode": "replaceNN", "replaceWith": 0}),
		"a string value":          replace("0"),
		"settings a list":         reduce([]any{}),
		"settings null":           reduce(nil),
		"a fractional factor":     with(func(d map[string]any) { node(d, "A")["model"].(map[string]any)["intervalFactor"] = 1.5 }),
		"a string factor":         with(func(d map[string]any) { node(d, "A")["model"].(map[string]any)["intervalFactor"] = "2" }),
	} {
		r := provOne(t, d)
		if r.Graph == nil {
			continue // not read at all
		}
		if s, ok := r.Parsed(); ok {
			t.Errorf("%s: a Parsed for what Grafana refuses: %s", name, s)
		}
	}
}
