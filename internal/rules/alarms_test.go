package rules

import (
	"strings"
	"testing"
	"time"
)

// A runbook in a one-description cloud is the first link in its text:
// replaced, or added on a line of its own; taken out, with its line when
// nothing else is on it.
func TestWithRunbook(t *testing.T) {
	for _, c := range []struct {
		text, url string
		unset     bool
		want      string
	}{
		{"CPU.\nRunbook: https://a.example.com/x", "https://b.example.com/y", false, "CPU.\nRunbook: https://b.example.com/y"},
		{"See https://a.example.com/x now", "https://b.example.com/y", false, "See https://b.example.com/y now"},
		{"CPU.", "https://b.example.com/y", false, "CPU.\nhttps://b.example.com/y"},
		{"", "https://b.example.com/y", false, "https://b.example.com/y"},
		{"CPU.\nRunbook: https://a.example.com/x", "", true, "CPU."},
		{"CPU.\nhttps://a.example.com/x\nMore.", "", true, "CPU.\nMore."},
		{"See https://a.example.com/x now", "", true, "See now"},
		{"CPU.", "", true, "CPU."},
	} {
		if got := WithRunbook(c.text, c.url, c.unset); got != c.want {
			t.Errorf("%q, %q, %v: %q, want %q", c.text, c.url, c.unset, got, c.want)
		}
	}
}

// Each cloud's change in its own tree, and put back: the fields it wrote,
// from the alarm as it was, and the rest as it is now.
func TestEditAlarm(t *testing.T) {
	cw := `{"MetricAlarms": [{"AlarmName": "a", "ActionsEnabled": true, "AlarmDescription": "A.", "OKActions": [],
  "EvaluationCriteria": {"PromQLCriteria": {"Query": "errors > 5", "PendingPeriod": 300}}}]}`
	rs, _ := Parse([]byte(cw))
	r := rs[0]
	d := 15 * time.Minute
	out, err := EditAlarm([]byte(cw), r, Edit{For: &d})
	if err != nil || !strings.Contains(string(out), `"PendingPeriod": 900`) {
		t.Fatalf("for: %s %v", out, err)
	}
	if odd := 90*time.Second + time.Millisecond; true {
		if _, err := EditAlarm([]byte(cw), r, Edit{For: &odd}); err == nil {
			t.Error("a for of a part of a second")
		}
	}
	// Someone adds an action; the for is put back, theirs stays.
	now := strings.Replace(string(out), `"OKActions": []`, `"OKActions": ["arn:aws:sns:x:1:t"]`, 1)
	back, err := PutBackAlarm([]byte(now), []byte(cw), "cloudwatch", "", Edit{For: &d})
	if err != nil || !strings.Contains(string(back), `"PendingPeriod": 300`) || !strings.Contains(string(back), "arn:aws:sns:x:1:t") {
		t.Errorf("put back: %s %v", back, err)
	}
	off, _ := EditAlarm([]byte(cw), r, Edit{Pause: true})
	if rs, _ := Parse(off); rs[0].Unsupported != Disabled {
		t.Errorf("off: %s", off)
	}
	if _, err := EditAlarm([]byte(cw), r, Edit{Annotation: "summary", Value: "x"}); err == nil {
		t.Error("a summary in a one-description cloud")
	}
	// Google: a policy of two conditions takes a condition's for, not its
	// text.
	policy := `{"displayName": "P", "conditions": [
  {"name": "c1", "conditionPrometheusQueryLanguage": {"query": "a > 1", "duration": "60s", "alertRule": "A"}},
  {"name": "c2", "conditionPrometheusQueryLanguage": {"query": "b > 1", "duration": "60s", "alertRule": "B"}}]}`
	rs, _ = Parse([]byte(policy))
	for i := range rs {
		rs[i].Cloud = "google"
	}
	out, err = EditAlarm([]byte(policy), rs[1], Edit{For: &d})
	if err != nil || !strings.Contains(string(out), `"duration": "900s"`) || strings.Count(string(out), `"duration": "60s"`) != 1 {
		t.Errorf("a condition's for: %s %v", out, err)
	}
	if _, err := EditAlarm([]byte(policy), rs[1], Edit{Annotation: "summary", Value: "x"}); err == nil {
		t.Error("what a policy of two says")
	}
	back, err = PutBackAlarm(out, []byte(policy), "google", "c2", Edit{For: &d})
	if err != nil || strings.Count(string(back), `"duration": "60s"`) != 2 {
		t.Errorf("a condition put back: %s %v", back, err)
	}
	// The shape of an alarm made again, with a new name, is the one kept.
	again := strings.Replace(strings.Replace(policy, `"c1"`, `"n1"`, 1), `"displayName": "P"`, `"displayName": "P", "name": "projects/p/alertPolicies/9"`, 1)
	a, _ := AlarmShape([]byte(policy), "google")
	b, _ := AlarmShape([]byte(again), "google")
	if a != b || a == "" {
		t.Errorf("made again reads apart")
	}
}
