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

func TestEffectiveAuthMode(t *testing.T) {
	cases := map[string]AuthClaimMode{
		"":             ClaimModeOAuth,
		"oauthBrowser": ClaimModeOAuth,
		"anything":     ClaimModeOAuth,
		"jwtQuery":     ClaimModeJWTQuery,
		"jwtHeader":    ClaimModeJWTHeader,
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

	tok := GenerateTokenAccountingScripts(tierBudgetPolicy(), ClaimModeJWTQuery, "vs-test").ReqScript
	if !strings.Contains(tok, "_b64url_decode") || strings.Contains(tok, "oauth_get_claim") {
		t.Errorf("token-accounting script did not switch to query-decode helper:\n%s", tok)
	}
}

func TestJwtClaimHelperHeaderMode(t *testing.T) {
	h := jwtClaimHelper(ClaimModeJWTHeader)
	mustContain := []string{
		"local function jwt_claim(claim)", // the shared entrypoint name
		"avi.http.get_userid()",           // the only validated value the SE exposes
		`if claim ~= "sub" then return "" end`,
		`get_reqvar("ai_sub")`, // cached for HTTP_RESP_DATA, where get_userid is undocumented
	}
	for _, sub := range mustContain {
		if !strings.Contains(h, sub) {
			t.Errorf("header helper missing %q\n---\n%s", sub, h)
		}
	}
	for _, banned := range []string{"oauth_get_claim", "get_query", "_b64url_decode", "get_header"} {
		if strings.Contains(h, banned) {
			t.Errorf("header helper must not use %s (the header is stripped and no claim can be decoded):\n%s", banned, h)
		}
	}
}

// The claim-consuming generators must thread the header mode too.
func TestGeneratorsThreadJWTHeaderMode(t *testing.T) {
	model := GenerateModelRouteScripts(&AIModelRoutePolicy{Spec: sampleSpec()}, tierPGForTest(), nil, nil, ClaimModeJWTHeader).ReqDataScript
	mcp := GenerateMCPToolAuthScripts(&AIMCPRoutePolicy{Spec: sampleMCPSpec()}, ClaimModeJWTHeader).ReqDataScript
	tok := GenerateTokenAccountingScripts(tierBudgetPolicy(), ClaimModeJWTHeader, "vs-test").ReqScript
	for name, src := range map[string]string{"model-route": model, "mcp-tool-authz": mcp, "token-accounting": tok} {
		if !strings.Contains(src, "avi.http.get_userid()") || strings.Contains(src, "oauth_get_claim") || strings.Contains(src, "_b64url_decode") {
			t.Errorf("%s script did not switch to the header (get_userid) helper:\n%s", name, src)
		}
	}
}

// Run the header helper under the SE sandbox stub: sub comes from get_userid,
// any other claim is "", and once seen the subject survives into a phase where
// get_userid returns nothing (the HTTP_RESP_DATA case) via the cached reqvar.
func TestJwtHeaderClaimHelperAgainstSEStub(t *testing.T) {
	lua, err := exec.LookPath("lua5.3")
	if err != nil {
		if lua, err = exec.LookPath("lua"); err != nil {
			t.Skip("no lua interpreter on PATH; skipping SE-sandbox execution test")
		}
	}
	dir := t.TempDir()
	stub, err := os.ReadFile(filepath.Join("testdata", "se_stub.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "se_stub.lua"), stub, 0o600); err != nil {
		t.Fatal(err)
	}
	helper := jwtClaimHelper(ClaimModeJWTHeader)
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("req.lua", helper+`
local s = jwt_claim("sub"); if s ~= "time-agent" then error("sub: got [" .. tostring(s) .. "]") end
local g = jwt_claim("group"); if g ~= "" then error("group must be empty in header mode, got [" .. tostring(g) .. "]") end
`)
	write("respdata.lua", helper+`
local s = jwt_claim("sub"); if s ~= "time-agent" then error("cached sub: got [" .. tostring(s) .. "]") end
`)
	write("unauth.lua", helper+`
local s = jwt_claim("sub"); if s ~= "" then error("unauthenticated must be empty, got [" .. tostring(s) .. "]") end
`)
	write("drive.lua", `
local dir = arg[1]
local SE = dofile(dir .. "/se_stub.lua")
local function slurp(n) local f = assert(io.open(dir .. "/" .. n, "rb")); local s = f:read("a"); f:close(); return s end
SE.reset_request({})
SE.env.userid = "time-agent"
SE.run("req", slurp("req.lua"))
-- a later phase where the SE no longer answers get_userid: the reqvar must carry it
SE.env.userid = nil
SE.run("respdata", slurp("respdata.lua"))
-- a request that never authenticated yields "" rather than raising
SE.reset_request({})
SE.env.userid = nil
SE.run("req", slurp("unauth.lua"))
io.write("header helper ok\n")
`)
	out, err := exec.Command(lua, filepath.Join(dir, "drive.lua"), dir).CombinedOutput()
	if err != nil {
		t.Fatalf("header helper failed in the SE sandbox: %v\n%s", err, out)
	}
	t.Log(string(out))
}
