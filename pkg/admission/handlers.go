// Package admission serves STACKIT provider configuration hooks alongside MAO's
// generic Machine and MachineSet lifecycle validation.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	providerapi "github.com/RiRa12621/machine-api-provider-stackit/api/v1alpha1"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	ValidateMachinePath    = "/validate-stackit-machine"
	MutateMachinePath      = "/mutate-stackit-machine"
	ValidateMachineSetPath = "/validate-stackit-machineset"
	MutateMachineSetPath   = "/mutate-stackit-machineset"
)

// Register installs only provider-specific hooks. MAO must retain its generic
// admission handlers, which enforce Machine lifecycle and MachineSet invariants.
func Register(server webhook.Server, namespace string) {
	for _, item := range []struct {
		path               string
		machineSet, mutate bool
	}{
		{ValidateMachinePath, false, false}, {MutateMachinePath, false, true},
		{ValidateMachineSetPath, true, false}, {MutateMachineSetPath, true, true},
	} {
		server.Register(item.path, &webhook.Admission{Handler: &Handler{Namespace: namespace, MachineSet: item.machineSet, Mutate: item.mutate}})
	}
}

// Handler validates or defaults one kind of embedded provider configuration.
// It never reads Secrets or invokes the cloud API.
type Handler struct {
	Namespace  string
	MachineSet bool
	Mutate     bool
}

func (h *Handler) Handle(_ context.Context, request cradmission.Request) cradmission.Response {
	if request.Namespace != h.Namespace || request.SubResource != "" || request.Operation == admissionv1.Delete {
		return cradmission.Allowed("outside STACKIT provider configuration scope")
	}
	if request.Operation != admissionv1.Create && request.Operation != admissionv1.Update {
		return cradmission.Denied("unsupported admission operation")
	}
	kind, patchPath := "Machine", "/spec/providerSpec/value"
	if h.MachineSet {
		kind, patchPath = "MachineSet", "/spec/template/spec/providerSpec/value"
	}
	if request.Kind.Group != machinev1.GroupName || request.Kind.Version != machinev1.SchemeGroupVersion.Version || request.Kind.Kind != kind {
		return cradmission.Denied("unsupported resource kind or version")
	}
	if !h.MachineSet && request.Operation == admissionv1.Update {
		finalizerOnly, err := isFinalizerOnlyRemoval(request.Object.Raw, request.OldObject.Raw)
		if err != nil {
			return cradmission.Errored(http.StatusBadRequest, err)
		}
		if finalizerOnly {
			// Match MAO's cleanup exemption in both hooks. Defaulting must not
			// rewrite a legacy provider spec during finalizer removal either.
			return cradmission.Allowed("terminating Machine finalizer removal")
		}
	}
	raw, err := providerSpec(request.Object.Raw, h.MachineSet)
	if err != nil {
		return cradmission.Errored(http.StatusBadRequest, err)
	}
	spec, err := providerapi.DecodeSpec(raw)
	if err != nil {
		return cradmission.Denied(err.Error())
	}
	if errs := providerapi.ValidateSpec(spec); len(errs) > 0 {
		return cradmission.Denied(errs.ToAggregate().Error())
	}
	if !h.MachineSet && request.Operation == admissionv1.Update {
		oldRaw, err := providerSpec(request.OldObject.Raw, false)
		if err != nil {
			return cradmission.Errored(http.StatusBadRequest, err)
		}
		oldSpec, err := providerapi.DecodeSpec(oldRaw)
		if err != nil {
			return cradmission.Denied(fmt.Sprintf("cannot validate previous provider configuration: %v", err))
		}
		if errs := providerapi.ValidateSpecUpdate(oldSpec, spec); len(errs) > 0 {
			return cradmission.Denied(errs.ToAggregate().Error())
		}
	}
	if h.Mutate {
		return cradmission.Patched("defaulted STACKIT provider configuration", jsonpatch.JsonPatchOperation{Operation: "replace", Path: patchPath, Value: spec})
	}
	return cradmission.Allowed("STACKIT provider configuration is valid")
}

// isFinalizerOnlyRemoval mirrors MAO's Machine validation exemption: the only
// semantic change must be removing the complete finalizer list. Managed fields
// may change as a consequence of the API update. Spec, status, identity and all
// other metadata must remain unchanged, including the deletion timestamp.
func isFinalizerOnlyRemoval(current, previous []byte) (bool, error) {
	var machine, oldMachine machinev1.Machine
	if err := json.Unmarshal(current, &machine); err != nil {
		return false, fmt.Errorf("decode Machine: %w", err)
	}
	if machine.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if err := json.Unmarshal(previous, &oldMachine); err != nil {
		return false, fmt.Errorf("decode previous Machine: %w", err)
	}
	machine.ManagedFields = oldMachine.ManagedFields
	patch, err := client.MergeFrom(&oldMachine).Data(&machine)
	if err != nil {
		return false, fmt.Errorf("calculate Machine finalizer patch: %w", err)
	}
	return string(patch) == `{"metadata":{"finalizers":null}}`, nil
}

func providerSpec(data []byte, machineSet bool) (*runtime.RawExtension, error) {
	if machineSet {
		var object machinev1.MachineSet
		if err := json.Unmarshal(data, &object); err != nil {
			return nil, fmt.Errorf("decode MachineSet: %w", err)
		}
		return object.Spec.Template.Spec.ProviderSpec.Value, nil
	}
	var object machinev1.Machine
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("decode Machine: %w", err)
	}
	return object.Spec.ProviderSpec.Value, nil
}

var _ cradmission.Handler = (*Handler)(nil)
