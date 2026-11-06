package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The week's digest goes, once, to the webhook, for everyone and for each
// team something happened to, and to the team that has a webhook of its own;
// and one team's that is down holds up no one else's.
func TestDigestTold(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]string{}
	down := true
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/broken" && down {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		got[r.URL.Path] = append(got[r.URL.Path], string(b))
	}))
	defer recv.Close()
	dir := t.TempDir()
	write := func(name, path string) string {
		file := filepath.Join(dir, name)
		os.WriteFile(file, []byte(recv.URL+path), 0o600)
		return file
	}
	s, rules := server(t, func(c *Config) {
		c.Backfill = 10 * 24 * time.Hour
		c.ExternalURL = "https://lapilli.example.com"
		c.NotifyWebhook = write("hook", "/hook")
		c.TeamDigests = map[string]string{"shop": write("shop", "/shop"), "broken": write("broken", "/broken"), "quiet": write("quiet", "/quiet"), "gone": write("gone", "/gone")}
	})
	os.WriteFile(filepath.Join(rules, "more.yaml"), []byte("groups:\n  - name: more\n    rules:\n      - {alert: Errors2, expr: errors > 1, for: 2m}\n"), 0o644)
	h := s.Handler()
	ctx := context.Background()
	for _, team := range []string{`{"name": "shop", "rules": "^Errors$"}`, `{"name": "broken", "rules": "^Errors2$"}`, `{"name": "quiet", "rules": "^Nothing$"}`} {
		if code, body := api(t, h, "POST", "/api/v1/teams", team); code != http.StatusCreated {
			t.Fatalf("team: %d %s", code, body)
		}
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	for _, d := range s.sendsTo() {
		if err := s.sendDue(ctx, d, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	hooks, shop, quiet := got["/hook"], got["/shop"], got["/quiet"]
	mu.Unlock()
	if len(quiet) != 0 {
		t.Errorf("a team nothing happened to heard: %v", quiet)
	}
	// broken's comes first, by name, and fails.
	if len(shop) != 1 {
		t.Fatalf("shop's webhook: %v", shop)
	}
	var msg struct{ Text string }
	json.Unmarshal([]byte(shop[0]), &msg)
	if !strings.Contains(msg.Text, "Lapilli  2026-10-26 00:00") || !strings.Contains(msg.Text, "Errors") || strings.Contains(msg.Text, "Errors2") {
		t.Errorf("shop's digest is of its rules alone: %s", msg.Text)
	}
	seen := map[string]map[string]any{}
	for _, x := range hooks {
		var m map[string]any
		if err := json.Unmarshal([]byte(x), &m); err != nil {
			t.Fatal(err)
		}
		if m["event"] != "digest.ready" {
			t.Errorf("event: %v", m["event"])
		}
		team, _ := m["team"].(string)
		seen[team] = m
	}
	all, ok := seen[""]
	if !ok {
		t.Fatalf("no digest for everyone in: %v", hooks)
	}
	if all["url"] != "https://lapilli.example.com/" || !strings.Contains(all["text"].(string), "Lapilli  2026-10-26") || all["firings"].(float64) == 0 {
		t.Errorf("the digest for everyone: %v", all)
	}
	team, ok := seen["shop"]
	if !ok || team["url"] != "https://lapilli.example.com/?team=shop" {
		t.Errorf("the digest for shop: %v", seen)
	}
	if _, ok := seen["quiet"]; ok {
		t.Error("the webhook heard of a team nothing happened to")
	}
	// Once only; and the one that was down is tried again.
	mu.Lock()
	n := len(got["/hook"])
	down = false
	mu.Unlock()
	s.jobs(ctx, true)
	for _, d := range s.sendsTo() {
		if err := s.sendDue(ctx, d, time.Now().Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got["/hook"]) != n || len(got["/shop"]) != 1 {
		t.Errorf("told twice: %d → %d, shop %d", n, len(got["/hook"]), len(got["/shop"]))
	}
	if len(got["/broken"]) != 1 {
		t.Errorf("the one that was down: %v", got["/broken"])
	}
}

// What a rule adds is reckoned against every rule, whichever team's are shown:
// another team's rule catching an incident as soon is as good.
func TestCreditAcrossTeams(t *testing.T) {
	s, rules := server(t, func(c *Config) { c.Backfill = 10 * 24 * time.Hour })
	os.WriteFile(filepath.Join(rules, "more.yaml"), []byte("groups:\n  - name: more\n    rules:\n      - {alert: Errors2, expr: errors > 1, for: 2m, labels: {severity: page}}\n"), 0o644)
	h := s.Handler()
	ctx := context.Background()
	for _, team := range []string{`{"name": "shop", "rules": "^Errors$"}`, `{"name": "other", "rules": "^Errors2$"}`} {
		if code, body := api(t, h, "POST", "/api/v1/teams", team); code != http.StatusCreated {
			t.Fatalf("team: %d %s", code, body)
		}
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	for _, path := range []string{"/api/v1/rules?weeks=12", "/api/v1/rules?weeks=12&team=shop"} {
		_, body, _ := get(t, h, path)
		var v struct {
			Lines []struct {
				Name                    string
				Incidents, First, Alone int
			}
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, l := range v.Lines {
			if l.Name != "Errors" {
				continue
			}
			found = true
			// Both rules fire alike, so neither was first, and neither alone.
			if l.Incidents != 1 || l.First != 0 || l.Alone != 0 {
				t.Errorf("%s: %+v", path, l)
			}
		}
		if !found {
			t.Errorf("%s: no Errors in %s", path, body)
		}
	}
}
