# AKO AI Gateway — Release Notes

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
