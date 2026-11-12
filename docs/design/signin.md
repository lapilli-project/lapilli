# Signing in: lapilli serve at a URL people open

Status: built, for v0.29.0. How lapilli serve says who is asking without a
proxy of plain headers in front, and why it is built this way.

## Why

Until 0.29.0 lapilli serve had no sign-in of its own. Roles needed
`--user-header` from a proxy that signs people in, and a NetworkPolicy
that keeps everyone but that proxy from setting the header. A team that
wanted a URL to open had to add oauth2-proxy, or an ingress's sign-in,
and get both right. Grafana and Argo CD sign people in themselves, and are
put behind a load balancer as they are. Lapilli should be served the same
way.

## Three ways, one at a time

| | who says who | what lapilli checks |
|---|---|---|
| `--user-header` (as before) | a proxy, in a plain header | nothing: only the proxy may reach the pod |
| `--oidc-issuer` | an OpenID Connect provider | the ID token, then its own session cookie |
| `--proxy-jwt` | a sign-in in front, in a JWT it signs | the JWT, on every request |

Giving two is refused: each request has one answer to "who". `--user-claim`
(`email`) and `--groups-claim` (`groups`) say which claims name the person
and list their groups; they feed `--editors`, `--admins`, `--approvers` and
the roles given in the UI exactly as `--user-header` and `--groups-header`
do. A name goes through the same check as the header's: over 200 bytes,
with a character that doesn't show, or one of lapilli's own (`incidents-token`
and the like) is no one's.

With `--oidc-issuer` or `--proxy-jwt`, every page and API call needs
someone signed in, except `/healthz`, `/readyz`, `/static/`, `/lang` and
`/auth/`. There are no anonymous viewers: a URL people open is usually one
anyone on the internet can open, and the ledger's rule names, incidents and
comments are a company's. `/metrics` is on the UI's port too, so Prometheus
scrapes `--metrics-listen` (the chart does).

## OpenID Connect

The authorization code flow, as a confidential client with
`--oidc-client-secret-file` (read for each sign-in, so it can be rotated)
or a public one without it, always with PKCE (S256).

1. A browser asking for a page, not signed in, gets 303 to
   `--external-url`'s `/auth/login?next=PAGE`. The absolute URL takes a
   browser that came by another name (a load balancer's address) to the
   name the provider sends it back to, where its cookies will be. An API
   call, MCP, or a request with an `Authorization` header gets 401 with
   `WWW-Authenticate: Bearer`.
2. `/auth/login` makes a state, a nonce and a PKCE verifier, 32 random
   bytes each, keeps them with `next` in a signed cookie
   (`lapilli-login`, ten minutes, HttpOnly, SameSite=Lax), and sends the
   browser to the provider's authorization endpoint.
3. `/auth/callback` takes the cookie away and refuses the request (400)
   unless:
   - the cookie verifies and hasn't expired;
   - the `state` is the cookie's, compared in constant time; and
   - the state wasn't used before. States come back once: each is kept
     in memory until the cookie would have expired, 10,000 at most.

   It then exchanges the code, with the verifier, at the token endpoint,
   and refuses (403) unless the ID token verifies:
   - its signature, with a key from the provider's JWKS, by an algorithm
     of a key pair (RS, PS, ES, EdDSA): `none` and HMAC never;
   - `iss`, `aud` containing the client ID, `exp`, `nbf`, and `iat` not in
     the future;
   - `azp` this client's, when the token names several audiences;
   - the nonce the cookie holds;
   - with `--user-claim email`, `email_verified` true. A provider that
     lets people type an address unchecked would otherwise let them take
     another's, an admin's. Microsoft Entra ID doesn't send it: take
     `preferred_username` or `oid` there.

   The provider's own `error` is shown by its code alone (`access_denied`),
   never its description, which whoever made the link wrote.
4. The browser gets a session cookie and goes back to `next`, which must
   be a path on this server (`//host`, `/\host`, a scheme, a control
   character all become `/`).

Discovery happens when it is first needed, not at start, so serve starts
while the provider is down; after a failure it is tried again at most every
ten seconds, however many browsers come. The issuer, and the JWKS, the
authorization and the token endpoints its configuration names, must be
https, or http to this machine or to a name inside a cluster (no dots,
`.svc`, `.cluster.local`), where nothing between can change what they say.

### Sessions

The session is a cookie, signed with HMAC-SHA256: the session's id
(16 random bytes), the name, the groups, when it was made (in seconds and
milliseconds) and when it expires (`--session-max-age`, 12h). Keys for the session cookie and the
sign-in's cookie are derived apart from the one key, and the MAC covers
the cookie's name, so one can't stand for the other. A cookie made when
sessions lasted longer is refused once older than they last now.

- **The key.** From `--session-key-file`, 32 characters or more, or else
  made once at random and kept in the ledger's `meta`. Whoever can read the
  ledger can make sessions; whoever can read the ledger can read
  everything the sessions would show, too. A new key signs everyone out.
- **Secure cookies.** With an https `--external-url`, the cookies are
  `Secure` and named `__Host-lapilli-session` and `__Host-lapilli-login`,
  which a browser keeps from any other host or path. `--oidc-issuer`
  refuses an http `--external-url` but to this machine (localhost, a name
  under `.localhost`, a loopback address): a session would otherwise cross
  the network in the clear.
- **Signing out.** `POST /auth/logout`, from a form on every page, takes
  the cookie away and ends every session of that person made until then,
  in this browser and any other, a copy of the cookie among them: who
  signed out and when (in milliseconds) is kept until a session made then
  would have expired, in memory and in the ledger's `meta`, so a restart
  doesn't bring one back. Keeping the people rather than each session's id
  bounds it by the people the provider signs in, not by how often someone
  signs in and out: 10,000 of them at a time, the oldest dropped first. It
  is a POST checked by the cross-origin protection every write has, and the
  cookie is SameSite=Lax: a page elsewhere can't sign people out. The
  provider's own session is left as it is (`/auth/signed-out` says so):
  signing out there is the provider's.
- **Why a cookie, not a table.** One replica, one SQLite writer: a table of
  sessions would be written on every sign-in, and would need a schema
  change. A signed cookie needs nothing kept but the ended ids.
- **Size.** A browser keeps 4 KB a cookie. When the groups don't fit, only
  those roles are given by are kept (named in `--editors`, `--admins`,
  `--approvers`, or given a role in the UI then); a role given to another
  group later applies from the next sign-in.

### API clients and agents

An API client or an agent (MCP) sends an ID token the provider gave this
client as a bearer token, as Kubernetes takes them: verified as above but
for the nonce, which only the sign-in that asked for it knows. It may do
what its role allows, but never approve a change, which is a person's, in
a browser: the approval form refuses a request signed in by a bearer token,
whatever headers it sends. The token ports (`--incidents-listen`,
`--notify-listen`) are as they were.

## A sign-in in front, signing its header

`--proxy-jwt` takes who each request comes from from the JWT a sign-in in
front puts in a header, checked against the sign-in's keys, on every
request. Reaching the pod around the proxy gets 401, and a header copied
from a log works until it expires (minutes): a NetworkPolicy is still worth
having, but no longer what keeps people from naming themselves anyone.

- `google-iap`: Google Cloud's Identity-Aware Proxy,
  `x-goog-iap-jwt-assertion`, ES256 with the keys at
  `https://www.gstatic.com/iap/verify/public_key-jwk`, `iss`
  `https://cloud.google.com/iap`, `aud` the backend service's
  (`--proxy-jwt-audience /projects/NUMBER/global/backendServices/ID`).
- `aws-alb`: an Application Load Balancer's sign-in, `x-amzn-oidc-data`,
  ES256, `signer` the load balancer `--proxy-jwt-alb-arn` names, and, with
  `--proxy-jwt-issuer`, `iss` the provider's. Its key is fetched by the
  token's `kid` from the load balancer's region and partition
  (`public-keys.auth.elb.REGION.amazonaws.com`, `.amazonaws.com.cn`, or
  GovCloud's bucket), worked out from the ARN, never from the token; a
  `kid` that isn't letters, digits and dashes is refused before anything is
  fetched. It is checked by hand: the load balancer pads the JWT's parts
  with `=`, which JWT libraries refuse, and signs them as sent.
- `jwks`: any other, by `--proxy-jwt-header`, `--proxy-jwt-jwks`,
  `--proxy-jwt-issuer` and `--proxy-jwt-audience`: oauth2-proxy's ID token
  (`Authorization`, with `--pass-authorization-header`), an Azure App
  Service's sign-in (`X-MS-TOKEN-AAD-ID-TOKEN`), an access proxy's.

The header named twice is refused: one may be the caller's. `email_verified`
isn't asked of a proxy's JWT: the proxy decided who may come in.

## Keys, fetched sparingly

Whatever an unsigned-in request can make lapilli do, it does at most so
often:

- keys come only from the URLs given (or, for a load balancer, the ARN),
  never from a URL a token names (`jku`, `x5u` are ignored);
- a token naming a key not held fetches the keys again at most once a
  minute (a load balancer's, once every ten seconds), and the keys held
  are fetched again after six hours, so a key the provider took away stops
  being taken;
- one fetch at a time, ten seconds at most, no redirects, 1 MB of keys (16
  KB for a load balancer's), 100 keys; a fetch that fails keeps the keys
  held;
- a request whose key is held never waits for a fetch.

## Roles, and listening on loopback

0.28.1 made everyone an admin, with no roles, only on a listener on
loopback alone. With a sign-in of its own, loopback grants nothing: roles
come from `--editors`, `--admins` and the roles given in the UI alone, and
with none everyone signed in may only look. Signing people in means they
come from elsewhere, usually through something on the same machine (an
ingress's or a mesh's proxy, a tunnel) that makes loopback anyone's.
`--everyone-admin` is refused with a sign-in: `--admins '*'` says
everyone signed in, and says it in the roles. `--user-header` keeps the
0.28.1 rule.

## The chart

`serve.service` (ClusterIP, LoadBalancer or NodePort, with the cloud's
annotations, class, source ranges and traffic policy), `serve.ingress` and
`serve.httpRoute` (Gateway API) serve the UI at a URL; `--external-url` and
`--allowed-hosts` are worked out from their hosts, so the host check and
the provider's redirect hold. A LoadBalancer or NodePort serves the UI's
port alone; the metrics port and the token ports go on `FULLNAME-internal`.
Serving the UI outside the cluster with no sign-in is refused unless
`serve.exposeWithoutSignIn`. The readiness probe is `/readyz`. The README
has each cloud's load balancer, GKE, EKS, AKS and OKE alike.

## Dependencies

- `github.com/coreos/go-oidc/v3` v3.21.0 (Apache-2.0): discovery and ID
  token verification, with lapilli's own key set in place of its remote
  one, which fetches on every unknown key. It is what Dex, Argo CD and
  oauth2-proxy use.
- `github.com/go-jose/go-jose/v4` v4.1.6 (Apache-2.0), go-oidc's: v4.1.7 to
  v4.1.9 are under two weeks old, and govulncheck finds nothing in v4.1.6.
- `golang.org/x/oauth2`, already a dependency: the code exchange and PKCE.

Also considered: `github.com/zitadel/oidc` (a whole provider and relying
party, more than this needs); `golang-jwt/jwt` (already indirect, but no
discovery or key sets); writing the JWS checks by hand (only for the load
balancer's padded tokens, which no library takes).

## What an attacker reviews

The new code paths that read what comes from outside before anyone is known:

- `internal/serve/signin.go`: `signedIn` and `identify` (every request),
  `notSignedIn`, `login`, `callback` (state, replay, error, open redirect),
  `logout`.
- `internal/signin/session.go`: the cookies' signing, `Read` and `Take`.
- `internal/signin/oidc.go`: discovery, `Finish` (exchange, nonce), `Bearer`,
  `verify` (azp, iat) and `Claims.Identity` (`claims.go`: email_verified,
  groups).
- `internal/signin/keys.go`: `Keys` (what a token can make lapilli fetch),
  `SafeURL`.
- `internal/signin/proxy.go`: `Identify`, `albClaims` (by hand), `albKeys`,
  the IAP and JWKS verifiers.
- `internal/serve/changesweb.go`: `person` refusing bearer tokens.
- `cmd/lapilli/serve.go`: which combinations of flags are refused.

## Not in this

- Signing out at the provider too (RP-initiated logout): a form's POST
  can't be redirected there under the pages' `form-action 'self'`.
- Refresh tokens: a session ends after `--session-max-age`, and the browser
  signs in again, usually without a word if the provider still knows it.
- Several providers at once; Microsoft Entra ID's group overage (more than
  200 groups sends a link instead of the list).
- Anonymous viewers beside people signed in.
