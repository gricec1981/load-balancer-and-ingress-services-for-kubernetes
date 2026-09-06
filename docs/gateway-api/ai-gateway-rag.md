# AI Gateway RAG — the estate searches its own source

> **Status: BUILT (2026-08-15), live and chat-verified.** `rag-service`
> (`ako-inference-demo/rag-service`, ns `mcp`, port **8979**) serves
> `https://rag.ai.avi.com/mcp`. The hub's chat answers questions about the
> estate's own source with GitHub citations via `code_search`.
> Companion to [ai-gateway-mcp.md](ai-gateway-mcp.md) (governed MCP tools),
> [ai-gateway-agent-factory.md](ai-gateway-agent-factory.md) (agent-runtime),
> and [ai-gateway-guardrails-semantic.md](ai-gateway-guardrails-semantic.md)
> (the indirect-injection gap this finally demos — now staged as UC3b).
>
> **Since the build, three things moved** (§8 has the detail):
>
> - **It lives behind the MCP gateway, authenticated.** The route's `parentRef`
>   is `mcp-gateway` (VIP `.27`), not `a2a-gateway` (`.28`), and `rag-auth`
>   enforces `jwtQuery` — an unauthenticated call gets `401`. The "MCP routes
>   carry no auth policy" note below is obsolete.
> - **Its embeddings are metered.** They ride the LLM front door as
>   `sub=rag-service`, group `agents` (the group formerly called `group2`), so
>   RAG shows on Dashboard ▸ Agents as an MCP tool card with a token badge. The
>   token is minted by **exchanging the pod's ServiceAccount** at `POST
>   /exchange`; the `?sub=&group=` in `JWT_ISSUER_URL` is vestigial, since
>   `GET /token` now returns `410`.
> - **Its registry entry is in `agent-hub`'s `manifests.yaml`.** It was added by
>   hand once and an unrelated apply silently wiped it, taking `code_search` out
>   of the hub. Never hand-patch that ConfigMap.
>
> **Build deltas vs. the plan below:** service port is **8979** (8978 was taken
> by nmap-mcp); the embedder rejects inputs over its **512-token physical
> batch** (not the 2048 model context) → chunks capped at 1200 chars with
> per-text fallback; CPU embedding is seconds-per-chunk → `nomic-embed`
> ISVC scaled to **3 replicas** (`minReplicas: 3`) + 2 concurrent client
> batches; pods resolve `ai.avi.com` natively (no VIP pinning anywhere,
> spike S2 collapsed — the hub already had a TLS child-MCP client);
> fresh egress HTTPRoutes came up 503 until **delete+recreate** (the known
> ExternalName-workaround quirk), and geo-DNS pins differ PC vs. lab —
> `codeload`'s EndpointSlice carries both answers. Private-repo indexing
> still awaits the `rag-github-token` Secret (PAT); the public AKO fork indexes
> without it.

## 1. Goal

The hub chat (and any approved agent) can answer questions **grounded in the
estate's own GitHub source** — "where is the ICAP gray-zone judge implemented?",
"what does AIModelRoutePolicy discovery mode do?" — with file/line citations,
via a `code_search` MCP tool. Every hop rides the gateway: GitHub fetch through
a governed SE egress route, embeddings through the SE-fronted models gateway,
tool calls through the MCP route on the a2a-gateway, retrieved content scored
by the same ICAP classifier that guards the front door.

**Corpus** (all github.com/gricec1981):

| Repo | Visibility | Indexed paths |
|---|---|---|
| `load-balancer-and-ingress-services-for-kubernetes` | public | `docs/gateway-api/**`, `ako-gateway-api/**` |
| `ako-inference-demo` | **private** | agents' `*.go`, `*.md` runbooks, `icap-shim/**` |
| `ai-gateway-ui` | **private** | `*.go`, `web/**` (no vendored assets) |

Private repos ⇒ a **fine-grained PAT (contents: read-only, those two repos)**
in Secret `mcp/rag-github-token`. Path allow-lists keep the corpus ~2–5k
chunks — brute-force cosine territory, no vector DB.

## 2. Architecture — `rag-service` (ns `mcp`)

Pure-stdlib Go binary, same mold as `k8s-logs` (the freshest template:
BuildConfig binary-upload, SE-only NetworkPolicy, a2a-gateway MCP route,
mcp-registry entry).

```
GitHub ──(SE egress route: api/codeload.github.com)──▶ indexer
  tarball per repo/ref → allow-listed files → chunks
  → embeddings (nomic-embed via models-gateway, SE-fronted)
  → in-memory vectors + gob cache on PVC (re-embed only changed hashes)

hub / agents ──(https://rag.ai.avi.com/mcp, mcp-gateway VIP .27, jwtQuery auth)──▶ /mcp
  code_search(query, repo?, k) → top-k chunks, each ICAP-scored; poisoned
    chunks quarantined (returned as a flagged stub, text withheld)
  read_file(repo, path, startLine?, endLine?) → exact source for follow-up
```

- **Fetch = GitHub tarball API** (`/repos/{o}/{r}/tarball/{ref}`), not git —
  plain HTTPS + `archive/tar`, no git binary, works through the
  ExternalName-workaround egress pattern ([[ako-externalname-egress]]:
  selectorless Svc + EndpointSlice + HTTPRoute + RouteBackendExtension, as
  built for mcp-web and the Gemini provider tier). Two egress hosts:
  `api.github.com` (redirect) + `codeload.github.com` (bytes).
- **Chunking**: markdown split heading-aware (~400–800 tokens); Go split on
  top-level decls, fixed-line fallback with overlap; ~1500-token cap
  (nomic-embed context is 2048). Chunk metadata: repo, path, line range,
  github URL — returned to the model so answers cite sources.
- **Embeddings**: `POST /v1/embeddings` on the **models gateway**
  (`nomic-embed-inference.models.ai.avi.com`, VIP .26) — already SE-fronted,
  zero policy work. Front-door tier + token metering is a stretch goal, NOT
  v1 (needs `/v1/embeddings` verified through the model-routing DataScript).
- **Search**: cosine over in-memory float32 slices (≤5k × 768 ≈ 15 MB, sub-ms;
  no AVX2 dependency). Index snapshot persisted to PVC; startup re-embeds only
  files whose content hash changed. `POST /reindex` (admin token) + hourly
  timer refresh.
- **Guardrail integration**: each returned chunk scored via the icap-shim's
  existing `GET /score?q=` before leaving the service. Doc/comment text over
  threshold → quarantined. **Code chunks flag-don't-block in v1** — imperative
  code strings ("ignore…", "override…") are classic classifier FP bait.

## 3. Per-service object set

| # | Object | ns | Notes |
|---|---|---|---|
| 1 | BuildConfig + ImageStream `rag-service` | `mcp` | binary-upload flow (k8s-logs pattern) |
| 2 | Secret `rag-github-token` | `mcp` | fine-grained PAT, contents:read |
| 3 | Deployment + Service `rag-service` :8978 | `mcp` | label `ai.ako.vmware.com/agent-factory` NOT set (not a factory agent) |
| 4 | PVC `rag-index` (1 Gi RWO thin-csi) | `mcp` | index cache; emptyDir acceptable v0 |
| 5 | Egress Svc/EndpointSlice/HTTPRoute ×2 | `inference` | `api.github.com`, `codeload.github.com` |
| 6 | HTTPRoute `rag.ai.avi.com` → a2a-gateway | `mcp` | + ReferenceGrant; DNS via AKO |
| 7 | NetworkPolicy `rag-gateway-only` | `mcp` | ingress only from 192.168.68.0/24:8978 (k8s-logs precedent, live-verified) |
| 8 | mcp-registry entry, `approved: true` | `inference` | the allow-list story |
| 9 | agent-hub-registry entry `{type: mcp, url: https://rag.ai.avi.com/mcp}` | `mcp` | one JSON line = chatbot has it |

## 4. Spikes (before build)

- **S1 — egress tarball**: fetch a repo tarball from a pod through the SE
  egress routes, following the api→codeload redirect (Go must re-resolve the
  redirect host against the second egress route; no auto-follow).
- **S2 — hub child-MCP dialing**: the hub's child MCP client dials registry
  URLs with the default transport — `https://rag.ai.avi.com` won't resolve
  from a pod. Reuse the east-west-lockdown VIP-pinning transport
  ([[a2a-eastwest-lockdown]]) for child MCP URLs matching `*.ai.avi.com`.
  (How does agent-runtime already manage it for k8s-logs? Copy that if
  simpler.)
- **S3 — MCP route auth on 31.2.1**: mirror whatever k8s-logs' route enforces
  today (native `AIMCPRoutePolicy` needs the 32.1.1 upgrade); add `?jwt=` if
  an auth policy is attached.

## 5. Build phases

| Phase | Scope | Est |
|---|---|---|
| 0 | Spikes S1–S3 | ½ day |
| 1 | `rag-service` v1: fetch→chunk→embed→cache, `/mcp` (`code_search`, `read_file`), `/healthz`, `/reindex`; deploy objects 1–8 | 1 day |
| 2 | Hub wiring: registry entry + child-MCP VIP pinning; E2E "where is the gray-zone judge?" → cites `guardrail_icap_rest.go` | ½ day |
| 3 | Poisoned-doc beat: injection file in a private repo, quarantine visible in chat trace | ½ day |
| 4 | Stretch: front-door embeddings tier + metering badge; push-webhook reindex; UI Sources panel; factory agents get `code_search` via `agent.json` | — |

## 6. Demo beats

1. Chat: "How does semantic-guardrail gray-zone judging work?" → `code_search`
   trace → answer citing repo/file/line.
2. Registry governance: flip the mcp-registry entry off → tool vanishes from
   the hub's inventory next turn.
3. Egress governance: the ONLY GitHub reachability is the two approved egress
   routes — `curl https://github.com` from the pod fails.
4. Poisoned doc retrieved → chunk quarantined by the front door's own
   classifier → answer still delivered from clean chunks. Segue to the
   response-side SE-native RFE (the SE-native RFE (internal memo, not in this repo)).

## 7. Gotchas carried in from the estate

- ~~Pods can't resolve `ai.avi.com` (S2); clients need VIP pinning.~~ **No longer
  true** — the cluster's CoreDNS forwards `ai.avi.com` to Avi DNS (`.21`), so pods
  resolve `*.ai.avi.com` natively and no `MCP_VIP`/`LLM_VIP` pin is needed.
- Build via internal-registry BuildConfig (Docker Hub anonymous pulls blocked;
  golang base from `mirror.gcr.io`).
- ICAP shim fails open ~2–5 min after pod reschedule (re-run `attach-icap.sh`)
  — don't demo beat 4 in that window.
- GitHub unauthenticated rate limit is 60/hr — always send the PAT; tarball
  fetches are 1 request/repo.
- ~~`jwt-issuer` `/token` returns `{"token":"…"}` JSON, not a bare string.~~
  `GET /token` is **retired (410)**. `rag-service` derives `POST /exchange` from
  `JWT_ISSUER_URL` and presents its ServiceAccount token; the `?sub=&group=`
  still in that env var is vestigial and authorizes nothing.
- The hub's `rag` registry entry belongs in `agent-hub`'s `manifests.yaml`. It
  was once added to the live ConfigMap by hand, and an unrelated apply
  overwrote it — the hub silently lost `code_search`.
- Keep `rag-service` OUT of the factory-agent NetworkPolicy label set; it gets
  its own policy (object 7) so factory-label changes never silently unguard it.
