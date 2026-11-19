package serve

import (
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"

	"lapilli.dev/internal/ledger"
)

// ledgerWork is what the ledger did while something ran, as counts, not
// times, so that a slow runner doesn't fail a test of how much a page or a
// write costs: the statements it sent, and on SQLite the rows it wrote.
//
// On SQLite, statements are the SELECT, INSERT, UPDATE and DELETE clauses
// compiled on the ledger's connections (SQLite's authorizer is told of each,
// a subquery's among them; a statement prepared once and run many times is
// one), and rows are the rows inserted, updated or deleted (its pre-update
// hook). On Postgres, statements are those its connections sent
// (ledger.StatementsSent), and rows aren't counted (-1).
type ledgerWork struct{ statements, rows int64 }

var watched struct {
	once sync.Once
	mu   sync.Mutex
	// by the ledger file's URI, as the connection's DSN begins
	of map[string]*ledgerCounts
}

type ledgerCounts struct{ statements, rows atomic.Int64 }

// watchLedgers makes every SQLite connection opened from now on count what
// it compiles and writes, by the file it is of.
func watchLedgers() {
	watched.once.Do(func() {
		watched.of = map[string]*ledgerCounts{}
		sqlite.RegisterConnectionHook(func(c sqlite.ExecQuerierContext, dsn string) error {
			file, _, _ := strings.Cut(dsn, "?")
			watched.mu.Lock()
			n := watched.of[file]
			if n == nil {
				n = &ledgerCounts{}
				watched.of[file] = n
			}
			watched.mu.Unlock()
			if a, ok := c.(sqlite.AuthorizerRegisterer); ok {
				if err := a.RegisterAuthorizer(func(action sqlite.AuthorizerActionCode, _, _, _, _ string) sqlite.AuthorizerReturnCode {
					switch action {
					case sqlite.AuthSelect, sqlite.AuthInsert, sqlite.AuthUpdate, sqlite.AuthDelete:
						n.statements.Add(1)
					}
					return sqlite.AuthorizerOK
				}); err != nil {
					return err
				}
			}
			if h, ok := c.(interface {
				RegisterPreUpdateHook(sqlite.PreUpdateHookFn)
			}); ok {
				h.RegisterPreUpdateHook(func(sqlite.SQLitePreUpdateData) { n.rows.Add(1) })
			}
			return nil
		})
	})
}

// counting is a server's ledger counted as watchLedgers counts it: call
// it before the server is made (server's change), so that every one of
// its connections counts.
func counting(*Config) { watchLedgers() }

// ledgerDid runs f and gives what the server's ledger did while it ran.
func ledgerDid(t *testing.T, s *Server, f func()) ledgerWork {
	t.Helper()
	now := func() ledgerWork {
		if s.cfg.LedgerURLFile != "" {
			return ledgerWork{statements: ledger.StatementsSent(), rows: -1}
		}
		file := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(s.cfg.Ledger, "ledger.db"))}).String()
		watched.mu.Lock()
		n := watched.of[file]
		watched.mu.Unlock()
		if n == nil {
			t.Fatalf("the ledger %s isn't counted: make the server with counting", file)
		}
		return ledgerWork{statements: n.statements.Load(), rows: n.rows.Load()}
	}
	was := now()
	f()
	is := now()
	if is.rows < 0 {
		return ledgerWork{statements: is.statements - was.statements, rows: -1}
	}
	return ledgerWork{statements: is.statements - was.statements, rows: is.rows - was.rows}
}
