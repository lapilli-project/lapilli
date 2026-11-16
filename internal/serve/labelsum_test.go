package serve

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
)

// manyInstances is how many series of one job the history's "up == 0"
// has down at once: one impact window merging them.
const manyInstances = 40

// manyLabelsTry is the incident criteria tried with "up == 0": a window
// with manyInstances values of instance, and job's one.
const manyLabelsTry = "/settings?impact-try=1&impact=up+%3D%3D+0&impact-for=5m"

// The try whose one window merges many series, among the pages every page
// test renders in each language and checks for accessibility.
func init() {
	pagePaths = append(pagePaths, manyLabelsTry)
}

// instances are n values of instance, api-7f9c-00 on.
func instances(n int) []string {
	var vs []string
	for i := range n {
		vs = append(vs, fmt.Sprintf("api-7f9c-%02d", i))
	}
	return vs
}

// manyLabelled gives s an incident written down with manyInstances values
// of instance, L-many, and gives back its page, for the pages' checks and
// test/a11y/axe.sh (TestA11yServe).
func manyLabelled(t *testing.T, s *Server) string {
	t.Helper()
	at := now.Add(-10 * time.Hour)
	if _, err := s.l.Put(context.Background(), incident.Incident{ID: "L-many", Title: "API nodes down", Start: at, End: at.Add(40 * time.Minute),
		Labels: incident.Labels{"instance": instances(manyInstances), "job": {"api"}}}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	return "/incidents/L-many"
}

// labelsHTML is labels summed up as the "labelsum" template shows them, in
// a language.
func labelsHTML(t *testing.T, lang string, l incident.Labels) string {
	t.Helper()
	var b bytes.Buffer
	if err := pagesIn[lang].ExecuteTemplate(&b, "labelsum", labelsum(l)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Labels sum up by name, in order: a name with one value as name=value, one
// with several as how many and the first labelsShown, sorted and each
// once; every name=value, the first labelsListed, behind a name cut.
func TestLabelSum(t *testing.T) {
	for _, x := range []struct {
		name   string
		labels incident.Labels
		want   labelSum
	}{
		{"none", nil, labelSum{}},
		{"no values", incident.Labels{"service": {}}, labelSum{}},
		{"one value", incident.Labels{"service": {"checkout"}},
			labelSum{Names: []summedLabel{{"service", 1, []string{"checkout"}}}, All: []string{"service=checkout"}, Total: 1}},
		{"a few values, sorted and each once", incident.Labels{"service": {"search", "cart", "checkout", "cart"}},
			labelSum{Names: []summedLabel{{"service", 3, []string{"cart", "checkout", "search"}}},
				All: []string{"service=cart", "service=checkout", "service=search"}, Total: 3}},
		{"many values for one name", incident.Labels{"instance": instances(5)},
			labelSum{Names: []summedLabel{{"instance", 5, instances(3)}}, Cut: true,
				All: []string{"instance=api-7f9c-00", "instance=api-7f9c-01", "instance=api-7f9c-02", "instance=api-7f9c-03", "instance=api-7f9c-04"}, Total: 5}},
		{"mixed names, by name", incident.Labels{"zone": {"b", "a"}, "job": {"api"}, "instance": instances(4)},
			labelSum{Names: []summedLabel{{"instance", 4, instances(3)}, {"job", 1, []string{"api"}}, {"zone", 2, []string{"a", "b"}}}, Cut: true,
				All: []string{"instance=api-7f9c-00", "instance=api-7f9c-01", "instance=api-7f9c-02", "instance=api-7f9c-03", "job=api", "zone=a", "zone=b"}, Total: 7}},
	} {
		if got := labelsum(x.labels); !equalSum(got, x.want) {
			t.Errorf("%s: %+v, want %+v", x.name, got, x.want)
		}
	}
	// What it is given stays as it was.
	l := incident.Labels{"service": {"search", "cart"}}
	labelsum(l)
	if !slices.Equal(l["service"], []string{"search", "cart"}) {
		t.Errorf("sorted what it was given: %v", l)
	}
	// Past labelsListed, the rest are counted.
	big := labelsum(incident.Labels{"instance": instances(150), "pod": instances(150)})
	if len(big.All) != labelsListed || big.Total != 300 || big.More != 300-labelsListed || !strings.HasPrefix(big.All[149], "instance=") || !strings.HasPrefix(big.All[150], "pod=") || !big.Cut {
		t.Errorf("past %d: %d listed of %d, %d more, the last %q", labelsListed, len(big.All), big.Total, big.More, big.All[len(big.All)-1])
	}
}

func equalSum(a, b labelSum) bool {
	return slices.EqualFunc(a.Names, b.Names, func(x, y summedLabel) bool {
		return x.Name == y.Name && x.N == y.N && slices.Equal(x.Shown, y.Shown)
	}) && a.Cut == b.Cut && slices.Equal(a.All, b.All) && a.Total == b.Total && a.More == b.More
}

// The template shows a name with one value as name=value, one with
// several as how many and the first few, and all of them behind a details
// once any is cut; none as –; the values escaped, each set apart.
func TestLabelSumShown(t *testing.T) {
	if got := labelsHTML(t, "en", nil); got != "–" {
		t.Errorf("none: %q", got)
	}
	one := labelsHTML(t, "en", incident.Labels{"service": {"checkout"}})
	if !strings.Contains(one, "<code><bdi>service=checkout</bdi></code>") || strings.Contains(one, "values") || strings.Contains(one, "<details") {
		t.Errorf("one value: %s", one)
	}
	few := labelsHTML(t, "en", incident.Labels{"service": {"cart", "search"}})
	if !strings.Contains(few, `<code><bdi>service</bdi></code> <span class="labelcount">2 values:</span> <span class="labelvalue"><code><bdi>cart</bdi></code>,</span> `+
		`<span class="labelvalue"><code><bdi>search</bdi></code></span></span>`) ||
		strings.Contains(few, "…") || strings.Contains(few, "<details") {
		t.Errorf("two values, both shown: %s", few)
	}
	many := labelsHTML(t, "en", incident.Labels{"instance": instances(manyInstances), "job": {"api"}})
	inline, all, ok := strings.Cut(many, `<details class="labels-all">`)
	if !ok || !strings.Contains(inline, fmt.Sprintf("<code><bdi>instance</bdi></code> <span class=\"labelcount\">%d values:</span>", manyInstances)) ||
		strings.Count(inline, "api-7f9c-") != labelsShown || !strings.Contains(inline, "<code><bdi>api-7f9c-02</bdi></code>, …</span>") || strings.Contains(inline, "instance=") ||
		!strings.Contains(inline, "<code><bdi>job=api</bdi></code>") {
		t.Errorf("many values, inline: %s", inline)
	}
	if !strings.HasPrefix(all, fmt.Sprintf("<summary>all %d labels</summary>", manyInstances+1)) || strings.Count(all, "<li>") != manyInstances+1 ||
		!strings.Contains(all, "<li><bdi>instance=api-7f9c-39</bdi></li><li><bdi>job=api</bdi></li>") {
		t.Errorf("many values, all of them: %s", all)
	}
	ko := labelsHTML(t, "ko", incident.Labels{"instance": instances(manyInstances)})
	if !strings.Contains(ko, fmt.Sprintf("값 %d개:", manyInstances)) || !strings.Contains(ko, fmt.Sprintf("라벨 %d개 모두 보기", manyInstances)) {
		t.Errorf("in Korean: %s", ko)
	}
	// Past labelsListed, how many more.
	capped := labelsHTML(t, "en", incident.Labels{"instance": instances(150), "pod": instances(150)})
	if strings.Count(capped, "<li>") != labelsListed+1 || !strings.Contains(capped, fmt.Sprintf("<li>and %d more</li>", 300-labelsListed)) ||
		!strings.Contains(capped, "<summary>all 300 labels</summary>") {
		t.Errorf("past %d: %d listed\n%s", labelsListed, strings.Count(capped, "<li>"), capped[strings.LastIndex(capped, "<li>"):])
	}
	// A series' label is text: escaped, in a name or a value, inline and
	// listed.
	hostile := labelsHTML(t, "en", incident.Labels{"<script>": {"x"}, "service": {"<script>alert(1)</script>", "b", "c", "d"}})
	if strings.Contains(hostile, "<script>") || !strings.Contains(hostile, "<code><bdi>&lt;script&gt;=x</bdi></code>") ||
		!strings.Contains(hostile, "<code><bdi>&lt;script&gt;alert(1)&lt;/script&gt;</bdi></code>") ||
		!strings.Contains(hostile, "<li><bdi>service=&lt;script&gt;alert(1)&lt;/script&gt;</bdi></li>") {
		t.Errorf("escaped: %s", hostile)
	}
}

// A try whose window merges many series, and an incident with as many
// labels, show how many values a name has and the first few, and all of
// them behind a details: not one name=value for each series in a row. On
// the incident's page the details follows the sentence it can't be in.
func TestManyLabelsSummedUp(t *testing.T) {
	s, _ := server(t, nil)
	s.jobs(context.Background(), true) // INC-1, from the incidents file
	page := manyLabelled(t, s)
	h := s.Handler()
	code, body := askAs(t, h, http.MethodGet, manyLabelsTry, nil, "", "same-origin")
	if code != 200 || !strings.Contains(body, "1 windows found;") {
		t.Fatalf("%d\n%s", code, body)
	}
	cell := regexp.MustCompile(`(?s)<td class="labels">(.*?)</td>`).FindStringSubmatch(body)
	if cell == nil {
		t.Fatalf("no labels cell:\n%s", body)
	}
	inline, all, ok := strings.Cut(cell[1], `<details class="labels-all">`)
	if !ok || !strings.Contains(inline, fmt.Sprintf("%d values:", manyInstances)) || strings.Contains(inline, "instance=") ||
		strings.Count(inline, "api-7f9c-") != labelsShown || strings.Count(all, "<li><bdi>instance=") != manyInstances {
		t.Errorf("the window's labels:\n%s", cell[1])
	}

	_, body, _ = get(t, h, page)
	lede := regexp.MustCompile(`(?s)<p class="lede">(.*?)</p>\n(<details class="labels-all">.*?</details>)?`).FindStringSubmatch(body)
	if lede == nil || !strings.Contains(lede[1], fmt.Sprintf("%d values:", manyInstances)) || strings.Contains(lede[1], "instance=") ||
		strings.Count(lede[1], "api-7f9c-") != labelsShown || strings.Contains(lede[1], "<details") ||
		strings.Count(lede[2], "<li><bdi>instance=") != manyInstances {
		t.Errorf("the incident's labels:\n%s", body)
	}
	// The form still changes the labels as a line.
	if !strings.Contains(body, `<input name="labels" value="instance=api-7f9c-00 instance=api-7f9c-01 `) {
		t.Errorf("the form's labels:\n%s", body)
	}
	// One with a label of one value reads as before.
	_, body, _ = get(t, h, "/incidents/INC-1")
	if !strings.Contains(body, " · <span class=\"labelsum\"><span class=\"labelname\"><code><bdi>service=checkout</bdi></code></span> </span><br>") ||
		strings.Contains(body, `<details class="labels-all">`) {
		t.Errorf("INC-1's labels:\n%s", body)
	}
	pageA11y(t, h, "", manyLabelsTry, page)
}

// The labels summed up keep their cell narrow, a name=value or a value
// whole on its line unless it is wider than the cell, a value with the
// comma after it, and what the details lists as narrow.
func TestLabelSumNarrow(t *testing.T) {
	b, err := os.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{`td\.labels \.labelsum \{[^}]*max-width: 24rem`, `details\.labels-all \{[^}]*max-width: 24rem`,
		`details\.labels-all li \{[^}]*overflow-wrap: anywhere`, `\.labelsum code \{[^}]*display: inline-block[^}]*overflow-wrap: anywhere`, `\.labelvalue \{[^}]*white-space: nowrap`} {
		if !regexp.MustCompile(rule).Match(b) {
			t.Errorf("no %s", rule)
		}
	}
}
