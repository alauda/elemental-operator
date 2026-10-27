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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/identity"
	"github.com/rancher/elemental-operator/pkg/register"
)

const (
	identityTestNamespace = "fleet-default"
	identityTestToken     = "identity-token"
	uuidA                 = "4C4C4544-0042-3510-8058-B4C04F4D4E32"
	uuidB                 = "4C4C4544-0051-4410-8058-B4C04F4D4E32" // differs from A in the high 64 bits only
)

// hashAuth authenticates "Bearer FAKE<kind>:<hash>" the way the TPM and plain
// authenticators do: cluster-wide lookup of the hash, or a new inventory.
type hashAuth struct {
	client.Client
}

func (a *hashAuth) Authenticate(_ *websocket.Conn, req *http.Request, namespace string) (*elementalv1.MachineInventory, bool, error) {
	kind, hash, _ := strings.Cut(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer FAKE"), ":")
	list := &elementalv1.MachineInventoryList{}
	if err := a.List(context.Background(), list); err != nil {
		return nil, false, err
	}
	for index := range list.Items {
		m := list.Items[index]
		if (kind == "tpm" && m.Spec.TPMHash == hash) || (kind == "plain" && m.Spec.MachineHash == hash) {
			found := m.DeepCopy()
			if found.CreationTimestamp.IsZero() {
				// The fake client does not set it; an API server always does.
				found.CreationTimestamp = metav1.Now()
			}
			return found, false, nil
		}
	}
	inventory := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{Namespace: namespace}}
	if kind == "tpm" {
		inventory.Spec.TPMHash = hash
	} else {
		inventory.Spec.MachineHash = hash
	}
	return inventory, false, nil
}

type identityTestEnv struct {
	t        *testing.T
	server   *InventoryServer
	recorder *record.FakeRecorder
	ws       *httptest.Server
}

func newIdentityTestEnv(t *testing.T, objects ...client.Object) *identityTestEnv {
	t.Helper()
	return newIdentityTestEnvFor(t, nil, objects...)
}

// newIdentityTestEnvFor lets a test adjust the MachineRegistration.
func newIdentityTestEnvFor(t *testing.T, customize func(*elementalv1.MachineRegistration), objects ...client.Object) *identityTestEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := elementalv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	registration := &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{Name: "registration", Namespace: identityTestNamespace},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineName: "node-${System Information/UUID}",
			MachineInventoryLabels: map[string]string{
				"pool.baremetal.alauda.io/eligible": "true",
			},
			MachineInventoryAnnotations: map[string]string{
				elementalv1.MachineUUIDAnnotation: "${System Information/UUID}",
			},
		},
		Status: elementalv1.MachineRegistrationStatus{
			RegistrationToken: identityTestToken,
			RegistrationURL:   "https://example.com/elemental/registration/" + identityTestToken,
			ServiceAccountRef: &corev1.ObjectReference{Name: "registration", Namespace: identityTestNamespace},
			Conditions:        []metav1.Condition{{Type: elementalv1.ReadyCondition, Status: metav1.ConditionTrue, Reason: elementalv1.SuccessfullyCreatedReason}},
		},
	}
	if customize != nil {
		customize(registration)
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
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	recorder := record.NewFakeRecorder(100)
	server := &InventoryServer{
		Context:                context.Background(),
		Client:                 cl,
		ServerURL:              "https://example.com",
		AgentTLSMode:           AgentTLSModeStrict,
		SystemAgentClusterName: DefaultSystemAgentClusterName,
		EventRecorder:          recorder,
		authenticators:         []authenticator{&hashAuth{Client: cl}},
	}
	env := &identityTestEnv{t: t, server: server, recorder: recorder, ws: httptest.NewServer(server)}
	t.Cleanup(env.ws.Close)
	return env
}

// host is one registering client.
type host struct {
	auth        string // "tpm:<hash>" or "plain:<hash>"
	smbios      map[string]interface{}
	annotations map[string]string
	update      bool
}

func smbios(uuid, serial, board string) map[string]interface{} {
	return map[string]interface{}{
		"System Information":     map[string]interface{}{"UUID": uuid, "Serial Number": serial},
		"Base Board Information": map[string]interface{}{"Serial Number": board},
	}
}

// selfHealingClient is what a new elemental-register sends for the first
// registration of an emulated identity it derived.
func selfHealingClient() map[string]string {
	return map[string]string{"auth": "emulated-tpm", "tpm-seed-version": "2", "tpm-mode": "emulated"}
}

// register runs a registration and returns the final reply.
func (e *identityTestEnv) register(h host) (register.MessageType, []byte) {
	e.t.Helper()
	g := NewWithT(e.t)
	header := http.Header{}
	header.Add("Authorization", "Bearer FAKE"+h.auth)
	url := "ws" + strings.TrimPrefix(e.ws.URL, "http") + "/elemental/registration/" + identityTestToken
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	g.Expect(err).ToNot(HaveOccurred())
	defer ws.Close()

	exchange := func(msgType register.MessageType, data []byte) (register.MessageType, []byte) {
		g.Expect(register.WriteMessage(ws, msgType, data)).To(Succeed())
		reply, payload, err := register.ReadMessage(ws)
		g.Expect(err).ToNot(HaveOccurred())
		return reply, payload
	}
	reply, _, err := register.ReadMessage(ws)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(reply).To(Equal(register.MsgReady))
	// Below MsgNetworkConfig: the server would wait for NetworkConfigReady.
	reply, _ = exchange(register.MsgVersion, []byte{byte(register.MsgSystemDataV2)})
	g.Expect(reply).To(Equal(register.MsgVersion))
	if h.update {
		if reply, payload := exchange(register.MsgUpdate, nil); reply != register.MsgReady {
			return reply, payload
		}
	}
	if h.smbios != nil {
		data, _ := json.Marshal(h.smbios)
		reply, _ = exchange(register.MsgSmbios, data)
		g.Expect(reply).To(Equal(register.MsgReady))
	}
	if h.annotations != nil {
		data, _ := json.Marshal(h.annotations)
		reply, _ = exchange(register.MsgAnnotations, data)
		g.Expect(reply).To(Equal(register.MsgReady))
	}
	return exchange(register.MsgGet, nil)
}

func (e *identityTestEnv) inventories() []elementalv1.MachineInventory {
	e.t.Helper()
	list := &elementalv1.MachineInventoryList{}
	NewWithT(e.t).Expect(e.server.List(context.Background(), list)).To(Succeed())
	return list.Items
}

func (e *identityTestEnv) inventory(name string) *elementalv1.MachineInventory {
	e.t.Helper()
	inventory := &elementalv1.MachineInventory{}
	NewWithT(e.t).Expect(e.server.Get(context.Background(), client.ObjectKey{Namespace: identityTestNamespace, Name: name}, inventory)).To(Succeed())
	return inventory
}

func (e *identityTestEnv) events() []string {
	var events []string
	for {
		select {
		case event := <-e.recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

// refused runs a registration the server must refuse and returns why.
func (e *identityTestEnv) refused(h host) register.ErrorMessage {
	e.t.Helper()
	msgType, data := e.register(h)
	return errorReply(e.t, msgType, data)
}

func errorReply(t *testing.T, msgType register.MessageType, data []byte) register.ErrorMessage {
	t.Helper()
	g := NewWithT(t)
	g.Expect(msgType).To(Equal(register.MsgError), string(data))
	message := register.ErrorMessage{}
	g.Expect(yaml.Unmarshal(data, &message)).To(Succeed())
	return message
}

func fingerprintOf(uuid, serial, board string) identity.Fingerprint {
	return identity.NewFields(uuid, serial, board).Fingerprint()
}

// registeredInventory is an inventory host A created with the current server.
func registeredInventory(name, tpmHash, auth string, fp identity.Fingerprint) *elementalv1.MachineInventory {
	return &elementalv1.MachineInventory{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         identityTestNamespace,
			CreationTimestamp: metav1.Now(),
			Labels:            map[string]string{"pool.baremetal.alauda.io/eligible": "true", "pool.baremetal.alauda.io/name": "workers"},
			Annotations: map[string]string{
				elementalv1.MachineInventoryAuthAnnotation:        auth,
				elementalv1.MachineUUIDAnnotation:                 uuidA,
				elementalv1.HardwareFingerprintAnnotation:         fp.Value,
				elementalv1.HardwareFingerprintStrengthAnnotation: fp.Strength(),
			},
		},
		Spec: elementalv1.MachineInventorySpec{TPMHash: tpmHash},
	}
}

func TestIdentityFingerprintRecordedOnCreate(t *testing.T) {
	g := NewWithT(t)
	env := newIdentityTestEnv(t)

	reply, data := env.register(host{auth: "tpm:hash-a", smbios: smbios(uuidA, "7XK9H42", "CN7016"), annotations: selfHealingClient()})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))

	inventory := env.inventory("node-4c4c4544-0042-3510-8058-b4c04f4d4e32")
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, fingerprintOf(uuidA, "7XK9H42", "CN7016").Value))
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintStrengthAnnotation, "strong"))
	g.Expect(inventory.Annotations).To(HaveKeyWithValue("elemental.cattle.io/tpm-seed-version", "2"))
}

func TestIdentityWeakFingerprintAndNoSMBIOS(t *testing.T) {
	g := NewWithT(t)
	env := newIdentityTestEnv(t)

	reply, data := env.register(host{auth: "tpm:hash-weak", smbios: smbios("03000200-0400-0500-0006-000700080009", "Default string", "To be filled by O.E.M.")})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	reply, data = env.register(host{auth: "tpm:hash-nosmbios"})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))

	for _, inventory := range env.inventories() {
		switch inventory.Spec.TPMHash {
		case "hash-weak":
			g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintStrengthAnnotation, "weak"))
		case "hash-nosmbios":
			g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.HardwareFingerprintAnnotation))
		}
	}
}

func TestIdentityMatchingHardwareRegisters(t *testing.T) {
	g := NewWithT(t)
	fp := fingerprintOf(uuidA, "7XK9H42", "")
	env := newIdentityTestEnv(t, registeredInventory("node-a", "hash-a", "emulated-tpm", fp))

	reply, data := env.register(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidA, "7xk9h42", ""), annotations: map[string]string{"auth": "emulated-tpm", "registration-ip": "10.0.0.21"}})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, fp.Value))
	g.Expect(inventory.Annotations).To(HaveKeyWithValue("elemental.cattle.io/registration-ip", "10.0.0.21"))
	g.Expect(env.events()).To(BeEmpty())
}

func TestIdentityConflictLeavesInventoryUntouched(t *testing.T) {
	g := NewWithT(t)
	stored := registeredInventory("node-a", "hash-a", "emulated-tpm", fingerprintOf(uuidA, "", ""))
	env := newIdentityTestEnv(t, stored)
	before := env.inventory("node-a")

	// Host B derived the same EK (31-bit seed collision) on its first
	// registration: it can retry with another salt.
	reply, data := env.register(host{auth: "tpm:hash-a", smbios: smbios(uuidB, "", ""), annotations: selfHealingClient()})
	message := errorReply(t, reply, data)
	g.Expect(message.Code).To(Equal(register.ErrorCodeIdentityConflict))
	g.Expect(message.Message).To(ContainSubstring("node-a"))

	g.Expect(env.inventory("node-a")).To(Equal(before))
	g.Expect(env.inventories()).To(HaveLen(1))
	events := env.events()
	g.Expect(events).To(HaveLen(1))
	g.Expect(events[0]).To(HavePrefix("Warning IdentityConflict hardware fingerprint does not match the registered host"))
	g.Expect(events[0]).To(ContainSubstring("retries with another seed salt"))
}

func TestIdentityConflictMarksInventoryWhenClientCannotHeal(t *testing.T) {
	tests := []struct {
		name string
		host host
	}{
		{name: "installed host", host: host{update: true, annotations: map[string]string{"auth": "emulated-tpm", "tpm-seed-version": "2"}}},
		{name: "static seed", host: host{annotations: map[string]string{"auth": "emulated-tpm", "tpm-seed-version": "static"}}},
		{name: "inherited seed", host: host{annotations: map[string]string{"auth": "emulated-tpm", "tpm-seed-version": "legacy"}}},
		{name: "old client", host: host{annotations: map[string]string{"auth": "emulated-tpm"}}},
		{name: "hardware TPM", host: host{annotations: map[string]string{"auth": "tpm", "tpm-seed-version": "none"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			env := newIdentityTestEnv(t, registeredInventory("node-a", "hash-a", "emulated-tpm", fingerprintOf(uuidA, "", "")))
			before := env.inventory("node-a")

			h := tt.host
			h.auth = "tpm:hash-a"
			h.smbios = smbios(uuidB, "", "")
			message := env.refused(h)
			g.Expect(message.Code).To(Equal(register.ErrorCodeIdentityConflict))

			after := env.inventory("node-a")
			g.Expect(after.Annotations).To(HaveKeyWithValue(elementalv1.IdentityConflictAnnotation, HavePrefix("hardware fingerprint does not match the registered host")))
			// Nothing but the mark changed.
			delete(after.Annotations, elementalv1.IdentityConflictAnnotation)
			g.Expect(after.Annotations).To(Equal(before.Annotations))
			g.Expect(after.Spec).To(Equal(before.Spec))
			g.Expect(after.Labels).To(Equal(before.Labels))
			g.Expect(env.events()).To(ConsistOf(ContainSubstring("marked with " + elementalv1.IdentityConflictAnnotation)))
		})
	}
}

func TestIdentityBackfillChecksOldEvidence(t *testing.T) {
	legacy := func(name, machineUUID string) *elementalv1.MachineInventory {
		inventory := &elementalv1.MachineInventory{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: identityTestNamespace, CreationTimestamp: metav1.Now(), Annotations: map[string]string{
				elementalv1.MachineInventoryAuthAnnotation: "emulated-tpm",
			}},
			Spec: elementalv1.MachineInventorySpec{TPMHash: "hash-a"},
		}
		if machineUUID != "" {
			inventory.Annotations[elementalv1.MachineUUIDAnnotation] = machineUUID
		}
		return inventory
	}
	t.Run("consistent evidence backfills the fingerprint", func(t *testing.T) {
		g := NewWithT(t)
		env := newIdentityTestEnv(t, legacy("node-4c4c4544-0042-3510-8058-b4c04f4d4e32", uuidA))
		reply, data := env.register(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidA, "", ""), annotations: map[string]string{"auth": "emulated-tpm"}})
		g.Expect(reply).To(Equal(register.MsgConfig), string(data))
		inventory := env.inventory("node-4c4c4544-0042-3510-8058-b4c04f4d4e32")
		g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, fingerprintOf(uuidA, "", "").Value))
	})
	t.Run("no evidence backfills the fingerprint", func(t *testing.T) {
		g := NewWithT(t)
		env := newIdentityTestEnv(t, legacy("m-0f8e4f9a-3c2b-4d1e-9a7b-6c5d4e3f2a1b", ""))
		reply, data := env.register(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidB, "", ""), annotations: map[string]string{"auth": "emulated-tpm"}})
		g.Expect(reply).To(Equal(register.MsgConfig), string(data))
		g.Expect(env.inventory("m-0f8e4f9a-3c2b-4d1e-9a7b-6c5d4e3f2a1b").Annotations).To(HaveKey(elementalv1.HardwareFingerprintAnnotation))
	})
	t.Run("a UUID in the name contradicting the host is a conflict", func(t *testing.T) {
		g := NewWithT(t)
		env := newIdentityTestEnv(t, legacy("node-4c4c4544-0042-3510-8058-b4c04f4d4e32", ""))
		message := env.refused(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidB, "", ""), annotations: map[string]string{"auth": "emulated-tpm"}})
		g.Expect(message.Code).To(Equal(register.ErrorCodeIdentityConflict))
		inventory := env.inventory("node-4c4c4544-0042-3510-8058-b4c04f4d4e32")
		g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.HardwareFingerprintAnnotation))
		g.Expect(inventory.Annotations).To(HaveKey(elementalv1.IdentityConflictAnnotation))
	})
	t.Run("a machine-uuid annotation contradicting the host is a conflict", func(t *testing.T) {
		g := NewWithT(t)
		// Already collided before the upgrade: B re-rendered machine-uuid.
		env := newIdentityTestEnv(t, legacy("node-4c4c4544-0042-3510-8058-b4c04f4d4e32", uuidB))
		message := env.refused(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidA, "", ""), annotations: map[string]string{"auth": "emulated-tpm"}})
		g.Expect(message.Code).To(Equal(register.ErrorCodeIdentityConflict))
		g.Expect(env.inventory("node-4c4c4544-0042-3510-8058-b4c04f4d4e32").Annotations).To(HaveKey(elementalv1.IdentityConflictAnnotation))
	})
}

func TestIdentityServerOwnedAnnotationsCannotBeSent(t *testing.T) {
	g := NewWithT(t)
	fp := fingerprintOf(uuidA, "", "")
	env := newIdentityTestEnv(t, registeredInventory("node-a", "hash-a", "tpm", fp))

	annotations := map[string]string{
		"auth":                          "tpm",
		"hardware-fingerprint":          "v1:forged",
		"allow-identity-rebind":         "true",
		"identity-conflict":             "nope",
		"hardware-fingerprint-strength": "weak",
	}
	reply, data := env.register(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidA, "", ""), annotations: annotations})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, fp.Value))
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintStrengthAnnotation, "strong"))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.AllowIdentityRebindAnnotation))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.IdentityConflictAnnotation))
}

func TestIdentityAllowRebindAcceptsNewHardware(t *testing.T) {
	g := NewWithT(t)
	stored := registeredInventory("node-a", "hash-a", "emulated-tpm", fingerprintOf(uuidA, "OLD-BOARD", ""))
	stored.Annotations[elementalv1.AllowIdentityRebindAnnotation] = "true"
	stored.Annotations[elementalv1.IdentityConflictAnnotation] = "hardware fingerprint does not match the registered host: earlier"
	env := newIdentityTestEnv(t, stored)

	// Mainboard swap: same state seed, new serial.
	reply, data := env.register(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidA, "NEW-BOARD", ""), annotations: map[string]string{"auth": "emulated-tpm"}})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, fingerprintOf(uuidA, "NEW-BOARD", "").Value))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.AllowIdentityRebindAnnotation))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.IdentityConflictAnnotation))
	g.Expect(env.events()).To(ConsistOf(HavePrefix("Normal IdentityRebound")))
}

func TestIdentityReboundForWeakIdentities(t *testing.T) {
	g := NewWithT(t)
	fp := fingerprintOf(uuidA, "7XK9H42", "")
	env := newIdentityTestEnv(t, registeredInventory("node-a", "hash-v1", "emulated-tpm", fp))

	// The v1 host reinstalled from a new ISO: v2 seed, new hash.
	reply, data := env.register(host{auth: "tpm:hash-v2", smbios: smbios(uuidA, "7XK9H42", ""), annotations: selfHealingClient()})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))

	g.Expect(env.inventories()).To(HaveLen(1))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Spec.TPMHash).To(Equal("hash-v2"))
	g.Expect(inventory.Labels).To(HaveKeyWithValue("pool.baremetal.alauda.io/name", "workers"))
	g.Expect(inventory.Annotations).To(HaveKeyWithValue("elemental.cattle.io/tpm-seed-version", "2"))
	g.Expect(env.events()).To(ConsistOf(SatisfyAll(HavePrefix("Normal IdentityRebound"), ContainSubstring("tpmHash hash-v1"), ContainSubstring("tpmHash hash-v2"))))

	// The old identity is gone.
	message := env.refused(host{auth: "tpm:hash-v1", update: true, smbios: smbios(uuidA, "7XK9H42", "")})
	g.Expect(message.Message).To(ContainSubstring("not found"))
}

func TestIdentityReboundAcrossAuthMethods(t *testing.T) {
	g := NewWithT(t)
	stored := registeredInventory("node-a", "", "sys-uuid", fingerprintOf(uuidA, "", ""))
	stored.Spec.MachineHash = "plain-a"
	env := newIdentityTestEnv(t, stored)

	reply, data := env.register(host{auth: "tpm:hash-tpm", smbios: smbios(uuidA, "", ""), annotations: map[string]string{"auth": "tpm"}})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Spec.TPMHash).To(Equal("hash-tpm"))
	g.Expect(inventory.Spec.MachineHash).To(BeEmpty())
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.MachineInventoryAuthAnnotation, "tpm"))
}

func TestIdentityReboundFromOldEvidence(t *testing.T) {
	g := NewWithT(t)
	legacy := &elementalv1.MachineInventory{
		ObjectMeta: metav1.ObjectMeta{Name: "node-4c4c4544-0042-3510-8058-b4c04f4d4e32", Namespace: identityTestNamespace, CreationTimestamp: metav1.Now(), Annotations: map[string]string{
			elementalv1.MachineInventoryAuthAnnotation: "emulated-tpm",
			elementalv1.MachineUUIDAnnotation:          uuidA,
		}},
		Spec: elementalv1.MachineInventorySpec{TPMHash: "hash-v1"},
	}
	env := newIdentityTestEnv(t, legacy)

	reply, data := env.register(host{auth: "tpm:hash-v2", smbios: smbios(uuidA, "", ""), annotations: selfHealingClient()})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	g.Expect(env.inventories()).To(HaveLen(1))
	inventory := env.inventory(legacy.Name)
	g.Expect(inventory.Spec.TPMHash).To(Equal("hash-v2"))
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, fingerprintOf(uuidA, "", "").Value))
}

func TestIdentityHardwareTPMNeedsRebindAnnotation(t *testing.T) {
	g := NewWithT(t)
	fp := fingerprintOf(uuidA, "7XK9H42", "")
	env := newIdentityTestEnv(t, registeredInventory("node-a", "hash-ek-old", "tpm", fp))
	before := env.inventory("node-a")

	// fTPM cleared: new EK, same hardware.
	message := env.refused(host{auth: "tpm:hash-ek-new", smbios: smbios(uuidA, "7XK9H42", ""), annotations: map[string]string{"auth": "tpm"}})
	g.Expect(message.Code).To(Equal(register.ErrorCodeIdentityRebindRequired))
	g.Expect(message.Message).To(ContainSubstring(elementalv1.AllowIdentityRebindAnnotation))
	g.Expect(env.inventories()).To(HaveLen(1))
	g.Expect(env.inventory("node-a")).To(Equal(before))
	g.Expect(env.events()).To(ConsistOf(HavePrefix("Warning IdentityRebindRequired")))

	// The operator allows it once.
	allowed := env.inventory("node-a")
	allowed.Annotations[elementalv1.AllowIdentityRebindAnnotation] = "true"
	g.Expect(env.server.Update(context.Background(), allowed)).To(Succeed())
	reply, data := env.register(host{auth: "tpm:hash-ek-new", smbios: smbios(uuidA, "7XK9H42", ""), annotations: map[string]string{"auth": "tpm"}})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Spec.TPMHash).To(Equal("hash-ek-new"))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.AllowIdentityRebindAnnotation))
}

func TestIdentityNoRebindForWeakOrAmbiguousHardware(t *testing.T) {
	t.Run("weak fingerprint", func(t *testing.T) {
		g := NewWithT(t)
		weak := fingerprintOf("Not Settable", "Default string", "")
		env := newIdentityTestEnv(t, registeredInventory("node-a", "hash-a", "emulated-tpm", weak))
		reply, data := env.register(host{auth: "tpm:hash-b", smbios: smbios("Not Settable", "To be filled by O.E.M.", ""), annotations: selfHealingClient()})
		g.Expect(reply).To(Equal(register.MsgConfig), string(data))
		g.Expect(env.inventories()).To(HaveLen(2))
		g.Expect(env.inventory("node-a").Spec.TPMHash).To(Equal("hash-a"))
	})
	t.Run("several candidates", func(t *testing.T) {
		g := NewWithT(t)
		fp := fingerprintOf(uuidA, "", "")
		env := newIdentityTestEnv(t,
			registeredInventory("node-a", "hash-a", "emulated-tpm", fp),
			registeredInventory("node-a2", "hash-a2", "emulated-tpm", fp))
		reply, data := env.register(host{auth: "tpm:hash-new", smbios: smbios(uuidA, "", ""), annotations: selfHealingClient()})
		// The new inventory renders the same name as nothing else: node-<uuid>.
		g.Expect(reply).To(Equal(register.MsgConfig), string(data))
		g.Expect(env.inventories()).To(HaveLen(3))
	})
	t.Run("candidate in identity conflict", func(t *testing.T) {
		g := NewWithT(t)
		stored := registeredInventory("node-a", "hash-a", "emulated-tpm", fingerprintOf(uuidA, "", ""))
		stored.Annotations[elementalv1.IdentityConflictAnnotation] = "hardware fingerprint does not match the registered host: x"
		env := newIdentityTestEnv(t, stored)
		message := env.refused(host{auth: "tpm:hash-new", smbios: smbios(uuidA, "", ""), annotations: selfHealingClient()})
		g.Expect(message.Code).To(Equal(register.ErrorCodeIdentityRebindRequired))
		g.Expect(env.inventories()).To(HaveLen(1))
	})
}

func TestIdentityRebindAnnotationSettlesConflict(t *testing.T) {
	g := NewWithT(t)
	stored := registeredInventory("node-a", "hash-a", "emulated-tpm", fingerprintOf(uuidA, "", ""))
	stored.Annotations[elementalv1.IdentityConflictAnnotation] = "hardware fingerprint does not match the registered host: x"
	stored.Annotations[elementalv1.AllowIdentityRebindAnnotation] = "true"
	env := newIdentityTestEnv(t, stored)

	// The rightful host reinstalled and gets its inventory back.
	reply, data := env.register(host{auth: "tpm:hash-new", smbios: smbios(uuidA, "", ""), annotations: selfHealingClient()})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Spec.TPMHash).To(Equal("hash-new"))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.IdentityConflictAnnotation))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.AllowIdentityRebindAnnotation))
	g.Expect(env.events()).To(ConsistOf(SatisfyAll(HavePrefix("Normal IdentityRebound"), ContainSubstring("allowed by"))))
}

func TestIdentityRandomUUIDNamesAreNoEvidence(t *testing.T) {
	g := NewWithT(t)
	// node-${Random/UUID}: the UUID in the name says nothing about the
	// hardware, so it must not turn every installed host into a conflict.
	legacy := &elementalv1.MachineInventory{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0f8e4f9a-3c2b-4d1e-9a7b-6c5d4e3f2a1b", Namespace: identityTestNamespace, CreationTimestamp: metav1.Now(),
			Annotations: map[string]string{elementalv1.MachineInventoryAuthAnnotation: "emulated-tpm"}},
		Spec: elementalv1.MachineInventorySpec{TPMHash: "hash-a"},
	}
	env := newIdentityTestEnvFor(t, func(r *elementalv1.MachineRegistration) {
		r.Spec.MachineName = "node-${Random/UUID}"
	}, legacy)
	reply, data := env.register(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidA, "", ""), annotations: map[string]string{"auth": "emulated-tpm"}})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory(legacy.Name)
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, fingerprintOf(uuidA, "", "").Value))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.IdentityConflictAnnotation))
}

func TestIdentityAllowRebindIsNotForCollidingNewHosts(t *testing.T) {
	g := NewWithT(t)
	stored := registeredInventory("node-a", "hash-a", "emulated-tpm", fingerprintOf(uuidA, "", ""))
	stored.Annotations[elementalv1.AllowIdentityRebindAnnotation] = "true"
	env := newIdentityTestEnv(t, stored)
	before := env.inventory("node-a")

	// A new host whose derived seed collides is not the inventory's
	// hardware after a repair: it has to take another salt.
	message := env.refused(host{auth: "tpm:hash-a", smbios: smbios(uuidB, "", ""), annotations: selfHealingClient()})
	g.Expect(message.Code).To(Equal(register.ErrorCodeIdentityConflict))
	g.Expect(env.inventory("node-a")).To(Equal(before))
}

func TestIdentityAllowRebindCanNameTheHardware(t *testing.T) {
	g := NewWithT(t)
	// Two installed hosts collided on node-a; the operator decides that the
	// one with system UUID B keeps it.
	stored := registeredInventory("node-a", "hash-a", "emulated-tpm", fingerprintOf(uuidA, "", ""))
	stored.Annotations[elementalv1.IdentityConflictAnnotation] = "hardware fingerprint does not match the registered host: x"
	stored.Annotations[elementalv1.AllowIdentityRebindAnnotation] = strings.ToLower(uuidB)
	env := newIdentityTestEnv(t, stored)

	third := "4C4C4544-0099-4410-8058-B4C04F4D4E32"
	message := env.refused(host{auth: "tpm:hash-a", update: true, smbios: smbios(third, "", ""), annotations: map[string]string{"auth": "emulated-tpm"}})
	g.Expect(message.Code).To(Equal(register.ErrorCodeIdentityConflict))
	g.Expect(env.inventory("node-a").Annotations).To(HaveKey(elementalv1.AllowIdentityRebindAnnotation))

	reply, data := env.register(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidB, "", ""), annotations: map[string]string{"auth": "emulated-tpm"}})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, fingerprintOf(uuidB, "", "").Value))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.AllowIdentityRebindAnnotation))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.IdentityConflictAnnotation))
}

func TestIdentityUnknownFingerprintVersionIsKept(t *testing.T) {
	g := NewWithT(t)
	stored := registeredInventory("node-a", "hash-a", "emulated-tpm", fingerprintOf(uuidA, "", ""))
	stored.Annotations[elementalv1.HardwareFingerprintAnnotation] = "v9:0123"
	env := newIdentityTestEnv(t, stored)

	reply, data := env.register(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidB, "", ""), annotations: map[string]string{"auth": "emulated-tpm"}})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory("node-a")
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, "v9:0123"))
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.IdentityConflictAnnotation))
}

func TestIdentityTemplatesCannotSetServerOwnedAnnotations(t *testing.T) {
	g := NewWithT(t)
	env := newIdentityTestEnvFor(t, func(r *elementalv1.MachineRegistration) {
		r.Spec.MachineInventoryAnnotations[elementalv1.AllowIdentityRebindAnnotation] = "true"
		r.Spec.MachineInventoryAnnotations[elementalv1.HardwareFingerprintAnnotation] = "v1:templated"
	})
	reply, data := env.register(host{auth: "tpm:hash-a", smbios: smbios(uuidA, "", ""), annotations: selfHealingClient()})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	inventory := env.inventory("node-4c4c4544-0042-3510-8058-b4c04f4d4e32")
	g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.AllowIdentityRebindAnnotation))
	g.Expect(inventory.Annotations).To(HaveKeyWithValue(elementalv1.HardwareFingerprintAnnotation, fingerprintOf(uuidA, "", "").Value))
}

func TestIdentityRebindCandidates(t *testing.T) {
	t.Run("skips inventories being deleted", func(t *testing.T) {
		g := NewWithT(t)
		deleting := registeredInventory("node-a", "hash-v1", "emulated-tpm", fingerprintOf(uuidA, "", ""))
		deleting.Finalizers = []string{elementalv1.MachineInventoryFinalizer}
		now := metav1.Now()
		deleting.DeletionTimestamp = &now
		env := newIdentityTestEnv(t, deleting)
		reply, data := env.register(host{auth: "tpm:hash-v2", smbios: smbios(uuidA, "", ""), annotations: selfHealingClient()})
		g.Expect(reply).To(Equal(register.MsgConfig), string(data))
		g.Expect(env.inventories()).To(HaveLen(2))
		g.Expect(env.inventory("node-a").Spec.TPMHash).To(Equal("hash-v1"))
	})
	t.Run("looks cluster wide", func(t *testing.T) {
		g := NewWithT(t)
		other := registeredInventory("node-a", "hash-v1", "emulated-tpm", fingerprintOf(uuidA, "", ""))
		other.Namespace = "other-namespace"
		env := newIdentityTestEnv(t, other)
		reply, data := env.register(host{auth: "tpm:hash-v2", smbios: smbios(uuidA, "", ""), annotations: selfHealingClient()})
		g.Expect(reply).To(Equal(register.MsgConfig), string(data))
		g.Expect(env.inventories()).To(HaveLen(1))
		moved := &elementalv1.MachineInventory{}
		g.Expect(env.server.Get(context.Background(), client.ObjectKey{Namespace: "other-namespace", Name: "node-a"}, moved)).To(Succeed())
		g.Expect(moved.Spec.TPMHash).To(Equal("hash-v2"))
	})
	t.Run("drops what the previous identity reported", func(t *testing.T) {
		g := NewWithT(t)
		stored := registeredInventory("node-a", "hash-v1", "emulated-tpm", fingerprintOf(uuidA, "", ""))
		stored.Annotations["elemental.cattle.io/tpm-mode"] = "emulated"
		stored.Annotations["elemental.cattle.io/tpm-seed-version"] = "legacy"
		stored.Annotations["elemental.cattle.io/registration-ip"] = "10.0.0.21"
		env := newIdentityTestEnv(t, stored)
		// A client too old to send annotations.
		reply, data := env.register(host{auth: "plain:plain-new", smbios: smbios(uuidA, "", "")})
		g.Expect(reply).To(Equal(register.MsgConfig), string(data))
		inventory := env.inventory("node-a")
		g.Expect(inventory.Spec.MachineHash).To(Equal("plain-new"))
		g.Expect(inventory.Spec.TPMHash).To(BeEmpty())
		g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.MachineInventoryAuthAnnotation))
		g.Expect(inventory.Annotations).ToNot(HaveKey("elemental.cattle.io/tpm-mode"))
		g.Expect(inventory.Annotations).ToNot(HaveKey("elemental.cattle.io/tpm-seed-version"))
		g.Expect(inventory.Annotations).To(HaveKeyWithValue("elemental.cattle.io/registration-ip", "10.0.0.21"))
	})
}

func TestIdentityRegistrationWithoutSMBIOSIsReported(t *testing.T) {
	g := NewWithT(t)
	env := newIdentityTestEnv(t, registeredInventory("node-a", "hash-a", "emulated-tpm", fingerprintOf(uuidA, "", "")))
	reply, data := env.register(host{auth: "tpm:hash-a", update: true, annotations: map[string]string{"auth": "emulated-tpm"}})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	g.Expect(env.events()).To(ConsistOf(SatisfyAll(HavePrefix("Warning IdentityConflict"), ContainSubstring("without SMBIOS"))))
}
