<!--
  DESIGN. Companion to ai-gateway-datacenter.md: that doc describes the ESTATE and its
  POLICY (tiers, budgets, which cluster serves what). This one describes the same estate
  as a NETWORK — zones, VIPs, routing, firewall posture — and takes one decision that
  doc left open: collapsing identity onto a single auth profile.
  Sections marked ⚠️ are design or unverified; everything else describes shipped behaviour.
-->

# AKO AI Gateway — Customer Datacenter Reference Architecture

> **Status: design.** The data plane is shipping. This document is the shape it takes
> inside a customer's datacenter: one trust root, three front doors, and a network the
> customer's security and network teams have to sign off on. Read
> [ai-gateway-datacenter.md](ai-gateway-datacenter.md) first for the estate and the
> policy; this is the network and identity companion to it.

**The claim being made.** Every LLM, MCP and agent-to-agent call in the datacenter enters
through an Avi Service Engine, is authenticated against **one** trust root, inspected,
tiered, metered and routed there — and no AI proxy runs inside any application cluster.

---

## 1. The customer's starting position

The architecture is only interesting if it fits what a real DC already has. Assume:

| Already there | Consequence for this design |
|---|---|
| Avi (NSX ALB) as the enterprise load balancer, one Controller per DC | No new product to buy, no new tier to run. The AI gateway is L7 config on an existing platform. |
| An enterprise IdP (Entra ID / Okta / Ping), already the source of human identity | It stays the source of human identity — but §3 puts it *behind* a broker, not beside one. |
| 10–20 Kubernetes clusters, a mix of prod and non-prod | Serving stays distributed. Policy does not. |
| GPUs in two or three clusters, not everywhere | *Which cluster serves a request* is a per-request scheduling decision, not a DNS record. |
| A SIEM, a CMDB, and a chargeback process that predate AI | The gateway must feed those, not replace them. §6. |
| Firewall zones between application tiers, enforced and audited | §4 is the part of this document that gets scrutinised. |

The estate used throughout is the one from the companion doc: fifteen clusters in `dc1`,
five production (`p01`–`p05`, H100 in `p01`/`p02`), ten non-production (`n01`–`n10`).

---

## 2. Three front doors, not one, and not fifteen

The estate has three *surfaces*, and they are separate VIPs:

| Surface | FQDN | Carries | Why it is its own VIP |
|---|---|---|---|
| Inference | `llm.ai.dc1.example.com` | OpenAI-compatible model traffic | Body-aware routing, token metering, the largest bodies |
| Tools | `mcp.ai.dc1.example.com` | MCP tool calls | Per-tool RBAC on the JSON-RPC method; a different allow-list model |
| Agents | `a2a.ai.dc1.example.com` | Agent-to-agent tasks | East-west by nature; different affinity and egress rules |

Non-production repeats the three under `*.np.ai.dc1.example.com` on its own VIPs, its own
SE group and its own Avi tenant.

**Why not one VIP for all three.** They differ in what "authorised" means (a model alias, a
tool name, a peer agent), in body size, and in blast radius. A DataScript defect on the tool
surface should not take inference down. **Why not one per cluster.** Fifteen copies of every
policy, fifteen ledgers, fifteen trust anchors, and no estate view — the argument is made in
full in §2 of the companion doc.

So: **six VIPs in the DC** (three surfaces × two environments), not ninety.

---

## 3. Identity: one auth profile

This section overrides §8 of the companion doc, which trusted a different issuer per
environment and, per the estate as built, a different issuer for humans and for workloads.

### 3.1 What "one auth profile" actually forces

Avi's two JWT validation paths are mutually exclusive on this build
([ai-gateway-auth.md](ai-gateway-auth.md)):

- `AUTH_PROFILE_OAUTH` / `SSO_TYPE_OAUTH` — browser auth-code flow, claims readable in a
  DataScript, but a machine client presenting `Authorization: Bearer` gets a 302.
- `AUTH_PROFILE_JWT` / `SSO_TYPE_JWT` with `jwt_location = QUERY_PARAM` — machine-correct
  401s, claims readable, token in the URL.

One profile therefore means **choosing one**, and in a datacenter the traffic is
overwhelmingly machine-to-machine: applications, batch jobs, CI, and agents. The choice is
`jwtQuery`, and it forces one clean architectural rule:

> **Humans never authenticate at an AI VIP.** They authenticate to a portal — the AI Gateway
> console, or their own application — which holds the OIDC session and calls the gateway as a
> workload, carrying the human's identity in the token it requests.

That is a simplification worth having on its own merits. It means one profile type, one code
path for claims, and identical budget, RBAC and metering behaviour on all three surfaces.

### 3.2 The broker is the only issuer

Three ways to get to a single trust root:

| Option | Shape | Verdict |
|---|---|---|
| **A — enterprise IdP is the issuer** | Every agent is an IdP client; workloads use client credentials | **Rejected.** The IdP must then hold a client per agent, and a typical Entra/Okta tenant will not mint 60-second, per-skill, `jti`-bearing tokens at agent rates. Mint-time authorisation is lost. |
| **B — federate the in-cluster keys into the IdP** | The broker's JWKS published under the IdP's issuer | **Rejected.** Not offered by SaaS IdPs. |
| **C — a workload identity broker in front of the IdP** | The broker is the sole issuer the gateway trusts; it authenticates humans *upstream* via OIDC and workloads via Kubernetes `TokenReview` / SPIFFE | **Recommended.** |

Option C is a small evolution of what the estate already runs: the in-cluster issuer already
exposes `POST /exchange` (TokenReview → mint-time authorisation → a short audience-, skill- and
`jti`-bound token) and `POST /persona` for console identities. Making it the *only* issuer
means adding the OIDC client leg for humans and retiring every other trust anchor.

```
   human ──OIDC code flow──► enterprise IdP ──┐
                                              ├──► BROKER ──► one signed JWT ──► SE
   workload ──SA token──► TokenReview ────────┘   (sole issuer, own JWKS)
```

What the DC gains by having a broker rather than pointing the gateway at the IdP directly:

- **One JWKS, one rotation, on your cadence.** AKO *snapshots* the JWKS into the generated
  configuration, so a key rotation is an estate-wide reconcile, not a per-cluster refresh. You
  do not want that event scheduled by someone else's IdP team.
- **Mint-time authorisation survives.** Whether a caller may use a tool is decided when the
  token is minted, by something that can read the agent registry — not by a claim the caller
  asks for.
- **You control token size.** Avi rejects a request line over 12 288 bytes
  (`client_max_header_size`, measured). Enterprise IdP tokens carrying full group memberships
  get large, and with `jwtQuery` the token rides in the URL — so token size is a hard
  operational limit. A broker mints the minimum claim set.
- **No IdP in the request path.** With `jwtQuery` the SE validates against the embedded JWKS
  and never calls out. One fewer firewall rule, one fewer runtime dependency, and an IdP
  outage does not stop traffic — it only stops *new* tokens being minted.

### 3.3 The one place to push back on "one"

Prod and non-prod should **not** share one auth profile object. The companion doc is right
that a different issuer and audience per environment *is* the environment boundary: a non-prod
token presented to a prod VIP then fails signature or audience validation before any policy
runs. Collapse that and the boundary becomes a claim check in Lua — enforcement moved from the
SE's crypto into your own code.

So the recommendation is precise: **one auth profile *pattern*, instantiated twice** — same
broker software, same CRD, same claim set, two audiences, two Avi objects, two tenants. Within
an environment, all three surfaces share one profile.

```yaml
# identical in shape on all three prod surfaces; the audience differs in non-prod
spec:
  authMode: jwtQuery
  jwt:
    issuer:    https://broker.ai.dc1.example.com
    jwksUri:   https://broker.ai.dc1.example.com/jwks
    audiences: [ai-dc1-prod]          # ⚠ Audiences[0] only — a change is a hard cutover
    identityClaim: sub
    forwardClaims: [groups, tier, tools]
```

### 3.4 The cost of this choice, stated plainly

`jwtQuery` puts a credential in the URL, and **on Avi 30.2.1+ that credential cannot be kept
out of the client log.** `uri_query_field_rules` match the parameter *name*, and even when
masking is configured correctly the raw token still appears in the log's `orig_uri`
("Unparsed URI") field. This is measured, not theoretical, and no configuration fixes it.

What makes it survivable, and what a security team will want written down:

1. **Sixty-second tokens.** A credential in a log that a reader sees minutes later is expired.
2. **Audience- and `jti`-bound.** Replay is bounded to one surface and one token.
3. **Log access is privileged.** Avi client logs move to the same access tier as any other
   credential-bearing log, and the SIEM ingest path masks the `jwt=` parameter on the way in.
4. **The RFE is open** — a `Bearer` token that is both validated *and* readable by policy
   removes the trade-off entirely, and is tracked in
   [rfe-se-ai-native-callout.md](rfe-se-ai-native-callout.md).

If the customer's policy forbids credentials in URLs under any compensating control, this
architecture does not fit today, and that belongs in the first meeting rather than the fifth.

---

## 4. Network architecture

### 4.1 Zones

```
  ZONE C — CLIENTS                          ZONE A — AI SERVICE
  app clusters p01..p05 / n01..n10          VIP subnet 10.20.8.0/22   (prod)
  CI runners, notebooks, the console                   10.20.12.0/22  (non-prod)
        |                                   SE groups: se-ai-prod, se-ai-nonprod
        |  443/TLS                          SE data NICs in A; VIPs advertised by BGP
        +----------------------------------> +---------------------------------+
                                             | llm.ai.dc1  .30  EVH parent VS  |
                                             | mcp.ai.dc1  .31                 |
                                             | a2a.ai.dc1  .32                 |
                                             | WAF . ICAP . JWT . tier . meter |
                                             +----+-----------+----------+-----+
                                                  |           |          |
        +-----------------------------------------+           |          +------------+
        v                                                     v                       v
  ZONE S — SERVING                              ZONE V — SECURITY SVCS       ZONE X — EGRESS
  per-cluster ingress VIPs                      ICAP classifier pool         proxy/NAT to public
  llm.p01.ai.dc1 ... llm.p05.ai.dc1             (1344 or TLS)                model providers
  llm.n01.ai.dc1 ... llm.n10.ai.dc1                                          443, allow-list

  ZONE M — MANAGEMENT / SHARED
  Avi Controller cluster . broker (IdP-facing) . Avi DNS VS . console + ledger collector
  enterprise DNS . NTP . SIEM
```

Five zones, and the interesting property is how few of them talk to each other. Clients only
ever reach Zone A. Serving clusters never reach clients. Only the SEs cross from A into S, V
and X.

### 4.2 VIP placement and routing

- **VIPs live in a dedicated AI service subnet**, not in a cluster's node network. The subnet
  is the unit the firewall policy is written against, and it survives cluster rebuilds.
- **BGP with ECMP from the SE group** for anything above a single SE pair. Static routes work
  and are simpler; they do not scale out and they do not fail over cleanly.
- **One SE group per environment**, not per surface. Surfaces share SEs; environments do not.
  That gives prod and non-prod separate failure domains and separate CPU budgets for the WAF
  and ICAP work, without paying for six SE groups.
- **⚠ VIPs float.** In the estate as built, renaming the cluster or flipping
  `useReadableObjectNames` moved every VIP. Anything that hardcodes a VIP address — the
  console, an agent's pinned peer address, a firewall rule written to a host address — breaks.
  **Write firewall rules to the VIP subnet, never to a VIP address**, and give every consumer a
  DNS name.

### 4.3 DNS

```
  enterprise DNS --conditional forward: ai.dc1.example.com--> Avi DNS VS (Zone M)
```

Each serving cluster's AKO publishes its own `llm.<cluster>.ai.dc1.example.com` into the Avi
DNS VS when it creates its EVH child VS. Nothing maintains a peer list, and the front-door
pools resolve site FQDNs at the SE. Clients resolve only the three front-door names.

### 4.4 Firewall matrix

The table a network security team will actually ask for:

| # | Source | Destination | Port | Purpose | Notes |
|---|---|---|---|---|---|
| 1 | Zone C (client cluster node/pod nets) | Zone A VIP subnet | 443/TCP | All AI traffic | The only client-facing rule |
| 2 | SE data (Zone A) | Zone S cluster ingress VIPs | 443/TCP | Tier → site | Long-lived; see §4.6 |
| 3 | SE data (Zone A) | Zone V ICAP pool | 1344/TCP or 11344/TLS | Semantic guardrails | In the request path |
| 4 | SE data (Zone A) | Zone X egress proxy | 443/TCP | Provider tiers | FQDN allow-list on the proxy |
| 5 | Console/collector (Zone M) | Zone A VIP subnet | 443/TCP | `/v1/admin/usage` drain | Out-of-band, no keep-alive; §6 |
| 6 | AKO, entry clusters | Avi Controller (Zone M) | 443/TCP | Config reconcile | Control plane only |
| 7 | Avi Controller (Zone M) | SE management NICs | 22, 8443/TCP | SE lifecycle | Standard Avi |
| 8 | Broker (Zone M) | Enterprise IdP | 443/TCP | Human OIDC leg | Not in the request path |
| 9 | AKO, entry clusters | Broker `/jwks` | 443/TCP | JWKS snapshot at reconcile | **Not** an SE rule |
| 10 | SEs, console | SIEM / syslog | 514, 6514 | Logs, ledger export | §6 |

Rule 9 is the one that surprises people: **the SE never fetches JWKS.** AKO does, at reconcile
time, and embeds it. That is why an IdP or broker outage does not stop request validation.

### 4.5 TLS, and where plaintext exists

Be direct about this, because it is the second architectural objection after token-in-URL:

- **TLS terminates at the SE.** It has to. Routing on the `model` field, enforcing a token
  budget, running a DLP signature over a prompt, and metering `usage` from a response all
  require the body in clear.
- **Re-encrypt to the backend.** SE → pool TLS is standard; SE → backend **mTLS with SPIFFE
  identities** is designed in [ai-gateway-backend-mtls.md](ai-gateway-backend-mtls.md) and is
  the right answer where the serving cluster must authenticate the gateway.
- **There is no "end-to-end encrypted and inspected" option.** A customer who requires opaque
  transport from client to model gets DNS-based steering and no AI policy at all. That is a
  different product, and it is worth saying so early.

### 4.6 Sizing: LLM traffic is not web traffic

| Property | Web | LLM | Design consequence |
|---|---|---|---|
| Connection lifetime | milliseconds | seconds to minutes | Size SEs by **concurrent connections**, not requests/sec |
| Response size | KB | tens to hundreds of KB | Response buffering (256 KB) is memory per concurrent request |
| Request size | small | tens of KB, sometimes MB | 32 KB request buffering; anything larger is truncated and fails closed |
| Latency budget | tens of ms | seconds | Guardrail latency is affordable here in a way it never is for web |
| Idle behaviour | short timeouts | long generations | Raise pool and client idle timeouts, or long generations are cut mid-answer |

Measured guardrail cost on the lab estate, for the latency-budget conversation: **~36 ms** for
a WAF signature decision, **~782 ms** for the ICAP semantic classifier plus the gray-zone
judge. Against a multi-second generation that is acceptable; against a 50 ms API call it would
not be.

Jumbo frames on the SE data VLAN into Zone S are worth the change at this payload size.

### 4.7 Failure domains

| Failure | Effect | Why |
|---|---|---|
| Entry cluster down | **Config freeze, not outage** | SEs keep serving the last reconcile |
| Avi Controller down | No config changes; data plane serves | Standard Avi behaviour |
| Broker down | No *new* tokens; validation unaffected | JWKS is snapshotted into the SE config |
| One serving cluster down | Health monitor drops it from the tier's pool group in ~10 s | A tier is a set of sites |
| ICAP pool down | ⚠ **fail-open window** on pod reschedule | Measured ~3 min in the lab; needs a static ICAP fleet, not a rescheduled pod |
| SE group down | That environment's AI traffic stops | The accepted cost of one enforcement point; §7 |
| AKO rollout | ⚠ front door returns 500 for **~90 s** while it reconciles | Roll in a change window; do not roll back mid-reconcile |

---

## 5. The request path, as network hops

```
 1  client -> llm.ai.dc1:443           Zone C -> A    TLS terminates at the SE
 2  parent EVH VS -> child VS          in-SE          host match picks the route
 3  WAF signature DLP                  in-SE          ~36 ms; blocks in prod, logs in non-prod
 4  ICAP classifier + judge            A -> V -> A    ~782 ms; blocks in prod, logs in non-prod
 5  JWT validation (one profile)       in-SE          embedded JWKS; no call-out
 6  HTTP_REQ_DATA: model -> tier       in-SE          entitlement may downgrade the tier
 7  token budget check                 in-SE          429 + Retry-After in prod; count only in non-prod
 8  poolgroup.select(tier)             in-SE          priority + health picks the SITE
 9  SE -> llm.p01.ai.dc1:443           Zone A -> S    re-encrypted, optionally mTLS
10  HTTP_RESP_DATA: parse usage        in-SE          budget counter + ledger record
11  collector drains the ring          M -> A         out-of-band, every ~5 s
```

Steps 6 and 8 are the pair that makes an estate work: **the model name picks the tier, the
pool group picks the site.** Step 6 is body-aware and cannot be done by DNS. Step 8 is
health-aware and should not be done in Lua.

---

## 6. Where visibility lands in this network

The ledger ([ai-gateway-token-ledger.md](ai-gateway-token-ledger.md)) is an out-of-band
collector, and its network requirements are unusual enough to state explicitly:

- **It polls the VIP, not the SEs.** An Avi SE answers for a VS on its *VIP*, not on its
  management or data-interface address, so there is nothing else to address (rule 5).
- **Keep-alives are disabled on the collector's client.** The load balancer only re-picks an SE
  on a new connection; a pooled connection pins every poll to one SE and lets the other SEs'
  rings age out unread. With BGP/ECMP in front (§4.2) this is not optional.
- **One cursor per ring instance**, so being bounced between SEs neither double-counts nor
  misses records.
- **⚠ Per-VS logging does not survive object recreation.** `full_client_logs` is a per-VS
  setting, and the operations that recreate child VSes — a cluster rename, the readable-names
  flip — silently wipe it. Anything depending on client logs needs a post-change check, and the
  ledger deliberately does not depend on them.
- **Export, don't replace.** Hourly rollups go to the SIEM and to the existing chargeback
  process via `GET /api/usage/export?format=csv`; per-request rows stay in the console for
  drill-down.

---

## 7. Pros and cons

### 7.1 What this shape buys

| Pro | Why it matters in a customer DC |
|---|---|
| **No AI proxy in any application cluster** | Nothing to install, patch or upgrade fifteen times. AI policy becomes L7 config on a platform the network team already runs. |
| **One enforcement point per environment** | Authentication, guardrails, entitlement, budget and metering are the *same code* for every workload. There is no second path to forget to secure. |
| **One ledger** | "How many premium tokens did payments burn last month" has one answer, not fifteen partial ones. Chargeback stops being a data-engineering project. |
| **Policy in Git, applied once** | A new blocked pattern or a budget change is one pull request against the entry clusters. |
| **Serving stays distributed** | Tiers are sets of sites; a cluster failing or draining is a health event or a one-line commit, not an incident. |
| **Existing platform, existing skills** | The team that runs Avi runs this. No new vendor, no new control plane, no new on-call rota. |
| **Visibility across surfaces nothing else sees** | Tool calls and agent hops sit on the same enforcement point as the model calls. A broker in front of public APIs sees only the model leg. |
| **Shadow-AI discovery comes free** | The SE is the data path, so unregistered egress to public model APIs is visible without adding a sensor. |

### 7.2 What it costs

| Con | Severity | Mitigation |
|---|---|---|
| **TLS must terminate at the SE** | Architectural | Re-encrypt and mTLS to the backend. Cannot be removed — inspection requires plaintext. |
| **Token in the URL, unmaskable in Avi logs** | High | 60-second tokens, `jti`, privileged log access, SIEM-side masking. RFE open. §3.4. |
| **Streaming responses are not metered** | High | Budgets are bypassable on streamed responses today. Either disable streaming at the front door, or accept estimated metering badged as an estimate. The trailer spike is the fix. |
| **One enforcement point is one blast radius** | High | Separate SE groups per environment; every change lands on the non-prod front door first. A DataScript defect is an estate-wide event. |
| **AKO rollout blanks the front door ~90 s** | Medium | Change window; do not roll back mid-reconcile. |
| **JWKS is snapshotted, so rotation is estate-wide** | Medium | Overlap old and new keys through a full reconcile before retiring a key. This is why §3.2 wants the broker's rotation cadence under your control. |
| **Audience change is a hard cutover** | Medium | `Audiences[0]` only. Plan it as a cutover, not a rolling change. |
| **ICAP adds a dependency and a fail-open window** | Medium | Static ICAP fleet, not a rescheduled pod; alert on classifier reachability. |
| **Budgets are token-based, not GPU-aware** | Medium | One 100 K-token request occupies a GPU far longer than a hundred 1 K ones costing the same. Pool concurrency ceilings bound occupancy; true queue-aware steering is an RFE. |
| **VIPs float on some config changes** | Low | Firewall on the VIP subnet; DNS everywhere; never hardcode an address. |
| **Humans cannot hit an AI VIP directly** | Low | A consequence of one auth profile (§3.1). In practice a portal is wanted anyway. |

### 7.3 Against the alternatives

| Option | Pro | Con |
|---|---|---|
| **This — one gateway per surface, on the existing LB** | No new data plane; estate-wide policy and one ledger; sees all three surfaces | TLS terminates at the SE; streaming unmetered; single blast radius |
| **A gateway per cluster** | Small blast radius; cluster teams stay autonomous | Fifteen copies of every policy, fifteen ledgers, fifteen trust anchors, no estate view |
| **In-cluster AI proxy (Envoy AI Gateway, LiteLLM)** | Rich AI-native features today; streaming metered | A second data plane to run and upgrade in every cluster; another hop; policy still fragmented; no network-layer view |
| **SaaS broker (OpenRouter and similar)** | Zero build; excellent model-marketplace visibility | Prompts leave the DC; no view of tool or agent traffic; attribution is per API key, not per identity; useless for on-prem models |

The honest read: the in-cluster proxy is *more capable at the model leg today* — streaming
metering being the clearest example. This architecture wins on everything that is not the model
leg: one identity, one ledger, one policy set, three surfaces, and no new data plane.

---

## 8. Spike before committing

Four unknowns, in the order in which they would sink the design:

1. **Response trailers in a DataScript** — can a post-body event read a trailer without
   buffering? If yes, streaming meters exactly and the largest con in §7.2 disappears.
2. **Per-pool Host rewrite** — a tier with several sites cannot rewrite Host in Lua, because the
   SE picks the pool after the script runs. Confirm the pool-level rewrite (§6.2 of the
   companion doc).
3. **Query-strip before the backend** — the `jwt=` parameter is currently forwarded to the model
   server and lands in *its* logs. Confirm the SE query-rewrite primitive.
4. **Body-buffer units** — `set_response_body_buffer_size` is called with `256` and the
   truncation test compares against 262 144. Production evidence says kilobytes; being wrong is
   silent in both directions.

---

## Related

- [ai-gateway-datacenter.md](ai-gateway-datacenter.md) — the estate, the tiers, the budgets
- [ai-gateway-auth.md](ai-gateway-auth.md) — the two auth modes and why they are exclusive
- [ai-gateway-token-ledger.md](ai-gateway-token-ledger.md) — the record, the collector, streaming
- [ai-gateway-backend-mtls.md](ai-gateway-backend-mtls.md) — SE → backend identity
- [ai-gateway-multisite.md](ai-gateway-multisite.md) — the GSLB-flavoured multi-DC variant
- [rfe-se-ai-native-callout.md](rfe-se-ai-native-callout.md) — the SE-native asks
