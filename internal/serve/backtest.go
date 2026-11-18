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
	"math"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// backtestsOfEntry are how many of an entry's may wait at once.
	backtestsOfEntry = 3
	// backtestFiles are the most files of a side, backtestPath the longest
	// path, backtestZone the longest zone's name.
	backtestFiles, backtestPath, backtestZone = 2000, 512, 64
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
	// asked counts what the lane asked of the history; progress, the rules
	// it replayed.
	asked    prom.Counts
	progress replay.Progress
	// reads is how fast the job's reads (GET, DELETE) are taken, in this
	// process.
	reads *limiter
	// wake is poked when a backtest is queued here, so the leader, if it
	// is this process, takes it at once.
	wake chan struct{}

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
}

// newBacktestPort readies the port: each issuer asked once for its keys,
// by every entry that names it; each entry's hour, shared among the
// replicas; and the lane's history.
func (s *Server) newBacktestPort() *backtestPort {
	p := &backtestPort{reads: &limiter{rate: 20, burst: 100}, wake: make(chan struct{}, 1), ended: map[[2]string]int{}, said: map[string]time.Time{}}
	cis := map[[2]string]*signin.CI{}
	for _, t := range s.cfg.Trust {
		tr := &trusted{Trust: t, hourly: &limiter{rate: float64(t.Hourly) / 3600, burst: float64(t.Hourly)}}
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
	base := replay.Base(s.h)
	if c, ok := base.(*prom.Client); ok {
		runs := c.Limiter()
		own := runs.Pace().Share(s.share())
		p.rate = own.Rate()
		p.lane = c.Lane(prom.NewLimiterPaced(own).AlsoPaced(runs.Pace()), backtestQueryTimeout)
	} else {
		p.lane = base
	}
	return p
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
// a plain token, held to each plain entry's file, read again, both hashed
// and compared in constant time; or an ID token, whose iss alone is read
// before it is verified, to pick the entries of its issuer, and is then
// checked by the issuer's keys, for each entry's audience, its pins held.
// An iss no entry names asks nothing of anyone.
func (s *Server) who(ctx context.Context, w http.ResponseWriter, r *http.Request) (asker, bool) {
	raw := bearerOf(r)
	if raw == "" || len(raw) > 8<<10 {
		s.refuse(w, "", "no bearer token, or one past 8 KiB")
		return asker{}, false
	}
	x := sha256.Sum256([]byte(raw))
	for _, t := range s.btPort.trust {
		if t.TokenFile == "" {
			continue
		}
		b, err := os.ReadFile(t.TokenFile)
		want := strings.TrimSpace(string(b))
		if err != nil || len(want) < minBacktestToken || s.backtestTokenShared(want, t) {
			s.noteRefused(t.Name, "its token_file can't be read, is shorter than 32 characters, or holds another token serve takes: none is taken with it until it is put right")
			continue
		}
		y := sha256.Sum256([]byte(want))
		if subtle.ConstantTimeCompare(x[:], y[:]) == 1 {
			return asker{t: t}, true
		}
	}
	iss, ok := issuerOf(raw)
	if !ok {
		s.refuse(w, "", "not a token any entry takes")
		return asker{}, false
	}
	why, of := "no entry names its issuer", ""
	for _, t := range s.btPort.trust {
		if t.ci == nil || t.Issuer != iss {
			continue
		}
		of = t.Name
		tok, err := t.ci.Verify(ctx, raw, t.Audience)
		if err != nil {
			why = refusedWhy(err)
			continue
		}
		if claim, ok := t.pinned(tok.Claims); !ok {
			why = "its " + claim + " claim isn't as the entry pins it"
			continue
		}
		key := "raw:" + hex.EncodeToString(x[:])
		if jti, ok := signin.ClaimString(tok.Claims["jti"]); ok && jti != "" && len(jti) <= 256 {
			j := sha256.Sum256([]byte(jti))
			key = "jti:" + hex.EncodeToString(j[:])
		}
		return asker{t: t, token: key, iat: tok.IssuedAt, claims: tok.Claims}, true
	}
	s.refuse(w, of, why)
	return asker{}, false
}

// backtestTokenShared reports whether a plain token is one another token
// file serve takes holds too: none is taken until each has its own.
func (s *Server) backtestTokenShared(tk string, of *trusted) bool {
	files := []string{s.cfg.IncidentsToken, s.cfg.FiringsToken, s.cfg.NotifyToken, s.cfg.TasksToken, s.cfg.ChangesToken}
	for _, t := range s.btPort.trust {
		if t != of && t.TokenFile != "" {
			files = append(files, t.TokenFile)
		}
	}
	for _, f := range files {
		if f == "" {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && strings.TrimSpace(string(b)) == tk {
			return true
		}
	}
	return false
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
	Expect       string         `json:"expect"`
	ExpectBefore string         `json:"expect_before"`
	RulesSelect  string         `json:"rules_select"`
	Days         int            `json:"days"`
	TZ           string         `json:"tz"`
	PullRequest  int64          `json:"pull_request"`
	Commit       string         `json:"commit"`
}

type backtestFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
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
}

// prepare reads and checks a backtest's request, as the port does before
// it is queued and the lane does again before it is run: 400's reason
// when it can't be, in Lapilli's words and the request's own.
func (s *Server) prepare(req backtestRequest, t Trust) (*prepared, string) {
	p := &prepared{days: req.Days, loc: s.cfg.Loc, passedOver: map[string]string{}, reasons: map[string]bool{}}
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
	case req.ExpectBefore != "" && req.Expect == "":
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
	read := func(side string, files []backtestFile, rec *[]rules.Recording) ([]rules.Rule, string) {
		if len(files) > backtestFiles {
			return nil, fmt.Sprintf("%s: %d files at most", side, backtestFiles)
		}
		in := make([]rules.File, 0, len(files))
		seen := map[string]bool{}
		for _, f := range files {
			if f.Path == "" || len(f.Path) > backtestPath || !utf8.ValidString(f.Path) || strings.ContainsFunc(f.Path, isControl) || seen[f.Path] {
				return nil, fmt.Sprintf("%s: each file has a path of its own, %d bytes at most, with nothing that doesn't show", side, backtestPath)
			}
			seen[f.Path] = true
			in = append(in, rules.File{Path: f.Path, Data: []byte(f.Content)})
		}
		rs, _, picked, err := rules.PickedFiles(in, rules.Picking{Select: sel, Trim: true, Recordings: rec})
		if err != nil && !errors.Is(err, rules.ErrNoneSelected) {
			return nil, fmt.Sprintf("the rules %s: %s", side, clip(verdict.Clean(err.Error()), 600))
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
	for _, rs := range [][]rules.Rule{before, after} {
		for _, r := range rs {
			switch {
			case r.Graph != nil:
				return nil, fmt.Sprintf("%s is a Grafana rule: this port takes rules in Prometheus's format", verdict.Clean(r.Name))
			case r.Logs != "":
				return nil, fmt.Sprintf("%s is a Loki rule: this port takes rules in Prometheus's format", verdict.Clean(r.Name))
			}
		}
	}
	source.TellApart(before, after)
	p.same, p.gone, p.came = source.Split(before, after)
	if len(p.gone) > backtestRules || len(p.came) > backtestRules {
		return nil, fmt.Sprintf("%d rules that differ at most, on either side: backtest a change a few rules at a time", backtestRules)
	}
	// Replayed as the rules in effect are: --late, --scrape-interval.
	s.replayedAs().Apply(p.gone)
	s.replayedAs().Apply(p.came)
	for _, r := range slices.Concat(p.gone, p.came) {
		name := verdict.Clean(r.Name)
		switch {
		case r.For < 0 || r.KeepFiringFor < 0 || r.Interval < 0:
			return nil, name + ": a duration is negative"
		case r.For > compareFor || r.KeepFiringFor > compareFor:
			return nil, fmt.Sprintf("%s: for and keep_firing_for are %s at most here", name, verdict.Duration(compareFor))
		case r.Interval > compareEvery:
			return nil, fmt.Sprintf("%s: its group's interval is %s at most here", name, verdict.Duration(compareEvery))
		case r.Unsupported != "":
			// Asks nothing (a query that doesn't parse): said as read, in
			// the request's own words.
			p.reasons[r.Unsupported] = true
			continue
		}
		if sel, wide := rules.Wide(r.Expr); wide {
			return nil, fmt.Sprintf("%s reads %s, which names no metric, or names it by a pattern: here each selector reads one metric, by its name", name, clip(verdict.Clean(sel), 200))
		}
	}
	p.recording = verdict.RecordingChanges(recBefore, recAfter)
	for _, e := range []struct {
		side, text string
		into       **verdict.Expectations
	}{{"expect", req.Expect, &p.expect}, {"expect_before", req.ExpectBefore, &p.before}} {
		if e.text == "" {
			continue
		}
		x, err := verdict.ReadExpectations(strings.NewReader(e.text))
		if err != nil {
			return nil, e.side + ": " + clip(verdict.Clean(err.Error()), 600)
		}
		*e.into = x
	}
	return p, ""
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 }

// period is a backtest's period at now: days, ending at now less
// --settle, on --step.
func (s *Server) backtestPeriod(days int) (time.Time, time.Time) {
	to := s.now().Add(-s.cfg.Settle).Truncate(s.cfg.Step).UTC()
	return to.Add(-time.Duration(days) * 24 * time.Hour), to
}

// fits says whether a backtest of p's rules fits in --backtest-timeout at
// the lane's rate, and, when it doesn't, why, with how many requests it
// needs and the days that would fit; and how many requests it needs.
func (s *Server) fits(p *prepared) (int, string) {
	sets := [][]replay.Version{{{Rules: p.gone}}, {{Rules: p.came}}}
	plan := func(days int) int {
		from, to := s.backtestPeriod(days)
		return replay.PlanEach(s.btPort.lane, sets, from, to, s.cfg.Step).Requests
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
	why := fmt.Sprintf("it needs %d requests of the history, which at the lane's %g a second take %s, past --backtest-timeout %s:", n, rate,
		verdict.Duration(took(n).Round(time.Minute)), verdict.Duration(limit))
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

// pullRequest is the pull request's number: from GitHub's or Forgejo's
// ref when the token says it, which the job's must agree with; or the
// job's.
func (a asker) pullRequest(given int64) (int64, bool) {
	ref, _ := signin.ClaimString(a.claims["ref"])
	m := gitRef.FindStringSubmatch(ref)
	if m == nil || (a.t.CI != "github" && a.t.CI != "forgejo") {
		return given, true
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n > math.MaxInt32 || given != 0 && given != n {
		return 0, false
	}
	return n, true
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
	body, req, code, why := readBacktestBody(w, r)
	if code != 0 {
		refused(code, why)
		return
	}
	if why, err := s.backtestReady(ctx); why != "" {
		if err != nil {
			s.log.Error("backtest port: reading the ledger", "err", err)
		}
		w.Header().Set("Retry-After", "60")
		backtestError(w, http.StatusServiceUnavailable, why)
		return
	}
	pr, ok := a.pullRequest(req.PullRequest)
	if !ok {
		refused(http.StatusBadRequest, "pull_request isn't the one the token's ref names")
		return
	}
	p, why := s.prepare(req, a.t.Trust)
	if why != "" {
		refused(http.StatusBadRequest, why)
		return
	}
	requests, why := s.fits(p)
	if why != "" {
		refused(http.StatusBadRequest, why)
		return
	}
	if !a.t.hourly.allow(s.now()) {
		w.Header().Set("Retry-After", strconv.Itoa(a.t.hourly.wait()))
		refused(http.StatusTooManyRequests, fmt.Sprintf("this entry starts %d backtests an hour at most", a.t.Hourly))
		return
	}
	id, key := randomHex(16), randomKey()
	kh, bh := sha256.Sum256([]byte(key)), sha256.Sum256(body)
	ask := ledger.BacktestAsk{Backtest: ledger.Backtest{ID: id, Trust: a.t.Name, Repository: a.repository(), PullRequest: pr, Commit: req.Commit,
		Rules: max(len(p.gone), len(p.came)), Requests: requests, KeyHash: hex.EncodeToString(kh[:])},
		Body: body, Token: a.token, BodyHash: hex.EncodeToString(bh[:]), Until: a.iat.Add(backtestTokenKept),
		Queue: s.backtestQueue(), PerTrust: backtestsOfEntry}
	b, again, superseded, err := s.l.AskBacktest(ctx, ask, s.now())
	switch {
	case errors.Is(err, ledger.ErrBacktestToken):
		a.t.hourly.give()
		refused(http.StatusConflict, "this token started another backtest: a job asks once, with a token of its own")
		return
	case errors.Is(err, ledger.ErrBacktestsFull):
		a.t.hourly.give()
		w.Header().Set("Retry-After", "60")
		refused(http.StatusTooManyRequests, "as many backtests wait as the queue takes: try again in a minute")
		return
	case err != nil:
		a.t.hourly.give()
		s.log.Error("backtest port: queueing a backtest", "trust", a.t.Name, "err", err)
		backtestError(w, http.StatusServiceUnavailable, "the backtest couldn't be queued: try again in a minute")
		return
	}
	for range superseded {
		s.backtestEnded(a.t.Name, ledger.BacktestSuperseded)
	}
	if again {
		// The same job asked twice: its backtest, without its key, which
		// was given once.
		a.t.hourly.give()
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

var backtestID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// kept is the backtest a job reads with its key: none, the same 404, for an
// id that isn't and a key that's wrong, compared in constant time.
func (s *Server) kept(w http.ResponseWriter, r *http.Request) (ledger.Backtest, bool) {
	if !s.btPort.reads.allow(s.now()) {
		w.Header().Set("Retry-After", "1")
		backtestError(w, http.StatusTooManyRequests, "too many reads: try again in a second")
		return ledger.Backtest{}, false
	}
	id, key := r.PathValue("id"), bearerOf(r)
	var b ledger.Backtest
	var found bool
	if backtestID.MatchString(id) {
		var err error
		if b, found, err = s.l.Backtest(r.Context(), id); err != nil {
			s.log.Error("backtest port: reading a backtest", "err", err)
			backtestError(w, http.StatusServiceUnavailable, "the backtest couldn't be read: try again in a minute")
			return ledger.Backtest{}, false
		}
	}
	got := sha256.Sum256([]byte(key))
	want := b.KeyHash
	if !found {
		want = strings.Repeat("0", 64)
	}
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(got[:])), []byte(want)) != 1 || !found || key == "" {
		backtestError(w, http.StatusNotFound, "no such backtest")
		return ledger.Backtest{}, false
	}
	return b, true
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
