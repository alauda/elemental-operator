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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBlockCollectorResolvesMultipathStableIDThroughKernelName(t *testing.T) {
	const (
		mapPath    = "/dev/mapper/mpatha"
		kernelPath = "/dev/dm-0"
		wwid       = "3500000007a02692"
	)

	mapDevice := map[string]any{
		"name": mapPath, "path": mapPath, "kname": kernelPath, "type": "mpath", "size": int64(16 << 30),
		"wwn": nil, "fstype": nil, "uuid": nil, "mountpoints": []any{nil},
	}
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{mapDevice}})
	if err != nil {
		t.Fatal(err)
	}
	stablePath := "/dev/disk/by-id/dm-uuid-mpath-" + wwid
	collector := &blockCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{kernelPath: {stablePath}}, nil
		},
	}

	snapshot, err := collector.blockSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Devices) != 1 {
		t.Fatalf("devices = %#v", snapshot.Devices)
	}
	device := snapshot.Devices[0]
	if device.Path != mapPath || device.ID != "wwid:"+wwid || device.IDProblem != "" {
		t.Fatalf("multipath map did not resolve through KNAME: %#v", device)
	}
	if !reflect.DeepEqual(device.StablePaths, []string{stablePath}) {
		t.Fatalf("stable paths = %#v", device.StablePaths)
	}
}

func TestBlockCollectorFailsClosedWhenIdentityOrInspectionIsUnknown(t *testing.T) {
	disk := diskFixture("/dev/sdb")
	disk["wwn"] = nil
	disk["serial"] = "SHARED"
	disk["model"] = "SAN"
	disk["tran"] = "fc"
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{disk}})
	if err != nil {
		t.Fatal(err)
	}
	collector := &blockCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return nil, errors.New("wipefs failed")
		},
		byIDPaths: func() (map[string][]string, error) { return nil, errors.New("udev unavailable") },
	}
	snapshot, err := collector.blockSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	device := snapshot.Devices[0]
	// A serial behind a fabric transport is no identity, and the problem says
	// which fact is missing.
	if device.ID != "" || !strings.Contains(device.IDProblem, `bus not proven local (transport="fc", driver="")`) {
		t.Fatalf("unexpected fail-closed device: %#v", device)
	}
	// A wipefs failure is reported, never read as "no signatures".
	if device.Signatures != nil || device.SignatureError != "wipefs failed" {
		t.Fatalf("wipefs failure was not reported: %#v", device)
	}
}

func TestCanonicalDeviceIdentity(t *testing.T) {
	tests := []struct {
		name   string
		device lsblkDevice
		paths  []string
		probe  busProbe
		want   string
	}{
		{name: "wwn alias", device: lsblkDevice{Type: "disk"}, paths: []string{"/dev/disk/by-id/wwn-0x5000-C500"}, want: "wwn:5000c500"},
		{name: "nvme eui", device: lsblkDevice{Type: "disk"}, paths: []string{"/dev/disk/by-id/nvme-eui.00112233"}, want: "nvme-eui:00112233"},
		{name: "partition", device: lsblkDevice{Type: "part", PartUUID: "CB6A-04B2"}, want: "partuuid:cb6a-04b2"},
		{name: "multipath", device: lsblkDevice{Type: "mpath"}, paths: []string{"/dev/disk/by-id/dm-uuid-mpath-3600508b400"}, want: "wwid:3600508b400"},
		{name: "multipath non-hex WWID fails closed", device: lsblkDevice{Type: "mpath", Path: "/dev/mapper/mpatha"}, paths: []string{"/dev/disk/by-id/dm-uuid-mpath-0QEMU_DATA1"}, want: ""},
		{name: "local serial fallback", device: lsblkDevice{Type: "disk", Transport: "virtio", Model: "Data Disk", Serial: "SER 1"}, want: "serial:data_disk:ser_1"},
		{name: "malformed wwn alias falls through", device: lsblkDevice{Type: "disk", Transport: "virtio", Serial: "c2793cff25c3aba3d4b7"}, paths: []string{"/dev/disk/by-id/wwn-0xEMU_HARDDISK_c2793cff25c3aba3d4b7", "/dev/disk/by-id/virtio-c2793cff25c3aba3d4b7"}, want: "serial:virtio:c2793cff25c3aba3d4b7"},
		{name: "malformed wwn alias alone fails closed", device: lsblkDevice{Type: "disk"}, paths: []string{"/dev/disk/by-id/wwn-0xEMU_HARDDISK_c2793cff25c3aba3d4b7"}, want: ""},
		{name: "virtio by-id alias without transport or model", device: lsblkDevice{Type: "disk"}, paths: []string{"/dev/disk/by-id/virtio-90a775711a809a8a75e5"}, want: "serial:virtio:90a775711a809a8a75e5"},
		{name: "wwn alias outranks virtio alias", device: lsblkDevice{Type: "disk"}, paths: []string{"/dev/disk/by-id/virtio-90a7", "/dev/disk/by-id/wwn-0x5000c500"}, want: "wwn:5000c500"},
		{name: "virtio alias without serial fails closed", device: lsblkDevice{Type: "disk"}, paths: []string{"/dev/disk/by-id/virtio-"}, want: ""},
		{name: "virtio without model falls back to transport", device: lsblkDevice{Type: "disk", Transport: "virtio", Serial: "90a775711a809a8a75e5"}, want: "serial:virtio:90a775711a809a8a75e5"},
		{name: "virtio without serial fails closed", device: lsblkDevice{Type: "disk", Transport: "virtio", Model: "Data Disk"}, want: ""},
		{name: "unusable model falls back to transport", device: lsblkDevice{Type: "disk", Transport: "virtio", Model: "???", Serial: "SER 1"}, want: "serial:virtio:ser_1"},
		{name: "spi is a local bus", device: lsblkDevice{Type: "disk", Transport: "spi", Model: "QEMU HARDDISK", Serial: "07a8489045cc024d75ee"}, want: "serial:qemu_harddisk:07a8489045cc024d75ee"},
		{name: "absent transport is not evidence", device: lsblkDevice{Type: "disk", Transport: "", Model: "QEMU HARDDISK", Serial: "50014ee01a2b3c4d"}, want: ""},
		{name: "virtio-scsi driver proves a local bus", device: lsblkDevice{Type: "disk", Model: "QEMU HARDDISK", Serial: "50014ee01a2b3c4d"}, probe: busProbe{driver: "virtio_scsi"}, want: "serial:qemu_harddisk:50014ee01a2b3c4d"},
		{name: "virtio ancestor proves a local bus", device: lsblkDevice{Type: "disk", Serial: "90a7"}, probe: busProbe{bus: "virtio"}, want: "serial:virtio:90a7"},
		{name: "libata ancestor proves a local bus", device: lsblkDevice{Type: "disk", Model: "QEMU HARDDISK", Serial: "c279"}, probe: busProbe{bus: "ata", driver: "ahci"}, want: "serial:qemu_harddisk:c279"},
		{name: "ata alias proves a local bus", device: lsblkDevice{Type: "disk", Model: "QEMU HARDDISK", Serial: "c279"}, paths: []string{"/dev/disk/by-id/ata-QEMU_HARDDISK_c279"}, want: "serial:qemu_harddisk:c279"},
		{name: "fabric HBA driver is not evidence", device: lsblkDevice{Type: "disk", Model: "LUN", Serial: "SER1"}, probe: busProbe{driver: "qla2xxx"}, want: ""},
		{name: "shared serial rejected", device: lsblkDevice{Type: "disk", Transport: "iscsi", Model: "LUN", Serial: "SER1"}, want: ""},
		{name: "shared serial rejected even with a local-looking driver", device: lsblkDevice{Type: "disk", Transport: "iscsi", Model: "LUN", Serial: "SER1"}, probe: busProbe{driver: "iscsi_tcp"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalDeviceID(tt.device, tt.paths, localBus(tt.device, tt.paths, tt.probe)); got != tt.want {
				t.Fatalf("canonicalDeviceID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBlockCollectorFlexibleLSBLKValues(t *testing.T) {
	disk := diskFixture("/dev/nvme0n1")
	disk["size"] = "1099511627776"
	disk["rota"] = "1"
	disk["rm"] = "0"
	disk["tran"] = "nvme"
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{disk}})
	if err != nil {
		t.Fatal(err)
	}
	collector := &blockCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{"/dev/nvme0n1": {"/dev/disk/by-id/nvme-eui.0000000000000099"}}, nil
		},
	}
	snapshot, err := collector.blockSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	device := snapshot.Devices[0]
	if device.SizeBytes != 1099511627776 || !device.Rotational || device.Removable {
		t.Fatalf("unexpected flexible values: %#v", device)
	}
}

func TestBlockCollectorCollectionErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		data []byte
		err  error
	}{
		{name: "lsblk command", err: errors.New("lsblk failed")},
		{name: "invalid lsblk JSON", data: []byte("not-json")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			collector := &blockCollector{
				run:       func(string, ...string) ([]byte, error) { return tt.data, tt.err },
				byIDPaths: func() (map[string][]string, error) { return nil, nil },
			}
			if _, err := collector.blockSnapshot(); err == nil {
				t.Fatal("expected collection error")
			}
		})
	}
}

func TestBlockCollectorLSBLKArgsRemainUtilLinux237Compatible(t *testing.T) {
	joined := strings.Join(lsblkStorageArgs, ",")
	for _, unsupported := range []string{"PARTN", "START", "OPTIONS"} {
		for _, column := range strings.Split(joined, ",") {
			if column == unsupported {
				t.Fatalf("%s is not supported by util-linux 2.37.4 used in the Elemental base image", unsupported)
			}
		}
	}
	for _, required := range []string{"KNAME", "PARTUUID", "PARTLABEL", "PKNAME", "LOG-SEC"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("lsblk columns do not include %s: %s", required, joined)
		}
	}
}

// The fork's message numbers are on the wire: hosts and operators of
// different releases negotiate by them, so they never move.
func TestForkProtocolMessageNumbers(t *testing.T) {
	want := map[MessageType]byte{
		MsgObservedNetworkConfig: 13,
		MsgLiveInstallSession:    16,
		MsgInstallHold:           17,
		MsgInstallReport:         18,
	}
	for msg, number := range want {
		if byte(msg) != number {
			t.Errorf("%s = %d, want %d", msg, byte(msg), number)
		}
	}
	if MsgLast != MsgInstallReport {
		t.Errorf("MsgLast = %d, want %d", MsgLast, MsgInstallReport)
	}
	// 14 and 15 carried the removed storage observer. Peers of older releases
	// may still send them, so no message may ever be given those numbers again.
	for _, number := range []byte{14, 15} {
		if name := MessageType(number).String(); name != "Unknown" {
			t.Errorf("message %d is reserved for the removed storage observer, but is named %q", number, name)
		}
	}
}

func TestBlockCollectorEnumeratesStablePathsForDisksAndPartitions(t *testing.T) {
	root := t.TempDir()
	deviceDir := filepath.Join(root, "dev")
	byIDDir := filepath.Join(deviceDir, "disk", "by-id")
	if err := os.MkdirAll(byIDDir, 0o755); err != nil {
		t.Fatal(err)
	}
	diskPath := filepath.Join(deviceDir, "sdb")
	partitionPath := filepath.Join(deviceDir, "sdb1")
	for _, path := range []string{diskPath, partitionPath} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{
		"wwn-data":       diskPath,
		"wwn-data-part1": partitionPath,
	} {
		if err := os.Symlink(target, filepath.Join(byIDDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := collectByIDPathsFrom(byIDDir)
	if err != nil {
		t.Fatal(err)
	}
	resolvedDisk, err := filepath.EvalSymlinks(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	resolvedPartition, err := filepath.EvalSymlinks(partitionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths[resolvedDisk], []string{filepath.Join(byIDDir, "wwn-data")}) {
		t.Fatalf("disk stable paths = %#v", paths[resolvedDisk])
	}
	if !reflect.DeepEqual(paths[resolvedPartition], []string{filepath.Join(byIDDir, "wwn-data-part1")}) {
		t.Fatalf("partition stable paths = %#v", paths[resolvedPartition])
	}
}

func diskFixture(path string) map[string]any {
	return map[string]any{
		"name": path, "path": path, "type": "disk", "size": int64(500 * 1024 * 1024 * 1024),
		"start": 0, "log-sec": 512, "ro": false, "rota": false, "rm": false,
		"model": "ExampleDataDisk", "serial": "DATA-SERIAL", "wwn": "0x5000c50000000001", "tran": "sata",
		"fstype": nil, "uuid": nil, "label": nil, "mountpoints": []any{nil}, "options": []any{nil},
		"pttype": nil, "parttype": nil, "partuuid": nil, "pkname": nil,
	}
}

func partitionFixture(path, partUUID, filesystem, uuid string, mounts []any) map[string]any {
	if mounts == nil {
		mounts = []any{nil}
	}
	return map[string]any{
		"name": path, "path": path, "type": "part", "size": int64(100 << 30), "start": 1024, "log-sec": 512,
		"fstype": filesystem, "uuid": uuid, "mountpoints": mounts, "options": []any{"rw,noatime"}, "partuuid": partUUID,
	}
}

func containsText(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func findBlockDevice(t *testing.T, snapshot *BlockSnapshot, path string) *BlockDevice {
	t.Helper()
	device := snapshot.Find(path)
	if device == nil {
		t.Fatalf("%s is missing from the snapshot: %#v", path, snapshot.Devices)
	}
	return device
}

// TestBlockCollectorReproducesCustomerQEMUEnvironment replays a customer's real
// device tree, captured from lsblk, /dev/disk/by-id and multipathd. It is the
// local stand-in for that environment so identity behaviour is pinned without a VM.
//
// The environment is entirely QEMU-backed and shows three separate hazards:
// virtio-blk publishing nothing but a serial, single-path disks that
// find_multipaths no wrapped into maps, and udev minting wwn-0x aliases by
// chopping the first character off a WWID that has no designator-type digit.
func TestBlockCollectorReproducesCustomerQEMUEnvironment(t *testing.T) {
	virtioDisk := func(path, serial string, sizeGiB int64) map[string]any {
		return map[string]any{
			"name": path, "path": path, "kname": path, "type": "disk",
			"size": sizeGiB << 30, "start": 0, "log-sec": 512,
			"ro": false, "rota": true, "rm": false,
			// MODEL is "virtio" only because a udev rule supplies it; TRAN and WWN
			// are empty for every virtio-blk disk no matter what udev does.
			"model": "virtio", "serial": serial, "wwn": nil, "tran": nil,
			"mountpoints": []any{nil}, "options": []any{nil},
		}
	}

	qemuMaps := []struct {
		alias, kname, member, wwid, memberSerial, memberTran string
		memberAliases                                        []string
		sizeGiB                                              int64
	}{{
		alias: "mpatha", kname: "/dev/dm-0", member: "/dev/sda", sizeGiB: 500,
		wwid: "0QEMU_QEMU_HARDDISK_50014ee01a2b3c4d", memberSerial: "50014ee01a2b3c4d", memberTran: "",
		memberAliases: []string{"scsi-SQEMU_QEMU_HARDDISK_50014ee01a2b3c4d"},
	}, {
		alias: "mpathb", kname: "/dev/dm-1", member: "/dev/sdb", sizeGiB: 80,
		wwid: "0QEMU_QEMU_HARDDISK_07a8489045cc024d75ee", memberSerial: "07a8489045cc024d75ee", memberTran: "spi",
		memberAliases: []string{"scsi-SQEMU_QEMU_HARDDISK_07a8489045cc024d75ee"},
	}, {
		// sdc is ATA-backed, so multipathd emits a WWID with no designator-type
		// digit at all. udev then builds wwn-0xEMU_HARDDISK_... from it.
		alias: "mpathc", kname: "/dev/dm-2", member: "/dev/sdc", sizeGiB: 100,
		wwid: "QEMU_HARDDISK_c2793cff25c3aba3d4b7", memberSerial: "c2793cff25c3aba3d4b7", memberTran: "ata",
		memberAliases: []string{
			"ata-QEMU_HARDDISK_c2793cff25c3aba3d4b7",
			"scsi-0ATA_QEMU_HARDDISK_c2793cff25c3aba3d4b7",
			"scsi-1ATA_QEMU_HARDDISK_c2793cff25c3aba3d4b7",
			"scsi-SATA_QEMU_HARDDISK_c2793cff25c3aba3d4b7",
		},
	}}

	blockdevices := []any{
		virtioDisk("/dev/vda", "5000c500142b3c4d", 500),
		virtioDisk("/dev/vdb", "90a775711a809a8a75e5", 500),
	}
	byIDPaths := map[string][]string{
		"/dev/vda": {"/dev/disk/by-id/virtio-5000c500142b3c4d"},
		"/dev/vdb": {"/dev/disk/by-id/virtio-90a775711a809a8a75e5"},
	}
	for _, m := range qemuMaps {
		mapPath := "/dev/mapper/" + m.alias
		mapped := map[string]any{
			"name": mapPath, "path": mapPath, "kname": m.kname, "type": "mpath",
			"size": m.sizeGiB << 30, "mountpoints": []any{nil}, "options": []any{nil},
		}
		member := map[string]any{
			"name": m.member, "path": m.member, "kname": m.member, "type": "disk",
			"size": m.sizeGiB << 30, "start": 0, "log-sec": 512,
			"ro": false, "rota": true, "rm": false,
			"model": "QEMU HARDDISK", "serial": m.memberSerial, "wwn": nil, "tran": m.memberTran,
			"fstype": "mpath_member", "mountpoints": []any{nil}, "options": []any{nil},
			"children": []any{mapped},
		}
		blockdevices = append(blockdevices, member)

		for _, alias := range m.memberAliases {
			byIDPaths[m.member] = append(byIDPaths[m.member], "/dev/disk/by-id/"+alias)
		}
		byIDPaths[m.kname] = []string{
			"/dev/disk/by-id/dm-name-" + m.alias,
			"/dev/disk/by-id/dm-uuid-mpath-" + m.wwid,
			"/dev/disk/by-id/scsi-" + m.wwid,
			// udev's wwn-0x alias: the WWID with its first character removed.
			"/dev/disk/by-id/wwn-0x" + m.wwid[1:],
		}
	}

	lsblk, err := json.Marshal(map[string]any{"blockdevices": blockdevices})
	if err != nil {
		t.Fatal(err)
	}
	collector := &blockCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) { return byIDPaths, nil },
	}

	snapshot, err := collector.blockSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Both virtio disks resolve through their by-id serial alias.
	for path, want := range map[string]string{
		"/dev/vda": "serial:virtio:5000c500142b3c4d",
		"/dev/vdb": "serial:virtio:90a775711a809a8a75e5",
	} {
		if device := findBlockDevice(t, snapshot, path); device.ID != want {
			t.Fatalf("%s was not identified as %s: %#v", path, want, device)
		}
	}

	// Every map still fails closed. mpathc is the one that would slip through a
	// rule keyed only on "not hexadecimal": udev's wwn-0xEMU_HARDDISK_... alias
	// must not be mistaken for a usable WWN.
	for _, m := range qemuMaps {
		mapped := findBlockDevice(t, snapshot, "/dev/mapper/"+m.alias)
		if mapped.ID != "" || !strings.Contains(mapped.IDProblem, "multipath WWID is not hexadecimal") {
			t.Fatalf("map %s was expected to fail closed: %#v", m.alias, mapped)
		}
	}
}

// TestBlockCollectorCustomerEnvironmentWithoutFakeMultipathMaps replays the same
// customer disks as they appear once the OS image stops shipping
// find_multipaths no, so single-path QEMU disks are no longer wrapped into maps.
//
// The three disks differ only in the transport lsblk manages to derive, and that
// alone decides whether the serial fallback is admissible.
func TestBlockCollectorCustomerEnvironmentWithoutFakeMultipathMaps(t *testing.T) {
	disks := []struct {
		path, serial, transport string
		aliases                 []string
	}{
		// Behind a virtio-scsi controller, which lsblk cannot classify. No
		// transport evidence, so this disk stays unmanageable by design.
		{"/dev/sda", "50014ee01a2b3c4d", "", []string{"scsi-SQEMU_QEMU_HARDDISK_50014ee01a2b3c4d"}},
		// Behind an emulated SPI controller: a local bus, never a fabric.
		{"/dev/sdb", "07a8489045cc024d75ee", "spi", []string{"scsi-SQEMU_QEMU_HARDDISK_07a8489045cc024d75ee"}},
		// ATA-backed, which the pre-existing allowlist already accepted.
		{"/dev/sdc", "c2793cff25c3aba3d4b7", "ata", []string{
			"ata-QEMU_HARDDISK_c2793cff25c3aba3d4b7",
			"scsi-0ATA_QEMU_HARDDISK_c2793cff25c3aba3d4b7",
			"scsi-1ATA_QEMU_HARDDISK_c2793cff25c3aba3d4b7",
			"scsi-SATA_QEMU_HARDDISK_c2793cff25c3aba3d4b7",
		}},
	}

	blockdevices := []any{}
	byIDPaths := map[string][]string{}
	for _, d := range disks {
		blockdevices = append(blockdevices, map[string]any{
			"name": d.path, "path": d.path, "kname": d.path, "type": "disk",
			"size": int64(100 << 30), "start": 0, "log-sec": 512,
			"ro": false, "rota": true, "rm": false,
			"model": "QEMU HARDDISK", "serial": d.serial, "wwn": nil, "tran": d.transport,
			"mountpoints": []any{nil}, "options": []any{nil},
		})
		for _, alias := range d.aliases {
			byIDPaths[d.path] = append(byIDPaths[d.path], "/dev/disk/by-id/"+alias)
		}
	}

	lsblk, err := json.Marshal(map[string]any{"blockdevices": blockdevices})
	if err != nil {
		t.Fatal(err)
	}
	collector := &blockCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) { return byIDPaths, nil },
	}

	snapshot, err := collector.blockSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	for id, wantPath := range map[string]string{
		"serial:qemu_harddisk:07a8489045cc024d75ee": "/dev/sdb",
		"serial:qemu_harddisk:c2793cff25c3aba3d4b7": "/dev/sdc",
	} {
		if device := findBlockDevice(t, snapshot, wantPath); device.ID != id || device.IDProblem != "" {
			t.Fatalf("%s was not identified as %s: %#v", wantPath, id, device)
		}
	}

	// sda publishes the same kind of serial, but nothing proves its bus is not a
	// shared fabric, so it must not become selectable on serial alone.
	sda := findBlockDevice(t, snapshot, "/dev/sda")
	if sda.ID != "" {
		t.Fatalf("sda was expected to fail closed without transport evidence: %#v", sda)
	}
	// The problem must say which fact is missing, not only that one is.
	if !strings.Contains(sda.IDProblem, `bus not proven local (transport="", driver="")`) {
		t.Fatalf("sda IDProblem = %q", sda.IDProblem)
	}
}

// TestBlockCollectorIdentifiesVirtioSCSIDiskThroughSysfs is the customer's
// /dev/sda: a QEMU disk behind a virtio-scsi controller. lsblk derives no
// transport for it, so only the sysfs bus probe can prove the bus is local.
func TestBlockCollectorIdentifiesVirtioSCSIDiskThroughSysfs(t *testing.T) {
	sda := map[string]any{
		"name": "/dev/sda", "path": "/dev/sda", "kname": "/dev/sda", "type": "disk",
		"size": int64(500 << 30), "start": 0, "log-sec": 512,
		"ro": false, "rota": true, "rm": false,
		"model": "QEMU HARDDISK", "serial": "50014ee01a2b3c4d", "wwn": nil, "tran": nil,
		"mountpoints": []any{nil}, "options": []any{nil},
	}
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{sda}})
	if err != nil {
		t.Fatal(err)
	}
	collector := &blockCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{"/dev/sda": {"/dev/disk/by-id/scsi-SQEMU_QEMU_HARDDISK_50014ee01a2b3c4d"}}, nil
		},
		busProbe: func(lsblkDevice) busProbe { return busProbe{bus: "virtio", driver: "virtio_scsi"} },
	}

	snapshot, err := collector.blockSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	device := findBlockDevice(t, snapshot, "/dev/sda")
	if device.ID != "serial:qemu_harddisk:50014ee01a2b3c4d" || device.IDProblem != "" {
		t.Fatalf("virtio-scsi disk was not identified: %#v", device)
	}
}

// TestProbeBusFromSysfsRoot walks fake sysfs trees shaped like the real ones.
func TestProbeBusFromSysfsRoot(t *testing.T) {
	root := t.TempDir()
	mkdir := func(rel string) string {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	link := func(kernelName, target string) {
		if err := os.Symlink(target, filepath.Join(mkdir(kernelName), "device")); err != nil {
			t.Fatal(err)
		}
	}
	host := func(hostDir, procName string) {
		dir := filepath.Join(hostDir, "scsi_host", filepath.Base(hostDir))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "proc_name"), []byte(procName+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// virtio-blk: the device link points at the virtio device itself.
	link("vdb", mkdir("devices/pci0000:00/0000:00:0c.0/virtio3"))
	// virtio-scsi: a SCSI host under a virtio device.
	host(mkdir("devices/pci0000:00/0000:00:0d.0/virtio4/host2"), "virtio_scsi")
	link("sda", mkdir("devices/pci0000:00/0000:00:0d.0/virtio4/host2/target2:0:0/2:0:0:0"))
	// AHCI: a SCSI host under a libata port.
	host(mkdir("devices/pci0000:00/0000:00:1f.2/ata3/host1"), "ahci")
	link("sdc", mkdir("devices/pci0000:00/0000:00:1f.2/ata3/host1/target1:0:0/1:0:0:0"))
	// iSCSI: a SCSI host with no bus ancestor and a fabric driver.
	host(mkdir("devices/platform/host5"), "iscsi_tcp")
	link("sdd", mkdir("devices/platform/host5/session1/target5:0:0/5:0:0:0"))

	tests := []struct {
		kname string
		want  busProbe
		local string
	}{
		{"/dev/vdb", busProbe{bus: "virtio"}, "virtio"},
		{"/dev/sda", busProbe{bus: "virtio", driver: "virtio_scsi"}, "virtio"},
		{"/dev/sdc", busProbe{bus: "ata", driver: "ahci"}, "ata"},
		{"/dev/sdd", busProbe{driver: "iscsi_tcp"}, ""},
		{"/dev/sde", busProbe{}, ""},
		{"/dev/../sda", busProbe{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.kname, func(t *testing.T) {
			device := lsblkDevice{KName: tt.kname, Type: "disk"}
			got := probeBusFromSysfsRoot(device, root)
			if got != tt.want {
				t.Fatalf("probe = %#v, want %#v", got, tt.want)
			}
			if bus := localBus(device, nil, got); bus != tt.local {
				t.Fatalf("localBus = %q, want %q", bus, tt.local)
			}
		})
	}
}

// TestBlockCollectorMeasuredQEMUBusEvidence replays facts measured on a real
// QEMU/KVM guest booted from this image, one data disk per bus class, none of
// them publishing a WWN. Every string below — lsblk transport, by-id alias,
// sysfs device path, SCSI host driver — is copied from that guest rather than
// assumed, so the bus evidence is exercised against shapes the kernel really
// produces.
//
// The guest also confirmed the premise: multipathd was active and created no
// map at all for these four single-path SCSI disks. Under find_multipaths no
// every one of them would have become mpatha..mpathd with a WWID the collector
// cannot use.
func TestBlockCollectorMeasuredQEMUBusEvidence(t *testing.T) {
	sysfsRoot := t.TempDir()
	mkdir := func(rel string) string {
		dir := filepath.Join(sysfsRoot, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	linkDevice := func(kname, deviceRel string) {
		if err := os.Symlink(mkdir(deviceRel), filepath.Join(mkdir(kname), "device")); err != nil {
			t.Fatal(err)
		}
	}
	scsiHost := func(hostRel, procName string) {
		dir := mkdir(filepath.Join(hostRel, "scsi_host", filepath.Base(hostRel)))
		if err := os.WriteFile(filepath.Join(dir, "proc_name"), []byte(procName+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Measured: readlink -f /sys/block/<name>/device, and
	// grep . /sys/class/scsi_host/host*/proc_name
	const (
		megasasHost  = "devices/pci0000:00/0000:00:02.0/0000:01:00.0/0000:02:02.0/host0"
		virtioSCSIHt = "devices/pci0000:00/0000:00:02.2/0000:04:00.0/virtio2/host1"
		spiHost      = "devices/pci0000:00/0000:00:02.0/0000:01:00.0/0000:02:01.0/host2"
		ahciHost     = "devices/pci0000:00/0000:00:1f.2/ata4/host6"
	)
	scsiHost(megasasHost, "megaraid_sas")
	scsiHost(virtioSCSIHt, "virtio_scsi")
	scsiHost(spiHost, "sym53c8xx")
	scsiHost(ahciHost, "ahci")

	linkDevice("vda", "devices/pci0000:00/0000:00:02.4/0000:06:00.0/virtio3")
	linkDevice("vdb", "devices/pci0000:00/0000:00:02.5/0000:07:00.0/virtio4")
	linkDevice("sda", megasasHost+"/target0:2:0/0:2:0:0")
	linkDevice("sdb", virtioSCSIHt+"/target1:0:0/1:0:0:0")
	linkDevice("sdc", spiHost+"/target2:0:0/2:0:0:0")
	linkDevice("sdd", ahciHost+"/target6:0:0/6:0:0:0")

	// Measured: lsblk -o NAME,MODEL,SERIAL,WWN,TRAN. Every WWN is empty, and
	// only the SPI and AHCI disks get a transport at all.
	disks := []struct{ kname, model, serial, transport string }{
		{"vda", "", "5000c500142b3c4d", ""},
		{"vdb", "", "90a775711a809a8a75e5", ""},
		{"sda", "QEMU HARDDISK", "6001405deadbeef00001", ""},
		{"sdb", "QEMU HARDDISK", "50014ee01a2b3c4d", ""},
		{"sdc", "QEMU HARDDISK", "07a8489045cc024d75ee", "spi"},
		{"sdd", "QEMU HARDDISK", "c2793cff25c3aba3d4b7", "sata"},
	}
	// Measured: ls /dev/disk/by-id. The AHCI disk alone gets an ata- alias; the
	// three other SCSI disks get only scsi- forms, which prove nothing about
	// whether the bus is shared.
	byIDPaths := map[string][]string{
		"/dev/vda": {"/dev/disk/by-id/virtio-5000c500142b3c4d"},
		"/dev/vdb": {"/dev/disk/by-id/virtio-90a775711a809a8a75e5"},
		"/dev/sda": {
			"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_6001405deadbeef00001",
			"/dev/disk/by-id/scsi-SQEMU_QEMU_HARDDISK_6001405deadbeef00001",
		},
		"/dev/sdb": {
			"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_50014ee01a2b3c4d",
			"/dev/disk/by-id/scsi-SQEMU_QEMU_HARDDISK_50014ee01a2b3c4d",
		},
		"/dev/sdc": {
			"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_07a8489045cc024d75ee",
			"/dev/disk/by-id/scsi-SQEMU_QEMU_HARDDISK_07a8489045cc024d75ee",
		},
		"/dev/sdd": {
			"/dev/disk/by-id/ata-QEMU_HARDDISK_c2793cff25c3aba3d4b7",
			"/dev/disk/by-id/scsi-0ATA_QEMU_HARDDISK_c2793cff25c3aba3d4b7",
			"/dev/disk/by-id/scsi-1ATA_QEMU_HARDDISK_c2793cff25c3aba3d4b7",
			"/dev/disk/by-id/scsi-SATA_QEMU_HARDDISK_c2793cff25c3aba3d4b7",
		},
	}

	blockdevices := []any{}
	for _, d := range disks {
		path := "/dev/" + d.kname
		blockdevices = append(blockdevices, map[string]any{
			"name": path, "path": path, "kname": path, "type": "disk",
			"size": int64(2 << 30), "start": 0, "log-sec": 512,
			"ro": false, "rota": true, "rm": false,
			"model": d.model, "serial": d.serial, "wwn": nil, "tran": d.transport,
			"mountpoints": []any{nil}, "options": []any{nil},
		})
	}
	lsblk, err := json.Marshal(map[string]any{"blockdevices": blockdevices})
	if err != nil {
		t.Fatal(err)
	}

	collector := &blockCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) { return byIDPaths, nil },
		busProbe:  func(d lsblkDevice) busProbe { return probeBusFromSysfsRoot(d, sysfsRoot) },
	}

	snapshot, err := collector.blockSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct{ id, path, note string }{
		{"serial:virtio:5000c500142b3c4d", "/dev/vda", "virtio-blk via its by-id serial alias"},
		{"serial:virtio:90a775711a809a8a75e5", "/dev/vdb", "virtio-blk via its by-id serial alias"},
		{"serial:qemu_harddisk:50014ee01a2b3c4d", "/dev/sdb", "virtio-scsi: no transport, proven by the sysfs virtio ancestor"},
		{"serial:qemu_harddisk:07a8489045cc024d75ee", "/dev/sdc", "SPI: proven by lsblk transport"},
		{"serial:qemu_harddisk:c2793cff25c3aba3d4b7", "/dev/sdd", "AHCI: proven by transport, ata ancestor and ata- alias alike"},
	} {
		if device := findBlockDevice(t, snapshot, want.path); device.ID != want.id || device.IDProblem != "" {
			t.Fatalf("%s (%s) was not identified as %s: %#v", want.path, want.note, want.id, device)
		}
	}

	// The MegaRAID SAS disk is the control. Its serial is just as good, but a
	// SAS HBA may front storage another host can also reach, so nothing here
	// proves the bus is local and the disk must stay unmanageable.
	sda := findBlockDevice(t, snapshot, "/dev/sda")
	if sda.ID != "" {
		t.Fatalf("the SAS-fronted disk was expected to fail closed: %#v", sda)
	}
	if !strings.Contains(sda.IDProblem, `bus not proven local (transport="", driver="megaraid_sas")`) {
		t.Fatalf("sda IDProblem must name the driver that failed the check: %q", sda.IDProblem)
	}
}
