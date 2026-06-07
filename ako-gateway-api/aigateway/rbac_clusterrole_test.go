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

package aigateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clusterRoleTemplatePath is the Helm ClusterRole template that grants AKO's
// service account access to the resources it watches. Relative to this package
// directory (ako-gateway-api/aigateway).
const clusterRoleTemplatePath = "../../helm/ako/templates/clusterrole.yaml"

// TestClusterRoleGrantsWatchedGVRs guards against the failure mode that bit
// AIModelRoutePolicy: a new AI Gateway policy CRD is wired with an informer but
// its resource is never added to the AKO ClusterRole, so the informer is
// RBAC-forbidden at runtime ("<resource> is forbidden"). Because the grant lives
// in a Helm template far from the Go informer registration, nothing catches the
// omission until a live deploy. This test couples the two: every GVR in
// WatchedAIGatewayPolicyGVRs() must be granted (resource + /status) in the
// ClusterRole. Adding a watched CRD without granting it fails the build here.
func TestClusterRoleGrantsWatchedGVRs(t *testing.T) {
	data, err := os.ReadFile(filepath.Clean(clusterRoleTemplatePath))
	if err != nil {
		t.Fatalf("read ClusterRole template %s: %v", clusterRoleTemplatePath, err)
	}
	content := string(data)

	for _, gvr := range WatchedAIGatewayPolicyGVRs() {
		resource := `"` + gvr.Resource + `"`
		status := `"` + gvr.Resource + `/status"`
		if !strings.Contains(content, resource) {
			t.Errorf("ClusterRole does not grant %s — add it to %s (and its /status). "+
				"Informer for %s would be RBAC-forbidden at runtime.",
				resource, clusterRoleTemplatePath, gvr.Resource)
		}
		if !strings.Contains(content, status) {
			t.Errorf("ClusterRole does not grant %s — status patches would be forbidden.", status)
		}
	}
}
