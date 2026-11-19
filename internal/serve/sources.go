package serve

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// Incident sources (docs/design/ui.md, "Incident sources"): where the
// incidents firings are judged against come from, and whether each is
// alive, on the Settings page: those written in Lapilli; those a tool's job
// copies in through the incidents port, by source; the incidents file; the
// impact signal. A source that isn't set up says so, in no colour; tokens
// are never shown, and only an admin sees which file or port is which.

// sourcesSpan is how far back the sources are counted.
const sourcesSpan = 28 * 24 * time.Hour

// receivedQuiet is how long a source that wrote through the incidents
// port may go without writing before it is said to be quiet: a job copies
// its tool's incidents every hour or so, each again, unchanged or not.
const receivedQuiet = 48 * time.Hour

// redactPaths is a text without the paths of the files and directories
// serve was started with (filePaths), which an admin alone reads: where
// the data, a token or a webhook is kept says how serve is run, and an
// error reading one says it, on the Settings page and in the status banner
// of every page. Each is said by its flag in their place.
func (s *Server) redactPaths(text, l string) string {
	if text == "" {
		return text
	}
	return hidePaths(text, s.filePaths(), l)
}

// runsFor are runs as they are shown to a reader, an admin or not, in
// language l: why a run failed, and why a rule couldn't be replayed, can
// name a file serve was started with (a webhook's that can't be read, the
// rules'), and are said without its path to anyone but an admin, as the
// status banner says them (redactPaths).
func (s *Server) runsFor(runs []ledger.Run, adminReads bool, l string) []ledger.Run {
	if adminReads || !slices.ContainsFunc(runs, func(r ledger.Run) bool { return r.Error != "" || len(r.Failed) > 0 }) {
		return runs
	}
	ps := s.filePaths()
	// Why the ledger's database couldn't be reached names its server, its
	// user and its database: said as the status banner says it.
	hide := func(text string) string {
		if unreached(text) {
			return tr(l, unreachedText)
		}
		return hidePaths(text, ps, l)
	}
	out := slices.Clone(runs)
	for i, r := range out {
		out[i].Error = hide(r.Error)
		if len(r.Failed) > 0 {
			out[i].Failed = make(map[string]string, len(r.Failed))
			for rule, why := range r.Failed {
				out[i].Failed[rule] = hide(why)
			}
		}
	}
	return out
}

// errorsFor is how a tool's error is said to an agent acting for a
// reader, an admin or not: as runs are (runsFor), without a path serve
// was started with, nor why the ledger's database couldn't be reached,
// but to an admin.
func (s *Server) errorsFor(adminReads bool) func(error) error {
	if adminReads {
		return func(err error) error { return err }
	}
	return func(err error) error {
		if err == nil {
			return nil
		}
		if t := err.Error(); unreached(t) {
			return errors.New(unreachedText)
		} else if hidden := s.redactPaths(t, ""); hidden != t {
			return errors.New(hidden)
		}
		return err
	}
}

// hidePaths is text with each of ps it says whole said by what it is
// (filePath.name), in one pass: at each place a path may begin, the
// longest of ps there is taken, and what is put in its place is never
// looked in again, for another. A path is there whole when no letter,
// digit, _, -, . or separator is before it, and no letter, digit or _
// after it: it begins the text or follows a space, a quote, a colon or the
// like, and ends the text, or a word, or a separator follows it, as it
// does a path joined under it. So a word that holds one as letters keeps
// them.
func hidePaths(text string, ps []filePath, l string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); i++ {
		if i > 0 && (wordByte(text[i-1]) || strings.IndexByte(`-./\`, text[i-1]) >= 0) {
			continue
		}
		for _, p := range ps {
			end := i + len(p.path)
			if strings.HasPrefix(text[i:], p.path) && (end == len(text) || !wordByte(text[end])) {
				b.WriteString(text[last:i])
				b.WriteString(p.name(l))
				last, i = end, end-1
				break
			}
		}
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// wordByte reports whether c is of a word, where a path that ends at it
// would end inside one: a letter, a digit or _.
func wordByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_'
}

// filePath is a path serve was started with, and the flag that gave it.
type filePath struct{ path, flag string }

// name is what a path is said as in its place, in language l: by what it
// is, or the flag that gave it.
func (p filePath) name(l string) string {
	switch p.flag {
	case "--incidents":
		return tr(l, "the incidents file")
	case "--deploys":
		return tr(l, "the deploys file")
	case "--marked":
		return tr(l, "the marked file")
	case "--config":
		return tr(l, "the config file")
	}
	return tr(l, "the path given to %s", p.flag)
}

// filePaths are the paths of the files and directories serve was started
// with, by the flag that gave each: as given, with no separator at its
// end, and cleaned, as a path joined under a directory says it
// (filepath.Join), and where it leads; the longest first, so that one
// inside another is taken out before it. A value that doesn't read as a path once its separators
// at the end are let go (pathLike) is left, so that a bare name, --rules
// rules or --rules rules/, doesn't take its word out of every sentence.
// --impact-file and --on-call aren't among them: they are read once, as
// serve starts, and a failure stops it.
func (s *Server) filePaths() []filePath {
	c := s.cfg
	given := []filePath{{c.Incidents, "--incidents"}, {c.Deploys, "--deploys"}, {c.Marked, "--marked"}, {c.Config, "--config"},
		{c.RulesPath, "--rules"}, {c.RulesHistory, "--rules-history"}, {c.LokiRules, "--loki-rules"},
		{c.Ledger, "--ledger"}, {c.LedgerURLFile, "--ledger-url-file"},
		{c.GrafanaToken, "--grafana-token-file"}, {c.LokiToken, "--loki-token-file"}, {c.RulerToken, "--ruler-token-file"},
		{c.Slack, "--slack-webhook-file"}, {c.NotifyWebhook, "--notify-webhook-file"}, {c.NotifyWebhookSecret, "--notify-webhook-secret-file"},
		{c.SessionKey, "--session-key-file"},
		{c.IncidentsToken, "--incidents-token-file"}, {c.FiringsToken, "--firings-token-file"}, {c.TasksToken, "--tasks-token-file"},
		{c.NotifyToken, "--notify-token-file"}, {c.NotifyGrafanaSecret, "--notify-grafana-secret-file"},
		{c.ChangesToken, "--changes-token-file"}, {c.ChangesGrafanaToken, "--changes-grafana-token-file"},
		{c.ChangesRulerToken, "--changes-ruler-token-file"}, {c.ChangesPaused, "--changes-paused"}, {c.ChangesOCIConfig, "--changes-oci-config"}}
	for _, f := range c.TeamDigests {
		given = append(given, filePath{f, "--team-digest"})
	}
	for _, f := range c.ReportDests {
		given = append(given, filePath{f, "--report-dest"})
	}
	var out []filePath
	add := func(path, flag string) {
		if p := (filePath{strings.TrimRight(path, `/\`), flag}); pathLike(p.path) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, p := range given {
		if p.path == "" {
			continue
		}
		add(p.path, p.flag)
		add(filepath.Clean(p.path), p.flag)
		// And where it leads, should it be a link or under one: what is
		// walked behind one is said there, as a history a git-sync or a
		// mounted volume keeps behind a link is (rules.History). As it
		// leads now, where it can be told.
		if real, err := filepath.EvalSymlinks(p.path); err == nil {
			add(real, p.flag)
		}
	}
	slices.SortStableFunc(out, func(a, b filePath) int { return len(b.path) - len(a.path) })
	return out
}

// pathLike reports whether a flag's value reads as a path where a text
// says it: with a separator or a dot, and more than separators and dots.
func pathLike(p string) bool {
	return strings.ContainsAny(p, `/\.`) && strings.Trim(p, `/\.`) != ""
}

// impactStale is how long since the impact signal was last read it is
// said to be read no more: two runs, the hours they settle, and one more.
func impactStale(c Config) time.Duration { return 2*c.Every + c.Settle + time.Hour }

// receivedEvery is how often a replica keeps that a source wrote through
// the incidents port: the write it kept stands for those after it, within
// this.
const receivedEvery = 10 * time.Minute

// recipesURL is where the recipes that copy incidents in are told of.
const recipesURL = "https://github.com/lapilli-project/lapilli/blob/main/recipes/incidents/README.md"

// sourcePrefix is an id's source: what comes before its first -, as the
// recipes give them (PD-41, GH-7, OPS-12 of a Jira project), a letter and
// fifteen more letters, digits or _.
var sourcePrefix = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]{0,15})-.`)

// sourceOf is the source an incident's id names, upper case: "" for none.
func sourceOf(id string) string {
	if m := sourcePrefix.FindStringSubmatch(id); m != nil {
		return strings.ToUpper(m[1])
	}
	return ""
}

// sourceTools are the tools whose recipes give their ids a prefix of their
// own; a Jira project's key, incident.io's INC and another tool's are
// shown as they are.
var sourceTools = map[string]string{"PD": "PagerDuty", "GH": "GitHub", "FH": "FireHydrant", "IRM": "Grafana IRM", "CP": "Compass", "RL": "Rootly"}

// noteReceived keeps that the source of an incident's id wrote through the
// incidents port now: once in receivedEvery by this replica.
func (s *Server) noteReceived(ctx context.Context, id string) {
	src, now := sourceOf(id), s.now()
	s.sources.Lock()
	if s.sources.noted == nil || len(s.sources.noted) > 4*receivedSources {
		s.sources.noted = map[string]time.Time{}
	}
	last, ok := s.sources.noted[src]
	if ok && !now.Before(last) && now.Sub(last) < receivedEvery {
		s.sources.Unlock()
		return
	}
	s.sources.noted[src] = now
	s.sources.Unlock()
	if err := s.l.NoteReceived(ctx, src, now); err != nil {
		s.log.Warn("keeping when a source last wrote through the incidents port", "err", err)
	}
}

// receivedSources is how many sources the page lists, the latest first.
const receivedSources = 20

// sourceRow is a source, as the page lists it: its name (a text to
// translate, or with Raw a source's own, an id's prefix), its state
// (statusKinds' source/…), when it last wrote or was read, and what (Last,
// LastID, the latest incident written), how many incidents it gave in the
// last four weeks (N, with Counts), what it is (About, to translate), and
// for an admin where (Where); Err, why it last failed.
type sourceRow struct {
	Name     string
	Raw      bool
	Tool     string
	State    string
	Last     time.Time
	LastWhat string
	LastID   string
	N        int
	Counts   bool
	About    string
	Where    string
	Err      string
	Recipes  bool
	// Data is, of the impact signal, where the data of the selectors it
	// reads begins.
	Data *dataView
	// Signal is, of a named impact signal's row, its name; Since, from when
	// it keeps the record, and NoRecord, one that keeps none
	// (docs/design/signals.md, step 3).
	Signal   string
	Since    time.Time
	NoRecord bool
}

// sourcesView is the incident sources: the rows, whether no incident was
// written down in two weeks (Quiet), and the sources past those listed.
type sourcesView struct {
	Rows    []sourceRow
	Quiet   bool
	More    int
	Recipes string
}

// sourcesOf works out the incident sources, for the reader: an admin sees
// which file and port.
func (s *Server) sourcesOf(ctx context.Context, r *http.Request) (*sourcesView, error) {
	now := s.now()
	adminReads := s.role(r) == admin
	v := &sourcesView{Recipes: recipesURL}
	got, err := s.l.IncidentSources(ctx, now.Add(-sourcesSpan))
	if err != nil {
		return nil, err
	}
	seen := s.shownSources()

	// Written in Lapilli.
	w := sourceRow{Name: "Written in Lapilli", State: "none", N: got.Written, Counts: true, LastWhat: "last written",
		About: "On the Incidents page, through the API, by confirming an impact window, or by saying a firing was real."}
	if got.Written > 0 {
		w.State = "ok"
	}
	if got.LastWritten != nil {
		w.Last, w.LastID = got.LastWritten.Start, got.LastWritten.ID
	}
	v.Rows = append(v.Rows, w)

	// Copied in through the incidents port, by source.
	if s.cfg.IncidentsListen == "" || s.cfg.IncidentsToken == "" {
		v.Rows = append(v.Rows, sourceRow{Name: "Intake API", State: "off", Recipes: true,
			About: "Not set up: a job copies a tool's incidents in through --incidents-listen, with the token in --incidents-token-file."})
	} else {
		received, err := s.l.Received(ctx)
		if err != nil {
			return nil, err
		}
		type src struct {
			last    time.Time
			changed time.Time
			n       int
		}
		bySrc := map[string]*src{}
		at := func(name string) *src {
			if bySrc[name] == nil {
				bySrc[name] = &src{}
			}
			return bySrc[name]
		}
		for name, t := range received {
			at(name).last = t
		}
		for _, c := range got.Copied {
			x := at(sourceOf(c.ID))
			x.changed = later(x.changed, c.Updated)
			if !c.Start.Before(now.Add(-sourcesSpan)) {
				x.n++
			}
		}
		where := ""
		if adminReads {
			where = tr(lang(r), "port %s, token in %s", s.cfg.IncidentsListen, s.cfg.IncidentsToken)
		}
		names := make([]string, 0, len(bySrc))
		for name := range bySrc {
			names = append(names, name)
		}
		lastOf := func(name string) time.Time { return later(bySrc[name].last, bySrc[name].changed) }
		quiet := func(name string) bool { return now.Sub(lastOf(name)) > receivedQuiet }
		// The quiet first, so that one is never among those past the
		// list; then the latest first.
		slices.SortFunc(names, func(a, b string) int {
			if qa, qb := quiet(a), quiet(b); qa != qb {
				if qa {
					return -1
				}
				return 1
			}
			if c := lastOf(b).Compare(lastOf(a)); c != 0 {
				return c
			}
			return strings.Compare(a, b)
		})
		if len(names) > receivedSources {
			v.More, names = len(names)-receivedSources, names[:receivedSources]
		}
		for _, name := range names {
			x := bySrc[name]
			row := sourceRow{Name: name + "-", Raw: true, Tool: sourceTools[name], N: x.n, Counts: true, Where: where, State: "ok", LastWhat: "last received", Last: x.last,
				About: "Copied in by a tool's job, through the incidents port."}
			switch name {
			case "":
				row.Name, row.Raw = "Ids with no prefix", false
			case ledger.OtherSources:
				row.Name, row.Raw = "Other sources, past the most kept", false
			}
			if x.last.IsZero() {
				// Kept before the ledger kept when each wrote: when one of
				// its incidents last changed.
				row.Last, row.LastWhat = x.changed, "last changed"
			}
			if now.Sub(row.Last) > receivedQuiet {
				row.State = "quiet"
				row.About = "Nothing received in two days: check its job is still running."
			}
			v.Rows = append(v.Rows, row)
		}
		if len(names) == 0 {
			v.Rows = append(v.Rows, sourceRow{Name: "Intake API", State: "pending", Where: where, Recipes: true,
				About: "Set up, and nothing received yet."})
		}
	}

	// The incidents file.
	file := sourceRow{Name: "Incidents file", State: "off", About: "Not set up: --incidents FILE, read again each run."}
	if s.cfg.Incidents != "" {
		file = sourceRow{Name: "Incidents file", State: "ok", Last: seen.FileRead, LastWhat: "last read", N: got.File, Counts: true,
			About: "Read again each run; the file owns what it holds."}
		if adminReads {
			file.Where = s.cfg.Incidents
		}
		switch {
		case seen.FileErr != "" && !seen.FileFailed.Before(seen.FileRead):
			file.State, file.Err = "failing", seen.FileErr
			if !adminReads {
				file.Err = s.redactPaths(file.Err, lang(r))
			}
		case seen.FileRead.IsZero():
			file.State = "pending"
		}
	}
	v.Rows = append(v.Rows, file)

	// The impact signal; with named ones, a row each.
	n, set, err := s.signalsNowOf(ctx)
	if err != nil {
		return nil, err
	}
	if n.named() {
		rows, err := s.signalSources(ctx, n, seen, now)
		if err != nil {
			return nil, err
		}
		v.Rows = append(v.Rows, rows...)
		out, err := s.outcomes(ctx)
		if err != nil {
			return nil, err
		}
		v.Quiet = out.Record.Quiet
		return v, nil
	}
	c := s.criterionOf(set)
	imp := sourceRow{Name: "Impact signal", State: "off", About: "Off: set it under Incident criteria."}
	if c.Expr != "" {
		imp = sourceRow{Name: "Impact signal", State: "ok", Last: seen.ImpactRead, LastWhat: "last read", N: got.Impact, Counts: true,
			About: "Each run reads it over the hours it replays; a window counts unless an incident written down covers it."}
		k, err := s.l.ImpactKept(ctx)
		if err != nil {
			return nil, err
		}
		imp.Data = dataViewOf(dataOf(k, c.key()))
		switch {
		case seen.ImpactErr != "" && !seen.ImpactFailed.Before(seen.ImpactRead):
			imp.State, imp.Err = "failing", seen.ImpactErr
		case seen.ImpactRead.IsZero() || seen.ImpactKey != c.key():
			// Not yet read with the criterion in effect: its last read was
			// another's.
			imp.State, imp.About = "pending", "Not read with the criterion in effect yet."
		case now.Sub(seen.ImpactRead) > impactStale(s.cfg):
			imp.State, imp.About = "quiet", "Not read for longer than two runs: check the runs are running."
		case !k.Floor.IsZero():
			imp.State, imp.About = "unread", "Read, waiting to be read again over the last four weeks: no record is kept by it meanwhile."
		case imp.Data != nil && imp.Data.NoData:
			imp.State, imp.About = "nodata", "A selector it reads has no data: the impact signal keeps no record until each has some."
		}
	}
	v.Rows = append(v.Rows, imp)

	out, err := s.outcomes(ctx)
	if err != nil {
		return nil, err
	}
	v.Quiet = out.Record.Quiet
	return v, nil
}

// signalSources are the named impact signals in effect, n, as the
// Incident sources panel lists them, a row each: when it was last read,
// its windows in the last four weeks, failing with Prometheus's error,
// each selector and where its data begins, and from when it keeps the
// record.
func (s *Server) signalSources(ctx context.Context, n signalsNow, seen sourcesSeen, now time.Time) ([]sourceRow, error) {
	counts, err := s.l.SignalWindowsSince(ctx, now.Add(-sourcesSpan))
	if err != nil {
		return nil, err
	}
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		return nil, err
	}
	var out []sourceRow
	for _, d := range n.Defs {
		got := seen.Signals[d.Name]
		row := sourceRow{Name: "Impact signal", Signal: d.Name, State: "ok", Last: got.Read, LastWhat: "last read", N: counts[d.Name], Counts: true, NoRecord: !d.Record,
			About: "Each run reads it on its own over the hours it replays; a window counts unless an incident written down covers it."}
		key := signalCriterion(d).key()
		var r ledger.SignalRow
		if i := slices.IndexFunc(rows, func(r ledger.SignalRow) bool { return r.Name == d.Name }); i >= 0 {
			r = rows[i]
		}
		k := r.Kept
		row.Since = k.From()
		row.Data = dataViewOf(dataOf(k, key))
		switch {
		case got.Err != "" && !got.Failed.Before(got.Read):
			row.State, row.Err = "failing", got.Err
		case !r.Missed.IsZero():
			row.State, row.About = "failing", "Missed by a run, which recorded the rest: a later one reads it from where it was missed, within a week."
		case got.Read.IsZero() || k.Read != key:
			row.State, row.About = "pending", "Not read with the criterion in effect yet."
		case now.Sub(got.Read) > impactStale(s.cfg):
			row.State, row.About = "quiet", "Not read for longer than two runs: check the runs are running."
		case !k.Floor.IsZero():
			row.State, row.About = "unread", "Read, waiting to be read again over the last four weeks: no record is kept by it meanwhile."
		case row.Data != nil && row.Data.NoData:
			row.State, row.About = "nodata", "A selector it reads has no data: it keeps no record until each has some."
		}
		out = append(out, row)
	}
	return out, nil
}
