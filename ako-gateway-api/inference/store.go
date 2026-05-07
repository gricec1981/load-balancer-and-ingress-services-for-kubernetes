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

import "sync"

// WeightStore is a thread-safe store mapping poolNsName → podIP → ratio.
// It is written by the Scraper after each scrape cycle and read by the
// graph layer when constructing Avi Pool Group members.
type WeightStore struct {
	mu    sync.RWMutex
	store map[string]map[string]uint32 // poolNsName → podIP → ratio
}

var globalWeightStore = &WeightStore{
	store: make(map[string]map[string]uint32),
}

// GlobalWeightStore returns the process-wide singleton weight store.
func GlobalWeightStore() *WeightStore {
	return globalWeightStore
}

// Set stores the latest weights for a pool.
func (w *WeightStore) Set(poolNsName string, weights []WeightedPod) {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := make(map[string]uint32, len(weights))
	for _, wp := range weights {
		m[wp.PodIP] = wp.Ratio
	}
	w.store[poolNsName] = m
}

// GetRatio returns the current ratio for a specific pod within a pool.
// Falls back to 1 if no weight is stored for the pod.
func (w *WeightStore) GetRatio(poolNsName, podIP string) uint32 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if pods, ok := w.store[poolNsName]; ok {
		if ratio, ok := pods[podIP]; ok {
			return ratio
		}
	}
	return 1
}

// GetWeights returns all pod weights for a pool. Returns nil if not found.
func (w *WeightStore) GetWeights(poolNsName string) []WeightedPod {
	w.mu.RLock()
	defer w.mu.RUnlock()
	pods, ok := w.store[poolNsName]
	if !ok {
		return nil
	}
	result := make([]WeightedPod, 0, len(pods))
	for ip, ratio := range pods {
		result = append(result, WeightedPod{PodIP: ip, Ratio: ratio})
	}
	return result
}

// Delete removes the stored weights for a pool.
func (w *WeightStore) Delete(poolNsName string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.store, poolNsName)
}
