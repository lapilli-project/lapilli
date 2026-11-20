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
func (v *routesView) ErrText(lang string) string {
	if t, ok := v.Err[lang]; ok {
		return t
	}
	return v.Err["en"]
}

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
	// neither the webhook nor Slack.
	Template, Unnamed bool
	LastSent          time.Time
	Waiting, Failed   int
	More              bool
	GivenUp           int
	GivenUpMore       bool
	// LastErr is why the latest still unsent failed, paths taken out: an
	// admin's to read.
	LastErr string
}

// routesView is what the page shows of routing; nil without --config and
// without a destination a route could name.
func (s *Server) routesView(ctx context.Context, lang string) (*routesView, error) {
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
		if s.cfg.NotifyWebhook != "" {
			names = append(names, "webhook")
		}
		if s.cfg.NotifySlack && s.cfg.Slack != "" {
			names = append(names, "slack")
		}
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
	for _, name := range s.destNamesGiven() {
		if !slices.Contains(names, name) {
			continue
		}
		d, _ := s.destination(name)
		dv := destView{Name: name, Flag: strings.Fields(d.Flag)[0], JSON: d.JSON, Template: c != nil && c.Templates[name] != nil,
			Unnamed: !slices.Contains(told, name)}
		standing, err := s.l.Standing(ctx, []string{d.Value}, since)
		if err != nil {
			return nil, err
		}
		dv.LastSent, dv.Waiting, dv.Failed, dv.More = standing.LastSent, standing.Waiting, standing.Failed, standing.More
		dv.GivenUp, dv.GivenUpMore = standing.GivenUp, standing.GivenUpMore
		why, err := s.l.LastError(ctx, d.Value)
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
