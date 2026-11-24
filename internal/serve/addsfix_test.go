package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/scenario/scenariotest"
)

// Adding a rule, the guards a review of 0.65.0 found low: two Grafana
// folders of one title, the rules read once a version, a scenario met as
// it was approved and not known since, a group's labels changed since, a
// team's editor's form, a cloud's for, a name taken by an add applied and
// not yet read back. Each fails with its guard taken out.

// grafanaFolders is the Grafana stand-in with its rules in folders of
// their own, nested folders' titles alike: each rule's folder's uid and
// group (in), Platform's api for one it doesn't name, each folder's title
// (titles); a rule made goes where its folderUID says.
type grafanaFolders struct {
	*grafanaStandIn
	in     map[string][2]string // uid to folder uid and group
	titles map[string]string    // folder uid to title
}

func (g *grafanaFolders) of(uid string) [2]string {
	if x, ok := g.in[uid]; ok {
		return x
	}
	return [2]string{"f1", "api"}
}

// export is as Grafana gives it: a group of each folder apart, the folder
// by its title alone; a rule's own export names no folder.
func (g *grafanaFolders) export(only string) []byte {
	groups := map[[2]string][]any{}
	for _, uid := range slices.Sorted(maps.Keys(g.rules)) {
		if only == "" || only == uid {
			groups[g.of(uid)] = append(groups[g.of(uid)], escaped(g.rules[uid]))
		}
	}
	var out []any
	for _, k := range slices.SortedFunc(maps.Keys(groups), func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) }) {
		title := g.titles[k[0]]
		if only != "" {
			title = ""
		}
		out = append(out, map[string]any{"orgId": 1, "name": k[1], "folder": title, "interval": "1m", "rules": groups[k]})
	}
	b, _ := json.Marshal(map[string]any{"apiVersion": 1, "groups": out})
	return b
}

func (g *grafanaFolders) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/provisioning/alert-rules")
	switch {
	case r.Method == http.MethodGet && rest == "/export":
		w.Write(g.export(""))
	case r.Method == http.MethodGet && strings.HasSuffix(rest, "/export"):
		uid := strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/export")
		if _, ok := g.rules[uid]; !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(g.export(uid))
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/"):
		rec := httptest.NewRecorder()
		g.grafanaStandIn.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return
		}
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		f := g.of(strings.TrimPrefix(rest, "/"))
		out["folderUID"], out["ruleGroup"] = f[0], f[1]
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		json.Unmarshal(body, &in)
		uid, _ := in["uid"].(string)
		folder, _ := in["folderUID"].(string)
		group, _ := in["ruleGroup"].(string)
		g.in[uid] = [2]string{folder, group}
		r.Body = io.NopCloser(bytes.NewReader(body))
		g.grafanaStandIn.ServeHTTP(w, r)
	default:
		g.grafanaStandIn.ServeHTTP(w, r)
	}
}

// pagerRule is a Grafana rule of the stand-in's, telling a contact point.
func pagerRule(uid, title, pager string) map[string]any {
	return map[string]any{"uid": uid, "title": title, "condition": "A", "for": "2m", "labels": map[string]any{"severity": "page"},
		"notification_settings": map[string]any{"receiver": pager},
		"data":                  []any{map[string]any{"refId": "A", "datasourceUid": "prom", "model": map[string]any{"expr": title + " > 1"}}}}
}

// alertsFolders is Platform's api, and Payments/Alerts's group critical,
// telling payments-pager; with security, Security/Alerts's too, telling
// security-pager.
func alertsFolders(t *testing.T, security bool) (*grafanaFolders, *Server) {
	t.Helper()
	g := &grafanaFolders{grafanaStandIn: &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}},
		in: map[string][2]string{"pay": {"fp", "critical"}}, titles: map[string]string{"f1": "Platform", "fp": "Alerts", "fs": "Alerts"}}
	g.rules["pay"] = pagerRule("pay", "PayDown", "payments-pager")
	if security {
		g.securityToo()
	}
	ts := httptest.NewServer(g)
	t.Cleanup(ts.Close)
	s, _ := changesServer(t, func(c *Config) {
		c.RulesPath, c.Grafana = "", ts.URL
		c.ChangesGrafanaToken = tokenFile(t, "grafana-writes-0123456789")
	})
	return g, s
}

func (g *grafanaFolders) securityToo() {
	g.rules["sec"] = pagerRule("sec", "SecDown", "security-pager")
	g.in["sec"] = [2]string{"fs", "critical"}
}

// Two Grafana folders of one title (nested, Payments/Alerts and
// Security/Alerts, each with a group critical) are no place: a place is
// named by its folder's title, and their rules read alike, so it would
// offer both folders' contact points and add the rule to either. A
// folder of a title its own is offered. One proposed while its title was
// its folder's alone, another folder of that title since, is refused as
// it is carried out, nothing written.
func TestAddGrafanaFolderTitleTwice(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	_, s := alertsFolders(t, true)
	for _, o := range mustPlaces(t, s) {
		if o.Folder == "Alerts" {
			t.Errorf("a folder's title two folders have, offered: %+v", o)
		}
	}
	placeKeyOf(t, s, addPlace{Path: ledger.PathGrafana, Folder: "Platform", Group: "api"})

	g, s := alertsFolders(t, false)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGrafana, Folder: "Alerts", Group: "critical"})
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(60)}, "tells": {"payments-pager"}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing in Payments/Alerts: %d %s", code, zzMsg(body))
	}
	approve(t, h, id, ana)
	g.securityToo()
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "folder's title") || len(g.writes) != 0 {
		t.Errorf("another folder of its title since: %s %q, wrote %v", p.State, p.Reason, g.writes)
	}
}

// The rules read are read once a version for a rule added: the new rule's
// form, MCP's places and proposals refused before a try is taken (a place
// not offered, a name in effect) read no version again, however often
// asked; a new version is read once.
func TestAddReadsAVersionOnce(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h := s.Handler()
	refused := func() {
		t.Helper()
		if _, code, _ := proposeAdding(t, h, bo, url.Values{"place": {"0123456789abcdef"}, "rule": {addedRule}, "expectations": {heldTo(60)}}); code != http.StatusBadRequest {
			t.Errorf("a place no offer is: %d", code)
		}
		taken := strings.Replace(addedRule, "ErrorsLow", "Errors", 1)
		key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
		if _, code, _ := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {taken}, "expectations": {strings.Replace(heldTo(60), "ErrorsLow", "Errors", 1)}}); code != http.StatusBadRequest {
			t.Errorf("a name in effect: %d", code)
		}
		if code, _, _ := ui(t, h, http.MethodGet, "/proposals/new", bo, "", nil); code != 200 {
			t.Errorf("the form: %d", code)
		}
	}
	refused() // the version read once, and whatever else the pages read
	before := namedReads.Load()
	for range 5 {
		refused()
		mustPlaces(t, s)
	}
	if n := namedReads.Load() - before; n != 0 {
		t.Errorf("asked again and again, the version read %d times more", n)
	}
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(strings.Replace(ruleFile, "for: 2m", "for: 3m", 1)), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	before = namedReads.Load()
	for range 3 {
		mustPlaces(t, s)
		if _, err := s.nameInEffect(ctx, "Errors"); err != nil {
			t.Fatal(err)
		}
	}
	if n := namedReads.Load() - before; n != 1 {
		t.Errorf("a new version read %d times", n)
	}
}

// A scenario a rule added passed as it was approved, not known as it is
// carried out (promtool gone), holds it as a broken one does: refused,
// nothing handed out. What was met is what its approver approved.
func TestAddCarriedOutNotKnown(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	link := scenariotest.Install(t)
	s, _ := changesServer(t, func(c *Config) { c.Promtool = link })
	h, port := s.Handler(), s.IncidentsHandler()
	expect := "expectations:\n  - rule: ErrorsLow\n    budget:\n      week: 60\n    scenarios: [tests/low.yaml]\n"
	id, err := s.proposeAdd(ctx, addAsk{Rule: addedRule, Place: addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"}, Expect: expect,
		Tests: map[string]string{"tests/low.yaml": scenariotest.Test("pass")}, From: "person"}, bo, false)
	if err != nil {
		t.Fatal(err)
	}
	ev := proposalEvidence(state(t, s, id))
	if ev.Add == nil || len(ev.Add.Items) != 2 || ev.Add.Items[0].State != "met" || ev.Add.Items[1].State != "met" {
		t.Fatalf("as proposed: %+v", ev.Add)
	}
	if code, body := approve(t, h, id, ana); code != http.StatusSeeOther {
		t.Fatalf("approving: %d %s", code, body)
	}
	s.cfg.Promtool = ""
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "is not known now (scenario tests/low.yaml)") || len(handed(t, port)) != 0 {
		t.Errorf("a scenario not known since: %s %q", p.State, p.Reason)
	}
}

// A rule added whose group's labels make it another team's between its
// approval and its carrying out is refused as it is carried out, as one
// approved by the team that owned it then: payments's approver approved
// it, and it is checkout's now.
func TestTeamAddMovedAsItIsCarriedOut(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, dir := boundsServer(t, teamsConfig, nil)
	id, code, body := proposeAddAs(t, s, bo, "pay.yaml", "pay", "PayErrors", "")
	if id == 0 {
		t.Fatalf("proposing: %d %s", code, zzMsg(body))
	}
	if code, body := approve(t, s.Handler(), id, quinn); code != http.StatusSeeOther {
		t.Fatalf("quinn approving payments's: %d %s", code, zzMsg(body))
	}
	os.WriteFile(filepath.Join(dir, "pay.yaml"), []byte(strings.Replace(payRules, "{team: payments}", "{team: checkout}", 1)), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || p.Reason != teamMovedText("payments", "checkout") {
		t.Errorf("carried out as checkout's: %s %q", p.State, p.Reason)
	}
}

// A team's editor, who may post a rule added of their team's, is shown its
// form, and the link to it; a viewer isn't.
func TestTeamEditorSeesNewRuleForm(t *testing.T) {
	parallel(t)
	s, _ := boundsServer(t, teamsConfig, nil)
	h := s.Handler()
	if code, body, _ := ui(t, h, http.MethodGet, "/proposals/new", pat, "", nil); code != 200 || !strings.Contains(body, `name="place"`) {
		t.Errorf("pat's form: %d", code)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals", pat, "", nil); !strings.Contains(body, `href="/proposals/new`) {
		t.Error("pat's proposals page doesn't link the form")
	}
	if code, _, _ := ui(t, h, http.MethodGet, "/proposals/new", "vi@example.com", "", nil); code != http.StatusForbidden {
		t.Errorf("a viewer's form: %d", code)
	}
}

// A cloud's alarm added whose for the cloud doesn't take is refused as it
// is proposed, not by the cloud once approved: CloudWatch's PendingPeriod
// and RecoveryPeriod are whole seconds, a day at most (a for of 90s, no
// multiple of a minute, is taken); a Google policy's duration is whole
// seconds.
func TestAddCloudFor(t *testing.T) {
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathAlarm, Alarms: "cw"})
	held := strings.Replace(heldTo(60), "ErrorsLow", "checkout-low", 1)
	for _, d := range []string{"for: 25h\n", "keep_firing_for: 25h\n", "for: 1500ms\n"} {
		if _, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {"alert: checkout-low\nexpr: errors > 1\n" + d}, "expectations": {held}}); code != http.StatusBadRequest ||
			!strings.Contains(body, "whole seconds, 24h at most") {
			t.Errorf("CloudWatch, %q: %d %s", d, code, zzMsg(body))
		}
	}
	if _, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {"alert: checkout-low\nexpr: errors > 1\nfor: 90s\n"}, "expectations": {held}}); code != http.StatusSeeOther {
		t.Errorf("CloudWatch, for 90s: %d %s", code, zzMsg(body))
	}
	g, _ := googlePolicies()
	cloudErrs(t, &g.Common)
	g.Add(map[string]any{"displayName": "Told", "enabled": true, "combiner": "OR", "notificationChannels": []any{"projects/shop/notificationChannels/7"},
		"conditions": []any{map[string]any{"displayName": "Told", "conditionPrometheusQueryLanguage": map[string]any{"query": "told > 1", "duration": "60s"}}}})
	gs := alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", g, []string{"gcp:*"}, nil)
	if _, code, body := proposeAdding(t, gs.Handler(), bo, url.Values{"place": {placeKeyOf(t, gs, addPlace{Path: ledger.PathAlarm, Alarms: "gcp"})},
		"rule": {"alert: ErrorsLow\nexpr: errors > 1\nfor: 1500ms\n"}, "expectations": {heldTo(60)}}); code != http.StatusBadRequest || !strings.Contains(body, "is whole seconds") {
		t.Errorf("Google, for 1500ms: %d %s", code, zzMsg(body))
	}
}

// A rule added and applied through a ruler's API is in effect before the
// rules are read again: another of its name, elsewhere, is refused
// meanwhile, as one in effect is. Once a version read after it holds it no
// more (deleted by hand), its name is free.
func TestAddNameTakenUntilReadBack(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, nil)
	h := s.Handler()
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, addPlace{Path: ledger.PathRuler, Namespace: "shop", Group: "api"})},
		"rule": {addedRule}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d %s", code, zzMsg(body))
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("applied: %s %q", p.State, p.Reason)
	}
	solo := placeKeyOf(t, s, addPlace{Path: ledger.PathRuler, Namespace: "solo", Group: "solo"})
	if _, code, body := proposeAdding(t, h, bo, url.Values{"place": {solo}, "rule": {addedRule}, "expectations": {heldTo(60)}}); code != http.StatusBadRequest ||
		!strings.Contains(body, "a rule in effect is called that") {
		t.Errorf("its name elsewhere, before the rules are read again: %d %s", code, zzMsg(body))
	}
	// Read back, then deleted by hand and read again: free.
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if taken, err := s.nameInEffect(ctx, "ErrorsLow"); err != nil || !taken {
		t.Fatalf("read back: %v %v", taken, err)
	}
	if err := m.put("shop", []byte(mimirShop)); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(time.Minute) }
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if taken, err := s.nameInEffect(ctx, "ErrorsLow"); err != nil || taken {
		t.Errorf("gone from a version read after it: %v %v", taken, err)
	}
}
