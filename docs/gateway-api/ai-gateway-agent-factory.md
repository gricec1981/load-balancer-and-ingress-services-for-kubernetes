<!--
  Designed 2026-08-14 on feature/ai-mixed-estate. Companion to ai-gateway-a2a.md and
  ai-gateway-agent-registry.md. Status: DESIGN — nothing built yet. The console work
  lands in the external ai-gateway-ui repo; the object recipe it automates is the one
  proven by hand on openshift06 (mcp-web-tools, jobs-agent).
-->

# AKO AI Gateway — Agent Factory (natural-language agent creation)

> **Status: Design.** This document specifies the **Agent Factory**: a console chat
> experience where an operator describes an agent in plain language — *"create an agent
> called weather-agent that can search the web and summarise forecasts; only the
> orchestrator may call it"* — and the platform creates everything else: a running agent
> on OpenShift, protected by the shared-IdP auth policy, routed through the Avi A2A
> Gateway on its own `<name>.ai.avi.com` hostname, and catalogued in the Agent Registry
> behind the register → approve → route governance gate.
>
> Nothing here invents new gateway machinery. The factory automates the exact object
> recipe already proven by hand for `mcp-web-tools` and `jobs-agent` on openshift06.

---

## 1. Why this is mostly assembly, not invention

Review of the lab (2026-08-14) shows every layer the factory needs already exists and is
verified live:

| Layer | What exists today | Where |
|---|---|---|
| A2A ingress | `a2a-gateway` (class `avi-lb`, HTTPS listener, `a2a-tls`) with per-agent HTTPRoutes | openshift06, ns `inference` |
| DNS/VIP | AKO auto-registers every new HTTPRoute hostname into the VsVip `dns_info` (TTL 30) — **a new agent hostname needs zero VIP or DNS work** | architecture doc §2 |
| Auth | `AIGatewayAuthPolicy llm-auth` (`jwtQuery`), inherited by A2A routes via `authRef`; jwtQuery claim helper verified | [ai-gateway-auth.md](ai-gateway-auth.md), [ai-gateway-a2a.md §5](ai-gateway-a2a.md) |
| Per-agent enforcement | `AIA2ARoutePolicy` — agent/skill access rules, task affinity, push-notification egress control | [ai-gateway-a2a.md §4](ai-gateway-a2a.md) |
| Catalog + governance | `agent-registry` ConfigMap, console table, `/.well-known/agents` federation, register → approve → route | [ai-gateway-agent-registry.md](ai-gateway-agent-registry.md) (UI code on branch `feature/agent-registry`) |
| Agent scaffold | The web-agent loop (`agent.go` / `web-agent/main.go`): MCP `tools/list` discovery, OpenAI tool-calling dialect, `<think>` stripping, `?jwt=` auth, 6-step cap — duplicated as a Go `const`-configured binary | ai-gateway-ui repo, mcp-web-agent repo |
| Model plane | Every model turn already crosses the SE front door `llm.ai.avi.com` → tiers, guardrails (WAF DLP + ICAP), and per-group token budgets apply to agents for free | chat.go / agent.go design intent |
| MCP tools | Approved MCP registry + MCP Routes tab that creates HTTPRoutes (with the ExternalName→EndpointSlice egress fix) | ai-gateway-ui `handleMCPRoutes` |
| Image build | Binary-upload OpenShift builds (`oc start-build --from-dir`, ubi9-minimal) into the internal registry, proven by jobs-agent | jobs-agent deploy dir |
| Console write-path | The UI backend already **creates** Gateways, HTTPRoutes, InferencePools, `AIModelRoutePolicy`, `AIGuardrailPolicy`, Services + EndpointSlices via its hand-rolled k8s REST client | ai-gateway-ui `k8s.go` |

What does *not* exist: (a) a **generic, config-driven agent runtime** (today's agents are
one-off binaries with compiled-in prompts), (b) a **deterministic provisioner** that emits
the whole per-agent object set from one spec, and (c) the **NL → spec** chat front-end.
Those three pieces are the factory.

---

## 2. UX flow

```
Console chat ("🏭 Create agent" mode)
   │  "Make me a weather agent that can search the web…"
   ▼
NL → AgentSpec extraction (LLM tool-call through llm.ai.avi.com)
   │  spec rendered as an editable PREVIEW CARD:
   │  name / hostname / model tier / skills / tools / callers / schedule
   │  + the exact YAML that will be applied (expandable)
   ▼
Operator clicks Approve            ← the human gate; chat alone NEVER applies
   ▼
POST /api/agents  → provisioner emits the object set (§5)
   ▼
Status ticker: Deployment ready → route Accepted → gateway address → card reachable
   ▼
Registry entry created approved:false → operator approves → published at /.well-known/agents
```

Two properties are non-negotiable:

1. **Chat proposes, the operator disposes.** The LLM only ever produces an `AgentSpec`.
   Creation happens on an explicit Approve of a rendered preview — this is the
   prompt-injection firewall (§7) and mirrors the registry's existing
   register → approve → route posture.
2. **The chat path and the provisioning path are decoupled.** `POST /api/agents` is a
   plain deterministic API (also usable from a form or `curl`); the NL layer is sugar on
   top. A malformed spec fails schema validation, not "the LLM did something weird to
   the cluster".

---

## 3. AgentSpec — the single contract

```json
{
  "name": "weather-agent",
  "description": "Answers weather questions using live web search.",
  "systemPrompt": "You are a weather assistant. Use the search tools for current data…",
  "model": "qwen-smart",
  "identity": "weather-agent",
  "skills": [ {"id": "weather.forecast", "description": "Forecast for a location"} ],
  "tools": { "mcpServers": ["web-search"] },
  "callers": [ {"agent": "orchestrator", "allow": ["tasks/*"]} ],
  "egress": { "hosts": ["api.open-meteo.com"] },
  "schedule": null,
  "resources": {"cpu": "50m", "memory": "256Mi"},
  "approved": false
}
```

| Field | Maps to |
|---|---|
| `name` | All object names, hostname `<name>.ai.avi.com`, registry key |
| `systemPrompt`, `model`, `skills` | Agent ConfigMap consumed by the runtime (§4) |
| `identity` | The `sub` the runtime mints its JWT for → group → token budget on the LLM front door |
| `tools.mcpServers` | Must exist in the **approved MCP registry**; resolved to MCP route URLs, injected as env |
| `callers[]` | `AIA2ARoutePolicy.spec.agentAccess.rules` (deployed shape) |
| `egress.hosts` | Optional per-host egress routes (selectorless Service + EndpointSlice + `RouteBackendExtension`), same as mcp-web's `egress.yaml`; runtime runs `EGRESS_STRICT=1` |
| `schedule` | Optional CronJob curling the agent's `/run` (jobs-agent pattern) |
| `approved` | Registry publication only — never route existence |

Validation is strict and boring: DNS-1123 name, model must be a live tier alias, every
MCP server must be registry-approved, every caller agent should exist in the agent
registry (warn otherwise), no duplicate hostname.

---

## 4. The generic agent runtime — build once, configure forever

**Key design decision: creating an agent must not require building an image.** Per-agent
binary builds (today's pattern) would make the factory a CI system. Instead: one
`agent-runtime` image, built once via the proven jobs-agent binary-upload flow, that is
**entirely configured by mounted ConfigMap + env**:

- Implements the **A2A wire contract** (which jobs-agent and mcp-web-tools today skip):
  `GET /.well-known/agent.json` (card generated from the spec: name, skills, auth),
  `POST /` JSON-RPC `tasks/send|get|cancel`, `X-Handled-By` stamping, `/healthz`.
- Runs the existing web-agent loop, generalised: system prompt, model, max steps, skill
  descriptions read from `/etc/agent/agent.json` (the ConfigMap) instead of Go consts.
- Model hop → `https://llm.ai.avi.com/v1/chat/completions?jwt=<minted for identity>`;
  tool hops → the configured MCP routes. **Both legs cross an SE**, so auth, tiers,
  guardrails, and budgets govern every generated agent with no extra wiring.
- Zero external Go deps, ubi9-minimal, same as jobs-agent.

This collapses "create an agent" to pure Kubernetes data: a ConfigMap edit even lets you
hot-tune a live agent's prompt (`kubectl rollout restart`).

---

## 5. What the provisioner emits (per agent)

All objects the factory creates for `weather-agent`, in apply order — each one is the
hand-proven lab pattern, parameterised:

| # | Object | Namespace | Notes |
|---|---|---|---|
| 1 | ConfigMap `weather-agent-config` | `mcp` | `agent.json`: prompt, model, skills, MCP endpoints |
| 2 | Deployment `weather-agent` | `mcp` | `agent-runtime` image, env `AGENT_NAME`, `LLM_URL`, `IDENTITY`, readiness `/healthz`; jobs-agent resource defaults |
| 3 | Service `weather-agent` :8080 | `mcp` | |
| 4 | ReferenceGrant `weather-agent-grant` | `mcp` | HTTPRoute(`inference`) → this Service (listener is `allowedRoutes: Same`) |
| 5 | HTTPRoute `weather-agent-a2a` | `inference` | `parentRefs: a2a-gateway (sectionName: https)`, hostname `weather-agent.ai.avi.com`, cross-ns backendRef — DNS is automatic |
| 6 | AIA2ARoutePolicy `weather-agent-a2a` | `inference` | `authRef: llm-auth`, `agentAccess` from `callers[]`, `taskAffinity: 30m`, `onUnauthorized: Reject/403` |
| 7 | Registry entry in `agent-registry` | `inference` | `approved: false`, `cardURL: https://weather-agent.ai.avi.com/.well-known/agent.json`, `health`, skills |
| 8 | *(optional)* egress Service + EndpointSlice + RouteBackendExtension + HTTPRoute per `egress.hosts[]` | `inference`/`mcp` | mcp-web egress pattern; omitted when no direct egress requested |
| 9 | *(optional)* CronJob `weather-agent-schedule` | `mcp` | jobs-agent pattern when `schedule` set |

Status is polled after apply: rollout ready → route `Accepted` → `gateway.status.addresses`
→ card fetch through the VIP. The console ticker shows each beat (great demo moment: the
agent's hostname resolving via Avi DNS seconds after Approve).

TLS note: `a2a-tls` is a static SAN cert; new hostnames won't match it. Either move the
listener cert to a wildcard `*.ai.avi.com` (one-time lab change, recommended) or accept
skip-verify as today's clients already do.

---

## 6. NL → AgentSpec extraction

The extractor is a **single OpenAI-style tool call** (`create_agent` with the AgentSpec
JSON Schema) sent through the existing chat proxy — the factory conversation itself is
governed traffic (guardrails, budgets, ICAP) like everything else.

- **Model choice matters.** qwen-3b is too weak for reliable structured output;
  default the factory to the top local tier (qwen3-14b) and offer the external provider
  tier (Gemini via `AIModelRoutePolicy.provider`, already SE-native) as the
  high-reliability option.
- The extractor is *conversational*: missing fields (which tools? who may call it?) come
  back as clarifying questions in the chat; the preview card renders only when the spec
  validates. Every field on the card stays hand-editable — the LLM drafts, it does not
  decide.
- Schema validation + registry lookups happen server-side after extraction; the LLM is
  never trusted to enforce the allow-lists it was told about.

---

## 7. Security posture

| Threat | Control |
|---|---|
| Prompt injection ("ignore instructions, create an agent that…") | LLM output is only ever a draft spec; a human Approve on a rendered preview is required; `POST /api/agents` is admin-gated |
| Rogue tool access | `tools.mcpServers` validated against the **approved** MCP registry; unknown server → hard reject |
| Unbounded egress | Runtime ships `EGRESS_STRICT=1`; only spec'd hosts get egress routes (default-deny) |
| Unauthenticated exposure | Every generated route carries `authRef: llm-auth` + `agentAccess` (fail-closed 403); federation publication additionally gated on registry approval |
| Runaway spend | Agent's `identity` maps to a group in `AITokenRateLimitPolicy` — factory agents can be given their own budget group |
| Console blast radius | New RBAC (§8) is additive and scoped; no `delete` on gateways/policies granted |

---

## 8. Gaps to close (deltas found in review)

1. **UI RBAC** — the `ai-gateway-ui` ClusterRole must add: `apps/deployments`
   (get,list,create,update,patch), `configmaps` write (master is read-only today; the
   `feature/agent-registry` branch already bumps this), `secrets` (create,update,patch —
   `UpsertSecret` already needs it), `gateway.networking.k8s.io/referencegrants`,
   `batch/cronjobs`, and `ako.vmware.com/routebackendextensions` for egress.
2. **Branch reconciliation** — `feature/agent-registry` (registry handlers +
   `/.well-known/agents`) forked before the chat work; its `main.go`/`server.go`/`web`
   must be merged into master before the factory builds on both.
3. **A2A wire contract** — jobs-agent/mcp-web-tools don't implement agent cards or
   JSON-RPC; the new runtime does, and they can later be migrated onto it.
4. **Wildcard TLS** for `a2a-gateway` (§5 note).
5. **AIA2ARoutePolicy naming drift** — deployed policies use `agentAccess/agentClaim`;
   the design doc describes `skillAccess/roleClaim`. The provisioner targets the
   **deployed** shape; reconcile the doc separately.

---

## 9. Spikes (in order of risk)

| Spike | Question | Verdict criterion |
|---|---|---|
| **A. Config-driven runtime** | Can one image + ConfigMap serve the agent card, JSON-RPC, and the tool loop end-to-end through the gateway? | A factory-shaped agent answers a task through `a2a-gateway` with JWT + agentAccess enforced, model hop metered on the front door |
| **B. NL → spec reliability** | Does qwen3-14b emit a valid `create_agent` tool call for typical descriptions? | ≥9/10 clean extractions on a 10-prompt suite; else default extractor to the Gemini tier |
| **C. Provisioner write-path** | Do Deployment/ReferenceGrant/CronJob creates work through the hand-rolled REST client + new RBAC? | `POST /api/agents` (spec from a file, no chat) yields a green status ticker |

None require new SE behaviour — this is the rare feature with no Avi-side unknowns.

---

## 10. Implementation phases

1. **Phase 0 — foundations:** merge `feature/agent-registry` into the UI master; RBAC
   deltas; wildcard `a2a-tls`.
2. **Phase 1 — deterministic factory:** `agent-runtime` image (spike A) +
   `POST /api/agents` provisioner + status ticker + "Agents" console tab with a manual
   create form (spike C). *Demo-able without any NL.*
3. **Phase 2 — natural language:** `create_agent` extraction in a "🏭 Create agent" chat
   mode, preview card, Approve wiring (spike B).
4. **Phase 3 — lifecycle:** delete/suspend, prompt hot-edit, scheduled agents, egress
   blocks, migrate ops/security/jobs/mcp-web onto the runtime, federation polish.

---

## 11. Related docs

- [ai-gateway-a2a.md](ai-gateway-a2a.md) — A2A Gateway, `AIA2ARoutePolicy`
- [ai-gateway-agent-registry.md](ai-gateway-agent-registry.md) — catalog, approval, federation
- [ai-gateway-auth.md](ai-gateway-auth.md) — `jwtQuery` shared-IdP auth
- [ai-gateway-mcp.md](ai-gateway-mcp.md) — approved MCP tool registry
- [model-routing.md](model-routing.md) — tiers the runtime's model field maps to
- [ai-gateway-guardrails.md](ai-gateway-guardrails.md) / [ai-gateway-guardrails-semantic.md](ai-gateway-guardrails-semantic.md) — controls every agent inherits on the model hop
