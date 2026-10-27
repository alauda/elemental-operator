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
	"fmt"
	"strconv"
	"time"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/tpm"
)

// Values of State.TPMModeSource.
const (
	// TPMModeSourceTPMMode: an explicit tpm-mode hardware or emulated.
	TPMModeSourceTPMMode = "tpm-mode"
	// TPMModeSourceEmulateTPM: no tpm-mode, the emulate-tpm flag decided.
	TPMModeSourceEmulateTPM = "emulate-tpm"
	// TPMModeSourceProbe: tpm-mode auto, the hardware TPM probe decided.
	TPMModeSourceProbe = "probe"
	// TPMModeSourceLegacyState: taken over from a state an earlier
	// elemental-register wrote, whose emulatedTPM flag is the mode it used.
	TPMModeSourceLegacyState = "legacy-state"
)

// Values of TPMProbe.Result.
const (
	TPMProbePassed = "passed"
	TPMProbeFailed = "failed"
)

// Annotation keys elemental-register reports about its TPM identity; the
// server prefixes them with "elemental.cattle.io/".
const (
	authAnnotation               = "auth"
	tpmModeAnnotation            = "tpm-mode"
	tpmModeSourceAnnotation      = "tpm-mode-source"
	tpmModeConflictAnnotation    = "tpm-mode-conflict"
	tpmProbeAnnotation           = "tpm-probe"
	tpmHardwarePresentAnnotation = "tpm-hardware-present"
	// TPMSeedVersionAnnotation is "2" for a seed derived with v2, "static"
	// for a seed from emulated-tpm-seed, "legacy" for a seed inherited from
	// an earlier release and "none" with a hardware TPM.
	//
	// Together with auth=emulated-tpm it is also the signal the registration
	// server relies on to tell whether an identity conflict can heal by
	// itself: a registration that sends tpm-seed-version=2 and no MsgUpdate
	// is the first registration of an identity this client derived, and on
	// an IdentityConflict error the client derives the next one (new salt)
	// and registers again. Everything else (MsgUpdate, static or legacy
	// seeds, hardware TPMs, plain auth, clients that predate this
	// annotation) cannot change its identity and needs an operator.
	TPMSeedVersionAnnotation = "tpm-seed-version"
	tpmSeedSaltAnnotation    = "tpm-seed-salt"

	authEmulatedTPM = "emulated-tpm"
	authTPM         = "tpm"
)

// hardwareTPM is the part of tpm.Prober the mode resolution needs.
type hardwareTPM interface {
	Probe() tpm.ProbeResult
	Present() bool
	WaitForDevice() (string, string)
}

// Hooks replaced in tests.
var (
	newHardwareTPM = func() hardwareTPM { return tpm.NewProber() }
	getTPMSeed     = tpm.GetTPMSeed
	timeNow        = time.Now
)

// tpmIdentity is how this run authenticates with auth "tpm".
type tpmIdentity struct {
	// mode is elementalv1.TPMModeHardware or elementalv1.TPMModeEmulated.
	mode string
	// saltRetry is true when the identity is new and its seed was derived
	// with v2 in this run: an IdentityConflict can then be answered by
	// deriving with the next salt.
	saltRetry bool
	// hardwarePresent is whether a TPM 2.0 device is visible now.
	hardwarePresent bool
	// configConflict describes a config that asks for another mode than the
	// one recorded in the state, which wins.
	configConflict string
}

// resolveTPMIdentity decides which TPM carries the identity and records the
// decision in the state. A host that registered before (state with an
// initial registration) is never probed and never follows the config; a new
// identity is decided from the config, probing the hardware TPM for
// tpm-mode auto and hardware.
func resolveTPMIdentity(reg elementalv1.Registration, state *State) (tpmIdentity, error) {
	hw := newHardwareTPM()
	if state.IsUpdatable() {
		return resumeTPMIdentity(reg, state, hw)
	}
	return newTPMIdentity(reg, state, hw)
}

func resumeTPMIdentity(reg elementalv1.Registration, state *State, hw hardwareTPM) (tpmIdentity, error) {
	if state.TPMMode == "" {
		state.TPMMode = elementalv1.TPMModeHardware
		if state.EmulatedTPM {
			state.TPMMode = elementalv1.TPMModeEmulated
		}
		state.TPMModeSource = TPMModeSourceLegacyState
		log.Infof("Registration state predates tpm-mode; keeping the %s TPM this host registered with", state.TPMMode)
	}
	id := tpmIdentity{mode: state.TPMMode}
	if wanted := configuredTPMMode(reg); wanted != elementalv1.TPMModeAuto && wanted != state.TPMMode {
		id.configConflict = fmt.Sprintf("config=%s state=%s", wanted, state.TPMMode)
		log.Warningf("The registration config asks for a %s TPM, but this host registered with a %s TPM; keeping %s (changing it would change the host identity)",
			wanted, state.TPMMode, state.TPMMode)
	}
	switch state.TPMMode {
	case elementalv1.TPMModeHardware:
		if device, reason := hw.WaitForDevice(); device == "" {
			return id, fmt.Errorf("%w: this host is bound to its hardware TPM (tpmMode %s in the registration state) but %s; "+
				"not falling back to TPM emulation, which would give it another identity: re-enable the TPM, or reinstall and rebind the MachineInventory",
				tpm.ErrHardwareTPMUnavailable, state.TPMMode, reason)
		}
		id.hardwarePresent = true
		state.EmulatedTPM = false
	case elementalv1.TPMModeEmulated:
		state.EmulatedTPM = true
		id.hardwarePresent = hw.Present()
		if id.hardwarePresent {
			log.Info("A hardware TPM is present, but this host keeps the emulated TPM identity it registered with")
		}
	default:
		return id, fmt.Errorf("unknown tpmMode %q in the registration state", state.TPMMode)
	}
	return id, nil
}

func newTPMIdentity(reg elementalv1.Registration, state *State, hw hardwareTPM) (tpmIdentity, error) {
	// A salt retry in the same run keeps the decision: probing again could
	// only change it for the worse.
	if state.TPMMode == "" {
		if err := decideTPMMode(reg, state, hw); err != nil {
			return tpmIdentity{}, err
		}
	}
	id := tpmIdentity{mode: state.TPMMode}
	switch state.TPMMode {
	case elementalv1.TPMModeHardware:
		state.EmulatedTPM = false
		// Without tpm-mode nothing probed the device; look instead of assuming.
		id.hardwarePresent = hw.Present()
	case elementalv1.TPMModeEmulated:
		seed, err := getTPMSeed(reg, false, 0, state.EmulatedTPMSeedSalt)
		if err != nil {
			return id, fmt.Errorf("choosing the emulated TPM seed: %w", err)
		}
		state.EmulatedTPM = true
		state.EmulatedTPMSeed = seed.Value
		state.EmulatedTPMSeedVersion = seed.Version
		state.EmulatedTPMSeedSalt = seed.Salt
		id.saltRetry = seed.Version == tpm.SeedVersionV2
		id.hardwarePresent = hw.Present()
	default:
		return id, fmt.Errorf("unknown TPM mode %q", state.TPMMode)
	}
	return id, nil
}

// decideTPMMode resolves the configured mode for a new identity.
func decideTPMMode(reg elementalv1.Registration, state *State, hw hardwareTPM) error {
	if err := reg.ValidateTPMMode(); err != nil {
		return err
	}
	switch reg.TPMMode {
	case "":
		state.TPMModeSource = TPMModeSourceEmulateTPM
		state.TPMMode = configuredTPMMode(reg)
	case elementalv1.TPMModeEmulated:
		state.TPMModeSource = TPMModeSourceTPMMode
		state.TPMMode = elementalv1.TPMModeEmulated
	case elementalv1.TPMModeHardware:
		state.TPMModeSource = TPMModeSourceTPMMode
		result := hw.Probe()
		state.TPMProbe = probeRecord(result)
		if !result.Usable {
			return fmt.Errorf("%w: tpm-mode is hardware but %s", tpm.ErrHardwareTPMUnavailable, result.Reason)
		}
		state.TPMMode = elementalv1.TPMModeHardware
	case elementalv1.TPMModeAuto:
		state.TPMModeSource = TPMModeSourceProbe
		result := hw.Probe()
		state.TPMProbe = probeRecord(result)
		if result.Usable {
			state.TPMMode = elementalv1.TPMModeHardware
		} else {
			state.TPMMode = elementalv1.TPMModeEmulated
			log.Warningf("tpm-mode auto: no usable hardware TPM (%s); using TPM emulation", result.Reason)
		}
	}
	log.Infof("TPM mode %s (from %s)", state.TPMMode, state.TPMModeSource)
	return nil
}

// configuredTPMMode is the mode the config asks for, without probing.
func configuredTPMMode(reg elementalv1.Registration) string {
	if reg.TPMMode != "" {
		return reg.TPMMode
	}
	if reg.EmulateTPM {
		return elementalv1.TPMModeEmulated
	}
	return elementalv1.TPMModeHardware
}

func probeRecord(result tpm.ProbeResult) *TPMProbe {
	record := &TPMProbe{Result: TPMProbePassed, At: timeNow().UTC().Truncate(time.Second)}
	if !result.Usable {
		record.Result = TPMProbeFailed
		record.Reason = result.Reason
	}
	return record
}

// registrationAnnotations are the annotations about how this host
// authenticates. auth reports the mode actually used, not the config.
func registrationAnnotations(reg elementalv1.Registration, state *State, id *tpmIdentity) map[string]string {
	data := map[string]string{authAnnotation: reg.Auth}
	if id == nil {
		return data
	}
	data[authAnnotation] = authTPM
	if id.mode == elementalv1.TPMModeEmulated {
		data[authAnnotation] = authEmulatedTPM
	}
	// Every key is sent every time: the server merges annotations, so a
	// value left out would keep whatever an earlier registration reported.
	data[tpmModeAnnotation] = state.TPMMode
	data[tpmModeSourceAnnotation] = state.TPMModeSource
	data[tpmHardwarePresentAnnotation] = strconv.FormatBool(id.hardwarePresent)
	data[tpmModeConflictAnnotation] = "false"
	if id.configConflict != "" {
		data[tpmModeConflictAnnotation] = id.configConflict
	}
	data[tpmProbeAnnotation] = "not-run"
	if state.TPMProbe != nil {
		data[tpmProbeAnnotation] = state.TPMProbe.Result
		if state.TPMProbe.Reason != "" {
			data[tpmProbeAnnotation] += ": " + state.TPMProbe.Reason
		}
	}
	data[TPMSeedVersionAnnotation], data[tpmSeedSaltAnnotation] = seedAnnotations(state)
	return data
}

func seedAnnotations(state *State) (string, string) {
	switch {
	case state.TPMMode != elementalv1.TPMModeEmulated:
		return "none", "none"
	case state.EmulatedTPMSeedVersion == tpm.SeedVersionV2:
		return strconv.Itoa(tpm.SeedVersionV2), strconv.FormatUint(uint64(state.EmulatedTPMSeedSalt), 10)
	case state.TPMModeSource == TPMModeSourceLegacyState:
		return "legacy", "none"
	default:
		return "static", "none"
	}
}

// InstalledRegistration is the registration config to write into the
// installed system: the mode this host resolved replaces tpm-mode (an auto
// must not be probed again) and emulate-tpm mirrors it, so that an older
// elemental-register after an OS rollback, which only knows emulate-tpm,
// keeps using the same TPM.
func InstalledRegistration(reg elementalv1.Registration, state State) elementalv1.Registration {
	if (reg.Auth != "" && reg.Auth != authTPM) || state.TPMMode == "" {
		return reg
	}
	reg.TPMMode = state.TPMMode
	reg.EmulateTPM = state.TPMMode == elementalv1.TPMModeEmulated
	return reg
}
