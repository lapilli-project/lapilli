package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// A cloud's alarms read with --alarms (docs/design/alarms.md), each cloud
// against its stand-in in internal/alarmtest.

const firingsTok = "a-firings-token-long-enough"

// alarmsServer is a server reading the rule file and a cloud's alarms
// through a stand-in, with changes on.
func alarmsServer(t *testing.T, src, raw string, h http.Handler, more func(*Config)) *Server {
	t.Helper()
	fakeAuth(t)
	s, _ := changesServer(t, func(c *Config) {
		c.Alarms = []AlarmsAt{{Source: src, URL: raw}}
		c.RulerTransport = &alarmtest.Transport{H: h}
		c.IncidentsListen, c.FiringsToken = "127.0.0.1:0", tokenFile(t, firingsTok)
		if more != nil {
			more(c)
		}
	})
	return s
}

// copyFirings sends a source's firings through the port, as recipes/firings/put.sh
// does.
func copyFirings(t *testing.T, s *Server, src, from, to, body string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/firings/"+src+"?from="+from+"&to="+to, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+firingsTok)
	w := httptest.NewRecorder()
	s.IncidentsHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("copying in: %d %s", w.Code, w.Body.String())
	}
}

func cloudWatchShop() *alarmtest.CloudWatch {
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "cpu-high", "Namespace": "AWS/EC2", "MetricName": "CPUUtilization", "Statistic": "Average",
		"Period": 300, "EvaluationPeriods": 3, "Threshold": 90, "ComparisonOperator": "GreaterThanThreshold",
		"AlarmActions": []any{"arn:aws:sns:eu-west-1:111122223333:oncall"}, "AlarmDescription": "CPU of the shop."}, "OK", nil)
	cw.Add(map[string]any{"AlarmName": "quiet", "Namespace": "AWS/EC2", "MetricName": "StatusCheckFailed", "Statistic": "Maximum",
		"Period": 60, "EvaluationPeriods": 1, "Threshold": 1, "ComparisonOperator": "GreaterThanOrEqualToThreshold"}, "OK", nil)
	return cw
}

// A CloudWatch alarm that isn't PromQL, and its history copied in by its
// recipe, labelled with its ARN, are one rule: judged under the alarm's
// name, its record its history's. Retiring it rests on twelve weeks of that
// history; an alarm with none tied to it has no record, isn't silent, and
// retiring it is refused.
func TestAlarmTiedToHistory(t *testing.T) {
	ctx := context.Background()
	cw := cloudWatchShop()
	// Its copies, stopped thirteen weeks before the end, are taken as up
	// to now (TestAlarmRecordUpToNow).
	s := alarmsServer(t, "cloudwatch-prod", "https://monitoring.eu-west-1.amazonaws.com", cw, func(c *Config) { c.SourceQuiet = 14 * weekLen })
	h := s.Handler()
	copyFirings(t, s, "cloudwatch-prod", "2026-11-01T00:00:00Z", "2026-11-03T00:00:00Z",
		`{"rule":"cpu-high","labels":{"alarm_id":"`+cw.ARN("cpu-high")+`","service":"checkout"},"start":"2026-11-02T06:00:00Z","end":"2026-11-02T06:30:00Z"}`+"\n")
	s.jobs(ctx, false)
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: now.Add(-7 * 24 * time.Hour), To: now, Rule: "cpu-high"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("the alarm's firings: %+v %v", fs, err)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/rules/cpu-high", bo, "", nil); !strings.Contains(body, "its history, copied in under cloudwatch-prod from 2026-11-01 on, is its record") {
		t.Errorf("the alarm's page doesn't say its history is its:\n%s", body)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/rules/quiet", bo, "", nil); !strings.Contains(body, "it has no record, since no history is tied to it") {
		t.Errorf("the untied alarm's page:\n%s", body)
	}
	// Its history reaches back a day: retiring it waits for twelve weeks.
	form := url.Values{"rule": {"cpu-high"}, "kind": {"disable"}}
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", form); code != http.StatusBadRequest || !strings.Contains(body, "less than twelve weeks") {
		t.Errorf("retiring a day's history: %d %s", code, zzMsg(body))
	}
	form.Set("rule", "quiet")
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", form); code != http.StatusBadRequest || !strings.Contains(body, "no history is tied to it") {
		t.Errorf("retiring with no history: %d %s", code, zzMsg(body))
	}
	// Thirteen weeks on, with nothing fired since: the tied alarm never
	// fired over twelve weeks, the untied one is unknown.
	s.now = func() time.Time { return now.Add(13 * weekLen) }
	silent, err := s.silentRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(silent, ","), "cpu-high") || strings.Contains(strings.Join(silent, ","), "quiet") {
		t.Errorf("silent: %q", silent)
	}
	if err := s.proposeFound(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "cpu-high"})
	if len(ps) != 1 || ps[0].Kind != ledger.KindDisable || proposalEvidence(ps[0]).Record == nil {
		t.Errorf("retiring the tied alarm: %+v", ps)
	}
	if ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "quiet"}); len(ps) != 0 {
		t.Errorf("retiring the untied one: %+v", ps)
	}
	// Deleting one Lapilli doesn't replay is never proposed.
	form = url.Values{"rule": {"quiet"}, "kind": {"delete"}}
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", form); code != http.StatusBadRequest || !strings.Contains(body, "goes no further") {
		t.Errorf("deleting an alarm not replayed: %d %s", code, zzMsg(body))
	}
	for _, e := range cw.Errs {
		t.Error(e)
	}
}

// Turning a rule off, or deleting it, waits while it fires, on every path:
// here the git path's deletion and Grafana's turning off, while the ledger
// has an alert of the rule still on. It waits approved, uncounted, saying
// why, and is carried out once the alert ends.
func TestRetireWaitsWhileFiring(t *testing.T) {
	ctx := context.Background()
	fire := func(s *Server, rule string, end time.Time) {
		t.Helper()
		f := ledger.Found{From: now.Add(-time.Hour), To: now, Rules: []ledger.Rule{{Name: rule}},
			Firings: []firing.Firing{{Rule: rule, Start: now.Add(-30 * time.Minute), End: end}},
			Run:     ledger.Run{Kind: "test", Started: now, Finished: now, From: now.Add(-time.Hour), To: now}}
		if err := s.l.Record(ctx, f, s.cfg.Options); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("git", func(t *testing.T) {
		s, dir := changesServer(t, nil)
		os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(ruleFile+"      - alert: Kept\n        expr: up == 0\n"), 0o644)
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		h := s.Handler()
		code, body, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"delete"}})
		if code != http.StatusSeeOther {
			t.Fatalf("%d %s", code, zzMsg(body))
		}
		id := locID(hd)
		approve(t, h, id, ana)
		fire(s, "Errors", time.Time{})
		if err := s.carryOut(ctx); err != nil {
			t.Fatal(err)
		}
		if p := state(t, s, id); p.State != ledger.StateApproved || p.Reason != firesNote {
			t.Fatalf("while it fires: %s %q", p.State, p.Reason)
		}
		if n, _ := s.l.Carried(ctx, now.Add(-24*time.Hour), ""); n != 0 {
			t.Errorf("counted while it waits: %d", n)
		}
		fire(s, "Errors", now.Add(-time.Minute))
		if err := s.carryOut(ctx); err != nil {
			t.Fatal(err)
		}
		if p := state(t, s, id); p.State != ledger.StateReady || !strings.Contains(p.Reason, "stopped firing") {
			t.Errorf("once it stopped: %s %q", p.State, p.Reason)
		}
	})
	t.Run("Grafana", func(t *testing.T) {
		g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
		s := grafanaServer(t, g)
		h := s.Handler()
		code, body, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"disable"}})
		if code != http.StatusSeeOther {
			t.Fatalf("%d %s", code, zzMsg(body))
		}
		id := locID(hd)
		approve(t, h, id, ana)
		fire(s, "Other", time.Time{})
		s.carryOut(ctx)
		if p := state(t, s, id); p.State != ledger.StateApproved || len(g.writes) != 0 {
			t.Fatalf("while it fires: %s, written %v", p.State, g.writes)
		}
		fire(s, "Other", now.Add(-time.Minute))
		s.carryOut(ctx)
		if p := state(t, s, id); p.State != ledger.StateApplied || p.Reason != firedNote {
			t.Errorf("once it stopped: %s %q", p.State, p.Reason)
		}
	})
}
