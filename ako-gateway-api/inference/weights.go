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
)

// ComputeWeights converts a slice of PodMetrics into Avi pool group member
// ratios using an inverse-load scoring function:
//
//	load(pod)  = (waiting / maxWaiting_across_pods)
//	           + alpha * kv_cache_perc
//	           + beta  * (totalTokensPerSec / maxTokensPerSec_across_pods)
//	score(pod) = 1 / (load + epsilon)
//	ratio(pod) = round(100 * score / sum(scores))
//
// All three terms are normalised against the pool maximum so each lives on
// [0, 1]. This makes alpha and beta directly comparable to the queue-depth
// signal: a value of 1.0 for any coefficient weights that signal equally with
// a fully-saturated waiting queue or a fully-loaded token pipeline.
//
// maxNumSeqs is the model's maximum concurrent sequence capacity (e.g. 256).
// It is used by callers to normalise NumRequestsRunning and to interpret
// WaitingSustainedStreak. Pass 0 to use a built-in default (256).
//
// When the pool-wide maximum of a signal is zero (e.g. no pod has any queued
// requests) that term contributes 0 to avoid division by zero, matching the
// idle-pool behaviour of the previous formula.
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

	// Single pass: find the per-signal pool maxima across reachable pods for
	// normalisation. When a maximum is zero the corresponding term is zeroed
	// (div-by-zero guard) so idle pools still produce near-equal ratios.
	maxTokenRate := 0.0
	maxWaiting := 0.0
	for _, m := range metrics {
		if !m.Reachable {
			continue
		}
		if m.TotalTokensPerSec > maxTokenRate {
			maxTokenRate = m.TotalTokensPerSec
		}
		if m.NumRequestsWaiting > maxWaiting {
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

		// Normalised waiting queue depth: high queue → high load → lower score.
		waitingLoad := 0.0
		if maxWaiting > 0 {
			waitingLoad = m.NumRequestsWaiting / maxWaiting
		}

		// Normalised token throughput: high throughput → high load → lower score.
		tokenLoad := 0.0
		if beta > 0 && maxTokenRate > 0 {
			tokenLoad = beta * (m.TotalTokensPerSec / maxTokenRate)
		}

		load := waitingLoad + alpha*m.KVCacheUsagePerc + tokenLoad
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
