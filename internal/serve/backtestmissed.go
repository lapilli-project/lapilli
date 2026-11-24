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
// ledger, once a backtest: nothing is asked of the history.
func (s *Server) holdMissed(ctx context.Context, cmp *verdict.Comparison, pr *prepared, from, to time.Time, o verdict.Options) error {
	if cmp.Before == nil || cmp.After == nil {
		// Nothing replayed: no incident caught before is missed after.
		cmp.HoldMissedBy(nil)
		return nil
	}
	rec, err := s.leftAloneRecord(ctx, pr, from, to, o)
	if err != nil {
		return err
	}
	cmp.HoldMissedBy(rec.catches)
	return nil
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
// interval); ambiguous, of two rules of a name that differ.
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
	return aloneText{sum: sha256.Sum256([]byte(text)), reach: r.For + max(r.Interval, time.Minute)}
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
// the rules, the versions of them over the period and the runs, each once.
func (s *Server) leftAloneRecord(ctx context.Context, pr *prepared, from, to time.Time, o verdict.Options) (*leftAlone, error) {
	a := &leftAlone{o: o, by: map[caughtAt][]verdict.Catch{}, changed: map[string]bool{}, same: map[string]bool{}, bases: map[string]bool{},
		copied: map[string]bool{}, want: map[string]aloneText{}}
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
	// doesn't: the rule is taken at it, once serve knows it.
	same := slices.Clone(pr.same)
	s.replayedAs().Apply(same)
	if every := s.evaluationInterval(); every > 0 {
		there := map[string]bool{}
		for _, r := range now {
			there[r.Name] = there[r.Name] || !loadedFrom(r)
		}
		for i, r := range same {
			if bad, ok := there[r.Name]; ok && !bad && r.Interval == 0 {
				same[i].Interval = every
			}
		}
	}
	texts := aloneTexts{}
	prText := map[string]aloneText{}
	for _, r := range same {
		a.same[r.Name], a.bases[nameBase(r.Name)] = true, true
		t := texts.of(r)
		addAloneText(prText, r.Name, t)
		a.sameReach = max(a.sameReach, t.reach)
	}
	incs, err := s.l.Incidents(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	caughtBy := map[string]bool{}
	for _, in := range incs {
		a.by[caughtAt{in.ID, in.Start.Unix()}] = in.CaughtBy
		for _, k := range in.CaughtBy {
			caughtBy[k.Rule] = true
		}
	}
	running := map[string]aloneText{}
	for _, r := range now {
		if caughtBy[r.Name] && !a.same[r.Name] && !a.changed[r.Name] {
			addAloneText(running, r.Name, texts.of(r))
		}
	}
	reach := a.sameReach
	for name, t := range running {
		a.want[name] = t
		reach = max(reach, t.reach)
	}
	for name := range a.same {
		a.want[name] = prText[name]
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
		if a.same[r.Name] && r.Unsupported != "" {
			a.unreplayable = true
		}
	}
	if a.runsFrom, err = s.l.RunsFrom(ctx); err != nil {
		return nil, err
	}
	if a.cursor, err = s.l.Cursor(ctx); err != nil {
		return nil, err
	}
	// Every version over the period's catch windows, each rule's reach
	// before them, to now.
	first := from.Add(-o.MaxLead() - min(reach, leftAloneReach))
	vs, err := s.l.Versions(ctx, first, s.now(), false)
	if err != nil {
		return nil, err
	}
	if len(vs) > 0 && len(vs) <= leftAloneVersions {
		a.read = true
		for i, v := range vs {
			rs, err := s.parse(ctx, v)
			if err != nil {
				return nil, err
			}
			vt := aloneVersion{since: v.Since, until: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), texts: map[string]aloneText{}}
			if i+1 < len(vs) {
				vt.until = vs[i+1].Since
			}
			all := map[string]aloneText{}
			for _, r := range rs {
				if a.want[r.Name] != (aloneText{}) || a.same[r.Name] {
					addAloneText(all, r.Name, texts.of(r))
				}
			}
			vt.sameAll = true
			for name := range a.same {
				if t, ok := all[name]; !ok || t.ambiguous || t.sum != prText[name].sum || prText[name].ambiguous {
					vt.sameAll = false
					break
				}
			}
			for name := range a.want {
				if t, ok := all[name]; ok {
					vt.texts[name] = t
				}
			}
			a.versions = append(a.versions, vt)
		}
	}
	if len(a.same) > 0 && !a.cursor.IsZero() {
		if a.failed, err = s.l.RunsFailing(ctx, from.Add(-o.MaxLead()), a.cursor.Add(time.Second), a.same); err != nil {
			return nil, err
		}
	}
	return a, nil
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
	known = true
	for _, k := range a.by[caughtAt{in.ID, in.Start.Unix()}] {
		switch {
		case a.changed[k.Rule] && a.same[k.Rule]:
			// Two rules of one name, group and labels, one changed and one
			// left alone, which the ledger counts as one: either caught it.
			known = false
		case a.changed[k.Rule]:
			// The rule as it was, which the backtest judged.
		case a.same[k.Rule]:
			// Caught by the rule as the pull request has it, or not known.
			if a.held(k.Rule, in) {
				return true, true
			}
			known = false
		case a.bases[nameBase(k.Rule)]:
			// Told apart from a rule the pull request holds, or one it is
			// told apart from: maybe the rule the change changes.
			known = false
		case a.want[k.Rule] != (aloneText{}):
			// Serve runs it now: caught by the rule as it is now, or not
			// known.
			if a.held(k.Rule, in) {
				return true, true
			}
			known = false
		case a.copied[k.Rule]:
			// Its firings copied in: no version says what it was.
			known = false
		default:
			// A rule serve runs no longer, which the pull request doesn't
			// hold: it catches nothing after the change.
		}
	}
	if !known {
		return false, false
	}
	if len(a.same) == 0 {
		// No rule the pull request leaves alone: serve's own rules beside
		// it, which lapilli verdict wouldn't know of, can only keep an
		// incident caught, never leave it not known.
		return false, true
	}
	// The catch window, as the ledger works it out (its window): from the
	// longest lead before the start to the end, or the start and
	// --unknown-length.
	from, to := in.Start.Add(-a.o.MaxLead()), in.EndOr(a.o.UnknownLen)
	if a.unreplayable || a.runsFrom.IsZero() || a.runsFrom.After(from) || a.cursor.Before(to) {
		return false, false
	}
	if !a.over(from.Add(-a.sameReach), to, func(v aloneVersion) bool { return v.sameAll }) {
		return false, false
	}
	meets := func(span [2]time.Time) bool { return !span[0].After(to) && span[1].After(from) }
	if slices.ContainsFunc(a.failed, meets) {
		return false, false
	}
	return false, true
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
