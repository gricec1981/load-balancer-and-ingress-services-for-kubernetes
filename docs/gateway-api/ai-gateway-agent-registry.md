<!--
  Implemented on feature/ai-a2a-gateway. Deliberately mirrors ai-gateway-mcp.md §9
  in structure (ConfigMap catalog, not a CRD). A2A per-agent enforcement stays in
  AIA2ARoutePolicy — this registry is discovery/catalog/federation only.
  Drafted 2026-07-02.
-->

# AKO AI Gateway — A2A Agent Registry

> **Status: Implemented (feature/ai-a2a-gateway).** This document covers the **Agent
> Registry** — a ConfigMap-backed catalog of known A2A agents that the AI Gateway console
> can browse, add to, and publish for federation. It is the direct A2A counterpart to the
> [MCP server registry](ai-gateway-mcp.md#9-mcp-server-registry--catalog-onboarding-option-a).
>
> The registry is **catalog and discovery only** — it deliberately mirrors the MCP
> registry's ConfigMap design rather than introducing a new CRD. Per-agent enforcement
> (skill authorization, session affinity, push-notification egress control) stays in
> [`AIA2ARoutePolicy`](ai-gateway-a2a.md); the registry is the layer above that decides
> *which agents exist* in the fleet.

---

## 1. Overview

The Agent Registry answers a single question: **which agents are approved for this
gateway, and how do other systems discover them?** It does three distinct jobs:

| Job | What it means | Where it lives |
|---|---|---|
| **Catalog** | A browsable list of registered agents with their skills, auth type, scope, and reachability | `agent-registry` ConfigMap, key `agents.json` |
| **Approval gate** | Only agents with `approved: true` are published for federation | checked at read time by `handleWellKnownAgents` |
| **Federation endpoint** | Other gateways or orchestrators can `GET /.well-known/agents` to discover the approved fleet | served by the `ai-gateway-ui` backend via the `agent-registry-route` HTTPRoute on the A2A Gateway |

An agent listed in the registry with `approved: false` appears in the console for
operator review but is **not** published at `/.well-known/agents` and has no gateway route
until an `AIA2ARoutePolicy` is explicitly created for it. The structural default-deny
property comes from the gateway routing layer, not the registry itself.

---

## 2. Storage — the `agent-registry` ConfigMap

The registry lives in a **ConfigMap** (namespace `inference`, name `agent-registry`), key
`agents.json` — a JSON array of `AgentInfo` objects. This is a deliberate design choice:
a ConfigMap requires no CRD, no controller reconciliation, and no webhook. It is readable
and writable with `kubectl` and is audited by whatever tracks ConfigMap mutations.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: agent-registry
  namespace: inference
data:
  agents.json: |
    [
      {
        "name": "ops-agent",
        "cardURL": "https://ops.demo.local/.well-known/agent.json",
        "skills": ["infra.inspect", "infra.remediate", "metrics.get"],
        "health": "https://ops.demo.local/healthz",
        "scope": "local",
        "auth": "jwt",
        "approved": true,
        "description": "Infrastructure operations agent — inspect and remediate cluster resources."
      },
      {
        "name": "security-agent",
        "cardURL": "https://sec.demo.local/.well-known/agent.json",
        "skills": ["vuln.scan", "policy.audit"],
        "scope": "local",
        "auth": "jwt",
        "approved": true,
        "description": "Security posture and vulnerability scanning agent."
      }
    ]
```

### `AgentInfo` field reference

| Field | Type | Description |
|---|---|---|
| `name` | string | Unique agent name (registry key; upserts match on this). |
| `cardURL` | string | The Agent Card discovery URL (`/.well-known/agent.json`). |
| `skills` | []string | Agent Card `skills[]` ids — shown in the console and in the federation doc. |
| `health` | string | Optional URL; if set, probed for reachability at read time (2-second timeout). Result fills `reachable` — **never persisted**. |
| `scope` | string | `public` or `local`. Informational — not enforced by the registry itself. |
| `auth` | string | `none`, `oauth`, or `jwt`. Describes the auth the agent expects; used for display. |
| `approved` | bool | When `true`, the entry is included in `/.well-known/agents`. When `false`, the entry is visible in the console only. |
| `description` | string | Free-text description shown in the console table. |
| `reachable` | string | Computed at read time: `""` (health not configured), `up`, or `down`. Never written to the ConfigMap. |

---

## 3. Backend implementation

The `ai-gateway-ui` Go backend (`/Users/cgrice/ai-gateway-ui`) implements the registry
in three functions in `k8s.go` and two HTTP handlers in `server.go`:

| Symbol | File | What it does |
|---|---|---|
| `AgentInfo` struct | `k8s.go` | The Go type for one registry entry. |
| `ListAgentRegistry(ns, cmName)` | `k8s.go` | Reads the ConfigMap, unmarshals `agents.json`, probes `health` URLs. |
| `AddAgentToRegistry(ns, cmName, agent)` | `k8s.go` | Upserts by `name` into `agents.json`; creates the ConfigMap if absent. Clears `reachable` before writing. |
| `handleAgentRegistry` | `server.go` | `GET /api/agentregistry` → lists; `POST /api/agentregistry` → upserts (name + cardURL required; scope defaults to `local`, auth to `jwt`). |
| `handleWellKnownAgents` | `server.go` | `GET /.well-known/agents` → filters to `approved: true` and returns `{name, card_url, skills, description}`. |

Routes registered in `main.go`:

```
/api/agentregistry     → handleAgentRegistry   (GET list / POST upsert)
/.well-known/agents    → handleWellKnownAgents  (GET — federation endpoint)
```

### RBAC requirement

`AddAgentToRegistry` patches or creates the `agent-registry` ConfigMap. The
`ai-gateway-ui` ClusterRole (`k8s/01-rbac.yaml` in the UI repo) must grant `configmaps`
verbs `create,update,patch` in addition to `get,list` — this was updated as part of the
agent registry implementation.

---

## 4. Console — Agent Registry page

The console has an **"Agent Registry"** left-nav item that renders a table and an add
form.

**Table columns:** Agent / AgentCard / Skills / Scope / Auth / Status

The **Status** column shows a coloured dot: green (`up`) when the `health` URL is
reachable, red (`down`) when unreachable, grey when no health URL is configured. An agent
with `approved: false` is shown with a warning indicator.

**"+ Add Agent" form fields:**

| Field | Notes |
|---|---|
| Name | Required. Used as the registry key (upserts overwrite by name). |
| AgentCard URL | Required. The `/.well-known/agent.json` URL for the agent. |
| Skills | Comma-separated skill ids (e.g. `infra.inspect,metrics.get`). |
| Scope | `local` (default) or `public`. |
| Auth | `jwt` (default), `oauth`, or `none`. |
| Description | Free text. |
| Approved | Checkbox; when checked, the agent will appear in `/.well-known/agents`. |

Submitting the form POSTs to `/api/agentregistry`. The backend upserts the entry, and the
page refreshes.

---

## 5. Federation — `/.well-known/agents`

The `handleWellKnownAgents` handler serves a filtered view of the registry at
`/.well-known/agents`, returning only entries where `approved: true`:

```json
{
  "agents": [
    {
      "name": "ops-agent",
      "card_url": "https://ops.demo.local/.well-known/agent.json",
      "skills": ["infra.inspect", "infra.remediate", "metrics.get"],
      "description": "Infrastructure operations agent — inspect and remediate cluster resources."
    },
    {
      "name": "security-agent",
      "card_url": "https://sec.demo.local/.well-known/agent.json",
      "skills": ["vuln.scan", "policy.audit"],
      "description": "Security posture and vulnerability scanning agent."
    }
  ]
}
```

This endpoint is exposed on the **A2A Gateway VIP** via an `HTTPRoute`:

```yaml
# From avi-mcp-gateway-demo/k8s/07-agent-registry.yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: agent-registry-route
  namespace: inference
spec:
  parentRefs:
    - name: a2a-gateway
  hostnames:
    - a2a.demo.local
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /.well-known/agents
      backendRefs:
        - name: ai-gateway-ui
          port: 80
```

No `AIA2ARoutePolicy` is attached to this route — the federation endpoint is intentionally
**public discovery** (no auth required; it returns only approved, non-sensitive metadata).

Test via:

```bash
curl -sk -H 'Host: a2a.demo.local' https://<A2A-VIP>/.well-known/agents
```

---

## 6. Demo manifest

The demo manifest at `avi-mcp-gateway-demo/k8s/07-agent-registry.yaml` ships the
`agent-registry` ConfigMap pre-populated with `ops-agent` and `security-agent`, and the
`agent-registry-route` HTTPRoute above. Apply it after the A2A gateway is up:

```bash
kubectl apply -f avi-mcp-gateway-demo/k8s/07-agent-registry.yaml
```

---

## 7. How to add an agent

### Option A — console (recommended)

1. `kubectl port-forward -n inference svc/ai-gateway-ui 8080:80`
2. Open `http://localhost:8080` and navigate to **Agent Registry**.
3. Click **+ Add Agent**, fill in the form, check **Approved** if the agent should be
   published, and click **Add**.
4. The console refreshes and the new agent appears in the table.

### Option B — edit the ConfigMap manifest and apply

1. Open `avi-mcp-gateway-demo/k8s/07-agent-registry.yaml` and add an entry to
   `agents.json`.
2. `kubectl apply -f avi-mcp-gateway-demo/k8s/07-agent-registry.yaml`

Example entry to add:

```json
{
  "name": "analytics-agent",
  "cardURL": "https://analytics.internal/.well-known/agent.json",
  "skills": ["data.read", "report.generate"],
  "scope": "local",
  "auth": "jwt",
  "approved": false,
  "description": "Data analytics and reporting agent — pending review."
}
```

Setting `approved: false` lists the agent in the console for review but keeps it out of
`/.well-known/agents` until an operator approves it.

### What `approved: false` means

An agent with `approved: false`:

- **Appears** in the console Agent Registry table with a warning indicator.
- **Is NOT published** at `/.well-known/agents`.
- **Has no gateway route** — no `AIA2ARoutePolicy` is automatically created. The gateway
  does not front the agent until a route and policy are explicitly applied.

This two-step (register → approve → route) is the governance checkpoint: operators can
catalog discovered agents from remote registries, review them, then approve the ones that
earn a place in the fleet.

---

## 8. Relationship to `AIA2ARoutePolicy`

The agent registry is **not** the enforcement layer. It is the catalog. Registering an
agent (even with `approved: true`) does not create a gateway route or a governing policy.
Per-agent enforcement — skill authorization, task/context session affinity,
push-notification egress control — is configured separately via an
[`AIA2ARoutePolicy`](ai-gateway-a2a.md) that targets the agent's `HTTPRoute`.

The registry's `skills[]` data is intended to pre-fill the `AIA2ARoutePolicy.spec.skillAccess`
role × skill matrix (the operator checks boxes against the real skill list rather than
typing skill ids by hand), but this pre-fill step is a console UX concern, not an
enforcement coupling.

---

## 9. Related docs

- [A2A Gateway & Agent-to-Agent Routes](ai-gateway-a2a.md) — `AIA2ARoutePolicy`, skill
  authorization, session affinity, push-notification egress control
- [MCP Gateway & MCP-Specific Routes](ai-gateway-mcp.md) — the sibling MCP server registry
  (§9) whose ConfigMap design this mirrors
- [AI Gateway Overview](ai-gateway-overview.md) — the full capability picture
