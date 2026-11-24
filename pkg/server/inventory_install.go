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
	"github.com/rancher/elemental-operator/pkg/register"
	"github.com/rancher/elemental-operator/pkg/registryaccess"
)

// installOf is the install section of the configuration a host of inventory
// gets: the MachineRegistration's, with what the MachineInventory overrides
// in spec.install. Read when the configuration is sent, so the values an
// install is released with are those at the moment of the release.
func installOf(registration *elementalv1.MachineRegistration, inventory *elementalv1.MachineInventory) elementalv1.Install {
	install := registration.Spec.Config.Elemental.Install
	if inventory != nil && inventory.Spec.Install != nil {
		if uri := strings.TrimSpace(inventory.Spec.Install.SystemURI); uri != "" {
			install.SystemURI = uri
		}
	}
	return install
}

// registrySelection is the cpaas.io/registry-reference label that selects
// the registry mirror input of an install, and the object it comes from.
type registrySelection struct {
	labels map[string]string
	// owner names the object the label was read from, for messages.
	owner string
}

// registrySelectionOf reads the cpaas.io/registry-reference label of the
// MachineInventory when it has one, the MachineRegistration's otherwise. A
// label present on the inventory wins even when empty: it then selects the
// default credential, whatever the registration names.
func registrySelectionOf(registration *elementalv1.MachineRegistration, inventory *elementalv1.MachineInventory) registrySelection {
	if inventory != nil {
		if value, found := inventory.Labels[registryaccess.ReferenceLabel]; found {
			return registrySelection{
				labels: map[string]string{registryaccess.ReferenceLabel: value},
				owner:  fmt.Sprintf("MachineInventory %s/%s", inventory.Namespace, inventory.Name),
			}
		}
	}
	return registrySelection{
		labels: registration.Labels,
		owner:  fmt.Sprintf("MachineRegistration %s/%s", registration.Namespace, registration.Name),
	}
}

// liveTooOld tells whether the install a live session would be released to
// is one its elemental-register cannot do safely: a toolkit install from a
// system-uri, by a client from before MsgToolkitInstallRegistry. Such a
// client ignores install-registry, and its toolkit partitions the disk
// before it finds out that it cannot pull the image.
func liveTooOld(protoVersion register.MessageType, session *register.LiveInstallSession, install elementalv1.Install) bool {
	return session.Installer == elementalv1.InstallerToolkit &&
		strings.TrimSpace(install.SystemURI) != "" &&
		protoVersion < register.MsgToolkitInstallRegistry
}

func liveTooOldMessage(protoVersion register.MessageType, session *register.LiveInstallSession) string {
	version := session.RegisterVersion
	if version == "" {
		version = "of unknown version"
	}
	return fmt.Sprintf("the install pulls install.system-uri, but elemental-register %s of live boot %s is too old to do that safely (protocol %d, needs %d): boot the host from a live ISO with a newer elemental-register",
		version, session.BootID, protoVersion, register.MsgToolkitInstallRegistry)
}
