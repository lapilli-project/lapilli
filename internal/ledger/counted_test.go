package ledger

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	msqlite "modernc.org/sqlite"
)

// work is what the ledger did while something ran, as counts, not times,
// so that a slow runner fails no test of what a write or a read costs: the
// statements it sent; on SQLite, the rows it wrote and the tables it read.
//
// On SQLite, statements are the SELECT, INSERT, UPDATE and DELETE clauses
// compiled on the ledger's connections (SQLite's authorizer is told of
// each, a subquery's among them; a statement prepared once and run many
// times is one), rows the rows inserted, updated or deleted (its pre-update
// hook), and read the tables a statement compiled read a column of. On
// Postgres, statements are those its connections sent (sent), rows -1,
// and read nil.
type work struct {
	statements, rows int64
	read             map[string]bool
}

var watched struct {
	once sync.Once
	mu   sync.Mutex
	// by the ledger file's URI, as the connection's DSN begins
	of map[string]*workCounts
}

type workCounts struct {
	statements, rows atomic.Int64
	mu               sync.Mutex
	read             map[string]int64
}

// watch makes every SQLite connection opened from now on count what it
// compiles, writes and reads, by the file it is of: call it before the
// ledger is opened.
func watch() {
	watched.once.Do(func() {
		watched.of = map[string]*workCounts{}
		msqlite.RegisterConnectionHook(func(c msqlite.ExecQuerierContext, dsn string) error {
			file, _, _ := strings.Cut(dsn, "?")
			watched.mu.Lock()
			n := watched.of[file]
			if n == nil {
				n = &workCounts{read: map[string]int64{}}
				watched.of[file] = n
			}
			watched.mu.Unlock()
			if a, ok := c.(msqlite.AuthorizerRegisterer); ok {
				if err := a.RegisterAuthorizer(func(action msqlite.AuthorizerActionCode, table, _, _, _ string) msqlite.AuthorizerReturnCode {
					switch action {
					case msqlite.AuthSelect, msqlite.AuthInsert, msqlite.AuthUpdate, msqlite.AuthDelete:
						n.statements.Add(1)
					case msqlite.AuthRead:
						n.mu.Lock()
						n.read[table]++
						n.mu.Unlock()
					}
					return msqlite.AuthorizerOK
				}); err != nil {
					return err
				}
			}
			if h, ok := c.(interface {
				RegisterPreUpdateHook(msqlite.PreUpdateHookFn)
			}); ok {
				h.RegisterPreUpdateHook(func(msqlite.SQLitePreUpdateData) { n.rows.Add(1) })
			}
			return nil
		})
	})
}

// openWatched is open's ledger, counted (watch), and where it is.
func openWatched(t *testing.T) (*Ledger, string) {
	t.Helper()
	watch()
	where := place(t)
	l, err := openAt(context.Background(), where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, where
}

// tables are the names of the tables read, sorted.
func (w work) tables() []string { return slices.Sorted(maps.Keys(w.read)) }

// doing runs f and gives what l did while it ran; l was opened at path
// (place's), after watch.
func doing(t *testing.T, l *Ledger, path string, f func()) work {
	t.Helper()
	if l.d == postgres {
		was := sent.Load()
		f()
		return work{statements: sent.Load() - was, rows: -1}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	u := fileURI(abs)
	file := u.String()
	watched.mu.Lock()
	n := watched.of[file]
	watched.mu.Unlock()
	if n == nil {
		t.Fatalf("the ledger %s isn't counted: open it after watch", file)
	}
	read := func() map[string]int64 {
		n.mu.Lock()
		defer n.mu.Unlock()
		return maps.Clone(n.read)
	}
	s0, r0, read0 := n.statements.Load(), n.rows.Load(), read()
	f()
	w := work{statements: n.statements.Load() - s0, rows: n.rows.Load() - r0, read: map[string]bool{}}
	for table, k := range read() {
		if k > read0[table] {
			w.read[table] = true
		}
	}
	return w
}
