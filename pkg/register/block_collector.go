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

	"github.com/twpayne/go-vfs"

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
	// Elemental image. PARTN is deliberately not used. PARTLABEL (the GPT
	// partition name) is what the install path matches old system layouts on.
	lsblkStorageArgs = []string{
		"--json",
		"--bytes",
		"--paths",
		"--tree",
		"--output",
		"NAME,KNAME,PATH,TYPE,SIZE,LOG-SEC,RO,ROTA,RM,MODEL,SERIAL,WWN,TRAN,FSTYPE,UUID,LABEL,MOUNTPOINTS,PTTYPE,PARTTYPE,PARTUUID,PARTLABEL,PKNAME",
	}
)

type storageCommandRunner func(name string, args ...string) ([]byte, error)

// blockCollector reads the host's block devices: the lsblk tree, the
// /dev/disk/by-id aliases and the sysfs bus evidence canonicalDeviceID needs.
// The install path uses it to describe disks (CollectBlockDevices).
type blockCollector struct {
	run       storageCommandRunner
	byIDPaths func() (map[string][]string, error)
	busProbe  func(lsblkDevice) busProbe
}

func newBlockCollector() *blockCollector {
	return &blockCollector{
		run:       runStorageCommand,
		byIDPaths: collectByIDPaths,
		busProbe:  probeBusFromSysfs,
	}
}

// blockInventory is the lsblk tree with each device's canonical identity.
type blockInventory struct {
	records   map[string]*blockRecord
	byIDPaths map[string][]string
	ids       map[string]string
	buses     map[string]string
	probes    map[string]busProbe
}

func (c *blockCollector) inventory() (*blockInventory, error) {
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

	records := flattenBlockTopology(topology.BlockDevices)
	inventory := &blockInventory{
		records:   records,
		byIDPaths: byIDPaths,
		ids:       make(map[string]string, len(records)),
		buses:     make(map[string]string, len(records)),
		probes:    make(map[string]busProbe, len(records)),
	}
	for path, record := range records {
		stable := stablePathsForDevice(record.device, byIDPaths)
		if c.busProbe != nil {
			inventory.probes[path] = c.busProbe(record.device)
		}
		inventory.buses[path] = localBus(record.device, stable, inventory.probes[path])
		inventory.ids[path] = canonicalDeviceID(record.device, stable, inventory.buses[path])
	}
	return inventory, nil
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

func stablePathsForDevice(device lsblkDevice, byIDPaths map[string][]string) []string {
	paths := append([]string(nil), byIDPaths[device.devicePath()]...)
	kernelPath := filepath.Clean(strings.TrimSpace(device.KName))
	if filepath.IsAbs(kernelPath) && strings.HasPrefix(kernelPath, "/dev/") {
		paths = append(paths, byIDPaths[kernelPath]...)
	}
	return sortedUniqueStrings(paths)
}

// LiveEnvironment reports whether fs carries the markers of a live (ISO or
// PXE) boot. The install path asks through its own filesystem so tests can
// fake it.
func LiveEnvironment(fs vfs.FS) bool {
	for _, path := range liveEnvironmentPaths {
		if _, err := fs.Stat(path); err == nil {
			return true
		}
	}
	return false
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
	PartLabel     string              `json:"partlabel"`
	PKName        string              `json:"pkname"`
	Children      []lsblkDevice       `json:"children"`
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

// canonicalDeviceID derives the management identity for one logical block
// device. bus is the proven-local bus name from localBus, or "" when nothing
// proves the bus is local; it gates the serial tier only.
func canonicalDeviceID(device lsblkDevice, stablePaths []string, bus string) string {
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

	virtioSerial := ""
	for _, stable := range stablePaths {
		base := filepath.Base(stable)
		// A malformed alias must fall through to the next candidate instead of
		// returning a prefix with an empty payload. "wwn:" is not empty, so it
		// would pass for a real identity. udev builds
		// wwn-0x<WWID minus its first character>, so any WWID lacking a
		// designator-type digit produces exactly such an alias.
		switch {
		case strings.HasPrefix(base, "wwn-"):
			if value := canonicalHexIdentity(strings.TrimPrefix(base, "wwn-")); value != "" {
				return "wwn:" + value
			}
		case strings.HasPrefix(base, "nvme-eui."):
			if value := canonicalHexIdentity(strings.TrimPrefix(base, "nvme-eui.")); value != "" {
				return "nvme-eui:" + value
			}
		case strings.HasPrefix(base, "nvme-eui-"):
			if value := canonicalHexIdentity(strings.TrimPrefix(base, "nvme-eui-")); value != "" {
				return "nvme-eui:" + value
			}
		case strings.HasPrefix(base, "nvme-uuid."):
			if value := canonicalHexIdentity(strings.TrimPrefix(base, "nvme-uuid.")); value != "" {
				return "nvme-nguid:" + value
			}
		case strings.HasPrefix(base, "nvme-uuid-"):
			if value := canonicalHexIdentity(strings.TrimPrefix(base, "nvme-uuid-")); value != "" {
				return "nvme-nguid:" + value
			}
		case strings.HasPrefix(base, "virtio-"):
			// Recorded, not returned: a wwn-/nvme- alias further along the
			// slice still outranks a virtio serial alias.
			if virtioSerial == "" {
				virtioSerial = canonicalSerialPart(strings.TrimPrefix(base, "virtio-"))
			}
		}
	}
	if value := canonicalHexIdentity(device.WWN); value != "" {
		return "wwn:" + value
	}
	// virtio-blk reports neither an lsblk TRAN nor a model. The
	// by-id/virtio-<serial> alias udev builds from the kernel-exposed virtio
	// serial is the same class of stable alias as wwn-/nvme-eui-, so consume it
	// directly; the sysfs bus probe would reach the same identity through the
	// serial fallback below, but the alias is the more direct fact. A virtio
	// disk with no serial gets no alias and therefore still gets no identity.
	if virtioSerial != "" {
		return "serial:virtio:" + virtioSerial
	}
	if bus != "" && strings.TrimSpace(device.Serial) != "" {
		// A disk without a usable model, which virtio-blk never has, is scoped
		// by the bus name instead so the identity keeps the two-segment shape
		// provider admission validates.
		model := canonicalSerialPart(device.Model)
		if model == "" {
			model = canonicalSerialPart(bus)
		}
		serial := canonicalSerialPart(device.Serial)
		if model != "" && serial != "" {
			return "serial:" + model + ":" + serial
		}
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

// localTransport reports whether lsblk classified the device onto a bus that
// cannot be a shared fabric. It is one of the facts localBus accepts; see there
// for why it cannot be the only one.
//
// spi is SCSI Parallel Interface, a short local cable, not a fabric. QEMU puts
// disks behind an emulated SPI controller.
func localTransport(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ata", "sata", "spi", "nvme", "virtio", "mmc":
		return true
	default:
		return false
	}
}

// busProbe carries the sysfs facts that decide whether a disk's bus is local.
// lsblk's TRAN column is a convenience heuristic derived from the same sysfs
// tree: it is empty for every paravirtualised SCSI controller (virtio-scsi,
// vmw_pvscsi, storvsc) and, on the util-linux shipped in the OS image, for
// virtio-blk as well. The collector therefore reads the tree directly.
type busProbe struct {
	// bus names a bus device found among the block device's sysfs ancestors:
	// "virtio" for virtio-blk and virtio-scsi, "ata" for a libata port.
	bus string
	// driver is the SCSI host driver (scsi_host/hostN/proc_name) when the
	// device sits under a SCSI host, whether or not that driver is local.
	driver string
}

// localSCSIDriver lists SCSI host drivers that only ever drive a controller
// inside this machine: paravirtualised controllers, libata and parallel SCSI.
// SAS, FC and iSCSI HBAs are deliberately absent. Devices behind them may be
// attached to other hosts too, and they publish a WWN, which is the identity
// tier that does not depend on this question.
func localSCSIDriver(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "virtio_scsi", "vmw_pvscsi", "storvsc_host_t", "ahci", "ata_piix", "sym53c8xx", "mptspi":
		return true
	default:
		return false
	}
}

func probeBusFromSysfs(device lsblkDevice) busProbe {
	return probeBusFromSysfsRoot(device, "/sys/class/block")
}

// probeBusFromSysfsRoot resolves <sysfsRoot>/<kname>/device, which points at
// the SCSI logical unit (…/virtio4/host2/target2:0:0/2:0:0:0) or, for
// virtio-blk, at the virtio device itself (…/0000:00:0c.0/virtio3), and reads
// the bus evidence off that path. Anything missing simply yields no evidence.
func probeBusFromSysfsRoot(device lsblkDevice, sysfsRoot string) busProbe {
	kernelName := sysfsKernelName(device)
	if kernelName == "" {
		return busProbe{}
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(sysfsRoot, kernelName, "device"))
	if err != nil {
		return busProbe{}
	}
	probe := busProbe{}
	components := strings.Split(filepath.ToSlash(resolved), "/")
	hostIndex := -1
	for i, component := range components {
		switch {
		case hasNumericSuffix(component, "virtio"):
			probe.bus = "virtio"
		case hasNumericSuffix(component, "ata"):
			probe.bus = "ata"
		case hasNumericSuffix(component, "host"):
			hostIndex = i
		}
	}
	if hostIndex >= 0 {
		hostDir := strings.Join(components[:hostIndex+1], "/")
		if data, err := os.ReadFile(filepath.Join(hostDir, "scsi_host", components[hostIndex], "proc_name")); err == nil {
			probe.driver = strings.TrimSpace(string(data))
		}
	}
	return probe
}

func hasNumericSuffix(value, prefix string) bool {
	rest, found := strings.CutPrefix(value, prefix)
	if !found || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// localBus returns the name of the bus a disk is proven to hang off, or "" when
// nothing proves the bus is local. The serial identity tier is only admissible
// with such proof: a serial identifies a device, but cannot show the device is
// not also attached to another host, which on FC, iSCSI or shared SAS it may
// be. Fabric devices publish a WWN under the SCSI standard, so a device that
// reaches this question without one is either local or broken, and only
// proven-local is accepted. Absent evidence is not evidence of absence.
//
// Accepted proof, any one of: lsblk's TRAN, a virtio or libata ancestor in
// sysfs, a local SCSI host driver, or a by-id alias udev only builds for a
// local bus (ata-, mmc-; virtio- is consumed as an identity of its own).
func localBus(device lsblkDevice, stablePaths []string, probe busProbe) string {
	if transport := strings.ToLower(strings.TrimSpace(device.Transport)); localTransport(transport) {
		return transport
	}
	switch probe.bus {
	case "virtio", "ata":
		return probe.bus
	}
	if localSCSIDriver(probe.driver) {
		return strings.ToLower(strings.TrimSpace(probe.driver))
	}
	for _, stable := range stablePaths {
		switch base := filepath.Base(stable); {
		case strings.HasPrefix(base, "ata-"):
			return "ata"
		case strings.HasPrefix(base, "mmc-"):
			return "mmc"
		}
	}
	return ""
}

// unidentifiedReason names the fact that is missing for a device that produced
// no canonical identity, so the evidence tells an operator what to change
// rather than only that something is wrong.
func unidentifiedReason(device lsblkDevice, bus string, probe busProbe) string {
	switch {
	case strings.EqualFold(device.Type, "part"):
		return "partition has no PARTUUID"
	case strings.EqualFold(device.Type, "mpath"):
		return "multipath WWID is not hexadecimal; the map carries no NAA or EUI designator"
	case strings.TrimSpace(device.Serial) == "":
		return "no WWN or EUI and no serial; assign the disk a WWN or a unique serial"
	case bus == "":
		return fmt.Sprintf("no WWN or EUI and bus not proven local (transport=%q, driver=%q); set a WWN on the disk or attach it through virtio, SATA or NVMe",
			strings.TrimSpace(device.Transport), probe.driver)
	default:
		return "no WWN or EUI and neither model nor bus name usable for a serial identity"
	}
}

// sysfsKernelName returns the bare kernel name (sda, vdb, dm-0) for looking the
// device up under /sys/class/block, or "" when KName is missing or would
// escape that directory.
func sysfsKernelName(device lsblkDevice) string {
	kernelPath := strings.TrimSpace(device.KName)
	kernelName := strings.TrimPrefix(kernelPath, "/dev/")
	if kernelName == kernelPath || kernelName == "" || kernelName == "." || strings.ContainsAny(kernelName, `/\\`) {
		return ""
	}
	return kernelName
}

type wipefsOutput struct {
	Signatures []map[string]any `json:"signatures"`
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
