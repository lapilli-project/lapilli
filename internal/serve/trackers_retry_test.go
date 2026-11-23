package serve

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// retryRig is a tracker's recipe (name) against its stand-in, and
// Lapilli's incidents port, each behind tracker and lapilli: given the
// request, and the handler it goes to, either answers it.
func retryRig(t *testing.T, name string, tracker, lapilli func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc)) *trackerRig {
	t.Helper()
	api := trackerAPIs[name]
	s := trackerServer(t, nil)
	in := s.IncidentsHandler()
	lap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { lapilli(w, r, in.ServeHTTP) }))
	t.Cleanup(lap.Close)
	f := &fakeTracker{t: t}
	inner := api.serve(f)
	trk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tracker(w, r, inner) }))
	t.Cleanup(trk.Close)
	env := append(trackerEnv(), api.env(trk.URL, tokenFile(t, trackerSecrets[name]))...)
	env = append(env, "LAPILLI_URL="+lap.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, trTasksTok))
	return &trackerRig{s: s, f: f, api: api, env: env}
}

// passed sends each request on.
func passed(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) { next(w, r) }

// runTracker runs the rig's recipe with the shell the recipes are tested
// with (recipeShell).
func runTracker(g *trackerRig, more ...string) (string, error) {
	cmd := exec.Command(recipeShell(), trackerRecipes+g.api.script)
	cmd.Env = append(slices.Clone(g.env), more...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// quickCurl is a directory whose curl is this machine's, given --max-time
// 1 in place of the recipe's 30.
func quickCurl(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl here")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
n=$#
while [ "$n" -gt 0 ]; do
  a=$1
  shift
  n=$((n - 1))
  if [ "$a" = --max-time ]; then
    shift
    n=$((n - 1))
    set -- "$@" --max-time 1
    continue
  fi
  set -- "$@" "$a"
done
exec '` + real + `' "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A tracker's request answered 502 was asked again whatever its method:
// an issue the tracker made, behind a proxy that answered 502, was made
// a second time, and the task linked to the second. So was one answered
// 503, which a proxy (Envoy) answers after the tracker has the request. A
// write that makes something is asked again now only where it can't have
// been taken, a 408 or 429, or a connection refused; otherwise it fails,
// and the next run finds the issue made by its marker.
func TestTrackerRecipePostAnsweredBadly(t *testing.T) {
	needTools(t)
	ctx := context.Background()
	for _, c := range []struct {
		code  int
		taken bool
	}{{http.StatusBadGateway, true}, {http.StatusGatewayTimeout, true}, {http.StatusServiceUnavailable, true}, {http.StatusTooManyRequests, false}} {
		var mu sync.Mutex
		posts, first := 0, true
		g := retryRig(t, "github", func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") {
				mu.Lock()
				posts++
				was := first
				first = false
				mu.Unlock()
				if was {
					if c.taken {
						next(httptest.NewRecorder(), r) // made
					}
					http.Error(w, "upstream", c.code)
					return
				}
			}
			next(w, r)
		}, passed)
		id := g.put(t, ledger.Task{Title: "Made here"})
		// What the tracker holds: each issue's marker, and how many have it.
		held := func() map[string]int {
			g.f.mu.Lock()
			defer g.f.mu.Unlock()
			out := map[string]int{}
			for _, i := range g.f.issues {
				for _, line := range strings.Split(i.body, "\n") {
					if strings.HasPrefix(line, "Kept in step with Lapilli: ") {
						out[line]++
					}
				}
			}
			return out
		}
		once := func(m map[string]int) bool {
			for _, n := range m {
				if n != 1 {
					return false
				}
			}
			return len(m) == 3 // tasks 1, 2 and the one put here
		}
		out, _ := runTracker(g)
		if m := held(); !once(m) {
			t.Errorf("%d: after the first run, the tracker holds %v\n%s", c.code, m, out)
		}
		// The next run: the issue made is found, and none made again.
		out, err := runTracker(g)
		for _, n := range []int64{1, 2, id} {
			if task, terr := g.s.l.TaskByID(ctx, n); terr != nil || task.Link == "" {
				t.Errorf("%d, the next run: task %d linked to %q, %v", c.code, n, task.Link, terr)
			}
		}
		if m := held(); err != nil || !once(m) {
			t.Errorf("%d, the next run: %v, the tracker holds %v\n%s", c.code, err, m, out)
		}
		// One POST an issue, and the one refused asked again where it
		// wasn't taken.
		want := 3
		if !c.taken {
			want = 4
		}
		mu.Lock()
		if posts != want {
			t.Errorf("%d: %d POSTs, want %d", c.code, posts, want)
		}
		mu.Unlock()
	}
}

// Lapilli's requests were asked again by curl's --retry: a comment copied
// to a task, which Lapilli took and a proxy answered 502, was sent again
// three times; and a read that timed out midway had the next try's answer
// written after its half, which wasn't then read. A POST is asked again
// as the tracker's are, and each try's answer is the last alone.
func TestTrackerRecipeLapilliAskedAgain(t *testing.T) {
	needTools(t)
	ctx := context.Background()
	t.Run("a comment copied, answered 502", func(t *testing.T) {
		var mu sync.Mutex
		copies := 0
		g := retryRig(t, "github", passed, func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
				mu.Lock()
				copies++
				n := copies
				mu.Unlock()
				if n == 1 {
					next(httptest.NewRecorder(), r) // taken
					http.Error(w, "upstream", http.StatusBadGateway)
					return
				}
			}
			next(w, r)
		})
		if out, err := runTracker(g); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		g.f.mu.Lock()
		g.f.comment(g.f.issue(1), "someone", "Raised it.")
		g.f.mu.Unlock()
		out, _ := runTracker(g, "COMMENTS=true")
		mu.Lock()
		n := copies
		mu.Unlock()
		if n != 1 || !strings.Contains(out, "couldn't be copied") {
			t.Errorf("the comment sent %d times\n%s", n, out)
		}
		if out, err := runTracker(g, "COMMENTS=true"); err != nil || !strings.Contains(out, "copied 0 comments") {
			t.Errorf("the next run: %v\n%s", err, out)
		}
		if cs, _ := g.s.l.Comments(ctx, "task", "1"); len(cs) != 1 {
			t.Errorf("the task's comments: %+v", cs)
		}
	})
	t.Run("a read timed out midway", func(t *testing.T) {
		t.Setenv("PATH", quickCurl(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
		var mu sync.Mutex
		first := true
		g := retryRig(t, "github", passed, func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
			mu.Lock()
			was := first && r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks"
			if was {
				first = false
			}
			mu.Unlock()
			if was {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"tasks": [`))
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(10 * time.Second):
				}
				return
			}
			next(w, r)
		})
		if out, err := runTracker(g); err != nil || !strings.Contains(out, "made 2 issues") {
			t.Errorf("%v\n%s", err, out)
		}
	})
}

// badFirst answers 502 to the first POST that is takes for one, and
// passes the rest on, that first one taken where taken says; asked counts
// the POSTs is takes.
func badFirst(is func(r *http.Request, body string) bool, taken bool, asked *atomic.Int64) func(http.ResponseWriter, *http.Request, http.HandlerFunc) {
	return func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		if r.Method == http.MethodPost && is(r, string(b)) && asked.Add(1) == 1 {
			if taken {
				next(httptest.NewRecorder(), r)
			}
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		next(w, r)
	}
}

// Linear asks each request as a POST, its reads among them, and Azure
// Boards its search: a read answered 502 was taken, by its method, for a
// write that may have been made, and not asked again, and the run stopped
// at the search that failed. A request is asked again or once as its
// caller says: a read answered 502 is asked again, and the run goes on;
// an issue made and answered 502 isn't made again, and the next run finds
// it.
func TestTrackerRecipePostsThatRead(t *testing.T) {
	needTools(t)
	ctx := context.Background()
	for _, c := range []struct {
		name         string
		read, create func(r *http.Request, body string) bool
	}{
		{"linear",
			func(r *http.Request, body string) bool { return strings.Contains(body, "issues(first: 20") },
			func(r *http.Request, body string) bool { return strings.Contains(body, "issueCreate") }},
		{"azure-boards",
			func(r *http.Request, body string) bool {
				return strings.HasSuffix(r.URL.EscapedPath(), "/_apis/wit/wiql")
			},
			func(r *http.Request, body string) bool {
				return strings.HasSuffix(r.URL.EscapedPath(), "/_apis/wit/workitems/%24Task")
			}},
	} {
		t.Run(c.name+", a read answered 502", func(t *testing.T) {
			var asked atomic.Int64
			g := retryRig(t, c.name, badFirst(c.read, false, &asked), passed)
			// A search for each of the two tasks, the first asked again.
			if out, err := runTracker(g); err != nil || !strings.Contains(out, "made 2 issues") || asked.Load() != 3 {
				t.Errorf("%v, searched %d times, want 3\n%s", err, asked.Load(), out)
			}
		})
		t.Run(c.name+", an issue made, answered 502", func(t *testing.T) {
			var asked atomic.Int64
			g := retryRig(t, c.name, badFirst(c.create, true, &asked), passed)
			runTracker(g)
			out, err := runTracker(g)
			for _, n := range []int64{1, 2} {
				if task, terr := g.s.l.TaskByID(ctx, n); terr != nil || task.Link == "" {
					t.Errorf("the next run: task %d linked to %q, %v", n, task.Link, terr)
				}
			}
			g.f.mu.Lock()
			made := g.f.creates
			g.f.mu.Unlock()
			if err != nil || made != 2 || asked.Load() != 2 {
				t.Errorf("the next run: %v; %d issues made, asked %d times, want 2 and 2\n%s", err, made, asked.Load(), out)
			}
		})
	}
}
