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
	"os"
	"os/exec"
	"path/filepath"
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
		case strings.Contains(args[1], "PERSIST or ROOT-A"):
			key = "persist"
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
		"persist":   {out: "/etc/elemental/registration/state.yaml\n"},
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
			"/etc/cloud/cloud.cfg.d/99-disable-network-config.cfg",
		} {
			Expect(toml).To(ContainSubstring(fmt.Sprintf("dst = %q\n", dst)), dst)
		}
		Expect(toml).NotTo(ContainSubstring("file://"))
		Expect(toml).NotTo(ContainSubstring("README"))
		Expect(strings.Count(toml, "[[install.configs]]")).To(Equal(11))
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
			"elemental_connection.json":     "-rw-------",
			"nm-01-bond0.nmconnection":      "-rw-------",
			"nm-02-eth0.nmconnection":       "-rw-------",
			"registration-state.yaml":       "-rw-------",
			"99-baremetal-datasource.cfg":   "-rw-r--r--",
			"99-disable-network-config.cfg": "-rw-r--r--",
			"registries-99-kubeos.conf":     "-rw-r--r--",
			"containers-auth.json":          "-rw-------",
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
				switch {
				case strings.Contains(c[2], "kbimg"):
					names = append(names, "kbimg")
				case strings.Contains(c[2], "PERSIST"):
					names = append(names, "persist")
				default:
					names = append(names, "grubcheck")
				}
				continue
			}
			names = append(names, c[0])
		}
		Expect(names).To(Equal([]string{"skopeo", "kbimg", "lsblk", "grubcheck", "persist", "eject", "systemctl"}))
		Expect(runner.calls[0]).To(Equal([]string{"skopeo", "inspect", "--raw", "docker://reg.internal/tkestack/kubeos-rootfs:alaudaos-44.ku.1-0.x86_64"}))
		Expect(runner.calls[1][2]).To(ContainSubstring("yes | kbimg install disk"))
		Expect(runner.calls[1]).To(ContainElement("/tmp/elemental/kubeos/kbimg.toml"))
		Expect(runner.calls[2]).To(Equal([]string{"lsblk", "-rno", "LABEL", "/dev/sda"}))
		Expect(runner.calls[6]).To(Equal([]string{"systemctl", "reboot"}))
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
		Expect(strings.Count(toml, "[[install.configs]]")).To(Equal(9))
	})

	It("copies every first-boot file into PERSIST so the other slot boots with it", func() {
		Expect(inst.InstallKubeOS(config, stateFixture, networkConfigFixture)).To(Succeed())
		var call []string
		for _, c := range runner.calls {
			if c[0] == "sh" && strings.Contains(c[2], "PERSIST") {
				call = c
			}
		}
		Expect(call).NotTo(BeNil())
		Expect(call[3:5]).To(Equal([]string{"persist", "/dev/sda"}))
		pairs := map[string]string{}
		for j := 5; j+1 < len(call); j += 2 {
			pairs[call[j+1]] = call[j]
		}
		Expect(pairs).To(HaveLen(11))
		Expect(pairs).To(HaveKeyWithValue("etc/NetworkManager/system-connections/bond0.nmconnection", "/tmp/elemental/kubeos/files/nm-01-bond0.nmconnection"))
		Expect(pairs).To(HaveKeyWithValue("etc/cloud/cloud.cfg.d/99-disable-network-config.cfg", "/tmp/elemental/kubeos/files/99-disable-network-config.cfg"))
		Expect(pairs).To(HaveKeyWithValue("var/lib/elemental/agent/elemental_connection.json", "/tmp/elemental/kubeos/files/elemental_connection.json"))
		Expect(pairs).To(HaveKeyWithValue("root/.config/containers/auth.json", "/tmp/elemental/kubeos/files/containers-auth.json"))
	})

	It("does not reboot when PERSIST cannot take the first-boot files", func() {
		runner.results["persist"] = fakeResult{out: "PERSIST or ROOT-A not found on /dev/sda", err: errors.New("exit 1")}
		err := inst.InstallKubeOS(config, stateFixture, networkConfigFixture)
		Expect(err).To(MatchError(ContainSubstring("PERSIST")))
		Expect(runner.called("systemctl")).To(BeFalse())
	})

	It("leaves cloud-init networking alone when no profiles were staged", func() {
		Expect(inst.InstallKubeOS(config, stateFixture, elementalv1.NetworkConfig{})).To(Succeed())
		toml := readFile("/tmp/elemental/kubeos/kbimg.toml")
		Expect(toml).NotTo(ContainSubstring("99-disable-network-config.cfg"))
		Expect(toml).NotTo(ContainSubstring("nmconnection"))
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

// The persist script runs for real against scratch directories. lsblk and
// mount are shims: "mounting" swaps the empty mountpoint for a symlink to the
// directory standing in for the partition.
var _ = Describe("kubeos persist script", Label("kubeos"), func() {
	var dir, persist, slot, staged string
	run := func(pairs ...string) (string, error) {
		args := append([]string{"-c", kubeosPersistScript, "persist", "/dev/sda"}, pairs...)
		cmd := exec.Command("sh", args...)
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		// The script closes directories the way ROOT-A has them; reopen them
		// so the temp dir can be removed without root.
		DeferCleanup(func() {
			_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
				if err == nil && info.IsDir() {
					_ = os.Chmod(p, 0o755)
				}
				return nil
			})
		})
		persist, slot, staged = filepath.Join(dir, "persist"), filepath.Join(dir, "slot"), filepath.Join(dir, "staged")
		for _, d := range []string{"bin", "persist/etc", "persist/var", "slot/etc/NetworkManager/system-connections", "slot/root", "staged"} {
			Expect(os.MkdirAll(filepath.Join(dir, d), 0o755)).To(Succeed())
		}
		Expect(os.Chmod(filepath.Join(slot, "root"), 0o550)).To(Succeed())
		Expect(os.Chmod(filepath.Join(slot, "etc/NetworkManager/system-connections"), 0o700)).To(Succeed())
		shims := map[string]string{
			"lsblk":  "#!/bin/sh\nprintf '/dev/sda1 BOOT\\n/dev/sda2 ROOT-A\\n/dev/sda3 ROOT-B\\n/dev/sda4 PERSIST\\n'\n",
			"mount":  fmt.Sprintf("#!/bin/sh\n[ \"$1\" = -o ] && shift 2\ncase $1 in /dev/sda4) src=%s ;; /dev/sda2) src=%s ;; *) exit 1 ;; esac\nrmdir \"$2\" && ln -s \"$src\" \"$2\"\n", persist, slot),
			"umount": "#!/bin/sh\n[ -L \"$1\" ] && rm \"$1\" && mkdir \"$1\"\n",
		}
		for name, body := range shims {
			Expect(os.WriteFile(filepath.Join(dir, "bin", name), []byte(body), 0o755)).To(Succeed())
		}
		Expect(os.WriteFile(filepath.Join(staged, "nm"), []byte("[connection]\nid=eth0\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(staged, "auth"), []byte("{}"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(staged, "cfg"), []byte("network: {config: disabled}\n"), 0o644)).To(Succeed())
	})

	It("lands each file in the upperdir with its mode, and parents take ROOT-A's", func() {
		out, err := run(
			filepath.Join(staged, "nm"), "etc/NetworkManager/system-connections/eth0.nmconnection",
			filepath.Join(staged, "cfg"), "etc/cloud/cloud.cfg.d/99-disable-network-config.cfg",
			filepath.Join(staged, "auth"), "root/.config/containers/auth.json",
		)
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("/etc/NetworkManager/system-connections/eth0.nmconnection\n"))

		mode := func(rel string) string {
			info, err := os.Stat(filepath.Join(persist, rel))
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), rel)
			return info.Mode().Perm().String()
		}
		Expect(mode("etc/NetworkManager/system-connections/eth0.nmconnection")).To(Equal("-rw-------"))
		Expect(mode("etc/cloud/cloud.cfg.d/99-disable-network-config.cfg")).To(Equal("-rw-r--r--"))
		Expect(mode("root/.config/containers/auth.json")).To(Equal("-rw-------"))
		// Copied from ROOT-A where it exists there...
		Expect(mode("etc/NetworkManager/system-connections")).To(Equal("-rwx------"))
		Expect(mode("root")).To(Equal("-r-xr-x---"))
		// ...and left to the umask where it does not.
		Expect(filepath.Join(persist, "etc/cloud/cloud.cfg.d")).To(BeADirectory())
		// Both mountpoints are unmounted and removed again.
		Expect(out).NotTo(ContainSubstring("rmdir"))
	})

	It("fails when the disk has no PERSIST partition", func() {
		Expect(os.WriteFile(filepath.Join(dir, "bin", "lsblk"), []byte("#!/bin/sh\necho '/dev/sda2 ROOT-A'\n"), 0o755)).To(Succeed())
		out, err := run(filepath.Join(staged, "nm"), "etc/x.nmconnection")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("PERSIST or ROOT-A not found"))
	})

	It("fails when a copy fails instead of reporting success", func() {
		out, err := run(filepath.Join(staged, "missing"), "etc/x.nmconnection")
		Expect(err).To(HaveOccurred(), out)
	})
})
