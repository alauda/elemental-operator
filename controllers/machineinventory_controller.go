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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rancher/yip/pkg/schema"
	corev1 "k8s.io/api/core/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"gopkg.in/yaml.v3"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	systemagent "github.com/rancher/elemental-operator/internal/system-agent"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/network"
	"github.com/rancher/elemental-operator/pkg/util"
)

const LocalResetPlanPath = "/oem/reset-cloud-config.yaml"
const LocalResetUnmanagedMarker = "/var/lib/elemental/.unmanaged_reset"

// MachineInventoryReconciler reconciles a MachineInventory object.
type MachineInventoryReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=elemental.cattle.io,resources=machineinventories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=elemental.cattle.io,resources=machineinventories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;watch;create;list

func (r *MachineInventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&elementalv1.MachineInventory{}).
		Owns(&corev1.Secret{}).
		WithEventFilter(r.ignoreIncrementalStatusUpdate()).
		Complete(r)
}

func (r *MachineInventoryReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) { //nolint:dupl
	logger := ctrl.LoggerFrom(ctx)

	mInventory := &elementalv1.MachineInventory{}
	err := r.Get(ctx, req.NamespacedName, mInventory)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(log.DebugDepth).Info("Object was not found, not an error")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get machine inventory object: %w", err)
	}

	// Ensure we patch the latest version otherwise we could erratically overlap with other controllers (e.g. backup and restore)
	patchBase := client.MergeFromWithOptions(mInventory.DeepCopy(), client.MergeFromWithOptimisticLock{})

	// We have to sanitize the conditions because old API definitions didn't have proper validation.
	mInventory.Status.Conditions = util.RemoveInvalidConditions(mInventory.Status.Conditions)

	// Collect errors as an aggregate to return together after all patches have been performed.
	var errs []error

	result, err := r.reconcile(ctx, mInventory)
	if err != nil {
		errs = append(errs, fmt.Errorf("error reconciling machine inventory object: %w", err))
	}

	machineInventoryStatusCopy := mInventory.Status.DeepCopy() // Patch call will erase the status

	if err := r.Patch(ctx, mInventory, patchBase); err != nil {
		errs = append(errs, fmt.Errorf("failed to patch machine inventory object: %w", err))
	}

	mInventory.Status = *machineInventoryStatusCopy

	// If the object was waiting for deletion and we just removed the finalizer, we will get a not found error
	if err := r.Status().Patch(ctx, mInventory, patchBase); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("failed to patch status for machine inventory object: %w", err))
	}

	if len(errs) > 0 {
		return ctrl.Result{}, errorutils.NewAggregate(errs)
	}

	return result, nil
}

func (r *MachineInventoryReconciler) reconcile(ctx context.Context, mInventory *elementalv1.MachineInventory) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Reconciling machineinventory object")

	if mInventory.GetDeletionTimestamp() == nil || mInventory.GetDeletionTimestamp().IsZero() {
		// The object is not being deleted, so register the finalizer
		if !controllerutil.ContainsFinalizer(mInventory, elementalv1.MachineInventoryFinalizer) {
			controllerutil.AddFinalizer(mInventory, elementalv1.MachineInventoryFinalizer)
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	} else {
		// The object is up for deletion
		if controllerutil.ContainsFinalizer(mInventory, elementalv1.MachineInventoryFinalizer) {
			if err := r.reconcileResetPlanSecret(ctx, mInventory); err != nil {
				meta.SetStatusCondition(&mInventory.Status.Conditions, metav1.Condition{
					Type:    elementalv1.ReadyCondition,
					Reason:  elementalv1.PlanFailureReason,
					Status:  metav1.ConditionFalse,
					Message: err.Error(),
				})
				return ctrl.Result{}, fmt.Errorf("reconciling reset plan secret: %w", err)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, nil
	}

	if err := r.createPlanSecret(ctx, mInventory); err != nil {
		meta.SetStatusCondition(&mInventory.Status.Conditions, metav1.Condition{
			Type:    elementalv1.ReadyCondition,
			Reason:  elementalv1.PlanCreationFailureReason,
			Status:  metav1.ConditionFalse,
			Message: err.Error(),
		})
		return ctrl.Result{}, fmt.Errorf("failed to create plan secret: %w", err)
	}

	if isNetworkConfigReadyWithoutReconciliation(mInventory) {
		meta.SetStatusCondition(&mInventory.Status.Conditions, metav1.Condition{
			Type:    elementalv1.NetworkConfigReady,
			Reason:  elementalv1.ReconcilingNetworkConfig,
			Status:  metav1.ConditionTrue,
			Message: "NetworkConfig is ready",
		})
	}

	if err := r.updateInventoryWithPlanStatus(ctx, mInventory); err != nil {
		meta.SetStatusCondition(&mInventory.Status.Conditions, metav1.Condition{
			Type:    elementalv1.ReadyCondition,
			Reason:  elementalv1.PlanFailureReason,
			Status:  metav1.ConditionFalse,
			Message: err.Error(),
		})
		return ctrl.Result{}, fmt.Errorf("failed to update inventory status with plan %w", err)
	}

	return ctrl.Result{}, nil
}

func isNetworkConfigReadyWithoutReconciliation(mInventory *elementalv1.MachineInventory) bool {
	configurator := mInventory.Spec.Network.Configurator
	return configurator == "" || configurator == network.ConfiguratorNone
}

func (r *MachineInventoryReconciler) reconcileResetPlanSecret(ctx context.Context, mInventory *elementalv1.MachineInventory) error {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Reconciling Reset plan")

	resettable, resettableFound := mInventory.Annotations[elementalv1.MachineInventoryResettableAnnotation]
	if !resettableFound || resettable != "true" {
		logger.V(log.DebugDepth).Info("Machine Inventory does not need reset. Removing finalizer.")
		controllerutil.RemoveFinalizer(mInventory, elementalv1.MachineInventoryFinalizer)
		return nil
	}

	if mInventory.Status.Plan == nil || mInventory.Status.Plan.PlanSecretRef == nil {
		return errors.New("machine inventory plan secret does not exist")
	}

	planSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: mInventory.Status.Plan.PlanSecretRef.Namespace,
		Name:      mInventory.Status.Plan.PlanSecretRef.Name,
	}, planSecret); err != nil {
		return fmt.Errorf("getting plan secret: %w", err)
	}

	if !util.IsObjectOwned(&planSecret.ObjectMeta, mInventory.UID) {
		return fmt.Errorf("secret already exists and was not created by this controller")
	}

	planType, annotationFound := planSecret.Annotations[elementalv1.PlanTypeAnnotation]

	if !annotationFound || planType != elementalv1.PlanTypeReset {
		logger.V(log.DebugDepth).Info("Non reset plan type found. Updating it with new reset plan.")
		return r.updatePlanSecretWithReset(ctx, mInventory, planSecret)
	}

	logger.V(log.DebugDepth).Info("Reset plan type found. Updating status and determine whether it was successfully applied.")
	if err := r.updateInventoryWithPlanStatus(ctx, mInventory); err != nil {
		return fmt.Errorf("updating inventory with plan status: %w", err)
	}
	if mInventory.Status.Plan.State == elementalv1.PlanApplied {
		logger.V(log.DebugDepth).Info("Reset plan was successfully applied.")
		controllerutil.RemoveFinalizer(mInventory, elementalv1.MachineInventoryFinalizer)
	}

	return nil
}

func (r *MachineInventoryReconciler) updatePlanSecretWithReset(ctx context.Context, mInventory *elementalv1.MachineInventory, planSecret *corev1.Secret) error {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Updating Secret with Reset plan")

	var checksum string
	var resetPlan []byte
	var err error

	networkNeedsReset := mInventory.Spec.Network.Configurator != network.ConfiguratorNone

	unmanaged, unmanagedFound := mInventory.Annotations[elementalv1.MachineInventoryOSUnmanagedAnnotation]
	if unmanagedFound && unmanaged == "true" {
		checksum, resetPlan, err = r.newUnmanagedResetPlan(ctx)
	} else {
		checksum, resetPlan, err = r.newResetPlan(ctx, networkNeedsReset)
	}

	if err != nil {
		return fmt.Errorf("getting new reset plan: %w", err)
	}

	patchBase := client.MergeFrom(planSecret.DeepCopy())

	planSecret.Data["applied-checksum"] = []byte("")
	planSecret.Data["failed-checksum"] = []byte("")
	planSecret.Data["plan"] = resetPlan
	planSecret.Annotations = map[string]string{elementalv1.PlanTypeAnnotation: elementalv1.PlanTypeReset}

	if err := r.Patch(ctx, planSecret, patchBase); err != nil {
		return fmt.Errorf("patching plan secret: %w", err)
	}

	// Clear the plan status
	mInventory.Status.Plan = &elementalv1.PlanStatus{
		Checksum: checksum,
		PlanSecretRef: &corev1.ObjectReference{
			Namespace: planSecret.Namespace,
			Name:      planSecret.Name,
		},
	}

	return nil
}

func (r *MachineInventoryReconciler) newResetPlan(ctx context.Context, resetNetwork bool) (string, []byte, error) {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Creating new Reset plan secret")

	// This is the local cloud-config that the elemental-system-agent will run while in recovery mode
	resetCloudConfig := schema.YipConfig{
		Name: "Elemental Reset",
		Stages: map[string][]schema.Stage{
			"network.after": {
				schema.Stage{
					If:   "[ -f /run/cos/recovery_mode ] || [ -f /run/elemental/recovery_mode ]",
					Name: "Runs elemental reset",
					Commands: []string{
						"systemctl start elemental-register-reset",
					},
				},
			},
		},
	}

	resetCloudConfigBytes, err := yaml.Marshal(resetCloudConfig)
	if err != nil {
		return "", nil, fmt.Errorf("marshalling local reset cloud-config to yaml: %w", err)
	}

	resetInstructions := []systemagent.OneTimeInstruction{}

	if resetNetwork {
		resetInstructions = append(resetInstructions, systemagent.OneTimeInstruction{
			CommonInstruction: systemagent.CommonInstruction{
				Name:    "restore first boot config",
				Command: "elemental-register",
				Args:    []string{"--debug", "--reset-network"},
			},
		})
	}

	resetInstructions = append(resetInstructions, systemagent.OneTimeInstruction{
		CommonInstruction: systemagent.CommonInstruction{
			Name:    "configure next boot to recovery mode",
			Command: "grub2-editenv",
			Args: []string{
				"/oem/grubenv",
				"set",
				"next_entry=recovery",
			},
		},
	})
	resetInstructions = append(resetInstructions, systemagent.OneTimeInstruction{
		CommonInstruction: systemagent.CommonInstruction{
			Name:    "schedule reboot",
			Command: "shutdown",
			Args: []string{
				"-r",
				"+1", // Need to have time to confirm plan execution before rebooting
			},
		},
	})

	// This is the remote plan that should trigger the reboot into recovery and reset
	resetPlan := systemagent.Plan{
		Files: []systemagent.File{
			{
				Content:     base64.StdEncoding.EncodeToString(resetCloudConfigBytes),
				Path:        LocalResetPlanPath,
				Permissions: "0600",
			},
		},
		OneTimeInstructions: resetInstructions,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resetPlan); err != nil {
		return "", nil, fmt.Errorf("failed to encode plan: %w", err)
	}

	plan := buf.Bytes()

	checksum := util.PlanChecksum(plan)

	return checksum, plan, nil
}

func (r *MachineInventoryReconciler) newUnmanagedResetPlan(ctx context.Context) (string, []byte, error) {
	logger := ctrl.LoggerFrom(ctx)

	logger.Info("Creating new Unmanaged Reset plan secret")

	// This is the remote plan that should trigger the creation of a node reset marker with current timestamp
	timeStamp := time.Now().Format("2006-01-02 15:04:05")
	resetPlan := systemagent.Plan{
		Files: []systemagent.File{
			{
				Content:     base64.StdEncoding.EncodeToString([]byte(timeStamp)),
				Path:        LocalResetUnmanagedMarker,
				Permissions: "0600",
			},
		},
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resetPlan); err != nil {
		return "", nil, fmt.Errorf("failed to encode plan: %w", err)
	}

	plan := buf.Bytes()

	checksum := util.PlanChecksum(plan)

	return checksum, plan, nil
}

func (r *MachineInventoryReconciler) createPlanSecret(ctx context.Context, mInventory *elementalv1.MachineInventory) error {
	logger := ctrl.LoggerFrom(ctx)

	readyCondition := meta.FindStatusCondition(mInventory.Status.Conditions, elementalv1.ReadyCondition)
	if readyCondition != nil && readyCondition.Reason != elementalv1.PlanCreationFailureReason {
		logger.V(log.DebugDepth).Info("Skipping plan secret creation because ready condition is already set")
		return nil
	}

	planSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{elementalv1.PlanTypeAnnotation: elementalv1.PlanTypeEmpty},
			Namespace:   mInventory.Namespace,
			Name:        mInventory.Name,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: elementalv1.GroupVersion.String(),
					Kind:       "MachineInventory",
					Name:       mInventory.Name,
					UID:        mInventory.UID,
					Controller: ptr.To(true),
				},
			},
			Labels: map[string]string{
				elementalv1.ElementalManagedLabel: "true",
			},
		},
		Type:       elementalv1.PlanSecretType,
		StringData: map[string]string{"plan": "{}"},
	}

	if err := r.Create(ctx, planSecret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create secret: %w", err)
	}

	mInventory.Status.Plan = &elementalv1.PlanStatus{
		PlanSecretRef: &corev1.ObjectReference{
			Namespace: planSecret.Namespace,
			Name:      planSecret.Name,
		},
		State: elementalv1.PlanState(""),
	}

	logger.Info("Plan secret created")
	meta.SetStatusCondition(&mInventory.Status.Conditions, metav1.Condition{
		Type:    elementalv1.ReadyCondition,
		Reason:  elementalv1.WaitingForPlanReason,
		Status:  metav1.ConditionFalse,
		Message: "waiting for plan to be applied",
	})

	return nil
}

func (r *MachineInventoryReconciler) updateInventoryWithPlanStatus(ctx context.Context, mInventory *elementalv1.MachineInventory) error {
	logger := ctrl.LoggerFrom(ctx)

	planSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: mInventory.Namespace,
			Name:      mInventory.Name,
		},
	}

	logger.V(log.DebugDepth).Info("Attempting to set plan status")
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: mInventory.Namespace,
		Name:      mInventory.Name,
	}, planSecret); err != nil {
		return fmt.Errorf("failed to get secret: %w", err)
	}

	appliedChecksum := string(planSecret.Data["applied-checksum"])
	failedChecksum := string(planSecret.Data["failed-checksum"])

	switch {
	case appliedChecksum != "":
		logger.Info("Plan successfully applied")
		mInventory.Status.Plan.State = elementalv1.PlanApplied
		mInventory.Status.Plan.Checksum = appliedChecksum
		meta.SetStatusCondition(&mInventory.Status.Conditions, metav1.Condition{
			Type:    elementalv1.ReadyCondition,
			Reason:  elementalv1.PlanSuccessfullyAppliedReason,
			Status:  metav1.ConditionTrue,
			Message: "plan successfully applied",
		})
		return nil
	case failedChecksum != "":
		logger.V(log.DebugDepth).Info("Plan failed to be applied")
		mInventory.Status.Plan.State = elementalv1.PlanFailed
		mInventory.Status.Plan.Checksum = failedChecksum
		return fmt.Errorf("failed to apply plan")
	default:
		logger.V(log.DebugDepth).Info("Waiting for plan to be applied")
		meta.SetStatusCondition(&mInventory.Status.Conditions, metav1.Condition{
			Type:    elementalv1.ReadyCondition,
			Reason:  elementalv1.WaitingForPlanReason,
			Status:  metav1.ConditionFalse,
			Message: "waiting for plan to be applied",
		})
		return nil
	}
}

func (r *MachineInventoryReconciler) ignoreIncrementalStatusUpdate() predicate.Funcs {
	return predicate.Funcs{
		// Avoid reconciling if the event triggering the reconciliation is related to incremental status updates
		// for MachineInventory resources only
		UpdateFunc: func(e event.UpdateEvent) bool {
			logger := ctrl.LoggerFrom(context.Background())

			if oldMInventory, ok := e.ObjectOld.(*elementalv1.MachineInventory); ok {

				oldMInventory = oldMInventory.DeepCopy()
				newMInventory := e.ObjectNew.(*elementalv1.MachineInventory).DeepCopy()

				// Ignore all fields that might be updated on a status update
				oldMInventory.Status = elementalv1.MachineInventoryStatus{}
				newMInventory.Status = elementalv1.MachineInventoryStatus{}
				oldMInventory.ObjectMeta.ResourceVersion = ""
				newMInventory.ObjectMeta.ResourceVersion = ""
				oldMInventory.ManagedFields = []metav1.ManagedFieldsEntry{}
				newMInventory.ManagedFields = []metav1.ManagedFieldsEntry{}

				update := !cmp.Equal(oldMInventory, newMInventory)
				if !update {
					logger.V(log.DebugDepth).Info("Ignoring status update", "MInventory", oldMInventory.Name)
				}
				return update
			}
			// Return true in case it watches other types
			return true
		},
	}
}
