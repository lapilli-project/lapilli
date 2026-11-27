package serve

import (
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// A disabled "Make it a task" says why once, and the others point at it:
// worked out over the items shown, after the grouping and the cut, so that
// no control points at a reason said by an action nothing draws.
func TestSayWhysOverTheItemsShown(t *testing.T) {
	const why = "Only %s's editors"
	off := func(rule, of string) action {
		return action{Kind: "tune", Rule: rule, Why: why, WhyOf: of}
	}
	on := func(rule string) action { return action{Kind: "retire", Rule: rule, May: true} }
	// Rule A's first line is disabled for payments' editors, its second
	// the reader may make: the item has a form and no disabled control, so
	// its first line says nothing. Rule B's is disabled for the same reason,
	// and C's for another.
	as := []action{off("A", "payments"), on("A"), off("B", "payments"), {Kind: "tune", Rule: "C", Why: "Editors only"}, off("D", "payments")}
	items := groupActions(as)
	sayWhys(items)
	var said []string
	for _, it := range items {
		if a := it.Off(); a != nil {
			if a.WhyID == "" {
				t.Errorf("%s: a disabled control with no reason to point at", a.Rule)
			}
			if a.Said {
				said = append(said, a.WhyID)
			}
		} else {
			for _, l := range it.Lines {
				if l.Said || l.WhyID != "" {
					t.Errorf("%s: a reason said by a line with no control: %+v", l.Rule, l)
				}
			}
		}
	}
	if strings.Join(said, ",") != "look-why-1,look-why-2" {
		t.Errorf("said %v, want one each of the two reasons, in order", said)
	}
	if b := items[1].Off(); b == nil || !b.Said || b.WhyID != "look-why-1" {
		t.Errorf("B, the first control disabled for payments': %+v", b)
	}
	if d := items[3].Off(); d == nil || d.Said || d.WhyID != "look-why-1" {
		t.Errorf("D points at B's: %+v", d)
	}

	// Cut to the first items, the reason is the first of those shown.
	cut := groupActions(as)[1:]
	sayWhys(cut)
	if b := cut[0].Off(); b == nil || !b.Said || b.WhyID != "look-why-1" {
		t.Errorf("after the cut: %+v", b)
	}
}

// A team's editor on the overview: each disabled control points at a
// reason the page says, whichever actions were grouped or cut.
func TestActionDisabledPointsAtItsReasonShown(t *testing.T) {
	parallel(t)
	s, _, search := bulkServer(t, nil)
	if _, err := s.l.PutGrant(t.Context(), ledger.Grant{Who: "user:sam@example.com", TeamID: search}, "boss"); err != nil {
		t.Fatal(err)
	}
	_, body := getAs(t, s.Handler(), "/?team=", "sam@example.com")
	i := strings.Index(body, "What to look at")
	j := strings.Index(body[i:], "</section>")
	look := body[i : i+j]
	for _, id := range idsDescribing(look) {
		if strings.Count(look, `id="`+id+`" class="why"`) != 1 {
			t.Errorf("a control points at %q, which the list doesn't say once", id)
		}
	}
	if len(idsDescribing(look)) == 0 {
		t.Error("no disabled control to check")
	}
}

func idsDescribing(s string) []string {
	var out []string
	for _, part := range strings.Split(s, `type="button" disabled aria-describedby="`)[1:] {
		out = append(out, part[:strings.Index(part, `"`)])
	}
	return out
}
