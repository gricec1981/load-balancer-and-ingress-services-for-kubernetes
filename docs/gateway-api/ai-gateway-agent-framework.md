<!--
  Designed 2026-08-18 on feature/ai-mixed-estate; ADK body BUILT and verified 2026-08-18.
  Companion to ai-gateway-agent-factory.md, ai-gateway-a2a.md and ai-gateway-mcp.md.
  The runtime code lives in the external ako-inference-demo repo (agent-runtime-adk);
  the provisioner change is in ai-gateway-ui/agents.go. First ADK agent on cluster:
  incident-analyst (mcp ns, parallel-synthesize over k8s-logs + rag + web-search).
  LangGraph (§8 phase 5) remains unbuilt.
-->

# AKO AI Gateway — Framework Agent Runtimes (ADK / LangGraph in the Factory)

> **Status: ADK body built (2026-08-18); LangGraph still design.** This document
> specifies how a real agent framework — Google's **Agent Development Kit (ADK)** or
> **LangGraph** — becomes a *second body* inside the existing Agent Factory, without
> changing the factory, the spec, the governance gate, or a single Avi object. One new
> field (`runtime`) selects which image the provisioner deploys. Everything the SE
> enforces stays byte-identical.
>
> The premise: the Factory is already the hard part. What it lacks is not orchestration
> plumbing — it is a *brain* with state, planning, and delegation.
>
> **What building it changed about the design.** Three things, all recorded in place
> below: `to_a2a()` is *not* usable (§3.5) because it would swap the estate's wire
> dialect; the spec needed a second optional field, `workflow`, or the ADK body would
> have been the Go body with a heavier image (§2); and the binding constraint turned out
> to be neither auth nor MCP but the lab GPU's **6144-token context window** (§3.6).

---

## 1. What exists, and what a framework actually adds

The Factory (see [ai-gateway-agent-factory.md](ai-gateway-agent-factory.md)) turns one
`AgentSpec` into a ConfigMap + Deployment + Service + ReferenceGrant + HTTPRoute +
`AIA2ARoutePolicy` + registry entry. Every agent runs the **same** generic image,
`agent-runtime`, whose `agent.json` supplies prompt/model/identity/MCP servers.

That runtime's brain is `loop.go` — 207 lines of flat ReAct:

```
mint JWT → POST /v1/chat/completions?jwt=… → tool_calls? → call MCP → append → repeat (max 6)
```

It works, and it is honest about the SE (401/403/429 are named by policy layer). Its
limits are structural, not cosmetic:

| Limit today | What a framework gives |
|---|---|
| Every `tasks/send` starts cold — no memory between A2A tasks | Session + state services, checkpointed across turns |
| One flat loop; no plan, no critic, no retry-on-bad-args | Planner / reflection / sequential + parallel sub-agent graphs |
| Sub-agent delegation is hand-rolled HTTP in agent-hub | Delegation as a first-class node (and in ADK, as native A2A) |
| Tool schemas hand-marshalled from `tools/list` | Toolsets, typed args, structured output, automatic retry |
| No trace of *why* a step happened | Framework callbacks/events → real per-step observability |

**What must not change**, because it is the demo:

1. Every model turn leaves through `llm.ai.avi.com` with `?jwt=` (tiers, WAF/ICAP
   guardrails, per-group budgets, SE token metering all apply).
2. Every tool call crosses the MCP gateway (`.27`) to a registry-approved server, with
   `?jwt=` and the agent's own `sub`.
3. The pod speaks the A2A wire contract inbound: `/.well-known/agent.json`,
   `tasks/send` / `tasks/get` / `tasks/cancel`.
4. Failures name the enforcing layer, never a generic stack trace.

---

## 2. The only contract change: `runtime`

`AgentSpec` (ai-gateway-ui/agents.go) and `agentConfig` (agent-runtime/main.go) each gain
one optional field:

```go
Runtime string `json:"runtime,omitempty"` // "go" (default) | "adk" | "langgraph"
```

**Built with one addition.** `runtime` alone buys a heavier image running the same flat
loop, so the spec also gained an optional `workflow` block — the only thing that makes a
framework body worth deploying:

```go
Workflow *AgentWorkflow `json:"workflow,omitempty"` // framework bodies only; "go" ignores it

type AgentWorkflow struct {
    Type        string            `json:"type,omitempty"` // single | parallel-synthesize
    Specialists []AgentSpecialist `json:"specialists,omitempty"`
    Synthesizer string            `json:"synthesizer,omitempty"`
}
```

It stays inside the governance model because `normaliseRuntime()` enforces one rule: **a
specialist may only use MCP servers the agent itself was granted.** Without that, the
workflow block would be a way to smuggle tools past the registry allow-list the factory
had just enforced. Unknown servers are dropped with a warning, and the runtime drops them
again on load — belt and braces, because the ConfigMap can be edited after provisioning.

The provisioner maps it to an image, and nothing else in the provisioning path moves:

```go
func (s *Server) agentImageFor(runtime string) string {
    switch runtime {
    case "adk":       return getenv("AGENT_IMAGE_ADK", defaultRegistry+"/agent-runtime-adk:latest")
    case "langgraph": return getenv("AGENT_IMAGE_LANGGRAPH", defaultRegistry+"/agent-runtime-lg:latest")
    default:          return s.agentImage() // today's Go body
    }
}
```

Consequences worth stating plainly:

- The **ConfigMap is unchanged**. `agent.json` is already a framework-neutral description
  of an agent (name, systemPrompt, model, identity, maxSteps, skills, mcpServers). A
  Python runtime reads the same file.
- The **HTTPRoute, AIA2ARoutePolicy, registry entry, netpol, and DNS are unchanged.**
- The **NL draft path is unchanged** — `create_agent` gains one enum property so chat can
  say *"…and build it on ADK"*, but a spec with no `runtime` still yields today's Go agent.
- The 12 live agents are untouched. Framework runtimes are strictly opt-in, per agent.

This is the whole point: **one spec, several bodies, identical governance.** The same agent
card served by a Go pod and an ADK pod behind the same `AIA2ARoutePolicy` makes the
platform argument better than either runtime alone.

---

## 3. The governed adapter layer (the actual work)

Both frameworks assume a normal cloud LLM endpoint. Our front door is not normal: Avi
consumes the `Authorization` header, so auth rides as a **query parameter**, and per-skill
tokens from `jwt-issuer POST /exchange` live **60 seconds**. Five adapters bridge that gap.

### 3.1 LLM transport — a custom model class, not LiteLLM

The clean seam in ADK is `BaseLlm`. Subclass it and implement
`generate_content_async(llm_request, stream)` as an async generator yielding
`LlmResponse` — roughly 80 lines of `httpx` that port `loop.go`'s request builder. This is
strictly better than routing through `LiteLlm`, because it keeps the HTTP layer in our
hands rather than two abstractions down:

```python
class GovernedFrontDoor(BaseLlm):
    """Every turn: fresh 60s token, jwtQuery, VIP-pinned dial, thinking off."""
    async def generate_content_async(self, llm_request, stream=False):
        tok = await self.mint()                      # per REQUEST, not per session
        url = f"{LLM_URL}/v1/chat/completions?jwt={quote(tok)}"
        body = to_openai(llm_request) | {
            "chat_template_kwargs": {"enable_thinking": False},   # §3.3
        }
        r = await self.client.post(url, json=body)   # client pins LLM_VIP, verify=False
        if r.status_code != 200:
            raise SEPolicyError(r.status_code, r.text)            # §3.4
        yield to_llm_response(r.json())
```

LangGraph's equivalent is `ChatOpenAI(base_url=…)` plus an **httpx event hook** that
rewrites `request.url` with a freshly minted `jwt` on every send. It must be a hook, not a
constructor argument: a graph run spans many LLM calls, and a token baked in at construction
time will 401 partway through a long run. Same trap, same fix, in both frameworks.

> Do **not** enable framework streaming. Streaming bypasses SE token metering (see
> [ai-gateway.md](ai-gateway.md)) — the agent would silently stop being counted.
> Non-streaming only, in both runtimes.

### 3.2 MCP transport — query-param auth is native

Query-string auth is a normal MCP pattern, not a hack: ADK's own documented `McpToolset`
example carries credentials in the URL. So our gateway URL drops straight in:

```python
McpToolset(connection_params=StreamableHTTPConnectionParams(
    url=f"{server.url}?jwt={quote(tok)}",
))
```

The one real constraint: **an MCP session outlives a 60-second token.** Mirror what
`mcp.go` already does with `setToken` — build the toolsets at the *start of each A2A task*
with a fresh token and discard them when the task ends. Task-scoped toolsets, never
process-scoped. If a task can exceed 60s (they can — the GPU floor is ~15s/turn), either
raise the exchange TTL for framework agents or re-create the toolset between graph steps.

DNS resolves `*.ai.avi.com` natively on openshift06 now, so `MCP_VIP` / `LLM_VIP` pinning
is a fallback only — keep the env knobs, expect them empty.

### 3.3 Qwen3 thinking

`chat_template_kwargs: {"enable_thinking": false}` on every request, via `extra_body` in
LangGraph or directly in the custom `BaseLlm` body. Omitting it costs roughly 10x tokens
per graph node — which now also means 10x against the group budget, so a framework agent
that forgets this will hit 429 mid-run and look like a platform fault.

### 3.4 Error-layer fidelity

Frameworks collapse HTTP into generic exceptions, and an agent that reports
`ClientResponseError` instead of *"403 — blocked by an SE guardrail"* destroys the
demo's whole point. Define one exception and unwrap it at the A2A boundary, reusing the
exact strings from `loop.go`:

| Status | Surfaced as |
|---|---|
| 401 | token rejected by the SE (auth policy) |
| 403 | blocked by an SE policy (guardrail / entitlement) |
| 429 | token budget exhausted mid-agent-loop |

Both frameworks wrap tool/model errors in retry logic — configure retries **off** for
these three codes. Retrying a 403 just burns budget against a policy that will not relent.

### 3.5 Inbound A2A

This is where the two frameworks genuinely differ.

- **ADK**: `to_a2a(agent)` exposes the agent over the A2A protocol and auto-generates an
  agent card, served by uvicorn. That card must then be *reconciled* with ours — the
  Factory publishes `url`, `skills[]` from `agent.json`, and the `securitySchemes.jwt`
  block that `AIA2ARoutePolicy` matches on. Expect a thin override rather than a rewrite.

  > **Correction (built 2026-08-18): `to_a2a()` is not usable here, and the reason
  > matters more than the workaround.** It serves the current a2a-sdk dialect
  > (`message/send`), while this estate — agent-hub, the console's live-run popup, and
  > **every deployed `AIA2ARoutePolicy` allow-list (`tasks/*`, `log.collection`, …)** —
  > speaks the earlier `tasks/send` JSON-RPC. Adopting the framework's dialect would have
  > silently broken twelve callers and every policy in one deploy, which is precisely the
  > "the framework decides the contract" failure the platform argument exists to prevent.
  > So ADK deletes *less* of §3.5 than the design assumed: `a2a_server.py` is ~130 lines
  > of Starlette porting `a2a.go` (card, `tasks/send|get|cancel`, task ring,
  > `X-Handled-By`) — the same shim LangGraph was costed for. **The body changes; the
  > wire does not.** Budget the shim for every framework body, not just LangGraph.
- **LangGraph**: no A2A server exists. ~120 lines of FastAPI porting `a2a.go` (card,
  `tasks/send|get|cancel`, in-memory task ring, `X-Handled-By` pod stamping).

`X-Handled-By` must survive in both — it is the live routing proof the console reads.

### 3.6 The constraint the design missed: context, not auth

Auth and MCP both landed first try. What actually broke the first live task was arithmetic:

```
400 — This model's maximum context length is 6144 tokens. However, you requested 1024
output tokens and your prompt contains at least 5121 input tokens
```

The lab GPU serves **Qwen3-14B-AWQ at `max_model_len 6144`** (VRAM, not choice), and a
fan-out multiplies pressure on that window in two places the flat loop never felt:

1. **Tool results.** One `get_pod_logs` over 400 lines is larger than the entire window.
   The Go loop survives because its prompts are short and it usually stops after one
   round; a specialist that reads logs *and then* has to report on them does not.
   Fixed by truncating at the tool boundary — `TOOL_RESULT_MAX_CHARS` (5000), and the
   truncation is *visible* to the model so it knows the evidence was cut.
2. **The synthesizer seeing everything twice.** By default that node receives the whole
   session history — every specialist's tool call and raw tool output — *plus* the
   findings already interpolated into its instruction from session state. Setting
   `include_contents="none"` on the synthesizer is load-bearing, not tidiness: it took
   that node from "cannot fit" to 3.4 KB.

Measured on the live agent afterwards, per node: ~2.9 KB on a specialist's first turn,
~6.5 KB by its last tool round, 3.4 KB at the synthesizer. Every turn logs its own shape
(`front door: model=… msgs=… tools=… payload=…B`) precisely because this is the first
thing to check when a graph node dies.

**Generalisation for the estate:** context is the scarcest resource here, scarcer than
tokens-per-budget or GPU time, and it is the one resource a *wider* graph consumes
faster. Any future body — LangGraph included — needs a tool-output cap and a
context-free merge node, or it will fail on the third hop of a demo.

---

## 4. Recommendation: ADK

| | ADK | LangGraph |
|---|---|---|
| Inbound A2A | **Native** (`to_a2a`, auto agent card) | Hand-rolled FastAPI shim |
| MCP | `McpToolset` + `StreamableHTTPConnectionParams`, query auth documented | `langchain-mcp-adapters`, comparable |
| Our LLM front door | Custom `BaseLlm` — full transport control | `ChatOpenAI` + httpx hook — also fine |
| Graph expressiveness | Sequential / parallel / loop agents, sub-agents | Richer arbitrary graphs, better checkpointers |
| Ecosystem breadth | Narrower | Much wider |
| Fit to *this* estate | **A2A is our third gateway surface; ADK speaks it natively** | A2A is a bolt-on |

**Build ADK first.** The obvious objection — that ADK drags Gemini and LiteLLM in, hiding
the very HTTP hops this estate exists to prove — does not survive contact with `BaseLlm`:
a custom model class gives exactly the transport control LangGraph would be credited with,
and ADK additionally deletes adapter §3.5. LangGraph remains a worthwhile *second* body
precisely because the `runtime` field makes it cheap, and having two proves the seam is
real rather than a rename.

---

## 5. Object recipe (delta only)

| Object | Change |
|---|---|
| ConfigMap `agent-<name>` | none — `agent.json` unchanged (optionally records `runtime` for display) |
| Deployment | `image:` from `agentImageFor(spec.Runtime)`; env unchanged (`LLM_URL`, `ISSUER_TOKEN_URL`, `POD_NAME`, `AGENT_PUBLIC_URL`, `MCP_VIP`/`LLM_VIP`). **Probes and resources are not unchanged** — see below |
| Service / ReferenceGrant / HTTPRoute | none |
| `AIA2ARoutePolicy` | none |
| Registry entry | gains `runtime` so the console can badge it |
| Netpol | none — `netpol-factory-agents.yaml` selects on the factory label, which the new pods still carry |

New build artifact: `agent-runtime-adk` via the same binary-upload BuildConfig pattern
(`oc start-build --from-dir`), ubi9-minimal + Python 3.12 base instead of a static Go
binary. Note the known repo-vs-jump-box Dockerfile drift (distroless vs ubi9-minimal) —
this one must be ubi9-minimal. (ubi9's default python is 3.9; `microdnf install
python3.12` is required, and the build pod reaches PyPI directly — verified.)

**Two deltas the design said were "none", found while building:**

- **Pod sizing is body-specific.** The Go body idles in a few MB; a Python framework body
  carries an interpreter plus the ADK import tree and is OOMKilled by the inherited
  256Mi limit *during startup*, which presents as a crash-looping agent rather than as a
  limit. `agentResources(runtime)` now returns 384Mi/1Gi and a 10s probe delay for `adk`,
  the original 64Mi/256Mi and 2s for `go`.
- **Pre-existing RBAC gap, surfaced by this work.** The provisioner has created a
  per-agent ServiceAccount since the per-skill-auth phase, but the `ai-gateway-ui`
  ClusterRole was never granted `serviceaccounts` (nor `services: patch`), so that step
  has been failing silently on every agent since. The Deployment then references an SA
  that does not exist and the ReplicaSet reports `FailedCreate: error looking up service
  account`. Needed:

  ```yaml
  - apiGroups: [""]
    resources: ["serviceaccounts"]
    verbs: ["get","list","create","update","patch","delete"]
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get","list","create","update","patch","delete"]   # patch was missing
  ```

  `serviceaccounts` was granted on 2026-08-19 and a throwaway agent then provisioned
  **9/9 green with no manual step**, start to ready. `services: patch` is still missing,
  which only bites on *re-provisioning* an existing agent (the upsert POSTs, gets 409,
  and the fallback PATCH 403s) — a first-time create is unaffected.

  The teardown path had the mirror-image bug: it never deleted the ServiceAccount, so
  every removed agent left an orphaned workload identity behind that the issuer would
  still happily TokenReview. `DELETE /api/agents` now removes it, after the Deployment.

---

## 6. Make-or-break spike

Before any of §5, prove the two things that can actually fail, in one afternoon:

1. A bare ADK `LlmAgent` with `GovernedFrontDoor` completes a tool-calling turn against
   `https://llm.ai.avi.com` with a per-request re-minted `?jwt=`, and the SE
   `/v1/admin/counters` shows the tokens against `sub=<agent>`.
2. An `McpToolset` pointed at `https://mcp-web.ai.avi.com/mcp?jwt=…` lists and calls a
   tool, and a **tokenless** attempt returns 401 from the SE.

If both hold, the rest is the assembly already proven twelve times.

---

## 7. Risks

| Risk | Mitigation |
|---|---|
| 60s token vs. long graph run | Task-scoped toolsets; re-mint per LLM request; consider a longer TTL for `runtime != go` |
| Framework retries burn budget against 403/429 | Retries disabled for SE policy codes (§3.4) |
| Streaming silently unmetered | Streaming disabled in both runtimes; assert it in the runtime's startup log |
| Python image size / cold start vs. the Go body | Accept; framework agents are opt-in, and the GPU floor dominates latency anyway |
| ADK's auto agent card diverges from the Factory card | Reconcile explicitly in §3.5; the card is what `AIA2ARoutePolicy` and the registry key on |
| Two runtimes drift in error strings | Extract the three SE-layer strings into one shared table, tested in both |

---

## 8. Phasing

1. ~~**Spike** (§6) — governed `BaseLlm` + `McpToolset`, no factory changes.~~ **Done**,
   folded into phase 2: `selftest.py` proves the whole body offline against a fake front
   door and fake MCP server (jwt on every turn, `enable_thinking:false`, tool-call id
   correlation, fan-out, and a 403 surfacing as the named policy layer) — a better spike
   than the live one, because it runs in seconds with no GPU.
2. ~~**Runtime** — `agent-runtime-adk`.~~ **Built.** `agent.json` loader, governed LLM,
   task-scoped MCP tools, hand-rolled `tasks/*` surface (§3.5), `X-Handled-By`, healthz,
   BuildConfig.
3. ~~**Factory** — `Runtime` field, `agentImageFor`, `create_agent` enum, registry
   badge.~~ **Built**, plus the `workflow` block (§2) and `agentResources` (§5). The
   console dropdown is still to do — the API and the NL path both take `runtime` today.
4. ~~**Prove.**~~ **Done 2026-08-18** — see §9.
5. **Second body** — LangGraph, to demonstrate the seam is a seam. Still open, and now
   cheaper to justify: the `tasks/*` shim it was costed for turned out to be needed for
   ADK too, so LangGraph's remaining delta is just the model adapter.

---

## 9. Built: `incident-analyst`, the first ADK agent

The proof agent is deliberately something the Go body *cannot* do: one A2A task fans out
to three specialists that run **concurrently**, each holding a different slice of the
approved MCP registry, then a synthesizer merges them.

```
tasks/send ──▶ incident-analyst (ADK)
                 ├── log-scout   → k8s-logs MCP   (what the cluster logged)
                 ├── code-scout  → rag MCP        (what the source says)
                 └── web-scout   → web-search MCP (whether it is known)
                          └────▶ synthesizer → root cause / evidence / next action
```

Every branch mints the same identity, crosses the same SE, and is metered against the
same budget — the graph gets wider, the governance does not get weaker.

### Provisioning it via the factory

Two paths, same `AgentSpec`, same provisioner:

**Console / NL** — describe it in the factory chat ("…triage AKO incidents by checking
the logs, the source and the web in parallel, and build it on ADK"). The extractor now
knows about `runtime` and `workflow` and proposes a spec; the operator approves.

**API** — the deterministic path, and what was actually used here:

```bash
curl -sX POST http://<ui>/api/agents -H 'Content-Type: application/json' -d '{
  "name": "incident-analyst",
  "description": "Triages an AKO/Avi incident by gathering live log, source-code and public evidence in parallel.",
  "systemPrompt": "You are incident-analyst … name the exact pod, log line, file or URL you relied on.",
  "model": "qwen3-14b", "group": "agents", "maxSteps": 4,
  "runtime": "adk",
  "mcpServers": ["k8s-logs", "rag", "web-search"],
  "skills": [{"id": "incident.triage", "name": "incident-triage", "description": "…"}],
  "callers": [{"agent": "agent-hub", "allow": ["tasks/*"]}],
  "workflow": {
    "type": "parallel-synthesize",
    "specialists": [
      {"name": "log-scout",  "instruction": "…list_pods/get_pod_logs on avi-system…", "mcpServers": ["k8s-logs"]},
      {"name": "code-scout", "instruction": "…code_search/read_file for the error…",  "mcpServers": ["rag"]},
      {"name": "web-scout",  "instruction": "…web_search for known issues…",          "mcpServers": ["web-search"]}
    ],
    "synthesizer": "…ROOT CAUSE / EVIDENCE / NEXT ACTION, attributing every claim…"
  }
}'
```

Then the ordinary factory flow: flip `approved` in the `agent-registry` ConfigMap, and
(until the RBAC gap in §5 is closed) `oc create sa incident-analyst -n mcp` +
`oc rollout restart deploy/incident-analyst -n mcp`.

### Verified on cluster, 2026-08-18

| Check | Result |
|---|---|
| Card without a token | **401** from the SE (fail-closed) |
| Card with `?jwt=` | 200, `X-Handled-By: incident-analyst-…`, `runtime: {body: adk, graph: …}` |
| `tasks/send` through the A2A gateway | **completed in 27.5s** — 3 branches + synthesis |
| Evidence quality | log-scout returned a real AKO warning (`lib/avi_api.go:93`, `/api/cluster/runtime`) with the raw line |
| Honesty | code-scout and web-scout both reported finding nothing, rather than inventing a cause |
| Artifacts | `answer` **plus** `log_scout_result`, `code_scout_result`, `web_scout_result` |
| SE metering | `/v1/admin/counters` → `incident-analyst: 14969` tokens — every graph node counted |

### What the fan-out cannot do (and the next workflow type)

The branches are **blind to each other by construction**. Here log-scout found the error
came from `lib/avi_api.go`, but code-scout — running at the same moment — was still
searching for the *original* phrasing of the request and found nothing. Parallelism buys
latency and isolation; it cannot buy a handoff.

The obvious next `workflow.type` is a sequential refine (`log → code → web → synthesize`),
where each stage sees the previous stage's `output_key`. It is a ~20-line addition to
`graph.py` and needs no factory, policy or Avi change at all — which is itself the point
the `runtime` seam was built to make.
