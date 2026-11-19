package serve

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/signin"
)

// Trust is an entry of --backtest-trust (docs/design/backtests.md, "Who
// may ask"): a CI's issuer of ID tokens, the audience its tokens must be
// for and the claims each must hold; or a plain token's file. Days are the
// most one of its backtests replays, Hourly how many it may start in an
// hour.
type Trust struct {
	Name     string         `yaml:"name"`
	Issuer   string         `yaml:"issuer"`
	Audience string         `yaml:"audience"`
	JWKSURL  string         `yaml:"jwks_url"`
	CI       string         `yaml:"ci"`
	Claims   map[string]Pin `yaml:"claims"`
	Days     int            `yaml:"days"`
	Hourly   int            `yaml:"hourly"`
	// TokenFile holds a plain token, read for each request.
	TokenFile string `yaml:"token_file"`
	// IncidentTitles shows the incidents' titles in its comments, beside
	// their ids: off by default, as a title written in a tracker is then
	// read by whoever reads the pull request.
	IncidentTitles bool `yaml:"incident_titles"`
}

// Pin is a claim's value an entry pins: one, or, for the claim that names
// the repository by its id, a list of them, one enough.
type Pin struct {
	Values []string
	List   bool
}

// UnmarshalYAML reads a pin: a string, an integer as written, or a list of
// them; nothing else (true, a number with a point, a map) is one.
func (p *Pin) UnmarshalYAML(n *yaml.Node) error {
	one := func(n *yaml.Node) (string, error) {
		switch {
		case n.Kind != yaml.ScalarNode:
			return "", &pinError{line: n.Line, why: "a claim's value that isn't a string or an integer",
				err: fmt.Errorf("line %d: want a string or an integer", n.Line)}
		case n.Tag == "!!str":
			return n.Value, nil
		case n.Tag == "!!int" && decimal.MatchString(n.Value):
			return n.Value, nil
		}
		return "", &pinError{line: n.Line, why: "a claim's value that isn't a string or an integer written in decimal: quote it",
			err: fmt.Errorf("line %d: %q: want a string or an integer written in decimal; quote it", n.Line, n.Value)}
	}
	if n.Kind == yaml.SequenceNode {
		p.List = true
		for _, c := range n.Content {
			v, err := one(c)
			if err != nil {
				return err
			}
			p.Values = append(p.Values, v)
		}
		return nil
	}
	v, err := one(n)
	p.Values = []string{v}
	return err
}

// pinError is a claim's value a pin can't take: its line, and why in
// Lapilli's words, beside the message serve says, which quotes the value.
type pinError struct {
	line int
	why  string
	err  error
}

func (e *pinError) Error() string { return e.err.Error() }

// TrustRefused is why ReadTrust refused a trust file. Error is serve's
// message, which may quote what the file holds, as a value it can't take;
// Said is the same refusal in Lapilli's words alone, where it is and what
// is wrong, nothing of the file's but its line: what lapilli preflight
// prints, for CI logs and tickets more people read than serve's log.
type TrustRefused struct {
	// Line is the file's line, 0 when not known; Entry the entry under
	// trust:, from 1, 0 for the file as a whole; Field one of an entry's
	// keys, as Lapilli names them, "" for none known; Why what is wrong.
	Line  int
	Entry int
	Field string
	Why   string
	msg   string
	err   error
}

func (e *TrustRefused) Error() string { return e.msg }
func (e *TrustRefused) Unwrap() error { return e.err }

// Said is the refusal with nothing the file holds: line 12, entry 2,
// claims: why.
func (e *TrustRefused) Said() string {
	var where []string
	if e.Line > 0 {
		where = append(where, fmt.Sprintf("line %d", e.Line))
	}
	if e.Entry > 0 {
		where = append(where, fmt.Sprintf("entry %d", e.Entry))
	}
	if e.Field != "" && !strings.HasPrefix(e.Why, e.Field) {
		where = append(where, e.Field)
	}
	if len(where) == 0 {
		return e.Why
	}
	return strings.Join(where, ", ") + ": " + e.Why
}

// entryRefusal is what check refuses an entry for: the field, why in
// Lapilli's words, and serve's message.
type entryRefusal struct {
	field, why string
	// claim is the file's name of the claim refused, to find its line by,
	// never said: a name of the file's own may hold a value.
	claim string
	err   error
}

func (e *entryRefusal) Error() string { return e.err.Error() }
func (e *entryRefusal) Unwrap() error { return e.err }

// refuse is an entry refused for field, why said in Lapilli's words and
// err in serve's.
func refuse(field, why string, err error) error {
	return &entryRefusal{field: field, why: why, err: err}
}

// refuseClaim is an entry refused for one of its claims, named: said by
// its name where that is one of Lapilli's (claimSaid), its line looked up
// by the file's.
func refuseClaim(name, why string, err error) error {
	return &entryRefusal{field: "claims", why: why, claim: name, err: err}
}

// knownClaims are the claims Lapilli's documentation names, which a
// refusal says by name: of the CIs it knows, and the checks a token has.
var knownClaims = []string{"repository_id", "repository_owner_id", "repository", "event_name", "runner_environment", "ref", "workflow_ref",
	"job_workflow_ref", "project_id", "project_path", "pipeline_source", "namespace_id", "job_project_id", "sub", "repositoryUuid", "workspaceUuid"}

// claimSaid is how a refusal names a claim: by its name when Lapilli's
// documentation names it, otherwise "a claim".
func claimSaid(name string) string {
	if slices.Contains(knownClaims, name) {
		return name
	}
	return "a claim"
}

// refuseAs is an entry refused for field with a message that holds nothing
// of the file's, said alike by both.
func refuseAs(field, msg string) error {
	return refuse(field, msg, errors.New(msg))
}

// trustFields are the keys of an entry, as a refusal may name them.
var trustFields = []string{"name", "issuer", "audience", "jwks_url", "ci", "claims", "days", "hourly", "token_file", "incident_titles"}

var (
	decimal   = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	trustName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	claimName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]{0,127}$`)
)

// The CIs whose claims Lapilli knows (ci:): the claim that names the
// repository by its id, which an entry must pin, alone of its claims a
// list; the claim of the event the job runs for, which it must pin too,
// to one of events, those a fork's pull request can't run in the
// repository's own context with; and the claim that names the repository,
// which the Run history shows.
var knownCIs = map[string]struct {
	repoID, event, repoName string
	events                  []string
}{
	"github":  {"repository_id", "event_name", "repository", []string{"pull_request", "push"}},
	"forgejo": {"repository_id", "event_name", "repository", []string{"pull_request", "push"}},
	"gitlab":  {"project_id", "pipeline_source", "project_path", []string{"merge_request_event", "push"}},
}

// GitHub Enterprise Cloud's issuer of an enterprise's own (its slug after
// GitHub's), and GitHub Enterprise Server's (its host's /_services/token).
var (
	ghec = regexp.MustCompile(`^https://token\.actions\.githubusercontent\.com/[A-Za-z0-9][A-Za-z0-9-]{0,99}$`)
	ghes = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]+)?/_services/token$`)
)

// eventClaims are the claims that say which event a job runs for, as the
// CIs known name them, and the events of each taken: never one a fork's
// pull request runs in the repository's own context with
// (pull_request_target, workflow_run, issue_comment), nor one no person
// asked for (schedule).
var eventClaims = map[string][]string{
	"event_name":      {"pull_request", "push"},
	"pipeline_source": {"merge_request_event", "push"},
}

// ciOf is the CI an issuer is known as, when ci: doesn't say.
func ciOf(issuer string) string {
	switch {
	case issuer == "https://token.actions.githubusercontent.com", ghec.MatchString(issuer), ghes.MatchString(issuer):
		return "github"
	case issuer == "https://gitlab.com":
		return "gitlab"
	case strings.HasSuffix(issuer, "/api/actions"):
		return "forgejo"
	}
	return ""
}

// checkedClaims are the claims the token's check holds already, which no
// pin may name.
var checkedClaims = []string{"iss", "aud", "exp", "nbf", "iat", "jti", "azp"}

// minBacktestToken is how long a plain token is, at least.
const minBacktestToken = 32

// maxTrustFile is the most a trust file is read of.
const maxTrustFile = 1 << 20

// ReadTrust reads --backtest-trust, strictly: an unknown key, an entry
// that names neither an issuer nor a token file or both, a pin missing, a
// list where one value goes, an audience a CI or a cloud gives by
// default, a plain token shorter than 32 characters or one another token
// file holds too, is an error, and serve doesn't start: a trust file half
// read could let in whom it shouldn't. others are the files of the other
// tokens serve takes. With secretsLater (lapilli preflight --offline), the
// token files are taken as given, unread.
// An error is a *TrustRefused, its message serve's.
func ReadTrust(path string, others []string, secretsLater bool) ([]Trust, error) {
	whole := func(why string, err error) error { return &TrustRefused{Why: why, msg: err.Error(), err: err} }
	f, err := os.Open(path)
	if err != nil {
		return nil, whole(unreadableWhy(err), fmt.Errorf("--backtest-trust: %w", err))
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTrustFile+1))
	if err != nil {
		return nil, whole(unreadableWhy(err), fmt.Errorf("--backtest-trust: %w", err))
	}
	if len(b) > maxTrustFile {
		return nil, whole(fmt.Sprintf("more than %d MiB", maxTrustFile>>20), fmt.Errorf("--backtest-trust %s: more than %d MiB", path, maxTrustFile>>20))
	}
	var file struct {
		Trust []Trust `yaml:"trust"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, decodeRefused(b, fmt.Errorf("--backtest-trust %s: %w", path, err), err)
	}
	var more any
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, whole("one YAML document, trust: and its entries", fmt.Errorf("--backtest-trust %s: one YAML document, trust: and its entries", path))
	}
	if len(file.Trust) == 0 || len(file.Trust) > 100 {
		return nil, whole("want 1 to 100 entries under trust:", fmt.Errorf("--backtest-trust %s: want 1 to 100 entries under trust:", path))
	}
	seen := map[string]bool{}
	tokens := map[string]string{}
	for _, o := range others {
		if o == "" || secretsLater {
			continue
		}
		if b, err := os.ReadFile(o); err == nil {
			tokens[strings.TrimSpace(string(b))] = o
		}
	}
	for i := range file.Trust {
		t := &file.Trust[i]
		if err := t.check(seen, tokens, secretsLater); err != nil {
			name := t.Name
			if name == "" {
				name = fmt.Sprint("entry ", i+1)
			}
			r := &TrustRefused{Entry: i + 1, Why: "an entry that can't be used", msg: fmt.Sprintf("--backtest-trust %s: %s: %v", path, name, err), err: err}
			var er *entryRefusal
			claim := ""
			if errors.As(err, &er) {
				r.Field, r.Why, claim = er.field, er.why, er.claim
			}
			r.Line = entryLine(b, i, r.Field, claim)
			return nil, r
		}
	}
	return file.Trust, nil
}

// unreadableWhy says why a file can't be read, in Lapilli's words.
func unreadableWhy(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "the file isn't there"
	case errors.Is(err, fs.ErrPermission):
		return "the file can't be read by this user"
	}
	return "the file can't be read"
}

// yamlLine is the line a YAML error names.
var yamlLine = regexp.MustCompile(`\bline ([0-9]+)\b`)

// decodeRefused is a trust file YAML didn't decode into entries: the line
// it names, the entry and the key at it, and what kind of wrong, never
// what YAML quoted of it.
func decodeRefused(b []byte, msg, err error) *TrustRefused {
	r := &TrustRefused{Why: "doesn't read as YAML", msg: msg.Error(), err: err}
	var pe *pinError
	var te *yaml.TypeError
	switch {
	case errors.As(err, &pe):
		r.Line, r.Why = pe.line, pe.why
	case errors.As(err, &te) && len(te.Errors) > 0:
		first := te.Errors[0]
		if m := yamlLine.FindStringSubmatch(first); m != nil {
			r.Line, _ = strconv.Atoi(m[1])
		}
		r.Entry, r.Field = trustAtLine(b, r.Line)
		switch {
		case !strings.Contains(first, " not found in type "):
			r.Why = "a value of another kind than its key takes"
		case strings.HasSuffix(first, " in type serve.Trust"):
			r.Why, r.Field = "a key that isn't one of an entry's: "+strings.Join(trustFields, ", "), ""
		default:
			r.Why, r.Entry, r.Field = "a key other than trust:", 0, ""
		}
		return r
	default:
		if m := yamlLine.FindStringSubmatch(err.Error()); m != nil {
			r.Line, _ = strconv.Atoi(m[1])
		}
	}
	r.Entry, r.Field = trustAtLine(b, r.Line)
	return r
}

// trustEntries are the mappings under a trust file's trust:, as its YAML
// reads, aliases followed; nil where it doesn't read so.
func trustEntries(b []byte) []*yaml.Node {
	var doc yaml.Node
	if yaml.Unmarshal(b, &doc) != nil {
		return nil
	}
	n := deref(&doc)
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = deref(n.Content[0])
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != "trust" {
			continue
		}
		seq := deref(n.Content[i+1])
		if seq.Kind != yaml.SequenceNode {
			return nil
		}
		var out []*yaml.Node
		for _, e := range seq.Content {
			out = append(out, deref(e))
		}
		return out
	}
	return nil
}

func deref(n *yaml.Node) *yaml.Node {
	for i := 0; n.Kind == yaml.AliasNode && n.Alias != nil && i < 100; i++ {
		n = n.Alias
	}
	return n
}

// entryLine is the line of entry i's field, of the claim under it when
// one is named, or of the entry; 0 for none.
func entryLine(b []byte, i int, field, claim string) int {
	es := trustEntries(b)
	if i >= len(es) {
		return 0
	}
	e := es[i]
	for j := 0; field != "" && j+1 < len(e.Content); j += 2 {
		if e.Content[j].Value != field {
			continue
		}
		if v := deref(e.Content[j+1]); claim != "" && v.Kind == yaml.MappingNode {
			for k := 0; k+1 < len(v.Content); k += 2 {
				if v.Content[k].Value == claim {
					return v.Content[k].Line
				}
			}
		}
		return e.Content[j].Line
	}
	return e.Line
}

// trustAtLine is the entry a line is in, from 1, and the key of it at the
// line, when the line holds one key and that key is one of an entry's.
func trustAtLine(b []byte, line int) (int, string) {
	if line <= 0 {
		return 0, ""
	}
	entry := 0
	es := trustEntries(b)
	for i, e := range es {
		if e.Line <= line {
			entry = i + 1
		}
	}
	if entry == 0 {
		return 0, ""
	}
	e := es[entry-1]
	var at []string
	last := ""
	for j := 0; j+1 < len(e.Content); j += 2 {
		k := e.Content[j]
		if k.Line == line {
			at = append(at, k.Value)
		}
		if k.Line <= line {
			last = k.Value
		}
	}
	key := last
	if len(at) > 1 {
		key = ""
	}
	if !slices.Contains(trustFields, key) {
		key = ""
	}
	return entry, key
}

// check holds an entry to what one must be, filling in its defaults. Each
// refusal says its field and why in Lapilli's words (refuse), beside the
// message serve says, which may quote a value of the file's.
func (t *Trust) check(seen map[string]bool, tokens map[string]string, secretsLater bool) error {
	switch {
	case !trustName.MatchString(t.Name):
		return refuseAs("name", "want a name of lowercase letters, digits and . _ -, 63 at most: what the log, /metrics and the Run history call it")
	case seen[t.Name]:
		return refuseAs("name", "two entries have this name")
	case t.Days < 0 || t.Days > backtestDays:
		return refuseAs("days", fmt.Sprintf("days is 1 to %d", backtestDays))
	case t.Hourly < 0 || t.Hourly > 3600:
		return refuseAs("hourly", "hourly is 1 to 3600")
	case (t.Issuer == "") == (t.TokenFile == ""):
		return refuseAs("", "give an issuer, for a CI's ID tokens, or a token_file, for a plain token: one of them")
	}
	seen[t.Name] = true
	if t.Days == 0 {
		t.Days = backtestDays
	}
	if t.Hourly == 0 {
		t.Hourly = 30
	}
	if t.TokenFile != "" {
		if t.Audience != "" || t.JWKSURL != "" || t.CI != "" || len(t.Claims) > 0 {
			return refuseAs("token_file", "a token_file entry takes a plain token: audience, jwks_url, ci and claims are an issuer's")
		}
		if secretsLater {
			return nil
		}
		b, err := os.ReadFile(t.TokenFile)
		if err != nil {
			return refuse("token_file", "token_file: "+unreadableWhy(err), fmt.Errorf("token_file: %w", err))
		}
		tk := strings.TrimSpace(string(b))
		if len(tk) < minBacktestToken {
			return refuseAs("token_file", fmt.Sprintf("token_file: want a token of %d characters at least", minBacktestToken))
		}
		if other, ok := tokens[tk]; ok {
			return refuse("token_file", "token_file holds the same token as another token file serve takes: give each its own",
				fmt.Errorf("token_file holds the same token as %s: give each its own", other))
		}
		tokens[tk] = "token_file " + t.Name
		return nil
	}
	if err := issuerURL(t.Issuer); err != nil {
		return refuse("issuer", "issuer: "+issuerWhy(t.Issuer), fmt.Errorf("issuer: %w", err))
	}
	if t.JWKSURL != "" {
		if err := issuerURL(t.JWKSURL); err != nil {
			return refuse("jwks_url", "jwks_url: "+issuerWhy(t.JWKSURL), fmt.Errorf("jwks_url: %w", err))
		}
	}
	if t.Audience == "" {
		return refuseAs("audience", "audience is required: the backtest port's URL, such as https://lapilli-ci.example.com, and never a CI's default")
	}
	if why := defaultAudience(t.Audience, t.Issuer); why != "" {
		return refuse("audience", "audience is "+why+": give one made for this port alone, such as its URL",
			fmt.Errorf("audience %q is %s: give one made for this port alone, such as its URL", t.Audience, why))
	}
	if t.CI == "" {
		t.CI = ciOf(t.Issuer)
	} else if _, ok := knownCIs[t.CI]; !ok {
		return refuse("ci", "ci: want github, gitlab or forgejo, or leave it out for another issuer",
			fmt.Errorf("ci %q: want github, gitlab or forgejo, or leave it out for another issuer", t.CI))
	}
	if len(t.Claims) == 0 {
		return refuseAs("claims", "claims: pin the claims a token must hold, the repository's id among them")
	}
	for _, name := range slices.Sorted(maps.Keys(t.Claims)) {
		p := t.Claims[name]
		switch {
		case !claimName.MatchString(name):
			return refuseClaim(name, "a claim's name that isn't one: a letter or _, then letters, digits and _ . : -, 128 at most",
				fmt.Errorf("claims: %q isn't a claim's name", name))
		case slices.Contains(checkedClaims, name):
			// One of a few names of Lapilli's own: said as it is.
			return refuseClaim(name, fmt.Sprintf("claims: %s is checked as the token is, and isn't pinned", name),
				fmt.Errorf("claims: %s is checked as the token is, and isn't pinned", name))
		case len(p.Values) == 0:
			return refuseClaim(name, claimSaid(name)+" pinned to no value", fmt.Errorf("claims: %s: give a value", name))
		case len(p.Values) > 100:
			return refuseClaim(name, claimSaid(name)+" pinned to more than 100 values", fmt.Errorf("claims: %s: 100 values at most", name))
		}
		for _, v := range p.Values {
			if v == "" || len(v) > 256 || strings.ContainsFunc(v, unicode.IsControl) {
				return refuseClaim(name, claimSaid(name)+": a value isn't 1 to 256 bytes, with nothing that doesn't show",
					fmt.Errorf("claims: %s: a value of 1 to 256 bytes, with nothing that doesn't show", name))
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(eventClaims)) {
		events := eventClaims[name]
		if p, ok := t.Claims[name]; ok && !slices.Contains(events, p.Values[0]) {
			return refuseClaim(name, fmt.Sprintf("claims: %s: want %s; another can run a fork's pull request in the repository's own context, or runs with no one asking", name, strings.Join(events, " or ")),
				fmt.Errorf("claims: %s %q: want %s; another can run a fork's pull request in the repository's own context, or runs with no one asking", name, p.Values[0], strings.Join(events, " or ")))
		}
	}
	known, ok := knownCIs[t.CI]
	if !ok {
		// Another issuer: pinned by sub and by the event, each one value.
		for _, name := range slices.Sorted(maps.Keys(t.Claims)) {
			if t.Claims[name].List {
				return refuseClaim(name, claimSaid(name)+" is a list: only the claim that names the repository by its id may be one, of a CI given as ci:",
					fmt.Errorf("claims: %s is a list: only the claim that names the repository by its id may be one, of a CI given as ci:", name))
			}
		}
		if _, ok := t.Claims["sub"]; !ok {
			return refuseAs("claims", "claims: an issuer Lapilli doesn't know by its URL needs ci: (github, gitlab or forgejo), or sub pinned, and the event (event_name or pipeline_source)")
		}
		if _, ok := t.Claims["event_name"]; !ok {
			if _, ok := t.Claims["pipeline_source"]; !ok {
				return refuseAs("claims", "claims: pin the event the job runs for, event_name or pipeline_source, beside sub: or give ci: for a CI Lapilli knows")
			}
		}
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(t.Claims)) {
		if t.Claims[name].List && name != known.repoID {
			return refuseClaim(name, fmt.Sprintf("%s is a list: only %s may be one, each repository by its id", claimSaid(name), known.repoID),
				fmt.Errorf("claims: %s is a list: only %s may be one, each repository by its id", name, known.repoID))
		}
	}
	if _, ok := t.Claims[known.repoID]; !ok {
		return refuseAs("claims", fmt.Sprintf("claims: pin %s, the repository's id (a name is taken again by a repository made under it; an owner alone would trust every repository made in it)", known.repoID))
	}
	ev, ok := t.Claims[known.event]
	if !ok {
		return refuseAs("claims", fmt.Sprintf("claims: pin %s to %s: the event the job runs for", known.event, strings.Join(known.events, " or ")))
	}
	if !slices.Contains(known.events, ev.Values[0]) {
		return refuseClaim(known.event, fmt.Sprintf("claims: %s: want %s; another can run a fork's pull request in the repository's own context", known.event, strings.Join(known.events, " or ")),
			fmt.Errorf("claims: %s %q: want %s; another can run a fork's pull request in the repository's own context", known.event, ev.Values[0], strings.Join(known.events, " or ")))
	}
	return nil
}

// issuerURL says what is wrong with an issuer's URL, or its keys': https,
// or http to this machine or a name inside a cluster; no query, fragment
// or user.
func issuerURL(u string) error {
	if why := issuerWhy(u); why != "" {
		return fmt.Errorf("%q: %s", u, why)
	}
	return nil
}

// issuerWhy is what is wrong with an issuer's URL, or its keys', without
// the URL; "" for nothing.
func issuerWhy(u string) string {
	p, err := url.Parse(u)
	switch {
	case err != nil || !signin.SafeURL(u):
		return "want https, or http to this machine or a name inside a cluster"
	case p.RawQuery != "" || p.Fragment != "" || p.User != nil:
		return "no query, fragment or user"
	}
	return ""
}

// defaultAudience says why an audience is one a token made for another
// service has, "" when it isn't: a CI's default (GitHub's is the
// repository owner's URL, Forgejo's its own, under the issuer's origin),
// sigstore's, or one a cloud's token exchange takes (AWS STS, Azure's
// Entra, Google Cloud's workload identity federation). A token of such an
// audience could be sent here, or one made here sent there.
func defaultAudience(aud, issuer string) string {
	a := strings.ToLower(strings.TrimSpace(aud))
	switch {
	case a == "https://github.com" || strings.HasPrefix(a, "https://github.com/"):
		return "GitHub's default, its repository owner's URL"
	case a == "sigstore":
		return "sigstore's"
	case a == "sts.amazonaws.com" || strings.HasSuffix(a, ".amazonaws.com"):
		return "the audience AWS STS takes"
	case a == "api://azureadtokenexchange":
		return "the audience Microsoft Entra's token exchange takes"
	case strings.HasPrefix(a, "//iam.googleapis.com/") || strings.HasPrefix(a, "https://iam.googleapis.com/") || strings.HasPrefix(a, "https://sts.googleapis.com"):
		return "Google Cloud's workload identity federation's"
	case strings.EqualFold(strings.TrimSuffix(a, "/"), strings.TrimSuffix(issuer, "/")) || signin.SameOrigin(aud, issuer):
		return "at the issuer's own origin, as a CI's default is"
	}
	return ""
}
