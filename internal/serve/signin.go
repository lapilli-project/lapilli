package serve

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/signin"
)

// A sign-in of lapilli serve's own (docs/design/signin.md), beside a
// sign-in proxy's plain header (--user-header): OpenID Connect, with
// sessions kept in signed cookies and ID tokens taken as bearer tokens; or
// a sign-in in front whose header is a JWT it signed (--proxy-jwt). With
// either, every page and API call but the health checks, the static files,
// the choice of language and the sign-in's own pages needs someone signed
// in, and no one is an admin for listening on loopback: roles come from
// --editors, --admins and the rest alone.
type signIn struct {
	oidc    *signin.OIDC
	proxy   *signin.Proxy
	cookies *signin.Cookies
	// ended are the sessions signed out of, by id, until they would have
	// expired; kept in the ledger, so a restart doesn't bring one back.
	ended expiring
	// used are the sign-ins come back from, by state, until they would
	// have expired: a callback can't be replayed.
	used expiring
	// warned is set once a refusal was logged.
	warned atomic.Bool
}

// expiring is a set of keys, each until a time, of at most max.
type expiring struct {
	sync.Mutex
	m     map[string]int64
	order []string
	max   int
}

// add puts a key in until a time, dropping those expired and, when full,
// the oldest; it reports whether the key wasn't there already.
func (e *expiring) add(key string, until, now int64) bool {
	e.Lock()
	defer e.Unlock()
	if e.m == nil {
		e.m = map[string]int64{}
	}
	if u, ok := e.m[key]; ok && u > now {
		return false
	}
	kept := e.order[:0]
	for _, k := range e.order {
		if e.m[k] > now {
			kept = append(kept, k)
		} else {
			delete(e.m, k)
		}
	}
	e.order = kept
	for len(e.order) >= e.max {
		delete(e.m, e.order[0])
		e.order = e.order[1:]
	}
	e.m[key] = until
	e.order = append(e.order, key)
	return true
}

func (e *expiring) has(key string, now int64) bool {
	e.Lock()
	defer e.Unlock()
	u, ok := e.m[key]
	return ok && u > now
}

// snapshot is the set as JSON, for the ledger.
func (e *expiring) snapshot() string {
	e.Lock()
	defer e.Unlock()
	b, _ := json.Marshal(e.m)
	return string(b)
}

// who is who a request comes from, as the sign-in said, and how: by a
// session (a browser), a bearer token (an API client, an agent), or a
// proxy's signed header.
type who struct {
	signin.Identity
	by string
}

const (
	bySession = "session"
	byBearer  = "bearer"
	byProxy   = "proxy"
)

type whoKey struct{}

// whoOf is who the sign-in said a request comes from.
func whoOf(r *http.Request) (who, bool) {
	w, ok := r.Context().Value(whoKey{}).(who)
	return w, ok
}

// Meta keys of the sign-in's, in the ledger.
const (
	metaSessionKey = "session-key"
	metaEnded      = "sessions-ended"
)

// newSignIn readies the sign-in: the key cookies are signed with, from
// --session-key-file or else made once and kept in the ledger; the
// sessions signed out of; and the provider's or the proxy's verifier.
func (s *Server) newSignIn(ctx context.Context) error {
	in := &signIn{ended: expiring{max: 10000}, used: expiring{max: 10000}}
	now := func() time.Time { return s.now() }
	if p := s.cfg.Proxy; p != nil {
		p.Client, p.Now = signin.Client(nil), now
		px, err := signin.NewProxy(*p)
		if err != nil {
			return err
		}
		in.proxy = px
	}
	if o := s.cfg.OIDC; o != nil {
		key, err := s.sessionKey(ctx)
		if err != nil {
			return err
		}
		maxAge := s.cfg.SessionMaxAge
		if maxAge == 0 {
			maxAge = 12 * time.Hour
		}
		if in.cookies, err = signin.NewCookies(key, strings.HasPrefix(strings.ToLower(s.cfg.ExternalURL), "https://"), maxAge, now); err != nil {
			return err
		}
		o.Redirect = strings.TrimRight(s.cfg.ExternalURL, "/") + "/auth/callback"
		if o.Client == nil {
			o.Client = signin.Client(nil)
		}
		o.Now, o.Scopes = now, signin.WithOpenID(o.Scopes)
		in.oidc = o
		if raw, err := s.l.Meta(ctx, metaEnded); err == nil && raw != "" {
			var m map[string]int64
			if json.Unmarshal([]byte(raw), &m) == nil {
				for id, until := range m {
					in.ended.add(id, until, s.now().Unix())
				}
			}
		}
	}
	s.in = in
	return nil
}

// sessionKey is the key session cookies are signed with: --session-key-file's,
// read at start, or one made at random the first time and kept in the
// ledger.
func (s *Server) sessionKey(ctx context.Context) ([]byte, error) {
	if f := s.cfg.SessionKey; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("--session-key-file: %v", err)
		}
		if k := strings.TrimSpace(string(b)); len(k) >= 32 {
			return []byte(k), nil
		}
		return nil, errors.New("--session-key-file: want a key of 32 characters at least")
	}
	k, err := s.l.Meta(ctx, metaSessionKey)
	if err != nil {
		return nil, fmt.Errorf("--ledger: %w", err)
	}
	if k == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		k = hex.EncodeToString(b)
		if err := s.l.SetMeta(ctx, metaSessionKey, k); err != nil {
			return nil, fmt.Errorf("--ledger: %w", err)
		}
	}
	return []byte(k), nil
}

// namesPeople reports whether requests say who they come from: a sign-in
// proxy's header, or a sign-in of lapilli's own.
func (s *Server) namesPeople() bool { return s.cfg.UserHeader != "" || s.in != nil }

// openPath reports whether a path is answered to anyone, signed in or not:
// the health checks, the static files, the choice of language, and the
// sign-in's own.
func openPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/lang":
		return true
	}
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/auth/")
}

// signedIn lets a request on only when the sign-in says who it comes from,
// and keeps who in its context.
func (s *Server) signedIn(h http.Handler) http.Handler {
	if s.in == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openPath(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		id, err := s.identify(r)
		if err != nil {
			s.notSignedIn(w, r, err)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), whoKey{}, id)))
	})
}

// errNoOne is a sign-in that names no one lapilli takes: a name that would
// be lapilli's own, a token's, or another's once characters that don't
// show are taken out.
var errNoOne = errors.New("the sign-in names no one lapilli can take")

// identify is who a request comes from: the proxy's signed header; or a
// bearer ID token, else the session cookie.
func (s *Server) identify(r *http.Request) (who, error) {
	var w who
	var err error
	switch {
	case s.in.proxy != nil:
		w.by = byProxy
		w.Identity, err = s.in.proxy.Identify(r)
	case r.Header.Get("Authorization") != "":
		w.by = byBearer
		tok, ok := signin.CutBearer(r.Header.Get("Authorization"))
		if !ok {
			return who{}, errors.New("the Authorization header holds no bearer token")
		}
		w.Identity, err = s.in.oidc.Bearer(r.Context(), tok)
	default:
		w.by = bySession
		sess, ok := s.in.cookies.Read(r)
		if !ok || s.in.ended.has(sess.ID, s.now().Unix()) {
			return who{}, errNoSession
		}
		w.User, w.Groups = sess.User, sess.Groups
	}
	if err != nil {
		return who{}, err
	}
	if s.signedName(w.User) == "anonymous" {
		return who{}, errNoOne
	}
	return w, nil
}

// errNoSession is a browser not signed in.
var errNoSession = errors.New("not signed in")

// notSignedIn answers a request the sign-in doesn't vouch for: a browser
// asking for a page is sent to sign in, and comes back to it; anything
// else gets 401.
func (s *Server) notSignedIn(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, errNoSession) && !errors.Is(err, signin.ErrNoHeader) && s.in.warned.CompareAndSwap(false, true) {
		// Once: anyone could otherwise fill the log.
		s.log.Warn("refused a request whose sign-in didn't verify; others are refused without a word", "path", r.URL.Path, "err", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	page := (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.Header.Get("Authorization") == "" &&
		!strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/mcp"
	if s.in.oidc != nil && page {
		http.Redirect(w, r, s.loginURL(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	msg := "sign in: send a session cookie, or an ID token from the identity provider as a bearer token"
	if s.in.oidc != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="lapilli"`)
	} else {
		msg = "no identity the sign-in in front signed (" + s.in.proxy.Header + ")"
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}
	http.Error(w, msg, http.StatusUnauthorized)
}

// loginURL is where a browser signs in, at --external-url, to come back to
// a page: a browser that reached lapilli by another name (a load
// balancer's address) is taken to the one the identity provider sends it
// back to, where its cookies are.
func (s *Server) loginURL(next string) string {
	return strings.TrimRight(s.cfg.ExternalURL, "/") + "/auth/login?next=" + url.QueryEscape(safeBack(next, "/"))
}

// signInRoutes are the sign-in's pages, with OpenID Connect.
func (s *Server) signInRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	if s.in == nil || s.in.oidc == nil {
		return
	}
	mux.HandleFunc("GET /auth/login", s.login)
	mux.HandleFunc("GET /auth/callback", s.callback)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("GET /auth/signed-out", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		s.render(w, r, "signedout", nil)
	})
}

// login sends the browser to the identity provider, keeping the sign-in's
// state, nonce and PKCE verifier in a signed cookie until it comes back.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	st := signin.NewStart()
	to, err := s.in.oidc.AuthURL(r.Context(), st)
	if err != nil {
		s.log.Warn("signing in", "err", err)
		s.oops(w, r, errSignInDown)
		return
	}
	if err := s.in.cookies.Begin(w, st, safeBack(r.URL.Query().Get("next"), "/")); err != nil {
		s.oops(w, r, err)
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// The sign-in's refusals, as people read them.
var (
	errSignInDown  = errors.New("the identity provider can't be reached: try again in a minute")
	errBadState    = errors.New("this sign-in didn't start here, was used already, or took too long: sign in again")
	errSignInFails = errors.New("the identity provider's answer didn't verify: sign in again, or ask whoever runs lapilli to look at its log")
	errTooManyGrp  = errors.New("the identity provider lists more groups than a session can hold")
)

// oauthError is the form of an OAuth error code: only that, never the
// description, which whoever made the link chose, is shown.
var oauthError = regexp.MustCompile(`^[a-z_]{1,40}$`)

// callback takes the browser back from the identity provider: the state
// must be the one its cookie holds, used once, and the code is exchanged
// for an ID token that must verify, with the nonce. Then the browser gets
// a session, and goes back to the page it came from.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	l, ok := s.in.cookies.Take(w, r)
	if !ok || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(l.State)) != 1 {
		s.oops(w, r, errBadState)
		return
	}
	if !s.in.used.add(l.State, l.Until, s.now().Unix()) {
		s.oops(w, r, errBadState)
		return
	}
	if e := q.Get("error"); e != "" {
		if !oauthError.MatchString(e) {
			e = "error"
		}
		s.oops(w, r, forbiddenSignIn{bad("the identity provider didn't sign you in (%s)", e)})
		return
	}
	id, err := s.in.oidc.Finish(r.Context(), l.Start, q.Get("code"))
	switch {
	case errors.Is(err, signin.ErrUnreachable):
		s.log.Warn("signing in", "err", err)
		s.oops(w, r, errSignInDown)
		return
	case errors.Is(err, signin.ErrNoName):
		s.log.Warn("refused a sign-in", "err", err)
		s.oops(w, r, forbiddenSignIn{err})
		return
	case err != nil:
		s.log.Warn("refused a sign-in", "err", err)
		s.oops(w, r, forbiddenSignIn{errSignInFails})
		return
	}
	if id.User = s.signedName(id.User); id.User == "anonymous" {
		s.oops(w, r, forbiddenSignIn{errNoOne})
		return
	}
	if _, err := s.in.cookies.Issue(w, id); errors.Is(err, signin.ErrTooBig) {
		// Only the groups roles are given by, then.
		id.Groups = slices.DeleteFunc(id.Groups, func(g string) bool { return !s.roleGroup(r.Context(), g) })
		if _, err := s.in.cookies.Issue(w, id); err != nil {
			s.oops(w, r, forbiddenSignIn{errTooManyGrp})
			return
		}
	} else if err != nil {
		s.oops(w, r, err)
		return
	}
	s.log.Info("signed in", "user", id.User)
	http.Redirect(w, r, safeBack(l.Next, "/"), http.StatusSeeOther)
}

// forbiddenSignIn is a sign-in refused.
type forbiddenSignIn struct{ error }

func (f forbiddenSignIn) Unwrap() error { return f.error }

// roleGroup reports whether a group is one roles are given by: in
// --editors, --admins or --approvers, or given a role in the UI.
func (s *Server) roleGroup(ctx context.Context, g string) bool {
	for _, list := range [][]string{s.cfg.Editors, s.cfg.Admins, s.cfg.Approvers} {
		if slices.Contains(list, g) || slices.Contains(list, "group:"+g) {
			return true
		}
	}
	for _, gr := range s.grants(ctx) {
		if gr.Who == "group:"+g {
			return true
		}
	}
	return false
}

// logout ends the session: its cookie is taken away, and its id kept as
// ended until it would have expired, so a copy of the cookie is refused
// too. It is a POST from this site alone (the cross-origin check): a page
// elsewhere can't sign people out.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if sess, ok := s.in.cookies.Read(r); ok {
		if s.in.ended.add(sess.ID, sess.Until, s.now().Unix()) {
			if err := s.l.SetMeta(r.Context(), metaEnded, s.in.ended.snapshot()); err != nil {
				s.log.Warn("the sessions signed out of can't be kept in the ledger: a copy of this one's cookie is refused until lapilli restarts", "err", err)
			}
		}
		s.log.Info("signed out", "user", sess.User)
	}
	s.in.cookies.End(w)
	http.Redirect(w, r, "/auth/signed-out", http.StatusSeeOther)
}
