package egress

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
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
// without it would send around a guard, unseen. Read from the source: an
// http.Client made whose Transport isn't egress.Wrap's, one's Transport
// set otherwise after, http.DefaultClient, or http.Get and its kind.
func TestEveryClientWrapped(t *testing.T) {
	var files []string
	for _, root := range []string{"../../internal", "../../cmd"} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				files = append(files, p)
			}
			return nil
		})
	}
	clients := 0
	for _, f := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range unwrapped(file, &clients) {
			t.Errorf("%s: %s", fset.Position(bad.pos), bad.what)
		}
	}
	if clients < 10 {
		t.Errorf("%d clients found: the walk read too little", clients)
	}
}

type found struct {
	pos  token.Pos
	what string
}

// unwrapped finds, in a file, what sends around Wrap; clients counts the
// clients made.
func unwrapped(file *ast.File, clients *int) []found {
	names := map[string]string{} // an import's name here to its path
	for _, im := range file.Imports {
		path, _ := strconv.Unquote(im.Path.Value)
		name := path[strings.LastIndex(path, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		names[name] = path
	}
	of := func(e ast.Expr, path, sel string) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != sel {
			return false
		}
		x, ok := s.X.(*ast.Ident)
		return ok && names[x.Name] == path
	}
	wrapped := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		if id, ok := c.Fun.(*ast.Ident); ok && file.Name.Name == "egress" && id.Name == "Wrap" {
			return true
		}
		return of(c.Fun, "lapilli.dev/internal/egress", "Wrap")
	}
	var out []found
	made := map[string]bool{} // the variables a client was made into
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if !of(n.Type, "net/http", "Client") {
				return true
			}
			*clients++
			ok := false
			for _, e := range n.Elts {
				if kv, isKV := e.(*ast.KeyValueExpr); isKV {
					if k, isID := kv.Key.(*ast.Ident); isID && k.Name == "Transport" {
						ok = wrapped(kv.Value)
					}
				}
			}
			if !ok {
				out = append(out, found{n.Pos(), "an http.Client whose Transport isn't egress.Wrap's"})
			}
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if i >= len(n.Rhs) {
					break
				}
				if id, ok := l.(*ast.Ident); ok {
					if u, ok := n.Rhs[i].(*ast.UnaryExpr); ok {
						if c, ok := u.X.(*ast.CompositeLit); ok && of(c.Type, "net/http", "Client") {
							made[id.Name] = true
						}
					}
				}
				if s, ok := l.(*ast.SelectorExpr); ok && s.Sel.Name == "Transport" {
					if x, ok := s.X.(*ast.Ident); ok && made[x.Name] && !wrapped(n.Rhs[i]) {
						out = append(out, found{n.Pos(), "a client's Transport set to what isn't egress.Wrap's"})
					}
				}
			}
		case *ast.SelectorExpr:
			for _, sel := range []string{"DefaultClient", "Get", "Post", "Head", "PostForm"} {
				if of(n, "net/http", sel) {
					out = append(out, found{n.Pos(), "http." + sel + ", which sends around Wrap"})
				}
			}
		}
		return true
	})
	return out
}

// The test above finds what sends around Wrap.
func TestUnwrappedFinds(t *testing.T) {
	src := `package x
import (
	h "net/http"
	"lapilli.dev/internal/egress"
)
var a = &h.Client{}
var b = &h.Client{Transport: h.DefaultTransport}
var ok = &h.Client{Transport: egress.Wrap(nil)}
func f() {
	c := &h.Client{Transport: egress.Wrap(nil)}
	c.Transport = &h.Transport{}
	h.Get("http://x")
	_ = h.DefaultClient
}`
	file, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	clients := 0
	if got := unwrapped(file, &clients); len(got) != 5 || clients != 4 {
		t.Errorf("found %d, %d clients: %v", len(got), clients, got)
	}
}
