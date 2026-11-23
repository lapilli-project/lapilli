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
	"sync"
	"sync/atomic"
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

// A source whose reads aren't counted, never read well, is down once it
// has failed past its bound since its first read failed, as noted where it
// is read. Looking at the sources, the page or the API,
// notes nothing: it neither starts that clock nor moves it, however often,
// and a source no failed read was noted of isn't down for being looked at.
func TestSourceFailingSinceItsFirstFailedRead(t *testing.T) {
	ctx := context.Background()
	s, clock := hookServer(t, func(c *Config) { c.Incidents = filepath.Join(t.TempDir(), "incidents.yaml") })
	h := s.Handler()
	bound := s.sourceStale()
	file := func() sourceItem {
		t.Helper()
		items, err := s.sourceList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(items, func(it sourceItem) bool { return it.ID == "incidents-file" })
		if i < 0 {
			t.Fatalf("no incidents file among %+v", items)
		}
		return items[i]
	}
	look := func() {
		t.Helper()
		for _, path := range []string{"/sources", "/api/v1/sources?all=true"} {
			if code, body, _ := get(t, h, path); code != http.StatusOK {
				t.Fatalf("%s: %d %s", path, code, body)
			}
		}
	}
	// Looked at before any read: nothing noted.
	look()
	if got := s.failingNow(); len(got) != 0 {
		t.Errorf("noted by looking: %v", got)
	}
	// Its first read fails, with no one looking. Looked at first near its
	// bound, and read failing again: the clock is the first failed read's,
	// not the first look's.
	first := *clock
	s.readFile(errors.New("incidents.yaml: line 3: bad"))
	*clock = first.Add(bound - time.Minute)
	look()
	s.readFile(errors.New("incidents.yaml: line 3: bad"))
	look()
	if got := s.failingNow(); len(got) != 1 || !got["incidents-file"].Equal(first) {
		t.Errorf("noted %v, want the first failed read's time, %s", got, first)
	}
	if it := file(); it.State != "failing" || it.Down {
		t.Errorf("a minute before its bound: %s, down %v", it.State, it.Down)
	}
	*clock = first.Add(bound + time.Minute)
	if it := file(); it.State != "failing" || !it.Down {
		t.Errorf("past its bound since its first failed read: %s, down %v", it.State, it.Down)
	}
	if err := s.tellSources(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sourceEvents(t, s); !slices.Equal(got, []string{"source.down incidents-file"}) {
		t.Errorf("told %q", got)
	}
	// Read well: no longer noted.
	s.readFile(nil)
	if got := s.failingNow(); len(got) != 0 {
		t.Errorf("still noted once read well: %v", got)
	}
	if it := file(); it.State != "ok" || it.Down {
		t.Errorf("read well: %s, down %v", it.State, it.Down)
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
			// The page sets the flag as code (flagsAsCode); the API says it as it is.
			want := "the path given to --rules"
			if p == "/sources" {
				want = "the path given to <code>--rules</code>"
			}
			if !c.admin && !strings.Contains(body, want) {
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
		// An Azure Function's key, in its query.
		`POST https://fn.example.com/api/alarms?code=abcDEF123==&x=1: 401`: `POST https://fn.example.com/api/alarms?code=***&x=1: 401`,
		`GET /x?zipcode=12345`: `GET /x?zipcode=12345`,
		// A JSON body quoted back, as is and escaped in a Go string.
		`400: {"token":"tok123","user":"ana"}`:                           `400: {"token":"***","user":"ana"}`,
		`400: {"password": "hunter2", "secret":"s3", "api_key" : "k3y"}`: `400: {"password": "***", "secret":"***", "api_key" : "***"}`,
		`body "{\"client_secret\":\"abc\",\"name\":\"x\"}"`:              `body "{\"client_secret\":\"***\",\"name\":\"x\"}"`,
		// Header lines.
		"request had api-key: abc123\nx-api-key: def456": "request had api-key: ***\nx-api-key: ***",
		`X-Auth-Token: short`:                            `X-Auth-Token: ***`,
		// Given between spaces, as a connection string says them.
		`connect host=db.example.com user=ana password=hunter2 sslmode=require`: `connect host=db.example.com user=ana password=*** sslmode=require`,
		`token=abc failed, access_key=AKIA1 refused`:                            `token=*** failed, access_key=*** refused`,
		`unexpected token: line 3`:                                              `unexpected token: line 3`,
	} {
		if got := scrubSecrets(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// The forms a credential took that the scrubber let through, each made up
// (S3CR3T, FAKE and the like in it): none is left, and what is around it
// is. An error that only names a credential, or a header, and an address
// without one, an @ in its path too, read as they were.
func TestScrubSecretsEveryForm(t *testing.T) {
	// fake is a made-up value, s and as many x after it as make n
	// characters, as long as its maker's token is.
	fake := func(s string, n int) string { return s + strings.Repeat("x", n-len(s)) }
	for _, c := range []struct{ in, want string }{
		// name: value, as YAML and Go's %+v write a field.
		{`password: S3CR3TVALUE9`, `password: ***`},
		{`client_secret: S3CR3TVALUE10`, `client_secret: ***`},
		{`apikey: S3CR3TVALUE8`, `apikey: ***`},
		{`{User:ana Password:S3CR3T1 Host:db}`, `{User:ana Password:*** Host:db}`},
		{`access_token: "S3CR3T two"`, `access_token: ***`},
		{`'token': 'S3CR3TVALUE24'`, `'token': '***'`},
		// An Authorization header, whatever its scheme, or none.
		{`Authorization: Bearer shortS3C`, `Authorization: Bearer ***`},
		{`Authorization: GenieKey S3CR3TVALUE3`, `Authorization: GenieKey ***`},
		{`Authorization: SSWS S3CR3TVALUE28xxxxxx`, `Authorization: SSWS ***`},
		{`Authorization: S3CR3TVALUE4xxxxxxxxx`, `Authorization: ***`},
		{`Authorization: S3CR3TVALUE4 rejected`, `Authorization: ***`},
		{`Authorization: Klaviyo-API-Key S3CR3T5`, `Authorization: ***`},
		{`map[Authorization:[Bearer S3CR3T6]]`, `map[Authorization:[Bearer ***]]`},
		{`Authorization=Bearer%20S3CR3TVALUE36`, `Authorization=***`},
		{`ApiKey S3CR3TVALUE27xxxxxxxxx`, `ApiKey ***`},
		{`token S3CR3TVALUE25short`, `token ***`},
		{`Authorization: Bearer S3CR3TVALUE2abc`, `Authorization: Bearer ***`},
		{`Authorization: Bearer "S3CR3T7"`, `Authorization: Bearer "***"`},
		{`retried with Bearer S3CR3T7`, `retried with Bearer ***`},
		// Headers named for a key or a token.
		{`DD-API-KEY: S3CR3TVALUE5`, `DD-API-KEY: ***`},
		{`DD-APPLICATION-KEY: S3CR3TVALUE6`, `DD-APPLICATION-KEY: ***`},
		{`Api-Key: NRAK-S3CR3TVALUE7`, `Api-Key: ***`},
		{`X-Api-Key: S3CR3TVALUE22`, `X-Api-Key: ***`},
		{`PRIVATE-TOKEN: S3CR3TVALUE23`, `PRIVATE-TOKEN: ***`},
		{`x-goog-api-key: S3CR3TVALUE26`, `x-goog-api-key: ***`},
		{`Cookie: session=S3CR3TVALUE21`, `Cookie: ***`},
		{`Cookie: session=S3CR3TVALUE21; theme=dark`, `Cookie: ***`},
		{`set-cookie: sid=S3CR3T8; Path=/; HttpOnly`, `set-cookie: ***`},
		// A query's keys, and its = and & written as an address quotes them.
		{`Get "https://prom.example/api/v1/query?access_token=S3CR3TVALUE1": EOF`, `Get "https://prom.example/api/v1/query?access_token=***": EOF`},
		{`?X-Amz-Signature=S3CR3TVALUE13&X-Amz-Credential=AKIAS3CR3T14`, `?X-Amz-Signature=***&X-Amz-Credential=***`},
		{`?pass=S3CR3TVALUE15&u=admin`, `?pass=***&u=admin`},
		{`?db_pass=S3CR3T9&u=admin`, `?db_pass=***&u=admin`},
		{`?jwt=S3CR3TVALUE17`, `?jwt=***`},
		{`?sig%3DS3CR3TVALUE37`, `?sig%3D***`},
		{`?url=https%3A%2F%2Fx.example%2Fb%3Fsv%3D1%26sig%3DS3CR3T10`, `?url=https%3A%2F%2Fx.example%2Fb%3Fsv%3D1%26sig%3D***`},
		{`&amp;token=S3CR3TVALUE38`, `&amp;token=***`},
		// A URL's password with an @ or a / in it.
		{`https://user:p@ssS3CR3TVALUE11@prom.example/`, `https://***@prom.example/`},
		{`postgres://lap:S3CR3T/VAL34@db.example:5432/l`, `postgres://***@db.example:5432/l`},
		{`Get "https://ana:S3CR3T@x@prom.example/api?query=up": EOF`, `Get "https://***@prom.example/api?query=up": EOF`},
		// A connection string's quoted password.
		{`user=lap password='S3CR3T VAL35' host=db`, `user=lap password=*** host=db`},
		{`user=lap password="S3CR3T VAL\"36" host=db`, `user=lap password=*** host=db`},
		{`parse "password=S3CR3T host=db": refused`, `parse "password=*** host=db": refused`},
		{`yaml: "client_secret: S3CR3T"`, `yaml: "client_secret: ***"`},
		// JSON: a string with escapes in it, a PEM block in one, a number.
		{`{"private_key":"-----BEGIN PRIVATE KEY-----\nS3CR3TVALUE18\n-----END PRIVATE KEY-----\n","client_email":"x@y.example"}`, `{"private_key":"***","client_email":"x@y.example"}`},
		{`{"client_secret": "S3CR3TVALUE19"}`, `{"client_secret": "***"}`},
		{`{"private_key":"-----BEGIN PRIVATE KEY-----\nS3CR3TVALUE18\n-----END PRIVATE KEY-----\n"}`, `{"private_key":"***"}`},
		{`{"password": "a\"S3CR3TVALUE20"}`, `{"password": "***"}`},
		{`{"password": "a\"S3CR3TVALUE20", "user": "ana"}`, `{"password": "***", "user": "ana"}`},
		{`body "{\"password\":\"a\\\"S3CR3T\\\\\",\"user\":\"ana\"}"`, `body "{\"password\":\"***\",\"user\":\"ana\"}"`},
		{`server said: {"error":"bad key","api_key":"S3CR3TVALUE29"}`, `server said: {"error":"bad key","api_key":"***"}`},
		{`{"token":12345678901234,"user":"ana"}`, `{"token":***,"user":"ana"}`},
		// A private key's PEM block outside JSON, whole or cut short.
		{"key: -----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nS3CR3T+/=\n-----END RSA PRIVATE KEY-----\nnext", "key: -----BEGIN RSA PRIVATE KEY-----***-----END RSA PRIVATE KEY-----\nnext"},
		{`"pem": "-----BEGIN OPENSSH PRIVATE KEY-----\nS3CR3T`, `"pem": "-----BEGIN OPENSSH PRIVATE KEY-----***`},
		// A Slack webhook, and tokens their makers prefix as theirs.
		{`POST https://hooks.slack.com/services/T000/B000/S3CR3TVALUE12: 404`, `POST https://hooks.slack.com/services/***: 404`},
		{"sk-ant-api03-" + fake("S3CR3TVALUE30", 93) + "AA glpat-" + fake("S3CR3TVALUE31", 20) + " ghp_" + fake("S3CR3TVALUE32", 36) + " xoxb-1234567890-1234567890123-" + fake("S3CR3TVALUE33", 24), `sk-ant-*** glpat-*** ghp_*** xoxb-***`},
		{"gho_" + fake("S3CR3T1", 36) + ", github_pat_" + fake("S3CR3T", 22) + "_" + fake("S3CR3T2", 59) + " and xoxp-1234567890-1234567890-1234567890-" + fake("S3CR3T", 32), `gho_***, github_pat_*** and xoxp-***`},
		{"glpat-" + fake("S3CR3T", 27) + ".010abcdef refused", `glpat-*** refused`},
		{"ya29.c." + fake("S3CR3T", 40), `ya29.***`},
		// Grafana's and Google's, alone in a text.
		{`token glsa_FAKEFAKEFAKEFAKEFAKE_deadbeef refused`, `token *** refused`},
		{`Get "https://x.example/?q=1": glsa_` + fake("FAKE", 32) + `_deadbeef`, `Get "https://x.example/?q=1": glsa_***`},
		{`glc_eyJvIjoiMTIzIiwibiI6ImZha2UifQ==`, `glc_***`},
		{`ya29.a0FakeFakeFakeFakeFakeFake`, `ya29.***`},
		// A URL's password with a / and a ? in it, before a host.
		{`postgres://user:pa/ss?x@db.example:5432/l`, `postgres://***@db.example:5432/l`},
		{`dial postgres://lap:S3CR3T/x#y@db: connection refused`, `dial postgres://***@db: connection refused`},
		{`postgres://lap:S3CR3T/x@[fd00::1]:5432/l`, `postgres://***@[fd00::1]:5432/l`},
		// An Authorization or Cookie header where a header is written, or
		// after a scheme, or with a name=value after it.
		{`Authorization: Bearer abc123`, `Authorization: Bearer ***`},
		{`authorization: Token glsa_FAKEFAKEFAKEFAKE_0123abcd`, `authorization: Token ***`},
		{"GET /x\nAuthorization: S3CR3TVALUE40", "GET /x\nAuthorization: ***"},
		{`Accept: */*, authorization: S3CR3TVALUE41`, `Accept: */*, authorization: ***`},
		{`map[Accept:[*/*] Authorization:[S3CR3TVALUE42]]`, `map[Accept:[*/*] Authorization:[***]]`},
		{`push failed: authorization: Bearer S3CR3TVALUE43`, `push failed: authorization: Bearer ***`},
		{`{Authorization: S3CR3TVALUE48} {Cookie: sid=S3CR3T49}`, `{Authorization: ***} {Cookie: ***}`},
		{`sent with cookie: session=S3CR3TVALUE44; theme=dark`, `sent with cookie: ***`},
		// A field after a word, its name written as code writes one, or
		// with no space after its colon; one on its own line.
		{`error: 403 api_key: FAKEKEY123`, `error: 403 api_key: ***`},
		{`config apiKey: S3CR3TVALUE45`, `config apiKey: ***`},
		{"db:\n  password: S3CR3TVALUE46\n  - passwd: S3CR3TVALUE47", "db:\n  password: ***\n  - passwd: ***"},
		{`x-api-key: abcdef`, `x-api-key: ***`},
		{`password=hunter2 sslmode=require`, `password=*** sslmode=require`},
		{`DSN user=lapilli password='p w' host=db`, `DSN user=lapilli password=*** host=db`},
		// A header or a field's name and a : anywhere in a line, its value
		// to the line's end or a mark that ends a sentence, its words
		// all: with a capital, after curl's >, after the word header, or
		// with what doesn't start as prose does.
		{`header Authorization: S3CR3TVALUE50 rejected`, `header Authorization: ***`},
		{`request failed: Authorization: MyScheme S3CR3TVALUE51`, `request failed: Authorization: ***`},
		{`401 with Authorization: S3CR3TVALUE52`, `401 with Authorization: ***`},
		{`> Authorization: S3CR3TVALUE53`, `> Authorization: ***`},
		{`login: Password: S3CR3T54 refused`, `login: Password: ***`},
		{`push: cookie: S3CR3TSESSION56`, `push: cookie: ***`},
		{`401 with Authorization: S3CR3TVALUE57. Retrying in 5s`, `401 with Authorization: ***. Retrying in 5s`},
		{`login: password: S3CR3T58 two, retrying`, `login: password: ***, retrying`},
		{`header authorization: fakevalue rejected`, `header authorization: ***`},
		{"< Set-Cookie: sid=S3CR3T59; Path=/\n< Location: /", "< Set-Cookie: ***\n< Location: /"},
		{`(Authorization: Bearer S3CR3T60) rejected`, `(Authorization: Bearer ***) rejected`},
		// A password of digits and then a /, which reads as a port and a
		// path, before a host and its port.
		{`dial postgres://lap:1234/S3CR3T55@db:5432/l: refused`, `dial postgres://***@db:5432/l: refused`},
		{`postgres://lap:1234/S3CR3T61@[fd00::1]:5432/l`, `postgres://***@[fd00::1]:5432/l`},
	} {
		got := scrubSecrets(c.in)
		leaked := slices.ContainsFunc([]string{"S3CR3T", "AKIAS3", "FAKE", "hunter2", "pa/ss", "abc123", "abcdef", "deadbeef", "0123abcd", "eyJvIjoi", "a0Fake", "'p w'"},
			func(s string) bool { return strings.Contains(got, s) })
		if got != c.want || leaked {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
	for _, in := range []string{
		`token bucket full`,
		`the key was not found`,
		`password reset failed`,
		`pq: password authentication failed for user "lap"`,
		`Authorization header missing`,
		`invalid bearer`,
		`invalid bearer token`,
		"invalid bearer\n2 retries left",
		`want Authorization: Bearer <token>`,
		`oauth2: cannot fetch token: 400 Bad Request`,
		`unexpected token: line 3`,
		`secret "lapilli" not found`,
		`Get "https://prom.example/api/v1/query?query=up": EOF`,
		`Get "http://prom.example:9090/api/v1/query_range?query=rate(up[5m])&start=1&step=60": EOF`,
		`read tcp 10.0.0.1:443: i/o timeout`,
		`dial tcp prom.example:9090: connect: connection refused`,
		`https://prom.example/path`,
		`open /etc/lapilli/keys/key.pem: permission denied`,
		`ghost_host is up, task-ant-1 too`,
		`GET /x?bypass=1&compass=n`,
		`-----BEGIN CERTIFICATE-----`,
		`{"error":"bad key","code":3}`,
		// An @ in an address's path or query, not in its authority.
		`Get "http://prometheus.example:9090/api/v1/query@x": EOF`,
		`http://host:9090/x@y`,
		`https://grafana.example:3000/d/abc/team@ops?orgId=1`,
		`https://grafana.example/d/abc/team@ops?orgId=1`,
		// Prose that names a header or a credential's field.
		`read https://loki.example:3100/loki/api/v1/push: authorization: the token has expired`,
		`the session cookie: expired after 10m`,
		`invalid password: check the file`,
		`Get "https://grafana.example/api/search": no Password: given`,
		`push: authorization: the token has expired. Log in again`,
		// A word after a token's prefix, shorter than a token or not in
		// its shape.
		`metric glc_requests_total rose`,
		`see https://example.com/ya29.png`,
		`ghp_x`,
		`glsa_x`,
		`glsa_requests_total`,
		`gho_st, ghs_ and ghr_cache`,
		`github_pat_docs`,
		`glpat-example`,
		`xoxb-test and xoxp-1-x`,
		`sk-ant-api03-example`,
		`ya29.png and ya29.example.com`,
	} {
		if got := scrubSecrets(in); got != in {
			t.Errorf("%q: %q, want it as it was", in, got)
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

// A source read beside the rules is read, and judged, by its own reads:
// the rules failing for five hours leave Loki's rule files working, never
// quiet nor told down, and the rules alone are told down and back. One the
// time ran out on before it was read waits, held, however long since it
// last read well.
func TestSourceBesideAFailingOne(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "loki")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "logs.yaml"), []byte("groups:\n  - name: logs\n    rules:\n      - alert: CheckoutLogErrors\n        expr: sum(count_over_time({app=\"checkout\"} |= \"error\" [5m])) > 5\n"), 0o644)
	s, clock := hookServer(t, func(c *Config) { c.Loki, c.LokiRules = "http://loki:3100", dir })
	broken := filepath.Join(s.cfg.RulesPath, "broken.yaml")
	raw := ""
	read := func(fail bool, step time.Duration) {
		t.Helper()
		*clock = clock.Add(step)
		if fail {
			os.WriteFile(broken, []byte("groups: [\n"), 0o644)
		} else {
			os.Remove(broken)
		}
		if err := s.look(ctx, &raw); fail != (err != nil) {
			t.Fatalf("a read meant to fail %v: %v", fail, err)
		}
		if err := s.tellSources(ctx); err != nil {
			t.Fatal(err)
		}
	}
	state := func(id string) sourceItem {
		t.Helper()
		items, err := s.sourceList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(items, func(it sourceItem) bool { return it.ID == id })
		if i < 0 {
			t.Fatalf("no %s in %+v", id, items)
		}
		return items[i]
	}
	read(false, 0)
	for range 30 {
		read(true, 10*time.Minute)
	}
	if it := state("loki"); it.State != "ok" || it.Down {
		t.Errorf("the rules failing five hours: Loki's rule files %s, down %v", it.State, it.Down)
	}
	read(false, time.Minute)
	read(false, 20*time.Minute)
	if got, want := sourceEvents(t, s), []string{"source.down rules", "source.back rules"}; !slices.Equal(got, want) {
		t.Errorf("told %q, want %q", got, want)
	}

	// The time out before Loki's turn: held, not read, and not quiet five
	// hours on.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.readRules(gone); err == nil {
		t.Fatal("a read with no time left read")
	}
	*clock = clock.Add(5 * time.Hour)
	if it := state("loki"); it.State != "pending" || it.Down {
		t.Errorf("held five hours: %s, down %v", it.State, it.Down)
	}
	read(false, 0)
	if it := state("loki"); it.State != "ok" {
		t.Errorf("read again: %s", it.State)
	}
}

// The watch and the jobs each look at the sources, at once at times, each
// at its own second: a source back is told back once between them.
func TestSourceBackOnce(t *testing.T) {
	ctx := context.Background()
	s, clock := hookServer(t, nil)
	for range sourceDownAfter {
		s.noteRead("rules", time.Now(), 2, errors.New("the ruler answered 503"))
	}
	if err := s.tellSources(ctx); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	s.noteRead("rules", time.Now(), 2, nil)
	base := clock.Add(sourceBackAfter + time.Minute)
	var ticks atomic.Int64
	s.now = func() time.Time { return base.Add(time.Duration(ticks.Add(1)) * time.Second) }
	const rounds = 10
	for round := range rounds {
		if round > 0 {
			// Kept down again, nothing told: the next round tells it back.
			if err := s.l.TellOnce(ctx, toldKey("rules"), "down", "source.down", nil); err != nil {
				t.Fatal(err)
			}
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				<-start
				if err := s.tellSources(ctx); err != nil {
					t.Error(err)
				}
			})
		}
		close(start)
		wg.Wait()
	}
	backs := 0
	for _, e := range sourceEvents(t, s) {
		if e == "source.back rules" {
			backs++
		}
	}
	if backs != rounds {
		t.Errorf("told back %d times in %d rounds", backs, rounds)
	}
}

// An incidents id prefix nothing was written to in two days is down, and
// told so; one nothing was written to in 14 days was an id's one-off
// prefix, not a job's: left out of the list, and never told down.
func TestSourceIntakeForgotten(t *testing.T) {
	ctx := context.Background()
	s, _ := hookServer(t, func(c *Config) {
		c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", tokenFile(t, "incidents-token-0123456789")
	})
	for name, ago := range map[string]time.Duration{"PAY": time.Hour, "SEARCH": 3 * 24 * time.Hour, "oneoff": 15 * 24 * time.Hour} {
		if err := s.l.NoteReceived(ctx, name, now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.sourceList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, it := range items {
		if strings.HasPrefix(it.ID, "intake/") {
			listed = append(listed, it.ID+" "+it.State)
		}
	}
	if want := []string{"intake/PAY ok", "intake/SEARCH quiet"}; !slices.Equal(listed, want) {
		t.Errorf("listed %q, want %q", listed, want)
	}
	if err := s.tellSources(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sourceEvents(t, s); !slices.Equal(got, []string{"source.down intake/SEARCH"}) {
		t.Errorf("told %q", got)
	}
}

// A replica that led, failed to read a source it never read well, and
// followed since kept its note: leading again days later, the source was
// down at once by that old note, before this replica read it at all,
// though the leader between may have read it well all along. Leading
// lets go of what the replica noted before; its first failed read in the
// new term is noted from then.
func TestLeadingAgainForgetsWhatWasNotedFailing(t *testing.T) {
	ctx := context.Background()
	s, clock := hookServer(t, func(c *Config) { c.Incidents = filepath.Join(t.TempDir(), "incidents.yaml") })
	s.readFile(errors.New("incidents.yaml: mounted late"))
	if len(s.failingNow()) != 1 {
		t.Fatalf("noted: %v", s.failingNow())
	}
	file := func() sourceItem {
		t.Helper()
		items, err := s.sourceList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(items, func(it sourceItem) bool { return it.ID == "incidents-file" })
		if i < 0 {
			t.Fatalf("no incidents file among %+v", items)
		}
		return items[i]
	}
	// Three days on, it leads again: this term's loops stopped at once.
	*clock = clock.Add(72 * time.Hour)
	term, cancel := context.WithCancel(ctx)
	cancel()
	s.lead(term, make(chan error, 1))
	if got := s.failingNow(); len(got) != 0 {
		t.Errorf("leading again, what was noted before: %v", got)
	}
	if it := file(); it.Down {
		t.Errorf("leading again, before any read: %s, down", it.State)
	}
	// Its first failed read in this term is noted from then.
	s.readFile(errors.New("incidents.yaml: mounted late"))
	if got := s.failingNow()["incidents-file"]; !got.Equal(*clock) {
		t.Errorf("noted again at %v, want %v", got, *clock)
	}
}
