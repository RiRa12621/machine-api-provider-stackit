package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	strictjson "sigs.k8s.io/json"
)

// DecodeSpec strictly decodes and defaults the embedded configuration. Call
// ValidateSpec before using it to access the cloud. Explicit nulls, duplicate and
// unknown fields, and unsupported kinds or versions are rejected.
func DecodeSpec(raw *runtime.RawExtension) (*STACKITMachineProviderSpec, error) {
	var spec STACKITMachineProviderSpec
	if err := decode(raw, &spec); err != nil {
		return nil, err
	}
	if err := validateTypeMeta(spec.TypeMeta, SpecKind); err != nil {
		return nil, err
	}
	DefaultSpec(&spec)
	return &spec, nil
}

// DecodeStatus returns an empty status for a Machine without provider status.
func DecodeStatus(raw *runtime.RawExtension) (*STACKITMachineProviderStatus, error) {
	if raw == nil {
		return &STACKITMachineProviderStatus{TypeMeta: typeMeta(StatusKind)}, nil
	}
	status := &STACKITMachineProviderStatus{}
	if err := decode(raw, status); err != nil {
		return nil, err
	}
	if err := validateTypeMeta(status.TypeMeta, StatusKind); err != nil {
		return nil, err
	}
	return status, nil
}

// EncodeSpec sets the canonical kind and version without mutating its input.
func EncodeSpec(spec *STACKITMachineProviderSpec) (*runtime.RawExtension, error) {
	if spec == nil {
		return nil, errors.New("provider spec is required")
	}
	copy := spec.DeepCopy()
	copy.TypeMeta = typeMeta(SpecKind)
	DefaultSpec(copy)
	return encode(copy)
}

// EncodeStatus sets the canonical kind and version without mutating its input.
func EncodeStatus(status *STACKITMachineProviderStatus) (*runtime.RawExtension, error) {
	if status == nil {
		return nil, errors.New("provider status is required")
	}
	copy := status.DeepCopy()
	copy.TypeMeta = typeMeta(StatusKind)
	return encode(copy)
}

func encode(value any) (*runtime.RawExtension, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &runtime.RawExtension{Raw: data}, nil
}

func decode(raw *runtime.RawExtension, value any) error {
	if raw == nil {
		return errors.New("provider configuration is required")
	}
	data := raw.Raw
	if len(data) == 0 && raw.Object != nil {
		var err error
		data, err = json.Marshal(raw.Object)
		if err != nil {
			return fmt.Errorf("encode provider configuration: %w", err)
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("provider configuration is empty")
	}
	var tree any
	if err := json.Unmarshal(data, &tree); err != nil {
		return fmt.Errorf("invalid provider JSON: %w", err)
	}
	if err := rejectNull(tree, "provider configuration"); err != nil {
		return err
	}
	strictErrors, err := strictjson.UnmarshalStrict(data, value)
	if err != nil {
		return fmt.Errorf("invalid provider configuration: %w", err)
	}
	if len(strictErrors) > 0 {
		return fmt.Errorf("invalid provider configuration: %w", errors.Join(strictErrors...))
	}
	return nil
}

func rejectNull(value any, path string) error {
	switch typed := value.(type) {
	case nil:
		return fmt.Errorf("%s must not be null", path)
	case map[string]any:
		for name, child := range typed {
			if err := rejectNull(child, path+"."+name); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := rejectNull(child, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func typeMeta(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: SchemeGroupVersion.String(), Kind: kind}
}

func validateTypeMeta(meta metav1.TypeMeta, kind string) error {
	if meta.APIVersion != SchemeGroupVersion.String() || meta.Kind != kind {
		return fmt.Errorf("provider configuration must use apiVersion %q and kind %q", SchemeGroupVersion.String(), kind)
	}
	return nil
}

// CloudConfigHash identifies immutable cloud configuration. Secret references can
// rotate without changing cloud identity. Security group ordering is immaterial.
func CloudConfigHash(spec *STACKITMachineProviderSpec) (string, error) {
	if spec == nil {
		return "", errors.New("provider spec is required")
	}
	copy := spec.DeepCopy()
	copy.TypeMeta = metav1.TypeMeta{}
	copy.CredentialsSecret = nil
	copy.UserDataSecret = nil
	DefaultSpec(copy)
	sort.Strings(copy.SecurityGroups)
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
