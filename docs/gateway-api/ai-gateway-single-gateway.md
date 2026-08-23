<!--
  DESIGN. Answers one question: can a single LLM gateway, a single MCP gateway and a
  single agent gateway serve every cluster in an estate, with the workloads staying where
  they are? Sections marked ⚠️ are design or unmeasured; everything else describes code in
  this repository, cited by file.

  Companion documents:
    ai-gateway-datacenter.md  — the same idea for the LLM surface only, sized at 15 clusters
    ai-gateway-multisite.md   — GSLB above this layer, for multiple datacentres
    ai-gateway-mcp.md / ai-gateway-a2a.md — the MCP and A2A surfaces themselves
-->

# AKO AI Gateway — One Gateway per Surface, Many Clusters

> **Status: design, built on shipped parts.** The LLM half is a configuration of code that
> is live today plus one CRD extension. The MCP and agent halves are possible today but
> constrained by one data-plane primitive (§5.4), and the honest answer for them is a
> two-hop shape until an RFE lands.

**The question.** An estate has *N* Kubernetes clusters. Rather than *N* AI gateways, can it
have **three** — one for LLM traffic, one for MCP tool traffic, one for agent-to-agent
traffic — each fronting workloads that stay in their own clusters?

**The short answer.** Yes, and "three" is the right target. But *single gateway* can only
mean *single owning cluster* (§2), and the three surfaces are not equally ready: LLM traffic
is stateless and flattens cleanly, while MCP sessions and A2A tasks pin to a backend server
and that pin is exact only for single-server pools (§5.4). That one fact decides the whole
topology.

---

## 1. Scope

| In scope | Out of scope |
|---|---|
| One Avi Controller, one datacentre, *N* clusters | Multiple datacentres — that is GSLB, see `ai-gateway-multisite.md` |
| Three surfaces: `llm`, `mcp`, `agent` | Non-AI ingress; ordinary HTTPRoutes are unaffected |
| Where policy lives and where traffic lands | Which models or agents an estate should run |

Throughout, **entry cluster** means the cluster whose AKO owns a front door's Avi objects,
and **serving cluster** means a cluster that runs workloads and no AI policy.

---

## 2. What "a single gateway" can actually mean

This is the load-bearing constraint and everything else follows from it.

AKO stamps every Avi object it creates with `created_by = ako-gw-<cluster>-<namespace>`
([`internal/lib/lib.go:531`](../../internal/lib/lib.go#L531)) and scopes **every cache read
back to that same string** — virtual services, pools, pool groups, DataScripts, policy sets
([`internal/cache/controller_obj_cache.go:2390`](../../internal/cache/controller_obj_cache.go#L2390)
and the ~20 sibling queries around it). Two AKOs pointed at one Controller therefore inhabit
two disjoint universes. Neither can see, extend, or safely share the other's virtual service.

So there is no version of this design where fifteen clusters collectively own one Gateway.
What is achievable — and is what the rest of this document means by *a single gateway* — is:

- **one `Gateway` object, in one cluster, per surface**, whose AKO owns the VIP, the parent
  VS, all child VSes, the pools and the DataScripts;
- **every `HTTPRoute` and every AI policy CR for that surface in that same cluster.** Routes
  may come from any namespace (`allowedRoutes.namespaces.from: All` is supported —
  [`ako-gateway-api/k8s/validator.go:116`](../../ako-gateway-api/k8s/validator.go#L116)) but
  not from any other cluster;
- **workloads anywhere**, reached by one of the three mechanisms in §4.

The consequence is organisational before it is technical, and it belongs at the top of any
review: **route authorship centralises.** A team in cluster `n07` no longer publishes its own
route from its own cluster; it opens a pull request against the entry cluster's repository.
For an estate that already runs AI policy centrally that is a feature. For one where each
cluster is a sovereign tenant it is the reason to choose the hybrid in §8.1.

---

## 3. Topology

```
                    ┌──────────────── ONE AVI CONTROLLER ────────────────┐
                    │            tenant(s) per environment               │
                    └────────────────────────────────────────────────────┘

  ENTRY CLUSTER (control plane only — no inference traffic touches its nodes)
  ┌───────────────────────────────────────────────────────────────────────────┐
  │  Gateway llm-gw      Gateway mcp-gw        Gateway agent-gw               │
  │  + HTTPRoutes        + HTTPRoutes          + HTTPRoutes                   │
  │  + AIModelRoute      + AIMCPRoute          + AIA2ARoute                   │
  │  + AITokenRateLimit  + AIGuardrail         + AIGatewayAuth                │
  └───────────────────────────────────────────────────────────────────────────┘
            │ AKO reconciles ↓                    (one writer, one created_by)

   clients ─►┌────────────────┐   ┌────────────────┐   ┌────────────────┐
             │ llm.ai.dc1     │   │ mcp.ai.dc1     │   │ agent.ai.dc1   │
             │ VIP A          │   │ VIP B          │   │ VIP C          │
             │ ────────────── │   │ ────────────── │   │ ────────────── │
             │ WAF · ICAP     │   │ WAF · ICAP     │   │ WAF · ICAP     │
             │ JWT            │   │ JWT            │   │ JWT            │
             │ model → tier   │   │ tool RBAC      │   │ agent+skill    │
             │ budget · meter │   │ session pin    │   │ RBAC · target  │
             │                │   │                │   │ task pin       │
             └───────┬────────┘   └───────┬────────┘   └───────┬────────┘
                     │                    │                    │
   ┌─────────────────┴────────────┬───────┴──────────┬─────────┴───────────┐
   ▼                              ▼                  ▼                     ▼
 cluster c01                  cluster c02        cluster c03           cluster cNN
 model servers                MCP servers        agents                 mixed
 (AKO + HTTPRoute, or nothing at all — see §4)
```

Three Gateways, not one with three listeners: a surface is a different VIP, a different SE
group if you want the blast radius separated, and a different policy set. They share
one **issuer** per environment because identity is estate-wide — though the policy object itself
is per route, not per gateway (§6.2). They share nothing else.

---

## 4. How a central gateway reaches a workload in another cluster

Backends are resolved by the owning AKO from **its own** cluster's EndpointSlices. Three
mechanisms exist to cross the boundary, and the choice is per surface, not per estate.

| # | Mechanism | Hops | Serving cluster runs | State |
|---|---|---|---|---|
| **A** | **FQDN pool.** Pool server is a *name*; `resolve_server_by_dns` hands every resolution to the SE; a health monitor probes it | 2 | AKO + its own VS + a route | **Built, verified cross-cluster 2026-08-23** — [`modelroute_remote_rest.go:28`](../../ako-gateway-api/aigateway/modelroute_remote_rest.go#L28) |
| **B** | **Selectorless Service + hand-maintained EndpointSlice.** Real pool members whose addresses are remote pods (or NPL `node:port`) | 1 | nothing — just workloads | Works today, no code change. Called out as a first-class shape at [`avi_model_route.go:118`](../../ako-gateway-api/nodes/avi_model_route.go#L118) |
| **C** | GSLB / DNS | — | — | **Rejected here.** DNS resolves before a request body exists, so it cannot route on `model`, on a tool name, or on a task id. It is the right tool one layer up (`ai-gateway-multisite.md`) |

### 4.1 Mechanism A — the peer is a name

```
AIModelRoutePolicy tier "premium"
   └── pool   …-premium-remote-pool     server = llm.c01.ai.dc1  (resolve_server_by_dns)
   └── hm     GET /v1/models  WITH a Host header
   └── pg     …-premium-remote-pg       ← the DataScript selects this by name
```

Nothing in the entry cluster tracks the peer's address: AKO resolves the name **once** to
seed the server object, because the Controller rejects a server without an `ip` even when
`hostname` and `resolve_server_by_dns` are set, and every resolution after that belongs to
the Service Engine ([`ensureFQDNPool`](../../ako-gateway-api/aigateway/modelroute_remote_rest.go#L84)).
No EndpointSlice to maintain, no credentials for the peer's kube API, no controller loop —
and a renumbered peer VIP fixes itself.

The price is a second hop and a virtual service in the serving cluster. Which, for MCP and
A2A, turns out to be a benefit (§5.4).

### 4.2 Mechanism B — the pool member is a remote pod

The entry cluster holds a `Service` with no selector and an `EndpointSlice` written by hand,
whose addresses are pods in another cluster. AKO builds pool members from the slice exactly
as it would for a local Service. This is the only mechanism that yields a **truly** single
gateway: the other clusters own no Avi object at all.

Two costs, both real:

1. **Someone must keep the slices true.** A sync controller, or the Multi-Cluster Services
   API (`ServiceExport`/`ServiceImport`). This is new software in the estate — the honest
   comparison is "one sync controller" versus "N-1 thin virtual services".
2. **The SE must reach the pod.** A routable pod network, a NodePort, or Antrea
   NodePortLocal, which AKO already understands
   ([`gateway_controller.go:266`](../../ako-gateway-api/k8s/gateway_controller.go#L266)).
   On a cluster whose CNI hides pod IPs, this mechanism needs NodePort or NPL, not a wish.

---

## 5. Surface by surface

### 5.1 LLM — flattens cleanly

The request is stateless, so a tier's pool may hold members from many clusters with no
correctness question. The decision sequence is unchanged from what runs today: the DataScript
reads `model` out of a 32 KB-buffered body and selects a tier's pool group; the pool group
picks a healthy site; the response's `usage` block is metered once.

`ai-gateway-datacenter.md §6` specifies the one CRD extension this needs — a tier whose
backend is a *set* of sites with `priority`, `weight` and `drain`, instead of today's single
`remote:` peer. `remote: {host: …}` stays valid as sugar for a one-element list, so nothing
deployed has to change. The implementation risk worth spiking first is the Host rewrite:
with several sites the peer FQDN is no longer a per-tier constant, and the SE picks the pool
*after* the DataScript has run — so Host must be set on the pool, where the site is known,
not in Lua.

**Verdict: a single LLM gateway for the estate is a configuration exercise plus one bounded
build.**

### 5.2 MCP — one gateway, one registry, one role vocabulary

A single MCP front door is the strongest *governance* case of the three, because the north
star for this surface is an enforced allow-list of approved tool servers. One gateway means
one registry: a server that is not routed there is not reachable, estate-wide, and
`toolAccess.roleClaim` gives the estate a single role vocabulary rather than N dialects
([`mcproute_types.go:95`](../../ako-gateway-api/aigateway/mcproute_types.go#L95)).

`AIMCPRoutePolicySpec` has no `remote:` or `sites:` concept
([`mcproute_types.go:80`](../../ako-gateway-api/aigateway/mcproute_types.go#L80)) — backends
arrive only through the HTTPRoute's `backendRefs`. So mechanism **B** is the zero-code path,
and mechanism **A** would need the same `sites:` work as §5.1, generalised off
`AIModelRoutePolicy`.

The constraint is sessions — §5.4.

### 5.3 Agent (A2A) — one gateway makes audience binding load-bearing

Two things change when every agent in the estate sits behind one VIP.

**The agent card must advertise the front door.** An agent that self-reports an in-cluster
address hands callers an address they cannot use, and that bypasses the gateway for the ones
who can. `agentCard.rewrite: true` with `agentCard.url` set to the central hostname is not
optional in this topology
([`A2AAgentCard`](../../ako-gateway-api/aigateway/a2aroute_types.go#L64)).

**Token target binding stops being a nicety.** With one VIP fronting every agent, a token
minted for agent A is *network-reachable* against agent B. `agentAccess.targetAgent` rejects a
token whose `target` claim names a different agent, and it exists precisely because the SE
validates a single audience per `AIGatewayAuthPolicy` — so doing this with `aud` would force
an estate-wide cutover, while the DataScript is generated per route and already knows which
agent it fronts
([`A2AAgentAccess.TargetAgent`](../../ako-gateway-api/aigateway/a2aroute_types.go#L118)).
Set it on every agent route in this topology.

Two further cautions that scale badly if ignored:

- **RBAC is fail-open for non-JSON-RPC requests.** The allow-list is matched against a
  JSON-RPC method and a skill claim, so a plain REST call carries neither and skips
  authorisation. `authorizePaths` and `requireMethod` close it, per route, and both default
  off because real agents serve REST alongside A2A. At estate scale this is a per-route audit,
  not a global switch.
- **East-west traffic hairpins.** Agent-to-agent calls are east-west by nature; a central
  gateway routes every one of them through one VIP. That is the price of central RBAC and one
  audit trail, and it is only *enforced* if each serving cluster's network policy restricts
  agent ingress to the SE subnet. Without that, the gateway is advisory.

### 5.4 The constraint that shapes MCP and agent: server pinning

Both stateful surfaces pin work to a backend the same way — store `(pool_name, server_ip)` in
a VS-scoped table keyed on the session or task id, then re-select on the follow-up call:

```lua
local pool_name = avi.vs.table_lookup("mcp_pool", sid, 600)
local server_ip = avi.vs.table_lookup("mcp_srv",  sid, 600)
if pool_name and server_ip then
  pcall(avi.pool.select, pool_name, server_ip)   -- guarded: may fall back to load balancing
end
```

([`mcproute_datascript.go:70`](../../ako-gateway-api/aigateway/mcproute_datascript.go#L70);
the A2A task path is the same shape at
[`a2aroute_datascript.go:141`](../../ako-gateway-api/aigateway/a2aroute_datascript.go#L141).)

The generator's own comment states the limit: the pin is **"exact for single-server pools;
graceful for multi-server"**
([`mcproute_datascript.go:58`](../../ako-gateway-api/aigateway/mcproute_datascript.go#L58)).
The `pcall` is there because the unguarded system DataScript *raises* — a 500 — on AKO's
EVH-child-VS + PoolGroup topology, verified live. "Graceful" means the request silently
load-balances instead of failing, which for a live `Mcp-Session-Id` or a `tasks/get` follow-up
is a **broken session, not a slow one**.

This inverts the intuitive preference:

| Shape | Pool composition | Affinity outcome |
|---|---|---|
| Flat, mechanism **B**, all clusters' pods in one pool | multi-server | pin degrades to load balancing → sessions break at estate scale |
| Two-hop, mechanism **A**, one FQDN pool per cluster | **single-server** | hop-1 pin is exact and cluster-sticky; hop 2 pins to a pod inside that cluster |

So for MCP and A2A the two-hop shape is not a compromise, it is the correct one — *until* a
native EVH server-pin primitive exists. The cost is that serving clusters are no longer
policy-free: each runs a thin route carrying `session:` or `taskAffinity:` and nothing else.
No auth, no guardrails, no budgets, no registry — those stay at the front door, evaluated once.

**The RFE this justifies:** an SE primitive that pins to a server within a pool group on an
EVH child VS, deterministically. It converts MCP and A2A from two-hop to one-hop and removes
the last AI object from serving clusters.

---

## 6. Identity: how one front door authenticates an estate

Authentication is the one subsystem where "one gateway" does **not** simplify by default. The
mechanism is per route, not per gateway (§6.2), which is simultaneously the design's biggest
piece of freedom and the reason one of the benefits in §7 has to be qualified.

### 6.1 The mechanism as built

`AIGatewayAuthPolicy` has two modes, and they behave very differently across clusters.

| | `oauthBrowser` (default) | `jwtQuery` |
|---|---|---|
| What the SE validates | a bearer JWT, via the OAuth resource-server flow | a JWT in `?jwt=<token>` |
| Avi objects | issuer **Pool** → `AUTH_PROFILE_OAUTH` → `SSOPolicy` → `oauth_vs_config` | `JWTServerProfile` → `AUTH_PROFILE_JWT` → `SSOPolicy` → `jwt_config` |
| Who fetches the JWKS | the **SE**, at runtime, through the issuer Pool | **AKO**, once, embedding the keyset inline in `jwks_keys` |
| Claims in Lua | `avi.http.oauth_get_claim(0, …)` | base64url-decode the query param the SE already validated |
| Key rotation | the SE picks up new keys | **re-apply the policy** |

Neither mode is a plain `Authorization: Bearer` check, and that is deliberate: `SSO_TYPE_JWT`
strips the `Authorization` header before any DataScript runs
([`translator.go:63`](../../ako-gateway-api/aigateway/translator.go#L63)), so a header-bearer
design authenticates the caller and then leaves budgets, entitlement, tool RBAC and agent RBAC
with nothing to read. Every AI policy downstream depends on the claim helper
([`datascript.go:274`](../../ako-gateway-api/aigateway/datascript.go#L274)), so the auth mode is
not an independent choice — it is what makes the rest of the gateway possible.

### 6.2 Auth binds to the route, not to the gateway

`ApplyAuthPolicy` sets `SsoPolicyRef`, `JwtConfig` and `OauthVsConfig` on the **EVH child VS** —
the route's virtual service, not the parent
([`translator.go:93`](../../ako-gateway-api/aigateway/translator.go#L93),
[`translator.go:181`](../../ako-gateway-api/aigateway/translator.go#L181)). And the policy store
binds a policy to `<policy namespace>/<targetRef.name>` and is only ever queried per route
([`controller.go:151`](../../ako-gateway-api/aigateway/controller.go#L151)); MCP and A2A resolve
their `authRef` by name **in their own namespace** too
([`avi_mcp_route.go:63`](../../ako-gateway-api/nodes/avi_mcp_route.go#L63)).

Four consequences, and they are the whole of this section:

1. **One Gateway can front routes that trust different issuers, audiences, even different
   modes.** "One front door" does not mean "one identity provider". This is what keeps §6.4
   solvable and makes an audience cutover incremental rather than estate-wide.
2. **An auth policy does not exist once — it exists once per route.** Forty routes in the entry
   cluster means forty `AIGatewayAuthPolicy` objects, each in its route's namespace, each
   producing its own `JWTServerProfile` + `AuthProfile` + `SSOPolicy` in Avi. §7 benefit 1
   ("policy exists once") is true of model routing, budgets and guardrails; **it is not true of
   auth.** Templating it is a GitOps job, not a gateway feature.
3. **`targetRef.Kind` is parsed but never used in the lookup** — the store indexes by name
   only, so a `Gateway`-scoped auth policy matches nothing unless a route happens to share the
   Gateway's name. A gateway-wide default issuer is the single highest-value auth RFE for this
   topology; it is what would let benefit 1 cover auth too.
4. **Two auth policies on one route is last-writer-wins.** The translator iterates every
   matching policy and each call overwrites the previous policy's fields
   ([`avi_model_l7_translator.go:209`](../../ako-gateway-api/nodes/avi_model_l7_translator.go#L209)),
   in map-iteration order. Keep exactly one per route and make that a lint rule.

### 6.3 Reaching the issuer from the entry cluster

The two modes fail differently here, and both are worth knowing before choosing:

- **`oauthBrowser`** needs the issuer reachable **from the Service Engine**. AKO builds that
  pool by turning the JWKS URL into servers: a literal IP is used directly, and a `<svc>.<ns>`
  name is resolved through **EndpointSlices in the entry cluster**
  ([`resolveIssuerServers`](../../ako-gateway-api/aigateway/oauth_rest.go#L68)) — because SEs
  cannot resolve cluster DNS or reach ClusterIPs. An issuer living in another cluster is
  therefore representable exactly like any other remote backend: a selectorless Service plus a
  hand-written EndpointSlice (§4, mechanism B), or a literal IP.
- **`jwtQuery`** needs the JWKS URL reachable **from the AKO pod** in the entry cluster only
  ([`fetchJWKS`](../../ako-gateway-api/aigateway/jwt_rest.go#L72)); the SE never calls the
  issuer. This is the easier constraint, and it is paid for with a snapshotted keyset.

Neither mode can resolve a `*.cluster.local` name belonging to a different cluster. Whichever
issuer topology §6.4 picks, its JWKS must be reachable by IP, by a routable name, or through a
slice the entry cluster holds.

### 6.4 Where workload identity comes from when the workloads are in *N* clusters

Human identity is unaffected: an enterprise OIDC issuer per environment, one route policy per
route naming it. The hard half is workload identity, because the estate mints short-lived
tokens with **mint-time authorisation** (`POST /exchange`: TokenReview → authorise → a 60 s
token carrying `aud` + `skill` + `jti`), and **TokenReview is cluster-local**.

| Option | How it works | What it costs |
|---|---|---|
| Central issuer + kube credentials per cluster | the issuer TokenReviews against each serving cluster's API server | *N* sets of cluster credentials concentrated in the entry cluster — the sprawl this design exists to avoid |
| **Central issuer, offline SA-token validation** *(recommended)* | the workload presents its projected ServiceAccount token (`audience: jwt-issuer`); the issuer verifies it against that cluster's published OIDC JWKS instead of calling TokenReview | *N* read-only JWKS URLs. Needs each cluster's issuer discovery reachable (`system:service-account-issuer-discovery`) |
| Per-cluster issuers | each cluster runs its own issuer; each route's policy names the issuer of the cluster it serves — possible because auth is per route (§6.2) | breaks east-west: a caller in cluster A holding an A-issued token reaches a route that trusts B. Trust has to follow the **caller**, not the callee's location |
| Raw SA tokens, no exchange | each route trusts the serving clusters' SA JWKS directly | strong authentication, but no `skill` or `target` claim — A2A RBAC loses its best signal and MCP tool access falls back to coarse roles |

**Recommendation: one exchange issuer per environment, hosted in the entry cluster, validating
serving-cluster ServiceAccount tokens offline.** One JWKS per environment, mint-time
authorisation preserved, and no cross-cluster credentials anywhere.

### 6.5 Identity must be estate-unique, or the single ledger quietly lies

Every downstream mechanism joins on a claim: `sub` → the ledger and the per-consumer counters,
the group claim → the budget, agent + skill → A2A RBAC, `target` → audience binding
([`A2AAgentAccess`](../../ako-gateway-api/aigateway/a2aroute_types.go#L118)).

Two clusters each running a `log-collector` ServiceAccount both mint `sub=log-collector`. At
fifteen separate gateways that is fifteen distinct rows; at **one** gateway they merge — into
one budget they now share, and into one ledger line that attributes both to whichever team owns
the name. Nothing errors; the numbers are simply wrong.

So namespace the subject at mint time — `sub = <cluster>/<namespace>/<serviceaccount>` — before
the first cluster is onboarded. It raises counter cardinality by the number of distinct
consumers, not per request, and retrofitting it means rewriting historical ledger data.

### 6.6 Rotation and cutover at estate scale

| Event | With per-route policies | Do this |
|---|---|---|
| JWKS rotation, `jwtQuery` | the keyset is a **snapshot per policy** — every one of them | publish new + old keys, reconcile **every** policy, then retire the old key. Script it; do not hand-apply at *N* routes |
| JWKS rotation, `oauthBrowser` | the SE re-fetches through the issuer pool | keep the issuer pool pointing at ready endpoints; nothing to re-apply |
| Audience change | `Audiences[0]` is the only one matched, so it is a hard cutover — but **per route** | stage it route by route. This is the compensation for §6.2's duplication: consolidate auth policies and you lose it |
| Issuer replacement | per route | run both issuers, migrate routes in waves, retire |

And the standing `jwtQuery` caveats, which get sharper with one front door for everything: the
token rides in the URL, so TLS only, short TTLs, redact the query parameter in access logs, and
keep claims lean enough that the request line stays well under the measured 12 KB ceiling
(§8, limitation 15).

---

## 7. Benefits

Ordered by how hard they are to get any other way.

1. **Policy exists once.** A budget, a model alias, a blocked pattern, a newly approved MCP
   server, an agent's RBAC rule — one object, one pull request, estate-wide. The alternative
   is *N* copies and an estate only as consistent as its slowest cluster. One exception, worth
   knowing before it surprises anyone: **auth policy is per route** (§6.2), so it templates
   rather than centralises.
2. **One ledger.** Every metered response is counted at one of three VSes, so "how many
   premium tokens did the payments team burn last month" has **one** answer instead of *N*
   partial ones. Chargeback stops being a data-engineering project.
3. **One trust anchor.** One issuer per environment, one JWKS to rotate, one place a token is
   accepted or refused. What is unified is the trust anchor, not the object count — the policy
   objects stay per route (§6.2), and §6.6 is the rotation procedure that follows from that.
4. **An estate view exists at all.** "Is premium capacity saturated", "which cluster served
   that request", "which agent called which tool" are answerable because one component sees
   everything. Nothing in a per-cluster design can answer them.
5. **Enforced allow-lists become real.** An MCP server or an agent that is not routed at the
   front door is not reachable — that is the difference between a registry and a wiki page.
6. **Model and tool names mean one thing.** Applications name a model or a tool; operators
   move clusters underneath it. `gpt-oss-120b` cannot mean an H100 in one cluster and nothing
   in another.
7. **Serving clusters get simpler, not more complex.** They run workloads, and (for the
   stateful surfaces, today) one thin route. No AI proxy, no policy engine, no second product
   to upgrade in *N* places.
8. **Uniform, readable object names.** With `useReadableObjectNames`, objects read
   `<cluster>--<surface>-<route>-<hash>`, driven by the `ai.ako.vmware.com/surface` label
   ([`constants.go:35`](../../ako-gateway-api/lib/constants.go#L35)). At estate scale that is
   the difference between an Avi object list you can read and one you have to decode.

---

## 8. Limitations

Stated plainly; each one is a reason someone might choose differently.

| # | Limitation | Why it exists | Mitigation |
|---|---|---|---|
| 1 | **Route authorship centralises.** Serving teams cannot publish a route from their own cluster | `created_by` scoping (§2) — no cross-cluster ownership is possible | GitOps with per-team directories in the entry repo; or the hybrid in §8.1 |
| 2 | **MCP/A2A affinity degrades on multi-server pools** | §5.4 | two-hop shape (mechanism A); RFE for a native server pin |
| 3 | **Mechanism B needs an endpoint sync controller** | the entry AKO watches only its own cluster | MCS `ServiceExport`, or accept mechanism A |
| 4 | **Mechanism B needs SE→pod reachability** | pod IPs are often not routable off-cluster | NodePort or Antrea NPL |
| 5 | **East-west agent traffic hairpins through one VIP** | central RBAC requires a central choke point | size the SE group for it; enforce SE-subnet-only ingress or the gateway is advisory |
| 6 | **One VS per surface is one failure domain per surface** | that is what "single" means | SE group HA (n+m). Note an SE answers only on its own VIP |
| 7 | **Losing the entry cluster freezes config** | it is the only writer | blast radius is *config freeze, not outage* — SEs keep serving the last reconciled state. Run the entry Gateway on an existing cluster, not a new one |
| 8 | **JWKS rotation is an estate-wide event, and per-policy in `jwtQuery` mode** | AKO snapshots the keyset inline, once per policy, and policies are per route (§6.2) | overlap old and new keys, reconcile every policy, then retire — §6.6. `oauthBrowser` avoids it entirely: the SE re-fetches |
| 9 | **`audiences` is matched on the first element** | SE behaviour | a hard cutover — but per route, so stage it route by route (§6.6) |
| 10 | **Streaming responses cannot be metered** | the DataScript cannot see chunks | run metered paths non-streaming; SE-native per-chunk metering is an open RFE |
| 11 | **`full_client_logs` is per-VS and dies on child-VS recreation** | Avi object lifecycle | put it in the deployment pipeline, never in an operator's memory. Any rename or flag flip recreates children |
| 12 | **Health-based downgrade does not exist** | entitlement downgrade covers *not allowed*, not *not available* | priority overflow inside a tier (§5.1); cross-tier fallback needs pool-group health in Lua — RFE |
| 13 | **No queue-depth-aware site selection** | the SE has no backend load signal for remote sites | in-cluster pools have the inference-extension scraper; remote sites are an RFE |
| 14 | ⚠️ **Per-VS scale is unmeasured at estate size** | child VSes, DataScripts and pool groups per parent VS have not been load-tested at *N*=15+ | measure before committing an estate; per-tier pool-group membership of ~5 is unremarkable, the parent-VS fan-out is the unknown |
| 15 | **Request line > 12 KB → 400** | measured SE limit | `jwtQuery` tokens must stay well under it; keep claims lean |
| 16 | **Counter cardinality is consumers × limits** | ledger design | keep limits to a handful; never key on a request id |
| 17 | **Auth policy is per route, not per gateway** | the store binds `<policy ns>/<targetRef.name>` and is only queried per route (§6.2) | template it in GitOps; a gateway-scoped default is an RFE |
| 18 | **Two auth policies on one route silently conflict** | each apply overwrites the previous, in map-iteration order | exactly one auth policy per route; make it a lint rule |
| 19 | **Identity claims collide across clusters** | one `sub` namespace now covers the whole estate | namespace the subject at mint time — `<cluster>/<ns>/<sa>` (§6.5) |
| 20 | **A central `/exchange` cannot TokenReview a serving cluster's SA** | TokenReview is cluster-local | validate projected SA tokens offline against each cluster's OIDC JWKS (§6.4) |

### 8.1 The hybrid, for when limitation 1 or 5 decides it

Per-cluster Gateways *and* a central front door: each cluster owns its own `Gateway` and
publishes its own routes; the central LLM/MCP/agent gateways front them through mechanism A.
Policy is then evaluated twice — expensively, and with two places to disagree — so it is not
recommended for its own sake. It is the right answer only when cluster sovereignty over route
publication is a hard requirement.

---

## 9. Failure domains

| Failure | Blast radius | Behaviour |
|---|---|---|
| One serving cluster | its share of the tiers/tools/agents it hosts | health monitor marks its pools down in ~10 s (5 s × 2); other sites absorb |
| One MCP server or agent | that route only | its pool goes down; callers get an error, everything else is unaffected |
| Entry cluster control plane | **config changes only** | SEs keep serving the last reconciled configuration |
| One SE | that SE's flows | SE group HA; an SE answers only on its own VIP |
| Avi Controller | config changes only | the data plane keeps running |
| IdP | new tokens only | validation is offline against the cached JWKS |
| One surface's VS | that surface, estate-wide | the argument for three Gateways rather than one with three listeners |

---

## 10. Built vs. needed

| Capability | State |
|---|---|
| Model → tier routing from the request body | **Built**, live |
| FQDN pool to a peer gateway, SE-resolved, health-monitored (mechanism A) | **Built**, verified cross-cluster 2026-08-23 |
| Selectorless Service + manual EndpointSlice as a backend (mechanism B) | **Built** (no AI-specific code needed) |
| MCP session affinity, A2A task affinity | **Built** — exact for single-server pools |
| MCP tool RBAC, A2A agent+skill RBAC, target binding | **Built** |
| Token budgets, RPS limiter, metering, ledger, counters | **Built** (non-streaming) |
| WAF + ICAP semantic guardrails, block or log | **Built** |
| Readable per-cluster Avi object names | **Built** |
| Agent-card URL rewrite to the front door | **Built** |
| Per-route JWT / OAuth auth with claims readable in Lua | **Built** |
| **Multi-site tier — `sites:` with priority/weight/drain** | ⚠️ **Design** — the LLM build (`ai-gateway-datacenter.md §6`) |
| Per-pool Host rewrite for multi-site tiers | ⚠️ **Spike first** |
| `sites:` generalised to `AIMCPRoutePolicy` / `AIA2ARoutePolicy` | ⚠️ **Design** — after the LLM one proves out |
| Native EVH server pin within a pool group | ⚠️ **RFE** — converts MCP/A2A to one hop |
| Health-based cross-tier fallback | ⚠️ RFE |
| Queue-depth-aware site selection | ⚠️ RFE |
| SE-native streaming metering | ⚠️ RFE |
| Endpoint sync for mechanism B (MCS or equivalent) | ⚠️ **Not started** — external to AKO |
| Gateway-scoped (default) auth policy | ⚠️ **RFE** — would let auth centralise like the rest (§6.2) |
| More than one trusted issuer on a single route | ⚠️ **RFE** |
| Offline SA-token validation in the exchange issuer | ⚠️ **Build** — external to AKO (§6.4) |
| Cluster-namespaced subjects at mint time | ⚠️ **Build** — external to AKO (§6.5) |

---

## 11. Day 2

**Onboarding a cluster** — one pull request against the entry cluster, no data-plane change:

1. Install AKO with its own `clusterName`, pointing at the same Controller, tenant and SE group.
2. Deploy the workloads and (mechanism A) one `HTTPRoute` per surface it serves, labelled
   `ai.ako.vmware.com/surface`; or (mechanism B) export the Services and let the sync
   controller write the slices.
3. Confirm the SEs resolve the new names — each cluster's own AKO publishes its records.
4. Add the site to the relevant tiers / MCP routes / agent routes in the **entry** cluster.
5. Watch the new pool go green.

The application-facing catalogue does not change, because applications name models, tools and
agents — never clusters.

**Draining a cluster:** `drain: true` on its site entries, wait one health interval, patch.
**Adding a model:** one alias, or label the pod and let discovery register it.
**Approving an MCP server:** one route in the entry cluster. **Revoking one:** delete it —
and it is unreachable estate-wide, which is the entire point.

---

## 12. Rejected alternatives

- **GSLB for cluster selection.** Resolves before the request body exists, so it cannot route
  on `model`, on a tool name, or on a task id. Correct one layer up, across datacentres.
- **One Gateway with three listeners.** Collapses three failure domains into one and forces
  one SE group and one VIP to carry every surface's load and latency profile. The saving is
  one Avi object.
- **A gateway per cluster.** The default answer, and it produces *N* policy copies, *N*
  ledgers, *N* trust anchors and no estate view. See `ai-gateway-datacenter.md §2`.
- **Service mesh multi-cluster.** Federates identity at L4/L7; not body-aware, does not meter
  tokens, and adds a proxy to every cluster.
- **An in-cluster LLM/MCP proxy behind Avi.** A second hop, a second policy language and a
  second thing to scale, to do work the SE already does in the flow it already terminates.

---

## 13. Recommendation

1. **Three Gateways in one entry cluster per environment.** Environments separate by VIP,
   tenant and issuer — never by a claim, which is a value in a token rather than a boundary.
2. **LLM: go flat.** Build `sites:` on `ModelTier`, spike the per-pool Host rewrite first.
   Serving clusters carry no AI policy.
3. **MCP and Agent: go two-hop** (mechanism A, single-server pools) so session and task
   affinity stay exact. Set `agentCard.rewrite` and `agentAccess.targetAgent` on every agent
   route, and audit `authorizePaths` / `requireMethod` per route.
4. **Settle the issuer topology before onboarding the second cluster.** One exchange issuer per
   environment in the entry cluster, validating serving-cluster ServiceAccount tokens offline
   (§6.4), and cluster-namespaced subjects from the first mint (§6.5). Both are cheap now and
   expensive to retrofit — the second rewrites ledger history.
5. **File the server-pin RFE.** It is the one primitive that would let MCP and A2A flatten
   like LLM, and it removes the last AI object from serving clusters.
6. **Measure the parent-VS fan-out** (limitation 14) before committing an estate to it. Every
   other constraint in this document is a measured number; that one is not.
