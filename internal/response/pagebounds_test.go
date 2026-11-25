package response

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A value that is not text where the spec says text is refused in YAML as it
// is in JSON, whatever it would read as.
func TestParseRefusesNonText(t *testing.T) {
	for _, c := range []struct{ name, field, value string }{
		{"id a number", "id", `123`},
		{"id a boolean", "id", `true`},
		{"id a huge number", "id", `1e999999`},
		{"id null", "id", `null`},
		{"resolved_by a number", "resolved_by", `5`},
		{"rule a number", "alerts", `[{"rule": 7}]`},
		{"rule a boolean", "alerts", `[{"rule": false}]`},
		{"label value a number", "alerts", `[{"rule": "R", "labels": {"a": 12}}]`},
		{"label value a boolean", "alerts", `[{"rule": "R", "labels": {"a": true}}]`},
		{"label value a huge number", "alerts", `[{"rule": "R", "labels": {"a": 1e999999}}]`},
		{"label value null", "alerts", `[{"rule": "R", "labels": {"a": null}}]`},
		{"sent a number", "sent", `20261120`},
	} {
		fields := map[string]string{c.field: c.value}
		if c.field == "resolved_by" {
			fields["resolved"] = `"2026-11-20T03:20:00Z"`
		}
		y, j := page(fields)
		for name, text := range map[string]string{"yaml": y, "json": j} {
			if _, err := Parse([]byte(text), name); err == nil {
				t.Errorf("%s, %s: taken", c.name, name)
			}
			if _, err := ParseJSON(strings.NewReader(j), 1<<20); name == "json" && err == nil {
				t.Errorf("%s, ParseJSON: taken", c.name)
			}
		}
	}
	// YAML-only: an unquoted number as a label's name or a value.
	for _, text := range []string{
		"responses:\n  - {id: 123, alerts: [{rule: R}], sent: 2026-11-20T03:12:00Z}\n",
		"responses:\n  - {id: a, alerts: [{rule: R, labels: {1: x}}], sent: 2026-11-20T03:12:00Z}\n",
		"responses:\n  - {id: a, alerts: [{rule: R, labels: {k: 0x1F}}], sent: 2026-11-20T03:12:00Z}\n",
		"responses:\n  - {id: a, alerts: [{rule: R, labels: {k: 1e999999}}], sent: 2026-11-20T03:12:00Z}\n",
		"responses:\n  - {id: 2026-11-20, alerts: [{rule: R}], sent: 2026-11-20T03:12:00Z}\n",
		"responses:\n  - {id: a, alerts: [{rule: 5}], sent: 2026-11-20T03:12:00Z}\n",
	} {
		if _, err := Parse([]byte(text), "x"); err == nil {
			t.Errorf("taken: %s", text)
		}
	}
	// Text that looks like a number is fine when it is quoted.
	if _, err := Parse([]byte("responses:\n  - {id: '123', alerts: [{rule: R, labels: {k: '12'}}], sent: 2026-11-20T03:12:00Z}\n"), "x"); err != nil {
		t.Errorf("quoted numbers: %v", err)
	}
}

func TestParseRefusesYAMLReferences(t *testing.T) {
	for name, text := range map[string]string{
		"anchor":                "responses:\n  - &a {id: a, alerts: [{rule: R}], sent: 2026-11-20T03:12:00Z}\n",
		"alias":                 "x: &al {rule: R}\nresponses:\n  - {id: a, alerts: [*al], sent: 2026-11-20T03:12:00Z}\n",
		"merge":                 "responses:\n  - <<: {id: a}\n    alerts: [{rule: R}]\n    sent: 2026-11-20T03:12:00Z\n",
		"alerts a list aliased": "responses:\n  - id: a\n    alerts: &l [{rule: R}]\n    sent: 2026-11-20T03:12:00Z\n  - id: b\n    alerts: *l\n    sent: 2026-11-20T03:12:00Z\n",
		"merge in a label":      "responses:\n  - {id: a, alerts: [{rule: R, labels: {<<: {k: v}}}], sent: 2026-11-20T03:12:00Z}\n",
	} {
		_, err := Parse([]byte(text), name)
		if err == nil || !strings.Contains(err.Error(), "anchors, aliases and merge keys") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A bomb: a few lines that would expand to billions of alerts.
	bomb := "a: &a [{rule: R}, {rule: R}, {rule: R}, {rule: R}, {rule: R}, {rule: R}, {rule: R}, {rule: R}]\n"
	prev := "a"
	for _, n := range []string{"b", "c", "d", "e", "f", "g", "h", "i"} {
		bomb += n + ": &" + n + " [*" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + "]\n"
		prev = n
	}
	bomb += "responses: [{id: x, alerts: *" + prev + ", sent: 2026-11-20T03:12:00Z}]\n"
	if _, err := Parse([]byte(bomb), "bomb"); err == nil || !strings.Contains(err.Error(), "anchors, aliases") {
		t.Errorf("a bomb: %v", err)
	}
}

func jsonBody(pre, repeat, post string, size int) []byte {
	var b bytes.Buffer
	b.WriteString(pre)
	for b.Len() < size {
		b.WriteString(repeat)
	}
	b.WriteString(post)
	return b.Bytes()
}

// What ParseJSON allocates, in all, to read body: heap that could be in use
// at once is at most this.
func allocated(t *testing.T, body []byte, max int64) (uint64, error) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := ParseJSON(bytes.NewReader(body), max)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, err
}

// An 8 MiB body made to take memory is refused as it is read: page by page,
// the work stays within bound, 256 MiB, 32 times the body, of everything
// allocated (garbage too; no wall-clock): 100,000 pages of about 1.6 KiB
// each are what the page bound lets through, and the rest of these refuse
// within 41 MiB or far less.
func TestParseJSONAdversarialBodies(t *testing.T) {
	const size = 8 << 20
	const bound = 256 << 20
	bodies := map[string][]byte{
		"tiny pages":       jsonBody(`{"responses":[`, `{},`, `{}]}`, size),
		"deep arrays":      jsonBody(`{"responses":[`, `[`, ``, size),
		"deep objects":     jsonBody(`{"responses":[`, `{"id":`, ``, size),
		"one long id":      jsonBody(`{"responses":[{"id":"`, `a`, `"}]}`, size),
		"many alerts":      jsonBody(`{"responses":[{"id":"a","alerts":[`, `{"rule":"R"},`, `{"rule":"R"}]}]}`, size),
		"many labels":      jsonBody(`{"responses":[{"id":"a","alerts":[{"rule":"R","labels":{`, `"k":"v",`, `"z":"v"}}]}]}`, size),
		"many keys":        jsonBody(`{"responses":[{`, `"id":"a",`, `}]}`, size),
		"a long rule":      jsonBody(`{"responses":[{"id":"a","alerts":[{"rule":"`, `r`, `"}]}]}`, size),
		"numbers":          jsonBody(`{"responses":[{"id":`, `1`, `}]}`, size),
		"many valid pages": jsonBody(`{"responses":[`, `{"id":"a","alerts":[{"rule":"R"}],"sent":"2026-11-20T03:12:00Z"},`, `{"id":"b","alerts":[{"rule":"R"}],"sent":"2026-11-20T03:12:00Z"}]}`, size),
	}
	for name, body := range bodies {
		got, err := allocated(t, body, MaxFile)
		if err == nil {
			t.Errorf("%s: taken", name)
		}
		if got > bound {
			t.Errorf("%s: %d MiB allocated reading %d MiB, want %d at most", name, got>>20, len(body)>>20, bound>>20)
		}
		t.Logf("%s: %d KiB allocated, %v", name, got>>10, err)
	}
	// The byte bound itself: a body past it is refused, said by size.
	big := jsonBody(`{"responses":[`, `{"id":"a","alerts":[{"rule":"R"}],"sent":"2026-11-20T03:12:00Z"},`, `{"id":"b","alerts":[{"rule":"R"}],"sent":"2026-11-20T03:12:00Z"}]}`, 2<<20)
	if _, err := ParseJSON(bytes.NewReader(big), 1<<20); err == nil || !strings.Contains(err.Error(), "more than 1 MiB") {
		t.Errorf("past the bytes: %v", err)
	}
	// The pages bound: 100001 pages of a valid kind.
	var b strings.Builder
	b.WriteString(`{"responses":[`)
	for i := 0; i <= MaxPages; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"p%d","alerts":[{"rule":"R"}],"sent":"2026-11-20T03:12:00Z"}`, i)
	}
	b.WriteString(`]}`)
	if _, err := ParseJSON(strings.NewReader(b.String()), MaxFile); err == nil || !strings.Contains(err.Error(), "100000 pages at most") {
		t.Errorf("one past the pages: %v", err)
	}
}

func TestParseJSONShape(t *testing.T) {
	ok := `{"responses":[{"id":"a","alerts":[{"rule":"R","labels":{"k":"v"}}],"sent":"2026-11-20T03:12:00Z","resolved_by":null}]}`
	pages, err := ParseJSON(strings.NewReader(ok), 1<<20)
	if err != nil || len(pages) != 1 || pages[0].Alerts[0].Labels["k"] != "v" || !pages[0].Sent.Equal(time.Date(2026, 11, 20, 3, 12, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", pages, err)
	}
	if pages, err := ParseJSON(strings.NewReader(`{"responses":[]}`), 1<<20); err != nil || pages == nil || len(pages) != 0 {
		t.Errorf("an empty list: %v %v", pages, err)
	}
	for name, c := range map[string]struct{ text, want string }{
		"empty":            {``, ""},
		"an array":         {`[]`, "responses"},
		"no key":           {`{}`, "no responses key"},
		"another key":      {`{"incidents":[]}`, "responses: alone"},
		"a second key":     {`{"responses":[],"x":1}`, "responses: alone"},
		"key twice":        {`{"responses":[],"responses":[]}`, "twice"},
		"not a list":       {`{"responses":{}}`, "list of pages"},
		"null":             {`{"responses":null}`, "list of pages"},
		"trailing":         {`{"responses":[]} {}`, "more after"},
		"truncated":        {`{"responses":[{"id":"a"`, ""},
		"page twice":       {`{"responses":[{"id":"a","id":"b"}]}`, "twice"},
		"page unknown key": {`{"responses":[{"acked":1}]}`, `not "acked"`},
		"alert twice":      {`{"responses":[{"id":"a","alerts":[{"rule":"R","rule":"S"}]}]}`, "twice"},
		"label twice":      {`{"responses":[{"id":"a","alerts":[{"rule":"R","labels":{"k":"v","k":"w"}}]}]}`, "twice"},
		"bad time":         {`{"responses":[{"id":"a","alerts":[{"rule":"R"}],"sent":"yesterday"}]}`, "RFC 3339"},
		"alerts bounds":    {`{"responses":[{"id":"a","alerts":[{"rule":"R","labels":{"k":""}}],"sent":"2026-11-20T03:12:00Z"}]}`, "neither empty"},
	} {
		if _, err := ParseJSON(strings.NewReader(c.text), 1<<20); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestLoadRefusesWhatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Error("a directory was read")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	// Nothing writes to it: a read of it would hang.
	done := make(chan error, 1)
	go func() { _, err := Load(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("%v", err)
		}
	case <-time.After(10 * time.Second):
		// Free the blocked open, so the test ends.
		exec.Command("sh", "-c", "exec 3<>"+fifo).Run()
		t.Fatal("Load of a FIFO hung")
	}
	_ = os.Remove(fifo)
}
