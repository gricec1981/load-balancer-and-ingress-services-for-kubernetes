# AKO AI Gateway — Overview

The AKO AI Gateway turns the Avi Load Balancer into the entry point for AI traffic on
Kubernetes. It authenticates callers, meters and governs usage, and routes requests to the
right model — all using the Avi Service Engines an organisation already runs, with no extra
proxies or sidecars. Every capability is expressed as a Kubernetes CRD that attaches to a
Gateway API route and is reconciled by AKO into native Avi features.

It governs both halves of an agent's work: **inference** calls to LLMs and **tool** calls to
MCP servers, under one verified identity and one set of policies.

---

## Goals

- **Make the Avi data plane the AI Gateway.** Authenticate, govern, meter, and route AI
  traffic on the load balancer customers already operate — no new data plane to adopt.
- **Govern the whole agent loop with one identity.** LLM inference and MCP tool calls share
  the same authentication, the same policies, and the same usage accounting.
- **Control cost and quality.** Route each request to the appropriate model tier and
  hardware, and keep expensive resources for the traffic entitled to them.
- **Give operators visibility and control.** Per-consumer and per-group token budgets, usage
  counters, and group-based entitlements.
- **Stay Kubernetes-native.** Configure everything as CRDs attached to Gateway API routes,
  reconciled into Avi Service Engine capabilities (OAuth/OIDC, DataScripts, pools,
  persistence).

---

## What's supported today

| Capability | What it provides | Status |
|---|---|---|
| [Authentication](#authentication) | OAuth/OIDC validation and verified identity/claims at the gateway | Available |
| [Inference](#inference) | Metric-weighted load balancing across model-server pods | Available |
| [Token Counting](#token-counting) | Per-consumer / per-group token budgets, counters, rate limits | Available |
| [Model Routing](#model-routing) | Model-aware routing to quality/cost tiers, with entitlements | Available |
| [MCP](#mcp) | The same governance for agent↔tool (Model Context Protocol) traffic | In design |

---

## Authentication

The gateway authenticates API consumers with **OAuth/OIDC** at the Service Engine. A caller's
bearer token is validated against the configured identity provider, and the verified identity
and claims — such as the consumer's `sub` and their `group` or `role` — are made available to
every downstream policy and can be forwarded to backends as request headers. One identity
provider serves the whole gateway, so a single login governs everything behind it.

This verified identity is the foundation the rest of the gateway builds on: token budgets are
keyed on it, tier entitlements are decided from it, and MCP tool authorization reads the same
claims. Authentication is configured with an `AIGatewayAuthPolicy` attached to a route.

→ [OAuth/OIDC Authentication — `AIGatewayAuthPolicy`](ai-gateway.md#oauthoidc-authentication--aigatewayauthpolicy)

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

Usage is also exposed through a read-only counters endpoint that the dashboard polls, giving
operators a live view of who is consuming how many tokens. Token counting is configured with
an `AITokenRateLimitPolicy` and keys its accounting on the identity established by
authentication.

→ [Token Rate Limiting — `AITokenRateLimitPolicy`](ai-gateway.md#token-rate-limiting--aitokenratelimitpolicy)

## MCP

MCP support extends the same governance to **agent↔tool** traffic. Agents call tools over the
Model Context Protocol, and the gateway fronts those tool servers through a dedicated **MCP
Gateway**: it keeps stateful agent sessions pinned to the right backend, authenticates callers
against the **same identity provider** as the LLM gateway, and authorizes individual tool
calls by the caller's role — so different job roles get access to different tools.

Tool servers are onboarded from an **approved MCP registry**, a curated catalog of vetted
servers that the gateway treats as an allow-list. This brings the whole agent loop — inference
and tools — under one identity and one governance model. MCP is configured with an
`AIMCPRoutePolicy`.

→ [MCP Gateway & MCP-Specific Routes](ai-gateway-mcp.md)

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
- [MCP Gateway](ai-gateway-mcp.md) — agent/tool traffic governance
