package oncall

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const people = `people:
  - name: Jane Doe
    also: [jane@example.com, jdoe]
    tz: Asia/Seoul
    sleep: 23:00-07:00
  - name: Ali Kaya
    also: [ali@example.com, akaya]
    tz: Europe/Istanbul
  - name: Sam Lee
    also: [sam@example.com]
`

func config(t *testing.T, schedules string) *Config {
	t.Helper()
	c, err := Load(write(t, "on-call.yaml", "schedules:\n"+schedules+people))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func feed(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// whoAt is whose turn it is at a time; "" between turns.
func whoAt(turns []verdict.RotaTurn, at string) string {
	t := utc(at)
	for _, x := range turns {
		if !t.Before(x.From) && t.Before(x.To) {
			return x.Who
		}
	}
	return ""
}

// Each tool's feed, as it writes one, read as turns of the people in the
// file: by an attendee's email (PagerDuty, Google), by what who takes out
// of the summary (Opsgenie), by the summary (Grafana OnCall); an override
// holding from its start, the turn under it resuming; a removed turn,
// cancelled, passed over; repeats expanded, an occurrence left out
// (EXDATE) or replaced (RECURRENCE-ID); what can't be read, said.
func TestFeeds(t *testing.T) {
	for _, c := range []struct {
		file, schedule string
		want           map[string]string
		turns          int
		notes          []string
	}{
		{"pagerduty.ics", "  - {name: payments, file: x}\n", map[string]string{
			"2026-10-20T00:00:00Z": "Jane Doe", "2026-10-27T12:00:00Z": "Ali Kaya", "2026-10-27T20:00:00Z": "Jane Doe",
			"2026-10-28T07:00:00Z": "Ali Kaya", "2026-11-03T12:00:00Z": "Sam Lee", "2026-11-10T00:00:00Z": "",
		}, 5, nil},
		{"opsgenie.ics", "  - {name: platform, file: x, who: '^(.+?) \\(user\\) is on call'}\n", map[string]string{
			"2026-11-01T10:00:00Z": "Jane Doe", "2026-11-02T10:00:00Z": "Ali Kaya",
		}, 2, nil},
		{"grafana-oncall.ics", "  - {name: storage, file: x}\n", map[string]string{
			"2026-11-01T01:00:00Z": "Jane Doe", "2026-11-01T07:00:00Z": "Ali Kaya", "2026-11-01T13:00:00Z": "Jane Doe", "2026-11-02T01:00:00Z": "",
		}, 3, nil},
		{"google.ics", "  - {name: search, file: x}\n", map[string]string{
			"2026-10-06T00:00:00Z": "Jane Doe", "2026-10-13T00:00:00Z": "Ali Kaya",
			"2026-10-19T12:00:00Z": "", // Jane's turn, replaced by Sam's a day later
			"2026-10-20T12:00:00Z": "Sam Lee",
			"2026-10-26T12:00:00Z": "Sam Lee", // a day's event over Ali's turn
			"2026-10-28T00:00:00Z": "Ali Kaya", "2026-11-03T00:00:00Z": "Jane Doe",
			"2026-11-10T00:00:00Z": "Ali Kaya", "2026-11-17T00:00:00Z": "", "2026-11-24T00:00:00Z": "Ali Kaya", // 16 November left out
			"2026-12-01T00:00:00Z": "Jane Doe", // UNTIL 1 December takes the 30 November turn
			"2026-12-08T00:00:00Z": "",         // and no more: COUNT=4 stopped Ali's too
		}, 9, []string{"Korea Standard Time", "FREQ=MONTHLY"}},
	} {
		t.Run(c.file, func(t *testing.T) {
			cfg := config(t, c.schedule)
			f, err := Parse(feed(t, c.file), cfg.Schedules[0], cfg, now)
			if err != nil {
				t.Fatal(err)
			}
			for at, who := range c.want {
				if got := whoAt(f.Turns, at); got != who {
					t.Errorf("at %s: %q, want %q", at, got, who)
				}
			}
			if len(f.Turns) != c.turns {
				t.Errorf("%d turns: %+v", len(f.Turns), f.Turns)
			}
			notes := strings.Join(f.Notes, "\n")
			for _, n := range c.notes {
				if !strings.Contains(notes, n) {
					t.Errorf("not said: %q, in %q", n, notes)
				}
			}
			if c.notes == nil && len(f.Notes) > 0 {
				t.Errorf("notes: %q", f.Notes)
			}
			for i := 1; i < len(f.Turns); i++ {
				if f.Turns[i].From.Before(f.Turns[i-1].To) {
					t.Errorf("turns overlap: %+v, %+v", f.Turns[i-1], f.Turns[i])
				}
			}
		})
	}
}

// What a feed's editor could write, refused or bounded: a body that isn't
// a calendar, isn't UTF-8, is too large, has a line too long, too many
// events or repeats; a zone given as a path; a repeat stepping from long
// ago; names that don't show.
func TestFeedRefused(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	s := cfg.Schedules[0]
	ev := func(body string) []byte {
		return []byte("BEGIN:VCALENDAR\r\n" + body + "END:VCALENDAR\r\n")
	}
	for name, c := range map[string]struct {
		body []byte
		says string
	}{
		"a sign-in page":  {[]byte("<html><body>Sign in</body></html>"), "BEGIN:VCALENDAR"},
		"Grafana's error": {[]byte(`{"detail":"Invalid token."}`), "BEGIN:VCALENDAR"},
		"not UTF-8":       {ev("BEGIN:VEVENT\r\nSUMMARY:\xff\xfe\r\nEND:VEVENT\r\n"), "UTF-8"},
		"too large":       {append(ev(""), bytes.Repeat([]byte("X"), MaxFeed)...), "over 8 MB"},
		"a line too long": {ev("X-LONG:" + strings.Repeat("a", maxLine) + "\r\n"), "line"},
		"too many events": {ev(strings.Repeat("BEGIN:VEVENT\r\nEND:VEVENT\r\n", maxEvents+1)), "more than"},
		"too many turns":  {ev(strings.Repeat("BEGIN:VEVENT\r\nDTSTART:20261001T000000Z\r\nDTEND:20261001T000001Z\r\nRRULE:FREQ=DAILY\r\nSUMMARY:x\r\nEND:VEVENT\r\n", 600)), "more than"},
	} {
		if _, err := Parse(c.body, s, cfg, now); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want an error saying %q", name, err, c.says)
		}
	}
	// Passed over, and said, while the rest is read.
	f, err := Parse(ev(
		"BEGIN:VEVENT\r\nDTSTART;TZID=../../../etc/passwd:20261101T000000\r\nDTEND:20261102T000000Z\r\nSUMMARY:path\r\nEND:VEVENT\r\n"+
			"BEGIN:VEVENT\r\nDTSTART:19000101T000000Z\r\nDTEND:19000102T000000Z\r\nSUMMARY:old\r\nEND:VEVENT\r\n"+
			"BEGIN:VEVENT\r\nDTSTART:20000101T000000Z\r\nDTEND:20000101T010000Z\r\nRRULE:FREQ=WEEKLY;BYSETPOS=1\r\nSUMMARY:setpos\r\nEND:VEVENT\r\n"+
			"BEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261102T000000Z\r\nSUMMARY:Jane‮Doe\r\nEND:VEVENT\r\n"+
			"BEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDURATION:P9999D\r\nSUMMARY:long\r\nEND:VEVENT\r\n"+
			"BEGIN:VEVENT\r\nDTSTART:20261103T000000Z\r\nDTEND:20261104T000000Z\r\nATTENDEE:mailto:sam@example.com\r\nSUMMARY:fine\r\nEND:VEVENT\r\n"),
		s, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Turns) != 1 || f.Turns[0].Who != "Sam Lee" {
		t.Errorf("turns %+v", f.Turns)
	}
	notes := strings.Join(f.Notes, "\n")
	for _, n := range []string{"etc/passwd", "1900", "BYSETPOS", "no one named", "duration"} {
		if !strings.Contains(notes, n) {
			t.Errorf("not said: %q, in %q", n, notes)
		}
	}
	// A daily repeat from 1970 is stepped through, and bounded.
	f, err = Parse(ev("BEGIN:VEVENT\r\nDTSTART:19700101T000000Z\r\nDTEND:19700101T010000Z\r\nRRULE:FREQ=DAILY\r\nSUMMARY:sam@example.com\r\nEND:VEVENT\r\n"), s, cfg, now)
	if err != nil || len(f.Turns) == 0 || len(f.Turns) > 600 {
		t.Errorf("a daily repeat from 1970: %v, %d turns", err, len(f.Turns))
	}
}

// The file is checked as it is read: names, sources, auth, who, zones and
// people's hours.
func TestLoadRefused(t *testing.T) {
	for name, c := range map[string]struct{ body, says string }{
		"no schedules":         {"schedules: []\n", "schedules"},
		"a bad name":           {"schedules:\n  - {name: Pay ments, file: x}\n", "lowercase"},
		"two sources":          {"schedules:\n  - {name: p, file: x, url: 'https://x.example/a.ics'}\n", "one of"},
		"http":                 {"schedules:\n  - {name: p, url: 'http://x.example/a.ics'}\n", "https"},
		"a user in the URL":    {"schedules:\n  - {name: p, url: 'https://u:p@x.example/a.ics'}\n", "no user"},
		"an unknown key":       {"schedules:\n  - {name: p, file: x, password: y}\n", "password"},
		"auth without token":   {"schedules:\n  - {name: p, url: 'https://x.example/a.ics', auth: genie}\n", "token_file"},
		"basic without user":   {"schedules:\n  - {name: p, url: 'https://x.example/a.ics', auth: basic, token_file: t}\n", "user"},
		"who without group":    {"schedules:\n  - {name: p, file: x, who: 'On Call'}\n", "group"},
		"a zone as a path":     {"schedules:\n  - {name: p, file: x, tz: '../../etc/passwd'}\n", "time zone"},
		"every too often":      {"every: 1s\nschedules:\n  - {name: p, file: x}\n", "every"},
		"bad hours":            {"schedules:\n  - {name: p, file: x}\npeople:\n  - {name: A, work: 'all day'}\n", "work"},
		"one name, two people": {"schedules:\n  - {name: p, file: x}\npeople:\n  - {name: A, also: [x@example.com]}\n  - {name: B, also: [X@example.com]}\n", "also"},
	} {
		if _, err := Load(write(t, "c.yaml", c.body)); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want one saying %q", name, err, c.says)
		}
	}
}

// A feed is fetched from its URL only, its secret never said: https only;
// no redirect followed; a token in a header, as the schedule's auth says;
// the URL read from its file each time.
func TestRead(t *testing.T) {
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+" "+r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/private/SECRETKEY/feed":
			w.Header().Set("Content-Type", "text/calendar")
			w.Write(feed(t, "pagerduty.ics"))
		case "/moved/SECRETKEY":
			http.Redirect(w, r, "https://elsewhere.example/not_found", http.StatusFound)
		default:
			http.Error(w, "no", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = Client().CheckRedirect
	urlFile := write(t, "url", strings.Replace(srv.URL, "https://", "webcal://", 1)+"/private/SECRETKEY/feed\n")
	tok := write(t, "token", "genie-key\n")
	cfg := config(t, "  - {name: payments, url_file: "+urlFile+"}\n  - {name: platform, url: '"+srv.URL+"/x.ics', token_file: "+tok+", auth: genie}\n")
	f, err := cfg.Fetch(context.Background(), cfg.Schedules[0], client, "lapilli/test", now)
	if err != nil || len(f.Turns) != 5 {
		t.Fatalf("%v %+v", err, f)
	}
	if _, err := cfg.Schedules[1].Read(context.Background(), client, "lapilli/test"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("a 403: %v", err)
	}
	if got[1] != "/x.ics GenieKey genie-key" {
		t.Errorf("asked %q", got)
	}
	os.WriteFile(urlFile, []byte(srv.URL+"/moved/SECRETKEY"), 0o600)
	_, err = cfg.Schedules[0].Read(context.Background(), client, "lapilli/test")
	if err == nil || !strings.Contains(err.Error(), "redirect to elsewhere.example") || strings.Contains(err.Error(), "SECRETKEY") {
		t.Errorf("a redirect: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("a redirect followed: %q", got)
	}
	os.WriteFile(urlFile, []byte("http://x.example/private/SECRETKEY/feed"), 0o600)
	if _, err := cfg.Schedules[0].Read(context.Background(), client, "lapilli/test"); err == nil || strings.Contains(err.Error(), "SECRETKEY") {
		t.Errorf("http: %v", err)
	}
	// An error fetching says the host, not the URL and its secret.
	os.WriteFile(urlFile, []byte("https://127.0.0.1:1/private/SECRETKEY/feed"), 0o600)
	if _, err := cfg.Schedules[0].Read(context.Background(), client, "lapilli/test"); err == nil || strings.Contains(err.Error(), "SECRETKEY") || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("a failed fetch: %v", err)
	}
}

// Shifts on-call=NAME take turns by the schedule's rota: each turn by its
// person's hours, the team's for anyone else and between turns.
func TestOnCallHours(t *testing.T) {
	cfg := config(t, "  - {name: oncall-hours-test, file: x}\n")
	f, err := Parse(feed(t, "pagerduty.ics"), cfg.Schedules[0], cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	base := verdict.DefaultHours(time.UTC)
	h := *base
	sh, err := verdict.ParseShifts("on-call=oncall-hours-test", &h)
	if err != nil {
		t.Fatal(err)
	}
	h.Shifts = sh
	before := h.Shifts.Key()
	// Before the feed is read: the team's hours.
	if got := h.Of(utc("2026-10-20T00:30:00Z")); got != verdict.Asleep {
		t.Errorf("before it is read: %s", got)
	}
	sh.OnCall.Set(verdict.NewRota(f.Turns, cfg.HoursOf()))
	if h.Shifts.Key() == before {
		t.Error("the key didn't change with the rota")
	}
	// 2026-10-20 00:30 UTC is 09:30 in Seoul, a Tuesday: Jane at work.
	if got := h.Of(utc("2026-10-20T00:30:00Z")); got != verdict.AtWork {
		t.Errorf("Jane's turn at 09:30 in Seoul: %s", got)
	}
	// 2026-10-20 15:00 UTC is midnight in Seoul: Jane asleep (23:00-07:00).
	if got := h.Of(utc("2026-10-20T15:00:00Z")); got != verdict.Asleep {
		t.Errorf("Jane's turn at midnight in Seoul: %s", got)
	}
	// Ali's turn, 2026-10-29 06:00 UTC: 09:00 in Istanbul, at work.
	if got := h.Of(utc("2026-10-29T06:00:00Z")); got != verdict.AtWork {
		t.Errorf("Ali's turn at 09:00 in Istanbul: %s", got)
	}
	// After every turn: the team's, UTC, 03:00 asleep.
	if got := h.Of(utc("2026-11-20T03:00:00Z")); got != verdict.Asleep {
		t.Errorf("after the turns: %s", got)
	}
	// Spans agree with Of, minute by minute.
	from, to := utc("2026-10-18T00:00:00Z"), utc("2026-11-12T00:00:00Z")
	of := map[int64]string{}
	for _, sp := range h.Spans(from, to) {
		for t := sp.From; t.Before(sp.To); t = t.Add(time.Minute) {
			of[t.Unix()] = sp.Of
		}
	}
	for at := from; at.Before(to); at = at.Add(7 * time.Minute) {
		want := h.Of(at)
		got := of[at.Unix()]
		if got == "" {
			got = verdict.AtWork
		}
		if got != want {
			t.Fatalf("at %s: spans say %s, Of %s", at, got, want)
		}
	}
	if !h.Sleeps() {
		t.Error("hours with Jane asleep don't sleep")
	}
}
