package main

import (
	"net/http"
	"testing"

	"lapilli.dev/internal/prom"
)

// alone is called first by a test that sets the package's hooks, which
// the code under test reads: newHistory, now and the others below. It
// sets each back as the test ends, to what it was as the test began: so
// that the tests after it see the package's own, and not what the last
// test to set one left.
func alone(t *testing.T) {
	t.Helper()
	h, n, l, gl, a, r, w := newHistory, now, newLoki, newGrafanaLoki, newAlarms, newRuler, watched
	after, every, samples, transport := progressAfter, progressEvery, prom.DefaultMaxSamples, http.DefaultTransport
	t.Cleanup(func() {
		newHistory, now, newLoki, newGrafanaLoki, newAlarms, newRuler, watched = h, n, l, gl, a, r, w
		progressAfter, progressEvery, prom.DefaultMaxSamples, http.DefaultTransport = after, every, samples, transport
	})
}
