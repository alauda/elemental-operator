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
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/register"
)

// Disks of the live session fixture: A and B can be the install target, B
// carries an old system, C is a USB stick.
const (
	diskA = "wwn:5000c500a1b2c3d4"
	diskB = "wwn:5000c500ffff0001"
	diskC = "serial:4c530001230904110463"
)

const gatedInstallDevice = "/dev/elemental-install-target"

func liveSession(bootID string) *register.LiveInstallSession {
	return &register.LiveInstallSession{
		BootID:          bootID,
		Installer:       elementalv1.InstallerToolkit,
		RegisterVersion: "v1.9.1-alauda",
		Candidates: []elementalv1.InstallCandidate{
			{ID: diskA, Path: "/dev/sda", SizeBytes: 480103981056, Model: "SSDSC2KB480G8", Serial: "PHYF0001", WWN: "0x5000c500a1b2c3d4", Transport: "sata", Eligible: true},
			{ID: diskB, Path: "/dev/sdb", SizeBytes: 960197124096, Model: "SSDSC2KB960G8", Serial: "PHYF0002", WWN: "0x5000c500ffff0001", Transport: "sata", StaleSystem: true, Eligible: true},
			{ID: diskC, Path: "/dev/sdc", SizeBytes: 32010928128, Model: "Ultra USB 3.0", Transport: "usb", Eligible: false, Reasons: []string{"removable"}},
		},
		StaleSystemDisks: []elementalv1.StaleSystemDisk{
			{ID: diskB, Evidence: []string{"COS_STATE", "PARTLABEL=persistent"}, Action: elementalv1.StaleSystemDiskWipe},
		},
	}
}

// liveInventory is an inventory the provider did not allocate, whose network
// configuration is ready.
func liveInventory(name string) *elementalv1.MachineInventory {
	return &elementalv1.MachineInventory{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         identityTestNamespace,
			UID:               types.UID("uid-" + name),
			CreationTimestamp: metav1.Now(),
			Labels:            map[string]string{"pool.baremetal.alauda.io/eligible": "true"},
			Annotations:       map[string]string{elementalv1.MachineInventoryAuthAnnotation: "tpm"},
		},
		Spec: elementalv1.MachineInventorySpec{TPMHash: "hash-" + name},
		Status: elementalv1.MachineInventoryStatus{Conditions: []metav1.Condition{{
			Type:               elementalv1.NetworkConfigReady,
			Status:             metav1.ConditionTrue,
			Reason:             elementalv1.ReconcilingNetworkConfig,
			LastTransitionTime: metav1.Now(),
		}}},
	}
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// installTestEnv registers live hosts with a fake API server that has the
// MachineInventory status subresource, and a clock the test moves.
type installTestEnv struct {
	*identityTestEnv
	clock *testClock
}

type installEnvOptions struct {
	// legacy leaves install.device-selection unset.
	legacy       bool
	registration func(*elementalv1.MachineRegistration)
	funcs        interceptor.Funcs
}

func newInstallTestEnv(t *testing.T, opts installEnvOptions, objects ...client.Object) *installTestEnv {
	t.Helper()
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(elementalv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	selection := elementalv1.DeviceSelectionInventory
	if opts.legacy {
		selection = ""
	}
	registration := &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{Name: "registration", Namespace: identityTestNamespace},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineName:                 "node-${System Information/UUID}",
			MachineInventoryAnnotations: map[string]string{},
			Config: elementalv1.Config{Elemental: elementalv1.Elemental{Install: elementalv1.Install{
				Device: gatedInstallDevice,
				DeviceSelector: elementalv1.DeviceSelector{{
					Key: elementalv1.DeviceSelectorKeySize, Operator: elementalv1.DeviceSelectorOpGt, Values: []string{"100Gi"},
				}},
				DeviceSelection: selection,
			}}},
		},
		Status: elementalv1.MachineRegistrationStatus{
			RegistrationToken: identityTestToken,
			RegistrationURL:   "https://example.com/elemental/registration/" + identityTestToken,
			ServiceAccountRef: &corev1.ObjectReference{Name: "registration", Namespace: identityTestNamespace},
			Conditions:        []metav1.Condition{{Type: elementalv1.ReadyCondition, Status: metav1.ConditionTrue, Reason: elementalv1.SuccessfullyCreatedReason}},
		},
	}
	if opts.registration != nil {
		opts.registration(registration)
	}
	objects = append(objects,
		registration,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "registration", Namespace: identityTestNamespace}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "registration" + elementalv1.SASecretSuffix, Namespace: identityTestNamespace},
			Type:       corev1.SecretTypeServiceAccountToken,
			Data:       map[string][]byte{"token": []byte("system-agent-token")},
		},
	)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&elementalv1.MachineInventory{}).
		WithObjects(objects...).
		WithInterceptorFuncs(opts.funcs).
		Build()
	recorder := record.NewFakeRecorder(100)
	clock := &testClock{now: time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)}
	server := &InventoryServer{
		Context:                context.Background(),
		Client:                 cl,
		ServerURL:              "https://example.com",
		AgentTLSMode:           AgentTLSModeStrict,
		SystemAgentClusterName: DefaultSystemAgentClusterName,
		EventRecorder:          recorder,
		authenticators:         []authenticator{&hashAuth{Client: cl}},
		clock:                  clock.Now,
	}
	env := &installTestEnv{
		identityTestEnv: &identityTestEnv{t: t, server: server, recorder: recorder, ws: httptest.NewServer(server)},
		clock:           clock,
	}
	t.Cleanup(env.ws.Close)
	return env
}

// liveHost is one registration of a host.
type liveHost struct {
	auth         string               // "tpm:<hash>"
	protoVersion register.MessageType // register.MsgLast when unset
	annotations  map[string]string
	session      *register.LiveInstallSession
	// update sends MsgUpdate, as a host with registration state does.
	update bool
}

// dial authenticates and negotiates the protocol version.
func (e *installTestEnv) dial(auth string, protoVersion register.MessageType) *websocket.Conn {
	e.t.Helper()
	g := NewWithT(e.t)
	if protoVersion == register.MsgUndefined {
		protoVersion = register.MsgLast
	}
	header := http.Header{}
	header.Add("Authorization", "Bearer FAKE"+auth)
	url := "ws" + strings.TrimPrefix(e.ws.URL, "http") + "/elemental/registration/" + identityTestToken
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	g.Expect(err).ToNot(HaveOccurred())
	reply, _, err := register.ReadMessage(ws)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(reply).To(Equal(register.MsgReady))
	g.Expect(register.WriteMessage(ws, register.MsgVersion, []byte{byte(protoVersion)})).To(Succeed())
	reply, _, err = register.ReadMessage(ws)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(reply).To(Equal(register.MsgVersion))
	return ws
}

func exchange(t *testing.T, ws *websocket.Conn, msgType register.MessageType, data []byte) (register.MessageType, []byte) {
	t.Helper()
	g := NewWithT(t)
	g.Expect(register.WriteMessage(ws, msgType, data)).To(Succeed())
	reply, payload, err := register.ReadMessage(ws)
	g.Expect(err).ToNot(HaveOccurred())
	return reply, payload
}

// registerLive runs a registration and returns the reply to MsgGet.
func (e *installTestEnv) registerLive(h liveHost) (register.MessageType, []byte) {
	e.t.Helper()
	g := NewWithT(e.t)
	ws := e.dial(h.auth, h.protoVersion)
	defer ws.Close()
	if h.annotations != nil {
		data, _ := json.Marshal(h.annotations)
		reply, _ := exchange(e.t, ws, register.MsgAnnotations, data)
		g.Expect(reply).To(Equal(register.MsgReady))
	}
	if h.update {
		reply, payload := exchange(e.t, ws, register.MsgUpdate, nil)
		g.Expect(reply).To(Equal(register.MsgReady), string(payload))
	}
	if h.session != nil {
		data, _ := json.Marshal(h.session)
		reply, payload := exchange(e.t, ws, register.MsgLiveInstallSession, data)
		if reply != register.MsgReady {
			return reply, payload
		}
	}
	return exchange(e.t, ws, register.MsgGet, nil)
}

// hold runs a registration the server must hold and returns the hold.
func (e *installTestEnv) hold(h liveHost) register.InstallHold {
	e.t.Helper()
	g := NewWithT(e.t)
	reply, data := e.registerLive(h)
	g.Expect(reply).To(Equal(register.MsgInstallHold), string(data))
	hold := register.InstallHold{}
	g.Expect(json.Unmarshal(data, &hold)).To(Succeed())
	return hold
}

// config runs a registration the server must answer with a configuration.
func (e *installTestEnv) config(h liveHost) (*elementalv1.Config, string) {
	e.t.Helper()
	g := NewWithT(e.t)
	reply, data := e.registerLive(h)
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	config := &elementalv1.Config{}
	g.Expect(yaml.Unmarshal(data, config)).To(Succeed())
	return config, string(data)
}

// update changes the stored inventory the way a person would.
func (e *installTestEnv) update(name string, change func(*elementalv1.MachineInventory)) {
	e.t.Helper()
	inventory := e.inventory(name)
	change(inventory)
	NewWithT(e.t).Expect(e.server.Update(context.Background(), inventory)).To(Succeed())
}

func (e *installTestEnv) selectDisk(name, id string) {
	e.update(name, func(inventory *elementalv1.MachineInventory) {
		inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation] = id
	})
}

func condition(inventory *elementalv1.MachineInventory, conditionType string) metav1.Condition {
	found := meta.FindStatusCondition(inventory.Status.Conditions, conditionType)
	if found == nil {
		return metav1.Condition{}
	}
	return *found
}

func haveCondition(status metav1.ConditionStatus, reason string) OmegaMatcher {
	return SatisfyAll(
		WithTransform(func(c metav1.Condition) metav1.ConditionStatus { return c.Status }, Equal(status)),
		WithTransform(func(c metav1.Condition) string { return c.Reason }, Equal(reason)),
	)
}

var hostA = liveHost{auth: "tpm:hash-node-a"}

func (h liveHost) in(bootID string) liveHost {
	h.session = liveSession(bootID)
	return h
}

func (h liveHost) with(session *register.LiveInstallSession) liveHost {
	h.session = session
	return h
}

func TestLiveInstallNewBootDiscardsEarlierSelection(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
	// Picked before the host booted the live ISO, e.g. left from an earlier
	// install: it must not release this boot.
	env.selectDisk("node-a", diskA)

	hold := env.hold(hostA.in("boot-1"))
	g.Expect(hold.Reason).To(Equal(elementalv1.SelectionDiscardedReason))
	g.Expect(hold.RetryAfterSeconds).To(Equal(register.DefaultInstallHoldSeconds))
	g.Expect(hold.Message).To(ContainSubstring("set before live boot boot-1 registered"))

	inventory := env.inventory("node-a")
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))
	install := inventory.Status.Install
	g.Expect(install).ToNot(BeNil())
	g.Expect(install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
	g.Expect(install.BootID).To(Equal("boot-1"))
	g.Expect(install.Installer).To(Equal(elementalv1.InstallerToolkit))
	g.Expect(install.LastSeenAt.Time.Equal(env.clock.Now())).To(BeTrue(), install.LastSeenAt.String())
	g.Expect(install.TargetDeviceID).To(BeEmpty())
	g.Expect(install.Candidates).To(Equal(liveSession("boot-1").Candidates))
	g.Expect(install.StaleSystemDisks).To(Equal(liveSession("boot-1").StaleSystemDisks))
	g.Expect(condition(inventory, elementalv1.InstallTargetReadyCondition)).To(haveCondition(metav1.ConditionFalse, elementalv1.SelectionDiscardedReason))
	stale := condition(inventory, elementalv1.StaleSystemDisksCondition)
	g.Expect(stale).To(haveCondition(metav1.ConditionFalse, elementalv1.DetectedReason))
	g.Expect(stale.Message).To(ContainSubstring(diskB))
	// The controller's conditions stay.
	g.Expect(condition(inventory, elementalv1.NetworkConfigReady)).To(haveCondition(metav1.ConditionTrue, elementalv1.ReconcilingNetworkConfig))
	g.Expect(env.events()).To(ConsistOf(HavePrefix("Warning SelectionDiscarded")))
}

func TestLiveInstallHoldsUntilADiskIsPicked(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))

	hold := env.hold(hostA.in("boot-1"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))

	seen := env.clock.Advance(17 * time.Second)
	hold = env.hold(liveHost{auth: hostA.auth, annotations: map[string]string{"registration-ip": "10.0.0.21"}}.in("boot-1"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	g.Expect(hold.RetryAfterSeconds).To(Equal(register.DefaultInstallHoldSeconds))
	g.Expect(hold.Message).To(ContainSubstring("2 of 3 disks"))
	g.Expect(hold.Message).To(ContainSubstring("kubectl -n fleet-default annotate machineinventory node-a baremetal.alauda.io/install-target-device=<id>"))

	inventory := env.inventory("node-a")
	g.Expect(inventory.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
	g.Expect(inventory.Status.Install.LastSeenAt.Time.Equal(seen)).To(BeTrue(), inventory.Status.Install.LastSeenAt.String())
	ready := condition(inventory, elementalv1.InstallTargetReadyCondition)
	g.Expect(ready).To(haveCondition(metav1.ConditionFalse, elementalv1.AwaitingSelectionReason))
	g.Expect(ready.Message).To(Equal(hold.Message))
	g.Expect(inventory.Labels).To(HaveKeyWithValue(elementalv1.RegistrationIPLabel, "10.0.0.21"))
	g.Expect(env.events()).To(BeEmpty())
}

func TestLiveInstallReleasesThePickedDisk(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
	env.hold(hostA.in("boot-1"))
	env.selectDisk("node-a", " "+diskA+" ")
	env.clock.Advance(15 * time.Second)

	config, raw := env.config(hostA.in("boot-1"))
	g.Expect(config.Elemental.Install.Device).To(BeEmpty())
	g.Expect(config.Elemental.Install.DeviceSelector).To(BeNil())
	g.Expect(config.Elemental.Install.DeviceSelection).To(Equal(elementalv1.DeviceSelectionInventory))
	target := config.Elemental.InstallTarget
	g.Expect(target).ToNot(BeNil(), raw)
	g.Expect(target.DeviceID).To(Equal(diskA))
	g.Expect(target.Expect).To(Equal(elementalv1.InstallTargetExpect{SizeBytes: 480103981056, WWN: "0x5000c500a1b2c3d4", Serial: "PHYF0001", Model: "SSDSC2KB480G8"}))
	g.Expect(target.WipeDeviceIDs).ToNot(BeNil(), raw)
	g.Expect(*target.WipeDeviceIDs).To(Equal([]string{diskB}))
	g.Expect(config.Elemental.SystemAgent.Token).To(Equal("system-agent-token"))

	inventory := env.inventory("node-a")
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))
	install := inventory.Status.Install
	g.Expect(install.Phase).To(Equal(elementalv1.InstallPhaseInstalling))
	g.Expect(install.TargetDeviceID).To(Equal(diskA))
	g.Expect(install.StaleSystemDisks).To(ConsistOf(HaveField("Action", elementalv1.StaleSystemDiskWipe)))
	ready := condition(inventory, elementalv1.InstallTargetReadyCondition)
	g.Expect(ready).To(haveCondition(metav1.ConditionTrue, elementalv1.ReleasedReason))
	g.Expect(ready.Message).To(ContainSubstring(diskA))
	g.Expect(condition(inventory, elementalv1.StaleSystemDisksCondition)).To(haveCondition(metav1.ConditionFalse, elementalv1.DetectedReason))
	g.Expect(env.events()).To(ConsistOf(SatisfyAll(HavePrefix("Normal InstallReleased"), ContainSubstring(diskA), ContainSubstring("wipes the stale system disks "+diskB))))

	// The selection is gone: the next registration of the boot does not
	// release again.
	hold := env.hold(hostA.in("boot-1"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	g.Expect(env.inventory("node-a").Status.Install.Message).To(Equal("the install released to " + diskA + " in this live boot did not complete"))
}

// Without the provider's webhook a hand-written ID reaches the server as
// typed; it matches the candidate the host reported in canonical form.
func TestLiveInstallMatchesAHandWrittenID(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
	env.hold(hostA.in("boot-1"))
	env.selectDisk("node-a", "WWN:0x5000C500A1B2C3D4")

	config, raw := env.config(hostA.in("boot-1"))
	g.Expect(config.Elemental.InstallTarget).ToNot(BeNil(), raw)
	g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
	g.Expect(env.inventory("node-a").Status.Install.TargetDeviceID).To(Equal(diskA))
}

func TestLiveInstallReleaseApprovesNoWipeBeyondTheList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale []elementalv1.StaleSystemDisk
		pick  string
	}{
		{name: "no stale system disk", pick: diskA},
		{name: "the target is the stale system disk", pick: diskB, stale: []elementalv1.StaleSystemDisk{
			{ID: diskB, Evidence: []string{"COS_STATE"}, Action: elementalv1.StaleSystemDiskWipe},
		}},
		{name: "the target is a blocked stale system disk", pick: diskB, stale: []elementalv1.StaleSystemDisk{
			{ID: diskB, Evidence: []string{"COS_STATE"}, Action: elementalv1.StaleSystemDiskBlocked, Reason: "other partitions"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
			session := liveSession("boot-1")
			session.StaleSystemDisks = tc.stale
			env.hold(hostA.with(session))
			env.selectDisk("node-a", tc.pick)

			config, raw := env.config(hostA.with(session))
			g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(tc.pick))
			// An empty list, not none: no other stale system disk may be wiped.
			g.Expect(raw).To(ContainSubstring("wipe-device-ids: []"))
			g.Expect(config.Elemental.InstallTarget.WipeDeviceIDs).To(HaveValue(BeEmpty()))

			inventory := env.inventory("node-a")
			for _, disk := range inventory.Status.Install.StaleSystemDisks {
				g.Expect(disk.Action).To(Equal(elementalv1.StaleSystemDiskTarget))
			}
			g.Expect(condition(inventory, elementalv1.StaleSystemDisksCondition)).To(haveCondition(metav1.ConditionTrue, elementalv1.NoneDetectedReason))
		})
	}
}

func TestLiveInstallDiscardsInvalidSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pick      string
		inventory func(*elementalv1.MachineInventory)
		session   func(*register.LiveInstallSession)
		reason    string
		message   string
	}{
		{name: "not reported", pick: "wwn:5000c500deadbeef", reason: elementalv1.TargetNotObservedReason, message: "reported no disk with this ID"},
		{name: "empty", pick: " ", reason: elementalv1.TargetNotObservedReason, message: "empty"},
		{name: "reported twice", pick: diskA, reason: elementalv1.TargetNotObservedReason, message: "reported 2 disks",
			session: func(s *register.LiveInstallSession) { s.Candidates[1].ID = diskA }},
		{name: "ineligible", pick: diskC, reason: elementalv1.TargetIneligibleReason, message: "removable"},
		{name: "no stable ID", pick: "unidentified:sdd", reason: elementalv1.TargetIneligibleReason, message: "no stable ID",
			session: func(s *register.LiveInstallSession) {
				s.Candidates = append(s.Candidates, elementalv1.InstallCandidate{ID: "unidentified:sdd", Path: "/dev/sdd", Eligible: true})
			}},
		{name: "allocated", pick: diskA, reason: elementalv1.TargetIneligibleReason, message: "baremetal.alauda.io/owner-machine=workers-0",
			inventory: func(i *elementalv1.MachineInventory) {
				i.Annotations["baremetal.alauda.io/owner-machine"] = "workers-0"
			}},
		{name: "identity conflict", pick: diskA, reason: elementalv1.IdentityConflictReason, message: elementalv1.IdentityConflictAnnotation,
			inventory: func(i *elementalv1.MachineInventory) {
				i.Annotations[elementalv1.IdentityConflictAnnotation] = "hardware fingerprint does not match the registered host: x"
			}},
		{name: "blocked stale system disk", pick: diskA, reason: elementalv1.BlockedReason, message: diskB + " (mounted at /mnt)",
			session: func(s *register.LiveInstallSession) {
				s.StaleSystemDisks[0].Action, s.StaleSystemDisks[0].Reason = elementalv1.StaleSystemDiskBlocked, "mounted at /mnt"
			}},
		{name: "survey failed", pick: diskA, reason: elementalv1.TargetNotObservedReason, message: "lsblk: exit status 32",
			session: func(s *register.LiveInstallSession) { s.SurveyError = "lsblk: exit status 32" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
			session := liveSession("boot-1")
			if tc.session != nil {
				tc.session(session)
			}
			env.hold(hostA.with(session))
			env.events()
			env.update("node-a", func(inventory *elementalv1.MachineInventory) {
				inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation] = tc.pick
				if tc.inventory != nil {
					tc.inventory(inventory)
				}
			})

			hold := env.hold(hostA.with(session))
			g.Expect(hold.Reason).To(Equal(tc.reason))
			g.Expect(hold.Message).To(HavePrefix("discarded " + elementalv1.InstallTargetDeviceAnnotation))
			g.Expect(hold.Message).To(ContainSubstring(tc.message))

			inventory := env.inventory("node-a")
			g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))
			g.Expect(inventory.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
			ready := condition(inventory, elementalv1.InstallTargetReadyCondition)
			g.Expect(ready).To(haveCondition(metav1.ConditionFalse, tc.reason))
			g.Expect(ready.Message).To(Equal(hold.Message))
			if tc.reason == elementalv1.BlockedReason {
				g.Expect(condition(inventory, elementalv1.StaleSystemDisksCondition)).To(haveCondition(metav1.ConditionFalse, elementalv1.BlockedReason))
			}
			g.Expect(env.events()).To(ConsistOf(HavePrefix("Warning SelectionDiscarded")))
		})
	}
}

// conflictOnce makes the next patch that touches the install-target
// annotation fail with a conflict, as if the inventory changed meanwhile.
func conflictOnce(armed *atomic.Bool) interceptor.Funcs {
	return interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		data, err := patch.Data(obj)
		if err == nil && strings.Contains(string(data), elementalv1.InstallTargetDeviceAnnotation) && armed.CompareAndSwap(true, false) {
			return apierrors.NewConflict(schema.GroupResource{Group: "elemental.cattle.io", Resource: "machineinventories"}, obj.GetName(), errors.New("the object has been modified"))
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}
}

func TestLiveInstallSelectionConsumeConflictHoldsBriefly(t *testing.T) {
	g := NewWithT(t)
	armed := &atomic.Bool{}
	env := newInstallTestEnv(t, installEnvOptions{funcs: conflictOnce(armed)}, liveInventory("node-a"))
	env.hold(hostA.in("boot-1"))
	env.selectDisk("node-a", diskA)

	armed.Store(true)
	hold := env.hold(hostA.in("boot-1"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	g.Expect(hold.RetryAfterSeconds).To(Equal(5))
	g.Expect(hold.Message).To(ContainSubstring("checked again"))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.InstallTargetDeviceAnnotation, diskA))
	g.Expect(inventory.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))

	// The next registration looks at it again and releases.
	config, _ := env.config(hostA.in("boot-1"))
	g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
	g.Expect(env.inventory("node-a").Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))
}

// failStatusPatches makes every status patch fail.
var failStatusPatches = interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return apierrors.NewServiceUnavailable("etcd is down")
}}

func TestLiveInstallStatusFailure(t *testing.T) {
	t.Run("a hold is still sent", func(t *testing.T) {
		g := NewWithT(t)
		env := newInstallTestEnv(t, installEnvOptions{funcs: failStatusPatches}, liveInventory("node-a"))
		g.Expect(env.hold(hostA.in("boot-1")).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
		g.Expect(env.inventory("node-a").Status.Install).To(BeNil())
	})
	t.Run("no release goes out unrecorded", func(t *testing.T) {
		g := NewWithT(t)
		inventory := liveInventory("node-a")
		inventory.Status.Install = &elementalv1.InstallStatus{Phase: elementalv1.InstallPhaseAwaitingTarget, BootID: "boot-1"}
		inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation] = diskA
		env := newInstallTestEnv(t, installEnvOptions{funcs: failStatusPatches}, inventory)
		reply, data := env.registerLive(hostA.in("boot-1"))
		message := errorReply(t, reply, data)
		g.Expect(message.Message).To(ContainSubstring("cannot record the install release"))
	})
}

func TestLiveInstallAfterAnInterruptedInstall(t *testing.T) {
	g := NewWithT(t)
	inventory := liveInventory("node-a")
	inventory.Status.Install = &elementalv1.InstallStatus{Phase: elementalv1.InstallPhaseInstalling, BootID: "boot-1", TargetDeviceID: diskA}
	env := newInstallTestEnv(t, installEnvOptions{}, inventory)

	// Power lost during the install: the host comes back in a new live boot.
	hold := env.hold(hostA.in("boot-2"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	install := env.inventory("node-a").Status.Install
	g.Expect(install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
	g.Expect(install.BootID).To(Equal("boot-2"))
	g.Expect(install.TargetDeviceID).To(BeEmpty())
	g.Expect(install.Message).To(Equal("the install released to " + diskA + " in live boot boot-1 did not complete"))
	g.Expect(env.events()).To(ConsistOf(HavePrefix("Warning InstallInterrupted the install released to " + diskA)))

	// The note stays while the host waits.
	env.hold(hostA.in("boot-2"))
	g.Expect(env.inventory("node-a").Status.Install.Message).To(Equal(install.Message))
}

func TestLiveInstallAfterAFailedAttemptInTheSameBoot(t *testing.T) {
	g := NewWithT(t)
	inventory := liveInventory("node-a")
	inventory.Status.Install = &elementalv1.InstallStatus{Phase: elementalv1.InstallPhaseFailed, BootID: "boot-1", TargetDeviceID: diskA, Message: "grub2-install: no space left on device"}
	env := newInstallTestEnv(t, installEnvOptions{}, inventory)

	hold := env.hold(hostA.in("boot-1"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	install := env.inventory("node-a").Status.Install
	g.Expect(install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
	g.Expect(install.Message).To(Equal("the last install attempt in this live boot failed: grub2-install: no space left on device"))

	// Picked again in the same boot: released again.
	env.selectDisk("node-a", diskA)
	config, _ := env.config(hostA.in("boot-1"))
	g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
	g.Expect(env.inventory("node-a").Status.Install.Message).To(BeEmpty())
}

func TestLiveInstallAlternatingBootsAreAnIdentityConflict(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
	// Two hosts present node-a's identity from the live ISO.
	g.Expect(env.hold(hostA.in("boot-a")).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	env.clock.Advance(8 * time.Second)
	g.Expect(env.hold(hostA.in("boot-b")).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	env.clock.Advance(8 * time.Second)
	hold := env.hold(hostA.in("boot-a"))
	g.Expect(hold.Reason).To(Equal(elementalv1.IdentityConflictReason))
	g.Expect(hold.Message).To(ContainSubstring("live boots boot-a and boot-b take turns"))
	g.Expect(condition(env.inventory("node-a"), elementalv1.InstallTargetReadyCondition)).To(haveCondition(metav1.ConditionFalse, elementalv1.IdentityConflictReason))
	g.Expect(env.events()).To(ConsistOf(HavePrefix("Warning IdentityConflict live boots boot-a and boot-b take turns")))

	// A selection made meanwhile does not release either host, even when
	// the same boot registers twice in a row.
	env.selectDisk("node-a", diskA)
	env.clock.Advance(8 * time.Second)
	hold = env.hold(hostA.in("boot-a"))
	g.Expect(hold.Reason).To(Equal(elementalv1.IdentityConflictReason))
	g.Expect(env.inventory("node-a").Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))

	// Once the other host is gone for good, the remaining one can be picked.
	env.clock.Advance(liveBootAlternation)
	g.Expect(env.hold(hostA.in("boot-a")).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	env.selectDisk("node-a", diskA)
	config, _ := env.config(hostA.in("boot-a"))
	g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
}

func TestLiveBootTrackerForgetsQuietInventories(t *testing.T) {
	g := NewWithT(t)
	tracker := &liveBootTracker{}
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	g.Expect(tracker.observe("uid-a", "boot-1", now)).To(Equal(liveBootObservation{}))
	g.Expect(tracker.observe("uid-a", "boot-2", now.Add(time.Second))).To(Equal(liveBootObservation{other: "boot-1"}))
	// A reboot never comes back to an earlier boot; long after, it is
	// forgotten anyway.
	g.Expect(tracker.observe("uid-b", "boot-9", now.Add(liveBootMemory+2*time.Second)).alternating).To(BeFalse())
	g.Expect(tracker.boots).ToNot(HaveKey(types.UID("uid-a")))
	g.Expect(tracker.observe("", "boot-1", now)).To(Equal(liveBootObservation{}))
}

func TestLiveInstallLegacyModeInstallsRightAway(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{legacy: true}, liveInventory("node-a"))

	config, _ := env.config(hostA.in("boot-1"))
	g.Expect(config.Elemental.Install.Device).To(Equal(gatedInstallDevice))
	g.Expect(config.Elemental.Install.DeviceSelector).To(HaveLen(1))
	g.Expect(config.Elemental.InstallTarget).To(BeNil())

	inventory := env.inventory("node-a")
	install := inventory.Status.Install
	g.Expect(install.Phase).To(Equal(elementalv1.InstallPhaseInstalling))
	g.Expect(install.BootID).To(Equal("boot-1"))
	g.Expect(install.TargetDeviceID).To(BeEmpty())
	g.Expect(install.Candidates).To(HaveLen(3))
	g.Expect(meta.FindStatusCondition(inventory.Status.Conditions, elementalv1.InstallTargetReadyCondition)).To(BeNil())
	g.Expect(condition(inventory, elementalv1.StaleSystemDisksCondition)).To(haveCondition(metav1.ConditionFalse, elementalv1.DetectedReason))
}

func TestLiveInstallClientThatCannotBeHeld(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
	// Knows the session message but not the hold: it gets no usable disk.
	config, _ := env.config(liveHost{auth: hostA.auth, protoVersion: register.MsgLiveInstallSession}.in("boot-1"))
	g.Expect(config.Elemental.Install.Device).To(Equal(installTargetRequiresSelection))
	g.Expect(config.Elemental.Install.DeviceSelector).To(BeNil())
	g.Expect(config.Elemental.InstallTarget).To(BeNil())
	g.Expect(env.inventory("node-a").Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalling))
}

func TestInventoryModeWithoutSessionFailsClosed(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
	// An installed system, or a live ISO from before sessions.
	config, raw := env.config(hostA)
	g.Expect(config.Elemental.Install.Device).To(Equal(installTargetRequiresSelection))
	g.Expect(config.Elemental.Install.DeviceSelector).To(BeNil())
	g.Expect(config.Elemental.InstallTarget).To(BeNil())
	g.Expect(raw).ToNot(ContainSubstring("install-target:"))
	g.Expect(env.inventory("node-a").Status.Install).To(BeNil())
}

func TestInstalledSystemSettlesTheInstall(t *testing.T) {
	for _, phase := range []elementalv1.InstallPhase{elementalv1.InstallPhaseInstalling, elementalv1.InstallPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			g := NewWithT(t)
			inventory := liveInventory("node-a")
			session := liveSession("boot-1")
			inventory.Status.Install = &elementalv1.InstallStatus{
				Phase: phase, BootID: "boot-1", TargetDeviceID: diskA, Message: "earlier",
				Candidates: session.Candidates, StaleSystemDisks: session.StaleSystemDisks,
			}
			// Set again while the host was installing.
			inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation] = diskA
			env := newInstallTestEnv(t, installEnvOptions{}, inventory)

			config, _ := env.config(hostA)
			g.Expect(config.Elemental.Install.Device).To(Equal(installTargetRequiresSelection))
			stored := env.inventory("node-a")
			g.Expect(stored.Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))
			g.Expect(stored.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalled))
			g.Expect(stored.Status.Install.Message).To(BeEmpty())
			g.Expect(stored.Status.Install.BootID).To(Equal("boot-1"))
			g.Expect(stored.Status.Install.TargetDeviceID).To(Equal(diskA))
			stale := condition(stored, elementalv1.StaleSystemDisksCondition)
			g.Expect(stale).To(haveCondition(metav1.ConditionTrue, elementalv1.WipedReason))
			g.Expect(stale.Message).To(ContainSubstring(diskB))
		})
	}
	t.Run("nothing was wiped", func(t *testing.T) {
		g := NewWithT(t)
		inventory := liveInventory("node-a")
		inventory.Status.Install = &elementalv1.InstallStatus{Phase: elementalv1.InstallPhaseInstalling, BootID: "boot-1", TargetDeviceID: diskA}
		env := newInstallTestEnv(t, installEnvOptions{legacy: true}, inventory)
		config, _ := env.config(hostA)
		g.Expect(config.Elemental.Install.Device).To(Equal(gatedInstallDevice))
		stored := env.inventory("node-a")
		g.Expect(stored.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalled))
		g.Expect(condition(stored, elementalv1.StaleSystemDisksCondition)).To(haveCondition(metav1.ConditionTrue, elementalv1.NoneDetectedReason))
	})
	// The installation media stayed attached: the host went back to the
	// live ISO after the install and waited there until someone booted it
	// from its disk. Its installed system registering ends the wait.
	t.Run("an installed system ends a wait for a target", func(t *testing.T) {
		g := NewWithT(t)
		inventory := liveInventory("node-a")
		session := liveSession("boot-2")
		inventory.Status.Install = &elementalv1.InstallStatus{
			Phase: elementalv1.InstallPhaseAwaitingTarget, BootID: "boot-2", Message: "waiting",
			Candidates: session.Candidates, StaleSystemDisks: session.StaleSystemDisks,
		}
		inventory.Status.Conditions = append(inventory.Status.Conditions,
			metav1.Condition{Type: elementalv1.InstallTargetReadyCondition, Status: metav1.ConditionFalse, Reason: elementalv1.AwaitingSelectionReason, LastTransitionTime: metav1.Now()},
			metav1.Condition{Type: elementalv1.StaleSystemDisksCondition, Status: metav1.ConditionFalse, Reason: elementalv1.DetectedReason, LastTransitionTime: metav1.Now()})
		env := newInstallTestEnv(t, installEnvOptions{}, inventory)

		env.config(liveHost{auth: hostA.auth, update: true})
		stored := env.inventory("node-a")
		g.Expect(stored.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalled))
		g.Expect(stored.Status.Install.Message).To(BeEmpty())
		g.Expect(meta.FindStatusCondition(stored.Status.Conditions, elementalv1.InstallTargetReadyCondition)).To(BeNil())
		g.Expect(meta.FindStatusCondition(stored.Status.Conditions, elementalv1.StaleSystemDisksCondition)).To(BeNil())
	})
	t.Run("an install awaiting a target is left alone", func(t *testing.T) {
		g := NewWithT(t)
		inventory := liveInventory("node-a")
		inventory.Status.Install = &elementalv1.InstallStatus{Phase: elementalv1.InstallPhaseAwaitingTarget, BootID: "boot-1"}
		inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation] = diskA
		env := newInstallTestEnv(t, installEnvOptions{}, inventory)
		env.config(hostA)
		stored := env.inventory("node-a")
		g.Expect(stored.Status.Install).To(Equal(inventory.Status.Install))
		g.Expect(stored.Annotations).To(HaveKeyWithValue(elementalv1.InstallTargetDeviceAnnotation, diskA))
	})
}

func TestLegacyRegistrationWithoutSessionIsUnchanged(t *testing.T) {
	for _, protoVersion := range []register.MessageType{register.MsgObserveStorage, register.MsgLast} {
		t.Run(protoVersion.String(), func(t *testing.T) {
			g := NewWithT(t)
			env := newInstallTestEnv(t, installEnvOptions{legacy: true}, liveInventory("node-a"))
			config, raw := env.config(liveHost{auth: hostA.auth, protoVersion: protoVersion})
			g.Expect(config.Elemental.Install.Device).To(Equal(gatedInstallDevice))
			g.Expect(config.Elemental.Install.DeviceSelector).To(HaveLen(1))
			g.Expect(raw).ToNot(ContainSubstring("install-target:"))
			inventory := env.inventory("node-a")
			g.Expect(inventory.Status.Install).To(BeNil())
			g.Expect(meta.FindStatusCondition(inventory.Status.Conditions, elementalv1.StaleSystemDisksCondition)).To(BeNil())
		})
	}
}

func TestLiveInstallSelectionComesFromTheInventoryOnly(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{registration: func(r *elementalv1.MachineRegistration) {
		r.Spec.MachineInventoryAnnotations[elementalv1.InstallTargetDeviceAnnotation] = diskA
	}}, liveInventory("node-a"))
	env.hold(hostA.in("boot-1"))
	// A template cannot pick a disk for every host of the registration.
	g.Expect(env.hold(hostA.in("boot-1")).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	g.Expect(env.inventory("node-a").Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))
}

func TestLiveInstallOnANewInventory(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{})
	hold := env.hold(liveHost{auth: "tpm:hash-new"}.in("boot-1"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	inventories := env.inventories()
	g.Expect(inventories).To(HaveLen(1))
	g.Expect(inventories[0].Status.Install).ToNot(BeNil())
	g.Expect(inventories[0].Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
	g.Expect(hold.Message).To(ContainSubstring("annotate machineinventory " + inventories[0].Name + " "))
}

func TestLiveInstallSessionIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		message string
	}{
		{name: "not JSON", payload: "{", message: "cannot decode live install session"},
		{name: "no boot ID", payload: `{"bootID":"  ","installer":"toolkit"}`, message: "no boot ID"},
		{name: "malformed boot ID", payload: `{"bootID":"boot 1"}`, message: "is malformed"},
		{name: "unknown installer", payload: `{"bootID":"boot-1","installer":"anaconda"}`, message: `unknown installer "anaconda"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
			ws := env.dial(hostA.auth, register.MsgLast)
			defer ws.Close()
			reply, data := exchange(t, ws, register.MsgLiveInstallSession, []byte(tc.payload))
			g.Expect(errorReply(t, reply, data).Message).To(ContainSubstring(tc.message))
			g.Expect(env.inventory("node-a").Status.Install).To(BeNil())
		})
	}
}

func TestDecodeLiveInstallSessionBoundsWhatItStores(t *testing.T) {
	g := NewWithT(t)
	session := liveSession(" boot-1\n")
	session.Installer = ""
	session.Candidates[0].Model = strings.Repeat("é", 300)
	session.Candidates[0].Reasons = make([]string, 40)
	for index := 0; index < 200; index++ {
		session.Candidates = append(session.Candidates, elementalv1.InstallCandidate{ID: "serial:extra"})
	}
	session.StaleSystemDisks = append(session.StaleSystemDisks,
		elementalv1.StaleSystemDisk{ID: "wwn:0x1", Action: elementalv1.StaleSystemDiskTarget},
		elementalv1.StaleSystemDisk{ID: "wwn:0x2", Reason: "busy"},
	)
	data, err := json.Marshal(session)
	g.Expect(err).ToNot(HaveOccurred())

	decoded, err := decodeLiveInstallSession(data)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(decoded.BootID).To(Equal("boot-1"))
	g.Expect(decoded.Installer).To(Equal(elementalv1.InstallerToolkit))
	g.Expect(decoded.Candidates).To(HaveLen(maxInstallDisks))
	g.Expect(len(decoded.Candidates[0].Model)).To(BeNumerically("<=", maxInstallField))
	g.Expect(strings.ToValidUTF8(decoded.Candidates[0].Model, "?")).To(Equal(decoded.Candidates[0].Model))
	g.Expect(decoded.Candidates[0].Reasons).To(HaveLen(maxInstallDiskStrings))
	// Only the host's own Wipe or Blocked verdict counts; anything else
	// never gets approved for wiping.
	g.Expect(decoded.StaleSystemDisks[1]).To(Equal(elementalv1.StaleSystemDisk{ID: "wwn:0x1", Action: elementalv1.StaleSystemDiskBlocked, Reason: `elemental-register reported action "Target"`}))
	g.Expect(decoded.StaleSystemDisks[2]).To(Equal(elementalv1.StaleSystemDisk{ID: "wwn:0x2", Action: elementalv1.StaleSystemDiskBlocked, Reason: `elemental-register reported action "": busy`}))
}

// report sends an install report on a session of its own, the way a
// released host does, and returns the reply.
func (e *installTestEnv) report(auth string, report register.InstallReport) (register.MessageType, []byte) {
	e.t.Helper()
	g := NewWithT(e.t)
	ws := e.dial(auth, register.MsgLast)
	defer ws.Close()
	data, err := json.Marshal(report)
	g.Expect(err).ToNot(HaveOccurred())
	reply, payload := exchange(e.t, ws, register.MsgInstallReport, data)
	if reply == register.MsgReady {
		// The server ends the session instead of waiting for more.
		g.Expect(ws.SetReadDeadline(time.Now().Add(5 * time.Second))).To(Succeed())
		_, _, err := ws.ReadMessage()
		var netErr net.Error
		g.Expect(errors.As(err, &netErr) && netErr.Timeout()).To(BeFalse(), "the server kept the report session open")
	}
	return reply, payload
}

func installingInventory(bootID string) *elementalv1.MachineInventory {
	inventory := liveInventory("node-a")
	session := liveSession(bootID)
	inventory.Status.Install = &elementalv1.InstallStatus{
		Phase: elementalv1.InstallPhaseInstalling, BootID: bootID, TargetDeviceID: diskA,
		Candidates: session.Candidates, StaleSystemDisks: session.StaleSystemDisks,
	}
	return inventory
}

func TestInstallReportOfAFailedInstall(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		condition string
	}{
		{reason: elementalv1.WipeFailedReason, condition: elementalv1.StaleSystemDisksCondition},
		{reason: elementalv1.BlockedReason, condition: elementalv1.StaleSystemDisksCondition},
		{reason: elementalv1.TargetMismatchReason, condition: elementalv1.InstallTargetReadyCondition},
		{reason: elementalv1.TargetNotObservedReason, condition: elementalv1.InstallTargetReadyCondition},
		{reason: "TargetAmbiguous", condition: elementalv1.InstallTargetReadyCondition},
		{reason: "ImagePullFailed"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			g := NewWithT(t)
			env := newInstallTestEnv(t, installEnvOptions{}, installingInventory("boot-1"))
			before := env.inventory("node-a")

			reply, data := env.report(hostA.auth, register.InstallReport{
				BootID: "boot-1", Phase: elementalv1.InstallPhaseFailed, TargetDeviceID: diskA,
				Reason: tc.reason, Message: "something went wrong on " + diskB,
			})
			g.Expect(reply).To(Equal(register.MsgReady), string(data))

			inventory := env.inventory("node-a")
			install := inventory.Status.Install
			g.Expect(install.Phase).To(Equal(elementalv1.InstallPhaseFailed))
			g.Expect(install.Message).To(Equal("something went wrong on " + diskB))
			g.Expect(install.TargetDeviceID).To(Equal(diskA))
			g.Expect(install.BootID).To(Equal("boot-1"))
			if tc.condition != "" {
				reported := condition(inventory, tc.condition)
				g.Expect(reported).To(haveCondition(metav1.ConditionFalse, tc.reason))
				g.Expect(reported.Message).To(Equal(install.Message))
			} else {
				g.Expect(inventory.Status.Conditions).To(Equal(before.Status.Conditions))
			}
			g.Expect(env.events()).To(ConsistOf(SatisfyAll(
				HavePrefix("Warning InstallFailed the install in live boot boot-1 failed ("+tc.reason+")"),
				HaveSuffix("something went wrong on "+diskB),
			)))
		})
	}
}

func TestInstallReportOfARunningInstall(t *testing.T) {
	g := NewWithT(t)
	inventory := installingInventory("boot-1")
	inventory.Status.Install.TargetDeviceID = ""
	env := newInstallTestEnv(t, installEnvOptions{}, inventory)

	reply, data := env.report(hostA.auth, register.InstallReport{BootID: "boot-1", Phase: elementalv1.InstallPhaseInstalling, TargetDeviceID: diskA})
	g.Expect(reply).To(Equal(register.MsgReady), string(data))
	install := env.inventory("node-a").Status.Install
	g.Expect(install.Phase).To(Equal(elementalv1.InstallPhaseInstalling))
	g.Expect(install.TargetDeviceID).To(Equal(diskA))
	g.Expect(env.events()).To(BeEmpty())
}

// A host reports its install complete before it reboots. When the
// installation media stayed attached it comes back in a new live boot: that
// install did complete, so nothing says it was interrupted.
func TestInstallReportOfACompletedInstall(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, installingInventory("boot-1"))

	reply, data := env.report(hostA.auth, register.InstallReport{BootID: "boot-1", Phase: elementalv1.InstallPhaseInstalled, TargetDeviceID: diskA})
	g.Expect(reply).To(Equal(register.MsgReady), string(data))
	stored := env.inventory("node-a")
	g.Expect(stored.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalled))
	g.Expect(stored.Status.Install.TargetDeviceID).To(Equal(diskA))
	g.Expect(condition(stored, elementalv1.StaleSystemDisksCondition)).To(haveCondition(metav1.ConditionTrue, elementalv1.WipedReason))
	g.Expect(env.events()).To(ConsistOf(SatisfyAll(HavePrefix("Normal InstallCompleted"), ContainSubstring(diskA))))

	hold := env.hold(hostA.in("boot-2"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	install := env.inventory("node-a").Status.Install
	g.Expect(install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
	g.Expect(install.Message).To(HavePrefix("the host booted the live ISO again after the install to " + diskA + " completed"))
	// Still the disk the installed system is on, until another install
	// is released.
	g.Expect(install.TargetDeviceID).To(Equal(diskA))
	g.Expect(env.hold(hostA.in("boot-2")).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	g.Expect(env.inventory("node-a").Status.Install.TargetDeviceID).To(Equal(diskA))
	g.Expect(env.events()).To(BeEmpty())
}

func TestInstallReportOfAnotherBootIsIgnored(t *testing.T) {
	for name, inventory := range map[string]*elementalv1.MachineInventory{
		"another boot":     installingInventory("boot-2"),
		"no install known": liveInventory("node-a"),
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			env := newInstallTestEnv(t, installEnvOptions{}, inventory)
			before := env.inventory("node-a")
			reply, data := env.report(hostA.auth, register.InstallReport{BootID: "boot-1", Phase: elementalv1.InstallPhaseFailed, Reason: elementalv1.WipeFailedReason, Message: "late"})
			g.Expect(reply).To(Equal(register.MsgReady), string(data))
			g.Expect(env.inventory("node-a").Status).To(Equal(before.Status))
			g.Expect(env.events()).To(BeEmpty())
		})
	}
}

func TestInstallReportIsAProtocolErrorWhenMalformed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		auth    string
		payload string
		message string
	}{
		{name: "unknown identity", auth: "tpm:hash-new", payload: `{"bootID":"boot-1","phase":"Failed"}`, message: "needs a registered MachineInventory"},
		{name: "not JSON", auth: hostA.auth, payload: "[", message: "cannot decode install report"},
		{name: "unexpected phase", auth: hostA.auth, payload: `{"bootID":"boot-1","phase":"AwaitingTarget"}`, message: `phase "AwaitingTarget"`},
		{name: "no phase", auth: hostA.auth, payload: `{"bootID":"boot-1"}`, message: `phase ""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			env := newInstallTestEnv(t, installEnvOptions{}, installingInventory("boot-1"))
			ws := env.dial(tc.auth, register.MsgLast)
			defer ws.Close()
			reply, data := exchange(t, ws, register.MsgInstallReport, []byte(tc.payload))
			g.Expect(errorReply(t, reply, data).Message).To(ContainSubstring(tc.message))
			g.Expect(env.inventories()).To(HaveLen(1))
			g.Expect(env.inventory("node-a").Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalling))
		})
	}
}

// A host goes through the whole live install: it waits, is released, fails
// to wipe a stale system disk, is released again after a person dealt with
// it, and its installed system registers.
func TestLiveInstallEndToEnd(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))

	g.Expect(env.hold(hostA.in("boot-1")).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	env.selectDisk("node-a", diskA)
	config, _ := env.config(hostA.in("boot-1"))
	g.Expect(*config.Elemental.InstallTarget.WipeDeviceIDs).To(Equal([]string{diskB}))

	reply, _ := env.report(hostA.auth, register.InstallReport{BootID: "boot-1", Phase: elementalv1.InstallPhaseInstalling, TargetDeviceID: diskA})
	g.Expect(reply).To(Equal(register.MsgReady))
	reply, _ = env.report(hostA.auth, register.InstallReport{BootID: "boot-1", Phase: elementalv1.InstallPhaseFailed, TargetDeviceID: diskA,
		Reason: elementalv1.WipeFailedReason, Message: diskB + " still carries COS_STATE after the wipe"})
	g.Expect(reply).To(Equal(register.MsgReady))
	g.Expect(condition(env.inventory("node-a"), elementalv1.StaleSystemDisksCondition)).To(haveCondition(metav1.ConditionFalse, elementalv1.WipeFailedReason))

	// elemental-register starts over in the same boot: the selection was
	// consumed, a person picks again.
	hold := env.hold(hostA.in("boot-1"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	install := env.inventory("node-a").Status.Install
	g.Expect(install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
	g.Expect(install.Message).To(Equal("the last install attempt in this live boot failed: " + diskB + " still carries COS_STATE after the wipe"))
	env.selectDisk("node-a", diskA)
	config, _ = env.config(hostA.in("boot-1"))
	g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))

	// The installed system registers: no live session any more.
	env.config(liveHost{auth: hostA.auth, annotations: map[string]string{"registration-ip": "10.0.0.31"}})
	inventory := env.inventory("node-a")
	g.Expect(inventory.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalled))
	g.Expect(inventory.Status.Install.TargetDeviceID).To(Equal(diskA))
	g.Expect(condition(inventory, elementalv1.InstallTargetReadyCondition)).To(haveCondition(metav1.ConditionTrue, elementalv1.ReleasedReason))
	g.Expect(condition(inventory, elementalv1.StaleSystemDisksCondition)).To(haveCondition(metav1.ConditionTrue, elementalv1.WipedReason))
	g.Expect(inventory.Labels).To(HaveKeyWithValue(elementalv1.RegistrationIPLabel, "10.0.0.31"))
	g.Expect(env.events()).To(ConsistOf(
		HavePrefix("Normal InstallReleased"),
		HavePrefix("Warning InstallFailed"),
		HavePrefix("Normal InstallReleased"),
	))
}
