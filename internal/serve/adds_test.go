package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// Adding a rule, as a proposal held to its expectations before it is
// approved (docs/design/expectations.md, step 5): each test here is a
// behaviour, and the guards are what an attacker would try.

// addedRule is a rule to add: errors > 1 fires as Errors does, 21 times
// over the period the stand-in history holds.
const addedRule = "alert: ErrorsLow\nexpr: errors > 1\nfor: 2m\nlabels:\n  severity: ticket\nannotations:\n  summary: \"Errors on {{ $labels.service }}\"\n"

// heldTo is what it is held to: a budget of so many a week. Its 21
// firings over the three days replayed are 50 a week, scaled: 60 is met, 5
// broken.
func heldTo(week int) string {
	return "expectations:\n  - rule: ErrorsLow\n    budget:\n      week: " + itoa(int64(week)) + "\n"
}

// placeKeyOf is the key of a place the new rule's form offers.
func placeKeyOf(t *testing.T, s *Server, p addPlace) string {
	t.Helper()
	offers, err := s.addPlaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	probe := p
	if probe.NewGroup {
		probe.Group = ""
	}
	for _, o := range offers {
		if o.Key == placeKey(probe) {
			return o.Key
		}
	}
	t.Fatalf("not offered: %+v among %+v", p, offers)
	return ""
}

// proposeAdding proposes a rule to add from the new rule's form, as who.
func proposeAdding(t *testing.T, h http.Handler, who string, form url.Values) (int64, int, string) {
	t.Helper()
	form.Set("kind", "add")
	code, body, hd := ui(t, h, http.MethodPost, "/proposals", who, "same-origin", form)
	var id int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &id)
	return id, code, body
}

// The git path, all the way: proposed from the form, held to its
// expectations, approved, checked again and handed to a recipe, merged,
// read back as applied; its expectations then kept as a draft accepted by
// its approver; undone by the file put back as it was.
func TestAddByPullRequest(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	if code, body, _ := ui(t, h, http.MethodGet, "/proposals/new", bo, "", nil); code != 200 || !strings.Contains(body, `name="place"`) {
		t.Fatalf("the form: %d\n%s", code, body)
	}
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(60)}, "reason": {"A ticket for low errors."}})
	if code != http.StatusSeeOther || id == 0 {
		t.Fatalf("proposing: %d\n%s", code, body)
	}
	p := state(t, s, id)
	ev := proposalEvidence(p)
	if p.Kind != ledger.KindAdd || p.Path != ledger.PathGit || p.Rule != "ErrorsLow" || p.BeforeHash != "" || p.AfterHash == "" ||
		!strings.Contains(p.Document, "      - alert: \"ErrorsLow\"\n") || p.DocHash != rules.Hash([]byte(ruleFile)) {
		t.Fatalf("the proposal: %+v", p)
	}
	if ev.Add == nil || len(ev.Add.Items) != 1 || ev.Add.Items[0].State != "met" || ev.Add.Items[0].Value != "50" || addHeldWhy(ev) != "" ||
		ev.Backtest == nil || ev.Backtest.After.Firings != 21 || ev.Backtest.Before.Firings != 0 {
		t.Fatalf("held to: %+v %+v", ev.Add, ev.Backtest)
	}
	if code, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); code != 200 || !strings.Contains(body, `id="held"`) ||
		!strings.Contains(body, "budget week") {
		t.Fatalf("its page: %d\n%s", code, body)
	}
	if code, body := approve(t, h, id, ana); code != http.StatusSeeOther {
		t.Fatalf("approving: %d %s", code, body)
	}
	s.jobs(ctx, false)
	if p = state(t, s, id); p.State != ledger.StateReady || p.Backup != ruleFile {
		t.Fatalf("checked again and ready: %s %q", p.State, p.Reason)
	}
	hs := handed(t, port)
	if len(hs) != 1 || hs[0].Document != p.Document || hs[0].Kind != ledger.KindAdd || !strings.Contains(hs[0].Body, "a new rule") {
		t.Fatalf("handed out: %+v", hs)
	}
	for _, st := range []string{"opened", "merged"} {
		in := map[string]string{"state": st}
		if st == "opened" {
			in["link"] = "https://git.example/shop/pull/8"
		}
		if code, body := recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, in); code != 200 {
			t.Fatalf("%s: %d %s", st, code, body)
		}
	}
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(p.Document), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if p = state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("read back: %s %q", p.State, p.Reason)
	}
	// Its expectations, kept as a draft its approver accepted.
	s.jobs(ctx, false)
	d, err := s.l.AddedDraft(ctx, id)
	if err != nil || d.State != ledger.DraftAccepted || d.DecidedBy != ana || d.MadeBy != bo || d.Rule != "ErrorsLow" || !strings.Contains(d.Text, "week: 60") {
		t.Fatalf("its expectations: %+v %v", d, err)
	}
	// Undone: the file put back as it was.
	key = keyFor(t, h, id, ana, "undo")
	if key == "" {
		t.Fatal("no undo form")
	}
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	s.jobs(ctx, false)
	u := state(t, s, locID(hd))
	if u.State != ledger.StateReady || u.Document != ruleFile {
		t.Fatalf("the undo: %s %q", u.State, u.Reason)
	}
}

// Grafana: made through the provisioning API in the folder and group of a
// rule there, its uid Lapilli's, the data source and contact point among
// those the folder's rules use, read back by its uid; undone by deleting
// it. A contact point no rule there tells is refused, before anything is
// read again or replayed.
func TestAddInGrafana(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	g.rules["def"]["notification_settings"] = map[string]any{"receiver": "oncall"}
	s := grafanaServer(t, g)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGrafana, Folder: "Platform", Group: "api"})
	form := url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(60)}, "tells": {"someone-elses-pager"}}
	if _, code, _ := proposeAdding(t, h, bo, form); code != http.StatusBadRequest {
		t.Fatalf("a contact point no rule there tells: %d", code)
	}
	form.Set("tells", "oncall")
	id, code, body := proposeAdding(t, h, bo, form)
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d\n%s", code, body)
	}
	p := state(t, s, id)
	ev := proposalEvidence(p)
	uid := ev.Edit.UID
	if p.Path != ledger.PathGrafana || !strings.HasPrefix(uid, "lapilli-") || ev.Add.Items[0].State != "met" || len(g.writes) != 0 {
		t.Fatalf("the proposal: %+v %+v %v", p, ev.Add, g.writes)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	p = state(t, s, id)
	r := g.rules[uid]
	data, _ := r["data"].([]any)
	q, _ := data[0].(map[string]any)
	ns, _ := r["notification_settings"].(map[string]any)
	if p.State != ledger.StateApplied || p.AfterHash == "" || r["title"] != "ErrorsLow" || q["datasourceUid"] != "prom" || ns["receiver"] != "oncall" ||
		len(g.writes) != 1 || g.writes[0] != "POST "+uid {
		t.Fatalf("made: %s %q, Grafana has %v, wrote %v", p.State, p.Reason, r, g.writes)
	}
	// Undone: deleted, by its uid.
	key = keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	s.jobs(ctx, false)
	if u, p := state(t, s, locID(hd)), state(t, s, id); u.State != ledger.StateApplied || p.State != ledger.StateUndone || g.rules[uid] != nil ||
		g.writes[len(g.writes)-1] != "DELETE "+uid {
		t.Errorf("undone: %s %q, the add %s, wrote %v", u.State, u.Reason, p.State, g.writes)
	}
}

// A ruler (Mimir): a rule added to a group of a namespace --changes-in
// names, the group posted with it, only it added; a new group, made while
// it is still gone; each undone by deleting the rule, the group made going
// with it. A namespace --changes-in doesn't name isn't offered, and is
// refused as any place not offered is.
func TestAddInRuler(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, nil)
	h := s.Handler()
	if _, _, err := placeFor(mustPlaces(t, s), addPlace{Path: ledger.PathRuler, Namespace: "alloy/payments", Group: "api"}); !sameErr(err, errNoPlace) {
		t.Errorf("a namespace --changes-in doesn't name: %v", err)
	}
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, addPlace{Path: ledger.PathRuler, Namespace: "shop", Group: "api"})},
		"rule": {addedRule}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d\n%s", code, zzMsg(body))
	}
	if p := state(t, s, id); p.Path != ledger.PathRuler || !strings.Contains(p.Diff, "+        - alert: \"ErrorsLow\"") {
		t.Fatalf("the proposal: %s %s", p.Path, p.Diff)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || !strings.Contains(m.group("shop", "api"), "ErrorsLow") ||
		!strings.Contains(m.group("shop", "api"), "Other") || p.AfterHash == "" {
		t.Fatalf("added: %s %q\n%s", p.State, p.Reason, m.group("shop", "api"))
	}
	undoNow(t, s, id, 1)
	if p := state(t, s, id); p.State != ledger.StateUndone || strings.Contains(m.group("shop", "api"), "ErrorsLow") || !strings.Contains(m.group("shop", "api"), "Other") {
		t.Fatalf("undone: %s\n%s", p.State, m.group("shop", "api"))
	}
	// A new group, made for it.
	id, code, body = proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, addPlace{Path: ledger.PathRuler, Namespace: "shop", NewGroup: true})},
		"group": {"slow"}, "interval": {"30s"}, "rule": {addedRule}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing a new group: %d\n%s", code, zzMsg(body))
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || !strings.Contains(m.group("shop", "slow"), "ErrorsLow") || !strings.Contains(m.group("shop", "slow"), "interval: 30s") {
		t.Fatalf("a group made: %s %q\n%s", p.State, p.Reason, m.group("shop", "slow"))
	}
	undoNow(t, s, id, 1)
	if p := state(t, s, id); p.State != ledger.StateUndone || m.group("shop", "slow") != "" {
		t.Fatalf("a group made, undone: %s %q", p.State, m.group("shop", "slow"))
	}
}

func mustPlaces(t *testing.T, s *Server) []placeOffer {
	t.Helper()
	offers, err := s.addPlaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return offers
}

// sameErr reports whether err says what want does.
func sameErr(err, want error) bool { return err != nil && err.Error() == want.Error() }

// A cloud's alarm: CloudWatch's, made by PutMetricAlarm with the actions
// the alarms there already take (an SNS topic), never an automation one
// (scaling out), and only under a name --changes-in names there; read
// back; undone by deleting it. Google's, made by alertPolicies.create
// telling a channel the policies there tell, read back by the name the
// create answers with; undone by deleting that policy.
func TestAddCloudAlarm(t *testing.T) {
	ctx := context.Background()
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathAlarm, Alarms: "cw"})
	rule := strings.Replace(addedRule, "ErrorsLow", "checkout-low", 1)
	rule = strings.Replace(rule, "labels:\n  severity: ticket\nannotations:\n  summary: \"Errors on {{ $labels.service }}\"\n", "annotations:\n  description: Low errors at checkout.\n", 1)
	held := strings.Replace(heldTo(60), "ErrorsLow", "checkout-low", 1)
	sns := "arn:aws:sns:eu-west-1:111122223333:oncall"
	for _, bad := range []url.Values{
		// An action no alarm there tells: scaling out is automation.
		{"place": {key}, "rule": {rule}, "expectations": {held}, "tells": {"arn:aws:autoscaling:eu-west-1:111122223333:scalingPolicy:x:autoScalingGroupName/web:policyName/up"}},
		{"place": {key}, "rule": {rule}, "expectations": {held}, "tells": {"arn:aws:sns:eu-west-1:999999999999:theirs"}},
		// A name --changes-in doesn't name there.
		{"place": {key}, "rule": {strings.Replace(rule, "checkout-low", "ErrorsLow", 1)}, "expectations": {heldTo(60)}, "tells": {sns}},
	} {
		if _, code, body := proposeAdding(t, h, bo, bad); code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad["tells"], code, zzMsg(body))
		}
	}
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {rule}, "expectations": {held}, "tells": {sns}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d\n%s", code, zzMsg(body))
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	a := cw.Alarm("checkout-low")
	if p := state(t, s, id); p.State != ledger.StateApplied || a == nil || cwQuery(a)["Query"] != "errors > 1" || a["AlarmDescription"] != "Low errors at checkout." ||
		len(list(a["AlarmActions"])) != 1 || list(a["AlarmActions"])[0] != sns || cw.Took()[len(cw.Took())-1] != "PutMetricAlarm checkout-low" {
		t.Fatalf("made: %s %q %v %v", p.State, p.Reason, a, cw.Took())
	}
	undoNow(t, s, id, 1)
	if p := state(t, s, id); p.State != ledger.StateUndone || cw.Alarm("checkout-low") != nil {
		t.Fatalf("undone: %s, %v", p.State, cw.Took())
	}

	g, _ := googlePolicies()
	cloudErrs(t, &g.Common)
	g.Add(map[string]any{"displayName": "Told", "enabled": true, "combiner": "OR", "notificationChannels": []any{"projects/shop/notificationChannels/7"},
		"conditions": []any{map[string]any{"displayName": "Told", "conditionPrometheusQueryLanguage": map[string]any{"query": "told > 1", "duration": "60s"}}}})
	gs := alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", g, []string{"gcp:*"}, nil)
	gh := gs.Handler()
	id, code, body = proposeAdding(t, gh, bo, url.Values{"place": {placeKeyOf(t, gs, addPlace{Path: ledger.PathAlarm, Alarms: "gcp"})}, "rule": {addedRule},
		"expectations": {heldTo(60)}, "tells": {"projects/shop/notificationChannels/7"}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing in Google: %d\n%s", code, zzMsg(body))
	}
	approve(t, gh, id, ana)
	gs.jobs(ctx, false)
	p := state(t, gs, id)
	var made map[string]any
	json.Unmarshal([]byte(p.Document), &made)
	name, _ := made["name"].(string)
	pol := g.Policy(name)
	if p.State != ledger.StateApplied || pol == nil || pol["displayName"] != "ErrorsLow" || len(list(pol["notificationChannels"])) != 1 {
		t.Fatalf("made in Google: %s %q %q %v %v", p.State, p.Reason, name, pol, g.Took())
	}
	undoNow(t, gs, id, 1)
	if p := state(t, gs, id); p.State != ledger.StateUndone || g.Policy(name) != nil {
		t.Fatalf("undone in Google: %s %v", p.State, g.Took())
	}
}

// Where a rule may go: only a place serve offers, each refused for one
// reason, whether it exists or not. A file under --rules that isn't a rule
// file (a workflow), one outside it, a path that climbs out, a folder,
// namespace or alarms serve doesn't read: none is a place, through the
// form or MCP. Tells on a rule file are refused: it names no one.
func TestAddPlaceRefused(t *testing.T) {
	s, dir := changesServer(t, nil)
	os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte("on: push\njobs: {}\n"), 0o644)
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	offers := mustPlaces(t, s)
	for _, p := range []addPlace{
		{Path: ledger.PathGit, File: ".github/workflows/ci.yml", Group: "shop"},
		{Path: ledger.PathGit, File: "../shop.yaml", Group: "shop"},
		{Path: ledger.PathGit, File: "/etc/passwd", Group: "shop"},
		{Path: ledger.PathGit, File: "nope.yaml", Group: "shop"},
		{Path: ledger.PathGit, File: "shop.yaml", Group: "nope"},
		{Path: ledger.PathGrafana, Folder: "Platform", Group: "api"},
		{Path: ledger.PathRuler, Namespace: "shop", Group: "api"},
		{Path: ledger.PathAlarm, Alarms: "cw"},
		{Path: "elsewhere", File: "shop.yaml", Group: "shop"},
	} {
		if _, _, err := placeFor(offers, p); !sameErr(err, errNoPlace) {
			t.Errorf("%+v: %v", p, err)
		}
	}
	if _, _, err := placeFor(offers, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop", Tells: []string{"oncall"}}); !sameErr(err, errNoTell) {
		t.Errorf("a rule file's rule told someone: %v", err)
	}
	h := s.Handler()
	if _, code, _ := proposeAdding(t, h, bo, url.Values{"place": {"0123456789abcdef"}, "rule": {addedRule}, "expectations": {heldTo(60)}}); code != http.StatusBadRequest {
		t.Errorf("a place no offer is: %d", code)
	}
	ps, _ := s.l.Proposals(context.Background(), ledger.ProposalQuery{})
	if len(ps) != 0 {
		t.Errorf("kept: %d", len(ps))
	}
}

// What the rule is, read strictly and written by Lapilli: a rule named as
// one in effect is refused, anywhere; one given with a comment, a flow
// mapping and its keys in another order is written in Lapilli's own form,
// nothing of the text as given.
func TestAddRuleWrittenByLapilli(t *testing.T) {
	s, _ := changesServer(t, nil)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	taken := strings.Replace(addedRule, "ErrorsLow", "Errors", 1)
	if _, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {taken}, "expectations": {strings.Replace(heldTo(60), "ErrorsLow", "Errors", 1)}}); code != http.StatusBadRequest ||
		!strings.Contains(body, "a rule in effect is called that") {
		t.Errorf("a name in effect: %d %s", code, zzMsg(body))
	}
	given := "# injected: groups:\n{expr: 'errors > 1', alert: ErrorsLow, labels: {severity: ticket}}\n"
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {given}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d %s", code, zzMsg(body))
	}
	p := state(t, s, id)
	if strings.Contains(p.Document, "injected") || strings.Contains(p.Document, "{expr") ||
		!strings.Contains(p.Document, "      - alert: \"ErrorsLow\"\n        expr: \"errors > 1\"\n        labels:\n          severity: \"ticket\"\n") {
		t.Errorf("written as given:\n%s", p.Document)
	}
	for _, bad := range []string{"alert: X\nexpr: up\nlabels: {team: 7}\n", "alert: X\nexpr: up\nnotes: hi\n", "record: x\nexpr: up\n"} {
		if _, code, _ := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {bad}, "expectations": {heldTo(60)}}); code != http.StatusBadRequest {
			t.Errorf("%q: %d", bad, code)
		}
	}
}

// What it is held to: one expectation at least, of it alone; an add held
// to nothing, or to another rule, is refused, and the ledger refuses one
// carrying none.
func TestAddHeldToSome(t *testing.T) {
	s, _ := changesServer(t, nil)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	for _, x := range []string{"", "expectations: []\n", "expectations:\n  - rule: ErrorsLow\n", strings.Replace(heldTo(60), "ErrorsLow", "Errors", 1),
		"expectations:\n  - group: shop\n    budget:\n      week: 60\n"} {
		if _, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {x}}); code != http.StatusBadRequest {
			t.Errorf("%q: %d %s", x, code, zzMsg(body))
		}
	}
	if _, err := s.l.Propose(context.Background(), ledger.Proposal{Rule: "ErrorsLow", Kind: ledger.KindAdd, Path: ledger.PathGit, MadeBy: bo}); err == nil {
		t.Error("the ledger kept a rule added held to nothing")
	}
}

// Approval: a rule added that breaks what it is held to isn't approved:
// its page offers no form, a form forged for it is refused, and --approvals
// lowered doesn't approve it. Backtested again it stays so. Its proposer
// doesn't approve it; nor does the person an agent proposed it for, whose
// hourly proposals it counts toward.
func TestAddApproval(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, func(c *Config) { c.MCPPropose = "editors" })
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(5)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d %s", code, zzMsg(body))
	}
	p := state(t, s, id)
	if addHeldWhy(proposalEvidence(p)) != addHeldBroken {
		t.Fatalf("not held: %+v", proposalEvidence(p).Add)
	}
	if keyFor(t, h, id, ana, "approve") != "" {
		t.Error("a form offered for a rule added that breaks what it is held to")
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); !strings.Contains(body, "No one approves it yet") {
		t.Error("its page doesn't say why it waits")
	}
	forged := s.formKey(p, ana, "approve")
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/approve", ana, "same-origin", url.Values{"key": {forged}, "read": {"1"}}); code != http.StatusBadRequest {
		t.Errorf("a forged form: %d", code)
	}
	// Approved by one before two were asked, then --approvals lowered: held.
	s.l.Approve(ctx, id, cy, 2, time.Time{})
	if _, err := s.l.Promote(ctx, 1, mustHeld(t, s)); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, id); p.State != ledger.StateProposed {
		t.Errorf("approved as fewer approvals are needed: %s", p.State)
	}
	// Its proposer doesn't approve one that meets them.
	id2, _, _ := proposeAdding(t, h, ana, url.Values{"place": {key}, "rule": {strings.Replace(addedRule, "ErrorsLow", "ErrorsLower", 1)},
		"expectations": {strings.Replace(heldTo(60), "ErrorsLow", "ErrorsLower", 1)}})
	if id2 == 0 || keyFor(t, h, id2, ana, "approve") != "" || keyFor(t, h, id2, cy, "approve") == "" {
		t.Errorf("its proposer's form, or another approver's missing: %d", id2)
	}
	// An agent's, for the person it acts for, who doesn't approve it; it
	// counts toward their hour.
	ts := httptest.NewServer(h)
	defer ts.Close()
	cs := connect(t, ts.URL, cy)
	var out proposeOut
	if failed, text := call(t, cs, "propose", map[string]any{"kind": "add", "rule": "ErrorsAgent", "added": strings.Replace(addedRule, "ErrorsLow", "ErrorsAgent", 1),
		"path": "git", "file": "shop.yaml", "group": "shop", "expectations": strings.Replace(heldTo(60), "ErrorsLow", "ErrorsAgent", 1)}, &out); failed {
		t.Fatalf("an agent's: %s", text)
	}
	if p := state(t, s, out.ID); !p.Agent || p.MadeBy != cy || keyFor(t, h, out.ID, cy, "approve") != "" || keyFor(t, h, out.ID, ana, "approve") == "" {
		t.Errorf("an agent's add: %+v", p)
	}
	var places placesOut
	if failed, text := call(t, cs, "places", map[string]any{}, &places); failed || len(places.Places) == 0 {
		t.Errorf("places: %s %+v", text, places)
	}
	// Its person's hour: with the agent's add, and drafts of theirs, at the
	// limit, the agent's next is refused.
	for i := range ledger.ProposalsAnHour - 1 {
		if _, err := s.l.KeepDraft(ctx, ledger.ExpectDraft{Rule: "Errors" + itoa(int64(i%2)), Origin: ledger.DraftHistory, MadeBy: cy, Text: heldTo(60),
			Target: ledger.DraftToNone}); err != nil {
			t.Fatal(err)
		}
	}
	if failed, text := call(t, cs, "propose", map[string]any{"kind": "add", "rule": "ErrorsAgain", "added": strings.Replace(addedRule, "ErrorsLow", "ErrorsAgain", 1),
		"path": "git", "file": "shop.yaml", "group": "shop", "expectations": strings.Replace(heldTo(60), "ErrorsLow", "ErrorsAgain", 1)}, &out); !failed ||
		!strings.Contains(text, ledger.ErrTooManyProposals.Error()) {
		t.Errorf("past the person's hour: %v %s", failed, text)
	}
}

func mustHeld(t *testing.T, s *Server) []int64 {
	t.Helper()
	held, err := s.heldPromotable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// Who may propose one: an editor, through the form; a viewer neither sees
// the form nor posts it.
func TestAddByEditorsOnly(t *testing.T) {
	s, _ := changesServer(t, func(c *Config) { c.Editors = []string{bo} })
	h := s.Handler()
	const vi = "vi@example.com"
	if code, _, _ := ui(t, h, http.MethodGet, "/proposals/new", vi, "", nil); code != http.StatusForbidden {
		t.Errorf("a viewer's form: %d", code)
	}
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	if _, code, _ := proposeAdding(t, h, vi, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(60)}}); code != http.StatusForbidden {
		t.Errorf("a viewer's add: %d", code)
	}
	if code, _, _ := ui(t, h, http.MethodGet, "/proposals/new", bo, "", nil); code != 200 {
		t.Errorf("an editor's form: %d", code)
	}
}

// A rule added backtested by an earlier lapilli is stale: held until it is
// backtested again, which Lapilli does itself, holding it to its
// expectations anew, its approvals withdrawn as any stale proposal's are.
func TestAddStale(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d %s", code, zzMsg(body))
	}
	was := semantics
	semantics++
	t.Cleanup(func() { semantics = was })
	if keyFor(t, h, id, ana, "approve") != "" {
		t.Error("a stale rule added offered for approval")
	}
	if err := s.backtestStale(ctx); err != nil {
		t.Fatal(err)
	}
	ev := proposalEvidence(state(t, s, id))
	if ev.Semantics != semantics || ev.Add == nil || len(ev.Add.Items) != 1 || ev.Add.Items[0].State != "met" || keyFor(t, h, id, ana, "approve") == "" {
		t.Errorf("backtested again: %d %+v", ev.Semantics, ev.Add)
	}
}

// Its pages in each language: the new rule's form, one held up, one
// approved and added with its expectations kept, the list of them.
func TestAddPagesInEachLanguage(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	held, _, _ := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(5)}})
	rule := strings.Replace(addedRule, "ErrorsLow", "ErrorsAdded", 1)
	added, _, _ := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {rule}, "expectations": {strings.Replace(heldTo(60), "ErrorsLow", "ErrorsAdded", 1)}})
	approve(t, h, added, ana)
	s.jobs(ctx, false)
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(added), changesToken, map[string]string{"state": "opened", "link": "https://git.example/shop/pull/9"})
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(added), changesToken, map[string]string{"state": "merged"})
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(state(t, s, added).Document), 0o644)
	raw := ""
	s.look(ctx, &raw)
	s.jobs(ctx, false)
	if _, err := s.l.AddedDraft(ctx, added); err != nil {
		t.Fatalf("its expectations: %v", err)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, n := range names {
		for _, who := range []string{ana, bo} {
			for _, path := range []string{"/proposals/new", "/proposals/" + itoa(held), "/proposals/" + itoa(added), "/proposals?kind=add&state=all"} {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.Header.Set("Accept-Language", n.Code)
				r.Header.Set("X-Forwarded-Email", who)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `<html lang="`+n.Code+`">`) {
					t.Errorf("%s %s as %s: %d", n.Code, path, who, w.Code)
				}
			}
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}

// What a rule added says is held as a change to what a rule says is: a
// template that calls query, or that doesn't parse (which would make
// Prometheus refuse its whole file), or a runbook that isn't an https URL,
// is refused; an expression the history refuses isn't replayed, and so
// isn't approved. A file under a CI system's directory is never a place.
func TestAddWhatItSays(t *testing.T) {
	s, dir := changesServer(t, nil)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	for _, says := range []string{
		"annotations:\n  summary: '{{ query \"up\" }}'\n",
		"annotations:\n  description: '{{ range $labels }}x{{ end }}'\n",
		"annotations:\n  summary: '{{ .Broken'\n",
		"annotations:\n  runbook_url: http://runbooks.example.com/x\n",
		"labels:\n  team: '{{ query \"up\" }}'\n",
		// A label's template, which Prometheus expands at each evaluation
		// and the backtest doesn't: each evaluation another alert.
		"labels:\n  value: '{{ $value }}'\n",
		"labels:\n  service: '{{ $labels.service }}'\n",
	} {
		rule := "alert: ErrorsLow\nexpr: errors > 1\n" + says
		if _, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {rule}, "expectations": {heldTo(60)}}); code != http.StatusBadRequest {
			t.Errorf("%q: %d %s", says, code, zzMsg(body))
		}
	}
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {"alert: ErrorsLow\nexpr: nope(up)\n"}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("an expression the history refuses: %d %s", code, zzMsg(body))
	}
	if why := addHeldWhy(proposalEvidence(state(t, s, id))); why != addHeldUnreplayed || keyFor(t, h, id, ana, "approve") != "" {
		t.Errorf("not replayed, yet approvable: %q", why)
	}
	// A file of rules a CI system runs too (hidden directories, .github's,
	// aren't read at all).
	os.WriteFile(filepath.Join(dir, "azure-pipelines.yml"), []byte(strings.ReplaceAll(ruleFile, "Errors", "Piped")), 0o644)
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.current(context.Background(), "Piped", false); err != nil {
		t.Fatalf("not read: %v", err)
	}
	for _, o := range mustPlaces(t, s) {
		if strings.Contains(o.File, "pipelines") {
			t.Errorf("a CI system's file offered: %+v", o)
		}
	}
	for p, want := range map[string]bool{"alerts/shop.yaml": false, "alerts/.github/x.yaml": true, "cloudbuild.yaml": true, "a/.gitlab-ci.yml": true,
		"Jenkinsfile.yaml": true, "x/package.json": true, "run.sh": true, "rules/shop.rules": false} {
		if ciPath(p) != want {
			t.Errorf("%s: %v", p, !want)
		}
	}
}

// Checked again as it is carried out, as every change is: a rule added
// whose backtest no longer replays it is refused, nothing handed out; in
// Grafana, one whose group another tool keeps since is refused, nothing
// written.
func TestAddCheckedAgain(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	id, _, _ := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(60)}})
	approve(t, h, id, ana)
	s.h.(*history).failLong = map[string]bool{"errors > 1": true}
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || len(handed(t, port)) != 0 {
		t.Errorf("checked again: %s %q", p.State, p.Reason)
	}

	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	gs := grafanaServer(t, g)
	gh := gs.Handler()
	id, code, body := proposeAdding(t, gh, bo, url.Values{"place": {placeKeyOf(t, gs, addPlace{Path: ledger.PathGrafana, Folder: "Platform", Group: "api"})},
		"rule": {addedRule}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d %s", code, zzMsg(body))
	}
	approve(t, gh, id, ana)
	g.provenance["abc"], g.provenance["def"] = "file", "file"
	gs.jobs(ctx, false)
	if p := state(t, gs, id); p.State != ledger.StateRefused || len(g.writes) != 0 {
		t.Errorf("a group another tool keeps: %s %q %v", p.State, p.Reason, g.writes)
	}
}

// A new rule by the git path, all the way through a change recipe: its
// rule file's group with it added, and a new group after the last, each
// pushed on a branch of its own and opened as a pull request, the
// recipe's own guards holding each (only the rules of a rule file under
// RULES_PATHS changed).
func TestChangeRecipeAdds(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	c := recipeHosts[0]
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h := s.Handler()
	in := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	apart := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", NewGroup: true})
	one, _, _ := proposeAdding(t, h, bo, url.Values{"place": {in}, "rule": {addedRule}, "expectations": {heldTo(60)}})
	two, _, _ := proposeAdding(t, h, bo, url.Values{"place": {apart}, "group": {"slow"}, "rule": {strings.Replace(addedRule, "ErrorsLow", "ErrorsSlow", 1)},
		"expectations": {strings.Replace(heldTo(60), "ErrorsLow", "ErrorsSlow", 1)}})
	approve(t, h, one, ana)
	approve(t, h, two, ana)
	s.jobs(ctx, false)
	lapilli := httptest.NewServer(s.IncidentsHandler())
	defer lapilli.Close()
	host := &prHost{t: t, kind: c.kind, auth: c.auth}
	hs := httptest.NewServer(host)
	defer hs.Close()
	host.url = hs.URL
	env := gitEnv(t)
	bare := remote(t, env, map[string]string{"alerts/shop.yaml": ruleFile, "README.md": "rules\n"})
	cmd := exec.Command("sh", changeRecipes+c.script)
	cmd.Env = append(append(env, "LAPILLI_URL="+lapilli.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE="+bare, "RULES_PATHS=alerts"),
		c.env(hs.URL, tokenFile(t, "host-token-0123456789"))...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", c.script, err, out)
	}
	for _, id := range []int64{one, two} {
		p := state(t, s, id)
		if p.State != ledger.StateOpened {
			t.Errorf("#%d: %s %q", id, p.State, p.Reason)
			continue
		}
		if got := gitIn(t, env, bare, "show", "lapilli/"+s.ledgerID+"/proposal-"+itoa(id)+":alerts/shop.yaml"); got != p.Document {
			t.Errorf("#%d's branch:\n%s\nwant:\n%s", id, got, p.Document)
		}
	}
	if len(host.opened) != 2 {
		t.Errorf("pull requests: %v", host.opened)
	}
}
