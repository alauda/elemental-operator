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

package server

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/network"
	"k8s.io/apimachinery/pkg/runtime"
)

func observedNetworkConfig(inventory *elementalv1.MachineInventory) elementalv1.NetworkConfig {
	if inventory == nil || !isEmptyNetworkConfig(inventory.Spec.Network) {
		return inventory.Spec.Network
	}
	return observedNetworkToNMConnections(inventory.Spec.ObservedNetwork)
}

func isEmptyNetworkConfig(netConf elementalv1.NetworkConfig) bool {
	return (netConf.Configurator == "" || netConf.Configurator == network.ConfiguratorNone) &&
		len(netConf.Config) == 0 &&
		len(netConf.IPAddresses) == 0
}

func observedNetworkToNMConnections(obs *elementalv1.ObservedNetwork) elementalv1.NetworkConfig {
	if obs == nil || len(obs.Interfaces) == 0 {
		return elementalv1.NetworkConfig{}
	}

	keptByName := map[string]elementalv1.ObservedInterface{}
	var keptNames []string
	for _, ifc := range obs.Interfaces {
		if ifc.Name == "" || ifc.MAC == "" || len(ifc.Addresses) == 0 {
			continue
		}
		keptByName[ifc.Name] = ifc
		keptNames = append(keptNames, ifc.Name)
	}
	if len(keptNames) == 0 {
		return elementalv1.NetworkConfig{}
	}
	sort.Strings(keptNames)

	routesByIface := map[string][]elementalv1.ObservedRoute{}
	defaultIface := ""
	for _, route := range obs.Routes {
		if route.Interface == "" {
			continue
		}
		if _, ok := keptByName[route.Interface]; !ok {
			continue
		}
		routesByIface[route.Interface] = append(routesByIface[route.Interface], route)
		if isDefaultRoute(route.Destination) && defaultIface == "" {
			defaultIface = route.Interface
		}
	}

	dnsIface := defaultIface
	if dnsIface == "" {
		dnsIface = keptNames[0]
	}

	connections := map[string]runtime.RawExtension{}
	for _, name := range keptNames {
		content := renderNMConnection(keptByName[name], routesByIface[name], name == dnsIface, obs)
		connections[name] = runtime.RawExtension{Raw: []byte(strconv.Quote(content))}
	}

	return elementalv1.NetworkConfig{
		Configurator: network.ConfiguratorNmconnections,
		Config:       connections,
	}
}

func renderNMConnection(ifc elementalv1.ObservedInterface, routes []elementalv1.ObservedRoute, withDNS bool, obs *elementalv1.ObservedNetwork) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[connection]\nid=%s\ntype=ethernet\ninterface-name=%s\n", ifc.Name, ifc.Name)
	fmt.Fprintf(&b, "\n[ethernet]\nmac-address=%s\n", strings.ToUpper(ifc.MAC))
	if ifc.MTU > 0 {
		fmt.Fprintf(&b, "mtu=%d\n", ifc.MTU)
	}

	ipv4Addrs, ipv6Addrs := splitAddresses(ifc.Addresses)
	ipv4Routes, ipv6Routes := splitRoutes(routes)
	writeIPSection(&b, "ipv4", ipv4Addrs, ipv4Routes, withDNS, obs.DNSServers, obs.SearchDomains)
	writeIPSection(&b, "ipv6", ipv6Addrs, ipv6Routes, withDNS, obs.DNSServers, obs.SearchDomains)

	return b.String()
}

func splitAddresses(addresses []string) ([]string, []string) {
	var ipv4, ipv6 []string
	for _, address := range addresses {
		if strings.Contains(address, ":") {
			ipv6 = append(ipv6, address)
			continue
		}
		ipv4 = append(ipv4, address)
	}
	return ipv4, ipv6
}

func splitRoutes(routes []elementalv1.ObservedRoute) ([]elementalv1.ObservedRoute, []elementalv1.ObservedRoute) {
	var ipv4, ipv6 []elementalv1.ObservedRoute
	for _, route := range routes {
		if strings.Contains(route.Destination, ":") || strings.Contains(route.Gateway, ":") {
			ipv6 = append(ipv6, route)
			continue
		}
		ipv4 = append(ipv4, route)
	}
	return ipv4, ipv6
}

func writeIPSection(
	b *strings.Builder,
	section string,
	addresses []string,
	routes []elementalv1.ObservedRoute,
	withDNS bool,
	dnsServers []string,
	searchDomains []string,
) {
	fmt.Fprintf(b, "\n[%s]\n", section)
	if len(addresses) == 0 {
		fmt.Fprintln(b, "method=disabled")
		return
	}

	fmt.Fprintln(b, "method=manual")
	for i, address := range addresses {
		fmt.Fprintf(b, "address%d=%s\n", i+1, address)
	}
	for i, route := range routes {
		routeValue := route.Destination
		if isDefaultRoute(route.Destination) {
			routeValue = "0.0.0.0/0"
			if section == "ipv6" {
				routeValue = "::/0"
			}
		}
		if route.Gateway != "" {
			routeValue += "," + route.Gateway
		}
		if route.Metric > 0 {
			routeValue += fmt.Sprintf(",%d", route.Metric)
		}
		fmt.Fprintf(b, "route%d=%s\n", i+1, routeValue)
	}
	if withDNS {
		if dns := filterDNSServers(dnsServers, section); len(dns) > 0 {
			fmt.Fprintf(b, "dns=%s\n", strings.Join(dns, ";"))
		}
		if len(searchDomains) > 0 {
			fmt.Fprintf(b, "dns-search=%s\n", strings.Join(searchDomains, ";"))
		}
	}
}

func filterDNSServers(servers []string, section string) []string {
	var out []string
	for _, server := range servers {
		isIPv6 := strings.Contains(server, ":")
		if (section == "ipv6" && isIPv6) || (section == "ipv4" && !isIPv6) {
			out = append(out, server)
		}
	}
	return out
}

func isDefaultRoute(dst string) bool {
	switch strings.ToLower(dst) {
	case "default", "0.0.0.0/0", "::/0":
		return true
	}
	return false
}
