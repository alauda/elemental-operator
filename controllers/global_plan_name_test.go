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

package controllers

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

// These run on a fake client, so they need no envtest binaries.

func globalInventory(name string) *elementalv1.MachineInventory {
	return &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "cpaas-system",
		Name:        name,
		UID:         types.UID("uid-" + name),
		Annotations: map[string]string{elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeGlobal},
	}}
}

func TestCreatePlanSecretNamesGlobalPlansWithThePrefix(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	global := globalInventory("global-4c4c")
	shared := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{Namespace: "cpaas-system", Name: "m-1", UID: "uid-m-1"}}
	cl := newFakeElementalClient(t, global, shared)
	r := &MachineInventoryReconciler{Client: cl}

	for inventory, want := range map[*elementalv1.MachineInventory]string{
		global: "global-bm-plan-global-4c4c",
		shared: "m-1",
	} {
		g.Expect(r.createPlanSecret(ctx, inventory)).To(Succeed())
		g.Expect(inventory.Status.Plan.PlanSecretRef.Name).To(Equal(want))
		secret := &corev1.Secret{}
		g.Expect(cl.Get(ctx, client.ObjectKey{Namespace: "cpaas-system", Name: want}, secret)).To(Succeed())
		g.Expect(secret.OwnerReferences).To(HaveLen(1))
		g.Expect(secret.OwnerReferences[0].Name).To(Equal(inventory.Name), "the plan still belongs to its inventory")
	}
}

func TestPlanStatusReadsTheReferencedSecret(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	inventory := globalInventory("global-4c4c")
	inventory.Status.Plan = &elementalv1.PlanStatus{PlanSecretRef: &corev1.ObjectReference{Namespace: "cpaas-system", Name: "global-bm-plan-global-4c4c"}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cpaas-system", Name: "global-bm-plan-global-4c4c"},
		Type:       elementalv1.PlanSecretType,
		Data:       map[string][]byte{"applied-checksum": []byte("abc")},
	}
	r := &MachineInventoryReconciler{Client: newFakeElementalClient(t, inventory, secret)}

	g.Expect(r.updateInventoryWithPlanStatus(ctx, inventory)).To(Succeed())
	g.Expect(inventory.Status.Plan.State).To(Equal(elementalv1.PlanApplied))
	g.Expect(inventory.Status.Plan.Checksum).To(Equal("abc"))
}

func TestPlanSecretEventsReachTheOwningInventory(t *testing.T) {
	g := NewWithT(t)
	owned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cpaas-system",
			Name:      "global-bm-plan-global-4c4c",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: elementalv1.GroupVersion.String(),
				Kind:       "MachineInventory",
				Name:       "global-4c4c",
				UID:        "uid-global-4c4c",
				Controller: ptr.To(true),
			}},
		},
		Type: elementalv1.PlanSecretType,
	}
	g.Expect(planSecretToMachineInventoryRequests(context.Background(), owned)).To(ConsistOf(
		HaveField("NamespacedName", types.NamespacedName{Namespace: "cpaas-system", Name: "global-4c4c"})))

	unowned := owned.DeepCopy()
	unowned.OwnerReferences = nil
	g.Expect(planSecretToMachineInventoryRequests(context.Background(), unowned)).To(ConsistOf(
		HaveField("NamespacedName", types.NamespacedName{Namespace: "cpaas-system", Name: "global-4c4c"})),
		"without an owner the prefix is stripped")

	shared := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cpaas-system", Name: "m-1"}, Type: elementalv1.PlanSecretType}
	g.Expect(planSecretToMachineInventoryRequests(context.Background(), shared)).To(ConsistOf(
		HaveField("NamespacedName", types.NamespacedName{Namespace: "cpaas-system", Name: "m-1"})))
}

func TestValidateGlobalRegistrationName(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	registration := func(name, scope, url string) *elementalv1.MachineRegistration {
		r := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if scope != "" {
			r.Annotations = map[string]string{elementalv1.SystemAgentAuthScopeAnnotation: scope}
		}
		r.Status.RegistrationURL = url
		return r
	}

	g.Expect(validateGlobalRegistrationName(ctx, registration("global", "global", ""))).To(Succeed())
	g.Expect(validateGlobalRegistrationName(ctx, registration("global-microos", "global", ""))).To(Succeed())
	g.Expect(validateGlobalRegistrationName(ctx, registration("workers", "shared", ""))).To(Succeed())
	g.Expect(validateGlobalRegistrationName(ctx, registration("workers", "", ""))).To(Succeed())
	g.Expect(validateGlobalRegistrationName(ctx, registration("microos", "global", ""))).To(
		MatchError(ContainSubstring(`global-scoped MachineRegistration microos must be named "global" or start with "global-"`)))
	g.Expect(validateGlobalRegistrationName(ctx, registration("microos", "global", "https://example.com/elemental/registration/x"))).To(Succeed(),
		"a registration that already serves hosts keeps working")
}

func TestMisnamedGlobalRegistrationGetsNoRegistrationURL(t *testing.T) {
	g := NewWithT(t)
	registration := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "default",
		Name:        "microos",
		Annotations: map[string]string{elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeGlobal},
	}}
	cl := newFakeElementalClient(t, registration)

	ready := reconcileRegistrationReady(t, cl, "microos")
	g.Expect(ready).ToNot(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(Equal(elementalv1.InvalidGlobalRegistrationNameReason))

	got := &elementalv1.MachineRegistration{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(registration), got)).To(Succeed())
	g.Expect(got.Status.RegistrationURL).To(BeEmpty(), "hosts cannot register with a misnamed global registration")
}
