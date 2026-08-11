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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
)

const (
	storageReasonSystemDisk    = "system disk"
	storageReasonLiveISO       = "live ISO cannot determine the installation target disk; refresh after installation"
	storageReasonRemovable     = "removable device"
	storageReasonNoByID        = "no stable /dev/disk/by-id path"
	storageReasonNoIdentity    = "no stable WWN or serial identity"
	storageReasonSignatureRead = "cannot verify that the blank disk has no existing signatures"
	storageCommandTimeout      = 5 * time.Second
)

var (
	partitionByIDPattern  = regexp.MustCompile(`-part[0-9]+$`)
	partitionNumberSuffix = regexp.MustCompile(`p?([0-9]+)$`)
	liveEnvironmentPaths  = []string{
		"/run/initramfs/live",
		"/run/cos/live_mode",
		"/run/elemental/live_mode",
	}
	lsblkStorageArgs = []string{
		"--json",
		"--bytes",
		"--paths",
		"--tree",
		"--output",
		"NAME,PATH,TYPE,SIZE,ROTA,RM,MODEL,SERIAL,WWN,FSTYPE,UUID,LABEL,MOUNTPOINTS,PTTYPE,PARTTYPE,PARTN",
	}
)

type storageCommandRunner func(name string, args ...string) ([]byte, error)

type observedStorageCollector struct {
	run             storageCommandRunner
	byIDPaths       func() (map[string][]string, error)
	liveEnvironment func() bool
}

func newObservedStorageCollector() *observedStorageCollector {
	return &observedStorageCollector{
		run:             runStorageCommand,
		byIDPaths:       collectByIDPaths,
		liveEnvironment: isLiveEnvironment,
	}
}

// sendObservedStorage collects a read-only, best-effort snapshot and sends it
// to the operator. The caller deliberately treats failures as informational so
// storage discovery cannot prevent machine registration.
func sendObservedStorage(conn *websocket.Conn) error {
	observed := collectObservedStorage()
	// Collection runs after the websocket is established and may legitimately
	// consume part of the registration deadline on hosts with many disks.
	refreshRegistrationDeadline(conn)
	return SendJSONData(conn, MsgObservedStorageConfig, observed)
}

func collectObservedStorage() *elementalv1.ObservedStorage {
	observed, err := newObservedStorageCollector().collect()
	if err != nil {
		log.Warningf("failed to collect observed storage: %v", err)
		return &elementalv1.ObservedStorage{}
	}
	return observed
}

func (c *observedStorageCollector) collect() (*elementalv1.ObservedStorage, error) {
	data, err := c.run("lsblk", lsblkStorageArgs...)
	if err != nil {
		return nil, fmt.Errorf("running lsblk: %w", err)
	}

	topology := lsblkOutput{}
	if err := json.Unmarshal(data, &topology); err != nil {
		return nil, fmt.Errorf("decoding lsblk JSON: %w", err)
	}

	byIDPaths, err := c.byIDPaths()
	if err != nil {
		log.Warningf("failed to enumerate /dev/disk/by-id: %v", err)
		byIDPaths = map[string][]string{}
	}

	live := c.liveEnvironment()
	records := buildDiskRecords(topology.BlockDevices)
	observed := &elementalv1.ObservedStorage{Devices: make([]elementalv1.ObservedStorageDevice, 0, len(records))}
	for _, record := range records {
		observed.Devices = append(observed.Devices, c.observeDisk(record, byIDPaths, live))
	}

	sort.SliceStable(observed.Devices, func(i, j int) bool {
		left := observed.Devices[i].ByID
		right := observed.Devices[j].ByID
		if left == "" {
			return false
		}
		if right == "" {
			return true
		}
		return left < right
	})
	return observed, nil
}

func runStorageCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storageCommandTimeout)
	defer cancel()
	// #nosec G204 -- name and args are fixed by this package and never contain user input.
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return output, nil
	}
	if ctx.Err() != nil {
		return output, fmt.Errorf("%s timed out after %s: %w", name, storageCommandTimeout, ctx.Err())
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		return nil, err
	}
	return output, fmt.Errorf("%w: %s", err, message)
}

func collectByIDPaths() (map[string][]string, error) {
	return collectByIDPathsFrom("/dev/disk/by-id")
}

func collectByIDPathsFrom(byIDDir string) (map[string][]string, error) {
	entries, err := os.ReadDir(byIDDir)
	if err != nil {
		return nil, err
	}

	paths := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() || partitionByIDPattern.MatchString(entry.Name()) {
			continue
		}
		byID := filepath.Join(byIDDir, entry.Name())
		resolved, err := filepath.EvalSymlinks(byID)
		if err != nil {
			continue
		}
		resolved = filepath.Clean(resolved)
		paths[resolved] = append(paths[resolved], byID)
	}
	return paths, nil
}

func isLiveEnvironment() bool {
	for _, path := range liveEnvironmentPaths {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name        string              `json:"name"`
	Path        string              `json:"path"`
	Type        string              `json:"type"`
	Size        flexibleInt64       `json:"size"`
	Rotational  flexibleBool        `json:"rota"`
	Removable   flexibleBool        `json:"rm"`
	Model       string              `json:"model"`
	Serial      string              `json:"serial"`
	WWN         string              `json:"wwn"`
	FSType      string              `json:"fstype"`
	UUID        string              `json:"uuid"`
	Label       string              `json:"label"`
	MountPoints nullableStringSlice `json:"mountpoints"`
	PTType      string              `json:"pttype"`
	PartType    string              `json:"parttype"`
	PartNumber  flexibleInt64       `json:"partn"`
	Children    []lsblkDevice       `json:"children"`
}

func (d lsblkDevice) devicePath() string {
	if path := strings.TrimSpace(d.Path); path != "" {
		return filepath.Clean(path)
	}
	if name := strings.TrimSpace(d.Name); name != "" {
		return filepath.Clean(name)
	}
	return ""
}

type flexibleInt64 int64

func (i *flexibleInt64) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || len(data) == 0 {
		*i = 0
		return nil
	}
	value := string(data)
	if data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return err
	}
	*i = flexibleInt64(parsed)
	return nil
}

type flexibleBool bool

func (b *flexibleBool) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || len(data) == 0 {
		*b = false
		return nil
	}
	value := string(data)
	if data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	switch strings.ToLower(value) {
	case "true", "1", "yes":
		*b = true
	case "false", "0", "no":
		*b = false
	default:
		return fmt.Errorf("invalid boolean %q", value)
	}
	return nil
}

type nullableStringSlice []string

func (s *nullableStringSlice) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || len(data) == 0 {
		*s = nil
		return nil
	}
	if data[0] == '"' {
		value := ""
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		if value != "" {
			*s = []string{value}
		}
		return nil
	}

	values := []json.RawMessage{}
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	result := make([]string, 0, len(values))
	for _, raw := range values {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		value := ""
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value != "" {
			result = append(result, value)
		}
	}
	*s = result
	return nil
}

type diskRecord struct {
	path          string
	disk          lsblkDevice
	descendants   map[string]lsblkDevice
	ancestorTypes map[string]struct{}
}

func buildDiskRecords(devices []lsblkDevice) []diskRecord {
	records := map[string]*diskRecord{}
	var walk func(lsblkDevice, []string)
	walk = func(device lsblkDevice, ancestors []string) {
		if strings.EqualFold(device.Type, "disk") {
			path := device.devicePath()
			if path == "" {
				return
			}
			record, found := records[path]
			if !found {
				record = &diskRecord{
					path:          path,
					disk:          device,
					descendants:   map[string]lsblkDevice{},
					ancestorTypes: map[string]struct{}{},
				}
				records[path] = record
			}
			for _, ancestor := range ancestors {
				record.ancestorTypes[ancestor] = struct{}{}
			}
			addDescendants(record.descendants, device.Children)
		}

		nextAncestors := append(append([]string(nil), ancestors...), device.Type)
		for _, child := range device.Children {
			walk(child, nextAncestors)
		}
	}
	for _, device := range devices {
		walk(device, nil)
	}

	result := make([]diskRecord, 0, len(records))
	for _, record := range records {
		result = append(result, *record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].path < result[j].path })
	return result
}

func addDescendants(result map[string]lsblkDevice, devices []lsblkDevice) {
	for _, device := range devices {
		path := device.devicePath()
		if path != "" && path != "." {
			result[path] = device
		}
		addDescendants(result, device.Children)
	}
}

func (r diskRecord) sortedDescendants() []lsblkDevice {
	result := make([]lsblkDevice, 0, len(r.descendants))
	for _, device := range r.descendants {
		result = append(result, device)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].devicePath() < result[j].devicePath() })
	return result
}

func (c *observedStorageCollector) observeDisk(record diskRecord, byIDPaths map[string][]string, live bool) elementalv1.ObservedStorageDevice {
	properties := map[string]string{}
	if record.disk.WWN == "" || record.disk.Serial == "" || record.disk.Model == "" {
		properties = c.udevProperties(record.path)
	}
	byID := canonicalByID(byIDPaths[record.path])
	if byID == "" {
		for path, candidates := range byIDPaths {
			if sameDevicePath(path, record.path) {
				byID = canonicalByID(candidates)
				break
			}
		}
	}

	wwn := firstNonEmpty(properties["ID_WWN"], record.disk.WWN, properties["ID_WWN_WITH_EXTENSION"])
	serial := firstNonEmpty(properties["ID_SERIAL_SHORT"], record.disk.Serial, properties["ID_SERIAL"])
	model := firstNonEmpty(record.disk.Model, properties["ID_MODEL"], properties["ID_MODEL_FROM_DATABASE"])
	descendants := record.sortedDescendants()
	partitions := observedPartitions(record.disk, descendants)
	systemDisk := isSystemDisk(record.disk, descendants)

	reasons := []string{}
	seenReasons := map[string]struct{}{}
	addReason := func(reason string) {
		if _, found := seenReasons[reason]; found {
			return
		}
		seenReasons[reason] = struct{}{}
		reasons = append(reasons, reason)
	}

	if systemDisk {
		addReason(storageReasonSystemDisk)
	} else if live {
		// The installation config is returned only after this snapshot is sent.
		// Until the installed system registers again, no blank disk can safely be
		// distinguished from the impending Elemental installation target.
		addReason(storageReasonLiveISO)
	}
	if bool(record.disk.Removable) {
		addReason(storageReasonRemovable)
	}
	if byID == "" {
		addReason(storageReasonNoByID)
	}
	if wwn == "" && serial == "" {
		addReason(storageReasonNoIdentity)
	}
	for _, reason := range topologyReasons(record, descendants) {
		addReason(reason)
	}
	for _, reason := range mountReasons(record.disk, descendants) {
		addReason(reason)
	}
	for _, reason := range c.layoutReasons(record, descendants) {
		addReason(reason)
	}

	return elementalv1.ObservedStorageDevice{
		ByID:              byID,
		WWN:               strings.TrimSpace(wwn),
		Serial:            strings.TrimSpace(serial),
		Model:             strings.TrimSpace(model),
		SizeBytes:         int64(record.disk.Size),
		Rotational:        bool(record.disk.Rotational),
		SystemDisk:        systemDisk,
		Partitions:        partitions,
		Eligible:          len(reasons) == 0,
		IneligibleReasons: reasons,
	}
}

func (c *observedStorageCollector) udevProperties(path string) map[string]string {
	properties := map[string]string{}
	data, err := c.run("udevadm", "info", "--query=property", "--name", path)
	if err != nil {
		log.Debugf("failed to inspect udev properties for %s: %v", path, err)
		return properties
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		properties[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return properties
}

func canonicalByID(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	candidates := append([]string(nil), paths...)
	sort.Slice(candidates, func(i, j int) bool {
		leftPriority := byIDPriority(filepath.Base(candidates[i]))
		rightPriority := byIDPriority(filepath.Base(candidates[j]))
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return candidates[i] < candidates[j]
	})
	return candidates[0]
}

func byIDPriority(name string) int {
	switch {
	case strings.HasPrefix(name, "wwn-"):
		return 0
	case strings.HasPrefix(name, "nvme-eui."), strings.HasPrefix(name, "nvme-eui-"):
		return 1
	case strings.HasPrefix(name, "nvme-uuid."), strings.HasPrefix(name, "nvme-uuid-"):
		return 2
	case strings.HasPrefix(name, "scsi-"):
		return 3
	case strings.HasPrefix(name, "ata-"):
		return 4
	case strings.HasPrefix(name, "virtio-"):
		return 5
	case strings.HasPrefix(name, "nvme-"):
		return 6
	case strings.HasPrefix(name, "usb-"):
		return 7
	default:
		return 8
	}
}

func sameDevicePath(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil {
		left = leftResolved
	}
	if rightErr == nil {
		right = rightResolved
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func observedPartitions(disk lsblkDevice, descendants []lsblkDevice) []elementalv1.ObservedStoragePartition {
	partitionDevices := partitionDevices(descendants)
	if len(partitionDevices) == 0 {
		if disk.FSType == "" {
			return nil
		}
		return []elementalv1.ObservedStoragePartition{{
			FilesystemType: disk.FSType,
			FilesystemUUID: disk.UUID,
			MountPoint:     firstMountPoint(disk),
		}}
	}

	result := make([]elementalv1.ObservedStoragePartition, 0, len(partitionDevices))
	for _, partition := range partitionDevices {
		result = append(result, elementalv1.ObservedStoragePartition{
			Number:         partitionNumber(partition),
			FilesystemType: partition.FSType,
			FilesystemUUID: partition.UUID,
			MountPoint:     firstMountPoint(partition),
		})
	}
	return result
}

func partitionDevices(descendants []lsblkDevice) []lsblkDevice {
	result := []lsblkDevice{}
	for _, device := range descendants {
		if strings.EqualFold(device.Type, "part") {
			result = append(result, device)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left := partitionNumber(result[i])
		right := partitionNumber(result[j])
		if left != right {
			return left < right
		}
		return result[i].devicePath() < result[j].devicePath()
	})
	return result
}

func partitionNumber(device lsblkDevice) int {
	if device.PartNumber > 0 {
		return int(device.PartNumber)
	}
	match := partitionNumberSuffix.FindStringSubmatch(device.devicePath())
	if len(match) != 2 {
		return 0
	}
	number, _ := strconv.Atoi(match[1])
	return number
}

func firstMountPoint(device lsblkDevice) string {
	for _, mountPoint := range device.MountPoints {
		if mountPoint = strings.TrimSpace(mountPoint); mountPoint != "" {
			return mountPoint
		}
	}
	return ""
}

func isSystemDisk(disk lsblkDevice, descendants []lsblkDevice) bool {
	devices := append([]lsblkDevice{disk}, descendants...)
	for _, device := range devices {
		label := strings.ToUpper(strings.TrimSpace(device.Label))
		if strings.HasPrefix(label, "COS_") || label == "EFI" || label == "EFI_SYSTEM" || label == "EFI SYSTEM PARTITION" {
			return true
		}
		if strings.EqualFold(device.PartType, "c12a7328-f81f-11d2-ba4b-00a0c93ec93b") {
			return true
		}
		for _, mountPoint := range device.MountPoints {
			if isSystemMountPoint(mountPoint) {
				return true
			}
		}
	}
	return false
}

func isSystemMountPoint(mountPoint string) bool {
	mountPoint = filepath.Clean(strings.TrimSpace(mountPoint))
	switch mountPoint {
	case "/", "/boot", "/boot/efi", "/oem":
		return true
	}
	return strings.HasPrefix(mountPoint, "/run/cos/") || strings.HasPrefix(mountPoint, "/run/elemental/")
}

func topologyReasons(record diskRecord, descendants []lsblkDevice) []string {
	unsafeTypes := map[string]struct{}{}
	for ancestor := range record.ancestorTypes {
		if isUnsafeBlockType(ancestor) {
			unsafeTypes[strings.ToLower(ancestor)] = struct{}{}
		}
	}
	for _, device := range descendants {
		if isUnsafeBlockType(device.Type) {
			unsafeTypes[strings.ToLower(device.Type)] = struct{}{}
		}
	}

	protectedSignatures := map[string]struct{}{}
	devices := append([]lsblkDevice{record.disk}, descendants...)
	for _, device := range devices {
		if isProtectedFilesystem(device.FSType) {
			protectedSignatures[device.FSType] = struct{}{}
		}
	}

	reasons := []string{}
	for _, blockType := range sortedKeys(unsafeTypes) {
		reasons = append(reasons, fmt.Sprintf("contains unsupported block topology %s", blockType))
	}
	for _, signature := range sortedKeys(protectedSignatures) {
		reasons = append(reasons, fmt.Sprintf("contains protected filesystem signature %s", signature))
	}
	return reasons
}

func isUnsafeBlockType(blockType string) bool {
	switch strings.ToLower(strings.TrimSpace(blockType)) {
	case "", "disk", "part":
		return false
	default:
		return true
	}
}

func isProtectedFilesystem(filesystem string) bool {
	switch strings.ToLower(strings.TrimSpace(filesystem)) {
	case "swap", "lvm2_member", "linux_raid_member", "crypto_luks", "mpath_member":
		return true
	default:
		return false
	}
}

func mountReasons(disk lsblkDevice, descendants []lsblkDevice) []string {
	mounts := map[string]struct{}{}
	devices := append([]lsblkDevice{disk}, descendants...)
	for _, device := range devices {
		for _, mountPoint := range device.MountPoints {
			if mountPoint = strings.TrimSpace(mountPoint); mountPoint != "" {
				mounts[mountPoint] = struct{}{}
			}
		}
	}
	reasons := []string{}
	for _, mountPoint := range sortedKeys(mounts) {
		reasons = append(reasons, fmt.Sprintf("mounted at %s", mountPoint))
	}
	return reasons
}

func (c *observedStorageCollector) layoutReasons(record diskRecord, descendants []lsblkDevice) []string {
	partitions := partitionDevices(descendants)
	switch len(partitions) {
	case 0:
		return c.wholeDiskLayoutReasons(record)
	case 1:
		return singlePartitionLayoutReasons(record.disk, partitions[0])
	default:
		return []string{fmt.Sprintf("contains %d partitions; v1 requires an empty disk, whole-disk XFS, or one XFS partition", len(partitions))}
	}
}

func (c *observedStorageCollector) wholeDiskLayoutReasons(record diskRecord) []string {
	filesystem := strings.TrimSpace(record.disk.FSType)
	if strings.EqualFold(filesystem, "xfs") {
		if strings.TrimSpace(record.disk.UUID) == "" {
			return []string{"whole-disk XFS filesystem has no UUID"}
		}
		return nil
	}
	if filesystem != "" {
		return []string{fmt.Sprintf("whole disk has unsupported filesystem %s; v1 requires XFS", filesystem)}
	}
	if strings.TrimSpace(record.disk.PTType) != "" {
		return []string{"contains a partition table without one usable XFS partition"}
	}

	empty, err := c.hasNoSignatures(record.path)
	if err != nil {
		log.Debugf("failed to inspect signatures on %s: %v", record.path, err)
		return []string{storageReasonSignatureRead}
	}
	if !empty {
		return []string{"contains an existing partition-table or filesystem signature without a usable XFS volume"}
	}
	return nil
}

func singlePartitionLayoutReasons(disk, partition lsblkDevice) []string {
	reasons := []string{}
	if filesystem := strings.TrimSpace(disk.FSType); filesystem != "" {
		reasons = append(reasons, fmt.Sprintf("whole disk also reports filesystem signature %s", filesystem))
	}
	filesystem := strings.TrimSpace(partition.FSType)
	if !strings.EqualFold(filesystem, "xfs") {
		if filesystem == "" {
			filesystem = "none"
		}
		reasons = append(reasons, fmt.Sprintf("single partition has filesystem %s; v1 requires XFS", filesystem))
	} else if strings.TrimSpace(partition.UUID) == "" {
		reasons = append(reasons, "single XFS partition has no UUID")
	}
	return reasons
}

type wipefsOutput struct {
	Signatures []struct {
		Type  string `json:"type"`
		Usage string `json:"usage"`
	} `json:"signatures"`
}

func (c *observedStorageCollector) hasNoSignatures(path string) (bool, error) {
	data, err := c.run("wipefs", "--no-act", "--json", path)
	if err != nil {
		return false, err
	}
	result := wipefsOutput{}
	if err := json.Unmarshal(data, &result); err != nil {
		return false, fmt.Errorf("decoding wipefs JSON: %w", err)
	}
	return len(result.Signatures) == 0, nil
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
