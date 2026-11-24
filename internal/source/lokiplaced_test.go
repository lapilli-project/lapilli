package source

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// A group of a Loki ruler's rule file is placed where the ruler runs the
// group of its name, by its last evaluation (Loki's hash isn't Lapilli's to
// check), when the file gives the same interval; one of another interval,
// or listed in two namespaces, is not.
func TestLokiFilesPlacedByTheRulersAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.yaml")
	os.WriteFile(path, []byte(`groups:
  - name: shop
    interval: 1m
    rules:
      - alert: ShopErrors
        expr: sum(count_over_time({app="shop"} |= "error" [5m])) > 5
  - name: api
    interval: 5m
    rules:
      - alert: ApiErrors
        expr: sum(count_over_time({app="api"} |= "error" [5m])) > 5
  - name: both
    interval: 1m
    rules:
      - alert: BothErrors
        expr: sum(count_over_time({app="both"} |= "error" [5m])) > 5
`), 0o644)
	rs, _, _, err := LokiFiles(path)
	if err != nil || len(rs) != 3 {
		t.Fatalf("%d rules, %v", len(rs), err)
	}
	l, err := rules.ReadListing([]byte(`{"status":"success","data":{"groups":[
	  {"name":"shop","file":"shop.yaml","interval":60,"lastEvaluation":"2026-11-11T20:15:11.6Z"},
	  {"name":"api","file":"shop.yaml","interval":60,"lastEvaluation":"2026-11-11T20:15:12.6Z"},
	  {"name":"both","file":"a.yaml","interval":60,"lastEvaluation":"2026-11-11T20:15:13.6Z"},
	  {"name":"both","file":"b.yaml","interval":60,"lastEvaluation":"2026-11-11T20:15:14.6Z"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	u := &rules.Unplaced{}
	rules.Replaying{LokiListing: l, Unplaced: u}.PlaceFiles(rs)
	got := map[string]time.Duration{}
	for _, r := range rs {
		got[r.Group] = r.Phase
	}
	if got["shop"] != 11*time.Second || got["api"] != 0 || got["both"] != 0 {
		t.Errorf("placed %v, want shop at 11s alone", got)
	}
	if len(u.Said("the Loki ruler's rules API")) != 3 {
		t.Errorf("said %q", u.Said("the Loki ruler's rules API"))
	}
}
