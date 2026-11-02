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

package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
	"github.com/twpayne/go-vfs"
	"github.com/twpayne/go-vfs/vfst"
	"go.uber.org/mock/gomock"
	"gopkg.in/yaml.v3"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/install"
	imocks "github.com/rancher/elemental-operator/pkg/install/mocks"
	"github.com/rancher/elemental-operator/pkg/register"
	rmocks "github.com/rancher/elemental-operator/pkg/register/mocks"
	"github.com/rancher/elemental-operator/pkg/version"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestRegister(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Register CLI Suite")
}

var (
	baseConfigFixture = elementalv1.Config{
		Network: elementalv1.NetworkTemplate{},
		Elemental: elementalv1.Elemental{
			Registration: elementalv1.Registration{
				URL:             "https://127.0.0.1.sslip.io",
				CACert:          "Just for testing",
				EmulateTPM:      true,
				EmulatedTPMSeed: -1,
				NoSMBIOS:        true,
				Auth:            "test",
			},
		},
	}
	notoolkitConfigFixture = elementalv1.Config{
		Network: elementalv1.NetworkTemplate{},
		Elemental: elementalv1.Elemental{
			Registration: elementalv1.Registration{
				URL:             "https://127.0.0.1.sslip.io",
				CACert:          "Just for testing",
				EmulateTPM:      true,
				EmulatedTPMSeed: -1,
				NoSMBIOS:        true,
				Auth:            "test",
				NoToolkit:       true,
			},
		},
	}
	alternateConfigFixture = elementalv1.Config{
		Network: elementalv1.NetworkTemplate{},
		Elemental: elementalv1.Elemental{
			Registration: elementalv1.Registration{
				URL:             "https://127.0.0.2.sslip.io",
				CACert:          "alternate ca",
				EmulateTPM:      true,
				EmulatedTPMSeed: 9876543210,
				NoSMBIOS:        true,
				Auth:            "alternate auth",
			},
			Install: elementalv1.Install{
				Firmware:         "a test firmware",
				Device:           "a test device",
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
				Enabled:          true,
				ResetPersistent:  false,
				ResetOEM:         false,
				ConfigURLs:       []string{"foo", "bar"},
				SystemURI:        "a system uri",
				PowerOff:         true,
				Reboot:           true,
				DisableBootEntry: true,
			},
		},
	}
	stateFixture = register.State{
		InitialRegistration: time.Now(),
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
				Raw: []byte(`"{foo}"`),
			},
			"bar": {
				Raw: []byte(`"{bar}"`),
			},
		},
	}
)

var _ = Describe("elemental-register", Label("registration", "cli"), func() {
	var fs vfs.FS
	var err error
	var fsCleanup func()
	var cmd *cobra.Command
	var mockCtrl *gomock.Controller
	var client *rmocks.MockClient
	var installer *imocks.MockInstaller
	var stateHandler *rmocks.MockStateHandler
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		mockCtrl = gomock.NewController(GinkgoT())
		client = rmocks.NewMockClient(mockCtrl)
		installer = imocks.NewMockInstaller(mockCtrl)
		stateHandler = rmocks.NewMockStateHandler(mockCtrl)
		cmd = newCommand(fs, client, stateHandler, installer)
		DeferCleanup(fsCleanup)
	})
	It("should return no error when printing version", func() {
		cmd.SetArgs([]string{"--version"})
		Expect(cmd.Execute()).ToNot(HaveOccurred())
	})
	When("using existing default config", func() {
		BeforeEach(func() {
			marshalIntoFile(fs, baseConfigFixture, defaultConfigPath)
			stateHandler.EXPECT().Init(defaultStatePath).Return(nil)
			stateHandler.EXPECT().Load().Return(stateFixture, nil)
			stateHandler.EXPECT().Save(stateFixture).Return(nil)
		})
		It("should use the config if no arguments passed", func() {
			cmd.SetArgs([]string{})
			client.EXPECT().
				Register(baseConfigFixture.Elemental.Registration, []byte(baseConfigFixture.Elemental.Registration.CACert), &stateFixture).
				Return(marshalToBytes(baseConfigFixture), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
		It("should overwrite the config values with passed arguments", func() {
			cmd.SetArgs([]string{
				"--registration-url", alternateConfigFixture.Elemental.Registration.URL,
				"--registration-ca-cert", alternateConfigFixture.Elemental.Registration.CACert,
				"--emulate-tpm",
				"--emulated-tpm-seed", fmt.Sprintf("%d", alternateConfigFixture.Elemental.Registration.EmulatedTPMSeed),
				"--no-smbios=false",
				"--auth", alternateConfigFixture.Elemental.Registration.Auth,
			})
			wantConfig := alternateConfigFixture.DeepCopy()
			wantConfig.Elemental.Registration.NoSMBIOS = false
			client.EXPECT().
				Register(wantConfig.Elemental.Registration, []byte(wantConfig.Elemental.Registration.CACert), &stateFixture).
				Return(marshalToBytes(wantConfig), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
		It("should use config path argument", func() {
			newPath := "/a/custom/config/path/custom-config.yaml"
			cmd.SetArgs([]string{"--config-path", newPath})
			marshalIntoFile(fs, alternateConfigFixture, newPath)
			client.EXPECT().
				Register(alternateConfigFixture.Elemental.Registration, []byte(alternateConfigFixture.Elemental.Registration.CACert), &stateFixture).
				Return(marshalToBytes(alternateConfigFixture), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
		It("derives the emulated TPM seed when the config sets none", func() {
			// MachineRegistrations that only say emulate-tpm: true used to give
			// every host seed 1, i.e. one shared identity.
			newPath := "/a/custom/config/path/no-seed.yaml"
			noSeed := baseConfigFixture.DeepCopy()
			noSeed.Elemental.Registration.EmulatedTPMSeed = 0 // omitted from the file
			marshalIntoFile(fs, noSeed, newPath)
			cmd.SetArgs([]string{"--config-path", newPath})
			want := noSeed.Elemental.Registration
			want.EmulatedTPMSeed = elementalv1.TPMRandomSeedValue
			client.EXPECT().
				Register(want, []byte(want.CACert), &stateFixture).
				Return(marshalToBytes(noSeed), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
		It("reads tpm-mode from the config and from --tpm-mode", func() {
			newPath := "/a/custom/config/path/tpm-mode.yaml"
			auto := baseConfigFixture.DeepCopy()
			auto.Elemental.Registration.EmulateTPM = false
			auto.Elemental.Registration.TPMMode = elementalv1.TPMModeAuto
			marshalIntoFile(fs, auto, newPath)
			cmd.SetArgs([]string{"--config-path", newPath})
			client.EXPECT().
				Register(auto.Elemental.Registration, []byte(auto.Elemental.Registration.CACert), &stateFixture).
				Return(marshalToBytes(auto), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
		It("lets --tpm-mode override the config", func() {
			cmd.SetArgs([]string{"--tpm-mode", elementalv1.TPMModeEmulated})
			want := baseConfigFixture.Elemental.Registration
			want.TPMMode = elementalv1.TPMModeEmulated
			client.EXPECT().
				Register(want, []byte(want.CACert), &stateFixture).
				Return(marshalToBytes(baseConfigFixture), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
	})
})

var _ = Describe("elemental-register state", Label("registration", "cli", "state"), func() {
	var fs vfs.FS
	var err error
	var fsCleanup func()
	var cmd *cobra.Command
	var mockCtrl *gomock.Controller
	var client *rmocks.MockClient
	var installer *imocks.MockInstaller
	var stateHandler *rmocks.MockStateHandler
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		mockCtrl = gomock.NewController(GinkgoT())
		client = rmocks.NewMockClient(mockCtrl)
		installer = imocks.NewMockInstaller(mockCtrl)
		stateHandler = rmocks.NewMockStateHandler(mockCtrl)
		cmd = newCommand(fs, client, stateHandler, installer)
		DeferCleanup(fsCleanup)
	})
	When("using existing default config", func() {
		BeforeEach(func() {
			marshalIntoFile(fs, baseConfigFixture, defaultConfigPath)
		})
		It("should use state path argument", func() {
			newPath := "/a/custom/state/path/custom-state.yaml"
			cmd.SetArgs([]string{"--state-path", newPath})
			registrationState := register.State{
				InitialRegistration: time.Now(),
			}
			stateHandler.EXPECT().Init(newPath).Return(nil)
			stateHandler.EXPECT().Load().Return(registrationState, nil)
			stateHandler.EXPECT().Save(registrationState).Return(nil)
			client.EXPECT().
				Register(baseConfigFixture.Elemental.Registration, []byte(baseConfigFixture.Elemental.Registration.CACert), &registrationState).
				Return(marshalToBytes(baseConfigFixture), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
		It("should return the registration data", func() {
			cmd.SetArgs([]string{})
			registrationState := register.State{
				InitialRegistration: time.Now(),
			}
			stateHandler.EXPECT().Init(defaultStatePath).Return(nil)
			stateHandler.EXPECT().Load().Return(registrationState, nil)
			stateHandler.EXPECT().Save(registrationState).Return(nil)
			client.EXPECT().
				Register(baseConfigFixture.Elemental.Registration, []byte(baseConfigFixture.Elemental.Registration.CACert), &registrationState).
				Return(marshalToBytes(baseConfigFixture), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
	})
})

var _ = Describe("elemental-register --install", Label("registration", "cli", "install"), func() {
	var fs vfs.FS
	var err error
	var fsCleanup func()
	var cmd *cobra.Command
	var mockCtrl *gomock.Controller
	var client *rmocks.MockClient
	var installer *imocks.MockInstaller
	var stateHandler *rmocks.MockStateHandler
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		mockCtrl = gomock.NewController(GinkgoT())
		installer = imocks.NewMockInstaller(mockCtrl)
		stateHandler = rmocks.NewMockStateHandler(mockCtrl)
		client = rmocks.NewMockClient(mockCtrl)
		cmd = newCommand(fs, client, stateHandler, installer)
		DeferCleanup(fsCleanup)
		allowCompletionReports(client)
		fakePowerActions()
	})
	When("using existing live config", func() {
		BeforeEach(func() {
			marshalIntoFile(fs, baseConfigFixture, defaultLiveConfigPath)
			stateHandler.EXPECT().Init(defaultLiveStatePath).Return(nil)
			stateHandler.EXPECT().Load().Return(stateFixture, nil)
			stateHandler.EXPECT().Save(stateFixture).Return(nil)
		})
		It("should trigger install when --install argument", func() {
			Expect(err).ShouldNot(HaveOccurred())

			cmd.SetArgs([]string{"--install"})
			installer.EXPECT().InstallElemental(handedOver(alternateConfigFixture), stateFixture, networkConfigFixture).Return(nil)
			installer.EXPECT().SurveyInstallDisks().Return(nil, nil, nil)
			client.EXPECT().
				RegisterForInstall(baseConfigFixture.Elemental.Registration, []byte(baseConfigFixture.Elemental.Registration.CACert), &stateFixture, gomock.Any()).
				Return(append(marshalToBytes(alternateConfigFixture), marshalToBytes(networkConfigFixture)...), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
	})
})

var _ = Describe("elemental-register --install --no-toolkit", Label("registration", "cli", "install-notoolkit"), func() {
	var fs vfs.FS
	var err error
	var fsCleanup func()
	var cmd *cobra.Command
	var mockCtrl *gomock.Controller
	var client *rmocks.MockClient
	var installer *imocks.MockInstaller
	var stateHandler *rmocks.MockStateHandler
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		mockCtrl = gomock.NewController(GinkgoT())
		installer = imocks.NewMockInstaller(mockCtrl)
		stateHandler = rmocks.NewMockStateHandler(mockCtrl)
		client = rmocks.NewMockClient(mockCtrl)
		cmd = newCommand(fs, client, stateHandler, installer)
		DeferCleanup(fsCleanup)
	})
	When("using existing default config", func() {
		BeforeEach(func() {
			marshalIntoFile(fs, baseConfigFixture, defaultConfigPath)
			stateHandler.EXPECT().Init(defaultStatePath).Return(nil)
			stateHandler.EXPECT().Load().Return(stateFixture, nil)
			stateHandler.EXPECT().Save(stateFixture).Return(nil)
		})
		It("should trigger local system agent config when --install and --no-toolkit arguments", func() {
			cmd.SetArgs([]string{"--install", "--no-toolkit"})
			installer.EXPECT().WriteLocalSystemAgentConfig(notoolkitConfigFixture.Elemental).Return(nil)
			client.EXPECT().
				Register(notoolkitConfigFixture.Elemental.Registration, []byte(notoolkitConfigFixture.Elemental.Registration.CACert), &stateFixture).
				Return(marshalToBytes(notoolkitConfigFixture), nil)
			// A pre-installed host has no live install session.
			client.EXPECT().RegisterForInstall(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
			installer.EXPECT().SurveyInstallDisks().Times(0)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
	})
})

var _ = Describe("elemental-register --reset", Label("registration", "cli", "reset"), func() {
	var fs vfs.FS
	var err error
	var fsCleanup func()
	var cmd *cobra.Command
	var mockCtrl *gomock.Controller
	var client *rmocks.MockClient
	var installer *imocks.MockInstaller
	var stateHandler *rmocks.MockStateHandler
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		mockCtrl = gomock.NewController(GinkgoT())
		installer = imocks.NewMockInstaller(mockCtrl)
		stateHandler = rmocks.NewMockStateHandler(mockCtrl)
		client = rmocks.NewMockClient(mockCtrl)
		cmd = newCommand(fs, client, stateHandler, installer)
		DeferCleanup(fsCleanup)
	})
	When("using existing default config", func() {
		BeforeEach(func() {
			marshalIntoFile(fs, baseConfigFixture, defaultConfigPath)
			stateHandler.EXPECT().Init(defaultStatePath).Return(nil)
			stateHandler.EXPECT().Load().Times(0) // When resetting expect new state to be initialized
			stateHandler.EXPECT().Save(register.State{}).Return(nil)
		})
		It("should trigger reset when --reset argument", func() {
			Expect(err).ShouldNot(HaveOccurred())

			cmd.SetArgs([]string{"--reset"})
			installer.EXPECT().ResetElemental(alternateConfigFixture, register.State{}, networkConfigFixture).Return(nil)
			client.EXPECT().
				Register(baseConfigFixture.Elemental.Registration, []byte(baseConfigFixture.Elemental.Registration.CACert), &register.State{}).
				Return(append(marshalToBytes(alternateConfigFixture), marshalToBytes(networkConfigFixture)...), nil)
			Expect(cmd.Execute()).ToNot(HaveOccurred())
		})
	})
})

func marshalIntoFile(fs vfs.FS, input any, filePath string) {
	bytes := marshalToBytes(input)
	Expect(vfs.MkdirAll(fs, path.Dir(filePath), os.ModePerm)).ToNot(HaveOccurred())
	Expect(fs.WriteFile(filePath, bytes, os.ModePerm)).ToNot(HaveOccurred())
}

func marshalToBytes(input any) []byte {
	bytes, err := yaml.Marshal(input)
	Expect(err).ToNot(HaveOccurred())
	return bytes
}

var _ = Describe("elemental-register --install --installer kubeos", Label("registration", "cli", "install-kubeos"), func() {
	var fs vfs.FS
	var err error
	var fsCleanup func()
	var cmd *cobra.Command
	var mockCtrl *gomock.Controller
	var client *rmocks.MockClient
	var installer *imocks.MockInstaller
	var stateHandler *rmocks.MockStateHandler
	var powerActions *[]string
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		mockCtrl = gomock.NewController(GinkgoT())
		installer = imocks.NewMockInstaller(mockCtrl)
		stateHandler = rmocks.NewMockStateHandler(mockCtrl)
		client = rmocks.NewMockClient(mockCtrl)
		cmd = newCommand(fs, client, stateHandler, installer)
		DeferCleanup(fsCleanup)
		// The package-level flag variable persists across Describe blocks.
		DeferCleanup(func() { osInstaller = install.ToolkitInstaller })
		allowCompletionReports(client)
		powerActions = fakePowerActions()
	})
	When("using existing live config", func() {
		BeforeEach(func() {
			// Same live paths as the toolkit installer: registration is identical.
			marshalIntoFile(fs, baseConfigFixture, defaultLiveConfigPath)
			stateHandler.EXPECT().Init(defaultLiveStatePath).Return(nil)
			stateHandler.EXPECT().Load().Return(stateFixture, nil)
			stateHandler.EXPECT().Save(stateFixture).Return(nil)
		})
		It("dispatches to the kubeos backend after registering", func() {
			cmd.SetArgs([]string{"--install", "--installer", "kubeos"})
			installer.EXPECT().InstallKubeOS(handedOver(alternateConfigFixture), stateFixture, networkConfigFixture).Return(nil)
			installer.EXPECT().SurveyInstallDisks().Return(nil, nil, nil)
			var installers []elementalv1.InstallerKind
			client.EXPECT().
				RegisterForInstall(baseConfigFixture.Elemental.Registration, []byte(baseConfigFixture.Elemental.Registration.CACert), &stateFixture, gomock.Any()).
				DoAndReturn(func(_ elementalv1.Registration, _ []byte, _ *register.State, session *register.LiveInstallSession) ([]byte, error) {
					installers = append(installers, session.Installer)
					return append(marshalToBytes(alternateConfigFixture), marshalToBytes(networkConfigFixture)...), nil
				})
			Expect(cmd.Execute()).ToNot(HaveOccurred())
			Expect(installers).To(Equal([]elementalv1.InstallerKind{elementalv1.InstallerKubeOS}))
			// The KubeOS backend powers off before it reboots, and so does
			// the register in its place.
			Expect(*powerActions).To(Equal([]string{"poweroff"}))
		})
	})
	It("rejects an unknown installer before reading any config", func() {
		cmd.SetArgs([]string{"--install", "--installer", "cloud"})
		err := cmd.Execute()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`--installer must be "toolkit" or "kubeos"`))
	})
})

// testJitter is the hold jitter in tests.
const testJitter = 1500 * time.Millisecond

// handedOver is the configuration an install backend gets from the live
// install: the reboot and poweroff are the register's to run, after it
// reported the install complete.
func handedOver(config elementalv1.Config) elementalv1.Config {
	config.Elemental.Install.Reboot = false
	config.Elemental.Install.PowerOff = false
	return config
}

// fakePowerActions records the systemctl power actions instead of running
// them.
func fakePowerActions() *[]string {
	actions := &[]string{}
	orig := powerAction
	powerAction = func(action string) error {
		*actions = append(*actions, action)
		return nil
	}
	DeferCleanup(func() { powerAction = orig })
	return actions
}

// reportOfPhase matches an InstallReport by its phase.
type reportOfPhase elementalv1.InstallPhase

func (m reportOfPhase) Matches(x any) bool {
	report, ok := x.(register.InstallReport)
	return ok && report.Phase == elementalv1.InstallPhase(m)
}

func (m reportOfPhase) String() string { return "an install report of phase " + string(m) }

// allowCompletionReports lets every successful install report itself.
func allowCompletionReports(client *rmocks.MockClient) {
	client.EXPECT().ReportInstall(gomock.Any(), gomock.Any(), gomock.Any(), reportOfPhase(elementalv1.InstallPhaseInstalled)).Return(nil).AnyTimes()
}

// fakeInstallWaits records the waits of the install loop instead of sleeping.
func fakeInstallWaits() *[]time.Duration {
	waits := &[]time.Duration{}
	origSleep, origJitter := sleep, holdJitter
	sleep = func(d time.Duration) { *waits = append(*waits, d) }
	holdJitter = func() time.Duration { return testJitter }
	DeferCleanup(func() { sleep, holdJitter = origSleep, origJitter })
	return waits
}

var _ = Describe("elemental-register --install live session", Label("registration", "cli", "install", "install-session"), func() {
	var fs *vfst.TestFS
	var err error
	var fsCleanup func()
	var cmd *cobra.Command
	var mockCtrl *gomock.Controller
	var client *rmocks.MockClient
	var installer *imocks.MockInstaller
	var stateHandler *rmocks.MockStateHandler
	var waits *[]time.Duration
	var powerActions *[]string

	const bootID = "4f6c1b2a-0d3e-4c5f-9a8b-7c6d5e4f3a2b"
	registration := baseConfigFixture.Elemental.Registration
	caCert := []byte(baseConfigFixture.Elemental.Registration.CACert)
	candidates := []elementalv1.InstallCandidate{
		{ID: "wwn:5000c500000000a1", Path: "/dev/sda", SizeBytes: 480 << 30, Eligible: true},
		{ID: "wwn:5000c500000000b2", Path: "/dev/sdb", SizeBytes: 2000 << 30, StaleSystem: true, Eligible: true},
	}
	stale := []elementalv1.StaleSystemDisk{{ID: "wwn:5000c500000000b2", Evidence: []string{"COS_STATE"}, Action: elementalv1.StaleSystemDiskWipe}}
	hold := func(seconds int) error {
		return fmt.Errorf("read elemental configuration: %w", &register.InstallHoldError{Hold: register.InstallHold{
			Reason: elementalv1.AwaitingSelectionReason, Message: "pick the install disk", RetryAfterSeconds: seconds,
		}})
	}
	released := func(config elementalv1.Config) []byte {
		return append(marshalToBytes(config), marshalToBytes(networkConfigFixture)...)
	}
	heldWait := 15*time.Second + testJitter

	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{bootIDPath: bootID + "\n"})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(fsCleanup)
		mockCtrl = gomock.NewController(GinkgoT())
		installer = imocks.NewMockInstaller(mockCtrl)
		stateHandler = rmocks.NewMockStateHandler(mockCtrl)
		client = rmocks.NewMockClient(mockCtrl)
		cmd = newCommand(fs, client, stateHandler, installer)
		DeferCleanup(func() { osInstaller = install.ToolkitInstaller })
		marshalIntoFile(fs, baseConfigFixture, defaultLiveConfigPath)
		stateHandler.EXPECT().Init(defaultLiveStatePath).Return(nil)
		waits = fakeInstallWaits()
		powerActions = fakePowerActions()
		cmd.SetArgs([]string{"--install"})
	})

	It("reports the install complete before it reboots", func() {
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil)
		config := alternateConfigFixture
		config.Elemental.InstallTarget = &elementalv1.InstallTarget{DeviceID: "wwn:5000c500000000a1"}
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).Return(released(config), nil)
		installer.EXPECT().InstallElemental(handedOver(config), stateFixture, networkConfigFixture).DoAndReturn(
			func(elementalv1.Config, register.State, elementalv1.NetworkConfig) error {
				Expect(*powerActions).To(BeEmpty())
				return nil
			})
		client.EXPECT().ReportInstall(registration, caCert, gomock.Any(), register.InstallReport{
			BootID: bootID, Phase: elementalv1.InstallPhaseInstalled, TargetDeviceID: "wwn:5000c500000000a1",
		}).DoAndReturn(func(elementalv1.Registration, []byte, *register.State, register.InstallReport) error {
			Expect(*powerActions).To(BeEmpty(), "reported before the reboot")
			return nil
		})
		Expect(cmd.Execute()).To(Succeed())
		// The toolkit reboots before it powers off; the fixture asks for both.
		Expect(*powerActions).To(Equal([]string{"reboot"}))
	})

	It("reboots even when the operator does not take the report", func() {
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil)
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).Return(released(alternateConfigFixture), nil)
		installer.EXPECT().InstallElemental(handedOver(alternateConfigFixture), stateFixture, networkConfigFixture).Return(nil)
		client.EXPECT().ReportInstall(gomock.Any(), gomock.Any(), gomock.Any(), reportOfPhase(elementalv1.InstallPhaseInstalled)).Return(errors.New("connection refused"))
		Expect(cmd.Execute()).To(Succeed())
		Expect(*powerActions).To(Equal([]string{"reboot"}))
	})

	It("leaves the host running when the configuration asks for no reboot", func() {
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil)
		config := handedOver(alternateConfigFixture)
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).Return(released(config), nil)
		installer.EXPECT().InstallElemental(config, stateFixture, networkConfigFixture).Return(nil)
		client.EXPECT().ReportInstall(gomock.Any(), gomock.Any(), gomock.Any(), reportOfPhase(elementalv1.InstallPhaseInstalled)).Return(nil)
		Expect(cmd.Execute()).To(Succeed())
		Expect(*powerActions).To(BeEmpty())
	})

	It("fails when the reboot after the install fails", func() {
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil)
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).Return(released(alternateConfigFixture), nil)
		installer.EXPECT().InstallElemental(handedOver(alternateConfigFixture), stateFixture, networkConfigFixture).Return(nil)
		client.EXPECT().ReportInstall(gomock.Any(), gomock.Any(), gomock.Any(), reportOfPhase(elementalv1.InstallPhaseInstalled)).Return(nil)
		powerAction = func(string) error { return errors.New("Failed to connect to bus") }
		Expect(cmd.Execute()).To(MatchError(ContainSubstring("systemctl reboot after the install: Failed to connect to bus")))
	})

	It("registers again while held, touching no disk, and installs once released", func() {
		allowCompletionReports(client)
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil).Times(3)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil).Times(3)
		gomock.InOrder(
			client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).Return(nil, hold(0)),
			client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).Return(nil, hold(40)),
			client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).Return(released(alternateConfigFixture), nil),
			installer.EXPECT().InstallElemental(handedOver(alternateConfigFixture), stateFixture, networkConfigFixture).Return(nil),
		)
		Expect(cmd.Execute()).To(Succeed())
		Expect(*waits).To(Equal([]time.Duration{heldWait, 40*time.Second + testJitter}))
	})

	It("tells the operator about the boot, the installer and the disks", func() {
		allowCompletionReports(client)
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil)
		var sessions []register.LiveInstallSession
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).
			DoAndReturn(func(_ elementalv1.Registration, _ []byte, _ *register.State, session *register.LiveInstallSession) ([]byte, error) {
				sessions = append(sessions, *session)
				return released(alternateConfigFixture), nil
			})
		installer.EXPECT().InstallElemental(handedOver(alternateConfigFixture), stateFixture, networkConfigFixture).Return(nil)
		Expect(cmd.Execute()).To(Succeed())
		Expect(sessions).To(Equal([]register.LiveInstallSession{{
			BootID:           bootID,
			Installer:        elementalv1.InstallerToolkit,
			RegisterVersion:  version.Version,
			Candidates:       candidates,
			StaleSystemDisks: stale,
		}}))
	})

	It("still registers when the disks cannot be surveyed, with the error instead", func() {
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		installer.EXPECT().SurveyInstallDisks().Return(nil, nil, errors.New("inspecting block devices: lsblk: not found"))
		var sessions []register.LiveInstallSession
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).
			DoAndReturn(func(_ elementalv1.Registration, _ []byte, _ *register.State, session *register.LiveInstallSession) ([]byte, error) {
				sessions = append(sessions, *session)
				return nil, errors.New("connection reset by peer")
			})
		Expect(cmd.Execute()).To(MatchError("registering machine: connection reset by peer"))
		Expect(sessions).To(Equal([]register.LiveInstallSession{{
			BootID:          bootID,
			Installer:       elementalv1.InstallerToolkit,
			RegisterVersion: version.Version,
			SurveyError:     "inspecting block devices: lsblk: not found",
		}}))
	})

	It("makes a boot ID up once when the kernel's cannot be read", func() {
		allowCompletionReports(client)
		Expect(fs.Remove(bootIDPath)).To(Succeed())
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil).Times(2)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil).Times(2)
		var bootIDs []string
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).
			DoAndReturn(func(_ elementalv1.Registration, _ []byte, _ *register.State, session *register.LiveInstallSession) ([]byte, error) {
				bootIDs = append(bootIDs, session.BootID)
				if len(bootIDs) == 1 {
					return nil, hold(0)
				}
				return released(alternateConfigFixture), nil
			}).Times(2)
		installer.EXPECT().InstallElemental(handedOver(alternateConfigFixture), stateFixture, networkConfigFixture).Return(nil)
		Expect(cmd.Execute()).To(Succeed())
		Expect(bootIDs).To(HaveLen(2))
		Expect(bootIDs[0]).To(MatchRegexp(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`))
		Expect(bootIDs[1]).To(Equal(bootIDs[0]))
	})

	It("gives up on an error before the operator held the host, as before", func() {
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil)
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).
			Return(nil, errors.New("dial tcp 127.0.0.1:443: connect: connection refused"))
		Expect(cmd.Execute()).To(MatchError("registering machine: dial tcp 127.0.0.1:443: connect: connection refused"))
		Expect(*waits).To(BeEmpty())
	})

	It("retries errors with a backoff once the operator held the host", func() {
		allowCompletionReports(client)
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil).Times(3)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil).Times(10)
		refused := errors.New("connection refused")
		round := func() *gomock.Call {
			return client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any())
		}
		calls := []any{round().Return(nil, hold(0))}
		for range 6 {
			calls = append(calls, round().Return(nil, refused))
		}
		calls = append(calls,
			round().Return(nil, hold(0)),
			round().Return(nil, refused),
			round().Return(released(alternateConfigFixture), nil),
			installer.EXPECT().InstallElemental(handedOver(alternateConfigFixture), stateFixture, networkConfigFixture).Return(nil),
		)
		gomock.InOrder(calls...)
		Expect(cmd.Execute()).To(Succeed())
		Expect(*waits).To(Equal([]time.Duration{
			heldWait, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second,
			heldWait, 5 * time.Second,
		}))
	})

	It("registers as a new host in every round until released", func() {
		allowCompletionReports(client)
		stateHandler.EXPECT().Load().Return(register.State{}, nil)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil).Times(3)
		var seen []register.State
		client.EXPECT().RegisterForInstall(registration, caCert, gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ elementalv1.Registration, _ []byte, state *register.State, _ *register.LiveInstallSession) ([]byte, error) {
				seen = append(seen, *state)
				// What a registration does to the state of a new host: the
				// TPM mode is decided once, the salt that worked is kept.
				state.InitialRegistration = time.Now()
				if state.TPMMode == "" {
					state.TPMMode, state.EmulatedTPMSeedSalt = elementalv1.TPMModeEmulated, 1
				}
				if len(seen) < 3 {
					return nil, hold(0)
				}
				return released(alternateConfigFixture), nil
			}).Times(3)
		var saved []register.State
		stateHandler.EXPECT().Save(gomock.Any()).DoAndReturn(func(state register.State) error {
			saved = append(saved, state)
			return nil
		}).Times(3)
		installer.EXPECT().InstallElemental(handedOver(alternateConfigFixture), gomock.Any(), networkConfigFixture).Return(nil)
		Expect(cmd.Execute()).To(Succeed())
		decided := register.State{TPMMode: elementalv1.TPMModeEmulated, EmulatedTPMSeedSalt: 1}
		Expect(seen).To(Equal([]register.State{{}, decided, decided}))
		// A restart while held registers as a new host as well; the released
		// host is registered.
		Expect(saved[:2]).To(Equal([]register.State{decided, decided}))
		Expect(saved[2].InitialRegistration).NotTo(BeZero())
		Expect(saved[2].TPMMode).To(Equal(elementalv1.TPMModeEmulated))
	})

	It("reports a failed install with the reason of the disk error", func() {
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil)
		target := alternateConfigFixture.DeepCopy()
		target.Elemental.InstallTarget = &elementalv1.InstallTarget{DeviceID: "wwn:5000c500000000a1"}
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).Return(released(*target), nil)
		installErr := fmt.Errorf("failed picking installation device: %w",
			install.NewDiskError("Blocked", "stale system disks cannot be wiped safely"))
		installer.EXPECT().InstallElemental(handedOver(*target), stateFixture, networkConfigFixture).Return(installErr)
		// With the registration the host registered with, not the one the
		// operator returned. A report that fails is only logged.
		client.EXPECT().ReportInstall(registration, caCert, &stateFixture, register.InstallReport{
			BootID:         bootID,
			Phase:          elementalv1.InstallPhaseFailed,
			TargetDeviceID: "wwn:5000c500000000a1",
			Reason:         "Blocked",
			Message:        "failed picking installation device: Blocked: stale system disks cannot be wiped safely",
		}).Return(errors.New("connection refused"))
		Expect(cmd.Execute()).To(MatchError("installing elemental: failed picking installation device: Blocked: stale system disks cannot be wiped safely"))
	})

	It("reports a failed kubeos install", func() {
		cmd.SetArgs([]string{"--install", "--installer", "kubeos"})
		stateHandler.EXPECT().Load().Return(stateFixture, nil)
		stateHandler.EXPECT().Save(stateFixture).Return(nil)
		installer.EXPECT().SurveyInstallDisks().Return(candidates, stale, nil)
		client.EXPECT().RegisterForInstall(registration, caCert, &stateFixture, gomock.Any()).Return(released(alternateConfigFixture), nil)
		installer.EXPECT().InstallKubeOS(handedOver(alternateConfigFixture), stateFixture, networkConfigFixture).Return(errors.New("kbimg install disk failed: exit status 1"))
		client.EXPECT().ReportInstall(registration, caCert, &stateFixture, register.InstallReport{
			BootID:  bootID,
			Phase:   elementalv1.InstallPhaseFailed,
			Message: "kbimg install disk failed: exit status 1",
		}).Return(nil)
		Expect(cmd.Execute()).To(MatchError("installing kubeos: kbimg install disk failed: exit status 1"))
	})
})
