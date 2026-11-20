package serve

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A rewrite checked against serve's own expectations for the rule says
// so in its post: the rule's, as serve holds it, not the agent's.
func TestTestExpectationServesPost(t *testing.T) {
	s, _, _ := draftServer(t, errorsHeld, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	agent := connect(t, ts.URL, "ed@example.com")
	rewrite := "groups:\n  - name: shop\n    rules:\n      - alert: Errors\n        expr: errors > 5\n        for: 15m\n        labels: {severity: page}\n"
	var out testExpectOut
	if failed, text := call(t, agent, "test_expectation", map[string]any{"rule": "Errors", "rewrite": rewrite}, &out); failed || out.HeldBy != "serve" || !out.Meets {
		t.Fatalf("%s %+v", text, out)
	}
	if !strings.Contains(out.Post, "it meets every expectation Lapilli holds the rule to,") || strings.Contains(out.Post, "the agent gave") {
		t.Errorf("the post:\n%s", out.Post)
	}
}
