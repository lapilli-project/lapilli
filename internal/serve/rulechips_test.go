package serve

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// chipsServer is server with rules of many labels, each fired over the
// last weeks: one with more labels than a row shows, its group's among
// them; one whose own label has the group's in another case; one with a
// label value of a quote; one with none but its severity.
func chipsServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	s, rules := server(t, nil)
	os.WriteFile(filepath.Join(rules, "labels.yaml"), []byte(`groups:
  - name: tiers
    labels: {tier: gold}
    rules:
      - {alert: ManyLabels, expr: up == 1, labels: {severity: critical, a: "1", b: "2", c: "3", d: "4", e: "5", f: "6"}}
      - {alert: SameTier, expr: up == 2, labels: {severity: warning, tier: GOLD, service: "a b"}}
      - {alert: Quoted, expr: up == 3, labels: {note: 'say "hi"', service: checkout}}
      - {alert: Plain, expr: up == 4, labels: {severity: info}}
      - {alert: Padded, expr: up == 5, labels: {pad: " x "}}
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	var fs []firing.Firing
	for i, n := range []string{"ManyLabels", "SameTier", "Quoted", "Plain", "Errors", "Padded"} {
		at := now.Add(-time.Duration(30+i) * time.Hour)
		fs = append(fs, firing.Firing{Rule: n, Labels: map[string]string{"service": "checkout"}, Start: at, End: at.Add(10 * time.Minute)})
	}
	from := now.Add(-7 * 24 * time.Hour)
	if err := s.l.Record(ctx, ledger.Found{From: from, To: now, Firings: fs, Run: ledger.Run{Kind: "backfill", Started: from, Finished: from}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	return s
}

// chipRow is a row of the rules list as a reader sees it: its rule, and
// its label chips, shown and behind "+N", each with its link.
type chipRow struct {
	rule        string
	shown, more []string
	href        map[string]string
	on          []string
	summary     string
}

// chipRows are the rows of a rules list's table.
func chipRows(t *testing.T, body string) []chipRow {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out []chipRow
	for _, td := range allNodes(doc) {
		if td.DataAtom != atom.Td || nodeAttr(td, "class") != "rule" || !nodeWithin(td, atom.Tbody) {
			continue
		}
		row := chipRow{href: map[string]string{}}
		for _, n := range allNodes(td) {
			switch {
			case n.DataAtom == atom.A && row.rule == "":
				row.rule = nodeText(n)
			case n.DataAtom == atom.Summary:
				row.summary = nodeText(n)
			case strings.Contains(nodeAttr(n, "class"), "chip label"):
				text := ""
				for _, b := range allNodes(n) {
					if b.DataAtom == atom.Bdi {
						text = nodeText(b)
					}
				}
				if inClass(n, "more-labels") {
					row.more = append(row.more, text)
				} else {
					row.shown = append(row.shown, text)
				}
				if n.DataAtom == atom.A {
					row.href[text] = nodeAttr(n, "href")
				}
				if strings.Contains(nodeAttr(n, "class"), " on") {
					row.on = append(row.on, text)
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// TestRuleLabelChips: each row of the rules list has the labels label:
// matches it by as chips, but its severity: five, and the rest behind
// "+N"; each a link to the list with that label too, kept as the list was
// asked, which shows exactly the rows that have it, the chip drawn there as
// picked; and none a link the search couldn't take.
func TestRuleLabelChips(t *testing.T) {
	parallel(t)
	s := chipsServer(t)
	h := s.Handler()
	code, body := getAs(t, h, "/rules?weeks=12", "")
	if code != http.StatusOK {
		t.Fatal(code)
	}
	rows := chipRows(t, body)
	byRule := map[string]chipRow{}
	for _, r := range rows {
		byRule[r.rule] = r
	}
	want := map[string][]string{
		"ManyLabels": {"a=1", "b=2", "c=3", "d=4", "e=5", "f=6", "tier=gold"},
		"SameTier":   {"service=a b", "tier=GOLD"},
		"Quoted":     {`note=say "hi"`, "service=checkout", "tier=gold"},
		"Plain":      {"tier=gold"},
		"Padded":     {"pad= x", "tier=gold"}, // as a reader reads it
		"Errors":     nil,
	}
	for rule, labels := range want {
		r, ok := byRule[rule]
		if !ok {
			t.Fatalf("no row for %s: %v", rule, slices.Collect(maps.Keys(byRule)))
		}
		if got := append(slices.Clone(r.shown), r.more...); !slices.Equal(got, labels) {
			t.Errorf("%s: chips %q, want %q", rule, got, labels)
		}
		if len(r.shown) > rowChips {
			t.Errorf("%s: %d chips shown", rule, len(r.shown))
		}
		if wantMore := max(0, len(labels)-rowChips); len(r.more) != wantMore || wantMore > 0 && r.summary != fmt.Sprintf("+%d%d more labels", wantMore, wantMore) {
			t.Errorf("%s: %d behind %q, want %d", rule, len(r.more), r.summary, wantMore)
		}
	}
	if href, ok := byRule["Quoted"].href[`note=say "hi"`]; ok {
		t.Errorf("a value the search can't write is a link: %s", href)
	}
	if len(byRule["Padded"].href) != 1 || byRule["Padded"].href["tier=gold"] == "" {
		t.Errorf("a value with spaces after it, which the search takes off, is a link: %v", byRule["Padded"].href)
	}
	// A name with "=" (copied in, as a recorded rule's labels are) would be
	// cut at it, and a value's spaces at its end taken off: no link; a
	// space at its start, after the "=", is kept, and the link is right.
	if picksLabel(qword{Key: "label", Value: "a=b=c"}, "a=b", "c") || !picksLabel(qword{Key: "label", Value: "a=b=c"}, "a", "b=c") ||
		picksLabel(qword{Key: "label", Value: "k=v "}, "k", "v ") || !picksLabel(qword{Key: "label", Value: "k= v"}, "k", " v") ||
		!picksLabel(qword{Key: "label", Value: "k=a b"}, "k", "a b") {
		t.Error("picksLabel")
	}
	// Each link shows the rows with its label, and only those, from the
	// list as it was asked.
	checked := 0
	for _, r := range rows {
		for label, href := range r.href {
			if !strings.HasPrefix(href, "/rules?") || !strings.Contains(href, "weeks=12") {
				t.Errorf("%s %s: %s", r.rule, label, href)
			}
			code, got := getAs(t, h, href, "")
			if code != http.StatusOK {
				t.Fatalf("%s: %d", href, code)
			}
			k, v, _ := strings.Cut(label, "=")
			var with, shown []string
			for _, x := range rows {
				if slices.ContainsFunc(append(slices.Clone(x.shown), x.more...), func(c string) bool { return sameLabel(c, k, v) }) {
					with = append(with, x.rule)
				}
			}
			for _, x := range chipRows(t, got) {
				shown = append(shown, x.rule)
				if !slices.ContainsFunc(x.on, func(c string) bool { return sameLabel(c, k, v) }) {
					t.Errorf("%s: %s's chip %s isn't drawn as picked: %q", href, x.rule, label, x.on)
				}
			}
			slices.Sort(with)
			slices.Sort(shown)
			if !slices.Equal(with, shown) {
				t.Errorf("%s: shows %q, the rows with %s are %q", href, shown, label, with)
			}
			if !strings.Contains(got, fmt.Sprintf("%d of ", len(shown))) {
				t.Errorf("%s: doesn't say it shows %d", href, len(shown))
			}
			checked++
		}
	}
	if checked < 8 {
		t.Errorf("%d links followed", checked)
	}
	// The links keep what the list was asked, its order and facets.
	_, body = getAs(t, h, "/rules?weeks=12&sort=refired&severity=critical", "")
	for _, r := range chipRows(t, body) {
		if h := r.href["a=1"]; !strings.Contains(h, "sort=refired") || !strings.Contains(h, "severity=critical") || !strings.Contains(h, "q=label%3Aa%3D1") {
			t.Errorf("a chip's link from a list with facets: %q", h)
		}
	}
	// With the search's twenty words used, none adds one.
	_, body = getAs(t, h, "/rules?weeks=12&q=e+e+e+e+e+e+e+e+e+e+e+e+e+e+e+e+e+e+e+e", "")
	for _, r := range chipRows(t, body) {
		if len(r.href) > 0 {
			t.Errorf("%s: links with the search full: %v", r.rule, r.href)
		}
	}
}
