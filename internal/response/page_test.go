package response

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodYAML = `responses:
  - id: PD-Q1W2E3
    alerts:
      - {rule: KubePodCrashLooping, labels: {namespace: shop, pod: cart-7f9c}}
      - {rule: KubePodCrashLooping, labels: {namespace: shop, pod: cart-7f9c}}
    sent: 2026-11-20T03:12:00Z
    acknowledged: 2026-11-20T03:15:00Z
    resolved: 2026-11-20T03:20:00Z
    resolved_by: alert
  - id: second
    alerts: [{rule: Disk}]
    sent: 2026-11-20T03:00:00Z
`

const goodJSON = `{"responses": [
 {"id": "PD-Q1W2E3", "alerts": [{"rule": "KubePodCrashLooping", "labels": {"namespace": "shop", "pod": "cart-7f9c"}}],
  "sent": "2026-11-20T03:12:00Z", "acknowledged": "2026-11-20T03:15:00Z", "resolved": "2026-11-20T03:20:00Z", "resolved_by": "alert"},
 {"id": "second", "alerts": [{"rule": "Disk"}], "sent": "2026-11-20T03:00:00Z", "acknowledged": null}
]}`

func TestParseGood(t *testing.T) {
	for name, text := range map[string]string{"yaml": goodYAML, "json": goodJSON} {
		pages, err := Parse([]byte(text), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// In order of the sent time; the repeated alert once.
		if len(pages) != 2 || pages[0].ID != "second" || pages[1].ID != "PD-Q1W2E3" {
			t.Fatalf("%s: pages %+v", name, pages)
		}
		p := pages[1]
		if len(p.Alerts) != 1 || p.Alerts[0].Labels["pod"] != "cart-7f9c" ||
			!p.Sent.Equal(time.Date(2026, 11, 20, 3, 12, 0, 0, time.UTC)) ||
			!p.Acknowledged.Equal(p.Sent.Add(3*time.Minute)) || !p.Resolved.Equal(p.Sent.Add(8*time.Minute)) || p.ResolvedBy != ByAlert {
			t.Errorf("%s: %+v", name, p)
		}
		if q := pages[0]; !q.Acknowledged.IsZero() || !q.Resolved.IsZero() || !q.Open() {
			t.Errorf("%s: %+v", name, q)
		}
	}
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.yaml")
	os.WriteFile(path, []byte(goodYAML), 0o644)
	if pages, err := Load(path); err != nil || len(pages) != 2 {
		t.Errorf("%v %v", pages, err)
	}
	if _, err := Load(path + ".none"); err == nil {
		t.Error("a file that isn't there was read")
	}
}

// page writes one page's text, in YAML and in JSON, with fields replaced.
func page(fields map[string]string) (y, j string) {
	f := map[string]string{
		"id":     `"p1"`,
		"alerts": `[{"rule": "Disk", "labels": {"a": "b"}}]`,
		"sent":   `"2026-11-20T03:12:00Z"`,
	}
	for k, v := range fields {
		if v == "-" {
			delete(f, k)
		} else {
			f[k] = v
		}
	}
	var parts []string
	for _, k := range []string{"id", "alerts", "sent", "acknowledged", "resolved", "resolved_by"} {
		if v, ok := f[k]; ok {
			parts = append(parts, fmt.Sprintf("%q: %s", k, v))
		}
	}
	for k, v := range f {
		switch k {
		case "id", "alerts", "sent", "acknowledged", "resolved", "resolved_by":
		default:
			parts = append(parts, fmt.Sprintf("%q: %s", k, v))
		}
	}
	// JSON is YAML: a flow mapping serves both.
	one := "{" + strings.Join(parts, ", ") + "}"
	return "responses:\n  - " + one + "\n", `{"responses": [` + one + `]}`
}

func TestParseRefusals(t *testing.T) {
	long := `"` + strings.Repeat("a", MaxID+1) + `"`
	var many []string
	for i := 0; i <= 100; i++ {
		many = append(many, fmt.Sprintf(`{"rule": "R%d"}`, i))
	}
	bigLabels := map[string]string{}
	for i := 0; i <= 32; i++ {
		bigLabels[fmt.Sprintf("l%d", i)] = "v"
	}
	lb, _ := json.Marshal(bigLabels)
	for _, c := range []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"unknown key", map[string]string{"acked": `"x"`}, "not \"acked\""},
		{"no id", map[string]string{"id": "-"}, "needs an id"},
		{"empty id", map[string]string{"id": `""`}, "needs an id"},
		{"id too long", map[string]string{"id": long}, "256 bytes"},
		{"id with a space", map[string]string{"id": `"a b"`}, "letters, digits"},
		{"id with a slash", map[string]string{"id": `"a/b"`}, "letters, digits"},
		{"id not text", map[string]string{"id": `["x"]`}, ""},
		{"no alerts", map[string]string{"alerts": "-"}, "one alert"},
		{"empty alerts", map[string]string{"alerts": "[]"}, "one alert"},
		{"alert key unknown", map[string]string{"alerts": `[{"rule": "R", "label": {}}]`}, "not \"label\""},
		{"alert label not text", map[string]string{"alerts": `[{"rule": "R", "labels": {"a": [1]}}]`}, ""},
		{"alert label a map", map[string]string{"alerts": `[{"rule": "R", "labels": {"a": {"b": "c"}}}]`}, ""},
		{"alert with no rule", map[string]string{"alerts": `[{"labels": {"a": "b"}}]`}, "needs a rule"},
		{"too many alerts", map[string]string{"alerts": "[" + strings.Join(many, ",") + "]"}, "alerts at most"},
		{"too many labels", map[string]string{"alerts": `[{"rule": "R", "labels": ` + string(lb) + `}]`}, "labels at most"},
		{"no sent", map[string]string{"sent": "-"}, "sent time"},
		{"sent not a time", map[string]string{"sent": `"yesterday"`}, "RFC 3339"},
		{"resolved without resolved_by", map[string]string{"resolved": `"2026-11-20T03:20:00Z"`}, "resolved_by"},
		{"resolved_by without resolved", map[string]string{"resolved_by": `"alert"`}, "no resolved time"},
		{"resolved_by unknown", map[string]string{"resolved": `"2026-11-20T03:20:00Z"`, "resolved_by": `"robot"`}, "alert or person"},
	} {
		y, j := page(c.fields)
		for name, text := range map[string]string{"yaml": y, "json": j} {
			_, err := Parse([]byte(text), name)
			if err == nil {
				t.Errorf("%s, %s: no error", c.name, name)
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s, %s: %v, want %q", c.name, name, err, c.want)
			}
		}
	}
	// The same on the JSON a write carries, one page at a time.
	var p Page
	if err := json.Unmarshal([]byte(`{"id": "x", "alerts": [{"rule": "R"}], "sent": "2026-11-20T03:12:00Z", "acked": 1}`), &p); err == nil {
		t.Error("a JSON page with an unknown key was taken")
	}
}

func TestParseFileShape(t *testing.T) {
	for name, c := range map[string]struct{ text, want string }{
		"empty":         {"", "no responses key"},
		"bare list":     {"- id: a\n", "responses:"},
		"another key":   {"incidents: []\n", "no responses key"},
		"not a list":    {"responses: {id: a}\n", "list of pages"},
		"duplicate id":  {"responses:\n  - {id: a, alerts: [{rule: R}], sent: 2026-11-20T03:12:00Z}\n  - {id: a, alerts: [{rule: R}], sent: 2026-11-20T03:13:00Z}\n", "twice"},
		"duplicate key": {"responses:\n  - {id: a, id: b, alerts: [{rule: R}], sent: 2026-11-20T03:12:00Z}\n", ""},
		"not a mapping": {"responses:\n  - a\n", ""},
	} {
		if _, err := Parse([]byte(c.text), name); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if pages, err := Parse([]byte("responses: []\n"), "x"); err != nil || len(pages) != 0 {
		t.Errorf("an empty list: %v %v", pages, err)
	}
}

func TestParseBounds(t *testing.T) {
	line := func(i int) string {
		return fmt.Sprintf("  - {id: p%d, alerts: [{rule: R}], sent: 2026-11-20T03:12:00Z}\n", i)
	}
	build := func(n int) string {
		var b strings.Builder
		b.WriteString("responses:\n")
		for i := 0; i < n; i++ {
			b.WriteString(line(i))
		}
		return b.String()
	}
	if pages, err := Parse([]byte(build(MaxPages)), "x"); err != nil || len(pages) != MaxPages {
		t.Fatalf("%d pages: %v", MaxPages, err)
	}
	if _, err := Parse([]byte(build(MaxPages+1)), "x"); err == nil || !strings.Contains(err.Error(), "100000 pages at most") {
		t.Errorf("one past: %v", err)
	}
	// The id's length is held at 256 exactly.
	id := strings.Repeat("a", MaxID)
	if _, err := Parse([]byte("responses:\n  - {id: "+id+", alerts: [{rule: R}], sent: 2026-11-20T03:12:00Z}\n"), "x"); err != nil {
		t.Errorf("an id of 256: %v", err)
	}
	if _, err := Parse([]byte("responses:\n  - {id: 'a-B_c.d:1', alerts: [{rule: R}], sent: 2026-11-20T03:12:00Z}\n"), "x"); err != nil {
		t.Errorf("an id of the allowed characters: %v", err)
	}
}

func at(min int) time.Time {
	return time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

func TestIndexFind(t *testing.T) {
	mk := func(id string, sent int, rules ...string) Page {
		p := Page{ID: id, Sent: at(sent)}
		for _, r := range rules {
			p.Alerts = append(p.Alerts, alert(r, "ns", "shop"))
		}
		return p
	}
	pages, err := Check([]Page{
		mk("far", 50, "Disk"),
		mk("b", 12, "Disk"),
		mk("a", 12, "Disk"),
		mk("group", 11, "Disk", "Cpu"),
		mk("other", 11, "Cpu"),
		mk("tagged", 30, "HighErrors"),
		mk("early", 2, "Disk"),
	}, "x")
	if err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(pages)
	labels := map[string]string{"ns": "shop", "extra": "x"}
	for _, c := range []struct {
		name     string
		rule     string
		labels   map[string]string
		from, to int
		want     string
	}{
		{"the nearest to the start", "Disk", labels, 10, 20, "group"},
		{"a page sent at the start counts", "Disk", labels, 12, 20, "a"},
		{"a page sent at the end counts", "Disk", labels, 5, 11, "group"},
		{"one sent after the end doesn't", "Disk", labels, 5, 10, ""},
		{"one sent before the start doesn't", "Disk", labels, 13, 40, ""},
		{"a group's page answers each of its alerts", "Cpu", labels, 10, 20, "group"},
		{"a firing told apart from others of its name", "HighErrors [severity=critical]", labels, 25, 35, "tagged"},
		{"another namespace", "Disk", map[string]string{"ns": "pay"}, 10, 20, ""},
		{"a label the firing hasn't stands for any", "Disk", map[string]string{}, 10, 20, "group"},
		{"another rule", "Mem", labels, 0, 100, ""},
	} {
		got := ""
		if p := ix.Find(c.rule, c.labels, at(c.from), at(c.to)); p != nil {
			got = p.ID
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if n := ix.Unmatched(map[string]bool{"group": true}); n != len(pages)-1 {
		t.Errorf("unmatched %d", n)
	}
	var none *Index
	if none.Find("Disk", labels, at(0), at(99)) != nil || none.Len() != 0 {
		t.Error("a nil index found something")
	}
}
