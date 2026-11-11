package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/alarmtest"
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
	s := alarmsServer(t, "cloudwatch-prod", "https://monitoring.eu-west-1.amazonaws.com", cw, nil)
	h := s.Handler()
	copyFirings(t, s, "cloudwatch-prod", "2026-11-01T00:00:00Z", "2026-11-03T00:00:00Z",
		`{"rule":"cpu-high","labels":{"alarm_id":"`+cw.ARN("cpu-high")+`","service":"checkout"},"start":"2026-11-02T06:00:00Z","end":"2026-11-02T06:30:00Z"}`+"\n")
	s.jobs(ctx, false)
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: now.Add(-7 * 24 * time.Hour), To: now, Rule: "cpu-high"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("the alarm's firings: %+v %v", fs, err)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/rules/cpu-high", bo, "", nil); !strings.Contains(body, "its history, copied in under cloudwatch-prod from 2026-11-02 on, is its record") {
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
