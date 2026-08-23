<!--
  DESIGN. Builds directly on shipped behaviour: model-based tier routing (model-routing.md)
  and the remote-site tier verified cross-cluster on 2026-08-23 (ai-gateway-multisite.md
  supersedes nothing here — it remains the GSLB-flavoured variant of §6).
  Sections marked ⚠️ NOT BUILT are design; everything else describes code that exists.
-->

# AKO AI Gateway — One Gateway for a Datacenter's AI Workloads

> **Status: design.** The data plane this describes is shipping; the estate shape is not
> yet deployed at this size. The one genuinely new engineering ask is §6 — a tier whose
> backend is a *set* of sites rather than one — and it is a CRD change plus pool-group
> authoring, not a new data path.

**Scope.** Fifteen Kubernetes clusters in one datacenter: five production, ten non-production.
Every LLM, MCP and agent-to-agent call in that estate enters through an Avi Service Engine and
is authenticated, inspected, tiered, metered and routed there. No AI proxy runs inside any of
the fifteen clusters.

---

## 1. The estate

| | Production | Non-production |
|---|---|---|
| Clusters | `p01`–`p05` | `n01`–`n10` |
| Tiers served | premium, standard, low | standard, low |
| Premium hardware | H100 — `p01`, `p02` only | none |
| Standard hardware | L40S — all five | A10 — `n01`–`n04` |
| Low | CPU (quantised 0.5–1.5b) — all five | CPU — all ten |
| Token budgets | enforced, per group, hourly | **not enforced** (counted, never rejected) |
| Guardrails | block | observe |
| Front door | `llm.ai.dc1.example.com` | `llm.np.ai.dc1.example.com` |
| Avi tenant | `ai-prod` | `ai-nonprod` |

Two facts about that table drive the whole design. Premium capacity exists in **two** clusters
out of fifteen, so *which cluster serves a request* is a scheduling decision that must be made
per request. And the prod/non-prod difference is entirely **policy** — same CRDs, same Service
Engines, same code path, different values.

---

## 2. What breaks if you build fifteen gateways instead of one

The default answer is one AI gateway per cluster. At fifteen clusters that produces:

- **Fifteen copies of every policy.** A budget change, a new model alias, a new blocked
  pattern — fifteen pull requests, and the estate is only as consistent as the slowest one.
- **Fifteen token ledgers.** Chargeback becomes a data-engineering project. "How many premium
  tokens did the payments team burn this month" has fifteen partial answers.
- **No estate view.** Nothing can answer "is premium capacity saturated" because no component
  sees more than one cluster.
- **Fifteen trust anchors.** Every cluster's gateway needs the IdP's JWKS, and every key
  rotation is a fifteen-way race.
- **Model names that mean different things in different clusters.** `gpt-oss-120b` routes to a
  H100 in `p01` and to nothing in `n07`, and the application learns this the hard way.

The design below collapses all five by putting the *decision* in one place — the Service
Engines behind two front doors — while leaving the *serving* distributed across all fifteen.

---

## 3. Design principles

1. **The Service Engine is the only data plane.** No sidecar, no in-cluster LLM proxy, no
   second product to upgrade. Everything in §5 happens in the SE.
2. **Two front doors, not fifteen.** One per environment class. The environment boundary is a
   different VIP, a different Avi tenant, and a different trusted issuer — not a header.
3. **A tier is a set of sites.** `premium` is not "cluster p01"; it is "the H100 fleet",
   which today means `p01` and `p02`, and next quarter means something else. Applications
   name tiers through model aliases; operators move sites underneath them.
4. **Policy lives once, in Git, applied to the entry clusters.** The other thirteen clusters
   carry no AI policy at all — they carry an HTTPRoute and a model server.
5. **Prod and non-prod differ by values, not by product.** Every difference in this document
   is a field in a CR, which is what makes "promote from non-prod" a meaningful phrase.

---

## 4. Topology

```
                    ┌───────────────── ONE AVI CONTROLLER (per DC) ─────────────────┐
                    │  tenant ai-prod          tenant ai-nonprod                    │
                    └───────────────────────────────────────────────────────────────┘

  prod clients          ┌──────────────────────────┐
  (apps, agents) ──────►│  llm.ai.dc1  VIP .30     │   SE group: se-ai-prod (n+m HA)
                        │  ─────────────────────── │
                        │  WAF · ICAP classifier   │        ┌─────────────────────────┐
                        │  JWT (prod issuer only)  │  ┌────►│ premium  H100  p01 p02  │
                        │  model → tier            │──┤     ├─────────────────────────┤
                        │  entitlement             │  ├────►│ standard L40S  p01..p05 │
                        │  token budget  (ENFORCE) │  │     ├─────────────────────────┤
                        │  meter → ledger          │  └────►│ low      CPU   p01..p05 │
                        └──────────────────────────┘        └─────────────────────────┘

  non-prod clients      ┌──────────────────────────┐
  (CI, notebooks,  ────►│  llm.np.ai.dc1  VIP .31  │   SE group: se-ai-nonprod
   experiments)         │  ─────────────────────── │
                        │  WAF · ICAP  (OBSERVE)   │        ┌─────────────────────────┐
                        │  JWT (nonprod issuer)    │  ┌────►│ np-standard A10 n01..n04│
                        │  model → tier            │──┤     ├─────────────────────────┤
                        │  token counters (LOG)    │  └────►│ np-low      CPU n01..n10│
                        │  meter → ledger          │        └─────────────────────────┘
                        └──────────────────────────┘

  Every arrow on the right is an Avi pool whose servers are the site's FQDN, resolved by the SE.
  The fifteen serving clusters run AKO + an HTTPRoute. They run no AI policy.
```

### 4.1 Where the front-door policy objects live

The two front doors are Gateways in **entry clusters**. An entry cluster is a control-plane
role, not a data-plane one: it holds the `Gateway`, the `HTTPRoute`, and the five AI policy CRs,
and AKO translates them into Avi objects. No inference traffic touches an entry cluster's nodes.

Run the prod entry Gateway on `p01` and the non-prod entry Gateway on `n01` rather than building
sixteenth and seventeenth clusters. The blast radius of losing an entry cluster is *config
freeze*, not outage: the SEs keep serving the last-reconciled configuration.

### 4.2 Naming

With `useReadableObjectNames` on, Avi objects read
`<cluster>--<surface>-<route>-<hash>` — so `p01--llm-inference-llm-route-…` and
`ako-gw-n07--inference-models-gateway-EVH` name themselves in the console object list, the pool
column of a client log, and the health-monitor list. At fifteen clusters this is not cosmetic;
it is the difference between an object list you can read and one you have to decode.

Site hostnames follow one pattern, because §6 generates health monitors and pools from them:

```
llm.<cluster>.ai.dc1.example.com      →  a serving cluster's model endpoint
llm.ai.dc1.example.com                →  the prod front door
llm.np.ai.dc1.example.com             →  the non-prod front door
```

Each serving cluster's AKO publishes its own hostname into the Avi DNS VS when it creates its
EVH child VS. Nothing maintains a peer list.

---

## 5. The request path

Identical in both environments; only the policy values differ.

| # | Phase | What happens | Prod | Non-prod |
|---|---|---|---|---|
| 1 | Parent EVH VS | Host picks the child VS | ✔ | ✔ |
| 2 | WAF | signature DLP: secrets, PII, injection | **block** | log |
| 3 | ICAP | semantic classifier + gray-zone judge | **block** | log |
| 4 | JWT | validate against the environment's issuer; `sub` → `x-ai-consumer`, group claim forwarded | ✔ | ✔ |
| 5 | `HTTP_REQ` | enable 32 KB request-body buffering | ✔ | ✔ |
| 6 | `HTTP_REQ_DATA` | read `model` → tier (exact → prefix → default); entitlement may downgrade; `poolgroup.select()`; `set_reqvar("ai_tier")` | ✔ | ✔ |
| 7 | `HTTP_REQ_DATA` | token budget keyed on `ai_tier` + group | **429 + Retry-After** | count only |
| 8 | Pool | site chosen by priority + health inside the tier's pool group | ✔ | ✔ |
| 9 | `HTTP_RESP_DATA` | parse `usage` from the response body → ledger | ✔ | ✔ |

Steps 6 and 8 are the two-stage decision that makes this work at estate scale: **the model name
picks the tier; the pool group picks the site.** Step 6 is body-aware and cannot be done by DNS.
Step 8 is health-aware and should not be done in Lua.

---

## 6. ⚠️ NOT BUILT — a tier is a set of sites

Today a `remote` tier names exactly one peer, so its pool group has exactly one member: peer
down means tier down. For an estate where `standard` lives in five clusters, a tier must name
them all.

### 6.1 CRD change

```yaml
tiers:
  - name: premium
    sites:                                   # NEW — ordered, priority-labelled
      - host: llm.p01.ai.dc1.example.com
        priority: 10
      - host: llm.p02.ai.dc1.example.com
        priority: 10
      - host: llm.p03.ai.dc1.example.com     # CPU overflow: only when both H100 sites are down
        priority: 1
    healthPath: /v1/models
  - name: standard
    sites:
      - {host: llm.p01.ai.dc1.example.com}   # equal priority ⇒ load balanced across all five
      - {host: llm.p02.ai.dc1.example.com}
      - {host: llm.p03.ai.dc1.example.com}
      - {host: llm.p04.ai.dc1.example.com}
      - {host: llm.p05.ai.dc1.example.com}
```

`remote: {host: …}` stays valid and becomes sugar for a one-element `sites:` list, so nothing
deployed today has to change. Per-site fields mirror the current `ModelRemote`
(`port`, `tls`, `preserveHost`) plus:

| Field | Default | Meaning |
|---|---|---|
| `priority` | 10 | Avi pool-group priority label. Highest healthy priority takes all traffic. |
| `weight` | 1 | Ratio within one priority band. |
| `drain` | false | Author the pool, mark it down. Maintenance without deleting config. |
| `maxConcurrent` | unset | Pool-level concurrent-connection ceiling (§7.3). |

### 6.2 What AKO authors

```
   AIModelRoutePolicy tier "premium"
              │
              ├── pool  …-premium-p01-remote-pool   server = FQDN, resolve_server_by_dns, prio 10
              ├── pool  …-premium-p02-remote-pool   server = FQDN, resolve_server_by_dns, prio 10
              ├── pool  …-premium-p03-remote-pool   server = FQDN, resolve_server_by_dns, prio 1
              ├── healthmonitor per distinct (path, tls) — GET /v1/models  WITH a Host header
              └── poolgroup …-premium-remote-pg     members = the three pools, priority-labelled
                        ▲
                        └── the DataScript still calls avi.poolgroup.select() on exactly one name
```

**No DataScript change.** `GenerateModelRouteScripts` already emits
`avi.poolgroup.select(TIER_PG[tier])` and one `replace_header("Host", …)`. The Host rewrite is
the only complication: with several sites the peer FQDN is no longer a per-tier constant. Two
options, and the second is the right one:

- ~~Bake a per-site table into Lua and rewrite Host after the pool is chosen~~ — the SE picks
  the pool *after* the DataScript runs, so the script cannot know which site won.
- **Set Host per pool, in the pool object.** Avi's pool `server_name` / host-header rewrite
  applies at the pool, which is exactly the scope where the site is known. The generator drops
  the remote Host rewrite when a tier has more than one site and lets the pool carry it.

That is the one implementation risk worth spiking before committing to this section.

### 6.3 Failure behaviour this buys

| Event | Result |
|---|---|
| `p01` premium pod crashloops | health monitor marks `p01` down in ~10 s; `p02` takes all premium traffic |
| Both H100 clusters down | priority-1 CPU site takes premium traffic (slow, serving) |
| No healthy site in a tier | pool group empty → the request fails; entitlement downgrade does **not** cover health. See §11.1 |
| Cluster maintenance | `drain: true`, one commit, traffic gone within a health interval |

---

## 7. Rate limits: enforced in prod, absent in non-prod

### 7.1 Production

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AITokenRateLimitPolicy
metadata:
  name: llm-prod-budgets
  namespace: inference
  annotations:
    ai.ako.vmware.com/admin-token-secret: counters-admin
spec:
  targetRef: {group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-route}
  identitySource: {header: x-ai-consumer, fallback: reject}
  requestRateLimit: {requestsPerSecond: 50, burst: 100, key: consumer}
  limits:
    - name: premium-hourly
      key: consumer
      tokens: total
      window: 1h
      groupHeader: reqvar:ai_tier          # this limit only bites on the premium tier
      groupBudgets:
        premium: 2000000
      budget: 0                            # any other tier: this limit does not apply
      action: {type: Reject, statusCode: 429, retryAfter: true}
    - name: per-group-hourly
      key: consumer
      tokens: total
      window: 1h
      groupHeader: x-ai-group
      groupBudgets:
        platform-eng: 20000000
        payments:      5000000
        risk:          5000000
        agents:       10000000
      budget: 0                            # unrecognised group ⇒ rejected, not silently allowed
      action: {type: Reject, statusCode: 429, retryAfter: true}
```

Three deliberate choices:

- **Counters are per consumer; budgets are per group.** Each `sub` gets its own counter and is
  checked against its group's ceiling, so one noisy service account cannot spend the team's
  entire allowance in the first ten minutes of the hour.
- **`budget: 0` for unrecognised groups.** Fail closed. A token whose group claim you have not
  onboarded gets 429, not the platform team's allowance.
- **An RPS limiter alongside the token budget.** Token budgets are a *cost* control and they
  are only enforced after a response has been metered. A runaway agent loop issuing 400 rps of
  short prompts is a *capacity* incident, and Avi's native distributed rate limiter is the
  right tool for it. Both, not either.

### 7.2 Non-production

```yaml
spec:
  targetRef: {group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-np-route}
  identitySource: {header: x-ai-consumer, fallback: clientIP}
  limits:
    - name: np-visibility
      key: consumer
      tokens: total
      window: 1h
      budget: 50000000                     # a tripwire, not a ceiling
      action: {type: Log}                  # counted, logged, never rejected
```

"No rate limit" must not mean "no counter". Keep the counter for three reasons: chargeback
across the whole estate uses one data source; a runaway notebook is visible the same hour
rather than at month end; and promoting a workload to prod is then a question with an answer —
"it burns 1.4 M tokens an hour, and the payments budget is 5 M".

### 7.3 The part that is not a rate limit at all

Unlimited tokens in non-prod is safe only while non-prod cannot starve prod of GPUs. Two
mechanisms, in order of preference:

1. **Separate fleets.** The table in §1 already does this: no non-prod tier is served by a prod
   cluster. This is the whole reason the tier catalogue has `np-standard` rather than letting
   non-prod name `standard`.
2. **Concurrency ceilings where fleets are shared.** If a shared fleet ever appears, cap it at
   the pool with `maxConcurrent` (§6.1) and give prod pools the higher priority label. A
   concurrency ceiling bounds GPU occupancy, which a token budget does not: one 100 K-token
   request occupies a GPU far longer than a hundred 1 K-token ones costing the same.

> **Not solved here.** Neither mechanism is queue-aware. Steering on *live* GPU queue depth
> needs the SE to see a backend load signal — that is the inference-extension scraper for
> in-cluster pools, and an RFE for remote sites.

---

## 8. Identity

Two issuers, per the estate's existing split: humans authenticate to the real OIDC provider,
workloads to the in-cluster issuer that can do mint-time authorisation.

```yaml
# prod front door
spec:
  authMode: jwtQuery
  jwt:
    issuer:    https://idp.dc1.example.com/realms/ai-prod
    jwksUri:   https://idp.dc1.example.com/realms/ai-prod/protocol/openid-connect/certs
    audiences: [ai-gateway-prod]
    identityClaim: sub
    forwardClaims: [groups]
```

The non-prod front door trusts a **different issuer and a different audience**. That is the
environment boundary: a non-prod token presented to the prod VIP fails signature or audience
validation at step 4, before any policy runs. A group claim is not a boundary — it is a
budget selector inside one environment.

Two operational warnings carried over from the estate as built:

- AKO snapshots the JWKS into the generated configuration, so **IdP key rotation is an
  estate-wide event**, not a per-cluster one. Overlap old and new keys in the JWKS through a
  full reconcile before retiring the old key.
- `audiences` is matched on the first element by the SE, so changing an audience is a hard
  cutover rather than a rolling config change. Plan it as one.

---

## 9. Guardrails

One `AIGuardrailPolicy` per front door, same detectors, different action:

| | Prod | Non-prod |
|---|---|---|
| `profile` | `BlockLLMAndMCP` | `BlockLLMAndMCP` |
| `inspect` | request + response | request |
| `semantic.enabled` | true | true |
| `action.type` | `Block` (403) | `Log` |

Running the same detectors in observe mode across ten non-prod clusters is the cheapest
false-positive corpus available: every block that *would* have happened is logged against real
developer traffic before it can break a production call. The measured cost is the classifier
hop — roughly 780 ms when the gray-zone judge runs, 36 ms for a signature-only match — which is
why non-prod runs it at all rather than turning it off to save latency.

---

## 10. Metering, chargeback and the estate view

Because every request in the datacenter passes through one of two front doors, **there are two
places that count tokens, not fifteen**. The SE parses `usage` out of each response body and
posts a record to the ledger; the console reads it through
`GET /v1/admin/counters` on the same VS.

What the estate can answer, from one data source:

- tokens by consumer, by group, by tier, by hour;
- which *cluster* served them — the DataScript writes
  `model=… tier=… pool-group=… remote=<site fqdn>` into the client log for every request;
- how close each group is to its budget right now, from the live counters;
- what non-prod would have cost if it were billed at prod rates.

Two known limits, unchanged by scale: **streaming responses cannot be metered** in the
DataScript (the estate runs non-streaming for metered paths; SE-native per-chunk metering is an
open RFE), and per-VS `full_client_logs` must be re-enabled after any change that recreates
child virtual services — which at fifteen clusters means it belongs in the deployment pipeline,
not in an operator's memory.

---

## 11. Failure domains

| Failure | Blast radius | Behaviour |
|---|---|---|
| One serving cluster | that cluster's share of its tiers | health monitor marks its pools down in ~10 s; other sites absorb |
| One tier's entire fleet | one tier, one environment | §11.1 |
| Entry cluster (`p01` control plane) | config changes only | SEs keep serving the last reconciled config |
| One SE | that SE's flows | SE group HA; note an SE answers only on its own VIP |
| Avi Controller | config changes only | data plane keeps running |
| IdP | new tokens only | validation is offline against cached JWKS; existing tokens keep working |
| Prod ↔ non-prod | none | different VIP, tenant, SE group, issuer and fleets |

### 11.1 Health-based downgrade is the gap

Entitlement downgrade moves a caller to a lower tier when they are *not allowed* the one they
asked for. Nothing moves a caller when the tier they asked for is *down*. Today they get an
error; the honest options are:

- **Priority overflow inside the tier** (§6.1) — covers "premium hardware is gone, serve it
  slowly" and needs no new decision logic. Recommended.
- **Cross-tier fallback in the DataScript** — a `fallbackTier` on `ModelTier`, selected when
  `TIER_PG[tier]` has no healthy member. Requires the SE to expose pool-group health to Lua,
  which it does not today. RFE.

---

## 12. Day 2

**Onboarding cluster 16** — one pull request, no data-plane change:

1. Install AKO on the cluster, `clusterName: n11`, pointing at the same Controller and the
   environment's tenant and SE group.
2. Deploy the model servers and one `HTTPRoute` for
   `llm.n11.ai.dc1.example.com` with the `ai.ako.vmware.com/surface: llm` label.
3. Confirm the SEs resolve that name (its own AKO publishes the record).
4. Add one `sites:` entry to the relevant tiers in the **entry** cluster's
   `AIModelRoutePolicy`.
5. Watch the new pool go green in the pool group.

The application-facing model catalogue does not change, because applications name models, not
clusters.

**Draining a cluster for maintenance:** `drain: true` on its site entries, wait one health
interval, patch. **Adding a model:** add a `modelTiers` alias, or label the pod and let
discovery register it. **Changing a budget:** one `groupBudgets` value in one file.

---

## 13. Measured constraints

These are spike results, not estimates, and they bound the design:

| Constraint | Value | Consequence at 15 clusters |
|---|---|---|
| Request-body buffer | 32 KB | `model` is at the head of the JSON, so tier selection is always safe |
| Request line | > 12 KB → 400 | `jwtQuery` tokens must stay well under it; keep claims lean |
| Health detection | 5 s × 2 checks ≈ 10 s | a dead site costs at most ~10 s of errors |
| Classifier | ~780 ms with judge, ~36 ms signature-only | budget it in prod SLOs |
| Counter cardinality | consumers × limits | keep limits to a handful; do not key on request id |
| Pool-group members | one per site | five sites per tier is unremarkable for Avi |

---

## 14. Built vs. needed

| Capability | State |
|---|---|
| Model → tier routing from the request body | **Built**, live |
| Remote tier to one peer cluster (FQDN pool, SE-resolved, health-monitored) | **Built**, verified cross-cluster 2026-08-23 |
| Entitlement gating + downgrade | **Built** |
| Token budgets per consumer/group, RPS limiter, `Log` action | **Built** |
| Response metering + ledger + counters endpoint | **Built** (non-streaming) |
| WAF + ICAP semantic guardrails, block or log | **Built** |
| MCP and A2A surfaces on their own gateways | **Built** |
| Readable Avi object names per cluster | **Built** |
| **Multi-site tier with priority/weight/drain** (§6) | ⚠️ **Design** — the one real build |
| Per-pool Host rewrite for multi-site tiers (§6.2) | ⚠️ **Spike first** |
| Health-based cross-tier fallback (§11.1) | ⚠️ RFE |
| Queue-depth-aware site selection (§7.3) | ⚠️ RFE |
| SE-native streaming metering | ⚠️ RFE (open) |

---

## 15. Rejected alternatives

- **GSLB for site selection.** DNS resolves before the request body exists, so it cannot route
  on `model`. It remains the right tool for *multi-datacenter* selection above this design —
  see [ai-gateway-multisite.md](ai-gateway-multisite.md) — but inside one DC it would move the
  decision to the one layer that cannot see what was asked for.
- **A gateway per cluster.** §2.
- **Service mesh multi-cluster.** Federates service identity at L4/L7; it is not body-aware,
  does not meter tokens, and adds a proxy to every one of the fifteen clusters.
- **An in-cluster LLM proxy (LiteLLM et al.) behind Avi.** Adds a hop, a second policy language
  and a second thing to scale, to do work the SE already does in the flow it already terminates.
- **One front door for both environments, separated by claims.** A claim is a value in a token;
  an environment boundary should survive a mis-issued token. Two VIPs, two issuers.
