package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// One alert in two groups is named apart in the ledger, and is still one
// alert: not the same condition as another rule.
func TestContentOneAlertTwoGroups(t *testing.T) {
	ctx := context.Background()
	s, dir := server(t, func(c *Config) { c.CheckContent = true })
	rule := `      - alert: HighErrors
        expr: errors > 5
        for: 5m
        labels: {severity: page}
        annotations: {summary: "errors", runbook_url: "https://runbooks.example/errors"}
`
	if err := os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte("groups:\n  - name: team-a\n    rules:\n"+rule+"  - name: slo\n    rules:\n"+rule), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	st, err := s.content(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.says["HighErrors [slo]"]; !ok {
		t.Fatalf("not named apart: %v", st.says)
	}
	for name, fs := range st.findings {
		t.Errorf("%s: %+v", name, fs)
	}
}
