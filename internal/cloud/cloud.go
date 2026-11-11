// Package cloud gives a Prometheus client the credentials a cloud's managed
// Prometheus wants, from wherever the machine or pod already has them: AWS,
// Azure and Google Cloud alike, so that no one cloud is the way in.
package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/oauth2/google"

	"lapilli.dev/internal/prom"
)

// Kinds are the clouds whose credentials lapilli can send.
var Kinds = []string{"aws", "azure", "google"}

// Auth returns what authenticates requests to endpoint the way that cloud's
// managed Prometheus wants:
//
//   - aws: Amazon Managed Service for Prometheus, each request signed with
//     SigV4 for the service aps, or Amazon CloudWatch's PromQL API
//     (https://monitoring.REGION.amazonaws.com) for the service monitoring,
//     with the default credential chain (environment, shared config, IRSA,
//     EKS Pod Identity, instance role). The region is the endpoint's unless
//     one is given.
//   - azure: Azure Monitor managed service for Prometheus, with a token from
//     the default Azure credential (environment, workload identity, managed
//     identity, Azure CLI).
//   - google: Google Cloud Managed Service for Prometheus, with Application
//     Default Credentials (environment, workload identity, metadata server,
//     gcloud).
func Auth(ctx context.Context, kind, endpoint, region string) (prom.Option, error) {
	switch kind {
	case "aws":
		return awsAuth(ctx, endpoint, region)
	case "azure":
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("Azure credentials: %w", err)
		}
		return azureAuth(cred, endpoint), nil
	case "google":
		ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/monitoring.read")
		if err != nil {
			return nil, fmt.Errorf("Google credentials: %w", err)
		}
		return prom.WithToken(func(context.Context) (string, error) {
			t, err := ts.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		}), nil
	}
	return nil, fmt.Errorf("no credentials for %q: want one of %v", kind, Kinds)
}

// awsHost finds the service and the region in an Amazon Managed Service
// for Prometheus URL (https://aps-workspaces.REGION.amazonaws.com/workspaces/ws-...)
// or a CloudWatch one (https://monitoring.REGION.amazonaws.com), dual-stack
// (REGION.api.aws) or through a VPC endpoint (vpce-...SERVICE.REGION.vpce.amazonaws.com).
var awsHost = regexp.MustCompile(`(?:^|\.)(aps-workspaces|aps|monitoring)(?:-fips)?\.([a-z0-9-]+)\.(?:vpce\.)?(?:amazonaws\.com(?:\.cn)?|api\.aws)$`)

// awsService is the service an endpoint's requests are signed for:
// monitoring for CloudWatch, aps for anything else.
func awsService(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		if m := awsHost.FindStringSubmatch(u.Hostname()); m != nil && m[1] == "monitoring" {
			return "monitoring"
		}
	}
	return "aps"
}

// awsRegionOf is the region in an endpoint's host name, if it has one.
func awsRegionOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	if m := awsHost.FindStringSubmatch(u.Hostname()); m != nil {
		return m[2]
	}
	return ""
}

func awsAuth(ctx context.Context, endpoint, region string) (prom.Option, error) {
	if region == "" {
		region = awsRegionOf(endpoint)
	}
	if region == "" {
		return nil, errors.New("--auth aws: the URL doesn't say which region; give --aws-region")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("AWS credentials: %w", err)
	}
	return awsSigner(cfg.Credentials, awsService(endpoint), region, time.Now), nil
}

func awsSigner(creds aws.CredentialsProvider, service, region string, now func() time.Time) prom.Option {
	return prom.WithSigner(SignRequest(creds, service, region, now))
}

func azureAuth(cred azcore.TokenCredential, endpoint string) prom.Option {
	return prom.WithToken(azureToken(cred, endpoint))
}

func azureToken(cred azcore.TokenCredential, endpoint string) func(context.Context) (string, error) {
	scope := policy.TokenRequestOptions{Scopes: []string{AzureScope(endpoint)}}
	return func(ctx context.Context) (string, error) {
		t, err := cred.GetToken(ctx, scope)
		if err != nil {
			return "", fmt.Errorf("Azure credentials: %w", err)
		}
		return t.Token, nil
	}
}

// AzureHosts are Azure Resource Manager's, in each of Azure's clouds: where
// a Prometheus rule group is read and written.
var AzureHosts = []string{"management.azure.com", "management.usgovcloudapi.net", "management.chinacloudapi.cn"}

// AzureScope is the scope of a token for an endpoint, told by its host:
// Resource Manager's, for a rule group, or the managed Prometheus query
// endpoint's.
func AzureScope(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		for _, h := range AzureHosts {
			if strings.EqualFold(u.Hostname(), h) {
				return "https://" + h + "/.default"
			}
		}
	}
	return "https://prometheus.monitor.azure.com/.default"
}

// AWSHost says what an Amazon host name is: the service (aps for Amazon
// Managed Service for Prometheus's control plane, aps-workspaces for its
// query URL, monitoring for CloudWatch) and the region; ok is false for a
// host that isn't one of them. It knows FIPS, dual-stack and VPC endpoint
// hosts.
func AWSHost(host string) (service, region string, ok bool) {
	m := awsHost.FindStringSubmatch(strings.ToLower(host))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Request authenticates a plain HTTP request to a cloud's API, as a ruler's
// and a cloud's alarms' requests are: kind aws signs it with SigV4 for the
// service and region in the endpoint's host (as, when given, a role to
// assume for it); azure sends a token of the scope the endpoint's host says
// (as, when given, the client ID of a user-assigned identity to send it
// as); google a token for Cloud Monitoring (as, when given, a service
// account to impersonate); oci signs it with OCI's signature (as, when
// given, an OCI config file naming a user and its API key). Each is the
// machine's or pod's own, as Auth's are.
func Request(ctx context.Context, kind, endpoint, as string) (func(*http.Request) error, error) {
	switch kind {
	case "aws":
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		service, region, ok := AWSHost(u.Hostname())
		if !ok {
			return nil, fmt.Errorf("%s isn't an Amazon endpoint", u.Hostname())
		}
		if service == "aps-workspaces" {
			service = "aps"
		}
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("AWS credentials: %w", err)
		}
		creds := cfg.Credentials
		if as != "" {
			creds = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), as,
				func(o *stscreds.AssumeRoleOptions) { o.RoleSessionName = "lapilli" }))
		}
		return SignRequest(creds, service, region, time.Now), nil
	case "azure":
		var cred azcore.TokenCredential
		var err error
		if as == "" {
			cred, err = azidentity.NewDefaultAzureCredential(nil)
		} else {
			cred, err = azureIdentity(as)
		}
		if err != nil {
			return nil, fmt.Errorf("Azure credentials: %w", err)
		}
		token := azureToken(cred, endpoint)
		return func(req *http.Request) error {
			t, err := token(req.Context())
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+t)
			return nil
		}, nil
	case "google":
		return googleRequest(ctx, as)
	case "oci":
		return ociRequest(as)
	}
	return nil, fmt.Errorf("no credentials for %q to send with a request", kind)
}

// AWSAccount is the account the machine's or pod's own AWS identity is
// in, as STS GetCallerIdentity says (which needs no permission), asked in
// the region of an Amazon endpoint.
func AWSAccount(ctx context.Context, endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	_, region, ok := AWSHost(u.Hostname())
	if !ok {
		return "", fmt.Errorf("%s isn't an Amazon endpoint", u.Hostname())
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return "", fmt.Errorf("AWS credentials: %w", err)
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Account), nil
}

// azureIdentity is a user-assigned identity, by its client ID: federated
// with the pod's service account (workload identity), or a VM's or a
// node's managed identity.
func azureIdentity(clientID string) (azcore.TokenCredential, error) {
	var chain []azcore.TokenCredential
	if w, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{ClientID: clientID}); err == nil {
		chain = append(chain, w)
	}
	m, err := azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{ID: azidentity.ClientID(clientID)})
	if err != nil {
		return nil, err
	}
	chain = append(chain, m)
	return azidentity.NewChainedTokenCredential(chain, nil)
}

// SignRequest signs a plain request with SigV4, its body hashed as sent.
func SignRequest(creds aws.CredentialsProvider, service, region string, now func() time.Time) func(*http.Request) error {
	signer := v4.NewSigner()
	return func(req *http.Request) error {
		c, err := creds.Retrieve(req.Context())
		if err != nil {
			return fmt.Errorf("AWS credentials: %w", err)
		}
		h := sha256.New()
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return err
			}
			defer body.Close()
			if _, err := io.Copy(h, body); err != nil {
				return err
			}
		}
		return signer.SignHTTP(req.Context(), c, req, hex.EncodeToString(h.Sum(nil)), service, region, now())
	}
}
