package source

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
)

// The rules Lapilli can't replay, judged from what real Grafanas kept in
// their alert state history (docs/design/grafanajudged.md): every rule the
// recordings ran taken to read a data source the replay doesn't take, and
// judged under a name of its own, its alert name the title Grafana fired
// it under. Each firing is one GRAFANA_ALERTS has, under the rule's name,
// starting and ending at the same evaluation, and none other; a rule the
// history doesn't tell apart, or whose alert name another of Grafana's
// rules has, is judged by none of it.
func TestRealGrafanaJudged(t *testing.T) {
	ctx := context.Background()
	for _, c := range realHistories {
		t.Run(c.dir, func(t *testing.T) {
			h := c.recorded(t)
			where, err := h.Where(ctx)
			if err != nil || where.Serves != c.where {
				t.Fatalf("where: %+v %v", where, err)
			}
			alerts := c.alerts(t)
			alerts = slices.DeleteFunc(alerts, func(f firing.Firing) bool {
				return !f.Start.After(c.read) && (f.End.IsZero() || f.End.After(c.to)) // on all through what was read
			})
			var titles []string
			for _, f := range alerts {
				if !slices.Contains(titles, f.Rule) {
					titles = append(titles, f.Rule)
				}
			}
			slices.Sort(titles)
			named := func(title string) string { return title + " [judged]" }
			var rs []rules.Rule
			for i, title := range titles {
				rs = append(rs, rules.Rule{Name: named(title), Alert: title, UID: "u" + string(rune('a'+i%26)) + title,
					Unsupported: "its condition reads a mysql data source: only PromQL and LogQL are replayed"})
			}
			// One title another of Grafana's rules has too, in another
			// folder: the history doesn't say which of the two fired.
			twin := titles[len(titles)-1]
			all := append(slices.Clone(rs), rules.Rule{Name: twin + " [other]", Alert: twin, UID: "other"})
			j, err := h.Judge(ctx, where.Serves, rs, all, c.read, c.to)
			if err != nil || j.Passed != 0 {
				t.Fatalf("%+v %v", j.Passed, err)
			}
			wantUnsure := []string{named(twin)}
			for _, u := range c.unsure {
				wantUnsure = append(wantUnsure, named(u))
			}
			slices.Sort(wantUnsure)
			if !slices.Equal(j.Unsure, wantUnsure) {
				t.Errorf("unsure: %v, want %v", j.Unsure, wantUnsure)
			}
			// GRAFANA_ALERTS as the history keeps an alert's labels: in
			// annotations, as their tags write them, tags_length too.
			var want []firing.Firing
			for _, f := range alerts {
				if f.Rule == twin || slices.Contains(c.unsure, f.Rule) {
					continue
				}
				l := maps.Clone(f.Labels)
				delete(l, "grafana_rule_uid")
				if c.where == HistoryAnnotations {
					l, _ = tagLabels(grafanaTags(l, 500))
				}
				f.Rule, f.Labels = named(f.Rule), l
				want = append(want, f)
			}
			got := within(j.Firings, c.from, c.to)
			wanted := within(want, c.from, c.to)
			var differ []string
			for _, l := range got {
				if !slices.Contains(wanted, l) {
					differ = append(differ, "only judged: "+l)
				}
			}
			for _, l := range wanted {
				if !slices.Contains(got, l) {
					differ = append(differ, "only in GRAFANA_ALERTS: "+l)
				}
			}
			if len(differ) > 0 || len(got) == 0 {
				t.Errorf("%d judged, %d in GRAFANA_ALERTS; %d differ:\n%s", len(got), len(wanted), len(differ), strings.Join(differ, "\n"))
			} else {
				t.Logf("%d firings of %d rules judged, every one as GRAFANA_ALERTS has it", len(got), len(titles)-len(wantUnsure))
			}
			for _, f := range j.Firings {
				if !strings.HasSuffix(f.Rule, " [judged]") || f.Rule == named(twin) {
					t.Fatalf("judged under %q", f.Rule)
				}
			}
		})
	}
}

// A read past the history's bounds judges nothing, as it checks nothing:
// it fails, counted by its changes, as too many.
func TestGrafanaJudgedBounds(t *testing.T) {
	c := realHistories[0]
	h := c.recorded(t)
	was := MaxChanges
	t.Cleanup(func() { MaxChanges = was })
	MaxChanges = 100
	rs := []rules.Rule{{Name: "Many pods", UID: "u", Unsupported: "its condition reads a mysql data source: only PromQL and LogQL are replayed"}}
	j, err := h.Judge(context.Background(), HistoryAnnotations, rs, nil, c.read, c.to)
	if !errors.Is(err, ErrTooManyChanges) || !errors.Is(err, ErrUnusable) || len(j.Firings) != 0 {
		t.Fatalf("%d firings, %v", len(j.Firings), err)
	}
}
