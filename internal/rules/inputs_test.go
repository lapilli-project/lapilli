package rules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNamespacesApart: a ruler's namespaces are each an input, what each
// may make its own, and together bounded: one that makes much within its
// own bound doesn't have another, innocent, refused in its place, as one
// shared bound did; one refused on its own is refused alone; and what
// refused ones make isn't given back, so twenty of them made 2 GB, and now
// a few MB, and past MaxMade in all the read is refused whole (OverTotal).
func TestNamespacesApart(t *testing.T) {
	read := NoAliases(Together(context.Background()))
	heavy := groupBomb(1000, 33, false, false)
	innocent := "groups:\n- name: b\n  labels: {team: b}\n  rules:\n  - {alert: B, expr: up == 0}\n"
	for _, d := range []Doc{{Source: "aaa", Namespace: "aaa", Data: []byte(heavy)}, {Source: "team-b", Namespace: "team-b", Data: []byte(innocent)}} {
		if rs, err := FromDocsContext(read, []Doc{d}); err != nil || len(rs) == 0 {
			t.Errorf("%s: %d, %v", d.Source, len(rs), err)
		}
	}
	bomb := "apiVersion: monitoring.googleapis.com/v1\nkind: Rules\nmetadata: {name: r, namespace: " + strings.Repeat("n", 16<<10) +
		"}\nspec:\n  groups:\n  - name: g\n    rules:\n    - alert: X\n      expr: " + strconv.Quote(selectors(4000)) + "\n"
	refused := 0
	bytes, took := allocated(func() {
		for i := range 20 {
			_, err := FromDocsContext(read, []Doc{{Source: fmt.Sprint("ns", i), Namespace: fmt.Sprint("ns", i), Data: []byte(bomb)}})
			if errors.Is(err, err) && err != nil && !OverTotal(err) {
				refused++
			}
		}
	})
	t.Logf("20 namespaces refused each on its own: %d MB allocated, %v", bytes>>20, took)
	if refused != 20 || bytes > 128<<20 {
		t.Errorf("%d refused, %d MB allocated", refused, bytes>>20)
	}
	if rs, err := FromDocsContext(read, []Doc{{Source: "after", Namespace: "after", Data: []byte(innocent)}}); err != nil || len(rs) != 1 {
		t.Errorf("after them: %d, %v", len(rs), err)
	}
	// Each within its own bound, all of them past MaxMade.
	read = NoAliases(Together(context.Background()))
	var err error
	for i := 0; i < 100 && err == nil; i++ {
		_, err = FromDocsContext(read, []Doc{{Source: fmt.Sprint("ns", i), Namespace: fmt.Sprint("ns", i), Data: []byte(heavy)}})
	}
	if !OverTotal(err) {
		t.Errorf("100 namespaces of 3.9 MB made each: %v", err)
	}
}

// TestReadInItsInputs: a version of the rules kept in the ledger is read
// again in the inputs it was read in: its files together, a Loki's apart,
// each ruler's namespace or alarm on its own; two documents each read
// alone, each some 3.6 MB made, were refused read again as one. And a file
// below a directory's top passed over gives back what it counted, so the
// files after it read as they would without it.
func TestReadInItsInputs(t *testing.T) {
	doc := groupBomb(1000, 30, false, false)
	docs := []Doc{{Source: "rules.yaml", Data: []byte(doc)}, {Source: "loki.yaml", Logs: LokiFile, Data: []byte(doc)},
		{Source: "ns", Namespace: "ns", Data: []byte(doc)}}
	if rs, err := FromDocs(docs); err != nil || len(rs) != 90 {
		t.Errorf("a version of three sources: %d, %v", len(rs), err)
	}
	if _, err := FromDocs(docs[:1:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := FromDocs([]Doc{docs[0], {Source: "more.yaml", Data: []byte(doc)}}); !refused(err) {
		t.Errorf("two files, read as one input as a directory is: %v", err)
	}

	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.yaml", groupBomb(1000, 15, false, false))
	write("sub/b.yaml", groupBomb(1000, 15, false, false)+"  - {alert: Bad, expr: up, for: 1x}\n")
	write("sub/c.yaml", groupBomb(1000, 10, false, false))
	if rs, _, _, err := ReadPicked(dir, Picking{}); err != nil || len(rs) != 25 {
		t.Errorf("a file passed over: %d, %v", len(rs), err)
	}
}

// TestTotalInAnyOrder: what a ruler's namespaces make together counts what
// each made, not a charge refused, which isn't made: a namespace whose name
// on each of its 4,200 rules would be 275 MB is refused on its own, and the
// next is read, in either order. Before, the refused charge was counted, so
// the next namespace, whatever it was, failed the whole read with an alias
// it didn't have ("line 0: aliases, * the last"), and read last, the
// refused one left the read whole. Past what they may make together, the
// namespace that took them past is named, in either order.
func TestTotalInAnyOrder(t *testing.T) {
	var b strings.Builder
	b.WriteString("groups:\n- name: g\n  rules:\n")
	for range 4200 {
		b.WriteString("  - {alert: a, expr: x}\n")
	}
	atk := Doc{Source: "atk", Namespace: strings.Repeat("n", 64<<10), Data: []byte(b.String())}
	ok := Doc{Source: "ok", Namespace: "shop", Data: []byte("groups:\n- name: h\n  rules:\n  - {alert: Real, expr: up}\n")}
	for _, order := range [][]Doc{{atk, ok}, {ok, atk}} {
		read := NoAliases(Together(context.Background()))
		for _, d := range order {
			_, err := FromDocsContext(read, []Doc{d})
			switch {
			case d.Source == "atk" && (!refused(err) || OverTotal(err) || !strings.Contains(err.Error(), "atk: its namespace")):
				t.Errorf("%s first: atk: %v", order[0].Source, err)
			case d.Source == "ok" && err != nil:
				t.Errorf("%s first: ok: %v", order[0].Source, err)
			}
		}
	}
	heavy := groupBomb(1000, 33, false, false)
	for _, reversed := range []bool{false, true} {
		read := NoAliases(Together(context.Background()))
		var err error
		for i := 0; i < 100 && err == nil; i++ {
			n := i
			if reversed {
				n = 99 - i
			}
			_, err = FromDocsContext(read, []Doc{{Source: fmt.Sprintf("ns%02d", n), Namespace: fmt.Sprintf("ns%02d", n), Data: []byte(heavy)}})
		}
		if !OverTotal(err) || !strings.HasPrefix(err.Error(), "ns") || strings.Contains(err.Error(), "aliases") {
			t.Errorf("reversed %t: %v", reversed, err)
		}
	}
}

// TestTotalNamesMost: past what a ruler's namespaces may make together,
// the read names the namespace that took them past and the one that had
// made the most, which may be another: the one that crosses may have made
// little. The one that crosses is the most when what it made in reading
// is the most, counted before it is done.
func TestTotalNamesMost(t *testing.T) {
	read := NoAliases(Together(context.Background()))
	if _, err := FromDocsContext(read, []Doc{{Source: "big", Namespace: "big", Data: []byte(groupBomb(1000, 33, false, false))}}); err != nil {
		t.Fatal(err)
	}
	small := groupBomb(1000, 20, false, false)
	var err error
	for i := 0; i < 200 && err == nil; i++ {
		_, err = FromDocsContext(read, []Doc{{Source: fmt.Sprintf("small%03d", i), Namespace: fmt.Sprintf("small%03d", i), Data: []byte(small)}})
	}
	if !OverTotal(err) || !strings.HasPrefix(err.Error(), "small") || !strings.Contains(err.Error(), "; big made the most of it") {
		t.Errorf("a big one, then small ones: %v", err)
	}

	// By aliases, which a namespace counts once it is read: the one that
	// crosses, having made the most in reading, is named the most.
	aliases := func(n int) []byte {
		return []byte("x: &s " + strings.Repeat("s", 64<<10) + "\ny: [" + strings.TrimSuffix(strings.Repeat("*s, ", n), ", ") +
			"]\ngroups: [{name: g, rules: [{alert: A, expr: up}]}]\n")
	}
	read = OneInput(Together(context.Background()))
	for i := 0; i < 200 && totalOf(read).left() > 3<<20; i++ {
		if _, err := FromDocsContext(read, []Doc{{Source: fmt.Sprintf("ns%03d", i), Namespace: fmt.Sprintf("ns%03d", i), Data: aliases(30)}}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = FromDocsContext(read, []Doc{{Source: "zz", Namespace: "zz", Data: aliases(60)}})
	if !OverTotal(err) || !strings.HasPrefix(err.Error(), "zz") || !strings.Contains(err.Error(), "; zz made the most of it") {
		t.Errorf("a big one by aliases, after small ones: %v", err)
	}
}
