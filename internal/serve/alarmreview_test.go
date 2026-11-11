package serve

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// What two reviews of the alarm path found (docs/design/alarms.md, "As
// built"), each held by a test that fails without its fix.

// undo asks for an applied change to be undone, as ana, and carries it out.
func undo(t *testing.T, s *Server, id int64) ledger.Proposal {
	t.Helper()
	h := s.Handler()
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	if err := s.carryOut(context.Background()); err != nil {
		t.Fatal(err)
	}
	return state(t, s, locID(hd))
}

// restart is lapilli serve started again: its alarms not read yet.
func restart(t *testing.T, s *Server, raw string) {
	t.Helper()
	fresh, err := source.NewAlarms(s.alarms[0].Source, raw, "lapilli/test")
	if err != nil {
		t.Fatal(err)
	}
	fresh.Transport, fresh.Reads, fresh.Now = s.alarms[0].Transport, s.alarms[0].Reads, s.alarms[0].Now
	s.alarms[0] = fresh
}

// deleted retires a PromQL alarm: off, then deleted four weeks later.
func deleted(t *testing.T, s *Server, rule string) int64 {
	t.Helper()
	ctx := context.Background()
	h := s.Handler()
	var off int64
	if ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: rule, States: []string{ledger.StateProposed}}); len(ps) == 1 && ps[0].Kind == ledger.KindDisable {
		off = ps[0].ID
	} else {
		off = proposeRule(t, h, rule, "disable", nil)
	}
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if p := state(t, s, off); p.State != ledger.StateApplied {
		t.Fatalf("%s off: %s %q", rule, p.State, p.Reason)
	}
	raw := ""
	s.look(ctx, &raw)
	was := s.now
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	defer func() { s.now = was }()
	if err := s.proposeFound(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: rule, States: []string{ledger.StateProposed}})
	if len(ps) != 1 || ps[0].Kind != ledger.KindDelete {
		t.Fatalf("deleting %s: %+v", rule, ps)
	}
	approve(t, h, ps[0].ID, cy)
	s.jobs(ctx, false)
	if p := state(t, s, ps[0].ID); p.State != ledger.StateApplied {
		t.Fatalf("%s deleted: %s %q", rule, p.State, p.Reason)
	}
	return ps[0].ID
}

// M1: an undo is a write as any other. It is refused where the change it
// undoes would be, now: an alarm since made automation, or given a
// warm-up; one --changes-in no longer names, after a restart too; and a
// deleted alarm isn't made again once --changes-in no longer names it, or
// its tags, as kept, are another tool's, on CloudWatch and Google alike.
func TestAlarmUndoAsAWrite(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		set  func(*alarmtest.CloudWatch, *Server)
		want string
	}{
		"made automation since": {func(cw *alarmtest.CloudWatch, _ *Server) {
			cw.Metric["checkout-5xx"]["AlarmActions"] = []any{"arn:aws:autoscaling:eu-west-1:111122223333:scalingPolicy:x:autoScalingGroupName/web:policyName/up"}
		}, noPathAutomation},
		"given a warm-up since": {func(cw *alarmtest.CloudWatch, _ *Server) {
			cw.Metric["checkout-5xx"]["WarmUpConfiguration"] = map[string]any{"WarmUpPeriodDurationInMinutes": 10}
		}, noPathWarmUp},
		"not named since, restarted": {func(_ *alarmtest.CloudWatch, s *Server) {
			s.cfg.ChangesIn = []string{"cw:latency"}
			restart(t, s, cwURL)
		}, noPathAlarmIn},
	} {
		t.Run(name, func(t *testing.T) {
			cw := cloudWatchAlarms()
			s := cwServer(t, cw, nil)
			id := proposeRule(t, s.Handler(), "checkout-5xx", "for", url.Values{"for": {"15m"}})
			approve(t, s.Handler(), id, ana)
			s.jobs(ctx, false)
			cw.Lock()
			c.set(cw, s)
			cw.Unlock()
			if u := undo(t, s, id); u.State != ledger.StateRefused || u.Reason != c.want || len(cw.Took()) != 1 {
				t.Errorf("%s %q, written %v", u.State, u.Reason, cw.Took())
			}
		})
	}
	t.Run("deleted, then not named or another tool's", func(t *testing.T) {
		for _, set := range []func(*Server){
			func(s *Server) { s.cfg.ChangesIn = []string{"cw:checkout-*"} },
			func(s *Server) { s.cfg.ChangesManaged = []string{"team=shop"} },
		} {
			cw := cloudWatchAlarms()
			s := cwServer(t, cw, nil)
			s.now = func() time.Time { return now.Add(13 * weekLen) }
			s.proposeFound(ctx)
			del := deleted(t, s, "latency")
			set(s)
			if u := undo(t, s, del); u.State != ledger.StateRefused || cw.Alarm("latency") != nil {
				t.Errorf("made again: %s %q", u.State, u.Reason)
			}
		}
	})
	t.Run("a Google policy deleted, then not named", func(t *testing.T) {
		g, _ := googlePolicies()
		cloudErrs(t, &g.Common)
		s := alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", g, []string{"gcp:Down", "gcp:Shop"}, nil)
		del := deleted(t, s, "Down")
		s.cfg.ChangesIn = []string{"gcp:Shop"}
		if u := undo(t, s, del); u.State != ledger.StateRefused || u.Reason != noPathAlarmIn || g.Took()[len(g.Took())-1] == "POST create" {
			t.Errorf("made again: %s %q %v", u.State, u.Reason, g.Took())
		}
	})
}

// M2: an alarm's parents are asked with ParentsOfAlarmName alone, as AWS
// takes it (the stand-in refuses any other parameter with it): one a
// composite alarm watches, or two do, isn't deleted, one none watches is;
// and an error, a throttle, is the cloud's, not "a composite alarm watches
// it".
func TestAlarmCompositeParents(t *testing.T) {
	ctx := context.Background()
	cw := cloudWatchAlarms()
	cw.AddComposite("shop-slow", "ALARM(latency) OR ALARM(cpu-high)")
	s := cwServer(t, cw, nil)
	tg := alarmTarget{s}
	for name, want := range map[string]string{"checkout-5xx": noPathWatched, "cpu-high": noPathWatched, "scale-out": noPathAutomation, "warm": ""} {
		if why, err := tg.offered(ctx, mustUnit(t, s, "cw", cw.ARN(name)), ledgerRule(t, s, name), ledger.KindDelete); why != want || err != nil {
			t.Errorf("%s: %q %v, want %q", name, why, err, want)
		}
	}
	cw.FailParents = true
	if why, err := tg.offered(ctx, mustUnit(t, s, "cw", cw.ARN("warm")), ledgerRule(t, s, "warm"), ledger.KindDelete); err == nil || why != "" {
		t.Errorf("throttled: %q %v", why, err)
	}
}

// downPolicy is the Google policy "Down" as a person makes it again.
func downPolicy() map[string]any {
	return map[string]any{"displayName": "Down", "enabled": true, "combiner": "OR",
		"documentation": map[string]any{"subject": "Down", "content": "Down."},
		"conditions": []any{map[string]any{"displayName": "Down",
			"conditionPrometheusQueryLanguage": map[string]any{"query": "up == 0", "duration": "60s", "alertRule": "Down"}}}}
}

// M3: a Google policy deleted here, and made again by a person in the
// console under its display name, isn't made a second time by the
// deletion's undo, whatever it holds. Made again by the undo, it is read back by
// the name the create answered, even when a person makes one alike a
// moment after; when the answer is lost, by what it holds, and only when
// one policy alone holds it.
func TestAlarmGoogleUndoAgain(t *testing.T) {
	named := func(g *alarmtest.Google, title string) int {
		g.Lock()
		defer g.Unlock()
		n := 0
		for _, p := range g.Policies {
			if p["displayName"] == title {
				n++
			}
		}
		return n
	}
	setup := func() (*alarmtest.Google, *Server, int64) {
		g, _ := googlePolicies()
		cloudErrs(t, &g.Common)
		s := alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", g, []string{"gcp:*"}, nil)
		return g, s, deleted(t, s, "Down")
	}
	for title, person := range map[string]func(map[string]any){
		"as it was":     func(map[string]any) {},
		"holding other": func(p map[string]any) { p["conditions"].([]any)[0].(map[string]any)["displayName"] = "Up" },
	} {
		g, s, del := setup()
		p := downPolicy()
		person(p)
		g.Lock()
		g.Add(p)
		g.Unlock()
		if u := undo(t, s, del); u.State != ledger.StateRefused || !strings.Contains(u.Reason, "in the project again") || named(g, "Down") != 1 {
			t.Errorf("made again by a person, %s: %s %q, %d named Down", title, u.State, u.Reason, named(g, "Down"))
		}
	}

	// A person makes one alike right after the undo does.
	alike := func(g *alarmtest.Google, made *string) func() {
		return func() {
			for n, p := range g.Policies {
				if p["displayName"] == "Down" {
					*made = n
				}
			}
			x := downPolicy()
			x["enabled"] = false // as the undo makes it: off, as it was deleted
			g.Add(x)
			g.AfterCreate = nil
		}
	}
	g, s, del := setup()
	var made string
	g.AfterCreate = alike(g, &made)
	u := undo(t, s, del)
	if m, _ := decodeTree([]byte(u.Document)); u.State != ledger.StateApplied || str(m["name"]) != made {
		t.Errorf("read back by the create's answer: %s %q, %s, made %s", u.State, u.Reason, str(m["name"]), made)
	}

	// The create's answer lost: read back by what it holds.
	g, s, del = setup()
	g.LoseCreate = true
	if u := undo(t, s, del); u.State != ledger.StateApplied || named(g, "Down") != 1 {
		t.Errorf("answer lost: %s %q, %d named Down", u.State, u.Reason, named(g, "Down"))
	}
	// And two hold it: which is Lapilli's isn't known.
	g, s, del = setup()
	g.LoseCreate = true
	g.AfterCreate = alike(g, &made)
	if u := undo(t, s, del); u.State != ledger.StateApplying {
		t.Errorf("answer lost, two alike: %s %q", u.State, u.Reason)
	}
}

// L1: an undone deletion the cloud took but reads as gone still is read
// again, as any change read as it was is, for two minutes, then failed.
func TestAlarmUndoDeletionReadBehind(t *testing.T) {
	ctx := context.Background()
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	s.now = func() time.Time { return now.Add(13 * weekLen) }
	s.proposeFound(ctx)
	del := deleted(t, s, "latency")
	s.now = time.Now
	cw.Lock()
	cw.Ignore = true
	cw.Unlock()
	if u := undo(t, s, del); u.State != ledger.StateApplying || u.Reason != readBehind {
		t.Fatalf("at first: %s %q", u.State, u.Reason)
	}
}

// waits turns rule off, approved, and says whether it waits for its alarm
// to stop firing; then lets it stop (stop), and says it is turned off.
func waits(t *testing.T, s *Server, rule string, stop func()) {
	t.Helper()
	ctx := context.Background()
	h := s.Handler()
	id := proposeRule(t, h, rule, "disable", nil)
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApproved || p.Reason != firesNote {
		t.Errorf("%s while it fires: %s %q", rule, p.State, p.Reason)
	}
	stop()
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied {
		t.Errorf("%s once it stopped: %s %q", rule, p.State, p.Reason)
	}
}

// M4: turning a cloud's alarm off waits while the cloud's own state has
// it firing, on every cloud: an Azure alert of the rule (its id in another
// letter case) whose monitor condition is Fired and that no one closed,
// asked of the rule alone, 250 a page; an open alert of a Google policy,
// its project named by number;
// an OCI alarm whose status is FIRING; and while its history, copied in,
// has an alert of its ID on, though a PromQL alarm's history is named
// apart from its replay.
func TestAlarmWaitsWhileCloudFires(t *testing.T) {
	z, zids := azureAlarms()
	cloudErrs(t, &z.Common)
	s := alarmServer(t, "az", "https://management.azure.com/subscriptions/sub-1", z, []string{"az:SHOP-RG/*"}, nil)
	for range 260 {
		z.Alerts = append(z.Alerts, alarmtest.AzureAlert{Rule: zids["checkout-errors"], Condition: "Fired", State: "Closed", Start: now.Add(-2 * time.Hour)})
	}
	z.Alerts = append(z.Alerts, alarmtest.AzureAlert{Rule: zids["log-errors"], Condition: "Fired", State: "New", Start: now.Add(-time.Hour)},
		alarmtest.AzureAlert{Rule: strings.ToUpper(zids["checkout-errors"]), Condition: "Fired", State: "Acknowledged", Start: now.Add(-time.Hour)})
	waits(t, s, "checkout-errors", func() {
		z.Lock()
		z.Alerts[len(z.Alerts)-1].Condition = "Resolved"
		z.Unlock()
	})
	if q := z.AlertsAsked[0]; q.Get("alertRule") != zids["checkout-errors"] || q.Get("pageCount") != "250" || q.Get("monitorCondition") != "Fired" {
		t.Errorf("asked: %v", q)
	}

	g, gids := googlePolicies()
	cloudErrs(t, &g.Common)
	s = alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", g, []string{"gcp:*"}, nil)
	// An alert names the policy's project by number, as --alarms doesn't.
	byNumber := strings.Replace(gids["one"], "projects/shop/", "projects/123456789012/", 1)
	g.Alerts = [][2]string{{gids["two"], "OPEN"}, {gids["one"], "CLOSED"}, {byNumber, "OPEN"}}
	waits(t, s, "Down", func() {
		g.Lock()
		g.Alerts[2][1] = "CLOSED"
		g.Unlock()
	})

	o, oids := ociAlarms(t)
	cloudErrs(t, &o.Common)
	s = alarmServer(t, "oci", "https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..shop", o,
		[]string{"oci:*"}, nil)
	old := now.Add(-13 * weekLen) // its history, tied over twelve weeks
	copyFirings(t, s, "oci", rfc(old.Add(-time.Hour)), rfc(old.Add(time.Hour)),
		`{"rule":"cpu","labels":{"alarm_id":"`+oids["cpu"]+`"},"start":"`+rfc(old)+`","end":"`+rfc(old.Add(30*time.Minute))+`"}`+"\n")
	copyFirings(t, s, "oci", rfc(now.Add(-time.Hour)), rfc(now), "") // and up to now
	o.Firing = []string{oids["checkout"], oids["cpu"]}
	waits(t, s, "cpu", func() {
		o.Lock()
		o.Firing = o.Firing[:1]
		o.Unlock()
	})
	// Whoever holds the firings token can't lift the wait while the
	// cloud's state says it fires.
	o.Firing = []string{oids["checkout"]}
	copyFirings(t, s, "oci", rfc(now.Add(-time.Hour)), rfc(now.Add(time.Minute)),
		`{"rule":"checkout-errors","labels":{"alarm_id":"`+oids["checkout"]+`"},"start":"`+rfc(now.Add(-20*time.Minute))+`"}`+"\n")
	waits(t, s, "checkout-errors", func() {
		copyFirings(t, s, "oci", rfc(now.Add(-time.Hour)), rfc(now.Add(time.Minute)), "")
		ctx := context.Background()
		s.jobs(ctx, false)
		ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "checkout-errors", States: []string{ledger.StateApproved}})
		if len(ps) != 1 || ps[0].Reason != firesNote {
			t.Errorf("the firings token lifted the wait: %+v", ps)
		}
		o.Lock()
		o.Firing = nil
		o.Unlock()
	})

	// A PromQL policy's history, named apart from its replay, open.
	g, gids = googlePolicies()
	cloudErrs(t, &g.Common)
	s = alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", g, []string{"gcp:*"}, nil)
	open := `{"rule":"Down","labels":{"alarm_id":"` + gids["one"] + `"},"start":"` + rfc(now.Add(-30*time.Minute)) + `"}` + "\n"
	copyFirings(t, s, "gcp", rfc(now.Add(-2*time.Hour)), rfc(now.Add(time.Minute)), open)
	waits(t, s, "Down", func() {
		copyFirings(t, s, "gcp", rfc(now.Add(-2*time.Hour)), rfc(now.Add(time.Minute)), "")
	})
	// An Azure rule's, by its id in another letter case.
	z, zids = azureAlarms()
	cloudErrs(t, &z.Common)
	s = alarmServer(t, "az", "https://management.azure.com/subscriptions/sub-1", z, []string{"az:SHOP-RG/*"}, nil)
	copyFirings(t, s, "az", rfc(now.Add(-2*time.Hour)), rfc(now.Add(time.Minute)),
		`{"rule":"checkout-errors","labels":{"alarm_id":"`+strings.ToUpper(zids["checkout-errors"])+`"},"start":"`+rfc(now.Add(-30*time.Minute))+`"}`+"\n")
	waits(t, s, "checkout-errors", func() {
		copyFirings(t, s, "az", rfc(now.Add(-2*time.Hour)), rfc(now.Add(time.Minute)), "")
	})
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// M7: an alarm's record is its history's copies from twelve weeks back up
// to now: copies that stopped (longer ago than --source-quiet) are no
// record to retire it by, until its source's are copied in again.
// Retiring it is proposed then, from its firing thirteen weeks back.
func TestAlarmRecordUpToNow(t *testing.T) {
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	h := s.Handler()
	old := now.Add(-13 * weekLen)
	copyFirings(t, s, "cw", rfc(old.Add(-time.Hour)), rfc(old.Add(time.Hour)),
		`{"rule":"cpu-high","labels":{"alarm_id":"`+cw.ARN("cpu-high")+`"},"start":"`+rfc(old)+`","end":"`+rfc(old.Add(30*time.Minute))+`"}`+"\n")
	form := url.Values{"rule": {"cpu-high"}, "kind": {"disable"}, "from": {"silent"}}
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", form); code != http.StatusBadRequest || !strings.Contains(body, "copied in only up to") {
		t.Errorf("copies stopped thirteen weeks ago: %d %s", code, zzMsg(body))
	}
	copyFirings(t, s, "cw", rfc(now.Add(-time.Hour)), rfc(now), "")
	if p := state(t, s, proposeRule(t, h, "cpu-high", "disable", url.Values{"from": {"silent"}})); p.Path != ledger.PathAlarm {
		t.Errorf("copied in up to now: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
}

// L2: an approved change waiting while the cloud says its alarm fires,
// though the ledger has no alert of it on, says so once, not "stopped" and
// "fires" again each round; once it stops, it is carried out, and says it
// stopped, as applied.
func TestAlarmFiresSaidOnce(t *testing.T) {
	ctx := context.Background()
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	h := s.Handler()
	cw.Lock()
	cw.Metric["latency"]["StateValue"] = "ALARM"
	cw.Unlock()
	id := proposeRule(t, h, "latency", "disable", nil)
	approve(t, h, id, ana)
	moves := func() int {
		cs, err := s.l.Changes(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range cs {
			if c.What == "proposal #"+itoa(id)+" approved" {
				n++
			}
		}
		return n
	}
	s.jobs(ctx, false)
	was := moves()
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApproved || p.Reason != firesNote || moves() != was {
		t.Errorf("rounds while it fires: %s %q, %d moves then %d", p.State, p.Reason, was, moves())
	}
	cw.Lock()
	cw.Metric["latency"]["StateValue"] = "OK"
	cw.Unlock()
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || p.Reason != firedNote {
		t.Errorf("once it stopped: %s %q", p.State, p.Reason)
	}
}

// L3: an Azure alert rule given a description where it had none, undone:
// the PATCH says the description is empty, as a PATCH merges and one
// leaving it out would leave it.
func TestAlarmAzureUndoDescription(t *testing.T) {
	ctx := context.Background()
	z, _ := azureAlarms()
	cloudErrs(t, &z.Common)
	id := z.Add("shop-rg", "metricAlerts", "quiet-errors", map[string]any{"severity": 3, "enabled": true, "evaluationFrequency": "PT1M",
		"scopes": []any{"/subscriptions/sub-1/resourceGroups/shop-rg/providers/microsoft.monitor/accounts/shop"},
		"criteria": map[string]any{"odata.type": rules.AzurePromQL, "failingPeriods": map[string]any{"for": "PT5M"},
			"allOf": []any{map[string]any{"name": "M1", "criterionType": "StaticThresholdCriterion", "query": "quiet > 5"}}}}, nil)
	s := alarmServer(t, "az", "https://management.azure.com/subscriptions/sub-1", z, []string{"az:SHOP-RG/*"}, nil)
	h := s.Handler()
	said := proposeRule(t, h, "quiet-errors", "says", url.Values{"annotation": {"description"}, "text": {"Quiet errors."}})
	approve(t, h, said, ana)
	s.jobs(ctx, false)
	if p := state(t, s, said); p.State != ledger.StateApplied {
		t.Fatalf("said: %s %q", p.State, p.Reason)
	}
	u := undo(t, s, said)
	desc, had := z.Rule(id)["properties"].(map[string]any)["description"]
	if u.State != ledger.StateApplied || had && desc != "" {
		t.Errorf("undone: %s %q, description %v", u.State, u.Reason, desc)
	}
}

// L5: a CloudWatch alarm is written only in its own account: CloudWatch's
// writes name an alarm by its name, in the account of whoever calls, so a
// role of another account, or the pod's own identity in another (STS
// GetCallerIdentity), is refused, and an account not known fails it.
func TestAlarmWriteAccount(t *testing.T) {
	ctx := context.Background()
	carried := func(s *Server) ledger.Proposal {
		h := s.Handler()
		id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		s.jobs(ctx, false)
		return state(t, s, id)
	}
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, func(c *Config) { c.ChangesAWSRole = "arn:aws:iam::444455556666:role/lapilli-alarms" })
	if p := carried(s); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "written in 444455556666") || len(cw.Took()) != 0 {
		t.Errorf("a role of another account: %s %q %v", p.State, p.Reason, cw.Took())
	}

	for account, want := range map[string]string{"444455556666": ledger.StateRefused, "": ledger.StateFailed, "111122223333": ledger.StateApplied} {
		cw := cloudWatchAlarms()
		cw.Writer = "pod"
		s := cwServer(t, cw, func(c *Config) { c.ChangesAWSRole = "" })
		callerAccount = func(context.Context, string) (string, error) {
			if account == "" {
				return "", errors.New("no credentials")
			}
			return account, nil
		}
		if p := carried(s); p.State != want || (want != ledger.StateApplied) != (len(cw.Took()) == 0) {
			t.Errorf("the pod's own identity in %q: %s %q %v", account, p.State, p.Reason, cw.Took())
		}
	}
}

// An Azure alert of a stateless rule stays Fired until someone closes it:
// an activity log alert's, or a metric alert's that doesn't resolve
// itself. Turning such a rule off waits only while an alert of it began
// within its evaluation window (an activity log alert's, five minutes),
// not for the thirty days Alerts Management keeps it Fired.
func TestAlarmAzureStateless(t *testing.T) {
	ctx := context.Background()
	server := func(stateless bool) (*alarmtest.Azure, map[string]string, *Server) {
		z, ids := azureAlarms()
		cloudErrs(t, &z.Common)
		if stateless {
			z.Rules[strings.ToLower(ids["checkout-errors"])]["properties"].(map[string]any)["autoMitigate"] = false
		}
		s := alarmServer(t, "az", "https://management.azure.com/subscriptions/sub-1", z, []string{"az:SHOP-RG/*"}, nil)
		old := now.Add(-13 * weekLen) // the activity log alert's history, tied, twelve weeks up to now
		copyFirings(t, s, "az", rfc(old.Add(-time.Hour)), rfc(old.Add(time.Hour)),
			`{"rule":"deletes","labels":{"alarm_id":"`+ids["deletes"]+`"},"start":"`+rfc(old)+`","end":"`+rfc(old)+`"}`+"\n")
		copyFirings(t, s, "az", rfc(now.Add(-time.Hour)), rfc(now), "")
		return z, ids, s
	}
	at := func(z *alarmtest.Azure, id string, ago time.Duration) {
		z.Lock()
		z.Alerts = []alarmtest.AzureAlert{{Rule: id, Condition: "Fired", State: "New", Start: now.Add(-ago)}}
		z.Unlock()
	}
	goes := func(s *Server, rule string) {
		t.Helper()
		h := s.Handler()
		id := proposeRule(t, h, rule, "disable", nil)
		approve(t, h, id, ana)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApplied {
			t.Errorf("%s, an alert of it long begun: %s %q", rule, p.State, p.Reason)
		}
	}
	for _, x := range []struct {
		rule      string
		stateless bool
		window    time.Duration
	}{{"deletes", false, 5 * time.Minute}, {"checkout-errors", true, time.Minute}} {
		z, ids, s := server(x.stateless)
		at(z, ids[x.rule], 20*24*time.Hour)
		goes(s, x.rule)
		z, ids, s = server(x.stateless)
		at(z, ids[x.rule], x.window/2)
		waits(t, s, x.rule, func() { at(z, ids[x.rule], x.window+time.Second) })
	}
	// A rule that resolves itself waits while its alert is Fired, however
	// long ago it began.
	z, ids, s := server(false)
	at(z, ids["checkout-errors"], 20*24*time.Hour)
	waits(t, s, "checkout-errors", func() {
		z.Lock()
		z.Alerts[0].Condition = "Resolved"
		z.Unlock()
	})
}

// Whether an alarm fires, asked of a cloud that throttles or fails a
// while, isn't known yet: turning it off waits, uncounted, and is carried
// out once the cloud answers. Any other failure fails it.
func TestAlarmFiresUnknownYet(t *testing.T) {
	ctx := context.Background()
	for _, x := range []struct {
		code int
		want string
	}{{http.StatusTooManyRequests, ledger.StateApproved}, {http.StatusServiceUnavailable, ledger.StateApproved}, {http.StatusForbidden, ledger.StateFailed}} {
		g, _ := googlePolicies()
		cloudErrs(t, &g.Common)
		failing := true
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if failing && strings.HasSuffix(r.URL.Path, "/alerts") {
				w.WriteHeader(x.code)
				w.Write([]byte(`{"error":{"code":` + itoa(int64(x.code)) + `}}`))
				return
			}
			g.ServeHTTP(w, r)
		})
		s := alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", h, []string{"gcp:*"}, nil)
		id := proposeRule(t, s.Handler(), "Down", "disable", nil)
		approve(t, s.Handler(), id, ana)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != x.want || len(g.Took()) != 0 {
			t.Errorf("alerts.list answering %d: %s %q %v", x.code, p.State, p.Reason, g.Took())
		}
		if n, _ := s.l.Carried(ctx, now.Add(-24*time.Hour), ""); x.want == ledger.StateApproved && n != 0 {
			t.Errorf("counted while it waits: %d", n)
		}
		failing = false
		s.jobs(ctx, false)
		if p := state(t, s, id); x.want == ledger.StateApproved && p.State != ledger.StateApplied {
			t.Errorf("answered again after %d: %s %q", x.code, p.State, p.Reason)
		}
	}
}

// An alarm whose severity changed has its history in two recorded rules,
// one at each severity. Tied at its severity now, it isn't retired as
// silent while it fired at the other within twelve weeks: its record isn't
// one rule's. Once that is more than twelve weeks ago, it is.
func TestAlarmRecordSeverityApart(t *testing.T) {
	for _, x := range []struct {
		warning time.Duration
		ok      bool
	}{{6 * weekLen, false}, {13 * weekLen, true}} {
		o, oids := ociAlarms(t)
		cloudErrs(t, &o.Common)
		o.Alarms[oids["cpu"]]["severity"] = "CRITICAL"
		s := alarmServer(t, "oci", "https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..shop", o,
			[]string{"oci:*"}, nil)
		fired := func(sev string, ago time.Duration) {
			at := now.Add(-ago)
			copyFirings(t, s, "oci", rfc(at.Add(-time.Hour)), rfc(at.Add(time.Hour)), `{"rule":"cpu","labels":{"severity":"`+sev+`","alarm_id":"`+oids["cpu"]+
				`"},"start":"`+rfc(at)+`","end":"`+rfc(at.Add(10*time.Minute))+`"}`+"\n")
		}
		fired("critical", 20*weekLen)
		fired("warning", x.warning)
		copyFirings(t, s, "oci", rfc(now.Add(-time.Hour)), rfc(now), "")
		code, body, _ := ui(t, s.Handler(), http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"cpu"}, "kind": {"disable"}, "from": {"silent"}})
		if x.ok && code != http.StatusSeeOther || !x.ok && (code != http.StatusBadRequest || !strings.Contains(body, "at another severity")) {
			t.Errorf("fired at warning %v ago: %d %s", x.warning, code, zzMsg(body))
		}
	}
}
