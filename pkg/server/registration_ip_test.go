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
	"testing"

	. "github.com/onsi/gomega"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/register"
)

func TestRegistrationIPLabelValue(t *testing.T) {
	for _, tc := range []struct {
		reported string
		want     string
	}{
		{reported: "10.0.0.21", want: "10.0.0.21"},
		{reported: " 192.168.164.230\n", want: "192.168.164.230"},
		{reported: "fd00::21", want: "fd00-0000-0000-0000-0000-0000-0000-0021"},
		{reported: "FD00:0:0:0:0:0:0:21", want: "fd00-0000-0000-0000-0000-0000-0000-0021"},
		{reported: "2001:db8:85a3:8d3:1319:8a2e:370:7348", want: "2001-0db8-85a3-08d3-1319-8a2e-0370-7348"},
		{reported: "[fd00::21]", want: "fd00-0000-0000-0000-0000-0000-0000-0021"},
		{reported: "fe80::1%eth0", want: "fe80-0000-0000-0000-0000-0000-0000-0001"},
		{reported: "[fe80::a:b%br314]", want: "fe80-0000-0000-0000-0000-0000-000a-000b"},
		{reported: "::ffff:10.0.0.21", want: "10.0.0.21"},
		{reported: "::", want: "0000-0000-0000-0000-0000-0000-0000-0000"},
		{reported: ""},
		{reported: "garbage"},
		{reported: "10.0.0.21:9090"},
		{reported: "[fd00::21]:9090"},
		{reported: "10.0.0.256"},
		{reported: "node-a.example.com"},
	} {
		t.Run(tc.reported, func(t *testing.T) {
			NewWithT(t).Expect(registrationIPLabelValue(tc.reported)).To(Equal(tc.want))
		})
	}
}

func TestRegistrationIPLabel(t *testing.T) {
	g := NewWithT(t)
	fp := fingerprintOf(uuidA, "", "")
	// A template for the same key must not win over what the host reports.
	env := newIdentityTestEnvFor(t, func(r *elementalv1.MachineRegistration) {
		r.Spec.MachineInventoryLabels[elementalv1.RegistrationIPLabel] = "templated"
	}, registeredInventory("node-a", "hash-a", "emulated-tpm", fp))
	registerFrom := func(ip string) map[string]string {
		t.Helper()
		reply, data := env.register(host{auth: "tpm:hash-a", update: true, smbios: smbios(uuidA, "", ""),
			annotations: map[string]string{"auth": "emulated-tpm", "registration-ip": ip}})
		g.Expect(reply).To(Equal(register.MsgConfig), string(data))
		return env.inventory("node-a").Labels
	}

	g.Expect(registerFrom("10.0.0.21")).To(HaveKeyWithValue(elementalv1.RegistrationIPLabel, "10.0.0.21"))
	// Refreshed by every registration, e.g. the installed system's address.
	labels := registerFrom("[fd00::21]")
	g.Expect(labels).To(HaveKeyWithValue(elementalv1.RegistrationIPLabel, "fd00-0000-0000-0000-0000-0000-0000-0021"))
	g.Expect(labels).To(HaveKeyWithValue("pool.baremetal.alauda.io/name", "workers"))
	// A value that is no IP removes the label rather than keep a stale one.
	g.Expect(registerFrom("unknown")).ToNot(HaveKey(elementalv1.RegistrationIPLabel))
	g.Expect(env.inventory("node-a").Annotations).To(HaveKeyWithValue("elemental.cattle.io/registration-ip", "unknown"))
}

func TestRegistrationIPLabelOnNewInventory(t *testing.T) {
	g := NewWithT(t)
	env := newIdentityTestEnv(t)
	reply, data := env.register(host{auth: "tpm:hash-new", smbios: smbios(uuidA, "", ""),
		annotations: map[string]string{"auth": "tpm", "registration-ip": "192.168.164.230"}})
	g.Expect(reply).To(Equal(register.MsgConfig), string(data))
	g.Expect(env.inventory("node-4c4c4544-0042-3510-8058-b4c04f4d4e32").Labels).To(HaveKeyWithValue(elementalv1.RegistrationIPLabel, "192.168.164.230"))
}
