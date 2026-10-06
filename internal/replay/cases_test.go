package replay

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/freeze"
	"github.com/lapilli-project/lapilli/internal/metrics"
)

// Every case this repository ships must be exactly what was sealed, and must be solvable: the decisive
// evidence its answer key names has to be in the frozen copy an agent is given. A case that fails the
// second half measures nothing, and nothing else would notice.
func TestEveryShippedCaseIsIntactAndSolvable(t *testing.T) {
	dirs, _ := filepath.Glob("../../cases/*/" + casefile.SpecName)
	if len(dirs) < 3 {
		t.Fatalf("found %d cases, want at least the three this was built with", len(dirs))
	}
	for _, spec := range dirs {
		dir := filepath.Dir(spec)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			if problems, err := casefile.Verify(dir); err != nil || len(problems) != 0 {
				t.Fatalf("not intact: %v %v", problems, err)
			}
			c, err := casefile.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if c.ID != filepath.Base(dir) {
				t.Errorf("the case in %s calls itself %q", dir, c.ID)
			}
			info, err := casefile.LoadFreezeInfo(dir)
			if err != nil || info.FreezeTime == 0 {
				t.Fatalf("freeze.json: %+v %v", info, err)
			}

			snapshot := filepath.Join(t.TempDir(), "kubernetes")
			if err := Extract(filepath.Join(dir, casefile.KubernetesName), snapshot); err != nil {
				t.Fatal(err)
			}
			found, err := freeze.EvidencePresent(snapshot, c.Patterns(casefile.StoreKubernetes))
			if err != nil {
				t.Fatal(err)
			}
			for pattern, ok := range found {
				if !ok {
					t.Errorf("decisive evidence %q is not in the frozen copy", pattern)
				}
				if recorded, known := info.EvidenceInSnapshot[pattern]; !known || recorded != ok {
					t.Errorf("freeze.json says %v (recorded: %v) for %q; the copy says %v", recorded, known, pattern, ok)
				}
			}

			_, err = os.Stat(filepath.Join(dir, casefile.MetricsName))
			if has := err == nil; has != c.Metrics {
				t.Fatalf("case.yaml says metrics: %v; a metrics file exists: %v", c.Metrics, has)
			}
			if c.Metrics {
				store, err := metrics.Load(filepath.Join(dir, casefile.MetricsName))
				if err != nil {
					t.Fatal(err)
				}
				_, newest, ok := store.Bounds()
				if freezeMillis := metrics.FromSeconds(info.FreezeTime).UnixMilli(); !ok || newest > freezeMillis || freezeMillis-newest > 60_000 {
					t.Errorf("the newest sample is at %d and the freeze at %d; \"now\" would be empty or in the past", newest, freezeMillis)
				}
			} else if len(c.Patterns(casefile.StoreMetrics)) > 0 {
				t.Error("the answer key names evidence in a metrics store the case does not carry")
			}
		})
	}
}
