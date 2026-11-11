package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// Changing a rule kept in a managed Prometheus ruler, through its own API
// (docs/design/rulers.md): only to --ruler's host, with the write
// credentials given for it, following no redirect, and only in the
// namespaces or rule groups --changes-in names. What a write replaces is
// the unit (a Mimir or Cortex group, an Amazon namespace, an Azure rule
// group): read again before it is sent, kept for undo, and read back after.

// Why a rule read from a ruler is for a person to change: shown on its
// proposal's page.
const (
	noPathIn        = "its namespace isn't one --changes-in names"
	noPathInAzure   = "its rule group isn't one --changes-in names"
	noPathName      = "the ruler names its namespace or group so that Lapilli doesn't write to it"
	noPathCFN       = "CloudFormation keeps its namespace, as its tag aws:cloudformation:stack-id says: change it in the stack's template"
	noPathManaged   = "another tool keeps it, as a tag --changes-managed names says: change it where that tool keeps it"
	noPathLastNS    = "it is its namespace's last rule: a person deletes the namespace"
	noPathLastGroup = "it is its rule group's last rule: a person deletes the rule group"
	noPathEdit      = "the change can't be written into the ruler's group as it reads"
)

// noPaths are every reason a rule read from a ruler is for a person.
var noPaths = []string{noPathIn, noPathInAzure, noPathName, noPathCFN, noPathManaged, noPathLastNS, noPathLastGroup, noPathEdit}

// unitName is a unit's name in a diff: the namespace and group, an Amazon
// namespace, the resource group and rule group.
func (s *Server) unitName(ns, group string) string {
	if s.ruler.Kind == source.RulerAmazon {
		return ns
	}
	return ns + "/" + group
}

// changesIn reports whether --changes-in names a namespace (Mimir, Cortex,
// Amazon) or a rule group (Azure: RG/NAME, or NAME when --ruler names one
// resource group, in any letter case, as Azure compares names). A name
// ending in * is a prefix.
func (s *Server) changesIn(ns, group string) bool {
	match := func(pat, name string) bool {
		if p, ok := strings.CutSuffix(pat, "*"); ok {
			return strings.HasPrefix(name, p)
		}
		return pat == name
	}
	for _, pat := range s.cfg.ChangesIn {
		if s.ruler.Kind != source.RulerAzure {
			if match(pat, ns) {
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
		if match(strings.ToLower(pat), strings.ToLower(ns+"/"+group)) {
			return true
		}
	}
	return false
}

// rulerAllowed is why Lapilli may not change a rule in a namespace and a
// group, "" when it may: --changes-in names it, and its names can be
// written to.
func (s *Server) rulerAllowed(ns, group string) string {
	switch {
	case s.ruler.NameOK(ns, true) != nil || s.ruler.NameOK(group, false) != nil:
		return noPathName
	case !s.changesIn(ns, group) && s.ruler.Kind == source.RulerAzure:
		return noPathInAzure
	case !s.changesIn(ns, group):
		return noPathIn
	}
	return ""
}

// rulerManaged is why another tool keeps a unit, as its tags say, "" when
// nothing says so: CloudFormation's own tag on an Amazon namespace, and
// those --changes-managed names. A Mimir's or a Cortex's group carries
// nothing that could say: --changes-in alone decides.
func (s *Server) rulerManaged(u source.Unit) string {
	if s.ruler.Kind == source.RulerAmazon {
		for k := range u.Tags {
			if strings.EqualFold(k, "aws:cloudformation:stack-id") {
				return noPathCFN
			}
		}
	}
	for _, m := range s.cfg.ChangesManaged {
		key, val, _ := strings.Cut(m, "=")
		for k, v := range u.Tags {
			if strings.EqualFold(k, key) && (val == "*" || strings.EqualFold(v, val)) {
				return noPathManaged
			}
		}
	}
	return ""
}

// unitRules are the rules in a unit, each with its namespace, as Read
// reads them.
func unitRules(u source.Unit) ([]rules.Rule, error) {
	if len(u.Raw) == 0 {
		return nil, nil
	}
	return rules.FromDocs([]rules.Doc{{Source: u.Source, Namespace: u.Namespace, Data: u.Raw}})
}

// unitSize is how many rules a unit's group holds, of any kind, and how
// many groups it holds.
func unitSize(raw []byte, group string) (int, int) {
	var t map[string]any
	if yaml.Unmarshal(raw, &t) != nil {
		return 0, 0
	}
	if props, ok := t["properties"].(map[string]any); ok {
		rs, _ := props["rules"].([]any)
		return len(rs), 1
	}
	gs, _ := t["groups"].([]any)
	for _, g := range gs {
		if gm, _ := g.(map[string]any); gm != nil && fmt.Sprint(gm["name"]) == group {
			rs, _ := gm["rules"].([]any)
			return len(rs), len(gs)
		}
	}
	return 0, len(gs)
}

// rulerEdit is a unit with the change made in it: by lines in a rule file
// (a Mimir or Cortex group, an Amazon namespace), in the tree in an Azure
// rule group. A group goes with its last rule, as in a file; an Amazon
// namespace or an Azure rule group never does. why says why it can't be
// made, when it can't.
func (s *Server) rulerEdit(u source.Unit, r rules.Rule, e rules.Edit, kind string, b rules.Bound, v float64) (out []byte, why string) {
	azure := s.ruler.Kind == source.RulerAzure
	if e.Delete {
		n, groups := unitSize(u.Raw, e.Group)
		switch {
		case n == 1 && azure:
			return nil, noPathLastGroup
		case n == 1 && s.ruler.Kind == source.RulerConfig:
			return []byte("groups: []\n"), "" // the group goes: DELETE
		case n == 1 && groups == 1:
			return nil, noPathLastNS
		}
	}
	var err error
	switch {
	case azure && kind == ledger.KindThreshold:
		out, err = rules.EditTreeThreshold(u.Raw, r, e, b, v)
	case azure:
		out, err = rules.EditTree(u.Raw, e)
	case kind == ledger.KindThreshold:
		out, err = rules.EditThreshold(u.Raw, r, e, b, v)
	default:
		out, err = rules.EditDoc(u.Raw, e)
	}
	if err != nil {
		return nil, noPathEdit
	}
	return out, ""
}

// readsRuler reports whether a unit edited reads as the rules before with
// want in place of r, as readsAs and readsSaying check a file, and gives
// the rule as read, with its namespace.
func readsRuler(before, after []byte, r, want rules.Rule, kind string) (*rules.Rule, bool) {
	rr, ww := r, want
	rr.Namespace, ww.Namespace = "", ""
	var got *rules.Rule
	var ok bool
	if kind == ledger.KindSays {
		got, ok = readsSaying(before, after, rr, ww)
	} else {
		got, ok = readsAs(before, after, rr, ww, kind == ledger.KindDelete)
	}
	if got != nil {
		g := *got
		g.Namespace = r.Namespace
		got = &g
	}
	return got, ok
}

// rulerPropose gives a proposal for a rule read from --ruler the ruler
// path, when it may take it: its namespace or rule group named in
// --changes-in, nothing on it saying another tool keeps it, and the change
// one that can be made there, made in the unit as it is now and read
// back. Otherwise it is for a person, and its evidence says why.
func (s *Server) rulerPropose(ctx context.Context, p *ledger.Proposal, ev *evidence, f found, want rules.Rule, e rules.Edit, b rules.Bound, v float64) error {
	r := f.rule
	if why := s.rulerAllowed(r.Namespace, r.Group); why != "" {
		ev.NoPath = why
		return nil
	}
	u, err := s.ruler.Unit(ctx, r.Namespace, r.Group)
	if err != nil {
		return err
	}
	if u.Gone || s.ruler.Owns(f.doc.Source, r.Namespace, r.Group) != nil {
		return bad("the rule changed since it was read: try again in a minute")
	}
	if why := s.rulerManaged(u); why != "" {
		ev.NoPath = why
		return nil
	}
	rs, err := unitRules(u)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(rs, func(x rules.Rule) bool { return hashOf(*p, x) == p.BeforeHash }) {
		return bad("the rule changed since it was read: try again in a minute")
	}
	out, why := s.rulerEdit(u, r, e, p.Kind, b, v)
	if why != "" {
		ev.NoPath = why
		return nil
	}
	after, ok := readsRuler(u.Raw, out, r, want, p.Kind)
	if !ok {
		ev.NoPath = noPathEdit
		return nil
	}
	p.Path, p.Document, p.DocHash = ledger.PathRuler, string(out), u.Hash
	p.Diff = rules.Diff(s.unitName(r.Namespace, r.Group), u.Raw, out)
	if after != nil {
		p.AfterHash = hashOf(*p, *after)
	}
	return nil
}

// amazonActive says whether an Amazon namespace may be written now: ACTIVE.
// One being made, updated or deleted waits (errWaits); one that failed is
// refused, with Amazon's reason.
func (s *Server) amazonActive(u source.Unit) error {
	if s.ruler.Kind != source.RulerAmazon {
		return nil
	}
	switch u.Status {
	case "ACTIVE":
		return nil
	case "CREATING", "UPDATING", "DELETING":
		return errWaits
	case "CREATION_FAILED", "UPDATE_FAILED":
		return refusal{clip(fmt.Sprintf("the namespace is %s in Amazon: %s", u.Status, u.StatusReason), 500)}
	}
	return refusal{clip(fmt.Sprintf("the namespace's status in Amazon is %q, not ACTIVE", u.Status), 200)}
}

// wantOf is a rule as an approved proposal would make it, and a
// threshold's number: made afresh from the rule as it is now.
func wantOf(p ledger.Proposal, ev evidence, now rules.Rule) (rules.Rule, rules.Bound, float64, bool) {
	want := now
	var b rules.Bound
	var v float64
	e := ev.Edit
	switch p.Kind {
	case ledger.KindFor:
		want.For = ev.For
	case ledger.KindThreshold:
		if ev.Bound == nil || ev.Value == nil {
			return want, b, v, false
		}
		j := slices.IndexFunc(now.Bounds(), func(x rules.Bound) bool { return x.At == *ev.Bound })
		if j < 0 {
			return want, b, v, false
		}
		b, v = now.Bounds()[j], *ev.Value
		want = now.WithBound(b, v)
	case ledger.KindDisable:
		want.Unsupported, want.Graph = rules.Disabled, nil
	case ledger.KindSays:
		want.Says = saysAfter(now.Says, e.Annotation, e.Value, e.Unset)
	}
	return want, b, v, true
}

// rulerApply applies an approved proposal in the ruler: the place worked
// out again from --ruler, the unit read again and checked, the change made
// afresh in it when other rules in it changed since, and the unit as it
// was kept; then rulerWrite sends it. A refusal says why it can't be.
func (s *Server) rulerApply(ctx context.Context, p ledger.Proposal, step ledger.Step) error {
	ev := proposalEvidence(p)
	if s.ruler == nil || ev.Edit == nil || ev.Edit.Namespace == "" {
		return refusal{"the proposal names no rule in a ruler, or lapilli serve no longer reads one (--ruler)"}
	}
	e := *ev.Edit
	// Written where it was read, and nowhere else.
	if err := s.ruler.Owns(p.Source, e.Namespace, e.Group); err != nil {
		return refusal{err.Error()}
	}
	// A flag changed by a restart refuses it now.
	if why := s.rulerAllowed(e.Namespace, e.Group); why != "" {
		return refusal{why}
	}
	u, err := s.ruler.Unit(ctx, e.Namespace, e.Group)
	if errors.Is(err, source.ErrElsewhere) {
		return refusal{"the ruler answered with another namespace's or group's: " + err.Error()}
	}
	if err != nil {
		return err
	}
	if p.Kind == ledger.KindUndo {
		return s.rulerUndo(ctx, p, step, u, e)
	}
	if u.Gone {
		return refusal{"the rule's group is gone from the ruler"}
	}
	if why := s.rulerManaged(u); why != "" {
		return refusal{why}
	}
	if err := s.amazonActive(u); err != nil {
		return err
	}
	rs, err := unitRules(u)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rs, func(x rules.Rule) bool { return hashOf(p, x) == p.BeforeHash })
	if i < 0 {
		return refusal{"the rule changed in the ruler since it was proposed: propose it again"}
	}
	now := rs[i]
	if p.Kind == ledger.KindSays && !e.Unset {
		if !slices.Contains(saysKeys, e.Annotation) {
			return refusal{"the proposal names no annotation Lapilli changes"}
		}
		if err := saidOK(e.Annotation, e.Value, now.Says.Template); err != nil {
			return refusal{err.Error()}
		}
	}
	doc := []byte(p.Document)
	if u.Hash != p.DocHash {
		// Other rules in the unit changed since: the change is made afresh
		// in it as it is now, and read back, every other rule as the ruler
		// holds it a moment before.
		want, b, v, ok := wantOf(p, ev, now)
		if !ok {
			return refusal{"the rule changed in the ruler since it was proposed: propose it again"}
		}
		out, why := s.rulerEdit(u, now, e, p.Kind, b, v)
		if why != "" {
			return refusal{"the change can't be made in the ruler as it is now: " + why}
		}
		after, ok := readsRuler(u.Raw, out, now, want, p.Kind)
		if !ok || (after != nil && p.AfterHash != "" && hashOf(p, *after) != p.AfterHash) {
			return refusal{"the change can't be made in the ruler as it is now: propose it again"}
		}
		doc = out
		step.Document, step.DocHash, step.Diff = string(out), u.Hash, rules.Diff(s.unitName(e.Namespace, e.Group), u.Raw, out)
	}
	step.Backup = string(u.Raw)
	return s.rulerWrite(ctx, p, step, u, doc)
}

// rulerUndo puts back the rule an applied proposal changed, as it was kept,
// in the unit as it is now: not the whole unit, whose other rules may have
// been changed since, nor a whole group that went since, only that one
// rule in it. The rule must be as the change left it, every key of it, not
// only those its hash covers; a deleted one, not in its group again.
func (s *Server) rulerUndo(ctx context.Context, p ledger.Proposal, step ledger.Step, u source.Unit, e rules.Edit) error {
	if p.Backup == "" {
		return refusal{"what the change replaced isn't kept, so it can't be put back"}
	}
	if u.Gone && s.ruler.Kind != source.RulerConfig {
		return refusal{"the namespace or rule group is gone from the ruler: undo it there"}
	}
	rs, err := unitRules(u)
	if err != nil {
		return err
	}
	named := slices.ContainsFunc(rs, func(x rules.Rule) bool { return x.Name == e.Name && x.Group == e.Group })
	switch {
	case p.BeforeHash == "" && named: // a deletion undone
		return refusal{"a rule of its name is in its group again: undo it in the ruler"}
	case p.BeforeHash != "" && !slices.ContainsFunc(rs, func(x rules.Rule) bool { return hashOf(p, x) == p.BeforeHash }):
		return refusal{"the rule changed in the ruler since the change: undo it in the ruler"}
	case p.BeforeHash != "":
		// Every key as the change wrote it: an action group, a severity or
		// an annotation set since, which no hash covers, isn't undone with
		// it.
		orig, err := s.l.ProposalByID(ctx, p.Follows)
		if err != nil {
			return err
		}
		left, ok1 := rules.Entry([]byte(orig.Document), e)
		now, ok2 := rules.Entry(u.Raw, e)
		if !ok1 || !ok2 || !reflect.DeepEqual(left, now) {
			return refusal{"the rule changed in the ruler since the change: undo it in the ruler"}
		}
	}
	if !u.Gone {
		if why := s.rulerManaged(u); why != "" {
			return refusal{why}
		}
		if err := s.amazonActive(u); err != nil {
			return err
		}
	}
	out, err := rules.PutBack(u.Raw, []byte(p.Backup), e)
	if err != nil {
		return refusal{"the rule as it was can't be put back in the ruler as it is now: undo it in the ruler"}
	}
	back, err := unitRules(source.Unit{Source: u.Source, Namespace: u.Namespace, Raw: out})
	if err != nil || !slices.ContainsFunc(back, func(x rules.Rule) bool { return x.Name == e.Name && hashOf(p, x) == p.AfterHash }) ||
		!rules.Adds(u.Raw, out, e) {
		return refusal{"the rule as it was can't be put back in the ruler as it is now: undo it in the ruler"}
	}
	step.Document, step.DocHash, step.Diff = string(out), u.Hash, rules.Diff(s.unitName(e.Namespace, e.Group), u.Raw, out)
	step.Backup = string(u.Raw)
	return s.rulerWrite(ctx, p, step, u, out)
}

// rulerWrite sends a unit to the ruler. The pause is looked at once more
// just before; then the proposal is kept as applying, with the unit as it
// was and counted against the limits, before anything is sent, so that
// whatever the ruler answers, or doesn't, the change is neither lost nor
// sent twice. What became of it is read back, not taken from the answer.
func (s *Server) rulerWrite(ctx context.Context, p ledger.Proposal, step ledger.Step, u source.Unit, doc []byte) error {
	// What is sent is worked out before anything is kept.
	method, at, ctype := http.MethodPut, u.At, "application/json"
	var body []byte
	var err error
	switch s.ruler.Kind {
	case source.RulerConfig:
		var ok bool
		body, ok, err = source.GroupOf(doc)
		if err != nil {
			return refusal{"the group can't be written out as it reads: " + err.Error()}
		}
		if ok {
			method, at, ctype = http.MethodPost, s.ruler.WriteAt(u), "application/yaml"
		} else {
			method, body = http.MethodDelete, nil // the group went with its last rule
		}
	case source.RulerAzure:
		if body, err = source.AzureBody(doc); err != nil {
			return refusal{err.Error()}
		}
	}
	if s.changesPaused() {
		return errPaused
	}
	step.Applied = true
	p, err = s.l.Move(ctx, p.ID, []string{ledger.StateApproved}, ledger.StateApplying, ledger.Proposer, step)
	if err != nil {
		return err
	}
	if s.ruler.Kind == source.RulerAmazon {
		// The SDK's way: a request repeated after a timeout isn't taken
		// twice. Lapilli never sends a change again.
		body, _ = json.Marshal(map[string]string{"data": base64.StdEncoding.EncodeToString(doc),
			"clientToken": fmt.Sprintf("lapilli-%d-%d", p.ID, p.Applied.Unix())})
	}
	code, out, err := s.ruler.Call(ctx, method, at, body, ctype, s.rulerWrites, source.UnitLimit)
	var sent error
	switch {
	case err != nil:
		sent = err
	case code/100 != 2:
		sent = source.Failed(code, out)
	}
	return s.rulerSettle(ctx, p, sent)
}

// notReadBackRuler is why a change sent to a ruler is still applying.
const notReadBackRuler = "sent to the ruler, but not read back yet: it is read again each round, and never sent again"

// rulerSettle reads back the unit a change was sent to, and says what
// became of the change: applied, when the unit reads as written (the rule
// as proposed, every other rule as it was); failed, when it reads as it
// was, or as neither (written to meanwhile: what it was is kept); still
// applying, with why, when it can't be read, or an Amazon namespace is
// being updated. sent is the ruler's answer to the change, when it was an
// error.
func (s *Server) rulerSettle(ctx context.Context, p ledger.Proposal, sent error) error {
	ev := proposalEvidence(p)
	var u source.Unit
	err := errors.New("the proposal names no rule in a ruler")
	if s.ruler != nil && ev.Edit != nil && ev.Edit.Namespace != "" {
		u, err = s.ruler.Unit(ctx, ev.Edit.Namespace, ev.Edit.Group)
	}
	still := func(why string) error {
		if sent != nil {
			why += "; the ruler answered: " + sent.Error()
		}
		if why == p.Reason {
			return nil
		}
		if _, err := s.l.Move(ctx, p.ID, []string{ledger.StateApplying}, ledger.StateApplying, ledger.Proposer, ledger.Step{Reason: clip(why, 500)}); err != nil && !errors.Is(err, ledger.ErrMoved) {
			return err
		}
		return nil
	}
	fail := func(why string) error {
		_, err := s.l.Move(ctx, p.ID, []string{ledger.StateApplying}, ledger.StateFailed, ledger.Proposer, ledger.Step{Reason: clip(why, 500)})
		if errors.Is(err, ledger.ErrMoved) {
			return nil
		}
		return err
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		s.log.Warn("a change sent to the ruler isn't read back yet", "proposal", p.ID, "err", err)
		return still(notReadBackRuler)
	}
	if s.ruler.Kind == source.RulerAmazon && !u.Gone {
		switch u.Status {
		case "CREATING", "UPDATING":
			return still("sent to Amazon, which is updating the namespace: it is read again each round until it is ACTIVE, and never sent again")
		case "UPDATE_FAILED", "CREATION_FAILED":
			return fail(fmt.Sprintf("Amazon couldn't apply it: the namespace is %s: %s", u.Status, u.StatusReason))
		}
	}
	switch {
	case s.sameUnit(u, []byte(p.Document)):
	case s.sameUnit(u, []byte(p.Backup)):
		why := "the ruler answered that it took the change, but it reads back as it was"
		if sent != nil {
			why = sent.Error()
		}
		return fail(why)
	default:
		why := "the ruler reads as neither it was nor as proposed: it may have been written to meanwhile, and what it was is kept on the proposal"
		if sent != nil {
			why += "; the ruler answered: " + sent.Error()
		}
		return fail(why)
	}
	step := ledger.Step{}
	if rs, err := unitRules(u); err == nil {
		for _, x := range rs {
			if x.Name == ev.Edit.Name && x.Group == ev.Edit.Group {
				step.AfterHash = hashOf(p, x)
			}
		}
	}
	switch {
	case sent != nil:
		step.Reason = clip("the ruler answered with an error, but it reads back changed: "+sent.Error(), 500)
	case p.Reason != "":
		step.Reason = "read back applied, after it couldn't be at first"
	}
	if _, err := s.l.Move(ctx, p.ID, []string{ledger.StateApplying}, ledger.StateApplied, ledger.Proposer, step); err != nil {
		if errors.Is(err, ledger.ErrMoved) {
			return nil
		}
		return err
	}
	if p.Kind == ledger.KindUndo {
		if _, err := s.l.Move(ctx, p.Follows, []string{ledger.StateApplied}, ledger.StateUndone, ledger.Proposer, ledger.Step{}); err != nil && !errors.Is(err, ledger.ErrMoved) {
			return err
		}
	}
	return nil
}

// sameUnit reports whether a unit read back holds what raw holds: the same
// alert rules, each as it fires and says, and the rest alike, however the
// ruler writes it out. A gone Mimir or Cortex group is a file of none; a
// gone Amazon namespace or Azure rule group is nothing written.
func (s *Server) sameUnit(u source.Unit, raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	if u.Gone && s.ruler.Kind != source.RulerConfig {
		return false
	}
	a, err1 := rules.Shape(u.Raw, source.AzureSets)
	b, err2 := rules.Shape(raw, source.AzureSets)
	return err1 == nil && err2 == nil && a == b
}
