# AI Gateway — Multi-Provider LLM Routing + Health-Based Failover (design)

> **Status: design. Half of the substrate it assumed is now built** — updated 2026-09-05.
>
> - **External-provider tiers shipped 2026-08-09** as `AIModelRoutePolicy.tiers[].provider`:
>   AKO authors the FQDN pool with backend TLS/SNI and the DataScript rewrites path/Host and
>   injects the key from a Secret. So build-checklist items 2 (external pool, key inject) and
>   3 (no DataScript change) are done, in a different shape from the `ModelBackendRef.External`
>   sketched below.
> - **Remote-site tiers shipped 2026-08-23** (`tiers[].remote`) and brought the other missing
>   piece with them: an FQDN pool with an attached **health monitor**, verified cross-cluster.
>
> What remains genuinely unbuilt is the thing this document is about: **a tier's pool group
> holding more than one member**, priority-labelled, so a provider going down fails traffic
> over rather than failing it. Today every provider and remote tier is a one-member pool group.
> The nearest specified version of that shape is
> [ai-gateway-datacenter.md §6](ai-gateway-datacenter.md) (`tiers[].sites[]`, with priority,
> weight and drain) — worth reconciling with the `Fallbacks` design here rather than building
> both.

**Goal:** route a model tier to a *primary* LLM provider and fail over to *backup* provider(s)
on health failure / rate-limit / cost-ceiling — using Avi's native LB muscle (priority pools +
health monitors + GSLB), not bolted-on proxy logic. Doubles as the demand evidence for RFE-4761
(SE-native streaming), which the response-side controls depend on for real streaming providers.

## Design principle: failover is Avi config, not new datascript logic

Today the model-route datascript resolves a tier and calls `avi.poolgroup.select(pg)` on the
tier's Pool Group (`modelroute_datascript.go` — `GenerateModelRouteScripts`, `TIER_PG` map,
line ~144). **That line does not change.** Each tier already maps to one Pool Group; Avi Pool
Groups already support:
- **priority labels** on members → all traffic to the highest-priority *healthy* pool, automatic
  failover to the next when the primary is marked down;
- **health monitors** per pool → active (GET /v1/models) or passive (mark-down on 5xx/429);
- **GSLB** → cross-region/provider failover if desired.

So failover = give each tier's Pool Group multiple members (provider pools) with priority labels
and health monitors. The "code" is unchanged; the value is Avi doing what it has done for 15 years.

## CRD changes (`AIModelRoutePolicy`, modelroute_types.go)

Extend `ModelTier` so a tier can name an ordered set of backends, and let a backend be an
*external* provider (not just a cluster Service):

```go
type ModelTier struct {
    Name       string            `json:"name"`
    BackendRef ModelBackendRef   `json:"backendRef"`            // existing: primary (unchanged)
    Fallbacks  []ModelBackendRef `json:"fallbacks,omitempty"`   // NEW: ordered backups
    Health     *ProviderHealth   `json:"health,omitempty"`      // NEW: monitor config (optional)
}

type ModelBackendRef struct {
    Group string `json:"group,omitempty"`
    Kind  string `json:"kind"`            // "Service" (existing) | "Provider" (NEW)
    Name  string `json:"name"`
    External *ExternalProvider `json:"external,omitempty"`      // NEW when Kind=Provider
}

type ExternalProvider struct {
    Host            string           `json:"host"`               // e.g. api.openai.com
    Port            int32            `json:"port,omitempty"`     // default 443
    TLS             bool             `json:"tls,omitempty"`      // default true for external
    SNI             string           `json:"sni,omitempty"`
    PathPrefix      string           `json:"pathPrefix,omitempty"` // rewrite to provider path
    APIKeySecretRef *SecretKeyRef    `json:"apiKeySecretRef,omitempty"` // Bearer key injection
    ModelRewrite    map[string]string `json:"modelRewrite,omitempty"`   // local→provider model name
}

type ProviderHealth struct {
    Path            string `json:"path,omitempty"`             // default /v1/models
    IntervalSeconds int32  `json:"intervalSeconds,omitempty"`  // default 5
    FailOn5xx       bool   `json:"failOn5xx,omitempty"`        // passive mark-down
    FailOn429       bool   `json:"failOn429,omitempty"`        // rate-limit → failover
}
```

## Translator changes (`ApplyModelRoutePolicy`, avi_model_l7_translator.go ~line 448-592)

For each tier, instead of one pool member, build the Pool Group with **primary + fallbacks as
priority-labelled members**:

1. Primary member: `priority_label` high (e.g. "10"); each fallback descending ("5", "1").
2. For a `Kind=Service` backend → existing pool build (unchanged).
3. For a `Kind=Provider` backend → build an Avi pool whose:
   - server = `External.Host:Port`, `ssl_profile`/SNI set when TLS (external HTTPS terminate-reinit);
   - **request header rewrite** injecting `Authorization: Bearer <key>` from `APIKeySecretRef`
     (AKO reads the Secret in-cluster, same pattern as JWKS fetch in `jwt_rest.go`);
   - **path rewrite** to `External.PathPrefix`; optional `ModelRewrite` for provider model names;
   - **health monitor** attached (active GET `Health.Path`, and/or passive fail-on-5xx/429).
4. Attach all members to the tier's existing Pool Group (the one the datascript already selects).

Everything downstream — datascript, token policy (`ai_tier` reqvar), guardrails — is unchanged.

## What is NOT in v1 (be honest — capability vs maturity)

- **Cross-schema translation** (OpenAI ⇄ Anthropic request/response bodies) is a large, separate
  lift. v1 fails over between **OpenAI-compatible** endpoints (OpenAI → Azure OpenAI → self-hosted
  vLLM) where the schema is identical. This demos the full value without the translation project.
- **Cost-ceiling-triggered failover** beyond rate-limit (429) needs a spend counter in the
  datascript/token policy → phase 2.

## Demo scenario (works TODAY, no streaming needed)

Tier `standard`: primary = `mock-openai` pool (priority 10), fallback = `mock-anthropic` (priority 5).
- Baseline: traffic → primary.
- Kill primary (scale to 0 / health monitor marks down / trip 429): PG fails over to backup,
  **zero dropped requests** — shown live on the topology + pool-group member view.
- Narrative: "competitors bolt failover onto a proxy; Avi *is* the enterprise LB — health
  monitors, priority pools, GSLB across providers/regions are native."

## The streaming boundary → RFE-4761 (the RFE writeup section)

Failover splits into two layers; only the second needs SE-native streaming:

| Capability | Needs streaming? | Status |
|---|---|---|
| Connection/health failover to healthy provider (new requests) | **No** | works today (native PG) |
| Request routing (read model from body → tier → PG) | No | works today (32KB body buffer) |
| Response-side guardrails/DLP on the answer (non-streaming) | No | works today (buffer+inspect) |
| **Response-side guardrails/token metering on a STREAMED (SSE) answer** | **YES** | **RFE-4761** |
| **Clean mid-stream failover** (provider dies mid-SSE response) | **YES** | **RFE-4761** |

**RFE framing:** failover is the demand-backed use case that surfaces the boundary. Point it at a
real streaming provider and the response-side controls hit the wall exactly where RFE-4761 applies —
turning "we'd like streaming" into "this differentiated, customer-wanted capability is gated on one
platform investment." Do NOT imply mid-stream failover works today; that gap *is* part of the RFE ask.

## Build checklist

1. CRD: `ModelTier.Fallbacks`, `ModelBackendRef.External`, `ProviderHealth` + schema regen.
2. Translator: priority-labelled PG members; external pool (server/SNI/TLS); API-key header inject
   from Secret; health monitor attach.
3. Datascript: **no change** (verify `avi.poolgroup.select` still drives it).
4. Demo manifests: mock-openai + mock-anthropic backends, an `AIModelRoutePolicy` with fallback.
5. Unit tests following `modelroute_test.go` (PG has N priority members; external pool shape).
6. Land on `feature/ai-a2a-gateway` (carries the WAF + SSN fixes already).
