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

// TestModelRouteLogsTheDecision covers the generated source. It is the cheap
// half: it runs everywhere, including where no Lua is installed.
//
// What the log line has to survive is a downgrade, because that is the only
// routing outcome the rest of the client log cannot show. Under EVH the pool and
// pool-group names are `<prefix>--<sha1>`, so a downgraded request looks
// identical to a request that asked for the lower tier in the first place.
func TestModelRouteLogsTheDecision(t *testing.T) {
	p := &AIModelRoutePolicy{Spec: sampleSpec()}
	s := GenerateModelRouteScripts(p, tierPGForTest(), nil, nil, ClaimModeOAuth).ReqDataScript

	for _, sub := range []string{
		`"ai-gateway: model=" .. (model ~= "" and model or "-")`, // never concatenates nil
		`" tier=" .. tier`,
		`" requested=" .. _rt .. " downgraded=1"`, // the downgrade, named
		`" pool-group=" .. (pg or "-")`,
		`pcall(function() avi.vs.log(_l) end)`, // closure form: see below
	} {
		if !strings.Contains(s, sub) {
			t.Errorf("ReqDataScript missing %q\n---\n%s", sub, s)
		}
	}

	// An unknown avi.* field RAISES on access, and Lua evaluates a call's
	// arguments before entering pcall — so `pcall(avi.vs.log, _l)` would take the
	// front door down on an SE build without the API instead of protecting it.
	if strings.Contains(s, "pcall(avi.vs.log") {
		t.Error("avi.vs.log must be called inside a closure, not passed to pcall as a function value")
	}

	// One entry per request, because a second call is a second entry and doubles
	// this VS's log volume.
	//
	// Measured on Avi 31.2.1: the entry avi.vs.log produces is a UDF log, NOT a
	// significant one -- the API reports significant=0, udf=true. That matters to
	// anyone reading these decisions in the console: the log viewer hides
	// non-significant entries by default, so every SUCCESSFUL routing decision is
	// invisible until "Non-Significant" is ticked. Only the request that also
	// tripped auth, WAF or the budget shows up on its own.
	if n := strings.Count(s, "avi.vs.log("); n != 1 {
		t.Errorf("expected exactly one avi.vs.log call per request, found %d", n)
	}
}

// TestModelRouteLogWithoutEntitlements: with no entitlements configured there is
// no requested-vs-served distinction to draw, and _rt is never emitted, so the
// log line must not reference it (an undefined global would log "nil" or worse).
func TestModelRouteLogWithoutEntitlements(t *testing.T) {
	spec := sampleSpec()
	spec.Entitlements = nil
	s := GenerateModelRouteScripts(&AIModelRoutePolicy{Spec: spec}, tierPGForTest(), nil, nil, ClaimModeOAuth).ReqDataScript

	if strings.Contains(s, "_rt") {
		t.Errorf("ReqDataScript should not mention _rt without entitlements\n%s", s)
	}
	if !strings.Contains(s, "avi.vs.log(_l)") {
		t.Errorf("the routing decision should still be logged without entitlements\n%s", s)
	}
}

// TestModelRouteScriptsAgainstSEStub is the half that matters: it executes the
// generated scripts in the SE sandbox stub and asserts on the Pool Group they
// selected and the log text they produced. A string match cannot tell you that
// `model` was nil on a bodyless request, and that raise is a 500 on the live LLM
// front door.
//
// See testdata/modelroute_spec.lua for the scenarios.
func TestModelRouteScriptsAgainstSEStub(t *testing.T) {
	// The SE runs an older Lua than a dev box, so a script that only compiles
	// under 5.3 is not evidence about the SE. Run under every interpreter present.
	var luas []string
	for _, name := range []string{"lua5.1", "lua5.3", "lua"} {
		if p, err := exec.LookPath(name); err == nil {
			luas = append(luas, p)
		}
	}
	if len(luas) == 0 {
		t.Skip("no lua interpreter on PATH; skipping SE-sandbox execution test")
	}

	scripts := GenerateModelRouteScripts(&AIModelRoutePolicy{Spec: sampleSpec()},
		tierPGForTest(), nil, nil, ClaimModeJWTQuery)

	// AIGW_MR_DUMP_DIR keeps the generated scripts and the harness on disk after
	// the run, which is how you debug a failing assertion against a real Lua.
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
	for _, f := range []string{"se_stub.lua", "modelroute_spec.lua"} {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, lua := range luas {
		out, err := exec.Command(lua, filepath.Join(dir, "modelroute_spec.lua"), dir).CombinedOutput()
		if err != nil {
			t.Fatalf("model-route SE-sandbox spec failed under %s: %v\n%s", lua, err, out)
		}
		t.Logf("%s:\n%s", lua, out)
	}
}
