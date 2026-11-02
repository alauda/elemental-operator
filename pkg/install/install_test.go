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
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/twpayne/go-vfs"
	"github.com/twpayne/go-vfs/vfst"
	"go.uber.org/mock/gomock"
	"k8s.io/apimachinery/pkg/runtime"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/controllers"
	"github.com/rancher/elemental-operator/pkg/elementalcli"
	climocks "github.com/rancher/elemental-operator/pkg/elementalcli/mocks"
	"github.com/rancher/elemental-operator/pkg/network"
	networkmocks "github.com/rancher/elemental-operator/pkg/network/mocks"
	"github.com/rancher/elemental-operator/pkg/register"
	"github.com/rancher/yip/pkg/schema"
)

var (
	configFixture = elementalv1.Config{
		Elemental: elementalv1.Elemental{
			Registration: elementalv1.Registration{
				URL:             "https://127.0.0.1.sslip.io/test/registration/endpoint",
				CACert:          "a test ca",
				EmulateTPM:      true,
				EmulatedTPMSeed: 9876543210,
				NoSMBIOS:        true,
				Auth:            "a test auth",
			},
			Install: elementalv1.Install{
				Firmware:         "a test firmware",
				Device:           "/dev/sda",
				NoFormat:         true,
				ConfigURLs:       []string{"foo", "bar"},
				ISO:              "a test iso",
				SystemURI:        "a system uri",
				Debug:            true,
				TTY:              "a test tty",
				PowerOff:         true,
				Reboot:           true,
				EjectCD:          true,
				DisableBootEntry: true,
				ConfigDir:        "a test config dir",
			},
			Reset: elementalv1.Reset{
				Enabled:         true,
				ResetPersistent: false,
				ResetOEM:        false,
				ConfigURLs:      []string{"foo", "bar"},
				SystemURI:       "a system uri",
				PowerOff:        true,
				Reboot:          true,
			},
			SystemAgent: elementalv1.SystemAgent{
				StrictTLSMode:   true,
				URL:             "https://127.0.0.1.sslip.io/test/control/plane/endpoint",
				CACert:          "a system agent ca",
				Token:           "a test token",
				SecretName:      "a test secret name",
				SecretNamespace: "a test namespace",
			},
		},
		CloudConfig: map[string]runtime.RawExtension{
			"users": {
				Raw: []byte(`[{"name":"root","passwd":"root"}]`),
			},
		},
	}
	stateFixture = register.State{
		InitialRegistration: time.Date(2023, time.August, 2, 12, 35, 10, 3, time.UTC),
		EmulatedTPM:         true,
		EmulatedTPMSeed:     987654321,
	}
	networkConfigFixture = elementalv1.NetworkConfig{
		IPAddresses: map[string]string{
			"foo": "192.168.122.10",
			"bar": "192.168.122.11",
		},
		Config: map[string]runtime.RawExtension{
			"foo": {
				Raw: []byte(`"bar"`),
			},
		},
	}
	networkConfigApplicatorFixture = schema.YipConfig{
		Name: "Test Network Config Applicator",
		Stages: map[string][]schema.Stage{
			"foo": {
				{
					Commands: []string{"bar"},
				},
			},
		},
	}
)

func TestInstall(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Install Suite")
}

var _ = Describe("installer install elemental", Label("installer", "install"), func() {
	var fs *vfst.TestFS
	var err error
	var fsCleanup func()
	var cliRunner *climocks.MockRunner
	var networkConfigurator *networkmocks.MockConfigurator
	var host *fakeHost
	var install Installer
	BeforeEach(func() {
		host = twoDisks()
	})
	JustBeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(merge(host.files(), map[string]interface{}{
			"/tmp/init":                     "",
			"/dev/elemental-install-target": &vfst.Symlink{Target: "disk/by-id/wwn-0x5000c500000000b2"},
		}))
		Expect(err).ToNot(HaveOccurred())
		mockCtrl := gomock.NewController(GinkgoT())
		cliRunner = climocks.NewMockRunner(mockCtrl)
		networkConfigurator = networkmocks.NewMockConfigurator(mockCtrl)
		install = &installer{
			fs:                  fs,
			runner:              cliRunner,
			networkConfigurator: networkConfigurator,
			disk:                host.diskOptions(),
		}
		DeferCleanup(fsCleanup)
	})
	It("should call elemental install", func() {
		cliRunner.EXPECT().Install(configFixture.Elemental.Install).Return(nil)
		networkConfigurator.EXPECT().GetNetworkConfigApplicator(networkConfigFixture).Return(networkConfigApplicatorFixture, nil)
		Expect(install.InstallElemental(configFixture, stateFixture, networkConfigFixture)).ToNot(HaveOccurred())
		compareFiles(fs, filepath.Join(elementalcli.TempCloudInitDir, elementalBeforeHook), "_testdata/before-hook-config-install.yaml")
	})
	It("hands the CLI the kernel path of a linked target", func() {
		config := configFixture
		config.Elemental.Install.Device = "/dev/elemental-install-target"
		want := config.Elemental.Install
		want.Device = "/dev/sdb"
		cliRunner.EXPECT().Install(want).Return(nil)
		networkConfigurator.EXPECT().GetNetworkConfigApplicator(networkConfigFixture).Return(networkConfigApplicatorFixture, nil)
		Expect(install.InstallElemental(config, stateFixture, networkConfigFixture)).To(Succeed())
	})
	It("fails before writing anything when the target cannot be resolved", func() {
		config := configFixture
		config.Elemental.Install.Device = "/dev/disk/by-id/wwn-0x5000c5000000ffff"
		err := install.InstallElemental(config, stateFixture, networkConfigFixture)
		Expect(err).To(MatchError(ContainSubstring("TargetNotObserved")))
		_, statErr := fs.Stat(filepath.Join(elementalcli.TempCloudInitDir, elementalBeforeHook))
		Expect(statErr).To(MatchError(os.ErrNotExist))
	})
	Context("in a live environment with a stale system disk", func() {
		BeforeEach(func() {
			host.live = true
			host.disk("sda").elementalLayout()
		})
		It("wipes the stale disk and the target before elemental install runs", func() {
			config := configFixture
			config.Elemental.Install.Device = "/dev/elemental-install-target"
			config.Elemental.Install.NoFormat = false
			want := config.Elemental.Install
			want.Device = "/dev/sdb"
			cliRunner.EXPECT().Install(want).DoAndReturn(func(elementalv1.Install) error {
				commands := destructive(host.calls)
				Expect(commands).To(ContainElements("blockdev --rereadpt /dev/sda", "blockdev --rereadpt /dev/sdb"))
				Expect(commands[len(commands)-1]).To(Equal("udevadm settle"))
				return nil
			})
			networkConfigurator.EXPECT().GetNetworkConfigApplicator(networkConfigFixture).Return(networkConfigApplicatorFixture, nil)
			Expect(install.InstallElemental(config, stateFixture, networkConfigFixture)).To(Succeed())
		})
		It("installs nothing when the stale disk cannot be wiped", func() {
			host.disk("sda").part("xfs", "DATA", "")
			config := configFixture
			config.Elemental.Install.Device = "/dev/sdb"
			err := install.InstallElemental(config, stateFixture, networkConfigFixture)
			Expect(err).To(MatchError(ContainSubstring("Blocked: stale system disks cannot be wiped safely")))
			Expect(err).To(MatchError(ContainSubstring("partition /dev/sda6 (xfs, label DATA)")))
			Expect(destructive(host.calls)).To(BeEmpty())
		})
	})
})

var _ = Describe("installer reset elemental", Label("installer", "reset"), func() {
	var fs *vfst.TestFS
	var err error
	var fsCleanup func()
	var cliRunner *climocks.MockRunner
	var networkConfigurator *networkmocks.MockConfigurator
	var install Installer
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{"/tmp/init": "", "/oem/init": ""})
		Expect(err).ToNot(HaveOccurred())
		mockCtrl := gomock.NewController(GinkgoT())
		cliRunner = climocks.NewMockRunner(mockCtrl)
		networkConfigurator = networkmocks.NewMockConfigurator(mockCtrl)
		install = &installer{
			fs:                  fs,
			runner:              cliRunner,
			networkConfigurator: networkConfigurator,
		}
		DeferCleanup(fsCleanup)
	})
	It("should call elemental reset", func() {
		cliRunner.EXPECT().Reset(configFixture.Elemental.Reset).Return(nil)
		networkConfigurator.EXPECT().GetNetworkConfigApplicator(networkConfigFixture).Return(networkConfigApplicatorFixture, nil)
		Expect(install.ResetElemental(configFixture, stateFixture, networkConfigFixture)).ToNot(HaveOccurred())
		compareFiles(fs, filepath.Join(elementalcli.TempCloudInitDir, elementalBeforeHook), "_testdata/before-hook-config-reset.yaml")
	})
	It("should remove reset plan", func() {
		Expect(fs.WriteFile(controllers.LocalResetPlanPath, []byte("{}\n"), os.FileMode(0600))).ToNot(HaveOccurred())
		cliRunner.EXPECT().Reset(gomock.Any()).Return(nil)
		networkConfigurator.EXPECT().GetNetworkConfigApplicator(elementalv1.NetworkConfig{}).Return(schema.YipConfig{}, network.ErrEmptyConfig)
		Expect(install.ResetElemental(configFixture, stateFixture, elementalv1.NetworkConfig{})).ToNot(HaveOccurred())
		_, err := fs.Stat(controllers.LocalResetPlanPath)
		Expect(err).To(MatchError(os.ErrNotExist))
	})
})

func compareFiles(fs vfs.FS, got string, want string) {
	gotFile, err := fs.ReadFile(got)
	Expect(err).ToNot(HaveOccurred())
	wantFile, err := os.ReadFile(want)
	Expect(err).ToNot(HaveOccurred())
	Expect(string(gotFile)).To(Equal(string(wantFile)))
}
