# AKO AI Gateway — Release Notes

Every AI Gateway feature, newest first, from metric-weighted inference load balancing through to the
token ledger. Each entry states what shipped, what it changed, and what it does not do.

For the whole system in one place — architecture, console guide, how-tos, security posture — see the
[Handbook](ai-gateway-handbook.md).

## Feature index

| Capability | First shipped | State today | Reference |
|---|---|---|---|
| Metric-weighted inference load balancing | 2026-05-08 | Built · p90 TTFT ~125 s → ~10 s under load | [inference-extension.md](inference-extension.md) |
| `AIGatewayAuthPolicy` — OIDC / JWT at the SE | 2026-05-28 | Built · two modes (`oauthBrowser`, `jwtQuery`) | [ai-gateway-auth.md](ai-gateway-auth.md) |
| `AITokenRateLimitPolicy` — token budgets | 2026-05-28 | Built · per-group budgets, counters endpoint, epoch reset | [ai-gateway.md](ai-gateway.md) |
| Real OIDC SSO + per-group budgets | 2026-05-31 | Built | [ai-gateway.md](ai-gateway.md) |
| Token counters endpoint + console reset | 2026-06-06 | Built · claim-gated since 2026-08-16 | [ai-gateway.md](ai-gateway.md) |
| `AIModelRoutePolicy` — quality/cost tier routing | 2026-06-07 | Built · InferencePool, Service and Provider tiers | [model-routing.md](model-routing.md) |
| `AIMCPRoutePolicy` — MCP gateway | 2026-06-07 | Built · dedicated VIP, session affinity, role-based tool auth | [ai-gateway-mcp.md](ai-gateway-mcp.md) |
| `AIGuardrailPolicy` — WAF DLP & content guardrails | 2026-06-07 | Built · verified blocking on live Avi | [ai-gateway-guardrails.md](ai-gateway-guardrails.md) |
| `jwtQuery` auth mode for machine clients | 2026-06-08 | Built | [ai-gateway-auth.md](ai-gateway-auth.md) |
| `AIA2ARoutePolicy` — A2A gateway | 2026-06-17 | Built · resource binding + fail-closed switches since 2026-08-16 | [ai-gateway-a2a.md](ai-gateway-a2a.md) |
| Agent registry + `/.well-known/agents` | 2026-07-02 | Built | [ai-gateway-agent-registry.md](ai-gateway-agent-registry.md) |
| Semantic guardrails over ICAP | 2026-08-09 | Built · FP-hardened v2 | [ai-gateway-guardrails-semantic.md](ai-gateway-guardrails-semantic.md) |
| External provider tiers (Gemini) | 2026-08-09 | Built · verified live | [model-routing.md](model-routing.md) |
| Agent factory — NL description → provisioned agent | 2026-08-15 | Built | [ai-gateway-agent-factory.md](ai-gateway-agent-factory.md) |
| Per-agent ServiceAccounts + `POST /exchange` | 2026-08-16 | Built | [ai-gateway-agent-registry.md](ai-gateway-agent-registry.md) |
| Framework agent runtimes (Go + ADK) | 2026-08-18 | Built | [ai-gateway-agent-framework.md](ai-gateway-agent-framework.md) |
| Forgeable `GET /token` retired | 2026-08-19 | Built · returns `410 Gone` | [Handbook §6.2](ai-gateway-handbook.md#62-principals) |
| Token ledger — true per-user / per-agent consumption | 2026-08-19 | Built | [ai-gateway-token-ledger.md](ai-gateway-token-ledger.md) |
| Backend mTLS with SPIFFE/SPIRE | — | Design, spike-gated | [ai-gateway-backend-mtls.md](ai-gateway-backend-mtls.md) |
| Multi-site delivery via AMKO + GSLB | — | Design, spike-gated | [ai-gateway-multisite.md](ai-gateway-multisite.md) |
| Cross-provider failover | — | Design only | [ai-provider-failover-design.md](ai-provider-failover-design.md) |

---

## Token Ledger — true per-user and per-agent consumption — 2026-08-19

Adds an **accounting** path beside the existing **enforcement** path. Until now the console rendered the
SE's enforcement counter as if it were an accounting figure; the two want opposite things. The counter is
unchanged and still enforces budgets. The ledger is new and sits beside it.

Commits `ceed2d26`, `cedaf142`, `711e8293`, `c1209ece`, `928bb266`, `2f10c281`.
Full design: [ai-gateway-token-ledger.md](ai-gateway-token-ledger.md).

### New features

**One immutable usage record per metered response.** In `HTTP_RESP_DATA` the SE appends a pipe-delimited
record to a ring alongside incrementing the budget counter:

```
ts | identity | model | tier | prompt | completion | cached | reasoning | quality | route
```

Every value is reduced to a safe character set at write time, so the drain has nothing to escape. Field
order is the contract between the AKO and console repos.

**Sequence + ring + drain endpoint.** Records are written as `ai_urec:<seq>` with a 300-second TTL and a
head pointer. `GET /v1/admin/usage?after=<seq>` returns `{inst, head, next, lost, records:[…]}`. The
collector stores its cursor per SE instance, so a drain is idempotent and a missed poll is recoverable
inside the TTL window.

**Console collector with a durable store.** The console polls every ~5 s, stores records permanently, and
adds what only it can know: which SE ring a record came from, the `kind` (`user` / `agent` / `mcp` /
`unknown`), and a cost derived at read time from the model rate and the cached discount.

**`kind` resolved at read time, not stamped at ingest.** Resolution uses the issuer's persona list plus
the agent and MCP registries, so an agent registered *after* its first call is classified correctly for
its whole history rather than being stuck as `unknown`. Anything no registry knows appears as `unknown`
in its own panel rather than being dropped, so the kinds still partition the estate total.

**Dashboard ▸ Tokens.** 24-hour / 7-day / 30-day windows, tokens-over-time with a prompt / cached /
completion split, Users and Agents panels, per-identity cost, CSV export, and a footer naming how many SE
rings were drained and how many records arrived.

**Accuracy is stated, not assumed.** Reported consumption sums `quality = exact` rows only. Penalty rows
are shown as budget events, never as measured tokens, and a **measured exactly** percentage states what
fraction of the window is real measurement.

### Fixes

- **Fail-closed penalty charged on absence** (`711e8293`) — the penalty was applied whenever `usage` was
  missing, so a single `GET /v1/models` cost 65 536 tokens. It is now charged only on evidence that a
  metered response was truncated or unreadable, not on its absence.
- **Admin exemption is a path list** (`c1209ece`) — the SSO `SKIP_AUTHENTICATION` exemption was written as
  a single prefix; with both `/v1/admin/counters` and `/v1/admin/usage` live it has to be a list, or the
  second endpoint gets OAuth-redirected.

### Testing

A **Lua SE-sandbox stub** now runs the generated DataScript outside Avi, under both **Lua 5.1 and 5.3**,
against a production-shaped policy (`928bb266`, `2f10c281`). Run it before touching any DataScript — the
SE sandbox raises on things stock Lua tolerates (`tonumber(nil)`, absent `string.match`).

### Known limitations

- **Streaming still counts zero.** `text/event-stream` is never buffered, so streamed responses produce
  neither a counter increment nor a ledger record. Non-streaming meters exactly.
- **Per-SE rings.** Each SE keeps its own ring and answers only on its own VIP; the collector drains each
  one it knows about. An SE it cannot reach contributes nothing.
- **Ring TTL is 300 s.** A collector outage longer than five minutes loses records; `lost` reports it
  rather than hiding it.

### Upgrade notes

- Rolling AKO **500s the front door for roughly 90 seconds** while it reconciles. Wait and retry — do not
  roll back.
- The console and `agent-hub` images are digest-pinned; a plain restart reuses the old image.

---

## Token service hardening — `/token` retired — 2026-08-19

The token service stops being an endpoint that mints a subject on request.

### Changes

- **`GET /token` retired** — it accepted a caller-supplied subject and was therefore forgeable. It now
  returns `410 Gone`.
- **`POST /exchange`** is how workloads mint: RFC 8693 token exchange, subject derived from a Kubernetes
  TokenReview on the caller's projected ServiceAccount token, never from the request body.
- **`POST /persona`** is how console personas mint, with the subject derived from the verified IdP token.
- Eight images were rolled and verified against the new endpoints.

### Upgrade notes

- Any caller still using `GET /token` breaks with `410`. There is no compatibility shim — that is the point.
- `agent-hub` and `ai-gateway-ui` are **digest-pinned**: restarting them reuses the old image. Re-pin to
  roll them.

---

## Framework agent runtimes — ADK body — 2026-08-18

An agent's implementation becomes interchangeable. A `runtime` field on the `AgentSpec` selects the image;
everything the gateway enforces is identical either way, because the governed surfaces are the wire
contracts, not the code behind them. Commit `f51f6532`. Doc: [ai-gateway-agent-framework.md](ai-gateway-agent-framework.md).

### New features

- **`runtime` and `workflow` on `AgentSpec`** — `go` (compact flat tool-calling loop) or `adk` (Python
  Agent Development Kit, graph workflows with parallel specialists and a synthesis step).
- **First ADK agent live** — `incident-analyst` fans out three specialists and synthesises, in 27.5 s,
  SE-metered like any other caller.
- **Sub-agent grant inheritance** — each specialist is restricted to the MCP servers the parent agent was
  granted. Enforced when the spec is validated *and* again when the runtime loads its ConfigMap, because a
  ConfigMap can be edited after provisioning.
- **Per-request minting in both bodies** — a graph running for minutes crosses many 60-second windows, so a
  credential captured at construction time would expire mid-run.

### Known limitations

- **The framework's own A2A server is unusable** — it speaks a newer wire dialect than the estate does, and
  adopting it would invalidate every deployed allow-list. A hand-written shim serves the same
  `tasks/send` · `tasks/get` · `tasks/cancel` surface instead.
- **`McpToolset` is not the default** — the ported native MCP client is.
- **The real constraint is a 6144-token context**, not the framework.

---

## A2A enforcement hardening — 2026-08-16

Four changes that close the fail-open paths in A2A authorization, plus fixes around them. Commits
`d3737368`, `945c1ec0`, `81501c6d`, `ecb3be81`, `d3a07f64`, `1abb6f6f`, `69802148`, `97ead3d7`.

### New features

**Agent-card skill enforced at the SE** (`d3737368`) — the skill was previously checked only at mint time.
It is now matched again at the Service Engine, so the two authorization decisions are genuinely
independent: the token service decides whether a credential may exist, the SE decides whether the one
presented is good for this request.

**`requireMethod`** (`945c1ec0`) — reject any request from which no JSON-RPC method could be read. Closes
the non-JSON-RPC fail-open on agents that speak only A2A. Agent-card discovery is exempted beforehand, so
`/.well-known/agent.json` still works.

**Resource binding without splitting the audience** (`81501c6d`) — a token names one target agent in its
`target` claim, compared at the SE to the target the route serves. A token minted for another agent is
refused *before* its scope is considered. Because Avi validates `Audiences[0]` only, this could not be done
by audience partitioning.

**`authorizePaths`** (`ecb3be81`) — evaluate the allow-list on every request, adding request path as a third
matching dimension for agents whose REST endpoints are their interface. Opt-in, because a policy listing no
paths would otherwise begin denying every REST call the moment it was enabled.

**Claim-gated counters endpoint** (`69802148`) — `/v1/admin/counters` is authorized from a verified claim
via an annotation-driven admin auth path, rather than from a shared secret alone.

### Fixes

- **Never emit an SSO policy with zero authn rules** (`d3a07f64`) — an empty rule set is accepted by the
  Controller and authenticates nothing.
- **Counters endpoint outlives its shared secret** (`1abb6f6f`).
- **A Gateway rebuild no longer deletes its child virtual services** (`97ead3d7`).
- **Reverted "never meter read-only requests"** (`a2a848a0` → `651a065a`) — the exclusion was wrong; the
  correct fix landed later as the evidence-based penalty (`711e8293`).

### Upgrade notes

- Routes created before these switches existed are **fail-open on non-JSON-RPC GETs**. Set `requireMethod`
  (A2A-only agents) or `authorizePaths` (REST agents) on each.
- The audience flip is a **hard cutover**, not a config change: Avi matches `Audiences[0]` only.

---

## Estate build-out — factory, per-skill auth, MCP gateway, RAG — 2026-08-14 → 2026-08-16

The period where the agent surface stopped being a demo and became an estate. These pieces are documented
in their own design docs; summarised here for the timeline.

### Agent factory (2026-08-15)

Natural-language description → drafted `AgentSpec` → one approval provisions the ConfigMap, ServiceAccount,
Deployment, Service, HTTPRoute on the A2A gateway, `AIA2ARoutePolicy` and registry entry as a single unit.
Key call: **one config-driven agent-runtime image**, no per-agent builds. Doc:
[ai-gateway-agent-factory.md](ai-gateway-agent-factory.md).

### Per-skill agent auth (2026-08-16)

Per-agent ServiceAccounts plus `POST /exchange`: TokenReview → mint-time authorization → a 60-second token
carrying audience, skill and `jti`. This is the first of the two independent authorization decisions.

### Dedicated MCP gateway (2026-08-15)

All four MCP servers moved onto a dedicated MCP gateway VIP with `jwtQuery` auth — unauthenticated calls
now get `401`. Agents followed via DNS with zero edits. The migration briefly broke `agent-hub`'s MCP
children, which sent no JWT; fixed in the same window.

### MCP servers built

- **`k8s-logs`** — read-only pod logs from a single namespace, RBAC-scoped, SE-fronted, no write access.
- **`nmap-mcp`** — unprivileged TCP-connect scanner scoped to one subnet, egress-locked, `-sT` pinned.
- **`rag`** — semantic `code_search` / `read_file` over the estate's own GitHub source. Its embedding calls
  ride the front door as `sub=rag-service`, so the RAG service is metered like any other consumer and
  appears on Dashboard ▸ Agents as an MCP tool card with a token badge.

### Known limitations

- **The provisioner cannot create each agent's ServiceAccount** — its ClusterRole is missing
  `serviceaccounts` and `services:patch`, so a new agent sits at `FailedCreate` with no pod. Workaround:
  `oc create sa <name>` then restart the rollout.
- **Registry entries are ConfigMaps.** An apply that overwrites the ConfigMap silently drops hand-added
  entries — keep entries in the manifest and never hand-patch the live object.

---

## External provider tiers — 2026-08-09

`AIModelRoutePolicy` tiers can now target an **external provider API** — a public endpoint such as
`generativelanguage.googleapis.com` — reached SE-native over egress, alongside `InferencePool` and core
`Service` tiers. Commits `b0cdf433`, `135f2045`. Verified live against Gemini.

The path rewrite between the OpenAI-style front-door path and the provider's own path is done with
`avi.http.set_path`. Selection, entitlement and budgets behave exactly as for in-cluster tiers, so an
external model is governed by the same policy objects as a local one.

---

## Semantic guardrails — prompt-injection classifier over ICAP — 2026-08-09

The model-based half of `AIGuardrailPolicy`, catching the novel and paraphrased injection that signatures
provably miss. Commits `88ccb197`, `d77336be`. Doc:
[ai-gateway-guardrails-semantic.md](ai-gateway-guardrails-semantic.md).

### New features

- **`AIGuardrailPolicy.semantic`** — AKO authors the Avi `icapprofile` and the security-policy rule from
  the CRD.
- **The no-proxy property is preserved** — the SE buffers the request body and *calls* the classifier as a
  service over ICAP (RFC 3507), exactly as it already calls the OIDC issuer, then the SE enforces the
  block. The model is never in the request path.
- **A pure-stdlib ICAP shim** bridges ICAP to the classifier, which speaks HTTP.
- **Built and verified end-to-end** on live Avi 31.2.1.

### Fixes

- **FP-hardened to v2 (2026-08-14)** — the benign-anchor set now covers imperative-but-benign prompts
  (counting tasks, output-format constraints, ordinary agent and tool traffic) that had been false
  positiving, plus a **gray-zone LLM-judge cascade** for uncertain scores. 7/7 live cases correct after the
  change.

### Known limitations

- **A pod reschedule requires re-running `attach-icap.sh`**, with roughly three minutes fail-open in
  between.
- **Classifier-verdict routing was disproven on 31.2.1** (`442c9f4a`) — the ICAP-header mechanism that
  would let a route decide on the classifier's verdict does not work on this build.

---

## Model routing — Service tiers, alias discovery, tenancy — 2026-08-08

Three changes to `AIModelRoutePolicy` and its reconciliation. Commits `dc2c768d`, `10be0ab5`, `cb1a1d7a`.

- **Core `Service` tier backends** (`dc2c768d`) — a tier can point at a plain Service, including a
  selectorless Service with a manual EndpointSlice used to reach an external endpoint through the SE. This
  removed the InferencePool-only limitation noted in the original model-routing release.
- **Pod-label model alias discovery** (`10be0ab5`) — model aliases are discovered from pod labels rather
  than restated in the policy.
- **Tenant-aware policy objects + cache-populate crash guard** (`cb1a1d7a`).

---

## Guardrails — signature hardening — 2026-07-31 → 2026-08-08

- **Word-boundary keyword rules and split prompt-injection tiers** (`7f1c3ab3`) — keyword rules were
  matching inside longer words; the injection signature set is now split into tiers so a high-confidence
  match and a weak heuristic are not treated alike.
- **Private-key signature exceeded the Avi WAF regex size limit** (`ff2080b0`) — the detector was rejected
  by the Controller at attach time, silently leaving private keys undetected. Split to fit.

---

## A2A Agent Registry — ConfigMap catalog + console view + federation endpoint — 2026-07-02

Implements the **Agent Registry**: a ConfigMap-backed catalog of approved A2A agents, a console Agent
Registry page, and a `/.well-known/agents` federation endpoint. This is the direct A2A counterpart to the
MCP server registry. Full doc: [ai-gateway-agent-registry.md](ai-gateway-agent-registry.md).

### New features

**`agent-registry` ConfigMap catalog.** Each entry (`AgentInfo`) carries `name`, `cardURL`, `skills[]`, an
optional `health` URL probed at read time, `scope`, `auth`, `approved` and `description`. The `reachable`
field is computed at read time and never persisted. Deliberately a ConfigMap rather than a CRD — no
controller reconciliation, auditable with standard tooling.

**Console Agent Registry page.** A table (Agent / AgentCard / Skills / Scope / Auth / Status) with a live
reachability dot per entry, and a **+ Add Agent** form that upserts by name via `POST /api/agentregistry`.

**`/.well-known/agents` federation endpoint.** Returns only entries where `approved: true`, as
`{name, card_url, skills, description}`. Served via an HTTPRoute on the A2A Gateway with no
`AIA2ARoutePolicy` attached — public discovery, no auth required.

### Behaviour notes

- `approved: false` — the agent appears in the console but is excluded from `/.well-known/agents` and has
  no gateway route.
- `approved: true` — the agent is published; a separate `AIA2ARoutePolicy` is still required to create an
  enforced gateway route.
- The registry is catalogue and discovery; per-agent enforcement stays in `AIA2ARoutePolicy`.

### Upgrade notes

- The `ai-gateway-ui` ClusterRole must grant `configmaps` verbs `create,update,patch` (previously
  `get,list` only) for the upsert write path.

---

## Guardrails — SSN detector hardened, secret library documented — 2026-07-02

### Changes

**SSN regex tightened to boundary + validity ranges.** The `ssn` detector now uses word-boundary anchors
and excludes invalid Social Security Number ranges (area `000`, `666`, `900-999`; group `00`; serial
`0000`):

```
\b(?!000|666|9[0-9]{2})[0-9]{3}-(?!00)[0-9]{2}-(?!0000)[0-9]{4}\b
```

This cuts false positives from arbitrary nine-digit strings while still catching every structurally valid
SSN. The previous detector matched any `\d{3}-\d{2}-\d{4}`.

**Built-in secret signature set documented.** The full detector set shipped in `guardrail_waf.go` is now
documented in [ai-gateway-guardrails.md](ai-gateway-guardrails.md) §6. Detectors added since initial
documentation:

| Detector | Regex (illustrative) |
|---|---|
| `gcp-api-key` | `AIza[0-9A-Za-z_-]{35}` |
| `github-token` | `gh[pousr]_[A-Za-z0-9]{36}` |
| `github-fine-grained-pat` | `github_pat_[A-Za-z0-9_]{82,}` |
| `slack-token` | `xox[baprs]-[0-9A-Za-z-]{10,}` |

Specify these in `AIGuardrailPolicy.spec.detectors.secrets[]` by name.

---

## Guardrails — WAF `jwtQuery` exclusion (`!ARGS:jwt`) — 2026-07-02

Resolves a conflict between the `AIGuardrailPolicy` WAF and `jwtQuery` authentication.

### Problem

Request-phase guardrail rules targeted `ARGS|REQUEST_BODY`, which includes query-string parameters.
`jwtQuery` carries the bearer JWT in `?jwt=`. With both active on the same route, the WAF matched the
built-in `jwt` detector pattern (`eyJ…`) against the query-param value and blocked **every** authenticated
request with a 403.

### Fix

The request-phase rule target is now `ARGS|REQUEST_BODY|!ARGS:jwt`. The SE validates the `jwt` param via
its own OAuth resource-server path; its raw bytes are not user prompt content and should not be DLP
scanned. Commit `591de2438` / `723cf862` in `ako-gateway-api/aigateway/guardrail_waf.go`.

### Upgrade notes

- An image built from a branch **without** this commit will 403 every `jwtQuery`-authenticated request on a
  guardrailed route. Verify the commit is present.
- No CRD or manifest changes required.

---

## Provider Failover — design doc added — 2026-07-02

A design document for **AI provider failover** at [ai-provider-failover-design.md](ai-provider-failover-design.md).
**Design only — no code.** Covers cross-provider failover strategies (active/passive, active/active
cost-weighted, latency-based), the Avi mechanisms that would underpin them (Pool Groups, health monitors,
DataScript routing), and the open questions. Listed here for traceability; not available in any build.

---

## A2A Gateway — `AIA2ARoutePolicy` — 2026-06-17 → 2026-06-30

The third governance surface: agents calling other agents over the Agent2Agent protocol (JSON-RPC over
HTTPS). Commits `72f6fd2a`, `80d36e07`, `6b88ba6a`, `5fbb575e`, `2034cc49`, `7eecb3be`, `9507a3c7`. Doc:
[ai-gateway-a2a.md](ai-gateway-a2a.md).

Unlike MCP — which Avi 32.1.1 supports natively — A2A has no native Avi support and is built from generic
primitives plus DataScripts, making it architecturally closer to model routing than to the MCP gateway.

### New features

- **`AIA2ARoutePolicy` CRD** — per-skill authorization, task affinity, agent RBAC, push-notification egress
  governance.
- **`ApplyA2ARoutePolicy` translator** — attaches the generated A2A DataScripts to the child EVH virtual
  service.
- **Task / context affinity** — `HTTP_RESP_DATA` captures the new task id and records the pool and server
  that answered, so later calls about that task land on the same backend for the configured `taskAffinity.timeout`.
- **Agent-card discovery is never body-inspected** — `/.well-known/agent.json` returns early in `HTTP_REQ`.

### Fixes

- **A2A RBAC reads the agent claim from the `?jwt=` token in `jwtQuery` mode** (`5fbb575e`) — previously it
  looked for a claim the SE had already stripped.
- **AKO ClusterRole granted `aia2aroutepolicies`** (`9507a3c7`) — without it the informer was forbidden from
  listing the CRD.

### Known limitations at the time

`agentAccess` was fail-open on requests carrying no JSON-RPC method. Closed on 2026-08-16 by `requireMethod`
and `authorizePaths` — see that entry.

---

## `jwtQuery` authentication mode — 2026-06-08

Adds `spec.authMode: jwtQuery` to `AIGatewayAuthPolicy`, so **machine clients** — SDKs, agents, MCP servers,
`curl` — can authenticate while their claims stay readable to policy. Commit `e1c3673a`. Doc:
[ai-gateway-auth.md](ai-gateway-auth.md).

### Why it exists

Avi's two JWT-validation paths are mutually exclusive on this build and only one exposes claims to a
DataScript. `CLIENT_OAUTH` (the browser flow) exposes claims but ignores a client-presented
`Authorization: Bearer` and 302-redirects. `SSO_TYPE_JWT` (the resource-server path) validates a bearer
correctly with 200/401 but strips the `Authorization` header before any DataScript runs, and
`oauth_get_claim()` returns nil.

`jwtQuery` configures `SSO_TYPE_JWT` with `jwt_location = JWT_LOCATION_QUERY_PARAM`. The SE validates the
token from `?jwt=`; unlike the header, the query parameter survives to the DataScript, which decodes the
same already-validated token. The decode is trustworthy because the SE verified signature, `aud` and `exp`
first — and AKO never emits the decode helper on a VS that is not enforcing `jwt_config`.

### Behaviour

| | `oauthBrowser` (default) | `jwtQuery` |
|---|---|---|
| Client | Browser / interactive | Machine (SDK, agent, curl) |
| Token presentation | OAuth auth-code → session cookie | `?jwt=<token>` query param |
| Unauthenticated response | `302` → `/authorize` | `401`, no redirect |
| Claims in DataScripts | `oauth_get_claim()` | base64url-decode the query token |
| Avi objects | Pool + `AUTH_PROFILE_OAUTH` + `SSO_TYPE_OAUTH` + `oauth_vs_config` | `JWTServerProfile` + `AUTH_PROFILE_JWT` + `SSO_TYPE_JWT` + `jwt_config` |

AKO — which, unlike the SE, has cluster network access — fetches the JWKS from `jwksUri` and embeds it in
the `JWTServerProfile`. Both modes require the listener to terminate TLS.

### Known limitations

- **Token in the URL.** RFC 6750 §2.3 permits this only when the header and body are impossible, which is
  exactly the condition here. Mitigations: mandatory TLS, short-lived tokens, SE query-param log redaction.
- **The token is forwarded upstream.** The SE passes the query string to the backend, so the token can land
  in *its* logs. A query-strip before the pool is the remaining hardening step; the exact SE query-rewrite
  primitive is unconfirmed, so AKO does not emit it automatically.
- Intended for machine-to-machine traffic on internal TLS paths, not public browser clients.

---

## Guardrails & DLP — `AIGuardrailPolicy` — 2026-06-07

Content inspection on the data plane already in place: the SE's native **WAF** (ModSecurity-based) does the
regex matching and request/response-body inspection. No proxy, no sidecar, no model in the path. Commits
`e5a706f7`, `27a190c2`, `c9302827`. Doc: [ai-gateway-guardrails.md](ai-gateway-guardrails.md).

### New features

**Pre-canned profiles per surface** — `BlockLLM` (DLP + prompt-injection signatures), `BlockMCP` (DLP +
tool abuse: command injection, path traversal, SSRF) and `BlockLLMAndMCP`. An operator drops one CR per
route or gateway; AKO authors the Avi WafPolicy from it and attaches it to the route's virtual service.

**Built-in signature library** — secret detectors (AWS/GCP/OpenAI/GitHub/Slack keys, private keys, JWTs),
PII detectors (SSN, credit card, email), prompt-injection patterns and MCP tool-abuse patterns, plus
per-policy keyword denylists and custom regex.

**Obfuscation-resistant injection detectors** (`27a190c2`) — each runs a normalised pass (lowercase,
URL/unicode decode, whitespace removal) *and* a base64-decode pass, so spaced-out text, zero-width tricks
and `%`-encoding are caught.

**Mode delegation** keeps `Block` vs `Log` (shadow) a per-rule flip — this is what the console's per-gateway
DLP toggle drives.

**Verified by spike** — an AWS key, an SSN or an API secret in a prompt returned `403` while a clean prompt
passed, on live Avi 31.2.2.

### Fixes

- Tightened credit-card detection, added the GitHub fine-grained PAT format, corrected OpenAI key formats
  (`c9302827`).

### Known limitations

Signature and regex based: it catches known patterns and cannot do semantic detection. That gap is what the
semantic guardrail entry (2026-08-09) closes.

---

## MCP Gateway — `AIMCPRoutePolicy` — 2026-06-07

Extends the same governance to **agent↔tool** traffic. Commits `e6c45634`, `9c03a752`, `bea755c7`,
`f62e721d`, `750094e5`. Doc: [ai-gateway-mcp.md](ai-gateway-mcp.md).

### New features

**A dedicated MCP Gateway** fronting tool servers, keeping stateful agent sessions pinned to the right
backend using Avi 32.1.1's native MCP session awareness, and bringing tool traffic under the same identity
and policy model as the LLM gateway.

**Same-IdP authentication.** An agent calling a tool authenticates exactly as it calls a model — a bearer
JWT in `jwtQuery` mode validated at the SE against the same issuer — so one agent carries one verified
identity across both its inference and its tool calls, with the same `sub` / `group` / `role` claims in
scope for both.

**Role-based per-tool authorization.** `AIMCPRoutePolicy` maps roles to the tools they may invoke, so one
MCP server can expose different tool subsets to different job roles, and a call to a tool the caller's role
does not hold is rejected before it reaches the backend.

**Two stacked gates.** The **approved MCP registry** is the server-level allow-list (which tool servers may
be fronted at all); role-based tool auth is the call-level gate (which tools a given caller may invoke on
them).

**EVH-safe MCP session affinity + route-derived OAuth callback** (`f62e721d`).

### Fixes

- Reference `System-Standard-MCP` as a full `vsdatascriptset` ref (`bea755c7`).

---

## Model-Based (Quality/Cost Tier) Routing — 2026-06-07

Adds **`AIModelRoutePolicy`** — route inference requests to different backends based on the requested
**model**, organised into quality/cost **tiers**. Like the other AI Gateway policies it attaches to an
`HTTPRoute` via `targetRef` and runs **entirely on the Avi Service Engine**. Verified end-to-end on a live
cluster (Avi 31.2.2). Doc: [model-routing.md](model-routing.md).

### New features

**Route by request `model` → per-tier `InferencePool`.** The SE reads the OpenAI-style `model` from the
request body and selects that tier's Avi Pool Group directly:

- `HTTP_REQ` enables request-body buffering (`set_request_body_buffer_size`, 32 KB); `HTTP_REQ_DATA` reads
  it (`get_req_body`), extracts `model`, resolves the tier (exact name or trailing-`*` prefix glob, else
  `defaultTier`), and calls `avi.poolgroup.select(<tier Pool Group>)`.
- Each tier's `InferencePool` keeps its scraper-weighted pod members — model routing chooses the *tier*, the
  scraper chooses the *pod*.
- Backends are declared **in the policy** (`tiers[].backendRef`), so a missing or unreconciled policy
  degrades to the route's own backend: no tiering, no outage.

**Group-based tier entitlement.** Reuses the verified `group` claim from `AIGatewayAuthPolicy`: a caller who
requests a tier above their entitlement is **downgraded** to their best allowed tier, or rejected, per
`onUnentitled`.

**Per-tier token budgets.** `AITokenRateLimitPolicy` limits can set their ceiling by tier via
`groupHeader: "reqvar:ai_tier"` + `groupBudgets`. The model-route script sets the `ai_tier` reqvar; because
the tier is known only after the body is read, such limits enforce in `HTTP_REQ_DATA`.

### Fixes

- **ClusterRole RBAC** — the AKO `ako` ClusterRole now grants `aimodelroutepolicies` (+ `/status`); without
  it the informer was forbidden from listing the CRD.

### Known limitations at the time

- **InferencePool tier backends only** — `Service` backends were accepted by the schema but not built.
  *Resolved 2026-08-08* (`dc2c768d`), which also added Provider tiers on 2026-08-09.
- **32 KB request-body buffer** — `model` is at the JSON start so the head suffices; larger bodies are not
  validated.
- Inherits the token policy's eventually-consistent counters and soft RPS.

### Upgrade notes

- Install the CRD: `kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aimodelroutepolicies.yaml`.
- Requires `inferenceExtension.enabled: true` for InferencePool tier backends.
- Additive and **gated** — existing routes are unaffected until an `AIModelRoutePolicy` targets them.

---

## Token Counters, Dashboard Reset & UI Integration — 2026-06-06

A read-only usage API and a one-click reset, wired into the console. All of it runs **entirely on the Avi
Service Engine**. Commits `021b026c`, `064410c9`, `4799c5d2`, `4348421b`.

### New features

**Read-only token-counters endpoint.**

- `GET /v1/admin/counters?users=alice,bob,carol` with header `X-Admin-Token: <token>` →
  `200 {"window":<sec>,"limit":"<name>","counters":[{"user":"alice","used":300}, …]}`
- Missing or wrong token → `403 {"error":"forbidden"}`
- Reads the **same SE counter key** the response-phase accounting writes, so the numbers match enforcement
  exactly.
- The SE table cannot be enumerated, so the caller passes the identities it wants in `?users=`.
- **Opt-in:** set `ai.ako.vmware.com/admin-token-secret: <secret>` on the `AITokenRateLimitPolicy` (Secret
  key `token`). When set, AKO emits the endpoint and adds an SSO `SKIP_AUTHENTICATION` rule for `/v1/admin/`
  so it is not OAuth-redirected. When absent, the endpoint is not generated.

**Counter reset via epoch bump.** Bump `ai.ako.vmware.com/counter-epoch` to move every counter to a fresh
keyspace — an instant reset that leaves budgets and the limit name untouched.

```bash
kubectl annotate aitokenratelimitpolicy llm-limits -n inference \
  ai.ako.vmware.com/counter-epoch=2 --overwrite
```

**Console: live counters + one-click reset.** The UI reads authoritative per-user usage from the endpoint
and resets all counters with one button. Reset no longer renames the limit — the name stays stable.

**OIDC issuer `/users` roster.** The issuer exposes its identity→group map so the console knows which users
to query without hardcoding them.

### Fixes

- **Counters endpoint 500 on unseen users** — the SE Lua sandbox raises on `tonumber(nil)`; a queried user
  with no counter yet crashed the request. Now guarded (`tonumber(lookup or 0)`).
- **OAuth issuer pool stale IP** — when the in-cluster OIDC issuer pod re-IPs, the immutable OAuth
  pool/authprofile pinned to the dead IP broke login fabric-wide; rebuilt against the live endpoint.
- **DataScript content changes detected in the checksum** (`064410c9`) — content edits were not triggering a
  republish.
- **Token metering gated with `set_reqvar`/`get_reqvar` across `HTTP_RESP` → `HTTP_RESP_DATA`** (`4799c5d2`).

### Known limitations

- **Streaming responses are not token-metered.** With `stream: true` the DataScript cannot read the body
  without buffering, and buffering collapses streaming, so streamed requests bypass the budget (count 0).
  Use non-streaming where budgets must hold. The proper fix is a native SE capability — tracked as an RFE.
- **Counters are per-SE and eventually consistent.** With multiple SEs a budget can overshoot by roughly the
  SE count. Exact fabric-wide budgets need a native distributed counter — RFE.
- **The OAuth issuer is a single pod.** Restarts re-IP and break the pinned OAuth pool until reconciled.

### Upgrade notes

- The counters endpoint is **off by default**; existing policies are unaffected until the annotation and its
  Secret are added.
- No CRD schema changes — both controls are annotations on the existing `AITokenRateLimitPolicy`.

---

## Real OIDC SSO and per-group token budgets — 2026-05-31

Commits `3d03fc53`, `13ca9d63`, `d7c30f95`, `92cebe11`, `0d23c8b3`, `2064108e`, `523835e7`.

### New features

- **Real OIDC SSO via Avi OAuth** — AKO builds the issuer Pool, `AUTH_PROFILE_OAUTH` and `SSO_TYPE_OAUTH`
  and sets `oauth_vs_config` on the child VS. No manual OAuth setup on the Controller.
- **Per-group token budgets** — budgets keyed on the verified `group` claim, so different tiers of user get
  different ceilings.
- **Token usage parsed from the response body** (`d7c30f95`) — `HTTP_RESP_DATA` reads the `usage` object,
  dropping the previous dependency on a response header that model servers do not reliably send.
- **DataScripts published for EVH child virtual services** (`523835e7`) — required for the EVH object model
  the gateway uses.
- **Idempotent `VSDataScriptSet` create + JWT validation config on the VS** (`0d23c8b3`).

### Fixes

- **Body-parse hardened** (`92cebe11`) — 256 KB buffer, POST/JSON gating, fail-closed on unparseable bodies.
- **Lua DataScript generation fixed for the Avi SE runtime** (`2064108e`) — the SE sandbox rejects
  constructs stock Lua accepts.

---

## AI Gateway Phase 1 — `AIGatewayAuthPolicy` and `AITokenRateLimitPolicy` — 2026-05-28

Commit `cc2d4edf`. The two founding CRDs, and the shape every later policy follows: a Kubernetes object with
a `targetRef` onto a Gateway API route, reconciled by AKO into native Avi objects, enforced entirely on the
Service Engine.

- **`AIGatewayAuthPolicy`** — JWT validation at the SE, with the verified claims made available to
  downstream policy. This is the trust anchor everything else keys on.
- **`AITokenRateLimitPolicy`** — token budgets and request-rate limits over a window, keyed on the identity
  authentication established.

Docs and a demo kit landed alongside (`5224d8ca`, `96ba9210`).

---

## Inference Extension — metric-weighted load balancing — 2026-05-08 → 2026-06-02

The foundation everything else routes *over*. An `InferencePool` groups the pods serving a model, and the
gateway distributes requests across them using live metrics scraped from each pod — **KV-cache utilisation,
request-queue depth and running-slot occupancy** — so load follows real serving capacity rather than
round-robin.

Model servers are GPU-bound and their cost per request is dominated by tail latency. In internal
benchmarking under load, replacing round-robin with the metric-weighted algorithm cut **p90
time-to-first-token from roughly 125 s to roughly 10 s**.

### Notable changes across the series

- **Three-term scoring formula** (`d6467fbd`, `cc2d796f`, `ae59d2d2`) — waiting-queue depth normalised
  against the pool maximum, `WaitingSustainedStreak` and `maxNumSeqs` wired into the scoring pipeline.
- **Pool members reconciled on pod events for any CNI** (`297a86df`) — previously NodePortLocal only.
- **NPL / NodePort / hostPort resolution for SE reachability** (`79ef49dc`, `5dab7993`, `f145a08d`) — the SE
  lives outside the cluster and needs a routable node:port for each member.
- **Bootstrap members with equal weights before the first scrape** (`e980d35b`) — an empty pool otherwise
  black-holed traffic at startup.
- **Scraper interval lowered from 15 s to 5 s** (`de894432`); **HTTP keep-alives enabled** (`804dc621`).
- **KV-cache metric falls back to `vllm:gpu_cache_usage_perc`** (`2bb40ade`).
- **Deterministic scenario table in `weights_test.go`** (`bd5dc48a`, `481ecbff`).

Docs: [inference-extension.md](inference-extension.md) · [inference-install.md](inference-install.md).
