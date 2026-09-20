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
	// Storage is the explicit desired state for Inventory-owned data volumes.
	// Devices omitted from this list remain unmanaged.
	// +optional
	Storage *MachineInventoryStorageSpec `json:"storage,omitempty"`
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
	// Connections holds the host's persisted NetworkManager keyfiles verbatim,
	// keyed by the connection file's base name (without the .nmconnection
	// suffix). Anything present here was created by an operator: NetworkManager
	// keeps its own auto-default connections in memory, with no file on disk.
	//
	// The install and reset paths feed these back to the host unchanged, which
	// is how a bond, VLAN or bridge configured from the live ISO survives into
	// the installed system. Every other registration reports them and discards
	// them, so editing this field has no effect on the host.
	// +optional
	Connections map[string]string `json:"connections,omitempty"`
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
	// Kind is the link type read from /sys/class/net/<name>/uevent DEVTYPE.
	// It is empty for a plain physical ethernet device, and otherwise carries
	// the kernel's own name for the link type: bond, vlan, bridge, ...
	// +optional
	Kind string `json:"kind,omitempty"`
	// Master is the name of the aggregating link this interface is enslaved to,
	// resolved from the /sys/class/net/<name>/master symlink. It is empty for
	// an interface that stands on its own.
	// +optional
	Master string `json:"master,omitempty"`
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

type MachineInventoryStatus struct {
	// Conditions describe the state of the machine inventory object.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// PlanStatus reflect the status of the plan owned by the machine inventory object.
	// +optional
	Plan *PlanStatus `json:"plan,omitempty"`
	// ObservedStorage is objective host state written only by the authenticated
	// periodic observer channel.
	// +optional
	ObservedStorage *ObservedStorage `json:"observedStorage,omitempty"`
	// Storage is desired-state convergence written only by the baremetal
	// provider Inventory Storage Reconciler.
	// +optional
	Storage *MachineInventoryStorageStatus `json:"storage,omitempty"`
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
