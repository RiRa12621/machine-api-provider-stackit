// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackitcloud/stackit-sdk-go/core/clients"
	"github.com/stackitcloud/stackit-sdk-go/core/config"
	"github.com/stackitcloud/stackit-sdk-go/core/oapierror"
	iaas "github.com/stackitcloud/stackit-sdk-go/services/iaas/v2api"
)

const (
	projectID = "11111111-1111-1111-1111-111111111111"
	serverID  = "22222222-2222-2222-2222-222222222222"
	networkID = "33333333-3333-3333-3333-333333333333"
	imageID   = "44444444-4444-4444-4444-444444444444"
	volumeID  = "55555555-5555-5555-5555-555555555555"
	groupID   = "66666666-6666-6666-6666-666666666666"
	apiPath   = "/v2/projects/" + projectID + "/regions/eu01"
)

func TestNewClient(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	credentials := map[string]interface{}{
		"privateKey": privateKey,
		"sub":        projectID, "iss": "test@sa.stackit.cloud", "aud": "test", "kid": "test-key",
		"tokenEndpoint": "https://service-account.api.stackit.cloud/token",
	}
	validJSON, err := json.Marshal(map[string]interface{}{"credentials": credentials})
	if err != nil {
		t.Fatal(err)
	}
	// None of these ambient sources may override or supplement the Secret.
	for _, variable := range []string{
		"STACKIT_SERVICE_ACCOUNT_KEY", "STACKIT_SERVICE_ACCOUNT_KEY_PATH", "STACKIT_PRIVATE_KEY", "STACKIT_PRIVATE_KEY_PATH",
		"STACKIT_SERVICE_ACCOUNT_TOKEN", "STACKIT_CREDENTIALS_PATH", "STACKIT_TOKEN_BASEURL", "STACKIT_REGION", "STACKIT_NO_AUTH",
	} {
		t.Setenv(variable, "poisoned-ambient-value")
	}
	t.Run("When explicit credentials are valid, it should ignore all ambient auth sources", func(t *testing.T) {
		client, err := NewClient(t.Context(), Credentials{ProjectID: projectID, Region: "eu01", ServiceAccountJSON: validJSON})
		if err != nil {
			t.Fatal(err)
		}
		concrete, ok := client.(*sdkClient)
		if !ok {
			t.Fatalf("unexpected client type %T", client)
		}
		cfg := concrete.api.GetConfig()
		flow, ok := cfg.HTTPClient.Transport.(*clients.KeyFlow)
		if !ok {
			t.Fatalf("unexpected authentication transport %T", cfg.HTTPClient.Transport)
		}
		flowCfg := flow.GetConfig()
		if flowCfg.PrivateKey != privateKey || flowCfg.TokenUrl != credentials["tokenEndpoint"] || concrete.region != "eu01" || cfg.Servers[0].URL != "https://iaas.api.stackit.cloud" {
			t.Error("client did not use only the explicit credential fields and region")
		}
	})
	for _, tc := range []struct {
		name    string
		project string
		region  string
		data    []byte
	}{
		{name: "empty Secret", project: projectID, region: "eu01"},
		{name: "empty object", project: projectID, region: "eu01", data: []byte(`{}`)},
		{name: "null", project: projectID, region: "eu01", data: []byte(`null`)},
		{name: "malformed JSON", project: projectID, region: "eu01", data: []byte(`private-secret-value`)},
		{name: "missing private key", project: projectID, region: "eu01", data: []byte(`{"credentials":{"sub":"` + projectID + `","iss":"issuer","aud":"aud","kid":"key"}}`)},
		{name: "invalid private key", project: projectID, region: "eu01", data: []byte(`{"credentials":{"privateKey":"private-secret-value","sub":"` + projectID + `","iss":"issuer","aud":"aud","kid":"key"}}`)},
		{name: "missing region", project: projectID, data: validJSON},
		{name: "invalid project", project: "wrong", region: "eu01", data: validJSON},
	} {
		t.Run("When "+tc.name+" is supplied, it should reject ambient fallback", func(t *testing.T) {
			_, err := NewClient(t.Context(), Credentials{ProjectID: tc.project, Region: tc.region, ServiceAccountJSON: tc.data})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected invalid input, got %v", err)
			}
			if strings.Contains(err.Error(), "private-secret-value") {
				t.Error("error leaked credentials")
			}
		})
	}
	for _, endpoint := range []string{"http://insecure.example/token", "https://user:password@example/token", "https://example/token?secret=key", "https://example/token#fragment"} {
		t.Run("When the token endpoint is unsafe, it should reject it: "+endpoint, func(t *testing.T) {
			credentials["tokenEndpoint"] = endpoint
			data, err := json.Marshal(map[string]interface{}{"credentials": credentials})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewClient(t.Context(), Credentials{ProjectID: projectID, Region: "eu01", ServiceAccountJSON: data}); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected invalid input, got %v", err)
			}
		})
	}
}

func TestGetServer(t *testing.T) {
	t.Run("When detailed metadata is returned, it should preserve ownership and cleanup information", func(t *testing.T) {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != apiPath+"/servers/"+serverID || r.URL.Query().Get("details") != "true" {
				t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			}
			writeJSON(t, w, http.StatusOK, detailedServer())
		})
		server, err := client.GetServer(t.Context(), serverID)
		if err != nil {
			t.Fatal(err)
		}
		if server.ID != serverID || server.Status != "ACTIVE" || server.PowerStatus != "RUNNING" || server.MachineType != "g1.2" ||
			server.Labels["openshift-machine-uid"] != "machine-uid" || server.BootVolume == nil || server.BootVolume.ID != volumeID || !server.BootVolume.DeleteOnTermination {
			t.Fatalf("incomplete server mapping: %#v", server)
		}
		if _, present := server.Labels["not-a-string"]; present {
			t.Error("non-string label was accepted as ownership evidence")
		}
		if !reflect.DeepEqual(server.NetworkIDs, []string{networkID}) || !reflect.DeepEqual(server.VolumeIDs, []string{volumeID}) {
			t.Errorf("network or volume identities lost: %#v", server)
		}
		want := []Address{{Type: "InternalIP", Address: "10.0.0.2"}, {Type: "InternalIP", Address: "fd00::2"}, {Type: "ExternalIP", Address: "192.0.2.2"}}
		if !reflect.DeepEqual(server.Addresses, want) {
			t.Errorf("addresses = %#v, want %#v", server.Addresses, want)
		}
	})
	for _, body := range []string{`null`, `{"id":"` + imageID + `","name":"worker","machineType":"g1.2"}`, `{"id":"` + serverID + `","name":"worker","machineType":"g1.2","nics":[{"ipv4":"invalid"}]}`} {
		t.Run("When a cloud response cannot identify the server safely, it should fail", func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})
			if _, err := client.GetServer(t.Context(), serverID); !errors.Is(err, ErrTransient) {
				t.Fatalf("expected temporary failure, got %v", err)
			}
		})
	}
}

func TestListServers(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != apiPath+"/servers" || r.URL.Query().Get("details") != "true" ||
			r.URL.Query().Get("label_selector") != "openshift-machine-provider=stackit,openshift-machine-uid=machine-uid" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		owned := detailedServer()
		foreign := detailedServer()
		foreign["labels"] = map[string]interface{}{"openshift-machine-uid": "another-machine"}
		writeJSON(t, w, http.StatusOK, map[string]interface{}{"items": []interface{}{owned, foreign, owned}})
	})
	servers, err := client.ListServers(t.Context(), map[string]string{"openshift-machine-uid": "machine-uid", "openshift-machine-provider": "stackit"})
	if err != nil {
		t.Fatal(err)
	}
	// Both matches remain visible so the actuator can reject ambiguous ownership.
	if len(servers) != 2 || servers[0].ID != serverID || servers[1].ID != serverID {
		t.Fatalf("expected all exact ownership matches, got %#v", servers)
	}
}

func TestCreateServer(t *testing.T) {
	input := validCreateInput()
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != apiPath+"/servers" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		data, _ := payload["userData"].(string)
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil || string(decoded) != string(input.UserData) || payload["configDrive"] != true {
			t.Error("raw Ignition was not passed unchanged with exactly one base64 encoding and a config drive")
		}
		boot, _ := payload["bootVolume"].(map[string]interface{})
		source, _ := boot["source"].(map[string]interface{})
		if boot["deleteOnTermination"] != true || boot["size"] != float64(50) || boot["performanceClass"] != "storage_premium_perf2" ||
			source["id"] != imageID || source["type"] != "image" {
			t.Errorf("incorrect owned boot volume: %#v", boot)
		}
		if _, found := boot["id"]; found {
			t.Error("must not adopt an existing volume")
		}
		if _, found := payload["imageId"]; found {
			t.Error("boot volume source and top-level imageId must not both be set")
		}
		network, _ := payload["networking"].(map[string]interface{})
		if network["networkId"] != networkID || payload["availabilityZone"] != "eu01-1" || payload["keypairName"] != "worker-key" {
			t.Errorf("incorrect placement: %#v", payload)
		}
		labels, _ := payload["labels"].(map[string]interface{})
		if labels["openshift-machine-uid"] != "machine-uid" {
			t.Error("missing ownership label at creation")
		}
		groups, _ := payload["securityGroups"].([]interface{})
		if len(groups) != 1 || groups[0] != groupID {
			t.Errorf("incorrect security groups: %#v", groups)
		}
		server := detailedServer()
		server["status"] = "CREATING"
		writeJSON(t, w, http.StatusAccepted, server)
	})
	server, err := client.CreateServer(t.Context(), input)
	if err != nil || server == nil || server.Status != "CREATING" {
		t.Fatalf("create result = %#v, error %v", server, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one asynchronous request, got %d", calls.Load())
	}
}

func TestDeleteServer(t *testing.T) {
	var getCalls, deleteCalls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPath+"/servers/"+serverID {
			t.Errorf("unexpected resource: %s", r.URL.Path)
		}
		if r.Method == http.MethodDelete {
			deleteCalls.Add(1)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if getCalls.Add(1) == 1 {
			server := detailedServer()
			server["status"] = "DELETING"
			writeJSON(t, w, http.StatusOK, server)
			return
		}
		writeJSON(t, w, http.StatusNotFound, map[string]interface{}{})
	})
	if err := client.DeleteServer(t.Context(), serverID); err != nil {
		t.Fatal(err)
	}
	server, err := client.GetServer(t.Context(), serverID)
	if err != nil || server == nil || server.Status != "DELETING" {
		t.Fatalf("deletion submission must not imply resource absence: %#v %v", server, err)
	}
	if _, err := client.GetServer(t.Context(), serverID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected absence confirmation, got %v", err)
	}
	if deleteCalls.Load() != 1 || getCalls.Load() != 2 {
		t.Error("adapter performed unrequested calls during asynchronous deletion")
	}
}

func TestGetNetworkAndVolume(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("must not mutate user infrastructure: %s %s", r.Method, r.URL)
		}
		switch r.URL.Path {
		case apiPath + "/networks/" + networkID:
			writeJSON(t, w, http.StatusOK, map[string]interface{}{"id": networkID, "name": "shared", "status": "CREATED"})
		case apiPath + "/volumes/" + volumeID:
			writeJSON(t, w, http.StatusOK, map[string]interface{}{"id": volumeID, "name": "root", "status": "DELETING", "size": 50, "availabilityZone": "eu01-1"})
		default:
			writeJSON(t, w, http.StatusNotFound, map[string]interface{}{})
		}
	})
	network, err := client.GetNetwork(t.Context(), networkID)
	if err != nil || network == nil || network.ID != networkID || network.Status != "CREATED" {
		t.Fatalf("get network = %#v, %v", network, err)
	}
	volume, err := client.GetVolume(t.Context(), volumeID)
	if err != nil || volume == nil || volume.ID != volumeID || volume.Status != "DELETING" {
		t.Fatalf("get volume = %#v, %v", volume, err)
	}
	if _, err := client.GetVolume(t.Context(), imageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected deleted volume confirmation, got %v", err)
	}
}

func TestSDKErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{400, ErrInvalidInput}, {401, ErrUnauthorized}, {403, ErrUnauthorized}, {404, ErrNotFound}, {409, ErrConflict}, {422, ErrInvalidInput}, {429, ErrTransient}, {500, ErrTransient}, {503, ErrTransient}} {
		t.Run(fmt.Sprintf("When create returns HTTP %d, it should classify without retry or secret leakage", tc.status), func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeJSON(t, w, tc.status, map[string]interface{}{"message": "private-secret-value"})
			})
			_, err := client.CreateServer(t.Context(), validCreateInput())
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "private-secret-value") || calls.Load() != 1 {
				t.Fatalf("incorrect sanitized error or retry count: error %v, calls %d", err, calls.Load())
			}
		})
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if !errors.Is(classifyError("operation", err), err) {
			t.Errorf("context error identity lost: %v", err)
		}
	}
	if err := classifyError("operation", errors.New("private-secret-value")); !errors.Is(err, ErrTransient) || strings.Contains(err.Error(), "private-secret-value") {
		t.Errorf("unexpected transport error classification: %v", err)
	}
	err := classifyError("operation", fmt.Errorf("wrapped: %w", &oapierror.GenericOpenAPIError{StatusCode: 404}))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("wrapped API error not classified: %v", err)
	}
}

func TestInputValidation(t *testing.T) {
	// A nil API ensures invalid inputs cannot accidentally reach the cloud.
	client := &sdkClient{}
	for _, labels := range []map[string]string{nil, {"invalid/key": "value"}, {"stackit-reserved": "value"}, {"key": "invalid,value"}, {strings.Repeat("k", 64): "value"}} {
		if _, err := client.ListServers(t.Context(), labels); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("invalid labels accepted: %#v", labels)
		}
	}
	if _, err := client.GetServer(t.Context(), "bad-id"); !errors.Is(err, ErrInvalidInput) {
		t.Error("invalid server ID accepted")
	}
	if err := client.DeleteServer(t.Context(), "bad-id"); !errors.Is(err, ErrInvalidInput) {
		t.Error("invalid deletion identity accepted")
	}
	if _, err := client.GetNetwork(t.Context(), "bad-id"); !errors.Is(err, ErrInvalidInput) {
		t.Error("invalid network ID accepted")
	}
	if _, err := client.GetVolume(t.Context(), "bad-id"); !errors.Is(err, ErrInvalidInput) {
		t.Error("invalid volume ID accepted")
	}
	for _, mutate := range []func(*CreateServerInput){
		func(in *CreateServerInput) { in.RootVolume.SizeGiB = 0 },
		func(in *CreateServerInput) { in.UserData = nil },
		func(in *CreateServerInput) { in.SecurityGroups = []string{"bad-id"} },
		func(in *CreateServerInput) { in.Labels = nil },
	} {
		input := validCreateInput()
		mutate(&input)
		if _, err := client.CreateServer(t.Context(), input); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("invalid creation input accepted: %v", err)
		}
	}
}

func testClient(t *testing.T, handler http.HandlerFunc) *sdkClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	api, err := iaas.NewAPIClient(config.WithoutAuthentication(), config.WithEndpoint(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	return &sdkClient{api: api, projectID: projectID, region: "eu01"}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Error(err)
	}
}

func validCreateInput() CreateServerInput {
	return CreateServerInput{
		Name: "worker", ImageID: imageID, MachineType: "g1.2", NetworkID: networkID,
		AvailabilityZone: "eu01-1", SSHKeyName: "worker-key", SecurityGroups: []string{groupID},
		Labels:     map[string]string{"openshift-machine-uid": "machine-uid", "openshift-machine-provider": "stackit"},
		UserData:   []byte("{\n  \"ignition\": {\"version\": \"3.4.0\"}\n}\n"),
		RootVolume: RootVolume{SizeGiB: 50, PerformanceClass: "storage_premium_perf2"},
	}
}

func detailedServer() map[string]interface{} {
	return map[string]interface{}{
		"id": serverID, "name": "worker", "machineType": "g1.2", "status": "ACTIVE", "powerStatus": "RUNNING",
		"imageId": imageID, "availabilityZone": "eu01-1",
		"labels":     map[string]interface{}{"openshift-machine-uid": "machine-uid", "openshift-machine-provider": "stackit", "not-a-string": 3},
		"bootVolume": map[string]interface{}{"id": volumeID, "deleteOnTermination": true},
		"volumes":    []string{volumeID},
		"nics": []interface{}{map[string]interface{}{
			"networkId": networkID, "networkName": "shared", "nicId": groupID, "nicSecurity": true, "mac": "00:11:22:33:44:55",
			"ipv4": "10.0.0.2", "ipv6": "fd00::2", "publicIp": "192.0.2.2",
		}},
	}
}
