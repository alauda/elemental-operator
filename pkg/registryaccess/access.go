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

// Package registryaccess reads the registry bootstrap Secret that cluster
// transformer (or the installer, for a self-hosted Global) publishes for a
// CAPI Cluster: the real registry endpoints behind the logical registry
// cpaas-registry.cpaas-system and their pull credentials. The rules are the
// ones of cluster-api-provider-baremetal's internal/registryaccess. External
// registry certificates are never verified, so the CA and the TLS policy are
// not read. An Access never prints its credentials.
package registryaccess

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// LogicalRegistry is the registry mirror source.
	LogicalRegistry = "cpaas-registry.cpaas-system"
	// ConfigSecretAnnotation names the bootstrap Secret on the CAPI Cluster.
	ConfigSecretAnnotation = "cpaas.io/registry-config-secret"
	// ModeAnnotation is "mirror" on a cluster that pulls through the mirror.
	ModeAnnotation = "cpaas.io/registry-mode"

	maxEndpoints = 16
)

var (
	configVersionPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	clusterGVK           = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "Cluster"}
)

// Access is the parsed bootstrap Secret of a mirror cluster.
type Access struct {
	Source        string
	Endpoints     []string
	ConfigVersion string
	// ConfigSecret and CredentialsSecret are namespace/name, for messages.
	ConfigSecret      string
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
	return fmt.Sprintf("registry %s via %s (config %s, %s)", a.Source, strings.Join(a.Endpoints, ","), a.ConfigVersion, a.ConfigSecret)
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

// InvalidError reports registry bootstrap input that is missing or does not
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

type payload struct {
	SchemaVersion                 string           `json:"schemaVersion,omitempty"`
	Mode                          string           `json:"mode"`
	Source                        string           `json:"source"`
	Endpoints                     []string         `json:"endpoints"`
	ClusterUID                    string           `json:"clusterUID"`
	ConfigVersion                 string           `json:"configVersion"`
	SandboxCredentialsSecretRef   *secretReference `json:"sandboxCredentialsSecretRef,omitempty"`
	BootstrapCredentialsSecretRef *secretReference `json:"bootstrapCredentialsSecretRef,omitempty"`
}

// Resolve returns the Access of the CAPI Cluster namespace/name. A missing
// Cluster, a Cluster without mirror input or invalid input is an
// *InvalidError: a MachineRegistration that names a cluster expects one.
// Other errors are transient API errors.
func Resolve(ctx context.Context, reader client.Reader, namespace, name string) (*Access, error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, invalid("CAPI Cluster %s/%s does not exist", namespace, name)
		}
		return nil, fmt.Errorf("read CAPI Cluster %s/%s: %w", namespace, name, err)
	}
	annotations := cluster.GetAnnotations()
	if annotations[ModeAnnotation] != "mirror" {
		return nil, invalid("CAPI Cluster %s/%s is not annotated %s=mirror", namespace, name, ModeAnnotation)
	}
	configName := strings.TrimSpace(annotations[ConfigSecretAnnotation])
	if configName == "" {
		return nil, invalid("CAPI Cluster %s/%s has no %s annotation yet", namespace, name, ConfigSecretAnnotation)
	}
	configRef := namespace + "/" + configName
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: configName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, invalid("registry bootstrap Secret %s does not exist", configRef)
		}
		return nil, fmt.Errorf("read registry bootstrap Secret %s: %w", configRef, err)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return nil, invalid("registry bootstrap Secret %s has type %s, want %s", configRef, secret.Type, corev1.SecretTypeOpaque)
	}
	var p payload
	if err := json.Unmarshal(secret.Data["config.json"], &p); err != nil {
		return nil, invalid("registry bootstrap Secret %s: config.json does not decode: %v", configRef, err)
	}
	switch {
	case p.SchemaVersion != "" && p.SchemaVersion != "v1":
		return nil, invalid("registry bootstrap Secret %s: unsupported schemaVersion %q", configRef, p.SchemaVersion)
	case p.Mode != "mirror":
		return nil, invalid("registry bootstrap Secret %s: mode %q, want mirror", configRef, p.Mode)
	case p.Source != LogicalRegistry:
		return nil, invalid("registry bootstrap Secret %s: source %q, want %s", configRef, p.Source, LogicalRegistry)
	case string(cluster.GetUID()) == "" || p.ClusterUID != string(cluster.GetUID()):
		return nil, invalid("registry bootstrap Secret %s belongs to cluster UID %q, not %s/%s", configRef, p.ClusterUID, namespace, name)
	case !configVersionPattern.MatchString(p.ConfigVersion):
		return nil, invalid("registry bootstrap Secret %s: configVersion %q is not sha256:<64 hex>", configRef, p.ConfigVersion)
	case len(p.Endpoints) == 0 || len(p.Endpoints) > maxEndpoints:
		return nil, invalid("registry bootstrap Secret %s: %d endpoints, want 1 to %d", configRef, len(p.Endpoints), maxEndpoints)
	case p.SandboxCredentialsSecretRef != nil:
		return nil, invalid("registry bootstrap Secret %s carries sandboxCredentialsSecretRef, which only SSH nodes take", configRef)
	}
	seen := map[string]bool{}
	for _, endpoint := range p.Endpoints {
		if !ValidEndpoint(endpoint) || endpoint == p.Source || seen[endpoint] {
			return nil, invalid("registry bootstrap Secret %s: endpoint %q is invalid, the logical source or a duplicate", configRef, endpoint)
		}
		seen[endpoint] = true
	}
	a := &Access{Source: p.Source, Endpoints: append([]string(nil), p.Endpoints...), ConfigVersion: p.ConfigVersion, ConfigSecret: configRef}
	if ref := p.BootstrapCredentialsSecretRef; ref != nil {
		if ref.Name == "" {
			return nil, invalid("registry bootstrap Secret %s: bootstrapCredentialsSecretRef has no name", configRef)
		}
		creds := &corev1.Secret{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, creds); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, invalid("Secret %s/%s referenced by bootstrapCredentialsSecretRef of %s does not exist", namespace, ref.Name, configRef)
			}
			return nil, fmt.Errorf("read Secret %s/%s: %w", namespace, ref.Name, err)
		}
		if (ref.UID != "" && string(creds.UID) != ref.UID) || (ref.ResourceVersion != "" && creds.ResourceVersion != ref.ResourceVersion) {
			return nil, invalid("Secret %s/%s changed after %s referenced it (uid/resourceVersion mismatch)", namespace, ref.Name, configRef)
		}
		a.CredentialsSecret = namespace + "/" + ref.Name
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
