package serve

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// The overview's charts are marked, as Grafana's annotations mark a
// panel (docs/design/ui.md): the weeks the rules changed, always; the
// weeks with firings during deploys, and the weeks a quiet window was
// over, unless asked not to (marks=deploys,windows; none for neither).
// The same marks are in the table of the weeks.

// marksView is what the charts mark, and the links that turn each on or
// off: Deploys and Windows, shown; HasDeploys, that there are deploys to
// mark; DeploysTo and WindowsTo, the marks= of the page with each turned
// over ("" for both, the page's own).
type marksView struct {
	Deploys, Windows     bool
	HasDeploys           bool
	DeploysTo, WindowsTo string
}

// marksAsked are the marks a page asks for: both, unless marks= says
// which; a value it doesn't take is none.
func marksAsked(q url.Values) (deploys, windows bool) {
	vs, ok := q["marks"]
	if !ok {
		return true, true
	}
	for _, v := range vs {
		for _, m := range strings.Split(v, ",") {
			switch strings.TrimSpace(m) {
			case "deploys":
				deploys = true
			case "windows":
				windows = true
			}
		}
	}
	return deploys, windows
}

// marksParam is marks= for the marks shown: "" for both, the default.
func marksParam(deploys, windows bool) string {
	switch {
	case deploys && windows:
		return ""
	case deploys:
		return "deploys"
	case windows:
		return "windows"
	}
	return "none"
}

// marksOf works out the overview's marks: the weeks ws, as tallied in t,
// are marked Quiet where a quiet window excused one of the team's firings,
// the ledger's own say, or covers some time of it, up to now, and is the
// team's or everyone's (any team's, for all teams); for a team, one that
// applies to some rule of the team's, by its name, team and labels, as a
// rule's strip has it.
func (s *Server) marksOf(ctx context.Context, q url.Values, t *tallies, o *owners, team *ledger.Team, ws []*weekView, deploys bool) (marksView, error) {
	var m marksView
	m.Deploys, m.Windows = marksAsked(q)
	m.Deploys = m.Deploys && deploys
	m.HasDeploys = deploys
	if deploys {
		m.DeploysTo, m.WindowsTo = marksParam(!m.Deploys, m.Windows), marksParam(m.Deploys, !m.Windows)
	} else {
		m.WindowsTo = marksParam(true, !m.Windows) // no deploys to mark either way
	}
	for _, w := range ws {
		if m.Deploys {
			w.DeployMark = w.Deploys
		}
	}
	if !m.Windows {
		return m, nil
	}
	quiet := 0
	for _, w := range ws {
		if w.Excused > 0 {
			w.Quiet = true
			quiet++
		}
	}
	first, n := t.weeks[0], 7*len(t.weeks)
	windows, err := s.l.Windows(ctx, s.l.Midnight(first), s.l.Midnight(first+int64(n)))
	if err != nil {
		return m, err
	}
	var until time.Time // the week under way, up to now
	if !t.over {
		until = s.now()
	}
	var mine []string
	var meta map[string]ruleMeta
	if team != nil && len(windows) > 0 {
		if meta, err = s.ruleMeta(ctx); err != nil {
			return m, err
		}
		for name, id := range o.rule {
			if id == team.ID {
				mine = append(mine, name)
			}
		}
		slices.Sort(mine)
	}
	for _, w := range windows {
		if quiet == len(ws) {
			break
		}
		if w.TeamID < 0 || team != nil && w.TeamID != 0 && w.TeamID != team.ID {
			continue // before working out when it falls due
		}
		if team != nil && !slices.ContainsFunc(mine, func(rule string) bool { return w.Applies(rule, meta[rule].labels) }) {
			continue
		}
		windowDaysTo(w, s.l, first, n, until, func(i int) {
			if k := i / 7; k < len(ws) && !ws[k].Quiet {
				ws[k].Quiet = true
				quiet++
			}
		})
	}
	return m, nil
}
