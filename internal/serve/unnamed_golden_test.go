package serve

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
)

// updateUnnamed writes the golden files of TestUnnamedSignalUnchanged
// afresh: run once, on the release before named signals were kept in
// lapilli serve, so that what one impact signal with no name gives can be
// held to it, byte for byte (docs/design/signals.md, "Step 2").
var updateUnnamed = flag.Bool("update-unnamed", false, "write testdata/unnamed-signal afresh")

// unnamedNow are the times a page or an answer says that come from the
// clock of the machine the test runs on (a decision's, a task's, a
// comment's), not from the ledger's runs: any time after the runs' now.
var unnamedNow = regexp.MustCompile(`20[0-9]{2}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]+)?)?(Z|[+-][0-9]{2}:[0-9]{2})?`)

func unnamedNormal(s string) string {
	s = unnamedNow.ReplaceAllStringFunc(s, func(t string) string {
		if t[:10] > "2026-11-10" {
			return "<clock>"
		}
		return t
	})
	// What serve says of itself, which a run's own time changes.
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "lapilli_serve_") || strings.Contains(line, "lapilli_last_run_timestamp_seconds") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// One impact signal with no name, as every lapilli serve before named
// signals read it: its windows, their keys, what was decided of them, the
// tasks and comments on them, the record it keeps, and what the summary,
// the report, /metrics, the pages and MCP say, the same byte for byte as
// the release before.
func TestUnnamedSignalUnchanged(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}, []string{"bo@example.com"}
		c.Impact = "impact > 1"
	})
	s.cfg.RecordByWeek = true
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.ready.Store(true)
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.l.ImpactWindows(ctx, now.Add(-30*24*3600e9), now)
	if err != nil || len(ws) != 3 {
		t.Fatalf("windows: %v %v", ws, err)
	}
	// Cart's dismissed; search's, undecided, with a task and a comment.
	var cart, key string
	for _, w := range ws {
		switch w.Labels["service"][0] {
		case "cart":
			cart = w.ID
		case "search":
			key = w.ID
		}
	}
	if _, err := s.l.Decide(ctx, cart, "dismissed", "", "ana@example.com", o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "Look at the search window", Kind: "decide", Incident: key, Status: "open"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutComment(ctx, ledger.Comment{On: "incident", Key: key, Body: "a deploy, likely"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, false)
	// The ledger as this release left it, for a later one to open.
	if keep := os.Getenv("LAPILLI_KEEP_UNNAMED_LEDGER"); keep != "" {
		ledgerExec(t, s, "VACUUM INTO ?", keep)
	}

	t.Run("a ledger made by this release", func(t *testing.T) { unnamedUnchanged(t, s, key, false) })
	if *updateUnnamed {
		return
	}
	// The same ledger taken back to 021, as 0.53 left it, and opened again:
	// migrated, it says the same.
	undo, version := slices.Concat(ledgertest.Undo036, ledgertest.Undo035, ledgertest.Undo034, ledgertest.Undo033, ledgertest.Undo032, ledgertest.Undo031, ledgertest.Undo030, ledgertest.Undo029, ledgertest.Undo028, ledgertest.Undo027, ledgertest.Undo026, ledgertest.Undo025, ledgertest.Undo024, ledgertest.Undo023, ledgertest.Undo022SQLite), "PRAGMA user_version = 21"
	if s.cfg.LedgerURLFile != "" {
		undo, version = slices.Concat(ledgertest.Undo036, ledgertest.Undo035, ledgertest.Undo034, ledgertest.Undo033, ledgertest.Undo032, ledgertest.Undo031, ledgertest.Undo030, ledgertest.Undo029, ledgertest.Undo028, ledgertest.Undo027, ledgertest.Undo026, ledgertest.Undo025, ledgertest.Undo024, ledgertest.Undo023, ledgertest.Undo022Postgres), "UPDATE schema_version SET version = 21"
	}
	for _, q := range append(slices.Clone(undo), version) {
		ledgerExec(t, s, q)
	}
	db := ledgerDB(t, s)
	var since int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM meta WHERE key = 'impact_since'").Scan(&since); err != nil || since != 1 {
		t.Fatalf("taken back to 021, impact_since in meta: %d %v", since, err)
	}
	db.Close()
	s.Close()
	again, _ := server(t, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}, []string{"bo@example.com"}
		c.Impact = "impact > 1"
		c.Ledger, c.LedgerURLFile = s.cfg.Ledger, s.cfg.LedgerURLFile
	})
	again.cfg.RecordByWeek = true
	again.jobs(ctx, false)
	again.ready.Store(true)
	if k, err := again.l.ImpactKept(ctx); err != nil || k.From().IsZero() {
		t.Fatalf("upgraded, what keeps the record: %+v %v", k, err)
	}
	t.Run("a ledger upgraded from 021", func(t *testing.T) { unnamedUnchanged(t, again, key, true) })
}

// unnamedUnchanged holds what s gives to the golden files of
// TestUnnamedSignalUnchanged, or writes them with -update-unnamed; the
// summary's unjudged, which the release before didn't give, held to the
// report's of each week. Of a server started again on the ledger
// (restarted), the status line of what this process did, which a page
// shows, isn't the ledger's, and is left out.
func unnamedUnchanged(t *testing.T, s *Server, key string, restarted bool) {
	got := map[string]string{}
	h := s.Handler()
	for name, path := range map[string]string{
		"summary.json":      "/api/v1/summary",
		"summary-team.json": "/api/v1/summary?team=*",
		"report.json":       "/api/v1/report?from=2026-10-27T00:00:00Z&to=2026-11-03T12:00:00Z",
		"report.csv":        "/api/v1/report?from=2026-10-27T00:00:00Z&to=2026-11-03T12:00:00Z&format=csv",
		"incidents.json":    "/api/v1/incidents?from=2026-10-01T00:00:00Z&to=2026-11-04T00:00:00Z",
		"metrics.prom":      "/metrics",
		"incidents.html":    "/incidents",
		"impacts.html":      "/incidents?source=impact",
		"window.html":       "/incidents/" + url.PathEscape(key),
		"rule.html":         "/rules/Errors",
		"rules.html":        "/rules",
		"overview.html":     "/",
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Forwarded-Email", "viewer@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		got[name] = w.Body.String()
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	cs := connect(t, ts.URL, "viewer@example.com")
	for name, c := range map[string]struct {
		tool string
		args map[string]any
	}{
		"mcp-summary.json":   {"summary", nil},
		"mcp-incidents.json": {"incidents", nil},
		"mcp-rule.json":      {"rule", map[string]any{"name": "Errors"}},
		"mcp-overview.json":  {"overview", nil},
	} {
		var out json.RawMessage
		if failed, text := call(t, cs, c.tool, c.args, &out); failed {
			t.Fatalf("%s: %s", c.tool, text)
		}
		got[name] = string(out)
	}
	// Each week's unjudged, as the report of that week counts it.
	var sum struct {
		Rows []struct {
			From, To time.Time
			Team     string
			Unjudged int
		}
	}
	if err := json.Unmarshal([]byte(got["summary.json"]), &sum); err != nil || len(sum.Rows) == 0 {
		t.Fatalf("summary: %v", err)
	}
	for _, row := range sum.Rows {
		code, body, _ := get(t, h, "/api/v1/report?from="+row.From.UTC().Format(time.RFC3339)+"&to="+row.To.UTC().Format(time.RFC3339))
		var rep struct{ Unjudged int }
		if code != http.StatusOK || json.Unmarshal([]byte(body), &rep) != nil || rep.Unjudged != row.Unjudged {
			t.Errorf("the week of %s: unjudged %d, the report's %d (%d)", row.From.Format("2006-01-02"), row.Unjudged, rep.Unjudged, code)
		}
	}

	dir := filepath.Join("testdata", "unnamed-signal")
	for name, body := range got {
		body = unnamedNormal(body)
		if !*updateUnnamed && strings.Contains(name, "summary") {
			// What docs/design/signals.md adds to the summary, beside what
			// it said before, for every ledger: each row's unjudged, held
			// above, and impacts.by_signal, {} with no named signal.
			body = addedToSummary(body)
		}
		path := filepath.Join(dir, name)
		if *updateUnnamed {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		wantS := string(want)
		if restarted && strings.HasSuffix(name, ".html") {
			body, wantS = withoutStatus(body), withoutStatus(wantS)
		}
		if body != wantS {
			t.Errorf("%s differs from the release before named signals:\n%s", name, firstDiff(wantS, body))
		}
	}
}

// withoutStatus is a page without its status lines, which say what this
// process did, not what the ledger holds.
func withoutStatus(x string) string {
	if i := strings.Index(x, `<section class="statuses"`); i >= 0 {
		if j := strings.Index(x[i:], "</section>\n"); j >= 0 {
			x = x[:i] + x[i+j+len("</section>\n"):]
		}
	}
	return strings.ReplaceAll(x, "\n\n", "\n")
}

// addedToSummary takes out of a summary the fields named signals add to it
// (docs/design/signals.md, "How they are counted"), so that the rest can
// be held to what the release before said: a row's "unjudged", always
// there, and impacts' "by_signal", {} with no named signal.
var (
	addedUnjudged = regexp.MustCompile(`(?m)^\s*"unjudged": [0-9]+,\n|"unjudged":[0-9]+,|,"unjudged":[0-9]+`)
	addedBySignal = regexp.MustCompile(`,\n\s*"by_signal": \{\}|,"by_signal":\{\}|"by_signal":\{\},`)
)

func addedToSummary(s string) string {
	return addedBySignal.ReplaceAllString(addedUnjudged.ReplaceAllString(s, ""), "")
}

// firstDiff is where two texts first differ, with some of each around it.
func firstDiff(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	lo := max(0, i-200)
	return "want: …" + a[lo:min(len(a), i+200)] + "\n got: …" + b[lo:min(len(b), i+200)]
}
