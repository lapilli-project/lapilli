package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
)

// lokiRuler answers a Loki 3.7.9 ruler's two rules APIs as it was seen to:
// the configuration API without the keep_firing_for and query_offset the
// files had, and the Prometheus-shaped one with when each group last ran.
func lokiRuler(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scope-OrgID") != "team-a" || r.Header.Get("Authorization") != "Bearer loki-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, "no org id")
			return
		}
		switch r.URL.Path {
		case "/loki/api/v1/rules":
			fmt.Fprint(w, `shop.yaml:
    - name: shop
      interval: 10s
      rules:
        - alert: ShopErrors
          expr: sum(count_over_time({app="shop", level="error"}[30s])) > 20
    - name: api
      interval: 1m
      rules:
        - alert: ApiSlow
          expr: max by (app) (max_over_time({app="api"} | logfmt | unwrap latency_ms [1m])) > 500
          for: 1m
`)
		case "/prometheus/api/v1/rules":
			fmt.Fprint(w, `{"status":"success","data":{"groups":[
				{"name":"shop","file":"shop.yaml","interval":10,"lastEvaluation":"2026-11-11T19:13:54.56508092Z","rules":[]},
				{"name":"api","file":"shop.yaml","interval":60,"lastEvaluation":"2026-11-11T19:13:11.640845929Z","rules":[]}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestLokiLoaded(t *testing.T) {
	srv := lokiRuler(t)
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(tok, []byte("loki-token\n"), 0o600)
	rs, docs, places, err := LokiLoaded(context.Background(), Loki{URL: srv.URL, TokenFile: tok, Tenant: "team-a"}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || len(docs) != 1 || docs[0].Logs != rules.LokiRuler || docs[0].Namespace != "shop.yaml" || len(places) != 2 {
		t.Fatalf("rules %d, docs %+v, places %v", len(rs), docs, places)
	}
	for _, r := range rs {
		want := map[string]time.Duration{"ShopErrors": 4 * time.Second, "ApiSlow": 11 * time.Second}[r.Name]
		if r.Logs != rules.LokiRuler || r.Namespace != "shop.yaml" || r.Phase != want {
			t.Errorf("%s: logs %q, namespace %q, placed %s, want %s", r.Name, r.Logs, r.Namespace, r.Phase, want)
		}
	}
	// Read again from the ledger, the same rules.
	again, err := rules.FromDocs(docs)
	if err != nil || len(again) != 2 || again[0].Key() != rs[0].Key() {
		t.Errorf("read again: %v %v", again, err)
	}
	// Without the tenant, Loki refuses, and so the read fails.
	if _, _, _, err := LokiLoaded(context.Background(), Loki{URL: srv.URL, TokenFile: tok}, "test", nil); err == nil ||
		!strings.Contains(err.Error(), "--loki-loaded-rules") {
		t.Errorf("refused: %v", err)
	}
}

func TestLokiFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(`groups:
  - name: shop
    interval: 10s
    rules:
      - alert: ShopErrors
        expr: sum(count_over_time({app="shop"} |= "error" [30s])) > 20
        keep_firing_for: 1m
`), 0o644)
	rs, docs, err := LokiFiles(dir)
	if err != nil || len(rs) != 1 || rs[0].Logs != rules.LokiRuler || rs[0].KeepFiringFor != 0 || docs[0].Logs != rules.LokiRuler {
		t.Fatalf("%+v %+v %v", rs, docs, err)
	}
	// Grafana's rules aren't a Loki ruler's.
	os.WriteFile(filepath.Join(dir, "grafana.yaml"), []byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: a
        title: A
        condition: A
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: up}
`), 0o644)
	if _, _, err := LokiFiles(dir); err == nil || !strings.Contains(err.Error(), "isn't a Loki ruler's rule") {
		t.Errorf("a Grafana file: %v", err)
	}
}

// TestGrafanaKinds: the type each data source is, as Grafana says, a
// plugin's id; anything else isn't kept.
func TestGrafanaKinds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/datasources/uid/loki":
			fmt.Fprint(w, `{"uid":"loki","type":"loki","url":"http://loki:3100","jsonData":{}}`)
		case "/api/datasources/uid/prom":
			fmt.Fprint(w, `{"uid":"prom","type":"prometheus","jsonData":{"timeInterval":"30s"}}`)
		case "/api/datasources/uid/odd":
			fmt.Fprint(w, `{"uid":"odd","type":"<script>"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ds, kinds, missed, err := GrafanaDataSources(context.Background(), srv.URL, "", "test", []string{"loki", "prom", "odd", "gone"})
	if err != nil || kinds["loki"] != "loki" || kinds["prom"] != "prometheus" || kinds["odd"] != "" || ds["prom"] != 30*time.Second ||
		len(missed) != 1 || !strings.HasPrefix(missed[0], "gone") {
		t.Errorf("%v %v %v %v", ds, kinds, missed, err)
	}
}

// TestLogsFor: a Loki ruler's rule is asked of --loki; a Grafana rule of
// its data source through Grafana's proxy, one client a data source, or,
// with no --grafana, of --loki.
func TestLogsFor(t *testing.T) {
	loki := prom.New("http://loki:3100", "test", prom.Loki())
	made := map[string]int{}
	l := &Logs{Loki: loki, Grafana: func(uid string) replay.History {
		made[uid]++
		return GrafanaLoki("http://grafana:3000", "", "test", uid)
	}}
	grafanaRule := rules.Rule{Logs: "lk", Graph: &rules.Graph{}}
	a, _ := l.For(grafanaRule)
	b, _ := l.For(grafanaRule)
	c, _ := l.For(rules.Rule{Logs: rules.LokiRuler})
	if a != b || made["lk"] != 1 || c != replay.History(loki) || a == c {
		t.Errorf("made %v", made)
	}
	if _, err := (&Logs{}).For(rules.Rule{Logs: rules.LokiRuler}); err == nil {
		t.Error("no --loki should be an error")
	}
	if got, _ := (&Logs{Loki: loki}).For(grafanaRule); got != replay.History(loki) {
		t.Error("with no --grafana, a Grafana rule's Loki is --loki")
	}
}
