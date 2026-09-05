<!--
  DESIGN DRAFT. Technical architecture is repo-appropriate (cf. model-routing.md).
  NOTE: §"Novelty & prior art" contains COMPETITIVE POSITIONING (a "first" claim and a
  named competitor). Review that section with marketing/legal before this doc is published
  to a public branch. Drafted 2026-06-06.
  UPDATED 2026-09-05: the one-peer case shipped WITHOUT GSLB (AIModelRoutePolicy
  tiers[].remote, verified cross-cluster 2026-08-23). This doc is now the layer ABOVE
  that — many sites, geo/capacity steering, cross-site transit. Read the status block
  and ai-gateway-datacenter.md §6 before quoting anything here as unbuilt.
-->

# AKO AI Gateway — Multi-Site (Cross-Cluster) Model Delivery

> **Status: superseded in part — the simple case is BUILT.** Since 2026-08-23 an
> `AIModelRoutePolicy` tier can name a **peer AI Gateway in another cluster**
> directly (`tiers[].remote`), and that path is verified cross-cluster on the lab
> estate: the `qwen-antrea` tier on the `vks-ai-01` front door is served by the
> `k8s-antrea` cluster, proven by the serving pod's fingerprint. See
> [model-routing.md](model-routing.md) for the shipped CRD and
> [ai-gateway-datacenter.md](ai-gateway-datacenter.md) for the estate shape.
>
> That mechanism deliberately does **not** use GSLB. The SE resolves the peer's
> FQDN itself at connection time and a Host-bearing health monitor decides whether
> the peer is up; which site serves a request is decided by *tier selection*, after
> the model has been read from the body — not by DNS answering a client. It covers
> one peer per tier, one datacentre, no geo steering.
>
> **This document remains the design for the layer above that**: many sites, geo
> and capacity steering, and a dedicated cross-site transit path, composing the
> per-cluster **AI Gateway** ([ai-gateway.md](ai-gateway.md)), **model-based tier
> routing** ([model-routing.md](model-routing.md)) and **AMKO + Avi GSLB**. It
> introduces no new data plane. Cross-site behaviour is still **spike-gated**:
> sections marked ⚠️ are hypotheses, not capabilities. Read §6 of
> [ai-gateway-datacenter.md](ai-gateway-datacenter.md) first — a tier that names a
> *set* of peers is the smaller, nearer step, and it does not need GSLB either.

---

## 1. Overview — the missing layer

Today's AI gateways — including AKO's — are **per-cluster**. They terminate, authenticate,
meter, and route inference traffic *within one cluster*. None of them route a **model**
across **sites**: deciding, per request, *which region's GPU fleet* should serve it, with
health-based failover and geo/capacity steering.

That gap matters because GPUs are scarce and unevenly distributed. A premium 70B fleet may
exist only in `us-west`; an economy fleet may be everywhere; a sovereign workload must stay
in `eu-central`. The control a fleet operator actually wants is:

> "Route this *model* to the best *site* that can serve its *tier*, fail over automatically,
> keep restricted models in-region, and don't make me run a new proxy to do it."

This design delivers that by letting each layer do only what it is natively good at:

| Decision | Layer | Mechanism |
|---|---|---|
| **Which tier?** (by `model` in the body) | L7, in the SE | model-route DataScript ([model-routing.md](model-routing.md)) |
| **Which site?** (health / geo / capacity) | DNS / global | AMKO-federated **GSLB service** |
| **How does it cross sites safely?** | dedicated transit SE | forwarder/ingress **SE groups** + NSX/vDefend DFW |

---

## 2. Novelty & prior art  *(competitive positioning — internal review before publish)*

To our knowledge this is the **first design to combine model-aware L7 routing with
cross-site global server load balancing for AI inference.** The claim is about the
*combination*; the parts exist, and we say so honestly:

- **Per-cluster AI / agentic gateways** (e.g. agentgateway, which reached v1.0 in March
  2026 under the Linux Foundation) are explicitly *cluster-scoped* — "not a multi-cluster
  orchestration platform." They route LLM/MCP/A2A traffic **within** a cluster and have no
  global-LB / cross-site delivery layer.
- **LLM gateways** (LiteLLM, Portkey, cloud AI gateways) steer to model *providers*; any
  multi-region behaviour is per-instance config, not body-aware model→site routing.
- **Service-mesh multi-cluster** (Istio, Gloo Mesh, Linkerd) federates *service identity*
  at L4/L7 — it is not model-aware and does not read the inference body.
- **GSLB** (incl. Avi's, via AMKO) is a mature global-LB primitive — but it is **DNS/site
  level and model-blind**; on its own it cannot route by `model`.

The novelty is the *bridge*: body-derived **model/tier** selection feeding a **GSLB**
site decision, gated by **group entitlement**, transported over a **dedicated SE** that is
the single segment a security fabric (vDefend/NSX) wraps. No single competitor occupies all
of: model-aware, cross-site, entitlement-gated, security-fabric-integrated.

> **Honesty marker.** "First" is a positioning claim, not a proven capability. It is only
> true once the §10 spikes pass (specifically that a tier's backend can be a GSLB-fronted
> FQDN that resolves through the global-LB logic). Until then this is the *first design*,
> not the first shipping product. Do not put "first AI gateway that…" on a slide ahead of
> the spike results.

---

## 3. The core constraint (the layer seam)

GSLB resolves a global FQDN to a **site VIP** at **DNS time** — *before the request body
exists*. The `model` field lives in the **POST body**, readable only at L7 in the SE
*after* a site has already been chosen. So GSLB **cannot** route by model, and the SE
cannot (cheaply) know global site health. The design resolves this by **ordering** the two
decisions and connecting them through one indirection:

```
tier  = f(body.model)        ← decided in the entry SE (L7)
site  = GSLB(tier-fqdn)      ← decided when the SE resolves the tier's FQDN backend
```

The entry SE picks the **tier**; resolving that tier's **GSLB-fronted FQDN backend** picks
the **site**. One FQDN pool member is the whole bridge (§7).

---

## 4. Architecture

Three SE-group roles per site, plus the GSLB control plane AMKO maintains:

```
        ┌──────────────────────── SITE A (entry) ────────────────────────┐
        │                                                                 │
 client │   ┌─────────────────────┐         ┌───────────────────────┐    │
 ──────►│──►│  GATEWAY SE group    │         │ CROSS-SITE FORWARDER  │    │
        │   │  • client TLS, OIDC  │ local   │ SE group              │    │
        │   │  • DataScript:       │ hop     │ • backend = tier GSLB │    │
        │   │    model → tier      │────────►│   FQDN (B′ resolver)  │    │
        │   │  • token metering    │         │ • re-originate mTLS   │    │
        │   │  • poolgroup.select  │         │ • WAN stream-proxy    │    │
        │   └─────────────────────┘         └───────────┬───────────┘    │
        └────────────────────────────────────[ NSX/vDefend DFW ]─────────┘
                                                         │ mTLS / WAN
            ┌──────────── SITE B ───────────────┐        │
            │  ┌────────────────────────────┐   │◄───────┘
            │  │ CROSS-SITE INGRESS SE group │   │   resolves via
            │  │ • trusts forwarder (mTLS)   │   │   GSLB to the
            │  │ • NO re-auth / NO re-tier   │   │   best site VIP
            │  │ • → local premium pool      │   │
            │  └─────────────┬──────────────┘   │
            │                ▼                   │
            │        premium InferencePool       │   (scraper-weighted H100 pods)
            └────────────────────────────────────┘

  Control plane:  AMKO (leader cluster) federates each site's per-tier InferencePool/VS
                  into GSLB services  <tier>.<route>.<gslb-domain>  on the Avi DNS VS.
```

| Component | Role | Notes |
|---|---|---|
| **Gateway SE group** | user-facing L7 AI policy | client TLS, OAuth/OIDC, model-route DataScript, token budgets. CPU-bound (Lua); scales with request rate. Unchanged from single-site AI Gateway. |
| **Cross-site Forwarder SE group** | egress transit | accepts only *remote-tier* traffic; backend is the tier's GSLB FQDN; re-originates mTLS; stream-proxies over the WAN. The **only** traffic crossing the site boundary. Scales with concurrent remote sessions. |
| **Cross-site Ingress SE group** | remote acceptance | trusts the forwarder via mTLS; routes **straight to the local tier pool**, skipping re-auth and re-tier (already done up-stream). |
| **GSLB service (per tier)** | site selection | `<tier>.<route>.<gslb-domain>`, members = each site's tier VIP; health/geo/weighted/priority. Built & reconciled by **AMKO**. |
| **AMKO** | federation control plane | runs in the leader cluster; turns per-site tier `InferencePool`/VS into GSLB services + Global Deployment Policy. |

**Why a dedicated transit SE (design constraint).** Separating the WAN proxy from the
gateway SE gives: (a) **failure isolation** — a saturated cross-site forwarder cannot starve
the DataScript engine every *local* request depends on; (b) a **single enforcement segment**
— cross-site traffic is one named thing the DFW wraps (§9); (c) a **clean home for the GSLB
resolver** (§7, B′) without touching gateway config; (d) a **trust boundary** for mTLS and
the "skip re-auth at the remote" decision. Cost: two extra SE-group roles per site and one
extra in-DC hop (the WAN hop still happens once — this isolates WAN latency, it does not
remove it).

---

## 5. Request flows

### 5.1 Local-tier request (no cross-site)

Identical to single-site model routing: gateway SE reads `model` → tier resolves to a
**local** pool group → served locally. The forwarder/ingress SEs are not involved. This is
the graceful-default: if federation is absent, every tier is local and the gateway still
works.

### 5.2 Remote-tier request (the full path)

1. GSLB lands the client at the nearest site (A); gateway SE terminates TLS, runs OIDC.
2. DataScript reads `model`, resolves **tier = premium**, checks group entitlement.
3. premium is a **federated** tier → its pool group's member is the local **forwarder VIP**
   (cheap in-DC hop).
4. Forwarder SE resolves the tier GSLB FQDN `premium.llm.<gslb-domain>` ⚠️ **through the
   GSLB logic** → best site VIP (B, because A lacks H100s / A's premium is saturated).
5. Forwarder SE opens **mTLS** to site B's ingress VIP and stream-proxies the request.
6. Site B ingress SE trusts the forwarder, **does not re-auth or re-tier**, sends straight
   to the local premium InferencePool → H100 pods serve the completion.
7. Tokens stream back B → forwarder → gateway → client.

Tier decided locally from the body; site decided globally by GSLB; **no failover or geo
logic was written — it is inherited from AMKO/GSLB.**

### 5.3 Failover

Site B's premium fleet goes unhealthy → GSLB health monitor marks B's `premium` VIP down →
`premium.llm.<gslb-domain>` stops resolving to B. On the forwarder SE's next re-resolution,
premium flows to the next-best site, **with no config change and no DataScript awareness.**
Failover time = GSLB health-detect + DNS TTL + SE re-resolve interval (§10, must be measured).

### 5.4 Geo / capacity arbitrage

Same mechanism, different GSLB algorithm: geo-proximity (serve from nearest GPU fleet) or
weighted/priority pools (push spillover to a site with spare/cheaper capacity). A pure
config property of the GSLB service AMKO builds — no data-plane change.

---

## 6. Configuration model

A tier in [`AIModelRoutePolicy`](model-routing.md#crd--aimodelroutepolicy) gains an optional
`federation` stanza marking it multi-site. Everything else is unchanged.

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIModelRoutePolicy
metadata: { name: llm-tiers, namespace: inference }
spec:
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-route }
  modelTiers: { "llama-3-70b-instruct": premium }
  defaultTier: economy
  tiers:
    - name: premium
      backendRef: { group: gateway.inference.x-k8s.io, kind: InferencePool, name: premium-llm }
      # NEW: mark this tier as served across sites via GSLB.
      federation:
        scope: Federated                 # Local (default) | Federated
        gslbFQDN: premium.llm.example.com   # optional; AKO derives one if omitted
        residency: []                    # optional allow-list of sites/regions (§9)
    - name: economy
      backendRef: { group: gateway.inference.x-k8s.io, kind: InferencePool, name: economy-llm }
      # no federation block → Local; served in-cluster as today
```

**How the GSLB service is built.** Each site runs the tier's `InferencePool` (local pods,
scraper-weighted). **AMKO**, in the leader cluster, federates those per-site tier VIPs into
the GSLB service `premium.llm.example.com` via a Global Deployment Policy, applying the
chosen algorithm (geo / weighted / priority-failover). AKO, when it sees `scope: Federated`,
emits the tier's forwarder pool member as that **FQDN** rather than local pod IPs, and points
the **forwarder SE group's** resolver at the Avi DNS/GSLB VS (§7, B′).

| Field | Type | Req | Description |
|---|---|---|---|
| `tiers[].federation.scope` | enum | no | `Local` (default) or `Federated`. |
| `tiers[].federation.gslbFQDN` | string | no | GSLB service FQDN; derived if omitted. |
| `tiers[].federation.residency` | []string | no | Sites/regions this tier may be served in (§9). Empty = any. |

---

## 7. The bridge mechanism — FQDN backend resolved through GSLB (B′)  ⚠️

The single load-bearing mechanism. The forwarder SE's tier backend is **not an IP** — it is
the GSLB FQDN, and the forwarder is configured to resolve it **through the Avi DNS VS that
hosts the GSLB service**, so global health/geo/weight apply:

```
forwarder VS (site A)
  pool: premium-xsite-pool
    member (FQDN): premium.llm.example.com
    resolver:     Avi DNS VS / GSLB  ◄── B′: forces GSLB logic at resolution time
        └─► GSLB service premium.llm.example.com
              members: [ siteA-premium-VIP (health,geo,weight),
                         siteB-premium-VIP (health,geo,weight) ]  ── AMKO-maintained
```

- **B (clean):** an FQDN pool member resolves through GSLB implicitly. *Unverified.*
- **B′ (explicit):** pin the forwarder pool's resolver at the GSLB-bearing DNS VS so the
  algorithm is applied deterministically. *The intended default once §10/Spike-1 settles.*
- **A (fallback):** static remote-VIP members + Avi pool **health monitors** for failover if
  neither B nor B′ resolves through GSLB. Loses geo/weight elegance; keeps failover.

Because resolution lives only on the **forwarder SE group**, this entire question is isolated
from the gateway and ingress SEs — a direct benefit of the dedicated-SE constraint.

---

## 8. Composition with auth, tiering, and token budgets

Event/role ordering across the path:

1. **Auth (OAuth/OIDC)** — gateway SE only. Claims become readable to the DataScript.
2. **Model-route tiering** — gateway SE only. `model` → tier, entitlement check, select
   local pool *or* (federated) the forwarder VIP. Sets the `ai_tier` reqvar.
3. **Token budgets** — gateway SE, keyed on `ai_tier` (per-tier budgets). Metering happens
   **once, at entry**, so per-tier accounting is correct regardless of serving site.
4. **Cross-site ingress** — remote SE **skips** auth and tiering by construction (trusted
   forwarder path), avoiding double-auth and double-metering.

> **Counter note.** Token counters live in the entry SE's string table (see
> [counters](ai-gateway.md)); cross-site serving does not split them because metering is at
> entry. A future per-site usage view would aggregate entry-SE counters across sites in the
> management plane, not in the data path.

---

## 9. Security model — the cross-site SE is the enforcement boundary

The dedicated transit SE turns "secured by vDefend" from a slogan into a concrete control:

- **One segment to wrap.** All cross-site inference traffic egresses through the forwarder
  SE group and ingresses through the ingress SE group. **NSX/vDefend DFW** policy: *only* the
  forwarder SE may reach remote tier VIPs; *only* trusted forwarders may reach an ingress SE;
  everything else denied.
- **mTLS between sites.** Forwarder ↔ ingress is mutually authenticated; the WAN hop is
  encrypted and identity-bound, so "skip re-auth at the remote" is safe (it rests on a
  trusted, attested transit path, not an open port).
- **Data residency / sovereignty.** `federation.residency` constrains which sites a tier may
  be served in; AKO omits non-allowed site VIPs from that tier's GSLB members, so a
  restricted model **cannot** be routed out of region even on failover. This is a governance
  control a per-cluster gateway structurally cannot offer.

This is the concrete object the security pitch was missing: a single, named, encrypted,
DFW-enforced data path carrying premium GPU traffic between regions.

---

## 10. Feasibility — spikes to run before any capability claim

Same method as the model-routing spike (throwaway VS + DataScriptSet + pools/GSLB via Avi
REST from an in-cluster pod, torn down after). Ordered by how load-bearing they are.

| # | Question | Pass criterion |
|---|---|---|
| **1 (make-or-break)** | Does a forwarder-SE FQDN pool member resolve **through GSLB** (B), or must we pin the resolver (B′)? | A premium request entering site A is served from site B and the path is deterministic. |
| **2** | **Failover time** when the serving site's tier goes unhealthy. | Premium re-routes to another site within an agreed budget (target: **< 30 s**; ideally single-digit s). |
| **3** | **Streaming through 3 SEs** (gateway→forwarder→ingress). | Token stream proxies end-to-end without full-response buffering; time-to-first-token within budget. |
| **4** | **mTLS + skip-reauth** at ingress. | Ingress accepts only attested forwarder traffic and serves without re-running auth/tier. |
| **5** | **Residency enforcement.** | A tier with `residency:[eu-central]` is never resolved/served outside it, including under failover. |

> **Residual from model-routing carries forward:** the 32 KB request-body buffer cap (model
> sits at the JSON head, so head-buffering suffices) and `get_req_body` behaviour — unchanged
> here, since tiering still happens at the gateway SE.

---

## 11. Failure modes (graceful degradation)

| Condition | Behaviour |
|---|---|
| `AIModelRoutePolicy` absent / unreconciled | Route keeps its own backendRef → all traffic local default backend. No tiering, **works**. |
| `federation.scope: Local` (or omitted) | Tier served in-cluster as today. Cross-site path unused. |
| GSLB / Avi DNS unreachable from forwarder | Forwarder fails the tier; **fall back to A** (static member + health monitor) if configured, else tier errors (alarmed). |
| Remote serving site down | GSLB removes it; traffic re-routes (Spike-2 timing). |
| Forwarder SE group down | Remote tiers unavailable from that site → **define**: degrade to a local lower tier, or 503. (Design choice; recommend downgrade where entitlement allows.) |

---

## 12. Roadmap

| Phase | Feature | Status |
|---|---|---|
| 3 | Single-peer remote tier (`tiers[].remote`) — no GSLB, SE-resolved FQDN, health-monitored | ✅ **Built 2026-08-23**, verified cross-cluster |
| 3 | A tier that names a **set** of peers (`tiers[].sites[]`, priority/weight/drain) | Designed — [datacenter §6](ai-gateway-datacenter.md) |
| 3 | Federated tier via GSLB FQDN backend (B′) + forwarder/ingress SE groups | Design (this doc); **spike-gated** |
| 3 | Health-based cross-site failover (inherited from GSLB) | Design |
| 3 | Geo / capacity-weighted site steering | Design |
| 3 | Data-residency enforcement via GSLB member filtering | Design |
| 3.x | DFW/mTLS policy templates for the cross-site segment (vDefend) | Idea |
| 3.x | Budget-aware cross-site spill (over-budget premium → cheaper site instead of 429) | Idea |
| 4 | Management-plane fleet view aggregating per-site counters | Idea (see strategy memo) |

---

## 13. Related docs

- [AI Gateway](ai-gateway.md) — per-cluster OAuth/OIDC + token budgets (the entry-SE policy)
- [Model-Based Routing](model-routing.md) — `AIModelRoutePolicy`, the tier decision this
  design federates across sites
- [Inference Extension](inference-extension.md) — metric-weighted LB within each tier's pool
- AMKO — Avi Multi-Cluster Kubernetes Operator (GSLB federation control plane)
