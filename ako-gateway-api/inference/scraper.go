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
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

const (
	// Gauge metrics — instantaneous values.
	metricRequestsRunning = "vllm:num_requests_running"
	metricRequestsWaiting = "vllm:num_requests_waiting"
	metricKVCacheUsage    = "vllm:kv_cache_usage_perc"

	// Counter metrics — cumulative totals used to compute per-interval rates.
	metricGenerationTokensTotal = "vllm:generation_tokens_total"
	metricPromptTokensTotal     = "vllm:prompt_tokens_total"

	// defaultScrapeInterval is the fallback when not set via config.
	defaultScrapeInterval = 15 * time.Second
	// maxJitterMs is the maximum random jitter added per pod scrape to avoid thundering herd.
	maxJitterMs = 2000
	// scrapeTimeout is the per-pod HTTP request timeout.
	scrapeTimeout = 5 * time.Second
)

// OnWeightsUpdated is called by the Scraper after each full scrape cycle
// with the recomputed weights for a given pool. Implementations should
// re-enqueue the associated HTTPRoute key for graph-layer processing.
type OnWeightsUpdated func(poolNsName string, weights []WeightedPod)

// Scraper manages one scrape goroutine per InferencePool. It periodically
// fetches Prometheus metrics from each pod in the pool, computes weights
// via the WeightCalculator, and calls OnWeightsUpdated.
type Scraper struct {
	mu       sync.Mutex
	pools    map[string]*poolScrapeState // key: namespace/name
	client   *http.Client
	interval time.Duration
	alpha    float64 // weight of kv_cache signal relative to waiting queue
	beta     float64 // weight of token throughput signal relative to waiting queue
	onUpdate OnWeightsUpdated
}

type poolScrapeState struct {
	nsName   string
	port     int32
	podIPs   []string
	cancel   context.CancelFunc
	// snapshots holds the previous counter values per pod IP, used to compute
	// token throughput rates as (Δcounter / Δtime).
	snapshots map[string]podCounterSnapshot
}

// NewScraper creates a Scraper with the given interval (seconds), alpha, and beta.
//   - alpha controls how much KV-cache pressure contributes relative to the
//     waiting queue depth (0 disables KV-cache signal).
//   - beta controls how much the normalised token throughput rate contributes
//     relative to the waiting queue depth (0 disables token-rate signal).
func NewScraper(intervalSeconds int, alpha, beta float64, onUpdate OnWeightsUpdated) *Scraper {
	interval := defaultScrapeInterval
	if intervalSeconds > 0 {
		interval = time.Duration(intervalSeconds) * time.Second
	}
	return &Scraper{
		pools:    make(map[string]*poolScrapeState),
		interval: interval,
		alpha:    alpha,
		beta:     beta,
		onUpdate: onUpdate,
		client: &http.Client{
			Timeout: scrapeTimeout,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		},
	}
}

// RegisterPool starts a scrape loop for the given pool if not already running.
// podIPs is the current list of pod IPs matching the InferencePool selector.
// port is the targetPort specified in the InferencePool spec.
func (s *Scraper) RegisterPool(nsName string, port int32, podIPs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.pools[nsName]; ok {
		// Update pod list in-place; the running goroutine picks it up on next cycle.
		existing.podIPs = podIPs
		existing.port = port
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	state := &poolScrapeState{
		nsName:    nsName,
		port:      port,
		podIPs:    podIPs,
		cancel:    cancel,
		snapshots: make(map[string]podCounterSnapshot),
	}
	s.pools[nsName] = state
	go s.scrapeLoop(ctx, state)
}

// DeregisterPool stops the scrape loop for the given pool.
func (s *Scraper) DeregisterPool(nsName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.pools[nsName]; ok {
		state.cancel()
		delete(s.pools, nsName)
	}
}

// GetPodIPs returns a snapshot of the current pod IPs for a pool.
// Returns nil if the pool is not registered yet.
func (s *Scraper) GetPodIPs(nsName string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.pools[nsName]
	if !ok {
		return nil
	}
	ips := make([]string, len(state.podIPs))
	copy(ips, state.podIPs)
	return ips
}

// UpdatePods refreshes the pod IP list for an already-registered pool.
func (s *Scraper) UpdatePods(nsName string, podIPs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.pools[nsName]; ok {
		state.podIPs = podIPs
	}
}

func (s *Scraper) scrapeLoop(ctx context.Context, state *poolScrapeState) {
	utils.AviLog.Infof("inference scraper: starting loop for pool %s (interval=%s)", state.nsName, s.interval)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			utils.AviLog.Infof("inference scraper: stopping loop for pool %s", state.nsName)
			return
		case <-ticker.C:
			s.mu.Lock()
			podIPs := make([]string, len(state.podIPs))
			copy(podIPs, state.podIPs)
			port := state.port
			nsName := state.nsName
			// Copy snapshots so we can read/write without holding the lock during scrape.
			snapshots := make(map[string]podCounterSnapshot, len(state.snapshots))
			for k, v := range state.snapshots {
				snapshots[k] = v
			}
			s.mu.Unlock()

			if len(podIPs) == 0 {
				continue
			}

			results, newSnapshots := s.scrapeAllPods(ctx, podIPs, port, snapshots)

			// Persist updated snapshots back into state.
			s.mu.Lock()
			for k, v := range newSnapshots {
				state.snapshots[k] = v
			}
			s.mu.Unlock()

			weights := ComputeWeights(results, s.alpha, s.beta)
			utils.AviLog.Debugf("inference scraper: pool %s weights updated: %+v", nsName, weights)
			s.onUpdate(nsName, weights)
		}
	}
}

func (s *Scraper) scrapeAllPods(
	ctx context.Context,
	podIPs []string,
	port int32,
	prevSnapshots map[string]podCounterSnapshot,
) ([]PodMetrics, map[string]podCounterSnapshot) {
	type result struct {
		idx      int
		metrics  PodMetrics
		snapshot podCounterSnapshot
	}

	results := make([]PodMetrics, len(podIPs))
	newSnapshots := make(map[string]podCounterSnapshot, len(podIPs))
	ch := make(chan result, len(podIPs))
	var wg sync.WaitGroup

	for i, ip := range podIPs {
		wg.Add(1)
		go func(idx int, podIP string) {
			defer wg.Done()
			// Add random jitter to avoid thundering herd across pods.
			jitter := time.Duration(rand.Intn(maxJitterMs)) * time.Millisecond //nolint:gosec
			select {
			case <-time.After(jitter):
			case <-ctx.Done():
				ch <- result{idx: idx, metrics: PodMetrics{PodIP: podIP, Reachable: false}}
				return
			}
			prev := prevSnapshots[podIP]
			m, snap := s.scrapePod(ctx, podIP, port, prev)
			ch <- result{idx: idx, metrics: m, snapshot: snap}
		}(i, ip)
	}

	wg.Wait()
	close(ch)
	for r := range ch {
		results[r.idx] = r.metrics
		if r.metrics.Reachable {
			newSnapshots[r.metrics.PodIP] = r.snapshot
		}
	}
	return results, newSnapshots
}

func (s *Scraper) scrapePod(
	ctx context.Context,
	podIP string,
	port int32,
	prev podCounterSnapshot,
) (PodMetrics, podCounterSnapshot) {
	m := PodMetrics{PodIP: podIP, Reachable: false}
	now := time.Now()
	url := fmt.Sprintf("http://%s:%d/metrics", podIP, port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		utils.AviLog.Warnf("inference scraper: failed to build request for %s: %v", url, err)
		return m, podCounterSnapshot{}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		utils.AviLog.Warnf("inference scraper: scrape failed for %s: %v", url, err)
		return m, podCounterSnapshot{}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		utils.AviLog.Warnf("inference scraper: non-200 from %s: %d", url, resp.StatusCode)
		return m, podCounterSnapshot{}
	}

	families, err := parsePrometheusMetrics(resp.Body)
	if err != nil {
		utils.AviLog.Warnf("inference scraper: parse error for %s: %v", url, err)
		return m, podCounterSnapshot{}
	}

	m.Reachable = true

	// Gauge metrics.
	if v, ok := getGaugeValue(families, metricRequestsRunning); ok {
		m.NumRequestsRunning = v
	}
	if v, ok := getGaugeValue(families, metricRequestsWaiting); ok {
		m.NumRequestsWaiting = v
	}
	if v, ok := getGaugeValue(families, metricKVCacheUsage); ok {
		m.KVCacheUsagePerc = v
	}

	// Counter metrics — read raw totals.
	var genTokens, promptTokens float64
	if v, ok := getCounterValue(families, metricGenerationTokensTotal); ok {
		genTokens = v
	}
	if v, ok := getCounterValue(families, metricPromptTokensTotal); ok {
		promptTokens = v
	}

	snap := podCounterSnapshot{
		generationTokensTotal: genTokens,
		promptTokensTotal:     promptTokens,
		scrapeTime:            now,
	}

	// Compute token throughput rates if we have a valid previous snapshot.
	if !prev.scrapeTime.IsZero() {
		dt := now.Sub(prev.scrapeTime).Seconds()
		if dt > 0 {
			// Guard against counter resets (e.g. pod restart) by clamping to 0.
			deltaGen := max0(genTokens - prev.generationTokensTotal)
			deltaPrompt := max0(promptTokens - prev.promptTokensTotal)
			m.GenerationTokensPerSec = deltaGen / dt
			m.PromptTokensPerSec = deltaPrompt / dt
			m.TotalTokensPerSec = m.GenerationTokensPerSec + m.PromptTokensPerSec
		}
	}

	return m, snap
}

// max0 returns v if v >= 0, else 0. Guards against Prometheus counter resets.
func max0(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func parsePrometheusMetrics(r io.Reader) (map[string]*dto.MetricFamily, error) {
	var parser expfmt.TextParser
	return parser.TextToMetricFamilies(r)
}

func getGaugeValue(families map[string]*dto.MetricFamily, name string) (float64, bool) {
	family, ok := families[name]
	if !ok || len(family.Metric) == 0 {
		return 0, false
	}
	gauge := family.Metric[0].GetGauge()
	if gauge == nil {
		return 0, false
	}
	return gauge.GetValue(), true
}

func getCounterValue(families map[string]*dto.MetricFamily, name string) (float64, bool) {
	family, ok := families[name]
	if !ok || len(family.Metric) == 0 {
		return 0, false
	}
	counter := family.Metric[0].GetCounter()
	if counter == nil {
		return 0, false
	}
	return counter.GetValue(), true
}
