package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// dayCounts are, of a report's firings, by rule and by day (in loc), how
// many caught nothing and were judged (black), and how many were counted,
// not judged (no record).
type dayCounts struct{ black, unjudged int }

func countsOf(rep verdict.Report, loc *time.Location) map[string]map[string]dayCounts {
	out := map[string]map[string]dayCounts{}
	for _, f := range rep.Firings {
		if f.White {
			continue
		}
		day := f.Start.In(loc).Format("2006-01-02")
		if out[f.Rule] == nil {
			out[f.Rule] = map[string]dayCounts{}
		}
		c := out[f.Rule][day]
		if f.NoRecord {
			c.unjudged++
		} else {
			c.black++
		}
		out[f.Rule][day] = c
	}
	return out
}

// rowCells are the numbers in the row of a table whose header cell is
// head: each <td class="n">, in order.
func rowCells(t *testing.T, body, head string) []int {
	t.Helper()
	i := strings.Index(body, `<th scope="row">`+head+`</th>`)
	if i < 0 {
		return nil
	}
	row := body[i:]
	row = row[:strings.Index(row, "</tr>")]
	var out []int
	for _, m := range regexp.MustCompile(`<td class="n">(\d+)</td>`).FindAllStringSubmatch(row, -1) {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

// hourSums are a firings page's hours table, summed by column: fired,
// caught, nothing and, when it has the column, not judged.
func hourSums(t *testing.T, body string) []int {
	t.Helper()
	sum := []int{}
	for h := range 24 {
		for i, n := range rowCells(t, body, strconv.Itoa(100 + h)[1:]+":00") {
			if i >= len(sum) {
				sum = append(sum, 0)
			}
			sum[i] += n
		}
	}
	return sum
}

// A firing that caught nothing with no record to judge it by is said
// alike on every page that draws or lists it: as the report says it, "not
// judged", never ● or "caught nothing" as one judged black. With no named
// signal, a week with no record has none of its firings judged; with
// named signals, those whose signals keep no record (TestSignalsJudged-
// ByTheirOwn), beside others judged the same day.
func TestNotJudgedAlike(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		s    func(*testing.T) *Server
	}{
		{"no record, no named signal", func(t *testing.T) *Server {
			s, _ := server(t, nil)
			s.cfg.RecordByWeek = true
			raw := ""
			if err := s.look(ctx, &raw); err != nil {
				t.Fatal(err)
			}
			s.jobs(ctx, true)
			return s
		}},
		{"named signals, one waiting", func(t *testing.T) *Server { return signalServer(t, shopHistory(), nil) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := c.s(t)
			h := s.Handler()
			// Every firing the ledger has, as the report judges each.
			code, body, _ := get(t, h, "/api/v1/report?from=2026-10-05T00:00:00Z&to=2026-11-04T00:00:00Z")
			if code != http.StatusOK {
				t.Fatalf("report: %d %s", code, body)
			}
			var rep verdict.Report
			if err := json.Unmarshal([]byte(body), &rep); err != nil {
				t.Fatal(err)
			}
			counts := countsOf(rep, s.cfg.Loc)
			unjudged := 0
			for rule, days := range counts {
				for day, want := range days {
					unjudged += want.unjudged
					// The rule's strip, a day a row: nothing, then not
					// judged, as the report counts them.
					_, page, _ := get(t, h, "/rules/"+rule)
					cells := rowCells(t, page, day)
					if want.unjudged > 0 && (len(cells) < 4 || cells[2] != want.black || cells[3] != want.unjudged) {
						t.Errorf("%s, %s: the rule's strip has %v; the report, %d black and %d not judged", rule, day, cells, want.black, want.unjudged)
					}
					// Its latest firings, one by one: as many not judged as
					// the report has of the rule (fewer than a page of them).
					ruleUnjudged := 0
					for _, d := range days {
						ruleUnjudged += d.unjudged
					}
					if _, tab, _ := get(t, h, "/rules/"+rule+"?tab=firings"); strings.Count(tab, "? not judged</span>") != ruleUnjudged {
						t.Errorf("%s: its latest firings call %d not judged, the report %d", rule, strings.Count(tab, "? not judged</span>"), ruleUnjudged)
					}
					// The rules list's spark, week by week, says them apart.
					if _, list, _ := get(t, h, "/rules?weeks=12"); !strings.Contains(list, "not judged</title>") {
						t.Errorf("%s: the rules list's spark says none not judged", rule)
					}
					// The day's firings by the hour, all rules.
					_, fp, _ := get(t, h, "/firings?day="+day)
					all := dayCounts{}
					for _, ds := range counts {
						all.black += ds[day].black
						all.unjudged += ds[day].unjudged
					}
					sums := hourSums(t, fp)
					if all.unjudged > 0 && (len(sums) < 4 || sums[2] != all.black || sums[3] != all.unjudged) {
						t.Errorf("%s: the hours sum to %v; the report, %d black and %d not judged", day, sums, all.black, all.unjudged)
					}
					if n := strings.Count(fp, "? not judged</span>"); all.unjudged > 0 && n != min(all.unjudged, firingsPage) {
						t.Errorf("%s: %d firings listed not judged, want %d", day, n, all.unjudged)
					}
					if all.unjudged > 0 && !strings.Contains(fp, "not judged: no record</li>") {
						t.Errorf("%s: the hours' legend doesn't name what isn't judged", day)
					}
				}
			}
			if unjudged == 0 {
				t.Fatal("no firing that isn't judged: the case tests nothing")
			}
			// The overview's weeks: ● and not judged, as the report of
			// each week counts them.
			_, ov, _ := get(t, h, "/")
			row := regexp.MustCompile(`(?s)<span class="d">(\d{4}-\d{2}-\d{2})</span>.*?</td>\s*<td class="n">(\d+)</td><td class="n">(\d+)</td><td class="n">(\d+)</td><td class="n">(\d+)</td>`)
			weeks := 0
			for _, m := range row.FindAllStringSubmatch(ov, -1) {
				monday, err := time.ParseInLocation("2006-01-02", m[1], s.cfg.Loc)
				if err != nil {
					t.Fatal(err)
				}
				var wk verdict.Report
				_, wb, _ := get(t, h, "/api/v1/report?from="+monday.UTC().Format(time.RFC3339)+"&to="+monday.AddDate(0, 0, 7).UTC().Format(time.RFC3339))
				if err := json.Unmarshal([]byte(wb), &wk); err != nil {
					t.Fatal(err)
				}
				black, un := 0, 0
				for _, f := range wk.Firings {
					switch {
					case f.White:
					case f.NoRecord:
						un++
					default:
						black++
					}
				}
				if got := m[4] + " " + m[5]; got != strconv.Itoa(black)+" "+strconv.Itoa(un) {
					t.Errorf("the overview's week of %s: ● and not judged %s; the report, %d %d", m[1], got, black, un)
				}
				weeks++
			}
			if weeks == 0 {
				t.Error("the overview's weeks table has no column of the not judged")
			}
		})
	}
}

// A week's firings not judged are in the chart's legend and the table's
// column however few of them are drawn: a sliver too thin to draw beside a
// tall week still has its column.
func TestStonesChartUnjudgedTooThinToDraw(t *testing.T) {
	parallel(t)
	ws := []*weekView{
		{Start: now.AddDate(0, 0, -14), Firings: 600, Black: 600, Kept: true},
		{Start: now.AddDate(0, 0, -7), Firings: 30, White: 10, Black: 20, Kept: true, Unjudged: 1},
	}
	if c := stonesChart("en", ws, time.UTC, false); !c.Unjudged {
		t.Error("one firing not judged beside 600: no legend, no column")
	}
	if c := stonesChart("en", ws[:1], time.UTC, false); c.Unjudged {
		t.Error("none not judged, and the legend names them")
	}
}

// With no record, the overview says why once, above its tiles, and the
// tiles it can't fill say only that they aren't known; seven or eight
// tiles are laid in rows of four.
func TestOverviewNoRecordSaidOnce(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
	s.cfg.RecordByWeek = true
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	_, body, _ := get(t, s.Handler(), "/")
	notice := strings.Index(body, "No incident written down for the week of")
	tiles := strings.Index(body, `<section class="tiles tiles-`)
	if notice < 0 || tiles < 0 || notice > tiles || strings.Count(body, "No incident written down for the week of") != 1 {
		t.Errorf("the notice, once, before the tiles: at %d, the tiles at %d", notice, tiles)
	}
	if !strings.Contains(body, `href="/settings#incident-sources"`) || !strings.Contains(body, `href="/incidents"`) {
		t.Error("the notice doesn't say where incidents come from, or where to write one down")
	}
	if n := strings.Count(body, `<div class="tile-of">not known: no record</div>`); n != 2 {
		t.Errorf("%d tiles say not known: no record, want precision and noisy rules", n)
	}
	if strings.Contains(body, "not known: no incident written down") {
		t.Error("a tile says why at length, as the notice does")
	}
}
