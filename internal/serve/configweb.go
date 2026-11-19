package serve

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// GET /config.yaml (docs/design/serveops.md): the teams, quiet windows,
// reports and roles the ledger holds, declared and made in the UI alike,
// as --config takes them, to paste under serve.config in the chart's
// values. Admins' alone: it names who has which role. Read with --config,
// it changes nothing, but that what it holds is declared.

// yLabels are labels as a config writes them: a single value as a text,
// more as a list.
type yLabels incident.Labels

func (l yLabels) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		var kn, vn yaml.Node
		if err := kn.Encode(k); err != nil {
			return nil, err
		}
		if vs := l[k]; len(vs) == 1 {
			if err := vn.Encode(vs[0]); err != nil {
				return nil, err
			}
		} else {
			if err := vn.Encode(vs); err != nil {
				return nil, err
			}
			vn.Style = yaml.FlowStyle
		}
		n.Content = append(n.Content, &kn, &vn)
	}
	return n, nil
}

type yTeam struct {
	Name       string  `yaml:"name"`
	Rules      string  `yaml:"rules,omitempty"`
	Labels     yLabels `yaml:"labels,omitempty"`
	Note       string  `yaml:"note,omitempty"`
	WorkHours  string  `yaml:"work_hours,omitempty"`
	SleepHours string  `yaml:"sleep_hours,omitempty"`
	TZ         string  `yaml:"tz,omitempty"`
	Shifts     string  `yaml:"shifts,omitempty"`
}

type yWindow struct {
	Title  string  `yaml:"title"`
	Team   string  `yaml:"team,omitempty"`
	Start  string  `yaml:"start"`
	End    string  `yaml:"end"`
	Every  string  `yaml:"every,omitempty"`
	On     string  `yaml:"repeat_on,omitempty"`
	Until  string  `yaml:"until,omitempty"`
	Rules  string  `yaml:"rules,omitempty"`
	Labels yLabels `yaml:"labels,omitempty"`
}

type yReport struct {
	Name     string   `yaml:"name"`
	Team     string   `yaml:"team,omitempty"`
	Schedule string   `yaml:"schedule"`
	Span     string   `yaml:"span,omitempty"`
	Zone     string   `yaml:"zone,omitempty"`
	Sections []string `yaml:"sections,flow"`
	Dests    []string `yaml:"dests,flow"`
	Enabled  *bool    `yaml:"enabled,omitempty"`
}

type yGrant struct {
	User  string `yaml:"user,omitempty"`
	Group string `yaml:"group,omitempty"`
	Team  string `yaml:"team,omitempty"`
}

type yGoal struct {
	Name        string   `yaml:"name"`
	Team        string   `yaml:"team,omitempty"`
	Measure     string   `yaml:"measure"`
	AtLeast     yNumber  `yaml:"at_least,omitempty"`
	AtMost      yNumber  `yaml:"at_most,omitempty"`
	NoWorseThan string   `yaml:"no_worse_than,omitempty"`
	By          yNumber  `yaml:"by,omitempty"`
	Critical    []string `yaml:"critical,flow,omitempty"`
	Each        string   `yaml:"each,omitempty"`
	Stones      string   `yaml:"stones,omitempty"`
}

// yNumber is a goal's number as the file wrote it, written back as a
// number.
type yNumber string

func (n yNumber) IsZero() bool { return n == "" }

func (n yNumber) MarshalYAML() (any, error) {
	tag := "!!int"
	if strings.ContainsAny(string(n), ".eE") {
		tag = "!!float"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(n)}, nil
}

type yMarked struct {
	Kind   string            `yaml:"kind"`
	Title  string            `yaml:"title"`
	Team   string            `yaml:"team,omitempty"`
	Start  string            `yaml:"start"`
	End    string            `yaml:"end"`
	Every  string            `yaml:"every,omitempty"`
	On     string            `yaml:"repeat_on,omitempty"`
	Until  string            `yaml:"until,omitempty"`
	Labels map[string]string `yaml:"labels,omitempty"`
}

type yConfig struct {
	Teams   []yTeam   `yaml:"teams,omitempty"`
	Windows []yWindow `yaml:"windows,omitempty"`
	Reports []yReport `yaml:"reports,omitempty"`
	Grants  []yGrant  `yaml:"grants,omitempty"`
	Goals   []yGoal   `yaml:"goals,omitempty"`
	Marked  []yMarked `yaml:"marked,omitempty"`
}

// exportConfig is what the ledger holds that --config can declare, as a
// --config file: times in loc, as RFC 3339, so each is the instant it is
// in any zone; a window's last day as a date when it is that day's start.
// What a file can't name is left out and said in a comment: a quiet window
// or a report of a team removed. Teams whose names differ only in letter
// case are said too, as a file can't declare both.
func exportConfig(d ledger.Declaring, loc *time.Location) []byte {
	names := map[int64]string{}
	var c yConfig
	for _, t := range d.Teams {
		names[t.ID] = t.Name
		c.Teams = append(c.Teams, yTeam{t.Name, t.Rules, yLabels(t.Labels), t.Note, t.WorkHours, t.SleepHours, t.TZ, t.Shifts})
	}
	var left []string
	teamOf := func(id int64) (string, bool) {
		if id == 0 {
			return "", true
		}
		n, ok := names[id]
		return n, ok
	}
	for _, w := range d.Windows {
		team, ok := teamOf(w.TeamID)
		if !ok {
			left = append(left, "quiet window "+commented(w.Title))
			continue
		}
		y := yWindow{Title: w.Title, Team: team, Start: w.Start.In(loc).Format(time.RFC3339), End: w.End.In(loc).Format(time.RFC3339),
			Every: w.Every, On: w.On, Rules: w.Rules, Labels: yLabels(w.Labels)}
		if !w.Until.IsZero() {
			if u := w.Until.In(loc); midnight(u, loc).Equal(w.Until) {
				y.Until = u.Format("2006-01-02")
			} else {
				y.Until = u.Format(time.RFC3339)
			}
		}
		c.Windows = append(c.Windows, y)
	}
	for _, r := range d.Reports {
		team, ok := teamOf(r.TeamID)
		if !ok {
			left = append(left, "report "+commented(r.Name))
			continue
		}
		y := yReport{Name: r.Name, Team: team, Schedule: r.Schedule, Span: r.Span, Sections: r.Sections, Dests: r.Dests}
		if r.Zone != "ledger" {
			y.Zone = r.Zone
		}
		if !r.Enabled {
			y.Enabled = new(bool)
		}
		c.Reports = append(c.Reports, y)
	}
	for _, g := range d.Grants {
		team, ok := teamOf(g.TeamID)
		if !ok {
			continue // the ledger deletes a team's roles with it
		}
		kind, who, _ := strings.Cut(g.Who, ":")
		y := yGrant{Team: team}
		if kind == "group" {
			y.Group = who
		} else {
			y.User = who
		}
		c.Grants = append(c.Grants, y)
	}
	for _, g := range d.Goals {
		team, ok := teamOf(g.TeamID)
		if g.EveryTeam {
			team, ok = "*", true
		}
		if !ok {
			left = append(left, "goal "+commented(g.Name))
			continue
		}
		y := yGoal{Name: g.Name, Team: team, Measure: g.Measure, AtLeast: yNumber(g.AtLeast), AtMost: yNumber(g.AtMost),
			NoWorseThan: g.NoWorseThan, By: yNumber(g.By), Critical: g.Critical, Stones: g.Stones}
		if g.Each != "week" {
			y.Each = g.Each
		}
		c.Goals = append(c.Goals, y)
	}
	for _, m := range d.Marked {
		team, ok := teamOf(m.TeamID)
		if !ok {
			left = append(left, "marked window "+commented(m.Title))
			continue
		}
		y := yMarked{Kind: m.Kind, Title: m.Title, Team: team, Start: m.Start.In(loc).Format(time.RFC3339), End: m.End.In(loc).Format(time.RFC3339),
			Every: m.Every, On: m.On, Labels: m.Labels}
		if !m.Until.IsZero() {
			if u := m.Until.In(loc); midnight(u, loc).Equal(m.Until) {
				y.Until = u.Format("2006-01-02")
			} else {
				y.Until = u.Format(time.RFC3339)
			}
		}
		c.Marked = append(c.Marked, y)
	}
	var b bytes.Buffer
	b.WriteString("# The teams, quiet windows, reports, roles, goals and marked windows lapilli serve's ledger holds,\n")
	b.WriteString("# declared in --config and made in the UI alike, as --config takes them.\n")
	if len(d.Goals) > 0 {
		b.WriteString("# The goals, which only --config declares.\n")
	}
	b.WriteString("# In the Helm chart, put them under serve.config in values.yaml: what\n")
	b.WriteString("# they declare can then be changed only there.\n")
	if len(left) > 0 {
		fmt.Fprintf(&b, "# Left out, as their team was removed: %s.\n", strings.Join(left, ", "))
	}
	alike := map[string][]string{}
	var order []string
	for _, t := range d.Teams {
		f := ledger.FoldName(t.Name)
		if len(alike[f]) == 0 {
			order = append(order, f)
		}
		alike[f] = append(alike[f], t.Name)
	}
	for _, f := range order {
		if len(alike[f]) > 1 {
			var names []string
			for _, n := range alike[f] {
				names = append(names, commented(n))
			}
			fmt.Fprintf(&b, "# The teams %s differ only in letter case: rename all but one before declaring them.\n", strings.Join(names, ", "))
		}
	}
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	enc.Encode(c)
	enc.Close()
	return b.Bytes()
}

// commented is a name as a comment of an export writes it: quoted, Go's
// way, every line break (\n, \r, NEL, U+2028, U+2029), control and format
// character escaped. A title written before titles were one line, by a
// team's editor, would otherwise end the comment, and an admin pasting the
// export into values would declare what follows: a role, for anyone.
func commented(name string) string { return strconv.Quote(name) }

// configYAML is GET /config.yaml: an admin's.
func (s *Server) configYAML(w http.ResponseWriter, r *http.Request) {
	if s.role(r) < admin {
		s.oops(w, r, s.forbidden(admin))
		return
	}
	d, err := s.l.AllDeclarable(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, bytes.NewReader(exportConfig(d, s.cfg.Loc)))
}
