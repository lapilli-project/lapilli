package oncall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/ledger"
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
			f, err := Parse(context.Background(), feed(t, c.file), cfg.Schedules[0], cfg, now)
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
		if _, err := Parse(context.Background(), c.body, s, cfg, now); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want an error saying %q", name, err, c.says)
		}
	}
	// Passed over, and said, while the rest is read.
	f, err := Parse(context.Background(), ev(
		"BEGIN:VEVENT\r\nDTSTART;TZID=../../../etc/passwd:20261101T000000\r\nDTEND:20261102T000000Z\r\nSUMMARY:path\r\nEND:VEVENT\r\n"+
			"BEGIN:VEVENT\r\nDTSTART:19000101T000000Z\r\nDTEND:19000102T000000Z\r\nSUMMARY:old\r\nEND:VEVENT\r\n"+
			"BEGIN:VEVENT\r\nDTSTART:20000101T000000Z\r\nDTEND:20000101T010000Z\r\nRRULE:FREQ=WEEKLY;BYSETPOS=1\r\nSUMMARY:setpos\r\nEND:VEVENT\r\n"+
			"BEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261102T000000Z\r\nSUMMARY:Jane\u202eDoe\r\nEND:VEVENT\r\n"+
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
	f, err = Parse(context.Background(), ev("BEGIN:VEVENT\r\nDTSTART:19700101T000000Z\r\nDTEND:19700101T010000Z\r\nRRULE:FREQ=DAILY\r\nSUMMARY:sam@example.com\r\nEND:VEVENT\r\n"), s, cfg, now)
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
		"a person twice":       {"schedules:\n  - {name: p, file: x}\npeople:\n  - {name: Kim, tz: Asia/Seoul}\n  - {name: Kim, tz: America/New_York}\n", "twice"},
		"a name too long":      {"schedules:\n  - {name: p, file: x}\npeople:\n  - {name: " + strings.Repeat("\u00e9", 101) + "}\n", "bytes"},
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
	f, err := Parse(context.Background(), feed(t, "pagerduty.ics"), cfg.Schedules[0], cfg, now)
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

// What the reviews found of the reader, each a test: WKST is honoured, as
// RFC 5545's own example shows (L1); a feed's repeats and their dates left
// out can't take minutes to read, and a read stops when asked (M13); a
// turn whose name the ledger won't keep is passed over, not the feed (L2);
// an event far outside the span read is no turn (L3); a feed past the
// turns it may make is refused, not cut short quietly (L5).
func TestFeedBounds(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	s := cfg.Schedules[0]
	ev := func(body string) []byte { return []byte("BEGIN:VCALENDAR\r\n" + body + "END:VCALENDAR\r\n") }
	ctx := context.Background()

	// RFC 5545, 3.8.5.3: every other week on Tuesday and Sunday, four
	// times, the week starting Monday, then Sunday.
	for wkst, want := range map[string][]string{
		"MO": {"1997-08-05", "1997-08-10", "1997-08-19", "1997-08-24"},
		"SU": {"1997-08-05", "1997-08-17", "1997-08-19", "1997-08-31"},
	} {
		f, err := Parse(ctx, ev("BEGIN:VEVENT\r\nDTSTART;TZID=America/New_York:19970805T090000\r\nDURATION:PT1H\r\n"+
			"RRULE:FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST="+wkst+"\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n"), s, cfg, time.Date(1997, 8, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, x := range f.Turns {
			got = append(got, x.From.In(time.FixedZone("EDT", -4*3600)).Format("2006-01-02"))
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("WKST=%s: %v, want %v", wkst, got, want)
		}
	}

	// Dates left out: a thousand, and more refuse the event.
	var ex strings.Builder
	for i := range 1001 {
		fmt.Fprintf(&ex, "EXDATE:%sT000000Z\r\n", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("20060102"))
	}
	f, err := Parse(ctx, ev("BEGIN:VEVENT\r\nDTSTART:20250101T000000Z\r\nDTEND:20250101T010000Z\r\nRRULE:FREQ=DAILY\r\n"+ex.String()+"SUMMARY:Sam Lee\r\nEND:VEVENT\r\n"), s, cfg, now)
	if err != nil || len(f.Turns) != 0 || !strings.Contains(strings.Join(f.Notes, " "), "more than 1000 dates") {
		t.Errorf("1,001 dates left out: %v %d %q", err, len(f.Turns), f.Notes)
	}

	// Twenty thousand daily repeats from 1970: refused, at once.
	var many strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&many, "BEGIN:VEVENT\r\nUID:%d\r\nDTSTART:19700101T000000Z\r\nDTEND:19700101T010000Z\r\nRRULE:FREQ=DAILY\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n", i)
	}
	began := time.Now()
	if _, err := Parse(ctx, ev(many.String()), s, cfg, now); err == nil || time.Since(began) > 5*time.Second {
		t.Errorf("20,000 daily repeats from 1970: %v, in %s", err, time.Since(began))
	}
	// Repeats counted from 1970 that end before the span read take their
	// steps all the same: bounded in all, not each.
	var counted strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&counted, "BEGIN:VEVENT\r\nUID:c%d\r\nDTSTART:19700101T000000Z\r\nDTEND:19700101T010000Z\r\nRRULE:FREQ=DAILY;COUNT=19000\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n", i)
	}
	began = time.Now()
	if _, err := Parse(ctx, ev(counted.String()), s, cfg, now); err == nil || !strings.Contains(err.Error(), "steps") || time.Since(began) > 5*time.Second {
		t.Errorf("2,000 counted repeats from 1970: %v, in %s", err, time.Since(began))
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Parse(cancelled, ev(many.String()), s, cfg, now); !errors.Is(err, context.Canceled) {
		t.Errorf("a read asked to stop: %v", err)
	}
	var single strings.Builder
	for i := range 300 {
		fmt.Fprintf(&single, "BEGIN:VEVENT\r\nUID:s%d\r\nDTSTART:20261101T000000Z\r\nDTEND:20261101T010000Z\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n", i)
	}
	if _, err := Parse(cancelled, ev(single.String()), s, cfg, now); !errors.Is(err, context.Canceled) {
		t.Errorf("a read of single events asked to stop: %v", err)
	}

	// Past the turns a feed may make, refused: 109 daily repeats over the
	// span read, the last one's cut short before.
	var lots strings.Builder
	for i := range 109 {
		fmt.Fprintf(&lots, "BEGIN:VEVENT\r\nUID:%d\r\nDTSTART:20250101T%02d0000Z\r\nDTEND:20250101T%02d0001Z\r\nRRULE:FREQ=DAILY\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n", i, i%24, i%24)
	}
	if _, err := Parse(ctx, ev(lots.String()), s, cfg, now); err == nil || !strings.Contains(err.Error(), "turns") {
		t.Errorf("past the turns a feed may make: %v", err)
	}

	// A name the ledger wouldn't keep, and an event from 1970: passed over.
	f, err = Parse(ctx, ev("BEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261102T000000Z\r\nSUMMARY:Ja\u200dne\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:19700101T000000Z\r\nDTEND:19700102T000000Z\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:20261102T000000Z\r\nDTEND:20261103T000000Z\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n"), s, cfg, now)
	if err != nil || len(f.Turns) != 1 || f.Turns[0].Who != "Sam Lee" || f.From.Year() == 1970 {
		t.Errorf("a name that hides, and 1970: %v %+v from %s", err, f.Turns, f.From)
	}
}

// A turn's name follows the ledger's rule for names: an emoji joined, or
// Persian written with its non-joiner, is a name; a soft hyphen hides.
func TestFeedNamesAsLedger(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261102T000000Z\r\nSUMMARY:Ana \U0001F469\u200D\U0001F4BB\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:20261102T000000Z\r\nDTEND:20261103T000000Z\r\nSUMMARY:\u0645\u06CC\u200C\u062E\u0648\u0627\u0647\u0645\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:20261103T000000Z\r\nDTEND:20261104T000000Z\r\nSUMMARY:Ja\u00ADne\r\nEND:VEVENT\r\n"+
		"END:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now)
	if err != nil || len(f.Turns) != 2 {
		t.Errorf("%v: %+v", err, f)
	}
}

// Who an event names, in order (F1 of the verification): an attendee in
// the file; what who takes out of the summary, or the summary, when that
// is someone in the file, or what who took out though it isn't; then a
// lone attendee the file doesn't know (PagerDuty lists one on every turn,
// so it comes after the summary); then the summary.
func TestWhoOrder(t *testing.T) {
	pd := "  - {name: s, file: x, who: '^On Call - (.+?) - '}\n"
	plain := "  - {name: s, file: x}\n"
	for _, c := range []struct{ name, schedule, event, want string }{
		{"who takes a person in the file", pd, "ATTENDEE:mailto:a.kaya@pagerduty.example\r\nSUMMARY:On Call - Ali Kaya - Payments Primary\r\n", "Ali Kaya"},
		{"who takes someone not in the file", pd, "ATTENDEE:mailto:k.min@pagerduty.example\r\nSUMMARY:On Call - Kim Min - Payments\r\n", "Kim Min"},
		{"the summary is a person in the file", plain, "ATTENDEE:mailto:x@example.com\r\nSUMMARY:Jane Doe\r\n", "Jane Doe"},
		{"an attendee in the file", plain, "ATTENDEE:mailto:sam@example.com\r\nSUMMARY:Jane Doe\r\n", "Sam Lee"},
		{"a lone attendee, the summary no one", plain, "ATTENDEE:mailto:x@example.com\r\nSUMMARY:On call\r\n", "x@example.com"},
		{"the summary alone", plain, "SUMMARY:On call\r\n", "On call"},
	} {
		cfg := config(t, c.schedule)
		f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261102T000000Z\r\n"+c.event+"END:VEVENT\r\nEND:VCALENDAR\r\n"),
			cfg.Schedules[0], cfg, now)
		if err != nil || len(f.Turns) != 1 || f.Turns[0].Who != c.want {
			t.Errorf("%s: %v %+v, want %q", c.name, err, f, c.want)
		}
	}
}

// A name longer than the ledger keeps (MaxWho bytes) is cut to fit, at a
// character's end, and so it still shows: no joiner left at the end of it
// (F2).
func TestNamesClipped(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	for _, name := range []string{strings.Repeat("y", 230), strings.Repeat("\u00e9", 150), strings.Repeat("\U0001F469\u200D\U0001F4BB", 30)} {
		f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261102T000000Z\r\nSUMMARY:"+name+"\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"),
			cfg.Schedules[0], cfg, now)
		if err != nil || len(f.Turns) != 1 {
			t.Fatalf("%v %+v", err, f)
		}
		who := f.Turns[0].Who
		if len(who) > ledger.MaxWho || len(who) < ledger.MaxWho-12 || !strings.HasSuffix(who, "\u2026") || !utf8.ValidString(who) || ledger.Hidden(who) {
			t.Errorf("%q (%d bytes) kept as %q (%d bytes)", name[:12], len(name), who, len(who))
		}
	}
}

// Two people at once is said: two events that start together, the later
// in the feed holding; an event with two people of the file as attendees,
// the first's.
func TestTwoAtOnce(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261102T000000Z\r\nSUMMARY:Jane Doe\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261102T000000Z\r\nSUMMARY:Ali Kaya\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:20261103T000000Z\r\nDTEND:20261104T000000Z\r\nATTENDEE:mailto:sam@example.com\r\nATTENDEE:mailto:jane@example.com\r\nSUMMARY:x\r\nEND:VEVENT\r\n"+
		"END:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(f.Notes, "\n")
	if whoAt(f.Turns, "2026-11-01T12:00:00Z") != "Ali Kaya" || whoAt(f.Turns, "2026-11-03T12:00:00Z") != "Sam Lee" ||
		!strings.Contains(notes, `"Jane Doe" and "Ali Kaya" start at the same time`) || !strings.Contains(notes, `names 2 people in the file, "Sam Lee" first`) {
		t.Errorf("turns %+v, notes %q", f.Turns, f.Notes)
	}
}

// An event passed over is held: it doesn't speak for the feed (From), and
// the turns kept of whoever it names, from its start (over its one turn,
// for one that holds one), stay where the feed's turns aren't (F3).
func TestHold(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	var ex strings.Builder
	for i := range maxExdates + 1 {
		fmt.Fprintf(&ex, "EXDATE:%sT000000Z\r\n", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("20060102"))
	}
	f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\n"+
		"BEGIN:VEVENT\r\nUID:z\r\nDTSTART:20261025T000000Z\r\nDTEND:20261025T010000Z\r\nRRULE:FREQ=DAILY;COUNT=10\r\n"+ex.String()+"SUMMARY:Ali Kaya\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:20261102T050000Z\r\nDTEND:20261102T060000Z\r\nDURATION:PT1H\r\nSUMMARY:Jane Doe\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261101T020000Z\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n"+
		"END:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time { return utc("2026-" + s + ":00:00Z") }
	if !f.From.Equal(at("11-01T00")) || len(f.Held) != 2 {
		t.Fatalf("from %s, held %+v", f.From, f.Held)
	}
	turn := func(from, to, who string) verdict.RotaTurn {
		return verdict.RotaTurn{From: at(from), To: at(to), Who: who}
	}
	kept := []verdict.RotaTurn{turn("10-31T00", "10-31T01", "Ali Kaya"), turn("11-01T00", "11-01T01", "Ali Kaya"), turn("11-02T00", "11-02T01", "Ali Kaya"),
		turn("11-02T03", "11-02T04", "Jane Doe"), turn("11-02T05", "11-02T06", "Jane Doe"), turn("11-02T07", "11-02T08", "Jane Doe"), turn("11-03T00", "11-03T01", "Ali Kaya"), turn("11-04T00", "11-04T01", "Jane Doe")}
	var got []string
	for _, x := range Hold(kept, f.Turns, f.Held, f.From) {
		got = append(got, x.From.UTC().Format("01-02T15")+" "+x.Who)
	}
	want := []string{"11-01T00 Sam Lee", "11-02T00 Ali Kaya", "11-02T05 Jane Doe", "11-03T00 Ali Kaya"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("held: %v, want %v", got, want)
	}
	// A repeat is held from its start, not before.
	held := Hold(kept, f.Turns, []Held{{Who: "Ali Kaya", From: at("11-02T12")}}, f.From)
	if len(held) != 2 || held[1].Who != "Ali Kaya" || !held[1].From.Equal(at("11-03T00")) {
		t.Errorf("a repeat held from 2 November, noon: %+v", held)
	}
}
