package serve

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"lapilli.dev/internal/incident"
)

// goBytes is what incident.CheckAlerts counts of alerts.
func goBytes(as []incident.Alert) int {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(as)
	return b.Len() - 1
}

func runAlertsJQ(t *testing.T, input []byte, program string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jq", append([]string{"-c", "-L", "../../recipes/incidents", "include \"alerts\"; " + program}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

// nasty draws from what Go's JSON encoder and jq's write differently, and what
// both escape.
var nasty = []string{"a", "z", "\u2028", "\u2029", "\u007f", "<", ">", "&", "\"", "\\", "\n", "\t", "\r", "\b", "\f", "\u0001", "\u001f", "é", "日", "😀", "\u00ad", "\u0085", "/", "'"}

func randString(r *rand.Rand, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(nasty[r.IntN(len(nasty))])
	}
	return b.String()
}

// The recipes count the bytes of the alerts they write as Go's encoder does,
// which incident.CheckAlerts holds the alerts to: the same number for any
// character, whatever jq's tojson says of it.
func TestAlertsRecipeCountsBytesAsGoDoes(t *testing.T) {
	parallel(t)
	needTools(t)
	r := rand.New(rand.NewPCG(7, 11))
	var lines []string
	var want []int
	for i := 0; i < 300; i++ {
		var as []incident.Alert
		for j, n := 0, r.IntN(4); j <= n; j++ {
			a := incident.Alert{Rule: "R" + randString(r, 1+r.IntN(6))}
			for k, m := 0, r.IntN(4); k < m; k++ {
				if a.Labels == nil {
					a.Labels = map[string]string{}
				}
				a.Labels["k"+strconv.Itoa(k)+randString(r, r.IntN(3))] = "v" + randString(r, r.IntN(8))
			}
			as = append(as, a)
		}
		b, _ := json.Marshal(as)
		lines = append(lines, string(b))
		want = append(want, goBytes(as))
	}
	out := runAlertsJQ(t, []byte(strings.Join(lines, "\n")), "go_bytes")
	got := strings.Fields(out)
	if len(got) != len(want) {
		t.Fatalf("%d answers for %d alerts", len(got), len(want))
	}
	for i := range want {
		if got[i] != strconv.Itoa(want[i]) {
			t.Fatalf("jq counts %s, Go %d, of %s", got[i], want[i], lines[i])
		}
	}
}

// Alerts written with U+2028 and U+2029 are held to 16384 bytes as Go counts
// them: what the recipe keeps incident.CheckAlerts accepts, and the one
// alert past it is left out, not written for serve to refuse the file.
func TestAlertsRecipeKeepsWhatCheckAlertsTakes(t *testing.T) {
	parallel(t)
	needTools(t)
	// Alerts of one label whose value is U+2028 eighty-five times (255
	// bytes as jq counts it, 510 as Go does), the rule padded to the byte.
	mk := func(i int, pad int) incident.Alert {
		return incident.Alert{Rule: "Rule" + strconv.Itoa(1000+i) + strings.Repeat("a", pad), Labels: map[string]string{"k": strings.Repeat("\u2028", 85)}}
	}
	var as []incident.Alert
	for i := 0; goBytes(append(as, mk(i, 0))) <= incident.MaxAlertBytes; i++ {
		as = append(as, mk(i, 0))
	}
	// as is the most that fit; the next one is past the bound.
	next := mk(len(as), 0)
	if err := incident.CheckAlerts(as); err != nil {
		t.Fatalf("the alerts that fit: %v", err)
	}
	if err := incident.CheckAlerts(append(slicesClone(as), next)); err == nil {
		t.Fatal("the alerts past the bound were taken")
	}
	// The bound to the byte: pad the last alert to exactly 16384.
	for pad := 0; ; pad++ {
		last := mk(len(as)-1, pad)
		cand := append(slicesClone(as[:len(as)-1]), last)
		if goBytes(cand) > incident.MaxAlertBytes {
			as = append(slicesClone(as[:len(as)-1]), mk(len(as)-1, pad-1))
			break
		}
	}
	if n := goBytes(as); n != incident.MaxAlertBytes {
		t.Fatalf("the alerts are %d bytes, not %d to the byte", n, incident.MaxAlertBytes)
	}
	// Each as a tool's alert: the recipe takes {rule, labels} and writes
	// what it keeps.
	in := append(slicesClone(as), next)
	b, _ := json.Marshal(in)
	out := runAlertsJQ(t, b, "map(as_alert) | linked")
	var got struct {
		Alerts []incident.Alert `json:"alerts"`
		Cut    int              `json:"cut"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := incident.CheckAlerts(got.Alerts); err != nil {
		t.Errorf("what the recipe kept is refused: %v", err)
	}
	if len(got.Alerts) != len(as) || got.Cut != 1 {
		t.Errorf("kept %d alerts and cut %d, want %d and 1 (%d bytes kept)", len(got.Alerts), got.Cut, len(as), goBytes(got.Alerts))
	}
	// The same alerts with one byte more: the padded one no longer fits, and
	// the one after it takes its place.
	as[len(as)-1].Rule += "a"
	b, _ = json.Marshal(append(slicesClone(as), next))
	out = runAlertsJQ(t, b, "map(as_alert) | linked")
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Alerts) != len(as) || got.Cut != 1 || got.Alerts[len(got.Alerts)-1].Rule != next.Rule {
		t.Errorf("one byte past 16384: kept %d, cut %d (%v)", len(got.Alerts), got.Cut, err)
	}
	if err := incident.CheckAlerts(got.Alerts); err != nil {
		t.Errorf("what the recipe kept is refused: %v", err)
	}
}

func slicesClone(as []incident.Alert) []incident.Alert { return append([]incident.Alert(nil), as...) }
