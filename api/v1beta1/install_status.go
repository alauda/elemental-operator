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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// InstallTargetDeviceAnnotation picks the install disk of a host that
	// waits in the live ISO with install.device-selection: inventory. The
	// value is the canonical ID of one of status.install.candidates. It is
	// one-shot: the registration server removes it when it releases the
	// host, and discards one that was set before the live session started.
	InstallTargetDeviceAnnotation = "baremetal.alauda.io/install-target-device"
	// RegistrationIPLabel carries the source IP the host registered from,
	// refreshed on every registration, so an inventory can be found by the
	// IP shown on the host console. IPv6 addresses are written in full with
	// ':' replaced by '-'.
	RegistrationIPLabel = "elemental.cattle.io/registration-ip"
)

// InstallPhase is status.install.phase.
type InstallPhase string

const (
	// InstallPhaseAwaitingTarget: the host is in the live ISO, has reported
	// its disks and waits for an install target.
	InstallPhaseAwaitingTarget InstallPhase = "AwaitingTarget"
	// InstallPhaseInstalling: the install was released and is running.
	InstallPhaseInstalling InstallPhase = "Installing"
	// InstallPhaseInstalled: the installed system has registered.
	InstallPhaseInstalled InstallPhase = "Installed"
	// InstallPhaseFailed: the last install attempt failed or was interrupted.
	InstallPhaseFailed InstallPhase = "Failed"
)

// InstallerKind is status.install.installer, the backend that installs the OS.
type InstallerKind string

const (
	InstallerToolkit InstallerKind = "toolkit"
	InstallerKubeOS  InstallerKind = "kubeos"
)

// StaleSystemDiskAction says what the install does with a disk that carries
// an old system layout.
type StaleSystemDiskAction string

const (
	// StaleSystemDiskWipe: the disk is wiped before the install.
	StaleSystemDiskWipe StaleSystemDiskAction = "Wipe"
	// StaleSystemDiskBlocked: the disk cannot be wiped safely; the install stops.
	StaleSystemDiskBlocked StaleSystemDiskAction = "Blocked"
	// StaleSystemDiskTarget: the disk is the install target itself.
	StaleSystemDiskTarget StaleSystemDiskAction = "Target"
)

// InstallStatus is the install session of a host booted from the live ISO.
// The baremetal provider mirrors this type; keep the JSON in step.
type InstallStatus struct {
	// +optional
	// +kubebuilder:validation:Enum=AwaitingTarget;Installing;Installed;Failed
	Phase InstallPhase `json:"phase,omitempty"`
	// BootID is the boot_id of the live boot the session belongs to.
	// +optional
	BootID string `json:"bootID,omitempty"`
	// +optional
	Installer InstallerKind `json:"installer,omitempty"`
	// LastSeenAt is refreshed by every registration of the live session.
	// +optional
	LastSeenAt metav1.Time `json:"lastSeenAt,omitempty"`
	// TargetDeviceID is the canonical ID of the disk the install was released
	// to. While an installed host waits in the live ISO again, it is the disk
	// its installed system is on.
	// +optional
	TargetDeviceID string `json:"targetDeviceID,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Candidates are the whole disks the live session reported.
	// +optional
	Candidates []InstallCandidate `json:"candidates,omitempty"`
	// StaleSystemDisks are the disks found carrying an old system layout.
	// +optional
	StaleSystemDisks []StaleSystemDisk `json:"staleSystemDisks,omitempty"`
}

// InstallCandidate is one whole disk a live session reported.
type InstallCandidate struct {
	// ID is the canonical whole-disk ID: wwn:, nvme-eui:, nvme-nguid: or
	// serial:; unidentified: when the disk has none.
	ID string `json:"id"`
	// Path is the kernel device path in the reporting boot only.
	// +optional
	Path string `json:"path,omitempty"`
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	Serial string `json:"serial,omitempty"`
	// +optional
	WWN string `json:"wwn,omitempty"`
	// +optional
	Transport string `json:"transport,omitempty"`
	// +optional
	Rotational bool `json:"rotational,omitempty"`
	// StaleSystem marks a disk that carries an old system layout.
	// +optional
	StaleSystem bool `json:"staleSystem,omitempty"`
	// Eligible says whether the disk may be chosen as the install target;
	// Reasons explains why not.
	Eligible bool `json:"eligible"`
	// +optional
	Reasons []string `json:"reasons,omitempty"`
	// HasData lists signatures of data found on the disk, e.g. LVM2_member.
	// Automatic selection never picks such a disk.
	// +optional
	HasData []string `json:"hasData,omitempty"`
}

// StaleSystemDisk is a disk, other than the install target, that carries an
// old system layout.
type StaleSystemDisk struct {
	ID string `json:"id"`
	// +optional
	Evidence []string `json:"evidence,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Wipe;Blocked;Target
	Action StaleSystemDiskAction `json:"action,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
}

// MachineInventory conditions of the live install session.
const (
	// InstallTargetReadyCondition is True once the install was released to
	// a disk.
	InstallTargetReadyCondition = "InstallTargetReady"
	// AwaitingSelectionReason: no install target was chosen yet.
	AwaitingSelectionReason = "AwaitingSelection"
	// SelectionDiscardedReason: an install-target annotation was set before
	// the live session started, or did not survive validation, and was
	// removed.
	SelectionDiscardedReason = "SelectionDiscarded"
	// TargetNotObservedReason: the chosen disk is not among the disks the
	// live session reported, or the host could not find it.
	TargetNotObservedReason = "TargetNotObserved"
	// TargetIneligibleReason: the chosen disk fails a safety gate.
	TargetIneligibleReason = "TargetIneligible"
	// TargetMismatchReason: the disk the host found under the chosen ID no
	// longer looks as reported.
	TargetMismatchReason = "TargetMismatch"
	// ReleasedReason: the install was released to the chosen disk.
	ReleasedReason = "Released"

	// StaleSystemDisksCondition is True when no stale system disk is left.
	StaleSystemDisksCondition = "StaleSystemDisks"
	// NoneDetectedReason: no disk carries an old system layout.
	NoneDetectedReason = "NoneDetected"
	// WipedReason: the stale system disks were wiped by the install.
	WipedReason = "Wiped"
	// DetectedReason: stale system disks were found; they are wiped when the
	// install runs.
	DetectedReason = "Detected"
	// BlockedReason: a stale system disk cannot be wiped safely; the install
	// stops until a person deals with it.
	BlockedReason = "Blocked"
	// WipeFailedReason: wiping a stale system disk failed or left a layout
	// behind.
	WipeFailedReason = "WipeFailed"
)
