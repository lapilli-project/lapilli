package signin

// Fuzz targets for what an identity provider, a JWKS endpoint or anyone
// sending a header gives the sign-in.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// fixedBody answers every request with one body, without a network.
type fixedBody []byte

func (b fixedBody) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(b)), Request: r}, nil
}

// A JWKS endpoint's answer, whatever it holds: read without a panic, and
// never a shared secret among the keys.
func FuzzJWKS(f *testing.F) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k", Algorithm: "ES256", Use: "sig"}}})
	f.Add(good)
	f.Add([]byte(`{"keys":[{"kty":"oct","k":"c2VjcmV0"},{"kty":"RSA","n":"AQAB","e":"AQAB"},{"kty":"OKP","crv":"Ed25519","x":"AA"}]}`))
	f.Add([]byte(`{"keys":[{"kty":"EC","crv":"P-256","x":"","y":""}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		k := NewKeys("https://idp.example.com/keys", Client(fixedBody(body)), nil, func() time.Time { return t0 })
		keys, err := k.fetch(context.Background())
		if err != nil {
			return
		}
		for _, x := range keys {
			if !x.IsPublic() {
				t.Fatalf("a key that isn't public: %+v", x)
			}
		}
	})
}

// An AWS load balancer's header, as anyone reaching the pod may send it.
func FuzzALB(f *testing.F) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	var pemKey bytes.Buffer
	pem.Encode(&pemKey, &pem.Block{Type: "PUBLIC KEY", Bytes: der})
	p, err := NewProxy(Proxy{Kind: AWSALB, ALBArn: albArn, Client: Client(fixedBody(pemKey.Bytes())),
		Now: func() time.Time { return t0 }, Claims: Claims{UserClaim: "email", GroupsClaim: "groups"}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(albToken(key, "kid-1", nil, map[string]any{"email": "ana@example.com"}))
	f.Add("a.b.c")
	f.Add("=.=.=")
	f.Fuzz(func(t *testing.T, tok string) {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header["X-Amzn-Oidc-Data"] = []string{tok}
		p.Identify(r)
	})
}
