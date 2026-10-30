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

package controllers

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

func seedImageMirrorObjects() []client.Object {
	cluster := &unstructured.Unstructured{}
	cluster.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	cluster.SetKind("Cluster")
	cluster.SetNamespace("fleet-default")
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
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-default", Name: "acp-registry-config-global"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"config.json": body}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-default", Name: "acp-registry-bootstrap-global"}, Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"harbor.example.com:11443":{"auth":"cm9ib3Q6eA=="},"backup.example.com":{"auth":"cm9ib3Q6eQ=="}}}`)}},
	}
}

func TestSeedImageRegistry(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = elementalv1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	seedImg := &elementalv1.SeedImage{
		ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-default", Name: "kubeos-arm", UID: "seed-uid"},
		Spec: elementalv1.SeedImageSpec{
			BaseImage:      "cpaas-registry.cpaas-system/tkestack/kubeos-iso:alaudaos-44.ku.1-0.aarch64",
			TargetPlatform: "linux/arm64",
		},
	}
	registration := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-default", Name: "kubeos"}}
	registration.Spec.RegistryClusterRef = &corev1.LocalObjectReference{Name: "global"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seedImageMirrorObjects()...).Build()
	r := &SeedImageReconciler{Client: c}

	registry, err := r.seedImageRegistry(context.Background(), seedImg, registration)
	if err != nil {
		t.Fatal(err)
	}
	if registry == nil || registry.secretName != "kubeos-arm-registry-auth" || strings.Join(registry.endpoints, ",") != "harbor.example.com:11443,backup.example.com" {
		t.Fatalf("registry = %+v", registry)
	}
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "fleet-default", Name: "kubeos-arm-registry-auth"}, secret); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(secret.Data["config.json"]), `"backup.example.com":{"auth":"cm9ib3Q6eQ=="}`) {
		t.Fatalf("credential Secret = %s", secret.Data["config.json"])
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != "seed-uid" {
		t.Fatalf("credential Secret owners = %+v, want the SeedImage", secret.OwnerReferences)
	}

	// Nothing to do when the builder does not pull the base image itself.
	for name, mutate := range map[string]func(*elementalv1.SeedImage){
		"kubelet pulls the base image": func(s *elementalv1.SeedImage) { s.Spec.TargetPlatform = "" },
		"raw image":                    func(s *elementalv1.SeedImage) { s.Spec.Type = elementalv1.TypeRaw },
		"http base image":              func(s *elementalv1.SeedImage) { s.Spec.BaseImage = "https://example.com/base.iso" },
	} {
		s := seedImg.DeepCopy()
		mutate(s)
		if got, err := r.seedImageRegistry(context.Background(), s, registration); err != nil || got != nil {
			t.Fatalf("%s: seedImageRegistry() = %+v, %v; want nothing", name, got, err)
		}
	}
	registration.Spec.RegistryClusterRef.Name = "missing"
	if _, err := r.seedImageRegistry(context.Background(), seedImg, registration); err == nil || !strings.Contains(err.Error(), "registryClusterRef missing") {
		t.Fatalf("seedImageRegistry() error = %v", err)
	}
}

// The rewritten baseiso command tries each endpoint and stops at the first
// pull that works; run it with a stub elemental.
func TestApplySeedImageRegistryTriesEachEndpoint(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	seedImg := &elementalv1.SeedImage{Spec: elementalv1.SeedImageSpec{
		BaseImage:      "cpaas-registry.cpaas-system/tkestack/kubeos-iso:alaudaos-44.ku.1-0.aarch64",
		TargetPlatform: "linux/arm64",
	}}
	pod := fillBuildImagePod(seedImg, "builder:latest", corev1.PullNever, nil, false)
	applySeedImageRegistry(pod, seedImg, &seedImageRegistry{source: "cpaas-registry.cpaas-system", endpoints: []string{"harbor.example.com:11443", "backup.example.com"}, secretName: "kubeos-arm-registry-auth"})

	baseiso := pod.Spec.InitContainers[0]
	if baseiso.Name != "baseiso" {
		t.Fatalf("first init container = %s", baseiso.Name)
	}
	if !containsEnv(baseiso.Env, "DOCKER_CONFIG", seedImageRegistryMount) || !containsMount(baseiso.VolumeMounts, seedImageRegistryVolume, seedImageRegistryMount) {
		t.Fatalf("baseiso env %v mounts %v", baseiso.Env, baseiso.VolumeMounts)
	}
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == seedImageRegistryVolume && v.Secret != nil && v.Secret.SecretName == "kubeos-arm-registry-auth" {
			found = true
		}
	}
	if !found {
		t.Fatalf("volumes = %v", pod.Spec.Volumes)
	}

	// Run the command: /work and /iso under a temp root, elemental stubbed
	// to fail on the first endpoint.
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/bash
echo "$*" >> ` + root + `/calls
case "$*" in *harbor.example.com*) exit 1;; esac
mkdir -p "$5/elemental-iso" && echo iso > "$5/elemental-iso/base.iso"
`
	if err := os.WriteFile(filepath.Join(bin, "elemental"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "iso"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("/work", root+"/work", "/iso/", root+"/iso/").Replace(baseiso.Args[0])
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("baseiso command failed: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(filepath.Join(root, "calls"))
	want := "pull-image --tls-verify=false --platform=linux/arm64 harbor.example.com:11443/tkestack/kubeos-iso:alaudaos-44.ku.1-0.aarch64 " + root + "/work\n" +
		"pull-image --tls-verify=false --platform=linux/arm64 backup.example.com/tkestack/kubeos-iso:alaudaos-44.ku.1-0.aarch64 " + root + "/work\n"
	if string(calls) != want {
		t.Fatalf("elemental calls =\n%s\nwant\n%s", calls, want)
	}
	if b, err := os.ReadFile(filepath.Join(root, "iso/base.iso")); err != nil || string(b) != "iso\n" {
		t.Fatalf("base.iso = %q, %v", b, err)
	}
}

func containsEnv(env []corev1.EnvVar, name, value string) bool {
	for _, e := range env {
		if e.Name == name && e.Value == value {
			return true
		}
	}
	return false
}

func containsMount(mounts []corev1.VolumeMount, name, path string) bool {
	for _, m := range mounts {
		if m.Name == name && m.MountPath == path {
			return true
		}
	}
	return false
}
