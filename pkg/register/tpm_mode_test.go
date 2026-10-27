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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/tpm"
)

// fakeHardwareTPM stands in for tpm.Prober and counts how it is used.
type fakeHardwareTPM struct {
	probe   tpm.ProbeResult
	present bool
	device  string
	reason  string
	probes  int
	waits   int
}

func (f *fakeHardwareTPM) Probe() tpm.ProbeResult { f.probes++; return f.probe }

func (f *fakeHardwareTPM) Present() bool { return f.present }

func (f *fakeHardwareTPM) WaitForDevice() (string, string) {
	f.waits++
	return f.device, f.reason
}

var usableTPM = tpm.ProbeResult{Usable: true, Device: "/dev/tpmrm0"}
var missingTPM = tpm.ProbeResult{Reason: "no TPM device in /sys/class/tpm"}

// fakeDerivedSeed is what the fake seed hook derives for a salt.
func fakeDerivedSeed(salt uint32) int64 { return 7000 + int64(salt) }

// useFakeTPMHooks replaces the hardware TPM and the v2 derivation, which
// would read this machine's SMBIOS data.
func useFakeTPMHooks(hw *fakeHardwareTPM) {
	origHW, origSeed, origNow := newHardwareTPM, getTPMSeed, timeNow
	newHardwareTPM = func() hardwareTPM { return hw }
	getTPMSeed = func(reg elementalv1.Registration, usePrevious bool, previous int64, salt uint32) (tpm.Seed, error) {
		switch {
		case usePrevious:
			return tpm.Seed{Value: previous, Previous: true}, nil
		case reg.EmulatedTPMSeed > 0:
			return tpm.Seed{Value: reg.EmulatedTPMSeed, Static: true}, nil
		default:
			return tpm.Seed{Value: fakeDerivedSeed(salt), Version: tpm.SeedVersionV2, Salt: salt}, nil
		}
	}
	timeNow = func() time.Time { return time.Date(2026, time.October, 27, 8, 0, 0, 0, time.UTC) }
	DeferCleanup(func() { newHardwareTPM, getTPMSeed, timeNow = origHW, origSeed, origNow })
}

func tpmReg(mode string, emulate bool) elementalv1.Registration {
	return elementalv1.Registration{Auth: "tpm", TPMMode: mode, EmulateTPM: emulate, EmulatedTPMSeed: -1}
}

var _ = Describe("tpm-mode for a new identity", Label("registration", "tpm-mode"), func() {
	var hw *fakeHardwareTPM
	var state *State
	BeforeEach(func() {
		hw = &fakeHardwareTPM{}
		useFakeTPMHooks(hw)
		state = &State{}
	})

	It("follows emulate-tpm when tpm-mode is empty, without probing", func() {
		id, err := resolveTPMIdentity(tpmReg("", true), state)
		Expect(err).ToNot(HaveOccurred())
		Expect(id.mode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(id.saltRetry).To(BeTrue())
		Expect(hw.probes).To(BeZero())
		Expect(*state).To(Equal(State{
			EmulatedTPM:            true,
			EmulatedTPMSeed:        fakeDerivedSeed(0),
			EmulatedTPMSeedVersion: tpm.SeedVersionV2,
			TPMMode:                elementalv1.TPMModeEmulated,
			TPMModeSource:          TPMModeSourceEmulateTPM,
		}))

		state = &State{}
		id, err = resolveTPMIdentity(tpmReg("", false), state)
		Expect(err).ToNot(HaveOccurred())
		Expect(id.mode).To(Equal(elementalv1.TPMModeHardware))
		Expect(state.TPMModeSource).To(Equal(TPMModeSourceEmulateTPM))
		Expect(hw.probes).To(BeZero())
	})
	It("auto uses a hardware TPM that passes the probe", func() {
		hw.probe = usableTPM
		id, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeAuto, false), state)
		Expect(err).ToNot(HaveOccurred())
		Expect(id.mode).To(Equal(elementalv1.TPMModeHardware))
		Expect(id.saltRetry).To(BeFalse())
		Expect(state.TPMMode).To(Equal(elementalv1.TPMModeHardware))
		Expect(state.TPMModeSource).To(Equal(TPMModeSourceProbe))
		Expect(state.TPMProbe).To(Equal(&TPMProbe{Result: TPMProbePassed, At: timeNow()}))
		Expect(state.EmulatedTPM).To(BeFalse())
	})
	It("auto falls back to emulation and records why", func() {
		hw.probe = missingTPM
		id, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeAuto, false), state)
		Expect(err).ToNot(HaveOccurred())
		Expect(id.mode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(id.saltRetry).To(BeTrue())
		Expect(state.TPMMode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(state.TPMModeSource).To(Equal(TPMModeSourceProbe))
		Expect(state.TPMProbe).To(Equal(&TPMProbe{Result: TPMProbeFailed, Reason: missingTPM.Reason, At: timeNow()}))
		Expect(state.EmulatedTPM).To(BeTrue())
		Expect(state.EmulatedTPMSeed).To(Equal(fakeDerivedSeed(0)))
	})
	It("hardware fails instead of falling back to emulation", func() {
		hw.probe = missingTPM
		_, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeHardware, false), state)
		Expect(err).To(MatchError(tpm.ErrHardwareTPMUnavailable))
		Expect(err.Error()).To(ContainSubstring(missingTPM.Reason))
		Expect(state.EmulatedTPM).To(BeFalse())
		Expect(state.TPMMode).To(BeEmpty())
	})
	It("hardware uses a TPM that passes the probe", func() {
		hw.probe = usableTPM
		id, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeHardware, false), state)
		Expect(err).ToNot(HaveOccurred())
		Expect(id.mode).To(Equal(elementalv1.TPMModeHardware))
		Expect(state.TPMModeSource).To(Equal(TPMModeSourceTPMMode))
		Expect(hw.probes).To(Equal(1))
	})
	It("never retries a static seed", func() {
		reg := tpmReg(elementalv1.TPMModeEmulated, false)
		reg.EmulatedTPMSeed = 42
		id, err := resolveTPMIdentity(reg, state)
		Expect(err).ToNot(HaveOccurred())
		Expect(id.saltRetry).To(BeFalse())
		Expect(state.EmulatedTPMSeed).To(Equal(int64(42)))
		Expect(state.EmulatedTPMSeedVersion).To(BeZero())
		Expect(registrationAnnotations(reg, state, &id)[TPMSeedVersionAnnotation]).To(Equal("static"))
	})
	It("rejects tpm-mode auto next to emulate-tpm", func() {
		_, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeAuto, true), state)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("emulate-tpm"))
		Expect(hw.probes).To(BeZero())
	})
	It("keeps the decision of the first attempt across salt retries", func() {
		hw.probe = missingTPM
		_, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeAuto, false), state)
		Expect(err).ToNot(HaveOccurred())
		retry := State{TPMMode: state.TPMMode, TPMModeSource: state.TPMModeSource, TPMProbe: state.TPMProbe, EmulatedTPMSeedSalt: 3}
		hw.probe = usableTPM // must not matter any more
		id, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeAuto, false), &retry)
		Expect(err).ToNot(HaveOccurred())
		Expect(hw.probes).To(Equal(1))
		Expect(id.mode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(retry.EmulatedTPMSeed).To(Equal(fakeDerivedSeed(3)))
		Expect(retry.EmulatedTPMSeedSalt).To(Equal(uint32(3)))
	})
})

var _ = Describe("tpm-mode of a registered host", Label("registration", "tpm-mode"), func() {
	var hw *fakeHardwareTPM
	registered := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	BeforeEach(func() {
		hw = &fakeHardwareTPM{device: "/dev/tpmrm0"}
		useFakeTPMHooks(hw)
	})

	It("neither probes nor follows the config", func() {
		state := &State{
			InitialRegistration:    registered,
			EmulatedTPM:            true,
			EmulatedTPMSeed:        fakeDerivedSeed(0),
			EmulatedTPMSeedVersion: tpm.SeedVersionV2,
			TPMMode:                elementalv1.TPMModeEmulated,
			TPMModeSource:          TPMModeSourceProbe,
		}
		hw.probe = usableTPM
		id, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeHardware, false), state)
		Expect(err).ToNot(HaveOccurred())
		Expect(hw.probes).To(BeZero())
		Expect(id.mode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(id.saltRetry).To(BeFalse())
		Expect(id.configConflict).To(Equal("config=hardware state=emulated"))
		Expect(state.EmulatedTPMSeed).To(Equal(fakeDerivedSeed(0)))
		Expect(registrationAnnotations(tpmReg(elementalv1.TPMModeHardware, false), state, &id)).To(HaveKeyWithValue("tpm-mode-conflict", "config=hardware state=emulated"))
	})
	It("keeps emulating after a hardware TPM appeared and reports it", func() {
		state := &State{InitialRegistration: registered, EmulatedTPM: true, EmulatedTPMSeed: 42, TPMMode: elementalv1.TPMModeEmulated, TPMModeSource: TPMModeSourceTPMMode}
		hw.present = true
		id, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeAuto, false), state)
		Expect(err).ToNot(HaveOccurred())
		Expect(id.mode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(id.configConflict).To(BeEmpty())
		annotations := registrationAnnotations(tpmReg(elementalv1.TPMModeAuto, false), state, &id)
		Expect(annotations).To(HaveKeyWithValue("tpm-hardware-present", "true"))
		Expect(annotations).To(HaveKeyWithValue("auth", "emulated-tpm"))
		Expect(state.EmulatedTPMSeed).To(Equal(int64(42)))
	})
	It("fails closed when its hardware TPM disappeared", func() {
		state := &State{InitialRegistration: registered, TPMMode: elementalv1.TPMModeHardware, TPMModeSource: TPMModeSourceProbe}
		hw.device, hw.reason = "", "no TPM device in /sys/class/tpm"
		_, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeAuto, false), state)
		Expect(err).To(MatchError(tpm.ErrHardwareTPMUnavailable))
		Expect(err.Error()).To(ContainSubstring("not falling back to TPM emulation"))
		Expect(state.TPMMode).To(Equal(elementalv1.TPMModeHardware))
		Expect(state.EmulatedTPM).To(BeFalse())
		Expect(hw.probes).To(BeZero())
	})
	It("takes the mode over from a state written by an earlier release", func() {
		state := &State{InitialRegistration: registered, EmulatedTPM: true, EmulatedTPMSeed: -9198403500847051214}
		id, err := resolveTPMIdentity(tpmReg(elementalv1.TPMModeAuto, false), state)
		Expect(err).ToNot(HaveOccurred())
		Expect(hw.probes).To(BeZero())
		Expect(id.mode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(state.TPMMode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(state.TPMModeSource).To(Equal(TPMModeSourceLegacyState))
		Expect(state.EmulatedTPMSeed).To(Equal(int64(-9198403500847051214)))
		Expect(registrationAnnotations(tpmReg(elementalv1.TPMModeAuto, false), state, &id)).To(HaveKeyWithValue(TPMSeedVersionAnnotation, "legacy"))

		hardware := &State{InitialRegistration: registered}
		id, err = resolveTPMIdentity(tpmReg("", true), hardware)
		Expect(err).ToNot(HaveOccurred())
		Expect(id.mode).To(Equal(elementalv1.TPMModeHardware))
		Expect(id.configConflict).To(Equal("config=emulated state=hardware"))
		Expect(hardware.TPMModeSource).To(Equal(TPMModeSourceLegacyState))
		Expect(hw.waits).To(Equal(1))
	})
})

var _ = Describe("registration annotations", Label("registration", "tpm-mode"), func() {
	It("reports the TPM actually used", func() {
		state := &State{TPMMode: elementalv1.TPMModeHardware, TPMModeSource: TPMModeSourceProbe, TPMProbe: &TPMProbe{Result: TPMProbePassed}}
		id := &tpmIdentity{mode: elementalv1.TPMModeHardware, hardwarePresent: true}
		Expect(registrationAnnotations(tpmReg(elementalv1.TPMModeAuto, false), state, id)).To(Equal(map[string]string{
			"auth":                 "tpm",
			"tpm-mode":             "hardware",
			"tpm-mode-source":      "probe",
			"tpm-mode-conflict":    "false",
			"tpm-probe":            "passed",
			"tpm-hardware-present": "true",
			"tpm-seed-version":     "none",
			"tpm-seed-salt":        "none",
		}))
	})
	It("reports an emulated fallback with its seed", func() {
		state := &State{
			EmulatedTPM: true, EmulatedTPMSeed: 1, EmulatedTPMSeedVersion: tpm.SeedVersionV2, EmulatedTPMSeedSalt: 2,
			TPMMode: elementalv1.TPMModeEmulated, TPMModeSource: TPMModeSourceProbe,
			TPMProbe: &TPMProbe{Result: TPMProbeFailed, Reason: "no TPM device"},
		}
		id := &tpmIdentity{mode: elementalv1.TPMModeEmulated, saltRetry: true}
		annotations := registrationAnnotations(tpmReg(elementalv1.TPMModeAuto, false), state, id)
		Expect(annotations).To(HaveKeyWithValue("auth", "emulated-tpm"))
		Expect(annotations).To(HaveKeyWithValue("tpm-probe", "failed: no TPM device"))
		Expect(annotations).To(HaveKeyWithValue("tpm-hardware-present", "false"))
		Expect(annotations).To(HaveKeyWithValue("tpm-seed-version", "2"))
		Expect(annotations).To(HaveKeyWithValue("tpm-seed-salt", "2"))
	})
	It("reports plain auth as configured", func() {
		Expect(registrationAnnotations(elementalv1.Registration{Auth: "sys-uuid", EmulateTPM: true}, &State{}, nil)).
			To(Equal(map[string]string{"auth": "sys-uuid"}))
	})
})

var _ = Describe("installed registration config", Label("registration", "tpm-mode", "install"), func() {
	It("replaces auto with the resolved mode and mirrors it in emulate-tpm", func() {
		reg := tpmReg(elementalv1.TPMModeAuto, false)
		emulated := InstalledRegistration(reg, State{TPMMode: elementalv1.TPMModeEmulated})
		Expect(emulated.TPMMode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(emulated.EmulateTPM).To(BeTrue())
		Expect(emulated.ValidateTPMMode()).To(Succeed())

		hardware := InstalledRegistration(reg, State{TPMMode: elementalv1.TPMModeHardware})
		Expect(hardware.TPMMode).To(Equal(elementalv1.TPMModeHardware))
		Expect(hardware.EmulateTPM).To(BeFalse())
	})
	It("leaves other auth methods alone", func() {
		reg := elementalv1.Registration{Auth: "mac", TPMMode: elementalv1.TPMModeAuto}
		Expect(InstalledRegistration(reg, State{TPMMode: elementalv1.TPMModeEmulated})).To(Equal(reg))
	})
})
