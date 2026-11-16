package cloud

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/egress"
	"lapilli.dev/internal/prom"
)

// guarded sets a guard that refuses every request, and gives the hosts
// it was asked for.
func guarded(t *testing.T) func() []string {
	var mu sync.Mutex
	var hosts []string
	release := egress.Guard(func(r *http.Request) error {
		mu.Lock()
		hosts = append(hosts, r.Method+" "+r.URL.Host)
		mu.Unlock()
		return errors.New("refused by the test's guard")
	})
	t.Cleanup(release)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), hosts...)
	}
}

func asked(hosts []string, host string) bool {
	for _, h := range hosts {
		if strings.HasSuffix(h, " "+host) {
			return true
		}
	}
	return false
}

// While a guard is set, Google's token source asks through it: for the
// history (Auth) and for a cloud's alarms (Request).
func TestGoogleThroughGuard(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	sa, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "example", "private_key_id": "k1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "client_email": "lapilli@example.iam.gserviceaccount.com",
		"client_id": "1", "token_uri": "https://oauth2.example.invalid/token"})
	file := filepath.Join(t.TempDir(), "sa.json")
	os.WriteFile(file, sa, 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
	seen := guarded(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sign, err := Request(ctx, "google", "https://monitoring.googleapis.com/v3/projects/example/alertPolicies", "")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "https://monitoring.googleapis.com/v3/projects/example/alertPolicies", nil)
	if err := sign(req); err == nil || !asked(seen(), "oauth2.example.invalid") {
		t.Errorf("alarms: %v; the guard saw %v", err, seen())
	}
	opt, err := Auth(ctx, "google", "https://monitoring.googleapis.com/v1/projects/example/location/global/prometheus", "")
	if err != nil {
		t.Fatal(err)
	}
	before := len(seen())
	c := prom.New("https://monitoring.googleapis.com/v1/projects/example/location/global/prometheus", "test", opt)
	c.QueryRange(ctx, "vector(1)", time.Now(), time.Now(), time.Minute)
	if got := seen()[before:]; !asked(got, "oauth2.example.invalid") {
		t.Errorf("history: the guard saw %v", got)
	}
}

// While a guard is set, Azure's credentials ask through it: the default
// credential, and a user-assigned identity's. Azure's pipeline asks again
// after a refusal, a while apart: the first is enough.
func TestAzureThroughGuard(t *testing.T) {
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "EnvironmentCredential")
	t.Setenv("AZURE_TENANT_ID", "00000000-0000-0000-0000-000000000001")
	t.Setenv("AZURE_CLIENT_ID", "00000000-0000-0000-0000-000000000002")
	t.Setenv("AZURE_CLIENT_SECRET", "example-secret")
	t.Setenv("AZURE_AUTHORITY_HOST", "https://login.example.invalid/")
	seen := guarded(t)
	signed := func(as string) (error, []string) {
		before := len(seen())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sign, err := Request(ctx, "azure", "https://management.azure.com/subscriptions/x", as)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://management.azure.com/subscriptions/x", nil)
		err = sign(req)
		return err, seen()[before:]
	}
	if err, got := signed(""); err == nil || len(got) == 0 {
		t.Errorf("the default credential: %v; the guard saw %v", err, got)
	}
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("example.federated.token"), 0o600)
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", token)
	if err, got := signed("00000000-0000-0000-0000-000000000003"); err == nil || len(got) == 0 {
		t.Errorf("a user-assigned identity: %v; the guard saw %v", err, got)
	}
}
