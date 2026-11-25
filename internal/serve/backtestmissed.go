package serve

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// holdMissed holds a backtest's change to the incidents, as lapilli
// verdict --fail-missed does (docs/design/backtests.md, "Held to the
// incidents"): the incidents a rule that differs caught before the change
// that no rule catches after it. The rules that differ are judged by the
// backtest; the rules the change leaves alone by serve's record of them,
// the catches the ledger keeps of each incident, never replayed. A catch
// in the record was made by the rule as it was written then, and lapilli
// verdict replays a rule left alone as it is now: so an incident is still
// caught only when the ledger says it was caught by a rule the change
// doesn't name that, in every version of serve's rules over the
// incident's time, is the rule it is after the change, as parsed
// (aloneTextOf): the pull request's own, of a rule it holds alike on both
// sides; serve's latest, of one serve runs now that the pull request
// doesn't hold. It is not known, not missed, when:
//
//   - such a rule over the incident's time was another, or isn't in the
//     record (a version before the ledger's first, one of two rules of a
//     name);
//   - a rule that caught it has a name told apart from one the pull
//     request holds ("Errors [shop]" beside "Errors"), which may be the
//     rule the change changes, or the name of one it changes and one it
//     leaves alone, alike in group and labels, which the ledger counts as
//     one; or it is a rule whose firings serve copies in, which no
//     version holds;
//   - a rule the pull request leaves alone may have caught it unrecorded:
//     serve's runs didn't cover its time (before the first, past the
//     cursor), a version of serve's rules over it doesn't hold the rule as
//     the pull request has it, or serve couldn't replay it, the last time
//     or in a run meeting its time;
//   - a rule after the change couldn't be replayed, as in lapilli verdict.
//
// A rule that caught it which serve runs no longer, and the pull request
// doesn't hold, catches nothing after the change. Every read is of the
// ledger, once a backtest; but where the record can't settle the rules
// left alone, they are replayed, over the incidents that hang on them alone
// (replayLeftAlone, in the lane, before holdMissed says what it found).
func (s *Server) holdMissed(cmp *verdict.Comparison, held *leftAloneHeld) {
	if held == nil {
		// Nothing replayed: no incident caught before is missed after.
		cmp.HoldMissedBy(nil)
		return
	}
	cmp.HoldMissedBy(held.catches)
	if cmp.Summary != nil {
		cmp.Summary.MissedLimited = held.limited
	}
}

// leftAloneVersions is the most versions of the rules a backtest held to
// the incidents reads over its period: past it, the record of the rules
// left alone isn't read, and every incident that hangs on it is not known.
const leftAloneVersions = 200

// leftAloneReach is the furthest back, before the period's first catch
// window, a rule is looked at: a rule whose for reaches further is not
// known.
const leftAloneReach = 14 * 24 * time.Hour

// leftAlone is what serve's record holds of the rules a backtest's change
// leaves alone, over its period.
type leftAlone struct {
	o verdict.Options
	// by are the ledger's catches of each incident in the period, by its
	// id and start.
	by map[caughtAt][]verdict.Catch
	// changed, same: the names of the rules the change takes away or
	// brings, and those it leaves alike; bases, the names of all of them
	// before what told them apart; copied, those of the rules whose
	// firings serve copies in, which no version of its rules holds now.
	changed, same, bases, copied map[string]bool
	// want is each rule a catch may be counted by as it is after the
	// change: the pull request's, of one it leaves alike; serve's latest,
	// of one serve runs now that the pull request doesn't hold.
	want map[string]aloneText
	// versions are the versions of serve's rules over the period, the
	// earliest first, each with the rules a catch may be counted by, and
	// whether it holds every rule the pull request leaves alike as the
	// pull request has it (sameAll); sameReach is the longest
	// of those rules' reach. read says they were read, fewer than
	// leftAloneVersions.
	versions  []aloneVersion
	sameReach time.Duration
	read      bool
	// unreplayable says a rule the pull request leaves alone is one serve
	// couldn't replay the last time it did: its record holds nothing of it
	// from then.
	unreplayable bool
	// runsFrom and cursor are where serve's runs began and have got to;
	// failed, the spans of the runs that couldn't replay a rule the pull
	// request leaves alone.
	runsFrom, cursor time.Time
	failed           [][2]time.Time
	// alone are the rules a replay of what the record can't settle asks
	// (replayLeftAlone): the pull request's rules left alike, as it has
	// them, and serve's latest of the rules it runs that the pull request
	// doesn't hold at all.
	alone []aloneRule
	// bad are the names of the rules left alike, and of those of serve's
	// own that a replay asks (widened), that serve couldn't replay the last
	// time it did.
	bad map[string]bool
	// What widen takes the record on by, kept from leftAloneRecord: the
	// period, the rules serve runs that the pull request doesn't hold, not
	// yet hashed but for those that caught an incident the change loses
	// (lostBy, running), the rules as hashed (texts, prText), and the reach
	// the versions are read by. widened says widen was done: the versions
	// hold every rule a replay asks, failedServe the runs that couldn't
	// replay one of serve's own.
	from, to    time.Time
	serve       []rules.Rule
	running     map[string]aloneText
	lostBy      map[string]bool
	caughtBy    map[string]bool
	texts       aloneTexts
	prText      map[string]aloneText
	reach       time.Duration
	widened     bool
	failedServe [][2]time.Time
}

// aloneRule is a rule a replay of the rules left alone asks. pr says it is
// the pull request's, whose replay must answer for an incident to be
// settled; of serve's other rules a replay only keeps an incident caught.
type aloneRule struct {
	rule  rules.Rule
	reach time.Duration
	pr    bool
	// refused says it failed a check the port makes of a rule that differs
	// (vet): it is not replayed.
	refused bool
}

type caughtAt struct {
	id    string
	start int64
}

// aloneText is a rule as it decides when it fires, by a hash: of it as
// parsed (rules.Rule.Parsed), so that a rule read from a file and the same
// rule read from an API that prints its expression again (Prometheus's
// rules API, Mimir's) are one; or, of a rule whose expression isn't read so
// (a Grafana rule, a Loki ruler's, one Parsed refuses), of its text
// (rules.Rule.Whole), and one read otherwise is another. reach is how long
// before a firing's start its condition is read (its for and its group's
// interval, and its keep_firing_for, which holds a firing on through gaps);
// ambiguous, of two rules of a name that differ.
type aloneText struct {
	sum       [sha256.Size]byte
	reach     time.Duration
	ambiguous bool
}

// aloneTextOf is r as aloneText has it. A parsed rule's hash is never a
// text's: Parsed begins otherwise than any Whole does.
func aloneTextOf(r rules.Rule) aloneText {
	text, ok := r.Parsed()
	if !ok {
		text = r.Whole()
	}
	return aloneText{sum: sha256.Sum256([]byte(text)), reach: r.For + r.KeepFiringFor + max(r.Interval, time.Minute)}
}

// aloneTexts are the rules read, each as aloneText has it, by what it is
// worked out from: a rule the same in every version is parsed once.
type aloneTexts map[[sha256.Size]byte]aloneText

func (m aloneTexts) of(r rules.Rule) aloneText {
	k := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%q\x00%t", r.Whole(), r.Cloud, r.Graph != nil)))
	t, ok := m[k]
	if !ok {
		t = aloneTextOf(r)
		m[k] = t
	}
	return t
}

// addAloneText keeps t, a rule's, under its name in m: a second rule of the
// name, written otherwise, makes it ambiguous.
func addAloneText(m map[string]aloneText, name string, t aloneText) {
	if was, ok := m[name]; ok && (was.sum != t.sum || was.ambiguous) {
		t.ambiguous = true
		t.reach = max(t.reach, was.reach)
	}
	m[name] = t
}

type aloneVersion struct {
	since, until time.Time
	texts        map[string]aloneText
	sameAll      bool
}

// leftAloneRecord reads what leftAlone holds: the incidents of [from, to),
// the rules, the versions of them over the period and the runs, each once,
// for the incidents lost, the ones the rules that differ caught before the
// change and none catches after it. Of serve's own rules the pull request
// doesn't hold it hashes only those that caught one of them: widen takes
// the rest when a replay needs them.
func (s *Server) leftAloneRecord(ctx context.Context, pr *prepared, from, to time.Time, o verdict.Options, lost []incident.Incident) (*leftAlone, error) {
	a := &leftAlone{o: o, by: map[caughtAt][]verdict.Catch{}, changed: map[string]bool{}, same: map[string]bool{}, bases: map[string]bool{},
		copied: map[string]bool{}, want: map[string]aloneText{}, from: from, to: to, lostBy: map[string]bool{}, caughtBy: map[string]bool{}}
	for _, r := range slices.Concat(pr.gone, pr.came) {
		a.changed[r.Name], a.bases[nameBase(r.Name)] = true, true
	}
	// The latest version: the rules serve runs now.
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return nil, err
	}
	var now []rules.Rule
	if ok {
		if now, err = s.parse(ctx, latest); err != nil {
			return nil, err
		}
	}
	// The pull request's rules left alike, as serve replays its own: so
	// that a rule read from a file and serve's of the same file are one.
	// Of a rule serve reads, every one of its name, from the rules API of
	// the Prometheus it reads them from (--loaded-rules), a group of the
	// file with no interval of its own runs there at its
	// evaluation_interval, which the API says of serve's, and the file
	// doesn't: the rule is taken at it, once serve knows it, and where
	// serve's latest of it runs at it too. Serve reads the configuration
	// once a day and the rules every look: an interval changed since is
	// another, and the rule is as its file has it.
	same := slices.Clone(pr.same)
	s.replayedAs().Apply(same)
	if every := s.evaluationInterval(); every > 0 {
		there := map[string]bool{}
		for _, r := range now {
			there[r.Name] = there[r.Name] || !loadedFrom(r) || r.Interval != every
		}
		for i, r := range same {
			if bad, ok := there[r.Name]; ok && !bad && r.Interval == 0 {
				same[i].Interval = every
			}
		}
	}
	a.texts = aloneTexts{}
	a.prText = map[string]aloneText{}
	for _, r := range same {
		a.same[r.Name], a.bases[nameBase(r.Name)] = true, true
		t := a.texts.of(r)
		addAloneText(a.prText, r.Name, t)
		a.sameReach = max(a.sameReach, t.reach)
	}
	incs, err := s.l.Incidents(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	inLost := map[caughtAt]bool{}
	for _, in := range lost {
		inLost[caughtAt{in.ID, in.Start.Unix()}] = true
	}
	for _, in := range incs {
		k := caughtAt{in.ID, in.Start.Unix()}
		a.by[k] = in.CaughtBy
		for _, c := range in.CaughtBy {
			a.caughtBy[c.Rule] = true
			if inLost[k] {
				a.lostBy[c.Rule] = true
			}
		}
	}
	// Serve's own rules the pull request doesn't hold at all, by name: a
	// name told apart from one the pull request holds may be that rule.
	// Those that caught an incident the change loses are held to the
	// record (held) as they are; the rest are taken by widen, where a
	// replay needs them.
	a.running = map[string]aloneText{}
	for _, r := range now {
		if !a.same[r.Name] && !a.changed[r.Name] && !a.bases[nameBase(r.Name)] {
			a.serve = append(a.serve, r)
			if a.lostBy[r.Name] {
				addAloneText(a.running, r.Name, a.texts.of(r))
			}
		}
	}
	a.reach = a.sameReach
	for name, t := range a.running {
		a.want[name] = t
		a.reach = max(a.reach, t.reach)
	}
	for _, r := range same {
		a.alone = append(a.alone, aloneRule{rule: r, reach: a.texts.of(r).reach, pr: true})
	}
	for name := range a.same {
		a.want[name] = a.prText[name]
	}
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		return nil, err
	}
	inNow := make(map[string]bool, len(now))
	for _, r := range now {
		inNow[r.Name] = true
	}
	for _, r := range table {
		if r.Source != "" && !inNow[r.Name] {
			a.copied[r.Name] = true
		}
		// A rule that fires nothing (disabled, paused, no query) is no rule
		// serve couldn't replay.
		if a.same[r.Name] && r.Unsupported != "" && !rules.CantFire(r.Unsupported) {
			a.unreplayable = true
			if a.bad == nil {
				a.bad = map[string]bool{}
			}
			a.bad[r.Name] = true
		}
	}
	if a.runsFrom, err = s.l.RunsFrom(ctx); err != nil {
		return nil, err
	}
	if a.cursor, err = s.l.Cursor(ctx); err != nil {
		return nil, err
	}
	if err := s.readVersions(ctx, a); err != nil {
		return nil, err
	}
	if len(a.same) > 0 && !a.cursor.IsZero() {
		if a.failed, err = s.l.RunsFailing(ctx, from.Add(-o.MaxLead()), a.cursor.Add(time.Second), a.same); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// readVersions reads every version over the period's catch windows, each
// rule's reach before them, to now, as each rule a catch may be counted by
// is in it (want, a.same).
func (s *Server) readVersions(ctx context.Context, a *leftAlone) error {
	// What was read before stays unless this read is done: widen reads again
	// with a longer reach, and a read that fails must not take away what the
	// first one gave.
	first := a.from.Add(-a.o.MaxLead() - min(a.reach, leftAloneReach))
	vs, err := s.l.Versions(ctx, first, s.now(), false)
	if err != nil {
		return err
	}
	if len(vs) == 0 || len(vs) > leftAloneVersions {
		a.versions, a.read = nil, false
		return nil
	}
	var out []aloneVersion
	for i, v := range vs {
		rs, err := s.parse(ctx, v)
		if err != nil {
			return err
		}
		vt := aloneVersion{since: v.Since, until: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), texts: map[string]aloneText{}}
		if i+1 < len(vs) {
			vt.until = vs[i+1].Since
		}
		all := map[string]aloneText{}
		for _, r := range rs {
			if a.want[r.Name] != (aloneText{}) || a.same[r.Name] {
				addAloneText(all, r.Name, a.texts.of(r))
			}
		}
		vt.sameAll = true
		for name := range a.same {
			if t, ok := all[name]; !ok || t.ambiguous || t.sum != a.prText[name].sum || a.prText[name].ambiguous {
				vt.sameAll = false
				break
			}
		}
		for name := range a.want {
			if t, ok := all[name]; ok {
				vt.texts[name] = t
			}
		}
		out = append(out, vt)
	}
	a.versions, a.read = out, true
	return nil
}

// widen takes the record on to every rule of serve's own the pull request
// doesn't hold, which a replay asks unless the record holds it: hashed, the
// versions read again for them, and the runs that couldn't replay one. It
// is done once, and only where an incident is open for a replay.
func (s *Server) widen(ctx context.Context, a *leftAlone) error {
	if a.widened {
		return nil
	}
	a.widened = true
	names := map[string]bool{}
	for _, r := range a.serve {
		t := a.texts.of(r)
		addAloneText(a.running, r.Name, t)
		a.alone = append(a.alone, aloneRule{rule: r, reach: t.reach})
		names[r.Name] = true
		if a.caughtBy[r.Name] {
			a.reach = max(a.reach, t.reach)
		}
	}
	for name, t := range a.running {
		a.want[name] = t
	}
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		return err
	}
	for _, r := range table {
		if names[r.Name] && r.Unsupported != "" && !rules.CantFire(r.Unsupported) {
			if a.bad == nil {
				a.bad = map[string]bool{}
			}
			a.bad[r.Name] = true
		}
	}
	if err := s.readVersions(ctx, a); err != nil {
		return err
	}
	if len(names) > 0 && !a.cursor.IsZero() {
		if a.failedServe, err = s.l.RunsFailing(ctx, a.from.Add(-a.o.MaxLead()), a.cursor.Add(time.Second), names); err != nil {
			return err
		}
	}
	return nil
}

// loadedFrom says serve read r from a Prometheus's rules API, as
// --loaded-rules reads one: not vmalert's, whose groups run at its own
// -evaluationInterval; not a Loki ruler's, a Grafana rule, nor a cloud's
// alarm.
func loadedFrom(r rules.Rule) bool {
	return r.Source == "api" && r.Prometheus && !r.Vmalert && r.Logs == "" && r.Graph == nil && r.Cloud == ""
}

// catches says whether, by serve's record, a rule the change leaves alone
// caught in, and whether the record says so at all (verdict.LeftAlone).
func (a *leftAlone) catches(in incident.Incident) (caught, known bool) {
	caught, known, _ = a.judge(in)
	return caught, known
}

// replayNeed says of an incident the record leaves not known whether a
// replay of the rules left alone may settle it: hard is false only where it
// may, and an incident with it set can only be found caught, never missed
// (a rule whose name is told apart from the changed rule's, a rule whose
// firings are copied in, or one name changed and left alone). names are the
// rules of serve's own that caught it in the record, as they are no longer:
// the replay of them must answer too.
type replayNeed struct {
	hard  bool
	names map[string]bool
}

// judge is catches, and, where the record leaves it not known, what a
// replay of the rules left alone would need to settle.
func (a *leftAlone) judge(in incident.Incident) (caught, known bool, need replayNeed) {
	known = true
	for _, k := range a.by[caughtAt{in.ID, in.Start.Unix()}] {
		switch {
		case a.changed[k.Rule] && a.same[k.Rule]:
			// Two rules of one name, group and labels, one changed and one
			// left alone, which the ledger counts as one: either caught it.
			known, need.hard = false, true
		case a.changed[k.Rule]:
			// The rule as it was, which the backtest judged.
		case a.same[k.Rule]:
			// Caught by the rule as the pull request has it, or not known.
			if a.held(k.Rule, in) {
				return true, true, replayNeed{}
			}
			known = false
		case a.bases[nameBase(k.Rule)]:
			// Told apart from a rule the pull request holds, or one it is
			// told apart from: maybe the rule the change changes.
			known, need.hard = false, true
		case a.want[k.Rule] != (aloneText{}):
			// Serve runs it now: caught by the rule as it is now, or not
			// known.
			if a.held(k.Rule, in) {
				return true, true, replayNeed{}
			}
			known = false
			if need.names == nil {
				need.names = map[string]bool{}
			}
			need.names[k.Rule] = true
		case a.copied[k.Rule]:
			// Its firings copied in: no version says what it was.
			known, need.hard = false, true
		default:
			// A rule serve runs no longer, which the pull request doesn't
			// hold: it catches nothing after the change.
		}
	}
	if !known {
		return false, false, need
	}
	if len(a.same) == 0 {
		// No rule the pull request leaves alone: serve's own rules beside
		// it, which lapilli verdict wouldn't know of, can only keep an
		// incident caught, never leave it not known.
		return false, true, replayNeed{}
	}
	if !a.covers(in) {
		return false, false, need
	}
	return false, true, replayNeed{}
}

// covers says serve's record settles what the rules the pull request leaves
// alone did over in's catch window: the runs covered it, none failed there,
// and none of those rules couldn't be replayed the last time, in every
// version of serve's rules over it as the pull request has them.
func (a *leftAlone) covers(in incident.Incident) bool {
	// The catch window, as the ledger works it out (its window): from the
	// longest lead before the start to the end, or the start and
	// --unknown-length.
	from, to := in.Start.Add(-a.o.MaxLead()), in.EndOr(a.o.UnknownLen)
	if a.unreplayable || a.runsFrom.IsZero() || a.runsFrom.After(from) || a.cursor.Before(to) {
		return false
	}
	if !a.over(from.Add(-a.sameReach), to, func(v aloneVersion) bool { return v.sameAll }) {
		return false
	}
	meets := func(span [2]time.Time) bool { return !span[0].After(to) && span[1].After(from) }
	return !slices.ContainsFunc(a.failed, meets)
}

// held reports whether the rule of that name was the rule it is after the
// change (want), as parsed, in every version of serve's rules over in's
// catch window, back as far as its condition is read before a firing's
// start.
func (a *leftAlone) held(name string, in incident.Incident) bool {
	w := a.want[name]
	if w.ambiguous {
		return false
	}
	return a.over(in.Start.Add(-a.o.MaxLead()-w.reach), in.EndOr(a.o.UnknownLen), func(v aloneVersion) bool {
		t, ok := v.texts[name]
		return ok && !t.ambiguous && t.sum == w.sum
	})
}

// over reports whether every version of serve's rules in effect at some
// time in [from, to] is as ok says, the versions read reaching back to
// from: never when they weren't read, or begin after it.
func (a *leftAlone) over(from, to time.Time, ok func(aloneVersion) bool) bool {
	if !a.read || len(a.versions) == 0 || a.versions[0].since.After(from) {
		return false
	}
	for _, v := range a.versions {
		if v.since.After(to) || !v.until.After(from) {
			continue // not in effect over it
		}
		if !ok(v) {
			return false
		}
	}
	return true
}

// nameBase is a rule's name before what told it apart from another of the
// same name (source.TellApart: "Errors [shop]", "Errors [severity=page]").
func nameBase(name string) string {
	if i := strings.Index(name, " ["); i > 0 && strings.HasSuffix(name, "]") {
		return name[:i]
	}
	return name
}
