package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/signin"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// Pull request backtests through serve (docs/design/backtests.md): a port
// of its own, --backtest-listen, takes a pull request's rules from a CI job
// that proves which repository it runs for, by the CI's ID token or a
// plain token (--backtest-trust); the leader replays them with serve's
// access and record, in a lane of its own, at a share of the rate; the job
// reads back the comment lapilli verdict --compare --changed-only
// --markdown would have written.

const (
	// backtestDays are the most days a backtest replays, 7 when not
	// given; backtestRules the most rules that differ, on either side.
	backtestDays, backtestDefaultDays = 14, 7
	backtestRules                     = 50
	// backtestsOfEntry are how many of an entry's may wait at once;
	// backtestReading, how many requests' bodies are read and checked at
	// once.
	backtestsOfEntry, backtestReading = 3, 2
	// backtestFiles are the most files of a side, backtestPath the longest
	// path, backtestZone the longest zone's name.
	backtestFiles, backtestPath, backtestZone = 2000, 512, 64
	// backtestReach is how far back a pull request's rule reads at most,
	// its ranges, subqueries and offsets added up: a week-over-week
	// comparison of a day's range; backtestPoints, the most points a
	// subquery has, as Prometheus holds a range query to.
	backtestReach, backtestPoints = 8 * 24 * time.Hour, 11000
	// backtestNoData is how far further back a Grafana rule's data source
	// query is asked when it returns nothing: whether it returned
	// anything in the day before (replay's probe).
	backtestNoData = 24 * time.Hour
	// backtestGrafanaQueries are the most queries of data sources a
	// Grafana rule that differs has, backtestGrafanaNodes its queries and
	// expressions in all: each query is asked a day at a time.
	backtestGrafanaQueries, backtestGrafanaNodes = 8, 16
	// backtestQueryTimeout is sent with each range query the lane asks, so
	// that a shared Prometheus gives up on a pull request's query before
	// its own default does.
	backtestQueryTimeout = time.Minute
	// backtestTold is how often a backtest running says how far it has
	// got.
	backtestTold = 30 * time.Second
	// backtestTokenKept is how long a token's row is kept: past the hour
	// a token is taken for, from its iat, and the clocks' leeway.
	backtestTokenKept = time.Hour + 10*time.Minute
)

// backtestBodyWithin is how long a request's body may take to come, from
// its headers: shorter in tests.
var backtestBodyWithin = 5 * time.Second

// backtestPoll is how often the leader looks for a backtest waiting, and
// whether one running was cancelled: shorter in tests.
var backtestPoll = 5 * time.Second

// backtestPort is what the backtest port keeps while serve runs.
type backtestPort struct {
	trust []*trusted
	// lane is the history the lane asks, through a Limiter of its own
	// paced at its share of --max-rate and by the runs' pace too; rate,
	// that share, 0 for none; parallel, the rules replayed at once.
	lane     replay.History
	rate     float64
	parallel int
	// laneCap holds the lane's requests in flight to parallel at once, of
	// every API it asks; share is its share of each API's rate.
	laneCap *prom.Limiter
	share   float64
	// logs are the runs' Lokis (replay.WithLogs): the lane asks each
	// through a Limiter of its own (laneLogs), kept by the runs' client in
	// lanes, its pace at the lane's share of the runs' pace, in paces.
	logs    replay.Logs
	lanesMu sync.Mutex
	lanes   map[*prom.Client]*prom.Client
	paces   map[*prom.Pace]*prom.Pace
	// asking are the paces of the Lokis the backtest running asks: the
	// runs keep the lane's share of those alone while it runs.
	asking atomic.Pointer[map[*prom.Pace]bool]
	// asked counts what the lane asked of the history; progress, the rules
	// it replayed.
	asked    prom.Counts
	progress replay.Progress
	// keys are the backtests' key hashes, by id, as this process knows
	// them: a job's id and key are told from them in memory, and only a
	// pair they know is read from the ledger.
	keys *btKeys
	// reads are how fast a backtest is read (GET, DELETE) with its key, by
	// its id, once the key is known right. misses are how many pairs keys
	// doesn't know may wait for a listing of the ledger, by the client's
	// address: past them, one is answered at once. A pair keys knows never
	// touches misses, so that one who guesses, from the address everyone
	// shares behind an ingress, can't keep a job from its own backtest.
	reads, misses perKey
	// wake is poked when a backtest is queued here, so the leader, if it
	// is this process, takes it at once.
	wake chan struct{}
	// tokens are the plain entries' tokens, as their files hold them.
	tokens plainTokens
	// busy is set while the lane runs a backtest: its share of the rate
	// is kept for it then.
	busy atomic.Bool
	// reading are the turns to read a request's body and check its rules,
	// backtestReading at once.
	reading chan struct{}

	mu sync.Mutex
	// ended are the backtests ended, and refused, since start, by entry
	// and how.
	ended map[[2]string]int
	// said is when a refusal was last logged, by entry ("" for a token no
	// entry is of).
	said map[string]time.Time
}

// trusted is an entry of --backtest-trust, as the port holds it.
type trusted struct {
	Trust
	ci     *signin.CI // nil for a plain token
	hourly *limiter
	// reading is the entry's one turn to have a body read.
	reading chan struct{}
}

// newBacktestPort readies the port: each issuer asked once for its keys,
// by every entry that names it; each entry's hour, shared among the
// replicas; and the lane's history.
func (s *Server) newBacktestPort() *backtestPort {
	p := &backtestPort{reads: perKey{rate: 1, burst: 10}, misses: perKey{rate: 1, burst: 20}, wake: make(chan struct{}, 1), reading: make(chan struct{}, backtestReading), ended: map[[2]string]int{}, said: map[string]time.Time{}}
	p.keys = newBTKeys(s.life, func(ctx context.Context) (map[string]string, error) { return s.l.BacktestKeys(ctx, backtestKeysListed) })
	cis := map[[2]string]*signin.CI{}
	for _, t := range s.cfg.Trust {
		tr := &trusted{Trust: t, hourly: &limiter{rate: float64(t.Hourly) / 3600, burst: float64(t.Hourly)}, reading: make(chan struct{}, 1)}
		if t.Issuer != "" {
			k := [2]string{t.Issuer, t.JWKSURL}
			if cis[k] == nil {
				cis[k] = &signin.CI{Issuer: t.Issuer, JWKS: t.JWKSURL, Client: s.backtestClient(), Now: func() time.Time { return s.now() }}
			}
			tr.ci = cis[k]
		}
		if s.l.Shared() {
			tr.hourly.share(s, "backtest:"+t.Name)
		}
		p.trust = append(p.trust, tr)
	}
	parallel := s.cfg.Parallel
	if parallel <= 0 {
		parallel = 8
	}
	p.parallel = max(1, parallel/4)
	p.share, p.laneCap = s.share(), prom.NewCap(p.parallel)
	base := replay.Base(s.h)
	if c, ok := base.(*prom.Client); ok {
		// The lane's share of --max-rate is kept for it while it runs a
		// backtest: the runs' pace slows by as much then, and the lane
		// asks at its own, which nothing else takes turns of.
		runs := c.Limiter()
		own := runs.Pace().Share(s.share())
		runs.Pace().Reserve(s.share(), p.busy.Load)
		p.rate = own.Rate()
		p.lane = c.Lane(prom.NewLimiterPaced(own).Within(p.laneCap), backtestQueryTimeout)
	} else {
		p.lane = base
	}
	// A pull request's rules in LogQL (a Loki ruler's, a Grafana rule's
	// on a Loki data source) are asked of the runs' Lokis, each at the
	// lane's share as --prometheus is.
	if p.logs = replay.LogsOf(s.h); p.logs != nil {
		p.lanes, p.paces = map[*prom.Client]*prom.Client{}, map[*prom.Pace]*prom.Pace{}
		p.lane = replay.WithLogs(p.lane, p.laneLogs)
	}
	return p
}

// laneLogs is the history a rule in LogQL is replayed against in the lane:
// the Loki the runs ask for it (--loki, or a Grafana Loki data source
// through --grafana's data source proxy, as source.Logs gives them, with
// serve's token and tenant), through a Limiter of the lane's own, at its
// share of that API's pace, which the runs keep for it while a backtest
// asking it runs, and within the lane's cap.
func (p *backtestPort) laneLogs(r rules.Rule) (replay.History, error) {
	h, err := p.logs(r)
	if err != nil {
		return nil, err
	}
	c, ok := h.(*prom.Client)
	if !ok {
		return h, nil
	}
	p.lanesMu.Lock()
	defer p.lanesMu.Unlock()
	if l := p.lanes[c]; l != nil {
		return l, nil
	}
	var own *prom.Pace
	if pace := c.Limiter().Pace(); pace != nil {
		if own = p.paces[pace]; own == nil {
			own = pace.Share(p.share)
			pace.Reserve(p.share, func() bool { return p.busy.Load() && p.asks(pace) })
			p.paces[pace] = own
		}
	}
	l := c.Lane(prom.NewLimiterPaced(own).Within(p.laneCap), backtestQueryTimeout)
	p.lanes[c] = l
	return l, nil
}

// asks says whether the backtest running asks a Loki of this pace.
func (p *backtestPort) asks(pace *prom.Pace) bool {
	m := p.asking.Load()
	return m != nil && (*m)[pace]
}

// pacesOf are the paces of the Lokis rules in LogQL are asked of, of those
// replayed: the runs' Limiters' own.
func (p *backtestPort) pacesOf(sides ...[]rules.Rule) map[*prom.Pace]bool {
	out := map[*prom.Pace]bool{}
	for _, rs := range sides {
		for _, r := range rs {
			if r.Logs == "" || r.Unsupported != "" || p.logs == nil {
				continue
			}
			h, err := p.logs(r)
			if c, ok := h.(*prom.Client); err == nil && ok && c.Limiter().Pace() != nil {
				out[c.Limiter().Pace()] = true
			}
		}
	}
	return out
}

// share is the lane's share of --max-rate.
func (s *Server) share() float64 {
	if s.cfg.BacktestShare > 0 {
		return s.cfg.BacktestShare
	}
	return 0.25
}

// backtestClient fetches the issuers' discovery documents and keys:
// signin's, through egress; tests give a transport.
func (s *Server) backtestClient() *http.Client {
	return signin.Client(s.cfg.BacktestTransport)
}

// BacktestHandler is the backtest port, and nothing else: POST
// /v1/backtests, GET and DELETE /v1/backtests/{id}. It checks no host
// name, as the notifications port doesn't: every request carries its own
// proof, which a browser doesn't add of itself; one that says where a page
// sent it from (Origin) is refused.
func (s *Server) BacktestHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/backtests", s.postBacktest)
	mux.HandleFunc("GET /v1/backtests/{id}", s.getBacktest)
	mux.HandleFunc("DELETE /v1/backtests/{id}", s.deleteBacktest)
	return headers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Origin") != "" {
			backtestError(w, http.StatusForbidden, "a request from a page is refused here")
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// backtestError answers with a reason of Lapilli's own words, as JSON.
func backtestError(w http.ResponseWriter, code int, why string) {
	backtestJSON(w, code, map[string]any{"error": why})
}

func backtestJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// refuse answers a token this port doesn't take: 401, with no word of
// why, which the log says, once a minute an entry at most.
func (s *Server) refuse(w http.ResponseWriter, entry, why string) {
	s.noteRefused(entry, why)
	w.Header().Set("WWW-Authenticate", "Bearer")
	backtestError(w, http.StatusUnauthorized, "unauthorized")
}

// noteRefused logs why a token was refused, once a minute an entry at
// most ("" for a token no entry is of): anyone who reaches the port could
// otherwise fill the log.
func (s *Server) noteRefused(entry, why string) {
	p := s.btPort
	p.mu.Lock()
	say := s.now().Sub(p.said[entry]) >= time.Minute
	if say {
		p.said[entry] = s.now()
	}
	p.mu.Unlock()
	if say {
		if entry == "" {
			s.log.Warn("backtest port: a token refused", "why", why)
		} else {
			s.log.Warn("backtest port: a token refused", "trust", entry, "why", why)
		}
	}
}

// asker is who a request to the port is from: its entry, and, of an ID
// token, what it was known by and its claims.
type asker struct {
	t      *trusted
	token  string // the token's key: its jti or hash; "" for a plain token
	iat    time.Time
	claims map[string]json.RawMessage
}

// bearerOf is the request's bearer token, "" for none.
func bearerOf(r *http.Request) string {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(got)
}

// who says which entry a request's token is of, before its body is read:
// a plain token, held to each plain entry's token (plainTokens), both
// hashed and compared in constant time; or an ID token, whose iss alone is
// read before it is verified, to pick the entries of its issuer, checked
// once by the issuer's keys, and then held to each entry's audience and
// pins. An iss no entry names asks nothing of anyone.
func (s *Server) who(ctx context.Context, w http.ResponseWriter, r *http.Request) (asker, bool) {
	raw := bearerOf(r)
	if raw == "" || len(raw) > 8<<10 {
		s.refuse(w, "", "no bearer token, or one past 8 KiB")
		return asker{}, false
	}
	x := sha256.Sum256([]byte(raw))
	plain := s.btPort.tokens.of(s)
	for _, t := range s.btPort.trust {
		if t.TokenFile == "" {
			continue
		}
		want, ok := plain[t.Name]
		if !ok {
			s.noteRefused(t.Name, "its token_file can't be read, is shorter than 32 characters, or holds another token serve takes: none is taken with it until it is put right")
			continue
		}
		if subtle.ConstantTimeCompare(x[:], want[:]) == 1 {
			return asker{t: t}, true
		}
	}
	iss, ok := issuerOf(raw)
	if !ok {
		s.refuse(w, "", "not a token any entry takes")
		return asker{}, false
	}
	why, of := "no entry names its issuer", ""
	verified := map[*signin.CI]signin.CIToken{}
	failed := map[*signin.CI]string{}
	for _, t := range s.btPort.trust {
		if t.ci == nil || t.Issuer != iss {
			continue
		}
		of = t.Name
		if f, ok := failed[t.ci]; ok {
			why = f
			continue
		}
		tok, ok := verified[t.ci]
		if !ok {
			var err error
			if tok, err = t.ci.VerifyToken(ctx, raw); err != nil {
				why = refusedWhy(err)
				failed[t.ci] = why
				continue
			}
			verified[t.ci] = tok
		}
		if !t.forUs(tok) {
			why = "its audience isn't the entry's, or it names another beside it"
			continue
		}
		if claim, ok := t.pinned(tok.Claims); !ok {
			why = "its " + claim + " claim isn't as the entry pins it"
			continue
		}
		// A merge queue's run is known by its ref alone, the queue's,
		// which says the pull request and the branch: one that isn't is no
		// merge group GitHub's merge queue made, and nothing else the token
		// says tells which pull request it is of.
		if (asker{t: t, claims: tok.Claims}).mergeGroup() {
			ref, _ := signin.ClaimString(tok.Claims["ref"])
			if _, _, ok := queueRef(ref); !ok {
				why = "it is a merge queue's (event_name merge_group), and its ref isn't the queue's, refs/heads/gh-readonly-queue/BRANCH/pr-N-SHA"
				continue
			}
		}
		// Known by its jti; or, with none, by what it says as it was
		// signed, never how it was written, which a lax decoder reads in
		// several ways as one.
		p := sha256.Sum256(tok.Payload)
		key := "claims:" + hex.EncodeToString(p[:])
		if jti, ok := signin.ClaimString(tok.Claims["jti"]); ok && jti != "" && len(jti) <= 256 {
			j := sha256.Sum256([]byte(jti))
			key = "jti:" + hex.EncodeToString(j[:])
		}
		return asker{t: t, token: key, iat: tok.IssuedAt, claims: tok.Claims}, true
	}
	s.refuse(w, of, why)
	return asker{}, false
}

// plainTokens are the plain entries' tokens, by entry, as their files hold
// them, hashed: read again only when a file serve takes a token from
// changed (its size, its time, what it is), and each held to the others
// then, not at each request. An entry whose file can't be read, holds
// fewer than 32 characters, or holds a token another file of serve's holds
// too, has none: none is taken with it until it is put right.
type plainTokens struct {
	mu    sync.Mutex
	stats map[string]tokenStat
	by    map[string][sha256.Size]byte
}

type tokenStat struct {
	size  int64
	mod   time.Time
	ok    bool
	inode string
}

// tokenFiles are the files of every token and secret serve takes but the
// backtest port's own: none of them may hold a plain entry's token.
func (s *Server) tokenFiles() []string {
	c := s.cfg
	return append([]string{c.IncidentsToken, c.FiringsToken, c.NotifyToken, c.TasksToken, c.ChangesToken, c.GrafanaToken, c.RulerToken,
		c.LokiToken, c.ChangesGrafanaToken, c.ChangesRulerToken, c.NotifyGrafanaSecret}, c.ResponsesFiles()...)
}

func statOf(path string) tokenStat {
	fi, err := os.Stat(path)
	if err != nil {
		return tokenStat{}
	}
	return tokenStat{size: fi.Size(), mod: fi.ModTime(), ok: true, inode: fmt.Sprint(fi.Sys())}
}

// of are the tokens as the files now hold them: each file stat'ed, and all
// read again only when one changed.
func (p *plainTokens) of(s *Server) map[string][sha256.Size]byte {
	var files []string
	for _, f := range s.tokenFiles() {
		if f != "" {
			files = append(files, f)
		}
	}
	for _, t := range s.btPort.trust {
		if t.TokenFile != "" {
			files = append(files, t.TokenFile)
		}
	}
	now := map[string]tokenStat{}
	for _, f := range files {
		now[f] = statOf(f)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.by != nil && maps.Equal(p.stats, now) {
		return p.by
	}
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	held := map[string]int{}
	for _, f := range s.tokenFiles() {
		if f != "" {
			if tk := read(f); tk != "" {
				held[tk]++
			}
		}
	}
	mine := map[string]string{}
	for _, t := range s.btPort.trust {
		if t.TokenFile != "" {
			if tk := read(t.TokenFile); tk != "" {
				mine[t.Name] = tk
				held[tk]++
			}
		}
	}
	p.by = map[string][sha256.Size]byte{}
	for name, tk := range mine {
		if len(tk) >= minBacktestToken && held[tk] == 1 {
			p.by[name] = sha256.Sum256([]byte(tk))
		}
	}
	p.stats = now
	return p.by
}

// issuerOf is the iss a compact JWS says, unverified: only to pick the
// entries it may be of.
func issuerOf(raw string) (string, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(b, &c) != nil || c.Iss == "" {
		return "", false
	}
	return c.Iss, true
}

// refusedWhy is why a token didn't verify, for the log, in Lapilli's
// words: what a token says is the sender's until it verifies, and isn't
// logged.
func refusedWhy(err error) string {
	msg := err.Error()
	switch {
	case errors.Is(err, signin.ErrUnreachable):
		return "the issuer couldn't be asked for its keys: " + verdict.Clean(msg)
	case errors.Is(err, signin.ErrMisconfigured):
		return "the issuer isn't as the entry says: " + verdict.Clean(msg)
	case strings.Contains(msg, "audience"):
		return "its audience isn't the entry's, or it names several"
	case strings.Contains(msg, "expired"):
		return "it expired"
	case strings.Contains(msg, "more than an hour ago"):
		return "it was issued more than an hour ago"
	case strings.Contains(msg, "future"), strings.Contains(msg, "before"), strings.Contains(msg, "nbf"):
		return "it isn't valid yet, or was issued in the future"
	case strings.Contains(msg, "iat"):
		return "it says no time it was issued"
	case strings.Contains(msg, "different provider"), strings.Contains(msg, "issuer"):
		return "another issuer's"
	}
	return "its signature doesn't verify by the issuer's keys, by an algorithm of a key pair"
}

// forUs reports whether a token was made for the entry's audience: that
// alone; or, of a Bitbucket entry, that beside its workspace's default,
// which Bitbucket puts in every token whatever the step asks, the
// workspace the entry pins (ReadTrust holds a Bitbucket entry to one).
// Never for a third service, and no other CI's beside anything.
func (t *trusted) forUs(tok signin.CIToken) bool {
	if t.CI != "bitbucket" {
		return tok.For(t.Audience)
	}
	ws := t.Claims["workspaceUuid"].Values
	if len(ws) != 1 {
		return tok.For(t.Audience)
	}
	return tok.ForBeside(t.Audience, bitbucketAudience(ws[0]))
}

// pinned holds a token's claims to the entry's pins, each exactly, as a
// string or an integer's decimal; the repository's id may be any one of a
// list. It names the first claim that isn't as pinned.
func (t *trusted) pinned(claims map[string]json.RawMessage) (string, bool) {
	for _, name := range sortedClaims(t.Claims) {
		got, ok := signin.ClaimString(claims[name])
		if !ok {
			return name, false
		}
		match := false
		for _, v := range t.Claims[name].Values {
			if subtle.ConstantTimeCompare([]byte(got), []byte(v)) == 1 {
				match = true
			}
		}
		if !match {
			return name, false
		}
	}
	return "", true
}

func sortedClaims(m map[string]Pin) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// backtestRequest is the body of POST /v1/backtests, read strictly.
type backtestRequest struct {
	Before       []backtestFile `json:"before"`
	After        []backtestFile `json:"after"`
	Expect       *string        `json:"expect"`
	ExpectBefore *string        `json:"expect_before"`
	RulesSelect  string         `json:"rules_select"`
	Days         int            `json:"days"`
	TZ           string         `json:"tz"`
	PullRequest  int64          `json:"pull_request"`
	Commit       string         `json:"commit"`
	// FailMissed holds the change to the incidents, as --fail-missed does:
	// the report says which incidents a rule caught before the change no
	// rule catches after it, the rules left alone judged by serve's record
	// of them (holdMissed). The job fails on it; serve fails nothing.
	FailMissed bool `json:"fail_missed,omitempty"`
}

type backtestFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Loki says it is a Loki ruler's rule file, read as --loki-rules reads
	// one: its rules' queries are LogQL, asked of --loki.
	Loki bool `json:"loki,omitempty"`
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// readBacktestBody reads the body, once the token verified, 4 MiB at most,
// as one JSON object of the fields known, nothing after it.
func readBacktestBody(w http.ResponseWriter, r *http.Request) ([]byte, backtestRequest, int, string) {
	var req backtestRequest
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ledger.MaxBacktestBody))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		return nil, req, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is %d MiB at most", ledger.MaxBacktestBody>>20)
	case err != nil:
		return nil, req, http.StatusBadRequest, "the body couldn't be read"
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, req, http.StatusBadRequest, "the body isn't a backtest's request: " + clip(verdict.Clean(err.Error()), 300)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, req, http.StatusBadRequest, "the body is one JSON object, and nothing after it"
	}
	// A file that is Terraform's JSON of a plan or a state holds every
	// resource's values, the sensitive ones among them: only its rule groups
	// are kept, in the request as it is queued and kept in the ledger.
	trimmed := false
	for _, files := range [][]backtestFile{req.Before, req.After} {
		for i := range files {
			if !strings.HasPrefix(strings.TrimSpace(files[i].Content), "{") {
				continue
			}
			kept, err := rules.TerraformRulesOnly([]byte(files[i].Content))
			if err != nil {
				return nil, req, http.StatusBadRequest, fmt.Sprintf("%s: %s", clip(verdict.Clean(files[i].Path), 200), clip(verdict.Clean(err.Error()), 300))
			}
			if string(kept) != files[i].Content {
				files[i].Content, trimmed = string(kept), true
			}
		}
	}
	if trimmed {
		var err error
		if b, err = json.Marshal(req); err != nil {
			return nil, req, http.StatusBadRequest, "the body couldn't be read"
		}
	}
	return b, req, 0, ""
}

// prepared is a backtest's request as read and checked, ready to be
// replayed: the rules alike, those it takes away and brings, the
// recording rules that differ, what reading each side passed over, the
// expectations, the period's days and the zone it is shown in.
type prepared struct {
	same, gone, came []rules.Rule
	recording        []verdict.RecordingChange
	passedOver       map[string]string
	expect, before   *verdict.Expectations
	days             int
	loc              *time.Location
	// reasons are why rules the request holds can't be replayed, as read
	// from it: its own words, said in the comment as they are.
	reasons map[string]bool
	// failMissed is the request's fail_missed.
	failMissed bool
}

// prepare reads and checks a backtest's request, as the port does before
// it is queued and the lane does again before it is run, its Grafana and
// Loki rules held to what the rules in effect read (eff): 400's reason
// when it can't be, in Lapilli's words and the request's own.
func (s *Server) prepare(req backtestRequest, t Trust, eff inEffectReads) (*prepared, string) {
	p := &prepared{days: req.Days, loc: s.cfg.Loc, passedOver: map[string]string{}, reasons: map[string]bool{}, failMissed: req.FailMissed}
	if p.days == 0 {
		p.days = backtestDefaultDays
	}
	switch {
	case p.days < 1 || p.days > backtestDays:
		return nil, fmt.Sprintf("days is 1 to %d", backtestDays)
	case p.days > t.Days:
		return nil, fmt.Sprintf("days is %d at most for this entry", t.Days)
	case len(req.Before)+len(req.After) == 0:
		return nil, "give the rules before and after: before, after"
	case req.PullRequest < 0 || req.PullRequest > math.MaxInt32:
		return nil, "pull_request is a pull request's number"
	case req.Commit != "" && !commitSHA.MatchString(req.Commit):
		return nil, "commit is a commit's hash, in lowercase hex"
	case req.ExpectBefore != nil && req.Expect == nil:
		return nil, "expect_before goes with expect"
	case len(req.TZ) > backtestZone || req.TZ == "Local":
		return nil, "tz is a zone's name, such as Asia/Seoul"
	case len(req.RulesSelect) > 1024:
		return nil, "rules_select is 1024 bytes at most"
	}
	if req.TZ != "" {
		loc, err := time.LoadLocation(req.TZ)
		if err != nil {
			return nil, "tz is a zone's name, such as Asia/Seoul"
		}
		p.loc = loc
	}
	if p.loc == nil {
		p.loc = time.Local
	}
	sel, err := rules.ParseSelector(req.RulesSelect)
	if err != nil {
		return nil, "rules_select: " + clip(verdict.Clean(err.Error()), 300)
	}
	var recBefore, recAfter []rules.Recording
	noneSelected := map[string]bool{}
	read := func(side string, files []backtestFile, rec *[]rules.Recording) ([]rules.Rule, string) {
		if len(files) > backtestFiles {
			return nil, fmt.Sprintf("%s: %d files at most", side, backtestFiles)
		}
		in := make([]rules.File, 0, len(files))
		seen, loki := map[string]bool{}, map[string]bool{}
		for _, f := range files {
			if f.Path == "" || len(f.Path) > backtestPath || !utf8.ValidString(f.Path) || strings.ContainsFunc(f.Path, isControl) || seen[f.Path] {
				return nil, fmt.Sprintf("%s: each file has a path of its own, %d bytes at most, with nothing that doesn't show", side, backtestPath)
			}
			seen[f.Path], loki[f.Path] = true, f.Loki
			in = append(in, rules.File{Path: f.Path, Data: []byte(f.Content)})
		}
		rs, _, picked, err := rules.PickedFiles(in, rules.Picking{Select: sel, Trim: true, Recordings: rec})
		noneSelected[side] = errors.Is(err, rules.ErrNoneSelected)
		if err != nil && !errors.Is(err, rules.ErrNoneSelected) {
			return nil, fmt.Sprintf("the rules %s: %s", side, clip(verdict.Clean(err.Error()), 600))
		}
		// A Loki ruler's file, read as --loki-rules reads one, as
		// rules.FromDocs reads a document of rules.LokiFile: its queries
		// LogQL, without keep_firing_for and query_offset, which Loki
		// 3.7.9 drops from a file and runs the rule without.
		for i := range rs {
			switch r := &rs[i]; {
			case !loki[r.Source]:
			case r.Graph != nil || r.UID != "" || r.Cloud != "":
				return nil, fmt.Sprintf("%s: %s isn't a Loki ruler's rule, in a file sent as one", side, verdict.Clean(r.Name))
			default:
				r.Logs, r.LokiFile, r.KeepFiringFor, r.QueryOffset = rules.LokiRuler, true, 0, 0
			}
		}
		if said := picked.String(); said != "" {
			key := "rules"
			if side == "before" {
				key = "compare"
			}
			p.passedOver[key] = said
		}
		return rs, ""
	}
	before, why := read("before", req.Before, &recBefore)
	if why != "" {
		return nil, why
	}
	after, why := read("after", req.After, &recAfter)
	if why != "" {
		return nil, why
	}
	// No rule left on either side, and the selector kept nothing of a side
	// it was given documents of: a selector that matches nothing, as
	// lapilli verdict refuses it, never two sets alike, the other side
	// empty or not (a pull request's first rules).
	if len(before)+len(after) == 0 && (noneSelected["before"] || noneSelected["after"]) {
		return nil, fmt.Sprintf("rules_select %s keeps no document of rules on either side", clip(verdict.Clean(req.RulesSelect), 200))
	}
	source.TellApart(before, after)
	p.same, p.gone, p.came = source.Split(before, after)
	if len(p.gone) > backtestRules || len(p.came) > backtestRules {
		return nil, fmt.Sprintf("%d rules that differ at most, on either side: backtest a change a few rules at a time", backtestRules)
	}
	// Replayed as the rules in effect are: --late, --scrape-interval, the
	// scrape intervals and types of Grafana's data sources as serve read
	// them for its own rules, where a Loki's ruler runs its groups.
	s.replayedAs().Apply(p.gone)
	s.replayedAs().Apply(p.came)
	for _, side := range [][]rules.Rule{p.gone, p.came} {
		for i := range side {
			// What it reads first, before anything it says is taken.
			if why := taken(side[i], eff); why != "" {
				return nil, why
			}
			if why := p.checkRule(&side[i]); why != "" {
				return nil, why
			}
		}
	}
	p.recording = verdict.RecordingChanges(recBefore, recAfter)
	// Given, however empty, as --expect takes an empty file: a field
	// left out is none.
	for _, e := range []struct {
		side string
		text *string
		into **verdict.Expectations
	}{{"expect", req.Expect, &p.expect}, {"expect_before", req.ExpectBefore, &p.before}} {
		if e.text == nil {
			continue
		}
		x, err := verdict.ReadExpectations(strings.NewReader(*e.text))
		if err != nil {
			return nil, e.side + ": " + clip(verdict.Clean(err.Error()), 600)
		}
		*e.into = x
	}
	return p, ""
}

// checkRule holds a rule that differs to what the port takes: its for,
// keep_firing_for and interval; its group's query_offset; each selector one
// metric's, by its name; what it reads back. A Grafana rule is held so in
// each query it asks, and to its own bounds (checkGraph); a rule in LogQL
// to LogQL's (checkLogQL). One whose expression doesn't read as PromQL
// asks nothing, and is said refused as written, as lapilli verdict says
// it, the rest backtested; one already unsupported as read is said in its
// own words.
func (p *prepared) checkRule(r *rules.Rule) string {
	name := verdict.Clean(r.Name)
	switch {
	case r.For < 0 || r.KeepFiringFor < 0 || r.Interval < 0 || r.QueryOffset < 0:
		return name + ": a duration is negative"
	case r.For > compareFor || r.KeepFiringFor > compareFor:
		return fmt.Sprintf("%s: for and keep_firing_for are %s at most here", name, verdict.Duration(compareFor))
	case r.Interval > compareEvery:
		return fmt.Sprintf("%s: its group's interval is %s at most here", name, verdict.Duration(compareEvery))
	case r.QueryOffset > backtestReach:
		// Each query asked that long before its evaluation: a read back,
		// as an offset is.
		return fmt.Sprintf("%s: its group's query_offset is %d days at most here", name, int(backtestReach/(24*time.Hour)))
	case r.Unsupported != "":
		p.reasons[r.Unsupported] = true
		return ""
	case r.Graph != nil:
		return p.checkGraph(r)
	case r.Logs != "":
		return checkLogQL(name, r.Expr, r.QueryOffset, true)
	}
	return p.checkPromQL(r, name, r.Expr, r.QueryOffset, true)
}

// checkPromQL holds a PromQL query a rule asks, read back before from each
// evaluation, to what the port takes: one that doesn't read leaves the
// rule unsupported, refused as written. A refusal says the query as name,
// and, with quote, what of it is past the bounds; without, nothing of it.
//
// A \r is refused, whatever else: Prometheus ends a # comment at one, and
// the checks here do too, but a rule's lines end in \n alone (YAML makes a
// \r\n one), so none has a reason to hold one but to read otherwise than
// it looks. So is what a history --prometheus names may read otherwise
// than these checks do (rules.Unalike): Prometheus with unquoted UTF-8
// names, VictoriaMetrics' MetricsQL.
func (p *prepared) checkPromQL(r *rules.Rule, name, expr string, before time.Duration, quote bool) string {
	if strings.Contains(expr, "\r") {
		return name + " holds a carriage return (\\r): here a rule's PromQL ends its lines in \\n alone"
	}
	if what := rules.Unalike(expr); what != "" {
		said := name + " holds what a history may read otherwise than Prometheus does"
		if quote {
			said = name + " holds " + clip(verdict.Clean(what), 200)
		}
		return said + ": here a rule's PromQL is read alike by Prometheus and the histories like it (VictoriaMetrics' MetricsQL): outside its strings, ASCII names, numbers, operators and brackets alone, an offset or @ right after a selector, a range or a subquery, and no WITH, nor or, and or unless in a selector's braces"
	}
	if _, err := rules.Selectors(expr); err != nil {
		r.Unsupported = refusedAsWritten
		p.reasons[r.Unsupported] = true
		return ""
	}
	if sel, wide := rules.Wide(expr); wide {
		if !quote {
			return name + " reads a selector that names no metric, or names it by a pattern: here each selector reads one metric, by its name"
		}
		return fmt.Sprintf("%s reads %s, which names no metric, or names it by a pattern: here each selector reads one metric, by its name", name, clip(verdict.Clean(sel), 200))
	}
	if what := rules.Unnamed(expr); what != "" {
		return name + " reads " + what + ": here a rule reads the metrics it names, and nothing beside them"
	}
	if what := rules.Reach(expr, backtestReach-before, backtestPoints); what != "" {
		said := name + " reads past what is taken"
		if quote {
			said = name + " reads " + clip(verdict.Clean(what), 300)
		}
		return fmt.Sprintf("%s: here what a rule reads, its ranges and offsets and its subqueries' added up, with its group's query_offset or its query's relativeTimeRange, reaches %d days back at most, a subquery %d points with a step of its own, each written as a duration, and no @",
			said, int(backtestReach/(24*time.Hour)), backtestPoints)
	}
	return ""
}

// checkGraph holds a Grafana rule that differs to what the port takes:
// backtestGrafanaQueries queries of data sources and backtestGrafanaNodes
// queries and expressions at most; each query's relativeTimeRange within
// backtestReach; and each query the replay asks, PromQL or LogQL, as a
// rule's query is held, read back as far again as its relativeTimeRange
// ends before the evaluation, and, of a reduce over LogQL, as its window;
// and each data source query a day further back again, as the replay asks
// it when it returns nothing (backtestNoData). Its expressions are worked
// out by Lapilli, and read nothing. A refusal
// says the query by its refId and quotes nothing of it: what the replay
// asks has the macros put in, $__rate_interval with serve's data source's
// scrape interval among them, and isn't what the pull request wrote.
func (p *prepared) checkGraph(r *rules.Rule) string {
	name := verdict.Clean(r.Name)
	g := r.Graph
	queries, all := g.Nodes()
	switch far := g.Furthest(); {
	case queries > backtestGrafanaQueries || all > backtestGrafanaNodes:
		return fmt.Sprintf("%s has %d queries of data sources, and %d queries and expressions in all: here a Grafana rule has %d and %d at most",
			name, queries, all, backtestGrafanaQueries, backtestGrafanaNodes)
	case far > backtestReach:
		return fmt.Sprintf("%s asks a query from %s before each evaluation (its relativeTimeRange): here %d days at most", name, verdict.Duration(far), int(backtestReach/(24*time.Hour)))
	}
	refs := slices.Collect(maps.Keys(g.Queries))
	for ref := range g.Asks {
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	for _, ref := range refs {
		expr, _ := g.Expr(ref, r.AskStep(ref))
		before := g.Shift[ref]
		query := name + "'s query " + clip(verdict.Clean(ref), 100)
		if g.Logs != "" {
			if a, ok := g.Asks[ref]; ok && a.Logs {
				before += a.Window
			}
			if why := checkLogQL(query, expr, before, false); why != "" {
				return why
			}
			continue
		}
		if why := p.checkPromQL(r, query, expr, before, false); why != "" || r.Unsupported != "" {
			return why
		}
	}
	// A data source query that returns nothing is asked again over the day
	// before (replay's before: a day's subquery of it, or, in LogQL, the
	// query itself over the day), from as long before the evaluation as
	// it is: a day further back than it reads otherwise. A classic
	// condition's window is in what its asks write (Ask.PromQL), a half
	// step past the window's first numbers, which the replay asks of the
	// query from the window's start.
	// In PromQL, that day is a subquery at the rule's interval (a minute at
	// most, a second at least: replay.ProbeStep), held to backtestPoints
	// as any subquery is: a group evaluated every second would have
	// 86,400 points a series.
	if step := replay.ProbeStep(*r); g.Logs == "" && len(g.Data) > 0 && int64(backtestNoData/step) > backtestPoints {
		least := time.Duration(math.Ceil(backtestNoData.Seconds()/backtestPoints)) * time.Second
		return fmt.Sprintf("%s is evaluated every %s, and a query of it that returns nothing is looked through over the day before at that step, %d points a series: here a subquery has %d points at most, a Grafana rule's group an interval of %s or more",
			name, verdict.Duration(r.Interval), int64(backtestNoData/step), backtestPoints, verdict.Duration(least))
	}
	for _, ref := range slices.Sorted(maps.Keys(g.Data)) {
		expr, ok := g.Queries[ref]
		if !ok {
			continue
		}
		query := name + "'s query " + clip(verdict.Clean(ref), 100) + ", looked through a day further back when it returns nothing,"
		if g.Logs != "" {
			if why := checkLogQL(query, expr, g.Shift[ref]+backtestNoData, false); why != "" {
				return why
			}
			continue
		}
		if why := p.checkPromQL(r, query, expr, g.Shift[ref]+backtestNoData, false); why != "" || r.Unsupported != "" {
			return why
		}
	}
	return ""
}

// checkLogQL holds a LogQL query a rule asks, read back before from each
// evaluation, to what the port takes: comments in # alone, and no ' outside
// a string (rules.LogQLText); a metric query, never one of log lines; each
// stream selector naming its streams by a label's value; its ranges with
// their offsets within backtestReach, with no subquery and no @. Each is
// read of the same text, its # comments read past. A refusal says the
// query as name, and, with quote, what of it is past the bounds.
func checkLogQL(name, expr string, before time.Duration, quote bool) string {
	text, ok := rules.LogQLText(expr)
	if !ok {
		return name + " has a // or /* comment, or a ' outside a string: here a rule in LogQL has comments in # alone, and its strings in \" or `"
	}
	if rules.LogQuery(text) {
		return name + " asks Loki for log lines: here a rule in LogQL is a metric query, such as count_over_time"
	}
	if sel, wide := rules.LogQLWide(text); wide {
		if !quote {
			return name + " reads a stream selector that names no stream by a label's value: here each stream selector has a label matched with = to a value"
		}
		return fmt.Sprintf("%s reads %s, which names no stream by a label's value: here each stream selector has a label matched with = to a value", name, clip(verdict.Clean(sel), 200))
	}
	if what := rules.LogQLReach(text, backtestReach-before); what != "" {
		said := name + " reads past what is taken"
		if quote {
			said = name + " reads " + clip(verdict.Clean(what), 300)
		}
		return fmt.Sprintf("%s: here what a rule in LogQL reads, a range and its offset, with its query's relativeTimeRange, reaches %d days back at most, each written as a duration, with no subquery and no @",
			said, int(backtestReach/(24*time.Hour)))
	}
	return ""
}

// inEffectReads is what the rules lapilli serve runs read, by which a pull
// request's Grafana and Loki rules are taken (docs/design/backtests.md,
// open question 2): the Grafana data sources whose queries the replay of a
// rule in effect asks, by uid, as it asks them (promql, of --prometheus;
// logql, through --grafana's data source proxy, or of --loki without it);
// and whether the rules in effect hold a Loki ruler's rule replayed
// against --loki.
type inEffectReads struct {
	promql, logql map[string]bool
	loki          bool
}

// readsInEffect is what the rules in effect read: the version of the rules
// serve runs now, as its runs replay it (parse), each rule it replays. A
// rule it can't replay (paused, unsupported as read, a Loki's with no
// Loki to ask) reads nothing, and a query of a Grafana rule's on a data
// source its condition doesn't read isn't asked.
func (s *Server) readsInEffect(ctx context.Context) (inEffectReads, error) {
	e := inEffectReads{promql: map[string]bool{}, logql: map[string]bool{}}
	v, ok, err := s.l.Latest(ctx)
	if err != nil || !ok {
		return e, err
	}
	rs, err := s.parse(ctx, v)
	if err != nil {
		return e, err
	}
	for _, r := range rs {
		switch {
		case r.Unsupported != "":
		case r.Graph != nil:
			for _, d := range r.Graph.Data {
				if d.UID == r.Graph.Logs {
					e.logql[d.UID] = true
				} else {
					e.promql[d.UID] = true
				}
			}
		case r.Logs != "" && s.cfg.Loki != "":
			e.loki = true
		}
	}
	return e, nil
}

// builtIn are Grafana's data sources of its own, by uid: none is a data
// source's of the rules', and none is asked.
var builtIn = map[string]bool{"-- Mixed --": true, "-- Dashboard --": true, "-- Grafana --": true, "grafana": true}

// dataSourceUID is a Grafana data source's uid as Grafana makes and takes
// one, as source.Logs puts one into its proxy's path.
var dataSourceUID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// taken says why a rule that differs can't be backtested for what it
// reads, "" when it can: a Grafana rule each of whose queries names a data
// source, by its uid exactly, that a rule in effect asks, as that rule
// asks it (PromQL or LogQL), never one of Grafana's own; a Loki ruler's
// rule only where a rule in effect is one, asked of --loki. Nothing of
// serve's says which: a data source refused is one no rule in effect
// reads, whatever the reason.
func taken(r rules.Rule, eff inEffectReads) string {
	name := verdict.Clean(r.Name)
	uids := r.GrafanaUIDs()
	if r.Graph != nil || len(uids) > 0 {
		refused := name + " reads a data source no rule in effect reads: here a Grafana rule reads only the data sources the rules lapilli serve runs read already"
		for _, u := range uids {
			if builtIn[u] || !dataSourceUID.MatchString(u) || !eff.promql[u] && !eff.logql[u] {
				return refused
			}
		}
		if g := r.Graph; g != nil && r.Unsupported == "" {
			for _, d := range g.Data {
				if d.UID == g.Logs && !eff.logql[d.UID] || d.UID != g.Logs && !eff.promql[d.UID] {
					return refused
				}
			}
		}
		return ""
	}
	if r.Logs != "" && !eff.loki {
		return name + " is a Loki ruler's rule: here one is taken only where the rules lapilli serve runs hold a Loki ruler's already"
	}
	return ""
}

// refusedAsWritten is why a rule whose expression doesn't read as PromQL
// isn't replayed: as lapilli verdict says one the history refused.
const refusedAsWritten = rules.QueryFailed + "it doesn't read as PromQL, and is refused as written"

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 }

// period is a backtest's period at now: days, ending at now less
// --settle, on --step.
func (s *Server) backtestPeriod(days int) (time.Time, time.Time) {
	return backtestPeriod(s.now(), days, s.cfg.Settle, s.cfg.Step)
}

func backtestPeriod(now time.Time, days int, settle, step time.Duration) (time.Time, time.Time) {
	to := now.Add(-settle).Truncate(step).UTC()
	return to.Add(-time.Duration(days) * 24 * time.Hour), to
}

// BacktestRequests counts what a backtest asks h for, the rules before a
// pull request (gone) and after it (came) replayed side by side over days
// ending at now less settle, on step, sharing their reads as RunEach
// shares them: what the port counts a backtest by before it is queued
// (fits), and lapilli preflight its estimate.
func BacktestRequests(h replay.History, gone, came []rules.Rule, days int, now time.Time, settle, step time.Duration) int {
	from, to := backtestPeriod(now, days, settle, step)
	return replay.PlanEach(h, [][]replay.Version{{{Rules: gone}}, {{Rules: came}}}, from, to, step).Requests
}

// fits says whether a backtest of p's rules fits in --backtest-timeout at
// the lane's rate, and, when it doesn't, why, with how many requests it
// needs and the days that would fit; and how many requests it needs.
func (s *Server) fits(p *prepared) (int, string) {
	plan := func(days int) int {
		return BacktestRequests(s.btPort.lane, p.gone, p.came, days, s.now(), s.cfg.Settle, s.cfg.Step)
	}
	n := plan(p.days)
	rate := s.btPort.rate
	if rate <= 0 {
		return n, ""
	}
	limit := s.backtestTimeout()
	took := func(n int) time.Duration { return time.Duration(float64(n) / rate * float64(time.Second)) }
	if took(n) <= limit {
		return n, ""
	}
	// What the rate is stays serve's: said as more than fits, not as a
	// number a job could read --max-rate from.
	why := fmt.Sprintf("it needs %d requests of the history, more than the backtests' share of the rate asks within --backtest-timeout %s:", n, verdict.Duration(limit))
	for d := p.days - 1; d >= 1; d-- {
		if took(plan(d)) <= limit {
			return n, fmt.Sprintf("%s %d days would fit", why, d)
		}
	}
	return n, why + " even a day doesn't fit: backtest fewer rules at a time"
}

func (s *Server) backtestTimeout() time.Duration {
	if s.cfg.BacktestTimeout > 0 {
		return s.cfg.BacktestTimeout
	}
	return 15 * time.Minute
}

func (s *Server) backtestQueue() int {
	if s.cfg.BacktestQueue > 0 {
		return s.cfg.BacktestQueue
	}
	return 20
}

// gitRef is a pull request's ref, as GitHub's and Forgejo's tokens say it.
var gitRef = regexp.MustCompile(`^refs/pull/([0-9]{1,10})/(merge|head)$`)

// GitHub's merge queue names a merge group's branch
// refs/heads/gh-readonly-queue/BRANCH/pr-N-SHA, BRANCH the branch it merges
// into, N the pull request the group is made for (the last of it, those
// ahead in the queue beside it) and SHA that pull request's head; a merge
// queue's run's ID token says it as its ref. queueTail is its last part,
// the queue's own, after any / a branch's name holds; queueBranch the
// branch, a name the recipes take as one: letters, digits and . _ / -,
// not first a - or a /, nor .., //, or a / last, 255 at most.
var (
	queueTail   = regexp.MustCompile(`^pr-([1-9][0-9]{0,9})-(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	queueBranch = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,254}$`)
)

// queueRef is the branch and the pull request's number a merge group's
// ref names, ok false for a ref that isn't a merge queue's.
func queueRef(ref string) (branch string, pr int64, ok bool) {
	rest, found := strings.CutPrefix(ref, "refs/heads/gh-readonly-queue/")
	i := strings.LastIndexByte(rest, '/')
	if !found || i < 0 {
		return "", 0, false
	}
	branch, tail := rest[:i], rest[i+1:]
	m := queueTail.FindStringSubmatch(tail)
	if m == nil || !queueBranch.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") {
		return "", 0, false
	}
	pr, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || pr > math.MaxInt32 {
		return "", 0, false
	}
	return branch, pr, true
}

// mergeGroup reports whether the token is a merge queue's run's, GitHub's
// merge_group, as its event_name says, which a GitHub entry pins.
func (a asker) mergeGroup() bool {
	if a.claims == nil || a.t.CI != "github" {
		return false
	}
	ev, _ := signin.ClaimString(a.claims["event_name"])
	return ev == "merge_group"
}

// runNumber is a run's number as GitHub's tokens write it, a decimal.
var runNumber = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// run is, of a GitHub Actions job's token, the workflow its run is of,
// its file (workflow_ref before its @ref, which names the run's ref), and
// the run's number (run_number), which GitHub's contexts reference says
// begins at 1 for a workflow's first run and increments with each new
// run, and doesn't change when a run is run again: what tells an earlier
// run from a later one, as run_id, said only to be unique, doesn't. "", 0
// for any other CI's, whose backtest supersedes by the commit alone, as
// before; and for a GitHub token that doesn't say both plainly, whose
// backtest supersedes none, as its run's place among its workflow's isn't
// known.
func (a asker) run() (string, int64) {
	if a.claims == nil || a.t.CI != "github" {
		return "", 0
	}
	ref, _ := signin.ClaimString(a.claims["workflow_ref"])
	wf, _, cut := strings.Cut(ref, "@refs/")
	num, _ := signin.ClaimString(a.claims["run_number"])
	if !cut || wf == "" || len(wf) > 512 || !signin.Plain(wf) || !runNumber.MatchString(num) {
		return "", 0
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return "", 0
	}
	return wf, n
}

// queue is the branch the merge queue a token's run is of merges into, as
// its ref says it: "" for any run but a merge queue's.
func (a asker) queue() string {
	if !a.mergeGroup() {
		return ""
	}
	ref, _ := signin.ClaimString(a.claims["ref"])
	branch, _, _ := queueRef(ref)
	return branch
}

// repository is the repository a token names, as a name the Run history
// shows: none, "", over 200 bytes or with what doesn't show, as a proxy's
// name is no one's then; a plain token's entry's name.
func (a asker) repository() string {
	if a.claims == nil {
		return a.t.Name
	}
	known, ok := knownCIs[a.t.CI]
	if !ok {
		return a.t.Name
	}
	name, ok := signin.ClaimString(a.claims[known.repoName])
	if !ok || len(name) > 200 || !signin.Plain(name) || ledger.Hidden(name) {
		return ""
	}
	return name
}

// repositoryID is the repository's id as the token says it, the claim its
// entry pins (knownCIs' repoID): what a repository is known by whatever it
// is named, and renamed. "" for a plain token's, and for a CI Lapilli
// doesn't know, whose entries pin what they will. A token passed its
// entry's pin with it, so it is one of the entry's values, 256 bytes at
// most with nothing that doesn't show.
func (a asker) repositoryID() string {
	if a.claims == nil {
		return ""
	}
	known, ok := knownCIs[a.t.CI]
	if !ok {
		return ""
	}
	id, ok := signin.ClaimString(a.claims[known.repoID])
	if !ok || len(id) > 256 {
		return ""
	}
	return id
}

// supersedes reports whether a newer commit's backtest may take the place
// of one waiting, matched by the entry, the repository, the pull
// request's number and, of a merge queue's run, the queue's branch (a
// group the queue built again, never a pull request's own backtest of
// the same number, nor one of another branch's queue): the repository by
// its id, which the token says and
// the entry pins, so that a repository renamed while one waits is still
// itself, and two an entry pins never each other; or, for a backtest kept
// with no id (ledger.Backtest.RepositoryID), by a name kept as it is. A
// name kept as "" (past 200 bytes, or with what doesn't show) is no
// repository's, and two repositories an entry pins so would share it, one's
// pull request 5 superseding the other's.
func (a asker) supersedes() bool { return a.repositoryID() != "" || a.repository() != "" }

// pullRequest is the pull request's number: from GitHub's or Forgejo's
// ref when the token says it (byToken), which the job's must agree with;
// or the job's, which a newer backtest of it doesn't take the place of: a
// job that says another's number can't push that one out of the queue.
// Of a merge queue's run, from the queue's ref alone, never the job's (who
// takes no token of one whose ref isn't the queue's).
func (a asker) pullRequest(given int64) (n int64, byToken, ok bool) {
	ref, _ := signin.ClaimString(a.claims["ref"])
	if a.mergeGroup() {
		_, n, parsed := queueRef(ref)
		if !parsed || given != 0 && given != n {
			return 0, false, false
		}
		return n, true, true
	}
	m := gitRef.FindStringSubmatch(ref)
	if m == nil || (a.t.CI != "github" && a.t.CI != "forgejo") {
		return given, false, true
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n > math.MaxInt32 || given != 0 && given != n {
		return 0, false, false
	}
	return n, true, true
}

// backtestReady says why the port can't take a backtest now, "" when it can: the
// rules read, and a run recorded.
func (s *Server) backtestReady(ctx context.Context) (string, error) {
	if _, ok, err := s.l.Latest(ctx); err != nil || !ok {
		return "the rules in effect haven't been read yet: try again in a minute", err
	}
	runs, err := s.l.Runs(ctx, 1)
	if err != nil || len(runs) == 0 {
		return "no run is recorded yet: try again once lapilli serve has run once", err
	}
	return "", nil
}

// postBacktest queues a backtest: the token first, then the body, the
// rules and their bounds, the time it would take, the entry's hour, and
// the queue's.
func (s *Server) postBacktest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, ok := s.who(ctx, w, r)
	if !ok {
		return
	}
	refused := func(code int, why string) {
		s.backtestEnded(a.t.Name, "refused")
		backtestError(w, code, why)
	}
	// The entry's hour is taken before the body is read, so that past it a
	// body costs nothing to send. What can be told without the body is
	// asked first, each refused cheaply and its hour given back: the
	// ledger not ready, no room in the queue for the entry, the reads
	// full. But for a token that started a backtest already, which keeps
	// it: its job has its backtest, and the same token sent again and
	// again would otherwise cost a signature and the ledger's reads each
	// time, for an hour, past the entry's hourly. Once the body is read,
	// whatever is refused keeps the hour: a request refused after its
	// rules were read and checked costs the port what it cost, and the
	// hour bounds how often.
	if !a.t.hourly.allow(s.now()) {
		w.Header().Set("Retry-After", strconv.Itoa(a.t.hourly.wait()))
		refused(http.StatusTooManyRequests, fmt.Sprintf("this entry starts %d backtests an hour at most", a.t.Hourly))
		return
	}
	refund := true
	defer func() {
		if refund {
			a.t.hourly.give()
		}
	}()
	if why, err := s.backtestReady(ctx); why != "" {
		if err != nil {
			s.log.Error("backtest port: reading the ledger", "err", err)
		}
		w.Header().Set("Retry-After", "60")
		backtestError(w, http.StatusServiceUnavailable, why)
		return
	}
	// What the rules in effect read, which a pull request's Grafana and
	// Loki rules are held to.
	eff, err := s.readsInEffect(ctx)
	if err != nil {
		s.log.Error("backtest port: reading the rules in effect", "err", err)
		w.Header().Set("Retry-After", "60")
		backtestError(w, http.StatusServiceUnavailable, "the rules in effect couldn't be read: try again in a minute")
		return
	}
	// The pull request a newer commit of would supersede the one waiting:
	// the token's own, as the body can't say otherwise, of a repository
	// it names (supersedes); of a merge queue's run, a group of it the
	// same queue built again.
	supersedes := int64(-1)
	if n, byToken, ok := a.pullRequest(0); ok && byToken && a.supersedes() {
		supersedes = n
	}
	used, full, err := s.l.BacktestRoom(ctx, a.t.Name, a.token, a.repositoryID(), a.repository(), a.queue(), supersedes, s.backtestQueue(), backtestsOfEntry, s.now())
	switch {
	case err != nil:
		s.log.Error("backtest port: reading the queue", "trust", a.t.Name, "err", err)
		w.Header().Set("Retry-After", "60")
		backtestError(w, http.StatusServiceUnavailable, "the queue couldn't be read: try again in a minute")
		return
	case used:
		refund = false
		refused(http.StatusConflict, "this token started a backtest already: a job asks once, with a token of its own")
		return
	case full:
		w.Header().Set("Retry-After", "60")
		refused(http.StatusTooManyRequests, "as many backtests wait as the queue takes: try again in a minute")
		return
	}
	// One body of an entry read at a time, and two of all of them: one
	// entry's slow bodies hold its own turn, not every entry's.
	select {
	case a.t.reading <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "1")
		refused(http.StatusTooManyRequests, "another request of this entry is being read: one at a time")
		return
	}
	defer func() { <-a.t.reading }()
	select {
	case s.btPort.reading <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "1")
		refused(http.StatusTooManyRequests, "as many requests are being read as the port reads at once: try again in a second")
		return
	}
	refund = false
	reading := true
	done := func() {
		if reading {
			<-s.btPort.reading
			reading = false
		}
	}
	defer done()
	// The body within backtestBodyWithin of its headers, well within the
	// server's ReadTimeout: a body sent slowly holds a turn no longer.
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(backtestBodyWithin))
	body, req, code, why := readBacktestBody(w, r)
	if code != 0 {
		refused(code, why)
		return
	}
	if bodyHook != nil {
		bodyHook(a)
	}
	pr, byToken, ok := a.pullRequest(req.PullRequest)
	if !ok {
		refused(http.StatusBadRequest, "pull_request isn't the one the token's ref names")
		return
	}
	p, why := s.prepare(req, a.t.Trust, eff)
	if why != "" {
		refused(http.StatusBadRequest, why)
		return
	}
	requests, why := s.fits(p)
	if why != "" {
		refused(http.StatusBadRequest, why)
		return
	}
	done()
	workflow, run := a.run()
	id, key := randomHex(16), randomKey()
	kh, bh := sha256.Sum256([]byte(key)), sha256.Sum256(body)
	ask := ledger.BacktestAsk{Backtest: ledger.Backtest{ID: id, Trust: a.t.Name, Repository: a.repository(), RepositoryID: a.repositoryID(), QueueBranch: a.queue(), Workflow: workflow, RunNumber: run, PullRequest: pr, Commit: req.Commit,
		Rules: max(len(p.gone), len(p.came)), Requests: requests, KeyHash: hex.EncodeToString(kh[:])},
		Body: body, Token: a.token, BodyHash: hex.EncodeToString(bh[:]), Until: a.iat.Add(backtestTokenKept),
		Queue: s.backtestQueue(), PerTrust: backtestsOfEntry, Supersede: byToken && a.supersedes() && (a.t.CI != "github" || workflow != "")}
	b, again, superseded, err := s.l.AskBacktest(ctx, ask, s.now())
	switch {
	case errors.Is(err, ledger.ErrBacktestToken):
		refused(http.StatusConflict, "this token started another backtest: a job asks once, with a token of its own")
		return
	case errors.Is(err, ledger.ErrBacktestsFull):
		w.Header().Set("Retry-After", "60")
		refused(http.StatusTooManyRequests, "as many backtests wait as the queue takes: try again in a minute")
		return
	case err != nil:
		s.log.Error("backtest port: queueing a backtest", "trust", a.t.Name, "err", err)
		backtestError(w, http.StatusServiceUnavailable, "the backtest couldn't be queued: try again in a minute")
		return
	}
	s.btPort.keys.put(b.ID, b.KeyHash)
	for range superseded {
		s.backtestEnded(a.t.Name, ledger.BacktestSuperseded)
	}
	if again {
		// The same job asked twice at once, its first not yet kept when the
		// second was let read: its backtest, without its key, which was
		// given once.
		backtestJSON(w, http.StatusOK, map[string]any{"id": b.ID, "state": b.State, "position": b.Position})
		return
	}
	select {
	case s.btPort.wake <- struct{}{}:
	default:
	}
	s.log.Info("backtest queued", "id", b.ID, "trust", a.t.Name, "rules", ask.Rules, "requests", requests, "position", b.Position)
	backtestJSON(w, http.StatusAccepted, map[string]any{"id": b.ID, "key": key, "position": b.Position, "requests": requests})
}

// bodyHook is called with the asker once a request's body is read: nil
// but in tests, which change the ledger between the checks before it and
// after.
var bodyHook func(asker)

var backtestID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// kept is the backtest a job reads with its key: none, the same 404, for an
// id that isn't and a key that's wrong. The pair is told from what this
// process knows of the backtests' keys (btKeys), in memory and in constant
// time: one it knows is read from the ledger, and charged to nothing but
// its own reads. One it doesn't waits for a listing of the ledger begun
// after it came, as another replica may have queued it, charged to the
// address it comes from while it waits, and is a 404 if the listing
// doesn't know it. Past what that address may wait, it is answered at
// once: a 404 only if a listing begun after it came has ended without it,
// otherwise a 503 with Retry-After, a listing begun all the same, so that
// a job's backtest queued elsewhere, polled from an address others share,
// is known by its next poll, never said not found, which the job takes as
// the end of its backtest.
func (s *Server) kept(w http.ResponseWriter, r *http.Request) (ledger.Backtest, bool) {
	id, key := r.PathValue("id"), bearerOf(r)
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])
	keys := s.btPort.keys
	came := keys.begunNow()
	if !keys.known(id, hash) {
		if !backtestID.MatchString(id) || key == "" {
			backtestError(w, http.StatusNotFound, "no such backtest")
			return ledger.Backtest{}, false
		}
		addr := clientAddr(r)
		if !s.btPort.misses.allow(addr, s.now()) {
			if keys.listedSince(came) && !keys.known(id, hash) {
				backtestError(w, http.StatusNotFound, "no such backtest")
				return ledger.Backtest{}, false
			}
			keys.kick()
			w.Header().Set("Retry-After", "1")
			backtestError(w, http.StatusServiceUnavailable, "too many backtests not known yet asked from here: try again in a second")
			return ledger.Backtest{}, false
		}
		listed, err := keys.await(r.Context())
		switch {
		case err != nil:
			s.log.Error("backtest port: listing the backtests", "err", err)
			backtestError(w, http.StatusServiceUnavailable, "the backtest couldn't be read: try again in a minute")
			return ledger.Backtest{}, false
		case !listed:
			// The ledger slower than the wait: not said not found, which
			// a job takes as the end of its backtest.
			w.Header().Set("Retry-After", "1")
			backtestError(w, http.StatusServiceUnavailable, "the backtests couldn't be listed in time: try again in a second")
			return ledger.Backtest{}, false
		}
		if !keys.known(id, hash) {
			backtestError(w, http.StatusNotFound, "no such backtest")
			return ledger.Backtest{}, false
		}
		s.btPort.misses.give(addr)
	}
	b, found, err := s.l.Backtest(r.Context(), id)
	if err != nil {
		s.log.Error("backtest port: reading a backtest", "err", err)
		backtestError(w, http.StatusServiceUnavailable, "the backtest couldn't be read: try again in a minute")
		return ledger.Backtest{}, false
	}
	if !found {
		// Let go of since it was known: known here no longer.
		keys.drop(id)
		backtestError(w, http.StatusNotFound, "no such backtest")
		return ledger.Backtest{}, false
	}
	if !s.btPort.reads.allow(id, s.now()) {
		w.Header().Set("Retry-After", "1")
		backtestError(w, http.StatusTooManyRequests, "too many reads of this backtest: try again in a second")
		return ledger.Backtest{}, false
	}
	return b, true
}

// clientAddr is the address a request came from, without its port.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// perKeyMost are the most buckets a perKey keeps.
const perKeyMost = 4096

// perKey are rate limits by a key, a bucket each, in this process;
// perKeyMost buckets at most. Past them, only buckets that hold nothing
// back, refilled to their burst, are let go, a second apart at most: one
// with many addresses can't begin everyone's afresh. When none can go, a
// key not kept is refused, as one past its bucket is.
type perKey struct {
	mu          sync.Mutex
	rate, burst float64
	m           map[string]*limiter
	// swept is when the full buckets were last let go.
	swept time.Time
}

// sweep lets go, at now, of the buckets refilled to their burst; with
// p.mu held.
func (p *perKey) sweep(now time.Time) {
	p.swept = now
	for k, l := range p.m {
		l.mu.Lock()
		full := !l.last.IsZero() && l.left+now.Sub(l.last).Seconds()*l.rate >= l.burst
		l.mu.Unlock()
		if full {
			delete(p.m, k)
		}
	}
}

// give gives back what allow took from key's bucket.
func (p *perKey) give(key string) {
	p.mu.Lock()
	l := p.m[key]
	p.mu.Unlock()
	if l != nil {
		l.give()
	}
}

func (p *perKey) allow(key string, now time.Time) bool {
	p.mu.Lock()
	if p.m == nil {
		p.m = map[string]*limiter{}
	}
	l := p.m[key]
	if l == nil {
		if len(p.m) >= perKeyMost && now.Sub(p.swept) >= time.Second {
			p.sweep(now)
		}
		if len(p.m) >= perKeyMost {
			// Every bucket holds something back: closed, not begun afresh.
			p.mu.Unlock()
			return false
		}
		l = &limiter{rate: p.rate, burst: p.burst}
		p.m[key] = l
	}
	p.mu.Unlock()
	return l.allow(now)
}

// getBacktest says where a backtest is, and once it ended, what it came
// to.
func (s *Server) getBacktest(w http.ResponseWriter, r *http.Request) {
	b, ok := s.kept(w, r)
	if !ok {
		return
	}
	switch b.State {
	case ledger.BacktestWaiting:
		backtestJSON(w, http.StatusAccepted, map[string]any{"state": b.State, "position": b.Position})
	case ledger.BacktestRunning:
		backtestJSON(w, http.StatusAccepted, map[string]any{"state": b.State, "requests": b.Requests, "asked": b.Made})
	case ledger.BacktestDone:
		res, err := s.l.BacktestResult(r.Context(), b.ID)
		if err != nil || res == nil {
			if err != nil {
				s.log.Error("backtest port: reading a backtest's result", "err", err)
			}
			backtestJSON(w, http.StatusOK, map[string]any{"state": ledger.BacktestFailed, "error": "its result couldn't be read"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(res)
	case ledger.BacktestFailed:
		backtestJSON(w, http.StatusOK, map[string]any{"state": b.State, "error": b.Error})
	default:
		backtestJSON(w, http.StatusOK, map[string]any{"state": b.State})
	}
}

// deleteBacktest cancels a backtest: one waiting stops waiting, one
// running stops at its next look.
func (s *Server) deleteBacktest(w http.ResponseWriter, r *http.Request) {
	b, ok := s.kept(w, r)
	if !ok {
		return
	}
	done, err := s.l.CancelBacktest(r.Context(), b.ID, s.now())
	if err != nil {
		s.log.Error("backtest port: cancelling a backtest", "err", err)
		backtestError(w, http.StatusServiceUnavailable, "the backtest couldn't be cancelled: try again in a minute")
		return
	}
	if done && b.State == ledger.BacktestWaiting {
		s.backtestEnded(b.Trust, ledger.BacktestCancelled)
	}
	w.WriteHeader(http.StatusNoContent)
}

// backtestEnded counts a backtest ended, or refused, for /metrics.
func (s *Server) backtestEnded(trust, how string) {
	p := s.btPort
	p.mu.Lock()
	p.ended[[2]string{trust, how}]++
	p.mu.Unlock()
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// randomKey is a backtest's key: 32 random bytes.
func randomKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// backtestLog is a backtest's one line in the log, with counts.
func (s *Server) backtestLog(b ledger.Backtest, state string, made int, took time.Duration) {
	s.log.Info("backtest ended", slog.String("id", b.ID), slog.String("trust", b.Trust), slog.String("state", state),
		slog.Int("rules", b.Rules), slog.Int("requests", made), slog.Duration("took", took.Round(time.Second)))
}
