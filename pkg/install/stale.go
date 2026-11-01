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
	"os/exec"
	"strings"

	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/register"
)

// Stale system disks.
//
// A reinstall leaves the old system's filesystem labels, GPT partition names
// and ESP on every disk but the new target. GRUB, the initrd and `elemental
// upgrade` look partitions up by label or name across all disks, and KubeOS
// mounts PERSIST and BOOT by label, so the new system may boot, mount or
// upgrade the old one. During `--install` in a live environment every disk
// carrying an old Elemental or KubeOS layout is therefore wiped. A disk that
// cannot be wiped safely — in use, read-only or removable, without a unique
// stable ID, or holding anything besides the old layout — stops the install
// before any disk is touched (Blocked).

// wipeZeroBytes is how much is zeroed at each end of a wiped disk: enough to
// cover the GPT and every superblock location blkid probes.
const wipeZeroBytes = int64(64 << 20)

// staleDiskAction is what the install does with a stale system disk.
type staleDiskAction string

const (
	staleDiskWipe    staleDiskAction = "Wipe"
	staleDiskBlocked staleDiskAction = "Blocked"
	staleDiskTarget  staleDiskAction = "Target"
)

// staleDisk is one disk carrying an old system layout.
type staleDisk struct {
	ID       string
	Path     string
	Evidence []string
	Action   staleDiskAction
	// Reason says why a Blocked disk cannot be wiped.
	Reason string
	disk   *diskFacts
}

type staleDiskReport struct {
	Disks []staleDisk
}

func (r *staleDiskReport) withAction(action staleDiskAction) []staleDisk {
	var disks []staleDisk
	for _, disk := range r.Disks {
		if disk.Action == action {
			disks = append(disks, disk)
		}
	}
	return disks
}

// detectStaleDisks classifies every disk with an old system layout: target
// is the kernel path of the install target ("" while none is chosen), and a
// non-nil wipeIDs restricts wiping to the listed IDs.
func detectStaleDisks(inv *diskInventory, target string, wipeIDs *[]string) *staleDiskReport {
	var approved map[string]bool
	if wipeIDs != nil {
		approved = map[string]bool{}
		for _, id := range *wipeIDs {
			approved[register.NormalizeDeviceID(id)] = true
		}
	}
	report := &staleDiskReport{}
	for _, d := range inv.disks {
		if d.liveMedia || d.virtual || !d.layout.stale() {
			continue
		}
		entry := staleDisk{ID: d.displayID(), Path: d.path, Evidence: d.layout.evidence, Action: staleDiskWipe, disk: d}
		if d.path == target {
			entry.Action = staleDiskTarget
		} else if reasons := wipeBlockers(d, approved); len(reasons) > 0 {
			entry.Action = staleDiskBlocked
			entry.Reason = strings.Join(reasons, "; ")
		}
		report.Disks = append(report.Disks, entry)
	}
	return report
}

// wipeBlockers lists why a stale disk must not be wiped automatically.
func wipeBlockers(d *diskFacts, approved map[string]bool) []string {
	var reasons []string
	if d.dev.ID == "" {
		reasons = append(reasons, "no unique stable ID: "+d.dev.IDProblem)
	}
	reasons = append(reasons, d.problems...)
	if len(d.layout.foreign) > 0 {
		reasons = append(reasons, "holds more than the old system: "+strings.Join(d.layout.foreign, ", "))
	}
	if approved != nil && !approved[d.dev.ID] {
		reasons = append(reasons, "not in the wipe list the operator approved")
	}
	return reasons
}

// diskCandidate is one disk as an install candidate.
type diskCandidate struct {
	ID          string
	Path        string
	SizeBytes   int64
	Model       string
	Serial      string
	WWN         string
	Transport   string
	Rotational  bool
	StaleSystem bool
	// Eligible is set when the disk passes the safety gates, so it may be
	// named as the target explicitly.
	Eligible bool
	Problems []string
	// HasData lists what makes the disk look like someone's data;
	// device-selector and the single-disk fallback never pick such a disk.
	HasData []string
}

// diskSurvey is what the host can tell about its disks without touching
// any. SurveyInstallDisks hands it to the operator to show before a person
// releases the host for installation.
type diskSurvey struct {
	Candidates []diskCandidate
	Stale      *staleDiskReport
	// Cleanup is set when stale system disks are wiped: in a live environment
	// whose / is not on a disk. CleanupSkipped says why not otherwise.
	Cleanup        bool
	CleanupSkipped string
}

// inspectInstallDisks lists the install candidates and the stale system
// disks. target is the kernel path of the chosen target, "" while there is
// none; wipeIDs is the operator-approved wipe list, nil when there is none.
// It has no side effects.
func (i *installer) inspectInstallDisks(inv *diskInventory, target string, wipeIDs *[]string) *diskSurvey {
	survey := &diskSurvey{Stale: detectStaleDisks(inv, target, wipeIDs)}
	switch {
	case !register.LiveEnvironment(i.fs):
		survey.CleanupSkipped = "not a live environment"
	case inv.rootDisk != "":
		survey.CleanupSkipped = "/ is mounted from " + inv.rootDisk
	default:
		survey.Cleanup = true
	}
	for _, d := range inv.disks {
		if d.dev.Type != blockTypeDisk || d.virtual {
			continue
		}
		survey.Candidates = append(survey.Candidates, diskCandidate{
			ID:          d.displayID(),
			Path:        d.path,
			SizeBytes:   d.sizeBytes,
			Model:       d.dev.Model,
			Serial:      d.dev.Serial,
			WWN:         d.dev.WWN,
			Transport:   d.dev.Transport,
			Rotational:  d.dev.Rotational,
			StaleSystem: d.layout.stale(),
			Eligible:    len(d.problems) == 0,
			Problems:    d.problems,
			HasData:     d.data,
		})
	}
	return survey
}

func logDiskSurvey(survey *diskSurvey) {
	for _, c := range survey.Candidates {
		log.Debugf("install candidate %s %s size=%d model=%q eligible=%t staleSystem=%t hasData=%v problems=%v",
			c.ID, c.Path, c.SizeBytes, c.Model, c.Eligible, c.StaleSystem, c.HasData, c.Problems)
	}
	for _, s := range survey.Stale.Disks {
		reason := ""
		if s.Reason != "" {
			reason = " reason=" + s.Reason
		}
		log.Infof("stale system disk %s (%s): evidence=[%s] action=%s%s", s.ID, s.Path, strings.Join(s.Evidence, " "), s.Action, reason)
	}
	if !survey.Cleanup && len(survey.Stale.Disks) > len(survey.Stale.withAction(staleDiskTarget)) {
		log.Warningf("stale system disks are left alone: %s", survey.CleanupSkipped)
	}
}

// wipeInstallDisks wipes the stale disks the plan marked Wipe and, unless
// install.no-format is set, the target. It returns the disks it wiped.
func (i *installer) wipeInstallDisks(plan *diskPlan) ([]*diskFacts, error) {
	if !plan.survey.Cleanup {
		return nil, nil
	}
	var disks []*diskFacts
	for _, stale := range plan.survey.Stale.withAction(staleDiskWipe) {
		disks = append(disks, stale.disk)
	}
	if plan.wipeTarget {
		disks = append(disks, plan.target.disk)
	}
	if len(disks) == 0 {
		return nil, nil
	}
	for _, d := range disks {
		log.Infof("wiping %s (%s)", d.path, d.displayID())
		if err := i.wipeDisk(d); err != nil {
			return nil, &installDiskError{Reason: reasonWipeFailed, Message: fmt.Sprintf("wiping %s: %v", d.path, err), Stale: plan.survey.Stale}
		}
	}
	for _, args := range [][]string{{"trigger", "--subsystem-match=block"}, {"settle"}} {
		if err := i.runDiskCommand("udevadm", args...); err != nil {
			return nil, &installDiskError{Reason: reasonWipeFailed, Message: err.Error(), Stale: plan.survey.Stale}
		}
	}
	return disks, nil
}

// wipeDisk removes every signature from a disk and its partitions, the
// partition table included, then has the kernel drop the partitions.
func (i *installer) wipeDisk(d *diskFacts) error {
	for _, part := range d.partitions {
		if err := i.runDiskCommand("wipefs", "--all", "--force", part.KernelPath); err != nil {
			return err
		}
	}
	if err := i.runDiskCommand("sgdisk", "--zap-all", d.path); err != nil {
		if !errors.Is(err, exec.ErrNotFound) {
			return err
		}
		log.Warningf("sgdisk is not installed; wipefs and zeroing both ends still remove the GPT of %s", d.path)
	}
	if err := i.runDiskCommand("wipefs", "--all", "--force", d.path); err != nil {
		return err
	}
	for _, r := range zeroRanges(d.sizeBytes) {
		if err := i.runDiskCommand("dd", "if=/dev/zero", "of="+d.path, "bs=1M",
			fmt.Sprintf("count=%d", r.length), "iflag=count_bytes",
			fmt.Sprintf("seek=%d", r.offset), "oflag=seek_bytes",
			"conv=fsync", "status=none"); err != nil {
			return err
		}
	}
	return i.runDiskCommand("blockdev", "--rereadpt", d.path)
}

func (i *installer) runDiskCommand(name string, args ...string) error {
	out, err := i.disk.runner.Output(name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

type byteRange struct {
	offset, length int64
}

// zeroRanges are the first and the last wipeZeroBytes of a disk, merged when
// they overlap.
func zeroRanges(size int64) []byteRange {
	if size <= 0 {
		return nil
	}
	head := min(size, wipeZeroBytes)
	ranges := []byteRange{{offset: 0, length: head}}
	if tail := max(size-wipeZeroBytes, head); tail < size {
		ranges = append(ranges, byteRange{offset: tail, length: size - tail})
	}
	return ranges
}

// verifyWipe checks a fresh inventory: no disk but the target may still
// carry an old system layout, and the wiped disks must be empty.
func verifyWipe(inv *diskInventory, target string, wiped []*diskFacts) *installDiskError {
	var leftovers []string
	for _, stale := range detectStaleDisks(inv, target, nil).Disks {
		if stale.Action != staleDiskTarget {
			leftovers = append(leftovers, fmt.Sprintf("%s still carries %s", stale.Path, strings.Join(stale.Evidence, ", ")))
		}
	}
	for _, w := range wiped {
		if w.path == target {
			continue
		}
		d := inv.disk(w.path)
		switch {
		case d == nil:
			leftovers = append(leftovers, w.path+" is no longer reported by lsblk")
		case len(d.partitions) > 0:
			leftovers = append(leftovers, fmt.Sprintf("%s still has %d partitions", d.path, len(d.partitions)))
		case len(d.layout.evidence) > 0:
			leftovers = append(leftovers, fmt.Sprintf("%s still carries %s", d.path, strings.Join(d.layout.evidence, ", ")))
		}
	}
	if len(leftovers) == 0 {
		return nil
	}
	return diskErrorf(reasonWipeFailed, "stale system disks are not clean after wiping: %s", strings.Join(sortedUnique(leftovers), "; "))
}
