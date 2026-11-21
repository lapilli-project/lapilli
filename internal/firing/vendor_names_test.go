package firing_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// TestRecipesNameRulesAsServeTakesThem: a name the tool gives with a
// character that doesn't show (a bell, a zero-width space, a soft hyphen,
// a line or paragraph separator), or longer than 512 characters, would
// have serve refuse the whole copy, every firing of the period with it.
// Each recipe's conversion takes those characters out and cuts the name
// to 512, and names a rule with nothing left by its ID; each firing it
// writes passes the check serve makes before it copies one in.
func TestRecipesNameRulesAsServeTakesThem(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	const dir = "../../recipes/firings/"
	odd := "Checkout\u0007 5xx\u200b on\u2028 call\u2029\u00ad\ufeff"
	long := strings.Repeat("가", 600)
	none := "\u200b\u2028\u0007"
	// Each wants Checkout 5xx on call, 512 of the 600, and its fallback.
	shown, cut := "Checkout 5xx on call", strings.Repeat("가", 512)
	names := []string{odd, long, none}
	at := func(i int) string { return fmt.Sprintf("2026-11-02T0%d:00:00Z", i+1) }
	ms := func(i int) int64 { return 1793581200000 + int64(i)*3600000 } // at(i)
	for _, c := range []struct {
		jq    string
		args  []string
		files map[string][]any
		want  []string
	}{
		{"datadog.jq", []string{"-s", "--argjson", "since", "1793491200000", "--argjson", "until", "1794096000000"},
			map[string][]any{"": each(names, func(i int, n string) any {
				// The third's template and filled-in name both show nothing.
				m := map[string]any{"templated_name": n}
				if n == none {
					m["name"] = "\ufeff" + none
				}
				return map[string]any{"id": fmt.Sprint("e", i), "attributes": map[string]any{"attributes": map[string]any{
					"monitor_id": 1001 + i, "status": "error", "timestamp": ms(i), "monitor": m}}}
			})},
			[]string{shown, cut, "monitor 1003"}},
		{"newrelic.jq", []string{"-s", "--argjson", "since", "1793491200000", "--argjson", "until", "1794096000000"},
			map[string][]any{"": each(names, func(i int, n string) any {
				return map[string]any{"incidentId": fmt.Sprint(i), "event": "open", "timestamp": ms(i),
					"conditionName": n, "conditionId": 701 + i}
			})},
			[]string{shown, cut, "condition 703"}},
		{"cloudwatch.jq", []string{"-n", "--arg", "since", "2026-11-01T00:00:00Z", "--arg", "until", "2026-11-08T00:00:00Z"},
			map[string][]any{
				"history": {map[string]any{"AlarmHistoryItems": each(names, func(i int, n string) any {
					return map[string]any{"AlarmName": n, "HistoryItemType": "StateUpdate", "Timestamp": at(i),
						"HistoryData": `{"oldState": {"stateValue": "OK"}, "newState": {"stateValue": "ALARM"}}`}
				})}},
				"alarms": {map[string]any{"MetricAlarms": each(names, func(i int, n string) any {
					return map[string]any{"AlarmName": n, "AlarmArn": "arn:aws:cloudwatch:us-east-1:123456789012:alarm:" + n}
				})}},
			},
			[]string{shown, cut, "alarm arn:aws:cloudwatch:us-east-1:123456789012:alarm:"}},
		{"google-cloud.jq", []string{"-s", "--arg", "since", "2026-11-01T00:00:00Z", "--arg", "until", "2026-11-08T00:00:00Z"},
			map[string][]any{"": each(names, func(i int, n string) any {
				return map[string]any{"openTime": at(i), "state": "OPEN",
					"policy": map[string]any{"displayName": n, "name": fmt.Sprint("projects/example/alertPolicies/", i+1)}}
			})},
			[]string{shown, cut, "projects/example/alertPolicies/3"}},
		{"oci.jq", []string{"-n", "--arg", "since", "2026-11-01T00:00:00Z", "--arg", "until", "2026-11-08T00:00:00Z"},
			map[string][]any{
				"alarms": {map[string]any{"data": each(names, func(i int, n string) any {
					return map[string]any{"id": fmt.Sprint("ocid1.alarm.oc1.ap-seoul-1.", i), "display-name": n, "severity": "CRITICAL"}
				})}},
				"entries": each(names, func(i int, n string) any {
					return map[string]any{"alarm": fmt.Sprint("ocid1.alarm.oc1.ap-seoul-1.", i),
						"summary": "State transitioned from OK to Firing", "timestamp": at(i), "timestamp-triggered": at(i)}
				}),
			},
			[]string{shown, cut, "ocid1.alarm.oc1.ap-seoul-1.2"}},
		{"azure-monitor.jq", []string{"-s"},
			map[string][]any{"": each(names, func(i int, n string) any {
				return map[string]any{"rule": "/subscriptions/s1/resourceGroups/shop/providers/microsoft.insights/metricalerts/" + n,
					"severity": "Sev1", "condition": "Fired", "state": "New", "start": at(i)}
			})},
			[]string{shown, cut, "/subscriptions/s1/resourceGroups/shop/providers/microsoft.insights/metricalerts/"}},
	} {
		args := append([]string{"-c"}, c.args...)
		tmp := t.TempDir()
		var input string
		for name, lines := range c.files {
			path := filepath.Join(tmp, "in-"+name)
			var b strings.Builder
			for _, l := range lines {
				j, _ := json.Marshal(l)
				b.Write(j)
				b.WriteByte('\n')
			}
			if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			if name == "" {
				input = path
			} else {
				args = append(args, "--slurpfile", name, path)
			}
		}
		args = append(args, "-f", dir+c.jq)
		if input != "" {
			args = append(args, input)
		}
		out, err := exec.Command("jq", args...).Output()
		if err != nil {
			t.Errorf("%s: %v", c.jq, err)
			continue
		}
		var rules []string
		sc := bufio.NewScanner(strings.NewReader(string(out)))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var f firing.Firing
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				t.Fatalf("%s: %v: %s", c.jq, err, sc.Text())
			}
			rules = append(rules, f.Rule)
			if err := ledger.Recordable(f); err != nil {
				t.Errorf("%s: serve would refuse the copy for %.40q: %v", c.jq, f.Rule, err)
			}
		}
		slices.Sort(rules)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if !slices.Equal(rules, want) {
			t.Errorf("%s: rules %.40q, want %.40q", c.jq, rules, want)
		}
	}
}

// each is what f makes of each name, in turn.
func each(names []string, f func(int, string) any) []any {
	var out []any
	for i, n := range names {
		out = append(out, f(i, n))
	}
	return out
}
