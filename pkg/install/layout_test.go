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

package install

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rancher/elemental-operator/pkg/register"
)

func partitionDevice(fstype, label, partlabel string) *register.BlockDevice {
	return &register.BlockDevice{Path: "/dev/sdx" + label + partlabel, Type: blockTypePart, FSType: fstype, Label: label, PartLabel: partlabel}
}

var _ = Describe("old system layouts", Label("installer", "stale"), func() {
	disk := &register.BlockDevice{Path: "/dev/sdx", Type: blockTypeDisk, PTType: "gpt", Signatures: []register.BlockSignature{{Type: "gpt"}, {Type: "PMBR"}}}

	DescribeTable("are recognised by labels and partition names",
		func(partitions []*register.BlockDevice, elemental, kubeos bool, evidence []string) {
			layout := classifyLayout(disk, partitions)
			Expect(layout.elemental).To(Equal(elemental))
			Expect(layout.kubeos).To(Equal(kubeos))
			Expect(layout.evidence).To(Equal(evidence))
		},
		Entry("one Elemental label", []*register.BlockDevice{partitionDevice("ext4", "COS_STATE", "")}, true, false, []string{"COS_STATE"}),
		Entry("an OLD_ label", []*register.BlockDevice{partitionDevice("ext4", "OLD_COS_PERSISTENT", "")}, true, false, []string{"OLD_COS_PERSISTENT"}),
		Entry("two Elemental partition names", []*register.BlockDevice{partitionDevice("", "", "efi"), partitionDevice("", "", "state")}, true, false, []string{"PARTLABEL=efi", "PARTLABEL=state"}),
		Entry("one Elemental partition name", []*register.BlockDevice{partitionDevice("vfat", "", "efi")}, false, false, []string{"PARTLABEL=efi"}),
		Entry("two KubeOS labels", []*register.BlockDevice{partitionDevice("ext4", "ROOT-A", "primary"), partitionDevice("ext4", "PERSIST", "primary")}, false, true, []string{"PERSIST", "ROOT-A"}),
		Entry("one KubeOS label", []*register.BlockDevice{partitionDevice("ext4", "ROOT-B", "primary")}, false, false, []string{"ROOT-B"}),
		Entry("BOOT alone", []*register.BlockDevice{partitionDevice("vfat", "BOOT", "primary")}, false, false, []string{}),
		Entry("COS_LIVE", []*register.BlockDevice{partitionDevice("iso9660", "COS_LIVE", "")}, false, false, []string{}),
		Entry("a label wipefs found", []*register.BlockDevice{{Path: "/dev/sdx9", Type: blockTypePart, Signatures: []register.BlockSignature{{Type: "ext4", Label: "COS_OEM"}}}}, true, false, []string{"COS_OEM"}),
	)

	DescribeTable("tell the old system from other content",
		func(partitions []*register.BlockDevice, foreign []string) {
			layout := classifyLayout(disk, partitions)
			Expect(layout.stale()).To(BeTrue())
			Expect(layout.foreign).To(Equal(foreign))
		},
		Entry("a legacy BIOS boot partition belongs to Elemental",
			[]*register.BlockDevice{partitionDevice("", "", "bios"), partitionDevice("ext4", "COS_STATE", "state")}, []string(nil)),
		Entry("a data partition does not",
			[]*register.BlockDevice{partitionDevice("ext4", "COS_STATE", "state"), partitionDevice("ext4", "DATA", "")},
			[]string{"partition /dev/sdxDATA (ext4, label DATA)"}),
		Entry("BOOT belongs to KubeOS",
			[]*register.BlockDevice{partitionDevice("vfat", "BOOT", "primary"), partitionDevice("ext4", "ROOT-A", "primary"), partitionDevice("ext4", "ROOT-B", "primary")}, []string(nil)),
		Entry("an unlabelled partition does not belong to KubeOS",
			[]*register.BlockDevice{partitionDevice("ext4", "ROOT-A", "primary"), partitionDevice("ext4", "PERSIST", "primary"), partitionDevice("", "", "primary")},
			[]string{"partition /dev/sdxprimary (name primary)"}),
		Entry("an Elemental partition relabelled by someone does not",
			[]*register.BlockDevice{partitionDevice("ext4", "COS_OEM", "oem"), partitionDevice("ext4", "backup", "persistent")},
			[]string{"partition /dev/sdxbackuppersistent (ext4, label backup, name persistent)"}),
	)
})
