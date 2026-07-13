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
	"regexp"
	"testing"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"gotest.tools/v3/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInitNewInventory(t *testing.T) {
	const alphanum = "[0-9a-fA-F]"
	// m  '-'  8 alphanum chars  '-'  3 blocks of 4 alphanum chars  '-'  12 alphanum chars
	mUUID := regexp.MustCompile("^m-" + alphanum + "{8}-(" + alphanum + "{4}-){3}" + alphanum + "{12}")
	// e.g., m-66588488-3eb6-4a6d-b642-c994f128c6f1

	testCase := []struct {
		config       elementalv1.Config
		initName     string
		expectedName string
	}{
		{
			config: elementalv1.Config{
				Elemental: elementalv1.Elemental{
					Registration: elementalv1.Registration{
						NoSMBIOS: false,
					},
				},
			},
			initName:     "custom-name",
			expectedName: "custom-name",
		},

		{
			config: elementalv1.Config{
				Elemental: elementalv1.Elemental{
					Registration: elementalv1.Registration{
						NoSMBIOS: false,
					},
				},
			},
		},
		{
			config: elementalv1.Config{
				Elemental: elementalv1.Elemental{
					Registration: elementalv1.Registration{
						NoSMBIOS: true,
					},
				},
			},
		},
		{
			config: elementalv1.Config{},
		},
	}

	for _, test := range testCase {
		registration := &elementalv1.MachineRegistration{
			Spec: elementalv1.MachineRegistrationSpec{
				MachineName: test.initName,
				Config:      test.config,
			},
		}

		inventory := &elementalv1.MachineInventory{}
		assert.NilError(t, initInventory(inventory, registration, false))
		_, hasAuthScope := inventory.Annotations[elementalv1.SystemAgentAuthScopeAnnotation]
		assert.Equal(t, hasAuthScope, false)

		if test.initName == "" {
			assert.Check(t, mUUID.Match([]byte(inventory.Name)), inventory.Name+" is not UUID based")
		} else {
			assert.Equal(t, inventory.Name, test.expectedName)
		}
	}
}

func TestGetSystemAgentURLDirectAPIServer(t *testing.T) {
	srv := &InventoryServer{SystemAgentClusterName: "global"}

	reg := &elementalv1.MachineRegistration{}
	reg.Annotations = map[string]string{
		elementalv1.SystemAgentServerURLAnnotation: "https://10.0.0.9:6443",
	}

	// Default (Erebus): the base URL gets the /kubernetes/<cluster> proxy path.
	got, err := srv.getSystemAgentURL(reg)
	assert.NilError(t, err)
	assert.Equal(t, got, "https://10.0.0.9:6443/kubernetes/global")

	// Direct: the base URL is used verbatim, with NO /kubernetes/<cluster> suffix.
	reg.Annotations[elementalv1.SystemAgentDirectAPIServerAnnotation] = "true"
	got, err = srv.getSystemAgentURL(reg)
	assert.NilError(t, err)
	assert.Equal(t, got, "https://10.0.0.9:6443")

	// Annotation is case-insensitive and the base URL trailing slash is trimmed.
	reg.Annotations[elementalv1.SystemAgentDirectAPIServerAnnotation] = "TRUE"
	reg.Annotations[elementalv1.SystemAgentServerURLAnnotation] = "https://10.0.0.9:6443/"
	got, err = srv.getSystemAgentURL(reg)
	assert.NilError(t, err)
	assert.Equal(t, got, "https://10.0.0.9:6443")
}

func TestInitInventoryAuthScope(t *testing.T) {
	registration := &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeGlobal,
			},
		},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineInventoryAnnotations: map[string]string{
				elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeShared,
			},
		},
	}
	inventory := &elementalv1.MachineInventory{}

	assert.NilError(t, initInventory(inventory, registration, true))
	assert.Equal(t, inventory.Annotations[elementalv1.SystemAgentAuthScopeAnnotation], elementalv1.SystemAgentAuthScopeGlobal)
	assert.Equal(t, registration.Spec.MachineInventoryAnnotations[elementalv1.SystemAgentAuthScopeAnnotation], elementalv1.SystemAgentAuthScopeShared)
}

func TestApplyMachineInventoryAuthScopeRejectsChange(t *testing.T) {
	registration := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeGlobal,
		},
	}}
	inventory := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{
		CreationTimestamp: metav1.Now(),
		Annotations: map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: elementalv1.SystemAgentAuthScopeShared,
		},
	}}

	err := applyMachineInventoryAuthScope(inventory, registration)
	assert.ErrorContains(t, err, "cannot change MachineInventory system-agent auth scope")
}

func TestApplyMachineInventoryAuthScopePreservesUnscopedLegacyInventory(t *testing.T) {
	registration := &elementalv1.MachineRegistration{}
	inventory := &elementalv1.MachineInventory{ObjectMeta: metav1.ObjectMeta{
		CreationTimestamp: metav1.Now(),
		Annotations: map[string]string{
			"baremetal.alauda.io/owner-cluster": "global",
		},
	}}

	assert.NilError(t, applyMachineInventoryAuthScope(inventory, registration))
	_, found := inventory.Annotations[elementalv1.SystemAgentAuthScopeAnnotation]
	assert.Equal(t, found, false)
}

func TestInitInventoryRejectsInvalidAuthScope(t *testing.T) {
	registration := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: "invalid",
		},
	}}

	err := initInventory(&elementalv1.MachineInventory{}, registration, true)
	assert.ErrorContains(t, err, "invalid system-agent auth scope")
}

func TestInitInventoryIgnoresAuthScopeWhenSplitDisabled(t *testing.T) {
	registration := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{
			elementalv1.SystemAgentAuthScopeAnnotation: "invalid",
		},
	}}
	inventory := &elementalv1.MachineInventory{}

	assert.NilError(t, initInventory(inventory, registration, false))
	_, found := inventory.Annotations[elementalv1.SystemAgentAuthScopeAnnotation]
	assert.Equal(t, found, false)
}
