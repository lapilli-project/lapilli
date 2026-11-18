package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// Named impact signals on the Settings page (docs/design/signals.md, step
// 3): an admin adds a signal, changes one, renames one in place, takes one
// away, or gives one back to what serve was started with; sets the rule
// entries in place of the start's; tries a change on the last four weeks
// first; and reads a signal again alone. Each change is kept in one write
// (ledger.ChangeSignals) with the signals in effect after it, by the same
// machinery the command line's are kept by (bits, ties, judging again),
// and noted in the activity log with its before and after.

// errBesideNamed is the form of the impact signal with no name, saved
// while named signals are in effect.
var errBesideNamed = bad("named impact signals are in effect: one with no name doesn't go beside them; change them under Impact signals")

// matchLabel is a label's name as Prometheus takes one, 128 bytes at most,
// as --impact-match's are.
var matchLabel = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`)

// maxEntriesText is the most of the rule entries the Settings page takes,
// as YAML: 64 entries of a pattern of 200 bytes and 16 labels fit, written
// plainly. maxEntriesJSON is the most they are kept as (in settings, in
// impact_rules, and before and after in the activity log): what a YAML
// text of maxEntriesText can't pass with no anchor or alias, and well
// within the 1 MiB --impact-file reads, eight signals' expressions beside.
const (
	maxEntriesText = 32 << 10
	maxEntriesJSON = 128 << 10
)

// errYAMLAliases is YAML with an anchor or an alias: one value repeated
// many times over, which a text bounded in bytes would be no bound to.
var errYAMLAliases = errors.New("a YAML anchor or alias: write each value out")

// errYAMLTags is YAML with a tag other than a string's, a list's or a
// mapping's: !!binary, which gives bytes, not text, among them.
var errYAMLTags = errors.New("a YAML tag other than !!str: write each value as text")

// PlainYAML refuses YAML with an anchor (&a) or an alias (*a) in any of its
// documents, read as nodes, nothing repeated, or a tag other than a
// string's, a list's or a mapping's (!!binary, whose value is bytes, not
// text): what is written is what is kept. The rule entries the Settings
// page takes, and --impact-file, are refused so.
func PlainYAML(b []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if aliased(&n) {
			return errYAMLAliases
		}
		if tagged(&n) {
			return errYAMLTags
		}
	}
}

func aliased(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return true
	}
	return slices.ContainsFunc(n.Content, aliased)
}

// tagged reports whether a node, or one within it, is tagged other than
// !!str, !!seq or !!map, as written.
func tagged(n *yaml.Node) bool {
	if n.Style&yaml.TaggedStyle != 0 && n.ShortTag() != "!!str" && n.ShortTag() != "!!seq" && n.ShortTag() != "!!map" {
		return true
	}
	return slices.ContainsFunc(n.Content, tagged)
}

// checkMatch is what the Settings page refuses of the labels a signal or a
// rule entry is matched on: more than verdict.MaxSignalMatch, one that
// isn't a label's name as Prometheus has it, or one twice. A name that
// isn't a label's never reaches a query (source.SeenFrom).
func checkMatch(labels []string) error {
	if len(labels) > verdict.MaxSignalMatch {
		return bad("%d labels to match on: %d at most", len(labels), verdict.MaxSignalMatch)
	}
	for i, l := range labels {
		if !matchLabel.MatchString(l) {
			return bad("%q isn't a label's name: a letter or _, then letters, digits and _", nameCut(l))
		}
		if slices.Contains(labels[:i], l) {
			return bad("the label %s is there twice", l)
		}
	}
	return nil
}

// nameCut is a name as a refusal shows it: cut short.
func nameCut(s string) string {
	if len(s) > 40 {
		return strings.ToValidUTF8(s[:39], "") + "…"
	}
	return s
}

// splitLabels reads labels written comma- or space-separated.
func splitLabels(v string) []string {
	out := []string{}
	for _, l := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		out = append(out, l)
	}
	return out
}

// signalForm is a named signal as the Settings page's form gives it: Was,
// its name as it is ("" to add one); its name, expression, "for" (empty for
// --impact-for's), whether it follows the match in effect, the labels it is
// matched on otherwise, and whether it keeps the record.
type signalForm struct {
	Was, Name, Expr, For string
	Follows              bool
	Match                string
	Record               bool
	// InEffect is the match in effect, --match's, said beside the choice
	// to follow it: "" for no label.
	InEffect string
}

// signalFormOf reads a signal's form, posted or tried.
func signalFormOf(get func(string) string) signalForm {
	return signalForm{Was: strings.TrimSpace(get("was")), Name: strings.TrimSpace(get("name")), Expr: impactText(get("expr")),
		For: strings.TrimSpace(get("for")), Follows: get("match-mode") != "labels", Match: strings.TrimSpace(get("match")), Record: get("record") != ""}
}

// formOfDef is a signal's form as the page fills it in.
func formOfDef(d ledger.SignalDef) signalForm {
	return signalForm{Was: d.Name, Name: d.Name, Expr: d.Expr, For: verdict.Duration(d.For), Follows: d.Match == nil, Match: strings.Join(d.Match, ", "), Record: d.Record}
}

// defOfForm checks a signal's form as the Settings page refuses one: its
// name 32 bytes of the small alphabet; its expression as v0.49.0's is
// checked (checkImpact), Prometheus's parser having the last word when it
// is read, but the one serve was started with (started), which the form
// posts back unchanged, and the UI's checks aren't for; its "for" from the
// step to a day; its match 16 label names at most, each Prometheus's.
func (s *Server) defOfForm(f signalForm, started string) (ledger.SignalDef, error) {
	if !verdict.SignalName.MatchString(f.Name) {
		return ledger.SignalDef{}, bad("a signal's name is a lowercase letter, then lowercase letters, digits, - and _, %d bytes at most", verdict.MaxSignalNameSize)
	}
	if f.Expr == "" {
		return ledger.SignalDef{}, bad("give the signal an expression: PromQL that returns something while users are hurt")
	}
	if started == "" || f.Expr != impactText(started) {
		if err := checkImpact(f.Expr); err != nil {
			return ledger.SignalDef{}, err
		}
	}
	d := ledger.SignalDef{Name: f.Name, Expr: f.Expr, For: s.cfg.ImpactFor, Record: f.Record}
	if f.For != "" {
		var err error
		if d.For, err = s.checkImpactFor(f.For); err != nil {
			return ledger.SignalDef{}, err
		}
	}
	if !f.Follows {
		d.Match = splitLabels(f.Match)
		if err := checkMatch(d.Match); err != nil {
			return ledger.SignalDef{}, err
		}
	}
	return d, nil
}

// sameSignal reports whether two signals say the same, who set them aside.
func sameSignal(a, b ledger.SignalDef) bool {
	return a.Name == b.Name && a.Expr == b.Expr && a.For == b.For && a.Record == b.Record && slices.Equal(a.Match, b.Match) && (a.Match == nil) == (b.Match == nil)
}

// loggedSignal is a signal as the activity log notes it: Match null for
// the match in effect.
type loggedSignal struct {
	Name   string   `json:"name"`
	Expr   string   `json:"expr"`
	For    string   `json:"for"`
	Match  []string `json:"match"`
	Record bool     `json:"record"`
}

func logged(d ledger.SignalDef, ok bool) any {
	if !ok {
		return nil
	}
	return loggedSignal{d.Name, d.Expr, verdict.Duration(d.For), d.Match, d.Record}
}

// signalPlan is a change to the named signals as saving it makes it: what
// the ledger writes (Change), the signals in effect after it (After), and
// those it gives a new criterion, added or with another expression or
// "for", which are read over the last four weeks before it is saved
// (Read); where the page goes back to (Anchor).
type signalPlan struct {
	Change ledger.SignalsChange
	After  signalsNow
	Read   []ledger.SignalDef
	Anchor string
	// Renamed is, by its new name, the name a signal renamed in place had
	// ("" for the impact signal with no name, the first named one saved
	// makes a named one).
	Renamed map[string]string
}

// planSignal works out a change to a signal from its form (f), by action:
// save (add one, change one, rename one in place), remove, or reset (back
// to the start); of the settings set, by who, at at. What it refuses, it
// refuses before anything is read or written:
//
//   - a signal the rule entries name, taken away (by remove, or back to
//     the start of one added here): which entries is said, so that no
//     rule is left judged against none, nor /settings.yaml gives entries
//     naming a signal it doesn't declare;
//   - a name the ledger keeps the windows of, of a signal taken away, for
//     one added (but given back as serve was started with it), as for one
//     renamed (ledger.ChangeSignals refuses that too): its windows would
//     be the new one's;
//   - a name set here and left aside, past the eight at most;
//   - a ninth signal.
//
// The impact signal with no name follows: the first named signal in
// effect makes it a named one (renamed in place, its windows' keys kept),
// and the last taken away, of its criterion, gives it back its own.
func (s *Server) planSignal(ctx context.Context, set map[string]ledger.Setting, f signalForm, action, who string, at time.Time) (signalPlan, error) {
	n := s.signalsIn(set)
	p := signalPlan{Change: ledger.SignalsChange{Settings: map[string]string{}}, Anchor: "impact-signals", Renamed: map[string]string{}}
	c := &p.Change
	what := func(name string) string { return "setting impact signal " + name }
	offOrNone := func(name string) string {
		if _, ok := n.started(name); ok {
			return signalOff
		}
		return ""
	}
	kept, err := s.l.SignalNamesKept(ctx)
	if err != nil {
		return p, err
	}
	switch action {
	case "reset":
		if _, ok := n.UI[f.Was]; !ok {
			return p, bad("the signal %s wasn't changed here: nothing to give back", nameCut(f.Was))
		}
		was, ok := n.def(f.Was)
		start, started := n.started(f.Was)
		if ok && !started {
			if err := entriesName(n.Entries, f.Was); err != nil {
				return p, err
			}
		}
		c.Settings[signalKey+f.Was] = ""
		c.Log = append(c.Log, ledger.Logged{What: what(f.Was), Before: logged(was, ok), After: logged(start, started)})
		if started && (!ok || start.Expr != was.Expr || start.For != was.For) {
			p.Read = append(p.Read, start)
		}
		p.Anchor = "signal-" + f.Was
	case "remove":
		was, ok := n.def(f.Was)
		if !ok {
			return p, bad("no signal named %s is in effect", nameCut(f.Was))
		}
		if err := entriesName(n.Entries, f.Was); err != nil {
			return p, err
		}
		c.Settings[signalKey+f.Was] = offOrNone(f.Was)
		c.Log = append(c.Log, ledger.Logged{What: what(f.Was), Before: logged(was, true)})
	default:
		if s.h == nil {
			return p, errNoHistory
		}
		// The expression serve was started with, of the signal changed (or
		// given back by its name), isn't the UI's to check.
		started := ""
		if st, ok := n.started(f.Was); ok {
			started = st.Expr
		} else if st, ok := n.started(f.Name); ok && f.Was == "" {
			started = st.Expr
		}
		d, err := s.defOfForm(f, started)
		if err != nil {
			return p, err
		}
		old, had := n.def(f.Was)
		if f.Was != "" && !had {
			return p, bad("no signal named %s is in effect", nameCut(f.Was))
		}
		_, inEffect := n.def(d.Name)
		switch {
		case inEffect && d.Name != f.Was:
			return p, bad("a signal named %s is in effect already: change it there", d.Name)
		case slices.Contains(n.Aside, d.Name):
			return p, bad("a signal named %s is set here and left aside, past the eight at most: take one away to have it read, or give this one another name", d.Name)
		case !inEffect && kept[d.Name]:
			if st, ok := n.started(d.Name); !ok || !sameSignal(st, d) || f.Was != "" {
				return p, bad("the ledger keeps the windows of a signal named %s, since taken away: give this one another name", d.Name)
			}
		}
		value := uiSignalOf(d).text()
		if st, ok := n.started(d.Name); ok && sameSignal(st, d) {
			value = "" // the command line's: nothing to keep
		}
		c.Settings[signalKey+d.Name] = value
		newCriterion := !had || old.Expr != d.Expr || old.For != d.For
		if f.Was == "" && len(n.Defs) >= verdict.MaxSignals {
			return p, bad("%d named signals at most: take one away first", verdict.MaxSignals)
		}
		if f.Was == "" && !n.named() && d.Name == "impact" && s.criterionOf(set).Expr != "" {
			return p, bad("the impact signal with no name becomes the signal impact beside it: give this one another name")
		}
		if f.Was != "" && d.Name != f.Was {
			// Renamed: in place, its windows renamed with it, when its
			// criterion is the same, nothing read; else taken away, its
			// windows kept with its name, and another added. Either way,
			// the rule entries that name it name it by its new name, the
			// UI's then.
			if !newCriterion {
				c.Rename, c.From, c.To = true, f.Was, d.Name
				p.Renamed[d.Name] = f.Was
			}
			c.Settings[signalKey+f.Was] = offOrNone(f.Was)
			if es, changed := renamedEntries(n.Entries, f.Was, d.Name); changed {
				c.Settings[rulesKey] = entriesJSON(es)
				c.Log = append(c.Log, ledger.Logged{What: "setting impact rule entries", Before: orNoEntries(n.Entries), After: es})
			}
		}
		c.Log = append(c.Log, ledger.Logged{What: what(d.Name), Before: logged(old, had), After: logged(d, true)})
		if newCriterion {
			p.Read = append(p.Read, d)
		}
		p.Anchor = "signal-" + d.Name
	}
	if unchanged(set, c) {
		*c = ledger.SignalsChange{}
		p.Read = nil
	}
	after := withSettings(set, c.Settings, who, at)
	p.After = s.signalsIn(after)
	switch c0, c1 := s.criterionOf(set), s.criterionOf(after); {
	case !n.named() && p.After.named() && c0.Expr != "" && !c.Rename:
		// The first named signal in effect: the one with no name becomes a
		// named one, its windows and their keys kept
		// (docs/design/signals.md, "Open questions").
		name := "impact"
		for k := 2; kept[name] || slices.ContainsFunc(p.After.Defs, func(d ledger.SignalDef) bool { return d.Name == name }); k++ {
			name = fmt.Sprintf("impact-%d", k)
		}
		// A "for" of none, which reads as the step, as the runs read it.
		u := ledger.SignalDef{Name: name, Expr: c0.Expr, For: max(c0.For, s.cfg.Step), Record: true}
		c.Settings[signalKey+name] = uiSignalOf(u).text()
		c.Rename, c.From, c.To = true, "", name
		p.Renamed[name] = ""
		c.Log = append(c.Log, ledger.Logged{What: what(name), Before: loggedSignal{Expr: c0.Expr, For: verdict.Duration(c0.For), Record: true}, After: logged(u, true)})
		p.After = s.signalsIn(withSettings(set, c.Settings, who, at))
	case n.named() && !p.After.named() && c1.Expr != "" && !c.Rename:
		// The last named signal taken away, the one with no name in effect
		// again: of the same criterion, it was made that one (or reads as
		// it), and has its row and windows back, keys and all, its record
		// standing, nothing read again.
		for _, d := range n.Defs {
			if signalCriterion(d).key() == c1.key() {
				c.Rename, c.From, c.To = true, d.Name, ""
				break
			}
		}
	}
	// None left aside that wasn't: one given back to the start, or added,
	// past the most, is refused.
	for _, name := range p.After.Aside {
		if !slices.Contains(n.Aside, name) {
			return p, bad("%d named signals at most: take one away first", verdict.MaxSignals)
		}
	}
	return p, nil
}

// entriesName refuses a signal the rule entries name taken away, saying
// which: their rules would be judged against no signal, silently.
func entriesName(es []verdict.RuleEntry, name string) error {
	var at []string
	for i, e := range es {
		if slices.Contains(e.Signals, name) {
			at = append(at, fmt.Sprint(i+1))
		}
	}
	if len(at) == 0 {
		return nil
	}
	return bad("rule entries %s name the signal %s: change them first, so that their rules aren't left judged against none", strings.Join(at, ", "), name)
}

// renamedEntries are rule entries with a signal's name changed, and
// whether one named it.
func renamedEntries(es []verdict.RuleEntry, from, to string) ([]verdict.RuleEntry, bool) {
	out := make([]verdict.RuleEntry, 0, len(es))
	changed := false
	for _, e := range es {
		x := verdict.RuleEntry{Rules: e.Rules, Labels: e.Labels, Match: e.Match}
		for _, name := range e.Signals {
			if name == from {
				name, changed = to, true
			}
			x.Signals = append(x.Signals, name)
		}
		out = append(out, x)
	}
	return out, changed
}

// orNoEntries is entries as the activity log notes them: [] for none.
func orNoEntries(es []verdict.RuleEntry) []verdict.RuleEntry {
	if es == nil {
		return []verdict.RuleEntry{}
	}
	return es
}

// entriesJSON is the rule entries as the setting keeps them.
func entriesJSON(es []verdict.RuleEntry) string {
	b, _ := json.Marshal(orNoEntries(es))
	return string(b)
}

// entryDoc is a rule entry as the Settings page's YAML gives it, as
// --impact-file's rules do: match not given (nil) for each signal's own.
type entryDoc struct {
	Rules   string          `yaml:"rules,omitempty"`
	Labels  incident.Labels `yaml:"labels,omitempty"`
	Signals []string        `yaml:"signals,flow"`
	Match   *[]string       `yaml:"match,omitempty,flow"`
}

// entriesYAML is the rule entries as the Settings page's form gives them.
func entriesYAML(es []verdict.RuleEntry) string {
	if len(es) == 0 {
		return ""
	}
	docs := make([]entryDoc, 0, len(es))
	for _, e := range es {
		x := entryDoc{Rules: e.Rules, Labels: e.Labels, Signals: e.Signals}
		if e.Match != nil {
			m := slices.Clone(e.Match)
			x.Match = &m
		}
		docs = append(docs, x)
	}
	b, err := yaml.Marshal(docs)
	if err != nil {
		return ""
	}
	return string(b)
}

// readEntries reads the rule entries the Settings page's form gives, as
// YAML, strictly, as --impact-file's are read, and checks each as the file
// checks it: 64 entries at most, each naming rules by a pattern (a team's
// or a quiet window's, ledger.CheckRE) or by 16 labels at most, and 1 to 8
// signals in effect (n), each once; its match 16 label names at most.
// Empty is no entries.
func readEntries(text string, n signalsNow) ([]verdict.RuleEntry, error) {
	if len(text) > maxEntriesText {
		return nil, bad("the rule entries are %d KB at most: more go in --impact-file", maxEntriesText>>10)
	}
	if strings.TrimSpace(text) == "" {
		return []verdict.RuleEntry{}, nil
	}
	switch err := PlainYAML([]byte(text)); {
	case errors.Is(err, errYAMLAliases):
		return nil, bad("the rule entries use a YAML anchor or alias: write each value out")
	case errors.Is(err, errYAMLTags):
		return nil, bad("the rule entries use a YAML tag other than !!str, such as !!binary: write each value as text")
	}
	var docs []entryDoc
	dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	dec.KnownFields(true)
	if err := dec.Decode(&docs); err != nil {
		return nil, bad("the rule entries don't read as a YAML list of them, as --impact-file's rules: %v", err)
	}
	if len(docs) > verdict.MaxRuleEntries {
		return nil, bad("%d rule entries: %d at most", len(docs), verdict.MaxRuleEntries)
	}
	out := make([]verdict.RuleEntry, 0, len(docs))
	for i, e := range docs {
		at := i + 1
		switch {
		case e.Rules == "" && len(e.Labels) == 0:
			return nil, bad("rule entry %d names no rule: give rules, a pattern of their names, or labels they have", at)
		case len(e.Labels) > verdict.MaxEntryLabels:
			return nil, bad("rule entry %d: %d labels: %d at most", at, len(e.Labels), verdict.MaxEntryLabels)
		case len(e.Signals) == 0:
			return nil, bad("rule entry %d: no signals: name the signals its rules are judged against", at)
		case len(e.Signals) > verdict.MaxEntrySignals:
			return nil, bad("rule entry %d: %d signals: %d at most", at, len(e.Signals), verdict.MaxEntrySignals)
		}
		if _, err := ledger.CheckRE(e.Rules); err != nil {
			return nil, bad("rule entry %d: %s", at, message("en", err))
		}
		for k, vs := range e.Labels {
			if !matchLabel.MatchString(k) || len(vs) == 0 || slices.Contains(vs, "") {
				return nil, bad("rule entry %d: labels: want a label's name and a value, or a list of them, for each", at)
			}
		}
		if err := ledger.CheckLabels(e.Labels); err != nil {
			return nil, bad("rule entry %d: %s", at, message("en", err))
		}
		for j, name := range e.Signals {
			switch {
			case !slices.ContainsFunc(n.Defs, func(d ledger.SignalDef) bool { return d.Name == name }):
				return nil, bad("rule entry %d: no signal named %q is in effect", at, nameCut(name))
			case slices.Contains(e.Signals[:j], name):
				return nil, bad("rule entry %d: the signal %s is there twice", at, name)
			}
		}
		x := verdict.RuleEntry{Rules: e.Rules, Labels: e.Labels, Signals: e.Signals}
		if e.Match != nil {
			x.Match = append([]string{}, *e.Match...)
			if err := checkMatch(x.Match); err != nil {
				return nil, bad("rule entry %d: %s", at, message("en", err))
			}
		}
		out = append(out, x)
	}
	if b, _ := json.Marshal(out); len(b) > maxEntriesJSON {
		return nil, bad("the rule entries are %d KB as kept: %d KB at most", len(b)>>10, maxEntriesJSON>>10)
	}
	return out, nil
}

// sameEntries reports whether two lists of rule entries say the same, as
// the ledger keeps them (ledger.SameEntries).
func sameEntries(a, b []verdict.RuleEntry) bool { return ledger.SameEntries(a, b) }

// planEntries works out the rule entries the form gives (text), or back to
// the start's (reset), of the settings set, by who, at at.
func (s *Server) planEntries(set map[string]ledger.Setting, text string, reset bool, who string, at time.Time) (signalPlan, error) {
	n := s.signalsIn(set)
	p := signalPlan{Change: ledger.SignalsChange{Settings: map[string]string{}}, Anchor: "impact-rule-entries", Renamed: map[string]string{}}
	if !n.named() {
		return p, bad("rule entries say which rules are judged against which named signals: add a named signal first")
	}
	var es []verdict.RuleEntry
	switch {
	case reset:
		if n.EntriesUI == nil {
			return p, bad("the rule entries weren't changed here: nothing to give back")
		}
		// The start's back only if every signal they name is in effect.
		for i, e := range s.cfg.ImpactRules {
			for _, name := range e.Signals {
				if _, ok := n.def(name); !ok {
					return p, bad("rule entry %d serve was started with names the signal %s, which isn't in effect: give it back first", i+1, name)
				}
			}
		}
		p.Change.Settings[rulesKey] = ""
		es = s.cfg.ImpactRules
	default:
		var err error
		if es, err = readEntries(text, n); err != nil {
			return p, err
		}
		p.Change.Settings[rulesKey] = entriesJSON(es)
		if sameEntries(es, s.cfg.ImpactRules) {
			p.Change.Settings[rulesKey] = "" // the start's: nothing to keep
		}
	}
	p.Change.Log = []ledger.Logged{{What: "setting impact rule entries", Before: orNoEntries(n.Entries), After: orNoEntries(es)}}
	if unchanged(set, &p.Change) {
		p.Change = ledger.SignalsChange{}
	}
	p.After = s.signalsIn(withSettings(set, p.Change.Settings, who, at))
	return p, nil
}

// unchanged reports whether a change sets nothing the settings set don't
// say already, and renames nothing: saved as it is, noted nowhere.
func unchanged(set map[string]ledger.Setting, c *ledger.SignalsChange) bool {
	if c.Rename {
		return false
	}
	for k, v := range c.Settings {
		if was, ok := set[k]; v == "" && ok || v != "" && was.Value != v {
			return false
		}
	}
	return true
}

// postSignal saves a change to a named signal, an admin's: added, changed,
// renamed in place, taken away (remove), or given back to the start
// (reset). One that gives a signal a new criterion reads it over the last
// four weeks first, as a try does, unless a try did within triedFor: what
// can't be read within the bounds isn't saved.
func (s *Server) postSignal(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	action := "save"
	switch {
	case r.PostFormValue("remove") != "":
		action = "remove"
	case r.PostFormValue("reset") != "":
		action = "reset"
	}
	ctx := r.Context()
	set, err := s.l.Settings(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	who := s.user(r)
	p, err := s.planSignal(ctx, set, signalFormOf(r.PostFormValue), action, who, time.Now().UTC().Truncate(time.Second))
	// A rename reads nothing, but renames every window of the signal,
	// judges every firing again, and notes the rule entries that name it,
	// before and after: among the reads and saves an admin may ask for in
	// an hour, as saving the entries is.
	if err == nil && p.Change.Rename && !s.takeTry(ctx, who) {
		err = errImpactTries
	}
	if err == nil {
		err = s.readPlanned(ctx, who, p)
	}
	if err == nil && len(p.Change.Settings) > 0 {
		p.Change.Planned = plannedOn(set)
		_, err = s.l.ChangeSignals(ctx, p.Change, s.signalsFrom, who)
	}
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if c := p.Change; c.Rename && c.From != "" && c.To != "" {
		s.renamed(c.From, c.To)
	}
	// Read again, and judged again, by the replica that leads: here, or
	// wherever it runs.
	s.poke()
	back(w, r, "/settings#"+p.Anchor)
}

// readPlanned reads each signal a change gives a new criterion over the
// span a try reads before it is saved, as postImpact reads one: counted
// among who's reads whether a try read it lately or not, and read unless
// one did.
func (s *Server) readPlanned(ctx context.Context, who string, p signalPlan) error {
	for _, d := range p.Read {
		if !s.takeTry(ctx, who) {
			return errImpactTries
		}
		c := signalCriterion(d)
		if s.tried.recent(c.key(), time.Now()) {
			continue
		}
		if err := s.readToSave(ctx, who, c); err != nil {
			return err
		}
	}
	return nil
}

// postEntries saves the rule entries, an admin's, in place of the start's
// as a whole, or gives the start's back (reset).
func (s *Server) postEntries(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	ctx := r.Context()
	set, err := s.l.Settings(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	who := s.user(r)
	p, err := s.planEntries(set, r.PostFormValue("rules"), r.PostFormValue("reset") != "", who, time.Now().UTC().Truncate(time.Second))
	// Saved, the entries judge every firing again: among the reads and
	// saves an admin may ask for in an hour, which bounds too what the
	// activity log keeps of them, their before and after.
	if err == nil && len(p.Change.Settings) > 0 && !s.takeTry(ctx, who) {
		err = errImpactTries
	}
	if err == nil && len(p.Change.Settings) > 0 {
		p.Change.Planned = plannedOn(set)
		_, err = s.l.ChangeSignals(ctx, p.Change, s.signalsFrom, who)
	}
	if err != nil {
		s.oops(w, r, err)
		return
	}
	s.poke()
	back(w, r, "/settings#"+p.Anchor)
}

// postSignalAgain reads one named signal again over the last four weeks,
// alone, an admin's: what saving a new expression for it does, asked for
// itself (after its data was mended, or a read again that kept failing),
// in the heavy turn, within tryWithin and --max-samples, counted among the
// admin's reads. Its windows from then on are put in place of its own, what
// was decided of them and their tasks and comments kept, and the weeks are
// judged again by the replica that leads.
func (s *Server) postSignalAgain(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("name"))
	err := s.signalAgainNow(ctx, s.user(r), name)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	s.poke()
	back(w, r, "/settings#signal-"+name)
}

// errAgainMoved is a read again the runs went past while it was read.
var errAgainMoved = bad("the runs moved on while the signal was read again: ask again")

// signalAgainNow reads the signal name again, for who (postSignalAgain).
func (s *Server) signalAgainNow(ctx context.Context, who, name string) error {
	n, _, err := s.signalsNowOf(ctx)
	if err != nil {
		return err
	}
	d, ok := n.def(name)
	switch {
	case !ok:
		return bad("no signal named %s is in effect", nameCut(name))
	case s.h == nil:
		return errNoHistory
	case !s.takeTry(ctx, who):
		return errImpactTries
	}
	done, err := s.busy(ctx)
	if err != nil {
		return err
	}
	defer done()
	o, err := s.options(ctx)
	if err != nil {
		return err
	}
	read, err := s.readSignalAgain(ctx, d, o, min(s.cfg.Timeout, tryWithin), who)
	switch {
	case err != nil && ctx.Err() == nil && !errors.Is(err, ledger.ErrRunsMoved):
		return bad("the signal couldn't be read again over the last four weeks within %s and --max-samples: %s", verdict.Duration(min(s.cfg.Timeout, tryWithin)), promText(err))
	case err != nil:
		return err
	case !read:
		return errAgainMoved
	}
	return nil
}

// namedViews sets on v the named signals in effect, n, as the Settings
// page lists them: each with what serve was started with of its name, who
// changed it here and when, what keeps the record by it, and whether it is
// to be read again; those serve was started with that were taken away
// here; those left aside; and the rule entries.
func (s *Server) namedViews(ctx context.Context, v *criteriaView, n signalsNow) error {
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		return err
	}
	seen := s.shownSources()
	for _, d := range n.Defs {
		x := namedView{Name: d.Name, Expr: d.Expr, For: verdict.Duration(d.For), Match: d.Match, Follows: d.Match == nil, Record: d.Record, Form: formOfDef(d)}
		if st, ok := n.started(d.Name); ok {
			x.Started = startedOf(st)
		}
		if ui, ok := n.UI[d.Name]; ok {
			x.Changed, x.By, x.Updated = true, ui.By, ui.Updated
		}
		c := signalCriterion(d)
		if i := slices.IndexFunc(rows, func(r ledger.SignalRow) bool { return r.Name == d.Name }); i >= 0 {
			k := rows[i].Kept
			x.Start, x.Floor = k.From(), k.Floor
			x.Data = dataViewOf(dataOf(k, c.key()))
		}
		plan, _, err := s.againPlanOf(ctx, d.Name, c, "")
		if err != nil {
			return err
		}
		if plan == againRead {
			x.Again = &againView{}
			if a, ok := seen.AgainBy[d.Name]; ok && a.Key == c.key() {
				x.Again.Failed, x.Again.Err, x.Again.Next = a.Failed, a.Err, a.Next
			}
		}
		v.Named = append(v.Named, x)
	}
	for _, name := range slices.Sorted(maps.Keys(n.UI)) {
		ui := n.UI[name]
		st, ok := n.started(name)
		if ui.Value != signalOff || !ok {
			continue
		}
		v.Removed = append(v.Removed, namedView{Name: name, Expr: st.Expr, For: verdict.Duration(st.For), Match: st.Match, Follows: st.Match == nil,
			Record: st.Record, Started: startedOf(st), Off: true, Changed: true, By: ui.By, Updated: ui.Updated})
	}
	v.Aside = n.Aside
	e := &entriesView{List: []entryView{}, Started: len(s.cfg.ImpactRules), Text: entriesYAML(n.Entries)}
	for i, x := range n.Entries {
		e.List = append(e.List, entryView{N: i + 1, Rules: x.Rules, Labels: x.Labels, Signals: x.Signals, Match: x.Match, Follows: x.Match == nil})
	}
	if n.EntriesUI != nil {
		e.Changed, e.By, e.Updated = true, n.EntriesUI.By, n.EntriesUI.Updated
	}
	v.Entries = e
	return nil
}

// impactYAML is the named signals in effect and the rule entries as the
// chart's impact, a mapping, as --impact-file reads it: what
// /settings.yaml gives once the Settings page changed any, to commit.
func impactYAML(n signalsNow) string {
	type signal struct {
		Name   string    `yaml:"name"`
		Expr   string    `yaml:"expr"`
		For    string    `yaml:"for"`
		Match  *[]string `yaml:"match,omitempty,flow"`
		Record *bool     `yaml:"record,omitempty"`
	}
	type doc struct {
		Signals []signal   `yaml:"signals"`
		Rules   []entryDoc `yaml:"rules,omitempty"`
	}
	var d doc
	for _, x := range n.Defs {
		y := signal{Name: x.Name, Expr: x.Expr, For: verdict.Duration(x.For)}
		if x.Match != nil {
			m := slices.Clone(x.Match)
			y.Match = &m
		}
		if !x.Record {
			no := false
			y.Record = &no
		}
		d.Signals = append(d.Signals, y)
	}
	for _, e := range n.Entries {
		x := entryDoc{Rules: e.Rules, Labels: e.Labels, Signals: e.Signals}
		if e.Match != nil {
			m := slices.Clone(e.Match)
			x.Match = &m
		}
		d.Rules = append(d.Rules, x)
	}
	b, err := yaml.Marshal(map[string]doc{"impact": d})
	if err != nil {
		return ""
	}
	return string(b)
}

// startedOf is a signal serve was started with, as the page says it.
func startedOf(d ledger.SignalDef) *startedView {
	return &startedView{Expr: d.Expr, For: verdict.Duration(d.For), Match: d.Match, Follows: d.Match == nil, Record: d.Record}
}

// entryView is a rule entry as the Settings page lists it: its place, from
// 1, the rules it names, the signals they are judged against, and the
// labels they are matched on (Follows, each signal's own).
type entryView struct {
	N       int             `json:"n"`
	Rules   string          `json:"rules,omitempty"`
	Labels  incident.Labels `json:"labels,omitempty"`
	Signals []string        `json:"signals"`
	Match   []string        `json:"match"`
	Follows bool            `json:"follows,omitempty"`
}

// plannedOn says whether the settings a change to the named signals reads
// (the signals, the rule entries, the match in effect, the one with no
// name) are as they were in set, which it was worked out from: a change
// saved meanwhile, by another admin or on another replica, refuses it
// (ledger.ErrSignalsChanged), so that one never undoes the other, nor two
// together pass the most of eight.
func plannedOn(set map[string]ledger.Setting) func(map[string]ledger.Setting) bool {
	want := signalsKeys(set)
	return func(now map[string]ledger.Setting) bool { return maps.Equal(want, signalsKeys(now)) }
}

// signalsKeys are the settings the named signals are worked out from, by
// key, their values alone.
func signalsKeys(set map[string]ledger.Setting) map[string]string {
	out := map[string]string{}
	for k, v := range set {
		if signalSetting(k) || k == "match" || k == "impact" || k == "impact-for" {
			out[k] = v.Value
		}
	}
	return out
}

// renamed moves what this process keeps of a signal by its name (how its
// last read went, how reading it again goes, its turns) to the name it was
// renamed to in place: the same signal. Another replica's moves with its
// next read of it.
func (s *Server) renamed(from, to string) {
	s.sources.Lock()
	if x, ok := s.sources.signals[from]; ok {
		s.sources.signals[to] = x
		delete(s.sources.signals, from)
	}
	if x, ok := s.sources.againBy[from]; ok {
		s.sources.againBy[to] = x
		delete(s.sources.againBy, from)
	}
	s.sources.Unlock()
	s.turns.rename(from, to)
}
