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
	"fmt"
	"sort"
	"strings"

	"github.com/google/go-cmp/cmp"
	"github.com/rancher/wrangler/v3/pkg/randomtoken"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	errorutils "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/util"
)

// MachineRegistrationReconciler reconciles a MachineRegistration object.
type MachineRegistrationReconciler struct {
	client.Client
	ServerURL                       string
	SystemAgentAuthMode             string
	SystemAgentServiceAccount       string
	GlobalSystemAgentServiceAccount string
	SystemAgentSplitAuthEnabled     bool
	SystemAgentSharedAuthReadOnly   bool
}

type planSecretScopeConflictError struct {
	Names []string
}

func (e *planSecretScopeConflictError) Error() string {
	return fmt.Sprintf("plan Secrets are referenced by both global and shared MachineInventories: %s", strings.Join(e.Names, ", "))
}

const (
	SystemAgentAuthModeRegistration = "registration"
	SystemAgentAuthModeShared       = "shared"

	DefaultSharedSystemAgentServiceAccountName = "baremetal-system-agent"
	DefaultGlobalSystemAgentServiceAccountName = "baremetal-global-system-agent"

	legacyMachineInventoryOwnerClusterAnnotation = "baremetal.alauda.io/owner-cluster"
	globalClusterName                            = "global"
)

func NormalizeSystemAgentAuthMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return SystemAgentAuthModeRegistration
	}
	return mode
}

// +kubebuilder:rbac:groups=elemental.cattle.io,resources=machineregistrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=elemental.cattle.io,resources=machineregistrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings;roles,verbs=create;delete;get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create;delete;get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;delete;get;list;watch;update;patch
// +kubebuilder:rbac:groups=elemental.cattle.io,resources=machineinventories,verbs=get;list;watch

func (r *MachineRegistrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&elementalv1.MachineRegistration{}).
		Owns(&corev1.ServiceAccount{}).
		WithEventFilter(r.ignoreIncrementalStatusUpdate())
	if r.sharedAuthEnabled() {
		builder = builder.Watches(
			&elementalv1.MachineInventory{},
			handler.EnqueueRequestsFromMapFunc(r.machineInventoryToMachineRegistrations),
		)
	}
	return builder.Complete(r)
}

func (r *MachineRegistrationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) { //nolint:dupl
	logger := ctrl.LoggerFrom(ctx)

	mRegistration := &elementalv1.MachineRegistration{}
	err := r.Get(ctx, req.NamespacedName, mRegistration)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(log.DebugDepth).Info("Object was not found, not an error")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get machine registration object: %w", err)
	}

	// Ensure we patch the latest version otherwise we could erratically overlap with other controllers (e.g. backup and restore)
	patchBase := client.MergeFromWithOptions(mRegistration.DeepCopy(), client.MergeFromWithOptimisticLock{})

	// We have to sanitize the conditions because old API definitions didn't have proper validation.
	mRegistration.Status.Conditions = util.RemoveInvalidConditions(mRegistration.Status.Conditions)

	// Collect errors as an aggregate to return together after all patches have been performed.
	var errs []error

	result, err := r.reconcile(ctx, mRegistration)
	if err != nil {
		errs = append(errs, fmt.Errorf("error reconciling machine registration object: %w", err))
	}

	machineRegistrationStatusCopy := mRegistration.Status.DeepCopy() // Patch call will erase the status

	if err := r.Patch(ctx, mRegistration, patchBase); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("failed to patch machine registration object: %w", err))
	}

	mRegistration.Status = *machineRegistrationStatusCopy

	if err := r.Status().Patch(ctx, mRegistration, patchBase); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("failed to patch status for machine registration object: %w", err))
	}

	if len(errs) > 0 {
		return ctrl.Result{}, errorutils.NewAggregate(errs)
	}

	return result, nil
}

func (r *MachineRegistrationReconciler) reconcile(ctx context.Context, mRegistration *elementalv1.MachineRegistration) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Reconciling machineregistration object")

	if mRegistration.GetDeletionTimestamp() != nil {
		controllerutil.RemoveFinalizer(mRegistration, elementalv1.MachineRegistrationFinalizer)
		return ctrl.Result{}, nil
	}

	if !r.sharedAuthEnabled() && r.isReady(ctx, mRegistration) {
		logger.Info("Machine registration is ready, no need to reconcile it")
		return ctrl.Result{}, nil
	}

	if err := r.setRegistrationTokenAndURL(ctx, mRegistration); err != nil {
		meta.SetStatusCondition(&mRegistration.Status.Conditions, metav1.Condition{
			Type:    elementalv1.ReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  elementalv1.MissingTokenOrServerURLReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, fmt.Errorf("failed to set registration token and url: %w", err)
	}

	if err := r.createRBACObjects(ctx, mRegistration); err != nil {
		meta.SetStatusCondition(&mRegistration.Status.Conditions, metav1.Condition{
			Type:    elementalv1.ReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  elementalv1.RbacCreationFailureReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, fmt.Errorf("failed to create RBAC objects: %w", err)
	}

	meta.SetStatusCondition(&mRegistration.Status.Conditions, metav1.Condition{
		Type:   elementalv1.ReadyCondition,
		Reason: elementalv1.SuccessfullyCreatedReason,
		Status: metav1.ConditionTrue,
	})

	return ctrl.Result{}, nil
}

func (r *MachineRegistrationReconciler) isReady(ctx context.Context, mRegistration *elementalv1.MachineRegistration) bool {
	if meta.IsStatusConditionTrue(mRegistration.Status.Conditions, elementalv1.ReadyCondition) {
		// Despite being on ready state we check if the serviceaccount token is still available as it can be deleted
		// by the control plane during backup & restore operations see: rancher/elemental#776
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: mRegistration.Namespace,
			Name:      r.serviceAccountTokenSecretName(mRegistration),
		}, &corev1.Secret{}); err != nil {
			return false
		}
		return true
	}

	return false
}

func (r *MachineRegistrationReconciler) setRegistrationTokenAndURL(ctx context.Context, mRegistration *elementalv1.MachineRegistration) error {
	var err error
	var serverURL string

	logger := ctrl.LoggerFrom(ctx)
	logger.Info("Setting registration token and url")

	if mRegistration.Status.RegistrationToken == "" {
		mRegistration.Status.RegistrationToken, err = randomtoken.Generate()
		if err != nil {
			return fmt.Errorf("failed to generate registration token: %w", err)
		}
	}

	if mRegistration.Status.RegistrationURL == "" {
		serverURL, err = r.getServerURL()
		if err != nil {
			return fmt.Errorf("failed to get the server url: %w", err)
		}
		mRegistration.Status.RegistrationURL = fmt.Sprintf("%s/elemental/registration/%s", serverURL, mRegistration.Status.RegistrationToken)
	}

	return nil
}

func (r *MachineRegistrationReconciler) getServerURL() (string, error) {
	serverURL := strings.TrimRight(r.ServerURL, "/")
	if serverURL == "" {
		err := errors.New("server-url is not set")
		return "", err
	}

	return serverURL, nil
}

func (r *MachineRegistrationReconciler) createRBACObjects(ctx context.Context, mRegistration *elementalv1.MachineRegistration) error {
	if r.sharedAuthEnabled() {
		if !r.splitAuthEnabled() {
			return r.createScopedRBACObjects(ctx, mRegistration, elementalv1.SystemAgentAuthScopeShared)
		}
		scope, err := elementalv1.ResolveSystemAgentAuthScope(mRegistration.Annotations)
		if err != nil {
			return err
		}
		return r.createScopedRBACObjects(ctx, mRegistration, scope)
	}
	return r.createRegistrationRBACObjects(ctx, mRegistration)
}

func (r *MachineRegistrationReconciler) createRegistrationRBACObjects(ctx context.Context, mRegistration *elementalv1.MachineRegistration) error {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Reconciling RBAC resources")

	ownerReferences := []metav1.OwnerReference{
		{
			APIVersion: elementalv1.GroupVersion.String(),
			Kind:       "MachineRegistration",
			Name:       mRegistration.Name,
			UID:        mRegistration.UID,
			Controller: ptr.To(true),
		},
	}

	logger.Info("Creating role")
	if err := r.Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:            mRegistration.Name,
			Namespace:       mRegistration.Namespace,
			OwnerReferences: ownerReferences,
			Labels: map[string]string{
				elementalv1.ElementalManagedLabel: "true",
			},
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Verbs:     []string{"get", "watch", "list", "update", "patch"}, // TODO: Review permissions, does it need update, patch?
			Resources: []string{"secrets"},
		},
		},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create role: %w", err)
	}

	logger.Info("Creating service account")
	if err := r.Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            mRegistration.Name,
			Namespace:       mRegistration.Namespace,
			OwnerReferences: ownerReferences,
			Labels: map[string]string{
				elementalv1.ElementalManagedLabel: "true",
			},
		},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("Failed to create service account: %w", err)
	}

	logger.Info("Creating token secret for the service account")
	if err := r.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            mRegistration.Name + elementalv1.SASecretSuffix,
			Namespace:       mRegistration.Namespace,
			OwnerReferences: ownerReferences,
			Annotations: map[string]string{
				"kubernetes.io/service-account.name": mRegistration.Name,
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create secret: %w", err)
	}

	logger.Info("Creating role binding")
	if err := r.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       mRegistration.Namespace,
			Name:            mRegistration.Name,
			OwnerReferences: ownerReferences,
			Labels: map[string]string{
				elementalv1.ElementalManagedLabel: "true",
			},
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      mRegistration.Name,
			Namespace: mRegistration.Namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			Kind:     "Role",
			Name:     mRegistration.Name,
			APIGroup: "rbac.authorization.k8s.io",
		},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create service account: %w", err)
	}

	logger.Info("Setting service account ref")
	mRegistration.Status.ServiceAccountRef = &corev1.ObjectReference{
		Kind:      "ServiceAccount",
		Namespace: mRegistration.Namespace,
		Name:      mRegistration.Name,
	}

	return nil
}

func (r *MachineRegistrationReconciler) createScopedRBACObjects(ctx context.Context, mRegistration *elementalv1.MachineRegistration, scope string) error {
	logger := ctrl.LoggerFrom(ctx)
	logger.Info("Reconciling scoped system-agent RBAC resources", "scope", scope)

	saName := r.serviceAccountNameForScope(scope)
	secretName := saName + elementalv1.SASecretSuffix
	mRegistration.Status.ServiceAccountRef = &corev1.ObjectReference{
		Kind:      "ServiceAccount",
		Namespace: mRegistration.Namespace,
		Name:      saName,
	}

	if scope == elementalv1.SystemAgentAuthScopeShared && r.SystemAgentSharedAuthReadOnly {
		return r.validateExternallyManagedSystemAgentAuth(ctx, mRegistration.Namespace, saName)
	}

	labels := map[string]string{
		elementalv1.ElementalManagedLabel: "true",
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: mRegistration.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, sa, func() error {
		mergeLabels(sa, labels)
		sa.OwnerReferences = nil
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile %s system-agent service account: %w", scope, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: mRegistration.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, secret, func() error {
		mergeLabels(secret, labels)
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations["kubernetes.io/service-account.name"] = saName
		secret.OwnerReferences = nil
		secret.Type = corev1.SecretTypeServiceAccountToken
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile %s system-agent token secret: %w", scope, err)
	}

	planSecretNames, planSecretErr := r.planSecretNames(ctx, mRegistration.Namespace, scope)
	var scopeConflict *planSecretScopeConflictError
	if planSecretErr != nil && !errors.As(planSecretErr, &scopeConflict) {
		return planSecretErr
	}
	// A scope conflict still returns a safe set with every conflicting name
	// removed. Reconcile that set first so stale Roles cannot retain cross-scope
	// access, then return the conflict below to keep the registration NotReady.
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: mRegistration.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, role, func() error {
		mergeLabels(role, labels)
		role.OwnerReferences = nil
		role.Rules = systemAgentRules(planSecretNames)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile %s system-agent role: %w", scope, err)
	}

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: mRegistration.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, roleBinding, func() error {
		mergeLabels(roleBinding, labels)
		roleBinding.OwnerReferences = nil
		roleBinding.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: mRegistration.Namespace,
		}}
		roleBinding.RoleRef = rbacv1.RoleRef{
			Kind:     "Role",
			Name:     saName,
			APIGroup: "rbac.authorization.k8s.io",
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile %s system-agent role binding: %w", scope, err)
	}

	return planSecretErr
}

func (r *MachineRegistrationReconciler) validateExternallyManagedSystemAgentAuth(ctx context.Context, namespace, saName string) error {
	key := types.NamespacedName{Name: saName, Namespace: namespace}
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, key, sa); err != nil {
		return fmt.Errorf("externally managed shared system-agent ServiceAccount %s/%s is not ready: %w", namespace, saName, err)
	}

	secretKey := types.NamespacedName{Name: saName + elementalv1.SASecretSuffix, Namespace: namespace}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return fmt.Errorf("externally managed shared system-agent token Secret %s/%s is not ready: %w", namespace, secretKey.Name, err)
	}
	if secret.Type != corev1.SecretTypeServiceAccountToken ||
		secret.Annotations["kubernetes.io/service-account.name"] != saName ||
		len(secret.Data["token"]) == 0 {
		return fmt.Errorf("externally managed shared system-agent token Secret %s/%s is invalid", namespace, secretKey.Name)
	}
	if tokenSAUID := secret.Annotations["kubernetes.io/service-account.uid"]; tokenSAUID != "" && tokenSAUID != string(sa.UID) {
		return fmt.Errorf("externally managed shared system-agent token Secret %s/%s references ServiceAccount UID %q, expected %q",
			namespace, secretKey.Name, tokenSAUID, sa.UID)
	}

	role := &rbacv1.Role{}
	if err := r.Get(ctx, key, role); err != nil {
		return fmt.Errorf("externally managed shared system-agent Role %s/%s is not ready: %w", namespace, saName, err)
	}
	expectedPlanSecretNames, planSecretErr := r.planSecretNames(ctx, namespace, elementalv1.SystemAgentAuthScopeShared)
	var scopeConflict *planSecretScopeConflictError
	if planSecretErr != nil && !errors.As(planSecretErr, &scopeConflict) {
		return planSecretErr
	}
	expectedRules := systemAgentRules(expectedPlanSecretNames)
	if !(len(role.Rules) == 0 && len(expectedRules) == 0) && !cmp.Equal(role.Rules, expectedRules) {
		return fmt.Errorf("externally managed shared system-agent Role %s/%s rules do not match expected MachineInventory plan Secret references: %s",
			namespace, saName, cmp.Diff(expectedRules, role.Rules))
	}

	roleBinding := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, key, roleBinding); err != nil {
		return fmt.Errorf("externally managed shared system-agent RoleBinding %s/%s is not ready: %w", namespace, saName, err)
	}
	if roleBinding.RoleRef.Kind != "Role" || roleBinding.RoleRef.Name != saName || roleBinding.RoleRef.APIGroup != rbacv1.GroupName {
		return fmt.Errorf("externally managed shared system-agent RoleBinding %s/%s has an invalid roleRef", namespace, saName)
	}
	if len(roleBinding.Subjects) != 1 {
		return fmt.Errorf("externally managed shared system-agent RoleBinding %s/%s must have exactly one subject", namespace, saName)
	}
	subject := roleBinding.Subjects[0]
	if subject.APIGroup != "" || subject.Kind != "ServiceAccount" || subject.Name != saName || subject.Namespace != namespace {
		return fmt.Errorf("externally managed shared system-agent RoleBinding %s/%s does not bind only ServiceAccount %s/%s", namespace, saName, namespace, saName)
	}
	return planSecretErr
}

func (r *MachineRegistrationReconciler) sharedAuthEnabled() bool {
	return NormalizeSystemAgentAuthMode(r.SystemAgentAuthMode) == SystemAgentAuthModeShared
}

func (r *MachineRegistrationReconciler) splitAuthEnabled() bool {
	return r.sharedAuthEnabled() && r.SystemAgentSplitAuthEnabled
}

func (r *MachineRegistrationReconciler) sharedServiceAccountName() string {
	name := strings.TrimSpace(r.SystemAgentServiceAccount)
	if name == "" {
		return DefaultSharedSystemAgentServiceAccountName
	}
	return name
}

func (r *MachineRegistrationReconciler) globalServiceAccountName() string {
	name := strings.TrimSpace(r.GlobalSystemAgentServiceAccount)
	if name == "" {
		return DefaultGlobalSystemAgentServiceAccountName
	}
	return name
}

func (r *MachineRegistrationReconciler) serviceAccountNameForScope(scope string) string {
	if scope == elementalv1.SystemAgentAuthScopeGlobal {
		return r.globalServiceAccountName()
	}
	return r.sharedServiceAccountName()
}

func (r *MachineRegistrationReconciler) serviceAccountTokenSecretName(mRegistration *elementalv1.MachineRegistration) string {
	if mRegistration.Status.ServiceAccountRef != nil && mRegistration.Status.ServiceAccountRef.Name != "" {
		return mRegistration.Status.ServiceAccountRef.Name + elementalv1.SASecretSuffix
	}
	if r.sharedAuthEnabled() {
		if !r.splitAuthEnabled() {
			return r.sharedServiceAccountName() + elementalv1.SASecretSuffix
		}
		scope, err := elementalv1.ResolveSystemAgentAuthScope(mRegistration.Annotations)
		if err == nil {
			return r.serviceAccountNameForScope(scope) + elementalv1.SASecretSuffix
		}
	}
	return mRegistration.Name + elementalv1.SASecretSuffix
}

func (r *MachineRegistrationReconciler) planSecretNames(ctx context.Context, namespace, scope string) ([]string, error) {
	if !r.splitAuthEnabled() {
		return r.allPlanSecretNames(ctx, namespace)
	}

	planScopes := map[string]map[string]struct{}{}

	inventories := &elementalv1.MachineInventoryList{}
	if err := r.List(ctx, inventories, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list machine inventories for %s system-agent role: %w", scope, err)
	}
	for i := range inventories.Items {
		inventory := &inventories.Items[i]
		inventoryScope, valid := machineInventorySystemAgentAuthScope(inventory)
		if !valid {
			ctrl.LoggerFrom(ctx).Info("Skipping MachineInventory with invalid system-agent auth scope",
				"machineInventory", client.ObjectKeyFromObject(inventory),
				"scope", inventory.Annotations[elementalv1.SystemAgentAuthScopeAnnotation])
			continue
		}
		if inventory.Status.Plan == nil || inventory.Status.Plan.PlanSecretRef == nil {
			continue
		}
		ref := inventory.Status.Plan.PlanSecretRef
		if ref.Name == "" {
			continue
		}
		if ref.Namespace != "" && ref.Namespace != namespace {
			continue
		}
		if planScopes[ref.Name] == nil {
			planScopes[ref.Name] = map[string]struct{}{}
		}
		planScopes[ref.Name][inventoryScope] = struct{}{}
	}

	out := make([]string, 0, len(planScopes))
	conflicts := []string{}
	for name, scopes := range planScopes {
		if len(scopes) > 1 {
			conflicts = append(conflicts, name)
			continue
		}
		if _, found := scopes[scope]; found {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return out, &planSecretScopeConflictError{Names: conflicts}
	}
	return out, nil
}

func (r *MachineRegistrationReconciler) allPlanSecretNames(ctx context.Context, namespace string) ([]string, error) {
	names := map[string]struct{}{}

	inventories := &elementalv1.MachineInventoryList{}
	if err := r.List(ctx, inventories, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list machine inventories for shared system-agent role: %w", err)
	}
	for i := range inventories.Items {
		inventory := &inventories.Items[i]
		if inventory.Status.Plan == nil || inventory.Status.Plan.PlanSecretRef == nil {
			continue
		}
		ref := inventory.Status.Plan.PlanSecretRef
		if ref.Name == "" || (ref.Namespace != "" && ref.Namespace != namespace) {
			continue
		}
		names[ref.Name] = struct{}{}
	}

	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func machineInventorySystemAgentAuthScope(inventory *elementalv1.MachineInventory) (string, bool) {
	if inventory.Annotations != nil {
		if rawScope, found := inventory.Annotations[elementalv1.SystemAgentAuthScopeAnnotation]; found {
			scope := elementalv1.NormalizeSystemAgentAuthScope(rawScope)
			switch scope {
			case elementalv1.SystemAgentAuthScopeGlobal, elementalv1.SystemAgentAuthScopeShared:
				return scope, true
			default:
				return "", false
			}
		}
		if strings.EqualFold(strings.TrimSpace(inventory.Annotations[legacyMachineInventoryOwnerClusterAnnotation]), globalClusterName) {
			return elementalv1.SystemAgentAuthScopeGlobal, true
		}
	}
	return elementalv1.SystemAgentAuthScopeShared, true
}

func systemAgentRules(planSecretNames []string) []rbacv1.PolicyRule {
	if len(planSecretNames) == 0 {
		return nil
	}
	return []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Verbs:         []string{"get", "watch", "list", "update", "patch"},
		Resources:     []string{"secrets"},
		ResourceNames: planSecretNames,
	}}
}

func mergeLabels(obj client.Object, labels map[string]string) {
	if obj.GetLabels() == nil {
		obj.SetLabels(map[string]string{})
	}
	current := obj.GetLabels()
	for k, v := range labels {
		current[k] = v
	}
	obj.SetLabels(current)
}

func (r *MachineRegistrationReconciler) machineInventoryToMachineRegistrations(ctx context.Context, obj client.Object) []reconcile.Request {
	if !r.sharedAuthEnabled() {
		return nil
	}
	registrations := &elementalv1.MachineRegistrationList{}
	if err := r.List(ctx, registrations, client.InNamespace(obj.GetNamespace())); err != nil {
		log.Errorf("failed to list MachineRegistrations for shared role update: %s", err.Error())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(registrations.Items))
	for i := range registrations.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: registrations.Items[i].Namespace,
			Name:      registrations.Items[i].Name,
		}})
	}
	return requests
}

func (r *MachineRegistrationReconciler) ignoreIncrementalStatusUpdate() predicate.Funcs {
	return predicate.Funcs{
		// Avoid reconciling if the event triggering the reconciliation is related to incremental status updates
		// for MachineRegistration resources only
		UpdateFunc: func(e event.UpdateEvent) bool {
			logger := ctrl.LoggerFrom(context.Background())

			if oldMRegistration, ok := e.ObjectOld.(*elementalv1.MachineRegistration); ok {
				oldMRegistration = oldMRegistration.DeepCopy()
				newMregistration := e.ObjectNew.(*elementalv1.MachineRegistration).DeepCopy()

				// Ignore all fields that might be updated on a status update
				oldMRegistration.Status = elementalv1.MachineRegistrationStatus{}
				newMregistration.Status = elementalv1.MachineRegistrationStatus{}
				oldMRegistration.ObjectMeta.ResourceVersion = ""
				newMregistration.ObjectMeta.ResourceVersion = ""
				oldMRegistration.ManagedFields = []metav1.ManagedFieldsEntry{}
				newMregistration.ManagedFields = []metav1.ManagedFieldsEntry{}

				update := !cmp.Equal(oldMRegistration, newMregistration)
				if !update {
					logger.V(log.DebugDepth).Info("Ignoring status update", "MRegistration", oldMRegistration.Name)
				}
				return !cmp.Equal(oldMRegistration, newMregistration)
			}
			// Return true in case it watches other resources
			return true
		},
	}
}
