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
	"fmt"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/registryaccess"
)

// addInstallRegistry gives an install the registry mirror of the
// MachineRegistration's registryClusterRef. Only a configuration that starts
// an install carries it: in inventory mode the one that releases the host,
// otherwise every one, since the host installs on its first configuration.
// Missing or invalid registry input fails the configuration, so the host does
// not install and registers again.
func (i *InventoryServer) addInstallRegistry(registration *elementalv1.MachineRegistration, config *elementalv1.Config) error {
	ref := registration.Spec.RegistryClusterRef
	if ref == nil || ref.Name == "" {
		return nil
	}
	if inventoryMode(registration) && config.Elemental.InstallTarget == nil {
		return nil
	}
	access, err := registryaccess.Resolve(i, i.Client, registration.Namespace, ref.Name)
	if err != nil {
		return fmt.Errorf("MachineRegistration %s/%s: registry mirror of registryClusterRef %s: %w", registration.Namespace, registration.Name, ref.Name, err)
	}
	doc, err := access.DockerConfig()
	if err != nil {
		return err
	}
	config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{
		Source:       access.Source,
		Endpoints:    append([]string(nil), access.Endpoints...),
		DockerConfig: string(doc),
	}
	log.Debugf("Install of MachineRegistration %s/%s pulls through %s", registration.Namespace, registration.Name, access.String())
	return nil
}
