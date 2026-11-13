package serve

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

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
	// out are the people who signed out, and when: their sessions made
	// before are ended. Kept in the ledger, so a restart doesn't bring one
	// back.
	out signedOut
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

// signedOut are the people who signed out, each with when (Unix
// milliseconds): every session of theirs made then or before is ended. It
// is bounded by the people who signed out within a session's life, of
// those a role lets in, which only the identity provider and the roles make
// more of, not by how often they did. The ledger holds it, for every
// process that shares the ledger.
type signedOut struct {
	sync.Mutex
	at map[string]int64
	// count is the ledger's count of changes to the record as last read
	// (MetaCount); -1 before the first read.
	count int64
	max   int
}

// everyone is the record's entry for every person: a session or a token of
// anyone's made then or before is ended. It is raised when the record is
// full, to when the person let go of signed out, rather than forgetting
// them: a copy of their cookie is refused still, with every other made
// before it. No one is named "" (signedName).
const everyone = ""

// add notes a person signed out at a time; entries older than a session
// lasts are dropped, and, when full, the oldest, raising everyone's.
func (o *signedOut) add(user string, at, keep int64) {
	o.Lock()
	defer o.Unlock()
	o.at = mergeOut(o.at, map[string]int64{user: at}, at, keep, o.max)
}

// stale reports whether the ledger's record changed since last read, by its
// count of changes: reading the count is a small read, the record, of up to
// max people, isn't.
func (o *signedOut) stale(count int64) bool {
	o.Lock()
	defer o.Unlock()
	return count != o.count
}

// load takes in what the ledger holds, as of its count of changes.
func (o *signedOut) load(raw string, count, now, keep int64) {
	o.Lock()
	defer o.Unlock()
	var m map[string]int64
	if raw != "" && json.Unmarshal([]byte(raw), &m) != nil {
		return
	}
	o.count = count
	o.at = mergeOut(o.at, m, now, keep, o.max)
}

// ended reports whether a session of a person's made at a time is one they
// signed out of, or one made before everyone's.
func (o *signedOut) ended(user string, made int64) bool {
	o.Lock()
	defer o.Unlock()
	if t, ok := o.at[everyone]; ok && made <= t {
		return true
	}
	t, ok := o.at[user]
	return ok && made <= t
}

// mergeOut is who signed out in a and b, each at the later time, less those
// older than a session lasts; past max people, the oldest are let go of,
// and everyone's raised to the last of them: failing closed, as a session
// or a token of theirs from before is still refused.
func mergeOut(a, b map[string]int64, now, keep int64, most int) map[string]int64 {
	out := make(map[string]int64, len(a)+len(b))
	for _, m := range []map[string]int64{a, b} {
		for u, t := range m {
			if t+keep >= now && t > out[u] {
				out[u] = t
			}
		}
	}
	floor, raised := out[everyone]
	delete(out, everyone)
	if len(out) > most {
		people := slices.Collect(maps.Keys(out))
		slices.SortFunc(people, func(x, y string) int { return cmp.Or(cmp.Compare(out[x], out[y]), strings.Compare(x, y)) })
		for _, u := range people[:len(out)-most] {
			floor, raised = max(floor, out[u]), true
			delete(out, u)
		}
	}
	if raised {
		// Anyone signed out at everyone's time or before is in it.
		maps.DeleteFunc(out, func(_ string, t int64) bool { return t <= floor })
		out[everyone] = floor
	}
	return out
}

// signedOut reports whether a session or a token of a person's, made at a
// time, is one they signed out of, here or at another process sharing the
// ledger. The record is read again only when the ledger counts a change to
// it.
func (s *Server) signedOut(ctx context.Context, user string, made int64) bool {
	keep := s.in.cookies.MaxAge.Milliseconds()
	if n, err := s.l.MetaCount(ctx, metaSignedOut); err == nil && s.in.out.stale(n) {
		if raw, err := s.l.Meta(ctx, metaSignedOut); err == nil {
			s.in.out.load(raw, n, s.now().UnixMilli(), keep)
		}
	}
	return s.in.out.ended(user, made)
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

// maxSignedOut is how many people signing out are kept at once.
const maxSignedOut = 10000

// metaSignedOut is who signed out, and when, in the ledger.
const metaSignedOut = "signed-out"

// newSignIn readies the sign-in: the key cookies are signed with, from
// --session-key-file or else made once and kept in the ledger; the
// sessions signed out of; and the provider's or the proxy's verifier.
func (s *Server) newSignIn(ctx context.Context) error {
	in := &signIn{out: signedOut{max: maxSignedOut, count: -1}, used: expiring{max: 10000}}
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
		key, err := s.sessionKey()
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
	}
	s.in = in
	return nil
}

// sessionKey is the key session cookies are signed with: --session-key-file's,
// read at start, or else one made at random for this process alone. It is
// never kept in the ledger: whoever could read the ledger (a backup, a
// snapshot of its volume) could then sign in as anyone, an approver among
// them.
func (s *Server) sessionKey() ([]byte, error) {
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
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	s.log.Info("sessions are signed with a key made for this process: they end when lapilli restarts. --session-key-file keeps them")
	return []byte(hex.EncodeToString(b)), nil
}

// namesPeople reports whether requests say who they come from: a sign-in
// proxy's header, or a sign-in of lapilli's own.
func (s *Server) namesPeople() bool { return s.cfg.UserHeader != "" || s.in != nil }

// openPath reports whether a path is answered to anyone, signed in or not:
// the health checks, the static files, the choice of language, and the
// sign-in's own.
func openPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/lang", "/.well-known/oauth-protected-resource":
		return true
	}
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/auth/")
}

// signedIn lets a request on only when the sign-in says who it comes from,
// and keeps who in its context; then, with a sign-in of lapilli's own or
// --viewers, only when a role lets them in.
func (s *Server) signedIn(h http.Handler) http.Handler {
	if s.in == nil && len(s.cfg.Viewers) == 0 {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openPath(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		if s.in != nil {
			id, err := s.identify(r)
			if err != nil {
				s.notSignedIn(w, r, err)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), whoKey{}, id))
		}
		if !s.letIn(r) {
			w.Header().Set("Cache-Control", "no-store")
			s.oops(w, r, notLetIn{})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// errNoOne is a sign-in that names no one lapilli takes: a name that would
// be lapilli's own, a token's, or another's once characters that don't
// show are taken out.
var errNoOne = errors.New("the sign-in names no one lapilli can take")

// identify is who a request comes from: the proxy's signed header; or a
// bearer ID token, else the session cookie. A JWT the proxy puts in
// Authorization (oauth2-proxy's) is a bearer token too: any program that
// holds one sends it as easily as the proxy, and never approves.
func (s *Server) identify(r *http.Request) (who, error) {
	var w who
	var err error
	switch {
	case s.in.proxy != nil:
		w.by = byProxy
		if s.in.proxy.Header == "Authorization" {
			w.by = byBearer
		}
		w.Identity, err = s.in.proxy.Identify(r)
		if err != nil {
			return who{}, reason(err)
		}
	case r.Header.Get("Authorization") != "":
		w.by = byBearer
		tok, ok := signin.CutBearer(r.Header.Get("Authorization"))
		switch {
		case !ok:
			return who{}, tokenError{"the Authorization header holds no bearer token"}
		case s.in.oidc.NoBearer:
			return who{}, tokenError{"bearer tokens aren't taken here: sign in in a browser"}
		}
		if w.Identity, err = s.in.oidc.Bearer(r.Context(), tok); err != nil {
			return who{}, reason(err)
		}
		// Taken as long as a session made then would be, at most, whatever
		// its own exp says: who signed out is kept that long, and a token
		// from before must not outlast the record of it.
		if s.now().Sub(w.IssuedAt) >= s.in.cookies.MaxAge {
			return who{}, tokenError{"the token was issued longer ago than a session lasts here (--session-max-age)"}
		}
		if s.signedOut(r.Context(), w.User, w.IssuedAt.UnixMilli()) {
			return who{}, tokenError{"the token was issued before its person signed out"}
		}
	default:
		w.by = bySession
		sess, ok := s.in.cookies.Read(r)
		if !ok || s.signedOut(r.Context(), sess.User, sess.Made) {
			return who{}, errNoSession
		}
		w.User, w.Groups = sess.User, sess.Groups
	}
	if s.signedName(w.User) == "anonymous" {
		return who{}, tokenError{"the token names no one lapilli takes"}
	}
	return w, nil
}

// tokenError is a token refused, said in words that hold nothing the token
// held: they go in WWW-Authenticate's error_description, and in the log.
type tokenError struct{ desc string }

func (t tokenError) Error() string { return t.desc }

// reason is why a token was refused, in a fixed set of words.
func reason(err error) error {
	var expired *oidc.TokenExpiredError
	switch {
	case errors.As(err, &expired):
		return tokenError{"the token has expired"}
	case errors.Is(err, signin.ErrNoHeader):
		return err
	case errors.Is(err, signin.ErrNoName):
		return tokenError{"the token names no one lapilli takes"}
	case errors.Is(err, signin.ErrUnreachable), errors.Is(err, signin.ErrMisconfigured):
		return tokenError{"the identity provider can't be asked now"}
	}
	return tokenError{"the token didn't verify"}
}

// errNoSession is a browser not signed in.
var errNoSession = errors.New("not signed in")

// notSignedIn answers a request the sign-in doesn't vouch for: a browser
// asking for a page is sent to sign in, and comes back to it; anything
// else gets 401, and a token refused says why (RFC 6750), so a client
// knows to get another.
func (s *Server) notSignedIn(w http.ResponseWriter, r *http.Request, err error) {
	var bad tokenError
	refused := errors.As(err, &bad)
	if refused && s.in.warned.CompareAndSwap(false, true) {
		// Once, and only the reason: anyone could otherwise fill the log,
		// and a token is a secret.
		s.log.Warn("refused a token; others are refused without a word", "reason", bad.desc)
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
		challenge := `Bearer realm="lapilli", resource_metadata="` + strings.TrimRight(s.cfg.ExternalURL, "/") + `/.well-known/oauth-protected-resource"`
		if refused {
			challenge += `, error="invalid_token", error_description="` + bad.desc + `"`
			msg = bad.desc
		}
		w.Header().Set("WWW-Authenticate", challenge)
	} else {
		msg = "no identity the sign-in in front signed (" + s.in.proxy.Header + ")"
		if refused {
			msg = bad.desc
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}
	http.Error(w, msg, http.StatusUnauthorized)
}

// protectedResource describes this server as an OAuth 2.0 protected
// resource (RFC 9728): the provider to get a token from. The token is an ID
// token this client was given (docs/design/signin.md says how).
func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=3600")
	json.NewEncoder(w).Encode(map[string]any{
		"resource":                 strings.TrimRight(s.cfg.ExternalURL, "/"),
		"authorization_servers":    []string{s.in.oidc.Issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         s.in.oidc.Scopes,
	})
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
	if !s.in.oidc.NoBearer {
		mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResource)
	}
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
		s.oops(w, r, s.providerFailed(err))
		return
	}
	// The sign-ins under way the browser holds already, but the newest few,
	// are taken away: a page elsewhere can send a browser here again and
	// again, and each would leave a cookie for ten minutes, until the
	// browser, full, drops the session's.
	s.in.cookies.Prune(w, r, keepLogins)
	if err := s.in.cookies.Begin(w, st, safeBack(r.URL.Query().Get("next"), "/")); err != nil {
		s.oops(w, r, err)
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// discover asks the identity provider for its configuration, and says in
// the log what is wrong with it, if anything.
func (s *Server) discover(ctx context.Context) {
	if err := s.in.oidc.Discover(ctx); err != nil {
		s.providerFailed(err)
		return
	}
	s.log.Info("signing in with the identity provider", "issuer", s.in.oidc.Issuer)
}

// providerFailed logs why the identity provider couldn't be used, and says
// it to people in their words: a setting that is wrong as an ERROR, for
// whoever runs lapilli, not as an outage.
func (s *Server) providerFailed(err error) error {
	if errors.Is(err, signin.ErrMisconfigured) {
		s.log.Error("signing in: the identity provider's configuration", "issuer", s.in.oidc.Issuer, "err", err)
		return errSignInBroken
	}
	s.log.Warn("signing in", "issuer", s.in.oidc.Issuer, "err", err)
	return errSignInDown
}

// The sign-in's refusals, as people read them: what went wrong is in the
// log, for whoever runs lapilli.
var (
	errSignInBroken = errors.New("signing in isn't set up right here: ask whoever runs lapilli to look at its log")
	errNoName       = errors.New("the identity provider's answer doesn't name you as lapilli needs: ask whoever runs lapilli to look at its log")
	errSignInDown   = errors.New("the identity provider can't be reached: try again in a minute")
	errBadState     = errors.New("this sign-in didn't start here, was used already, or took too long: sign in again")
	errSignInFails  = errors.New("the identity provider's answer didn't verify: sign in again, or ask whoever runs lapilli to look at its log")
	errTooManyGrp   = errors.New("the identity provider lists more groups than a session can hold")
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
	// Each sign-in's cookie is named by its state: two tabs signing in at
	// once each come back to their own.
	l, ok := s.in.cookies.Take(w, r, q.Get("state"))
	if !ok {
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
	case errors.Is(err, signin.ErrUnreachable), errors.Is(err, signin.ErrMisconfigured):
		s.oops(w, r, s.providerFailed(err))
		return
	case errors.Is(err, signin.ErrNoName):
		s.log.Warn("refused a sign-in: the ID token names no one lapilli takes", "err", err)
		s.oops(w, r, forbiddenSignIn{errNoName})
		return
	case err != nil:
		s.log.Warn("refused a sign-in", "err", err)
		s.oops(w, r, forbiddenSignIn{errSignInFails})
		return
	}
	if id.User = s.signedName(id.User); id.User == "anonymous" {
		s.log.Warn("refused a sign-in: the name is lapilli's own, or too long, or holds a character that doesn't show")
		s.oops(w, r, forbiddenSignIn{errNoOne})
		return
	}
	if id.Overage {
		s.log.Warn("the identity provider sent a link to the groups (_claim_names) in place of the groups: Microsoft Entra ID's group overage, past 200. No group is taken. Have it send only the groups the application is given (groupMembershipClaims: ApplicationGroup), or name people", "user", id.User)
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
	for _, list := range [][]string{s.cfg.Viewers, s.cfg.Editors, s.cfg.Admins, s.cfg.Approvers} {
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

// keepLogins is how many sign-ins under way a browser keeps, the one begun
// included: tabs signing in at once.
const keepLogins = 4

// logout signs the person out: the cookie is taken away, and every
// session of theirs made until now is ended, this one and any copy of its
// cookie, in another browser too, until it would have expired. It is a POST
// from this site alone (the cross-origin check): a page elsewhere can't
// sign people out.
//
// Who signed out is written in the ledger, with --read-only too: signing
// out must work. Only for a session not ended already, and of someone a
// role lets in: anyone the provider signs in has a cookie that reaches this
// page, and the record is bounded by people, so one cookie signing out
// again and again, or strangers signing out by the thousand, write nothing.
// A stranger's cookie, kept, lets no one in anywhere.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if sess, ok := s.in.cookies.Read(r); ok && !s.signedOut(r.Context(), sess.User, sess.Made) {
		as := r.WithContext(context.WithValue(r.Context(), whoKey{}, who{Identity: signin.Identity{User: sess.User, Groups: sess.Groups}, by: bySession}))
		if s.signedName(sess.User) != "anonymous" && s.letIn(as) {
			s.keepSignOut(r.Context(), sess.User)
		}
		s.log.Info("signed out", "user", sess.User)
	}
	s.in.cookies.End(w)
	http.Redirect(w, r, "/auth/signed-out", http.StatusSeeOther)
}

// keepSignOut notes that a person signed out now, here and in the ledger,
// merged with what it holds, written by another process sharing it, too.
func (s *Server) keepSignOut(ctx context.Context, user string) {
	at, keep := s.now().UnixMilli(), s.in.cookies.MaxAge.Milliseconds()
	s.in.out.add(user, at, keep)
	err := s.l.UpdateMetaCounted(ctx, metaSignedOut, func(old string) (string, bool) {
		var m map[string]int64
		json.Unmarshal([]byte(old), &m)
		b, _ := json.Marshal(mergeOut(m, map[string]int64{user: at}, at, keep, s.in.out.max))
		return string(b), string(b) != old
	})
	if err != nil {
		s.log.Warn("who signed out can't be kept in the ledger: a copy of their cookie is refused until lapilli restarts", "err", err)
	}
}
