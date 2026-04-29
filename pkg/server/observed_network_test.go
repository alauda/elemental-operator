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
