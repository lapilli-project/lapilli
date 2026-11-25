package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestServeQuotaFlags: the flags that keep Lapilli off the alerts' quota
// are lapilli serve's, said in its -h, and refused when given wrong.
func TestServeQuotaFlags(t *testing.T) {
	parallel(t)
	rules := write(t, "rules.yaml", "groups: []\n")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--yield-for", "1h"}, "give --yield-on-429"},
		{[]string{"--yield-on-429=maybe"}, "want true or false"},
		{[]string{"--yield-on-429", "--yield-for", "soon"}, "--yield-for soon"},
		{[]string{"--yield-on-429", "--yield-for", "25h"}, "a day at most"},
		{[]string{"--catch-up-hours", "01:00"}, "--catch-up-hours"},
		{[]string{"--catch-up-hours", "01:00-01:30"}, "shorter than --every"},
		{[]string{"--catch-up-hours", "01:00-02:00", "--every", "2h"}, "shorter than --every"},
		{[]string{"--catch-up-rate", "0"}, "--catch-up-rate 0"},
		{[]string{"--guard", " "}, "--guard"},
	} {
		var out, errs strings.Builder
		code := run(append([]string{"serve", "--prometheus", "http://127.0.0.1:1", "--rules", rules}, c.args...), &out, &errs)
		if code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%v: %d, want 2 and %q\n%s", c.args, code, c.want, errs.String())
		}
	}
	for _, flag := range []string{"--yield-on-429", "--guard", "--catch-up-hours", "--catch-up-rate"} {
		var out, errs strings.Builder
		if code := run([]string{"verdict", "--prometheus", "http://127.0.0.1:1", "--rules", rules, flag, "x"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "unknown flag") {
			t.Errorf("verdict %s: %d %s", flag, code, errs.String())
		}
	}
	var out, errs strings.Builder
	run([]string{"serve", "-h"}, &out, &errs)
	for _, flag := range []string{"--yield-on-429", "--yield-for DUR", "--guard EXPR", "--catch-up-hours HH:MM-HH:MM", "--catch-up-rate N", "docs/design/quota.md"} {
		if !strings.Contains(out.String(), flag) {
			t.Errorf("serve -h doesn't say %s", flag)
		}
	}
}

// --record-by-week is lapilli serve's: said in its -h, on by itself,
// refused by lapilli verdict, which judges each firing by its own time.
func TestServeRecordByWeekFlag(t *testing.T) {
	parallel(t)
	dir := t.TempDir()
	for given, want := range map[string]bool{"": false, "--record-by-week": true, "--record-by-week=false": false} {
		args := demoServeArgs(dir, demoListen, "UTC", time.UTC)
		args = slices.DeleteFunc(args, func(a string) bool { return a == "--record-by-week" })
		if given != "" {
			args = append(args, given)
		}
		st, err := readServe(args, false)
		if err != nil || st.cfg.RecordByWeek != want {
			t.Errorf("%q: RecordByWeek %v, %v", given, st.cfg.RecordByWeek, err)
		}
	}
	rules := write(t, "rules.yaml", "groups: []\n")
	var out, errs strings.Builder
	if code := run([]string{"verdict", "--prometheus", "http://127.0.0.1:1", "--rules", rules, "--record-by-week", "x"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "unknown flag") {
		t.Errorf("verdict --record-by-week: %d %s", code, errs.String())
	}
	out.Reset()
	run([]string{"serve", "-h"}, &out, &errs)
	if !strings.Contains(out.String(), "--record-by-week") {
		t.Error("serve -h doesn't say --record-by-week")
	}
}
