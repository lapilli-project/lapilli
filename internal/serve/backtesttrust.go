package serve

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
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
			return "", fmt.Errorf("line %d: want a string or an integer", n.Line)
		case n.Tag == "!!str":
			return n.Value, nil
		case n.Tag == "!!int" && decimal.MatchString(n.Value):
			return n.Value, nil
		}
		return "", fmt.Errorf("line %d: %q: want a string or an integer written in decimal; quote it", n.Line, n.Value)
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

// ciOf is the CI an issuer is known as, when ci: doesn't say.
func ciOf(issuer string) string {
	switch {
	case issuer == "https://token.actions.githubusercontent.com":
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
func ReadTrust(path string, others []string, secretsLater bool) ([]Trust, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--backtest-trust: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTrustFile+1))
	if err != nil {
		return nil, fmt.Errorf("--backtest-trust: %w", err)
	}
	if len(b) > maxTrustFile {
		return nil, fmt.Errorf("--backtest-trust %s: more than %d MiB", path, maxTrustFile>>20)
	}
	var file struct {
		Trust []Trust `yaml:"trust"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("--backtest-trust %s: %w", path, err)
	}
	var more any
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("--backtest-trust %s: one YAML document, trust: and its entries", path)
	}
	if len(file.Trust) == 0 || len(file.Trust) > 100 {
		return nil, fmt.Errorf("--backtest-trust %s: want 1 to 100 entries under trust:", path)
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
			return nil, fmt.Errorf("--backtest-trust %s: %s: %w", path, name, err)
		}
	}
	return file.Trust, nil
}

// check holds an entry to what one must be, filling in its defaults.
func (t *Trust) check(seen map[string]bool, tokens map[string]string, secretsLater bool) error {
	switch {
	case !trustName.MatchString(t.Name):
		return errors.New("want a name of lowercase letters, digits and . _ -, 63 at most: what the log, /metrics and the Run history call it")
	case seen[t.Name]:
		return errors.New("two entries have this name")
	case t.Days < 0 || t.Days > backtestDays:
		return fmt.Errorf("days is 1 to %d", backtestDays)
	case t.Hourly < 0 || t.Hourly > 3600:
		return errors.New("hourly is 1 to 3600")
	case (t.Issuer == "") == (t.TokenFile == ""):
		return errors.New("give an issuer, for a CI's ID tokens, or a token_file, for a plain token: one of them")
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
			return errors.New("a token_file entry takes a plain token: audience, jwks_url, ci and claims are an issuer's")
		}
		if secretsLater {
			return nil
		}
		b, err := os.ReadFile(t.TokenFile)
		if err != nil {
			return fmt.Errorf("token_file: %w", err)
		}
		tk := strings.TrimSpace(string(b))
		if len(tk) < minBacktestToken {
			return fmt.Errorf("token_file: want a token of %d characters at least", minBacktestToken)
		}
		if other, ok := tokens[tk]; ok {
			return fmt.Errorf("token_file holds the same token as %s: give each its own", other)
		}
		tokens[tk] = "token_file " + t.Name
		return nil
	}
	if err := issuerURL(t.Issuer); err != nil {
		return fmt.Errorf("issuer: %w", err)
	}
	if t.JWKSURL != "" {
		if err := issuerURL(t.JWKSURL); err != nil {
			return fmt.Errorf("jwks_url: %w", err)
		}
	}
	if t.Audience == "" {
		return errors.New("audience is required: the backtest port's URL, such as https://lapilli-ci.example.com, and never a CI's default")
	}
	if why := defaultAudience(t.Audience, t.Issuer); why != "" {
		return fmt.Errorf("audience %q is %s: give one made for this port alone, such as its URL", t.Audience, why)
	}
	if t.CI == "" {
		t.CI = ciOf(t.Issuer)
	} else if _, ok := knownCIs[t.CI]; !ok {
		return fmt.Errorf("ci %q: want github, gitlab or forgejo, or leave it out for another issuer", t.CI)
	}
	if len(t.Claims) == 0 {
		return errors.New("claims: pin the claims a token must hold, the repository's id among them")
	}
	for name, p := range t.Claims {
		switch {
		case !claimName.MatchString(name):
			return fmt.Errorf("claims: %q isn't a claim's name", name)
		case slices.Contains(checkedClaims, name):
			return fmt.Errorf("claims: %s is checked as the token is, and isn't pinned", name)
		case len(p.Values) == 0:
			return fmt.Errorf("claims: %s: give a value", name)
		case len(p.Values) > 100:
			return fmt.Errorf("claims: %s: 100 values at most", name)
		}
		for _, v := range p.Values {
			if v == "" || len(v) > 256 || strings.ContainsFunc(v, unicode.IsControl) {
				return fmt.Errorf("claims: %s: a value of 1 to 256 bytes, with nothing that doesn't show", name)
			}
		}
	}
	known, ok := knownCIs[t.CI]
	if !ok {
		// Another issuer: pinned by sub, or another claim, each one value.
		for name, p := range t.Claims {
			if p.List {
				return fmt.Errorf("claims: %s is a list: only the claim that names the repository by its id may be one, of a CI given as ci:", name)
			}
		}
		return nil
	}
	for name, p := range t.Claims {
		if p.List && name != known.repoID {
			return fmt.Errorf("claims: %s is a list: only %s may be one, each repository by its id", name, known.repoID)
		}
	}
	if _, ok := t.Claims[known.repoID]; !ok {
		return fmt.Errorf("claims: pin %s, the repository's id (a name is taken again by a repository made under it; an owner alone would trust every repository made in it)", known.repoID)
	}
	ev, ok := t.Claims[known.event]
	if !ok {
		return fmt.Errorf("claims: pin %s to %s: the event the job runs for", known.event, strings.Join(known.events, " or "))
	}
	if !slices.Contains(known.events, ev.Values[0]) {
		return fmt.Errorf("claims: %s %q: want %s; another can run a fork's pull request in the repository's own context", known.event, ev.Values[0], strings.Join(known.events, " or "))
	}
	return nil
}

// issuerURL says what is wrong with an issuer's URL, or its keys': https,
// or http to this machine or a name inside a cluster; no query, fragment
// or user.
func issuerURL(u string) error {
	p, err := url.Parse(u)
	switch {
	case err != nil || !signin.SafeURL(u):
		return fmt.Errorf("%q: want https, or http to this machine or a name inside a cluster", u)
	case p.RawQuery != "" || p.Fragment != "" || p.User != nil:
		return fmt.Errorf("%q: no query, fragment or user", u)
	}
	return nil
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
