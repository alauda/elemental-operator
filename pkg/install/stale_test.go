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

func installConfig(device string) elementalv1.Config {
	return elementalv1.Config{Elemental: elementalv1.Elemental{Install: elementalv1.Install{Device: device}}}
}

// wipeCommands is the sequence that wipes one disk with the given partitions.
func wipeCommands(disk string, size int64, partitions ...string) []string {
	var commands []string
	for _, part := range partitions {
		commands = append(commands, "wipefs --all --force "+part)
	}
	commands = append(commands, "sgdisk --zap-all "+disk, "wipefs --all --force "+disk)
	for _, r := range zeroRanges(size) {
		commands = append(commands, fmt.Sprintf("dd if=/dev/zero of=%s bs=1M count=%d iflag=count_bytes seek=%d oflag=seek_bytes conv=fsync status=none", disk, r.length, r.offset))
	}
	return append(commands, "blockdev --rereadpt "+disk)
}

var settleCommands = []string{"udevadm trigger --subsystem-match=block", "udevadm settle"}

var _ = Describe("stale system disks", Label("installer", "install", "stale"), func() {
	var host *fakeHost
	BeforeEach(func() {
		host = twoDisks()
		host.live = true
		host.disk("sdb").elementalLayout()
	})

	sdbPartitions := []string{"/dev/sdb1", "/dev/sdb2", "/dev/sdb3", "/dev/sdb4", "/dev/sdb5"}
	elementalEvidence := []string{
		"COS_GRUB", "COS_OEM", "COS_PERSISTENT", "COS_RECOVERY", "COS_STATE",
		"PARTLABEL=efi", "PARTLABEL=oem", "PARTLABEL=persistent", "PARTLABEL=recovery", "PARTLABEL=state",
	}

	It("wipes the stale disks, then the target, and checks the result", func() {
		env := newDiskTestEnv(host, nil)
		config := installConfig("/dev/sda")
		plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())
		Expect(destructive(host.calls)).To(BeEmpty())
		Expect(plan.survey.Stale.Disks).To(HaveLen(1))
		stale := plan.survey.Stale.Disks[0]
		Expect(stale.ID).To(Equal("wwn:5000c500000000b2"))
		Expect(stale.Path).To(Equal("/dev/sdb"))
		Expect(stale.Action).To(Equal(staleDiskWipe))
		Expect(stale.Evidence).To(Equal(elementalEvidence))

		Expect(env.inst.finalizeInstallDisks(plan)).To(Succeed())
		want := wipeCommands("/dev/sdb", 2000*gib, sdbPartitions...)
		want = append(want, wipeCommands("/dev/sda", 480*gib)...)
		Expect(destructive(host.calls)).To(Equal(append(want, settleCommands...)))
		Expect(host.disk("sdb").partitions).To(BeEmpty())
	})

	It("leaves the target alone with no-format", func() {
		env := newDiskTestEnv(host, nil)
		config := installConfig("/dev/sda")
		config.Elemental.Install.NoFormat = true
		plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())
		Expect(env.inst.finalizeInstallDisks(plan)).To(Succeed())
		Expect(destructive(host.calls)).To(Equal(append(wipeCommands("/dev/sdb", 2000*gib, sdbPartitions...), settleCommands...)))
	})

	It("still wipes without sgdisk", func() {
		host.missing = map[string]bool{"sgdisk": true}
		env := newDiskTestEnv(host, nil)
		config := installConfig("/dev/sda")
		plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())
		Expect(env.inst.finalizeInstallDisks(plan)).To(Succeed())
		Expect(destructive(host.calls)).To(ContainElement("blockdev --rereadpt /dev/sdb"))
	})

	It("does not install when a wipe did not take", func() {
		host.stuck = map[string]bool{"/dev/sdb": true}
		env := newDiskTestEnv(host, nil)
		config := installConfig("/dev/sda")
		plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())
		err = env.inst.finalizeInstallDisks(plan)
		Expect(reasonOf(err)).To(Equal(reasonWipeFailed))
		Expect(err.Error()).To(ContainSubstring("/dev/sdb still carries COS_GRUB"))
		Expect(err.Error()).To(ContainSubstring("/dev/sdb still has 5 partitions"))
	})

	It("stops the wipe at the first failing command", func() {
		host.missing = map[string]bool{"blockdev": true}
		env := newDiskTestEnv(host, nil)
		config := installConfig("/dev/sda")
		plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())
		err = env.inst.finalizeInstallDisks(plan)
		Expect(reasonOf(err)).To(Equal(reasonWipeFailed))
		Expect(err.Error()).To(ContainSubstring("wiping /dev/sdb: blockdev --rereadpt /dev/sdb"))
		Expect(destructive(host.calls)).NotTo(ContainElement(ContainSubstring("/dev/sda")))
	})

	It("touches no disk outside a live environment", func() {
		host.live = false
		env := newDiskTestEnv(host, nil)
		config := installConfig("/dev/sda")
		plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.survey.Cleanup).To(BeFalse())
		Expect(plan.survey.CleanupSkipped).To(Equal("not a live environment"))
		Expect(env.inst.finalizeInstallDisks(plan)).To(Succeed())
		Expect(destructive(host.calls)).To(BeEmpty())
	})

	It("touches no disk when / is on a disk", func() {
		root := sataDisk("sdc", 32, 100, "5000c500000000c3").part("xfs", "root", "")
		root.partitions[0].mounts = []string{"/"}
		host.disks = append(host.disks, root)
		env := newDiskTestEnv(host, nil)
		config := installConfig("/dev/sda")
		plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.survey.CleanupSkipped).To(Equal("/ is mounted from /dev/sdc"))
		Expect(env.inst.finalizeInstallDisks(plan)).To(Succeed())
		Expect(destructive(host.calls)).To(BeEmpty())
	})

	DescribeTable("stops the install without touching any disk",
		func(tweak func(*fakeHost, *elementalv1.Config), reason string) {
			config := installConfig("/dev/sda")
			tweak(host, &config)
			env := newDiskTestEnv(host, nil)
			_, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
			Expect(reasonOf(err)).To(Equal(reasonBlocked))
			Expect(err.Error()).To(ContainSubstring(reason))
			var diskErr *installDiskError
			Expect(errors.As(err, &diskErr)).To(BeTrue())
			blocked := diskErr.Stale.withAction(staleDiskBlocked)
			Expect(blocked).NotTo(BeEmpty())
			Expect(blocked[0].Path).To(Equal("/dev/sdb"))
			Expect(blocked[0].Reason).To(ContainSubstring(reason))
			Expect(config.Elemental.Install.Device).To(Equal("/dev/sda"))
			Expect(destructive(host.calls)).To(BeEmpty())
		},
		Entry("a partition that is not part of the old system",
			func(h *fakeHost, _ *elementalv1.Config) { h.disk("sdb").part("xfs", "DATA", "data") },
			"holds more than the old system: partition /dev/sdb6 (xfs, label DATA, name data)"),
		Entry("a mounted partition",
			func(h *fakeHost, _ *elementalv1.Config) { h.disk("sdb").partitions[4].mounts = []string{"/mnt/old"} },
			"/dev/sdb5 is mounted on /mnt/old"),
		Entry("an active swap partition",
			func(h *fakeHost, _ *elementalv1.Config) {
				h.disk("sdb").part("swap", "", "")
				h.disk("sdb").partitions[5].mounts = []string{"[SWAP]"}
			},
			"/dev/sdb6 is in use as swap"),
		Entry("a partition held by device-mapper",
			func(h *fakeHost, _ *elementalv1.Config) { h.disk("sdb").partitions[3].holders = []string{"dm-3"} },
			"/dev/sdb4 is held by dm-3"),
		Entry("a volume group on a partition",
			func(h *fakeHost, _ *elementalv1.Config) {
				h.disk("sdb").partitions[4].users = []*fakeDisk{{name: "dm-1", path: "/dev/mapper/vg-old", major: 253, minor: 1, typ: "lvm", size: gib}}
			},
			"/dev/sdb5 is used by /dev/mapper/vg-old (lvm)"),
		Entry("no stable ID",
			func(h *fakeHost, _ *elementalv1.Config) {
				h.disk("sdb").byID, h.disk("sdb").wwn, h.disk("sdb").tran = nil, "", ""
			},
			"no unique stable ID: no WWN or EUI and bus not proven local"),
		Entry("a WWN another path of the same LUN carries",
			func(h *fakeHost, _ *elementalv1.Config) {
				path := sataDisk("sdc", 32, 2000, "5000c500000000b2").elementalLayout()
				path.byID = nil
				h.disks = append(h.disks, path)
			},
			"no unique stable ID: identity is shared with another device on this host"),
		Entry("a removable disk",
			func(h *fakeHost, _ *elementalv1.Config) { h.disk("sdb").rm = true },
			"removable"),
		Entry("a read-only disk",
			func(h *fakeHost, _ *elementalv1.Config) { h.disk("sdb").ro = true },
			"read-only"),
		Entry("unreadable signatures",
			func(h *fakeHost, _ *elementalv1.Config) { h.disk("sdb").wipefsFails = true },
			"unreadable signatures on /dev/sdb"),
		Entry("a disk the operator did not approve",
			func(_ *fakeHost, c *elementalv1.Config) {
				c.Elemental.InstallTarget = &elementalv1.InstallTarget{DeviceID: "wwn:5000c500000000a1", WipeDeviceIDs: &[]string{}}
			},
			"not in the wipe list the operator approved"),
	)

	It("wipes only the disks the operator approved", func() {
		host.disks = append(host.disks, sataDisk("sdc", 32, 100, "5000c500000000c3").kubeosLayout())
		config := installConfig("")
		config.Elemental.InstallTarget = &elementalv1.InstallTarget{DeviceID: "wwn:5000c500000000a1", WipeDeviceIDs: &[]string{"WWN:0x5000c500000000b2"}}
		env := newDiskTestEnv(host, nil)
		_, err := env.inst.prepareInstallDisks(&config, KubeOSInstaller)
		Expect(reasonOf(err)).To(Equal(reasonBlocked))
		Expect(err.Error()).To(ContainSubstring("wwn:5000c500000000c3 (/dev/sdc): not in the wipe list the operator approved"))
		Expect(err.Error()).NotTo(ContainSubstring("/dev/sdb"))

		*config.Elemental.InstallTarget.WipeDeviceIDs = append(*config.Elemental.InstallTarget.WipeDeviceIDs, "wwn:5000c500000000c3")
		plan, err := env.inst.prepareInstallDisks(&config, KubeOSInstaller)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Elemental.Install.Device).To(Equal("/dev/sda"))
		Expect(env.inst.finalizeInstallDisks(plan)).To(Succeed())
		Expect(destructive(host.calls)).To(ContainElements("blockdev --rereadpt /dev/sdb", "blockdev --rereadpt /dev/sdc", "blockdev --rereadpt /dev/sda"))
	})

	It("reports a target with an old layout as the target and wipes it as such", func() {
		host.disk("sda").kubeosLayout()
		env := newDiskTestEnv(host, nil)
		config := installConfig("/dev/disk/by-id/wwn-0x5000c500000000a1")
		plan, err := env.inst.prepareInstallDisks(&config, KubeOSInstaller)
		Expect(err).NotTo(HaveOccurred())
		actions := map[string]staleDiskAction{}
		for _, disk := range plan.survey.Stale.Disks {
			actions[disk.Path] = disk.Action
		}
		Expect(actions).To(Equal(map[string]staleDiskAction{"/dev/sda": staleDiskTarget, "/dev/sdb": staleDiskWipe}))
		Expect(env.inst.finalizeInstallDisks(plan)).To(Succeed())
		want := wipeCommands("/dev/sdb", 2000*gib, sdbPartitions...)
		want = append(want, wipeCommands("/dev/sda", 480*gib, "/dev/sda1", "/dev/sda2", "/dev/sda3", "/dev/sda4")...)
		Expect(destructive(host.calls)).To(Equal(append(want, settleCommands...)))
	})

	It("detects a KubeOS layout", func() {
		host.disk("sdb").partitions = nil
		host.disk("sdb").kubeosLayout()
		env := newDiskTestEnv(host, nil)
		inv, err := env.inst.collectDiskInventory()
		Expect(err).NotTo(HaveOccurred())
		report := detectStaleDisks(inv, "/dev/sda", nil)
		Expect(report.Disks).To(HaveLen(1))
		Expect(report.Disks[0].Evidence).To(Equal([]string{"PERSIST", "ROOT-A", "ROOT-B"}))
		Expect(report.Disks[0].Action).To(Equal(staleDiskWipe))
	})

	DescribeTable("never takes the live medium or a data disk for a stale disk",
		func(label string, mounts []string) {
			// A dd'ed ISO: iso9660 on the whole disk, the EFI image as a
			// partition labelled like an installed system's.
			usb := sataDisk("sdc", 32, 16, "5000c500000000c3")
			usb.tran, usb.fstype, usb.label = "usb", "iso9660", label
			usb.part("vfat", "COS_GRUB", "")
			usb.partitions[0].mounts = mounts
			lvm := sataDisk("sdd", 48, 4000, "5000c500000000d4")
			lvm.fstype = "LVM2_member"
			host.disks = append(host.disks, usb, lvm)
			env := newDiskTestEnv(host, nil)
			config := installConfig("/dev/sda")
			plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
			Expect(err).NotTo(HaveOccurred())
			Expect(plan.survey.Stale.Disks).To(HaveLen(1))
			Expect(env.inst.finalizeInstallDisks(plan)).To(Succeed())
			for _, command := range destructive(host.calls) {
				Expect(command).NotTo(ContainSubstring("/dev/sdc"))
				Expect(command).NotTo(ContainSubstring("/dev/sdd"))
			}
		},
		Entry("labelled COS_LIVE", "COS_LIVE", nil),
		Entry("mounted on /run/initramfs/live", "CUSTOM_ISO", []string{liveMediaMount}),
	)

	It("surveys candidates and stale disks without touching anything", func() {
		lvm := sataDisk("sdd", 48, 4000, "5000c500000000d4")
		lvm.fstype = "LVM2_member"
		host.disks = append(host.disks, lvm)
		env := newDiskTestEnv(host, nil)
		inv, err := env.inst.collectDiskInventory()
		Expect(err).NotTo(HaveOccurred())
		survey := env.inst.inspectInstallDisks(inv, "", nil)
		Expect(survey.Cleanup).To(BeTrue())
		Expect(survey.Candidates).To(HaveLen(3))
		Expect(survey.Candidates[1]).To(Equal(diskCandidate{
			ID: "wwn:5000c500000000b2", Path: "/dev/sdb", SizeBytes: 2000 * gib, Model: "ST2000NM", Serial: "SER-SDB",
			WWN: "0x5000c500000000b2", Transport: "sas", Rotational: true, StaleSystem: true, Eligible: true,
		}))
		Expect(survey.Candidates[2].HasData).To(Equal([]string{"LVM2_member"}))
		Expect(survey.Candidates[2].Eligible).To(BeTrue())
		Expect(survey.Stale.Disks).To(HaveLen(1))
		Expect(survey.Stale.Disks[0].Action).To(Equal(staleDiskWipe))
		Expect(destructive(host.calls)).To(BeEmpty())
	})
})

var _ = Describe("wiping", Label("installer", "stale"), func() {
	DescribeTable("zero both ends of a disk",
		func(size int64, want []byteRange) {
			Expect(zeroRanges(size)).To(Equal(want))
		},
		Entry("nothing to zero", int64(0), []byteRange(nil)),
		Entry("a disk smaller than 64 MiB", int64(10<<20), []byteRange{{0, 10 << 20}}),
		Entry("overlapping ends", int64(100<<20), []byteRange{{0, 64 << 20}, {64 << 20, 36 << 20}}),
		Entry("a large disk", int64(1<<30)+512, []byteRange{{0, 64 << 20}, {(1 << 30) + 512 - (64 << 20), 64 << 20}}),
	)
})
