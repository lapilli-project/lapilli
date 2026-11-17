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
