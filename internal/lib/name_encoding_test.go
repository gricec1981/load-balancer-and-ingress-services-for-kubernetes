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
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// withReadableNames runs fn with the readable object name flag set to enabled, and
// with a known cluster name so that GetNamePrefix() is deterministic.
func withReadableNames(t *testing.T, enabled bool, fn func()) {
	t.Helper()
	oldCluster := ClusterName
	oldPrefix := NamePrefix
	oldFlag := os.Getenv(USE_READABLE_OBJECT_NAMES)
	defer func() {
		ClusterName = oldCluster
		NamePrefix = oldPrefix
		os.Setenv(USE_READABLE_OBJECT_NAMES, oldFlag)
	}()

	ClusterName = "my-cluster"
	if enabled {
		os.Setenv(USE_READABLE_OBJECT_NAMES, "true")
	} else {
		os.Setenv(USE_READABLE_OBJECT_NAMES, "false")
	}
	SetNamePrefix("")
	fn()
}

// TestEncodeDefaultIsUnchanged pins the legacy behaviour: with the flag off, an
// encoded name is the prefix followed by the full 40 character SHA1 digest.
func TestEncodeDefaultIsUnchanged(t *testing.T) {
	withReadableNames(t, false, func() {
		name := EncodeWithPrefix(NamePrefix+"foo.example.com", EVHVS)
		if name != "my-cluster--e24f911e1705eda821eee091cc57d5cc16d685b7" {
			t.Fatalf("legacy encoding changed, got %q", name)
		}
		if !IsNameEncoded(name) {
			t.Fatalf("IsNameEncoded(%q) = false, want true", name)
		}
	})
}

// TestReadableNameShape covers the common case: a short input stays fully legible and
// carries the short hash as its last hyphen separated token.
func TestReadableNameShape(t *testing.T) {
	withReadableNames(t, true, func() {
		name := EncodeWithPrefix(NamePrefix+"foo.example.com", EVHVS)
		if !strings.HasPrefix(name, "my-cluster--foo.example.com-") {
			t.Fatalf("readable head missing or prefix duplicated, got %q", name)
		}
		if name != "my-cluster--foo.example.com-e24f911e" {
			t.Fatalf("unexpected readable name %q", name)
		}
		if !IsNameEncoded(name) {
			t.Fatalf("IsNameEncoded(%q) = false, want true", name)
		}
	})
}

// TestReadableNameSharesHashPrefixWithLegacy proves the short hash really is the head
// of the digest the legacy encoding uses, so both forms derive from the same input.
func TestReadableNameSharesHashPrefixWithLegacy(t *testing.T) {
	var legacy, readable string
	withReadableNames(t, false, func() {
		legacy = EncodeWithPrefix(NamePrefix+"default-foo.example.com_api-web-ing-web-svc", Pool)
	})
	withReadableNames(t, true, func() {
		readable = EncodeWithPrefix(NamePrefix+"default-foo.example.com_api-web-ing-web-svc", Pool)
	})
	legacyHash := strings.TrimPrefix(legacy, "my-cluster--")
	shortHash := readable[len(readable)-READABLE_NAME_HASH_LENGTH:]
	if !strings.HasPrefix(legacyHash, shortHash) {
		t.Fatalf("short hash %q is not a prefix of the full digest %q", shortHash, legacyHash)
	}
}

// TestReadableNameGatewayAPICaller covers the Gateway API call sites, which pass a
// name that does not already contain the prefix.
func TestReadableNameGatewayAPICaller(t *testing.T) {
	withReadableNames(t, true, func() {
		name := EncodeWithPrefix("gw-ns-my-gw-route-ns-my-route-1234", EVHVS)
		if !strings.HasPrefix(name, "my-cluster--gw-ns-my-gw-route-ns-my-route-1234-") {
			t.Fatalf("unexpected gateway api readable name %q", name)
		}
		if strings.Count(name, "--") != 1 {
			t.Fatalf("name %q must contain exactly one \"--\" delimiter", name)
		}
	})
}

// TestReadableNameRespectsLengthLimit is the reason the encoding exists at all: no
// generated name may exceed the Avi 255 character object name limit.
func TestReadableNameRespectsLengthLimit(t *testing.T) {
	withReadableNames(t, true, func() {
		longHost := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." +
			strings.Repeat("c", 63) + "." + strings.Repeat("d", 59)
		longPath := "_" + strings.Repeat("p", 200)
		input := NamePrefix + "a-very-long-namespace-name-" + longHost + longPath +
			"-a-very-long-ingress-name-and-a-very-long-service-name"
		name := EncodeWithPrefix(input, Pool)
		// Dedicated mode appends EVHSuffix after encoding, so the encoded name has to
		// leave room for it and still fit.
		if len(name+EVHSuffix) > AVI_OBJ_NAME_MAX_LENGTH {
			t.Fatalf("name length %d (%d with %s) exceeds the %d character limit: %q",
				len(name), len(name+EVHSuffix), EVHSuffix, AVI_OBJ_NAME_MAX_LENGTH, name)
		}
		if !IsNameEncoded(name) {
			t.Fatalf("IsNameEncoded(%q) = false, want true", name)
		}
		if !strings.HasPrefix(name, "my-cluster--a-very-long-namespace-name-") {
			t.Fatalf("truncation dropped the readable head: %q", name)
		}
	})
}

// TestReadableNameNeverContainsDoubleHyphen guards the "--" delimiter that both
// GetNamePrefix() and IsNameEncoded() depend on. Punycode hosts and namespaces are
// both allowed to contain "--" in Kubernetes.
func TestReadableNameNeverContainsDoubleHyphen(t *testing.T) {
	withReadableNames(t, true, func() {
		for _, input := range []string{
			NamePrefix + "xn--bcher-kva.example.com",
			NamePrefix + "my--namespace-foo.example.com",
			NamePrefix + "ns-host.com_a//b-ing",
			NamePrefix + "ns-host.com-!!!-ing",
		} {
			name := EncodeWithPrefix(input, EVHVS)
			if strings.Count(name, "--") != 1 {
				t.Errorf("name %q from %q must contain exactly one \"--\"", name, input)
			}
			if !IsNameEncoded(name) {
				t.Errorf("IsNameEncoded(%q) = false, want true", name)
			}
		}
	})
}

// TestReadableNameUniqueness checks that inputs which sanitise or truncate to the same
// readable head are still distinguished by the short hash.
func TestReadableNameUniqueness(t *testing.T) {
	withReadableNames(t, true, func() {
		a := EncodeWithPrefix(NamePrefix+"ns-host.com_a-b-ing", Pool)
		b := EncodeWithPrefix(NamePrefix+"ns-host.com_a/b-ing", Pool)
		if a == b {
			t.Fatalf("distinct inputs collided on %q", a)
		}
		if strings.TrimSuffix(a, a[len(a)-READABLE_NAME_HASH_LENGTH:]) !=
			strings.TrimSuffix(b, b[len(b)-READABLE_NAME_HASH_LENGTH:]) {
			t.Fatalf("expected identical readable heads for %q and %q", a, b)
		}
	})
}

// TestReadableNameIsStable guards against accidental non determinism: the same input
// must always produce the same name, or every reconcile would rename its objects.
func TestReadableNameIsStable(t *testing.T) {
	withReadableNames(t, true, func() {
		first := EncodeWithPrefix(NamePrefix+"default-foo.example.com", EVHVS)
		for i := 0; i < 10; i++ {
			if got := EncodeWithPrefix(NamePrefix+"default-foo.example.com", EVHVS); got != first {
				t.Fatalf("unstable encoding: %q then %q", first, got)
			}
		}
	})
}

// TestIsNameEncodedRejectsLegacyChildren is the upgrade contract. Names created before
// encoding was introduced must still be reported as not encoded, because
// listEVHChildrenToDelete uses that to garbage collect them.
func TestIsNameEncodedRejectsLegacyChildren(t *testing.T) {
	legacy := []string{
		"my-cluster--default-foo.example.com",
		"my-cluster--default-foo.example.com-myingress",
		"my-cluster--foo",
		"my-cluster--default--foo.example.com",
	}
	for _, readable := range []bool{false, true} {
		withReadableNames(t, readable, func() {
			for _, name := range legacy {
				if IsNameEncoded(name) {
					t.Errorf("readable=%v: IsNameEncoded(%q) = true, want false", readable, name)
				}
			}
		})
	}
}

// TestIsNameEncodedIgnoresShortHashWhenDisabled pins the guard that keeps the flag off
// behaviour identical: with readable names disabled, a trailing token that merely looks
// like a short hash - a service called "deadbeef", say - must not be treated as one, or
// the SNI regex paths would skip re-encoding a pool name they still need to encode.
func TestIsNameEncodedIgnoresShortHashWhenDisabled(t *testing.T) {
	withReadableNames(t, false, func() {
		if IsNameEncoded("my-cluster--default-foo.example.com-myingress-deadbeef") {
			t.Errorf("short hash form must not be recognised while the flag is off")
		}
	})
}

// TestIsNameEncodedShortHashLength pins the short hash to exactly
// READABLE_NAME_HASH_LENGTH hex characters, since a shorter or longer trailing token
// must not be mistaken for one.
func TestIsNameEncodedShortHashLength(t *testing.T) {
	if _, err := hex.DecodeString(strings.Repeat("a", READABLE_NAME_HASH_LENGTH)); err != nil {
		t.Fatalf("READABLE_NAME_HASH_LENGTH must be even to be valid hex: %v", err)
	}
	withReadableNames(t, true, func() {
		if IsNameEncoded("my-cluster--foo.example.com-abcdefa") {
			t.Errorf("a 7 character trailing token must not be treated as a short hash")
		}
		if IsNameEncoded("my-cluster--foo.example.com-abcdefabc") {
			t.Errorf("a 9 character trailing token must not be treated as a short hash")
		}
		if !IsNameEncoded("my-cluster--foo.example.com-abcdefab") {
			t.Errorf("an 8 character hex trailing token must be treated as a short hash")
		}
	})
}
