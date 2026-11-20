package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// sourceEvents are the notices of sources down or back the webhook has
// waiting, "EVENT ID" each, in order.
func sourceEvents(t *testing.T, s *Server) []string {
	t.Helper()
	ns, err := s.l.Due(context.Background(), "webhook", time.Now().Add(time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range ns {
		if x.Event != "source.down" && x.Event != "source.back" {
			continue
		}
		var n notice
		if err := json.Unmarshal(x.Body, &n); err != nil || n.SourceRead == nil {
			t.Fatalf("%s: %s %v", x.Event, x.Body, err)
		}
		out = append(out, x.Event+" "+n.SourceRead.ID)
	}
	return out
}

// hookServer is serve telling the webhook what happens, its clock at
// *clock, with the rules read once.
func hookServer(t *testing.T, change func(*Config)) (*Server, *time.Time) {
	t.Helper()
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x"), 0o600)
	s, _ := server(t, func(c *Config) {
		c.NotifyWebhook = hook
		if change != nil {
			change(c)
		}
	})
	clock := now
	s.now = func() time.Time { return clock }
	return s, &clock
}

// A source whose reads fail three times in a row is told down, once
// however long it stays down; back once it has read well for
// sourceBackAfter; down again no sooner than sourceRenotify after it was
// told back.
func TestSourceDownAndBack(t *testing.T) {
	ctx := context.Background()
	s, clock := hookServer(t, nil)
	raw := ""
	broken := filepath.Join(s.cfg.RulesPath, "broken.yaml")
	read := func(fail bool, step time.Duration) {
		t.Helper()
		*clock = clock.Add(step)
		if fail {
			os.WriteFile(broken, []byte("groups: [\n"), 0o644)
		} else {
			os.Remove(broken)
		}
		err := s.look(ctx, &raw)
		if fail != (err != nil) {
			t.Fatalf("a read meant to fail %v: %v", fail, err)
		}
		if err := s.tellSources(ctx); err != nil {
			t.Fatal(err)
		}
	}
	want := func(when string, events ...string) {
		t.Helper()
		if got := sourceEvents(t, s); !slices.Equal(got, events) {
			t.Errorf("%s: told %q, want %q", when, got, events)
		}
	}
	read(false, 0)
	want("read well")
	read(true, time.Minute)
	read(true, time.Minute)
	want("two reads failed")
	read(true, time.Minute)
	want("three reads failed", "source.down rules")
	for range 30 {
		read(true, time.Minute)
	}
	want("down half an hour", "source.down rules")
	read(false, time.Minute)
	read(false, 10*time.Minute)
	want("reading well for ten minutes", "source.down rules")
	read(false, 5*time.Minute)
	want("reading well for fifteen minutes", "source.down rules", "source.back rules")
	// Down again at once: shown, not told until the hour is out.
	for range 3 {
		read(true, time.Minute)
	}
	want("down again within the hour", "source.down rules", "source.back rules")
	items, err := s.sourceList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(items, func(it sourceItem) bool { return it.ID == "rules" }); i < 0 || !items[i].Down || items[i].State != "failing" || items[i].Failures != 3 {
		t.Errorf("not shown down: %+v", items)
	}
	read(true, time.Hour)
	want("down past the hour", "source.down rules", "source.back rules", "source.down rules")
}

// A source that flaps, failing three reads and reading one well, over and
// over, is told down once: one read well isn't back.
func TestSourceFlapping(t *testing.T) {
	ctx := context.Background()
	s, clock := hookServer(t, nil)
	for i := range 240 {
		*clock = clock.Add(time.Minute)
		var err error
		if i%4 != 3 {
			err = errors.New("the ruler answered 503")
		}
		s.noteRead("rules", time.Now(), 2, err)
		if err := s.tellSources(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := sourceEvents(t, s); !slices.Equal(got, []string{"source.down rules"}) {
		t.Errorf("four hours of flapping told %q", got)
	}
}

// Each source is told of by its own bounds: a file the runs read, failing
// past two runs and more, is down; firings copied in are told of as
// source.quiet alone.
func TestSourceDownByItsBound(t *testing.T) {
	ctx := context.Background()
	s, clock := hookServer(t, func(c *Config) { c.Deploys = filepath.Join(t.TempDir(), "deploys.yaml") })
	os.WriteFile(s.cfg.Deploys, []byte("deploys: []\n"), 0o644)
	s.noteRead("deploys-file", time.Now(), -1, nil)
	*clock = clock.Add(time.Hour)
	s.noteRead("deploys-file", time.Now(), -1, errors.New("deploys.yaml: line 3: bad"))
	if err := s.tellSources(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sourceEvents(t, s); len(got) != 0 {
		t.Errorf("one failed read told: %q", got)
	}
	s.noteRead("deploys-file", time.Now(), -1, errors.New("deploys.yaml: line 3: bad"))
	s.noteRead("deploys-file", time.Now(), -1, errors.New("deploys.yaml: line 3: bad"))
	if err := s.tellSources(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sourceEvents(t, s); !slices.Equal(got, []string{"source.down deploys-file"}) {
		t.Errorf("three failed reads told %q", got)
	}
}

// What the Sources page, the API and a notice say of a source's error:
// the paths of serve's files to an admin alone, as Run history says them,
// and a credential to no one.
func TestSourcesRedacted(t *testing.T) {
	ctx := context.Background()
	s, _ := hookServer(t, func(c *Config) {
		c.UserHeader, c.Admins, c.Viewers = "X-Forwarded-Email", []string{"boss@example.com"}, []string{"*"}
	})
	why := `open ` + filepath.Join(s.cfg.RulesPath, "shop.yaml") + `: permission denied; Get "https://ops:hunter2@ruler.example/api?x=1&token=s3cr3tvalue": Authorization: Bearer abcdefghijklmnop1234`
	for range 3 {
		s.noteRead("rules", time.Now(), 0, errors.New(why))
	}
	secrets := []string{"hunter2", "s3cr3tvalue", "abcdefghijklmnop1234"}
	h := s.Handler()
	for _, c := range []struct {
		who   string
		admin bool
	}{{"vi@example.com", false}, {"boss@example.com", true}} {
		for _, p := range []string{"/sources", "/api/v1/sources?all=true"} {
			code, body := getAs(t, h, p, c.who)
			if code != http.StatusOK || !strings.Contains(body, "permission denied") {
				t.Fatalf("%s as %s: %d %.300s", p, c.who, code, body)
			}
			for _, x := range secrets {
				if strings.Contains(body, x) {
					t.Errorf("%s as %s says %q", p, c.who, x)
				}
			}
			if has := strings.Contains(body, s.cfg.RulesPath); has != c.admin {
				t.Errorf("%s as %s: the rules' path shown %v", p, c.who, has)
			}
			if !c.admin && !strings.Contains(body, "the path given to --rules") {
				t.Errorf("%s as %s doesn't say which path: %.500s", p, c.who, body)
			}
		}
	}
	if err := s.tellSources(ctx); err != nil {
		t.Fatal(err)
	}
	ns, err := s.l.Due(ctx, "webhook", time.Now().Add(time.Hour), 100)
	if err != nil || len(ns) == 0 {
		t.Fatalf("no notice: %v", err)
	}
	for _, n := range ns {
		body := string(n.Body)
		for _, x := range append(secrets, s.cfg.RulesPath) {
			if strings.Contains(body, x) {
				t.Errorf("the notice says %q: %s", x, body)
			}
		}
	}
	if line := s.line(notice{Event: "source.down", SourceRead: &sourceNotice{Name: "Rule files", From: "--rules", Failures: 3, Error: "x"}}); line != "lapilli: Rule files (--rules) failed 3 reads in a row: x" {
		t.Errorf("Slack's line: %q", line)
	}
}

// What may be a credential is taken out of what a source says, wherever it
// is in the text.
func TestScrubSecrets(t *testing.T) {
	for in, want := range map[string]string{
		`Get "https://user:pw@h.example/x": refused`:         `Get "https://***@h.example/x": refused`,
		`dial https://tok3n@h.example:443`:                   `dial https://***@h.example:443`,
		`GET /api?a=1&api_key=k3y&b=2`:                       `GET /api?a=1&api_key=***&b=2`,
		`GET /x?X-Amz-Signature=abc&X-Amz-Credential=AKIA/1`: `GET /x?X-Amz-Signature=***&X-Amz-Credential=***`,
		`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.x.y`:     `Authorization: Bearer ***`,
		`token=ghp_abcdefghijklmnopqrstuvwxyz`:               `token=***`,
		`basic auth failed`:                                  `basic auth failed`,
		`open /etc/lapilli/rules.yaml: no such file`:         `open /etc/lapilli/rules.yaml: no such file`,
	} {
		if got := scrubSecrets(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// GET /api/v1/sources without all is the sources firings are copied in
// from, as before; with all=true, every source; anything else, 400. The
// status banner's failure to read a source links to the page.
func TestSourcesAPIAndBanner(t *testing.T) {
	s, _ := hookServer(t, nil)
	h := s.Handler()
	code, body, _ := get(t, h, "/api/v1/sources")
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Errorf("as before: %d %s", code, body)
	}
	code, body, hd := get(t, h, "/api/v1/sources?all=true")
	var all struct {
		Sources []sourceItem `json:"sources"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &all) != nil || len(all.Sources) == 0 || hd.Get("Cache-Control") != "no-store" {
		t.Errorf("all: %d %v %s", code, hd, body)
	}
	if code, _, _ := get(t, h, "/api/v1/sources?all=yes"); code != http.StatusBadRequest {
		t.Errorf("all=yes: %d", code)
	}
	s.fail("reading the rules", errors.New("the ruler answered 503"))
	_, page, _ := get(t, h, "/rules")
	if !strings.Contains(page, `the ruler answered 503 <a href="/sources">Sources</a>`) {
		t.Errorf("the banner doesn't link to the Sources page")
	}
	s.fail("pruning", errors.New("disk full"))
	if _, page, _ := get(t, h, "/rules"); strings.Contains(page, `disk full <a href="/sources">`) {
		t.Errorf("the banner links a failure that isn't a source's")
	}
}
