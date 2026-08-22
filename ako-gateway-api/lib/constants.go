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

package lib

const (
	Prefix                    = "ako-gw-"
	GatewayController         = "ako.vmware.com/avi-lb"
	CoreGroup                 = "v1"
	GatewayGroup              = "gateway.networking.k8s.io"
	HealthMonitorKind         = "HealthMonitor"
	RouteBackendExtensionKind = "RouteBackendExtension"
	AKOCRDController          = "AKOCRDController"
	CRDOperatorPrefix         = "ako-crd-operator-"
	HTTPRouteAcceptedMessage  = "Parent reference is valid"

	// SurfaceLabel on an HTTPRoute declares which AI Gateway surface the route
	// serves. It leads the object name when readable names are enabled, so that an
	// LLM route, an MCP tool server and an agent are told apart at a glance in the
	// Avi UI. The surface is read from the route itself rather than inferred from an
	// attached AI*RoutePolicy on purpose: the policy store is filled by informer
	// events with no ordering guarantee against route processing, so inferring it
	// would rename objects non deterministically on restart.
	SurfaceLabel = "ai.ako.vmware.com/surface"

	SurfaceLLM   = "llm"
	SurfaceMCP   = "mcp"
	SurfaceAgent = "agent"
)

const (
	ZeroAttachedRoutes = 0
)

const (
	// Gateway annotations
	DedicatedGatewayModeAnnotation = "ako.vmware.com/dedicated-gateway-mode"
)

const (
	GatewayClassGatewayControllerIndex = "GatewayClassGatewayController"
	REGULAREXPRESSION                  = "RegularExpression"
	EXACT                              = "Exact"
	PATHPREFIX                         = "PathPrefix"
	LBVipTypeAnnotation                = "networking.vmware.com/lb-vip-type"
	VCFGatewayClassName                = "avi-lb"
)

const (
	AllowedRoutesNamespaceFromAll  = "All"
	AllowedRoutesNamespaceFromSame = "Same"
)

var SupportedLBVipTypes = map[string]string{
	"public":  "PUBLIC",
	"private": "PRIVATE",
}
