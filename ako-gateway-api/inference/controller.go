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
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// InferencePoolGVR is the GroupVersionResource for the InferencePool CRD.
var InferencePoolGVR = schema.GroupVersionResource{
	Group:    "gateway.inference.x-k8s.io",
	Version:  "v1",
	Resource: "inferencepools",
}

// Controller reconciles InferencePool objects and manages the metrics Scraper.
// On each reconcile it:
//  1. Resolves the matching pod IPs from the InferencePool label selector.
//  2. Registers/updates the Scraper for the pool.
//  3. On delete, deregisters the pool and cleans the WeightStore.
//
// The Scraper calls back into the workqueue via OnWeightsUpdated to trigger
// a graph-layer re-enqueue of the associated HTTPRoute.
type Controller struct {
	mu              sync.Mutex
	dynamicClient   dynamic.Interface
	informer        informers.GenericInformer
	scraper         *Scraper
	workqueue       []workqueue.RateLimitingInterface //nolint:staticcheck
	numWorkers      uint32
	// poolToRoutes maps poolNsName → set of HTTPRoute ns/name keys that reference it.
	poolToRoutes    map[string][]string
}

var controllerInstance *Controller
var once sync.Once

// SharedInferenceController returns the process-wide singleton Controller.
func SharedInferenceController() *Controller {
	return controllerInstance
}

// InitController initialises the Controller singleton. Must be called once at
// startup before SetupEventHandlers.
func InitController(
	dynamicClient dynamic.Interface,
	informer informers.GenericInformer,
	wq []workqueue.RateLimitingInterface, //nolint:staticcheck
	numWorkers uint32,
	scrapeIntervalSeconds int,
	alphaKVCache float64,
) *Controller {
	once.Do(func() {
		ctrl := &Controller{
			dynamicClient: dynamicClient,
			informer:      informer,
			workqueue:     wq,
			numWorkers:    numWorkers,
			poolToRoutes:  make(map[string][]string),
		}
		ctrl.scraper = NewScraper(scrapeIntervalSeconds, alphaKVCache, ctrl.onWeightsUpdated)
		controllerInstance = ctrl
	})
	return controllerInstance
}

// SetupEventHandlers wires Add/Update/Delete handlers onto the InferencePool
// dynamic informer.
func (c *Controller) SetupEventHandlers() {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				return
			}
			ns := u.GetNamespace()
			name := u.GetName()
			key := lib.InferencePool + "/" + ns + "/" + name
			utils.AviLog.Infof("key: %s, msg: InferencePool ADD", key)
			c.enqueuePool(ns, key)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			ns := u.GetNamespace()
			name := u.GetName()
			key := lib.InferencePool + "/" + ns + "/" + name
			utils.AviLog.Infof("key: %s, msg: InferencePool UPDATE", key)
			c.enqueuePool(ns, key)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utils.AviLog.Errorf("couldn't get object from tombstone %#v", obj)
					return
				}
				u, ok = tombstone.Obj.(*unstructured.Unstructured)
				if !ok {
					utils.AviLog.Errorf("tombstone contained non-Unstructured object %#v", tombstone.Obj)
					return
				}
			}
			ns := u.GetNamespace()
			name := u.GetName()
			nsName := ns + "/" + name
			utils.AviLog.Infof("InferencePool DELETE: %s", nsName)
			c.scraper.DeregisterPool(nsName)
			GlobalWeightStore().Delete(nsName)
			c.reEnqueueAssociatedRoutes(nsName)
		},
	}
	c.informer.Informer().AddEventHandler(handler)
}

// Reconcile is the main reconcile function for a single InferencePool.
// It resolves pod IPs, updates the scraper, and re-enqueues associated routes.
func (c *Controller) Reconcile(key string) error {
	_, ns, name := lib.ExtractTypeNameNamespace(key)
	nsName := ns + "/" + name

	obj, err := c.dynamicClient.Resource(InferencePoolGVR).Namespace(ns).Get(
		context.TODO(), name, metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("InferencePool %s not found: %w", nsName, err)
	}

	pool, err := parseInferencePool(obj)
	if err != nil {
		return fmt.Errorf("failed to parse InferencePool %s: %w", nsName, err)
	}

	podIPs, err := c.resolvePodIPs(ns, pool.Spec.Selector)
	if err != nil {
		utils.AviLog.Warnf("key: %s, msg: failed to resolve pods: %v", key, err)
	}

	c.scraper.RegisterPool(nsName, pool.Spec.TargetPort, podIPs)
	utils.AviLog.Debugf("key: %s, msg: registered %d pods for scraping", key, len(podIPs))
	return nil
}

// GetRoutesForPool returns the HTTPRoute keys that currently reference the pool.
func (c *Controller) GetRoutesForPool(poolNsName string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	routes := make([]string, len(c.poolToRoutes[poolNsName]))
	copy(routes, c.poolToRoutes[poolNsName])
	return routes
}

// RegisterRouteForPool records that an HTTPRoute references a given InferencePool.
// The controller uses this mapping to re-enqueue the route when weights change.
func (c *Controller) RegisterRouteForPool(poolNsName, routeKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.poolToRoutes[poolNsName] {
		if existing == routeKey {
			return
		}
	}
	c.poolToRoutes[poolNsName] = append(c.poolToRoutes[poolNsName], routeKey)
}

// onWeightsUpdated is the Scraper callback. It stores the new weights and
// re-enqueues all HTTPRoutes that reference this pool.
func (c *Controller) onWeightsUpdated(poolNsName string, weights []WeightedPod) {
	GlobalWeightStore().Set(poolNsName, weights)
	c.reEnqueueAssociatedRoutes(poolNsName)
}

func (c *Controller) reEnqueueAssociatedRoutes(poolNsName string) {
	c.mu.Lock()
	routes := make([]string, len(c.poolToRoutes[poolNsName]))
	copy(routes, c.poolToRoutes[poolNsName])
	c.mu.Unlock()

	for _, routeKey := range routes {
		_, ns, _ := lib.ExtractTypeNameNamespace(routeKey)
		bkt := utils.Bkt(ns, c.numWorkers)
		c.workqueue[bkt].AddRateLimited(routeKey)
		utils.AviLog.Debugf("inference: re-enqueued %s for weight update from pool %s", routeKey, poolNsName)
	}
}

func (c *Controller) enqueuePool(namespace, key string) {
	bkt := utils.Bkt(namespace, c.numWorkers)
	c.workqueue[bkt].AddRateLimited(key)
}

// resolvePodIPs lists all pods in the given namespace matching the selector
// and returns their pod IPs.
func (c *Controller) resolvePodIPs(namespace string, selector metav1.LabelSelector) ([]string, error) {
	parsed, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return nil, fmt.Errorf("invalid label selector: %w", err)
	}

	podInformer := utils.GetInformers().PodInformer
	if podInformer == nil {
		// Fallback: use the dynamic API client directly.
		return c.resolvePodIPsDynamic(namespace, selector)
	}

	pods, err := podInformer.Lister().Pods(namespace).List(parsed)
	if err != nil {
		return nil, err
	}

	var ips []string
	for _, pod := range pods {
		if pod.Status.PodIP != "" && isPodReady(pod.Status.Conditions) {
			ips = append(ips, pod.Status.PodIP)
		}
	}
	return ips, nil
}

func (c *Controller) resolvePodIPsDynamic(namespace string, selector metav1.LabelSelector) ([]string, error) {
	podGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	ls, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return nil, err
	}
	list, err := c.dynamicClient.Resource(podGVR).Namespace(namespace).List(
		context.TODO(),
		metav1.ListOptions{LabelSelector: ls.String()},
	)
	if err != nil {
		return nil, err
	}
	var ips []string
	for _, item := range list.Items {
		ip, _, _ := unstructured.NestedString(item.Object, "status", "podIP")
		if ip != "" {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

// parseInferencePool converts an unstructured InferencePool to the typed struct.
func parseInferencePool(obj *unstructured.Unstructured) (*InferencePool, error) {
	pool := &InferencePool{}
	pool.Name = obj.GetName()
	pool.Namespace = obj.GetNamespace()

	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("spec not found")
	}

	port, _, _ := unstructured.NestedInt64(spec, "targetPort")
	pool.Spec.TargetPort = int32(port)

	selectorMap, _, _ := unstructured.NestedStringMap(spec, "selector", "matchLabels")
	pool.Spec.Selector.MatchLabels = selectorMap

	return pool, nil
}

func toUnstructured(obj interface{}) (*unstructured.Unstructured, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	return u, ok
}

// isPodReady returns true if the pod has the Ready condition set to True.
func isPodReady(conditions []corev1.PodCondition) bool {
	for _, c := range conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
