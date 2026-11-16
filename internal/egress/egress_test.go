package egress

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sent struct {
	n    int
	body string
}

func (s *sent) RoundTrip(r *http.Request) (*http.Response, error) {
	s.n++
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		s.body = string(b)
	}
	return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
}

// A guard sees each request before it is sent: one it refuses isn't, and
// one it lets go is sent whole, a body it read included. Without a guard,
// Wrap only sends.
func TestGuard(t *testing.T) {
	base := &sent{}
	rt := Wrap(base)
	var seen []string
	release := Guard(func(r *http.Request) error {
		b, _ := r.GetBody()
		raw, _ := io.ReadAll(b)
		seen = append(seen, string(raw))
		if strings.Contains(string(raw), "write") {
			return errors.New("no")
		}
		return nil
	})
	if !Guarded() {
		t.Fatal("not guarded")
	}
	// A body without GetBody, as an SDK may send.
	r, _ := http.NewRequest("POST", "http://api.example/", io.NopCloser(strings.NewReader("read")))
	r.GetBody = nil
	if _, err := rt.RoundTrip(r); err != nil || base.n != 1 || base.body != "read" {
		t.Fatalf("let go: %v, sent %d %q", err, base.n, base.body)
	}
	r, _ = http.NewRequest("POST", "http://api.example/", strings.NewReader("write"))
	if _, err := rt.RoundTrip(r); err == nil || base.n != 1 {
		t.Fatalf("refused: %v, sent %d", err, base.n)
	}
	release()
	if Guarded() {
		t.Fatal("still guarded")
	}
	r, _ = http.NewRequest("POST", "http://api.example/", strings.NewReader("write"))
	if _, err := rt.RoundTrip(r); err != nil || base.n != 2 || len(seen) != 2 {
		t.Fatalf("unguarded: %v, sent %d, seen %d", err, base.n, len(seen))
	}
	if Unwrap(rt) != base || Unwrap(base) != base {
		t.Error("Unwrap")
	}
}

// Every HTTP client lapilli makes sends through Wrap: a client made
// without it would send around a guard, unseen.
func TestEveryClientWrapped(t *testing.T) {
	var files []string
	for _, root := range []string{"../../internal", "../../cmd"} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") && !strings.Contains(p, "test/") {
				files = append(files, p)
			}
			return nil
		})
	}
	found := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if !strings.Contains(l, "http.Client{") {
				continue
			}
			found++
			near := strings.Join(lines[i:min(i+4, len(lines))], "\n")
			if !strings.Contains(near, "Transport:") || !strings.Contains(near, "egress.Wrap(") && !strings.Contains(near, "Wrap(") {
				t.Errorf("%s:%d: an http.Client not made with egress.Wrap", f, i+1)
			}
		}
	}
	if found < 8 {
		t.Errorf("%d clients found: the walk read too little", found)
	}
}
