/*
Copyright 2026 The Alauda Authors.

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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withConnectionsDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	previous := nmSystemConnectionsPath
	nmSystemConnectionsPath = dir
	t.Cleanup(func() { nmSystemConnectionsPath = previous })
	return dir
}

func TestCollectObservedConnections(t *testing.T) {
	bond := "[connection]\nid=bond0\ntype=bond\n"
	withConnectionsDir(t, map[string]string{
		"bond0.nmconnection": bond,
		// Not a keyfile: NetworkManager leaves backups and editors leave swap
		// files in this directory, and neither belongs in the inventory.
		"bond0.nmconnection.bak": "ignored",
		"README":                 "ignored",
	})

	got := collectObservedConnections()

	if len(got) != 1 {
		t.Fatalf("collected %d connections, want 1: %v", len(got), got)
	}
	if got["bond0"] != bond {
		t.Errorf("bond0 = %q, want %q", got["bond0"], bond)
	}
}

func TestCollectObservedConnectionsSkipsSecrets(t *testing.T) {
	for name, content := range map[string]string{
		"wifi":    "[connection]\nid=wifi\ntype=wifi\n\n[wifi-security]\npsk=hunter2\n",
		"dot1x":   "[connection]\nid=dot1x\ntype=ethernet\n\n[802-1X]\nidentity=host\n",
		"vpnpass": "[connection]\nid=vpn\n\n[vpn]\npassword=hunter2\n",
	} {
		t.Run(name, func(t *testing.T) {
			withConnectionsDir(t, map[string]string{
				name + ".nmconnection": content,
				"plain.nmconnection":   "[connection]\nid=plain\ntype=ethernet\n",
			})

			got := collectObservedConnections()

			if _, found := got[name]; found {
				t.Errorf("collected %q, which carries a secret", name)
			}
			if _, found := got["plain"]; !found {
				t.Error("a profile without secrets should still be collected")
			}
		})
	}
}

func TestCollectObservedConnectionsBounded(t *testing.T) {
	t.Run("a single oversized file is skipped", func(t *testing.T) {
		withConnectionsDir(t, map[string]string{
			"huge.nmconnection":  strings.Repeat("x", maxConnectionFileBytes+1),
			"small.nmconnection": "[connection]\nid=small\n",
		})

		got := collectObservedConnections()

		if _, found := got["huge"]; found {
			t.Error("an oversized profile should be skipped")
		}
		if _, found := got["small"]; !found {
			t.Error("skipping one file must not abandon the rest")
		}
	})

	t.Run("the total is capped", func(t *testing.T) {
		// Names are collected in sorted order, so the truncation point is
		// stable rather than depending on readdir.
		files := map[string]string{}
		for _, name := range []string{"a", "b", "c", "d", "e"} {
			files[name+".nmconnection"] = strings.Repeat("x", maxConnectionFileBytes)
		}
		withConnectionsDir(t, files)

		got := collectObservedConnections()

		total := 0
		for _, content := range got {
			total += len(content)
		}
		if total > maxConnectionsTotalBytes {
			t.Errorf("collected %d bytes, want at most %d", total, maxConnectionsTotalBytes)
		}
		if _, found := got["a"]; !found {
			t.Error("the first profile in sorted order should survive the cap")
		}
	})
}

func TestCollectObservedConnectionsMissingDirectory(t *testing.T) {
	previous := nmSystemConnectionsPath
	nmSystemConnectionsPath = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { nmSystemConnectionsPath = previous })

	if got := collectObservedConnections(); got != nil {
		t.Errorf("collected %v, want nil when the directory does not exist", got)
	}
}

func TestLinkKindAndMaster(t *testing.T) {
	root := t.TempDir()
	previous := sysClassNetPath
	sysClassNetPath = root
	t.Cleanup(func() { sysClassNetPath = previous })

	mkLink := func(name, uevent string) string {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
		if uevent != "" {
			if err := os.WriteFile(filepath.Join(dir, "uevent"), []byte(uevent), 0o644); err != nil {
				t.Fatalf("writing uevent for %s: %v", name, err)
			}
		}
		return dir
	}

	mkLink("bond0", "INTERFACE=bond0\nIFINDEX=4\nDEVTYPE=bond\n")
	eth0 := mkLink("eth0", "INTERFACE=eth0\nIFINDEX=2\n")
	mkLink("nouevent", "")
	if err := os.Symlink(filepath.Join(root, "bond0"), filepath.Join(eth0, "master")); err != nil {
		t.Fatalf("linking master: %v", err)
	}

	if got := linkKind("bond0"); got != "bond" {
		t.Errorf("linkKind(bond0) = %q, want bond", got)
	}
	if got := linkKind("eth0"); got != "" {
		t.Errorf("linkKind(eth0) = %q, want empty for a plain ethernet link", got)
	}
	if got := linkKind("nouevent"); got != "" {
		t.Errorf("linkKind(nouevent) = %q, want empty", got)
	}
	if got := linkMaster("eth0"); got != "bond0" {
		t.Errorf("linkMaster(eth0) = %q, want bond0", got)
	}
	if got := linkMaster("bond0"); got != "" {
		t.Errorf("linkMaster(bond0) = %q, want empty", got)
	}
}
