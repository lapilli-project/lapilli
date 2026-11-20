package serve

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/scenario"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// Adding a rule, as a proposal held to its expectations before it is
// approved (docs/design/expectations.md, step 5). Whoever proposes it gives
// the rule, as one Prometheus alerting rule, the place it goes, of those
// serve reads and may change, and what it is held to: at least one
// expectation. Lapilli reads the rule strictly, writes it where it goes as
// that place holds a rule, reads that back, replays it over the twelve
// weeks with nothing beside it, and holds it to its expectations as
// test_expectation does. It is approved only when none is broken, and
// goes the four paths a change goes: a pull request, Grafana's API, a
// ruler's, a cloud's. Undone, the rule is deleted the way each path
// deletes one.

// addPlace is where a proposal adds a rule.
type addPlace struct {
	Path string `json:"path"`
	// File is, of the git path, the rule file as the rules are named by it
	// (docName); Folder, of Grafana, its folder's title; Namespace, of a
	// ruler, its namespace; Alarms, of a cloud's alarms, the --alarms
	// source they are read under.
	File      string `json:"file,omitempty"`
	Folder    string `json:"folder,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Alarms    string `json:"alarms,omitempty"`
	// Group is the group it goes in (Grafana's, within its folder); with
	// NewGroup, a group made for it, its Interval when one is given.
	Group    string        `json:"group,omitempty"`
	NewGroup bool          `json:"new_group,omitempty"`
	Interval time.Duration `json:"interval_ns,omitempty"`
	// DataSource is, of Grafana, the data source its query reads; Tells,
	// of Grafana and a cloud's alarm, whom it tells: a contact point, an
	// action, a notification channel, each one the rules in that place
	// already tell.
	DataSource string   `json:"data_source,omitempty"`
	Tells      []string `json:"tells,omitempty"`
}

// Where is a place as a person reads it: its file, folder, namespace or
// alarms, then its group.
func (p addPlace) Where() string { return p.where() }

func (p addPlace) where() string {
	g := p.Group
	if p.NewGroup {
		g = "+ " + p.Group
	}
	switch p.Path {
	case ledger.PathGit:
		return p.File + ", " + g
	case ledger.PathGrafana:
		return p.Folder + ", " + g
	case ledger.PathRuler:
		return p.Namespace + ", " + g
	}
	return p.Alarms
}

// placeOffer is a place serve may add a rule to: what may be given there.
type placeOffer struct {
	addPlace
	// Key names it in a form; Name is how it is shown.
	Key, Name string
	// DataSources are, of Grafana, those the folder's rules read their
	// PromQL from; Tells, whom the rules there tell.
	DataSources, Tells []string
	// kind is a ruler's or an alarm's kind.
	kind string
	// account is, of CloudWatch's alarms, the account they are read in.
	account string
	// routed says, of a Grafana group, that a rule there names no contact
	// point: Grafana's notification policies route it by its labels.
	routed bool
}

// placeKey is what a place is named by in a form: a hash of where it is.
func placeKey(p addPlace) string {
	b, _ := json.Marshal([]any{p.Path, p.File, p.Folder, p.Namespace, p.Alarms, p.Group, p.NewGroup})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// addPlaces are the places serve may add a rule to: of the rules it reads,
// each group of a rule file under --rules (and a new group in it), each
// Grafana folder's group, each group of a namespace --changes-in names on
// a Mimir, Cortex or Amazon ruler (and a new group in it), and each
// CloudWatch or Google Cloud --alarms with alarms read. Nothing else: no
// path is ever taken from what a proposal says, only matched to these.
func (s *Server) addPlaces(ctx context.Context) ([]placeOffer, error) {
	latest, ok, err := s.l.Latest(ctx)
	if err != nil || !ok {
		return nil, err
	}
	full, err := s.l.VersionByID(ctx, latest.ID)
	if err != nil {
		return nil, err
	}
	all, err := rules.FromDocs(full.Docs)
	if err != nil {
		return nil, err
	}
	var out []placeOffer
	seen := map[string]int{}
	add := func(o placeOffer) *placeOffer {
		o.Key = placeKey(o.addPlace)
		if i, ok := seen[o.Key]; ok {
			return &out[i]
		}
		seen[o.Key] = len(out)
		out = append(out, o)
		return &out[len(out)-1]
	}
	fits := map[string]bool{} // a file a new group fits, by its source
	for _, r := range all {
		d := docOf(r, full.Docs)
		switch {
		case s.gitPlace(r, d):
			file := s.docName(d.Source)
			add(placeOffer{addPlace: addPlace{Path: ledger.PathGit, File: file, Group: r.Group}})
			ok, known := fits[d.Source]
			if !known {
				ok = rules.NewGroupFits(d.Data)
				fits[d.Source] = ok
			}
			if ok {
				add(placeOffer{addPlace: addPlace{Path: ledger.PathGit, File: file, NewGroup: true}})
			}
		case s.grafanaPlace(r, d):
			o := add(placeOffer{addPlace: addPlace{Path: ledger.PathGrafana, Folder: r.Folder, Group: strings.TrimPrefix(r.Group, r.Folder+"/")}})
			for _, src := range r.Graph.Data {
				if src.UID != "" && !slices.Contains(o.DataSources, src.UID) {
					o.DataSources = append(o.DataSources, src.UID)
				}
			}
			if r.Receiver != "" && !slices.Contains(o.Tells, r.Receiver) {
				o.Tells = append(o.Tells, r.Receiver)
			}
			if r.Receiver == "" {
				o.routed = true
			}
		case s.rulerPlace(r, d):
			add(placeOffer{addPlace: addPlace{Path: ledger.PathRuler, Namespace: r.Namespace, Group: r.Group}, kind: s.ruler.Kind})
			add(placeOffer{addPlace: addPlace{Path: ledger.PathRuler, Namespace: r.Namespace, NewGroup: true}, kind: s.ruler.Kind})
		}
	}
	for _, a := range s.alarms {
		if a.Kind != source.AlarmsAWS && a.Kind != source.AlarmsGoogle {
			continue
		}
		docs, err := a.Docs(ctx, s.alarmsEvery())
		if err != nil || len(docs) == 0 {
			continue // none read: nothing says whom alarms there tell
		}
		o := placeOffer{addPlace: addPlace{Path: ledger.PathAlarm, Alarms: a.Source}, kind: a.Kind}
		for _, d := range docs {
			for _, t := range alarmTells(a.Kind, d.Data) {
				if !slices.Contains(o.Tells, t) {
					o.Tells = append(o.Tells, t)
				}
			}
			if a.Kind == source.AlarmsAWS {
				acct := ""
				if parts := strings.SplitN(d.Source, ":", 6); len(parts) == 6 {
					acct = parts[4]
				}
				if o.account != "" && o.account != acct {
					o.account = "?" // read in more than one: which isn't known
				} else if o.account == "" {
					o.account = acct
				}
			}
		}
		if a.Kind == source.AlarmsAWS && (o.account == "" || o.account == "?") {
			continue
		}
		add(o)
	}
	for i := range out {
		slices.Sort(out[i].DataSources)
		slices.Sort(out[i].Tells)
		out[i].Name = out[i].where()
	}
	slices.SortFunc(out, func(a, b placeOffer) int { return strings.Compare(a.Path+"\x00"+a.Name, b.Path+"\x00"+b.Name) })
	return out, nil
}

// gitPlace reports whether a rule's file is one a rule may be added to by
// the git path: a Prometheus rule file Lapilli read under --rules, not an
// API's answer, a Loki ruler's or a Grafana provisioning file.
func (s *Server) gitPlace(r rules.Rule, d rules.Doc) bool {
	if s.cfg.RulesPath == "" || s.cfg.Grafana != "" || s.ruler != nil || d.API || len(d.Data) == 0 || d.Logs != "" || d.Namespace != "" ||
		r.Cloud != "" || r.UID != "" || r.Graph != nil || !r.Prometheus || r.Group == "" {
		return false
	}
	if d.Source == s.cfg.RulesPath {
		return !ciPath(filepath.Base(d.Source)) // --rules names the file itself
	}
	rel, err := filepath.Rel(s.cfg.RulesPath, d.Source)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) && !ciPath(rel)
}

// grafanaPlace reports whether a rule is a Grafana rule read from Grafana's
// API, replayed as PromQL: its folder and group a place.
func (s *Server) grafanaPlace(r rules.Rule, d rules.Doc) bool {
	return s.cfg.Grafana != "" && d.Source == s.cfg.Grafana && r.UID != "" && r.Graph != nil && r.Graph.Logs == "" && r.Folder != "" &&
		strings.HasPrefix(r.Group, r.Folder+"/")
}

// rulerPlace reports whether a rule is a Mimir's, a Cortex's or an Amazon
// ruler's in a namespace and group --changes-in names: not an Azure rule
// group's, whose rules carry action groups and scopes of their own.
func (s *Server) rulerPlace(r rules.Rule, d rules.Doc) bool {
	if s.ruler == nil || s.ruler.Kind == source.RulerAzure || r.Cloud != "" || d.Logs != "" || d.Namespace == "" || r.Namespace != d.Namespace {
		return false
	}
	return rulerTarget{s}.allowed("", rules.Edit{Namespace: r.Namespace, Group: r.Group}) == ""
}

// alarmTells are whom a cloud's alarm tells, as a rule added may: of
// CloudWatch's actions, those that only tell (tellsOnly: an SNS topic, an
// OpsItem, a response plan; a Lambda function, an automation or any other
// is never offered), a Google policy's notification channels.
func alarmTells(kind string, raw []byte) []string {
	m, err := decodeTree(raw)
	if err != nil {
		return nil
	}
	var out []string
	switch kind {
	case source.AlarmsAWS:
		alarm, composite := cwAlarmOf(m)
		if composite {
			return nil
		}
		for _, k := range []string{"AlarmActions", "OKActions", "InsufficientDataActions"} {
			for _, x := range list(alarm[k]) {
				if s, _ := x.(string); tellsOnly.MatchString(s) {
					out = append(out, s)
				}
			}
		}
	case source.AlarmsGoogle:
		for _, x := range list(m["notificationChannels"]) {
			if s, _ := x.(string); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// errNoPlace is a place given that serve doesn't add rules to: one reason
// for all, whether it exists or not.
var errNoPlace = bad("that isn't a place this lapilli serve adds rules to: pick one the new rule's form offers (a group of a rule file, a Grafana folder's group, a ruler's namespace --changes-in names, a cloud's alarms it reads)")

// errNoTell is whom a rule added would tell that the rules in its place
// don't already: refused as a place is, saying nothing of what exists.
var errNoTell = bad("an added rule tells only whom the rules in its place already tell: pick among those the new rule's form offers")

// placeFor is the offer a place given is, and the place as it is to be:
// its path and where as one offered, a group's name for a new one, an
// interval of an hour at most for a new group only, a data source of
// Grafana's folder's (the only one, when it reads one and none is given),
// and whom it tells, each among the offer's.
func placeFor(offers []placeOffer, want addPlace) (placeOffer, addPlace, error) {
	probe := want
	if probe.NewGroup {
		probe.Group = ""
	}
	i := slices.IndexFunc(offers, func(o placeOffer) bool { return o.Key == placeKey(probe) })
	if i < 0 {
		return placeOffer{}, addPlace{}, errNoPlace
	}
	o := offers[i]
	p := o.addPlace
	p.Group, p.Tells, p.DataSource, p.Interval = want.Group, nil, "", 0
	if !p.NewGroup {
		p.Group = o.Group
	}
	switch {
	case p.NewGroup && !rules.AddedName.MatchString(p.Group):
		return o, p, bad("a new group's name is letters, digits, -, _, . and :, 200 at most")
	case want.Interval != 0 && (!p.NewGroup || want.Interval < 0 || want.Interval > time.Hour || want.Interval%time.Second != 0):
		return o, p, bad("a new group's interval is whole seconds, an hour at most; a group there keeps its own")
	}
	p.Interval = want.Interval
	if p.Path == ledger.PathGrafana {
		p.DataSource = want.DataSource
		if p.DataSource == "" && len(o.DataSources) == 1 {
			p.DataSource = o.DataSources[0]
		}
		if !slices.Contains(o.DataSources, p.DataSource) {
			return o, p, bad("give the data source its query reads: one the rules in its folder read")
		}
	}
	for _, t := range want.Tells {
		if !slices.Contains(o.Tells, t) {
			return o, p, errNoTell
		}
		if !slices.Contains(p.Tells, t) {
			p.Tells = append(p.Tells, t)
		}
	}
	if len(p.Tells) > 0 && p.Path != ledger.PathGrafana && p.Path != ledger.PathAlarm {
		return o, p, bad("a rule added to a rule file or a ruler tells whom its labels route it to: it names no one itself")
	}
	if len(p.Tells) > 1 && p.Path == ledger.PathGrafana {
		return o, p, bad("a Grafana rule tells one contact point at most")
	}
	if err := grafanaRouted(p, o.Tells, o.routed); err != nil {
		return o, p, err
	}
	return o, p, nil
}

// errNamesNone is a Grafana rule added that names no contact point where
// every rule of its group names one.
var errNamesNone = bad("every rule in its group names its contact point: name one of theirs, so that it tells only whom they tell")

// grafanaRouted holds a Grafana rule added to whom the rules of its group
// tell (tells, and routed when one of them names none): where each names a
// contact point, it names one of theirs; named none, Grafana's notification
// policies would route it by its labels to whichever contact point they
// match. Where none of them names one, they and it are so routed.
func grafanaRouted(p addPlace, tells []string, routed bool) error {
	if p.Path == ledger.PathGrafana && len(p.Tells) == 0 && len(tells) > 0 && !routed {
		return errNamesNone
	}
	return nil
}

// addAsk is what a proposal adding a rule is asked for.
type addAsk struct {
	Rule   string // the rule, as given
	Place  addPlace
	Expect string            // what it is held to, as an expectations file
	Tests  map[string]string // the test files its scenarios name, by path
	From   string            // person or agent
	Reason string
}

// addEvidence is what a proposal adding a rule carries beside its
// backtest: where it goes, the rule as Lapilli wrote it, and what holding
// it to its expectations found, item by item.
type addEvidence struct {
	Place addPlace    `json:"place"`
	Rule  string      `json:"rule"`
	Items []draftItem `json:"items"`
	Weeks int         `json:"weeks"`
	Note  string      `json:"note,omitempty"`
	// Unreplayed says the backtest couldn't replay the rule: why.
	Unreplayed string `json:"unreplayed,omitempty"`
}

// addBuilt is a rule added, written where it goes and read back.
type addBuilt struct {
	place  addPlace
	offer  placeOffer
	added  rules.Added
	rule   rules.Rule // as read from where it goes, under its name
	edit   rules.Edit
	source string // the proposal's source: a file, Grafana, a ruler's unit, an alarm's ID
	// before is the document or unit as it was read, doc it with the rule
	// added; docHash the hash a path finds it by.
	before, doc []byte
	docHash     string
	diff        string
	// group is, for a scenario, the rule's group with it added, as
	// promtool reads one; nil where promtool doesn't run it.
	group []byte
}

// heldAdded reads what a rule added is held to: an expectations file
// holding it by rule: NAME alone, one expectation at least (an added rule
// held to nothing is refused), with the test files its scenarios name
// (each bounded as an agent's are), and none it doesn't.
func heldAdded(name, text string, tests map[string]string) (*verdict.Expectations, map[string][]byte, error) {
	if err := agentText("expectations", text, ledger.MaxDraftText); err != nil {
		return nil, nil, bad("its expectations: %s", err.Error())
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil, bad("an added rule is held to what it is to do: give its expectations, one at least (a budget, a catch or a scenario)")
	}
	e, err := verdict.ReadExpectations(strings.NewReader(text))
	if err != nil {
		return nil, nil, bad("its expectations: %s", clip(err.Error(), 300))
	}
	n := 0
	named := map[string]bool{}
	for _, x := range e.Expectations {
		if x.Rule != name {
			return nil, nil, bad("its expectations hold it by rule: %s alone", verdict.Clean(name))
		}
		n += len(x.Items())
		for _, p := range x.Scenarios {
			named[p] = true
		}
	}
	if n == 0 {
		return nil, nil, bad("an added rule is held to what it is to do: give its expectations, one at least (a budget, a catch or a scenario)")
	}
	given, err := cleanTests(tests)
	if err != nil {
		return nil, nil, bad("its test files: %s", err.Error())
	}
	for p := range given {
		if !named[p] {
			return nil, nil, bad("test file %s: no scenario its expectations name", verdict.Clean(p))
		}
	}
	held, err := verdict.NewExpectations(e.Expectations)
	if err != nil {
		return nil, nil, bad("its expectations: %s", clip(err.Error(), 300))
	}
	return held, given, nil
}

// buildAdd writes the rule where its place says, as that place holds a
// rule, from the rules read again where they are kept, and reads it back:
// the document or unit with it, and the rule as Lapilli would read it
// there.
func (s *Server) buildAdd(ctx context.Context, a rules.Added, p addPlace, o placeOffer) (*addBuilt, error) {
	if err := addedTemplates(a, p.Path); err != nil {
		return nil, err
	}
	b := &addBuilt{place: p, offer: o, added: a, edit: rules.Edit{Name: a.Alert, Group: p.Group}}
	switch p.Path {
	case ledger.PathGit:
		return b, s.buildGit(ctx, b)
	case ledger.PathRuler:
		return b, s.buildRuler(ctx, b)
	case ledger.PathGrafana:
		return b, s.buildGrafana(ctx, b)
	case ledger.PathAlarm:
		return b, s.buildAlarm(ctx, b)
	}
	return nil, errNoPlace
}

// addedTemplates holds a rule added's labels and annotations to what a
// change to what a rule says is held to (saidOK, templateOK): each a
// template where the rule is kept, which must parse in its language,
// calling only what it has, never query, nor growing without bound each
// time the alert fires; a summary or a description 4 KB at most, a runbook
// an https URL. Prometheus refuses a whole rule file for one that doesn't
// parse, every rule in it with it. A cloud's alarm's text isn't a Go
// template: its runbook is held to the URL alone.
func addedTemplates(a rules.Added, path string) error {
	lang := rules.TemplatePrometheus
	if path == ledger.PathGrafana {
		lang = rules.TemplateGrafana
	}
	for _, k := range slices.Sorted(maps.Keys(a.Annotations)) {
		v := a.Annotations[k]
		switch {
		case path == ledger.PathAlarm && (k == "runbook_url" || k == "runbook"):
			if err := saidOK(k, v, rules.TemplatePrometheus); err != nil {
				return err
			}
		case path == ledger.PathAlarm:
		case slices.Contains(saysKeys, k):
			if err := saidOK(k, v, lang); err != nil {
				return err
			}
		default:
			if err := templateOK(v, lang); err != nil {
				return err
			}
		}
	}
	if path != ledger.PathAlarm {
		for _, k := range slices.Sorted(maps.Keys(a.Labels)) {
			if err := templateOK(a.Labels[k], lang); err != nil {
				return err
			}
		}
	}
	return nil
}

// ciPath reports whether a file's path, under --rules, is, or is under,
// what a CI system or a tool a push sets off runs: never a place, as the
// change recipes never write one (their ci, a second lock).
func ciPath(rel string) bool {
	p := "/" + strings.ToLower(filepath.ToSlash(rel))
	for _, dir := range []string{".github", ".gitea", ".forgejo", ".gitlab", ".azuredevops", ".azure-pipelines", ".circleci", ".buildkite",
		".woodpecker", ".tekton", ".drone", ".travis", ".semaphore", ".harness", ".teamcity", ".devcontainer", ".husky", ".githooks", ".git"} {
		if strings.Contains(p+"/", "/"+dir+"/") {
			return true
		}
	}
	base := p[strings.LastIndex(p, "/")+1:]
	for _, x := range []string{"gitlab-ci.yml", "gitlab-ci.yaml", "bitbucket-pipelines.yml", ".drone.yml", ".drone.yaml", ".woodpecker.yml",
		".woodpecker.yaml", ".travis.yml", "appveyor.yml", ".appveyor.yml", ".cirrus.yml", "codemagic.yaml", "bitrise.yml", "codefresh.yml",
		"wercker.yml", ".pre-commit-config.yaml", ".pre-commit-hooks.yaml", "package.json", "renovate.json", "taskfile.yml", "taskfile.yaml",
		"skaffold.yaml", ".goreleaser.yml", ".goreleaser.yaml"} {
		if base == x || (strings.HasPrefix(x, "gitlab-ci") && strings.HasSuffix(base, x)) {
			return true
		}
	}
	for _, pre := range []string{"azure-pipelines", "cloudbuild", "buildspec", "jenkinsfile", ".renovaterc"} {
		if strings.HasPrefix(base, pre) {
			return true
		}
	}
	switch filepath.Ext(base) {
	case ".yml", ".yaml", ".json", ".rules":
		return false
	}
	return true // not named as rules are
}

// readBackAdded is the rule added as rules read from a document: one rule
// more than before, of its name and group, every other as it was.
func readBackAdded(before, after []rules.Rule, name, group string) (rules.Rule, error) {
	left := map[string]int{}
	for _, x := range before {
		left[x.Key()]++
	}
	var got []rules.Rule
	for _, x := range after {
		if left[x.Key()] > 0 {
			left[x.Key()]--
			continue
		}
		got = append(got, x)
	}
	for _, n := range left {
		if n != 0 {
			return rules.Rule{}, errAddUnread
		}
	}
	if len(got) != 1 || got[0].Name != name || (group != "" && got[0].Group != group) || got[0].Unsupported != "" {
		return rules.Rule{}, errAddUnread
	}
	return got[0], nil
}

// ruleRefused is a rule given that isn't read: why, as the rules package
// says it.
func ruleRefused(err error) error {
	var e rules.ErrAdded
	if errors.As(err, &e) {
		return bad("the rule: %s", e.Why)
	}
	return bad("the rule: %s", clip(err.Error(), 300))
}

// errAddUnread is a rule added that doesn't read back as given where it
// goes.
var errAddUnread = bad("the rule can't be added there as it reads: add it where it is written")

func (s *Server) buildGit(ctx context.Context, b *addBuilt) error {
	_, docs, err := s.readRules(ctx)
	if err != nil {
		return err
	}
	var d rules.Doc
	n := 0
	for _, x := range docs {
		if s.docName(x.Source) == b.place.File && !x.API && x.Logs == "" && x.Namespace == "" {
			d = x
			n++
		}
	}
	if n != 1 {
		return errNoPlace
	}
	out, err := rules.AddRule(d.Data, b.place.Group, b.place.NewGroup, b.place.Interval, b.added)
	if err != nil {
		return errAddUnread
	}
	was, err := rules.FromDocs([]rules.Doc{d})
	if err != nil {
		return errAddUnread
	}
	nd := d
	nd.Data = out
	is, err := rules.FromDocs([]rules.Doc{nd})
	if err != nil {
		return errAddUnread
	}
	if b.rule, err = readBackAdded(was, is, b.added.Alert, b.place.Group); err != nil {
		return err
	}
	b.source, b.before, b.doc, b.docHash = d.Source, d.Data, out, rules.Hash(d.Data)
	b.diff = rules.Diff(s.docName(d.Source), d.Data, out)
	b.group, _ = scenarioGroupOf(is, b.rule)
	return nil
}

// scenarioGroupOf is the group of a rule among rules, as promtool reads
// one, when promtool runs it.
func scenarioGroupOf(rs []rules.Rule, r rules.Rule) ([]byte, error) {
	if !scenario.Runs(r) {
		return nil, errors.New("not in Prometheus's format")
	}
	k := scenario.GroupKey(r.Source, r.Namespace, r.Group)
	var alerts []rules.Rule
	for _, x := range rs {
		if scenario.GroupKey(x.Source, x.Namespace, x.Group) == k {
			alerts = append(alerts, x)
		}
	}
	return scenario.Group(alerts, nil)
}

func (s *Server) buildRuler(ctx context.Context, b *addBuilt) error {
	tg := rulerTarget{s}
	if s.ruler == nil || s.ruler.Kind == source.RulerAzure {
		return errNoPlace
	}
	b.edit.Namespace = b.place.Namespace
	if why := tg.allowed("", b.edit); why != "" {
		return errNoPlace
	}
	u, err := tg.unit(ctx, "", b.edit)
	if errors.Is(err, source.ErrUnreadable) {
		return errAddUnread
	}
	if err != nil {
		return err
	}
	if why := tg.managed(u); why != "" {
		return errNoPlace // another tool keeps it
	}
	var out []byte
	switch {
	case s.ruler.Kind == source.RulerConfig && b.place.NewGroup:
		// The unit is the group: one gone is made, holding the rule alone.
		if !u.Gone {
			return bad("a group of that name is in the namespace: pick it, or name the new one apart")
		}
		out, err = newGroupFile(b.place.Group, b.place.Interval, b.added)
	case u.Gone:
		return errNoPlace
	default:
		out, err = rules.AddRule(u.Raw, b.place.Group, b.place.NewGroup, b.place.Interval, b.added)
	}
	if err != nil || !rules.AddsRule(u.Raw, out, b.place.Group, b.place.Interval, b.added) {
		return errAddUnread
	}
	was, err := tg.rules(u)
	if err != nil {
		return err
	}
	is, err := tg.rules(apiUnit{Raw: out, ruler: source.Unit{Source: u.ruler.Source, Namespace: u.ruler.Namespace}})
	if err != nil {
		return errAddUnread
	}
	if b.rule, err = readBackAdded(was, is, b.added.Alert, b.place.Group); err != nil {
		return err
	}
	b.source, b.before, b.doc, b.docHash = u.ruler.Source, u.Raw, out, u.Hash
	b.diff = rules.Diff(tg.unitName("", b.edit), u.Raw, out)
	b.group, _ = scenarioGroupOf(is, b.rule)
	return nil
}

// newGroupFile is a rule file of one group, made for the rule, as a Mimir's
// or a Cortex's group is written.
func newGroupFile(group string, interval time.Duration, a rules.Added) ([]byte, error) {
	out, err := rules.AddRule([]byte("groups:\n  - name: placeholder\n    rules:\n      - alert: Placeholder\n        expr: vector(1)\n"), group, true, interval, a)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil || len(doc.Content) != 1 {
		return nil, errAddUnread
	}
	gs := doc.Content[0].Content[1]
	gs.Content = gs.Content[1:] // the placeholder out
	return yamlOut(&doc)
}

func yamlOut(n *yaml.Node) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// grafanaAdded is a rule added to Grafana as its provisioning API takes
// it, and as its export writes it: one query of the data source given,
// instant, its expression the rule's; a math expression firing on every
// series it returns, as Grafana's own conversion of a Prometheus rule
// writes one (is_number, is_nan or is_inf of it); no data OK, as a
// Prometheus rule that returns nothing doesn't fire; its for,
// keep_firing_for, labels and annotations, and the contact point it tells
// when one is given. Lapilli writes every field: nothing given is passed
// on as it was given.
func grafanaAdded(a rules.Added, p addPlace, uid string, export bool) map[string]any {
	query := map[string]any{"refId": "A", "datasourceUid": p.DataSource, "relativeTimeRange": map[string]any{"from": 600, "to": 0},
		"model": map[string]any{"refId": "A", "expr": a.Expr, "instant": true, "range": false}}
	cond := map[string]any{"refId": "B", "datasourceUid": "__expr__",
		"model": map[string]any{"refId": "B", "type": "math", "expression": "is_number($A) || is_nan($A) || is_inf($A)"}}
	r := map[string]any{"uid": uid, "title": a.Alert, "condition": "B", "data": []any{query, cond},
		"noDataState": "OK", "execErrState": "Error", "for": verdict.Duration(a.For), "isPaused": false}
	keep := "keep_firing_for"
	if export {
		keep = "keepFiringFor"
	}
	if a.KeepFiringFor > 0 {
		r[keep] = verdict.Duration(a.KeepFiringFor)
	}
	if len(a.Labels) > 0 {
		r["labels"] = a.Labels
	}
	if len(a.Annotations) > 0 {
		r["annotations"] = a.Annotations
	}
	if len(p.Tells) == 1 {
		r["notification_settings"] = map[string]any{"receiver": p.Tells[0]}
	}
	return r
}

// newUID is a uid for a Grafana rule added: Lapilli's, so that it is read
// back by it.
func newUID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "lapilli-" + hex.EncodeToString(b)
}

func (s *Server) buildGrafana(ctx context.Context, b *addBuilt) error {
	if !grafanaUID.MatchString("lapilli-00000000000000000000") {
		return errNoPlace
	}
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return bad("the rules haven't been read yet: try again in a minute")
	}
	full, err := s.l.VersionByID(ctx, latest.ID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(full.Docs, func(d rules.Doc) bool { return d.Source == s.cfg.Grafana })
	if i < 0 {
		return errNoPlace
	}
	tree, err := rules.UnmarshalYAML(full.Docs[i].Data)
	if err != nil {
		return err
	}
	m, _ := tree.(map[string]any)
	var head map[string]any
	for _, g := range list(m["groups"]) {
		gm, _ := g.(map[string]any)
		if gm != nil && str(gm["folder"]) == b.place.Folder && str(gm["name"]) == b.place.Group {
			head = map[string]any{"orgId": gm["orgId"], "name": gm["name"], "folder": gm["folder"], "interval": gm["interval"]}
		}
	}
	if head == nil {
		return errNoPlace
	}
	b.edit.UID = newUID()
	b.edit.Group = b.place.Folder + "/" + b.place.Group
	head["rules"] = []any{grafanaAdded(b.added, b.place, b.edit.UID, true)}
	doc, err := json.Marshal(map[string]any{"apiVersion": 1, "groups": []any{head}})
	if err != nil {
		return err
	}
	rs, err := rules.FromDocs([]rules.Doc{{Source: s.cfg.Grafana, Data: doc}})
	if err != nil || len(rs) != 1 || rs[0].Name != b.added.Alert || rs[0].Unsupported != "" {
		return errAddUnread
	}
	body, err := json.MarshalIndent(grafanaAdded(b.added, b.place, b.edit.UID, false), "", "  ")
	if err != nil {
		return err
	}
	b.rule, b.source, b.doc = rs[0], s.cfg.Grafana, body
	b.diff = rules.Diff(b.place.Folder+"/"+b.place.Group+"/"+b.added.Alert, nil, body)
	return nil
}

// cloudSays are the annotations a cloud's alarm added says: CloudWatch's
// one description (a runbook a link in it; its summary is its name),
// Google's documentation's subject, content and link.
var cloudSays = map[string][]string{source.AlarmsAWS: {"description", "runbook_url"}, source.AlarmsGoogle: {"summary", "description", "runbook_url"}}

func (s *Server) buildAlarm(ctx context.Context, b *addBuilt) error {
	a := s.alarmsOf(b.place.Alarms)
	if a == nil {
		return errNoPlace
	}
	b.edit.Namespace, b.edit.Group = a.Source, ""
	for k := range b.added.Annotations {
		if !slices.Contains(cloudSays[a.Kind], k) {
			return bad("a cloud's alarm says %s alone: %s has no place there", strings.Join(cloudSays[a.Kind], ", "), k)
		}
	}
	if !s.changesInAlarm(a.Source, b.added.Alert, a.Kind) {
		return errNoPlace // --changes-in doesn't name it there
	}
	var doc map[string]any
	switch a.Kind {
	case source.AlarmsAWS:
		if len(b.added.Labels) > 0 {
			return bad("a CloudWatch alarm carries no labels: leave them out")
		}
		b.source = cwARNLike(ctx, a, b.offer.account, b.added.Alert)
		if a.OwnsID(b.source) != nil {
			return errNoPlace
		}
		desc := rules.WithRunbook(b.added.Annotations["description"], b.added.Annotations["runbook_url"], b.added.Annotations["runbook_url"] == "")
		alarm := map[string]any{"AlarmName": b.added.Alert, "ActionsEnabled": true,
			"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": b.added.Expr, "PendingPeriod": int64(b.added.For / time.Second),
				"RecoveryPeriod": int64(b.added.KeepFiringFor / time.Second)}},
			"EvaluationInterval": 60}
		if desc != "" {
			if len([]rune(desc)) > 1024 {
				return bad("a CloudWatch alarm's description is 1,024 characters at most")
			}
			alarm["AlarmDescription"] = desc
		}
		if len(b.place.Tells) > 0 {
			alarm["AlarmActions"], alarm["OKActions"] = slices.Clone(b.place.Tells), slices.Clone(b.place.Tells)
		}
		doc = map[string]any{"MetricAlarms": []any{alarm}}
	case source.AlarmsGoogle:
		i := strings.Index(a.URL, "/v3/")
		if i < 0 {
			return errNoPlace
		}
		// No policy has this name: Google names one as it makes it, and
		// it is read back by the name it answers with (madeAs).
		b.source = strings.TrimSuffix(a.URL[i+len("/v3/"):], "/") + "/lapilli-added"
		pq := map[string]any{"query": b.added.Expr, "duration": fmt.Sprintf("%ds", int64(b.added.For/time.Second)), "alertRule": b.added.Alert}
		if len(b.added.Labels) > 0 {
			pq["labels"] = b.added.Labels
		}
		doc = map[string]any{"displayName": b.added.Alert, "combiner": "OR", "enabled": true,
			"conditions": []any{map[string]any{"displayName": b.added.Alert, "conditionPrometheusQueryLanguage": pq}}}
		if len(b.added.Annotations) > 0 {
			d := map[string]any{"mimeType": "text/markdown"}
			if v := b.added.Annotations["summary"]; v != "" {
				d["subject"] = v
			}
			if v := b.added.Annotations["description"]; v != "" {
				d["content"] = v
			}
			if v := b.added.Annotations["runbook_url"]; v != "" {
				d["links"] = []any{map[string]any{"displayName": "Runbook", "url": v}}
			}
			doc["documentation"] = d
		}
		if len(b.place.Tells) > 0 {
			doc["notificationChannels"] = slices.Clone(b.place.Tells)
		}
		if b.added.KeepFiringFor > 0 {
			return bad("a Google Cloud policy closes its alerts itself: leave keep_firing_for out")
		}
	default:
		return errNoPlace
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if raw, err = rules.CanonJSON(raw); err != nil {
		return err
	}
	rs, err := rules.FromDocs([]rules.Doc{{Source: b.source, Namespace: a.Source, Data: raw}})
	if err != nil || len(rs) != 1 || rs[0].Unsupported != "" || rs[0].Name != b.added.Alert {
		return errAddUnread
	}
	b.rule, b.doc = rs[0], raw
	b.diff = rules.Diff(a.Source+"/"+b.added.Alert, nil, raw)
	return nil
}

// cwARNLike is a CloudWatch alarm's ARN in --alarms's partition and region,
// as an alarm read there is written.
func cwARNLike(ctx context.Context, a *source.Alarms, account, name string) string {
	part, region := "aws", ""
	if docs, err := a.Docs(ctx, time.Hour); err == nil {
		for _, d := range docs {
			if p := strings.SplitN(d.Source, ":", 6); len(p) == 6 {
				part, region = p[1], p[3]
				break
			}
		}
	}
	return fmt.Sprintf("arn:%s:cloudwatch:%s:%s:alarm:%s", part, region, account, name)
}

// nameInEffect reports whether a rule in effect, or one read again where
// rules are kept, is named so: an added rule is named apart from every one,
// not its group's alone, as a proposal, its expectations and the ledger
// name a rule by its name.
func (s *Server) nameInEffect(ctx context.Context, name string) (bool, error) {
	latest, ok, err := s.l.Latest(ctx)
	if err != nil || !ok {
		return false, err
	}
	all, names, err := s.named(ctx, latest.ID)
	if err != nil {
		return false, err
	}
	for i, r := range all {
		base, _, _ := strings.Cut(names[i], " [")
		if r.Name == name || base == name || r.AlertName() == name {
			return true, nil
		}
	}
	return false, nil
}

// errNameTaken is an added rule named as one in effect.
var errNameTaken = bad("a rule in effect is called that: name the new rule apart from every rule, as its proposal, its expectations and its history name it by its name")

// checkAdd replays a rule added over the twelve weeks to now, with nothing
// beside it, and holds it to its expectations: what step 3's
// test_expectation does, for a rule not yet in effect. Its scenarios run
// against its group with it added. wait says to wait for a comparison
// running (replayHeld), as carrying it out does.
func (s *Server) checkAdd(ctx context.Context, b *addBuilt, held *verdict.Expectations, tests map[string][]byte, agent, wait bool) (*checked, error) {
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, bad("the rules haven't been read yet: try again in a minute")
	}
	current, err := s.parse(ctx, latest)
	if err != nil {
		return nil, err
	}
	r := b.rule
	r.Name = b.added.Alert
	c := &checked{out: testExpectOut{Rule: r.Name, HeldBy: "given", Items: []draftItem{}, Lost: []string{}}}
	runner := func(ctx context.Context) (verdict.ScenarioRunner, error) {
		d := &draftRun{ctx: ctx, s: s, tests: tests, agent: agent}
		switch {
		case s.cfg.Promtool == "" || len(tests) == 0:
		case b.group == nil:
			d.why = [2]string{"not a rule in Prometheus's format, which promtool runs", "not a rule in Prometheus's format, which promtool runs"}
		default:
			d.groups = [2][]byte{b.group, b.group}
		}
		return d, nil
	}
	if _, err := s.replayHeld(ctx, c, held, nil, []rules.Rule{r}, len(current), runner, wait); err != nil {
		return nil, err
	}
	return c, nil
}

// addHeldWhy says why a proposal adding a rule isn't approved, as its
// evidence says now: an expectation it breaks, a backtest that couldn't
// replay it, or nothing it is held to; "" when it may be.
func addHeldWhy(ev evidence) string {
	x := ev.Add
	switch {
	case x == nil:
		return ""
	case len(x.Items) == 0:
		return addHeldNothing
	case x.Unreplayed != "" || ev.Backtest == nil || ev.Backtest.Unfinished:
		return addHeldUnreplayed
	}
	for _, it := range x.Items {
		if it.State == verdict.ExpectBroken {
			return addHeldBroken
		}
	}
	return ""
}

// Why a proposal adding a rule isn't approved, in English: keys into the
// catalogs.
const (
	addHeldBroken     = "it breaks an expectation it is held to: it isn't approved until its backtest meets them, or what is not known stays not known"
	addHeldUnreplayed = "its backtest didn't replay it to the end: backtest it again, or propose it again"
	addHeldNothing    = "it is held to nothing: a rule added is held to one expectation at least"
)

// addItemsOf are what holding a rule added found, as its evidence keeps
// it, and whether the replay couldn't replay it, why.
func addItemsOf(c *checked) ([]draftItem, string) {
	why := ""
	for _, it := range c.out.Items {
		if strings.HasPrefix(it.Why, "it couldn't be replayed") {
			why = it.Why
		}
	}
	if c.out.Unfinished {
		why = "the replay didn't finish in time"
	}
	return c.out.Items, why
}

// addBacktest is a rule added's backtest, as other proposals' are kept:
// nothing before it, its firings and catches after.
func (s *Server) addBacktest(ctx context.Context, c *checked) (*backtest, error) {
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	kept, err := s.l.RecordKept(ctx, c.out.From, c.out.To, o)
	if err != nil {
		return nil, err
	}
	no := !kept
	return &backtest{From: c.out.From, To: c.out.To, After: side{Firings: c.out.Fired[1], White: c.out.Caught[1]},
		Lost: []string{}, Gained: []string{}, Later: []string{}, Unfinished: c.out.Unfinished, NoRecord: &no, At: s.now().UTC().Truncate(time.Second)}, nil
}

// proposeAdd makes a proposal adding a rule: refused before anything is
// read again or replayed when the hour's proposals are made, the rule
// given doesn't read, a rule in effect has its name, its place isn't one
// offered, or it is held to nothing; then a try is taken, the rule written
// where it goes from the rules read again, read back, replayed and held
// to its expectations, and kept, whatever they found: approval is what
// they hold up.
func (s *Server) proposeAdd(ctx context.Context, q addAsk, by string, agent bool) (int64, error) {
	if !s.replaying() {
		return 0, bad("this lapilli serve reads no rules of its own: there is nothing to add to")
	}
	a, err := rules.ParseAdded([]byte(q.Rule))
	if err != nil {
		return 0, ruleRefused(err)
	}
	if err := s.mayPropose(ctx, a.Alert, by, agent); err != nil {
		return 0, err
	}
	if taken, err := s.nameInEffect(ctx, a.Alert); err != nil {
		return 0, err
	} else if taken {
		return 0, errNameTaken
	}
	offers, err := s.addPlaces(ctx)
	if err != nil {
		return 0, err
	}
	o, place, err := placeFor(offers, q.Place)
	if err != nil {
		return 0, err
	}
	held, tests, err := heldAdded(a.Alert, q.Expect, q.Tests)
	if err != nil {
		return 0, err
	}
	if !s.tries.take(by, agent, time.Now()) {
		return 0, errTooManyTries
	}
	b, err := s.buildAdd(ctx, a, place, o)
	if err != nil {
		return 0, err
	}
	c, err := s.checkAdd(ctx, b, held, tests, agent, false)
	if err != nil {
		return 0, err
	}
	text, err := verdict.ExpectationsYAML(held.Expectations)
	if err != nil {
		return 0, err
	}
	expect, err := json.Marshal(ledger.AddedExpect{Text: string(text), Tests: q.Tests})
	if err != nil {
		return 0, err
	}
	ev := evidence{From: cmpOr(q.From, "person"), Reason: q.Reason, Version: s.versionOf(), Semantics: semantics, Edit: &b.edit}
	if ev.Impact, err = s.impactHeld(ctx); err != nil {
		return 0, err
	}
	if ev.Backtest, err = s.addBacktest(ctx, c); err != nil {
		return 0, err
	}
	items, unreplayed := addItemsOf(c)
	ev.Add = &addEvidence{Place: place, Rule: string(a.YAML()), Items: items, Weeks: c.out.Weeks, Note: c.out.Note, Unreplayed: unreplayed}
	if ev.Backtest != nil {
		for _, in := range c.caughtBy() {
			ev.Backtest.Gained = append(ev.Backtest.Gained, in)
		}
	}
	p := ledger.Proposal{Rule: a.Alert, Kind: ledger.KindAdd, Path: place.Path, MadeBy: by, Agent: agent, Source: b.source, DocHash: b.docHash,
		Document: string(b.doc), After: ruleText(b.rule), AfterHash: ruleHash(b.rule), Diff: b.diff, Expect: string(expect)}
	if place.Path == ledger.PathGrafana || place.Path == ledger.PathAlarm {
		p.AfterHash = "" // read back from where it is made
	}
	if p.Evidence, err = json.Marshal(ev); err != nil {
		return 0, err
	}
	id, err := s.l.Propose(ctx, p)
	switch {
	case errors.Is(err, ledger.ErrProposed):
		return 0, bad("this rule has a proposal not yet finished with: one at a time, so that each is seen applied")
	case errors.Is(err, ledger.ErrTooManyProposals):
		return 0, ledger.ErrTooManyProposals
	}
	return id, err
}

// caughtBy are the incidents the rule checked caught, as its catches'
// items say: each "catches ID" met.
func (c *checked) caughtBy() []string {
	var out []string
	for _, it := range c.out.Items {
		if id, ok := strings.CutPrefix(it.What, "catches "); ok && it.State == verdict.ExpectMet {
			out = append(out, id)
		}
	}
	return out
}

// addedOf is a proposal adding a rule's rule, as Lapilli wrote it, read
// again from its evidence.
func addedOf(ev evidence) (rules.Added, error) {
	if ev.Add == nil {
		return rules.Added{}, refusal{"the proposal holds no rule to add"}
	}
	a, err := rules.ParseAdded([]byte(ev.Add.Rule))
	if err != nil {
		return a, refusal{err.Error()}
	}
	return a, nil
}

// addAgain holds a proposal adding a rule to its expectations again, as
// the rules and the history are now: the rule written where its place says
// from the rules read again, replayed over the twelve weeks to now. What
// it gives is the evidence anew, and the rule as built; a place no longer
// offered, or a rule in effect of its name since, is a refusal. wait says
// to wait for a comparison running, as carrying it out does: an approved
// add isn't failed for an agent's comparison meanwhile.
func (s *Server) addAgain(ctx context.Context, p ledger.Proposal, ev evidence, wait bool) (evidence, *addBuilt, error) {
	a, err := addedOf(ev)
	if err != nil {
		return ev, nil, err
	}
	if taken, err := s.nameInEffect(ctx, a.Alert); err != nil {
		return ev, nil, err
	} else if taken {
		return ev, nil, refusal{"a rule in effect is called that now: propose it again under another name"}
	}
	offers, err := s.addPlaces(ctx)
	if err != nil {
		return ev, nil, err
	}
	o, place, err := placeFor(offers, ev.Add.Place)
	if err != nil {
		return ev, nil, refusal{"its place isn't one this lapilli serve adds rules to any more: propose it again"}
	}
	x, err := s.l.ProposalExpect(ctx, p.ID)
	if err != nil {
		return ev, nil, err
	}
	held, tests, err := heldAdded(a.Alert, x.Text, x.Tests)
	if err != nil {
		return ev, nil, refusal{err.Error()}
	}
	b, err := s.buildAdd(ctx, a, place, o)
	var no badRequest
	if errors.As(err, &no) {
		return ev, nil, refusal{fmt.Sprintf(no.format, no.args...)}
	} else if err != nil {
		return ev, nil, err
	}
	if ev.Edit != nil && ev.Edit.UID != "" {
		b.edit.UID = ev.Edit.UID // Grafana's, as it was kept
	}
	c, err := s.checkAdd(ctx, b, held, tests, p.Agent, wait)
	if err != nil {
		return ev, nil, err
	}
	again := ev
	if again.Backtest, err = s.addBacktest(ctx, c); err != nil {
		return ev, nil, err
	}
	again.Backtest.Gained = append(again.Backtest.Gained, c.caughtBy()...)
	items, unreplayed := addItemsOf(c)
	add := *ev.Add
	add.Items, add.Weeks, add.Note, add.Unreplayed = items, c.out.Weeks, c.out.Note, unreplayed
	again.Add, again.Semantics, again.Version = &add, semantics, s.versionOf()
	if again.Impact, err = s.impactHeld(ctx); err != nil {
		return ev, nil, err
	}
	return again, b, nil
}

// addWorse says how a rule added, checked again, is worse than what was
// approved: an expectation broken now, or a backtest that didn't replay it
// to the end.
func addWorse(now evidence) string {
	switch addHeldWhy(now) {
	case addHeldBroken:
		return "checked again, it now breaks an expectation it is held to"
	case addHeldUnreplayed, addHeldNothing:
		return "checked again, its backtest didn't replay it to the end, so it couldn't be held to its expectations"
	}
	return ""
}

// placesView are the places a reader may add a rule to, for the new rule's
// form.
type placesView struct {
	Offers []placeOffer
	// DataSources and Tells are those of every place, each once, with the
	// places they are of: checked against the place picked.
	DataSources, Tells []string
}

func placesViewOf(offers []placeOffer) placesView {
	v := placesView{Offers: offers}
	for _, o := range offers {
		for _, d := range o.DataSources {
			if !slices.Contains(v.DataSources, d) {
				v.DataSources = append(v.DataSources, d)
			}
		}
		for _, t := range o.Tells {
			if !slices.Contains(v.Tells, t) {
				v.Tells = append(v.Tells, t)
			}
		}
	}
	slices.Sort(v.DataSources)
	slices.Sort(v.Tells)
	return v
}

// offerByKey is the offer a form's key names, its new group's name given.
func offerByKey(offers []placeOffer, key, group string) (addPlace, bool) {
	i := slices.IndexFunc(offers, func(o placeOffer) bool { return o.Key == key })
	if i < 0 {
		return addPlace{}, false
	}
	p := offers[i].addPlace
	if p.NewGroup {
		p.Group = group
	}
	return p, true
}

// durationOrZero reads an interval a form or an agent gives: "" is none.
func durationOrZero(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	d, err := ParseDuration(v)
	if err != nil {
		if n, err2 := strconv.Atoi(v); err2 == nil {
			return time.Duration(n) * time.Second, nil
		}
		return 0, bad("interval %q: a duration such as 1m", clip(v, 32))
	}
	return d, nil
}
