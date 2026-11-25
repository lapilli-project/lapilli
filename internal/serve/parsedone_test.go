package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// prFileRules reads a pull request's files as the backtest port does, as
// the rules that came with it.
func prFileRules(t *testing.T, files ...backtestFile) []rules.Rule {
	t.Helper()
	s := &Server{cfg: Config{Loc: time.UTC, Loki: "http://loki.example:3100"}}
	p, why := s.prepare(backtestRequest{After: files}, Trust{Days: 30}, inEffectReads{loki: true})
	if why != "" {
		t.Fatalf("the port refuses it: %s", why)
	}
	return p.came
}

// A Loki ruler's rules as serve reads them from its configuration API
// (they carry the namespace of their group) and the same rules in a pull
// request's rule files (which never do), written otherwise in places, are
// one, rule by rule.
func TestParsedLokiRulerAndPullRequestFile(t *testing.T) {
	parallel(t)
	raw, err := os.ReadFile("../replay/testdata/loki-3.7.9/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/rules" {
			http.NotFound(w, r)
			return
		}
		w.Write(raw)
	}))
	defer srv.Close()
	served, _, err := source.LokiRules(context.Background(), source.Loki{URL: srv.URL}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string][]any
	if err := yaml.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	var files []backtestFile
	for _, ns := range slices.Sorted(func(yield func(string) bool) {
		for k := range all {
			if !yield(k) {
				return
			}
		}
	}) {
		data, err := yaml.Marshal(map[string]any{"groups": all[ns]})
		if err != nil {
			t.Fatal(err)
		}
		// One written otherwise: spaces in the call, the matchers the
		// other way round, a space after sum.
		text := strings.Replace(string(data), `sum(count_over_time({app="shop", level="error"}[30s]`, `sum ( count_over_time({level="error",app="shop"}[30s]`, 1)
		files = append(files, backtestFile{Path: ns, Content: text, Loki: true})
	}
	pr := prFileRules(t, files...)
	if len(served) != 9 || len(pr) != 9 {
		t.Fatalf("%d rules from the ruler, %d from the files", len(served), len(pr))
	}
	byName := map[string]rules.Rule{}
	for _, r := range pr {
		byName[r.Name] = r
	}
	written := 0
	for _, a := range served {
		b, ok := byName[a.Name]
		if !ok || a.Namespace == "" || b.Namespace != "" {
			t.Fatalf("%s: namespace %q from the ruler, %q from the file (read: %v)", a.Name, a.Namespace, b.Namespace, ok)
		}
		ap, aok := a.Parsed()
		bp, bok := b.Parsed()
		if !aok || !bok || ap != bp {
			t.Errorf("%s isn't one: %v %v\n%s\n%s", a.Name, aok, bok, ap, bp)
		}
		if a.Expr != b.Expr {
			written++
		}
	}
	if written != 1 {
		t.Errorf("%d rules written otherwise in the file, want 1", written)
	}
}

// A cloud's alarm as serve reads it from the cloud (its namespace is the
// source it is read from) and the same alarm in a pull request's file, the
// query written otherwise, are one.
func TestParsedAlarmAndPullRequestFile(t *testing.T) {
	parallel(t)
	g := alarmtest.NewGoogle("shop-prod")
	policy := map[string]any{"displayName": "Checkout", "enabled": true, "combiner": "OR",
		"conditions": []any{map[string]any{"displayName": "Checkout",
			"conditionPrometheusQueryLanguage": map[string]any{"query": `sum(rate(errors_total{app="shop",env="prod"}[5m])) > 5`, "duration": "300s", "alertRule": "CheckoutErrors"}}}}
	g.Add(policy)
	a, err := source.NewAlarms("gcp-prod", "https://monitoring.googleapis.com/v3/projects/shop-prod/alertPolicies", "test")
	if err != nil {
		t.Fatal(err)
	}
	a.Transport = &alarmtest.Transport{H: g}
	a.Reads.Sign = func(r *http.Request) error { r.Header.Set("Authorization", "Bearer x"); return nil }
	served, _, err := a.Read(context.Background())
	if err != nil || len(served) != 1 {
		t.Fatalf("%d rules from the cloud: %v", len(served), err)
	}
	file := clonePolicy(policy)
	file["conditions"].([]any)[0].(map[string]any)["conditionPrometheusQueryLanguage"].(map[string]any)["query"] = `sum( rate(errors_total{env="prod", app="shop"}[5m]) ) > 5`
	b, _ := json.Marshal(file)
	pr := prFileRules(t, backtestFile{Path: "checkout.json", Content: string(b)})
	if len(pr) != 1 {
		t.Fatalf("%d rules from the file", len(pr))
	}
	if served[0].Cloud != "google" || pr[0].Cloud != "google" || served[0].Namespace == "" || pr[0].Namespace != "" {
		t.Fatalf("the rules: %+v %+v", served[0], pr[0])
	}
	ap, aok := served[0].Parsed()
	bp, bok := pr[0].Parsed()
	if !aok || !bok || ap != bp || served[0].Expr == pr[0].Expr {
		t.Errorf("isn't one: %v %v\n%s\n%s", aok, bok, ap, bp)
	}
}

func clonePolicy(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}
