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

package nodes

import (
	"context"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	akogatewayapilib "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/lib"
	akogatewayapiobjects "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/objects"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/objects"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

func DequeueIngestion(key string, fullsync bool) {
	utils.AviLog.Infof("key: %s, msg: starting graph Sync", key)
	objType, namespace, name := lib.ExtractTypeNameNamespace(key)

	utils.AviLog.Infof("Key: %s, msg: objectType: %s, namespace: %s, name: %s", key, objType, namespace, name)
	schema, valid := ConfigDescriptor().GetByType(objType)
	if !valid {
		return
	}
	// For HTTPRoute updates, capture old gateways BEFORE schema.GetGateways updates the mapping
	var oldGatewaysForCleanup []string
	if objType == lib.HTTPRoute {
		httpRoute, err := akogatewayapilib.AKOControlConfig().GatewayApiInformers().HTTPRouteInformer.Lister().HTTPRoutes(namespace).Get(name)
		if err == nil {
			utils.AviLog.Debugf("key: %s, msg: Successfully retrieved the HTTPRoute object %s", key, name)
			if !IsHTTPRouteValid(key, httpRoute) {
				return
			}
			// Get current gateways from the HTTPRoute spec
			var currentGateways []string
			for _, parentRef := range httpRoute.Spec.ParentRefs {
				parentNs := namespace
				if parentRef.Namespace != nil {
					parentNs = string(*parentRef.Namespace)
				}
				gwNsName := parentNs + "/" + string(parentRef.Name)
				currentGateways = append(currentGateways, gwNsName)
			}

			// Get old gateways that need cleanup before the mapping is updated
			oldGatewaysForCleanup = GetOldGatewaysForHTTPRouteCleanup(namespace, name, key, currentGateways)
		}
	}

	gatewayNsNameList, found := schema.GetGateways(namespace, name, key)
	if !found {
		//returning due to error, cannot delete or update
		utils.AviLog.Errorf("key: %s, msg: got error while getting k8s object", key)
		return
	}
	utils.AviLog.Infof("key: %s, msg: processing gateways %v", key, gatewayNsNameList)
	// handleGateway rebuilds the parent VS, which used to drop every EVH child from the
	// shared model until the route loop below put them back. Two things now keep the REST
	// layer from ever seeing that gap: handleGateway carries the existing children onto the
	// new parent (carryOverEvhChildren), and it defers its publish to this function, so a
	// model only reaches the REST layer once its route rebuild has finished. Both are needed
	// — the REST layer reads the shared lister directly, so suppressing the publish alone
	// would still expose a childless model to any key already queued for it.
	gatewayModelChanged := false
	if objType == lib.Gateway {
		gatewayModelChanged = handleGateway(namespace, name, fullsync, key, true)
	}

	if objType == utils.Service {
		_, err := utils.GetInformers().ServiceInformer.Lister().Services(namespace).Get(name)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				objects.SharedClusterIpLister().Delete(namespace + "/" + name)
			} else {
				utils.AviLog.Errorf("key: %s, msg: got error while getting service object", key)
				return
			}
		}
		objects.SharedClusterIpLister().Save(namespace+"/"+name, name)
	}

	var routeTypeNsNameList []string
	if !(objType == lib.Gateway && fullsync) {
		routeTypeNsNameList, found = schema.GetRoutes(namespace, name, key)
		if !found {
			utils.AviLog.Infof("key: %s, msg: got error while getting object %s", key, objType)
			return
		}
	}
	utils.AviLog.Infof("key: %s, msg: processing gateways %v and routes %v", key, gatewayNsNameList, routeTypeNsNameList)
	for _, gatewayNsName := range gatewayNsNameList {

		parentNs, _, parentName := lib.ExtractTypeNameNamespace(gatewayNsName)
		tenant := objects.SharedNamespaceTenantLister().GetTenantInNamespace(gatewayNsName)
		if tenant == "" {
			tenant = lib.GetTenant()
		}
		modelName := lib.GetModelName(tenant, akogatewayapilib.GetGatewayParentName(parentNs, parentName))
		// Carries the publish that handleGateway deferred to this loop. It belongs to the
		// model handled here: GatewayGetGw returns exactly the one gateway for a Gateway key,
		// and modelName is derived from the same tenant lister that handleGateway consulted,
		// after it ran. For every other objType this stays false until the Secret branch
		// below sets it.
		parentModelChanged := gatewayModelChanged

		modelFound, modelIntf := objects.SharedAviGraphLister().Get(modelName)
		// Seq: GW first and the secret created.
		modelNil := !modelFound || modelIntf == nil
		if objType == utils.Secret {
			if modelNil {
				parentModelChanged = handleGateway(parentNs, parentName, fullsync, key, true)
				modelFound, modelIntf = objects.SharedAviGraphLister().Get(modelName)
				modelNil = !modelFound || modelIntf == nil
				if modelNil {
					utils.AviLog.Warnf("key: %s, msg: no model found: %s", key, modelName)
					continue
				}
				// Fetch routes for a gateway
				routeTypeNsNameList, found = GatewayToRoutes(parentNs, parentName, key)
				if !found {
					utils.AviLog.Errorf("key: %s, msg: got error while getting route objects for gateway %s/%s", key, parentNs, parentName)
					continue
				}
				utils.AviLog.Infof("key: %s, msg: Routes for gateway %s/%s are: %v", key, parentNs, parentName, utils.Stringify(routeTypeNsNameList))
			} else {
				model := &AviObjectGraph{modelIntf.(*nodes.AviObjectGraph)}
				vsToDelete := handleSecrets(parentNs, parentName, key, model)
				if vsToDelete {
					utils.AviLog.Warnf("key: %s, msg: No valid listener on Gateway %s/%s. Removing Parent VS from Controller", key, parentNs, parentName)
					objects.SharedAviGraphLister().Save(modelName, nil)
					if !fullsync {
						sharedQueue := utils.SharedWorkQueue().GetQueueByName(utils.GraphLayer)
						nodes.PublishKeyToRestLayer(modelName, key, sharedQueue)
						continue
					}
				}
			}
		}
		if modelNil {
			utils.AviLog.Warnf("key: %s, msg: no model found: %s", key, modelName)
			continue
		}

		model := &AviObjectGraph{modelIntf.(*nodes.AviObjectGraph)}
		utils.AviLog.Infof("key: %s, msg: processing routes %v", key, routeTypeNsNameList)
		for _, routeTypeNsName := range routeTypeNsNameList {
			objType, namespace, name := lib.ExtractTypeNameNamespace(routeTypeNsName)
			utils.AviLog.Infof("key: %s, msg: processing route %s mapped to gateway %s", key, routeTypeNsName, gatewayNsName)

			routeModel, err := NewRouteModel(key, objType, name, namespace)
			if err != nil {
				if k8serrors.IsNotFound(err) {
					utils.AviLog.Infof("key: %s, msg: deleting configurations corresponding to route %s", key, routeTypeNsName)
					model.ProcessRouteDeletion(key, gatewayNsName, routeModel, fullsync)
				}
				continue
			}

			childVSes := make(map[string]struct{}, 0)

			switch objType {
			case lib.HTTPRoute:
				model.ProcessL7Routes(key, routeModel, gatewayNsName, childVSes, fullsync)
			default:
				utils.AviLog.Warnf("key: %s, msg: route of type %s not supported", key, objType)
				continue
			}
			model.DeleteStaleChildVSes(key, routeModel, childVSes, fullsync)
		}
		if !akogatewayapilib.IsGatewayInDedicatedMode(parentNs, parentName) {
			model.AddDefaultHTTPPolicySet(key)
		}

		// Only add this node to the list of models if the checksum has changed.
		// parentModelChanged covers a Gateway-only change that the route loop leaves
		// untouched (no routes attached, or none whose child VS differs), whose publish
		// handleGateway deferred to here.
		modelChanged := saveAviModel(modelName, model.AviObjectGraph, key)
		if (modelChanged || parentModelChanged) && !fullsync {
			sharedQueue := utils.SharedWorkQueue().GetQueueByName(utils.GraphLayer)
			nodes.PublishKeyToRestLayer(modelName, key, sharedQueue)
		}
	}

	// For HTTPRoute updates, process old gateways for cleanup only
	if objType == lib.HTTPRoute {
		utils.AviLog.Infof("key: %s, msg: Checking for old gateways to cleanup. Current gateways: %v, Old gateways: %v", key, gatewayNsNameList, oldGatewaysForCleanup)
		if len(oldGatewaysForCleanup) > 0 {
			utils.AviLog.Infof("key: %s, msg: Processing old gateways for cleanup: %v", key, oldGatewaysForCleanup)
			for _, oldGatewayNsName := range oldGatewaysForCleanup {
				parentNs, _, parentName := lib.ExtractTypeNameNamespace(oldGatewayNsName)
				tenant := objects.SharedNamespaceTenantLister().GetTenantInNamespace(oldGatewayNsName)
				if tenant == "" {
					tenant = lib.GetTenant()
				}
				modelName := lib.GetModelName(tenant, akogatewayapilib.GetGatewayParentName(parentNs, parentName))

				modelFound, modelIntf := objects.SharedAviGraphLister().Get(modelName)
				if !modelFound || modelIntf == nil {
					utils.AviLog.Debugf("key: %s, msg: no model found for old gateway: %s", key, modelName)
					continue
				}

				model := &AviObjectGraph{modelIntf.(*nodes.AviObjectGraph)}
				routeTypeNsName := lib.HTTPRoute + "/" + namespace + "/" + name
				utils.AviLog.Infof("key: %s, msg: cleaning up route %s from old gateway %s", key, routeTypeNsName, oldGatewayNsName)

				// Process deletion for this route on the old gateway
				routeModel, err := NewRouteModel(key, lib.HTTPRoute, name, namespace)
				if err != nil {
					utils.AviLog.Warnf("key: %s, msg: error getting route model for cleanup: %v", key, err)
					continue
				}
				model.ProcessRouteDeletion(key, oldGatewayNsName, routeModel, fullsync)

				// Save the model changes
				modelChanged := saveAviModel(modelName, model.AviObjectGraph, key)
				if modelChanged && !fullsync {
					sharedQueue := utils.SharedWorkQueue().GetQueueByName(utils.GraphLayer)
					nodes.PublishKeyToRestLayer(modelName, key, sharedQueue)
				}
			}
		}
	}

	utils.AviLog.Infof("key: %s, msg: finished graph Sync", key)
}
func handleSecrets(gatewayNamespace string, gatewayName string, key string, object *AviObjectGraph) bool {
	_, _, secretName := lib.ExtractTypeNameNamespace(key)
	utils.AviLog.Infof("key: %s, msg: Processing secret update %s has been added.", key, secretName)
	cs := utils.GetInformers().ClientSet
	gatewayObj, err := akogatewayapilib.AKOControlConfig().GatewayApiInformers().GatewayInformer.Lister().Gateways(gatewayNamespace).Get(gatewayName)
	if err != nil {
		utils.AviLog.Errorf("key: %s, msg: unable to get the gateway object. err: %s", key, err)
		return false
	}
	secretObj, err := cs.CoreV1().Secrets(gatewayNamespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil || secretObj == nil {
		utils.AviLog.Warnf("key: %s, msg: secret %s has been deleted, err: %s", key, secretName, err)
		vsToDelete := DeleteTLSNode(key, object, gatewayObj, secretObj)
		return vsToDelete
	} else {
		utils.AviLog.Infof("key: %s, msg: secret %s has been added.", key, secretName)
		AddTLSNode(key, object, gatewayObj, secretObj)
	}
	return false
}
// handleGateway (re)builds the parent VS model for a Gateway and returns whether the saved
// model changed.
//
// Set deferPublish when the caller will republish the model itself once its routes have been
// rebuilt onto it; the model saved here is only half-reconciled until then. A deferring
// caller must honour the returned value, because a Gateway-only change leaves the caller's
// own saveAviModel with nothing to report. The early returns below publish regardless: they
// store a nil model, which has no children to lose and which the caller skips over.
func handleGateway(namespace, name string, fullsync bool, key string, deferPublish bool) bool {
	utils.AviLog.Debugf("key: %s, msg: processing gateway: %s", key, name)

	tenant := objects.SharedNamespaceTenantLister().GetTenantInNamespace(namespace + "/" + name)
	if tenant == "" {
		tenant = lib.GetTenant()
	}
	modelName := lib.GetModelName(tenant, akogatewayapilib.GetGatewayParentName(namespace, name))
	modelFound, _ := objects.SharedAviGraphLister().Get(modelName)
	if modelFound {
		utils.AviLog.Debugf("key: %s, msg: found model: %s", key, modelName)
	} else {
		utils.AviLog.Debugf("key: %s, msg: no model found: %s", key, modelName)
	}

	gatewayObj, err := akogatewayapilib.AKOControlConfig().GatewayApiInformers().GatewayInformer.Lister().Gateways(namespace).Get(name)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			utils.AviLog.Infof("key: %s, msg: got error while getting gateway class: %v", key, err)
			return false
		}
		utils.AviLog.Debugf("key: %s, msg: gateway not found: %s/%s", key, namespace, name)
		if !modelFound {
			// try to get model if it was dedicated mode since there is no way to find the annotation once gateway is deleted
			modelName = lib.GetModelName(tenant, lib.GetNamePrefix()+namespace+"-"+name+lib.DedicatedSuffix+"-EVH")
			modelFound, _ = objects.SharedAviGraphLister().Get(modelName)
		}
		if modelFound {
			// As gateway is not present, we need to remove mapping.
			gwNsName := namespace + "/" + name
			akogatewayapiobjects.GatewayApiLister().DeleteGatewayFromStore(gwNsName)
			objects.SharedAviGraphLister().Save(modelName, nil)
			objects.SharedNamespaceTenantLister().RemoveNamespaceToTenantCache(gwNsName)
			if !fullsync {
				sharedQueue := utils.SharedWorkQueue().GetQueueByName(utils.GraphLayer)
				nodes.PublishKeyToRestLayer(modelName, key, sharedQueue)
			}
		}
		return false
	}
	gwClass := string(gatewayObj.Spec.GatewayClassName)
	utils.AviLog.Debugf("key: %s, msg: fetching gateway class %s for gateway: %s/%s", key, gwClass, namespace, name)
	found, isAkoCtrl := akogatewayapiobjects.GatewayApiLister().IsGatewayClassControllerAKO(gwClass)
	if !found {
		//gateway class deleted
		utils.AviLog.Debugf("key: %s, msg: gateway class not found: %s", key, gwClass)
		objects.SharedAviGraphLister().Save(modelName, nil)
		if !fullsync {
			sharedQueue := utils.SharedWorkQueue().GetQueueByName(utils.GraphLayer)
			nodes.PublishKeyToRestLayer(modelName, key, sharedQueue)
		}
		return false
	}
	utils.AviLog.Debugf("key: %s, msg: fetching gateway class found: %s", key, gwClass)
	if !isAkoCtrl {
		//AKO is not the controller, do not build model
		utils.AviLog.Infof("key: %s, msg: Controller is not AKO for %s, not building VS model", key, modelName)
		return false
	}
	aviModelGraph := NewAviObjectGraph()
	aviModelGraph.BuildGatewayVs(gatewayObj, key)
	carryOverEvhChildren(modelName, aviModelGraph, key)

	// Reload the tenant to handle the change in tenant annotation in a Namespace
	tenant = objects.SharedNamespaceTenantLister().GetTenantInNamespace(namespace + "/" + name)
	if tenant == "" {
		tenant = lib.GetTenant()
	}
	modelName = lib.GetModelName(tenant, akogatewayapilib.GetGatewayParentName(namespace, name))
	modelChanged := saveAviModel(modelName, aviModelGraph.AviObjectGraph, key)
	if modelChanged && !fullsync && !deferPublish {
		sharedQueue := utils.SharedWorkQueue().GetQueueByName(utils.GraphLayer)
		nodes.PublishKeyToRestLayer(modelName, key, sharedQueue)
	}
	return modelChanged
}

// carryOverEvhChildren moves the EVH children of the stored model onto a freshly built
// parent before it replaces that model.
//
// BuildGatewayVs only builds the parent, so without this the model spends the whole route
// rebuild in DequeueIngestion holding no children at all. That state is not private: the
// REST layer reads models straight out of the shared lister, so any key already queued for
// this model would observe it, find every cached child missing from the graph, and delete
// the child virtual services off the Controller — they come back on the next publish, but
// the route status and the data path churn in between. Carrying the children over keeps the
// Gateway path consistent with every other object type, none of which empty the model.
//
// The route loop reconciles these nodes immediately afterwards — updating them, adding new
// ones and pruning through DeleteStaleChildVSes/ProcessRouteDeletion — so carrying them over
// cannot strand a child whose route is gone. A tenant change is the one case that must not
// carry over, and it is handled for free: BuildGatewayParent nils out the old tenant's model
// first, so the lookup below finds nothing.
func carryOverEvhChildren(modelName string, newGraph *AviObjectGraph, key string) {
	found, prevModelIntf := objects.SharedAviGraphLister().Get(modelName)
	if !found || prevModelIntf == nil {
		return
	}
	prevModel, ok := prevModelIntf.(*nodes.AviObjectGraph)
	if !ok {
		return
	}
	prevParents := prevModel.GetAviEvhVS()
	newParents := newGraph.GetAviEvhVS()
	if len(prevParents) == 0 || len(newParents) == 0 || len(prevParents[0].EvhNodes) == 0 {
		return
	}
	newParents[0].EvhNodes = prevParents[0].EvhNodes
	utils.AviLog.Infof("key: %s, msg: carried over %d EVH children onto the rebuilt parent of model %s",
		key, len(newParents[0].EvhNodes), modelName)
}

func saveAviModel(modelName string, aviGraph *nodes.AviObjectGraph, key string) bool {
	utils.AviLog.Debugf("key: %s, msg: Evaluating model :%s", key, modelName)
	if lib.DisableSync {
		// Note: This is not thread safe, however locking is expensive and the condition for locking should happen rarely
		utils.AviLog.Infof("key: %s, msg: Disable Sync is True, model %s can not be saved", key, modelName)
		return false
	}
	found, aviModel := objects.SharedAviGraphLister().Get(modelName)
	if found && aviModel != nil {
		prevChecksum := aviModel.(*nodes.AviObjectGraph).GraphChecksum
		utils.AviLog.Debugf("key: %s, msg: the model: %s has a previous checksum: %v", key, modelName, prevChecksum)
		presentChecksum := aviGraph.GetCheckSum()
		utils.AviLog.Debugf("key: %s, msg: the model: %s has a present checksum: %v", key, modelName, presentChecksum)
		if prevChecksum == presentChecksum {
			utils.AviLog.Debugf("key: %s, msg: The model: %s has identical checksums, hence not processing. Checksum value: %v", key, modelName, presentChecksum)
			return false
		}
	}
	// Right before saving the model, let's reset the retry counter for the graph.
	aviGraph.SetRetryCounter()
	aviGraph.CalculateCheckSum()
	objects.SharedAviGraphLister().Save(modelName, aviGraph)
	return true
}

func (o *AviObjectGraph) ProcessRouteDeletion(key, parentNsName string, routeModel RouteModel, fullsync bool) {
	// Perform locked operations in a separate scope to ensure lock is always released
	func() {
		o.Lock.Lock()
		defer o.Lock.Unlock()
		o.processRouteDeletionInternal(key, parentNsName, routeModel, fullsync)
	}()

	// Save model AFTER releasing lock to avoid deadlock with SetRetryCounter/CalculateCheckSum
	parentNode := o.GetAviEvhVS()
	modelName := parentNode[0].Tenant + "/" + parentNode[0].Name
	ok := saveAviModel(modelName, o.AviObjectGraph, key)
	if ok && len(o.AviObjectGraph.GetOrderedNodes()) != 0 && !fullsync {
		sharedQueue := utils.SharedWorkQueue().GetQueueByName(utils.GraphLayer)
		nodes.PublishKeyToRestLayer(modelName, key, sharedQueue)
	}
}

func (o *AviObjectGraph) processRouteDeletionInternal(key, parentNsName string, routeModel RouteModel, fullsync bool) {
	parentNode := o.GetAviEvhVS()
	routeTypeNsName := routeModel.GetType() + "/" + routeModel.GetNamespace() + "/" + routeModel.GetName()
	if parentNode[0].Dedicated {
		o.ProcessRouteDeletionForDedicatedMode(key, parentNsName, routeModel, fullsync)

	} else {
		found, childVSNames := akogatewayapiobjects.GatewayApiLister().GetRouteToChildVS(routeTypeNsName)
		if found {
			utils.AviLog.Infof("key: %s, msg: child VSes retrieved for deletion %v", key, childVSNames)

			for _, childVSName := range childVSNames {
				removed := nodes.RemoveEvhInModel(childVSName, parentNode, key)
				if removed {
					akogatewayapiobjects.GatewayApiLister().DeleteRouteChildVSMappings(routeTypeNsName, childVSName)
				}
			}
		}
	}
	updateHostname(key, parentNsName, parentNode[0])
}
func (o *AviObjectGraph) ProcessRouteDeletionForDedicatedMode(key, parentNsName string, routeModel RouteModel, fullsync bool) {
	utils.AviLog.Infof("key: %s, msg: Processing route deletion for dedicated mode: %s/%s", key, routeModel.GetNamespace(), routeModel.GetName())

	gatewayVSes := o.GetAviEvhVS()
	if len(gatewayVSes) == 0 {
		utils.AviLog.Errorf("key: %s, msg: No Gateway VS found for dedicated mode deletion", key)
		return
	}

	dedicatedVS := gatewayVSes[0]

	httpPSName := akogatewayapilib.GetHttpPolicySetName(dedicatedVS.AviMarkers.GatewayNamespace, dedicatedVS.AviMarkers.GatewayName, routeModel.GetNamespace(), routeModel.GetName())

	var updatedHttpPolicyRefs []*nodes.AviHttpPolicySetNode
	for _, policy := range dedicatedVS.HttpPolicyRefs {
		if policy.Name != httpPSName {
			updatedHttpPolicyRefs = append(updatedHttpPolicyRefs, policy)
		} else {
			utils.AviLog.Infof("key: %s, msg: Removing HTTP PolicySet %s for route deletion", key, httpPSName)
		}
	}
	dedicatedVS.HttpPolicyRefs = updatedHttpPolicyRefs

	dedicatedVS.PoolGroupRefs = []*nodes.AviPoolGroupNode{}
	dedicatedVS.PoolRefs = []*nodes.AviPoolNode{}

	if dedicatedVS.ServiceMetadata.HTTPRoute == routeModel.GetNamespace()+"/"+routeModel.GetName() {
		dedicatedVS.ServiceMetadata.HTTPRoute = ""
		dedicatedVS.AviMarkers.HTTPRouteName = ""
		dedicatedVS.AviMarkers.HTTPRouteNamespace = ""
	}

	utils.AviLog.Infof("key: %s, msg: Completed route deletion for dedicated mode: %s/%s", key, routeModel.GetNamespace(), routeModel.GetName())
}

func (o *AviObjectGraph) DeleteStaleChildVSes(key string, routeModel RouteModel, childVSes map[string]struct{}, fullsync bool) {
	o.Lock.Lock()
	defer o.Lock.Unlock()
	parentNode := o.GetAviEvhVS()

	_, storedChildVSes := akogatewayapiobjects.GatewayApiLister().GetRouteToChildVS(routeModel.GetType() + "/" + routeModel.GetNamespace() + "/" + routeModel.GetName())

	for _, childVSName := range storedChildVSes {
		if _, ok := childVSes[childVSName]; !ok {
			utils.AviLog.Infof("key: %s, msg: child VS retrieved for deletion %v", key, childVSName)
			removed := nodes.RemoveEvhInModel(childVSName, parentNode, key)
			if removed {
				akogatewayapiobjects.GatewayApiLister().DeleteRouteChildVSMappings(routeModel.GetType()+"/"+routeModel.GetNamespace()+"/"+routeModel.GetName(), childVSName)
			}
		}
	}
}
