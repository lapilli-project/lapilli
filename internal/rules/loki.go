package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// lokiQuery reads one of a Grafana rule's queries on a Loki data source as
// Grafana 13.3's Loki data source asks it, as measured through its
// /api/v1/eval and Loki's log of what it was asked (docs/design/loki.md):
// an instant query at the end of its window, or a range query over its
// window, every step, both ends in; with its macros put in. span is the
// window, relativeTimeRange's from less its to.
func lokiQuery(ref string, q dataQuery, dm, model map[string]any, span time.Duration) (dataQuery, error) {
	// Grafana's Loki data source reads the model's queryType alone: one
	// without it is a range query, as alert queries stored before it had
	// one were, whatever an instant: true beside it says.
	instant := str(model["queryType"]) == "instant"
	q.res = lokiStep(model, span)
	q.window = 0
	if !instant {
		q.window = span
	}
	q.expr = lokiMacros(q.expr, instant, q.res, intervalMs(model), span)
	if LogQuery(q.expr) {
		return q, fmt.Errorf("query %s is a log query, which returns log lines, not numbers: only a metric query, such as count_over_time, is replayed", ref)
	}
	return q, nil
}

// LogQuery reports whether a LogQL query is a log query, a stream selector
// and its pipeline, which returns log lines: every metric query starts
// with a function, an aggregation, a number or a bracket.
func LogQuery(expr string) bool { return strings.HasPrefix(strings.TrimSpace(expr), "{") }

// lokiMaxPoints is the most points Grafana's Loki data source asks for a
// range: a longer range is asked at a longer step.
const lokiMaxPoints = 11000

// LokiLeastStep is the finest step a replay asks Loki for a Grafana
// reduce's points at: a range query Grafana asks at a finer step (its own
// step, or intervalMs, of a millisecond) isn't replayed. Grafana asks
// such a query for one window an evaluation, which Loki allows (11,000
// points); a replay asks for every window of the period, and at a
// millisecond that is 86.4 million points a series a day.
const LokiLeastStep = time.Second

// lokiStep is how far apart Grafana's Loki data source asks for a range
// query's points over span, as Grafana's calculateStep works it out
// (pkg/tsdb/loki, step.go): the query's own step (15s, or seconds alone),
// times its resolution; or else its intervalMs, 1s when it has none, times
// its resolution, and at least span over 11,000, rounded up to the
// millisecond. A resolution is 1 to 5, or 10; any other is 1.
// maxDataPoints doesn't come into it. Measured on Grafana 13.3.1: 1s for
// ten minutes, 1.964s for six hours, 2s at resolution 2, 5s at intervalMs
// 5000, 15s for step 15s, 30s for step 15s at resolution 2, 7s for step 7.
func lokiStep(model map[string]any, span time.Duration) time.Duration {
	res := int64(1)
	if n, ok := toFloat(model["resolution"]); ok && n == math.Trunc(n) && (n >= 1 && n <= 5 || n == 10) {
		res = int64(n)
	}
	if s := lokiOwnStep(model["step"]); s > 0 {
		return time.Duration(int64(s) * res)
	}
	least := time.Duration(math.Ceil(float64(span/time.Millisecond)/lokiMaxPoints)) * time.Millisecond
	return max(time.Duration(int64(intervalMs(model))*res), least)
}

// lokiOwnStep is a query's own step, as a duration (15s) or seconds alone
// (7, "7"); 0 when it has none that reads.
func lokiOwnStep(v any) time.Duration {
	switch x := v.(type) {
	case float64, int, int64, json.Number:
		if f, ok := toFloat(x); ok && f > 0 && f < 1e6 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		x = strings.TrimSpace(x)
		if f, err := strconv.ParseFloat(x, 64); err == nil && f > 0 && f < 1e6 {
			return time.Duration(f * float64(time.Second))
		}
		if d, err := duration(x); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

// lokiMacros puts in the variables Grafana's Loki data source puts in a
// query (pkg/tsdb/loki, interpolateVariables): $__interval and
// $__interval_ms are the query's interval (its intervalMs, 1s when it has
// none), not its step; $__auto is the step of a range query and the window
// of an instant one; $__range, $__range_s and $__range_ms the window.
// Measured on Grafana 13.3.1: a range query with step 10s counted
// [$__interval] over 1s. Loki's data source has no $__rate_interval.
func lokiMacros(expr string, instant bool, step, interval, span time.Duration) string {
	if !strings.Contains(expr, "$") {
		return expr
	}
	secs := int64(math.Round(float64(span/time.Millisecond) / 1000))
	rangeText := strconv.FormatInt(secs, 10) + "s"
	auto := grafanaInterval(step)
	if instant {
		auto = rangeText
	}
	ms := func(d time.Duration) string { return strconv.FormatInt(int64(d/time.Millisecond), 10) }
	// The longer names first: $__interval would take $__interval_ms's
	// start.
	for _, v := range []struct{ name, value string }{
		{"__interval_ms", ms(interval)}, {"__range_ms", ms(span)}, {"__range_s", strconv.FormatInt(secs, 10)},
		{"__interval", grafanaInterval(interval)}, {"__range", rangeText}, {"__auto", auto},
	} {
		expr = strings.ReplaceAll(expr, "${"+v.name+"}", v.value)
		expr = strings.ReplaceAll(expr, "$"+v.name, v.value)
	}
	return expr
}

// Places reads where a Loki ruler runs each of its groups within its
// interval, from its Prometheus-shaped rules API's answer
// (/prometheus/api/v1/rules): each group's last evaluation, to the second
// below, as FromAPI reads a Prometheus's, by the namespace Loki gives as
// the group's file and its name. A group not yet run isn't among them.
func Places(b []byte) (map[Place]time.Duration, error) {
	var r struct {
		Status string `json:"status"`
		Data   struct {
			Groups []struct {
				Name           string    `json:"name"`
				File           string    `json:"file"`
				Interval       float64   `json:"interval"`
				LastEvaluation time.Time `json:"lastEvaluation"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("not a rules API answer: %w", err)
	}
	if r.Status != "success" {
		return nil, errors.New("the rules API didn't answer with success")
	}
	out := map[Place]time.Duration{}
	for _, g := range r.Data.Groups {
		if !(g.Interval >= 1 && g.Interval < never.Seconds()) || g.LastEvaluation.Year() <= 1970 {
			continue
		}
		iv := int64(g.Interval)
		out[Place{g.File, g.Name}] = time.Duration((g.LastEvaluation.Unix()%iv+iv)%iv) * time.Second
	}
	return out, nil
}
