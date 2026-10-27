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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/twpayne/go-vfs"
	"k8s.io/apimachinery/pkg/api/resource"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/register"
)

// The pre-install step, shared by both backends.
//
// Before either backend writes anything, register turns what the
// configuration names — a disk ID the operator released, a device path
// (symlinks included), a device-selector, or nothing — into exactly one whole
// disk, checks that it is safe to overwrite, cleans stale system disks (see
// stale.go) and hands the backend the disk's kernel path (/dev/sdb). The
// toolkit CLI would resolve a symlink on its own; kbimg would not: it matches
// the literal path against parted's output and derives partition nodes by
// appending numbers to it. Disk facts come from the storage observer's lsblk
// collector, so a disk has the same ID here, in the observer and on the
// MachineInventory.

const (
	devDir        = "/dev/"
	diskByIDDir   = "/dev/disk/by-id"
	sysClassBlock = "/sys/class/block"
	sysDevBlock   = "/sys/dev/block"

	// liveMediaMount is where dracut mounts the medium the live system booted
	// from; liveMediaLabel is the filesystem label of every Elemental ISO.
	liveMediaMount = "/run/initramfs/live"
	liveMediaLabel = "COS_LIVE"

	// maxLinkHops bounds symlink resolution like the kernel's MAXSYMLINKS.
	maxLinkHops = 40

	blockTypeDisk  = "disk"
	blockTypePart  = "part"
	blockTypeMpath = "mpath"
	blockTypeRom   = "rom"
)

// Reasons the pre-install step stops. They are named after the conditions
// the operator will report (InstallTargetReady and StaleSystemDisks);
// TargetAmbiguous has no condition counterpart yet.
const (
	reasonTargetNotObserved = "TargetNotObserved"
	reasonTargetIneligible  = "TargetIneligible"
	reasonTargetAmbiguous   = "TargetAmbiguous"
	reasonTargetMismatch    = "TargetMismatch"
	reasonBlocked           = "Blocked"
	reasonWipeFailed        = "WipeFailed"
)

// installDiskError is a failure of the pre-install step. Unless Reason is
// WipeFailed, no disk has been written when it is returned.
type installDiskError struct {
	Reason  string
	Message string
	// Stale is the stale system disk report when the failure concerns them.
	Stale *staleDiskReport
}

func (e *installDiskError) Error() string {
	return e.Reason + ": " + e.Message
}

func diskErrorf(reason, format string, args ...any) *installDiskError {
	return &installDiskError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// commandRunner runs the disk commands of the pre-install step. It is the
// method kubeosRunner has, so the KubeOS tests record both backends' commands
// with one fake.
type commandRunner interface {
	Output(name string, args ...string) ([]byte, error)
}

// blockNode is the device number of a block special file.
type blockNode struct {
	major, minor uint32
}

func (n blockNode) String() string {
	return fmt.Sprintf("%d:%d", n.major, n.minor)
}

// diskOptions are the seams of the pre-install step. Sysfs and /dev are
// always read through the installer's filesystem.
type diskOptions struct {
	// runner runs the wipe sequence.
	runner commandRunner
	// inspect runs lsblk and `wipefs --no-act` for the collector. Nil keeps
	// pkg/register's runner, which bounds every command with a timeout.
	inspect func(name string, args ...string) ([]byte, error)
	// statNode returns the device number of a block special file. Nil stats
	// the file through the installer's filesystem.
	statNode func(path string) (blockNode, error)
	// busProbe replaces the collector's sysfs bus probe. Nil keeps it.
	busProbe func(kernelName string) (bus, driver string)
}

func defaultDiskOptions() diskOptions {
	return diskOptions{runner: execKubeOSRunner{}}
}

// diskFacts is what the pre-install step knows about one whole disk or
// multipath map, or about the device an explicit install.device names.
type diskFacts struct {
	dev *register.BlockDevice
	// kname is the kernel name (sdb); path is the kernel path (/dev/sdb), the
	// form both backends receive.
	kname string
	path  string
	// majMin is the device number sysfs reports for kname.
	majMin    string
	sizeBytes int64
	// partitions are the lsblk children of type part.
	partitions []*register.BlockDevice
	// problems are the safety gates the device fails as an install target.
	problems []string
	// liveMedia marks the medium the live system runs from: never selected,
	// never wiped.
	liveMedia bool
	// virtual marks devices automatic selection never considers: zram, ram
	// disks, loop and network block devices.
	virtual bool
	layout  systemLayout
	// data is what makes the device look like someone's data: signatures and
	// partitions outside an old system layout, or content wipefs could not
	// read. Automatic selection skips such devices.
	data []string
}

// displayID is the canonical ID, or a diagnostic placeholder nothing can
// select.
func (d *diskFacts) displayID() string {
	if d.dev.ID != "" {
		return d.dev.ID
	}
	return register.UnidentifiedDeviceID(d.path)
}

func (d *diskFacts) describe() string {
	return fmt.Sprintf("%s %s %d %q", d.displayID(), d.path, d.sizeBytes, d.dev.Model)
}

// diskInventory is one look at the host's disks.
type diskInventory struct {
	snapshot *register.BlockSnapshot
	// disks are the whole disks and multipath maps, sorted by kernel path.
	disks []*diskFacts
	// rootDisk is the disk / is mounted from; empty when / is not on a disk,
	// as in a live environment.
	rootDisk string
}

func (inv *diskInventory) disk(path string) *diskFacts {
	for _, d := range inv.disks {
		if d.path == path {
			return d
		}
	}
	return nil
}

// describeDisks lists every disk with the reason automatic selection skips
// it, for error messages that must let a person pick the right one.
func (inv *diskInventory) describeDisks() string {
	lines := make([]string, 0, len(inv.disks))
	for _, d := range inv.disks {
		line := d.describe()
		if reason := automaticExclusion(d); reason != "" {
			line += " (excluded: " + reason + ")"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "; ")
}

func (i *installer) collectDiskInventory() (*diskInventory, error) {
	snapshot, err := register.CollectBlockDevices(register.BlockCollectorOptions{
		Run:       i.disk.inspect,
		ByIDPaths: i.byIDPaths,
		BusProbe:  i.disk.busProbe,
	})
	if err != nil {
		return nil, fmt.Errorf("inspecting block devices: %w", err)
	}
	inv := &diskInventory{snapshot: snapshot, rootDisk: rootDisk(snapshot)}
	for idx := range snapshot.Devices {
		dev := &snapshot.Devices[idx]
		if dev.Type == blockTypeDisk || dev.Type == blockTypeMpath {
			inv.disks = append(inv.disks, i.describeDevice(snapshot, dev))
		}
	}
	sort.Slice(inv.disks, func(a, b int) bool { return inv.disks[a].path < inv.disks[b].path })
	return inv, nil
}

// byIDPaths enumerates /dev/disk/by-id through the installer's filesystem for
// the collector.
func (i *installer) byIDPaths() (map[string][]string, error) {
	entries, err := i.fs.ReadDir(diskByIDDir)
	if err != nil {
		return nil, err
	}
	paths := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		alias := filepath.Join(diskByIDDir, entry.Name())
		resolved, _, err := resolveLinks(i.fs, alias)
		if err != nil {
			continue
		}
		paths[resolved] = append(paths[resolved], alias)
	}
	for path := range paths {
		sort.Strings(paths[path])
	}
	return paths, nil
}

func (i *installer) describeDevice(snapshot *register.BlockSnapshot, dev *register.BlockDevice) *diskFacts {
	d := &diskFacts{dev: dev, path: dev.KernelPath, kname: strings.TrimPrefix(dev.KernelPath, devDir)}
	for _, child := range dev.Children {
		if part := snapshot.Find(child); part != nil && part.Type == blockTypePart {
			d.partitions = append(d.partitions, part)
		}
	}
	d.virtual = virtualDevice(d.kname)
	d.liveMedia = isLiveMedia(snapshot, dev)
	d.layout = classifyLayout(dev, d.partitions)
	d.data = dataFindings(dev, d.partitions, d.layout)
	sys := i.readBlockSysfs(d.kname)
	d.majMin, d.sizeBytes = sys.majMin, sys.sizeBytes
	if containsString(sys.unreadable, "size") {
		d.sizeBytes = dev.SizeBytes
	}
	d.problems = i.targetProblems(snapshot, d, sys)
	return d
}

// virtualDevice reports device classes automatic selection never considers.
func virtualDevice(kname string) bool {
	for _, prefix := range []string{"zram", "ram", "loop", "nbd"} {
		if strings.HasPrefix(kname, prefix) {
			return true
		}
	}
	return false
}

// isLiveMedia reports whether dev, or anything on it, is the live medium:
// labelled COS_LIVE or mounted on /run/initramfs/live.
func isLiveMedia(snapshot *register.BlockSnapshot, dev *register.BlockDevice) bool {
	for _, d := range withDescendants(snapshot, dev) {
		for _, label := range deviceLabels(d) {
			if strings.EqualFold(label, liveMediaLabel) {
				return true
			}
		}
		if containsString(d.MountPoints, liveMediaMount) {
			return true
		}
	}
	return false
}

// withDescendants returns dev followed by everything stacked on it.
func withDescendants(snapshot *register.BlockSnapshot, dev *register.BlockDevice) []*register.BlockDevice {
	seen := map[string]bool{dev.Path: true}
	devices := []*register.BlockDevice{dev}
	for idx := 0; idx < len(devices); idx++ {
		for _, child := range devices[idx].Children {
			if seen[child] {
				continue
			}
			seen[child] = true
			if device := snapshot.Find(child); device != nil {
				devices = append(devices, device)
			}
		}
	}
	return devices
}

// rootDisk returns the kernel path of the disk / is mounted from, or "" when
// / is not on a disk.
func rootDisk(snapshot *register.BlockSnapshot) string {
	for idx := range snapshot.Devices {
		dev := &snapshot.Devices[idx]
		if !containsString(dev.MountPoints, "/") {
			continue
		}
		for _, top := range topAncestors(snapshot, dev) {
			if (top.Type == blockTypeDisk || top.Type == blockTypeMpath) && !virtualDevice(strings.TrimPrefix(top.KernelPath, devDir)) {
				return top.KernelPath
			}
		}
	}
	return ""
}

func topAncestors(snapshot *register.BlockSnapshot, dev *register.BlockDevice) []*register.BlockDevice {
	seen := map[string]bool{dev.Path: true}
	queue := []*register.BlockDevice{dev}
	var tops []*register.BlockDevice
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if len(current.Parents) == 0 {
			tops = append(tops, current)
			continue
		}
		for _, parent := range current.Parents {
			if seen[parent] {
				continue
			}
			seen[parent] = true
			if device := snapshot.Find(parent); device != nil {
				queue = append(queue, device)
			}
		}
	}
	return tops
}

// blockSysfs are the attributes of /sys/class/block/<kname> the gates read.
type blockSysfs struct {
	majMin    string
	partition bool
	readOnly  bool
	removable bool
	sizeBytes int64
	// scsiType is device/type; 5 is an optical drive.
	scsiType string
	holders  []string
	// unreadable lists required attributes that could not be read.
	unreadable []string
}

func (i *installer) readBlockSysfs(kname string) blockSysfs {
	dir := filepath.Join(sysClassBlock, kname)
	read := func(attr string) (string, bool) {
		data, err := i.fs.ReadFile(filepath.Join(dir, attr))
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(data)), true
	}
	var s blockSysfs
	if _, s.partition = read("partition"); s.partition {
		return s
	}
	var ok bool
	if s.majMin, ok = read("dev"); !ok {
		s.unreadable = append(s.unreadable, "dev")
	}
	if value, ok := read("ro"); ok {
		s.readOnly = value != "0"
	} else {
		s.unreadable = append(s.unreadable, "ro")
	}
	if value, ok := read("removable"); ok {
		s.removable = value != "0"
	} else {
		s.unreadable = append(s.unreadable, "removable")
	}
	if value, ok := read("size"); ok {
		sectors, err := strconv.ParseInt(value, 10, 64)
		if err != nil || sectors < 0 || sectors > (1<<63-1)/512 {
			s.unreadable = append(s.unreadable, "size")
		} else {
			s.sizeBytes = sectors * 512
		}
	} else {
		s.unreadable = append(s.unreadable, "size")
	}
	s.scsiType, _ = read("device/type")
	holders, err := i.holders(kname)
	if err != nil {
		s.unreadable = append(s.unreadable, "holders")
	}
	s.holders = holders
	return s
}

func (i *installer) holders(kname string) ([]string, error) {
	entries, err := i.fs.ReadDir(filepath.Join(sysClassBlock, kname, "holders"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// targetProblems applies the safety gates: a whole disk, writable, fixed,
// not an optical drive, not the live medium, with nothing mounted, swapped
// on or stacked on it or its partitions, and not empty.
func (i *installer) targetProblems(snapshot *register.BlockSnapshot, d *diskFacts, sys blockSysfs) []string {
	if sys.partition {
		parent := strings.Join(d.dev.Parents, ", ")
		return []string{fmt.Sprintf("%s is a partition of %s, not a whole disk", d.path, parent)}
	}
	var problems []string
	if len(sys.unreadable) > 0 {
		problems = append(problems, fmt.Sprintf("cannot read %s of %s", strings.Join(sys.unreadable, ", "), filepath.Join(sysClassBlock, d.kname)))
	}
	if d.dev.Type == blockTypeRom || sys.scsiType == "5" || strings.HasPrefix(d.kname, "sr") {
		problems = append(problems, "optical drive")
	}
	if sys.readOnly || d.dev.ReadOnly {
		problems = append(problems, "read-only")
	}
	if sys.removable || d.dev.Removable {
		problems = append(problems, "removable")
	}
	if d.sizeBytes <= 0 {
		problems = append(problems, "size is 0")
	}
	if d.liveMedia {
		problems = append(problems, "live media")
	}
	if d.dev.TopologyAmbiguous {
		problems = append(problems, "lsblk reports conflicting facts for it")
	}
	problems = append(problems, mountProblems(snapshot, d.dev)...)
	return append(problems, i.holderProblems(snapshot, d, sys.holders)...)
}

func mountProblems(snapshot *register.BlockSnapshot, dev *register.BlockDevice) []string {
	var problems []string
	for _, d := range withDescendants(snapshot, dev) {
		for _, mount := range d.MountPoints {
			if strings.EqualFold(mount, "[SWAP]") {
				problems = append(problems, d.Path+" is in use as swap")
			} else {
				problems = append(problems, d.Path+" is mounted on "+mount)
			}
		}
	}
	return problems
}

// holderProblems reports anything stacked on the device or its partitions:
// device-mapper (LVM, crypt, multipath) and md, as sysfs holders and as lsblk
// children that are not partitions.
func (i *installer) holderProblems(snapshot *register.BlockSnapshot, d *diskFacts, diskHolders []string) []string {
	var problems []string
	for _, holder := range diskHolders {
		problems = append(problems, fmt.Sprintf("%s is held by %s", d.path, holder))
	}
	for _, part := range d.partitions {
		holders, err := i.holders(strings.TrimPrefix(part.KernelPath, devDir))
		if err != nil {
			problems = append(problems, fmt.Sprintf("cannot read the holders of %s", part.KernelPath))
		}
		for _, holder := range holders {
			problems = append(problems, fmt.Sprintf("%s is held by %s", part.KernelPath, holder))
		}
	}
	for _, dev := range append([]*register.BlockDevice{d.dev}, d.partitions...) {
		for _, child := range dev.Children {
			if user := snapshot.Find(child); user != nil && user.Type != blockTypePart {
				problems = append(problems, fmt.Sprintf("%s is used by %s (%s)", dev.Path, user.Path, user.Type))
			}
		}
	}
	return problems
}

// installRequest is what the configuration asks for, kept so the target can
// be resolved again right before the backend writes it.
type installRequest struct {
	device   string
	selector elementalv1.DeviceSelector
	target   *elementalv1.InstallTarget
}

func newInstallRequest(config elementalv1.Config) installRequest {
	return installRequest{
		device:   strings.TrimSpace(config.Elemental.Install.Device),
		selector: config.Elemental.Install.DeviceSelector,
		target:   config.Elemental.InstallTarget,
	}
}

func (r installRequest) byID() bool {
	return r.target != nil && strings.TrimSpace(r.target.DeviceID) != ""
}

// wipeIDs is the operator-approved wipe list; nil when there is none.
func (r installRequest) wipeIDs() *[]string {
	if r.target == nil {
		return nil
	}
	return r.target.WipeDeviceIDs
}

func (r installRequest) warnConflicts() {
	switch {
	case r.byID() && (r.device != "" || len(r.selector) > 0):
		log.Warningf("install-target device-id %s takes precedence over install.device and install.device-selector", r.target.DeviceID)
	case r.device != "" && len(r.selector) > 0:
		log.Warningf("Both device and device-selector set, using device-field '%s'", r.device)
	}
}

// installTarget is a resolved install target.
type installTarget struct {
	// requested is what the configuration asked for, for the audit log.
	requested string
	// chain is the symlink chain from the requested path to the kernel path.
	chain []string
	disk  *diskFacts
}

// resolveInstallTarget picks the target by priority: the disk ID the
// operator released, then install.device, then install.device-selector, and
// with nothing configured the only disk automatic selection may use. The
// target must pass every safety gate.
func (i *installer) resolveInstallTarget(inv *diskInventory, req installRequest) (*installTarget, error) {
	var target *installTarget
	var err error
	switch {
	case req.byID():
		target, err = targetByID(inv, req.target)
	case req.device != "":
		target, err = i.targetByPath(inv, req.device)
	default:
		target, err = findInstallationDevice(inv, req.selector)
	}
	if err != nil {
		return nil, err
	}
	if len(target.disk.problems) > 0 {
		return nil, diskErrorf(reasonTargetIneligible, "%s cannot be the install target: %s", target.disk.describe(), strings.Join(target.disk.problems, "; "))
	}
	return target, nil
}

func targetByID(inv *diskInventory, want *elementalv1.InstallTarget) (*installTarget, error) {
	id := register.NormalizeDeviceID(want.DeviceID)
	var matches []*diskFacts
	for _, d := range inv.disks {
		if d.dev.Type == blockTypeDisk && d.dev.ID != "" && d.dev.ID == id {
			matches = append(matches, d)
		}
	}
	if len(matches) == 0 {
		return nil, diskErrorf(reasonTargetNotObserved, "no disk has ID %s; disks: %s", id, inv.describeDisks())
	}
	if len(matches) > 1 {
		return nil, diskErrorf(reasonTargetAmbiguous, "%d disks have ID %s; disks: %s", len(matches), id, inv.describeDisks())
	}
	d := matches[0]
	if mismatch := expectMismatch(d, want.Expect); mismatch != "" {
		return nil, diskErrorf(reasonTargetMismatch, "disk %s (%s) no longer looks as the operator saw it: %s", id, d.path, mismatch)
	}
	return &installTarget{requested: "install-target " + id, chain: []string{d.path}, disk: d}, nil
}

func expectMismatch(d *diskFacts, expect elementalv1.InstallTargetExpect) string {
	var problems []string
	if expect.SizeBytes > 0 && expect.SizeBytes != d.sizeBytes {
		problems = append(problems, fmt.Sprintf("size is %d, expected %d", d.sizeBytes, expect.SizeBytes))
	}
	if wwn := strings.TrimSpace(expect.WWN); wwn != "" && !sameWWN(wwn, d.dev.WWN) {
		problems = append(problems, fmt.Sprintf("WWN is %q, expected %q", d.dev.WWN, wwn))
	}
	if serial := strings.TrimSpace(expect.Serial); serial != "" && serial != d.dev.Serial {
		problems = append(problems, fmt.Sprintf("serial is %q, expected %q", d.dev.Serial, serial))
	}
	if model := strings.TrimSpace(expect.Model); model != "" && !strings.EqualFold(model, d.dev.Model) {
		problems = append(problems, fmt.Sprintf("model is %q, expected %q", d.dev.Model, model))
	}
	return strings.Join(problems, "; ")
}

func sameWWN(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	return register.NormalizeDeviceID("wwn:"+a) == register.NormalizeDeviceID("wwn:"+b)
}

// targetByPath resolves install.device. Any device a person names explicitly
// may carry data; it is logged and overwritten.
func (i *installer) targetByPath(inv *diskInventory, device string) (*installTarget, error) {
	resolved, chain, err := resolveLinks(i.fs, device)
	if err != nil {
		return nil, diskErrorf(reasonTargetNotObserved, "install.device %s: %v", strings.Join(chain, " -> "), err)
	}
	node, err := i.statBlockNode(resolved)
	if err != nil {
		return nil, diskErrorf(reasonTargetIneligible, "install.device %s: %v", strings.Join(chain, " -> "), err)
	}
	kname, err := i.kernelName(node)
	if err != nil {
		return nil, diskErrorf(reasonTargetNotObserved, "install.device %s: %v", strings.Join(chain, " -> "), err)
	}
	kernelPath := devDir + kname
	if chain[len(chain)-1] != kernelPath {
		chain = append(chain, kernelPath)
	}
	d := inv.disk(kernelPath)
	if d == nil {
		dev := inv.snapshot.Find(kernelPath)
		if dev == nil {
			return nil, diskErrorf(reasonTargetNotObserved, "install.device %s: lsblk does not report %s", strings.Join(chain, " -> "), kernelPath)
		}
		d = i.describeDevice(inv.snapshot, dev)
	}
	if majMin := node.String(); d.majMin != "" && d.majMin != majMin {
		return nil, diskErrorf(reasonTargetMismatch, "install.device %s: the device node is %s but sysfs says %s is %s", strings.Join(chain, " -> "), majMin, kname, d.majMin)
	}
	if len(d.data) > 0 && len(d.problems) == 0 {
		log.Warningf("install target %s carries %s; it was named explicitly and will be overwritten", d.path, strings.Join(d.data, ", "))
	}
	return &installTarget{requested: device, chain: chain, disk: d}, nil
}

func (i *installer) statBlockNode(path string) (blockNode, error) {
	if i.disk.statNode != nil {
		return i.disk.statNode(path)
	}
	return statBlockNode(i.fs, path)
}

// kernelName maps a device number to its kernel name through
// /sys/dev/block/<major>:<minor>, which links to the device's sysfs directory.
func (i *installer) kernelName(node blockNode) (string, error) {
	link := filepath.Join(sysDevBlock, node.String())
	target, err := i.fs.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("no block device %s in sysfs: %w", node, err)
	}
	name := filepath.Base(target)
	if name == "" || name == "." || name == "/" || name == ".." {
		return "", fmt.Errorf("%s links to %q, not to a device", link, target)
	}
	return name, nil
}

// resolveLinks follows symlinks the way filepath.EvalSymlinks does — every
// path component, relative targets against the link's directory, a bounded
// number of hops — but through fs. chain starts with the cleaned input and
// gains one entry per link followed; on success it ends with the resolved
// path.
func resolveLinks(fs vfs.FS, name string) (string, []string, error) {
	if !filepath.IsAbs(name) {
		return "", []string{name}, fmt.Errorf("%q is not an absolute path", name)
	}
	start := filepath.Clean(name)
	chain := []string{start}
	resolved := "/"
	pending := splitPath(start)
	hops := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, component)
		info, err := fs.Lstat(next)
		if err != nil {
			if os.IsNotExist(err) {
				return "", chain, fmt.Errorf("%s does not exist", next)
			}
			return "", chain, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if hops++; hops > maxLinkHops {
			return "", chain, fmt.Errorf("more than %d symbolic links", maxLinkHops)
		}
		target, err := fs.Readlink(next)
		if err != nil {
			return "", chain, fmt.Errorf("reading link %s: %w", next, err)
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		pending = append(splitPath(target), pending...)
		chain = append(chain, filepath.Join(append([]string{resolved}, pending...)...))
	}
	return resolved, chain, nil
}

func splitPath(path string) []string {
	return strings.Split(filepath.ToSlash(path), "/")
}

// automaticExclusion is why a device-selector or the single-disk fallback
// skips a disk, or "" when it may pick it.
func automaticExclusion(d *diskFacts) string {
	switch {
	case d.dev.Type != blockTypeDisk:
		return "type " + d.dev.Type
	case d.virtual:
		return "virtual device"
	case len(d.problems) > 0:
		return strings.Join(d.problems, "; ")
	case len(d.data) > 0:
		return "has data: " + strings.Join(d.data, ", ")
	}
	return ""
}

// findInstallationDevice picks the one disk the selector matches among the
// disks automatic selection may use. An empty selector matches every such
// disk, so the host must then have exactly one. Zero or several matches are
// an error that lists every disk.
func findInstallationDevice(inv *diskInventory, selector elementalv1.DeviceSelector) (*installTarget, error) {
	if err := validateDeviceSelector(selector); err != nil {
		return nil, err
	}
	requested := describeSelector(selector)
	var matched []*diskFacts
	for _, d := range inv.disks {
		if reason := automaticExclusion(d); reason != "" {
			log.Debugf("%s is not an install candidate: %s", d.path, reason)
			continue
		}
		if matchesSelector(d, selector) {
			matched = append(matched, d)
		}
	}
	switch len(matched) {
	case 1:
		return &installTarget{requested: requested, chain: []string{matched[0].path}, disk: matched[0]}, nil
	case 0:
		return nil, diskErrorf(reasonTargetNotObserved, "%s matches no eligible disk; disks: %s", requested, inv.describeDisks())
	default:
		paths := make([]string, 0, len(matched))
		for _, d := range matched {
			paths = append(paths, d.path)
		}
		return nil, diskErrorf(reasonTargetAmbiguous, "%s matches %d disks (%s) but must match exactly one; disks: %s",
			requested, len(matched), strings.Join(paths, ", "), inv.describeDisks())
	}
}

func describeSelector(selector elementalv1.DeviceSelector) string {
	if len(selector) == 0 {
		return "the only eligible disk (no device, device-selector or install-target configured)"
	}
	parts := make([]string, 0, len(selector))
	for _, req := range selector {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", req.Key, req.Operator, strings.Join(req.Values, ", ")))
	}
	return "device-selector [" + strings.Join(parts, ", ") + "]"
}

func validateDeviceSelector(selector elementalv1.DeviceSelector) error {
	for idx, req := range selector {
		if err := validateRequirement(req); err != nil {
			return fmt.Errorf("device-selector[%d] %s %s: %w", idx, req.Key, req.Operator, err)
		}
	}
	return nil
}

func validateRequirement(req elementalv1.DeviceSelectorRequirement) error {
	switch req.Key {
	case elementalv1.DeviceSelectorKeySize:
		if req.Operator != elementalv1.DeviceSelectorOpGt && req.Operator != elementalv1.DeviceSelectorOpLt {
			return errors.New("the key takes Gt or Lt")
		}
		if len(req.Values) != 1 {
			return errors.New("the key takes exactly one quantity")
		}
		if _, err := resource.ParseQuantity(req.Values[0]); err != nil {
			return fmt.Errorf("failed to parse quantity %s", req.Values[0])
		}
	case elementalv1.DeviceSelectorKeyRotational:
		if req.Operator != elementalv1.DeviceSelectorOpIn {
			return errors.New("the key takes In")
		}
		if len(req.Values) == 0 {
			return errors.New(`the key takes "true" or "false"`)
		}
		for _, value := range req.Values {
			if _, err := strconv.ParseBool(strings.TrimSpace(value)); err != nil {
				return fmt.Errorf(`the key takes "true" or "false", not %q`, value)
			}
		}
	case elementalv1.DeviceSelectorKeyName, elementalv1.DeviceSelectorKeyID, elementalv1.DeviceSelectorKeyWWN,
		elementalv1.DeviceSelectorKeySerial, elementalv1.DeviceSelectorKeyModel, elementalv1.DeviceSelectorKeyTransport:
		if req.Operator != elementalv1.DeviceSelectorOpIn && req.Operator != elementalv1.DeviceSelectorOpNotIn {
			return errors.New("the key takes In or NotIn")
		}
		if len(req.Values) == 0 {
			return errors.New("the key needs at least one value")
		}
	default:
		return errors.New("unknown key")
	}
	return nil
}

// matchesSelector applies a validated selector.
func matchesSelector(d *diskFacts, selector elementalv1.DeviceSelector) bool {
	for _, req := range selector {
		if !matchesRequirement(d, req) {
			log.Debugf("%s does not match selector %s", d.path, req.Key)
			return false
		}
	}
	return true
}

func matchesRequirement(d *diskFacts, req elementalv1.DeviceSelectorRequirement) bool {
	switch req.Key {
	case elementalv1.DeviceSelectorKeySize:
		limit := resource.MustParse(req.Values[0])
		size := resource.NewQuantity(d.sizeBytes, resource.BinarySI)
		if req.Operator == elementalv1.DeviceSelectorOpLt {
			return size.Cmp(limit) < 0
		}
		return size.Cmp(limit) > 0
	case elementalv1.DeviceSelectorKeyRotational:
		for _, value := range req.Values {
			if want, _ := strconv.ParseBool(strings.TrimSpace(value)); want == d.dev.Rotational {
				return true
			}
		}
		return false
	}
	in := false
	for _, value := range req.Values {
		if matchesValue(d, req.Key, value) {
			in = true
			break
		}
	}
	return in == (req.Operator == elementalv1.DeviceSelectorOpIn)
}

func matchesValue(d *diskFacts, key elementalv1.DeviceSelectorKey, value string) bool {
	value = strings.TrimSpace(value)
	switch key {
	case elementalv1.DeviceSelectorKeyName:
		return value == d.kname || value == d.path
	case elementalv1.DeviceSelectorKeyID:
		return d.dev.ID != "" && register.NormalizeDeviceID(value) == d.dev.ID
	case elementalv1.DeviceSelectorKeyWWN:
		return sameWWN(value, d.dev.WWN)
	case elementalv1.DeviceSelectorKeySerial:
		return d.dev.Serial != "" && value == d.dev.Serial
	case elementalv1.DeviceSelectorKeyModel:
		return d.dev.Model != "" && strings.EqualFold(value, d.dev.Model)
	case elementalv1.DeviceSelectorKeyTransport:
		return d.dev.Transport != "" && strings.EqualFold(value, d.dev.Transport)
	}
	return false
}

// kbimgTargetProblem rejects disks kbimg cannot partition. kbimg names
// partitions by appending a number to the disk path, or "p" and a number when
// the path contains nvme or mmcblk, so a kernel name that already ends in a
// digit (md126, dm-0, loop0, nbd0, pmem0) yields nodes that do not exist.
func kbimgTargetProblem(kname string) string {
	if kname == "" {
		return "kbimg needs a kernel device name"
	}
	if last := kname[len(kname)-1]; last < '0' || last > '9' {
		return ""
	}
	if strings.HasPrefix(kname, "nvme") || strings.HasPrefix(kname, "mmcblk") {
		return ""
	}
	return fmt.Sprintf("kbimg cannot partition %s%s: it would look for partitions named %s%s1, which the kernel does not create", devDir, kname, devDir, kname)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// diskPlan is the outcome of the pre-install step before anything is written.
type diskPlan struct {
	request installRequest
	target  *installTarget
	survey  *diskSurvey
	// wipeTarget is set when the target is wiped along with the stale disks.
	wipeTarget bool
}

// prepareInstallDisks resolves the install target, applies the gates and
// decides which disks get wiped, without writing anything. On success
// config.Elemental.Install.Device is the target's kernel path.
func (i *installer) prepareInstallDisks(config *elementalv1.Config, backend string) (*diskPlan, error) {
	request := newInstallRequest(*config)
	request.warnConflicts()
	inv, err := i.collectDiskInventory()
	if err != nil {
		return nil, err
	}
	target, err := i.resolveInstallTarget(inv, request)
	if err != nil {
		return nil, err
	}
	if backend == KubeOSInstaller {
		if problem := kbimgTargetProblem(target.disk.kname); problem != "" {
			return nil, diskErrorf(reasonTargetIneligible, "%s", problem)
		}
	}
	survey := i.inspectInstallDisks(inv, target.disk.path, request.wipeIDs())
	logDiskSurvey(survey)
	plan := &diskPlan{request: request, target: target, survey: survey}
	if survey.Cleanup {
		if blocked := survey.Stale.withAction(staleDiskBlocked); len(blocked) > 0 {
			lines := make([]string, 0, len(blocked))
			for _, disk := range blocked {
				lines = append(lines, fmt.Sprintf("%s (%s): %s", disk.ID, disk.Path, disk.Reason))
			}
			return nil, &installDiskError{
				Reason:  reasonBlocked,
				Message: "stale system disks cannot be wiped safely; clean them by hand or remove them, then restart the install: " + strings.Join(lines, "; "),
				Stale:   survey.Stale,
			}
		}
		plan.wipeTarget = !config.Elemental.Install.NoFormat
	}
	config.Elemental.Install.Device = target.disk.path
	log.Infof("install target: requested=%s chain=%s id=%s size=%d", target.requested, strings.Join(target.chain, "->"), target.disk.displayID(), target.disk.sizeBytes)
	return plan, nil
}

// finalizeInstallDisks runs right before the backend writes the target. In a
// live environment it wipes the stale system disks and the target and checks
// the wipe took; in any case it resolves the target again and refuses to go
// on if it is no longer the same disk.
func (i *installer) finalizeInstallDisks(plan *diskPlan) error {
	wiped, err := i.wipeInstallDisks(plan)
	if err != nil {
		return err
	}
	inv, err := i.collectDiskInventory()
	if err != nil {
		if len(wiped) > 0 {
			return &installDiskError{Reason: reasonWipeFailed, Message: err.Error(), Stale: plan.survey.Stale}
		}
		return err
	}
	if len(wiped) > 0 {
		if err := verifyWipe(inv, plan.target.disk.path, wiped); err != nil {
			err.Stale = plan.survey.Stale
			return err
		}
	}
	again, err := i.resolveInstallTarget(inv, plan.request)
	if err != nil {
		return diskErrorf(reasonTargetMismatch, "resolving %s again before the install: %v", plan.target.requested, err)
	}
	before, after := plan.target.disk, again.disk
	if before.path != after.path || before.majMin != after.majMin || before.dev.ID != after.dev.ID {
		return diskErrorf(reasonTargetMismatch, "%s resolved to %s (%s, %s) and now to %s (%s, %s); not installing",
			plan.target.requested, before.path, before.majMin, before.displayID(), after.path, after.majMin, after.displayID())
	}
	return nil
}
