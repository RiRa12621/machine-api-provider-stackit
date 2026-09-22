package admission

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	providerapi "github.com/RiRa12621/machine-api-provider-stackit/api/v1alpha1"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const testNamespace = "openshift-machine-api"

func TestRegisteredAdmissionReviewRoutes(t *testing.T) {
	server := webhook.NewServer(webhook.Options{Port: 8440})
	Register(server, testNamespace)
	for _, route := range []struct {
		path               string
		machineSet, mutate bool
	}{
		{ValidateMachinePath, false, false}, {MutateMachinePath, false, true},
		{ValidateMachineSetPath, true, false}, {MutateMachineSetPath, true, true},
	} {
		t.Run(route.path, func(t *testing.T) {
			request := requestFor(t, testSpec(), route.machineSet)
			request.UID = "admission-request"
			payload, err := json.Marshal(admissionv1.AdmissionReview{
				TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
				Request:  &request.AdmissionRequest,
			})
			if err != nil {
				t.Fatal(err)
			}
			httpRequest := httptest.NewRequest(http.MethodPost, route.path, bytes.NewReader(payload))
			httpRequest.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.WebhookMux().ServeHTTP(response, httpRequest)
			if response.Code != http.StatusOK {
				t.Fatalf("unexpected HTTP response: %s", response.Body.String())
			}
			var review admissionv1.AdmissionReview
			if err := json.Unmarshal(response.Body.Bytes(), &review); err != nil {
				t.Fatal(err)
			}
			if review.Response == nil || !review.Response.Allowed || review.Response.UID != request.UID {
				t.Fatalf("unexpected admission review: %#v", review.Response)
			}
			if (len(review.Response.Patch) > 0) != route.mutate {
				t.Fatalf("incorrect mutation routing: %#v", review.Response)
			}
		})
	}
}

func testSpec() *providerapi.STACKITMachineProviderSpec {
	return &providerapi.STACKITMachineProviderSpec{
		TypeMeta:  metav1.TypeMeta{APIVersion: providerapi.SchemeGroupVersion.String(), Kind: providerapi.SpecKind},
		ProjectID: "11111111-1111-4111-8111-111111111111", Region: "eu01", ImageID: "22222222-2222-4222-8222-222222222222",
		NetworkID: "33333333-3333-4333-8333-333333333333", MachineType: "c2i.4", AvailabilityZone: "eu01-1",
		SecurityGroups:    []string{"44444444-4444-4444-8444-444444444444"},
		CredentialsSecret: &corev1.LocalObjectReference{Name: "credentials"}, UserDataSecret: &corev1.LocalObjectReference{Name: "user-data"},
	}
}

func objectJSON(t *testing.T, spec *providerapi.STACKITMachineProviderSpec, machineSet bool) []byte {
	t.Helper()
	// Marshal directly so the admission hook must add defaults itself.
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	machineSpec := machinev1.MachineSpec{ProviderSpec: machinev1.ProviderSpec{Value: &runtime.RawExtension{Raw: data}}}
	var object any = &machinev1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: testNamespace}, Spec: machineSpec}
	if machineSet {
		object = &machinev1.MachineSet{ObjectMeta: metav1.ObjectMeta{Name: "workers", Namespace: testNamespace}, Spec: machinev1.MachineSetSpec{Template: machinev1.MachineTemplateSpec{Spec: machineSpec}}}
	}
	result, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func requestFor(t *testing.T, spec *providerapi.STACKITMachineProviderSpec, machineSet bool) cradmission.Request {
	t.Helper()
	kind := "Machine"
	if machineSet {
		kind = "MachineSet"
	}
	return cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Namespace: testNamespace, Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Group: machinev1.GroupName, Version: "v1beta1", Kind: kind},
		Object: runtime.RawExtension{Raw: objectJSON(t, spec, machineSet)},
	}}
}

func TestDefaultingChangesOnlyProviderConfiguration(t *testing.T) {
	for _, isSet := range []bool{false, true} {
		t.Run(map[bool]string{false: "Machine", true: "MachineSet"}[isSet], func(t *testing.T) {
			handler := &Handler{Namespace: testNamespace, MachineSet: isSet, Mutate: true}
			request := requestFor(t, testSpec(), isSet)
			// Fields owned by other controllers must remain untouched by our patch.
			response := handler.Handle(t.Context(), request)
			if !response.Allowed || len(response.Patches) != 1 {
				t.Fatalf("unexpected response: %#v", response)
			}
			expectedPath := "/spec/providerSpec/value"
			if isSet {
				expectedPath = "/spec/template/spec/providerSpec/value"
			}
			patch := response.Patches[0]
			if patch.Path != expectedPath || patch.Operation != "replace" {
				t.Fatalf("unexpected patch: %#v", patch)
			}
			spec, ok := patch.Value.(*providerapi.STACKITMachineProviderSpec)
			if !ok || spec.RootVolume.SizeGiB != 32 {
				t.Fatalf("root volume was not defaulted: %#v", patch.Value)
			}
			if spec.CredentialsSecret.Name != "credentials" || spec.UserDataSecret.Name != "user-data" {
				t.Fatal("secret references changed")
			}
		})
	}
}

func TestInvalidMachineAndMachineSetAdmission(t *testing.T) {
	for _, isSet := range []bool{false, true} {
		for _, mutate := range []bool{false, true} {
			handler := &Handler{Namespace: testNamespace, MachineSet: isSet, Mutate: mutate}
			for name, change := range map[string]func(*providerapi.STACKITMachineProviderSpec){
				"region mismatch":     func(s *providerapi.STACKITMachineProviderSpec) { s.AvailabilityZone = "eu02-1" },
				"missing groups":      func(s *providerapi.STACKITMachineProviderSpec) { s.SecurityGroups = []string{} },
				"missing userdata":    func(s *providerapi.STACKITMachineProviderSpec) { s.UserDataSecret = nil },
				"missing credentials": func(s *providerapi.STACKITMachineProviderSpec) { s.CredentialsSecret = nil },
				"wrong kind":          func(s *providerapi.STACKITMachineProviderSpec) { s.Kind = "OpenStackMachineProviderSpec" },
			} {
				t.Run(name, func(t *testing.T) {
					spec := testSpec()
					change(spec)
					if response := handler.Handle(t.Context(), requestFor(t, spec, isSet)); response.Allowed {
						t.Fatalf("invalid spec accepted (set=%v,mutate=%v)", isSet, mutate)
					}
				})
			}
		}
	}
}

func TestMachineUpdatesPreserveCloudOwnership(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		handler := &Handler{Namespace: testNamespace, Mutate: mutate}
		original := testSpec()
		for name, change := range map[string]func(*providerapi.STACKITMachineProviderSpec){
			"project": func(s *providerapi.STACKITMachineProviderSpec) { s.ProjectID = s.NetworkID },
			"image":   func(s *providerapi.STACKITMachineProviderSpec) { s.ImageID = s.NetworkID },
			"network": func(s *providerapi.STACKITMachineProviderSpec) { s.NetworkID = s.ImageID },
			"shape":   func(s *providerapi.STACKITMachineProviderSpec) { s.MachineType = "c2i.8" },
		} {
			t.Run(name, func(t *testing.T) {
				next := original.DeepCopy()
				change(next)
				request := requestFor(t, next, false)
				request.Operation = admissionv1.Update
				request.OldObject.Raw = objectJSON(t, original, false)
				if response := handler.Handle(t.Context(), request); response.Allowed {
					t.Fatal("Machine cloud change accepted")
				}
			})
		}
		rotated := original.DeepCopy()
		rotated.CredentialsSecret.Name = "rotated"
		rotated.UserDataSecret.Name = "rotated-user-data"
		request := requestFor(t, rotated, false)
		request.Operation = admissionv1.Update
		request.OldObject.Raw = objectJSON(t, original, false)
		if response := handler.Handle(t.Context(), request); !response.Allowed {
			t.Fatalf("credential rotation rejected: %#v", response)
		}
	}
}

func TestMachineSetTemplateCanChangeForFutureMachines(t *testing.T) {
	original := testSpec()
	updated := original.DeepCopy()
	updated.MachineType = "c2i.8"
	updated.ImageID = original.NetworkID
	request := requestFor(t, updated, true)
	request.Operation = admissionv1.Update
	request.OldObject.Raw = objectJSON(t, original, true)
	if response := (&Handler{Namespace: testNamespace, MachineSet: true}).Handle(t.Context(), request); !response.Allowed {
		t.Fatalf("MachineSet update rejected: %#v", response)
	}
}

func TestAdmissionIsolationAndCleanup(t *testing.T) {
	handler := &Handler{Namespace: testNamespace}
	for name, change := range map[string]func(*cradmission.Request){
		"other namespace": func(r *cradmission.Request) { r.Namespace = "other" },
		"status":          func(r *cradmission.Request) { r.SubResource = "status" },
		"delete":          func(r *cradmission.Request) { r.Operation = admissionv1.Delete },
	} {
		t.Run(name, func(t *testing.T) {
			request := requestFor(t, testSpec(), false)
			request.Object.Raw = []byte("invalid")
			change(&request)
			if response := handler.Handle(t.Context(), request); !response.Allowed {
				t.Fatalf("unrelated operation blocked: %#v", response)
			}
		})
	}
	request := requestFor(t, testSpec(), false)
	request.Kind.Kind = "MachineSet"
	if response := handler.Handle(t.Context(), request); response.Allowed {
		t.Fatal("wrong resource accepted")
	}
	request = requestFor(t, testSpec(), false)
	request.Object.Raw = []byte("invalid")
	if response := handler.Handle(t.Context(), request); response.Allowed {
		t.Fatal("invalid JSON accepted")
	}
	request = requestFor(t, testSpec(), false)
	request.Object.Raw = []byte(strings.Replace(string(request.Object.Raw), `"region":"eu01"`, `"region":"eu01","unrecognized":true`, 1))
	if response := handler.Handle(t.Context(), request); response.Allowed {
		t.Fatal("unknown provider field accepted")
	}
}

func TestTerminatingMachineFinalizerOnlyRemoval(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			allowed bool
			change  func(*machinev1.Machine, *machinev1.Machine)
		}{
			{name: "all finalizers removed", allowed: true},
			{name: "managed fields updated", allowed: true, change: func(_, next *machinev1.Machine) {
				next.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "machine-controller", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: machinev1.SchemeGroupVersion.String()}}
			}},
			{name: "spec changed", change: func(_, next *machinev1.Machine) {
				next.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: []byte(`{"apiVersion":"legacy.example/v1","kind":"DifferentProvider"}`)}
			}},
			{name: "providerID changed", change: func(_, next *machinev1.Machine) {
				id := "stackit://different-instance"
				next.Spec.ProviderID = &id
			}},
			{name: "labels changed", change: func(_, next *machinev1.Machine) { next.Labels = map[string]string{"changed": "true"} }},
			{name: "annotations changed", change: func(_, next *machinev1.Machine) { next.Annotations = map[string]string{"changed": "true"} }},
			{name: "status changed", change: func(_, next *machinev1.Machine) {
				phase := "Failed"
				next.Status.Phase = &phase
			}},
			{name: "identity changed", change: func(_, next *machinev1.Machine) { next.UID = "other-machine" }},
			{name: "not terminating", change: func(old, next *machinev1.Machine) { old.DeletionTimestamp, next.DeletionTimestamp = nil, nil }},
			{name: "deletion just initiated", change: func(old, _ *machinev1.Machine) { old.DeletionTimestamp = nil }},
			{name: "finalizers unchanged", change: func(old, next *machinev1.Machine) { next.Finalizers = append([]string(nil), old.Finalizers...) }},
			{name: "finalizer added", change: func(old, next *machinev1.Machine) {
				next.Finalizers = append(append([]string(nil), old.Finalizers...), "other.example/finalizer")
			}},
			{name: "only some finalizers removed", change: func(old, next *machinev1.Machine) {
				old.Finalizers = append(old.Finalizers, "other.example/finalizer")
				next.Finalizers = []string{"other.example/finalizer"}
			}},
		} {
			t.Run(fmt.Sprintf("When %s, it should preserve strict cleanup scope (mutating=%t)", tc.name, mutate), func(t *testing.T) {
				old := terminatingLegacyMachine()
				next := old.DeepCopy()
				next.Finalizers = nil
				if tc.change != nil {
					tc.change(old, next)
				}
				request := requestFor(t, testSpec(), false)
				request.Operation = admissionv1.Update
				request.OldObject.Raw = marshalAdmissionObject(t, old)
				request.Object.Raw = marshalAdmissionObject(t, next)
				response := (&Handler{Namespace: testNamespace, Mutate: mutate}).Handle(t.Context(), request)
				if response.Allowed != tc.allowed {
					t.Fatalf("allowed=%t, want %t: %#v", response.Allowed, tc.allowed, response.Result)
				}
				if len(response.Patches) != 0 || len(response.Patch) != 0 {
					t.Fatal("finalizer cleanup rewrote provider configuration")
				}
			})
		}
	}
}

func TestFinalizerRemovalWithMissingProviderSpec(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		old := terminatingLegacyMachine()
		old.Spec.ProviderSpec.Value = nil
		next := old.DeepCopy()
		next.Finalizers = []string{}
		request := requestFor(t, testSpec(), false)
		request.Operation = admissionv1.Update
		request.OldObject.Raw = marshalAdmissionObject(t, old)
		request.Object.Raw = marshalAdmissionObject(t, next)
		response := (&Handler{Namespace: testNamespace, Mutate: mutate}).Handle(t.Context(), request)
		if !response.Allowed || len(response.Patches) != 0 {
			t.Fatalf("never-provisioned Machine cleanup blocked or mutated: %#v", response)
		}
	}
}

func TestFinalizerRemovalExemptionRequiresMachineUpdate(t *testing.T) {
	old := terminatingLegacyMachine()
	next := old.DeepCopy()
	next.Finalizers = nil
	for _, mutate := range []bool{false, true} {
		request := requestFor(t, testSpec(), false)
		request.Object.Raw = marshalAdmissionObject(t, next)
		request.OldObject.Raw = marshalAdmissionObject(t, old)
		handler := &Handler{Namespace: testNamespace, Mutate: mutate}
		if response := handler.Handle(t.Context(), request); response.Allowed {
			t.Fatal("CREATE request used the finalizer exemption")
		}
		request.Operation = admissionv1.Update
		request.OldObject.Raw = []byte("invalid")
		if response := handler.Handle(t.Context(), request); response.Allowed {
			t.Fatal("malformed old object used the finalizer exemption")
		}
		request.OldObject.Raw = marshalAdmissionObject(t, old)
		request.Object.Raw = []byte("invalid")
		if response := handler.Handle(t.Context(), request); response.Allowed {
			t.Fatal("malformed new object used the finalizer exemption")
		}
		oldSet := &machinev1.MachineSet{ObjectMeta: old.ObjectMeta}
		oldSet.Spec.Template.Spec = old.Spec
		newSet := oldSet.DeepCopy()
		newSet.Finalizers = nil
		request.Kind.Kind = "MachineSet"
		request.OldObject.Raw = marshalAdmissionObject(t, oldSet)
		request.Object.Raw = marshalAdmissionObject(t, newSet)
		handler.MachineSet = true
		if response := handler.Handle(t.Context(), request); response.Allowed {
			t.Fatal("MachineSet request used a Machine-only exemption")
		}
	}
}

func terminatingLegacyMachine() *machinev1.Machine {
	timestamp := metav1.NewTime(time.Unix(1700000000, 0))
	return &machinev1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: testNamespace, UID: "machine-uid", ResourceVersion: "10", DeletionTimestamp: &timestamp, Finalizers: []string{"machine.openshift.io/machine"}},
		Spec:       machinev1.MachineSpec{ProviderSpec: machinev1.ProviderSpec{Value: &runtime.RawExtension{Raw: []byte(`{"apiVersion":"legacy.example/v1","kind":"OldProvider"}`)}}},
	}
}

func marshalAdmissionObject(t *testing.T, object any) []byte {
	t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
