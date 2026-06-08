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

// ApplyMCPRoutePolicy translates an AIMCPRoutePolicy onto the given MCP child EVH
// VS. Per the verified Avi 32.1.1 object model (docs/gateway-api/ai-gateway-mcp.md
// §2), it reuses Avi's built-in MCP objects rather than generating session logic:
//
//   - sets the VS application profile to System-Secure-HTTP-MCP
//     (APPLICATION_PROFILE_TYPE_HTTP, app_service_type APP_SERVICE_TYPE_HTTP_MCP);
//   - references the system DataScriptSet System-Standard-MCP for Mcp-Session-Id
//     session persistence (create-on-response, delete-on-DELETE);
//   - shares the LLM gateway's identity provider by resolving spec.authRef to an
//     AIGatewayAuthPolicy and applying its OAuth graph to this VS;
//   - attaches the AKO-generated per-role tool-authorization DataScript
//     (HTTP_REQ buffer-enable + HTTP_REQ_DATA gate on the JSON-RPC tool).
//
// The tool-authz script runs in HTTP_REQ_DATA, a different event from the system
// session DataScript (HTTP_REQ/HTTP_RESP), so the two coexist on the same VS.
func ApplyMCPRoutePolicy(key string, policy *akogatewayapiaigateway.AIMCPRoutePolicy, childVsNode *nodes.AviEvhVsNode, authHost, routePrefix string) {
	if policy == nil {
		return
	}
	if err := policy.Spec.Validate(); err != nil {
		utils.AviLog.Warnf("key: %s, msg: AIMCPRoutePolicy %s/%s invalid, skipping: %v", key, policy.Namespace, policy.Name, err)
		return
	}

	// 1. MCP application profile (native, Avi 32.1.1).
	childVsNode.ApplicationProfile = akogatewayapiaigateway.MCPApplicationProfile

	// 2. MCP session affinity. The system System-Standard-MCP DataScript pins a
	// session to its backend via avi.pool.select(name, ip), which RAISES (HTTP 500)
	// on AKO's EVH-child-VS + PoolGroup topology — verified live (a tools/call with
	// an Mcp-Session-Id 500s; the same call without it succeeds). Author our own
	// pcall-guarded equivalent instead of referencing the system script.
	sess := akogatewayapiaigateway.GenerateMCPSessionScripts()
	attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSMCPSessReqName(childVsNode.Name),
		akogatewayapiaigateway.DSEvtHTTPReq, sess.ReqScript, nil)
	attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSMCPSessRespName(childVsNode.Name),
		akogatewayapiaigateway.DSEvtHTTPResp, sess.RespScript, nil)

	// 3. Share the LLM IdP: resolve authRef and apply its OAuth graph to this VS.
	if policy.Spec.AuthRef != nil && policy.Spec.AuthRef.Name != "" {
		authPolicy := akogatewayapiaigateway.SharedPolicyStore().GetAuthPolicyByNsName(policy.Namespace, policy.Spec.AuthRef.Name)
		if authPolicy != nil {
			akogatewayapiaigateway.ApplyAuthPolicy(key, authPolicy, childVsNode, authHost, routePrefix)
		} else {
			utils.AviLog.Warnf("key: %s, msg: AIMCPRoutePolicy %s/%s authRef %q not found; MCP route left unauthenticated",
				key, policy.Namespace, policy.Name, policy.Spec.AuthRef.Name)
		}
	}

	// 4. Per-role tool authorization DataScript (only when toolAccess is set).
	scripts := akogatewayapiaigateway.GenerateMCPToolAuthScripts(policy)
	if scripts.ReqDataScript != "" {
		vsName := childVsNode.Name
		attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSMCPReqName(vsName),
			akogatewayapiaigateway.DSEvtHTTPReq, scripts.ReqScript, nil)
		attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSMCPReqDataName(vsName),
			akogatewayapiaigateway.DSEvtHTTPReqData, scripts.ReqDataScript, nil)
	}

	utils.AviLog.Infof("key: %s, msg: AIMCPRoutePolicy %s/%s: attached MCP gateway config on VS %s (app profile %s, session DS %s, tool-authz=%v)",
		key, policy.Namespace, policy.Name, childVsNode.Name,
		akogatewayapiaigateway.MCPApplicationProfile, akogatewayapiaigateway.MCPSessionDataScript, scripts.ReqDataScript != "")
}
