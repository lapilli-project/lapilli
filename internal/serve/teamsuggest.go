package serve

import (
	"cmp"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// Teams the rules' labels suggest, and why a rule is a team's
// (docs/design/ui.md, "First use"). A set of rules often says whose each
// is in a label already (team=payments), and a reader new to Lapilli has
// no team, so no page can be shown for one, and no way to tell that the
// Teams page is where that is set. The Teams page lists, of the labels
// --team-labels names, each value no team owns by and how many rules it
// would own; an admin makes them all with one button, each as the team
// form would. A rule's page says how its team was found, or why none was.

// defaultTeamLabels are the labels whose values are taken for teams'
// names, unless --team-labels names others.
var defaultTeamLabels = []string{"team", "owner", "squad"}

// maxSuggested is how many teams the page suggests, and one request makes,
// at most: those with the most rules.
const maxSuggested = 50

func (s *Server) teamLabels() []string {
	if len(s.cfg.TeamLabels) > 0 {
		return s.cfg.TeamLabels
	}
	return defaultTeamLabels
}

// ownHow is how a rule's team is found, as owners finds it: by its note
// (By "note"; Team "" when that team is gone), by its name matching a
// team's pattern ("pattern"), or by its labels having a team's ("labels",
// Labels those of its that did); By "" for none.
type ownHow struct {
	By, Team, Pattern, Labels string
}

// ownWalk finds a rule's team as owners does, its teams' patterns read
// once.
type ownWalk struct {
	o   *owners
	res []*regexp.Regexp
}

func (o *owners) walk() ownWalk {
	w := ownWalk{o: o, res: make([]*regexp.Regexp, len(o.teams))}
	for i, t := range o.teams {
		if t.Rules != "" {
			w.res[i], _ = regexp.Compile(t.Rules)
		}
	}
	return w
}

// how is how a rule's team is found: the note, then the first team, by
// name, whose pattern matches its name, then the first whose labels it has.
func (w ownWalk) how(rule string) ownHow {
	o := w.o
	if n, ok := o.notes[rule]; ok && n.TeamID != 0 {
		return ownHow{By: "note", Team: o.byID[n.TeamID].Name}
	}
	for i, t := range o.teams {
		if w.res[i] != nil && w.res[i].MatchString(rule) {
			return ownHow{By: "pattern", Team: t.Name, Pattern: t.Rules}
		}
	}
	ls := o.ruleLabels[rule]
	for _, t := range o.teams {
		if len(t.Labels) > 0 && has(ls, t.Labels) {
			var kv []string
			for _, k := range slices.Sorted(maps.Keys(t.Labels)) {
				kv = append(kv, k+"="+ls[k])
			}
			return ownHow{By: "labels", Team: t.Name, Labels: strings.Join(kv, " ")}
		}
	}
	return ownHow{}
}

// ownedView is how a rule's page says its team was found: How; and, when
// none was and there is no team at all, Teamless, to the teams the labels
// suggest.
type ownedView struct {
	How      ownHow
	Teamless *teamless
}

func (s *Server) ownedOf(o *owners, rule string) ownedView {
	v := ownedView{How: o.walk().how(rule)}
	if v.How.By == "" && len(o.teams) == 0 {
		v.Teamless = &teamless{Suggested: s.suggestTeams(o).Make}
	}
	return v
}

// teamless is, where no team is kept at all, how many teams the rules'
// labels suggest: the rules list and a rule's page say so, and link to
// them.
type teamless struct{ Suggested int }

func (s *Server) teamlessOf(o *owners) *teamless {
	if len(o.teams) > 0 {
		return nil
	}
	return &teamless{Suggested: s.suggestTeams(o).Make}
}

// teamSuggestion is a team the rules' labels suggest: named Value, owning
// by Key=Value. Rules is how many rules it would own, made with the others;
// Taken, a team of its name already there, whose labels don't have it,
// which isn't made: Rules is then how many with it that team doesn't own.
type teamSuggestion struct {
	Key, Value string
	Rules      int
	Taken      *ledger.Team
}

// teamSuggestions are the teams the rules' labels suggest, those with the
// most rules first: Make of them are made by "Make these teams"; More
// values are past maxSuggested; Unnamed, label values that can't be a
// team's name, are left out.
type teamSuggestions struct {
	Lines         []teamSuggestion
	Make          int
	More, Unnamed int
}

// suggestTeams are the teams the rules' labels suggest: of the rules no
// note or name pattern gives a team, the values of the labels teamLabels
// names that no team owns by. A value under two of those labels is one
// team, by the label named first. How many rules each would own is worked
// out as owners would, with them made: a rule goes to the first team, by
// name, whose labels it has, a team there already or one made. All from
// the rules in effect, and those recorded elsewhere, as owners has their
// labels; nothing kept.
func (s *Server) suggestTeams(o *owners) teamSuggestions {
	var v teamSuggestions
	keys := s.teamLabels()
	w := o.walk()
	var free []string // the rules no note or name pattern gives a team
	for name := range o.ruleLabels {
		if h := w.how(name); h.By != "note" && h.By != "pattern" {
			free = append(free, name)
		}
	}
	slices.Sort(free)
	owned := func(k, val string) bool {
		return slices.ContainsFunc(o.teams, func(t ledger.Team) bool { return slices.Contains(t.Labels[k], val) })
	}
	// Each value, by the label named first that has it, and how many rules
	// have it so.
	type cand struct {
		key   int
		rules int
	}
	byValue := map[string]*cand{}
	unnamed := map[string]bool{}
	for _, name := range free {
		ls := o.ruleLabels[name]
		for i, k := range keys {
			val, ok := ls[k]
			switch {
			case !ok || val == "" || owned(k, val):
				continue
			case !ledger.TeamNameOK(val):
				unnamed[k+"="+val] = true
				continue
			}
			c := byValue[val]
			if c == nil {
				c = &cand{key: i}
				byValue[val] = c
			}
			if i < c.key {
				c.key, c.rules = i, 0
			}
			if i == c.key {
				c.rules++
			}
		}
	}
	values := slices.Collect(maps.Keys(byValue))
	slices.SortFunc(values, func(a, b string) int { return cmp.Or(byValue[b].rules-byValue[a].rules, strings.Compare(a, b)) })
	if len(values) > maxSuggested {
		v.More, values = len(values)-maxSuggested, values[:maxSuggested]
	}
	for _, val := range values {
		l := teamSuggestion{Key: keys[byValue[val].key], Value: val}
		if t, ok := o.team(val); ok {
			l.Taken = &t
		}
		v.Lines = append(v.Lines, l)
	}
	// Who would own each rule with them made: the teams owning by labels
	// and those made, by name.
	type labelTeam struct {
		name   string
		labels map[string][]string
		made   *teamSuggestion
	}
	var ts []labelTeam
	for _, t := range o.teams {
		if len(t.Labels) > 0 {
			ts = append(ts, labelTeam{t.Name, t.Labels, nil})
		}
	}
	for i, l := range v.Lines {
		if l.Taken == nil {
			ts = append(ts, labelTeam{l.Value, map[string][]string{l.Key: {l.Value}}, &v.Lines[i]})
		}
	}
	slices.SortStableFunc(ts, func(a, b labelTeam) int { return strings.Compare(a.name, b.name) })
	for _, name := range free {
		for _, t := range ts {
			if has(o.ruleLabels[name], t.labels) {
				if t.made != nil {
					t.made.Rules++
				}
				break
			}
		}
	}
	// One a team of its name has, how many rules with it that team doesn't
	// own yet.
	for i, l := range v.Lines {
		if l.Taken != nil {
			for _, name := range free {
				if o.ruleLabels[name][l.Key] == l.Value && o.teamOf(name) != l.Value {
					v.Lines[i].Rules++
				}
			}
		}
	}
	// One that would own nothing, every rule of its another's first, or
	// its team's already, isn't suggested: taken away, it changes no other's
	// count.
	v.Lines = slices.DeleteFunc(v.Lines, func(l teamSuggestion) bool { return l.Rules == 0 })
	for _, l := range v.Lines {
		if l.Taken == nil {
			v.Make++
		}
	}
	v.Unnamed = len(unnamed)
	return v
}

// teamsMade is what "Make these teams" did, said once on the page it goes
// back to, from its query: how many were made, and how many were passed
// over as a team of the name, or owning by the label, was there.
type teamsMade struct{ Made, Skipped int }

func teamsMadeOf(q url.Values) *teamsMade {
	n := func(k string) (int, bool) {
		x, err := strconv.Atoi(q.Get(k))
		return x, err == nil && x >= 0 && x <= maxSuggested
	}
	made, ok1 := n("made")
	skipped, ok2 := n("skipped")
	if !ok1 || !ok2 || made+skipped > maxSuggested {
		return nil
	}
	return &teamsMade{made, skipped}
}

// postSuggested makes the teams the rules' labels suggest, as worked out
// now, not as the page that asked showed them: each as the team form makes
// one (postTeam), its name the value and its labels the label. One a team
// of the name, or one owning by the label, took meanwhile is passed over.
// An admin's (the route's), from the same site, as every form.
func (s *Server) postSuggested(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.owners(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	var done teamsMade
	defer s.poke() // who owns a rule changed: a team's windows are judged again
	for _, l := range s.suggestTeams(o).Lines {
		if l.Taken != nil {
			done.Skipped++
			continue
		}
		_, err := s.l.PutTeam(r.Context(), ledger.Team{Name: l.Value, Labels: incident.Labels{l.Key: {l.Value}}}, s.user(r))
		if code, ok := errCode(err); ok && code == http.StatusBadRequest {
			done.Skipped++ // a team of the name, made meanwhile
			continue
		}
		if err != nil {
			s.oops(w, r, err)
			return
		}
		done.Made++
	}
	back(w, r, "/teams?made="+strconv.Itoa(done.Made)+"&skipped="+strconv.Itoa(done.Skipped)+"#suggested")
}
