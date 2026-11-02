/*
Copyright © 2022 - 2026 SUSE LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPlanSecretName(t *testing.T) {
	global := map[string]string{SystemAgentAuthScopeAnnotation: "Global"}
	for _, tc := range []struct {
		name      string
		inventory MachineInventory
		want      string
	}{
		{
			name:      "shared inventory keeps its name",
			inventory: MachineInventory{ObjectMeta: metav1.ObjectMeta{Name: "m-1"}},
			want:      "m-1",
		},
		{
			name:      "explicit shared scope keeps its name",
			inventory: MachineInventory{ObjectMeta: metav1.ObjectMeta{Name: "m-1", Annotations: map[string]string{SystemAgentAuthScopeAnnotation: SystemAgentAuthScopeShared}}},
			want:      "m-1",
		},
		{
			name:      "global inventory gets the plan prefix",
			inventory: MachineInventory{ObjectMeta: metav1.ObjectMeta{Name: "global-4c4c", Annotations: global}},
			want:      "global-bm-plan-global-4c4c",
		},
		{
			name: "the referenced Secret wins, so existing plans keep their names",
			inventory: MachineInventory{
				ObjectMeta: metav1.ObjectMeta{Name: "global-4c4c", Annotations: global},
				Status:     MachineInventoryStatus{Plan: &PlanStatus{PlanSecretRef: &corev1.ObjectReference{Name: "global-4c4c"}}},
			},
			want: "global-4c4c",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlanSecretName(&tc.inventory); got != tc.want {
				t.Fatalf("PlanSecretName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsGlobalRegistrationName(t *testing.T) {
	for name, want := range map[string]bool{
		"global":         true,
		"global-microos": true,
		"globalx":        false,
		"microos":        false,
		"my-global":      false,
	} {
		if got := IsGlobalRegistrationName(name); got != want {
			t.Errorf("IsGlobalRegistrationName(%q) = %v, want %v", name, got, want)
		}
	}
}
