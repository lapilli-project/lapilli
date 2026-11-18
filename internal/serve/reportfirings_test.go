package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// The firings section lists the span's firings as /api/v1/report gives
// them, each with its rule's severity and team: a team's report, its
// rules' alone; past reportFiringsMost, the first of them, with how many
// there were; in a span with none, none. The API and the UI's form pick
// it, and report.ready carries it.
func TestReportFirings(t *testing.T) {
	rv := newReceiver(t)
	s := reportServer(t, rv, func(c *Config) {
		two := ruleFile + "      - alert: Busy\n        expr: busy > 1\n        labels: {severity: ticket}\n"
		if err := os.WriteFile(filepath.Join(c.RulesPath, "shop.yaml"), []byte(two), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()
	h := s.Handler()
	shop, err := s.l.PutTeam(ctx, ledger.Team{Name: "shop", Rules: "^Errors$"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	none, err := s.l.PutTeam(ctx, ledger.Team{Name: "none", Rules: "^Nothing$"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	put := func(name string, team int64, span string) ledger.Scheduled {
		t.Helper()
		id, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: name, TeamID: team, Schedule: "0 12 * * *", Span: span, Sections: []string{"firings"},
			Dests: []string{"webhook"}, Enabled: true}, "ana", now.Add(-24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		line, err := s.reportByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return line.Scheduled
	}
	everyone, ofShop, ofNone, quiet := put("everyone", 0, "72h"), put("shop", shop, "72h"), put("none", none, "72h"), put("quiet", 0, "2h")

	type section struct {
		Items     []map[string]any `json:"items"`
		Total     int              `json:"total"`
		Truncated bool             `json:"truncated"`
	}
	// What report.ready would say of a report, as JSON.
	ready := func(r ledger.Scheduled) (from, to, text string, sec section, raw string) {
		t.Helper()
		b, err := s.preview(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		j, err := json.Marshal(b.about())
		if err != nil {
			t.Fatal(err)
		}
		var about struct {
			From, To, Text string
			Numbers        map[string]json.RawMessage
		}
		if err := json.Unmarshal(j, &about); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(about.Numbers["firings"], &sec); err != nil {
			t.Fatalf("%s: %v", about.Numbers["firings"], err)
		}
		return about.From, about.To, about.Text, sec, string(about.Numbers["firings"])
	}
	from, to, text, all, _ := ready(everyone)
	code, body := api(t, h, "GET", "/api/v1/report?from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to), "")
	if code != http.StatusOK {
		t.Fatalf("/api/v1/report: %d %s", code, body)
	}
	var rep struct{ Firings []map[string]any }
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatal(err)
	}
	rules := map[string]int{}
	for _, f := range rep.Firings {
		rules[f["rule"].(string)]++
	}
	if rules["Errors"] < 4 || rules["Busy"] == 0 {
		t.Fatalf("the span's firings, by rule: %v: the test shows less", rules)
	}
	// Each as the report has it, and its rule's severity and team.
	bare := func(items []map[string]any) []map[string]any {
		t.Helper()
		out := []map[string]any{}
		for _, f := range items {
			g := map[string]any{}
			for k, v := range f {
				g[k] = v
			}
			severity, team := map[string]string{"Errors": "page", "Busy": "ticket"}[f["rule"].(string)], map[string]string{"Errors": "shop"}[f["rule"].(string)]
			if g["severity"] != severity || (team == "") != (g["team"] == nil) || team != "" && g["team"] != team {
				t.Errorf("%v: want severity %q, team %q", f, severity, team)
			}
			delete(g, "severity")
			delete(g, "team")
			out = append(out, g)
		}
		return out
	}
	of := func(rule string) []map[string]any {
		out := []map[string]any{}
		for _, f := range rep.Firings {
			if f["rule"] == rule {
				out = append(out, f)
			}
		}
		return out
	}
	if !reflect.DeepEqual(bare(all.Items), rep.Firings) || all.Total != len(rep.Firings) || all.Truncated {
		t.Errorf("everyone's: %d of %d, truncated %v; /api/v1/report has %d", len(all.Items), all.Total, all.Truncated, len(rep.Firings))
	}
	if want := fmt.Sprintf("Firings that started in the span: %d, each listed in the JSON.", len(rep.Firings)); !strings.Contains(text, want) {
		t.Errorf("the text: %q, want %q", text, want)
	}
	_, _, _, mine, _ := ready(ofShop)
	if !reflect.DeepEqual(bare(mine.Items), of("Errors")) || mine.Total != rules["Errors"] || mine.Truncated {
		t.Errorf("a team's, of its rules alone: %d of %d, truncated %v", len(mine.Items), mine.Total, mine.Truncated)
	}

	// Cut: the first of them, and how many there were, everyone's and a
	// team's, counted by its rules.
	was := reportFiringsMost
	reportFiringsMost = 3
	_, _, text, cut, _ := ready(everyone)
	_, _, _, cutMine, _ := ready(ofShop)
	reportFiringsMost = was
	if !reflect.DeepEqual(bare(cut.Items), rep.Firings[:3]) || cut.Total != len(rep.Firings) || !cut.Truncated {
		t.Errorf("cut: %d of %d, truncated %v", len(cut.Items), cut.Total, cut.Truncated)
	}
	if want := fmt.Sprintf("Firings that started in the span: %d; the JSON lists the first 3.", len(rep.Firings)); !strings.Contains(text, want) {
		t.Errorf("the text, cut: %q, want %q", text, want)
	}
	if !reflect.DeepEqual(bare(cutMine.Items), of("Errors")[:3]) || cutMine.Total != rules["Errors"] || !cutMine.Truncated {
		t.Errorf("a team's, cut: %d of %d, truncated %v", len(cutMine.Items), cutMine.Total, cutMine.Truncated)
	}

	// Cut by length too: the items, as report.ready encodes them, kept
	// within reportFiringsBytes; whichever cut comes first.
	_, _, _, _, whole := ready(everyone)
	var enc struct{ Items json.RawMessage }
	if err := json.Unmarshal([]byte(whole), &enc); err != nil {
		t.Fatal(err)
	}
	wasBytes := reportFiringsBytes
	t.Cleanup(func() { reportFiringsMost, reportFiringsBytes = was, wasBytes })
	n := len(rep.Firings)
	for _, c := range []struct{ most, bytes, want int }{
		{was, len(enc.Items), n}, {was, len(enc.Items) - 1, n - 1}, {was, len("[]"), 0}, {3, len(enc.Items) - 1, 3}, {n, len(enc.Items) - 1, n - 1},
	} {
		reportFiringsMost, reportFiringsBytes = c.most, c.bytes
		_, _, text, sec, raw := ready(everyone)
		var got struct{ Items json.RawMessage }
		json.Unmarshal([]byte(raw), &got)
		cut := c.want < n
		line := fmt.Sprintf("Firings that started in the span: %d, each listed in the JSON.", n)
		if cut {
			line = fmt.Sprintf("Firings that started in the span: %d; the JSON lists the first %d.", n, c.want)
		}
		if !reflect.DeepEqual(bare(sec.Items), rep.Firings[:c.want]) || sec.Total != n || sec.Truncated != cut || len(got.Items) > c.bytes ||
			!strings.Contains(text, line) {
			t.Errorf("%d at most, %d bytes: %d of %d, truncated %v, %d bytes, %q", c.most, c.bytes, len(sec.Items), sec.Total, sec.Truncated, len(got.Items), text)
		}
	}
	reportFiringsMost, reportFiringsBytes = was, wasBytes

	// None: a team that owns no rule, and a span nothing started in.
	for _, r := range []ledger.Scheduled{ofNone, quiet} {
		_, _, text, sec, raw := ready(r)
		if len(sec.Items) != 0 || sec.Total != 0 || sec.Truncated || !strings.Contains(raw, `"items":[]`) || !strings.Contains(text, "No firing started in the span.") {
			t.Errorf("%s: %s %q", r.Name, raw, text)
		}
	}

	// The API and the UI's form take it.
	code, body = api(t, h, "POST", "/api/v1/reports", `{"name": "listed", "schedule": "daily@09:00", "sections": ["firings", "summary"], "dests": ["webhook"]}`)
	var made ledger.Scheduled
	if json.Unmarshal([]byte(body), &made); code != http.StatusCreated || !reflect.DeepEqual(made.Sections, []string{"summary", "firings"}) {
		t.Errorf("POST /api/v1/reports: %d %s", code, body)
	}
	if _, body, _ := get(t, h, "/reports"); !strings.Contains(body, `name="sections" value="firings"`) {
		t.Errorf("the form has no firings to pick: %s", body)
	}

	// report.ready carries it, to the webhook.
	at(t, s, now.Add(6*time.Hour), false)
	sent := 0
	for _, e := range rv.events("report.ready") {
		if e["report"] != everyone.Name {
			continue
		}
		sent++
		sec, _ := e["numbers"].(map[string]any)["firings"].(map[string]any)
		if items, _ := sec["items"].([]any); len(items) != len(rep.Firings) || sec["total"] != float64(len(rep.Firings)) || sec["truncated"] != false {
			t.Errorf("report.ready's firings: %v", sec)
		}
	}
	if sent != 1 {
		t.Errorf("report.ready of everyone's: %d", sent)
	}
}

// --config declares a report holding it, as the form and the API make one.
func TestReportFiringsDeclared(t *testing.T) {
	s, _ := configServer(t, strings.Replace(configFile, "sections: [summary, noise]", "sections: [firings, summary]", 1), nil)
	rs, err := s.l.Scheduleds(context.Background())
	if err != nil || len(rs) != 1 || !rs[0].Declared || !reflect.DeepEqual(rs[0].Sections, []string{"summary", "firings"}) {
		t.Errorf("%+v %v", rs, err)
	}
}
