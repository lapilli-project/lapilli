package serve

import (
	"context"
	"regexp"
	"slices"
	"sync"

	"lapilli.dev/internal/ledger"
)

// owners say which team owns each rule and incident: a rule's note names
// its team, or else the first team, by name, whose rule pattern matches
// the rule's name or whose labels the rule has.
type owners struct {
	teams  []ledger.Team
	byID   map[int64]ledger.Team
	rule   map[string]int64 // rule name to team id
	notes  map[string]ledger.Note
	labels []map[string][]string // each team's labels, in teams' order
}

// ownersCache keeps the owners worked out until the ledger changes.
type ownersCache struct {
	sync.Mutex
	gen int64
	o   *owners
}

func (s *Server) owners(ctx context.Context) (*owners, error) {
	gen := s.l.Gen()
	s.own.Lock()
	if s.own.o != nil && s.own.gen == gen {
		o := s.own.o
		s.own.Unlock()
		return o, nil
	}
	s.own.Unlock()
	teams, err := s.l.Teams(ctx)
	if err != nil {
		return nil, err
	}
	notes, err := s.l.Notes(ctx)
	if err != nil {
		return nil, err
	}
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		return nil, err
	}
	o := &owners{teams: teams, byID: map[int64]ledger.Team{}, rule: map[string]int64{}, notes: notes}
	res := make([]*regexp.Regexp, len(teams))
	for i, t := range teams {
		o.byID[t.ID] = t
		if t.Rules != "" {
			res[i], _ = regexp.Compile(t.Rules)
		}
		o.labels = append(o.labels, t.Labels)
	}
	// The rules' labels, from the latest version of them.
	labels := map[string]map[string]string{}
	if !s.cfg.Alerts {
		if vs, err := s.versions(ctx, s.now(), s.now()); err == nil && len(vs) > 0 {
			for _, r := range vs[len(vs)-1].Rules {
				labels[r.Name] = r.Labels
			}
		}
	}
	for _, r := range table {
		if n, ok := notes[r.Name]; ok && n.TeamID != 0 {
			o.rule[r.Name] = n.TeamID
			continue
		}
		for i, t := range teams {
			if (res[i] != nil && res[i].MatchString(r.Name)) || (len(t.Labels) > 0 && has(labels[r.Name], t.Labels)) {
				o.rule[r.Name] = t.ID
				break
			}
		}
	}
	s.own.Lock()
	s.own.gen, s.own.o = gen, o
	s.own.Unlock()
	return o, nil
}

// has reports whether labels have, for each label named, one of its values.
func has(labels map[string]string, want map[string][]string) bool {
	for k, vs := range want {
		if v, ok := labels[k]; !ok || !slices.Contains(vs, v) {
			return false
		}
	}
	return true
}

// team is the team named, ok false for none or an unknown name.
func (o *owners) team(name string) (ledger.Team, bool) {
	for _, t := range o.teams {
		if t.Name == name {
			return t, true
		}
	}
	return ledger.Team{}, false
}

// teamOf is the name of the team that owns a rule, "" for none.
func (o *owners) teamOf(rule string) string {
	return o.byID[o.rule[rule]].Name
}

// ownsIncident reports whether a team owns an incident: one of its rules
// caught it, or it has a value of a label the team owns by.
func (o *owners) ownsIncident(t ledger.Team, in ledger.Incident) bool {
	for _, c := range in.CaughtBy {
		if o.rule[c.Rule] == t.ID {
			return true
		}
	}
	for k, vs := range t.Labels {
		if ivs, ok := in.Labels[k]; ok && slices.ContainsFunc(ivs, func(v string) bool { return slices.Contains(vs, v) }) {
			return true
		}
	}
	return false
}

// only keeps, of the tallies, the rules a team owns.
func (t *tallies) only(o *owners, team ledger.Team) {
	for _, w := range t.weeks {
		for id := range t.by[w] {
			if o.rule[t.rules[id].Name] != team.ID {
				delete(t.by[w], id)
			}
		}
	}
}

// teamFilter is the team a page is filtered by, from ?team=.
type teamFilter struct {
	Name  string
	Teams []string
}

func (s *Server) teamFilter(o *owners, name string) (teamFilter, *ledger.Team) {
	f := teamFilter{}
	for _, t := range o.teams {
		f.Teams = append(f.Teams, t.Name)
	}
	if t, ok := o.team(name); ok {
		f.Name = name
		return f, &t
	}
	return f, nil
}
