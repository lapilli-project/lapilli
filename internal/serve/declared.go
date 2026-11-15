package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	"":       {"teams", "windows", "reports", "grants"},
	"team":   {"name", "rules", "labels", "note", "work_hours", "sleep_hours", "tz", "shifts"},
	"window": {"title", "team", "start", "end", "every", "on", "until", "rules", "labels"},
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
	if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
		return d, nil
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
		return d, nil
	}
	err := each(root, "", func(key string, v *yaml.Node) error {
		if null(v) {
			return nil
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
			t.Name, err = text(key, v)
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
			w.Title, err = text(key, v)
		case "team":
			dw.Team, err = text(key, v)
		case "start":
			w.Start, err = when(key, v, loc)
		case "end":
			w.End, err = when(key, v, loc)
		case "every":
			w.Every, err = text(key, v)
		case "on":
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
			r.Name, err = text(key, v)
		case "team":
			dr.Team, err = text(key, v)
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
			user, err = text(key, v)
		case "group":
			group, err = text(key, v)
		case "team":
			dg.Team, err = text(key, v)
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
// flags name, and each team's on-call shifts, which --on-call names.
func (s *Server) checkDeclared(d ledger.Declared) error {
	for _, t := range d.Teams {
		if err := s.checkShifts(t.Shifts); err != nil {
			return ledger.DeclaredError{Line: t.Line, Name: t.Name, Err: err}
		}
	}
	for _, r := range d.Reports {
		if err := s.knowsDests(r.Scheduled); err != nil {
			return ledger.DeclaredError{Line: r.Line, Name: r.Name, Err: err}
		}
	}
	return nil
}

// configState is what the leader last made of --config: the file's hash it
// last kept the ledger to (applied), when, and what the last that changed
// anything did; and, when the file last read couldn't be used, its hash
// (failed), the ledger's generation then, and why, in each language.
type configState struct {
	sync.Mutex
	applied    string
	at         time.Time
	did        ledger.Reconciled
	failed     string
	failedGen  int64
	err        map[string]string
	errAt      time.Time
	loggedText string
}

// configShown is what the pages say of --config, as the leader last made
// of it (sharedStatus): its hash cut short.
type configShown struct {
	File    string            `json:"file,omitempty"`
	Prune   bool              `json:"prune,omitempty"`
	Hash    string            `json:"hash,omitempty"`
	Applied time.Time         `json:"applied,omitzero"`
	Did     ledger.Reconciled `json:"did,omitzero"`
	Err     map[string]string `json:"err,omitempty"`
	ErrAt   time.Time         `json:"err_at,omitzero"`
}

// ErrText is what was wrong with the file, in a language.
func (c *configShown) ErrText(lang string) string {
	if c == nil || len(c.Err) == 0 {
		return ""
	}
	if t, ok := c.Err[lang]; ok {
		return t
	}
	return c.Err["en"]
}

// configShown is this replica's: nil without --config.
func (s *Server) configShown() *configShown {
	if s.cfg.Config == "" {
		return nil
	}
	s.config.Lock()
	defer s.config.Unlock()
	c := &configShown{File: s.cfg.Config, Prune: s.cfg.ConfigPrune, Did: s.config.did, ErrAt: s.config.errAt}
	if c.File != "" && len(s.config.applied) >= 12 {
		c.Hash, c.Applied = s.config.applied[:12], s.config.at
	}
	if len(s.config.err) > 0 {
		c.Err = s.config.err
	}
	return c
}

// noConfig is the hash kept for no --config: nothing declared.
const noConfig = "none"

// reconcileConfig keeps the ledger holding what --config declares, as the
// file now says it, when it changed since the ledger was last kept to it;
// without --config, nothing declared, so that what a file declared before
// is the UI's again. The leader's alone (lead), in one write its lease
// fences: what replicas read of the file can differ for a minute as
// Kubernetes updates each one's copy, and two writing by turns would undo
// each other. A file that can't be used changes nothing, and is said once
// why; it is tried again as it changes, or the ledger does (a team it names
// made in the UI). An error of the ledger's own is returned, and the file
// read again next time.
func (s *Server) reconcileConfig(ctx context.Context) error {
	d, hash := ledger.Declared{}, noConfig
	var fileErr error
	if s.cfg.Config != "" {
		d, hash, fileErr = ReadConfig(s.cfg.Config, s.cfg.Loc)
		if fileErr == nil {
			fileErr = s.checkDeclared(d)
		}
		if hash == "" { // not read at all
			hash = "unread"
		}
	}
	gen := s.l.Gen()
	s.config.Lock()
	same := hash == s.config.applied || hash == s.config.failed && gen == s.config.failedGen
	s.config.Unlock()
	if same {
		return nil
	}
	var did ledger.Reconciled
	if fileErr == nil {
		o, err := s.options(ctx)
		if err != nil {
			return err
		}
		var de ledger.DeclaredError
		var inv ledger.ErrInvalid
		did, err = s.l.Reconcile(ctx, d, s.cfg.ConfigPrune, o)
		switch {
		case err == nil:
		case errors.As(err, &de), errors.As(err, &inv):
			fileErr = err
		default:
			return err // the ledger's
		}
	}
	s.config.Lock()
	defer s.config.Unlock()
	if fileErr != nil {
		s.config.failed, s.config.failedGen = hash, s.l.Gen()
		texts := map[string]string{}
		for _, l := range Langs() {
			texts[l] = configText(l, fileErr)
		}
		if texts["en"] != s.config.loggedText {
			s.config.errAt, s.config.loggedText = time.Now(), texts["en"]
			s.log.Error("--config can't be used; the ledger keeps what it last declared", "file", s.cfg.Config, "err", texts["en"])
		}
		s.config.err = texts
		return nil
	}
	s.config.applied, s.config.at, s.config.failed, s.config.err, s.config.loggedText = hash, time.Now(), "", nil, ""
	if did.Any() {
		s.config.did = did
		s.poke() // the windows changed are judged again
		s.log.Info("the ledger keeps what --config declares", "file", s.cfg.Config, "made", did.Made, "changed", did.Changed,
			"taken over", did.Adopted, "given back to the UI", did.Undeclared, "deleted", did.Deleted)
	}
	return nil
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
