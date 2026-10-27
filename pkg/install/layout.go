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
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/rancher/elemental-operator/pkg/register"
)

// Old system layouts and data.
//
// A disk that once held Elemental or KubeOS carries that layout's filesystem
// labels and GPT partition names; whatever else is on a disk is someone's
// data. Automatic target selection skips disks with data; an old layout is
// not data.

// elementalLabelPattern matches the filesystem labels elemental-toolkit gives
// its partitions, and the OLD_ prefix the manual procedure used to rename
// them.
var elementalLabelPattern = regexp.MustCompile(`^(OLD_)?COS_(GRUB|OEM|RECOVERY|STATE|PERSISTENT)$`)

// elementalPartitionNames are the GPT names elemental-toolkit gives its
// partitions; two of them on one disk identify an Elemental layout.
var elementalPartitionNames = []string{"efi", "oem", "recovery", "state", "persistent"}

// elementalBIOSPartitionName is the legacy-BIOS boot partition of an Elemental
// layout. It belongs to the layout but is too generic to identify one.
const elementalBIOSPartitionName = "bios"

// kubeosLabels are the filesystem labels kbimg gives the A/B slots and the
// shared partition; two of them on one disk identify a KubeOS layout.
var kubeosLabels = []string{"ROOT-A", "ROOT-B", "PERSIST"}

// kubeosBootLabel is the ESP of a KubeOS layout. It belongs to the layout but
// is far too common to identify one.
const kubeosBootLabel = "BOOT"

// partitionTableTypes are the blkid partition table names. A partition table
// alone is not data; the partitions in it may be.
var partitionTableTypes = []string{"aix", "amiga", "atari", "bsd", "dos", "gpt", "mac", "minix", "pmbr", "sgi", "solaris", "sun", "ultrix", "unixware"}

// systemLayout is the old system layout found on one disk.
type systemLayout struct {
	elemental bool
	kubeos    bool
	// evidence lists the matching labels and PARTLABEL=<name> entries found,
	// whether or not they add up to a layout.
	evidence []string
	// foreign lists what on a stale disk is not part of its layout.
	foreign []string
}

func (l systemLayout) stale() bool {
	return l.elemental || l.kubeos
}

// owns reports whether a partition of a stale disk belongs to its layout:
// every filesystem label it has matches the layout, or it has none and an
// Elemental partition name.
func (l systemLayout) owns(part *register.BlockDevice) bool {
	labels := deviceLabels(part)
	if len(labels) == 0 {
		name := strings.ToLower(part.PartLabel)
		return l.elemental && (containsString(elementalPartitionNames, name) || name == elementalBIOSPartitionName)
	}
	for _, label := range labels {
		upper := strings.ToUpper(label)
		elemental := l.elemental && elementalLabelPattern.MatchString(upper)
		kubeos := l.kubeos && (containsString(kubeosLabels, upper) || upper == kubeosBootLabel)
		if !elemental && !kubeos {
			return false
		}
	}
	return true
}

// classifyLayout looks for an old Elemental or KubeOS layout on a disk and
// its partitions. COS_LIVE never counts.
func classifyLayout(dev *register.BlockDevice, partitions []*register.BlockDevice) systemLayout {
	var evidence []string
	elementalLabel := false
	partitionNames := map[string]bool{}
	kubeos := map[string]bool{}
	for _, d := range append([]*register.BlockDevice{dev}, partitions...) {
		for _, label := range deviceLabels(d) {
			upper := strings.ToUpper(label)
			switch {
			case elementalLabelPattern.MatchString(upper):
				elementalLabel = true
				evidence = append(evidence, label)
			case containsString(kubeosLabels, upper):
				kubeos[upper] = true
				evidence = append(evidence, label)
			}
		}
		if name := strings.ToLower(d.PartLabel); d.Type == blockTypePart && containsString(elementalPartitionNames, name) {
			partitionNames[name] = true
			evidence = append(evidence, "PARTLABEL="+name)
		}
	}
	layout := systemLayout{
		elemental: elementalLabel || len(partitionNames) >= 2,
		kubeos:    len(kubeos) >= 2,
		evidence:  sortedUnique(evidence),
	}
	if layout.stale() {
		layout.foreign = foreignContent(dev, partitions, layout)
	}
	return layout
}

// foreignContent lists what on a stale disk does not belong to its layout.
func foreignContent(dev *register.BlockDevice, partitions []*register.BlockDevice, layout systemLayout) []string {
	var foreign []string
	if len(deviceLabels(dev)) == 0 || !layout.owns(dev) {
		for _, signature := range dataSignatures(dev) {
			foreign = append(foreign, signature+" on "+dev.Path)
		}
	}
	if dev.SignatureError != "" {
		foreign = append(foreign, "unreadable signatures on "+dev.Path)
	}
	for _, part := range partitions {
		switch {
		case part.SignatureError != "":
			foreign = append(foreign, describePartition(part))
		case !layout.owns(part):
			foreign = append(foreign, describePartition(part))
		}
	}
	return foreign
}

// dataFindings is what makes a disk look like someone's data: on a stale
// disk whatever is not its layout, elsewhere every signature but the
// partition table and every partition.
func dataFindings(dev *register.BlockDevice, partitions []*register.BlockDevice, layout systemLayout) []string {
	if layout.stale() {
		return layout.foreign
	}
	data := dataSignatures(dev)
	if dev.SignatureError != "" {
		data = append(data, "unreadable signatures")
	}
	for _, part := range partitions {
		data = append(data, describePartition(part))
	}
	return data
}

// dataSignatures lists the signatures on dev other than a partition table:
// LVM2_member, linux_raid_member, crypto_LUKS, filesystems.
func dataSignatures(dev *register.BlockDevice) []string {
	var types []string
	if dev.FSType != "" && !containsString(partitionTableTypes, strings.ToLower(dev.FSType)) {
		types = append(types, dev.FSType)
	}
	for _, signature := range dev.Signatures {
		if !containsString(partitionTableTypes, strings.ToLower(signature.Type)) {
			types = append(types, signature.Type)
		}
	}
	return sortedUnique(types)
}

func describePartition(part *register.BlockDevice) string {
	var facts []string
	if signatures := dataSignatures(part); len(signatures) > 0 {
		facts = append(facts, strings.Join(signatures, "+"))
	}
	if labels := deviceLabels(part); len(labels) > 0 {
		facts = append(facts, "label "+strings.Join(labels, "+"))
	}
	if part.PartLabel != "" {
		facts = append(facts, "name "+part.PartLabel)
	}
	if part.SignatureError != "" {
		facts = append(facts, "unreadable signatures")
	}
	if len(facts) == 0 {
		facts = append(facts, "no signature")
	}
	return fmt.Sprintf("partition %s (%s)", part.Path, strings.Join(facts, ", "))
}

// deviceLabels are the filesystem labels lsblk and wipefs report for dev.
func deviceLabels(dev *register.BlockDevice) []string {
	labels := []string{dev.Label}
	for _, signature := range dev.Signatures {
		labels = append(labels, signature.Label)
	}
	return sortedUnique(labels)
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
