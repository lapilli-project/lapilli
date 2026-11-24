package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --keep-activity is how long the activity log is kept: 800 days unless
// given, or --keep when longer, so that a --keep past 800 days starts as
// before; 0 for all of it, as with --keep 0; never less than --keep when
// given, and refused beside --keep 0, which prunes nothing. The chart
// passes it as serve.keepActivity says, 0 too, and nothing for a release
// from before it.
func TestServeKeepActivity(t *testing.T) {
	parallel(t)
	rules := filepath.Join(t.TempDir(), "rules.yaml")
	os.WriteFile(rules, []byte("groups: []\n"), 0o644)
	base := []string{"--rules", rules, "--prometheus", "http://127.0.0.1:1", "--ledger", t.TempDir()}
	for _, c := range []struct {
		args []string
		want time.Duration
		says string
	}{
		{nil, 800 * 24 * time.Hour, ""},
		{[]string{"--keep", "1000d"}, 1000 * 24 * time.Hour, ""},
		{[]string{"--keep", "100d"}, 800 * 24 * time.Hour, ""},
		{[]string{"--keep", "0"}, 0, ""},
		{[]string{"--keep", "0", "--keep-activity", "0"}, 0, ""},
		{[]string{"--keep-activity", "0"}, 0, ""},
		{[]string{"--keep-activity", "1100d"}, 1100 * 24 * time.Hour, ""},
		{[]string{"--keep", "1000d", "--keep-activity", "1100d"}, 1100 * 24 * time.Hour, ""},
		{[]string{"--keep", "0", "--keep-activity", "30d"}, 0, "--keep-activity 720h0m0s with --keep 0"},
		{[]string{"--keep-activity", "30d"}, 0, "--keep-activity 720h0m0s is shorter than --keep"},
		{[]string{"--keep", "1000d", "--keep-activity", "900d"}, 0, "--keep-activity 21600h0m0s is shorter than --keep"},
		{[]string{"--keep-activity", "soon"}, 0, "--keep-activity soon"},
	} {
		st, err := readServe(append(append([]string{}, base...), c.args...), false)
		switch {
		case c.says != "":
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("%v: %v, want it to say %q", c.args, err, c.says)
			}
		case err != nil || st.cfg.KeepActivity != c.want:
			t.Errorf("%v: %v %v, want %v", c.args, st.cfg.KeepActivity, err, c.want)
		}
	}
	for _, c := range []struct {
		set  []string
		want string
	}{
		{nil, ""}, // serve's own
		{[]string{"--set", "serve.keepActivity=0"}, `- "--keep-activity=0"`},
		{[]string{"--set", "serve.keepActivity=1100d"}, `- "--keep-activity=1100d"`},
		{[]string{"--set", "serve.keepActivity=null"}, ""},
		{[]string{"--set", "serve.keep=1000d"}, ""}, // serve's own: 1000d
	} {
		out, err := helmTemplate(t, append(append([]string{}, serveValues...), c.set...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", c.set, err, out)
		}
		if got := strings.Contains(out, "--keep-activity"); c.want == "" && got || c.want != "" && !strings.Contains(out, c.want) {
			t.Errorf("%v: %s", c.set, out[strings.Index(out, "--keep="):][:120])
		}
	}
	// An install upgraded with serve.keep past 800 days and
	// serve.keepActivity left empty starts, the activity log kept as long.
	m, _ := render(t, append(append([]string{}, serveValues...), "--set", "serve.keep=1000d")...)
	var flags []string
	for _, a := range m.args(t)[1:] {
		flags = append(flags, strings.Replace(a, "--rules=/etc/lapilli/rules/0", "--rules="+rules, 1))
	}
	if st, err := readServe(flags, true); err != nil || st.cfg.KeepActivity != 1000*24*time.Hour {
		t.Errorf("serve.keep=1000d: %v %v\n%v", st.cfg.KeepActivity, err, flags)
	}
}
