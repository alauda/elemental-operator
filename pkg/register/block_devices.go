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
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// BlockDevice is one node of the lsblk tree as the install path needs it: the
// raw facts plus the management identity canonicalDeviceID derives, so the
// installer, the storage observer and the operator call a disk the same name.
type BlockDevice struct {
	// Path is lsblk PATH (/dev/sdb, /dev/mapper/…); KernelPath is
	// /dev/<kernel name> (/dev/sdb, /dev/dm-0).
	Path       string
	KernelPath string
	// Type is the lower-case lsblk TYPE: disk, part, rom, loop, lvm, crypt,
	// mpath, raid1, ...
	Type string
	// ID is the canonical identity. It is empty when the device has none or
	// shares it with another device on the host; IDProblem then says why.
	ID          string
	IDProblem   string
	StablePaths []string
	SizeBytes   int64
	ReadOnly    bool
	Rotational  bool
	Removable   bool
	Model       string
	Serial      string
	WWN         string
	Transport   string
	FSType      string
	Label       string
	UUID        string
	PTType      string
	PartType    string
	PartLabel   string
	PartUUID    string
	MountPoints []string
	// Parents and Children are the lsblk tree neighbours, by Path.
	Parents  []string
	Children []string
	// TopologyAmbiguous is set when lsblk listed the path twice with
	// different facts.
	TopologyAmbiguous bool
	// Signatures are what `wipefs --no-act` found on a disk, partition or
	// multipath map. SignatureError is set instead when wipefs failed; the
	// device must then be assumed to carry data.
	Signatures     []BlockSignature
	SignatureError string
}

// BlockSignature is one wipefs finding.
type BlockSignature struct {
	// Type is the blkid name: gpt, PMBR, dos, ext4, vfat, LVM2_member, ...
	Type  string
	Label string
}

// BlockSnapshot is every block device lsblk reported at one moment, sorted by
// Path.
type BlockSnapshot struct {
	Devices []BlockDevice
}

// Find returns the device whose Path or KernelPath is path, or nil.
func (s *BlockSnapshot) Find(path string) *BlockDevice {
	if s == nil {
		return nil
	}
	path = filepath.Clean(path)
	for i := range s.Devices {
		if s.Devices[i].Path == path || s.Devices[i].KernelPath == path {
			return &s.Devices[i]
		}
	}
	return nil
}

// BlockCollectorOptions replaces probes of the collector. Nil members keep the
// defaults the storage observer uses.
type BlockCollectorOptions struct {
	// Run executes lsblk and wipefs. The default bounds each command with a
	// timeout.
	Run func(name string, args ...string) ([]byte, error)
	// ByIDPaths maps resolved device paths to their /dev/disk/by-id aliases.
	ByIDPaths func() (map[string][]string, error)
	// BusProbe returns the sysfs evidence for a kernel name (sda): the bus
	// ("virtio", "ata") and the SCSI host driver.
	BusProbe func(kernelName string) (bus, driver string)
}

// CollectBlockDevices takes one snapshot of the host's block devices with the
// storage observer's collector.
func CollectBlockDevices(opts BlockCollectorOptions) (*BlockSnapshot, error) {
	c := newObservedStorageCollector()
	if opts.Run != nil {
		c.run = opts.Run
	}
	if opts.ByIDPaths != nil {
		c.byIDPaths = opts.ByIDPaths
	}
	if opts.BusProbe != nil {
		probe := opts.BusProbe
		c.busProbe = func(device lsblkDevice) busProbe {
			bus, driver := probe(sysfsKernelName(device))
			return busProbe{bus: bus, driver: driver}
		}
	}
	return c.blockSnapshot()
}

func (c *observedStorageCollector) blockSnapshot() (*BlockSnapshot, error) {
	inventory, err := c.inventory()
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(inventory.records))
	for path := range inventory.records {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	// An identity two devices share identifies neither. Unlike the observer
	// report, multipath backing paths are counted: in a live environment they
	// are disks an install could touch, and a LUN seen through two paths must
	// not look like two identified disks.
	counts := map[string]int{}
	for _, path := range paths {
		if id := inventory.ids[path]; id != "" && isObservableLogicalDevice(inventory.records[path].device.Type) {
			counts[id]++
		}
	}

	snapshot := &BlockSnapshot{Devices: make([]BlockDevice, 0, len(paths))}
	for _, path := range paths {
		record := inventory.records[path]
		device := newBlockDevice(path, record, inventory)
		switch {
		case device.ID != "" && counts[device.ID] > 1:
			device.ID = ""
			device.IDProblem = "identity is shared with another device on this host"
		case device.ID == "":
			device.IDProblem = unidentifiedReason(record.device, inventory.buses[path], inventory.probes[path])
		}
		if isObservableLogicalDevice(record.device.Type) {
			device.Signatures, device.SignatureError = c.wipefsSignatures(path)
		}
		snapshot.Devices = append(snapshot.Devices, device)
	}
	return snapshot, nil
}

func newBlockDevice(path string, record *blockRecord, inventory *blockInventory) BlockDevice {
	raw := record.device
	kernelPath := filepath.Clean(strings.TrimSpace(raw.KName))
	if !strings.HasPrefix(kernelPath, "/dev/") {
		kernelPath = path
	}
	mounts := make([]string, 0, len(raw.MountPoints))
	for _, mount := range raw.MountPoints {
		if value := strings.TrimSpace(mount); value != "" {
			mounts = append(mounts, filepath.Clean(value))
		}
	}
	return BlockDevice{
		Path:              path,
		KernelPath:        kernelPath,
		Type:              strings.ToLower(strings.TrimSpace(raw.Type)),
		ID:                inventory.ids[path],
		StablePaths:       stablePathsForDevice(raw, inventory.byIDPaths),
		SizeBytes:         int64(raw.Size),
		ReadOnly:          bool(raw.ReadOnly),
		Rotational:        bool(raw.Rotational),
		Removable:         bool(raw.Removable),
		Model:             strings.TrimSpace(raw.Model),
		Serial:            strings.TrimSpace(raw.Serial),
		WWN:               strings.TrimSpace(raw.WWN),
		Transport:         strings.TrimSpace(raw.Transport),
		FSType:            strings.TrimSpace(raw.FSType),
		Label:             strings.TrimSpace(raw.Label),
		UUID:              strings.TrimSpace(raw.UUID),
		PTType:            strings.TrimSpace(raw.PTType),
		PartType:          strings.TrimSpace(raw.PartType),
		PartLabel:         strings.TrimSpace(raw.PartLabel),
		PartUUID:          strings.TrimSpace(raw.PartUUID),
		MountPoints:       sortedUniqueStrings(mounts),
		Parents:           sortedSetMembers(record.parents),
		Children:          sortedSetMembers(record.children),
		TopologyAmbiguous: record.topologyAmbiguous,
	}
}

// wipefsSignatures lists the signatures wipefs would erase from path, without
// erasing them.
func (c *observedStorageCollector) wipefsSignatures(path string) ([]BlockSignature, string) {
	data, err := c.run("wipefs", "--json", "--no-act", path)
	if err != nil {
		return nil, err.Error()
	}
	decoded := wipefsOutput{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			return nil, "decoding wipefs JSON: " + err.Error()
		}
	}
	seen := map[BlockSignature]bool{}
	signatures := []BlockSignature{}
	for _, item := range decoded.Signatures {
		signature := BlockSignature{Type: stringValue(item["type"]), Label: stringValue(item["label"])}
		if signature.Type == "" || seen[signature] {
			continue
		}
		seen[signature] = true
		signatures = append(signatures, signature)
	}
	sort.Slice(signatures, func(i, j int) bool {
		if signatures[i].Type != signatures[j].Type {
			return signatures[i].Type < signatures[j].Type
		}
		return signatures[i].Label < signatures[j].Label
	})
	return signatures, ""
}

// NormalizeDeviceID brings a device ID written by a person, or sent back by
// the operator, into the form canonicalDeviceID produces: lower case, hex
// payloads without 0x or separators, serial parts with the same character set.
func NormalizeDeviceID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	prefix, value, found := strings.Cut(id, ":")
	if !found {
		return id
	}
	switch prefix {
	case "wwn", "wwid", "nvme-eui", "nvme-nguid":
		if hex := canonicalHexIdentity(value); hex != "" {
			return prefix + ":" + hex
		}
	case "partuuid":
		if uuid := canonicalPartUUID(value); uuid != "" {
			return prefix + ":" + uuid
		}
	case "serial":
		parts := strings.Split(value, ":")
		for i := range parts {
			parts[i] = canonicalSerialPart(parts[i])
		}
		return prefix + ":" + strings.Join(parts, ":")
	}
	return id
}

// UnidentifiedDeviceID is the diagnostic ID reported for a device without a
// canonical identity. Nothing can select a device by it.
func UnidentifiedDeviceID(path string) string {
	return "unidentified:" + sanitizeDiagnosticID(path)
}

func sortedSetMembers(set map[string]struct{}) []string {
	members := make([]string, 0, len(set))
	for member := range set {
		members = append(members, member)
	}
	sort.Strings(members)
	return members
}
