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
	"os"
	"os/exec"
	"path/filepath"
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

func pullSecret(typ corev1.SecretType) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cpaas-system", Name: seedImagePullSecret},
		Type:       typ,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"harbor.example.com:11443":{"auth":"cm9ib3Q6eA=="}}}`)},
	}
}

func TestSeedImagePullAuth(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = elementalv1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	reconciler := func(objs ...client.Object) *SeedImageReconciler {
		return &SeedImageReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()}
	}
	seedImg := &elementalv1.SeedImage{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cpaas-system", Name: "kubeos-arm"},
		Spec: elementalv1.SeedImageSpec{
			BaseImage:      "harbor.example.com:11443/tkestack/kubeos-iso:alaudaos-44.ku.1-0.aarch64",
			TargetPlatform: "linux/arm64",
		},
	}

	if got, err := reconciler(pullSecret(corev1.SecretTypeDockerConfigJson)).seedImagePullAuth(context.Background(), seedImg); err != nil || got != seedImagePullSecret {
		t.Fatalf("seedImagePullAuth() = %q, %v; want %s", got, err, seedImagePullSecret)
	}
	if got, err := reconciler().seedImagePullAuth(context.Background(), seedImg); err != nil || got != "" {
		t.Fatalf("without the Secret: seedImagePullAuth() = %q, %v; want an anonymous pull", got, err)
	}
	if _, err := reconciler(pullSecret(corev1.SecretTypeOpaque)).seedImagePullAuth(context.Background(), seedImg); err == nil || !strings.Contains(err.Error(), "has type Opaque") {
		t.Fatalf("seedImagePullAuth() error = %v, want a type error", err)
	}

	// Nothing to do when the builder does not pull the base image itself.
	for name, mutate := range map[string]func(*elementalv1.SeedImage){
		"kubelet pulls the base image": func(s *elementalv1.SeedImage) { s.Spec.TargetPlatform = "" },
		"raw image":                    func(s *elementalv1.SeedImage) { s.Spec.Type = elementalv1.TypeRaw },
		"http base image":              func(s *elementalv1.SeedImage) { s.Spec.BaseImage = "https://example.com/base.iso" },
	} {
		s := seedImg.DeepCopy()
		mutate(s)
		if got, err := reconciler(pullSecret(corev1.SecretTypeDockerConfigJson)).seedImagePullAuth(context.Background(), s); err != nil || got != "" {
			t.Fatalf("%s: seedImagePullAuth() = %q, %v; want nothing", name, got, err)
		}
	}
}

// The baseiso command pulls baseImage as written, with the mounted
// credentials and without certificate verification; run it with a stub
// elemental.
func TestApplySeedImagePullAuth(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	seedImg := &elementalv1.SeedImage{Spec: elementalv1.SeedImageSpec{
		BaseImage:      "harbor.example.com:11443/tkestack/kubeos-iso:alaudaos-44.ku.1-0.aarch64",
		TargetPlatform: "linux/arm64",
	}}
	for name, disableTLSVerify := range map[string]bool{"verifying operator": false, "operator already skips verification": true} {
		t.Run(name, func(t *testing.T) {
			pod := fillBuildImagePod(seedImg, "builder:latest", corev1.PullNever, nil, disableTLSVerify)
			applySeedImagePullAuth(pod, seedImagePullSecret)

			baseiso := pod.Spec.InitContainers[0]
			if baseiso.Name != "baseiso" {
				t.Fatalf("first init container = %s", baseiso.Name)
			}
			if !containsEnv(baseiso.Env, "DOCKER_CONFIG", seedImageRegistryMount) || !containsMount(baseiso.VolumeMounts, seedImageRegistryVolume, seedImageRegistryMount) {
				t.Fatalf("baseiso env %v mounts %v", baseiso.Env, baseiso.VolumeMounts)
			}
			found := false
			for _, v := range pod.Spec.Volumes {
				if v.Name == seedImageRegistryVolume && v.Secret != nil && v.Secret.SecretName == seedImagePullSecret &&
					len(v.Secret.Items) == 1 && v.Secret.Items[0].Key == corev1.DockerConfigJsonKey && v.Secret.Items[0].Path == "config.json" {
					found = true
				}
			}
			if !found {
				t.Fatalf("volumes = %v", pod.Spec.Volumes)
			}
			if strings.Count(baseiso.Args[0], "--tls-verify=false") != 1 {
				t.Fatalf("baseiso args = %q, want one --tls-verify=false", baseiso.Args[0])
			}

			// Run the command: /work and /iso under a temp root.
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			stub := `#!/bin/bash
echo "$*" >> ` + root + `/calls
mkdir -p "${@: -1}/elemental-iso" && echo iso > "${@: -1}/elemental-iso/base.iso"
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
			want := "pull-image --tls-verify=false --platform=linux/arm64 harbor.example.com:11443/tkestack/kubeos-iso:alaudaos-44.ku.1-0.aarch64 " + root + "/work\n"
			if string(calls) != want {
				t.Fatalf("elemental calls =\n%s\nwant\n%s", calls, want)
			}
		})
	}
}

// Without a pull Secret the pod is left as the upstream builder makes it.
func TestApplySeedImagePullAuthWithoutSecret(t *testing.T) {
	seedImg := &elementalv1.SeedImage{Spec: elementalv1.SeedImageSpec{BaseImage: "registry.example.com/iso:1", TargetPlatform: "linux/amd64"}}
	pod := fillBuildImagePod(seedImg, "builder:latest", corev1.PullNever, nil, false)
	before := pod.DeepCopy()
	applySeedImagePullAuth(pod, "")
	if len(pod.Spec.Volumes) != len(before.Spec.Volumes) || pod.Spec.InitContainers[0].Args[0] != before.Spec.InitContainers[0].Args[0] {
		t.Fatalf("pod changed without a pull Secret")
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
