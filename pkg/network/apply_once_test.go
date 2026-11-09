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

package network

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/yip/pkg/schema"
	"github.com/twpayne/go-vfs"
	"github.com/twpayne/go-vfs/vfst"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	eth0Profile = "[connection]\nid=eth0\n[ipv4]\nmethod=manual\naddress1=192.168.122.10/24\n"
	eth1Profile = "[connection]\nid=Wired connection 2\n[ipv4]\nmethod=auto\n"
)

func nmconnectionsConfig(profiles map[string]string) v1beta1.NetworkConfig {
	config := map[string]runtime.RawExtension{}
	for name, content := range profiles {
		raw, err := json.Marshal(content)
		Expect(err).ToNot(HaveOccurred())
		config[name] = runtime.RawExtension{Raw: raw}
	}
	return v1beta1.NetworkConfig{Configurator: ConfiguratorNmconnections, Config: config}
}

var _ = Describe("applyOnce", Label("network", "apply-once"), func() {
	var networkConfigurator Configurator
	BeforeEach(func() {
		networkConfigurator = &configurator{fs: vfs.OSFS}
	})

	It("guards the profiles with the marker and records them afterwards", func() {
		applicator, err := networkConfigurator.GetNetworkConfigApplicator(nmconnectionsConfig(map[string]string{
			"eth1": eth1Profile,
			"eth0": eth0Profile,
		}))
		Expect(err).ToNot(HaveOccurred())
		Expect(applicator.Stages).To(HaveLen(2))

		steps := applicator.Stages[applicatorStage]
		Expect(steps).To(HaveLen(2))

		apply, record := steps[0], steps[1]
		Expect(apply.Name).To(Equal(applicatorName))
		Expect(apply.Files).To(HaveLen(2))
		Expect(apply.Files[0].Path).To(Equal(filepath.Join(systemConnectionsDir, "eth0.nmconnection")))
		Expect(apply.Files[1].Path).To(Equal(filepath.Join(systemConnectionsDir, "eth1.nmconnection")))
		Expect(apply.If).To(ContainSubstring(appliedMarkerPath))
		Expect(apply.Commands).To(BeEmpty())

		Expect(record.Name).To(Equal(appliedRecorderName))
		Expect(record.If).To(Equal(apply.If))
		Expect(record.Files).To(BeEmpty())
		Expect(record.Commands).To(HaveLen(1))
		Expect(record.Commands[0]).To(ContainSubstring("sha256sum -c"))
		Expect(record.Commands[0]).To(ContainSubstring("> " + appliedMarkerPath))
		Expect(record.Commands[0]).To(ContainSubstring(": > " + appliedThisBootPath))

		relabel := applicator.Stages[relabelStage]
		Expect(relabel).To(HaveLen(1))
		Expect(relabel[0].Name).To(Equal(relabelName))
		Expect(relabel[0].If).To(Equal("[ -f " + appliedThisBootPath + " ]"))
		Expect(relabel[0].Files).To(BeEmpty())
		Expect(relabel[0].Commands).To(HaveLen(1))
		Expect(relabel[0].Commands[0]).To(ContainSubstring("restorecon -i '/etc/NetworkManager/system-connections/eth0.nmconnection' '/etc/NetworkManager/system-connections/eth1.nmconnection' " + appliedMarkerPath))
	})

	It("yields the same applicator for the same profiles", func() {
		profiles := map[string]string{"eth0": eth0Profile, "eth1": eth1Profile, "bond0": "[connection]\nid=bond0\n"}
		first, err := networkConfigurator.GetNetworkConfigApplicator(nmconnectionsConfig(profiles))
		Expect(err).ToNot(HaveOccurred())
		for i := 0; i < 10; i++ {
			again, err := networkConfigurator.GetNetworkConfigApplicator(nmconnectionsConfig(profiles))
			Expect(err).ToNot(HaveOccurred())
			Expect(again).To(Equal(first))
		}
	})

	It("stamps different profiles differently", func() {
		first, err := networkConfigurator.GetNetworkConfigApplicator(nmconnectionsConfig(map[string]string{"eth0": eth0Profile}))
		Expect(err).ToNot(HaveOccurred())
		second, err := networkConfigurator.GetNetworkConfigApplicator(nmconnectionsConfig(map[string]string{"eth0": eth1Profile}))
		Expect(err).ToNot(HaveOccurred())
		Expect(second.Stages[applicatorStage][0].If).ToNot(Equal(first.Stages[applicatorStage][0].If))
	})

	It("leaves an applicator without profiles alone", func() {
		empty := schema.YipConfig{Name: applicatorName, Stages: map[string][]schema.Stage{applicatorStage: {{}}}}
		Expect(applyOnce(empty)).To(Equal(empty))
	})

	It("leaves encoded profiles to be written on every boot", func() {
		encoded := schema.YipConfig{Name: applicatorName, Stages: map[string][]schema.Stage{applicatorStage: {{
			Files: []schema.File{{Path: filepath.Join(systemConnectionsDir, "eth0.nmconnection"), Content: "Zm9v", Encoding: "b64"}},
		}}}}
		Expect(applyOnce(encoded)).To(Equal(encoded))
	})

	// The boots below run the If guards and the commands through a real shell,
	// the way yip does: stage by stage, per step the If first, then the files,
	// then the commands, steps in order. The paths the applicator touches are
	// moved under a temporary root, and restorecon is a stub that records its
	// arguments, so the relabel step runs as on an SELinux host.
	Context("across boots", func() {
		var root string
		var applicator schema.YipConfig

		profilePath := func(name string) string {
			return filepath.Join(root, systemConnectionsDir, name+".nmconnection")
		}
		markerPath := func() string { return filepath.Join(root, appliedMarkerPath) }
		relabelledPath := func() string { return filepath.Join(root, "relabelled") }
		rehome := func(s string) string {
			for _, dir := range []string{"/etc/NetworkManager", "/run/elemental", "/sys/fs/selinux"} {
				s = strings.ReplaceAll(s, dir, filepath.Join(root, dir))
			}
			return s
		}
		sh := func(script string) error {
			cmd := exec.Command("sh", "-c", rehome(script))
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"))
			return cmd.Run()
		}
		runStage := func(stage string) {
			for _, step := range applicator.Stages[stage] {
				if step.If != "" && sh(step.If) != nil {
					continue
				}
				for _, f := range step.Files {
					path := rehome(f.Path)
					Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
					Expect(os.WriteFile(path, []byte(f.Content), os.FileMode(f.Permissions))).To(Succeed())
				}
				for _, command := range step.Commands {
					Expect(sh(command)).To(Succeed())
				}
			}
		}
		boot := func() {
			// /run starts empty on every boot.
			Expect(os.RemoveAll(filepath.Join(root, "run"))).To(Succeed())
			Expect(os.RemoveAll(relabelledPath())).To(Succeed())
			runStage(applicatorStage)
			runStage(relabelStage)
		}
		readProfile := func(name string) string {
			content, err := os.ReadFile(profilePath(name))
			Expect(err).ToNot(HaveOccurred())
			return string(content)
		}
		relabelled := func() []string {
			content, err := os.ReadFile(relabelledPath())
			if os.IsNotExist(err) {
				return nil
			}
			Expect(err).ToNot(HaveOccurred())
			return strings.Fields(string(content))
		}

		BeforeEach(func() {
			for _, tool := range []string{"sh", "sha256sum", "cat"} {
				if _, err := exec.LookPath(tool); err != nil {
					Skip(tool + " is not available")
				}
			}
			root = GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(root, "sys/fs/selinux"), 0o755)).To(Succeed())
			Expect(os.MkdirAll(filepath.Join(root, "bin"), 0o755)).To(Succeed())
			stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" | tr ' ' '_' >> " + relabelledPath() + "; done\n"
			Expect(os.WriteFile(filepath.Join(root, "bin", "restorecon"), []byte(stub), 0o755)).To(Succeed())
			var err error
			applicator, err = networkConfigurator.GetNetworkConfigApplicator(nmconnectionsConfig(map[string]string{
				"eth0":               eth0Profile,
				"Wired connection 2": eth1Profile,
			}))
			Expect(err).ToNot(HaveOccurred())
		})

		It("applies the profiles on the first boot and keeps later changes", func() {
			boot()
			Expect(readProfile("eth0")).To(Equal(eth0Profile))
			Expect(readProfile("Wired connection 2")).To(Equal(eth1Profile))
			Expect(markerPath()).To(BeARegularFile())
			Expect(relabelled()).To(Equal([]string{
				"-i",
				rehome(filepath.Join(systemConnectionsDir, "Wired_connection_2.nmconnection")),
				rehome(filepath.Join(systemConnectionsDir, "eth0.nmconnection")),
				rehome(appliedMarkerPath),
			}))

			// nmcli changes one profile and deletes the other.
			changed := strings.Replace(eth0Profile, "192.168.122.10", "192.168.122.20", 1)
			Expect(os.WriteFile(profilePath("eth0"), []byte(changed), 0o600)).To(Succeed())
			Expect(os.Remove(profilePath("Wired connection 2"))).To(Succeed())

			boot()
			Expect(readProfile("eth0")).To(Equal(changed))
			Expect(profilePath("Wired connection 2")).ToNot(BeAnExistingFile())
			Expect(relabelled()).To(BeEmpty(), "nothing was written, nothing to relabel")
		})

		It("restores the profiles once the marker is removed", func() {
			boot()
			Expect(os.WriteFile(profilePath("eth0"), []byte("changed"), 0o600)).To(Succeed())
			Expect(os.Remove(profilePath("Wired connection 2"))).To(Succeed())
			Expect(os.Remove(markerPath())).To(Succeed())

			boot()
			Expect(readProfile("eth0")).To(Equal(eth0Profile))
			Expect(readProfile("Wired connection 2")).To(Equal(eth1Profile))
			Expect(markerPath()).To(BeARegularFile())
			Expect(relabelled()).ToNot(BeEmpty(), "restored profiles are relabelled")
		})

		It("applies a different set of profiles over an older marker", func() {
			boot()
			Expect(os.WriteFile(profilePath("eth0"), []byte("changed"), 0o600)).To(Succeed())

			var err error
			applicator, err = networkConfigurator.GetNetworkConfigApplicator(nmconnectionsConfig(map[string]string{
				"eth0": eth1Profile,
			}))
			Expect(err).ToNot(HaveOccurred())
			boot()
			Expect(readProfile("eth0")).To(Equal(eth1Profile))
		})

		It("does not record profiles that are not on disk as applied", func() {
			apply, record := applicator.Stages[applicatorStage][0], applicator.Stages[applicatorStage][1]
			Expect(sh(apply.If)).To(Succeed())
			// The first profile was written, the second was not.
			Expect(os.MkdirAll(filepath.Join(root, systemConnectionsDir), 0o755)).To(Succeed())
			Expect(os.WriteFile(profilePath("eth0"), []byte(eth0Profile), 0o600)).To(Succeed())

			Expect(sh(record.Commands[0])).ToNot(Succeed())
			Expect(markerPath()).ToNot(BeAnExistingFile())
			Expect(filepath.Join(root, appliedThisBootPath)).ToNot(BeAnExistingFile())

			// The next boot tries again.
			boot()
			Expect(readProfile("Wired connection 2")).To(Equal(eth1Profile))
			Expect(markerPath()).To(BeARegularFile())
		})
	})
})

var _ = Describe("ResetNetworkConfig", Label("network", "apply-once"), func() {
	It("forgets the applied profiles even when none is left", func() {
		fs, cleanup, err := vfst.NewTestFS(map[string]interface{}{
			appliedMarkerPath:    "stamp\n",
			systemConnectionsDir: &vfst.Dir{Perm: 0o700},
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(cleanup)

		networkConfigurator := &configurator{fs: fs}
		Expect(networkConfigurator.ResetNetworkConfig()).To(Succeed())
		_, err = fs.Stat(appliedMarkerPath)
		Expect(os.IsNotExist(err)).To(BeTrue(), "the marker must be removed")
	})
})
