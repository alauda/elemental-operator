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
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/register"
)

// The live install session between the real elemental-register client and
// this server: the other tests drive the server message by message, this one
// checks that both sides read what the other writes.
func TestLiveInstallWithTheRegisterClient(t *testing.T) {
	g := NewWithT(t)
	const (
		namespace = "fleet-default"
		token     = "live-token"
		name      = "live-host"
		uuid      = "4c4c4544-0042-3510-8051-b2c04f4e3132"
	)

	// The client authenticates with auth sys-uuid, which reads the SMBIOS
	// UUID through ghw: give it one in a fake sysfs.
	root := t.TempDir()
	dmi := filepath.Join(root, "sys", "class", "dmi", "id")
	g.Expect(os.MkdirAll(dmi, 0o755)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(dmi, "product_uuid"), []byte(uuid+"\n"), 0o644)).To(Succeed())
	t.Setenv("GHW_CHROOT", root)

	scheme := runtime.NewScheme()
	g.Expect(elementalv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	registration := &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{Name: "registration", Namespace: namespace},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineName: name,
			Config: elementalv1.Config{Elemental: elementalv1.Elemental{Install: elementalv1.Install{
				Device:          gatedInstallDevice,
				DeviceSelection: elementalv1.DeviceSelectionInventory,
			}}},
		},
		Status: elementalv1.MachineRegistrationStatus{
			RegistrationToken: token,
			RegistrationURL:   "https://example.com/elemental/registration/" + token,
			ServiceAccountRef: &corev1.ObjectReference{Name: "registration", Namespace: namespace},
			Conditions:        []metav1.Condition{{Type: elementalv1.ReadyCondition, Status: metav1.ConditionTrue, Reason: elementalv1.SuccessfullyCreatedReason}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&elementalv1.MachineInventory{}).
		WithObjects(
			registration,
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "registration", Namespace: namespace}},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "registration" + elementalv1.SASecretSuffix, Namespace: namespace},
				Type:       corev1.SecretTypeServiceAccountToken,
				Data:       map[string][]byte{"token": []byte("system-agent-token")},
			},
		).
		WithInterceptorFuncs(interceptor.Funcs{
			// An API server stamps these; the fake client does not, and the
			// registration server tells new inventories by them.
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetCreationTimestamp(metav1.Now())
				obj.SetUID(types.UID("uid-" + obj.GetName()))
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	server := NewWithOptions(context.Background(), cl, Options{ServerURL: "https://example.com", EventRecorder: record.NewFakeRecorder(100)})
	ws := httptest.NewServer(server)
	t.Cleanup(ws.Close)

	inventory := func() *elementalv1.MachineInventory {
		t.Helper()
		found := &elementalv1.MachineInventory{}
		NewWithT(t).Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, found)).To(Succeed())
		return found
	}
	selectDisk := func(id string) {
		t.Helper()
		found := inventory()
		found.Annotations[elementalv1.InstallTargetDeviceAnnotation] = id
		NewWithT(t).Expect(cl.Update(context.Background(), found)).To(Succeed())
	}

	reg := elementalv1.Registration{URL: ws.URL + "/elemental/registration/" + token, Auth: "sys-uuid", NoSMBIOS: true}
	registerClient := register.NewClient()
	state := &register.State{}
	session := liveSession("boot-1")
	// One round of the install loop of a host that was never registered.
	liveRound := func() ([]byte, error) {
		state.InitialRegistration = time.Time{}
		return registerClient.RegisterForInstall(reg, nil, state, session)
	}
	held := func(err error) register.InstallHold {
		t.Helper()
		var hold *register.InstallHoldError
		NewWithT(t).Expect(errors.As(err, &hold)).To(BeTrue(), fmt.Sprintf("want an install hold, got %v", err))
		return hold.Hold
	}
	released := func(data []byte, err error) *elementalv1.Config {
		t.Helper()
		g := NewWithT(t)
		g.Expect(err).ToNot(HaveOccurred())
		config := &elementalv1.Config{}
		g.Expect(yaml.Unmarshal(data, config)).To(Succeed())
		return config
	}

	// The host waits; its disks and registration IP are on the inventory.
	_, err := liveRound()
	hold := held(err)
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	g.Expect(hold.RetryAfterSeconds).To(Equal(register.DefaultInstallHoldSeconds))
	stored := inventory()
	g.Expect(stored.Spec.MachineHash).To(Equal(fmt.Sprintf("%x", sha256.Sum256([]byte(uuid)))))
	g.Expect(stored.Labels).To(HaveKeyWithValue(elementalv1.RegistrationIPLabel, "127.0.0.1"))
	g.Expect(stored.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
	g.Expect(stored.Status.Install.BootID).To(Equal("boot-1"))
	g.Expect(stored.Status.Install.Candidates).To(Equal(session.Candidates))
	g.Expect(stored.Status.Install.StaleSystemDisks).To(Equal(session.StaleSystemDisks))

	// What the machine inventory controller does for a new inventory.
	meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
		Type: elementalv1.NetworkConfigReady, Status: metav1.ConditionTrue, Reason: elementalv1.ReconcilingNetworkConfig,
	})
	g.Expect(cl.Status().Update(context.Background(), stored)).To(Succeed())

	// A person picks a disk: the next round releases the install to it.
	selectDisk(diskA)
	config := released(liveRound())
	g.Expect(config.Elemental.Install.Device).To(BeEmpty())
	g.Expect(config.Elemental.InstallTarget).ToNot(BeNil())
	g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
	g.Expect(config.Elemental.InstallTarget.WipeDeviceIDs).To(HaveValue(Equal([]string{diskB})))
	g.Expect(inventory().Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalling))

	// The install fails; the host says so on a session of its own.
	g.Expect(registerClient.ReportInstall(reg, nil, state, register.InstallReport{
		BootID: "boot-1", Phase: elementalv1.InstallPhaseFailed, TargetDeviceID: diskA,
		Reason: elementalv1.WipeFailedReason, Message: diskB + " still carries COS_STATE after the wipe",
	})).To(Succeed())
	stored = inventory()
	g.Expect(stored.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseFailed))
	g.Expect(meta.FindStatusCondition(stored.Status.Conditions, elementalv1.StaleSystemDisksCondition)).To(
		HaveField("Reason", elementalv1.WipeFailedReason))

	// elemental-register starts over in the same boot and waits again.
	_, err = liveRound()
	g.Expect(held(err).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	g.Expect(inventory().Status.Install.Message).To(ContainSubstring("the last install attempt in this live boot failed"))
	selectDisk(diskA)
	config = released(liveRound())
	g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))

	// This time the install completes and the host says so before it
	// reboots; the installation media stays attached and it comes back in
	// a new live boot. It waits there, and nothing says the install failed.
	g.Expect(registerClient.ReportInstall(reg, nil, state, register.InstallReport{
		BootID: "boot-1", Phase: elementalv1.InstallPhaseInstalled, TargetDeviceID: diskA,
	})).To(Succeed())
	g.Expect(inventory().Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalled))
	registered := state.InitialRegistration
	session = liveSession("boot-2")
	_, err = liveRound()
	g.Expect(held(err).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	g.Expect(inventory().Status.Install.Message).To(HavePrefix("the host booted the live ISO again after the install to " + diskA + " completed"))
	state.InitialRegistration = registered

	// Booted from its disk, the installed system registers without a live
	// session: the wait is over.
	config = released(registerClient.Register(reg, nil, state))
	g.Expect(config.Elemental.InstallTarget).To(BeNil())
	g.Expect(config.Elemental.Install.Device).To(Equal(installTargetRequiresSelection))
	stored = inventory()
	g.Expect(stored.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalled))
	g.Expect(stored.Status.Install.TargetDeviceID).To(Equal(diskA))
}
