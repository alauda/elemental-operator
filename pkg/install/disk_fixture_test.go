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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/gomega"
	"github.com/twpayne/go-vfs/vfst"
)

// fakeHost models a host's block devices the way lsblk, wipefs, sysfs and
// /dev show them. It answers the inspection commands, applies wipes to its
// own state so a second inspection sees the result, and records every
// command it is asked to run.
type fakeHost struct {
	disks []*fakeDisk
	// live adds the marker of a live environment.
	live bool
	// stuck lists kernel paths whose wipes do not take.
	stuck map[string]bool
	// missing lists commands that are not installed.
	missing map[string]bool
	calls   [][]string
}

type fakeDisk struct {
	name         string // kernel name: sda, nvme0n1, sr0, md126
	path         string // lsblk PATH when it is not /dev/<name>
	major, minor uint32
	typ          string // lsblk TYPE, default disk
	size         int64
	ro, rm, rota bool
	model        string
	serial       string
	wwn          string
	tran         string
	byID         []string // /dev/disk/by-id aliases
	pttype       string
	fstype       string
	label        string
	mounts       []string
	holders      []string
	scsiType     string
	partitions   []*fakePart
	// users are lsblk children other than partitions: lvm, mpath, raid.
	users []*fakeDisk
	// wipefsFails makes wipefs --no-act fail on the disk.
	wipefsFails bool
}

type fakePart struct {
	name      string // sda1
	minor     uint32
	size      int64
	fstype    string
	label     string
	partlabel string
	parttype  string
	mounts    []string
	holders   []string
	users     []*fakeDisk
}

const gib = int64(1) << 30

func (d *fakeDisk) devPath() string {
	if d.path != "" {
		return d.path
	}
	return devDir + d.name
}

func (d *fakeDisk) kind() string {
	if d.typ != "" {
		return d.typ
	}
	return blockTypeDisk
}

// sataDisk is a fixed SATA disk with a WWN and its udev by-id alias.
func sataDisk(name string, minor uint32, sizeGiB int64, wwn string) *fakeDisk {
	return &fakeDisk{
		name: name, major: 8, minor: minor, size: sizeGiB * gib,
		model: "Disk " + strings.ToUpper(name), serial: "SER-" + strings.ToUpper(name),
		wwn: "0x" + wwn, tran: "sata", byID: []string{"wwn-0x" + wwn},
	}
}

// part adds a partition; minor numbers follow the disk's.
func (d *fakeDisk) part(fstype, label, partlabel string) *fakeDisk {
	n := len(d.partitions) + 1
	name := fmt.Sprintf("%s%d", d.name, n)
	if strings.HasPrefix(d.name, "nvme") {
		name = fmt.Sprintf("%sp%d", d.name, n)
	}
	d.pttype = "gpt"
	d.partitions = append(d.partitions, &fakePart{
		name: name, minor: d.minor + uint32(n), size: 1 * gib,
		fstype: fstype, label: label, partlabel: partlabel,
	})
	return d
}

// elementalLayout gives the disk the partitions elemental-toolkit creates.
func (d *fakeDisk) elementalLayout() *fakeDisk {
	return d.part("vfat", "COS_GRUB", "efi").
		part("ext4", "COS_OEM", "oem").
		part("ext4", "COS_RECOVERY", "recovery").
		part("ext4", "COS_STATE", "state").
		part("ext4", "COS_PERSISTENT", "persistent")
}

// kubeosLayout gives the disk the partitions kbimg creates.
func (d *fakeDisk) kubeosLayout() *fakeDisk {
	return d.part("vfat", "BOOT", "primary").
		part("ext4", "ROOT-A", "primary").
		part("ext4", "ROOT-B", "primary").
		part("ext4", "PERSIST", "primary")
}

func (h *fakeHost) disk(name string) *fakeDisk {
	for _, d := range h.disks {
		if d.name == name {
			return d
		}
	}
	panic("fake host has no disk " + name)
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func mountpoints(mounts []string) []any {
	if len(mounts) == 0 {
		return []any{nil}
	}
	out := make([]any, 0, len(mounts))
	for _, mount := range mounts {
		out = append(out, mount)
	}
	return out
}

func (d *fakeDisk) lsblk() map[string]any {
	entry := map[string]any{
		"name": d.devPath(), "kname": devDir + d.name, "path": d.devPath(), "type": d.kind(),
		"size": d.size, "log-sec": 512, "ro": d.ro, "rota": d.rota, "rm": d.rm,
		"model": nullable(d.model), "serial": nullable(d.serial), "wwn": nullable(d.wwn), "tran": nullable(d.tran),
		"fstype": nullable(d.fstype), "uuid": nil, "label": nullable(d.label), "mountpoints": mountpoints(d.mounts),
		"pttype": nullable(d.pttype), "parttype": nil, "partuuid": nil, "partlabel": nil, "pkname": nil,
	}
	children := []any{}
	for _, p := range d.partitions {
		child := map[string]any{
			"name": devDir + p.name, "kname": devDir + p.name, "path": devDir + p.name, "type": blockTypePart,
			"size": p.size, "log-sec": 512, "ro": false, "rota": d.rota, "rm": d.rm,
			"fstype": nullable(p.fstype), "uuid": nil, "label": nullable(p.label), "mountpoints": mountpoints(p.mounts),
			"pttype": nullable(d.pttype), "parttype": nullable(p.parttype), "partuuid": fmt.Sprintf("0000-%s", p.name),
			"partlabel": nullable(p.partlabel), "pkname": d.devPath(),
		}
		var users []any
		for _, user := range p.users {
			users = append(users, user.lsblk())
		}
		if len(users) > 0 {
			child["children"] = users
		}
		children = append(children, child)
	}
	for _, user := range d.users {
		children = append(children, user.lsblk())
	}
	if len(children) > 0 {
		entry["children"] = children
	}
	return entry
}

func (h *fakeHost) lsblkJSON() []byte {
	devices := []any{}
	for _, d := range h.disks {
		devices = append(devices, d.lsblk())
	}
	data, err := json.Marshal(map[string]any{"blockdevices": devices})
	Expect(err).NotTo(HaveOccurred())
	return data
}

func signatureJSON(sigType, label string) map[string]any {
	signature := map[string]any{"type": sigType, "offset": "0x0"}
	if label != "" {
		signature["label"] = label
	}
	return signature
}

func (h *fakeHost) wipefsJSON(path string) ([]byte, error) {
	var signatures []any
	for _, d := range h.disks {
		if d.devPath() == path || devDir+d.name == path {
			if d.wipefsFails {
				return []byte("wipefs: error: " + path + ": probing initialization failed"), errors.New("exit status 1")
			}
			switch d.pttype {
			case "gpt":
				signatures = append(signatures, signatureJSON("gpt", ""), signatureJSON("PMBR", ""))
			case "dos":
				signatures = append(signatures, signatureJSON("dos", ""))
			}
			if d.fstype != "" {
				signatures = append(signatures, signatureJSON(d.fstype, d.label))
			}
		}
		for _, p := range d.partitions {
			if devDir+p.name == path && p.fstype != "" {
				signatures = append(signatures, signatureJSON(p.fstype, p.label))
			}
		}
	}
	if len(signatures) == 0 {
		// wipefs prints nothing at all when it finds nothing.
		return nil, nil
	}
	return json.Marshal(map[string]any{"signatures": signatures})
}

// handles reports whether a command belongs to the fake host rather than to
// the KubeOS backend.
func (h *fakeHost) handles(name string, args []string) bool {
	switch name {
	case "wipefs", "sgdisk", "dd", "blockdev", "udevadm":
		return true
	case "lsblk":
		return containsString(args, "--json")
	}
	return false
}

// Output implements commandRunner.
func (h *fakeHost) Output(name string, args ...string) ([]byte, error) {
	h.calls = append(h.calls, append([]string{name}, args...))
	if h.missing[name] {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	last := ""
	if len(args) > 0 {
		last = args[len(args)-1]
	}
	switch {
	case name == "lsblk":
		return h.lsblkJSON(), nil
	case name == "wipefs" && containsString(args, "--no-act"):
		return h.wipefsJSON(last)
	case name == "wipefs":
		h.wipeSignatures(last)
	case name == "sgdisk":
		if d := h.byPath(last); d != nil && !h.stuck[last] {
			d.pttype = ""
		}
	case name == "blockdev":
		if d := h.byPath(last); d != nil && !h.stuck[last] && d.pttype == "" {
			d.partitions = nil
		}
	case name == "dd", name == "udevadm":
	default:
		return nil, fmt.Errorf("fake host: unexpected command %s %s", name, strings.Join(args, " "))
	}
	return nil, nil
}

func (h *fakeHost) byPath(path string) *fakeDisk {
	for _, d := range h.disks {
		if devDir+d.name == path {
			return d
		}
	}
	return nil
}

func (h *fakeHost) wipeSignatures(path string) {
	for _, d := range h.disks {
		if h.stuck[devDir+d.name] {
			continue
		}
		if devDir+d.name == path {
			d.pttype, d.fstype, d.label = "", "", ""
		}
		for _, p := range d.partitions {
			if devDir+p.name == path {
				p.fstype, p.label = "", ""
			}
		}
	}
}

// statNode stands in for stat(2) on the device nodes the host has.
func (h *fakeHost) statNode(path string) (blockNode, error) {
	for _, d := range h.disks {
		if devDir+d.name == path {
			return blockNode{major: d.major, minor: d.minor}, nil
		}
		for _, p := range d.partitions {
			if devDir+p.name == path {
				return blockNode{major: d.major, minor: p.minor}, nil
			}
		}
		for _, user := range d.users {
			if devDir+user.name == path {
				return blockNode{major: user.major, minor: user.minor}, nil
			}
		}
	}
	return blockNode{}, fmt.Errorf("%s is not a block device", path)
}

func boolFile(value bool) string {
	if value {
		return "1\n"
	}
	return "0\n"
}

// sysfs adds a device's /sys/class/block and /sys/dev/block entries.
func sysfs(files map[string]any, name, sysPath string, major, minor uint32, size int64, holders []string) {
	dir := filepath.Join(sysClassBlock, name)
	files[filepath.Join(dir, "dev")] = fmt.Sprintf("%d:%d\n", major, minor)
	files[filepath.Join(dir, "size")] = fmt.Sprintf("%d\n", size/512)
	if len(holders) == 0 {
		files[filepath.Join(dir, "holders")] = &vfst.Dir{Perm: 0o755}
	}
	for _, holder := range holders {
		files[filepath.Join(dir, "holders", holder)] = ""
	}
	files[filepath.Join(sysDevBlock, fmt.Sprintf("%d:%d", major, minor))] = &vfst.Symlink{Target: "../../devices/" + sysPath}
	files[devDir+name] = ""
}

// files lays the host out as /dev and sysfs entries for vfst.
func (h *fakeHost) files() map[string]any {
	files := map[string]any{}
	if h.live {
		files["/run/cos/live_mode"] = ""
	}
	add := func(d *fakeDisk) {
		sysfs(files, d.name, "virtual/block/"+d.name, d.major, d.minor, d.size, d.holders)
		dir := filepath.Join(sysClassBlock, d.name)
		files[filepath.Join(dir, "ro")] = boolFile(d.ro)
		files[filepath.Join(dir, "removable")] = boolFile(d.rm)
		if d.scsiType != "" {
			files[filepath.Join(dir, "device", "type")] = d.scsiType + "\n"
		}
		for _, alias := range d.byID {
			files[filepath.Join(diskByIDDir, alias)] = &vfst.Symlink{Target: "../../" + d.name}
		}
		for _, p := range d.partitions {
			sysfs(files, p.name, "virtual/block/"+d.name+"/"+p.name, d.major, p.minor, p.size, p.holders)
			files[filepath.Join(sysClassBlock, p.name, "partition")] = strings.TrimPrefix(strings.TrimPrefix(p.name, d.name), "p") + "\n"
		}
	}
	for _, d := range h.disks {
		add(d)
		for _, user := range d.users {
			add(user)
		}
	}
	return files
}

// merge combines vfst roots; later maps win.
func merge(roots ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, root := range roots {
		for path, value := range root {
			out[path] = value
		}
	}
	return out
}

// diskOptions wires an installer to the fake host.
func (h *fakeHost) diskOptions() diskOptions {
	return diskOptions{
		runner:   h,
		inspect:  h.Output,
		statNode: h.statNode,
		busProbe: func(string) (string, string) { return "", "" },
	}
}

// destructive lists the recorded commands that write to a disk.
func destructive(calls [][]string) []string {
	var out []string
	for _, call := range calls {
		switch {
		case call[0] == "lsblk", call[0] == "wipefs" && containsString(call, "--no-act"):
			continue
		case call[0] == "wipefs", call[0] == "sgdisk", call[0] == "dd", call[0] == "blockdev", call[0] == "udevadm":
			out = append(out, strings.Join(call, " "))
		}
	}
	return out
}

// absoluteSymlink creates a link whose target is an absolute path inside the
// test filesystem. vfst would rewrite it to the temporary directory.
func absoluteSymlink(fs *vfst.TestFS, target, link string) {
	real := filepath.Join(fs.TempDir(), link)
	Expect(os.MkdirAll(filepath.Dir(real), 0o755)).To(Succeed())
	Expect(os.Symlink(target, real)).To(Succeed())
}
