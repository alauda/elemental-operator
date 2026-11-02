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
	"reflect"
	"strings"
	"testing"

	"github.com/twpayne/go-vfs/vfst"
)

func TestCollectBlockDevicesSharesTheObserverIdentity(t *testing.T) {
	const (
		system  = "/dev/sda"
		pathA   = "/dev/sdb"
		pathB   = "/dev/sdc"
		mapPath = "/dev/mapper/mpatha"
		mapKern = "/dev/dm-0"
	)
	osDisk := diskFixture(system)
	osDisk["kname"] = system
	efi := partitionFixture("/dev/sda1", "11111111-01", "vfat", "efi-uuid", []any{"/boot/efi"})
	efi["label"], efi["partlabel"], efi["kname"] = "COS_GRUB", "efi", "/dev/sda1"
	osDisk["children"] = []any{efi}

	// One LUN seen through two paths, with multipathd running.
	memberA := diskFixture(pathA)
	memberA["wwn"], memberA["tran"] = "0x600508b400105e21", "fc"
	memberB := diskFixture(pathB)
	memberB["wwn"], memberB["tran"] = "0x600508b400105e21", "fc"
	mapDevice := map[string]any{
		"name": mapPath, "path": mapPath, "kname": mapKern, "type": "mpath", "size": int64(4 << 40),
		"mountpoints": []any{nil},
	}
	memberA["children"] = []any{mapDevice}
	memberB["children"] = []any{mapDevice}

	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{osDisk, memberA, memberB}})
	if err != nil {
		t.Fatal(err)
	}
	var probed []string
	snapshot, err := CollectBlockDevices(BlockCollectorOptions{
		Run: func(name string, args ...string) ([]byte, error) {
			switch {
			case name == "lsblk" && reflect.DeepEqual(args, lsblkStorageArgs):
				return lsblk, nil
			case name == "wipefs" && args[len(args)-1] == system:
				return []byte(`{"signatures":[{"type":"gpt"},{"type":"PMBR"},{"type":"gpt"}]}`), nil
			case name == "wipefs" && args[len(args)-1] == "/dev/sda1":
				return []byte(`{"signatures":[{"type":"vfat","label":"COS_GRUB"}]}`), nil
			case name == "wipefs" && args[len(args)-1] == mapPath:
				return nil, errors.New("exit status 1: probing initialization failed")
			case name == "wipefs":
				return nil, nil
			}
			return nil, errors.New("unexpected command " + name)
		},
		ByIDPaths: func() (map[string][]string, error) {
			return map[string][]string{
				system:  {"/dev/disk/by-id/wwn-0x5000c50000000001"},
				mapKern: {"/dev/disk/by-id/dm-uuid-mpath-3600508b400105e21"},
			}, nil
		},
		BusProbe: func(kernelName string) (string, string) {
			probed = append(probed, kernelName)
			return "", ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{}
	for _, device := range snapshot.Devices {
		paths = append(paths, device.Path)
	}
	if !reflect.DeepEqual(paths, []string{mapPath, system, "/dev/sda1", pathA, pathB}) {
		t.Fatalf("devices = %v, want every lsblk node sorted by path", paths)
	}
	if !containsText(probed, "sda") {
		t.Fatalf("the bus probe was not consulted: %v", probed)
	}

	disk := snapshot.Find(system)
	if disk.ID != "wwn:5000c50000000001" || disk.Type != "disk" || disk.KernelPath != system {
		t.Fatalf("system disk = %#v", disk)
	}
	if !reflect.DeepEqual(disk.Signatures, []BlockSignature{{Type: "PMBR"}, {Type: "gpt"}}) {
		t.Fatalf("disk signatures = %#v", disk.Signatures)
	}
	if !reflect.DeepEqual(disk.Children, []string{"/dev/sda1"}) {
		t.Fatalf("disk children = %#v", disk.Children)
	}

	part := snapshot.Find("/dev/sda1")
	if part.PartLabel != "efi" || part.Label != "COS_GRUB" || part.ID != "partuuid:11111111-01" ||
		!reflect.DeepEqual(part.MountPoints, []string{"/boot/efi"}) || !reflect.DeepEqual(part.Parents, []string{system}) {
		t.Fatalf("partition = %#v", part)
	}
	if !reflect.DeepEqual(part.Signatures, []BlockSignature{{Type: "vfat", Label: "COS_GRUB"}}) {
		t.Fatalf("partition signatures = %#v", part.Signatures)
	}

	for _, member := range []string{pathA, pathB} {
		device := snapshot.Find(member)
		if device.ID != "" || device.IDProblem != "identity is shared with another device on this host" {
			t.Fatalf("multipath path %s must not keep the LUN's WWN as its own identity: %#v", member, device)
		}
		if !reflect.DeepEqual(device.Children, []string{mapPath}) {
			t.Fatalf("%s children = %#v", member, device.Children)
		}
	}

	mpath := snapshot.Find(mapKern)
	if mpath == nil || mpath.Path != mapPath || mpath.ID != "wwid:3600508b400105e21" {
		t.Fatalf("multipath map, found by kernel path = %#v", mpath)
	}
	if mpath.Signatures != nil || !strings.Contains(mpath.SignatureError, "probing initialization failed") {
		t.Fatalf("a wipefs failure must be reported, not read as no signatures: %#v", mpath)
	}
	if !reflect.DeepEqual(mpath.Parents, []string{pathA, pathB}) {
		t.Fatalf("multipath parents = %#v", mpath.Parents)
	}
}

func TestCollectBlockDevicesExplainsMissingIdentity(t *testing.T) {
	disk := diskFixture("/dev/sdb")
	disk["wwn"], disk["tran"] = nil, nil
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{disk}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := CollectBlockDevices(BlockCollectorOptions{
		Run: func(name string, _ ...string) ([]byte, error) {
			if name == "lsblk" {
				return lsblk, nil
			}
			return nil, nil
		},
		ByIDPaths: func() (map[string][]string, error) { return nil, errors.New("no udev") },
		BusProbe:  func(string) (string, string) { return "", "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	device := snapshot.Find("/dev/sdb")
	if device.ID != "" || !strings.Contains(device.IDProblem, "bus not proven local") {
		t.Fatalf("device = %#v", device)
	}
	if device.Signatures == nil || len(device.Signatures) != 0 || device.SignatureError != "" {
		t.Fatalf("empty wipefs output means no signatures: %#v", device)
	}
	if snapshot.Find("/dev/sdz") != nil {
		t.Fatal("Find returned a device lsblk did not report")
	}
}

func TestCollectBlockDevicesFailsWithoutLsblk(t *testing.T) {
	_, err := CollectBlockDevices(BlockCollectorOptions{
		Run: func(string, ...string) ([]byte, error) { return nil, errors.New("lsblk: not found") },
	})
	if err == nil || !strings.Contains(err.Error(), "running lsblk") {
		t.Fatalf("err = %v", err)
	}
}

func TestNormalizeDeviceID(t *testing.T) {
	for in, want := range map[string]string{
		"wwn:5000c500a1b2c3d4":           "wwn:5000c500a1b2c3d4",
		" WWN:0x5000C500A1B2C3D4 ":       "wwn:5000c500a1b2c3d4",
		"nvme-eui:0011.2233-4455":        "nvme-eui:001122334455",
		"NVME-NGUID:AA:BB":               "nvme-nguid:aabb",
		"wwid:3600508B400105E21":         "wwid:3600508b400105e21",
		"partuuid:{CB6A04B2-01}":         "partuuid:cb6a04b2-01",
		"serial:QEMU HARDDISK:Ser 1":     "serial:qemu_harddisk:ser_1",
		"serial:virtio:90A775711A809A8A": "serial:virtio:90a775711a809a8a",
		"wwn:not-hex":                    "wwn:not-hex",
		"unidentified:dev_sdb":           "unidentified:dev_sdb",
		"sdb":                            "sdb",
	} {
		if got := NormalizeDeviceID(in); got != want {
			t.Errorf("NormalizeDeviceID(%q) = %q, want %q", in, got, want)
		}
	}
	if got := UnidentifiedDeviceID("/dev/mapper/Some Map"); got != "unidentified:dev_mapper_some_map" {
		t.Errorf("UnidentifiedDeviceID = %q", got)
	}
}

func TestLiveEnvironmentReadsTheGivenFilesystem(t *testing.T) {
	for _, marker := range liveEnvironmentPaths {
		fs, cleanup, err := vfst.NewTestFS(map[string]any{marker: ""})
		if err != nil {
			t.Fatal(err)
		}
		if !LiveEnvironment(fs) {
			t.Errorf("%s does not mark a live environment", marker)
		}
		cleanup()
	}
	fs, cleanup, err := vfst.NewTestFS(map[string]any{"/run/cos/active_mode": ""})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if LiveEnvironment(fs) {
		t.Error("an installed system was taken for a live environment")
	}
}
