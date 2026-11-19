package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// Declared configuration (docs/design/serveops.md): --config is a YAML
// file declaring teams, quiet windows, reports and roles, as the UI's forms
// make them, which the leader keeps the ledger holding (ledger.Reconcile):
// when it starts, and whenever the file's content changes, read again
// every configEvery. What it declares the UI shows, and refuses to change.
// A file that can't be used changes nothing: the ledger keeps what the
// last good one said, and the Settings and Runs pages say why. GET
// /config.yaml gives what the ledger holds in the same form.

// maxConfig is the most a --config file holds, in bytes: a ConfigMap's
// most.
const maxConfig = 1 << 20

// configEvery is how often the leader reads --config again, for a change.
// Kubernetes swaps a ConfigMap's files in place, through a link, a minute
// or so after it changes: its content is read and hashed, not watched.
var configEvery = 15 * time.Second

// configKeys are the keys of a --config file and of each thing in it, in
// the order an export writes them.
var configKeys = map[string][]string{
	"":     {"teams", "windows", "reports", "grants", "goals", "marked"},
	"team": {"name", "rules", "labels", "note", "work_hours", "sleep_hours", "tz", "shifts", "renamed_from"},
	// A window's on is repeat_on: YAML 1.1, which Helm reads values by,
	// takes a key on, unquoted, for true.
	"window": {"title", "team", "start", "end", "every", "repeat_on", "until", "rules", "labels"},
	"report": {"name", "team", "schedule", "span", "zone", "sections", "dests", "enabled"},
	"grant":  {"user", "group", "team"},
	// A goal's bound is one of at_least, at_most and no_worse_than; by goes
	// with no_worse_than (docs/design/goals.md).
	"goal": {"name", "team", "measure", "at_least", "at_most", "no_worse_than", "by", "critical", "each", "stones"},
	// A marked window (docs/design/marked.md): a kind and a title, its
	// times as a quiet window's, and labels of one value each.
	"marked": {"kind", "title", "team", "start", "end", "every", "repeat_on", "until", "labels"},
}

// What can be wrong with a --config file, as texts to translate: the
// keys of each thing it declares (unknownIn), a thing that isn't a
// mapping (wantA), and the rest (configFormats lists them all).
var (
	unknownIn = map[string]string{
		"team":   "unknown key %q in a team: want %s",
		"window": "unknown key %q in a quiet window: want %s",
		"report": "unknown key %q in a report: want %s",
		"grant":  "unknown key %q in a role: want %s",
		"goal":   "unknown key %q in a goal: want %s",
		"marked": "unknown key %q in a marked window: want %s",
	}
	wantA = map[string]string{
		"team":   "want a team, as a mapping of %s",
		"window": "want a quiet window, as a mapping of %s",
		"report": "want a report, as a mapping of %s",
		"grant":  "want a role, as a mapping of %s",
		"goal":   "want a goal, as a mapping of %s",
		"marked": "want a marked window, as a mapping of %s",
	}
	configFormats = []string{
		"line %d: %s", "line %d (%s): %s", "the file isn't YAML: %s", "the file is more than %d bytes",
		"one document only: --config is one file of teams, windows, reports, grants, goals and marked",
		"want a mapping of teams, windows, reports, grants, goals and marked", "unknown key %q: want teams, windows, reports, grants, goals or marked",
		"%s is given twice, on line %d too", "%s is a list", "%s is a text", "%s is a list of texts",
		"labels are a mapping of a label's name to a value, or a list of values", "a label's name is a text", "label %s has no value",
		"%s %q: want a time like 2026-11-02T09:30, or 2026-11-02T09:30:00+09:00", "a team has a name", "a quiet window has a title",
		"a report has a name", "enabled is true or false", "a role is given to a user or a group: one of user and group",
		"the file declares nothing: an empty file would take away all it declared; to declare nothing, give teams: []",
		"%s has nothing after it: give %s: [] for none",
		"team has nothing after it: leave the key out for every team", "rules has nothing after it: leave the key out for every rule",
		"%s has nothing after it: give it a value, or leave the key out",
		"%s is %s, read as a boolean or a number, not a text: quote it, or give serve.config as a text",
		"label %s's value %s is read as a boolean or a number, not a text: quote it, or give serve.config as a text",
		"a config declares %d teams at most", "a config declares %d quiet windows at most", "a config declares %d reports at most",
		"a config declares %d roles at most", "a config declares %d goals at most", "a goal has a name", "a goal has a measure",
		"a goal called %s is declared on line %d already: a goal's name is unique, across everyone's and every team's",
		"%s is a number, such as 0.8 or 40", "a config declares %d marked windows at most", "a marked window has a kind and a title",
		"label %s of a marked window has one value",
		unknownIn["team"], unknownIn["window"], unknownIn["report"], unknownIn["grant"], unknownIn["goal"], unknownIn["marked"],
		wantA["team"], wantA["window"], wantA["report"], wantA["grant"], wantA["goal"], wantA["marked"],
	}
)

// configError is what is wrong with a --config file, kept as a text to
// translate and its values: the line, and why.
type configError struct {
	line   int
	format string
	args   []any
}

func (e configError) Error() string {
	return fmt.Sprintf("line %d: %s", e.line, fmt.Sprintf(e.format, e.args...))
}

func badAt(n *yaml.Node, format string, args ...any) error {
	return configError{n.Line, format, args}
}

// configText is what is wrong with a --config file, in a language.
func configText(lang string, err error) string {
	var ce configError
	var de ledger.DeclaredError
	switch {
	case errors.As(err, &ce):
		return tr(lang, "line %d: %s", ce.line, tr(lang, ce.format, ce.args...))
	case errors.As(err, &de) && de.Name != "":
		return tr(lang, "line %d (%s): %s", de.Line, de.Name, message(lang, de.Err))
	case errors.As(err, &de):
		return tr(lang, "line %d: %s", de.Line, message(lang, de.Err))
	}
	return message(lang, err)
}

// ReadConfig reads a --config file: what it declares, times without a
// zone in loc, and the hash of what it holds.
func ReadConfig(path string, loc *time.Location) (ledger.Declared, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return ledger.Declared{}, "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfig+1))
	if err != nil {
		return ledger.Declared{}, "", err
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	if len(b) > maxConfig {
		return ledger.Declared{}, hash, bad("the file is more than %d bytes", maxConfig)
	}
	d, err := ParseConfig(b, loc)
	return d, hash, err
}

// ParseConfig reads what a --config file declares, strictly: a key it
// doesn't know, a value of the wrong kind, or one given twice is an error
// naming its line. Times without a zone are loc's, as the UI's forms take
// them.
func ParseConfig(b []byte, loc *time.Location) (ledger.Declared, error) {
	var d ledger.Declared
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(b))
	// Nothing is no file: an empty or truncated one, read as a ConfigMap
	// is swapped, would take away all it declared, and with
	// --config-prune delete it.
	empty := bad("the file declares nothing: an empty file would take away all it declared; to declare nothing, give teams: []")
	if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
		return d, empty
	} else if err != nil {
		return d, bad("the file isn't YAML: %s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var more yaml.Node
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return d, badAt(&more, "one document only: --config is one file of teams, windows, reports, grants, goals and marked")
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if null(root) {
		return d, empty
	}
	err := each(root, "", func(key string, v *yaml.Node) error {
		// A key with nothing after it, as a file cut short reads: not a
		// list of none, which [] says.
		if null(v) {
			return badAt(v, "%s has nothing after it: give %s: [] for none", key, key)
		}
		if v.Kind != yaml.SequenceNode {
			return badAt(v, "%s is a list", key)
		}
		for _, item := range v.Content {
			var err error
			switch key {
			case "teams":
				err = parseTeam(item, &d)
			case "windows":
				err = parseWindow(item, loc, &d)
			case "reports":
				err = parseReport(item, &d)
			case "grants":
				err = parseGrant(item, &d)
			case "goals":
				err = parseGoal(item, &d)
			case "marked":
				err = parseMarkedConfig(item, loc, &d)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return d, err
	}
	// As many as the ledger keeps, at most: read here, each is checked
	// before the write, which no file holds long.
	switch {
	case len(d.Teams) > ledger.MaxDeclaredTeams:
		err = bad("a config declares %d teams at most", ledger.MaxDeclaredTeams)
	case len(d.Windows) > ledger.MaxWindows:
		err = bad("a config declares %d quiet windows at most", ledger.MaxWindows)
	case len(d.Reports) > ledger.MaxScheduled:
		err = bad("a config declares %d reports at most", ledger.MaxScheduled)
	case len(d.Grants) > ledger.MaxGrants:
		err = bad("a config declares %d roles at most", ledger.MaxGrants)
	case len(d.Marked) > ledger.MaxMarkedConfig:
		err = bad("a config declares %d marked windows at most", ledger.MaxMarkedConfig)
	case len(d.Goals) > ledger.MaxGoals:
		err = bad("a config declares %d goals at most", ledger.MaxGoals)
	}
	if err == nil {
		err = checkGoals(d.Goals)
	}
	return d, err
}

// null says a node holds nothing: empty, or null.
func null(n *yaml.Node) bool {
	return n.Kind == 0 || n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// each calls fn with each key of a mapping and its value, refusing a key
// that what (a key of configKeys) doesn't have. yaml refuses a key given
// twice.
func each(n *yaml.Node, what string, fn func(key string, v *yaml.Node) error) error {
	if n.Kind != yaml.MappingNode {
		if what == "" {
			return badAt(n, "want a mapping of teams, windows, reports, grants, goals and marked")
		}
		return badAt(n, wantA[what], strings.Join(configKeys[what], ", "))
	}
	seen := map[string]int{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if line, ok := seen[k.Value]; ok && k.Kind == yaml.ScalarNode {
			return badAt(k, "%s is given twice, on line %d too", k.Value, line)
		}
		seen[k.Value] = k.Line
		if k.Kind != yaml.ScalarNode || !slices.Contains(configKeys[what], k.Value) {
			if what == "" {
				return badAt(k, "unknown key %q: want teams, windows, reports, grants, goals or marked", k.Value)
			}
			return badAt(k, unknownIn[what], k.Value, strings.Join(configKeys[what], ", "))
		}
		if what != "" && (null(v) || blank(v) && broadens(what, k.Value)) {
			return nothingAfter(what, k.Value, v)
		}
		if err := fn(k.Value, v); err != nil {
			return err
		}
	}
	return nil
}

// blank says a scalar is empty, or blanks alone: "" or ' '.
func blank(v *yaml.Node) bool {
	return v.Kind == yaml.ScalarNode && strings.TrimSpace(v.Value) == ""
}

// broadens says a key of a thing, given as nothing, would make it hold for
// more: a role's, a quiet window's, a report's or a goal's team, every
// team or everyone; a quiet window's rules, every rule.
func broadens(what, key string) bool {
	return key == "team" || what == "window" && key == "rules"
}

// nothingAfter refuses a key of a thing given with nothing after it, as a
// file cut short reads, or a value left out: read as nothing, a role's or
// a quiet window's team would be every team, a quiet window's rules every
// rule. Left out, the key says so.
func nothingAfter(what, key string, v *yaml.Node) error {
	switch {
	case key == "team":
		return badAt(v, "team has nothing after it: leave the key out for every team")
	case what == "window" && key == "rules":
		return badAt(v, "rules has nothing after it: leave the key out for every rule")
	}
	return badAt(v, "%s has nothing after it: give it a value, or leave the key out", key)
}

// text is a scalar's text: a string, or a number or a word as written.
func text(key string, v *yaml.Node) (string, error) {
	if null(v) {
		return "", nil
	}
	if v.Kind != yaml.ScalarNode {
		return "", badAt(v, "%s is a text", key)
	}
	return v.Value, nil
}

// name is a scalar's text that names something (a team, a window, a
// report, a person or a group): as written, a string. One YAML reads as a
// boolean or a number is refused, as Helm, reading values as YAML 1.1,
// gives it as one: no is false, 0123 is 83, 1.10 is 1.1.
func name(key string, v *yaml.Node) (string, error) {
	s, err := text(key, v)
	if err == nil && s != "" && notText(v) {
		err = badAt(v, "%s is %s, read as a boolean or a number, not a text: quote it, or give serve.config as a text", key, s)
	}
	return s, err
}

// notText says a scalar, unquoted, is read as a boolean or a number.
func notText(v *yaml.Node) bool {
	switch v.ShortTag() {
	case "!!bool", "!!int", "!!float":
		return v.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) == 0
	}
	return false
}

// texts are a list of scalars' texts; a single one is a list of one.
func texts(key string, v *yaml.Node) ([]string, error) {
	if null(v) {
		return nil, nil
	}
	if v.Kind == yaml.ScalarNode {
		return []string{v.Value}, nil
	}
	if v.Kind != yaml.SequenceNode {
		return nil, badAt(v, "%s is a list of texts", key)
	}
	var out []string
	for _, x := range v.Content {
		if x.Kind != yaml.ScalarNode || null(x) {
			return nil, badAt(x, "%s is a list of texts", key)
		}
		out = append(out, x.Value)
	}
	return out, nil
}

// labels are labels and the values each may have: name: value, or name:
// [value, value].
func labels(v *yaml.Node) (incident.Labels, error) {
	if null(v) {
		return nil, nil
	}
	if v.Kind != yaml.MappingNode {
		return nil, badAt(v, "labels are a mapping of a label's name to a value, or a list of values")
	}
	out := incident.Labels{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, x := v.Content[i], v.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.Value == "" {
			return nil, badAt(k, "a label's name is a text")
		}
		vs, err := texts("a label's values", x)
		if err != nil {
			return nil, err
		}
		// A value read as a boolean or a fraction isn't as written: on is
		// true, 1.10 is 1.1. A whole number is, as a status code is.
		for _, y := range append([]*yaml.Node{x}, x.Content...) {
			if y.Kind == yaml.ScalarNode && (y.ShortTag() == "!!bool" || y.ShortTag() == "!!float") && notText(y) {
				return nil, badAt(y, "label %s's value %s is read as a boolean or a number, not a text: quote it, or give serve.config as a text", k.Value, y.Value)
			}
		}
		if len(vs) == 0 {
			return nil, badAt(x, "label %s has no value", k.Value)
		}
		out[k.Value] = vs
	}
	return out, nil
}

// when reads a time as the UI's forms and the API take one: 2026-11-02T09:30
// in loc, or RFC 3339.
func when(key string, v *yaml.Node, loc *time.Location) (time.Time, error) {
	s, err := text(key, v)
	if err != nil || s == "" {
		return time.Time{}, err
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, badAt(v, "%s %q: want a time like 2026-11-02T09:30, or 2026-11-02T09:30:00+09:00", key, s)
}

func parseTeam(n *yaml.Node, d *ledger.Declared) error {
	dt := ledger.DeclaredTeam{Line: n.Line}
	t := &dt.Team
	err := each(n, "team", func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "name":
			t.Name, err = name(key, v)
		case "renamed_from":
			dt.RenamedFrom, err = name(key, v)
		case "rules":
			t.Rules, err = text(key, v)
			t.Rules = strings.TrimSpace(t.Rules)
		case "labels":
			t.Labels, err = labels(v)
		case "note":
			t.Note, err = text(key, v)
			t.Note = strings.TrimSpace(t.Note)
		case "work_hours":
			t.WorkHours, err = text(key, v)
		case "sleep_hours":
			t.SleepHours, err = text(key, v)
		case "tz":
			t.TZ, err = text(key, v)
		case "shifts":
			t.Shifts, err = text(key, v)
		}
		return err
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(t.Name) == "" {
		return badAt(n, "a team has a name")
	}
	d.Teams = append(d.Teams, dt)
	return nil
}

func parseWindow(n *yaml.Node, loc *time.Location, d *ledger.Declared) error {
	dw := ledger.DeclaredWindow{Line: n.Line}
	w := &dw.Window
	err := each(n, "window", func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "title":
			w.Title, err = name(key, v)
		case "team":
			dw.Team, err = name(key, v)
		case "start":
			w.Start, err = when(key, v, loc)
		case "end":
			w.End, err = when(key, v, loc)
		case "every":
			w.Every, err = text(key, v)
		case "repeat_on":
			w.On, err = text(key, v)
		case "until":
			// A day, as the form's: its start in loc; or a time.
			var s string
			if s, err = text(key, v); err == nil && s != "" {
				if day, derr := time.ParseInLocation("2006-01-02", s, loc); derr == nil {
					w.Until = midnight(day, loc)
				} else {
					w.Until, err = when(key, v, loc)
				}
			}
		case "rules":
			w.Rules, err = text(key, v)
			w.Rules = strings.TrimSpace(w.Rules)
		case "labels":
			w.Labels, err = labels(v)
		}
		return err
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(w.Title) == "" {
		return badAt(n, "a quiet window has a title")
	}
	d.Windows = append(d.Windows, dw)
	return nil
}

// parseMarkedConfig reads a marked window --config declares, as
// parseWindow a quiet window: its until a day, or a time; each label of
// one value.
func parseMarkedConfig(n *yaml.Node, loc *time.Location, d *ledger.Declared) error {
	dm := ledger.DeclaredMarked{Line: n.Line}
	m := &dm.Marked
	err := each(n, "marked", func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "kind":
			m.Kind, err = name(key, v)
		case "title":
			m.Title, err = name(key, v)
		case "team":
			dm.Team, err = name(key, v)
		case "start":
			m.Start, err = when(key, v, loc)
		case "end":
			m.End, err = when(key, v, loc)
		case "every":
			m.Every, err = text(key, v)
		case "repeat_on":
			m.On, err = text(key, v)
		case "until":
			var s string
			if s, err = text(key, v); err == nil && s != "" {
				if day, derr := time.ParseInLocation("2006-01-02", s, loc); derr == nil {
					m.Until = midnight(day, loc)
				} else {
					m.Until, err = when(key, v, loc)
				}
			}
		case "labels":
			var ls incident.Labels
			if ls, err = labels(v); err == nil && len(ls) > 0 {
				m.Labels = map[string]string{}
				for k, vs := range ls {
					if len(vs) != 1 {
						return badAt(v, "label %s of a marked window has one value", k)
					}
					m.Labels[k] = vs[0]
				}
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(m.Kind) == "" || strings.TrimSpace(m.Title) == "" {
		return badAt(n, "a marked window has a kind and a title")
	}
	d.Marked = append(d.Marked, dm)
	return nil
}

// midnight is when a day starts in loc, as the ledger's Midnight has it:
// where the clocks skip midnight, the first hour of the day.
func midnight(day time.Time, loc *time.Location) time.Time {
	y, m, d := day.Date()
	want := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	t := time.Date(y, m, d, 0, 0, 0, 0, loc)
	for {
		ty, tm, td := t.In(loc).Date()
		if !time.Date(ty, tm, td, 0, 0, 0, 0, time.UTC).Before(want) {
			return t.UTC()
		}
		t = t.Add(time.Hour)
	}
}

func parseReport(n *yaml.Node, d *ledger.Declared) error {
	dr := ledger.DeclaredReport{Line: n.Line}
	r := &dr.Scheduled
	r.Enabled = true // unless it says otherwise, as the API takes one
	err := each(n, "report", func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "name":
			r.Name, err = name(key, v)
		case "team":
			dr.Team, err = name(key, v)
		case "schedule":
			r.Schedule, err = text(key, v)
		case "span":
			r.Span, err = text(key, v)
			r.Span = strings.TrimSpace(r.Span)
		case "zone":
			r.Zone, err = text(key, v)
		case "sections":
			r.Sections, err = texts(key, v)
		case "dests":
			r.Dests, err = texts(key, v)
		case "enabled":
			var s string
			if s, err = text(key, v); err == nil {
				if r.Enabled, err = strconv.ParseBool(s); err != nil {
					err = badAt(v, "enabled is true or false")
				}
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(r.Name) == "" {
		return badAt(n, "a report has a name")
	}
	d.Reports = append(d.Reports, dr)
	return nil
}

func parseGrant(n *yaml.Node, d *ledger.Declared) error {
	dg := ledger.DeclaredGrant{Line: n.Line}
	var user, group string
	err := each(n, "grant", func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "user":
			user, err = name(key, v)
		case "group":
			group, err = name(key, v)
		case "team":
			dg.Team, err = name(key, v)
		}
		return err
	})
	if err != nil {
		return err
	}
	switch {
	case (user == "") == (group == ""):
		return badAt(n, "a role is given to a user or a group: one of user and group")
	case user != "":
		dg.Who = "user:" + user
	default:
		dg.Who = "group:" + group
	}
	d.Grants = append(d.Grants, dg)
	return nil
}

// parseGoal reads a goal (docs/design/goals.md). Its numbers are kept as
// written, and checked with the rest of it once the file is read
// (checkGoals): a goal that breaks a rule refuses the file, with its line.
func parseGoal(n *yaml.Node, d *ledger.Declared) error {
	dg := ledger.DeclaredGoal{Line: n.Line}
	g := &dg.Goal
	number := func(key string, v *yaml.Node) (string, error) {
		s, err := text(key, v)
		if err == nil && (v.Kind != yaml.ScalarNode || v.ShortTag() == "!!bool") {
			err = badAt(v, "%s is a number, such as 0.8 or 40", key)
		}
		return strings.TrimSpace(s), err
	}
	err := each(n, "goal", func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "name":
			g.Name, err = name(key, v)
		case "team":
			dg.Team, err = name(key, v)
		case "measure":
			g.Measure, err = text(key, v)
		case "at_least":
			g.AtLeast, err = number(key, v)
		case "at_most":
			g.AtMost, err = number(key, v)
		case "by":
			g.By, err = number(key, v)
		case "no_worse_than":
			g.NoWorseThan, err = text(key, v)
		case "critical":
			g.Critical, err = texts(key, v)
		case "each":
			g.Each, err = text(key, v)
		case "stones":
			g.Stones, err = text(key, v)
		}
		return err
	})
	if err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(g.Name) == "":
		return badAt(n, "a goal has a name")
	case strings.TrimSpace(g.Measure) == "":
		return badAt(n, "a goal has a measure")
	}
	d.Goals = append(d.Goals, dg)
	return nil
}

// checkGoals refuses goals that break a rule of the design's, each with its
// line: on its own (ledger.CheckGoal), a name given twice, or more lists
// of severities than the goals may count as critical.
func checkGoals(gs []ledger.DeclaredGoal) error {
	seen := map[string]int{}
	var all []ledger.Goal
	for i := range gs {
		g := &gs[i]
		if err := ledger.CheckGoal(&g.Goal); err != nil {
			return atLine(g.Line, err)
		}
		if line, ok := seen[g.Name]; ok {
			return configError{g.Line, "a goal called %s is declared on line %d already: a goal's name is unique, across everyone's and every team's", []any{g.Name, line}}
		}
		seen[g.Name] = g.Line
		all = append(all, g.Goal)
		if err := ledger.CheckGoals(all); err != nil {
			return atLine(g.Line, err)
		}
	}
	return nil
}

// atLine is the ledger's refusal of something on a line of the file, as
// the file's own are said.
func atLine(line int, err error) error {
	var inv ledger.ErrInvalid
	if errors.As(err, &inv) {
		return configError{line, inv.Format, inv.Args}
	}
	return err
}

// checkDeclared checks what the ledger doesn't of what --config declares,
// as the UI's forms are checked: each report's destinations, which the
// flags name, and each team's on-call shifts, which --on-call names. One
// refused is Bad: skipped, and kept as it was, as one the ledger refuses.
func (s *Server) checkDeclared(d *ledger.Declared) {
	for i := range d.Teams {
		if err := s.checkShifts(d.Teams[i].Shifts); err != nil {
			d.Teams[i].Bad = err
		}
	}
	for i := range d.Reports {
		if err := s.knowsDests(d.Reports[i].Scheduled); err != nil {
			d.Reports[i].Bad = err
		}
	}
}

// configProblem is something wrong with --config: the file as a whole
// (Kind "file"), or a thing it declares that can't be kept, of a kind
// (team, window, report, grant), on a line. Text is why, in each language,
// for admins: it can name a role's holder or a report's destination.
type configProblem struct {
	Line int               `json:"line,omitempty"`
	Kind string            `json:"kind"`
	Text map[string]string `json:"text,omitempty"`
}

// briefs are what a problem says to a reader who isn't an admin: its line
// and kind alone.
var briefs = map[string]string{
	"file":            "line %d: the file can't be read as --config",
	ledger.KindTeam:   "line %d: a team that can't be kept",
	ledger.KindWindow: "line %d: a quiet window that can't be kept",
	ledger.KindReport: "line %d: a report that can't be kept",
	ledger.KindGrant:  "line %d: a role that can't be kept",
	ledger.KindGoal:   "line %d: a goal that can't be kept",
	ledger.KindMarked: "line %d: a marked window that can't be kept",
}

// Shown is the problem in a language: why, for an admin; its line and
// kind, for anyone else.
func (p configProblem) Shown(lang string) string {
	if t, ok := p.Text[lang]; ok {
		return t
	}
	if t, ok := p.Text["en"]; ok {
		return t
	}
	switch {
	case p.Line == 0 && p.Kind == "file":
		return tr(lang, "the file can't be read as --config")
	case p.Line == 0:
		// What the file no longer names, which couldn't be let go.
		return tr(lang, "a team the file no longer declares, kept declared")
	}
	return tr(lang, briefs[p.Kind], p.Line)
}

// problemOf is what is wrong, as a problem.
func problemOf(err error) configProblem {
	p := configProblem{Kind: "file", Text: map[string]string{}}
	var de ledger.DeclaredError
	var ce configError
	switch {
	case errors.As(err, &de):
		p.Kind, p.Line = de.Kind, de.Line
	case errors.As(err, &ce):
		p.Line = ce.line
	}
	for _, l := range Langs() {
		p.Text[l] = configText(l, err)
	}
	return p
}

// configProblemsKept is how many problems are kept to be shown, the first;
// the rest are counted.
const configProblemsKept = 50

// configState is what the leader last made of --config, in its term: the
// file's hash it last kept the ledger to (applied), when, what the last
// that wrote did, and what of it couldn't be kept; a file that couldn't be
// used as a whole (failed); another read once (pending, at pendingAt), to
// be kept to once it is read the same again; and when to try again a file
// kept in part, or after an error of the ledger's.
type configState struct {
	sync.Mutex
	applied, failed, pending string
	at, pendingAt            time.Time
	// last is the hash last read. pendingSince is when a file other than
	// the one applied was first read, since it was kept to: none kept to
	// since, it is said (stuckLogged, once configStuck on). backAt is when
	// the one applied was read again after another, which, read the same
	// again, ends the wait. termAt is when this term began.
	last                         string
	pendingSince, backAt, termAt time.Time
	stuckLogged                  bool
	did                          ledger.Reconciled
	// shown are the problems the pages show; kept, those of the file
	// applied, shown again once a file that failed is put back.
	shown, kept configProblems
	// retryHash, kept in part, is tried again from retryAt, after backoff,
	// once the ledger changed since triedGen.
	retryHash string
	retryAt   time.Time
	backoff   time.Duration
	triedGen  int64
	// failHash met an error of the ledger's, at failedAt, and isn't tried
	// again before failAt, after failBackoff, whatever else would read it
	// again.
	failHash         string
	failedAt, failAt time.Time
	failBackoff      time.Duration
	errAt            time.Time
	loggedText       string
}

// configProblems are what is wrong with a file: the first kept, how many
// more, and whether it couldn't be used at all.
type configProblems struct {
	list  []configProblem
	more  int
	whole bool
}

func problemsOf(errs []ledger.DeclaredError) configProblems {
	var ps configProblems
	for i, e := range errs {
		if i >= configProblemsKept {
			ps.more = len(errs) - i
			break
		}
		ps.list = append(ps.list, problemOf(e))
	}
	return ps
}

// newTerm forgets what an earlier term made of the file: another leader
// may have kept the ledger to another since, or a failover lost what this
// one wrote. Kept again to the same file, the ledger writes nothing. A
// file the last term read once, that began within configStuck of now, is
// still waited for (pendingSince): terms too short to read a file twice
// keep the ledger from every file, and that is said.
func (c *configState) newTerm(now time.Time) {
	c.Lock()
	defer c.Unlock()
	short := c.pending != "" && now.Sub(c.termAt) < configStuck
	c.applied, c.failed, c.pending, c.retryHash, c.retryAt, c.backoff = "", "", "", "", time.Time{}, 0
	c.failHash, c.failedAt, c.failAt, c.failBackoff = "", time.Time{}, time.Time{}, 0
	c.last, c.backAt, c.termAt = "", time.Time{}, now
	if !short {
		c.pendingSince, c.stuckLogged = time.Time{}, false
	}
}

// configShown is what the pages say of --config, as the leader last made
// of it (sharedStatus): its hash cut short. Brief says Text is left out,
// for a reader who isn't an admin.
type configShown struct {
	File     string            `json:"file,omitempty"`
	Prune    bool              `json:"prune,omitempty"`
	Hash     string            `json:"hash,omitempty"`
	Applied  time.Time         `json:"applied,omitzero"`
	Did      ledger.Reconciled `json:"did,omitzero"`
	Whole    bool              `json:"whole,omitempty"`
	Problems []configProblem   `json:"problems,omitempty"`
	More     int               `json:"more,omitempty"`
	ErrAt    time.Time         `json:"err_at,omitzero"`
	Brief    bool              `json:"brief,omitempty"`
	// Pending is when a new file was read, once: the ledger is kept to it
	// once it is read the same again. PendingSince is when files other
	// than the one applied began to be read, none kept to since, said
	// when that is more than the one read once, and not of a file read
	// twice that the ledger failed to keep to. WriteFailed is when the
	// ledger couldn't be kept to the file last read, tried again at Retry;
	// Again, the file it was kept to, tried again for what couldn't be
	// kept of it.
	Pending      time.Time `json:"pending,omitzero"`
	PendingSince time.Time `json:"pending_since,omitzero"`
	WriteFailed  time.Time `json:"write_failed,omitzero"`
	Retry        time.Time `json:"retry,omitzero"`
	Again        bool      `json:"again,omitempty"`
}

// ErrText is what is wrong, in a language, "" for nothing.
func (c *configShown) ErrText(lang string) string {
	if c == nil {
		return ""
	}
	var out []string
	for _, p := range c.Problems {
		out = append(out, p.Shown(lang))
	}
	return strings.Join(out, "; ")
}

// forReader is c as a reader is shown it: why each problem is, and the
// file's path, which says how serve is run, as the other files' paths do
// (redactPaths), for an admin alone.
func (c *configShown) forReader(admin bool) *configShown {
	if c == nil || admin {
		return c
	}
	b := *c
	b.File, b.Brief, b.Problems = "", true, nil
	for _, p := range c.Problems {
		b.Problems = append(b.Problems, configProblem{Line: p.Line, Kind: p.Kind})
	}
	return &b
}

// configFor is what --config came to, as the leader last said it, as the
// reader is shown it.
func (s *Server) configFor(r *http.Request) *configShown {
	return s.shownStatus().Config.forReader(s.role(r) == admin)
}

// configShown is this replica's: without --config, nil.
func (s *Server) configShown() *configShown {
	if s.cfg.Config == "" {
		return nil
	}
	s.config.Lock()
	defer s.config.Unlock()
	c := &configShown{File: s.cfg.Config, Prune: s.cfg.ConfigPrune, Did: s.config.did, ErrAt: s.config.errAt,
		Whole: s.config.shown.whole, Problems: s.config.shown.list, More: s.config.shown.more}
	st := &s.config
	failing := st.failHash != "" && st.pending == st.failHash // read twice, the write failing
	if st.failHash != "" && st.failHash == st.last {
		c.WriteFailed, c.Retry, c.Again = st.failedAt, st.failAt, st.failHash == st.applied
	}
	if st.pending != "" && st.pending != st.failHash {
		c.Pending = st.pendingAt
	}
	if !st.pendingSince.IsZero() && !failing && (c.Pending.IsZero() || st.pendingSince.Before(st.pendingAt)) {
		c.PendingSince = st.pendingSince
	}
	if len(s.config.applied) >= 12 {
		c.Hash, c.Applied = s.config.applied[:12], s.config.at
	}
	return c
}

// noConfig is the hash kept for no --config: nothing declared.
const noConfig = "none"

// configBackoffMost is the longest a file kept in part, or the ledger's
// error, waits to be tried again.
const configBackoffMost = 10 * time.Minute

// configStuck is how long files other than the one applied are read, none
// twice in a row, before it is logged: a ConfigMap that changes back and
// forth, or terms too short to read a file twice.
const configStuck = 5 * time.Minute

// configTerm keeps the ledger to --config as a term of leading begins:
// whatever an earlier term kept it to.
func (s *Server) configTerm(ctx context.Context) error {
	s.config.newTerm(s.wall())
	return s.reconcileConfig(ctx)
}

// reconcileConfig keeps the ledger holding what --config declares, as the
// file now says it, when it changed since the ledger was last kept to it;
// without --config, nothing declared, so that what a file declared before
// is the UI's again. The leader's alone (lead), in one write its lease
// fences: what replicas read of the file can differ for a minute as
// Kubernetes updates each one's copy, and two writing by turns would undo
// each other.
//
// A file is kept to only once it has been read the same twice in a row,
// configEvery apart at least, with no other read between: the first read
// of it is noted, and nothing changed. A file read half written, or a
// ConfigMap swapped in part, changes nothing, then: no one is given a
// role, nor taken one away, nothing is deleted, and no quiet window
// quiets more. Kept to, what it no longer declares is let go at once. As
// a term begins, the file is read once, and kept to at the next read,
// configEvery after the rules are read (lead): no sooner, and no bound on
// how much later, as a ConfigMap that changes back and forth, or terms
// too short, keep any file from being read twice; that is shown, and
// logged once configStuck on.
//
// A file that can't be read as --config changes nothing, and is said at
// once why, on its first read. Kept to, each thing it declares that can't
// be kept is skipped, and kept as it was, and the rest is kept; such a
// file is tried again, as the ledger changes (a team it names made in the
// UI, or made by the write itself), from configEvery to configBackoffMost
// after. An error of the ledger's is returned, and the file tried again
// after a while.
func (s *Server) reconcileConfig(ctx context.Context) error {
	d, hash := ledger.Declared{}, noConfig
	var fileErr error
	if s.cfg.Config != "" {
		d, hash, fileErr = ReadConfig(s.cfg.Config, s.cfg.Loc)
		if hash == "" { // not read at all
			hash = "unread"
		}
	}
	now, gen := s.wall(), s.l.Gen()
	st := &s.config
	st.Lock()
	st.last = hash
	// waiting gives the lock back, for a read that keeps the ledger to
	// nothing, and says once that files have been read for configStuck,
	// none twice in a row.
	waiting := func() {
		var stuck time.Time
		// A file read twice that the ledger fails to keep to isn't one
		// waited for: the write's error is logged, and shown.
		failing := st.failHash != "" && st.pending == st.failHash
		if !st.pendingSince.IsZero() && !now.Before(st.pendingSince.Add(configStuck)) && !st.stuckLogged && !failing {
			st.stuckLogged, stuck = true, st.pendingSince
		}
		st.Unlock()
		if !stuck.IsZero() {
			s.log.Warn("--config has changed, and no file has been read the same twice in a row since; the ledger keeps what it was last kept to", "file", s.cfg.Config, "since", stuck)
		}
	}
	switch {
	case hash == st.failHash && now.Before(st.failAt):
		// The ledger's error, this file met last: not before a while. Read
		// between two of another, it makes them not in a row.
		if st.pending != hash {
			st.pending, st.backAt = "", time.Time{}
		}
		waiting()
		return nil
	case fileErr != nil:
		// Said at once, on its first read; and between two reads of a
		// file, it makes them not in a row.
		st.pending, st.backAt = "", time.Time{}
		again := hash == st.failed
		waiting()
		if !again {
			s.configSaid(configProblems{list: []configProblem{problemOf(fileErr)}, whole: true}, func() { st.failed = hash })
		}
		return nil
	case hash == st.applied:
		// The file the ledger is kept to: another read once since is read
		// twice in a row no more; read the same twice in a row itself, no
		// other is waited for.
		if st.pending != "" || st.backAt.IsZero() {
			st.pending, st.backAt = "", now
		} else if !now.Before(st.backAt.Add(configEvery)) {
			st.pendingSince, st.stuckLogged = time.Time{}, false
		}
		back, kept := st.failed != "", st.kept
		retry := len(st.kept.list) > 0 && gen != st.triedGen && !now.Before(st.retryAt)
		waiting()
		if back {
			// Back to it after one that couldn't be used: what is wrong
			// with it is what is shown again.
			s.configSaid(kept, func() { st.failed = "" })
		}
		if !retry {
			return nil
		}
		st.Lock()
	case s.cfg.Config == "":
		// No file to read twice: what one declared is given back at once,
		// and nothing taken away.
	case hash != st.pending:
		// Read once: kept to once it is read the same again.
		st.pending, st.pendingAt, st.backAt = hash, now, time.Time{}
		if st.pendingSince.IsZero() {
			st.pendingSince = now
		}
		waiting()
		return nil
	case now.Before(st.pendingAt.Add(configEvery)):
		waiting()
		return nil
	}
	if hash != st.retryHash {
		// Another file: tried from the start.
		st.backoff, st.retryAt = 0, time.Time{}
	}
	st.Unlock()
	// An error of the ledger's: the file is tried again, after a while,
	// still read twice.
	later := func(err error) error {
		st.Lock()
		defer st.Unlock()
		if st.failHash != hash {
			st.failBackoff = 0
		}
		st.failHash, st.failBackoff = hash, nextBackoff(st.failBackoff)
		st.failedAt, st.failAt = now, now.Add(st.failBackoff)
		return err
	}
	s.checkDeclared(&d)
	o, err := s.options(ctx)
	if err != nil {
		return later(err)
	}
	r, skipped, err := s.l.Reconcile(ctx, d, ledger.Leave{Prune: s.cfg.ConfigPrune, NoFile: s.cfg.Config == ""}, o)
	if err != nil {
		return later(err)
	}
	st.Lock()
	st.applied, st.at, st.failed, st.pending = hash, now, "", ""
	st.failHash, st.failedAt, st.failAt, st.failBackoff = "", time.Time{}, time.Time{}, 0
	st.pendingSince, st.backAt, st.stuckLogged = time.Time{}, time.Time{}, false
	// The ledger as it was before this write: what the write itself made
	// (a team made once others were let go) is a change, and what names
	// it is tried again.
	st.triedGen, st.retryHash = gen, hash
	if len(skipped) > 0 {
		st.backoff = nextBackoff(st.backoff)
		st.retryAt = now.Add(st.backoff)
	} else {
		st.backoff, st.retryAt = 0, time.Time{}
	}
	if r.Any() {
		st.did = r
	}
	st.Unlock()
	ps := problemsOf(skipped)
	s.configSaid(ps, func() { st.kept = ps })
	if r.Any() {
		s.poke() // the windows changed are judged again
		s.log.Info("the ledger keeps what --config declares", "file", s.cfg.Config, "made", r.Made, "changed", r.Changed,
			"taken over", r.Adopted, "given back to the UI", r.Undeclared, "deleted", r.Deleted, "kept declared", r.Kept)
	}
	return nil
}

// configSaid shows what is wrong with the file, and logs it once, with
// what else is set beside it.
func (s *Server) configSaid(ps configProblems, also func()) {
	st := &s.config
	st.Lock()
	defer st.Unlock()
	also()
	st.shown = ps
	var texts []string
	for _, p := range ps.list {
		texts = append(texts, p.Shown("en"))
	}
	text := strings.Join(texts, "; ")
	if text == st.loggedText {
		return
	}
	st.loggedText, st.errAt = text, time.Now()
	switch {
	case text == "":
	case ps.whole:
		s.log.Error("--config can't be used; the ledger keeps what it last declared", "file", s.cfg.Config, "err", text)
	default:
		s.log.Warn("--config declares what can't be kept; each is kept as it was", "file", s.cfg.Config, "skipped", len(ps.list)+ps.more, "err", text)
	}
}

// nextBackoff is how long a file is left before it is tried again, doubled
// each time from configEvery.
func nextBackoff(b time.Duration) time.Duration {
	if b <= 0 {
		return configEvery
	}
	return min(2*b, configBackoffMost)
}

// watchConfig reads --config again every configEvery, until ctx is done,
// and keeps the ledger to it when it changed: the leader's (lead).
func (s *Server) watchConfig(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(configEvery):
		}
		if err := s.reconcileConfig(ctx); err != nil && !steppingDown(ctx, err) {
			s.log.Error("keeping the ledger to --config", "err", err)
		}
	}
}

// postKeepGivenBack keeps what a leader without --config gave back as the
// UI's, so that no file takes it away: one (kind and id), or all of them
// (all=1). An admin's, as only an admin sees which they are.
func (s *Server) postKeepGivenBack(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	kind, id := "", int64(0)
	if r.PostFormValue("all") != "1" {
		var err error
		kind = r.PostFormValue("kind")
		if id, err = strconv.ParseInt(r.PostFormValue("id"), 10, 64); err != nil || kind == "" {
			s.oops(w, r, bad("say which to keep, by its kind and id, or keep them all"))
			return
		}
	}
	if _, err := s.l.KeepGivenBack(r.Context(), kind, id, s.user(r)); err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, "/settings#config")
}
