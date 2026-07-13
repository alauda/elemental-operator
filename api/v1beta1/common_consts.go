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

package v1beta1

import (
	"fmt"
	"strings"
)

const (
	// ElementalManagedLabel label used to put on resources managed by the elemental operator.
	ElementalManagedLabel = "elemental.cattle.io/managed"

	// ElementalManagedOSImageVersionNameLabel label is used to filter ManagedOSImages referencing a ManagedOSVersion.
	ElementalManagedOSImageVersionNameLabel = "elemental.cattle.io/managed-os-version-name"

	// ElementalManagedOSVersionChannelLabel is used to filter a set of ManagedOSVersions given the channel they originate from.
	ElementalManagedOSVersionChannelLabel = "elemental.cattle.io/channel"

	// ElementalManagedOSVersionChannelLastSyncAnnotation reports when a ManagedOSVersion was last synced from a channel.
	ElementalManagedOSVersionChannelLastSyncAnnotation = "elemental.cattle.io/channel-last-sync"

	// ElementalManagedOSVersionNoLongerSyncedAnnotation is used to mark a no longer in sync ManagedOSVersion, this highlight it can be deleted.
	ElementalManagedOSVersionNoLongerSyncedAnnotation = "elemental.cattle.io/channel-no-longer-in-sync"
	ElementalManagedOSVersionNoLongerSyncedValue      = "true"

	// SASecretSuffix is the suffix used to name registration service account's token secret
	SASecretSuffix = "-token"

	// SystemAgentServerURLAnnotation overrides the base URL used in the system-agent
	// kubeconfig returned for a MachineRegistration.
	SystemAgentServerURLAnnotation = "baremetal.cluster.io/system-agent-server-url"

	// SystemAgentEndpointModeAnnotation overrides how the system-agent URL is built
	// for a MachineRegistration. Supported values are "erebus" and
	// "direct-apiserver".
	SystemAgentEndpointModeAnnotation = "baremetal.cluster.io/system-agent-endpoint-mode"

	// SystemAgentDirectAPIServerAnnotation is the legacy boolean alias for
	// SystemAgentEndpointModeAnnotation=direct-apiserver. The enum annotation,
	// when present, takes precedence over this alias.
	SystemAgentDirectAPIServerAnnotation = "baremetal.cluster.io/system-agent-direct"

	// SystemAgentAuthScopeAnnotation selects which shared system-agent identity a
	// MachineRegistration and its MachineInventories use when split auth is
	// enabled. Global machines use a cluster-local identity while all other
	// machines use the DR-synchronized shared identity.
	SystemAgentAuthScopeAnnotation = "baremetal.cluster.io/system-agent-auth-scope"

	SystemAgentAuthScopeGlobal = "global"
	SystemAgentAuthScopeShared = "shared"

	// TimeoutEnvVar is the environment variable key passed to pods to express a timeout
	TimeoutEnvVar = "ELEMENTAL_TIMEOUT"
)

// NormalizeSystemAgentAuthScope normalizes an auth scope. The empty value is
// kept backwards compatible with the original single shared identity.
func NormalizeSystemAgentAuthScope(scope string) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return SystemAgentAuthScopeShared
	}
	return scope
}

// ResolveSystemAgentAuthScope returns the auth scope selected by annotations.
func ResolveSystemAgentAuthScope(annotations map[string]string) (string, error) {
	scope := ""
	if annotations != nil {
		scope = annotations[SystemAgentAuthScopeAnnotation]
	}
	scope = NormalizeSystemAgentAuthScope(scope)
	switch scope {
	case SystemAgentAuthScopeGlobal, SystemAgentAuthScopeShared:
		return scope, nil
	default:
		return "", fmt.Errorf("invalid system-agent auth scope %q, valid values: %q, %q",
			scope, SystemAgentAuthScopeGlobal, SystemAgentAuthScopeShared)
	}
}
