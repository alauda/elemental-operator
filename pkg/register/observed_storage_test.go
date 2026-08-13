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
		want   string
	}{
		{name: "wwn alias", device: lsblkDevice{Type: "disk"}, paths: []string{"/dev/disk/by-id/wwn-0x5000-C500"}, want: "wwn:5000c500"},
		{name: "nvme eui", device: lsblkDevice{Type: "disk"}, paths: []string{"/dev/disk/by-id/nvme-eui.00112233"}, want: "nvme-eui:00112233"},
		{name: "partition", device: lsblkDevice{Type: "part", PartUUID: "CB6A-04B2"}, want: "partuuid:cb6a-04b2"},
		{name: "multipath", device: lsblkDevice{Type: "mpath"}, paths: []string{"/dev/disk/by-id/dm-uuid-mpath-3600508b400"}, want: "wwid:3600508b400"},
		{name: "multipath non-hex WWID fails closed", device: lsblkDevice{Type: "mpath", Path: "/dev/mapper/mpatha"}, paths: []string{"/dev/disk/by-id/dm-uuid-mpath-0QEMU_DATA1"}, want: ""},
		{name: "local serial fallback", device: lsblkDevice{Type: "disk", Transport: "virtio", Model: "Data Disk", Serial: "SER 1"}, want: "serial:data_disk:ser_1"},
		{name: "shared serial rejected", device: lsblkDevice{Type: "disk", Transport: "iscsi", Model: "LUN", Serial: "SER1"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalDeviceID(tt.device, tt.paths); got != tt.want {
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
