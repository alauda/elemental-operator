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
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rancher/yip/pkg/schema"
	"github.com/twpayne/go-vfs/vfst"
	"go.uber.org/mock/gomock"
	"gopkg.in/yaml.v3"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/elementalcli"
	climocks "github.com/rancher/elemental-operator/pkg/elementalcli/mocks"
	networkmocks "github.com/rancher/elemental-operator/pkg/network/mocks"
	"github.com/rancher/elemental-operator/pkg/register"
)

// The installed system must keep the TPM the live registration resolved:
// tpm-mode auto must not be probed again after install, and emulate-tpm
// must say the same for an older elemental-register after an OS rollback.
var _ = Describe("installed registration config", Label("installer", "install", "tpm-mode"), func() {
	autoRegistration := elementalv1.Registration{URL: "https://example.com", Auth: "tpm", TPMMode: elementalv1.TPMModeAuto, EmulatedTPMSeed: -1}
	emulatedState := register.State{
		InitialRegistration: stateFixture.InitialRegistration,
		EmulatedTPM:         true,
		EmulatedTPMSeed:     7,
		TPMMode:             elementalv1.TPMModeEmulated,
		TPMModeSource:       register.TPMModeSourceProbe,
	}
	hardwareState := register.State{
		InitialRegistration: stateFixture.InitialRegistration,
		TPMMode:             elementalv1.TPMModeHardware,
		TPMModeSource:       register.TPMModeSourceProbe,
	}

	It("records the resolved mode for the elemental toolkit", func() {
		host := twoDisks()
		fs, fsCleanup, err := vfst.NewTestFS(merge(host.files(), map[string]interface{}{"/tmp/init": ""}))
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(fsCleanup)
		mockCtrl := gomock.NewController(GinkgoT())
		cliRunner := climocks.NewMockRunner(mockCtrl)
		networkConfigurator := networkmocks.NewMockConfigurator(mockCtrl)
		inst := &installer{fs: fs, runner: cliRunner, networkConfigurator: networkConfigurator, disk: host.diskOptions()}

		config := configFixture
		config.Elemental.Registration = autoRegistration
		cliRunner.EXPECT().Install(config.Elemental.Install).Return(nil)
		networkConfigurator.EXPECT().GetNetworkConfigApplicator(networkConfigFixture).Return(networkConfigApplicatorFixture, nil)
		Expect(inst.InstallElemental(config, emulatedState, networkConfigFixture)).To(Succeed())

		// before-install hook -> registration yip -> /oem/registration/config.yaml
		data, err := fs.ReadFile(filepath.Join(elementalcli.TempCloudInitDir, elementalBeforeHook))
		Expect(err).ToNot(HaveOccurred())
		hook := schema.YipConfig{}
		Expect(yaml.Unmarshal(data, &hook)).To(Succeed())
		registrationYip := schema.YipConfig{}
		Expect(yaml.Unmarshal([]byte(hook.Stages[beforeInstallStage][0].Files[0].Content), &registrationYip)).To(Succeed())
		Expect(registrationYip.Stages["initramfs"][0].Files[0].Path).To(Equal(registrationConf))
		installed := elementalv1.Config{}
		Expect(yaml.Unmarshal([]byte(registrationYip.Stages["initramfs"][0].Files[0].Content), &installed)).To(Succeed())
		Expect(installed.Elemental.Registration.TPMMode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(installed.Elemental.Registration.EmulateTPM).To(BeTrue())
		Expect(installed.Elemental.Registration.EmulatedTPMSeed).To(Equal(int64(-1)))
	})

	It("records the resolved mode for KubeOS", func() {
		host := kubeosHost()
		fs, fsCleanup, err := vfst.NewTestFS(merge(host.files(), map[string]interface{}{"/root/kbimg.toml": kbimgTemplateFixture}))
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(fsCleanup)
		mockCtrl := gomock.NewController(GinkgoT())
		networkConfigurator := networkmocks.NewMockConfigurator(mockCtrl)
		runner := &fakeKubeOSRunner{results: healthyResults(), host: host}
		disk := host.diskOptions()
		disk.runner, disk.inspect = runner, runner.Output
		inst := &installer{
			fs:                  fs,
			networkConfigurator: networkConfigurator,
			kubeos: kubeosOptions{
				runner:            runner,
				tomlTemplate:      "/root/kbimg.toml",
				workDir:           "/tmp/elemental/kubeos",
				liveRegistriesDir: "/etc/containers/registries.conf.d",
				liveAuthFile:      "/run/containers/0/auth.json",
			},
			disk: disk,
		}
		config := configFixture
		config.CloudConfig = nil
		config.Elemental.Registration = autoRegistration
		config.Elemental.Install = elementalv1.Install{Device: "/dev/sda", SystemURI: "reg.internal/kubeos-rootfs:1"}
		Expect(inst.InstallKubeOS(config, hardwareState, elementalv1.NetworkConfig{})).To(Succeed())

		data, err := fs.ReadFile("/tmp/elemental/kubeos/files/registration-config.yaml")
		Expect(err).ToNot(HaveOccurred())
		installed := elementalv1.Config{}
		Expect(yaml.Unmarshal(data, &installed)).To(Succeed())
		Expect(installed.Elemental.Registration.TPMMode).To(Equal(elementalv1.TPMModeHardware))
		Expect(installed.Elemental.Registration.EmulateTPM).To(BeFalse())
		state, err := fs.ReadFile("/tmp/elemental/kubeos/files/registration-state.yaml")
		Expect(err).ToNot(HaveOccurred())
		Expect(string(state)).To(ContainSubstring("tpmMode: hardware"))
	})
})
