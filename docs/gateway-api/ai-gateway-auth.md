# AI Gateway — Authentication

`AIGatewayAuthPolicy` attaches JWT authentication to an HTTPRoute (or Gateway).
The Avi Service Engine (SE) validates the caller's token and AKO's policy
DataScripts read the **validated** claims to drive per-user token budgets,
identity counters, model entitlements, and MCP per-tool RBAC.

On the current Avi build, *how* the SE validates the token determines whether
claims are readable by DataScripts. There are three modes, selected by
`spec.authMode`. A route is exactly one of them.

| | `oauthBrowser` (default) | `jwtQuery` | `jwtHeader` |
|---|---|---|---|
| Client | Browser / interactive | Machine client that can put the token in the URL (our agents, the hub) | Any standard bearer client — MCP clients, AgentMinder discovery, SDKs |
| Token presentation | OAuth auth-code → session cookie | `?jwt=<token>` query param | `Authorization: Bearer <token>` |
| Unauthenticated response | `302` → `/authorize` | `401` (no redirect) | `401` (no redirect) |
| Claims in DataScripts | `oauth_get_claim()` — all | base64url-decode the query token — all | `avi.http.get_userid()` — **`sub` only** |
| Avi objects | Pool + `AUTH_PROFILE_OAUTH` + `SSO_TYPE_OAUTH` + `oauth_vs_config` | `JWTServerProfile` + `AUTH_PROFILE_JWT` + `SSO_TYPE_JWT` + `jwt_config` (query) | same as `jwtQuery`, `jwt_config` location = header |
| Main tradeoff | No machine clients | Token in the URL (unmaskable in logs, 12 KB line limit) | Only `sub` reaches policy |

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
DataScript — also means the token rides in the URL. What actually helps:

- **TLS is mandatory** (encrypts the URL in transit).
- **Short-lived tokens.** This is the real mitigation, and the estate leans on it:
  workload tokens minted at the issuer's `POST /exchange` live **60 seconds** and
  are bound to one target and one skill. A leaked URL is a leaked credential for
  about as long as it takes to read the log line.
- **Strip before backend.** The SE forwards the query string upstream, so the
  token can land in the *backend's* logs. A query-strip before the pool removes
  it; the exact SE query-rewrite primitive is still unconfirmed on-cluster, so
  AKO does not emit it automatically — this remains the open hardening step.

> ⚠️ **Avi access logs cannot be made to hide the token — measured, not assumed.**
> Two things go wrong. `uri_query_field_rules` match on the **parameter name**,
> so a rule written as `jwt=` never fires. And fixing that does not solve it:
> on Avi 30.2.1 and later, masking a query field *populates* `orig_uri`
> ("Unparsed URI") with the original request line, token and all. No
> configuration removes it from the log. Treat every `jwtQuery` request line as
> containing a live credential for the token's lifetime, and keep that lifetime
> short.

> **Length limit.** The whole request line must stay under the SE's
> `client_max_header_size` (default 12 KB, applied per line rather than to the
> headers in aggregate). A longer request line is rejected with `400`. Tokens up
> to ~12 KB read correctly in the DataScript and RBAC stays correct — the limit
> fails loudly rather than by silently truncating a claim.

`jwtQuery` is intended for **machine-to-machine** traffic on internal/TLS paths,
not for public browser clients (which use `oauthBrowser`).

### Where a machine client's token comes from

`AIGatewayAuthPolicy` validates a token; it does not mint one. On the lab estate
the issuer that mints them is deliberately **in-cluster**, and a workload proves
its identity rather than asserting it: it presents its projected Kubernetes
ServiceAccount token to `POST /exchange`, and the issuer runs a TokenReview and
chooses the `sub`, `group`, `target` and `skill` itself. Authorization therefore
happens at **mint time** as well as at enforcement time. The forgeable
`GET /token` was retired on 2026-08-19 and answers `410 Gone`.

Two constraints keep that issuer in-cluster rather than pointing the auth policy
at an enterprise IdP: a generic IdP cannot do mint-time authorization, and AKO
**snapshots the JWKS** into the `JWTServerProfile`, so a key rotation at the IdP
is an estate-wide `401` until AKO re-reconciles. See
[ai-gateway-agentminder-pdp.md](ai-gateway-agentminder-pdp.md) for the
broker-shaped way out, and [Handbook §6.2](ai-gateway-handbook.md#62-principals)
for the full identity model.

## `jwtHeader` (standard bearer — MCP clients)

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIGatewayAuthPolicy
metadata:
  name: load-control-am-auth
  namespace: inference
spec:
  authMode: jwtHeader
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: load-control-am
  jwt:
    issuer: https://default.idsp.ai.avi.com/common/
    jwksUri: https://default.idsp.ai.avi.com/common/oauth2/v1/jwks
    audiences: ["https://default.idsp.ai.avi.com/common/"]
```

Same object graph as `jwtQuery` — `JWTServerProfile` (JWKS fetched in-cluster and
embedded), `AUTH_PROFILE_JWT`, `SSO_TYPE_JWT` — with `jwt_config.jwt_location:
JWT_LOCATION_AUTHORIZATION_HEADER` and no `jwt_name`. The client sends the token
the way every OAuth client already does:

```bash
curl https://load-control-am.ai.avi.com/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

### What a DataScript can see in this mode

The SE strips `Authorization` before any DataScript runs, and the header APIs are
disabled outright in the `HTTP_AUTH` / `HTTP_POST_AUTH` events, so there is no
point at which the token itself can be read. What the SE *does* expose, after
validation, is the subject: `avi.http.get_userid()` returns the token's `sub`
(nil before authentication). Measured on Avi 31.2.1, 2026-09-05, with two tokens
of different subjects; see the spike result below.

So on a `jwtHeader` route the claim helper answers `jwt_claim("sub")` from
`get_userid()` and returns `""` for **every other claim**. Consequences, per
policy:

| Policy | On a `jwtHeader` route |
|---|---|
| `AITokenRateLimitPolicy` | Per-identity budgets work unchanged (identity = `sub`). `groupBudgets` keyed on a `group` claim see `""` — key them on `sub`, or leave the group map to the issuer that decides it from `sub` anyway. |
| `AIModelRoutePolicy.entitlements` | `groupClaim: sub` — rules per subject. |
| `AIMCPRoutePolicy.toolAccess` | `roleClaim: sub` — **per-tool rules per caller**. With an external IdP whose `sub` is a client id (IDSP: `sub == azp == client id`, not overridable), that is per-tool, per-agent. |
| `AIA2ARoutePolicy.agentAccess` | `agentClaim: sub` works; `skill` / `target` claims read `""`, so skill- and target-binding need the issuer to pack them into `sub` — possible with the in-cluster broker, not with an IdP you don't control. |
| Token ledger / counters | Identity = `sub`; unchanged. |

A route is exactly one mode. Choose `jwtHeader` where the caller is a standard
client (an MCP client, AgentMinder's tool discovery, an SDK) or where the token
must stay out of the URL; keep `jwtQuery` where a policy needs a claim other than
`sub` from a token whose `sub` you cannot shape.

### Why this is conformance, not a workaround

The MCP specification mandates `Authorization: Bearer`, and RFC 9728 protected-
resource metadata — which IDSP publishes for its own resources with
`bearer_methods_supported: ["header"]` — declares the header as the only
presentation. No compliant MCP client will ever send `?jwt=`. `jwtHeader` is what
lets such a client reach a governed route at all; the query mode remains the
richer one only because of what the SE chooses to expose to Lua.

## The clean long-term fix (RFE)

The ideal is a standard `Authorization: Bearer` JWT that is both validated **and**
whose claims are visible to policy — i.e. after JWT validation the SE either
preserves the `Authorization` header to DataScripts or exposes validated claims
via a `get_jwt_claim()` API / claim-to-header injection. With that, `jwtQuery`
collapses to "the same thing, but read from the header instead of the query
string," and the token-in-URL tradeoff disappears.

## Spike result 2026-09-05 — the header mode is not as blind as we thought

Measured on the lab (Avi 31.2.1) with a throwaway EVH child whose `jwt_config` was
set to `JWT_LOCATION_AUTHORIZATION_HEADER`, reusing the live SSO policy by
reference:

| What | Result |
|---|---|
| `avi.http.get_header("Authorization")` in `HTTP_REQ` | stripped (`-1`), as before |
| Any header/cookie API in `HTTP_AUTH` / `HTTP_POST_AUTH` | **disabled by the sandbox** — `API avi.http.get_header() disabled in the event of http_auth`. Copying the header before validation is impossible by design, not by ordering |
| **`avi.http.get_userid()`** in `HTTP_POST_AUTH` and `HTTP_REQ` | **the validated token's `sub`** — nil before authentication, `time-agent` / `load-probe` for two different tokens, `401` with no token |
| reqvars set in `HTTP_AUTH` / `HTTP_POST_AUTH` | persist into `HTTP_REQ` |
| `avi.utils.sha1_hash`, `md5_hash`, `base64_encode/decode`, `rand_bytes`; `avi.http.method()` | all work, in all three events |

So a bearer in the **Authorization header** gives a DataScript a verified identity
after all — the SE just never told anyone that JWT validation populates the user
id (the docs credit only Basic Auth and client certificates). What header mode
still withholds is every claim *other* than `sub`.

That reframes the trade. `jwtQuery` exists to expose claims; if the only verified
string the SE will hand over is `sub`, the issuer we own can put the claims *in*
`sub` — `"agent|target|skill"` for the 60-second per-skill tokens, with the
`sub→group` map baked into the DataScript exactly as `groupBudgets` is — and the
token leaves the URL, taking the unmaskable `orig_uri` leak with it. Design only;
not built. It also unblocks AgentMinder MCP tool discovery, which sends a bearer
and could never get past the query-only route.

Probe method: `hack/spikes/auth-probe.sh` (header) — the result above supersedes
its §6 hypothesis.
