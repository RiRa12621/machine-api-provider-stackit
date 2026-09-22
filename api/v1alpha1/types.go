// Package v1alpha1 defines the STACKIT configuration embedded in Machine API objects.
// These are serialized configurations, not independently served Kubernetes resources.
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	GroupName                      = "stackitproviderconfig.openshift.io"
	SpecKind                       = "STACKITMachineProviderSpec"
	StatusKind                     = "STACKITMachineProviderStatus"
	DefaultRootVolumeSizeGiB int32 = 32
)

var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

// STACKITMachineProviderSpec is stored in Machine.spec.providerSpec.value.
// Credentials and user data are referenced in the Machine's own namespace.
type STACKITMachineProviderSpec struct {
	metav1.TypeMeta   `json:",inline"`
	ProjectID         string                       `json:"projectID"`
	Region            string                       `json:"region"`
	ImageID           string                       `json:"imageID"`
	MachineType       string                       `json:"machineType"`
	NetworkID         string                       `json:"networkID"`
	AvailabilityZone  string                       `json:"availabilityZone"`
	RootVolume        RootVolume                   `json:"rootVolume,omitempty"`
	SecurityGroups    []string                     `json:"securityGroups"`
	SSHKeyName        string                       `json:"sshKeyName,omitempty"`
	CredentialsSecret *corev1.LocalObjectReference `json:"credentialsSecret"`
	UserDataSecret    *corev1.LocalObjectReference `json:"userDataSecret"`
}

// RootVolume describes a boot disk that is deleted with its server.
type RootVolume struct {
	SizeGiB          int32  `json:"sizeGiB,omitempty"`
	PerformanceClass string `json:"performanceClass,omitempty"`
}

// STACKITMachineProviderStatus preserves ownership and recovery information in
// Machine.status.providerStatus. CreatePending records an uncertain create result
// so a reconciliation never blindly creates a second server after a timeout.
type STACKITMachineProviderStatus struct {
	metav1.TypeMeta `json:",inline"`
	InstanceID      string             `json:"instanceID,omitempty"`
	ProjectID       string             `json:"projectID,omitempty"`
	Region          string             `json:"region,omitempty"`
	MachineUID      string             `json:"machineUID,omitempty"`
	RootVolumeID    string             `json:"rootVolumeID,omitempty"`
	CreatePending   bool               `json:"createPending,omitempty"`
	SpecHash        string             `json:"specHash,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
}

// AddToScheme registers the versioned embedded provider configurations.
func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &STACKITMachineProviderSpec{}, &STACKITMachineProviderStatus{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

func (in *STACKITMachineProviderSpec) DeepCopy() *STACKITMachineProviderSpec {
	if in == nil {
		return nil
	}
	out := new(STACKITMachineProviderSpec)
	*out = *in
	out.SecurityGroups = append([]string(nil), in.SecurityGroups...)
	if in.CredentialsSecret != nil {
		out.CredentialsSecret = &corev1.LocalObjectReference{Name: in.CredentialsSecret.Name}
	}
	if in.UserDataSecret != nil {
		out.UserDataSecret = &corev1.LocalObjectReference{Name: in.UserDataSecret.Name}
	}
	return out
}

func (in *STACKITMachineProviderSpec) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

func (in *STACKITMachineProviderStatus) DeepCopy() *STACKITMachineProviderStatus {
	if in == nil {
		return nil
	}
	out := new(STACKITMachineProviderStatus)
	*out = *in
	out.Conditions = append([]metav1.Condition(nil), in.Conditions...)
	return out
}

func (in *STACKITMachineProviderStatus) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}
