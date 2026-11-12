package ledger

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// undo017 takes back what 017 made, for the tests of the migrations
// before it.
var undo017 = []string{
	"DROP INDEX firings_refired",
	"DROP TABLE deploys",
	"DROP TABLE deploy_labels",
	"ALTER TABLE firings DROP COLUMN deploy",
	"ALTER TABLE days DROP COLUMN deploy",
	"ALTER TABLE days DROP COLUMN deploy_white",
	"ALTER TABLE weeks DROP COLUMN deploy",
	"ALTER TABLE weeks DROP COLUMN deploy_white",
}

// A ledger lapilli 0.28.1 wrote (schema 016: firings copied in from a
// source, six of an alert that re-fired five times, one an incident
// caught), opened by this one, migrates to 017: what it had kept, its
// stones and tallies as they were, no firing during a deploy; then deploys
// read mark its firings, and its re-fired alert is found by its index.
func TestMigrate017(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	in, err := os.Open("testdata/v0.28.1.db.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	z, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, z); err != nil {
		t.Fatal(err)
	}
	out.Close()
	l, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var v int
	l.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	if want := schemaVersion(t); v != want || want < 17 {
		t.Errorf("migrated to %d, want %d", v, want)
	}
	day := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	o := opts
	o.DeployAfter = 10 * time.Minute
	rep, err := l.Report(ctx, day, day.Add(24*time.Hour), o, false)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string][4]int{}
	for _, r := range rep.Rules {
		by[r.Rule] = [4]int{r.Firings, r.White, r.Refired, r.DuringDeploys}
	}
	if by["CheckoutErrors"] != [4]int{6, 2, 5, 0} || by["CartLatency"] != [4]int{1, 0, 0, 0} {
		t.Errorf("after migrating: %v", by)
	}
	if len(rep.Incidents) != 1 || len(rep.Incidents[0].CaughtBy) != 1 {
		t.Errorf("incidents after migrating: %+v", rep.Incidents)
	}
	worst, err := l.RefiredOf(ctx, "CheckoutErrors", day, day.Add(24*time.Hour), 1)
	if err != nil || len(worst) != 1 || worst[0].Refired != 5 || worst[0].Labels["service"] != "checkout" {
		t.Errorf("re-fired after migrating: %+v %v", worst, err)
	}
	var plan string
	rows, err := l.r.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT series_id, count(*) FROM firings
		WHERE rule_id = 1 AND refired = 1 AND excused = 0 AND start_at >= 0 AND start_at < 1 GROUP BY series_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, b, c int
		var d string
		rows.Scan(&a, &b, &c, &d)
		plan += d + "; "
	}
	rows.Close()
	if !strings.Contains(plan, "firings_refired") {
		t.Errorf("a rule's re-fired firings aren't read by their index: %s", plan)
	}
	// Deploys read mark its firings: the checkout deploy at 10:20 reaches
	// the firing at 10:30, ten minutes after.
	if _, err := l.SyncDeploys(ctx, []Deploy{{Start: day.Add(10*time.Hour + 20*time.Minute), Labels: map[string]string{"namespace": "shop"}}}, o, DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	rep, err = l.Report(ctx, day, day.Add(24*time.Hour), o, false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rep.Rules {
		n += r.DuringDeploys
	}
	if n != 1 {
		t.Errorf("after a deploy: %d firings during it, want 1", n)
	}
}
