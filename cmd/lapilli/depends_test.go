package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

const dependsDir = "testdata/depends"

// dependsHistory is a stand-in for the history --depends reads: checkout
// calls payments-db and ledger at the hours of the incidents, payments-db
// calls ledger-db, and search calls nothing. Its samples are inside the hours they count (`[1h]` looks back), in UTC,
// as the real client's are. It is asked nothing but the graph, and keeps
// the queries asked.
type dependsHistory struct{ asked *[]string }

func (h dependsHistory) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	*h.asked = append(*h.asked, fmt.Sprintf("%s @%s", expr, step))
	if !strings.Contains(expr, "istio_requests_total") {
		return nil, fmt.Errorf("asked %s", expr)
	}
	edge := func(from, to string, hours ...string) prom.Series {
		s := prom.Series{Labels: map[string]string{"source_workload": from, "destination_workload": to, "namespace": "shop"}}
		for _, at := range hours {
			t, err := time.Parse(time.RFC3339, at)
			if err != nil {
				panic(err)
			}
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t.UTC(), V: 3})
			}
		}
		return s
	}
	return []prom.Series{
		edge("checkout", "payments-db", "2026-10-06T10:30:00Z", "2026-10-08T12:30:00Z", "2026-09-01T00:00:00Z"),
		edge("checkout", "ledger", "2026-10-06T09:30:00Z"),
		edge("payments-db", "ledger-db", "2026-10-06T10:30:00Z"),
		edge("cart", "search", "2026-10-06T10:30:00Z"),
	}, nil
}

func dependsArgs(extra ...string) []string {
	args := []string{"verdict", "--firings", filepath.Join(dependsDir, "firings.jsonl"), "--incidents", filepath.Join(dependsDir, "incidents.yaml"),
		"--from", "2026-10-05T00:00:00Z", "--to", "2026-10-12T00:00:00Z", "--match", "service", "--tz", "UTC", "--prometheus", "http://prometheus:9090",
		"--depends", `sum by (source_workload, destination_workload) (rate(istio_requests_total{reporter="source"}[1h])) > 0`,
		"--depends-labels", "source_workload,destination_workload"}
	return append(args, extra...)
}

// A report judged with --depends, as text and --json (golden, with
// LAPILLI_GOLDEN=update): the payments database's alert catches checkout's
// incident, marked with its path; checkout's alert catches the database's
// only with both directions; the ledger's database, two calls from
// checkout, isn't reached at depth 1, and a service no call reaches never is. The graph is asked
// once, at an hour's step.
func TestVerdictDepends(t *testing.T) {
	alone(t)
	now = func() time.Time { return time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC) }
	var asked []string
	newHistory = func(context.Context, *verdictFlags) (history, error) { return dependsHistory{&asked}, nil }
	for _, out := range []struct{ file, flag string }{{"report.txt", ""}, {"report.json", "--json"}} {
		args := dependsArgs()
		if out.flag != "" {
			args = append(args, out.flag)
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d\n%s", out.file, code, stderr.String())
		}
		golden(t, filepath.Join(dependsDir, out.file), stripRunTime(stdout.String()))
	}
	if len(asked) != 2 || !strings.HasSuffix(asked[0], "@1h0m0s") {
		t.Errorf("asked %q: want the graph once a run, at an hour's step", asked)
	}
}

// Both directions and three calls: the caller's alert catches the
// callee's incident, and the ledger's database, two calls from checkout,
// is reached.
func TestVerdictDependsBothAndDepth(t *testing.T) {
	alone(t)
	var asked []string
	newHistory = func(context.Context, *verdictFlags) (history, error) { return dependsHistory{&asked}, nil }
	var stdout, stderr bytes.Buffer
	if code := run(dependsArgs("--depends-direction", "both", "--depends-depth", "2", "--depends-step", "30m"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"CheckoutErrors +1m (via checkout → payments-db)",
		"PaymentsDBConnections -5m (via checkout → payments-db)",
		"LedgerLag +3m (via checkout → ledger)",
		"LedgerDBDisk +4m (via checkout → payments-db → ledger-db)", // two calls: depth 2
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "SearchDown +") {
		t.Errorf("a service no call reaches caught:\n%s", out)
	}
	if len(asked) == 0 || !strings.HasSuffix(asked[0], "@30m0s") {
		t.Errorf("asked %q", asked)
	}
}

// --redact aliases the services on a path, and the expression stays out.
func TestVerdictDependsRedact(t *testing.T) {
	alone(t)
	var asked []string
	newHistory = func(context.Context, *verdictFlags) (history, error) { return dependsHistory{&asked}, nil }
	var stdout, stderr bytes.Buffer
	if code := run(dependsArgs("--redact"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	for _, name := range []string{"checkout", "payments-db", "PaymentsDBConnections", "ledger"} {
		if strings.Contains(stdout.String(), name) {
			t.Errorf("--redact left %s:\n%s", name, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), "(via ") || !strings.Contains(stdout.String(), "by dependency") {
		t.Errorf("no catch by dependency in:\n%s", stdout.String())
	}
}

// The flags: what they take, what they go with.
func TestDependsFlags(t *testing.T) {
	parallel(t)
	base := []string{"--firings", "f.jsonl", "--incidents", "i.yaml", "--prometheus", "http://p"}
	ok := []string{"--depends", "up", "--depends-labels", "a,b", "--match", "service"}
	for _, c := range []struct {
		args []string
		want string // "" for accepted
	}{
		{ok, ""},
		{append([]string{"--depends-match", "service", "--depends-depth", "3", "--depends-direction", "both", "--depends-step", "10m"}, ok...), ""},
		{[]string{"--depends", "up", "--match", "service"}, "--depends needs --depends-labels"},
		{[]string{"--depends", "up", "--depends-labels", "a,b"}, "--depends needs a --match label"},
		{[]string{"--depends-labels", "a,b"}, "--depends-labels goes with --depends"},
		{[]string{"--depends-depth", "2"}, "--depends-depth goes with --depends"},
		{append([]string{"--depends-labels", "a"}, ok...), "want FROM,TO"},
		{append([]string{"--depends-labels", "a,a"}, ok...), "want FROM,TO"},
		{append([]string{"--depends-labels", "a,b,c"}, ok...), "want FROM,TO"},
		{append([]string{"--depends-depth", "0"}, ok...), "--depends-depth 0: want a number of calls, 1 to 3"},
		{append([]string{"--depends-depth", "4"}, ok...), "1 to 3"},
		{append([]string{"--depends-depth", "x"}, ok...), "1 to 3"},
		{append([]string{"--depends-direction", "up"}, ok...), "want down or both"},
		{append([]string{"--depends-step", "5m"}, ok...), "from 10m to 1h"},
		{append([]string{"--depends-step", "2h"}, ok...), "from 10m to 1h"},
		{append([]string{"--depends-step", "x"}, ok...), "want a duration"},
		{append([]string{"--depends-match", "team"}, ok...), "want one of the labels --match gives (service)"},
		{append([]string{"--depends", "other"}, ok...), "--depends is given twice"},
		{[]string{"--depends", " ", "--depends-labels", "a,b", "--match", "service"}, "want an expression"},
	} {
		_, err := parseVerdictFlags(append(append([]string{}, base...), c.args...))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%v: %v", c.args, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
	// It reads history.
	if _, err := parseVerdictFlags(append([]string{"--firings", "f.jsonl", "--incidents", "i.yaml"}, ok...)); err == nil || !strings.Contains(err.Error(), "reads history") {
		t.Errorf("without a history: %v", err)
	}
	// The defaults.
	f, err := parseVerdictFlags(append(append([]string{}, base...), "--depends", "up", "--depends-labels", "a, b", "--match", "cluster", "--match", "service"))
	if err != nil || f.dependsMatch != "cluster" || f.dependsDepth != 1 || f.dependsDirection != "down" || f.dependsStep != time.Hour || f.dependsFrom != "a" || f.dependsTo != "b" {
		t.Errorf("defaults: %+v %v", f.dependsFlags, err)
	}
}

// An error from the history while the graph is read is the run's, naming
// the flag.
func TestDependsReadFails(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return dependsFails{}, nil }
	var stdout, stderr bytes.Buffer
	if code := run(dependsArgs(), &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "--depends: ") {
		t.Errorf("exit %d: %s", code, stderr.String())
	}
}

type dependsFails struct{}

func (dependsFails) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, fmt.Errorf("no")
}

// The graph's hours are UTC's whatever --tz says: in a zone a half hour
// off, the same edges catch the same incidents, and the report is the same
// but for the zone it writes its times in (golden, with LAPILLI_GOLDEN=update).
func TestVerdictDependsZone(t *testing.T) {
	alone(t)
	now = func() time.Time { return time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC) }
	var asked []string
	newHistory = func(context.Context, *verdictFlags) (history, error) { return dependsHistory{&asked}, nil }
	catches := func(args []string) (string, string) {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr.String())
		}
		var rows []string
		for _, ln := range strings.Split(stdout.String(), "\n") {
			if strings.Contains(ln, "via ") || strings.Contains(ln, "nothing fired") {
				// Everything after the start's time: what caught it.
				if _, after, ok := strings.Cut(ln, "m  "); ok {
					rows = append(rows, after)
				}
			}
		}
		return stdout.String(), strings.Join(rows, "\n")
	}
	_, utc := catches(dependsArgs())
	args := dependsArgs()
	for i, a := range args {
		if a == "--tz" {
			args[i+1] = "Asia/Kolkata"
		}
	}
	out, kolkata := catches(args)
	if utc == "" || utc != kolkata {
		t.Errorf("UTC caught\n%s\nAsia/Kolkata caught\n%s", utc, kolkata)
	}
	if !strings.Contains(out, "2026-10-06 15:30") {
		t.Errorf("times aren't in Asia/Kolkata:\n%s", out)
	}
	golden(t, filepath.Join(dependsDir, "report-kolkata.txt"), stripRunTime(out))
}

// --depends-count-missed goes with --depends, reaches the options, and
// lapilli serve doesn't take it.
func TestDependsCountMissedFlag(t *testing.T) {
	parallel(t)
	base := []string{"--rules", "r.yaml", "--prometheus", "http://p", "--incidents", "i.yml", "--match", "service"}
	dep := []string{"--depends", "up", "--depends-labels", "a,b"}
	f, err := parseVerdictFlags(append(slices.Concat(base, dep), "--depends-count-missed"))
	if err != nil || !f.dependsCountMissed {
		t.Fatalf("with --depends: %v, %v", f != nil && f.dependsCountMissed, err)
	}
	if f, err := parseVerdictFlags(slices.Concat(base, dep)); err != nil || f.dependsCountMissed {
		t.Errorf("by default: %v, %v", f != nil && f.dependsCountMissed, err)
	}
	if _, err := parseVerdictFlags(append(slices.Clone(base), "--depends-count-missed")); err == nil || !strings.Contains(err.Error(), "goes with --depends") {
		t.Errorf("without --depends: %v", err)
	}
}
