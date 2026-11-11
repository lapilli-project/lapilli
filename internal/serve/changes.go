package serve

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template/parse"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/sse"
	"lapilli.dev/internal/verdict"
)

// Changes to the rules, with a person's approval (docs/design/changes.md).
// Lapilli proposes a change to one rule, with the evidence for it; a person
// with the role for it approves it, in the UI; and only then, with
// --changes, is it checked again and carried out: handed to a recipe that
// opens a pull request where the rule is kept (the git path), or applied
// through Grafana's API with the rule as it was kept for undo.

// Limits are how many changes are carried out at most: deletions a day,
// and changes of every kind an hour.
type Limits struct{ Deletions, Changes int }

// DefaultLimits are --changes-limits' default.
var DefaultLimits = Limits{Deletions: 5, Changes: 20}

// ParseLimits reads DELETIONS,CHANGES: 5,20.
func ParseLimits(v string) (Limits, error) {
	a, b, ok := strings.Cut(v, ",")
	d, err1 := strconv.Atoi(strings.TrimSpace(a))
	c, err2 := strconv.Atoi(strings.TrimSpace(b))
	if !ok || err1 != nil || err2 != nil || d < 0 || c < 1 || d > 1000 || c > 1000 {
		return Limits{}, fmt.Errorf("%q: want deletions a day and changes an hour, such as 5,20", v)
	}
	return Limits{d, c}, nil
}

// retireAfter is how long a rule stays off before it may be deleted.
const retireAfter = 4 * weekLen

// changesState is what changes keep while serve runs.
type changesState struct {
	// key signs the approval forms: a form is good for one proposal, as it
	// stood, for one person.
	key []byte
	// paused is the admin's switch, as last kept.
	paused atomic.Bool
	// The changes token's: see token.
	warned atomic.Bool
	rate   *limiter
	busy   chan struct{}
	// mu is held while proposals are carried out: one at a time.
	mu sync.Mutex
}

func newChanges(ctx context.Context, l *ledger.Ledger) *changesState {
	c := &changesState{key: make([]byte, 32), rate: &limiter{rate: 1, burst: 30}, busy: make(chan struct{}, 1)}
	rand.Read(c.key)
	if v, err := l.Meta(ctx, pausedKey); err == nil && v == "1" {
		c.paused.Store(true)
	}
	return c
}

// pausedKey is where the admin's switch is kept.
const pausedKey = "changes_paused"

// changesPaused reports whether changes are stopped: by an admin, or by
// --changes-paused's file being there.
func (s *Server) changesPaused() bool {
	if s.changes.paused.Load() {
		return true
	}
	if s.cfg.ChangesPaused != "" {
		if _, err := os.Stat(s.cfg.ChangesPaused); err == nil {
			return true
		}
	}
	return false
}

// limits are --changes-limits, or their default.
func (s *Server) limits() Limits {
	if s.cfg.ChangesLimits == (Limits{}) {
		return DefaultLimits
	}
	return s.cfg.ChangesLimits
}

// approvals is how many people approve a proposal: --approvals.
func (s *Server) approvals() int { return max(1, min(2, s.cfg.Approvals)) }

// approver reports whether the reader may approve a change: one named in
// --approvers, or, when it isn't given, an admin. Without a list of either,
// no one may: approving is a person's, and someone must say who.
func (s *Server) approver(r *http.Request) bool {
	switch {
	case s.cfg.ReadOnly, !s.cfg.Changes, s.user(r) == "anonymous":
		return false
	case len(s.cfg.Approvers) > 0:
		return s.listed(r, s.cfg.Approvers)
	case len(s.cfg.Admins) > 0:
		return s.role(r) == admin
	}
	return false
}

// ruleHash is what a rule hashes to: all of it that decides when it fires.
func ruleHash(r rules.Rule) string { return rules.Hash([]byte(r.Key())) }

// saysHash is what a rule hashes to with what it says: a change to what it
// says is stale when either changed.
func saysHash(r rules.Rule) string {
	b, _ := json.Marshal(r.Says)
	return rules.Hash([]byte(r.Key() + "\x00" + string(b)))
}

// saysChange reports whether a proposal changes what a rule says: one of
// kind says, or the undo of one.
func saysChange(p ledger.Proposal) bool {
	if p.Kind == ledger.KindSays {
		return true
	}
	ev := proposalEvidence(p)
	return p.Kind == ledger.KindUndo && ev.Edit != nil && ev.Edit.Annotation != ""
}

// hashOf is what a proposal's rule hashes to, as the proposal hashed it.
func hashOf(p ledger.Proposal, r rules.Rule) string {
	if saysChange(p) {
		return saysHash(r)
	}
	return ruleHash(r)
}

// saysKeys are the annotations a proposal may set or take out: what the
// content check reads. Grafana's links to a dashboard and a panel aren't
// among them.
var saysKeys = []string{"summary", "description", "runbook_url", "runbook"}

// maxSaid is how long a summary or a description may be, in bytes.
const maxSaid = 4 << 10

// saidOK checks a value a person or an agent gave for an annotation: text
// of maxSaid bytes at most with no control characters (a description may
// have line breaks and tabs), or, for a runbook, an https URL; and, as every
// annotation is a template where the rule is kept, one that parses in the
// rule's template language (lang), calling only the functions it has, so
// Prometheus doesn't refuse the whole file for it, and calls no query, which
// would run PromQL each time the alert fires.
func saidOK(key, v, lang string) error {
	if strings.TrimSpace(v) == "" {
		return bad("give what it is to say, or take it out")
	}
	for _, c := range v {
		if (unicode.IsControl(c) && !(key == "description" && (c == '\n' || c == '\t'))) || unicode.Is(unicode.Cf, c) || c == utf8.RuneError {
			return bad("what it says has a control character in it")
		}
	}
	switch key {
	case "runbook_url", "runbook":
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(v) > 2048 || strings.ContainsAny(v, " \t\n") {
			return bad("a runbook is an https URL of 2,048 characters at most")
		}
	default:
		if len(v) > maxSaid {
			return bad("a summary or a description is 4 KB at most")
		}
	}
	return templateOK(v, lang)
}

// templateOK checks a text Lapilli writes as an annotation: a template
// that parses in its language, calling only functions it has, and no query.
func templateOK(v, lang string) error {
	if err := content.TemplateStrict(lang, v); err != nil {
		return bad("what it says isn't a template that parses, and the rule's file would be refused for it: %s", clip(err.Error(), 200))
	}
	if (lang == rules.TemplatePrometheus || lang == rules.TemplateGrafana) && callsQuery(v) {
		return bad("what it says calls query, which would run PromQL each time the alert fires: Lapilli doesn't write one")
	}
	return nil
}

// queryPreamble declares the variables Prometheus's and Grafana's templates
// begin with, so that a text naming them parses.
const queryPreamble = "{{$labels := 0}}{{$externalLabels := 0}}{{$externalURL := 0}}{{$value := 0}}{{$values := 0}}"

// callsQuery reports whether a template calls query, as a function or in a
// pipeline, in it or a template it defines; a text that can't be parsed is
// taken to.
func callsQuery(text string) bool {
	if !strings.Contains(text, "{{") {
		return false
	}
	t := parse.New("t")
	t.Mode = parse.SkipFuncCheck
	trees := map[string]*parse.Tree{}
	if _, err := t.Parse(queryPreamble+text, "{{", "}}", trees); err != nil {
		return true
	}
	var walk func(n parse.Node) bool
	walk = func(n parse.Node) bool {
		switch n := n.(type) {
		case nil:
			return false
		case *parse.IdentifierNode:
			return n.Ident == "query"
		case *parse.ListNode:
			if n == nil {
				return false
			}
			return slices.ContainsFunc(n.Nodes, walk)
		case *parse.ActionNode:
			return walk(n.Pipe)
		case *parse.PipeNode:
			if n == nil {
				return false
			}
			return slices.ContainsFunc(n.Cmds, func(c *parse.CommandNode) bool { return walk(c) })
		case *parse.CommandNode:
			return slices.ContainsFunc(n.Args, walk)
		case *parse.ChainNode:
			return walk(n.Node)
		case *parse.IfNode:
			return walk(n.Pipe) || walk(n.List) || walk(n.ElseList)
		case *parse.RangeNode:
			return walk(n.Pipe) || walk(n.List) || walk(n.ElseList)
		case *parse.WithNode:
			return walk(n.Pipe) || walk(n.List) || walk(n.ElseList)
		case *parse.TemplateNode:
			return walk(n.Pipe)
		}
		return false
	}
	for _, tr := range trees {
		if tr != nil && walk(tr.Root) {
			return true
		}
	}
	return false
}

// saysAfter is what a rule says with an annotation set or taken out, as
// annotations are read: a runbook_url before a runbook. Where the other
// runbook key is, it can't be told; the rule read back says.
func saysAfter(says rules.Says, key, v string, unset bool) rules.Says {
	if unset {
		v = ""
	}
	switch key {
	case "summary":
		says.Summary = v
	case "description":
		says.Description = v
	case "runbook_url":
		says.Runbook = v
	case "runbook":
		if says.Runbook == "" || unset {
			says.Runbook = v
		}
	}
	return says
}

// evidence is what a proposal carries for the person who approves it.
type evidence struct {
	// From is what it was made from: suggestion, near miss, spare, silent,
	// person or agent; Incident the near miss's incident.
	From     string `json:"from"`
	Incident string `json:"incident,omitempty"`
	// Reason is what whoever proposed it said of it: an agent's words are
	// data, shown as such.
	Reason string `json:"reason,omitempty"`
	// Edit is the change as it is written into the rule's document; Bound
	// and Value a threshold's, For a for's.
	Edit  *rules.Edit   `json:"edit,omitempty"`
	Bound *int          `json:"bound,omitempty"`
	Value *float64      `json:"value,omitempty"`
	For   time.Duration `json:"for_ns,omitempty"`
	// Backtest is the change replayed: a for or a threshold.
	Backtest *backtest `json:"backtest,omitempty"`
	// Finding is the content check's finding a change to what a rule says
	// answers, when it answers one.
	Finding *contentFinding `json:"finding,omitempty"`
	// Record is what the rule did, for retiring it.
	Record *ruleRecord `json:"record,omitempty"`
}

// backtest is a change replayed, as it is and as proposed.
type backtest struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Before     side      `json:"before"`
	After      side      `json:"after"`
	Lost       []string  `json:"lost"`   // caught as it is, missed as proposed
	Gained     []string  `json:"gained"` // missed as it is, caught as proposed
	Later      []string  `json:"later"`  // caught later as proposed
	Unfinished bool      `json:"unfinished,omitempty"`
}

type side struct {
	Firings int `json:"firings"`
	White   int `json:"white"`
}

// ruleRecord is what a rule did over the weeks shown, for retiring it.
type ruleRecord struct {
	Weeks     int  `json:"weeks"`
	Firings   int  `json:"firings"`
	White     int  `json:"white"`
	Incidents int  `json:"incidents"`
	First     int  `json:"first"`
	Alone     int  `json:"alone"`
	Spare     bool `json:"spare"`
	// Would are the incidents a rule turned off would have caught since,
	// that nothing caught: a deletion after a rule was off.
	Would []string `json:"would,omitempty"`
}

// ask is what a proposal is asked for.
type ask struct {
	Rule string
	Kind string
	For  time.Duration // a for's
	// Bound is which of the condition's numbers, as rules.Bounds counts
	// them, and Value its new number: a threshold's.
	Bound int
	Value float64
	// From, Incident and Reason are as evidence has them.
	From, Incident, Reason string
	// Follows is a deletion's disable.
	Follows int64
	// Annotation is the one a change to what the rule says sets to Text,
	// or, with Unset, takes out; Check the content check's finding it
	// answers.
	Annotation, Text, Check string
	Unset                   bool
}

// found is a rule as it is in effect now.
type found struct {
	rule rules.Rule // under its own name, as written
	name string     // the name the ledger judges it under
	doc  rules.Doc
}

// current is the rule in effect a ledger name names, and the document it
// was read from. fresh reads the rules again, for what the latest version
// doesn't have: a change to what a rule says makes no version.
func (s *Server) current(ctx context.Context, name string, fresh bool) (found, error) {
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return found{}, err
	}
	if !ok {
		return found{}, bad("the rules haven't been read yet: try again in a minute")
	}
	full, err := s.l.VersionByID(ctx, latest.ID)
	if err != nil {
		return found{}, err
	}
	all, names, err := s.named(ctx, latest.ID)
	if err != nil {
		return found{}, err
	}
	i := slices.Index(names, name)
	if i < 0 || name == "" {
		return found{}, bad("no rule in effect is called %q", name)
	}
	f := found{rule: all[i], name: name}
	docs := full.Docs
	if fresh {
		rs, now, err := s.readRules(ctx)
		if err != nil {
			return found{}, err
		}
		j := slices.IndexFunc(rs, func(r rules.Rule) bool { return identity(r) == identity(f.rule) && r.Source == f.rule.Source })
		if j < 0 {
			return found{}, bad("the rule changed since it was read: try again in a minute")
		}
		f.rule, docs = rs[j], now
	}
	for _, d := range docs {
		if d.Source == f.rule.Source {
			f.doc = d
			break
		}
	}
	return f, nil
}

// grafanaKept reports whether a rule was read from Grafana's API: kept
// nowhere else, so changed through it.
func (s *Server) grafanaKept(f found) bool {
	return s.cfg.Grafana != "" && f.doc.Source == s.cfg.Grafana
}

// propose makes a proposal: the change written as where the rule is kept
// expects it, read back, and backtested.
func (s *Server) propose(ctx context.Context, q ask, by string, agent bool, wait bool) (int64, error) {
	if !s.replaying() {
		return 0, bad("this lapilli serve reads no rules of its own: there is nothing to change")
	}
	if err := s.incidentNamed(ctx, q.Incident); err != nil {
		return 0, err
	}
	f, err := s.current(ctx, q.Rule, true)
	if err != nil {
		return 0, err
	}
	r := f.rule
	e := rules.Edit{Name: r.Name, Group: r.Group, UID: r.UID}
	want := r
	ev := evidence{From: q.From, Incident: q.Incident, Reason: q.Reason}
	var b rules.Bound
	switch q.Kind {
	case ledger.KindFor:
		switch {
		case q.For <= 0 || q.For > compareFor:
			return 0, bad("a for of more than nothing and %s at most", verdict.Duration(compareFor))
		case q.For == r.For:
			return 0, bad("that is its for already")
		case r.Unsupported != "":
			return 0, bad("it isn't replayed (%s): no backtest could check a change to it", r.Unsupported)
		}
		want.For, e.For, ev.For = q.For, &q.For, q.For
	case ledger.KindThreshold:
		if math.IsNaN(q.Value) || math.IsInf(q.Value, 0) {
			return 0, bad("threshold %q: want a number", strconv.FormatFloat(q.Value, 'g', -1, 64))
		}
		i := slices.IndexFunc(r.Bounds(), func(x rules.Bound) bool { return x.At == q.Bound })
		if i < 0 || r.Unsupported != "" {
			return 0, bad("its condition has no such number to change")
		}
		b = r.Bounds()[i]
		if want = r.WithBound(b, q.Value); want.Key() == r.Key() {
			return 0, bad("that is its threshold already")
		}
		ev.Bound, ev.Value = &q.Bound, &q.Value
	case ledger.KindDisable:
		switch {
		case r.UID == "":
			return 0, bad("a rule file has no off: retire it by deleting it")
		case r.Unsupported == rules.Paused:
			return 0, bad("it is off already")
		}
		want.Unsupported, want.Graph, e.Pause = rules.Paused, nil, true
	case ledger.KindDelete:
		e.Delete = true
		if r.UID != "" {
			// A Grafana rule is turned off first, and deleted four weeks
			// later: the deletion follows the disable that turned it off,
			// and the rule is off still.
			if q.Follows, err = s.turnedOff(ctx, f.name, q.Follows); err != nil {
				var b badRequest
				if errors.As(err, &b) && r.Unsupported == rules.Paused {
					return 0, bad("it was turned off by hand, not by a proposal approved here: delete it in Grafana")
				}
				return 0, err
			}
			if r.Unsupported != rules.Paused {
				return 0, bad("it was turned on again since it was turned off: it isn't deleted")
			}
		}
	case ledger.KindSays:
		switch {
		case q.Annotation == "__dashboardUid__" || q.Annotation == "__panelId__":
			return 0, bad("a Grafana rule's dashboard and panel are changed in Grafana")
		case !slices.Contains(saysKeys, q.Annotation):
			return 0, bad("what a rule says is changed in its summary, description, runbook_url or runbook")
		case !q.Unset:
			if err := saidOK(q.Annotation, q.Text, r.Says.Template); err != nil {
				return 0, err
			}
		}
		was := saysAfter(r.Says, q.Annotation, "", true) // as it would say with it taken out
		switch now := saysAfter(r.Says, q.Annotation, q.Text, q.Unset); {
		case q.Unset && now == r.Says && was == r.Says:
			return 0, bad("it says nothing there to take out")
		case !q.Unset && now == r.Says:
			return 0, bad("it says that already")
		}
		want.Says = saysAfter(r.Says, q.Annotation, q.Text, q.Unset)
		e.Annotation, e.Value, e.Unset = q.Annotation, q.Text, q.Unset
		if q.Unset {
			e.Value = ""
		}
		if q.Check != "" {
			fs, err := s.contentFindings(ctx)
			if err != nil {
				return 0, err
			}
			i := slices.IndexFunc(fs[f.name], func(x contentFinding) bool { return x.Kind == q.Check })
			if i < 0 {
				return 0, bad("the content check finds no %q for this rule", q.Check)
			}
			ev.Finding = &fs[f.name][i]
		}
	default:
		return 0, bad("a proposal's kind is for, threshold, disable, delete or says")
	}
	ev.Edit = &e
	says := q.Kind == ledger.KindSays
	hash := ruleHash
	if says {
		hash = saysHash
	}
	p := ledger.Proposal{Rule: f.name, Kind: q.Kind, Path: ledger.PathNone, MadeBy: by, Agent: agent, Source: f.doc.Source,
		Before: ruleText(r), BeforeHash: hash(r), Follows: q.Follows}
	if q.Kind != ledger.KindDelete {
		p.After, p.AfterHash = ruleText(want), hash(want)
	}
	switch {
	case r.Cloud != "":
		// A cloud's own alarm: step 3 of the design.
	case s.grafanaKept(f):
		if q.Kind != ledger.KindThreshold {
			p.Path = ledger.PathGrafana
			p.AfterHash = "" // Grafana says, once it is applied
		}
	case !f.doc.API && len(f.doc.Data) > 0:
		var out []byte
		var err error
		if q.Kind == ledger.KindThreshold {
			out, err = rules.EditThreshold(f.doc.Data, r, e, b, q.Value)
		} else {
			out, err = rules.EditDoc(f.doc.Data, e)
		}
		if err == nil {
			after, ok := readsAs(f.doc.Data, out, r, want, q.Kind == ledger.KindDelete)
			if says {
				after, ok = readsSaying(f.doc.Data, out, r, want)
			}
			if ok {
				p.Path, p.Document, p.DocHash = ledger.PathGit, string(out), rules.Hash(f.doc.Data)
				p.Diff = rules.Diff(s.docName(f.doc.Source), f.doc.Data, out)
				if after != nil {
					p.AfterHash = hash(*after)
				}
			}
		}
	}
	if p.Diff == "" {
		p.Diff = rules.Diff(f.name, []byte(p.Before), []byte(p.After))
	}
	switch q.Kind {
	case ledger.KindSays:
		// It doesn't change when the rule fires: nothing to backtest.
	case ledger.KindFor, ledger.KindThreshold:
		if ev.Backtest, err = s.backtest(ctx, f.name, r, want, wait); err != nil {
			return 0, err
		}
	default:
		if ev.Record, err = s.recordOf(ctx, f.name); err != nil {
			return 0, err
		}
	}
	if p.Evidence, err = json.Marshal(ev); err != nil {
		return 0, err
	}
	id, err := s.l.Propose(ctx, p)
	switch {
	case errors.Is(err, ledger.ErrProposed):
		return 0, bad("this rule has a proposal not yet finished with: one at a time, so that each is seen applied")
	case errors.Is(err, ledger.ErrTooManyProposals):
		return 0, bad("you have made %d proposals in the last hour, with your agents, the most anyone may: try again later", ledger.ProposalsAnHour)
	}
	return id, err
}

// maxIncidentID is how long an incident's ID a proposal names may be.
const maxIncidentID = 128

// incidentNamed checks the incident a proposal is made for: one the
// ledger has, named by its ID, plain and short, since it goes into a pull
// request's description and a commit.
func (s *Server) incidentNamed(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if len(id) > maxIncidentID || !ledger.Plain(id) || strings.ContainsAny(id, "`\n") {
		return bad("a proposal names an incident by its ID, as the ledger has it")
	}
	if _, err := s.l.IncidentByID(ctx, id); errors.Is(err, ledger.ErrNotFound) {
		return bad("the ledger has no incident %q", id)
	} else if err != nil {
		return err
	}
	return nil
}

// turnedOff is the applied disable a deletion of a Grafana rule follows:
// the one given, or else the latest for the rule.
func (s *Server) turnedOff(ctx context.Context, rule string, follows int64) (int64, error) {
	if follows == 0 {
		ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: rule, States: []string{ledger.StateApplied}, Limit: 100})
		if err != nil {
			return 0, err
		}
		for _, p := range ps {
			if p.Kind == ledger.KindDisable {
				return p.ID, nil
			}
		}
		return 0, bad("a Grafana rule is turned off first, and deleted no sooner than four weeks later: propose turning it off")
	}
	off, err := s.l.ProposalByID(ctx, follows)
	if errors.Is(err, ledger.ErrNotFound) || err == nil && (off.Rule != rule || off.Kind != ledger.KindDisable || off.State != ledger.StateApplied) {
		return 0, bad("a Grafana rule is turned off first, and deleted no sooner than four weeks later: propose turning it off")
	}
	return follows, err
}

// docName is a document's name in a diff: its path under --rules, or the
// file's own name.
func (s *Server) docName(source string) string {
	if rel, err := filepath.Rel(s.cfg.RulesPath, source); s.cfg.RulesPath != "" && err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(source)
}

// readsAs reports whether a document edited reads as the rules before
// with want in place of r (none, for a deletion), and gives the rule as
// read.
func readsAs(before, after []byte, r, want rules.Rule, gone bool) (*rules.Rule, bool) {
	a, err := rules.Parse(before)
	if err != nil {
		return nil, false
	}
	b, err := rules.Parse(after)
	if err != nil {
		return nil, false
	}
	left := map[string]int{}
	for _, x := range a {
		left[x.Key()]++
	}
	left[r.Key()]--
	var got *rules.Rule
	for i, x := range b {
		if left[x.Key()] > 0 {
			left[x.Key()]--
			continue
		}
		if got != nil {
			return nil, false
		}
		got = &b[i]
	}
	for _, n := range left {
		if n != 0 {
			return nil, false
		}
	}
	if gone {
		return nil, got == nil
	}
	return got, got != nil && shape(*got) == shape(want)
}

// readsSaying reports whether a document edited reads as the rules before,
// every one firing as it did and saying what it said, but for r, which
// fires as it did and says what want says; and gives r as read. r is
// compared but for its digest: a Grafana rule's covers what it says, and
// rules.Check has compared the rest of its tree whole. What it says now is
// read back as a template, as saidOK reads it.
func readsSaying(before, after []byte, r, want rules.Rule) (*rules.Rule, bool) {
	a, err := rules.Parse(before)
	if err != nil {
		return nil, false
	}
	b, err := rules.Parse(after)
	if err != nil || len(a) != len(b) {
		return nil, false
	}
	var got *rules.Rule
	for i := range a {
		if a[i].Key() == r.Key() && a[i].Says == r.Says && got == nil {
			if keyBut(a[i]) != keyBut(b[i]) {
				return nil, false
			}
			got = &b[i]
			continue
		}
		if a[i].Key() != b[i].Key() || a[i].Says != b[i].Says {
			return nil, false
		}
	}
	if got == nil || got.Says.Summary != want.Says.Summary || got.Says.Description != want.Says.Description {
		return nil, false
	}
	for _, x := range [][2]string{{got.Says.Summary, r.Says.Summary}, {got.Says.Description, r.Says.Description}, {got.Says.Runbook, r.Says.Runbook}} {
		if x[0] != x[1] && templateOK(x[0], got.Says.Template) != nil {
			return nil, false
		}
	}
	return got, true
}

// keyBut is a rule's key but for its digest.
func keyBut(r rules.Rule) string {
	r.Digest = ""
	return r.Key()
}

// shape is a rule but for its digest, which a Grafana rule's whole text
// decides: what a change to it is checked by.
func shape(r rules.Rule) string {
	c := describe(r, time.Time{})
	return fmt.Sprintf("%q %q %q %d %d %d %q %q %q %q", r.Name, r.Group, r.Expr, r.For, r.KeepFiringFor, r.Interval,
		sse.LabelKey(r.Labels), r.UID, r.Unsupported, c.Expr)
}

// ruleText is a rule as a proposal shows it.
func ruleText(r rules.Rule) string {
	c := describe(r, time.Time{})
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", r.Name, r.Group)
	if r.Unsupported == rules.Paused {
		b.WriteString("paused\n")
	}
	if c.Expr != "" {
		fmt.Fprintf(&b, "expr: %s\n", strings.ReplaceAll(c.Expr, "\n", "\n  "))
	}
	fmt.Fprintf(&b, "for: %s\n", c.For)
	if c.Keep != "" {
		fmt.Fprintf(&b, "keep_firing_for: %s\n", c.Keep)
	}
	if c.Labels != "" {
		fmt.Fprintf(&b, "labels: %s\n", c.Labels)
	}
	for _, x := range []struct{ k, v string }{{"summary", r.Says.Summary}, {"description", r.Says.Description}, {"runbook", r.Says.Runbook}} {
		if x.v != "" {
			fmt.Fprintf(&b, "%s: %s\n", x.k, strings.ReplaceAll(x.v, "\n", "\n  "))
		}
	}
	return b.String()
}

// backtest replays a change over the twelve weeks to now, or since the
// runs began when that is later. wait says to wait for a comparison
// running, as a job does; a person asking is told to try again.
func (s *Server) backtest(ctx context.Context, name string, before, after rules.Rule, wait bool) (*backtest, error) {
	to := s.now().Add(-s.cfg.Settle).Truncate(s.cfg.Step).UTC()
	from := to.Add(-weeks * weekLen)
	if began, err := s.l.RunsFrom(ctx); err != nil {
		return nil, err
	} else if began.After(from) {
		from = began
	}
	if !from.Before(to) {
		return nil, bad("nothing has been replayed yet to backtest it against: try again after the first run")
	}
	if wait {
		s.comparing.Lock()
	} else if !s.comparing.TryLock() {
		return nil, bad("another comparison is running: try again in a minute")
	}
	defer s.comparing.Unlock()
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	b, a := before, after
	b.Name, a.Name = name, name
	cmp, err := s.replayChange(ctx, []rules.Rule{b}, []rules.Rule{a}, 0, from, to, s.cfg.Timeout, o)
	if err != nil {
		return nil, err
	}
	bt := &backtest{From: from, To: to, Lost: []string{}, Gained: []string{}, Later: []string{}}
	for _, rc := range cmp.Rules {
		if rc.Rule != name {
			continue
		}
		for _, x := range []struct {
			r  *verdict.RuleResult
			to *side
		}{{rc.Before, &bt.Before}, {rc.After, &bt.After}} {
			if x.r != nil {
				*x.to = side{x.r.Firings, x.r.White}
				bt.Unfinished = bt.Unfinished || x.r.Unsupported == replay.Unfinished
			}
		}
	}
	for _, ic := range cmp.Incidents {
		switch {
		case len(ic.Before) > 0 && len(ic.After) == 0:
			bt.Lost = append(bt.Lost, ic.ID)
		case len(ic.Before) == 0 && len(ic.After) > 0:
			bt.Gained = append(bt.Gained, ic.ID)
		case len(ic.Before) > 0 && ic.After[0].Delay > ic.Before[0].Delay:
			bt.Later = append(bt.Later, ic.ID)
		}
	}
	return bt, nil
}

// recordOf is what a rule did over the twelve weeks shown.
func (s *Server) recordOf(ctx context.Context, name string) (*ruleRecord, error) {
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	lines, _, err := s.ruleLines(ctx, "en", t, weeks)
	if err != nil {
		return nil, err
	}
	rec := &ruleRecord{Weeks: weeks}
	if i := slices.IndexFunc(lines, func(l ruleLine) bool { return l.Name == name }); i >= 0 {
		l := lines[i]
		rec.Firings, rec.White, rec.Incidents, rec.First, rec.Alone, rec.Spare = l.Firings, l.White, l.Incidents, l.First, l.Alone, l.Spare
	}
	return rec, nil
}

// worse says how what a proposal does now is worse than what was approved:
// an incident it would miss that it didn't, or one more caught by the rule
// being retired; "" when it isn't.
func worse(was, now evidence) string {
	if was.Backtest != nil && now.Backtest != nil {
		if now.Backtest.Unfinished {
			return "the backtest didn't finish within --timeout, so the change couldn't be checked again"
		}
		for _, id := range now.Backtest.Lost {
			if !slices.Contains(was.Backtest.Lost, id) {
				return fmt.Sprintf("the backtest now says it would miss %s, which the rule catches as it is", id)
			}
		}
	}
	if was.Record != nil && now.Record != nil {
		switch {
		case now.Record.Alone > was.Record.Alone:
			return fmt.Sprintf("it has caught %d incidents no other rule caught since it was proposed", now.Record.Alone-was.Record.Alone)
		case len(now.Record.Would) > 0:
			return fmt.Sprintf("while it was off it would have caught %s, which nothing caught", strings.Join(now.Record.Would, ", "))
		}
	}
	return ""
}

// proposalEvidence is a proposal's evidence as kept.
func proposalEvidence(p ledger.Proposal) evidence {
	var ev evidence
	json.Unmarshal(p.Evidence, &ev)
	return ev
}

// formKey is what an approval form carries: good for one proposal as it
// stood when the page was shown, for one person, for one thing to do.
func (s *Server) formKey(p ledger.Proposal, user, what string) string {
	m := hmac.New(sha256.New, s.changes.key)
	fmt.Fprintf(m, "%s\x00%d\x00%s\x00%d\x00%d\x00%s", what, p.ID, p.State, p.Updated.Unix(), len(p.Approvals), user)
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) formKeyOK(p ledger.Proposal, user, what, got string) bool {
	return got != "" && hmac.Equal([]byte(got), []byte(s.formKey(p, user, what)))
}

// overLimit reports whether a proposal of a kind would go past the limits,
// and waits.
func (s *Server) overLimit(ctx context.Context, kind string) (bool, error) {
	lim, now := s.limits(), s.now()
	n, err := s.l.Carried(ctx, now.Add(-time.Hour), "")
	if err != nil || n >= lim.Changes {
		return true, err
	}
	if kind == ledger.KindDelete {
		n, err := s.l.Carried(ctx, now.Add(-24*time.Hour), ledger.KindDelete)
		if err != nil || n >= lim.Deletions {
			return true, err
		}
	}
	return false, nil
}

// carryOut carries out the approved proposals, oldest first, as far as the
// limits let it: each checked again first. Nothing is, without --changes,
// or while changes are paused. Those with the approvals --approvals now
// asks for are approved first, and changes sent to Grafana and not yet read
// back read again, paused or not: they are never sent again.
func (s *Server) carryOut(ctx context.Context) error {
	if !s.cfg.Changes {
		return nil
	}
	s.changes.mu.Lock()
	defer s.changes.mu.Unlock()
	// Approved before --approvals was lowered: moved on.
	if _, err := s.l.Promote(ctx, s.approvals()); err != nil {
		return err
	}
	if err := s.settleApplying(ctx); err != nil {
		return err
	}
	if s.changesPaused() {
		return nil
	}
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: []string{ledger.StateApproved}, Limit: 100})
	if err != nil {
		return err
	}
	slices.Reverse(ps)
	for _, p := range ps {
		// The stop is looked at before each: pausing stops what is left.
		if s.changesPaused() {
			return nil
		}
		wait, err := s.overLimit(ctx, p.Kind)
		if err != nil {
			return err
		}
		if wait {
			continue
		}
		if err := s.carry(ctx, p); err != nil {
			switch {
			case errors.Is(err, errPaused):
				return nil // paused while it was checked again: it waits, approved
			case errors.Is(err, context.Canceled):
				return err
			}
			s.log.Warn("a change failed", "proposal", p.ID, "err", err)
			if _, err := s.l.Move(ctx, p.ID, []string{ledger.StateApproved}, ledger.StateFailed, ledger.Proposer, ledger.Step{Reason: clip(err.Error(), 500)}); err != nil && !errors.Is(err, ledger.ErrMoved) {
				return err
			}
		}
	}
	return nil
}

// errPaused is a change stopped by the pause just before it was sent or
// handed out: it waits, approved, until changes are resumed.
var errPaused = errors.New("changes are paused")

// refusal is a proposal refused when it was checked again, and why.
type refusal struct{ why string }

func (r refusal) Error() string { return r.why }

// carry checks a proposal again and carries it out: handed to a recipe, or
// sent to Grafana. The pause is looked at again just before either, since
// checking again can take as long as a backtest.
func (s *Server) carry(ctx context.Context, p ledger.Proposal) error {
	step, to, err := s.checkAgain(ctx, p)
	if err == nil && p.Path == ledger.PathGrafana {
		err = s.grafanaApply(ctx, p, step)
	}
	var no refusal
	if errors.As(err, &no) {
		_, err := s.l.Move(ctx, p.ID, []string{ledger.StateApproved}, ledger.StateRefused, ledger.Proposer, ledger.Step{Reason: no.why})
		return err
	}
	if err != nil || p.Path == ledger.PathGrafana {
		return err
	}
	if s.changesPaused() {
		return errPaused
	}
	step.Applied = true
	_, err = s.l.Move(ctx, p.ID, []string{ledger.StateApproved}, to, ledger.Proposer, step)
	return err
}

// checkAgain checks an approved proposal against the rule as it is now:
// read again from where it is kept, and the backtest run again. It gives
// what the step to it sets, and the state it goes to: ready for a recipe;
// Grafana's are sent by grafanaApply. A refusal says why it can't go on.
func (s *Server) checkAgain(ctx context.Context, p ledger.Proposal) (ledger.Step, string, error) {
	var step ledger.Step
	ev := proposalEvidence(p)
	if p.Path == ledger.PathGrafana {
		// Grafana's own copy is read again where it is applied.
		return step, ledger.StateApplying, s.checkEvidence(ctx, p, ev, nil)
	}
	if p.Path != ledger.PathGit {
		return step, "", refusal{"this one is for a person to make where the rule is written"}
	}
	rs, docs, err := s.readRules(ctx)
	if err != nil {
		return step, "", err
	}
	var now *rules.Rule
	for i, r := range rs {
		if hashOf(p, r) == p.BeforeHash {
			now = &rs[i]
			break
		}
	}
	if p.BeforeHash != "" && now == nil {
		return step, "", refusal{"the rule changed since it was proposed, or is gone: propose it again"}
	}
	i := slices.IndexFunc(docs, func(d rules.Doc) bool { return d.Source == p.Source })
	if i < 0 {
		return step, "", refusal{"the file the rule was read from is gone"}
	}
	doc := docs[i]
	step.Backup = string(doc.Data)
	if rules.Hash(doc.Data) != p.DocHash {
		// The rest of the file changed: the change is made afresh in it,
		// and read back again. An undo writes back the file as it was, so
		// a file changed since is reverted in the repository instead.
		if p.Kind == ledger.KindUndo || ev.Edit == nil || now == nil {
			return step, "", refusal{"the file changed since: revert the change in the repository"}
		}
		e := *ev.Edit
		var out []byte
		want := *now
		if p.Kind == ledger.KindThreshold {
			b, v := rules.Bound{}, 0.0
			if ev.Bound != nil && ev.Value != nil {
				if j := slices.IndexFunc(now.Bounds(), func(x rules.Bound) bool { return x.At == *ev.Bound }); j >= 0 {
					b, v = now.Bounds()[j], *ev.Value
				}
			}
			want = now.WithBound(b, v)
			out, err = rules.EditThreshold(doc.Data, *now, e, b, v)
		} else {
			if e.For != nil {
				want.For = *e.For
			}
			out, err = rules.EditDoc(doc.Data, e)
		}
		if err != nil {
			return step, "", refusal{"the change can't be written into the file as it is now: propose it again"}
		}
		ok := false
		if e.Annotation != "" {
			want.Says = saysAfter(now.Says, e.Annotation, e.Value, e.Unset)
			_, ok = readsSaying(doc.Data, out, *now, want)
		} else {
			_, ok = readsAs(doc.Data, out, *now, want, e.Delete)
		}
		if !ok {
			return step, "", refusal{"the change can't be written into the file as it is now: propose it again"}
		}
		step.Document, step.DocHash, step.Diff = string(out), rules.Hash(doc.Data), rules.Diff(s.docName(p.Source), doc.Data, out)
	}
	if err := s.checkEvidence(ctx, p, ev, now); err != nil {
		return step, "", err
	}
	return step, ledger.StateReady, nil
}

// checkEvidence runs a proposal's backtest, or looks at its rule's record,
// again, and refuses it if what it says is worse than what was approved.
// now is the rule as read again, when it was.
func (s *Server) checkEvidence(ctx context.Context, p ledger.Proposal, ev evidence, now *rules.Rule) error {
	if p.Kind == ledger.KindUndo {
		return nil
	}
	again := evidence{}
	switch {
	case ev.Backtest != nil:
		f, err := s.current(ctx, p.Rule, false)
		if err != nil {
			return refusal{"the rule is no longer in effect"}
		}
		r := f.rule
		if now != nil {
			r = *now
		}
		want := r
		switch p.Kind {
		case ledger.KindFor:
			want.For = ev.For
		case ledger.KindThreshold:
			if ev.Bound == nil || ev.Value == nil {
				return refusal{"the proposal doesn't say which number to change"}
			}
			j := slices.IndexFunc(r.Bounds(), func(x rules.Bound) bool { return x.At == *ev.Bound })
			if j < 0 {
				return refusal{"the rule changed since it was proposed, or is gone: propose it again"}
			}
			want = r.WithBound(r.Bounds()[j], *ev.Value)
		}
		bt, err := s.backtest(ctx, p.Rule, r, want, true)
		if err != nil {
			return err
		}
		again.Backtest = bt
	case ev.Record != nil:
		rec, err := s.recordOf(ctx, p.Rule)
		if err != nil {
			return err
		}
		switch {
		case p.Kind == ledger.KindDelete && p.Follows != 0:
			if err := s.offLongEnough(ctx, p, rec); err != nil {
				return err
			}
		case p.Kind == ledger.KindDelete && ev.Edit != nil && ev.Edit.UID != "":
			return refusal{"a Grafana rule is turned off first, and deleted no sooner than four weeks later"}
		}
		again.Record = rec
	}
	if why := worse(ev, again); why != "" {
		return refusal{why}
	}
	return nil
}

// offLongEnough checks a deletion after a rule was turned off: off four
// weeks at least, since it took effect (its proposal's last move, to
// applied), and nothing missed meanwhile that it would have caught.
func (s *Server) offLongEnough(ctx context.Context, p ledger.Proposal, rec *ruleRecord) error {
	off, err := s.l.ProposalByID(ctx, p.Follows)
	if err != nil {
		return err
	}
	if off.State != ledger.StateApplied || off.Kind != ledger.KindDisable || off.Rule != p.Rule {
		return refusal{"the rule wasn't turned off by the proposal this one follows"}
	}
	// Turned on again by hand since: it isn't retired.
	if f, err := s.current(ctx, p.Rule, false); err != nil || f.rule.Unsupported != rules.Paused {
		return refusal{"the rule was turned on again since it was turned off"}
	}
	if s.now().Sub(off.Updated) < retireAfter {
		return refusal{fmt.Sprintf("it has been off since %s: four weeks aren't up", off.Updated.In(s.cfg.Loc).Format("2006-01-02"))}
	}
	// The rule as it was before it was turned off, replayed since.
	vs, err := s.versions(ctx, off.Made, off.Made)
	if err != nil {
		return err
	}
	var was *rules.Rule
	for _, v := range vs {
		if i := slices.IndexFunc(v.Rules, func(r rules.Rule) bool { return r.Name == p.Rule && r.Unsupported == "" }); i >= 0 {
			was = &v.Rules[i]
		}
	}
	if was == nil {
		return nil // nothing to replay: it wasn't replayed before either
	}
	from, to := off.Updated, s.now().Add(-s.cfg.Settle).Truncate(s.cfg.Step)
	o, err := s.options(ctx)
	if err != nil {
		return err
	}
	s.comparing.Lock()
	cmp, err := s.replayChange(ctx, nil, []rules.Rule{*was}, 0, from, to, s.cfg.Timeout, o)
	s.comparing.Unlock()
	if err != nil {
		return err
	}
	incs, err := s.l.Incidents(ctx, from, to, false)
	if err != nil {
		return err
	}
	missed := map[string]bool{}
	for _, in := range incs {
		if in.Counts && len(in.CaughtBy) == 0 {
			missed[in.ID] = true
		}
	}
	for _, ic := range cmp.Incidents {
		if len(ic.After) > 0 && missed[ic.ID] {
			rec.Would = append(rec.Would, ic.ID)
		}
	}
	return nil
}

// seen marks the proposals a version of the rules carries out applied: a
// pull request merged and read back, or a change a person made by hand.
// Taking effect isn't being carried out: the limits counted a pull request
// when it was handed out, and count a person's change never.
func (s *Server) seen(ctx context.Context, version int64) error {
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: ledger.Unfinished(), Limit: 500})
	if err != nil || len(ps) == 0 {
		return err
	}
	all, _, err := s.named(ctx, version)
	if err != nil {
		return err
	}
	has := map[string]bool{}
	for _, r := range all {
		has[ruleHash(r)] = true
	}
	for _, p := range ps {
		if p.Path == ledger.PathGrafana || saysChange(p) {
			continue // applied when Grafana takes it; what a rule says, by seenSays
		}
		if err := s.amendedInReview(ctx, p, all); err != nil {
			if errors.Is(err, errAmended) {
				continue
			}
			return err
		}
		var done bool
		switch {
		case p.AfterHash != "":
			done = has[p.AfterHash]
		case p.Kind == ledger.KindDelete || p.Kind == ledger.KindUndo:
			// Gone is applied only once it was merged, or for a person to
			// make: a rule can go for other reasons.
			done = !has[p.BeforeHash] && (p.State == ledger.StateMerged || p.Path == ledger.PathNone)
		}
		if !done {
			continue
		}
		if _, err := s.l.Move(ctx, p.ID, []string{p.State}, ledger.StateApplied, ledger.Proposer, ledger.Step{}); err != nil && !errors.Is(err, ledger.ErrMoved) {
			return err
		}
		if p.Kind == ledger.KindUndo {
			if _, err := s.l.Move(ctx, p.Follows, []string{ledger.StateApplied}, ledger.StateUndone, ledger.Proposer, ledger.Step{}); err != nil && !errors.Is(err, ledger.ErrMoved) {
				return err
			}
		}
	}
	return nil
}

// errAmended is a merged proposal seen changed in review.
var errAmended = errors.New("changed in review")

// amendedInReview settles a merged pull request whose rule reads now as
// neither it was nor as proposed: changed in review, 15m made 20m, it would
// otherwise wait for ever to be seen applied, and hold up its rule. The
// rule is the one the proposal was made for, by its document, group and
// name; one gone, or moved, isn't told apart from one not yet read, and
// waits, for an admin to close. errAmended says it was settled.
func (s *Server) amendedInReview(ctx context.Context, p ledger.Proposal, rs []rules.Rule) error {
	if p.State != ledger.StateMerged || p.Path != ledger.PathGit {
		return nil
	}
	ev := proposalEvidence(p)
	if ev.Edit == nil {
		return nil
	}
	i := slices.IndexFunc(rs, func(r rules.Rule) bool {
		return r.Source == p.Source && r.Group == ev.Edit.Group && r.Name == ev.Edit.Name && r.UID == ev.Edit.UID
	})
	if i < 0 {
		return nil
	}
	if h := hashOf(p, rs[i]); h == p.BeforeHash || h == p.AfterHash {
		return nil
	}
	_, err := s.l.Move(ctx, p.ID, []string{ledger.StateMerged}, ledger.StateAmended, ledger.Proposer,
		ledger.Step{Reason: "merged, but the rule reads as neither it was nor as proposed: the pull request was changed in review"})
	if err != nil && !errors.Is(err, ledger.ErrMoved) {
		return err
	}
	return errAmended
}

// seenLatest is seen for the latest version of the rules read.
func (s *Server) seenLatest(ctx context.Context) error {
	latest, ok, err := s.l.Latest(ctx)
	if err != nil || !ok {
		return err
	}
	return s.seen(ctx, latest.ID)
}

// seenSays marks the changes to what rules say that the rules read now
// carry out applied: they make no new version, so seen doesn't see them.
func (s *Server) seenSays(ctx context.Context, rs []rules.Rule) error {
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: ledger.Unfinished(), Limit: 500})
	if err != nil || len(ps) == 0 {
		return err
	}
	has := map[string]bool{}
	for _, r := range rs {
		has[saysHash(r)] = true
	}
	for _, p := range ps {
		if p.Path == ledger.PathGrafana || !saysChange(p) {
			continue
		}
		if err := s.amendedInReview(ctx, p, rs); err != nil {
			if errors.Is(err, errAmended) {
				continue
			}
			return err
		}
		if !has[p.AfterHash] {
			continue
		}
		if _, err := s.l.Move(ctx, p.ID, []string{p.State}, ledger.StateApplied, ledger.Proposer, ledger.Step{}); err != nil && !errors.Is(err, ledger.ErrMoved) {
			return err
		}
		if p.Kind == ledger.KindUndo {
			if _, err := s.l.Move(ctx, p.Follows, []string{ledger.StateApplied}, ledger.StateUndone, ledger.Proposer, ledger.Step{}); err != nil && !errors.Is(err, ledger.ErrMoved) {
				return err
			}
		}
	}
	return nil
}

// proposeWeekly makes the week's proposals once, after the digest.
func (s *Server) proposeWeekly(ctx context.Context) error {
	if !s.cfg.Changes {
		return nil
	}
	key := "none"
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil {
		return err
	}
	if len(rs) > 0 {
		key = strconv.FormatInt(rs[0].From.Unix(), 10)
	}
	if have, err := s.l.Meta(ctx, foundKey); err != nil || have == key {
		return err
	}
	if err := s.proposeFound(ctx); err != nil {
		return err
	}
	return s.l.SetMeta(ctx, foundKey, key)
}

// foundKey is where the week proposals were last made for is kept.
const foundKey = "changes_found"

// proposeMost is how many proposals Lapilli makes of itself in a week, at
// most: each is backtested over twelve weeks.
const proposeMost = 10

// proposeFound makes proposals from what Lapilli found: longer fors for
// noisy rules, thresholds and fors for near misses, retiring spare rules
// and those that never fired, and deleting a rule four weeks off. Only with
// --changes: without it, people propose from a rule's page.
func (s *Server) proposeFound(ctx context.Context) error {
	if !s.cfg.Changes || !s.replaying() {
		return nil
	}
	have, err := s.l.Proposals(ctx, ledger.ProposalQuery{Limit: 1000})
	if err != nil {
		return err
	}
	// Not again what is waiting, or what people turned down.
	asked := map[string]bool{}
	for _, p := range have {
		if p.Open() || p.State == ledger.StateRefused || p.State == ledger.StateDropped || p.State == ledger.StateFailed || p.State == ledger.StateAmended {
			asked[p.Rule+"\x00"+p.Kind] = true
		}
	}
	var asks []ask
	if rs, err := s.l.Results(ctx, "suggestions", 1); err != nil {
		return err
	} else if len(rs) > 0 {
		var ss []verdict.Suggestion
		if err := rs[0].Decode(&ss); err != nil {
			return err
		}
		for _, x := range ss {
			asks = append(asks, ask{Rule: x.Rule, Kind: ledger.KindFor, For: x.NewFor, From: "suggestion"})
		}
	}
	near, err := s.nearMisses(ctx)
	if err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(near)) {
		for _, nm := range near[id] {
			a := ask{Rule: nm.Rule, From: "near miss", Incident: nm.Incident}
			switch {
			case nm.Value != nil:
				f, err := s.current(ctx, nm.Rule, false)
				if err != nil {
					continue
				}
				i := slices.IndexFunc(f.rule.Bounds(), func(b rules.Bound) bool {
					return b.Op == nm.Value.Op && b.Value == nm.Value.Bound && b.Of == nm.Value.Of && b.Unless == nm.Value.Unless
				})
				if i < 0 {
					continue
				}
				a.Kind, a.Bound, a.Value = ledger.KindThreshold, f.rule.Bounds()[i].At, nm.Value.New
			case nm.NewFor > 0 && nm.NewFor < nm.For:
				a.Kind, a.For = ledger.KindFor, nm.NewFor
			default:
				continue
			}
			asks = append(asks, a)
		}
	}
	spare, err := s.spareRules(ctx)
	if err != nil {
		return err
	}
	silent, err := s.silentRules(ctx)
	if err != nil {
		return err
	}
	for _, x := range []struct {
		names []string
		from  string
	}{{spare, "spare"}, {silent, "silent"}} {
		for _, name := range x.names {
			f, err := s.current(ctx, name, false)
			if err != nil || f.rule.Unsupported == rules.Paused {
				continue // off already: deleted four weeks after, below
			}
			kind := ledger.KindDelete
			if f.rule.UID != "" {
				kind = ledger.KindDisable
			}
			asks = append(asks, ask{Rule: name, Kind: kind, From: x.from})
		}
	}
	// A rule off for four weeks, and off still, deleted, approved again.
	for _, p := range have {
		if p.Kind == ledger.KindDisable && p.State == ledger.StateApplied && s.now().Sub(p.Updated) >= retireAfter {
			if f, err := s.current(ctx, p.Rule, false); err != nil || f.rule.Unsupported != rules.Paused {
				continue // turned on again by hand
			}
			asks = append(asks, ask{Rule: p.Rule, Kind: ledger.KindDelete, From: "off four weeks", Follows: p.ID})
		}
	}
	made := 0
	for _, a := range asks {
		if made >= proposeMost {
			break
		}
		if asked[a.Rule+"\x00"+a.Kind] {
			continue
		}
		asked[a.Rule+"\x00"+a.Kind] = true
		if _, err := s.propose(ctx, a, ledger.Proposer, false, true); err != nil {
			var b badRequest
			if errors.As(err, &b) {
				continue // nothing to propose for it
			}
			return err
		}
		made++
	}
	// Nothing of what a rule says: Lapilli doesn't write the text. A
	// finding the text can fix is offered, on the rule's page, as a form to
	// propose it (ruleChanges.SaysForms).
	return nil
}

// silentRules are the rules in effect that never fired over the twelve
// weeks, once the ledger has twelve weeks: before, a rule is silent only
// for want of time.
func (s *Server) silentRules(ctx context.Context) ([]string, error) {
	began, err := s.l.RunsFrom(ctx)
	if err != nil || began.IsZero() || s.now().Sub(began) < weeks*weekLen {
		return nil, err
	}
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	_, silent, err := s.ruleLines(ctx, "en", t, weeks)
	return silent, err
}

// clip cuts a text to n bytes at most, at a rune.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

type proposeIn struct {
	Rule   string   `json:"rule" jsonschema:"the rule's name, as rules gives it"`
	Kind   string   `json:"kind" jsonschema:"for, threshold, retire or says"`
	For    string   `json:"for,omitempty" jsonschema:"the new for, such as 15m: kind for"`
	Bound  int      `json:"bound,omitempty" jsonschema:"which number of its condition, from 0, as written: kind threshold"`
	Value  *float64 `json:"value,omitempty" jsonschema:"the threshold's new number: kind threshold"`
	Reason string   `json:"reason,omitempty" jsonschema:"why, in a few sentences: shown as your words"`
	// What it says.
	Annotation string `json:"annotation,omitempty" jsonschema:"summary, description, runbook_url or runbook: kind says"`
	Text       string `json:"text,omitempty" jsonschema:"what the annotation is to say, as you wrote it (a summary or description of 4 KB at most; a runbook an https URL): kind says"`
	Unset      bool   `json:"unset,omitempty" jsonschema:"take the annotation out instead: kind says"`
	Check      string `json:"check,omitempty" jsonschema:"the content check's finding it answers, as the rule tool gives it, such as no_runbook: kind says"`
}

type proposeOut struct {
	ID   int64  `json:"id"`
	Path string `json:"path" jsonschema:"git, a pull request once approved; grafana, Grafana's API once approved; none, for a person to make"`
	Page string `json:"page,omitempty" jsonschema:"its page, where a person approves it"`
}

// toolPropose is an agent proposing a change, for the person it acts for:
// kept as an agent's, which that person can't approve. An agent can't
// approve or apply one.
func (s *Server) toolPropose(ctx context.Context, req *mcp.CallToolRequest, in proposeIn) (*mcp.CallToolResult, proposeOut, error) {
	var out proposeOut
	by := "anonymous"
	if req != nil && req.Extra != nil {
		by = s.user(&http.Request{Header: req.Extra.Header})
	}
	reason := strings.TrimSpace(strings.Map(func(c rune) rune {
		if c != '\n' && (unicode.IsControl(c) || unicode.Is(unicode.Cf, c)) {
			return -1
		}
		return c
	}, in.Reason))
	if len(reason) > 2000 {
		return nil, out, errors.New("reason is 2,000 characters at most")
	}
	q := ask{Rule: in.Rule, From: "agent", Reason: reason}
	switch in.Kind {
	case "for":
		d, err := ParseDuration(in.For)
		if err != nil {
			return nil, out, fmt.Errorf("for %q: want a duration such as 15m", in.For)
		}
		q.Kind, q.For = ledger.KindFor, d
	case "threshold":
		if in.Value == nil {
			return nil, out, errors.New("a threshold is given its new number in value")
		}
		q.Kind, q.Bound, q.Value = ledger.KindThreshold, in.Bound, *in.Value
	case "retire":
		f, err := s.current(ctx, in.Rule, false)
		if err != nil {
			return nil, out, err
		}
		q.Kind = ledger.KindDelete
		if f.rule.UID != "" && f.rule.Unsupported != rules.Paused {
			q.Kind = ledger.KindDisable // off first; deleted once off four weeks
		}
	case "says":
		q.Kind, q.Annotation, q.Text, q.Unset, q.Check = ledger.KindSays, in.Annotation, in.Text, in.Unset, in.Check
	default:
		return nil, out, errors.New("kind is for, threshold, retire or says")
	}
	id, err := s.propose(ctx, q, by, true, false)
	if err != nil {
		return nil, out, err
	}
	p, err := s.l.ProposalByID(ctx, id)
	if err != nil {
		return nil, out, err
	}
	out.ID, out.Path = id, p.Path
	if s.cfg.ExternalURL != "" {
		out.Page = fmt.Sprintf("%s/proposals/%d", strings.TrimRight(s.cfg.ExternalURL, "/"), id)
	}
	return nil, out, nil
}
