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

import "fmt"

// TPM modes accepted by Registration.TPMMode. Only "hardware" and
// "emulated" are ever recorded in the elemental-register state: "auto" is
// resolved to one of them by the first registration.
const (
	TPMModeHardware = "hardware"
	TPMModeEmulated = "emulated"
	TPMModeAuto     = "auto"
)

// Annotations the registration server owns on a MachineInventory to tie it to
// the hardware that registered it. elemental-register cannot set them: the
// server drops these keys from the annotations a client sends.
const (
	// HardwareFingerprintAnnotation is "v1:<sha256>" over the normalized SMBIOS
	// system UUID, system serial number and baseboard serial number. It is
	// written when the inventory is created (or backfilled on an older
	// inventory) and compared at every later registration.
	HardwareFingerprintAnnotation = "elemental.cattle.io/hardware-fingerprint"
	// HardwareFingerprintStrengthAnnotation is HardwareFingerprintStrong when
	// at least one of the three fields carries a real value, and
	// HardwareFingerprintWeak when all of them are missing or vendor
	// placeholders. Weak fingerprints are compared but never used to move an
	// identity to another inventory.
	HardwareFingerprintStrengthAnnotation = "elemental.cattle.io/hardware-fingerprint-strength"
	HardwareFingerprintStrong             = "strong"
	HardwareFingerprintWeak               = "weak"
	// IdentityConflictAnnotation marks a MachineInventory whose identity was
	// presented by other hardware that cannot switch to a new identity by
	// itself (static seed, hardware TPM, an old client, or an installed host).
	// The value describes the conflict. The MachineInventory controller keeps
	// the inventory Ready=False/IdentityConflict until an operator removes it.
	IdentityConflictAnnotation = "elemental.cattle.io/identity-conflict"
	// AllowIdentityRebindAnnotation, set by an operator, lets a registration
	// move this inventory's identity: a hardware TPM inventory (or one in
	// identity conflict) may then be rebound to a new hash, and a fingerprint
	// mismatch on the current hash (for example after a mainboard swap) is
	// accepted; either way the identity-conflict annotation is cleared. The
	// value "true" allows whichever hardware registers next; a system UUID,
	// serial number or hardware fingerprint allows only that hardware, which
	// settles which of two colliding hosts keeps the inventory. A host
	// registering a freshly derived emulated identity never uses it. The
	// server removes the annotation once it has been used; a registration
	// that needs no rebind leaves it in place.
	AllowIdentityRebindAnnotation = "elemental.cattle.io/allow-identity-rebind"
	// MachineInventoryAuthAnnotation is the authentication elemental-register
	// reports it used: tpm, emulated-tpm, sys-uuid or mac.
	MachineInventoryAuthAnnotation = "elemental.cattle.io/auth"
	// MachineUUIDAnnotation is the annotation MachineRegistrations
	// conventionally render from ${System Information/UUID}. Inventories that
	// predate the hardware fingerprint are checked against it.
	MachineUUIDAnnotation = "elemental.cattle.io/machine-uuid"
)

// Condition reasons for hardware identity problems.
const (
	// InvalidTPMConfigReason documents a MachineRegistration whose tpm-mode
	// contradicts emulate-tpm.
	InvalidTPMConfigReason = "InvalidTPMConfig"
	// IdentityConflictReason documents a MachineInventory carrying
	// IdentityConflictAnnotation.
	IdentityConflictReason = "IdentityConflict"
)

// ValidateTPMMode reports a tpm-mode value elemental-register cannot honour.
// The empty value keeps the emulate-tpm semantics and is always valid.
func (r Registration) ValidateTPMMode() error {
	switch r.TPMMode {
	case "", TPMModeEmulated:
		return nil
	case TPMModeHardware, TPMModeAuto:
		if r.EmulateTPM {
			return fmt.Errorf("tpm-mode %q contradicts emulate-tpm: true; drop emulate-tpm or use tpm-mode %q", r.TPMMode, TPMModeEmulated)
		}
		return nil
	default:
		return fmt.Errorf("invalid tpm-mode %q: must be %q, %q or %q", r.TPMMode, TPMModeHardware, TPMModeEmulated, TPMModeAuto)
	}
}
