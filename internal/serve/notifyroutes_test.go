package serve

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// routedHooks are the places a routed server tells: the webhook, Slack,
// two named webhooks (payments-hook signed with its own key), a
// --report-dest and a team's digest webhook, each a hook taking posts.
type routedHooks struct {
	webhook, slack, payments, checkout, ops, teamPay *signedHook
	paymentsKey                                      string
}

// routedServer is a serve with --config holding config, and every kind of
// destination a route may name.
func routedServer(t *testing.T, config string, change func(*Config)) (*Server, routedHooks, string) {
	t.Helper()
	hs := routedHooks{webhook: newSignedHook(t), slack: newSignedHook(t), payments: newSignedHook(t), checkout: newSignedHook(t),
		ops: newSignedHook(t), teamPay: newSignedHook(t), paymentsKey: "payments-key-of-thirty-two-bytes"}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "payments-key")
	os.WriteFile(keyFile, []byte(hs.paymentsKey), 0o600)
	cfgFile := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgFile, config)
	s, _ := server(t, func(c *Config) {
		c.Config, c.NotifyWebhook, c.Slack, c.NotifySlack = cfgFile, hs.webhook.file, hs.slack.file, true
		c.WebhookDests = map[string]string{"payments-hook": hs.payments.file, "checkout-hook": hs.checkout.file}
		c.WebhookDestSecrets = map[string]string{"payments-hook": keyFile}
		c.ReportDests = map[string]string{"ops": hs.ops.file}
		c.TeamDigests = map[string]string{"payments": hs.teamPay.file}
		c.ExternalURL = "https://lapilli.example.com"
		if change != nil {
			change(c)
		}
	})
	s.loadRoutes(time.Now(), true) // as Run does
	return s, hs, cfgFile
}

// outbox is what the outbox holds unsent, by place: each notice's event
// and body.
func outbox(t *testing.T, s *Server) map[string][]ledger.Notice {
	t.Helper()
	out := map[string][]ledger.Notice{}
	for _, d := range s.sendsTo() {
		ns, err := s.l.Due(context.Background(), d, time.Now().Add(time.Hour), 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(ns) > 0 {
			out[d] = ns
		}
	}
	return out
}

// events are the events of notices, in order.
func events(ns []ledger.Notice) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Event)
	}
	return out
}

const routesConfig = `teams: []
notify:
  routes:
    - kinds: [task.*]
      teams: [payments]
      to: [payments-hook, team:payments]
      continue: true
    - kinds: [task.created]
      to: [ops]
    - kinds: [window.kept]
      teams: [checkout]
      to: [checkout-hook]
  default: [slack]
  templates:
    ops: "*{{.Event}}* {{.Title}} <{{.Link}}|open> <!here>"
    payments-hook: "{{.Team}}: {{.Line}}"
`

// Routes send each event by its kind and team, the first that takes it
// deciding, the routes after it too while one says to go on, and what none
// takes to the default; nothing to the places no route names. The digest
// still goes where its own settings send it.
func TestRoutesSendByKindAndTeam(t *testing.T) {
	ctx := context.Background()
	s, _, _ := routedServer(t, routesConfig, nil)
	pay, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Pay"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	chk, err := s.l.PutTeam(ctx, ledger.Team{Name: "checkout", Rules: "^Chk"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.owners(ctx); err != nil { // as watchRoutes keeps them
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "tune Errors", Kind: "tune", TeamID: pay}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "tidy", Kind: "other", TeamID: chk}, "ana"); err != nil {
		t.Fatal(err)
	}
	o := s.cfg.Options
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "deploy", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), TeamID: chk}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "everyone's", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)}, "ana", o); err != nil {
		t.Fatal(err)
	}
	// The digest: to the webhook, as its own settings say, routes or not.
	if err := s.l.TellOnce(ctx, "digest-told", "1", "digest.ready", []ledger.Told{{Dest: "webhook", About: map[string]any{"text": "the week"}}}); err != nil {
		t.Fatal(err)
	}
	got := outbox(t, s)
	want := map[string][]string{
		// payments' task: route 1 (goes on), then route 2.
		"route:payments-hook": {"task.created"},
		"route:team:payments": {"task.created"},
		"route:ops":           {"task.created", "task.created"},
		"route:checkout-hook": {"window.kept"},
		"slack":               {"window.kept"},
		"webhook":             {"digest.ready"},
	}
	if len(got) != len(want) {
		t.Errorf("places told: %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	for d, w := range want {
		if e := events(got[d]); !slices.Equal(e, w) {
			t.Errorf("%s: %v, want %v", d, e, w)
		}
	}
	if n := s.routing.unrouted.Load(); n != 0 {
		t.Errorf("%d events went where no route sent them, with a default", n)
	}
}

// Without routes, with --config and every kind of destination given,
// what happens is told as before routes, byte for byte: to the webhook and
// Slack alone, the same rows; the named destinations get nothing.
func TestNoRoutesAsBefore(t *testing.T) {
	ctx := context.Background()
	write := func(s *Server) map[string][]ledger.Notice {
		if _, err := s.l.PutTask(ctx, ledger.Task{Title: "tune Errors", Kind: "tune", Rule: "Errors"}, "ana"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "deploy", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)}, "ana", s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		return outbox(t, s)
	}
	before, _ := server(t, func(c *Config) {
		hook := newSignedHook(t)
		c.NotifyWebhook, c.Slack, c.NotifySlack = hook.file, hook.file, true
	})
	routed, _, _ := routedServer(t, "teams: []\n", nil)
	b, r := write(before), write(routed)
	if !slices.Equal(slices.Sorted(maps.Keys(b)), []string{"slack", "webhook"}) || !slices.Equal(slices.Sorted(maps.Keys(r)), []string{"slack", "webhook"}) {
		t.Fatalf("places: %v and %v", slices.Sorted(maps.Keys(b)), slices.Sorted(maps.Keys(r)))
	}
	for _, d := range []string{"webhook", "slack"} {
		if len(b[d]) != 2 || len(r[d]) != 2 {
			t.Fatalf("%s: %d and %d", d, len(b[d]), len(r[d]))
		}
		for i := range b[d] {
			if x, y := withoutAt(t, b[d][i].Body), withoutAt(t, r[d][i].Body); x != y || b[d][i].Event != r[d][i].Event || b[d][i].ID != r[d][i].ID {
				t.Errorf("%s %d: %s\nnot %s", d, i, y, x)
			}
		}
	}
	// notify: with templates alone routes nothing either.
	tmpl, _, _ := routedServer(t, "notify:\n  templates:\n    ops: \"{{.Line}}\"\n", nil)
	if got := slices.Sorted(maps.Keys(write(tmpl))); !slices.Equal(got, []string{"slack", "webhook"}) {
		t.Errorf("templates alone: %v", got)
	}
}

// withoutAt is a notice's body with its time, which the clock gives, left
// out.
func withoutAt(t *testing.T, b []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "at")
	out, _ := json.Marshal(m)
	return string(out)
}

// A route's labels pick an event by its rule's labels, a marked window's
// or an incident's, exact values; teams by a rule's owner, an incident's
// labels, or a goal's teams; kinds by name or family. Labels choose among
// the routes alone: one whose value is a destination's name, or a URL,
// sends nothing there.
func TestRouteMatch(t *testing.T) {
	o := &owners{byID: map[int64]ledger.Team{1: {ID: 1, Name: "payments", Labels: map[string][]string{"service": {"pay"}}}},
		teams: []ledger.Team{{ID: 1, Name: "payments", Labels: map[string][]string{"service": {"pay"}}}},
		rule:  map[string]int64{"Errors": 1}, ruleLabels: map[string]map[string]string{"Errors": {"severity": "page", "to": "checkout-hook", "url": "https://attacker.example/x"}}}
	c, err := notifyIn([]byte(`notify:
  routes:
    - labels: {severity: page}
      teams: [Payments]
      kinds: [proposal.*]
      to: [payments-hook]
    - labels: {severity: [critical, page]}
      to: [ops]
    - kinds: [goal.missed]
      teams: [checkout]
      to: [checkout-hook]
  default: [slack]
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		kind, about string
		want        []string
	}{
		{"proposal.made", `{"proposal": {"id": 1, "rule": "Errors", "kind": "for"}}`, []string{"payments-hook"}},
		{"change.applied", `{"proposal": {"id": 1, "rule": "Errors", "kind": "for"}}`, []string{"ops"}},
		{"comment.written", `{"comment": {"on": "rule", "key": "Errors"}}`, []string{"ops"}},
		{"comment.written", `{"comment": {"on": "rule", "key": "Other"}}`, []string{"slack"}},
		{"replay.differs", `{"rule": "Errors"}`, []string{"ops"}},
		{"marked.kept", `{"marked": {"title": "launch", "labels": {"severity": "critical"}}}`, []string{"ops"}},
		{"incident.written", `{"incident": {"id": "INC-1", "labels": {"severity": ["page"], "service": ["pay"]}}}`, []string{"ops"}},
		{"goal.missed", `{"goal": {"name": "g", "team": "*", "teams": [{"team": "checkout"}]}}`, []string{"checkout-hook"}},
		{"goal.missed", `{"goal": {"name": "g", "team": "payments"}}`, []string{"slack"}},
		// A label naming a destination, or holding a URL, sends nothing there.
		{"task.created", `{"task": {"id": 1, "rule": "Errors", "title": "<!channel>", "to": "checkout-hook", "url": "https://attacker.example/x"}}`, []string{"ops"}},
		{"source.quiet", `{"source": {"name": "cloudwatch"}, "to": "checkout-hook"}`, []string{"slack"}},
	} {
		sub := subjectOf(x.kind, []byte(x.about), o)
		if got := c.match(sub); !slices.Equal(got, x.want) {
			t.Errorf("%s %s: %v, want %v (%+v)", x.kind, x.about, got, x.want, sub)
		}
	}
	if sub := subjectOf("incident.written", []byte(`{"incident": {"id": "INC-1", "labels": {"service": ["pay"]}}}`), o); !slices.Equal(sub.teams, []string{"payments"}) {
		t.Errorf("an incident's teams by its labels: %v", sub.teams)
	}
}

// The no-drop default: routes without a default are refused, as the file
// is, and the routing in force stays. With routes in force, an event their
// destinations can't take goes to the default, or else everywhere, never
// nowhere, and is counted.
func TestRoutesNeverDrop(t *testing.T) {
	_, err := ParseConfig([]byte("notify:\n  routes:\n    - {kinds: [task.created], to: [ops]}\n"), time.UTC)
	var ce configError
	if !errors.As(err, &ce) || ce.format != "routes need a default: where what no route takes goes, so that nothing is dropped" || ce.line != 2 {
		t.Fatalf("routes without a default: %v", err)
	}
	s, _, _ := routedServer(t, routesConfig, nil)
	about := []byte(`{"task": {"id": 1, "title": "t"}}`)
	// The default's, where the routes' destinations aren't among the places.
	if got := s.route("task.created", about, []string{"webhook", "slack"}); !slices.Equal(got, []string{"slack"}) {
		t.Errorf("to the default: %v", got)
	}
	if got := s.route("task.created", about, []string{"webhook"}); !slices.Equal(got, []string{"webhook"}) {
		t.Errorf("everywhere, the default not among the places: %v", got)
	}
	if n := s.routing.unrouted.Load(); n != 2 {
		t.Errorf("unrouted %d, want 2", n)
	}
	// Routes declared, and none read since the start: as without routes,
	// counted.
	broken, _, _ := routedServer(t, "notify:\n  routes:\n    - {to: [nowhere]}\n  default: [slack]\n", nil)
	if broken.routing.table.Load() != nil || !broken.routing.broken.Load() {
		t.Fatal("a notify: naming no destination there is was put in force")
	}
	if got := broken.route("task.created", about, broken.dests()); !slices.Equal(got, []string{"webhook", "slack"}) {
		t.Errorf("broken: %v", got)
	}
	if got := broken.route("task.created", about, []string{"route:ops"}); !slices.Equal(got, []string{"route:ops"}) {
		t.Errorf("broken, with no webhook or Slack: %v, not everywhere", got)
	}
	if n := broken.routing.unrouted.Load(); n != 2 {
		t.Errorf("broken: unrouted %d, want 2", n)
	}
}

// A new file is in force once it is read the same twice in a row,
// configEvery apart; one that can't be used leaves the routing in force,
// and is said with its line; back to the one in force, nothing is said.
func TestRoutesReadTwice(t *testing.T) {
	s, _, file := routedServer(t, routesConfig, nil)
	first := s.routing.table.Load()
	if first == nil || len(first.conf.Routes) != 3 {
		t.Fatalf("at start: %+v", first)
	}
	at := time.Now()
	writeConfig(t, file, "notify:\n  default: [ops]\n")
	s.loadRoutes(at, false)
	if s.routing.table.Load() != first {
		t.Fatal("a file read once is in force")
	}
	s.loadRoutes(at.Add(configEvery/2), false)
	if s.routing.table.Load() != first {
		t.Fatal("a file read twice, sooner than configEvery apart, is in force")
	}
	s.loadRoutes(at.Add(configEvery), false)
	if rt := s.routing.table.Load(); rt == first || !slices.Equal(rt.conf.Default, []string{"ops"}) {
		t.Fatalf("read twice: %+v", rt.conf)
	}
	in := s.routing.table.Load()
	writeConfig(t, file, "notify:\n  routes:\n    - kinds: [task.nope]\n      to: [ops]\n  default: [ops]\n")
	s.loadRoutes(at.Add(2*configEvery), false)
	s.loadRoutes(at.Add(4*configEvery), false)
	if s.routing.table.Load() != in || s.routing.err == nil {
		t.Fatal("a file that can't be used was put in force, or not said")
	}
	if got := configText("en", s.routing.err); !strings.HasPrefix(got, `line 3: unknown kind "task.nope"`) {
		t.Errorf("said: %s", got)
	}
}

// What notify: can be told is wrong with, each with its line, in each
// language: the file is refused as a whole.
func TestNotifyConfigRefused(t *testing.T) {
	for _, x := range []struct{ file, want string }{
		{"notify:\n", "line 1: notify has nothing after it"},
		{"notify: [a]\n", "line 1: want notify as a mapping of routes, default, templates"},
		{"notify:\n  rutes: []\n", `line 2: unknown key "rutes" in notify: want routes, default, templates`},
		{"notify:\n  routes:\n    - {kinds: [task.created], go: [ops]}\n  default: [ops]\n", `line 3: unknown key "go" in a route`},
		{"notify:\n  routes:\n    - {kinds: [task.created]}\n  default: [ops]\n", "line 3: a route names where it sends, under to"},
		{"notify:\n  routes:\n    - {kinds: [tasks.*], to: [ops]}\n  default: [ops]\n", `line 3: unknown kind "tasks.*"`},
		{"notify:\n  routes:\n    - {kinds: ['*'], to: [ops]}\n  default: [ops]\n", `line 3: unknown kind "*"`},
		{"notify:\n  routes:\n    - {to: ['https://hooks.example.com/x']}\n  default: [ops]\n", `line 3: "https://hooks.example.com/x" isn't a destination's name`},
		{"notify:\n  routes:\n    - {to: [ops], continue: maybe}\n  default: [ops]\n", "line 3: continue is true or false"},
		{"notify:\n  routes:\n    - {to: [ops], labels: {severity: }}\n  default: [ops]\n", "line 3: label severity has no value"},
		{"notify:\n  default: [ops]\n  templates:\n    ops: \"{{.Rule}} {{.Annotations}}\"\n", "line 4: the template for ops takes {{.Field}} alone, a field one of Event, Line, Link, Rule, Team, Title, ID, By, Labels, At: not {{.Annotations}}"},
		{"notify:\n  default: [ops]\n  templates:\n    ops: \"{{printf \\\"%s\\\" .Rule}}\"\n", "line 4: the template for ops takes {{.Field}} alone"},
		{"notify:\n  default: [ops]\n  templates:\n    ops: \"{{.Rule\"\n", "line 4: the template for ops opens {{ and doesn't close it"},
		{"notify:\n  default: [ops]\n  templates:\n    ops: \"" + strings.Repeat("x", maxTemplate+1) + "\"\n", "line 4: the template for ops is more than 2000 bytes"},
		{"notify:\n  default: [ops]\n  templates:\n    ops: [a]\n", "line 4: the template for ops is a text"},
	} {
		_, err := ParseConfig([]byte(x.file), time.UTC)
		if err == nil {
			t.Errorf("%q: taken", x.file)
			continue
		}
		if got := configText("en", err); !strings.HasPrefix(got, x.want) {
			t.Errorf("%q: %s, want %s", x.file, got, x.want)
		}
		for lang := range catalogs {
			if got := configText(lang, err); got == configText("en", err) {
				t.Errorf("%s: %q isn't translated", lang, got)
			}
		}
	}
	// What the flags give is checked apart, by each replica, at the line.
	s, _, _ := routedServer(t, "teams: []\n", nil)
	c, err := notifyIn([]byte("notify:\n  routes:\n    - to: [ops, team:checkout]\n  default: [webhook]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := configText("en", s.checkNotify(c)); got != `line 3: there is no destination called "team:checkout": --notify-webhook-file names webhook, --slack-webhook-file slack, --team-digest team:NAME, and --webhook-dest and --report-dest the rest` {
		t.Errorf("checked: %s", got)
	}
	for _, f := range notifyFormats {
		for lang, cat := range catalogs {
			if _, ok := cat[f]; !ok {
				t.Errorf("%s has no translation for %q", lang, f)
			}
		}
	}
}

// Every kind the outbox is told of is one routes can name, or one that
// goes its own way: a kind added is a line in routeKinds.
func TestRouteKindsKnown(t *testing.T) {
	literal := regexp.MustCompile(`"([a-z]+\.[a-z_]+)"`)
	told := regexp.MustCompile(`tell\(|TellOnce\(|routeTold\(|var events = map|StateFailed: "|event, value = "`)
	var found []string
	for _, dir := range []string{"../ledger", "."} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(b), "\n") {
				if !told.MatchString(line) {
					continue
				}
				for _, m := range literal.FindAllStringSubmatch(line, -1) {
					if strings.Contains(m[1], "-") || strings.HasSuffix(m[1], ".go") {
						continue
					}
					found = append(found, m[1])
				}
			}
		}
	}
	slices.Sort(found)
	found = slices.Compact(found)
	if len(found) < 15 {
		t.Fatalf("found %d kinds told: %v", len(found), found)
	}
	for _, k := range found {
		if !slices.Contains(routeKinds, k) && !slices.Contains(ownWay, k) {
			t.Errorf("%s is told, and isn't in routeKinds: add it there, a line, for routes to name it", k)
		}
	}
	for _, k := range slices.Concat(routeKinds, ownWay) {
		if !slices.Contains(found, k) {
			t.Errorf("%s is in routeKinds or ownWay, and nothing tells it", k)
		}
	}
}
