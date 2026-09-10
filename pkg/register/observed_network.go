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
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
)

// resolveConfPath is the default resolver config path. Overridable in tests.
var resolveConfPath = "/etc/resolv.conf"

// procNetRoutePath is the default IPv4 route table in proc. Overridable in tests.
var procNetRoutePath = "/proc/net/route"

// sysClassNetPath is where the kernel exposes per-link attributes. Overridable
// in tests.
var sysClassNetPath = "/sys/class/net"

// nmSystemConnectionsPath is NetworkManager's keyfile directory. Overridable in
// tests.
var nmSystemConnectionsPath = "/etc/NetworkManager/system-connections"

const (
	// nmConnectionSuffix is the extension NetworkManager gives its keyfiles.
	nmConnectionSuffix = ".nmconnection"
	// maxConnectionFileBytes bounds a single keyfile. A real one is well under
	// a kilobyte; anything larger is not a keyfile worth carrying.
	maxConnectionFileBytes = 64 * 1024
	// maxConnectionsTotalBytes bounds the whole set, so a host cannot inflate
	// its MachineInventory without limit.
	maxConnectionsTotalBytes = 256 * 1024
)

// secretMarkers are substrings that mark a connection profile as carrying a
// secret. Such profiles are skipped rather than sanitised: a wired baremetal
// host has no business shipping credentials into its MachineInventory, and
// stripping the keys would hand back a profile that no longer works.
var secretMarkers = []string{
	"[wifi-security]",
	"[802-1x]",
	"psk=",
	"password=",
	"password-raw=",
	"preshared-key=",
	"wep-key",
}

// sendObservedNetwork collects a snapshot of the host's current network state
// (interfaces, routes, DNS) and sends it to the operator via the Alauda-fork
// MsgObservedNetworkConfig message. Failures are returned to the caller but
// collection itself is best-effort: missing sub-fields do not abort the send.
func sendObservedNetwork(conn *websocket.Conn) error {
	observed := collectObservedNetwork()
	return SendJSONData(conn, MsgObservedNetworkConfig, observed)
}

func collectObservedNetwork() *elementalv1.ObservedNetwork {
	observed := &elementalv1.ObservedNetwork{}

	if ifaces, err := collectInterfaces(); err != nil {
		log.Warningf("failed to enumerate network interfaces: %v", err)
	} else {
		observed.Interfaces = ifaces
	}

	if routes, err := collectRoutes(procNetRoutePath); err != nil {
		log.Warningf("failed to parse routes from %s: %v", procNetRoutePath, err)
	} else {
		observed.Routes = routes
	}

	if dns, search, err := collectResolvConf(resolveConfPath); err != nil {
		log.Warningf("failed to parse %s: %v", resolveConfPath, err)
	} else {
		observed.DNSServers = dns
		observed.SearchDomains = search
	}

	observed.Connections = collectObservedConnections()

	return observed
}

func collectInterfaces() ([]elementalv1.ObservedInterface, error) {
	netIfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]elementalv1.ObservedInterface, 0, len(netIfaces))
	for _, ni := range netIfaces {
		// Skip loopback; not useful for inventory purposes.
		if ni.Flags&net.FlagLoopback != 0 {
			continue
		}
		oi := elementalv1.ObservedInterface{
			Name:   ni.Name,
			MAC:    ni.HardwareAddr.String(),
			MTU:    ni.MTU,
			Kind:   linkKind(ni.Name),
			Master: linkMaster(ni.Name),
		}
		addrs, err := ni.Addrs()
		if err != nil {
			log.Warningf("failed to read addresses for %s: %v", ni.Name, err)
		} else {
			for _, a := range addrs {
				oi.Addresses = append(oi.Addresses, a.String())
			}
		}
		out = append(out, oi)
	}
	return out, nil
}

// collectRoutes parses /proc/net/route (IPv4). Fields are tab-separated and the
// first line is a header. Destination/Gateway/Mask are little-endian hex.
func collectRoutes(path string) ([]elementalv1.ObservedRoute, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var routes []elementalv1.ObservedRoute
	scanner := bufio.NewScanner(f)
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 11 {
			continue
		}
		dst, err := decodeProcCIDR(fields[1], fields[7])
		if err != nil {
			continue
		}
		gw, _ := decodeProcIP(fields[2])
		metric, _ := strconv.Atoi(fields[6])
		routes = append(routes, elementalv1.ObservedRoute{
			Destination: dst,
			Gateway:     gw,
			Interface:   fields[0],
			Metric:      metric,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return routes, nil
}

// decodeProcIP converts /proc/net/route's little-endian hex-encoded IPv4
// address to dotted-quad. "00000000" yields an empty string.
func decodeProcIP(hexLE string) (string, error) {
	if hexLE == "00000000" {
		return "", nil
	}
	b, err := hex.DecodeString(hexLE)
	if err != nil || len(b) != 4 {
		return "", fmt.Errorf("invalid hex ip %q", hexLE)
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[3], b[2], b[1], b[0]), nil
}

func decodeProcCIDR(dstHex, maskHex string) (string, error) {
	if dstHex == "00000000" && maskHex == "00000000" {
		return "default", nil
	}
	ip, err := decodeProcIP(dstHex)
	if err != nil {
		return "", err
	}
	mb, err := hex.DecodeString(maskHex)
	if err != nil || len(mb) != 4 {
		return "", fmt.Errorf("invalid hex mask %q", maskHex)
	}
	mask := net.IPv4Mask(mb[3], mb[2], mb[1], mb[0])
	ones, _ := mask.Size()
	return fmt.Sprintf("%s/%d", ip, ones), nil
}

// collectResolvConf parses /etc/resolv.conf for nameservers and search domains.
func collectResolvConf(path string) (nameservers []string, search []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "nameserver":
			if len(fields) >= 2 {
				nameservers = append(nameservers, fields[1])
			}
		case "search":
			if len(fields) >= 2 {
				search = append(search, fields[1:]...)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	return nameservers, search, nil
}

// collectObservedConnections reads the host's persisted NetworkManager
// keyfiles. NetworkManager keeps its own auto-default connections in memory,
// with no file on disk, so every file found here was written by an operator —
// which is what makes this an exact record of intent rather than a guess
// reconstructed from addresses.
//
// Collection is best-effort: a missing directory, an unreadable file or a
// profile carrying a secret is skipped with a warning, never fatal.
func collectObservedConnections() map[string]string {
	entries, err := os.ReadDir(nmSystemConnectionsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warningf("failed to read %s: %v", nmSystemConnectionsPath, err)
		}
		return nil
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), nmConnectionSuffix) {
			continue
		}
		names = append(names, entry.Name())
	}
	// Sort so that hitting the total cap truncates the same set every time.
	sort.Strings(names)

	connections := map[string]string{}
	total := 0
	for _, name := range names {
		path := filepath.Join(nmSystemConnectionsPath, name)
		info, err := os.Stat(path)
		if err != nil {
			log.Warningf("failed to stat %s: %v", path, err)
			continue
		}
		if info.Size() > maxConnectionFileBytes {
			log.Warningf("skipping %s: %d bytes exceeds the %d byte limit for a connection profile", path, info.Size(), maxConnectionFileBytes)
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			log.Warningf("failed to read %s: %v", path, err)
			continue
		}
		if marker, found := findSecretMarker(string(content)); found {
			log.Warningf("skipping %s: it carries a secret (%s)", path, marker)
			continue
		}
		if total+len(content) > maxConnectionsTotalBytes {
			log.Warningf("stopping at %s: the collected connection profiles would exceed %d bytes", path, maxConnectionsTotalBytes)
			break
		}
		total += len(content)
		connections[strings.TrimSuffix(name, nmConnectionSuffix)] = string(content)
	}

	if len(connections) == 0 {
		return nil
	}
	return connections
}

func findSecretMarker(content string) (string, bool) {
	lowered := strings.ToLower(content)
	for _, marker := range secretMarkers {
		if strings.Contains(lowered, marker) {
			return marker, true
		}
	}
	return "", false
}

// linkKind returns the kernel's DEVTYPE for a link: empty for a plain physical
// ethernet device, and the link type otherwise (bond, vlan, bridge, ...). A
// link whose uevent cannot be read reports no kind rather than failing
// collection.
func linkKind(name string) string {
	content, err := os.ReadFile(filepath.Join(sysClassNetPath, name, "uevent"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(content), "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), "DEVTYPE="); found {
			return value
		}
	}
	return ""
}

// linkMaster returns the name of the aggregating link this interface is
// enslaved to, or the empty string when it stands on its own.
func linkMaster(name string) string {
	target, err := os.Readlink(filepath.Join(sysClassNetPath, name, "master"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}
