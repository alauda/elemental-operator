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

package tpm

import (
	"github.com/gorilla/websocket"
	gotpm "github.com/rancher-sandbox/go-tpm"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
)

type AttestationChannel struct {
	conn *websocket.Conn
}

func (att *AttestationChannel) Read(p []byte) (int, error) {
	_, r, err := att.conn.NextReader()
	if err != nil {
		return 0, err
	}
	return r.Read(p)
}

func (att *AttestationChannel) Write(p []byte) (int, error) {
	w, err := att.conn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return 0, err
	}
	defer w.Close()

	return w.Write(p)
}

type AuthClient struct {
	emulateTPM bool
	seed       int64
	ak         []byte
}

func NewAuthClient(seed int64) *AuthClient {
	return &AuthClient{
		seed: seed,
	}
}

func (auth *AuthClient) Init(reg elementalv1.Registration) error {
	if reg.EmulateTPM {
		log.Info("Enable TPM emulation")
		auth.emulateTPM = true
	}
	return nil
}

func (auth *AuthClient) Authenticate(conn *websocket.Conn) error {
	var opts []gotpm.Option
	if auth.emulateTPM {
		opts = append(opts, gotpm.Emulated)
		opts = append(opts, gotpm.WithSeed(auth.seed))
	}

	return gotpm.Authenticate(auth.ak, &AttestationChannel{conn}, opts...)
}

func (auth *AuthClient) GetName() string {
	return "TPM"
}

func (auth *AuthClient) GetToken() (string, error) {
	var opts []gotpm.Option
	if auth.emulateTPM {
		opts = append(opts, gotpm.Emulated)
		opts = append(opts, gotpm.WithSeed(auth.seed))
	}
	token, akBytes, err := gotpm.GetAuthToken(opts...)
	if err != nil {
		return "", err
	}
	auth.ak = akBytes

	return token, nil
}

func (auth *AuthClient) GetPubHash() (string, error) {
	var opts []gotpm.Option
	if auth.emulateTPM {
		opts = append(opts, gotpm.Emulated)
		opts = append(opts, gotpm.WithSeed(auth.seed))
	}
	return gotpm.GetPubHash(opts...)
}
