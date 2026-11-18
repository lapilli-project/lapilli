package serve

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// signedHook takes posts to a webhook, failing the first fail of them, and
// keeps each one's headers and body, those it failed too.
type signedHook struct {
	sync.Mutex
	fail  int
	posts []signedPost
	file  string
}

type signedPost struct {
	header http.Header
	body   []byte
	failed bool
}

func newSignedHook(t *testing.T) *signedHook {
	t.Helper()
	sh := &signedHook{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sh.Lock()
		defer sh.Unlock()
		p := signedPost{header: r.Header.Clone(), body: b, failed: sh.fail > 0}
		sh.posts = append(sh.posts, p)
		if p.failed {
			sh.fail--
			http.Error(w, "down", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	sh.file = filepath.Join(t.TempDir(), "hook")
	os.WriteFile(sh.file, []byte(srv.URL+"/hook"), 0o600)
	return sh
}

func (sh *signedHook) all() []signedPost {
	sh.Lock()
	defer sh.Unlock()
	return append([]signedPost(nil), sh.posts...)
}

// holds says whether a post's signature holds for key, as a receiver
// checks it: the HMAC-SHA256 of its timestamp, a dot and the body as
// taken, compared in constant time.
func (p signedPost) holds(key string) bool {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(p.header.Get("X-Lapilli-Timestamp") + "."))
	mac.Write(p.body)
	return hmac.Equal([]byte(p.header.Get("X-Lapilli-Signature")), []byte("sha256="+hex.EncodeToString(mac.Sum(nil))))
}

func (p signedPost) event() string {
	var n struct{ Event string }
	json.Unmarshal(p.body, &n)
	return n.Event
}

// With --notify-webhook-secret-file, every post to the webhook is signed
// over the bytes it carries, each try again at its own time, the same body
// and id each time: a task's notice and a report's report.ready alike. A
// key that turns short posts nothing until it is put right. Neither the key
// nor a signature is in the log.
func TestNotifyWebhookSigned(t *testing.T) {
	sh := newSignedHook(t)
	sh.fail = 1
	key := "a-key-of-thirty-two-bytes-012345"
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte(key+"\n"), 0o600)
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.NotifyWebhook, c.NotifyWebhookSecret = "X-Forwarded-Email", sh.file, keyFile
		c.ExternalURL = "https://lapilli.example.com"
	})
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	ctx := context.Background()
	h := s.Handler()
	if code, body := api(t, h, "POST", "/api/v1/tasks", `{"title": "tune Errors", "rule": "Errors"}`); code != http.StatusCreated {
		t.Fatalf("task: %d %s", code, body)
	}
	tried := time.Unix(1_800_000_000, 0)
	s.wall = func() time.Time { return tried }
	if err := s.sendDue(ctx, "webhook", time.Now()); err != nil {
		t.Fatal(err)
	}
	tried = tried.Add(3 * time.Minute)
	if err := s.sendDue(ctx, "webhook", time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ps := sh.all()
	if len(ps) != 2 || !ps[0].failed || ps[1].failed {
		t.Fatalf("two tries, the first failed: %+v", ps)
	}
	for i, p := range ps {
		if p.event() != "task.created" || !p.holds(key) || p.holds(key+"\n") {
			t.Errorf("try %d: %s %v", i, p.body, p.header)
		}
	}
	if ps[0].header.Get("X-Lapilli-Timestamp") != "1800000000" || ps[1].header.Get("X-Lapilli-Timestamp") != "1800000180" ||
		ps[0].header.Get("X-Lapilli-Signature") == ps[1].header.Get("X-Lapilli-Signature") {
		t.Errorf("each try signed at its own time: %v; %v", ps[0].header, ps[1].header)
	}
	if !bytes.Equal(ps[0].body, ps[1].body) || ps[0].header.Get("Idempotency-Key") != ps[1].header.Get("Idempotency-Key") {
		t.Errorf("tried again, another body or id:\n%s\n%s", ps[0].body, ps[1].body)
	}

	// A report's report.ready, signed as every post is.
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "daily", Schedule: "0 9 * * *", Span: "24h", Sections: []string{"summary", "firings"},
		Dests: []string{"webhook"}, Enabled: true}, "ana", now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	at(t, s, now.Add(6*time.Hour), false)
	ready := 0
	for _, p := range sh.all()[2:] {
		if !p.holds(key) {
			t.Errorf("%s: unsigned, or not so: %v", p.event(), p.header)
		}
		if p.event() == "report.ready" {
			ready++
		}
	}
	if ready != 1 {
		t.Errorf("report.ready: %d", ready)
	}

	// A key that turns short posts nothing, and says why without it.
	before := len(sh.all())
	os.WriteFile(keyFile, []byte("tiny-k3y\n"), 0o600)
	if code, body := api(t, h, "POST", "/api/v1/tasks", `{"title": "again", "rule": "Errors"}`); code != http.StatusCreated {
		t.Fatalf("task: %d %s", code, body)
	}
	if err := s.sendDue(ctx, "webhook", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(sh.all()); n != before {
		t.Errorf("posted with a key too short: %d", n-before)
	}
	if !strings.Contains(logged.String(), "--notify-webhook-secret-file: want a key of 32 bytes at least") {
		t.Errorf("the log doesn't say why: %s", logged.String())
	}
	os.WriteFile(keyFile, []byte(key), 0o600)
	if err := s.sendDue(ctx, "webhook", time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	last := sh.all()
	if len(last) != before+1 || !last[before].holds(key) {
		t.Errorf("put right, not sent signed: %d", len(last)-before)
	}
	for _, p := range last {
		if sig := p.header.Get("X-Lapilli-Signature"); strings.Contains(logged.String(), strings.TrimPrefix(sig, "sha256=")) {
			t.Errorf("a signature in the log: %s", logged.String())
		}
	}
	if strings.Contains(logged.String(), key) || strings.Contains(logged.String(), "tiny-k3y") {
		t.Errorf("the key in the log: %s", logged.String())
	}
}

// Without the flag, a post carries no signature, as before.
func TestNotifyWebhookUnsigned(t *testing.T) {
	sh := newSignedHook(t)
	s, _ := server(t, func(c *Config) { c.UserHeader, c.NotifyWebhook = "X-Forwarded-Email", sh.file })
	if code, body := api(t, s.Handler(), "POST", "/api/v1/tasks", `{"title": "tune Errors", "rule": "Errors"}`); code != http.StatusCreated {
		t.Fatalf("task: %d %s", code, body)
	}
	if err := s.sendDue(context.Background(), "webhook", time.Now()); err != nil {
		t.Fatal(err)
	}
	ps := sh.all()
	if len(ps) != 1 || ps[0].event() != "task.created" {
		t.Fatalf("%+v", ps)
	}
	for name := range ps[0].header {
		if strings.HasPrefix(name, "X-Lapilli-") {
			t.Errorf("unsigned, but with %s", name)
		}
	}
}
