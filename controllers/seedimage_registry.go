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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/util"
)

const (
	// seedImagePullSecret is the platform's registry pull Secret: setup.sh
	// creates it on the bootstrap cluster and platform components pull with
	// it on the Global.
	seedImagePullSecret     = "global-registry-auth"
	seedImageRegistryVolume = "registry-auth"
	seedImageRegistryMount  = "/registry-auth"
)

// seedImagePullAuth returns the dockerconfigjson Secret the builder of a
// SeedImage pulls its base image with, when the builder pulls it itself (ISO
// with targetPlatform): the platform's global-registry-auth in the SeedImage's
// namespace. It returns "" when the builder does not pull or the Secret does
// not exist, which leaves the pull anonymous as before.
func (r *SeedImageReconciler) seedImagePullAuth(ctx context.Context, seedImg *elementalv1.SeedImage) (string, error) {
	if seedImg.Spec.BuildContainer != nil || seedImg.Spec.TargetPlatform == "" ||
		seedImg.Spec.Type == elementalv1.TypeRaw || util.IsHTTP(seedImg.Spec.BaseImage) {
		return "", nil
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: seedImg.Namespace, Name: seedImagePullSecret}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading the registry pull Secret %s/%s: %w", seedImg.Namespace, seedImagePullSecret, err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		return "", fmt.Errorf("registry pull Secret %s/%s has type %s, want %s", seedImg.Namespace, seedImagePullSecret, secret.Type, corev1.SecretTypeDockerConfigJson)
	}
	return secret.Name, nil
}

// applySeedImagePullAuth mounts the pull Secret as the baseiso init
// container's DOCKER_CONFIG and pulls without certificate verification:
// external registry certificates are never verified. baseImage is pulled as
// written, so it names a real registry.
func applySeedImagePullAuth(pod *corev1.Pod, secretName string) {
	if secretName == "" {
		return
	}
	var baseiso *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "baseiso" {
			baseiso = &pod.Spec.InitContainers[i]
		}
	}
	if baseiso == nil {
		return
	}
	readOnly := int32(0o400)
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: seedImageRegistryVolume,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  secretName,
			Items:       []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
			DefaultMode: &readOnly,
		}},
	})
	baseiso.VolumeMounts = append(baseiso.VolumeMounts, corev1.VolumeMount{Name: seedImageRegistryVolume, MountPath: seedImageRegistryMount, ReadOnly: true})
	baseiso.Env = append(baseiso.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: seedImageRegistryMount})
	for i, arg := range baseiso.Args {
		if !strings.Contains(arg, "--tls-verify=false") {
			baseiso.Args[i] = strings.Replace(arg, "elemental pull-image ", "elemental pull-image --tls-verify=false ", 1)
		}
	}
}
