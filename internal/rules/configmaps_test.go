package rules

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestConfigMapReadOnce: a ConfigMap's value is read as rules once in an
// input: one another key holds already, which only a YAML alias makes, is
// refused, saying so, where each key was read into rules again (before,
// 10,000 rules aliased by 16 keys, 250 KB, made 1.2 GB parsed and more
// than 2.2 GB through ReadPicked). An aliased value that isn't rules is
// passed over once read.
func TestConfigMapReadOnce(t *testing.T) {
	var file strings.Builder
	file.WriteString("groups:\n- name: g\n  rules:\n")
	for range 10000 {
		file.WriteString("  - {alert: a, expr: x}\n")
	}
	var cm strings.Builder
	fmt.Fprintf(&cm, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  k0: &s %s\n", strconv.Quote(file.String()))
	for i := 1; i < 16; i++ {
		fmt.Fprintf(&cm, "  k%d: *s\n", i)
	}
	var err error
	bytes, took := allocated(func() { _, err = Parse([]byte(cm.String())) })
	t.Logf("10,000 rules by 16 keys: %d MB allocated, %v: %v", bytes>>20, took, err)
	if err == nil || !errors.Is(err, errReadAgain) || !strings.Contains(err.Error(), `"k1"`) {
		t.Errorf("%v", err)
	}
	if bytes > 128<<20 {
		t.Errorf("%d MB allocated", bytes>>20)
	}
	p := filepath.Join(t.TempDir(), "cm.yaml")
	if err := os.WriteFile(p, []byte(cm.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadPicked(p, Picking{Trim: true}); !errors.Is(err, errReadAgain) {
		t.Errorf("ReadPicked: %v", err)
	}
	flags := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  a: &on \"true\"\n  b: *on\n  rules.yaml: \"groups: [{name: g, rules: [{alert: A, expr: up}]}]\"\n"
	if rs, err := Parse([]byte(flags)); err != nil || len(rs) != 1 {
		t.Errorf("an aliased value that isn't rules: %d, %v", len(rs), err)
	}
}

// TestReaderRefusalsSaid: a ConfigMap's rule file that yaml.v3 decodes, as
// Prometheus loads it, but Lapilli's own reader refuses (a rule of 10,001
// labels, past MaxKeys), was taken for one that isn't YAML and passed over
// unsaid: its rules left out of a backtest. It is an error naming the key
// now, through Parse and ReadPicked, and counted broken where a stream is
// picked; a value that isn't rules, refused alike, is passed over still.
func TestReaderRefusalsSaid(t *testing.T) {
	var l strings.Builder
	for i := range 10001 {
		if i > 0 {
			l.WriteString(", ")
		}
		fmt.Fprintf(&l, "l%d: v", i)
	}
	refused := "groups: [{name: g, rules: [{alert: Hidden, expr: x, labels: {" + l.String() + "}}]}]"
	cm := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  ok.yml: \"groups: [{name: ok, rules: [{alert: Seen, expr: x}]}]\"\n  big.yml: %s\n", strconv.Quote(refused))
	if rs, err := Parse([]byte(cm)); err == nil || !strings.Contains(err.Error(), `"big.yml"`) || !strings.Contains(err.Error(), "10000 keys") {
		t.Errorf("Parse: %d rules, %v", len(rs), err)
	}
	p := filepath.Join(t.TempDir(), "cm.yaml")
	if err := os.WriteFile(p, []byte(cm), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, picked, err := ReadPicked(p, Picking{Trim: true}); err == nil || picked.Broken != 1 {
		t.Errorf("ReadPicked: %+v, %v", picked, err)
	}
	if kept, picked, err := Pick([]byte(cm), Selector{}); err != nil || picked.Broken != 1 || !strings.Contains(string(kept), "big.yml") {
		t.Errorf("Pick: %+v, %v", picked, err)
	}
	other := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  ok.yml: \"groups: [{name: ok, rules: [{alert: Seen, expr: x}]}]\"\n  big.json: %s\n",
		strconv.Quote("{"+l.String()+"}"))
	if rs, err := Parse([]byte(other)); err != nil || len(rs) != 1 {
		t.Errorf("a value refused alike that isn't rules: %d, %v", len(rs), err)
	}
}

// TestBrokenNotOnTopOfRead: a ConfigMap's rule files are read once where a
// stream is picked, what doesn't read counted from that one reading: they
// were read again to tell, on top of what the first reading made, and a
// group's labels on 25 rules (some 3 MB made, within the 4 MiB it may)
// were called broken where Parse reads them.
func TestBrokenNotOnTopOfRead(t *testing.T) {
	cm := configMap(groupBomb(1000, 25, false, false))
	if rs, err := Parse([]byte(cm)); err != nil || len(rs) != 25 {
		t.Fatalf("Parse: %d, %v", len(rs), err)
	}
	if _, picked, err := Pick([]byte(cm), Selector{}); err != nil || picked.Broken != 0 {
		t.Errorf("Pick: %+v, %v", picked, err)
	}
}
