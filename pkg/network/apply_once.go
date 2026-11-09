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
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rancher/yip/pkg/schema"
)

const (
	// appliedMarkerPath records which set of profiles the applicator last put
	// in place. It sits under /etc/NetworkManager next to the profiles, so it
	// survives a reboot exactly when they do.
	appliedMarkerPath = "/etc/NetworkManager/elemental-network.applied"
	// appliedRecorderName names the step that writes appliedMarkerPath.
	appliedRecorderName = "Record applied network config"
	// appliedThisBootPath tells the boot stage that the initramfs stage of
	// this boot wrote the profiles. /run is carried over from the initramfs.
	appliedThisBootPath = "/run/elemental/network-config-applied"
	// relabelStage runs once the SELinux policy is loaded.
	relabelStage = "boot"
	// relabelName names the step that relabels what the initramfs wrote.
	relabelName = "Relabel applied network config"
)

// applyOnce makes the applicator write the profiles once instead of on every
// boot.
//
// The applicator is installed to /oem and runs in the initramfs stage of every
// boot. Writing the profiles unconditionally there overwrites them each time:
// a change made on the host with nmcli is reverted by the next reboot, and a
// profile that was deleted, for example when a NIC was moved into a bond,
// comes back and competes with its replacement for the device.
//
// The profiles are therefore written only when the marker does not name this exact
// set of profiles, and the marker is written once all of them are on disk as
// applied. Where /etc/NetworkManager is persistent, the profiles are thus
// applied on the first boot after an install or a reset and belong to the host
// from then on. Where it is not, as on the stock Elemental layout, the marker
// is lost together with the profiles and they are written on every boot.
// Deleting the marker and rebooting restores the profiles from the
// install.
//
// A set that cannot be checked as written, because a file is encoded or its
// content is rendered as a template by yip, is never recorded and keeps being
// written on every boot.
//
// The initramfs stage runs before the SELinux policy is loaded, so a profile
// it creates is left unlabeled. NetworkManager can read such a file but not
// replace or delete it: nmcli reports success and the file stays. On a boot
// that applied the profiles, the boot stage therefore relabels them.
func applyOnce(applicator schema.YipConfig) schema.YipConfig {
	var files []schema.File
	for _, step := range applicator.Stages[applicatorStage] {
		files = append(files, step.Files...)
	}
	if len(files) == 0 {
		return applicator
	}
	for _, f := range files {
		if f.Encoding != "" {
			return applicator
		}
	}
	// Configurators may build the list from a map. Sort it so that the same
	// profiles always yield the same applicator and the same stamp.
	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	// The sha256sum check list doubles as the identity of the set.
	var sums strings.Builder
	for _, f := range files {
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256([]byte(f.Content)), f.Path)
	}
	stamp := fmt.Sprintf("%x", sha256.Sum256([]byte(sums.String())))

	notApplied := fmt.Sprintf(`[ "$(cat %s 2>/dev/null)" != "%s" ]`, appliedMarkerPath, stamp)
	record := fmt.Sprintf("sha256sum -c --quiet <<'EOF' || exit 1\n%sEOF\nprintf '%%s\\n' %s > %s\nmkdir -p %s\n: > %s",
		sums.String(), stamp, appliedMarkerPath, filepath.Dir(appliedThisBootPath), appliedThisBootPath)

	relabelPaths := make([]string, 0, len(files)+1)
	for _, f := range files {
		relabelPaths = append(relabelPaths, shellQuote(f.Path))
	}
	relabelPaths = append(relabelPaths, appliedMarkerPath)
	relabel := fmt.Sprintf("if [ -d /sys/fs/selinux ] && command -v restorecon >/dev/null 2>&1; then restorecon -i %s; fi",
		strings.Join(relabelPaths, " "))

	applicator.Stages = map[string][]schema.Stage{
		applicatorStage: {
			{Name: applicatorName, If: notApplied, Files: files},
			{Name: appliedRecorderName, If: notApplied, Commands: []string{record}},
		},
		relabelStage: {
			{Name: relabelName, If: fmt.Sprintf("[ -f %s ]", appliedThisBootPath), Commands: []string{relabel}},
		},
	}
	return applicator
}

// shellQuote quotes s as a single word for sh. Profile file names come from
// connection names and may contain spaces.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
