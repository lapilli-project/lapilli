package serve

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// /api/v1/report?redact=true: the report with every name an alias, its
// numbers as they were; the rules', the team's, the services' and the
// incidents' names nowhere in it.
func TestReportRedacted(t *testing.T) {
	s := nocServer(t)
	h := s.Handler()
	span := "from=" + nocBase.Add(-15*24*time.Hour).UTC().Format(time.RFC3339) + "&to=" + now.Add(time.Hour).UTC().Format(time.RFC3339)
	_, plain, _ := get(t, h, "/api/v1/report?"+span)
	code, body, _ := get(t, h, "/api/v1/report?redact=true&"+span)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	for _, name := range []string{`"Flap"`, `"Noise"`, `"DiskFull"`, `"search"`, `"cart"`, `"INC-1"`, `"cloud-test"`, `"shop"`} {
		if !strings.Contains(plain, name) {
			t.Fatalf("the report unredacted lacks %s: a test of nothing", name)
		}
		if strings.Contains(body, name) {
			t.Errorf("redacted: %s is there", name)
		}
	}
	var a, b verdict.Report
	if err := json.Unmarshal([]byte(plain), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatal(err)
	}
	counts := func(r verdict.Report) []int {
		var out []int
		for _, rr := range r.Rules {
			out = append(out, rr.Firings, rr.White, rr.Refired, rr.DuringDeploys)
		}
		return out
	}
	if !slices.Equal(counts(a), counts(b)) || len(a.Firings) != len(b.Firings) || len(a.Incidents) != len(b.Incidents) {
		t.Errorf("numbers: %v, redacted %v", counts(a), counts(b))
	}
	// Without it, as it was; firings=false too.
	if _, again, _ := get(t, h, "/api/v1/report?redact=false&"+span); again != plain {
		t.Error("redact=false changed the report")
	}
	if code, body, _ := get(t, h, "/api/v1/report?firings=false&redact=true"); code != 200 || strings.Contains(body, `"Flap"`) {
		t.Errorf("firings=false: %d, Flap there: %v", code, strings.Contains(body, `"Flap"`))
	}
}
