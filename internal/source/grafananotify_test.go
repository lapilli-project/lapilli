package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Grafana's templates, contact points and policies are read with the
// rules' token, from Grafana alone: no redirect is followed, and an answer
// over 8 MB is refused.
func TestGrafanaNotifications(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v1/provisioning/templates":
			w.Write([]byte(`[{"name": "t", "template": "x"}]`))
		case "/api/v1/provisioning/contact-points":
			w.Write([]byte(`[]`))
		case "/api/v1/provisioning/policies":
			w.Write([]byte(`{"receiver": "r"}`))
		case "/moved/api/v1/provisioning/templates":
			http.Redirect(w, r, "http://elsewhere.invalid/", http.StatusFound)
		case "/big/api/v1/provisioning/templates":
			w.Write([]byte(strings.Repeat(" ", 8<<20+1)))
		default:
			http.Error(w, "permission denied", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("glsa_x\n"), 0o600)
	ts, cps, ps, err := GrafanaNotifications(context.Background(), srv.URL+"/", token, "lapilli/test")
	if err != nil || string(ts) != `[{"name": "t", "template": "x"}]` || string(cps) != `[]` || string(ps) != `{"receiver": "r"}` {
		t.Fatalf("%s %s %s %v", ts, cps, ps, err)
	}
	if len(auth) != 3 || auth[0] != "Bearer glsa_x" {
		t.Errorf("auth %q", auth)
	}
	for _, base := range []string{srv.URL + "/moved", srv.URL + "/big", srv.URL + "/denied"} {
		if _, _, _, err := GrafanaNotifications(context.Background(), base, token, "lapilli/test"); err == nil {
			t.Errorf("%s: read", base)
		}
	}
}
