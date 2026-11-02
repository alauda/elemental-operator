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

package tpm

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeSysfs builds the parts of /sys and /dev the prober reads.
type fakeSysfs struct {
	root string
}

func newFakeSysfs() *fakeSysfs {
	return &fakeSysfs{root: GinkgoT().TempDir()}
}

func (f *fakeSysfs) write(path, content string) {
	full := filepath.Join(f.root, path)
	Expect(os.MkdirAll(filepath.Dir(full), 0o755)).To(Succeed())
	Expect(os.WriteFile(full, []byte(content), 0o644)).To(Succeed())
}

func (f *fakeSysfs) mkdir(path string) {
	Expect(os.MkdirAll(filepath.Join(f.root, path), 0o755)).To(Succeed())
}

// tpm2 adds a TPM 2.0 with a kernel resource manager.
func (f *fakeSysfs) tpm2(name, rm string, versionFile bool) {
	f.mkdir("sys/class/tpm/" + name)
	if versionFile {
		f.write("sys/class/tpm/"+name+"/tpm_version_major", "2\n")
	}
	if rm != "" {
		f.mkdir("sys/class/tpm/" + name + "/device/tpmrm/" + rm)
		f.write("dev/"+rm, "")
	} else {
		f.write("dev/"+name, "")
	}
}

func (f *fakeSysfs) acpiTPM2() {
	f.write("sys/firmware/acpi/tables/TPM2", "")
}

// testProber returns a prober on a fake clock that counts settles, sleeps
// and self-tests.
func testProber(fs *fakeSysfs, selfTest func() error) (*Prober, *probeCounters) {
	counters := &probeCounters{now: time.Unix(1_700_000_000, 0)}
	p := &Prober{
		Root:            fs.root,
		SettleTimeout:   time.Second,
		DeviceWait:      60 * time.Second,
		PollInterval:    2 * time.Second,
		SelfTestTimeout: time.Second,
		Settle: func(time.Duration) error {
			counters.settles++
			return nil
		},
		SelfTest: func() error {
			counters.selfTests++
			return selfTest()
		},
		Sleep: func(d time.Duration) {
			counters.sleeps++
			counters.now = counters.now.Add(d)
			if counters.onSleep != nil {
				counters.onSleep(counters.sleeps)
			}
		},
	}
	p.Now = func() time.Time { return counters.now }
	return p, counters
}

type probeCounters struct {
	now       time.Time
	settles   int
	sleeps    int
	selfTests int
	onSleep   func(n int)
}

var _ = Describe("hardware TPM probe", Label("tpm", "probe"), func() {
	var fs *fakeSysfs
	BeforeEach(func() {
		fs = newFakeSysfs()
	})

	It("accepts a TPM 2.0 that passes the self-test and prefers its resource manager", func() {
		fs.tpm2("tpm0", "tpmrm0", true)
		p, c := testProber(fs, func() error { return nil })
		result := p.Probe()
		Expect(result).To(Equal(ProbeResult{Usable: true, Device: "/dev/tpmrm0"}))
		Expect(c.selfTests).To(Equal(1))
		Expect(c.settles).To(BeZero())
		Expect(p.Present()).To(BeTrue())
	})
	It("follows go-attestation when tpm_version_major is missing", func() {
		fs.tpm2("tpm0", "", false)
		p, _ := testProber(fs, func() error { return nil })
		Expect(p.Probe()).To(Equal(ProbeResult{Usable: true, Device: "/dev/tpm0"}))
	})
	It("rejects a TPM 1.2 by tpm_version_major", func() {
		fs.mkdir("sys/class/tpm/tpm0")
		fs.write("sys/class/tpm/tpm0/tpm_version_major", "1\n")
		fs.write("dev/tpm0", "")
		p, c := testProber(fs, func() error { return nil })
		result := p.Probe()
		Expect(result.Usable).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("TPM 1.x"))
		Expect(c.selfTests).To(BeZero())
		Expect(p.Present()).To(BeFalse())
	})
	It("rejects a TPM 1.2 by its caps file on older kernels", func() {
		fs.write("sys/class/tpm/tpm0/caps", "Manufacturer: 0x0\n")
		fs.write("dev/tpm0", "")
		p, _ := testProber(fs, func() error { return nil })
		result := p.Probe()
		Expect(result.Usable).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("TPM 1.2"))
	})
	It("reports a host without TPM at once when the firmware declares none", func() {
		p, c := testProber(fs, func() error { return nil })
		result := p.Probe()
		Expect(result.Usable).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("no TPM device"))
		Expect(result.Reason).To(ContainSubstring("no ACPI TPM2 table"))
		Expect(c.settles).To(BeZero())
		Expect(c.sleeps).To(BeZero())
	})
	It("waits for a device the firmware declares and the driver creates late", func() {
		fs.acpiTPM2()
		p, c := testProber(fs, func() error { return nil })
		c.onSleep = func(n int) {
			if n == 3 {
				fs.tpm2("tpm0", "tpmrm0", true)
			}
		}
		Expect(p.Probe()).To(Equal(ProbeResult{Usable: true, Device: "/dev/tpmrm0"}))
		Expect(c.settles).To(Equal(1))
		Expect(c.sleeps).To(Equal(3))
	})
	It("gives up after the device wait when the firmware declares a TPM that never shows up", func() {
		fs.acpiTPM2()
		p, c := testProber(fs, func() error { return nil })
		result := p.Probe()
		Expect(result.Usable).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("ACPI TPM2 table"))
		Expect(result.Reason).To(ContainSubstring("1m0s"))
		Expect(c.sleeps).To(Equal(30))
		Expect(c.selfTests).To(BeZero())
	})
	It("rejects a device that fails the self-test", func() {
		fs.tpm2("tpm0", "tpmrm0", true)
		p, _ := testProber(fs, func() error { return errors.New("TPM_RC_FAILURE") })
		result := p.Probe()
		Expect(result.Usable).To(BeFalse())
		Expect(result.Device).To(Equal("/dev/tpmrm0"))
		Expect(result.Reason).To(ContainSubstring("TPM_RC_FAILURE"))
	})
	It("rejects a device that does not answer the self-test in time", func() {
		fs.tpm2("tpm0", "tpmrm0", true)
		block := make(chan struct{})
		DeferCleanup(func() { close(block) })
		p, _ := testProber(fs, func() error { <-block; return nil })
		p.SelfTestTimeout = 50 * time.Millisecond
		result := p.Probe()
		Expect(result.Usable).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("no answer within 50ms"))
	})
	It("reports a sysfs entry without device node", func() {
		fs.mkdir("sys/class/tpm/tpm0")
		fs.write("sys/class/tpm/tpm0/tpm_version_major", "2\n")
		p, _ := testProber(fs, func() error { return nil })
		result := p.Probe()
		Expect(result.Usable).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("/dev/tpm0 is missing"))
	})
})

var _ = Describe("hardware TPM probe, go-attestation parity", Label("tpm", "probe"), func() {
	It("does not skip a broken first TPM 2.0 for a later one", func() {
		fs := newFakeSysfs()
		fs.mkdir("sys/class/tpm/tpm0")
		fs.write("sys/class/tpm/tpm0/tpm_version_major", "2\n")
		fs.tpm2("tpm1", "tpmrm1", true)
		p, _ := testProber(fs, func() error { return nil })
		Expect(p.Present()).To(BeFalse())
		result := p.Probe()
		Expect(result.Usable).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("/dev/tpm0 is missing"))
	})
	It("skips TPM 1.2 devices like go-attestation does", func() {
		fs := newFakeSysfs()
		fs.write("sys/class/tpm/tpm0/caps", "Manufacturer: 0x0\n")
		fs.tpm2("tpm1", "tpmrm1", true)
		p, _ := testProber(fs, func() error { return nil })
		Expect(p.Probe()).To(Equal(ProbeResult{Usable: true, Device: "/dev/tpmrm1"}))
	})
	It("turns a panicking self-test into a failed probe", func() {
		fs := newFakeSysfs()
		fs.tpm2("tpm0", "tpmrm0", true)
		p, _ := testProber(fs, func() error { panic("boom") })
		result := p.Probe()
		Expect(result.Usable).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("panic: boom"))
	})
})
