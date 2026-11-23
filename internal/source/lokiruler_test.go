package source

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestLokiRuler: a Loki's ruler is read and its groups written through
// /loki/api/v1/rules, Cortex's API under Loki's path, each request with
// the Loki's tenant and token. A document read is sourced where a change
// to it is written (Owns), and a group read again is the unit a change
// replaces.
func TestLokiRuler(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.EscapedPath())
		if r.Header.Get("X-Scope-OrgID") != "acme" || r.Header.Get("Authorization") != "Bearer loki-reads-0123456789" {
			http.Error(w, "no org id", http.StatusUnauthorized)
			return
		}
		switch r.URL.EscapedPath() {
		case "/loki/api/v1/rules":
			io.WriteString(w, "shop/logs:\n    - name: api\n      rules:\n        - alert: LogErrors\n          expr: sum(count_over_time({app=\"shop\"} |= \"error\" [5m])) > 9\n          for: 5m\n")
		case "/loki/api/v1/rules/shop%2Flogs/api":
			io.WriteString(w, "name: api\nrules:\n    - alert: LogErrors\n      expr: sum(count_over_time({app=\"shop\"} |= \"error\" [5m])) > 9\n      for: 5m\n")
		default:
			http.Error(w, "group does not exist", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("loki-reads-0123456789\n"), 0o600)
	r := Loki{URL: srv.URL + "/", Tenant: "acme", TokenFile: tok}.Ruler("test", nil)
	if r.Kind != RulerConfig || r.URL != srv.URL+"/loki/api/v1/rules" || r.flag() != "--loki-loaded-rules" {
		t.Fatalf("the ruler: %+v", r)
	}
	rs, docs, err := r.Read(context.Background())
	if err != nil || len(rs) != 1 || len(docs) != 1 {
		t.Fatalf("%v %+v %+v", err, rs, docs)
	}
	if err := r.Owns(docs[0].Source, "shop/logs", "api"); err != nil || docs[0].Source != srv.URL+"/loki/api/v1/rules/shop%2Flogs" {
		t.Errorf("the document's source %s isn't where its namespace is written: %v", docs[0].Source, err)
	}
	if err := r.Owns(docs[0].Source, "shop", "api"); err == nil {
		t.Error("a source owned for another namespace")
	}
	u, err := r.Unit(context.Background(), "shop/logs", "api")
	if err != nil || u.Gone || u.Hash == "" || r.WriteAt(u) != docs[0].Source {
		t.Fatalf("the unit: %v %+v", err, u)
	}
	if u, err := r.Unit(context.Background(), "shop/logs", "gone"); err != nil || !u.Gone {
		t.Errorf("a group that isn't there: %v %+v", err, u)
	}
	want := []string{"GET /loki/api/v1/rules", "GET /loki/api/v1/rules/shop%2Flogs/api", "GET /loki/api/v1/rules/shop%2Flogs/gone"}
	if len(asked) != len(want) {
		t.Fatalf("asked %v", asked)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Errorf("asked %v, want %v", asked, want)
		}
	}
}
