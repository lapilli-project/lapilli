package main

import (
	"strings"
	"testing"
	"time"
)

// --shutdown-delay is how long serve, told to stop, answers on first: a
// duration from 0 to 10m, said in -h, and refused otherwise.
func TestServeShutdownDelayFlag(t *testing.T) {
	parallel(t)
	rules := write(t, "rules.yaml", "groups: []\n")
	st, err := readServe([]string{"--prometheus", "http://127.0.0.1:1", "--rules", rules, "--shutdown-delay", "5s"}, true)
	if err != nil || st.cfg.ShutdownDelay != 5*time.Second {
		t.Errorf("5s: %v %v", st.cfg.ShutdownDelay, err)
	}
	if st, err := readServe([]string{"--prometheus", "http://127.0.0.1:1", "--rules", rules}, true); err != nil || st.cfg.ShutdownDelay != 0 {
		t.Errorf("by default: %v %v", st.cfg.ShutdownDelay, err)
	}
	for _, c := range []struct{ val, want string }{{"11m", "at most 10m"}, {"-1s", "--shutdown-delay -1s"}, {"soon", "--shutdown-delay soon"}} {
		var out, errs strings.Builder
		code := run([]string{"serve", "--prometheus", "http://127.0.0.1:1", "--rules", rules, "--shutdown-delay", c.val}, &out, &errs)
		if code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d, want 2 and %q\n%s", c.val, code, c.want, errs.String())
		}
	}
	if !strings.Contains(serveUsage, "--shutdown-delay DUR") {
		t.Error("not in -h")
	}
}
