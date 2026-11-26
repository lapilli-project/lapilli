package serve

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A route renamed is still taken under its old path until the next major
// release (ROADMAP.md, "Toward 1.0"). The old path is not a second
// registration: a request to it is rewritten to the new path before any
// check runs, and then goes the way a request to the new path goes, through
// the same sign-in, role, host, token and size checks, to the same handler.
// Only the response says more: Deprecation (RFC 9745) and a Link to the
// new path (RFC 8288, rel="successor-version").

// routeRename is one route renamed: Old and New are routes as the mux has
// them, "METHOD /path/{name}", the same method in both, every {name} of New
// in Old too. Since is when the old name was deprecated: the release's day.
type routeRename struct {
	Old, New string
	Since    time.Time
}

// renamedRoutes is every route renamed since 1.0's promise began, the old
// name and the new: the one place a route's old name is kept. Each is taken
// by every port that has the new route, and docs/upgrading.md lists them.
var renamedRoutes = []routeRename{
	{"GET /api/v1/expectation-drafts", "GET /api/v1/expectations/drafts", time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)},
	{"GET /api/v1/expectation-drafts/{id}", "GET /api/v1/expectations/drafts/{id}", time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)},
}

// checkRenames says what is wrong with a table of renames, if anything.
func checkRenames(table []routeRename) error {
	for _, rn := range table {
		om, op, ok1 := strings.Cut(rn.Old, " ")
		nm, np, ok2 := strings.Cut(rn.New, " ")
		if !ok1 || !ok2 || om != nm || !strings.HasPrefix(op, "/") || !strings.HasPrefix(np, "/") {
			return fmt.Errorf("renamed route %q to %q: want METHOD /path in both, the same method", rn.Old, rn.New)
		}
		if rn.Since.IsZero() {
			return fmt.Errorf("renamed route %q: no date it was deprecated", rn.Old)
		}
		have := map[string]bool{}
		for _, seg := range strings.Split(op, "/") {
			if strings.HasPrefix(seg, "{") {
				have[seg] = true
			}
		}
		for _, seg := range strings.Split(np, "/") {
			if strings.HasPrefix(seg, "{") && !have[seg] {
				return fmt.Errorf("renamed route %q to %q: %s is not in the old path", rn.Old, rn.New, seg)
			}
		}
	}
	return nil
}

// rename gives the new escaped path of a request to the old one, if it is one.
func (rn routeRename) rename(method, escaped string) (string, bool) {
	om, op, _ := strings.Cut(rn.Old, " ")
	if method != om && !(method == http.MethodHead && om == http.MethodGet) {
		return "", false
	}
	_, np, _ := strings.Cut(rn.New, " ")
	have, want := strings.Split(escaped, "/"), strings.Split(op, "/")
	if len(have) != len(want) {
		return "", false
	}
	got := map[string]string{}
	for i, w := range want {
		switch {
		case strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}"):
			if have[i] == "" {
				return "", false
			}
			got[w] = have[i]
		case w != have[i]:
			return "", false
		}
	}
	segs := strings.Split(np, "/")
	for i, seg := range segs {
		if v, ok := got[seg]; ok {
			segs[i] = v
		}
	}
	return strings.Join(segs, "/"), true
}

// renames serves the routes of table under their old names: a request to
// one is rewritten to the new path, and handled by outer, which must
// reach mux, where the new route is registered. A request whose new path
// mux has no route for (another port's) is left as it came.
func (s *Server) renames(table []routeRename, mux *http.ServeMux, outer http.Handler) http.Handler {
	if len(table) == 0 {
		return outer
	}
	var said sync.Map
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, rn := range table {
			path, ok := rn.rename(r.Method, r.URL.EscapedPath())
			if !ok {
				continue
			}
			plain, err := url.PathUnescape(path)
			if err != nil {
				break
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path, r2.URL.RawPath = plain, path
			if _, pattern := mux.Handler(r2); pattern == "" {
				continue
			}
			if _, dup := said.LoadOrStore(rn.Old, true); !dup {
				s.log.Warn("a request to a route by its old name", "old", rn.Old, "new", rn.New, "note", "the old name goes in a major release")
			}
			w.Header().Set("Deprecation", "@"+strconv.FormatInt(rn.Since.Unix(), 10))
			next := (&url.URL{Path: plain, RawPath: path, RawQuery: r.URL.RawQuery}).String()
			w.Header().Add("Link", "<"+next+`>; rel="successor-version"`)
			r = r2
			break
		}
		outer.ServeHTTP(w, r)
	})
}
