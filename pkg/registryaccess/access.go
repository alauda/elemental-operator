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

// Package registryaccess reads the registry mirror input a live install
// pulls through: the real registry endpoints behind the logical registry
// cpaas-registry.cpaas-system and their pull credentials. The input is a
// CloudCredential the MachineRegistration selects with the
// cpaas.io/registry-reference label, as business clusters do, and
// public-registry-credential when the label is unset. Before the installer
// fills that default (a bootstrap cluster before POST), the prepared Global
// input acp-registry-config is used instead. External registry certificates
// are never verified, so the CA and the TLS policy are not read. An Access
// never prints its credentials.
package registryaccess

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// LogicalRegistry is the registry mirror source.
	LogicalRegistry = "cpaas-registry.cpaas-system"
	// ReferenceLabel names the CloudCredential an object pulls through, the
	// label business clusters carry for the same purpose.
	ReferenceLabel = "cpaas.io/registry-reference"
	// DefaultCredential is the platform's default registry credential.
	DefaultCredential = "public-registry-credential"
	// PreparedInput is the Global registry input the bootstrap cluster
	// prepares before the installer fills DefaultCredential.
	PreparedInput = "acp-registry-config"

	credentialType          = corev1.SecretType("CloudCredential")
	credentialProviderLabel = "capi.cpaas.io/provider"
	credentialProvider      = "registry-credential"
	maxEndpoints            = 16
)

// Access is a parsed registry mirror input.
type Access struct {
	Source    string
	Endpoints []string
	// Input and CredentialsSecret are namespace/name, for messages.
	Input             string
	CredentialsSecret string

	auths map[string]AuthEntry
}

// AuthEntry is one dockerconfigjson auths entry.
type AuthEntry struct {
	Auth          string `json:"auth,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	RegistryToken string `json:"registrytoken,omitempty"`
}

// String describes a without its credentials.
func (a Access) String() string {
	return fmt.Sprintf("registry %s via %s (input %s)", a.Source, strings.Join(a.Endpoints, ","), a.Input)
}

// GoString keeps %#v from printing the credentials too.
func (a Access) GoString() string { return a.String() }

// DockerConfig returns a dockerconfigjson document with the credentials of
// every endpoint, keyed by endpoint; an anonymous mirror yields empty auths.
func (a *Access) DockerConfig() ([]byte, error) {
	auths := map[string]AuthEntry{}
	for _, endpoint := range a.Endpoints {
		if entry, ok := lookupAuth(a.auths, endpoint); ok {
			auths[endpoint] = entry
		}
	}
	return json.Marshal(map[string]any{"auths": auths})
}

// InvalidError reports registry mirror input that is missing or does not
// validate; Message says which object or field.
type InvalidError struct{ Message string }

func (e *InvalidError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &InvalidError{Message: fmt.Sprintf(format, args...)}
}

type secretReference struct {
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// payload is the config.json of a registry mirror input, as minialauda
// writes acp-registry-config and the installer writes it into
// public-registry-credential. Fields an install does not need (TLS policy,
// CA, the SSH-only sandbox credentials) are ignored.
type payload struct {
	SchemaVersion        string           `json:"schemaVersion,omitempty"`
	Mode                 string           `json:"mode"`
	Source               string           `json:"source"`
	Endpoints            []string         `json:"endpoints"`
	CredentialsSecretRef *secretReference `json:"credentialsSecretRef,omitempty"`
}

// ResolveDefault returns the registry mirror input selected by labels in
// namespace: the CloudCredential named by ReferenceLabel, or
// DefaultCredential when the label is unset. For the default only, a
// placeholder without data, or no credential at all, falls back to
// PreparedInput. It returns nil without an error when the default selects no
// mirror input: a platform that does not pull through a mirror. An explicitly
// named credential must be a mirror input. Missing or invalid input is an
// *InvalidError; other errors are transient API errors.
func ResolveDefault(ctx context.Context, reader client.Reader, namespace string, labels map[string]string) (*Access, error) {
	name := strings.TrimSpace(labels[ReferenceLabel])
	explicit := name != ""
	if !explicit {
		name = DefaultCredential
	}
	ref := namespace + "/" + name
	credential := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, credential); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("read registry credential %s: %w", ref, err)
		}
		if explicit {
			return nil, invalid("registry credential %s named by %s does not exist", ref, ReferenceLabel)
		}
		return resolvePrepared(ctx, reader, namespace)
	}
	if credential.Type != credentialType || credential.Labels[credentialProviderLabel] != credentialProvider {
		return nil, invalid("registry credential %s is not a %s with %s=%s", ref, credentialType, credentialProviderLabel, credentialProvider)
	}
	switch {
	case len(credential.Data["config.json"]) > 0:
		return parseInput(ctx, reader, credential, ref)
	case explicit && len(credential.Data) == 0:
		return nil, invalid("registry credential %s named by %s is still an empty placeholder", ref, ReferenceLabel)
	case explicit:
		return nil, invalid("registry credential %s named by %s has no registry mirror input (config.json)", ref, ReferenceLabel)
	case len(credential.Data) == 0:
		// The chart placeholder before the installer fills it.
		return resolvePrepared(ctx, reader, namespace)
	default:
		// A filled credential without config.json: the platform pulls from a
		// direct registry, not through the mirror.
		return nil, nil
	}
}

func resolvePrepared(ctx context.Context, reader client.Reader, namespace string) (*Access, error) {
	ref := namespace + "/" + PreparedInput
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: PreparedInput}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read registry input %s: %w", ref, err)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return nil, invalid("registry input %s has type %s, want %s", ref, secret.Type, corev1.SecretTypeOpaque)
	}
	return parseInput(ctx, reader, secret, ref)
}

func parseInput(ctx context.Context, reader client.Reader, secret *corev1.Secret, ref string) (*Access, error) {
	var p payload
	if err := json.Unmarshal(secret.Data["config.json"], &p); err != nil {
		return nil, invalid("registry input %s: config.json does not decode: %v", ref, err)
	}
	switch {
	case p.SchemaVersion != "" && p.SchemaVersion != "v1":
		return nil, invalid("registry input %s: unsupported schemaVersion %q", ref, p.SchemaVersion)
	case p.Mode != "mirror":
		return nil, invalid("registry input %s: mode %q, want mirror", ref, p.Mode)
	case p.Source != LogicalRegistry:
		return nil, invalid("registry input %s: source %q, want %s", ref, p.Source, LogicalRegistry)
	case len(p.Endpoints) == 0 || len(p.Endpoints) > maxEndpoints:
		return nil, invalid("registry input %s: %d endpoints, want 1 to %d", ref, len(p.Endpoints), maxEndpoints)
	}
	seen := map[string]bool{}
	for _, endpoint := range p.Endpoints {
		if !ValidEndpoint(endpoint) || endpoint == p.Source || seen[endpoint] {
			return nil, invalid("registry input %s: endpoint %q is invalid, the logical source or a duplicate", ref, endpoint)
		}
		seen[endpoint] = true
	}
	a := &Access{Source: p.Source, Endpoints: append([]string(nil), p.Endpoints...), Input: ref}
	if cref := p.CredentialsSecretRef; cref != nil {
		if cref.Name == "" {
			return nil, invalid("registry input %s: credentialsSecretRef has no name", ref)
		}
		namespace := secret.Namespace
		creds := &corev1.Secret{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cref.Name}, creds); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, invalid("Secret %s/%s referenced by credentialsSecretRef of %s does not exist", namespace, cref.Name, ref)
			}
			return nil, fmt.Errorf("read Secret %s/%s: %w", namespace, cref.Name, err)
		}
		if (cref.UID != "" && string(creds.UID) != cref.UID) || (cref.ResourceVersion != "" && creds.ResourceVersion != cref.ResourceVersion) {
			return nil, invalid("Secret %s/%s changed after %s referenced it (uid/resourceVersion mismatch)", namespace, cref.Name, ref)
		}
		a.CredentialsSecret = namespace + "/" + cref.Name
		var err error
		if a.auths, err = parseAuths(creds, a.CredentialsSecret, p.Endpoints); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func parseAuths(secret *corev1.Secret, name string, endpoints []string) (map[string]AuthEntry, error) {
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		return nil, invalid("registry credential Secret %s has type %s, want %s", name, secret.Type, corev1.SecretTypeDockerConfigJson)
	}
	var config struct {
		Auths map[string]AuthEntry `json:"auths"`
	}
	if json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &config) != nil || config.Auths == nil {
		return nil, invalid("registry credential Secret %s: %s is not a dockerconfigjson with auths", name, corev1.DockerConfigJsonKey)
	}
	if len(config.Auths) == 0 {
		return config.Auths, nil
	}
	for _, endpoint := range endpoints {
		entry, ok := lookupAuth(config.Auths, endpoint)
		if !ok {
			return nil, invalid("registry credential Secret %s has no entry for endpoint %s", name, endpoint)
		}
		if entry.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil || !strings.Contains(string(raw), ":") {
				return nil, invalid("registry credential Secret %s: the entry for %s is not base64 user:password", name, endpoint)
			}
		} else if entry.Username == "" && entry.IdentityToken == "" && entry.RegistryToken == "" {
			return nil, invalid("registry credential Secret %s: the entry for %s is empty", name, endpoint)
		}
	}
	return config.Auths, nil
}

func lookupAuth(auths map[string]AuthEntry, endpoint string) (AuthEntry, bool) {
	if entry, ok := auths[endpoint]; ok {
		return entry, true
	}
	u, err := url.Parse("https://" + endpoint)
	if err != nil || u.Hostname() == "" {
		return AuthEntry{}, false
	}
	if u.Port() == "" {
		if entry, ok := auths[net.JoinHostPort(u.Hostname(), "443")]; ok {
			return entry, true
		}
	} else if u.Port() == "443" {
		host := u.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		if entry, ok := auths[host]; ok {
			return entry, true
		}
	}
	return AuthEntry{}, false
}

// ValidEndpoint reports whether endpoint is a bare host[:port] registry
// address that is safe in a registries.conf or a shell argument.
func ValidEndpoint(endpoint string) bool {
	if strings.ContainsAny(endpoint, "/\\?#@\"'\n\r\t ") || endpoint == "" {
		return false
	}
	u, err := url.Parse("https://" + endpoint)
	if err != nil || u.Host != endpoint || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		if len(host) > 253 || strings.Contains(host, ":") {
			return false
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return false
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
					return false
				}
			}
		}
	} else if strings.Contains(host, ":") && !strings.HasPrefix(endpoint, "[") {
		return false
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return false
		}
	} else if strings.HasSuffix(endpoint, ":") {
		return false
	}
	return true
}
