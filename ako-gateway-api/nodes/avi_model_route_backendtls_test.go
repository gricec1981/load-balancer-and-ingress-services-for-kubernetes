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

package nodes

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/vmware/alb-sdk/go/models"

	akogatewayapiaigateway "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/aigateway"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

func testCertPEM(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func backendTLSTestNodes() (*nodes.AviEvhVsNode, *nodes.AviPoolNode) {
	vs := &nodes.AviEvhVsNode{Name: "cluster--gateway-llm-gateway-gateway-llm-route-abc", Tenant: "admin"}
	pool := &nodes.AviPoolNode{
		Name:    "cluster--gateway-llm-gateway-gateway-llm-route-aimr-pais-cpu-pais-pais-llm-1443",
		Tenant:  "admin",
		Port:    1443,
		Servers: []nodes.AviPoolMetaServer{{Ip: models.IPAddr{Addr: strPtr("10.0.0.5"), Type: strPtr("V4")}, Port: 61001}},
	}
	pool.AviMarkers = utils.AviObjectMarkers{GatewayName: "llm-gateway", GatewayNamespace: "gateway", BackendNs: "pais", BackendName: "pais-llm"}
	return vs, pool
}

func strPtr(s string) *string { return &s }

// A backendTLS tier pool carries the SSL profile, SNI server name, host check,
// the PKI profile (root + intermediate) as its PkiProfile node, and the client
// certificate as ssl_key_and_certificate_ref — with the leaf and its
// intermediate authored as TLSKeyCert nodes on the child VS (CA first).
func TestApplyModelTierBackendTLSPoolRefs(t *testing.T) {
	root, inter, leaf := testCertPEM(t, "root"), testCertPEM(t, "pais-mtls-ca"), testCertPEM(t, "client")
	m := &akogatewayapiaigateway.BackendTLSMaterial{
		SNI:       "pais-llm.pais.svc",
		HostCheck: true,
		CABundle:  root + inter,
		ClientCert: &akogatewayapiaigateway.ClientCertMaterial{
			SecretNamespace: "pais", SecretName: "pais-mtls-uid",
			LeafPEM: leaf, KeyPEM: "-----BEGIN EC PRIVATE KEY-----\nx\n-----END EC PRIVATE KEY-----\n",
			ChainPEM: []string{inter},
		},
	}
	vs, pool := backendTLSTestNodes()
	plainSum := pool.GetCheckSum()
	applyModelTierBackendTLS("key", vs, pool, m)

	if !pool.SniEnabled {
		t.Error("sni_enabled not set")
	}
	if pool.SslProfileRef == nil || *pool.SslProfileRef != "/api/sslprofile?name="+lib.DefaultPoolSSLProfile {
		t.Errorf("ssl_profile_ref = %v", pool.SslProfileRef)
	}
	if pool.ServerName == nil || *pool.ServerName != "pais-llm.pais.svc" {
		t.Errorf("server_name = %v", pool.ServerName)
	}
	if pool.HostCheckEnabled == nil || !*pool.HostCheckEnabled || len(pool.DomainName) != 1 || pool.DomainName[0] != "pais-llm.pais.svc" {
		t.Errorf("host check = %v / %v", pool.HostCheckEnabled, pool.DomainName)
	}
	if pool.PkiProfile == nil || pool.PkiProfile.Name != lib.GetPoolPKIProfileName(pool.Name) ||
		pool.PkiProfile.Tenant != "admin" || pool.PkiProfile.CACert != root+inter {
		t.Fatalf("pki profile = %+v", pool.PkiProfile)
	}
	if got := lib.SplitPKICABundle(pool.PkiProfile.CACert); len(got) != 2 || got[0] != root || got[1] != inter {
		t.Fatalf("PKI profile must list root and intermediate as separate CAs, got %d entries", len(got))
	}

	if len(vs.CACertRefs) != 2 {
		t.Fatalf("child VS cert nodes = %d, want 2 (intermediate + leaf)", len(vs.CACertRefs))
	}
	ca, leafNode := vs.CACertRefs[0], vs.CACertRefs[1]
	if ca.Type != lib.CertTypeCA || string(ca.Cert) != inter || len(ca.Key) != 0 || ca.Tenant != "admin" {
		t.Errorf("intermediate node = type %s tenant %s", ca.Type, ca.Tenant)
	}
	if leafNode.Type != lib.CertTypeVS || string(leafNode.Cert) != leaf || len(leafNode.Key) == 0 || leafNode.CACert != ca.Name {
		t.Errorf("leaf node = type %s caRef %q", leafNode.Type, leafNode.CACert)
	}
	if pool.SslKeyAndCertificateRef == nil || *pool.SslKeyAndCertificateRef != "/api/sslkeyandcertificate?name="+leafNode.Name {
		t.Fatalf("ssl_key_and_certificate_ref = %v, want the leaf %s", pool.SslKeyAndCertificateRef, leafNode.Name)
	}
	if len(vs.SSLKeyCertRefs) != 0 {
		t.Fatal("client certificate must not become a VS server certificate (SSLKeyCertRefs)")
	}
	if !strings.HasPrefix(leafNode.Name, lib.GetNamePrefix()) || leafNode.Name == ca.Name {
		t.Errorf("names: leaf %q ca %q", leafNode.Name, ca.Name)
	}
	if pool.GetCheckSum() == plainSum {
		t.Error("pool checksum unchanged by backend TLS")
	}

	// Idempotent on rebuild (a second tier presenting the same Secret, or the
	// next reconcile): nodes are replaced by name, not duplicated.
	_, pool2 := backendTLSTestNodes()
	applyModelTierBackendTLS("key", vs, pool2, m)
	if len(vs.CACertRefs) != 2 {
		t.Fatalf("re-apply duplicated cert nodes: %d", len(vs.CACertRefs))
	}

	// Rotation: new leaf content → same object name, different checksum.
	sum := leafNode.GetCheckSum()
	rotated := *m
	cc := *m.ClientCert
	cc.LeafPEM = testCertPEM(t, "client-rotated")
	rotated.ClientCert = &cc
	_, pool3 := backendTLSTestNodes()
	applyModelTierBackendTLS("key", vs, pool3, &rotated)
	if len(vs.CACertRefs) != 2 || vs.CACertRefs[1].Name != leafNode.Name || vs.CACertRefs[1].GetCheckSum() == sum {
		t.Fatal("rotation must update the same-named leaf with a new checksum")
	}
}

// SNI and host check are optional; no CA → no PKI profile; no client cert →
// no ssl_key_and_certificate_ref and no cert nodes.
func TestApplyModelTierBackendTLSMinimal(t *testing.T) {
	vs, pool := backendTLSTestNodes()
	applyModelTierBackendTLS("key", vs, pool, &akogatewayapiaigateway.BackendTLSMaterial{})
	if !pool.SniEnabled || pool.SslProfileRef == nil {
		t.Fatal("TLS pool fields not set")
	}
	if pool.ServerName != nil || pool.PkiProfile != nil || pool.SslKeyAndCertificateRef != nil || len(vs.CACertRefs) != 0 {
		t.Fatalf("unexpected refs: server_name=%v pki=%v cert=%v nodes=%d",
			pool.ServerName, pool.PkiProfile, pool.SslKeyAndCertificateRef, len(vs.CACertRefs))
	}
	if pool.HostCheckEnabled == nil || *pool.HostCheckEnabled || pool.DomainName != nil {
		t.Fatal("host check must default off")
	}
}

// Client-cert object names are deterministic per (pool, Secret): distinct pools
// get distinct objects, leaf and intermediates differ.
func TestModelTierClientCertNames(t *testing.T) {
	a := modelTierClientCertName("pool-a", "pais", "pais-mtls-uid", 0)
	if a != modelTierClientCertName("pool-a", "pais", "pais-mtls-uid", 0) {
		t.Fatal("name not deterministic")
	}
	if a == modelTierClientCertName("pool-b", "pais", "pais-mtls-uid", 0) {
		t.Fatal("distinct pools must not share a certificate object")
	}
	if a == modelTierClientCertName("pool-a", "pais", "pais-mtls-uid", 1) {
		t.Fatal("leaf and intermediate must differ")
	}
}

// A multi-rule HTTPRoute gives every rule its own child VS, but the tier pool is
// named per route, so each child builds the same pool. All of them must author
// the same certificate nodes and point the shared pool at the same leaf —
// otherwise each child's publish flips the pool's ssl_key_and_certificate_ref
// and one child's cleanup deletes the certificate another child's pool uses.
func TestModelTierClientCertSharedPoolAcrossChildVSes(t *testing.T) {
	inter, leaf := testCertPEM(t, "pais-mtls-ca"), testCertPEM(t, "client")
	m := &akogatewayapiaigateway.BackendTLSMaterial{
		SNI: "pais-llm.pais.svc",
		ClientCert: &akogatewayapiaigateway.ClientCertMaterial{
			SecretNamespace: "pais", SecretName: "pais-mtls-uid",
			LeafPEM: leaf, KeyPEM: "-----BEGIN EC PRIVATE KEY-----\nx\n-----END EC PRIVATE KEY-----\n",
			ChainPEM: []string{inter},
		},
	}
	vsA, poolA := backendTLSTestNodes()
	vsB, poolB := backendTLSTestNodes()
	vsB.Name = vsA.Name + "-rule2"
	applyModelTierBackendTLS("key", vsA, poolA, m)
	applyModelTierBackendTLS("key", vsB, poolB, m)

	if *poolA.SslKeyAndCertificateRef != *poolB.SslKeyAndCertificateRef {
		t.Fatalf("shared pool points at different certificates per child VS: %s vs %s",
			*poolA.SslKeyAndCertificateRef, *poolB.SslKeyAndCertificateRef)
	}
	if poolA.GetCheckSum() != poolB.GetCheckSum() {
		t.Fatal("the same pool must checksum identically from every child VS")
	}
	if len(vsA.CACertRefs) != 2 || len(vsB.CACertRefs) != 2 {
		t.Fatalf("cert nodes = %d / %d, want 2 each", len(vsA.CACertRefs), len(vsB.CACertRefs))
	}
	for i := range vsA.CACertRefs {
		if vsA.CACertRefs[i].Name != vsB.CACertRefs[i].Name ||
			vsA.CACertRefs[i].GetCheckSum() != vsB.CACertRefs[i].GetCheckSum() {
			t.Fatalf("cert node %d differs between child VSes: %s vs %s", i, vsA.CACertRefs[i].Name, vsB.CACertRefs[i].Name)
		}
	}
}

// The PKI bundle split only fires for a canonical multi-certificate bundle, so
// existing single-certificate PKI profiles keep their exact payload.
func TestSplitPKICABundle(t *testing.T) {
	root, inter := testCertPEM(t, "root"), testCertPEM(t, "inter")
	if got := lib.SplitPKICABundle(root); len(got) != 1 || got[0] != root {
		t.Fatal("single certificate must pass through")
	}
	if got := lib.SplitPKICABundle(root + inter); len(got) != 2 || strings.Join(got, "") != root+inter {
		t.Fatal("canonical bundle must split and re-join to itself")
	}
	odd := "# comment\n" + root + "\n\n" + inter
	if got := lib.SplitPKICABundle(odd); len(got) != 1 || got[0] != odd {
		t.Fatal("non-canonical text must pass through unchanged")
	}
}
