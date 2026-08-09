/*
 * Copyright © 2026 Broadcom Inc. and/or its subsidiaries. All Rights Reserved.
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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func discPod(ns, name, alias, tier string, created time.Time) *corev1.Pod {
	p := &corev1.Pod{}
	p.Namespace = ns
	p.Name = name
	p.CreationTimestamp = metav1.NewTime(created)
	p.Labels = map[string]string{}
	p.Annotations = map[string]string{}
	if alias != "" {
		p.Annotations[DefaultAliasAnnotation] = alias
	}
	if tier != "" {
		p.Labels[DefaultTierLabel] = tier
	}
	return p
}

var discKnownTiers = map[string]bool{"fast": true, "quality": true}

func runMerge(t *testing.T, static map[string]string, pods []*corev1.Pod) (map[string]string, []string) {
	t.Helper()
	return mergeDiscoveredModels(discKnownTiers, static, pods, DefaultAliasAnnotation, DefaultTierLabel)
}

func TestDiscoveryMergesLabeledPods(t *testing.T) {
	t0 := time.Now()
	got, warnings := runMerge(t, nil, []*corev1.Pod{
		discPod("inference", "m1", "smol", "fast", t0),
		discPod("inference", "m2", "big", "quality", t0),
		discPod("inference", "unlabeled", "", "", t0), // no alias: ignored silently
	})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if got["smol"] != "fast" || got["big"] != "quality" || len(got) != 2 {
		t.Fatalf("unexpected merge result: %v", got)
	}
}

func TestDiscoveryReplicasOfOneModelMergeSilently(t *testing.T) {
	t0 := time.Now()
	got, warnings := runMerge(t, nil, []*corev1.Pod{
		discPod("inference", "m1-a", "smol", "fast", t0),
		discPod("inference", "m1-b", "smol", "fast", t0.Add(time.Minute)),
	})
	if len(warnings) != 0 {
		t.Fatalf("same-tier replicas must not warn: %v", warnings)
	}
	if got["smol"] != "fast" || len(got) != 1 {
		t.Fatalf("unexpected merge result: %v", got)
	}
}

func TestDiscoveryStaticEntryWins(t *testing.T) {
	got, warnings := runMerge(t,
		map[string]string{"smol": "quality"}, // admin says quality
		[]*corev1.Pod{discPod("inference", "m1", "smol", "fast", time.Now())},
	)
	if len(got) != 0 {
		t.Fatalf("discovered alias must not override static entry: %v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "static wins") {
		t.Fatalf("expected a static-wins warning, got: %v", warnings)
	}
}

func TestDiscoveryUnknownTierIgnored(t *testing.T) {
	got, warnings := runMerge(t, nil, []*corev1.Pod{
		discPod("inference", "m1", "smol", "made-up-tier", time.Now()),
	})
	if len(got) != 0 {
		t.Fatalf("undeclared tier must not register: %v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "undeclared tier") {
		t.Fatalf("expected an undeclared-tier warning, got: %v", warnings)
	}
}

func TestDiscoveryMissingTierLabelIgnored(t *testing.T) {
	got, warnings := runMerge(t, nil, []*corev1.Pod{
		discPod("inference", "m1", "smol", "", time.Now()),
	})
	if len(got) != 0 {
		t.Fatalf("alias without tier label must not register: %v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no") {
		t.Fatalf("expected a missing-label warning, got: %v", warnings)
	}
}

func TestDiscoveryGlobAliasRejected(t *testing.T) {
	got, warnings := runMerge(t, nil, []*corev1.Pod{
		discPod("inference", "m1", "smol*", "fast", time.Now()),
	})
	if len(got) != 0 {
		t.Fatalf("glob alias must not register: %v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "glob") {
		t.Fatalf("expected a glob warning, got: %v", warnings)
	}
}

func TestDiscoveryCollisionOldestWins(t *testing.T) {
	t0 := time.Now()
	got, warnings := runMerge(t, nil, []*corev1.Pod{
		// Newer pod listed first: sort order, not slice order, must decide.
		discPod("inference", "newer", "smol", "quality", t0.Add(time.Hour)),
		discPod("inference", "older", "smol", "fast", t0),
	})
	if got["smol"] != "fast" {
		t.Fatalf("oldest claimant must win, got: %v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "already claimed") {
		t.Fatalf("expected a collision warning, got: %v", warnings)
	}
}

func TestDiscoveryTerminatingPodIgnored(t *testing.T) {
	p := discPod("inference", "m1", "smol", "fast", time.Now())
	now := metav1.Now()
	p.DeletionTimestamp = &now
	got, warnings := runMerge(t, nil, []*corev1.Pod{p})
	if len(got) != 0 || len(warnings) != 0 {
		t.Fatalf("terminating pod must be ignored silently, got %v / %v", got, warnings)
	}
}

func TestDiscoveryNamespaceDefaultsAndScoping(t *testing.T) {
	d := &ModelDiscovery{Enabled: true}
	if !d.AllowsNamespace("inference", "inference") {
		t.Fatal("default scope must allow the policy's own namespace")
	}
	if d.AllowsNamespace("inference", "other") {
		t.Fatal("default scope must not allow other namespaces")
	}
	d.Namespaces = []string{"models-a", "models-b"}
	if d.AllowsNamespace("inference", "inference") {
		t.Fatal("explicit scope must replace the default, not extend it")
	}
	if !d.AllowsNamespace("inference", "models-b") {
		t.Fatal("explicit scope must allow listed namespaces")
	}
}

func TestDiscoveryDisabledIsIdentity(t *testing.T) {
	p := &AIModelRoutePolicy{}
	p.Namespace = "inference"
	p.Name = "llm-tiers"
	p.Spec.Tiers = []ModelTier{{Name: "fast"}}
	p.Spec.DefaultTier = "fast"
	p.Spec.ModelTiers = map[string]string{"a": "fast"}
	// Discovery nil → the exact same pointer comes back, no copy, no informer use.
	if got := WithDiscoveredModels("test", p); got != p {
		t.Fatal("discovery off must return the original policy unchanged")
	}
}
