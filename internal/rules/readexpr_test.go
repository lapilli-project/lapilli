package rules

import (
	"strings"
	"testing"
)

// An expression reads when its strings end and its brackets match, within
// 16 KiB and 32 levels; what it says isn't evaluated.
func TestReadExpr(t *testing.T) {
	for _, x := range []struct {
		expr string
		ok   bool
	}{
		{`rate(prometheus_rule_evaluation_failures_total[5m]) > 0`, true},
		{`sum by (job) (rate(errors_total{code=~"5.."}[5m])) / sum by (job) (rate(requests_total[5m])) > 0.01`, true},
		{`up{job="a)"} == 0`, true}, // a bracket in a string
		{``, false},
		{`   `, false},
		{`rate(errors_total[5m] > 0`, false},
		{`up{job="a} == 0`, false},
		{`up) == 0 (`, false},
		{strings.Repeat("(", 33) + "up" + strings.Repeat(")", 33), false},
		{strings.Repeat("(", 31) + "up" + strings.Repeat(")", 31), true},
		{"up > " + strings.Repeat("1", 16<<10), false},
	} {
		if err := ReadExpr(x.expr); (err == nil) != x.ok {
			t.Errorf("%.60q: %v, want ok %v", x.expr, err, x.ok)
		}
	}
}
