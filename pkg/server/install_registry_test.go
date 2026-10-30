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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

// mirrorClusterObjects is a CAPI Cluster pulling through a two-endpoint
// registry mirror, with its bootstrap and credential Secrets.
func mirrorClusterObjects(namespace string) []client.Object {
	cluster := &unstructured.Unstructured{}
	cluster.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	cluster.SetKind("Cluster")
	cluster.SetNamespace(namespace)
	cluster.SetName("global")
	cluster.SetUID("cluster-uid")
	cluster.SetAnnotations(map[string]string{"cpaas.io/registry-mode": "mirror", "cpaas.io/registry-config-secret": "acp-registry-config-global"})
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": "v1", "mode": "mirror", "source": "cpaas-registry.cpaas-system",
		"endpoints": []string{"harbor.example.com:11443", "backup.example.com"}, "clusterUID": "cluster-uid",
		"configVersion":                 "sha256:" + strings.Repeat("3", 64),
		"bootstrapCredentialsSecretRef": map[string]string{"name": "acp-registry-bootstrap-global"},
	})
	return []client.Object{
		cluster,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "acp-registry-config-global"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"config.json": body}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "acp-registry-bootstrap-global"}, Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"harbor.example.com:11443":{"auth":"cm9ib3Q6eA=="},"backup.example.com":{"auth":"cm9ib3Q6eQ=="}}}`)}},
	}
}

func TestAddInstallRegistry(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = elementalv1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	server := &InventoryServer{Context: context.Background(), Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(mirrorClusterObjects("fleet-default")...).Build()}
	registration := func(ref string, inventory bool) *elementalv1.MachineRegistration {
		r := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-default", Name: "kubeos"}}
		if ref != "" {
			r.Spec.RegistryClusterRef = &corev1.LocalObjectReference{Name: ref}
		}
		if inventory {
			r.Spec.Config.Elemental.Install.DeviceSelection = elementalv1.DeviceSelectionInventory
		}
		return r
	}

	t.Run("no registryClusterRef", func(t *testing.T) {
		config := &elementalv1.Config{}
		if err := server.addInstallRegistry(registration("", false), config); err != nil || config.Elemental.InstallRegistry != nil {
			t.Fatalf("addInstallRegistry() = %v, registry %+v", err, config.Elemental.InstallRegistry)
		}
	})
	t.Run("inventory mode before the release", func(t *testing.T) {
		config := &elementalv1.Config{}
		if err := server.addInstallRegistry(registration("global", true), config); err != nil || config.Elemental.InstallRegistry != nil {
			t.Fatalf("a configuration that releases no install got the registry: %v %+v", err, config.Elemental.InstallRegistry)
		}
	})
	for name, inventory := range map[string]bool{"inventory mode release": true, "immediate install": false} {
		t.Run(name, func(t *testing.T) {
			config := &elementalv1.Config{}
			if inventory {
				config.Elemental.InstallTarget = &elementalv1.InstallTarget{}
			}
			if err := server.addInstallRegistry(registration("global", inventory), config); err != nil {
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
	t.Run("cluster without mirror input fails the configuration", func(t *testing.T) {
		config := &elementalv1.Config{}
		err := server.addInstallRegistry(registration("missing", false), config)
		if err == nil || !strings.Contains(err.Error(), "registryClusterRef missing") || config.Elemental.InstallRegistry != nil {
			t.Fatalf("addInstallRegistry() = %v, registry %+v", err, config.Elemental.InstallRegistry)
		}
	})
}
