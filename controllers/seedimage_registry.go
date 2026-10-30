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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/registryaccess"
	"github.com/rancher/elemental-operator/pkg/util"
)

const (
	seedImageRegistryVolume = "registry-auth"
	seedImageRegistryMount  = "/registry-auth"
)

// seedImageRegistry is the registry mirror the base image of a SeedImage is
// pulled through, with the Secret holding its credentials.
type seedImageRegistry struct {
	source     string
	endpoints  []string
	secretName string
}

// seedImageRegistry resolves the registry mirror of the MachineRegistration's
// registryClusterRef for a SeedImage whose builder pulls the base image
// itself (ISO with targetPlatform) and keeps its credentials in a Secret the
// SeedImage owns. It returns nil when the builder does not pull or the
// MachineRegistration names no cluster.
func (r *SeedImageReconciler) seedImageRegistry(ctx context.Context, seedImg *elementalv1.SeedImage, mRegistration *elementalv1.MachineRegistration) (*seedImageRegistry, error) {
	ref := mRegistration.Spec.RegistryClusterRef
	if ref == nil || ref.Name == "" || seedImg.Spec.BuildContainer != nil || seedImg.Spec.TargetPlatform == "" ||
		seedImg.Spec.Type == elementalv1.TypeRaw || util.IsHTTP(seedImg.Spec.BaseImage) {
		return nil, nil
	}
	access, err := registryaccess.Resolve(ctx, r.Client, mRegistration.Namespace, ref.Name)
	if err != nil {
		return nil, fmt.Errorf("registry mirror of registryClusterRef %s: %w", ref.Name, err)
	}
	doc, err := access.DockerConfig()
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: seedImg.Name + "-registry-auth", Namespace: seedImg.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{"config.json": doc}
		return controllerutil.SetControllerReference(seedImg, secret, r.Scheme())
	}); err != nil {
		return nil, fmt.Errorf("writing the registry mirror credentials of SeedImage %s/%s: %w", seedImg.Namespace, seedImg.Name, err)
	}
	return &seedImageRegistry{source: access.Source, endpoints: access.Endpoints, secretName: secret.Name}, nil
}

// applySeedImageRegistry makes the baseiso init container pull the base
// image with the mirror credentials, without certificate verification. A base
// image under the logical registry is tried on each endpoint in order.
func applySeedImageRegistry(pod *corev1.Pod, seedImg *elementalv1.SeedImage, registry *seedImageRegistry) {
	if registry == nil {
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
		Name:         seedImageRegistryVolume,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: registry.secretName, DefaultMode: &readOnly}},
	})
	baseiso.VolumeMounts = append(baseiso.VolumeMounts, corev1.VolumeMount{Name: seedImageRegistryVolume, MountPath: seedImageRegistryMount, ReadOnly: true})
	baseiso.Env = append(baseiso.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: seedImageRegistryMount})

	refs := []string{seedImg.Spec.BaseImage}
	if rest, ok := strings.CutPrefix(seedImg.Spec.BaseImage, registry.source+"/"); ok {
		refs = refs[:0]
		for _, endpoint := range registry.endpoints {
			refs = append(refs, endpoint+"/"+rest)
		}
	}
	quoted := make([]string, 0, len(refs))
	for _, ref := range refs {
		quoted = append(quoted, shellQuote(ref))
	}
	baseiso.Args = []string{fmt.Sprintf(`mkdir /work && for ref in %s; do
  if elemental pull-image --tls-verify=false --platform=%s "$ref" /work; then cp /work/elemental-iso/*.iso /iso/base.iso; exit 0; fi
  echo "pulling $ref failed; trying the next registry mirror endpoint" >&2
  rm -rf /work/*
done
exit 1`, strings.Join(quoted, " "), shellQuote(string(seedImg.Spec.TargetPlatform)))}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
