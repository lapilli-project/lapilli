package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"lapilli.dev/internal/verdict"
)

// lapilli demo --serve: lapilli serve on the sample, by its own code
// (docs/design/demo.md, "--serve").

// demoListen is where lapilli demo --serve listens without --listen: a free
// port of this machine's loopback, which it says as it starts.
const demoListen = "127.0.0.1:0"

// demoLead is how long after a period a firing of it can still catch an
// incident: lapilli verdict's --lead, as the demo leaves it. The weekly
// digest waits for the runs to be that far past the week.
const demoLead = 15 * time.Minute

// demoServeAt is the time lapilli demo --serve pins serve's clock to: the
// end of the sample's week, and the lead after it. The first run replays
// the week from its Monday, and the digest, due at the week's end, works
// it out; the week is the last whole week the pages show.
func demoServeAt(loc *time.Location) time.Time {
	return demoWeek(loc).AddDate(0, 0, 7).Add(demoLead)
}

// demoPort is a port as --listen takes it: digits alone, so that no name of
// a service is looked up.
var demoPort = regexp.MustCompile(`^[0-9]{1,5}$`)

// loopbackOnly refuses an address to listen on that isn't a loopback
// address, written as one, and a port: a name could resolve elsewhere, and
// an address left out is every interface's.
func loopbackOnly(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("want a loopback address and a port, like 127.0.0.1:8080 or [::1]:8080")
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("the demo listens on this machine alone: want a loopback address, 127.0.0.1:PORT or [::1]:PORT, not a name or another address")
	}
	if n, err := strconv.Atoi(port); !demoPort.MatchString(port) || err != nil || n > 65535 {
		return errors.New("want a port, 0 to 65535 (0 for a free one)")
	}
	return nil
}

// demoServing is what lapilli demo --serve says once it listens, with the
// URL to open.
const demoServing = `lapilli demo --serve: lapilli serve's pages and API over the sample, read-only, reading and sending nothing. Open
  %s
Its clock stands at the end of the sample's week, the last whole week its pages show. Ctrl-C ends it, and removes its ledger.
`

// demoServe runs lapilli serve on the sample: its rules and the incidents of
// its week as files, its history answered in the process, a ledger in the
// same temporary directory, removed as it ends.
func demoServe(s *demoSample, loc *time.Location, tz, listen string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "lapilli demo: %s\n", verdict.Clean(err.Error()))
		return 1
	}
	dir, err := os.MkdirTemp("", "lapilli-demo-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	rulesFile, incidentsFile := filepath.Join(dir, "rules.yaml"), filepath.Join(dir, "incidents.yaml")
	rules, err := demoFiles.ReadFile("demo/rules.yaml")
	if err == nil {
		err = os.WriteFile(rulesFile, rules, 0o600)
	}
	week := demoWeek(loc)
	if err == nil {
		// The week's incidents alone: serve judges a week by its own and the
		// two weeks' before, and the week's keep its record. The week
		// before's would be shown missed, nothing replayed over them.
		err = os.WriteFile(incidentsFile, s.incidents(week, week.AddDate(0, 0, 7), loc), 0o600)
	}
	if err != nil {
		return fail(err)
	}
	at := demoServeAt(loc)
	code := serveOn(demoServeArgs(dir, listen, tz, loc), &serveWith{
		history:   s.history(loc),
		clock:     func() time.Time { return at },
		listening: func(a net.Addr) { fmt.Fprintf(stdout, demoServing, "http://"+a.String()+"/") },
	}, stdout, stderr)
	if err := os.RemoveAll(dir); err != nil {
		return fail(fmt.Errorf("removing its ledger: %w", err))
	}
	if code == 0 {
		fmt.Fprintln(stdout, "lapilli demo: ended; its ledger is removed.")
	}
	return code
}

// demoServeArgs are the flags lapilli demo --serve runs lapilli serve with,
// the files in dir: the rules and the incidents written there, the ledger
// kept there. The first run replays from the week's Monday to the clock,
// with nothing to settle, and the digest falls due as the week ends.
// Read-only, nothing is written but by serve's own jobs; and the replay
// isn't set beside ALERTS, which the sample hasn't. No flag names anything
// to read or send to: no Grafana, ruler, alarms, Loki, on-call feed,
// webhook, Slack, sign-in, nor another port.
func demoServeArgs(dir, listen, tz string, loc *time.Location) []string {
	week := demoWeek(loc)
	args := []string{"--rules", filepath.Join(dir, "rules.yaml"), "--incidents", filepath.Join(dir, "incidents.yaml"),
		"--match", "service", "--prometheus", demoPrometheus, "--ledger", filepath.Join(dir, "ledger"), "--listen", listen,
		"--read-only", "--check-replay=false", "--backfill", demoServeAt(loc).Sub(week).String(), "--settle", "0s", "--digest", "mon@00:00"}
	if tz != "" {
		args = append(args, "--tz", tz)
	}
	return args
}
