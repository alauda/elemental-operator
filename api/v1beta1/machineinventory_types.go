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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	MachineInventoryFinalizer                               = "machineinventory.elemental.cattle.io"
	PlanSecretType                        corev1.SecretType = "elemental.cattle.io/plan"
	PlanTypeAnnotation                                      = "elemental.cattle.io/plan.type"
	PlanTypeEmpty                                           = "empty"
	PlanTypeBootstrap                                       = "bootstrap"
	PlanTypeReset                                           = "reset"
	MachineInventoryResettableAnnotation                    = "elemental.cattle.io/resettable"
	MachineInventoryOSUnmanagedAnnotation                   = "elemental.cattle.io/os.unmanaged"
)

type MachineInventorySpec struct {
	// TPMHash the hash of the TPM EK public key. This is used if you are
	// using TPM2 to identifiy nodes.  You can obtain the TPM by
	// running `rancherd get-tpm-hash` on the node. Or nodes can
	// report their TPM hash by using the MachineRegister.
	// +optional
	TPMHash string `json:"tpmHash,omitempty"`
	// MachineHash the hash of the identifier used by the host to identify
	// to the operator. This is used when the host authenticates without TPM.
	// Both the authentication method and the identifier used to derive the hash
	// depend upon the MachineRegistration spec.config.elemental.registration.auth value.
	// +optional
	MachineHash string `json:"machineHash,omitempty"`
	// IPAddressClaims is a map of IPAddressClaim associated to this machine.
	// The map key is the ipAddressPool.name.
	IPAddressClaims map[string]*corev1.ObjectReference `json:"ipAddressClaims,omitempty"`
	// IPAddressPools is a list of IPAddressPool associated to this machine.
	IPAddressPools map[string]*corev1.TypedLocalObjectReference `json:"ipAddressPools,omitempty"`
	// NetworkConfig is the final NetworkConfig.
	// +optional
	Network NetworkConfig `json:"network,omitempty"`
	// ObservedNetwork is a live-ISO-side snapshot of the host's actual network
	// configuration uploaded by elemental-register during initial registration
	// (Alauda fork). It is informational only and is intentionally kept
	// separate from Network (the target/template network). Consumers such as
	// baremetal providers can read this to know what the host looked like
	// before install-time network reconfiguration.
	// +optional
	ObservedNetwork *ObservedNetwork `json:"observedNetwork,omitempty"`
	// ObservedStorage is a registration-time snapshot of the host's physical
	// block devices. It is informational and intended for inventory UI and
	// allocation preflight; provisioning code must inspect the live device again
	// before making any change.
	// +optional
	ObservedStorage *ObservedStorage `json:"observedStorage,omitempty"`
}

// ObservedNetwork captures a snapshot of the host's actual network state as
// observed from the live ISO at registration time. Unlike NetworkConfig, this
// structure carries no template/configurator semantics — it is purely
// descriptive data.
type ObservedNetwork struct {
	// Interfaces observed on the host.
	// +optional
	Interfaces []ObservedInterface `json:"interfaces,omitempty"`
	// Routes observed in the host's routing table.
	// +optional
	Routes []ObservedRoute `json:"routes,omitempty"`
	// DNSServers observed (typically from /etc/resolv.conf).
	// +optional
	DNSServers []string `json:"dnsServers,omitempty"`
	// SearchDomains observed.
	// +optional
	SearchDomains []string `json:"searchDomains,omitempty"`
}

// ObservedInterface describes a single network interface on the host.
type ObservedInterface struct {
	// Name is the OS-level interface name (e.g. eth0, ens3).
	Name string `json:"name"`
	// MAC is the hardware address.
	// +optional
	MAC string `json:"mac,omitempty"`
	// MTU in bytes.
	// +optional
	MTU int `json:"mtu,omitempty"`
	// Addresses assigned to the interface, in CIDR form (e.g. 10.0.0.5/24,
	// fe80::1/64). Both IPv4 and IPv6 may appear.
	// +optional
	Addresses []string `json:"addresses,omitempty"`
}

// ObservedRoute describes a single routing table entry.
type ObservedRoute struct {
	// Destination CIDR, or the literal string "default" for the default
	// route.
	Destination string `json:"destination"`
	// Gateway is the next-hop IP, if any.
	// +optional
	Gateway string `json:"gateway,omitempty"`
	// Interface is the outgoing device name, if any.
	// +optional
	Interface string `json:"interface,omitempty"`
	// Metric of the route.
	// +optional
	Metric int `json:"metric,omitempty"`
}

// ObservedStorage captures stable identities and the non-destructive state of
// physical block devices seen by elemental-register.
type ObservedStorage struct {
	// Devices observed on the host.
	// +optional
	Devices []ObservedStorageDevice `json:"devices,omitempty"`
}

// ObservedStorageDevice describes one physical disk. ByID is empty only when
// no stable /dev/disk/by-id path exists; such a device is always ineligible.
type ObservedStorageDevice struct {
	// ByID is the canonical stable /dev/disk/by-id path for this disk.
	ByID string `json:"byID"`
	// WWN reported by udev or lsblk.
	// +optional
	WWN string `json:"wwn,omitempty"`
	// Serial reported by udev or lsblk.
	// +optional
	Serial string `json:"serial,omitempty"`
	// Model reported by udev or lsblk.
	// +optional
	Model string `json:"model,omitempty"`
	// SizeBytes is the disk capacity in bytes.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Rotational is true for rotational media.
	// +optional
	Rotational bool `json:"rotational,omitempty"`
	// SystemDisk marks a disk that backs the installed OS or carries an
	// Elemental/EFI system label.
	// +optional
	SystemDisk bool `json:"systemDisk,omitempty"`
	// Partitions describes filesystems and mounts below the disk. A filesystem
	// directly on the whole disk is represented with Number 0.
	// +optional
	Partitions []ObservedStoragePartition `json:"partitions,omitempty"`
	// Eligible indicates that the registration-time snapshot matches the safe
	// v1 data-disk layouts. Host-side plan checks remain authoritative.
	// +optional
	Eligible bool `json:"eligible,omitempty"`
	// IneligibleReasons explains why the disk cannot be selected.
	// +optional
	IneligibleReasons []string `json:"ineligibleReasons,omitempty"`
}

// ObservedStoragePartition describes one partition or a whole-disk filesystem.
type ObservedStoragePartition struct {
	// Number is the partition number. Zero denotes a whole-disk filesystem.
	// +optional
	Number int `json:"number,omitempty"`
	// FilesystemType is the detected filesystem or signature type.
	// +optional
	FilesystemType string `json:"filesystemType,omitempty"`
	// FilesystemUUID is the detected filesystem UUID.
	// +optional
	FilesystemUUID string `json:"filesystemUUID,omitempty"`
	// MountPoint is one observed mount target, if any.
	// +optional
	MountPoint string `json:"mountPoint,omitempty"`
}

type MachineInventoryStatus struct {
	// Conditions describe the state of the machine inventory object.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// PlanStatus reflect the status of the plan owned by the machine inventory object.
	// +optional
	Plan *PlanStatus `json:"plan,omitempty"`
}

type PlanState string

const (
	PlanApplied PlanState = "Applied"
	PlanFailed  PlanState = "Failed"
)

type PlanStatus struct {
	// PlanSecretRef a reference to the created plan secret.
	// +optional
	PlanSecretRef *corev1.ObjectReference `json:"secretRef,omitempty"`
	// Checksum checksum of the created plan.
	// +optional
	Checksum string `json:"checksum,omitempty"`
	// State reflect state of the plan that belongs to the machine inventory.
	// +kubebuilder:validation:Enum=Applied;Failed
	// +optional
	State PlanState `json:"state,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type MachineInventory struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineInventorySpec   `json:"spec,omitempty"`
	Status MachineInventoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineInventoryList contains a list of MachineInventories.
type MachineInventoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineInventory `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MachineInventory{}, &MachineInventoryList{})
}
