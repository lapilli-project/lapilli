package serve

import (
	"context"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// What the Settings page's Notifications section shows of routing
// (docs/design/ui.md, "Where notices go"): the routes in force, as
// --config's notify: declares them and as this replica read it, read-only,
// changed in the file alone; and each destination an event goes to, when
// a notice to it was last sent, how many wait, how many of the last day's
// failed or were given up on, and, for an admin, why the latest that is
// still unsent failed. Names and counts, never a URL.

// routesView is the routing, and each destination's state.
type routesView struct {
	// File says --config is given; Declared, that routes are in force.
	File, Declared bool
	Rows           []routeRow
	Default        []string
	// At is when the routing in force was read, and Hash the file's.
	At   time.Time
	Hash string
	// Err is why the file last read can't be used for its notify:, in
	// each language, since ErrAt; Line its line, for a reader who isn't
	// an admin. Broken says none could be read since serve started.
	Err    map[string]string
	ErrAt  time.Time
	Line   int
	Broken bool
	// Unknown are the destinations the routes in force name that this
	// replica's flags don't give, left out here, in each language, the
	// first maxUnknownShown; UnknownMore, how many more.
	Unknown     []langText
	UnknownMore int
	// Pending is when a new file was read once: in force once it is read
	// the same again.
	Pending time.Time
	// Unrouted are the events, since serve started, that went where no
	// route sent them.
	Unrouted int64
	Dests    []destView
	// Counted is how many of a destination's waiting are read at most.
	Counted int
}

// ErrText is why the file can't be used, in a language.
func (v *routesView) ErrText(lang string) string { return langText(v.Err).In(lang) }

// langText is a text in each language.
type langText map[string]string

// In is the text in a language, or else in English.
func (m langText) In(lang string) string {
	if t, ok := m[lang]; ok {
		return t
	}
	return m["en"]
}

// maxUnknownShown is how many destinations the flags don't give the page
// names; the rest are counted.
const maxUnknownShown = 10

// routeRow is a route, as the table shows it.
type routeRow struct {
	N, Line             int
	Kinds, Teams, To    []string
	Labels              []string
	Continue            bool
	KindsAny, TeamsAny  bool
	LabelsAny, Template bool
}

// destView is a destination an event goes to, and how its notices went.
type destView struct {
	Name, Flag string
	// JSON says it takes JSON (a webhook); else a line, Slack's way.
	JSON bool
	// Template says it has words of its own; Unnamed, that no event goes
	// to it: no route names it, nor the default, or, without routes, it is
	// neither the webhook nor Slack. Gone, that no flag here gives it any
	// more: what waits for it fails, and is given up on (sweepGone).
	Template, Unnamed, Gone bool
	LastSent                time.Time
	Waiting, Failed         int
	More                    bool
	GivenUp                 int
	GivenUpMore             bool
	// LastErr is why the latest still unsent failed, paths taken out: an
	// admin's to read.
	LastErr string
}

// routesView is what the page shows of routing; nil without --config and
// without a destination a route could name or one gone with notices
// unsent, the outbox's places gone (gonePlaces).
func (s *Server) routesView(ctx context.Context, lang string, gone []string) (*routesView, error) {
	st := &s.routing
	v := &routesView{File: s.cfg.Config != "", Unrouted: st.unrouted.Load(), Broken: st.broken.Load(), Counted: ledger.FailedCounted}
	rt := st.table.Load()
	st.Lock()
	if st.err != nil {
		p := problemOf(st.err)
		v.Err, v.Line, v.ErrAt = p.Text, p.Line, st.errAt
		for l, t := range v.Err {
			v.Err[l] = s.redactPaths(t, l)
		}
	}
	if st.pending != "" {
		v.Pending = st.pendingAt
	}
	st.Unlock()
	var c *notifyConf
	if rt != nil {
		c, v.At = rt.conf, rt.at
		if len(rt.hash) >= 12 {
			v.Hash = rt.hash[:12]
		}
		for i, err := range rt.unknown {
			if i == maxUnknownShown {
				v.UnknownMore = len(rt.unknown) - i
				break
			}
			v.Unknown = append(v.Unknown, problemOf(err).Text)
		}
	}
	var names []string
	if c.declared() {
		v.Declared, v.Default = true, c.Default
		for i, r := range c.Routes {
			row := routeRow{N: i + 1, Line: r.Line, Kinds: r.Kinds, Teams: r.Teams, To: r.To, Continue: r.Continue,
				KindsAny: len(r.Kinds) == 0, TeamsAny: len(r.Teams) == 0, LabelsAny: len(r.Labels) == 0}
			row.Labels = strings.Split(routeLabelsText(r.Labels), ", ")
			if row.LabelsAny {
				row.Labels = nil
			}
			v.Rows = append(v.Rows, row)
			names = append(names, r.To...)
		}
		names = append(names, c.Default...)
	} else {
		// As before routes: the webhook, and Slack with --notify-slack.
		names = s.beforeRoutes()
	}
	// Where events go; and a destination with a template besides, which
	// none may go to.
	told := slices.Clone(names)
	if c != nil {
		for name := range c.Templates {
			names = append(names, name)
		}
	}
	since := time.Now().Add(-24 * time.Hour)
	var shown []destView
	for _, name := range s.destNamesGiven() {
		if !slices.Contains(names, name) {
			continue
		}
		d, _ := s.destination(name)
		shown = append(shown, destView{Name: name, Flag: strings.Fields(d.Flag)[0], JSON: d.JSON, Template: c != nil && c.Templates[name] != nil,
			Unnamed: !slices.Contains(told, name)})
	}
	// A destination gone, by the name its notices keep: no flag says
	// what it took.
	for _, place := range gone {
		shown = append(shown, destView{Name: strings.TrimPrefix(place, routedPrefix), Gone: true})
	}
	for _, dv := range shown {
		place := routedPrefix + dv.Name
		if !dv.Gone {
			d, _ := s.destination(dv.Name)
			place = d.Value
		}
		standing, err := s.l.Standing(ctx, []string{place}, since)
		if err != nil {
			return nil, err
		}
		dv.LastSent, dv.Waiting, dv.Failed, dv.More = standing.LastSent, standing.Waiting, standing.Failed, standing.More
		dv.GivenUp, dv.GivenUpMore = standing.GivenUp, standing.GivenUpMore
		why, err := s.l.LastError(ctx, place)
		if err != nil {
			return nil, err
		}
		dv.LastErr = s.redactPaths(why, lang)
		v.Dests = append(v.Dests, dv)
	}
	if !v.File && len(v.Dests) == 0 {
		return nil, nil
	}
	return v, nil
}
