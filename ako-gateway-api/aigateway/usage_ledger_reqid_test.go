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

// The record is positional and pipe-delimited, so the collector reads it by
// index. Field order is therefore a wire contract: a field inserted rather than
// appended silently re-labels everything after it, and the collector cannot
// tell — a model name would start arriving as a tier.
func TestUsageRecordAppendsReqIDAsTheTwelfthField(t *testing.T) {
	lua := buildUsageRecordBlock("llm-route")

	start := strings.Index(lua, "local rec = tostring(now)")
	if start < 0 {
		t.Fatal("record assembly not found — this test is reading the wrong thing")
	}
	end := strings.Index(lua[start:], "\n    avi.vs.table_remove")
	if end < 0 {
		t.Fatal("end of record assembly not found")
	}
	rec := lua[start : start+end]

	// One leading "tostring(now)" plus one concat per subsequent field.
	if got, want := strings.Count(rec, `.. "|" ..`), 11; got != want {
		t.Fatalf("record has %d delimiters (%d fields), want %d (12 fields):\n%s", got, got+1, want, rec)
	}

	fields := strings.Split(rec, `.. "|" ..`)
	last := strings.TrimSpace(fields[len(fields)-1])
	if !strings.Contains(last, "_rid") {
		t.Errorf("last field is %q, want the request id", last)
	}
	// The chain id must still be eleventh: ReqID was appended after it, not in
	// front of it.
	if eleventh := strings.TrimSpace(fields[len(fields)-2]); !strings.Contains(eleventh, "ai_chain") {
		t.Errorf("eleventh field is %q, want the chain id", eleventh)
	}
}

// This block is emitted into HTTP_RESP (the streamed paths) and into
// HTTP_RESP_DATA (the measured path). A header read that a phase does not
// support must cost an empty field, not an error — an unprotected call that
// raises would abort the whole block and lose the record, which reads
// downstream as "this consumer sent no traffic".
func TestUsageRecordReqIDReadIsFailSafe(t *testing.T) {
	lua := buildUsageRecordBlock("llm-route")

	if !strings.Contains(lua, UsageReqIDHeader) {
		t.Fatalf("the block never reads %s", UsageReqIDHeader)
	}
	i := strings.Index(lua, "local _rid")
	if i < 0 {
		t.Fatal("no _rid local")
	}
	j := strings.Index(lua[i:], "local rec =")
	if j < 0 {
		t.Fatal("no record assembly after _rid")
	}
	if !strings.Contains(lua[i:i+j], "pcall(") {
		t.Error("the header read is not wrapped in pcall")
	}
	if !strings.Contains(lua[i:i+j], `local _rid = ""`) {
		t.Error("_rid is not initialised to empty before the read")
	}
}

// _safe replaces anything outside its allow-list and returns "-" for an empty
// string, which is what keeps the delimiter out of a field. An id that arrived
// with a pipe in it would otherwise split one record into two.
func TestUsageRecordReqIDIsSanitised(t *testing.T) {
	lua := buildUsageRecordBlock("llm-route")
	if !strings.Contains(lua, `_safe(_rid, 64)`) {
		t.Error("the request id is not passed through _safe with a length cap")
	}
}
