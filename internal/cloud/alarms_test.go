package cloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"lapilli.dev/internal/alarmtest"
)

// Google's writes are a service account's, impersonated: the pod's own
// token asks IAM Credentials' generateAccessToken for one of the
// monitoring scope, which is what the request is sent with; reads are the
// pod's own, of monitoring.read.
func TestGoogleImpersonated(t *testing.T) {
	var asked []string
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Scope    []string `json:"scope"`
			Lifetime string   `json:"lifetime"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+strings.Join(in.Scope, ","))
		io.WriteString(w, `{"accessToken": "writer-token", "expireTime": "2099-01-01T00:00:00Z"}`)
	}))
	defer iam.Close()
	wasIAM, wasSource := IAMCredentials, googleSource
	t.Cleanup(func() { IAMCredentials, googleSource = wasIAM, wasSource })
	IAMCredentials = iam.URL
	googleSource = func(_ context.Context, scope string) (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "pod-token " + scope}), nil
	}
	sign, err := Request(context.Background(), "google", "https://monitoring.googleapis.com/v3/projects/p/alertPolicies", "lapilli-writes@p.iam.gserviceaccount.com")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r := httptest.NewRequest(http.MethodPatch, "https://monitoring.googleapis.com/v3/projects/p/alertPolicies/1", nil)
		if err := sign(r); err != nil || r.Header.Get("Authorization") != "Bearer writer-token" {
			t.Fatalf("%q %v", r.Header.Get("Authorization"), err)
		}
	}
	want := "POST /v1/projects/-/serviceAccounts/lapilli-writes@p.iam.gserviceaccount.com:generateAccessToken " +
		"Bearer pod-token https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/monitoring"
	if len(asked) != 1 || asked[0] != want {
		t.Errorf("IAM Credentials was asked %q, want once %q", asked, want)
	}
	read, _ := Request(context.Background(), "google", "https://monitoring.googleapis.com/v3/projects/p/alertPolicies", "")
	r := httptest.NewRequest(http.MethodGet, "https://monitoring.googleapis.com/v3/projects/p/alertPolicies", nil)
	if read(r); r.Header.Get("Authorization") != "Bearer pod-token https://www.googleapis.com/auth/monitoring.read" {
		t.Errorf("a read: %q", r.Header.Get("Authorization"))
	}
}

// OCI's writes are signed as the user an OCI config file names, with the
// key it names; the signature checks out apart from the SDK, body and all.
func TestOCIConfigSigns(t *testing.T) {
	k, pem := alarmtest.OCIKey()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "key.pem"), []byte(pem), 0o600)
	cfg := "[DEFAULT]\nuser=ocid1.user.oc1..writer\nfingerprint=11:22\ntenancy=ocid1.tenancy.oc1..t\nregion=ap-seoul-1\nkey_file=" +
		filepath.Join(dir, "key.pem") + "\n"
	os.WriteFile(filepath.Join(dir, "config"), []byte(cfg), 0o600)
	sign, err := Request(context.Background(), "oci", "https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms", filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	o := alarmtest.NewOCI("ocid1.compartment.oc1..c", &k.PublicKey, "ocid1.tenancy.oc1..t/ocid1.user.oc1..writer/11:22")
	id := o.Add(map[string]any{"displayName": "a", "isEnabled": true})
	c := &http.Client{Transport: &alarmtest.Transport{H: o}}
	req, _ := http.NewRequest(http.MethodPut, "https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms/"+id, strings.NewReader(`{"isEnabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	if err := sign(req); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK || len(o.Errs) > 0 {
		t.Fatalf("%v %v %v", resp, err, o.Errs)
	}
	if _, err := Request(context.Background(), "oci", "https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms", filepath.Join(dir, "nothing")); err == nil {
		t.Error("a config file that isn't there")
	}
}

// IAM Credentials' refusal is shown on pages and in the log: what acts on a
// terminal taken out, and the pod's token, if echoed, too.
func TestGoogleImpersonatedRefusal(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "denied \x1b]52;c;eA==\x07 for "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	defer iam.Close()
	wasIAM, wasSource := IAMCredentials, googleSource
	t.Cleanup(func() { IAMCredentials, googleSource = wasIAM, wasSource })
	IAMCredentials = iam.URL
	googleSource = func(_ context.Context, scope string) (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.pod-token-0123456789abcdef"}), nil
	}
	sign, err := Request(context.Background(), "google", "https://monitoring.googleapis.com/v3/projects/p/alertPolicies", "lapilli-writes@p.iam.gserviceaccount.com")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPatch, "https://monitoring.googleapis.com/v3/projects/p/alertPolicies/1", nil)
	err = sign(r)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403: denied") || strings.ContainsAny(err.Error(), "\x1b\x07") || strings.Contains(err.Error(), "pod-token") {
		t.Errorf("IAM Credentials' refusal: %q", err)
	}
}
