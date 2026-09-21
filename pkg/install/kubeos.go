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

package install

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/twpayne/go-vfs"
	"gopkg.in/yaml.v3"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/register"
)

// KubeOS install backend.
//
// KubeOS (ait/os-build, kubeos/) is installed by `kbimg install disk` from a
// live ISO that the same elemental toolkit built, so registration runs exactly
// as on SLE Micro; only the step that writes the OS to disk differs. kbimg
// reads one TOML file (shipped in the live image as /root/kbimg.toml with the
// partition layout already computed), pulls the rootfs from a registry, and
// copies any [[install.configs]] entries into the target. Everything the
// installed system needs at first boot — agent connection files, registration
// state, NetworkManager profiles, the cloud-init datasource — goes in through
// those entries.
//
// Nothing here touches the elemental toolkit: KubeOS keeps its own A/B slots,
// PERSIST partition and kbosctl for upgrades. See
// cluster-api-provider-baremetal docs/kubeos/design-spec.md §4.1.

const (
	// KubeOSInstaller is the --installer value that selects this backend.
	KubeOSInstaller = "kubeos"
	// ToolkitInstaller is the default --installer value: elemental install.
	ToolkitInstaller = "toolkit"

	kubeosDefaultTomlTemplate = "/root/kbimg.toml"
	kubeosDefaultWorkDir      = "/tmp/elemental/kubeos"
	kubeosLiveRegistriesDir   = "/etc/containers/registries.conf.d"
	kubeosLiveAuthFile        = "/run/containers/0/auth.json"

	// Target-side paths. /etc and /var are overlays whose upperdirs live on
	// PERSIST, shared by both A/B slots, so these survive kbosctl upgrade.
	kubeosRegistrationDir    = "/etc/elemental/registration"
	kubeosRegistrationConfig = kubeosRegistrationDir + "/config.yaml"
	kubeosRegistrationState  = kubeosRegistrationDir + "/state.yaml"
	kubeosDatasourceCfgPath  = "/etc/cloud/cloud.cfg.d/99-baremetal-datasource.cfg"
	kubeosTargetAuthFile     = "/root/.config/containers/auth.json"

	// kubeosDatasourceCfg pins cloud-init to NoCloud. The image does not ship a
	// datasource list; without one cloud-init probes every cloud provider and
	// finds none on bare metal. No seedfrom: the provider writes the seed into
	// cloud-init's default directory at reprovision, the same path the elemental
	// contract uses.
	kubeosDatasourceCfg = "datasource_list: [ NoCloud ]\n"

	kubeosInstallSuccess = "KubeOS installed successfully"
)

// kubeosRequiredPartitions must all appear on the target disk after
// `kbimg install disk`. kbimg's own exit status is not trustworthy — internal
// failures often return 0 — so the layout is checked directly.
var kubeosRequiredPartitions = []string{"ROOT-A", "ROOT-B", "PERSIST"}

// kubeosRunner is the subset of process execution the backend needs, kept as
// an interface so tests can drive it without kbimg on the box.
type kubeosRunner interface {
	// Output runs a command quietly and returns its combined output.
	Output(name string, args ...string) ([]byte, error)
	// Stream runs a command, tees its combined output to the console, and
	// returns it. Used for the install itself, which takes minutes and
	// prints progress worth seeing on the serial console.
	Stream(name string, args ...string) ([]byte, error)
}

type execKubeOSRunner struct{}

func (execKubeOSRunner) Output(name string, args ...string) ([]byte, error) {
	log.Debugf("running: %s %s", name, strings.Join(args, " "))
	return exec.Command(name, args...).CombinedOutput()
}

func (execKubeOSRunner) Stream(name string, args ...string) ([]byte, error) {
	log.Infof("running: %s %s", name, strings.Join(args, " "))
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = io.MultiWriter(os.Stderr, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	err := cmd.Run()
	return buf.Bytes(), err
}

// kubeosOptions are the injectable knobs of the backend.
type kubeosOptions struct {
	runner            kubeosRunner
	tomlTemplate      string
	workDir           string
	liveRegistriesDir string
	liveAuthFile      string
}

func defaultKubeOSOptions() kubeosOptions {
	return kubeosOptions{
		runner:            execKubeOSRunner{},
		tomlTemplate:      kubeosDefaultTomlTemplate,
		workDir:           kubeosDefaultWorkDir,
		liveRegistriesDir: kubeosLiveRegistriesDir,
		liveAuthFile:      kubeosLiveAuthFile,
	}
}

// kubeosStagedFile is one [[install.configs]] entry: src is a file the
// backend wrote under workDir/files, dst its absolute path in the target.
type kubeosStagedFile struct {
	src  string
	dst  string
	mode os.FileMode
}

// InstallKubeOS registers nothing (that already happened) and writes KubeOS
// to the selected disk with everything the first boot needs.
func (i *installer) InstallKubeOS(config elementalv1.Config, state register.State, networkConfig elementalv1.NetworkConfig) error {
	install := config.Elemental.Install

	// 1. Target disk: an explicit device wins, otherwise the selector.
	device := install.Device
	if device == "" {
		selected, err := i.findInstallationDevice(install.DeviceSelector)
		if err != nil {
			return fmt.Errorf("failed picking installation device: %w", err)
		}
		device = selected
	} else if len(install.DeviceSelector) > 0 {
		log.Warningf("Both device and device-selector set, using device-field '%s'", device)
	}

	// 2. The rootfs reference. kbimg pulls it itself; there is no local fallback.
	if install.SystemURI == "" {
		return errors.New("kubeos install requires elemental.install.system-uri (the kubeos-rootfs image reference)")
	}
	systemURI := kubeosImageReference(install.SystemURI)

	warnIgnoredKubeOSInstallFields(install)
	if len(config.Elemental.Install.ConfigURLs) > 0 {
		log.Warningf("kubeos install ignores %d config-urls entries; KubeOS has no yip stage to run them", len(config.Elemental.Install.ConfigURLs))
	}
	if len(config.CloudConfig) > 0 {
		log.Warning("kubeos install ignores MachineRegistration cloud-config: KubeOS has no yip; use the SeedImage cloud-config for the live phase and the provider's cloud-init for the installed system")
	}

	// 3. Stage every first-boot file, then the toml that references them.
	filesDir := filepath.Join(i.kubeos.workDir, "files")
	if err := vfs.MkdirAll(i.fs, filesDir, 0o700); err != nil {
		return fmt.Errorf("creating kubeos staging dir: %w", err)
	}
	staged, err := i.stageKubeOSFiles(filesDir, config, state, networkConfig)
	if err != nil {
		return err
	}
	tomlPath := filepath.Join(i.kubeos.workDir, "kbimg.toml")
	if err := i.writeKubeOSToml(tomlPath, device, systemURI, staged); err != nil {
		return err
	}

	// 4. Fail before partitioning if the image cannot be reached: kbimg
	// otherwise discovers that several gigabytes in, on a disk it already wiped.
	if out, err := i.kubeos.runner.Output("skopeo", "inspect", "--raw", systemURI); err != nil {
		return fmt.Errorf("kubeos rootfs image %s is not reachable from the live environment (check registries.conf.d and skopeo login): %w\n%s", systemURI, err, strings.TrimSpace(string(out)))
	}

	// 5. Install. `yes |` is not optional: kbimg's final format_rootb runs
	// mkfs.ext4 without -F and waits for confirmation forever otherwise.
	out, runErr := i.kubeos.runner.Stream("sh", "-c", `yes | kbimg install disk -f "$1"`, "kbimg", tomlPath)
	if runErr != nil {
		return fmt.Errorf("kbimg install disk failed: %w", runErr)
	}
	if err := i.verifyKubeOSInstall(device, out); err != nil {
		return err
	}
	log.Info("KubeOS install completed")

	// 6. Finish the way elemental install would.
	if install.EjectCD {
		if out, err := i.kubeos.runner.Output("eject", "-r"); err != nil {
			log.Warningf("eject-cd failed (continuing): %v\n%s", err, strings.TrimSpace(string(out)))
		}
	}
	switch {
	case install.PowerOff:
		log.Info("powering off as requested")
		if out, err := i.kubeos.runner.Output("systemctl", "poweroff"); err != nil {
			return fmt.Errorf("poweroff: %w\n%s", err, out)
		}
	case install.Reboot:
		log.Info("rebooting as requested")
		if out, err := i.kubeos.runner.Output("systemctl", "reboot"); err != nil {
			return fmt.Errorf("reboot: %w\n%s", err, out)
		}
	default:
		log.Info("KubeOS installed; reboot to start the installed system")
	}
	return nil
}

// stageKubeOSFiles writes every first-boot file under filesDir and returns the
// [[install.configs]] entries for them. Order is stable so the toml, and thus
// the install log, is reproducible.
func (i *installer) stageKubeOSFiles(filesDir string, config elementalv1.Config, state register.State, networkConfig elementalv1.NetworkConfig) ([]kubeosStagedFile, error) {
	var staged []kubeosStagedFile
	add := func(name, dst string, content []byte, mode os.FileMode) error {
		src := filepath.Join(filesDir, name)
		// kbimg copies with cp, which keeps the source mode bits; 0600 sources
		// are how the agent token and NetworkManager keyfiles arrive with the
		// permissions they need (NM refuses over-permissive keyfiles outright).
		if err := i.fs.WriteFile(src, content, mode); err != nil {
			return fmt.Errorf("staging %s: %w", dst, err)
		}
		staged = append(staged, kubeosStagedFile{src: src, dst: dst, mode: mode})
		return nil
	}

	// Agent runtime files: same bytes WriteLocalSystemAgentConfig writes.
	connectionInfo, err := i.getConnectionInfoBytes(config.Elemental)
	if err != nil {
		return nil, fmt.Errorf("getting connection info: %w", err)
	}
	if err := add("elemental_connection.json", filepath.Join(agentStateDir, "elemental_connection.json"), connectionInfo, 0o600); err != nil {
		return nil, err
	}
	agentConfig, err := i.getAgentConfigBytes()
	if err != nil {
		return nil, fmt.Errorf("getting agent config: %w", err)
	}
	if err := add("agent-config.yaml", filepath.Join(agentConfDir, "config.yaml"), agentConfig, 0o600); err != nil {
		return nil, err
	}
	if err := add("agent-envs", filepath.Join(agentConfDir, "envs"), i.getAgentConfigEnvs(config.Elemental), 0o600); err != nil {
		return nil, err
	}

	// Registration config and state, bare (not yip-wrapped): the installed
	// system's elemental-register.timer and the storage observer read them from
	// /etc/elemental/registration.
	registrationConfig, err := yaml.Marshal(elementalv1.Config{Elemental: elementalv1.Elemental{Registration: config.Elemental.Registration}})
	if err != nil {
		return nil, fmt.Errorf("marshalling registration config: %w", err)
	}
	if err := add("registration-config.yaml", kubeosRegistrationConfig, registrationConfig, 0o600); err != nil {
		return nil, err
	}
	registrationState, err := yaml.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshalling registration state: %w", err)
	}
	if err := add("registration-state.yaml", kubeosRegistrationState, registrationState, 0o600); err != nil {
		return nil, err
	}

	// cloud-init datasource pin.
	if err := add("99-baremetal-datasource.cfg", kubeosDatasourceCfgPath, []byte(kubeosDatasourceCfg), 0o644); err != nil {
		return nil, err
	}

	// NetworkManager profiles from the network applicator. Its yip already
	// carries each keyfile's target path and 0600 mode; only the transport
	// differs here.
	if len(networkConfig.Config) > 0 {
		applicator, err := i.networkConfigurator.GetNetworkConfigApplicator(networkConfig)
		if err != nil {
			return nil, fmt.Errorf("getting network config applicator: %w", err)
		}
		n := 0
		for _, stage := range applicator.Stages {
			for _, s := range stage {
				for _, f := range s.Files {
					n++
					if err := add(fmt.Sprintf("nm-%02d-%s", n, filepath.Base(f.Path)), f.Path, []byte(f.Content), 0o600); err != nil {
						return nil, err
					}
				}
			}
		}
		if n == 0 {
			log.Warning("network config produced no NetworkManager profiles; the installed system will fall back to DHCP")
		}
	}

	// Registry trust and credentials the live environment needed to pull the
	// rootfs are needed again by kbosctl upgrade in the installed system.
	if entries, err := i.fs.ReadDir(i.kubeos.liveRegistriesDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			content, err := i.fs.ReadFile(filepath.Join(i.kubeos.liveRegistriesDir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", e.Name(), err)
			}
			if err := add("registries-"+e.Name(), filepath.Join(kubeosLiveRegistriesDir, e.Name()), content, 0o644); err != nil {
				return nil, err
			}
		}
	}
	if content, err := i.fs.ReadFile(i.kubeos.liveAuthFile); err == nil && len(content) > 0 {
		if err := add("containers-auth.json", kubeosTargetAuthFile, content, 0o600); err != nil {
			return nil, err
		}
	}
	return staged, nil
}

// writeKubeOSToml copies the image's template, rewrites exactly three keys and
// appends the [[install.configs]] entries. The [disk_partition] block is left
// alone: it was computed for this rootfs at build time and a smaller root
// fails halfway through the install with "No space left on device".
func (i *installer) writeKubeOSToml(path, device, systemURI string, staged []kubeosStagedFile) error {
	template, err := i.fs.ReadFile(i.kubeos.tomlTemplate)
	if err != nil {
		return fmt.Errorf("reading kbimg template %s: %w", i.kubeos.tomlTemplate, err)
	}
	rewritten, err := rewriteKubeOSToml(template, device, systemURI)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	b.Write(rewritten)
	if len(rewritten) > 0 && rewritten[len(rewritten)-1] != '\n' {
		b.WriteByte('\n')
	}
	for _, f := range staged {
		// dst must be a full file path (kbimg feeds it to curl -o / cp) and a
		// local src must be a bare path, not file://.
		fmt.Fprintf(&b, "\n[[install.configs]]\nsrc = %q\ndst = %q\n", f.src, f.dst)
	}
	if err := i.fs.WriteFile(path, b.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// rewriteKubeOSToml is line-based on purpose: a TOML round-trip would reorder
// and reformat a file the OS builders maintain by hand.
func rewriteKubeOSToml(template []byte, device, systemURI string) ([]byte, error) {
	var out bytes.Buffer
	seenDisk, seenImage, seenReboot := false, false, false
	scanner := bufio.NewScanner(bytes.NewReader(template))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "target_disk"):
			fmt.Fprintf(&out, "target_disk = %q\n", device)
			seenDisk = true
		case strings.HasPrefix(trimmed, "oci_image"):
			fmt.Fprintf(&out, "oci_image = %q\n", systemURI)
			seenImage = true
		case strings.HasPrefix(trimmed, "reboot"):
			// The register process decides about rebooting after verification.
			out.WriteString("reboot = false\n")
			seenReboot = true
		default:
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading kbimg template: %w", err)
	}
	if !seenDisk || !seenImage {
		return nil, fmt.Errorf("kbimg template lacks target_disk or oci_image; refusing to guess the [install] block layout")
	}
	if !seenReboot {
		// Older templates may omit it; add under [install] is not safe without
		// parsing, so fail loudly rather than reboot mid-verification.
		return nil, fmt.Errorf("kbimg template lacks a reboot key; expected the shipped /root/kbimg.toml")
	}
	return out.Bytes(), nil
}

// verifyKubeOSInstall applies the four success criteria. kbimg's exit code is
// the least reliable of them.
func (i *installer) verifyKubeOSInstall(device string, installOutput []byte) error {
	if !bytes.Contains(installOutput, []byte(kubeosInstallSuccess)) {
		return fmt.Errorf("kbimg exited 0 but never printed %q; treating the install as failed", kubeosInstallSuccess)
	}
	out, err := i.kubeos.runner.Output("lsblk", "-rno", "LABEL", device)
	if err != nil {
		return fmt.Errorf("lsblk %s after install: %w\n%s", device, err, out)
	}
	labels := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		labels[l] = true
	}
	for _, want := range kubeosRequiredPartitions {
		if !labels[want] {
			return fmt.Errorf("partition %s not found on %s after install (labels: %s)", want, device, strings.TrimSpace(string(out)))
		}
	}
	// A grub.cfg that still says root=/dev/vdaN (the build VM's device name)
	// boots into "A start job is running for /dev/disk/by-partuuid/…" forever
	// on hardware. kbimg rewrites it to PARTUUID on success; confirm it did.
	out, err = i.kubeos.runner.Output("sh", "-c", kubeosGrubCheckScript, "grubcheck", device)
	if err != nil {
		return fmt.Errorf("grub root= check on %s: %w\n%s", device, err, strings.TrimSpace(string(out)))
	}
	if !bytes.Contains(out, []byte("root=PARTUUID=")) {
		return fmt.Errorf("installed grub.cfg on %s does not use root=PARTUUID= (got %q); the host would hang at boot", device, strings.TrimSpace(string(out)))
	}
	return nil
}

// kubeosGrubCheckScript mounts the BOOT partition read-only, prints every
// root= it finds in the EFI grub.cfg files, and unmounts. $1 is the disk.
const kubeosGrubCheckScript = `set -e
disk=$1
boot=$(lsblk -rno PATH,LABEL "$disk" | awk '$2=="BOOT"{print $1; exit}')
[ -n "$boot" ] || { echo "no BOOT partition on $disk" >&2; exit 1; }
mnt=$(mktemp -d)
trap 'umount "$mnt" 2>/dev/null; rmdir "$mnt" 2>/dev/null' EXIT
mount -o ro "$boot" "$mnt"
grep -rho 'root=[^ ]*' "$mnt"/EFI/*/grub.cfg 2>/dev/null | sort -u`

// kubeosImageReference normalises the catalog / registration value to what
// kbimg and kbosctl expect.
func kubeosImageReference(ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "docker://") {
		return ref
	}
	return "docker://" + strings.TrimPrefix(ref, "docker:")
}

func warnIgnoredKubeOSInstallFields(install elementalv1.Install) {
	ignored := []string{}
	if install.ISO != "" {
		ignored = append(ignored, "iso")
	}
	if install.Firmware != "" {
		ignored = append(ignored, "firmware")
	}
	if install.NoFormat {
		ignored = append(ignored, "no-format")
	}
	if install.Snapshotter.Type != "" {
		ignored = append(ignored, "snapshotter")
	}
	if install.DisableBootEntry {
		ignored = append(ignored, "disable-boot-entry")
	}
	if install.TTY != "" {
		ignored = append(ignored, "tty")
	}
	if install.ConfigDir != "" {
		ignored = append(ignored, "config-dir")
	}
	for _, f := range ignored {
		log.Warningf("kubeos install ignores elemental.install.%s", f)
	}
}
