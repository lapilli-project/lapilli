package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// Adding a rule, the guards a review found missing: an approved add
// carried out while a comparison runs, the expectations of adds that can't
// be kept, a file of two PrometheusRules, whom a Grafana rule added tells,
// which CloudWatch actions only tell.

// waitsInLock reports whether a goroutine waits for a sync.Mutex inside
// fn: what the stacks say, so a test needs no clock to know it got there.
func waitsInLock(fn string) bool {
	buf := make([]byte, 16<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		head, _, _ := strings.Cut(g, "\n")
		if strings.Contains(head, "sync.Mutex.Lock") && strings.Contains(g, fn) {
			return true
		}
	}
	return false
}

// An approved add carried out while another comparison runs (an agent's
// compare or test_expectation) waits for it, as a change's backtest does,
// and is carried out once it is over: it isn't failed for good.
func TestAddCarriedOutWhileComparing(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d %s", code, zzMsg(body))
	}
	if code, body := approve(t, h, id, ana); code != http.StatusSeeOther {
		t.Fatalf("approving: %d %s", code, body)
	}
	s.comparing.Lock() // another comparison, running
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.jobs(ctx, false)
	}()
	for !waitsInLock("(*Server).replayHeld") {
		select {
		case <-done:
			s.comparing.Unlock()
			p := state(t, s, id)
			t.Fatalf("carried out without waiting for the comparison: %s %q", p.State, p.Reason)
		default:
			runtime.Gosched()
		}
	}
	s.comparing.Unlock()
	<-done
	if p := state(t, s, id); p.State != ledger.StateReady {
		t.Errorf("once the comparison was over: %s %q", p.State, p.Reason)
	}
}

// Adds whose expectations can't be kept hold none up: twenty added with no
// one's approval (their diff merged by hand) and twenty whose evidence
// doesn't read come before an approved one, which is kept all the same,
// the second round at most; those that can't be kept leave the list. A
// git add merged by hand before anyone approved it is applied, its
// expectations never kept.
func TestAddsUnkeptHoldNoneUp(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "shop.yaml", Group: "shop"})
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {addedRule}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d %s", code, zzMsg(body))
	}
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(state(t, s, id).Document), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, id); p.State != ledger.StateApplied || len(p.Approvals) != 0 {
		t.Fatalf("merged by hand: %s %d", p.State, len(p.Approvals))
	}
	expect := `{"text":"expectations:\n  - rule: R\n    budget:\n      week: 5\n"}`
	made := func(i int, rule, evidence string, approved bool) int64 {
		t.Helper()
		id, err := s.l.Propose(ctx, ledger.Proposal{Rule: rule, Kind: ledger.KindAdd, Path: ledger.PathGit, MadeBy: "u" + itoa(int64(i)) + "@example.com",
			Expect: expect, AfterHash: "h", Evidence: json.RawMessage(evidence)})
		if err != nil {
			t.Fatal(err)
		}
		from := ledger.StateProposed
		if approved {
			if _, err := s.l.Approve(ctx, id, ana, 1, time.Time{}); err != nil {
				t.Fatal(err)
			}
			from = ledger.StateApproved
		}
		if _, err := s.l.Move(ctx, id, []string{from}, ledger.StateApplied, ledger.Proposer, ledger.Step{}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i := range 20 {
		made(i, "Hand"+itoa(int64(i)), `{"add":{"place":{"path":"git"},"rule":"x","items":[]}}`, false)
	}
	for i := range 20 {
		made(20+i, "Unread"+itoa(int64(i)), `{}`, true)
	}
	kept := made(40, "Approved", `{"add":{"place":{"path":"git"},"rule":"x","items":[]}}`, true)
	for range 2 {
		if err := s.addedExpectations(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if d, err := s.l.AddedDraft(ctx, kept); err != nil || d.DecidedBy != ana {
		t.Errorf("the approved add's expectations: %+v %v", d, err)
	}
	if ids, err := s.l.AddsUnkept(ctx, 50); err != nil || len(ids) != 0 {
		t.Errorf("still waiting: %v %v", ids, err)
	}
	if _, err := s.l.AddedDraft(ctx, id); err == nil {
		t.Error("an add merged by hand had its expectations kept, accepted by no one")
	}
}

// A file of two PrometheusRules: each one's group is a place, and a rule
// added to the second's is written there, the first as it was; a new group
// isn't offered in it, which would take it isn't the file's to say.
func TestAddToEitherPrometheusRule(t *testing.T) {
	s, dir := changesServer(t, nil)
	two := "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: a\nspec:\n  groups:\n  - name: one\n    rules:\n    - alert: A\n      expr: errors > 3\n---\n" +
		"apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: b\nspec:\n  groups:\n  - name: two\n    rules:\n    - alert: B\n      expr: errors > 4\n"
	os.WriteFile(filepath.Join(dir, "two.yaml"), []byte(two), 0o644)
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, o := range mustPlaces(t, s) {
		if o.File == "two.yaml" && o.NewGroup {
			t.Errorf("a new group offered in two PrometheusRules: %+v", o)
		}
	}
	if _, _, err := placeFor(mustPlaces(t, s), addPlace{Path: ledger.PathGit, File: "shop.yaml", NewGroup: true, Group: "slow"}); err != nil {
		t.Errorf("a new group in a file of one: %v", err)
	}
	h := s.Handler()
	first, _, _ := strings.Cut(two, "---\n")
	for _, g := range []string{"one", "two"} {
		rule := strings.Replace(addedRule, "ErrorsLow", "ErrorsIn"+g, 1)
		id, code, body := proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: "two.yaml", Group: g})},
			"rule": {rule}, "expectations": {strings.Replace(heldTo(60), "ErrorsLow", "ErrorsIn"+g, 1)}})
		if code != http.StatusSeeOther {
			t.Fatalf("group %s: %d %s", g, code, zzMsg(body))
		}
		p := state(t, s, id)
		if g == "two" && (!strings.HasPrefix(p.Document, first+"---\n") || !strings.Contains(p.Document, "    - alert: \"ErrorsIntwo\"\n")) {
			t.Errorf("added to group two:\n%s", p.Document)
		}
		if g == "one" && !strings.HasSuffix(p.Document, "---\n"+strings.SplitN(two, "---\n", 2)[1]) {
			t.Errorf("added to group one, the second document changed:\n%s", p.Document)
		}
	}
}

// grafanaTold is a Grafana stand-in whose rules each tell as told says
// (uid to a contact point, "" for none).
func grafanaTold(t *testing.T, told map[string]string) (*grafanaStandIn, *Server) {
	t.Helper()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	for uid, cp := range told {
		if cp != "" {
			g.rules[uid]["notification_settings"] = map[string]any{"receiver": cp}
		}
	}
	return g, grafanaServer(t, g)
}

// Whom a Grafana rule added tells: where every rule of its group names a
// contact point, one of theirs, never none (Grafana's notification
// policies would route it, by its labels, to any contact point); where
// none of them names one, none, routed as they are, its page saying so;
// where some do, either. Checked again as it is carried out: one naming
// none, where every rule there names one since, is refused, nothing
// written.
func TestAddGrafanaWhomItTells(t *testing.T) {
	ctx := context.Background()
	_, s := grafanaTold(t, map[string]string{"abc": "payments-slack", "def": "oncall"})
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGrafana, Folder: "Platform", Group: "api"})
	form := url.Values{"place": {key}, "rule": {"alert: ErrorsLow\nexpr: errors > 1\nlabels:\n  team: security\n"}, "expectations": {heldTo(60)}}
	if _, code, body := proposeAdding(t, h, bo, form); code != http.StatusBadRequest || !strings.Contains(body, "names its contact point") {
		t.Errorf("none named where every rule names one: %d %s", code, zzMsg(body))
	}
	form.Set("tells", "oncall")
	if _, code, body := proposeAdding(t, h, bo, form); code != http.StatusSeeOther {
		t.Errorf("one of theirs: %d %s", code, zzMsg(body))
	}

	_, s = grafanaTold(t, nil)
	h = s.Handler()
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, addPlace{Path: ledger.PathGrafana, Folder: "Platform", Group: "api"})},
		"rule": {addedRule}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("none, where none names one: %d %s", code, zzMsg(body))
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); !strings.Contains(body, "It names no contact point") {
		t.Error("its page doesn't say Grafana's policies route it")
	}

	g, s := grafanaTold(t, map[string]string{"def": "oncall"})
	h = s.Handler()
	id, code, body = proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, addPlace{Path: ledger.PathGrafana, Folder: "Platform", Group: "api"})},
		"rule": {addedRule}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("none, where some name none: %d %s", code, zzMsg(body))
	}
	approve(t, h, id, ana)
	g.rules["abc"]["notification_settings"] = map[string]any{"receiver": "payments-slack"}
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || len(g.writes) != 0 {
		t.Errorf("every rule there names one now: %s %q %v", p.State, p.Reason, g.writes)
	}
}

// The CloudWatch actions a rule added may take, of those the alarms there
// take: an SNS topic, an OpsItem, a response plan; never a Lambda function
// or a Systems Manager automation, which run code, nor anything else.
func TestAddCloudWatchTellsOnly(t *testing.T) {
	for arn, want := range map[string]bool{
		"arn:aws:sns:eu-west-1:111122223333:oncall":                                                         true,
		"arn:aws:sns:eu-west-1:111122223333:oncall.fifo":                                                    true,
		"arn:aws-us-gov:sns:us-gov-west-1:111122223333:oncall":                                              true,
		"arn:aws:ssm:eu-west-1:111122223333:opsitem:2":                                                      true,
		"arn:aws:ssm:eu-west-1:111122223333:opsitem:3#CATEGORY=Availability":                                true,
		"arn:aws:ssm-incidents::111122223333:response-plan/checkout":                                        true,
		"arn:aws:lambda:eu-west-1:111122223333:function:failover-db":                                        false,
		"arn:aws:ssm:eu-west-1:111122223333:automation-definition/AWS-RestartEC2Instance:$DEFAULT":          false,
		"arn:aws:ssm:eu-west-1:111122223333:document/Restart":                                               false,
		"arn:aws:automate:eu-west-1:ec2:stop":                                                               false,
		"arn:aws:autoscaling:eu-west-1:111122223333:scalingPolicy:x:autoScalingGroupName/web:policyName/up": false,
		"arn:aws:sns:eu-west-1:111122223333:oncall:extra":                                                   false,
		"arn:aws:sns:eu-west-1:111122223333:oncall\n":                                                       false,
		"arn:aws:ssm:eu-west-1:111122223333:opsitem:9":                                                      false,
	} {
		if tellsOnly.MatchString(arn) != want {
			t.Errorf("%q: only tells %v", arn, !want)
		}
	}
	cw := cloudWatchAlarms()
	lambda := "arn:aws:lambda:eu-west-1:111122223333:function:failover-db"
	automation := "arn:aws:ssm:eu-west-1:111122223333:automation-definition/AWS-RestartEC2Instance:$DEFAULT"
	opsitem := "arn:aws:ssm:eu-west-1:111122223333:opsitem:2"
	fix := cwPromQL("checkout-fix", "fix > 5")
	fix["AlarmActions"] = []any{lambda, automation, opsitem}
	cw.Add(fix, "OK", nil)
	s := cwServer(t, cw, nil)
	h := s.Handler()
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathAlarm, Alarms: "cw"})
	for _, o := range mustPlaces(t, s) {
		for _, x := range o.Tells {
			if x == lambda || x == automation {
				t.Errorf("offered: %s", x)
			}
		}
	}
	rule := "alert: checkout-low\nexpr: errors > 1\nannotations:\n  description: Low errors at checkout.\n"
	held := strings.Replace(heldTo(60), "ErrorsLow", "checkout-low", 1)
	for _, x := range []string{lambda, automation} {
		if _, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {rule}, "expectations": {held}, "tells": {x}}); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", x, code, zzMsg(body))
		}
	}
	if _, code, body := proposeAdding(t, h, bo, url.Values{"place": {key}, "rule": {rule}, "expectations": {held}, "tells": {opsitem}}); code != http.StatusSeeOther {
		t.Errorf("an OpsItem the alarms there take: %d %s", code, zzMsg(body))
	}
}
