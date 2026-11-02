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
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gotpm "github.com/rancher-sandbox/go-tpm"

	"github.com/rancher/elemental-operator/pkg/log"
)

// ProbeResult tells whether the hardware TPM can carry the registration
// identity.
type ProbeResult struct {
	// Usable is true when a TPM 2.0 device exists and passed the self-test.
	Usable bool
	// Device is the device node that was checked, e.g. /dev/tpmrm0.
	Device string
	// Reason explains why the TPM is not usable.
	Reason string
}

// Prober checks the hardware TPM the way go-attestation will use it.
type Prober struct {
	// Root prefixes /sys and /dev; "/" outside tests.
	Root string
	// SettleTimeout bounds `udevadm settle`.
	SettleTimeout time.Duration
	// DeviceWait bounds the wait for a TPM the firmware declares but the
	// kernel has not created yet (slow tpm_crb/tpm_tis probing).
	DeviceWait time.Duration
	// PollInterval is the delay between two looks at sysfs while waiting.
	PollInterval time.Duration
	// SelfTestTimeout bounds the attestation round trip.
	SelfTestTimeout time.Duration

	// Settle waits for udev to process pending events.
	Settle func(timeout time.Duration) error
	// SelfTest runs the registration attestation path offline.
	SelfTest func() error
	// Sleep and Now are the clock.
	Sleep func(time.Duration)
	Now   func() time.Time
}

// NewProber returns a prober for this host.
func NewProber() *Prober {
	return &Prober{
		Root:          "/",
		SettleTimeout: 30 * time.Second,
		DeviceWait:    60 * time.Second,
		PollInterval:  2 * time.Second,
		// Covers EK, SRK and AK generation (RSA-2048 each) on slow TPMs.
		SelfTestTimeout: 3 * time.Minute,
		Settle:          udevSettle,
		SelfTest:        func() error { return SelfTest() },
		Sleep:           time.Sleep,
		Now:             time.Now,
	}
}

// Probe decides whether the hardware TPM is usable: a TPM 2.0 device must
// exist (waiting for it when the firmware declares one) and must complete
// the attestation the registration server will ask for. A device file alone
// proves nothing: fTPMs and TPMs left in a failed state show up in sysfs too.
func (p *Prober) Probe() ProbeResult {
	device, reason := p.WaitForDevice()
	if device == "" {
		return ProbeResult{Reason: reason}
	}
	if err := p.runSelfTest(); err != nil {
		return ProbeResult{Device: device, Reason: fmt.Sprintf("TPM self-test on %s failed: %v", device, err)}
	}
	return ProbeResult{Usable: true, Device: device}
}

// Present reports, without waiting or talking to the device, whether a TPM
// 2.0 device is visible.
func (p *Prober) Present() bool {
	device, _ := p.scan()
	return device != ""
}

// WaitForDevice returns the TPM 2.0 device node go-attestation will open, or
// why there is none. When the firmware declares a TPM 2.0 (ACPI TPM2 table)
// but the kernel shows no usable device yet, it lets udev settle and polls
// for up to DeviceWait.
func (p *Prober) WaitForDevice() (string, string) {
	device, reason := p.scan()
	if device != "" {
		return device, ""
	}
	if !p.firmwareDeclaresTPM2() {
		return "", reason + "; the firmware declares no TPM 2.0 (no ACPI TPM2 table)"
	}
	log.Infof("Firmware declares a TPM 2.0 but no usable device is present yet (%s); waiting up to %s", reason, p.DeviceWait)
	if err := p.Settle(p.SettleTimeout); err != nil {
		log.Debugf("udevadm settle: %v", err)
	}
	deadline := p.Now().Add(p.DeviceWait)
	for {
		if device, reason = p.scan(); device != "" {
			return device, ""
		}
		if !p.Now().Before(deadline) {
			break
		}
		p.Sleep(p.PollInterval)
	}
	return "", fmt.Sprintf("the firmware declares a TPM 2.0 (ACPI TPM2 table) but no usable device appeared within %s: %s; check that the tpm_crb or tpm_tis driver is available",
		p.DeviceWait, reason)
}

// scan applies go-attestation's device rules to /sys/class/tpm: the first
// TPM 2.0 (in name order) is the one it opens, through its kernel resource
// manager (tpmrmN) when there is one.
func (p *Prober) scan() (string, string) {
	classDir := filepath.Join(p.Root, "sys", "class", "tpm")
	entries, err := os.ReadDir(classDir)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Sprintf("cannot read /sys/class/tpm: %v", err)
	}
	var notes []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "tpm") {
			continue
		}
		if major, why := p.versionMajor(name); major != 2 {
			notes = append(notes, name+": "+why)
			continue
		}
		node := p.deviceNode(name)
		if _, err := os.Stat(filepath.Join(p.Root, node)); err != nil {
			// go-attestation would fail on this one, not try the next.
			notes = append(notes, fmt.Sprintf("%s: device node %s is missing", name, node))
			return "", strings.Join(notes, "; ")
		}
		return node, ""
	}
	if len(notes) == 0 {
		return "", "no TPM device in /sys/class/tpm"
	}
	return "", strings.Join(notes, "; ")
}

// versionMajor reads tpm_version_major. Kernels older than 5.x lack it;
// then, like go-attestation, a device with a caps file is a TPM 1.2.
func (p *Prober) versionMajor(name string) (int, string) {
	dir := filepath.Join(p.Root, "sys", "class", "tpm", name)
	data, err := os.ReadFile(filepath.Join(dir, "tpm_version_major"))
	switch {
	case err == nil:
		major, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if convErr != nil {
			return 0, fmt.Sprintf("unreadable tpm_version_major %q", strings.TrimSpace(string(data)))
		}
		if major != 2 {
			return major, fmt.Sprintf("TPM %d.x, TPM 2.0 is required", major)
		}
		return major, ""
	case !os.IsNotExist(err):
		return 0, fmt.Sprintf("cannot read tpm_version_major: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "caps")); err == nil {
		return 1, "TPM 1.2 (caps file present), TPM 2.0 is required"
	}
	return 2, ""
}

func (p *Prober) deviceNode(name string) string {
	rm, err := os.ReadDir(filepath.Join(p.Root, "sys", "class", "tpm", name, "device", "tpmrm"))
	if err == nil && len(rm) > 0 {
		return "/dev/" + rm[0].Name()
	}
	return "/dev/" + name
}

func (p *Prober) firmwareDeclaresTPM2() bool {
	_, err := os.Stat(filepath.Join(p.Root, "sys", "firmware", "acpi", "tables", "TPM2"))
	return err == nil
}

// runSelfTest bounds SelfTest. A TPM that never answers leaves its
// goroutine behind; the probe has moved on by then and auto falls back to
// emulation.
func (p *Prober) runSelfTest() error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- p.SelfTest()
	}()
	timer := time.NewTimer(p.SelfTestTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("no answer within %s", p.SelfTestTimeout)
	}
}

func udevSettle(timeout time.Duration) error {
	out, err := exec.Command("udevadm", "settle", fmt.Sprintf("--timeout=%d", int(timeout.Seconds()))).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SelfTest runs, against the local TPM and without a server, the exchange a
// registration performs: create the attestation token, decode it as the
// server does, hash the EK, generate the credential challenge, answer it
// with the AK and validate the answer. opts select the TPM like the
// registration does (none for the hardware TPM).
func SelfTest(opts ...gotpm.Option) error {
	token, ak, err := gotpm.GetAuthToken(opts...)
	if err != nil {
		return fmt.Errorf("creating attestation data: %w", err)
	}
	ek, attestation, err := gotpm.GetAttestationData(token)
	if err != nil {
		return fmt.Errorf("decoding attestation data: %w", err)
	}
	if _, err := gotpm.DecodePubHash(ek); err != nil {
		return fmt.Errorf("hashing the EK: %w", err)
	}
	secret, challenge, err := gotpm.GenerateChallenge(ek, attestation)
	if err != nil {
		return fmt.Errorf("generating the challenge: %w", err)
	}
	channel := &loopbackChannel{in: bytes.NewReader(challenge)}
	if err := gotpm.Authenticate(ak, channel, opts...); err != nil {
		return fmt.Errorf("answering the challenge: %w", err)
	}
	if err := gotpm.ValidateChallenge(secret, channel.out.Bytes()); err != nil {
		return fmt.Errorf("validating the challenge answer: %w", err)
	}
	return nil
}

// loopbackChannel hands the challenge to gotpm.Authenticate and keeps its
// answer, standing in for the websocket.
type loopbackChannel struct {
	in  *bytes.Reader
	out bytes.Buffer
}

func (c *loopbackChannel) Read(p []byte) (int, error) { return c.in.Read(p) }

func (c *loopbackChannel) Write(p []byte) (int, error) { return c.out.Write(p) }

// ErrHardwareTPMUnavailable is returned when a host bound to its hardware
// TPM finds none.
var ErrHardwareTPMUnavailable = errors.New("hardware TPM unavailable")
