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
	"strconv"
	"strings"
	"testing"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/network"
	"gotest.tools/v3/assert"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestObservedNetworkConfigKeepsExplicitNetwork(t *testing.T) {
	inventory := &elementalv1.MachineInventory{}
	inventory.Spec.Network = elementalv1.NetworkConfig{
		Configurator: network.ConfiguratorNmconnections,
		Config: map[string]runtime.RawExtension{
			"explicit": {Raw: []byte(`"explicit-content"`)},
		},
	}
	inventory.Spec.ObservedNetwork = observedNetworkFixture()

	got := observedNetworkConfig(inventory)

	assert.DeepEqual(t, got, inventory.Spec.Network)
}

func TestObservedNetworkConfigEmptyWithoutSnapshot(t *testing.T) {
	got := observedNetworkConfig(&elementalv1.MachineInventory{})

	assert.Equal(t, got.Configurator, "")
	assert.Equal(t, len(got.Config), 0)
}

func TestObservedNetworkConfigFromSnapshot(t *testing.T) {
	inventory := &elementalv1.MachineInventory{}
	inventory.Spec.ObservedNetwork = observedNetworkFixture()

	got := observedNetworkConfig(inventory)

	assert.Equal(t, got.Configurator, network.ConfiguratorNmconnections)
	assert.Equal(t, len(got.Config), 1)

	raw, ok := got.Config["ens3"]
	assert.Equal(t, ok, true)
	content, err := strconv.Unquote(string(raw.Raw))
	assert.NilError(t, err)

	mustContain(t, content, "[connection]")
	mustContain(t, content, "id=ens3")
	mustContain(t, content, "interface-name=ens3")
	mustContain(t, content, "mac-address=02:00:00:00:00:01")
	mustContain(t, content, "mtu=1500")
	mustContain(t, content, "[ipv4]")
	mustContain(t, content, "method=manual")
	mustContain(t, content, "address1=192.168.122.10/24")
	mustContain(t, content, "route1=0.0.0.0/0,192.168.122.1,100")
	mustContain(t, content, "dns=192.168.122.1")
	mustContain(t, content, "dns-search=example.test")
}

func observedNetworkFixture() *elementalv1.ObservedNetwork {
	return &elementalv1.ObservedNetwork{
		Interfaces: []elementalv1.ObservedInterface{
			{
				Name:      "ens3",
				MAC:       "02:00:00:00:00:01",
				MTU:       1500,
				Addresses: []string{"192.168.122.10/24"},
			},
			{
				Name:      "no-address",
				MAC:       "02:00:00:00:00:02",
				Addresses: nil,
			},
		},
		Routes: []elementalv1.ObservedRoute{
			{
				Destination: "default",
				Gateway:     "192.168.122.1",
				Interface:   "ens3",
				Metric:      100,
			},
		},
		DNSServers:    []string{"192.168.122.1"},
		SearchDomains: []string{"example.test"},
	}
}

func mustContain(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Fatalf("expected %q to contain %q", s, substr)
	}
}

func TestObservedNetworkConfigPrefersCapturedConnections(t *testing.T) {
	bond := "[connection]\nid=bond0\ntype=bond\n\n[bond]\nmode=active-backup\n"
	slave := "[connection]\nid=eth0\ntype=ethernet\nmaster=bond0\nslave-type=bond\n"

	observed := observedNetworkFixture()
	observed.Connections = map[string]string{"bond0": bond, "eth0": slave}

	inventory := &elementalv1.MachineInventory{}
	inventory.Spec.ObservedNetwork = observed

	got := observedNetworkConfig(inventory)

	assert.Equal(t, network.ConfiguratorNmconnections, got.Configurator)
	assert.Equal(t, 2, len(got.Config))
	// The keys are the keyfile base names, so the applicator writes each one
	// back to the very path it was read from.
	for name, want := range map[string]string{"bond0": bond, "eth0": slave} {
		raw, found := got.Config[name]
		assert.Assert(t, found, "connection %q is missing", name)
		unquoted, err := strconv.Unquote(string(raw.Raw))
		assert.NilError(t, err)
		assert.Equal(t, want, unquoted)
	}
}

func TestObservedNetworkConfigRefusesToGuessForAggregatedLinks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		iface elementalv1.ObservedInterface
	}{
		{
			name:  "the aggregate itself",
			iface: elementalv1.ObservedInterface{Name: "bond0", MAC: "02:00:00:00:00:03", Kind: "bond", Addresses: []string{"10.0.0.5/24"}},
		},
		{
			name:  "a member of one",
			iface: elementalv1.ObservedInterface{Name: "eth1", MAC: "02:00:00:00:00:04", Master: "bond0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := observedNetworkFixture()
			observed.Interfaces = append(observed.Interfaces, tc.iface)

			inventory := &elementalv1.MachineInventory{}
			inventory.Spec.ObservedNetwork = observed

			// Rendering the address snapshot would flatten the aggregate into
			// an ethernet profile bound to a member's MAC, so nothing is sent.
			assert.DeepEqual(t, elementalv1.NetworkConfig{}, observedNetworkConfig(inventory))
		})
	}
}

func TestObservedNetworkConfigStillRendersPlainLinks(t *testing.T) {
	// An older elemental-register reports neither Kind nor Master; the guard
	// must not fire and turn a working host into an unconfigured one.
	inventory := &elementalv1.MachineInventory{}
	inventory.Spec.ObservedNetwork = observedNetworkFixture()

	got := observedNetworkConfig(inventory)

	assert.Equal(t, network.ConfiguratorNmconnections, got.Configurator)
	assert.Equal(t, 1, len(got.Config))
	raw, found := got.Config["ens3"]
	assert.Assert(t, found)
	unquoted, err := strconv.Unquote(string(raw.Raw))
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(unquoted, "type=ethernet"))
}
