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

package lib

import (
	"os"
	"strings"
	"testing"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
)

// withNaming runs fn with the readable-name flag in the requested state and the same
// name prefix wiring that cmd/gateway-api/main.go performs at boot.
func withNaming(t *testing.T, readable bool, fn func()) {
	t.Helper()
	oldCluster := lib.ClusterName
	oldPrefix := lib.NamePrefix
	oldFlag := os.Getenv(lib.USE_READABLE_OBJECT_NAMES)
	defer func() {
		lib.ClusterName = oldCluster
		lib.NamePrefix = oldPrefix
		os.Setenv(lib.USE_READABLE_OBJECT_NAMES, oldFlag)
	}()

	lib.ClusterName = "openshift06"
	if readable {
		os.Setenv(lib.USE_READABLE_OBJECT_NAMES, "true")
		lib.SetNamePrefix("")
	} else {
		os.Setenv(lib.USE_READABLE_OBJECT_NAMES, "false")
		lib.SetNamePrefix(Prefix)
	}
	fn()
}

// TestChildNameDefaultUnchanged is the safety net for the whole change: with readable
// names off, a child VS is named exactly as it was before surfaces existed, prefix and
// all. GetChildNameWithHint must agree with GetChildName for the same route.
func TestChildNameDefaultUnchanged(t *testing.T) {
	withNaming(t, false, func() {
		plain := GetChildName("ai-gw", "mcp-gateway", "mcp", "mcp-web", "tools")
		hinted := GetChildNameWithHint("ai-gw", "mcp-gateway", "mcp", "mcp-web", "tools", SurfaceMCP, "tools")
		if plain != hinted {
			t.Fatalf("surface hint changed the default name: %q vs %q", plain, hinted)
		}
		if !strings.HasPrefix(plain, "ako-gw-openshift06--") {
			t.Fatalf("default name lost its ako-gw- prefix: %q", plain)
		}
		if strings.Contains(strings.TrimPrefix(plain, "ako-gw-openshift06--"), "mcp") {
			t.Fatalf("default name must be a bare digest, got %q", plain)
		}
	})
}

// TestChildNameReadableLeadsWithSurface covers the point of the exercise: what a
// person reads in the Avi UI says what the object is.
func TestChildNameReadableLeadsWithSurface(t *testing.T) {
	withNaming(t, true, func() {
		for _, tc := range []struct {
			surface, routeNs, routeName, rule, wantPrefix string
		}{
			{SurfaceMCP, "mcp", "mcp-web", "tools", "openshift06--mcp-mcp-mcp-web-tools-"},
			{SurfaceAgent, "agents", "log-collector", "", "openshift06--agent-agents-log-collector-"},
			{SurfaceLLM, "ai-gw", "llm", "chat", "openshift06--llm-ai-gw-llm-chat-"},
			{"", "default", "shop", "checkout", "openshift06--default-shop-checkout-"},
		} {
			got := GetChildNameWithHint("ai-gw", "gw", tc.routeNs, tc.routeName, "m", tc.surface, tc.rule)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("surface %q: got %q, want prefix %q", tc.surface, got, tc.wantPrefix)
			}
			if strings.Contains(got, "ako-gw-") {
				t.Errorf("readable name should not carry the ako-gw- prefix: %q", got)
			}
			if !lib.IsNameEncoded(got) {
				t.Errorf("IsNameEncoded(%q) = false, want true", got)
			}
		}
	})
}

// TestChildNameHintDoesNotAffectIdentity is the invariant that makes the hint safe:
// two routes that differ only in gateway must still get different names, even though
// the gateway is deliberately absent from the readable head.
func TestChildNameHintDoesNotAffectIdentity(t *testing.T) {
	withNaming(t, true, func() {
		a := GetChildNameWithHint("ai-gw", "gw-one", "mcp", "mcp-web", "m", SurfaceMCP, "tools")
		b := GetChildNameWithHint("ai-gw", "gw-two", "mcp", "mcp-web", "m", SurfaceMCP, "tools")
		if a == b {
			t.Fatalf("routes under different gateways collided on %q", a)
		}
		headA := a[:strings.LastIndex(a, "-")]
		headB := b[:strings.LastIndex(b, "-")]
		if headA != headB {
			t.Fatalf("expected identical readable heads, got %q and %q", headA, headB)
		}
	})
}

// TestGetRouteSurface pins the accepted vocabulary, including the protocol-flavoured
// aliases, and that anything unrecognised is treated as an ordinary route rather than
// being passed through into an object name.
func TestGetRouteSurface(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"llm", SurfaceLLM},
		{"inference", SurfaceLLM},
		{"MCP", SurfaceMCP},
		{" mcp ", SurfaceMCP},
		{"agent", SurfaceAgent},
		{"a2a", SurfaceAgent},
		{"A2A", SurfaceAgent},
		{"", ""},
		{"nonsense", ""},
		{"../evil", ""},
	} {
		if got := GetRouteSurface(map[string]string{SurfaceLabel: tc.in}); got != tc.want {
			t.Errorf("GetRouteSurface(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := GetRouteSurface(nil); got != "" {
		t.Errorf("GetRouteSurface(nil) = %q, want empty", got)
	}
	if got := GetRouteSurface(map[string]string{"other": "mcp"}); got != "" {
		t.Errorf("an unrelated label must not set a surface, got %q", got)
	}
}

// TestDefaultHTTPPSNameFollowsPrefix guards the one object whose name was built from
// the Prefix constant directly rather than from GetNamePrefix().
func TestDefaultHTTPPSNameFollowsPrefix(t *testing.T) {
	withNaming(t, false, func() {
		if got := GetDefaultHTTPPSName(); !strings.HasPrefix(got, "ako-gw-openshift06--") {
			t.Errorf("default httpps name = %q, want the ako-gw- prefix", got)
		}
	})
	withNaming(t, true, func() {
		if got := GetDefaultHTTPPSName(); !strings.HasPrefix(got, "openshift06--") ||
			strings.Contains(got, "ako-gw-") {
			t.Errorf("default httpps name = %q, want no ako-gw- prefix", got)
		}
	})
}

// TestPKIProfileNameAlwaysHashed pins the ako-crd-operator interop: that name mirrors
// what a separate module generates, so it must stay a bare digest even when this
// process has switched to readable names.
func TestPKIProfileNameAlwaysHashed(t *testing.T) {
	var hashed, readable string
	withNaming(t, false, func() { hashed = getPKIProfileName("avi-system", "my-pki") })
	withNaming(t, true, func() { readable = getPKIProfileName("avi-system", "my-pki") })
	if hashed != readable {
		t.Fatalf("pki profile name must not depend on the naming mode: %q vs %q", hashed, readable)
	}
	if strings.Contains(hashed, "my-pki") {
		t.Fatalf("pki profile name must be a bare digest, got %q", hashed)
	}
}
