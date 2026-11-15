package rules

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestConfigMapAliased: a ConfigMap's value a YAML alias brings in is
// refused, naming its key, whatever its tag and wherever the alias is: it
// was read into rules again for each key, 10,000 rules by 16 keys, 250 KB,
// making 1.2 GB parsed and more than 2.2 GB through ReadPicked; and, a
// !!binary value decoded afresh for each alias, 320 KB made 1.22 GB past
// a check by where a value's text is. So too an alias of the whole data, of
// a whole ConfigMap in a List, and a merge (<<) into the data. An alias of
// a value that isn't rules by its text (no groups, backslash or binary in
// it), as an env-style ConfigMap writes, is passed over unread. The same
// text written twice, no alias, reads twice.
func TestConfigMapAliased(t *testing.T) {
	var file strings.Builder
	file.WriteString("groups:\n- name: g\n  rules:\n")
	for range 10000 {
		file.WriteString("  - {alert: a, expr: x}\n")
	}
	keys := func(first string) string {
		var cm strings.Builder
		fmt.Fprintf(&cm, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  k00: %s\n", first)
		for i := 1; i < 16; i++ {
			fmt.Fprintf(&cm, "  k%02d: *s\n", i)
		}
		return cm.String()
	}
	small := "groups: [{name: g, rules: [{alert: A, expr: up}]}]"
	cm := func(data string) string {
		return "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: " + data + "\n"
	}
	for _, c := range []struct{ what, doc string }{
		{"a value by 16 keys", keys("&s " + strconv.Quote(file.String()))},
		{"a !!binary value by 16 keys", keys("&s !!binary " + base64.StdEncoding.EncodeToString([]byte(file.String())))},
		{"a whole ConfigMap in a List", "kind: List\nitems:\n- &c {kind: ConfigMap, metadata: {name: a}, data: {a: !!binary " +
			base64.StdEncoding.EncodeToString([]byte(small)) + "}}\n- *c\n- *c\n"},
		// Rules by their text though neither groups nor binary is written:
		// its key a !!binary tag written with a %-escape.
		{"a value whose groups key is !!bin%61ry", keys("&s " + strconv.Quote("!!bin%61ry "+base64.StdEncoding.EncodeToString([]byte("groups"))+
			": [{name: g, rules: [{alert: A, expr: up}]}]"))},
	} {
		var err error
		bytes, took := allocated(func() { _, err = Parse([]byte(c.doc)) })
		t.Logf("%s: %d MB allocated, %v: %.140v", c.what, bytes>>20, took, err)
		if !errors.Is(err, errAliased) {
			t.Errorf("%s: %v", c.what, err)
		}
		if bytes > 128<<20 {
			t.Errorf("%s: %d MB allocated", c.what, bytes>>20)
		}
	}
	whole := "kind: List\nitems:\n- kind: ConfigMap\n  metadata: {name: a}\n  data: &d {a: " + strconv.Quote(small) + "}\n- kind: ConfigMap\n  metadata: {name: b}\n  data: *d\n"
	merged := "kind: List\nitems:\n- {kind: Secret, metadata: {name: s}, stringData: &b {a: " + strconv.Quote(small) + "}}\n- kind: ConfigMap\n  metadata: {name: c}\n  data: {<<: *b}\n"
	for what, doc := range map[string]string{"the whole data": whole, "a merge into the data": merged} {
		if _, err := Parse([]byte(doc)); !errors.Is(err, errAliased) {
			t.Errorf("%s: %v", what, err)
		}
	}
	p := filepath.Join(t.TempDir(), "cm.yaml")
	if err := os.WriteFile(p, []byte(keys("&s !!binary "+base64.StdEncoding.EncodeToString([]byte(file.String())))), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadPicked(p, Picking{Trim: true}); !errors.Is(err, errAliased) {
		t.Errorf("ReadPicked: %v", err)
	}
	// An alias of what isn't rules, by its text, an env-style value, is
	// passed over unread, as before; so is it in !!binary.
	env := cm("{DB_HOST: &db \"db.internal:5432\", REPLICA_HOST: *db, r.yaml: " + strconv.Quote(small) + "}")
	envBinary := cm("{DB_HOST: &db !!binary " + base64.StdEncoding.EncodeToString([]byte("db.internal")) + ", REPLICA_HOST: *db, r.yaml: " + strconv.Quote(small) + "}")
	for what, doc := range map[string]string{"an env-style value": env, "an env-style value in !!binary": envBinary} {
		if rs, err := Parse([]byte(doc)); err != nil || len(rs) != 1 {
			t.Errorf("%s by an alias: %d, %v", what, len(rs), err)
		}
	}
	twice := cm("{a.yaml: " + strconv.Quote(small) + ", b.yaml: " + strconv.Quote(small) + "}")
	if rs, err := Parse([]byte(twice)); err != nil || len(rs) != 2 {
		t.Errorf("the same text written twice: %d, %v", len(rs), err)
	}
}

// TestAmazonNamespaceAliased: an Amazon rule groups namespace's data a
// YAML alias brings in is refused before it is decoded, naming the
// namespace, as a ConfigMap's value is: each namespace aliasing it was
// decoded and read into rules again. The AWS CLI writes none. The same
// data written twice, no alias, reads twice.
func TestAmazonNamespaceAliased(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("groups: [{name: g, rules: [{alert: A, expr: up}]}]"))
	aliased := "- ruleGroupsNamespace: {name: a, data: &d " + data + "}\n- ruleGroupsNamespace: {name: b, data: *d}\n"
	if rs, err := Parse([]byte(aliased)); !errors.Is(err, errAliased) || !strings.Contains(err.Error(), `namespace "a"`) {
		t.Errorf("aliased: %d, %v", len(rs), err)
	}
	twice := "- ruleGroupsNamespace: {name: a, data: " + data + "}\n- ruleGroupsNamespace: {name: b, data: " + data + "}\n"
	if rs, err := Parse([]byte(twice)); err != nil || len(rs) != 2 {
		t.Errorf("written twice: %d, %v", len(rs), err)
	}
}

// TestBelowTopRefusedSaid: a file below a directory's top in the shape of
// rules that Lapilli's own reader refuses (a rule of 10,001 labels, past
// MaxKeys), which Prometheus may load, is an error naming it, as it is at
// the top, whether a rule file or a ConfigMap holding one: it was passed
// over unsaid, as one that doesn't read is, its rules left out. One that
// doesn't read, which Prometheus refuses too, is passed over still
// (TestReadInItsInputs).
func TestBelowTopRefusedSaid(t *testing.T) {
	var l strings.Builder
	for i := range 10001 {
		fmt.Fprintf(&l, "l%d: v, ", i)
	}
	big := "groups: [{name: g, rules: [{alert: Hidden, expr: x, labels: {" + strings.TrimSuffix(l.String(), ", ") + "}}]}]\n"
	for what, body := range map[string]string{"a rule file": big, "a ConfigMap": configMap(big)} {
		dir := t.TempDir()
		for name, b := range map[string]string{"top.yaml": "groups: [{name: ok, rules: [{alert: Seen, expr: x}]}]\n", "sub/x.yaml": body} {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if rs, _, _, err := ReadPicked(dir, Picking{}); err == nil || !strings.Contains(err.Error(), "x.yaml") || !strings.Contains(err.Error(), "10000 keys") {
			t.Errorf("%s below the top: %d rules, %v", what, len(rs), err)
		}
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

// TestNotRulesPassedOver: a ConfigMap's value that isn't in the shape of
// rules is passed over, whatever Lapilli's reader says of it, as it was
// before rule files it refuses were said: a file_sd list of targets whose
// labels have a key twice (Go's JSON takes the last), a list holding a
// mapping of 10,001 keys, a list 250 deep, a TOML file, plain text. A list is rules only if an item is a mapping with
// groups or a kind. Each such ConfigMap failed whole, and a version kept in
// the ledger holding one couldn't be read again. A rule file with a key
// twice, which yaml.v3 refuses too, is broken, not an error.
func TestNotRulesPassedOver(t *testing.T) {
	var many strings.Builder
	many.WriteString("[{")
	for i := range 10001 {
		if i > 0 {
			many.WriteString(", ")
		}
		fmt.Fprintf(&many, "k%d: v", i)
	}
	many.WriteString("}]")
	ok := "groups: [{name: ok, rules: [{alert: Seen, expr: x}]}]"
	for what, v := range map[string]string{
		"a file_sd list, a key twice": `[{"targets": ["a:9100"], "labels": {"env": "p", "env": "q"}}]`,
		"a list, 10,001 keys":         many.String(),
		"a list 250 deep":             strings.Repeat("[", 250) + strings.Repeat("]", 250),
		"TOML":                        "[server]\nhttp_port = 3000\n[[groups]]\nname = \"a\"\n",
		"plain text":                  "hello: world: nope\n  - x",
	} {
		cm := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  ok.yml: %s\n  other: %s\n", strconv.Quote(ok), strconv.Quote(v))
		if rs, err := Parse([]byte(cm)); err != nil || len(rs) != 1 {
			t.Errorf("%s: %d, %v", what, len(rs), err)
		}
		if rs, err := FromDocs([]Doc{{Source: "v0.43.0", Data: []byte(cm)}}); err != nil || len(rs) != 1 {
			t.Errorf("%s, read again from the ledger: %d, %v", what, len(rs), err)
		}
		if _, picked, err := Pick([]byte(cm), Selector{}); err != nil || picked.Broken != 0 {
			t.Errorf("%s, picked: %+v, %v", what, picked, err)
		}
	}
	twice := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  ok.yml: %s\n  twice.yml: %s\n", strconv.Quote(ok),
		strconv.Quote("groups: [{name: g, rules: [{alert: T, expr: x}]}]\nname: x\nname: y\n"))
	if rs, err := Parse([]byte(twice)); err != nil || len(rs) != 1 {
		t.Errorf("a rule file with a key twice: %d, %v", len(rs), err)
	}
	if _, picked, err := Pick([]byte(twice), Selector{}); err != nil || picked.Broken != 1 {
		t.Errorf("a rule file with a key twice, picked: %+v, %v", picked, err)
	}
}
