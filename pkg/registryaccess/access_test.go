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

package registryaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace = "cpaas-system"
	// base64("robot:fixture-secret")
	testAuth = "cm9ib3Q6Zml4dHVyZS1zZWNyZXQ="
)

func inputPayload(endpoints []string, creds string) map[string]any {
	p := map[string]any{
		"schemaVersion": "v1", "mode": "mirror", "source": LogicalRegistry, "endpoints": endpoints,
		"tlsPolicy":                   map[string]bool{"endpointInsecureSkipVerify": true},
		"caSecretRef":                 map[string]string{"name": "not-read"},
		"sandboxCredentialsSecretRef": map[string]string{"name": "not-read"},
	}
	if creds != "" {
		p["credentialsSecretRef"] = map[string]string{"name": creds, "uid": creds + "-uid", "resourceVersion": "8"}
	}
	return p
}

func credsSecret(name string, auths string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name, UID: "x", ResourceVersion: "8"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(auths)},
	}
}

func cloudCredential(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name, Labels: map[string]string{credentialProviderLabel: credentialProvider}},
		Type:       credentialType,
		Data:       data,
	}
}

func configJSON(p map[string]any) []byte {
	b, _ := json.Marshal(p)
	return b
}

const twoEndpointAuths = `{"auths":{"harbor.example.com:11443":{"auth":"` + testAuth + `"},"backup.example.com:443":{"username":"robot","password":"fixture-secret"}}}`

func resolve(t *testing.T, labels map[string]string, objs ...client.Object) (*Access, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if s, ok := o.(*corev1.Secret); ok && s.UID == "x" {
			s.UID = k8stypes.UID(s.Name + "-uid")
		}
	}
	return ResolveDefault(context.Background(), fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), testNamespace, labels)
}

func TestResolveDefaultCredential(t *testing.T) {
	endpoints := []string{"harbor.example.com:11443", "backup.example.com"}
	a, err := resolve(t, nil,
		cloudCredential(DefaultCredential, map[string][]byte{"registry": []byte("harbor.example.com:11443"), "config.json": configJSON(inputPayload(endpoints, "acp-registry-bootstrap-auth-1"))}),
		credsSecret("acp-registry-bootstrap-auth-1", twoEndpointAuths),
		// Not read: the default credential has mirror input.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: PreparedInput}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"config.json": []byte("{")}},
	)
	if err != nil {
		t.Fatalf("ResolveDefault() error = %v", err)
	}
	if a.Source != LogicalRegistry || strings.Join(a.Endpoints, ",") != strings.Join(endpoints, ",") ||
		a.Input != testNamespace+"/"+DefaultCredential || a.CredentialsSecret != testNamespace+"/acp-registry-bootstrap-auth-1" {
		t.Fatalf("ResolveDefault() = %s, creds %q", a, a.CredentialsSecret)
	}
	doc, err := a.DockerConfig()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Auths map[string]AuthEntry `json:"auths"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	// Keyed by endpoint, the backup credential found under backup.example.com:443.
	if parsed.Auths["harbor.example.com:11443"].Auth != testAuth || parsed.Auths["backup.example.com"].Username != "robot" || len(parsed.Auths) != 2 {
		t.Fatalf("DockerConfig() = %s", doc)
	}
	for _, printed := range []string{a.String(), fmt.Sprintf("%v", a), fmt.Sprintf("%+v", *a), fmt.Sprintf("%#v", a)} {
		if strings.Contains(printed, testAuth) || strings.Contains(printed, "fixture-secret") {
			t.Fatalf("printed Access leaks a credential: %s", printed)
		}
	}
}

func TestResolveReferencedCredential(t *testing.T) {
	a, err := resolve(t, map[string]string{ReferenceLabel: "team-registry"},
		cloudCredential("team-registry", map[string][]byte{"config.json": configJSON(inputPayload([]string{"team.example.com"}, "team-auth"))}),
		credsSecret("team-auth", `{"auths":{"team.example.com":{"auth":"`+testAuth+`"}}}`),
		cloudCredential(DefaultCredential, map[string][]byte{"config.json": configJSON(inputPayload([]string{"harbor.example.com"}, ""))}),
	)
	if err != nil || a.Input != testNamespace+"/team-registry" || strings.Join(a.Endpoints, ",") != "team.example.com" {
		t.Fatalf("ResolveDefault() = %v, %v", a, err)
	}
}

func TestResolvePreparedFallback(t *testing.T) {
	prepared := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: PreparedInput},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"config.json": configJSON(inputPayload([]string{"harbor.example.com:11443"}, "acp-registry-creds"))},
	}
	creds := credsSecret("acp-registry-creds", `{"auths":{"harbor.example.com:11443":{"auth":"`+testAuth+`"}}}`)
	for name, objs := range map[string][]client.Object{
		"placeholder":   {cloudCredential(DefaultCredential, nil), prepared, creds},
		"no credential": {prepared, creds},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := resolve(t, nil, objs...)
			if err != nil || a == nil || a.Input != testNamespace+"/"+PreparedInput || a.CredentialsSecret != testNamespace+"/acp-registry-creds" {
				t.Fatalf("ResolveDefault() = %v, %v", a, err)
			}
		})
	}
}

func TestResolveNoMirror(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"nothing":                 nil,
		"placeholder, no input":   {cloudCredential(DefaultCredential, nil)},
		"direct registry default": {cloudCredential(DefaultCredential, map[string][]byte{"registry": []byte("harbor.example.com")})},
	} {
		t.Run(name, func(t *testing.T) {
			if a, err := resolve(t, nil, objs...); a != nil || err != nil {
				t.Fatalf("ResolveDefault() = %v, %v; want no mirror input", a, err)
			}
		})
	}
}

func TestResolveInvalid(t *testing.T) {
	good := func() map[string]any {
		return inputPayload([]string{"harbor.example.com:11443", "backup.example.com"}, "auth")
	}
	auth := func() *corev1.Secret { return credsSecret("auth", twoEndpointAuths) }
	named := map[string]string{ReferenceLabel: "team-registry"}
	tests := []struct {
		name   string
		labels map[string]string
		objs   func() []client.Object
		want   string
	}{
		{"named credential missing", named, func() []client.Object { return nil }, "does not exist"},
		{"named credential placeholder", named, func() []client.Object { return []client.Object{cloudCredential("team-registry", nil)} }, "empty placeholder"},
		{"named credential without input", named, func() []client.Object {
			return []client.Object{cloudCredential("team-registry", map[string][]byte{"registry": []byte("x")})}
		}, "no registry mirror input"},
		{"wrong credential type", nil, func() []client.Object {
			s := cloudCredential(DefaultCredential, map[string][]byte{"config.json": configJSON(good())})
			s.Type = corev1.SecretTypeOpaque
			return []client.Object{s}
		}, "is not a CloudCredential"},
		{"not mirror", nil, func() []client.Object {
			p := good()
			p["mode"] = "direct"
			return []client.Object{cloudCredential(DefaultCredential, map[string][]byte{"config.json": configJSON(p)}), auth()}
		}, `mode "direct"`},
		{"duplicate endpoint", nil, func() []client.Object {
			p := good()
			p["endpoints"] = []string{"a.example.com", "a.example.com"}
			return []client.Object{cloudCredential(DefaultCredential, map[string][]byte{"config.json": configJSON(p)}), auth()}
		}, "duplicate"},
		{"unsafe endpoint", nil, func() []client.Object {
			p := good()
			p["endpoints"] = []string{"a.example.com\"\n[[registry]]"}
			return []client.Object{cloudCredential(DefaultCredential, map[string][]byte{"config.json": configJSON(p)}), auth()}
		}, "invalid"},
		{"credential rotated", nil, func() []client.Object {
			s := auth()
			s.UID = "new-uid"
			return []client.Object{cloudCredential(DefaultCredential, map[string][]byte{"config.json": configJSON(good())}), s}
		}, "uid/resourceVersion mismatch"},
		{"credential missing an endpoint", nil, func() []client.Object {
			return []client.Object{cloudCredential(DefaultCredential, map[string][]byte{"config.json": configJSON(good())}),
				credsSecret("auth", `{"auths":{"harbor.example.com:11443":{"auth":"`+testAuth+`"}}}`)}
		}, "no entry for endpoint backup.example.com"},
		{"prepared input invalid", nil, func() []client.Object {
			return []client.Object{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: PreparedInput}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"config.json": []byte("{")}}}
		}, "does not decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolve(t, tt.labels, tt.objs()...)
			var inv *InvalidError
			if !errors.As(err, &inv) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ResolveDefault() error = %v, want an InvalidError containing %q", err, tt.want)
			}
		})
	}
}
