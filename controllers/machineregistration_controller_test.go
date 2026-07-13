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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/test"
)

var _ = Describe("reconcile machine registration", func() {
	var r *MachineRegistrationReconciler
	var mRegistration *elementalv1.MachineRegistration
	var role *rbacv1.Role
	var roleBinding *rbacv1.RoleBinding
	var sa *corev1.ServiceAccount
	var secret *corev1.Secret

	BeforeEach(func() {
		r = &MachineRegistrationReconciler{
			Client:    cl,
			ServerURL: "https://example.com",
		}

		objKey := metav1.ObjectMeta{
			Name:      "test-name",
			Namespace: "default",
		}

		mRegistration = &elementalv1.MachineRegistration{ObjectMeta: objKey}
		role = &rbacv1.Role{ObjectMeta: objKey}
		roleBinding = &rbacv1.RoleBinding{ObjectMeta: objKey}
		sa = &corev1.ServiceAccount{ObjectMeta: objKey}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: mRegistration.Namespace,
				Name:      mRegistration.Name + elementalv1.SASecretSuffix,
			},
		}
		Expect(cl.Create(ctx, mRegistration)).To(Succeed())
	})

	AfterEach(func() {
		Expect(test.CleanupAndWait(ctx, cl, mRegistration, role, roleBinding, sa, secret)).To(Succeed())
	})

	reconcileTest := func() {
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: mRegistration.Namespace,
				Name:      mRegistration.Name,
			},
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(cl.Get(ctx, client.ObjectKey{
			Name:      mRegistration.Name,
			Namespace: mRegistration.Namespace,
		}, mRegistration)).To(Succeed())

		Expect(mRegistration.Status.RegistrationToken).ToNot(BeEmpty())
		Expect(mRegistration.Status.RegistrationURL).To(ContainSubstring("https://example.com/elemental/registration/"))
		Expect(mRegistration.Status.ServiceAccountRef.Kind).To(Equal("ServiceAccount"))
		Expect(mRegistration.Status.ServiceAccountRef.Name).To(Equal(mRegistration.Name))
		Expect(mRegistration.Status.ServiceAccountRef.Namespace).To(Equal(mRegistration.Namespace))
		Expect(mRegistration.Status.Conditions).To(HaveLen(1))
		Expect(mRegistration.Status.Conditions[0].Type).To(Equal(elementalv1.ReadyCondition))
		Expect(mRegistration.Status.Conditions[0].Reason).To(Equal(elementalv1.SuccessfullyCreatedReason))
		Expect(mRegistration.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))

		objKey := types.NamespacedName{Namespace: mRegistration.Namespace, Name: mRegistration.Name}
		secretKey := types.NamespacedName{Namespace: mRegistration.Namespace, Name: mRegistration.Name + elementalv1.SASecretSuffix}
		Expect(r.Get(ctx, objKey, &rbacv1.Role{})).To(Succeed())
		Expect(r.Get(ctx, objKey, &corev1.ServiceAccount{})).To(Succeed())
		Expect(r.Get(ctx, objKey, &rbacv1.RoleBinding{})).To(Succeed())
		Expect(r.Get(ctx, secretKey, &corev1.Secret{})).To(Succeed())
	}

	It("should reconcile machine registration object", reconcileTest)

	It("should reconcile a ready machine registration and recreate token secret if missing", func() {
		// Reconciles machine registration and creates and verify all dependent resources
		reconcileTest()

		// delete the token secret
		saSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: mRegistration.Namespace,
				Name:      mRegistration.Name + elementalv1.SASecretSuffix,
			},
		}
		Expect(r.Delete(ctx, saSecret)).To(Succeed())
		secretKey := types.NamespacedName{Namespace: mRegistration.Namespace, Name: mRegistration.Name + elementalv1.SASecretSuffix}
		Expect(r.Get(ctx, secretKey, &corev1.Secret{})).ToNot(Succeed())

		// Reconciles machine registration and creates and verify all dependent resources
		reconcileTest()
	})
})

var _ = Describe("setRegistrationTokenAndURL", func() {
	var r *MachineRegistrationReconciler
	var mRegistration *elementalv1.MachineRegistration

	BeforeEach(func() {
		r = &MachineRegistrationReconciler{
			Client:    cl,
			ServerURL: "https://example.com",
		}

		mRegistration = &elementalv1.MachineRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-name",
				Namespace: "default",
			},
		}
	})

	AfterEach(func() {
		Expect(test.CleanupAndWait(ctx, cl, mRegistration)).To(Succeed())
	})

	It("should successfully set registration token and url", func() {
		Expect(r.setRegistrationTokenAndURL(ctx, mRegistration)).To(Succeed())
		Expect(mRegistration.Status.RegistrationToken).ToNot(BeEmpty())
		Expect(mRegistration.Status.RegistrationURL).To(ContainSubstring("https://example.com/elemental/registration/"))
	})

	It("should return error when server-url is not configured", func() {
		r.ServerURL = ""
		err := r.setRegistrationTokenAndURL(ctx, mRegistration)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("server-url is not set"))
	})
})

var _ = Describe("createRBACObjects", func() {
	var r *MachineRegistrationReconciler
	var mRegistration *elementalv1.MachineRegistration
	var role *rbacv1.Role
	var sa *corev1.ServiceAccount
	var secret *corev1.Secret
	var roleBinding *rbacv1.RoleBinding

	BeforeEach(func() {
		r = &MachineRegistrationReconciler{
			Client: cl,
		}

		mRegistration = &elementalv1.MachineRegistration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-name",
				Namespace: "default",
				UID:       "testuid",
			},
		}

		objMeta := metav1.ObjectMeta{Namespace: mRegistration.Namespace, Name: mRegistration.Name}
		role = &rbacv1.Role{ObjectMeta: objMeta}
		sa = &corev1.ServiceAccount{ObjectMeta: objMeta}
		roleBinding = &rbacv1.RoleBinding{
			ObjectMeta: objMeta,
			RoleRef: rbacv1.RoleRef{
				Kind:     "Role",
				Name:     mRegistration.Name,
				APIGroup: "rbac.authorization.k8s.io",
			},
		}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: mRegistration.Namespace,
				Name:      mRegistration.Name + elementalv1.SASecretSuffix,
			},
		}
	})

	AfterEach(func() {
		Expect(test.CleanupAndWait(ctx, cl, role, sa, roleBinding, secret, mRegistration)).To(Succeed())
	})

	It("should successfully create RBAC objects", func() {
		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())
		objKey := types.NamespacedName{Namespace: mRegistration.Namespace, Name: mRegistration.Name}
		role := &rbacv1.Role{}
		Expect(r.Get(ctx, objKey, role)).To(Succeed())

		Expect(role.OwnerReferences).To(HaveLen(1))
		Expect(role.OwnerReferences[0].APIVersion).To(Equal(elementalv1.GroupVersion.String()))
		Expect(role.OwnerReferences[0].Kind).To(Equal("MachineRegistration"))
		Expect(role.OwnerReferences[0].Name).To(Equal(mRegistration.Name))
		Expect(role.OwnerReferences[0].UID).To(Equal(mRegistration.UID))
		Expect(role.OwnerReferences[0].Controller).To(Equal(ptr.To(true)))
		Expect(role.Labels).To(HaveKey(elementalv1.ElementalManagedLabel))

		Expect(role.Rules).To(HaveLen(1))
		Expect(role.Rules[0].APIGroups).To(Equal([]string{""}))
		Expect(role.Rules[0].Verbs).To(Equal([]string{"get", "watch", "list", "update", "patch"}))
		Expect(role.Rules[0].Resources).To(Equal([]string{"secrets"}))

		sa := &corev1.ServiceAccount{}
		Expect(r.Get(ctx, objKey, sa)).To(Succeed())
		Expect(sa.OwnerReferences).To(HaveLen(1))
		Expect(sa.OwnerReferences[0].APIVersion).To(Equal(elementalv1.GroupVersion.String()))
		Expect(sa.OwnerReferences[0].Kind).To(Equal("MachineRegistration"))
		Expect(sa.OwnerReferences[0].Name).To(Equal(mRegistration.Name))
		Expect(sa.OwnerReferences[0].UID).To(Equal(mRegistration.UID))
		Expect(sa.OwnerReferences[0].Controller).To(Equal(ptr.To(true)))
		Expect(sa.Labels).To(HaveKey(elementalv1.ElementalManagedLabel))

		secret := &corev1.Secret{}
		Expect(r.Get(ctx, types.NamespacedName{Namespace: mRegistration.Namespace, Name: mRegistration.Name + elementalv1.SASecretSuffix}, secret)).To(Succeed())
		Expect(secret.OwnerReferences).To(HaveLen(1))
		Expect(secret.OwnerReferences[0].APIVersion).To(Equal(elementalv1.GroupVersion.String()))
		Expect(secret.OwnerReferences[0].Kind).To(Equal("MachineRegistration"))
		Expect(secret.OwnerReferences[0].Name).To(Equal(mRegistration.Name))
		Expect(secret.OwnerReferences[0].UID).To(Equal(mRegistration.UID))
		Expect(secret.OwnerReferences[0].Controller).To(Equal(ptr.To(true)))
		Expect(secret.Annotations).To(HaveKeyWithValue("kubernetes.io/service-account.name", mRegistration.Name))
		Expect(secret.Type).To(Equal(corev1.SecretTypeServiceAccountToken))

		roleBinding := &rbacv1.RoleBinding{}
		Expect(r.Get(ctx, objKey, roleBinding)).To(Succeed())
		Expect(roleBinding.OwnerReferences).To(HaveLen(1))
		Expect(roleBinding.OwnerReferences[0].APIVersion).To(Equal(elementalv1.GroupVersion.String()))
		Expect(roleBinding.OwnerReferences[0].Kind).To(Equal("MachineRegistration"))
		Expect(roleBinding.OwnerReferences[0].Name).To(Equal(mRegistration.Name))
		Expect(roleBinding.OwnerReferences[0].UID).To(Equal(mRegistration.UID))
		Expect(roleBinding.OwnerReferences[0].Controller).To(Equal(ptr.To(true)))
		Expect(roleBinding.Labels).To(HaveKey(elementalv1.ElementalManagedLabel))

		Expect(roleBinding.Subjects).To(HaveLen(1))
		Expect(roleBinding.Subjects[0].Kind).To(Equal("ServiceAccount"))
		Expect(roleBinding.Subjects[0].Name).To(Equal(mRegistration.Name))
		Expect(roleBinding.Subjects[0].Namespace).To(Equal(mRegistration.Namespace))

		Expect(mRegistration.Status.ServiceAccountRef.Kind).To(Equal("ServiceAccount"))
		Expect(mRegistration.Status.ServiceAccountRef.Name).To(Equal(mRegistration.Name))
		Expect(mRegistration.Status.ServiceAccountRef.Namespace).To(Equal(mRegistration.Namespace))

	})

	It("shouldn't error when RBAC already exists", func() {
		Expect(r.Create(ctx, role)).To(Succeed())
		Expect(r.Create(ctx, sa)).To(Succeed())
		Expect(r.Create(ctx, roleBinding)).To(Succeed())
		Expect(r.Create(ctx, secret)).To(Succeed())
		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())
		Expect(mRegistration.Status.ServiceAccountRef.Kind).To(Equal("ServiceAccount"))
		Expect(mRegistration.Status.ServiceAccountRef.Name).To(Equal(mRegistration.Name))
		Expect(mRegistration.Status.ServiceAccountRef.Namespace).To(Equal(mRegistration.Namespace))
	})

	It("should preserve the single shared identity while split auth is disabled", func() {
		const globalServiceAccountName = "unused-global-system-agent"

		r.SystemAgentAuthMode = SystemAgentAuthModeShared
		r.SystemAgentServiceAccount = DefaultSharedSystemAgentServiceAccountName
		r.GlobalSystemAgentServiceAccount = globalServiceAccountName
		mRegistration.Annotations = map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeGlobal,
		}

		newInventory := func(name, planName, scope string) *elementalv1.MachineInventory {
			annotations := map[string]string{}
			if scope != "" {
				annotations[elementalv1.SystemAgentAuthScopeAnnotation] = scope
			}
			inventory := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   mRegistration.Namespace,
				Annotations: annotations,
			}}
			Expect(r.Create(ctx, inventory)).To(Succeed())
			inventory.Status.Plan = &elementalv1.PlanStatus{PlanSecretRef: &corev1.ObjectReference{
				Name:      planName,
				Namespace: mRegistration.Namespace,
			}}
			Expect(r.Status().Update(ctx, inventory)).To(Succeed())
			return inventory
		}

		sharedInventory := newInventory("legacy-shared", "plan-shared", elementalv1.SystemAgentAuthScopeShared)
		globalInventory := newInventory("legacy-global", "plan-global", elementalv1.SystemAgentAuthScopeGlobal)
		invalidInventory := newInventory("legacy-invalid", "plan-invalid", "invalid")
		sharedConflict := newInventory("legacy-shared-conflict", "same-plan", elementalv1.SystemAgentAuthScopeShared)
		globalConflict := newInventory("legacy-global-conflict", "same-plan", elementalv1.SystemAgentAuthScopeGlobal)
		sharedRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		sharedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		sharedRoleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		sharedTokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName + elementalv1.SASecretSuffix, Namespace: mRegistration.Namespace}}
		defer func() {
			Expect(test.CleanupAndWait(ctx, cl,
				sharedInventory, globalInventory, invalidInventory, sharedConflict, globalConflict,
				sharedRole, sharedSA, sharedRoleBinding, sharedTokenSecret,
			)).To(Succeed())
		}()

		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())
		Expect(mRegistration.Status.ServiceAccountRef.Name).To(Equal(DefaultSharedSystemAgentServiceAccountName))
		Expect(r.Get(ctx, client.ObjectKeyFromObject(sharedRole), sharedRole)).To(Succeed())
		Expect(sharedRole.Rules).To(Equal(systemAgentRules([]string{"plan-global", "plan-invalid", "plan-shared", "same-plan"})))

		mRegistration.Annotations[elementalv1.SystemAgentAuthScopeAnnotation] = "invalid"
		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())
		Expect(mRegistration.Status.ServiceAccountRef.Name).To(Equal(DefaultSharedSystemAgentServiceAccountName))

		globalKey := types.NamespacedName{Namespace: mRegistration.Namespace, Name: globalServiceAccountName}
		Expect(r.Get(ctx, globalKey, &corev1.ServiceAccount{})).To(MatchError(ContainSubstring("not found")))
		Expect(r.Get(ctx, globalKey, &rbacv1.Role{})).To(MatchError(ContainSubstring("not found")))
		Expect(r.Get(ctx, globalKey, &rbacv1.RoleBinding{})).To(MatchError(ContainSubstring("not found")))
		Expect(r.Get(ctx, types.NamespacedName{Namespace: mRegistration.Namespace, Name: globalServiceAccountName + elementalv1.SASecretSuffix}, &corev1.Secret{})).To(MatchError(ContainSubstring("not found")))
	})

	It("should validate the single shared identity in read-only mode while split auth is disabled", func() {
		r.SystemAgentAuthMode = SystemAgentAuthModeShared
		r.SystemAgentSharedAuthReadOnly = true
		mRegistration.Annotations = map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeGlobal,
		}

		newInventory := func(name, planName, scope string) *elementalv1.MachineInventory {
			inventory := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: mRegistration.Namespace,
				Annotations: map[string]string{
					elementalv1.SystemAgentAuthScopeAnnotation: scope,
				},
			}}
			Expect(r.Create(ctx, inventory)).To(Succeed())
			inventory.Status.Plan = &elementalv1.PlanStatus{PlanSecretRef: &corev1.ObjectReference{
				Name:      planName,
				Namespace: mRegistration.Namespace,
			}}
			Expect(r.Status().Update(ctx, inventory)).To(Succeed())
			return inventory
		}

		sharedInventory := newInventory("read-only-shared", "shared-plan", elementalv1.SystemAgentAuthScopeShared)
		globalInventory := newInventory("read-only-global", "global-plan", elementalv1.SystemAgentAuthScopeGlobal)
		sharedKey := types.NamespacedName{Namespace: mRegistration.Namespace, Name: DefaultSharedSystemAgentServiceAccountName}
		sharedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sharedKey.Name, Namespace: sharedKey.Namespace}}
		Expect(r.Create(ctx, sharedSA)).To(Succeed())
		sharedTokenSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sharedKey.Name + elementalv1.SASecretSuffix,
				Namespace: sharedKey.Namespace,
				Annotations: map[string]string{
					"kubernetes.io/service-account.name": sharedKey.Name,
					"kubernetes.io/service-account.uid":  string(sharedSA.UID),
				},
			},
			Data: map[string][]byte{"token": []byte("synced-token")},
			Type: corev1.SecretTypeServiceAccountToken,
		}
		sharedRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: sharedKey.Name, Namespace: sharedKey.Namespace},
			Rules:      systemAgentRules([]string{"global-plan", "shared-plan"}),
		}
		sharedRoleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: sharedKey.Name, Namespace: sharedKey.Namespace},
			Subjects: []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      sharedKey.Name,
				Namespace: sharedKey.Namespace,
			}},
			RoleRef: rbacv1.RoleRef{Kind: "Role", Name: sharedKey.Name, APIGroup: rbacv1.GroupName},
		}
		Expect(r.Create(ctx, sharedTokenSecret)).To(Succeed())
		Expect(r.Create(ctx, sharedRole)).To(Succeed())
		Expect(r.Create(ctx, sharedRoleBinding)).To(Succeed())
		defer func() {
			Expect(test.CleanupAndWait(ctx, cl,
				sharedInventory, globalInventory, sharedSA, sharedTokenSecret, sharedRole, sharedRoleBinding,
			)).To(Succeed())
		}()

		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())
		Expect(mRegistration.Status.ServiceAccountRef.Name).To(Equal(DefaultSharedSystemAgentServiceAccountName))
		Expect(r.Get(ctx, types.NamespacedName{Namespace: mRegistration.Namespace, Name: DefaultGlobalSystemAgentServiceAccountName}, &rbacv1.Role{})).To(MatchError(ContainSubstring("not found")))
	})

	It("should reconcile exact global and shared RBAC from scoped inventory status", func() {
		const globalServiceAccountName = "custom-global-system-agent"

		r.SystemAgentAuthMode = SystemAgentAuthModeShared
		r.SystemAgentServiceAccount = DefaultSharedSystemAgentServiceAccountName
		r.GlobalSystemAgentServiceAccount = globalServiceAccountName
		r.SystemAgentSplitAuthEnabled = true

		orphanPlanSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "orphan-plan-secret",
				Namespace: mRegistration.Namespace,
			},
			Type: elementalv1.PlanSecretType,
		}
		Expect(r.Create(ctx, orphanPlanSecret)).To(Succeed())

		newInventory := func(name, planName string, annotations map[string]string) *elementalv1.MachineInventory {
			inventory := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   mRegistration.Namespace,
				Annotations: annotations,
			}}
			Expect(r.Create(ctx, inventory)).To(Succeed())
			inventory.Status.Plan = &elementalv1.PlanStatus{PlanSecretRef: &corev1.ObjectReference{
				Name:      planName,
				Namespace: mRegistration.Namespace,
			}}
			Expect(r.Status().Update(ctx, inventory)).To(Succeed())
			return inventory
		}

		sharedInventory := newInventory("inventory-shared", "plan-shared", map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation:   elementalv1.SystemAgentAuthScopeShared,
			legacyMachineInventoryOwnerClusterAnnotation: globalClusterName,
		})
		globalInventory := newInventory("inventory-global", "plan-global", map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeGlobal,
		})
		legacyGlobalInventory := newInventory("inventory-legacy-global", "plan-legacy-global", map[string]string{
			legacyMachineInventoryOwnerClusterAnnotation: globalClusterName,
		})
		legacySharedInventory := newInventory("inventory-legacy-shared", "plan-legacy-shared", nil)
		invalidInventory := newInventory("inventory-invalid", "plan-invalid", map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: "invalid",
		})

		sharedRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		sharedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		sharedRoleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		sharedTokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName + elementalv1.SASecretSuffix, Namespace: mRegistration.Namespace}}
		globalRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: globalServiceAccountName, Namespace: mRegistration.Namespace}}
		globalSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: globalServiceAccountName, Namespace: mRegistration.Namespace}}
		globalRoleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: globalServiceAccountName, Namespace: mRegistration.Namespace}}
		globalTokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: globalServiceAccountName + elementalv1.SASecretSuffix, Namespace: mRegistration.Namespace}}
		defer func() {
			Expect(test.CleanupAndWait(ctx, cl,
				orphanPlanSecret,
				sharedInventory, globalInventory, legacyGlobalInventory, legacySharedInventory, invalidInventory,
				sharedRole, sharedSA, sharedRoleBinding, sharedTokenSecret,
				globalRole, globalSA, globalRoleBinding, globalTokenSecret,
			)).To(Succeed())
		}()

		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())
		sharedKey := types.NamespacedName{Namespace: mRegistration.Namespace, Name: DefaultSharedSystemAgentServiceAccountName}

		role := &rbacv1.Role{}
		Expect(r.Get(ctx, sharedKey, role)).To(Succeed())
		Expect(role.OwnerReferences).To(BeEmpty())
		Expect(role.Rules).To(HaveLen(1))
		Expect(role.Rules[0].Resources).To(Equal([]string{"secrets"}))
		Expect(role.Rules[0].ResourceNames).To(Equal([]string{"plan-legacy-shared", "plan-shared"}))

		sa := &corev1.ServiceAccount{}
		Expect(r.Get(ctx, sharedKey, sa)).To(Succeed())
		Expect(sa.OwnerReferences).To(BeEmpty())

		tokenSecret := &corev1.Secret{}
		Expect(r.Get(ctx, types.NamespacedName{Namespace: mRegistration.Namespace, Name: DefaultSharedSystemAgentServiceAccountName + elementalv1.SASecretSuffix}, tokenSecret)).To(Succeed())
		Expect(tokenSecret.OwnerReferences).To(BeEmpty())
		Expect(tokenSecret.Annotations).To(HaveKeyWithValue("kubernetes.io/service-account.name", DefaultSharedSystemAgentServiceAccountName))

		roleBinding := &rbacv1.RoleBinding{}
		Expect(r.Get(ctx, sharedKey, roleBinding)).To(Succeed())
		Expect(roleBinding.OwnerReferences).To(BeEmpty())
		Expect(roleBinding.Subjects[0].Name).To(Equal(DefaultSharedSystemAgentServiceAccountName))

		Expect(mRegistration.Status.ServiceAccountRef.Kind).To(Equal("ServiceAccount"))
		Expect(mRegistration.Status.ServiceAccountRef.Name).To(Equal(DefaultSharedSystemAgentServiceAccountName))
		Expect(mRegistration.Status.ServiceAccountRef.Namespace).To(Equal(mRegistration.Namespace))

		originalTokenUID := tokenSecret.UID
		tokenSecret.Data = map[string][]byte{"token": []byte("preserved-token")}
		Expect(r.Update(ctx, tokenSecret)).To(Succeed())
		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())
		Expect(r.Get(ctx, types.NamespacedName{Namespace: mRegistration.Namespace, Name: tokenSecret.Name}, tokenSecret)).To(Succeed())
		Expect(tokenSecret.UID).To(Equal(originalTokenUID))
		Expect(tokenSecret.Data).To(HaveKeyWithValue("token", []byte("preserved-token")))

		globalRegistration := mRegistration.DeepCopy()
		globalRegistration.Annotations = map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeGlobal,
		}
		Expect(r.createRBACObjects(ctx, globalRegistration)).To(Succeed())
		Expect(globalRegistration.Status.ServiceAccountRef.Name).To(Equal(globalServiceAccountName))

		globalKey := types.NamespacedName{Namespace: mRegistration.Namespace, Name: globalServiceAccountName}
		Expect(r.Get(ctx, globalKey, globalRole)).To(Succeed())
		Expect(globalRole.OwnerReferences).To(BeEmpty())
		Expect(globalRole.Rules).To(HaveLen(1))
		Expect(globalRole.Rules[0].ResourceNames).To(Equal([]string{"plan-global", "plan-legacy-global"}))

		sharedInventory.Status.Plan = nil
		Expect(r.Status().Update(ctx, sharedInventory)).To(Succeed())
		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())
		Expect(r.Get(ctx, sharedKey, role)).To(Succeed())
		Expect(role.Rules[0].ResourceNames).To(Equal([]string{"plan-legacy-shared"}))
	})

	It("should reject an invalid registration auth scope", func() {
		r.SystemAgentAuthMode = SystemAgentAuthModeShared
		r.SystemAgentSplitAuthEnabled = true
		mRegistration.Annotations = map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: "invalid",
		}

		err := r.createRBACObjects(ctx, mRegistration)
		Expect(err).To(MatchError(ContainSubstring("invalid system-agent auth scope")))
	})

	It("should exclude plan Secret names referenced by both scopes while deduplicating within one scope", func() {
		r.SystemAgentAuthMode = SystemAgentAuthModeShared
		r.SystemAgentSplitAuthEnabled = true

		newInventory := func(name, planName, scope string) *elementalv1.MachineInventory {
			inventory := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: mRegistration.Namespace,
				Annotations: map[string]string{
					elementalv1.SystemAgentAuthScopeAnnotation: scope,
				},
			}}
			Expect(r.Create(ctx, inventory)).To(Succeed())
			inventory.Status.Plan = &elementalv1.PlanStatus{PlanSecretRef: &corev1.ObjectReference{
				Name:      planName,
				Namespace: mRegistration.Namespace,
			}}
			Expect(r.Status().Update(ctx, inventory)).To(Succeed())
			return inventory
		}

		sharedOne := newInventory("shared-one", "same-shared-plan", elementalv1.SystemAgentAuthScopeShared)
		sharedTwo := newInventory("shared-two", "same-shared-plan", elementalv1.SystemAgentAuthScopeShared)
		sharedConflict := newInventory("shared-conflict", "cross-scope-plan", elementalv1.SystemAgentAuthScopeShared)
		globalConflict := newInventory("global-conflict", "cross-scope-plan", elementalv1.SystemAgentAuthScopeGlobal)
		globalNormal := newInventory("global-normal", "same-global-plan", elementalv1.SystemAgentAuthScopeGlobal)

		sharedRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName, Namespace: mRegistration.Namespace},
			Rules:      systemAgentRules([]string{"cross-scope-plan", "same-shared-plan"}),
		}
		sharedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		sharedRoleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		sharedTokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: DefaultSharedSystemAgentServiceAccountName + elementalv1.SASecretSuffix, Namespace: mRegistration.Namespace}}
		globalRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultGlobalSystemAgentServiceAccountName, Namespace: mRegistration.Namespace},
			Rules:      systemAgentRules([]string{"cross-scope-plan", "same-global-plan"}),
		}
		globalSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: DefaultGlobalSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		globalRoleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: DefaultGlobalSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		globalTokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: DefaultGlobalSystemAgentServiceAccountName + elementalv1.SASecretSuffix, Namespace: mRegistration.Namespace}}
		Expect(r.Create(ctx, sharedRole)).To(Succeed())
		Expect(r.Create(ctx, globalRole)).To(Succeed())
		defer func() {
			Expect(test.CleanupAndWait(ctx, cl,
				sharedOne, sharedTwo, sharedConflict, globalConflict, globalNormal,
				sharedRole, sharedSA, sharedRoleBinding, sharedTokenSecret,
				globalRole, globalSA, globalRoleBinding, globalTokenSecret,
			)).To(Succeed())
		}()

		Expect(r.createRBACObjects(ctx, mRegistration)).To(MatchError(ContainSubstring("referenced by both global and shared")))
		Expect(r.Get(ctx, client.ObjectKeyFromObject(sharedRole), sharedRole)).To(Succeed())
		Expect(sharedRole.Rules).To(Equal(systemAgentRules([]string{"same-shared-plan"})))

		globalRegistration := mRegistration.DeepCopy()
		globalRegistration.Annotations = map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeGlobal,
		}
		Expect(r.createRBACObjects(ctx, globalRegistration)).To(MatchError(ContainSubstring("referenced by both global and shared")))
		Expect(r.Get(ctx, client.ObjectKeyFromObject(globalRole), globalRole)).To(Succeed())
		Expect(globalRole.Rules).To(Equal(systemAgentRules([]string{"same-global-plan"})))
	})

	It("should leave shared auth untouched in read-only mode while reconciling global auth", func() {
		r.SystemAgentAuthMode = SystemAgentAuthModeShared
		r.SystemAgentSplitAuthEnabled = true
		r.SystemAgentSharedAuthReadOnly = true

		mRegistration.Annotations = map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeShared,
		}
		Expect(r.createRBACObjects(ctx, mRegistration)).To(MatchError(ContainSubstring("externally managed shared system-agent ServiceAccount")))
		Expect(mRegistration.Status.ServiceAccountRef.Name).To(Equal(DefaultSharedSystemAgentServiceAccountName))

		sharedKey := types.NamespacedName{Namespace: mRegistration.Namespace, Name: DefaultSharedSystemAgentServiceAccountName}
		sharedInventory := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{
			Name:      "synced-inventory",
			Namespace: mRegistration.Namespace,
			Annotations: map[string]string{
				elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeShared,
			},
		}}
		Expect(r.Create(ctx, sharedInventory)).To(Succeed())
		sharedInventory.Status.Plan = &elementalv1.PlanStatus{PlanSecretRef: &corev1.ObjectReference{
			Name:      "synced-plan",
			Namespace: mRegistration.Namespace,
		}}
		Expect(r.Status().Update(ctx, sharedInventory)).To(Succeed())

		sharedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name:      sharedKey.Name,
			Namespace: sharedKey.Namespace,
			Labels:    map[string]string{"source": "etcd-sync"},
		}}
		sharedTokenSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sharedKey.Name + elementalv1.SASecretSuffix,
				Namespace: sharedKey.Namespace,
				Annotations: map[string]string{
					"kubernetes.io/service-account.name": sharedKey.Name,
				},
			},
			Data: map[string][]byte{"token": []byte("synced-token")},
			Type: corev1.SecretTypeServiceAccountToken,
		}
		sharedRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: sharedKey.Name, Namespace: sharedKey.Namespace},
			Rules:      systemAgentRules([]string{"synced-plan"}),
		}
		sharedRoleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: sharedKey.Name, Namespace: sharedKey.Namespace},
			Subjects: []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      sharedKey.Name,
				Namespace: sharedKey.Namespace,
			}},
			RoleRef: rbacv1.RoleRef{Kind: "Role", Name: sharedKey.Name, APIGroup: rbacv1.GroupName},
		}
		Expect(r.Create(ctx, sharedSA)).To(Succeed())
		sharedTokenSecret.Annotations["kubernetes.io/service-account.uid"] = string(sharedSA.UID)
		Expect(r.Create(ctx, sharedTokenSecret)).To(Succeed())
		Expect(r.Create(ctx, sharedRole)).To(Succeed())
		Expect(r.Create(ctx, sharedRoleBinding)).To(Succeed())
		defer func() {
			Expect(test.CleanupAndWait(ctx, cl, sharedInventory, sharedSA, sharedTokenSecret, sharedRole, sharedRoleBinding)).To(Succeed())
		}()

		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())
		Expect(r.Get(ctx, sharedKey, sharedRole)).To(Succeed())
		Expect(sharedRole.Rules).To(Equal(systemAgentRules([]string{"synced-plan"})))
		Expect(r.Get(ctx, types.NamespacedName{Namespace: sharedKey.Namespace, Name: sharedTokenSecret.Name}, sharedTokenSecret)).To(Succeed())
		Expect(sharedTokenSecret.Data).To(HaveKeyWithValue("token", []byte("synced-token")))

		sharedTokenSecret.Annotations["kubernetes.io/service-account.uid"] = "wrong-uid"
		Expect(r.Update(ctx, sharedTokenSecret)).To(Succeed())
		Expect(r.createRBACObjects(ctx, mRegistration)).To(MatchError(ContainSubstring("references ServiceAccount UID")))
		sharedTokenSecret.Annotations["kubernetes.io/service-account.uid"] = string(sharedSA.UID)
		Expect(r.Update(ctx, sharedTokenSecret)).To(Succeed())

		sharedRole.Rules = []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}
		Expect(r.Update(ctx, sharedRole)).To(Succeed())
		Expect(r.createRBACObjects(ctx, mRegistration)).To(MatchError(ContainSubstring("rules do not match")))
		sharedRole.Rules = systemAgentRules([]string{"synced-plan"})
		Expect(r.Update(ctx, sharedRole)).To(Succeed())

		sharedRoleBinding.Subjects = append(sharedRoleBinding.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "extra", Namespace: sharedKey.Namespace})
		Expect(r.Update(ctx, sharedRoleBinding)).To(Succeed())
		Expect(r.createRBACObjects(ctx, mRegistration)).To(MatchError(ContainSubstring("must have exactly one subject")))
		sharedRoleBinding.Subjects = sharedRoleBinding.Subjects[:1]
		Expect(r.Update(ctx, sharedRoleBinding)).To(Succeed())
		Expect(r.createRBACObjects(ctx, mRegistration)).To(Succeed())

		globalRegistration := mRegistration.DeepCopy()
		globalRegistration.Annotations[elementalv1.SystemAgentAuthScopeAnnotation] = elementalv1.SystemAgentAuthScopeGlobal
		globalRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: DefaultGlobalSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		globalSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: DefaultGlobalSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		globalRoleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: DefaultGlobalSystemAgentServiceAccountName, Namespace: mRegistration.Namespace}}
		globalTokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: DefaultGlobalSystemAgentServiceAccountName + elementalv1.SASecretSuffix, Namespace: mRegistration.Namespace}}
		defer func() {
			Expect(test.CleanupAndWait(ctx, cl, globalRole, globalSA, globalRoleBinding, globalTokenSecret)).To(Succeed())
		}()

		Expect(r.createRBACObjects(ctx, globalRegistration)).To(Succeed())
		Expect(globalRegistration.Status.ServiceAccountRef.Name).To(Equal(DefaultGlobalSystemAgentServiceAccountName))
		Expect(r.Get(ctx, client.ObjectKeyFromObject(globalRole), globalRole)).To(Succeed())
	})

	It("should error when RBAC fails to be created", func() {
		r.Client = machineRegistrationFailingClient{}
		err := r.createRBACObjects(ctx, mRegistration)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to create"))
	})
})

type machineRegistrationFailingClient struct {
	client.Client
}

func (cl machineRegistrationFailingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return errors.New("failed to create")
}

func (cl machineRegistrationFailingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return errors.New("failed to delete")
}
