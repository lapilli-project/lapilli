package serve

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const eventsRecipe = "../../recipes/events/alertmanager.sh"

// eventsPort is serve's notifications port, counting the requests that
// reach it and their events, and answering 500 to the next fail of them.
type eventsPort struct {
	mu       sync.Mutex
	posts    int
	most     int
	fail     int
	h        http.Handler
	bodySize int
}

func (p *eventsPort) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	if p.fail > 0 {
		p.fail--
		p.mu.Unlock()
		http.Error(w, "down for a moment", http.StatusInternalServerError)
		return
	}
	p.posts++
	p.mu.Unlock()
	p.h.ServeHTTP(w, r)
}

func (p *eventsPort) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.posts
}

// recipeEnv is what the recipe is run with: serve's port and its token,
// and PATH, with the stand-ins in bin first.
func recipeEnv(url, token, bin string, more ...string) []string {
	path := os.Getenv("PATH")
	if bin != "" {
		path = bin + string(os.PathListSeparator) + path
	}
	// Not tried again: a test's 500 is meant.
	return append([]string{"PATH=" + path, "LAPILLI_URL=" + url, "LAPILLI_TOKEN_FILE=" + token, "HOME=" + filepath.Dir(token), "RETRIES=0"}, more...)
}

// eventsShell is the shell the recipe is run with: sh, and dash where it
// is installed, as Debian's sh is.
var eventsShell = "sh"

// eachShell runs f with each shell the recipe is run with.
func eachShell(t *testing.T, f func(t *testing.T)) {
	shells := []string{"sh"}
	if _, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, "dash")
	}
	for _, sh := range shells {
		t.Run(sh, func(t *testing.T) {
			eventsShell = sh
			defer func() { eventsShell = "sh" }()
			f(t)
		})
	}
}

func runEvents(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(eventsShell, append([]string{eventsRecipe}, args...)...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	// A test of a number that isn't one, as sh and dash say it: the
	// script went on past what it didn't mean to.
	for _, bad := range []string{"integer expression expected", "Illegal number", "unexpected operator", "arithmetic"} {
		if strings.Contains(stderr.String(), bad) {
			t.Errorf("%s said %q:\n%s", eventsShell, bad, stderr.String())
		}
	}
	return code, stderr.String()
}

func needEventTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"jq", "curl", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
}

func appendLines(t *testing.T, path string, lines [][]byte, partial []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		f.Write(append(l, '\n'))
	}
	f.Write(partial)
}

// The recipe sends the event recorder's file output on, in batches, from
// where it stopped: a line still being written waits; a file rotated is
// read to its end, then the new one; one truncated is read again from its
// start; a batch serve didn't take is sent again the next time. Each
// delivery is kept once, however often it is sent.
func TestEventsRecipeFile(t *testing.T) {
	eachShell(t, func(t *testing.T) {
		needEventTools(t)
		s := notifyServer(t, nil)
		s.now = func() time.Time { return time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC) }
		port := &eventsPort{h: s.NotifyHandler()}
		srv := httptest.NewServer(port)
		defer srv.Close()
		dir := t.TempDir()
		token := tokenFile(t, notifyToken)
		env := recipeEnv(srv.URL, token, "", "BATCH=7")
		events := filepath.Join(dir, "events.jsonl")
		lines := recorded(t, "alertmanager-0.35.0.jsonl")
		if len(lines) < 45 {
			t.Fatalf("%d lines recorded", len(lines))
		}
		kept := func(want [][]byte) {
			t.Helper()
			if got, n := kept2(t, s), notificationEvents(t, want); got != n {
				t.Errorf("%d deliveries kept, want %d", got, n)
			}
		}

		// Twenty lines, and half of the next.
		appendLines(t, events, lines[:20], lines[20][:10])
		if code, out := runEvents(t, env, "file", events); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		kept(lines[:20])
		if port.count() != 3 {
			t.Errorf("%d requests for 20 events, 7 a request", port.count())
		}
		off, _ := os.ReadFile(events + ".offset")
		var size int
		for _, l := range lines[:20] {
			size += len(l) + 1
		}
		if f := strings.Fields(string(off)); len(f) != 3 || f[1] != fmt.Sprint(size) {
			t.Errorf("the offset kept: %q, want %d", off, size)
		}

		// The rest of that line, and more.
		appendLines(t, events, nil, append(lines[20][10:], '\n'))
		appendLines(t, events, lines[21:35], nil)
		if code, out := runEvents(t, env, "file", events); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		kept(lines[:35])

		// Rotated: renamed, written to before Alertmanager opened the new one.
		if err := os.Rename(events, events+".1"); err != nil {
			t.Fatal(err)
		}
		appendLines(t, events+".1", lines[35:40], nil)
		appendLines(t, events, lines[40:], nil)
		if code, out := runEvents(t, env, "file", events); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		kept(lines)

		// serve down: nothing moves, and the next time it is sent.
		before := port.count()
		appendLines(t, events, lines[:3], nil)
		port.mu.Lock()
		port.fail = 100
		port.mu.Unlock()
		if code, out := runEvents(t, env, "file", events); code != 1 || !strings.Contains(out, "weren't sent (500)") {
			t.Errorf("serve down: exit %d: %s", code, out)
		}
		port.mu.Lock()
		port.fail = 0
		port.mu.Unlock()
		if code, out := runEvents(t, env, "file", events); code != 0 || port.count() != before+1 {
			t.Errorf("sent again: exit %d, %d requests: %s", code, port.count()-before, out)
		}
		kept(lines)

		// Truncated in place and written again: read from its start.
		if err := os.WriteFile(events, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		appendLines(t, events, lines[:2], nil)
		if code, out := runEvents(t, env, "file", events); code != 0 || !strings.Contains(out, "was truncated") {
			t.Errorf("truncated: exit %d: %s", code, out)
		}
		kept(lines)

		// The wrong token: refused, and said.
		bad := tokenFile(t, "not-the-token-at-all")
		appendLines(t, events, lines[2:4], nil)
		if code, out := runEvents(t, recipeEnv(srv.URL, bad, ""), "file", events); code != 1 || !strings.Contains(out, "(401)") || strings.Contains(out, "not-the-token") {
			t.Errorf("the wrong token: exit %d: %s", code, out)
		}
	})
}

// What isn't run: a URL with a user before @, a batch past serve's, a file
// that holds no token.
func TestEventsRecipeRefuses(t *testing.T) {
	eachShell(t, func(t *testing.T) {
		needEventTools(t)
		dir := t.TempDir()
		token := tokenFile(t, notifyToken)
		events := filepath.Join(dir, "events.jsonl")
		os.WriteFile(events, nil, 0o644)
		two := filepath.Join(dir, "two")
		os.WriteFile(two, []byte("a\nb\n"), 0o600)
		for name, env := range map[string][]string{
			"a user":       recipeEnv("http://user:pw@lapilli:8083", token, ""),
			"ftp":          recipeEnv("ftp://lapilli:8083", token, ""),
			"a query":      recipeEnv("http://lapilli:8083/?x=1", token, ""),
			"a batch":      recipeEnv("http://lapilli:8083", token, "", "BATCH=1001"),
			"two lines":    recipeEnv("http://lapilli:8083", two, ""),
			"a bad skip":   recipeEnv("http://lapilli:8083", token, "", "SKIP=a b"),
			"kafka, topic": recipeEnv("http://lapilli:8083", token, "", "KAFKA_BROKERS=k:9092", "KAFKA_TOPIC=-x", "OFFSET_FILE="+events+".o"),
		} {
			args := []string{"file", events}
			if strings.HasPrefix(name, "kafka") {
				args = []string{"kafka"}
			}
			if code, out := runEvents(t, env, args...); code != 2 || strings.Contains(out, "pw") {
				t.Errorf("%s: exit %d: %s", name, code, out)
			}
		}
	})
}

// fakeKcat stands in for kcat: it lists the topic's two partitions, gives
// a partition's messages from an offset as '%o %s', or, in a group, every
// message; and notes how it was run.
const fakeKcat = `#!/bin/sh
echo "$*" >>"$FAKE_KAFKA/args"
mode=$1
shift
p= o= g=
while [ $# -gt 0 ]; do
  case $1 in
    -p) p=$2; shift ;;
    -o) o=$2; shift ;;
    -G) g=$2; shift ;;
    -b | -t | -F | -X | -c | -f) shift ;;
  esac
  shift
done
case $mode in
  -L) printf '{"topics":[{"topic":"am-events","partitions":[{"partition":0},{"partition":1}]}]}\n' ;;
  -C)
    if [ -n "$g" ]; then
      cat "$FAKE_KAFKA/0" "$FAKE_KAFKA/1"
      exit 0
    fi
    case $o in '' | *[!0-9]*) o=0 ;; esac
    awk -v o="$o" 'NR - 1 >= o { print NR - 1 " " $0 }' "$FAKE_KAFKA/$p"
    ;;
esac
`

// The recipe consumes the recorder's Kafka output with kcat: from the
// offsets it keeps, a partition at a time, or as a consumer group's member,
// from Kafka's own.
func TestEventsRecipeKafka(t *testing.T) {
	eachShell(t, func(t *testing.T) {
		needEventTools(t)
		s := notifyServer(t, nil)
		s.now = func() time.Time { return time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC) }
		port := &eventsPort{h: s.NotifyHandler()}
		srv := httptest.NewServer(port)
		defer srv.Close()
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		kafka := filepath.Join(dir, "kafka")
		os.MkdirAll(bin, 0o755)
		os.MkdirAll(kafka, 0o755)
		if err := os.WriteFile(filepath.Join(bin, "kcat"), []byte(fakeKcat), 0o755); err != nil {
			t.Fatal(err)
		}
		lines := recorded(t, "alertmanager-0.35.0.jsonl")
		var parts [2][][]byte
		for i, l := range lines {
			parts[i%2] = append(parts[i%2], l)
		}
		for p := range parts {
			appendLines(t, filepath.Join(kafka, fmt.Sprint(p)), parts[p], nil)
		}
		token := tokenFile(t, notifyToken)
		offsets := filepath.Join(dir, "offsets")
		env := recipeEnv(srv.URL, token, bin, "FAKE_KAFKA="+kafka, "KAFKA_BROKERS=kafka-0:9092,kafka-1:9092", "KAFKA_TOPIC=am-events", "OFFSET_FILE="+offsets, "BATCH=10")
		if code, out := runEvents(t, env, "kafka"); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if got, want := kept2(t, s), notificationEvents(t, lines); got != want {
			t.Errorf("%d deliveries kept, want %d", got, want)
		}
		kept, _ := os.ReadFile(offsets)
		want := fmt.Sprintf("0 %d\n1 %d\n", len(parts[0]), len(parts[1]))
		if string(kept) != want {
			t.Errorf("offsets kept %q, want %q", kept, want)
		}
		// Again: each partition from where it stopped, and nothing to send.
		before := port.count()
		if code, out := runEvents(t, env, "kafka"); code != 0 || port.count() != before {
			t.Errorf("again: exit %d, %d requests: %s", code, port.count()-before, out)
		}
		args, _ := os.ReadFile(filepath.Join(kafka, "args"))
		if !strings.Contains(string(args), fmt.Sprintf("-p 0 -o %d", len(parts[0]))) || !strings.Contains(string(args), "-o s@") {
			t.Errorf("kcat run as:\n%s", args)
		}
		// As a group's member: Kafka keeps the offsets.
		os.Remove(filepath.Join(kafka, "args"))
		if code, out := runEvents(t, append(env, "KAFKA_GROUP=lapilli", "KAFKA_START=beginning"), "kafka"); code != 0 {
			t.Fatalf("a group: exit %d: %s", code, out)
		}
		args, _ = os.ReadFile(filepath.Join(kafka, "args"))
		if !strings.Contains(string(args), "-G lapilli -X auto.offset.reset=earliest") {
			t.Errorf("kcat run as:\n%s", args)
		}
		if got, want := kept2(t, s), notificationEvents(t, lines); got != want {
			t.Errorf("sent again in a group: %d deliveries kept, want %d", got, want)
		}
	})
}

// An event alone over a request's 1 MB, and a line that isn't an event,
// are passed over, and said; those around them are sent.
func TestEventsRecipeOversize(t *testing.T) {
	needEventTools(t)
	s := notifyServer(t, nil)
	s.now = func() time.Time { return time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC) }
	srv := httptest.NewServer(s.NotifyHandler())
	defer srv.Close()
	events := filepath.Join(t.TempDir(), "events.jsonl")
	lines := recorded(t, "alertmanager-0.35.0.jsonl")
	huge := []byte(`{"@timestamp":"2026-11-11T15:56:00Z","instance":"x","data":{"pad":"` + strings.Repeat("x", 1<<20) + `"}}`)
	appendLines(t, events, [][]byte{lines[0], huge, []byte("not an event"), lines[1]}, nil)
	appendLines(t, events, lines[2:], nil)
	code, out := runEvents(t, recipeEnv(srv.URL, tokenFile(t, notifyToken), ""), "file", events)
	if code != 0 || !strings.Contains(out, "1 events over a request's 1 MB, passed over") || !strings.Contains(out, "1 lines that aren't an event left out") {
		t.Errorf("exit %d: %s", code, out)
	}
	if got, want := kept2(t, s), notificationEvents(t, lines); got != want {
		t.Errorf("%d deliveries kept, want %d", got, want)
	}
}

// A batch whose body jq writes longer than serve takes (a DEL is six bytes
// as \u007f) is sent one event at a time, and an event serve can't take
// alone is passed over: the run goes on, where it stopped there for good.
func TestEventsRecipeLongBody(t *testing.T) {
	needEventTools(t)
	eachShell(t, func(t *testing.T) {
		s := notifyServer(t, nil)
		s.now = func() time.Time { return time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC) }
		srv := httptest.NewServer(s.NotifyHandler())
		defer srv.Close()
		events := filepath.Join(t.TempDir(), "events.jsonl")
		lines := recorded(t, "alertmanager-0.35.0.jsonl")
		var notes [][]byte
		for _, l := range lines {
			if bytes.Contains(l, []byte(`"notification"`)) {
				notes = append(notes, l)
			}
		}
		pad := func(l []byte, n int) []byte {
			return append([]byte(`{"pad":"`+strings.Repeat("\x7f", n)+`",`), l[1:]...)
		}
		// Four of 100 KB, 2.4 MB as jq writes them; one of 300 KB, 1.8 MB
		// alone, which serve can't take.
		appendLines(t, events, [][]byte{pad(notes[0], 100<<10), pad(notes[1], 100<<10), pad(notes[2], 100<<10), pad(notes[3], 100<<10)}, nil)
		appendLines(t, events, [][]byte{pad(notes[4], 300<<10)}, nil)
		appendLines(t, events, notes[5:7], nil)
		code, out := runEvents(t, recipeEnv(srv.URL, tokenFile(t, notifyToken), "", "BATCH=4"), "file", events)
		if code != 0 || !strings.Contains(out, "sent one at a time") || !strings.Contains(out, "1 events serve can't take, passed over (413)") {
			t.Fatalf("exit %d: %s", code, out)
		}
		if got := kept2(t, s); got != 6 {
			t.Errorf("%d deliveries kept, want 6", got)
		}
	})
}
