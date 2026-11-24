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
// destination a route may name, started as Run starts it.
func routedServer(t *testing.T, config string, change func(*Config)) (*Server, routedHooks, string) {
	t.Helper()
	s, hs, file := unstartedRoutedServer(t, config, change)
	s.startRoutes(context.Background()) // as Run does
	return s, hs, file
}

// unstartedRoutedServer is routedServer before Run reads its routes.
func unstartedRoutedServer(t *testing.T, config string, change func(*Config)) (*Server, routedHooks, string) {
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
	parallel(t)
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
	parallel(t)
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
	parallel(t)
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
    - kinds: [goal.missed, expectation.*]
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
		// An expectation's by its rule's labels, and the team it was told
		// with; a draft's by its rule's.
		{"expectation.broken", `{"expectation": {"rule": "Errors", "team": "payments", "what": "budget week"}}`, []string{"ops"}},
		{"expectation.broken", `{"expectation": {"rule": "Other", "team": "checkout", "what": "budget week"}}`, []string{"checkout-hook"}},
		{"expectation.broken", `{"expectation": {"rule": "Other", "team": "payments", "what": "budget week"}}`, []string{"slack"}},
		{"expectation.drafted", `{"draft": {"id": 3, "rule": "Errors"}}`, []string{"ops"}},
		{"expectation.accepted", `{"draft": {"id": 3, "rule": "Other"}}`, []string{"slack"}},
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

// A route names a team as it is, or in another letter case where one team
// alone has the name. Of two teams a ledger kept from before whose names
// differ only in letter case (the Teams page lists them apart), a route
// naming one takes that one's events and never the other's; one naming
// neither as written takes neither's, and they go to the default.
func TestRouteTeamsAlike(t *testing.T) {
	parallel(t)
	teams := []ledger.Team{{ID: 1, Name: "Payments"}, {ID: 2, Name: "payments"}, {ID: 3, Name: "Checkout"}}
	o := &owners{byID: map[int64]ledger.Team{1: teams[0], 2: teams[1], 3: teams[2]}, teams: teams,
		rule: map[string]int64{"Errors": 1, "Latency": 2, "Carts": 3}}
	c, err := notifyIn([]byte(`notify:
  routes:
    - teams: [payments]
      to: [small-hook]
    - teams: [PAYMENTS]
      to: [neither-hook]
    - teams: [checkout]
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
		// The team named as it is.
		{"proposal.made", `{"proposal": {"id": 1, "rule": "Latency", "kind": "for"}}`, []string{"small-hook"}},
		{"goal.missed", `{"goal": {"name": "g", "team": "payments"}}`, []string{"small-hook"}},
		{"task.created", `{"task": {"id": 1, "team_id": 2, "title": "t"}}`, []string{"small-hook"}},
		// The other, whose name differs only in letter case: no route's.
		{"proposal.made", `{"proposal": {"id": 2, "rule": "Errors", "kind": "for"}}`, []string{"slack"}},
		{"goal.missed", `{"goal": {"name": "g", "team": "Payments"}}`, []string{"slack"}},
		{"task.created", `{"task": {"id": 2, "team_id": 1, "title": "t"}}`, []string{"slack"}},
		// One team alone has the name: in any letter case, as before.
		{"proposal.made", `{"proposal": {"id": 3, "rule": "Carts", "kind": "for"}}`, []string{"checkout-hook"}},
		{"goal.missed", `{"goal": {"name": "g", "team": "Checkout"}}`, []string{"checkout-hook"}},
	} {
		sub := subjectOf(x.kind, []byte(x.about), o)
		if got := c.match(sub); !slices.Equal(got, x.want) {
			t.Errorf("%s %s: %v, want %v (%+v)", x.kind, x.about, got, x.want, sub)
		}
	}
}

// The no-drop default: routes without a default are refused, as the file
// is, and the routing in force stays. With routes in force, an event their
// destinations can't take goes to the default's, or else as without
// routes, to the webhook and Slack; with neither, nowhere, never to every
// place; and is counted.
func TestRoutesNeverDrop(t *testing.T) {
	parallel(t)
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
	// The default not among them either: as without routes.
	if got := s.route("task.created", about, []string{"webhook"}); !slices.Equal(got, []string{"webhook"}) {
		t.Errorf("as without routes, the default not among the places: %v", got)
	}
	// Neither: nowhere, not to every place a route may send to.
	if got := s.route("task.created", about, []string{"route:checkout-hook", "route:team:payments"}); len(got) != 0 {
		t.Errorf("with no webhook or Slack: %v, not nowhere", got)
	}
	if n := s.routing.unrouted.Load(); n != 3 {
		t.Errorf("unrouted %d, want 3", n)
	}
	// Routes declared, and none read since the start: as without routes,
	// counted; with no webhook or Slack, nowhere.
	broken, _, _ := routedServer(t, "notify:\n  routes:\n    - {kinds: [task.nope], to: [ops]}\n  default: [slack]\n", nil)
	if broken.routing.table.Load() != nil || !broken.routing.broken.Load() {
		t.Fatal("a notify: that can't be read was put in force")
	}
	if got := broken.route("task.created", about, broken.dests()); !slices.Equal(got, []string{"webhook", "slack"}) {
		t.Errorf("broken: %v", got)
	}
	if got := broken.route("task.created", about, []string{"route:ops", "route:team:payments"}); len(got) != 0 {
		t.Errorf("broken, with no webhook or Slack: %v, not nowhere", got)
	}
	if n := broken.routing.unrouted.Load(); n != 2 {
		t.Errorf("broken: unrouted %d, want 2", n)
	}
	// A destination the flags don't give is left out, the routes kept: a
	// route left with none takes what it takes, which goes to the
	// default's; the default left with none, as without routes.
	left, _, _ := routedServer(t, "notify:\n  routes:\n    - {kinds: [task.*], to: [nowhere]}\n    - {to: [ops]}\n  default: [slack, gone]\n", nil)
	if rt := left.routing.table.Load(); rt == nil || !rt.conf.declared() || len(rt.unknown) != 2 || left.routing.broken.Load() {
		t.Fatalf("routes naming a destination not given weren't kept: %+v", rt)
	}
	if got := left.route("task.created", about, left.dests()); !slices.Equal(got, []string{"slack"}) {
		t.Errorf("a route left with no destination: %v, want the default's", got)
	}
	if got := left.route("window.kept", []byte(`{"window": {"id": 1}}`), left.dests()); !slices.Equal(got, []string{"route:ops"}) {
		t.Errorf("the route after it: %v", got)
	}
	noSlack, _, _ := routedServer(t, "notify:\n  routes:\n    - {kinds: [task.*], to: [nowhere]}\n  default: [gone]\n", func(c *Config) {
		c.NotifySlack = false
	})
	if got := noSlack.route("task.created", about, noSlack.dests()); !slices.Equal(got, []string{"webhook"}) {
		t.Errorf("the default left with none: %v, want as without routes", got)
	}
	none, _, _ := routedServer(t, "notify:\n  routes:\n    - {kinds: [task.*], to: [nowhere]}\n  default: [gone]\n", func(c *Config) {
		c.NotifyWebhook, c.NotifySlack = "", false
	})
	if got := none.route("task.created", about, none.dests()); len(got) != 0 {
		t.Errorf("with nowhere to go: %v, want nowhere", got)
	}
	if left.routing.unrouted.Load() != 1 || noSlack.routing.unrouted.Load() != 1 || none.routing.unrouted.Load() != 1 {
		t.Errorf("unrouted: %d, %d, %d, want 1 each", left.routing.unrouted.Load(), noSlack.routing.unrouted.Load(), none.routing.unrouted.Load())
	}
}

// Routes are each replica's own, read from notify: alone: what is wrong
// elsewhere in the file, which the leader refuses as a whole, leaves them
// in force, and a team's event goes where its team's route sends it, not
// to another team's destination, nor to every place.
func TestRoutesPastAnotherError(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	const file = `teams: []
windows: nope
notify:
  routes:
    - kinds: [task.*]
      teams: [payments]
      to: [team:payments]
    - teams: [search]
      to: [team:search]
  default: [ops]
`
	if _, err := ParseConfig([]byte(file), time.UTC); err == nil {
		t.Fatal("the file is taken by the leader")
	}
	s, hs, _ := routedServer(t, file, func(c *Config) {
		// Neither the webhook nor Slack: nothing to fall back on.
		c.NotifyWebhook, c.Slack, c.NotifySlack = "", "", false
		c.TeamDigests["search"] = newSignedHook(t).file
	})
	if rt := s.routing.table.Load(); rt == nil || !rt.conf.declared() || s.routing.err != nil {
		t.Fatalf("routes not in force past the file's other error: %v", s.routing.err)
	}
	pay, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Pay"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "search", Rules: "^Search"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.owners(ctx); err != nil { // as watchRoutes keeps them
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "refunds slow", Kind: "tune", TeamID: pay}, "ana"); err != nil {
		t.Fatal(err)
	}
	got := outbox(t, s)
	if len(got) != 1 || !slices.Equal(events(got["route:team:payments"]), []string{"task.created"}) {
		t.Errorf("payments' task went to %v, want route:team:payments alone", slices.Sorted(maps.Keys(got)))
	}
	if n := s.routing.unrouted.Load(); n != 0 {
		t.Errorf("unrouted %d", n)
	}
	for _, h := range []*signedHook{hs.payments, hs.checkout, hs.ops} {
		if n := len(h.all()); n != 0 {
			t.Errorf("another destination was posted %d times", n)
		}
	}
}

// Without routes in force, what is told once asks whether to tell, and
// marks it told, as before routes: by the webhook and Slack alone. A team's
// digest or a --report-dest, which --config makes places a route may send
// to, isn't somewhere to tell it, and a task overdue isn't marked told with
// no notice written.
func TestToldToAsBeforeRoutes(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _, _ := routedServer(t, "teams: []\n", func(c *Config) {
		c.NotifyWebhook, c.Slack, c.NotifySlack = "", "", false
	})
	if len(s.dests()) == 0 {
		t.Fatal("no place a route may send to")
	}
	if got := s.toldTo(); len(got) != 0 {
		t.Errorf("told to %v without routes, webhook or Slack", got)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "tune Errors", Kind: "tune", Due: now.AddDate(0, 0, -2)}, "ana"); err != nil {
		t.Fatal(err)
	}
	s.tellDue(ctx)
	if n, err := s.l.Overdue(ctx, s.l.Midnight(s.l.Day(s.now()))); err != nil || n != 1 {
		t.Errorf("the overdue task was marked told with nowhere to tell it: %d still to tell, %v", n, err)
	}
	if got := outbox(t, s); len(got) != 0 {
		t.Errorf("written: %v", slices.Sorted(maps.Keys(got)))
	}
	// As before routes, with the webhook: there; with routes in force,
	// every place they may send to.
	hooked, _, _ := routedServer(t, "teams: []\n", nil)
	if got := hooked.toldTo(); !slices.Equal(got, []string{"webhook", "slack"}) {
		t.Errorf("told to %v, want the webhook and Slack", got)
	}
	routed, _, _ := routedServer(t, routesConfig, nil)
	if got := routed.toldTo(); !slices.Equal(got, routed.dests()) || !slices.Contains(got, "route:team:payments") {
		t.Errorf("with routes, told to %v", got)
	}
}

// Routes read as serve starts read the owners at once: a team's event in
// the first seconds, before the owners are kept every configEvery, goes
// where its team's route sends it.
func TestRoutesOwnersAtStart(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _, _ := unstartedRoutedServer(t, routesConfig, nil)
	pay, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Pay"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	s.startRoutes(ctx)
	if s.ownersNow() == nil {
		t.Fatal("no owners worked out as serve starts")
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "refunds slow", Kind: "tune", TeamID: pay}, "ana"); err != nil {
		t.Fatal(err)
	}
	got := outbox(t, s)
	for _, d := range []string{"route:payments-hook", "route:team:payments"} {
		if !slices.Equal(events(got[d]), []string{"task.created"}) {
			t.Errorf("%s: %v, as of no team", d, events(got[d]))
		}
	}
}

// What waits for a destination no flag gives any more, which no sender
// starts for, is tried by the leader's sweep: it fails, posting nothing,
// is tried again, and is given up on as any, and the pages count it and
// name it.
func TestGoneDestSwept(t *testing.T) {
	ctx := context.Background()
	s, hs, _ := routedServer(t, routesConfig, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{ana}, []string{"*"}
	})
	// Written while a --webhook-dest refunds-hook was given.
	s.l.RouteBy(nil)
	if err := s.l.TellOnce(ctx, "gone-test", "1", "task.created", []ledger.Told{{Dest: "route:refunds-hook", About: map[string]any{"task": map[string]any{"id": 1}}}}); err != nil {
		t.Fatal(err)
	}
	s.l.RouteBy(s.route)
	if gone, err := s.gonePlaces(ctx); err != nil || !slices.Equal(gone, []string{"route:refunds-hook"}) {
		t.Fatalf("gone: %v, %v", gone, err)
	}
	at := time.Now()
	for i := range ledger.GiveUpAfter {
		if err := s.sweepGone(ctx, at.Add(time.Duration(i)*7*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	ns, err := s.l.Due(ctx, "route:refunds-hook", at.Add(1000*time.Hour), 10)
	if err != nil || len(ns) != 0 {
		t.Fatalf("still waiting: %+v, %v", ns, err)
	}
	if got := s.status.notices["route:refunds-hook failed"]; got != ledger.GiveUpAfter {
		t.Errorf("tried %d times, want %d", got, ledger.GiveUpAfter)
	}
	for _, h := range []*signedHook{hs.webhook, hs.slack, hs.payments, hs.checkout, hs.ops, hs.teamPay} {
		if n := len(h.all()); n != 0 {
			t.Errorf("another destination was posted %d times", n)
		}
	}
	st, err := s.notifyStatus(ctx, "en")
	if err != nil {
		t.Fatal(err)
	}
	if st.GivenUp != 1 {
		t.Errorf("given up on, counted: %d", st.GivenUp)
	}
	var shown *destView
	for i, d := range st.Routes.Dests {
		if d.Name == "refunds-hook" {
			shown = &st.Routes.Dests[i]
		}
	}
	if shown == nil || !shown.Gone || shown.GivenUp != 1 || !strings.Contains(shown.LastErr, `no destination called "refunds-hook" any more`) {
		t.Errorf("the gone destination on the page: %+v", shown)
	}
	h := s.Handler()
	_, page := getAs(t, h, "/settings", ana)
	if !strings.Contains(page, "<code>refunds-hook</code><br>") || !strings.Contains(page, "no flag gives it any more") {
		t.Error("/settings doesn't name the gone destination")
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	pageA11y(t, h, ana, "/settings", "/manage")
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}

// A new file is in force once it is read the same twice in a row,
// configEvery apart; one that can't be used leaves the routing in force,
// and is said with its line; back to the one in force, nothing is said.
func TestRoutesReadTwice(t *testing.T) {
	parallel(t)
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
	parallel(t)
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
		{"notify:\n  default: [ops]\n  templates:\n    ops: \"{{.Line}}\"\n    ops: \"{{.Title}}\"\n", "line 5: ops is given twice, on line 4 too"},
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
		// Each replica reads notify: alone, as strictly.
		if _, err := notifyIn([]byte(x.file)); err == nil || !strings.HasPrefix(configText("en", err), x.want) {
			t.Errorf("%q, read alone: %v", x.file, err)
		}
	}
	for _, file := range []string{"", "~\n", "notify:\n  default: [ops]\nnotify:\n  default: [slack]\n"} {
		if c, err := notifyIn([]byte(file)); err == nil {
			t.Errorf("%q, read alone: taken, %+v", file, c)
		}
	}
	// What the flags give is checked apart, by each replica, at the line.
	s, _, _ := routedServer(t, "teams: []\n", nil)
	c, err := notifyIn([]byte("notify:\n  routes:\n    - to: [ops, team:checkout]\n  default: [webhook]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.unknownDests(c); len(got) != 1 || configText("en", got[0]) != `line 3: there is no destination called "team:checkout": --notify-webhook-file names webhook, --slack-webhook-file slack, --team-digest team:NAME, and --webhook-dest and --report-dest the rest` {
		t.Errorf("checked: %v", got)
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
	parallel(t)
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
