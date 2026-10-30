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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace = "fleet-default"
	testVersion   = "sha256:3f2a000000000000000000000000000000000000000000000000000000000000"
	// base64("robot:fixture-secret")
	testAuth = "cm9ib3Q6Zml4dHVyZS1zZWNyZXQ="
)

type fixture struct {
	annotations map[string]string
	config      map[string]any
	creds       *corev1.Secret
}

func newFixture() *fixture {
	return &fixture{
		annotations: map[string]string{ModeAnnotation: "mirror", ConfigSecretAnnotation: "acp-registry-config-c1"},
		config: map[string]any{
			"schemaVersion": "v1", "mode": "mirror", "source": LogicalRegistry,
			"endpoints": []string{"harbor.example.com:11443", "backup.example.com"}, "clusterUID": "cluster-uid", "configVersion": testVersion,
			"caSecretRef":                   map[string]string{"name": "not-read"},
			"bootstrapCredentialsSecretRef": map[string]string{"name": "acp-registry-bootstrap-c1", "uid": "creds-uid", "resourceVersion": "8"},
		},
		creds: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "acp-registry-bootstrap-c1", UID: "creds-uid", ResourceVersion: "8"},
			Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"harbor.example.com:11443":{"auth":"` + testAuth + `"},"backup.example.com:443":{"username":"robot","password":"fixture-secret"}}}`)}},
	}
}

func (f *fixture) resolve(t *testing.T) (*Access, error) {
	t.Helper()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetNamespace(testNamespace)
	cluster.SetName("c1")
	cluster.SetUID("cluster-uid")
	cluster.SetAnnotations(f.annotations)
	body, _ := json.Marshal(f.config)
	config := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "acp-registry-config-c1"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"config.json": body}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, config, f.creds).Build()
	return Resolve(context.Background(), c, testNamespace, "c1")
}

func TestResolve(t *testing.T) {
	a, err := newFixture().resolve(t)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if a.Source != LogicalRegistry || strings.Join(a.Endpoints, ",") != "harbor.example.com:11443,backup.example.com" || a.ConfigVersion != testVersion ||
		a.CredentialsSecret != testNamespace+"/acp-registry-bootstrap-c1" {
		t.Fatalf("Resolve() = %s, creds %q", a, a.CredentialsSecret)
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

func TestResolveInvalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fixture)
		want   string
	}{
		{"not a mirror cluster", func(f *fixture) { delete(f.annotations, ModeAnnotation) }, "is not annotated cpaas.io/registry-mode=mirror"},
		{"no bootstrap Secret yet", func(f *fixture) { delete(f.annotations, ConfigSecretAnnotation) }, "has no cpaas.io/registry-config-secret annotation yet"},
		{"other cluster", func(f *fixture) { f.config["clusterUID"] = "other" }, `cluster UID "other"`},
		{"bad version", func(f *fixture) { f.config["configVersion"] = "v1" }, "configVersion"},
		{"duplicate endpoint", func(f *fixture) { f.config["endpoints"] = []string{"a.example.com", "a.example.com"} }, "duplicate"},
		{"unsafe endpoint", func(f *fixture) { f.config["endpoints"] = []string{"a.example.com\"\n[[registry]]"} }, "invalid"},
		{"sandbox reference", func(f *fixture) { f.config["sandboxCredentialsSecretRef"] = map[string]string{"name": "pause"} }, "sandboxCredentialsSecretRef"},
		{"credential rotated", func(f *fixture) { f.creds.UID = "new-uid" }, "uid/resourceVersion mismatch"},
		{"credential missing an endpoint", func(f *fixture) {
			f.creds.Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{"harbor.example.com:11443":{"auth":"` + testAuth + `"}}}`)
		}, "no entry for endpoint backup.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture()
			tt.mutate(f)
			_, err := f.resolve(t)
			var inv *InvalidError
			if !errors.As(err, &inv) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Resolve() error = %v, want an InvalidError containing %q", err, tt.want)
			}
		})
	}
}

func TestResolveMissingCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_, err := Resolve(context.Background(), fake.NewClientBuilder().WithScheme(scheme).Build(), testNamespace, "gone")
	var inv *InvalidError
	if !errors.As(err, &inv) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

