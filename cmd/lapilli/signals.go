package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// Named impact signals (docs/design/signals.md): --impact NAME=PROMQL, each
// with its own --impact-for NAME=DUR, --impact-match NAME=LABELS and
// --impact-record NAME=no, or all of them, and the rule entries, in
// --impact-file.

// impactSignal is a named impact signal, as the flags or --impact-file
// give it.
type impactSignal struct {
	name, expr string
	// dur is its "for", 0 for --impact-for's; match its labels, nil for the
	// match in effect; record whether it keeps the record.
	dur    time.Duration
	match  []string
	record bool
}

// impactSet is what --impact-for NAME=DUR, --impact-match NAME=LABELS and
// --impact-record NAME=... said of a signal, given before or after its
// --impact.
type impactSet struct {
	dur      time.Duration
	match    []string
	matchSet bool
	record   *bool
}

// signalHead is how a named --impact starts: a name and a = that isn't
// followed by = or ~. No PromQL expression starts so: a lone = is PromQL
// only inside a selector's braces, after a label's name.
var signalHead = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)=([^=~]|$)`)

// nameText is a name as an error shows it: cut short.
func nameText(s string) string {
	if len(s) > 40 {
		return s[:39] + "…"
	}
	return s
}

// splitSignal reads --impact's value: NAME=PROMQL, its name and its
// expression, or PROMQL, with no name.
func splitSignal(val string) (name, expr string, err error) {
	m := signalHead.FindStringSubmatch(val)
	if m == nil {
		return "", val, nil
	}
	name = m[1]
	if !verdict.SignalName.MatchString(name) {
		return "", "", fmt.Errorf("--impact %s=...: a signal's name is a lowercase letter, then lowercase letters, digits, - and _, %d bytes at most", nameText(name), verdict.MaxSignalNameSize)
	}
	if expr = strings.TrimSpace(val[len(name)+1:]); expr == "" {
		return "", "", fmt.Errorf("--impact %s=: no expression after the name", name)
	}
	return name, expr, nil
}

// signalFlag reads a NAME=VALUE given to one of the --impact-... flags.
func signalFlag(flag, val string) (name, rest string, err error) {
	name, rest, ok := strings.Cut(val, "=")
	if !ok || !verdict.SignalName.MatchString(name) {
		return "", "", fmt.Errorf("%s %s: want NAME=..., NAME the name an --impact NAME=PROMQL gives", flag, nameText(val))
	}
	return name, rest, nil
}

// matchList reads the labels a signal or a rule entry is matched on.
func matchList(at string, labels []string) error {
	if len(labels) > verdict.MaxSignalMatch {
		return fmt.Errorf("%s: %d labels to match on: %d at most", at, len(labels), verdict.MaxSignalMatch)
	}
	for i, l := range labels {
		if !labelName.MatchString(l) {
			return fmt.Errorf("%s: %q isn't a label's name", at, nameText(l))
		}
		if slices.Contains(labels[:i], l) {
			return fmt.Errorf("%s: the label %s is there twice", at, l)
		}
	}
	return nil
}

// impactBy is the --impact-for NAME=DUR, --impact-match or --impact-record
// of a signal, kept until every flag is read.
func (f *verdictFlags) impactBy(name string) *impactSet {
	if f.impactSets == nil {
		f.impactSets = map[string]*impactSet{}
	}
	if f.impactSets[name] == nil {
		f.impactSets[name] = &impactSet{}
	}
	return f.impactSets[name]
}

// impactFlag reads --impact, --impact-for, --impact-match and
// --impact-record; ok is false for another flag.
func (f *verdictFlags) impactFlag(name, val string) (ok bool, err error) {
	switch name {
	case "--impact":
		n, expr, err := splitSignal(val)
		switch {
		case err != nil:
			return true, err
		case n == "" && f.impact != "":
			return true, errors.New("--impact is given twice without a name: give each signal a name, --impact NAME=PROMQL, or join them in one expression")
		case n == "":
			f.impact = val
		case slices.ContainsFunc(f.signals, func(s impactSignal) bool { return s.name == n }):
			return true, fmt.Errorf("--impact %s=...: the signal %s is named twice", n, n)
		default:
			f.signals = append(f.signals, impactSignal{name: n, expr: expr, record: true})
		}
	case "--impact-for":
		n, d, named := strings.Cut(val, "=")
		if !named {
			if f.impactFor, err = parseDuration(val); err != nil || f.impactFor < time.Second {
				return true, fmt.Errorf("--impact-for %s: want a duration like 5m, or NAME=DUR for one signal", val)
			}
			return true, nil
		}
		if n, _, err = signalFlag(name, val); err != nil {
			return true, err
		}
		s := f.impactBy(n)
		if s.dur != 0 {
			return true, fmt.Errorf("--impact-for %s=...: given twice", n)
		}
		if s.dur, err = parseDuration(d); err != nil || s.dur <= 0 {
			return true, fmt.Errorf("--impact-for %s: want NAME=DUR, a duration like 2m", val)
		}
	case "--impact-match":
		n, labels, err := signalFlag(name, val)
		if err != nil {
			return true, err
		}
		s := f.impactBy(n)
		if s.matchSet {
			return true, fmt.Errorf("--impact-match %s=...: given twice: give its labels together, comma-separated", n)
		}
		s.matchSet, s.match = true, []string{}
		if labels != "" {
			for _, l := range strings.Split(labels, ",") {
				s.match = append(s.match, strings.TrimSpace(l))
			}
		}
		return true, matchList("--impact-match "+n, s.match)
	case "--impact-record":
		n, v, err := signalFlag(name, val)
		if err != nil {
			return true, err
		}
		s := f.impactBy(n)
		var b bool
		switch strings.ToLower(v) {
		case "yes", "true":
			b = true
		case "no", "false":
		default:
			return true, fmt.Errorf("--impact-record %s: want NAME=yes or NAME=no", val)
		}
		if s.record != nil {
			return true, fmt.Errorf("--impact-record %s=...: given twice", n)
		}
		s.record = &b
	case "--impact-file":
		if f.impactFile != "" {
			return true, errors.New("--impact-file is given twice: give every signal in one file")
		}
		f.impactFile = val
	default:
		return false, nil
	}
	return true, nil
}

// anyImpact reports whether an impact signal is read: one with no name, or
// named ones.
func (f *verdictFlags) anyImpact() bool { return f.impact != "" || len(f.signals) > 0 }

// signalsRead checks the impact signals once every flag is read, the one
// with no name as the named ones are (serve.CheckImpact, as the Settings
// page checks one), reads --impact-file, and sets what judges by the
// named ones (Options.Signals): their names, "for"s, matches and records,
// and the rule entries. Where each signal's data begins, and what it
// returned, a read of them says (readSignals); lapilli serve's runs, of
// serve's (signalDefs).
func (f *verdictFlags) signalsRead() error {
	perName := len(f.impactSets) > 0
	switch {
	case f.impactFile != "" && f.anyImpact():
		return errors.New("--impact-file gives the impact signals: leave out --impact")
	case f.impactFile != "" && perName:
		return errors.New("--impact-file says each signal's for, match and record: leave out --impact-for NAME=DUR, --impact-match and --impact-record")
	case len(f.signals) > 0 && f.impact != "":
		return errors.New("--impact: give every impact signal a name, --impact NAME=PROMQL, or give one alone without: a signal with no name and named ones don't go together")
	}
	if f.impact != "" {
		if err := serve.CheckImpact(f.impact); err != nil {
			return fmt.Errorf("--impact: %w", err)
		}
	}
	var entries []verdict.RuleEntry
	if f.impactFile != "" {
		var err error
		if f.signals, entries, err = readImpactFile(f.impactFile); err != nil {
			return fmt.Errorf("--impact-file %s: %w", f.impactFile, err)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(f.impactSets)) {
		set := f.impactSets[n]
		i := slices.IndexFunc(f.signals, func(s impactSignal) bool { return s.name == n })
		if i < 0 {
			return fmt.Errorf("--impact-for, --impact-match or --impact-record name %s, which no --impact NAME=PROMQL gives", n)
		}
		s := &f.signals[i]
		if set.dur != 0 {
			s.dur = set.dur
		}
		if set.matchSet {
			s.match = set.match
		}
		if set.record != nil {
			s.record = *set.record
		}
	}
	if len(f.signals) == 0 {
		return nil
	}
	if len(f.signals) > verdict.MaxSignals {
		return fmt.Errorf("--impact: %d named signals: %d at most", len(f.signals), verdict.MaxSignals)
	}
	list := make([]verdict.Signal, 0, len(f.signals))
	for _, s := range f.signals {
		if err := serve.CheckImpact(s.expr); err != nil {
			return fmt.Errorf("--impact %s: %w", s.name, err)
		}
		d := s.dur
		if d == 0 {
			d = f.impactFor
		}
		if d < f.step || d > 24*time.Hour {
			return fmt.Errorf("--impact-for %s: %s: want from --step (%s) to 1d", s.name, verdict.Duration(d), verdict.Duration(f.step))
		}
		list = append(list, verdict.Signal{Name: s.name, For: d, Match: s.match, Record: s.record})
	}
	f.opts.Signals = verdict.NewSignals(list, entries)
	return nil
}

// signalDefs are the named signals as lapilli serve keeps them, each with
// its "for", --impact-for's when it gives none; and the rule entries.
func (f *verdictFlags) signalDefs() ([]ledger.SignalDef, []verdict.RuleEntry) {
	if !f.opts.Signals.Named() {
		return nil, nil
	}
	var out []ledger.SignalDef
	for i, s := range f.signals {
		out = append(out, ledger.SignalDef{Name: s.name, Expr: s.expr, For: f.opts.Signals.List[i].For, Match: s.match, Record: s.record})
	}
	return out, slices.Clone(f.opts.Signals.Entries)
}

// warnTo is where what a run says on its way goes: stderr, or nowhere.
func (f *verdictFlags) warnTo() io.Writer {
	if f.warn == nil {
		return io.Discard
	}
	return f.warn
}

// maxImpactFile is the most of --impact-file read: more is refused.
const maxImpactFile = 1 << 20

// impactFileDoc is --impact-file: the signals and the rule entries, at its
// top or under impact:, as the chart's impact mapping holds them.
type impactFileDoc struct {
	Signals []impactFileSignal `yaml:"signals"`
	Rules   []impactFileEntry  `yaml:"rules"`
	Impact  *impactFileDoc     `yaml:"impact"`
}

type impactFileSignal struct {
	Name   string   `yaml:"name"`
	Expr   string   `yaml:"expr"`
	For    string   `yaml:"for"`
	Match  []string `yaml:"match"`
	Record *bool    `yaml:"record"`
}

type impactFileEntry struct {
	Rules   string          `yaml:"rules"`
	Labels  incident.Labels `yaml:"labels"`
	Signals []string        `yaml:"signals"`
	Match   []string        `yaml:"match"`
}

// readImpactFile reads --impact-file strictly, as --config is read: a key
// it doesn't know, a signal named twice, a rule entry naming a signal the
// file doesn't declare, or a file over 1 MiB refuses all of it.
func readImpactFile(path string) ([]impactSignal, []verdict.RuleEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	b, err := io.ReadAll(io.LimitReader(file, maxImpactFile+1))
	file.Close()
	switch {
	case err != nil:
		return nil, nil, err
	case len(b) > maxImpactFile:
		return nil, nil, fmt.Errorf("more than %d MiB: a file of impact signals is less", maxImpactFile>>20)
	}
	var doc impactFileDoc
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, errors.New("empty: want signals:, with a list of them under it")
		}
		return nil, nil, err
	}
	// One document: what comes after a --- would be read by nothing.
	var more yaml.Node
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("more than one YAML document: give the signals and the rules in one")
	}
	if doc.Impact != nil {
		if len(doc.Signals) > 0 || len(doc.Rules) > 0 || doc.Impact.Impact != nil {
			return nil, nil, errors.New("give signals and rules under impact:, or at the top, not both")
		}
		doc = *doc.Impact
	}
	if len(doc.Signals) == 0 {
		return nil, nil, errors.New("no signals: want signals:, with a list of them under it")
	}
	if len(doc.Signals) > verdict.MaxSignals {
		return nil, nil, fmt.Errorf("%d signals: %d at most", len(doc.Signals), verdict.MaxSignals)
	}
	var out []impactSignal
	for i, s := range doc.Signals {
		at := fmt.Sprintf("signal %d", i+1)
		switch {
		case !verdict.SignalName.MatchString(s.Name):
			return nil, nil, fmt.Errorf("%s: its name is a lowercase letter, then lowercase letters, digits, - and _, %d bytes at most", at, verdict.MaxSignalNameSize)
		case slices.ContainsFunc(out, func(x impactSignal) bool { return x.name == s.Name }):
			return nil, nil, fmt.Errorf("%s: %s is named twice", at, s.Name)
		case strings.TrimSpace(s.Expr) == "":
			return nil, nil, fmt.Errorf("%s (%s): no expr", at, s.Name)
		}
		x := impactSignal{name: s.Name, expr: strings.TrimSpace(s.Expr), match: s.Match, record: s.Record == nil || *s.Record}
		if s.For != "" {
			if x.dur, err = parseDuration(s.For); err != nil || x.dur <= 0 {
				return nil, nil, fmt.Errorf("%s (%s): for %s: want a duration like 2m", at, s.Name, s.For)
			}
		}
		if err := matchList(at+" ("+s.Name+")", s.Match); err != nil {
			return nil, nil, err
		}
		out = append(out, x)
	}
	if len(doc.Rules) > verdict.MaxRuleEntries {
		return nil, nil, fmt.Errorf("%d rule entries: %d at most", len(doc.Rules), verdict.MaxRuleEntries)
	}
	var entries []verdict.RuleEntry
	for i, e := range doc.Rules {
		at := fmt.Sprintf("rule entry %d", i+1)
		switch {
		case e.Rules == "" && len(e.Labels) == 0:
			return nil, nil, fmt.Errorf("%s names no rule: give rules, a pattern of their names, or labels they have", at)
		case len(e.Labels) > verdict.MaxEntryLabels:
			return nil, nil, fmt.Errorf("%s: %d labels: %d at most", at, len(e.Labels), verdict.MaxEntryLabels)
		case len(e.Signals) == 0:
			return nil, nil, fmt.Errorf("%s: no signals: name the signals its rules are judged against", at)
		case len(e.Signals) > verdict.MaxEntrySignals:
			return nil, nil, fmt.Errorf("%s: %d signals: %d at most", at, len(e.Signals), verdict.MaxEntrySignals)
		}
		if _, err := ledger.CheckRE(e.Rules); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", at, err)
		}
		for k, vs := range e.Labels {
			if !labelName.MatchString(k) || len(vs) == 0 || slices.Contains(vs, "") {
				return nil, nil, fmt.Errorf("%s: labels: want a label's name and a value, or a list of them, for each", at)
			}
		}
		for j, name := range e.Signals {
			switch {
			case !slices.ContainsFunc(out, func(x impactSignal) bool { return x.name == name }):
				return nil, nil, fmt.Errorf("%s: the signal %q isn't one the file declares", at, nameText(name))
			case slices.Contains(e.Signals[:j], name):
				return nil, nil, fmt.Errorf("%s: the signal %s is there twice", at, name)
			}
		}
		if err := matchList(at, e.Match); err != nil {
			return nil, nil, err
		}
		entries = append(entries, verdict.RuleEntry{Rules: e.Rules, Labels: e.Labels, Signals: e.Signals, Match: e.Match})
	}
	return out, entries, nil
}

// readSignals reads each named impact signal over the period and the two
// weeks before it, as an impact signal with no name is read (readImpact):
// its windows in the period, of its series alone, matched on its labels;
// what its series returned, and what it reads (source.SeenFrom), for the
// firings it could have seen (verdict.Seen); and where the data of each
// selector it reads begins, by
// the one rule (ledger.FloorBy, each firing judged by its own time): from
// there on, it keeps the record. Its windows join incs as an unnamed
// signal's do, each one that is an incident written down left out.
func (f *verdictFlags) readSignals(ctx context.Context, h history, incs []incident.Incident) ([]incident.Incident, error) {
	from := verdict.RecordFrom(f.opts.From, f.opts.To)
	list := slices.Clone(f.opts.Signals.List)
	var found []incident.Incident
	for i, s := range f.signals {
		x := &list[i]
		series, err := h.QueryRange(ctx, s.expr, from, f.opts.To, f.step)
		if err != nil {
			return nil, fmt.Errorf("--impact %s: %w", s.name, err)
		}
		seen := verdict.NewSeen(x.MatchOr(f.opts.Match))
		in := make([]prom.Series, 0, len(series))
		for _, se := range series {
			if len(se.Samples) > 0 {
				seen.Add(se.Labels)
			}
			se.Samples = slices.DeleteFunc(slices.Clone(se.Samples), func(p prom.Sample) bool { return p.T.Before(f.opts.From) })
			in = append(in, se)
		}
		// And what it reads, by the same labels: a service it measured all
		// along, never hurt, is one it could have seen hurt.
		// A selector that fails adds nothing: the others' counts are kept,
		// and the run says so rather than failing.
		read, _, err := source.SeenFrom(ctx, h, s.expr, x.MatchOr(f.opts.Match), from, f.opts.To)
		if err != nil {
			fmt.Fprintf(f.warnTo(), "lapilli verdict: --impact %s: reading what it reads, by its match labels, a selector failed: %s; what it returned, and what the others read, tie its firings\n", verdict.Clean(s.name), verdict.Clean(err.Error()))
		}
		seen.Merge(read)
		x.Seen = seen
		ws, err := incident.FromSignal(ctx, in, f.step, x.For, x.Name, x.Match)
		if err != nil {
			return nil, fmt.Errorf("--impact %s: %w", s.name, err)
		}
		found = append(found, ws...)
		if x.Start, err = f.recordStart(ctx, h, s.expr, from); err != nil {
			return nil, fmt.Errorf("--impact %s: looking for where the data it reads begins: %w", s.name, err)
		}
	}
	f.opts.Signals = verdict.NewSignals(list, f.opts.Signals.Entries)
	return incident.Merge(incs, found, f.opts.UnknownLen, f.opts.Match), nil
}

// recordStart is from when an impact signal read from from on keeps the
// record, named or not, by the one rule lapilli serve keeps it by
// (ledger.FloorBy), each firing judged by its own time: from, or later,
// where the data of every selector expr reads begins, the earliest series
// of each, the latest of those; zero when a selector it reads has no data.
func (f *verdictFlags) recordStart(ctx context.Context, h history, expr string, from time.Time) (time.Time, error) {
	sels, err := source.FirstSamples(ctx, h, expr, from, f.opts.To, true)
	if err != nil {
		return time.Time{}, err
	}
	d := ledger.ImpactData{From: from, CheckedTo: f.opts.To, Selectors: sels}
	ledger.FloorBy(&d, func(t time.Time) time.Time { return t })
	if d.NoData {
		return time.Time{}, nil
	}
	if d.Floor.After(from) {
		return d.Floor, nil
	}
	return from, nil
}

// indexSignals is s with each rule's entry found once: by its name, or
// else by its labels, as the latest version of the rules that has it gives
// them; of a rule with none, its firings', those all of them have alike.
func indexSignals(s *verdict.Signals, versions []replay.Version, fs []firing.Firing) *verdict.Signals {
	if !s.Named() || len(s.Entries) == 0 {
		return s
	}
	labels := map[string]map[string]string{}
	for _, v := range versions {
		for _, r := range v.Rules {
			labels[r.Name] = r.AlertLabels()
		}
	}
	shared := map[string]map[string]string{}
	for _, x := range fs {
		if _, ok := labels[x.Rule]; ok {
			continue
		}
		l, ok := shared[x.Rule]
		if !ok {
			shared[x.Rule] = maps.Clone(x.Labels)
			continue
		}
		for k, v := range l {
			if x.Labels[k] != v {
				delete(l, k)
			}
		}
	}
	for rule, l := range shared {
		labels[rule] = l
	}
	return s.Index(labels)
}

// indexRules is s indexed by one set of rules, for one side of --compare.
func indexRules(s *verdict.Signals, rs []rules.Rule, fs []firing.Firing) *verdict.Signals {
	return indexSignals(s, []replay.Version{{Rules: rs}}, fs)
}

// shortestSignal is, of named signals, the one whose "for" is shortest,
// the first of those as short: the shortest impact that counts as an
// incident.
func (f *verdictFlags) shortestSignal() (verdict.Signal, bool) {
	if !f.opts.Signals.Named() {
		return verdict.Signal{}, false
	}
	s := f.opts.Signals.List[0]
	for _, x := range f.opts.Signals.List[1:] {
		if x.For < s.For {
			s = x
		}
	}
	return s, true
}

// shortestFor is the shortest "for" of the impact signals read: --impact-for
// with none named.
func (f *verdictFlags) shortestFor() time.Duration {
	if s, ok := f.shortestSignal(); ok {
		return s.For
	}
	return f.impactFor
}

// judgedAgainst reports whether a rule is judged against the windows of
// the signal with the shortest "for": no rule entry names it, or its
// entry names that signal. With no named signal, every rule is.
func (f *verdictFlags) judgedAgainst(rule string) bool {
	s, ok := f.shortestSignal()
	if !ok {
		return true
	}
	e := f.opts.Signals.Entry(rule)
	return e == nil || slices.Contains(e.Signals, s.Name)
}
