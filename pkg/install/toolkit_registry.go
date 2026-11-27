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
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/twpayne/go-vfs"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/elementalcli"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/registryaccess"
)

// Toolkit install of install.system-uri.
//
// The operator gives an install that pulls a system-uri the registry it
// pulls through when the platform pulls through a mirror: the logical
// registry, its real endpoints in order, and a dockerconfigjson with their
// credentials (install-registry). The KubeOS backend hands that to skopeo and
// kbimg as a registries.conf.d mirror; the toolkit pulls with
// go-containerregistry instead, which reads credentials from
// $DOCKER_CONFIG/config.json and knows nothing of registries.conf. So for the
// toolkit the register:
//
//   - writes the dockerconfigjson to a root-only directory on tmpfs and runs
//     elemental install with DOCKER_CONFIG pointing at it;
//   - pulls a system-uri that names a real registry as it is, and rewrites
//     one under the logical registry to the first endpoint that serves it;
//   - checks that the image can be pulled (a manifest HEAD) before any disk
//     is touched: the toolkit partitions first and pulls afterwards;
//   - passes --tls-verify=false: registry certificates are never verified,
//     and the same image as the recovery system (elementalcli);
//   - removes the credentials once elemental install returns, so the
//     installed system never gets them.
//
// Without install-registry the system-uri is pulled anonymously, as written,
// with the same check before the disk is touched and without certificate
// verification either.

const (
	toolkitRegistryDockerDir = "/run/elemental/registry-docker"
	// toolkitImageProbeTimeout bounds each request of the pull check.
	toolkitImageProbeTimeout = 30 * time.Second
	// dockerHubRegistry is how references name Docker Hub, and
	// dockerHubAuthKey how a dockerconfigjson keys its credentials.
	dockerHubRegistry = "docker.io"
	dockerHubAuthKey  = "index.docker.io"
	dockerHubAPIHost  = "registry-1.docker.io"
)

// toolkitOptions are the injectable knobs of the toolkit registry pull.
type toolkitOptions struct {
	// dockerConfigDir is where the install's registry credentials are
	// written; empty means toolkitRegistryDockerDir.
	dockerConfigDir string
}

func defaultToolkitOptions() toolkitOptions {
	return toolkitOptions{dockerConfigDir: toolkitRegistryDockerDir}
}

func (i *installer) toolkitDockerConfigDir() string {
	if i.toolkit.dockerConfigDir != "" {
		return i.toolkit.dockerConfigDir
	}
	return toolkitRegistryDockerDir
}

// prepareSystemImagePull prepares the toolkit to pull install.system-uri,
// through reg when the operator sent one: it writes the credentials,
// rewrites a logical system-uri to the first endpoint that serves the image
// and checks that the image can be pulled. It returns the options elemental
// install runs with and a function that removes the credentials. Without a
// system-uri that names an image it changes nothing.
func (i *installer) prepareSystemImagePull(install *elementalv1.Install, reg *elementalv1.InstallRegistry) (elementalcli.InstallOptions, func(), error) {
	noop := func() {}
	if !pullsImage(install.SystemURI) {
		return elementalcli.InstallOptions{}, noop, nil
	}
	ref, err := parseImageReference(install.SystemURI)
	if err != nil {
		return elementalcli.InstallOptions{}, nil, fmt.Errorf("install.system-uri %q: %w", install.SystemURI, err)
	}
	if reg == nil {
		return i.pullAnonymously(install, ref)
	}
	if err := validateInstallRegistry(reg); err != nil {
		return elementalcli.InstallOptions{}, nil, err
	}
	auths, err := parseDockerAuths(reg.DockerConfig)
	if err != nil {
		return elementalcli.InstallOptions{}, nil, fmt.Errorf("install-registry docker-config: %w", err)
	}
	candidates := []imageReference{ref}
	if ref.registry == reg.Source {
		candidates = candidates[:0]
		for _, endpoint := range reg.Endpoints {
			candidates = append(candidates, ref.on(endpoint))
		}
	}

	dir := i.toolkitDockerConfigDir()
	cleanup := func() {
		if err := i.fs.RemoveAll(dir); err != nil {
			log.Warningf("Removing the install registry credentials %s: %v", dir, err)
		}
	}
	rawDir, err := i.writeToolkitDockerConfig(dir, reg.DockerConfig)
	if err != nil {
		cleanup()
		return elementalcli.InstallOptions{}, nil, err
	}

	chosen, err := newImageProbe().first(candidates, auths)
	if err != nil {
		cleanup()
		return elementalcli.InstallOptions{}, nil, fmt.Errorf("the system image %s cannot be pulled, nothing was written to disk: %w", ref, err)
	}
	if chosen.String() != ref.String() {
		log.Infof("Pulling the system image %s from %s", ref, chosen.registry)
	}
	log.Infof("The system image %s can be pulled; installing it through the registry mirror %s", chosen, reg.Source)
	install.SystemURI = "docker:" + chosen.String()
	return elementalcli.InstallOptions{DockerConfig: rawDir, SkipTLSVerify: true}, cleanup, nil
}

// pullAnonymously prepares the pull of a system-uri the operator sent no
// install-registry for: the platform does not pull through a mirror. The
// image is pulled as written, without credentials or certificate
// verification, once it was seen to be there.
func (i *installer) pullAnonymously(install *elementalv1.Install, ref imageReference) (elementalcli.InstallOptions, func(), error) {
	if ref.registry == registryaccess.LogicalRegistry {
		return elementalcli.InstallOptions{}, nil, fmt.Errorf("install.system-uri %s is under %s, but the operator sent no install-registry to reach it through: write the platform registry address instead", ref, registryaccess.LogicalRegistry)
	}
	if _, err := newImageProbe().first([]imageReference{ref}, dockerAuths{}); err != nil {
		return elementalcli.InstallOptions{}, nil, fmt.Errorf("the system image %s cannot be pulled, nothing was written to disk: %w", ref, err)
	}
	log.Infof("The system image %s can be pulled; installing it", ref)
	install.SystemURI = "docker:" + ref.String()
	return elementalcli.InstallOptions{SkipTLSVerify: true}, func() {}, nil
}

// writeToolkitDockerConfig writes config as dir/config.json, readable by root
// only, and returns the directory's path on the host.
func (i *installer) writeToolkitDockerConfig(dir, config string) (string, error) {
	if config == "" {
		config = `{"auths":{}}`
	}
	if err := vfs.MkdirAll(i.fs, dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	// MkdirAll keeps the mode of a directory left over from an earlier run.
	if err := i.fs.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("restricting %s: %w", dir, err)
	}
	if err := i.fs.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("writing the install registry credentials: %w", err)
	}
	return i.fs.RawPath(dir)
}

// validateInstallRegistry checks the install-registry the operator sent.
func validateInstallRegistry(reg *elementalv1.InstallRegistry) error {
	if reg.Source == "" || len(reg.Endpoints) == 0 {
		return errors.New("install-registry needs a source and endpoints")
	}
	for _, endpoint := range append([]string{reg.Source}, reg.Endpoints...) {
		if !registryaccess.ValidEndpoint(endpoint) {
			return fmt.Errorf("install-registry names an invalid registry %q", endpoint)
		}
	}
	return nil
}

// pullsImage tells whether a system-uri names a registry image, as opposed
// to nothing (the live system itself) or a directory or file.
func pullsImage(systemURI string) bool {
	uri := strings.TrimSpace(systemURI)
	return uri != "" && !strings.HasPrefix(uri, "dir:") && !strings.HasPrefix(uri, "file:")
}

// imageReference is a registry image reference split the way the registry
// API needs it.
type imageReference struct {
	// registry is host[:port] as written, docker.io when none is.
	registry   string
	repository string
	tag        string
	digest     string
}

// parseImageReference parses a system-uri: an image reference with an
// optional docker: or oci: transport, the forms the toolkit accepts.
func parseImageReference(uri string) (imageReference, error) {
	ref := strings.TrimSpace(uri)
	for _, transport := range []string{"docker://", "docker:", "oci://", "oci:"} {
		if strings.HasPrefix(ref, transport) {
			ref = strings.TrimPrefix(ref, transport)
			break
		}
	}
	if ref == "" || strings.ContainsAny(ref, " \t\r\n\"'\\?#") {
		return imageReference{}, errors.New("not an image reference")
	}
	name, digest, err := splitDigest(ref)
	if err != nil {
		return imageReference{}, err
	}
	parsed := imageReference{digest: digest}
	if parsed.registry, name, err = splitRegistry(name); err != nil {
		return imageReference{}, err
	}
	if colon := strings.LastIndex(name, ":"); colon >= 0 && !strings.Contains(name[colon:], "/") {
		name, parsed.tag = name[:colon], name[colon+1:]
		if parsed.tag == "" {
			return imageReference{}, errors.New("the tag is empty")
		}
	}
	if parsed.tag == "" && parsed.digest == "" {
		parsed.tag = "latest"
	}
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return imageReference{}, fmt.Errorf("repository %q is malformed", name)
	}
	parsed.repository = name
	return parsed, nil
}

// splitDigest splits name@algorithm:hex.
func splitDigest(ref string) (string, string, error) {
	name, digest, found := strings.Cut(ref, "@")
	if !found {
		return ref, "", nil
	}
	if algorithm, hex, ok := strings.Cut(digest, ":"); !ok || algorithm == "" || hex == "" {
		return "", "", fmt.Errorf("digest %q is not <algorithm>:<hex>", digest)
	}
	return name, digest, nil
}

// splitRegistry splits the registry off a name the way Docker does: the first
// path component names one when it has a dot or a port, or is localhost.
func splitRegistry(name string) (string, string, error) {
	slash := strings.Index(name, "/")
	if slash > 0 && (strings.ContainsAny(name[:slash], ".:") || name[:slash] == "localhost") {
		registry := name[:slash]
		if !registryaccess.ValidEndpoint(registry) {
			return "", "", fmt.Errorf("registry %q is not a host[:port]", registry)
		}
		return registry, name[slash+1:], nil
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	return dockerHubRegistry, name, nil
}

func (r imageReference) String() string {
	s := r.registry + "/" + r.repository
	if r.tag != "" {
		s += ":" + r.tag
	}
	if r.digest != "" {
		s += "@" + r.digest
	}
	return s
}

// on is the same image in another registry.
func (r imageReference) on(registry string) imageReference {
	r.registry = registry
	return r
}

func (r imageReference) manifest() string {
	if r.digest != "" {
		return r.digest
	}
	return r.tag
}

func (r imageReference) apiHost() string {
	if r.registry == dockerHubRegistry {
		return dockerHubAPIHost
	}
	return r.registry
}

// dockerAuth is one dockerconfigjson auths entry.
type dockerAuth struct {
	Auth          string `json:"auth,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	RegistryToken string `json:"registrytoken,omitempty"`
}

// credentials are the username and password of the entry.
func (a dockerAuth) credentials() (string, string, error) {
	if a.Auth == "" {
		return a.Username, a.Password, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(a.Auth)
	if err != nil {
		return "", "", errors.New("an auth entry is not base64")
	}
	username, password, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", "", errors.New("an auth entry is not username:password")
	}
	return username, password, nil
}

// dockerAuths are the credentials of a dockerconfigjson, keyed by registry
// host the way go-containerregistry looks them up: without a scheme or path.
type dockerAuths map[string]dockerAuth

func parseDockerAuths(config string) (dockerAuths, error) {
	if strings.TrimSpace(config) == "" {
		return dockerAuths{}, nil
	}
	var doc struct {
		Auths map[string]dockerAuth `json:"auths"`
	}
	if err := json.Unmarshal([]byte(config), &doc); err != nil {
		return nil, fmt.Errorf("not a dockerconfigjson: %w", err)
	}
	auths := dockerAuths{}
	for key, entry := range doc.Auths {
		auths[authHost(key)] = entry
	}
	return auths, nil
}

// authHost strips the scheme and path of a dockerconfigjson auths key.
func authHost(key string) string {
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	host, _, _ := strings.Cut(key, "/")
	return host
}

func (a dockerAuths) lookup(registry string) (dockerAuth, bool) {
	if registry == dockerHubRegistry {
		registry = dockerHubAuthKey
	}
	entry, found := a[registry]
	return entry, found
}

// imageProbe checks that an image can be pulled the way the toolkit pulls it
// with --tls-verify=false: HTTPS without certificate verification, plain HTTP
// when HTTPS cannot be spoken, the credentials of the registry host.
type imageProbe struct {
	client *http.Client
}

func newImageProbe() *imageProbe {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- install-registry endpoints are never verified.
	return &imageProbe{client: &http.Client{Transport: transport, Timeout: toolkitImageProbeTimeout}}
}

// first returns the first of candidates that can be pulled.
func (p *imageProbe) first(candidates []imageReference, auths dockerAuths) (imageReference, error) {
	var failures []string
	for _, candidate := range candidates {
		err := p.check(candidate, auths)
		if err == nil {
			return candidate, nil
		}
		log.Warningf("The system image %s cannot be pulled: %v", candidate, err)
		failures = append(failures, fmt.Sprintf("%s: %v", candidate, err))
	}
	return imageReference{}, errors.New(strings.Join(failures, "; "))
}

// statusError is a registry's answer that the image cannot be pulled.
type statusError struct{ message string }

func (e *statusError) Error() string { return e.message }

// check reports why ref cannot be pulled, nil when its manifest is there.
func (p *imageProbe) check(ref imageReference, auths dockerAuths) error {
	var failures []string
	for _, scheme := range []string{"https", "http"} {
		err := p.checkOver(scheme, ref, auths)
		if err == nil {
			return nil
		}
		var answered *statusError
		if errors.As(err, &answered) {
			return err
		}
		failures = append(failures, err.Error())
	}
	return errors.New(strings.Join(failures, "; "))
}

var manifestMediaTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

func (p *imageProbe) checkOver(scheme string, ref imageReference, auths dockerAuths) error {
	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, ref.apiHost(), ref.repository, ref.manifest())
	status, challenge, err := p.head(manifestURL, "")
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		entry, found := auths.lookup(ref.registry)
		authorization, err := p.authorize(challenge, ref, entry, found)
		if err != nil {
			return err
		}
		if status, _, err = p.head(manifestURL, authorization); err != nil {
			return err
		}
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return &statusError{"the registry has no such manifest (HTTP 404)"}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &statusError{fmt.Sprintf("the registry refuses the credentials install-registry has for %s (HTTP %d)", ref.registry, status)}
	default:
		return &statusError{fmt.Sprintf("the registry answered HTTP %d", status)}
	}
}

func (p *imageProbe) head(target, authorization string) (int, string, error) {
	request, err := http.NewRequest(http.MethodHead, target, nil)
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Accept", manifestMediaTypes)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	return response.StatusCode, response.Header.Get("WWW-Authenticate"), nil
}

// authorize answers the challenge of a registry with the credentials of the
// registry host, if any.
func (p *imageProbe) authorize(challenge string, ref imageReference, entry dockerAuth, found bool) (string, error) {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if !found {
			return "", &statusError{fmt.Sprintf("the registry asks for credentials, and install-registry has none for %s", ref.registry)}
		}
		username, password, err := entry.credentials()
		if err != nil {
			return "", fmt.Errorf("the credentials of %s: %w", ref.registry, err)
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password)), nil
	case "bearer":
		token, err := p.token(params, ref, entry, found)
		if err != nil {
			return "", err
		}
		return "Bearer " + token, nil
	default:
		return "", &statusError{fmt.Sprintf("the registry asks for unsupported authentication %q", challenge)}
	}
}

// token gets a pull token from the registry's token service.
func (p *imageProbe) token(params map[string]string, ref imageReference, entry dockerAuth, found bool) (string, error) {
	if found && entry.RegistryToken != "" {
		return entry.RegistryToken, nil
	}
	request, err := tokenRequest(params, ref, entry, found)
	if err != nil {
		return "", err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", &statusError{fmt.Sprintf("the registry's token service refused a pull token for %s (HTTP %d)", ref.repository, response.StatusCode)}
	}
	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &answer) != nil || (answer.Token == "" && answer.AccessToken == "") {
		return "", &statusError{"the registry's token service answered no token"}
	}
	if answer.Token != "" {
		return answer.Token, nil
	}
	return answer.AccessToken, nil
}

// tokenRequest asks the token service of a Bearer challenge for a pull
// token: with the refresh token of the credentials when they have one,
// otherwise with their username and password.
func tokenRequest(params map[string]string, ref imageReference, entry dockerAuth, found bool) (*http.Request, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || params["realm"] == "" || (realm.Scheme != "https" && realm.Scheme != "http") {
		return nil, &statusError{fmt.Sprintf("the registry's token realm %q is unusable", params["realm"])}
	}
	scope := params["scope"]
	if scope == "" {
		scope = fmt.Sprintf("repository:%s:pull", ref.repository)
	}
	if found && entry.IdentityToken != "" {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {entry.IdentityToken}, "service": {params["service"]}, "scope": {scope}, "client_id": {"elemental-register"}}
		request, err := http.NewRequest(http.MethodPost, realm.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return request, nil
	}
	query := realm.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	query.Set("scope", scope)
	realm.RawQuery = query.Encode()
	request, err := http.NewRequest(http.MethodGet, realm.String(), nil)
	if err != nil {
		return nil, err
	}
	if !found {
		return request, nil
	}
	username, password, err := entry.credentials()
	if err != nil {
		return nil, fmt.Errorf("the credentials of %s: %w", ref.registry, err)
	}
	if username != "" || password != "" {
		request.SetBasicAuth(username, password)
	}
	return request, nil
}

// parseChallenge splits a WWW-Authenticate challenge into its scheme and
// parameters: Bearer realm="https://h/token",service="h",scope="...".
func parseChallenge(challenge string) (string, map[string]string) {
	challenge = strings.TrimSpace(challenge)
	scheme, rest, _ := strings.Cut(challenge, " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; rest = strings.TrimLeft(strings.TrimSpace(rest), ",") {
		key, value, found := strings.Cut(rest, "=")
		if !found {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			end := strings.Index(value[1:], `"`)
			if end < 0 {
				params[key] = value[1:]
				break
			}
			params[key], rest = value[1:1+end], value[2+end:]
			continue
		}
		value, rest, _ = strings.Cut(value, ",")
		params[key] = strings.TrimSpace(value)
	}
	return scheme, params
}
