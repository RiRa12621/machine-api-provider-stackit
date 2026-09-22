// SPDX-License-Identifier: Apache-2.0

// Package actuator implements OpenShift Machine API lifecycle operations for STACKIT.
package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"time"

	providerv1 "github.com/RiRa12621/machine-api-provider-stackit/api/v1alpha1"
	"github.com/RiRa12621/machine-api-provider-stackit/pkg/cloud"
	ignition "github.com/coreos/ignition/v2/config/v3_6"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	maomachine "github.com/openshift/machine-api-operator/pkg/controller/machine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	CredentialsKey  = "serviceaccount.json"
	UserDataKey     = "userData"
	ClusterLabel    = "machine.openshift.io/cluster-api-cluster"
	MachineUIDLabel = "openshift-machine-uid"
	ClusterIDLabel  = "openshift-cluster-id"
	ProviderLabel   = "openshift-machine-provider"
	RetryDelay      = 30 * time.Second
)

type Actuator struct {
	client  client.Client
	reader  client.Reader
	factory cloud.Factory
}

var _ maomachine.Actuator = (*Actuator)(nil)

func New(c client.Client, reader client.Reader, factory cloud.Factory) *Actuator {
	return &Actuator{client: c, reader: reader, factory: factory}
}

type scope struct {
	machine *machinev1.Machine
	spec    *providerv1.STACKITMachineProviderSpec
	status  *providerv1.STACKITMachineProviderStatus
	cloud   cloud.Client
	labels  map[string]string
	hash    string
}

// load bypasses the cache for credentials and the creation journal. This avoids
// stale credentials after rotation and duplicate creates after a status write.
func (a *Actuator) load(ctx context.Context, m *machinev1.Machine) (*scope, error) {
	fresh := &machinev1.Machine{}
	if err := a.reader.Get(ctx, client.ObjectKeyFromObject(m), fresh); err != nil {
		return nil, fmt.Errorf("read Machine: %w", err)
	}
	if fresh.UID == "" || fresh.UID != m.UID {
		return nil, fmt.Errorf("machine identity changed or is missing")
	}
	spec, err := providerv1.DecodeSpec(fresh.Spec.ProviderSpec.Value)
	if err != nil {
		return nil, maomachine.InvalidMachineConfiguration("invalid STACKIT provider spec: %v", err)
	}
	if errs := providerv1.ValidateSpec(spec); len(errs) != 0 {
		return nil, maomachine.InvalidMachineConfiguration("invalid STACKIT provider spec: %v", errs.ToAggregate())
	}
	status, err := providerv1.DecodeStatus(fresh.Status.ProviderStatus)
	if err != nil {
		return nil, fmt.Errorf("invalid STACKIT provider status: %w", err)
	}
	hash, err := providerv1.CloudConfigHash(spec)
	if err != nil {
		return nil, err
	}
	clusterID := fresh.Labels[ClusterLabel]
	if clusterID == "" || len(clusterID) > 63 {
		return nil, maomachine.InvalidMachineConfiguration("Machine requires a cluster identity label of at most 63 characters")
	}
	if status.MachineUID != "" && status.MachineUID != string(fresh.UID) {
		return nil, fmt.Errorf("provider status belongs to a different Machine UID")
	}
	if status.ProjectID != "" && (status.ProjectID != spec.ProjectID || status.Region != spec.Region) {
		return nil, fmt.Errorf("project and region differ from the persisted infrastructure identity")
	}
	if status.SpecHash != "" && status.SpecHash != hash {
		return nil, fmt.Errorf("cloud configuration changed after creation intent; create a replacement Machine")
	}
	if (status.CreatePending || status.InstanceID != "" || status.RootVolumeID != "") && (status.ProjectID == "" || status.Region == "" || status.MachineUID == "" || status.SpecHash == "") {
		return nil, fmt.Errorf("provider status is missing its persisted infrastructure identity")
	}
	if status.InstanceID != "" && fresh.Spec.ProviderID != nil && *fresh.Spec.ProviderID != "" && *fresh.Spec.ProviderID != "stackit://"+status.InstanceID {
		return nil, fmt.Errorf("machine providerID contradicts its persisted STACKIT instance identity")
	}
	secret := &corev1.Secret{}
	if err := a.reader.Get(ctx, client.ObjectKey{Namespace: fresh.Namespace, Name: spec.CredentialsSecret.Name}, secret); err != nil {
		return nil, fmt.Errorf("read STACKIT credentials Secret: %w", err)
	}
	if !secret.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("STACKIT credentials Secret is terminating")
	}
	key := secret.Data[CredentialsKey]
	var document map[string]json.RawMessage
	if json.Unmarshal(key, &document) != nil || len(document) == 0 {
		return nil, fmt.Errorf("credentials Secret must contain a nonempty JSON object under %s", CredentialsKey)
	}
	if project, ok := secret.Data["project-id"]; ok && string(project) != spec.ProjectID {
		return nil, fmt.Errorf("credentials Secret project-id does not match provider spec")
	}
	cloudClient, err := a.factory(ctx, cloud.Credentials{ProjectID: spec.ProjectID, Region: spec.Region, ServiceAccountJSON: slices.Clone(key)})
	if err != nil {
		return nil, fmt.Errorf("initialize STACKIT client: %w", err)
	}
	return &scope{machine: fresh, spec: spec, status: status, cloud: cloudClient, hash: hash,
		labels: map[string]string{MachineUIDLabel: string(fresh.UID), ClusterIDLabel: clusterID, ProviderLabel: "stackit"}}, nil
}

func (a *Actuator) Exists(ctx context.Context, m *machinev1.Machine) (bool, error) {
	if done, err := a.unprovisionedDeletion(ctx, m); done || err != nil {
		return false, err
	}
	s, err := a.load(ctx, m)
	if err != nil {
		return false, err
	}
	server, err := s.findServer(ctx)
	if err != nil {
		return false, err
	}
	if server == nil && s.status.CreatePending {
		return false, fmt.Errorf("STACKIT creation outcome is uncertain; awaiting UID-labelled server discovery; do not clear creation intent before auditing cloud resources")
	}
	return server != nil, nil
}

func (a *Actuator) Create(ctx context.Context, m *machinev1.Machine) error {
	s, err := a.load(ctx, m)
	if err != nil {
		return err
	}
	if !s.machine.DeletionTimestamp.IsZero() {
		return fmt.Errorf("refusing creation for a terminating Machine")
	}
	server, err := s.findServer(ctx)
	if err != nil {
		return err
	}
	if server != nil {
		return a.observe(ctx, m, s, server)
	}
	if s.status.CreatePending || s.status.InstanceID != "" || s.machine.Spec.ProviderID != nil {
		return fmt.Errorf("prior STACKIT creation requires recovery; refusing to submit another server")
	}
	network, err := s.cloud.GetNetwork(ctx, s.spec.NetworkID)
	if err != nil {
		return fmt.Errorf("validate existing STACKIT network: %w", err)
	}
	if network == nil || network.ID != s.spec.NetworkID {
		return fmt.Errorf("STACKIT network identity does not match configuration")
	}
	if network.Status != "CREATED" && network.Status != "UPDATED" {
		return fmt.Errorf("STACKIT network is not ready for server creation")
	}
	userData, err := a.userData(ctx, s)
	if err != nil {
		return err
	}
	// Persist intent with optimistic locking BEFORE the non-idempotent request.
	// A lost response or failed subsequent status write is recovered by UID labels.
	s.status.ProjectID, s.status.Region = s.spec.ProjectID, s.spec.Region
	s.status.MachineUID, s.status.SpecHash = string(s.machine.UID), s.hash
	s.status.CreatePending = true
	setCondition(s, metav1.ConditionUnknown, "Creating", "STACKIT server creation is pending")
	if err := a.saveStatus(ctx, m, s); err != nil {
		return err
	}
	server, err = s.cloud.CreateServer(ctx, cloud.CreateServerInput{
		Name: "openshift-" + string(s.machine.UID), ImageID: s.spec.ImageID,
		MachineType: s.spec.MachineType, NetworkID: s.spec.NetworkID,
		AvailabilityZone: s.spec.AvailabilityZone, SSHKeyName: s.spec.SSHKeyName,
		SecurityGroups: slices.Clone(s.spec.SecurityGroups), Labels: s.labels, UserData: userData,
		RootVolume: cloud.RootVolume{SizeGiB: int64(s.spec.RootVolume.SizeGiB), PerformanceClass: s.spec.RootVolume.PerformanceClass},
	})
	if err != nil {
		// Even a rejected request is conservatively journaled. A retryable HTTP
		// result may have passed through a proxy after the server accepted it.
		return fmt.Errorf("STACKIT server creation requires recovery: %w", err)
	}
	if server == nil || server.ID == "" {
		return fmt.Errorf("STACKIT create response omitted server identity; awaiting UID-label recovery")
	}
	// The create response may be sparse. Persist its ID, then validate ownership
	// and readiness with a detailed GET before publishing providerID or addresses.
	s.status.InstanceID = server.ID
	if err := a.saveStatus(ctx, m, s); err != nil {
		return err
	}
	return &maomachine.RequeueAfterError{RequeueAfter: RetryDelay}
}

func (a *Actuator) Update(ctx context.Context, m *machinev1.Machine) error {
	s, err := a.load(ctx, m)
	if err != nil {
		return err
	}
	server, err := s.findServer(ctx)
	if err != nil {
		return err
	}
	if server == nil {
		return fmt.Errorf("STACKIT instance is not available")
	}
	return a.observe(ctx, m, s, server)
}

func (a *Actuator) Delete(ctx context.Context, m *machinev1.Machine) error {
	if done, err := a.unprovisionedDeletion(ctx, m); done || err != nil {
		return err
	}
	s, err := a.load(ctx, m)
	if err != nil {
		return err
	}
	server, err := s.findServer(ctx)
	if err != nil {
		return err
	}
	if server == nil {
		if s.status.CreatePending {
			return fmt.Errorf("creation outcome is uncertain; retaining Machine until UID-label recovery or a manual cloud audit")
		}
		if s.status.RootVolumeID != "" {
			_, err := s.cloud.GetVolume(ctx, s.status.RootVolumeID)
			if err == nil {
				return &maomachine.RequeueAfterError{RequeueAfter: RetryDelay}
			}
			if !errors.Is(err, cloud.ErrNotFound) {
				return fmt.Errorf("confirm root volume deletion: %w", err)
			}
		}
		return nil
	}
	if server.BootVolume == nil || server.BootVolume.ID == "" || !server.BootVolume.DeleteOnTermination {
		return fmt.Errorf("server has no verifiable automatically deleted root volume; refusing destructive cleanup")
	}
	for _, volumeID := range server.VolumeIDs {
		if volumeID != "" && volumeID != server.BootVolume.ID {
			return fmt.Errorf("server still has additional attached volumes; detach workload volumes before deleting the Machine")
		}
	}
	if err := a.rememberServer(ctx, m, s, server); err != nil {
		return err
	}
	if !strings.EqualFold(server.Status, "DELETING") {
		if err := s.cloud.DeleteServer(ctx, server.ID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
			return fmt.Errorf("delete STACKIT server: %w", err)
		}
	}
	// Never remove the Machine finalizer on an accepted asynchronous DELETE.
	return &maomachine.RequeueAfterError{RequeueAfter: RetryDelay}
}

func (s *scope) findServer(ctx context.Context) (*cloud.Server, error) {
	if s.status.InstanceID != "" {
		server, err := s.cloud.GetServer(ctx, s.status.InstanceID)
		if errors.Is(err, cloud.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("get STACKIT server: %w", err)
		}
		if server == nil || server.ID != s.status.InstanceID {
			return nil, fmt.Errorf("STACKIT server response identity mismatch")
		}
		if err := s.checkOwnership(server); err != nil {
			return nil, err
		}
		return server, nil
	}
	servers, err := s.cloud.ListServers(ctx, s.labels)
	if err != nil {
		return nil, fmt.Errorf("discover STACKIT server by Machine UID: %w", err)
	}
	var found *cloud.Server
	for i := range servers {
		matches := true
		for key, value := range s.labels {
			if servers[i].Labels[key] != value {
				matches = false
			}
		}
		if !matches {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple STACKIT servers share this Machine identity; manual recovery is required")
		}
		if err := s.checkOwnership(&servers[i]); err != nil {
			return nil, err
		}
		found = &servers[i]
	}
	if found == nil && s.machine.Spec.ProviderID != nil && *s.machine.Spec.ProviderID != "" {
		return nil, fmt.Errorf("machine has a providerID but no tracked or UID-labelled server; refusing unverified adoption")
	}
	return found, nil
}

func (s *scope) checkOwnership(server *cloud.Server) error {
	if server.ID == "" {
		return fmt.Errorf("STACKIT server identity is empty")
	}
	for key, value := range s.labels {
		if server.Labels[key] != value {
			return fmt.Errorf("STACKIT server ownership labels do not match this Machine")
		}
	}
	if s.machine.Spec.ProviderID != nil && *s.machine.Spec.ProviderID != "" && *s.machine.Spec.ProviderID != "stackit://"+server.ID {
		return fmt.Errorf("machine providerID does not match its owned STACKIT server")
	}
	if server.MachineType != s.spec.MachineType || server.AvailabilityZone != s.spec.AvailabilityZone || !slices.Contains(server.NetworkIDs, s.spec.NetworkID) {
		return fmt.Errorf("STACKIT server configuration differs from its Machine identity")
	}
	if server.ImageID != "" && server.ImageID != s.spec.ImageID {
		return fmt.Errorf("STACKIT server image differs from its Machine configuration")
	}
	if s.status.RootVolumeID != "" && (server.BootVolume == nil || server.BootVolume.ID != s.status.RootVolumeID) {
		return fmt.Errorf("STACKIT server root volume differs from its persisted identity")
	}
	return nil
}

func (a *Actuator) rememberServer(ctx context.Context, m *machinev1.Machine, s *scope, server *cloud.Server) error {
	s.status.InstanceID, s.status.ProjectID, s.status.Region = server.ID, s.spec.ProjectID, s.spec.Region
	s.status.MachineUID, s.status.SpecHash = string(s.machine.UID), s.hash
	// Persist the root disk BEFORE clearing uncertain creation. This allows
	// deletion retries to confirm cleanup even once the server disappears.
	if server.BootVolume == nil || server.BootVolume.ID == "" || !server.BootVolume.DeleteOnTermination {
		return fmt.Errorf("STACKIT server root-volume ownership/deletion policy is not yet verifiable")
	}
	s.status.RootVolumeID = server.BootVolume.ID
	s.status.CreatePending = false
	return a.saveStatus(ctx, m, s)
}

func (a *Actuator) observe(ctx context.Context, m *machinev1.Machine, s *scope, server *cloud.Server) error {
	if err := a.rememberServer(ctx, m, s, server); err != nil {
		return err
	}
	if !strings.EqualFold(server.Status, "ACTIVE") || !strings.EqualFold(server.PowerStatus, "RUNNING") {
		setCondition(s, metav1.ConditionFalse, "ServerNotActive", "STACKIT server is not ACTIVE and RUNNING")
		if err := a.saveStatus(ctx, m, s); err != nil {
			return err
		}
		return &maomachine.RequeueAfterError{RequeueAfter: RetryDelay}
	}
	addresses := []corev1.NodeAddress{{Type: corev1.NodeInternalDNS, Address: server.Name}}
	for _, address := range server.Addresses {
		if net.ParseIP(address.Address) == nil {
			continue
		}
		t := corev1.NodeInternalIP
		if address.Type == "ExternalIP" {
			t = corev1.NodeExternalIP
		}
		candidate := corev1.NodeAddress{Type: t, Address: address.Address}
		if !slices.Contains(addresses, candidate) {
			addresses = append(addresses, candidate)
		}
	}
	if len(addresses) < 2 {
		return &maomachine.RequeueAfterError{RequeueAfter: RetryDelay}
	}
	base := s.machine.DeepCopy()
	providerID := "stackit://" + server.ID
	s.machine.Spec.ProviderID = &providerID
	if s.machine.Labels == nil {
		s.machine.Labels = map[string]string{}
	}
	s.machine.Labels[maomachine.MachineRegionLabelName] = s.spec.Region
	s.machine.Labels[maomachine.MachineAZLabelName] = s.spec.AvailabilityZone
	s.machine.Labels[maomachine.MachineInstanceTypeLabelName] = s.spec.MachineType
	if s.machine.Annotations == nil {
		s.machine.Annotations = map[string]string{}
	}
	s.machine.Annotations[maomachine.MachineInstanceStateAnnotationName] = server.Status
	if !reflect.DeepEqual(base.Spec, s.machine.Spec) || !reflect.DeepEqual(base.Labels, s.machine.Labels) || !reflect.DeepEqual(base.Annotations, s.machine.Annotations) {
		if err := a.client.Patch(ctx, s.machine, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("persist STACKIT Machine metadata: %w", err)
		}
	}
	s.machine.Status.Addresses = addresses
	setCondition(s, metav1.ConditionTrue, "ServerReady", "STACKIT server is ACTIVE and has an IP address")
	return a.saveStatus(ctx, m, s)
}

func setCondition(s *scope, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&s.status.Conditions, metav1.Condition{Type: "InstanceReady", Status: status, Reason: reason, Message: message, ObservedGeneration: s.machine.Generation})
}

// Every cloud create is preceded by a durable status journal. A terminating
// Machine with no journal and no providerID never submitted a server request,
// so missing initial credentials must not prevent its Kubernetes deletion.
func (a *Actuator) unprovisionedDeletion(ctx context.Context, m *machinev1.Machine) (bool, error) {
	if m.DeletionTimestamp.IsZero() {
		return false, nil
	}
	fresh := &machinev1.Machine{}
	if err := a.reader.Get(ctx, client.ObjectKeyFromObject(m), fresh); err != nil {
		return false, err
	}
	if fresh.UID == "" || fresh.UID != m.UID {
		return false, fmt.Errorf("machine identity changed")
	}
	status, err := providerv1.DecodeStatus(fresh.Status.ProviderStatus)
	if err != nil {
		return false, err
	}
	return !status.CreatePending && status.InstanceID == "" && status.RootVolumeID == "" && status.MachineUID == "" && status.ProjectID == "" && status.Region == "" && status.SpecHash == "" && (fresh.Spec.ProviderID == nil || *fresh.Spec.ProviderID == ""), nil
}

func (a *Actuator) saveStatus(ctx context.Context, m *machinev1.Machine, s *scope) error {
	base := &machinev1.Machine{}
	if err := a.reader.Get(ctx, client.ObjectKeyFromObject(s.machine), base); err != nil {
		return err
	}
	// A new revision must be reconciled again, not overwritten by stale cloud
	// work. In particular, only one create-intent writer may proceed to IaaS.
	if base.UID != s.machine.UID || base.ResourceVersion != s.machine.ResourceVersion {
		return fmt.Errorf("machine changed during reconciliation; retry with fresh state")
	}
	status, err := providerv1.EncodeStatus(s.status)
	if err != nil {
		return err
	}
	s.machine.Status.ProviderStatus = status
	if !reflect.DeepEqual(base.Status, s.machine.Status) {
		if err := a.client.Status().Patch(ctx, s.machine, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("persist STACKIT lifecycle status: %w", err)
		}
	}
	*m = *s.machine.DeepCopy()
	return nil
}

func (a *Actuator) userData(ctx context.Context, s *scope) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := a.reader.Get(ctx, client.ObjectKey{Namespace: s.machine.Namespace, Name: s.spec.UserDataSecret.Name}, secret); err != nil {
		return nil, fmt.Errorf("read Ignition Secret: %w", err)
	}
	if !secret.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("ignition Secret is terminating")
	}
	data := secret.Data[UserDataKey]
	if len(data) == 0 || len(data) > 65535 {
		return nil, fmt.Errorf("userData must contain an Ignition v3 JSON document of at most 65535 bytes")
	}
	if _, report, err := ignition.ParseCompatibleVersion(data); err != nil || report.IsFatal() {
		// Reports can contain credentials embedded in Ignition URLs or headers.
		return nil, fmt.Errorf("userData is not a valid supported Ignition v3 configuration")
	}
	return slices.Clone(data), nil
}
