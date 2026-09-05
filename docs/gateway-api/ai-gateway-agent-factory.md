<!--
  Designed 2026-08-14 on feature/ai-mixed-estate. Companion to ai-gateway-a2a.md,
  ai-gateway-agent-registry.md and ai-gateway-agent-framework.md.
  Status: BUILT — phases 0-2 shipped 2026-08-15, the ADK second body 2026-08-18.
  The console/provisioner code lives in the external ai-gateway-ui repo (agents.go);
  the runtime images are ako-inference-demo/agent-runtime and agent-runtime-adk.
  UPDATED 2026-09-05 against agents.go: §3 and §5 now describe the SHIPPED contract
  and object set, which differ from the 2026-08-14 sketch. §8 records the one gap
  that is still open.
-->

# AKO AI Gateway — Agent Factory (natural-language agent creation)

> **Status: Built (phases 0-2), 2026-08-15.** An operator describes an agent in plain
> language — *"create an agent called weather-agent that can search the web and summarise
> forecasts; only the orchestrator may call it"* — and the platform creates everything
> else: a running agent on OpenShift, holding its own ServiceAccount identity, protected
> by the shared-IdP auth policy, routed through the Avi A2A Gateway on its own
> `<name>.ai.avi.com` hostname, and catalogued in the Agent Registry behind the
> register → approve → route governance gate.
>
> **First factory-provisioned agent live 2026-08-15:** `log-collector` (ns `mcp`), which
> reads real AKO logs through the `k8s-logs` MCP server and is callable only by
> `avi-controller-agent`. A [second runtime body](ai-gateway-agent-framework.md) (Google
> ADK) landed 2026-08-18 — same ConfigMap, same route, same policy, same registry entry;
> only the image changes.
>
> Nothing here invented new gateway machinery. The factory automates the exact object
> recipe already proven by hand for `mcp-web-tools` and `jobs-agent`.
>
> **One gap is still open** — the provisioner's ClusterRole cannot create the agent's
> ServiceAccount, so a new agent needs one manual `oc create sa`. See §8.

---

## 1. Why this is mostly assembly, not invention

Review of the lab (2026-08-14) shows every layer the factory needs already exists and is
verified live:

| Layer | What exists today | Where |
|---|---|---|
| A2A ingress | `a2a-gateway` (class `avi-lb`, HTTPS listener, `a2a-tls`) with per-agent HTTPRoutes | `vks-ai-01`, ns `inference` |
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

The shipped contract between the NL extractor, the console form and the provisioner
(`ai-gateway-ui/agents.go`). It is deliberately flatter than the 2026-08-14 sketch:

```json
{
  "name": "weather-agent",
  "description": "Answers weather questions using live web search.",
  "systemPrompt": "You are a weather assistant. Use the search tools for current data…",
  "model": "qwen-smart",
  "identity": "weather-agent",
  "group": "agents",
  "maxSteps": 6,
  "skills": [ {"id": "weather.forecast", "name": "Forecast", "description": "Forecast for a location"} ],
  "mcpServers": ["web-search"],
  "callers": [ {"agent": "orchestrator", "allow": ["tasks/send"]} ],
  "runtime": "go",
  "workflow": null
}
```

| Field | Maps to |
|---|---|
| `name` | All object names, hostname `<name>.ai.avi.com`, registry key |
| `description`, `systemPrompt`, `model`, `skills`, `maxSteps` | `agent.json` in the agent ConfigMap, consumed by the runtime (§4) |
| `identity` | Display/`agent.json` only. The token's real `sub` comes from a **TokenReview of the pod's ServiceAccount** at `POST /exchange` — see the note below |
| `group` | Budget group. Also settled at mint time, not asserted by the pod |
| `mcpServers` | Must exist in the **approved MCP registry**; resolved to gateway-fronted MCP route URLs and written into `agent.json`. An unknown name fails the whole provision before any object exists |
| `callers[]` | `AIA2ARoutePolicy.spec.agentAccess.rules` |
| `runtime` | Which **body** runs: `go` (flat ReAct loop) or `adk` (Google ADK). Changes the Deployment image and nothing else. Unknown values fall back to `go` with a warning |
| `workflow` | Multi-agent graph (`type: single \| parallel-synthesize`, `specialists[]`, `synthesizer`). Framework bodies only — the `go` body ignores it, and a specialist's `mcpServers` **must be a subset of the agent's**, so a workflow can never smuggle a tool past the registry allow-list |

> **`identity` is not a claim source.** Early drafts had the runtime mint a token for
> whatever `sub` the spec named. It does not: the runtime presents its pod's projected
> ServiceAccount token to `POST /exchange`, and the issuer derives subject, group, target
> and skill from the TokenReview. A spec cannot assert an identity it was not granted.
> See [Handbook §6.2](ai-gateway-handbook.md#62-principals).

**Not in the shipped spec** (designed 2026-08-14, never built): `egress.hosts[]`,
`schedule`, `resources`, `approved`. Per-agent egress is still applied by hand with the
`mcp-web` pattern; resources are chosen by the provisioner from the runtime
(`agentResources` — the ADK body gets a larger request and a longer probe delay);
`approved` was always a registry field rather than a spec field, and the provisioner
always writes `approved: false`.

Validation is strict and boring: DNS-1123 name, every MCP server must be
registry-approved, callers should exist in the agent registry (warn otherwise).

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

Every object the factory creates for `weather-agent`, in the order
`provisionAgent` applies them. Each is the hand-proven lab pattern, parameterised.
Step 0 runs first on purpose: a bad tool list must fail before anything exists.

| # | Step | Namespace | Notes |
|---|---|---|---|
| 0 | Resolve MCP tools | — | Every `mcpServers[]` name looked up in the approved `mcp-registry`. Not found → the whole provision fails here, having created nothing |
| 1 | ConfigMap `weather-agent` | `mcp` | `agent.json`: name, description, prompt, model, identity, maxSteps, skills, resolved MCP endpoints, `runtime`, and `workflow` when set |
| 2 | ServiceAccount `weather-agent` | `mcp` | The agent's identity. Its projected token is what the runtime exchanges for a front-door JWT — **this is the step that currently fails, see §8** |
| 3 | Deployment `weather-agent` | `mcp` | Image chosen by `runtime` (`agent-runtime` or `agent-runtime-adk`); env `AGENT_CONFIG`, `AGENT_PUBLIC_URL`, `LLM_URL`, `LLM_VIP`, `MCP_VIP`, `ISSUER_EXCHANGE_URL`. **No identity or group is passed** — both come from the TokenReview. Requests/limits and probe delay are picked per runtime |
| 4 | Service `weather-agent` :8080 | `mcp` | |
| 5 | ReferenceGrant `weather-agent-grant` | `mcp` | HTTPRoute(`inference`) → this Service (the listener is `allowedRoutes: Same`) |
| 6 | HTTPRoute `weather-agent-a2a` | `inference` | `parentRefs: a2a-gateway (sectionName: https)`, hostname `weather-agent.ai.avi.com`, cross-ns backendRef — DNS is automatic |
| 7 | AIA2ARoutePolicy `weather-agent-a2a` | `inference` | `authRef: llm-auth`, `agentClaim`, `agentAccess` from `callers[]`, card URL, fail-closed |
| 8 | Registry entry in `agent-registry` | `inference` | `approved: false`, `cardURL`, in-cluster `health` URL, skills, `runtime`, scope `local`, auth `jwt` |

Status is polled after apply: rollout ready → route `Accepted` →
`gateway.status.addresses` → card fetch through the VIP. The console ticker shows each
beat — the agent's hostname resolving via Avi DNS seconds after Approve is the demo
moment.

Deletion reverses it, including removing the registry entry.

**Not emitted** (designed, not built): per-host egress objects and the scheduled-agent
CronJob. Both are still applied by hand from the `mcp-web` / `jobs-agent` patterns.

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

## 8. Gaps — what closed, and the one still open

| # | Gap (2026-08-14 review) | Today |
|---|---|---|
| 1 | UI ClusterRole needs deployments, configmap writes, secrets, referencegrants, cronjobs | ✅ Granted — **except `serviceaccounts`**, see below |
| 2 | `feature/agent-registry` branch must merge into UI master before the factory builds on both | ✅ Merged |
| 3 | A2A wire contract — jobs-agent/mcp-web-tools implement no agent card or JSON-RPC | ✅ The runtime does both; the hand-built agents remain un-migrated |
| 4 | Wildcard TLS for `a2a-gateway` | ⬜ Open — clients still skip-verify |
| 5 | `AIA2ARoutePolicy` naming drift (`agentAccess/agentClaim` deployed vs `skillAccess/roleClaim` in the doc) | ✅ Resolved in favour of the deployed shape; [ai-gateway-a2a.md](ai-gateway-a2a.md) uses it |

### 8.1 ⚠️ Open: the provisioner cannot create its agents' ServiceAccounts

The provisioner emits a ServiceAccount per agent (§5 step 2) because the agent's identity
*is* that ServiceAccount — the runtime exchanges its projected token at `POST /exchange`
and the issuer derives the subject from a TokenReview. But `ai-gateway-ui`'s ClusterRole
(`ai-gateway-ui/k8s/01-rbac.yaml`) grants no `serviceaccounts` resource at all, and its
`services` rule has no `patch` verb.

The failure is quiet in the wrong way. The provision reports the ServiceAccount step as
failed but carries on, so the Deployment lands, the route is `Accepted`, DNS resolves —
and the ReplicaSet sits at `FailedCreate` with **no pod**, because the pod spec references
a ServiceAccount that does not exist. Everything the console shows is green except the
one dot that matters.

Workaround until the ClusterRole is amended:

```bash
oc create sa <agent-name> -n mcp
oc rollout restart deploy/<agent-name> -n mcp
```

Fix: add `serviceaccounts` (get, list, create) and `services: patch` to the ClusterRole.

---

## 9. Spikes — all three passed

| Spike | Question | Outcome |
|---|---|---|
| **A. Config-driven runtime** | Can one image + ConfigMap serve the agent card, JSON-RPC, and the tool loop end-to-end through the gateway? | ✅ Passed. `log-collector` answers tasks through `a2a-gateway` with JWT + `agentAccess` enforced and the model hop metered on the front door. The seam held well enough that a **second** body (ADK) later dropped in behind the same ConfigMap |
| **B. NL → spec reliability** | Does qwen3-14b emit a valid `create_agent` tool call for typical descriptions? | ✅ Passed on qwen3-14b; the Gemini fallback was not needed. Agents must send `enable_thinking: false` |
| **C. Provisioner write-path** | Do the creates work through the hand-rolled REST client + new RBAC? | ⚠️ Partial. Every object provisions green **except the ServiceAccount** (§8.1). CronJob and egress creates were never built, so that half of the spike is untested |

None required new SE behaviour — this was the rare feature with no Avi-side unknowns,
and that held.

---

## 10. Implementation phases

1. **Phase 0 — foundations.** ✅ `feature/agent-registry` merged into the UI master; RBAC
   deltas granted (bar `serviceaccounts`). Wildcard `a2a-tls` still outstanding.
2. **Phase 1 — deterministic factory.** ✅ 2026-08-15. `agent-runtime` image +
   `POST /api/agents` provisioner + status ticker + the "Agents" console tab.
3. **Phase 2 — natural language.** ✅ `POST /api/agents/draft` extracts a spec from a
   description and *proposes only*; the operator approves before anything is applied.
4. **Phase 3 — lifecycle.** ◐ Partial. Delete/teardown is built (registry entry included).
   Not built: suspend, prompt hot-edit, scheduled agents, egress blocks. The hand-built
   agents (ops/security/jobs/mcp-web) were never migrated onto the runtime — two mock
   agents were deleted outright on 2026-08-16.
5. **Phase 4 — framework bodies.** ✅ 2026-08-18, ADK. See
   [ai-gateway-agent-framework.md](ai-gateway-agent-framework.md).

---
---

## 11. Related docs

- [ai-gateway-a2a.md](ai-gateway-a2a.md) — A2A Gateway, `AIA2ARoutePolicy`
- [ai-gateway-agent-registry.md](ai-gateway-agent-registry.md) — catalog, approval, federation
- [ai-gateway-auth.md](ai-gateway-auth.md) — `jwtQuery` shared-IdP auth
- [ai-gateway-mcp.md](ai-gateway-mcp.md) — approved MCP tool registry
- [model-routing.md](model-routing.md) — tiers the runtime's model field maps to
- [ai-gateway-guardrails.md](ai-gateway-guardrails.md) / [ai-gateway-guardrails-semantic.md](ai-gateway-guardrails-semantic.md) — controls every agent inherits on the model hop
