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

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
)

// What a host waiting in the live ISO tells the operator about its disks
// (install.device-selection: inventory), and why a released install stopped.

// SurveyInstallDisks lists the whole disks as install candidates and the
// stale system disks as the install would treat them with no target chosen
// yet. It touches no disk: lsblk, wipefs --no-act and sysfs only.
func (i *installer) SurveyInstallDisks() ([]elementalv1.InstallCandidate, []elementalv1.StaleSystemDisk, error) {
	inv, err := i.collectDiskInventory()
	if err != nil {
		return nil, nil, err
	}
	survey := i.inspectInstallDisks(inv, "", nil)
	log.Debugf("disk survey: %d install candidates, %d stale system disks", len(survey.Candidates), len(survey.Stale.Disks))
	return installCandidates(survey), staleSystemDisks(survey), nil
}

func installCandidates(survey *diskSurvey) []elementalv1.InstallCandidate {
	var candidates []elementalv1.InstallCandidate
	for _, c := range survey.Candidates {
		candidates = append(candidates, elementalv1.InstallCandidate{
			ID:          c.ID,
			Path:        c.Path,
			SizeBytes:   c.SizeBytes,
			Model:       c.Model,
			Serial:      c.Serial,
			WWN:         c.WWN,
			Transport:   c.Transport,
			Rotational:  c.Rotational,
			StaleSystem: c.StaleSystem,
			Eligible:    c.Eligible,
			Reasons:     orNil(c.Problems),
			HasData:     orNil(c.HasData),
		})
	}
	return candidates
}

// staleSystemDisks keeps the action the install would take. Where the
// install would not clean up at all (not a live environment, or / on a
// disk), the reason says the disk is left alone and why.
func staleSystemDisks(survey *diskSurvey) []elementalv1.StaleSystemDisk {
	var disks []elementalv1.StaleSystemDisk
	for _, s := range survey.Stale.Disks {
		disk := elementalv1.StaleSystemDisk{
			ID:       s.ID,
			Evidence: orNil(s.Evidence),
			// The actions are named like the API's.
			Action: elementalv1.StaleSystemDiskAction(s.Action),
			Reason: s.Reason,
		}
		if !survey.Cleanup && s.Action != staleDiskTarget {
			leftAlone := "left alone: " + survey.CleanupSkipped
			if disk.Reason == "" {
				disk.Reason = leftAlone
			} else {
				disk.Reason += "; " + leftAlone
			}
		}
		disks = append(disks, disk)
	}
	return disks
}

// orNil drops empty lists, which JSON leaves out anyway.
func orNil(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return values
}

// DiskErrorReason returns why the pre-install step stopped when err comes
// from it — TargetNotObserved, TargetIneligible, TargetAmbiguous,
// TargetMismatch, Blocked or WipeFailed — and "" otherwise.
func DiskErrorReason(err error) string {
	var diskErr *installDiskError
	if errors.As(err, &diskErr) {
		return diskErr.Reason
	}
	return ""
}

// NewDiskError returns an error DiskErrorReason reports reason for, for
// code that stands in for an Installer, such as tests.
func NewDiskError(reason, message string) error {
	return &installDiskError{Reason: reason, Message: message}
}
