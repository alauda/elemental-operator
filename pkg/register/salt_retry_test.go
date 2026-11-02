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

package register

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gotpm "github.com/rancher-sandbox/go-tpm"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/tpm"
)

// conflictServer authenticates emulated TPMs exactly like the operator
// (tpm.AuthServer, attestation included) and then answers MsgGet with an
// error for the EK hashes it is told to reject.
type conflictServer struct {
	auth     *tpm.AuthServer
	rejected func(hash string) bool
	code     string

	mu          sync.Mutex
	hashes      []string
	annotations []map[string]string
}

func newConflictServer(code string, rejected func(hash string) bool) (*conflictServer, *httptest.Server) {
	scheme := runtime.NewScheme()
	Expect(elementalv1.AddToScheme(scheme)).To(Succeed())
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	s := &conflictServer{
		auth:     tpm.New(context.Background(), fake.NewClientBuilder().WithScheme(scheme).Build()),
		rejected: rejected,
		code:     code,
	}
	server := httptest.NewServer(s)
	DeferCleanup(server.Close)
	return s, server
}

func (s *conflictServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	inventory, _, err := s.auth.Authenticate(conn, r, "default")
	if err != nil || inventory == nil {
		return
	}
	hash := inventory.Spec.TPMHash
	s.mu.Lock()
	s.hashes = append(s.hashes, hash)
	s.mu.Unlock()
	if WriteMessage(conn, MsgReady, nil) != nil {
		return
	}
	for {
		msgType, data, err := ReadMessage(conn)
		if err != nil {
			return
		}
		switch msgType {
		case MsgVersion:
			// Below MsgObservedNetworkConfig: no host network snapshot.
			_ = WriteMessage(conn, MsgVersion, []byte{byte(MsgSystemDataV2)})
		case MsgAnnotations:
			annotations := map[string]string{}
			_ = json.Unmarshal(data, &annotations)
			s.mu.Lock()
			s.annotations = append(s.annotations, annotations)
			s.mu.Unlock()
			_ = WriteMessage(conn, MsgReady, nil)
		case MsgGet:
			if s.rejected(hash) {
				payload, _ := yaml.Marshal(ErrorMessage{Message: "identity conflict: MachineInventory default/other belongs to other hardware", Code: s.code})
				_ = WriteMessage(conn, MsgError, payload)
				return
			}
			_ = WriteMessage(conn, MsgConfig, []byte("elemental:\n  install:\n    device: /dev/sda\n"))
			return
		default:
			_ = WriteMessage(conn, MsgReady, nil)
		}
	}
}

func (s *conflictServer) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hashes)
}

func emulatedHash(seed int64) string {
	hash, err := gotpm.GetPubHash(gotpm.Emulated, gotpm.WithSeed(seed))
	Expect(err).ToNot(HaveOccurred())
	return hash
}

var _ = Describe("seed salt retries", Label("registration", "tpm-mode", "simulator"), func() {
	var hw *fakeHardwareTPM
	BeforeEach(func() {
		hw = &fakeHardwareTPM{probe: missingTPM}
		useFakeTPMHooks(hw)
	})
	registration := func(url string) elementalv1.Registration {
		return elementalv1.Registration{URL: url, Auth: "tpm", TPMMode: elementalv1.TPMModeAuto, EmulatedTPMSeed: -1, NoSMBIOS: true}
	}

	It("derives the next salt after an identity conflict and keeps the one that worked", func() {
		taken := map[string]bool{emulatedHash(fakeDerivedSeed(0)): true, emulatedHash(fakeDerivedSeed(1)): true}
		server, ws := newConflictServer(ErrorCodeIdentityConflict, func(hash string) bool { return taken[hash] })
		state := &State{}

		data, err := NewClient().Register(registration(ws.URL), nil, state)
		Expect(err).ToNot(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("/dev/sda"))
		Expect(server.attempts()).To(Equal(3))
		Expect(hw.probes).To(Equal(1))
		Expect(state.TPMMode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(state.TPMModeSource).To(Equal(TPMModeSourceProbe))
		Expect(state.EmulatedTPM).To(BeTrue())
		Expect(state.EmulatedTPMSeed).To(Equal(fakeDerivedSeed(2)))
		Expect(state.EmulatedTPMSeedVersion).To(Equal(tpm.SeedVersionV2))
		Expect(state.EmulatedTPMSeedSalt).To(Equal(uint32(2)))
		Expect(state.InitialRegistration).ToNot(BeZero())
		for salt, annotations := range server.annotations {
			Expect(annotations).To(HaveKeyWithValue("auth", "emulated-tpm"))
			Expect(annotations).To(HaveKeyWithValue(TPMSeedVersionAnnotation, "2"))
			Expect(annotations).To(HaveKeyWithValue("tpm-seed-salt", []string{"0", "1", "2"}[salt]))
		}
	})
	It("gives up after the last salt", func() {
		server, ws := newConflictServer(ErrorCodeIdentityConflict, func(string) bool { return true })
		state := &State{}
		_, err := NewClient().Register(registration(ws.URL), nil, state)
		Expect(err).To(MatchError(ContainSubstring("giving up")))
		var regErr *RegistrationError
		Expect(errors.As(err, &regErr)).To(BeTrue())
		Expect(regErr.Code).To(Equal(ErrorCodeIdentityConflict))
		Expect(server.attempts()).To(Equal(MaxSeedSaltRetries + 1))
	})
	It("never retries a static seed", func() {
		server, ws := newConflictServer(ErrorCodeIdentityConflict, func(string) bool { return true })
		reg := registration(ws.URL)
		reg.EmulatedTPMSeed = 42
		_, err := NewClient().Register(reg, nil, &State{})
		Expect(err).To(MatchError(ContainSubstring("identity conflict")))
		Expect(server.attempts()).To(Equal(1))
	})
	It("never retries for a registered host", func() {
		server, ws := newConflictServer(ErrorCodeIdentityConflict, func(string) bool { return true })
		state := &State{
			InitialRegistration:    time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC),
			EmulatedTPM:            true,
			EmulatedTPMSeed:        fakeDerivedSeed(0),
			EmulatedTPMSeedVersion: tpm.SeedVersionV2,
			TPMMode:                elementalv1.TPMModeEmulated,
			TPMModeSource:          TPMModeSourceProbe,
		}
		_, err := NewClient().Register(registration(ws.URL), nil, state)
		Expect(err).To(HaveOccurred())
		Expect(server.attempts()).To(Equal(1))
		Expect(state.EmulatedTPMSeed).To(Equal(fakeDerivedSeed(0)))
	})
	It("does not retry other refusals", func() {
		for _, code := range []string{"", ErrorCodeIdentityRebindRequired} {
			server, ws := newConflictServer(code, func(string) bool { return true })
			_, err := NewClient().Register(registration(ws.URL), nil, &State{})
			Expect(err).To(HaveOccurred())
			Expect(server.attempts()).To(Equal(1), "code %q", code)
		}
	})
})
