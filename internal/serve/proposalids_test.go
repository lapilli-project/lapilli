package serve

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"lapilli.dev/internal/ledger"
)

// TestProposalsPickedByID: the proposals list picks the proposals its URL
// names by id, as a bulk proposal's result links to them: those alone,
// counted alone in the facets, a chip each, kept by the form and the other
// chips, taken away by Clear; what isn't a proposal's id is left out, and
// twenty are taken at most.
func TestProposalsPickedByID(t *testing.T) {
	s, _ := changesServer(t, nil)
	ctx := context.Background()
	var ids []string
	for i := range 25 {
		id, err := s.l.Propose(ctx, ledger.Proposal{Rule: fmt.Sprint("Other", i), Kind: []string{ledger.KindFor, ledger.KindDelete}[i%2], Path: ledger.PathNone, MadeBy: fmt.Sprint("p", i, "@example.com")})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, fmt.Sprint(id))
	}
	h := s.Handler()
	listed := func(body string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`<td><a href="/proposals/(\d+)[^"]*">#`).FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		slices.Sort(out)
		return out
	}
	_, body := getAs(t, h, "/proposals?state=all&id="+ids[0]+"&id="+ids[1]+"&id=x&id=-1&id=0&id="+ids[0], bo)
	if got, want := listed(body), []string{ids[0], ids[1]}; !slices.Equal(got, want) {
		t.Errorf("by id: %v, want %v", got, want)
	}
	for _, w := range []string{"2 of 2 proposals", `<input type="hidden" name="id" value="` + ids[0] + `">`,
		`<span class="vh">Remove filter: </span>Proposals: <bdi>#` + ids[1] + `</bdi>`, `name="kind" value="delete"> <span>deleting it</span> <span class="n">1</span>`} {
		if !strings.Contains(body, w) {
			t.Errorf("no %s", w)
		}
	}
	if strings.Contains(body, "<bdi>#x") || strings.Contains(body, "#-1") || strings.Count(body, `class="chip"`) != 2 {
		t.Error("an id that isn't one is taken")
	}
	// A chip takes its id away, the others kept; Clear takes them all.
	chip := regexp.MustCompile(`<a class="chip" href="([^"]+)"><span class="vh">Remove filter: </span>Proposals: <bdi>#` + ids[0]).FindStringSubmatch(body)
	if chip == nil {
		t.Fatal("no chip for the first")
	}
	if _, b := getAs(t, h, html.UnescapeString(chip[1]), bo); !slices.Equal(listed(b), []string{ids[1]}) {
		t.Errorf("its chip: %v", listed(b))
	}
	clear := regexp.MustCompile(`<a href="([^"]+)">Clear filters</a>`).FindStringSubmatch(body)
	if clear == nil || strings.Contains(clear[1], "id=") {
		t.Errorf("Clear: %v", clear)
	}
	// With a facet picked too, and twenty-five named: the first twenty.
	q := url.Values{"state": {"all"}, "kind": {"for"}, "id": ids}
	_, body = getAs(t, h, "/proposals?"+q.Encode(), bo)
	var want []string
	for i, id := range ids[:20] {
		if i%2 == 0 {
			want = append(want, id)
		}
	}
	slices.Sort(want)
	if got := listed(body); !slices.Equal(got, want) {
		t.Errorf("twenty-five named, kind for: %v, want %v", got, want)
	}
}
