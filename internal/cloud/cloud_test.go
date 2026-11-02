package cloud

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"lapilli.dev/internal/prom"
)

// api answers query_range with one empty matrix, after check has looked at
// the request and its body.
func api(t *testing.T, check func(r *http.Request, body []byte)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		check(r, body)
		io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func query(t *testing.T, base string, opt prom.Option) {
	t.Helper()
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if _, err := prom.New(base, "test", opt).QueryRange(context.Background(), `up{job="api"}`, at, at.Add(time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestAWS(t *testing.T) {
	// Credentials from the environment only: no shared files, no instance role.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_SESSION_TOKEN", "session")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	verified := false
	srv := api(t, func(r *http.Request, body []byte) {
		if err := verifySigV4(r, body, "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "eu-west-2"); err != "" {
			t.Error(err)
		}
		if !strings.Contains(string(body), "query=up%7Bjob%3D%22api%22%7D") {
			t.Errorf("the query didn't arrive whole: %s", body)
		}
		verified = true
	})
	opt, err := Auth(context.Background(), "aws", srv.URL, "eu-west-2")
	if err != nil {
		t.Fatal(err)
	}
	query(t, srv.URL, opt)
	if !verified {
		t.Fatal("no request reached the API")
	}
	if _, err := Auth(context.Background(), "aws", srv.URL, ""); err == nil || !strings.Contains(err.Error(), "--aws-region") {
		t.Errorf("no region anywhere: %v", err)
	}
	for url, want := range map[string]string{
		"https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1":       "us-east-1",
		"https://aps-workspaces-fips.us-gov-west-1.amazonaws.com/workspaces/x": "us-gov-west-1",
		"https://aps-workspaces.cn-north-1.amazonaws.com.cn/workspaces/x":      "cn-north-1",
		"https://prometheus.example.com":                                       "",
	} {
		if got := awsRegionOf(url); got != want {
			t.Errorf("awsRegionOf(%s) = %q, want %q", url, got, want)
		}
	}
}

// verifySigV4 checks a request's signature the way AWS would, written out
// apart from the SDK that made it.
func verifySigV4(r *http.Request, body []byte, secret, region string) string {
	auth := r.Header.Get("Authorization")
	fields := map[string]string{}
	for _, f := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ", ") {
		k, v, _ := strings.Cut(f, "=")
		fields[k] = v
	}
	scope := strings.SplitN(fields["Credential"], "/", 2)
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || len(scope) != 2 || scope[0] != "AKIDEXAMPLE" {
		return "Authorization " + auth
	}
	date := r.Header.Get("X-Amz-Date")
	if want := date[:8] + "/" + region + "/aps/aws4_request"; scope[1] != want {
		return "scope " + scope[1] + ", want " + want
	}
	if r.Header.Get("X-Amz-Security-Token") != "session" {
		return "no session token"
	}
	signed := strings.Split(fields["SignedHeaders"], ";")
	for _, h := range []string{"host", "x-amz-date", "x-amz-security-token"} {
		if !slices.Contains(signed, h) {
			return "unsigned " + h
		}
	}
	var canon strings.Builder
	for _, h := range signed {
		v := r.Header.Get(h)
		switch h {
		case "host":
			v = r.Host
		case "content-length":
			v = strconv.FormatInt(r.ContentLength, 10)
		}
		canon.WriteString(h + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	payload := sha256.Sum256(body)
	request := strings.Join([]string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, canon.String(), fields["SignedHeaders"], hex.EncodeToString(payload[:])}, "\n")
	hashed := sha256.Sum256([]byte(request))
	toSign := "AWS4-HMAC-SHA256\n" + date + "\n" + scope[1] + "\n" + hex.EncodeToString(hashed[:])
	mac := func(key []byte, s string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(s))
		return h.Sum(nil)
	}
	key := mac(mac(mac(mac([]byte("AWS4"+secret), date[:8]), region), "aps"), "aws4_request")
	if want := hex.EncodeToString(mac(key, toSign)); fields["Signature"] != want {
		return "signature " + fields["Signature"] + ", want " + want
	}
	return ""
}

type fakeAzure struct{ scopes []string }

func (f *fakeAzure) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.scopes = o.Scopes
	return azcore.AccessToken{Token: "azure-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestAzure(t *testing.T) {
	cred := &fakeAzure{}
	srv := api(t, func(r *http.Request, _ []byte) {
		if got := r.Header.Get("Authorization"); got != "Bearer azure-token" {
			t.Errorf("Authorization %q", got)
		}
	})
	query(t, srv.URL, azureAuth(cred))
	if !slices.Equal(cred.scopes, []string{"https://prometheus.monitor.azure.com/.default"}) {
		t.Errorf("scopes %v", cred.scopes)
	}
}

func TestGoogle(t *testing.T) {
	// A service account whose token endpoint is a test server.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant %q", r.Form.Get("grant_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"google-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokens.Close()
	sa, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "lapilli@example.iam.gserviceaccount.com",
		"private_key_id": "1", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri": tokens.URL,
	})
	file := filepath.Join(t.TempDir(), "sa.json")
	os.WriteFile(file, sa, 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
	srv := api(t, func(r *http.Request, _ []byte) {
		if got := r.Header.Get("Authorization"); got != "Bearer google-token" {
			t.Errorf("Authorization %q", got)
		}
	})
	opt, err := Auth(context.Background(), "google", srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	query(t, srv.URL, opt)
}

func TestUnknown(t *testing.T) {
	if _, err := Auth(context.Background(), "oracle", "http://x", ""); err == nil {
		t.Error("an unknown cloud was taken")
	}
}
