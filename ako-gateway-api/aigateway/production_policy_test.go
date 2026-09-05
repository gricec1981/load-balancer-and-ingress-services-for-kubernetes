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
	"testing"
)

// livePolicy mirrors the AITokenRateLimitPolicy actually deployed on the demo
// LLM front door (inference/llm-limits), including the pieces countersPolicy
// leaves out: a Reject action with retryAfter, a real counter epoch, and two
// group budgets.
//
// It exists because the generic fixture passed every check and the real one
// still 500'd the live gateway. A fixture that is not the shape you deploy is
// only testing the shape you did not deploy.
func livePolicy() *AITokenRateLimitPolicy {
	return &AITokenRateLimitPolicy{
		AdminToken:   "s3cr3t",
		CounterEpoch: "1786748551",
		Spec: AITokenRateLimitPolicySpec{
			TargetRef: PolicyTargetRef{
				Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "llm-route",
			},
			IdentitySource: &IdentitySource{Header: "x-ai-consumer", Fallback: "clientIP"},
			Limits: []TokenLimit{{
				Name: "hourly-group-budget", Key: "consumer", Tokens: "total", Window: "1h",
				GroupHeader:  "group",
				GroupBudgets: map[string]int64{"group1": 500, "group2": 10000000},
				Budget:       0,
				Action:       &LimitAction{Type: "Reject", StatusCode: 429, RetryAfter: true},
			}},
		},
	}
}

// The production policy's generated scripts must load and run in the SE sandbox
// exactly as the simpler fixture's do.
func TestLivePolicyScriptsAgainstSEStub(t *testing.T) {
	lua, err := exec.LookPath("lua5.3")
	if err != nil {
		if lua, err = exec.LookPath("lua"); err != nil {
			t.Skip("no lua interpreter on PATH; skipping SE-sandbox execution test")
		}
	}

	scripts := GenerateTokenAccountingScripts(livePolicy(), ClaimModeJWTQuery, "vs-test")

	dir := os.Getenv("AIGW_LIVE_DUMP_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	for name, src := range map[string]string{
		"req.lua":      scripts.ReqScript,
		"resp.lua":     scripts.RespScript,
		"respdata.lua": scripts.RespDataScript,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join("testdata", "se_stub.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "se_stub.lua"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	// Load and run each phase on an ordinary chat completion. A syntax error or a
	// raise here is what a 500 on the live front door looks like.
	driver := `
local dir = arg[1]
local SE = dofile(dir .. "/se_stub.lua")
local function slurp(n) local f = assert(io.open(dir .. "/" .. n, "rb")); local s = f:read("a"); f:close(); return s end
local JWT = "hdr.eyJzdWIiOiJhbGljZSIsImdyb3VwIjoiZ3JvdXAxIn0.sig"

-- 1. an ordinary completion
SE.reset_request({ respheaders = { ["Content-Type"] = "application/json" },
  body = '{"model":"m","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}' })
SE.env.query = "jwt=" .. JWT
SE.run("req", slurp("req.lua"))
if SE.env.resp then error("request was rejected: " .. tostring(SE.env.resp.code)) end
SE.run("resp", slurp("resp.lua"))
SE.run("respdata", slurp("respdata.lua"))

-- 2. a request with NO query string at all (health checks, probes)
SE.reset_request({ respheaders = { ["Content-Type"] = "application/json" } })
SE.env.query = ""
SE.run("req", slurp("req.lua"))

-- 3. the drain endpoint
SE.reset_request({ path = "/v1/admin/usage" })
SE.env.query = "after=0"
SE.env.reqheaders["X-Admin-Token"] = "s3cr3t"
SE.run("req", slurp("req.lua"))
if not SE.env.resp or SE.env.resp.code ~= 200 then
  error("drain did not answer 200: " .. tostring(SE.env.resp and SE.env.resp.code))
end
io.write("live-policy scripts loaded and ran cleanly\n")
`
	if err := os.WriteFile(filepath.Join(dir, "drive.lua"), []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(lua, filepath.Join(dir, "drive.lua"), dir).CombinedOutput()
	if err != nil {
		t.Fatalf("live-policy scripts failed in the SE sandbox: %v\n%s", err, out)
	}
	t.Log(string(out))
}
