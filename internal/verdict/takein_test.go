package verdict

import (
	"bytes"
	"strings"
	"testing"
)

// A draft taken into an expectations file holds its rule to what it says
// in place of what the file held it to, field by field, catch by catch;
// adds a rule the file didn't hold; and leaves the rest of the file, its
// comments among it, as it was. The file is written by the encoder and
// read back as an expectations file.
func TestTakeIn(t *testing.T) {
	file := `# Our alerts' expectations: see the runbook.
expectations:
  - rule: A # paged twice a night, last quarter
    budget: {week: 10, asleep: 2}
    catches:
      - incident: INC-1
        within: 10m
  - group: shop
    budget:
      week: 50
  - rule: A
    catches:
      - incident: INC-9
`
	draft, err := DraftEntries("expectations:\n  - rule: A\n    budget: {week: 5}\n    catches:\n      - {incident: INC-1, within: 3m}\n      - {incident: INC-2}\n  - rule: A\n    catches: [{incident: INC-9}]\n", "A")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := DraftEntries("expectations:\n  - rule: \"B: new\\nline\"\n    budget: {refires: 1}\n", "B: new\nline")
	if err != nil {
		t.Fatal(err)
	}
	out, err := TakeIn([]byte(file), append(draft, b2...))
	if err != nil {
		t.Fatal(err)
	}
	e, err := ReadExpectations(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	held := map[string][]string{}
	for _, x := range e.Expectations {
		for _, it := range x.Items() {
			held[x.Selector()] = append(held[x.Selector()], it.What+" "+it.Held)
		}
	}
	for sel, want := range map[string]string{
		"rule: A":            "budget week 5,budget asleep 2,catches INC-1 within 3m,catches INC-2 caught,catches INC-9 caught",
		"group: shop":        "budget week 50",
		"rule: B: new\nline": "budget refires 1",
	} {
		if got := strings.Join(held[sel], ","); got != want {
			t.Errorf("%q: %s, want %s\n%s", sel, got, want, out)
		}
	}
	for _, kept := range []string{"# Our alerts' expectations: see the runbook.", "# paged twice a night, last quarter"} {
		if !strings.Contains(string(out), kept) {
			t.Errorf("%q lost:\n%s", kept, out)
		}
	}
	// Taken in again, nothing changes.
	again, err := TakeIn(out, append(draft, b2...))
	if err != nil || !bytes.Equal(again, out) {
		t.Errorf("taken in twice: %v\n%s\n%s", err, out, again)
	}
	// An empty file is the draft; a file that isn't one, one with anchors,
	// and a draft by group are refused.
	if out, err := TakeIn(nil, b2); err != nil || !strings.Contains(string(out), "refires: 1") {
		t.Errorf("an empty file: %v\n%s", err, out)
	}
	for want, c := range map[string]struct {
		file  string
		draft []Expectation
	}{
		"the expectations file can't be read": {"expectations:\n  - rule: A\n    budjet: {week: 1}\n", draft},
		// One budget two rules share: a change to A's would be B's too.
		"anchors or aliases":                  {"expectations:\n  - rule: A\n    budget: &b {week: 1}\n  - rule: B\n    budget: *b\n", draft},
		"a draft holds a rule by rule: alone": {file, []Expectation{{Group: "shop", Budget: &Budget{}}}},
		"more than one document":              {"expectations: []\n---\nexpectations: []\n", draft},
	} {
		if out, err := TakeIn([]byte(c.file), c.draft); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v\n%s", want, err, out)
		}
	}
	// A draft holds its rule by rule: alone.
	if _, err := DraftEntries("expectations:\n  - group: shop\n    budget: {week: 1}\n", "A"); err == nil {
		t.Error("a group's draft read as a rule's")
	}
}
