package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/report"
)

// Who is told what (docs/design/notifications.md, "Who is told what").
// --config's notify: declares routes: each takes events by their kind,
// their team and their labels, and names the destinations they go to, in
// order, the first that takes one deciding unless it says to go on; what
// none takes goes to the default, which routes need, so that nothing is
// dropped. Without routes, what happens is told where it was before them:
// to --notify-webhook-file and, with --notify-slack, to Slack.
//
// The outbox keeps a row for each place a notice goes, so an event is
// routed as it is written (ledger.RouteBy): a place no route names gets no
// row. Destinations are named by the flags, which read their URLs from
// files, as before: webhook, slack, team:NAME (--team-digest), the names
// --report-dest gives Slack-style webhooks, and those --webhook-dest gives
// JSON ones, each signed with its own key. A route names destinations
// alone, never a URL, and a label only picks among the routes.
//
// Each replica routes what it writes, so each reads the file, as the
// leader does for what the ledger keeps of it: a file is in force once it
// has been read the same twice in a row, configEvery apart, and one that
// can't be used leaves the last that could.

// routeKinds are the kinds of what happens that routes send, as the
// outbox names them: a route's kinds are among these, or a family of them
// (task.*). A kind the outbox is given is a line here, for routes to name
// it; one not here is routed all the same, by the routes that name no
// kind, or else to the default.
var routeKinds = []string{
	"task.created", "task.assigned", "task.status", "task.overdue",
	"incident.written",
	"window.kept",
	"marked.kept",
	"comment.written",
	"proposal.made", "proposal.approved", "proposal.backtested",
	"change.opened", "change.applied", "change.refused", "change.failed", "change.undone", "change.amended",
	"replay.differs",
	"goal.missed",
	"source.quiet", "source.down", "source.back",
}

// ownWay are the kinds that go where their own settings send them, never
// routed: the weekly digest, where the Reports page or the flags send it,
// and a scheduled report, to the places it names.
var ownWay = []string{"digest.ready", "report.ready"}

// routedPrefix marks, in the outbox, a notice to a destination a route
// named, sent by its name: route:NAME. The webhook keeps its own name,
// webhook, and so does Slack with --notify-slack, slack.
const routedPrefix = "route:"

// Bounds on what notify: declares.
const (
	maxRoutes     = 100
	maxRouteList  = 50
	maxRouteLabel = 20
)

// The keys of notify: and of a route, beside --config's own (declared.go).
func init() {
	configKeys[""] = append(configKeys[""], "notify")
	configKeys["notify"] = []string{"routes", "default", "templates"}
	configKeys["route"] = []string{"kinds", "teams", "labels", "to", "continue"}
	unknownIn["notify"], unknownIn["route"] = notifyFormats[0], notifyFormats[1]
	wantA["notify"], wantA["route"] = notifyFormats[2], notifyFormats[3]
}

// notifyFormats are what can be wrong with notify:, as texts to translate.
var notifyFormats = []string{
	"unknown key %q in notify: want %s", "unknown key %q in a route: want %s", "want notify as a mapping of %s", "want a route, as a mapping of %s",
	"notify has nothing after it: give routes and a default, or templates; or leave the key out",
	"routes need a default: where what no route takes goes, so that nothing is dropped",
	"a route names where it sends, under to",
	"unknown kind %q: want one of %s, or a family of them, such as task.*",
	"%q isn't a destination's name: want webhook, slack, team:NAME, or lowercase letters, digits and . _ -",
	"there is no destination called %q: --notify-webhook-file names webhook, --slack-webhook-file slack, --team-digest team:NAME, and --webhook-dest and --report-dest the rest",
	"continue is true or false",
	"a team's name is a text of %d bytes at most",
	"notify declares %d routes at most",
	"a route names %d kinds, teams or destinations at most, each",
	"a route names %d labels at most",
	"templates are a mapping of a destination's name to its template",
	"the template for %s is more than %d bytes",
	"the template for %s opens {{ and doesn't close it",
	"the template for %s takes {{.Field}} alone, a field one of %s: not {{%s}}",
	"the template for %s is a text",
}

// notifyConf is what notify: declares.
type notifyConf struct {
	Routes      []noticeRoute
	Default     []string
	DefaultLine int
	Templates   map[string]*noticeTemplate
	// TemplateLines are the lines each template is on, by its destination.
	TemplateLines map[string]int
}

// declared says routes are declared, a default at least: without them,
// what happens is told as before them.
func (c *notifyConf) declared() bool {
	return c != nil && (len(c.Routes) > 0 || len(c.Default) > 0)
}

// noticeRoute is a route: the events it takes, by kind, team and labels,
// each left out for any; where it sends them; and whether the routes after
// it are looked at too.
type noticeRoute struct {
	Line     int
	Kinds    []string
	Teams    []string
	Labels   incident.Labels
	To       []string
	ToLine   int
	Continue bool
}

// atNode is a refusal of what a node of the file holds, at its line.
func atNode(n *yaml.Node, err error) error {
	var b badRequest
	if errors.As(err, &b) && b.format != "" {
		return configError{n.Line, b.format, b.args}
	}
	return err
}

// parseNotify reads notify:, strictly, as the rest of the file is read:
// its destinations are checked against the flags apart (checkNotify).
func parseNotify(n *yaml.Node) (*notifyConf, error) {
	if null(n) {
		return nil, badAt(n, "notify has nothing after it: give routes and a default, or templates; or leave the key out")
	}
	c := &notifyConf{}
	err := each(n, "notify", func(key string, v *yaml.Node) error {
		switch key {
		case "routes":
			if v.Kind != yaml.SequenceNode {
				return badAt(v, "%s is a list", key)
			}
			if len(v.Content) > maxRoutes {
				return badAt(v, "notify declares %d routes at most", maxRoutes)
			}
			for _, item := range v.Content {
				r, err := parseRoute(item)
				if err != nil {
					return err
				}
				c.Routes = append(c.Routes, r)
			}
		case "default":
			names, err := destNames(key, v)
			if err != nil {
				return err
			}
			c.Default, c.DefaultLine = names, v.Line
		case "templates":
			if v.Kind != yaml.MappingNode {
				return badAt(v, "templates are a mapping of a destination's name to its template")
			}
			c.Templates, c.TemplateLines = map[string]*noticeTemplate{}, map[string]int{}
			for i := 0; i+1 < len(v.Content); i += 2 {
				k, x := v.Content[i], v.Content[i+1]
				if k.Kind != yaml.ScalarNode || !validDestName(k.Value) {
					return badAt(k, "%q isn't a destination's name: want webhook, slack, team:NAME, or lowercase letters, digits and . _ -", k.Value)
				}
				if x.Kind != yaml.ScalarNode || null(x) {
					return badAt(x, "the template for %s is a text", k.Value)
				}
				t, err := parseTemplate(k.Value, x.Value)
				if err != nil {
					return atNode(x, err)
				}
				c.Templates[k.Value], c.TemplateLines[k.Value] = t, x.Line
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(c.Routes) > 0 && len(c.Default) == 0 {
		return nil, badAt(n, "routes need a default: where what no route takes goes, so that nothing is dropped")
	}
	return c, nil
}

// parseRoute reads a route.
func parseRoute(n *yaml.Node) (noticeRoute, error) {
	r := noticeRoute{Line: n.Line}
	err := each(n, "route", func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "kinds":
			if r.Kinds, err = texts(key, v); err == nil {
				for _, k := range r.Kinds {
					if !knownKind(k) {
						return badAt(v, "unknown kind %q: want one of %s, or a family of them, such as task.*", k, strings.Join(routeKinds, ", "))
					}
				}
			}
		case "teams":
			if r.Teams, err = texts(key, v); err == nil {
				for _, t := range r.Teams {
					if strings.TrimSpace(t) == "" || len(t) > maxTeamName {
						return badAt(v, "a team's name is a text of %d bytes at most", maxTeamName)
					}
				}
			}
		case "labels":
			r.Labels, err = labels(v)
			if err == nil && len(r.Labels) > maxRouteLabel {
				err = badAt(v, "a route names %d labels at most", maxRouteLabel)
			}
		case "to":
			r.To, err = destNames(key, v)
			r.ToLine = v.Line
		case "continue":
			var s string
			if s, err = text(key, v); err == nil {
				if r.Continue, err = strconv.ParseBool(s); err != nil {
					err = badAt(v, "continue is true or false")
				}
			}
		}
		if err == nil && max(len(r.Kinds), len(r.Teams), len(r.To)) > maxRouteList {
			err = badAt(v, "a route names %d kinds, teams or destinations at most, each", maxRouteList)
		}
		return err
	})
	if err != nil {
		return r, err
	}
	if len(r.To) == 0 {
		return r, badAt(n, "a route names where it sends, under to")
	}
	return r, nil
}

// destNames are the destinations a key names, each a name a destination
// can have; whether the flags give one is checked apart (checkNotify).
func destNames(key string, v *yaml.Node) ([]string, error) {
	names, err := texts(key, v)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		if !validDestName(name) {
			return nil, badAt(v, "%q isn't a destination's name: want webhook, slack, team:NAME, or lowercase letters, digits and . _ -", name)
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// validDestName says a name is one a destination can have: webhook,
// slack, team:NAME, or a name --report-dest or --webhook-dest gives.
func validDestName(name string) bool {
	return name == "webhook" || name == "slack" || ledger.ValidDest(name)
}

// knownKind says a route may name a kind: one of routeKinds, or a family
// of them, task.* for each task.KIND.
func knownKind(k string) bool {
	if fam, ok := strings.CutSuffix(k, ".*"); ok {
		return fam != "" && slices.ContainsFunc(routeKinds, func(x string) bool { return strings.HasPrefix(x, fam+".") })
	}
	return slices.Contains(routeKinds, k)
}

// kindIs says a kind is one a route's kind names: itself, or its family.
func kindIs(kind, named string) bool {
	if fam, ok := strings.CutSuffix(named, ".*"); ok {
		return strings.HasPrefix(kind, fam+".")
	}
	return kind == named
}

// notifyIn is what a --config file's notify: declares, nil for none: of a
// file ParseConfig took.
func notifyIn(b []byte) (*notifyConf, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(b)).Decode(&doc); err != nil {
		return nil, bad("the file isn't YAML: %s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "notify" {
			return parseNotify(root.Content[i+1])
		}
	}
	return nil, nil
}

// declaresNotify says a file that can't be used declares notify:, as far
// as can be told: its routes, then, couldn't be read.
func declaresNotify(b []byte) bool {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(b, &top); err != nil {
		return bytes.Contains(b, []byte("notify:"))
	}
	_, ok := top["notify"]
	return ok
}

// destination is a place a route can send to: its name, its row's in the
// outbox, whether it takes JSON (a webhook) or a line (Slack's way), the
// file holding its URL, the one holding its signing key, and the flag that
// gave it, which its errors say.
type destination struct {
	Name, Value, File, Secret, Flag string
	JSON                            bool
}

// destination is the one a name names; ok false for one the flags don't
// give.
func (s *Server) destination(name string) (destination, bool) {
	c := s.cfg
	d := destination{Name: name, Value: s.destValue(name)}
	switch {
	case name == "webhook":
		d.JSON, d.File, d.Secret, d.Flag = true, c.NotifyWebhook, c.NotifyWebhookSecret, "--notify-webhook-file"
	case name == "slack":
		d.File, d.Flag = c.Slack, "--slack-webhook-file"
	case strings.HasPrefix(name, "team:"):
		team := strings.TrimPrefix(name, "team:")
		d.File, d.Flag = c.TeamDigests[team], "--team-digest "+team
	case c.WebhookDests[name] != "":
		d.JSON, d.File, d.Secret, d.Flag = true, c.WebhookDests[name], c.WebhookDestSecrets[name], "--webhook-dest "+name
	default:
		d.File, d.Flag = c.ReportDests[name], "--report-dest "+name
	}
	return d, d.File != ""
}

// destValue is the outbox's place for a destination: webhook, and slack
// with --notify-slack, as before routes; any other, route:NAME.
func (s *Server) destValue(name string) string {
	if name == "webhook" || name == "slack" && s.cfg.NotifySlack {
		return name
	}
	return routedPrefix + name
}

// destNamesGiven are the names of every destination the flags give a
// route to send to, webhook and slack first.
func (s *Server) destNamesGiven() []string {
	c := s.cfg
	var out []string
	if c.NotifyWebhook != "" {
		out = append(out, "webhook")
	}
	if c.Slack != "" {
		out = append(out, "slack")
	}
	named := slices.Concat(slices.Collect(maps.Keys(c.WebhookDests)), slices.Collect(maps.Keys(c.ReportDests)))
	for team := range c.TeamDigests {
		if ledger.ValidDest("team:" + team) {
			named = append(named, "team:"+team)
		}
	}
	slices.Sort(named)
	return append(out, slices.Compact(named)...)
}

// routedDests are the outbox's places, beside the webhook's and Slack's,
// that a route may send to, with --config: route:NAME for each destination
// the flags give, which no route need name. Each has a loop that sends
// what the outbox holds for it, so that a route read later finds one.
func (s *Server) routedDests() []string {
	if s.cfg.Config == "" {
		return nil
	}
	var out []string
	for _, name := range s.destNamesGiven() {
		if v := s.destValue(name); strings.HasPrefix(v, routedPrefix) {
			out = append(out, v)
		}
	}
	return out
}

// checkNotify refuses routes that name a destination the flags don't
// give, at its line.
func (s *Server) checkNotify(c *notifyConf) error {
	if c == nil {
		return nil
	}
	check := func(line int, names []string) error {
		for _, name := range names {
			if _, ok := s.destination(name); !ok {
				return configError{line, "there is no destination called %q: --notify-webhook-file names webhook, --slack-webhook-file slack, --team-digest team:NAME, and --webhook-dest and --report-dest the rest", []any{name}}
			}
		}
		return nil
	}
	for _, r := range c.Routes {
		if err := check(r.ToLine, r.To); err != nil {
			return err
		}
	}
	if err := check(c.DefaultLine, c.Default); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(c.Templates)) {
		if err := check(c.TemplateLines[name], []string{name}); err != nil {
			return err
		}
	}
	return nil
}

// subject is what routes and templates read of an event: its kind, the
// rule it is about, the teams it is of and the labels it has, as the
// outbox keeps it and the owners have it; and for templates, its title,
// its id, who and when.
type subject struct {
	kind, rule, title, id, by string
	at                        time.Time
	teams                     []string
	labels                    map[string][]string
}

// toldOf is what subjectOf reads of what a notice tells: the parts of the
// row it is about that say a rule, a team or labels.
type toldOf struct {
	By   string    `json:"by"`
	At   time.Time `json:"at"`
	Task *struct {
		ID     int64  `json:"id"`
		Title  string `json:"title"`
		Rule   string `json:"rule"`
		TeamID int64  `json:"team_id"`
	} `json:"task"`
	Window *struct {
		ID     int64  `json:"id"`
		Title  string `json:"title"`
		TeamID int64  `json:"team_id"`
	} `json:"window"`
	Marked *struct {
		ID     int64             `json:"id"`
		Title  string            `json:"title"`
		TeamID int64             `json:"team_id"`
		Labels map[string]string `json:"labels"`
	} `json:"marked"`
	Comment *struct {
		ID  int64  `json:"id"`
		On  string `json:"on"`
		Key string `json:"key"`
	} `json:"comment"`
	Incident *struct {
		ID     string          `json:"id"`
		Title  string          `json:"title"`
		Labels incident.Labels `json:"labels"`
	} `json:"incident"`
	Proposal *struct {
		ID   int64  `json:"id"`
		Rule string `json:"rule"`
		Kind string `json:"kind"`
	} `json:"proposal"`
	Goal *struct {
		Name  string `json:"name"`
		Team  string `json:"team"`
		Teams []struct {
			Team string `json:"team"`
		} `json:"teams"`
	} `json:"goal"`
	Source *struct {
		Name string `json:"name"`
	} `json:"source"`
	// A rule's replay that differs (replay.differs); a team, of a kind
	// that says one.
	Rule string `json:"rule"`
	Team string `json:"team"`
}

// subjectOf is what routes read of an event: what is told of it, its rule's
// team and labels as o has them, and teams by their ids. With no owners
// worked out yet, nil, an event has the teams and labels it carries alone.
func subjectOf(kind string, about []byte, o *owners) subject {
	var x toldOf
	json.Unmarshal(about, &x) // what doesn't read says nothing
	sub := subject{kind: kind, by: x.By, at: x.At, labels: map[string][]string{}}
	team := func(name string) {
		if name != "" && name != "*" && !slices.Contains(sub.teams, name) {
			sub.teams = append(sub.teams, name)
		}
	}
	teamID := func(id int64) bool {
		if o == nil || id <= 0 {
			return false
		}
		t, ok := o.byID[id]
		team(t.Name)
		return ok
	}
	rule := func(name string, owned bool) {
		sub.rule = name
		if o == nil || name == "" {
			return
		}
		if !owned {
			team(o.teamOf(name))
		}
		for k, v := range o.ruleLabels[name] {
			sub.labels[k] = []string{v}
		}
	}
	switch {
	case x.Task != nil:
		sub.title, sub.id = x.Task.Title, strconv.FormatInt(x.Task.ID, 10)
		rule(x.Task.Rule, teamID(x.Task.TeamID))
	case x.Proposal != nil:
		sub.title, sub.id = x.Proposal.Kind, strconv.FormatInt(x.Proposal.ID, 10)
		rule(x.Proposal.Rule, false)
	case x.Comment != nil:
		sub.id = strconv.FormatInt(x.Comment.ID, 10)
		if x.Comment.On == "rule" {
			rule(x.Comment.Key, false)
		}
	case x.Window != nil:
		sub.title, sub.id = x.Window.Title, strconv.FormatInt(x.Window.ID, 10)
		teamID(x.Window.TeamID)
	case x.Marked != nil:
		sub.title, sub.id = x.Marked.Title, strconv.FormatInt(x.Marked.ID, 10)
		teamID(x.Marked.TeamID)
		for k, v := range x.Marked.Labels {
			sub.labels[k] = []string{v}
		}
	case x.Incident != nil:
		sub.title, sub.id = x.Incident.Title, x.Incident.ID
		maps.Copy(sub.labels, x.Incident.Labels)
		if o != nil {
			// The teams whose labels it has a value of, as a team owns an
			// incident it has caught nothing of yet (ownsIncident).
			for _, t := range o.teams {
				for k, vs := range t.Labels {
					if slices.ContainsFunc(x.Incident.Labels[k], func(v string) bool { return slices.Contains(vs, v) }) {
						team(t.Name)
						break
					}
				}
			}
		}
	case x.Goal != nil:
		sub.title = x.Goal.Name
		team(x.Goal.Team)
		for _, t := range x.Goal.Teams {
			team(t.Team)
		}
	case x.Source != nil:
		sub.title = x.Source.Name
	case x.Rule != "":
		rule(x.Rule, false)
	}
	team(x.Team)
	return sub
}

// takes says a route takes an event: of one of its kinds, of one of its
// teams (a team's name in any letter case), and with each of its labels at
// one of its values; each left out for any.
func (r noticeRoute) takes(sub subject) bool {
	if len(r.Kinds) > 0 && !slices.ContainsFunc(r.Kinds, func(k string) bool { return kindIs(sub.kind, k) }) {
		return false
	}
	if len(r.Teams) > 0 && !slices.ContainsFunc(r.Teams, func(t string) bool {
		return slices.ContainsFunc(sub.teams, func(u string) bool { return strings.EqualFold(t, u) })
	}) {
		return false
	}
	for k, vs := range r.Labels {
		have := sub.labels[k]
		if !slices.ContainsFunc(vs, func(v string) bool { return slices.Contains(have, v) }) {
			return false
		}
	}
	return true
}

// match are the destinations an event goes to: of the first route that
// takes it, and of each after it while the one before says to go on; with
// none, the default's.
func (c *notifyConf) match(sub subject) []string {
	var out []string
	for _, r := range c.Routes {
		if !r.takes(sub) {
			continue
		}
		for _, name := range r.To {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
		if !r.Continue {
			break
		}
	}
	if len(out) == 0 {
		return c.Default
	}
	return out
}

// routeTable is the routing in force: what notify: declared, in the file
// with the hash, read at when.
type routeTable struct {
	conf *notifyConf
	hash string
	at   time.Time
}

// routingState is what this replica made of notify: last. The routing in
// force; broken, that the file declares notify: and none could be read
// since serve started, so what happens is told as without routes;
// unrouted, the events that went where no route sent them since.
type routingState struct {
	table    atomic.Pointer[routeTable]
	broken   atomic.Bool
	unrouted atomic.Int64
	sync.Mutex
	// applied is the file in force's hash; pending, another read once, at
	// pendingAt; err why the last file read, errHash, can't be used, and
	// errAt when it was first read.
	applied, pending, errHash string
	pendingAt, errAt          time.Time
	err                       error
	// unroutedSaid is how many unrouted the log has said.
	unroutedSaid int64
}

// route is the outbox's Router: of the places an event may go, those the
// routes send it to. It reads the routing in force and the owners last
// worked out, and nothing else: it is asked in the write that tells.
func (s *Server) route(event string, about []byte, dests []string) []string {
	if slices.Contains(ownWay, event) {
		return dests
	}
	// Without routes: the webhook and Slack, as before them.
	before := slices.DeleteFunc(slices.Clone(dests), func(d string) bool { return strings.HasPrefix(d, routedPrefix) })
	rt := s.routing.table.Load()
	if rt == nil || !rt.conf.declared() {
		if rt == nil && s.routing.broken.Load() {
			// Routes declared, none read: as without them, or, with no
			// webhook or Slack to tell, everywhere, never nowhere.
			s.routing.unrouted.Add(1)
			if len(before) == 0 {
				return dests
			}
		}
		return before
	}
	got := s.valuesIn(rt.conf.match(subjectOf(event, about, s.ownersNow())), dests)
	if len(got) == 0 {
		// No route, not even the default, sent it anywhere there is: to
		// the default where it can go, or else everywhere.
		s.routing.unrouted.Add(1)
		if got = s.valuesIn(rt.conf.Default, dests); len(got) == 0 {
			got = dests
		}
	}
	return got
}

// valuesIn are the outbox's places for the destinations named, of dests.
func (s *Server) valuesIn(names, dests []string) []string {
	var out []string
	for _, name := range names {
		if v := s.destValue(name); slices.Contains(dests, v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// ownersNow are the owners last worked out, nil before any were: read,
// not worked out, as route is asked in a write.
func (s *Server) ownersNow() *owners {
	s.own.Lock()
	defer s.own.Unlock()
	return s.own.o
}

// readNotifyFile reads --config, as ReadConfig does, keeping its bytes.
func readNotifyFile(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfig+1))
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	if len(b) > maxConfig {
		return nil, hash, bad("the file is more than %d bytes", maxConfig)
	}
	return b, hash, nil
}

// loadRoutes reads --config's notify: and puts it in force: as serve
// starts (first, in Run), at once; after, once the same file has been read twice
// in a row, configEvery apart, as the leader keeps the ledger to it. A
// file that can't be used, as a whole or for its notify:, leaves the
// routing in force, and is said.
func (s *Server) loadRoutes(now time.Time, first bool) {
	if s.cfg.Config == "" {
		return
	}
	st := &s.routing
	b, hash, err := readNotifyFile(s.cfg.Config)
	if hash == "" {
		hash = "unread"
	}
	st.Lock()
	defer st.Unlock()
	switch {
	case hash == st.applied:
		st.pending, st.err, st.errHash = "", nil, ""
		return
	case err == nil && hash == st.errHash:
		return // read as it was, and as wrong
	}
	var c *notifyConf
	if err == nil {
		if _, err = ParseConfig(b, s.cfg.Loc); err == nil {
			if c, err = notifyIn(b); err == nil {
				err = s.checkNotify(c)
			}
		}
	}
	if err != nil {
		if hash != st.errHash {
			st.errAt = now
			s.log.Error("--config's notify: can't be used; the routes in force stay", "file", s.cfg.Config, "err", configText("en", err))
		}
		st.err, st.errHash, st.pending = err, hash, ""
		if first {
			st.broken.Store(declaresNotify(b))
		}
		return
	}
	st.err, st.errHash = nil, ""
	if !first && (hash != st.pending || now.Before(st.pendingAt.Add(configEvery))) {
		if hash != st.pending {
			st.pending, st.pendingAt = hash, now
		}
		return
	}
	st.table.Store(&routeTable{conf: c, hash: hash, at: now})
	st.applied, st.pending = hash, ""
	st.broken.Store(false)
	if c.declared() {
		s.log.Info("routes in force", "file", s.cfg.Config, "routes", len(c.Routes), "default", strings.Join(c.Default, ","))
	}
}

// watchRoutes reads --config's notify: again every configEvery, until ctx
// is done; and keeps the owners routes read worked out, and says in the
// log the events no route sent anywhere.
func (s *Server) watchRoutes(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(configEvery):
		}
		s.loadRoutes(time.Now(), false)
		if rt := s.routing.table.Load(); rt != nil && rt.conf.declared() {
			// Worked out here, as the ledger changes, not as an event is
			// told: route reads the owners, and works out none.
			if _, err := s.owners(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("working out the owners routes read", "err", err)
			}
		}
		if n := s.routing.unrouted.Load(); n > s.routing.unroutedSaid {
			s.log.Warn("events went where no route sent them: as without routes, or everywhere", "events", n-s.routing.unroutedSaid, "since start", n)
			s.routing.unroutedSaid = n
		}
	}
}

// sendsRouted says a notice is sent by routing's sender: to a destination
// a route named, or to the webhook or Slack with a template of their own.
func (s *Server) sendsRouted(x ledger.Notice) bool {
	if strings.HasPrefix(x.Dest, routedPrefix) {
		return true
	}
	if slices.Contains(ownWay, x.Event) || x.Dest != "webhook" && x.Dest != "slack" {
		return false
	}
	rt := s.routing.table.Load()
	return rt != nil && rt.conf != nil && rt.conf.Templates[x.Dest] != nil
}

// sendRouted sends a notice to the destination its row names, the row's
// alone: what it tells (a label, a title, a "to" or a "url" in it) never
// says where it goes. To a JSON webhook, as the webhook is sent one, and
// signed with that destination's key; to a Slack-style one, as a line.
// With a template of the destination's own, in its words: as the text of
// the JSON, or as the message, Slack's markup read as the admin wrote it.
func (s *Server) sendRouted(ctx context.Context, x ledger.Notice, n notice, link string) error {
	name := strings.TrimPrefix(x.Dest, routedPrefix)
	d, ok := s.destination(name)
	if !ok {
		return fmt.Errorf("no destination called %q any more", name)
	}
	var t *noticeTemplate
	if rt := s.routing.table.Load(); rt != nil && rt.conf != nil && !slices.Contains(ownWay, x.Event) {
		t = rt.conf.Templates[name]
	}
	var f noticeFields
	if t != nil {
		f = s.fieldsOf(ctx, x, n, link)
	}
	if d.JSON {
		var body map[string]any
		if err := json.Unmarshal(x.Body, &body); err != nil {
			return err
		}
		if link != "" {
			body["url"] = link
		}
		id := fmt.Sprintf("lapilli-%d", x.ID)
		body["id"] = id
		if t != nil {
			body["text"] = t.render(f, jsonEscape)
		}
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return report.WebhookFrom(ctx, d.File, d.Flag, b, id, d.Secret, s.wall())
	}
	if t != nil {
		return report.SlackMarkup(ctx, d.File, d.Flag, t.render(f, slackEscape))
	}
	text := s.line(n)
	if link != "" {
		text += " " + link
	}
	return report.SlackMessageFrom(ctx, d.File, d.Flag, text)
}

// fieldsOf are what a template's fields hold for a notice: its rule's team
// and labels as the owners have them now.
func (s *Server) fieldsOf(ctx context.Context, x ledger.Notice, n notice, link string) noticeFields {
	o, err := s.owners(ctx)
	if err != nil {
		o = s.ownersNow()
	}
	sub := subjectOf(x.Event, x.Body, o)
	by := n.By
	if by == "" || by == "anonymous" {
		by = tr(s.cfg.Lang, "anonymous")
	}
	f := noticeFields{Event: x.Event, Line: s.line(n), Link: link, Rule: sub.rule, Team: strings.Join(sub.teams, ", "), Title: sub.title,
		ID: sub.id, By: by, Labels: routeLabelsText(sub.labels)}
	if !n.At.IsZero() {
		f.At = n.At.In(s.cfg.Loc).Format("2006-01-02 15:04")
	}
	return f
}

// routeLabelsText is labels as a line: name=value, by name, each value of a
// label with several.
func routeLabelsText(ls map[string][]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(ls)) {
		for _, v := range ls[k] {
			out = append(out, k+"="+v)
		}
	}
	return strings.Join(out, ", ")
}

// webhookDestPaths are the files --webhook-dest and --webhook-dest-secret
// give, which no page shows the paths of (filePaths).
func (s *Server) webhookDestPaths() []filePath {
	var out []filePath
	for _, name := range slices.Sorted(maps.Keys(s.cfg.WebhookDests)) {
		out = append(out, filePath{s.cfg.WebhookDests[name], "--webhook-dest"})
	}
	for _, name := range slices.Sorted(maps.Keys(s.cfg.WebhookDestSecrets)) {
		out = append(out, filePath{s.cfg.WebhookDestSecrets[name], "--webhook-dest-secret"})
	}
	return out
}
