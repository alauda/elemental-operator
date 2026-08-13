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
	"sort"
	"strconv"
	"strings"
	"time"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
)

const storageCommandTimeout = 10 * time.Second

var (
	liveEnvironmentPaths = []string{
		"/run/initramfs/live",
		"/run/cos/live_mode",
		"/run/elemental/live_mode",
	}
	// Keep the column set compatible with util-linux 2.37 shipped in the
	// Elemental image. PARTN is deliberately not used.
	lsblkStorageArgs = []string{
		"--json",
		"--bytes",
		"--paths",
		"--tree",
		"--output",
		"NAME,KNAME,PATH,TYPE,SIZE,LOG-SEC,RO,ROTA,RM,MODEL,SERIAL,WWN,TRAN,FSTYPE,UUID,LABEL,MOUNTPOINTS,PTTYPE,PARTTYPE,PARTUUID,PKNAME",
	}
	findmntStorageArgs = []string{
		"--json",
		"--list",
		"--output",
		"TARGET,OPTIONS",
	}
)

type storageCommandRunner func(name string, args ...string) ([]byte, error)

type observedStorageCollector struct {
	run             storageCommandRunner
	byIDPaths       func() (map[string][]string, error)
	mountOptions    func() (map[string][]string, error)
	partitionStart  func(lsblkDevice) int64
	liveEnvironment func() bool
	bootID          func() string
}

func newObservedStorageCollector() *observedStorageCollector {
	collector := &observedStorageCollector{
		run:             runStorageCommand,
		byIDPaths:       collectByIDPaths,
		partitionStart:  partitionStartBytesFromSysfs,
		liveEnvironment: isLiveEnvironment,
		bootID:          currentBootID,
	}
	collector.mountOptions = collector.collectMountOptions
	return collector
}

func collectObservedStorage() *elementalv1.ObservedStorage {
	observed, err := newObservedStorageCollector().collect()
	if err != nil {
		log.Warningf("failed to collect observed storage: %v", err)
		return &elementalv1.ObservedStorage{BootID: currentBootID()}
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
	mountOptions := map[string][]string{}
	if c.mountOptions != nil {
		mountOptions, err = c.mountOptions()
		if err != nil {
			log.Warningf("failed to collect mount options: %v", err)
			mountOptions = map[string][]string{}
		}
	}

	records := flattenBlockTopology(topology.BlockDevices)
	ids := make(map[string]string, len(records))
	for path, record := range records {
		ids[path] = canonicalDeviceID(record.device, byIDPaths[path])
	}

	systemPaths, systemEvidence := classifySystemClosure(records)
	multipathBacking := multipathBackingPaths(records)
	health := c.collectMultipathHealth(records, ids)
	live := c.liveEnvironment != nil && c.liveEnvironment()

	paths := make([]string, 0, len(records))
	for path, record := range records {
		if isObservableLogicalDevice(record.device.Type) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	observed := &elementalv1.ObservedStorage{BootID: c.bootID()}
	for _, devicePath := range paths {
		record := records[devicePath]
		// Physical paths underneath a Multipath map, including raw partitions
		// that lsblk may expose in parallel with the aggregate map partitions,
		// are aliases of the Multipath topology. Report them only through the
		// aggregate map/member view so shared filesystem and PARTUUID identities
		// cannot appear twice in one objective report.
		if multipathBacking.Has(devicePath) {
			continue
		}

		deviceID := ids[devicePath]
		if deviceID == "" {
			// Preserve visibility for diagnostics while making the record
			// impossible to select through provider admission.
			deviceID = "unidentified:" + sanitizeDiagnosticID(devicePath)
		}

		role := elementalv1.ObservedStorageSystemRoleData
		evidence := append([]string(nil), systemEvidence[devicePath]...)
		if systemPaths.Has(devicePath) {
			role = elementalv1.ObservedStorageSystemRoleSystem
		} else if live || strings.HasPrefix(deviceID, "unidentified:") || record.topologyAmbiguous {
			role = elementalv1.ObservedStorageSystemRoleUnknown
			if live {
				evidence = append(evidence, "live environment cannot prove the installation target")
			}
			if strings.HasPrefix(deviceID, "unidentified:") {
				evidence = append(evidence, "no supported stable device identity")
			}
			if record.topologyAmbiguous {
				evidence = append(evidence, "block topology is ambiguous")
			}
		}

		signatures, signatureComplete := c.observeSignatures(record.device)
		if !signatureComplete {
			evidence = append(evidence, "wipefs signature inspection failed")
			if role == elementalv1.ObservedStorageSystemRoleData {
				role = elementalv1.ObservedStorageSystemRoleUnknown
			}
		}

		startBytes := partitionStartBytes(record.device)
		if c.partitionStart != nil {
			startBytes = c.partitionStart(record.device)
		}
		device := elementalv1.ObservedStorageDevice{
			ID:                 deviceID,
			Kind:               observedDeviceKind(record.device.Type),
			Path:               devicePath,
			StablePaths:        sortedUniqueStrings(byIDPaths[devicePath]),
			SizeBytes:          int64(record.device.Size),
			StartBytes:         startBytes,
			ReadOnly:           bool(record.device.ReadOnly),
			Rotational:         bool(record.device.Rotational),
			Removable:          bool(record.device.Removable),
			SystemRole:         role,
			SystemEvidence:     sortedUniqueStrings(evidence),
			PartitionTableType: strings.TrimSpace(record.device.PTType),
			Signatures:         signatures,
			Filesystem:         observedFilesystem(record.device),
			Mounts:             observedMounts(record.device, mountOptions),
			Transport:          strings.TrimSpace(record.device.Transport),
			Model:              strings.TrimSpace(record.device.Model),
			Serial:             strings.TrimSpace(record.device.Serial),
			WWN:                strings.TrimSpace(record.device.WWN),
			Consumers:          observedBlockConsumers(devicePath, records),
		}
		if parent := canonicalParentID(record, records, ids); parent != "" {
			device.ParentID = parent
		}
		if device.Kind == elementalv1.ObservedStorageDeviceMultipath {
			device.MemberIDs = observedMultipathMembers(devicePath, records, byIDPaths)
			if value, found := health[deviceID]; found {
				valueCopy := value
				device.Health = &valueCopy
			}
		}
		observed.Devices = append(observed.Devices, device)
	}

	sort.SliceStable(observed.Devices, func(i, j int) bool {
		return observed.Devices[i].ID < observed.Devices[j].ID
	})
	return observed, nil
}

func observedBlockConsumers(path string, records map[string]*blockRecord) []elementalv1.ObservedStorageConsumer {
	seen := stringSet{}
	queue := []string{path}
	consumers := []elementalv1.ObservedStorageConsumer{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		record := records[current]
		if record == nil {
			continue
		}
		children := make([]string, 0, len(record.children))
		for child := range record.children {
			children = append(children, child)
		}
		sort.Strings(children)
		for _, child := range children {
			if child == path || seen.Has(child) {
				continue
			}
			seen.Add(child)
			queue = append(queue, child)
			childRecord := records[child]
			if childRecord == nil {
				continue
			}
			mounts := make([]string, 0, len(childRecord.device.MountPoints))
			for _, mount := range childRecord.device.MountPoints {
				if value := strings.TrimSpace(mount); value != "" {
					mounts = append(mounts, filepath.Clean(value))
				}
			}
			consumers = append(consumers, elementalv1.ObservedStorageConsumer{
				Path:           child,
				Type:           strings.ToLower(strings.TrimSpace(childRecord.device.Type)),
				FilesystemType: strings.ToLower(strings.TrimSpace(childRecord.device.FSType)),
				Mounts:         sortedUniqueStrings(mounts),
			})
		}
	}
	sort.Slice(consumers, func(i, j int) bool {
		if consumers[i].Path != consumers[j].Path {
			return consumers[i].Path < consumers[j].Path
		}
		return consumers[i].Type < consumers[j].Type
	})
	return consumers
}

func runStorageCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storageCommandTimeout)
	defer cancel()
	// #nosec G204 -- callers use fixed command names and generated arguments.
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
		if entry.IsDir() {
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
	for path := range paths {
		paths[path] = sortedUniqueStrings(paths[path])
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

func currentBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (c *observedStorageCollector) collectMountOptions() (map[string][]string, error) {
	data, err := c.run("findmnt", findmntStorageArgs...)
	if err != nil {
		return nil, fmt.Errorf("running findmnt: %w", err)
	}
	decoded := findmntOutput{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decoding findmnt JSON: %w", err)
	}
	options := make(map[string][]string, len(decoded.Filesystems))
	for _, filesystem := range decoded.Filesystems {
		target := strings.TrimSpace(filesystem.Target)
		if target == "" {
			continue
		}
		options[filepath.Clean(target)] = sortedUniqueStrings(strings.Split(filesystem.Options, ","))
	}
	return options, nil
}

type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name          string              `json:"name"`
	KName         string              `json:"kname"`
	Path          string              `json:"path"`
	Type          string              `json:"type"`
	Size          flexibleInt64       `json:"size"`
	Start         flexibleInt64       `json:"start"`
	LogicalSector flexibleInt64       `json:"log-sec"`
	ReadOnly      flexibleBool        `json:"ro"`
	Rotational    flexibleBool        `json:"rota"`
	Removable     flexibleBool        `json:"rm"`
	Model         string              `json:"model"`
	Serial        string              `json:"serial"`
	WWN           string              `json:"wwn"`
	Transport     string              `json:"tran"`
	FSType        string              `json:"fstype"`
	UUID          string              `json:"uuid"`
	Label         string              `json:"label"`
	MountPoints   nullableStringSlice `json:"mountpoints"`
	Options       nullableStringSlice `json:"options"`
	PTType        string              `json:"pttype"`
	PartType      string              `json:"parttype"`
	PartUUID      string              `json:"partuuid"`
	PKName        string              `json:"pkname"`
	Children      []lsblkDevice       `json:"children"`
}

type findmntOutput struct {
	Filesystems []findmntFilesystem `json:"filesystems"`
}

type findmntFilesystem struct {
	Target  string `json:"target"`
	Options string `json:"options"`
}

func (d lsblkDevice) devicePath() string {
	if value := strings.TrimSpace(d.Path); value != "" {
		return filepath.Clean(value)
	}
	if value := strings.TrimSpace(d.Name); value != "" {
		return filepath.Clean(value)
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
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	*s = result
	return nil
}

type blockRecord struct {
	device            lsblkDevice
	parents           map[string]struct{}
	children          map[string]struct{}
	topologyAmbiguous bool
}

func flattenBlockTopology(devices []lsblkDevice) map[string]*blockRecord {
	records := map[string]*blockRecord{}
	var walk func(lsblkDevice, string)
	walk = func(device lsblkDevice, parent string) {
		path := device.devicePath()
		if path == "" || path == "." {
			return
		}
		record, found := records[path]
		if !found {
			record = &blockRecord{device: device, parents: map[string]struct{}{}, children: map[string]struct{}{}}
			records[path] = record
		} else if !sameBlockFacts(record.device, device) {
			record.topologyAmbiguous = true
		}
		if parent != "" {
			record.parents[parent] = struct{}{}
			if parentRecord := records[parent]; parentRecord != nil {
				parentRecord.children[path] = struct{}{}
			}
		}
		for _, child := range device.Children {
			walk(child, path)
		}
	}
	for _, device := range devices {
		walk(device, "")
	}
	return records
}

func sameBlockFacts(left, right lsblkDevice) bool {
	return left.devicePath() == right.devicePath() &&
		strings.EqualFold(left.Type, right.Type) &&
		int64(left.Size) == int64(right.Size) &&
		strings.EqualFold(strings.TrimSpace(left.UUID), strings.TrimSpace(right.UUID))
}

func isObservableLogicalDevice(deviceType string) bool {
	switch strings.ToLower(strings.TrimSpace(deviceType)) {
	case "disk", "part", "mpath":
		return true
	default:
		return false
	}
}

func observedDeviceKind(deviceType string) elementalv1.ObservedStorageDeviceKind {
	switch strings.ToLower(strings.TrimSpace(deviceType)) {
	case "part":
		return elementalv1.ObservedStorageDevicePartition
	case "mpath":
		return elementalv1.ObservedStorageDeviceMultipath
	default:
		return elementalv1.ObservedStorageDeviceDirectDisk
	}
}

func canonicalDeviceID(device lsblkDevice, stablePaths []string) string {
	if strings.EqualFold(device.Type, "part") {
		if value := canonicalPartUUID(device.PartUUID); value != "" {
			return "partuuid:" + value
		}
		return ""
	}
	if strings.EqualFold(device.Type, "mpath") {
		for _, stable := range stablePaths {
			base := filepath.Base(stable)
			if strings.HasPrefix(base, "dm-uuid-mpath-") {
				if value := canonicalHexIdentity(strings.TrimPrefix(base, "dm-uuid-mpath-")); value != "" {
					return "wwid:" + value
				}
			}
		}
		if value := canonicalHexIdentity(device.WWN); value != "" {
			return "wwid:" + value
		}
		if name := filepath.Base(device.devicePath()); strings.TrimSpace(name) != "" {
			if value := canonicalHexIdentity(name); value != "" {
				return "wwid:" + value
			}
		}
		return ""
	}

	for _, stable := range stablePaths {
		base := filepath.Base(stable)
		switch {
		case strings.HasPrefix(base, "wwn-"):
			return "wwn:" + canonicalHexIdentity(strings.TrimPrefix(base, "wwn-"))
		case strings.HasPrefix(base, "nvme-eui."):
			return "nvme-eui:" + canonicalHexIdentity(strings.TrimPrefix(base, "nvme-eui."))
		case strings.HasPrefix(base, "nvme-eui-"):
			return "nvme-eui:" + canonicalHexIdentity(strings.TrimPrefix(base, "nvme-eui-"))
		case strings.HasPrefix(base, "nvme-uuid."):
			return "nvme-nguid:" + canonicalHexIdentity(strings.TrimPrefix(base, "nvme-uuid."))
		case strings.HasPrefix(base, "nvme-uuid-"):
			return "nvme-nguid:" + canonicalHexIdentity(strings.TrimPrefix(base, "nvme-uuid-"))
		}
	}
	if value := canonicalHexIdentity(device.WWN); value != "" {
		return "wwn:" + value
	}
	if localTransport(device.Transport) && strings.TrimSpace(device.Serial) != "" && strings.TrimSpace(device.Model) != "" {
		return "serial:" + canonicalSerialPart(device.Model) + ":" + canonicalSerialPart(device.Serial)
	}
	return ""
}

func canonicalPartUUID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "{")
	value = strings.TrimSuffix(value, "}")
	if value == "" || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") || strings.Contains(value, "--") {
		return ""
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && r != '-' {
			return ""
		}
	}
	return value
}

func canonicalHexIdentity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "0x")
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, ":", "")
	value = strings.ReplaceAll(value, ".", "")
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return value
}

func canonicalSerialPart(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func localTransport(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ata", "sata", "nvme", "virtio", "mmc":
		return true
	default:
		return false
	}
}

func canonicalParentID(record *blockRecord, records map[string]*blockRecord, ids map[string]string) string {
	if record == nil || len(record.parents) != 1 {
		return ""
	}
	for parent := range record.parents {
		if records[parent] != nil {
			return ids[parent]
		}
	}
	return ""
}

func partitionStartBytes(device lsblkDevice) int64 {
	sector := int64(device.LogicalSector)
	if sector <= 0 {
		sector = 512
	}
	start := int64(device.Start)
	if start <= 0 || start > (1<<63-1)/sector {
		return 0
	}
	return start * sector
}

func partitionStartBytesFromSysfs(device lsblkDevice) int64 {
	return partitionStartBytesFromSysfsRoot(device, "/sys/class/block")
}

func partitionStartBytesFromSysfsRoot(device lsblkDevice, sysfsRoot string) int64 {
	if !strings.EqualFold(strings.TrimSpace(device.Type), "part") {
		return 0
	}
	kernelPath := strings.TrimSpace(device.KName)
	kernelName := strings.TrimPrefix(kernelPath, "/dev/")
	if kernelName == kernelPath || kernelName == "" || kernelName == "." || strings.ContainsAny(kernelName, `/\\`) {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(sysfsRoot, kernelName, "start"))
	if err != nil {
		return 0
	}
	start, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	sector := int64(device.LogicalSector)
	if sector <= 0 {
		sector = 512
	}
	if err != nil || start <= 0 || start > (1<<63-1)/sector {
		return 0
	}
	return start * sector
}

func observedFilesystem(device lsblkDevice) *elementalv1.ObservedStorageFilesystem {
	if strings.TrimSpace(device.FSType) == "" {
		return nil
	}
	return &elementalv1.ObservedStorageFilesystem{
		Type: strings.TrimSpace(device.FSType),
		UUID: strings.TrimSpace(device.UUID),
	}
}

func observedMounts(device lsblkDevice, optionsByTarget map[string][]string) []elementalv1.ObservedStorageMount {
	mounts := make([]elementalv1.ObservedStorageMount, 0, len(device.MountPoints))
	for i, mountPath := range device.MountPoints {
		mountPath = strings.TrimSpace(mountPath)
		if mountPath == "" {
			continue
		}
		mount := elementalv1.ObservedStorageMount{Path: filepath.Clean(mountPath)}
		if options, found := optionsByTarget[mount.Path]; found {
			mount.Options = append([]string(nil), options...)
		} else if i < len(device.Options) {
			mount.Options = sortedUniqueStrings(strings.Split(device.Options[i], ","))
		} else if len(device.Options) == 1 {
			mount.Options = sortedUniqueStrings(strings.Split(device.Options[0], ","))
		}
		mounts = append(mounts, mount)
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Path < mounts[j].Path })
	return mounts
}

type wipefsOutput struct {
	Signatures []map[string]any `json:"signatures"`
}

func (c *observedStorageCollector) observeSignatures(device lsblkDevice) ([]elementalv1.ObservedStorageSignature, bool) {
	signatures := []elementalv1.ObservedStorageSignature{}
	if value := strings.TrimSpace(device.PTType); value != "" {
		signatures = append(signatures, elementalv1.ObservedStorageSignature{Type: "partition-table", Value: value})
	}
	if value := strings.TrimSpace(device.FSType); value != "" {
		signatures = append(signatures, elementalv1.ObservedStorageSignature{Type: "filesystem", Value: value})
	}
	data, err := c.run("wipefs", "--json", "--no-act", device.devicePath())
	if err != nil {
		return sortedSignatures(signatures), false
	}
	decoded := wipefsOutput{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			return sortedSignatures(signatures), false
		}
	}
	for _, item := range decoded.Signatures {
		usage := stringValue(item["usage"])
		typeValue := stringValue(item["type"])
		value := typeValue
		if label := stringValue(item["label"]); label != "" {
			value = typeValue + ":" + label
		}
		if usage == "" {
			usage = "signature"
		}
		if value != "" {
			signatures = append(signatures, elementalv1.ObservedStorageSignature{Type: usage, Value: value})
		}
	}
	return sortedSignatures(signatures), true
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func sortedSignatures(in []elementalv1.ObservedStorageSignature) []elementalv1.ObservedStorageSignature {
	seen := map[string]struct{}{}
	out := make([]elementalv1.ObservedStorageSignature, 0, len(in))
	for _, signature := range in {
		key := signature.Type + "\x00" + signature.Value
		if _, found := seen[key]; found {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, signature)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Value < out[j].Value
	})
	return out
}

func classifySystemClosure(records map[string]*blockRecord) (stringSet, map[string][]string) {
	system := stringSet{}
	evidence := map[string][]string{}
	queue := []string{}
	for path, record := range records {
		if reason := systemSeedEvidence(record.device); reason != "" {
			system.Add(path)
			evidence[path] = append(evidence[path], reason)
			queue = append(queue, path)
		}
	}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		record := records[path]
		if record == nil {
			continue
		}
		for related := range unionStringSets(record.parents, record.children) {
			if system.Has(related) {
				continue
			}
			system.Add(related)
			evidence[related] = append(evidence[related], "topology closure of system device "+path)
			queue = append(queue, related)
		}
	}
	return system, evidence
}

func systemSeedEvidence(device lsblkDevice) string {
	label := strings.ToUpper(strings.TrimSpace(device.Label))
	if strings.HasPrefix(label, "COS_") || label == "EFI" || label == "EFI_SYSTEM" || label == "EFI SYSTEM PARTITION" {
		return "system filesystem label " + label
	}
	if strings.EqualFold(device.PartType, "c12a7328-f81f-11d2-ba4b-00a0c93ec93b") {
		return "EFI system partition type"
	}
	for _, mountPoint := range device.MountPoints {
		mountPoint = filepath.Clean(strings.TrimSpace(mountPoint))
		switch mountPoint {
		case "/", "/boot", "/boot/efi", "/oem", "/run/elemental/persistent", "/run/cos/persistent":
			return "system mount " + mountPoint
		}
		if strings.HasPrefix(mountPoint, "/run/cos/") || strings.HasPrefix(mountPoint, "/run/elemental/") {
			return "Elemental system mount " + mountPoint
		}
	}
	return ""
}

func multipathBackingPaths(records map[string]*blockRecord) stringSet {
	aggregate := stringSet{}
	queue := []string{}
	for path, record := range records {
		if strings.EqualFold(record.device.Type, "mpath") {
			aggregate.Add(path)
			queue = append(queue, path)
		}
	}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		record := records[path]
		if record == nil {
			continue
		}
		for child := range record.children {
			if !aggregate.Has(child) {
				aggregate.Add(child)
				queue = append(queue, child)
			}
		}
	}

	backing := stringSet{}
	queue = queue[:0]
	for _, record := range records {
		if !strings.EqualFold(record.device.Type, "mpath") {
			continue
		}
		for parent := range record.parents {
			if !aggregate.Has(parent) && !backing.Has(parent) {
				backing.Add(parent)
				queue = append(queue, parent)
			}
		}
	}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		record := records[path]
		if record == nil {
			continue
		}
		for child := range record.children {
			if aggregate.Has(child) || backing.Has(child) {
				continue
			}
			backing.Add(child)
			queue = append(queue, child)
		}
	}
	return backing
}

func observedMultipathMembers(path string, records map[string]*blockRecord, byIDPaths map[string][]string) []string {
	record := records[path]
	if record == nil {
		return nil
	}
	members := []string{}
	for parent := range record.parents {
		stable := byIDPaths[parent]
		added := false
		for _, candidate := range stable {
			base := filepath.Base(candidate)
			if strings.HasPrefix(base, "scsi-") || strings.HasPrefix(base, "wwn-") {
				members = append(members, "scsi-path:"+base)
				added = true
			}
		}
		if !added {
			members = append(members, "path:"+sanitizeDiagnosticID(parent))
		}
	}
	return sortedUniqueStrings(members)
}

func (c *observedStorageCollector) collectMultipathHealth(records map[string]*blockRecord, ids map[string]string) map[string]elementalv1.ObservedStorageMultipathHealth {
	result := map[string]elementalv1.ObservedStorageMultipathHealth{}
	hasMultipath := false
	for _, record := range records {
		if strings.EqualFold(record.device.Type, "mpath") {
			hasMultipath = true
			break
		}
	}
	if !hasMultipath {
		return result
	}
	data, err := c.run("multipathd", "show", "paths", "raw", "format", "%w|%d|%t")
	if err != nil {
		return result
	}
	byWWID := map[string]elementalv1.ObservedStorageMultipathHealth{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "|")
		if len(fields) < 3 {
			continue
		}
		wwid := canonicalHexIdentity(fields[0])
		if wwid == "" {
			continue
		}
		health := byWWID[wwid]
		health.TotalPaths++
		switch strings.ToLower(strings.TrimSpace(fields[2])) {
		case "active", "ready", "running", "up":
			health.ActivePaths++
		}
		byWWID[wwid] = health
	}
	for path, record := range records {
		if !strings.EqualFold(record.device.Type, "mpath") {
			continue
		}
		id := ids[path]
		wwid := strings.TrimPrefix(id, "wwid:")
		if value, found := byWWID[wwid]; found {
			result[id] = value
		}
	}
	return result
}

type stringSet map[string]struct{}

func (s stringSet) Add(value string) { s[value] = struct{}{} }
func (s stringSet) Has(value string) bool {
	_, found := s[value]
	return found
}

func unionStringSets(left, right map[string]struct{}) stringSet {
	result := stringSet{}
	for value := range left {
		result.Add(value)
	}
	for value := range right {
		result.Add(value)
	}
	return result
}

func sortedUniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		if _, found := seen[value]; found {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sanitizeDiagnosticID(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
