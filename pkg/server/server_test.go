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
		initInventory(inventory, registration)

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

func TestConcatCABundle(t *testing.T) {
	assert.Equal(t, concatCABundle("A", "B"), "A\nB")
	assert.Equal(t, concatCABundle("", "B"), "B")
	assert.Equal(t, concatCABundle("A", ""), "A")
	assert.Equal(t, concatCABundle("  A  ", "  B  "), "A\nB")
}
