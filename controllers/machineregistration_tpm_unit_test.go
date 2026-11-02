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
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

// These run on a fake client, so they need no envtest binaries.

// newFakeElementalClient registers no status subresource on purpose. The
// reconcilers patch the object and then its status with the same
// optimistic-lock base; the fake client bumps resourceVersion on the first,
// even no-op, patch, so the status patch would always conflict. Without the
// subresource the first patch carries the status and the second returns
// NotFound, which the reconcilers ignore.
func newFakeElementalClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := elementalv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func reconcileRegistrationReady(t *testing.T, cl client.Client, name string) *metav1.Condition {
	t.Helper()
	g := NewWithT(t)
	r := &MachineRegistrationReconciler{Client: cl, ServerURL: "https://example.com"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: name}})
	g.Expect(err).ToNot(HaveOccurred())
	current := &elementalv1.MachineRegistration{}
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, current)).To(Succeed())
	return meta.FindStatusCondition(current.Status.Conditions, elementalv1.ReadyCondition)
}

func TestMachineRegistrationTPMModeValidation(t *testing.T) {
	tests := []struct {
		name       string
		reg        elementalv1.Registration
		wantReason string
	}{
		{name: "auto with emulate-tpm", reg: elementalv1.Registration{Auth: "tpm", TPMMode: "auto", EmulateTPM: true}, wantReason: elementalv1.InvalidTPMConfigReason},
		{name: "hardware with emulate-tpm", reg: elementalv1.Registration{Auth: "tpm", TPMMode: "hardware", EmulateTPM: true}, wantReason: elementalv1.InvalidTPMConfigReason},
		{name: "auto with default auth and emulate-tpm", reg: elementalv1.Registration{TPMMode: "auto", EmulateTPM: true}, wantReason: elementalv1.InvalidTPMConfigReason},
		{name: "unknown mode", reg: elementalv1.Registration{Auth: "tpm", TPMMode: "maybe"}, wantReason: elementalv1.InvalidTPMConfigReason},
		{name: "auto", reg: elementalv1.Registration{Auth: "tpm", TPMMode: "auto"}, wantReason: elementalv1.SuccessfullyCreatedReason},
		{name: "emulated with emulate-tpm", reg: elementalv1.Registration{Auth: "tpm", TPMMode: "emulated", EmulateTPM: true}, wantReason: elementalv1.SuccessfullyCreatedReason},
		{name: "legacy emulate-tpm only", reg: elementalv1.Registration{Auth: "tpm", EmulateTPM: true}, wantReason: elementalv1.SuccessfullyCreatedReason},
		{name: "ignored for auth mac", reg: elementalv1.Registration{Auth: "mac", TPMMode: "auto", EmulateTPM: true}, wantReason: elementalv1.SuccessfullyCreatedReason},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			mRegistration := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Name: "reg", Namespace: "default"}}
			mRegistration.Spec.Config.Elemental.Registration = tt.reg
			cl := newFakeElementalClient(t, mRegistration)

			ready := reconcileRegistrationReady(t, cl, "reg")
			g.Expect(ready).ToNot(BeNil())
			g.Expect(ready.Reason).To(Equal(tt.wantReason))
			if tt.wantReason == elementalv1.InvalidTPMConfigReason {
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(ready.Message).ToNot(BeEmpty())
			} else {
				g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			}
		})
	}
}

func TestMachineRegistrationTPMModeValidationFollowsSpecEdits(t *testing.T) {
	g := NewWithT(t)
	mRegistration := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Name: "reg", Namespace: "default"}}
	mRegistration.Spec.Config.Elemental.Registration = elementalv1.Registration{Auth: "tpm", TPMMode: "auto"}
	cl := newFakeElementalClient(t, mRegistration)
	ctx := context.Background()

	ready := reconcileRegistrationReady(t, cl, "reg")
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// A ready registration that gets a contradicting spec must turn unready,
	// otherwise the next SeedImage would bake the contradiction into an ISO.
	current := &elementalv1.MachineRegistration{}
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(mRegistration), current)).To(Succeed())
	current.Spec.Config.Elemental.Registration.EmulateTPM = true
	g.Expect(cl.Update(ctx, current)).To(Succeed())
	ready = reconcileRegistrationReady(t, cl, "reg")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(Equal(elementalv1.InvalidTPMConfigReason))
	g.Expect(ready.Message).To(ContainSubstring("emulate-tpm"))

	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(mRegistration), current)).To(Succeed())
	current.Spec.Config.Elemental.Registration.EmulateTPM = false
	g.Expect(cl.Update(ctx, current)).To(Succeed())
	ready = reconcileRegistrationReady(t, cl, "reg")
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(Equal(elementalv1.SuccessfullyCreatedReason))
}

func TestGetClientRegistrationConfigPassesTPMMode(t *testing.T) {
	g := NewWithT(t)
	mRegistration := elementalv1.MachineRegistration{}
	mRegistration.Spec.Config.Elemental.Registration = elementalv1.Registration{Auth: "tpm", TPMMode: elementalv1.TPMModeAuto, EmulatedTPMSeed: -1}
	mRegistration.Status.RegistrationURL = "https://example.com/elemental/registration/token"
	mRegistration.Status.Conditions = []metav1.Condition{{Type: elementalv1.ReadyCondition, Status: metav1.ConditionTrue, Reason: elementalv1.SuccessfullyCreatedReason}}

	config, err := mRegistration.GetClientRegistrationConfig("ca")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(config.Elemental.Registration.TPMMode).To(Equal(elementalv1.TPMModeAuto))
	g.Expect(config.Elemental.Registration.EmulatedTPMSeed).To(Equal(int64(-1)))
}
