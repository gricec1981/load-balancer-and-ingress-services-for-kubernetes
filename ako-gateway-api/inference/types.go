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

// Package inference implements AKO-native support for the Gateway API Inference
// Extension (gateway.inference.x-k8s.io). Instead of using ext-proc/EPP, AKO
// scrapes LLM Prometheus endpoints directly and adjusts Avi Pool Group member
// weights on each scrape cycle.
package inference

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InferencePoolGroup is the API group for inference extension resources.
const InferencePoolGroup = "gateway.inference.x-k8s.io"

// InferencePoolKind is the Kind string for InferencePool.
const InferencePoolKind = "InferencePool"

// InferencePool mirrors the relevant fields of the upstream
// gateway.inference.x-k8s.io/v1 InferencePool CRD for use inside AKO.
// AKO reads this resource via the dynamic client (unstructured) and maps it
// into this typed struct for processing.
type InferencePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InferencePoolSpec   `json:"spec"`
	Status InferencePoolStatus `json:"status,omitempty"`
}

// InferencePoolSpec defines the desired state of an InferencePool.
type InferencePoolSpec struct {
	// Selector selects the pods that belong to this pool.
	Selector metav1.LabelSelector `json:"selector"`

	// TargetPort is the port on the pod that the model server listens on.
	// This port is also where Prometheus metrics are scraped from (via /metrics).
	TargetPort int32 `json:"targetPort"`

	// ExtensionRef is optional. When omitted, AKO acts as the native
	// endpoint picker using its built-in Prometheus scraper.
	// +optional
	ExtensionRef *ExtensionRef `json:"extensionRef,omitempty"`
}

// ExtensionRef points to an external Endpoint Picker service.
// When set, AKO defers endpoint selection to the referenced EPP and disables
// its own metric-based weight adjustment.
type ExtensionRef struct {
	// Name of the Service exposing the EPP.
	Name string `json:"name"`
	// Namespace of the EPP Service.
	Namespace string `json:"namespace"`
	// Port the EPP gRPC server listens on.
	Port int32 `json:"port"`
}

// InferencePoolStatus is the observed state of an InferencePool.
type InferencePoolStatus struct {
	// Conditions holds standard condition types for the pool.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// InferencePoolList is a list of InferencePool objects.
type InferencePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferencePool `json:"items"`
}

// PodMetrics holds the scraped Prometheus metric values for a single pod.
type PodMetrics struct {
	// PodIP is the IP address of the pod.
	PodIP string
	// NumRequestsRunning is the vllm:num_requests_running gauge value.
	NumRequestsRunning float64
	// NumRequestsWaiting is the vllm:num_requests_waiting gauge value.
	NumRequestsWaiting float64
	// KVCacheUsagePerc is the vllm:kv_cache_usage_perc gauge value (0.0–1.0).
	KVCacheUsagePerc float64
	// Reachable is false when the scrape failed for this pod.
	Reachable bool
}

// WeightedPod associates a pod IP with its computed Avi pool member ratio.
type WeightedPod struct {
	PodIP string
	// Ratio is an integer in [1, 100] for use as Avi PoolGroupMember.Ratio.
	Ratio uint32
}
