package serve

import (
	"context"
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
// the rules the last read gave (Rules, -1 for a source of no rules); and
// how long the last read took.
type sourceRead struct {
	OK     time.Time     `json:"ok,omitzero"`
	Failed time.Time     `json:"failed,omitzero"`
	Err    string        `json:"err,omitempty"`
	InARow int           `json:"in_a_row,omitempty"`
	Rules  int           `json:"rules"`
	Took   time.Duration `json:"took,omitempty"`
}

// readsState is what this replica saw of the sources it reads itself,
// by id.
type readsState struct {
	sync.Mutex
	m map[string]sourceRead
	// failing is when each source whose reads aren't counted was first
	// seen failing, since it last read well, by this replica.
	failing map[string]time.Time
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
	x.Took = time.Since(began)
	if err != nil {
		x.Failed, x.Err = s.now(), err.Error()
		x.InARow++
	} else {
		x.OK, x.Err, x.InARow, x.Rules = s.now(), "", 0, rules
	}
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
	// a 429 yielded to), so that Prometheus not read meanwhile isn't quiet.
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
		it := sourceItem{ID: id, Kind: kind, Name: name, From: from, Where: where, counted: true, Failures: x.InARow}
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
		for _, name := range slices.Sorted(maps.Keys(received)) {
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
	for i := range out {
		s.judgeSource(&out[i], now)
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
// quiet, past its bound.
func (s *Server) judgeSource(it *sourceItem, now time.Time) {
	failing := it.LastError != "" && !it.LastFailed.Before(it.LastOK)
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
	case failing:
		it.State = "failing"
		if it.counted {
			it.Down = it.Failures >= sourceDownAfter
		} else {
			// Failing since it last read well, or since this replica first
			// saw it failing, never having read it well: past the bound,
			// down.
			since := it.LastOK
			if since.IsZero() {
				since = s.seenFailing(it.ID, now)
			}
			it.Down = now.Sub(since) > bound
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
	if !failing && !it.counted {
		s.reads.Lock()
		delete(s.reads.failing, it.ID)
		s.reads.Unlock()
	}
}

// seenFailing is when this replica first saw the source id failing, now
// for the first time.
func (s *Server) seenFailing(id string, now time.Time) time.Time {
	s.reads.Lock()
	defer s.reads.Unlock()
	if s.reads.failing == nil {
		s.reads.failing = map[string]time.Time{}
	}
	at, ok := s.reads.failing[id]
	if !ok {
		at = now
		s.reads.failing[id] = at
	}
	return at
}

// sourceErrors are the failures of the status banner that are a source's
// read: the banner links to the Sources page.
var sourceErrors = []string{"reading the rules", "a run", "incidents", "deploys", "marked windows",
	"reading the impact signal again", "reading what the impact signals read", "reading the marked windows' expressions again"}

// errOfSource reports whether the status banner's failure is a source's.
func errOfSource(what string) bool { return slices.Contains(sourceErrors, what) }

// secrets are what may stand for a credential in an error or an address:
// a URL's user and password, a query's token, key, secret, password or
// signature, and a bearer or basic token.
var (
	secretUser  = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@"'<>]+@`)
	secretQuery = regexp.MustCompile(`(?i)([?&;][A-Za-z0-9_.-]*(?:token|key|secret|passw|pwd|auth|signature|sig|credential)[A-Za-z0-9_.-]*=)[^&\s"'<>]+`)
	secretToken = regexp.MustCompile(`(?i)\b(bearer|basic|token)(\s+|=|:\s*)[A-Za-z0-9._~+/=-]{12,}`)
)

// scrubSecrets is a text with what may be a credential in it said as ***,
// for every reader, an admin too: a source's error or address shows none.
func scrubSecrets(text string) string {
	text = secretUser.ReplaceAllString(text, "${1}***@")
	text = secretQuery.ReplaceAllString(text, "${1}***")
	return secretToken.ReplaceAllString(text, "${1}${2}***")
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
	dests := s.dests()
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
		if err := s.l.TellOnce(ctx, toldKey(it.ID), value, event, told); err != nil {
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
