package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/signin"
	"lapilli.dev/internal/source"
)

// A team as the boundary of change (docs/design/changes.md): a team's
// editors propose changes to the rules it owns, its approvers approve
// them, its rules are written only into the sources it may write, and no
// one approves or applies a change while a change freeze holds. All of it
// is declared in --config beside the teams, and kept in the ledger in the
// write that keeps the ledger to the file (ledger.BoundsKey): read twice
// first, as a role is, and the same on every replica. The UI changes none
// of it.
//
// Who owns a rule is worked out as the pages work it out (owners), and
// again without its note (bare): a note moves a rule into a team, and the
// team its name and labels give it still holds it. A proposal keeps the
// teams that owned its rule as it was proposed (ownedBy); approving it and
// carrying it out each ask again who owns it now: a change to who owns it
// in between refuses the one a team's approver could have approved.

// bounds are the boundaries of change a file declares: by a team's name,
// who approves its rules' changes and where they are written; and the
// change freezes.
type bounds struct {
	Teams   map[string]teamBounds `json:"teams,omitempty"`
	Freezes []freeze              `json:"freezes,omitempty"`
}

// teamBounds are a team's: its approvers, as --approvers names people and
// groups; and In, where its rules are written, of what --changes-in names,
// when Narrowed says the file gave it (an empty list is nowhere).
type teamBounds struct {
	Approvers []string `json:"approvers,omitempty"`
	In        []string `json:"changes_in,omitempty"`
	Narrowed  bool     `json:"narrowed,omitempty"`
}

// freeze is a change freeze: its times as a quiet window's, once or
// repeating, for everyone or for a team (its name).
type freeze struct {
	Title string    `json:"title"`
	Team  string    `json:"team,omitempty"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Every string    `json:"every,omitempty"`
	On    string    `json:"repeat_on,omitempty"`
	Until time.Time `json:"until,omitzero"`
	w     ledger.Window
}

// maxFreezes is how many change freezes a file declares at most.
const maxFreezes = 100

func init() {
	noPaths = append(noPaths, noPathTeamIn)
	configFormats = append(configFormats, boundsFormats...)
}

// boundsFormats are what can be wrong with the boundaries a file declares,
// as texts to translate (configFormats).
var boundsFormats = []string{
	"approvers names no one: leave it out for none",
	"changes_in %s: a namespace, a rule group or SOURCE:NAME, as --changes-in names one",
	"team %s's approvers or changes_in are given twice",
	"approver %s: *@DOMAIN names everyone of a domain",
	"approver * is everyone signed in: name people (user:NAME), groups (group:NAME) or a domain (*@DOMAIN)",
	"approver %s: write user:NAME for a person or group:NAME for a group",
	"approver %s: a name is 200 bytes at most, with no character that doesn't show",
	"approver %s: a group's name has no comma",
	"approver %s: a name Lapilli keeps for itself",
	"a change freeze's team %s isn't one this file declares: declare it under teams",
	"a change freeze has a title",
	"a config declares %d change freezes at most",
	"team %s's changes_in %s isn't within --changes-in: a team's sources are some of those --changes-in names",
	"team %s is declared on line %d too, in another letter case or with spaces around it: give its approvers, changes_in and change freezes to one team",
}

// lineOf is a refusal of something on a line of a file, as the file's
// other refusals name their line.
func lineOf(line int, err error) error {
	var b badRequest
	if errors.As(err, &b) && b.format != "" {
		return configError{line, b.format, b.args}
	}
	return atLine(line, err)
}

// noPathTeamIn is why a team's rule kept where its team's changes_in
// doesn't name is for a person to change.
const noPathTeamIn = "its team's changes_in doesn't name where it is kept: Lapilli writes a team's rules only there"

// parseBounds reads the boundaries of change a file declares, once the
// rest of it was read into d: each team's approvers and changes_in, and
// the freezes, each freeze's team one the file declares. A team's name is
// read as the ledger keeps it, without the spaces around it
// (ledger.checkTeam). Kept in d.Bounds, as the ledger keeps them.
func parseBounds(root *yaml.Node, loc *time.Location, d *ledger.Declared) error {
	b := bounds{Teams: map[string]teamBounds{}}
	declared := map[string]bool{}
	lines := map[string][]int{} // the lines of the teams of a name, in any letter case
	for _, t := range d.Teams {
		n := strings.TrimSpace(t.Name)
		declared[n] = true
		lines[ledger.FoldName(n)] = append(lines[ledger.FoldName(n)], t.Line)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		for _, item := range v.Content {
			var err error
			switch k.Value {
			case "teams":
				err = parseTeamBounds(item, &b)
			case "freezes":
				err = parseFreeze(item, loc, declared, &b)
			}
			if err != nil {
				return err
			}
		}
	}
	if len(b.Freezes) > maxFreezes {
		return bad("a config declares %d change freezes at most", maxFreezes)
	}
	// A name two teams of the file have, in another letter case or with
	// spaces around it: the ledger keeps one of them, and skips the other
	// (declareTeams), so what is given under it would hold a team it
	// wasn't given to, or a name no team has.
	named := sortedKeys(b.Teams)
	for _, f := range b.Freezes {
		if f.Team != "" {
			named = append(named, f.Team)
		}
	}
	for _, n := range named {
		if ls := lines[ledger.FoldName(n)]; len(ls) > 1 {
			return configError{ls[1], "team %s is declared on line %d too, in another letter case or with spaces around it: give its approvers, changes_in and change freezes to one team", []any{n, ls[0]}}
		}
	}
	if len(b.Teams) == 0 && len(b.Freezes) == 0 {
		return nil
	}
	j, err := json.Marshal(b)
	d.Bounds = string(j)
	return err
}

// parseTeamBounds reads a team's approvers and changes_in.
func parseTeamBounds(n *yaml.Node, b *bounds) error {
	var team string
	var tb teamBounds
	err := each(n, "team", func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "name":
			team, err = name(key, v)
		case "approvers":
			if tb.Approvers, err = texts(key, v); err == nil {
				for _, x := range tb.Approvers {
					if err := approverOK(x); err != nil {
						return lineOf(v.Line, err)
					}
				}
				if len(tb.Approvers) == 0 {
					return badAt(v, "approvers names no one: leave it out for none")
				}
			}
		case "changes_in":
			tb.Narrowed = true
			if tb.In, err = texts(key, v); err == nil {
				for _, x := range tb.In {
					if strings.TrimSpace(x) != x || !changesInEntry.MatchString(x) {
						return badAt(v, "changes_in %s: a namespace, a rule group or SOURCE:NAME, as --changes-in names one", x)
					}
				}
			}
		}
		return err
	})
	if err != nil || len(tb.Approvers) == 0 && !tb.Narrowed {
		return err
	}
	team = strings.TrimSpace(team) // as the ledger keeps it
	if _, twice := b.Teams[team]; twice {
		return badAt(n, "team %s's approvers or changes_in are given twice", team)
	}
	b.Teams[team] = tb
	return nil
}

// changesInEntry is an entry of --changes-in, as the flag takes it: a
// name, or a prefix ending in *.
var changesInEntry = regexp.MustCompile(`^[^*,\x00-\x1f\x7f]+\*?$`)

// approverOK checks an approver a team names, as --approvers names one but
// stricter: user:NAME, group:NAME or *@DOMAIN. A name with neither user:
// nor group: is refused (one whose name is a group's would take the
// group's approval, as the UI's roles refuse it), and so is * (everyone
// signed in).
func approverOK(x string) error {
	kind, who, ok := strings.Cut(x, ":")
	if domain, at := strings.CutPrefix(x, "*@"); at {
		if domain == "" || strings.ContainsAny(domain, "@,* ") || !signin.Plain(domain) || ledger.Hidden(domain) {
			return bad("approver %s: *@DOMAIN names everyone of a domain", x)
		}
		return nil
	}
	switch {
	case x == "*":
		return bad("approver * is everyone signed in: name people (user:NAME), groups (group:NAME) or a domain (*@DOMAIN)")
	case !ok || kind != "user" && kind != "group":
		return bad("approver %s: write user:NAME for a person or group:NAME for a group", x)
	case who == "" || strings.TrimSpace(who) != who || len(who) > ledger.MaxName || ledger.Hidden(who) || !signin.Plain(who):
		return bad("approver %s: a name is 200 bytes at most, with no character that doesn't show", x)
	case kind == "group" && strings.Contains(who, ","):
		return bad("approver %s: a group's name has no comma", x)
	}
	if f := ledger.FoldName(who); kind == "user" && slices.ContainsFunc([]string{"anonymous", ledger.Copier, ledger.FiringsCopier, ledger.GrafanaCopier, ledger.TaskTracker,
		ledger.ChangesCopier, ledger.Proposer, ledger.ConfigBy}, func(n string) bool { return f == ledger.FoldName(n) }) {
		return bad("approver %s: a name Lapilli keeps for itself", x)
	}
	return nil
}

// parseFreeze reads a change freeze: its times as a quiet window's, and
// checked as one is (ledger.CheckSpan).
func parseFreeze(n *yaml.Node, loc *time.Location, declared map[string]bool, b *bounds) error {
	var f freeze
	err := each(n, "freeze", func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "title":
			f.Title, err = name(key, v)
		case "team":
			if f.Team, err = name(key, v); err == nil && !declared[strings.TrimSpace(f.Team)] {
				return badAt(v, "a change freeze's team %s isn't one this file declares: declare it under teams", f.Team)
			}
		case "start":
			f.Start, err = when(key, v, loc)
		case "end":
			f.End, err = when(key, v, loc)
		case "every":
			f.Every, err = text(key, v)
		case "repeat_on":
			f.On, err = text(key, v)
		case "until":
			var s string
			if s, err = text(key, v); err == nil && s != "" {
				if day, derr := time.ParseInLocation("2006-01-02", s, loc); derr == nil {
					f.Until = midnight(day, loc)
				} else {
					f.Until, err = when(key, v, loc)
				}
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(f.Title) == "" {
		return badAt(n, "a change freeze has a title")
	}
	f.Team = strings.TrimSpace(f.Team) // as the ledger keeps a team's name
	w := ledger.Window{Title: f.Title, Start: f.Start, End: f.End, Every: f.Every, On: f.On, Until: f.Until}
	if err := ledger.CheckSpan(&w, loc); err != nil {
		return atLine(n.Line, err)
	}
	// Kept as instants: however the file writes a time, the same one is
	// kept the same.
	f.Title, f.On = w.Title, w.On
	f.Start, f.End, f.Until = w.Start.UTC(), w.End.UTC(), w.Until.UTC()
	if w.Until.IsZero() {
		f.Until = time.Time{}
	}
	b.Freezes = append(b.Freezes, f)
	return nil
}

// boundsOK checks what a file declares of where a team's rules are written
// against --changes-in, which it may only narrow: a file that names more
// is refused as a whole, as one with a key it doesn't know is.
func (s *Server) boundsOK(d ledger.Declared) error {
	if d.Bounds == "" {
		return nil
	}
	var b bounds
	if err := json.Unmarshal([]byte(d.Bounds), &b); err != nil {
		return err
	}
	for _, team := range sortedKeys(b.Teams) {
		for _, x := range b.Teams[team].In {
			if !s.withinChangesIn(x) {
				return bad("team %s's changes_in %s isn't within --changes-in: a team's sources are some of those --changes-in names", team, x)
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// withinChangesIn reports whether an entry a team's changes_in gives is
// within what --changes-in names: an entry of it, or a name or a narrower
// prefix within one of its prefixes, as written. An alarm's entry
// (SOURCE:NAME) is within one of the same source alone, a Loki ruler's
// (loki:NAMESPACE) within one of Loki's, a ruler's within one of the
// ruler's; each in any letter case where its source compares
// names so (foldsIn): Azure's rule groups and Azure's alarms.
func (s *Server) withinChangesIn(x string) bool {
	src, alarm := s.alarmsSource(x)
	fold := s.foldsIn(x)
	if fold {
		x = strings.ToLower(x)
	}
	for _, pat := range s.cfg.ChangesIn {
		if p, a := s.alarmsSource(pat); a != alarm || p != src {
			continue // another source's
		}
		if fold {
			pat = strings.ToLower(pat)
		}
		if x == pat {
			return true
		}
		if p, ok := strings.CutSuffix(pat, "*"); ok && strings.HasPrefix(strings.TrimSuffix(x, "*"), p) {
			return true
		}
	}
	return false
}

// alarmsSource is the source of --alarms an entry of --changes-in names an
// alarm of (SOURCE:NAME, alarmsEntry), ok false for a ruler's entry.
func (s *Server) alarmsSource(x string) (src string, ok bool) {
	if s.lokiEntry(x) {
		// Loki's ruler's namespaces (loki:NAMESPACE): within one of its own
		// alone, as an alarm's within its source's.
		return lokiIn, true
	}
	if !s.alarmsEntry(x) {
		return "", false
	}
	src, _, _ = strings.Cut(x, ":")
	return src, true
}

// foldsIn reports whether an entry of --changes-in names what it names in
// any letter case, as its own source compares names: an alarm's, by its
// source's kind (changesInAlarm), a ruler's by the ruler's (changesIn).
func (s *Server) foldsIn(x string) bool {
	if s.lokiEntry(x) {
		return false // Loki compares namespaces as written
	}
	if src, ok := s.alarmsSource(x); ok {
		return s.alarmsOf(src).Kind == source.AlarmsAzure
	}
	return s.ruler != nil && s.ruler.Kind == source.RulerAzure
}

// boundsCache keeps the boundaries the ledger holds until it changes.
type boundsCache struct {
	sync.Mutex
	gen int64
	b   *bounds
}

// bounds are the boundaries of change the ledger holds, as the file the
// ledger was last kept to declared them; none without one. Each freeze is
// checked again, by the calendar of --tz. One that can't be read is an
// error: nothing a boundary holds is approved or applied meanwhile.
func (s *Server) bounds(ctx context.Context) (*bounds, error) {
	gen := s.l.Gen()
	s.bnd.Lock()
	defer s.bnd.Unlock()
	if s.bnd.b != nil && s.bnd.gen == gen {
		return s.bnd.b, nil
	}
	raw, err := s.l.Meta(ctx, ledger.BoundsKey)
	if err != nil {
		return nil, err
	}
	b := &bounds{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), b); err != nil {
			return nil, fmt.Errorf("the change boundaries the ledger holds can't be read: %w", err)
		}
	}
	for i := range b.Freezes {
		f := &b.Freezes[i]
		f.w = ledger.Window{Title: f.Title, Start: f.Start, End: f.End, Every: f.Every, On: f.On, Until: f.Until}
		if err := ledger.CheckSpan(&f.w, s.cfg.Loc); err != nil {
			return nil, fmt.Errorf("the change freeze %q the ledger holds can't be read: %w", f.Title, err)
		}
	}
	s.bnd.gen, s.bnd.b = gen, b
	return b, nil
}

// frozen is the freeze that holds a change to the rules of teams now: one
// for everyone, or for one of the teams; the one that ends last, and when
// it ends. ok is false when none does.
func (b *bounds) frozen(teams []string, now time.Time) (f freeze, until time.Time, ok bool) {
	for _, x := range b.Freezes {
		if x.Team != "" && !slices.Contains(teams, x.Team) {
			continue
		}
		if end, in := x.w.EndOf(now); in && (!ok || end.After(until)) {
			f, until, ok = x, end, true
		}
	}
	return f, until, ok
}

// teamRef is a team as a proposal keeps it: its id and its name then; id 0
// for no team.
type teamRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// ownedBy is who owned a proposal's rule as it was proposed, kept in its
// evidence: the team of the rule as the ledger names it, of the rule as it
// was read again and of the rule as proposed, each once; none when no
// team did. One team is a team's proposal, which its approvers may
// approve; more are no one team's, which only the approvers of every team
// may. Owned says they were kept: a proposal made before they were has
// none, and is approved and carried out as it was then.
type ownedBy struct {
	Owners []teamRef `json:"owners,omitempty"`
	Owned  bool      `json:"owned,omitempty"`
}

// team is the one team that owned it, ok false for none or more than one.
func (w ownedBy) team() (teamRef, bool) {
	if !w.Owned || len(w.Owners) != 1 || w.Owners[0].ID == 0 {
		return teamRef{}, false
	}
	return w.Owners[0], true
}

// ownerOf is the team that owns a rule of a name and labels, as owners
// works it out for the rules the ledger holds: a note's team, the first
// team whose pattern matches the name, the first whose labels it has; 0
// for none.
func (o *owners) ownerOf(name string, labels map[string]string) int64 {
	if n, ok := o.notes[name]; ok && n.TeamID != 0 {
		return n.TeamID
	}
	return o.bareOf(name, labels)
}

// bareOf is the team that owns a rule of a name and labels by them alone,
// its note left out: the first team whose pattern matches the name, the
// first whose labels it has; 0 for none. A rule a note moved into a team
// is still held by this one's boundaries too.
func (o *owners) bareOf(name string, labels map[string]string) int64 {
	for i, t := range o.teams {
		if i < len(o.res) && o.res[i] != nil && o.res[i].MatchString(name) {
			return t.ID
		}
	}
	for _, t := range o.teams {
		if len(t.Labels) > 0 && has(labels, t.Labels) {
			return t.ID
		}
	}
	return 0
}

// owner is the team that owns a rule of a name, 0 for none: as the pages
// say it of a rule that fired (rule), and as they would of one in effect
// that never did, by the labels it has.
func (o *owners) owner(name string) int64 {
	if id := o.rule[name]; id != 0 {
		return id
	}
	return o.ownerOf(name, o.verLabels[name])
}

// bare is the team that owns a rule of a name by its name and labels
// alone, as owner would without its note; 0 for none.
func (o *owners) bare(name string) int64 { return o.bareOf(name, o.verLabels[name]) }

// refOf is a team the ledger holds, as a proposal keeps it.
func (o *owners) refOf(id int64) teamRef {
	if id == 0 {
		return teamRef{}
	}
	return teamRef{ID: id, Name: o.byID[id].Name}
}

// mayProposeHere refuses a team's editor a proposal for a rule their team
// doesn't own, as the ledger names it: asked before anything is read for
// it. propose asks again of the rule as it is read again and as proposed.
func (s *Server) mayProposeHere(ctx context.Context, rule string) error {
	teams, limited := ledger.TeamsOf(ctx)
	if !limited {
		return nil
	}
	o, err := s.owners(ctx)
	if err != nil {
		return err
	}
	if id := o.owner(rule); id == 0 || !slices.Contains(teams, id) {
		return ledger.ErrNotYourTeam
	}
	if id := o.bare(rule); id != 0 && !slices.Contains(teams, id) {
		return ledger.ErrNotYourTeam // a note moved it out of another team's
	}
	return nil
}

// proposedOwners are who owned a rule as it is proposed: the team of its
// name as the ledger has it, of the rule as it was read again (was), and
// of it as proposed (want), whose name or labels a change could move to
// another team; and of each, the team its name and labels give it without
// its note, when they give one (bareOf): a rule a note moved out of a
// team is no one team's. A team's editor is refused unless each is a team
// they edit: a proposal for another team's rule, or one that would make
// theirs another team's, isn't theirs to make.
func (s *Server) proposedOwners(ctx context.Context, f found, was, want rules.Rule) (ownedBy, error) {
	o, err := s.owners(ctx)
	if err != nil {
		return ownedBy{}, err
	}
	name := f.name
	if want.Name != was.Name {
		name = want.Name
	}
	ids := []int64{o.owner(f.name), o.ownerOf(f.name, was.AlertLabels()), o.ownerOf(name, want.AlertLabels())}
	for _, id := range []int64{o.bare(f.name), o.bareOf(f.name, was.AlertLabels()), o.bareOf(name, want.AlertLabels())} {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	if teams, limited := ledger.TeamsOf(ctx); limited {
		for _, id := range ids {
			if id == 0 || !slices.Contains(teams, id) {
				return ownedBy{}, ledger.ErrNotYourTeam
			}
		}
	}
	out := ownedBy{Owned: true}
	for _, id := range ids {
		if !slices.ContainsFunc(out.Owners, func(t teamRef) bool { return t.ID == id }) {
			out.Owners = append(out.Owners, o.refOf(id))
		}
	}
	if len(out.Owners) == 1 && out.Owners[0].ID == 0 {
		out.Owners = nil
	}
	return out, nil
}

// addOwners are who would own a rule added, of a name and the labels it
// is added with, as owners will once it is in effect and teamsNow says of
// its proposal: the team they give it, its note first, and the team its
// name and labels give it without its note, when they give one. A team's
// editor is refused unless each is a team they edit, as a proposal for a
// rule in effect is (proposedOwners).
func (s *Server) addOwners(ctx context.Context, name string, labels map[string]string) (ownedBy, error) {
	o, err := s.owners(ctx)
	if err != nil {
		return ownedBy{}, err
	}
	ids := []int64{o.ownerOf(name, labels)}
	if id := o.bareOf(name, labels); id != 0 {
		ids = append(ids, id)
	}
	if teams, limited := ledger.TeamsOf(ctx); limited {
		for _, id := range ids {
			if id == 0 || !slices.Contains(teams, id) {
				return ownedBy{}, ledger.ErrNotYourTeam
			}
		}
	}
	out := ownedBy{Owned: true}
	for _, id := range ids {
		if id != 0 && !slices.ContainsFunc(out.Owners, func(t teamRef) bool { return t.ID == id }) {
			out.Owners = append(out.Owners, o.refOf(id))
		}
	}
	return out, nil
}

// addMoved says why a rule added, read back where it goes again as it is
// carried out, is refused for its team: its group's labels changed since
// it was proposed, and the team its name and those labels give it now
// isn't the one team that owned it then (teamMoved), "" when it is. Its
// team's approvers approved it as it was proposed, and its proposal keeps
// the labels it was read back with then (teamsNow asks of those).
func (s *Server) addMoved(ctx context.Context, p ledger.Proposal, ev evidence, labels map[string]string) (string, error) {
	o, err := s.owners(ctx)
	if err != nil {
		return "", err
	}
	now := o.refOf(o.ownerOf(p.Rule, labels))
	if from, ok := teamMoved(ev.ownedBy, now); ok {
		return teamMovedText(from.Name, now.Name), nil
	}
	return "", nil
}

// addLabels are the labels a proposal adding a rule gives it, as it was
// read back where it goes, its group's with its own, which the rule has
// once it is in effect; kept before they were, its own as proposed. A
// refusal when it holds no rule that reads (addedOf): its team can't be
// told, and it isn't carried out (heldByBounds).
func addLabels(p ledger.Proposal) (map[string]string, error) {
	ev := proposalEvidence(p)
	if ev.Add != nil && ev.Add.Labels != nil {
		return ev.Add.Labels, nil
	}
	a, err := addedOf(ev)
	if err != nil {
		return nil, err
	}
	return a.Labels, nil
}

// addPending says a proposal adds a rule not in effect: one not carried
// out yet, or carried out and not yet in the rules read since. Its team is
// asked of its name and the labels it is added with (addLabels), as the
// rules read hold neither.
func addPending(p ledger.Proposal, o *owners) bool {
	if p.Kind != ledger.KindAdd {
		return false
	}
	switch p.State {
	case ledger.StateApplied, ledger.StateMerged, ledger.StateAmended, ledger.StateUndone:
		_, read := o.verLabels[p.Rule]
		return !read
	}
	return true
}

// teamNow is the team that owns a proposal's rule now, as the pages name
// it; ID 0 for none.
func (s *Server) teamNow(ctx context.Context, p ledger.Proposal) (teamRef, error) {
	now, _, err := s.teamsNow(ctx, p)
	return now, err
}

// teamsNow are the team that owns a proposal's rule now (teamNow), and the
// team its name and labels give it without its note (bare); ID 0 for
// none. Of a rule added not yet in effect, those its name and the labels
// it is added with give it, as they will once it is: an error, which holds
// it, when they can't be read.
func (s *Server) teamsNow(ctx context.Context, p ledger.Proposal) (now, bare teamRef, err error) {
	o, err := s.owners(ctx)
	if err != nil {
		return teamRef{}, teamRef{}, err
	}
	if addPending(p, o) {
		labels, err := addLabels(p)
		if err != nil {
			return teamRef{}, teamRef{}, err
		}
		return o.refOf(o.ownerOf(p.Rule, labels)), o.refOf(o.bareOf(p.Rule, labels)), nil
	}
	return o.refOf(o.owner(p.Rule)), o.refOf(o.bare(p.Rule)), nil
}

// teamMoved says a proposal one team owned as it was proposed is another's,
// or no team's, now: approved by that team's approvers, it is refused, and
// it isn't approved by anyone, but proposed again.
func teamMoved(w ownedBy, now teamRef) (from teamRef, ok bool) {
	t, one := w.team()
	if !one || t.ID == now.ID && t.Name == now.Name {
		return teamRef{}, false
	}
	return t, true
}

// teamsOf are the names of the teams whose boundaries hold a proposal: the
// team that owns its rule now, the team its name and labels give it
// without its note (bare), and those that did as it was proposed.
func teamsOf(w ownedBy, now, bare teamRef) []string {
	var out []string
	for _, t := range append([]teamRef{now, bare}, w.Owners...) {
		if t.Name != "" && !slices.Contains(out, t.Name) {
			out = append(out, t.Name)
		}
	}
	return out
}

// movedFormats are why a proposal whose rule another team owns now than
// as it was proposed isn't approved, or carried out: to no team, or to
// another.
const (
	movedNone  = "its rule was %s's when it was proposed and is no team's now: propose it again"
	movedOther = "its rule was %s's when it was proposed and is %s's now: propose it again, for %s's approvers"
)

// teamMovedErr is why, as a refusal a person reads in their language.
func teamMovedErr(from, to string) error {
	if to == "" {
		return bad(movedNone, from)
	}
	return bad(movedOther, from, to, to)
}

// teamMovedText is why, as a proposal's reason keeps it.
func teamMovedText(from, to string) string { return teamMovedErr(from, to).Error() }

// frozenErr is an approval, or an undo asked for, while a change freeze
// holds its rule: refused, the freeze named, in the reader's language.
type frozenErr struct{ badRequest }

func (e frozenErr) Unwrap() error { return e.badRequest }

func (s *Server) frozenErr(title string, until time.Time) error {
	return frozenErr{bad("a change freeze, %s, holds it until %s: nothing is approved or applied until then", title,
		until.In(s.cfg.Loc).Format("2006-01-02 15:04")).(badRequest)}
}

// freezeNote is why an approved change waits during a freeze, kept as its
// reason once; thawedNote, what it says once it was carried out after.
const (
	freezeNote = "a change freeze, %s, holds it until %s: it waits, approved, and is carried out once the freeze ends"
	thawedNote = "the change freeze ended, and it was carried out then"
)

// frozeNote reports whether a reason is a freeze's note.
func frozeNote(reason string) bool {
	return strings.HasPrefix(reason, "a change freeze, ") && strings.Contains(reason, "it waits, approved")
}

// approverOf reports whether the reader may approve, or ask to undo, a
// proposal's change, as its rule's team is now: an approver of every team
// (approver), or an approver its team's declares. A team's approver may
// approve only what one team, theirs, owned as it was proposed and owns
// now.
func (s *Server) approverOf(r *http.Request, p ledger.Proposal) bool {
	if s.approver(r) {
		return true
	}
	t, ok := s.teamApproved(r, p)
	return ok && s.teamApprover(r, t.Name)
}

// teamApproved is the team whose approvers may approve a proposal: the one
// team that owns its rule now, with its note and without it (a rule a
// note moved out of another team is no one team's), and, unless it is an
// undo, owned it as it was proposed.
func (s *Server) teamApproved(r *http.Request, p ledger.Proposal) (teamRef, bool) {
	now, bare, err := s.teamsNow(r.Context(), p)
	if err != nil || now.ID == 0 || bare.ID != 0 && bare != now {
		return teamRef{}, false
	}
	if p.Kind == ledger.KindUndo || p.State == ledger.StateApplied {
		return now, true // asked for now, by its team's approvers now
	}
	t, one := proposalEvidence(p).ownedBy.team()
	if !one || t != now {
		return teamRef{}, false
	}
	return t, true
}

// teamApprover reports whether the reader is one of a team's approvers,
// as the sign-in names them and their groups (listed): with --changes on,
// not --read-only, and someone signed in.
func (s *Server) teamApprover(r *http.Request, team string) bool {
	if s.cfg.ReadOnly || !s.cfg.Changes || s.user(r) == "anonymous" || team == "" {
		return false
	}
	b, err := s.bounds(r.Context())
	if err != nil {
		return false
	}
	list := b.Teams[team].Approvers
	return len(list) > 0 && s.listed(r, list)
}

// anyTeamApprover reports whether the reader is an approver of some team:
// let past person to the proposal, which says whose it is.
func (s *Server) anyTeamApprover(r *http.Request) bool {
	b, err := s.bounds(r.Context())
	if err != nil {
		return false
	}
	for team := range b.Teams {
		if s.teamApprover(r, team) {
			return true
		}
	}
	return false
}

// mayApproveNow refuses an approval, or an undo asked for, that the
// boundaries don't let through now: the rule another team's than it was
// as it was proposed; a freeze that holds it; a reader who is no approver
// of every team, nor of its one team.
func (s *Server) mayApproveNow(r *http.Request, p ledger.Proposal) error {
	ctx := r.Context()
	b, err := s.bounds(ctx)
	if err != nil {
		return err
	}
	now, bare, err := s.teamsNow(ctx, p)
	if err != nil {
		return err
	}
	w := proposalEvidence(p).ownedBy
	if p.State == ledger.StateProposed {
		if from, ok := teamMoved(w, now); ok {
			return teamMovedErr(from.Name, now.Name)
		}
	} else {
		w = ownedBy{} // an undo is its team's now
	}
	if f, until, ok := b.frozen(teamsOf(w, now, bare), s.wall()); ok {
		return s.frozenErr(f.Title, until)
	}
	if !s.approverOf(r, p) {
		return errNotApprover
	}
	return nil
}

// frozenView is a freeze that holds a proposal, as its page says it.
type frozenView struct {
	Title, Team string
	Until       time.Time
}

// frozenFor is the freeze that holds a proposal now (frozenNow), nil for
// none; boundaries that can't be read are said as one that holds it, so
// that nothing they would hold goes ahead meanwhile.
func (s *Server) frozenFor(ctx context.Context, p ledger.Proposal) *frozenView {
	f, err := s.frozenNow(ctx, p)
	if err != nil {
		return &frozenView{Title: "?"}
	}
	return f
}

// heldByBounds says whether an approved proposal waits, or was refused,
// for the boundaries, as it is about to be carried out: refused when one
// team owned its rule as it was proposed and another, or none, does now,
// or when where it is kept is outside its team's changes_in; waiting while
// a freeze holds it, said once, or while the boundaries can't be read.
func (s *Server) heldByBounds(ctx context.Context, p ledger.Proposal) (bool, error) {
	b, err := s.bounds(ctx)
	if err != nil {
		s.log.Warn("an approved change waits: the change boundaries can't be read", "proposal", p.ID, "err", err)
		return true, nil
	}
	refuse := func(why string) (bool, error) {
		_, err := s.l.Move(ctx, p.ID, []string{ledger.StateApproved}, ledger.StateRefused, ledger.Proposer, ledger.Step{Reason: clip(why, 500)})
		if errors.Is(err, ledger.ErrMoved) {
			err = nil
		}
		return true, err
	}
	now, bare, err := s.teamsNow(ctx, p)
	var no refusal
	switch {
	case errors.As(err, &no):
		return refuse(no.Error()) // a rule added whose team can't be told
	case err != nil:
		return true, err
	}
	ev := proposalEvidence(p)
	if from, ok := teamMoved(ev.ownedBy, now); ok {
		return refuse(teamMovedText(from.Name, now.Name))
	}
	teams := teamsOf(ev.ownedBy, now, bare)
	if tg := s.targetOf(p.Path); tg != nil && ev.Edit != nil {
		why, wait := s.outsideTeamIn(b, tg, teams, p.Source, *ev.Edit)
		switch {
		case wait:
			return true, nil
		case why != "":
			return refuse(why)
		}
	}
	if f, until, ok := b.frozen(teams, s.wall()); ok {
		note := fmt.Sprintf(freezeNote, f.Title, until.In(s.cfg.Loc).Format("2006-01-02 15:04"))
		if p.Reason != note {
			if _, err := s.l.Move(ctx, p.ID, []string{ledger.StateApproved}, ledger.StateApproved, ledger.Proposer, ledger.Step{Reason: note}); err != nil && !errors.Is(err, ledger.ErrMoved) {
				return true, err
			}
		}
		return true, nil
	}
	return false, nil
}

// errBoundsHold is a change its team's boundaries hold as it is about to be
// sent or handed out (boundsAgain): it waits, approved, or was refused, as
// heldByBounds says, and the round goes on.
var errBoundsHold = errors.New("held by its team's boundaries of change")

// boundsAgain asks a change's team's boundaries again just before it is
// sent or handed out, as the pause is asked there: checking it again can
// take as long as a backtest (--timeout), and meanwhile a freeze may have
// begun, its rule moved to another team, or its team's changes_in
// narrowed. Held, it is errBoundsHold (heldByBounds says it once, or
// refuses it); boundaries that can't be asked again hold it too.
func (s *Server) boundsAgain(ctx context.Context, p ledger.Proposal) error {
	if err := s.l.Refresh(ctx); err != nil {
		if ctx.Err() != nil {
			return err
		}
		s.log.Warn("an approved change waits: the ledger couldn't be read again just before it was written", "proposal", p.ID, "err", err)
		return errBoundsHold
	}
	held, err := s.heldByBounds(ctx, p)
	switch {
	case err != nil && ctx.Err() != nil:
		return err
	case err != nil:
		s.log.Warn("an approved change waits: its team's boundaries couldn't be asked again just before it was written", "proposal", p.ID, "err", err)
		return errBoundsHold
	case held:
		return errBoundsHold
	}
	return nil
}

// frozenReady says whether a change checked and ready for a recipe waits
// while a freeze holds it, or while the boundaries can't be read: it isn't
// handed out until it ends.
func (s *Server) frozenReady(ctx context.Context, p ledger.Proposal) bool {
	return s.frozenFor(ctx, p) != nil
}

// alsoFrozen are, beside held, the proposals --approvals lowered would
// approve that a freeze holds: approving is what a freeze stops. Unread,
// none is approved so this round (carryOut).
func (s *Server) alsoFrozen(ctx context.Context, held []int64) ([]int64, error) {
	ps, err := s.l.Promotable(ctx, s.approvals())
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if !slices.Contains(held, p.ID) && s.frozenFor(ctx, p) != nil {
			held = append(held, p.ID)
		}
	}
	return held, nil
}

// outsideTeamIn is why a change through a ruler or a cloud's alarms isn't
// written where it is kept, for the teams of its rule: one of them
// declares changes_in, and it doesn't name the namespace and group, or the
// alarm. wait says what it names can't be told yet (an alarm not read
// since a restart).
func (s *Server) outsideTeamIn(b *bounds, tg target, teams []string, src string, e rules.Edit) (why string, wait bool) {
	for _, team := range teams {
		tb := b.Teams[team]
		if !tb.Narrowed {
			continue
		}
		switch t := tg.(type) {
		case rulerTarget:
			if t.loki {
				if !lokiInOf(tb.In, s.loki != nil, e.Namespace) {
					return noPathTeamIn, false
				}
				continue
			}
			if !s.inOf(tb.In, e.Namespace, e.Group) {
				return noPathTeamIn, false
			}
		case alarmTarget:
			a := s.alarmsOf(e.Namespace)
			if a == nil {
				return noPathTeamIn, false
			}
			d, ok := a.Doc(src)
			if !ok {
				return "", true
			}
			if !alarmInOf(tb.In, a.Source, rules.AlarmName(d.Data, cloudOf[a.Kind]), a.Kind) {
				return noPathTeamIn, false
			}
		default:
			return noPathTeamIn, false
		}
	}
	return "", false
}

// teamInWhy is why a rule read through an API isn't written there, for the
// teams that own it as it is proposed: apiPropose's.
func (s *Server) teamInWhy(ctx context.Context, tg target, w ownedBy, src string, e rules.Edit) (string, error) {
	b, err := s.bounds(ctx)
	if err != nil {
		return "", err
	}
	var teams []string
	for _, t := range w.Owners {
		if t.Name != "" {
			teams = append(teams, t.Name)
		}
	}
	why, wait := s.outsideTeamIn(b, tg, teams, src, e)
	if wait {
		return noPathTeamIn, nil // read just now: not told apart, it isn't written
	}
	return why, nil
}

// inOf is changesIn of a team's own list: whether it names a namespace
// (Mimir, Cortex, Amazon) or a rule group (Azure).
func (s *Server) inOf(pats []string, ns, group string) bool {
	if s.ruler == nil {
		return false
	}
	for _, pat := range pats {
		if s.alarmsEntry(pat) || s.lokiEntry(pat) {
			continue
		}
		if s.ruler.Kind != source.RulerAzure {
			if namedBy(pat, ns, false) {
				return true
			}
			continue
		}
		if !strings.Contains(pat, "/") {
			rg := s.ruler.ResourceGroup()
			if rg == "" {
				continue
			}
			pat = rg + "/" + pat
		}
		if namedBy(pat, ns+"/"+group, true) {
			return true
		}
	}
	return false
}

// alarmInOf is changesInAlarm of a team's own list.
func alarmInOf(pats []string, src, name, kind string) bool {
	for _, pat := range pats {
		x, p, ok := strings.Cut(pat, ":")
		if ok && x == src && namedBy(p, name, kind == source.AlarmsAzure) {
			return true
		}
	}
	return false
}

// teamProposes reports whether the reader is an editor of the team that
// owns a rule now, when they are no editor of every team, and the team's
// name, as a rule's page offers its forms.
func (s *Server) teamProposes(r *http.Request, rule string) (bool, string) {
	o, err := s.owners(r.Context())
	if err != nil {
		return false, ""
	}
	id, bare := o.owner(rule), o.bare(rule)
	if bare != 0 && bare != id {
		return false, "" // no one team's: an editor of every team's
	}
	teams := s.editsTeams(r)
	return id != 0 && slices.Contains(teams, id), o.byID[id].Name
}

// proposalInScope refuses a team's editor what they ask of a proposal
// (backtesting it again, dropping it) when its rule isn't their team's
// now, nor was as it was proposed.
func (s *Server) proposalInScope(r *http.Request, p ledger.Proposal) error {
	teams, limited := scoped(r)
	if !limited {
		return nil
	}
	now, bare, err := s.teamsNow(r.Context(), p)
	if err != nil {
		return err
	}
	if now.ID == 0 || !slices.Contains(teams, now.ID) || bare.ID != 0 && !slices.Contains(teams, bare.ID) {
		return ledger.ErrNotYourTeam
	}
	for _, t := range proposalEvidence(p).Owners {
		if t.ID == 0 || !slices.Contains(teams, t.ID) {
			return ledger.ErrNotYourTeam
		}
	}
	return nil
}

// ownDroppable are the states a team's editor may drop a proposal from:
// one they made, or an agent made for them, waiting for approval or to be
// carried out, of a rule their team owns.
func (s *Server) ownDroppable(r *http.Request, p ledger.Proposal) []string {
	if s.cfg.ReadOnly || s.role(r) >= editor || ledger.FoldName(p.MadeBy) != ledger.FoldName(s.user(r)) || s.user(r) == "anonymous" {
		return nil
	}
	teams := s.editsTeams(r)
	if len(teams) == 0 {
		return nil
	}
	if err := s.proposalInScope(r.WithContext(ledger.WithTeams(r.Context(), teams)), p); err != nil {
		return nil
	}
	return []string{ledger.StateProposed, ledger.StateApproved}
}
