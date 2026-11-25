package serve

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
)

// Changing a Loki ruler's rule read from its rule files (--loki-rules) by
// the git path, as a Prometheus rule file's rule is: the file edited in
// its tree, so every other line stays, handed to a recipe, read back.

// lokiFile has a comment, a rule with a keep_firing_for that Loki 3.7.9
// drops when it reads a file (so Lapilli doesn't read it) and a second
// rule, all of which an edit must leave as they are.
const lokiFile = `# Loki rules of checkout
groups:
  - name: logs
    rules:
      - alert: CheckoutLogErrors  # the noisy one
        expr: sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 5
        for: 2m
        keep_firing_for: 5m
        labels: {severity: page}
      - alert: CheckoutLogSlow
        expr: max(max_over_time({app="checkout"} | logfmt | unwrap latency_ms [1m])) > 500
        for: 3m
`

// lokiFilesServer is a serve that reads one Loki rule file with
// --loki-rules and no other rules, with changes on.
func lokiFilesServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "loki")
	os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, "logs.yaml")
	os.WriteFile(file, []byte(lokiFile), 0o644)
	s, _ := changesServer(t, func(c *Config) {
		c.RulesPath = ""
		c.Loki, c.LokiRules = "http://loki:3100", dir
	})
	s.h = replay.WithLogs(s.h, func(rules.Rule) (replay.History, error) { return &lokiLogs{}, nil })
	if err := s.look(context.Background(), new(string)); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), true)
	return s, file
}

// A for, a threshold and what a rule says, for a Loki rule file's rule:
// each is a proposal on the git path, with its LogQL backtest, whose file
// differs from the one read by the lines changed alone; approved, it is
// handed to a recipe by the hash of the file Lapilli read, and read back
// from the file with the change as applied; undone the same way.
func TestLokiFileChangeByPullRequest(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, file := lokiFilesServer(t)
	h, port := s.Handler(), s.IncidentsHandler()
	name := "CheckoutLogErrors"

	id := proposeRule(t, h, name, "for", url.Values{"for": {"15m"}})
	p := state(t, s, id)
	ev := proposalEvidence(p)
	if p.Path != ledger.PathGit || ev.NoPath != "" || ev.Backtest == nil || p.DocHash != rules.Hash([]byte(lokiFile)) {
		t.Fatalf("the proposal: path %q, why %q, backtest %+v", p.Path, ev.NoPath, ev.Backtest)
	}
	want := strings.Replace(lokiFile, "for: 2m", "for: 15m", 1)
	if p.Document != want {
		t.Errorf("the file as proposed isn't the file read with the for changed:\n%s", p.Diff)
	}
	if !strings.Contains(p.Document, "keep_firing_for: 5m") || !strings.Contains(p.Document, "# the noisy one") {
		t.Error("an edit dropped what the file holds that Lapilli doesn't read")
	}
	if ev.Backtest.Before.Firings != 1 || ev.Backtest.Before.White != 1 {
		t.Errorf("the backtest isn't the rule replayed in LogQL (one firing, which caught INC-1): %+v", ev.Backtest)
	}

	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p = state(t, s, id); p.State != ledger.StateReady || p.Backup != lokiFile {
		t.Fatalf("checked again: %s %q", p.State, p.Reason)
	}
	if got := handed(t, port); len(got) != 1 || got[0].Document != p.Document || got[0].DocHash != p.DocHash {
		t.Fatalf("handed out: %+v", got)
	}
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://git.example/logs/pull/1"})
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "merged"})
	os.WriteFile(file, []byte(p.Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p = state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("read back: %s %q", p.State, p.Reason)
	}

	// A threshold, in LogQL, and a text: the same path.
	th := state(t, s, proposeRule(t, h, "CheckoutLogSlow", "threshold", url.Values{"bound": {"0"}, "value": {"900"}}))
	if th.Path != ledger.PathGit || !strings.Contains(th.Document, "> 900") || strings.Contains(th.Document, "> 500") {
		t.Errorf("the threshold: path %q why %q\n%s", th.Path, proposalEvidence(th).NoPath, th.Diff)
	}
	code, body, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin",
		url.Values{"rule": {name}, "kind": {"says"}, "annotation": {"runbook_url"}, "text": {"https://wiki.example/logs"}})
	if code != http.StatusSeeOther {
		t.Fatalf("says: %d %s", code, zzMsg(body))
	}
	if sp := state(t, s, locID(hd)); sp.Path != ledger.PathGit || !strings.Contains(sp.Document, "https://wiki.example/logs") {
		t.Errorf("the text: path %q why %q", sp.Path, proposalEvidence(sp).NoPath)
	}
}

// A Loki rule file's rule deleted is its deletion in the file, as a
// Prometheus rule file's is; the rest of the file, and the other rule,
// stay.
func TestLokiFileDelete(t *testing.T) {
	parallel(t)
	s, _ := lokiFilesServer(t)
	form := url.Values{"rule": {"CheckoutLogSlow"}, "kind": {"delete"}}
	code, body, hd := ui(t, s.Handler(), http.MethodPost, "/proposals", bo, "same-origin", form)
	if code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, zzMsg(body))
	}
	p := state(t, s, locID(hd))
	if p.Path != ledger.PathGit || strings.Contains(p.Document, "CheckoutLogSlow") || !strings.Contains(p.Document, "CheckoutLogErrors") {
		t.Errorf("the deletion: path %q\n%s", p.Path, p.Diff)
	}
}
