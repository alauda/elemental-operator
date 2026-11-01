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

package tpm

import (
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/identity"
)

func TestTPM(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "TPM Suite")
}

// fakeHost replaces the host identifier readers for one spec.
func fakeHost(uuid, systemSerial, boardSerial, mac string) {
	origIDs, origMAC := readHardwareIDs, readHostMAC
	readHardwareIDs = func() (string, string, string) { return uuid, systemSerial, boardSerial }
	readHostMAC = func() (string, error) {
		if mac == "" {
			return "", errors.New("cannot retrieve MAC address from any network interface")
		}
		return mac, nil
	}
	DeferCleanup(func() { readHardwareIDs, readHostMAC = origIDs, origMAC })
}

const (
	dellUUIDA = "4c4c4544-0042-3510-8058-b4c04f4d4e32"
	// dellUUIDB differs from dellUUIDA in the high 64 bits only.
	dellUUIDB = "4c4c4544-0051-4410-8058-b4c04f4d4e32"
)

var _ = Describe("v1 seed derivation", Label("tpm", "seed"), func() {
	// Seeds of hosts registered by earlier releases; states keep them, so the
	// derivation must never change. Values cross-checked independently as
	// the low 64 bits of the UUID read as a signed integer.
	DescribeTable("keeps its golden values",
		func(uuid string, want int64) {
			seed, ok := SeedV1FromUUID(uuid)
			Expect(ok).To(BeTrue())
			Expect(seed).To(Equal(want))
		},
		Entry("Dell A", dellUUIDA, int64(-9198403500847051214)),
		Entry("Dell B collides with A", dellUUIDB, int64(-9198403500847051214)),
		Entry("upper case", "4C4C4544-0042-3510-8058-B4C04F4D4E32", int64(-9198403500847051214)),
		Entry("all zeros gives the random-seed value", "00000000-0000-0000-0000-000000000000", int64(0)),
		Entry("all F gives the derive value", "ffffffff-ffff-ffff-ffff-ffffffffffff", int64(-1)),
		Entry("AMI placeholder", "03000200-0400-0500-0006-000700080009", int64(1688879925559305)),
		Entry("positive", "123e4567-e89b-12d3-2456-426614174000", int64(2618353239553687552)),
	)
	It("rejects what is not hex", func() {
		_, ok := SeedV1FromUUID("Not Settable")
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("v2 seed derivation", Label("tpm", "seed"), func() {
	material := func(uuid, systemSerial, boardSerial string) SeedMaterial {
		return SeedMaterial{Fields: identity.NewFields(uuid, systemSerial, boardSerial)}
	}

	It("tells apart UUIDs that only differ in their high 64 bits", func() {
		a, err := SeedV2(material(dellUUIDA, "", ""), 0)
		Expect(err).ToNot(HaveOccurred())
		b, err := SeedV2(material(dellUUIDB, "", ""), 0)
		Expect(err).ToNot(HaveOccurred())
		Expect(a).ToNot(Equal(b))
	})
	It("is deterministic and does not depend on how the UUID is printed", func() {
		a, _ := SeedV2(material(dellUUIDA, "7xk9h42", ""), 0)
		b, _ := SeedV2(material("4C4C4544-0042-3510-8058-B4C04F4D4E32", " 7XK9H42 ", "Default string"), 0)
		Expect(a).To(Equal(b))
	})
	It("changes with the salt", func() {
		a, _ := SeedV2(material(dellUUIDA, "", ""), 0)
		b, _ := SeedV2(material(dellUUIDA, "", ""), 1)
		Expect(a).ToNot(Equal(b))
	})
	It("uses the serial numbers next to a placeholder UUID", func() {
		a, err := SeedV2(material("03000200-0400-0500-0006-000700080009", "SERIAL-A", "To be filled by O.E.M."), 0)
		Expect(err).ToNot(HaveOccurred())
		b, err := SeedV2(material("03000200-0400-0500-0006-000700080009", "SERIAL-B", "To be filled by O.E.M."), 0)
		Expect(err).ToNot(HaveOccurred())
		Expect(a).ToNot(Equal(b))
	})
	It("never returns 0 or -1", func() {
		for salt := uint32(0); salt < 2000; salt++ {
			seed, err := SeedV2(material(dellUUIDA, "", ""), salt)
			Expect(err).ToNot(HaveOccurred())
			Expect(seed).ToNot(BeElementOf(int64(0), int64(-1)))
		}
	})
	It("refuses material without any identifier", func() {
		_, err := SeedV2(material("Not Settable", "Default string", ""), 0)
		Expect(err).To(MatchError(ErrNoHardwareIdentity))
	})
	It("keeps its golden value", func() {
		// Pins the material layout: a reinstalled host must find the seed,
		// and so the EK, it registered with.
		seed, err := SeedV2(material(dellUUIDA, "7XK9H42", "CN7016361S00QA"), 0)
		Expect(err).ToNot(HaveOccurred())
		Expect(seed).To(Equal(goldenSeedV2))
	})

	When("reading the host", func() {
		It("derives from the SMBIOS identifiers without reading the MAC", func() {
			fakeHost(dellUUIDA, "7XK9H42", "CN7016361S00QA", "")
			seed, err := DeriveSeedV2(0)
			Expect(err).ToNot(HaveOccurred())
			Expect(seed).To(Equal(goldenSeedV2))
		})
		It("falls back to the MAC when every SMBIOS identifier is a placeholder", func() {
			fakeHost("00000000-0000-0000-0000-000000000000", "To be filled by O.E.M.", "Default string", "52:54:00:AA:BB:01")
			a, err := DeriveSeedV2(0)
			Expect(err).ToNot(HaveOccurred())
			fakeHost("FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF", "", "unknown", "52:54:00:aa:bb:02")
			b, err := DeriveSeedV2(0)
			Expect(err).ToNot(HaveOccurred())
			Expect(a).ToNot(Equal(b))
			m, err := ReadSeedMaterial()
			Expect(err).ToNot(HaveOccurred())
			Expect(m.MAC).To(Equal("52:54:00:aa:bb:02"))
		})
		It("fails with a clear error instead of a random seed", func() {
			fakeHost("03000200-0400-0500-0006-000700080009", "Default string", "", "")
			_, err := DeriveSeedV2(0)
			Expect(err).To(MatchError(ErrNoHardwareIdentity))
			Expect(err.Error()).To(ContainSubstring("emulated-tpm-seed"))
			Expect(err.Error()).To(ContainSubstring("03000200-0400-0500-0006-000700080009"))
		})
	})
})

var _ = Describe("GetTPMSeed", Label("tpm", "seed"), func() {
	BeforeEach(func() {
		fakeHost(dellUUIDA, "7XK9H42", "CN7016361S00QA", "")
	})

	DescribeTable("keeps the seed recorded in the state whatever the config says",
		func(configSeed int64) {
			// An installed host whose state holds a v1 seed (or the old
			// default 1) must keep its EK hash after the default became -1.
			reg := elementalv1.Registration{EmulateTPM: true, EmulatedTPMSeed: configSeed}
			seed, err := GetTPMSeed(reg, true, 987654321, 3)
			Expect(err).ToNot(HaveOccurred())
			Expect(seed).To(Equal(Seed{Value: 987654321, Previous: true}))
		},
		Entry("new default -1", int64(-1)),
		Entry("old default 1", int64(1)),
		Entry("static", int64(42)),
		Entry("zero", int64(0)),
	)
	It("uses a static seed for a new identity", func() {
		seed, err := GetTPMSeed(elementalv1.Registration{EmulateTPM: true, EmulatedTPMSeed: 42}, false, 0, 0)
		Expect(err).ToNot(HaveOccurred())
		Expect(seed).To(Equal(Seed{Value: 42, Static: true}))
	})
	It("derives a v2 seed for a new identity with -1", func() {
		seed, err := GetTPMSeed(elementalv1.Registration{EmulateTPM: true, EmulatedTPMSeed: -1}, false, 0, 0)
		Expect(err).ToNot(HaveOccurred())
		Expect(seed).To(Equal(Seed{Value: goldenSeedV2, Version: SeedVersionV2}))
	})
	It("derives with the requested salt", func() {
		seed, err := GetTPMSeed(elementalv1.Registration{EmulateTPM: true, EmulatedTPMSeed: -1}, false, 0, 5)
		Expect(err).ToNot(HaveOccurred())
		want, _ := SeedV2(SeedMaterial{Fields: identity.NewFields(dellUUIDA, "7XK9H42", "CN7016361S00QA")}, 5)
		Expect(seed).To(Equal(Seed{Value: want, Version: SeedVersionV2, Salt: 5}))
	})
	It("derives instead of using 0, which would change the identity at every start", func() {
		seed, err := GetTPMSeed(elementalv1.Registration{EmulateTPM: true}, false, 0, 0)
		Expect(err).ToNot(HaveOccurred())
		Expect(seed.Version).To(Equal(SeedVersionV2))
		Expect(seed.Value).To(Equal(goldenSeedV2))
	})
})

// goldenSeedV2 is SeedV2 for dellUUIDA, "7XK9H42", "CN7016361S00QA", salt 0,
// computed when v2 was introduced.
const goldenSeedV2 = int64(-8384082019430403094)
