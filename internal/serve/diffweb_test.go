package serve

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A diff in its options (docs/design/ui.md): unified or side by side,
// around each change or whole.

// TestShapeDiff: a diff's lines in its options. Unified, the lines the
// same more than three from a change are one row saying how many; every
// line with context=all; side by side, a run taken out beside the run put
// in after it, line by line, and the lines the same on both sides.
func TestShapeDiff(t *testing.T) {
	var a, b []string
	for i := range 20 {
		a = append(a, fmt.Sprint("line ", i))
	}
	b = slices.Clone(a)
	b[10] = "line ten"
	b = slices.Insert(b, 11, "line ten and a half")
	ds, whole := lineDiff(a, b)
	if whole {
		t.Fatal("whole")
	}
	text := func(v diffView) string {
		var out []string
		for _, r := range v.Rows {
			if r.Skip > 0 {
				out = append(out, fmt.Sprintf("~%d", r.Skip))
			} else {
				out = append(out, r.Op+r.Text)
			}
		}
		for _, p := range v.Pairs {
			if p.Skip > 0 {
				out = append(out, fmt.Sprintf("~%d", p.Skip))
			} else {
				out = append(out, p.OldOp+p.Old+"|"+p.NewOp+p.New)
			}
		}
		return strings.Join(out, "\n")
	}
	if got, want := text(shapeDiff(ds, false, diffOpts{})), "~7\n line 7\n line 8\n line 9\n-line 10\n+line ten\n+line ten and a half\n line 11\n line 12\n line 13\n~6"; got != want {
		t.Errorf("unified, around each change:\n%s\nwant\n%s", got, want)
	}
	if v := shapeDiff(ds, false, diffOpts{All: true}); len(v.Rows) != 22 || slices.ContainsFunc(v.Rows, func(r diffRow) bool { return r.Skip > 0 }) {
		t.Errorf("every line: %d rows", len(v.Rows))
	}
	if got, want := text(shapeDiff(ds, false, diffOpts{Split: true})), "~7\n line 7| line 7\n line 8| line 8\n line 9| line 9\n-line 10|+line ten\n|+line ten and a half\n line 11| line 11\n line 12| line 12\n line 13| line 13\n~6"; got != want {
		t.Errorf("side by side:\n%s\nwant\n%s", got, want)
	}
	if v := shapeDiff(ds, false, diffOpts{Split: true, All: true}); len(v.Pairs) != 21 {
		t.Errorf("side by side, every line: %d rows", len(v.Pairs))
	}
	// Nothing changed: every line, and said so.
	same, _ := lineDiff(a, a)
	if v := shapeDiff(same, false, diffOpts{}); !v.Same || len(v.Rows) != 20 {
		t.Errorf("the same: %v, %d rows", v.Same, len(v.Rows))
	}
	// The options, read: anything but split and all is the default.
	for q, want := range map[string]diffOpts{"": {}, "diff=split": {Split: true}, "context=all": {All: true}, "diff=split&context=all": {true, true},
		"diff=SPLIT&context=3": {}, "diff=<b>&context=99": {}} {
		v, _ := url.ParseQuery(q)
		if got := diffOptsOf(v); got != want {
			t.Errorf("%q: %+v, want %+v", q, got, want)
		}
	}
}

// TestVersionsDiffOptions: a rule's versions tab shows each version beside
// the one before in the same options as a proposal's page, unified unless
// asked, side by side in a table headed with the two; the options are
// what the page is cached by on that tab alone, and only those it has.
func TestVersionsDiffOptions(t *testing.T) {
	s, rules := server(t, nil)
	ctx := context.Background()
	for i := range 2 {
		os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(strings.Replace(ruleFile, "errors > 5", fmt.Sprint("errors > ", 5+i), 1)), 0o644)
		s.now = func() time.Time { return now.Add(time.Duration(i-3) * 24 * time.Hour) }
		if err := s.look(ctx, new(string)); err != nil {
			t.Fatal(err)
		}
	}
	s.now = func() time.Time { return now }
	h := s.Handler()
	_, body, _ := get(t, h, "/rules/Errors?tab=versions")
	if !strings.Contains(body, `<span class="del">-expr: errors &gt; 5</span>`+"\n"+`<span class="ins">&#43;expr: errors &gt; 6</span>`) ||
		!strings.Contains(body, `<a href="/rules/Errors?diff=split&amp;tab=versions#versions">Side by side</a>`) {
		t.Error("the versions tab isn't unified, with a link to side by side")
	}
	_, body, _ = get(t, h, "/rules/Errors?tab=versions&diff=split")
	if !strings.Contains(body, `<thead><tr><th scope="col">The version before</th><th scope="col">This version</th></tr></thead>`) ||
		!strings.Contains(body, `<tr><td class="del">-expr: errors &gt; 5</td><td class="ins">&#43;expr: errors &gt; 6</td></tr>`) {
		t.Error("side by side isn't a table of the two versions")
	}
	for q, want := range map[string]string{
		"tab=versions&diff=split&context=all": "context=all&diff=split&tab=versions",
		"tab=versions&diff=unified&context=3": "tab=versions",
		"tab=versions&diff=x&context=y":       "tab=versions",
		"diff=split&context=all":              "",
		"tab=firings&diff=split":              "tab=firings",
	} {
		v, _ := url.ParseQuery(q)
		if got := ruleQuery(v); got != want {
			t.Errorf("%q is read as %q, want %q", q, got, want)
		}
	}
}
