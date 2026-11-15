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
	"":     {"teams", "windows", "reports", "grants"},
	"team": {"name", "rules", "labels", "note", "work_hours", "sleep_hours", "tz", "shifts", "renamed_from"},
	// A window's on is repeat_on: YAML 1.1, which Helm reads values by,
	// takes a key on, unquoted, for true.
	"window": {"title", "team", "start", "end", "every", "repeat_on", "until", "rules", "labels"},
	"report": {"name", "team", "schedule", "span", "zone", "sections", "dests", "enabled"},
	"grant":  {"user", "group", "team"},
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
	}
	wantA = map[string]string{
		"team":   "want a team, as a mapping of %s",
		"window": "want a quiet window, as a mapping of %s",
		"report": "want a report, as a mapping of %s",
		"grant":  "want a role, as a mapping of %s",
	}
	configFormats = []string{
		"line %d: %s", "line %d (%s): %s", "the file isn't YAML: %s", "the file is more than %d bytes",
		"one document only: --config is one file of teams, windows, reports and grants",
		"want a mapping of teams, windows, reports and grants", "unknown key %q: want teams, windows, reports or grants",
		"%s is given twice, on line %d too", "%s is a list", "%s is a text", "%s is a list of texts",
		"labels are a mapping of a label's name to a value, or a list of values", "a label's name is a text", "label %s has no value",
		"%s %q: want a time like 2026-11-02T09:30, or 2026-11-02T09:30:00+09:00", "a team has a name", "a quiet window has a title",
		"a report has a name", "enabled is true or false", "a role is given to a user or a group: one of user and group",
		"the file declares nothing: an empty file would take away all it declared; to declare nothing, give teams: []",
		"%s has nothing after it: give %s: [] for none",
		"%s is %s, read as a boolean or a number, not a text: quote it, or give serve.config as a text",
		"label %s's value %s is read as a boolean or a number, not a text: quote it, or give serve.config as a text",
		"a config declares %d teams at most", "a config declares %d quiet windows at most", "a config declares %d reports at most",
		"a config declares %d roles at most",
		unknownIn["team"], unknownIn["window"], unknownIn["report"], unknownIn["grant"],
		wantA["team"], wantA["window"], wantA["report"], wantA["grant"],
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
		return d, badAt(&more, "one document only: --config is one file of teams, windows, reports and grants")
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
			return badAt(n, "want a mapping of teams, windows, reports and grants")
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
				return badAt(k, "unknown key %q: want teams, windows, reports or grants", k.Value)
			}
			return badAt(k, unknownIn[what], k.Value, strings.Join(configKeys[what], ", "))
		}
		if err := fn(k.Value, v); err != nil {
			return err
		}
	}
	return nil
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
// used as a whole (failed); one kept with what it no longer declares kept
// too, to be deleted when it is read again (keptFor); and when to try
// again a file kept in part, or after an error of the ledger's.
type configState struct {
	sync.Mutex
	applied, failed, keptFor string
	at                       time.Time
	did                      ledger.Reconciled
	// shown are the problems the pages show; kept, those of the file
	// applied, shown again once a file that failed is put back.
	shown, kept configProblems
	// retryHash, kept in part, is tried again from retryAt, after backoff,
	// once the ledger changed since triedGen.
	retryHash string
	retryAt   time.Time
	backoff   time.Duration
	triedGen  int64
	// failHash met an error of the ledger's, and isn't tried again before
	// failAt, after failBackoff, whatever else would read it again.
	failHash    string
	failAt      time.Time
	failBackoff time.Duration
	errAt       time.Time
	loggedText  string
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
// one wrote. Kept again to the same file, the ledger writes nothing.
func (c *configState) newTerm() {
	c.Lock()
	defer c.Unlock()
	c.applied, c.failed, c.keptFor, c.retryHash, c.retryAt, c.backoff = "", "", "", "", time.Time{}, 0
	c.failHash, c.failAt, c.failBackoff = "", time.Time{}, 0
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

// forReader is c as a reader is shown it: why each problem is, for an
// admin alone.
func (c *configShown) forReader(admin bool) *configShown {
	if c == nil || admin {
		return c
	}
	b := *c
	b.Brief, b.Problems = true, nil
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

// configShown is this replica's: without --config, nil, or what it gave
// back to the UI that a file declared before.
func (s *Server) configShown() *configShown {
	if s.cfg.Config == "" {
		s.config.Lock()
		defer s.config.Unlock()
		if s.config.did.Undeclared == 0 {
			return nil
		}
		return &configShown{Did: s.config.did}
	}
	s.config.Lock()
	defer s.config.Unlock()
	c := &configShown{File: s.cfg.Config, Prune: s.cfg.ConfigPrune, Did: s.config.did, ErrAt: s.config.errAt,
		Whole: s.config.shown.whole, Problems: s.config.shown.list, More: s.config.shown.more}
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

// configTerm keeps the ledger to --config as a term of leading begins:
// whatever an earlier term kept it to.
func (s *Server) configTerm(ctx context.Context) error {
	s.config.newTerm()
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
// A file that can't be read as --config changes nothing, and is said once
// why. One read, each thing it declares that can't be kept is skipped, and
// kept as it was, and the rest is kept; such a file is tried again, as the
// ledger changes (a team it names made in the UI), from configEvery to
// configBackoffMost after. With --config-prune, what it no longer declares
// is kept until the file is read the same twice, then deleted: an empty or
// half-written file read once deletes nothing. An error of the ledger's is
// returned, and the file tried again after a while.
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
	// Read the same twice in a row: what the file no longer names may be
	// deleted, a role taken away. Without --config, there is no file to
	// say so: everything is given back, nothing deleted.
	sure := fileErr == nil && hash == st.keptFor && hash == st.applied
	left := ledger.Leave{Prune: s.cfg.ConfigPrune, Sure: sure, NoFile: s.cfg.Config == ""}
	switch {
	case hash == st.failHash && now.Before(st.failAt):
		// The ledger's error, this file met last: not before a while.
		st.Unlock()
		return nil
	case fileErr != nil && hash == st.failed:
		st.Unlock()
		return nil
	case fileErr != nil:
	case hash == st.applied:
		back := st.failed != ""
		if back {
			// Back to the file the ledger is kept to: what is wrong with it
			// is what is shown again, and it is read afresh, as one read
			// once, for what would be deleted.
			st.failed, st.shown, st.loggedText = "", st.kept, ""
		}
		retry := len(st.kept.list) > 0 && gen != st.triedGen && !now.Before(st.retryAt)
		if !sure && !retry && !back {
			st.Unlock()
			return nil
		}
	}
	if hash != st.retryHash {
		// Another file: tried from the start.
		st.backoff, st.retryAt = 0, time.Time{}
	}
	st.Unlock()
	if fileErr != nil {
		// Read the same twice in a row, a file is: one that can't be read
		// between two reads of it makes them two.
		s.configSaid(configProblems{list: []configProblem{problemOf(fileErr)}, whole: true}, func() { st.failed, st.keptFor = hash, "" })
		return nil
	}
	// An error of the ledger's: the file is tried again, after a while.
	later := func(err error) error {
		st.Lock()
		defer st.Unlock()
		if st.failHash != hash {
			st.failBackoff = 0
		}
		st.failHash, st.failBackoff = hash, nextBackoff(st.failBackoff)
		st.failAt = now.Add(st.failBackoff)
		return err
	}
	s.checkDeclared(&d)
	o, err := s.options(ctx)
	if err != nil {
		return later(err)
	}
	r, skipped, err := s.l.Reconcile(ctx, d, left, o)
	if err != nil {
		return later(err)
	}
	st.Lock()
	st.applied, st.at, st.failed, st.keptFor = hash, now, "", ""
	st.failHash, st.failAt, st.failBackoff = "", time.Time{}, 0
	if !sure && r.Kept > 0 {
		st.keptFor = hash // deleted, or taken away, once it is read the same again
	}
	st.triedGen, st.retryHash = s.l.Gen(), hash
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
			"taken over", r.Adopted, "given back to the UI", r.Undeclared, "deleted", r.Deleted, "kept to delete when read again", r.Kept)
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
