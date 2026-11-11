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
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// Changing a rule kept behind an API, once a person approves: a managed
// Prometheus ruler's (docs/design/rulers.md) or a cloud's own alarm
// (docs/design/alarms.md). One path, over a target: the unit a write
// replaces (a Mimir or Cortex group, an Amazon namespace, an Azure rule
// group; an alarm, a Google policy) is read again before it is sent, kept
// for undo, and read back after; only to the host the flag names, with the
// write credentials given for it, following no redirect, and only where
// --changes-in names.

// apiUnit is a unit as a target read it.
type apiUnit struct {
	Raw  []byte
	Hash string
	Gone bool
	// The unit as its reader gave it: a ruler's, or a cloud's alarm, and
	// the --alarms source it was read under.
	ruler source.Unit
	alarm source.AlarmUnit
	ns    string
}

// target is where a rule read through an API is changed.
type target interface {
	// path is the proposal's path, and where what the API is, in a reason.
	path() string
	where() string
	// unitName is a unit's name in a diff.
	unitName(src string, e rules.Edit) string
	// allowed is why Lapilli may not change the rule there, "" when it
	// may: --changes-in names it, and it can be written to.
	allowed(src string, e rules.Edit) string
	// owns checks that a proposal's source is this target's, for its
	// rule: a proposal is written where it was read.
	owns(src string, e rules.Edit) error
	// unit reads the unit again from the API.
	unit(ctx context.Context, src string, e rules.Edit) (apiUnit, error)
	// managed is why another tool keeps a unit, as its tags say, "".
	managed(u apiUnit) string
	// ready says whether a change of a kind may be written to a unit now:
	// nil, errWaits, errFires, or a refusal; offered says whether it may
	// be proposed at all, and why not.
	ready(ctx context.Context, u apiUnit, p ledger.Proposal) error
	offered(ctx context.Context, u apiUnit, r rules.Rule, kind string) (string, error)
	// rules are the rules a unit holds, as Read reads them.
	rules(u apiUnit) ([]rules.Rule, error)
	// edit is the unit with the change made in it, or why it can't be.
	edit(u apiUnit, r rules.Rule, e rules.Edit, kind string, b rules.Bound, v float64) ([]byte, string)
	// reads reports whether a unit edited reads as the rules before with
	// want in place of r, and gives the rule as read.
	reads(before, after []byte, r, want rules.Rule, kind string) (*rules.Rule, bool)
	// putBack is the unit as it is now with the rule an applied change
	// changed put back as it was kept; undoGone says a unit that is gone
	// may be put back into, as a deleted one is made again.
	putBack(u apiUnit, p ledger.Proposal, e rules.Edit) ([]byte, error)
	undoGone(u apiUnit, p ledger.Proposal) bool
	// request is what is sent, worked out before anything is kept: a
	// function that sends it, and gives the API's answer when it was an
	// error.
	request(ctx context.Context, p ledger.Proposal, u apiUnit, doc []byte) (func(context.Context, ledger.Proposal) error, error)
	// readBack reads back the unit a change was sent to.
	readBack(ctx context.Context, p ledger.Proposal, e rules.Edit) (apiUnit, error)
	// settling says, of a unit read back, that it is still being written,
	// or that the API failed it, by the API's own state.
	settling(u apiUnit) (still, failed string)
	// same reports whether a unit read back holds raw.
	same(u apiUnit, raw []byte) bool
	// gone is why a change is refused when its unit is gone; cantEdit why
	// a change made in it doesn't read back as proposed.
	gone() string
	cantEdit() string
	// goneUndo is why an undo is refused when the unit is gone.
	goneUndo() string
	// again is why a deletion isn't undone: one of its name is there again.
	again() string
	// goneAllowed is why a unit that is gone may not be made again by an
	// undo, as it was kept (p.Backup): its name no longer one --changes-in
	// names, its tags another tool's; "" when it may.
	goneAllowed(p ledger.Proposal, e rules.Edit) string
	// backup is what a write keeps for undo: the unit as it was.
	backup(u apiUnit) []byte
	// lag is how long after a change was sent a unit that still reads as
	// it was is read again before the change is called failed, by the
	// clock (a round can come at once), and why it is, meanwhile: what
	// an API caching its storage, or read a moment behind a write, may lag
	// by; none where it isn't.
	lag() (time.Duration, string)
	// notThere is why a change is still applying when the flag no longer
	// names where it was sent: it is read back there only.
	notThere() string
	// oneUnit reports whether two changes, q and p, are to one unit: a
	// change waits while another to its unit isn't read back.
	oneUnit(q ledger.Proposal, qe rules.Edit, p ledger.Proposal, e rules.Edit) bool
	// leftAsChanged reports whether a unit holds the rule as the change an
	// undo undoes (orig) left it: every key of it, not only those its hash
	// covers, durations as lengths of time.
	leftAsChanged(orig ledger.Proposal, u apiUnit, p ledger.Proposal, e rules.Edit) bool
	// onlyRule reports whether out is the unit as it is now with that one
	// rule put back, and nothing else.
	onlyRule(u apiUnit, out []byte, p ledger.Proposal, e rules.Edit) bool
}

// readBackWithin is how long after a change was sent a unit that still
// reads as it was is read again before the change is called failed: what
// a ruler caching its storage, or a cloud read a moment behind, may lag by.
const readBackWithin = 2 * time.Minute

// Why a change sent is still applying: read back as it was, within
// readBackWithin, from a ruler or a cloud; or the flag names another place
// since.
const (
	asWasYet      = "sent to the ruler, which took it, but still gives it as it was: it is read again each round for two minutes from when it was sent, as a ruler caching its storage may lag, before the change is called failed"
	readBehind    = "sent to the cloud, which took it, but still reads as it was: it is read again each round for two minutes from when it was sent, as a cloud may be read a moment behind a write, before the change is called failed; it is never sent again"
	notThereRuler = "--ruler no longer names the ruler the change was sent to: it is read back there once it does again, and never sent again"
	notThereAlarm = "--alarms no longer names where the change was sent: it is read back there once it does again, and never sent again"
)

// targetOf is where a proposal's rule is changed, nil for none.
func (s *Server) targetOf(path string) target {
	switch {
	case path == ledger.PathRuler && s.ruler != nil:
		return rulerTarget{s}
	case path == ledger.PathAlarm && len(s.alarms) > 0:
		return alarmTarget{s}
	}
	return nil
}

// apiPropose gives a proposal for a rule read through an API the target's
// path, when it may take it: --changes-in names it, nothing on it says
// another tool keeps it, and the change is one that can be made there,
// made in the unit as it is now and read back. Otherwise it is for a
// person, and its evidence says why.
func (s *Server) apiPropose(ctx context.Context, tg target, p *ledger.Proposal, ev *evidence, f found, want rules.Rule, e rules.Edit, b rules.Bound, v float64) error {
	r := f.rule
	if why := tg.allowed(f.doc.Source, e); why != "" {
		ev.NoPath = why
		return nil
	}
	u, err := tg.unit(ctx, f.doc.Source, e)
	if errors.Is(err, source.ErrUnreadable) {
		ev.NoPath = noPathUnread
		return nil
	}
	if err != nil {
		return err
	}
	if u.Gone || tg.owns(f.doc.Source, e) != nil {
		return bad("the rule changed since it was read: try again in a minute")
	}
	if why := tg.managed(u); why != "" {
		ev.NoPath = why
		return nil
	}
	if why, err := tg.offered(ctx, u, r, p.Kind); err != nil {
		return err
	} else if why != "" {
		ev.NoPath = why
		return nil
	}
	rs, err := tg.rules(u)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(rs, func(x rules.Rule) bool { return hashOf(*p, x) == p.BeforeHash }) {
		return bad("the rule changed since it was read: try again in a minute")
	}
	out, why := tg.edit(u, r, e, p.Kind, b, v)
	if why != "" {
		ev.NoPath = why
		return nil
	}
	after, ok := tg.reads(u.Raw, out, r, want, p.Kind)
	if !ok {
		ev.NoPath = tg.cantEdit()
		return nil
	}
	p.Path, p.Document, p.DocHash = tg.path(), string(out), u.Hash
	p.Diff = rules.Diff(tg.unitName(f.doc.Source, e), u.Raw, out)
	if after != nil {
		p.AfterHash = hashOf(*p, *after)
	}
	return nil
}

// apiApply applies an approved proposal through its target: the place
// worked out again from the flag, the unit read again and checked, the
// change made afresh in it when only the rest of it changed since, and the
// unit as it was kept; then apiWrite sends it. A refusal says why it can't
// be.
func (s *Server) apiApply(ctx context.Context, tg target, p ledger.Proposal, step ledger.Step) error {
	ev := proposalEvidence(p)
	if ev.Edit == nil || ev.Edit.Namespace == "" {
		return refusal{"the proposal names no rule read through an API Lapilli still reads"}
	}
	e := *ev.Edit
	// Written where it was read, and nowhere else.
	if err := tg.owns(p.Source, e); err != nil {
		return refusal{err.Error()}
	}
	// A flag changed by a restart refuses it now.
	if why := tg.allowed(p.Source, e); why != "" {
		return refusal{why}
	}
	// One change to a unit at a time: one written over another not yet read
	// back would make that one read back as neither it was nor as proposed.
	if busy, err := s.unitBusy(ctx, tg, p, e); err != nil {
		return err
	} else if busy {
		return errWaits
	}
	u, err := tg.unit(ctx, p.Source, e)
	if errors.Is(err, source.ErrElsewhere) || errors.Is(err, source.ErrAlarmElsewhere) {
		return refusal{fmt.Sprintf("%s answered with another's: %v", tg.where(), err)}
	}
	if errors.Is(err, source.ErrUnreadable) {
		return refusal{noPathUnread}
	}
	if err != nil {
		return err
	}
	if p.Kind == ledger.KindUndo {
		return s.apiUndo(ctx, tg, p, step, u, e)
	}
	if u.Gone {
		return refusal{tg.gone()}
	}
	if why := tg.managed(u); why != "" {
		return refusal{why}
	}
	rs, err := tg.rules(u)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rs, func(x rules.Rule) bool { return hashOf(p, x) == p.BeforeHash })
	if i < 0 {
		return refusal{fmt.Sprintf("the rule changed in %s since it was proposed: propose it again", tg.where())}
	}
	now := rs[i]
	if why, err := tg.offered(ctx, u, now, p.Kind); err != nil {
		return err
	} else if why != "" {
		return refusal{why}
	}
	if err := tg.ready(ctx, u, p); err != nil {
		return err
	}
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
		// Only the rest of the unit changed since: the change is made
		// afresh in it as it is now, and read back, the rest as the API
		// holds it a moment before.
		want, b, v, ok := wantOf(p, ev, now)
		if !ok {
			return refusal{fmt.Sprintf("the rule changed in %s since it was proposed: propose it again", tg.where())}
		}
		out, why := tg.edit(u, now, e, p.Kind, b, v)
		if why != "" {
			return refusal{fmt.Sprintf("the change can't be made in %s as it is now: %s", tg.where(), why)}
		}
		after, ok := tg.reads(u.Raw, out, now, want, p.Kind)
		if !ok || (after != nil && p.AfterHash != "" && hashOf(p, *after) != p.AfterHash) {
			return refusal{fmt.Sprintf("the change can't be made in %s as it is now: propose it again", tg.where())}
		}
		doc = out
		step.Document, step.DocHash, step.Diff = string(out), u.Hash, rules.Diff(tg.unitName(p.Source, e), u.Raw, out)
	}
	step.Backup = string(tg.backup(u))
	return s.apiWrite(ctx, tg, p, step, u, doc)
}

// unitBusy reports whether another change to a proposal's unit was sent
// through its target and isn't read back yet: one to the same group (Mimir,
// Cortex), namespace (Amazon), rule group (Azure) or alarm.
func (s *Server) unitBusy(ctx context.Context, tg target, p ledger.Proposal, e rules.Edit) (bool, error) {
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: []string{ledger.StateApplying}, Limit: 1000})
	if err != nil {
		return false, err
	}
	for _, q := range ps {
		ev := proposalEvidence(q)
		if q.ID == p.ID || q.Path != tg.path() || ev.Edit == nil {
			continue
		}
		if tg.oneUnit(q, *ev.Edit, p, e) {
			return true, nil
		}
	}
	return false, nil
}

// apiUndo puts back the rule an applied proposal changed, as it was kept,
// in the unit as it is now: not the whole unit, whose other rules or
// fields may have been changed since, nor a whole group that went since,
// only that one rule in it. The rule must be as the change left it, every
// key of it, not only those its hash covers; a deleted one, not there
// again.
func (s *Server) apiUndo(ctx context.Context, tg target, p ledger.Proposal, step ledger.Step, u apiUnit, e rules.Edit) error {
	if p.Backup == "" {
		return refusal{"what the change replaced isn't kept, so it can't be put back"}
	}
	if u.Gone && !tg.undoGone(u, p) {
		return refusal{tg.goneUndo()}
	}
	rs, err := tg.rules(u)
	if err != nil {
		return err
	}
	orig, err := s.l.ProposalByID(ctx, p.Follows)
	if err != nil {
		return err
	}
	named := slices.ContainsFunc(rs, func(x rules.Rule) bool { return x.Name == e.Name && x.Group == e.Group })
	changed := refusal{fmt.Sprintf("the rule changed in %s since the change: undo it in %s", tg.where(), tg.where())}
	i := slices.IndexFunc(rs, func(x rules.Rule) bool { return hashOf(p, x) == p.BeforeHash })
	switch {
	case p.BeforeHash == "" && named: // a deletion undone
		return refusal{tg.again()}
	case p.BeforeHash != "" && i < 0:
		return changed
	case p.BeforeHash != "" && !tg.leftAsChanged(orig, u, p, e):
		// Every key as the change wrote it: an action group, a severity or
		// an annotation set since, which no hash covers, isn't undone with
		// it.
		return changed
	}
	// An undo is a write as any other: what would refuse the change it
	// undoes, were it made now, refuses it. A unit made again is asked of
	// as it was kept: its name, for --changes-in, and its tags.
	if u.Gone {
		if why := tg.goneAllowed(p, e); why != "" {
			return refusal{why}
		}
	} else {
		if why := tg.managed(u); why != "" {
			return refusal{why}
		}
		if i >= 0 {
			if why, err := tg.offered(ctx, u, rs[i], orig.Kind); err != nil {
				return err
			} else if why != "" {
				return refusal{why}
			}
		}
		if err := tg.ready(ctx, u, p); err != nil {
			return err
		}
	}
	out, err := tg.putBack(u, p, e)
	if err != nil {
		return refusal{fmt.Sprintf("the rule as it was can't be put back in %s as it is now: undo it there", tg.where())}
	}
	back, err := tg.rules(apiUnit{Raw: out, ruler: source.Unit{Source: u.ruler.Source, Namespace: u.ruler.Namespace}, alarm: u.alarm, ns: u.ns})
	if err != nil || !slices.ContainsFunc(back, func(x rules.Rule) bool { return x.Name == e.Name && hashOf(p, x) == p.AfterHash }) ||
		!tg.onlyRule(u, out, p, e) {
		return refusal{fmt.Sprintf("the rule as it was can't be put back in %s as it is now: undo it there", tg.where())}
	}
	step.Document, step.DocHash, step.Diff = string(out), u.Hash, rules.Diff(tg.unitName(p.Source, e), u.Raw, out)
	step.Backup = string(tg.backup(u))
	return s.apiWrite(ctx, tg, p, step, u, out)
}

// apiWrite sends a unit to its API. The pause is looked at once more just
// before; then the proposal is kept as applying, with the unit as it was
// and counted against the limits, before anything is sent, so that
// whatever the API answers, or doesn't, the change is neither lost nor sent
// twice. What became of it is read back, not taken from the answer.
func (s *Server) apiWrite(ctx context.Context, tg target, p ledger.Proposal, step ledger.Step, u apiUnit, doc []byte) error {
	// What is sent is worked out before anything is kept.
	send, err := tg.request(ctx, p, u, doc)
	if err != nil {
		return err
	}
	if s.changesPaused() {
		return errPaused
	}
	step.Applied = true
	p, err = s.l.Move(ctx, p.ID, []string{ledger.StateApproved}, ledger.StateApplying, ledger.Proposer, step)
	if err != nil {
		return err
	}
	return s.apiSettle(ctx, tg, p, send(ctx, p))
}

// apiSettle reads back the unit a change was sent to, and says what
// became of the change: applied, when the unit reads as written (the rule
// as proposed, the rest as it was); failed, when it reads as it was, or as
// neither (written to meanwhile: what it was is kept); still applying,
// with why, when it can't be read, or the API is still writing it. sent is
// the API's answer to the change, when it was an error.
func (s *Server) apiSettle(ctx context.Context, tg target, p ledger.Proposal, sent error) error {
	ev := proposalEvidence(p)
	var u apiUnit
	err := errors.New("the proposal names no rule read through an API")
	elsewhere := false
	if tg != nil && ev.Edit != nil && ev.Edit.Namespace != "" {
		// Read back where it was sent, and nowhere else: a restart that
		// points the flag elsewhere leaves it applying.
		if elsewhere = tg.owns(p.Source, *ev.Edit) != nil; !elsewhere {
			u, err = tg.readBack(ctx, p, *ev.Edit)
		}
	}
	where := "the API"
	if tg != nil {
		where = tg.where()
	}
	still := func(why string) error {
		if sent != nil {
			why += fmt.Sprintf("; %s answered: %v", where, sent)
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
	if elsewhere {
		return still(tg.notThere())
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		s.log.Warn("a change sent through an API isn't read back yet", "proposal", p.ID, "err", err)
		return still(fmt.Sprintf("sent to %s, but not read back yet: it is read again each round, and never sent again", where))
	}
	if why, failed := tg.settling(u); why != "" {
		return still(why)
	} else if failed != "" {
		return fail(failed)
	}
	// As it was: the unit as kept before it was sent; for a deletion
	// undone, gone still.
	asWas := tg.same(u, []byte(p.Backup)) || (p.Kind == ledger.KindUndo && p.BeforeHash == "" && u.Gone)
	switch {
	case tg.same(u, []byte(p.Document)):
	case asWas && sent == nil && within(s.now(), p.Applied, tg):
		// What may give the unit as it was for a while after it took the
		// change is read again, each round, until its lag is up. A round
		// can come at once (a change approved pokes one), so this is a
		// length of time.
		_, why := tg.lag()
		return still(why)
	case asWas:
		why := fmt.Sprintf("%s answered that it took the change, but it reads back as it was", where)
		if sent != nil {
			why = sent.Error()
		}
		return fail(why)
	default:
		why := fmt.Sprintf("%s reads as neither it was nor as proposed: it may have been written to meanwhile, and what it was is kept on the proposal", where)
		if sent != nil {
			why += fmt.Sprintf("; %s answered: %v", where, sent)
		}
		return fail(why)
	}
	step := ledger.Step{}
	if rs, err := tg.rules(u); err == nil && !u.Gone {
		for _, x := range rs {
			if x.Name == ev.Edit.Name && x.Group == ev.Edit.Group {
				step.AfterHash = hashOf(p, x)
			}
		}
	}
	switch {
	case sent != nil:
		step.Reason = clip(fmt.Sprintf("%s answered with an error, but it reads back changed: %v", where, sent), 500)
	case p.Reason == readBehind || p.Reason == asWasYet:
		step.Reason = "read back applied, after it read as it was at first"
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

// within reports whether a change sent at applied is within its target's
// lag at now.
func within(now, applied time.Time, tg target) bool {
	d, _ := tg.lag()
	return d > 0 && now.Sub(applied) < d
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
		want.Says = saysFor(now, e.Annotation, e.Value, e.Unset)
	}
	return want, b, v, true
}

// ---- A managed Prometheus ruler ----

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
	noPathUnread    = "the ruler gives its group in a form that can't be read back as written: change it in the ruler"
)

// noPaths are every reason a rule read from a ruler, or a cloud's alarm,
// is for a person.
var noPaths = append([]string{noPathIn, noPathInAzure, noPathName, noPathCFN, noPathManaged, noPathLastNS, noPathLastGroup, noPathEdit, noPathUnread},
	alarmNoPaths...)

// rulerTarget is --ruler's.
type rulerTarget struct{ s *Server }

func (t rulerTarget) path() string     { return ledger.PathRuler }
func (t rulerTarget) where() string    { return "the ruler" }
func (t rulerTarget) gone() string     { return "the rule's group is gone from the ruler" }
func (t rulerTarget) cantEdit() string { return noPathEdit }
func (t rulerTarget) goneUndo() string {
	return "the namespace or rule group is gone from the ruler: undo it there"
}
func (t rulerTarget) again() string {
	return "a rule of its name is in its group again: undo it in the ruler"
}
func (t rulerTarget) backup(u apiUnit) []byte { return u.Raw }
func (t rulerTarget) notThere() string        { return notThereRuler }

// goneAllowed: a Mimir's or a Cortex's group is asked of by its namespace,
// as apiApply asked already.
func (t rulerTarget) goneAllowed(ledger.Proposal, rules.Edit) string { return "" }

// lag: a Mimir or a Cortex caching its rule storage may give a group as it
// was for a while after it took a change.
func (t rulerTarget) lag() (time.Duration, string) {
	if t.s.ruler.Kind == source.RulerConfig {
		return readBackWithin, asWasYet
	}
	return 0, ""
}

// oneUnit: the same group (Mimir, Cortex), namespace (Amazon), rule group
// (Azure).
func (t rulerTarget) oneUnit(q ledger.Proposal, qe rules.Edit, p ledger.Proposal, e rules.Edit) bool {
	return strings.EqualFold(q.Source, p.Source) && (t.s.ruler.Kind == source.RulerAmazon || qe.Group == e.Group)
}

func (t rulerTarget) leftAsChanged(orig ledger.Proposal, u apiUnit, _ ledger.Proposal, e rules.Edit) bool {
	left, ok1 := rules.Entry([]byte(orig.Document), e)
	now, ok2 := rules.Entry(u.Raw, e)
	return ok1 && ok2 && reflect.DeepEqual(left, now)
}

func (t rulerTarget) onlyRule(u apiUnit, out []byte, _ ledger.Proposal, e rules.Edit) bool {
	return rules.Adds(u.Raw, out, e)
}

// unitName is a unit's name in a diff: the namespace and group, an Amazon
// namespace, the resource group and rule group.
func (t rulerTarget) unitName(_ string, e rules.Edit) string {
	if t.s.ruler.Kind == source.RulerAmazon {
		return e.Namespace
	}
	return e.Namespace + "/" + e.Group
}

// changesIn reports whether --changes-in names a namespace (Mimir, Cortex,
// Amazon) or a rule group (Azure: RG/NAME, or NAME when --ruler names one
// resource group, in any letter case, as Azure compares names). A name
// ending in * is a prefix. An entry naming an --alarms source is that
// source's.
func (s *Server) changesIn(ns, group string) bool {
	for _, pat := range s.cfg.ChangesIn {
		if s.alarmsEntry(pat) {
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

// namedBy reports whether a pattern of --changes-in names a name: the name,
// or a prefix ending in *; fold compares in any letter case.
func namedBy(pat, name string, fold bool) bool {
	if fold {
		pat, name = strings.ToLower(pat), strings.ToLower(name)
	}
	if p, ok := strings.CutSuffix(pat, "*"); ok {
		return strings.HasPrefix(name, p)
	}
	return pat == name
}

// allowed is why Lapilli may not change a rule in a namespace and a group,
// "" when it may: --changes-in names it, and its names can be written to.
func (t rulerTarget) allowed(_ string, e rules.Edit) string {
	s := t.s
	switch {
	case s.ruler.NameOK(e.Namespace, true) != nil || s.ruler.NameOK(e.Group, false) != nil:
		return noPathName
	case !s.changesIn(e.Namespace, e.Group) && s.ruler.Kind == source.RulerAzure:
		return noPathInAzure
	case !s.changesIn(e.Namespace, e.Group):
		return noPathIn
	}
	return ""
}

func (t rulerTarget) owns(src string, e rules.Edit) error {
	return t.s.ruler.Owns(src, e.Namespace, e.Group)
}

func (t rulerTarget) unit(ctx context.Context, _ string, e rules.Edit) (apiUnit, error) {
	u, err := t.s.ruler.Unit(ctx, e.Namespace, e.Group)
	return apiUnit{Raw: u.Raw, Hash: u.Hash, Gone: u.Gone, ruler: u, ns: e.Namespace}, err
}

// managed is why another tool keeps a unit, as its tags say, "" when
// nothing says so: CloudFormation's own tag on an Amazon namespace, and
// those --changes-managed names. A Mimir's or a Cortex's group carries
// nothing that could say: --changes-in alone decides.
func (t rulerTarget) managed(u apiUnit) string {
	if t.s.ruler.Kind == source.RulerAmazon {
		for k := range u.ruler.Tags {
			if strings.EqualFold(k, "aws:cloudformation:stack-id") {
				return noPathCFN
			}
		}
	}
	if managedBy(t.s.cfg.ChangesManaged, u.ruler.Tags) {
		return noPathManaged
	}
	return ""
}

// managedBy reports whether tags carry one --changes-managed names.
func managedBy(managed []string, tags map[string]string) bool {
	for _, m := range managed {
		key, val, _ := strings.Cut(m, "=")
		for k, v := range tags {
			if strings.EqualFold(k, key) && (val == "*" || strings.EqualFold(v, val)) {
				return true
			}
		}
	}
	return false
}

// ready says whether an Amazon namespace may be written now: ACTIVE. One
// being made, updated or deleted waits (errWaits); one that failed is
// refused, with Amazon's reason.
func (t rulerTarget) ready(_ context.Context, u apiUnit, _ ledger.Proposal) error {
	if t.s.ruler.Kind != source.RulerAmazon || u.Gone {
		return nil
	}
	switch u.ruler.Status {
	case "ACTIVE":
		return nil
	case "CREATING", "UPDATING", "DELETING":
		return errWaits
	case "CREATION_FAILED", "UPDATE_FAILED":
		return refusal{clip(fmt.Sprintf("the namespace is %s in Amazon: %s", u.ruler.Status, u.ruler.StatusReason), 500)}
	}
	return refusal{clip(fmt.Sprintf("the namespace's status in Amazon is %q, not ACTIVE", u.ruler.Status), 200)}
}

func (t rulerTarget) offered(context.Context, apiUnit, rules.Rule, string) (string, error) {
	return "", nil
}

// rules are the rules in a unit, each with its namespace, as Read reads
// them.
func (t rulerTarget) rules(u apiUnit) ([]rules.Rule, error) {
	if len(u.Raw) == 0 {
		return nil, nil
	}
	return rules.FromDocs([]rules.Doc{{Source: u.ruler.Source, Namespace: u.ruler.Namespace, Data: u.Raw}})
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

// edit is a unit with the change made in it: by lines in a rule file (a
// Mimir or Cortex group, an Amazon namespace), in the tree in an Azure
// rule group. A group goes with its last rule, as in a file; an Amazon
// namespace or an Azure rule group never does. why says why it can't be
// made, when it can't.
func (t rulerTarget) edit(u apiUnit, r rules.Rule, e rules.Edit, kind string, b rules.Bound, v float64) (out []byte, why string) {
	s := t.s
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

// reads reports whether a unit edited reads as the rules before with want
// in place of r, as readsAs and readsSaying check a file, and gives the
// rule as read, with its namespace.
func (t rulerTarget) reads(before, after []byte, r, want rules.Rule, kind string) (*rules.Rule, bool) {
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

func (t rulerTarget) putBack(u apiUnit, p ledger.Proposal, e rules.Edit) ([]byte, error) {
	return rules.PutBack(u.Raw, []byte(p.Backup), e)
}

// undoGone: a Mimir or Cortex group gone is a file of none, which a rule
// is put back into; an Amazon namespace or an Azure rule group gone is for
// a person.
func (t rulerTarget) undoGone(apiUnit, ledger.Proposal) bool {
	return t.s.ruler.Kind == source.RulerConfig
}

func (t rulerTarget) request(_ context.Context, p ledger.Proposal, u apiUnit, doc []byte) (func(context.Context, ledger.Proposal) error, error) {
	s := t.s
	method, at, ctype := http.MethodPut, u.ruler.At, "application/json"
	var body []byte
	var err error
	switch s.ruler.Kind {
	case source.RulerConfig:
		var ok bool
		body, ok, err = source.GroupOf(doc)
		if err != nil {
			return nil, refusal{"the group can't be written out as it reads: " + err.Error()}
		}
		if ok {
			method, at, ctype = http.MethodPost, s.ruler.WriteAt(u.ruler), "application/yaml"
		} else {
			method, body = http.MethodDelete, nil // the group went with its last rule
		}
	case source.RulerAzure:
		if body, err = source.AzureBody(doc); err != nil {
			return nil, refusal{err.Error()}
		}
	}
	return func(ctx context.Context, p ledger.Proposal) error {
		if s.ruler.Kind == source.RulerAmazon {
			// The SDK's way: a request repeated after a timeout isn't
			// taken twice. Lapilli never sends a change again.
			body, _ = json.Marshal(map[string]string{"data": base64.StdEncoding.EncodeToString(doc),
				"clientToken": fmt.Sprintf("lapilli-%d-%d", p.ID, p.Applied.Unix())})
		}
		code, out, err := s.ruler.Call(ctx, method, at, body, ctype, s.rulerWrites, source.UnitLimit)
		switch {
		case err != nil:
			return err
		case code/100 != 2:
			return source.Failed(code, out)
		}
		return nil
	}, nil
}

func (t rulerTarget) readBack(ctx context.Context, p ledger.Proposal, e rules.Edit) (apiUnit, error) {
	return t.unit(ctx, p.Source, e)
}

// settling says an Amazon namespace being updated is still being written,
// and one that failed, failed.
func (t rulerTarget) settling(u apiUnit) (string, string) {
	if t.s.ruler.Kind != source.RulerAmazon || u.Gone {
		return "", ""
	}
	switch u.ruler.Status {
	case "CREATING", "UPDATING":
		return "sent to Amazon, which is updating the namespace: it is read again each round until it is ACTIVE, and never sent again", ""
	case "UPDATE_FAILED", "CREATION_FAILED":
		return "", fmt.Sprintf("Amazon couldn't apply it: the namespace is %s: %s", u.ruler.Status, u.ruler.StatusReason)
	}
	return "", ""
}

// same reports whether a unit read back holds what raw holds: the same
// alert rules, each as it fires and says, and the rest alike, however the
// ruler writes it out. A gone Mimir or Cortex group is a file of none; a
// gone Amazon namespace or Azure rule group is nothing written.
func (t rulerTarget) same(u apiUnit, raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	if u.Gone && t.s.ruler.Kind != source.RulerConfig {
		return false
	}
	a, err1 := rules.Shape(u.Raw, source.AzureSets)
	b, err2 := rules.Shape(raw, source.AzureSets)
	return err1 == nil && err2 == nil && a == b
}
