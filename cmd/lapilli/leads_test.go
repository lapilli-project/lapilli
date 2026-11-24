package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// A lead of a signal's or a rule entry's own, and require: on an entry
// (docs/design/judging.md, item 1), in lapilli verdict.

// leadsWorld is a window of errors over checkout on day 1 from 10:00, and
// three firings 40 minutes before it: a cause with service, one of a node
// with none, and ApiErrors; and an incident written down over cart on day
// 2, with a cause 40 minutes before it.
func leadsWorld(t *testing.T) (signalHistory, []string) {
	t.Helper()
	h := signalHistory{series: map[string][]on{
		errorsExpr: {{lbl("service", "checkout", "team", "shop", "backend", "checkout-v2"), at(1, 10, 0), at(1, 10, 30)}},
	}}
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-1, title: Cart, start: `+ts(at(2, 10, 0))+`, end: `+ts(at(2, 10, 30))+`, labels: {service: cart}}
`)
	firings := firingsFile(t, [][4]string{
		{"MemoryPressure", `{"service":"checkout","team":"shop"}`, ts(at(1, 9, 20)), ts(at(1, 9, 30))},
		{"MemoryPressure", `{"node":"n1"}`, ts(at(1, 9, 20)), ts(at(1, 9, 30))},
		{"ApiErrors", `{"service":"checkout","team":"shop"}`, ts(at(1, 9, 20)), ts(at(1, 9, 30))},
		{"MemoryPressure", `{"service":"cart","team":"shop"}`, ts(at(2, 9, 20)), ts(at(2, 9, 30))},
	})
	return h, []string{"verdict", "--firings", firings, "--incidents", incidents, "--prometheus", "http://p",
		"--from", ts(sigBase), "--to", ts(sigBase.Add(7 * 24 * time.Hour)), "--match", "service", "--tz", "UTC"}
}

// caughtBy is, of each firing, its rule, labels and what it caught.
func caughtBy(t *testing.T, out string) []string {
	t.Helper()
	var rep verdict.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range rep.Firings {
		got = append(got, fmt.Sprintf("%s %v %v", f.Rule, f.Labels, f.Incidents))
	}
	slices.Sort(got)
	return got
}

// An entry's lead catches a cause 40 minutes before its signals' window,
// and require: keeps a node's alert with no service from it; a signal's
// lead, by --impact-lead or the file, catches every rule's firing as long
// before; an incident written down is caught from --lead alone. The text
// says the leads; the report gives them, and numbers.md's blocks count the
// stones again from it.
func TestLeads(t *testing.T) {
	alone(t)
	h, args := leadsWorld(t)
	var plain string
	for _, extra := range [][]string{{"--impact", "errors=" + errorsExpr}, {"--impact-file", write(t, "plain.yaml", "signals:\n  - {name: errors, expr: '"+errorsExpr+"'}\n")}} {
		plain = runSignals(t, h, append(slices.Concat(args, extra), "--json")...)
		if got := caughtBy(t, plain); slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "impact-errors-1") }) {
			t.Errorf("%v: caught 40 minutes before, by --lead alone: %q", extra, got)
		}
	}
	if strings.Contains(plain, `"require"`) || strings.Count(plain, "lead_ns") != 1 {
		t.Errorf("leads in the settings of a report with none:\n%s", plain)
	}
	file := write(t, "impact.yaml", `signals:
  - {name: errors, expr: '`+errorsExpr+`', for: 5m}
rules:
  - rules: ^MemoryPressure$
    signals: [errors]
    lead: 1h
    require: [service]
`)
	out := runSignals(t, h, append(slices.Clone(args), "--impact-file", file, "--json")...)
	want := []string{
		"ApiErrors map[service:checkout team:shop] []",
		"MemoryPressure map[node:n1] []",
		"MemoryPressure map[service:cart team:shop] []",
		"MemoryPressure map[service:checkout team:shop] [impact-errors-1]",
	}
	if got := caughtBy(t, out); !slices.Equal(got, want) {
		t.Errorf("the entry's lead:\n got %q\nwant %q", got, want)
	}
	var rep verdict.Report
	json.Unmarshal([]byte(out), &rep)
	if e := rep.Settings.ImpactRules; len(e) != 1 || e[0].Lead == nil || *e[0].Lead != time.Hour || !slices.Equal(e[0].Require, []string{"service"}) {
		t.Errorf("the report's rule entries: %+v", e)
	}
	// The signal's own lead, by the flag and by the file alike: every
	// rule's firing 40 minutes before, the node's too, as no entry asks.
	byFlag := runSignals(t, h, append(slices.Clone(args), "--impact", "errors="+errorsExpr, "--impact-lead", "errors=1h", "--json")...)
	byFile := runSignals(t, h, append(slices.Clone(args), "--impact-file", write(t, "lead.yaml", "signals:\n  - {name: errors, expr: '"+errorsExpr+"', lead: 1h}\n"), "--json")...)
	if byFlag != byFile {
		t.Errorf("--impact-lead judged otherwise than the file's lead:\n%s\nfile:\n%s", byFlag, byFile)
	}
	want = []string{
		"ApiErrors map[service:checkout team:shop] [impact-errors-1]",
		"MemoryPressure map[node:n1] [impact-errors-1]",
		"MemoryPressure map[service:cart team:shop] []",
		"MemoryPressure map[service:checkout team:shop] [impact-errors-1]",
	}
	if got := caughtBy(t, byFlag); !slices.Equal(got, want) {
		t.Errorf("the signal's lead:\n got %q\nwant %q", got, want)
	}
	text := runSignals(t, h, append(slices.Clone(args), "--impact-file", file)...)
	if !strings.Contains(text, "Leads: errors 15m (for 5m), rule entry 1 1h; incidents written down 15m\n") {
		t.Errorf("the text says no leads:\n%s", text)
	}
	if plainText := runSignals(t, h, append(slices.Clone(args), "--impact", "errors="+errorsExpr)...); strings.Contains(plainText, "Leads:") {
		t.Errorf("the text of a report with no lead says leads:\n%s", plainText)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		return
	}
	// numbers.md's "incidents caught", counted again from each report.
	var prog, has string
	for _, r := range recounts(t, "../../docs/numbers.md") {
		if r.name == "incidents caught" && r.as == "counted again" {
			prog = r.prog
		}
		if r.name == "incidents caught" && r.as == "as the report has it" {
			has = r.prog
		}
	}
	for name, body := range map[string]string{"entry": out, "signal": byFlag} {
		p := filepath.Join(t.TempDir(), name+".json")
		os.WriteFile(p, []byte(body), 0o644)
		if got, want := jq(t, prog, p), jq(t, has, p); got != want {
			t.Errorf("%s's lead, incidents caught counted again %s, the report has %s", name, got, want)
		}
	}
}

// A lead is from 0 to a day, a named signal's alone, given once; with
// --impact-file, in the file. require: is held as match is.
func TestLeadsRefused(t *testing.T) {
	parallel(t)
	file := func(body string) string { return write(t, "impact.yaml", body) }
	base := []string{"verdict", "--firings", write(t, "f.jsonl", ""), "--prometheus", "http://p"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--impact", "errors=up", "--impact-lead", "errors=24h1s"}, "want a duration from 0 to 24h"},
		{[]string{"--impact", "errors=up", "--impact-lead", "errors=25h"}, "want a duration from 0 to 24h"},
		{[]string{"--impact", "errors=up", "--impact-lead", "errors=-1m"}, "want a duration from 0 to 24h"},
		{[]string{"--impact", "errors=up", "--impact-lead", "errors=soon"}, "want a duration from 0 to 24h"},
		{[]string{"--impact", "errors=up", "--impact-lead", "errors=1h", "--impact-lead", "errors=2h"}, "given twice"},
		{[]string{"--impact", "errors=up", "--impact-lead", "probe=1h"}, "name probe, which no --impact NAME=PROMQL gives"},
		{[]string{"--impact", "up", "--impact-lead", "1h"}, "want NAME=..."},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]"), "--impact-lead", "a=1h"}, "leave out --impact-for NAME=DUR"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up, lead: 25h}]")}, "signal 1 (a): lead 25h: want a duration from 0 to 24h"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]\nrules: [{rules: x, signals: [a], lead: 2d}]")}, "rule entry 1: lead 2d: want a duration from 0 to 24h"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]\nrules: [{rules: x, signals: [a], require: [9x]}]")}, `rule entry 1: require: "9x" isn't a label's name`},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]\nrules: [{rules: x, signals: [a], require: [service, service]}]")}, "there twice"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append(slices.Clone(base), c.args...), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%q: exit %d, want 2 and %q\n%s", c.args, code, c.want, stderr.String())
		}
	}
	// The bounds themselves are leads.
	for _, v := range []string{"0", "0s", "24h", "1d"} {
		f := &verdictFlags{}
		if _, err := f.impactFlag("--impact", "errors=up"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.impactFlag("--impact-lead", "errors="+v); err != nil {
			t.Errorf("--impact-lead errors=%s: %v", v, err)
		}
	}
	if _, _, err := readImpactFile(file("signals: [{name: a, expr: up, lead: 0}]\nrules: [{rules: x, signals: [a], lead: 24h}]")); err != nil {
		t.Errorf("the bounds in the file: %v", err)
	}
}
