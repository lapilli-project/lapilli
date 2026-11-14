package serve

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"lapilli.dev/internal/ledger"
)

// A quiet window's preview (docs/design/ui.md): the form to add or change
// one has a Preview button, on the same POST, that writes nothing. Checked
// as saving it is, by the same handler (the role, the team, the same
// site), the page comes back with the form as typed, and what the window
// would do: the rules it applies to, the firings of the last four weeks
// it would have excused, and when it falls in the next four.

// What a preview reads and says, at most: the firings of the last four
// weeks, in one read; the rules it names; the repeats ahead.
const (
	previewFirings = 20_000
	previewRules   = 50
	previewStarts  = 31
	previewSpan    = 28 * 24 * time.Hour
)

// windowDraft is a quiet window's form, as kept or as typed: each field as
// the form's input takes it; Pick, the teams the reader may pick.
type windowDraft struct {
	ID                                                 int64
	Title, Start, End, Every, On, Until, Rules, Labels string
	Team                                               int64
	Pick                                               teamPick
}

// draftOf is a window's form.
func draftOf(w ledger.Window, loc *time.Location, pick teamPick) windowDraft {
	d := windowDraft{ID: w.ID, Title: w.Title, Every: w.Every, On: w.On, Team: w.TeamID, Rules: w.Rules, Labels: labelText(w.Labels), Pick: pick}
	if !w.Start.IsZero() {
		d.Start, d.End = w.Start.In(loc).Format("2006-01-02T15:04"), w.End.In(loc).Format("2006-01-02T15:04")
	}
	if !w.Until.IsZero() {
		d.Until = w.Until.In(loc).Format("2006-01-02")
	}
	return d
}

// windowPreview is what a quiet window would do: Rules, the rules it
// applies to (by their names, team and labels, or by firings it would have
// excused), each with how many, MoreRules past previewRules; Excused, the
// firings that caught nothing it would have excused over [From, To),
// Already of them excused already (by another window, or by this one as
// it is kept); Cut, that the four weeks had
// more firings than were read, Read; Starts, its repeats starting in the
// next four weeks, MoreStarts past previewStarts.
type windowPreview struct {
	From, To         time.Time
	Rules            []previewRule
	MoreRules        int
	Excused, Already int
	Read             int
	Cut              bool
	Starts           []previewStart
	MoreStarts       bool
	Loc              *time.Location
}

type previewRule struct {
	Name    string
	Excused int
}

type previewStart struct{ Start, End time.Time }

// previewWindow answers a Preview: the page the form was on, the form as
// typed (as the window would be kept), and what it would do. A window that
// couldn't be kept is refused as saving it would be.
func (s *Server) previewWindow(w http.ResponseWriter, r *http.Request, x ledger.Window) {
	ctx := r.Context()
	pw, err := s.l.PreviewWindow(ctx, x)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	p, err := s.windowPreviewOf(r, pw)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if x.ID == 0 {
		v, err := s.windowsPage(r)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		ws := v.(windowsView)
		ws.Draft, ws.Preview = draftOf(pw, s.cfg.Loc, ws.Pick), p
		s.render(w, r, "windows", ws)
		return
	}
	v, err := s.windowViewOf(r, x.ID)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	v.Draft, v.Preview = draftOf(pw, s.cfg.Loc, v.Pick), p
	s.render(w, r, "window", v)
}

// windowPreviewOf is what a window, as PreviewWindow gives it, would do.
func (s *Server) windowPreviewOf(r *http.Request, w ledger.Window) (*windowPreview, error) {
	ctx := r.Context()
	now := s.now()
	p := &windowPreview{From: now.Add(-previewSpan), To: now, Loc: s.cfg.Loc}
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: p.From, To: now.Add(time.Second), Limit: previewFirings + 1})
	if err != nil {
		return nil, err
	}
	if len(fs) > previewFirings {
		fs, p.Cut = fs[:previewFirings], true
	}
	p.Read = len(fs)
	// The window's repeats over the span worked out once, as judging does.
	excuses := ledger.Excusing([]ledger.Window{w}, p.From, now)
	by := map[string]int{}
	for _, f := range fs {
		if f.White || !excuses(f.Firing) {
			continue
		}
		p.Excused++
		by[f.Rule]++
		if f.Excused {
			p.Already++
		}
	}
	// The rules in effect it applies to by their own names, team and
	// labels, whether or not they fired.
	meta, err := s.ruleMeta(ctx)
	if err != nil {
		return nil, err
	}
	for name, m := range meta {
		if _, ok := by[name]; !ok && w.Applies(name, m.labels) {
			by[name] = 0
		}
	}
	for name, n := range by {
		p.Rules = append(p.Rules, previewRule{name, n})
	}
	slices.SortFunc(p.Rules, func(a, b previewRule) int { return cmp.Or(b.Excused-a.Excused, cmp.Compare(a.Name, b.Name)) })
	if len(p.Rules) > previewRules {
		p.Rules, p.MoreRules = p.Rules[:previewRules], len(p.Rules)-previewRules
	}
	// Its repeats as judging covers them (Covers), not by the calendar
	// alone.
	falls := w.Falls(now, now.Add(previewSpan), previewStarts+1)
	if len(falls) > previewStarts {
		falls, p.MoreStarts = falls[:previewStarts], true
	}
	for _, f := range falls {
		p.Starts = append(p.Starts, previewStart{f.Start, f.End})
	}
	return p, nil
}
