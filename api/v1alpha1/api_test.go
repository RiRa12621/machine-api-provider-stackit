package v1alpha1

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func validSpec() *STACKITMachineProviderSpec {
	return &STACKITMachineProviderSpec{
		TypeMeta: typeMeta(SpecKind), ProjectID: "11111111-1111-4111-8111-111111111111", Region: "eu01",
		ImageID: "22222222-2222-4222-8222-222222222222", NetworkID: "33333333-3333-4333-8333-333333333333",
		MachineType: "c2i.4", AvailabilityZone: "eu01-1", RootVolume: RootVolume{SizeGiB: 32},
		SecurityGroups:    []string{"44444444-4444-4444-8444-444444444444"},
		CredentialsSecret: &corev1.LocalObjectReference{Name: "stackit-credentials"},
		UserDataSecret:    &corev1.LocalObjectReference{Name: "worker-user-data"},
	}
}

func TestValidateSpec(t *testing.T) {
	if errs := ValidateSpec(validSpec()); len(errs) > 0 {
		t.Fatal(errs)
	}
	if errs := ValidateSpec(nil); len(errs) == 0 {
		t.Fatal("nil spec accepted")
	}
	tests := []struct {
		name, field string
		mutate      func(*STACKITMachineProviderSpec)
	}{
		{"missing kind", "providerSpec", func(s *STACKITMachineProviderSpec) { s.Kind = "" }},
		{"wrong version", "providerSpec", func(s *STACKITMachineProviderSpec) { s.APIVersion = "stackitproviderconfig.openshift.io/v1beta1" }},
		{"bad project", "projectID", func(s *STACKITMachineProviderSpec) { s.ProjectID = "project" }},
		{"uppercase UUID", "networkID", func(s *STACKITMachineProviderSpec) { s.NetworkID = "AAAA1111-1111-4111-8111-111111111111" }},
		{"braced UUID", "imageID", func(s *STACKITMachineProviderSpec) { s.ImageID = "{" + s.ImageID + "}" }},
		{"bad region", "region", func(s *STACKITMachineProviderSpec) { s.Region = "eu-01" }},
		{"missing zone", "availabilityZone", func(s *STACKITMachineProviderSpec) { s.AvailabilityZone = "" }},
		{"different region", "availabilityZone", func(s *STACKITMachineProviderSpec) { s.AvailabilityZone = "eu02-1" }},
		{"zero zone", "availabilityZone", func(s *STACKITMachineProviderSpec) { s.AvailabilityZone = "eu01-0" }},
		{"empty type", "machineType", func(s *STACKITMachineProviderSpec) { s.MachineType = "" }},
		{"type whitespace", "machineType", func(s *STACKITMachineProviderSpec) { s.MachineType = "c2i.4 " }},
		{"type too long", "machineType", func(s *STACKITMachineProviderSpec) { s.MachineType = strings.Repeat("a", 64) }},
		{"small disk", "rootVolume.sizeGiB", func(s *STACKITMachineProviderSpec) { s.RootVolume.SizeGiB = 15 }},
		{"negative disk", "rootVolume.sizeGiB", func(s *STACKITMachineProviderSpec) { s.RootVolume.SizeGiB = -1 }},
		{"invalid disk class", "performanceClass", func(s *STACKITMachineProviderSpec) { s.RootVolume.PerformanceClass = "bad/class" }},
		{"invalid ssh key", "sshKeyName", func(s *STACKITMachineProviderSpec) { s.SSHKeyName = "bad\nkey" }},
		{"missing groups", "securityGroups", func(s *STACKITMachineProviderSpec) { s.SecurityGroups = nil }},
		{"invalid group", "securityGroups", func(s *STACKITMachineProviderSpec) { s.SecurityGroups = []string{"default"} }},
		{"duplicate group", "securityGroups", func(s *STACKITMachineProviderSpec) { s.SecurityGroups = append(s.SecurityGroups, s.SecurityGroups[0]) }},
		{"too many groups", "securityGroups", func(s *STACKITMachineProviderSpec) { s.SecurityGroups = make([]string, 65) }},
		{"missing credentials", "credentialsSecret", func(s *STACKITMachineProviderSpec) { s.CredentialsSecret = nil }},
		{"empty credential name", "credentialsSecret.name", func(s *STACKITMachineProviderSpec) { s.CredentialsSecret.Name = "" }},
		{"credential path", "credentialsSecret.name", func(s *STACKITMachineProviderSpec) { s.CredentialsSecret.Name = "other/credentials" }},
		{"missing userdata", "userDataSecret", func(s *STACKITMachineProviderSpec) { s.UserDataSecret = nil }},
		{"invalid userdata name", "userDataSecret.name", func(s *STACKITMachineProviderSpec) { s.UserDataSecret.Name = "UPPER" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validSpec()
			tt.mutate(spec)
			errs := ValidateSpec(spec)
			if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tt.field) {
				t.Fatalf("expected %s error, got %v", tt.field, errs)
			}
		})
	}
}

func TestDecodeSpecRejectsUnsafeJSON(t *testing.T) {
	raw, err := EncodeSpec(validSpec())
	if err != nil {
		t.Fatal(err)
	}
	valid := string(raw.Raw)
	for name, data := range map[string]string{
		"empty": "", "null": "null", "array": "[]", "trailing object": valid + "{}",
		"wrong kind":             strings.Replace(valid, SpecKind, StatusKind, 1),
		"wrong version":          strings.Replace(valid, "v1alpha1", "v1beta1", 1),
		"wrong case":             strings.Replace(valid, "projectID", "projectId", 1),
		"unknown field":          strings.Replace(valid, `"region":"eu01"`, `"region":"eu01","password":"secret"`, 1),
		"duplicate field":        strings.Replace(valid, `"region":"eu01"`, `"region":"eu01","region":"eu02"`, 1),
		"nested unknown":         strings.Replace(valid, `"sizeGiB":32`, `"sizeGiB":32,"volumeID":"foreign"`, 1),
		"cross namespace secret": strings.Replace(valid, `"name":"stackit-credentials"`, `"name":"stackit-credentials","namespace":"other"`, 1),
		"null ref":               strings.Replace(valid, `{"name":"stackit-credentials"}`, `null`, 1),
		"null nested":            strings.Replace(valid, `"sizeGiB":32`, `"sizeGiB":null`, 1),
		"null array item":        strings.Replace(valid, `"44444444-4444-4444-8444-444444444444"`, `null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSpec(&runtime.RawExtension{Raw: []byte(data)}); err == nil {
				t.Fatalf("accepted unsafe JSON: %s", data)
			}
		})
	}
	if _, err := DecodeSpec(nil); err == nil {
		t.Fatal("accepted nil spec")
	}
}

func TestSpecRoundTripDefaultingAndScheme(t *testing.T) {
	spec := validSpec()
	spec.RootVolume = RootVolume{}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSpec(&runtime.RawExtension{Raw: data})
	if err != nil {
		t.Fatal(err)
	}
	if decoded.RootVolume.SizeGiB != 32 {
		t.Fatalf("unexpected default: %d", decoded.RootVolume.SizeGiB)
	}
	if spec.RootVolume.SizeGiB != 0 {
		t.Fatal("defaulting mutated input")
	}
	raw, err := EncodeSpec(decoded)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecodeSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, again) {
		t.Fatalf("spec did not round trip: %#v", again)
	}
	fromObject, err := DecodeSpec(&runtime.RawExtension{Object: spec})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromObject, decoded) {
		t.Fatal("Object and Raw decoding differ")
	}
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{SpecKind, StatusKind} {
		if _, err := scheme.New(SchemeGroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
	}
	copy := spec.DeepCopy()
	copy.SecurityGroups[0] = "other"
	copy.CredentialsSecret.Name = "other"
	copy.UserDataSecret.Name = "other"
	if spec.SecurityGroups[0] == "other" || spec.CredentialsSecret.Name == "other" || spec.UserDataSecret.Name == "other" {
		t.Fatal("DeepCopy aliases input")
	}
}

func TestCloudConfigImmutabilityAndCredentialRotation(t *testing.T) {
	old := validSpec()
	old.SecurityGroups = append(old.SecurityGroups, "55555555-5555-4555-8555-555555555555")
	newSpec := old.DeepCopy()
	newSpec.CredentialsSecret.Name = "rotated"
	newSpec.UserDataSecret.Name = "rotated-user-data"
	newSpec.SecurityGroups[0], newSpec.SecurityGroups[1] = newSpec.SecurityGroups[1], newSpec.SecurityGroups[0]
	if errs := ValidateSpecUpdate(old, newSpec); len(errs) > 0 {
		t.Fatalf("secret rotation or group reordering rejected: %v", errs)
	}
	for name, mutate := range map[string]func(*STACKITMachineProviderSpec){
		"project":        func(s *STACKITMachineProviderSpec) { s.ProjectID = s.NetworkID },
		"region":         func(s *STACKITMachineProviderSpec) { s.Region = "eu02"; s.AvailabilityZone = "eu02-1" },
		"image":          func(s *STACKITMachineProviderSpec) { s.ImageID = s.NetworkID },
		"network":        func(s *STACKITMachineProviderSpec) { s.NetworkID = s.ImageID },
		"zone":           func(s *STACKITMachineProviderSpec) { s.AvailabilityZone = "eu01-2" },
		"type":           func(s *STACKITMachineProviderSpec) { s.MachineType = "c2i.8" },
		"root volume":    func(s *STACKITMachineProviderSpec) { s.RootVolume.SizeGiB = 64 },
		"disk class":     func(s *STACKITMachineProviderSpec) { s.RootVolume.PerformanceClass = "storage_premium_perf2" },
		"security group": func(s *STACKITMachineProviderSpec) { s.SecurityGroups = s.SecurityGroups[:1] },
		"ssh key":        func(s *STACKITMachineProviderSpec) { s.SSHKeyName = "new-key" },
	} {
		t.Run(name, func(t *testing.T) {
			next := old.DeepCopy()
			mutate(next)
			if errs := ValidateSpecUpdate(old, next); len(errs) == 0 {
				t.Fatal("accepted immutable cloud change")
			}
		})
	}
	before, _ := json.Marshal(old)
	if _, err := CloudConfigHash(old); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(old)
	if !bytes.Equal(before, after) {
		t.Fatal("hash mutated input")
	}
}

func TestStatusRoundTripAndStrictDecode(t *testing.T) {
	status := &STACKITMachineProviderStatus{InstanceID: "instance", ProjectID: "project", Region: "eu01", MachineUID: "uid", RootVolumeID: "volume", CreatePending: true, SpecHash: "hash", Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created", LastTransitionTime: metav1.Now()}}}
	raw, err := EncodeStatus(status)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeStatus(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.InstanceID != status.InstanceID || decoded.MachineUID != status.MachineUID || !decoded.CreatePending || len(decoded.Conditions) != 1 {
		t.Fatalf("lost recovery data: %#v", decoded)
	}
	copy := decoded.DeepCopy()
	copy.Conditions[0].Reason = "Changed"
	if decoded.Conditions[0].Reason == "Changed" {
		t.Fatal("status DeepCopy aliases conditions")
	}
	if empty, err := DecodeStatus(nil); err != nil || empty.Kind != StatusKind {
		t.Fatalf("empty status: %#v %v", empty, err)
	}
	for _, data := range []string{`{}`, `null`, `{"kind":"Wrong","apiVersion":"stackitproviderconfig.openshift.io/v1alpha1"}`, strings.Replace(string(raw.Raw), `"instanceID":"instance"`, `"instanceID":"instance","foreign":true`, 1)} {
		if _, err := DecodeStatus(&runtime.RawExtension{Raw: []byte(data)}); err == nil {
			t.Fatalf("accepted invalid status: %s", data)
		}
	}
}
