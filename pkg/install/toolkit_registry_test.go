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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/twpayne/go-vfs/vfst"
	"go.uber.org/mock/gomock"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/elementalcli"
	climocks "github.com/rancher/elemental-operator/pkg/elementalcli/mocks"
	networkmocks "github.com/rancher/elemental-operator/pkg/network/mocks"
)

const (
	registryUser     = "robot$install"
	registryPassword = "s3cret"
	registryToken    = "pull-token"
	systemRepository = "tkestack/baremetal-base-image"
	systemTag        = "alaudaos-45.m55.202611201030-0.x86_64"
	logicalRegistry  = "cpaas-registry.cpaas-system"
)

// fakeRegistry serves manifests of the images it holds, the way a registry
// behind a token service (Harbor) or HTTP basic authentication does.
type fakeRegistry struct {
	server *httptest.Server
	// auth is "bearer", "basic" or "" (anonymous).
	auth   string
	images map[string]bool

	mu       sync.Mutex
	requests []string
}

func newFakeRegistry(tlsServer bool, auth string, images ...string) *fakeRegistry {
	r := &fakeRegistry{auth: auth, images: map[string]bool{}}
	for _, image := range images {
		r.images[image] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		if user, password, ok := req.BasicAuth(); !ok || user != registryUser || password != registryPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": registryToken})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		repository, reference, found := strings.Cut(strings.TrimPrefix(req.URL.Path, "/v2/"), "/manifests/")
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.auth {
		case "bearer":
			if req.Header.Get("Authorization") != "Bearer "+registryToken {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="harbor-registry",scope="repository:%s:pull"`, r.server.URL, repository))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case "basic":
			if user, password, ok := req.BasicAuth(); !ok || user != registryUser || password != registryPassword {
				w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		if req.Method != http.MethodHead || !strings.Contains(req.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !r.images[repository+":"+reference] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.WriteHeader(http.StatusOK)
	})
	if tlsServer {
		r.server = httptest.NewTLSServer(mux)
	} else {
		r.server = httptest.NewServer(mux)
	}
	DeferCleanup(r.server.Close)
	return r
}

func (r *fakeRegistry) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)
}

func (r *fakeRegistry) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

// host is the registry's host:port.
func (r *fakeRegistry) host() string {
	return strings.TrimPrefix(strings.TrimPrefix(r.server.URL, "https://"), "http://")
}

// deadEndpoint is a host:port nothing listens on.
func deadEndpoint() string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).ToNot(HaveOccurred())
	address := listener.Addr().String()
	Expect(listener.Close()).To(Succeed())
	return address
}

func dockerConfigFor(hosts ...string) string {
	auths := map[string]map[string]string{}
	for _, host := range hosts {
		auths[host] = map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + registryPassword))}
	}
	data, _ := json.Marshal(map[string]any{"auths": auths})
	return string(data)
}

var systemImage = systemRepository + ":" + systemTag

var _ = Describe("toolkit install through install-registry", Label("installer", "install", "registry"), func() {
	var fs *vfst.TestFS
	var cliRunner *climocks.MockRunner
	var networkConfigurator *networkmocks.MockConfigurator
	var host *fakeHost
	var inst *installer
	var config elementalv1.Config
	const dockerDir = "/run/elemental/registry-docker"

	BeforeEach(func() {
		host = twoDisks()
		config = configFixture
		config.Elemental.Install.SystemURI = ""
	})
	JustBeforeEach(func() {
		var cleanup func()
		var err error
		fs, cleanup, err = vfst.NewTestFS(merge(host.files(), map[string]interface{}{"/tmp/init": "", "/run/init": ""}))
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(cleanup)
		mockCtrl := gomock.NewController(GinkgoT())
		cliRunner = climocks.NewMockRunner(mockCtrl)
		networkConfigurator = networkmocks.NewMockConfigurator(mockCtrl)
		inst = &installer{
			fs:                  fs,
			runner:              cliRunner,
			networkConfigurator: networkConfigurator,
			disk:                host.diskOptions(),
			toolkit:             toolkitOptions{dockerConfigDir: dockerDir},
		}
	})

	rawDockerDir := func() string {
		raw, err := fs.RawPath(dockerDir)
		Expect(err).ToNot(HaveOccurred())
		return raw
	}
	expectHook := func() {
		networkConfigurator.EXPECT().GetNetworkConfigApplicator(networkConfigFixture).Return(networkConfigApplicatorFixture, nil)
	}
	// expectInstall expects elemental install of systemURI with the
	// credentials of the install, and checks them while it runs.
	expectInstall := func(systemURI string, credentials string) *gomock.Call {
		want := config.Elemental.Install
		want.SystemURI = systemURI
		return cliRunner.EXPECT().Install(want, elementalcli.InstallOptions{DockerConfig: rawDockerDir(), SkipTLSVerify: true}).
			DoAndReturn(func(elementalv1.Install, elementalcli.InstallOptions) error {
				dir, err := fs.Stat(dockerDir)
				Expect(err).ToNot(HaveOccurred())
				Expect(dir.Mode().Perm()).To(Equal(os.FileMode(0o700)))
				file, err := fs.Stat(filepath.Join(dockerDir, "config.json"))
				Expect(err).ToNot(HaveOccurred())
				Expect(file.Mode().Perm()).To(Equal(os.FileMode(0o600)))
				written, err := fs.ReadFile(filepath.Join(dockerDir, "config.json"))
				Expect(err).ToNot(HaveOccurred())
				Expect(string(written)).To(Equal(credentials))
				return nil
			})
	}
	expectNoCredentials := func() {
		_, err := fs.Stat(dockerDir)
		Expect(err).To(MatchError(os.ErrNotExist))
	}

	Context("with a system-uri that names a real registry", func() {
		for _, form := range []string{"%s", "docker:%s", "docker://%s"} {
			It(fmt.Sprintf("pulls it as written (%q) with the credentials of that registry", form), func() {
				registry := newFakeRegistry(true, "bearer", systemImage)
				image := registry.host() + "/" + systemImage
				config.Elemental.Install.SystemURI = fmt.Sprintf(form, image)
				credentials := dockerConfigFor(registry.host(), "backup.example.com")
				config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{Source: logicalRegistry, Endpoints: []string{registry.host(), "backup.example.com"}, DockerConfig: credentials}
				expectHook()
				expectInstall("docker:"+image, credentials)

				Expect(inst.InstallElemental(config, stateFixture, networkConfigFixture)).To(Succeed())
				Expect(registry.seen()).To(ContainElements("HEAD /v2/"+systemRepository+"/manifests/"+systemTag, "GET /token"))
				expectNoCredentials()
			})
		}
	})

	Context("with a system-uri under the logical registry", func() {
		It("rewrites it to the first endpoint that serves the image", func() {
			missing := newFakeRegistry(true, "bearer")
			serving := newFakeRegistry(true, "bearer", systemImage)
			dead := deadEndpoint()
			config.Elemental.Install.SystemURI = "docker:" + logicalRegistry + "/" + systemImage
			credentials := dockerConfigFor(missing.host(), serving.host())
			config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{Source: logicalRegistry, Endpoints: []string{dead, missing.host(), serving.host()}, DockerConfig: credentials}
			expectHook()
			expectInstall("docker:"+serving.host()+"/"+systemImage, credentials)

			Expect(inst.InstallElemental(config, stateFixture, networkConfigFixture)).To(Succeed())
			Expect(missing.seen()).To(ContainElement("HEAD /v2/" + systemRepository + "/manifests/" + systemTag))
			expectNoCredentials()
		})
		It("speaks plain HTTP to an endpoint without TLS", func() {
			plain := newFakeRegistry(false, "basic", systemImage)
			config.Elemental.Install.SystemURI = logicalRegistry + "/" + systemImage
			credentials := dockerConfigFor(plain.host())
			config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{Source: logicalRegistry, Endpoints: []string{plain.host()}, DockerConfig: credentials}
			expectHook()
			expectInstall("docker:"+plain.host()+"/"+systemImage, credentials)

			Expect(inst.InstallElemental(config, stateFixture, networkConfigFixture)).To(Succeed())
		})
		It("pulls anonymously from a mirror without credentials", func() {
			anonymous := newFakeRegistry(true, "", systemImage)
			config.Elemental.Install.SystemURI = logicalRegistry + "/" + systemImage
			config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{Source: logicalRegistry, Endpoints: []string{anonymous.host()}}
			expectHook()
			expectInstall("docker:"+anonymous.host()+"/"+systemImage, `{"auths":{}}`)

			Expect(inst.InstallElemental(config, stateFixture, networkConfigFixture)).To(Succeed())
		})
	})

	Context("in a live environment with a stale system disk", func() {
		BeforeEach(func() {
			host.live = true
			host.disk("sda").elementalLayout()
			config.Elemental.Install.Device = "/dev/sdb"
			config.Elemental.Install.NoFormat = false
		})
		It("touches no disk and runs no install when no endpoint serves the image", func() {
			missing := newFakeRegistry(true, "bearer")
			dead := deadEndpoint()
			config.Elemental.Install.SystemURI = logicalRegistry + "/" + systemImage
			config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{Source: logicalRegistry, Endpoints: []string{dead, missing.host()}, DockerConfig: dockerConfigFor(missing.host())}

			err := inst.InstallElemental(config, stateFixture, networkConfigFixture)
			Expect(err).To(MatchError(ContainSubstring("cannot be pulled, nothing was written to disk")))
			Expect(err).To(MatchError(ContainSubstring(dead + "/" + systemImage + ": ")))
			Expect(err).To(MatchError(ContainSubstring(missing.host() + "/" + systemImage + ": the registry has no such manifest (HTTP 404)")))
			Expect(destructive(host.calls)).To(BeEmpty())
			_, statErr := fs.Stat(filepath.Join(elementalcli.TempCloudInitDir, elementalBeforeHook))
			Expect(statErr).To(MatchError(os.ErrNotExist))
			expectNoCredentials()
		})
		It("touches no disk when the registry refuses the credentials", func() {
			registry := newFakeRegistry(true, "bearer", systemImage)
			config.Elemental.Install.SystemURI = registry.host() + "/" + systemImage
			wrong := `{"auths":{"` + registry.host() + `":{"username":"robot$install","password":"wrong"}}}`
			config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{Source: logicalRegistry, Endpoints: []string{registry.host()}, DockerConfig: wrong}

			err := inst.InstallElemental(config, stateFixture, networkConfigFixture)
			Expect(err).To(MatchError(ContainSubstring("token service refused a pull token")))
			Expect(destructive(host.calls)).To(BeEmpty())
			expectNoCredentials()
		})
		It("removes the credentials when elemental install fails", func() {
			registry := newFakeRegistry(true, "bearer", systemImage)
			config.Elemental.Install.SystemURI = registry.host() + "/" + systemImage
			config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{Source: logicalRegistry, Endpoints: []string{registry.host()}, DockerConfig: dockerConfigFor(registry.host())}
			expectHook()
			expectInstall("docker:"+registry.host()+"/"+systemImage, dockerConfigFor(registry.host())).Return(errors.New("grub2-install failed"))

			Expect(inst.InstallElemental(config, stateFixture, networkConfigFixture)).To(MatchError(ContainSubstring("grub2-install failed")))
			// The pull check came before the wipe.
			Expect(destructive(host.calls)).ToNot(BeEmpty())
			expectNoCredentials()
		})
	})

	It("leaves an install without install-registry as it is", func() {
		config.Elemental.Install.SystemURI = "registry.example.com/" + systemImage
		expectHook()
		cliRunner.EXPECT().Install(config.Elemental.Install, elementalcli.InstallOptions{}).Return(nil)
		Expect(inst.InstallElemental(config, stateFixture, networkConfigFixture)).To(Succeed())
		expectNoCredentials()
	})
	It("leaves an install from the live system as it is", func() {
		config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{Source: logicalRegistry, Endpoints: []string{"harbor.example.com"}}
		expectHook()
		cliRunner.EXPECT().Install(config.Elemental.Install, elementalcli.InstallOptions{}).Return(nil)
		Expect(inst.InstallElemental(config, stateFixture, networkConfigFixture)).To(Succeed())
		expectNoCredentials()
	})
	It("refuses an invalid install-registry before it touches anything", func() {
		config.Elemental.Install.SystemURI = logicalRegistry + "/" + systemImage
		config.Elemental.InstallRegistry = &elementalv1.InstallRegistry{Source: logicalRegistry, Endpoints: []string{"harbor.example.com/evil path"}}
		Expect(inst.InstallElemental(config, stateFixture, networkConfigFixture)).To(MatchError(ContainSubstring("invalid registry")))
		expectNoCredentials()
	})
})

var _ = Describe("image references", Label("registry"), func() {
	DescribeTable("parses the system-uri forms the toolkit accepts",
		func(uri, registry, repository, tag, digest, rendered string) {
			ref, err := parseImageReference(uri)
			Expect(err).ToNot(HaveOccurred())
			Expect(ref).To(Equal(imageReference{registry: registry, repository: repository, tag: tag, digest: digest}))
			Expect(ref.String()).To(Equal(rendered))
		},
		Entry("bare", "10.64.16.40:11443/tkestack/base:v1", "10.64.16.40:11443", "tkestack/base", "v1", "", "10.64.16.40:11443/tkestack/base:v1"),
		Entry("docker:", "docker:harbor.example.com/tkestack/base:v1", "harbor.example.com", "tkestack/base", "v1", "", "harbor.example.com/tkestack/base:v1"),
		Entry("docker://", " docker://harbor.example.com/tkestack/base:v1 ", "harbor.example.com", "tkestack/base", "v1", "", "harbor.example.com/tkestack/base:v1"),
		Entry("oci:", "oci:harbor.example.com/base:v1", "harbor.example.com", "base", "v1", "", "harbor.example.com/base:v1"),
		Entry("logical", "cpaas-registry.cpaas-system/tkestack/base:v1", logicalRegistry, "tkestack/base", "v1", "", "cpaas-registry.cpaas-system/tkestack/base:v1"),
		Entry("no tag", "harbor.example.com/tkestack/base", "harbor.example.com", "tkestack/base", "latest", "", "harbor.example.com/tkestack/base:latest"),
		Entry("digest", "localhost:5000/base@sha256:abcd", "localhost:5000", "base", "", "sha256:abcd", "localhost:5000/base@sha256:abcd"),
		Entry("tag and digest", "harbor.example.com/base:v1@sha256:abcd", "harbor.example.com", "base", "v1", "sha256:abcd", "harbor.example.com/base:v1@sha256:abcd"),
		Entry("Docker Hub", "busybox", dockerHubRegistry, "library/busybox", "latest", "", "docker.io/library/busybox:latest"),
	)
	DescribeTable("rejects what is not an image reference",
		func(uri string) {
			_, err := parseImageReference(uri)
			Expect(err).To(HaveOccurred())
		},
		Entry("empty", "docker:"),
		Entry("space", "harbor.example.com/base:v1 --evil"),
		Entry("empty tag", "harbor.example.com/base:"),
		Entry("bad digest", "harbor.example.com/base@sha256"),
		Entry("bad registry", "harbor_example.com:x/base:v1"),
		Entry("empty repository", "harbor.example.com/"),
	)
	It("rewrites a reference onto an endpoint", func() {
		ref, err := parseImageReference("cpaas-registry.cpaas-system/tkestack/base:v1")
		Expect(err).ToNot(HaveOccurred())
		Expect(ref.on("10.0.0.1:11443").String()).To(Equal("10.0.0.1:11443/tkestack/base:v1"))
	})
	It("parses registry challenges", func() {
		scheme, params := parseChallenge(`Bearer realm="https://h:11443/service/token",service="harbor-registry",scope="repository:a/b:pull,push"`)
		Expect(scheme).To(Equal("Bearer"))
		Expect(params).To(Equal(map[string]string{"realm": "https://h:11443/service/token", "service": "harbor-registry", "scope": "repository:a/b:pull,push"}))
		scheme, params = parseChallenge(`Basic realm=registry`)
		Expect(scheme).To(Equal("Basic"))
		Expect(params).To(Equal(map[string]string{"realm": "registry"}))
	})
	It("finds credentials the way go-containerregistry keys them", func() {
		auths, err := parseDockerAuths(`{"auths":{"https://index.docker.io/v1/":{"auth":"dTpw"},"10.0.0.1:11443":{"username":"u","password":"p"}}}`)
		Expect(err).ToNot(HaveOccurred())
		hub, found := auths.lookup(dockerHubRegistry)
		Expect(found).To(BeTrue())
		user, password, err := hub.credentials()
		Expect(err).ToNot(HaveOccurred())
		Expect(user + ":" + password).To(Equal("u:p"))
		_, found = auths.lookup("10.0.0.1:11443")
		Expect(found).To(BeTrue())
		// No port equivalence: go-containerregistry would not match either.
		_, found = auths.lookup("10.0.0.1")
		Expect(found).To(BeFalse())
	})
})
