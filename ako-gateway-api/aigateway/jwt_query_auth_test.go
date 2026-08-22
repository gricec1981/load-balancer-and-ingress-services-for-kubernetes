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
	"strings"
	"testing"
)

func TestEffectiveAuthMode(t *testing.T) {
	cases := map[string]AuthClaimMode{
		"":             ClaimModeOAuth,
		"oauthBrowser": ClaimModeOAuth,
		"anything":     ClaimModeOAuth,
		"jwtQuery":     ClaimModeJWTQuery,
	}
	for in, want := range cases {
		got := AIGatewayAuthPolicySpec{AuthMode: in}.EffectiveAuthMode()
		if got != want {
			t.Errorf("EffectiveAuthMode(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestJwtClaimHelperOAuthMode(t *testing.T) {
	h := jwtClaimHelper(ClaimModeOAuth)
	if !strings.Contains(h, "oauth_get_claim") {
		t.Errorf("OAuth helper must call oauth_get_claim:\n%s", h)
	}
	if strings.Contains(h, "get_query") {
		t.Errorf("OAuth helper must not decode a query param:\n%s", h)
	}
}

func TestJwtClaimHelperQueryMode(t *testing.T) {
	h := jwtClaimHelper(ClaimModeJWTQuery)

	// A stray '%' would mean a fmt verb leaked (e.g. the Lua modulo not escaped).
	if strings.Contains(h, "%!") {
		t.Fatalf("query helper has a formatting error:\n%s", h)
	}
	mustContain := []string{
		"local function jwt_claim(claim)", // the shared entrypoint name
		"avi.http.get_query()",            // reads the (unstripped) query param
		`string.find(q, "jwt=", 1, true)`, // looks for the configured param name
		"_b64url_decode",                  // base64url decode of the JWT payload
		"_json_scalar",                    // claim extraction from the payload JSON
		"% 256",                           // Lua modulo rendered (escaped %% in Go)
	}
	for _, sub := range mustContain {
		if !strings.Contains(h, sub) {
			t.Errorf("query helper missing %q\n---\n%s", sub, h)
		}
	}
	// Decode-and-trust must NOT use oauth_get_claim in this mode.
	if strings.Contains(h, "oauth_get_claim") {
		t.Errorf("query helper must not call oauth_get_claim:\n%s", h)
	}
}

// The three claim-consuming generators must honour the mode and emit the
// query-decoding helper (not the OAuth one) when asked.
func TestGeneratorsThreadJWTQueryMode(t *testing.T) {
	model := GenerateModelRouteScripts(&AIModelRoutePolicy{Spec: sampleSpec()}, tierPGForTest(), nil, nil, ClaimModeJWTQuery).ReqDataScript
	if !strings.Contains(model, "_b64url_decode") || strings.Contains(model, "oauth_get_claim") {
		t.Errorf("model-route script did not switch to query-decode helper:\n%s", model)
	}

	mcp := GenerateMCPToolAuthScripts(&AIMCPRoutePolicy{Spec: sampleMCPSpec()}, ClaimModeJWTQuery).ReqDataScript
	if !strings.Contains(mcp, "_b64url_decode") || strings.Contains(mcp, "oauth_get_claim") {
		t.Errorf("MCP tool-authz script did not switch to query-decode helper:\n%s", mcp)
	}

	tok := GenerateTokenAccountingScripts(tierBudgetPolicy(), ClaimModeJWTQuery).ReqScript
	if !strings.Contains(tok, "_b64url_decode") || strings.Contains(tok, "oauth_get_claim") {
		t.Errorf("token-accounting script did not switch to query-decode helper:\n%s", tok)
	}
}
