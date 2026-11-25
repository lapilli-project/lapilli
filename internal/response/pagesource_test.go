package response

import (
	"fmt"
	"lapilli.dev/internal/incident"
	"testing"
	"time"
)

// With pages from several sources for one firing, the file's is its page
// even when another is nearer, and among a token's sources the nearest wins,
// those as far apart by source then id, whatever the order they were given in.
func TestFindPrecedesFileSource(t *testing.T) {
	mk := func(src, id string, sent int) Page {
		return Page{ID: id, Source: src, Sent: at(sent), Alerts: []incident.Alert{alert("Disk", "ns", "shop")}}
	}
	labels := map[string]string{"ns": "shop"}
	find := func(pages ...Page) *Page {
		ps, err := Check(pages, "x")
		if err != nil {
			t.Fatal(err)
		}
		return NewIndex(ps).Find("Disk", labels, at(10), at(30))
	}
	if p := find(mk("pd", "near", 10), mk(FileSource, "far", 25)); p.ID != "far" {
		t.Errorf("a nearer API page overrode the file's: %s", p.ID)
	}
	if p := find(mk(FileSource, "far", 25), mk("pd", "near", 10)); p.ID != "far" {
		t.Errorf("order of the pages decided: %s", p.ID)
	}
	if p := find(mk("pd", "a", 20), mk("ops", "z", 12)); p.ID != "z" {
		t.Errorf("among API sources, the nearest: %s", p.ID)
	}
	// As far apart: the lesser source, then the lesser id, whichever is first.
	for _, ps := range [][]Page{
		{mk("b", "1", 12), mk("a", "9", 12)},
		{mk("a", "9", 12), mk("b", "1", 12)},
	} {
		if p := find(ps...); p.Source != "a" {
			t.Errorf("a tie broken by order: %s/%s", p.Source, p.ID)
		}
	}
}

// A rule with more pages in the window than MaxFindScan is looked at in its
// first MaxFindScan only.
func TestFindScansABoundedNumber(t *testing.T) {
	var pages []Page
	for i := 0; i < MaxFindScan+5; i++ {
		// Each lists another namespace but the firing's, but the last.
		ns := fmt.Sprintf("n%d", i)
		pages = append(pages, Page{ID: fmt.Sprintf("p%05d", i), Sent: at(10).Add(time.Duration(i) * time.Second), Alerts: []incident.Alert{alert("Disk", "ns", ns)}})
	}
	ps, err := Check(pages, "x")
	if err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(ps)
	if p := ix.Find("Disk", map[string]string{"ns": "n3"}, at(10), at(30)); p == nil || p.ID != "p00003" {
		t.Errorf("a page among the first: %v", p)
	}
	if p := ix.Find("Disk", map[string]string{"ns": fmt.Sprintf("n%d", MaxFindScan+2)}, at(10), at(30)); p != nil {
		t.Errorf("a page past the bound was found: %v", p)
	}
}

// A JSON file that starts with a byte order mark is read, not refused.
func TestParseJSONWithBOM(t *testing.T) {
	b := []byte("\xef\xbb\xbf {\"responses\": [{\"id\": \"p1\", \"alerts\": [{\"rule\": \"R\", \"labels\": {}}], \"sent\": \"2026-11-20T00:00:00Z\"}]}")
	ps, err := Parse(b, "r.json")
	if err != nil || len(ps) != 1 {
		t.Fatalf("%v %v", ps, err)
	}
}

// Two sources may use one id: a page of one that matched is not the other's.
func TestUnmatchedKeysBySourceAndID(t *testing.T) {
	a := Page{ID: "p", Source: "a", Sent: t0}
	b := Page{ID: "p", Source: "b", Sent: t0}
	ix := NewIndex([]Page{a, b})
	if n := ix.Unmatched(map[PageKey]bool{a.Key(): true}); n != 1 {
		t.Errorf("unmatched %d, want 1", n)
	}
}
