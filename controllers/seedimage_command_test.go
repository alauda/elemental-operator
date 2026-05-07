/*
Copyright © 2026 SUSE LLC

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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

func TestFillBuildImagePodPullImageTLSVerify(t *testing.T) {
	seedImg := &elementalv1.SeedImage{
		Spec: elementalv1.SeedImageSpec{
			BaseImage:      "elemental/default-iso:latest",
			TargetPlatform: "linux/amd64",
		},
	}

	defaultPod := fillBuildImagePod(seedImg, "default-builder:latest", corev1.PullNever, nil, false)
	defaultArg := defaultPod.Spec.InitContainers[0].Args[0]
	if !strings.Contains(defaultArg, "elemental pull-image --platform=linux/amd64") {
		t.Fatalf("default pull-image command missing platform flag:\n%s", defaultArg)
	}
	if strings.Contains(defaultArg, "--tls-verify=false") {
		t.Fatalf("default pull-image command disabled TLS verify:\n%s", defaultArg)
	}

	disabledPod := fillBuildImagePod(seedImg, "default-builder:latest", corev1.PullNever, nil, true)
	disabledArg := disabledPod.Spec.InitContainers[0].Args[0]
	if !strings.Contains(disabledArg, "elemental pull-image --tls-verify=false --platform=linux/amd64") {
		t.Fatalf("disabled pull-image command missing tls flag:\n%s", disabledArg)
	}
}
