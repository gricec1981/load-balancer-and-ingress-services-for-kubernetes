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

package inference

import (
	"testing"
)

// roundRobinRatio3 is the ratio each of 3 equal pods receives under a naive
// round-robin policy (100 / 3, integer division).
const roundRobinRatio3 = 33

// sumRatios returns the sum of all Ratio values in a WeightedPod slice.
func sumRatios(pods []WeightedPod) uint32 {
	var s uint32
	for _, p := range pods {
		s += p.Ratio
	}
	return s
}

// ── Basic correctness ────────────────────────────────────────────────────────

func TestComputeWeights_Empty(t *testing.T) {
	result := ComputeWeights(nil, 1.0, 1.0, 0)
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestComputeWeights_SinglePod(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.1, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0, 0)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].Ratio != 100 {
		t.Errorf("single pod should get ratio 100, got %d", result[0].Ratio)
	}
}

func TestComputeWeights_RatiosAlwaysSumTo100(t *testing.T) {
	// Asymmetric load — rounding must still produce sum=100.
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 30, KVCacheUsagePerc: 0.95,
			WaitingSustainedStreak: 3, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 1, KVCacheUsagePerc: 0.10, Reachable: true},
		{PodIP: "10.0.0.3", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.05, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0, 0)
	if total := sumRatios(result); total != 100 {
		t.Errorf("ratios should sum to 100, got %d (results: %v)", total, result)
	}
}

func TestComputeWeights_MinRatioEnforced(t *testing.T) {
	// Extremely high load on pod-0 should still give it at least minRatio=1.
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 10000, KVCacheUsagePerc: 1.0,
			WaitingSustainedStreak: 5, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.0, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0, 0)
	for i, w := range result {
		if w.Ratio < minRatio {
			t.Errorf("pod[%d] ratio %d is below minRatio=%d", i, w.Ratio, minRatio)
		}
		if w.Ratio > maxRatio {
			t.Errorf("pod[%d] ratio %d exceeds maxRatio=%d", i, w.Ratio, maxRatio)
		}
	}
	if total := sumRatios(result); total != 100 {
		t.Errorf("ratios should sum to 100, got %d", total)
	}
}

func TestComputeWeights_PodIPsPreserved(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "192.168.1.10", NumRequestsWaiting: 1, Reachable: true},
		{PodIP: "192.168.1.11", NumRequestsWaiting: 2, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0, 0)
	for i, w := range result {
		if w.PodIP != metrics[i].PodIP {
			t.Errorf("pod[%d] IP mismatch: expected %s, got %s", i, metrics[i].PodIP, w.PodIP)
		}
	}
}

// ── Unreachable pods ─────────────────────────────────────────────────────────

func TestComputeWeights_UnreachablePodGetsMinRatio(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", Reachable: false},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 2, KVCacheUsagePerc: 0.20, Reachable: true},
		{PodIP: "10.0.0.3", NumRequestsWaiting: 2, KVCacheUsagePerc: 0.20, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0, 0)
	if total := sumRatios(result); total != 100 {
		t.Errorf("ratios should sum to 100, got %d", total)
	}
	if result[0].Ratio != minRatio {
		t.Errorf("unreachable pod should get minRatio=%d, got %d", minRatio, result[0].Ratio)
	}
	if result[1].Ratio <= result[0].Ratio {
		t.Errorf("reachable pod[1] ratio %d should exceed unreachable pod[0] ratio %d",
			result[1].Ratio, result[0].Ratio)
	}
}

func TestComputeWeights_AllUnreachable(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", Reachable: false},
		{PodIP: "10.0.0.2", Reachable: false},
		{PodIP: "10.0.0.3", Reachable: false},
	}
	result := ComputeWeights(metrics, 1.0, 0.0, 0)
	for i, w := range result {
		if w.Ratio != minRatio {
			t.Errorf("unreachable pod[%d] should get minRatio=%d, got %d", i, minRatio, w.Ratio)
		}
	}
}

// ── KV-cache signal ──────────────────────────────────────────────────────────

// TestComputeWeights_KVThreshold verifies the KV signal only fires above the
// 75% threshold. Below the threshold the overloaded pod scores the same as idle
// pods — weights are equal (round-robin behaviour). Above it the pod is penalised.
func TestComputeWeights_KVThreshold(t *testing.T) {
	below := []PodMetrics{
		{PodIP: "10.0.0.1", KVCacheUsagePerc: 0.74, Reachable: true}, // just below threshold
		{PodIP: "10.0.0.2", KVCacheUsagePerc: 0.00, Reachable: true},
	}
	above := []PodMetrics{
		{PodIP: "10.0.0.1", KVCacheUsagePerc: 0.76, Reachable: true}, // just above threshold
		{PodIP: "10.0.0.2", KVCacheUsagePerc: 0.00, Reachable: true},
	}

	resBelow := ComputeWeights(below, 1.0, 0.0, 0)
	resAbove := ComputeWeights(above, 1.0, 0.0, 0)

	// Below threshold: KV contributes nothing → equal weights (round-robin)
	if resBelow[0].Ratio != resBelow[1].Ratio {
		t.Errorf("below KV threshold: expected equal ratios, got %d vs %d",
			resBelow[0].Ratio, resBelow[1].Ratio)
	}
	// Above threshold: pod-0 is penalised → lower ratio
	if resAbove[0].Ratio >= resAbove[1].Ratio {
		t.Errorf("above KV threshold: stressed pod[0] ratio %d should be less than idle pod[1] ratio %d",
			resAbove[0].Ratio, resAbove[1].Ratio)
	}
}

func TestComputeWeights_HighKVReducesRatio(t *testing.T) {
	// Pod-0 KV at full capacity; pods 1 & 2 idle.
	// Expects pod-0 to receive less traffic than round-robin's ~33.
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", KVCacheUsagePerc: 1.0, Reachable: true},
		{PodIP: "10.0.0.2", KVCacheUsagePerc: 0.0, Reachable: true},
		{PodIP: "10.0.0.3", KVCacheUsagePerc: 0.0, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0, 0)
	if sumRatios(result) != 100 {
		t.Fatalf("ratios should sum to 100, got %d", sumRatios(result))
	}
	if result[0].Ratio >= roundRobinRatio3 {
		t.Errorf("full-KV pod[0] ratio %d should be below round-robin (%d)", result[0].Ratio, roundRobinRatio3)
	}
	if result[1].Ratio <= roundRobinRatio3 {
		t.Errorf("idle pod[1] ratio %d should exceed round-robin (%d)", result[1].Ratio, roundRobinRatio3)
	}
	t.Logf("KV-only (α=1): %d/%d/%d vs round-robin 33/33/33", result[0].Ratio, result[1].Ratio, result[2].Ratio)
}

// ── Waiting-queue signal ─────────────────────────────────────────────────────

// TestComputeWeights_WaitingRequiresSustainedStreak verifies that a transient
// single-cycle queue spike (streak < 2) does NOT affect routing — the formula
// degrades to round-robin until the queue has been sustained.
func TestComputeWeights_WaitingRequiresSustainedStreak(t *testing.T) {
	transient := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 50, WaitingSustainedStreak: 1, Reachable: true}, // streak too short
		{PodIP: "10.0.0.2", NumRequestsWaiting: 0, WaitingSustainedStreak: 0, Reachable: true},
	}
	sustained := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 50, WaitingSustainedStreak: 2, Reachable: true}, // streak meets threshold
		{PodIP: "10.0.0.2", NumRequestsWaiting: 0, WaitingSustainedStreak: 0, Reachable: true},
	}

	resTransient := ComputeWeights(transient, 1.0, 0.0, 0)
	resSustained := ComputeWeights(sustained, 1.0, 0.0, 0)

	// Transient spike: waiting term is suppressed → equal weights (round-robin)
	if resTransient[0].Ratio != resTransient[1].Ratio {
		t.Errorf("transient spike: expected equal ratios (round-robin), got %d vs %d",
			resTransient[0].Ratio, resTransient[1].Ratio)
	}
	// Sustained queue: waiting term fires → pod-0 penalised
	if resSustained[0].Ratio >= resSustained[1].Ratio {
		t.Errorf("sustained queue: stressed pod[0] ratio %d should be less than idle pod[1] ratio %d",
			resSustained[0].Ratio, resSustained[1].Ratio)
	}
}

// ── Slot-utilisation signal (beta) ───────────────────────────────────────────

// TestComputeWeights_SlotUtilisationContribution verifies the beta / slot term.
// beta weights NumRequestsRunning / maxNumSeqs, not token throughput.
func TestComputeWeights_SlotUtilisationContribution(t *testing.T) {
	const maxSeqs = 128.0

	// Pod-0 runs at full slot capacity; pod-1 is idle.
	noBeta := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsRunning: 128, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsRunning: 0, Reachable: true},
	}
	withBeta := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsRunning: 128, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsRunning: 0, Reachable: true},
	}

	resNoBeta := ComputeWeights(noBeta, 1.0, 0.0, maxSeqs)
	resWithBeta := ComputeWeights(withBeta, 1.0, 1.0, maxSeqs)

	// beta=0: slot term disabled → pod-0 and pod-1 look identical → equal ratios
	if resNoBeta[0].Ratio != resNoBeta[1].Ratio {
		t.Errorf("beta=0: expected equal ratios, got %d vs %d",
			resNoBeta[0].Ratio, resNoBeta[1].Ratio)
	}
	// beta=1.0: fully-utilised pod-0 → higher load → lower ratio
	if resWithBeta[0].Ratio >= resWithBeta[1].Ratio {
		t.Errorf("beta=1.0: full-slot pod[0] ratio %d should be less than idle pod[1] ratio %d",
			resWithBeta[0].Ratio, resWithBeta[1].Ratio)
	}
	if total := sumRatios(resWithBeta); total != 100 {
		t.Errorf("ratios should sum to 100, got %d", total)
	}
	t.Logf("slot-only (β=1, maxSeqs=128): %d/%d vs round-robin 50/50",
		resWithBeta[0].Ratio, resWithBeta[1].Ratio)
}

// ── Intelligent routing vs round-robin ──────────────────────────────────────

// TestComputeWeights_IntelligentVsRoundRobin is the headline demo test: it
// quantifies the maximum divergence from round-robin achievable with all three
// signals firing together on one pod.
//
// Setup (3 pods, α=β=1.0, maxNumSeqs=128):
//
//	Pod-0 (overloaded): waiting=50 (streak≥2), KV=1.0, running=128
//	  waitingLoad = 50/50 = 1.0
//	  kvLoad      = 1.0 × (1.0−0.75)/(1−0.75) = 1.0
//	  slotLoad    = 1.0 × 128/128 = 1.0
//	  total load  = 3.0  →  score = 1/(3+1) = 0.25
//
//	Pods 1 & 2 (idle): all signals zero → score = 1/(0+1) = 1.0
//
//	Ratios: pod0 ≈ 11  |  pod1/2 ≈ 44–45  (sum = 100)
//
// Round-robin would give 33/33/33.
// The intelligent formula delivers ~4× more traffic to healthy pods.
func TestComputeWeights_IntelligentVsRoundRobin(t *testing.T) {
	const maxSeqs = 128.0

	metrics := []PodMetrics{
		{
			PodIP:                  "10.0.0.1",
			NumRequestsWaiting:     50,
			WaitingSustainedStreak: 3, // >= waitingSustainedScrapes (2)
			KVCacheUsagePerc:       1.0,
			NumRequestsRunning:     128,
			Reachable:              true,
		},
		{PodIP: "10.0.0.2", Reachable: true},
		{PodIP: "10.0.0.3", Reachable: true},
	}

	result := ComputeWeights(metrics, 1.0, 1.0, maxSeqs)

	if total := sumRatios(result); total != 100 {
		t.Fatalf("ratios should sum to 100, got %d", total)
	}

	overloaded := int(result[0].Ratio)
	idle1 := int(result[1].Ratio)
	idle2 := int(result[2].Ratio)

	// Overloaded pod must receive substantially less traffic than round-robin's 33.
	if overloaded >= roundRobinRatio3 {
		t.Errorf("overloaded pod ratio %d should be well below round-robin (%d)",
			overloaded, roundRobinRatio3)
	}
	// Idle pods must receive substantially more traffic than round-robin's 33.
	if idle1 <= roundRobinRatio3 {
		t.Errorf("idle pod[1] ratio %d should exceed round-robin (%d)", idle1, roundRobinRatio3)
	}
	if idle2 <= roundRobinRatio3 {
		t.Errorf("idle pod[2] ratio %d should exceed round-robin (%d)", idle2, roundRobinRatio3)
	}
	// Healthy pods should receive at least 3× more traffic than the stressed pod.
	if idle1 < 3*overloaded || idle2 < 3*overloaded {
		t.Errorf("idle pods (%d, %d) should get at least 3× the traffic of the overloaded pod (%d)",
			idle1, idle2, overloaded)
	}

	t.Logf("round-robin baseline:   33 / 33 / 33")
	t.Logf("intelligent routing:    %2d / %2d / %2d  (overloaded / idle / idle)",
		overloaded, idle1, idle2)
	t.Logf("traffic gain to idle pods: %.1f×", float64(idle1)/float64(overloaded))
}

// TestComputeWeights_EqualLoad_RatiosSumTo100 proves that when all pods carry
// identical load the formula degrades to round-robin (equal weights).
func TestComputeWeights_EqualLoad_RatiosSumTo100(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 3, KVCacheUsagePerc: 0.30, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 3, KVCacheUsagePerc: 0.30, Reachable: true},
		{PodIP: "10.0.0.3", NumRequestsWaiting: 3, KVCacheUsagePerc: 0.30, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0, 0)
	if total := sumRatios(result); total != 100 {
		t.Fatalf("ratios should sum to 100, got %d", total)
	}
	for i, w := range result {
		if w.Ratio < 30 || w.Ratio > 37 {
			t.Errorf("pod[%d] ratio %d out of expected equal-load range [30,37]", i, w.Ratio)
		}
	}
}

// TestComputeWeights_OverloadedPodGetsMinimumRatio checks the directional
// ordering: a pod with high KV and a sustained waiting queue should receive
// significantly less traffic than idle pods.
func TestComputeWeights_OverloadedPodGetsMinimumRatio(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 30, WaitingSustainedStreak: 3,
			KVCacheUsagePerc: 0.95, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.05, Reachable: true},
		{PodIP: "10.0.0.3", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.05, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0, 0)
	if total := sumRatios(result); total != 100 {
		t.Errorf("ratios should sum to 100, got %d", total)
	}
	if result[0].Ratio >= result[1].Ratio {
		t.Errorf("overloaded pod[0] ratio %d should be less than idle pod[1] ratio %d",
			result[0].Ratio, result[1].Ratio)
	}
	if result[0].Ratio >= result[2].Ratio {
		t.Errorf("overloaded pod[0] ratio %d should be less than idle pod[2] ratio %d",
			result[0].Ratio, result[2].Ratio)
	}
	t.Logf("overloaded vs idle: %d / %d / %d (round-robin would be 33/33/33)",
		result[0].Ratio, result[1].Ratio, result[2].Ratio)
}

// TestComputeWeights_ScenarioTable runs a grid of realistic load scenarios and
// logs exact ratios so the output serves as a deterministic benchmark table.
// Run with:  go test -v -run TestComputeWeights_ScenarioTable ./ako-gateway-api/inference/
//
// No GPU or external infrastructure is needed — ComputeWeights is pure math.
func TestComputeWeights_ScenarioTable(t *testing.T) {
	const maxSeqs = 128.0

	type scenario struct {
		name     string
		metrics  []PodMetrics
		alpha    float64
		beta     float64
		wantDesc string // what makes this scenario interesting
	}

	scenarios := []scenario{
		{
			name:     "Baseline: all pods idle (equal load)",
			alpha:    1.0, beta: 1.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "should equal round-robin 33/33/33",
		},
		{
			name:     "KV cache: pod-0 at 80% (just above threshold)",
			alpha:    1.0, beta: 0.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", KVCacheUsagePerc: 0.80, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "mild KV pressure",
		},
		{
			name:     "KV cache: pod-0 at 85%",
			alpha:    1.0, beta: 0.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", KVCacheUsagePerc: 0.85, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "moderate KV pressure",
		},
		{
			name:     "KV cache: pod-0 at 90%",
			alpha:    1.0, beta: 0.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", KVCacheUsagePerc: 0.90, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "high KV pressure",
		},
		{
			name:     "KV cache: pod-0 at 100% (full)",
			alpha:    1.0, beta: 0.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", KVCacheUsagePerc: 1.00, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "KV saturated",
		},
		{
			name:     "Queue only: pod-0 has sustained queue (streak=2)",
			alpha:    1.0, beta: 0.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", NumRequestsWaiting: 20, WaitingSustainedStreak: 2, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "queue depth normalised to 1.0 (only pod with queue)",
		},
		{
			name:     "Slot utilisation only: pod-0 at 50% capacity",
			alpha:    0.0, beta: 1.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", NumRequestsRunning: 64, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "slot signal only, half capacity",
		},
		{
			name:     "Slot utilisation only: pod-0 at 100% capacity",
			alpha:    0.0, beta: 1.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", NumRequestsRunning: 128, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "slot signal only, fully saturated",
		},
		{
			name:     "KV + queue: pod-0 at 100% KV with sustained queue",
			alpha:    1.0, beta: 0.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", KVCacheUsagePerc: 1.0, NumRequestsWaiting: 20,
					WaitingSustainedStreak: 3, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "two signals: KV + queue",
		},
		{
			name:     "All 3 signals: pod-0 fully overloaded (unit test scenario)",
			alpha:    1.0, beta: 1.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", KVCacheUsagePerc: 1.0, NumRequestsWaiting: 50,
					WaitingSustainedStreak: 3, NumRequestsRunning: 128, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "all three signals maxed",
		},
		{
			name:     "Graduated: pod-0 heavy, pod-1 medium, pod-2 idle",
			alpha:    1.0, beta: 1.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", KVCacheUsagePerc: 1.0, NumRequestsWaiting: 20,
					WaitingSustainedStreak: 3, NumRequestsRunning: 128, Reachable: true},
				{PodIP: "pod-1", KVCacheUsagePerc: 0.85, NumRequestsRunning: 64, Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "graduated load: heavy/medium/idle",
		},
		{
			name:     "Transient spike: queue not yet sustained (streak=1)",
			alpha:    1.0, beta: 0.0,
			metrics: []PodMetrics{
				{PodIP: "pod-0", NumRequestsWaiting: 50, WaitingSustainedStreak: 1, Reachable: true},
				{PodIP: "pod-1", Reachable: true},
				{PodIP: "pod-2", Reachable: true},
			},
			wantDesc: "transient spike suppressed → should equal round-robin",
		},
	}

	t.Log("")
	t.Log("┌─────────────────────────────────────────────────────────────────────────────────┐")
	t.Log("│            ComputeWeights scenario table  (round-robin baseline = 33/33/33)     │")
	t.Log("├──────────────────────────────────────────────────────┬──────────┬───────────────┤")
	t.Log("│ Scenario                                             │ Ratios   │ RR reduction  │")
	t.Log("├──────────────────────────────────────────────────────┼──────────┼───────────────┤")

	for _, s := range scenarios {
		result := ComputeWeights(s.metrics, s.alpha, s.beta, maxSeqs)
		if len(result) != 3 {
			t.Errorf("%s: expected 3 results, got %d", s.name, len(result))
			continue
		}
		r0, r1, r2 := result[0].Ratio, result[1].Ratio, result[2].Ratio
		rrReduction := float64(roundRobinRatio3-int(r0)) / float64(roundRobinRatio3) * 100

		if total := sumRatios(result); total != 100 {
			t.Errorf("%s: ratios sum to %d, want 100", s.name, total)
		}

		label := s.name
		if len(label) > 52 {
			label = label[:49] + "..."
		}
		t.Logf("│ %-52s │ %2d/%2d/%2d │ %+.0f%%           │",
			label, r0, r1, r2, rrReduction)
	}

	t.Log("└──────────────────────────────────────────────────────┴──────────┴───────────────┘")
	t.Log("")
	t.Log("  pod-0 = stressed pod    pod-1/2 = idle pods    RR reduction = % less traffic to stressed pod vs round-robin")
}

// TestComputeWeights_AllSignalsCombined verifies that enabling all three signals
// together produces greater skew than any single signal alone.
func TestComputeWeights_AllSignalsCombined(t *testing.T) {
	const maxSeqs = 128.0

	kvOnly := []PodMetrics{
		{PodIP: "10.0.0.1", KVCacheUsagePerc: 1.0, Reachable: true},
		{PodIP: "10.0.0.2", Reachable: true},
	}
	allSignals := []PodMetrics{
		{
			PodIP:                  "10.0.0.1",
			KVCacheUsagePerc:       1.0,
			NumRequestsWaiting:     50,
			WaitingSustainedStreak: 3,
			NumRequestsRunning:     128,
			Reachable:              true,
		},
		{PodIP: "10.0.0.2", Reachable: true},
	}

	resKVOnly := ComputeWeights(kvOnly, 1.0, 0.0, maxSeqs)
	resAll := ComputeWeights(allSignals, 1.0, 1.0, maxSeqs)

	// All-signals combo should penalise pod-0 more than KV alone.
	if resAll[0].Ratio >= resKVOnly[0].Ratio {
		t.Errorf("combined signals pod[0] ratio %d should be lower than KV-only ratio %d",
			resAll[0].Ratio, resKVOnly[0].Ratio)
	}
	t.Logf("KV-only:      pod0=%d pod1=%d", resKVOnly[0].Ratio, resKVOnly[1].Ratio)
	t.Logf("All signals:  pod0=%d pod1=%d  (greater skew with combined signals)", resAll[0].Ratio, resAll[1].Ratio)
}
