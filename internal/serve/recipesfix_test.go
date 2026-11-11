package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// Rootly takes five GETs a minute: a 429 is waited out as its Retry-After
// says, and the run goes on.
func TestRootlyWaitsOutTooMany(t *testing.T) {
	needTools(t)
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1) == 2 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many", http.StatusTooManyRequests)
			return
		}
		name := "rootly-1.json"
		if r.URL.Query().Get("page[number]") == "2" {
			name = "rootly-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "rootly.sh", "ROOTLY_API="+srv.URL, "ROOTLY_TOKEN_FILE="+tokenFile(t, "x"), "SINCE=2026-10-01")
	if len(got) != 2 || asked.Load() != 3 {
		t.Errorf("%d incidents in %d requests", len(got), asked.Load())
	}
}

// Numbered pages with no order can give an incident twice: it is one, and
// the file still loads.
func TestRecipesOneIncidentOnTwoPages(t *testing.T) {
	needTools(t)
	for _, c := range []struct {
		script, first, second, page string
		env                         func(string) []string
	}{
		{"rootly.sh", "rootly-1.json", "rootly-2.json", "page[number]", func(u string) []string {
			return []string{"ROOTLY_API=" + u, "ROOTLY_TOKEN_FILE=" + tokenFile(t, "x")}
		}},
		{"firehydrant.sh", "firehydrant-1.json", "firehydrant-2.json", "page", func(u string) []string {
			return []string{"FIREHYDRANT_API=" + u, "FIREHYDRANT_TOKEN_FILE=" + tokenFile(t, "x")}
		}},
	} {
		// The second page carries the first page's incidents again, as well.
		var one, two map[string]any
		json.Unmarshal(fixture(t, c.first), &one)
		json.Unmarshal(fixture(t, c.second), &two)
		two["data"] = append(two["data"].([]any), one["data"].([]any)...)
		again, _ := json.Marshal(two)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get(c.page) == "2" {
				w.Write(again)
				return
			}
			w.Write(fixture(t, c.first))
		}))
		got := runRecipe(t, c.script, c.env(srv.URL)...)
		srv.Close()
		ids := map[string]int{}
		for _, in := range got {
			ids[in.ID]++
		}
		for id, n := range ids {
			if n > 1 {
				t.Errorf("%s: %s %d times", c.script, id, n)
			}
		}
	}
}

// Compass answers the first 20,000 alerts of a search and refuses a page
// that reaches past them: the recipe stops before asking for one.
func TestCompassStopsAt20000(t *testing.T) {
	needTools(t)
	var last atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.Store(r.URL.Query().Get("offset"))
		if off := r.URL.Query().Get("offset"); off == "19900" {
			http.Error(w, "offset + size past 20000", http.StatusUnprocessableEntity)
			return
		}
		w.Write([]byte(`{"values": [], "links": {"next": "more"}}`))
	}))
	defer srv.Close()
	cmd := exec.Command("sh", recipes+"compass.sh")
	cmd.Env = append(os.Environ(), "COMPASS_API="+srv.URL, "COMPASS_CLOUD_ID=35273b54", "COMPASS_EMAIL=a@example.com",
		"COMPASS_TOKEN_FILE="+tokenFile(t, "x"), "MAX_PAGES=1000")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "more than 20,000 alerts") || last.Load() != "19800" {
		t.Errorf("%v, the last offset asked %v:\n%s", err, last.Load(), out)
	}
}
