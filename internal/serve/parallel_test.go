package serve

import (
	"testing"

	"lapilli.dev/internal/ledger/ledgertest"
)

// parallel runs the test beside the package's other parallel tests, once
// the ones that run alone have: on SQLite, where each test's ledger, server
// and state are its own.
//
// A test that runs alone doesn't call it: one that sets a package-level
// variable (a limit, a wait, a hook such as requestAuth), the environment
// or the working directory; one that reads what every test adds to (the
// heap, a counter such as namedReads, the translations missing); and one
// whose outcome hangs on how quickly a real clock's time passes, such as
// a --timeout of a few hundred milliseconds shared out among signals.
// Whatever such a test sets, it sets back before the next one starts, so
// the parallel tests, which never run beside it, see the package as it is.
//
// On Postgres (ledgertest.On) every test runs alone, as before: the
// statements counted (ledger.StatementsSent) are the process's, not a
// ledger's, and the tests share one database.
func parallel(t *testing.T) {
	t.Helper()
	if !ledgertest.On() {
		t.Parallel()
	}
}
