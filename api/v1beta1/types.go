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
	runtime "k8s.io/apimachinery/pkg/runtime"
)

const TPMRandomSeedValue = -1

// DeviceSelectionInventory is the Install.DeviceSelection value that has a
// person pick the install disk on the MachineInventory.
const DeviceSelectionInventory = "inventory"

type Install struct {
	// +optional
	Firmware string `json:"firmware,omitempty" yaml:"firmware,omitempty"`
	// +optional
	Device string `json:"device,omitempty" yaml:"device,omitempty"`
	// +optional
	DeviceSelector DeviceSelector `json:"device-selector,omitempty" yaml:"device-selector,omitempty"`
	// DeviceSelection chooses how the install disk is picked. Empty keeps
	// device and device-selector. "inventory" makes a host booted from the
	// live ISO report its disks on its MachineInventory (status.install) and
	// wait until a person picks one with the
	// baremetal.alauda.io/install-target-device annotation; device and
	// device-selector are not used then.
	// +optional
	// +kubebuilder:validation:Enum="";inventory
	DeviceSelection string `json:"device-selection,omitempty" yaml:"device-selection,omitempty"`
	// +optional
	NoFormat bool `json:"no-format,omitempty" yaml:"no-format,omitempty"`
	// +optional
	ConfigURLs []string `json:"config-urls,omitempty" yaml:"config-urls,omitempty"`
	// +optional
	ISO string `json:"iso,omitempty" yaml:"iso,omitempty"`
	// +optional
	SystemURI string `json:"system-uri,omitempty" yaml:"system-uri,omitempty"`
	// +optional
	Debug bool `json:"debug,omitempty" yaml:"debug,omitempty"`
	// +optional
	TTY string `json:"tty,omitempty" yaml:"tty,omitempty"`
	// +optional
	PowerOff bool `json:"poweroff,omitempty" yaml:"poweroff,omitempty"`
	// +optional
	Reboot bool `json:"reboot,omitempty" yaml:"reboot,omitempty"`
	// +optional
	EjectCD bool `json:"eject-cd,omitempty" yaml:"eject-cd,omitempty"`
	// +optional
	DisableBootEntry bool `json:"disable-boot-entry,omitempty" yaml:"disable-boot-entry,omitempty"`
	// +optional
	ConfigDir string `json:"config-dir,omitempty" yaml:"config-dir,omitempty"`
	// +optional
	// +kubebuilder:default:={"type": "loopdevice", "maxSnaps": 2}
	Snapshotter SnapshotterConfig `json:"snapshotter,omitempty" yaml:"snapshotter,omitempty"`
}

type SnapshotterConfig struct {
	// Type sets the snapshotter type for a new installation, available options are 'loopdevice' and 'btrfs'
	// +optional
	// +kubebuilder:default:=loopdevice
	Type string `json:"type,omitempty" yaml:"type,omitempty"`
	// MaxSnaps sets the maximum amount of snapshots to keep
	// +optional
	// +kubebuilder:default:=2
	// +kubebuilder:validation:Minimum:=2
	MaxSnaps int `json:"maxSnaps,omitempty" yaml:"maxSnaps,omitempty"`
}

type Reset struct {
	// +optional
	Enabled bool `json:"enabled" yaml:"enabled" mapstructure:"enabled"`
	// +optional
	// +kubebuilder:default:=true
	ResetPersistent bool `json:"reset-persistent,omitempty" yaml:"reset-persistent,omitempty" mapstructure:"reset-persistent"`
	// +optional
	// +kubebuilder:default:=true
	ResetOEM bool `json:"reset-oem,omitempty" yaml:"reset-oem,omitempty" mapstructure:"reset-oem"`
	// +optional
	ConfigURLs []string `json:"config-urls,omitempty" yaml:"config-urls,omitempty" mapstructure:"config-urls"`
	// +optional
	SystemURI string `json:"system-uri,omitempty" yaml:"system-uri,omitempty" mapstructure:"system-uri"`
	// +optional
	Debug bool `json:"debug,omitempty" yaml:"debug,omitempty" mapstructure:"debug"`
	// +optional
	PowerOff bool `json:"poweroff,omitempty" yaml:"poweroff,omitempty" mapstructure:"poweroff"`
	// +optional
	// +kubebuilder:default:=true
	Reboot bool `json:"reboot,omitempty" yaml:"reboot,omitempty" mapstructure:"reboot"`
	// +optional
	DisableBootEntry bool `json:"disable-boot-entry,omitempty" yaml:"disable-boot-entry,omitempty"`
}

// Registration is the registration section of a MachineRegistration. A
// tpm-mode that contradicts emulate-tpm is rejected at admission; the
// MachineRegistration controller reports InvalidTPMConfig as well for
// objects that bypassed it.
// +kubebuilder:validation:XValidation:rule="!(has(self.tpm__dash__mode) && self.tpm__dash__mode in ['hardware', 'auto'] && has(self.emulate__dash__tpm) && self.emulate__dash__tpm && (!has(self.auth) || self.auth == 'tpm'))",message="tpm-mode hardware or auto cannot be combined with emulate-tpm: true"
type Registration struct {
	// +optional
	URL string `json:"url,omitempty" yaml:"url,omitempty" mapstructure:"url"`
	// +optional
	CACert string `json:"ca-cert,omitempty" yaml:"ca-cert,omitempty" mapstructure:"ca-cert"`
	// +optional
	EmulateTPM bool `json:"emulate-tpm,omitempty" yaml:"emulate-tpm,omitempty" mapstructure:"emulate-tpm"`
	// +optional
	EmulatedTPMSeed int64 `json:"emulated-tpm-seed,omitempty" yaml:"emulated-tpm-seed,omitempty" mapstructure:"emulated-tpm-seed"`
	// TPMMode selects the TPM behind auth "tpm". "hardware" requires a usable
	// TPM 2.0 device, "emulated" uses the software TPM seeded by
	// emulated-tpm-seed, and "auto" probes the host once at its first
	// registration and keeps the result. The resolved mode is recorded in the
	// registration state and never changes afterwards. Empty keeps the
	// emulate-tpm behaviour. "hardware" and "auto" cannot be combined with
	// emulate-tpm: true. Ignored for other auth methods.
	// +optional
	// +kubebuilder:validation:Enum=hardware;emulated;auto
	TPMMode string `json:"tpm-mode,omitempty" yaml:"tpm-mode,omitempty" mapstructure:"tpm-mode"`
	// +optional
	NoSMBIOS bool `json:"no-smbios,omitempty" yaml:"no-smbios,omitempty" mapstructure:"no-smbios"`
	// +optional
	// +kubebuilder:default:=tpm
	Auth string `json:"auth,omitempty" yaml:"auth,omitempty" mapstructure:"auth"`
	// +optional
	NoToolkit bool `json:"no-toolkit,omitempty" yaml:"no-toolkit,omitempty" mapstructure:"no-toolkit"`
}

type SystemAgent struct {
	// +optional
	StrictTLSMode bool `json:"strictTLSMode,omitempty" yaml:"strictTLSMode,omitempty"`
	// +optional
	URL string `json:"url,omitempty" yaml:"url,omitempty"`
	// +optional
	CACert string `json:"ca-cert,omitempty" yaml:"ca-cert,omitempty" mapstructure:"ca-cert"`
	// +optional
	Token string `json:"token,omitempty" yaml:"token,omitempty"`
	// +optional
	SecretName string `json:"secret-name,omitempty" yaml:"secret-name,omitempty"`
	// +optional
	SecretNamespace string `json:"secret-namespace,omitempty" yaml:"secret-namespace,omitempty"`
}

type Elemental struct {
	// +optional
	Install Install `json:"install,omitempty" yaml:"install,omitempty"`
	// +optional
	// +kubebuilder:default:={"reset-persistent":true,"reset-oem":true,"reboot":true}
	Reset Reset `json:"reset,omitempty" yaml:"reset,omitempty"`
	// +optional
	Registration Registration `json:"registration,omitempty" yaml:"registration,omitempty"`
	// +optional
	SystemAgent SystemAgent `json:"system-agent,omitempty" yaml:"system-agent,omitempty"`
	// InstallTarget pins the disk of one install. The operator sets it in the
	// configuration it returns to a host it released for installation; it is
	// not taken from MachineRegistration.
	// +optional
	InstallTarget *InstallTarget `json:"install-target,omitempty" yaml:"install-target,omitempty"`
	// InstallRegistry is the registry mirror the install pulls the system
	// image through. The operator sets it in the configuration that starts
	// an install with a system-uri, from the registry mirror input the
	// MachineRegistration selects with its cpaas.io/registry-reference label
	// (public-registry-credential by default); it is not taken from
	// MachineRegistration.
	// +optional
	InstallRegistry *InstallRegistry `json:"install-registry,omitempty" yaml:"install-registry,omitempty"`
}

// InstallRegistry maps the logical registry onto its real endpoints for a
// live install. Hosts never verify the endpoints' certificates and keep none
// of it in the installed system.
type InstallRegistry struct {
	// Source is the logical registry, cpaas-registry.cpaas-system.
	Source string `json:"source" yaml:"source"`
	// Endpoints are the real registries, tried in order.
	Endpoints []string `json:"endpoints" yaml:"endpoints"`
	// DockerConfig is a dockerconfigjson document with the endpoints'
	// credentials; empty auths for an anonymous mirror.
	// +optional
	DockerConfig string `json:"docker-config,omitempty" yaml:"docker-config,omitempty"`
}

// InstallTarget names the disk an install must use by its canonical ID, as
// elemental-register reports it, and the stale system disks the operator
// approved for wiping.
type InstallTarget struct {
	// DeviceID is the canonical ID of the target disk: wwn:…, nvme-eui:…,
	// nvme-nguid:… or serial:…. It takes precedence over install.device and
	// install.device-selector.
	// +optional
	DeviceID string `json:"device-id,omitempty" yaml:"device-id,omitempty"`
	// Expect lists properties the target must still have; empty fields are
	// not checked.
	// +optional
	Expect InstallTargetExpect `json:"expect,omitempty" yaml:"expect,omitempty"`
	// WipeDeviceIDs lists the stale system disks the operator approved for
	// wiping. Unset (null) keeps the default of wiping every stale system disk
	// found. Set, even to an empty list, only the listed disks are wiped and
	// any other stale system disk stops the install. It is a pointer so an
	// empty list survives the YAML round trip to the host.
	// +optional
	WipeDeviceIDs *[]string `json:"wipe-device-ids,omitempty" yaml:"wipe-device-ids,omitempty"`
}

// InstallTargetExpect are the properties the operator saw on the target disk.
type InstallTargetExpect struct {
	// +optional
	SizeBytes int64 `json:"size-bytes,omitempty" yaml:"size-bytes,omitempty"`
	// +optional
	WWN string `json:"wwn,omitempty" yaml:"wwn,omitempty"`
	// +optional
	Serial string `json:"serial,omitempty" yaml:"serial,omitempty"`
	// +optional
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
}

type Config struct {
	// +optional
	Elemental Elemental `json:"elemental,omitempty" yaml:"elemental"`
	// +optional
	Network NetworkTemplate `json:"network,omitempty" yaml:"network,omitempty"`
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:XPreserveUnknownFields
	// +optional
	CloudConfig map[string]runtime.RawExtension `json:"cloud-config,omitempty" yaml:"cloud-config,omitempty"`
}

type DeviceSelector []DeviceSelectorRequirement

// DeviceSelectorRequirement narrows the install candidates. All requirements
// of a selector must hold, and exactly one disk must remain. Name, ID, WWN,
// Serial, Model and Transport take In or NotIn; Rotational takes In with
// "true" or "false"; Size takes Gt or Lt with a quantity.
type DeviceSelectorRequirement struct {
	// +required
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Name;Size;ID;WWN;Serial;Model;Transport;Rotational
	Key DeviceSelectorKey `json:"key"`
	// +required
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=In;NotIn;Gt;Lt
	Operator DeviceSelectorOperator `json:"operator"`
	// +optional
	Values []string `json:"values,omitempty"`
}

type DeviceSelectorKey string
type DeviceSelectorOperator string

const (
	DeviceSelectorOpIn    DeviceSelectorOperator = "In"
	DeviceSelectorOpNotIn DeviceSelectorOperator = "NotIn"
	DeviceSelectorOpGt    DeviceSelectorOperator = "Gt"
	DeviceSelectorOpLt    DeviceSelectorOperator = "Lt"

	DeviceSelectorKeyName DeviceSelectorKey = "Name"
	DeviceSelectorKeySize DeviceSelectorKey = "Size"
	// DeviceSelectorKeyID matches the canonical device ID (wwn:…, nvme-eui:…).
	DeviceSelectorKeyID DeviceSelectorKey = "ID"
	// DeviceSelectorKeyWWN matches the WWN lsblk reports, with or without 0x.
	DeviceSelectorKeyWWN DeviceSelectorKey = "WWN"
	// DeviceSelectorKeySerial matches the serial number lsblk reports.
	DeviceSelectorKeySerial DeviceSelectorKey = "Serial"
	// DeviceSelectorKeyModel matches the model lsblk reports, ignoring case.
	DeviceSelectorKeyModel DeviceSelectorKey = "Model"
	// DeviceSelectorKeyTransport matches the lsblk transport: sata, sas, nvme, usb, ...
	DeviceSelectorKeyTransport DeviceSelectorKey = "Transport"
	// DeviceSelectorKeyRotational matches "true" for spinning disks, "false" otherwise.
	DeviceSelectorKeyRotational DeviceSelectorKey = "Rotational"
)

// NetworkTemplate contains a map of IPAddressPools and a schemaless network config template.
type NetworkTemplate struct {
	// Configurator
	// +kubebuilder:validation:Enum=none;nmc;nmstate;nmconnections
	// +kubebuilder:default:=none
	Configurator string `json:"configurator,omitempty" yaml:"configurator,omitempty"`
	// IPAddresses contains a map of IPPools references
	IPAddresses map[string]*corev1.TypedLocalObjectReference `json:"ipAddresses,omitempty" yaml:"ipAddresses,omitempty"`
	// Config contains the network config template (nmc, nmstate, or nmconnections formats)
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:XPreserveUnknownFields
	// +optional
	Config map[string]runtime.RawExtension `json:"config,omitempty" yaml:"config,omitempty"`
}

// NetworkConfig contains a map of claimed IPAddresses and a schemaless network config template.
// This NetworkConfig is a digested NetworkTemplate, the MachineInventory's NetworkConfigReady condition
// highlight that this config is ready to be consumed, this means all needed IPAddressClaims for this machine
// have been created and the IPAM provider served real IPAddresses that can be applied to the machine.
//
// Note that the Config is the same as in NetworkTemplate, so actually a template, not a fully digested config.
// An alternative could be to simplify this object to contain final connections where the variable
// substitution already took place.
// Right now we send both Config template and real IPAddresses so the consumer (elemental-register)
// can do the substitution itself.
type NetworkConfig struct {
	// Configurator
	// +kubebuilder:validation:Enum=none;nmc;nmstate;nmconnections
	// +kubebuilder:default:=none
	Configurator string `json:"configurator,omitempty" yaml:"configurator,omitempty"`
	// IPAddresses contains a map of claimed IPAddresses
	IPAddresses map[string]string `json:"ipAddresses,omitempty" yaml:"ipAddresses,omitempty"`
	// Config contains the network config template (nmc, nmstate, or nmconnections formats)
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:XPreserveUnknownFields
	// +optional
	Config map[string]runtime.RawExtension `json:"config,omitempty" yaml:"config,omitempty"`
}
