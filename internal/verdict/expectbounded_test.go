package verdict

import (
	"strings"
	"testing"
)

// An expectations file is read as the rules are, its values bounded before
// it is decoded: one denser than the rules' reader takes, or whose aliases
// expand past it, is refused by that reader, not decoded first.
func TestExpectationsBounded(t *testing.T) {
	dense := "expectations: [" + strings.Repeat("a,", 300000) + "a]\n"
	if _, err := ReadExpectations(strings.NewReader(dense)); err == nil || !strings.Contains(err.Error(), "values") {
		t.Errorf("a dense file: %v", err)
	}
	bomb := "a: &a [x, x, x, x, x, x, x, x, x, x]\nb: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a, *a]\nc: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b, *b]\n" +
		"d: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c, *c]\ne: &e [*d, *d, *d, *d, *d, *d, *d, *d, *d, *d]\nexpectations: []\n"
	if _, err := ReadExpectations(strings.NewReader(bomb)); err == nil || strings.Contains(err.Error(), "field a not found") {
		t.Errorf("aliases expanding: %v", err)
	}
	if e, err := ReadExpectations(strings.NewReader("")); err != nil || len(e.Expectations) != 0 {
		t.Errorf("an empty file: %+v %v", e, err)
	}
}
