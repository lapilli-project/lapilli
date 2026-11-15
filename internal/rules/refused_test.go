package rules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMadeInputWide: what an input makes is bounded for the input, not
// for each of its documents: 200 documents of 21 KB, each of which alone
// reads (its group's 2,000 labels on 18 rules, some 3.9 MB made, within
// the 4 MiB any input may make), are 4 MB together, and refused as one
// input, as FromDocs reads a version's documents, and as ReadPicked reads
// a directory's files, in a directory below the top too. Before: per
// document, the 200 made 1.9 GB when each rule was hashed.
func TestMadeInputWide(t *testing.T) {
	doc := groupBomb(2000, 18, false, false)
	if rs, err := Parse([]byte(doc)); err != nil || len(rs) != 18 {
		t.Fatalf("one document alone: %d, %v", len(rs), err)
	}
	var docs []Doc
	dir := t.TempDir()
	below := filepath.Join(dir, "below")
	if err := os.Mkdir(below, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		docs = append(docs, Doc{Source: fmt.Sprint("d", i), Data: []byte(doc)})
		if err := os.WriteFile(filepath.Join(below, fmt.Sprintf("f%03d.yaml", i)), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var made *MadeError
	var err error
	bytes, took := allocated(func() { _, err = FromDocs(docs) })
	t.Logf("200 documents of %d bytes: %d MB allocated, %v: %v", len(doc), bytes>>20, took, err)
	if !errors.As(err, &made) || made.Size > 200*len(doc) {
		t.Errorf("200 documents: %v", err)
	}
	if _, _, _, err := ReadPicked(dir, Picking{}); !errors.As(err, &made) {
		t.Errorf("a directory of 200 files, below its top: %v", err)
	}
}

// TestMadeRefusalSeen: a rule file refused for what it makes is said
// wherever it is read: a ConfigMap holding it, through FromDocs (as serve
// reads a version of the rules in the ledger), is an error naming the key,
// not "no alert rules", nor its rules left out unsaid beside another
// file's (before, passed over, a group of 9,999 labels on 3,000 recording
// rules in a ConfigMap of 193 KB made 1.9 GB); through ReadPicked and
// Pick, an error. A document refused gives
// back what it counted: a ruler's next namespace, read with the same
// input, reads as it would have.
func TestMadeRefusalSeen(t *testing.T) {
	bomb := groupBomb(9999, 3000, false, false)
	ok := "groups:\n- name: ok\n  labels: {team: a}\n  rules:\n  - {alert: OK, expr: up == 0}\n"
	var made *MadeError
	for _, docs := range [][]Doc{
		{{Source: "cm", Data: []byte(configMap(bomb))}},
		{{Source: "cm", Data: []byte(configMap(ok, bomb))}},
	} {
		var rs []Rule
		var err error
		bytes, took := allocated(func() { rs, err = FromDocs(docs) })
		files := strings.Count(string(docs[0].Data), ".yaml")
		t.Logf("a ConfigMap of %d files, %d KB: %d MB allocated, %v: %v", files, len(docs[0].Data)>>10, bytes>>20, took, err)
		if !errors.As(err, &made) || errors.Is(err, errNoRules) || !strings.Contains(err.Error(), `cm: "f0`) {
			t.Errorf("a ConfigMap of %d files, one refused: %d rules, %v", files, len(rs), err)
		}
		if bytes > 64<<20 {
			t.Errorf("a ConfigMap of %d files: %d MB allocated to refuse it", files, bytes>>20)
		}
	}
	p := filepath.Join(t.TempDir(), "cm.yaml")
	if err := os.WriteFile(p, []byte(configMap(ok, bomb)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, picked, err := ReadPicked(p, Picking{}); !errors.As(err, &made) {
		t.Errorf("ReadPicked: %v (%s)", err, picked)
	}
	if _, _, err := Pick([]byte(configMap(ok, bomb)), Selector{}); !errors.As(err, &made) {
		t.Errorf("Pick: %v", err)
	}

	// A ruler's namespaces: one refused, the next read.
	ctx := NoAliases(context.Background())
	if _, err := FromDocsContext(ctx, []Doc{{Source: "bomb", Data: []byte(bomb)}}); !errors.As(err, &made) {
		t.Fatalf("the namespace refused: %v", err)
	}
	if rs, err := FromDocsContext(ctx, []Doc{{Source: "ok", Data: []byte(ok)}}); err != nil || len(rs) != 1 {
		t.Errorf("the next namespace: %d, %v", len(rs), err)
	}
	if in := inputOf(ctx); in.size != len(ok) || in.made != labelsCost(map[string]string{"team": "a"}) {
		t.Errorf("the input counted %d bytes and %d made, as if the refused namespace were read", in.size, in.made)
	}
}

// TestPickCountsOnce: a stream picked is counted at its size once, not
// again for each of its pieces, nor for each of its ConfigMaps' rule files
// read once and read again to tell whether they read; what reading them
// again makes isn't counted twice either.
func TestPickCountsOnce(t *testing.T) {
	file := "groups:\n- name: g\n  labels: {team: a}\n  rules:\n  - {alert: A, expr: up == 0}\n"
	b := []byte(configMap(file, file) + "---\n" + configMap(file))
	ctx := OneInput(context.Background())
	if _, _, err := pick(ctx, b, Selector{}); err != nil {
		t.Fatal(err)
	}
	in := inputOf(ctx)
	if in.size != len(b) {
		t.Errorf("picked %d bytes, counted %d", len(b), in.size)
	}
	if want := 3 * labelsCost(map[string]string{"team": "a"}); in.made != want {
		t.Errorf("made %d, want %d: three rules, each its group's labels", in.made, want)
	}
}
