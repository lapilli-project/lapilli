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
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
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
		return azureAuth(cred), nil
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
	signer := v4.NewSigner()
	return prom.WithSigner(func(req *http.Request) error {
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
	})
}

func azureAuth(cred azcore.TokenCredential) prom.Option {
	scope := policy.TokenRequestOptions{Scopes: []string{"https://prometheus.monitor.azure.com/.default"}}
	return prom.WithToken(func(ctx context.Context) (string, error) {
		t, err := cred.GetToken(ctx, scope)
		if err != nil {
			return "", fmt.Errorf("Azure credentials: %w", err)
		}
		return t.Token, nil
	})
}
