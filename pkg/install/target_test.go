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
	"github.com/twpayne/go-vfs/vfst"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

type diskTestEnv struct {
	fs   *vfst.TestFS
	host *fakeHost
	inst *installer
}

func newDiskTestEnv(host *fakeHost, extra map[string]any) *diskTestEnv {
	fs, cleanup, err := vfst.NewTestFS(merge(host.files(), extra))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(cleanup)
	return &diskTestEnv{fs: fs, host: host, inst: &installer{fs: fs, disk: host.diskOptions()}}
}

func (e *diskTestEnv) resolve(req installRequest) (*installTarget, error) {
	inv, err := e.inst.collectDiskInventory()
	Expect(err).NotTo(HaveOccurred())
	return e.inst.resolveInstallTarget(inv, req)
}

func reasonOf(err error) string {
	var diskErr *installDiskError
	if errors.As(err, &diskErr) {
		return diskErr.Reason
	}
	return ""
}

// twoDisks is a SATA SSD and a spinning SAS disk, both with WWNs.
func twoDisks() *fakeHost {
	sdb := sataDisk("sdb", 16, 2000, "5000c500000000b2")
	sdb.tran, sdb.rota, sdb.model = "sas", true, "ST2000NM"
	sda := sataDisk("sda", 0, 480, "5000c500000000a1")
	sda.model = "SAMSUNG MZ7LH480"
	return &fakeHost{disks: []*fakeDisk{sda, sdb}}
}

var _ = Describe("install target resolution", Label("installer", "install", "target"), func() {
	var host *fakeHost
	BeforeEach(func() {
		host = twoDisks()
	})

	Context("install.device", func() {
		It("follows a chain of relative links to the kernel path", func() {
			env := newDiskTestEnv(host, map[string]any{
				"/dev/elemental-install-target": &vfst.Symlink{Target: "disk/by-id/wwn-0x5000c500000000b2"},
			})
			target, err := env.resolve(installRequest{device: "/dev/elemental-install-target"})
			Expect(err).NotTo(HaveOccurred())
			Expect(target.disk.path).To(Equal("/dev/sdb"))
			Expect(target.chain).To(Equal([]string{"/dev/elemental-install-target", "/dev/disk/by-id/wwn-0x5000c500000000b2", "/dev/sdb"}))
			Expect(target.disk.dev.ID).To(Equal("wwn:5000c500000000b2"))
			Expect(target.disk.majMin).To(Equal("8:16"))
			Expect(target.disk.sizeBytes).To(Equal(2000 * gib))
		})

		It("follows an absolute link", func() {
			env := newDiskTestEnv(host, nil)
			absoluteSymlink(env.fs, "/dev/disk/by-id/wwn-0x5000c500000000a1", "/dev/elemental-install-target")
			target, err := env.resolve(installRequest{device: "/dev/elemental-install-target"})
			Expect(err).NotTo(HaveOccurred())
			Expect(target.chain).To(Equal([]string{"/dev/elemental-install-target", "/dev/disk/by-id/wwn-0x5000c500000000a1", "/dev/sda"}))
		})

		It("takes a kernel path as it is", func() {
			env := newDiskTestEnv(host, nil)
			target, err := env.resolve(installRequest{device: "/dev/sda"})
			Expect(err).NotTo(HaveOccurred())
			Expect(target.chain).To(Equal([]string{"/dev/sda"}))
		})

		It("stops at a dangling link", func() {
			env := newDiskTestEnv(host, map[string]any{
				"/dev/elemental-install-target": &vfst.Symlink{Target: "disk/by-id/wwn-0x5000c5000000ffff"},
			})
			_, err := env.resolve(installRequest{device: "/dev/elemental-install-target"})
			Expect(reasonOf(err)).To(Equal(reasonTargetNotObserved))
			Expect(err.Error()).To(ContainSubstring("/dev/elemental-install-target -> /dev/disk/by-id/wwn-0x5000c5000000ffff"))
			Expect(err.Error()).To(ContainSubstring("does not exist"))
		})

		It("refuses a link to a partition", func() {
			host.disk("sda").part("ext4", "", "")
			env := newDiskTestEnv(host, map[string]any{
				"/dev/elemental-install-target": &vfst.Symlink{Target: "sda1"},
			})
			_, err := env.resolve(installRequest{device: "/dev/elemental-install-target"})
			Expect(reasonOf(err)).To(Equal(reasonTargetIneligible))
			Expect(err.Error()).To(ContainSubstring("/dev/sda1 is a partition of /dev/sda"))
		})

		It("refuses a read-only disk", func() {
			host.disk("sdb").ro = true
			env := newDiskTestEnv(host, nil)
			_, err := env.resolve(installRequest{device: "/dev/sdb"})
			Expect(reasonOf(err)).To(Equal(reasonTargetIneligible))
			Expect(err.Error()).To(ContainSubstring("read-only"))
		})

		It("refuses an optical drive", func() {
			host.disks = append(host.disks, &fakeDisk{name: "sr0", major: 11, typ: blockTypeRom, size: 4 * gib, rm: true, scsiType: "5", fstype: "iso9660", label: "COS_LIVE", mounts: []string{liveMediaMount}})
			env := newDiskTestEnv(host, map[string]any{"/dev/cdrom": &vfst.Symlink{Target: "sr0"}})
			_, err := env.resolve(installRequest{device: "/dev/cdrom"})
			Expect(reasonOf(err)).To(Equal(reasonTargetIneligible))
			Expect(err.Error()).To(ContainSubstring("optical drive"))
			Expect(err.Error()).To(ContainSubstring("live media"))
		})

		It("refuses a disk with something mounted or swapped on", func() {
			host.disk("sdb").part("ext4", "", "").part("swap", "", "")
			host.disk("sdb").partitions[0].mounts = []string{"/mnt"}
			host.disk("sdb").partitions[1].mounts = []string{"[SWAP]"}
			env := newDiskTestEnv(host, nil)
			_, err := env.resolve(installRequest{device: "/dev/sdb"})
			Expect(reasonOf(err)).To(Equal(reasonTargetIneligible))
			Expect(err.Error()).To(ContainSubstring("/dev/sdb1 is mounted on /mnt"))
			Expect(err.Error()).To(ContainSubstring("/dev/sdb2 is in use as swap"))
		})

		It("refuses a disk something is stacked on", func() {
			host.disk("sdb").fstype = "LVM2_member"
			host.disk("sdb").holders = []string{"dm-0"}
			host.disk("sdb").users = []*fakeDisk{{name: "dm-0", path: "/dev/mapper/vg-data", major: 253, typ: "lvm", size: gib}}
			env := newDiskTestEnv(host, nil)
			_, err := env.resolve(installRequest{device: "/dev/disk/by-id/wwn-0x5000c500000000b2"})
			Expect(reasonOf(err)).To(Equal(reasonTargetIneligible))
			Expect(err.Error()).To(ContainSubstring("/dev/sdb is held by dm-0"))
			Expect(err.Error()).To(ContainSubstring("/dev/sdb is used by /dev/mapper/vg-data (lvm)"))
		})

		It("refuses a file that is not a block device", func() {
			env := newDiskTestEnv(host, map[string]any{"/dev/notadisk": "plain file"})
			_, err := env.resolve(installRequest{device: "/dev/notadisk"})
			Expect(reasonOf(err)).To(Equal(reasonTargetIneligible))
			Expect(err.Error()).To(ContainSubstring("not a block device"))
		})

		It("refuses a relative path", func() {
			env := newDiskTestEnv(host, nil)
			_, err := env.resolve(installRequest{device: "sda"})
			Expect(err).To(MatchError(ContainSubstring("not an absolute path")))
		})

		It("gives up on a symlink loop", func() {
			env := newDiskTestEnv(host, map[string]any{
				"/dev/loop-a": &vfst.Symlink{Target: "loop-b"},
				"/dev/loop-b": &vfst.Symlink{Target: "loop-a"},
			})
			_, err := env.resolve(installRequest{device: "/dev/loop-a"})
			Expect(err).To(MatchError(ContainSubstring("more than 40 symbolic links")))
		})

		It("allows a named disk that carries data and says so", func() {
			host.disk("sdb").fstype = "LVM2_member"
			env := newDiskTestEnv(host, nil)
			target, err := env.resolve(installRequest{device: "/dev/sdb"})
			Expect(err).NotTo(HaveOccurred())
			Expect(target.disk.data).To(ConsistOf("LVM2_member"))
		})
	})

	Context("install-target from the operator", func() {
		It("selects the disk with that ID, however the ID is spelled", func() {
			env := newDiskTestEnv(host, nil)
			target, err := env.resolve(installRequest{
				device: "/dev/sda",
				target: &elementalv1.InstallTarget{DeviceID: "WWN:0x5000C500000000B2"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(target.disk.path).To(Equal("/dev/sdb"))
			Expect(target.requested).To(Equal("install-target wwn:5000c500000000b2"))
		})

		It("checks what the operator expects of the disk", func() {
			env := newDiskTestEnv(host, nil)
			expect := elementalv1.InstallTargetExpect{SizeBytes: 2000 * gib, WWN: "0x5000c500000000b2", Serial: "SER-SDB", Model: "st2000nm"}
			_, err := env.resolve(installRequest{target: &elementalv1.InstallTarget{DeviceID: "wwn:5000c500000000b2", Expect: expect}})
			Expect(err).NotTo(HaveOccurred())

			expect.SizeBytes = 480 * gib
			expect.Serial = "SER-OTHER"
			_, err = env.resolve(installRequest{target: &elementalv1.InstallTarget{DeviceID: "wwn:5000c500000000b2", Expect: expect}})
			Expect(reasonOf(err)).To(Equal(reasonTargetMismatch))
			Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("size is %d, expected %d", 2000*gib, 480*gib)))
			Expect(err.Error()).To(ContainSubstring(`serial is "SER-SDB", expected "SER-OTHER"`))
		})

		It("fails when no disk has the ID and lists the disks", func() {
			env := newDiskTestEnv(host, nil)
			_, err := env.resolve(installRequest{target: &elementalv1.InstallTarget{DeviceID: "wwn:5000c5000000ffff"}})
			Expect(reasonOf(err)).To(Equal(reasonTargetNotObserved))
			Expect(err.Error()).To(ContainSubstring(`wwn:5000c500000000a1 /dev/sda 515396075520 "SAMSUNG MZ7LH480"`))
			Expect(err.Error()).To(ContainSubstring(`wwn:5000c500000000b2 /dev/sdb 2147483648000 "ST2000NM"`))
		})

		It("never selects a disk without a unique ID", func() {
			host.disk("sdb").byID = nil
			host.disk("sdb").wwn = host.disk("sda").wwn
			env := newDiskTestEnv(host, nil)
			_, err := env.resolve(installRequest{target: &elementalv1.InstallTarget{DeviceID: "wwn:5000c500000000a1"}})
			Expect(reasonOf(err)).To(Equal(reasonTargetNotObserved))
			Expect(err.Error()).To(ContainSubstring("unidentified:dev_sda"))
		})
	})

	Context("device-selector", func() {
		DescribeTable("picks exactly one disk",
			func(want string, selector ...elementalv1.DeviceSelectorRequirement) {
				env := newDiskTestEnv(host, nil)
				target, err := env.resolve(installRequest{selector: selector})
				Expect(err).NotTo(HaveOccurred())
				Expect(target.disk.path).To(Equal(want))
				Expect(target.chain).To(Equal([]string{want}))
			},
			Entry("Name with a kernel name", "/dev/sda", req("Name", "In", "sda")),
			Entry("Name with a path", "/dev/sdb", req("Name", "In", "/dev/sdb")),
			Entry("Name NotIn", "/dev/sdb", req("Name", "NotIn", "sda")),
			Entry("Size Lt", "/dev/sda", req("Size", "Lt", "1Ti")),
			Entry("Size Gt", "/dev/sdb", req("Size", "Gt", "1Ti")),
			Entry("ID", "/dev/sdb", req("ID", "In", "wwn:0x5000C500000000B2")),
			Entry("WWN", "/dev/sda", req("WWN", "In", "5000C500000000A1")),
			Entry("Serial", "/dev/sdb", req("Serial", "In", "SER-SDB")),
			Entry("Model, ignoring case", "/dev/sda", req("Model", "In", "samsung mz7lh480")),
			Entry("Transport", "/dev/sdb", req("Transport", "In", "sas")),
			Entry("Transport NotIn", "/dev/sdb", req("Transport", "NotIn", "sata", "nvme")),
			Entry("Rotational true", "/dev/sdb", req("Rotational", "In", "true")),
			Entry("Rotational false", "/dev/sda", req("Rotational", "In", "false")),
			Entry("several requirements", "/dev/sda", req("Size", "Gt", "100Gi"), req("Rotational", "In", "false"), req("Model", "NotIn", "ST2000NM")),
		)

		It("fails when nothing matches and lists every disk", func() {
			env := newDiskTestEnv(host, nil)
			_, err := env.resolve(installRequest{selector: elementalv1.DeviceSelector{req("Model", "In", "INTEL SSDSC2KB038")}})
			Expect(reasonOf(err)).To(Equal(reasonTargetNotObserved))
			Expect(err.Error()).To(ContainSubstring("device-selector [Model In (INTEL SSDSC2KB038)] matches no eligible disk"))
			Expect(err.Error()).To(ContainSubstring("/dev/sda"))
			Expect(err.Error()).To(ContainSubstring("/dev/sdb"))
		})

		It("fails instead of picking one of several matches", func() {
			env := newDiskTestEnv(host, nil)
			for range 3 {
				_, err := env.resolve(installRequest{selector: elementalv1.DeviceSelector{req("Size", "Gt", "100Gi")}})
				Expect(reasonOf(err)).To(Equal(reasonTargetAmbiguous))
				Expect(err.Error()).To(ContainSubstring("matches 2 disks (/dev/sda, /dev/sdb) but must match exactly one"))
			}
		})

		It("fails without any configuration when several disks are eligible", func() {
			env := newDiskTestEnv(host, nil)
			_, err := env.resolve(installRequest{})
			Expect(reasonOf(err)).To(Equal(reasonTargetAmbiguous))
		})

		DescribeTable("rejects malformed selectors",
			func(message string, requirement elementalv1.DeviceSelectorRequirement) {
				env := newDiskTestEnv(host, nil)
				_, err := env.resolve(installRequest{selector: elementalv1.DeviceSelector{requirement}})
				Expect(err).To(MatchError(ContainSubstring(message)))
			},
			Entry("In on Size", "the key takes Gt or Lt", req("Size", "In", "1Ti")),
			Entry("Gt on a string key", "the key takes In or NotIn", req("Model", "Gt", "x")),
			Entry("NotIn on Rotational", "the key takes In", req("Rotational", "NotIn", "true")),
			Entry("a Rotational value that is not a boolean", `not "spinning"`, req("Rotational", "In", "spinning")),
			Entry("an unparsable quantity", "failed to parse quantity", req("Size", "Lt", "big")),
			Entry("Size without a value", "exactly one quantity", req("Size", "Lt")),
			Entry("In without values", "at least one value", req("Serial", "In")),
			Entry("an unknown key", "unknown key", req("Vendor", "In", "x")),
		)
	})

	Context("automatic selection", func() {
		It("only considers fixed, idle, empty whole disks", func() {
			usbLive := sataDisk("sdc", 32, 16, "5000c500000000c3")
			usbLive.tran, usbLive.pttype = "usb", "dos"
			usbLive.fstype, usbLive.label = "iso9660", "COS_LIVE"
			usbLive.part("vfat", "COS_GRUB", "")
			lvm := sataDisk("sdd", 48, 4000, "5000c500000000d4")
			lvm.fstype = "LVM2_member"
			removable := sataDisk("sde", 64, 32, "5000c500000000e5")
			removable.rm = true
			empty := sataDisk("sdf", 80, 0, "5000c500000000f6")
			readOnly := sataDisk("sdg", 96, 100, "5000c50000000007")
			readOnly.ro = true
			unreadable := sataDisk("sdh", 112, 100, "5000c50000000008")
			unreadable.wipefsFails = true
			host.disks = []*fakeDisk{
				host.disk("sda"), usbLive, lvm, removable, empty, readOnly, unreadable,
				{name: "sr0", major: 11, typ: blockTypeRom, size: 4 * gib, rm: true, scsiType: "5"},
				{name: "loop0", major: 7, typ: "loop", size: gib, mounts: []string{"/run/rootfsbase"}},
				{name: "zram0", major: 252, size: gib, mounts: []string{"[SWAP]"}},
			}
			env := newDiskTestEnv(host, nil)
			inv, err := env.inst.collectDiskInventory()
			Expect(err).NotTo(HaveOccurred())

			target, err := env.inst.resolveInstallTarget(inv, installRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(target.disk.path).To(Equal("/dev/sda"))

			_, err = env.inst.resolveInstallTarget(inv, installRequest{selector: elementalv1.DeviceSelector{req("Name", "NotIn", "sda")}})
			Expect(reasonOf(err)).To(Equal(reasonTargetNotObserved))
			for _, excluded := range []string{
				"/dev/sdc 17179869184 \"Disk SDC\" (excluded: live media)",
				"/dev/sdd 4294967296000 \"Disk SDD\" (excluded: has data: LVM2_member)",
				"(excluded: removable)",
				"(excluded: size is 0)",
				"(excluded: read-only)",
				"(excluded: has data: unreadable signatures)",
				"/dev/zram0",
			} {
				Expect(err.Error()).To(ContainSubstring(excluded))
			}
			Expect(err.Error()).NotTo(ContainSubstring("/dev/sr0"))
			Expect(err.Error()).NotTo(ContainSubstring("/dev/loop0"))
		})

		It("does not count a disk with an old system layout as data", func() {
			host.disk("sdb").elementalLayout()
			host.disk("sda").fstype = "xfs"
			env := newDiskTestEnv(host, nil)
			target, err := env.resolve(installRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(target.disk.path).To(Equal("/dev/sdb"))
		})
	})

	DescribeTable("kbimg compatibility",
		func(kname string, ok bool) {
			if ok {
				Expect(kbimgTargetProblem(kname)).To(BeEmpty())
			} else {
				Expect(kbimgTargetProblem(kname)).To(ContainSubstring("kbimg cannot partition /dev/" + kname))
			}
		},
		Entry("sda", "sda", true),
		Entry("vdb", "vdb", true),
		Entry("xvda", "xvda", true),
		Entry("nvme0n1", "nvme0n1", true),
		Entry("mmcblk0", "mmcblk0", true),
		Entry("md126", "md126", false),
		Entry("dm-0", "dm-0", false),
		Entry("loop0", "loop0", false),
		Entry("nbd0", "nbd0", false),
		Entry("pmem0", "pmem0", false),
	)

	It("lets only the toolkit backend install on md and dm devices", func() {
		md := &fakeDisk{name: "md126", major: 9, minor: 126, typ: "raid1", size: 400 * gib}
		host.disk("sda").fstype, host.disk("sda").holders, host.disk("sda").users = "linux_raid_member", []string{"md126"}, []*fakeDisk{md}
		host.disk("sdb").fstype, host.disk("sdb").holders, host.disk("sdb").users = "linux_raid_member", []string{"md126"}, []*fakeDisk{md}
		env := newDiskTestEnv(host, nil)
		config := elementalv1.Config{Elemental: elementalv1.Elemental{Install: elementalv1.Install{Device: "/dev/md126"}}}

		_, err := env.inst.prepareInstallDisks(&config, KubeOSInstaller)
		Expect(reasonOf(err)).To(Equal(reasonTargetIneligible))
		Expect(err.Error()).To(ContainSubstring("kbimg cannot partition /dev/md126"))
		Expect(config.Elemental.Install.Device).To(Equal("/dev/md126"))

		_, err = env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Elemental.Install.Device).To(Equal("/dev/md126"))
	})

	It("writes the kernel path back into the configuration", func() {
		env := newDiskTestEnv(host, map[string]any{
			"/dev/elemental-install-target": &vfst.Symlink{Target: "disk/by-id/wwn-0x5000c500000000b2"},
		})
		config := elementalv1.Config{Elemental: elementalv1.Elemental{Install: elementalv1.Install{Device: "/dev/elemental-install-target"}}}
		plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Elemental.Install.Device).To(Equal("/dev/sdb"))
		Expect(plan.request.device).To(Equal("/dev/elemental-install-target"))
		Expect(env.inst.finalizeInstallDisks(plan)).To(Succeed())
	})

	It("refuses to go on when the target changed between resolution and install", func() {
		env := newDiskTestEnv(host, map[string]any{
			"/dev/elemental-install-target": &vfst.Symlink{Target: "disk/by-id/wwn-0x5000c500000000b2"},
		})
		config := elementalv1.Config{Elemental: elementalv1.Elemental{Install: elementalv1.Install{Device: "/dev/elemental-install-target"}}}
		plan, err := env.inst.prepareInstallDisks(&config, ToolkitInstaller)
		Expect(err).NotTo(HaveOccurred())

		Expect(env.fs.Remove("/dev/elemental-install-target")).To(Succeed())
		Expect(env.fs.Symlink("disk/by-id/wwn-0x5000c500000000a1", "/dev/elemental-install-target")).To(Succeed())
		err = env.inst.finalizeInstallDisks(plan)
		Expect(reasonOf(err)).To(Equal(reasonTargetMismatch))
		Expect(err.Error()).To(ContainSubstring("resolved to /dev/sdb (8:16, wwn:5000c500000000b2) and now to /dev/sda (8:0, wwn:5000c500000000a1)"))
	})

	It("refuses to go on when the device number changed", func() {
		env := newDiskTestEnv(host, nil)
		config := elementalv1.Config{Elemental: elementalv1.Elemental{Install: elementalv1.Install{Device: "/dev/sdb"}}}
		plan, err := env.inst.prepareInstallDisks(&config, KubeOSInstaller)
		Expect(err).NotTo(HaveOccurred())

		Expect(env.fs.WriteFile("/sys/class/block/sdb/dev", []byte("8:32\n"), 0o644)).To(Succeed())
		err = env.inst.finalizeInstallDisks(plan)
		Expect(reasonOf(err)).To(Equal(reasonTargetMismatch))
		Expect(err.Error()).To(ContainSubstring("the device node is 8:16 but sysfs says sdb is 8:32"))
	})
})

func req(key, operator string, values ...string) elementalv1.DeviceSelectorRequirement {
	return elementalv1.DeviceSelectorRequirement{
		Key:      elementalv1.DeviceSelectorKey(key),
		Operator: elementalv1.DeviceSelectorOperator(operator),
		Values:   values,
	}
}

var _ = Describe("resolveLinks", Label("installer", "target"), func() {
	It("resolves links in the middle of a path and keeps the chain", func() {
		fs, cleanup, err := vfst.NewTestFS(map[string]any{
			"/dev/sdb":                   "",
			"/dev/disk/by-id/wwn-0xb2":   &vfst.Symlink{Target: "../../sdb"},
			"/dev/alias":                 &vfst.Symlink{Target: "disk"},
			"/dev/elemental-target-link": &vfst.Symlink{Target: "alias/by-id/wwn-0xb2"},
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cleanup)
		resolved, chain, err := resolveLinks(fs, "/dev/elemental-target-link")
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved).To(Equal("/dev/sdb"))
		Expect(chain).To(Equal([]string{
			"/dev/elemental-target-link",
			"/dev/alias/by-id/wwn-0xb2",
			"/dev/disk/by-id/wwn-0xb2",
			"/dev/sdb",
		}))
	})
})
