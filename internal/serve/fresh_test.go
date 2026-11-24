package serve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lapilli.dev/internal/ledger"
)

// freshLedgers makes one SQLite ledger as ledger.Open makes a new one,
// every schema file run, and has each test's new ledger written as a copy
// of it (freshLedger): the schema files took a tenth of a second for each
// of the thousand servers the tests make. A ledger that is there already
// is opened as it is, and the ledger's own tests still make theirs from
// nothing. The copy is the file as Open left it when closed: SQLite's
// write-ahead log checkpointed into it and removed, the zone UTC (a test
// opening it in another zone has its days counted again by Open, as a new
// ledger has), and no ledger-id yet, which New makes for each.
func freshLedgers() (done func(), err error) {
	dir, err := os.MkdirTemp("", "lapilli-fresh-")
	if err != nil {
		return nil, err
	}
	done = func() { os.RemoveAll(dir) }
	path := filepath.Join(dir, "ledger.db")
	l, err := ledger.Open(context.Background(), path, time.UTC)
	if err != nil {
		done()
		return nil, err
	}
	if err := l.Close(); err != nil {
		done()
		return nil, err
	}
	for _, left := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + left); err == nil {
			done()
			return nil, fmt.Errorf("the fresh ledger left %s behind when closed", filepath.Base(path+left))
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		done()
		return nil, err
	}
	freshLedger = func(to string) error { return os.WriteFile(to, b, 0o644) }
	return done, nil
}
