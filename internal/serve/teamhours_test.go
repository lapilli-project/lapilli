package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// Two rules fire alike: the search firings, from 10:02 on Sunday 1 November
// to 05:02 on the Monday, an hour apart, catch nothing. The server works
// every day, 08:00 to 18:00 UTC. The search team owns Errors and keeps the
// same hours in Tokyo, nine hours ahead: there 13:02 UTC is 22:02, asleep,
// where the server counts it at work. By the server's hours each rule's
// twenty come 8 asleep, 4 off hours and 8 at work; by Tokyo's, 10 asleep,
// 3 off hours and 7 at work.
func TestTeamHours(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, func(c *Config) {
		c.Options.Hours = verdict.DefaultHours(time.UTC)
		if err := c.Options.Hours.ParseWork("mon-sun@08:00-18:00"); err != nil {
			t.Fatal(err)
		}
	})
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(`groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
        labels: {severity: page}
      - alert: Slow
        expr: errors > 1
        for: 2m
        labels: {severity: ticket}
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()

	// Hours that aren't are refused, saying why.
	for _, bad := range []string{`"tz": "Mars/Olympus"`, `"work_hours": "weekdays"`, `"sleep_hours": "22:00"`, `"tz": "../../etc/passwd"`} {
		if code, body := api(t, h, "POST", "/api/v1/teams", `{"name": "search", "rules": "^Errors$", `+bad+`}`); code != 400 || !strings.Contains(body, "want") {
			t.Errorf("%s: %d %s", bad, code, body)
		}
	}
	// Before the team: both by the server's hours, and no team named.
	if _, body, _ := get(t, h, "/rules"); strings.Count(body, "8 asleep, 4 off hours") != 2 {
		t.Errorf("rules before the team: %s", body)
	}
	code, body := api(t, h, "POST", "/api/v1/teams", `{"name": "search", "rules": "^Errors$", "tz": "Asia/Tokyo"}`)
	if code/100 != 2 {
		t.Fatalf("team: %d %s", code, body)
	}

	if _, body, _ := get(t, h, "/rules"); !strings.Contains(body, "10 asleep, 3 off hours") || !strings.Contains(body, "8 asleep, 4 off hours") {
		t.Errorf("rules: %s", body)
	}
	if _, body, _ := get(t, h, "/rules?team=search"); !strings.Contains(body, "10 asleep, 3 off hours") || strings.Contains(body, "8 asleep, 4 off hours") {
		t.Errorf("the team's rules: %s", body)
	}
	if _, body, _ := get(t, h, "/rules/Errors"); !strings.Contains(body, "10 came while people slept, and 3 more off hours") {
		t.Errorf("rule: %s", body)
	}
	code, body, _ = get(t, h, "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false")
	if code != 200 || !strings.Contains(body, `"asleep_black": 10`) || !strings.Contains(body, `"asleep_black": 8`) ||
		!strings.Contains(body, `"team": "search"`) || !strings.Contains(body, `"zone": "Asia/Tokyo"`) {
		t.Errorf("report: %d %s", code, body)
	}
	if _, body, _ := get(t, s.MetricsHandler(), "/metrics"); !strings.Contains(body, `lapilli_rule_nothing_by_hours{rule="Errors",severity="page",team="search",hours="asleep"} 10`) ||
		!strings.Contains(body, `lapilli_rule_nothing_by_hours{rule="Slow",severity="ticket",hours="asleep"} 8`) ||
		!strings.Contains(body, `lapilli_rule_nothing_by_hours{rule="Errors",severity="page",team="search",hours="at_work"} 7`) {
		t.Errorf("metrics: %s", body)
	}

	// The last whole week has the Sunday: of Errors' ten asleep, all ten; of
	// Slow's eight, the two before midnight.
	tile := func(path, lang string) string {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Accept-Language", lang)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		body := w.Body.String()
		i := strings.Index(body, map[string]string{"en": "Woken for nothing", "ko": tr("ko", "Woken for nothing")}[lang])
		if i < 0 {
			t.Fatalf("%s: no tile: %s", path, body)
		}
		return body[i:min(len(body), i+500)]
	}
	if x := tile("/", "en"); !strings.Contains(x, ">12<") || !strings.Contains(x, "firings that caught nothing, by each team&#39;s hours asleep") {
		t.Errorf("overview: %s", x)
	}
	if x := tile("/?team=search", "en"); !strings.Contains(x, ">10<") || !strings.Contains(x, "firings that caught nothing, 22:00–08:00 in Asia/Tokyo") {
		t.Errorf("the team's overview: %s", x)
	}
	if x := tile("/?team=search", "ko"); !strings.Contains(x, "아무것도 못 잡은 발화, Asia/Tokyo 기준 22:00–08:00") {
		t.Errorf("in Korean: %s", x)
	}
	if _, body, _ := get(t, h, "/teams"); !strings.Contains(body, "at work mon-sun 08:00-18:00, asleep 22:00-08:00, in Asia/Tokyo") ||
		!strings.Contains(body, "by the server&#39;s otherwise: at work mon-sun 08:00-18:00, asleep 22:00-08:00, in UTC") {
		t.Errorf("teams: %s", body)
	}

	// The team's digest is by its hours, and says so; everyone's names the
	// team's.
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	rep, err := s.digestReport(ctx, from, to, o)
	if err != nil {
		t.Fatal(err)
	}
	own, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	team, _ := own.team("search")
	text, err := s.digestText(ctx, teamReport(rep, own, team, o.Hours), nil, from, to, &team)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Out of hours, in Asia/Tokyo (at work mon-sun 08:00-18:00, asleep 22:00-08:00)") ||
		!strings.Contains(text, "10 came while people slept, and 3 more off hours") || strings.Contains(text, "Slow") {
		t.Errorf("the team's digest:\n%s", text)
	}
	if text, err = s.digestText(ctx, rep, nil, from, to, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Out of hours, in UTC (at work mon-sun 08:00-18:00, asleep 22:00-08:00):\nsearch's rules by its own hours, in Asia/Tokyo") ||
		!strings.Contains(text, "12 came while people slept, and 7 more off hours") {
		t.Errorf("the digest:\n%s", text)
	}

	// The team's zone taken away, the next page counts Errors by the
	// server's again.
	code, body = api(t, h, "PUT", "/api/v1/teams/"+strconv.FormatInt(team.ID, 10), `{"name": "search", "rules": "^Errors$"}`)
	if code/100 != 2 {
		t.Fatalf("team: %d %s", code, body)
	}
	if _, body, _ := get(t, h, "/rules"); strings.Count(body, "8 asleep, 4 off hours") != 2 {
		t.Errorf("rules after the team's hours went: %s", body)
	}
	if x := tile("/", "en"); !strings.Contains(x, ">4<") || !strings.Contains(x, "firings that caught nothing, 22:00–08:00") {
		t.Errorf("overview after: %s", x)
	}
}

// A team's hours count when the server counts by none.
func TestTeamHoursAlone(t *testing.T) {
	ctx := context.Background()
	// As the command line gives "none": hours with nothing in them.
	none := verdict.DefaultHours(time.UTC)
	if err := errors.Join(none.ParseWork("none"), none.ParseSleep("none")); err != nil {
		t.Fatal(err)
	}
	s, _ := server(t, func(c *Config) { c.Options.Hours = none })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	if _, body, _ := get(t, h, "/"); strings.Contains(body, "Woken for nothing") {
		t.Error("a tile for hours not given")
	}
	if code, body := api(t, h, "POST", "/api/v1/teams", `{"name": "search", "rules": "^Errors$", "sleep_hours": "22:00-08:00"}`); code/100 != 2 {
		t.Fatalf("team: %d %s", code, body)
	}
	if _, body, _ := get(t, h, "/rules"); !strings.Contains(body, "8 asleep") {
		t.Errorf("rules: %s", body)
	}
	if _, body, _ := get(t, h, "/"); !strings.Contains(body, "Woken for nothing") || !strings.Contains(body, "22:00–08:00") {
		t.Errorf("the tile: %s", body)
	}
	_, body, _ := get(t, h, "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false")
	if !strings.Contains(body, `"asleep_black": 8`) || !strings.Contains(body, `"team": "search"`) {
		t.Errorf("report: %s", body)
	}
	// Hours the same as the server's are the server's: not the team's own.
	if code, body := api(t, h, "POST", "/api/v1/teams", `{"name": "shop", "rules": "^Slow$", "work_hours": "none", "tz": "UTC"}`); code/100 != 2 {
		t.Fatalf("team: %d %s", code, body)
	}
	if _, body, _ = get(t, h, "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false"); strings.Contains(body, `"team": "shop"`) {
		t.Errorf("the server's hours as a team's own: %s", body)
	}
	// Refused: the server's own zone, and hours too long to be any.
	for _, team := range []string{
		`{"name": "x", "rules": "^X$", "tz": "Local"}`,
		`{"name": "x", "rules": "^X$", "work_hours": "` + strings.Repeat("mon,", 40) + `mon@08:00-18:00"}`,
	} {
		if code, body := api(t, h, "POST", "/api/v1/teams", team); code != 400 {
			t.Errorf("%s: %d %s", team, code, body)
		}
	}
}

// A team in shifts: one turn, in Tokyo, counts as Tokyo's hours do, and is
// kept from the form and shown as shifts.
func TestTeamShifts(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, func(c *Config) {
		c.Options.Hours = verdict.DefaultHours(time.UTC)
		if err := c.Options.Hours.ParseWork("mon-sun@08:00-18:00"); err != nil {
			t.Fatal(err)
		}
	})
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(`groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
      - alert: Slow
        expr: errors > 1
        for: 2m
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()

	if code, body := api(t, h, "POST", "/api/v1/teams", `{"name": "search", "rules": "^Errors$", "shifts": "every=1w"}`); code != 400 || !strings.Contains(body, "aren't shifts") {
		t.Errorf("not shifts: %d %s", code, body)
	}
	const shifts = "every=1w from=2026-10-26; tz=Asia/Tokyo"
	form := url.Values{"name": {"search"}, "rules": {"^Errors$"}, "shifts": {shifts}}
	if code, body, _ := post(t, h, "/teams", form, "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("team: %d %s", code, body)
	}
	if _, body, _ := get(t, h, "/teams"); !strings.Contains(body, "in shifts: "+shifts+"; where a turn doesn&#39;t say, at work mon-sun 08:00-18:00, asleep 22:00-08:00, in UTC") {
		t.Errorf("teams: %s", body)
	}
	if _, body, _ := get(t, h, "/teams?edit=1"); !strings.Contains(body, `value="`+shifts+`"`) {
		t.Errorf("the team's form: %s", body)
	}
	if _, body, _ := get(t, h, "/?team=search"); !strings.Contains(body, "by the hours asleep of each shift") {
		t.Errorf("the team's overview: %s", body)
	}
	if _, body, _ := get(t, h, "/rules"); !strings.Contains(body, "10 asleep, 3 off hours") || !strings.Contains(body, "8 asleep, 4 off hours") {
		t.Errorf("rules: %s", body)
	}
	code, body, _ := get(t, h, "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false")
	if code != 200 || !strings.Contains(body, `"shifts": "`+shifts+`"`) || !strings.Contains(body, `"asleep_black": 10`) {
		t.Errorf("report: %d %s", code, body)
	}
}
