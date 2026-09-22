package manager

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	providerapi "github.com/RiRa12621/machine-api-provider-stackit/api/v1alpha1"
	provideradmission "github.com/RiRa12621/machine-api-provider-stackit/pkg/admission"
	configv1 "github.com/openshift/api/config/v1"
	apifeatures "github.com/openshift/api/features"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	"github.com/openshift/library-go/pkg/features"
	machinecontroller "github.com/openshift/machine-api-operator/pkg/controller/machine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	crmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

type ActuatorFactory func(crmanager.Manager) (machinecontroller.Actuator, error)

// Run starts the standard MAO lifecycle and drain controllers with the STACKIT
// actuator. It deliberately does not start MachineSet replica, nodelink or health
// controllers: MAO deploys those separately.
func Run(ctx context.Context, options Options, factory ActuatorFactory) error {
	if err := options.Validate(); err != nil {
		return err
	}
	if factory == nil {
		return fmt.Errorf("an actuator factory is required")
	}
	gate, err := NewFeatureGate(options.FeatureGates)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, configv1.Install, machinev1.Install, providerapi.AddToScheme} {
		if err := install(scheme); err != nil {
			return fmt.Errorf("register API scheme: %w", err)
		}
	}
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	managerOptions, err := ManagerOptions(options, scheme)
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(restConfig, managerOptions)
	if err != nil {
		return fmt.Errorf("create controller manager: %w", err)
	}
	infra := &configv1.Infrastructure{}
	if err := mgr.GetAPIReader().Get(ctx, client.ObjectKey{Name: "cluster"}, infra); err != nil {
		return fmt.Errorf("read cluster infrastructure before starting: %w", err)
	}
	if err := ValidateInfrastructure(infra); err != nil {
		return err
	}
	actuator, err := factory(mgr)
	if err != nil {
		return fmt.Errorf("create STACKIT actuator: %w", err)
	}
	if actuator == nil {
		return fmt.Errorf("actuator factory returned no actuator")
	}
	if err := machinecontroller.AddWithActuatorOpts(mgr, actuator, controller.Options{MaxConcurrentReconciles: options.MaxConcurrentReconciles}, gate); err != nil {
		return fmt.Errorf("register MAO lifecycle controllers: %w", err)
	}
	provideradmission.Register(mgr.GetWebhookServer(), options.Namespace)
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("admission", mgr.GetWebhookServer().StartedChecker()); err != nil {
		return err
	}
	return mgr.Start(ctx)
}

// ValidateInfrastructure prevents this self-managed provider from taking
// ownership of HyperShift workers or machines on unrelated platforms.
func ValidateInfrastructure(infra *configv1.Infrastructure) error {
	if infra == nil || infra.Status.PlatformStatus == nil {
		return fmt.Errorf("STACKIT requires Infrastructure/cluster platform status")
	}
	platform := infra.Status.PlatformStatus
	spec := infra.Spec.PlatformSpec
	if platform.Type != configv1.ExternalPlatformType || spec.Type != configv1.ExternalPlatformType || spec.External == nil || spec.External.PlatformName != "STACKIT" {
		return fmt.Errorf("STACKIT requires the External platform with platformName STACKIT")
	}
	switch infra.Status.ControlPlaneTopology {
	case configv1.HighlyAvailableTopologyMode, configv1.SingleReplicaTopologyMode:
		return nil
	default:
		return fmt.Errorf("STACKIT Machine API requires a self-managed HighlyAvailable or SingleReplica control plane; got %q", infra.Status.ControlPlaneTopology)
	}
}

// NewFeatureGate uses the same OpenShift feature-gate defaults as upstream MAO
// providers. MAO also sends gates used by sibling containers; those are ignored.
func NewFeatureGate(value string) (featuregate.MutableFeatureGate, error) {
	gate := featuregate.NewFeatureGate()
	if err := features.InitializeFeatureGates(gate, 4, apifeatures.SelfManaged, apifeatures.FeatureGateMachineAPIMigration); err != nil {
		return nil, err
	}
	flags := map[string][]string{}
	if value != "" {
		flags["feature-gates"] = []string{value}
	}
	warnings, err := features.SetFeatureGates(flags, gate)
	if err != nil {
		return nil, fmt.Errorf("configure feature gates: %w", err)
	}
	for _, warning := range warnings {
		klog.Info(warning)
	}
	return gate, nil
}

// ManagerOptions keeps namespaced informers and the election lease in the
// configured Machine namespace. Secrets are read directly, never cached.
func ManagerOptions(options Options, scheme *runtime.Scheme) (ctrl.Options, error) {
	if err := options.Validate(); err != nil {
		return ctrl.Options{}, err
	}
	tlsConfig, err := options.TLSConfig()
	if err != nil {
		return ctrl.Options{}, err
	}
	syncPeriod := 10 * time.Minute
	leaseDuration := options.LeaderElectionLeaseDuration
	renewDeadline := leaseDuration * 11 / 12
	retryPeriod := leaseDuration / 6
	return ctrl.Options{
		Scheme:         scheme,
		Cache:          cache.Options{SyncPeriod: &syncPeriod, DefaultNamespaces: map[string]cache.Config{options.Namespace: {}}},
		Client:         client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		LeaderElection: options.LeaderElect, LeaderElectionNamespace: options.Namespace,
		LeaderElectionID: "machine-api-provider-stackit-leader", LeaderElectionReleaseOnCancel: true,
		LeaseDuration: &leaseDuration, RenewDeadline: &renewDeadline, RetryPeriod: &retryPeriod,
		Metrics: server.Options{BindAddress: options.MetricsAddress}, HealthProbeBindAddress: options.HealthAddress,
		WebhookServer: webhook.NewServer(webhook.Options{Port: options.WebhookPort, CertDir: options.WebhookCertDir, TLSOpts: []func(*tls.Config){func(config *tls.Config) {
			config.MinVersion, config.CipherSuites, config.NextProtos = tlsConfig.MinVersion, tlsConfig.CipherSuites, tlsConfig.NextProtos
		}}}),
	}, nil
}
