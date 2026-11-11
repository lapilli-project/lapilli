package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// The credentials a cloud's own alarms are read and written with
// (docs/design/alarms.md): Google Cloud's, impersonating a service account
// for writes, and OCI's, which sign each request. Amazon's and Azure's are
// Request's, as a ruler's are.

// googleMonitoring is the scope a token for Cloud Monitoring's alerting
// policies is asked for: monitoring.read to read, monitoring to write.
const (
	googleMonitoringRead = "https://www.googleapis.com/auth/monitoring.read"
	googleMonitoring     = "https://www.googleapis.com/auth/monitoring"
)

// IAMCredentials is where a service account's token is asked for; tests
// replace it.
var IAMCredentials = "https://iamcredentials.googleapis.com"

// googleSource is Application Default Credentials' token source, of a
// scope; tests replace it.
var googleSource = func(ctx context.Context, scope string) (oauth2.TokenSource, error) {
	return google.DefaultTokenSource(ctx, scope)
}

// googleRequest sends a token for Cloud Monitoring: the pod's own, from
// Application Default Credentials, to read; or, with as, a service
// account's, impersonated through IAM Credentials' generateAccessToken with
// the pod's identity, to write.
func googleRequest(ctx context.Context, as string) (func(*http.Request) error, error) {
	var ts oauth2.TokenSource
	var err error
	if as == "" {
		if ts, err = googleSource(ctx, googleMonitoringRead); err != nil {
			return nil, fmt.Errorf("Google credentials: %w", err)
		}
	} else {
		base, err := googleSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, fmt.Errorf("Google credentials: %w", err)
		}
		ts = oauth2.ReuseTokenSource(nil, &impersonated{ctx: ctx, base: base, email: as, scope: googleMonitoring})
	}
	return func(req *http.Request) error {
		t, err := ts.Token()
		if err != nil {
			return fmt.Errorf("Google credentials: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+t.AccessToken)
		return nil
	}, nil
}

// impersonated is a service account's token, asked of IAM Credentials with
// the pod's: the pod holds roles/iam.serviceAccountTokenCreator on that one
// account, and the account what writing alerting policies needs.
type impersonated struct {
	ctx   context.Context
	base  oauth2.TokenSource
	email string
	scope string
}

var iamClient = &http.Client{Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (i *impersonated) Token() (*oauth2.Token, error) {
	t, err := i.base.Token()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"scope": []string{i.scope}, "lifetime": "3600s"})
	u := strings.TrimRight(IAMCredentials, "/") + "/v1/projects/-/serviceAccounts/" + url.PathEscape(i.email) + ":generateAccessToken"
	req, err := http.NewRequestWithContext(i.ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	resp, err := iamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("impersonating %s: %w", i.email, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("impersonating %s: %w", i.email, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("impersonating %s: IAM Credentials answered HTTP %d: %s", i.email, resp.StatusCode, msg)
	}
	var out struct {
		AccessToken string    `json:"accessToken"`
		ExpireTime  time.Time `json:"expireTime"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.AccessToken == "" {
		return nil, fmt.Errorf("impersonating %s: IAM Credentials gave no token", i.email)
	}
	return &oauth2.Token{AccessToken: out.AccessToken, TokenType: "Bearer", Expiry: out.ExpireTime}, nil
}

// ociProvider is the pod's or the machine's own OCI identity, in this
// order: OKE workload identity, a resource principal (both told by the
// environment OCI sets), an OCI config file where there is one, and an
// instance principal, which asks the instance's metadata service; tests
// replace it.
var ociProvider = func() (common.ConfigurationProvider, error) {
	if os.Getenv("OCI_RESOURCE_PRINCIPAL_VERSION") != "" {
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			if p, err := auth.OkeWorkloadIdentityConfigurationProvider(); err == nil {
				return p, nil
			}
		}
		if p, err := auth.ResourcePrincipalConfigurationProvider(); err == nil {
			return p, nil
		}
	}
	file := os.Getenv("OCI_CLI_CONFIG_FILE")
	if file == "" {
		if home, err := os.UserHomeDir(); err == nil {
			file = filepath.Join(home, ".oci", "config")
		}
	}
	if _, err := os.Stat(file); file != "" && err == nil {
		return common.ConfigurationProviderFromFile(file, "")
	}
	return auth.InstancePrincipalConfigurationProvider()
}

// ociRequest signs each request with OCI's HTTP signature: as the pod's or
// the machine's own identity, or, with as, the user an OCI config file
// names, with the API key it names.
func ociRequest(as string) (func(*http.Request) error, error) {
	var p common.ConfigurationProvider
	var err error
	if as == "" {
		p, err = ociProvider()
	} else {
		p, err = common.ConfigurationProviderFromFile(as, "")
	}
	if err != nil {
		return nil, fmt.Errorf("OCI credentials: %w", err)
	}
	if ok, err := common.IsConfigurationProviderValid(p); !ok {
		if err == nil {
			err = errors.New("incomplete")
		}
		return nil, fmt.Errorf("OCI credentials: %w", err)
	}
	return OCISigner(p), nil
}

// OCISigner signs a request as OCI's SDK does: date, (request-target) and
// host, and for a body its length, type and SHA-256.
func OCISigner(p common.KeyProvider) func(*http.Request) error {
	signer := common.DefaultRequestSigner(p)
	return func(req *http.Request) error {
		if req.Header.Get("Date") == "" {
			req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
		}
		if err := signer.Sign(req); err != nil {
			return fmt.Errorf("OCI credentials: %w", err)
		}
		return nil
	}
}
