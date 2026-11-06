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
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
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

var pages = template.Must(template.New("").Funcs(funcs).ParseFS(files, "templates/*.html"))

// Handler is the web UI, the API and the metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(files, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", immutable(http.FileServerFS(assets))))
	s.checks(mux)

	page := func(name string, view func(*http.Request) (any, error)) http.HandlerFunc {
		return s.cached(func(w http.ResponseWriter, r *http.Request) {
			v, err := view(r)
			if err != nil {
				s.oops(w, r, err)
				return
			}
			s.render(w, r, name, v)
		})
	}
	mux.HandleFunc("GET /{$}", page("overview", s.overview))
	mux.HandleFunc("GET /rules", page("rules", s.rulesPage))
	mux.HandleFunc("GET /rules/{name...}", page("rule", s.rulePage))
	mux.HandleFunc("GET /incidents", page("incidents", s.incidentsPage))
	mux.HandleFunc("GET /incidents/{id...}", page("incident", s.incidentPage))
	mux.HandleFunc("GET /firings", page("firings", s.firingsPage))
	mux.HandleFunc("GET /settings", page("settings", s.settingsPage))
	mux.HandleFunc("GET /settings.yaml", s.settingsYAML)
	mux.HandleFunc("GET /runs", page("runs", s.runsPage))
	mux.HandleFunc("GET /outcomes", page("outcomes", s.outcomesPage))
	mux.HandleFunc("GET /lang", chooseLang)
	s.workRoutes(mux, page)

	mux.HandleFunc("POST /incidents", s.write(s.postIncident))
	mux.HandleFunc("POST /incidents/{id}/delete", s.write(s.deleteIncident))
	mux.HandleFunc("POST /incidents/{id}/decide", s.write(s.decide))
	mux.HandleFunc("POST /firings/{id}/real", s.write(s.real))
	mux.HandleFunc("POST /settings", s.admin(s.postSettings))
	mux.HandleFunc("GET /api/v1/me", s.me)

	mux.HandleFunc("GET /api/v1/report", s.report)
	mux.HandleFunc("GET /api/v1/rules", s.cached(s.apiRules))
	mux.HandleFunc("GET /api/v1/incidents", s.cached(s.apiIncidents))
	mux.HandleFunc("POST /api/v1/incidents", s.write(s.apiPutIncident))
	mux.HandleFunc("PUT /api/v1/incidents/{id}", s.write(s.apiPutIncident))
	mux.HandleFunc("DELETE /api/v1/incidents/{id}", s.write(s.apiDeleteIncident))
	mux.HandleFunc("GET /api/v1/runs", s.cached(s.apiRuns))
	mux.HandleFunc("GET /api/v1/settings", s.cached(s.apiSettings))
	mux.HandleFunc("GET /api/v1/outcomes", s.cached(s.apiOutcomes))
	mux.Handle("/mcp", s.mcpHandler())

	cop := http.NewCrossOriginProtection()
	return s.hosts(headers(gzipped(cop.Handler(mux))))
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

// errBusy is a heavy request that found another taking its turn.
var errBusy = errors.New("the server is busy with another large request: try again in a minute")

// busy takes the turn for a heavy request, waiting for it up to thirty
// seconds; the func it returns gives the turn back.
func (s *Server) busy(ctx context.Context) (func(), error) {
	t := time.NewTimer(busyWait)
	defer t.Stop()
	select {
	case s.heavy <- struct{}{}:
		return func() { <-s.heavy }, nil
	case <-t.C:
		return nil, errBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// checks are the health checks and the metrics, which a second listener
// (--metrics-listen) serves alone: a NetworkPolicy can then let Prometheus
// reach them, and no more.
func (s *Server) checks(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "the first run hasn't finished", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /metrics", s.metrics)
}

// MetricsHandler is the health checks and the metrics alone.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	s.checks(mux)
	return s.hosts(headers(gzipped(mux)))
}

// IncidentsHandler is writing incidents, with a token, and nothing else. It
// is for a job that copies them from where they are kept: on a port of its
// own, because a NetworkPolicy that let the job reach the UI's port would let
// it name itself anyone there, in the headers the sign-in proxy sets. Here,
// those headers mean nothing.
func (s *Server) IncidentsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/incidents", s.withToken(s.apiPutIncident))
	mux.HandleFunc("PUT /api/v1/incidents/{id}", s.withToken(s.apiPutIncident))
	mux.HandleFunc("DELETE /api/v1/incidents/{id}", s.withToken(s.apiDeleteIncident))
	return s.hosts(headers(mux))
}

// tokenUser is who changes made with the incidents token are noted as: not a
// person, so outcomes don't count them as use.
const tokenUser = "incidents-token"

// actingAs is who a request acts as, in its context, when that isn't for the
// sign-in proxy to say.
type actingAs struct{}

// withToken is a handler for those who hold the incidents token.
func (s *Server) withToken(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadOnly {
			http.Error(w, "lapilli serve is read-only (--read-only)", http.StatusForbidden)
			return
		}
		if !s.tokenOK(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "this port takes the token in --incidents-token-file, as a bearer token", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), actingAs{}, tokenUser))
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r)
	}
}

// tokenOK reports whether a request holds the incidents token. The file is
// read each time, so a rotated token is picked up; both are hashed first, so
// comparing them takes as long whatever their lengths.
func (s *Server) tokenOK(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || got == "" {
		return false
	}
	b, err := os.ReadFile(s.cfg.IncidentsToken)
	if err != nil {
		s.log.Warn("the incidents token can't be read", "err", err)
		return false
	}
	want := strings.TrimSpace(string(b))
	if want == "" {
		return false
	}
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
const (
	cacheBytes = 32 << 20
	cacheOne   = 2 << 20
)

type cached struct {
	ctype string
	body  []byte
}

// tag is the version of everything a page can show.
func (s *Server) tag() string {
	s.status.Lock()
	st := fmt.Sprintf("%s|%v|%s|%.2f", s.status.doing, s.status.args, s.status.err, s.status.judged)
	s.status.Unlock()
	h := fnv.New64a()
	io.WriteString(h, st)
	return fmt.Sprintf("%s-%d-%x", s.boot, s.l.Gen(), h.Sum64())
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(status int)      { r.status = status }

func (s *Server) cached(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tag := s.tag()
		// A page is in the reader's language, and shows who reads it what
		// they can change: it is kept by both.
		key := lang(r) + " " + r.URL.RequestURI()
		if s.cfg.UserHeader != "" {
			key = s.user(r) + "\x00" + strings.Join(s.groups(r), ",") + "\x00" + key
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
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c, ok := s.cache.get(tag, key)
		if !ok {
			rec := &recorder{header: http.Header{}, status: http.StatusOK}
			h(rec, r)
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

// user is who a request comes from: the header a sign-in proxy sets, when
// told to trust one.
func (s *Server) user(r *http.Request) string {
	if by, ok := r.Context().Value(actingAs{}).(string); ok {
		return by
	}
	// Named twice, the header may be the caller's as well as the proxy's.
	if s.cfg.UserHeader == "" || len(r.Header.Values(s.cfg.UserHeader)) != 1 {
		return "anonymous"
	}
	u := strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return -1
		}
		return c
	}, r.Header.Get(s.cfg.UserHeader))
	if u == "" {
		return "anonymous"
	}
	if len(u) > 200 {
		u = u[:200]
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
}

type langLink struct {
	Name, Href string
	Current    bool
}

func (s *Server) page(r *http.Request, name string, v any) page {
	l := lang(r)
	p := page{Page: name, View: v, Status: s.statusView(l), Path: r.URL.Path, Loc: s.cfg.Loc, Lang: l}
	// What the page offers to change is what the reader may.
	role := s.role(r)
	p.Status.ReadOnly = role < editor
	p.Status.Admin = role == admin
	if s.cfg.UserHeader != "" {
		p.Status.User, p.Status.Role = s.user(r), role.String()
	}
	for _, n := range names {
		p.Langs = append(p.Langs, langLink{n.Name, "/lang?to=" + n.Code + "&back=" + url.QueryEscape(r.URL.RequestURI()), n.Code == l})
	}
	return p
}

// render writes a page, in the reader's language.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, v any) {
	var b bytes.Buffer
	p := s.page(r, name, v)
	if err := pagesIn[p.Lang].ExecuteTemplate(&b, name, p); err != nil {
		s.oops(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b.Bytes())
}

type statusView struct {
	Doing, Err string
	Judged     int
	ReadOnly   bool // the reader may change nothing
	Admin      bool // the reader may change settings and teams
	User, Role string
	Version    string
	Zone       string
}

func (s *Server) statusView(l string) statusView {
	s.status.Lock()
	defer s.status.Unlock()
	v := statusView{Judged: int(100 * s.status.judged), ReadOnly: s.cfg.ReadOnly, Version: s.cfg.Version, Zone: s.cfg.Loc.String()}
	if s.status.doing != "" {
		v.Doing = tr(l, s.status.doing, s.status.args...)
	}
	if s.status.err != "" {
		v.Err = tr(l, s.status.errWhat) + ": " + s.status.err
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
	case errors.Is(err, ledger.ErrNotYours), errors.Is(err, ledger.ErrTaken):
		code = http.StatusConflict
	case errors.Is(err, context.Canceled):
		return
	}
	if code == http.StatusInternalServerError {
		s.log.Error("serving", "path", r.URL.Path, "err", err)
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
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
	}{code, message(p.Lang, err)}
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
	f, err := s.l.FiringByID(r.Context(), id)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if f.White {
		// Pressed twice, or caught since: there is an incident already.
		back(w, r, "/firings?day="+f.Start.In(s.cfg.Loc).Format("2006-01-02"))
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	in := incident.Incident{Title: "caught by " + f.Rule, Start: f.Start, End: f.End}
	for _, m := range o.Match {
		if v, ok := f.Labels[m]; ok {
			if in.Labels == nil {
				in.Labels = incident.Labels{}
			}
			in.Labels[m] = []string{v}
		}
	}
	made, err := s.l.Put(r.Context(), in, s.user(r), o)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, "/incidents/"+url.PathEscape(made))
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
		s.rejudge.Store(true)
		s.poke()
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
	rep, err := s.l.Report(r.Context(), from, to, o, withFirings)
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
