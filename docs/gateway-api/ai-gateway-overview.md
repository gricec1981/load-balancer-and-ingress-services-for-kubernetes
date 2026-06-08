# AKO AI Gateway — Overview

The AKO AI Gateway turns the Avi Load Balancer into the entry point for AI traffic on
Kubernetes. It authenticates callers, meters and governs usage, routes requests to the
right model, and inspects content for data loss and abuse — all using the Avi Service
Engines an organisation already runs, with no extra proxies or sidecars. Every capability is
expressed as a Kubernetes CRD that attaches to a Gateway API route and is reconciled by AKO
into native Avi features.

It governs the whole agent loop under **one verified identity**: **inference** calls to LLMs,
**tool** calls to MCP servers, and **agent↔agent** delegation — with the same authentication,
the same policies, and the same usage accounting across all three.

---

## Goals

- **Make the Avi data plane the AI Gateway.** Authenticate, govern, meter, route, and inspect
  AI traffic on the load balancer customers already operate — no new data plane to adopt.
- **Govern the whole agent loop with one identity.** LLM inference, MCP tool calls, and
  agent-to-agent traffic share the same authentication, policies, and usage accounting.
- **Control cost and quality.** Route each request to the appropriate model tier and
  hardware, and keep expensive resources for the traffic entitled to them.
- **Protect the data.** Stop secrets, PII, and known attacks from entering prompts, tool
  arguments, and agent messages — natively, with no model in the hot path.
- **Give operators visibility and control.** Per-consumer and per-group token budgets, usage
  counters, group-based entitlements, and a console to see and drive it all.
- **Stay Kubernetes-native.** Configure everything as CRDs attached to Gateway API routes,
  reconciled into Avi Service Engine capabilities (OAuth/OIDC, DataScripts, WAF, pools,
  persistence).

---

## What's supported today

| Capability | What it provides | Status |
|---|---|---|
| [Authentication](#authentication) | OAuth/OIDC validation and verified identity/claims at the gateway | Available |
| [Inference](#inference) | Metric-weighted load balancing across model-server pods | Available |
| [Token Counting](#token-counting) | Per-consumer / per-group token budgets, counters, rate limits | Available |
| [Model Routing](#model-routing) | Model-aware routing to quality/cost tiers, with entitlements | Available |
| [MCP](#mcp-agenttool) | The same governance for agent↔tool (Model Context Protocol) traffic | Implemented · design evolving |
| [A2A](#a2a-agentagent) | Governance for agent↔agent (Agent2Agent) delegation traffic | In design |
| [Guardrails & DLP](#guardrails--dlp) | WAF-native data-loss prevention and content guardrails | Built · spike-verified |
| [Semantic Guardrails](#semantic-guardrails) | Model-based prompt-injection detection over ICAP | In design |
| [Backend mTLS](#backend-mtls-spiffespire) | SE↔backend mutual TLS with SPIFFE/SPIRE short-lived identity | In design |
| [Multi-Site Delivery](#multi-site-cross-cluster-delivery) | Cross-cluster model routing via AMKO + Avi GSLB | In design |
| [Console (UI)](#console-the-ai-gateway-ui) | Avi-style web console to view and drive the gateway | Built (companion repo) |

The three governance surfaces — **inference**, **MCP**, and **A2A** — and their protocols:

| Surface | Protocol | Governed by |
|---|---|---|
| agent ↔ model | OpenAI-style HTTP | [Authentication](#authentication) + [Model Routing](#model-routing) — available |
| agent ↔ tool | MCP (JSON-RPC + Streamable HTTP) | [MCP](#mcp-agenttool) — implemented, design evolving |
| agent ↔ agent | A2A (JSON-RPC over HTTPS) | [A2A](#a2a-agentagent) — design |

---

## Authentication

The gateway authenticates API consumers with **OAuth/OIDC** at the Service Engine. A caller's
bearer token is validated against the configured identity provider, and the verified identity
and claims — such as the consumer's `sub` and their `group` or `role` — are made available to
every downstream policy and can be forwarded to backends as request headers. One identity
provider serves the whole gateway, so a single login governs everything behind it.

This verified identity is the foundation the rest of the gateway builds on: token budgets are
keyed on it, tier entitlements are decided from it, and MCP/A2A authorization reads the same
claims. Authentication is configured with an `AIGatewayAuthPolicy` attached to a route.

→ [OAuth/OIDC Authentication — `AIGatewayAuthPolicy`](ai-gateway.md#oauthoidc-authentication--aigatewayauthpolicy)

## Inference

The Inference Extension load-balances traffic **within** a model fleet. An `InferencePool`
groups the pods serving a model, and the gateway distributes requests across them using
live metrics scraped from each pod — such as KV-cache utilisation and request-queue depth —
so load follows real serving capacity rather than simple round-robin.

This is the layer the rest of the gateway routes *over*: each model tier and each route
ultimately lands on an inference pool whose members are weighted by current load. It keeps
GPU-bound model servers evenly and efficiently utilised.

→ [Native Inference Extension](inference-extension.md) · [Install guide](inference-install.md)

## Token Counting

The gateway meters **token usage**, not just requests. It reads the token counts from model
responses and maintains running counters per consumer and per group in Service Engine shared
state, enforcing **token budgets** over a time window alongside classic request-rate limits.
Budgets can vary by group, so different tiers of users get different ceilings.

Usage is also exposed through a read-only counters endpoint that the [console](#console-the-ai-gateway-ui)
polls, giving operators a live view of who is consuming how many tokens. Token counting is
configured with an `AITokenRateLimitPolicy` and keys its accounting on the identity
established by authentication.

→ [Token Rate Limiting — `AITokenRateLimitPolicy`](ai-gateway.md#token-rate-limiting--aitokenratelimitpolicy)

## Model Routing

Model routing inspects the incoming request, reads the requested **model**, and steers it to
a backend organised by **quality/cost tier** — for example a premium tier on high-end GPUs, a
standard tier on smaller accelerators, and an economy tier on quantised or CPU hardware. This
turns the model name in each request into a cost-and-quality control: expensive hardware
serves only the requests that should reach it, and everything else lands on cheaper backends.

Tiers can be gated by the caller's verified group, so entitlement decides which callers may
reach which tier, and a caller who asks for a tier they aren't entitled to can be downgraded
to one they are. Model routing is configured with an `AIModelRoutePolicy` and composes with
authentication and token budgets on the same route.

→ [Model-Based (Quality/Cost Tier) Routing](model-routing.md)

## MCP (agent↔tool)

MCP support extends the same governance to **agent↔tool** traffic. Agents call tools over the
Model Context Protocol, and the gateway fronts those tool servers through a dedicated **MCP
Gateway**: it keeps stateful agent sessions pinned to the right backend (using Avi 32.1.1's
native MCP session awareness), authenticates callers against the **same identity provider** as
the LLM gateway, and authorizes individual tool calls by the caller's role — so different job
roles get access to different tools.

Tool servers are onboarded from an **approved MCP registry**, a curated catalog of vetted
servers that the gateway treats as an allow-list. This brings the agent loop's tool half under
one identity and one governance model. MCP is configured with an `AIMCPRoutePolicy`; the AKO
controller and translator are wired, with the design continuing to evolve against Avi's native
MCP features.

→ [MCP Gateway & MCP-Specific Routes](ai-gateway-mcp.md)

## A2A (agent↔agent)

A2A governs the third surface: agents calling **other agents** — delegation and task hand-off
between independent agentic systems over the **Agent2Agent** protocol (JSON-RPC over HTTPS). A
dedicated **A2A Gateway** authenticates callers against the same identity provider, authorizes
**per-skill** (each Agent Card advertises `skills[]`), keeps long-running stateful **tasks**
pinned to the right backend via session affinity, and governs **push-notification egress** so
agents can only call back approved webhooks.

Unlike MCP — which Avi 32.1.1 supports natively — A2A has **no native Avi support** and is
built from generic Avi primitives plus DataScripts, making it architecturally closer to model
routing. It is specified by a new `AIA2ARoutePolicy` CRD and completes the
"govern the whole agent loop under one identity" thesis. It is a design draft, not yet
implemented.

→ [A2A Gateway & Agent-to-Agent Routes](ai-gateway-a2a.md)

## Guardrails & DLP

Guardrails add the **content-inspection** layer: stop secrets, PII, and known attacks from
entering prompts, tool arguments, and agent messages — and (optionally) leaking back out.
Crucially, this runs on the data plane already in place: the Avi Service Engine's native
**WAF** (ModSecurity-based) does the regex matching and request/response-body inspection, so
there is **no proxy, no sidecar, and no model in the hot path**.

An `AIGuardrailPolicy` ships pre-canned **profiles** per surface — `BlockLLM` (DLP +
prompt-injection signatures), `BlockMCP` (DLP + tool-abuse: command-injection / path-traversal
/ SSRF), and `BlockLLMAndMCP` — so an operator drops one CR per route or gateway. AKO **authors**
the Avi WafPolicy from the spec over REST and attaches it to the route's virtual service.

AKO maintains a **built-in signature library** so the profiles work out of the box: **secret**
detectors (AWS / GCP / OpenAI / GitHub / Slack keys, private keys, JWTs), **PII** detectors
(SSN, credit-card, email), **prompt-injection** patterns (ignore-instructions, jailbreak,
reveal-system-prompt, override-safety, role-injection), and **MCP tool-abuse** patterns —
plus per-policy keyword denylists and custom regex. The prompt-injection detectors are
**evasion-resistant ("hardened")**: each emits a normalised pass (lowercase + URL/unicode-decode
+ whitespace removal, so `i g n o r e`, zero-width tricks, and `%`-encoding are caught) and a
base64-decode pass. Each detector compiles to request-phase (phase 2, `ARGS|REQUEST_BODY`)
and/or response-phase (phase 4, `RESPONSE_BODY`) SecRules, and a **mode-delegation** trick keeps
the policy in detection-only while the AKO rules enforce — so `Block` vs `Log` (shadow) is a
per-rule flip, which is exactly what the console's DLP toggle drives.

The AKO side is built; request-body DLP blocking is spike-verified (an AWS key / SSN / API
secret in a prompt returned 403 while a clean prompt passed). The WAF layer is signature/regex
based — it catches known patterns but cannot do semantic detection (see below).

→ [Guardrails & DLP — `AIGuardrailPolicy`](ai-gateway-guardrails.md)

## Semantic Guardrails

Semantic guardrails are the model-based half of `AIGuardrailPolicy`: a **prompt-injection
classifier** the Service Engine calls over **ICAP** to catch the **novel / paraphrased**
injection the signature (WAF) layer provably misses. It preserves the no-proxy thesis — the SE
buffers the request body and *calls* the classifier as a service (exactly as it already calls
the OIDC issuer), then **the SE enforces** the block; the model is never a proxy in the request
path. This is a design draft; nothing is built yet.

→ [Semantic Guardrails (prompt-injection over ICAP)](ai-gateway-guardrails-semantic.md)

## Backend mTLS (SPIFFE/SPIRE)

Authentication secures the **north-bound** hop (caller → gateway); backend mTLS secures the
**south-bound** hop (gateway → model/tool backend). The Service Engine presents a client
certificate to local inference pools and MCP servers and validates theirs, with both sides
using **SPIFFE SVIDs** issued by **SPIRE** — short-lived, automatically rotated workload
identity rather than long-lived shared certs. So a leaked cert is useless within minutes, and
backends can refuse anything that isn't the gateway.

This extends the already-implemented `RouteBackendExtension.BackendTLS` (which does one-way TLS
with server validation today) with an `seClientCert` field for the SE's client cert, plus a
small controller that pulls the SE's SVID from SPIRE and rotates it into Avi at half-life. It is
a design draft; the make-or-break open question is whether the Avi pool can pin a SPIFFE **URI
SAN** rather than just a DNS name (spike-gated).

→ [Backend mTLS with SPIFFE/SPIRE](ai-gateway-backend-mtls.md)

## Multi-Site (cross-cluster) delivery

Multi-site delivery routes an inference request to the right **model tier** *and* the right
**site**, across a fleet of Kubernetes clusters, by composing three existing Broadcom/Avi
capabilities: the per-cluster AI Gateway, model-based tier routing, and **AMKO + Avi GSLB**
global server load balancing. GPUs are scarce, expensive, and scattered across clusters and
regions; this layer lets the gateway understand *which model* a request wants and deliver it to
the *best site* that can serve it — without introducing a new data plane. It is a design draft;
cross-site behaviour is spike-gated.

→ [Multi-Site (Cross-Cluster) Model Delivery](ai-gateway-multisite.md)

## Console — the AI Gateway UI

The AI Gateway ships with an **Avi-Controller-style web console** that makes the whole gateway
visible and operable without hand-editing YAML. It is a single static Go binary with an
embedded Clarity-style SPA, hand-styled to match Avi's dark navy/teal look, and it talks to the
cluster through its own ServiceAccount + RBAC (real CRD reads and writes) and to the Avi
controller's read-only REST API for live data-plane state. It lives in a **companion repo**
(`gricec1981/ai-gateway-ui`), separate from AKO.

What the console gives operators:

- **Dashboard → Topology.** A live left→right graph of **Avi Service Engine → Gateways
  (LLM/MCP) → backends**, with healthy/down connector edges, a KPI strip, and auto-refresh —
  assembled from real SE health, virtual-service oper status, and HTTPRoute backend refs.
- **Dashboard → Live Counters.** Real-time per-consumer / per-group token usage against
  budgets, mirroring the same `X-*-Tokens` accounting the Service Engine enforces (alice/carol
  → 429 at the group budget, dave with no policy → 403).
- **Auth.** List and edit `AITokenRateLimitPolicy` objects cluster-wide and the
  `AIModelRoutePolicy` tier/entitlement policies, with a green/red dot reflecting the target
  route's `Accepted` condition. Identities are pulled live from the OIDC issuer, not a static
  list.
- **Models.** List and create `InferencePool`s.
- **Gateways.** List Gateways (with attached-route counts, Programmed status, and an MCP flag),
  create LLM or MCP gateways, and flip a **per-gateway DLP toggle** — checked = enforcing
  (`AIGuardrailPolicy` `action: Block`), unchecked = shadow/detect-only (`Log`), an
  instantly-reversible patch. Sub-tabs surface **Inference** (live Avi pool-group member ratios
  per model pod, the weights AKO steers from vLLM metrics) and **MCP**.
- **MCP Registry & MCP Routes.** Browse the **approved MCP-server allow-list** (with
  reachability probes), and create/edit/delete the HTTPRoutes that load-balance MCP traffic
  through an MCP gateway — in-cluster servers point at their Service directly, public servers
  get an auto-created ExternalName Service, with a listener-hostname guardrail that keeps routes
  from being rejected.

The console has been deployed in-cluster and verified against real OIDC traffic; it is accessed
in the demo environment via `kubectl port-forward` (a self-healing port-forward loop).

---

## Getting started

- [AI Gateway install guide](ai-gateway-install.md) — enable the feature and apply the auth
  and token-counting policies.
- [Inference install guide](inference-install.md) — set up `InferencePool`-based inference
  load balancing.

## Related docs

- [AI Gateway](ai-gateway.md) — authentication and token counting reference
- [Model-Based Routing](model-routing.md) — quality/cost tier routing
- [Native Inference Extension](inference-extension.md) — metric-weighted load balancing
- [MCP Gateway](ai-gateway-mcp.md) — agent↔tool traffic governance
- [A2A Gateway](ai-gateway-a2a.md) — agent↔agent traffic governance
- [Guardrails & DLP](ai-gateway-guardrails.md) · [Semantic Guardrails](ai-gateway-guardrails-semantic.md) — content inspection
- [Backend mTLS](ai-gateway-backend-mtls.md) — SE↔backend mutual TLS with SPIFFE/SPIRE
- [Multi-Site Delivery](ai-gateway-multisite.md) — cross-cluster model routing
- [Release Notes](ai-gateway-release-notes.md) — what shipped, by date
