package rules

import "testing"

// A selector covers another that only narrows it: the same metric, offset
// and @, and each of its matchers among the other's, written any way.
func TestCovers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{`http_requests_total`, `http_requests_total{code=~"5.."}`, true},
		{`http_requests_total{service="api"}`, `http_requests_total{code=~"5..", service = "api"}`, true},
		{`http_requests_total{service="api"}`, `http_requests_total{service="web",code=~"5.."}`, false},
		{`http_requests_total{code=~"5.."}`, `http_requests_total`, false},
		{`http_requests_total`, `http_requests_total`, true},
		{`http_requests_total`, `grpc_requests_total{code="5"}`, false},
		{`http_requests_total offset 1w`, `http_requests_total{code="5"} offset 1w`, true},
		{`http_requests_total`, `http_requests_total{code="5"} offset 1w`, false},
		{`http_requests_total @ 1700000000`, `http_requests_total{code="5"}  @  1700000000`, true},
		{`{__name__="up"}`, `up{job="x"}`, true},
		{`{"http.requests"}`, `{"http.requests", "status.code"="500"}`, true},
		{`{job="x"}`, `up{job="x"}`, true},
		{`up{job="a,b"}`, `up{job="a,b",code="5"}`, true},
		{`up{job="a}"}`, `up{job="a}",code="5"}`, true},
		{`up{job=`, `up{job="x"}`, false},
		{`up{job="x"}`, `up{job=`, false},
		{`up`, `up{}`, true},
	} {
		if got := Covers(c.a, c.b); got != c.want {
			t.Errorf("Covers(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// A selector that only narrows another stands in by that one's series with
// the labels it narrows them by; not one an absent() reads.
func TestStandIns(t *testing.T) {
	for _, c := range []struct {
		expr string
		want []StandIn
	}{
		{`sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.05`,
			[]StandIn{{`http_requests_total{code!=""}`, `http_requests_total{code=~"5.."}`}}},
		{`sum(rate(x{service="api",code=~"5.."}[5m])) / sum(rate(x{service="api"}[5m]))`,
			[]StandIn{{`x{service="api",code!=""}`, `x{service="api",code=~"5.."}`}}},
		{`x{code!="200"} / x`, []StandIn{{`x`, `x{code!="200"}`}}},
		{`x{code=~".*"} / x`, []StandIn{{`x`, `x{code=~".*"}`}}},
		{`x{code!~"5.."} offset 1w / x offset 1w`, []StandIn{{`x offset 1w`, `x{code!~"5.."} offset 1w`}}},
		{`absent(up{job="new"}) and on() up`, nil},
		{`absent_over_time(up{job="new"}[5m]) or up{job="new"} == 0 and on() count(up) > 0`, nil},
		{`probe_success{job=~"fan-.+"} == 0`, nil},
		{`{__name__="x",code="500"} / x`, []StandIn{{`x{code!=""}`, `{__name__="x",code="500"}`}}},
	} {
		got, err := StandIns(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, not %v", c.expr, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %v, not %v", c.expr, got, c.want)
			}
		}
	}
}
