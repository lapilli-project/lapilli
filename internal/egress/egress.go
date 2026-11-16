// Package egress is the way out of the process for HTTP: every client
// lapilli makes sends through Wrap, so that a guard set for the process
// sees each request before it is sent. lapilli preflight sets one, its
// allowlist (docs/design/preflight.md); lapilli serve and verdict set
// none, and Wrap then only sends.
package egress

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

type guard struct{ check func(*http.Request) error }

var current atomic.Pointer[guard]

// Guard has check see every request a client of Wrap sends, before it is
// sent: one it returns an error for isn't sent, and the client gets the
// error. release puts back the guard there was before, if any.
func Guard(check func(*http.Request) error) (release func()) {
	g := &guard{check}
	prev := current.Swap(g)
	return func() { current.CompareAndSwap(g, prev) }
}

// Guarded reports whether a guard is set: what an SDK's client is given
// then is Client, so that its requests go through the guard too.
func Guarded() bool { return current.Load() != nil }

type wrapped struct{ base http.RoundTripper }

// Wrap sends through base, http.DefaultTransport for nil, once the guard
// set, if any, lets a request go.
func Wrap(base http.RoundTripper) http.RoundTripper { return wrapped{base} }

func (w wrapped) RoundTrip(req *http.Request) (*http.Response, error) {
	if g := current.Load(); g != nil {
		if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
			// A body the guard can read again (GetBody), as the request
			// sent: an SDK's request may come without one.
			b, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
			req.Body.Close()
			if err != nil {
				return nil, err
			}
			if len(b) > maxBody {
				return nil, errors.New("a request's body past what a guard reads")
			}
			again := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
			req = req.Clone(req.Context())
			req.Body, _ = again()
			req.GetBody = again
		}
		if err := g.check(req); err != nil {
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, err
		}
	}
	return w.transport().RoundTrip(req)
}

// maxBody is how much of a request's body is held for a guard to read.
const maxBody = 1 << 20

func (w wrapped) transport() http.RoundTripper {
	if w.base == nil {
		return http.DefaultTransport
	}
	return w.base
}

// CloseIdleConnections closes those of the transport wrapped.
func (w wrapped) CloseIdleConnections() {
	if c, ok := w.transport().(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// Unwrap is the transport rt sends through, rt itself when it isn't one
// Wrap made.
func Unwrap(rt http.RoundTripper) http.RoundTripper {
	if w, ok := rt.(wrapped); ok {
		return w.transport()
	}
	return rt
}

// Client is a client sending through Wrap(nil), following no redirect: for
// an SDK that takes one (a cloud's credential chain).
func Client() *http.Client {
	return &http.Client{Transport: Wrap(nil), Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
