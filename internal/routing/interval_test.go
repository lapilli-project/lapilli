package routing

import (
	"strings"
	"testing"
	"time"
)

func parse(t testing.TB, v any) TimeInterval {
	t.Helper()
	ti, err := ParseTimeInterval(v)
	if err != nil {
		t.Fatal(err)
	}
	return ti
}

// The parts of a time interval, one by one, and their edges: the end of a
// range of times is out and 24:00 is the day's end; a wrapped weekday range;
// days of the month counted from the end, and one a month hasn't; months
// and years; a time zone that puts the day on the other side of UTC's.
func TestTimeIntervalParts(t *testing.T) {
	u := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name string
		ti   map[string]any
		in   []time.Time
		out  []time.Time
	}{
		{"times", map[string]any{"times": []any{map[string]any{"start_time": "09:00", "end_time": "17:30"}}},
			[]time.Time{u(2026, 1, 5, 9, 0), u(2026, 1, 5, 17, 29)}, []time.Time{u(2026, 1, 5, 8, 59), u(2026, 1, 5, 17, 30)}},
		{"24:00", map[string]any{"times": []any{map[string]any{"start_time": "23:00", "end_time": "24:00"}}},
			[]time.Time{u(2026, 1, 5, 23, 59)}, []time.Time{u(2026, 1, 6, 0, 0)}},
		{"weekdays", map[string]any{"weekdays": []any{"monday:wednesday", "saturday"}},
			[]time.Time{u(2026, 1, 5, 0, 0), u(2026, 1, 7, 23, 59), u(2026, 1, 10, 12, 0)}, []time.Time{u(2026, 1, 8, 0, 0), u(2026, 1, 11, 0, 0)}},
		{"a weekday range that wraps", map[string]any{"weekdays": []any{"friday:monday"}},
			[]time.Time{u(2026, 1, 9, 12, 0), u(2026, 1, 11, 12, 0), u(2026, 1, 5, 12, 0)}, []time.Time{u(2026, 1, 6, 12, 0), u(2026, 1, 8, 12, 0)}},
		{"days of the month", map[string]any{"days_of_month": []any{"1:3", "-2:-1"}},
			[]time.Time{u(2026, 2, 1, 5, 0), u(2026, 2, 3, 5, 0), u(2026, 2, 27, 5, 0), u(2026, 2, 28, 5, 0), u(2024, 2, 29, 5, 0), u(2026, 1, 31, 5, 0)},
			[]time.Time{u(2026, 2, 4, 5, 0), u(2026, 2, 26, 5, 0), u(2024, 2, 27, 5, 0), u(2026, 1, 29, 5, 0)}},
		{"a day a month hasn't", map[string]any{"days_of_month": []any{"31"}},
			[]time.Time{u(2026, 1, 31, 5, 0), u(2026, 3, 31, 5, 0)}, []time.Time{u(2026, 4, 30, 5, 0), u(2026, 2, 28, 5, 0)}},
		{"the last day", map[string]any{"days_of_month": []any{"-1"}},
			[]time.Time{u(2026, 2, 28, 5, 0), u(2024, 2, 29, 5, 0), u(2026, 4, 30, 5, 0)}, []time.Time{u(2024, 2, 28, 5, 0), u(2026, 4, 29, 5, 0)}},
		{"months", map[string]any{"months": []any{"december", "1:2", "6"}},
			[]time.Time{u(2026, 12, 1, 0, 0), u(2026, 1, 31, 23, 59), u(2026, 2, 28, 0, 0), u(2026, 6, 15, 0, 0)}, []time.Time{u(2026, 3, 1, 0, 0), u(2026, 11, 30, 23, 59)}},
		{"years", map[string]any{"years": []any{"2025:2026", 2030}},
			[]time.Time{u(2025, 1, 1, 0, 0), u(2026, 12, 31, 23, 59), u(2030, 6, 1, 0, 0)}, []time.Time{u(2024, 12, 31, 23, 59), u(2027, 1, 1, 0, 0)}},
		{"all parts", map[string]any{"times": []any{map[string]any{"start_time": "09:00", "end_time": "10:00"}}, "weekdays": []any{"monday"}, "months": []any{"1"}},
			[]time.Time{u(2026, 1, 5, 9, 30)}, []time.Time{u(2026, 1, 5, 10, 0), u(2026, 1, 6, 9, 30), u(2026, 2, 2, 9, 30)}},
		{"none of the parts", map[string]any{}, []time.Time{u(2026, 1, 5, 9, 30), u(2031, 8, 7, 0, 0)}, nil},
		// 09:00 to 10:00 in Seoul, nine hours ahead, is 00:00 to 01:00 UTC;
		// its Monday starts on Sunday in UTC.
		{"a time zone", map[string]any{"location": "Asia/Seoul", "weekdays": []any{"monday"}, "times": []any{map[string]any{"start_time": "09:00", "end_time": "10:00"}}},
			[]time.Time{u(2026, 1, 5, 0, 0), u(2026, 1, 5, 0, 59)}, []time.Time{u(2026, 1, 5, 9, 0), u(2026, 1, 4, 0, 30), u(2026, 1, 5, 1, 0)}},
		// New York's January 31 begins at 05:00 UTC; the month's end moves with it.
		{"a time zone's last day", map[string]any{"location": "America/New_York", "days_of_month": []any{"-1"}},
			[]time.Time{u(2026, 2, 1, 4, 59), u(2026, 1, 31, 5, 0)}, []time.Time{u(2026, 2, 1, 5, 0), u(2026, 1, 31, 4, 59)}},
	} {
		ti := parse(t, tc.ti)
		for _, x := range tc.in {
			if !ti.ContainsTime(x) {
				t.Errorf("%s: doesn't contain %v", tc.name, x)
			}
		}
		for _, x := range tc.out {
			if ti.ContainsTime(x) {
				t.Errorf("%s: contains %v", tc.name, x)
			}
		}
	}
	// A list that is there and empty holds nothing; one that isn't is any.
	if parse(t, map[string]any{"weekdays": []any{}}).ContainsTime(t0) || !parse(t, map[string]any{}).ContainsTime(t0) {
		t.Error("an empty list is any")
	}
}

// What Alertmanager won't load.
func TestTimeIntervalErrors(t *testing.T) {
	for what, v := range map[string]any{
		"a backwards range of times":       map[string]any{"times": []any{map[string]any{"start_time": "10:00", "end_time": "09:00"}}},
		"a time that isn't one":            map[string]any{"times": []any{map[string]any{"start_time": "9:00", "end_time": "10:00"}}},
		"a missing end":                    map[string]any{"times": []any{map[string]any{"start_time": "09:00"}}},
		"a weekday that isn't one":         map[string]any{"weekdays": []any{"funday"}},
		"a day of the month, zero":         map[string]any{"days_of_month": []any{"0"}},
		"a day past the month's end":       map[string]any{"days_of_month": []any{"32"}},
		"a negative start, a positive end": map[string]any{"days_of_month": []any{"-3:5"}},
		"a day range backwards":            map[string]any{"days_of_month": []any{"20:10"}},
		"a month backwards":                map[string]any{"months": []any{"march:january"}},
		"a month of thirteen":              map[string]any{"months": []any{"13"}},
		"a year backwards":                 map[string]any{"years": []any{"2026:2025"}},
		"a place that isn't one":           map[string]any{"location": "Nowhere/Atlantis"},
		"a list that isn't":                map[string]any{"weekdays": "monday"},
		"not a map":                        []any{},
	} {
		if _, err := ParseTimeInterval(v); err == nil {
			t.Errorf("accepted %s", what)
		}
	}
}

// The next time a time interval's answer changes, which is when a muted
// group's alerts are let through; looked for as far as asked.
func TestNextChange(t *testing.T) {
	u := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, time.UTC) }
	work := []TimeInterval{parse(t, map[string]any{"weekdays": []any{"monday:friday"}, "times": []any{map[string]any{"start_time": "09:00", "end_time": "17:00"}}})}
	for _, tc := range []struct {
		from, want time.Time
	}{
		{u(2026, 1, 5, 8, 0), u(2026, 1, 5, 9, 0)},   // Monday morning: in at nine
		{u(2026, 1, 5, 9, 0), u(2026, 1, 5, 17, 0)},  // in: out at five
		{u(2026, 1, 9, 17, 0), u(2026, 1, 12, 9, 0)}, // Friday evening: Monday
		{u(2026, 1, 10, 3, 0), u(2026, 1, 12, 9, 0)},
	} {
		if got := nextChange(work, tc.from, 30*24*time.Hour); !got.Equal(tc.want) {
			t.Errorf("from %v: %v, want %v", tc.from, got, tc.want)
		}
	}
	// A year interval changes at the new year, far on; not found if not asked.
	y := []TimeInterval{parse(t, map[string]any{"years": []any{"2027"}})}
	if got := nextChange(y, u(2026, 3, 1, 0, 0), 400*24*time.Hour); !got.Equal(u(2027, 1, 1, 0, 0)) {
		t.Errorf("a year: %v", got)
	}
	if got := nextChange(y, u(2026, 3, 1, 0, 0), 30*24*time.Hour); !got.IsZero() {
		t.Errorf("a year, not looked for: %v", got)
	}
	// In a time zone, its minutes: Seoul's nine is UTC's midnight.
	seoul := []TimeInterval{parse(t, map[string]any{"location": "Asia/Seoul", "times": []any{map[string]any{"start_time": "09:00", "end_time": "10:00"}}})}
	if got := nextChange(seoul, u(2026, 1, 5, 12, 0), 48*time.Hour); !got.Equal(u(2026, 1, 6, 0, 0)) {
		t.Errorf("Seoul: %v", got)
	}
	// Several names' intervals as one: either contains it.
	if got := nextChange(append(work, seoul...), u(2026, 1, 5, 8, 0), 48*time.Hour); !got.Equal(u(2026, 1, 5, 9, 0)) {
		t.Errorf("two: %v", got)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30s": 30 * time.Second, "5m": 5 * time.Minute, "4h": 4 * time.Hour, "1h30m": 90 * time.Minute, "1d": 24 * time.Hour,
		"2w": 14 * 24 * time.Hour, "500ms": 500 * time.Millisecond, "1y": 365 * 24 * time.Hour, "0": 0, "1m30s": 90 * time.Second,
	} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("%q: %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "5", "m", "-5m", "1.5h", "30m1h", "1h1h", "5x", "99999999999y", "1 h"} {
		if got, err := ParseDuration(in); err == nil {
			t.Errorf("%q accepted: %v", in, got)
		}
	}
	if _, err := ParseDuration(strings.Repeat("9", 30) + "s"); err == nil {
		t.Error("a huge number")
	}
}

// Silences as the API lists them: only matchers, start, end and id are read.
func TestReadSilences(t *testing.T) {
	good := `[
	  {"id": "a1", "status": {"state": "expired"}, "comment": "secret plans", "createdBy": "someone@example.com", "updatedAt": "2026-01-01T00:00:00Z",
	   "startsAt": "2026-01-05T10:00:00.5Z", "endsAt": "2026-01-05T12:00:00Z",
	   "matchers": [{"name": "alertname", "value": "A", "isRegex": false, "isEqual": true}, {"name": "sev", "value": "p.*", "isRegex": true},
	                {"name": "env", "value": "dev", "isRegex": false, "isEqual": false}, {"name": "team", "value": "a|b", "isRegex": true, "isEqual": false}]}
	]`
	s, err := ReadSilences([]byte(good))
	if err != nil || len(s) != 1 {
		t.Fatal(s, err)
	}
	m := s[0].Matchers
	if s[0].ID != "a1" || !s[0].Start.Equal(t0.Add(500*time.Millisecond)) || !s[0].End.Equal(t0.Add(2*time.Hour)) || len(m) != 4 {
		t.Fatalf("%+v", s[0])
	}
	if m[0].Op != OpEqual || m[1].Op != OpRegexp || m[2].Op != OpNotEqual || m[3].Op != OpNotRegex {
		t.Errorf("operators: %v", m)
	}
	if !matchAll(m, map[string]string{"alertname": "A", "sev": "page", "env": "prod", "team": "c"}) || matchAll(m, map[string]string{"alertname": "A", "sev": "page", "env": "dev", "team": "c"}) {
		t.Error("the matchers' meaning")
	}
	if strings.Contains(strings.ToLower(strings.Join([]string{s[0].ID, m[0].Name}, "")), "secret") {
		t.Error("a comment was kept")
	}
	for what, in := range map[string]string{
		"not JSON":              `{"silences": []}`,
		"no id":                 `[{"startsAt": "2026-01-05T10:00:00Z", "endsAt": "2026-01-05T11:00:00Z", "matchers": [{"name": "a", "value": "b", "isRegex": false}]}]`,
		"no matchers":           `[{"id": "x", "startsAt": "2026-01-05T10:00:00Z", "endsAt": "2026-01-05T11:00:00Z", "matchers": []}]`,
		"ends before it starts": `[{"id": "x", "startsAt": "2026-01-05T10:00:00Z", "endsAt": "2026-01-05T09:00:00Z", "matchers": [{"name": "a", "value": "b", "isRegex": false}]}]`,
		"a bad time":            `[{"id": "x", "startsAt": "yesterday", "endsAt": "2026-01-05T09:00:00Z", "matchers": [{"name": "a", "value": "b", "isRegex": false}]}]`,
		"a bad regex":           `[{"id": "x", "startsAt": "2026-01-05T10:00:00Z", "endsAt": "2026-01-05T11:00:00Z", "matchers": [{"name": "a", "value": "(", "isRegex": true}]}]`,
		"no isRegex":            `[{"id": "x", "startsAt": "2026-01-05T10:00:00Z", "endsAt": "2026-01-05T11:00:00Z", "matchers": [{"name": "a", "value": "b"}]}]`,
		"two documents":         `[] []`,
	} {
		if _, err := ReadSilences([]byte(in)); err == nil {
			t.Errorf("accepted %s", what)
		}
	}
}
