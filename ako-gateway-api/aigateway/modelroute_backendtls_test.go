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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	gatewaylistersv1beta1 "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1beta1"
)

// ─── fixtures ────────────────────────────────────────────────────────────────

// testPKI is a root CA → intermediate CA → leaf chain, the shape of the PAIS
// mTLS Secret (ca.crt = root, tls.crt = leaf issued by the intermediate).
type testPKI struct {
	rootPEM, intPEM, leafPEM, leafKeyPEM, otherKeyPEM []byte
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	mk := func(cn string, isCA bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
		tmpl := &x509.Certificate{
			SerialNumber:          serial,
			Subject:               pkix.Name{CommonName: cn},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			IsCA:                  isCA,
			BasicConstraintsValid: true,
		}
		if isCA {
			tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature
		} else {
			tmpl.KeyUsage = x509.KeyUsageDigitalSignature
		}
		signer, signerKey := tmpl, key
		if parent != nil {
			signer, signerKey = parent, parentKey
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(der)
		return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	keyPEM := func(k *ecdsa.PrivateKey) []byte {
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	}
	root, rootKey, rootPEM := mk("pais-root-ca", true, nil, nil)
	inter, interKey, intPEM := mk("pais-mtls-ca", true, root, rootKey)
	_, leafKey, leafPEM := mk("pais-client", false, inter, interKey)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return testPKI{rootPEM: rootPEM, intPEM: intPEM, leafPEM: leafPEM, leafKeyPEM: keyPEM(leafKey), otherKeyPEM: keyPEM(other)}
}

// withSecrets substitutes the Secret getter for the test.
func withSecrets(t *testing.T, secrets ...*corev1.Secret) {
	t.Helper()
	prev := backendTLSSecretGetter
	t.Cleanup(func() { backendTLSSecretGetter = prev })
	backendTLSSecretGetter = func(ns, name string) (*corev1.Secret, error) {
		for _, s := range secrets {
			if s.Namespace == ns && s.Name == name {
				return s, nil
			}
		}
		return nil, k8serrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
}

// withGrants substitutes an indexer-backed ReferenceGrant lister for the test.
// nil grants (no call) leaves the default; withGrants(t) = watched but empty.
func withGrants(t *testing.T, grants ...*gatewayv1beta1.ReferenceGrant) {
	t.Helper()
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, g := range grants {
		if err := idx.Add(g); err != nil {
			t.Fatal(err)
		}
	}
	prev := referenceGrantLister
	t.Cleanup(func() { referenceGrantLister = prev })
	referenceGrantLister = func() gatewaylistersv1beta1.ReferenceGrantLister {
		return gatewaylistersv1beta1.NewReferenceGrantLister(idx)
	}
}

func grant(ns, fromNs, toKind, toName string) *gatewayv1beta1.ReferenceGrant {
	g := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("allow-%s-%s", strings.ToLower(toKind), fromNs)},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{Group: PolicyGroup, Kind: ModelRoutePolicyKind, Namespace: gatewayv1beta1.Namespace(fromNs)}},
			To:   []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: gatewayv1beta1.Kind(toKind)}},
		},
	}
	if toName != "" {
		n := gatewayv1beta1.ObjectName(toName)
		g.Spec.To[0].Name = &n
	}
	return g
}

func paisSecret(p testPKI, key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pais", Name: "pais-mtls-uid"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"ca.crt":                p.rootPEM,
			corev1.TLSCertKey:       append(append([]byte{}, p.leafPEM...), p.intPEM...),
			corev1.TLSPrivateKeyKey: key,
		},
	}
}

// paisPolicy is the live shape: policy in "gateway", Service + mTLS Secret in "pais".
func paisPolicy() *AIModelRoutePolicy {
	p := &AIModelRoutePolicy{}
	p.Namespace, p.Name = "gateway", "llm-tiers"
	p.Spec.TargetRef.Name = "llm-route"
	p.Spec.DefaultTier = "pais-cpu"
	p.Spec.Tiers = []ModelTier{{
		Name:       "pais-cpu",
		BackendRef: ModelBackendRef{Kind: "Service", Name: "pais-llm", Namespace: "pais"},
		BackendTLS: &ModelBackendTLS{
			SNI:                        "pais-llm.pais.svc",
			CASecretRef:                &BackendTLSCASecretRef{Name: "pais-mtls-uid"},
			ClientCertificateSecretRef: &BackendTLSSecretRef{Name: "pais-mtls-uid"},
		},
	}}
	return p
}

// ─── parsing ─────────────────────────────────────────────────────────────────

func TestParseBackendRefNamespaceAndBackendTLS(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "llm-tiers", "namespace": "gateway"},
		"spec": map[string]interface{}{
			"targetRef":   map[string]interface{}{"kind": "HTTPRoute", "name": "llm-route"},
			"defaultTier": "pais-cpu",
			"tiers": []interface{}{
				map[string]interface{}{
					"name":       "pais-cpu",
					"backendRef": map[string]interface{}{"kind": "Service", "name": "pais-llm", "namespace": "pais"},
					"backendTLS": map[string]interface{}{
						"sni":       "pais-llm.pais.svc",
						"hostCheck": true,
						"caSecretRef": map[string]interface{}{
							"name": "pais-mtls-uid", "namespace": "pais", "key": "root.pem",
						},
						"clientCertificateSecretRef": map[string]interface{}{"name": "pais-mtls-uid"},
					},
				},
				map[string]interface{}{
					"name":       "plain",
					"backendRef": map[string]interface{}{"kind": "Service", "name": "vllm"},
				},
			},
		},
	}}
	p, err := unstructuredToModelRoutePolicy(u)
	if err != nil {
		t.Fatal(err)
	}
	tls0 := p.Spec.Tiers[0]
	if tls0.BackendRef.Namespace != "pais" || tls0.BackendRef.EffectiveNamespace(p.Namespace) != "pais" {
		t.Fatalf("backendRef.namespace = %q", tls0.BackendRef.Namespace)
	}
	bt := tls0.BackendTLS
	if bt == nil || bt.SNI != "pais-llm.pais.svc" || !bt.HostCheck {
		t.Fatalf("backendTLS = %+v", bt)
	}
	if bt.CASecretRef == nil || bt.CASecretRef.Name != "pais-mtls-uid" || bt.CASecretRef.EffectiveKey() != "root.pem" ||
		bt.CASecretRef.EffectiveNamespace("x") != "pais" {
		t.Fatalf("caSecretRef = %+v", bt.CASecretRef)
	}
	if bt.ClientCertificateSecretRef == nil || bt.ClientCertificateSecretRef.EffectiveNamespace("pais") != "pais" {
		t.Fatalf("clientCertificateSecretRef = %+v", bt.ClientCertificateSecretRef)
	}
	plain := p.Spec.Tiers[1]
	if plain.BackendTLS != nil || plain.BackendRef.EffectiveNamespace(p.Namespace) != "gateway" {
		t.Fatalf("plain tier = %+v", plain)
	}
	if (&BackendTLSCASecretRef{Name: "x"}).EffectiveKey() != "ca.crt" {
		t.Fatal("caSecretRef.key must default to ca.crt")
	}
}

// ─── per-tier validation ─────────────────────────────────────────────────────

func TestValidateTierBackend(t *testing.T) {
	svcTLS := &ModelBackendTLS{CASecretRef: &BackendTLSCASecretRef{Name: "ca"}}
	cases := map[string]struct {
		tier    ModelTier
		wantErr string
	}{
		"service + backendTLS ok": {ModelTier{Name: "a", BackendRef: ModelBackendRef{Kind: "Service", Name: "s", Namespace: "pais"}, BackendTLS: svcTLS}, ""},
		"service cross-ns ok":     {ModelTier{Name: "a", BackendRef: ModelBackendRef{Kind: "Service", Name: "s", Namespace: "pais"}}, ""},
		"inferencepool same-ns":   {ModelTier{Name: "a", BackendRef: ModelBackendRef{Kind: "InferencePool", Name: "p", Namespace: "gateway"}}, ""},
		"inferencepool cross-ns": {ModelTier{Name: "a", BackendRef: ModelBackendRef{Kind: "InferencePool", Name: "p", Namespace: "pais"}},
			"Service tiers only"},
		"inferencepool + backendTLS": {ModelTier{Name: "a", BackendRef: ModelBackendRef{Kind: "InferencePool", Name: "p"}, BackendTLS: svcTLS},
			"Service tiers only"},
		"provider + backendTLS": {ModelTier{Name: "a", Provider: &ModelProvider{Host: "h", Path: "/p"}, BackendTLS: svcTLS},
			"provider tiers"},
		"remote + backendTLS": {ModelTier{Name: "a", Remote: &ModelRemote{Host: "h"}, BackendTLS: svcTLS},
			"remote tiers"},
		"empty ca name": {ModelTier{Name: "a", BackendRef: ModelBackendRef{Kind: "Service", Name: "s"},
			BackendTLS: &ModelBackendTLS{CASecretRef: &BackendTLSCASecretRef{}}}, "caSecretRef.name"},
		"empty client name": {ModelTier{Name: "a", BackendRef: ModelBackendRef{Kind: "Service", Name: "s"},
			BackendTLS: &ModelBackendTLS{ClientCertificateSecretRef: &BackendTLSSecretRef{}}}, "clientCertificateSecretRef.name"},
		"hostCheck without sni": {ModelTier{Name: "a", BackendRef: ModelBackendRef{Kind: "Service", Name: "s"},
			BackendTLS: &ModelBackendTLS{HostCheck: true}}, "requires backendTLS.sni"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateTierBackend("gateway", c.tier)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

// ─── ReferenceGrant ──────────────────────────────────────────────────────────

func TestReferenceGrantsPermit(t *testing.T) {
	wrongGroup := grant("pais", "gateway", "Service", "")
	wrongGroup.Spec.From[0].Group = "gateway.networking.k8s.io"
	wrongKind := grant("pais", "gateway", "Service", "")
	wrongKind.Spec.From[0].Kind = "HTTPRoute"
	coreGroupTo := grant("pais", "gateway", "Service", "")
	coreGroupTo.Spec.To[0].Group = "apps"

	cases := map[string]struct {
		grants       []*gatewayv1beta1.ReferenceGrant
		kind, toName string
		want         bool
	}{
		"service any name":         {[]*gatewayv1beta1.ReferenceGrant{grant("pais", "gateway", "Service", "")}, "Service", "pais-llm", true},
		"service named match":      {[]*gatewayv1beta1.ReferenceGrant{grant("pais", "gateway", "Service", "pais-llm")}, "Service", "pais-llm", true},
		"service named mismatch":   {[]*gatewayv1beta1.ReferenceGrant{grant("pais", "gateway", "Service", "other")}, "Service", "pais-llm", false},
		"service grant for secret": {[]*gatewayv1beta1.ReferenceGrant{grant("pais", "gateway", "Secret", "")}, "Service", "pais-llm", false},
		"secret granted":           {[]*gatewayv1beta1.ReferenceGrant{grant("pais", "gateway", "Secret", "pais-mtls-uid")}, "Secret", "pais-mtls-uid", true},
		"secret grant for service": {[]*gatewayv1beta1.ReferenceGrant{grant("pais", "gateway", "Service", "")}, "Secret", "pais-mtls-uid", false},
		"other from namespace":     {[]*gatewayv1beta1.ReferenceGrant{grant("pais", "tenant-b", "Service", "")}, "Service", "pais-llm", false},
		"wrong from group":         {[]*gatewayv1beta1.ReferenceGrant{wrongGroup}, "Service", "pais-llm", false},
		"wrong from kind":          {[]*gatewayv1beta1.ReferenceGrant{wrongKind}, "Service", "pais-llm", false},
		"non-core to group":        {[]*gatewayv1beta1.ReferenceGrant{coreGroupTo}, "Service", "pais-llm", false},
		"no grants":                {nil, "Service", "pais-llm", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := referenceGrantsPermit(c.grants, "gateway", c.kind, c.toName); got != c.want {
				t.Fatalf("permit = %v, want %v", got, c.want)
			}
		})
	}
}

func TestServiceTierBackendNamespaceNeedsGrant(t *testing.T) {
	p := paisPolicy()
	tier := p.Spec.Tiers[0]

	// Grants not watched at all (nil lister): refused.
	prev := referenceGrantLister
	referenceGrantLister = func() gatewaylistersv1beta1.ReferenceGrantLister { return nil }
	if _, err := ServiceTierBackendNamespace(p, tier); err == nil {
		t.Fatal("cross-namespace Service admitted with ReferenceGrants unwatched")
	}
	referenceGrantLister = prev

	// Watched, but the only grant is for Secrets: refused, and no fallback to the
	// policy namespace.
	withGrants(t, grant("pais", "gateway", "Secret", ""))
	ns, err := ServiceTierBackendNamespace(p, tier)
	if err == nil || ns != "" {
		t.Fatalf("got (%q, %v), want refusal with no namespace", ns, err)
	}
	if !strings.Contains(err.Error(), "ReferenceGrant") {
		t.Fatalf("error should name the missing ReferenceGrant: %v", err)
	}
	if refs := ServiceTierBackendRefs(p); len(refs) != 0 {
		t.Fatalf("ungranted tier registered as route backend: %v", refs)
	}

	// Granted.
	withGrants(t, grant("pais", "gateway", "Service", "pais-llm"))
	if ns, err := ServiceTierBackendNamespace(p, tier); err != nil || ns != "pais" {
		t.Fatalf("got (%q, %v), want (pais, nil)", ns, err)
	}
	if refs := ServiceTierBackendRefs(p); len(refs) != 1 || refs[0] != "pais/pais-llm" {
		t.Fatalf("route backend refs = %v, want [pais/pais-llm]", refs)
	}

	// Same namespace needs no grant.
	withGrants(t)
	tier.BackendRef.Namespace = ""
	if ns, err := ServiceTierBackendNamespace(p, tier); err != nil || ns != "gateway" {
		t.Fatalf("same-namespace: got (%q, %v)", ns, err)
	}
}

// ─── backendTLS resolution ───────────────────────────────────────────────────

func pemCount(s string) int { return strings.Count(s, "-----BEGIN CERTIFICATE-----") }

func TestResolveBackendTLSBundlesRootAndIntermediate(t *testing.T) {
	pki := newTestPKI(t)
	withSecrets(t, paisSecret(pki, pki.leafKeyPEM))
	withGrants(t, grant("pais", "gateway", "Service", ""), grant("pais", "gateway", "Secret", "pais-mtls-uid"))
	p := paisPolicy()

	m, err := ResolveBackendTLS(p, p.Spec.Tiers[0], "pais")
	if err != nil {
		t.Fatal(err)
	}
	if m.SNI != "pais-llm.pais.svc" || m.HostCheck {
		t.Fatalf("sni/hostCheck = %q/%v", m.SNI, m.HostCheck)
	}
	// Root (ca.crt) + intermediate (tls.crt after the leaf), leaf NOT trusted.
	if pemCount(m.CABundle) != 2 {
		t.Fatalf("CA bundle holds %d certificates, want 2 (root + intermediate)", pemCount(m.CABundle))
	}
	if !strings.HasPrefix(m.CABundle, string(pki.rootPEM)) || !strings.HasSuffix(m.CABundle, string(pki.intPEM)) {
		t.Fatal("CA bundle must be root then intermediate")
	}
	if strings.Contains(m.CABundle, string(pki.leafPEM)) {
		t.Fatal("the client leaf must not be a trusted CA")
	}
	// The bundle validates the leaf the way the SE's PKI profile will.
	roots, inters := x509.NewCertPool(), x509.NewCertPool()
	roots.AppendCertsFromPEM(pki.rootPEM)
	inters.AppendCertsFromPEM([]byte(m.CABundle))
	block, _ := pem.Decode([]byte(m.ClientCert.LeafPEM))
	leaf, _ := x509.ParseCertificate(block.Bytes)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inters, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatalf("leaf does not chain through the bundle: %v", err)
	}

	cc := m.ClientCert
	if cc == nil || cc.SecretNamespace != "pais" || cc.SecretName != "pais-mtls-uid" {
		t.Fatalf("client cert = %v", cc)
	}
	if cc.LeafPEM != string(pki.leafPEM) || len(cc.ChainPEM) != 1 || cc.ChainPEM[0] != string(pki.intPEM) {
		t.Fatal("tls.crt must split into leaf + [intermediate]")
	}
	if !strings.Contains(cc.KeyPEM, "PRIVATE KEY") {
		t.Fatal("key material missing")
	}
	// Formatting the material never prints the key.
	for _, s := range []string{cc.String(), fmt.Sprintf("%v", cc), fmt.Sprintf("%s", cc)} {
		if strings.Contains(s, "PRIVATE KEY") || strings.Contains(s, cc.KeyPEM) {
			t.Fatalf("client cert formatting leaks the key: %s", s)
		}
	}

	// Repeated resolution is byte-identical (stable checksums, no Avi churn).
	m2, _ := ResolveBackendTLS(p, p.Spec.Tiers[0], "pais")
	if m2.CABundle != m.CABundle || m2.ClientCert.LeafPEM != cc.LeafPEM {
		t.Fatal("resolution is not deterministic")
	}
}

func TestResolveBackendTLSRefusals(t *testing.T) {
	pki := newTestPKI(t)
	p := paisPolicy()
	tier := p.Spec.Tiers[0]
	allGrants := []*gatewayv1beta1.ReferenceGrant{grant("pais", "gateway", "Service", ""), grant("pais", "gateway", "Secret", "")}

	t.Run("secret not granted", func(t *testing.T) {
		withSecrets(t, paisSecret(pki, pki.leafKeyPEM))
		withGrants(t, grant("pais", "gateway", "Service", "")) // Service only
		if _, err := ResolveBackendTLS(p, tier, "pais"); err == nil || !strings.Contains(err.Error(), "to Secret") {
			t.Fatalf("err = %v, want Secret ReferenceGrant refusal", err)
		}
	})
	t.Run("secret granted for another name", func(t *testing.T) {
		withSecrets(t, paisSecret(pki, pki.leafKeyPEM))
		withGrants(t, grant("pais", "gateway", "Secret", "something-else"))
		if _, err := ResolveBackendTLS(p, tier, "pais"); err == nil {
			t.Fatal("name-scoped Secret grant admitted a different Secret")
		}
	})
	t.Run("missing secret", func(t *testing.T) {
		withSecrets(t)
		withGrants(t, allGrants...)
		if _, err := ResolveBackendTLS(p, tier, "pais"); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing ca key", func(t *testing.T) {
		s := paisSecret(pki, pki.leafKeyPEM)
		delete(s.Data, "ca.crt")
		withSecrets(t, s)
		withGrants(t, allGrants...)
		if _, err := ResolveBackendTLS(p, tier, "pais"); err == nil || !strings.Contains(err.Error(), `no key "ca.crt"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing tls.key", func(t *testing.T) {
		s := paisSecret(pki, nil)
		withSecrets(t, s)
		withGrants(t, allGrants...)
		if _, err := ResolveBackendTLS(p, tier, "pais"); err == nil || !strings.Contains(err.Error(), "tls.key") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("mismatched key", func(t *testing.T) {
		withSecrets(t, paisSecret(pki, pki.otherKeyPEM))
		withGrants(t, allGrants...)
		_, err := ResolveBackendTLS(p, tier, "pais")
		if err == nil || !strings.Contains(err.Error(), "key pair") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Fatal("error leaks key material")
		}
	})
	t.Run("no certificate in ca.crt", func(t *testing.T) {
		s := paisSecret(pki, pki.leafKeyPEM)
		s.Data["ca.crt"] = []byte("not pem")
		withSecrets(t, s)
		withGrants(t, allGrants...)
		if _, err := ResolveBackendTLS(p, tier, "pais"); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("backendTLS on provider", func(t *testing.T) {
		pt := ModelTier{Name: "g", Provider: &ModelProvider{Host: "h", Path: "/p"}, BackendTLS: tier.BackendTLS}
		if _, err := ResolveBackendTLS(p, pt, "gateway"); err == nil {
			t.Fatal("backendTLS on a provider tier resolved")
		}
	})
	t.Run("no backendTLS", func(t *testing.T) {
		plain := ModelTier{Name: "plain", BackendRef: ModelBackendRef{Kind: "Service", Name: "s"}}
		if m, err := ResolveBackendTLS(p, plain, "gateway"); m != nil || err != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", m, err)
		}
	})
	t.Run("same-namespace secrets need no grant", func(t *testing.T) {
		s := paisSecret(pki, pki.leafKeyPEM)
		s.Namespace = "gateway"
		withSecrets(t, s)
		withGrants(t)
		same := ModelTier{Name: "t", BackendRef: ModelBackendRef{Kind: "Service", Name: "s"}, BackendTLS: tier.BackendTLS}
		if _, err := ResolveBackendTLS(p, same, "gateway"); err != nil {
			t.Fatal(err)
		}
	})
}

// ─── rotation / grant re-enqueue ─────────────────────────────────────────────

func TestBackendTLSSecretEventReenqueuesReferencingRoutes(t *testing.T) {
	t.Setenv("AI_GATEWAY_ENABLED", "true")
	p := paisPolicy()
	p.Namespace = "backendtls-rotation"
	unrelated := &AIModelRoutePolicy{}
	unrelated.Namespace, unrelated.Name = "backendtls-rotation", "other"
	unrelated.Spec.TargetRef.Name = "other-route"
	unrelated.Spec.Tiers = []ModelTier{{Name: "a", BackendRef: ModelBackendRef{Kind: "Service", Name: "vllm"}}}
	store := SharedPolicyStore()
	store.upsertModelRoutePolicy(p)
	store.upsertModelRoutePolicy(unrelated)
	t.Cleanup(func() {
		store.deleteModelRoutePolicy(p.Namespace, p.Name)
		store.deleteModelRoutePolicy(unrelated.Namespace, unrelated.Name)
	})

	q := &recordingQueue{}
	wqs := []workqueue.RateLimitingInterface{q} //nolint:staticcheck

	HandleBackendTLSSecretEvent("pais", "pais-mtls-uid", wqs, 1)
	if got := q.take(); len(got) != 1 || got[0] != "HTTPRoute/backendtls-rotation/llm-route" {
		t.Fatalf("rotation enqueued %v, want [HTTPRoute/backendtls-rotation/llm-route]", got)
	}
	HandleBackendTLSSecretEvent("pais", "some-other-secret", wqs, 1)
	HandleBackendTLSSecretEvent("backendtls-rotation", "pais-mtls-uid", wqs, 1) // same name, wrong namespace
	if got := q.take(); len(got) != 0 {
		t.Fatalf("unrelated Secret enqueued %v", got)
	}

	// A grant change in the backend namespace re-translates the policy reaching
	// into it; one elsewhere does not.
	HandleReferenceGrantEvent("pais", wqs, 1)
	if got := q.take(); len(got) != 1 || got[0] != "HTTPRoute/backendtls-rotation/llm-route" {
		t.Fatalf("grant change enqueued %v", got)
	}
	HandleReferenceGrantEvent("kube-system", wqs, 1)
	HandleReferenceGrantEvent("backendtls-rotation", wqs, 1) // own namespace: grants irrelevant
	if got := q.take(); len(got) != 0 {
		t.Fatalf("irrelevant grant change enqueued %v", got)
	}

	// AI gateway off: no-op.
	t.Setenv("AI_GATEWAY_ENABLED", "false")
	HandleBackendTLSSecretEvent("pais", "pais-mtls-uid", wqs, 1)
	if got := q.take(); len(got) != 0 {
		t.Fatalf("AI gateway disabled but enqueued %v", got)
	}
}

func TestReferencesBackendTLSSecretDefaultsToBackendNamespace(t *testing.T) {
	p := paisPolicy()
	if !p.ReferencesBackendTLSSecret("pais", "pais-mtls-uid") {
		t.Fatal("Secret refs without namespace must default to the backend namespace (pais)")
	}
	if p.ReferencesBackendTLSSecret("gateway", "pais-mtls-uid") {
		t.Fatal("must not match the policy namespace when the backend is elsewhere")
	}
	p.Spec.Tiers[0].BackendTLS.CASecretRef.Namespace = "trust"
	if !p.ReferencesBackendTLSSecret("trust", "pais-mtls-uid") || !p.reachesIntoNamespace("trust") {
		t.Fatal("explicit caSecretRef.namespace not honoured")
	}
}
