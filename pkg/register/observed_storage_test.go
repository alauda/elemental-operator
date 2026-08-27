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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

func TestObservedStorageReportsObjectiveTopology(t *testing.T) {
	const (
		dataDisk = "/dev/sdb"
		dataPart = "/dev/sdb1"
		system   = "/dev/vda"
		sysPart  = "/dev/vda4"
		pathA    = "/dev/sdc"
		pathB    = "/dev/sdd"
		mpath    = "/dev/mapper/3600508b400105e210000900000490000"
	)

	data := diskFixture(dataDisk)
	data["wwn"] = "0x5000c50000000001"
	data["pttype"] = "gpt"
	data["children"] = []any{partitionFixture(dataPart, "cb6a04b2-01", "ext4", "59e82e0a-1111-2222-3333-444444444444", []any{"/data/application"})}

	osDisk := diskFixture(system)
	osDisk["wwn"] = "0x5000c50000000002"
	osPartition := partitionFixture(sysPart, "system-part", "btrfs", "system-uuid", []any{"/run/elemental/persistent"})
	osPartition["label"] = "COS_PERSISTENT"
	osDisk["children"] = []any{osPartition}

	memberA := diskFixture(pathA)
	memberA["wwn"] = "0x5000c50000000003"
	memberA["tran"] = "fc"
	memberB := diskFixture(pathB)
	memberB["wwn"] = "0x5000c50000000004"
	memberB["tran"] = "fc"
	mapFixture := map[string]any{
		"name": mpath, "path": mpath, "type": "mpath", "size": int64(4 << 40),
		"wwn": "3600508b400105e210000900000490000", "fstype": "xfs",
		"uuid": "79288fcb-1111-2222-3333-444444444444", "mountpoints": []any{nil},
	}
	memberA["children"] = []any{mapFixture}
	memberB["children"] = []any{mapFixture}

	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{data, osDisk, memberA, memberB}})
	if err != nil {
		t.Fatal(err)
	}
	collector := &observedStorageCollector{
		run: func(name string, args ...string) ([]byte, error) {
			switch name {
			case "lsblk":
				return lsblk, nil
			case "wipefs":
				if len(args) > 0 && args[len(args)-1] == dataDisk {
					return []byte(`{"signatures":[{"type":"gpt","usage":"partition-table"}]}`), nil
				}
				return []byte(`{"signatures":[]}`), nil
			case "multipathd":
				return []byte("3600508b400105e210000900000490000|sdc|active\n3600508b400105e210000900000490000|sdd|ready\n"), nil
			default:
				return nil, fmt.Errorf("unexpected command %s %s", name, strings.Join(args, " "))
			}
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{
				dataDisk: {"/dev/disk/by-id/wwn-0x5000c50000000001"},
				dataPart: {"/dev/disk/by-id/wwn-0x5000c50000000001-part1"},
				system:   {"/dev/disk/by-id/wwn-0x5000c50000000002"},
				pathA:    {"/dev/disk/by-id/scsi-path-a"},
				pathB:    {"/dev/disk/by-id/scsi-path-b"},
				mpath:    {"/dev/disk/by-id/dm-uuid-mpath-3600508b400105e210000900000490000"},
			}, nil
		},
		mountOptions: func() (map[string][]string, error) {
			return map[string][]string{"/data/application": {"noatime", "rw"}}, nil
		},
		liveEnvironment: func() bool { return false },
		bootID:          func() string { return "boot-1" },
	}

	observed, err := collector.collect()
	if err != nil {
		t.Fatalf("collect observed storage: %v", err)
	}
	if observed.BootID != "boot-1" {
		t.Fatalf("BootID = %q", observed.BootID)
	}
	if len(observed.Devices) != 5 {
		t.Fatalf("got %d logical devices, want disk, partition, system disk, system partition and map: %#v", len(observed.Devices), observed.Devices)
	}

	byID := observedDevicesByID(observed.Devices)
	disk := byID["wwn:5000c50000000001"]
	if disk == nil || disk.Kind != elementalv1.ObservedStorageDeviceDirectDisk || disk.SystemRole != elementalv1.ObservedStorageSystemRoleData {
		t.Fatalf("unexpected direct disk: %#v", disk)
	}
	if !reflect.DeepEqual(disk.Signatures, []elementalv1.ObservedStorageSignature{{Type: "partition-table", Value: "gpt"}}) {
		t.Fatalf("direct disk signatures = %#v", disk.Signatures)
	}
	if len(disk.Consumers) != 1 || disk.Consumers[0].Type != "part" || disk.Consumers[0].Path != dataPart || !reflect.DeepEqual(disk.Consumers[0].Mounts, []string{"/data/application"}) {
		t.Fatalf("direct disk consumers = %#v", disk.Consumers)
	}

	partition := byID["partuuid:cb6a04b2-01"]
	if partition == nil || partition.ParentID != disk.ID || partition.StartBytes != 1024*512 {
		t.Fatalf("unexpected partition topology: %#v", partition)
	}
	if partition.Filesystem == nil || partition.Filesystem.Type != "ext4" || partition.Filesystem.UUID == "" {
		t.Fatalf("unexpected partition filesystem: %#v", partition.Filesystem)
	}
	if len(partition.Mounts) != 1 || partition.Mounts[0].Path != "/data/application" || !reflect.DeepEqual(partition.Mounts[0].Options, []string{"noatime", "rw"}) {
		t.Fatalf("unexpected partition mounts: %#v", partition.Mounts)
	}

	if got := byID["wwn:5000c50000000002"]; got == nil || got.SystemRole != elementalv1.ObservedStorageSystemRoleSystem {
		t.Fatalf("system closure did not reach parent disk: %#v", got)
	}
	mapDevice := byID["wwid:3600508b400105e210000900000490000"]
	if mapDevice == nil || mapDevice.Kind != elementalv1.ObservedStorageDeviceMultipath {
		t.Fatalf("unexpected multipath map: %#v", mapDevice)
	}
	if mapDevice.Health == nil || mapDevice.Health.ActivePaths != 2 || mapDevice.Health.TotalPaths != 2 {
		t.Fatalf("unexpected multipath health: %#v", mapDevice.Health)
	}
	if len(mapDevice.MemberIDs) != 2 {
		t.Fatalf("member IDs = %#v", mapDevice.MemberIDs)
	}
	if byID["wwn:5000c50000000003"] != nil || byID["wwn:5000c50000000004"] != nil {
		t.Fatal("multipath member disks must not be exposed as selectable DirectDisk records")
	}
}

func TestObservedStorageHidesRawMultipathPartitionAliases(t *testing.T) {
	const (
		member       = "/dev/sda"
		rawPartition = "/dev/sda1"
		mapPath      = "/dev/mapper/3600508b400105e210000900000490000"
		mapPartition = "/dev/mapper/3600508b400105e210000900000490000-part1"
		partUUID     = "153c602f-2f96-4797-8785-fcb1572c19d4"
	)

	rawPart := partitionFixture(rawPartition, partUUID, "btrfs", "shared-filesystem", nil)
	mapPart := partitionFixture(mapPartition, partUUID, "btrfs", "shared-filesystem", nil)
	mapDevice := map[string]any{
		"name": mapPath, "path": mapPath, "type": "mpath", "size": int64(100 << 30),
		"wwn": "3600508b400105e210000900000490000", "mountpoints": []any{nil},
		"children": []any{mapPart},
	}
	memberDevice := diskFixture(member)
	memberDevice["children"] = []any{rawPart, mapDevice}
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{memberDevice}})
	if err != nil {
		t.Fatal(err)
	}
	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{
				member:  {"/dev/disk/by-id/scsi-member"},
				mapPath: {"/dev/disk/by-id/dm-uuid-mpath-3600508b400105e210000900000490000"},
			}, nil
		},
		liveEnvironment: func() bool { return false },
		bootID:          func() string { return "boot-multipath-alias" },
	}

	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	byID := observedDevicesByID(observed.Devices)
	partition := byID["partuuid:"+partUUID]
	if partition == nil || partition.Path != mapPartition {
		t.Fatalf("aggregate partition not preserved: %#v", observed.Devices)
	}
	for _, device := range observed.Devices {
		if device.Path == member || device.Path == rawPartition {
			t.Fatalf("raw Multipath backing alias was exposed: %#v", device)
		}
	}
}

func TestObservedStorageResolvesMultipathStableIDThroughKernelName(t *testing.T) {
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
	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{kernelPath: {stablePath}}, nil
		},
		liveEnvironment: func() bool { return false },
		bootID:          func() string { return "boot-mapper-kname" },
	}

	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Devices) != 1 {
		t.Fatalf("devices = %#v", observed.Devices)
	}
	device := observed.Devices[0]
	if device.ID != "wwid:"+wwid || device.SystemRole != elementalv1.ObservedStorageSystemRoleData {
		t.Fatalf("multipath map did not resolve through KNAME: %#v", device)
	}
	if !reflect.DeepEqual(device.StablePaths, []string{stablePath}) {
		t.Fatalf("stable paths = %#v", device.StablePaths)
	}
}

func TestObservedStorageReportsLayeredConsumersAndSystemClosure(t *testing.T) {
	pv := diskFixture("/dev/sdb")
	pv["wwn"] = "0x5000c50000000009"
	lv := map[string]any{
		"name": "/dev/mapper/vg-data", "path": "/dev/mapper/vg-data", "type": "lvm", "size": int64(100 << 30),
		"fstype": "xfs", "uuid": "lv-uuid", "mountpoints": []any{"/srv/data"}, "options": []any{"rw"},
	}
	pv["children"] = []any{lv}
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{pv}})
	if err != nil {
		t.Fatal(err)
	}
	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{"/dev/sdb": {"/dev/disk/by-id/wwn-0x5000c50000000009"}}, nil
		},
		liveEnvironment: func() bool { return false },
		bootID:          func() string { return "boot-layered" },
	}
	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Devices) != 1 {
		t.Fatalf("devices = %#v", observed.Devices)
	}
	device := observed.Devices[0]
	if len(device.Consumers) != 1 || device.Consumers[0].Type != "lvm" || device.Consumers[0].FilesystemType != "xfs" || !reflect.DeepEqual(device.Consumers[0].Mounts, []string{"/srv/data"}) {
		t.Fatalf("layered consumers = %#v", device.Consumers)
	}
	if device.SystemRole != elementalv1.ObservedStorageSystemRoleData {
		t.Fatalf("ordinary business LVM should remain an objective Data fact, got %#v", device)
	}
}

func TestObservedStorageFailsClosedWhenIdentityOrInspectionIsUnknown(t *testing.T) {
	disk := diskFixture("/dev/sdb")
	disk["wwn"] = nil
	disk["serial"] = "SHARED"
	disk["model"] = "SAN"
	disk["tran"] = "fc"
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{disk}})
	if err != nil {
		t.Fatal(err)
	}
	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return nil, errors.New("wipefs failed")
		},
		byIDPaths:       func() (map[string][]string, error) { return nil, errors.New("udev unavailable") },
		liveEnvironment: func() bool { return false },
		bootID:          func() string { return "boot-2" },
	}
	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	device := observed.Devices[0]
	if device.ID != "unidentified:dev_sdb" || device.SystemRole != elementalv1.ObservedStorageSystemRoleUnknown {
		t.Fatalf("unexpected fail-closed device: %#v", device)
	}
	if !containsText(device.SystemEvidence, "no supported stable device identity") || !containsText(device.SystemEvidence, "wipefs signature inspection failed") {
		t.Fatalf("missing uncertainty evidence: %#v", device.SystemEvidence)
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

func TestObservedStorageFlexibleLSBLKValuesAndLiveRole(t *testing.T) {
	disk := diskFixture("/dev/nvme0n1")
	disk["size"] = "1099511627776"
	disk["rota"] = "1"
	disk["rm"] = "0"
	disk["tran"] = "nvme"
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{disk}})
	if err != nil {
		t.Fatal(err)
	}
	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{"/dev/nvme0n1": {"/dev/disk/by-id/nvme-eui.0000000000000099"}}, nil
		},
		liveEnvironment: func() bool { return true },
		bootID:          func() string { return "boot-live" },
	}
	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	device := observed.Devices[0]
	if device.SizeBytes != 1099511627776 || !device.Rotational || device.Removable {
		t.Fatalf("unexpected flexible values: %#v", device)
	}
	if device.SystemRole != elementalv1.ObservedStorageSystemRoleUnknown || !containsText(device.SystemEvidence, "live environment") {
		t.Fatalf("live device must be Unknown: %#v", device)
	}
}

func TestObservedStorageCollectionErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		data []byte
		err  error
	}{
		{name: "lsblk command", err: errors.New("lsblk failed")},
		{name: "invalid lsblk JSON", data: []byte("not-json")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			collector := &observedStorageCollector{
				run:             func(string, ...string) ([]byte, error) { return tt.data, tt.err },
				byIDPaths:       func() (map[string][]string, error) { return nil, nil },
				liveEnvironment: func() bool { return false },
				bootID:          func() string { return "boot" },
			}
			if _, err := collector.collect(); err == nil {
				t.Fatal("expected collection error")
			}
		})
	}
}

func TestObservedStorageLSBLKArgsRemainUtilLinux237Compatible(t *testing.T) {
	joined := strings.Join(lsblkStorageArgs, ",")
	for _, unsupported := range []string{"PARTN", "START", "OPTIONS"} {
		for _, column := range strings.Split(joined, ",") {
			if column == unsupported {
				t.Fatalf("%s is not supported by util-linux 2.37.4 used in the Elemental base image", unsupported)
			}
		}
	}
	for _, required := range []string{"KNAME", "PARTUUID", "PKNAME", "LOG-SEC"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("lsblk columns do not include %s: %s", required, joined)
		}
	}
}

func TestObservedStorageCollectsMountOptionsWithFindmnt(t *testing.T) {
	collector := &observedStorageCollector{run: func(name string, args ...string) ([]byte, error) {
		if name != "findmnt" || !reflect.DeepEqual(args, findmntStorageArgs) {
			return nil, fmt.Errorf("unexpected command %s %v", name, args)
		}
		return []byte(`{"filesystems":[{"target":"/data/application","options":"rw,noatime,nodev"},{"target":null,"options":null}]}`), nil
	}}
	got, err := collector.collectMountOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string][]string{"/data/application": {"noatime", "nodev", "rw"}}) {
		t.Fatalf("mount options = %#v", got)
	}
}

func TestPartitionStartBytesFromUtilLinux237SysfsFallback(t *testing.T) {
	sysfsRoot := t.TempDir()
	deviceDir := filepath.Join(sysfsRoot, "sda1")
	if err := os.MkdirAll(deviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deviceDir, "start"), []byte("2048\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	device := lsblkDevice{KName: "/dev/sda1", Type: "part", LogicalSector: 512}
	if got := partitionStartBytesFromSysfsRoot(device, sysfsRoot); got != 1048576 {
		t.Fatalf("partition start bytes = %d, want 1048576", got)
	}
	device.KName = "/dev/../sda1"
	if got := partitionStartBytesFromSysfsRoot(device, sysfsRoot); got != 0 {
		t.Fatalf("unsafe kernel name returned %d", got)
	}
}

func TestObservedStorageProtocolMessages(t *testing.T) {
	if MsgObservedStorageConfig <= MsgObservedNetworkConfig || MsgObserveStorage <= MsgObservedStorageConfig {
		t.Fatalf("unexpected storage protocol ordering: network=%d report=%d observe=%d", MsgObservedNetworkConfig, MsgObservedStorageConfig, MsgObserveStorage)
	}
	if MsgLast != MsgObserveStorage {
		t.Fatalf("MsgLast = %d, want %d", MsgLast, MsgObserveStorage)
	}
	if MsgObservedStorageConfig.String() != "ObservedStorageConfig" || MsgObserveStorage.String() != "ObserveStorage" {
		t.Fatalf("unexpected message names: %q %q", MsgObservedStorageConfig, MsgObserveStorage)
	}
}

func TestObservedStorageEnumeratesStablePathsForDisksAndPartitions(t *testing.T) {
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

func observedDevicesByID(devices []elementalv1.ObservedStorageDevice) map[string]*elementalv1.ObservedStorageDevice {
	out := make(map[string]*elementalv1.ObservedStorageDevice, len(devices))
	for i := range devices {
		out[devices[i].ID] = &devices[i]
	}
	return out
}

func containsText(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

// TestObservedStorageReproducesCustomerQEMUEnvironment replays a customer's real
// device tree, captured from lsblk, /dev/disk/by-id and multipathd. It is the
// local stand-in for that environment so identity behaviour is pinned without a VM.
//
// The environment is entirely QEMU-backed and shows three separate hazards:
// virtio-blk publishing nothing but a serial, single-path disks that
// find_multipaths no wrapped into maps, and udev minting wwn-0x aliases by
// chopping the first character off a WWID that has no designator-type digit.
func TestObservedStorageReproducesCustomerQEMUEnvironment(t *testing.T) {
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
	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths:       func() (map[string][]string, error) { return byIDPaths, nil },
		liveEnvironment: func() bool { return false },
		bootID:          func() string { return "boot-customer-qemu" },
	}

	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	byID := observedDevicesByID(observed.Devices)

	// Both virtio disks resolve through their by-id serial alias.
	for path, want := range map[string]string{
		"/dev/vda": "serial:virtio:5000c500142b3c4d",
		"/dev/vdb": "serial:virtio:90a775711a809a8a75e5",
	} {
		device := byID[want]
		if device == nil || device.Path != path {
			t.Fatalf("%s was not identified as %s: %#v", path, want, observed.Devices)
		}
	}

	// Every map still fails closed, and no member path is exposed. mpathc is the
	// one that would slip through a rule keyed only on "not hexadecimal": udev's
	// wwn-0xEMU_HARDDISK_... alias must not be mistaken for a usable WWN.
	for _, m := range qemuMaps {
		mapped := byID["unidentified:dev_mapper_"+m.alias]
		if mapped == nil || mapped.Path != "/dev/mapper/"+m.alias {
			t.Fatalf("map %s was expected to fail closed: %#v", m.alias, observed.Devices)
		}
		if mapped.SystemRole != elementalv1.ObservedStorageSystemRoleUnknown {
			t.Fatalf("map %s role = %q, want Unknown", m.alias, mapped.SystemRole)
		}
		for _, device := range observed.Devices {
			if device.Path == m.member {
				t.Fatalf("multipath member %s was exposed: %#v", m.member, device)
			}
		}
	}
}

// TestObservedStorageCustomerEnvironmentWithoutFakeMultipathMaps replays the same
// customer disks as they appear once the OS image stops shipping
// find_multipaths no, so single-path QEMU disks are no longer wrapped into maps.
//
// The three disks differ only in the transport lsblk manages to derive, and that
// alone decides whether the serial fallback is admissible.
func TestObservedStorageCustomerEnvironmentWithoutFakeMultipathMaps(t *testing.T) {
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
	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths:       func() (map[string][]string, error) { return byIDPaths, nil },
		liveEnvironment: func() bool { return false },
		bootID:          func() string { return "boot-customer-no-maps" },
	}

	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	byID := observedDevicesByID(observed.Devices)

	for id, wantPath := range map[string]string{
		"serial:qemu_harddisk:07a8489045cc024d75ee": "/dev/sdb",
		"serial:qemu_harddisk:c2793cff25c3aba3d4b7": "/dev/sdc",
	} {
		device := byID[id]
		if device == nil || device.Path != wantPath {
			t.Fatalf("%s was not identified as %s: %#v", wantPath, id, observed.Devices)
		}
		if device.SystemRole != elementalv1.ObservedStorageSystemRoleData {
			t.Fatalf("%s role = %q, want Data", wantPath, device.SystemRole)
		}
	}

	// sda publishes the same kind of serial, but nothing proves its bus is not a
	// shared fabric, so it must not become selectable on serial alone.
	sda := byID["unidentified:dev_sda"]
	if sda == nil || sda.Path != "/dev/sda" {
		t.Fatalf("sda was expected to fail closed without transport evidence: %#v", observed.Devices)
	}
	if sda.SystemRole != elementalv1.ObservedStorageSystemRoleUnknown {
		t.Fatalf("sda role = %q, want Unknown", sda.SystemRole)
	}
	// The evidence must say which fact is missing, not only that one is.
	if !containsText(sda.SystemEvidence, `bus not proven local (transport="", driver="")`) {
		t.Fatalf("sda evidence = %v", sda.SystemEvidence)
	}
}

// TestObservedStorageIdentifiesVirtioSCSIDiskThroughSysfs is the customer's
// /dev/sda: a QEMU disk behind a virtio-scsi controller. lsblk derives no
// transport for it, so only the sysfs bus probe can prove the bus is local.
func TestObservedStorageIdentifiesVirtioSCSIDiskThroughSysfs(t *testing.T) {
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
	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{"/dev/sda": {"/dev/disk/by-id/scsi-SQEMU_QEMU_HARDDISK_50014ee01a2b3c4d"}}, nil
		},
		busProbe:        func(lsblkDevice) busProbe { return busProbe{bus: "virtio", driver: "virtio_scsi"} },
		liveEnvironment: func() bool { return false },
		bootID:          func() string { return "boot-virtio-scsi" },
	}

	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	device := observedDevicesByID(observed.Devices)["serial:qemu_harddisk:50014ee01a2b3c4d"]
	if device == nil || device.Path != "/dev/sda" {
		t.Fatalf("virtio-scsi disk was not identified: %#v", observed.Devices)
	}
	if device.SystemRole != elementalv1.ObservedStorageSystemRoleData {
		t.Fatalf("role = %q, want Data", device.SystemRole)
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

// TestObservedStorageMeasuredQEMUBusEvidence replays facts measured on a real
// QEMU/KVM guest booted from this image, one data disk per bus class, none of
// them publishing a WWN. Every string below — lsblk transport, by-id alias,
// sysfs device path, SCSI host driver — is copied from that guest rather than
// assumed, so the bus evidence is exercised against shapes the kernel really
// produces.
//
// The guest also confirmed the premise: multipathd was active and created no
// map at all for these four single-path SCSI disks. Under find_multipaths no
// every one of them would have become mpatha..mpathd with a WWID the observer
// cannot use.
func TestObservedStorageMeasuredQEMUBusEvidence(t *testing.T) {
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

	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return []byte(`{"signatures":[]}`), nil
		},
		byIDPaths:       func() (map[string][]string, error) { return byIDPaths, nil },
		busProbe:        func(d lsblkDevice) busProbe { return probeBusFromSysfsRoot(d, sysfsRoot) },
		liveEnvironment: func() bool { return false },
		bootID:          func() string { return "boot-measured-qemu" },
	}

	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	byID := observedDevicesByID(observed.Devices)

	for _, want := range []struct{ id, path, note string }{
		{"serial:virtio:5000c500142b3c4d", "/dev/vda", "virtio-blk via its by-id serial alias"},
		{"serial:virtio:90a775711a809a8a75e5", "/dev/vdb", "virtio-blk via its by-id serial alias"},
		{"serial:qemu_harddisk:50014ee01a2b3c4d", "/dev/sdb", "virtio-scsi: no transport, proven by the sysfs virtio ancestor"},
		{"serial:qemu_harddisk:07a8489045cc024d75ee", "/dev/sdc", "SPI: proven by lsblk transport"},
		{"serial:qemu_harddisk:c2793cff25c3aba3d4b7", "/dev/sdd", "AHCI: proven by transport, ata ancestor and ata- alias alike"},
	} {
		device := byID[want.id]
		if device == nil || device.Path != want.path {
			t.Fatalf("%s (%s) was not identified as %s: %#v", want.path, want.note, want.id, observed.Devices)
		}
		if device.SystemRole != elementalv1.ObservedStorageSystemRoleData {
			t.Fatalf("%s role = %q, want Data", want.path, device.SystemRole)
		}
	}

	// The MegaRAID SAS disk is the control. Its serial is just as good, but a
	// SAS HBA may front storage another host can also reach, so nothing here
	// proves the bus is local and the disk must stay unmanageable.
	sda := byID["unidentified:dev_sda"]
	if sda == nil || sda.Path != "/dev/sda" {
		t.Fatalf("the SAS-fronted disk was expected to fail closed: %#v", observed.Devices)
	}
	if sda.SystemRole != elementalv1.ObservedStorageSystemRoleUnknown {
		t.Fatalf("sda role = %q, want Unknown", sda.SystemRole)
	}
	if !containsText(sda.SystemEvidence, `bus not proven local (transport="", driver="megaraid_sas")`) {
		t.Fatalf("sda evidence must name the driver that failed the check: %v", sda.SystemEvidence)
	}
}
