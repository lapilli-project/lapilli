package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// The reports' pages and API: everyone sees them and their previews;
// admins make, change and remove them, as a report sends the ledger's
// numbers somewhere, as the settings do; a team's editors, and editors,
// a team's report, sent to the team's own destination
// (docs/design/serveops.md). The weekly digest is an admin's.
func (s *Server) reportRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	mux.HandleFunc("GET /reports", page("reports", s.reportsPage))
	mux.HandleFunc("GET /reports/digest", page("report", s.digestPage))
	mux.HandleFunc("GET /reports/{id}", page("report", s.reportPage))
	mux.HandleFunc("POST /reports", s.reportWrite(s.postReport))
	mux.HandleFunc("POST /reports/digest", s.admin(s.postDigestSet))
	mux.HandleFunc("POST /reports/digest/delete", s.admin(s.deleteDigestSet))
	mux.HandleFunc("POST /reports/{id}/delete", s.reportWrite(s.deleteReport))

	mux.HandleFunc("GET /api/v1/reports", s.cached(s.apiScheduleds))
	mux.HandleFunc("GET /api/v1/reports/digest", s.cached(s.apiDigest))
	mux.HandleFunc("GET /api/v1/reports/digest/preview", s.cached(s.apiDigestPreview))
	mux.HandleFunc("GET /api/v1/reports/{id}", s.cached(s.apiScheduled))
	mux.HandleFunc("GET /api/v1/reports/{id}/preview", s.cached(s.apiPreview))
	mux.HandleFunc("POST /api/v1/reports", s.reportWrite(s.apiPutScheduled))
	mux.HandleFunc("PUT /api/v1/reports/digest", s.admin(s.apiPutDigest))
	mux.HandleFunc("DELETE /api/v1/reports/digest", s.admin(s.apiDelete(func(r *http.Request, _ int64) error {
		return s.backToFlags(r.Context(), s.user(r))
	})))
	mux.HandleFunc("PUT /api/v1/reports/{id}", s.reportWrite(s.apiPutScheduled))
	mux.HandleFunc("DELETE /api/v1/reports/{id}", s.reportWrite(s.apiDelete(func(r *http.Request, id int64) error {
		return s.l.DeleteScheduled(r.Context(), id, s.user(r))
	})))
}

// reportWrite is a handler that changes the reports: an admin's, any; an
// editor's, a team's; a team's editor's, their team's. The ledger refuses
// the rest in the write itself.
func (s *Server) reportWrite(h http.HandlerFunc) http.HandlerFunc {
	return s.teamWrite(func(w http.ResponseWriter, r *http.Request) {
		if _, limited := scoped(r); !limited && s.role(r) < admin {
			own, err := s.owners(r.Context())
			if err != nil {
				s.oops(w, r, err)
				return
			}
			var all []int64
			for _, t := range own.teams {
				all = append(all, t.ID)
			}
			r = r.WithContext(ledger.WithTeams(r.Context(), all))
		}
		h(w, r)
	})
}

// reportLine is a report as the pages and the API show it.
type reportLine struct {
	ledger.Scheduled
	// SetBy is "flags" for the weekly digest, which --digest,
	// --team-digest and --slack-webhook-file define.
	SetBy string `json:"set_by,omitempty"`
	// TeamName is its team's name.
	TeamName string `json:"team,omitempty"`
	// LastSent is the end of the last span it was sent for.
	LastSent time.Time `json:"last_span_end,omitzero"`
	// Kind is "digest" for the weekly digest.
	Kind string `json:"kind,omitempty"`
	// Stopped says why it doesn't send: "off", or "team removed".
	Stopped string `json:"stopped,omitempty"`
	// Unknown are the destinations it names that nothing names now.
	Unknown []string `json:"unknown_dests,omitempty"`
}

// digestSections are what the weekly digest holds.
var digestSections = []string{"summary", "noise", "missed", "woken", "refired", "deploys", "notifications", "replay", "sources", "tasks", "suggestions"}

// reportLines are the reports kept, and first, when it sends anywhere, the
// weekly digest the flags define.
func (s *Server) reportLines(ctx context.Context) ([]reportLine, error) {
	rs, err := s.l.Scheduleds(ctx)
	if err != nil {
		return nil, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	var out []reportLine
	if d := s.digestLine(ctx); d != nil {
		out = append(out, *d)
	}
	known := s.reportDests()
	for _, r := range rs {
		l := reportLine{Scheduled: r}
		if team, ok := reportTeam(r, own); !ok {
			l.Stopped = "team removed"
		} else if team != nil {
			l.TeamName = team.Name
		}
		if !r.Enabled && l.Stopped == "" {
			l.Stopped = "off"
		}
		for _, d := range r.Dests {
			if !slices.Contains(known, d) {
				l.Unknown = append(l.Unknown, d)
			}
		}
		sent, err := s.l.Told(ctx, ledger.ReportSentKey(r.ID))
		if err != nil {
			return nil, err
		}
		if n, err := strconv.ParseInt(sent, 10, 64); err == nil {
			l.LastSent = time.Unix(n, 0).UTC()
		}
		out = append(out, l)
	}
	return out, nil
}

// digestLine is the weekly digest, as a report the flags or the ledger
// define: nil when the flags define it, it goes nowhere, and nothing names
// anywhere it could go, to set it to.
func (s *Server) digestLine(ctx context.Context) *reportLine {
	d := s.digestLineAll(ctx)
	if d.SetBy == "flags" && len(d.Dests) == 0 && !s.sendsReports() {
		return nil
	}
	return d
}

// digestLineAll is the weekly digest, whether it goes anywhere or not.
func (s *Server) digestLineAll(ctx context.Context) *reportLine {
	in, err := s.digestNow(ctx)
	if err != nil {
		s.log.Warn("the weekly digest as set in the ledger can't be read: the flags' is shown", "err", err)
	}
	d := &reportLine{Scheduled: ledger.Scheduled{Name: "weekly digest", Schedule: "weekly " + in.When.String(), Zone: "ledger",
		Sections: digestSections, Dests: in.Dests, Enabled: in.On}, SetBy: "flags", Kind: "digest"}
	if d.Dests == nil {
		d.Dests = []string{}
	}
	if in.Set != nil {
		d.SetBy, d.Updated, d.UpdatedBy = "ledger", in.Set.Updated, in.Set.By
		if !in.On {
			d.Stopped = "off"
		}
		known := s.reportDests()
		for _, x := range in.Dests {
			if !slices.Contains(known, x) {
				d.Unknown = append(d.Unknown, x)
			}
		}
	}
	// Posted to Slack, or told through the outbox.
	sent, _ := s.l.Meta(ctx, "digest-sent")
	told, _ := s.l.Told(ctx, "digest-told")
	for _, v := range []string{sent, told} {
		if week, err := strconv.ParseInt(v, 10, 64); err == nil {
			if to := s.l.Midnight(week); to.After(d.LastSent) {
				d.LastSent = to
			}
		}
	}
	return d
}

type reportsView struct {
	Lines []reportLine
	Edit  *reportLine
	// Teams are those the reader may make a report for; Everyone, whether
	// a report for everyone too (an admin).
	Teams    []ledger.Team
	Everyone bool
	// Digest is the weekly digest, to change, with where it can go.
	Digest      *reportLine
	DigestDests []string
	// Dests are the destinations a report can go to; Sections, what it
	// can hold.
	Dests, Sections, Default []string
	Most                     int
}

// mayEdit reports whether the reader may change a report: an admin any; who
// isn't, one of a team they may pick, sent only to the team's own
// destination.
func (v reportsView) mayEdit(l reportLine) bool {
	if v.Everyone {
		return true
	}
	i := slices.IndexFunc(v.Teams, func(t ledger.Team) bool { return t.ID == l.TeamID })
	return l.TeamID > 0 && i >= 0 && !slices.ContainsFunc(l.Dests, func(d string) bool { return d != "team:"+v.Teams[i].Name })
}

// MayEdit is mayEdit, for the page.
func (v reportsView) MayEdit(l reportLine) bool { return v.mayEdit(l) }

func (s *Server) reportsPage(r *http.Request) (any, error) {
	ctx := r.Context()
	lines, err := s.reportLines(ctx)
	if err != nil {
		return nil, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	v := reportsView{Lines: lines, Teams: own.teams, Dests: s.reportDests(), Sections: ledger.ReportSections,
		Default: ledger.DefaultSections, Most: ledger.MaxScheduled, Everyone: true}
	// Who isn't an admin makes a team's report, to its own destination.
	if s.role(r) < admin {
		v.Everyone, v.Teams, v.Dests = false, nil, nil
		edits := s.editsTeams(r)
		for _, t := range own.teams {
			if s.role(r) == editor || slices.Contains(edits, t.ID) {
				v.Teams = append(v.Teams, t)
				if d := "team:" + t.Name; slices.Contains(s.reportDests(), d) {
					v.Dests = append(v.Dests, d)
				}
			}
		}
	}
	switch edit := r.URL.Query().Get("edit"); {
	case edit == "digest" && s.role(r) == admin:
		v.Digest = s.digestLineAll(r.Context())
		v.DigestDests = s.reportDests()
	case edit != "":
		for i := range v.Lines {
			l := &v.Lines[i]
			if l.SetBy == "" && strconv.FormatInt(l.ID, 10) == edit && v.mayEdit(*l) {
				v.Edit = l
			}
		}
	}
	return v, nil
}

type reportView struct {
	reportLine
	Preview *built
	// Why is why there is no preview.
	Why string
}

func (s *Server) reportPage(r *http.Request) (any, error) {
	line, err := s.reportByID(r.Context(), pathID(r))
	if err != nil {
		return nil, err
	}
	v := reportView{reportLine: line}
	v.Preview, err = s.preview(r.Context(), line.Scheduled)
	if err != nil {
		v.Why = message(lang(r), err)
	}
	return v, nil
}

// reportByID is one report, as the pages show it.
func (s *Server) reportByID(ctx context.Context, id int64) (reportLine, error) {
	lines, err := s.reportLines(ctx)
	if err != nil {
		return reportLine{}, err
	}
	for _, l := range lines {
		if l.SetBy == "" && l.ID == id && id != 0 {
			return l, nil
		}
	}
	return reportLine{}, ledger.ErrNotFound
}

// putReport keeps a report people made or changed, refusing destinations
// nothing names; and, from who isn't an admin, any but the report's own
// team's.
func (s *Server) putReport(r *http.Request, x ledger.Scheduled) (int64, error) {
	if err := s.knowsDests(x); err != nil {
		return 0, err
	}
	if _, limited := scoped(r); limited {
		own, err := s.owners(r.Context())
		if err != nil {
			return 0, err
		}
		for _, d := range x.Dests {
			if t, ok := own.byID[x.TeamID]; !ok || d != "team:"+t.Name {
				return 0, errTeamDest
			}
		}
	}
	return s.l.PutScheduled(r.Context(), x, s.user(r), s.now())
}

// errTeamDest is a team's report, from who isn't an admin, sent elsewhere
// than to the team's own destination.
var errTeamDest = errors.New("a team's report goes to its team's own destination (team:NAME), unless an admin sends it elsewhere")

// digestPage is the weekly digest, and how it would be sent now.
func (s *Server) digestPage(r *http.Request) (any, error) {
	v := reportView{reportLine: *s.digestLineAll(r.Context())}
	var err error
	if v.Preview, err = s.digestPreview(r.Context()); err != nil {
		v.Why = message(lang(r), err)
	}
	return v, nil
}

func (s *Server) postDigestSet(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	d := ledger.DigestSet{Schedule: r.PostFormValue("schedule"), Dests: r.PostForm["dests"], Enabled: r.PostFormValue("enabled") == "1"}
	s.written(w, r, s.putDigest(r.Context(), d, s.user(r)), "/reports/digest")
}

func (s *Server) deleteDigestSet(w http.ResponseWriter, r *http.Request) {
	s.written(w, r, s.backToFlags(r.Context(), s.user(r)), "/reports/digest")
}

func (s *Server) apiDigest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.digestLineAll(r.Context()))
}

func (s *Server) apiDigestPreview(w http.ResponseWriter, r *http.Request) {
	b, err := s.digestPreview(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, b.about())
}

// apiPutDigest sets the weekly digest: when (weekly DAY@HH:MM), where to,
// and whether it is sent. The rest is the digest's own.
func (s *Server) apiPutDigest(w http.ResponseWriter, r *http.Request) {
	d := ledger.DigestSet{Enabled: true} // unless it says otherwise
	if err := readJSON(r, &d); err != nil {
		s.oops(w, r, err)
		return
	}
	if err := s.putDigest(r.Context(), d, s.user(r)); err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, s.digestLineAll(r.Context()))
}

func (s *Server) postReport(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	x := ledger.Scheduled{ID: formInt(r, "id"), Name: r.PostFormValue("name"), TeamID: formInt(r, "team"),
		Schedule: r.PostFormValue("schedule"), Span: strings.TrimSpace(r.PostFormValue("span")), Zone: r.PostFormValue("zone"),
		Sections: r.PostForm["sections"], Dests: r.PostForm["dests"], Enabled: r.PostFormValue("enabled") == "1"}
	id, err := s.putReport(r, x)
	s.written(w, r, err, fmt.Sprintf("/reports/%d", id))
}

func (s *Server) deleteReport(w http.ResponseWriter, r *http.Request) {
	s.written(w, r, s.l.DeleteScheduled(r.Context(), pathID(r), s.user(r)), "/reports")
}

func (s *Server) apiScheduleds(w http.ResponseWriter, r *http.Request) {
	lines, err := s.reportLines(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, orNoneOf(lines))
}

func (s *Server) apiScheduled(w http.ResponseWriter, r *http.Request) {
	line, err := s.reportByID(r.Context(), pathID(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, line)
}

// apiPreview is a report as it would be sent now, for its last span, sent
// nowhere: what report.ready would say.
func (s *Server) apiPreview(w http.ResponseWriter, r *http.Request) {
	line, err := s.reportByID(r.Context(), pathID(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	b, err := s.preview(r.Context(), line.Scheduled)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, b.about())
}

func (s *Server) apiPutScheduled(w http.ResponseWriter, r *http.Request) {
	x := ledger.Scheduled{Enabled: true} // unless it says otherwise
	if err := readJSON(r, &x); err != nil {
		s.oops(w, r, err)
		return
	}
	x.ID = 0
	if r.Method == http.MethodPut {
		var err error
		if x.ID, err = putID(r); err != nil {
			s.oops(w, r, err)
			return
		}
	}
	id, err := s.putReport(r, x)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	line, err := s.reportByID(r.Context(), id)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	answer(w, r, fmt.Sprintf("/api/v1/reports/%d", id), line)
}

// stoppedReports are the names of the reports whose team was removed.
func (s *Server) stoppedReports(ctx context.Context) ([]string, error) {
	rs, err := s.l.Scheduleds(ctx)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rs {
		if _, ok := reportTeam(r, own); !ok {
			out = append(out, r.Name)
		}
	}
	return out, nil
}
