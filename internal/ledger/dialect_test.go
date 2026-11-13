package ledger

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sqliteOnly are words of SQLite's that Postgres doesn't take, or takes
// to mean something else (json_each is Postgres's too, of json).
var sqliteOnly = regexp.MustCompile(`json_each|json_extract|json_valid|json_type|json_array|json_object|json_group|PRAGMA|pragma_|GLOB|\browid\b|` +
	`INSERT OR |UPDATE OR |REPLACE INTO|\binstr\(|\bifnull\(|\biif\(|group_concat|sqlite_|AUTOINCREMENT|WITHOUT ROWID|strftime|julianday|` +
	`unixepoch|typeof\(|\btotal\(|randomblob|zeroblob`)

// sqliteFuncs are the functions that run on SQLite alone.
var sqliteFuncs = map[string]bool{"migrate": true}

// Every statement Postgres takes otherwise than SQLite goes through the
// dialect (dialect.go): SQLite's words appear in a pick's first argument,
// in dialect.go, or in a function only SQLite runs, and nowhere else; and
// nothing asks for LastInsertId, which pgx doesn't give. A statement added
// in SQLite's words alone fails here, without a Postgres server to run it
// on.
func TestDialectSeam(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "dialect.go" {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range unnamedSubqueries(string(src)) {
			t.Errorf("%s:%d: a subquery in FROM without a name: Postgres before 16 refuses it (FROM (...) AS x)", f, at)
		}
		allowed := map[*ast.BasicLit]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if sqliteFuncs[n.Name.Name] {
					ast.Inspect(n, func(m ast.Node) bool {
						if lit, ok := m.(*ast.BasicLit); ok {
							allowed[lit] = true
						}
						return true
					})
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "pick" && len(n.Args) == 2 {
					ast.Inspect(n.Args[0], func(m ast.Node) bool {
						if lit, ok := m.(*ast.BasicLit); ok {
							allowed[lit] = true
						}
						return true
					})
				}
			case *ast.SelectorExpr:
				if n.Sel.Name == "LastInsertId" {
					t.Errorf("%s: LastInsertId: pgx gives none; INSERT ... RETURNING id", fset.Position(n.Pos()))
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			checked++
			if w := sqliteOnly.FindString(s); w != "" && !allowed[lit] {
				t.Errorf("%s: %q is SQLite's alone: give Postgres's beside it (dialect.pick), or a dialect method", fset.Position(lit.Pos()), w)
			}
			if twoArgs(s) {
				t.Errorf("%s: max or min of two is SQLite's; Postgres's is greatest or least (dialect.greatest)", fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if checked < 500 {
		t.Fatalf("only %d strings read: the files weren't", checked)
	}
}

// unnamedSubqueries are the lines of a file's statements with a subquery
// in FROM or a JOIN that has no name after it. The statements are read
// across the literals they are put together from, roughly.
func unnamedSubqueries(src string) []int {
	var at []int
	name := regexp.MustCompile("^[\\s`\"+]*(AS\\s+)?[a-z_][a-z0-9_]*")
	clause := regexp.MustCompile("^[\\s`\"+]*(WHERE|GROUP|ORDER|LIMIT|UNION|ON|AND|OR|JOIN|LEFT)\\b")
	for _, m := range regexp.MustCompile(`\b(FROM|JOIN)\s*\(`).FindAllStringIndex(src, -1) {
		depth, j := 0, m[1]-1
		for ; j < len(src); j++ {
			if src[j] == '(' {
				depth++
			} else if src[j] == ')' {
				if depth--; depth == 0 {
					break
				}
			}
		}
		rest := src[min(j+1, len(src)):]
		if !name.MatchString(rest) || clause.MatchString(rest) {
			at = append(at, strings.Count(src[:m[0]], "\n")+1)
		}
	}
	return at
}

// twoArgs says a statement takes the max or min of two or more, not of a
// column.
func twoArgs(s string) bool {
	for _, m := range regexp.MustCompile(`(?i)\b(max|min)\(`).FindAllStringIndex(s, -1) {
		depth := 0
		for _, c := range s[m[1]-1:] {
			switch c {
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 1 {
					return true
				}
			}
			if depth == 0 {
				break
			}
		}
	}
	return false
}

func TestTwoArgs(t *testing.T) {
	for s, want := range map[string]bool{
		"max(a)": false, "max(a, b)": true, "min(coalesce(a, b))": false, "max(f(a, b), c)": true, "SELECT max(x) FROM t WHERE y IN (1, 2)": false,
	} {
		if twoArgs(s) != want {
			t.Errorf("%s: %v", s, !want)
		}
	}
}
