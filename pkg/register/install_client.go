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
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
)

// The client side of the live install session. A host booted from the live
// ISO with --install sends its session (boot, installer, disks) right before
// MsgGet. With install.device-selection: inventory the operator answers
// MsgInstallHold until a person picks the install disk, and the host
// registers again after the wait the operator asked for, touching no disk in
// between. A released host that fails to install says so with MsgInstallReport
// on a short session of its own.

// maxInstallHoldWait caps the wait an operator can ask for, so a bogus value
// cannot park a host for long.
const maxInstallHoldWait = 5 * time.Minute

// InstallHoldError is the operator's answer to a live install session that
// has no install target yet: the host must not touch any disk and registers
// again after RetryAfter.
type InstallHoldError struct {
	Hold InstallHold
}

func (e *InstallHoldError) Error() string {
	msg := "install held by the operator: " + e.Hold.Reason
	if e.Hold.Message != "" {
		msg += ": " + e.Hold.Message
	}
	return msg
}

// RetryAfter is the wait the operator asked for, DefaultInstallHoldSeconds
// when it did not say, and at most maxInstallHoldWait.
func (e *InstallHoldError) RetryAfter() time.Duration {
	seconds := e.Hold.RetryAfterSeconds
	if seconds <= 0 {
		seconds = DefaultInstallHoldSeconds
	}
	if seconds > int(maxInstallHoldWait/time.Second) {
		return maxInstallHoldWait
	}
	return time.Duration(seconds) * time.Second
}

// RegisterForInstall is Register for a host booted from the live ISO with
// --install: when the operator speaks the live install protocol, the session
// goes out right before the configuration is requested. The operator then
// either releases the host (the configuration, as from Register) or holds
// it (*InstallHoldError).
func (r *client) RegisterForInstall(reg elementalv1.Registration, caCert []byte, state *State, session *LiveInstallSession) ([]byte, error) {
	return registerWithSaltRetries(state, func() ([]byte, *tpmIdentity, error) {
		return r.register(reg, caCert, state, session)
	})
}

func (r *client) sendLiveInstallSession(conn *websocket.Conn, protoVersion MessageType, session *LiveInstallSession) error {
	if protoVersion < MsgLiveInstallSession {
		// An operator without install sessions installs like it always did.
		if !r.sessionUnsupportedLogged {
			log.Infof("elemental-operator protocol version %d has no live install session; registering without one", protoVersion)
			r.sessionUnsupportedLogged = true
		}
		return nil
	}
	log.Infof("Send live install session: boot %s, %d disks, %d stale system disks",
		session.BootID, len(session.Candidates), len(session.StaleSystemDisks))
	if err := SendJSONData(conn, MsgLiveInstallSession, session); err != nil {
		return fmt.Errorf("failed to send live install session: %w", err)
	}
	return nil
}

// installHoldError decodes a MsgInstallHold reply. The host does not answer
// it; the session ends there.
func installHoldError(data []byte) error {
	hold := InstallHold{}
	if err := json.Unmarshal(data, &hold); err != nil {
		return fmt.Errorf("decoding install hold: %w", err)
	}
	return &InstallHoldError{Hold: hold}
}

// ReportInstall reports the install outcome of a released host on a short
// session of its own: authenticate, negotiate, report. It authenticates with
// the identity the registration used, never deriving another one, and leaves
// the state as it is. An operator that predates install reports is skipped.
func (r *client) ReportInstall(reg elementalv1.Registration, caCert []byte, state *State, report InstallReport) error {
	reportState := State{}
	if state != nil {
		reportState = *state
	}
	auth, err := getAuthenticator(reg, &reportState)
	if err != nil {
		return fmt.Errorf("initializing authenticator: %w", err)
	}

	log.Infof("Connect to %s to report the install", reg.URL)
	conn, err := initWebsocketConn(reg.URL, caCert, auth)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := authenticate(conn, auth); err != nil {
		return fmt.Errorf("%s authentication failed: %w", auth.GetName(), err)
	}
	protoVersion, err := negotiateProtoVersion(conn)
	if err != nil {
		return fmt.Errorf("failed to negotiate protocol version: %w", err)
	}
	log.Debugf("Negotiated protocol version: %d", protoVersion)
	if protoVersion < MsgInstallReport {
		log.Infof("elemental-operator protocol version %d takes no install reports; not reporting the install as %s", protoVersion, report.Phase)
		return nil
	}

	log.Infof("Report the install as %s", report.Phase)
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encoding install report: %w", err)
	}
	if err := WriteMessage(conn, MsgInstallReport, data); err != nil {
		return fmt.Errorf("sending install report: %w", err)
	}
	msgType, reply, err := ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("reading install report reply: %w", err)
	}
	switch msgType {
	case MsgReady:
		return nil
	case MsgError:
		msg := &ErrorMessage{}
		if err := yaml.Unmarshal(reply, msg); err != nil {
			return fmt.Errorf("decoding install report error: %w", err)
		}
		return fmt.Errorf("install report refused: %w", &RegistrationError{Code: msg.Code, Message: msg.Message})
	default:
		return fmt.Errorf("unexpected install report reply: %s", msgType)
	}
}
