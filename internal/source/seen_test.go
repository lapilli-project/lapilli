package source

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// countsHistory answers count by (L) (SEL) with each selector's series,
// and fails for the selectors named.
type countsHistory struct {
	by    map[string][]map[string]string
	fail  map[string]bool
	asked []string
}

func (c *countsHistory) QueryRange(_ context.Context, expr string, start, _ time.Time, _ time.Duration) ([]prom.Series, error) {
	c.asked = append(c.asked, expr)
	rest, _ := strings.CutPrefix(expr, "count by (")
	by, sel, _ := strings.Cut(rest, ") (")
	sel = strings.TrimSuffix(sel, ")")
	if c.fail[sel] {
		return nil, errors.New("too many samples")
	}
	var out []prom.Series
	for _, l := range c.by[sel] {
		kept := map[string]string{}
		for _, k := range strings.Split(by, ", ") {
			if v, ok := l[k]; ok {
				kept[k] = v
			}
		}
		out = append(out, prom.Series{Labels: kept, Samples: []prom.Sample{{T: start, V: 1}}})
	}
	return out, nil
}

var seenFrom = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// What a signal reads, label by label: the values its selectors' series
// have; a gate selector without the label adds nothing for it, rather
// than taking the signal to have every value; a label none carries has
// every value; a selector that fails adds nothing, the others' kept.
func TestSeenFrom(t *testing.T) {
	h := &countsHistory{by: map[string][]map[string]string{
		`http_requests_total`: {{"service": "checkout"}, {"service": "search"}},
		`up{job="api"}`:       {{"job": "api", "instance": "a"}},
		`probe_success`:       {{"instance": "https://checkout.example.com"}},
	}}
	gate := `sum by (service) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total[5m])) > 0.05 and on() (sum(up{job="api"}) > 0)`
	seen, complete, err := SeenFrom(context.Background(), h, gate, []string{"service"}, seenFrom, seenFrom.Add(24*time.Hour))
	if err != nil || !complete {
		t.Fatal(complete, err)
	}
	if seen.Ties(map[string]string{"service": "queue"}, []string{"service"}) || !seen.Ties(map[string]string{"service": "search"}, []string{"service"}) {
		b, _ := seen.MarshalJSON()
		t.Errorf("a gate's series without service: seen %s", b)
	}
	// The 5xx selector counted through the requests it narrows, once.
	if slices.ContainsFunc(h.asked, func(q string) bool { return strings.Contains(q, "code") }) {
		t.Errorf("asked %q", h.asked)
	}
	// A label the expression makes: every value.
	relabelled := `label_replace(probe_success, "service", "$1", "instance", "https://(.*)\\..*") == 0`
	if seen, _, _ := SeenFrom(context.Background(), h, relabelled, []string{"service"}, seenFrom, seenFrom.Add(24*time.Hour)); !seen.Ties(map[string]string{"service": "queue"}, []string{"service"}) {
		t.Error("a label none of its selectors carries isn't every value")
	}
	// A name PromQL can't count by: every value, and not asked.
	h.asked = nil
	if seen, _, _ := SeenFrom(context.Background(), h, gate, []string{"service.name"}, seenFrom, seenFrom.Add(24*time.Hour)); !seen.Ties(map[string]string{"service.name": "queue"}, []string{"service.name"}) || len(h.asked) != 0 {
		t.Errorf("a dotted name: asked %q", h.asked)
	}
	// One selector failing: the others' counts kept, the look incomplete,
	// and no label taken to have every value by it.
	h.fail = map[string]bool{`up{job="api"}`: true}
	seen, complete, err = SeenFrom(context.Background(), h, gate, []string{"service"}, seenFrom, seenFrom.Add(24*time.Hour))
	if err == nil || complete || seen == nil || !seen.Ties(map[string]string{"service": "search"}, []string{"service"}) || seen.Ties(map[string]string{"service": "queue"}, []string{"service"}) {
		t.Errorf("a selector failing: complete %v, %v", complete, err)
	}
	h.fail = map[string]bool{`http_requests_total`: true}
	if seen, _, _ = SeenFrom(context.Background(), h, gate, []string{"service"}, seenFrom, seenFrom.Add(24*time.Hour)); seen.Ties(map[string]string{"service": "queue"}, []string{"service"}) {
		t.Error("the one selector carrying service failed, and service was taken to have every value")
	}
}
