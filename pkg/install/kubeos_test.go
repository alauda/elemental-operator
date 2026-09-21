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
	"strings"

	"github.com/jaypipes/ghw/pkg/block"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rancher/yip/pkg/schema"
	"github.com/twpayne/go-vfs"
	"github.com/twpayne/go-vfs/vfst"
	"go.uber.org/mock/gomock"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	networkmocks "github.com/rancher/elemental-operator/pkg/network/mocks"
)

// fakeKubeOSRunner records every command and answers from a script of
// canned results keyed by the executable name.
type fakeKubeOSRunner struct {
	calls   [][]string
	results map[string]fakeResult
}

type fakeResult struct {
	out string
	err error
}

func (f *fakeKubeOSRunner) answer(name string, args []string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	key := name
	// The grub check and the install both go through sh -c; tell them apart by
	// the script's first line.
	if name == "sh" && len(args) >= 2 {
		switch {
		case strings.Contains(args[1], "kbimg install disk"):
			key = "kbimg"
		case strings.Contains(args[1], "root="):
			key = "grubcheck"
		}
	}
	r, ok := f.results[key]
	if !ok {
		return nil, fmt.Errorf("fake runner: no result scripted for %s", key)
	}
	return []byte(r.out), r.err
}

func (f *fakeKubeOSRunner) Output(name string, args ...string) ([]byte, error) {
	return f.answer(name, args)
}

func (f *fakeKubeOSRunner) Stream(name string, args ...string) ([]byte, error) {
	return f.answer(name, args)
}

func (f *fakeKubeOSRunner) called(name string) bool {
	for _, c := range f.calls {
		if c[0] == name || (c[0] == "sh" && len(c) > 2 && strings.Contains(c[2], name)) {
			return true
		}
	}
	return false
}

const kbimgTemplateFixture = `# shipped by os-build
[from_repo]
agent_path = "/opt/kubeOS/scripts/os-agent"
legacy_bios = false

[install]
target_disk = "/dev/vda"
oci_image = "docker://build.example/kubeos:baremetal"
reboot = true
skip_tls = true

[disk_partition]
img_size = 60
root = 20000
`

func healthyResults() map[string]fakeResult {
	return map[string]fakeResult{
		"skopeo":    {out: `{"schemaVersion":2}`},
		"kbimg":     {out: "pulling rootfs...\nKubeOS installed successfully\n"},
		"lsblk":     {out: "\nBOOT\nROOT-A\nROOT-B\nPERSIST\n"},
		"grubcheck": {out: "root=PARTUUID=1111-2222\n"},
		"eject":     {out: ""},
		"systemctl": {out: ""},
	}
}

var _ = Describe("installer install kubeos", Label("installer", "install", "kubeos"), func() {
	var fs *vfst.TestFS
	var fsCleanup func()
	var runner *fakeKubeOSRunner
	var networkConfigurator *networkmocks.MockConfigurator
	var inst *installer
	var config elementalv1.Config

	BeforeEach(func() {
		var err error
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{
			"/root/kbimg.toml": kbimgTemplateFixture,
			"/etc/containers/registries.conf.d/99-kubeos.conf": "[[registry]]\nlocation = \"reg.internal\"\ninsecure = true\n",
			"/etc/containers/registries.conf.d/README":         "not a conf",
			"/run/containers/0/auth.json":                      `{"auths":{"reg.internal":{"auth":"dXNlcjpwYXNz"}}}`,
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(fsCleanup)
		mockCtrl := gomock.NewController(GinkgoT())
		networkConfigurator = networkmocks.NewMockConfigurator(mockCtrl)
		runner = &fakeKubeOSRunner{results: healthyResults()}
		inst = &installer{
			fs:                  fs,
			networkConfigurator: networkConfigurator,
			kubeos: kubeosOptions{
				runner:            runner,
				tomlTemplate:      "/root/kbimg.toml",
				workDir:           "/tmp/elemental/kubeos",
				liveRegistriesDir: "/etc/containers/registries.conf.d",
				liveAuthFile:      "/run/containers/0/auth.json",
			},
		}
		config = configFixture
		config.Elemental.Install = elementalv1.Install{
			Device:    "/dev/sda",
			SystemURI: "reg.internal/tkestack/kubeos-rootfs:alaudaos-44.ku.1-0.x86_64",
			Reboot:    true,
			EjectCD:   true,
		}
		config.CloudConfig = nil
		networkConfigurator.EXPECT().GetNetworkConfigApplicator(networkConfigFixture).Return(schema.YipConfig{
			Stages: map[string][]schema.Stage{"initramfs": {{Files: []schema.File{
				{Path: "/etc/NetworkManager/system-connections/bond0.nmconnection", Permissions: 0600, Content: "[connection]\nid=bond0\n"},
				{Path: "/etc/NetworkManager/system-connections/eth0.nmconnection", Permissions: 0600, Content: "[connection]\nid=eth0\nmaster=bond0\n"},
			}}}},
		}, nil).AnyTimes()
	})

	readFile := func(path string) string {
		b, err := fs.ReadFile(path)
		ExpectWithOffset(1, err).ToNot(HaveOccurred(), path)
		return string(b)
	}

	It("rewrites only the three install keys and appends the staged configs", func() {
		Expect(inst.InstallKubeOS(config, stateFixture, networkConfigFixture)).To(Succeed())

		toml := readFile("/tmp/elemental/kubeos/kbimg.toml")
		Expect(toml).To(ContainSubstring(`target_disk = "/dev/sda"`))
		Expect(toml).To(ContainSubstring(`oci_image = "docker://reg.internal/tkestack/kubeos-rootfs:alaudaos-44.ku.1-0.x86_64"`))
		Expect(toml).To(ContainSubstring("reboot = false\n"))
		// Untouched lines survive byte for byte.
		Expect(toml).To(ContainSubstring("# shipped by os-build\n"))
		Expect(toml).To(ContainSubstring("skip_tls = true\n"))
		Expect(toml).To(ContainSubstring("[disk_partition]\nimg_size = 60\nroot = 20000\n"))
		Expect(toml).NotTo(ContainSubstring("reboot = true"))
		Expect(toml).NotTo(ContainSubstring("/dev/vda"))

		// Every first-boot file is a full dst path with a bare src.
		for _, dst := range []string{
			"/var/lib/elemental/agent/elemental_connection.json",
			"/etc/rancher/elemental/agent/config.yaml",
			"/etc/rancher/elemental/agent/envs",
			"/etc/elemental/registration/config.yaml",
			"/etc/elemental/registration/state.yaml",
			"/etc/cloud/cloud.cfg.d/99-baremetal-datasource.cfg",
			"/etc/NetworkManager/system-connections/bond0.nmconnection",
			"/etc/NetworkManager/system-connections/eth0.nmconnection",
			"/etc/containers/registries.conf.d/99-kubeos.conf",
			"/root/.config/containers/auth.json",
		} {
			Expect(toml).To(ContainSubstring(fmt.Sprintf("dst = %q\n", dst)), dst)
		}
		Expect(toml).NotTo(ContainSubstring("file://"))
		Expect(toml).NotTo(ContainSubstring("README"))
		Expect(strings.Count(toml, "[[install.configs]]")).To(Equal(10))
	})

	It("stages first-boot files with the modes the target needs", func() {
		Expect(inst.InstallKubeOS(config, stateFixture, networkConfigFixture)).To(Succeed())

		Expect(readFile("/tmp/elemental/kubeos/files/99-baremetal-datasource.cfg")).To(Equal("datasource_list: [ NoCloud ]\n"))
		Expect(readFile("/tmp/elemental/kubeos/files/registration-state.yaml")).To(ContainSubstring("emulatedTPMSeed: 987654321"))
		Expect(readFile("/tmp/elemental/kubeos/files/registration-config.yaml")).To(ContainSubstring("url: https://127.0.0.1.sslip.io/test/registration/endpoint"))
		// Bare, not yip-wrapped.
		Expect(readFile("/tmp/elemental/kubeos/files/registration-config.yaml")).NotTo(ContainSubstring("stages:"))
		Expect(readFile("/tmp/elemental/kubeos/files/elemental_connection.json")).To(ContainSubstring("a test token"))
		Expect(readFile("/tmp/elemental/kubeos/files/nm-01-bond0.nmconnection")).To(Equal("[connection]\nid=bond0\n"))

		for name, mode := range map[string]string{
			"elemental_connection.json":   "-rw-------",
			"nm-01-bond0.nmconnection":    "-rw-------",
			"nm-02-eth0.nmconnection":     "-rw-------",
			"registration-state.yaml":     "-rw-------",
			"99-baremetal-datasource.cfg": "-rw-r--r--",
			"registries-99-kubeos.conf":   "-rw-r--r--",
			"containers-auth.json":        "-rw-------",
		} {
			info, err := fs.Stat("/tmp/elemental/kubeos/files/" + name)
			Expect(err).ToNot(HaveOccurred(), name)
			Expect(info.Mode().String()).To(Equal(mode), name)
		}
	})

	It("runs preflight, install, verification and the requested power action in order", func() {
		Expect(inst.InstallKubeOS(config, stateFixture, networkConfigFixture)).To(Succeed())
		names := make([]string, 0, len(runner.calls))
		for _, c := range runner.calls {
			if c[0] == "sh" {
				if strings.Contains(c[2], "kbimg") {
					names = append(names, "kbimg")
				} else {
					names = append(names, "grubcheck")
				}
				continue
			}
			names = append(names, c[0])
		}
		Expect(names).To(Equal([]string{"skopeo", "kbimg", "lsblk", "grubcheck", "eject", "systemctl"}))
		Expect(runner.calls[0]).To(Equal([]string{"skopeo", "inspect", "--raw", "docker://reg.internal/tkestack/kubeos-rootfs:alaudaos-44.ku.1-0.x86_64"}))
		Expect(runner.calls[1][2]).To(ContainSubstring("yes | kbimg install disk"))
		Expect(runner.calls[1]).To(ContainElement("/tmp/elemental/kubeos/kbimg.toml"))
		Expect(runner.calls[2]).To(Equal([]string{"lsblk", "-rno", "LABEL", "/dev/sda"}))
		Expect(runner.calls[5]).To(Equal([]string{"systemctl", "reboot"}))
	})

	It("fails before touching the disk when the image is unreachable", func() {
		runner.results["skopeo"] = fakeResult{out: "unauthorized", err: errors.New("exit 1")}
		err := inst.InstallKubeOS(config, stateFixture, networkConfigFixture)
		Expect(err).To(MatchError(ContainSubstring("not reachable")))
		Expect(runner.called("kbimg")).To(BeFalse())
	})

	It("treats a zero exit without the success line as a failure and does not reboot", func() {
		runner.results["kbimg"] = fakeResult{out: "ERROR kbimg: rootfs.tar: Cannot open\n"}
		err := inst.InstallKubeOS(config, stateFixture, networkConfigFixture)
		Expect(err).To(MatchError(ContainSubstring("never printed")))
		Expect(runner.called("systemctl")).To(BeFalse())
	})

	It("refuses a disk that lacks the A/B layout", func() {
		runner.results["lsblk"] = fakeResult{out: "BOOT\nROOT-A\n"}
		err := inst.InstallKubeOS(config, stateFixture, networkConfigFixture)
		Expect(err).To(MatchError(ContainSubstring("ROOT-B")))
		Expect(runner.called("systemctl")).To(BeFalse())
	})

	It("refuses a grub.cfg that still names a device instead of PARTUUID", func() {
		runner.results["grubcheck"] = fakeResult{out: "root=/dev/vda2\n"}
		err := inst.InstallKubeOS(config, stateFixture, networkConfigFixture)
		Expect(err).To(MatchError(ContainSubstring("root=PARTUUID=")))
		Expect(runner.called("systemctl")).To(BeFalse())
	})

	It("requires system-uri", func() {
		config.Elemental.Install.SystemURI = ""
		Expect(inst.InstallKubeOS(config, stateFixture, networkConfigFixture)).To(MatchError(ContainSubstring("system-uri")))
		Expect(runner.calls).To(BeEmpty())
	})

	It("uses the device selector when no device is given", func() {
		config.Elemental.Install.Device = ""
		config.Elemental.Install.DeviceSelector = elementalv1.DeviceSelector{{
			Key: elementalv1.DeviceSelectorKeyName, Operator: elementalv1.DeviceSelectorOpIn, Values: []string{"/dev/nvme0n1"},
		}}
		inst.disks = []*block.Disk{{Name: "nvme0n1", SizeBytes: 500 * 1024 * 1024 * 1024}}
		Expect(inst.InstallKubeOS(config, stateFixture, networkConfigFixture)).To(Succeed())
		Expect(readFile("/tmp/elemental/kubeos/kbimg.toml")).To(ContainSubstring(`target_disk = "/dev/nvme0n1"`))
	})

	It("powers off instead of rebooting when asked, and does nothing when neither", func() {
		config.Elemental.Install.PowerOff = true
		Expect(inst.InstallKubeOS(config, stateFixture, networkConfigFixture)).To(Succeed())
		Expect(runner.calls[len(runner.calls)-1]).To(Equal([]string{"systemctl", "poweroff"}))

		runner.calls = nil
		config.Elemental.Install.PowerOff = false
		config.Elemental.Install.Reboot = false
		config.Elemental.Install.EjectCD = false
		Expect(inst.InstallKubeOS(config, stateFixture, networkConfigFixture)).To(Succeed())
		Expect(runner.called("systemctl")).To(BeFalse())
		Expect(runner.called("eject")).To(BeFalse())
	})

	It("skips optional live files that do not exist", func() {
		Expect(fs.RemoveAll("/etc/containers/registries.conf.d")).To(Succeed())
		Expect(fs.Remove("/run/containers/0/auth.json")).To(Succeed())
		Expect(inst.InstallKubeOS(config, stateFixture, networkConfigFixture)).To(Succeed())
		toml := readFile("/tmp/elemental/kubeos/kbimg.toml")
		Expect(toml).NotTo(ContainSubstring("registries.conf.d"))
		Expect(toml).NotTo(ContainSubstring("auth.json"))
		Expect(strings.Count(toml, "[[install.configs]]")).To(Equal(8))
	})

	It("fails loudly on a template without the expected keys", func() {
		Expect(fs.WriteFile("/root/kbimg.toml", []byte("[install]\nsomething = 1\n"), 0o600)).To(Succeed())
		err := inst.InstallKubeOS(config, stateFixture, networkConfigFixture)
		Expect(err).To(MatchError(ContainSubstring("target_disk or oci_image")))
		Expect(runner.calls).To(BeEmpty())
	})
})

var _ = Describe("kubeos image reference", func() {
	It("normalises to docker://", func() {
		Expect(kubeosImageReference("reg/x:y")).To(Equal("docker://reg/x:y"))
		Expect(kubeosImageReference("docker://reg/x:y")).To(Equal("docker://reg/x:y"))
		Expect(kubeosImageReference("docker:reg/x:y")).To(Equal("docker://reg/x:y"))
		Expect(kubeosImageReference("  reg/x:y\n")).To(Equal("docker://reg/x:y"))
	})
})

var _ = Describe("rewriteKubeOSToml", func() {
	It("keeps indentation and comments on untouched lines", func() {
		in := []byte("[install]\n  target_disk = \"/dev/vda\" # build vm\n  oci_image = \"x\"\n  reboot = true\n  keep = 1\n")
		out, err := rewriteKubeOSToml(in, "/dev/sda", "docker://y")
		Expect(err).ToNot(HaveOccurred())
		Expect(string(out)).To(Equal("[install]\ntarget_disk = \"/dev/sda\"\noci_image = \"docker://y\"\nreboot = false\n  keep = 1\n"))
	})
	It("needs a reboot key to override", func() {
		_, err := rewriteKubeOSToml([]byte("target_disk = \"a\"\noci_image = \"b\"\n"), "/dev/sda", "docker://y")
		Expect(err).To(MatchError(ContainSubstring("reboot")))
	})
})

// Keep the vfs import used even if the FS helpers above change shape.
var _ = vfs.OSFS
