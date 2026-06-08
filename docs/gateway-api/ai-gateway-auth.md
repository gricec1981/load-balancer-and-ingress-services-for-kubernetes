# AI Gateway — Authentication

`AIGatewayAuthPolicy` attaches JWT authentication to an HTTPRoute (or Gateway).
The Avi Service Engine (SE) validates the caller's token and AKO's policy
DataScripts read the **validated** claims to drive per-user token budgets,
identity counters, model entitlements, and MCP per-tool RBAC.

On the current Avi build, *how* the SE validates the token determines whether
claims are readable by DataScripts. There are two modes, selected by
`spec.authMode`.

| | `oauthBrowser` (default) | `jwtQuery` |
|---|---|---|
| Client | Browser / interactive | Machine (SDK, agent, curl) |
| Token presentation | OAuth auth-code → session cookie | `?jwt=<token>` query param |
| Unauthenticated response | `302` → `/authorize` | `401` (no redirect) |
| Claims in DataScripts | `oauth_get_claim()` | base64url-decode the query token |
| Avi objects | Pool + `AUTH_PROFILE_OAUTH` + `SSO_TYPE_OAUTH` + `oauth_vs_config` | `JWTServerProfile` + `AUTH_PROFILE_JWT` + `SSO_TYPE_JWT` + `jwt_config` |
| Main tradeoff | No machine clients | Token in the URL |

Both modes require the Gateway listener to terminate **TLS** (HTTPS).

## Why two modes (the constraint)

Avi's two JWT-validation paths are mutually exclusive on this build, and only
one of them exposes claims to a DataScript:

- **`CLIENT_OAUTH`** (used by `oauthBrowser`): the SE runs the browser
  auth-code/session-cookie flow. Validated claims are readable in a DataScript
  via `avi.http.oauth_get_claim()` — but a client-presented
  `Authorization: Bearer` is *ignored* (the SE 302-redirects to `/authorize`),
  so non-browser clients can't authenticate.
- **`SSO_TYPE_JWT`** (the resource-server path): the SE validates a bearer JWT
  (200/401, no redirect) — the machine-client-correct behavior — but it
  **strips the `Authorization` header** before any DataScript runs *and*
  `oauth_get_claim()` returns nil. So a token in the standard header is
  validated but its claims are invisible to policy.

`jwtQuery` threads this needle: configure `SSO_TYPE_JWT` with
`jwt_location = JWT_LOCATION_QUERY_PARAM`. The SE validates the token from the
`?jwt=` query parameter, and — unlike the `Authorization` header — the query
param is **not stripped**, so the DataScript reads the same (already-validated)
token and base64url-decodes its payload to recover the claims. The decode is
trustworthy precisely because the SE already verified the signature, `aud`, and
`exp`; AKO never emits the decode helper on a VS that isn't enforcing
`jwt_config` validation.

## `oauthBrowser` (default)

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIGatewayAuthPolicy
metadata:
  name: llm-auth
  namespace: inference
spec:
  # authMode: oauthBrowser            # default; may be omitted
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route
  jwt:
    issuer: https://issuer.example.com
    jwksUri: http://jwt-issuer.inference.svc.cluster.local:8080/jwks
    audiences: ["llm-api"]
```

AKO resolves `jwksUri` to SE-reachable endpoints, builds the issuer Pool +
`AUTH_PROFILE_OAUTH` + `SSO_TYPE_OAUTH`, and sets `oauth_vs_config` on the child
VS. DataScripts read claims with `oauth_get_claim()`.

## `jwtQuery` (machine clients)

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIGatewayAuthPolicy
metadata:
  name: llm-auth
  namespace: inference
spec:
  authMode: jwtQuery
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route
  jwt:
    issuer: https://issuer.example.com
    jwksUri: http://jwt-issuer.inference.svc.cluster.local:8080/jwks
    audiences: ["llm-api"]
```

AKO (which, unlike the SE, has cluster network access) fetches the JWKS from
`jwksUri` and embeds it in a `JWTServerProfile`, then builds `AUTH_PROFILE_JWT`
+ `SSO_TYPE_JWT` and sets the VS `jwt_config`
(`audience`, `jwt_location: JWT_LOCATION_QUERY_PARAM`, `jwt_name: jwt`).

Clients append the token as the `jwt` query parameter:

```bash
curl "https://llm.example.com/v1/chat/completions?jwt=$TOKEN" \
  -H 'Content-Type: application/json' -d '{"model":"...","messages":[...]}'
```

```python
# OpenAI SDK via a custom httpx client that adds the token to every request URL
import httpx
from openai import OpenAI
client = OpenAI(
    base_url="https://llm.example.com/v1",
    api_key="unused",
    http_client=httpx.Client(params={"jwt": TOKEN}),
)
```

### Security: token-in-URL

The property that makes claims readable — the query param surviving to the
DataScript — also means the token rides in the URL. Mitigate:

- **TLS is mandatory** (encrypts the URL in transit).
- **Short-lived tokens** (minutes) to bound exposure.
- **SE query-param log redaction** so the token isn't written to access logs.
- **Strip before backend.** The SE forwards the query string to the upstream
  model/MCP server, so the token can land in *its* logs. A query-strip before
  the pool removes it; the exact SE query-rewrite primitive must be confirmed
  on-cluster, so AKO does not yet emit it automatically — treat this as the
  remaining hardening step for production `jwtQuery` use.

`jwtQuery` is intended for **machine-to-machine** traffic on internal/TLS paths,
not for public browser clients (which use `oauthBrowser`).

## The clean long-term fix (RFE)

The ideal is a standard `Authorization: Bearer` JWT that is both validated **and**
whose claims are visible to policy — i.e. after JWT validation the SE either
preserves the `Authorization` header to DataScripts or exposes validated claims
via a `get_jwt_claim()` API / claim-to-header injection. With that, `jwtQuery`
collapses to "the same thing, but read from the header instead of the query
string," and the token-in-URL tradeoff disappears.
