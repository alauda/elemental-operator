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

package identity

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestIdentity(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Hardware Identity Suite")
}

var _ = Describe("NormalizeUUID", Label("identity"), func() {
	DescribeTable("keeps real UUIDs in canonical form",
		func(raw, want string) {
			Expect(NormalizeUUID(raw)).To(Equal(want))
		},
		Entry("dmidecode upper case", "4C4C4544-0042-3510-8058-B4C04F4D4E32", "4c4c4544-0042-3510-8058-b4c04f4d4e32"),
		Entry("sysfs lower case", "4c4c4544-0051-4410-8058-b4c04f4d4e32", "4c4c4544-0051-4410-8058-b4c04f4d4e32"),
		Entry("space inside", "4c4c454400423510 8058b4c04f4d4e32", ""),
		Entry("compact", "4c4c4544004235108058b4c04f4d4e32", "4c4c4544-0042-3510-8058-b4c04f4d4e32"),
		Entry("braces and spaces", " {4C4C4544-0042-3510-8058-B4C04F4D4E32} ", "4c4c4544-0042-3510-8058-b4c04f4d4e32"),
	)
	DescribeTable("drops missing and placeholder UUIDs",
		func(raw string) {
			Expect(NormalizeUUID(raw)).To(BeEmpty())
		},
		Entry("empty", ""),
		Entry("dmidecode all zeros", "Not Settable"),
		Entry("dmidecode all ones", "Not Present"),
		Entry("ghw unreadable", "unknown"),
		Entry("all zeros", "00000000-0000-0000-0000-000000000000"),
		Entry("all F", "FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF"),
		Entry("AMI default", "03000200-0400-0500-0006-000700080009"),
		Entry("AMI default, unswapped", "00020003-0004-0005-0006-000700080009"),
		Entry("too short", "4c4c4544-0042-3510-8058"),
		Entry("not hex", "4c4c4544-0042-3510-8058-b4c04f4d4e3z"),
	)
})

var _ = Describe("NormalizeSerial", Label("identity"), func() {
	It("keeps real serial numbers, upper-cased and trimmed", func() {
		Expect(NormalizeSerial("  7xk9h42 ")).To(Equal("7XK9H42"))
		Expect(NormalizeSerial("VMware-56 4d 2a  11")).To(Equal("VMWARE-56 4D 2A 11"))
		Expect(NormalizeSerial("CN7016361S00QA")).To(Equal("CN7016361S00QA"))
	})
	DescribeTable("drops missing and placeholder serials",
		func(raw string) {
			Expect(NormalizeSerial(raw)).To(BeEmpty())
		},
		Entry("empty", ""),
		Entry("blank", "   "),
		Entry("O.E.M.", "To be filled by O.E.M."),
		Entry("O.E.M. upper", "TO BE FILLED BY O.E.M."),
		Entry("default string", "Default string"),
		Entry("not specified", "Not Specified"),
		Entry("not available", "Not Available"),
		Entry("none", "None"),
		Entry("N/A", "N/A"),
		Entry("unknown", "unknown"),
		Entry("system serial number", "System Serial Number"),
		Entry("base board serial number", "Base Board Serial Number"),
		Entry("AMI type2", "Type2 - Board Serial Number"),
		Entry("zero", "0"),
		Entry("zeros", "0000000000"),
		Entry("Hyper-V zeros", "0000-0000-0000-0000-0000-0000-00"),
		Entry("x filler", "XXXXXXXXXX"),
		Entry("counting", "0123456789"),
		Entry("punctuation only", "..."),
	)
})

var _ = Describe("Fingerprint", Label("identity"), func() {
	It("is strong when one identifier is real", func() {
		fp := NewFields("Not Settable", "To be filled by O.E.M.", "BOARD-1").Fingerprint()
		Expect(fp.Strong).To(BeTrue())
		Expect(fp.Strength()).To(Equal("strong"))
		Expect(fp.Value).To(MatchRegexp(`^v1:[0-9a-f]{64}$`))
	})
	It("is weak and shared by hosts with only placeholders", func() {
		a := NewFields("03000200-0400-0500-0006-000700080009", "Default string", "Default string").Fingerprint()
		b := NewFields("", "To be filled by O.E.M.", "").Fingerprint()
		Expect(a.Strong).To(BeFalse())
		Expect(a.Strength()).To(Equal("weak"))
		Expect(a).To(Equal(b))
	})
	It("does not depend on how the host prints its identifiers", func() {
		dmidecode := NewFields("4C4C4544-0042-3510-8058-B4C04F4D4E32", "7XK9H42", " ..CN7016 ")
		sysfs := NewFields("4c4c4544-0042-3510-8058-b4c04f4d4e32", "7xk9h42", "..cn7016")
		Expect(dmidecode.Fingerprint()).To(Equal(sysfs.Fingerprint()))
	})
	It("tells apart UUIDs that only differ in their high 64 bits", func() {
		a := NewFields("4c4c4544-0042-3510-8058-b4c04f4d4e32", "", "").Fingerprint()
		b := NewFields("4c4c4544-0051-4410-8058-b4c04f4d4e32", "", "").Fingerprint()
		Expect(a.Value).ToNot(Equal(b.Value))
	})
	It("does not let a value move between fields", func() {
		a := NewFields("", "SERIAL-1", "").Fingerprint()
		b := NewFields("", "", "SERIAL-1").Fingerprint()
		Expect(a.Value).ToNot(Equal(b.Value))
	})
	It("is stable across releases (golden)", func() {
		// Stored on MachineInventories: changing it turns every registered
		// host into an identity conflict.
		fp := NewFields("4C4C4544-0042-3510-8058-B4C04F4D4E32", "7XK9H42", "CN7016361S00QA").Fingerprint()
		Expect(fp.Value).To(Equal(goldenStrongFingerprint))
		Expect(NewFields("", "", "").Fingerprint().Value).To(Equal(goldenWeakFingerprint))
	})
})

var _ = Describe("SMBIOS payload", Label("identity"), func() {
	payload := map[string]interface{}{
		"System Information": map[string]interface{}{
			"UUID":          "4C4C4544-0042-3510-8058-B4C04F4D4E32",
			"Serial Number": "7XK9H42",
		},
		"Base Board Information": map[string]interface{}{
			"Serial Number": "..CN7016361S00QA.",
		},
	}
	It("extracts the three identifiers", func() {
		Expect(FieldsFromSMBIOS(payload)).To(Equal(Fields{
			SystemUUID:   "4c4c4544-0042-3510-8058-b4c04f4d4e32",
			SystemSerial: "7XK9H42",
			BoardSerial:  "..CN7016361S00QA.",
		}))
		Expect(RawUUIDFromSMBIOS(payload)).To(Equal("4C4C4544-0042-3510-8058-B4C04F4D4E32"))
	})
	It("treats missing sections as missing identifiers", func() {
		Expect(FieldsFromSMBIOS(map[string]interface{}{}).Valid()).To(BeFalse())
		Expect(FieldsFromSMBIOS(nil).Valid()).To(BeFalse())
	})
})

var _ = Describe("UUIDsInName", Label("identity"), func() {
	It("finds UUIDs rendered into names", func() {
		Expect(UUIDsInName("demo-4c4c4544-0042-3510-8058-b4c04f4d4e32")).To(ConsistOf("4c4c4544-0042-3510-8058-b4c04f4d4e32"))
	})
	It("ignores names the server generated", func() {
		Expect(UUIDsInName("m-0f8e4f9a-3c2b-4d1e-9a7b-6c5d4e3f2a1b")).To(BeEmpty())
	})
	It("finds nothing in plain names", func() {
		Expect(UUIDsInName("inv-r620-01")).To(BeEmpty())
	})
})

var _ = Describe("CanonicalUUID", Label("identity"), func() {
	It("keeps placeholders but unifies their spelling", func() {
		Expect(CanonicalUUID("03000200-0400-0500-0006-000700080009")).To(Equal("03000200-0400-0500-0006-000700080009"))
		Expect(CanonicalUUID(" Not Settable ")).To(Equal("not settable"))
		Expect(CanonicalUUID("4C4C4544-0042-3510-8058-B4C04F4D4E32")).To(Equal("4c4c4544-0042-3510-8058-b4c04f4d4e32"))
	})
})

// Fingerprints computed when fingerprint v1 was introduced.
const (
	goldenStrongFingerprint = "v1:178f9be1e1335efef6f1e8246361119de465c7ed7ec9e8c8f985c6fd0f95f784"
	goldenWeakFingerprint   = "v1:1244d59eae1fb63807003b726a15e91ddfd63299cf87851152d266cd4ee424cd"
)
