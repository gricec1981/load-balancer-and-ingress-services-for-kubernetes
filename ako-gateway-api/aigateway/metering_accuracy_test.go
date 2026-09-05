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

// The fail-closed penalty must be reachable ONLY on evidence that the body was
// unreadable — it filled the buffer, or it was compressed. Charging it whenever
// `usage` is merely absent is what made one GET /v1/models cost 65536 tokens and
// 429 a consumer for the rest of the window (measured live on Avi 31.2.x).
func TestPenaltyRequiresTruncationEvidence(t *testing.T) {
	lua := GenerateTokenAccountingScripts(countersPolicy(), ClaimModeJWTQuery, "vs-test").RespDataScript

	// The unconditional form: "no usage parsed" => charge the penalty.
	if strings.Contains(lua, "if total_tokens == 0 then\n    total_tokens      =") {
		t.Fatal("penalty is charged whenever usage is absent; it must require truncation evidence")
	}
	for _, want := range []string{
		"#_body >= 262144",                           // body filled the 256 KB buffer
		`avi.http.get_reqvar("ai_meter_enc") == "1"`, // or arrived compressed
	} {
		if !strings.Contains(lua, want) {
			t.Errorf("penalty gate missing evidence test %q", want)
		}
	}
}

// A short 2xx JSON response with no `usage` is not a completion. It must leave
// every counter untouched rather than being charged as an unparseable one.
func TestUnmeterableShortResponseCostsNothing(t *testing.T) {
	lua := GenerateTokenAccountingScripts(countersPolicy(), ClaimModeJWTQuery, "vs-test").RespDataScript

	// total_tokens is initialised to 0 and only the evidence-gated branch may
	// raise it without a parsed usage block.
	if n := strings.Count(lua, "total_tokens      = 65536"); n != 1 {
		t.Fatalf("expected exactly one penalty assignment, found %d", n)
	}
	idx := strings.Index(lua, "total_tokens      = 65536")
	gate := strings.LastIndex(lua[:idx], "elseif")
	if gate < 0 || !strings.Contains(lua[gate:idx], "#_body") {
		t.Fatal("penalty assignment is not guarded by the truncation-evidence branch")
	}
}

// Reporting and enforcement disagree by design: the budget may be charged a
// penalty, but the ledger must never present one as a measurement. The response
// phase publishes which of the two it was.
func TestMeterQualityIsPublishedForTheLedger(t *testing.T) {
	lua := GenerateTokenAccountingScripts(countersPolicy(), ClaimModeJWTQuery, "vs-test").RespDataScript

	for _, want := range []string{
		`local meter_quality     = "none"`,
		`meter_quality = "exact"`,
		`meter_quality     = "penalty"`,
		`avi.http.set_reqvar("ai_meter_quality", meter_quality)`,
	} {
		if !strings.Contains(lua, want) {
			t.Errorf("RespDataScript missing %q", want)
		}
	}
}

// avi.http.get_method does not exist on this SE build. The bare
// pcall(avi.http.get_method) form protected nothing (Lua evaluates the field
// access before pcall runs) and failed open, so no generated script may call it.
// Every avi.http probe must go through a deferred closure instead.
func TestNoBareAviHTTPProbes(t *testing.T) {
	s := GenerateTokenAccountingScripts(countersPolicy(), ClaimModeJWTQuery, "vs-test")
	for name, lua := range map[string]string{
		"req": s.ReqScript, "resp": s.RespScript, "respdata": s.RespDataScript,
		"reqdata": s.ReqDataEnforceScript,
	} {
		for n, line := range strings.Split(lua, "\n") {
			// Lua comments explain the trap by naming it, so only executable
			// lines are checked.
			if code, _, _ := strings.Cut(strings.TrimSpace(line), "--"); code != "" {
				if strings.Contains(code, "avi.http.get_method") {
					t.Errorf("%s:%d calls avi.http.get_method, which this SE build does not have",
						name, n+1)
				}
				// The argument is evaluated before pcall runs, so a raise on the
				// field access escapes the pcall entirely.
				if strings.Contains(code, "pcall(avi.http.") {
					t.Errorf("%s:%d uses the unprotected pcall(avi.http.X) form: %s\n"+
						"wrap the call in a closure: pcall(function() return avi.http.X() end)",
						name, n+1, strings.TrimSpace(line))
				}
			}
		}
	}
}

// A compressed body genuinely cannot be parsed, and HTTP_RESP_DATA cannot read
// headers — so the decision has to be recorded in HTTP_RESP, where they are
// still available.
func TestCompressedResponseFlaggedInResponseHeaderPhase(t *testing.T) {
	lua := GenerateTokenAccountingScripts(countersPolicy(), ClaimModeJWTQuery, "vs-test").RespScript

	if !strings.Contains(lua, `avi.http.get_header("Content-Encoding")`) {
		t.Error("RespScript does not inspect Content-Encoding")
	}
	if !strings.Contains(lua, `avi.http.set_reqvar("ai_meter_enc", "1")`) {
		t.Error("RespScript does not publish the compressed-body flag for HTTP_RESP_DATA")
	}
}
