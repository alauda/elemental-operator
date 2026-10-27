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
	"fmt"
	"os"
	"syscall"

	"github.com/twpayne/go-vfs"
)

// statBlockNode returns the device number of the block special file at path.
func statBlockNode(fs vfs.FS, path string) (blockNode, error) {
	info, err := fs.Stat(path)
	if err != nil {
		return blockNode{}, err
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return blockNode{}, fmt.Errorf("%s is not a block device (mode %s)", path, info.Mode())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return blockNode{}, fmt.Errorf("%s: the device number is not available", path)
	}
	// The glibc encoding of dev_t: 12 bits of major and 20 of minor, split.
	rdev := uint64(stat.Rdev)
	return blockNode{
		major: uint32(((rdev >> 8) & 0xfff) | ((rdev >> 32) & 0xfffff000)),
		minor: uint32((rdev & 0xff) | ((rdev >> 12) & 0xffffff00)),
	}, nil
}
