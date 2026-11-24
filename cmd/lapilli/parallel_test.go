package main

import (
	"net/http"
	"testing"

	"lapilli.dev/internal/prom"
)

// parallel runs the test beside the package's other parallel tests, once
// the ones that run alone have: one that keeps to its own files, its own
// stand-in servers, and the environment it gives what it starts (a
// recipe's shell, git, helm, and this test binary run as lapilli).
//
// A test that runs alone doesn't call it: one that sets a hook the code
// reads (newHistory, now, ...: alone); the environment or the working
// directory (t.Setenv, t.Chdir); one that runs lapilli preflight in this
// process, which sets the process's egress guard, its allowlist, over
// every request any test's client sends meanwhile; one that reads the
// heap, which every test adds to; and one that writes the docs or its
// golden files when asked to (-write-docs, -update-...). Whatever such a
// test sets, it sets back before the next one starts, so the parallel
// tests, which never run beside it, see the package as it is.
//
// On Postgres as on SQLite, unlike serve's: a test's ledger there is a
// schema of its own (ledgertest), and no test here counts the statements
// the process sends.
func parallel(t *testing.T) {
	t.Helper()
	t.Parallel()
}

// alone is called first by a test that sets the package's hooks, which
// the code under test reads: newHistory, now and the others below. Such a
// test runs alone (parallel says why), and alone sets each hook back as
// the test ends, to what it was as the test began: so that the tests
// after it, the parallel ones among them, see the package's own, and not
// what the last test to set one left.
func alone(t *testing.T) {
	t.Helper()
	h, n, l, gl, a, r, w := newHistory, now, newLoki, newGrafanaLoki, newAlarms, newRuler, watched
	after, every, samples, transport := progressAfter, progressEvery, prom.DefaultMaxSamples, http.DefaultTransport
	t.Cleanup(func() {
		newHistory, now, newLoki, newGrafanaLoki, newAlarms, newRuler, watched = h, n, l, gl, a, r, w
		progressAfter, progressEvery, prom.DefaultMaxSamples, http.DefaultTransport = after, every, samples, transport
	})
}
