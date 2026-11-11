package serve

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// The reports' pages and API: everyone sees them and their previews;
// admins make, change and remove them, as a report sends the ledger's
// numbers somewhere, as the settings do.
func (s *Server) reportRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	mux.HandleFunc("GET /reports", page("reports", s.reportsPage))
	mux.HandleFunc("GET /reports/{id}", page("report", s.reportPage))
	mux.HandleFunc("POST /reports", s.admin(s.postReport))
	mux.HandleFunc("POST /reports/{id}/delete", s.admin(s.deleteReport))

	mux.HandleFunc("GET /api/v1/reports", s.cached(s.apiScheduleds))
	mux.HandleFunc("GET /api/v1/reports/{id}", s.cached(s.apiScheduled))
	mux.HandleFunc("GET /api/v1/reports/{id}/preview", s.cached(s.apiPreview))
	mux.HandleFunc("POST /api/v1/reports", s.admin(s.apiPutScheduled))
	mux.HandleFunc("PUT /api/v1/reports/{id}", s.admin(s.apiPutScheduled))
	mux.HandleFunc("DELETE /api/v1/reports/{id}", s.admin(s.apiDelete(func(r *http.Request, id int64) error {
		return s.l.DeleteScheduled(r.Context(), id, s.user(r))
	})))
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
	// Stopped says why it doesn't send: "off", or "team removed".
	Stopped string `json:"stopped,omitempty"`
	// Unknown are the destinations it names that nothing names now.
	Unknown []string `json:"unknown_dests,omitempty"`
}

// digestSections are what the weekly digest holds.
var digestSections = []string{"summary", "noise", "missed", "woken", "notifications", "replay", "sources", "tasks", "suggestions"}

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

// digestLine is the weekly digest, as a report the flags define: nil when
// it goes nowhere.
func (s *Server) digestLine(ctx context.Context) *reportLine {
	var dests []string
	if s.cfg.Slack != "" {
		dests = append(dests, "slack")
	}
	if s.cfg.NotifyWebhook != "" {
		dests = append(dests, "webhook")
	}
	for _, team := range slices.Sorted(maps.Keys(s.cfg.TeamDigests)) {
		dests = append(dests, "team:"+team)
	}
	if len(dests) == 0 {
		return nil
	}
	d := &reportLine{Scheduled: ledger.Scheduled{Name: "weekly digest", Schedule: "weekly " + s.cfg.Digest.String(), Zone: "ledger",
		Sections: digestSections, Dests: dests, Enabled: true}, SetBy: "flags"}
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
	Teams []ledger.Team
	// Dests are the destinations a report can go to; Sections, what it
	// can hold.
	Dests, Sections, Default []string
	Most                     int
}

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
		Default: ledger.DefaultSections, Most: ledger.MaxScheduled}
	if edit := r.URL.Query().Get("edit"); edit != "" {
		for i := range v.Lines {
			if v.Lines[i].SetBy == "" && strconv.FormatInt(v.Lines[i].ID, 10) == edit {
				v.Edit = &v.Lines[i]
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
// nothing names.
func (s *Server) putReport(r *http.Request, x ledger.Scheduled) (int64, error) {
	if err := s.knowsDests(x); err != nil {
		return 0, err
	}
	return s.l.PutScheduled(r.Context(), x, s.user(r), s.now())
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
