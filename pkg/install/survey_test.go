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
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

var _ = Describe("disk survey for the operator", Label("installer", "install", "survey"), func() {
	var host *fakeHost
	BeforeEach(func() {
		host = twoDisks()
		host.live = true
		host.disk("sdb").elementalLayout()
		lvm := sataDisk("sdd", 48, 4000, "5000c500000000d4")
		lvm.fstype = "LVM2_member"
		removable := sataDisk("sde", 64, 32, "5000c500000000e5")
		removable.rm = true
		host.disks = append(host.disks, lvm, removable)
	})

	staleSDB := func(action elementalv1.StaleSystemDiskAction, reason string) elementalv1.StaleSystemDisk {
		return elementalv1.StaleSystemDisk{
			ID: "wwn:5000c500000000b2",
			Evidence: []string{
				"COS_GRUB", "COS_OEM", "COS_PERSISTENT", "COS_RECOVERY", "COS_STATE",
				"PARTLABEL=efi", "PARTLABEL=oem", "PARTLABEL=persistent", "PARTLABEL=recovery", "PARTLABEL=state",
			},
			Action: action,
			Reason: reason,
		}
	}

	It("reports every whole disk and the stale ones without touching any", func() {
		env := newDiskTestEnv(host, nil)
		candidates, stale, err := env.inst.SurveyInstallDisks()
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(Equal([]elementalv1.InstallCandidate{
			{
				ID: "wwn:5000c500000000a1", Path: "/dev/sda", SizeBytes: 480 * gib, Model: "SAMSUNG MZ7LH480", Serial: "SER-SDA",
				WWN: "0x5000c500000000a1", Transport: "sata", Eligible: true,
			},
			{
				ID: "wwn:5000c500000000b2", Path: "/dev/sdb", SizeBytes: 2000 * gib, Model: "ST2000NM", Serial: "SER-SDB",
				WWN: "0x5000c500000000b2", Transport: "sas", Rotational: true, StaleSystem: true, Eligible: true,
			},
			{
				ID: "wwn:5000c500000000d4", Path: "/dev/sdd", SizeBytes: 4000 * gib, Model: "Disk SDD", Serial: "SER-SDD",
				WWN: "0x5000c500000000d4", Transport: "sata", Eligible: true, HasData: []string{"LVM2_member"},
			},
			{
				ID: "wwn:5000c500000000e5", Path: "/dev/sde", SizeBytes: 32 * gib, Model: "Disk SDE", Serial: "SER-SDE",
				WWN: "0x5000c500000000e5", Transport: "sata", Reasons: []string{"removable"},
			},
		}))
		Expect(stale).To(Equal([]elementalv1.StaleSystemDisk{staleSDB(elementalv1.StaleSystemDiskWipe, "")}))
		Expect(destructive(host.calls)).To(BeEmpty())
	})

	It("says why stale disks are left alone outside a live environment", func() {
		host.live = false
		env := newDiskTestEnv(host, nil)
		_, stale, err := env.inst.SurveyInstallDisks()
		Expect(err).NotTo(HaveOccurred())
		Expect(stale).To(Equal([]elementalv1.StaleSystemDisk{
			staleSDB(elementalv1.StaleSystemDiskWipe, "left alone: not a live environment"),
		}))
		Expect(destructive(host.calls)).To(BeEmpty())
	})

	It("keeps why a disk is blocked next to why it is left alone", func() {
		root := sataDisk("sdc", 32, 100, "5000c500000000c3").part("xfs", "root", "")
		root.partitions[0].mounts = []string{"/"}
		host.disks = append(host.disks, root)
		host.disk("sdb").partitions[4].mounts = []string{"/mnt/old"}
		env := newDiskTestEnv(host, nil)
		candidates, stale, err := env.inst.SurveyInstallDisks()
		Expect(err).NotTo(HaveOccurred())
		Expect(stale).To(Equal([]elementalv1.StaleSystemDisk{
			staleSDB(elementalv1.StaleSystemDiskBlocked, "/dev/sdb5 is mounted on /mnt/old; left alone: / is mounted from /dev/sdc"),
		}))
		Expect(candidates).To(HaveLen(5))
		Expect(candidates[1].Eligible).To(BeFalse())
		Expect(candidates[1].Reasons).To(Equal([]string{"/dev/sdb5 is mounted on /mnt/old"}))
		Expect(candidates[2].Path).To(Equal("/dev/sdc"))
		Expect(candidates[2].Reasons).To(Equal([]string{"/dev/sdc1 is mounted on /"}))
		Expect(destructive(host.calls)).To(BeEmpty())
	})

	It("fails when the disks cannot be listed", func() {
		host.missing = map[string]bool{"lsblk": true}
		env := newDiskTestEnv(host, nil)
		candidates, stale, err := env.inst.SurveyInstallDisks()
		Expect(err).To(MatchError(ContainSubstring("inspecting block devices")))
		Expect(candidates).To(BeNil())
		Expect(stale).To(BeNil())
	})
})

var _ = Describe("install disk error reasons", Label("installer", "install"), func() {
	It("finds the reason of a wrapped pre-install error", func() {
		err := fmt.Errorf("failed picking installation device: %w", diskErrorf(reasonTargetNotObserved, "no disk has ID %s", "wwn:1"))
		Expect(DiskErrorReason(err)).To(Equal(reasonTargetNotObserved))
		Expect(DiskErrorReason(fmt.Errorf("installing: %w", NewDiskError(reasonBlocked, "sdb is mounted")))).To(Equal(reasonBlocked))
		Expect(NewDiskError(reasonWipeFailed, "sgdisk failed")).To(MatchError("WipeFailed: sgdisk failed"))
	})
	It("has none for other errors", func() {
		Expect(DiskErrorReason(errors.New("kbimg install disk failed"))).To(BeEmpty())
		Expect(DiskErrorReason(nil)).To(BeEmpty())
	})
})
