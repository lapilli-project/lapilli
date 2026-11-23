package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
)

// Sources as objects (docs/design/serveops.md, "Sources"): every source
// serve reads, each as Argo CD shows an application: what it is, when it
// was last read well, its last error, the rules it gave, how far behind it
// is, and a state in the status colours; one notice when it goes down,
// and one when it is back.

// sourceRead is how reading a source went, as the replica that reads it
// (the leader) saw: when it last read well (OK); when a read last failed
// (Failed) and why (Err); how many reads in a row failed since (InARow);
// the rules the last read gave (Rules, -1 for a source of no rules); how
// long the last read took; and Held, that its last turn to be read was
// passed over, the time for it spent by another (noteHeld).
type sourceRead struct {
	OK     time.Time     `json:"ok,omitzero"`
	Failed time.Time     `json:"failed,omitzero"`
	Err    string        `json:"err,omitempty"`
	InARow int           `json:"in_a_row,omitempty"`
	Rules  int           `json:"rules"`
	Took   time.Duration `json:"took,omitempty"`
	Held   bool          `json:"held,omitempty"`
}

// readsState is what this replica saw of the sources it reads itself,
// by id.
type readsState struct {
	sync.Mutex
	m map[string]sourceRead
	// failing is when each source whose reads aren't counted first failed
	// to read, since it last read well, by this replica (noteFailing).
	failing map[string]time.Time
	// led says this replica led, and term is the term it led last
	// (takeReads).
	led  bool
	term int64
}

// noteFailing keeps when the source id, one whose reads aren't counted
// (the incidents file, an impact signal, an on-call schedule), first failed
// to read since it last read well: err is how its read went. It is noted
// where the source is read, never where the sources are shown, so that
// looking at them writes nothing.
func (s *Server) noteFailing(id string, err error) {
	s.reads.Lock()
	defer s.reads.Unlock()
	if err == nil {
		delete(s.reads.failing, id)
		return
	}
	if _, ok := s.reads.failing[id]; ok {
		return
	}
	if s.reads.failing == nil {
		s.reads.failing = map[string]time.Time{}
	}
	s.reads.failing[id] = s.now()
}

// takeReads sets what this replica holds of the sources' reads as it
// starts to lead term (beginTerm): the reads of the term before, whoever led
// it. Where this replica led it, its own are kept. Where another did, and
// this replica read what it said (prev, as the ledger's Led gives it), the
// reads it said are taken, as it last said them: a source it read well
// all along is read well as late here, and one failing is failing since
// when it said, so that a leader that changes more often than a source's
// bound doesn't start the source's clock again each time. Else, none: what
// this replica read in a term it led earlier, kept while others led,
// would have a source read well all along quiet or down at once, before
// this term read it. Each source is then pending until it is read, and
// one never read well failing from its first failed read in this term.
// The incidents file, the impact signals, the on-call schedules and the
// sources counted read by read alike.
func (s *Server) takeReads(term int64, prev ledger.Leader, ok bool) {
	s.reads.Lock()
	own := s.reads.led && s.reads.term == term-1
	s.reads.led, s.reads.term = true, term
	s.reads.Unlock()
	if own {
		return
	}
	var said sharedStatus
	if ok && !prev.Self && prev.Term == term-1 && json.Unmarshal(prev.Status, &said) != nil {
		said = sharedStatus{}
	}
	s.reads.Lock()
	s.reads.m, s.reads.failing = maps.Clone(said.Reads), maps.Clone(said.Failing)
	s.reads.Unlock()
	seen := sourcesSeen{}
	if said.Sources != nil {
		seen = *said.Sources
	}
	s.sources.Lock()
	s.sources.impactRead, s.sources.impactKey, s.sources.impactFailed, s.sources.impactErr = seen.ImpactRead, seen.ImpactKey, seen.ImpactFailed, seen.ImpactErr
	s.sources.fileRead, s.sources.fileFailed, s.sources.fileErr = seen.FileRead, seen.FileFailed, seen.FileErr
	s.sources.signals = maps.Clone(seen.Signals)
	s.sources.Unlock()
	s.takeOnCallReads(said.OnCall)
}

// failingNow is when each source this replica reads first failed, as it
// noted (noteFailing).
func (s *Server) failingNow() map[string]time.Time {
	s.reads.Lock()
	defer s.reads.Unlock()
	if len(s.reads.failing) == 0 {
		return nil
	}
	return maps.Clone(s.reads.failing)
}

// shownFailing is failingNow as the pages judge by it: this replica's
// while it leads, else the leader's, as it last said.
func (s *Server) shownFailing() map[string]time.Time {
	if !s.follows() {
		return s.failingNow()
	}
	return s.shownStatus().Failing
}

// noteRead keeps how a read of the source id went: began when it began,
// rules the rules it gave (-1 for none to count).
func (s *Server) noteRead(id string, began time.Time, rules int, err error) {
	s.reads.Lock()
	defer s.reads.Unlock()
	if s.reads.m == nil {
		s.reads.m = map[string]sourceRead{}
	}
	x := s.reads.m[id]
	x.Took, x.Held = time.Since(began), false
	if err != nil {
		x.Failed, x.Err = s.now(), err.Error()
		x.InARow++
	} else {
		x.OK, x.Err, x.InARow, x.Rules = s.now(), "", 0, rules
	}
	s.reads.m[id] = x
}

// noteHeld keeps that the source id wasn't read in its turn, the time
// for it spent by another source: until it is read, it is waiting, not
// quiet, however long since it last read well, and isn't down for it.
func (s *Server) noteHeld(id string) {
	s.reads.Lock()
	defer s.reads.Unlock()
	if s.reads.m == nil {
		s.reads.m = map[string]sourceRead{}
	}
	x := s.reads.m[id]
	x.Held = true
	s.reads.m[id] = x
}

// readsNow is this replica's sourceReads.
func (s *Server) readsNow() map[string]sourceRead {
	s.reads.Lock()
	defer s.reads.Unlock()
	if len(s.reads.m) == 0 {
		return nil
	}
	return maps.Clone(s.reads.m)
}

// shownReads are the reads the pages show: this replica's while it leads,
// else the leader's, as it last said.
func (s *Server) shownReads() map[string]sourceRead {
	if !s.follows() {
		return s.readsNow()
	}
	return s.shownStatus().Reads
}

// The bounds a source is held to.
const (
	// sourceDownAfter is how many reads in a row fail before a source
	// read on its own clock (the rules, a cloud's alarms, a file each run)
	// is down.
	sourceDownAfter = 3
	// sourceBackAfter is how long a source that was down reads well
	// before it is back: a source that fails three reads, reads once, and
	// fails again stays down, and is told of once.
	sourceBackAfter = 15 * time.Minute
	// sourceRenotify is how long after a source is told back that it is
	// told down again, at the soonest: one that goes down, back and down
	// again is told of once an hour at most, and is shown down meanwhile.
	sourceRenotify = time.Hour
	// intakeForgotten is how long an incidents id prefix nothing was
	// written to since is listed (--keep when shorter): past it, it was no
	// job's that stopped but an id's one-off prefix, and is left out, not
	// told down.
	intakeForgotten = 14 * 24 * time.Hour
)

// sourceItem is a source as the Sources page and the API show it.
type sourceItem struct {
	// ID is the source's own, kept from one release to the next: rules,
	// prometheus, alarms/SOURCE, loki, incidents-file, deploys-file,
	// marked-file, impact, signal/NAME, on-call/NAME, intake/PREFIX,
	// firings/NAME.
	ID string `json:"id"`
	// Kind is what it gives: rules, history (what Prometheus answers the
	// runs), incidents, deploys, marked, impact, on-call, firings.
	Kind string `json:"kind"`
	// Name is what the page calls it: a text to translate, or, with Raw,
	// the source's own name (a cloud's alarms', a signal's, a schedule's).
	Name string `json:"name"`
	Raw  bool   `json:"-"`
	// From is where it is read: a flag, as the page says it.
	From string `json:"from,omitempty"`
	// State is ok, failing, quiet or pending (statusKinds' source/…);
	// Down, whether it is down by the bounds above, as its notices say.
	State string `json:"state"`
	Down  bool   `json:"down"`
	// LastOK is when it was last read, or written to, well; LastFailed,
	// when a read last failed, and LastError why, as its reader may be told
	// it; Failures, the reads in a row that failed (-1 when not counted).
	LastOK     time.Time `json:"last_ok,omitzero"`
	LastFailed time.Time `json:"last_failed,omitzero"`
	LastError  string    `json:"last_error,omitempty"`
	Failures   int       `json:"failures"`
	// Rules is how many rules it gave, nil for a source that gives none.
	Rules *int `json:"rules,omitempty"`
	// Lag is how far behind now what it gave reaches, where that is known
	// (the runs' cursor, a source's furthest period copied in); Took, how
	// long its last read took. The API says each in seconds.
	Lag         *time.Duration `json:"-"`
	Took        *time.Duration `json:"-"`
	LagSeconds  *float64       `json:"lag_seconds,omitempty"`
	TookSeconds *float64       `json:"took_seconds,omitempty"`
	// Where is, for an admin alone, the path or address it is read from.
	Where string `json:"where,omitempty"`
	// counted says Failures is counted; held, that the runs wait (--guard,
	// a 429 yielded to), so that Prometheus not read meanwhile isn't quiet,
	// or that a source read beside the rules was passed over (noteHeld).
	counted, held bool
}

// sourceStale is how long a source read on the runs' clock may go without
// a read before it is quiet, and failing before it is down: two runs, the
// hours they settle, and one more, as the impact signal's (impactStale).
func (s *Server) sourceStale() time.Duration { return impactStale(s.cfg) }

// sourceList is every source serve reads, as now: the rules and what
// reads beside them, Prometheus as the runs ask it, the incidents file,
// the deploys and marked files, the impact signals, the on-call schedules,
// and what jobs copy in. Errors are as they were read, for the caller to
// say to its reader (shownError); Where is filled for everyone, for the
// caller to keep to an admin.
func (s *Server) sourceList(ctx context.Context) ([]sourceItem, error) {
	now := s.now()
	reads := s.shownReads()
	var out []sourceItem
	read := func(id, kind, name, from, where string, rules bool) *sourceItem {
		x, ok := reads[id]
		it := sourceItem{ID: id, Kind: kind, Name: name, From: from, Where: where, counted: true, Failures: x.InARow, held: x.Held}
		if ok {
			it.LastOK, it.LastFailed, it.LastError = x.OK, x.Failed, x.Err
			if rules && !x.OK.IsZero() {
				n := x.Rules
				it.Rules = &n
			}
			if x.Took > 0 {
				d := x.Took
				it.Took = &d
			}
		}
		out = append(out, it)
		return &out[len(out)-1]
	}

	// The rules, where they are kept, and what is read beside them.
	if s.replaying() {
		switch {
		case s.cfg.Loaded:
			read("rules", "rules", "Rules loaded in Prometheus", "--loaded-rules", "", true)
		case s.cfg.Grafana != "":
			read("rules", "rules", "Grafana's alert rules", "--grafana", s.cfg.Grafana, true)
		case s.ruler != nil:
			read("rules", "rules", "A ruler's rules", "--ruler", s.cfg.Ruler, true)
		case s.following():
			read("rules", "rules", "The rules' history", "--rules-history", s.cfg.RulesHistory, true)
		case s.cfg.RulesPath != "":
			read("rules", "rules", "Rule files", "--rules", s.cfg.RulesPath, true)
		}
		for _, a := range s.alarms {
			read("alarms/"+a.Source, "rules", a.Source, "--alarms", a.URL, true).Raw = true
		}
		switch {
		case s.cfg.LokiLoaded:
			read("loki", "rules", "Loki's ruler", "--loki-loaded-rules", s.cfg.Loki, true)
		case s.cfg.LokiRules != "":
			read("loki", "rules", "Loki rule files", "--loki-rules", s.cfg.LokiRules, true)
		}
	}

	// Prometheus, as the runs ask it: each run is a read, the latest
	// first.
	if s.running(ctx) {
		it, err := s.historySource(ctx, now)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}

	// What incidents come from: the file, and the impact signals.
	seen := s.shownSources()
	if s.cfg.Incidents != "" {
		out = append(out, sourceItem{ID: "incidents-file", Kind: "incidents", Name: "Incidents file", From: "--incidents", Where: s.cfg.Incidents,
			LastOK: seen.FileRead, LastFailed: seen.FileFailed, LastError: seen.FileErr, Failures: -1})
	}
	if s.cfg.Deploys != "" {
		read("deploys-file", "deploys", "Deploys file", "--deploys", s.cfg.Deploys, false)
	}
	if s.cfg.Marked != "" {
		read("marked-file", "marked", "Marked file", "--marked", s.cfg.Marked, false)
	}
	n, set, err := s.signalsNowOf(ctx)
	if err != nil {
		return nil, err
	}
	if n.named() {
		for _, d := range n.Defs {
			got := seen.Signals[d.Name]
			out = append(out, sourceItem{ID: "signal/" + d.Name, Kind: "impact", Name: d.Name, Raw: true,
				LastOK: got.Read, LastFailed: got.Failed, LastError: got.Err, Failures: -1})
		}
	} else if c := s.criterionOf(set); c.Expr != "" {
		out = append(out, sourceItem{ID: "impact", Kind: "impact", Name: "Impact signal",
			LastOK: seen.ImpactRead, LastFailed: seen.ImpactFailed, LastError: seen.ImpactErr, Failures: -1})
	}
	for _, oc := range s.onCallShown(s.shownStatus()) {
		out = append(out, sourceItem{ID: "on-call/" + oc.Name, Kind: "on-call", Name: oc.Name, Raw: true, From: "--on-call",
			LastOK: oc.OK, LastFailed: oc.At, LastError: oc.Err, Failures: -1})
	}

	// What jobs copy in: incidents through the incidents port, by their
	// ids' prefix, and firings, by source.
	if s.cfg.IncidentsListen != "" && s.cfg.IncidentsToken != "" {
		received, err := s.l.Received(ctx)
		if err != nil {
			return nil, err
		}
		forgotten := intakeForgotten
		if s.cfg.Keep > 0 {
			forgotten = min(forgotten, s.cfg.Keep)
		}
		for _, name := range slices.Sorted(maps.Keys(received)) {
			if now.Sub(received[name]) > forgotten {
				continue
			}
			it := sourceItem{ID: "intake/" + name, Kind: "incidents", Name: name + "-", Raw: true, From: "--incidents-token-file", LastOK: received[name], Failures: -1}
			switch name {
			case "":
				it.Name, it.Raw = "Ids with no prefix", false
			case ledger.OtherSources:
				it.Name, it.Raw = "Other sources, past the most kept", false
			}
			out = append(out, it)
		}
	}
	if s.cfg.FiringsToken != "" {
		srcs, err := s.l.Sources(ctx)
		if err != nil {
			return nil, err
		}
		for _, src := range srcs {
			r := src.Rules
			it := sourceItem{ID: "firings/" + src.Name, Kind: "firings", Name: src.Name, Raw: true, From: "--firings-token-file", LastOK: src.Written, Failures: -1, Rules: &r}
			if !src.To.IsZero() {
				lag := now.Sub(src.To).Round(time.Second)
				it.Lag = &lag
			}
			out = append(out, it)
		}
	}
	failing := s.shownFailing()
	for i := range out {
		s.judgeSource(&out[i], now, failing)
	}
	return out, nil
}

// historySource is Prometheus as the runs ask it: each run that isn't
// held a read, its error the run's, behind by where the firings are in up
// to.
func (s *Server) historySource(ctx context.Context, now time.Time) (sourceItem, error) {
	it := sourceItem{ID: "prometheus", Kind: "history", Name: "Prometheus, as the runs ask it", From: "--prometheus", counted: true}
	runs, err := s.l.Runs(ctx, 100)
	if err != nil {
		return it, err
	}
	inARow := true
	for i, r := range runs {
		if i == 0 && r.Kind == "held" {
			it.held = true
		}
		if r.Kind != "hourly" && r.Kind != "backfill" {
			continue // held, a review: not a read of the runs' own
		}
		if r.Error == "" {
			if it.LastOK.IsZero() {
				it.LastOK = r.Finished
				if took := r.Finished.Sub(r.Started); took >= 0 {
					it.Took = &took
				}
			}
			inARow = false
		} else {
			if it.LastFailed.IsZero() {
				it.LastFailed, it.LastError = r.Started, r.Error
			}
			if inARow {
				it.Failures++
			}
		}
		if !it.LastOK.IsZero() && !it.LastFailed.IsZero() {
			break
		}
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		return it, err
	}
	if !cursor.IsZero() {
		lag := now.Sub(cursor).Round(time.Second)
		it.Lag = &lag
	}
	return it, nil
}

// judgeSource sets a source's state, and whether it is down: failing, its
// last read failed; quiet, nothing read or written past its bound;
// pending, not read yet; ok. A source whose reads are counted is down once
// sourceDownAfter in a row failed; another, once it has failed, or been
// quiet, past its bound. failing is when each source that never read well
// first failed, as the replica that reads it noted (shownFailing). It
// reads what was noted, and notes nothing: a page judges as often as it is
// looked at.
func (s *Server) judgeSource(it *sourceItem, now time.Time, failing map[string]time.Time) {
	fails := it.LastError != "" && !it.LastFailed.Before(it.LastOK)
	bound := s.sourceStale()
	switch it.Kind {
	case "firings":
		bound = s.quietAfter()
	case "incidents":
		if strings.HasPrefix(it.ID, "intake/") {
			bound = receivedQuiet
		}
	}
	if !it.counted {
		it.Failures = -1
	}
	switch {
	case fails:
		it.State = "failing"
		if it.counted {
			it.Down = it.Failures >= sourceDownAfter
		} else {
			// Failing since it last read well, or, never having read well,
			// since its first read failed: past the bound, down. One no
			// failed read was noted of yet (a leader that hasn't said)
			// isn't down for it.
			since := it.LastOK
			if since.IsZero() {
				since = failing[it.ID]
			}
			it.Down = !since.IsZero() && now.Sub(since) > bound
		}
	case it.LastOK.IsZero(), it.held:
		it.State = "pending"
	case now.Sub(it.LastOK) > bound:
		// A source read on a clock (the rules every --watch, a file each
		// run) is read well again and again, whatever it says: none past
		// the bound is a loop that stopped, or a job that copies in.
		it.State, it.Down = "quiet", true
	default:
		it.State = "ok"
	}
}

// sourceErrors are the failures of the status banner that are a source's
// read: the banner links to the Sources page.
var sourceErrors = []string{"reading the rules", "a run", "incidents", "deploys", "marked windows",
	"reading the impact signal again", "reading what the impact signals read", "reading the marked windows' expressions again"}

// errOfSource reports whether the status banner's failure is a source's.
func errOfSource(what string) bool { return slices.Contains(sourceErrors, what) }

// secrets are what may stand for a credential in an error or an address:
// a private key's PEM block, in JSON's escapes too, or cut short; a URL's
// user and password, read as a URL's parser reads them, between its // and
// the last @ before the first /, ? or # after it, so an @ in the password
// is taken out and an @ in the path is left (as in Grafana's
// /d/abc/team@ops); a password with a / ? or # in it, when the address
// before its first / ? or # has a : that isn't a port's (user:pa/ss, not
// host:3000) and what follows the first @ after it is a host, a name or
// an address with or without its port and then a / ? # or another mark
// that ends an address (: , ; ) ] > & \), a quote, a space or the end (a
// password with both an @ and a / ? or # is read as the parser reads it,
// and what follows its @ shows); one of digits and then a / ? or #, which
// reads as a port and a path (lap:1234/pa), when a host and its port
// follow its @, as they seldom do in a path (with a host and no port, as
// in postgres://lap:1234/pa@db/l, it shows, read as host:port/path); a
// Slack webhook's path; a query's token, key, secret, password (pass too,
// as a word of the name), signature, JWT or code (an Azure Function's
// key), its ? & and = written %3F %26 and %3D too, as an address quoted in
// another's query writes them; an Authorization header's value, its scheme
// kept when it is one HTTP's clients send, and else (another scheme, or
// none) all of it, to the line's end or a mark that ends a sentence or a
// clause (a . before a space or at the end, a comma, a ;, a quote, a
// closing bracket, a < or >); a Cookie or Set-Cookie header's, its
// cookies between ; too, as far; each where the header is written as one
// (at the start of the text or of a line, after curl's > or <, after the
// word header, a quote, a {, a [, a ( or a comma, or as Go prints a
// header's map, Authorization:[...]) or with a capital, as a header's name
// is, and in small letters in a sentence too, but where its value starts
// with a word in small letters only, as prose does and a token, a
// session's id or a key seldom does (one has a digit, a capital or a mark
// in it), so "push: authorization: the token has expired" and "the session
// cookie: expired after 10m" read as they were (a cookie's name=value is
// taken out all the same); the same names given as name=value between
// spaces or after a quote, its value quoted as libpq quotes one, or as
// name: value, as YAML and Go write a field, for a password, a client's
// secret, an API, access, secret or private key, or an access token (a
// bare token, key or secret is a word as often, as in "cannot fetch token:
// 400"), at the start of a line, after a {, a [, a (, a comma or a quote,
// or after a space when the name is written as code writes one (with a _
// . or -, or a capital after its first letter, as api_key or apiKey), with
// no space after its colon, or with a value that doesn't start with a word
// in small letters only (a lone word such as password or Password after
// another, a space after its colon and such a word after that, is prose
// as often, as in "invalid password: check the file" and "no Password:
// given"), its value one word when no space follows its colon, as Go's
// %+v puts the next field after a space, and else as far as a header's;
// a JSON string, read with its escapes, as is and escaped in a Go string,
// or a JSON number, and a Python dict's string; a header such as api-key
// or x-api-key; a bearer, basic, token, API key, GenieKey or SSWS token of
// 12 characters or more, and a bearer one shorter with a digit in it; and
// a token GitHub, GitLab, Grafana, Google, Slack or Anthropic writes with
// a prefix of its own, anywhere in the text, in the shape and at about the
// length its maker writes one (secretPrefixed), so a word after a prefix
// (glc_requests_total, ya29.png, ghp_x) reads as it was.
//
// What a pattern can't tell from prose shows: a value whose first word is
// in small letters only, after a header's name in small letters or a
// field's such as password in a sentence ("push: cookie: abcdef",
// "invalid password: hunter"), and a password of digits and then a /
// before a host with no port.
var (
	secretPEM        = regexp.MustCompile(`(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----)(?:[A-Za-z0-9+/=\s\\:,.]|-[A-Za-z0-9])*(-----END [A-Z0-9 ]*PRIVATE KEY-----)?`)
	secretUser       = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/?#\s"'<>]*@`)
	secretUserPath   = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/:?#\s"'<>@\[\]]*:[^/?#\s"'<>@]*[^/?#\s"'<>@0-9][^\s"'<>@]*@((?:[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*|\[[0-9A-Fa-f:.]+\])(?::[0-9]+)?(?:[/?#"'\s:,;)\]>&\\]|$))`)
	secretUserPort   = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/:?#\s"'<>@\[\]]*:[0-9]+[/?#][^\s"'<>]*@((?:[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*|\[[0-9A-Fa-f:.]+\]):[0-9]+(?:[/?#"'\s,;)\]>&\\]|$))`)
	secretSlack      = regexp.MustCompile(`(?i)(hooks\.slack(?:-gov)?\.com/(?:services|workflows|triggers)/)[A-Za-z0-9/_-]+`)
	secretQuery      = regexp.MustCompile(`(?i)((?:[?&;]|%3F|%26)(?:[A-Za-z0-9_.-]*(?:token|key|secret|passw|pwd|auth|signature|sig|credential|jwt)[A-Za-z0-9_.-]*|(?:[A-Za-z0-9_.-]*[_.-])?pass|code)(?:=|%3D))[^&\s"'<>]+`)
	secretAuth       = regexp.MustCompile(`(?im)(` + secretHeaderAt + `(?:proxy-)?authorization[ \t]*[:=][ \t]*\[?|\b(?:proxy-)?authorization:\[)` + secretAuthValue)
	secretAuthSaid   = regexp.MustCompile(`(?i)(\b(?:proxy-)?authorization[ \t]*[:=][ \t]*\[?)` + secretAuthValue)
	secretCookie     = regexp.MustCompile(`(?im)(` + secretHeaderAt + `(?:set-)?cookie[ \t]*:[ \t]*\[?|\b(?:set-)?cookie:\[)` + secretCookieValue)
	secretCookieSaid = regexp.MustCompile(`(?i)(\b(?:set-)?cookie[ \t]*:[ \t]*\[?)` + secretCookieValue)
	secretSaid       = regexp.MustCompile(`(?i)((?:^|[\s,("'])[A-Za-z0-9_.-]*(?:token|secret|passw|pwd|credential|api[_-]?key|access[_-]?key)[A-Za-z0-9_.-]*=)(?:'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|[^\s&"'<>,;)]+)`)
	secretField      = regexp.MustCompile(`(?im)((?:^[ \t]*(?:-[ \t]+)?|[,({\["'][ \t]*)` + secretFieldName + `[ \t]*:)` + secretFieldValue)
	secretFieldMid   = regexp.MustCompile(`(?i)(\s(` + secretFieldName + `)[ \t]*:)` + secretFieldValue)
	secretJSON       = regexp.MustCompile(`(?i)("` + secretName + `"\s*:\s*")(?:[^"\\]|\\.)*`)
	secretGoJSON     = regexp.MustCompile(`(?i)(\\"` + secretName + `\\"\s*:\s*\\")(?:[^"\\]|\\\\(?:\\["\\]|[^"\\]))*`)
	secretNumber     = regexp.MustCompile(`(?i)(\\?"` + secretName + `\\?"\s*:\s*)-?[0-9][0-9.eE+-]*`)
	secretDict       = regexp.MustCompile(`(?i)('` + secretName + `'\s*:\s*')(?:[^'\\]|\\.)*`)
	secretHead       = regexp.MustCompile(`(?i)\b((?:[A-Za-z0-9]+-)+(?:key|token|secret|password|signature)\s*:\s*)[^\s"'<>,;]+`)
	secretToken      = regexp.MustCompile(`(?i)\b(bearer|basic|token|api-?key|geniekey|ssws)(\s+|=|:\s*)[A-Za-z0-9._~+/=-]{12,}`)
	secretBearer     = regexp.MustCompile(`(?i)\b(bearer)([ \t]+|=|:[ \t]*)[A-Za-z._~+/=-]*[0-9][A-Za-z0-9._~+/=-]*`)
)

// secretPrefixed are the tokens their makers write with a prefix of their
// own, each in its shape and at about its length, a little shorter too,
// so a word after the prefix (ghp_x, glc_requests_total, ya29.png) is no
// token: GitHub's ghp_, gho_, ghu_, ghs_ and ghr_ and 36 letters and
// digits (30 here), and github_pat_ and 82 (70); GitLab's glpat- and 20,
// or 27 or more and a . with a version and a checksum, as a routable one
// is; Slack's xoxb- and the like, their numbers and -, then 8 letters and
// digits or more; Anthropic's sk-ant-, its kind (api03-) and 95 (32);
// Grafana's glsa_ for a service account's, 32 letters and digits (24), a
// _ and the 8 hex digits of a CRC-32, and glc_ for Grafana Cloud's, 32 of
// base64 or more; and Google's ya29. for an access token, 20 or more, a .
// in a service account's too. Each is said as its prefix and ***.
var secretPrefixed = []*regexp.Regexp{
	regexp.MustCompile(`\b(gh[pousr]_)[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`\b(github_pat_)[A-Za-z0-9_]{70,}`),
	regexp.MustCompile(`\b(glpat-)[A-Za-z0-9_-]{20,}(?:\.[A-Za-z0-9_-]+)*`),
	regexp.MustCompile(`\b(xox[abposr]-)(?:[0-9]+-)*[A-Za-z0-9]{8,}[A-Za-z0-9-]*`),
	regexp.MustCompile(`\b(sk-ant-)[A-Za-z0-9_-]{32,}`),
	regexp.MustCompile(`\b(glsa_)[A-Za-z0-9]{24,}_[0-9A-Fa-f]{8}`),
	regexp.MustCompile(`\b(glc_)[A-Za-z0-9+/=_-]{32,}`),
	regexp.MustCompile(`\b(ya29\.)[A-Za-z0-9_-](?:[A-Za-z0-9_-]|\.[A-Za-z0-9_-]){19,}`),
}

// secretName is a JSON or a Python dict's name that may stand for a
// credential's.
const secretName = `[A-Za-z0-9_.-]*(?:token|key|secret|passw|pwd|auth|signature|credential|jwt)[A-Za-z0-9_.-]*`

// secretWord is a word of a header's or a field's value: no space, quote,
// < or >, comma, ; or closing bracket in it, and no . at its end, as a
// sentence ends. A value's words, between spaces or tabs, run as far as
// the line's end or such a mark.
const secretWord = `[^\s"'<>,;)\]}]*[^\s"'<>,;)\]}.]`

// secretHeaderAt is where a header is written as one: the start of the
// text or of a line, curl's > or < too, after the word header, a quote, a
// {, a [, a ( or a comma.
const secretHeaderAt = `(?:^[ \t]*(?:[<>][ \t]*)?|\bheaders?[ \t]+|["'{\[(,][ \t]*)`

// secretAuthValue is an Authorization header's value: its first word (the
// scheme, or the credential), the space and a quote after it and its next
// word, and the words after that.
const secretAuthValue = `(` + secretWord + `)(?:([ \t]+["']?)(` + secretWord + `))?((?:[ \t]+` + secretWord + `)*)`

// secretCookieValue is a Cookie header's value: its first word and the
// words and the cookies after it, between spaces and ;.
const secretCookieValue = `(` + secretWord + `)((?:(?:;[ \t]*|[ \t]+)` + secretWord + `)*)`

// secretFieldName is a name: value field's name that may stand for a
// credential's, and secretFieldValue its value: after a space, a quoted
// string or its words (the first on its own too); and right after the
// colon, a quoted string or one word.
const (
	secretFieldName  = `(?:[A-Za-z0-9_.-]*(?:passw|pwd|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*|[A-Za-z0-9_.-]*[_.-](?:secret|token))`
	secretQuoted     = `(?:'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*")`
	secretFieldValue = `(?:([ \t]+)(?:` + secretQuoted + `|(` + secretWord + `)(?:[ \t]+` + secretWord + `)*)|` + secretQuoted + `|` + secretWord + `)`
)

// wordName is a field's name written as a word is, all in small letters
// but the first: password or Password, not api_key, apiKey or PASSWORD.
var wordName = regexp.MustCompile(`^[A-Za-z][a-z]*$`)

// proseWord is a word as prose writes one, in small letters only (the,
// expired, given), not as a token, a session's id or a key is, with a
// digit, a capital or a mark in it.
var proseWord = regexp.MustCompile(`^[a-z]+$`)

// capitalized reports whether a header's name is written as a header's
// is, with a capital: Authorization, Cookie, Set-Cookie.
func capitalized(name string) bool { return name != "" && 'A' <= name[0] && name[0] <= 'Z' }

// authSchemes are the Authorization header's schemes HTTP's clients send,
// in lower case: a value after one is the credential, and the scheme is
// said as it was.
var authSchemes = map[string]bool{"bearer": true, "basic": true, "digest": true, "token": true,
	"apikey": true, "api-key": true, "geniekey": true, "ssws": true, "negotiate": true, "ntlm": true,
	"dpop": true, "aws4-hmac-sha256": true}

// scrubSecrets is a text with what may be a credential in it said as ***,
// for every reader, an admin too: a source's error or address shows none.
// It goes by patterns, and takes out more than a credential where a name
// only looks like one's.
func scrubSecrets(text string) string {
	text = secretPEM.ReplaceAllString(text, "${1}***${2}")
	text = secretUser.ReplaceAllString(text, "${1}***@")
	text = secretUserPath.ReplaceAllString(text, "${1}***@${2}")
	text = secretUserPort.ReplaceAllString(text, "${1}***@${2}")
	text = secretSlack.ReplaceAllString(text, "${1}***")
	text = secretQuery.ReplaceAllString(text, "${1}***")
	text = secretAuth.ReplaceAllStringFunc(text, scrubAuthorization)
	text = secretAuthSaid.ReplaceAllStringFunc(text, scrubAuthorizationSaid)
	text = secretCookie.ReplaceAllString(text, "${1}***")
	text = secretCookieSaid.ReplaceAllStringFunc(text, scrubCookieSaid)
	text = secretSaid.ReplaceAllString(text, "${1}***")
	text = secretField.ReplaceAllString(text, "${1}${2}***")
	text = secretFieldMid.ReplaceAllStringFunc(text, scrubFieldMid)
	text = secretJSON.ReplaceAllString(text, "${1}***")
	text = secretGoJSON.ReplaceAllString(text, "${1}***")
	text = secretNumber.ReplaceAllString(text, "${1}***")
	text = secretDict.ReplaceAllString(text, "${1}***")
	text = secretHead.ReplaceAllString(text, "${1}***")
	text = secretToken.ReplaceAllString(text, "${1}${2}***")
	text = secretBearer.ReplaceAllString(text, "${1}${2}***")
	for _, re := range secretPrefixed {
		text = re.ReplaceAllString(text, "${1}***")
	}
	return text
}

// scrubAuthorization is an Authorization header as secretAuth found it,
// written as a header is, its value said as ***: after a scheme HTTP's
// clients send, the scheme kept and the words after the credential too,
// and a scheme alone as it was, as an error that names one says it; with
// another scheme, or none, all of what follows the header's name, as any
// of it may be the credential.
func scrubAuthorization(header string) string {
	m := secretAuth.FindStringSubmatch(header)
	if authSchemes[strings.ToLower(m[2])] {
		if m[4] == "" {
			return header
		}
		return m[1] + m[2] + m[3] + "***" + m[5]
	}
	return m[1] + "***"
}

// scrubAuthorizationSaid is an Authorization header as secretAuthSaid
// found it, in the middle of a text: as scrubAuthorization has it, but for
// a name in small letters whose value starts with a word in small letters,
// which is as it was, as that is prose as often ("authorization: the token
// has expired").
func scrubAuthorizationSaid(said string) string {
	m := secretAuthSaid.FindStringSubmatch(said)
	if authSchemes[strings.ToLower(m[2])] {
		if m[4] == "" {
			return said
		}
		return m[1] + m[2] + m[3] + "***" + m[5]
	}
	if !capitalized(m[1]) && proseWord.MatchString(m[2]) {
		return said
	}
	return m[1] + "***"
}

// scrubCookieSaid is a Cookie header as secretCookieSaid found it, in the
// middle of a text: its value said as ***, but for a name in small letters
// whose value starts with a word in small letters and has no name=value in
// it, which is as it was ("the session cookie: expired after 10m").
func scrubCookieSaid(said string) string {
	m := secretCookieSaid.FindStringSubmatch(said)
	if !capitalized(m[1]) && proseWord.MatchString(m[2]) && !strings.Contains(m[2]+m[3], "=") {
		return said
	}
	return m[1] + "***"
}

// scrubFieldMid is a name: value field as secretFieldMid found it, after a
// space: its value said as ***, but for a name written as a word is with a
// space after its colon and a word in small letters after that, which is
// as it was ("invalid password: check the file").
func scrubFieldMid(field string) string {
	m := secretFieldMid.FindStringSubmatch(field)
	if m[3] != "" && wordName.MatchString(m[2]) && proseWord.MatchString(m[4]) {
		return field
	}
	return m[1] + m[3] + "***"
}

// shownError is a source's error as its reader may read it, in language
// l: as Run history says a run's (runsFor: without the paths of serve's
// files, nor why the ledger's database couldn't be reached, but to an
// admin), and without a credential, to anyone.
func (s *Server) shownError(text string, adminReads bool, l string) string {
	if text == "" {
		return ""
	}
	return scrubSecrets(s.runsFor([]ledger.Run{{Error: text}}, adminReads, l)[0].Error)
}

// sourcesFor are the sources as a reader is shown them, in language l.
func (s *Server) sourcesFor(ctx context.Context, adminReads bool, l string) ([]sourceItem, error) {
	items, err := s.sourceList(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].LastError = s.shownError(items[i].LastError, adminReads, l)
		if d := items[i].Lag; d != nil {
			x := d.Seconds()
			items[i].LagSeconds = &x
		}
		if d := items[i].Took; d != nil {
			x := d.Seconds()
			items[i].TookSeconds = &x
		}
		if adminReads {
			items[i].Where = scrubSecrets(items[i].Where)
		} else {
			items[i].Where = ""
		}
	}
	return items, nil
}

// sourcesView is the Sources page.
type sourcesPageView struct {
	Items                       []sourceItem
	OK, Failing, Quiet, Pending int
	Down                        int
	Cursor                      time.Time
}

func (s *Server) sourcesPage(r *http.Request) (any, error) {
	items, err := s.sourcesFor(r.Context(), s.role(r) == admin, lang(r))
	if err != nil {
		return nil, err
	}
	v := sourcesPageView{Items: items}
	for _, it := range items {
		switch it.State {
		case "ok":
			v.OK++
		case "failing":
			v.Failing++
		case "quiet":
			v.Quiet++
		default:
			v.Pending++
		}
		if it.Down {
			v.Down++
		}
	}
	return v, nil
}

// apiSourcesAll is GET /api/v1/sources?all=true: every source, as the
// Sources page shows it. Without all, the sources firings are copied in
// from, as before.
func (s *Server) apiSourcesAll(w http.ResponseWriter, r *http.Request) {
	items, err := s.sourcesFor(r.Context(), s.role(r) == admin, "en")
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if items == nil {
		items = []sourceItem{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"sources": items})
}

// sourceNotice is what a notice says of a source down or back; Raw, that
// its name is its own, not a text to translate.
type sourceNotice struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Raw      bool      `json:"own_name,omitempty"`
	From     string    `json:"from,omitempty"`
	State    string    `json:"state"`
	LastOK   time.Time `json:"last_ok,omitzero"`
	Failures int       `json:"failures"`
	Error    string    `json:"error,omitempty"`
}

// toldKey is the meta key a source's last notice is kept under: down, or
// up and when, "up UNIX".
func toldKey(id string) string { return "source-told." + id }

// tellSources tells, once each time, of a source gone down, and of one
// back: down by its bounds (judgeSource), back once it has read well for
// sourceBackAfter, and down again no sooner than sourceRenotify after it
// was told back. Firings copied in are told of as source.quiet, as before.
// What is told of a source's error is what anyone but an admin is shown.
func (s *Server) tellSources(ctx context.Context) error {
	dests := s.toldTo()
	if len(dests) == 0 {
		return nil
	}
	items, err := s.sourceList(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	for _, it := range items {
		if it.Kind == "firings" {
			continue // source.quiet
		}
		was, err := s.l.Told(ctx, toldKey(it.ID))
		if err != nil {
			return err
		}
		event, value := "", ""
		switch {
		case it.Down && was != "down":
			if at, ok := strings.CutPrefix(was, "up "); ok {
				if u, err := strconv.ParseInt(at, 10, 64); err == nil && now.Sub(time.Unix(u, 0)) < sourceRenotify {
					continue // shown down, told once the hour is out
				}
			}
			event, value = "source.down", "down"
		case !it.Down && was == "down" && it.State == "ok" && (it.LastFailed.IsZero() || now.Sub(it.LastFailed) >= sourceBackAfter):
			event, value = "source.back", fmt.Sprintf("up %d", now.Unix())
		default:
			continue
		}
		n := sourceNotice{ID: it.ID, Kind: it.Kind, Name: it.Name, Raw: it.Raw, From: it.From, State: it.State, LastOK: it.LastOK, Failures: it.Failures,
			Error: s.shownError(it.LastError, false, "en")}
		told := make([]ledger.Told, 0, len(dests))
		for _, d := range dests {
			told = append(told, ledger.Told{Dest: d, About: map[string]any{"source_read": n}})
		}
		// Told only while what was read is kept still: a call a moment
		// before this one (the watch's, and the jobs') told it already.
		if err := s.l.TellFrom(ctx, toldKey(it.ID), was, value, event, told); err != nil {
			return err
		}
	}
	return nil
}

// sourceLine is a notice of a source down or back, in a sentence, in
// language l.
func (s *Server) sourceLine(l, event string, n *sourceNotice) string {
	name := n.Name
	if !n.Raw {
		name = tr(l, n.Name)
	}
	if n.From != "" {
		name += " (" + n.From + ")"
	}
	switch {
	case event == "source.back":
		return tr(l, "lapilli: %s reads well again", name)
	case n.State == "quiet":
		return tr(l, "lapilli: %s hasn't been read, nor written to, since %s", name, n.LastOK.In(s.cfg.Loc).Format("2006-01-02 15:04"))
	case n.Failures > 0:
		return tr(l, "lapilli: %s failed %d reads in a row: %s", name, n.Failures, n.Error)
	}
	return tr(l, "lapilli: %s has failed since it last read well: %s", name, n.Error)
}

// sourceKindText is each kind of source as the page names it, in
// English: the page says it in its language as "source kind|TEXT".
var sourceKindText = map[string]string{
	"rules": "Rules", "history": "Firings replayed", "incidents": "Incidents", "deploys": "Deploys", "marked": "Marked windows",
	"impact": "Impact signal", "on-call": "On-call", "firings": "Firings copied in",
}

// KindText is a source's kind, as the page names it.
func (it sourceItem) KindText() string { return sourceKindText[it.Kind] }

// RuleCount is how many rules it gave, HasRules whether it gives any to
// count.
func (it sourceItem) RuleCount() int { return *it.Rules }
func (it sourceItem) HasRules() bool { return it.Rules != nil }
