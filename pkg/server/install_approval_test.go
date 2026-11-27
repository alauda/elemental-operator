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
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/network"
	"github.com/rancher/elemental-operator/pkg/register"
)

// The install is prepared (disk, spec.network, spec.install) and released by
// the install-approved annotation; these tests cover what the approval
// changes beyond the release itself, in install_session_test.go.

const (
	registrationSystemURI = "harbor.example.com:11443/tkestack/baremetal-base-image:v1"
	teamSystemURI         = "team.example.com:5000/tkestack/baremetal-base-image:v1"
	nodeKeyfile           = "[connection]\nid=node\ntype=ethernet\n[ethernet]\nmac-address=52:54:00:aa:00:01\n[ipv4]\nmethod=manual\naddress1=192.168.10.11/24,192.168.10.1\n"
)

func withSystemURI(uri string) func(*elementalv1.MachineRegistration) {
	return func(r *elementalv1.MachineRegistration) {
		r.Spec.Config.Elemental.Install.SystemURI = uri
	}
}

// A selection is not one-shot: a host that boots the live ISO again keeps
// the disk it was given and waits for an approval of this boot.
func TestLiveInstallSelectionSurvivesANewBoot(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
	env.hold(hostA.in("boot-1"))
	env.selectDisk("node-a", diskA)
	g.Expect(env.hold(hostA.in("boot-1")).Reason).To(Equal(elementalv1.AwaitingApprovalReason))

	hold := env.hold(hostA.in("boot-2"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingApprovalReason))
	g.Expect(hold.Message).To(HavePrefix("install target " + diskA + " "))
	g.Expect(env.inventory("node-a").Annotations).To(HaveKeyWithValue(elementalv1.InstallTargetDeviceAnnotation, diskA))
	g.Expect(env.events()).To(BeEmpty())

	env.approve("node-a")
	config, _ := env.config(hostA.in("boot-2"))
	g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
}

func TestLiveInstallApprovalWithoutSelectionIsDiscarded(t *testing.T) {
	g := NewWithT(t)
	env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
	env.hold(hostA.in("boot-1"))
	env.approve("node-a")

	hold := env.hold(hostA.in("boot-1"))
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	g.Expect(hold.Message).To(HavePrefix(`discarded baremetal.alauda.io/install-approved="true": no install target is selected; 2 of 3 disks`))
	g.Expect(env.inventory("node-a").Annotations).ToNot(HaveKey(elementalv1.InstallApprovedAnnotation))
	g.Expect(env.events()).To(ConsistOf(HavePrefix("Warning ApprovalDiscarded")))
	g.Expect(env.inventory("node-a").Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
}

func TestLiveInstallApprovalMustBeTrue(t *testing.T) {
	for _, value := range []string{"yes", "True", "false", ""} {
		t.Run(value, func(t *testing.T) {
			g := NewWithT(t)
			env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
			env.hold(hostA.in("boot-1"))
			env.update("node-a", func(inventory *elementalv1.MachineInventory) {
				inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation] = diskA
				inventory.Annotations[elementalv1.InstallApprovedAnnotation] = value
			})

			hold := env.hold(hostA.in("boot-1"))
			g.Expect(hold.Reason).To(Equal(elementalv1.ApprovalDiscardedReason))
			g.Expect(hold.Message).To(ContainSubstring(`only "true" approves the install`))
			g.Expect(env.inventory("node-a").Annotations).ToNot(HaveKey(elementalv1.InstallApprovedAnnotation))
			g.Expect(env.inventory("node-a").Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
		})
	}
}

// What the install is released with is what the MachineInventory holds when
// the approval is consumed: the preparation can change until then.
func TestLiveInstallReleasesWhatTheInventoryHoldsAtTheApproval(t *testing.T) {
	g := NewWithT(t)
	objects := append(defaultRegistryObjects(identityTestNamespace), teamRegistryObjects(identityTestNamespace)...)
	env := newInstallTestEnv(t, installEnvOptions{registration: withSystemURI(registrationSystemURI)}, append(objects, liveInventory("node-a"))...)
	env.hold(hostA.in("boot-1"))

	// Prepared in several steps, none of which releases anything.
	env.selectDisk("node-a", diskA)
	g.Expect(env.hold(hostA.in("boot-1")).Reason).To(Equal(elementalv1.AwaitingApprovalReason))
	env.update("node-a", func(inventory *elementalv1.MachineInventory) {
		inventory.Spec.Install = &elementalv1.MachineInventoryInstall{SystemURI: "elsewhere.example.com/tkestack/baremetal-base-image:v0"}
	})
	g.Expect(env.hold(hostA.in("boot-1")).Reason).To(Equal(elementalv1.AwaitingApprovalReason))
	env.update("node-a", func(inventory *elementalv1.MachineInventory) {
		inventory.Spec.Install.SystemURI = teamSystemURI
		inventory.Labels["cpaas.io/registry-reference"] = "team-registry"
		inventory.Spec.Network = elementalv1.NetworkConfig{
			Configurator: network.ConfiguratorNmconnections,
			Config:       map[string]runtime.RawExtension{"node": {Raw: []byte(`"` + strings.ReplaceAll(nodeKeyfile, "\n", `\n`) + `"`)}},
		}
	})
	g.Expect(env.hold(hostA.in("boot-1")).Reason).To(Equal(elementalv1.AwaitingApprovalReason))
	env.approve("node-a")

	config, raw := env.config(hostA.in("boot-1"))
	g.Expect(config.Elemental.Install.SystemURI).To(Equal(teamSystemURI))
	g.Expect(config.Elemental.InstallRegistry).ToNot(BeNil(), raw)
	g.Expect(config.Elemental.InstallRegistry.Endpoints).To(Equal([]string{"team.example.com:5000"}))
	g.Expect(config.Elemental.InstallRegistry.DockerConfig).To(ContainSubstring(`"team.example.com:5000":{"auth":"dGVhbTp6"}`))
	netConf := elementalv1.NetworkConfig{}
	g.Expect(yaml.Unmarshal([]byte(raw), &netConf)).To(Succeed())
	g.Expect(netConf.Configurator).To(Equal(network.ConfiguratorNmconnections))
	g.Expect(netConf.Config).To(HaveKey("node"))
	g.Expect(string(netConf.Config["node"].Raw)).To(ContainSubstring("mac-address=52:54:00:aa:00:01"))
	// The registration is untouched.
	g.Expect(env.registration().Spec.Config.Elemental.Install.SystemURI).To(Equal(registrationSystemURI))
}

// Two hosts of one registration pull from different registries: one keeps
// the registration's system-uri and credentials, the other overrides both on
// its MachineInventory. Both are toolkit installs: they get install-registry
// as KubeOS installs do.
func TestLiveInstallPerHostRegistry(t *testing.T) {
	g := NewWithT(t)
	objects := append(defaultRegistryObjects(identityTestNamespace), teamRegistryObjects(identityTestNamespace)...)
	nodeB := liveInventory("node-b")
	nodeB.Spec.Install = &elementalv1.MachineInventoryInstall{SystemURI: teamSystemURI}
	nodeB.Labels["cpaas.io/registry-reference"] = "team-registry"
	env := newInstallTestEnv(t, installEnvOptions{registration: withSystemURI(registrationSystemURI)}, append(objects, liveInventory("node-a"), nodeB)...)
	hostB := liveHost{auth: "tpm:hash-node-b"}

	for _, host := range []liveHost{hostA, hostB} {
		env.hold(host.in("boot-1"))
	}
	env.selectAndApprove("node-a", diskA)
	env.selectAndApprove("node-b", diskA)

	configA, _ := env.config(hostA.in("boot-1"))
	g.Expect(configA.Elemental.Install.SystemURI).To(Equal(registrationSystemURI))
	g.Expect(configA.Elemental.InstallRegistry.Endpoints).To(Equal([]string{"harbor.example.com:11443", "backup.example.com"}))

	configB, _ := env.config(hostB.in("boot-1"))
	g.Expect(configB.Elemental.Install.SystemURI).To(Equal(teamSystemURI))
	g.Expect(configB.Elemental.InstallRegistry.Endpoints).To(Equal([]string{"team.example.com:5000"}))
}

// A registration's template labels are applied to the inventory at every
// registration: one that names the registry wins over the label set on the
// inventory, which is why the default goes on the registration's own labels.
func TestRegistryReferenceTemplateLabelIsReapplied(t *testing.T) {
	g := NewWithT(t)
	objects := append(defaultRegistryObjects(identityTestNamespace), teamRegistryObjects(identityTestNamespace)...)
	inventory := liveInventory("node-a")
	inventory.Labels["cpaas.io/registry-reference"] = "team-registry"
	env := newInstallTestEnv(t, installEnvOptions{registration: func(r *elementalv1.MachineRegistration) {
		r.Spec.Config.Elemental.Install.SystemURI = registrationSystemURI
		r.Spec.MachineInventoryLabels = map[string]string{"cpaas.io/registry-reference": "public-registry-credential"}
	}}, append(objects, inventory)...)
	env.hold(hostA.in("boot-1"))
	g.Expect(env.inventory("node-a").Labels).To(HaveKeyWithValue("cpaas.io/registry-reference", "public-registry-credential"))
	env.selectAndApprove("node-a", diskA)
	config, _ := env.config(hostA.in("boot-1"))
	g.Expect(config.Elemental.InstallRegistry.Endpoints).To(Equal([]string{"harbor.example.com:11443", "backup.example.com"}))
}

// A live ISO whose elemental-register predates the toolkit install through
// install-registry is not released to an install from a system-uri: its
// toolkit would partition the disk first and fail to pull afterwards.
func TestLiveInstallOldClientProtection(t *testing.T) {
	old := register.MsgInstallReport

	t.Run("inventory mode", func(t *testing.T) {
		g := NewWithT(t)
		env := newInstallTestEnv(t, installEnvOptions{registration: withSystemURI(registrationSystemURI)},
			append(defaultRegistryObjects(identityTestNamespace), liveInventory("node-a"))...)
		oldHost := liveHost{auth: hostA.auth, protoVersion: old}

		hold := env.hold(oldHost.in("boot-1"))
		g.Expect(hold.Reason).To(Equal(elementalv1.LiveTooOldReason))
		g.Expect(hold.Message).To(ContainSubstring("elemental-register v1.9.1-alauda of live boot boot-1 is too old"))
		inventory := env.inventory("node-a")
		g.Expect(inventory.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
		g.Expect(inventory.Status.Install.Message).To(Equal(hold.Message))
		g.Expect(condition(inventory, elementalv1.InstallTargetReadyCondition)).To(haveCondition(metav1.ConditionFalse, elementalv1.LiveTooOldReason))

		// An approval does not get it released either, and is used up.
		env.selectAndApprove("node-a", diskA)
		g.Expect(env.hold(oldHost.in("boot-1")).Reason).To(Equal(elementalv1.LiveTooOldReason))
		g.Expect(env.inventory("node-a").Annotations).ToNot(HaveKey(elementalv1.InstallApprovedAnnotation))
		g.Expect(env.inventory("node-a").Annotations).To(HaveKeyWithValue(elementalv1.InstallTargetDeviceAnnotation, diskA))
		g.Expect(env.inventory("node-a").Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))

		// The same host booted from a new ISO is released.
		g.Expect(env.hold(hostA.in("boot-2")).Reason).To(Equal(elementalv1.AwaitingApprovalReason))
		g.Expect(env.inventory("node-a").Status.Install.Message).To(BeEmpty())
		env.approve("node-a")
		config, raw := env.config(hostA.in("boot-2"))
		g.Expect(config.Elemental.Install.SystemURI).To(Equal(registrationSystemURI))
		g.Expect(config.Elemental.InstallRegistry).ToNot(BeNil(), raw)
	})
	t.Run("a system-uri on the inventory only", func(t *testing.T) {
		g := NewWithT(t)
		inventory := liveInventory("node-a")
		inventory.Spec.Install = &elementalv1.MachineInventoryInstall{SystemURI: teamSystemURI}
		env := newInstallTestEnv(t, installEnvOptions{}, inventory)
		g.Expect(env.hold(liveHost{auth: hostA.auth, protoVersion: old}.in("boot-1")).Reason).To(Equal(elementalv1.LiveTooOldReason))
	})
	t.Run("once the system-uri is gone the old live waits as before", func(t *testing.T) {
		g := NewWithT(t)
		inventory := liveInventory("node-a")
		inventory.Spec.Install = &elementalv1.MachineInventoryInstall{SystemURI: teamSystemURI}
		env := newInstallTestEnv(t, installEnvOptions{}, inventory)
		oldHost := liveHost{auth: hostA.auth, protoVersion: old}
		g.Expect(env.hold(oldHost.in("boot-1")).Reason).To(Equal(elementalv1.LiveTooOldReason))
		env.update("node-a", func(inventory *elementalv1.MachineInventory) { inventory.Spec.Install = nil })
		g.Expect(env.hold(oldHost.in("boot-1")).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
		g.Expect(env.inventory("node-a").Status.Install.Message).To(BeEmpty())
		env.selectAndApprove("node-a", diskA)
		config, _ := env.config(oldHost.in("boot-1"))
		g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
		g.Expect(config.Elemental.Install.SystemURI).To(BeEmpty())
	})
	t.Run("an install from the live system is not affected", func(t *testing.T) {
		g := NewWithT(t)
		env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
		oldHost := liveHost{auth: hostA.auth, protoVersion: old}
		env.hold(oldHost.in("boot-1"))
		env.selectAndApprove("node-a", diskA)
		config, _ := env.config(oldHost.in("boot-1"))
		g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
	})
	t.Run("KubeOS is not affected", func(t *testing.T) {
		g := NewWithT(t)
		env := newInstallTestEnv(t, installEnvOptions{registration: withSystemURI(registrationSystemURI)},
			append(defaultRegistryObjects(identityTestNamespace), liveInventory("node-a"))...)
		session := liveSession("boot-1")
		session.Installer = elementalv1.InstallerKubeOS
		oldHost := liveHost{auth: hostA.auth, protoVersion: old}.with(session)
		g.Expect(env.hold(oldHost).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
		env.selectAndApprove("node-a", diskA)
		config, _ := env.config(oldHost)
		g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
		g.Expect(config.Elemental.InstallRegistry).ToNot(BeNil())
	})
	t.Run("legacy mode holds a client that can be held", func(t *testing.T) {
		g := NewWithT(t)
		env := newInstallTestEnv(t, installEnvOptions{legacy: true, registration: withSystemURI(registrationSystemURI)},
			append(defaultRegistryObjects(identityTestNamespace), liveInventory("node-a"))...)
		hold := env.hold(liveHost{auth: hostA.auth, protoVersion: old}.in("boot-1"))
		g.Expect(hold.Reason).To(Equal(elementalv1.LiveTooOldReason))
		g.Expect(env.inventory("node-a").Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))

		// A new client installs right away, as legacy mode does.
		config, _ := env.config(hostA.in("boot-2"))
		g.Expect(config.Elemental.Install.SystemURI).To(Equal(registrationSystemURI))
		g.Expect(config.Elemental.InstallRegistry).ToNot(BeNil())
		g.Expect(env.inventory("node-a").Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalling))
	})
	t.Run("legacy mode refuses a client that cannot be held", func(t *testing.T) {
		g := NewWithT(t)
		env := newInstallTestEnv(t, installEnvOptions{legacy: true, registration: withSystemURI(registrationSystemURI)},
			append(defaultRegistryObjects(identityTestNamespace), liveInventory("node-a"))...)
		reply, data := env.registerLive(liveHost{auth: hostA.auth, protoVersion: register.MsgLiveInstallSession}.in("boot-1"))
		g.Expect(errorReply(t, reply, data).Message).To(ContainSubstring("is too old"))
	})
}

// registration reads the stored MachineRegistration.
func (e *installTestEnv) registration() *elementalv1.MachineRegistration {
	e.t.Helper()
	registration := &elementalv1.MachineRegistration{}
	NewWithT(e.t).Expect(e.server.Get(e.server, client.ObjectKey{Namespace: identityTestNamespace, Name: "registration"}, registration)).To(Succeed())
	return registration
}

// An unidentified: ID names a disk by its kernel path in the live boot that
// reported it: after a reboot it may name another disk. Such a selection does
// not survive a new live boot, while one by a stable ID does.
func TestLiveInstallUnidentifiedSelectionDoesNotSurviveANewBoot(t *testing.T) {
	unidentified := func(bootID string) *register.LiveInstallSession {
		session := liveSession(bootID)
		session.Candidates = append(session.Candidates, elementalv1.InstallCandidate{ID: "unidentified:dev_vda", Path: "/dev/vda", SizeBytes: 107374182400, Eligible: true})
		return session
	}

	t.Run("selection only", func(t *testing.T) {
		g := NewWithT(t)
		env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
		env.hold(hostA.with(unidentified("boot-1")))
		env.selectDisk("node-a", "UNIDENTIFIED:dev_vda")
		g.Expect(env.hold(hostA.with(unidentified("boot-1"))).Reason).To(Equal(elementalv1.AwaitingApprovalReason))

		hold := env.hold(hostA.with(unidentified("boot-2")))
		g.Expect(hold.Reason).To(Equal(elementalv1.SelectionDiscardedReason))
		g.Expect(hold.Message).To(HavePrefix(`discarded baremetal.alauda.io/install-target-device="UNIDENTIFIED:dev_vda": an unidentified: ID names a disk only in the live boot that reported it, and live boot boot-2 registered since`))
		g.Expect(hold.Message).To(ContainSubstring("pick one with"))
		g.Expect(env.inventory("node-a").Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))
		g.Expect(condition(env.inventory("node-a"), elementalv1.InstallTargetReadyCondition)).To(haveCondition(metav1.ConditionFalse, elementalv1.SelectionDiscardedReason))
		g.Expect(env.events()).To(ConsistOf(HavePrefix("Warning SelectionDiscarded")))
		g.Expect(env.hold(hostA.with(unidentified("boot-2"))).Reason).To(Equal(elementalv1.AwaitingSelectionReason))
	})
	t.Run("selection and approval", func(t *testing.T) {
		g := NewWithT(t)
		env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
		env.hold(hostA.with(unidentified("boot-1")))
		env.selectAndApprove("node-a", "unidentified:dev_vda")

		// The host rebooted before it registered again: neither carries over.
		hold := env.hold(hostA.with(unidentified("boot-2")))
		g.Expect(hold.Reason).To(Equal(elementalv1.SelectionDiscardedReason))
		g.Expect(hold.Message).To(ContainSubstring(`discarded baremetal.alauda.io/install-target-device="unidentified:dev_vda"`))
		g.Expect(hold.Message).To(ContainSubstring(`discarded baremetal.alauda.io/install-approved="true": it was set before live boot boot-2 registered`))
		inventory := env.inventory("node-a")
		g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))
		g.Expect(inventory.Annotations).ToNot(HaveKey(elementalv1.InstallApprovedAnnotation))
		g.Expect(env.events()).To(ConsistOf(HavePrefix("Warning SelectionDiscarded"), HavePrefix("Warning ApprovalDiscarded")))
	})
	t.Run("same boot", func(t *testing.T) {
		g := NewWithT(t)
		env := newInstallTestEnv(t, installEnvOptions{}, liveInventory("node-a"))
		env.hold(hostA.with(unidentified("boot-1")))
		env.selectDisk("node-a", "unidentified:dev_vda")
		g.Expect(env.hold(hostA.with(unidentified("boot-1"))).Reason).To(Equal(elementalv1.AwaitingApprovalReason))
		env.approve("node-a")
		config, _ := env.config(hostA.with(unidentified("boot-1")))
		g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal("unidentified:dev_vda"))
	})
	t.Run("a discard that runs into a conflict still removes it", func(t *testing.T) {
		g := NewWithT(t)
		armed := &atomic.Bool{}
		env := newInstallTestEnv(t, installEnvOptions{funcs: conflictOnPatchOf(elementalv1.InstallTargetDeviceAnnotation, armed)}, liveInventory("node-a"))
		env.hold(hostA.with(unidentified("boot-1")))
		env.selectDisk("node-a", "unidentified:dev_vda")
		armed.Store(true)
		g.Expect(env.hold(hostA.with(unidentified("boot-2"))).Reason).To(Equal(elementalv1.SelectionDiscardedReason))
		g.Expect(armed.Load()).To(BeFalse())
		g.Expect(env.inventory("node-a").Annotations).ToNot(HaveKey(elementalv1.InstallTargetDeviceAnnotation))
	})
}

// conflictOnPatchOf makes the next patch that touches the annotation key fail
// with a conflict.
func conflictOnPatchOf(key string, armed *atomic.Bool) interceptor.Funcs {
	return interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		data, err := patch.Data(obj)
		if err == nil && strings.Contains(string(data), key) && armed.CompareAndSwap(true, false) {
			return apierrors.NewConflict(schema.GroupResource{Group: "elemental.cattle.io", Resource: "machineinventories"}, obj.GetName(), errors.New("the object has been modified"))
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}
}

// The registry mirror input an install pulls its system image through is
// resolved before the approval is consumed. Input that cannot be used holds
// the host with the reason on the inventory and discards the approval; the
// selection stays, and the install is released once the input is fixed and
// the install approved again.
func TestLiveInstallRegistryInputIsCheckedBeforeTheRelease(t *testing.T) {
	missingEndpoint := defaultRegistryObjects(identityTestNamespace)
	missingEndpoint[1].(*corev1.Secret).Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{"harbor.example.com:11443":{"auth":"cm9ib3Q6eA=="}}}`)
	for _, tc := range []struct {
		name      string
		systemURI string
		objects   []client.Object
		label     string
		want      string
		fix       func(env *installTestEnv)
	}{
		{
			name: "a named credential that does not exist", systemURI: registrationSystemURI,
			objects: defaultRegistryObjects(identityTestNamespace), label: "missing",
			want: "MachineInventory " + identityTestNamespace + "/node-a: registry mirror input: registry credential " + identityTestNamespace + "/missing named by cpaas.io/registry-reference does not exist",
			fix: func(env *installTestEnv) {
				env.update("node-a", func(inventory *elementalv1.MachineInventory) { delete(inventory.Labels, "cpaas.io/registry-reference") })
			},
		},
		{
			name: "an endpoint without credentials", systemURI: registrationSystemURI, objects: missingEndpoint,
			want: "MachineRegistration " + identityTestNamespace + "/registration: registry mirror input: registry credential Secret " + identityTestNamespace + "/acp-registry-bootstrap-auth-1 has no entry for endpoint backup.example.com",
			fix: func(env *installTestEnv) {
				secret := &corev1.Secret{}
				NewWithT(env.t).Expect(env.server.Get(context.Background(), client.ObjectKey{Namespace: identityTestNamespace, Name: "acp-registry-bootstrap-auth-1"}, secret)).To(Succeed())
				secret.Data[corev1.DockerConfigJsonKey] = defaultRegistryObjects(identityTestNamespace)[1].(*corev1.Secret).Data[corev1.DockerConfigJsonKey]
				NewWithT(env.t).Expect(env.server.Update(context.Background(), secret)).To(Succeed())
			},
		},
		{
			name: "a logical system-uri without mirror input", systemURI: "cpaas-registry.cpaas-system/tkestack/baremetal-base-image:v1",
			want: "system-uri cpaas-registry.cpaas-system/tkestack/baremetal-base-image:v1 is under cpaas-registry.cpaas-system, but no registry mirror input exists",
			fix: func(env *installTestEnv) {
				for _, object := range defaultRegistryObjects(identityTestNamespace) {
					NewWithT(env.t).Expect(env.server.Create(context.Background(), object)).To(Succeed())
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			inventory := liveInventory("node-a")
			if tc.label != "" {
				inventory.Labels["cpaas.io/registry-reference"] = tc.label
			}
			env := newInstallTestEnv(t, installEnvOptions{registration: withSystemURI(tc.systemURI)}, append(tc.objects, inventory)...)
			env.hold(hostA.in("boot-1"))
			env.selectAndApprove("node-a", diskA)

			hold := env.hold(hostA.in("boot-1"))
			g.Expect(hold.Reason).To(Equal(elementalv1.RegistryInputInvalidReason))
			g.Expect(hold.RetryAfterSeconds).To(Equal(register.DefaultInstallHoldSeconds))
			g.Expect(hold.Message).To(HavePrefix(`discarded baremetal.alauda.io/install-approved="true": `))
			g.Expect(hold.Message).To(ContainSubstring(tc.want))
			g.Expect(hold.Message).To(HaveSuffix(`; baremetal.alauda.io/install-target-device="` + diskA + `" stays; fix the registry mirror input, then approve the install again`))
			stored := env.inventory("node-a")
			g.Expect(stored.Annotations).ToNot(HaveKey(elementalv1.InstallApprovedAnnotation))
			g.Expect(stored.Annotations).To(HaveKeyWithValue(elementalv1.InstallTargetDeviceAnnotation, diskA))
			g.Expect(stored.Status.Install.Phase).To(Equal(elementalv1.InstallPhaseAwaitingTarget))
			g.Expect(condition(stored, elementalv1.InstallTargetReadyCondition)).To(haveCondition(metav1.ConditionFalse, elementalv1.RegistryInputInvalidReason))
			g.Expect(condition(stored, elementalv1.InstallTargetReadyCondition).Message).To(Equal(hold.Message))
			g.Expect(env.events()).To(ConsistOf(HavePrefix("Warning ApprovalDiscarded")))

			// Fixed, the host waits for a new approval, which releases it.
			tc.fix(env)
			g.Expect(env.hold(hostA.in("boot-1")).Reason).To(Equal(elementalv1.AwaitingApprovalReason))
			env.approve("node-a")
			config, raw := env.config(hostA.in("boot-1"))
			g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
			g.Expect(config.Elemental.InstallRegistry).ToNot(BeNil(), raw)
			g.Expect(config.Elemental.InstallRegistry.Endpoints).To(Equal([]string{"harbor.example.com:11443", "backup.example.com"}))
			g.Expect(env.inventory("node-a").Status.Install.Phase).To(Equal(elementalv1.InstallPhaseInstalling))
		})
	}
}

// Input that cannot be read for now does not use up the approval: the host
// is checked again shortly and released once the read succeeds.
func TestLiveInstallRegistryInputReadErrorKeepsTheApproval(t *testing.T) {
	g := NewWithT(t)
	armed := &atomic.Bool{}
	funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if key.Name == "public-registry-credential" && armed.CompareAndSwap(true, false) {
			return apierrors.NewServiceUnavailable("etcd is unavailable")
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	env := newInstallTestEnv(t, installEnvOptions{registration: withSystemURI(registrationSystemURI), funcs: funcs},
		append(defaultRegistryObjects(identityTestNamespace), liveInventory("node-a"))...)
	env.hold(hostA.in("boot-1"))
	env.selectAndApprove("node-a", diskA)
	armed.Store(true)

	hold := env.hold(hostA.in("boot-1"))
	g.Expect(armed.Load()).To(BeFalse())
	g.Expect(hold.Reason).To(Equal(elementalv1.AwaitingApprovalReason))
	g.Expect(hold.RetryAfterSeconds).To(Equal(selectionRetrySeconds))
	g.Expect(hold.Message).To(HavePrefix("baremetal.alauda.io/install-approved is checked again: the registry mirror input could not be read: "))
	g.Expect(hold.Message).To(ContainSubstring("etcd is unavailable"))
	g.Expect(env.inventory("node-a").Annotations).To(HaveKeyWithValue(elementalv1.InstallApprovedAnnotation, elementalv1.InstallApprovedValue))
	g.Expect(env.events()).To(BeEmpty())

	config, _ := env.config(hostA.in("boot-1"))
	g.Expect(config.Elemental.InstallTarget.DeviceID).To(Equal(diskA))
	g.Expect(config.Elemental.InstallRegistry.Endpoints).To(Equal([]string{"harbor.example.com:11443", "backup.example.com"}))
	g.Expect(env.inventory("node-a").Annotations).ToNot(HaveKey(elementalv1.InstallApprovedAnnotation))
}
