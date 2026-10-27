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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gotpm "github.com/rancher-sandbox/go-tpm"

	"github.com/rancher/elemental-operator/pkg/identity"
)

// These specs drive the go-tpm-tools simulator elemental-register embeds
// (CGO): the EK hash is the identity the registration server sees.
func emulatedEKHash(seed int64) string {
	hash, err := gotpm.GetPubHash(gotpm.Emulated, gotpm.WithSeed(seed))
	Expect(err).ToNot(HaveOccurred())
	return hash
}

var _ = Describe("emulated TPM identities", Label("tpm", "simulator"), func() {
	It("v1 gives hosts whose UUIDs differ only in the high 64 bits one EK, v2 tells them apart", func() {
		v1A, _ := SeedV1FromUUID(dellUUIDA)
		v1B, _ := SeedV1FromUUID(dellUUIDB)
		Expect(emulatedEKHash(v1A)).To(Equal(emulatedEKHash(v1B)))

		v2A, err := SeedV2(SeedMaterial{Fields: identity.NewFields(dellUUIDA, "", "")}, 0)
		Expect(err).ToNot(HaveOccurred())
		v2B, err := SeedV2(SeedMaterial{Fields: identity.NewFields(dellUUIDB, "", "")}, 0)
		Expect(err).ToNot(HaveOccurred())
		Expect(emulatedEKHash(v2A)).ToNot(Equal(emulatedEKHash(v2B)))
	})
	It("gives a new EK for the next salt", func() {
		material := SeedMaterial{Fields: identity.NewFields(dellUUIDA, "7XK9H42", "")}
		salt0, _ := SeedV2(material, 0)
		salt1, _ := SeedV2(material, 1)
		Expect(emulatedEKHash(salt0)).ToNot(Equal(emulatedEKHash(salt1)))
	})
	It("only uses the seed modulo 2^31-1, which is why collisions are caught server side", func() {
		const mod = int64(1)<<31 - 1
		Expect(emulatedEKHash(12345)).To(Equal(emulatedEKHash(12345 + mod)))
	})
	It("completes the offline attestation round trip used by the probe", func() {
		Expect(SelfTest(gotpm.Emulated, gotpm.WithSeed(goldenSeedV2))).To(Succeed())
	})
})
