<!--
  AgentMinder (Symantec Identity Security, 4.1) as the workload identity BROKER — the box
  ai-gateway-dc-reference-architecture.md §3.2 already specified and the in-cluster jwt-issuer
  currently implements by hand. Enforcement stays the Avi auth profile the SE already runs.
  Drafted 2026-09-04 from the Broadcom TechDocs 4.1 set
  (techdocs.broadcom.com/us/en/symantec-security-software/identity-security/agentminder/4-1).
  DESIGN ONLY - not built, no AgentMinder instance in the lab yet.

  Two earlier drafts were wrong and are superseded:
  (1) a per-request AuthZEN PDP callout over ICAP — unnecessary, our authz is mint-time;
  (2) "point the auth profile at AgentMinder as the enterprise IdP" — that is option A in
      dc-reference-architecture §3.2, which that doc explicitly REJECTS. AgentMinder is not
      an enterprise IdP; it is a broker, and it belongs in option C's box.
  §8 also corrects a wrong reason: response-side enforcement was written off as "breaks
  streaming", but this estate is already non-streaming BY DESIGN and already buffers every
  metered response. That objection does not apply here.
-->

# AKO AI Gateway — AgentMinder as the Identity Broker

> **Status: Designed, not built.** AgentMinder 4.1 GA'd on 2026-08-31.
>
> **The change, in one line.** AgentMinder becomes the **broker** —
> the sole issuer the gateway trusts, already specified in
> [ai-gateway-dc-reference-architecture.md](ai-gateway-dc-reference-architecture.md) §3.2 and
> currently implemented by ~400 lines of our own Python (`jwt-issuer`). The Avi auth profile
> is unchanged and remains the enforcement point.
>
> **Why this framing and not the last one.** The obvious reading — "a real IdP exists now, so
> point `AIGatewayAuthPolicy` at it and delete the issuer" — is **option A** in §3.2, and that
> document rejects it for four specific reasons. AgentMinder escapes the rejection not because
> the reasons are wrong but because **it is not an enterprise IdP**. Broadcom's own integration
> guidance says to *"federate human identities into Symantec Identity Security Platform via SAML
> or OpenID Connect (OIDC); agents remain native IDSP clients"* — which is option C's diagram,
> drawn by the vendor. The architecture does not change. The broker box gets a product in it.
>
> Extends [ai-gateway-auth.md](ai-gateway-auth.md) (the two SE auth modes, unchanged) and
> implements [ai-gateway-dc-reference-architecture.md](ai-gateway-dc-reference-architecture.md)
> §3.2 option C.

---

## 1. Why there is a broker at all

§3.2 of the reference architecture asks how to reach a single trust root and gives three
answers. Re-running that table against AgentMinder specifically:

| Option | Original verdict | With AgentMinder |
|---|---|---|
| **A** — the IdP is the issuer; every agent is an IdP client | **Rejected**: an Entra/Okta tenant will not hold a client per agent, nor mint 60-second, per-skill, `jti`-bearing tokens at agent rates; mint-time authz is lost | Still the right rejection **for Entra/Okta**. AgentMinder is a different animal: agent clients are a first-class type, it ships **DCR** so the agent factory can register them programmatically, and mission/intent **is** mint-time authz |
| **B** — federate our keys into the IdP | **Rejected**: not offered by SaaS IdPs | Unchanged, and moot |
| **C** — a workload identity broker in front of the IdP | **Recommended** | **This is AgentMinder.** Humans federate in upstream (SAML/OIDC), agents are native clients, one JWKS, one issuer the SE trusts |

So the question is not *"broker or IdP"*. It is *"does AgentMinder do the broker's four jobs?"*

| Broker job (§3.2) | AgentMinder | Notes |
|---|---|---|
| **One JWKS, one rotation, on your cadence** | ✅ *conditionally* | It is self-hosted, so the cadence is ours — unlike a SaaS IdP. Still pin the signing key to an external key provider (HSM / KMS, supported) so rotation is a decision, not an event. |
| **Mint-time authorisation survives** | ✅ | Mission credential, intent scope, tool→intent resolution, `agentMaxDelegationDepth` — all shipped. This is the job AgentMinder was built for. ⚠ But the *source of truth* moves from our CRDs to their intent catalog — §6. |
| **You control token size** | ⚠ **unproven, and this one can end the integration** | The 12 288-byte request-line limit is measured, and `jwtQuery` puts the token in the URL. A token carrying mission + intent scope + `act` + `delegatedBy` is fatter than our minimal one. If it does not fit and cannot be trimmed, **there is no fallback**: `oauthBrowser` does not work for machine clients, and no third mode exposes claims to a DataScript. Budgets, model tiers and MCP tool RBAC all read claims, so they all stop. Measure before committing — **S2**. |
| **No IdP in the request path** | ✅ | Unchanged: AKO snapshots the JWKS, the SE never calls out. An IDSP outage stops *new* tokens, not validation. |

Three and a half out of four. The half is token size, and it is measurable in an afternoon.

## 2. What replaces what

| Today | After |
|---|---|
| `jwt-issuer` Deployment (ns `inference`) — our hand-built broker | AgentMinder IDSP — the same role, as a product |
| `POST /exchange` — TokenReview → mint-time authz → 60 s token | RFC 8693 **token exchange**, SA token in, agent token out (§3) |
| `POST /persona` — console personas | Enterprise IdP federated into IDSP; humans get real identities (§4) |
| `GET /token` (retired, 410) | stays gone |
| `GET /jwks` served from the pod | IDSP `/.well-known/jwks.json` |
| Allow-list logic hand-written in the issuer | AgentMinder **intent catalog** + **tool bindings** |
| Per-agent ServiceAccounts | Kept — they become the *input* to token exchange, not the credential the gateway sees |

Everything on the Avi side — `JWTServerProfile`, `AUTH_PROFILE_JWT`, `SSO_TYPE_JWT`,
`jwt_config`, and every DataScript that reads claims — is **unchanged**.

## 3. The workload leg — the make-or-break

This is the part that decides whether the integration is clean or grubby, and it is the one
thing neither the AgentMinder docs nor the reference architecture answers.

Our broker authenticates a workload by **`TokenReview`**: the pod presents its projected
ServiceAccount token, the issuer asks the API server "is this really `log-collector`?", and mints
accordingly. The credential is bound, short-lived and audience-scoped, and **nothing is stored in
a Secret**. That property is worth protecting.

AgentMinder needs an equivalent. Two ways:

| | Shape | Assessment |
|---|---|---|
| **Preferred — federated token exchange** | Register each cluster in IDSP as a federated identity source against the API server's OIDC discovery + JWKS; the pod exchanges its **projected SA token** as RFC 8693 `subject_token` for an agent token carrying mission + intent | Preserves every property of TokenReview. Token exchange **is shipped** in 4.1. Whether IDSP will accept a Kubernetes SA JWT as `subject_token` is unconfirmed — **S1** |
| **Fallback — client secret per agent** | Each agent client gets a secret, stored in a K8s Secret, used for `client_credentials` | Works today with certainty, but replaces a bound 10-minute token with a long-lived static secret sitting in etcd. A real regression in posture — accept only as a stopgap |

If S1 passes, this is strictly better than what we have: same authn strength, plus a catalog and
an audit trail. If S1 fails, say so plainly rather than pretending the fallback is equivalent.

**Registration.** AgentMinder ships **DCR**, so the agent factory — which already mints a
ServiceAccount per agent — can register the matching agent client and intent binding in the same
provisioning step. That closes the [factory SA RBAC gap] pattern rather than adding a second
manual registry to keep in sync.

## 4. The human leg

The estate has never had this. Humans authenticate to the console today via `POST /persona`,
which is a mint-anything endpoint with no real login behind it.

Broadcom's guidance: *"Federate human identities into Symantec Identity Security Platform via
SAML or OpenID Connect (OIDC); agents remain native IDSP clients."* So the enterprise IdP
(Entra/Okta/Ping — or, in the lab, whatever we point at) sits **upstream of** IDSP, exactly as
§3.2 draws it:

```
   human ──OIDC code flow──► enterprise IdP ──┐
                                              ├──► AgentMinder ──► one signed JWT ──► SE
   workload ──projected SA token──► exchange ─┘   (sole issuer, own JWKS)
```

Note the reference architecture's stronger rule still applies and is worth keeping: **humans
never authenticate at an AI VIP.** They authenticate to the console, which holds the OIDC session
and calls the gateway as a workload carrying the human's identity. That keeps one profile type
(`jwtQuery`) and one claim path on every surface.

## 5. The auth profile

Unchanged in shape from what is deployed — only the issuer, JWKS and audience move.

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIGatewayAuthPolicy
metadata:
  name: agentminder-broker
  namespace: inference
spec:
  authMode: jwtQuery
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: mcp-route
  jwt:
    issuer:   https://idsp.ai.avi.com/oauth2/default
    jwksUri:  https://idsp.ai.avi.com/oauth2/default/.well-known/jwks.json
    audiences: ["ai-gateway"]          # ⚠ Audiences[0] only — a change is a hard cutover
```

## 6. Claim mapping — no Go changes

Every claim name the DataScripts key on is already a CRD field:

| What the policy needs | CRD knob | Default | Source |
|---|---|---|---|
| Entitlement group (model tiers, budgets) | `AIModelRoutePolicy.entitlements.groupClaim` | `group` | [modelroute_types.go:286](../../ako-gateway-api/aigateway/modelroute_types.go#L286) |
| Role (MCP per-tool RBAC) | `AIMCPRoutePolicy.toolAccess.roleClaim` | `role` | [mcproute_types.go:125](../../ako-gateway-api/aigateway/mcproute_types.go#L125) |
| Calling agent (A2A) | `AIA2ARoutePolicy.agentAccess.agentClaim` | `agent` | [a2aroute_types.go:111](../../ako-gateway-api/aigateway/a2aroute_types.go#L111) |
| Invoked skill (A2A) | `AIA2ARoutePolicy.agentAccess.skillClaim` | `skill` | [a2aroute_types.go:123](../../ako-gateway-api/aigateway/a2aroute_types.go#L123) |

Point the knobs at IDSP's claim names rather than mapping IDSP's claims back to ours — the
policy then reads in the vocabulary a security reviewer can check against the intent catalog.

**The real question underneath this is ownership.** Today the allow-list lives in our CRDs and
the DataScript is compiled from them. After this change the *decision* is made from
AgentMinder's intent catalog at mint time, and the DataScript enforces whatever claim arrives.
Two sources of truth is the failure mode to avoid. Pick one:

- **AgentMinder is authoritative** (recommended) — the CRD allow-lists become a **backstop**:
  keep them, but they should never be the thing that denies a correctly-minted token. If they
  do, the catalog and the CRD have drifted, and that is worth alerting on.
- CRDs stay authoritative and something syncs them into the catalog — more moving parts, and it
  re-creates the registry-drift problem the factory already has.

**Open item:** confirm how mission and intent scope appear in the access token — discrete
claims, a space-delimited `scope`, or only inside the mission credential. Our DataScripts do
exact-match on a claim value, so a space-delimited scope needs an IDSP claim-mapping rule or a
small DataScript change. This is the one thing that turns "config only" into "a little Go".

## 7. MCP tool authorization

This is the surface where the two models meet most directly, so it is worth being exact about
both sides.

### 7.1 What we enforce today

`GenerateMCPToolAuthScripts` ([mcproute_datascript.go:124](../../ako-gateway-api/aigateway/mcproute_datascript.go#L124))
generates two Lua snippets from `AIMCPRoutePolicy.toolAccess`: `HTTP_REQ` turns on request-body
buffering, and `HTTP_REQ_DATA` reads the JSON-RPC body, extracts `method` and `params.name`, and
gates **`tools/call`** against baked-in constant tables — `ROLE_STAR` (role may call anything),
`ALLOW` (role → tool, exact) and `ALLOW_PFX` (role → prefix). The caller's role comes from
`jwt_claim(ROLE_CLAIM)`.

Worth noting against the A2A surface: **MCP already keys on the tool, not the method.** The
method-vs-skill problem is an A2A problem; MCP tool authz is correct today. What it lacks:

- **`tools/list` is not filtered.** Protocol plumbing — `initialize`, `tools/list`, `ping` —
  passes on OAuth alone, so every caller *sees* every tool on a server even when it cannot call
  one. Discovery leaks the surface.
- **The allow-list is hand-maintained** in the CRD and baked into Lua at reconcile. Adding a tool
  is a CRD edit plus an AKO reconcile.
- **Role, not intent.** `role → tool` is a flat grant with no notion of what the agent was sent
  to do.

### 7.2 What AgentMinder models

Configuration is built in dependency order: AI Resource Server Apps → Agent Apps → Intent Catalog
→ Mission Catalog → **Tool Bindings** → **Authorization Surface** → Authorization Policies →
Scoping Constraints → Gateway Routes.

A **tool binding** maps one MCP tool to one intent (`GET /admin/v1/AgentToolBindingsHelper`):

```json
{
  "toolName": "hris_get_employee",
  "intentScope": "urn:iam:agent:intent:employee_lookup",
  "skill": "hris.lookupEmployee",
  "isPublic": false,
  "source": "DISCOVERED",
  "resourceServerApp": {
    "appId": "app-hr-resources",
    "name": "HR Resources",
    "primaryAudience": "https://gw.example.com/aigateway/v1/mcp/hr"
  }
}
```

Three things in that payload matter to us:

- **`"source": "DISCOVERED"`** — AgentMinder *discovers* tools from the MCP server rather than
  being told about them. That is a direct upgrade on the hand-maintained `mcp-registry` ConfigMap,
  and it means the catalog cannot silently drift from what a server actually exposes.
- **`primaryAudience` is per resource server** — see §7.4.
- **`isPublic`** — the input a `tools/list` filter would need.

The **Authorization Surface** (`PUT /admin/v1/AgentAuthzSurface/<clientId>`) is the agent's
mission-type × intent matrix:

```json
{ "onboard_new_hire": ["employee_lookup","create_task","assign_training"],
  "offboard_employee": ["employee_lookup","create_task"] }
```

So the full chain is **agent → mission type → intents → tool bindings → tools**, and every link
is a reviewable object rather than a line of YAML.

### 7.3 Three ways to wire it, and which to pick

| | Design | Assessment |
|---|---|---|
| **A** | Point `toolAccess.roleClaim` at the token's intent claim; rules become `intent → tools` | Zero new code — **but `jwt_claim()` returns a scalar** and an agent normally holds *several* intents. Works only for single-intent tokens. Needs a DataScript change to test set membership — **S6** |
| **B** | **Mint the resolved tool list into the token.** AgentMinder already knows mission × surface × bindings; if that resolves to a `tools` claim, the DataScript does a set-membership check and the CRD tables disappear entirely | **Recommended.** Authorization is wholly decided at mint, adding a tool to an agent needs no AKO reconcile, and the SE check is O(1). Costs token size (S2) and depends on IDSP supporting a computed claim — question 9 in §16 |
| **C** | Keep the CRD tables, sourced from AgentMinder's bindings by a sync job | Two sources of truth, which §6 says to avoid. Only if A and B both fail |

Recommend **B**, with the existing CRD tables demoted to the backstop described in §6: they stay
deployed, but a correctly-minted token should never be denied by them. If one is, the catalog and
the CRD have drifted — alert on it rather than relying on it.

**What runs today (2026-09-05) is C, and it is not a fallback.** On `authMode: jwtHeader` the SE
exposes only the validated `sub` (`avi.http.get_userid()`), so neither an intent claim (A) nor a
resolved-tools claim (B) is readable at the front door; both stay tied to `jwtQuery` and its
`orig_uri` leak. `ako-inference-demo/am-sync-bindings.sh` resolves the AgentMinder chain
(`AgentAuthzSurface` × `AgentToolBindingsHelper`) per MCP server and replaces `toolAccess.rules`
keyed on the agent client id. Measured: grant widened in the console → CRD patched → AKO re-baked
the DataScript in 4 s → the SE's `ALLOW` table carried the new tools. The two-sources-of-truth
objection is answered by making the sync the only writer of those rules.

### 7.4 Free server-level authorization, via the audience

`primaryAudience` is per resource-server app, and the SE already validates `aud` in crypto before
any DataScript runs. So if each MCP server is its own route with its own audience — which is how
the estate is already laid out, one hostname per server behind `mcp-gateway` — then **a token
minted for `mcp-web` is cryptographically unusable against `k8s-logs`**, with no Lua involved.

That gives a clean two-tier model:

| Tier | Granularity | Enforced by | Cost |
|---|---|---|---|
| Server | which MCP server | SE `aud` validation | free, already running |
| Tool | which tool on it | `HTTP_REQ_DATA` DataScript (§7.3 design B) | one claim lookup |

The one thing to check is that `Audiences[0]` per route lines up with `primaryAudience` per
resource-server app — a 1:1 mapping we should keep deliberate rather than accidental.

### 7.5 Filtering `tools/list`

`isPublic` plus the agent's resolved binding set is exactly the input needed to filter a
`tools/list` response so an agent sees only what it may call. The mechanism already exists:
responses are buffered (§8), so a response-phase DataScript could rewrite the array.

Honest caveat: this is the same sandbox problem as `fieldsExcluded` — no regex, no
`string.match`, and filtering a JSON array of objects by name is harder than truncating one.
Spike it with §8 as **S7**; do not promise it first. It is a real gap worth closing, but the
`tools/call` gate is the control that matters, and that one is solid.

## 8. Constraints: decided at mint, enforced on the response

This section corrects an error in the previous draft. It wrote off response-side enforcement
because it "breaks streaming". **That reason is void in this estate.**

We do not stream — deliberately. `HTTP_RESP_DATA` fires buffer-complete, so reading a streamed
body collapses the stream, which means a streamed response cannot be metered
([ai-gateway-token-ledger.md](ai-gateway-token-ledger.md) — streaming counts **zero**, a known
budget bypass). The consequence is that AKO **already turns on response buffering** for every
metered JSON response ([datascript.go:606](../../ako-gateway-api/aigateway/datascript.go#L606),
`buildBufferEnableBlock`) and already parses the body in the response phase to read the OpenAI
`usage` block.

So the response body is **already fully in the SE's hands**, for exactly the traffic we care
about, at no additional architectural cost. Which reopens something worth having:

AgentMinder's PDP returns `constraints` — `fieldsExcluded`, `maxRecordsReturned` — and
Appendix A is explicit that **no gateway enforces them**, including AgentMinder's own. They are
recorded and ignored. But constraints are a property of an *intent*, and intent is decided at
mint time. **So they can ride in the token as a claim** and be enforced by the response-phase
DataScript that already exists — no PDP callout, no new component, no per-request round trip.

Worth being precise about the limits before promising it:

| Constraint | Feasible in the response DataScript? |
|---|---|
| `maxRecordsReturned` — truncate a JSON array | Plausible — counting and cutting at a delimiter is within the sandbox |
| `fieldsExcluded` — strip named keys | **Uncertain.** The SE Lua sandbox has no regex and no `string.match`; general JSON rewriting without a parser is not something to promise before spiking it — **S4** |

If S4 passes even partially, this is the strongest thing in the integration: a governance
control the vendor ships as roadmap, enforced on the Avi data plane, using only mechanisms both
products already have. If it fails, we have lost nothing — constraints are ignored today by
everyone.

## 9. The audit join — out of band, zero request-path cost

AgentMinder ships a **NATS JetStream `AUDIT_EVENTS` stream** and an **OTel Collector** with
exporters for Splunk HEC, Elastic, Azure Monitor and Kafka. Its correlation keys are
`clientTxnId`, `agentClientId`, `missionId`, `intentScope`.

We already have the other half: the SE emits a usage record per metered response and
`ai-gateway-ui` collects it into the token ledger, and Avi VS logs carry the per-request detail.

Joining them costs nothing in the request path: have the ledger collector stamp AgentMinder's
`clientTxnId` (it arrives in the token) onto each usage record, and either consume
`AUDIT_EVENTS` into the console or export both sides into the same SIEM. The result is one
timeline per agent action: *who was minted what, what they then called, how many tokens it
cost, and whether the gateway blocked it* — which is the story neither product tells alone.

Use **their** correlation key names, not ours. This is a join, not a schema negotiation.

## 10. The two consoles — and which one is the pane of glass

AgentMinder ships two UIs. Neither replaces `ai-gateway-ui`, and understanding *why* decides how
to wire them.

**Admin Console** — the control plane:

| Area | What it manages |
|---|---|
| Gateway Instance Management | registering, monitoring and unregistering Layer7 AI Gateway instances |
| Golden Config Pipeline | configuration assembly, **versioning and rollback** |
| Mission Administration | mission lifecycle — including suspend/revoke |
| Certificate Management | X.509 issuance and renewal |
| Dynamic Risk Scoring | behavioural anomaly scoring on mission credentials |

**Observe Console** — telemetry, rendered as *"dashboards, traces, and event queries"*, correlated
on `clientTxnId`, `agentClientId`, `missionId`, `missionType`, `intentScope`. Pre-built dashboards
cover agent inventory and risk scoring, mission delegation chains, privileged actions, and
rogue-action detection. Three data types: audit events, Prometheus metrics, and distributed traces
spanning SDK → gateway → PDP → backend.

### 10.1 The catch

**Their gateway is not in our request path — so the Observe Console will be largely empty.** Its
audit events and traces come from *its own* Layer7 AI Gateway instances. With Avi as the gateway,
AgentMinder sees the **mint side** only: tokens issued, missions created and revoked, delegation
chains, risk scores. Every request-level fact — which tool was called, whether the SE blocked it,
how many tokens it cost, which pool served it — lives in Avi and in `ai-gateway-ui`.

That is not a defect of either product. It is the direct consequence of §1: we took the broker and
declined the gateway. But it does mean **neither console is complete on its own**, and pretending
otherwise in a demo will not survive the first question.

### 10.2 Who owns which pane

| Job | Console | Why |
|---|---|---|
| Authoring intents, bindings, missions, authorization surface | **AgentMinder Admin** | It is the source of truth (§6), and this is the reviewable-catalog UI the estate has never had — today it is YAML in CRDs |
| Config versioning and rollback | **AgentMinder Admin** | Golden Config Pipeline; nothing in the estate does this for policy |
| Mission revocation, cert renewal, risk scores | **AgentMinder Admin** | Lifecycle operations with no Avi equivalent |
| The AI estate at runtime — topology, flow map, tool calls, token ledger, blocks | **`ai-gateway-ui`** | It already has this, and it is where the request-side truth is |

**Recommendation: `ai-gateway-ui` stays the single pane of glass**, and pulls the mint side across.
The Dashboard already has Topology / Agents / Flow Map / Tokens; identity becomes one more surface —
per agent: its client id, current mission, granted intents, resolved tools, and risk score, pulled
from the Admin APIs and joined to the ledger on `clientTxnId` per §9. That preserves the demo
narrative (one console for the whole AI estate) while AgentMinder's Admin Console does what it is
genuinely better at — being the policy authoring and audit system of record.

Push the other way as well if it is cheap: exporting Avi's usage records into AgentMinder's OTel
Collector makes the Observe Console complete too, which is the version that matters for a
Broadcom-stack conversation. Both directions use the same correlation key, so it is one join done
twice, not two integrations.

### 10.3 Could Avi register as a gateway instance? — checked

**Mechanically yes. Architecturally no. And the part we actually wanted is available without it.**

The protocol is documented, small, and entirely conventional — there is no proprietary transport,
no agent, and no mesh:

| Step | Call |
|---|---|
| Register at startup | `POST /admin/v1/AIGateways/register` with group ID + host metadata → returns an instance ID and a config-version pointer |
| Poll for config | `POST /admin/v1/AIGatewaysExternal/config/sync` — the **external** variant, scope `urn:iam:t.aigatewayclient`, `X-TENANT-ID` header, body `BootstrapConfigDTO {id, groupId, name, type, environment, metadata, syncInterval}` |
| Response | `{ "version": 7, "config": "routes:\n  - name: hr\n    path: /aigateway/v1/mcp/hr\n    ..." }`, or **304** when `If-None-Match` matches |
| Deregister | `DELETE /admin/v1/AIGateways/instances/{id}` (also on graceful shutdown) |

Default poll interval is 300 s (`AgentBindingsPoller` / `gatewayAgentBindingsPollInterval`), and
the Instances tab shows `lastSync` and applied version per instance. Nothing in the docs restricts
instances to Layer7 gateways — they are described generically. Writing an AKO-side agent for this
would be a small controller, not a project.

**So why not.** The golden config is assembled from *"resource-server applications, tool bindings,
routes, route-policy trees, and gateway settings"*, and routes carry `routePath`, `methods`,
`settings` and `policy`. Consuming it makes **AgentMinder the authoring surface for routing** and
demotes Gateway API to an implementation detail. That is the exact inversion this project exists to
argue against: today `HTTPRoute` + the AI CRDs are the source of truth and AKO translates them onto
Avi. It is not a small trade dressed as an integration — it is a different product thesis.

**And the honest middle doesn't survive contact.** "Register, but apply only the authorization
slices and ignore routing" sounds reasonable until you look at what the console does with it: an
instance is flagged stale when `lastSync` exceeds twice the poll interval **or when its applied
version lags the group's current version**. A partial applier is permanently either lying about its
applied version or permanently red. Reporting a version you did not apply is worse than not
registering at all, particularly on a console a security team reads.

**What to do instead.** The thing that made registration attractive was the *discovered tool
catalog* (§7.2). That is available from a plain admin API — `GET /admin/v1/AgentToolBindingsHelper`
— with no registration, no authorship inversion and no staleness to misreport. Pull it into
`ai-gateway-ui` alongside the mission and risk data in §10.2. Same value, none of the cost.

> **Useful side-finding.** Gateway groups carry `allowedGrantTypes`, `regenerateClientId` and
> `regenerateClientSecret`, and registration yields the `urn:iam:t.aigatewayclient` scope — which is
> one of the two scopes accepted by the AuthZEN evaluation endpoint. So **registering as a gateway
> group is the documented way a third party obtains PDP credentials**, which answers the question
> the first draft left open. We are not calling the PDP (§11), but the door is documented and open,
> and that is worth knowing before deciding we never will.

## 10.4 What only their gateway does — the honest forfeit

Appendix A lists several capabilities as **gateway** functions. We are not deploying their
gateway, so nothing in our path performs them. Two of these matter and were under-reported in
earlier drafts:

| Capability | Consequence of Avi being the gateway | Recoverable? |
|---|---|---|
| **Mission liveness check** | Suspending a mission in the Admin Console has **no effect on traffic** until the token expires. The console shows a control that nothing in our path enforces | Partly — 60-second TTLs bound the window. Do not describe mission revocation as immediate |
| **DPoP validation** | DPoP sender-constrains a token to a client key, so a leaked token cannot be replayed — the mitigation for our `orig_uri` log leak (§12.4). The SE cannot validate a DPoP proof | **Not as DPoP — but the *goal* is reachable by another route.** See §10.5: certificate-bound tokens (RFC 8705) are implementable on the SE today |
| **Intent-token receipt at the backend** | No per-call signed chain of custody arriving with the request | Mostly — §9's audit join reconstructs the same timeline out of band |
| **`header.*` directives** | No gateway applies them | **Yes** — a DataScript can read `act` from the verified token and set `X-On-Behalf-Of` itself. Cheap; do it in Phase 3 |

## 10.5 Why not DPoP — and what to do instead

**Why DPoP specifically cannot work on the SE.** Three concrete blockers, not a general
objection:

1. **No signature verification primitive.** Validating a DPoP proof means verifying an ES256/RS256
   signature over the proof JWT, per request. The DataScript sandbox exposes `avi.http`, `avi.vs`,
   `avi.pool` and `avi.ssl` — there is no crypto call to do it with, and unknown `avi.*` fields
   *raise* rather than returning nil.
2. **No SHA-256**, so the JWK thumbprint cannot be computed and compared to the token's `cnf.jkt`.
3. **`avi.http.get_method()` does not exist on this build** (measured — it 500'd the LLM front
   door), so DPoP's `htm` binding could not be checked even if 1 and 2 were solved.

For completeness, the two halves that *would* have worked: base64url decoding of the proof is
already implemented (`jwtClaimHelper`), and `jti` anti-replay is straightforward with
`avi.vs.table_insert` / `table_lookup` and a TTL — the same persistence tables the MCP session
DataScript already uses.

**But the goal is sender-constrained tokens, not DPoP** — and that *is* reachable, using the
mechanism a load balancer is actually good at. RFC 8705 (mutual-TLS certificate-bound access
tokens) is the sibling standard to DPoP, and every piece exists:

| Step | Mechanism |
|---|---|
| Issue a client cert per agent | AgentMinder's **Private CA** (Private CA REST APIs, CA Helper API, Certificate Management in the Admin Console). Appendix A already lists mission-credential validation as **JWT / X.509**, so X.509 is first-class in their model |
| Require and validate it | The SE, natively — PKI profile with `CLIENT_VERIFY_REQUIRE`, or `avi.ssl.set_pki_profile()`. **The chain validation is real crypto in C, not Lua** |
| Read the presented cert | `avi.ssl.client_cert_verified()` → must be `1`; `avi.ssl.client_cert(avi.CLIENT_CERT_FINGERPRINT)` — also available: `_SERIAL`, `_ISSUER`, `_SUBJECT`, `_SAN_EXTENSION` |
| Bind it to the token | Compare the fingerprint to the token's `cnf["x5t#S256"]` claim, read with the existing `jwt_claim` helper. **A string comparison — no crypto in Lua** |
| Enforce | Mismatch or unverified → 403, in the same `HTTP_REQ` phase that already runs |

That is sender-constraining, on the Avi data plane, with the SE doing the cryptography and Lua
doing a comparison. **A token scraped from `orig_uri` in a client log becomes useless** without the
corresponding private key — which is precisely the property DPoP was wanted for.

**Two caveats, both survivable.** Avi's `CLIENT_CERT_FINGERPRINT` may be SHA-1 rather than the
SHA-256 that RFC 8705's `x5t#S256` specifies; if so, bind on `CLIENT_CERT_SERIAL` + `CLIENT_CERT_ISSUER`
instead, which is unique under a private CA and needs no hash agreement at all. And mTLS must be
scoped to the **agent** surfaces (`jwtQuery` routes), not the human ones — a browser cannot be asked
for a client cert without ruining the console.

The cost is real: a certificate per agent, and rotation. But the agent factory already mints a
ServiceAccount per agent and would already be calling DCR (§3), so issuing a cert from AgentMinder's
CA in the same step is one more API call — and it is the same direction as the SPIFFE/SPIRE work in
[ai-gateway-backend-mtls-design](ai-gateway-backend-mtls.md), just on the client side of the SE
instead of the backend side.

Spiked as **S8**. If it passes, the DPoP forfeit in §10.4 largely disappears and the estate ends up
with a *stronger* client-authentication story than the one it has today — and the standing RFE (a
`Bearer` token both validated and readable by policy) remains the way to remove the token-from-URL
problem at its root rather than compensating for it.

## 11. What we are still not doing

The per-request AuthZEN PDP callout (`POST /access/v1/evaluation`). The trade, restated now
that the streaming objection is gone:

| Given up | Cost | Mitigation |
|---|---|---|
| Per-request decisions | A suspended mission stays usable until the token expires | 60-second tokens |
| Signed intent token per call | No per-call chain of custody at the backend | §9 gives the same audit trail out of band |
| PDP-evaluated constraints | none | §8 gets them from the token instead |

The reason remains structural: the SE has no general-purpose HTTP callout
([rfe-se-ai-native-callout.md](rfe-se-ai-native-callout.md)), so a PDP call would ride ICAP
REQMOD — a second bespoke 2003-protocol shim, in the authorization path, on every tool call.
Mint-time authz plus §8 plus §9 gets the value without it. If per-request revocation ever
becomes a hard requirement, that is the trigger to revisit, and this section is the record.

## 12. Risks

1. **JWKS rotation is an estate-wide 401.** AKO snapshots the keyset because the SE has no
   cluster egress ([jwt_rest.go:47-49](../../ako-gateway-api/aigateway/jwt_rest.go#L47-L49):
   *"Re-apply the policy to refresh rotated keys."*). Our issuer never rotates; a product will.
   Fix before Phase 3: pin the IDSP signing key to an external key provider **and** make AKO
   re-fetch the JWKS on a timer. Worth doing regardless of AgentMinder.
2. **The audience change is a hard cutover, per route.** Only `Audiences[0]` reaches Avi
   ([translator.go:90](../../ako-gateway-api/aigateway/translator.go#L90), [:174](../../ako-gateway-api/aigateway/translator.go#L174)).
   No rolling migration within a route; there is one across routes (§13).
3. **Token size vs the 12 288-byte request line** — §1's surviving broker job. Measure early.
4. **Token-in-URL still leaks into Avi logs.** Unchanged and unfixable by config: masking rules
   match the parameter *name*, and the raw token still lands in `orig_uri` on 30.2.1+. Survivable
   only because tokens are 60 seconds and `jti`-bound.
5. **Two sources of truth** (§6) if the CRD allow-lists are not demoted to a backstop.
6. Mint-time decisions cannot be revoked mid-token. Keep TTLs at ~60 s.

## 13. Migration

Each `AIGatewayAuthPolicy` builds its own `JWTServerProfile`, so the unit of cutover is the
**route** — which is what makes risk #2 survivable.

**Phase 0 — stand IDSP up.** `ssp-infra` + `ssp` (not `ssp-aigateway`; we are not using their
gateway). Register one application, one agent client, one intent, one tool binding.
*Exit:* a token mints, and AKO's pod can reach the JWKS URI.

**Phase 1 — the workload leg.** S1 (federated token exchange). Migrate `log-collector` — smallest
blast radius — and measure token size (S2). *Exit:* that agent reaches the MCP gateway on an
AgentMinder token; every other route untouched.

**Phase 2 — the human leg.** Federate an OIDC provider into IDSP; console and chat UI on real
logins. Retire `POST /persona`. *Exit:* a real login driving the existing per-group budgets.

**Phase 3 — the rest, route by route.** MCP, then A2A last (that is where mission/intent replaces
the hand-written skill allow-list; keep `agentAccess` as the backstop of §6). *Exit:* no route
references `jwt-issuer`.

**Phase 4 — delete `jwt-issuer`**, and land §8/§9 if their spikes passed.

Rollback at every phase is a one-line CRD revert.

## 14. Can we install it in the lab?

**Yes, but not without freeing RAM first.** Measured 2026-09-04.

**What IDSP 4.0 demo mode needs** ([sizing page](https://techdocs.broadcom.com/us/en/symantec-security-software/identity-security/identity-security-platform/4-0/Installing/sizing-the-deployment.html)):

- *"14 cores available in the cluster (7 cores in case enclave services are not deployed)"*
- *"44 GB available in the cluster. (28 GB in case enclave services are not deployed)"*

> ✅ **Those figures are correct — but only for the `demo` sizing profile.** The charts are **public**
> (`https://ssp_helm_charts.storage.googleapis.com`, chart `ssp` 4.1.1+1673, *"Helm chart to deploy
> AgentMinder"*), so the real footprint can be measured rather than guessed. Rendered locally with
> stock values (2026-09-05, `observe.enabled=false`), **the `ssp` chart alone** asks for:
>
> **The single most important value is `ssp.deployment.size`.** It defaults to **`custom`**, and the
> options are `demo` (60 auth/min), `small` (1200), `medium` (4000), `large` (8000), `custom`. Every
> deployment template branches on it:
> `{{- if eq .Values.ssp.deployment.size "demo" }} replicas: 1 {{- else }} replicas: {{ .Values.ssp.<c>.podReplicaCount }} {{- end }}`
>
> | Profile | CPU requests | Memory requests |
> |---|---|---|
> | `size: custom` (the **default** — 2 replicas everywhere) | 20.5 cores | 45.8 Gi |
> | **`size: demo`** | **6.7 cores** | **17.8 Gi** |
>
> So the techdocs figure of *"7 cores … 28 GB"* is **accurate for demo mode** — 6.7 cores measured.
> An earlier draft of this section claimed the sizing page was "badly wrong"; that was this author
> rendering the wrong profile, not a documentation error. Corrected 2026-09-05.
>
> ⚠ **Setting `podReplicaCount: "1"` on all 21 components does NOT work** — the templates ignore it
> unless `size` is `custom`, and at `custom` the *other* branch is taken. Set `size: demo`; do not
> hand-roll replica counts. `ssp-infra` (a **Percona XtraDB Cluster**, not a single MySQL) sits on
> top either way.
>
> **VM sizing that follows:** demo needs ~6.7 cores / ~17.8 Gi plus the database, so **16 vCPU /
> 30 GB** is comfortable — and 30 rather than 32 leaves `.66` about 6 GB of headroom instead of 4.5.
- An **NGINX ingress controller** (chart 4.14.0 / ingress-nginx v1.14.0) — note this lab
  deliberately runs no router, with Avi as the LB
- The infra chart stands up **its own MySQL** in demo mode (`db.enabled=true`) — no external DB to
  provision. The [database page](https://techdocs.broadcom.com/us/en/symantec-security-software/identity-security/identity-security-platform/4-0/Installing/Database-Consideration.html)
  lists four supported engines: MySQL 8.0/8.4 (demo-bundled *or* customer-managed), PostgreSQL
  14/15/17, Oracle 19c and Aurora MySQL 8.0. **Oracle is an option, not a requirement** — the
  Oracle-only `.sql` scripts in the AgentMinder packlist are for sites that choose it
- ClickHouse/observe **must be ON**: `clickhouse.enabled=true` is one of the two flags that turn
  IDSP into AgentMinder (§14.1). An earlier draft here said to leave it off. That was wrong, and
  it is why the AVX2 result below is load-bearing rather than incidental

Target the no-enclave profile: **~7 cores / 28 GB** — *plus* ClickHouse, which the sizing page's
numbers exclude.

### 14.1 How it installs

**AgentMinder has no installer of its own.** Its
[Installing page](https://techdocs.broadcom.com/us/en/symantec-security-software/identity-security/agentminder/4-1/installing-am.html)
says it outright: *"The AgentMinder installation process follows the same steps as IDSP 4.0, with
the addition of two feature flags that enable AgentMinder observability capabilities."* You
install IDSP 4.0 and switch AgentMinder on with Helm values.

```bash
export SSP_FQDN=idsp.ai.avi.com RELEASENAME=<rel> NAMESPACE=<ns>
export HELM_REPO=ssp_helm_charts
export HELM_REPO_URL=https://ssp_helm_charts.storage.googleapis.com

kubectl create ns ${NAMESPACE}
helm repo add "${HELM_REPO}" "${HELM_REPO_URL}" && helm repo update
helm search repo ssp_helm_charts --versions

# infra chart + the AgentMinder ClickHouse flag
helm install "infra-${RELEASENAME}" "${HELM_REPO}/ssp-infra" -n "${NAMESPACE}" \
  --set sspReleaseName="${RELEASENAME}" --set clickhouse.enabled=true

kubectl wait jobs.batch -n "${NAMESPACE}" \
  --selector app.kubernetes.io/name=ssp-infra-create-db-job \
  --for condition=complete --timeout 5m

# platform chart + the AgentMinder observability flags
helm install "${RELEASENAME}" -n "${NAMESPACE}" "${HELM_REPO}/ssp" \
  --set ssp.ingress.host="${SSP_FQDN}" --timeout=120m \
  --set global.observe.enabled=true \
  --set ssp.featureFlags.nats.enabled=true \
  --set global.otelAcceptExternalRequests.enabled=true \
  --set opentelemetry-collector.imagePullSecrets[0].name=<registry-creds> \
  --set nats.natsBox.container.image.pullSecretNames[0]=<registry-creds>

kubectl get pods -n "${NAMESPACE}"
curl -k "https://${SSP_FQDN}/default/.well-known/openid-configuration?sspinfo=true"
```

That last call returns the discovery document — the `issuer` and `jwksUri` §5 needs, straight out
of the installer. Then configure administrative access, and import Grafana dashboard **25536**.

**The chart repo is public** (a GCS bucket, no credentials), and neither `helm install` above names
a pull secret for the platform images — only the OTel-collector and NATS flags take
`<registry-creds>`. So entitlement may gate less than §16.1–3 assumes. `helm repo add` plus
`helm search` costs nothing, settles it, and pulling the charts exposes the image references, which
lets the x86-64-v2 precheck below run *before* any install rather than after credentials land.

**Do not deploy `ssp-aigateway`.** AgentMinder ships its own
[external AI gateway chart](https://techdocs.broadcom.com/us/en/symantec-security-software/identity-security/agentminder/4-1/installing-am/deploying-the-external-ai-gateway-with-helm.html)
from a second repo (`.../agentminder-charts/`), registered to a Gateway Group GUID with a platform
client id/secret. That is a second data path beside the one Avi already is — §10.3 settles why we
do not adopt their gateway even where it would work. Read the chart for the values shape; do not
install it.

**It does not fit on `vks-ai-01`.** 21 cores / ~87 Gi allocatable, but CPU *requests* are already
16.8 of 21 cores, with the three workers at 89 / 94 / 89 % — roughly **800 m of schedulable CPU
left across the whole worker pool**. Not a squeeze; an order of magnitude.

**The binding constraint is ESXi `.66`.** It runs all six openshift06 nodes (96 GB of 128 GB)
plus the jump box — which is why openshift07 is powered off. Realistic free memory is ~16–24 GB
against a 28 GB floor. `.67` (64 GB) already carries vCenter, the Avi Controller, the SEs and the
k8s-antrea nodes.

| | Route | Cost | Verdict |
|---|---|---|---|
| **A** | Drain one openshift06 worker, build a **single-node k8s VM (8 vCPU / 32 GB)** | +16 GB, no spend, reversible | **do this now** |
| **B** | Add RAM to `.66` (R720, DDR3 ECC, secondhand): 128 → 256 GB | ~£100–200 | do anyway; also unblocks openshift07 |
| **C** | Rebuild openshift07 as the identity cluster | nothing until B — same host, same RAM | after B |

> ⚠ **Correction, verified 2026-09-04 — route A does not work as first written.** An earlier draft
> said draining `worker-2` was "tight but schedulable". That was reasoned from **memory** requests
> and pod counts, and memory is not the constraint. **CPU requests are**, and they do not fit:
>
> | Node | CPU requested / 3500m allocatable | Free |
> |---|---|---|
> | `worker-0` | 3238m (92%) | **262m** |
> | `worker-1` | 3304m (94%) | **196m** |
> | `worker-2` | 3140m (89%) | *to relocate* |
>
> Masters are tainted `node-role.kubernetes.io/master`, so nothing relocates there. Stripping the
> DaemonSets and per-node pods that leave with the node, roughly **2 600m still has to move into
> 458m of space** — short by a factor of five or six. The drain would strand
> `qwen-15b-predictor` (1000m), `nomic-embed-predictor` (500m) and quite possibly **`ako-0`**
> (400m) in `Pending`. AKO going Pending stops the gateway reconciling, which is the worst
> possible outcome of a capacity exercise.
>
> **The cluster is request-bound, not resource-bound.** Actual CPU use is 7–22% against requests of
> 89–94%. The two KServe predictors alone reserve 1500m for work that runs on the PC's GPU. So the
> prerequisite for route A is a **request-trimming pass** (and/or making masters schedulable, which
> frees ~3.4 cores and is the compact-cluster posture openshift07 already used). Do that first,
> re-measure, then drain.

Two further mechanical notes for whenever the drain does happen:

- **Draining does not free ESXi RAM.** The VM keeps its 16 GB until it is powered off. The good
  news is that `worker-2`'s `Machine` has **no `ownerReferences`** and the only MachineSet is at
  `DESIRED=0`, so nothing will auto-recreate it — `oc delete machine openshift06-2l8ct-worker-2`
  deprovisions the VM and actually returns the memory. That is the irreversible step; the drain
  itself is undone with `uncordon`.
- **Evicting `ako-0` costs a front-door blip.** Rolling AKO 500s the front door for ~90 s while it
  reconciles. Expected, not a fault — wait it out rather than rolling anything back.

**Build it as plain Kubernetes, not OpenShift.** IDSP wants NGINX ingress and this estate
deliberately has the router disabled — a k3s/kubeadm VM sidesteps that, and it demonstrates that
the broker lives outside the AI cluster. Publish via ingress-nginx on a NodePort behind one
hand-built Avi VS at `idsp.ai.avi.com`.

> ⚠ **AVX2 — measured 2026-09-04. Not the blocker it looked like; the residual risk is narrower.**
> Both hosts are Dell R720 / Xeon E5-2660 (Sandy Bridge), the reason OpenShift 4.22 was impossible
> here. Measured on two guests on `.66` (`worker-2` and the jump box; `.67` not measured
> directly, and IDSP lands on `.66` under route A anyway): `avx`, `aes`,
> `pclmulqdq`, `sse4_2`, `popcnt`, and **no `avx2` / `fma` / `bmi1` / `bmi2` / `f16c`**. glibc names
> the ceiling exactly — `x86-64-v2 (supported, searched)`, with v3 and v4 unsupported. So the line
> is **v2 yes, v3 no**, and note that **AVX itself is present**: the usual AVX-gated components
> (MongoDB 5+ and later) need AVX, not AVX2, and are fine here.
>
> The stack's *public* components were run on that CPU, on `worker-2`, and all of them work:
>
> | Image | Test | Result |
> |---|---|---|
> | `mysql:8.4` (8.4.11) | initialise datadir, start the server | ✅ `ready for connections` |
> | `clickhouse/clickhouse-server:25.3` | start, then a 20 M-row vectorised aggregation | ✅ correct result |
> | `registry.k8s.io/ingress-nginx/controller:v1.14.0` | controller + nginx 1.27.1 | ✅ |
> | `eclipse-temurin:21-jre` | JVM startup | ✅ `UseAVX=1`, BMI1/BMI2 off — the JIT detects the CPU |
>
> ClickHouse — the one flagged as risky — is fine, and §14.1 *requires* it, so that result is
> load-bearing rather than incidental. The databases are not the problem. What is still untested is
> the **Broadcom `ssp-infra` / `ssp` images themselves** — but the charts come from a public bucket
> (§14.1), so this is probably runnable today rather than blocked on §16.3. Two failure modes:
>
> 1. **A UBI 10 / RHEL 10 base.** RHEL 10 raised its floor to x86-64-v3, so a UBI10 image dies in
>    `ld.so` — `Fatal glibc error: CPU does not support x86-64-v3` — whatever the application is.
>    UBI9 (what these nodes run) is v2 and safe.
> 2. **A Go binary built `GOAMD64=v3`** — SIGILL, exit 132, on the first instruction.
>
> Both are one command per image, before any Helm install, the moment credentials land:
>
> ```bash
> podman run --rm --entrypoint sh <image> -c 'grep ^PRETTY /etc/os-release; echo STARTS-OK'
> ```
>
> If the image is built above the baseline, that command fails by itself — no `STARTS-OK`, and the
> glibc or SIGILL message names which of the two it is. Clearing it is necessary, not sufficient:
> a hot path picked at runtime can still fault later, so Phase 0 is only green once one real token
> exchange has been driven end to end.

## 15. Spikes

| # | Spike | Pass criterion | If it fails |
|---|---|---|---|
| **S1** | **Federated token exchange**: register the cluster's OIDC discovery in IDSP, exchange a projected SA token for an agent token | agent token minted, `sub` traceable to the SA | fall back to per-agent client secrets and **document the posture regression** (§3) |
| **S2** | **Token size** in the query string, with mission + intent + `act` | request line < 12 288 B | trim via IDSP claim mapping. **If it cannot be trimmed, the integration is dead** — no fallback mode exposes claims to a DataScript (§1). Run this in Phase 1, before any other commitment |
| **S3** | **JWKS rotation**: rotate the IDSP signing key and watch | AKO re-fetches within the timer window, no sustained 401 | build the timer refresh before Phase 3 |
| **S4** | **Response-side constraints** in the existing `HTTP_RESP_DATA` DataScript | `maxRecordsReturned` truncates; assess `fieldsExcluded` honestly | drop §8; nothing lost, nobody enforces them today |
| **S5** | Delegated identity: human → exchange → `act` claim readable in the DataScript | claim present | drop delegation; additive |
| **S6** | **Multi-intent claim**: does `jwt_claim()` handle an array-valued claim, and what does set-membership cost in the sandbox? | an agent holding 3 intents authorizes correctly | §7.3 design A is dead; go to design B (tool list minted into the token) |
| **S7** | **`tools/list` filtering** in the response phase, using `isPublic` + the agent's binding set | an agent sees only tools it may call | leave discovery unfiltered and say so — the `tools/call` gate is the control that matters (§7.5) |
| **S8** | **Certificate-bound tokens (RFC 8705)** — PKI profile on an agent route, then `avi.ssl.client_cert_verified()` + `avi.ssl.client_cert(avi.CLIENT_CERT_FINGERPRINT)` compared to the token's `cnf` claim | a token presented without its client cert is rejected 403 | bind on `CLIENT_CERT_SERIAL` + `_ISSUER` if the fingerprint hash disagrees; if `avi.ssl` is absent on this build, the DPoP forfeit in §10.4 stands as written |

S1 decides whether this is an upgrade or a trade. S3 decides whether it is safe in front of the
whole estate.

## 16. What to ask Broadcom for

1. **Entitlement/license** for AgentMinder 4.1 and the IDSP 4.0 base platform.
2. **The interactive sizing spreadsheet.** The install pages themselves are public and are folded
   into §14.1; only the sizing sheet is behind the login.
3. **Registry credentials** — but scoped: the `ssp-infra` / `ssp` charts come from a public GCS
   bucket and name no pull secret, so confirm whether the *platform* images pull anonymously and
   credentials are needed only for the OTel-collector and NATS images (§14.1).
4. **Appendix B Postman collection.**
5. **Does token exchange accept a Kubernetes ServiceAccount JWT as `subject_token`**, with the
   cluster registered as a federated identity source? This is S1, and it is the single most
   important question in the integration.
6. **Claim shape**: which claims carry mission, mission type, intent scope and delegation, and
   whether per-client claim mapping exists so we can name and trim them (§6, S2).
7. **Key-rotation guidance**: cadence, external key-provider pinning, and whether the JWKS
   publishes overlapping keys during rotation — which would turn risk #1 from fatal to routine.
8. Whether `constraints` are exposed **in the minted token** or only in a PDP response. If the
   former, §8 is straightforward; if the latter, we need a claim-mapping rule.
9. **Can IDSP mint a computed claim** — the agent's resolved tool list (mission × authorization
   surface × tool bindings), flattened into the token? This is §7.3 design B and it decides
   whether MCP tool authz needs any CRD tables at all.
10. **The golden-config YAML schema, and the route `settings` / `policy` object schemas.** Neither
    is published. We have declined gateway registration on architectural grounds (§10.3), but the
    schema is what would let anyone re-open that decision on evidence rather than assumption — and
    it costs nothing to send. Ask once, file it.
11. How is **tool discovery** performed (`"source": "DISCOVERED"`) — does the control plane call
    `tools/list` on the resource server directly, and if so, from where? Our MCP servers sit behind
    the gateway with JWT auth, so discovery needs a path and a credential.

## 17. Open questions

- Does one IDSP tenant cover both clusters? A single issuer across the estate would also fix the
  cross-cluster subject-ambiguity bug in
  [ai-gateway-cross-cluster-auth.md](ai-gateway-cross-cluster-auth.md) §5 — an agent client id is
  globally unique in a way a bare ServiceAccount name is not. That is a second real win and worth
  confirming early.
- Should the **agent factory** call DCR at provision time (§3)? It already mints the
  ServiceAccount; this makes AgentMinder the registry of record rather than a parallel one.
- Does `AGENT_GROUP_DEFAULT`-style defaulting survive, and does the noisy-neighbour demo keep its
  own `noisy-agent` group? Whatever we do must preserve that — the demo depends on it.
- If S2 shows tokens are too large, is a thin claim-trimming shim in front of IDSP better than
  losing `jwtQuery`? That would put a small broker back in front of the broker, which is worth
  naming as the ugly outcome it is.
