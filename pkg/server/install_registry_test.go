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
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

// defaultRegistryObjects is public-registry-credential carrying a
// two-endpoint registry mirror input, with its credential Secret.
func defaultRegistryObjects(namespace string) []client.Object {
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": "v1", "mode": "mirror", "source": "cpaas-registry.cpaas-system",
		"endpoints":            []string{"harbor.example.com:11443", "backup.example.com"},
		"credentialsSecretRef": map[string]string{"name": "acp-registry-bootstrap-auth-1"},
	})
	return []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "public-registry-credential", Labels: map[string]string{"capi.cpaas.io/provider": "registry-credential"}},
			Type: corev1.SecretType("CloudCredential"), Data: map[string][]byte{"config.json": body}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "acp-registry-bootstrap-auth-1"}, Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"harbor.example.com:11443":{"auth":"cm9ib3Q6eA=="},"backup.example.com":{"auth":"cm9ib3Q6eQ=="}}}`)}},
	}
}

func installRegistryServer(objs ...client.Object) *InventoryServer {
	scheme := runtime.NewScheme()
	_ = elementalv1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	return &InventoryServer{Context: context.Background(), Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()}
}

func kubeosRegistration(labels map[string]string, inventory bool) *elementalv1.MachineRegistration {
	r := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-default", Name: "kubeos", Labels: labels}}
	if inventory {
		r.Spec.Config.Elemental.Install.DeviceSelection = elementalv1.DeviceSelectionInventory
	}
	return r
}

func kubeosConfig(systemURI string, released bool) *elementalv1.Config {
	config := &elementalv1.Config{}
	config.Elemental.Install.SystemURI = systemURI
	if released {
		config.Elemental.InstallTarget = &elementalv1.InstallTarget{}
	}
	return config
}

func TestAddInstallRegistry(t *testing.T) {
	const logical = "cpaas-registry.cpaas-system/tkestack/kubeos-rootfs:v1"
	server := installRegistryServer(defaultRegistryObjects("fleet-default")...)

	t.Run("no system-uri", func(t *testing.T) {
		config := kubeosConfig("", false)
		if err := server.addInstallRegistry(kubeosRegistration(nil, false), nil, config); err != nil || config.Elemental.InstallRegistry != nil {
			t.Fatalf("an install that pulls nothing got the registry: %v %+v", err, config.Elemental.InstallRegistry)
		}
	})
	t.Run("inventory mode before the release", func(t *testing.T) {
		config := kubeosConfig(logical, false)
		if err := server.addInstallRegistry(kubeosRegistration(nil, true), nil, config); err != nil || config.Elemental.InstallRegistry != nil {
			t.Fatalf("a configuration that releases no install got the registry: %v %+v", err, config.Elemental.InstallRegistry)
		}
	})
	t.Run("inventory mode release", func(t *testing.T) {
		// The release resolved it before it consumed the approval.
		resolved := &elementalv1.InstallRegistry{Source: "resolved by the release"}
		config := kubeosConfig("docker://"+logical, true)
		config.Elemental.InstallRegistry = resolved
		if err := server.addInstallRegistry(kubeosRegistration(nil, true), nil, config); err != nil || config.Elemental.InstallRegistry != resolved {
			t.Fatalf("addInstallRegistry() = %v, registry %+v", err, config.Elemental.InstallRegistry)
		}
	})
	for name, inventory := range map[string]bool{"immediate install": false} {
		t.Run(name, func(t *testing.T) {
			config := kubeosConfig("docker://"+logical, inventory)
			if err := server.addInstallRegistry(kubeosRegistration(nil, inventory), nil, config); err != nil {
				t.Fatal(err)
			}
			reg := config.Elemental.InstallRegistry
			if reg == nil || reg.Source != "cpaas-registry.cpaas-system" || strings.Join(reg.Endpoints, ",") != "harbor.example.com:11443,backup.example.com" {
				t.Fatalf("registry = %+v", reg)
			}
			if !strings.Contains(reg.DockerConfig, `"harbor.example.com:11443":{"auth":"cm9ib3Q6eA=="}`) || !strings.Contains(reg.DockerConfig, `"backup.example.com":{"auth":"cm9ib3Q6eQ=="}`) {
				t.Fatalf("docker-config = %s", reg.DockerConfig)
			}
		})
	}
	t.Run("named credential that does not exist fails the configuration", func(t *testing.T) {
		config := kubeosConfig(logical, false)
		err := server.addInstallRegistry(kubeosRegistration(map[string]string{"cpaas.io/registry-reference": "team-registry"}, false), nil, config)
		if err == nil || !strings.Contains(err.Error(), "team-registry") || config.Elemental.InstallRegistry != nil {
			t.Fatalf("addInstallRegistry() = %v, registry %+v", err, config.Elemental.InstallRegistry)
		}
	})
}

func TestAddInstallRegistryWithoutMirrorInput(t *testing.T) {
	server := installRegistryServer()
	t.Run("logical system-uri fails the configuration", func(t *testing.T) {
		config := kubeosConfig("cpaas-registry.cpaas-system/tkestack/kubeos-rootfs:v1", false)
		err := server.addInstallRegistry(kubeosRegistration(nil, false), nil, config)
		if err == nil || !strings.Contains(err.Error(), "no registry mirror input") || config.Elemental.InstallRegistry != nil {
			t.Fatalf("addInstallRegistry() = %v, registry %+v", err, config.Elemental.InstallRegistry)
		}
	})
	t.Run("real system-uri installs without a mirror", func(t *testing.T) {
		config := kubeosConfig("registry.example.com/tkestack/kubeos-rootfs:v1", false)
		if err := server.addInstallRegistry(kubeosRegistration(nil, false), nil, config); err != nil || config.Elemental.InstallRegistry != nil {
			t.Fatalf("addInstallRegistry() = %v, registry %+v", err, config.Elemental.InstallRegistry)
		}
	})
}

// teamRegistryObjects is CloudCredential team-registry, a one-endpoint
// registry mirror input with its credential Secret.
func teamRegistryObjects(namespace string) []client.Object {
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": "v1", "mode": "mirror", "source": "cpaas-registry.cpaas-system",
		"endpoints":            []string{"team.example.com:5000"},
		"credentialsSecretRef": map[string]string{"name": "team-registry-auth"},
	})
	return []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "team-registry", Labels: map[string]string{"capi.cpaas.io/provider": "registry-credential"}},
			Type: corev1.SecretType("CloudCredential"), Data: map[string][]byte{"config.json": body}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "team-registry-auth"}, Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"team.example.com:5000":{"auth":"dGVhbTp6"}}}`)}},
	}
}

func inventoryWithLabels(labels map[string]string) *elementalv1.MachineInventory {
	return &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-default", Name: "node-a", Labels: labels}}
}

// The cpaas.io/registry-reference label of the MachineInventory selects the
// registry of its host's install over the one of the MachineRegistration.
func TestAddInstallRegistryInventoryLabelWins(t *testing.T) {
	const systemURI = "team.example.com:5000/tkestack/baremetal-base-image:v1"
	server := installRegistryServer(append(defaultRegistryObjects("fleet-default"), teamRegistryObjects("fleet-default")...)...)
	team := map[string]string{"cpaas.io/registry-reference": "team-registry"}

	for _, tc := range []struct {
		name         string
		registration map[string]string
		inventory    map[string]string
		endpoints    string
		auth         string
	}{
		{name: "registration default", endpoints: "harbor.example.com:11443,backup.example.com", auth: "cm9ib3Q6eA=="},
		{name: "registration label", registration: team, endpoints: "team.example.com:5000", auth: "dGVhbTp6"},
		{name: "inventory label over the default", inventory: team, endpoints: "team.example.com:5000", auth: "dGVhbTp6"},
		{name: "inventory label over the registration label", registration: map[string]string{"cpaas.io/registry-reference": "missing"}, inventory: team, endpoints: "team.example.com:5000", auth: "dGVhbTp6"},
		{name: "empty inventory label selects the default", registration: team, inventory: map[string]string{"cpaas.io/registry-reference": ""}, endpoints: "harbor.example.com:11443,backup.example.com", auth: "cm9ib3Q6eA=="},
		{name: "other inventory labels do not count", registration: team, inventory: map[string]string{"pool": "a"}, endpoints: "team.example.com:5000", auth: "dGVhbTp6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := kubeosConfig(systemURI, false)
			if err := server.addInstallRegistry(kubeosRegistration(tc.registration, false), inventoryWithLabels(tc.inventory), config); err != nil {
				t.Fatal(err)
			}
			reg := config.Elemental.InstallRegistry
			if reg == nil || strings.Join(reg.Endpoints, ",") != tc.endpoints || !strings.Contains(reg.DockerConfig, tc.auth) {
				t.Fatalf("registry = %+v, want endpoints %s with auth %s", reg, tc.endpoints, tc.auth)
			}
		})
	}
	t.Run("a wrong inventory label names the inventory", func(t *testing.T) {
		config := kubeosConfig(systemURI, false)
		err := server.addInstallRegistry(kubeosRegistration(team, false), inventoryWithLabels(map[string]string{"cpaas.io/registry-reference": "missing"}), config)
		if err == nil || !strings.Contains(err.Error(), "MachineInventory fleet-default/node-a") || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("addInstallRegistry() = %v", err)
		}
	})
}

func TestInstallOfTakesTheInventorySystemURI(t *testing.T) {
	registration := kubeosRegistration(nil, true)
	registration.Spec.Config.Elemental.Install.SystemURI = "harbor.example.com:11443/tkestack/baremetal-base-image:v1"
	registration.Spec.Config.Elemental.Install.Snapshotter = elementalv1.SnapshotterConfig{Type: "btrfs", MaxSnaps: 4}

	for _, tc := range []struct {
		name    string
		install *elementalv1.MachineInventoryInstall
		want    string
	}{
		{name: "no spec.install", want: "harbor.example.com:11443/tkestack/baremetal-base-image:v1"},
		{name: "empty system-uri", install: &elementalv1.MachineInventoryInstall{SystemURI: "  "}, want: "harbor.example.com:11443/tkestack/baremetal-base-image:v1"},
		{name: "override", install: &elementalv1.MachineInventoryInstall{SystemURI: " team.example.com:5000/tkestack/baremetal-base-image:v1 "}, want: "team.example.com:5000/tkestack/baremetal-base-image:v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inventory := inventoryWithLabels(nil)
			inventory.Spec.Install = tc.install
			install := installOf(registration, inventory)
			if install.SystemURI != tc.want {
				t.Fatalf("system-uri = %q, want %q", install.SystemURI, tc.want)
			}
			// Nothing else of the registration's install changes.
			if install.Snapshotter != registration.Spec.Config.Elemental.Install.Snapshotter || install.DeviceSelection != elementalv1.DeviceSelectionInventory {
				t.Fatalf("install = %+v", install)
			}
		})
	}
	if registration.Spec.Config.Elemental.Install.SystemURI != "harbor.example.com:11443/tkestack/baremetal-base-image:v1" {
		t.Fatalf("installOf changed the registration: %q", registration.Spec.Config.Elemental.Install.SystemURI)
	}
}
