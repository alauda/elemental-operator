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
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
)

const TempCloudInitDir = "/tmp/elemental/cloud-init"

// dockerConfigEnv names the directory go-containerregistry, which the toolkit
// pulls images with, reads config.json from.
const dockerConfigEnv = "DOCKER_CONFIG"

// InstallOptions are what elemental install needs beyond the install section
// of the configuration. The zero value runs it as before.
type InstallOptions struct {
	// DockerConfig is the directory set as DOCKER_CONFIG for the install:
	// the registry credentials the toolkit pulls install.system-uri with.
	DockerConfig string
	// SkipTLSVerify passes --tls-verify=false: the toolkit does not verify
	// the certificate of the registry it pulls install.system-uri from.
	SkipTLSVerify bool
}

type Runner interface {
	Install(elementalv1.Install, InstallOptions) error
	Reset(elementalv1.Reset) error
}

func NewRunner() Runner {
	return &runner{}
}

var _ Runner = (*runner)(nil)

type runner struct{}

func (r *runner) Install(conf elementalv1.Install, opts InstallOptions) error {
	installerOpts := installArgs(conf, opts)
	cmd := exec.Command("elemental")
	environmentVariables := installEnv(conf, opts)
	cmd.Env = append(os.Environ(), environmentVariables...)
	cmd.Stdout = os.Stdout
	cmd.Args = installerOpts
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	log.Debugf("running: %s\n with ENV:\n%s", strings.Join(installerOpts, " "), strings.Join(environmentVariables, "\n"))
	return cmd.Run()
}

func (r *runner) Reset(conf elementalv1.Reset) error {
	installerOpts := []string{"elemental"}
	// There are no env var bindings in elemental-cli for elemental root options
	// so root flags should be passed within the command line
	if conf.Debug {
		installerOpts = append(installerOpts, "--debug")
	}
	installerOpts = append(installerOpts, "reset")

	cmd := exec.Command("elemental")
	environmentVariables := mapToResetEnv(conf)
	cmd.Env = append(os.Environ(), environmentVariables...)
	cmd.Stdout = os.Stdout
	cmd.Args = installerOpts
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	log.Debugf("running: %s\n with ENV:\n%s", strings.Join(installerOpts, " "), strings.Join(environmentVariables, "\n"))
	return cmd.Run()
}

// installArgs is the elemental install command line.
func installArgs(conf elementalv1.Install, opts InstallOptions) []string {
	installerOpts := []string{"elemental"}
	// There are no env var bindings in elemental-cli for elemental root options
	// so root flags should be passed within the command line
	if conf.Debug {
		installerOpts = append(installerOpts, "--debug")
	}

	if conf.ConfigDir != "" {
		installerOpts = append(installerOpts, "--config-dir", conf.ConfigDir)
	}
	installerOpts = append(installerOpts, "install")
	// The registry TLS policy has no environment variable.
	if opts.SkipTLSVerify {
		installerOpts = append(installerOpts, "--tls-verify=false")
	}
	return installerOpts
}

// installEnv is the environment elemental install runs with on top of the
// register's own: later entries win over the inherited ones.
func installEnv(conf elementalv1.Install, opts InstallOptions) []string {
	variables := mapToInstallEnv(conf)
	if opts.DockerConfig != "" {
		variables = append(variables, formatEV(dockerConfigEnv, opts.DockerConfig))
	}
	return variables
}

func mapToInstallEnv(conf elementalv1.Install) []string {
	var variables []string
	// See GetInstallKeyEnvMap() in https://github.com/rancher/elemental-toolkit/blob/main/pkg/constants/constants.go
	variables = append(variables, formatEV("ELEMENTAL_INSTALL_TARGET", conf.Device))
	variables = append(variables, formatEV("ELEMENTAL_INSTALL_SYSTEM", conf.SystemURI))
	// The toolkit sets the recovery source to the live system before it
	// reads the environment, so a system-uri alone would put the live system
	// on the recovery partition. The same source makes it snapshot the
	// installed system instead of pulling the image a second time.
	if conf.SystemURI != "" {
		variables = append(variables, formatEV("ELEMENTAL_INSTALL_RECOVERY_SYSTEM", conf.SystemURI))
	}
	variables = append(variables, formatEV("ELEMENTAL_INSTALL_FIRMWARE", conf.Firmware))
	variables = append(variables, formatEV("ELEMENTAL_INSTALL_ISO", conf.ISO))
	variables = append(variables, formatEV("ELEMENTAL_INSTALL_TTY", conf.TTY))
	variables = append(variables, formatEV("ELEMENTAL_INSTALL_DISABLE_BOOT_ENTRY", strconv.FormatBool(conf.DisableBootEntry)))
	variables = append(variables, formatEV("ELEMENTAL_INSTALL_NO_FORMAT", strconv.FormatBool(conf.NoFormat)))
	// See GetRunKeyEnvMap() in https://github.com/rancher/elemental-toolkit/blob/main/pkg/constants/constants.go
	variables = append(variables, formatEV("ELEMENTAL_POWEROFF", strconv.FormatBool(conf.PowerOff)))
	variables = append(variables, formatEV("ELEMENTAL_REBOOT", strconv.FormatBool(conf.Reboot)))
	variables = append(variables, formatEV("ELEMENTAL_EJECT_CD", strconv.FormatBool(conf.EjectCD)))
	variables = append(variables, formatEV("ELEMENTAL_SNAPSHOTTER_TYPE", conf.Snapshotter.Type))
	variables = append(variables, formatEV("ELEMENTAL_SNAPSHOTTER_MAX_SNAPS", fmt.Sprintf("%d", conf.Snapshotter.MaxSnaps)))
	variables = append(variables, formatEV("ELEMENTAL_CLOUD_INIT_PATHS", TempCloudInitDir))
	return variables
}

func mapToResetEnv(conf elementalv1.Reset) []string {
	var variables []string
	// See GetResetKeyEnvMap() in https://github.com/rancher/elemental-toolkit/blob/main/pkg/constants/constants.go
	variables = append(variables, formatEV("ELEMENTAL_RESET_SYSTEM", conf.SystemURI))
	variables = append(variables, formatEV("ELEMENTAL_RESET_PERSISTENT", strconv.FormatBool(conf.ResetPersistent)))
	variables = append(variables, formatEV("ELEMENTAL_RESET_OEM", strconv.FormatBool(conf.ResetOEM)))
	variables = append(variables, formatEV("ELEMENTAL_RESET_DISABLE_BOOT_ENTRY", strconv.FormatBool(conf.DisableBootEntry)))
	// See GetRunKeyEnvMap() in https://github.com/rancher/elemental-toolkit/blob/main/pkg/constants/constants.go
	variables = append(variables, formatEV("ELEMENTAL_POWEROFF", strconv.FormatBool(conf.PowerOff)))
	variables = append(variables, formatEV("ELEMENTAL_REBOOT", strconv.FormatBool(conf.Reboot)))
	variables = append(variables, formatEV("ELEMENTAL_CLOUD_INIT_PATHS", TempCloudInitDir))
	return variables
}

func formatEV(key string, value string) string {
	return fmt.Sprintf("%s=%s", key, value)
}
