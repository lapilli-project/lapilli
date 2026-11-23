package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
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
// history doesn't tell apart is judged by none of it. One whose title
// another of Grafana's rules has too, in another folder, is judged all the
// same, by its uid or its id as the history has them; and that other rule,
// judged too, has none of its firings.
func TestRealGrafanaJudged(t *testing.T) {
	ctx := context.Background()
	for _, c := range realHistories {
		t.Run(c.dir, func(t *testing.T) {
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
			uids := map[string]string{}
			for _, f := range alerts {
				uids[f.Rule] = f.Labels["grafana_rule_uid"]
			}
			named := func(title string) string { return title + " [judged]" }
			const unsupported = "its condition reads a mysql data source: only PromQL and LogQL are replayed"
			var rs []rules.Rule
			for _, title := range titles {
				rs = append(rs, rules.Rule{Name: named(title), Alert: title, UID: uids[title], Unsupported: unsupported})
			}
			// One title another of Grafana's rules has too, in another
			// folder, judged as well: it fired nothing.
			twin := titles[len(titles)-1]
			rs = append(rs, rules.Rule{Name: twin + " [other]", Alert: twin, UID: "other", Unsupported: unsupported})
			h := c.recorded(t, listedRule{ID: 1 << 40, UID: "other", Title: twin})
			where, err := h.Where(ctx)
			if err != nil || where.Serves != c.where {
				t.Fatalf("where: %+v %v", where, err)
			}
			j, err := h.Judge(ctx, where.Serves, rs, nil, nil, c.read, c.to)
			if err != nil || j.Passed != 0 {
				t.Fatalf("%+v %v", j.Passed, err)
			}
			var wantUnsure []string
			for _, u := range c.unsure {
				wantUnsure = append(wantUnsure, named(u))
			}
			if !slices.Equal(j.Unsure, wantUnsure) || len(j.Shared) != 0 {
				t.Errorf("unsure: %v, want %v; shared %v", j.Unsure, wantUnsure, j.Shared)
			}
			// GRAFANA_ALERTS as the history keeps an alert's labels: in
			// annotations, as their tags write them, tags_length too.
			var want []firing.Firing
			for _, f := range alerts {
				if slices.Contains(c.unsure, f.Rule) {
					continue
				}
				l := maps.Clone(f.Labels)
				delete(l, "grafana_rule_uid")
				if c.where == HistoryAnnotations {
					// Its alertname among them, as tags_length counts it.
					l["alertname"] = f.Rule
					l, _ = tagLabels(grafanaTags(l, 500))
					delete(l, "alertname")
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
				if !strings.HasSuffix(f.Rule, " [judged]") {
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
	j, err := h.Judge(context.Background(), HistoryAnnotations, rs, nil, nil, c.read, c.to)
	if !errors.Is(err, ErrTooManyChanges) || !errors.Is(err, ErrUnusable) || len(j.Firings) != 0 {
		t.Fatalf("%d firings, %v", len(j.Firings), err)
	}
}

// Kept in Loki, each change carries its rule's uid: a rule of a judged
// rule's title in another folder, read with it or not, has none of its
// changes, and the judged rule isn't shared, even where the other's alert
// has the labels of its own. Where the changes carry no uid, as the
// history then says nothing of which rule fired, a title two rules of
// every version given (ever) have is neither's; one only the judged rule
// has is judged by its name; and one the read holds no change of is the
// judged rule's, which fired nothing.
func TestGrafanaJudgedByUID(t *testing.T) {
	line := func(m int, uid, from, to string) fakeLine {
		l := map[string]any{"schemaVersion": 1, "previous": from, "current": to, "ruleTitle": "Orders slow", "fingerprint": uid + "-orders",
			"labels": map[string]any{"alertname": "Orders slow", "service": "orders"}}
		if uid != "" {
			l["ruleUID"] = uid
		}
		return fakeLine{min_(m).UnixMilli(), uid, l}
	}
	const unsupported = "its condition reads a mysql data source: only PromQL and LogQL are replayed"
	mine := rules.Rule{Name: "Orders slow", UID: "orders", Unsupported: unsupported}
	theirs := rules.Rule{Name: "Orders slow [Elsewhere]", Alert: "Orders slow", UID: "elsewhere"}
	for _, c := range []struct {
		what      string
		uid       string
		all, ever []rules.Rule
		want      string
		shared    bool
		quiet     bool // the read holds no change of the title, but another's
	}{
		{what: "told, the other rule read", uid: "elsewhere", all: []rules.Rule{mine, theirs}, want: "Orders slow map[service:orders] 07:01-07:09"},
		{what: "told, the other rule never read", uid: "elsewhere", all: []rules.Rule{mine}, want: "Orders slow map[service:orders] 07:01-07:09"},
		{what: "untold, the other rule of a version across the read", all: []rules.Rule{mine}, ever: []rules.Rule{theirs}, shared: true},
		{what: "untold, no other rule", all: []rules.Rule{mine}, want: "Orders slow map[service:orders] 07:01-07:09"},
		{what: "untold, no change of it", all: []rules.Rule{mine}, ever: []rules.Rule{theirs}, quiet: true},
	} {
		g := &fakeGrafana{settings: `{"unifiedAlerting": {"alertStateHistoryBackend": "loki"}}`}
		mineUID := ""
		if c.uid != "" {
			mineUID = "orders"
		}
		g.lines = []fakeLine{
			line(1, mineUID, "Normal", "Alerting"),
			line(9, mineUID, "Alerting", "Normal"),
		}
		if c.uid != "" {
			// The other rule's alert, of the same labels, on and off while
			// the judged rule's is on.
			g.lines = append(g.lines, line(3, c.uid, "Normal", "Alerting"), line(4, c.uid, "Alerting", "Normal"))
		}
		if c.quiet {
			for i := range g.lines {
				g.lines[i].line["labels"] = map[string]any{"alertname": "Stock low", "service": "orders"}
				g.lines[i].line["ruleTitle"] = "Stock low"
			}
		}
		h, _ := g.start(t)
		j, err := h.Judge(context.Background(), HistoryLoki, []rules.Rule{mine}, c.all, c.ever, min_(0), min_(10))
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if got := firingsOf(j.Firings); got != c.want || len(j.Unsure) != 0 || (len(j.Shared) == 1) != c.shared {
			t.Errorf("%s: firings %q, unsure %v, shared %v", c.what, got, j.Unsure, j.Shared)
		}
	}
}

// In annotations, each change carries its rule's id, and its title now:
// the judged rule's id is the one Grafana lists its uid with now, asked
// once a read. A change of another id, of a rule since deleted, renamed,
// read with it or in a folder the token doesn't read, is none of the
// judged rule's, whether the judged rule changes state in the read or not.
// A rule Grafana lists no more, lists under another title (renamed since
// the rules were read), or lists twice, is unsure.
func TestGrafanaJudgedByID(t *testing.T) {
	note := func(id int64, m int, alertID int64, title, from, to string) annotation {
		return annotation{ID: id, AlertID: alertID, AlertName: title, PrevState: from, NewState: to, Time: min_(m).UnixMilli(),
			Tags: []string{"alertname:Orders slow", "service:orders"}}
	}
	const unsupported = "its condition reads a mysql data source: only PromQL and LogQL are replayed"
	mine := rules.Rule{Name: "Orders slow", UID: "orders", Unsupported: unsupported}
	theirs := rules.Rule{Name: "Orders slow [Elsewhere]", Alert: "Orders slow", UID: "elsewhere"}
	listed := []listedRule{{7, "orders", "Orders slow"}, {9, "elsewhere", "Orders slow"}}
	const judged = "Orders slow map[service:orders] 07:01-07:09"
	for _, c := range []struct {
		what   string
		quiet  bool   // the judged rule changes state in none of the read
		other  bool   // the other rule's changes are read
		title  string // the other rule's title now
		all    []rules.Rule
		listed []listedRule
		want   string
		unsure bool
	}{
		{what: "its changes alone", all: []rules.Rule{mine}, listed: listed[:1], want: judged},
		{what: "the other rule deleted", other: true, title: "", all: []rules.Rule{mine}, listed: listed[:1], want: judged},
		{what: "the other rule renamed", other: true, title: "Orders slower", all: []rules.Rule{mine}, listed: []listedRule{listed[0], {9, "elsewhere", "Orders slower"}}, want: judged},
		{what: "two rules of the title now", other: true, title: "Orders slow", all: []rules.Rule{mine, theirs}, listed: listed, want: judged},
		{what: "the other rule in a folder the token doesn't read", other: true, title: "Orders slow", all: []rules.Rule{mine}, listed: listed[:1], want: judged},
		{what: "the other rule in a folder the token doesn't read, the judged rule quiet", quiet: true, other: true, title: "Orders slow", all: []rules.Rule{mine}, listed: listed[:1]},
		{what: "the judged rule renamed since the rules were read", all: []rules.Rule{mine}, listed: []listedRule{{7, "orders", "Orders slower"}}, unsure: true},
		{what: "the judged rule listed no more", all: []rules.Rule{mine}, listed: listed[1:], unsure: true},
		{what: "the judged rule listed twice", all: []rules.Rule{mine}, listed: []listedRule{listed[0], {8, "orders", "Orders slow"}}, unsure: true},
	} {
		g := &fakeGrafana{settings: `{"buildInfo": {"version": "12.4.14"}, "unifiedAlerting": {"alertStateHistoryBackend": "annotations"}}`, listed: c.listed}
		if !c.quiet {
			g.notes = []annotation{note(1, 1, 7, "Orders slow", "Normal", "Alerting"), note(4, 9, 7, "Orders slow", "Alerting", "Normal")}
		}
		if c.other {
			g.notes = append(g.notes, note(2, 3, 9, c.title, "Normal", "Alerting"), note(3, 4, 9, c.title, "Alerting", "Normal"))
		}
		h, _ := g.start(t)
		j, err := h.Judge(context.Background(), HistoryAnnotations, []rules.Rule{mine}, c.all, nil, min_(0), min_(10))
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if got := firingsOf(j.Firings); got != c.want || (len(j.Unsure) == 1) != c.unsure || len(j.Shared) != 0 {
			t.Errorf("%s: firings %q, unsure %v, shared %v", c.what, got, j.Unsure, j.Shared)
		}
		if n := countAsked(g, "/api/v1/provisioning/alert-rules"); n != 1 {
			t.Errorf("%s: its rules listed %d times", c.what, n)
		}
	}
}

// countAsked is how many requests g was asked of path.
func countAsked(g *fakeGrafana, path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, a := range g.asked {
		if p, _, _ := strings.Cut(a, "?"); p == path {
			n++
		}
	}
	return n
}

// However many rules of a title shared are judged, each is judged by its
// id, Grafana's rules listed once: none is left out, as none is asked of
// alone.
func TestGrafanaJudgedManyShared(t *testing.T) {
	const n = 250
	const unsupported = "its condition reads a mysql data source: only PromQL and LogQL are replayed"
	g := &fakeGrafana{settings: `{"buildInfo": {"version": "12.4.14"}, "unifiedAlerting": {"alertStateHistoryBackend": "annotations"}}`}
	var rs, all []rules.Rule
	for i := range n {
		title := fmt.Sprintf("Rule %03d", i)
		uid, id := fmt.Sprintf("j%03d", i), int64(1000+i)
		r := rules.Rule{Name: title, UID: uid, Unsupported: unsupported}
		rs = append(rs, r)
		// Another rule of the title in another folder, paused.
		other := rules.Rule{Name: title + " [Elsewhere]", Alert: title, UID: fmt.Sprintf("o%03d", i)}
		all = append(all, r, other)
		g.listed = append(g.listed, listedRule{id, uid, title}, listedRule{id + 5000, other.UID, title})
		a := note(int64(2*i+1), min_(1), title, "Normal", "Alerting", "service", "x")
		b := note(int64(2*i+2), min_(2), title, "Alerting", "Normal", "service", "x")
		a.AlertID, b.AlertID = id, id
		g.notes = append(g.notes, a, b)
	}
	h, _ := g.start(t)
	j, err := h.Judge(context.Background(), HistoryAnnotations, rs, all, nil, min_(0), min_(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Firings) != n || len(j.Shared)+len(j.Unsure) != 0 {
		t.Errorf("%d firings of %d rules, shared %v, unsure %v", len(j.Firings), n, j.Shared, j.Unsure)
	}
	if got := countAsked(g, "/api/v1/provisioning/alert-rules"); got != 1 {
		t.Errorf("its rules listed %d times", got)
	}
}

// A list of Grafana's rules that isn't one, or holds more than MaxRules,
// can't be used: no rule is judged.
func TestGrafanaJudgedListBounds(t *testing.T) {
	const unsupported = "its condition reads a mysql data source: only PromQL and LogQL are replayed"
	mine := rules.Rule{Name: "Orders slow", UID: "orders", Unsupported: unsupported}
	for what, body := range map[string]string{
		"not a list":     `{"id": 7, "uid": "orders", "title": "Orders slow"}`,
		"too many rules": "[" + strings.Repeat(`{},`, MaxRules) + `{"id": 7, "uid": "orders", "title": "Orders slow"}]`,
	} {
		g := &fakeGrafana{settings: `{"buildInfo": {"version": "12.4.14"}, "unifiedAlerting": {"alertStateHistoryBackend": "annotations"}}`}
		a, b := note(1, min_(1), "Orders slow", "Normal", "Alerting"), note(2, min_(2), "Orders slow", "Alerting", "Normal")
		a.AlertID, b.AlertID = 7, 7
		g.notes = []annotation{a, b}
		h, srv := g.start(t)
		srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/provisioning/alert-rules" {
				io.WriteString(w, body)
				return
			}
			g.ServeHTTP(w, r)
		})
		j, err := h.Judge(context.Background(), HistoryAnnotations, []rules.Rule{mine}, nil, nil, min_(0), min_(10))
		if !errors.Is(err, ErrUnusable) || len(j.Firings) != 0 {
			t.Errorf("%s: %d firings, %v", what, len(j.Firings), err)
		}
	}
}
