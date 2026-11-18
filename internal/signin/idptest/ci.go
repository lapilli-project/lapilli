package idptest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// CI is a CI provider's issuer of ID tokens for tests, as GitHub Actions,
// GitLab and Forgejo serve theirs: a discovery document with only issuer
// and jwks_uri (GitHub's has no authorization or token endpoint), under a
// path (Forgejo's /api/actions), and its keys.
type CI struct {
	*httptest.Server
	// Issuer is the server's URL and the path under it.
	Issuer string
	Key    *rsa.PrivateKey
	KID    string
	Now    func() time.Time
	// JWKS, when set, is the jwks_uri the discovery document names, in
	// place of the issuer's own.
	JWKS string
	// Fetches count the requests for the discovery document and for keys.
	Discoveries, KeyFetches atomic.Int64
}

// NewCI starts an issuer at path under a local port ("" for its root), with
// an RSA key of 2048 bits; it stops with the test.
func NewCI(t testing.TB, path string, now func() time.Time) *CI {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := &CI{Key: key, KID: "ci1", Now: now}
	mux := http.NewServeMux()
	path = strings.TrimSuffix(path, "/")
	mux.HandleFunc("GET "+path+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		c.Discoveries.Add(1)
		jwks := c.JWKS
		if jwks == "" {
			jwks = c.Issuer + "/.well-known/jwks"
		}
		json.NewEncoder(w).Encode(map[string]any{"issuer": c.Issuer, "jwks_uri": jwks,
			"subject_types_supported": []string{"public"}, "response_types_supported": []string{"id_token"},
			"id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET "+path+"/.well-known/jwks", func(w http.ResponseWriter, r *http.Request) {
		c.KeyFetches.Add(1)
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &c.Key.PublicKey, KeyID: c.KID, Algorithm: "RS256", Use: "sig"}}})
	})
	c.Server = httptest.NewServer(mux)
	c.Issuer = c.URL + path
	t.Cleanup(c.Close)
	return c
}

// Token is an ID token of the issuer's for audience: iss, aud, iat, nbf and
// exp as a fresh one's (exp in ten minutes), then claims, which can
// replace any of them; a claim of nil is left out.
func (c *CI) Token(audience string, claims map[string]any) string {
	now := c.Now()
	m := map[string]any{"iss": c.Issuer, "aud": audience, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(), "jti": rand.Text()}
	for k, v := range claims {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return Sign(c.Key, c.KID, jose.RS256, m)
}

// GitHub are the claims of a GitHub Actions job's token for a pull request,
// as GitHub documents them: the repository, its owner and the event, by
// name and by id.
func GitHub(repoID, prNumber string) map[string]any {
	return map[string]any{
		"sub": "repo:example/alerts:pull_request", "ref": "refs/pull/" + prNumber + "/merge", "ref_type": "branch",
		"repository": "example/alerts", "repository_id": repoID, "repository_owner": "example", "repository_owner_id": "4200",
		"repository_visibility": "private", "run_id": "1234567890", "run_number": "7", "run_attempt": "1",
		"actor": "someone", "actor_id": "77", "workflow": "backtest", "head_ref": "raise-threshold", "base_ref": "main",
		"event_name": "pull_request", "runner_environment": "github-hosted",
		"workflow_ref":     "example/alerts/.github/workflows/backtest.yml@refs/pull/" + prNumber + "/merge",
		"job_workflow_ref": "example/alerts/.github/workflows/backtest.yml@refs/pull/" + prNumber + "/merge",
	}
}

// GitLab are the claims of a GitLab CI job's token in a merge request
// pipeline, as GitLab documents them: ids written as strings, the project's
// path, the pipeline's source.
func GitLab(projectID string) map[string]any {
	return map[string]any{
		"sub": "project_path:monitoring/alerts:ref_type:branch:ref:raise-threshold", "namespace_id": "72", "namespace_path": "monitoring",
		"project_id": projectID, "project_path": "monitoring/alerts", "user_id": "1", "user_login": "someone", "user_email": "someone@example.com",
		"user_access_level": "developer", "pipeline_id": "1212", "pipeline_source": "merge_request_event", "job_id": "1213",
		"ref": "raise-threshold", "ref_type": "branch", "ref_path": "refs/heads/raise-threshold", "ref_protected": "false",
		"runner_id": 1, "runner_environment": "gitlab-hosted", "sha": "0123456789abcdef0123456789abcdef01234567",
		"project_visibility": "private", "ci_config_ref_uri": "gitlab.example.com/monitoring/alerts//.gitlab-ci.yml@refs/heads/raise-threshold",
		"job_project_id": projectID, "job_project_path": "monitoring/alerts",
	}
}

// Forgejo are the claims of a Forgejo Actions job's token for a pull
// request: GitHub's names, the repository's id as a number, as Forgejo
// writes it.
func Forgejo(repoID int64) map[string]any {
	return map[string]any{
		"sub": "repo:monitoring/alerts:pull_request", "repository": "monitoring/alerts", "repository_id": repoID,
		"repository_owner": "monitoring", "repository_owner_id": int64(3), "event_name": "pull_request",
		"ref": "refs/pull/9/head", "ref_type": "branch", "run_id": "55", "run_number": "4", "actor": "someone",
		"workflow": "backtest", "head_ref": "raise-threshold", "base_ref": "main",
	}
}
