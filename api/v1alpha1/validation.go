package v1alpha1

import (
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	regionPattern = regexp.MustCompile(`^[a-z]{2}[0-9]{2}$`)
	zonePattern   = regexp.MustCompile(`^[a-z]{2}[0-9]{2}-[1-9][0-9]*$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// DefaultSpec supplies only documented local defaults; it never queries cloud APIs.
func DefaultSpec(spec *STACKITMachineProviderSpec) {
	if spec.RootVolume.SizeGiB == 0 {
		spec.RootVolume.SizeGiB = DefaultRootVolumeSizeGiB
	}
}

// ValidateSpec checks the fully defaulted configuration independently of admission
// so the actuator remains safe when webhook installation is incomplete.
func ValidateSpec(spec *STACKITMachineProviderSpec) field.ErrorList {
	path := field.NewPath("spec", "providerSpec", "value")
	if spec == nil {
		return field.ErrorList{field.Required(path, "STACKIT provider configuration is required")}
	}
	var errs field.ErrorList
	if err := validateTypeMeta(spec.TypeMeta, SpecKind); err != nil {
		errs = append(errs, field.Invalid(path, nil, err.Error()))
	}
	for _, item := range []struct{ name, value string }{
		{"projectID", spec.ProjectID}, {"networkID", spec.NetworkID}, {"imageID", spec.ImageID},
	} {
		if !uuidPattern.MatchString(item.value) {
			errs = append(errs, field.Invalid(path.Child(item.name), item.value, "must be a canonical lowercase UUID"))
		}
	}
	if !regionPattern.MatchString(spec.Region) {
		errs = append(errs, field.Invalid(path.Child("region"), spec.Region, "must use the STACKIT region format, for example eu01"))
	}
	if !zonePattern.MatchString(spec.AvailabilityZone) || !strings.HasPrefix(spec.AvailabilityZone, spec.Region+"-") {
		errs = append(errs, field.Invalid(path.Child("availabilityZone"), spec.AvailabilityZone, "must be an availability zone in the configured region, for example eu01-1"))
	}
	if len(spec.MachineType) > 63 || !namePattern.MatchString(spec.MachineType) {
		errs = append(errs, field.Invalid(path.Child("machineType"), spec.MachineType, "must be a nonempty STACKIT machine type of at most 63 letters, digits, dots, underscores or hyphens"))
	}
	if spec.RootVolume.SizeGiB < 16 {
		errs = append(errs, field.Invalid(path.Child("rootVolume", "sizeGiB"), spec.RootVolume.SizeGiB, "must be at least 16 GiB"))
	}
	for _, item := range []struct {
		path  *field.Path
		value string
	}{
		{path.Child("rootVolume", "performanceClass"), spec.RootVolume.PerformanceClass},
		{path.Child("sshKeyName"), spec.SSHKeyName},
	} {
		if len(item.value) > 255 || (item.value != "" && !namePattern.MatchString(item.value)) {
			errs = append(errs, field.Invalid(item.path, item.value, "must be at most 255 letters, digits, dots, underscores or hyphens"))
		}
	}
	if len(spec.SecurityGroups) < 1 || len(spec.SecurityGroups) > 64 {
		errs = append(errs, field.Invalid(path.Child("securityGroups"), len(spec.SecurityGroups), "must contain between 1 and 64 existing security group UUIDs"))
	}
	seen := make(map[string]bool, len(spec.SecurityGroups))
	for index, group := range spec.SecurityGroups {
		groupPath := path.Child("securityGroups").Index(index)
		if !uuidPattern.MatchString(group) {
			errs = append(errs, field.Invalid(groupPath, group, "must be a canonical lowercase UUID"))
		}
		if seen[group] {
			errs = append(errs, field.Duplicate(groupPath, group))
		}
		seen[group] = true
	}
	for _, item := range []struct {
		name string
		ref  *corev1.LocalObjectReference
	}{
		{"credentialsSecret", spec.CredentialsSecret}, {"userDataSecret", spec.UserDataSecret},
	} {
		if item.ref == nil {
			errs = append(errs, field.Required(path.Child(item.name), "a same-namespace Secret reference is required"))
			continue
		}
		if problems := validation.IsDNS1123Subdomain(item.ref.Name); len(problems) > 0 {
			errs = append(errs, field.Invalid(path.Child(item.name, "name"), item.ref.Name, strings.Join(problems, "; ")))
		}
	}
	return errs
}

// ValidateSpecUpdate forbids changing a Machine's cloud identity and provisioning
// configuration. MachineSet templates can change for future Machines. Secret
// references can rotate, but changing user data does not modify an existing server.
func ValidateSpecUpdate(oldSpec, newSpec *STACKITMachineProviderSpec) field.ErrorList {
	errs := ValidateSpec(newSpec)
	oldHash, oldErr := CloudConfigHash(oldSpec)
	newHash, newErr := CloudConfigHash(newSpec)
	if oldErr != nil || newErr != nil || oldHash != newHash {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "providerSpec", "value"), "cloud configuration is immutable on a Machine; change the MachineSet template and replace the Machine"))
	}
	return errs
}
