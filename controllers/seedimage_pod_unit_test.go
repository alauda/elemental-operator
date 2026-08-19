package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

func TestFillBuildImagePodToleratesControlPlaneTaints(t *testing.T) {
	t.Parallel()

	pod := fillBuildImagePod(&elementalv1.SeedImage{}, "default-builder:latest", corev1.PullNever, nil, false)
	want := defaultSeedImageTolerations()
	if len(pod.Spec.Tolerations) != len(want) {
		t.Fatalf("tolerations len = %d, want %d", len(pod.Spec.Tolerations), len(want))
	}
	for i := range want {
		got := pod.Spec.Tolerations[i]
		if got.Key != want[i].Key || got.Operator != want[i].Operator || got.Effect != want[i].Effect {
			t.Fatalf("toleration[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}
