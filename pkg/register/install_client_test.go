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
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/tpm"
)

// installServer authenticates emulated TPMs like the operator and negotiates
// version. It acknowledges every message with MsgReady, records what each
// connection sent, and answers MsgGet and MsgInstallReport as told.
type installServer struct {
	auth    *tpm.AuthServer
	version MessageType
	// get answers MsgGet from the host with this EK hash.
	get func(hash string) (MessageType, []byte)
	// report answers MsgInstallReport.
	report func(InstallReport) (MessageType, []byte)

	mu       sync.Mutex
	conns    []*installConn
	finished int
}

// installConn is what one authenticated connection sent.
type installConn struct {
	hash     string
	messages []MessageType
	sessions []LiveInstallSession
	reports  []InstallReport
	// afterReply is what the host sent after the final answer.
	afterReply []MessageType
}

// newInstallServer starts an installServer that answers MsgGet with a
// configuration and MsgInstallReport with MsgReady, unless answers say
// otherwise.
func newInstallServer(version MessageType, answers ...func(*installServer)) (*installServer, *httptest.Server) {
	scheme := runtime.NewScheme()
	Expect(elementalv1.AddToScheme(scheme)).To(Succeed())
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	s := &installServer{
		auth:    tpm.New(context.Background(), fake.NewClientBuilder().WithScheme(scheme).Build()),
		version: version,
		get: func(string) (MessageType, []byte) {
			return MsgConfig, []byte("elemental:\n  install:\n    device: /dev/sda\n")
		},
		report: func(InstallReport) (MessageType, []byte) { return MsgReady, nil },
	}
	for _, answer := range answers {
		answer(s)
	}
	server := httptest.NewServer(s)
	DeferCleanup(server.Close)
	return s, server
}

func (s *installServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		s.mu.Lock()
		s.finished++
		s.mu.Unlock()
	}()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	inventory, _, err := s.auth.Authenticate(conn, r, "default")
	if err != nil || inventory == nil {
		return
	}
	rec := &installConn{hash: inventory.Spec.TPMHash}
	s.mu.Lock()
	s.conns = append(s.conns, rec)
	s.mu.Unlock()
	if WriteMessage(conn, MsgReady, nil) != nil {
		return
	}
	for {
		msgType, data, err := ReadMessage(conn)
		if err != nil {
			return
		}
		s.mu.Lock()
		rec.messages = append(rec.messages, msgType)
		s.mu.Unlock()
		var reply MessageType
		var payload []byte
		switch msgType {
		case MsgVersion:
			_ = WriteMessage(conn, MsgVersion, []byte{byte(s.version)})
			continue
		case MsgLiveInstallSession:
			session := LiveInstallSession{}
			if json.Unmarshal(data, &session) != nil {
				return
			}
			s.mu.Lock()
			rec.sessions = append(rec.sessions, session)
			s.mu.Unlock()
			_ = WriteMessage(conn, MsgReady, nil)
			continue
		case MsgGet:
			reply, payload = s.get(rec.hash)
		case MsgInstallReport:
			report := InstallReport{}
			if json.Unmarshal(data, &report) != nil {
				return
			}
			s.mu.Lock()
			rec.reports = append(rec.reports, report)
			s.mu.Unlock()
			reply, payload = s.report(report)
		default:
			_ = WriteMessage(conn, MsgReady, nil)
			continue
		}
		if WriteMessage(conn, reply, payload) != nil {
			return
		}
		// The host closes the session after the final answer; anything it
		// sends instead is recorded.
		if msgType, _, err := ReadMessage(conn); err == nil {
			s.mu.Lock()
			rec.afterReply = append(rec.afterReply, msgType)
			s.mu.Unlock()
		}
		return
	}
}

func answerGet(get func(hash string) (MessageType, []byte)) func(*installServer) {
	return func(s *installServer) { s.get = get }
}

func answerReport(report func(InstallReport) (MessageType, []byte)) func(*installServer) {
	return func(s *installServer) { s.report = report }
}

// connections waits until n connections are over and returns what they sent.
func (s *installServer) connections(n int) []*installConn {
	Eventually(func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.finished
	}).Should(Equal(n))
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func holdPayload(hold InstallHold) []byte {
	data, err := json.Marshal(hold)
	Expect(err).NotTo(HaveOccurred())
	return data
}

var _ = Describe("live install session", Label("registration", "install-session", "simulator"), func() {
	var hw *fakeHardwareTPM
	BeforeEach(func() {
		hw = &fakeHardwareTPM{probe: missingTPM}
		useFakeTPMHooks(hw)
	})
	registration := func(url string) elementalv1.Registration {
		return elementalv1.Registration{URL: url, Auth: "tpm", TPMMode: elementalv1.TPMModeAuto, EmulatedTPMSeed: -1, NoSMBIOS: true}
	}
	session := &LiveInstallSession{
		BootID:          "5d2b1f0e-9c4a-4e1b-8f3d-6a7c8b9d0e1f",
		Installer:       elementalv1.InstallerKubeOS,
		RegisterVersion: "v1.8.0-test",
		Candidates: []elementalv1.InstallCandidate{
			{ID: "wwn:5000c500000000a1", Path: "/dev/sda", SizeBytes: 480 << 30, Eligible: true},
			{ID: "wwn:5000c500000000b2", Path: "/dev/sdb", SizeBytes: 2000 << 30, StaleSystem: true, Eligible: true},
		},
		StaleSystemDisks: []elementalv1.StaleSystemDisk{
			{ID: "wwn:5000c500000000b2", Evidence: []string{"COS_STATE"}, Action: elementalv1.StaleSystemDiskWipe},
		},
	}

	It("sends the session after the network snapshot and right before MsgGet", func() {
		server, ws := newInstallServer(MsgLast)
		data, err := NewClient().RegisterForInstall(registration(ws.URL), nil, &State{}, session)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("/dev/sda"))
		conns := server.connections(1)
		Expect(conns[0].messages).To(Equal([]MessageType{MsgVersion, MsgAnnotations, MsgObservedNetworkConfig, MsgLiveInstallSession, MsgGet}))
		Expect(conns[0].sessions).To(Equal([]LiveInstallSession{*session}))
		Expect(conns[0].afterReply).To(BeEmpty())
	})

	It("registers without the session with an operator that predates it", func() {
		server, ws := newInstallServer(MsgObserveStorage)
		data, err := NewClient().RegisterForInstall(registration(ws.URL), nil, &State{}, session)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("/dev/sda"))
		Expect(server.connections(1)[0].messages).To(Equal([]MessageType{MsgVersion, MsgAnnotations, MsgObservedNetworkConfig, MsgGet}))
	})

	It("is never sent by Register", func() {
		server, ws := newInstallServer(MsgLast)
		_, err := NewClient().Register(registration(ws.URL), nil, &State{})
		Expect(err).NotTo(HaveOccurred())
		Expect(server.connections(1)[0].messages).To(Equal([]MessageType{MsgVersion, MsgAnnotations, MsgObservedNetworkConfig, MsgGet}))
	})

	It("returns a hold as InstallHoldError and does not answer it", func() {
		hold := InstallHold{Reason: elementalv1.AwaitingSelectionReason, Message: "pick the install disk", RetryAfterSeconds: 30}
		payload := holdPayload(hold)
		server, ws := newInstallServer(MsgLast, answerGet(func(string) (MessageType, []byte) { return MsgInstallHold, payload }))
		state := &State{}
		data, err := NewClient().RegisterForInstall(registration(ws.URL), nil, state, session)
		Expect(data).To(BeNil())
		var holdErr *InstallHoldError
		Expect(errors.As(err, &holdErr)).To(BeTrue(), "error %v", err)
		Expect(holdErr.Hold).To(Equal(hold))
		Expect(holdErr.RetryAfter()).To(Equal(30 * time.Second))
		Expect(err.Error()).To(ContainSubstring("install held by the operator: AwaitingSelection: pick the install disk"))
		conns := server.connections(1)
		Expect(conns[0].messages).To(Equal([]MessageType{MsgVersion, MsgAnnotations, MsgObservedNetworkConfig, MsgLiveInstallSession, MsgGet}))
		Expect(conns[0].afterReply).To(BeEmpty())
		// The TPM mode decision is kept for the next round.
		Expect(state.TPMMode).To(Equal(elementalv1.TPMModeEmulated))
		Expect(hw.probes).To(Equal(1))
	})

	It("moves to the next seed salt on an identity conflict, then takes the hold", func() {
		taken := emulatedHash(fakeDerivedSeed(0))
		conflict, err := yaml.Marshal(ErrorMessage{Message: "identity conflict", Code: ErrorCodeIdentityConflict})
		Expect(err).NotTo(HaveOccurred())
		hold := holdPayload(InstallHold{Reason: elementalv1.AwaitingSelectionReason})
		server, ws := newInstallServer(MsgLast, answerGet(func(hash string) (MessageType, []byte) {
			if hash == taken {
				return MsgError, conflict
			}
			return MsgInstallHold, hold
		}))
		state := &State{}
		_, err = NewClient().RegisterForInstall(registration(ws.URL), nil, state, session)
		var holdErr *InstallHoldError
		Expect(errors.As(err, &holdErr)).To(BeTrue(), "error %v", err)
		Expect(holdErr.RetryAfter()).To(Equal(DefaultInstallHoldSeconds * time.Second))
		conns := server.connections(2)
		Expect(conns[0].hash).To(Equal(taken))
		Expect(conns[1].hash).To(Equal(emulatedHash(fakeDerivedSeed(1))))
		for _, conn := range conns {
			Expect(conn.sessions).To(HaveLen(1))
		}
		Expect(state.EmulatedTPMSeedSalt).To(Equal(uint32(1)))
		Expect(state.EmulatedTPMSeed).To(Equal(fakeDerivedSeed(1)))
		Expect(hw.probes).To(Equal(1))
	})
})

var _ = Describe("install hold", Label("registration", "install-session"), func() {
	DescribeTable("waits as long as the operator asks, within bounds",
		func(seconds int, want time.Duration) {
			Expect((&InstallHoldError{Hold: InstallHold{RetryAfterSeconds: seconds}}).RetryAfter()).To(Equal(want))
		},
		Entry("the default when not said", 0, 15*time.Second),
		Entry("the default for nonsense", -3, 15*time.Second),
		Entry("what was asked", 42, 42*time.Second),
		Entry("the cap", 300, 5*time.Minute),
		Entry("no more than the cap", 3600, 5*time.Minute),
		Entry("no overflow", 1<<62, 5*time.Minute),
	)
	It("says why", func() {
		Expect((&InstallHoldError{Hold: InstallHold{Reason: "AwaitingSelection"}}).Error()).
			To(Equal("install held by the operator: AwaitingSelection"))
		Expect((&InstallHoldError{Hold: InstallHold{Reason: "SelectionDiscarded", Message: "set before the session started"}}).Error()).
			To(Equal("install held by the operator: SelectionDiscarded: set before the session started"))
	})
})

var _ = Describe("install report", Label("registration", "install-session", "simulator"), func() {
	var hw *fakeHardwareTPM
	BeforeEach(func() {
		hw = &fakeHardwareTPM{probe: missingTPM}
		useFakeTPMHooks(hw)
	})
	registration := func(url string) elementalv1.Registration {
		return elementalv1.Registration{URL: url, Auth: "tpm", TPMMode: elementalv1.TPMModeAuto, EmulatedTPMSeed: -1, NoSMBIOS: true}
	}
	// The state the install registration left behind.
	registered := func() *State {
		return &State{
			InitialRegistration:    time.Date(2026, time.October, 27, 8, 0, 0, 0, time.UTC),
			EmulatedTPM:            true,
			EmulatedTPMSeed:        fakeDerivedSeed(1),
			EmulatedTPMSeedVersion: tpm.SeedVersionV2,
			EmulatedTPMSeedSalt:    1,
			TPMMode:                elementalv1.TPMModeEmulated,
			TPMModeSource:          TPMModeSourceProbe,
		}
	}
	report := InstallReport{
		BootID:         "5d2b1f0e-9c4a-4e1b-8f3d-6a7c8b9d0e1f",
		Phase:          elementalv1.InstallPhaseFailed,
		TargetDeviceID: "wwn:5000c500000000a1",
		Reason:         "Blocked",
		Message:        "stale system disks cannot be wiped safely",
	}

	It("goes out on a short session with the registered identity", func() {
		server, ws := newInstallServer(MsgLast)
		state := registered()
		Expect(NewClient().ReportInstall(registration(ws.URL), nil, state, report)).To(Succeed())
		conns := server.connections(1)
		Expect(conns[0].hash).To(Equal(emulatedHash(fakeDerivedSeed(1))))
		Expect(conns[0].messages).To(Equal([]MessageType{MsgVersion, MsgInstallReport}))
		Expect(conns[0].reports).To(Equal([]InstallReport{report}))
		Expect(conns[0].afterReply).To(BeEmpty())
		Expect(*state).To(Equal(*registered()))
		Expect(hw.probes).To(BeZero())
	})

	It("leaves the state alone", func() {
		server, ws := newInstallServer(MsgLast)
		// A state from before tpm-mode: the authenticator fills the mode in.
		legacy := State{InitialRegistration: time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC), EmulatedTPM: true, EmulatedTPMSeed: 4242}
		state := legacy
		Expect(NewClient().ReportInstall(registration(ws.URL), nil, &state, report)).To(Succeed())
		Expect(server.connections(1)[0].hash).To(Equal(emulatedHash(4242)))
		Expect(state).To(Equal(legacy))
	})

	It("is skipped with an operator that predates it", func() {
		server, ws := newInstallServer(MsgObserveStorage)
		Expect(NewClient().ReportInstall(registration(ws.URL), nil, registered(), report)).To(Succeed())
		conns := server.connections(1)
		Expect(conns[0].messages).To(Equal([]MessageType{MsgVersion}))
		Expect(conns[0].reports).To(BeEmpty())
	})

	It("returns the operator's refusal", func() {
		payload, err := yaml.Marshal(ErrorMessage{Message: "no live install session for boot 5d2b1f0e"})
		Expect(err).NotTo(HaveOccurred())
		server, ws := newInstallServer(MsgLast, answerReport(func(InstallReport) (MessageType, []byte) { return MsgError, payload }))
		err = NewClient().ReportInstall(registration(ws.URL), nil, registered(), report)
		Expect(err).To(MatchError(ContainSubstring("install report refused: no live install session for boot 5d2b1f0e")))
		var regErr *RegistrationError
		Expect(errors.As(err, &regErr)).To(BeTrue())
		Expect(server.connections(1)[0].reports).To(HaveLen(1))
	})
})
