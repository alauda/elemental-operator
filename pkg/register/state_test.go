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

package register

import (
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/twpayne/go-vfs"
	"github.com/twpayne/go-vfs/vfst"
	"gopkg.in/yaml.v3"
)

func TestRegister(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Register State Suite")
}

var (
	testStateDir  = "/test/register/state"
	testStatePath = fmt.Sprintf("%s/%s", testStateDir, "state.yaml")
	stateFixture  = State{
		InitialRegistration: time.Now().UTC(),
		EmulatedTPM:         true,
		EmulatedTPMSeed:     123456789,
	}
)

var _ = Describe("is state updatable", Label("registration", "state"), func() {
	It("returns false if the state is new", func() {
		state := State{}
		Expect(state.IsUpdatable()).To(BeFalse())
	})
	It("returns true if the initial registration already happened", func() {
		state := State{
			InitialRegistration: time.Now(),
		}
		Expect(state.IsUpdatable()).To(BeTrue())
	})
})

var _ = Describe("init file state handler", Label("registration", "state"), func() {
	var fs vfs.FS
	var handler StateHandler
	var err error
	var fsCleanup func()
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		handler = NewFileStateHandler(fs)
		DeferCleanup(fsCleanup)
	})
	When("directory exists", func() {
		BeforeEach(func() {
			Expect(vfs.MkdirAll(fs, testStateDir, os.ModePerm)).ToNot(HaveOccurred())
		})
		It("should create empty state", func() {
			Expect(handler.Init(testStatePath)).ToNot(HaveOccurred())
			Expect(fs.ReadFile(testStatePath)).To(Equal([]byte("{}\n")))
		})
		It("should do nothing if file exists", func() {
			testBytes := []byte("foobar")
			Expect(fs.WriteFile(testStatePath, testBytes, 0700)).ToNot(HaveOccurred())
			Expect(handler.Init(testStatePath)).ToNot(HaveOccurred())
			Expect(fs.ReadFile(testStatePath)).To(Equal(testBytes))
		})
	})
	When("directory does not exist", func() {
		It("should create empty state", func() {
			Expect(handler.Init(testStatePath)).ToNot(HaveOccurred())
			Expect(fs.ReadFile(testStatePath)).To(Equal([]byte("{}\n")))
		})
	})
})

var _ = Describe("load state from filesystem", Label("registration", "state"), func() {
	var fs vfs.FS
	var handler StateHandler
	var err error
	var fsCleanup func()
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		handler = NewFileStateHandler(fs)
		DeferCleanup(fsCleanup)
	})
	When("init handler", func() {
		BeforeEach(func() {
			Expect(handler.Init(testStatePath)).ToNot(HaveOccurred())
		})
		It("should return state if state is deserializable", func() {
			bytes, err := yaml.Marshal(stateFixture)
			Expect(err).ToNot(HaveOccurred())
			Expect(fs.WriteFile(testStatePath, bytes, 0700)).ToNot(HaveOccurred())
			Expect(handler.Load()).To(Equal(stateFixture))
		})
		It("should return error if state is not deserializable", func() {
			bytes := []byte("I am definitely not yaml")
			Expect(fs.WriteFile(testStatePath, bytes, 0700)).ToNot(HaveOccurred())
			state, err := handler.Load()
			Expect(state).To(Equal(State{}))
			Expect(err).To(MatchError(errDecodingState))
		})
	})
	When("missing init handler", func() {
		It("should return error", func() {
			_, err := handler.Load()
			Expect(err).To(HaveOccurred())
		})
	})
})

var _ = Describe("save state to filesystem", Label("registration", "state"), func() {
	var fs vfs.FS
	var handler StateHandler
	var err error
	var fsCleanup func()
	BeforeEach(func() {
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		handler = NewFileStateHandler(fs)
		DeferCleanup(fsCleanup)
	})
	When("init handler", func() {
		BeforeEach(func() {
			Expect(handler.Init(testStatePath)).ToNot(HaveOccurred())
		})
		It("should return no error", func() {
			Expect(handler.Save(stateFixture))
			Expect(handler.Load()).To(Equal(stateFixture))
		})
		It("should return no error if file already exists", func() {
			bytes := []byte("I am going to be overwritten")
			Expect(fs.WriteFile(testStatePath, bytes, 0700)).ToNot(HaveOccurred())
			Expect(handler.Save(stateFixture))
			Expect(handler.Load()).To(Equal(stateFixture))
		})
	})
	When("missing init handler", func() {
		It("should return error", func() {
			Expect(handler.Save(stateFixture)).To(HaveOccurred())
		})
	})
})

var _ = Describe("state format compatibility", Label("registration", "state"), func() {
	var fs vfs.FS
	var handler StateHandler
	BeforeEach(func() {
		var fsCleanup func()
		var err error
		fs, fsCleanup, err = vfst.NewTestFS(map[string]interface{}{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(fsCleanup)
		handler = NewFileStateHandler(fs)
		Expect(handler.Init(testStatePath)).To(Succeed())
	})

	It("reads a state written by an earlier release", func() {
		legacy := "initialRegistration: 2025-01-02T03:04:05Z\nemulatedTPM: true\nemulatedTPMSeed: -9198403500847051214\n"
		Expect(fs.WriteFile(testStatePath, []byte(legacy), 0600)).To(Succeed())
		Expect(handler.Load()).To(Equal(State{
			InitialRegistration: time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC),
			EmulatedTPM:         true,
			EmulatedTPMSeed:     -9198403500847051214,
		}))
	})

	It("writes a state an earlier release still understands", func() {
		current := State{
			InitialRegistration:    time.Date(2026, time.October, 27, 8, 0, 0, 0, time.UTC),
			EmulatedTPM:            true,
			EmulatedTPMSeed:        -8384082019430403094,
			EmulatedTPMSeedVersion: 2,
			EmulatedTPMSeedSalt:    1,
			TPMMode:                "emulated",
			TPMModeSource:          TPMModeSourceProbe,
			TPMProbe:               &TPMProbe{Result: TPMProbeFailed, Reason: "no TPM device", At: time.Date(2026, time.October, 27, 8, 0, 0, 0, time.UTC)},
		}
		Expect(handler.Save(current)).To(Succeed())
		Expect(handler.Load()).To(Equal(current))

		// The State struct and decoder of elemental-register before tpm-mode.
		type legacyState struct {
			InitialRegistration time.Time `yaml:"initialRegistration,omitempty"`
			EmulatedTPM         bool      `yaml:"emulatedTPM,omitempty"`
			EmulatedTPMSeed     int64     `yaml:"emulatedTPMSeed,omitempty"`
		}
		file, err := fs.Open(testStatePath)
		Expect(err).ToNot(HaveOccurred())
		defer file.Close()
		var legacy legacyState
		Expect(yaml.NewDecoder(file).Decode(&legacy)).To(Succeed())
		Expect(legacy).To(Equal(legacyState{
			InitialRegistration: current.InitialRegistration,
			EmulatedTPM:         true,
			EmulatedTPMSeed:     -8384082019430403094,
		}))
	})

	It("keeps an empty state empty", func() {
		Expect(handler.Save(State{})).To(Succeed())
		Expect(fs.ReadFile(testStatePath)).To(Equal([]byte("{}\n")))
	})
})
