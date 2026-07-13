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

package operator

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestSharedSystemAgentServiceAccountsMustDiffer(t *testing.T) {
	cmd := NewOperatorCommand()
	assert.NilError(t, cmd.ParseFlags([]string{
		"--server-url=https://elemental.example.com",
		"--system-agent-auth-mode=shared",
		"--system-agent-service-account=system-agent",
		"--system-agent-global-service-account=system-agent",
		"--system-agent-split-auth-enabled",
	}))

	err := cmd.Args(cmd, nil)
	assert.ErrorContains(t, err, "must be different")
}

func TestLegacySharedSystemAgentAllowsUnusedGlobalServiceAccount(t *testing.T) {
	cmd := NewOperatorCommand()
	assert.NilError(t, cmd.ParseFlags([]string{
		"--server-url=https://elemental.example.com",
		"--system-agent-auth-mode=shared",
		"--system-agent-service-account=system-agent",
		"--system-agent-global-service-account=system-agent",
	}))

	assert.NilError(t, cmd.Args(cmd, nil))
}

func TestSharedSystemAgentSplitFlags(t *testing.T) {
	cmd := NewOperatorCommand()
	assert.NilError(t, cmd.ParseFlags([]string{
		"--server-url=https://elemental.example.com",
		"--system-agent-auth-mode=shared",
		"--system-agent-service-account=baremetal-system-agent",
		"--system-agent-global-service-account=baremetal-global-system-agent",
		"--system-agent-split-auth-enabled",
		"--system-agent-shared-auth-read-only",
	}))

	assert.NilError(t, cmd.Args(cmd, nil))
	readOnlyFlag := cmd.Flags().Lookup("system-agent-shared-auth-read-only")
	assert.Assert(t, readOnlyFlag != nil)
	assert.Equal(t, readOnlyFlag.Value.String(), "true")
	splitAuthFlag := cmd.Flags().Lookup("system-agent-split-auth-enabled")
	assert.Assert(t, splitAuthFlag != nil)
	assert.Equal(t, splitAuthFlag.Value.String(), "true")
}

func TestSharedSystemAgentSplitDefaultsOff(t *testing.T) {
	cmd := NewOperatorCommand()
	splitAuthFlag := cmd.PersistentFlags().Lookup("system-agent-split-auth-enabled")
	assert.Assert(t, splitAuthFlag != nil)
	assert.Equal(t, splitAuthFlag.DefValue, "false")
}

func TestSplitAndReadOnlyRequireSharedAuthMode(t *testing.T) {
	for _, flag := range []string{"--system-agent-split-auth-enabled", "--system-agent-shared-auth-read-only"} {
		t.Run(flag, func(t *testing.T) {
			cmd := NewOperatorCommand()
			assert.NilError(t, cmd.ParseFlags([]string{
				"--server-url=https://elemental.example.com",
				"--system-agent-auth-mode=registration",
				flag,
			}))
			err := cmd.Args(cmd, nil)
			assert.ErrorContains(t, err, "requires system-agent-auth-mode=shared")
		})
	}
}
