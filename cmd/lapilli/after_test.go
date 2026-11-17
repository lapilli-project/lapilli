package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestFiredAfterVersions: a rule replayed as two versions is known by the
// last, but when its firings' conditions held is told by every version:
// with "for" 0 when it fired and 40m only later, a firing just after an
// incident held only once it fired, and came after nothing.
func TestFiredAfterVersions(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	v1 := write(t, "v1.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: Errors, expr: errors > 5}\n")
	v2 := write(t, "v2.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: Errors, expr: errors > 5, for: 40m}\n")
	// errors > 5 holds from 3h to 2h before t0; the incident ends 10m before.
	inc := write(t, "incidents.yaml", `incidents:
  - {id: INC-1, title: Errors, start: "2026-11-01T20:40:00Z", end: "2026-11-01T20:50:00Z"}
`)
	judge := func(inc string, rs ...string) (late int, after int) {
		args := []string{"verdict", "--prometheus", "http://p", "--incidents", inc, "--tz", "UTC", "--json", "--progress=false", "--extras", "0",
			"--from", "2026-11-01T00:00:00Z", "--to", "2026-11-02T00:00:00Z"}
		for _, r := range rs {
			args = append(args, "--rules", r)
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr.String())
		}
		var rep struct {
			Rules []struct {
				Late int `json:"late"`
			} `json:"rules"`
			Incidents []struct {
				FiredAfter []any `json:"fired_after"`
			} `json:"incidents"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		for _, r := range rep.Rules {
			late += r.Late
		}
		for _, in := range rep.Incidents {
			after += len(in.FiredAfter)
		}
		return late, after
	}
	if late, after := judge(inc, v1+"@2026-10-01T00:00:00Z", v2+"@2026-11-01T23:00:00Z"); late != 0 || after != 0 {
		t.Errorf("for 0 when it fired: late %d, fired after %d; want none", late, after)
	}
	// With "for" 40m all along, it held from 21:00, in an incident to 21:08,
	// and fired at 21:40, after it was over.
	held := write(t, "held.yaml", `incidents:
  - {id: INC-2, title: Errors, start: "2026-11-01T20:58:00Z", end: "2026-11-01T21:08:00Z"}
`)
	if late, after := judge(held, v2+"@2026-10-01T00:00:00Z"); late != 1 || after != 1 {
		t.Errorf("for 40m all along: late %d, fired after %d; want 1 and 1", late, after)
	}
}
