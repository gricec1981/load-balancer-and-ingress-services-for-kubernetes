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

package status

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func acceptedCondition(status metav1.ConditionStatus, reason string, message string) metav1.Condition {
	return metav1.Condition{
		Type:    string(gatewayv1.RouteConditionAccepted),
		Status:  status,
		Reason:  reason,
		Message: message,
	}
}

func TestAcceptedConditionTakesVSUUID(t *testing.T) {
	tests := []struct {
		name      string
		condition metav1.Condition
		want      bool
	}{
		{
			name:      "already accepted, refresh the UUID",
			condition: acceptedCondition(metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), `{"rule-0":"virtualservice-abc"}`),
			want:      true,
		},
		{
			name:      "accepted by the validator before any VS exists",
			condition: acceptedCondition(metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), "Parent reference is valid"),
			want:      true,
		},
		{
			// The regression: a child VS deleted and recreated during a parent VS rebuild
			// leaves Pending behind. The recreate must be able to clear it.
			name:      "pending after the child VS was dropped and recreated",
			condition: acceptedCondition(metav1.ConditionFalse, string(gatewayv1.RouteReasonPending), ""),
			want:      true,
		},
		{
			name:      "pending while AKO has not processed the gateway yet",
			condition: acceptedCondition(metav1.ConditionUnknown, string(gatewayv1.RouteReasonPending), "AKO is yet to process Gateway gw for parent reference gw"),
			want:      true,
		},
		{
			name:      "rejected: no matching listener hostname",
			condition: acceptedCondition(metav1.ConditionFalse, string(gatewayv1.RouteReasonNoMatchingListenerHostname), "no matching hostname"),
			want:      false,
		},
		{
			name:      "rejected: not allowed by listeners",
			condition: acceptedCondition(metav1.ConditionFalse, string(gatewayv1.RouteReasonNotAllowedByListeners), "not allowed"),
			want:      false,
		},
		{
			name:      "rejected: no matching parent",
			condition: acceptedCondition(metav1.ConditionFalse, string(gatewayv1.RouteReasonNoMatchingParent), "no matching parent"),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := acceptedConditionTakesVSUUID(tt.condition); got != tt.want {
				t.Errorf("acceptedConditionTakesVSUUID(%s/%s) = %v, want %v",
					tt.condition.Status, tt.condition.Reason, got, tt.want)
			}
		})
	}
}

// TestBuildJSONMessageDeleteRecreateCycle walks the message through the exact sequence a
// parent VS rebuild produces: the rules are programmed, the child VS is deleted (draining
// the message to ""), then recreated. The message must come back rather than error out on
// the empty string it was left holding.
func TestBuildJSONMessageDeleteRecreateCycle(t *testing.T) {
	o := &httproute{}
	conditions := []metav1.Condition{
		acceptedCondition(metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), "Parent reference is valid"),
	}
	setMessage := func(m string) { conditions[0].Message = m }

	message, err := o.buildJSONMessage(conditions, "rule-0", "virtualservice-1", false)
	if err != nil {
		t.Fatalf("adding rule-0: unexpected error: %v", err)
	}
	if message != `{"rule-0":"virtualservice-1"}` {
		t.Fatalf("adding rule-0: got %q", message)
	}
	setMessage(message)

	message, err = o.buildJSONMessage(conditions, "rule-1", "virtualservice-2", false)
	if err != nil {
		t.Fatalf("adding rule-1: unexpected error: %v", err)
	}
	if message != `{"rule-0":"virtualservice-1","rule-1":"virtualservice-2"}` {
		t.Fatalf("adding rule-1: got %q", message)
	}
	setMessage(message)

	// The parent VS is rebuilt: every child is deleted before being recreated.
	for _, rule := range []string{"rule-0", "rule-1"} {
		message, err = o.buildJSONMessage(conditions, rule, "", true)
		if err != nil {
			t.Fatalf("deleting %s: unexpected error: %v", rule, err)
		}
		setMessage(message)
	}
	if message != "" {
		t.Fatalf("after deleting every rule: got %q, want empty (which is what flips Accepted to Pending)", message)
	}

	// Recreated. buildJSONMessage must rebuild from the drained message.
	message, err = o.buildJSONMessage(conditions, "rule-0", "virtualservice-3", false)
	if err != nil {
		t.Fatalf("recreating rule-0: unexpected error: %v", err)
	}
	if message != `{"rule-0":"virtualservice-3"}` {
		t.Fatalf("recreating rule-0: got %q", message)
	}
}
