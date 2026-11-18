package serve

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A trust file is read strictly, and one that can't be used, or would trust
// more than it says, is refused, each with why: serve doesn't start on it.
func TestReadTrustRefused(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	os.WriteFile(short, []byte("only-twenty-characters"), 0o600)
	good := filepath.Join(dir, "good")
	os.WriteFile(good, []byte(strings.Repeat("g", 32)), 0o600)
	same := filepath.Join(dir, "same")
	os.WriteFile(same, []byte(strings.Repeat("g", 32)), 0o600)
	incidents := filepath.Join(dir, "incidents")
	os.WriteFile(incidents, []byte(strings.Repeat("i", 32)), 0o600)
	asIncidents := filepath.Join(dir, "as-incidents")
	os.WriteFile(asIncidents, []byte(strings.Repeat("i", 32)+"\n"), 0o600)
	gh := func(claims string) string {
		return "trust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n    audience: https://lapilli-ci.example.com\n    claims:\n" + claims
	}
	for name, c := range map[string]struct{ file, says string }{
		"an unknown key":                 {gh("      repository_id: \"1\"\n      event_name: pull_request\n    hourlyy: 3\n"), "hourlyy"},
		"no entry":                       {"trust: []\n", "1 to 100"},
		"two documents":                  {gh("      repository_id: \"1\"\n      event_name: pull_request\n") + "---\ntrust: []\n", "one YAML document"},
		"no name":                        {"trust:\n  - issuer: https://gitlab.com\n", "name"},
		"a name of capitals":             {"trust:\n  - name: Alerts\n    token_file: " + good + "\n", "lowercase"},
		"two of one name":                {"trust:\n  - {name: a, token_file: " + good + "}\n  - {name: a, token_file: " + good + "}\n", "two entries"},
		"issuer and token":               {"trust:\n  - {name: a, token_file: " + good + ", issuer: https://gitlab.com}\n", "one of them"},
		"neither":                        {"trust:\n  - {name: a, days: 3}\n", "one of them"},
		"a token and claims":             {"trust:\n  - name: a\n    token_file: " + good + "\n    claims: {sub: x}\n", "plain token"},
		"a short token":                  {"trust:\n  - {name: a, token_file: " + short + "}\n", "32 characters"},
		"a token twice":                  {"trust:\n  - {name: a, token_file: " + good + "}\n  - {name: b, token_file: " + same + "}\n", "same token"},
		"the incidents' token":           {"trust:\n  - {name: a, token_file: " + asIncidents + "}\n", "same token as " + incidents},
		"a token file not there":         {"trust:\n  - {name: a, token_file: " + filepath.Join(dir, "none") + "}\n", "token_file"},
		"http elsewhere":                 {"trust:\n  - {name: a, issuer: 'http://ci.example.com', audience: x, claims: {sub: y}}\n", "https"},
		"an issuer's query":              {"trust:\n  - {name: a, issuer: 'https://ci.example.com/?x=1', audience: x, claims: {sub: y}}\n", "no query"},
		"no audience":                    {"trust:\n  - name: a\n    issuer: https://token.actions.githubusercontent.com\n    claims: {repository_id: '1', event_name: push}\n", "audience is required"},
		"GitHub's default":               {strings.Replace(gh("      repository_id: \"1\"\n      event_name: pull_request\n"), "https://lapilli-ci.example.com", "https://github.com/example", 1), "GitHub's default"},
		"sigstore":                       {strings.Replace(gh("      repository_id: \"1\"\n"), "https://lapilli-ci.example.com", "sigstore", 1), "sigstore"},
		"AWS STS":                        {strings.Replace(gh("      repository_id: \"1\"\n"), "https://lapilli-ci.example.com", "sts.amazonaws.com", 1), "AWS STS"},
		"Entra":                          {strings.Replace(gh("      repository_id: \"1\"\n"), "https://lapilli-ci.example.com", "api://AzureADTokenExchange", 1), "Entra"},
		"Google's federation":            {strings.Replace(gh("      repository_id: \"1\"\n"), "https://lapilli-ci.example.com", "//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/p/providers/q", 1), "Google Cloud"},
		"the issuer's origin":            {"trust:\n  - name: a\n    issuer: https://code.example.com/api/actions\n    audience: https://code.example.com/monitoring\n    claims: {repository_id: '1', event_name: pull_request}\n", "issuer's own origin"},
		"no claims":                      {"trust:\n  - {name: a, issuer: 'https://ci.example.com', audience: x}\n", "claims"},
		"the owner alone":                {gh("      repository_owner_id: \"4200\"\n      event_name: pull_request\n"), "pin repository_id"},
		"the name, not the id":           {gh("      repository: example/alerts\n      event_name: pull_request\n"), "pin repository_id"},
		"no event":                       {gh("      repository_id: \"1\"\n"), "pin event_name"},
		"pull_request_target":            {gh("      repository_id: \"1\"\n      event_name: pull_request_target\n"), "fork"},
		"workflow_run":                   {gh("      repository_id: \"1\"\n      event_name: workflow_run\n"), "fork"},
		"an owner's list":                {gh("      repository_id: [\"1\", \"2\"]\n      repository_owner_id: [\"4200\", \"4300\"]\n      event_name: pull_request\n"), "only repository_id may be one"},
		"GitLab without source":          {"trust:\n  - name: g\n    issuer: https://gitlab.com\n    audience: https://lapilli-ci.example.com\n    claims: {project_id: '42'}\n", "pin pipeline_source"},
		"GitLab's job project":           {"trust:\n  - name: g\n    issuer: https://gitlab.com\n    audience: https://lapilli-ci.example.com\n    claims: {job_project_id: '42', pipeline_source: merge_request_event}\n", "pin project_id"},
		"a generic list":                 {"trust:\n  - {name: a, issuer: 'https://ci.example.com', audience: x, claims: {sub: [y, z]}}\n", "list"},
		"a bool pin":                     {gh("      repository_id: \"1\"\n      event_name: pull_request\n      private: true\n"), "decimal"},
		"a float pin":                    {gh("      repository_id: 1.0\n      event_name: pull_request\n"), "decimal"},
		"a hex pin":                      {gh("      repository_id: 0x1F\n      event_name: pull_request\n"), "decimal"},
		"a map pin":                      {gh("      repository_id: {id: 1}\n      event_name: pull_request\n"), "string or an integer"},
		"aud pinned":                     {gh("      repository_id: \"1\"\n      event_name: pull_request\n      aud: x\n"), "checked as the token is"},
		"an unknown ci":                  {"trust:\n  - {name: a, ci: jenkins, issuer: 'https://ci.example.com', audience: x, claims: {sub: y}}\n", "github, gitlab or forgejo"},
		"15 days":                        {"trust:\n  - {name: a, token_file: " + good + ", days: 15}\n", "days is 1 to 14"},
		"an unknown issuer, no sub":      {"trust:\n  - {name: a, issuer: 'https://ci.example.com', audience: x, claims: {project_id: '42', event_name: push}}\n", "needs ci:"},
		"an unknown issuer, no event":    {"trust:\n  - {name: a, issuer: 'https://ci.example.com', audience: x, claims: {sub: y}}\n", "pin the event"},
		"an unknown issuer's fork event": {"trust:\n  - {name: a, issuer: 'https://ci.example.com', audience: x, claims: {sub: y, event_name: pull_request_target}}\n", "fork"},
		"a schedule":                     {gh("      repository_id: \"1\"\n      event_name: schedule\n"), "no one asking"},
		"a GitLab of its own, no ci:":    {"trust:\n  - name: g\n    issuer: https://gitlab.example.com\n    audience: https://lapilli-ci.example.com\n    claims: {project_id: '42', pipeline_source: merge_request_event}\n", "needs ci:"},
		"GHEC's, owner alone":            {"trust:\n  - name: g\n    issuer: https://token.actions.githubusercontent.com/example\n    audience: https://lapilli-ci.example.com\n    claims: {repository_owner_id: '4200', event_name: pull_request}\n", "pin repository_id"},
		"GHES's, a fork event":           {"trust:\n  - name: g\n    issuer: https://github.example.com/_services/token\n    audience: https://lapilli-ci.example.com\n    claims: {repository_id: '1', event_name: pull_request_target}\n", "fork"},
	} {
		f := filepath.Join(dir, "trust.yaml")
		os.WriteFile(f, []byte(c.file), 0o600)
		_, err := ReadTrust(f, []string{incidents}, false)
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want it to say %q", name, err, c.says)
		}
		// And the same refusal in Lapilli's words, with nothing of the file.
		var tr *TrustRefused
		if !errors.As(err, &tr) || tr.Why == "" {
			t.Errorf("%s: %T %v", name, err, err)
		} else if what := leaked(c.file, tr.Said()); what != "" {
			t.Errorf("%s: %q says %q of the file", name, tr.Said(), what)
		}
	}
}

// trustWords are the words of Lapilli's own a trust file holds too, which
// a refusal may say: its keys, the claims and events it names, its CIs.
var trustWords = append(append([]string{"trust", "true", "false", "repository_id", "project_id", "event_name", "pipeline_source", "sub",
	"repository", "project_path", "pull_request", "push", "merge_request_event", "github", "gitlab", "forgejo", "https", "http", "sigstore"}, trustFields...), checkedClaims...)

// leaked is a word of the file's that said says, "" for none: every
// scalar of the file, keys among them, outside trust: too, and every run
// of letters and digits in each, of four characters or more, but
// Lapilli's own words.
func leaked(file, said string) string {
	var doc yaml.Node
	yaml.Unmarshal([]byte(file), &doc)
	var scalars []string
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			scalars = append(scalars, n.Value)
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	// What a value the YAML doesn't read holds, by line, as yaml.v3 can't
	// make it a node: every run of the file's.
	whole := regexp.MustCompile(`[A-Za-z0-9_]+(?:[._:/@-]+[A-Za-z0-9_]+)*`)
	// But a file's path, the token's, which no refusal says either.
	paths := regexp.MustCompile(`(^|[\s{,])/[^\s,}]*`)
	scalars = append(scalars, whole.FindAllString(paths.ReplaceAllString(file, " "), -1)...)
	part := regexp.MustCompile(`[A-Za-z0-9_.@-]+|[0-9]{4,}`)
	for _, sc := range scalars {
		if strings.HasPrefix(sc, "/") {
			continue // a file's path, the token's: said by no refusal
		}
		for _, w := range append(append([]string{sc}, whole.FindAllString(sc, -1)...), part.FindAllString(sc, -1)...) {
			if len(w) < 4 || slices.Contains(trustWords, w) || strings.HasPrefix(w, "/") {
				continue
			}
			if regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(w) + `($|[^A-Za-z0-9_])`).MatchString(said) {
				return w
			}
		}
	}
	return ""
}

// What a trust file holds never reaches what a refusal says in Lapilli's
// words, however the file is written: a key that holds the value (no space
// after its colon), a key and a value the other way about, aliases and
// merge keys from outside trust:, an audience with spaces after it. Nor
// does a value change Lapilli's own words.
func TestReadTrustSaid(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "trust.yaml")
	for name, c := range map[string]struct {
		file, says string
		line       int
		field      string
	}{
		"no space after the colon": {"trust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n    audience: https://lapilli-ci.example.com\n" +
			"    claims: {repository_id:987650001, event_name: pull_request}\n", "no value", 5, "claims"},
		"a key and a value the other way": {"trust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n    audience: https://lapilli-ci.example.com\n" +
			"    claims:\n      \"987650002\": repository_id\n      event_name: pull_request\n", "a claim's name that isn't one", 5, "claims"},
		"an alias from outside trust:": {"ids: &ids [\"987650003\", 0x3ade]\ntrust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n    audience: https://lapilli-ci.example.com\n" +
			"    claims:\n      repository_id: *ids\n      event_name: pull_request\n", "decimal", 1, ""},
		"a merge key from outside trust:": {"base: &base\n  repository_id: \"987650004\"\n  event_name: workflow_run\ntrust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n" +
			"    audience: https://lapilli-ci.example.com\n    claims:\n      <<: *base\n", "a key other than trust:", 1, ""},
		"an audience with spaces after it": {"trust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n" +
			"    audience: \"https://github.com/example-owner-987650005   \"\n    claims: {repository_id: \"987650006\", event_name: push}\n", "GitHub's default", 4, "audience"},
		"a value Lapilli's words hold": {"trust:\n  - {name: plain, token_file: /x, days: 30, hourly: 1}\n  - {name: alerts, issuer: 'https://token.actions.githubusercontent.com', audience: x, claims: {repository_id: '1', event_name: push}, days: 30}\n",
			"days is 1 to 14", 2, "days"},
		"an unknown ci":  {"trust:\n  - {name: a, ci: jenkins-987650007, issuer: 'https://ci.example.com', audience: x, claims: {sub: y}}\n", "github, gitlab or forgejo", 2, "ci"},
		"an unknown key": {"trust:\n  - name: a\n    token_file: /x\n    weekly_987650008: 3\n", "isn't one of an entry's", 4, ""},
		"a merge key in an entry": {"base: &base\n  repository_id: \"987650011\"\n  event_name: workflow_run\ntrust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n" +
			"    audience: https://lapilli-ci.example.com\n    claims: {event_name: push}\n    <<: *base\n", "a key other than trust:", 1, ""},
		"YAML that doesn't read":  {"trust:\n  - name: a\n   issuer: \"https://ci-987650009.example.com\n", "doesn't read as YAML", 0, ""},
		"an issuer of plain http": {"trust:\n  - {name: a, issuer: 'http://ci-987650010.example.com', audience: x, claims: {sub: y}}\n", "want https", 2, "issuer"},
	} {
		os.WriteFile(f, []byte(c.file), 0o600)
		_, err := ReadTrust(f, nil, true)
		var tr *TrustRefused
		if !errors.As(err, &tr) {
			t.Errorf("%s: %T %v", name, err, err)
			continue
		}
		said := tr.Said()
		if what := leaked(c.file, said); what != "" || strings.Contains(said, "98765") || !strings.Contains(said, c.says) ||
			c.line > 0 && tr.Line != c.line || tr.Field != c.field {
			t.Errorf("%s: %q (line %d, field %q), leaks %q; serve says %v", name, said, tr.Line, tr.Field, what, err)
		}
	}
}

// What a trust file takes: the design's example, with a list of
// repositories, an owner beside them to narrow it, ids written as integers
// or strings; its defaults filled in; a generic issuer by sub; the CI
// worked out from the issuer.
func TestReadTrust(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "azure-token")
	os.WriteFile(tok, []byte(strings.Repeat("a", 40)+"\n"), 0o600)
	f := filepath.Join(dir, "trust.yaml")
	os.WriteFile(f, []byte(`trust:
  - name: alerts
    issuer: https://token.actions.githubusercontent.com
    audience: https://lapilli-ci.example.com
    claims:
      repository_id: ["123456789", 987654321]
      repository_owner_id: "4200"
      event_name: pull_request
      runner_environment: github-hosted
    days: 14
    hourly: 30
  - name: monitoring-gitlab
    issuer: https://gitlab.example.com
    ci: gitlab
    audience: https://lapilli-ci.example.com
    claims:
      project_id: "42"
      pipeline_source: merge_request_event
  - name: forgejo
    issuer: https://code.example.com/api/actions
    audience: https://lapilli-ci.example.com
    claims: {repository_id: 7, event_name: pull_request}
  - name: oidc
    issuer: https://ci.example.com
    jwks_url: https://keys.example.com/jwks
    audience: https://lapilli-ci.example.com
    claims: {sub: "project:monitoring", event_name: push}
  - name: enterprise
    issuer: https://token.actions.githubusercontent.com/example-enterprise
    audience: https://lapilli-ci.example.com
    claims: {repository_id: "5", event_name: pull_request}
  - name: server
    issuer: https://github.example.com/_services/token
    audience: https://lapilli-ci.example.com
    claims: {repository_id: "6", event_name: pull_request}
  - name: monitoring-azure
    token_file: `+tok+`
`), 0o600)
	trust, err := ReadTrust(f, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(trust) != 7 || trust[4].CI != "github" || trust[5].CI != "github" {
		t.Fatalf("%+v", trust)
	}
	if a := trust[0]; a.CI != "github" || !a.Claims["repository_id"].List || strings.Join(a.Claims["repository_id"].Values, ",") != "123456789,987654321" || a.Claims["repository_owner_id"].List {
		t.Errorf("%+v", a)
	}
	if g := trust[1]; g.CI != "gitlab" || g.Days != 14 || g.Hourly != 30 {
		t.Errorf("defaults: %+v", g)
	}
	if fj := trust[2]; fj.CI != "forgejo" || fj.Claims["repository_id"].Values[0] != "7" {
		t.Errorf("%+v", fj)
	}
	if o := trust[3]; o.CI != "" || o.JWKSURL == "" {
		t.Errorf("%+v", o)
	}
	// Offline, the token files are taken as given.
	os.Remove(tok)
	if _, err := ReadTrust(f, nil, true); err != nil {
		t.Errorf("offline: %v", err)
	}
}
