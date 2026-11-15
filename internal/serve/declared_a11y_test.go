package serve

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// declaredPages gives s what --config declares (a team, its quiet window,
// its report and a role), a file that can't be used since, a rule's name
// gone that the ledger keeps a note and a task of, and proposals that
// expire: the pages that show them, for the accessibility and language
// checks (a11yServer).
func declaredPages(t *testing.T, s *Server) []string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.yaml")
	good := `teams:
  - name: platform
    rules: ^Platform
windows:
  - title: Platform freeze
    team: platform
    start: 2026-11-02T22:00
    end: 2026-11-03T02:00
    every: week
reports:
  - name: platform weekly
    team: platform
    schedule: weekly mon@09:00
    dests: [ops]
grants:
  - group: platform-oncall
    team: platform
`
	os.WriteFile(path, []byte(good), 0o644)
	s.cfg.Config, s.cfg.ProposalsExpire = path, 30*24*time.Hour
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if e := s.configShown().ErrText("en"); e != "" {
		t.Fatal(e)
	}
	// Changed since to what can't be kept: the ledger keeps the last.
	os.WriteFile(path, []byte(strings.Replace(good, "every: week", "every: fortnight", 1)), 0o644)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	// A rule's name gone, that the ledger keeps a note and a task of.
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "ErrorsOld", Status: "retire", Note: "renamed"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "move it", Kind: "tune", Rule: "ErrorsOld"}, "ana"); err != nil {
		t.Fatal(err)
	}
	ts, _ := s.l.Teams(ctx)
	ws, _ := s.l.Windows(ctx, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
	rs, _ := s.l.Scheduleds(ctx)
	paths := []string{"/orphans", "/settings", "/runs", "/manage", "/teams", "/roles", "/reports", "/windows"}
	for _, x := range ts {
		if x.Declared {
			paths = append(paths, "/teams?edit="+strconv.FormatInt(x.ID, 10))
		}
	}
	for _, w := range ws {
		if w.Declared {
			paths = append(paths, "/windows/"+strconv.FormatInt(w.ID, 10))
		}
	}
	for _, r := range rs {
		if r.Declared {
			paths = append(paths, "/reports/"+strconv.FormatInt(r.ID, 10), "/reports?edit="+strconv.FormatInt(r.ID, 10))
		}
	}
	return paths
}

// The pages that show what --config declares, orphans, and when a
// proposal expires find a translation for every text they show, in each
// language.
func TestDeclaredPagesTranslated(t *testing.T) {
	s, paths := a11yServer(t)
	h := s.Handler()
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	paths = append(paths, "/proposals/1")
	for _, n := range names {
		for _, p := range paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Accept-Language", n.Code)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code >= 500 {
				t.Errorf("%s %s: %d", n.Code, p, w.Code)
			}
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
	// What --config can be told is wrong with, in each language: every
	// way ParseConfig and configText say it.
	formats := slices.Concat(configFormats, slices.Collect(maps.Values(briefs)), []string{"the file can't be read as --config", "and %d more", "a team the file no longer declares, kept declared"})
	for _, f := range formats {
		for lang, c := range catalogs {
			if _, ok := c[f]; !ok {
				t.Errorf("%s has no translation for %q", lang, f)
			}
		}
	}
}
