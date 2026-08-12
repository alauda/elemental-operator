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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// MachineInventoryStorageFinalizer keeps an Inventory alive until all
	// provider-managed filesystems have been released non-destructively.
	MachineInventoryStorageFinalizer = "storage.baremetal.alauda.io/finalizer"
	// MaxMachineInventoryStorageVolumes bounds admission, status and plan size.
	MaxMachineInventoryStorageVolumes = 32
	// MaxObservedStorageDevices bounds one observer report.
	MaxObservedStorageDevices = 1024
)

// StorageFilesystemPolicy declares whether an existing filesystem is adopted
// or an entirely blank logical device may be initialized.
// +kubebuilder:validation:Enum=Adopt;InitializeIfBlank
type StorageFilesystemPolicy string

const (
	StorageFilesystemPolicyAdopt             StorageFilesystemPolicy = "Adopt"
	StorageFilesystemPolicyInitializeIfBlank StorageFilesystemPolicy = "InitializeIfBlank"
)

// StorageFilesystemType is a supported ordinary single-host filesystem.
// +kubebuilder:validation:Enum=xfs;ext4
type StorageFilesystemType string

const (
	StorageFilesystemTypeXFS  StorageFilesystemType = "xfs"
	StorageFilesystemTypeExt4 StorageFilesystemType = "ext4"
)

// MachineInventoryStorageSpec is the complete desired storage state of one
// physical Inventory. Devices omitted from Volumes remain unmanaged.
type MachineInventoryStorageSpec struct {
	// RetryNonce is a monotonic operator-controlled retry counter. It is not
	// part of the normalized storage content hash.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	RetryNonce int64 `json:"retryNonce,omitempty"`

	// Volumes are the only block devices the platform may manage.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Volumes []MachineInventoryStorageVolume `json:"volumes,omitempty"`
}

// MachineInventoryStorageVolume declares one filesystem and its business
// mount point on this Inventory.
type MachineInventoryStorageVolume struct {
	// Name is the stable logical identity inside this Inventory.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	Source StorageVolumeSource `json:"source"`

	Filesystem StorageVolumeFilesystem `json:"filesystem"`

	Mount StorageVolumeMount `json:"mount"`
}

// StorageVolumeSource selects one observer-reported logical block device.
type StorageVolumeSource struct {
	// DeviceID must exactly reference status.observedStorage.devices[].id.
	// Dynamic kernel paths such as /dev/sdb are not accepted.
	// +kubebuilder:validation:MinLength=5
	// +kubebuilder:validation:MaxLength=512
	DeviceID string `json:"deviceID"`

	// MinimumSize is a non-destructive capacity assertion.
	// +optional
	MinimumSize *resource.Quantity `json:"minimumSize,omitempty"`

	// Multipath is valid only for a wwid: aggregate map.
	// +optional
	Multipath *StorageMultipathPolicy `json:"multipath,omitempty"`
}

// StorageMultipathPolicy declares the path-health gate for a Multipath map.
type StorageMultipathPolicy struct {
	// MinimumActivePaths defaults to one.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	MinimumActivePaths int32 `json:"minimumActivePaths,omitempty"`
}

// StorageVolumeFilesystem declares filesystem adoption/initialization intent.
type StorageVolumeFilesystem struct {
	Policy StorageFilesystemPolicy `json:"policy"`

	Type StorageFilesystemType `json:"type"`

	// ExpectedUUID is mandatory for Adopt and forbidden for a new blank
	// initialization.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	ExpectedUUID string `json:"expectedUUID,omitempty"`
}

// StorageVolumeMount declares the business mount path and readiness policy.
type StorageVolumeMount struct {
	// Path is a normalized absolute path accepted by provider admission.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=255
	Path string `json:"path"`

	// Required defaults to true. Optional volumes are still prepared and
	// activated; only Machine readiness is relaxed.
	// +optional
	// +kubebuilder:default=true
	Required *bool `json:"required,omitempty"`

	// Options contains individual, admission-whitelisted mount options.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Options []string `json:"options,omitempty"`
}

// ObservedStorage is an objective, periodically refreshed host report. It is
// written only through the authenticated Elemental observer channel.
type ObservedStorage struct {
	// ObservedAt is set by the operator when it accepts the report.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// BootID is the host's current Linux boot ID.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	BootID string `json:"bootID,omitempty"`

	// ReportEpoch is allocated by the operator for an observer session.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReportEpoch int64 `json:"reportEpoch,omitempty"`

	// Sequence strictly increases within ReportEpoch.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Sequence int64 `json:"sequence,omitempty"`

	// Devices contains facts for all discovered logical block devices.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +listType=map
	// +listMapKey=id
	Devices []ObservedStorageDevice `json:"devices,omitempty"`
}

// ObservedStorageDeviceKind identifies the logical object represented by a
// report record.
// +kubebuilder:validation:Enum=DirectDisk;Partition;Multipath
type ObservedStorageDeviceKind string

const (
	ObservedStorageDeviceDirectDisk ObservedStorageDeviceKind = "DirectDisk"
	ObservedStorageDevicePartition  ObservedStorageDeviceKind = "Partition"
	ObservedStorageDeviceMultipath  ObservedStorageDeviceKind = "Multipath"
)

// ObservedStorageSystemRole is an objective system-disk classification.
// Unknown is intentionally fail-closed in provider policy.
// +kubebuilder:validation:Enum=System;Data;Unknown
type ObservedStorageSystemRole string

const (
	ObservedStorageSystemRoleSystem  ObservedStorageSystemRole = "System"
	ObservedStorageSystemRoleData    ObservedStorageSystemRole = "Data"
	ObservedStorageSystemRoleUnknown ObservedStorageSystemRole = "Unknown"
)

// ObservedStorageDevice describes one logical block device and its topology.
type ObservedStorageDevice struct {
	// ID is the canonical management identity (wwn:, nvme-eui:,
	// nvme-nguid:, serial:, partuuid:, or wwid:).
	// +kubebuilder:validation:MinLength=5
	// +kubebuilder:validation:MaxLength=512
	ID string `json:"id"`

	Kind ObservedStorageDeviceKind `json:"kind"`

	// Path is diagnostic only and must never be persisted as desired identity.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Path string `json:"path,omitempty"`

	// StablePaths are current udev aliases for diagnostics/resolution.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	StablePaths []string `json:"stablePaths,omitempty"`

	// ParentID links a partition to its direct parent disk.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	ParentID string `json:"parentID,omitempty"`

	// MemberIDs lists Multipath member-path diagnostic identities. Members are
	// never valid desired device IDs.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	MemberIDs []string `json:"memberIDs,omitempty"`

	// Consumers contains descendant block devices layered on this logical
	// device (for example partitions, dm-crypt, LVM or RAID). These are
	// objective topology facts; provider policy decides whether they make the
	// device ineligible for management.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Consumers []ObservedStorageConsumer `json:"consumers,omitempty"`

	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// StartBytes is populated for partitions.
	// +optional
	StartBytes int64 `json:"startBytes,omitempty"`

	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
	// +optional
	Rotational bool `json:"rotational,omitempty"`
	// +optional
	Removable bool `json:"removable,omitempty"`

	SystemRole ObservedStorageSystemRole `json:"systemRole"`

	// +optional
	// +kubebuilder:validation:MaxItems=32
	SystemEvidence []string `json:"systemEvidence,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=32
	PartitionTableType string `json:"partitionTableType,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxItems=64
	Signatures []ObservedStorageSignature `json:"signatures,omitempty"`

	// +optional
	Filesystem *ObservedStorageFilesystem `json:"filesystem,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxItems=64
	Mounts []ObservedStorageMount `json:"mounts,omitempty"`

	// +optional
	Health *ObservedStorageMultipathHealth `json:"health,omitempty"`

	// Transport and identity evidence are informational and support safe
	// serial fallback classification.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Transport string `json:"transport,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Model string `json:"model,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Serial string `json:"serial,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=256
	WWN string `json:"wwn,omitempty"`
}

// ObservedStorageConsumer describes one descendant block consumer. Path is
// diagnostic only; Type and FilesystemType are the kernel/util-linux facts.
type ObservedStorageConsumer struct {
	// +kubebuilder:validation:MaxLength=512
	Path string `json:"path"`
	// +kubebuilder:validation:MaxLength=64
	Type string `json:"type"`
	// +optional
	// +kubebuilder:validation:MaxLength=64
	FilesystemType string `json:"filesystemType,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Mounts []string `json:"mounts,omitempty"`
}

// ObservedStorageSignature is one wipefs/lsblk signature fact.
type ObservedStorageSignature struct {
	// +kubebuilder:validation:MaxLength=64
	Type string `json:"type"`
	// +kubebuilder:validation:MaxLength=256
	Value string `json:"value"`
}

// ObservedStorageFilesystem describes an existing filesystem.
type ObservedStorageFilesystem struct {
	// +kubebuilder:validation:MaxLength=64
	Type string `json:"type"`
	// +optional
	// +kubebuilder:validation:MaxLength=128
	UUID string `json:"uuid,omitempty"`
}

// ObservedStorageMount describes a current mount of this exact logical device.
type ObservedStorageMount struct {
	// +kubebuilder:validation:MaxLength=512
	Path string `json:"path"`
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Options []string `json:"options,omitempty"`
}

// ObservedStorageMultipathHealth reports aggregate path health.
type ObservedStorageMultipathHealth struct {
	// +optional
	// +kubebuilder:validation:Minimum=0
	ActivePaths int32 `json:"activePaths,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	TotalPaths int32 `json:"totalPaths,omitempty"`
}

// StorageLifecyclePhase is the Inventory-wide managed-storage phase.
// +kubebuilder:validation:Enum=Unmanaged;Observed;Validating;Preparing;Prepared;Activating;Active;Deactivating;Releasing;Degraded;Failed
type StorageLifecyclePhase string

const (
	StorageLifecycleUnmanaged    StorageLifecyclePhase = "Unmanaged"
	StorageLifecycleObserved     StorageLifecyclePhase = "Observed"
	StorageLifecycleValidating   StorageLifecyclePhase = "Validating"
	StorageLifecyclePreparing    StorageLifecyclePhase = "Preparing"
	StorageLifecyclePrepared     StorageLifecyclePhase = "Prepared"
	StorageLifecycleActivating   StorageLifecyclePhase = "Activating"
	StorageLifecycleActive       StorageLifecyclePhase = "Active"
	StorageLifecycleDeactivating StorageLifecyclePhase = "Deactivating"
	StorageLifecycleReleasing    StorageLifecyclePhase = "Releasing"
	StorageLifecycleDegraded     StorageLifecyclePhase = "Degraded"
	StorageLifecycleFailed       StorageLifecyclePhase = "Failed"
)

// StorageMutationState records whether durable host state may have changed.
// +kubebuilder:validation:Enum=None;DurableStateChanged;Unknown
type StorageMutationState string

const (
	StorageMutationNone                StorageMutationState = "None"
	StorageMutationDurableStateChanged StorageMutationState = "DurableStateChanged"
	StorageMutationUnknown             StorageMutationState = "Unknown"
)

// StorageOperationType identifies one logical convergence operation.
// +kubebuilder:validation:Enum=Prepare;Release;Activate;Deactivate
type StorageOperationType string

const (
	StorageOperationPrepare    StorageOperationType = "Prepare"
	StorageOperationRelease    StorageOperationType = "Release"
	StorageOperationActivate   StorageOperationType = "Activate"
	StorageOperationDeactivate StorageOperationType = "Deactivate"
)

// StorageOperationPhase is the controller-side state of an operation.
// +kubebuilder:validation:Enum=Pending;Running;Applied;Failed
type StorageOperationPhase string

const (
	StorageOperationPending StorageOperationPhase = "Pending"
	StorageOperationRunning StorageOperationPhase = "Running"
	StorageOperationApplied StorageOperationPhase = "Applied"
	StorageOperationFailed  StorageOperationPhase = "Failed"
)

// MachineInventoryStorageStatus is written only by the provider Inventory
// Storage Reconciler. appliedVolumes remains authoritative while an operation
// is pending or failed.
type MachineInventoryStorageStatus struct {
	// +optional
	HashVersion string `json:"hashVersion,omitempty"`
	// +optional
	AppliedSpecHash string `json:"appliedSpecHash,omitempty"`
	// +optional
	PendingSpecHash string `json:"pendingSpecHash,omitempty"`
	// +optional
	LastConsumedRetryNonce int64 `json:"lastConsumedRetryNonce,omitempty"`
	// +optional
	LastConsumedInitializationApprovalCounter int64 `json:"lastConsumedInitializationApprovalCounter,omitempty"`
	// +optional
	LastConsumedInitializationApprovalHash string `json:"lastConsumedInitializationApprovalHash,omitempty"`

	// +optional
	Phase StorageLifecyclePhase `json:"phase,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	AppliedVolumes []ManagedStorageVolumeStatus `json:"appliedVolumes,omitempty"`

	// +optional
	Operation *StorageOperationStatus `json:"operation,omitempty"`
}

// ManagedStorageVolumeStatus is a complete cleanup-capable snapshot of one
// managed volume, not merely a diagnostic summary.
type ManagedStorageVolumeStatus struct {
	Name     string `json:"name"`
	DeviceID string `json:"deviceID"`
	// +optional
	MinimumSizeBytes int64 `json:"minimumSizeBytes,omitempty"`
	// +optional
	MinimumActivePaths int32                   `json:"minimumActivePaths,omitempty"`
	FilesystemPolicy   StorageFilesystemPolicy `json:"filesystemPolicy"`
	FilesystemType     StorageFilesystemType   `json:"filesystemType"`
	// +optional
	ExpectedFilesystemUUID string `json:"expectedFilesystemUUID,omitempty"`
	// +optional
	FilesystemUUID string `json:"filesystemUUID,omitempty"`
	MountPath      string `json:"mountPath"`
	// +optional
	MountOptions      []string `json:"mountOptions,omitempty"`
	Required          bool     `json:"required"`
	EffectiveRequired bool     `json:"effectiveRequired"`
	// +optional
	ResolvedPath string `json:"resolvedPath,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Active bool `json:"active,omitempty"`
	// +optional
	MutationState StorageMutationState `json:"mutationState,omitempty"`
	// +optional
	LastOperationID string `json:"lastOperationID,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// StorageOperationStatus is the durable controller journal projection for a
// logical operation and its current execution attempt.
type StorageOperationStatus struct {
	Type           StorageOperationType  `json:"type"`
	Phase          StorageOperationPhase `json:"phase"`
	OperationID    string                `json:"operationID"`
	AttemptID      string                `json:"attemptID"`
	TargetSpecHash string                `json:"targetSpecHash"`
	RetryNonce     int64                 `json:"retryNonce"`
	// ObservationReportEpoch and ObservationSequence capture the host report
	// seen when the operation was claimed. The controller waits for a strictly
	// newer accepted report before writing a host plan.
	// +optional
	ObservationReportEpoch int64 `json:"observationReportEpoch,omitempty"`
	// +optional
	ObservationSequence int64 `json:"observationSequence,omitempty"`
	// ObservationBootID is the host boot ID captured before a Machine-owned
	// lifecycle operation. Activate uses it to prove that reprovision crossed a
	// boot before accepting a later mounted observation. It is a fact, not an
	// ordering key.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	ObservationBootID string `json:"observationBootID,omitempty"`
	// +optional
	InitializationApprovalCounter int64 `json:"initializationApprovalCounter,omitempty"`
	// +optional
	InitializationApprovalHash string `json:"initializationApprovalHash,omitempty"`
	PlanOwnerKind              string `json:"planOwnerKind"`
	PlanOwnerUID               string `json:"planOwnerUID"`
	// +optional
	PlanChecksum  string               `json:"planChecksum,omitempty"`
	MutationState StorageMutationState `json:"mutationState"`
	StartedAt     metav1.Time          `json:"startedAt"`
	UpdatedAt     metav1.Time          `json:"updatedAt"`
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Volumes []ManagedStorageVolumeStatus `json:"volumes,omitempty"`
}

const (
	StorageObservedCondition = "StorageObserved"
	StoragePreparedCondition = "StoragePrepared"
	StorageActiveCondition   = "StorageActive"
	StorageDegradedCondition = "StorageDegraded"
)

// Keep corev1 referenced here because the generated OpenAPI code in older
// controller-gen versions otherwise prunes imports shared with the parent API.
var _ = corev1.ConditionTrue
