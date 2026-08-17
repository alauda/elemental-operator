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

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/jaypipes/ghw/pkg/memory"

	"gopkg.in/yaml.v3"
	"gotest.tools/v3/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/hostinfo"
	"github.com/rancher/elemental-operator/pkg/register"
	"github.com/rancher/elemental-operator/pkg/templater"
)

func TestUnauthenticatedResponse(t *testing.T) {
	testCase := []struct {
		config elementalv1.Config
		regUrl string
	}{
		{
			config: elementalv1.Config{},
			regUrl: "https://rancher/url1",
		},
		{
			config: elementalv1.Config{
				Elemental: elementalv1.Elemental{
					Registration: elementalv1.Registration{
						EmulateTPM:      true,
						EmulatedTPMSeed: 127,
						NoSMBIOS:        true,
					},
				},
			},
			regUrl: "https://rancher/url2",
		},
	}

	for _, test := range testCase {
		i := &InventoryServer{
			Context: context.Background(),
			Client:  fake.NewClientBuilder().Build(),
			CACert:  "test-ca",
		}
		registration := elementalv1.MachineRegistration{}
		registration.Spec.Config = test.config
		registration.Status.RegistrationURL = test.regUrl
		registration.Status.Conditions = []metav1.Condition{
			{
				Type:               elementalv1.ReadyCondition,
				Reason:             elementalv1.SuccessfullyCreatedReason,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
			},
		}

		buffer := new(bytes.Buffer)

		err := i.unauthenticatedResponse(&registration, buffer)
		assert.NilError(t, err, err)

		conf := elementalv1.Config{}
		err = yaml.NewDecoder(buffer).Decode(&conf)
		assert.NilError(t, err, strings.TrimSpace(buffer.String()))
		assert.Equal(t, conf.Elemental.Registration.URL, test.regUrl)

		confReg := conf.Elemental.Registration
		testReg := elementalv1.Registration{}
		if !reflect.DeepEqual(test.config, elementalv1.Config{}) {
			testReg = test.config.Elemental.Registration
		}
		assert.Equal(t, confReg.EmulateTPM, testReg.EmulateTPM)
		assert.Equal(t, confReg.EmulatedTPMSeed, testReg.EmulatedTPMSeed)
		assert.Equal(t, confReg.NoSMBIOS, testReg.NoSMBIOS)
		assert.Equal(t, confReg.CACert, "test-ca")
	}
}

func TestBuildName(t *testing.T) {
	data := map[string]interface{}{
		"level1A": map[string]interface{}{
			"level2A": "level2AValue",
			"level2B": map[string]interface{}{
				"level3A": "level3AValue",
			},
		},
		"level1B": "level1BValue",
	}

	testCase := []struct {
		Format string
		Output string
		Error  string
	}{
		{
			Format: "${level1B}",
			Output: "level1BValue",
		},
		{
			Format: "${level1B",
			Output: "level1B",
		},
		{
			Format: "a${level1B",
			Output: "a-level1B",
		},
		{
			Format: "${}",
			Error:  "value not found",
		},
		{
			Format: "${",
			Output: "",
		},
		{
			Format: "a${",
			Output: "a",
		},
		{
			Format: "${level1A}",
			Error:  "value not found",
		},
		{
			Format: "a${level1A}c",
			Error:  "value not found",
		},
		{
			Format: "a${level1A}",
			Error:  "value not found",
		},
		{
			Format: "${level1A}c",
			Error:  "value not found",
		},
		{
			Format: "a${level1A/level2A}c",
			Output: "alevel2AValuec",
		},
		{
			Format: "a${level1A/level2B/level3A}c",
			Output: "alevel3AValuec",
		},
		{
			Format: "a${level1A/level2B/level3A}c${level1B}",
			Output: "alevel3AValueclevel1BValue",
		},
		{
			Format: "a${unknown}",
			Error:  "value not found",
		},
		{
			Format: "+check-sanitize!",
			Output: "check-sanitize",
		},
		{
			Format: "VeryVeryVeryLongLabelValueThatWillBeCutTo58Chars-xxxxx-End|CUTFROMHERE",
			Output: "VeryVeryVeryLongLabelValueThatWillBeCutTo58Chars-xxxxx-End",
		},
		{
			Format: "double--dash--+-sanitized++--",
			Output: "double-dash-sanitized",
		},
		{
			Format: "AllowedNotAlphaNumChars:_.NotTrailingBTW_.",
			Output: "AllowedNotAlphaNumChars-_.NotTrailingBTW",
		},
		{
			Format: "+.!_-",
			Output: "",
		},
		{
			Format: "",
			Output: "",
		},
	}

	tmpl := templater.NewTemplater()
	tmpl.Fill(data)
	for _, testCase := range testCase {
		t.Run(testCase.Format, func(t *testing.T) {
			str, err := tmpl.Decode(testCase.Format)
			if testCase.Error == "" {
				str = sanitizeLabel(str)
				assert.NilError(t, err)
				assert.Equal(t, testCase.Output, str, "'%s' not equal to '%s'", testCase.Output, str)
			} else {
				assert.Equal(t, testCase.Error, err.Error())
			}
		})
	}
}

func TestMergeInventoryLabels(t *testing.T) {
	testCase := []struct {
		data     []byte            // labels to add to the inventory
		labels   map[string]string // labels already in the inventory
		fail     bool
		expected map[string]string
	}{
		{
			[]byte(`{"key2":"val2"}`),
			map[string]string{"key1": "val1"},
			false,
			map[string]string{"key1": "val1"},
		},
		{
			[]byte(`{"key2":2}`),
			map[string]string{"key1": "val1"},
			true,
			map[string]string{"key1": "val1"},
		},
		{
			[]byte(`{"key2":"val2", "key3":"val3"}`),
			map[string]string{"key1": "val1", "key3": "previous_val", "key4": "val4"},
			false,
			map[string]string{"key1": "val1", "key3": "previous_val"},
		},
		{
			[]byte{},
			map[string]string{"key1": "val1"},
			true,
			map[string]string{"key1": "val1"},
		},
		{
			[]byte(`{"key2":"val2"}`),
			nil,
			false,
			map[string]string{},
		},
	}

	for _, test := range testCase {
		inventory := &elementalv1.MachineInventory{}
		inventory.Labels = test.labels

		err := mergeInventoryLabels(inventory, test.data)
		if test.fail {
			assert.Assert(t, err != nil)
		} else {
			assert.Equal(t, err, nil)
		}
		for k, v := range test.expected {
			val, ok := inventory.Labels[k]
			assert.Equal(t, ok, true)
			assert.Equal(t, v, val)
		}

	}
}

func TestMergeInventoryAnnotations(t *testing.T) {
	testCase := []struct {
		data        []byte            // annotations to add to the inventory
		annotations map[string]string // annotations already in the inventory
		fail        bool
		expected    map[string]string
	}{
		{
			[]byte(`{"key2":"val2"}`),
			map[string]string{"key1": "val1"},
			false,
			map[string]string{"key1": "val1", "elemental.cattle.io/key2": "val2"},
		},
		{
			[]byte(`{"key2":2}`),
			map[string]string{"key1": "val1"},
			true,
			map[string]string{"key1": "val1"},
		},
		{
			[]byte(`{"key2":"val2", "key3":"val3"}`),
			map[string]string{"key1": "val1", "elemental.cattle.io/key3": "previous_val"},
			false,
			map[string]string{"key1": "val1", "elemental.cattle.io/key3": "val3", "elemental.cattle.io/key2": "val2"},
		},
		{
			[]byte{},
			map[string]string{"key1": "val1"},
			true,
			map[string]string{"key1": "val1"},
		},
		{
			[]byte(`{"key2":"val2"}`),
			nil,
			false,
			map[string]string{"elemental.cattle.io/key2": "val2"},
		},
	}

	for _, test := range testCase {
		inventory := &elementalv1.MachineInventory{}
		inventory.Annotations = test.annotations

		err := mergeInventoryAnnotations(test.data, inventory)
		if test.fail {
			assert.Assert(t, err != nil)
		} else {
			assert.NilError(t, err)
		}
		for k, v := range test.expected {
			val, ok := inventory.Annotations[k]
			assert.Equal(t, ok, true, "annotations: %v\nexpected: %v ", inventory.Annotations, test.expected)
			assert.Equal(t, v, val, "annotations: %v\nexpected: %v ", inventory.Annotations, test.expected)
		}
	}
}

func TestUpdateInventoryObservedStorage(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NilError(t, elementalv1.AddToScheme(scheme))
	inventory := &elementalv1.MachineInventory{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "storage-observer-test",
			Namespace:         "default",
			UID:               "inventory-uid",
			ResourceVersion:   "1",
			CreationTimestamp: metav1.Now(),
		},
	}
	server := &InventoryServer{
		Context: context.Background(),
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&elementalv1.MachineInventory{}).
			WithObjects(inventory.DeepCopy()).Build(),
	}
	epoch, err := server.claimStorageObserverEpoch(inventory)
	assert.NilError(t, err)
	assert.Equal(t, epoch, int64(1))

	data := []byte(`{
		"bootID": "boot-1",
		"reportEpoch": 1,
		"sequence": 1,
		"devices": [{
			"id": "wwn:5000c50000000001",
			"kind": "DirectDisk",
			"path": "/dev/sdb",
			"wwn": "0x5000c50000000001",
			"serial": "DATA-SERIAL",
			"model": "ExampleDataDisk",
			"sizeBytes": 1099511627776,
			"systemRole": "Data",
			"filesystem": {
				"type": "xfs",
				"uuid": "11111111-2222-3333-4444-555555555555"
			}
		}]
	}`)

	observed := &elementalv1.ObservedStorage{}
	assert.NilError(t, json.Unmarshal(data, observed))
	if err := server.patchObservedStorageStatus(inventory, observed); err != nil {
		t.Fatalf("patchObservedStorageStatus: %v", err)
	}
	current := &elementalv1.MachineInventory{}
	assert.NilError(t, server.Get(context.Background(), client.ObjectKeyFromObject(inventory), current))
	if current.Status.ObservedStorage == nil || len(current.Status.ObservedStorage.Devices) != 1 {
		t.Fatalf("unexpected observed storage: %#v", current.Status.ObservedStorage)
	}
	device := current.Status.ObservedStorage.Devices[0]
	assert.Equal(t, device.ID, "wwn:5000c50000000001")
	assert.Equal(t, device.SizeBytes, int64(1099511627776))
	assert.Equal(t, device.Filesystem.Type, "xfs")
	assert.Assert(t, current.Status.ObservedStorage.ObservedAt != nil)

	if err := server.patchObservedStorageStatus(inventory, observed.DeepCopy()); err == nil || !strings.Contains(err.Error(), "stale storage report") {
		t.Fatalf("stale report error = %v", err)
	}
	epoch, err = server.claimStorageObserverEpoch(inventory)
	assert.NilError(t, err)
	assert.Equal(t, epoch, int64(2))
	observed.ReportEpoch = 1
	observed.Sequence = 2
	if err := server.patchObservedStorageStatus(inventory, observed); err == nil || !strings.Contains(err.Error(), "stale storage report") {
		t.Fatalf("old epoch error = %v", err)
	}
	observed.ReportEpoch = 2
	observed.Sequence = 1
	observed.BootID = ""
	if err := server.patchObservedStorageStatus(inventory, observed); err == nil || !strings.Contains(err.Error(), "boot ID is required") {
		t.Fatalf("invalid report error = %v", err)
	}
}

func TestRegistrationMsgGet(t *testing.T) {
	testCases := []struct {
		name                string
		machineName         string
		protoVersion        register.MessageType
		wantConnectionError bool
		wantRawResponse     bool
		wantMessageType     register.MessageType
		wantSystemAgentURL  string
		wantSystemAgentCA   string
		wantRegistrationURL string
		wantRegistrationCA  string
	}{
		{
			name:                "returns not-found error for unknown machine",
			machineName:         "unknown",
			wantConnectionError: true,
		},
		{
			name:            "returns raw config for older protoVersion",
			machineName:     "machine-1",
			protoVersion:    register.MsgUndefined,
			wantRawResponse: true,
		},
		{
			name:               "returns MsgConfig for newer protoVersion",
			machineName:        "machine-2",
			protoVersion:       register.MsgError,
			wantRawResponse:    false,
			wantMessageType:    register.MsgConfig,
			wantSystemAgentURL: "https://global-vip.example.com/kubernetes/global",
			wantSystemAgentCA:  "platform-ca",
			wantRegistrationCA: "platform-ca",
		},
		{
			name:                "returns direct apiserver CA separately from registration CA",
			machineName:         "machine-4",
			protoVersion:        register.MsgError,
			wantRawResponse:     false,
			wantMessageType:     register.MsgConfig,
			wantSystemAgentURL:  "https://global-vip.example.com:6443",
			wantSystemAgentCA:   "apiserver-ca",
			wantRegistrationURL: "https://platform.example.org/elemental/registration/machine-4",
			wantRegistrationCA:  "platform-ca",
		},
		{
			name:            "returns MsgError for newer protoVersion and error",
			machineName:     "machine-2",
			protoVersion:    register.MsgError,
			wantRawResponse: false,
			wantMessageType: register.MsgError,
		},
		{
			name:            "returns MsgError if mregistration status has no URL",
			machineName:     "machine-3",
			protoVersion:    register.MsgError,
			wantRawResponse: false,
			wantMessageType: register.MsgError,
		},
	}

	server := NewInventoryServer(&FakeAuthServer{})
	server.SystemAgentClusterName = "global"
	server.CACert = "platform-ca"

	server.Client.Create(context.Background(), &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-1",
		},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineName: "machine-1",
		},
		Status: elementalv1.MachineRegistrationStatus{
			ServiceAccountRef: &v1.ObjectReference{
				Name: "test-account",
			},
			RegistrationURL:   "https://example.machine-1.org",
			RegistrationToken: "machine-1",
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: "True",
				},
			},
		},
	})

	server.Client.Create(context.Background(), &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-2",
			Annotations: map[string]string{
				elementalv1.SystemAgentServerURLAnnotation: "https://global-vip.example.com",
			},
		},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineName: "machine-2",
		},
		Status: elementalv1.MachineRegistrationStatus{
			ServiceAccountRef: &v1.ObjectReference{
				Name: "test-account",
			},
			RegistrationURL:   "https://example.machine-2.org",
			RegistrationToken: "machine-2",
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: "True",
				},
			},
		},
	})

	server.Client.Create(context.Background(), &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-3",
		},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineName: "machine-3",
		},
		Status: elementalv1.MachineRegistrationStatus{
			ServiceAccountRef: &v1.ObjectReference{
				Name: "test-account",
			},
			RegistrationToken: "machine-3",
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: "True",
				},
			},
		},
	})

	server.Client.Create(context.Background(), &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-4",
			Annotations: map[string]string{
				elementalv1.SystemAgentServerURLAnnotation:    "https://global-vip.example.com:6443",
				elementalv1.SystemAgentEndpointModeAnnotation: SystemAgentEndpointModeDirectAPIServer,
			},
		},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineName: "machine-4",
		},
		Status: elementalv1.MachineRegistrationStatus{
			ServiceAccountRef: &v1.ObjectReference{
				Name: "test-account",
			},
			RegistrationURL:   "https://platform.example.org/elemental/registration/machine-4",
			RegistrationToken: "machine-4",
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: "True",
				},
			},
		},
	})

	createDefaultResources(t, server)

	wsServer := httptest.NewServer(server)
	defer wsServer.Close()

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			url := fmt.Sprintf("ws%s/%s", strings.TrimPrefix(wsServer.URL, "http"), "elemental/registration/"+tc.machineName)

			header := http.Header{}
			header.Add("Authorization", tc.machineName)

			ws, _, err := websocket.DefaultDialer.Dial(url, header)
			if tc.wantConnectionError {
				assert.Error(t, err, "websocket: bad handshake")
				return
			} else {
				assert.NilError(t, err)
			}

			defer ws.Close()

			// Read MsgReady
			msgType, _, err := register.ReadMessage(ws)
			assert.NilError(t, err)
			assert.Equal(t, register.MsgReady, msgType)

			// Negotiate version
			if tc.protoVersion != register.MsgUndefined {
				err = register.WriteMessage(ws, register.MsgVersion, []byte{byte(tc.protoVersion)})
				assert.NilError(t, err)

				msgType, _, err = register.ReadMessage(ws)
				assert.NilError(t, err)
				assert.Equal(t, register.MsgVersion, msgType)
			}

			// Actual send MsgGet
			err = register.WriteMessage(ws, register.MsgGet, []byte{})
			assert.NilError(t, err)

			if tc.wantRawResponse {
				msgType, r, err := ws.NextReader()
				assert.NilError(t, err)
				assert.Equal(t, msgType, websocket.BinaryMessage)
				data, _ := io.ReadAll(r)
				assert.Assert(t, strings.HasPrefix(string(data), "elemental:"))
				return
			}

			msgType, data, err := register.ReadMessage(ws)
			assert.NilError(t, err)
			assert.Assert(t, data != nil)
			assert.Equal(t, tc.wantMessageType, msgType)

			config := &elementalv1.Config{}
			err = yaml.Unmarshal(data, &config)
			assert.NilError(t, err)
			if tc.wantMessageType == register.MsgConfig {
				wantURL := tc.wantSystemAgentURL
				if wantURL == "" {
					wantURL = "https://test-server.example.com/kubernetes/global"
				}
				assert.Equal(t, wantURL, config.Elemental.SystemAgent.URL)
				if tc.wantSystemAgentCA != "" {
					assert.Equal(t, tc.wantSystemAgentCA, config.Elemental.SystemAgent.CACert)
				}
				if tc.wantRegistrationURL != "" {
					assert.Equal(t, tc.wantRegistrationURL, config.Elemental.Registration.URL)
				}
				if tc.wantRegistrationCA != "" {
					assert.Equal(t, tc.wantRegistrationCA, config.Elemental.Registration.CACert)
				}
			}
		})
	}
}

func TestRegistrationMsgUpdate(t *testing.T) {
	testCases := []struct {
		name                string
		machineName         string
		doesInventoryExists bool
		wantMessageType     register.MessageType
	}{
		{
			name:            "returns not-found error for unknown machine inventory",
			machineName:     "machine-1",
			wantMessageType: register.MsgError,
		},
		{
			name:                "returns MsgConfig config on registration update",
			machineName:         "machine-1",
			doesInventoryExists: true,
			wantMessageType:     register.MsgReady,
		},
	}

	authenticator := &FakeAuthServer{}
	server := NewInventoryServer(authenticator)

	server.Client.Create(context.Background(), &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-1",
		},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineName: "machine-1",
		},
		Status: elementalv1.MachineRegistrationStatus{
			ServiceAccountRef: &v1.ObjectReference{
				Name: "test-account",
			},
			RegistrationToken: "machine-1",
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: "True",
				},
			},
		},
	})

	createDefaultResources(t, server)

	wsServer := httptest.NewServer(server)
	defer wsServer.Close()

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			authenticator.CreateExistingInventory = tc.doesInventoryExists

			url := fmt.Sprintf("ws%s/%s", strings.TrimPrefix(wsServer.URL, "http"), "elemental/registration/"+tc.machineName)

			header := http.Header{}
			header.Add("Authorization", fmt.Sprintf("Bearer TPM%s", tc.machineName))

			ws, _, _ := websocket.DefaultDialer.Dial(url, header)
			defer ws.Close()

			// Read MsgReady
			msgType, _, err := register.ReadMessage(ws)
			assert.NilError(t, err)
			assert.Equal(t, register.MsgReady, msgType)

			// Negotiate version
			err = register.WriteMessage(ws, register.MsgVersion, []byte{byte(register.MsgUpdate)})
			assert.NilError(t, err)
			msgType, _, err = register.ReadMessage(ws)
			assert.NilError(t, err)
			assert.Equal(t, register.MsgVersion, msgType)

			// Actual send MsgUpdate
			err = register.WriteMessage(ws, register.MsgUpdate, []byte{})
			assert.NilError(t, err)

			// Read response message
			msgType, _, err = register.ReadMessage(ws)
			assert.NilError(t, err)
			assert.Equal(t, tc.wantMessageType, msgType)
		})
	}
}

func TestRegistrationDynamicLabels(t *testing.T) {
	server := NewInventoryServer(&FakeAuthServer{})

	server.Client.Create(context.Background(), &elementalv1.MachineRegistration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-1",
		},
		Spec: elementalv1.MachineRegistrationSpec{
			MachineName: "machine-1",
			MachineInventoryLabels: map[string]string{
				"uuid":            "${System Information/UUID}",
				"physical_memory": "${System Data/Memory/Total Physical Bytes}",
			},
		},
		Status: elementalv1.MachineRegistrationStatus{
			ServiceAccountRef: &v1.ObjectReference{
				Name: "test-account",
			},
			RegistrationToken: "machine-1",
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: "True",
				},
			},
		},
	})

	createDefaultResources(t, server)

	wsServer := httptest.NewServer(server)
	defer wsServer.Close()

	t.Run("generates registration labels from SMBIOS and System data", func(t *testing.T) {
		machineName := "machine-1"
		url := fmt.Sprintf("ws%s/%s", strings.TrimPrefix(wsServer.URL, "http"), "elemental/registration/"+machineName)

		header := http.Header{}
		header.Add("Authorization", machineName)

		ws, _, err := websocket.DefaultDialer.Dial(url, header)
		assert.NilError(t, err)

		defer ws.Close()

		// Read MsgReady
		msgType, _, err := register.ReadMessage(ws)
		assert.NilError(t, err)
		assert.Equal(t, register.MsgReady, msgType)

		// Negotiate version
		err = register.WriteMessage(ws, register.MsgVersion, []byte{byte(register.MsgLast)})
		assert.NilError(t, err)

		msgType, _, err = register.ReadMessage(ws)
		assert.NilError(t, err)
		assert.Equal(t, register.MsgVersion, msgType)

		// Send SMBIOS
		smbios, err := json.Marshal(map[string]interface{}{
			"System Information": map[string]interface{}{
				"UUID": "uuid-123",
			},
		})
		assert.NilError(t, err)
		err = register.WriteMessage(ws, register.MsgSmbios, smbios)
		assert.NilError(t, err)

		msgType, _, err = register.ReadMessage(ws)
		assert.NilError(t, err)
		assert.Equal(t, register.MsgReady, msgType)

		// Send System Data
		systemData := &hostinfo.HostInfo{}
		systemData.Memory = &memory.Info{
			Area: memory.Area{
				TotalPhysicalBytes: 100,
			},
		}
		systemDataJson, err := json.Marshal(systemData)
		assert.NilError(t, err)
		err = register.WriteMessage(ws, register.MsgSystemData, systemDataJson)
		assert.NilError(t, err)

		msgType, _, err = register.ReadMessage(ws)
		assert.NilError(t, err)
		assert.Equal(t, register.MsgReady, msgType)
	})
}

func TestAgentTLSMode(t *testing.T) {
	type test struct {
		name              string
		agentTLSMode      string
		wantStrictTLSMode bool
	}

	tests := []test{
		{
			name:              "missing agent-tls-mode",
			agentTLSMode:      "",
			wantStrictTLSMode: true,
		},
		{
			name:              "strict agent-tls-mode",
			agentTLSMode:      AgentTLSModeStrict,
			wantStrictTLSMode: true,
		},
		{
			name:              "system-store agent-tls-mode",
			agentTLSMode:      AgentTLSModeSystemStore,
			wantStrictTLSMode: false,
		},
	}

	for _, tt := range tests {
		server := NewInventoryServer(&FakeAuthServer{})

		t.Run(tt.name, func(t *testing.T) {
			server.AgentTLSMode = tt.agentTLSMode
			assert.Equal(t, server.isAgentTLSModeStrict(), tt.wantStrictTLSMode)
		})
	}

}

func TestSystemAgentEndpointMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to erebus", want: SystemAgentEndpointModeErebus},
		{name: "erebus", mode: SystemAgentEndpointModeErebus, want: SystemAgentEndpointModeErebus},
		{name: "direct apiserver", mode: SystemAgentEndpointModeDirectAPIServer, want: SystemAgentEndpointModeDirectAPIServer},
		{name: "direct alias", mode: "direct", want: SystemAgentEndpointModeDirectAPIServer},
		{name: "invalid", mode: "bad-mode", want: "bad-mode", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeSystemAgentEndpointMode(tt.mode)
			assert.Equal(t, got, tt.want)
			err := ValidateSystemAgentEndpointMode(tt.mode)
			if tt.wantErr {
				assert.Assert(t, err != nil)
				return
			}
			assert.NilError(t, err)
		})
	}
}

func TestMachineRegistrationSystemAgentEndpointModePrecedence(t *testing.T) {
	tests := []struct {
		name        string
		defaultMode string
		annotations map[string]string
		want        string
		wantErr     bool
	}{
		{
			name:        "operator default is used without annotations",
			defaultMode: SystemAgentEndpointModeDirectAPIServer,
			want:        SystemAgentEndpointModeDirectAPIServer,
		},
		{
			name: "legacy true selects direct apiserver",
			annotations: map[string]string{
				elementalv1.SystemAgentDirectAPIServerAnnotation: " true ",
			},
			want: SystemAgentEndpointModeDirectAPIServer,
		},
		{
			name:        "legacy false does not override the operator default",
			defaultMode: SystemAgentEndpointModeDirectAPIServer,
			annotations: map[string]string{
				elementalv1.SystemAgentDirectAPIServerAnnotation: "false",
			},
			want: SystemAgentEndpointModeDirectAPIServer,
		},
		{
			name: "explicit erebus overrides legacy true",
			annotations: map[string]string{
				elementalv1.SystemAgentEndpointModeAnnotation:    SystemAgentEndpointModeErebus,
				elementalv1.SystemAgentDirectAPIServerAnnotation: "true",
			},
			want: SystemAgentEndpointModeErebus,
		},
		{
			name:        "explicit empty enum normalizes to erebus and overrides legacy true",
			defaultMode: SystemAgentEndpointModeDirectAPIServer,
			annotations: map[string]string{
				elementalv1.SystemAgentEndpointModeAnnotation:    "",
				elementalv1.SystemAgentDirectAPIServerAnnotation: "true",
			},
			want: SystemAgentEndpointModeErebus,
		},
		{
			name: "explicit direct overrides legacy false",
			annotations: map[string]string{
				elementalv1.SystemAgentEndpointModeAnnotation:    SystemAgentEndpointModeDirectAPIServer,
				elementalv1.SystemAgentDirectAPIServerAnnotation: "false",
			},
			want: SystemAgentEndpointModeDirectAPIServer,
		},
		{
			name: "invalid explicit enum fails instead of falling back to legacy true",
			annotations: map[string]string{
				elementalv1.SystemAgentEndpointModeAnnotation:    "invalid",
				elementalv1.SystemAgentDirectAPIServerAnnotation: "true",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewInventoryServer(&FakeAuthServer{})
			server.SystemAgentEndpointMode = NormalizeSystemAgentEndpointMode(tt.defaultMode)
			registration := &elementalv1.MachineRegistration{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}}

			got, err := server.getSystemAgentEndpointMode(registration)
			if tt.wantErr {
				assert.Assert(t, err != nil)
				return
			}
			assert.NilError(t, err)
			assert.Equal(t, got, tt.want)
		})
	}
}

func TestDirectAPIServerSystemAgentCAFailsClosed(t *testing.T) {
	server := NewInventoryServer(&FakeAuthServer{})
	secret := &v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "agent-token", Namespace: "default"}}

	_, err := server.getSystemAgentCACert(SystemAgentEndpointModeDirectAPIServer, secret)
	assert.ErrorContains(t, err, "has no ca.crt")

	server.AgentTLSMode = AgentTLSModeSystemStore
	caCert, err := server.getSystemAgentCACert(SystemAgentEndpointModeDirectAPIServer, secret)
	assert.NilError(t, err)
	assert.Equal(t, caCert, "")
}

func NewInventoryServer(auth authenticator) *InventoryServer {
	scheme := runtime.NewScheme()
	elementalv1.AddToScheme(scheme)
	clientgoscheme.AddToScheme(scheme)

	return &InventoryServer{
		Context:                context.Background(),
		Client:                 fake.NewClientBuilder().WithScheme(scheme).Build(),
		ServerURL:              "https://test-server.example.com",
		AgentTLSMode:           AgentTLSModeStrict,
		SystemAgentClusterName: DefaultSystemAgentClusterName,
		authenticators: []authenticator{
			auth,
		},
	}
}

type FakeAuthServer struct {
	CreateExistingInventory bool
}

// Authenticate always returns true and a MachineInventory with the TPM-Hash
// set to the machine-name from the URL.
func (a *FakeAuthServer) Authenticate(conn *websocket.Conn, req *http.Request, registerNamespace string) (*elementalv1.MachineInventory, bool, error) {
	token := path.Base(req.URL.Path)

	// A zero CreationTimestamp implies the MachineInventory is new
	creationTimestamp := metav1.Time{}
	if a.CreateExistingInventory {
		creationTimestamp = metav1.Now()
	}

	return &elementalv1.MachineInventory{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: creationTimestamp,
		},
		Spec: elementalv1.MachineInventorySpec{
			TPMHash: token,
		},
	}, true, nil
}

func createDefaultResources(t *testing.T, server *InventoryServer) {
	t.Helper()
	server.Client.Create(context.Background(), &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-account-token",
		},

		Type: v1.SecretTypeServiceAccountToken,
		Data: map[string][]byte{
			"ca.crt": []byte("apiserver-ca"),
			"token":  []byte("system-agent-token"),
		},
	})

	server.Client.Create(context.Background(), &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-account",
		},
	})
}
