package report

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// posted is what a webhook was sent.
type posted struct {
	header http.Header
	body   []byte
}

func hookServer(t *testing.T) (file string, got func() []posted) {
	t.Helper()
	var mu sync.Mutex
	var all []posted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		all = append(all, posted{r.Header.Clone(), b})
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	file = filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(file, []byte(srv.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file, func() []posted {
		mu.Lock()
		defer mu.Unlock()
		return append([]posted(nil), all...)
	}
}

// verified says whether a post's signature holds for key, as a receiver
// checks it: the HMAC-SHA256 of the timestamp, a dot and the body taken.
func verified(p posted, key string) bool {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(p.header.Get("X-Lapilli-Timestamp") + "." + string(p.body)))
	return hmac.Equal([]byte(p.header.Get("X-Lapilli-Signature")), []byte("sha256="+hex.EncodeToString(mac.Sum(nil))))
}

// With a key, a post to the webhook carries its time and its signature,
// of the bytes sent, which the key alone makes; without one, neither.
func TestWebhookSigned(t *testing.T) {
	hook, got := hookServer(t)
	dir := t.TempDir()
	key := "0123456789abcdef 0123456789abcde" // 32 bytes, a space among them
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := []byte(`{"event":"task.created","id":"lapilli-1","text":"a <b> & é"}`)
	at := time.Unix(1_800_000_000, 0)
	if err := Webhook(ctx, hook, body, "lapilli-1", keyFile, at); err != nil {
		t.Fatal(err)
	}
	if err := Webhook(ctx, hook, body, "lapilli-1", keyFile, at.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := Webhook(ctx, hook, body, "lapilli-1", "", at); err != nil {
		t.Fatal(err)
	}
	ps := got()
	if len(ps) != 3 {
		t.Fatalf("%d posts", len(ps))
	}
	first, again, plain := ps[0], ps[1], ps[2]
	if first.header.Get("X-Lapilli-Timestamp") != "1800000000" || again.header.Get("X-Lapilli-Timestamp") != "1800000090" {
		t.Errorf("timestamps: %q, %q", first.header.Get("X-Lapilli-Timestamp"), again.header.Get("X-Lapilli-Timestamp"))
	}
	sig := first.header.Get("X-Lapilli-Signature")
	if !strings.HasPrefix(sig, "sha256=") || len(sig) != len("sha256=")+64 || strings.ToLower(sig) != sig {
		t.Errorf("the signature: %q", sig)
	}
	for i, p := range []posted{first, again} {
		if string(p.body) != string(body) || !verified(p, key) {
			t.Errorf("post %d doesn't verify over the bytes sent: %s %v", i, p.body, p.header)
		}
		if verified(p, key+"\n") || verified(p, strings.ReplaceAll(key, " ", "")) {
			t.Errorf("post %d verifies with another key", i)
		}
	}
	if again.header.Get("X-Lapilli-Signature") == sig {
		t.Error("signed again at another time, the same signature")
	}
	// The signature is of the timestamp too: moved to another time, it
	// doesn't hold.
	moved := posted{first.header.Clone(), first.body}
	moved.header.Set("X-Lapilli-Timestamp", "1800000090")
	if verified(moved, key) {
		t.Error("a signature holds at another timestamp")
	}
	if plain.header.Get("X-Lapilli-Timestamp") != "" || plain.header.Get("X-Lapilli-Signature") != "" || plain.header.Get("Idempotency-Key") != "lapilli-1" {
		t.Errorf("unsigned: %v", plain.header)
	}
}

// A key that can't be read, or is shorter than 32 bytes once its line
// break is let go, posts nothing, and says so without the key.
func TestWebhookKeyRefused(t *testing.T) {
	hook, got := hookServer(t)
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	os.WriteFile(short, []byte("0123456789abcdef0123456789abcde\r\n"), 0o600) // 31 and a line break
	for _, f := range []string{short, filepath.Join(dir, "none")} {
		err := Webhook(context.Background(), hook, []byte(`{}`), "lapilli-1", f, time.Now())
		if err == nil || !strings.Contains(err.Error(), "--notify-webhook-secret-file") || strings.Contains(err.Error(), "0123456789") {
			t.Errorf("%s: %v", f, err)
		}
	}
	// A key another flag gave says that flag.
	for _, f := range []string{short, filepath.Join(dir, "none")} {
		err := WebhookFrom(context.Background(), hook, "--webhook-dest payments-hook", []byte(`{}`), "lapilli-1", f, "--webhook-dest-secret payments-hook", time.Now())
		if err == nil || !strings.HasPrefix(err.Error(), "--webhook-dest-secret payments-hook: ") || strings.Contains(err.Error(), "--notify-webhook-secret-file") || strings.Contains(err.Error(), "0123456789") {
			t.Errorf("%s: %v", f, err)
		}
	}
	if n := len(got()); n != 0 {
		t.Errorf("%d posts with no key to sign them", n)
	}
}

// One line break at the end isn't of the key, \n or \r\n; anything else,
// spaces and a lone \r among it, is.
func TestTrimKey(t *testing.T) {
	for in, want := range map[string]string{
		"key": "key", "key\n": "key", "key\r\n": "key", "key\n\n": "key\n", " key \n": " key ", "key\r": "key\r", "\n": "",
	} {
		if got := string(TrimKey([]byte(in))); got != want {
			t.Errorf("TrimKey(%q) = %q, want %q", in, got, want)
		}
	}
}
