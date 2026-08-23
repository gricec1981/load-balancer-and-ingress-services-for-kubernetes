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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// remoteSpec is sampleSpec with a fourth tier that lives at a peer site.
func remoteSpec() AIModelRoutePolicySpec {
	s := sampleSpec()
	s.Tiers = append(s.Tiers, ModelTier{
		Name:   "premium-eu",
		Remote: &ModelRemote{Host: "llm.siteb.ai.avi.com"},
	})
	s.ModelTiers["llama-3-70b-instruct-eu"] = "premium-eu"
	s.Entitlements.Rules[0].Allow = append(s.Entitlements.Rules[0].Allow, "premium-eu")
	return s
}

func remoteRuntimesForTest() map[string]*RemoteRuntime {
	return map[string]*RemoteRuntime{
		"premium-eu": {Tier: "premium-eu", PGName: "ns-pol-premium-eu-remote-pg", Host: "llm.siteb.ai.avi.com"},
	}
}

func TestModelRemoteDefaults(t *testing.T) {
	r := &ModelRemote{Host: "llm.siteb.ai.avi.com"}
	if got := r.EffectivePort(); got != 443 {
		t.Errorf("default port = %d, want 443", got)
	}
	if !r.EffectiveTLS() {
		t.Error("TLS should default on: a peer gateway is reached over the SE's egress")
	}
	if r.EffectivePreserveHost() {
		t.Error("Host should be rewritten by default so the peer's EVH child VS matches")
	}
	if got := r.EffectiveHealthPath(); got != "/v1/models" {
		t.Errorf("default healthPath = %q, want /v1/models", got)
	}

	off, keep := false, true
	r2 := &ModelRemote{Host: "h", Port: 8443, TLS: &off, PreserveHost: &keep, HealthPath: "-"}
	if r2.EffectivePort() != 8443 || r2.EffectiveTLS() || !r2.EffectivePreserveHost() || r2.EffectiveHealthPath() != "-" {
		t.Errorf("explicit values not honoured: %+v", r2)
	}
}

func TestValidateRemoteTier(t *testing.T) {
	valid := remoteSpec()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid remote tier rejected: %v", err)
	}

	cases := []struct {
		name string
		tier ModelTier
		want string
	}{
		{"no host", ModelTier{Name: "t", Remote: &ModelRemote{}}, "remote requires host"},
		{"url not fqdn", ModelTier{Name: "t", Remote: &ModelRemote{Host: "https://llm.siteb.ai.avi.com"}}, "bare FQDN"},
		{"host with port", ModelTier{Name: "t", Remote: &ModelRemote{Host: "llm.siteb.ai.avi.com:443"}}, "bare FQDN"},
		{
			"provider and remote together",
			ModelTier{Name: "t", Remote: &ModelRemote{Host: "h"}, Provider: &ModelProvider{Host: "h", Path: "/p"}},
			"mutually exclusive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := AIModelRoutePolicySpec{Tiers: []ModelTier{tc.tier}, DefaultTier: "t"}
			err := s.Validate()
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// A remote tier rewrites Host and nothing else. In particular it must NOT set
// ai_skip_meter the way a provider tier does — a peer gateway returns a normal
// non-streaming response, and routing a request across a site boundary must not
// quietly stop it being counted against the caller's budget.
func TestGenerateModelRouteScriptsRemote(t *testing.T) {
	p := &AIModelRoutePolicy{Spec: remoteSpec()}
	tierPG := tierPGForTest()
	tierPG["premium-eu"] = "ns-pol-premium-eu-remote-pg"
	s := GenerateModelRouteScripts(p, tierPG, nil, remoteRuntimesForTest(), ClaimModeOAuth).ReqDataScript

	mustContain := []string{
		`local REMOTES = { ["premium-eu"]={h="llm.siteb.ai.avi.com"} }`, // baked table
		`local _rm = REMOTES[tier]`,                                     // per-tier lookup
		`avi.http.replace_header("Host", _rm.h)`,                        // the only rewrite
		`["premium-eu"]="ns-pol-premium-eu-remote-pg"`,                  // tier → PG
		`["llama-3-70b-instruct-eu"]="premium-eu"`,                      // model → tier
	}
	for _, want := range mustContain {
		if !strings.Contains(s, want) {
			t.Errorf("ReqDataScript missing %q\n---\n%s", want, s)
		}
	}
	for _, unwanted := range []string{"ai_skip_meter", "avi.http.set_path"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("remote tier must not emit %q (that is provider-tier behaviour)\n---\n%s", unwanted, s)
		}
	}
}

// preserveHost leaves the Host header alone: RemoteRuntime.Host is empty, and the
// generated Lua guards on that rather than sending an empty header.
func TestGenerateModelRouteScriptsRemotePreserveHost(t *testing.T) {
	p := &AIModelRoutePolicy{Spec: remoteSpec()}
	tierPG := tierPGForTest()
	tierPG["premium-eu"] = "ns-pol-premium-eu-remote-pg"
	remotes := map[string]*RemoteRuntime{
		"premium-eu": {Tier: "premium-eu", PGName: "ns-pol-premium-eu-remote-pg", Host: ""},
	}
	s := GenerateModelRouteScripts(p, tierPG, nil, remotes, ClaimModeOAuth).ReqDataScript

	if !strings.Contains(s, `["premium-eu"]={h=""}`) {
		t.Errorf("preserveHost should bake an empty host\n---\n%s", s)
	}
	if !strings.Contains(s, `if _rm and _rm.h ~= "" then`) {
		t.Errorf("generated Lua must guard the empty host\n---\n%s", s)
	}
}

// With no remote tiers the script is byte-identical to before the feature apart
// from the empty REMOTES table — no rewrite block is emitted at all.
func TestGenerateModelRouteScriptsNoRemotes(t *testing.T) {
	p := &AIModelRoutePolicy{Spec: sampleSpec()}
	s := GenerateModelRouteScripts(p, tierPGForTest(), nil, nil, ClaimModeOAuth).ReqDataScript

	if strings.Contains(s, "REMOTES[tier]") {
		t.Errorf("no remote tiers should emit no rewrite block\n---\n%s", s)
	}
	if !strings.Contains(s, "local REMOTES = {  }") {
		t.Errorf("expected an empty REMOTES table\n---\n%s", s)
	}
}

func TestRemoteObjectNames(t *testing.T) {
	// Names must be stable and distinct from the provider tier's, or a policy
	// carrying both kinds of tier would have them collide in Avi.
	if got, want := remotePoolName("inference", "llm", "premium-eu"), "inference-llm-premium-eu-remote-pool"; got != want {
		t.Errorf("pool name = %q, want %q", got, want)
	}
	if got, want := remotePoolGroupName("inference", "llm", "premium-eu"), "inference-llm-premium-eu-remote-pg"; got != want {
		t.Errorf("pool group name = %q, want %q", got, want)
	}
	if got, want := remoteHealthMonitorName("inference", "llm", "premium-eu"), "inference-llm-premium-eu-remote-hm"; got != want {
		t.Errorf("health monitor name = %q, want %q", got, want)
	}
	if remotePoolName("ns", "p", "t") == providerPoolName("ns", "p", "t") {
		t.Error("remote and provider pool names must not collide")
	}
}

// The FQDN pool is the whole point: AKO must never write an address into Avi, so
// that a renumbered peer needs no reconcile.
func TestFQDNPoolSpecHasNoAddress(t *testing.T) {
	pool := fqdnPoolBody(fqdnPoolSpec{
		Name: "p", TenantRef: "/api/tenant/?name=admin", CloudRef: "/api/cloud/?name=Default-Cloud",
		Host: "llm.siteb.ai.avi.com", Port: 443, TLS: true,
		HealthMonitorRefs: []string{"/api/healthmonitor/?name=hm"},
	})

	servers, ok := pool["servers"].([]map[string]interface{})
	if !ok || len(servers) != 1 {
		t.Fatalf("expected exactly one server, got %#v", pool["servers"])
	}
	srv := servers[0]
	if srv["resolve_server_by_dns"] != true {
		t.Error("the SE must resolve the peer name itself")
	}
	if srv["hostname"] != "llm.siteb.ai.avi.com" {
		t.Errorf("server hostname = %v, want the peer FQDN", srv["hostname"])
	}
	// Avi 31.2.1 rejects a pool whose server has no `ip` ("Pool is missing
	// required fields: servers[0].ip"), so the field must be present -- but as a
	// DNS-typed IpAddr carrying the NAME. That is the distinction worth
	// asserting: an FQDN pool must never pin a V4/V6 literal, because the whole
	// point is that the peer can be renumbered without AKO noticing.
	ip, ok := srv["ip"].(map[string]interface{})
	if !ok {
		t.Fatalf("server must carry an ip field; Avi rejects the POST without it: %#v", srv["ip"])
	}
	if ip["type"] != "DNS" {
		t.Errorf("ip.type = %v, want DNS -- a V4/V6 type would pin the peer's address", ip["type"])
	}
	if ip["addr"] != "llm.siteb.ai.avi.com" {
		t.Errorf("ip.addr = %v, want the peer FQDN", ip["addr"])
	}
	if pool["ssl_profile_ref"] == nil {
		t.Error("TLS true should attach an SSL profile (SNI comes from the server hostname)")
	}
	if pool["health_monitor_refs"] == nil {
		t.Error("health monitor refs should be attached so failover is not TTL-bound")
	}

	// TLS off and no monitor: neither key should be present at all.
	plain := fqdnPoolBody(fqdnPoolSpec{Name: "p", Host: "h", Port: 80})
	if _, present := plain["ssl_profile_ref"]; present {
		t.Error("TLS false should not attach an SSL profile")
	}
	if _, present := plain["health_monitor_refs"]; present {
		t.Error("healthPath \"-\" should attach no monitor refs")
	}
}

func TestRemoteHealthMonitorBody(t *testing.T) {
	// The monitor is what makes a dead peer detectable in seconds. HTTPS peers
	// need the https_monitor field, not http_monitor — Avi rejects the mismatch.
	for _, tc := range []struct {
		tls       bool
		wantType  string
		wantField string
		notField  string
	}{
		{true, "HEALTH_MONITOR_HTTPS", "https_monitor", "http_monitor"},
		{false, "HEALTH_MONITOR_HTTP", "http_monitor", "https_monitor"},
	} {
		hm := remoteHealthMonitorBody("hm", "/api/tenant/?name=admin", "/v1/models", tc.tls)
		if hm["type"] != tc.wantType {
			t.Errorf("tls=%t type = %v, want %s", tc.tls, hm["type"], tc.wantType)
		}
		m, ok := hm[tc.wantField].(map[string]interface{})
		if !ok {
			t.Fatalf("tls=%t missing %s", tc.tls, tc.wantField)
		}
		if _, present := hm[tc.notField]; present {
			t.Errorf("tls=%t should not set %s", tc.tls, tc.notField)
		}
		if m["http_request"] != "GET /v1/models HTTP/1.0" {
			t.Errorf("http_request = %v", m["http_request"])
		}
	}
}

// TestRemoteScriptsAgainstSEStub executes the generated scripts for a policy
// with a remote tier in the SE sandbox. A string match on the Lua cannot tell
// you that the Host rewrite fires for the peer tier and stays out of the way for
// the local ones, nor that ai_skip_meter is still unset when the request leaves
// the site — and that last one fails silently in production.
//
// See testdata/modelroute_remote_spec.lua for the scenarios.
func TestRemoteScriptsAgainstSEStub(t *testing.T) {
	var luas []string
	for _, name := range []string{"lua5.1", "lua5.3", "lua"} {
		if p, err := exec.LookPath(name); err == nil {
			luas = append(luas, p)
		}
	}
	if len(luas) == 0 {
		t.Skip("no lua interpreter on PATH; skipping SE-sandbox execution test")
	}

	tierPG := tierPGForTest()
	tierPG["premium-eu"] = "ns-pol-premium-eu-remote-pg"
	scripts := GenerateModelRouteScripts(&AIModelRoutePolicy{Spec: remoteSpec()},
		tierPG, nil, remoteRuntimesForTest(), ClaimModeJWTQuery)

	dir := os.Getenv("AIGW_MR_DUMP_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	for name, src := range map[string]string{
		"mrreq.lua":     scripts.ReqScript,
		"mrreqdata.lua": scripts.ReqDataScript,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"se_stub.lua", "modelroute_remote_spec.lua"} {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, lua := range luas {
		out, err := exec.Command(lua, filepath.Join(dir, "modelroute_remote_spec.lua"), dir).CombinedOutput()
		if err != nil {
			t.Fatalf("remote-tier SE-sandbox spec failed under %s: %v\n%s", lua, err, out)
		}
		t.Logf("%s:\n%s", lua, out)
	}
}
