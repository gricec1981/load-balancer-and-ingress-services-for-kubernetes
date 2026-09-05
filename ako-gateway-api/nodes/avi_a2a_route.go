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
	akogatewayapiaigateway "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/aigateway"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// ApplyA2ARoutePolicy translates an AIA2ARoutePolicy onto a child EVH VS. It
// attaches four DataScripts that together enforce the A2A protocol on the data
// plane:
//
//   - HTTP_REQ:       buffer request body; re-pin requests whose task ID is
//     already known to the backend that owns that task.
//   - HTTP_REQ_DATA:  read buffered body; gate the calling agent's A2A method
//     against the agentAccess allow-list; look up params.id in the VS
//     persistence table for follow-up task calls.
//   - HTTP_RESP:      enable response-body buffering on tasks/send responses so
//     the next phase can capture the new task ID.
//   - HTTP_RESP_DATA: extract result.id from the buffered response and store the
//     task ID → backend mapping in the VS persistence table.
//
// Agent identity is resolved from the JWT validated by the shared
// AIGatewayAuthPolicy (authRef). Agent card requests (GET
// /.well-known/agent.json) bypass body inspection and are proxied directly.
func ApplyA2ARoutePolicy(key string, policy *akogatewayapiaigateway.AIA2ARoutePolicy, childVsNode *nodes.AviEvhVsNode, authHost, routePrefix string, mode akogatewayapiaigateway.AuthClaimMode) {
	if policy == nil {
		return
	}
	if err := policy.Spec.Validate(); err != nil {
		utils.AviLog.Warnf("key: %s, msg: AIA2ARoutePolicy %s/%s invalid, skipping: %v",
			key, policy.Namespace, policy.Name, err)
		return
	}

	// 1. Share the LLM/MCP IdP: resolve authRef and apply its OAuth graph.
	// Also derive the effective claim mode from the referenced auth policy so the
	// A2A DataScripts read claims the same way (jwtQuery / jwtHeader vs OAuth browser).
	effectiveMode := mode
	if policy.Spec.AuthRef != nil && policy.Spec.AuthRef.Name != "" {
		authPolicy := akogatewayapiaigateway.SharedPolicyStore().GetAuthPolicyByNsName(
			policy.Namespace, policy.Spec.AuthRef.Name)
		if authPolicy != nil {
			akogatewayapiaigateway.ApplyAuthPolicy(key, authPolicy, childVsNode, authHost, routePrefix)
			if m := authPolicy.Spec.EffectiveAuthMode(); m != akogatewayapiaigateway.ClaimModeOAuth {
				effectiveMode = m
			}
		} else {
			utils.AviLog.Warnf("key: %s, msg: AIA2ARoutePolicy %s/%s authRef %q not found; A2A route left unauthenticated",
				key, policy.Namespace, policy.Name, policy.Spec.AuthRef.Name)
		}
	}

	// 2. Generate the four A2A DataScripts and attach them to the VS.
	scripts := akogatewayapiaigateway.GenerateA2AScripts(policy, effectiveMode)
	vsName := childVsNode.Name

	attachModelRouteDS(childVsNode,
		akogatewayapiaigateway.DSA2AReqName(vsName),
		akogatewayapiaigateway.DSEvtHTTPReq, scripts.ReqScript, nil)

	attachModelRouteDS(childVsNode,
		akogatewayapiaigateway.DSA2AReqDataName(vsName),
		akogatewayapiaigateway.DSEvtHTTPReqData, scripts.ReqDataScript, nil)

	attachModelRouteDS(childVsNode,
		akogatewayapiaigateway.DSA2ARespName(vsName),
		akogatewayapiaigateway.DSEvtHTTPResp, scripts.RespScript, nil)

	attachModelRouteDS(childVsNode,
		akogatewayapiaigateway.DSA2ARespDataName(vsName),
		akogatewayapiaigateway.DSEvtHTTPRespData, scripts.RespDataScript, nil)

	utils.AviLog.Infof("key: %s, msg: AIA2ARoutePolicy %s/%s: attached A2A gateway config on VS %s (task-affinity=%v, agent-rbac=%v)",
		key, policy.Namespace, policy.Name, vsName,
		policy.Spec.TaskAffinity != nil,
		policy.Spec.AgentAccess != nil && len(policy.Spec.AgentAccess.Rules) > 0)
}
