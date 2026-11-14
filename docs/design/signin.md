# Signing in: lapilli serve at a URL people open

Status: built, for v0.30.0, after two reviews (an attacker's and a
correctness one). How lapilli serve says who is asking without a proxy of
plain headers in front, and why it is built this way.

## Why

Until 0.30.0 lapilli serve had no sign-in of its own. Roles needed
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
and list their groups; they feed `--viewers`, `--editors`, `--admins`,
`--approvers` and the roles given in the UI exactly as `--user-header` and
`--groups-header` do.

A name, from a header or a claim, is no one's when it is over 200 bytes,
one of lapilli's own (`incidents-token` and the like), has spaces around
it, or holds spacing other than the ASCII space (U+00A0, U+3000 and the
other separators), a format character (a zero-width space, a soft hyphen,
a direction mark) or a control character: each could read as another's,
an admin's, in a comment or an approval. A group like that is dropped,
from a token, a signed header or `--groups-header` (before 0.34.1, from a
token only for its spacing, and from `--groups-header` never).

## Who comes in

An identity provider signs in whoever it knows. For a company's own
directory that is the company; for a public one it is anyone: Google's
for any Google account (an OAuth client whose consent screen is
External), GitLab.com, GitHub through Dex without an organization, a
Microsoft Entra ID app that is multi-tenant, a pool people sign up to.

So with `--oidc-issuer` or `--proxy-jwt`, only those a role names come in
at all: `--viewers` (who may look), `--editors`, `--admins`, `--approvers`,
or a role given in the UI. Everyone else is signed in, and gets 403, with
a way to sign out. A sign-in with no role list is refused at start.

- Every list takes people (`ana@example.com`, `user:NAME`), groups
  (`group:NAME`), and `*@DOMAIN`, everyone whose name ends in `@DOMAIN` in
  any case of its ASCII letters, as DNS compares names (Unicode's folding
  would take the Kelvin sign for a `k`, a long s for an `s`): with
  `--user-claim email`, which the provider must say is verified, that is
  everyone of a domain. `--oidc-hosted-domain` compares `hd` so too.
- `*` in `--viewers` is everyone the provider signs in, said in so many
  words. In `--editors`, `--admins` or `--approvers` it is refused until
  `--viewers` names who comes in, and is then everyone `--viewers` lets
  in, never everyone signed in.
- `--oidc-hosted-domain` checks Google's `hd` claim, the Workspace domain
  an account is in, and asks Google to offer only that domain's accounts.
- `--viewers` with `--user-header` lets only those it names in, too.

There is no anonymous viewing with a sign-in of lapilli's own, in 0.30.0:
the ledger's rule names, incidents and comments are a company's.

## OpenID Connect

The authorization code flow, as a confidential client with
`--oidc-client-secret-file` (read for each sign-in, so it can be rotated)
or a public one without it, always with PKCE (S256).

1. A browser asking for a page, not signed in, gets 303 to
   `--external-url`'s `/auth/login?next=PAGE`. The absolute URL takes a
   browser that came by another name (a load balancer's address) to the
   name the provider sends it back to, where its cookies will be. An API
   call, MCP, or a request with an `Authorization` header gets 401.
2. `/auth/login` makes a state, a nonce and a PKCE verifier, 32 random
   bytes each, keeps them with `next` in a signed cookie named by the
   state (`lapilli-login-HASH`, ten minutes, HttpOnly, SameSite=Lax), so
   two tabs signing in at once each come back to their own, and sends the
   browser to the provider's authorization endpoint. The sign-ins under way
   the browser holds already are taken away but the newest three (one that
   doesn't verify, or has expired, too): a page elsewhere can send a
   browser to `/auth/login` again and again, and each would leave a cookie
   for ten minutes, until the browser, past its 180 cookies a site, drops
   the least recently used, the session's among them.
3. `/auth/callback` takes the state's cookie away and refuses the request
   (400) unless the cookie verifies, hasn't expired, holds that state
   (compared in constant time), and the state wasn't used before: states
   come back once, each kept in memory until its cookie would have
   expired, 10,000 at most.

   It then exchanges the code, with the verifier, at the token endpoint,
   and refuses (403) unless the ID token verifies:
   - its signature, with a key from the provider's JWKS, by an algorithm
     of a key pair (RS, PS, ES, EdDSA): `none` and HMAC never;
   - `iss`, `aud` containing the client ID, `exp`, `nbf`, and `iat` not in
     the future;
   - `azp`: when present, this client's or one `--oidc-trusted-clients`
     names (OpenID Connect Core 3.1.3.7); when the token names several
     audiences, present;
   - `hd`, with `--oidc-hosted-domain`;
   - the nonce the cookie holds;
   - with `--user-claim email`, `email_verified` true. A provider that
     lets people type an address unchecked would otherwise let them take
     another's, an admin's.

   The provider's own `error` is shown by its code alone (`access_denied`),
   never its description, which whoever made the link wrote.
4. The browser gets a session cookie and goes back to `next`, which must
   be a path on this server (`//host`, `/\host`, a scheme, a control
   character all become `/`); the login checks it, and the callback
   again.

People see refusals in their own language, in general words; what went
wrong (which claim, which check) is in the log, for whoever runs lapilli.

Lapilli reads the provider's configuration itself, at start and when first
needed, and after a failure at most every ten seconds: its `issuer` must
be `--oidc-issuer` to the byte (a slash more, or Entra ID's `/common`, is
a setting to put right), and the JWKS, authorization and token endpoints
it names must be https, or http to this machine or a name inside a
cluster. A provider that answers with a client's error (404), a document
that isn't one, or an issuer that differs, is an ERROR in the log and a
page saying signing in isn't set up; one that doesn't answer, or answers
with a server's error, is an outage, said as one (503).

### Sessions

The session is a cookie, signed with HMAC-SHA256: the session's id
(16 random bytes), the name, the groups, when it was made (in seconds and
milliseconds) and when it expires (`--session-max-age`, 12h). Keys for
the session cookie and the sign-in's cookies are derived apart from the
one key, and the MAC covers the cookie's name, so one can't stand for
another. A cookie made when sessions lasted longer is refused once older
than they last now.

- **The key.** Whoever holds it can sign in as anyone, an approver among
  them, so it is never kept in the ledger: `--session-key-file`, 32
  characters or more, or else one made at random for the process, and
  sessions end when it restarts. The chart makes a Secret for it and keeps
  it, or takes one the user names. A new key signs everyone out.
- **Secure cookies.** With an https `--external-url`, the cookies are
  `Secure` and named `__Host-lapilli-session` and
  `__Host-lapilli-login-HASH`, which a browser keeps from any other host
  or path. `--oidc-issuer` refuses an http `--external-url` but to this
  machine (localhost, a name under `.localhost`, a loopback address). A
  cookie isn't kept to a port: one set at `http://localhost:8080` goes to
  every other server on `localhost` the browser visits. A name of its own
  under `.localhost` (`http://lapilli.localhost:8080`) keeps it from them.
- **Signing out.** `POST /auth/logout`, from a form on every page, takes
  the cookie away and ends every session of that person made until then,
  in this browser and any other, a copy of the cookie among them, and
  every bearer token issued before. Who signed out and when (in
  milliseconds) is merged into the ledger's `meta` in one transaction,
  with a count of its changes; each request reads the count, and the
  record again only when the count moved, so two processes sharing a
  ledger don't write over each other, and one signing out signs out at
  both. It is kept until a session made then would have expired, and
  bounded by the people the provider signs in and a role lets in, not by
  how often anyone signs in and out:
  - Only someone a role lets in is kept: anyone the provider signs in
    gets a session cookie, which reaches this page, and a stranger's
    sign-outs by the thousand mustn't push an admin's out. A stranger's
    cookie, kept, lets no one in anywhere. A session signed out of already
    is signed out again (its cookie taken away) with nothing written.
  - 10,000 people at once. Past that, the oldest is let go of, and a time
    for everyone raised to when they signed out: every session and bearer
    token made then or before, anyone's, is ended, failing closed, rather
    than the one let go of taken again. Those people sign in again. With
    `--viewers '*'`, or a provider people sign up to and a broad
    `*@DOMAIN`, 10,001 accounts signing in and out within
    `--session-max-age`, again and again, sign everyone out so: a way to
    make people sign in again, at an account each, not a way in.
  - Under `--read-only` it is written too: signing out must work, and it
    changes nothing a page shows.

  It is a POST checked by the cross-origin protection every write has, and
  the cookie is SameSite=Lax: a page elsewhere can't sign people out. The
  provider's own session is left as it is (`/auth/signed-out` says so).
- **Why a cookie, not a table.** Sessions in a table would be written on
  every sign-in, and would need a schema change. A signed cookie needs
  nothing kept but who signed out, and every replica sharing a Postgres
  ledger takes it, signed with the key all of them mount
  (`--session-key-file`; [replicas.md](replicas.md)): no pod need be the one
  a person came to. A sign-in's state is used once among the replicas.
- **Size.** A browser keeps 4 KB a cookie. When the groups don't fit, only
  those roles are given by are kept (named in a role list, or given a role
  in the UI then); a role given to another group later applies from the
  next sign-in.

### API clients and agents

An API client or an agent (MCP) sends an ID token the provider gave this
client as a bearer token, as Kubernetes takes them: verified as above but
for the nonce, which only the sign-in that asked for it knows, and
refused when issued before its person signed out, or longer ago than
`--session-max-age`, as a session is, whatever its own `exp` says: who
signed out is kept that long, and a provider's ID token can outlast it
(Dex gives 24h). A client gets another. It may do what its role
allows, but never approve a change, which is a person's, in a browser.
`--oidc-bearer=false` takes none.

A token refused gets `WWW-Authenticate: Bearer error="invalid_token",
error_description="..."` (RFC 6750), the description one of a few fixed
sentences ("the token has expired"), never what the token held. Every 401
names `/.well-known/oauth-protected-resource` (RFC 9728), which says the
provider to get a token from.

The token must be an ID token whose audience is this client:

- a program a person runs signs in with this client's own flow: a local
  redirect (as `kubectl oidc-login` does, with its redirect URI, such as
  `http://localhost:8000`, registered beside lapilli's), or the device
  authorization grant where the provider offers it for the client
  (Okta, Microsoft Entra ID, Keycloak, Dex);
- a service account on Google Cloud gets one with `gcloud auth
  print-identity-token --audiences=CLIENT_ID` (as the account itself, or
  `--impersonate-service-account`);
- a client credentials grant gives an access token, not an ID token: it
  isn't taken.

### What providers send

| provider | name | groups |
|---|---|---|
| Google | `email` (with `email_verified`); `hd` for Workspace | none in the ID token |
| Microsoft Entra ID | no `email_verified`: take `preferred_username` (the sign-in name) or `oid` (the object id) | `groups`, object ids; past 200, a link (`_claim_names`) in their place: lapilli takes none and logs a warning. Send only the application's groups (`groupMembershipClaims: ApplicationGroup`) |
| Okta | `email` | `groups`, with a groups claim on the authorization server, and the `groups` scope |
| Keycloak | `email` | `groups` with the group mapper, as full paths (`/team/ops`) unless "Full group path" is off |
| GitLab | `email` | `groups_direct` (`--groups-claim groups_direct`); `groups` is not in the ID token |
| Dex | `email` | `groups`, with the `groups` scope; GitHub's as `org:team` |
| AWS Cognito | `email` | `cognito:groups` |
| OCI IAM identity domains | `email` or `sub` | `groups` when the application is set to send them |
| An AWS ALB's sign-in (`--proxy-jwt aws-alb`) | the provider's userinfo: ask for `openid email` in `auth-scope` | as the provider's userinfo has them |
| Google's IAP (`--proxy-jwt google-iap`) | `email` | none |

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
  token's `kid` from the host of the ARN's partition and region alone
  (`public-keys.auth.elb.REGION.amazonaws.com`, `.amazonaws.com.cn`, or
  GovCloud's bucket); a `kid` that isn't letters, digits and dashes is
  refused before anything is fetched. It is checked by hand: the load
  balancer pads the JWT's parts with `=`, which JWT libraries refuse, and
  signs them as sent.
- `jwks`: any other, by `--proxy-jwt-header`, `--proxy-jwt-jwks`,
  `--proxy-jwt-issuer` and `--proxy-jwt-audience`: oauth2-proxy's ID token
  (`Authorization`, with `--pass-authorization-header`), an Azure App
  Service's sign-in (`X-MS-TOKEN-AAD-ID-TOKEN`), an access proxy's.

The header named twice is refused: one may be the caller's. A name from
the `email` claim beside `email_verified: false` is refused for every
kind; one that doesn't say (IAP's) is taken, as the proxy decided who may
come in. A JWT in `Authorization` is a bearer token: any program that
holds one sends it as easily as the proxy, so it never approves.

A program let through IAP or an ALB (a service account's token, a
script with a session) comes with the same signed header as a person's
browser, and lapilli can't tell them apart: keep programs out of
`--approvers` and `--admins`.

## Keys, fetched sparingly

Whatever an unsigned-in request can make lapilli do, it does at most so
often:

- keys come only from the URLs given, or, for a load balancer, the hosts
  its ARN names, never from a URL a token names (`jku`, `x5u` are
  ignored);
- a token naming a key not held fetches the keys again at most once a
  minute, and the keys held are fetched again after six hours, in the
  background, so a key the provider took away stops being taken and no
  request whose key is held waits;
- a load balancer's keys not held come from a budget of their own: six at
  once, one more each ten seconds; a key id the load balancer doesn't have
  is remembered for an hour and costs nothing again. Made-up keys sent
  slower than the budget fills can't keep the load balancer's own new key
  from being fetched; faster, they can delay it, which a NetworkPolicy
  letting only the load balancer in stops;
- one fetch at a time, ten seconds at most, no redirects, 1 MB of keys (16
  KB for a load balancer's), 100 keys; a fetch that fails keeps the keys
  held.

## Roles, and listening on loopback

0.28.1 made everyone an admin, with no roles, only on a listener on
loopback alone. With a sign-in of its own, loopback grants nothing: roles
come from the lists and the roles given in the UI alone. Signing people in
means they come from elsewhere, usually through something on the same
machine (an ingress's or a mesh's proxy, a tunnel) that makes loopback
anyone's. `--everyone-admin` is refused with a sign-in. `--user-header`
keeps the 0.28.1 rule.

## Readiness

`/readyz` is ready once the ledger has a run recorded: at once after a
restart, after the backfill's first day on a first install. Before 0.30.0
it waited for this process's first round of jobs, which a slow Prometheus
stretches to `--timeout`, keeping the UI, and the sign-in's callback, out
of the load balancer.

## The chart

`serve.service` puts the UI on a Service of its own, `FULLNAME-ui` (a
LoadBalancer or a NodePort, with the cloud's annotations, class, source
ranges and traffic policy, on `serve.service.port`); `FULLNAME` stays a
ClusterIP with every port, as jobs and Prometheus reach it.
`serve.ingress` and `serve.httpRoute` (Gateway API) serve the UI at a
host; `--external-url` (https unless their `https` says not) and
`--allowed-hosts` are worked out from it. Lapilli is served at the root of
its host: a path is refused.

- Serving the UI outside the cluster with no sign-in is refused unless
  `serve.exposeWithoutSignIn`.
- `serve.userHeader` with a LoadBalancer or a NodePort is refused: nothing
  in front of a load balancer of TCP sets or strips a header. Only with
  `loadBalancerSourceRanges` naming the proxy's addresses alone and
  `serve.service.sourceRangesAreTheProxy`.
- With `serve.signIn.oidc`, an http URL is refused but to this machine:
  end TLS at an Ingress or a Gateway, or at a load balancer that does (an
  AWS NLB's or an OCI load balancer's TLS listener); GKE's and AKS's
  load balancers of TCP don't.
- The session key is a Secret the chart makes and keeps across upgrades
  (it reads the old one back with `lookup`), or one the user names: a
  GitOps tool that renders without the cluster would make a new one each
  time.
- Upgrades with `--reuse-values` keep the old release's values: the chart
  fills in the keys added since.

## Dependencies

- `github.com/coreos/go-oidc/v3` v3.21.0 (Apache-2.0): ID token
  verification, with lapilli's own key set in place of its remote one,
  which fetches on every unknown key, and lapilli's own discovery, which
  says a wrong setting apart from an outage. It is what Dex, Argo CD and
  oauth2-proxy use.
- `github.com/go-jose/go-jose/v4` v4.1.6 (Apache-2.0), go-oidc's: v4.1.7 to
  v4.1.9 were under two weeks old, and govulncheck finds nothing in v4.1.6.
- `golang.org/x/oauth2`, already a dependency: the code exchange and PKCE.

Also considered: `github.com/zitadel/oidc` (a whole provider and relying
party, more than this needs); `golang-jwt/jwt` (already indirect, but no
discovery or key sets); writing the JWS checks by hand (only for the load
balancer's padded tokens, which no library takes).

## What an attacker reviews

The code paths that read what comes from outside before anyone is known:

- `internal/serve/signin.go`: `signedIn` (every request: who, then whether
  a role lets them in), `identify`, `notSignedIn`, `login`, `callback`
  (state, replay, error, open redirect), `logout`, `signedOut`.
- `internal/serve/roles.go`: `letIn`, `listed` (`*@DOMAIN`), `CheckRoles`.
- `internal/signin/session.go`: the cookies' signing, `Read` and `Take`.
- `internal/signin/oidc.go`: `discover`, `Finish` (exchange, nonce),
  `Bearer`, `verify` (azp, hd, iat) and `Claims.Identity` (`claims.go`:
  email_verified, `Plain`, groups, overage).
- `internal/signin/keys.go`: `Keys` (what a token can make lapilli fetch),
  `SafeURL`.
- `internal/signin/proxy.go`: `Identify`, `albClaims` (by hand),
  `albKeyURL`, `albKeys`, the IAP and JWKS verifiers.
- `internal/serve/changesweb.go`: `person` refusing bearer tokens.
- `cmd/lapilli/serve.go`: which combinations of flags are refused.

## Not in this

- Signing out at the provider too (RP-initiated logout): a form's POST
  can't be redirected there under the pages' `form-action 'self'`.
- Refresh tokens: a session ends after `--session-max-age`, and the browser
  signs in again, usually without a word if the provider still knows it.
- Several providers at once; Microsoft Entra ID's group overage read
  through Microsoft Graph.
- Anonymous viewers beside people signed in.
