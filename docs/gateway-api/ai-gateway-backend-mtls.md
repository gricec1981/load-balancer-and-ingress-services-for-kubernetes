<!--
  DESIGN DRAFT. Mirrors model-routing.md / ai-gateway-mcp.md / ai-gateway-guardrails.md
  conventions. Backend (south-bound) mTLS between the Avi SE and local inference/MCP
  backends, with SPIFFE/SPIRE short-lived workload identity.
  This EXTENDS the already-implemented RouteBackendExtension.BackendTLS — it is not a new CRD.
  Sections marked ⚠️ are spike-gated and NOT yet verified. NOTHING here is built.
  Drafted 2026-06-08.
-->

# AKO AI Gateway — Backend mTLS with SPIFFE/SPIRE

> **Status: Design draft (Phase 3). Nothing built; spikes not yet run.** This document
> specifies **mutual TLS between the Avi Service Engine and local backends** (inference
> pools, MCP servers) using **SPIFFE/SPIRE short-lived workload identity** — so the SE
> *presents* a rotating client SVID and *validates* the backend's SPIFFE ID, not just a
> hostname. It is a small **extension of the already-implemented**
> [`RouteBackendExtension.BackendTLS`](../../ako-crd-operator/api/v1alpha1/routebackendextension_types.go),
> not a new CRD. It composes with the rest of the AI Gateway ([ai-gateway.md](ai-gateway.md),
> [ai-gateway-mcp.md](ai-gateway-mcp.md), [ai-gateway-guardrails.md](ai-gateway-guardrails.md))
> and is the **south-bound** complement to the **north-bound** OAuth/OIDC consumer identity:
> caller → SE → backend, verified end to end.
>
> **Two things are net-new and one is a genuine open question.** Net-new: a client-cert
> field on `BackendTLS` (the SE presenting a cert) and a SPIRE→Avi rotation controller.
> Open question (⚠️ spike): whether the Avi Pool can **match a URI-type SAN** (`spiffe://…`)
> for backend-identity pinning, or only DNS SAN/CN. Everything downstream of that answer is
> mechanical; the answer itself is the make-or-break.

---

## 1. Overview — the south-bound identity gap

The AI Gateway runs entirely on the Avi data plane (see
the SE-native brief (internal memo, not in this repo)). North-bound, the SE already
verifies **who the caller is** with OAuth/OIDC. South-bound — SE to the model/tool backend —
the connection today is at best **one-way TLS**: the SE can validate the backend's server
cert, but neither side proves a strong, short-lived **workload identity**.

For local inference pools and MCP servers we want:

- **The SE presents a client certificate** (mutual TLS), so backends can refuse anything that
  isn't the gateway.
- **Both certs are SPIFFE SVIDs** issued by **SPIRE**, so identity is a verifiable
  `spiffe://` ID, not a long-lived shared cert.
- **Certs live as briefly as possible** — short SVID TTLs, automatically rotated, so a leaked
  cert is useless within minutes.

This is the load-balancer-native version of a service mesh's mTLS, without putting a mesh
sidecar in the request path on the *gateway* side.

---

## 2. What exists today (the foundation)

AKO already reconciles SE→backend TLS through **`RouteBackendExtension.BackendTLS`** — this is
implemented, not vendored. Today's `BackendTLS`
([routebackendextension_types.go](../../ako-crd-operator/api/v1alpha1/routebackendextension_types.go)):

```go
type BackendTLS struct {
    PKIProfile       *BackendPKIProfile `json:"pkiProfile,omitempty"`       // CA to validate the backend cert
    HostCheckEnabled *bool              `json:"hostCheckEnabled,omitempty"` // turn on hostname verification
    DomainName       []string           `json:"domainName,omitempty"`       // CN / DNS-SAN to match
}
```

Wiring that already works:

- The [`RouteBackendExtension` controller](../../ako-crd-operator/internal/controller/routebackendextension_controller.go)
  validates the referenced `PKIProfile` exists, is tenant-matched, and is `Programmed=True`
  before accepting the RBE.
- The [`PKIProfile` controller](../../ako-crd-operator/internal/controller/pkiprofile_controller.go)
  authors the Avi PKIProfile (CA bundle) over REST.
- When the RBE is attached to an HTTPRoute `backendRef`, `BackendTLS` lands on the Avi **Pool**
  (`ssl_profile_ref` for SE→server TLS, `pki_profile_ref` for validation).

So **one-way TLS with server validation is a solved, shipping path.** What it cannot do:
make the SE *present* a cert (no mTLS), and pin a **SPIFFE ID** (`domainName` is DNS-only).

> Note: the upstream Gateway API **`BackendTLSPolicy`** (`v1alpha3`) is vendored here but
> **not implemented** by AKO (no CRD shipped, no controller). It has a `subjectAltNames[].type:
> URI` field whose spec comment literally cites a `spiffe://…` ID — useful as **precedent that
> URI-SAN matching is a standardised concept** — but it is still validation-only (no gateway
> client cert), so it does not get us to mTLS either. We extend the live RBE surface instead of
> chasing an unimplemented CRD.

---

## 3. The two gaps, precisely

| # | Gap | Why `BackendTLS` can't express it today | Fix |
|---|---|---|---|
| 1 | **SE presents a client cert (mTLS)** | `BackendTLS` has `pkiProfile`/`hostCheckEnabled`/`domainName` — **no client-cert field** | Add `seClientCert` → Avi Pool `ssl_key_and_certificate_ref` (§4) |
| 2 | **Pin backend SPIFFE ID** | `domainName` matches CN / **DNS** SAN; SPIFFE identity is a **URI** SAN (`spiffe://…`) | Add `subjectAltNames` (URI type) — **⚠️ gated on Avi pool support (§7)** |

The Avi `Pool` model already carries `ssl_key_and_certificate_ref`, so the *data plane* can do
mTLS — only the CRD field and the controller wiring are missing for gap 1. Gap 2 depends on a
data-plane capability we have not verified.

---

## 4. CRD change — `seClientCert` on `BackendTLS`

Minimal, consistent with the existing `BackendPKIProfile` / `BackendHealthMonitor` shapes:

```go
// BackendClientCert references the certificate+key the SE PRESENTS to the backend
// (the client side of mTLS). Kind=AVIREF points at an Avi SSLKeyAndCertificate that the
// SPIFFE rotation controller (§6) keeps current; Kind=CRD is reserved for a future
// Secret-backed source.
type BackendClientCert struct {
    // +kubebuilder:validation:Enum=AVIREF
    // +required
    Kind ObjectKind `json:"kind,omitempty"`
    // Name of the Avi SSLKeyAndCertificate holding the SE's client SVID.
    // +required
    Name string `json:"name,omitempty"`
}

type BackendTLS struct {
    PKIProfile       *BackendPKIProfile `json:"pkiProfile,omitempty"`
    HostCheckEnabled *bool              `json:"hostCheckEnabled,omitempty"`
    DomainName       []string           `json:"domainName,omitempty"`

    // SeClientCert turns one-way TLS into mTLS: the SE presents this cert to the backend.
    // +optional
    SeClientCert *BackendClientCert `json:"seClientCert,omitempty"`

    // SubjectAltNames pins the backend's SPIFFE ID (URI SAN, e.g. spiffe://td/ns/…/sa/…),
    // beyond DNS-oriented domainName. ⚠️ Honored only if the Avi pool supports URI-SAN
    // matching (§7) — until that spike passes, this field is parsed but may no-op.
    // +optional
    SubjectAltNames []string `json:"subjectAltNames,omitempty"`
}
```

Implementation steps once the §7 spike passes:

1. Add the struct + fields above to
   [`routebackendextension_types.go`](../../ako-crd-operator/api/v1alpha1/routebackendextension_types.go),
   regenerate `zz_generated.deepcopy.go`, and re-render the CRD YAML.
2. Controller: validate the referenced `SSLKeyAndCertificate` exists (mirrors the existing
   `PKIProfile` readiness check in the RBE controller).
3. Translator: set Pool `ssl_key_and_certificate_ref` from `seClientCert`; emit a URI-SAN
   server-cert check from `subjectAltNames`.

---

## 5. End-to-end architecture

```
  SPIRE server ──issues SVIDs──┬───────────────────────────┐
        │ trust bundle         │ (Workload API)             │ (Workload API)
        ▼                      ▼                            ▼
  ┌───────────────┐    ┌──────────────────┐      ┌────────────────────────┐
  │ AKO SPIRE     │    │ AKO CRD operator │      │ backend pods            │
  │ rotation ctlr │    │ (RBE / PKIProfile│      │ (vLLM / MCP)            │
  │ = SPIFFE wl   │    │  controllers)    │      │  + SPIFFE CSI driver    │
  └──────┬────────┘    └────────┬─────────┘      │    OR ghostunnel/Envoy  │
         │ REST                 │ REST            │    sidecar (mTLS term)  │
         ▼                      ▼                 └───────────┬────────────┘
   Avi SSLKeyAndCert      Avi PKIProfile                      │ presents backend SVID,
   (SE client SVID,       (SPIRE trust bundle)                │ validates SE client SVID
    short TTL, rotated)         │                             │
         └──────────┬──────────┘                             │
                    ▼               mTLS (both SVIDs)         ▼
                 Avi SE  ─────────────────────────────────► backend
```

Roles:

- **Backends** get short-lived SVIDs via the **SPIFFE CSI driver** (mounts SVID + bundle) or a
  **ghostunnel/Envoy sidecar** in server mode that terminates SE→pod mTLS and forwards
  plaintext over localhost. The latter needs **zero app change** to vLLM/MCP servers, which
  won't do SPIFFE natively.
- **SE** is **not** a SPIFFE workload (no SPIRE agent attests it), so it cannot call the
  Workload API itself. A controller bridges it (§6).

---

## 6. The SPIRE→Avi rotation controller

A small controller — naturally an extension of AKO, which already authors Avi objects over
REST exactly this way (see the OAuth object graph and
[`guardrail_rest.go`](../../ako-gateway-api/aigateway/guardrail_rest.go)) — runs **as a SPIFFE
workload** and:

1. Obtains the **SE's client X509-SVID** for one registered SPIFFE ID
   (e.g. `spiffe://<trust-domain>/avi/ai-gateway`) from the SPIRE Workload API.
2. Writes/updates the Avi **`SSLKeyAndCertificate`** that `seClientCert` points at.
3. Obtains the **trust bundle** and keeps the **`PKIProfile`** CA current (the existing
   PKIProfile path; CA can also be a ConfigMap `ca.crt` refreshed by `spiffe-helper`).
4. **Re-pushes at ~half the SVID lifetime.**

**Hitless rotation:** prefer **blue/green** — create a new `SSLKeyAndCertificate`, swap the
Pool's `ssl_key_and_certificate_ref`, then GC the old object — over mutating in place, so live
connections don't flap. ⚠️ Confirm the SE applies a pool client-cert swap without dropping
established upstream connections (§7).

---

## 7. Spikes (must pass before claiming this)

| # | Spike | Question | Make-or-break? |
|---|---|---|---|
| ⚠️ 1 | **URI-SAN matching** | Can the Avi Pool validate a **URI-type SAN** (`spiffe://…`), or only DNS SAN/CN? | **Yes** — decides whether we pin SPIFFE ID or only trust at CA level |
| ⚠️ 2 | **Pool client cert** | Does setting Pool `ssl_key_and_certificate_ref` make the SE present that cert and complete mTLS to a backend that requires it? | Yes |
| ⚠️ 3 | **Hitless rotation** | Does a blue/green swap of the pool client cert rotate without flapping established connections? How short can TTL go before churn hurts? | Tunes the floor TTL |
| ⚠️ 4 | **CA bundle refresh** | Does updating the PKIProfile CA (rotated SPIRE bundle) take effect without a VS bounce? | Operational |

**Fallback if Spike 1 fails:** trust at the **CA / trust-domain level** — i.e. "any cert from
*my* SPIRE" — using a dedicated trust domain or intermediate CA per backend boundary to keep
the blast radius small, and file an RFE for native URI-SAN verification. mTLS (gaps 1) still
ships; only exact SPIFFE-ID pinning (gap 2) waits.

---

## 8. How short can the certs be?

The TTL is set on the SPIRE registration entry (`x509SVIDTTL`); SPIRE defaults to ~1h, floor a
few minutes. The binding constraint is **not** SPIRE — it's the push-to-Avi cadence and how
cheaply the SE reloads:

- **Backend leaf SVIDs:** as short as SPIRE/CSI rotates reliably (minutes) — fully automatic,
  no Avi involvement.
- **SE client SVID:** bounded by the rotation controller (§6). **1h SVID rotated at 30 min**
  is comfortable (~48 pushes/day). Sub-15-min is possible but hammers cert churn — prove the
  hitless swap (Spike 3) first, then tighten.
- **Trust bundle (CA):** rotates far less often than leaves; lowest-churn object.

---

## 9. The honest ceiling

- **No native SPIRE↔Avi integration.** The rotation controller is hand-built — the same
  "repurposed primitive" pattern as the ICAP/DataScript workarounds in
  the SE-native brief (internal memo, not in this repo). The first-class end state is
  a **native SPIFFE/SVID cert source** on Avi (analogous to a cert-manager integration) so the
  SE pulls its own identity instead of being fed one. → RFE.
- **The SE isn't a SPIFFE workload**, so its identity is controller-issued, not node-attested.
- **URI-SAN pinning is unproven** on Avi (Spike 1) — until then, CA-level trust.
- **North-south ↔ south-bound binding** (mapping the verified OIDC caller onto the south-bound
  mTLS identity for true end-to-end attribution) is a later step, not in this draft.

---

## 10. Implementation plan

1. **Spike 1 + 2** on the demo Avi (URI-SAN match; pool client cert completes mTLS).
2. Add `seClientCert` (+ `subjectAltNames`) to `RouteBackendExtension.BackendTLS`; regenerate
   deepcopy + CRD YAML; wire the translator to Pool `ssl_key_and_certificate_ref`.
3. Build the **SPIRE→Avi rotation controller** (blue/green `SSLKeyAndCertificate`,
   half-life refresh).
4. Backend side: SPIFFE CSI driver or ghostunnel sidecar on the inference/MCP pods.
5. **Spike 3 + 4** (hitless rotation, CA refresh); tighten TTLs.
6. RFE: native SPIFFE cert source + URI-SAN verification on the SE.

---

## 11. Related docs

- [AI Gateway](ai-gateway.md) — the policy family this joins; north-bound OAuth/OIDC identity
- [MCP Gateway](ai-gateway-mcp.md) — MCP backends this secures south-bound
- [Guardrails & DLP](ai-gateway-guardrails.md) — content inspection on the same SE
- SE-Native Brief (internal memo, not in this repo) — the "move workarounds native" thesis (RFE home)
- [Overview](ai-gateway-overview.md) — where this fits in the gateway
