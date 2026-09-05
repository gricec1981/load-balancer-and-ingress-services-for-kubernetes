# AKO AI Gateway — Handbook

**Complete technical reference and console guide.**
Written for someone who has never seen this system before.

| | |
|---|---|
| **System** | AKO AI Gateway on Avi Load Balancer |
| **Data plane** | Avi Service Engine (no in-path proxy or sidecar) |
| **Control plane** | AKO reconciling Kubernetes CRDs into Avi configuration |
| **Console** | `ai-gateway-ui` — Go binary + embedded SPA |
| **Revision** | 5 September 2026 |

**Contents**

1. [What it is](#1-what-it-is)
2. [Feature map](#2-feature-map)
3. [Architecture](#3-architecture)
4. [The console, tab by tab](#4-the-console-tab-by-tab)
5. [How-to guides](#5-how-to-guides)
6. [Security posture](#6-security-posture)
7. [Failure behaviour](#7-failure-behaviour)
8. [Known limits](#8-known-limits)
9. [Where things live](#9-where-things-live)

---

## 1. What it is

The AI Gateway turns the Avi Load Balancer into the governed entry point for AI traffic on
Kubernetes. It authenticates the caller, meters and budgets token spend, routes the request to the
right model tier, inspects content for data loss and prompt injection, and authorizes tool and
agent calls — all on Service Engines the organisation already runs.

Three claims define the design:

- **No new data plane.** Every control runs on the SE already carrying the traffic. Nothing is
  inserted between caller and backend. The classifier and the token service are *called* by the SE
  the way it already calls an OIDC issuer; they are never proxies.
- **One identity across the whole agent loop.** The model call, the tool call and the agent-to-agent
  call are authenticated against the same issuer, and the same verified claims drive budgets, tier
  entitlement and per-capability authorization on all three.
- **Everything is a CRD.** Each capability is a Kubernetes object attached to a Gateway API route.
  AKO reconciles it into native Avi objects — DataScripts, WAF policies, SSO policies, pool groups.
  There is no hand-edited Avi configuration and nothing to drift.

### The three surfaces

| Surface | Protocol | Governed by | Status |
|---|---|---|---|
| agent ↔ model | OpenAI-style HTTP | `AIGatewayAuthPolicy` + `AIModelRoutePolicy` + `AITokenRateLimitPolicy` + `AIGuardrailPolicy` | Built |
| agent ↔ tool | MCP — JSON-RPC over Streamable HTTP | `AIMCPRoutePolicy` + MCP registry | Built |
| agent ↔ agent | A2A — JSON-RPC over HTTPS | `AIA2ARoutePolicy` + agent registry | Built |

---

## 2. Feature map

| Capability | What it does | Configured by | Console tab | Status |
|---|---|---|---|---|
| **Inference load balancing** | Metric-weighted LB across model-server pods — KV-cache use, queue depth, running slots | `InferencePool` | Models · Gateways ▸ Inference | Built |
| **Authentication** | JWT/OIDC validation at the SE; verified claims exposed to every downstream policy | `AIGatewayAuthPolicy` | Governance | Built |
| **Token budgets** | Per-consumer / per-group token ceilings over a window, plus request rate limits | `AITokenRateLimitPolicy` | Governance ▸ Token Rate Limits | Built |
| **Model routing** | Requested `model` → quality/cost tier; group entitlement with downgrade or reject | `AIModelRoutePolicy` | Governance ▸ Model Routing Policies | Built |
| **External provider tiers** | A tier whose backend is a public API (e.g. Gemini) reached SE-native over egress | `AIModelRoutePolicy.tiers[].provider` | Governance ▸ Model Routing Policies | Built |
| **Remote-site tiers** | A tier whose backend is a **peer AI Gateway in another cluster**, reached by FQDN over SE egress with a Host-bearing health monitor. Verified cross-cluster | `AIModelRoutePolicy.tiers[].remote` | Governance ▸ Model Routing Policies | Built |
| **Service tiers & model discovery** | A tier backed by Service endpoints (so a selectorless Service fronts a GPU VM); a labelled model pod publishes its own alias into the model→tier table | `tiers[].backendRef` (`kind: Service`), `spec.discovery` | Governance ▸ Model Routing Policies | Built |
| **Guardrails / DLP** | WAF-native secret, PII, prompt-injection and tool-abuse signatures on request and response bodies | `AIGuardrailPolicy` | Gateways (DLP toggle) | Built |
| **Semantic guardrails** | Embedding classifier over ICAP for novel/paraphrased injection, with an LLM judge for gray-zone scores | `AIGuardrailPolicy.semantic` | Gateways (DLP toggle) | Built |
| **MCP gateway** | Tool traffic under the same identity model; session affinity; per-tool authorization by role | `AIMCPRoutePolicy` | MCP Registry · Gateways ▸ MCP | Built |
| **MCP registry** | The enforced allow-list of tool servers the gateway may broker at all | `mcp-registry` ConfigMap | MCP Registry | Built |
| **A2A gateway** | Agent-to-agent delegation; per-skill authorization; task affinity; target-bound tokens | `AIA2ARoutePolicy` | Agent Registry | Built |
| **Agent registry** | Catalogue of approved agents; federated at `/.well-known/agents` | `agent-registry` ConfigMap | Agent Registry | Built |
| **Agent factory** | Natural-language description → drafted `AgentSpec` → one approval provisions workload, identity, route, policy and registry entry | Console + provisioner | Agents | Built |
| **Agent runtimes** | Interchangeable bodies behind an identical wire contract — compact Go loop, or Python ADK with parallel specialists | `runtime` field on `AgentSpec` | Agents | Built |
| **Token ledger** | One immutable usage record per metered response; true per-user and per-agent consumption with cost. Recording measured exact at 333 rps; the collector's ceiling is ~200 rps sustained | SE ring + console collector | Dashboard ▸ Tokens | Built |
| **RAG over the estate's source** | `code_search` MCP tool answering from the estate's own GitHub, with retrieved chunks scored by the same classifier that guards the front door | `rag-service` + MCP registry | MCP Registry · Dashboard ▸ Agents | Built |
| **Readable Avi object names** | Avi objects named `<cluster>--<surface>-<route>-<hash>` instead of a SHA-1, so an Avi log names its own backend | `useReadableObjectNames` | — | Built |
| **Live counters** | The enforcement counter itself, per identity against budget | `AITokenRateLimitPolicy` | Dashboard ▸ Live Counters | Built |
| **Console** | Operate all of the above without hand-editing YAML | — | all | Built |
| **Backend mTLS** | SE↔backend mutual TLS with SPIFFE/SPIRE short-lived SVIDs | `RouteBackendExtension.BackendTLS` + `seClientCert` | — | Design, spike-gated |
| **Multi-site delivery** | Geo/capacity site steering across a fleet via AMKO + Avi GSLB. The **single-peer** case is built — see *Remote-site tiers* | AMKO | — | Design, spike-gated |
| **A tier that names a set of sites** | Today a `remote` tier names one peer, so peer-down is tier-down | `tiers[].sites[]` | — | Design |
| **A2A push-notification egress control** | Allow-list for the webhook an agent may POST task updates to. Spike-proven, never carried into the CRD | — | — | Design only |
| **Cross-cluster agent authorization** | One A2A policy governing agents in every cluster | — | — | Design; blocked on an ambiguous subject |
| **Provider failover** | Cross-provider active/passive and cost-weighted failover | — | — | Design only |
| **Classifier routing** | Route on an ICAP classifier verdict header | — | — | Disproven on Avi 31.2.1 |

---

## 3. Architecture

### 3.1 Components

| Component | Runs where | Role |
|---|---|---|
| **Avi Controller** | Outside the cluster | Configuration, API, analytics. Never in the request path. |
| **Service Engine (SE)** | On the data path | The proxy. Terminates TLS, validates tokens, runs DataScripts and WAF, meters tokens, selects pools. |
| **AKO** | In-cluster | Watches Gateway API objects and the `ai.ako.vmware.com` CRDs; reconciles them into Avi objects. |
| **Token service** (`jwt-issuer`) | In-cluster | Security Token Service (RFC 8693 model). Mints short-lived, target-bound credentials after a Kubernetes TokenReview. Called by workloads and by the console, never by the SE. |
| **ICAP classifier** | In-cluster | Prompt-injection scoring. Called by the SE over ICAP; not a proxy. |
| **Console** (`ai-gateway-ui`) | In-cluster | Reads the cluster through its own ServiceAccount and the Avi Controller's read-only REST API. Collects the token ledger. |

### 3.2 The request path

```
  caller ──TLS──▶  Service Engine  ──▶  backend pool
                   │
                   ├─ SSO / JWT validation      AIGatewayAuthPolicy
                   ├─ WAF request-body scan     AIGuardrailPolicy
                   ├─ ICAP classifier call      AIGuardrailPolicy.semantic
                   ├─ DataScript HTTP_REQ_DATA  model routing · budgets · MCP/A2A authz
                   └─ DataScript HTTP_RESP_DATA token metering · usage record
```

DataScript phases, and what each is for:

| Phase | Work |
|---|---|
| `HTTP_REQ` | Enable request-body buffering. Agent-card discovery (`/.well-known/agent.json`) returns early and is never body-inspected. |
| `HTTP_REQ_DATA` | Read the buffered body: `model` for tier selection, JSON-RPC `method` for MCP/A2A authorization. Run identity, target-binding and allow-list checks. Enforce budgets whose ceiling depends on the tier. |
| `HTTP_RESP` | Enable response buffering for metered and task-creating responses. |
| `HTTP_RESP_DATA` | Parse `usage` from the response body, increment the enforcement counter, append a usage record to the ledger ring. Capture new A2A task ids for affinity. |

### 3.3 Gateways in the estate

Each surface gets its own Gateway, so each has its own VIP, listener hostname and policy set.
`*.ai.avi.com` resolves through Avi DNS, so a new route is reachable without a DNS change.

| Gateway | VIP | Type | Serves |
|---|---|---|---|
| `avi-gateway` (`inference`) | 192.168.68.29 | LLM | `llm.ai.avi.com` — the model front door |
| `models-gateway` (`inference`) | 192.168.68.26 | LLM | `*.models.ai.avi.com` — per-model KServe endpoints |
| `mcp-gateway` (`inference`) | 192.168.68.27 | MCP | the approved MCP servers |
| `a2a-gateway` (`inference`) | 192.168.68.28 | A2A | every agent, at `<name>.ai.avi.com` |
| `avi-ingress` (`avi-system`) | 192.168.68.11 | LLM | cluster ingress and the console |

---

## 4. The console, tab by tab

The console is an Avi-Controller-styled SPA served by a single Go binary. Eight left-nav entries;
the Dashboard is a Preact island with five sub-tabs, and several pages carry their own tab bars.

```
Dashboard ─┬─ Topology        SE → gateways → backends, with health edges
           ├─ Agents          per-agent flow-path cards with token badges
           ├─ Flow Map        the estate as a graph of governed hops
           ├─ Tokens          the token ledger — true consumption, users vs agents
           └─ Live Counters   the enforcement counter, per identity against budget
Governance ┬─ Token Rate Limits       AITokenRateLimitPolicy
           └─ Model Routing Policies  AIModelRoutePolicy
Models                                InferencePool + KServe models + external backends
Gateways ──┬─ Gateways                Gateway API objects, DLP toggle per gateway
           ├─ Inference               live pool member weights
           └─ MCP                     MCP-annotated gateways
MCP Registry ┬─ MCP Registry          the approved tool-server allow-list
             └─ MCP Routes            HTTPRoutes publishing those servers
Agent Registry                        approved A2A agents; /.well-known/agents federation
Agents                                the Agent Factory — describe, draft, approve, provision
Chat                                  a governed client for proving enforcement end to end
```

### 4.1 Dashboard ▸ Topology

The SE data plane fronting every LLM, MCP and A2A VIP, and the pools behind them. Counters across
the top; a three-column graph below — Service Engine, Gateways, Backends — with an edge per route
and a health dot per node. Each gateway node shows its VIP, programmed state, route count and
whether DLP is enforcing.

![Dashboard ▸ Topology](images/console/01-dashboard-topology.png)

### 4.2 Dashboard ▸ Agents

One card per agent and MCP server showing the full governed flow path — which gateway it enters,
which auth mode, which model tier it calls, which tools it may use — with a token badge per card.
Workloads that make no LLM calls are labelled as such rather than shown as zero.

![Dashboard ▸ Agents](images/console/02-dashboard-agents.png)

### 4.3 Dashboard ▸ Flow Map

The same estate drawn as a graph, derived from observed hops rather than static config. Nodes are
gateways (agent, MCP, LLM); MCP tools sit behind the MCP gateway; auth mode is labelled per
gateway. Use this to answer "what actually talks to what, through which front door".

![Dashboard ▸ Flow Map](images/console/03-dashboard-flowmap.png)

### 4.4 Dashboard ▸ Tokens

The token **ledger** — accounting, not enforcement. Every metered response produces one immutable
record on the SE; the console drains the ring and stores it durably. Windows of 24 hours / 7 days /
30 days, CSV export, and a split between **Users** and **Agents** resolved at read time from the
issuer's persona list and the two registries.

The honesty rules matter. *Tokens measured* sums only records marked `exact`; fail-closed penalty
charges appear as budget events, never as measured tokens; and **measured exactly** states what
fraction of the window is real measurement. The footer names how many SE rings were drained and how
many records arrived.

![Dashboard ▸ Tokens](images/console/04-dashboard-tokens.png)

### 4.5 Dashboard ▸ Live Counters

The other number: the SE's own enforcement counter for the current window, per identity, against
that identity's budget. This is what returns 429. It is SE-local, window-scoped and deliberately
pessimistic — see [§8](#8-known-limits) for why it and the ledger legitimately differ.

![Dashboard ▸ Live Counters](images/console/05-dashboard-counters.png)

### 4.6 Governance

Two tabs, one per policy kind, listing objects across every namespace with their Kubernetes
acceptance status.

**Token Rate Limits** — every `AITokenRateLimitPolicy`, its target route and whether AKO accepted it.

![Governance ▸ Token Rate Limits](images/console/06-governance-tokenlimits.png)

**Model Routing Policies** — every `AIModelRoutePolicy`, with tier count and default tier.

![Governance ▸ Model Routing Policies](images/console/07-governance-modelroutes.png)

Clicking a policy name opens its editor. Saving writes the CRD; AKO regenerates the SE DataScript.
Nothing in the console talks to the Avi Controller to change configuration.

### 4.7 Models

Three sources of model backend in one place: `InferencePool` objects with their pod selector and
port, KServe models with their tier and readiness, and selectorless Service/EndpointSlice pairs used
to reach external endpoints through the SE.

![Models](images/console/08-models.png)

### 4.8 Gateways

**Gateways** lists every Gateway API object — class, VIP, listeners, routes, programmed state and
DLP state. The DLP control is a per-gateway enforce/shadow toggle backed by the guardrail policy's
mode delegation: the same rules either block or only log.

![Gateways](images/console/09-gateways.png)

**Inference** shows live pool members and their current scraper-derived weights — this is where you
watch metric-weighted load balancing move traffic.

![Gateways ▸ Inference](images/console/10-gateways-inference.png)

**MCP** lists gateways carrying the MCP annotation and the routes on them.

![Gateways ▸ MCP](images/console/11-gateways-mcp.png)

### 4.9 MCP Registry

The allow-list of Model Context Protocol servers the gateway may broker, read from the
`mcp-registry` ConfigMap. Each row carries endpoint, transport, scope, auth mode and a live
reachability probe. A server absent from this list has no route and cannot be fronted at all — this
is the *server-level* gate; per-tool authorization in `AIMCPRoutePolicy` is the *call-level* gate.

![MCP Registry](images/console/12-mcp-registry.png)

**MCP Routes** lists and creates the HTTPRoutes that publish those servers on an MCP gateway, with a
hostname guardrail so a route cannot be published on a listener that will not serve it.

![MCP Registry ▸ MCP Routes](images/console/13-mcp-routes.png)

### 4.10 Agent Registry

The A2A counterpart: the catalogue of agents the gateway fronts, by AgentCard, read from the
`agent-registry` ConfigMap and federated at `/.well-known/agents` on the A2A gateway VIP. Columns
are Agent, AgentCard, Skills, Scope, Auth and a live reachability dot.

`approved: false` means the agent appears here but is excluded from federation. Approval is what
moves an agent from catalogued to callable.

![Agent Registry](images/console/14-agent-registry.png)

### 4.11 Agents — the Agent Factory

Describe an agent in plain language; the factory drafts an `AgentSpec`; you approve; the platform
creates the workload, its ServiceAccount, its HTTPRoute on the A2A gateway (`<name>.ai.avi.com`,
DNS automatic), its `AIA2ARoutePolicy` and its registry entry — as one unit. Drafting and
provisioning are separate operations and only the second one writes.

The **Fleet** table tracks each agent's deployment, route and registry state; `runtime` records
which body is deployed (`go` or `adk`).

![Agents](images/console/15-agents.png)

### 4.12 Chat

A client that talks to the models **through the governed front door**, so it proves enforcement
rather than describing it. Pick an identity and a model; each reply reports the model the SE
actually routed to. An injection prompt returns 403 from the guardrail, an over-budget identity
returns 429, and the diagnostics name the exact layer that refused.

Two toggles change the shape of the turn: **Web agent** runs a tool loop where model turns cross the
LLM gateway and `web_search` / `web_fetch` calls cross the MCP gateway; **Coordinator** delegates
the turn to `agent-hub`, which chooses across every registered agent.

![Chat](images/console/16-chat.png)

---

## 5. How-to guides

Every guide below writes a Kubernetes object. AKO reconciles it into Avi configuration; the console
never configures the Avi Controller directly. Verification always means checking the cluster and
then checking behaviour on the wire.

### 5.1 Give a group a token budget

Budgets are keyed on the verified `group` claim from authentication, so this only does something
useful on a route that already has an `AIGatewayAuthPolicy`.

1. **Governance ▸ Token Rate Limits** — click the policy name to edit an existing one, or **+ Create**.
2. Under **Per-group budgets**, add a row: the group claim value exactly as the IdP emits it, and the
   ceiling in tokens per window.
3. Set **Window** (the counter resets on this boundary), **Token dimension** (`total`, `prompt` or
   `completion`), **Fallback budget** for identities in no listed group — `0` rejects them with 403 —
   and the **Reject status code**, conventionally `429` with `Retry-After`.
4. **Save & apply to cluster.** **View YAML** shows exactly what will be written first.

> **Name groups after the teams they stand for.** The lab estate uses `engineering`,
> `product-management` and `agents` (it once used `group1`/`group2`/`group3`, which told a
> viewer nothing). Group names are pure data as far as AKO is concerned — they are baked into
> the generated Lua as table keys — so renaming them is safe, but it must be done in the issuer
> and every policy together, since the claim value and the budget key must match exactly.
>
> **Every workload defaults into one group** (`agents`), which means the whole fleet shares one
> budget and one entitlement set — so a single noisy agent can neither be given its own quota
> nor routed elsewhere without moving every other agent with it. Putting one agent in its own
> group is what makes it individually controllable; the demo issuer takes an `AGENT_GROUPS`
> map (`serviceaccount:group`) for exactly that.

![Editing a token rate limit policy](images/console/17-governance-editor.png)

Creating one from scratch asks for the target `HTTPRoute` as well:

![Creating a token rate limit policy](images/console/23-tokenlimit-create.png)

**Verify.** The policy shows *Accepted* in the list. Send traffic as a member of that group and watch
**Dashboard ▸ Live Counters** climb; exceed the ceiling and the SE returns the reject code mid-window.

**Reset.** **Dashboard ▸ Live Counters ▸ Reset counters** bumps the `ai.ako.vmware.com/counter-epoch`
annotation, moving every counter to a fresh keyspace. Budgets, limit name and policy are untouched.
The ledger is unaffected — resetting enforcement does not rewrite history.

### 5.2 Route models to quality/cost tiers

1. **Governance ▸ Model Routing Policies** — click a policy to edit, or **+ Create**.
2. Name it, set the target `HTTPRoute`, and leave **Model field** as `model` unless callers put the
   model name somewhere non-standard in the request body.
3. Add a **tier** per backend class. Backend kind is one of:
   - `InferencePool` — a metric-weighted fleet of model pods.
   - `Service` — a core Service, including selectorless ones pointing at external endpoints.
   - `Provider` — an external API reached SE-native over egress, e.g. `generativelanguage.googleapis.com`.
4. Add **model mappings**: the requested model name, exact or a trailing-`*` prefix glob, and the tier
   it resolves to. Set a **default tier** — this is what an unrecognised model gets, and it is what the
   route degrades to if the policy is missing or unreconciled.
5. Optionally set **entitlements**: which groups may reach which tiers, and whether an unentitled
   request is **downgraded** to the caller's best allowed tier or **rejected**.
6. **Save**.

![Editing a model routing policy](images/console/18-modelroute-editor.png)

**Verify.** Call the front door from **Chat** with a model that maps to a tier — the reply reports the
model the SE actually routed to. Ask for a tier you are not entitled to and confirm the downgrade.

### 5.3 Add a model backend

1. **Models ▸ + Create**.
2. **Name**, **Namespace**, **Pod selector** (the label selector matching the model-server pods),
   **Target port**, and the endpoint-picker Service (unused with the native scraper).
3. **Create**. AKO builds the Avi pool and the scraper begins weighting members by KV-cache
   utilisation, queue depth and running slots.

![Creating an InferencePool](images/console/19-models-create.png)

**Verify.** **Gateways ▸ Inference** shows the members and their live weights. Members appear with
equal bootstrap weights before the first scrape completes.

### 5.4 Create a gateway for a surface

1. **Gateways ▸ + Create**.
2. **Name**, **Namespace**, **Gateway class** (`avi-lb`), and **Gateway type** — `LLM`, `MCP` or `A2A`.
   The type is what adds the surface-specific annotation and listener shape; `MCP` adds the
   `ai.ako.vmware.com/mcp` annotation and a wildcard HTTPS listener.
3. **Hostname**, **TLS secret** (required for HTTPS, MCP and A2A — both auth modes need TLS
   termination), and the HTTP/HTTPS ports.
4. **Create**.

![Creating a gateway](images/console/20-gateway-create.png)

**Verify.** The row shows *Programmed* with a VIP. `*.ai.avi.com` names resolve through Avi DNS with
no extra DNS work.

### 5.5 Approve an MCP server and publish it

Two gates, in order: the registry decides which servers may be fronted at all; the route publishes one
of them on an MCP gateway.

1. **MCP Registry** — confirm the server is present and `approved`, with a green reachability dot. The
   list is the `mcp-registry` ConfigMap; an unapproved server gets no route.
2. **MCP Registry ▸ MCP Routes ▸ + Create**.
3. Pick the **MCP Gateway** and the **MCP server (backend)** from the dropdowns — both are constrained
   to what exists, so a route cannot name a server the registry does not know.
4. Leave **Hostname** blank to inherit the gateway listener, or set one; the form refuses a hostname the
   chosen listener will not serve. Set the **path prefix** (`/mcp` by default).
5. **Create**.

![Publishing an MCP route](images/console/21-mcproute-create.png)

**Verify.** The route appears with its Kubernetes status. Call it without a token and expect `401`; call
a tool the caller's role does not hold and expect `403`.

### 5.6 Register an A2A agent

1. **Agent Registry ▸ + Add Agent**.
2. **Name**, **AgentCard URL** (`https://<name>.ai.avi.com/.well-known/agent.json`), **Skills**
   (comma-separated — these are the capability names the allow-list and the `scope` claim are written
   against), **Scope**, **Auth**, **Description**.
3. **Approved** publishes the entry at `/.well-known/agents`. Leave it off to catalogue an agent without
   advertising it.
4. **Register** — this upserts by name into the `agent-registry` ConfigMap.

![Registering an agent](images/console/22-agentregistry-add.png)

**Verify.** The row appears with a reachability dot, and `GET /.well-known/agents` on the A2A gateway
VIP returns approved entries only. Registration is catalogue and discovery — enforcement still comes
from an `AIA2ARoutePolicy` on the route.

### 5.7 Create an agent from a description

1. **Agents** — describe the agent in plain language in the box at the bottom, then **Draft**.
2. Read the drafted spec: name, model, skills, approved MCP servers, runtime (`go` or `adk`), prompt.
   Drafting writes nothing.
3. **✔ Approve & create**. The provisioner creates, as one unit: the ConfigMap, the ServiceAccount,
   the Deployment and Service, the HTTPRoute on the A2A gateway, the `AIA2ARoutePolicy` whose
   allow-list is derived from the declared skills, and the registry entry awaiting approval.

![The Agent Factory](images/console/15-agents.png)

**Verify.** The Fleet row turns ready across deploy, route and registry. Until the registry entry is
approved, the agent holds an identity and a route but no caller has been granted access to it.

### 5.8 Prove enforcement end to end

1. **Chat** — pick an **Identity** (this selects the persona whose claims are minted) and a **Model**.
2. Send a normal prompt. The reply names the model the SE actually routed to — that is tier routing
   working.
3. Send an injection prompt (the placeholder text is one). Expect `403`, and read the diagnostics: the
   panel names the layer that refused.
4. Send enough traffic to exceed that identity's budget. Expect `429`.
5. Toggle **Web agent** to run a tool loop across the MCP gateway, or **Coordinator** to delegate the
   whole turn to `agent-hub`.

![The chat playground](images/console/16-chat.png)

### 5.9 Read consumption and export it

1. **Dashboard ▸ Tokens**. Choose the window — 24 hours, 7 days, 30 days.
2. Read the four figures: tokens measured, derived cost, metered requests, and the share **measured
   exactly**. Treat the last one as the confidence in the other three.
3. **Users** and **Agents** are the same records partitioned by a `kind` resolved at read time from the
   issuer's persona list and the registries — not two mechanisms that can disagree.
4. **Export CSV** for the current window.

![The token ledger](images/console/04-dashboard-tokens.png)

Use **Live Counters** for "will this identity be rate-limited", and **Tokens** for "what did this
identity actually consume". They are different questions and different numbers by design.

---

## 6. Security posture

### 6.1 The theory

Three ideas carry the whole model.

**No ambient authority.** Nothing is trusted because of where it sits. A pod on the cluster network
cannot reach an agent, a model or a tool by addressing it; every hop crosses the Service Engine and
every hop carries a credential. Being inside grants nothing.

**Authorization decided twice, by components that share no code path.** The token service decides
whether a credential may *exist*; the Service Engine decides whether the credential presented is good
for *this* request. Neither is reachable from inside an agent and neither trusts the other's outcome. A
credential the token service refuses cannot be produced; one the SE refuses cannot be used.

**Credentials useless out of context.** An agent-to-agent token names one target, one capability and
one task, and expires in 60 seconds. Nothing an agent holds at any moment is useful against a
different target.

The agent participates in none of these decisions. It obtains a credential, presents it, and gets back
either a result or a status code naming the layer that stopped it.

```
  CALLER            MINT                 GRANT                ENFORCE            TARGET
┌──────────┐   ┌────────────────┐   ┌────────────────┐   ┌────────────────┐   ┌──────────┐
│ Pod + SA │──▶│ Token service  │──▶│ 60-second      │──▶│ Service Engine │──▶│ Backend  │
│ projected│   │ TokenReview +  │   │ token          │   │ DataScript,    │   │ netpol:  │
│ token    │   │ mint-time authz│   │ sub·target·    │   │ 4 checks       │   │ SE only  │
└──────────┘   └────────────────┘   │ skill·jti      │   └────────────────┘   └──────────┘
                                    └────────────────┘
```

### 6.2 Principals

Two kinds, proven by different authorities, never sharing a credential path.

| Principal | Proven by | Subject derived from | Reaches the gateway as |
|---|---|---|---|
| **Human** — real user | Corporate IdP: OIDC authorization code | The IdP's `sub` and group claims | An access token carrying `sub` + `group`, minted by the OIDC flow |
| **Human** — demo persona | The console's own ServiceAccount, via TokenReview, calling `POST /persona` | A **closed allow-list** of demo personas — never an agent name | A one-hour identity token carrying `sub` + `group` |
| **Workload** — agent, MCP server, console | Kubernetes: TokenReview on the pod's projected ServiceAccount token | The ServiceAccount name | Either a one-hour front-door identity token, or a 60-second token bound to one target and one skill |

Neither can be asserted. A workload cannot claim to be a human because TokenReview returns a
ServiceAccount, not an arbitrary name. The console cannot mint an agent identity because `/persona`
checks the subject against a closed persona list — the check exists so that compromising the console
does not yield a credential any agent allow-list would honour. And a shared ServiceAccount is refused
outright: `default` proves a *namespace*, not a workload, so `/exchange` rejects it.

**There is no endpoint that mints a subject on request.** The forgeable `GET /token` — which minted any
`sub` to anyone who could reach the service — was retired to `410 Gone` on 2026-08-19, and the issuer
logs every attempt against it by name so the retirement is verifiable rather than assumed.

Every agent has its own ServiceAccount, so the subject it presents downstream is a property of *where it
runs*, established by the cluster, not something in its configuration or reachable by editing it.

### 6.3 Minting — the token service

The token service is a Security Token Service in the **model** of RFC 8693: the caller presents a
credential it already holds, and gets back a narrower one — or a refusal. It is not the identity
provider for humans (the corporate IdP is), and it issues nothing on the basis of a name alone.

The wire form is a small JSON API rather than RFC 8693's form-encoded grant. The behaviour the RFC
specifies — an authorization server that may *refuse* an exchange — is the part that matters here, and it
is implemented.

```http
POST /exchange
Authorization: Bearer <caller's projected ServiceAccount token>
Content-Type: application/json

{"target": "weather-agent", "skill": "weather.monitor", "task": "<task id>"}
```

**Three gates, in order:**

1. **TokenReview.** The bearer token is verified against the Kubernetes TokenReview API, which returns
   the namespace and ServiceAccount. That is the subject — it is never read from the body.
2. **Identity specificity.** The SA must be in the agent namespace and must not be `default`: a shared
   ServiceAccount proves a namespace, not a workload, so it cannot carry an agent identity. `403`.
3. **Mint-time authorization.** The target's policy is consulted: may *this* caller hold *this* skill on
   *this* target? `403` and no token if not. If the policy cannot be evaluated the answer is **`503`,
   explicitly not a denial**, so a control-plane problem is never read as "this agent lacks access".

**Two mint modes:**

| Mode | Request | Lifetime | Used for |
|---|---|---|---|
| **Identity** | no `target`, no `skill` | 1 hour (`IDENTITY_TTL`) | Calling the LLM or MCP front door. Subject comes from TokenReview — this is what made retiring `GET /token` possible. Authorizes nothing on the A2A surface, where the allow-list needs a skill or a target. |
| **Exchange** | `target` + `skill` | 60 seconds (`EXCHANGE_TTL`) | One agent calling one other agent about one task. |

**Claims emitted:**

| Claim | Carries | Enforced by |
|---|---|---|
| `sub` | The caller — a ServiceAccount name, or a persona for `/persona` | Allow-list match at the SE |
| `group` | The caller's group | Budget tier selection; model-tier entitlement |
| `aud` | The audience the SE validates | SE JWT validation |
| `target` | The single agent this token is good for | Compared to `AIA2ARoutePolicy.agentAccess.targetAgent`; mismatch rejected |
| `skill` | The capability requested | Matched against the caller's allow-list at the SE |
| `jti` | The task this token was minted for | Correlation across hops |
| `exp` | 60 seconds (exchange) or 1 hour (identity) | Rejected on expiry at the SE |

> **Why `target` is a claim and not the audience.** Binding the audience to the target would be the
> strongest form — a token stolen from one agent would be structurally useless against another. The SE
> validates a single audience per `AIGatewayAuthPolicy` (`Audiences[0]`), and one shared policy currently
> covers the LLM route *and* every A2A route, so minting a per-target audience would 401 the whole estate
> until each surface has its own auth policy. The `target` **claim** carries the binding today and the SE
> enforces it; per-target audience switches on with `EXCHANGE_AUDIENCE=target` once the auth policies are
> split per route. That flip is a **simultaneous cutover**, not a rolling change.

**Personas.** `POST /persona` exists for the console's identity switcher, because TokenReview cannot
prove "alice" — there is no such workload. It checks two things `GET /token` never did: the **caller** is
proven to be the console's own ServiceAccount, and the **subject** must be one of a closed set of demo
personas. Real users log in through the OIDC flow instead. There is no `act` claim — delegation is
constrained by *who may call the endpoint*, not asserted on the wire.

### 6.4 Enforcement at the Service Engine

Enforcement is a DataScript AKO generates from the policy objects and attaches to the virtual service.
It is regenerated whenever policy changes, so it is never hand-edited and cannot drift from the CRDs
that describe it.

**Checks, in order:**

1. **Signature and expiry** — RS256 against the issuer's published keys. Failure is `401`.
2. **Target binding** — the `target` claim is compared to the agent this route serves
   (`agentAccess.targetAgent`). A token minted for another agent is refused here, before its skill is
   even considered, so a misdirected credential fails on the strongest available ground.
3. **Caller identity** — the subject claim is read and looked up in the route's allow-list.
4. **Capability** — what the caller is attempting is matched against what that caller is allowed.

**Three matching dimensions.** An allow-list entry may name any of them; entries match exactly, or as a
prefix when they end in `*`.

| Dimension | Source | Used for |
|---|---|---|
| Skill | The token's `skill` claim (claim name configurable via `skillClaim`) | Capability-level control — the primary dimension |
| Method | The JSON-RPC `method` in the request body | A2A and MCP protocol operations |
| Path | The request path, without query string | Agents serving plain REST alongside A2A |

**Fail-closed switches.** Two settings characterise every request, so nothing reaches a backend unmatched:

- **`requireMethod`** — reject any request from which no JSON-RPC method could be read. Set on agents
  that speak only A2A. Agent-card discovery is exempted beforehand, so discovery still works.
- **`authorizePaths`** — evaluate the rules on every request rather than only JSON-RPC ones, adding path
  as a matchable dimension. Set on agents whose REST endpoints are their interface. It is opt-in because
  a policy that lists no paths would otherwise start denying every REST call the moment it was enabled.

**Status codes name the layer.** `401` the token was rejected · `403` a policy refused the call · `429`
the budget for this group is exhausted. The agent runtimes surface these verbatim rather than collapsing
them into a transport error.

### 6.5 Why the credential rides in the query string

On this Avi build the two JWT-validation paths are mutually exclusive, and only one exposes claims to
policy:

- **`CLIENT_OAUTH`** (`authMode: oauthBrowser`) — the SE runs the browser auth-code/session-cookie flow.
  Claims are readable via `oauth_get_claim()`, but a client-presented `Authorization: Bearer` is ignored
  and the SE 302-redirects, so machine clients cannot authenticate.
- **`SSO_TYPE_JWT`** (the resource-server path) — the SE validates a bearer JWT and returns 200/401 with
  no redirect, which is the machine-correct behaviour, but it **strips the `Authorization` header**
  before any DataScript runs and `oauth_get_claim()` returns nil. The token is validated and its claims
  are invisible to policy.

`authMode: jwtQuery` threads the needle: `SSO_TYPE_JWT` with `jwt_location = JWT_LOCATION_QUERY_PARAM`.
The SE validates the token from `?jwt=`, and — unlike the header — the query parameter is not stripped,
so the DataScript reads the same already-validated token and decodes its claims. The decode is
trustworthy precisely because the SE verified signature, `aud` and `exp` first; AKO never emits the
decode helper on a VS that is not enforcing `jwt_config`.

This is a deliberate, documented deviation. **RFC 6750 §2.3** defines the URI query parameter method and
says it *"SHOULD NOT be used unless it is impossible to transport the access token in the `Authorization`
request header field or the request entity-body"* — which is exactly the condition here. The mitigations
that make it acceptable:

| Risk | Mitigation | State |
|---|---|---|
| Token visible in transit | TLS mandatory on both auth modes | Enforced |
| Token in access logs | ❌ **Nothing works.** `uri_query_field_rules` match on the parameter *name*, so a rule written as `jwt=` never fires — and correcting it does not help: on Avi 30.2.1+ masking a query field populates `orig_uri` ("Unparsed URI") with the original request line, token and all. No configuration removes it. | **Cannot be mitigated** — rely on the 60-second lifetime |
| Long exposure window | 60-second tokens, minted per request | Enforced |
| Replay against another target | `target` claim checked at the SE | Enforced |
| Token forwarded to the backend and logged there | Query-strip before the pool | **Open** — the exact SE query-rewrite primitive is unconfirmed, so AKO does not emit it. This is the remaining hardening step for production `jwtQuery`. |
| Request line longer than ~12 KB | The SE rejects it with `400` (`client_max_header_size`, applied per line). Tokens up to ~12 KB decode correctly and RBAC stays right — it fails loudly, never by emptying a claim | Understood, measured |
| Guardrail WAF matching the JWT as a secret | Request-phase rule target excludes `!ARGS:jwt` | Fixed |

The clean fix is an SE capability: after JWT validation, either preserve the `Authorization` header to
DataScripts or expose validated claims via a `get_jwt_claim()` API. That is filed as an RFE; with it,
`jwtQuery` collapses to "the same thing, read from the header", and the trade-off disappears.

### 6.6 From a user's perspective

A person never holds a credential that reaches a model, a tool or an agent.

1. **Login.** The OIDC authorization-code flow against the corporate IdP. The console ends up holding the
   user's token and their group claim. For the demo personas that have no directory behind them, the
   console instead calls `POST /persona` with its own ServiceAccount token; the subject must be one of a
   closed persona list.
2. **The call crosses the SE like any other traffic** — there is no in-cluster shortcut. Signature,
   audience, allow-list and budget are all checked at the Service Engine.
3. **Budget and attribution follow the user, not the console.** The `group` claim selects the budget tier,
   `429` lands on that group, and the ledger records the spend against `sub` = the user.
4. **The console cannot escalate.** The one thing it can mint for a person is a persona token, and the
   persona list explicitly excludes every agent identity — so compromising the console yields no
   credential that an agent allow-list would honour.

What the user sees when something refuses: `401` (their session is not valid for that target), `403`
(policy, guardrail or entitlement) or `429` (their group's budget). The Chat panel names which.

### 6.7 From an agent's perspective

1. **Identity is where it runs.** Each agent has its own ServiceAccount; its projected token is the only
   thing it starts with, and TokenReview is what attests it.
2. **It mints per request, not per session.** Before each agent-to-agent call it exchanges its SA token
   for a 60-second credential naming that one target and that one skill, with `jti` set to the current
   task. A graph that runs for minutes crosses many 60-second windows, which is why minting happens
   inside the request path rather than at construction time.
3. **It cannot widen its own grant.** The allow-list on its route is derived by the provisioner from its
   declared skills, so advertised capability and enforced capability come from one source. Editing its
   ConfigMap does not change what the SE will accept.
4. **Sub-agents inherit, never exceed.** Where a runtime fans out to parallel specialists, each is
   restricted to the MCP servers the agent itself was granted — validated when the spec is written and
   again when the runtime loads its configuration, because a ConfigMap can be edited afterwards.
5. **It is unreachable except through the gateway.** NetworkPolicy admits only the SE data subnet, so
   there is no ClusterIP path to it for another pod to use.
6. **The wire contract is fixed, the body is not.** Go and ADK runtimes serve the identical
   `tasks/send` · `tasks/get` · `tasks/cancel` surface; the framework's own A2A server is deliberately
   unused because it speaks a newer dialect than every deployed allow-list is written against.

### 6.8 Network isolation

Policy governs what a caller may ask for; NetworkPolicy governs whether the backend is reachable at all,
so the policy plane cannot be sidestepped by addressing a pod directly.

- Every agent and MCP server accepts ingress **only from the SE data subnet**, on its own port. Kubelet
  probes are permitted by the CNI independently, so health checking is unaffected.
- A legitimate in-cluster caller — a CronJob triggering a digest agent — is admitted by explicit **pod
  label**, never by namespace: a namespace-wide allowance would re-admit every other workload including
  the coordinator, which is precisely what the rule exists to prevent.
- Workloads that reach outside the estate send **egress back through the SE**, so the one hop that leaves
  the environment is observable on the same data plane as everything inside it.

This establishes reachability, not identity. A NetworkPolicy admits an address range, so it constrains
*where* a connection may originate, not *what* originated it. Identity on that hop is the credential the
SE already validated; the network rule narrows who can attempt the connection at all.

### 6.9 Content inspection

Two layers, both enforced by the SE, both authored by AKO from `AIGuardrailPolicy`.

**Signature layer — Avi WAF (ModSecurity-based).** Built-in detectors for cloud and API secrets
(AWS/GCP/OpenAI/GitHub incl. fine-grained PATs/Slack), private keys and JWTs; PII (SSN with validity
ranges, credit card, email); prompt-injection patterns; and MCP tool-abuse patterns (command injection,
path traversal, SSRF). Plus per-policy keyword denylists and custom regex. Injection detectors run a
normalised pass (lowercase, URL/unicode decode, whitespace strip) *and* a base64 pass, so spaced-out
text, zero-width tricks and `%`-encoding are caught. Pre-canned profiles per surface: `BlockLLM`,
`BlockMCP`, `BlockLLMAndMCP`. A mode-delegation trick keeps Block vs Log a per-rule flip — that is what
the console's DLP toggle drives. Request **and** response bodies are inspected, so regulated content is
caught leaving as well as arriving.

**Semantic layer — ICAP classifier.** Signatures cannot catch novel or paraphrased injection. The SE
buffers the body and *calls* an embedding-prototype classifier over ICAP (RFC 3507), then enforces the
verdict itself; the model is never in the request path. Hardened to v2 after false positives on
imperative-but-benign prompts, with a gray-zone LLM-judge cascade for uncertain scores.

### 6.10 Standards referenced

Compliance is claimed where it holds and qualified where it does not. Two entries are deliberate,
documented deviations.

| Standard | Where it is used | Conformance |
|---|---|---|
| **RFC 6749** — OAuth 2.0 Authorization Framework | The authorization-code flow behind `authMode: oauthBrowser`; the SE runs it natively against the issuer | Conforms |
| **OpenID Connect Core 1.0** | Identity layer for human principals; `sub` and `group` claims; the issuer publishes OIDC discovery metadata | Conforms |
| **RFC 7519** — JSON Web Token | Every credential in the system | Conforms |
| **RFC 7517** — JSON Web Key | The JWKS the SE validates against; AKO fetches it and embeds it in the `JWTServerProfile` | Conforms |
| **RFC 7515** — JSON Web Signature | RS256 with a published `kid` on every token | Conforms |
| **RFC 6750** — Bearer Token Usage | Header presentation in `oauthBrowser` | **Deviation** in `jwtQuery`: §2.3 permits the URI query method only when the header and body are impossible, which is the condition on this Avi build. Mitigations and the remaining gap are in §6.5. |
| **RFC 8693** — OAuth 2.0 Token Exchange | `POST /exchange` — a caller presents a credential it holds and receives a narrower one, and the service may refuse the exchange | **Model, not grammar.** The behaviour conforms; the wire form is a JSON body, not the RFC's `grant_type=token-exchange` form encoding, and there is no `actor_token` / `act` claim. |
| **RFC 8707** — Resource Indicators for OAuth 2.0 | Restricting a token to one target | **Partial.** The concept is realised as a `target` claim enforced at the SE. Per-target *audience* binding is implemented but off by default — see the note in §6.3. |
| **RFC 9700** — Best Current Practice for OAuth 2.0 Security | Short-lived, audience-restricted, target-bound tokens; no durable credential in an agent | Followed, with the token-in-URL exception above |
| **RFC 3507** — ICAP | How the SE calls the semantic classifier | Conforms |
| **MCP — Authorization** (spec rev 2025-11-25) | Resource-server model for tool traffic | Aligned |
| **A2A — Enterprise-ready** | Agent cards and declared security schemes | Aligned |
| **Kubernetes TokenReview** | Attesting workload identity before any mint | Conforms |

---

## 7. Failure behaviour

How the system behaves when something breaks is part of the design.

| Condition | Behaviour |
|---|---|
| Enforcement script raises | `500`. A script error fails closed — a request is never admitted because the code that would have judged it failed. |
| Credential expires mid-run | Does not arise: credentials are minted per request, so a long graph simply mints again. |
| Token service unreachable | No credential is obtained, the call goes out without one, and the SE returns `401`. The failure surfaces as an authentication refusal, never as an unauthorized success. |
| Caller removed from an allow-list | Denied from the next configuration push; any credential already held expires within a minute. |
| Budget exhausted | `429` at the model surface, mid-loop. Tool results are token-hungry, so this usually lands partway through a multi-step turn. |
| NetworkPolicy denies | The connection is dropped, not refused — the caller sees a timeout. Diagnose with a short client timeout rather than waiting on an application-level one. |
| Backend for a running task is lost | Task-affinity pinning expires with its timeout; later calls about that task route normally and the task is no longer retrievable. |
| Model-route policy missing or unreconciled | The route degrades to its own backend: no tiering, no outage. |
| AKO rolling | The front door can 500 for roughly 90 seconds while it reconciles. Wait and retry; do not roll back. |

The pattern: every failure mode resolves toward refusal. A component that cannot make a decision does not
pass the request to something that will assume the decision was made.

---

## 8. Known limits

Stated plainly, because several of them change what the numbers mean.

| Limit | Detail |
|---|---|
| **Streaming is not metered** | With `stream: true` the SE cannot read the response body without buffering, and buffering collapses streaming. Streamed requests currently bypass the budget (count 0). Use non-streaming where budgets must hold. The fix is a native SE per-chunk event — filed as an RFE. |
| **Enforcement counters are per-SE and per-VS** | Each SE enforces against its local table, so a budget can overshoot by roughly the SE count, and the LLM / MCP / A2A virtual services keep separate tables. Exact fabric-wide budgets need a native distributed counter — RFE. |
| **Counters have no history** | The enforcement key is window-scoped with a TTL; at the boundary it drops to zero and the bucket is gone. History lives in the ledger, not the counter. |
| **Ledger vs counters differ by design** | The counter is fast, local and pessimistic; the ledger is exact, durable and attributable. Treat a gap as expected, not as a bug. |
| **`jwtQuery` forwards the token upstream** | The query string reaches the backend, so the token can land in *its* logs. Query-strip before the pool is the open hardening step (§6.5). |
| **Request-body buffer is 32 KB for model routing** | `model` sits at the start of the JSON so the head suffices; larger bodies are not yet validated. |
| **Agent-card discovery is never body-inspected** | `/.well-known/agent.json` returns early by design, so discovery works before authorization. |
| **`agentAccess` was fail-open on non-JSON-RPC GETs** | Closed by `requireMethod` / `authorizePaths`; routes that predate those switches must set one of them. |
| **The ledger collector's ceiling is ~200 rps** | A drain walks back at most 1,000 sequence numbers; at the default 5 s poll, anything above ~200 rps sustained ages past its reach and is reported `lost`. Measured: at 333 rps, 34 % of records were lost. The SE's *recording* stays exact. |
| **The ledger's ring is per-VS** | Every metered route needs its own collector target. A route the collector does not poll accumulates records that expire unread — and looks identical to a route that never metered. |
| **Metering is correct only because the SE serializes DataScripts** | The counter and ring head are read-modify-writes with no atomic primitive; a multi-core datapath or a scaled-out VS could lose updates, and nothing in the ledger would report it. |
| **Tier pools are baked at policy-reconcile time** | Editing an `EndpointSlice` behind a `Service` tier never pushes new members; only a patch to the policy **spec** forces a re-resolve. A stale pool presents as a hang, not a 4xx. |
| **A `remote` tier names one peer** | Peer down means tier down. A tier that names a set of sites is designed, not built. |
| **Renaming the cluster or flipping readable names re-creates every Avi object** | VIPs float and **per-VS settings are lost** — `full_client_logs` reverts to off estate-wide, and anything pinning a VIP by address (console `GATEWAY_VIP`, hub `A2A_GATEWAY_VIP`) breaks until repointed. |
| **A 4xx is not proof of a guardrail block** | Confirm the VS actually carries a WAF policy or ICAP profile *and* that the log shows `response_code: 403` with `waf_log: REJECTED` before claiming anything was blocked. |
| **A `jwtQuery` token in the URL cannot be masked from Avi logs** | `uri_query_field_rules` match the parameter *name*, and even a correct rule leaves the raw token in `orig_uri` on Avi 30.2.1+. No configuration removes it. |
| **Registry entries are ConfigMaps, not CRDs** | Deliberate — auditable with standard tooling, no controller reconciliation. But an apply that overwrites the ConfigMap drops hand-added entries; keep entries in the manifest, never hand-patch the live object. |
| **OAuth issuer is a single pod** | An issuer restart re-IPs and breaks the pinned OAuth pool until reconciled. An HA issuer is needed for production. |
| **AKO snapshots the JWKS** | Key rotation at the IdP is an estate-wide `401` until AKO re-reconciles — a reason the workload issuer stays in-cluster. |

---

## 9. Where things live

| Thing | Location |
|---|---|
| AKO controller, translators, DataScript generation | `ako-gateway-api/aigateway/` in this repo |
| CRD schemas | `helm/ako/crds/ai.ako.vmware.com_*.yaml` |
| Design and reference docs | `docs/gateway-api/ai-gateway-*.md` |
| Console (Go binary + embedded SPA) | `ai-gateway-ui/` — separate repo; SPA under `web/`, Dashboard island under `web/dash/` |
| Token service, agent runtimes, MCP servers, demo manifests | `ako-inference-demo/` — separate repo |
| Security narrative in full | `ako-inference-demo/agent-trust-chain.md` |
| Release history | [ai-gateway-release-notes.md](ai-gateway-release-notes.md) |

### Related docs

- [Overview](ai-gateway-overview.md) — capability summary by status
- [Authentication](ai-gateway-auth.md) — `AIGatewayAuthPolicy`, both auth modes
- [AI Gateway reference](ai-gateway.md) — auth and token-counting reference, counters endpoint
- [Model routing](model-routing.md) · [Inference extension](inference-extension.md)
- [Guardrails](ai-gateway-guardrails.md) · [Semantic guardrails](ai-gateway-guardrails-semantic.md)
- [MCP gateway](ai-gateway-mcp.md) · [A2A gateway](ai-gateway-a2a.md) · [Agent registry](ai-gateway-agent-registry.md)
- [Agent factory](ai-gateway-agent-factory.md) · [Agent runtimes](ai-gateway-agent-framework.md) · [RAG](ai-gateway-rag.md)
- [Token ledger](ai-gateway-token-ledger.md)
- [Install guide](ai-gateway-install.md)
