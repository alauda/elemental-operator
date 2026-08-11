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
	"strings"
	"testing"
)

func TestObservedStorageEligibility(t *testing.T) {
	const (
		diskPath = "/dev/sdb"
		dataUUID = "11111111-2222-3333-4444-555555555555"
	)

	tests := []struct {
		name             string
		mutate           func(map[string]any)
		byIDPaths        []string
		udevProperties   string
		wipefsOutput     string
		wipefsErr        error
		live             bool
		wantEligible     bool
		wantSystemDisk   bool
		wantReason       string
		wantByID         string
		wantPartitions   int
		wantFilesystem   string
		wantFilesystemID string
	}{
		{
			name:           "blank disk",
			byIDPaths:      []string{"/dev/disk/by-id/ata-data", "/dev/disk/by-id/wwn-0x5000c50000000001"},
			wipefsOutput:   `{"signatures":[]}`,
			wantEligible:   true,
			wantByID:       "/dev/disk/by-id/wwn-0x5000c50000000001",
			wantPartitions: 0,
		},
		{
			name: "whole-disk XFS",
			mutate: func(disk map[string]any) {
				disk["fstype"] = "xfs"
				disk["uuid"] = dataUUID
			},
			byIDPaths:        []string{"/dev/disk/by-id/scsi-data"},
			wantEligible:     true,
			wantByID:         "/dev/disk/by-id/scsi-data",
			wantPartitions:   1,
			wantFilesystem:   "xfs",
			wantFilesystemID: dataUUID,
		},
		{
			name: "single XFS partition",
			mutate: func(disk map[string]any) {
				disk["pttype"] = "gpt"
				disk["children"] = []any{partitionFixture("/dev/sdb1", 1, "xfs", dataUUID, nil)}
			},
			byIDPaths:        []string{"/dev/disk/by-id/ata-data"},
			wantEligible:     true,
			wantByID:         "/dev/disk/by-id/ata-data",
			wantPartitions:   1,
			wantFilesystem:   "xfs",
			wantFilesystemID: dataUUID,
		},
		{
			name: "Elemental system disk",
			mutate: func(disk map[string]any) {
				disk["pttype"] = "gpt"
				partition := partitionFixture("/dev/sdb4", 4, "btrfs", "system-uuid", []any{"/run/elemental/state"})
				partition["label"] = "COS_STATE"
				disk["children"] = []any{partition}
			},
			byIDPaths:      []string{"/dev/disk/by-id/wwn-system"},
			wantSystemDisk: true,
			wantReason:     storageReasonSystemDisk,
			wantByID:       "/dev/disk/by-id/wwn-system",
			wantPartitions: 1,
			wantFilesystem: "btrfs",
		},
		{
			name:         "blank disk in live ISO",
			byIDPaths:    []string{"/dev/disk/by-id/wwn-data"},
			wipefsOutput: `{"signatures":[]}`,
			live:         true,
			wantReason:   storageReasonLiveISO,
			wantByID:     "/dev/disk/by-id/wwn-data",
		},
		{
			name: "multiple partitions",
			mutate: func(disk map[string]any) {
				disk["pttype"] = "gpt"
				disk["children"] = []any{
					partitionFixture("/dev/sdb1", 1, "xfs", dataUUID, nil),
					partitionFixture("/dev/sdb2", 2, "xfs", "other-uuid", nil),
				}
			},
			byIDPaths:      []string{"/dev/disk/by-id/wwn-data"},
			wantReason:     "contains 2 partitions",
			wantByID:       "/dev/disk/by-id/wwn-data",
			wantPartitions: 2,
			wantFilesystem: "xfs",
		},
		{
			name: "mounted XFS",
			mutate: func(disk map[string]any) {
				disk["fstype"] = "xfs"
				disk["uuid"] = dataUUID
				disk["mountpoints"] = []any{"/srv/existing"}
			},
			byIDPaths:      []string{"/dev/disk/by-id/wwn-data"},
			wantReason:     "mounted at /srv/existing",
			wantByID:       "/dev/disk/by-id/wwn-data",
			wantPartitions: 1,
			wantFilesystem: "xfs",
		},
		{
			name:           "missing by-id",
			wipefsOutput:   `{"signatures":[]}`,
			wantReason:     storageReasonNoByID,
			wantPartitions: 0,
		},
		{
			name: "missing WWN and serial",
			mutate: func(disk map[string]any) {
				disk["wwn"] = nil
				disk["serial"] = nil
			},
			byIDPaths:      []string{"/dev/disk/by-id/ata-data"},
			wipefsOutput:   `{"signatures":[]}`,
			wantReason:     storageReasonNoIdentity,
			wantByID:       "/dev/disk/by-id/ata-data",
			wantPartitions: 0,
		},
		{
			name: "LVM member",
			mutate: func(disk map[string]any) {
				partition := partitionFixture("/dev/sdb1", 1, "LVM2_member", "lvm-uuid", nil)
				partition["children"] = []any{map[string]any{
					"name": "/dev/mapper/vg-data", "path": "/dev/mapper/vg-data", "type": "lvm",
				}}
				disk["children"] = []any{partition}
			},
			byIDPaths:      []string{"/dev/disk/by-id/wwn-data"},
			wantReason:     "protected filesystem signature LVM2_member",
			wantByID:       "/dev/disk/by-id/wwn-data",
			wantPartitions: 1,
			wantFilesystem: "LVM2_member",
		},
		{
			name: "removable device",
			mutate: func(disk map[string]any) {
				disk["rm"] = true
			},
			byIDPaths:      []string{"/dev/disk/by-id/usb-data"},
			wipefsOutput:   `{"signatures":[]}`,
			wantReason:     storageReasonRemovable,
			wantByID:       "/dev/disk/by-id/usb-data",
			wantPartitions: 0,
		},
		{
			name: "unsupported filesystem",
			mutate: func(disk map[string]any) {
				disk["fstype"] = "ext4"
				disk["uuid"] = dataUUID
			},
			byIDPaths:      []string{"/dev/disk/by-id/wwn-data"},
			wantReason:     "unsupported filesystem ext4",
			wantByID:       "/dev/disk/by-id/wwn-data",
			wantPartitions: 1,
			wantFilesystem: "ext4",
		},
		{
			name:           "hidden disk signature",
			byIDPaths:      []string{"/dev/disk/by-id/wwn-data"},
			wipefsOutput:   `{"signatures":[{"type":"gpt","usage":"partition-table"}]}`,
			wantReason:     "existing partition-table or filesystem signature",
			wantByID:       "/dev/disk/by-id/wwn-data",
			wantPartitions: 0,
		},
		{
			name:           "signature inspection failure",
			byIDPaths:      []string{"/dev/disk/by-id/wwn-data"},
			wipefsErr:      errors.New("wipefs unavailable"),
			wantReason:     storageReasonSignatureRead,
			wantByID:       "/dev/disk/by-id/wwn-data",
			wantPartitions: 0,
		},
		{
			name: "XFS partition without UUID",
			mutate: func(disk map[string]any) {
				disk["children"] = []any{partitionFixture("/dev/sdb1", 1, "xfs", "", nil)}
			},
			byIDPaths:      []string{"/dev/disk/by-id/wwn-data"},
			wantReason:     "single XFS partition has no UUID",
			wantByID:       "/dev/disk/by-id/wwn-data",
			wantPartitions: 1,
			wantFilesystem: "xfs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disk := diskFixture(diskPath)
			if tt.mutate != nil {
				tt.mutate(disk)
			}
			lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{disk}})
			if err != nil {
				t.Fatalf("marshal lsblk fixture: %v", err)
			}

			collector := &observedStorageCollector{
				run: func(name string, args ...string) ([]byte, error) {
					switch name {
					case "lsblk":
						return lsblk, nil
					case "udevadm":
						return []byte(tt.udevProperties), nil
					case "wipefs":
						if tt.wipefsErr != nil {
							return nil, tt.wipefsErr
						}
						output := tt.wipefsOutput
						if output == "" {
							output = `{"signatures":[]}`
						}
						return []byte(output), nil
					default:
						return nil, fmt.Errorf("unexpected command %s %s", name, strings.Join(args, " "))
					}
				},
				byIDPaths: func() (map[string][]string, error) {
					return map[string][]string{diskPath: tt.byIDPaths}, nil
				},
				liveEnvironment: func() bool { return tt.live },
			}

			observed, err := collector.collect()
			if err != nil {
				t.Fatalf("collect observed storage: %v", err)
			}
			if len(observed.Devices) != 1 {
				t.Fatalf("got %d devices, want 1", len(observed.Devices))
			}
			device := observed.Devices[0]
			if device.Eligible != tt.wantEligible {
				t.Errorf("Eligible = %v, want %v; reasons: %v", device.Eligible, tt.wantEligible, device.IneligibleReasons)
			}
			if device.SystemDisk != tt.wantSystemDisk {
				t.Errorf("SystemDisk = %v, want %v", device.SystemDisk, tt.wantSystemDisk)
			}
			if device.ByID != tt.wantByID {
				t.Errorf("ByID = %q, want %q", device.ByID, tt.wantByID)
			}
			if len(device.Partitions) != tt.wantPartitions {
				t.Fatalf("got %d partitions, want %d: %#v", len(device.Partitions), tt.wantPartitions, device.Partitions)
			}
			if tt.wantFilesystem != "" && device.Partitions[0].FilesystemType != tt.wantFilesystem {
				t.Errorf("FilesystemType = %q, want %q", device.Partitions[0].FilesystemType, tt.wantFilesystem)
			}
			if tt.wantFilesystemID != "" && device.Partitions[0].FilesystemUUID != tt.wantFilesystemID {
				t.Errorf("FilesystemUUID = %q, want %q", device.Partitions[0].FilesystemUUID, tt.wantFilesystemID)
			}
			if tt.wantReason != "" && !containsReason(device.IneligibleReasons, tt.wantReason) {
				t.Errorf("reasons %v do not contain %q", device.IneligibleReasons, tt.wantReason)
			}
		})
	}
}

func TestObservedStorageUsesUdevIdentityAndFlexibleLSBLKValues(t *testing.T) {
	disk := diskFixture("/dev/nvme0n1")
	disk["size"] = "1099511627776"
	disk["rota"] = "1"
	disk["rm"] = "0"
	disk["serial"] = nil
	disk["wwn"] = nil
	lsblk, err := json.Marshal(map[string]any{"blockdevices": []any{disk}})
	if err != nil {
		t.Fatal(err)
	}

	collector := &observedStorageCollector{
		run: func(name string, _ ...string) ([]byte, error) {
			switch name {
			case "lsblk":
				return lsblk, nil
			case "udevadm":
				return []byte("ID_WWN=0x5000c50000000099\nID_SERIAL_SHORT=NVME-SERIAL\nID_MODEL=NVME_DATA\n"), nil
			case "wipefs":
				return []byte(`{"signatures":[]}`), nil
			default:
				return nil, fmt.Errorf("unexpected command %s", name)
			}
		},
		byIDPaths: func() (map[string][]string, error) {
			return map[string][]string{
				"/dev/nvme0n1": {
					"/dev/disk/by-id/nvme-eui.0000000000000099",
					"/dev/disk/by-id/wwn-0x5000c50000000099",
				},
			}, nil
		},
		liveEnvironment: func() bool { return false },
	}

	observed, err := collector.collect()
	if err != nil {
		t.Fatal(err)
	}
	device := observed.Devices[0]
	if !device.Eligible {
		t.Fatalf("device should be eligible: %v", device.IneligibleReasons)
	}
	if device.WWN != "0x5000c50000000099" || device.Serial != "NVME-SERIAL" {
		t.Errorf("unexpected identity: WWN=%q Serial=%q", device.WWN, device.Serial)
	}
	if device.ByID != "/dev/disk/by-id/wwn-0x5000c50000000099" {
		t.Errorf("unexpected canonical by-id: %q", device.ByID)
	}
	if device.SizeBytes != 1099511627776 || !device.Rotational {
		t.Errorf("unexpected capacity/media: size=%d rotational=%v", device.SizeBytes, device.Rotational)
	}
}

func TestObservedStorageCollectionErrors(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		err  error
	}{
		{name: "lsblk command", err: errors.New("lsblk failed")},
		{name: "invalid lsblk JSON", data: []byte("not-json")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			collector := &observedStorageCollector{
				run:             func(string, ...string) ([]byte, error) { return tt.data, tt.err },
				byIDPaths:       func() (map[string][]string, error) { return nil, nil },
				liveEnvironment: func() bool { return false },
			}
			if _, err := collector.collect(); err == nil {
				t.Fatal("expected collection error")
			}
		})
	}
}

func TestObservedStorageProtocolMessage(t *testing.T) {
	if MsgObservedStorageConfig <= MsgObservedNetworkConfig {
		t.Fatalf("storage protocol message %d must follow observed network %d", MsgObservedStorageConfig, MsgObservedNetworkConfig)
	}
	if MsgLast != MsgObservedStorageConfig {
		t.Fatalf("MsgLast = %d, want %d", MsgLast, MsgObservedStorageConfig)
	}
	if got := MsgObservedStorageConfig.String(); got != "ObservedStorageConfig" {
		t.Fatalf("String() = %q", got)
	}
}

func TestObservedStorageEnumeratesWholeDiskByIDPaths(t *testing.T) {
	root := t.TempDir()
	deviceDir := filepath.Join(root, "dev")
	byIDDir := filepath.Join(deviceDir, "disk", "by-id")
	if err := os.MkdirAll(byIDDir, 0o755); err != nil {
		t.Fatal(err)
	}
	diskPath := filepath.Join(deviceDir, "sdb")
	partitionPath := filepath.Join(deviceDir, "sdb1")
	if err := os.WriteFile(diskPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partitionPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"ata-data":       diskPath,
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
	resolvedDiskPath, err := filepath.EvalSymlinks(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	resolvedPartitionPath, err := filepath.EvalSymlinks(partitionPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalByID(paths[resolvedDiskPath]); got != filepath.Join(byIDDir, "wwn-data") {
		t.Fatalf("canonical by-id = %q", got)
	}
	if _, found := paths[resolvedPartitionPath]; found {
		t.Fatal("partition by-id path was not filtered")
	}
}

func diskFixture(path string) map[string]any {
	return map[string]any{
		"name":        path,
		"path":        path,
		"type":        "disk",
		"size":        int64(500 * 1024 * 1024 * 1024),
		"rota":        false,
		"rm":          false,
		"model":       "ExampleDataDisk",
		"serial":      "DATA-SERIAL",
		"wwn":         "0x5000c50000000001",
		"fstype":      nil,
		"uuid":        nil,
		"label":       nil,
		"mountpoints": []any{nil},
		"pttype":      nil,
		"parttype":    nil,
		"partn":       nil,
	}
}

func partitionFixture(path string, number int, filesystem, uuid string, mounts []any) map[string]any {
	if mounts == nil {
		mounts = []any{nil}
	}
	return map[string]any{
		"name":        path,
		"path":        path,
		"type":        "part",
		"fstype":      filesystem,
		"uuid":        uuid,
		"mountpoints": mounts,
		"partn":       number,
	}
}

func containsReason(reasons []string, expected string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, expected) {
			return true
		}
	}
	return false
}
