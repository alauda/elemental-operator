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
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/twpayne/go-vfs"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/install"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/register"
	"github.com/rancher/elemental-operator/pkg/version"
)

// The live install session of `--install` from the live ISO. With
// install.device-selection: inventory the operator holds the host until a
// person picks its install disk. The host then registers again and again in
// this process, touching no disk, until the operator releases it with the
// configuration. It does not leave the waiting to the unit's restart policy:
// systemd's start limit would give up on a host that waits for long.

const (
	bootIDPath = "/proc/sys/kernel/random/boot_id"
	// maxHoldJitter spreads the registrations of held hosts.
	maxHoldJitter = 5 * time.Second
	// The backoff of failed registrations once the operator held the host.
	minRetryBackoff = 5 * time.Second
	maxRetryBackoff = 60 * time.Second
)

// Replaced in tests.
var (
	sleep      = time.Sleep
	holdJitter = func() time.Duration { return rand.N(maxHoldJitter + 1) }
)

// liveInstall is the live install session of this process.
type liveInstall struct {
	client register.Client
	// registration is what the host registers and reports with: the local
	// configuration, not the one the operator returns.
	registration elementalv1.Registration
	caCert       []byte
	bootID       string
}

func newLiveInstall(fs vfs.FS, client register.Client, registration elementalv1.Registration, caCert []byte) *liveInstall {
	return &liveInstall{client: client, registration: registration, caCert: caCert, bootID: readBootID(fs)}
}

// readBootID returns the boot ID the operator ties the session to. Without
// one a random ID stands in for this process; a restart of the command then
// looks like a new boot to the operator.
func readBootID(fs vfs.FS) string {
	data, err := fs.ReadFile(bootIDPath)
	if id := strings.TrimSpace(string(data)); err == nil && id != "" {
		return id
	}
	if err == nil {
		err = errors.New("the file is empty")
	}
	id := uuid.NewString()
	log.Warningf("Cannot read the boot ID from %s: %v; using %s for this run", bootIDPath, err, id)
	return id
}

// registerUntilReleased registers until the operator releases the host and
// returns the configuration. Before the operator first held the host, an
// error ends the command as it always did and the unit's restart policy
// takes over; once it held the host, errors are retried here with a backoff.
func (l *liveInstall) registerUntilReleased(stateHandler register.StateHandler, installer install.Installer, state *register.State) ([]byte, error) {
	// A host that has never been registered keeps registering as a new host
	// until it is released: the operator refuses an update of an inventory
	// that was deleted while the host waited, but recreates it for a new
	// registration. Everything else in the state carries over from round to
	// round, so the TPM mode is decided only once and a seed salt that
	// cleared an identity conflict is kept.
	initialRegistration := state.InitialRegistration
	held := false
	var backoff time.Duration
	for {
		state.InitialRegistration = initialRegistration
		data, err := l.client.RegisterForInstall(l.registration, l.caCert, state, l.session(installer))
		var holdErr *register.InstallHoldError
		switch {
		case err == nil:
			return data, nil
		case errors.As(err, &holdErr):
			// Save what the next round registers with, so that a restart of
			// the command carries on the same way.
			state.InitialRegistration = initialRegistration
			if err := stateHandler.Save(*state); err != nil {
				return nil, fmt.Errorf("saving registration state: %w", err)
			}
			why := holdErr.Hold.Reason
			if holdErr.Hold.Message != "" {
				why += ": " + holdErr.Hold.Message
			}
			wait := holdErr.RetryAfter() + holdJitter()
			log.Infof("Install held by the operator (%s); registering again in %s", why, wait.Round(time.Second))
			held, backoff = true, 0
			sleep(wait)
		case held:
			backoff = nextBackoff(backoff)
			log.Warningf("Registering for install failed: %v; retrying in %s", err, backoff)
			sleep(backoff)
		default:
			return nil, err
		}
	}
}

func nextBackoff(current time.Duration) time.Duration {
	if current < minRetryBackoff {
		return minRetryBackoff
	}
	return min(2*current, maxRetryBackoff)
}

// session surveys the disks for this round. A host whose disks cannot be
// listed registers with the error instead: the operator shows it and does
// not release the host.
func (l *liveInstall) session(installer install.Installer) *register.LiveInstallSession {
	session := &register.LiveInstallSession{
		BootID:          l.bootID,
		Installer:       elementalv1.InstallerToolkit,
		RegisterVersion: version.Version,
	}
	if osInstaller == install.KubeOSInstaller {
		session.Installer = elementalv1.InstallerKubeOS
	}
	candidates, stale, err := installer.SurveyInstallDisks()
	if err != nil {
		log.Warningf("Cannot survey the disks for the operator: %v", err)
		session.SurveyError = err.Error()
		return session
	}
	session.Candidates, session.StaleSystemDisks = candidates, stale
	return session
}

// reportFailure tells the operator that the install failed. It is best
// effort: the install error is what the command fails with either way. A nil
// session, from a command that did not install from the live ISO, reports
// nothing.
func (l *liveInstall) reportFailure(state *register.State, target *elementalv1.InstallTarget, installErr error) {
	if l == nil {
		return
	}
	report := register.InstallReport{
		BootID:  l.bootID,
		Phase:   elementalv1.InstallPhaseFailed,
		Reason:  install.DiskErrorReason(installErr),
		Message: installErr.Error(),
	}
	if target != nil {
		report.TargetDeviceID = target.DeviceID
	}
	if err := l.client.ReportInstall(l.registration, l.caCert, state, report); err != nil {
		log.Warningf("Reporting the failed install to the operator: %v", err)
	}
}
