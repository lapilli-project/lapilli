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
// with S3CR3T in it: none is left, and what is around it is. An error that
// only names a credential, or an address without one, reads as it was.
func TestScrubSecretsEveryForm(t *testing.T) {
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
		{`sk-ant-S3CR3TVALUE30 glpat-S3CR3TVALUE31 ghp_S3CR3TVALUE32 xoxb-S3CR3TVALUE33`, `sk-ant-*** glpat-*** ghp_*** xoxb-***`},
		{`gho_S3CR3T1, github_pat_S3CR3T_2 and xoxp-1-S3CR3T`, `gho_***, github_pat_*** and xoxp-***`},
	} {
		got := scrubSecrets(c.in)
		if got != c.want || strings.Contains(got, "S3CR3T") || strings.Contains(got, "AKIAS3") {
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
