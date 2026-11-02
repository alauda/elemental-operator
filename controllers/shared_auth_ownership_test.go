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
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

// These run on a fake client, so they need no envtest binaries.

func etcdSyncConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: etcdSyncNamespace, Name: etcdSyncConfigMapName}}
}

// fakeClock lets a test move through the protection gap.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newObserverWithClock(reader client.Reader) (*etcdSyncObserver, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	o := &etcdSyncObserver{reader: reader, now: clock.now}
	o.changedAt = clock.now().Add(-etcdSyncProtectGap)
	return o, clock
}

func TestNormalizeSharedAuthOwnership(t *testing.T) {
	g := NewWithT(t)
	for in, want := range map[string]string{
		"":          SharedAuthOwnershipAuto,
		"auto":      SharedAuthOwnershipAuto,
		" Owner ":   SharedAuthOwnershipOwner,
		"READONLY":  SharedAuthOwnershipReadOnly,
		"readonly ": SharedAuthOwnershipReadOnly,
	} {
		got, err := NormalizeSharedAuthOwnership(in)
		g.Expect(err).ToNot(HaveOccurred(), in)
		g.Expect(got).To(Equal(want), in)
	}
	_, err := NormalizeSharedAuthOwnership("read-only")
	g.Expect(err).To(MatchError(ContainSubstring(`invalid shared system-agent auth ownership "read-only"`)))
}

func TestEtcdSyncObserverFollowsTheConfigMap(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cl := newFakeElementalClient(t)
	o, clock := newObserverWithClock(cl)

	v := o.observe(ctx)
	g.Expect(v.readOnly).To(BeFalse(), "a ConfigMap never seen leaves this side the owner")
	g.Expect(v.recheckAfter).To(BeZero())

	g.Expect(cl.Create(ctx, etcdSyncConfigMap())).To(Succeed())
	v = o.observe(ctx)
	g.Expect(v.readOnly).To(BeTrue(), "etcd-sync writes here")
	g.Expect(v.recheckAfter).To(BeZero(), "the ConfigMap deletion is an event, no recheck needed")

	g.Expect(cl.Delete(ctx, etcdSyncConfigMap())).To(Succeed())
	clock.advance(20 * time.Second)
	v = o.observe(ctx)
	g.Expect(v.readOnly).To(BeTrue(), "read-only during the protection gap")
	g.Expect(v.recheckAfter).To(Equal(etcdSyncProtectGap), "the gap starts when the deletion is seen")

	clock.advance(etcdSyncProtectGap - time.Second)
	v = o.observe(ctx)
	g.Expect(v.readOnly).To(BeTrue())
	g.Expect(v.recheckAfter).To(Equal(time.Second))

	clock.advance(time.Second)
	v = o.observe(ctx)
	g.Expect(v.readOnly).To(BeFalse(), "owner once the gap has passed")
	g.Expect(v.recheckAfter).To(BeZero())
}

type failingReader struct{ client.Reader }

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return apierrors.NewServiceUnavailable("apiserver is restarting")
}

func TestEtcdSyncObserverStaysReadOnlyWhenItCannotTell(t *testing.T) {
	g := NewWithT(t)
	o, _ := newObserverWithClock(failingReader{})
	v := o.observe(context.Background())
	g.Expect(v.readOnly).To(BeTrue())
	g.Expect(v.recheckAfter).To(Equal(etcdSyncRetryInterval))
	g.Expect(v.reason).To(ContainSubstring("cannot read kube-public/etcd-sync"))
}

func TestEtcdSyncObserverReportsEachRoleChangeOnce(t *testing.T) {
	g := NewWithT(t)
	o, _ := newObserverWithClock(failingReader{})
	from, changed := o.transition(false)
	g.Expect(changed).To(BeTrue())
	g.Expect(from).To(Equal("unknown"))
	_, changed = o.transition(false)
	g.Expect(changed).To(BeFalse())
	from, changed = o.transition(true)
	g.Expect(changed).To(BeTrue())
	g.Expect(from).To(Equal("owner"))
}

func TestSharedAuthVerdictHonoursAFixedOwnership(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cl := newFakeElementalClient(t, etcdSyncConfigMap())

	owner := &MachineRegistrationReconciler{Client: cl, SystemAgentSharedAuthOwnership: SharedAuthOwnershipOwner}
	g.Expect(owner.sharedAuthVerdict(ctx).readOnly).To(BeFalse(), "owner ignores etcd-sync")

	readOnly := &MachineRegistrationReconciler{Client: newFakeElementalClient(t), SystemAgentSharedAuthOwnership: SharedAuthOwnershipReadOnly}
	g.Expect(readOnly.sharedAuthVerdict(ctx).readOnly).To(BeTrue(), "readonly holds without etcd-sync")

	auto := &MachineRegistrationReconciler{Client: cl}
	g.Expect(auto.sharedAuthVerdict(ctx).readOnly).To(BeTrue(), "empty ownership is auto")
}

// The shared identity follows the role: a standby only checks it, and the
// side takes it over once etcd-sync is gone and the gap has passed.
func TestSharedIdentityFollowsTheEtcdSyncRole(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cl := newFakeElementalClient(t, etcdSyncConfigMap())
	o, clock := newObserverWithClock(cl)
	r := &MachineRegistrationReconciler{
		Client:                    cl,
		SystemAgentAuthMode:       SystemAgentAuthModeShared,
		SystemAgentServiceAccount: DefaultSharedSystemAgentServiceAccountName,
		etcdSync:                  o,
	}
	registration := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Namespace: "cpaas-system", Name: "workers"}}
	saKey := client.ObjectKey{Namespace: "cpaas-system", Name: DefaultSharedSystemAgentServiceAccountName}

	recheck, err := r.createRBACObjects(ctx, registration)
	g.Expect(err).To(MatchError(ContainSubstring("externally managed shared system-agent ServiceAccount")),
		"a standby checks the identity etcd-sync copies, it does not create one")
	g.Expect(recheck).To(BeZero())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, saKey, &corev1.ServiceAccount{}))).To(BeTrue())

	g.Expect(cl.Delete(ctx, etcdSyncConfigMap())).To(Succeed())
	recheck, err = r.createRBACObjects(ctx, registration)
	g.Expect(err).To(HaveOccurred(), "still read-only right after etcd-sync is removed")
	g.Expect(recheck).To(Equal(etcdSyncProtectGap))
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, saKey, &corev1.ServiceAccount{}))).To(BeTrue())

	clock.advance(etcdSyncProtectGap)
	recheck, err = r.createRBACObjects(ctx, registration)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(recheck).To(BeZero())
	g.Expect(cl.Get(ctx, saKey, &corev1.ServiceAccount{})).To(Succeed(), "the side now owns the shared identity")
}

func TestEtcdSyncChangeReconcilesEveryRegistration(t *testing.T) {
	g := NewWithT(t)
	cl := newFakeElementalClient(t,
		&elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Namespace: "cpaas-system", Name: "workers"}},
		&elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-default", Name: "edge"}},
	)
	r := &MachineRegistrationReconciler{Client: cl}
	requests := r.etcdSyncToMachineRegistrations(context.Background(), etcdSyncConfigMap())
	var keys []string
	for _, req := range requests {
		keys = append(keys, req.String())
	}
	g.Expect(keys).To(ConsistOf("cpaas-system/workers", "fleet-default/edge"))
}
