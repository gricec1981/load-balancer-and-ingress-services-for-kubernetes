/*
 * Copyright © 2025 Broadcom Inc. and/or its subsidiaries. All Rights Reserved.
 * All Rights Reserved.
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*   http://www.apache.org/licenses/LICENSE-2.0
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*/

package aigateway

import (
	"bytes"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/workqueue"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	gatewaylistersv1beta1 "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1beta1"

	akogatewayapilib "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// Cross-namespace Service tiers and backend TLS for AIModelRoutePolicy.
// ─────────────────────────────────────────────────────────────────────
// A Service tier may name a Service in another namespace (backendRef.namespace)
// and may carry backendTLS: a CA bundle the backend's server certificate must
// chain to and a client certificate the SE presents (mTLS). Anything that
// crosses out of the policy's namespace — the Service, or either Secret — is
// honoured only when a Gateway API ReferenceGrant in the target namespace
// allows it, exactly as an HTTPRoute backendRef would be.
//
// This file only RESOLVES (validation, grants, Secret material). The Avi objects
// are node-graph objects built in ako-gateway-api/nodes (avi_model_route.go),
// so they share the node lifecycle: the PKI profile is the pool's PkiProfile
// node, the client certificate and its intermediates are TLSKeyCert nodes on
// the child VS. Secret material is never logged.

// ModelRoutePolicyKind is the kind a ReferenceGrant must name in spec.from.
const ModelRoutePolicyKind = "AIModelRoutePolicy"

// Kinds a ReferenceGrant spec.to must name for the two cross-namespace refs.
const (
	referenceGrantKindService = "Service"
	referenceGrantKindSecret  = "Secret"
)

// referenceGrantLister returns the ReferenceGrant lister, or nil when grants
// are not watched (AI gateway off, or the cluster does not serve them), in
// which case every cross-namespace reference is refused. A var so tests can
// substitute an indexer-backed lister.
var referenceGrantLister = func() gatewaylistersv1beta1.ReferenceGrantLister {
	informers := akogatewayapilib.AKOControlConfig().GatewayApiInformers()
	if informers == nil || informers.ReferenceGrantInformer == nil {
		return nil
	}
	return informers.ReferenceGrantInformer.Lister()
}

// backendTLSSecretGetter reads a Secret from the shared informer cache. A var
// so tests can substitute fixtures.
var backendTLSSecretGetter = func(ns, name string) (*corev1.Secret, error) {
	return utils.GetInformers().SecretInformer.Lister().Secrets(ns).Get(name)
}

// referenceGrantAllows reports whether an AIModelRoutePolicy in fromNs may
// reference the object toKind/toName in toNs. Same-namespace references need
// no grant.
func referenceGrantAllows(fromNs, toNs, toKind, toName string) bool {
	if fromNs == toNs {
		return true
	}
	lister := referenceGrantLister()
	if lister == nil {
		return false
	}
	grants, err := lister.ReferenceGrants(toNs).List(labels.Everything())
	if err != nil {
		return false
	}
	return referenceGrantsPermit(grants, fromNs, toKind, toName)
}

// referenceGrantsPermit is the grant-matching rule, split out for tests: some
// grant must list from {group: ai.ako.vmware.com, kind: AIModelRoutePolicy,
// namespace: fromNs} and to {group: "" (core), kind: toKind, name: toName or
// unset}.
func referenceGrantsPermit(grants []*gatewayv1beta1.ReferenceGrant, fromNs, toKind, toName string) bool {
	for _, g := range grants {
		if g == nil {
			continue
		}
		fromOK := false
		for _, f := range g.Spec.From {
			if string(f.Group) == PolicyGroup && string(f.Kind) == ModelRoutePolicyKind && string(f.Namespace) == fromNs {
				fromOK = true
				break
			}
		}
		if !fromOK {
			continue
		}
		for _, t := range g.Spec.To {
			if string(t.Group) != "" || string(t.Kind) != toKind {
				continue
			}
			if t.Name == nil || *t.Name == "" || string(*t.Name) == toName {
				return true
			}
		}
	}
	return false
}

// ValidateTierBackend checks the per-tier rules OpenAPI cannot express for
// backendRef.namespace and backendTLS. A violation skips that tier only (the
// rest of the policy still applies), so it is not part of Spec.Validate.
func ValidateTierBackend(policyNs string, tier ModelTier) error {
	isService := !tier.IsProvider() && !tier.IsRemote() && tier.BackendRef.Kind == utils.Service
	if tier.BackendTLS != nil {
		switch {
		case tier.IsProvider():
			return fmt.Errorf("backendTLS is not supported on provider tiers (provider.tls governs their transport)")
		case tier.IsRemote():
			return fmt.Errorf("backendTLS is not supported on remote tiers (remote.tls governs their transport)")
		case !isService:
			return fmt.Errorf("backendTLS is supported on Service tiers only, not %q", tier.BackendRef.Kind)
		}
		bt := tier.BackendTLS
		if bt.CASecretRef != nil && bt.CASecretRef.Name == "" {
			return fmt.Errorf("backendTLS.caSecretRef.name is required")
		}
		if bt.ClientCertificateSecretRef != nil && bt.ClientCertificateSecretRef.Name == "" {
			return fmt.Errorf("backendTLS.clientCertificateSecretRef.name is required")
		}
		if bt.HostCheck && bt.SNI == "" {
			return fmt.Errorf("backendTLS.hostCheck requires backendTLS.sni (the name the certificate is checked against)")
		}
	}
	if !tier.IsProvider() && !tier.IsRemote() && tier.BackendRef.Namespace != "" &&
		tier.BackendRef.Namespace != policyNs && !isService {
		return fmt.Errorf("backendRef.namespace is supported on Service tiers only (%s tiers stay in the policy namespace)", tier.BackendRef.Kind)
	}
	return nil
}

// ServiceTierBackendNamespace returns the namespace a Service tier's backend
// lives in, or an error when it is a cross-namespace reference no
// ReferenceGrant allows. It never falls back to the policy namespace.
func ServiceTierBackendNamespace(policy *AIModelRoutePolicy, tier ModelTier) (string, error) {
	ns := tier.BackendRef.EffectiveNamespace(policy.Namespace)
	if ns == policy.Namespace {
		return ns, nil
	}
	if !referenceGrantAllows(policy.Namespace, ns, referenceGrantKindService, tier.BackendRef.Name) {
		return "", fmt.Errorf("no ReferenceGrant in namespace %q allows %s from namespace %q to Service %q",
			ns, ModelRoutePolicyKind, policy.Namespace, tier.BackendRef.Name)
	}
	return ns, nil
}

// ServiceTierBackendRefs returns "ns/name" for every Service tier of the
// policy whose backend reference is honoured (same namespace, or granted).
// HTTPRoute ingestion registers these as backends of the route so Service,
// EndpointSlice, Pod and NodePortLocal events for them re-enqueue it.
func ServiceTierBackendRefs(policy *AIModelRoutePolicy) []string {
	var out []string
	for _, t := range policy.Spec.Tiers {
		if t.IsProvider() || t.IsRemote() || t.BackendRef.Kind != utils.Service || t.BackendRef.Name == "" {
			continue
		}
		ns, err := ServiceTierBackendNamespace(policy, t)
		if err != nil {
			continue
		}
		out = append(out, ns+"/"+t.BackendRef.Name)
	}
	return out
}

// BackendTLSMaterial is a tier's backendTLS resolved against its Secrets.
type BackendTLSMaterial struct {
	// SNI is sent as the pool server_name ("" = the incoming Host).
	SNI string
	// HostCheck enables the pool's certificate host check against SNI.
	HostCheck bool
	// CABundle is every trusted CA certificate as canonical PEM, deduplicated,
	// in order: the caSecretRef bundle, then the intermediates that follow the
	// leaf in the client certificate's tls.crt. "" = no server validation.
	CABundle string
	// ClientCert is the certificate the SE presents; nil = no mTLS.
	ClientCert *ClientCertMaterial
}

// ClientCertMaterial is a resolved kubernetes.io/tls client certificate.
type ClientCertMaterial struct {
	SecretNamespace string
	SecretName      string
	// LeafPEM is the first certificate of tls.crt.
	LeafPEM string
	// KeyPEM is tls.key. Never log it.
	KeyPEM string
	// ChainPEM holds the certificates that follow the leaf in tls.crt, in file
	// order (the leaf's issuer first).
	ChainPEM []string
}

// String keeps the key out of any %v/%s formatting of the material.
func (c *ClientCertMaterial) String() string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf("client-cert{secret=%s/%s, chain=%d}", c.SecretNamespace, c.SecretName, len(c.ChainPEM))
}

// ResolveBackendTLS resolves tier.BackendTLS for a Service tier whose backend
// lives in backendNs. It returns (nil, nil) when the tier has no backendTLS. Any
// error means the tier must be skipped: a backend that expects TLS must never
// be given a plaintext pool.
func ResolveBackendTLS(policy *AIModelRoutePolicy, tier ModelTier, backendNs string) (*BackendTLSMaterial, error) {
	bt := tier.BackendTLS
	if bt == nil {
		return nil, nil
	}
	if err := ValidateTierBackend(policy.Namespace, tier); err != nil {
		return nil, err
	}
	out := &BackendTLSMaterial{SNI: bt.SNI, HostCheck: bt.HostCheck}
	var caCerts [][]byte

	if ref := bt.CASecretRef; ref != nil {
		ns := ref.EffectiveNamespace(backendNs)
		if !referenceGrantAllows(policy.Namespace, ns, referenceGrantKindSecret, ref.Name) {
			return nil, fmt.Errorf("backendTLS.caSecretRef: no ReferenceGrant in namespace %q allows %s from namespace %q to Secret %q",
				ns, ModelRoutePolicyKind, policy.Namespace, ref.Name)
		}
		sec, err := backendTLSSecretGetter(ns, ref.Name)
		if err != nil {
			return nil, fmt.Errorf("backendTLS.caSecretRef: Secret %s/%s: %w", ns, ref.Name, err)
		}
		key := ref.EffectiveKey()
		raw, ok := sec.Data[key]
		if !ok || len(bytes.TrimSpace(raw)) == 0 {
			return nil, fmt.Errorf("backendTLS.caSecretRef: Secret %s/%s has no key %q", ns, ref.Name, key)
		}
		certs := pemCertificates(raw)
		if len(certs) == 0 {
			return nil, fmt.Errorf("backendTLS.caSecretRef: Secret %s/%s key %q holds no PEM certificate", ns, ref.Name, key)
		}
		caCerts = append(caCerts, certs...)
	}

	if ref := bt.ClientCertificateSecretRef; ref != nil {
		ns := ref.EffectiveNamespace(backendNs)
		if !referenceGrantAllows(policy.Namespace, ns, referenceGrantKindSecret, ref.Name) {
			return nil, fmt.Errorf("backendTLS.clientCertificateSecretRef: no ReferenceGrant in namespace %q allows %s from namespace %q to Secret %q",
				ns, ModelRoutePolicyKind, policy.Namespace, ref.Name)
		}
		sec, err := backendTLSSecretGetter(ns, ref.Name)
		if err != nil {
			return nil, fmt.Errorf("backendTLS.clientCertificateSecretRef: Secret %s/%s: %w", ns, ref.Name, err)
		}
		crt, key := sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey]
		if len(bytes.TrimSpace(crt)) == 0 || len(bytes.TrimSpace(key)) == 0 {
			return nil, fmt.Errorf("backendTLS.clientCertificateSecretRef: Secret %s/%s needs both %q and %q",
				ns, ref.Name, corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
		}
		certs := pemCertificates(crt)
		if len(certs) == 0 {
			return nil, fmt.Errorf("backendTLS.clientCertificateSecretRef: Secret %s/%s %q holds no PEM certificate",
				ns, ref.Name, corev1.TLSCertKey)
		}
		// Pairing check only; the error text names the mismatch, never the key.
		if _, err := tls.X509KeyPair(crt, key); err != nil {
			return nil, fmt.Errorf("backendTLS.clientCertificateSecretRef: Secret %s/%s: %q and %q are not a usable key pair: %v",
				ns, ref.Name, corev1.TLSCertKey, corev1.TLSPrivateKeyKey, err)
		}
		cc := &ClientCertMaterial{
			SecretNamespace: ns,
			SecretName:      ref.Name,
			LeafPEM:         string(certs[0]),
			KeyPEM:          strings.TrimSpace(string(key)) + "\n",
		}
		for _, c := range certs[1:] {
			cc.ChainPEM = append(cc.ChainPEM, string(c))
		}
		out.ClientCert = cc
		// A leaf issued by an intermediate must validate: the intermediates the
		// client chain carries are trusted too.
		caCerts = append(caCerts, certs[1:]...)
	}

	out.CABundle = joinUniquePEM(caCerts)
	return out, nil
}

// pemCertificates returns every CERTIFICATE block in data, each re-encoded as
// canonical PEM, in order. Other block types (keys, parameters) are skipped.
func pemCertificates(data []byte) [][]byte {
	var out [][]byte
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return out
		}
		if block.Type != "CERTIFICATE" || len(block.Bytes) == 0 {
			continue
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))
	}
}

// joinUniquePEM concatenates canonical PEM certificates, dropping repeats.
func joinUniquePEM(certs [][]byte) string {
	seen := make(map[string]bool, len(certs))
	var b strings.Builder
	for _, c := range certs {
		if seen[string(c)] {
			continue
		}
		seen[string(c)] = true
		b.Write(c)
	}
	return b.String()
}

// secretRefsOf returns "ns/name" for every Secret a tier's backendTLS names.
func secretRefsOf(policy *AIModelRoutePolicy, tier ModelTier) []string {
	bt := tier.BackendTLS
	if bt == nil {
		return nil
	}
	backendNs := tier.BackendRef.EffectiveNamespace(policy.Namespace)
	var out []string
	if bt.CASecretRef != nil && bt.CASecretRef.Name != "" {
		out = append(out, bt.CASecretRef.EffectiveNamespace(backendNs)+"/"+bt.CASecretRef.Name)
	}
	if bt.ClientCertificateSecretRef != nil && bt.ClientCertificateSecretRef.Name != "" {
		out = append(out, bt.ClientCertificateSecretRef.EffectiveNamespace(backendNs)+"/"+bt.ClientCertificateSecretRef.Name)
	}
	return out
}

// ReferencesBackendTLSSecret reports whether any tier's backendTLS names the
// Secret ns/name.
func (p *AIModelRoutePolicy) ReferencesBackendTLSSecret(ns, name string) bool {
	want := ns + "/" + name
	for _, t := range p.Spec.Tiers {
		for _, ref := range secretRefsOf(p, t) {
			if ref == want {
				return true
			}
		}
	}
	return false
}

// reachesIntoNamespace reports whether the policy references anything (a
// Service backend or a backendTLS Secret) in ns other than its own namespace —
// i.e. whether a ReferenceGrant in ns can change how it translates.
func (p *AIModelRoutePolicy) reachesIntoNamespace(ns string) bool {
	if ns == p.Namespace {
		return false
	}
	for _, t := range p.Spec.Tiers {
		if t.IsProvider() || t.IsRemote() {
			continue
		}
		if t.BackendRef.EffectiveNamespace(p.Namespace) == ns {
			return true
		}
		for _, ref := range secretRefsOf(p, t) {
			if strings.HasPrefix(ref, ns+"/") {
				return true
			}
		}
	}
	return false
}

// HandleBackendTLSSecretEvent re-enqueues the routes of every AIModelRoutePolicy
// whose backendTLS names the Secret ns/name, so a rotated CA bundle or client
// certificate reaches the Avi PKI profile / SSL key-and-certificate objects.
// Called from the existing Secret informer handlers; a no-op when the AI
// gateway is off or nothing references the Secret.
func HandleBackendTLSSecretEvent(ns, name string, wqs []workqueue.RateLimitingInterface, numWorkers uint32) { //nolint:staticcheck
	if !lib.IsAIGatewayEnabled() || len(wqs) == 0 {
		return
	}
	for _, p := range SharedPolicyStore().AllModelRoutePolicies() {
		if !p.ReferencesBackendTLSSecret(ns, name) {
			continue
		}
		utils.AviLog.Infof("AIModelRoutePolicy %s/%s: backendTLS Secret %s/%s changed; re-translating HTTPRoute %s/%s",
			p.Namespace, p.Name, ns, name, p.Namespace, p.Spec.TargetRef.Name)
		enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AIModelRoutePolicy, wqs, numWorkers)
	}
}

// HandleReferenceGrantEvent re-enqueues the routes of every AIModelRoutePolicy
// that references a Service or Secret in ns from another namespace: a grant in
// ns appearing, changing or disappearing can admit or drop those tiers.
func HandleReferenceGrantEvent(ns string, wqs []workqueue.RateLimitingInterface, numWorkers uint32) { //nolint:staticcheck
	if !lib.IsAIGatewayEnabled() || len(wqs) == 0 {
		return
	}
	for _, p := range SharedPolicyStore().AllModelRoutePolicies() {
		if !p.reachesIntoNamespace(ns) {
			continue
		}
		utils.AviLog.Infof("AIModelRoutePolicy %s/%s: ReferenceGrants in namespace %s changed; re-translating HTTPRoute %s/%s",
			p.Namespace, p.Name, ns, p.Namespace, p.Spec.TargetRef.Name)
		enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AIModelRoutePolicy, wqs, numWorkers)
	}
}
