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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

func TestMachineInventoryIdentityConflictKeepsItUnready(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	inventory := &elementalv1.MachineInventory{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "node-a",
			Namespace:  "default",
			Finalizers: []string{elementalv1.MachineInventoryFinalizer},
			Annotations: map[string]string{
				elementalv1.IdentityConflictAnnotation: "hardware fingerprint does not match the registered host: v1:aa vs v1:bb [2026-10-27T08:00:00Z]",
			},
		},
	}
	cl := newFakeElementalClient(t, inventory)
	r := &MachineInventoryReconciler{Client: cl}
	key := client.ObjectKeyFromObject(inventory)
	reconcileReady := func() *metav1.Condition {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		g.Expect(err).ToNot(HaveOccurred())
		current := &elementalv1.MachineInventory{}
		g.Expect(cl.Get(ctx, key, current)).To(Succeed())
		return meta.FindStatusCondition(current.Status.Conditions, elementalv1.ReadyCondition)
	}

	ready := reconcileReady()
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(Equal(elementalv1.IdentityConflictReason))
	g.Expect(ready.Message).To(HavePrefix("hardware fingerprint does not match the registered host"))
	// The plan secret is still created: the inventory works once resolved.
	g.Expect(cl.Get(ctx, key, &corev1.Secret{})).To(Succeed())

	// An applied plan does not make it ready while the conflict stands.
	plan := &corev1.Secret{}
	g.Expect(cl.Get(ctx, key, plan)).To(Succeed())
	plan.Data = map[string][]byte{"applied-checksum": []byte("abc")}
	g.Expect(cl.Update(ctx, plan)).To(Succeed())
	ready = reconcileReady()
	g.Expect(ready.Reason).To(Equal(elementalv1.IdentityConflictReason))

	// Removing the annotation hands Ready back to the plan.
	current := &elementalv1.MachineInventory{}
	g.Expect(cl.Get(ctx, key, current)).To(Succeed())
	delete(current.Annotations, elementalv1.IdentityConflictAnnotation)
	g.Expect(cl.Update(ctx, current)).To(Succeed())
	ready = reconcileReady()
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(Equal(elementalv1.PlanSuccessfullyAppliedReason))
}

func TestMachineInventoryIdentityConflictWithEmptyAnnotation(t *testing.T) {
	g := NewWithT(t)
	inventory := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{elementalv1.IdentityConflictAnnotation: ""},
	}}
	setIdentityConflictCondition(inventory)
	ready := meta.FindStatusCondition(inventory.Status.Conditions, elementalv1.ReadyCondition)
	g.Expect(ready).ToNot(BeNil())
	g.Expect(ready.Reason).To(Equal(elementalv1.IdentityConflictReason))
	g.Expect(ready.Message).To(ContainSubstring(elementalv1.IdentityConflictAnnotation))
}
