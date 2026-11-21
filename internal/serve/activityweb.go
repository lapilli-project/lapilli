package serve

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lapilli.dev/internal/ledger"
)

// The activity log (docs/design/serveops.md, "The activity log"): every
// change kept, filtered by who made it, its kind, its team and its period,
// a page at a time past the hundred Run history shows, and exported as CSV
// or JSON with the same filters. Anyone who reads Run history reads it,
// as there: the roles given and taken are an admin's alone.

// auditRoutes are the Sources page, the activity log, its export, and the
// API's daily writes (apidaily.go).
func (s *Server) auditRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	mux.HandleFunc("GET /sources", page("sourcelist", s.sourcesPage))
	mux.HandleFunc("GET /activity", page("activity", s.activityPage))
	mux.HandleFunc("GET /api/v1/activity", s.apiActivity)
	mux.HandleFunc("POST /api/v1/incidents/{id}/decide", s.write(s.apiDecide))
	mux.HandleFunc("POST /api/v1/firings/real", s.write(s.apiReal))
}

// apiSourcesOf is GET /api/v1/sources: with all=true every source, as the
// Sources page shows it, worked out afresh, as how each was last read is
// no write to the ledger that a kept answer would be told by; without, the
// sources firings are copied in from, as before, kept.
func (s *Server) apiSourcesOf() http.HandlerFunc {
	kept := s.cached(s.apiSources)
	return func(w http.ResponseWriter, r *http.Request) {
		switch v := r.URL.Query().Get("all"); v {
		case "true":
			s.apiSourcesAll(w, r)
		case "", "false":
			kept(w, r)
		default:
			s.oops(w, r, bad("all %q: want true or false", v))
		}
	}
}

// How much of the activity log one request reads.
const (
	// activityShown is a page's changes.
	activityShown = 100
	// activityMost is the most an export gives at once: past it, it says
	// where to go on from.
	activityMost = 1000
	// activityBatch is how many changes, with their before and after, a
	// request asks the ledger for at once.
	activityBatch = 200
	// activityExports is how many exports are written out at once: each
	// holds its answer, activityBytes and a change at most, until its
	// reader has read it.
	activityExports = 4
	// activitySmall is the most changes an export takes no turn for:
	// activitySmallBytes at most of its answer.
	activitySmall      = 50
	activitySmallBytes = 1 << 20
	// activityScan is how many changes a request looks through for those
	// of a team, or those its reader may read, before it says where it
	// stopped, so that a filter that matches little reads no more than
	// this.
	activityScan = 20000
	// activityLookups is how many tasks and proposals a request looks up
	// for their team, before it says where it stopped: a page of a team's
	// changes asks the ledger this many rows at most besides.
	activityLookups = 200
)

// activityBytes is how much of the changes' text, who, what, before and
// after, a page or an export holds at most: past it, it ends at the change
// before and says where to go on from, so that a thousand changes of an
// incident's labels and alerts, tens of megabytes, are a few pages. A
// change bigger than it alone is given whole, as the first of its page.
var activityBytes = 8 << 20

// changeText is how much of a change's text a page holds.
func changeText(c ledger.LogEntry) int { return len(c.By) + len(c.What) + len(c.Before) + len(c.After) }

// readActivity is the ledger's Activity; replaced in tests, to count.
var readActivity = (*ledger.Ledger).Activity

// errLookedUp is a request of a team's changes past activityLookups.
var errLookedUp = errors.New("looked up as many tasks and proposals as a request may")

// activityAsk is what a request asks of the activity log.
type activityAsk struct {
	q ledger.ActivityQuery
	// team is the team asked for, by name, and teamID its id.
	team   string
	teamID int64
	// Asked as the request gave them, for the page's form and its links.
	who, kind, since, until string
}

// activityAsked reads what a request asks of the activity log: who (the
// name a change is noted under, whole), kind (one of ledger.ActivityKinds,
// or other), team (a team's name), since and until (a day, 2026-11-02, in
// the UI's zone, until taking the day whole; or an RFC 3339 time), and
// before (where the page before ended).
func (s *Server) activityAsked(ctx context.Context, q url.Values) (activityAsk, error) {
	a := activityAsk{who: strings.TrimSpace(q.Get("who")), kind: q.Get("kind"), since: q.Get("since"), until: q.Get("until")}
	if len(a.who) > ledger.MaxName {
		return a, bad("who is %d bytes at most", ledger.MaxName)
	}
	a.q.By = a.who
	if a.kind != "" && !ledger.KnownKind(a.kind) {
		return a, bad("kind %q: want one of the activity log's kinds", a.kind)
	}
	a.q.Kind = a.kind
	if name := q.Get("team"); name != "" {
		o, err := s.owners(ctx)
		if err != nil {
			return a, err
		}
		t, ok := o.team(name)
		if !ok {
			return a, bad("no team called %q", name)
		}
		a.team, a.teamID = t.Name, t.ID
	}
	when := func(name, v string, end bool) (time.Time, error) {
		if v == "" {
			return time.Time{}, nil
		}
		if d, err := time.ParseInLocation("2006-01-02", v, s.cfg.Loc); err == nil {
			if end {
				d = d.AddDate(0, 0, 1)
			}
			return d, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, bad("%s %q: want a day, like 2026-11-02, or an RFC 3339 time", name, v)
		}
		return t, nil
	}
	var err error
	if a.q.Since, err = when("since", a.since, false); err != nil {
		return a, err
	}
	if a.q.Until, err = when("until", a.until, true); err != nil {
		return a, err
	}
	if !a.q.Since.IsZero() && !a.q.Until.IsZero() && !a.q.Until.After(a.q.Since) {
		return a, bad("until must be after since")
	}
	if a.q.Before, err = ledger.ParseActivityCursor(q.Get("before")); err != nil {
		return a, bad("before %q: want where a page ended, as its link gives it", q.Get("before"))
	}
	return a, nil
}

// activityRead is a page of the activity log: its changes, the latest
// first, limit of them or activityBytes of their text at most; where the
// next page goes on from (zero for none); and Cut, that the request looked
// through activityScan changes without filling the page, so that more may
// be past Next.
type activityRead struct {
	Changes []ledger.LogEntry
	Next    ledger.ActivityCursor
	Cut     bool
}

// activity reads, of what a asks, limit changes at most, those the reader
// may read: the roles given and taken are an admin's alone, as on Run
// history (changesSeen), and every text is said to anyone but an admin
// without the paths of serve's files.
func (s *Server) activity(ctx context.Context, a activityAsk, limit int, adminReads bool, l string) (activityRead, error) {
	var out activityRead
	if !adminReads && a.q.Kind == "role" {
		return out, nil
	}
	o, err := s.owners(ctx)
	if err != nil {
		return out, err
	}
	tasks, proposals := map[int64]bool{}, map[int64]bool{}
	lookups := 0
	inTeam := func(c ledger.LogEntry) (bool, error) {
		if a.team == "" {
			return true, nil
		}
		if c.Kind == "team" {
			rest := strings.TrimPrefix(c.What, "team ")
			if hasPrefixFold(rest, a.team+" ") || strings.EqualFold(rest, a.team) {
				return true, nil
			}
			if _, to, ok := strings.Cut(rest, " renamed to "); ok && strings.EqualFold(to, a.team) {
				return true, nil
			}
			return false, nil
		}
		rules, task, proposal := ledger.ChangeSubject(c.What, c.After)
		for _, r := range rules {
			if o.teamOf(r) == a.team {
				return true, nil
			}
		}
		if task != 0 {
			in, ok := tasks[task]
			if !ok {
				if lookups++; lookups > activityLookups {
					return false, errLookedUp
				}
				t, err := s.l.TaskByID(ctx, task)
				if err != nil && !isNotFound(err) {
					return false, err
				}
				in = err == nil && t.TeamID == a.teamID
				tasks[task] = in
			}
			return in, nil
		}
		if proposal != 0 {
			in, ok := proposals[proposal]
			if !ok {
				if lookups++; lookups > activityLookups {
					return false, errLookedUp
				}
				p, err := s.l.ProposalByID(ctx, proposal)
				if err != nil && !isNotFound(err) {
					return false, err
				}
				in = err == nil && o.teamOf(p.Rule) == a.team
				proposals[proposal] = in
			}
			return in, nil
		}
		return false, nil
	}
	q := a.q
	// A team's changes are a few of those looked through: each one's before
	// and after are read once it is kept, not for every one looked at, a
	// batch at a time, until activityBytes. Every change's otherwise. A
	// batch a read either way, as a bulk verdict's after, which says its
	// rules, is read with each looked through.
	q.Light = a.team != ""
	q.Limit = activityBatch
	held := 0 // the text of the changes kept
	done := func() (activityRead, error) {
		if q.Light {
			for i := 0; i < len(out.Changes); i += activityBatch {
				part := out.Changes[i:min(i+activityBatch, len(out.Changes))]
				if err := s.l.ActivityBodies(ctx, part); err != nil {
					return out, err
				}
				for j, c := range part {
					if held += changeText(c); held > activityBytes && i+j > 0 {
						out.Changes = out.Changes[:i+j]
						out.Next, out.Cut = out.Changes[i+j-1].Cursor(), false
						return s.activitySaid(out, adminReads, l), nil
					}
				}
			}
		}
		return s.activitySaid(out, adminReads, l), nil
	}
	scanned := 0
	last := q.Before // the last change looked at
	for {
		got, err := readActivity(s.l, ctx, q)
		if err != nil {
			return out, err
		}
		for _, c := range got {
			scanned++
			if !adminReads && c.Kind == "role" {
				last = c.Cursor()
				continue
			}
			ok, err := inTeam(c)
			if errors.Is(err, errLookedUp) && last.ID != 0 {
				// Stopped before it: the next page looks it up afresh.
				out.Next, out.Cut = last, true
				return done()
			} else if err != nil && !errors.Is(err, errLookedUp) {
				return out, err
			}
			last = c.Cursor()
			if !ok {
				continue
			}
			if len(out.Changes) == limit || !q.Light && len(out.Changes) > 0 && held+changeText(c) > activityBytes {
				// One more than the page holds: the page ends at its last.
				out.Next = out.Changes[len(out.Changes)-1].Cursor()
				return done()
			}
			if !q.Light {
				held += changeText(c)
			}
			out.Changes = append(out.Changes, c)
		}
		if len(got) < q.Limit {
			return done() // no more
		}
		q.Before = got[len(got)-1].Cursor()
		if scanned >= activityScan {
			out.Next, out.Cut = q.Before, true
			return done()
		}
	}
}

// activitySaid is a page of the activity log as its reader may read it,
// in language l: to anyone but an admin, without the paths of serve's
// files, as Run history says a run's error (redactPaths).
func (s *Server) activitySaid(out activityRead, adminReads bool, l string) activityRead {
	if adminReads || len(out.Changes) == 0 {
		return out
	}
	// The paths worked out once a page, each text looked through only
	// when it holds one: a page of bulk verdicts is megabytes.
	ps := s.filePaths()
	hide := func(text string) string {
		for _, p := range ps {
			if strings.Contains(text, p.path) {
				return hidePaths(text, ps, l)
			}
		}
		return text
	}
	for i, c := range out.Changes {
		out.Changes[i].What = hide(c.What)
		out.Changes[i].Before = hide(c.Before)
		out.Changes[i].After = hide(c.After)
	}
	return out
}

// isNotFound reports whether err is a row that isn't there.
func isNotFound(err error) bool { return errors.Is(err, ledger.ErrNotFound) }

// hasPrefixFold is strings.HasPrefix in any letter case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// activityView is the activity log's page.
type activityView struct {
	Rows []activityRow
	// What was asked, for the form, and the kinds and teams it offers.
	Who, Kind, Team, Since, Until string
	Kinds                         []kindOption
	Teams                         []string
	// Paged, that the page goes on from another, and First, the link to
	// the newest with the same filters; Next, the link to the one after;
	// Cut, that it stopped looking (activityScan, activityLookups).
	Paged       bool
	First, Next string
	Cut         bool
	// CSV and JSON are the export with the same filters.
	CSV, JSON string
	// KeepDays is how long the activity log is kept, 0 for all of it.
	KeepDays int
	// RolesHidden says the roles given and taken aren't listed: an
	// admin's to read.
	RolesHidden bool
}

// kindOption is a kind of change the page's form offers, and its name.
type kindOption struct{ Value, Text string }

// activityRow is a change as the page lists it: before and after cut to
// what a page shows (activityTextMost), the rest in the export.
type activityRow struct {
	At                time.Time
	By, Kind, What    string
	Before, After     string
	BeforeCut, AfterC bool
}

// activityTextMost is how much of a change's before and after the page
// shows.
const activityTextMost = 2000

func (s *Server) activityPage(r *http.Request) (any, error) {
	ctx := r.Context()
	a, err := s.activityAsked(ctx, r.URL.Query())
	if err != nil {
		return nil, err
	}
	adminReads := s.role(r) == admin
	got, err := s.activity(ctx, a, activityShown, adminReads, lang(r))
	if err != nil {
		return nil, err
	}
	v := activityView{Who: a.who, Kind: a.kind, Team: a.team, Since: a.since, Until: a.until, Paged: a.q.Before.ID != 0, Cut: got.Cut,
		RolesHidden: !adminReads}
	for _, k := range ledger.ActivityKinds {
		if k.Kind != "role" || adminReads {
			v.Kinds = append(v.Kinds, kindOption{k.Kind, activityKindText[k.Kind]})
		}
	}
	v.Kinds = append(v.Kinds, kindOption{ledger.KindOther, activityKindText[ledger.KindOther]})
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range o.teams {
		v.Teams = append(v.Teams, t.Name)
	}
	cut := func(t string) (string, bool) {
		if len(t) <= activityTextMost {
			return t, false
		}
		i := activityTextMost
		for i > 0 && !utf8Start(t[i]) {
			i--
		}
		return t[:i], true
	}
	for _, c := range got.Changes {
		row := activityRow{At: c.At, By: c.By, Kind: c.Kind, What: c.What}
		row.Before, row.BeforeCut = cut(c.Before)
		row.After, row.AfterC = cut(c.After)
		v.Rows = append(v.Rows, row)
	}
	params := url.Values{}
	for k, x := range map[string]string{"who": a.who, "kind": a.kind, "team": a.team, "since": a.since, "until": a.until} {
		if x != "" {
			params.Set(k, x)
		}
	}
	v.First = "/activity"
	if len(params) > 0 {
		v.First += "?" + params.Encode()
	}
	if got.Next.ID != 0 {
		next := url.Values{}
		for k, x := range params {
			next[k] = x
		}
		next.Set("before", got.Next.String())
		v.Next = "/activity?" + next.Encode()
	}
	export := url.Values{}
	for k, x := range params {
		export[k] = x
	}
	export.Set("format", "csv")
	v.CSV = "/api/v1/activity?" + export.Encode()
	export.Set("format", "json")
	v.JSON = "/api/v1/activity?" + export.Encode()
	if s.cfg.Keep > 0 && s.cfg.KeepActivity > 0 {
		v.KeepDays = int(max(s.cfg.Keep, s.cfg.KeepActivity) / (24 * time.Hour))
	}
	return v, nil
}

// utf8Start reports whether b starts a UTF-8 character.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// apiLogged is a change as the API gives it.
type apiLogged struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	By     string          `json:"by"`
	Kind   string          `json:"kind"`
	What   string          `json:"what"`
	Before json.RawMessage `json:"before"`
	After  json.RawMessage `json:"after"`
}

// rawOf is a change's before or after as JSON: as kept, which is JSON, or
// null for none, or a string for what doesn't read as JSON.
func rawOf(t string) json.RawMessage {
	switch {
	case t == "":
		return json.RawMessage("null")
	case json.Valid([]byte(t)):
		return json.RawMessage(t)
	}
	b, _ := json.Marshal(t)
	return b
}

// apiActivity is GET /api/v1/activity: the activity log as the page
// filters it (who, kind, team, since, until, before), limit changes (1 to
// activityMost, activityMost unless given), as JSON (activity, and next,
// where to go on from) or, with format=csv, as CSV, the next page's link
// in Link. Its reader is the page's: a viewer, roles an admin's.
func (s *Server) apiActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a, err := s.activityAsked(r.Context(), q)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	limit := activityMost
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > activityMost {
			s.oops(w, r, bad("limit %q: want 1 to %d", v, activityMost))
			return
		}
		limit = n
	}
	format := q.Get("format")
	if format != "" && format != "json" && format != "csv" {
		s.oops(w, r, bad("format %q: want json or csv", format))
		return
	}
	// Written out a few at a time (activityExports): an export holds its
	// answer until its reader has read it, which one who doesn't read makes
	// take until the write's deadline, and its turn with it, so that what
	// such readers make serve hold is that of a few exports, not of each
	// one sent. The turn is one of the exports' own, not of the pages':
	// those readers keep no page from being worked out. A person has one
	// export at a time, so that one viewer can't hold every turn; a small
	// one (activitySmall changes at most) takes no turn, and holds
	// activitySmallBytes at most.
	most := activityBytes
	if limit <= activitySmall {
		most = activitySmallBytes
	} else {
		ex, err := s.exportTurn(r.Context())
		if err != nil {
			s.oops(w, r, err)
			return
		}
		defer ex.give()
	}
	done, ok := s.exportOf(r)
	if !ok {
		s.oops(w, r, errPagesBusy)
		return
	}
	defer done()
	// Worked out as a page is, a few at a time (pageRenders): it takes a
	// page's turn too, and gives it back once the answer is written out,
	// before it goes to the network.
	tn, err := s.renderTurn(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	defer tn.give()
	got, err := s.activity(r.Context(), a, limit, s.role(r) == admin, "en")
	if err != nil {
		s.oops(w, r, err)
		return
	}
	// Who did what is read as it is now: never kept by a browser or a
	// proxy between.
	w.Header().Set("Cache-Control", "no-store")
	// Written out whole, then the page's turn given back, then sent, the
	// export's turn held until it is. The answer is held to most bytes as
	// written, not as kept: JSON writes < > & and U+2028 as six bytes each,
	// and CSV a quote as two. A change past them ends it, but the first,
	// which is given whole, and the answer says where to go on from.
	var body bytes.Buffer
	nextAt, cut := got.Next, got.Cut
	fits := func(i, was int) bool {
		if i == 0 || body.Len() <= most {
			return true
		}
		body.Truncate(was)
		nextAt, cut = got.Changes[i-1].Cursor(), false
		return false
	}
	if format == "csv" {
		cw := csv.NewWriter(&body)
		cw.Write([]string{"id", "at", "by", "kind", "what", "before", "after"})
		cw.Flush()
		for i, c := range got.Changes {
			was := body.Len()
			cw.Write([]string{strconv.FormatInt(c.ID, 10), c.At.UTC().Format(time.RFC3339), csvCell(c.By), c.Kind, csvCell(c.What), csvCell(c.Before), csvCell(c.After)})
			if cw.Flush(); !fits(i, was) {
				break
			}
		}
		if nextAt.ID != 0 {
			w.Header().Set("Link", "<"+s.activityNext(q, nextAt)+`>; rel="next"`)
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="lapilli-activity.csv"`)
	} else {
		// Written a change at a time, as encoding/json would write the
		// whole, not indented: a change's before and after are JSON of
		// their own, which an indent made a third bigger.
		body.WriteString(`{"activity":[`)
		for i, c := range got.Changes {
			was := body.Len()
			b, err := json.Marshal(apiLogged{c.ID, c.At.UTC(), c.By, c.Kind, c.What, rawOf(c.Before), rawOf(c.After)})
			if err != nil {
				s.oops(w, r, err)
				return
			}
			if i > 0 {
				body.WriteByte(',')
			}
			if body.Write(b); !fits(i, was) {
				break
			}
		}
		body.WriteByte(']')
		if nextAt.ID != 0 {
			n, _ := json.Marshal(nextAt.String())
			body.WriteString(`,"next":`)
			body.Write(n)
		}
		if cut {
			body.WriteString(`,"cut":true`)
		}
		body.WriteString("}\n")
		w.Header().Set("Content-Type", "application/json")
	}
	got.Changes = nil
	tn.give()
	// A reader who doesn't read holds the export's turn as long as a page
	// too big to keep may take to go out (spillWithin), not the server's
	// WriteTimeout.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(spillWithin))
	w.Write(body.Bytes())
}

// activityNext is the export's link to the page after the one that ends at
// c, with the same query.
func (s *Server) activityNext(q url.Values, c ledger.ActivityCursor) string {
	nq := url.Values{}
	for k, v := range q {
		nq[k] = v
	}
	nq.Set("before", c.String())
	return "/api/v1/activity?" + nq.Encode()
}

// exportOf marks an export as its reader's: the person, as signed in, or
// the address it came from for anyone not named; false when they have one
// going already. done ends it.
func (s *Server) exportOf(r *http.Request) (done func(), ok bool) {
	who := "user\x00" + ledger.FoldName(s.user(r))
	if s.user(r) == "anonymous" {
		who = "addr\x00" + clientAddr(r)
	}
	s.exporting.Lock()
	defer s.exporting.Unlock()
	if s.exporting.by == nil {
		s.exporting.by = map[string]bool{}
	}
	if s.exporting.by[who] {
		return nil, false
	}
	s.exporting.by[who] = true
	return func() {
		s.exporting.Lock()
		delete(s.exporting.by, who)
		s.exporting.Unlock()
	}, true
}

// csvCell is a cell of the CSV export a spreadsheet won't take for a
// formula (CSV injection, OWASP): one that starts with =, +, -, @, a tab
// or a carriage return, or their full-width forms, after any spaces, starts
// with ' instead, which a spreadsheet shows as text. A space is any of
// Unicode's (a no-break space, an ideographic one) and those of no width,
// as a spreadsheet may take them off before it reads the cell. Who and
// what come from people: a name, a title, a rule's.
func csvCell(v string) string {
	t := strings.TrimLeftFunc(v, func(r rune) bool {
		return unicode.Is(unicode.Zs, r) || r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\u2060' || r == '\ufeff'
	})
	if t == "" {
		return v
	}
	if r, _ := utf8.DecodeRuneInString(t); strings.ContainsRune("=+-@\t\r＝＋－＠", r) {
		return "'" + v
	}
	return v
}

// activityBefore is when the activity log is pruned to: as long as --keep
// at least, --keep-activity when longer, and none with it 0 (Prune keeps
// all of it).
func (s *Server) activityBefore() time.Time {
	if s.cfg.KeepActivity <= 0 {
		return time.Time{}
	}
	return s.now().Add(-max(s.cfg.Keep, s.cfg.KeepActivity))
}

// activityKindText is each kind of change as the page names it, in
// English: the page says it in its language as "activity kind|TEXT".
var activityKindText = map[string]string{
	"setting": "Setting", "incident": "Incident", "verdict": "Verdict", "proposal": "Proposal", "task": "Task",
	"comment": "Comment", "note": "Note", "window": "Quiet window", "marked": "Marked window", "report": "Report",
	"team": "Team", "role": "Role", "goal": "Goal", "firings": "Firings copied in", "cleanup": "Cleanup", ledger.KindOther: "Other",
}

// KindText is a change's kind, as the page names it.
func (r activityRow) KindText() string { return activityKindText[r.Kind] }
