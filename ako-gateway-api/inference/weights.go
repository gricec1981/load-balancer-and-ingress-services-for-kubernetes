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

import "math"

const (
	// epsilon prevents division by zero when all pods are idle.
	epsilon = 1.0
	// minRatio is the minimum Avi PoolGroupMember.Ratio value (Avi enforces >= 1).
	minRatio = uint32(1)
	// maxRatio is the maximum Avi PoolGroupMember.Ratio value.
	maxRatio = uint32(100)
	// kvThreshold is the KV-cache occupancy fraction above which the KV signal
	// begins to contribute load. Below this level the cache is not considered
	// a bottleneck.
	kvThreshold = 0.75
	// waitingSustainedScrapes is the minimum number of consecutive scrape
	// cycles with NumRequestsWaiting > 0 before the waiting term is activated.
	// Transient single-cycle spikes are ignored.
	waitingSustainedScrapes = 2
	// defaultMaxNumSeqs is the fallback maximum concurrent sequence capacity
	// when the InferencePool annotation is absent or zero.
	defaultMaxNumSeqs = 256.0
)

// ComputeWeights converts a slice of PodMetrics into Avi pool group member
// ratios using an inverse-load scoring function with three additive terms:
//
//	waitingLoad  = NumRequestsWaiting / maxWaiting_across_pods
//	                 (only when WaitingSustainedStreak >= waitingSustainedScrapes;
//	                  transient single-cycle spikes are ignored)
//
//	kvLoad       = alpha * (KVCacheUsagePerc - kvThreshold) / (1 - kvThreshold)
//	                 (only when KVCacheUsagePerc > kvThreshold=0.75;
//	                  zero below the threshold, ramps to alpha at full occupancy)
//
//	slotLoad     = beta * clamp(NumRequestsRunning / maxNumSeqs, 0, 1)
//	                 (slot utilisation normalised by the model's max capacity)
//
//	load(pod)    = waitingLoad + kvLoad + slotLoad
//	score(pod)   = 1 / (load + epsilon)
//	ratio(pod)   = round(100 * score / sum(scores))
//
// maxNumSeqs is the model's maximum concurrent sequence capacity. Pass 0 (or
// omit the InferencePool annotation) to use the built-in default of 256.
//
// When maxWaiting is zero (no pod has a queue) or the sustained streak is too
// short, the waiting term contributes 0 so idle pools produce near-equal ratios.
//
// Pods that are unreachable receive the minimum ratio (1) so they still
// receive some traffic while health monitors decide whether to remove them.
//
// The ratios always sum to 100 (with rounding adjustment on the highest-score
// pod to absorb any remainder).
func ComputeWeights(metrics []PodMetrics, alpha, beta, maxNumSeqs float64) []WeightedPod {
	if len(metrics) == 0 {
		return nil
	}

	if maxNumSeqs <= 0 {
		maxNumSeqs = defaultMaxNumSeqs
	}

	// Single pass: find maxWaiting across reachable pods for normalisation.
	// When maxWaiting is zero (no pod has a queue) the waiting term is zeroed
	// so idle pools still produce near-equal ratios.
	maxWaiting := 0.0
	for _, m := range metrics {
		if m.Reachable && m.NumRequestsWaiting > maxWaiting {
			maxWaiting = m.NumRequestsWaiting
		}
	}

	scores := make([]float64, len(metrics))
	totalScore := 0.0

	for i, m := range metrics {
		if !m.Reachable {
			// Unreachable pods get a zero score so they still appear in the PG
			// at minRatio until Avi's health monitor marks them down.
			scores[i] = 0
			continue
		}

		// Streak-gated waiting: only count the queue depth once it has been
		// sustained for at least waitingSustainedScrapes consecutive cycles,
		// filtering out transient single-cycle bursts.
		waitingLoad := 0.0
		if m.WaitingSustainedStreak >= waitingSustainedScrapes && maxWaiting > 0 {
			waitingLoad = m.NumRequestsWaiting / maxWaiting
		}

		// Threshold-ramped KV cache: contributes load only above kvThreshold,
		// ramping linearly from 0 at the threshold to alpha at full occupancy.
		kvLoad := 0.0
		if m.KVCacheUsagePerc > kvThreshold {
			kvLoad = alpha * (m.KVCacheUsagePerc - kvThreshold) / (1.0 - kvThreshold)
		}

		// Slot utilisation: running requests normalised by max model capacity.
		// Clamped to [0, 1] before applying beta so an overloaded pod never
		// produces a negative score contribution.
		slotLoad := 0.0
		if beta > 0 {
			util := m.NumRequestsRunning / maxNumSeqs
			if util > 1.0 {
				util = 1.0
			}
			slotLoad = beta * util
		}

		load := waitingLoad + kvLoad + slotLoad
		scores[i] = 1.0 / (load + epsilon)
		totalScore += scores[i]
	}

	weights := make([]WeightedPod, len(metrics))
	assignedTotal := uint32(0)
	maxScoreIdx := 0

	for i, m := range metrics {
		weights[i].PodIP = m.PodIP
		if !m.Reachable || totalScore == 0 {
			weights[i].Ratio = minRatio
			assignedTotal += minRatio
			continue
		}
		raw := math.Round(100.0 * scores[i] / totalScore)
		ratio := uint32(raw)
		if ratio < minRatio {
			ratio = minRatio
		}
		if ratio > maxRatio {
			ratio = maxRatio
		}
		weights[i].Ratio = ratio
		assignedTotal += ratio
		if scores[i] > scores[maxScoreIdx] {
			maxScoreIdx = i
		}
	}

	// Adjust the pod with the highest score to absorb rounding error so the
	// total sums to exactly 100.
	if assignedTotal != 100 && weights[maxScoreIdx].Ratio > minRatio {
		diff := int32(100) - int32(assignedTotal)
		adjusted := int32(weights[maxScoreIdx].Ratio) + diff
		if adjusted < int32(minRatio) {
			adjusted = int32(minRatio)
		}
		weights[maxScoreIdx].Ratio = uint32(adjusted)
	}

	return weights
}
