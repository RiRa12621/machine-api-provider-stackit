// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	providerv1 "github.com/RiRa12621/machine-api-provider-stackit/api/v1alpha1"
	"github.com/RiRa12621/machine-api-provider-stackit/pkg/cloud"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	maomachine "github.com/openshift/machine-api-operator/pkg/controller/machine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testProjectID  = "11111111-1111-4111-8111-111111111111"
	testServerID   = "22222222-2222-4222-8222-222222222222"
	testNetworkID  = "33333333-3333-4333-8333-333333333333"
	testImageID    = "44444444-4444-4444-8444-444444444444"
	testVolumeID   = "55555555-5555-4555-8555-555555555555"
	testGroupID    = "66666666-6666-4666-8666-666666666666"
	testMachineUID = "77777777-7777-4777-8777-777777777777"
)

func TestCreatePersistsIntentBeforeCloudRequest(t *testing.T) {
	f := newFixture(t)
	f.cloud.create = func(_ context.Context, input cloud.CreateServerInput) (*cloud.Server, error) {
		status := f.status()
		if !status.CreatePending || status.MachineUID != testMachineUID || status.ProjectID != testProjectID || status.Region != "eu01" || status.SpecHash == "" {
			t.Fatalf("server requested without durable creation identity: %#v", status)
		}
		if status.InstanceID != "" || f.current().Spec.ProviderID != nil {
			t.Fatal("server identity was published before the cloud request")
		}
		if !bytes.Equal(input.UserData, f.userData.Data[UserDataKey]) {
			t.Fatal("actuator rewrote or encoded raw Ignition")
		}
		if input.Labels[MachineUIDLabel] != testMachineUID || input.Labels[ClusterIDLabel] != "cluster-id" || input.Labels[ProviderLabel] != "stackit" {
			t.Fatalf("creation omitted immutable ownership labels: %#v", input.Labels)
		}
		if input.NetworkID != testNetworkID || input.ImageID != testImageID || input.RootVolume.SizeGiB != 32 || input.Name != "openshift-"+testMachineUID {
			t.Fatalf("unexpected create input: %#v", input)
		}
		return &cloud.Server{ID: testServerID, Status: "ACTIVE", PowerStatus: "RUNNING"}, nil
	}
	requireRequeue(t, f.actuator.Create(t.Context(), f.machine))
	if status := f.status(); status.InstanceID != testServerID || !status.CreatePending {
		t.Fatalf("create response was not journaled pending detailed verification: %#v", status)
	}
	if machine := f.current(); machine.Spec.ProviderID != nil || len(machine.Status.Addresses) != 0 {
		t.Fatal("sparse create response prematurely published readiness")
	}
}

func TestCreateUncertainOutcomeNeverDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name             string
		response         *cloud.Server
		createError      error
		failStatusWrite  bool
		concurrentChange bool
	}{
		{name: "cloud timeout", createError: cloud.ErrTransient},
		{name: "cloud rejection", createError: cloud.ErrUnauthorized},
		{name: "nil response"},
		{name: "response without ID", response: &cloud.Server{}},
		{name: "lost status write", response: &cloud.Server{ID: testServerID}, failStatusWrite: true},
		{name: "Machine revision changes during request", response: &cloud.Server{ID: testServerID}, concurrentChange: true},
	} {
		t.Run("When "+tc.name+", it should recover only through UID labels", func(t *testing.T) {
			f := newFixture(t)
			if tc.failStatusWrite {
				writes := 0
				f.actuator.client = interceptor.NewClient(f.kube, interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					writes++
					if writes == 2 {
						return errors.New("status unavailable")
					}
					return c.SubResource(sub).Patch(ctx, object, patch, opts...)
				}})
			}
			f.cloud.create = func(context.Context, cloud.CreateServerInput) (*cloud.Server, error) {
				if tc.concurrentChange {
					machine := f.current()
					machine.Annotations = map[string]string{"another-controller": "changed"}
					if err := f.kube.Update(t.Context(), machine); err != nil {
						t.Fatal(err)
					}
				}
				return tc.response, tc.createError
			}
			if err := f.actuator.Create(t.Context(), f.machine); err == nil {
				t.Fatal("uncertain create unexpectedly succeeded")
			}
			if !f.status().CreatePending {
				t.Fatal("uncertain create lost its durable intent")
			}
			if err := f.actuator.Create(t.Context(), f.machine); err == nil {
				t.Fatal("ambiguous retry unexpectedly succeeded")
			}
			if f.cloud.count("create") != 1 {
				t.Fatalf("created %d servers after uncertain outcome", f.cloud.count("create"))
			}
			f.cloud.list = func(context.Context, map[string]string) ([]cloud.Server, error) {
				return []cloud.Server{*f.server()}, nil
			}
			if err := f.actuator.Create(t.Context(), f.machine); err != nil {
				t.Fatalf("UID recovery failed: %v", err)
			}
			if status := f.status(); status.CreatePending || status.InstanceID != testServerID || status.RootVolumeID != testVolumeID {
				t.Fatalf("recovery did not preserve cleanup identity: %#v", status)
			}
			if f.cloud.count("create") != 1 {
				t.Fatal("recovery created a duplicate server")
			}
		})
	}
}

func TestCreateRequiresSuccessfulIntentWrite(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "When status storage fails, it should not call IaaS", true: "When another writer wins, it should not call IaaS"}[conflict], func(t *testing.T) {
			f := newFixture(t)
			f.actuator.client = interceptor.NewClient(f.kube, interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if !conflict {
					return errors.New("cannot persist intent")
				}
				machine := f.current()
				machine.Annotations = map[string]string{"concurrent-write": "true"}
				if err := f.kube.Update(ctx, machine); err != nil {
					t.Fatal(err)
				}
				return c.SubResource(sub).Patch(ctx, object, patch, opts...)
			}})
			if err := f.actuator.Create(t.Context(), f.machine); err == nil {
				t.Fatal("expected failed journal write")
			}
			if f.cloud.count("create") != 0 || f.status().CreatePending {
				t.Fatal("cloud request proceeded without durable exclusive intent")
			}
		})
	}
}

func TestCreatePreflight(t *testing.T) {
	for _, state := range []string{"CREATING", "DELETING", "DELETED", "FAILED", ""} {
		t.Run("When network is "+state+", it should retain the ability to retry without submitting", func(t *testing.T) {
			f := newFixture(t)
			f.cloud.network = func(context.Context, string) (*cloud.Network, error) {
				return &cloud.Network{ID: testNetworkID, Status: state}, nil
			}
			if err := f.actuator.Create(t.Context(), f.machine); err == nil {
				t.Fatal("unusable network accepted")
			}
			if f.cloud.count("create") != 0 || f.status().CreatePending {
				t.Fatal("unusable network consumed creation intent")
			}
		})
	}
	for _, data := range []string{"", `{}`, `{"ignition":{"version":"2.2.0"}}`, `{"ignition":{"version":"3."}}`, `{"ignition":{"version":"3.garbage"}}`, strings.Repeat("x", 65536)} {
		t.Run("When Ignition is invalid, it should fail before creation intent", func(t *testing.T) {
			f := newFixture(t)
			secret := f.userData.DeepCopy()
			secret.Data[UserDataKey] = []byte(data)
			if err := f.kube.Update(t.Context(), secret); err != nil {
				t.Fatal(err)
			}
			if err := f.actuator.Create(t.Context(), f.machine); err == nil {
				t.Fatal("invalid Ignition accepted")
			}
			if f.cloud.count("create") != 0 || f.status().CreatePending {
				t.Fatal("invalid Ignition consumed creation intent")
			}
		})
	}
}

func TestUIDRecoveryRejectsAmbiguousServers(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		foreignOnly, multiple bool
	}{
		{name: "only a foreign Machine server exists", foreignOnly: true},
		{name: "multiple servers have the same identity", multiple: true},
	} {
		t.Run("When "+tc.name+", it should refuse adoption and creation", func(t *testing.T) {
			f := newFixture(t)
			f.persistStatus(f.journal("", "", true))
			f.cloud.list = func(context.Context, map[string]string) ([]cloud.Server, error) {
				owned := f.server()
				if tc.foreignOnly {
					owned.Labels[MachineUIDLabel] = "someone-else"
					return []cloud.Server{*owned}, nil
				}
				other := f.server()
				other.ID = testGroupID
				return []cloud.Server{*owned, *other}, nil
			}
			if err := f.actuator.Create(t.Context(), f.machine); err == nil {
				t.Fatal("unsafe recovery succeeded")
			}
			if f.cloud.count("create") != 0 || f.cloud.count("delete") != 0 {
				t.Fatal("unsafe recovery mutated cloud resources")
			}
			if !f.status().CreatePending || f.current().Spec.ProviderID != nil {
				t.Fatal("unsafe recovery cleared intent or published identity")
			}
		})
	}
}

func TestOwnershipAndImmutableIdentity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        func(*providerv1.STACKITMachineProviderStatus)
		machine       func(*machinev1.Machine)
		server        func(*cloud.Server)
		missingServer bool
	}{
		{name: "persisted Machine UID differs", status: func(s *providerv1.STACKITMachineProviderStatus) { s.MachineUID = "foreign" }},
		{name: "persisted project differs", status: func(s *providerv1.STACKITMachineProviderStatus) { s.ProjectID = testNetworkID }},
		{name: "persisted region differs", status: func(s *providerv1.STACKITMachineProviderStatus) { s.Region = "eu02" }},
		{name: "persisted identity is incomplete", status: func(s *providerv1.STACKITMachineProviderStatus) { s.SpecHash = "" }},
		{name: "providerID contradicts tracked server", machine: func(m *machinev1.Machine) { id := "stackit://" + testGroupID; m.Spec.ProviderID = &id }},
		{name: "providerID contradicts absent tracked server", machine: func(m *machinev1.Machine) { id := "stackit://" + testGroupID; m.Spec.ProviderID = &id }, missingServer: true},
		{name: "server UID label differs", server: func(s *cloud.Server) { s.Labels[MachineUIDLabel] = "foreign" }},
		{name: "server cluster label differs", server: func(s *cloud.Server) { s.Labels[ClusterIDLabel] = "foreign" }},
		{name: "server provider label differs", server: func(s *cloud.Server) { s.Labels[ProviderLabel] = "foreign" }},
		{name: "server type differs", server: func(s *cloud.Server) { s.MachineType = "c2i.8" }},
		{name: "server zone differs", server: func(s *cloud.Server) { s.AvailabilityZone = "eu01-2" }},
		{name: "server network differs", server: func(s *cloud.Server) { s.NetworkIDs = []string{testGroupID} }},
		{name: "server image differs", server: func(s *cloud.Server) { s.ImageID = testGroupID }},
		{name: "server root disk differs", server: func(s *cloud.Server) { s.BootVolume.ID = testGroupID }},
	} {
		t.Run("When "+tc.name+", it should block destructive cleanup", func(t *testing.T) {
			f := newFixture(t)
			status := f.journal(testServerID, testVolumeID, false)
			if tc.status != nil {
				tc.status(status)
			}
			f.persistStatus(status)
			if tc.machine != nil {
				m := f.current()
				tc.machine(m)
				if err := f.kube.Update(t.Context(), m); err != nil {
					t.Fatal(err)
				}
			}
			f.terminate()
			f.cloud.get = func(context.Context, string) (*cloud.Server, error) {
				if tc.missingServer {
					return nil, cloud.ErrNotFound
				}
				server := f.server()
				if tc.server != nil {
					tc.server(server)
				}
				return server, nil
			}
			if err := f.actuator.Delete(t.Context(), f.machine); err == nil {
				t.Fatal("contradictory ownership allowed cleanup completion")
			}
			if f.cloud.count("delete") != 0 || f.cloud.count("create") != 0 {
				t.Fatal("contradictory ownership caused a cloud mutation")
			}
		})
	}
	t.Run("When immutable provider configuration changes, it should fail before cloud access", func(t *testing.T) {
		f := newFixture(t)
		f.persistStatus(f.journal(testServerID, testVolumeID, false))
		m := f.current()
		spec := f.spec.DeepCopy()
		spec.MachineType = "c2i.8"
		m.Spec.ProviderSpec.Value = encodeSpec(t, spec)
		if err := f.kube.Update(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if err := f.actuator.Update(t.Context(), f.machine); err == nil {
			t.Fatal("immutable cloud configuration change accepted")
		}
		if len(f.cloud.calls) != 0 || len(f.credentials) != 0 {
			t.Fatal("immutable change reached cloud client initialization")
		}
	})
}

func TestObservePublishesOnlyReadyServers(t *testing.T) {
	for _, tc := range []struct {
		name, status, power string
		addresses           []cloud.Address
		ready               bool
	}{
		{name: "active running with IPv4", status: "ACTIVE", power: "RUNNING", addresses: []cloud.Address{{Type: "InternalIP", Address: "10.0.0.2"}}, ready: true},
		{name: "active running with IPv6", status: "ACTIVE", power: "RUNNING", addresses: []cloud.Address{{Type: "InternalIP", Address: "fd00::2"}}, ready: true},
		{name: "still creating", status: "CREATING", power: "RUNNING", addresses: []cloud.Address{{Type: "InternalIP", Address: "10.0.0.2"}}},
		{name: "stopped", status: "ACTIVE", power: "STOPPED", addresses: []cloud.Address{{Type: "InternalIP", Address: "10.0.0.2"}}},
		{name: "power status unknown", status: "ACTIVE", addresses: []cloud.Address{{Type: "InternalIP", Address: "10.0.0.2"}}},
		{name: "no address", status: "ACTIVE", power: "RUNNING"},
		{name: "invalid address", status: "ACTIVE", power: "RUNNING", addresses: []cloud.Address{{Type: "InternalIP", Address: "not-an-ip"}}},
	} {
		t.Run("When "+tc.name+", it should publish readiness conservatively", func(t *testing.T) {
			f := newFixture(t)
			f.persistStatus(f.journal(testServerID, "", true))
			f.cloud.get = func(context.Context, string) (*cloud.Server, error) {
				s := f.server()
				s.Status = tc.status
				s.PowerStatus = tc.power
				s.Addresses = tc.addresses
				return s, nil
			}
			err := f.actuator.Update(t.Context(), f.machine)
			if tc.ready {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireRequeue(t, err)
			}
			m := f.current()
			if tc.ready {
				if m.Spec.ProviderID == nil || *m.Spec.ProviderID != "stackit://"+testServerID || len(m.Status.Addresses) != 2 {
					t.Fatalf("ready identity not published: %#v", m)
				}
				if m.Labels[maomachine.MachineRegionLabelName] != "eu01" || m.Labels[maomachine.MachineAZLabelName] != "eu01-1" || m.Labels[maomachine.MachineInstanceTypeLabelName] != "c2i.4" {
					t.Fatal("Machine placement metadata missing")
				}
			} else if m.Spec.ProviderID != nil || len(m.Status.Addresses) != 0 {
				t.Fatal("unready server published providerID or addresses")
			}
			if s := f.status(); s.CreatePending || s.RootVolumeID != testVolumeID {
				t.Fatalf("observed server lost cleanup journal: %#v", s)
			}
		})
	}
}

func TestExistsDistinguishesAbsenceFromCloudFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cloudError error
		wantError  bool
	}{
		{name: "not found", cloudError: cloud.ErrNotFound},
		{name: "forbidden", cloudError: cloud.ErrUnauthorized, wantError: true},
		{name: "temporary outage", cloudError: cloud.ErrTransient, wantError: true},
	} {
		t.Run("When GET returns "+tc.name+", it should distinguish absence from uncertainty", func(t *testing.T) {
			f := newFixture(t)
			f.persistStatus(f.journal(testServerID, testVolumeID, false))
			f.cloud.get = func(context.Context, string) (*cloud.Server, error) { return nil, tc.cloudError }
			exists, err := f.actuator.Exists(t.Context(), f.machine)
			if exists || (err != nil) != tc.wantError {
				t.Fatalf("exists=%v, error=%v", exists, err)
			}
			if err := f.actuator.Create(t.Context(), f.machine); err == nil {
				t.Fatal("lost provisioned server was silently recreated")
			}
			if f.cloud.count("create") != 0 {
				t.Fatal("GET failure caused duplicate creation")
			}
		})
	}
}

func TestDeleteWaitsForServerAndRootVolume(t *testing.T) {
	f := newFixture(t)
	// Deletion may begin immediately after create, before the detailed GET
	// has discovered and journaled the automatically deleted root disk.
	f.persistStatus(f.journal(testServerID, "", true))
	f.terminate()
	state := "ACTIVE"
	f.cloud.get = func(context.Context, string) (*cloud.Server, error) {
		if state == "gone" {
			return nil, cloud.ErrNotFound
		}
		s := f.server()
		s.Status = state
		return s, nil
	}
	volumeExists := true
	f.cloud.volume = func(context.Context, string) (*cloud.Volume, error) {
		if volumeExists {
			return &cloud.Volume{ID: testVolumeID, Status: "DELETING"}, nil
		}
		return nil, cloud.ErrNotFound
	}
	f.cloud.delete = func(context.Context, string) error {
		if f.status().RootVolumeID != testVolumeID {
			t.Fatal("server deletion started without durable root identity")
		}
		return nil
	}
	requireRequeue(t, f.actuator.Delete(t.Context(), f.machine))
	state = "DELETING"
	requireRequeue(t, f.actuator.Delete(t.Context(), f.machine))
	if f.cloud.count("delete") != 1 {
		t.Fatal("deletion was resubmitted while already in progress")
	}
	state = "gone"
	requireRequeue(t, f.actuator.Delete(t.Context(), f.machine))
	if f.cloud.count("volume") != 1 {
		t.Fatal("server absence did not trigger root-volume verification")
	}
	volumeExists = false
	if err := f.actuator.Delete(t.Context(), f.machine); err != nil {
		t.Fatalf("cleanup did not finish after both resources disappeared: %v", err)
	}
	if f.cloud.count("delete") != 1 {
		t.Fatal("root-disk observation caused unexpected server mutation")
	}
}

func TestDeleteRequiresDurableRecoveredIdentity(t *testing.T) {
	for _, failJournal := range []bool{false, true} {
		name := "When only creation intent survived, it should journal the UID-recovered root before deletion"
		if failJournal {
			name = "When recovery status cannot be persisted, it should preserve the server"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.persistStatus(f.journal("", "", true))
			f.terminate()
			f.cloud.list = func(context.Context, map[string]string) ([]cloud.Server, error) {
				return []cloud.Server{*f.server()}, nil
			}
			if failJournal {
				f.actuator.client = interceptor.NewClient(f.kube, interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
					return errors.New("cannot persist recovered identity")
				}})
			}
			f.cloud.delete = func(context.Context, string) error {
				status := f.status()
				if status.InstanceID != testServerID || status.RootVolumeID != testVolumeID || status.CreatePending {
					t.Fatal("destructive cleanup preceded its durable identity journal")
				}
				return nil
			}
			err := f.actuator.Delete(t.Context(), f.machine)
			if failJournal {
				if err == nil || f.cloud.count("delete") != 0 || !f.status().CreatePending {
					t.Fatal("cleanup proceeded despite an unpersisted identity")
				}
			} else {
				requireRequeue(t, err)
				if f.cloud.count("delete") != 1 {
					t.Fatal("UID recovery did not resume cleanup")
				}
			}
		})
	}
}

func TestDeleteRefusesUnverifiedCleanup(t *testing.T) {
	for _, tc := range []struct {
		name         string
		change       func(*cloud.Server)
		volumeError  error
		serverAbsent bool
	}{
		{name: "additional workload volume attached", change: func(s *cloud.Server) { s.VolumeIDs = append(s.VolumeIDs, testGroupID) }},
		{name: "root deletion disabled", change: func(s *cloud.Server) { s.BootVolume.DeleteOnTermination = false }},
		{name: "root identity missing", change: func(s *cloud.Server) { s.BootVolume = nil }},
		{name: "root query forbidden", serverAbsent: true, volumeError: cloud.ErrUnauthorized},
	} {
		t.Run("When "+tc.name+", it should retain cleanup responsibility", func(t *testing.T) {
			f := newFixture(t)
			f.persistStatus(f.journal(testServerID, testVolumeID, false))
			f.terminate()
			f.cloud.get = func(context.Context, string) (*cloud.Server, error) {
				if tc.serverAbsent {
					return nil, cloud.ErrNotFound
				}
				s := f.server()
				if tc.change != nil {
					tc.change(s)
				}
				return s, nil
			}
			f.cloud.volume = func(context.Context, string) (*cloud.Volume, error) { return nil, tc.volumeError }
			if err := f.actuator.Delete(t.Context(), f.machine); err == nil {
				t.Fatal("unverified cleanup succeeded")
			}
			if f.cloud.count("delete") != 0 {
				t.Fatal("unsafe destructive action was requested")
			}
		})
	}
	t.Run("When creation is still uncertain, it should not finish deletion", func(t *testing.T) {
		f := newFixture(t)
		f.persistStatus(f.journal("", "", true))
		f.terminate()
		if err := f.actuator.Delete(t.Context(), f.machine); err == nil {
			t.Fatal("uncertain creation was abandoned")
		}
		if f.cloud.count("delete") != 0 || !f.status().CreatePending {
			t.Fatal("uncertain cleanup changed ownership")
		}
	})
}

func TestCredentialsRecoveryDuringDeletion(t *testing.T) {
	for _, tc := range []struct {
		name                                               string
		data                                               []byte
		missing, wrongProject, terminating, factoryFailure bool
	}{
		{name: "missing", missing: true}, {name: "malformed", data: []byte("invalid")}, {name: "empty", data: []byte(`{}`)},
		{name: "null", data: []byte(`null`)}, {name: "wrong project", data: []byte(`{"key":"old"}`), wrongProject: true},
		{name: "terminating", data: []byte(`{"key":"old"}`), terminating: true}, {name: "invalid key", data: []byte(`{"key":"old"}`), factoryFailure: true},
	} {
		t.Run("When credentials are "+tc.name+", it should wait for restoration", func(t *testing.T) {
			f := newFixture(t)
			f.persistStatus(f.journal(testServerID, testVolumeID, false))
			f.terminate()
			secret := f.credentialsSecret()
			if tc.missing {
				if err := f.kube.Delete(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
			} else {
				secret.Data[CredentialsKey] = tc.data
				if tc.wrongProject {
					secret.Data["project-id"] = []byte(testGroupID)
				}
				if tc.terminating {
					secret.Finalizers = []string{"test/hold"}
				}
				if err := f.kube.Update(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
				if tc.terminating {
					if err := f.kube.Delete(t.Context(), secret); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.factoryFailure {
				f.factoryError = cloud.ErrUnauthorized
			}
			if err := f.actuator.Delete(t.Context(), f.machine); err == nil {
				t.Fatal("cleanup ignored unavailable credentials")
			}
			if len(f.cloud.calls) != 0 || f.status().InstanceID != testServerID {
				t.Fatal("credential failure changed cloud resources or discarded identity")
			}
			if tc.terminating {
				secret = f.credentialsSecret()
				secret.Finalizers = nil
				if err := f.kube.Update(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
			}
			if tc.missing || tc.terminating {
				secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: f.spec.CredentialsSecret.Name, Namespace: f.machine.Namespace}, Data: map[string][]byte{CredentialsKey: []byte(`{"key":"rotated"}`)}}
				if err := f.kube.Create(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
			} else {
				secret = f.credentialsSecret()
				secret.Data = map[string][]byte{CredentialsKey: []byte(`{"key":"rotated"}`)}
				if err := f.kube.Update(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
			}
			f.factoryError = nil
			f.cloud.get = func(context.Context, string) (*cloud.Server, error) { return f.server(), nil }
			requireRequeue(t, f.actuator.Delete(t.Context(), f.machine))
			if f.cloud.count("delete") != 1 || string(f.credentials[len(f.credentials)-1].ServiceAccountJSON) != `{"key":"rotated"}` {
				t.Fatal("restored credentials did not resume cleanup")
			}
		})
	}
}

func TestNeverProvisionedDeletionNeedsNoCredentials(t *testing.T) {
	f := newFixture(t)
	if err := f.kube.Delete(t.Context(), f.credentialsSecret()); err != nil {
		t.Fatal(err)
	}
	m := f.current()
	m.Spec.ProviderSpec.Value = nil
	if err := f.kube.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	f.terminate()
	if exists, err := f.actuator.Exists(t.Context(), f.machine); exists || err != nil {
		t.Fatalf("never-created Machine cannot be deleted: exists=%v, error=%v", exists, err)
	}
	if err := f.actuator.Delete(t.Context(), f.machine); err != nil {
		t.Fatal(err)
	}
	if len(f.cloud.calls) != 0 || len(f.credentials) != 0 {
		t.Fatal("never-created deletion accessed cloud credentials")
	}
}

func TestFreshReadsAllowCredentialAndSecretReferenceRotation(t *testing.T) {
	f := newFixture(t)
	f.persistStatus(f.journal(testServerID, testVolumeID, false))
	newSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "new-credentials", Namespace: f.machine.Namespace}, Data: map[string][]byte{CredentialsKey: []byte(`{"key":"new"}`)}}
	if err := f.kube.Create(t.Context(), newSecret); err != nil {
		t.Fatal(err)
	}
	m := f.current()
	spec := f.spec.DeepCopy()
	spec.CredentialsSecret.Name = "new-credentials"
	spec.UserDataSecret.Name = "no-longer-needed"
	m.Spec.ProviderSpec.Value = encodeSpec(t, spec)
	if err := f.kube.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	// The cache still has the old Machine. Reconciliation must use API-reader state.
	f.cloud.get = func(context.Context, string) (*cloud.Server, error) { return f.server(), nil }
	if err := f.actuator.Update(t.Context(), f.machine); err != nil {
		t.Fatal(err)
	}
	if got := string(f.credentials[len(f.credentials)-1].ServiceAccountJSON); got != `{"key":"new"}` {
		t.Fatalf("stale credentials used: %s", got)
	}
	if f.cloud.count("create") != 0 {
		t.Fatal("Secret reference rotation recreated the server")
	}
}

type fixture struct {
	t            *testing.T
	kube         client.WithWatch
	actuator     *Actuator
	machine      *machinev1.Machine
	spec         *providerv1.STACKITMachineProviderSpec
	userData     *corev1.Secret
	cloud        *fakeCloud
	credentials  []cloud.Credentials
	factoryError error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := machinev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, cloud: &fakeCloud{}, spec: &providerv1.STACKITMachineProviderSpec{
		ProjectID: testProjectID, Region: "eu01", ImageID: testImageID, NetworkID: testNetworkID, MachineType: "c2i.4", AvailabilityZone: "eu01-1",
		RootVolume: providerv1.RootVolume{SizeGiB: 32}, SecurityGroups: []string{testGroupID},
		CredentialsSecret: &corev1.LocalObjectReference{Name: "stackit-credentials"}, UserDataSecret: &corev1.LocalObjectReference{Name: "worker-user-data"},
	}}
	f.machine = &machinev1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "openshift-machine-api", UID: types.UID(testMachineUID), Labels: map[string]string{ClusterLabel: "cluster-id"}, Finalizers: []string{"machine.openshift.io/machine"}}}
	f.machine.Spec.ProviderSpec.Value = encodeSpec(t, f.spec)
	credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: f.spec.CredentialsSecret.Name, Namespace: f.machine.Namespace}, Data: map[string][]byte{CredentialsKey: []byte(`{"key":"initial"}`), "project-id": []byte(testProjectID)}}
	f.userData = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: f.spec.UserDataSecret.Name, Namespace: f.machine.Namespace}, Data: map[string][]byte{UserDataKey: []byte("{\n  \"ignition\": {\"version\": \"3.4.0\"}\n}\n")}}
	f.kube = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&machinev1.Machine{}).WithObjects(f.machine, credentials, f.userData).Build()
	f.actuator = New(f.kube, f.kube, func(_ context.Context, creds cloud.Credentials) (cloud.Client, error) {
		f.credentials = append(f.credentials, creds)
		if f.factoryError != nil {
			return nil, f.factoryError
		}
		return f.cloud, nil
	})
	f.machine = f.current()
	if err := f.kube.Get(t.Context(), client.ObjectKeyFromObject(f.userData), f.userData); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) current() *machinev1.Machine {
	f.t.Helper()
	m := &machinev1.Machine{}
	if err := f.kube.Get(f.t.Context(), client.ObjectKeyFromObject(f.machine), m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fixture) credentialsSecret() *corev1.Secret {
	f.t.Helper()
	secret := &corev1.Secret{}
	if err := f.kube.Get(f.t.Context(), client.ObjectKey{Namespace: f.machine.Namespace, Name: f.spec.CredentialsSecret.Name}, secret); err != nil {
		f.t.Fatal(err)
	}
	return secret
}

func (f *fixture) status() *providerv1.STACKITMachineProviderStatus {
	f.t.Helper()
	status, err := providerv1.DecodeStatus(f.current().Status.ProviderStatus)
	if err != nil {
		f.t.Fatal(err)
	}
	return status
}

func (f *fixture) journal(instanceID, rootID string, pending bool) *providerv1.STACKITMachineProviderStatus {
	f.t.Helper()
	hash, err := providerv1.CloudConfigHash(f.spec)
	if err != nil {
		f.t.Fatal(err)
	}
	return &providerv1.STACKITMachineProviderStatus{InstanceID: instanceID, RootVolumeID: rootID, CreatePending: pending, MachineUID: testMachineUID, ProjectID: testProjectID, Region: "eu01", SpecHash: hash}
}

func (f *fixture) persistStatus(status *providerv1.STACKITMachineProviderStatus) {
	f.t.Helper()
	m := f.current()
	raw, err := providerv1.EncodeStatus(status)
	if err != nil {
		f.t.Fatal(err)
	}
	m.Status.ProviderStatus = raw
	if err := f.kube.Status().Update(f.t.Context(), m); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) terminate() {
	f.t.Helper()
	if err := f.kube.Delete(f.t.Context(), f.current()); err != nil {
		f.t.Fatal(err)
	}
	f.machine = f.current()
}

func (f *fixture) server() *cloud.Server {
	return &cloud.Server{ID: testServerID, Name: "openshift-" + testMachineUID, Status: "ACTIVE", PowerStatus: "RUNNING", MachineType: f.spec.MachineType, AvailabilityZone: f.spec.AvailabilityZone, ImageID: f.spec.ImageID,
		Labels: map[string]string{MachineUIDLabel: testMachineUID, ClusterIDLabel: "cluster-id", ProviderLabel: "stackit"}, NetworkIDs: []string{testNetworkID}, Addresses: []cloud.Address{{Type: "InternalIP", Address: "10.0.0.2"}},
		BootVolume: &cloud.BootVolume{ID: testVolumeID, DeleteOnTermination: true}, VolumeIDs: []string{testVolumeID}}
}

func encodeSpec(t *testing.T, spec *providerv1.STACKITMachineProviderSpec) *runtime.RawExtension {
	t.Helper()
	raw, err := providerv1.EncodeSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireRequeue(t *testing.T, err error) {
	t.Helper()
	var pending *maomachine.RequeueAfterError
	if !errors.As(err, &pending) || pending.RequeueAfter <= 0 {
		t.Fatalf("expected bounded async requeue, got %v", err)
	}
}

type fakeCloud struct {
	calls   []string
	get     func(context.Context, string) (*cloud.Server, error)
	list    func(context.Context, map[string]string) ([]cloud.Server, error)
	create  func(context.Context, cloud.CreateServerInput) (*cloud.Server, error)
	delete  func(context.Context, string) error
	network func(context.Context, string) (*cloud.Network, error)
	volume  func(context.Context, string) (*cloud.Volume, error)
}

func (f *fakeCloud) GetServer(ctx context.Context, id string) (*cloud.Server, error) {
	f.calls = append(f.calls, "get")
	if f.get != nil {
		return f.get(ctx, id)
	}
	return nil, cloud.ErrNotFound
}
func (f *fakeCloud) ListServers(ctx context.Context, labels map[string]string) ([]cloud.Server, error) {
	f.calls = append(f.calls, "list")
	if f.list != nil {
		return f.list(ctx, labels)
	}
	return nil, nil
}
func (f *fakeCloud) CreateServer(ctx context.Context, in cloud.CreateServerInput) (*cloud.Server, error) {
	f.calls = append(f.calls, "create")
	if f.create != nil {
		return f.create(ctx, in)
	}
	return &cloud.Server{ID: testServerID}, nil
}
func (f *fakeCloud) DeleteServer(ctx context.Context, id string) error {
	f.calls = append(f.calls, "delete")
	if f.delete != nil {
		return f.delete(ctx, id)
	}
	return nil
}
func (f *fakeCloud) GetNetwork(ctx context.Context, id string) (*cloud.Network, error) {
	f.calls = append(f.calls, "network")
	if f.network != nil {
		return f.network(ctx, id)
	}
	return &cloud.Network{ID: id, Status: "CREATED"}, nil
}
func (f *fakeCloud) GetVolume(ctx context.Context, id string) (*cloud.Volume, error) {
	f.calls = append(f.calls, "volume")
	if f.volume != nil {
		return f.volume(ctx, id)
	}
	return nil, cloud.ErrNotFound
}
func (f *fakeCloud) count(method string) int {
	n := 0
	for _, call := range f.calls {
		if call == method {
			n++
		}
	}
	return n
}

var _ cloud.Client = (*fakeCloud)(nil)
