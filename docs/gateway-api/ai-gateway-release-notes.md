# AKO AI Gateway — Release Notes

## Model-Based (Quality/Cost Tier) Routing — 2026-06-07

Adds **`AIModelRoutePolicy`** — route inference requests to different backends
based on the requested **model**, organised into quality/cost **tiers**. Like the
other AI Gateway policies it attaches to an `HTTPRoute` via `targetRef` and runs
**entirely on the Avi Service Engine** (no proxy/sidecar). Verified end-to-end on
a live cluster (Avi 31.2.2). Full doc: `docs/gateway-api/model-routing.md`.

### New features

**Route by request `model` → per-tier `InferencePool`**
The SE reads the OpenAI-style `model` from the request body and selects that tier's
Avi Pool Group directly:
- `HTTP_REQ` enables request-body buffering (`set_request_body_buffer_size`, 32 KB);
  `HTTP_REQ_DATA` reads it (`get_req_body`), extracts `model`, resolves the tier
  (exact name or trailing-`*` prefix glob, else `defaultTier`), and calls
  `avi.poolgroup.select(<tier Pool Group>)`.
- Each tier's `InferencePool` keeps its [Inference Extension](inference-extension.md)
  scraper-weighted pod members — model routing chooses the *tier*, the scraper
  chooses the *pod*.
- Backends are declared **in the policy** (`tiers[].backendRef`), so a missing/
  unreconciled policy degrades to the route's own backend (no tiering, no outage).

**Group-based tier entitlement**
Reuse the verified `group` claim from [`AIGatewayAuthPolicy`](ai-gateway.md): a
caller who requests a tier above their entitlement is **downgraded** to their best
allowed tier (or rejected, per `onUnentitled`).

**Per-tier token budgets**
`AITokenRateLimitPolicy` limits can set their budget ceiling by tier via
`groupHeader: "reqvar:ai_tier"` + `groupBudgets` (e.g. `{premium: 500,
economy: 100000}`). The model-route script sets the `ai_tier` reqvar; because the
tier is known only after the body is read, such limits enforce in `HTTP_REQ_DATA`
(classic limits are unchanged).

### Fixes

- **ClusterRole RBAC** — the AKO `ako` ClusterRole now grants
  `aimodelroutepolicies` (+ `/status`); without it the informer was forbidden from
  listing the CRD.

### Known limitations

- **InferencePool tier backends only** — `Service` backends are accepted by the
  schema but not yet built (logged and skipped).
- **32 KB request-body buffer** — `model` is at the JSON start so the head
  suffices; bodies larger than 32 KB are not yet validated.
- Inherits the token policy's **eventually-consistent** counters and **soft RPS**.

### Upgrade notes

- Install the CRD: `kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aimodelroutepolicies.yaml`.
- Requires `inferenceExtension.enabled: true` (tier backends are InferencePools).
- Additive and **gated** — existing routes are unaffected until an
  `AIModelRoutePolicy` targets them.

---

## Token Counters, Dashboard Reset & UI Integration — 2026-06-07

This release adds a read-only usage API and a one-click reset to the AKO AI Gateway,
and wires both into the AI Gateway console. All of it runs **entirely on the Avi
Service Engine** — no in-cluster proxy or sidecar.

### New features

**Read-only token-counters endpoint**
A token-gated endpoint AKO generates on the same VS so a dashboard/UI can show live
per-user token usage.

- `GET /v1/admin/counters?users=alice,bob,carol` with header `X-Admin-Token: <token>`
  → `200 {"window":<sec>,"limit":"<name>","counters":[{"user":"alice","used":300}, …]}`
- Missing / wrong token → `403 {"error":"forbidden"}`
- Reads the **same SE counter key** the response-phase accounting writes, so the
  numbers match enforcement exactly.
- The SE table can't be enumerated, so the caller passes the identities it wants in
  `?users=` (the UI gets its roster from the issuer's `/users` map).
- **Opt-in:** set `ai.ako.vmware.com/admin-token-secret: <secret>` on the
  `AITokenRateLimitPolicy` (Secret key `token`). When set, AKO emits the endpoint and
  adds an SSO `SKIP_AUTHENTICATION` rule for `/v1/admin/` so it isn't OAuth-redirected;
  when absent, the endpoint is not generated.

**Counter reset via epoch bump**
Bump `ai.ako.vmware.com/counter-epoch` on the policy to move every counter to a fresh
keyspace — an instant reset that leaves budgets and the limit name untouched.
```bash
kubectl annotate aitokenratelimitpolicy llm-limits -n inference \
  ai.ako.vmware.com/counter-epoch=2 --overwrite
```

**AI Gateway console: live counters + one-click reset**
The UI now reads authoritative per-user usage from the counters endpoint and resets all
counters with one button (epoch bump via the k8s API). Reset no longer renames the
limit (`-2`/`-3`/…) — the name stays stable.

**OIDC issuer `/users` roster**
The demo OIDC provider exposes its identity→group map at `GET /users` so a UI knows
which users to query without hardcoding them.

### Fixes

- **Counters endpoint 500 on un-seen users** — the SE Lua sandbox raises on
  `tonumber(nil)`; a queried user with no counter yet crashed the request. Now guarded
  (`tonumber(lookup or 0)`).
- **OAuth issuer pool stale IP** — when the in-cluster OIDC issuer pod re-IPs, the
  immutable OAuth pool/authprofile pinned to the dead IP broke login fabric-wide;
  rebuilt against the live endpoint. (Recurs on issuer restart — see Known limitations.)

### Known limitations

- **Streaming responses are not token-metered.** With `stream:true`
  (`Content-Type: text/event-stream`) the SE DataScript can't read the body without
  buffering — and buffering collapses streaming. So streamed requests currently
  **bypass the budget (count 0)**; non-streaming JSON meters exactly. *Use
  non-streaming where budgets must hold.* The proper fix is a native SE capability (a
  per-chunk response-body event, or native LLM token metering) — tracked as an SE RFE.
- **Counters are per-SE / eventually consistent.** With multiple Service Engines a
  user's budget can overshoot (~N_SE) because each SE enforces against its local table.
  Exact, fabric-wide budgets need the native distributed counter — SE RFE.
- **OAuth issuer is a single pod.** Issuer pod restarts re-IP and break the pinned
  OAuth pool until reconciled; an HA issuer is needed for production.

### Documentation

- `docs/gateway-api/ai-gateway.md` — "Dashboard counters endpoint" and "Resetting
  counters" sections.
- `docs/gateway-api/ai-gateway-install.md` — "Part C — Dashboard counters endpoint &
  reset" (enable / read / reset commands).
- `docs/gateway-api/examples/ai-gateway-demo/ai-gateway-policies.yaml` — inline
  `admin-token-secret` / `counter-epoch` annotation docs.
- `docs/gateway-api/examples/ai-gateway-demo/jwt-issuer.yaml` — `/users` endpoint.

### Upgrade notes

- The counters endpoint is **off by default** — existing policies are unaffected until
  you add the `admin-token-secret` annotation and its Secret.
- No CRD schema changes; both new controls are annotations on the existing
  `AITokenRateLimitPolicy`.
