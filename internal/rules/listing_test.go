package rules

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// listAPI is a rules API's answer of groups: name, file, interval and last
// evaluation.
func listAPI(groups ...[4]any) []byte {
	var gs []string
	for _, g := range groups {
		gs = append(gs, fmt.Sprintf(`{"name":%q,"file":%q,"interval":%v,"lastEvaluation":%q,"rules":[]}`, g[0], g[1], g[2], g[3]))
	}
	return []byte(`{"status":"success","data":{"groups":[` + strings.Join(gs, ",") + `]}}`)
}

// The groups of a real Prometheus 3.5.0, kept as test data: each is placed
// where its last evaluation says, by a hash of its name and the file it
// loaded, which Place takes for a rule file's group, and for another
// interval too.
func TestListingOfARealPrometheus(t *testing.T) {
	b, err := os.ReadFile("../replay/testdata/prometheus-3.5.0/api.json")
	if err != nil {
		t.Fatal(err)
	}
	l, err := ReadListing(b)
	if err != nil {
		t.Fatal(err)
	}
	if l.layout != hashStrings {
		t.Fatalf("layout %d, want the string labels'", l.layout)
	}
	api, err := FromAPI(b)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range api {
		if seen[r.Group] {
			continue
		}
		seen[r.Group] = true
		d, why := l.Place(r.Group, r.Interval)
		if why != "" || d != r.Phase {
			t.Errorf("%s: placed %s (%q), its last evaluation says %s", r.Group, d, why, r.Phase)
		}
	}
	// The same group at another interval, as a file may have it: the
	// hash's modulo, not the one it runs at.
	d, why := l.Place("phase-a", 5*time.Minute)
	want := offset(hashStrings, "phase-a", "/etc/prometheus/rules.yml", 5*time.Minute)
	if why != "" || d != want {
		t.Errorf("at 5m: %s %q, want %s", d, why, want)
	}
	if _, why := l.Place("nothing", time.Minute); !strings.Contains(why, "no group") {
		t.Errorf("not listed: %q", why)
	}
}

func TestListingPlace(t *testing.T) {
	min := time.Minute
	// Groups placed by the hash, as the layouts have it, and the one slice
	// labels' build's too.
	for layout, name := range map[int]string{hashStrings: "string labels", hashSlice: "slice labels"} {
		var gs [][4]any
		for _, g := range []string{"a", "b", "c", "d"} {
			off := offset(layout, g, "/rules/"+g+".yml", min)
			gs = append(gs, [4]any{g, "/rules/" + g + ".yml", 60, time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC).Add(off + 40*time.Millisecond).Format(time.RFC3339Nano)})
		}
		l, err := ReadListing(listAPI(gs...))
		if err != nil || l.layout != layout {
			t.Fatalf("%s: layout %d %v", name, l.layout, err)
		}
		// A file of another interval is placed by the same hash.
		d, why := l.Place("a", 5*min)
		if why != "" || d != offset(layout, "a", "/rules/a.yml", 5*min) {
			t.Errorf("%s: %s %q", name, d, why)
		}
	}
	// A name listed twice, in two files: not placed.
	l, _ := ReadListing(listAPI([4]any{"g", "/a.yml", 60, "2026-11-11T12:30:07Z"}, [4]any{"g", "/b.yml", 60, "2026-11-11T12:30:09Z"}))
	if d, why := l.Place("g", min); d != 0 || !strings.Contains(why, "2 groups") {
		t.Errorf("twice: %s %q", d, why)
	}
	// A server that places groups by a hash of its own: where the group of
	// that name last ran, at the same interval; not at another.
	l, _ = ReadListing(listAPI([4]any{"g", "/a.yml", 60, "2026-11-11T12:30:07Z"}, [4]any{"h", "/b.yml", 60, "2026-11-11T12:30:33Z"}, [4]any{"k", "/c.yml", 60, "2026-11-11T12:30:51Z"}))
	if l.layout != -1 {
		t.Fatalf("layout %d for groups no hash places", l.layout)
	}
	if d, why := l.Place("g", min); d != 7*time.Second || why != "" {
		t.Errorf("by its last evaluation: %s %q", d, why)
	}
	if d, why := l.Place("g", 5*min); d != 0 || why == "" {
		t.Errorf("at another interval: %s %q", d, why)
	}
	// One that hasn't run.
	l, _ = ReadListing(listAPI([4]any{"g", "/a.yml", 60, "0001-01-01T00:00:00Z"}))
	if d, why := l.Place("g", min); d != 0 || !strings.Contains(why, "hasn't run") {
		t.Errorf("not run: %s %q", d, why)
	}
	// Not an answer.
	if _, err := ReadListing([]byte(`{"status":"error"}`)); err == nil {
		t.Error("an error answer was read")
	}
	var none *Listing
	if _, why := none.Place("g", min); why == "" || none.Tag() != "" {
		t.Error("a listing not read places nothing")
	}
}

// Replaying places a rule read from a file by its listing, and says which
// groups it couldn't; rules from an API, Grafana's, Loki's from its API and
// those whose group is Loki's namespace's are left as they are.
func TestReplayingPlacesRuleFiles(t *testing.T) {
	min := time.Minute
	off := offset(hashStrings, "web", "/etc/rules/web.yml", min)
	var gs [][4]any
	for _, g := range []string{"web", "db", "cache", "q"} {
		o := offset(hashStrings, g, "/etc/rules/"+g+".yml", min)
		gs = append(gs, [4]any{g, "/etc/rules/" + g + ".yml", 60, time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC).Add(o).Format(time.RFC3339Nano)})
	}
	gs = append(gs, [4]any{"twice", "/a.yml", 60, "2026-11-11T12:30:07Z"}, [4]any{"twice", "/b.yml", 60, "2026-11-11T12:30:09Z"})
	l, err := ReadListing(listAPI(gs...))
	if err != nil {
		t.Fatal(err)
	}
	mk := func(group string, interval time.Duration) Rule {
		return Rule{Name: group + "Down", Group: group, Expr: "up == 0", Interval: interval, Prometheus: true, Source: "/rules/x.yml"}
	}
	rs := []Rule{mk("web", min), mk("twice", min), mk("absent", min), mk("db", 0), mk("cache", 5*min)}
	api := mk("web", min)
	api.Source, api.Phase = "api", 0
	rs = append(rs, api)
	u := &Unplaced{}
	Replaying{Listing: l, Unplaced: u}.Apply(rs)
	if rs[0].Phase != off {
		t.Errorf("web: %s, want %s", rs[0].Phase, off)
	}
	if rs[4].Phase != offset(hashStrings, "cache", "/etc/rules/cache.yml", 5*min) {
		t.Errorf("cache at 5m: %s", rs[4].Phase)
	}
	for _, i := range []int{1, 2, 3, 5} {
		if rs[i].Phase != 0 {
			t.Errorf("%s placed at %s", rs[i].Group, rs[i].Phase)
		}
	}
	said := strings.Join(u.Said("Prometheus"), "\n")
	for _, want := range []string{"2 groups placed as Prometheus places them", `"twice"`, `"absent"`, `"db"`, "no group of that name", "2 groups of that name", "gives the group no interval"} {
		if !strings.Contains(said, want) {
			t.Errorf("said %q, want %q", said, want)
		}
	}
	if strings.Contains(said, "web") && !strings.Contains(said, "placed") {
		t.Errorf("a group placed is not said as not: %q", said)
	}
	// No listing: nothing placed, nothing said.
	rs = []Rule{mk("web", min)}
	Replaying{Unplaced: u}.Apply(rs)
	if rs[0].Phase != 0 {
		t.Errorf("without a listing: %s", rs[0].Phase)
	}
}
