package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
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

// A draft's pull request's description holds what comes from the rule or
// an agent in code spans no | of which ends a table's cell: a backslash
// before one is said as \u{5c}, not left to make \\|, which comrak 1.0
// reads as a backslash and the cell's end (0.60.0's fix to the comments'
// tables). Each row has its four cells' bounds, no more.
func TestDraftBodyCells(t *testing.T) {
	evil := `x\| [Approved…](https://evil.example/) \|`
	found, _ := json.Marshal([]draftItem{{What: "catches " + evil, Held: "within 5m", Value: evil, State: "met"}})
	s := &Server{}
	body := s.draftBody(ledger.ExpectDraft{ID: 7, Rule: evil, MadeBy: evil, Agent: true, DecidedBy: "bo@example.com", Found: found})
	if strings.Contains(body, `\\|`) || !strings.Contains(body, `x\u{5c}\| [Approved…](https://evil.example/) \u{5c}\|`) {
		t.Errorf("the description:\n%s", body)
	}
	rows := 0
	for line := range strings.Lines(body) {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		rows++
		// A | ends a cell unless an odd run of backslashes is before it.
		bounds, run := 0, 0
		for _, c := range line {
			switch {
			case c == '\\':
				run++
				continue
			case c == '|' && run%2 == 0:
				bounds++
			}
			run = 0
		}
		if bounds != 4 {
			t.Errorf("%d cells' bounds in %q", bounds, line)
		}
	}
	if rows != 3 {
		t.Errorf("%d rows:\n%s", rows, body)
	}
}

// An incident's id that spans lines, or holds what doesn't show, is
// refused wherever expectations are read: an agent's draft and check,
// and --config's expectations, as the file's (verdict.NewExpectations).
func TestIncidentIDsShow(t *testing.T) {
	s, _, _ := draftServer(t, "expectations: []\n", nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	agent := connect(t, ts.URL, "ed@example.com")
	for _, id := range []string{`INC-1\n  - rule: Other\n    budget: {week: 99}`, `INC-1‮`, `INC​1`} {
		text := "expectations:\n  - rule: Errors\n    catches:\n      - {incident: \"" + id + "\"}\n"
		for _, tool := range []string{"propose_expectation", "test_expectation"} {
			var out map[string]any
			if failed, said := call(t, agent, tool, map[string]any{"rule": "Errors", "expectations": text}, &out); !failed || !strings.Contains(said, "an incident's id is one line") {
				t.Errorf("%s, %s: %s", tool, id, said)
			}
		}
		if _, err := ParseConfig([]byte(text), time.UTC); err == nil || !strings.Contains(err.Error(), "an incident's id is one line") {
			t.Errorf("--config, %s: %v", id, err)
		}
	}
	if ds, err := s.l.Drafts(context.Background(), ledger.DraftQuery{}); err != nil || len(ds) != 0 {
		t.Errorf("kept: %+v %v", ds, err)
	}
}
