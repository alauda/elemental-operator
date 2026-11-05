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
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	systemagent "github.com/rancher/elemental-operator/internal/system-agent"
	"github.com/rancher/elemental-operator/pkg/network"
	"github.com/rancher/elemental-operator/pkg/test"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var _ = Describe("reconcile machine inventory", func() {
	var r *MachineInventoryReconciler
	var mInventory *elementalv1.MachineInventory
	var planSecret *corev1.Secret

	BeforeEach(func() {
		r = &MachineInventoryReconciler{
			Client: cl,
		}

		mInventory = &elementalv1.MachineInventory{
			ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{elementalv1.MachineInventoryFinalizer},
				Name:       "machine-inventory-suite",
				Namespace:  "default",
			},
		}

		planSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
			Data: map[string][]byte{
				"applied-checksum": []byte("appliedchecksum"),
			},
		}

		Expect(cl.Create(ctx, mInventory)).To(Succeed())
	})

	AfterEach(func() {
		Expect(test.CleanupAndWait(ctx, cl, mInventory, planSecret)).To(Succeed())
	})

	It("should reconcile machine inventory object when plan secret doesn't exist", func() {
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)).To(Succeed())

		cond := meta.FindStatusCondition(mInventory.Status.Conditions, elementalv1.ReadyCondition)
		Expect(cond).NotTo(BeNil())

		Expect(cond.Reason).To(Equal(elementalv1.WaitingForPlanReason))
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(Equal("waiting for plan to be applied"))

		networkCond := meta.FindStatusCondition(mInventory.Status.Conditions, elementalv1.NetworkConfigReady)
		Expect(networkCond).NotTo(BeNil())
		Expect(networkCond.Reason).To(Equal(elementalv1.ReconcilingNetworkConfig))
		Expect(networkCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(networkCond.Message).To(Equal("NetworkConfig is ready"))
	})

	It("should reconcile machine inventory object when plan secret already exists", func() {
		Expect(cl.Create(ctx, planSecret)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)).To(Succeed())

		Expect(mInventory.Status.Plan.Checksum).To(Equal(string(planSecret.Data["applied-checksum"])))
		Expect(mInventory.Status.Plan.State).To(Equal(elementalv1.PlanApplied))

		cond := meta.FindStatusCondition(mInventory.Status.Conditions, elementalv1.ReadyCondition)
		Expect(cond).NotTo(BeNil())

		Expect(cond.Reason).To(Equal(elementalv1.PlanSuccessfullyAppliedReason))
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(Equal("plan successfully applied"))

		networkCond := meta.FindStatusCondition(mInventory.Status.Conditions, elementalv1.NetworkConfigReady)
		Expect(networkCond).NotTo(BeNil())
		Expect(networkCond.Reason).To(Equal(elementalv1.ReconcilingNetworkConfig))
		Expect(networkCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(networkCond.Message).To(Equal("NetworkConfig is ready"))
	})

	It("should not mark network config ready when reconciliation is required", func() {
		mInventory.Spec.Network.Configurator = network.ConfiguratorNmc
		Expect(cl.Update(ctx, mInventory)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)).To(Succeed())

		networkCond := meta.FindStatusCondition(mInventory.Status.Conditions, elementalv1.NetworkConfigReady)
		Expect(networkCond).To(BeNil())
	})

	It("should add finalizer if not exist", func() {
		noFinalizerMI := &elementalv1.MachineInventory{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-inventory-no-finalizer",
				Namespace: "default",
			},
		}
		Expect(cl.Create(ctx, noFinalizerMI)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, noFinalizerMI)).To(Succeed())

		Expect(controllerutil.ContainsFinalizer(noFinalizerMI, elementalv1.MachineInventoryFinalizer)).To(BeTrue())
		Expect(test.CleanupAndWait(ctx, cl, noFinalizerMI)).To(Succeed())
	})
})

var _ = Describe("plan secret watch mapping", func() {
	It("should enqueue the same named machine inventory for a plan secret without owner reference", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "machine-inventory-suite",
			},
			Type: elementalv1.PlanSecretType,
		}

		requests := planSecretToMachineInventoryRequests(context.Background(), secret)

		Expect(requests).To(Equal([]reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: secret.Namespace,
				Name:      secret.Name,
			},
		}}))
	})

	It("should enqueue managed plan secrets even when the secret type is not set", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "machine-inventory-suite",
				Labels: map[string]string{
					elementalv1.ElementalManagedLabel: "true",
				},
				Annotations: map[string]string{
					elementalv1.PlanTypeAnnotation: elementalv1.PlanTypeBootstrap,
				},
			},
		}

		requests := planSecretToMachineInventoryRequests(context.Background(), secret)

		Expect(requests).To(HaveLen(1))
		Expect(requests[0].NamespacedName).To(Equal(types.NamespacedName{
			Namespace: secret.Namespace,
			Name:      secret.Name,
		}))
	})

	It("should ignore unrelated secrets", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "registry-auth",
			},
			Type: corev1.SecretTypeOpaque,
		}

		Expect(planSecretToMachineInventoryRequests(context.Background(), secret)).To(BeNil())
	})
})

var _ = Describe("createPlanSecret", func() {
	var r *MachineInventoryReconciler
	var mInventory *elementalv1.MachineInventory
	var planSecret *corev1.Secret

	BeforeEach(func() {
		r = &MachineInventoryReconciler{
			Client: cl,
		}

		mInventory = &elementalv1.MachineInventory{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-inventory-suite",
				Namespace: "default",
			},
		}

		planSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		}

		Expect(cl.Create(ctx, mInventory)).To(Succeed())
	})

	AfterEach(func() {
		Expect(test.CleanupAndWait(ctx, cl, mInventory, planSecret)).To(Succeed())
	})

	It("should succesfully create plan secret", func() {
		Expect(r.createPlanSecret(ctx, mInventory)).To(Succeed())

		Expect(r.Get(ctx, types.NamespacedName{Namespace: mInventory.Namespace, Name: mInventory.Name}, planSecret)).To(Succeed())

		Expect(planSecret.OwnerReferences).To(HaveLen(1))
		Expect(planSecret.OwnerReferences[0].APIVersion).To(Equal(elementalv1.GroupVersion.String()))
		Expect(planSecret.OwnerReferences[0].Kind).To(Equal("MachineInventory"))
		Expect(planSecret.OwnerReferences[0].Name).To(Equal(mInventory.Name))
		Expect(planSecret.OwnerReferences[0].UID).To(Equal(mInventory.UID))
		Expect(planSecret.OwnerReferences[0].Controller).To(Equal(ptr.To(true)))
		Expect(planSecret.Labels).To(HaveKey(elementalv1.ElementalManagedLabel))
		Expect(planSecret.Type).To(Equal(elementalv1.PlanSecretType))
		Expect(planSecret.Data).To(HaveKey("plan"))

		Expect(mInventory.Status.Plan.PlanSecretRef.Name).To(Equal(mInventory.Name))
		Expect(mInventory.Status.Plan.PlanSecretRef.Namespace).To(Equal(mInventory.Namespace))

		cond := meta.FindStatusCondition(mInventory.Status.Conditions, elementalv1.ReadyCondition)
		Expect(cond).NotTo(BeNil())

		Expect(cond.Reason).To(Equal(elementalv1.WaitingForPlanReason))
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(Equal("waiting for plan to be applied"))
	})

	It("shouldn't return error is secret already exists", func() {
		Expect(r.createPlanSecret(ctx, mInventory)).To(Succeed())
		Expect(r.createPlanSecret(ctx, mInventory)).To(Succeed())
	})

	It("should return error is secret fails to be created", func() {
		r.Client = machineInventoryFailingClient{}
		err := r.createPlanSecret(ctx, mInventory)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to create"))
	})

	It("should do nothing if ready condition is present", func() {
		meta.SetStatusCondition(&mInventory.Status.Conditions, metav1.Condition{
			Type:    elementalv1.ReadyCondition,
			Reason:  elementalv1.PlanSuccessfullyAppliedReason,
			Status:  metav1.ConditionTrue,
			Message: "plan successfully applied",
		})
		Expect(r.createPlanSecret(ctx, mInventory)).To(Succeed())
	})

	It("should not reset network if configurator none", func() {
		inventoryWithNoneNetwork := &elementalv1.MachineInventory{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-inventory-network-none",
				Namespace: "default",
			},
			Spec: elementalv1.MachineInventorySpec{
				Network: elementalv1.NetworkConfig{
					Configurator: network.ConfiguratorNone,
				},
			},
		}
		Expect(r.Create(ctx, inventoryWithNoneNetwork)).Should(Succeed())
		Expect(r.createPlanSecret(ctx, inventoryWithNoneNetwork)).To(Succeed())
		Expect(r.Get(ctx, types.NamespacedName{Namespace: inventoryWithNoneNetwork.Namespace, Name: inventoryWithNoneNetwork.Name}, planSecret)).To(Succeed())
		Expect(r.updatePlanSecretWithReset(ctx, inventoryWithNoneNetwork, planSecret)).Should(Succeed())
		Expect(r.Get(ctx, types.NamespacedName{Namespace: inventoryWithNoneNetwork.Namespace, Name: inventoryWithNoneNetwork.Name}, planSecret)).To(Succeed())
		planData, found := planSecret.Data["plan"]
		Expect(found).To(BeTrue(), "Secret should contain reset plan data")

		plan := &systemagent.Plan{}
		Expect(json.Unmarshal(planData, plan)).Should(Succeed())
		Expect(len(plan.OneTimeInstructions)).Should(Equal(2), "Should contain 2 reset instructions")
		Expect(plan.OneTimeInstructions).ShouldNot(ContainElement(systemagent.OneTimeInstruction{
			CommonInstruction: systemagent.CommonInstruction{
				Name:    "restore first boot config",
				Command: "elemental-register",
				Args:    []string{"--debug", "--reset-network"},
			}}))
		Expect(test.CleanupAndWait(ctx, cl, inventoryWithNoneNetwork)).To(Succeed())
	})

	It("should reset network if configurator not none", func() {
		inventoryWithNetwork := &elementalv1.MachineInventory{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-inventory-network-nmc",
				Namespace: "default",
			},
			Spec: elementalv1.MachineInventorySpec{
				Network: elementalv1.NetworkConfig{
					Configurator: network.ConfiguratorNmc,
				},
			},
		}
		Expect(r.Create(ctx, inventoryWithNetwork)).Should(Succeed())
		Expect(r.createPlanSecret(ctx, inventoryWithNetwork)).To(Succeed())
		Expect(r.Get(ctx, types.NamespacedName{Namespace: inventoryWithNetwork.Namespace, Name: inventoryWithNetwork.Name}, planSecret)).To(Succeed())
		Expect(r.updatePlanSecretWithReset(ctx, inventoryWithNetwork, planSecret)).Should(Succeed())
		Expect(r.Get(ctx, types.NamespacedName{Namespace: inventoryWithNetwork.Namespace, Name: inventoryWithNetwork.Name}, planSecret)).To(Succeed())
		planData, found := planSecret.Data["plan"]
		Expect(found).To(BeTrue(), "Secret should contain reset plan data")

		plan := &systemagent.Plan{}
		Expect(json.Unmarshal(planData, plan)).Should(Succeed())
		Expect(plan.OneTimeInstructions).Should(ContainElement(systemagent.OneTimeInstruction{
			CommonInstruction: systemagent.CommonInstruction{
				Name:    "restore first boot config",
				Command: "elemental-register",
				Args:    []string{"--debug", "--reset-network"},
			}}))
		Expect(test.CleanupAndWait(ctx, cl, inventoryWithNetwork)).To(Succeed())
	})
})

var _ = Describe("updateInventoryWithPlanStatus", func() {
	var r *MachineInventoryReconciler
	var mInventory *elementalv1.MachineInventory
	var planSecret *corev1.Secret

	BeforeEach(func() {
		r = &MachineInventoryReconciler{
			Client: cl,
		}

		mInventory = &elementalv1.MachineInventory{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-inventory-suite",
				Namespace: "default",
			},
		}

		planSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		}

		Expect(cl.Create(ctx, mInventory)).To(Succeed())
	})

	AfterEach(func() {
		Expect(test.CleanupAndWait(ctx, cl, mInventory, planSecret)).To(Succeed())
	})

	It("should succesfully update when plan is applied", func() {
		planSecret.Data = map[string][]byte{
			"applied-checksum": []byte("applied-checksum"),
		}
		Expect(cl.Create(ctx, planSecret)).To(Succeed())

		mInventory.Status.Plan = &elementalv1.PlanStatus{}
		Expect(r.updateInventoryWithPlanStatus(ctx, mInventory)).To(Succeed())

		Expect(mInventory.Status.Plan.State).To(Equal(elementalv1.PlanApplied))

		Expect(mInventory.Status.Conditions).To(HaveLen(1))
		Expect(mInventory.Status.Conditions[0].Type).To(Equal(elementalv1.ReadyCondition))
		Expect(mInventory.Status.Conditions[0].Reason).To(Equal(elementalv1.PlanSuccessfullyAppliedReason))
		Expect(mInventory.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
		Expect(mInventory.Status.Conditions[0].Message).To(Equal("plan successfully applied"))
	})

	It("should return error when plan failed to be applied", func() {
		planSecret.Data = map[string][]byte{
			"failed-checksum": []byte("failed-checksum"),
		}
		Expect(cl.Create(ctx, planSecret)).To(Succeed())

		mInventory.Status.Plan = &elementalv1.PlanStatus{}
		err := r.updateInventoryWithPlanStatus(ctx, mInventory)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to apply plan"))
	})

	AfterEach(func() {
		Expect(test.CleanupAndWait(ctx, cl, mInventory, planSecret)).To(Succeed())
	})

})

var _ = Describe("handle finalizer", func() {
	var r *MachineInventoryReconciler
	var mInventory *elementalv1.MachineInventory
	var planSecret *corev1.Secret

	BeforeEach(func() {
		r = &MachineInventoryReconciler{
			Client: cl,
		}

		mInventory = &elementalv1.MachineInventory{
			ObjectMeta: metav1.ObjectMeta{
				DeletionTimestamp: &metav1.Time{Time: time.Now()},
				Annotations:       map[string]string{elementalv1.MachineInventoryResettableAnnotation: "true"},
				Finalizers:        []string{elementalv1.MachineInventoryFinalizer},
				Name:              "machine-inventory-suite-finalizer",
				Namespace:         "default",
			},
		}

		planSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mInventory.Name,
				Namespace: mInventory.Namespace,
			},
		}

		// 1. Create initial MachineInventory
		Expect(cl.Create(ctx, mInventory)).To(Succeed())
		// 2. Create initial plan Secret
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())
		// 3. Update meta.DeletionTimestamp
		Expect(cl.Delete(ctx, mInventory)).To(Succeed())
		// 4. Update secret with reset plan
		_, err = r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())
		// 5. Update MachineInventory plan status
		_, err = r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())
	})

	It("should update secret with reset plan", func() {
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      planSecret.Name,
			Namespace: planSecret.Namespace,
		}, planSecret)).To(Succeed())
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)).To(Succeed())

		wantChecksum, wantPlan, err := r.newResetPlan(ctx, false)
		Expect(err).ToNot(HaveOccurred())

		// Check Plan status
		Expect(mInventory.Status.Plan.Checksum).To(Equal(wantChecksum))
		Expect(mInventory.Status.Plan.PlanSecretRef.Name).To(Equal(planSecret.Name))
		Expect(mInventory.Status.Plan.PlanSecretRef.Namespace).To(Equal(planSecret.Namespace))
		Expect(mInventory.Status.Plan.State).To(Equal(elementalv1.PlanState("")))

		// Check MachineInventory status
		Expect(mInventory.Status.Conditions[0].Type).To(Equal(elementalv1.ReadyCondition))
		Expect(mInventory.Status.Conditions[0].Reason).To(Equal(elementalv1.WaitingForPlanReason))
		Expect(mInventory.Status.Conditions[0].Status).To(Equal(metav1.ConditionFalse))
		Expect(mInventory.Status.Conditions[0].Message).To(Equal("waiting for plan to be applied"))

		// Check plan secret was updated
		Expect(planSecret.Annotations[elementalv1.PlanTypeAnnotation]).To(Equal(elementalv1.PlanTypeReset))
		Expect(planSecret.Data["plan"]).To(Equal(wantPlan))
		Expect(planSecret.Data["applied-checksum"]).To(Equal([]byte("")))
		Expect(planSecret.Data["failed-checksum"]).To(Equal([]byte("")))

		// Check we are holding on the MachineInventory (preventing actual deletion)
		_, err = r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)).To(Succeed())
	})

	It("should remove finalizer on reset plan applied", func() {
		// 6. Mark the reset plan as applied
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, planSecret)).To(Succeed())
		planSecret.Data["applied-checksum"] = []byte("applied")
		Expect(cl.Update(ctx, planSecret)).To(Succeed())

		// 7. Trigger finalizer removal
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())

		// Check MachineInventory was actually deleted
		err = cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("should delete by removing finalizer when resettable annotation is false", func() {
		// 6. Manually disable resettable annotation
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)).To(Succeed())
		mInventory.Annotations[elementalv1.MachineInventoryResettableAnnotation] = "false"
		Expect(cl.Update(ctx, mInventory)).To(Succeed())

		// 7. Trigger finalizer removal
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())

		// Check MachineInventory was actually deleted
		err = cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("should delete by removing finalizer when up for deletion", func() {
		// 6. Manually remove finalizer
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)).To(Succeed())
		controllerutil.RemoveFinalizer(mInventory, elementalv1.MachineInventoryFinalizer)
		Expect(cl.Update(ctx, mInventory)).To(Succeed())

		// Check MachineInventory was actually deleted
		err := cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	AfterEach(func() {
		Expect(test.CleanupAndWait(ctx, cl, mInventory, planSecret)).To(Succeed())
	})
})

var _ = Describe("handle unmanaged finalizer", func() {
	var r *MachineInventoryReconciler
	var mInventory *elementalv1.MachineInventory
	var planSecret *corev1.Secret

	BeforeEach(func() {
		r = &MachineInventoryReconciler{
			Client: cl,
		}

		mInventory = &elementalv1.MachineInventory{
			ObjectMeta: metav1.ObjectMeta{
				DeletionTimestamp: &metav1.Time{Time: time.Now()},
				Annotations:       map[string]string{elementalv1.MachineInventoryResettableAnnotation: "true", elementalv1.MachineInventoryOSUnmanagedAnnotation: "true"},
				Finalizers:        []string{elementalv1.MachineInventoryFinalizer},
				Name:              "machine-inventory-suite-finalizer",
				Namespace:         "default",
			},
		}

		planSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mInventory.Name,
				Namespace: mInventory.Namespace,
			},
		}

		// 1. Create initial MachineInventory
		Expect(cl.Create(ctx, mInventory)).To(Succeed())
		// 2. Create initial plan Secret
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())
		// 3. Update meta.DeletionTimestamp
		Expect(cl.Delete(ctx, mInventory)).To(Succeed())
		// 4. Update secret with reset plan
		_, err = r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())
		// 5. Update MachineInventory plan status
		_, err = r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())
	})

	It("should update secret with reset plan when unmanaged annotation is true", func() {
		// Manually enable os.unmanaged annotation
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      planSecret.Name,
			Namespace: planSecret.Namespace,
		}, planSecret)).To(Succeed())
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)).To(Succeed())
		mInventory.Annotations[elementalv1.MachineInventoryOSUnmanagedAnnotation] = "true"
		Expect(cl.Update(ctx, mInventory)).To(Succeed())

		// Check Plan status
		Expect(mInventory.Status.Plan.PlanSecretRef.Name).To(Equal(planSecret.Name))
		Expect(mInventory.Status.Plan.PlanSecretRef.Namespace).To(Equal(planSecret.Namespace))
		Expect(mInventory.Status.Plan.State).To(Equal(elementalv1.PlanState("")))

		// Check MachineInventory status
		Expect(mInventory.Status.Conditions[0].Type).To(Equal(elementalv1.ReadyCondition))
		Expect(mInventory.Status.Conditions[0].Reason).To(Equal(elementalv1.WaitingForPlanReason))
		Expect(mInventory.Status.Conditions[0].Status).To(Equal(metav1.ConditionFalse))
		Expect(mInventory.Status.Conditions[0].Message).To(Equal("waiting for plan to be applied"))

		// Check plan secret was updated
		Expect(planSecret.Annotations[elementalv1.PlanTypeAnnotation]).To(Equal(elementalv1.PlanTypeReset))
		var gotPlan systemagent.Plan
		Expect(json.Unmarshal(planSecret.Data["plan"], &gotPlan)).To(Succeed())
		Expect(gotPlan.Files).To(HaveLen(1))
		Expect(gotPlan.Files[0].Path).To(Equal(LocalResetUnmanagedMarker))
		Expect(gotPlan.Files[0].Permissions).To(Equal("0600"))
		timestamp, err := base64.StdEncoding.DecodeString(gotPlan.Files[0].Content)
		Expect(err).ToNot(HaveOccurred())
		_, err = time.Parse("2006-01-02 15:04:05", string(timestamp))
		Expect(err).ToNot(HaveOccurred())
		Expect(string(planSecret.Data["applied-checksum"])).To(Equal(""))
		Expect(string(planSecret.Data["failed-checksum"])).To(Equal(""))

		// Check we are holding on the MachineInventory (preventing actual deletion)
		_, err = r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)).To(Succeed())
	})

	It("should remove finalizer on unmanaged reset plan applied", func() {
		// 6. Mark the reset plan as applied
		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, planSecret)).To(Succeed())
		mInventory.Annotations[elementalv1.MachineInventoryOSUnmanagedAnnotation] = "true"
		planSecret.Data["applied-checksum"] = []byte("applied")
		Expect(cl.Update(ctx, planSecret)).To(Succeed())

		// 7. Trigger finalizer removal
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mInventory.Namespace,
				Name:      mInventory.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())

		// Check MachineInventory was actually deleted
		err = cl.Get(ctx, client.ObjectKey{
			Name:      mInventory.Name,
			Namespace: mInventory.Namespace,
		}, mInventory)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	AfterEach(func() {
		Expect(test.CleanupAndWait(ctx, cl, mInventory, planSecret)).To(Succeed())
	})
})

type machineInventoryFailingClient struct {
	client.Client
}

func (cl machineInventoryFailingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return errors.New("failed to create")
}
