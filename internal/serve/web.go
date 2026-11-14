package serve

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"io/fs"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/signin"
	"lapilli.dev/internal/verdict"
)

//go:embed templates/*.html static/*
var files embed.FS

// The font and its licence: Go knows neither type, and an image without
// /etc/mime.types would send them as anything.
func init() {
	mime.AddExtensionType(".woff2", "font/woff2")
	mime.AddExtensionType(".txt", "text/plain; charset=utf-8")
}

var pages = template.Must(template.New("").Funcs(funcs).Funcs(offFuncs).ParseFS(files, "templates/*.html"))

// Handler is the web UI, the API and the metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(files, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", immutable(http.FileServerFS(assets))))
	// The UI's port answers anyone, whatever the host: its readiness says
	// no more than ok.
	s.checks(mux, false)

	page := func(name string, view func(*http.Request) (any, error)) http.HandlerFunc {
		h := s.cached(func(w http.ResponseWriter, r *http.Request) {
			// A page worked out, not kept already: a few at a time. The
			// turn is given back once it is rendered (render), before it
			// goes to the network.
			tn, err := s.renderTurn(r.Context())
			if err != nil {
				s.oops(w, r, err)
				return
			}
			defer tn.give()
			r = r.WithContext(context.WithValue(r.Context(), turnKey{}, tn))
			// The page context, read once; the view reads the reader's own
			// team as team= when it was taken for them.
			r, vr := s.withContext(r, name)
			v, err := view(vr)
			var to redirect
			if errors.As(err, &to) {
				http.Redirect(w, r, to.to, http.StatusSeeOther)
				return
			}
			if err != nil {
				s.oops(w, r, err)
				return
			}
			s.render(w, r, name, v)
		})
		return func(w http.ResponseWriter, r *http.Request) {
			// A query past maxQuery is refused before anything is read
			// for it.
			if len(r.URL.RawQuery) > maxQuery {
				s.oops(w, r, errLongQuery)
				return
			}
			// A page that reads only some of its query is asked, and
			// cached, by them alone, in one order.
			if canon := readsOf[name]; canon != nil {
				r = r.Clone(r.Context())
				q := r.URL.Query()
				// A team no one has is all teams, as the page reads it:
				// another name mustn't make it worked out again.
				if vs, ok := q["team"]; ok && vs[0] != "" {
					if o, err := s.owners(r.Context()); err == nil {
						if _, ok := o.team(vs[0]); !ok {
							q.Set("team", "")
						}
					}
				}
				// A "?" with nothing after it is no query either.
				r.URL.RawQuery, r.URL.ForceQuery = canon(q), false
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /{$}", page("overview", s.overview))
	mux.HandleFunc("GET /rules", page("rules", s.rulesList))
	mux.HandleFunc("GET /rules/{name...}", page("rule", s.rulePage))
	mux.HandleFunc("GET /incidents", page("incidents", s.incidentsPage))
	mux.HandleFunc("GET /incidents/{id...}", page("incident", s.incidentPage))
	mux.HandleFunc("GET /firings", page("firings", s.firingsPage))
	mux.HandleFunc("GET /settings", page("settings", s.settingsPage))
	mux.HandleFunc("GET /settings.yaml", s.settingsYAML)
	mux.HandleFunc("GET /runs", page("runs", s.runsPage))
	mux.HandleFunc("GET /manage", page("manage", s.managePage))
	mux.HandleFunc("GET /outcomes", page("outcomes", s.outcomesPage))
	mux.HandleFunc("GET /search", page("search", s.searchPage))
	mux.HandleFunc("GET /lang", chooseLang)
	s.workRoutes(mux, page)
	s.reportRoutes(mux, page)
	s.rolesRoutes(mux, page)
	s.changesRoutes(mux, page)
	s.signInRoutes(mux, page)

	mux.HandleFunc("POST /incidents", s.write(s.postIncident))
	mux.HandleFunc("POST /incidents/{id}/delete", s.write(s.deleteIncident))
	mux.HandleFunc("POST /incidents/{id}/decide", s.write(s.decide))
	mux.HandleFunc("POST /firings/{id}/real", s.write(s.real))
	s.bulkRoutes(mux)
	mux.HandleFunc("POST /settings", s.admin(s.postSettings))
	mux.HandleFunc("GET /api/v1/me", s.me)

	mux.HandleFunc("GET /api/v1/report", s.report)
	mux.HandleFunc("GET /api/v1/rules", s.cached(s.apiRules))
	mux.HandleFunc("GET /api/v1/incidents", s.cached(s.apiIncidents))
	mux.HandleFunc("POST /api/v1/incidents", s.write(s.apiPutIncident))
	mux.HandleFunc("PUT /api/v1/incidents/{id}", s.write(s.apiPutIncident))
	mux.HandleFunc("DELETE /api/v1/incidents/{id}", s.write(s.apiDeleteIncident))
	mux.HandleFunc("GET /api/v1/runs", s.cached(s.apiRuns))
	mux.HandleFunc("GET /api/v1/sources", s.cached(s.apiSources))
	mux.HandleFunc("GET /api/v1/settings", s.cached(s.apiSettings))
	mux.HandleFunc("GET /api/v1/outcomes", s.cached(s.apiOutcomes))
	mux.HandleFunc("GET /api/v1/summary", s.apiSummary)
	mux.Handle("/mcp", s.mcpHandler())

	cop := http.NewCrossOriginProtection()
	return s.hosts(s.fresh(headers(s.signedIn(gzipped(cop.Handler(mux))))))
}

// fresh reads, before a request is answered, what other replicas wrote
// since this one last did (ledger.Refresh), so that what a request shows
// holds every write made before it was asked, on whichever replica: a
// round trip to the database, with replicas. The health checks and the
// static files need none.
func (s *Server) fresh(h http.Handler) http.Handler {
	if !s.l.Shared() {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p != "/healthz" && p != "/readyz" && !strings.HasPrefix(p, "/static/") {
			// One that fails says nothing more than the request's own reads
			// will: it is answered from what this replica has.
			s.l.Refresh(r.Context())
		}
		h.ServeHTTP(w, r)
	})
}

// hosts answers only requests to a host name this server answers to. A
// page elsewhere can point a name of its own at this server (DNS
// rebinding): the browser then takes the server for the page's own site,
// and lets the page read it and write to it, through the cross-origin
// check. The health checks are answered whatever the name.
func (s *Server) hosts(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && !s.hostOK(r.Host, loopback(r.RemoteAddr)) {
			if s.refused.CompareAndSwap(false, true) {
				s.log.Warn("refused a request to a host name lapilli serve doesn't answer to; if it is yours, give it in --allowed-hosts or --external-url", "host", r.Host)
			}
			http.Error(w, "lapilli serve doesn't answer to this host name: give it in --allowed-hosts or --external-url", http.StatusMisdirectedRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// hostOK reports whether the server answers to a host name: an address (as
// a pod's is reached), localhost or a name under .localhost; a name without
// dots or under .internal, .svc or .cluster.local, which a page elsewhere
// can't make a browser look up to here, unless the request came over
// loopback (from this machine, a port-forward, a sidecar), where someone
// with a hand in the company's or the cluster's names could; --external-url's
// name; and those given in --allowed-hosts.
func (s *Server) hostOK(hostport string, loopback bool) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	switch {
	case host == "":
		return false
	case net.ParseIP(host) != nil, host == "localhost", strings.HasSuffix(host, ".localhost"):
		return true
	case !loopback && !strings.Contains(host, "."):
		return true
	}
	for _, suffix := range []string{".internal", ".svc", ".cluster.local"} {
		if !loopback && strings.HasSuffix(host, suffix) {
			return true
		}
	}
	if u, err := url.Parse(s.cfg.ExternalURL); err == nil && strings.EqualFold(u.Hostname(), host) {
		return true
	}
	for _, a := range s.cfg.AllowedHosts {
		if a == "*" || strings.EqualFold(strings.TrimSuffix(a, "."), host) {
			return true
		}
	}
	return false
}

// loopback reports whether a request came from this machine.
func loopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// pageRenders is how many pages are worked out at once, those not kept
// already: a page of two thousand rules takes tens of megabytes while it
// is, and forty of them at once a gigabyte. Another waits its turn up to
// renderWait, then is answered 503, to try again. The API, MCP, the health
// checks, the metrics and the ports that take notifications and copies
// aren't pages, and don't wait.
const pageRenders = 8

var renderWait = 10 * time.Second

// errPagesBusy is a page that waited renderWait for its turn.
var errPagesBusy = errors.New("the server is busy with many pages at once: try again in a few seconds")

// turn is a page's turn to be worked out (pageRenders), held while it is:
// given back once it is rendered, before it goes to the network, which a
// reader who doesn't read can make take minutes; or when the request ends;
// and, while the page waits for the heavy turn (busy), given back and taken
// again.
type turn struct {
	mu   sync.Mutex
	of   chan struct{} // the turns it is one of, nil for none
	held bool
}

// turnKey is the context key a page's turn is kept under.
type turnKey struct{}

// turnOf is the turn the request whose context ctx is holds, nil for none.
func turnOf(ctx context.Context) *turn {
	t, _ := ctx.Value(turnKey{}).(*turn)
	return t
}

// give gives the turn back, once.
func (t *turn) give() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held {
		<-t.of
		t.held = false
	}
}

// take takes the turn again, given back, waiting for one up to renderWait.
func (t *turn) take(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held || t.of == nil {
		return nil
	}
	w := time.NewTimer(renderWait)
	defer w.Stop()
	select {
	case t.of <- struct{}{}:
		t.held = true
		return nil
	case <-w.C:
		return errPagesBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

// renderTurn takes a turn to work out a page, waiting for one up to
// renderWait.
func (s *Server) renderTurn(ctx context.Context) (*turn, error) {
	if s.renders == nil {
		return &turn{}, nil
	}
	select {
	case s.renders <- struct{}{}:
		return &turn{of: s.renders, held: true}, nil
	default:
	}
	t := time.NewTimer(renderWait)
	defer t.Stop()
	select {
	case s.renders <- struct{}{}:
		return &turn{of: s.renders, held: true}, nil
	case <-t.C:
		return nil, errPagesBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// errBusy is a heavy request that found another taking its turn.
var errBusy = errors.New("the server is busy with another large request: try again in a minute")

// busy takes the turn for a heavy request, waiting for it up to thirty
// seconds; the func it returns gives the turn back. A page waiting for it
// holds no turn to be worked out (renderTurn) meanwhile: its turn is given
// back while it waits, and taken again once this one is, so that pages
// waiting for heavy work never keep others from being worked out.
func (s *Server) busy(ctx context.Context) (func(), error) {
	give := func() { <-s.heavy }
	select {
	case s.heavy <- struct{}{}:
		return give, nil
	default:
	}
	tn := turnOf(ctx)
	if tn != nil {
		tn.give()
	}
	heavyWaiting.Add(1)
	defer heavyWaiting.Add(-1)
	t := time.NewTimer(busyWait)
	defer t.Stop()
	select {
	case s.heavy <- struct{}{}:
	case <-t.C:
		return nil, errBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if tn != nil {
		if err := tn.take(ctx); err != nil {
			give()
			return nil, err
		}
	}
	return give, nil
}

// heavyWaiting counts the requests waiting for the heavy turn, for tests.
var heavyWaiting atomic.Int64

// checks are the health checks and the metrics, which a second listener
// (--metrics-listen) serves alone: a NetworkPolicy can then let Prometheus
// reach them, and no more.
//
// detail says, on the port Prometheus alone reaches, which replica leads:
// this one's id and its host, the leader's and its version, the term, how
// many live. The UI's port, which answers anyone, says ok.
func (s *Server) checks(mux *http.ServeMux, detail bool) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	// Ready once the ledger has a run recorded: at once after a restart,
	// so a Prometheus that is slow, or down, doesn't keep the UI (and the
	// sign-in's callback) away from a load balancer for a run's timeout; on
	// a first start, after the backfill's first day. What is being worked
	// out is on the pages.
	//
	// A replica that follows is ready as the leader is, once a run is
	// recorded; one that can't reach the ledger's database isn't, so that
	// the others answer for it. One whose writes wait for the writers'
	// turn is: the turn is every replica's, and reads are answered. Which
	// replica leads is said after, and whether writes wait.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			if c, err := s.l.Cursor(r.Context()); err == nil && !c.IsZero() {
				s.ready.Store(true)
			}
		}
		if !s.ready.Load() {
			http.Error(w, "no run is recorded yet", http.StatusServiceUnavailable)
			return
		}
		if !s.l.Shared() {
			io.WriteString(w, "ok\n")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.l.Refresh(ctx); err != nil {
			http.Error(w, "can't reach the ledger's database", http.StatusServiceUnavailable)
			return
		}
		if !detail {
			io.WriteString(w, "ok\n")
			return
		}
		io.WriteString(w, "ok\n"+s.leadership()+"\n")
	})
	mux.HandleFunc("GET /metrics", s.metrics)
}

// MetricsHandler is the health checks and the metrics alone.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	s.checks(mux, true)
	return s.hosts(s.fresh(headers(gzipped(mux))))
}

// IncidentsHandler is writing incidents, and firings recorded elsewhere,
// each with a token of its own, and nothing else. It is for jobs that copy
// them from where they are kept: on a port of its own, because a
// NetworkPolicy that let a job reach the UI's port would let it name itself
// anyone there, in the headers the sign-in proxy sets. Here, those headers
// mean nothing.
func (s *Server) IncidentsHandler() http.Handler {
	mux := http.NewServeMux()
	if s.cfg.IncidentsToken != "" {
		incidents := token{s.cfg.IncidentsToken, tokenUser, "--incidents-token-file", []string{s.cfg.FiringsToken, s.cfg.NotifyToken, s.cfg.TasksToken, s.cfg.ChangesToken}, &s.tokenWarned, 1 << 20, s.tokenRate, s.tokenBusy}
		mux.HandleFunc("POST /api/v1/incidents", s.withToken(incidents, s.apiPutIncident))
		mux.HandleFunc("PUT /api/v1/incidents/{id}", s.withToken(incidents, s.apiPutIncident))
		mux.HandleFunc("DELETE /api/v1/incidents/{id}", s.withToken(incidents, s.apiDeleteIncident))
	}
	if s.cfg.FiringsToken != "" {
		firings := token{s.cfg.FiringsToken, ledger.FiringsCopier, "--firings-token-file", []string{s.cfg.IncidentsToken, s.cfg.NotifyToken, s.cfg.TasksToken, s.cfg.ChangesToken}, &s.firingsWarned, maxFiringsBody, s.firingsRate, s.firingsBusy}
		mux.HandleFunc("PUT /api/v1/firings/{source}", s.withToken(firings, s.apiPutFirings))
	}
	s.trackerPaths(mux)
	s.changesTokenRoutes(mux)
	return s.hosts(s.fresh(headers(mux)))
}

// tokenUser is who changes made with the incidents token are noted as: not a
// person, so outcomes don't count them as use.
const tokenUser = ledger.Copier

// token is a token the incidents port takes: the file it is read from, who
// what it writes is noted as, and how much one request may send; others are
// the other tokens' files, none of which may hold the same. Each has its own
// rate and its own turn, so a job sending firings can't use up the incidents
// job's; both still wait their turn at the ledger, which writes one thing
// at a time.
type token struct {
	file, user, flag string
	others           []string
	// warned is set once a token that can't be used was logged.
	warned *atomic.Bool
	body   int64
	rate   *limiter
	busy   chan struct{}
}

// minToken is how long the incidents token is, at least.
const minToken = 16

// limiter lets so many things a second through, and a burst of more: in
// this process, or, shared, among the replicas of a ledger (shareLimits),
// from a bucket the ledger keeps.
type limiter struct {
	mu          sync.Mutex
	rate, burst float64
	left        float64
	last        time.Time
	// shared is the server whose ledger keeps the bucket, by name; nil
	// for this process's own.
	shared *Server
	name   string
}

// share has the limiter's bucket kept by the ledger the replicas share.
func (l *limiter) share(s *Server, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.shared, l.name = s, name
}

// take takes n from the ledger's bucket: refused when the ledger can't be
// asked, as the write it guards would be. A connection the server ended is
// tried again once, on another: taken twice, it costs a token, no more.
func (l *limiter) take(n float64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, err := l.shared.l.Take(ctx, l.name, l.rate, l.burst, n)
	if err != nil && ctx.Err() == nil {
		ok, err = l.shared.l.Take(ctx, l.name, l.rate, l.burst, n)
	}
	if err != nil {
		l.shared.log.Warn("a rate limit the replicas share couldn't be read: refused", "limit", l.name, "err", err)
		return false
	}
	return ok
}

func (l *limiter) allow(now time.Time) bool {
	if l.shared != nil {
		return l.take(1)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last.IsZero() {
		l.left = l.burst
	} else {
		l.left = min(l.burst, l.left+now.Sub(l.last).Seconds()*l.rate)
	}
	l.last = now
	if l.left < 1 {
		return false
	}
	l.left--
	return true
}

// allowN reports whether n may be taken now, as allow does for one: a
// budget of bytes, say. More than the burst is never taken.
func (l *limiter) allowN(now time.Time, n float64) bool {
	if l.shared != nil {
		return l.take(n)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last.IsZero() {
		l.left = l.burst
	} else {
		l.left = min(l.burst, l.left+now.Sub(l.last).Seconds()*l.rate)
	}
	l.last = now
	if l.left < n {
		return false
	}
	l.left -= n
	return true
}

// give gives back what allow took, for something that cost nothing.
func (l *limiter) give() {
	if l.shared != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l.shared.l.Give(ctx, l.name, l.burst)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.left = min(l.burst, l.left+1)
}

// wait is how long, in whole seconds, until allow may say yes again.
func (l *limiter) wait() int {
	return max(1, int(math.Ceil(1/l.rate)))
}

// actingAs is who a request acts as, in its context, when that isn't for the
// sign-in proxy to say.
type actingAs struct{}

// withToken is a handler for those who hold a token.
func (s *Server) withToken(tk token, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadOnly {
			http.Error(w, "lapilli serve is read-only (--read-only)", http.StatusForbidden)
			return
		}
		if !s.tokenOK(r, tk) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "this path takes the token in "+tk.flag+", as a bearer token", http.StatusUnauthorized)
			return
		}
		// A job copying incidents or firings needs a few writes a second,
		// and someone who has its token shouldn't get more: each write
		// holds the ledger's one writer, and is kept.
		if !tk.rate.allow(time.Now()) {
			w.Header().Set("Retry-After", strconv.Itoa(tk.rate.wait()))
			http.Error(w, "too many writes: try again in a few seconds", http.StatusTooManyRequests)
			return
		}
		// One at a time: a write that judges a long span again holds the
		// ledger's writer, and people's writes wait behind it.
		select {
		case tk.busy <- struct{}{}:
			defer func() { <-tk.busy }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "another write is being made with this token: one at a time", http.StatusTooManyRequests)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), actingAs{}, tk.user))
		r.Body = http.MaxBytesReader(w, r.Body, tk.body)
		h(w, r)
	}
}

// tokenOK reports whether a request holds a token. The file is read each
// time, so a rotated token is picked up; both are hashed first, so comparing
// them takes as long whatever their lengths.
func (s *Server) tokenOK(r *http.Request, tk token) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || got == "" {
		return false
	}
	b, err := os.ReadFile(tk.file)
	want := strings.TrimSpace(string(b))
	if err != nil || len(want) < minToken {
		// Once: anyone who can reach the port could otherwise fill the log.
		if tk.warned.CompareAndSwap(false, true) {
			s.log.Warn("the token in "+tk.flag+" can't be read, or is shorter than 16 characters: nothing is taken with it until it is put right", "err", err)
		}
		return false
	}
	// One token for two would let a job that may copy one write the other,
	// or whoever can read an alert manager's webhook URL write either:
	// none is taken until each differs.
	for _, f := range tk.others {
		if f == "" {
			continue
		}
		if o, err := os.ReadFile(f); err == nil && strings.TrimSpace(string(o)) == want {
			if tk.warned.CompareAndSwap(false, true) {
				s.log.Warn(tk.flag + " holds the same token as another of --incidents-token-file, --firings-token-file, --notify-token-file, --tasks-token-file and --changes-token-file: it isn't taken until each has its own")
			}
			return false
		}
	}
	tk.warned.Store(false)
	x, y := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

// headers are sent with everything: no scripts, no frames, no referrer.
func headers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' data:; font-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func immutable(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") == staticSum {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		h.ServeHTTP(w, r)
	})
}

// staticSum is a short hash of everything under /static/. Pages ask for
// /static/NAME?v=SUM, so the files a release brings are fetched at once
// after an upgrade, and kept for good until the next.
var staticSum = func() string {
	h := sha256.New()
	fs.WalkDir(files, "static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := files.ReadFile(p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

// static is the URL of a file under /static/, for this release.
func static(name string) string { return "/static/" + name + "?v=" + staticSum }

// gzipWriter compresses a response once it knows there is a body.
type gzipWriter struct {
	http.ResponseWriter
	z         *gzip.Writer
	wrote, on bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.wrote {
		return
	}
	g.wrote = true
	if code != http.StatusNoContent && code != http.StatusNotModified && g.Header().Get("Content-Encoding") == "" {
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Del("Content-Length")
		g.on = true
	}
	g.ResponseWriter.WriteHeader(code)
}

// Unwrap lets a ResponseController reach the connection beneath.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.wrote {
		g.WriteHeader(http.StatusOK)
	}
	if g.on {
		return g.z.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

var zippers = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

func gzipped(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.Method == http.MethodHead {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		z := zippers.Get().(*gzip.Writer)
		z.Reset(w)
		g := &gzipWriter{ResponseWriter: w, z: z}
		defer func() {
			if g.on {
				z.Close()
			}
			zippers.Put(z)
		}()
		h.ServeHTTP(g, r)
	})
}

// cache keeps pages and answers already worked out, until the ledger or the
// status changes: a page read twice between runs is worked out once.
type cache struct {
	sync.Mutex
	gen   string
	body  map[string]cached
	bytes int
}

// What the cache holds, at most: a page or answer bigger than one
// worth keeping is worked out each time.
const cacheBytes = 32 << 20

var cacheOne = 2 << 20

// spillWithin is how long a page or answer too big to keep has to go to
// the network, from when the first of it does: a reader who doesn't read
// holds the request no longer.
var spillWithin = 30 * time.Second

// spills counts the pages and answers too big to keep, for tests.
var spills atomic.Int64

type cached struct {
	ctype string
	body  []byte
}

// tag is the version of everything a page can show.
func (s *Server) tag() string {
	shown := s.shownStatus()
	st := fmt.Sprintf("%s|%v|%s|%.2f", shown.Doing, shown.Args, shown.Err, shown.Judged)
	// The file that pauses changes can come and go at any time.
	if s.cfg.Changes && s.changesPaused() {
		st += "|paused"
	}
	h := fnv.New64a()
	io.WriteString(h, st)
	return fmt.Sprintf("%s-%d-%d-%x", s.boot, s.l.Gen(), s.saidGen(), h.Sum64())
}

// recorder keeps a page or answer as it is written, to be cached: one
// bigger than the cache keeps (cacheOne) is written to the response, to,
// from then on, and not kept, so that it isn't held twice, by the
// recorder and by whatever wrote it.
type recorder struct {
	header  http.Header
	status  int
	body    bytes.Buffer
	to      http.ResponseWriter
	spilled bool
}

func (r *recorder) Header() http.Header    { return r.header }
func (r *recorder) WriteHeader(status int) { r.status = status }

func (r *recorder) Write(b []byte) (int, error) {
	if !r.spilled && r.to != nil && r.body.Len()+len(b) > cacheOne {
		h := r.to.Header()
		for k, v := range r.header {
			h[k] = v
		}
		if r.status != http.StatusOK {
			h.Del("ETag")
		}
		spills.Add(1)
		http.NewResponseController(r.to).SetWriteDeadline(time.Now().Add(spillWithin))
		r.to.WriteHeader(r.status)
		if _, err := r.to.Write(r.body.Bytes()); err != nil {
			return 0, err
		}
		r.body, r.spilled = bytes.Buffer{}, true
	}
	if r.spilled {
		return r.to.Write(b)
	}
	return r.body.Write(b)
}

func (s *Server) cached(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tag := s.tag()
		// A page is in the reader's language, and shows who reads it what
		// they can change: it is kept by both.
		key := lang(r) + " " + r.URL.RequestURI()
		if s.namesPeople() {
			key = s.user(r) + "\x00" + strings.Join(s.groups(r), ",") + "\x00" + key
		}
		// Signed in by a bearer token, as an API client is, a reader is
		// offered no approval, which a session's page offers: kept apart.
		if id, ok := whoOf(r); ok && id.by == byBearer {
			key = "bearer\x00" + key
		}
		etag := `"` + tag + "-" + strconv.FormatUint(hash(key), 16) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Add("Vary", "Accept-Language, Cookie")
		if s.cfg.UserHeader != "" {
			w.Header().Add("Vary", s.cfg.UserHeader)
		}
		if s.cfg.GroupsHeader != "" {
			w.Header().Add("Vary", s.cfg.GroupsHeader)
		}
		switch {
		case s.in == nil:
		case s.in.proxy != nil:
			w.Header().Add("Vary", s.in.proxy.Header)
		default:
			w.Header().Add("Vary", "Authorization")
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c, ok := s.cache.get(tag, key)
		if !ok {
			rec := &recorder{header: http.Header{}, status: http.StatusOK, to: w}
			h(rec, r)
			if rec.spilled {
				return // too big to keep, and written
			}
			if rec.status != http.StatusOK || r.Context().Err() != nil {
				for k, v := range rec.header {
					w.Header()[k] = v
				}
				w.Header().Del("ETag")
				w.WriteHeader(rec.status)
				w.Write(rec.body.Bytes())
				return
			}
			c = cached{rec.header.Get("Content-Type"), rec.body.Bytes()}
			s.cache.put(tag, key, c)
		}
		w.Header().Set("Content-Type", c.ctype)
		w.Write(c.body)
	}
}

func (c *cache) get(tag, key string) (cached, bool) {
	c.Lock()
	defer c.Unlock()
	if c.gen != tag {
		c.gen, c.body, c.bytes = tag, map[string]cached{}, 0
	}
	x, ok := c.body[key]
	return x, ok
}

func (c *cache) put(tag, key string, x cached) {
	if len(x.body) == 0 || len(x.body) > cacheOne {
		return
	}
	c.Lock()
	defer c.Unlock()
	if c.gen != tag {
		return
	}
	if c.bytes+len(x.body) > cacheBytes {
		c.body, c.bytes = map[string]cached{}, 0
	}
	c.body[key] = x
	c.bytes += len(x.body)
}

func hash(s string) uint64 {
	h := fnv.New64a()
	io.WriteString(h, s)
	return h.Sum64()
}

// user is who a request comes from: who the sign-in of lapilli's own said,
// or the header a sign-in proxy sets, when told to trust one.
func (s *Server) user(r *http.Request) string {
	if by, ok := r.Context().Value(actingAs{}).(string); ok {
		return by
	}
	if s.in != nil {
		if w, ok := whoOf(r); ok {
			return s.signedName(w.User)
		}
		return "anonymous"
	}
	// Named twice, the header may be the caller's as well as the proxy's.
	if s.cfg.UserHeader == "" || len(r.Header.Values(s.cfg.UserHeader)) != 1 {
		return "anonymous"
	}
	return s.signedName(r.Header.Get(s.cfg.UserHeader))
}

// signedName is a name a sign-in gave, as changes are recorded under it, or
// "anonymous" for one lapilli doesn't take.
func (s *Server) signedName(u string) string {
	// A name with a character that doesn't show (a zero-width space, a
	// soft hyphen, a control character: ledger.Hidden), spaces around it or
	// spacing that isn't the ASCII space (U+00A0, U+3000: signin.Plain), or
	// too long to keep whole, is no one's: taken out, or cut, or read, it
	// could become another's, an admin's.
	if len(u) > ledger.MaxName || ledger.Hidden(u) || !signin.Plain(u) {
		return "anonymous"
	}
	// Nor take the names changes made with a token are noted under, or
	// Lapilli's own, in any case: the ledger folds names.
	if f := ledger.FoldName(u); u == "" || slices.ContainsFunc([]string{tokenUser, ledger.FiringsCopier, ledger.TaskTracker,
		ledger.ChangesCopier, ledger.Proposer}, func(n string) bool { return f == ledger.FoldName(n) }) {
		return "anonymous"
	}
	return u
}

// write is a handler that changes the ledger, for editors; admin, for
// admins.
func (s *Server) write(h http.HandlerFunc) http.HandlerFunc { return s.need(editor, h) }

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc { return s.need(admin, h) }

// render writes a page.
// page is what every page is rendered with.
type page struct {
	Page   string
	View   any
	Status statusView
	Path   string
	Loc    *time.Location
	Lang   string
	Langs  []langLink
	// Title is the page's, as its h1 has it: a template sets it with
	// titled, for the browser's tab and for screen readers; Short, with
	// crumbed, is the breadcrumb's name for it, when shorter.
	Title, Short string
	// Ctx is the team and period the page is looked at for, carried on
	// its links; Nav the sidebar, Here the page of it the reader is on,
	// Crumb where the page is, and Bar the bar at its top
	// (docs/design/ui.md).
	Ctx            pageCtx
	Nav            []navGroup
	Here, HereName string
	Crumb          crumbOf
	Bar            barView
}

// Link is a page's URL with the page context; see pageCtx.Link.
func (p page) Link(target string, kv ...any) string { return p.Ctx.Link(target, kv...) }

// Hidden is the page context as a form's hidden fields, for a GET form of
// the page's own.
func (p page) Hidden() []field { return p.Ctx.hidden() }

// Rule is a rule's page, Incident an incident's, with the page context.
// From a page that shows a period of its own, a rule's page has its weeks:
// see pageCtx.ofPage.
func (p page) Rule(name string) string   { return p.RuleAt(name, "") }
func (p page) Incident(id string) string { return p.Ctx.Incident(id) }

// RuleAt is a part of a rule's page, frag ("#says", or ""), with the pairs
// kv as Link sets them, and the period of the page it is on.
func (p page) RuleAt(name, frag string, kv ...any) string {
	return p.Ctx.ofPage(p.Page).Link("/rules/"+url.PathEscape(name)+frag, kv...)
}

// errLongQuery is a page asked with a query past maxQuery bytes.
var errLongQuery = errors.New("this address asks too much: a page's query is 8192 bytes at most")

// redirect is a view's answer that the page is elsewhere: always a path on
// this server, built from fixed paths and escaped values.
type redirect struct{ to string }

func (r redirect) Error() string { return "see " + r.to }

// langLink is a language the page is offered in, named in that language.
type langLink struct {
	Code, Name, Href string
	Current          bool
}

func (s *Server) page(r *http.Request, name string, v any) page {
	l := lang(r)
	p := page{Page: name, View: v, Status: s.statusView(l), Path: r.URL.Path, Loc: s.cfg.Loc, Lang: l}
	// What the page offers to change is what the reader may.
	role := s.role(r)
	p.Status.ReadOnly = role < editor
	p.Status.Admin = role == admin
	p.Status.Teams = s.editsTeams(r)
	p.Status.RolesKept = role == admin && s.rolesKept()
	if s.namesPeople() {
		p.Status.User, p.Status.Role = s.user(r), role.String()
	}
	if w, ok := whoOf(r); ok && w.by == bySession {
		p.Status.SignOut = true
	}
	if role < editor {
		p.Status.Why = s.why(r)
	}
	// A page that refuses or finds nothing, or says one has signed out, is
	// shown to whoever asked, a stranger too: nothing of the ledger, nor
	// of the team and period the URL names (docs/design/ui.md).
	if !bare(name) {
		p.Ctx = s.pageContext(r)
	} else {
		// Nor how the runs are going: an error's text can name a host
		// inside, and the version is no stranger's business.
		p.Status.Doing, p.Status.Judged, p.Status.Err, p.Status.ChangesPaused, p.Status.Version = "", 0, "", false, ""
	}
	p.Here, p.HereName = navPage(name)
	p.Nav, p.Crumb, p.Bar = s.nav(r.Context(), p), crumbsOf[name], barOf(name, r, p.Ctx)
	back := unsaid(r.URL)
	if len(r.URL.RawQuery) > maxQuery {
		back = r.URL.EscapedPath()
	}
	for _, n := range names {
		p.Langs = append(p.Langs, langLink{n.Code, n.Name, "/lang?to=" + n.Code + "&back=" + url.QueryEscape(back), n.Code == l})
	}
	return p
}

// unsaid is a page's own address as the links it makes name it: without
// what a bulk write did (bulkParams), which the page says once.
func unsaid(u *url.URL) string {
	q := u.Query()
	if !slices.ContainsFunc(bulkParams, q.Has) {
		return u.RequestURI()
	}
	for _, k := range bulkParams {
		q.Del(k)
	}
	v := *u
	v.RawQuery = q.Encode()
	return v.RequestURI()
}

// render writes a page, in the reader's language.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, v any) {
	var b bytes.Buffer
	p := s.page(r, name, v)
	if err := pagesIn[p.Lang].ExecuteTemplate(&b, name, p); err != nil {
		s.oops(w, r, err)
		return
	}
	// Rendered: the page's turn is given back before it goes to the
	// network, however long the reader takes to read it.
	if t := turnOf(r.Context()); t != nil {
		t.give()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b.Bytes())
}

type statusView struct {
	Doing, Err string
	Judged     int
	ReadOnly   bool // the reader may change nothing, but what Teams have
	Admin      bool // the reader may change settings and teams
	// Teams are the teams the reader edits, when they are no editor.
	Teams []int64
	// RolesKept says the reader, an admin, may give roles in the UI.
	RolesKept  bool
	User, Role string
	Version    string
	Zone       string
	// Changes to the rules: on (--changes), and paused.
	ChangesOn, ChangesPaused bool
	// SignOut offers to sign out: the reader is signed in with a session.
	SignOut bool
	// Why is why the reader may change nothing, when it isn't their role
	// (see Server.why).
	Why string
}

func (s *Server) statusView(l string) statusView {
	// The leader's, on a replica that follows.
	st := s.shownStatus()
	v := statusView{Judged: int(100 * st.Judged), ReadOnly: s.cfg.ReadOnly, Version: s.cfg.Version, Zone: s.cfg.Loc.String(),
		ChangesOn: s.cfg.Changes}
	v.ChangesPaused = s.cfg.Changes && s.changesPaused()
	if st.Doing != "" {
		v.Doing = tr(l, st.Doing, st.args()...)
	}
	if st.Err != "" {
		v.Err = tr(l, st.ErrWhat) + ": " + st.Err
	}
	return v
}

// oops answers with an error, as a page or as JSON.
func (s *Server) oops(w http.ResponseWriter, r *http.Request, err error) {
	code := http.StatusInternalServerError
	var bad badRequest
	if c, ok := errCode(err); ok {
		code = c
	}
	var no forbidden
	if errors.As(err, &no) {
		code = http.StatusForbidden
	}
	switch {
	case code != http.StatusInternalServerError:
	case errors.As(err, &bad):
		code = http.StatusBadRequest
	case errors.Is(err, ledger.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ledger.ErrNotYours), errors.Is(err, ledger.ErrTaken), errors.Is(err, ledger.ErrTooMany):
		code = http.StatusConflict
	case errors.Is(err, context.Canceled):
		return
	}
	if code == http.StatusInternalServerError {
		s.log.Error("serving", "path", r.URL.Path, "err", err)
	}
	text := func(lang string) string { return message(lang, err) }
	if errors.As(err, new(notLetIn)) {
		text = func(lang string) string { return tr(lang, notLetInText, s.user(r)) }
	}
	if errors.Is(err, errPagesBusy) {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(renderWait/time.Second))))
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": text("en")})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	var b bytes.Buffer
	p := s.page(r, "error", struct {
		Code int
		Err  string
	}{code, err.Error()})
	p.View = struct {
		Code int
		Err  string
	}{code, text(p.Lang)}
	pagesIn[p.Lang].ExecuteTemplate(&b, "error", p)
	w.Write(b.Bytes())
}

// badRequest is a request that can't be done as it is; its message is
// kept as a format and args, to be shown in the reader's language.
type badRequest struct {
	error
	format string
	args   []any
}

func bad(format string, args ...any) error {
	return badRequest{fmt.Errorf(format, args...), format, args}
}

// message is an error in a language: one whose text was kept apart from
// its values is translated, or one that is the same text each time.
func message(lang string, err error) string {
	var b badRequest
	var inv ledger.ErrInvalid
	switch {
	case errors.As(err, &b) && b.format != "":
		return tr(lang, b.format, b.args...)
	case errors.As(err, &inv):
		return tr(lang, inv.Format, inv.Args...)
	}
	if t, ok := catalogs[lang][err.Error()]; ok {
		return t
	}
	return err.Error()
}

// back sends a browser back to a page after a form.
func back(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// form reads a posted form.
func form(r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return bad("%v", err)
	}
	return nil
}

// parseTime reads a time from a form, in the zone the UI shows times in.
func (s *Server) parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, v, s.cfg.Loc); err == nil {
			return t, nil
		}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Time{}, bad("%q: want a time like 2026-11-02T09:30", v)
}

// parseLabels reads labels written as service=checkout service=cart
// team=pay.
func parseLabels(v string) (incident.Labels, error) {
	var out incident.Labels
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' || r == '\n' }) {
		k, val, ok := strings.Cut(f, "=")
		if !ok || k == "" || val == "" {
			return nil, bad("labels %q: write them as service=checkout team=pay", v)
		}
		if out == nil {
			out = incident.Labels{}
		}
		out[k] = append(out[k], val)
	}
	return out, nil
}

func (s *Server) postIncident(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	start, err := s.parseTime(r.PostFormValue("start"))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	end, err := s.parseTime(r.PostFormValue("end"))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	labels, err := parseLabels(r.PostFormValue("labels"))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	in := incident.Incident{ID: strings.TrimSpace(r.PostFormValue("id")), Title: strings.TrimSpace(r.PostFormValue("title")),
		Start: start, End: end, Labels: labels}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	id, err := s.l.Put(r.Context(), in, s.user(r), o)
	if err != nil {
		s.oops(w, r, wrapBad(err))
		return
	}
	back(w, r, "/incidents/"+url.PathEscape(id))
}

// wrapBad makes an error from checking what was written a 400.
func wrapBad(err error) error {
	if errors.Is(err, ledger.ErrNotYours) || errors.Is(err, ledger.ErrTaken) || errors.Is(err, ledger.ErrNotFound) {
		return err
	}
	if strings.HasPrefix(err.Error(), "an ") || strings.HasPrefix(err.Error(), "state ") || strings.Contains(err.Error(), "isn't an impact window") {
		return badRequest{error: err}
	}
	return err
}

func (s *Server) deleteIncident(w http.ResponseWriter, r *http.Request) {
	o, err := s.options(r.Context())
	if err == nil {
		err = s.l.Delete(r.Context(), r.PathValue("id"), s.user(r), o)
	}
	if err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, "/incidents")
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	made, err := s.l.Decide(r.Context(), r.PathValue("id"), r.PostFormValue("state"), strings.TrimSpace(r.PostFormValue("title")), s.user(r), o)
	if err != nil {
		s.oops(w, r, wrapBad(err))
		return
	}
	if made != "" {
		back(w, r, "/incidents/"+url.PathEscape(made))
		return
	}
	back(w, r, "/incidents")
}

// real writes an incident from a black stone: the firing caught something
// nobody wrote down.
func (s *Server) real(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.oops(w, r, bad("no such firing"))
		return
	}
	made, f, err := s.realOne(r, id)
	switch {
	case err != nil:
		s.oops(w, r, err)
	case made == "":
		// Pressed twice, or caught since: there is an incident already.
		back(w, r, "/firings?day="+f.Start.In(s.cfg.Loc).Format("2006-01-02"))
	default:
		back(w, r, "/incidents/"+url.PathEscape(made))
	}
}

// realOne writes the firing id down as an incident, from its start and end,
// with the labels --match matches on: the incident's id, or "" when the
// firing has caught something, or one was written from it already. The
// ledger does it in one write (ledger.Real): two at once write one.
func (s *Server) realOne(r *http.Request, id int64) (string, ledger.Firing, error) {
	o, err := s.options(r.Context())
	if err != nil {
		return "", ledger.Firing{}, err
	}
	return s.l.Real(r.Context(), id, s.user(r), o)
}

func (s *Server) postSettings(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	values := map[string]string{}
	for _, st := range settable {
		v := strings.TrimSpace(r.PostFormValue(st.key))
		if r.PostFormValue("reset-"+st.key) != "" {
			v = ""
		}
		if v != "" {
			var o verdict.Options
			if err := apply(&o, st.key, v); err != nil {
				s.oops(w, r, bad("%s %s: %v", st.flag, v, err))
				return
			}
			if v == show(s.cfg.Options, st.key) {
				v = "" // the command line's: nothing to keep
			}
		}
		values[st.key] = v
	}
	before, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if err := s.l.SetSettings(r.Context(), values, s.user(r)); err != nil {
		s.oops(w, r, err)
		return
	}
	after, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	// The noisy thresholds are read when stones are counted; the rest
	// decide the stones themselves.
	if before.Lead != after.Lead || before.RefireWithin != after.RefireWithin || strings.Join(before.Match, ",") != strings.Join(after.Match, ",") {
		// Here, and through the ledger, wherever the leader runs.
		s.askRejudge(r.Context())
	}
	back(w, r, "/settings")
}

func (s *Server) settingsYAML(w http.ResponseWriter, r *http.Request) {
	set, err := s.l.Settings(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	var b strings.Builder
	b.WriteString("# Judging settings changed in lapilli serve's UI, as Helm values.\n")
	b.WriteString("# Commit them, then reset them in the UI.\n")
	var args []string
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	for _, st := range settable {
		if _, ok := set[st.key]; !ok {
			continue
		}
		if st.key == "match" {
			fmt.Fprintf(&b, "match: %s\n", yamlList(o.Match))
			continue
		}
		args = append(args, fmt.Sprintf("%q", st.flag+"="+show(o, st.key)))
	}
	if len(args) > 0 {
		fmt.Fprintf(&b, "extraArgs: [%s]\n", strings.Join(args, ", "))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, b.String())
}

func yamlList(vs []string) string {
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = strconv.Quote(v)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// The API.

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// period reads from and to (RFC 3339), the last seven days when not given.
func (s *Server) period(r *http.Request, longest time.Duration) (time.Time, time.Time, error) {
	q := r.URL.Query()
	to := s.now()
	from := to.Add(-7 * 24 * time.Hour)
	var err error
	if v := q.Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, bad("from %q: want an RFC 3339 time", v)
		}
	}
	if v := q.Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, bad("to %q: want an RFC 3339 time", v)
		}
	}
	switch {
	case !to.After(from):
		return from, to, bad("to must be after from")
	case to.Sub(from) > longest:
		return from, to, bad("at most %s at a time", verdict.Duration(longest))
	}
	return from, to, nil
}

// report is the report API. With every firing it is heavy: a hundred
// thousand firings at a time, one request at a time, its turn held until
// the answer is written, and the writing given 30 seconds, so a client that
// doesn't read can't keep one answer after another in memory. It isn't
// kept with the pages: it is too big to.
func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("firings") == "false" {
		// Up to 31 days, the firings are read again to count them by the
		// hours they came in: one such report at a time, as with firings.
		if from, to, err := s.period(r, 366*24*time.Hour); err == nil && to.Sub(from) <= ledger.HoursLongest && s.countsHours(r.Context()) {
			done, err := s.busy(r.Context())
			if err != nil {
				s.oops(w, r, err)
				return
			}
			defer done()
		}
		s.cached(s.apiReport)(w, r)
		return
	}
	from, to, err := s.period(r, 31*24*time.Hour)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	done, err := s.heavyTurn(r.Context(), from, to)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	defer done()
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	s.apiReport(w, r)
}

// heavyTurn takes the turn for loading every firing of [from, to), and
// refuses more firings than reportFirings.
func (s *Server) heavyTurn(ctx context.Context, from, to time.Time) (func(), error) {
	n, err := s.l.CountFirings(ctx, from, to)
	if err != nil {
		return nil, err
	}
	if n > reportFirings {
		return nil, bad("%d firings in that period, more than %d at a time: ask for a shorter period, or firings=false", n, reportFirings)
	}
	return s.busy(ctx)
}

// reportFirings is the most firings a report gives at once, and busyWait
// how long a heavy request waits for its turn.
var (
	reportFirings = 100_000
	busyWait      = 30 * time.Second
)

func (s *Server) apiReport(w http.ResponseWriter, r *http.Request) {
	withFirings := r.URL.Query().Get("firings") != "false"
	longest := 366 * 24 * time.Hour
	if withFirings {
		longest = 31 * 24 * time.Hour
	}
	from, to, err := s.period(r, longest)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	rep, err := s.ledgerReport(r.Context(), from, to, o, withFirings)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, rep)
}

// apiIncident is an incident as the API gives and takes it.
type apiIncident struct {
	ID       string          `json:"id"`
	Title    string          `json:"title,omitempty"`
	Start    time.Time       `json:"start"`
	End      time.Time       `json:"end,omitzero"`
	Labels   incident.Labels `json:"labels,omitempty"`
	Origin   string          `json:"origin,omitempty"`
	State    string          `json:"state,omitempty"`
	Counts   bool            `json:"counts"`
	Updated  time.Time       `json:"updated,omitzero"`
	By       string          `json:"by,omitempty"`
	CaughtBy []verdict.Catch `json:"caught_by"`
}

func (s *Server) apiIncidents(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.period(r, 366*24*time.Hour)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	incs, err := s.l.Incidents(r.Context(), from, to, true)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	out := []apiIncident{}
	for _, in := range incs {
		out = append(out, apiIncident{in.ID, in.Title, in.Start, in.End, in.Labels, in.Origin, in.State, in.Counts, in.Updated, in.By, in.CaughtBy})
	}
	writeJSON(w, out)
}

func (s *Server) apiPutIncident(w http.ResponseWriter, r *http.Request) {
	var in apiIncident
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.oops(w, r, bad("want an incident as JSON: %v", err))
		return
	}
	if id := r.PathValue("id"); id != "" {
		in.ID = id
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	id, err := s.l.Put(r.Context(), incident.Incident{ID: in.ID, Title: in.Title, Start: in.Start, End: in.End, Labels: in.Labels}, s.user(r), o)
	if err != nil {
		s.oops(w, r, wrapBad(err))
		return
	}
	got, err := s.l.IncidentByID(r.Context(), id)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if r.Method == http.MethodPost {
		w.Header().Set("Location", "/api/v1/incidents/"+url.PathEscape(id))
		w.WriteHeader(http.StatusCreated)
	}
	writeJSON(w, apiIncident{got.ID, got.Title, got.Start, got.End, got.Labels, got.Origin, got.State, got.Counts, got.Updated, got.By, got.CaughtBy})
}

func (s *Server) apiDeleteIncident(w http.ResponseWriter, r *http.Request) {
	o, err := s.options(r.Context())
	if err == nil {
		err = s.l.Delete(r.Context(), r.PathValue("id"), s.user(r), o)
	}
	if err != nil {
		s.oops(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiRules(w http.ResponseWriter, r *http.Request) {
	v, err := s.rulesPage(r)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, v)
}

func (s *Server) apiRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.l.Runs(r.Context(), 100)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if runs == nil {
		runs = []ledger.Run{}
	}
	writeJSON(w, runs)
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.settingsView(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, v)
}
