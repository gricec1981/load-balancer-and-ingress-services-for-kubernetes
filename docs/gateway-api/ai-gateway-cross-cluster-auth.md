<!--
  Cross-cluster agent authorization: one AIA2ARoutePolicy governing agents in every
  cluster of the estate, enforced on the shared Avi SE fabric.
  Drafted 2026-08-28. DESIGN ONLY — nothing here is implemented.
  Substrate is proven: vks-ai-01 and k8s-antrea-cp both register with the same Avi
  Controller, and the qwen-antrea remote tier already routes across the boundary
  (see ai-gateway-multisite.md). What is missing is policy authoring and mint-time
  authorization, not data path.
  PREREQUISITE: the issuer's subject is a bare ServiceAccount name and is ambiguous
  across clusters (§5). That is a correctness bug and blocks everything below.
-->

# AKO AI Gateway — Cross-Cluster Agent Authorization

> **Status: Designed, not built.** One `AIA2ARoutePolicy` authored once and governing
> agent-to-agent traffic across every cluster in the estate. The claim rests on a property
> we already have and competitors structurally do not: **the enforcement plane is already
> one thing.** Both clusters' AKO instances program the same Avi Controller, and the same
> SE fabric serves both — so an allow-list does not care which cluster authored it. What
> remains per-cluster is *authoring* and *mint-time authorization*, which is a smaller
> problem than the one we do not have.
>
> Extends [ai-gateway-a2a.md](ai-gateway-a2a.md) (the `AIA2ARoutePolicy` CRD and per-skill
> authorization) and [ai-gateway-auth.md](ai-gateway-auth.md) (the two auth modes), and is
> the concrete first step of [ai-gateway-multicluster-strategy.md](ai-gateway-multicluster-strategy.md).
>
> **One correctness bug blocks all of it** — the minted subject is a bare ServiceAccount
> name, which two clusters can collide on (§5). Fix that first, whatever else happens here.

---

## 1. The problem

`AIA2ARoutePolicy` is a Kubernetes custom resource. It lives in one cluster. Two things read
it, and today both are cluster-scoped:

- the **issuer**, at mint time — `agent_allowed(caller, target, skill)` decides whether to
  issue a capability token at all;
- **AKO**, at translation time — the allow-list is compiled into the DataScript the SE runs.

So an agent in cluster A calling an agent in cluster B needs a mint-time decision made against
a policy that lives in cluster B, by an issuer that can only read cluster A. That is the gap.
Everything else already works.

## 2. What is already estate-wide

This is the part worth being precise about, because it is the whole argument.

| Layer | Scope today | Work needed |
|---|---|---|
| **Avi Controller** | One, shared by every cluster | none |
| **SE fabric / VIPs** | Shared; `*.ai.avi.com` resolves estate-wide via Avi DNS | none |
| **Enforcement (DataScript allow-list)** | Runs on the shared SEs; indifferent to authoring cluster | none |
| **Token validation (JWKS, `jwt_config`)** | One issuer, one JWKS, trusted by every VS | none |
| **Data path across clusters** | Proven — the `qwen-antrea` remote tier | none |
| **Policy authoring** | Per-cluster CRD | **§4.2** |
| **Mint-time authorization** | Issuer reads one cluster | **§4.3** |
| **Workload identity** | TokenReview against one API server | **§4.3** |
| **Subject identity** | Bare ServiceAccount name — ambiguous | **§5 (blocker)** |

Four rows of work, and none of them are in the data path.

## 3. Why this is not how anyone else does it

Every comparable product enforces in **per-cluster proxies** — Istio sidecars, Kong nodes,
Envoy AI Gateway's per-cluster Envoys. "One policy across clusters" there means replicating
policy to N independent enforcement planes and reconciling their drift. That is the hard part
of multi-cluster policy, and it is the part we do not have, because the load balancer was
always shared infrastructure sitting outside the cluster.

The claim this design supports is therefore stronger than the usual one:

> Not "no proxy in your cluster" — **one policy plane and one enforcement plane across your
> whole estate, because the load balancer already spanned it.**

## 4. Design

### 4.1 Enforcement plane — no change

Each cluster's AKO continues to own the Avi objects for routes in its own cluster
(`created_by = ako-gw-<cluster>-<namespace>`). A policy mirrored into cluster B targets
cluster B's HTTPRoute and produces cluster B's VS — so there is no contention between AKO
instances. The two only collide if two clusters serve the **same hostname**, which is a
routing conflict independent of this design and should be rejected at admission.

### 4.2 Policy plane — a designated policy cluster

One cluster (or [vDefend SSP](ai-gateway-multicluster-strategy.md), which is the version with
the portfolio story) holds the canonical `AIA2ARoutePolicy` objects. Two implementation
options:

**(a) Mirroring controller (recommended first).** A small controller watches the policy
cluster and creates read-only copies in each member cluster, stamped
`ai.ako.vmware.com/managed-by: estate-policy`. AKO is unchanged — it keeps reading local CRDs.
Local edits to a mirrored object are reverted, which must be visible in the object's status or
operators will lose an afternoon to it. This is the pattern ACM and Argo already use, so it is
familiar to the audience.

**(b) AKO reads the policy cluster directly** via a `--policy-cluster` kubeconfig. Fewer moving
parts, but it makes AKO multi-cluster-aware, which is a significant scope change to a component
that is deliberately per-cluster today. Not recommended for a first cut.

### 4.3 Mint plane — an estate-aware issuer

The issuer gains a cluster map. Everything else about the exchange in
[ai-gateway-auth.md](ai-gateway-auth.md) is unchanged.

A projected ServiceAccount token carries an `iss` claim naming the cluster that issued it
(`--service-account-issuer`). That is the routing key:

```
POST /exchange  { target, skill, task }  + Authorization: Bearer <SA token>
   |
   |- read `iss` from the SA token (unverified, routing only)
   |- look up the cluster in the estate map
   |- verify the token against THAT cluster              <- §4.3.1
   |- subject := cluster / namespace / serviceaccount    <- §5
   |- agent_allowed(subject, target, skill) against the POLICY CLUSTER
   `- mint { sub, skill, target, jti, aud, exp=60s }
```

`iss` is used only to select which cluster to verify against. It is not trusted for identity —
verification is what establishes that, and a token claiming an unknown issuer is rejected
before anything else happens.

#### 4.3.1 Two ways to verify a remote cluster's token

**TokenReview per cluster (recommended).** The issuer holds a ServiceAccount and kubeconfig for
each member cluster with `create` on `tokenreviews`, and calls the issuing cluster's API server.
This is what `review_sa_token` already does, N times instead of once. It is the strongest option
because TokenReview also checks that the ServiceAccount and its pod still **exist** — a bound
token from a deleted pod fails.

**Offline JWKS validation (scale path).** Bound ServiceAccount tokens are ordinary OIDC JWTs, and
clusters publish `/.well-known/openid-configuration` and a JWKS. The issuer could validate the
signature locally with no per-cluster credential and no per-request API call. Cheaper and it
scales, but it **cannot see revocation** — a token from a deleted pod still verifies until it
expires. This is the same mechanism, and the same weakness, as the SE's own JWT validation.

Start with TokenReview. Reach for JWKS only if exchange volume makes the API-server round trip a
problem, and state plainly which one is running, because the security properties differ.

### 4.4 Trust — unchanged

One issuer, one JWKS, one `AIGatewayAuthPolicy` issuer configuration per surface. Adding
clusters does not add issuers, which is the property that keeps the SE side simple.

## 5. Blocker — the subject is ambiguous across clusters

`_exchange` resolves `ns, sa = review_sa_token(...)`, checks the namespace, and then mints
**`sub = sa`** — the bare ServiceAccount name. The cluster is not represented at all and the
namespace is discarded.

Within one cluster that is merely lossy. Across an estate it is wrong: two clusters each running
`mcp/agent-hub` produce **identical subjects**, and every downstream decision then treats them as
the same agent:

- the A2A allow-list authorizes one when it meant the other,
- the token budget counter merges two consumers into one bucket,
- the usage ledger attributes one cluster's spend to the other,
- an audit cannot distinguish them after the fact.

The last one is why this is a blocker rather than a cleanup task: it is unrecoverable in
retrospect. Records written under the ambiguous subject cannot be split later.

**Fix.** Make the subject `<cluster>/<namespace>/<serviceaccount>` throughout. Three cautions,
all of which have bitten adjacent code already:

- **Do not use `:` as the separator.** It is the counter-key separator in
  [datascript.go](../../ako-gateway-api/aigateway/datascript.go)
  (`<limit>:<identity>:<epoch>`), and a subject containing `:` makes those keys ambiguous in
  exactly the way this fix exists to prevent.
- **Check it survives `_safe`.** The usage record is pipe-delimited and `_safe` reduces each
  field to a restricted character set; a separator that `_safe` rewrites turns two subjects back
  into one.
- **It is a data migration.** Counters, ledger records and any dashboard grouping key change
  shape. Do it while the estate is still ours.

## 6. Prerequisite — split the audience

`issuer.py` already documents this: the SE validates a single audience per
`AIGatewayAuthPolicy` (`Audiences[0]`), one shared policy covers the whole estate, so per-target
audience binding is a simultaneous cutover and is defaulted off.

Cross-cluster makes that a prerequisite rather than an annoyance. More surfaces across more
clusters, still one audience, and the cutover now spans clusters owned by different operators.
Split `AIGatewayAuthPolicy` per surface **before** widening the estate, not after.

Until it is split, audience binding is not the control — the `target` claim is, checked by the
DataScript. That is weaker and should be described as such.

## 7. Sequence — a cross-cluster call

```
agent-hub (cluster A)        issuer         policy cluster      SE fabric     log-collector (B)
      |                        |                  |                 |                 |
      | POST /exchange         |                  |                 |                 |
      | + SA token (iss = A)   |                  |                 |                 |
      |----------------------->|                  |                 |                 |
      |            TokenReview -> cluster A API    |                 |                 |
      |            sub = A/mcp/agent-hub           |                 |                 |
      |                        |----------------->|                 |                 |
      |                        |  AIA2ARoutePolicy |                 |                 |
      |                        |<-----------------|                 |                 |
      |  60s {sub,skill,target}|  allow/deny/503   |                 |                 |
      |<-----------------------|                  |                 |                 |
      |                                                             |                 |
      | POST https://log-collector.ai.avi.com?jwt=...               |                 |
      |------------------------------------------------------------>|                 |
      |               SE validates JWT (one JWKS); DataScript checks |                 |
      |               the skill against the allow-list AKO-B compiled|                 |
      |                                                             |---------------->|
```

The right-hand half of that diagram works today. Only the left-hand half is new.

## 8. Failure modes

| Failure | Behaviour | Mitigation |
|---|---|---|
| Policy cluster unreachable | Issuer cannot evaluate — **503 `policy_unavailable`, retryable**, deliberately *not* a denial | Cache last-known policy with an explicit staleness bound; log which was used |
| Member cluster API server down | Its agents cannot mint; other clusters unaffected | Blast radius is one cluster, which is correct |
| Issuer down | No new grants estate-wide; **in-flight 60s tokens keep working** | Stateless — run replicas; the short TTL makes an outage a stall, not a break |
| Mirror lags the policy cluster | A revoked grant stays mintable for the lag window | Bound the lag and state it; revocation is not instant and the doc must not imply it is |
| Two clusters serve one hostname | AKO instances contend for the same Avi object | Reject at admission — a routing conflict, not a policy one |

The 503-not-403 distinction already implemented in `_exchange` is what makes the first row
survivable. A design that collapsed "cannot evaluate" into "denied" would present a
policy-cluster outage as an estate-wide authorization failure.

## 9. What has to be proven

Nothing here is implemented and three assumptions are load-bearing:

1. **`iss` reliably identifies the cluster** on projected tokens in both OpenShift and upstream
   Kubernetes, and the two clusters do not share an issuer URL. Cheap to check; do it first.
2. **Cross-cluster TokenReview is reachable and acceptable** — the issuer needs a credential in
   every member cluster, which is an ask of whoever owns those clusters and may be the real
   adoption constraint rather than anything technical.
3. **Mirrored policies reconcile cleanly** in AKO with no ownership fight, and a mirrored object
   is visibly read-only to an operator.

## 10. Phases

| Phase | Content | Gate |
|---|---|---|
| 0 | **Qualified subject** (§5) — `<cluster>/<namespace>/<sa>` through tokens, counters, ledger | do this regardless of the rest |
| 1 | Split `AIGatewayAuthPolicy` per surface (§6); enable per-target audience | unblocks real audience binding |
| 2 | Estate-aware issuer: cluster map, per-cluster TokenReview (§4.3) | proves cross-cluster mint |
| 3 | Policy mirroring controller (§4.2a) | one authored policy, many clusters |
| 4 | SSP as the policy plane | the portfolio story |

Phase 0 stands alone and should be done whether or not any of the rest is funded.

## 11. Related docs

- [ai-gateway-a2a.md](ai-gateway-a2a.md) — the `AIA2ARoutePolicy` CRD and per-skill authorization
- [ai-gateway-auth.md](ai-gateway-auth.md) — auth modes, the exchange, and the JWKS constraint
- [ai-gateway-multicluster-strategy.md](ai-gateway-multicluster-strategy.md) — why multi-cluster at all
- [ai-gateway-multisite.md](ai-gateway-multisite.md) — the proven cross-cluster data path
- [ai-gateway-token-ledger.md](ai-gateway-token-ledger.md) — the records the subject change touches
