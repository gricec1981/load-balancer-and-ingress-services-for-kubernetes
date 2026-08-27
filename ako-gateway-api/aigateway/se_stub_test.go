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

// The generated Lua only ever runs inside an Avi Service Engine, where a syntax
// error is a 500 on the live LLM front door and a logic error is a wrong bill.
// Neither is visible to a test that matches strings in the generated source.
//
// So the scripts are executed here, against a stub that reproduces the SE
// sandbox including the ways it is not stock Lua (testdata/se_stub.lua). The
// assertions live in testdata/ledger_spec.lua, next to the state they inspect.
//
// Skipped where no Lua interpreter is installed, so it never blocks a build; on
// a machine that has one it is the only test here that can tell you the scripts
// actually work.
func TestGeneratedScriptsAgainstSEStub(t *testing.T) {
	runSEStubSpec(t, "ledger_spec.lua")
}

// TestConcurrentResponsesAgainstSEStub schedules the interleaving a real SE can
// produce and a sequential harness never will.
//
// TestGeneratedScriptsAgainstSEStub runs each script to completion before the
// next one starts, so every assertion it makes is a single-threaded assertion. A
// VS is served by many dispatcher cores against one shared VS table, and the
// counter and the ring head are both read-modify-writes with no atomic primitive
// under them. This test stops two responses between the read and the write on
// purpose, so the lost update is deterministic rather than a flake nobody can
// reproduce.
//
// See testdata/concurrency_spec.lua for what each scenario measures.
func TestConcurrentResponsesAgainstSEStub(t *testing.T) {
	runSEStubSpec(t, "concurrency_spec.lua")
}

// runSEStubSpec generates the DataScript phases, lays them out next to the stub
// and the named spec, and executes the spec under a real Lua.
//
// Shared rather than copied: two specs that must agree about how the scripts are
// generated cannot be allowed to drift apart in their setup.
func runSEStubSpec(t *testing.T, spec string) {
	t.Helper()

	lua, err := exec.LookPath("lua5.3")
	if err != nil {
		if lua, err = exec.LookPath("lua"); err != nil {
			t.Skip("no lua interpreter on PATH; skipping SE-sandbox execution test")
		}
	}

	scripts := GenerateTokenAccountingScripts(countersPolicy(), ClaimModeJWTQuery, "vs-test")

	// AIGW_DUMP_DIR keeps the generated scripts and the harness on disk after the
	// run, which is how you debug a failing assertion against the real Lua.
	dir := os.Getenv("AIGW_DUMP_DIR")
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
	// The stub and the spec are copied in so the spec can dofile the stub by a
	// path relative to the generated scripts.
	for _, f := range []string{"se_stub.lua", spec} {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, err := exec.Command(lua, filepath.Join(dir, spec), dir).CombinedOutput()
	if err != nil {
		t.Fatalf("SE-sandbox spec %s failed: %v\n%s", spec, err, out)
	}
	t.Log(string(out))
}
