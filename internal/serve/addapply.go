package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// Carrying out a proposal adding a rule, once approved and checked again
// (checkAddAgain), on each path that writes through an API: Grafana's
// provisioning API makes it in its folder and group; a ruler's group is
// written with it, as rulers.md writes a group, only while the group is
// as it was read; a cloud's alarm is made as undoing a deletion makes
// one. Each is read back, as every change is. Undone, the rule is deleted
// the way each path deletes one, only while it is as the add left it.

// undoesAdd reports whether an undo undoes a rule added.
func undoesAdd(ctx context.Context, s *Server, p ledger.Proposal) bool {
	if p.Kind != ledger.KindUndo || p.Follows == 0 {
		return false
	}
	orig, err := s.l.ProposalByID(ctx, p.Follows)
	return err == nil && orig.Kind == ledger.KindAdd
}

// grafanaAdd makes a rule added in Grafana: in the folder and group a rule
// of that group is in now (its folder's uid and group's name read from
// it, kept by no other tool), the data source and contact point checked
// again against the folder's rules read just before, no rule of its uid
// or its title there, as Lapilli wrote it (grafanaAdded), with
// X-Disable-Provenance, so that it stays editable in Grafana's UI. Read
// back by its uid.
func (s *Server) grafanaAdd(ctx context.Context, p ledger.Proposal, ev evidence, step ledger.Step) error {
	a, err := addedOf(ev)
	if err != nil {
		return err
	}
	place := ev.Add.Place
	if _, _, gone, err := s.grafanaRule(ctx, ev.Edit.UID); err != nil {
		return err
	} else if !gone {
		return refusal{"a rule of its uid is in Grafana already"}
	}
	rs, _, err := s.readRules(ctx)
	if err != nil {
		return err
	}
	var sibling string
	var srcs, tells []string
	routed := false
	for _, r := range rs {
		if r.UID == "" || r.Folder != place.Folder {
			continue
		}
		if r.Name == a.Alert {
			return refusal{"a rule of its title is in its folder now: propose it again under another name"}
		}
		if r.Group != place.Folder+"/"+place.Group {
			continue
		}
		if sibling == "" {
			sibling = r.UID
		}
		if r.Graph != nil {
			for _, d := range r.Graph.Data {
				srcs = append(srcs, d.UID)
			}
		}
		if r.Receiver != "" {
			tells = append(tells, r.Receiver)
		} else {
			routed = true
		}
	}
	switch {
	case sibling == "":
		return refusal{"its group is gone from Grafana: propose it again"}
	case !slices.Contains(srcs, place.DataSource):
		return refusal{"the rules of its group no longer read its data source: propose it again"}
	case grafanaRouted(place, tells, routed) != nil:
		return refusal{"every rule of its group names its contact point now: propose it again, naming one of theirs"}
	}
	for _, t := range place.Tells {
		if !slices.Contains(tells, t) {
			return refusal{"the rules of its group no longer tell whom it tells: propose it again"}
		}
	}
	raw, _, gone, err := s.grafanaRule(ctx, sibling)
	if err != nil {
		return err
	}
	if gone {
		return refusal{"its group is gone from Grafana: propose it again"}
	}
	if err := provenanceRefusal(raw); err != nil {
		return refusal{"another tool keeps its group in Grafana: add the rule there"}
	}
	folder, _ := raw["folderUID"].(string)
	group, _ := raw["ruleGroup"].(string)
	if folder == "" || group != place.Group {
		return refusal{"Grafana gives its group otherwise than it was read: propose it again"}
	}
	w := grafanaAdded(a, place, ev.Edit.UID, false)
	w["folderUID"], w["ruleGroup"] = folder, group
	if org, ok := raw["orgID"]; ok {
		w["orgID"] = org
	}
	body, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return s.grafanaWrite(ctx, p, step, ev.Edit.UID, http.MethodPost, "/api/v1/provisioning/alert-rules", body)
}

// apiAdd adds a rule through a ruler's API or a cloud's: written where it
// was proposed, only where --changes-in names it still, one change to a
// unit at a time. A ruler's group is read again and the rule added to it
// as it is now, only it added (rules.AddsRule), a group made only while
// it is still gone; a cloud's alarm is made only while no alarm of its
// name is there, telling only whom the alarms there tell now.
func (s *Server) apiAdd(ctx context.Context, tg target, p ledger.Proposal, step ledger.Step) error {
	ev := proposalEvidence(p)
	a, err := addedOf(ev)
	if err != nil {
		return err
	}
	e := *ev.Edit
	if tg.path() == ledger.PathAlarm {
		return s.alarmAdd(ctx, p, ev, a, step)
	}
	if err := tg.owns(p.Source, e); err != nil {
		return refusal{err.Error()}
	}
	if why := tg.allowed(p.Source, e); why != "" {
		return refusal{why}
	}
	if busy, err := s.unitBusy(ctx, tg, p, e); err != nil {
		return err
	} else if busy {
		return errWaits
	}
	u, err := tg.unit(ctx, p.Source, e)
	if errors.Is(err, source.ErrUnreadable) {
		return refusal{noPathUnread}
	}
	if err != nil {
		return err
	}
	if why := tg.managed(u); why != "" {
		return refusal{why}
	}
	if err := tg.ready(ctx, u, p); err != nil {
		return err
	}
	place := ev.Add.Place
	doc := []byte(p.Document)
	if u.Hash != p.DocHash {
		// The unit changed since: the rule added afresh in it as it is now.
		var out []byte
		switch {
		case s.ruler.Kind == source.RulerConfig && place.NewGroup && !u.Gone:
			return refusal{"a group of its name is in the namespace now: propose it again"}
		case s.ruler.Kind == source.RulerConfig && place.NewGroup:
			out, err = newGroupFile(place.Group, place.Interval, a)
		case u.Gone:
			return refusal{tg.gone()}
		default:
			out, err = rules.AddRule(u.Raw, place.Group, place.NewGroup, place.Interval, a)
		}
		if err != nil {
			return refusal{"the rule can't be added to the ruler's group as it is now: propose it again"}
		}
		doc = out
		step.Document, step.DocHash, step.Diff = string(out), u.Hash, rules.Diff(tg.unitName(p.Source, e), u.Raw, out)
	}
	if !rules.AddsRule(u.Raw, doc, place.Group, place.Interval, a) {
		return refusal{"the rule can't be added to the ruler's group as it is now: propose it again"}
	}
	rs, err := tg.rules(u)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(rs, func(r rules.Rule) bool { return r.Name == a.Alert }) {
		return refusal{"a rule of its name is in its group now: propose it again under another name"}
	}
	step.Backup = string(tg.backup(u))
	return s.apiWrite(ctx, tg, p, step, u, doc)
}

// alarmAdd makes a cloud's alarm added: CloudWatch's by PutMetricAlarm,
// in the account its writes are made in, which must be the one its
// alarms are read in; Google's by alertPolicies.create, refused when a
// policy of its display name is there, read back by the name it answers
// with, as undoing a deletion makes one again.
func (s *Server) alarmAdd(ctx context.Context, p ledger.Proposal, ev evidence, a rules.Added, step ledger.Step) error {
	tg := alarmTarget{s}
	al := s.alarmsOf(ev.Edit.Namespace)
	if al == nil {
		return refusal{"the proposal names a cloud's alarms, and lapilli serve no longer reads them (--alarms)"}
	}
	if al.OwnsID(p.Source) != nil {
		return refusal{"the alarm would be made in another account, region or project than --alarms names: propose it again"}
	}
	if !s.changesInAlarm(al.Source, a.Alert, al.Kind) {
		return refusal{noPathAlarmIn}
	}
	if busy, err := s.unitBusy(ctx, tg, p, *ev.Edit); err != nil {
		return err
	} else if busy {
		return errWaits
	}
	_, docs, err := al.Read(ctx)
	if err != nil {
		return err
	}
	var tells []string
	for _, d := range docs {
		tells = append(tells, alarmTells(al.Kind, d.Data)...)
		if rules.AlarmName(d.Data, cloudOf[al.Kind]) == a.Alert || d.Source == p.Source {
			return refusal{"an alarm of its name is in the cloud now: propose it again under another name"}
		}
	}
	for _, t := range ev.Add.Place.Tells {
		if !slices.Contains(tells, t) {
			return refusal{"the alarms there no longer tell whom it tells: propose it again"}
		}
	}
	u := apiUnit{Gone: true, alarm: source.AlarmUnit{ID: p.Source}, ns: al.Source}
	if al.Kind == source.AlarmsAWS {
		// Read by its name, as CloudWatch names one: not there.
		if got, err := tg.unit(ctx, p.Source, *ev.Edit); err != nil {
			return err
		} else if !got.Gone {
			return refusal{"an alarm of its name is in the cloud now: propose it again under another name"}
		}
	}
	return s.apiWrite(ctx, tg, p, step, u, []byte(p.Document))
}

// addReadBack reports whether a unit read back after a rule was added
// holds it: the rule, as it fires and says, of the name it was added
// under.
func addReadBack(tg target, u apiUnit, p ledger.Proposal) (rules.Rule, bool) {
	if u.Gone {
		return rules.Rule{}, false
	}
	ev := proposalEvidence(p)
	rs, err := tg.rules(u)
	if err != nil || ev.Add == nil {
		return rules.Rule{}, false
	}
	a, err := rules.ParseAdded([]byte(ev.Add.Rule))
	if err != nil {
		return rules.Rule{}, false
	}
	want, err := rules.FromDocs([]rules.Doc{{Source: p.Source, Namespace: u.ns, Data: []byte(p.Document)}})
	if err != nil {
		return rules.Rule{}, false
	}
	for _, r := range rs {
		if r.Name != a.Alert {
			continue
		}
		for _, w := range want {
			if w.Name == a.Alert && w.Expr == r.Expr && w.For == r.For {
				return r, true
			}
		}
	}
	return rules.Rule{}, false
}

// addedExpectations keeps, for each proposal adding a rule now applied
// that someone approved, what it was held to, where a draft accepted goes
// (step 3): accepted by its first approver, by the git path to --expect's
// file when it can go there, else the YAML on the rule's tab for a person.
// Once a proposal. One the ledger refuses to keep is marked never to be
// kept, and one that fails otherwise is left for the next round: neither
// holds up the others, nor the changes carried out after.
func (s *Server) addedExpectations(ctx context.Context) error {
	ids, err := s.l.AddsUnkept(ctx, 20)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.keepAdded(ctx, id); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var inv ledger.ErrInvalid
			never := errors.As(err, &inv) || errors.Is(err, ledger.ErrOwnDraft) || errors.Is(err, errAddedUnread)
			s.log.Warn("a rule added's expectations weren't kept", "proposal", id, "err", err, "never", never)
			if never {
				if err := s.l.SkipAddedExpect(ctx, id); err != nil {
					s.log.Warn("a rule added's expectations weren't marked never to be kept", "proposal", id, "err", err)
				}
			}
		}
	}
	return nil
}

// errAddedUnread is a proposal adding a rule whose expectations or
// evidence don't read, or that no one approved: they are never kept.
var errAddedUnread = errors.New("what the rule added is held to doesn't read, or no one approved it")

// keepAdded keeps what one proposal adding a rule, applied and approved,
// was held to (addedExpectations).
func (s *Server) keepAdded(ctx context.Context, id int64) error {
	p, err := s.l.ProposalByID(ctx, id)
	if err != nil {
		return err
	}
	if len(p.Approvals) == 0 {
		return errAddedUnread // no approver to accept them (AddsUnkept reads none such)
	}
	x, err := s.l.ProposalExpect(ctx, p.ID)
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax), errors.As(err, &typed):
		return errAddedUnread
	case err != nil:
		return err
	}
	ev := proposalEvidence(p)
	if ev.Add == nil || x.Text == "" {
		return errAddedUnread
	}
	found, _ := json.Marshal(ev.Add.Items)
	target, hash := s.draftTarget()
	d := ledger.ExpectDraft{Rule: p.Rule, MadeBy: p.MadeBy, Agent: p.Agent, Text: x.Text, Tests: x.Tests, Found: found, Target: target,
		DocHash: hash, Proposal: p.ID}
	doc, docHash := "", ""
	if s.gitPath(d) {
		text, h, whole, err := s.takenIn(d)
		if err == nil && whole {
			doc, docHash = text, h
		}
	}
	_, err = s.l.KeepAddedDraft(ctx, d, p.Approvals[0].By, doc, docHash)
	if errors.Is(err, ledger.ErrMoved) {
		return nil // kept meanwhile
	}
	return err
}

// addedYAML is what a proposal adding a rule holds it to, as a person puts
// it where the expectations are declared once it is added: the file with
// it taken in, or its entries.
func (s *Server) addedYAML(ctx context.Context, p ledger.Proposal) (string, error) {
	x, err := s.l.ProposalExpect(ctx, p.ID)
	if err != nil || x.Text == "" {
		return "", err
	}
	entries, err := verdict.DraftEntries(x.Text, p.Rule)
	if err != nil {
		return "", err
	}
	b, err := verdict.ExpectationsYAML(entries)
	return string(b), err
}
