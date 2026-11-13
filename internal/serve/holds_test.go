package serve

import (
	"context"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
)

// takeLedger ends the session that holds a server's ledger's lock, and
// opens the ledger in another process's stead, which takes it: the server
// no longer holds it, and doesn't know yet.
func takeLedger(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	db := ledgerDB(t, s)
	defer db.Close()
	const here = "locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())"
	if _, err := db.ExecContext(ctx, "SELECT pg_terminate_backend(pid, 5000) FROM pg_locks WHERE "+here); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.cfg.LedgerURLFile)
	other, err := ledger.OpenPostgres(ctx, strings.TrimSpace(string(b)), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
}

// The week's digest is posted only while this process holds the ledger:
// one whose writer's session went, while another process took the ledger
// and will post it, posts nothing.
func TestDigestHoldsTheLedger(t *testing.T) {
	if !ledgertest.On() {
		t.Skip("the ledgers are SQLite's")
	}
	var posted atomic.Int64
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posted.Add(1) }))
	defer recv.Close()
	hook := filepath.Join(t.TempDir(), "slack")
	os.WriteFile(hook, []byte(recv.URL+"/slack"), 0o600)
	s, _ := server(t, func(c *Config) {
		c.Slack = hook
		c.LedgerURLFile = ledgertest.DatabaseFile(t)
	})
	takeLedger(t, s)
	to := time.Now().UTC().Truncate(24 * time.Hour)
	s.postDigest(context.Background(), digestIn{On: true, Dests: []string{"slack"}}, "2950", to.Add(-7*24*time.Hour), to, s.cfg.Options)
	if n := posted.Load(); n != 0 {
		t.Errorf("a process whose lock another took posted the digest %d times", n)
	}
}

// Every way serve's code sends out of the process is one of these, and
// each that tells people or another system what the ledger says does so
// only while this process holds the ledger (Ledger.Hold, before it), or a
// process that lost the ledger and the one that took it both send it. A
// new one fails this test until it is held, or said here why it needn't be.
func TestSendersHoldTheLedger(t *testing.T) {
	ways := map[string]string{
		"send":        "a notice of the outbox: sendDue holds the ledger before each",
		"postDigest":  "the week's digest: holds the ledger before it posts",
		"grafanaCall": "a change applied: Move takes the proposal through the writer before it is applied",
		"ociGet":      "a subscription confirmed, as an incoming request asks: fetching it twice confirms it once",
		"snsGet":      "a subscription confirmed, as an incoming request asks: fetching it twice confirms it once",
		"asked":       "a page's view an agent asks for, served in the process: nothing leaves it",
	}
	held := map[string]string{"send": "sendDue", "postDigest": "postDigest"} // where Hold comes first
	senders := map[string]bool{"Slack": true, "SlackMessage": true, "SlackFrom": true, "Webhook": true, "Push": true}
	fs := gotoken.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string]*ast.FuncDecl{}
	type way struct {
		fn   string
		call gotoken.Pos
		what string
	}
	var found []way
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fs, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			funcs[fd.Name.Name] = fd
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if ok && (x.Name == "report" && senders[sel.Sel.Name] || x.Name == "http" && strings.HasPrefix(sel.Sel.Name, "NewRequest")) {
					found = append(found, way{fd.Name.Name, c.Pos(), x.Name + "." + sel.Sel.Name})
				}
				return true
			})
		}
	}
	// calls says where in a function a call to name is made, by its
	// selector's name: s.l.Hold is "Hold".
	calls := func(fd *ast.FuncDecl, name string) []gotoken.Pos {
		var at []gotoken.Pos
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
					at = append(at, c.Pos())
				}
			}
			return true
		})
		return at
	}
	for _, w := range found {
		if _, ok := ways[w.fn]; !ok {
			t.Errorf("%s: %s in %s, a way out of the process this test doesn't know: hold the ledger before it, and say so here", fs.Position(w.call), w.what, w.fn)
			continue
		}
		by, ok := held[w.fn]
		if !ok {
			continue
		}
		fd := funcs[by]
		at := w.call
		if by != w.fn {
			sends := calls(fd, w.fn)
			if len(sends) == 0 {
				t.Errorf("%s doesn't call %s", by, w.fn)
				continue
			}
			at = sends[0]
		}
		holds := calls(fd, "Hold")
		if len(holds) == 0 || holds[0] > at {
			t.Errorf("%s: %s sends (%s) before it holds the ledger", fs.Position(at), by, w.what)
		}
	}
}
