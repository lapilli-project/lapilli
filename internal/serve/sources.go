package serve

import (
	"context"
	"net/http"
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

// redactPaths is a text without the incidents file's path, which an
// admin alone reads: an error reading it says it, on the Settings page and
// in the status banner of every page.
func (s *Server) redactPaths(text, l string) string {
	if s.cfg.Incidents == "" || text == "" {
		return text
	}
	return strings.ReplaceAll(text, s.cfg.Incidents, tr(l, "the incidents file"))
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
				row.About = "Nothing received in two days: is its job still running?"
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

	// The impact signal.
	c, err := s.criterion(ctx)
	if err != nil {
		return nil, err
	}
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
			imp.State, imp.About = "quiet", "Not read for longer than two runs: are the runs running?"
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
