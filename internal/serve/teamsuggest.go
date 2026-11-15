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
// by Key=Value; Rules, how many of the rules no team owns it would own. Why
// says why "Make these teams" doesn't make it, "" when it does: "taken", a
// team of its name, in any case, is there already (Taken; SameKey, that it
// owns by Key alone or by no label, so that the label added to it loses it
// none of its rules); "takes", its label is on rules another team owns now,
// which a team made of it, first by name, would take; "hand", its value
// isn't a plain name (plainName).
type teamSuggestion struct {
	Key, Value string
	Rules      int
	Why        string
	Taken      *ledger.Team
	SameKey    bool
}

// plainName is a value "Make these teams" makes a team of: lowercase ASCII
// letters, digits and . _ -, as label values that name teams mostly are.
// One with capitals, or with a letter of another script, can read as
// another team's name (Web and web; pаyments, its а Cyrillic, and
// payments): it is made by hand, its name chosen.
var plainName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// maxTeamName is the longest a team's name is, in bytes (ledger.TeamNameOK):
// a label value longer is no team's name, and is passed over before
// anything is made of it.
const maxTeamName = 64

// teamSuggestions are the teams the rules' labels suggest, those with the
// most rules first: Make of them are made by "Make these teams"; More are
// past maxSuggested; Long rules have a value longer than a team's name, and
// are left out.
type teamSuggestions struct {
	Lines      []teamSuggestion
	Make       int
	More, Long int
}

// suggestTeams are the teams the rules' labels suggest: of the rules no team
// owns, by a note, a name pattern or labels, the values of the labels
// teamLabels names. A value under two of those labels is one team, by the
// label named first. How many rules each would own is worked out as owners
// would, with those made: a rule goes to the first, by name, whose label it
// has. All from the rules in effect, and those recorded elsewhere, as owners
// has their labels; nothing kept.
func (s *Server) suggestTeams(o *owners) teamSuggestions {
	var v teamSuggestions
	keys := s.teamLabels()
	w := o.walk()
	var free []string             // the rules no team owns
	owned := map[[2]string]bool{} // the labels of rules a team owns by labels
	for name := range o.ruleLabels {
		switch w.how(name).By {
		case "":
			free = append(free, name)
		case "labels":
			for _, k := range keys {
				if val, ok := o.ruleLabels[name][k]; ok && len(val) <= maxTeamName {
					owned[[2]string{k, val}] = true
				}
			}
		}
	}
	slices.Sort(free)
	// Each value, by the label named first that has it.
	key := map[string]int{}
	for _, name := range free {
		ls := o.ruleLabels[name]
		for i, k := range keys {
			val, ok := ls[k]
			switch {
			case !ok || val == "":
				continue
			case len(val) > maxTeamName: // before anything is made of it
				v.Long++
				continue
			}
			if j, ok := key[val]; !ok || i < j {
				key[val] = i
			}
		}
	}
	lines := map[string]*teamSuggestion{}
	for val, i := range key {
		l := &teamSuggestion{Key: keys[i], Value: val}
		lines[val] = l
		k := [2]string{l.Key, val}
		if j := slices.IndexFunc(o.teams, func(t ledger.Team) bool { return strings.EqualFold(t.Name, val) }); j >= 0 {
			t := o.teams[j]
			l.Why, l.Taken = "taken", &t
			l.SameKey = len(t.Labels) == 0 || len(t.Labels) == 1 && t.Labels[l.Key] != nil
		} else if owned[k] {
			l.Why = "takes"
		} else if !plainName.MatchString(val) {
			l.Why = "hand"
		}
	}
	// Who would own each rule with those made: the first, by name, whose
	// label it has; one not made, how many rules with it no team owns.
	for _, name := range free {
		ls := o.ruleLabels[name]
		var first *teamSuggestion
		for _, k := range keys {
			l := lines[ls[k]]
			if l == nil || l.Key != k {
				continue
			}
			if l.Why != "" {
				l.Rules++
			} else if first == nil || l.Value < first.Value {
				first = l
			}
		}
		if first != nil {
			first.Rules++
		}
	}
	// One that would own nothing, every rule of its another's first, isn't
	// suggested: taken away, it changes no other's count. Those with the
	// most rules first, maxSuggested of them.
	for _, l := range lines {
		if l.Rules > 0 {
			v.Lines = append(v.Lines, *l)
		}
	}
	slices.SortFunc(v.Lines, func(a, b teamSuggestion) int { return cmp.Or(b.Rules-a.Rules, strings.Compare(a.Value, b.Value)) })
	if len(v.Lines) > maxSuggested {
		v.More, v.Lines = len(v.Lines)-maxSuggested, v.Lines[:maxSuggested]
	}
	for _, l := range v.Lines {
		if l.Why == "" {
			v.Make++
		}
	}
	return v
}

// teamsMade is what "Make these teams" did, said once on the page it goes
// back to: the teams it made, by their ids in the query, those alone that
// are there and were made by the reader, as a bulk proposal's result is,
// so that a link can't make it say more; None, that it made none.
type teamsMade struct {
	Names []string
	None  bool
}

func teamsMadeOf(q url.Values, o *owners, user string) *teamsMade {
	vs := q["made"]
	if len(vs) == 1 && vs[0] == "0" {
		return &teamsMade{None: true}
	}
	var out teamsMade
	for i, v := range vs {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 || i == maxSuggested {
			return nil
		}
		if t, ok := o.byID[id]; ok && ledger.FoldName(t.By) == ledger.FoldName(user) && !slices.Contains(out.Names, t.Name) {
			out.Names = append(out.Names, t.Name)
		}
	}
	if len(out.Names) == 0 {
		return nil
	}
	return &out
}

// postSuggested makes the teams the rules' labels suggest, as worked out
// now, not as the page that asked showed them: those it says it makes, each
// as the team form makes one (postTeam), its name the value and its labels
// the label. One a team of the name took meanwhile is passed over; those it
// doesn't make, the page goes on saying why. An admin's (the route's), from
// the same site, as every form.
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
	defer s.poke() // who owns a rule changed: a team's windows are judged again
	q := url.Values{}
	for _, l := range s.suggestTeams(o).Lines {
		if l.Why != "" {
			continue
		}
		id, err := s.l.PutTeam(r.Context(), ledger.Team{Name: l.Value, Labels: incident.Labels{l.Key: {l.Value}}}, s.user(r))
		if code, ok := errCode(err); ok && code == http.StatusBadRequest {
			continue // a team of the name, made meanwhile
		}
		if err != nil {
			s.oops(w, r, err)
			return
		}
		q.Add("made", strconv.FormatInt(id, 10))
	}
	if len(q) == 0 {
		q.Set("made", "0")
	}
	back(w, r, "/teams?"+q.Encode()+"#suggested")
}
