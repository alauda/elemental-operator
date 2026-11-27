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

package elementalcli

import (
	"slices"
	"strings"
	"testing"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

func envValue(env []string, key string) (string, bool) {
	found, value := false, ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			found, value = true, v
		}
	}
	return value, found
}

func TestInstallFromASystemURIInstallsTheSameRecoverySystem(t *testing.T) {
	env := installEnv(elementalv1.Install{SystemURI: "docker:10.0.0.1:11443/tkestack/base:v1"}, InstallOptions{})
	system, _ := envValue(env, "ELEMENTAL_INSTALL_SYSTEM")
	recovery, found := envValue(env, "ELEMENTAL_INSTALL_RECOVERY_SYSTEM")
	if !found || recovery != system || system != "docker:10.0.0.1:11443/tkestack/base:v1" {
		t.Fatalf("SYSTEM=%q RECOVERY_SYSTEM=%q (set %v), want both the system-uri", system, recovery, found)
	}
	if _, found := envValue(env, "DOCKER_CONFIG"); found {
		t.Fatalf("DOCKER_CONFIG set without credentials: %v", env)
	}
}

func TestInstallFromTheLiveSystemLeavesTheRecoveryToTheToolkit(t *testing.T) {
	env := installEnv(elementalv1.Install{}, InstallOptions{})
	if _, found := envValue(env, "ELEMENTAL_INSTALL_RECOVERY_SYSTEM"); found {
		t.Fatalf("RECOVERY_SYSTEM set for an install from the live system: %v", env)
	}
	if args := installArgs(elementalv1.Install{}, InstallOptions{}); !slices.Equal(args, []string{"elemental", "install"}) {
		t.Fatalf("args = %v", args)
	}
}

func TestInstallThroughAnInstallRegistry(t *testing.T) {
	conf := elementalv1.Install{SystemURI: "docker:harbor.example.com/tkestack/base:v1", Debug: true, ConfigDir: "/etc/elemental"}
	opts := InstallOptions{DockerConfig: "/run/elemental/registry-docker", SkipTLSVerify: true}

	args := installArgs(conf, opts)
	want := []string{"elemental", "--debug", "--config-dir", "/etc/elemental", "install", "--tls-verify=false"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	env := installEnv(conf, opts)
	if dir, _ := envValue(env, "DOCKER_CONFIG"); dir != "/run/elemental/registry-docker" {
		t.Fatalf("DOCKER_CONFIG = %q in %v", dir, env)
	}
	// It comes last, so it wins over one the register inherited.
	if !strings.HasPrefix(env[len(env)-1], "DOCKER_CONFIG=") {
		t.Fatalf("DOCKER_CONFIG is not the last entry: %v", env)
	}
}
