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

// sumRatios returns the sum of all Ratio values in a WeightedPod slice.
func sumRatios(pods []WeightedPod) uint32 {
	var s uint32
	for _, p := range pods {
		s += p.Ratio
	}
	return s
}

func TestComputeWeights_Empty(t *testing.T) {
	result := ComputeWeights(nil, 1.0, 0.5)
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestComputeWeights_SinglePod(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.1, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].Ratio != 100 {
		t.Errorf("single pod should get ratio 100, got %d", result[0].Ratio)
	}
}

func TestComputeWeights_EqualLoad_RatiosSumTo100(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 3, KVCacheUsagePerc: 0.30, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 3, KVCacheUsagePerc: 0.30, Reachable: true},
		{PodIP: "10.0.0.3", NumRequestsWaiting: 3, KVCacheUsagePerc: 0.30, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0)
	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}
	total := sumRatios(result)
	if total != 100 {
		t.Errorf("ratios should sum to 100, got %d", total)
	}
	// Equal load → each pod should get roughly equal share (~33)
	for i, w := range result {
		if w.Ratio < 30 || w.Ratio > 37 {
			t.Errorf("pod[%d] ratio %d out of expected equal-load range [30,37]", i, w.Ratio)
		}
	}
}

func TestComputeWeights_RatiosAlwaysSumTo100(t *testing.T) {
	// Asymmetric load - rounding must still produce sum=100
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 30, KVCacheUsagePerc: 0.95, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 1, KVCacheUsagePerc: 0.10, Reachable: true},
		{PodIP: "10.0.0.3", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.05, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0)
	total := sumRatios(result)
	if total != 100 {
		t.Errorf("ratios should sum to 100, got %d (results: %v)", total, result)
	}
}

func TestComputeWeights_OverloadedPodGetsMinimumRatio(t *testing.T) {
	// Pod-1 is heavily loaded; it should get a much smaller ratio than pods 2 & 3
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 30, KVCacheUsagePerc: 0.95, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.05, Reachable: true},
		{PodIP: "10.0.0.3", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.05, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0)
	total := sumRatios(result)
	if total != 100 {
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
}

func TestComputeWeights_UnreachablePodGetsMinRatio(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", Reachable: false},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 2, KVCacheUsagePerc: 0.20, Reachable: true},
		{PodIP: "10.0.0.3", NumRequestsWaiting: 2, KVCacheUsagePerc: 0.20, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0)
	total := sumRatios(result)
	if total != 100 {
		t.Errorf("ratios should sum to 100, got %d", total)
	}
	if result[0].Ratio != minRatio {
		t.Errorf("unreachable pod should get minRatio=%d, got %d", minRatio, result[0].Ratio)
	}
	// Reachable pods should get significantly more traffic than the unreachable one
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
	result := ComputeWeights(metrics, 1.0, 0.0)
	for i, w := range result {
		if w.Ratio != minRatio {
			t.Errorf("unreachable pod[%d] should get minRatio=%d, got %d", i, minRatio, w.Ratio)
		}
	}
}

func TestComputeWeights_MinRatioEnforced(t *testing.T) {
	// Extremely high load on pod-1 should still give it at least minRatio=1
	metrics := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 10000, KVCacheUsagePerc: 1.0, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.0, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0)
	for i, w := range result {
		if w.Ratio < minRatio {
			t.Errorf("pod[%d] ratio %d is below minRatio=%d", i, w.Ratio, minRatio)
		}
		if w.Ratio > maxRatio {
			t.Errorf("pod[%d] ratio %d exceeds maxRatio=%d", i, w.Ratio, maxRatio)
		}
	}
	total := sumRatios(result)
	if total != 100 {
		t.Errorf("ratios should sum to 100, got %d", total)
	}
}

func TestComputeWeights_TokenRateContribution(t *testing.T) {
	// With beta>0, a high token rate should increase load → lower ratio
	metricsNoBeta := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.0, TotalTokensPerSec: 1000, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.0, TotalTokensPerSec: 0, Reachable: true},
	}
	metricsWithBeta := []PodMetrics{
		{PodIP: "10.0.0.1", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.0, TotalTokensPerSec: 1000, Reachable: true},
		{PodIP: "10.0.0.2", NumRequestsWaiting: 0, KVCacheUsagePerc: 0.0, TotalTokensPerSec: 0, Reachable: true},
	}
	noBeta := ComputeWeights(metricsNoBeta, 1.0, 0.0)
	withBeta := ComputeWeights(metricsWithBeta, 1.0, 2.0)

	// Without beta, token rate has no effect so ratios should be equal
	if noBeta[0].Ratio != noBeta[1].Ratio {
		// Both have waiting=0, kv=0; equal load → should be ~equal
		// (may differ by 1 due to rounding but not significantly)
		diff := int(noBeta[0].Ratio) - int(noBeta[1].Ratio)
		if diff > 1 || diff < -1 {
			t.Errorf("without beta, equal-load pods should have equal ratios, got %d vs %d",
				noBeta[0].Ratio, noBeta[1].Ratio)
		}
	}
	// With beta>0, high-throughput pod[0] has more load → smaller ratio
	if withBeta[0].Ratio >= withBeta[1].Ratio {
		t.Errorf("with beta, high-throughput pod[0] ratio %d should be less than idle pod[1] ratio %d",
			withBeta[0].Ratio, withBeta[1].Ratio)
	}
	if sumRatios(withBeta) != 100 {
		t.Errorf("ratios with beta should sum to 100, got %d", sumRatios(withBeta))
	}
}

func TestComputeWeights_PodIPsPreserved(t *testing.T) {
	metrics := []PodMetrics{
		{PodIP: "192.168.1.10", NumRequestsWaiting: 1, Reachable: true},
		{PodIP: "192.168.1.11", NumRequestsWaiting: 2, Reachable: true},
	}
	result := ComputeWeights(metrics, 1.0, 0.0)
	for i, w := range result {
		if w.PodIP != metrics[i].PodIP {
			t.Errorf("pod[%d] IP mismatch: expected %s, got %s", i, metrics[i].PodIP, w.PodIP)
		}
	}
}
