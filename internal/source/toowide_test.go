package source

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// wideDays is held, its ranges held to 1,000 samples (--max-samples
// 1000): a range of a day or more answered with 100 series more, so a
// rule's labels on them are too many to replay, a window's not.
type wideDays struct{ *held }

func (wideDays) MaxSamples() int { return 1000 }

func (w wideDays) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	ss, err := w.held.QueryRange(ctx, expr, start, end, step)
	if end.Sub(start) >= 12*time.Hour {
		for i := range 100 {
			ss = append(ss, prom.Series{Labels: map[string]string{"pod": strconv.Itoa(i)}, Samples: []prom.Sample{{T: start, V: 1}}})
		}
	}
	return ss, err
}

// labelled is a rule of n labels of its own beside service.
func labelled(r rules.Rule, n int) rules.Rule {
	r.Labels = map[string]string{"service": "checkout"}
	for i := range n {
		r.Labels[fmt.Sprintf("l%03d", i)] = "v"
	}
	return r
}

// TestTooWideNotPassedOver: a replay that gave no answer as its rule's
// labels on its series were too many is an error where a near miss's cost
// or an unrecorded firing's clearance is worked out, as one whose query
// failed is: it was taken for a rule that can't be replayed, and passed
// over unsaid.
func TestTooWideNotPassedOver(t *testing.T) {
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	rs := []rules.Rule{labelled(rules.Rule{Name: "Errors", Expr: "errors", For: 5 * time.Minute}, 300)}
	incs := []incident.Incident{{ID: "INC-1", Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + 30*time.Minute), Labels: incident.Labels{"service": {"checkout"}}}}
	versions := []replay.Version{{Rules: rs}}
	h := &held{}
	_, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 4)
	rep := verdict.Judge(rs, fs, incs, o)
	if _, _, err := NearMisses(context.Background(), wideDays{h}, versions, rep, incs, o, time.Minute, 4, nil); err == nil || !strings.Contains(err.Error(), rules.TooWide) {
		t.Errorf("a near miss's cost: %v", err)
	}

	bounded := []replay.Version{{Rules: []rules.Rule{labelled(rules.Rule{Name: "Errors", Expr: "errors > 5"}, 300)}}}
	f := firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: tue.Add(10 * time.Hour), End: tue.Add(11 * time.Hour)}
	if _, _, err := clearOf(context.Background(), wideDays{h}, bounded, "Errors", []firing.Firing{f}, o.From, o.To, time.Minute); err == nil || !strings.Contains(err.Error(), rules.TooWide) {
		t.Errorf("an unrecorded firing's clearance: %v", err)
	}
}
