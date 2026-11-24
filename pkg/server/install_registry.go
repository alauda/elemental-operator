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
	"strings"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/registryaccess"
)

// addInstallRegistry gives an install that pulls its system image
// (install.system-uri, by the toolkit or the KubeOS live install) the
// registry mirror input the host's cpaas.io/registry-reference label selects:
// the MachineInventory's when it has the label, else the
// MachineRegistration's, public-registry-credential when neither names one.
// Only a configuration that starts an install carries it: in inventory mode
// the one that releases the host, otherwise every one, since the host
// installs on its first configuration. Invalid input, or a system-uri under
// the logical registry with no mirror input to pull it through, fails the
// configuration, so the host does not install and registers again.
func (i *InventoryServer) addInstallRegistry(registration *elementalv1.MachineRegistration, inventory *elementalv1.MachineInventory, config *elementalv1.Config) error {
	systemURI := config.Elemental.Install.SystemURI
	if systemURI == "" {
		return nil
	}
	if inventoryMode(registration) && config.Elemental.InstallTarget == nil {
		return nil
	}
	selection := registrySelectionOf(registration, inventory)
	access, err := registryaccess.ResolveDefault(i, i.Client, registration.Namespace, selection.labels)
	if err != nil {
		return fmt.Errorf("%s: registry mirror input: %w", selection.owner, err)
	}
	if access == nil {
		if pullsLogicalRegistry(systemURI) {
			return fmt.Errorf("%s: system-uri %s is under %s, but no registry mirror input exists (%s, or %s on a bootstrap cluster)",
				selection.owner, systemURI, registryaccess.LogicalRegistry, registryaccess.DefaultCredential, registryaccess.PreparedInput)
		}
		return nil
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
	log.Debugf("Install of %s pulls through %s", selection.owner, access.String())
	return nil
}

// pullsLogicalRegistry reports whether a system-uri, read the way the KubeOS
// install reads it (docker:// or docker: transport optional), is under the
// logical registry.
func pullsLogicalRegistry(ref string) bool {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "docker://")
	ref = strings.TrimPrefix(ref, "docker:")
	return strings.HasPrefix(ref, registryaccess.LogicalRegistry+"/")
}
