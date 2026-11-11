package serve

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// Changing a cloud's own alarms through its API (docs/design/alarms.md),
// each cloud against its stand-in: CloudWatch as botocore's cloudwatch
// model has it, Azure as its REST specifications have it, Google Cloud as
// its v3 reference has it, OCI as its Go SDK's monitoring package has it.

const awsWriter = "arn:aws:iam::111122223333:role/lapilli-alarms"

// alarmServer is a server reading the rule file and a cloud's alarms
// through a stand-in, read again each time, with changes on where in
// names, and the cloud's write identity.
func alarmServer(t *testing.T, src, raw string, h http.Handler, in []string, more func(*Config)) *Server {
	t.Helper()
	return alarmsServer(t, src, raw, h, func(c *Config) {
		c.ChangesIn, c.AlarmsEvery = in, -1
		c.ChangesAWSRole, c.ChangesAzureClientID = awsWriter, "11111111-2222-3333-4444-555555555555"
		c.ChangesGoogleServiceAccount, c.ChangesOCIConfig = "lapilli@shop.iam.gserviceaccount.com", "/etc/lapilli/oci/config"
		if more != nil {
			more(c)
		}
	})
}

// cwPromQL is a PromQL alarm, as CloudWatch keeps one.
func cwPromQL(name, query string) map[string]any {
	return map[string]any{"AlarmName": name, "EvaluationInterval": 60, "AlarmDescription": "Errors of the shop.",
		"AlarmActions":       []any{"arn:aws:sns:eu-west-1:111122223333:oncall"},
		"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": query, "PendingPeriod": 300, "RecoveryPeriod": 600}}}
}

func cloudWatchAlarms() *alarmtest.CloudWatch {
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Writer, cw.Reader = "as "+awsWriter, "pod"
	cw.Add(cwPromQL("checkout-5xx", "errors > 5"), "OK", map[string]string{"team": "shop"})
	cw.Add(cwPromQL("latency", "latency > 1"), "OK", map[string]string{"team": "shop"})
	cw.Add(map[string]any{"AlarmName": "cpu-high", "Namespace": "AWS/EC2", "MetricName": "CPUUtilization", "Statistic": "Average",
		"Period": 300, "EvaluationPeriods": 3, "Threshold": 90, "ComparisonOperator": "GreaterThanThreshold",
		"AlarmActions":     []any{"arn:aws:sns:eu-west-1:111122223333:oncall"},
		"AlarmDescription": "CPU of the shop.\nRunbook: https://runbooks.example.com/old"}, "OK", nil)
	scale := cwPromQL("scale-out", "load > 5")
	scale["AlarmActions"] = []any{"arn:aws:autoscaling:eu-west-1:111122223333:scalingPolicy:x:autoScalingGroupName/web:policyName/up"}
	cw.Add(scale, "OK", nil)
	warm := cwPromQL("warm", "warm > 5")
	warm["WarmUpConfiguration"] = map[string]any{"WarmUpPeriodDurationInMinutes": 10}
	cw.Add(warm, "OK", nil)
	odd := cwPromQL("odd", "odd > 5")
	odd["ShinyNewField"] = true
	cw.Add(odd, "OK", nil)
	cw.Add(cwPromQL("stacked", "stacked > 5"), "OK", map[string]string{"aws:cloudformation:stack-id": "arn:aws:cloudformation:eu-west-1:111122223333:stack/s/1"})
	cw.Add(cwPromQL("unnamed", "unnamed > 5"), "OK", nil)
	cw.AddComposite("shop-down", "ALARM(checkout-5xx) AND ALARM(cpu-high)")
	return cw
}

const cwURL = "https://monitoring.eu-west-1.amazonaws.com"

var cwIn = []string{"cw:checkout-*", "cw:latency", "cw:cpu-*", "cw:scale-*", "cw:warm", "cw:odd", "cw:stacked", "cw:shop-*"}

func cwServer(t *testing.T, cw *alarmtest.CloudWatch, more func(*Config)) *Server {
	t.Helper()
	s := alarmServer(t, "cw", cwURL, cw, cwIn, more)
	t.Cleanup(func() {
		for _, e := range cw.Errs {
			t.Error(e)
		}
	})
	return s
}

func cwQuery(a map[string]any) map[string]any {
	crit, _ := a["EvaluationCriteria"].(map[string]any)
	pq, _ := crit["PromQLCriteria"].(map[string]any)
	return pq
}

// A PromQL alarm's for: proposed with the whole alarm as it would be
// written, approved, sent as PutMetricAlarm as the write role, read back;
// undone, the for put back as it was and a person's change since to the
// alarm's actions kept. A threshold, and what an alarm that isn't PromQL
// says, alike.
func TestAlarmCloudWatch(t *testing.T) {
	ctx := context.Background()
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	h := s.Handler()
	id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
	p := state(t, s, id)
	ev := proposalEvidence(p)
	if p.Path != ledger.PathAlarm || ev.Backtest == nil || p.Source != cw.ARN("checkout-5xx") || ev.Cloud != "cloudwatch" {
		t.Fatalf("the proposal: %s %q %+v", p.Path, ev.NoPath, p)
	}
	if !strings.Contains(p.Diff, `"PendingPeriod": 900,`) || changedLines(p.Diff) != 2 {
		t.Errorf("the diff:\n%s", p.Diff)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); !strings.Contains(body, "A cloud&#39;s alarm, read with --alarms cw:") {
		t.Error("the page doesn't say it is a cloud's alarm")
	}
	if len(cw.Took()) != 0 {
		t.Fatalf("written before approval: %v", cw.Took())
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	p = state(t, s, id)
	a := cw.Alarm("checkout-5xx")
	if p.State != ledger.StateApplied || !slices.Equal(cw.Took(), []string{"PutMetricAlarm checkout-5xx"}) || cwQuery(a)["PendingPeriod"] != json.Number("900") {
		t.Fatalf("applied: %s %q, written %v, %v", p.State, p.Reason, cw.Took(), a)
	}
	if a["AlarmDescription"] != "Errors of the shop." || len(a["AlarmActions"].([]any)) != 1 || cw.Tags[cw.ARN("checkout-5xx")]["team"] != "shop" {
		t.Errorf("the rest of the alarm, rewritten: %v", a)
	}
	if !strings.Contains(p.Backup, `"PendingPeriod": 300`) || !strings.Contains(p.Backup, `"team"`) {
		t.Errorf("what it was: %s", p.Backup)
	}
	// Someone adds an action; the change is undone, and theirs stays.
	cw.Lock()
	cw.Metric["checkout-5xx"]["AlarmActions"] = []any{"arn:aws:sns:eu-west-1:111122223333:oncall", "arn:aws:sns:eu-west-1:111122223333:chat"}
	cw.Unlock()
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	undo := locID(hd)
	s.jobs(ctx, false)
	a = cw.Alarm("checkout-5xx")
	if u := state(t, s, undo); u.State != ledger.StateApplied || state(t, s, id).State != ledger.StateUndone ||
		cwQuery(a)["PendingPeriod"] != json.Number("300") || len(a["AlarmActions"].([]any)) != 2 {
		t.Fatalf("undone: %s %q, %v", u.State, u.Reason, a)
	}
	// A threshold.
	id = proposeRule(t, h, "checkout-5xx", "threshold", url.Values{"bound": {"0"}, "value": {"7"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || cwQuery(cw.Alarm("checkout-5xx"))["Query"] != "errors > 7" {
		t.Errorf("a threshold: %s %q %v", p.State, p.Reason, cw.Alarm("checkout-5xx"))
	}
	// A runbook for an alarm that isn't PromQL: the first link in its
	// description, replaced.
	id = proposeRule(t, h, "cpu-high", "says", url.Values{"annotation": {"runbook_url"}, "text": {"https://runbooks.example.com/cpu"}})
	if p := state(t, s, id); p.Path != ledger.PathAlarm || !strings.Contains(p.Diff, `Runbook: https://runbooks.example.com/cpu`) {
		t.Fatalf("a runbook: %s %q\n%s", p.Path, proposalEvidence(p).NoPath, p.Diff)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || cw.Alarm("cpu-high")["AlarmDescription"] != "CPU of the shop.\nRunbook: https://runbooks.example.com/cpu" {
		t.Errorf("a runbook applied: %s %q %v", p.State, p.Reason, cw.Alarm("cpu-high")["AlarmDescription"])
	}
	// A summary is an alarm's name.
	id = proposeRule(t, h, "cpu-high", "says", url.Values{"annotation": {"summary"}, "text": {"CPU high"}})
	if p := state(t, s, id); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathSummary {
		t.Errorf("a summary: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
}

// What CloudWatch's alarms allow: one whose actions do more than tell
// isn't changed at all; one that warms up isn't rewritten but is turned
// off; a composite alarm is turned off and on alone; one with a field this
// Lapilli doesn't know isn't rewritten; CloudFormation's own, one not
// named in --changes-in and one read from a file are for a person.
func TestAlarmCloudWatchNotEvery(t *testing.T) {
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	h := s.Handler()
	for _, c := range []struct {
		rule, kind string
		more       url.Values
		path, why  string
	}{
		{"scale-out", "for", url.Values{"for": {"15m"}}, ledger.PathNone, noPathAutomation},
		{"warm", "for", url.Values{"for": {"15m"}}, ledger.PathNone, noPathWarmUp},
		{"warm", "disable", nil, ledger.PathAlarm, ""},
		{"shop-down", "says", url.Values{"annotation": {"description"}, "text": {"The shop."}}, ledger.PathNone, noPathComposite},
		{"odd", "for", url.Values{"for": {"15m"}}, ledger.PathNone, noPathUnknown},
		{"stacked", "for", url.Values{"for": {"15m"}}, ledger.PathNone, noPathAlarmCFN},
		{"unnamed", "for", url.Values{"for": {"15m"}}, ledger.PathNone, noPathAlarmIn},
	} {
		id := proposeRule(t, h, c.rule, c.kind, c.more)
		p := state(t, s, id)
		if ev := proposalEvidence(p); p.Path != c.path || ev.NoPath != c.why {
			t.Errorf("%s %s: %s %q, want %s %q", c.rule, c.kind, p.Path, ev.NoPath, c.path, c.why)
		}
		s.l.Move(context.Background(), id, []string{p.State}, ledger.StateDropped, ana, ledger.Step{}) // one at a time
	}
	// What the page says why is in each language.
	for _, x := range alarmNoPaths {
		if _, ok := catalogs["ko"][x]; !ok {
			t.Errorf("ko has no translation for %q", x)
		}
	}
	// Read from a file, a cloud's alarm is for a person.
	f, _ := changesServer(t, func(c *Config) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "cw.json"), []byte(`{"MetricAlarms": [{"AlarmName": "from-file", "EvaluationCriteria": {"PromQLCriteria": {"Query": "errors > 5"}}}]}`), 0o644)
		c.RulesPath = dir
	})
	id := proposeRule(t, f.Handler(), "from-file", "for", url.Values{"for": {"15m"}})
	if p := state(t, f, id); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathAlarmFile {
		t.Errorf("from a file: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
}

// What a CloudWatch alarm's change is checked against once approved: the
// rule changed since, refused as stale; only the rest of the alarm changed
// since, the change made afresh in it; a write landing between the read
// and Lapilli's own, read and checked once more; the source's URL changed
// by a restart, refused.
func TestAlarmCloudWatchChecksAgain(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		set   func(*alarmtest.CloudWatch)
		state string
		why   string
	}{
		"the rule changed": {func(cw *alarmtest.CloudWatch) {
			cwQuery(cw.Metric["checkout-5xx"])["Query"] = "errors > 6"
		}, ledger.StateRefused, "changed in the cloud since"},
		"its actions changed": {func(cw *alarmtest.CloudWatch) {
			cw.Metric["checkout-5xx"]["OKActions"] = []any{"arn:aws:sns:eu-west-1:111122223333:chat"}
		}, ledger.StateApplied, ""},
		"written meanwhile, the rest": {func(cw *alarmtest.CloudWatch) {
			// Between the read and the write: read and checked once more.
			cw.OnDescribe = func(n int) {
				if n == 2 {
					cw.Metric["checkout-5xx"]["OKActions"] = []any{"arn:aws:sns:eu-west-1:111122223333:chat"}
				}
			}
		}, ledger.StateApplied, ""},
		"written meanwhile, twice": {func(cw *alarmtest.CloudWatch) {
			cw.OnDescribe = func(n int) {
				if n == 2 || n == 4 {
					cw.Metric["checkout-5xx"]["OKActions"] = []any{"arn:aws:sns:eu-west-1:111122223333:chat" + string(rune('a'+n))}
				}
			}
		}, ledger.StateRefused, "twice"},
	} {
		t.Run(name, func(t *testing.T) {
			cw := cloudWatchAlarms()
			s := cwServer(t, cw, nil)
			h := s.Handler()
			id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
			approve(t, h, id, ana)
			cw.Lock()
			c.set(cw)
			cw.Unlock()
			s.jobs(ctx, false)
			p := state(t, s, id)
			if p.State != c.state || !strings.Contains(p.Reason, c.why) {
				t.Errorf("%s %q", p.State, p.Reason)
			}
			if c.state == ledger.StateApplied {
				a := cw.Alarm("checkout-5xx")
				if cwQuery(a)["PendingPeriod"] != json.Number("900") || a["OKActions"] == nil {
					t.Errorf("made afresh: %v", a)
				}
			} else if len(cw.Took()) != 0 {
				t.Errorf("written: %v", cw.Took())
			}
		})
	}
	t.Run("elsewhere after a restart", func(t *testing.T) {
		cw := cloudWatchAlarms()
		s := cwServer(t, cw, nil)
		h := s.Handler()
		id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		s.alarms[0].URL = "https://monitoring.us-east-1.amazonaws.com"
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateRefused || len(cw.Took()) != 0 {
			t.Errorf("another region: %s %q %v", p.State, p.Reason, cw.Took())
		}
	})
	t.Run("--changes-in no longer names it", func(t *testing.T) {
		cw := cloudWatchAlarms()
		s := cwServer(t, cw, nil)
		h := s.Handler()
		id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		s.cfg.ChangesIn = []string{"cw:cpu-*"}
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateRefused || p.Reason != noPathAlarmIn || len(cw.Took()) != 0 {
			t.Errorf("not named: %s %q %v", p.State, p.Reason, cw.Took())
		}
	})
}

// After the write: one that can't be read back stays applying, read again
// each round and never sent again; one the cloud took but reads as it was
// is read again for two minutes by the clock, however soon the rounds come,
// then failed; a redirect isn't followed;
// paused just before, nothing is sent until resumed, and it is counted
// once.
func TestAlarmCloudWatchReadBack(t *testing.T) {
	ctx := context.Background()
	t.Run("unreadable", func(t *testing.T) {
		cw := cloudWatchAlarms()
		s := cwServer(t, cw, nil)
		h := s.Handler()
		id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		cw.FailAfterWrite = true
		s.jobs(ctx, false)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApplying || !strings.Contains(p.Reason, "never sent again") || len(cw.Took()) != 1 {
			t.Fatalf("%s %q %v", p.State, p.Reason, cw.Took())
		}
		cw.Lock()
		cw.FailAfterWrite = false
		cw.Unlock()
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApplied || len(cw.Took()) != 1 {
			t.Errorf("read back later: %s %q %v", p.State, p.Reason, cw.Took())
		}
	})
	t.Run("taken, reading as it was", func(t *testing.T) {
		cw := cloudWatchAlarms()
		s := cwServer(t, cw, nil)
		h := s.Handler()
		s.now = time.Now
		id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		cw.Ignore = true
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApplying || p.Reason != readBehind {
			t.Fatalf("at first: %s %q", p.State, p.Reason)
		}
		if err := s.carryOut(ctx); err != nil { // a round at once
			t.Fatal(err)
		}
		if p := state(t, s, id); p.State != ledger.StateApplying {
			t.Fatalf("read again at once: %s %q", p.State, p.Reason)
		}
		s.now = func() time.Time { return time.Now().Add(readBackWithin + time.Minute) }
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateFailed || len(cw.Took()) != 1 {
			t.Errorf("two minutes on: %s %q %v", p.State, p.Reason, cw.Took())
		}
	})
	t.Run("a redirect", func(t *testing.T) {
		cw := cloudWatchAlarms()
		s := cwServer(t, cw, nil)
		h := s.Handler()
		id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		cw.Redirect = "https://evil.example.com/"
		s.jobs(ctx, false)
		tr := s.cfg.RulerTransport.(*alarmtest.Transport)
		if p := state(t, s, id); p.State != ledger.StateFailed || !strings.Contains(p.Reason, "redirect") || tr.Hosts["evil.example.com"] != 0 {
			t.Errorf("%s %q %v", p.State, p.Reason, tr.Hosts)
		}
	})
	t.Run("paused just before", func(t *testing.T) {
		cw := cloudWatchAlarms()
		s := cwServer(t, cw, nil)
		h := s.Handler()
		id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		pause := filepath.Join(t.TempDir(), "paused")
		s.cfg.ChangesPaused = pause
		// The pause comes as the alarm is read once more, just before
		// the write.
		cw.OnDescribe = func(n int) {
			if n == 2 {
				os.WriteFile(pause, nil, 0o644)
			}
		}
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApproved || len(cw.Took()) != 0 {
			t.Fatalf("paused: %s %v", p.State, cw.Took())
		}
		os.Remove(pause)
		s.jobs(ctx, false)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApplied || len(cw.Took()) != 1 {
			t.Errorf("resumed: %s %v", p.State, cw.Took())
		}
		if n, _ := s.l.Carried(ctx, time.Now().Add(-time.Hour), ""); n != 1 {
			t.Errorf("counted %d times", n)
		}
	})
}

// Retiring a CloudWatch alarm. One that isn't PromQL, its history tied to
// it over twelve weeks and silent: turned off with DisableAlarmActions,
// waiting while CloudWatch says it is in ALARM, and no further; undone with
// EnableAlarmActions. A PromQL alarm: off, then deleted four weeks later
// with DeleteAlarms, its replay checked; undone, made again with its tags.
// One a composite alarm watches isn't deleted.
func TestAlarmCloudWatchRetire(t *testing.T) {
	ctx := context.Background()
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	h := s.Handler()
	copyFirings(t, s, "cw", "2026-11-01T00:00:00Z", "2026-11-03T00:00:00Z",
		`{"rule":"cpu-high","labels":{"alarm_id":"`+cw.ARN("cpu-high")+`"},"start":"2026-11-02T06:00:00Z","end":"2026-11-02T06:30:00Z"}`+"\n")
	s.now = func() time.Time { return now.Add(13 * weekLen) }
	id := proposeRule(t, h, "cpu-high", "disable", url.Values{"from": {"silent"}})
	p := state(t, s, id)
	if ev := proposalEvidence(p); p.Path != ledger.PathAlarm || !ev.OffOnly || ev.Record == nil {
		t.Fatalf("turning it off: %s %+v", p.Path, ev)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); !strings.Contains(body, "retiring it stops here, at off") {
		t.Error("the page doesn't say retiring it stops at off")
	}
	approve(t, h, id, ana)
	cw.Lock()
	cw.Metric["cpu-high"]["StateValue"] = "ALARM"
	cw.Unlock()
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApproved || p.Reason != firesNote || len(cw.Took()) != 0 {
		t.Fatalf("while in ALARM: %s %q %v", p.State, p.Reason, cw.Took())
	}
	cw.Lock()
	cw.Metric["cpu-high"]["StateValue"] = "OK"
	cw.Unlock()
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || !slices.Equal(cw.Took(), []string{"DisableAlarmActions cpu-high"}) ||
		cw.Alarm("cpu-high")["ActionsEnabled"] != false {
		t.Fatalf("turned off: %s %q %v", p.State, p.Reason, cw.Took())
	}
	// Four weeks on, it isn't deleted: Lapilli doesn't replay it.
	raw := ""
	s.look(ctx, &raw)
	s.now = func() time.Time { return now.Add(18 * weekLen) }
	if err := s.proposeFound(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "cpu-high", States: []string{ledger.StateProposed}}); len(ps) != 0 {
		t.Errorf("deleting it: %+v", ps)
	}
	// Undone: on again.
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	s.jobs(ctx, false)
	if u := state(t, s, locID(hd)); u.State != ledger.StateApplied || cw.Alarm("cpu-high")["ActionsEnabled"] != true ||
		cw.Took()[len(cw.Took())-1] != "EnableAlarmActions cpu-high" {
		t.Errorf("undone: %s %q %v", u.State, u.Reason, cw.Took())
	}

	// A PromQL alarm, which never fired: Lapilli proposed turning it off,
	// then deletes it.
	lat, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "latency", States: []string{ledger.StateProposed}})
	if len(lat) != 1 || lat[0].Kind != ledger.KindDisable || lat[0].Path != ledger.PathAlarm || proposalEvidence(lat[0]).OffOnly {
		t.Fatalf("turning latency off: %+v", lat)
	}
	off := lat[0].ID
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if p := state(t, s, off); p.State != ledger.StateApplied {
		t.Fatalf("latency off: %s %q", p.State, p.Reason)
	}
	s.look(ctx, &raw)
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	if err := s.proposeFound(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "latency", States: []string{ledger.StateProposed}})
	if len(ps) != 1 || ps[0].Kind != ledger.KindDelete || ps[0].Path != ledger.PathAlarm || ps[0].Follows != off {
		t.Fatalf("deleting latency: %+v", ps)
	}
	approve(t, h, ps[0].ID, cy)
	s.jobs(ctx, false)
	if p := state(t, s, ps[0].ID); p.State != ledger.StateApplied || cw.Alarm("latency") != nil {
		t.Fatalf("deleted: %s %q %v", p.State, p.Reason, cw.Took())
	}
	key = keyFor(t, h, ps[0].ID, ana, "undo")
	_, _, hd = ui(t, h, http.MethodPost, "/proposals/"+itoa(ps[0].ID)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	s.jobs(ctx, false)
	if u := state(t, s, locID(hd)); u.State != ledger.StateApplied || cw.Alarm("latency") == nil || cw.Tags[cw.ARN("latency")]["team"] != "shop" {
		t.Errorf("made again: %s %q %v %v", u.State, u.Reason, cw.Alarm("latency"), cw.Tags[cw.ARN("latency")])
	}
	// One a composite alarm watches isn't deleted.
	if why := (alarmTarget{s}).offered(ctx, mustUnit(t, s, "cw", cw.ARN("checkout-5xx")), ledgerRule(t, s, "checkout-5xx"), ledger.KindDelete); why != noPathWatched {
		t.Errorf("watched: %q", why)
	}
}

func mustUnit(t *testing.T, s *Server, src, id string) apiUnit {
	t.Helper()
	u, err := (alarmTarget{s}).unit(context.Background(), id, ruleEdit(src))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func ledgerRule(t *testing.T, s *Server, name string) rules.Rule {
	t.Helper()
	f, err := s.current(context.Background(), name, false)
	if err != nil {
		t.Fatal(err)
	}
	return f.rule
}

func ruleEdit(src string) rules.Edit { return rules.Edit{Namespace: src} }

// The fields Lapilli knows of a CloudWatch alarm, and those PutMetricAlarm
// takes, are the model's (botocore's cloudwatch 2010-08-01, kept as test
// data): an upgrade of the model that adds a field fails here, until the
// lists know it.
func TestCloudWatchModel(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "cloudwatch-2010-08-01-alarms.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Metadata struct {
			Protocols    []string `json:"protocols"`
			TargetPrefix string   `json:"targetPrefix"`
			JSONVersion  string   `json:"jsonVersion"`
		} `json:"metadata"`
		Shapes map[string]struct {
			Members map[string]struct{ Shape string } `json:"members"`
			Member  *struct{ Shape string }           `json:"member"`
		} `json:"shapes"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Metadata.Protocols, "json") || m.Metadata.TargetPrefix != "GraniteServiceVersion20100801" || m.Metadata.JSONVersion != "1.0" {
		t.Errorf("the model's protocol: %+v", m.Metadata)
	}
	reach := func(root string) map[string]bool {
		out, seen := map[string]bool{}, map[string]bool{}
		var walk func(string)
		walk = func(n string) {
			if seen[n] {
				return
			}
			seen[n] = true
			sh := m.Shapes[n]
			for k, v := range sh.Members {
				out[k] = true
				walk(v.Shape)
			}
			if sh.Member != nil {
				walk(sh.Member.Shape)
			}
		}
		walk(root)
		return out
	}
	if got := reach("MetricAlarm"); !maps.Equal(got, cloudWatchKnown) {
		t.Errorf("a metric alarm's fields, known %d, the model's %d: %v", len(cloudWatchKnown), len(got), setDiff(got, cloudWatchKnown))
	}
	top := map[string]bool{}
	for k := range m.Shapes["PutMetricAlarmInput"].Members {
		top[k] = true
	}
	if !maps.Equal(top, putMetricAlarmInput) {
		t.Errorf("PutMetricAlarm's input: %v", setDiff(top, putMetricAlarmInput))
	}
}

func setDiff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, "+"+k)
		}
	}
	for k := range b {
		if !a[k] {
			out = append(out, "-"+k)
		}
	}
	slices.Sort(out)
	return out
}

// A proposal is written where it was read: an alarm's ID of another
// region than --alarms's, or none of its form, is neither allowed nor
// owned.
func TestAlarmWrittenWhereRead(t *testing.T) {
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	tg := alarmTarget{s}
	for _, id := range []string{"arn:aws:cloudwatch:us-east-1:111122223333:alarm:checkout-5xx", "checkout-5xx",
		"arn:aws:cloudwatch:eu-west-1:111122223333:alarm:"} {
		if err := tg.owns(id, ruleEdit("cw")); err == nil {
			t.Errorf("%s owned", id)
		}
		if why := tg.allowed(id, ruleEdit("cw")); why != noPathAlarmName {
			t.Errorf("%s allowed: %q", id, why)
		}
	}
	if err := tg.owns(cw.ARN("checkout-5xx"), ruleEdit("other")); err == nil {
		t.Error("owned under another source")
	}
}

// Restarted, before the first read of its alarms: an approved change is
// carried out, --changes-in asked of the alarm as read just before; and
// an alarm renamed out of --changes-in since it was proposed is refused.
func TestAlarmNamedAsReadNow(t *testing.T) {
	ctx := context.Background()
	cw := cloudWatchAlarms()
	s := cwServer(t, cw, nil)
	h := s.Handler()
	id := proposeRule(t, h, "checkout-5xx", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	fresh, err := source.NewAlarms("cw", cwURL, "lapilli/test")
	if err != nil {
		t.Fatal(err)
	}
	fresh.Transport, fresh.Reads, fresh.Now = s.alarms[0].Transport, s.alarms[0].Reads, s.alarms[0].Now
	s.alarms[0] = fresh // nothing read yet
	if err := s.carryOut(ctx); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, id); p.State != ledger.StateApplied {
		t.Errorf("after a restart: %s %q", p.State, p.Reason)
	}
	if why := (alarmTarget{s}).offered(ctx, mustUnit(t, s, "cw", cw.ARN("unnamed")), ledgerRule(t, s, "unnamed"), ledger.KindFor); why != noPathAlarmIn {
		t.Errorf("not named: %q", why)
	}
}
