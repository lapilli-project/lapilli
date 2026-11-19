package serve

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The named impact signals in effect (docs/design/signals.md, step 3):
// those serve was started with (--impact NAME=PROMQL, --impact-file, the
// chart's impact), and those an admin set on the Settings page, kept in
// settings as the other judging settings are, by name: the UI's kept over
// the command line's of the same name, a signal the command line gives
// taken away by the UI's "off", and the rule entries the UI saved in place
// of the start's, as a whole. Back to the start deletes the UI's.

// The settings named signals and the rule entries are kept under: a
// signal's by its name after signalKey, its value a uiSignal as JSON, or
// signalOff; the rule entries as a JSON list.
const (
	signalKey = "impact-signal/"
	rulesKey  = "impact-rules"
	signalOff = "off"
)

// signalSetting reports whether a setting is a named signal's, or the
// rule entries': not a judging setting --lead and the others are.
func signalSetting(key string) bool { return strings.HasPrefix(key, signalKey) || key == rulesKey }

// uiSignal is a named signal as the Settings page keeps it: Match nil for
// the match in effect, empty for none; Lead "" to follow the lead in
// effect (docs/design/judging.md, item 1).
type uiSignal struct {
	Expr   string   `json:"expr"`
	For    string   `json:"for"`
	Match  []string `json:"match"`
	Record bool     `json:"record"`
	Lead   string   `json:"lead,omitempty"`
}

// uiSignalOf is a signal as the Settings page keeps it.
func uiSignalOf(d ledger.SignalDef) uiSignal {
	return uiSignal{Expr: d.Expr, For: verdict.Duration(d.For), Match: d.Match, Record: d.Record, Lead: leadShown(d.Lead)}
}

// leadShown is a lead of a signal's or a rule entry's own as the Settings
// page, /settings.yaml and the summary say it: "" to follow.
func leadShown(d *time.Duration) string {
	if d == nil {
		return ""
	}
	return verdict.Duration(*d)
}

// leadOfText reads a lead of a signal's or a rule entry's own, as the
// Settings page takes one: "" to follow, else a duration from 0 to
// verdict.LeadBound.
func leadOfText(v string) (*time.Duration, error) {
	d, ok := parseLead(v)
	if !ok {
		return nil, bad("a lead of %s: want a duration from 0 to %s, such as 1h, or none to follow the lead above", nameCut(v), verdict.Duration(verdict.LeadBound))
	}
	return d, nil
}

// parseLead is leadOfText, ok false for a lead it refuses.
func parseLead(v string) (*time.Duration, bool) {
	if v == "" {
		return nil, true
	}
	d, err := ParseDuration(v)
	if err != nil || d < 0 || d > verdict.LeadBound {
		return nil, false
	}
	return &d, true
}

// text is the setting it is kept as.
func (u uiSignal) text() string {
	b, _ := json.Marshal(u)
	return string(b)
}

// defOf reads a named signal the Settings page kept: who set it and when
// with it. One that doesn't read, as no lapilli writes it, is none.
func defOf(name string, set ledger.Setting) (ledger.SignalDef, bool) {
	var u uiSignal
	if set.Value == signalOff || json.Unmarshal([]byte(set.Value), &u) != nil || u.Expr == "" {
		return ledger.SignalDef{}, false
	}
	d, err := ParseDuration(u.For)
	if err != nil || d <= 0 {
		return ledger.SignalDef{}, false
	}
	lead, ok := parseLead(u.Lead)
	if !ok {
		return ledger.SignalDef{}, false
	}
	return ledger.SignalDef{Name: name, Expr: u.Expr, For: d, Match: u.Match, Record: u.Record, Lead: lead, SetBy: set.By, SetAt: set.Updated}, true
}

// signalsNow are the named impact signals in effect, and the rule
// entries, of the settings the UI changed and the command line: Defs, in
// the start's order, the UI's own after them, the earliest set first;
// Start, the command line's; UI, what the Settings page set, by name (a
// signal, or off); Aside, the UI's own past verdict.MaxSignals, which a
// start with more signals than before left no room for. Entries are the
// UI's, when EntriesUI is the setting they came from, else the start's.
type signalsNow struct {
	Defs      []ledger.SignalDef
	Entries   []verdict.RuleEntry
	EntriesUI *ledger.Setting
	Start     []ledger.SignalDef
	UI        map[string]ledger.Setting
	Aside     []string
}

// signalsIn are the named signals in effect, by the settings set.
func (s *Server) signalsIn(set map[string]ledger.Setting) signalsNow {
	n := signalsNow{Start: s.cfg.Signals, Entries: s.cfg.ImpactRules, UI: map[string]ledger.Setting{}}
	for k, v := range set {
		if name, ok := strings.CutPrefix(k, signalKey); ok && verdict.SignalName.MatchString(name) {
			n.UI[name] = v
		}
	}
	started := func(name string) bool {
		return slices.ContainsFunc(s.cfg.Signals, func(d ledger.SignalDef) bool { return d.Name == name })
	}
	for _, d := range s.cfg.Signals {
		ui, ok := n.UI[d.Name]
		switch {
		case !ok:
			n.Defs = append(n.Defs, d)
		case ui.Value == signalOff:
		default:
			if x, ok := defOf(d.Name, ui); ok {
				d = x
			}
			n.Defs = append(n.Defs, d)
		}
	}
	var own []string
	for name, ui := range n.UI {
		if ui.Value != signalOff && !started(name) {
			own = append(own, name)
		}
	}
	slices.SortFunc(own, func(a, b string) int {
		return cmp.Or(n.UI[a].Updated.Compare(n.UI[b].Updated), strings.Compare(a, b))
	})
	for _, name := range own {
		d, ok := defOf(name, n.UI[name])
		switch {
		case !ok:
		case len(n.Defs) >= verdict.MaxSignals:
			n.Aside = append(n.Aside, name)
		default:
			n.Defs = append(n.Defs, d)
		}
	}
	if v, ok := set[rulesKey]; ok {
		var es []verdict.RuleEntry
		if json.Unmarshal([]byte(v.Value), &es) == nil {
			n.Entries, n.EntriesUI = es, &v
		}
	}
	return n
}

// named says there are named signals in effect.
func (n signalsNow) named() bool { return len(n.Defs) > 0 }

// set is what the ledger keeps of them (ledger.SyncSignalSet).
func (n signalsNow) set() ledger.SignalSet {
	return ledger.SignalSet{Defs: n.Defs, Entries: n.Entries, EntriesUI: n.EntriesUI != nil}
}

// def is the signal in effect by name.
func (n signalsNow) def(name string) (ledger.SignalDef, bool) {
	i := slices.IndexFunc(n.Defs, func(d ledger.SignalDef) bool { return d.Name == name })
	if i < 0 {
		return ledger.SignalDef{}, false
	}
	return n.Defs[i], true
}

// started is the signal serve was started with by name.
func (n signalsNow) started(name string) (ledger.SignalDef, bool) {
	i := slices.IndexFunc(n.Start, func(d ledger.SignalDef) bool { return d.Name == name })
	if i < 0 {
		return ledger.SignalDef{}, false
	}
	return n.Start[i], true
}

// signalsNowOf are the named signals in effect now, with the settings
// they were read from.
func (s *Server) signalsNowOf(ctx context.Context) (signalsNow, map[string]ledger.Setting, error) {
	set, err := s.l.Settings(ctx)
	if err != nil {
		return s.signalsIn(nil), nil, err
	}
	return s.signalsIn(set), set, nil
}

// matchOf is the match in effect, of the settings set: --match, or the
// UI's.
func (s *Server) matchOf(set map[string]ledger.Setting) []string {
	o := s.cfg.Options
	if v, ok := set["match"]; ok {
		if err := apply(&o, "match", v.Value); err != nil {
			return s.cfg.Options.Match
		}
	}
	return o.Match
}

// leadOf is --lead in effect, by the settings set: the command line's, or
// the UI's, as options reads it, the command line's when it doesn't parse.
func (s *Server) leadOf(set map[string]ledger.Setting) time.Duration {
	o := s.cfg.Options
	if v, ok := set["lead"]; ok {
		if err := apply(&o, "lead", v.Value); err != nil {
			return s.cfg.Options.Lead
		}
	}
	return o.Lead
}

// withSettings are the settings set with values changed, as saving them
// would leave them: "" deletes one; who set them and when, by and at.
func withSettings(set map[string]ledger.Setting, values map[string]string, by string, at time.Time) map[string]ledger.Setting {
	out := maps.Clone(set)
	if out == nil {
		out = map[string]ledger.Setting{}
	}
	for k, v := range values {
		if v == "" {
			delete(out, k)
		} else if out[k].Value != v {
			out[k] = ledger.Setting{Value: v, By: by, Updated: at}
		}
	}
	return out
}
