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

package register

import (
	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

// DefaultInstallHoldSeconds is how long a held host waits before it
// registers again when the operator did not say.
const DefaultInstallHoldSeconds = 15

// LiveInstallSession is the MsgLiveInstallSession payload. A host booted from
// the live ISO with --install sends it before MsgGet, so the operator can
// show its disks and hold it until a person picks the install target.
type LiveInstallSession struct {
	// BootID is /proc/sys/kernel/random/boot_id of the live boot.
	BootID string `json:"bootID"`
	// Installer is the install backend: toolkit or kubeos.
	Installer elementalv1.InstallerKind `json:"installer"`
	// RegisterVersion is the elemental-register version.
	RegisterVersion string `json:"registerVersion,omitempty"`
	// Candidates are the whole disks of the host.
	Candidates []elementalv1.InstallCandidate `json:"candidates,omitempty"`
	// StaleSystemDisks are the disks carrying an old system layout, as the
	// host would treat them with no target chosen yet.
	StaleSystemDisks []elementalv1.StaleSystemDisk `json:"staleSystemDisks,omitempty"`
	// SurveyError is set when the disks could not be listed. The operator
	// shows it and does not release the host.
	SurveyError string `json:"surveyError,omitempty"`
}

// InstallHold is the MsgInstallHold payload: the operator answers MsgGet with
// it instead of MsgConfig while the host has to wait for an install target.
// The host must not touch any disk and registers again after
// RetryAfterSeconds.
type InstallHold struct {
	// Reason is the InstallTargetReady condition reason, e.g.
	// AwaitingSelection.
	Reason string `json:"reason"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	RetryAfterSeconds int `json:"retryAfterSeconds,omitempty"`
}

// InstallReport is the MsgInstallReport payload a released host sends on a
// short session of its own (authenticate, negotiate, report) when its
// install fails.
type InstallReport struct {
	BootID string `json:"bootID"`
	// Phase is Installing or Failed.
	Phase elementalv1.InstallPhase `json:"phase"`
	// +optional
	TargetDeviceID string `json:"targetDeviceID,omitempty"`
	// Reason is the install-disk error reason when there is one:
	// TargetNotObserved, TargetIneligible, TargetMismatch, TargetAmbiguous,
	// Blocked, WipeFailed, ...
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}
