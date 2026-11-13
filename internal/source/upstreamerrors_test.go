package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// A ruler, a Loki's ruler, a Grafana or a cloud that echoes the
// Authorization it was sent in an error. Those errors are the status line
// of every lapilli serve page (s.fail -> statusView.Err), its log, a failed
// change's reason in the ledger (source.Failed from a write), Slack's
// change.failed, and lapilli verdict's stderr: the token, the signature
// and any bearer token are taken out of them, as of what a history API
// answers (prom.serverText).

const echoedToken = "glsa_s3cretTokenValue_0123456789abcdef"

// echoing answers 401 with what a gateway might: the header it refused.
func echoing(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized: refused header Authorization: "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestUpstreamErrorsHoldNoToken(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(echoedToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := echoing(t)
	ctx := context.Background()
	for name, read := range map[string]func() error{
		"--ruler": func() error {
			r, err := NewRuler(ts.URL+"/prometheus/config/v1/rules", "test")
			if err != nil {
				return err
			}
			r.Reads.TokenFile = file
			_, _, err = r.Read(ctx)
			return err
		},
		"--loki-loaded-rules": func() error {
			_, _, err := LokiRules(ctx, Loki{URL: ts.URL, TokenFile: file}, "test", nil)
			return err
		},
		"--grafana": func() error {
			_, _, err := Grafana(ctx, ts.URL, file, "test", nil)
			return err
		},
		"a ruler's write (source.Failed)": func() error {
			r, err := NewRuler(ts.URL+"/prometheus/config/v1/rules", "test")
			if err != nil {
				return err
			}
			code, out, err := r.Call(ctx, http.MethodPost, r.URL+"/shop", []byte("name: g\nrules: []\n"), "application/yaml", Creds{TokenFile: file}, UnitLimit)
			if err != nil {
				return err
			}
			return Failed(code, out)
		},
	} {
		err := read()
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		t.Logf("%s: %v", name, err)
		if strings.Contains(err.Error(), echoedToken) {
			t.Errorf("%s: the error shows the token it was read with: %q", name, err)
		}
		if !strings.Contains(err.Error(), "HTTP 401: unauthorized: refused header Authorization:") {
			t.Errorf("%s: the error doesn't say what was answered: %q", name, err)
		}
	}
}

// A cloud that echoes the credential it was asked with, without the word
// Bearer, is said without it too: what the request carried is taken out of
// what the answer said.
func TestCloudErrorEchoesToken(t *testing.T) {
	a, _ := reader(t, "cw", "https://monitoring.eu-west-1.amazonaws.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"not authorized: ` + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + `"}`))
	}))
	sign := func(r *http.Request) error { r.Header.Set("Authorization", "Bearer "+echoedToken); return nil }
	code, _, body, err := a.Call(context.Background(), http.MethodGet, a.URL+"/?Action=DescribeAlarms", nil, nil, Creds{Sign: sign}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := CloudFailed(code, body); err == nil || strings.Contains(err.Error(), echoedToken) || !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("the cloud's refusal: %v", err)
	}
	// A ruler's write likewise, the token echoed bare.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), http.StatusUnauthorized)
	}))
	defer ts.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(echoedToken), 0o600)
	r, _ := NewRuler(ts.URL+"/prometheus/config/v1/rules", "test")
	code, out, err := r.Call(context.Background(), http.MethodPost, r.URL+"/shop", []byte("name: g\nrules: []\n"), "application/yaml", Creds{TokenFile: file}, UnitLimit)
	if err != nil {
		t.Fatal(err)
	}
	if err := Failed(code, out); strings.Contains(err.Error(), echoedToken) || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("the ruler's refusal: %v", err)
	}
}

// A Grafana that takes the request and never answers is given up on,
// whatever the context: its headers within half a minute, the whole within
// a minute.
func TestGrafanaClientGivesUp(t *testing.T) {
	tr, ok := client.Transport.(*http.Transport)
	if client.Timeout <= 0 || client.Timeout > 2*time.Minute || !ok || tr.ResponseHeaderTimeout <= 0 || tr.ResponseHeaderTimeout > time.Minute {
		t.Errorf("the client to Grafana waits %v, %v for headers", client.Timeout, tr)
	}
	if client.CheckRedirect == nil {
		t.Error("follows redirects")
	}
}

// An error's answer is said without what doesn't show and without a bearer
// token, whatever path it came by (Failed, CloudFailed).
func TestFailedText(t *testing.T) {
	body := []byte("refused: Bearer " + echoedToken + " \x1b]52;c;eA==\x07 ‮done")
	for name, err := range map[string]error{"Failed": Failed(401, body), "CloudFailed": CloudFailed(403, body)} {
		if m := err.Error(); strings.Contains(m, echoedToken) || strings.ContainsAny(m, "\x1b\x07‮") || !strings.Contains(m, "refused: Bearer [redacted]") {
			t.Errorf("%s: %q", name, m)
		}
	}
}

// What a ruler's read passes over is told with its source as it shows: an
// Azure rule group's id is the cloud's.
func TestPassedOverSourceShown(t *testing.T) {
	var list map[string]any
	if err := json.Unmarshal(testdata(t, "azure-list.json"), &list); err != nil {
		t.Fatal(err)
	}
	item := list["value"].([]any)[0].(map[string]any)
	item["name"] = "sh\x1b]0;x\x07op"
	item["id"] = strings.Replace(item["id"].(string), "/shop", "/sh\x1b]0;x\x07op", 1)
	labels := map[string]any{}
	for i := range rules.MaxKeys + 1 {
		labels[fmt.Sprintf("k%d", i)] = "v"
	}
	rule := item["properties"].(map[string]any)["rules"].([]any)[0].(map[string]any)
	rule["labels"] = labels
	list["value"] = []any{item}
	delete(list, "nextLink")
	page, _ := json.Marshal(list)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(page) }))
	defer ts.Close()
	r, err := NewRuler("https://management.azure.com/subscriptions/sub-1", "test")
	if err != nil {
		t.Fatal(err)
	}
	r.Transport = &to{ts: ts}
	var told []string
	r.Warn = func(m string) { told = append(told, m) }
	if _, _, err := r.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(told) != 1 || strings.ContainsAny(told[0], "\x1b\x07") || !strings.Contains(told[0], `sh\u{1b}]0;x\u{7}op`) {
		t.Errorf("told %q", told)
	}
}

// GrafanaNotifications' errors hold no token either, one echoed bare among
// them.
func TestGrafanaNotificationsEchoedToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no rights for "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), http.StatusForbidden)
	}))
	defer ts.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(echoedToken), 0o600)
	_, _, _, err := GrafanaNotifications(context.Background(), ts.URL, file, "test")
	if err == nil || strings.Contains(err.Error(), echoedToken) || !strings.Contains(err.Error(), "no rights for") {
		t.Errorf("%v", err)
	}
}

// cancelling ends a request's context once it is answered: what is done
// with the answer after is done with the context ended.
type cancelling struct {
	body   []byte
	cancel context.CancelFunc
}

func (c cancelling) RoundTrip(r *http.Request) (*http.Response, error) {
	c.cancel()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/yaml"}}, Body: io.NopCloser(bytes.NewReader(c.body)), Request: r}, nil
}

// A ruler's read whose context ends once the list is answered is given
// up, not read as rules passed over.
func TestRulerReadLooksAtContext(t *testing.T) {
	r, err := NewRuler("http://ruler/prometheus/config/v1/rules", "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Transport = cancelling{[]byte("shop:\n- name: g\n  rules:\n  - {alert: A, expr: up}\n"), cancel}
	if rs, _, err := r.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("read with its context ended: %d rules, %v", len(rs), err)
	}
}
