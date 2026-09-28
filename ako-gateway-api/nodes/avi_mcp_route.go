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
	"fmt"

	akogatewayapiaigateway "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/aigateway"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// ApplyMCPRoutePolicy translates an AIMCPRoutePolicy onto the given MCP child EVH
// VS (docs/gateway-api/ai-gateway-mcp.md §2):
//
//   - attaches AKO's own pcall-guarded Mcp-Session-Id session-affinity DataScripts
//     (HTTP_REQ re-pin, HTTP_RESP capture/forget);
//   - shares the LLM gateway's identity provider by resolving spec.authRef to an
//     AIGatewayAuthPolicy and applying its OAuth graph to this VS;
//   - attaches the AKO-generated per-role tool-authorization DataScript
//     (HTTP_REQ buffer-enable + HTTP_REQ_DATA gate on the JSON-RPC tool).
//
// It deliberately leaves the VS on the ordinary HTTP application profile rather than
// Avi's System-Secure-HTTP-MCP. On Avi 32.1.3 the controller attaches the system
// System-Standard-MCP DataScriptSet to any VS on an MCP-service-type profile, and that
// script's unguarded avi.pool.select raises on AKO's EVH-child + PoolGroup topology —
// verified live on mcp-01: every request carrying an Mcp-Session-Id 500s with
// "System-Standard-MCP:13: server [...] not found in pool". AKO's session scripts
// already do its job, and the profile's other features (websockets, HTTP/2) are not
// used by MCP's streamable-HTTP transport.
func ApplyMCPRoutePolicy(key string, policy *akogatewayapiaigateway.AIMCPRoutePolicy, childVsNode *nodes.AviEvhVsNode, authHost, routePrefix string, mode akogatewayapiaigateway.AuthClaimMode) {
	if policy == nil {
		return
	}
	if err := policy.Spec.Validate(); err != nil {
		utils.AviLog.Warnf("key: %s, msg: AIMCPRoutePolicy %s/%s invalid, skipping: %v", key, policy.Namespace, policy.Name, err)
		// A policy that asks for auth must not leave the route open because some
		// other part of it is invalid: fail closed until it is fixed.
		if policy.Spec.AuthRef != nil {
			akogatewayapiaigateway.DenyUnauthenticated(key, childVsNode,
				fmt.Sprintf("AIMCPRoutePolicy %s/%s declares an authRef but is invalid: %v",
					policy.Namespace, policy.Name, err))
		}
		return
	}

	// 1. MCP session affinity. The system System-Standard-MCP DataScript pins a
	// session to its backend via avi.pool.select(name, ip), which RAISES (HTTP 500)
	// on AKO's EVH-child-VS + PoolGroup topology — verified live (a tools/call with
	// an Mcp-Session-Id 500s; the same call without it succeeds). Author our own
	// pcall-guarded equivalent instead, and keep the VS off the MCP application
	// profile so the controller does not attach the system script for us (above).
	sess := akogatewayapiaigateway.GenerateMCPSessionScripts()
	attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSMCPSessReqName(childVsNode.Name),
		akogatewayapiaigateway.DSEvtHTTPReq, sess.ReqScript, nil)
	attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSMCPSessRespName(childVsNode.Name),
		akogatewayapiaigateway.DSEvtHTTPResp, sess.RespScript, nil)

	// 2. Share the LLM IdP: resolve authRef and apply its OAuth graph to this VS.
	if policy.Spec.AuthRef != nil && policy.Spec.AuthRef.Name != "" {
		authPolicy := akogatewayapiaigateway.SharedPolicyStore().GetAuthPolicyByNsName(policy.Namespace, policy.Spec.AuthRef.Name)
		if authPolicy != nil {
			akogatewayapiaigateway.ApplyAuthPolicy(key, authPolicy, childVsNode, authHost, routePrefix)
		} else {
			// The policy asks for auth that cannot be applied: fail closed rather
			// than serve the MCP route unauthenticated. The guard retries the route
			// and is removed once the referenced policy exists and realizes.
			akogatewayapiaigateway.DenyUnauthenticated(key, childVsNode,
				fmt.Sprintf("AIMCPRoutePolicy %s/%s authRef %q not found",
					policy.Namespace, policy.Name, policy.Spec.AuthRef.Name))
		}
	}

	// 3. Per-role tool authorization DataScript (only when toolAccess is set).
	scripts := akogatewayapiaigateway.GenerateMCPToolAuthScripts(policy, mode)
	if scripts.ReqDataScript != "" {
		vsName := childVsNode.Name
		attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSMCPReqName(vsName),
			akogatewayapiaigateway.DSEvtHTTPReq, scripts.ReqScript, nil)
		attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSMCPReqDataName(vsName),
			akogatewayapiaigateway.DSEvtHTTPReqData, scripts.ReqDataScript, nil)
	}

	utils.AviLog.Infof("key: %s, msg: AIMCPRoutePolicy %s/%s: attached MCP gateway config on VS %s (AKO session DataScripts, tool-authz=%v)",
		key, policy.Namespace, policy.Name, childVsNode.Name, scripts.ReqDataScript != "")
}
