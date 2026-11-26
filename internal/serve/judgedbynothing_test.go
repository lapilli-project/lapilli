package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"lapilli.dev/internal/verdict"
)

// not_judged is written with judged_by_nothing beside it, the same value,
// wherever it is written: the report's rules and firings, the summary's
// rows and MCP's rules. With a rule entry that judges Errors by nothing
// each has a count, so it isn't two zeros.
func TestJudgedByNothingBeside(t *testing.T) {
	parallel(t)
	no := false
	s := verdictServer(t, func(c *Config) { c.ImpactRules = []verdict.RuleEntry{{Rules: "^Errors$", Judged: &no}} })
	if err := s.l.JudgeAll(context.Background(), mustOptions(t, s), nil); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	pairs, counted := 0, 0
	var walk func(where string, v any)
	walk = func(where string, v any) {
		switch x := v.(type) {
		case map[string]any:
			old, hasOld := x["not_judged"]
			now, hasNow := x["judged_by_nothing"]
			if hasOld != hasNow || !reflect.DeepEqual(old, now) {
				t.Errorf("%s: not_judged %v (%v), judged_by_nothing %v (%v)", where, old, hasOld, now, hasNow)
			}
			if hasOld {
				pairs++
				if n, ok := old.(float64); ok && n > 0 {
					counted++
				}
				if b, ok := old.(bool); ok && b {
					counted++
				}
			}
			for _, c := range x {
				walk(where, c)
			}
		case []any:
			for _, c := range x {
				walk(where, c)
			}
		}
	}
	f := errorsFiring(t, s)
	day := f.Start.In(s.cfg.Loc).Format("2006-01-02")
	for name, path := range map[string]string{
		"the report":  "/api/v1/report?from=2026-10-12T00:00:00Z&to=2026-11-10T00:00:00Z",
		"the summary": "/api/v1/summary?from=" + day + "&to=" + day,
	} {
		code, body, _ := apiAs(t, h, http.MethodGet, path, nil, vViewer, "", "")
		var doc any
		if code != http.StatusOK || json.Unmarshal([]byte(body), &doc) != nil {
			t.Fatalf("%s: %d %.200s", name, code, body)
		}
		before := counted
		walk(name, doc)
		if counted == before {
			t.Errorf("%s: no not_judged with a count", name)
		}
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var rules rulesOut
	if failed, text := call(t, connect(t, ts.URL, vViewer), "rules", nil, &rules); failed || len(rules.Rules) == 0 {
		t.Fatalf("rules: %s", text)
	}
	for _, r := range rules.Rules {
		if r.NotJudged != r.JudgedByNothing {
			t.Errorf("MCP %s: not_judged %v, judged_by_nothing %v", r.Name, r.NotJudged, r.JudgedByNothing)
		}
		if r.Name == "Errors" && !r.JudgedByNothing {
			t.Errorf("MCP Errors isn't judged by nothing")
		}
	}
}
