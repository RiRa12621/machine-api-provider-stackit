package manager

import (
	"crypto/tls"
	"flag"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	apifeatures "github.com/openshift/api/features"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/component-base/featuregate"
)

func TestMAOFlagContract(t *testing.T) {
	options := DefaultOptions()
	flags := flag.NewFlagSet("manager", flag.ContinueOnError)
	options.BindFlags(flags)
	err := flags.Parse([]string{"--logtostderr=true", "--v=3", "--leader-elect=true", "--leader-elect-lease-duration=120s", "--namespace=openshift-machine-api", "--max-concurrent-reconciles=10", "--feature-gates=MachineAPIMigration=false,AWSDedicatedHosts=false,VSphereHostVMGroupZonal=false", "--tls-min-version=VersionTLS12", "--tls-cipher-suites=TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"})
	if err != nil {
		t.Fatal(err)
	}
	if err := options.Validate(); err != nil {
		t.Fatal(err)
	}
	if options.WebhookPort != 8440 || options.WebhookCertDir != "/etc/machine-api-operator/tls" || options.HealthAddress != ":9440" || options.MetricsAddress != ":8081" || options.MaxConcurrentReconciles != 10 {
		t.Fatalf("MAO defaults changed: %#v", options)
	}
	gate, err := NewFeatureGate(options.FeatureGates)
	if err != nil {
		t.Fatal(err)
	}
	if gate.Enabled(featuregate.Feature(apifeatures.FeatureGateMachineAPIMigration)) {
		t.Fatal("migration feature was not disabled")
	}
}

func TestManagerNamespaceAndSecretIsolation(t *testing.T) {
	options := DefaultOptions()
	options.Namespace = "stackit-machines"
	options.LeaderElectionNamespace = options.Namespace
	actual, err := ManagerOptions(options, runtime.NewScheme())
	if err != nil {
		t.Fatal(err)
	}
	if len(actual.Cache.DefaultNamespaces) != 1 {
		t.Fatalf("unexpected namespace cache: %#v", actual.Cache.DefaultNamespaces)
	}
	if _, ok := actual.Cache.DefaultNamespaces[options.Namespace]; !ok {
		t.Fatal("missing namespace cache restriction")
	}
	if actual.LeaderElectionNamespace != options.Namespace || actual.LeaderElectionID != "machine-api-provider-stackit-leader" {
		t.Fatal("leader lease escaped namespace")
	}
	if actual.Client.Cache == nil || len(actual.Client.Cache.DisableFor) != 1 {
		t.Fatal("Secrets may be cached")
	}
	if _, ok := actual.Client.Cache.DisableFor[0].(*corev1.Secret); !ok {
		t.Fatal("Secrets do not bypass cache")
	}
	if *actual.LeaseDuration != 120*time.Second || *actual.RenewDeadline != 110*time.Second || *actual.RetryPeriod != 20*time.Second {
		t.Fatal("leader election durations changed")
	}
}

func TestInvalidManagerOptions(t *testing.T) {
	for name, change := range map[string]func(*Options){
		"all namespaces":    func(o *Options) { o.Namespace = "" },
		"invalid namespace": func(o *Options) { o.Namespace = "UPPER" },
		"foreign lease":     func(o *Options) { o.LeaderElectionNamespace = "other" },
		"short lease":       func(o *Options) { o.LeaderElectionLeaseDuration = time.Second },
		"no webhook":        func(o *Options) { o.WebhookPort = 0 },
		"invalid port":      func(o *Options) { o.WebhookPort = 65536 },
		"no cert directory": func(o *Options) { o.WebhookCertDir = "" },
		"no reconciles":     func(o *Options) { o.MaxConcurrentReconciles = 0 },
		"old TLS":           func(o *Options) { o.TLSMinVersion = "VersionTLS11" },
		"invalid TLS":       func(o *Options) { o.TLSMinVersion = "invalid" },
		"invalid cipher":    func(o *Options) { o.TLSCipherSuites = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			options := DefaultOptions()
			change(&options)
			if err := options.Validate(); err == nil {
				t.Fatal("unsafe manager options accepted")
			}
		})
	}
}

func TestAdmissionTLS(t *testing.T) {
	options := DefaultOptions()
	options.TLSMinVersion = "VersionTLS13"
	config, err := options.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.MinVersion != tls.VersionTLS13 || len(config.NextProtos) != 1 || config.NextProtos[0] != "http/1.1" {
		t.Fatalf("unexpected TLS settings: %#v", config)
	}
}

func TestInfrastructureGuard(t *testing.T) {
	valid := func() *configv1.Infrastructure {
		return &configv1.Infrastructure{
			Spec: configv1.InfrastructureSpec{PlatformSpec: configv1.PlatformSpec{Type: configv1.ExternalPlatformType, External: &configv1.ExternalPlatformSpec{PlatformName: "STACKIT"}}},
			Status: configv1.InfrastructureStatus{
				PlatformStatus:       &configv1.PlatformStatus{Type: configv1.ExternalPlatformType},
				ControlPlaneTopology: configv1.HighlyAvailableTopologyMode,
			},
		}
	}
	if err := ValidateInfrastructure(valid()); err != nil {
		t.Fatal(err)
	}
	single := valid()
	single.Status.ControlPlaneTopology = configv1.SingleReplicaTopologyMode
	if err := ValidateInfrastructure(single); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*configv1.Infrastructure){
		"hosted control plane":      func(i *configv1.Infrastructure) { i.Status.ControlPlaneTopology = configv1.ExternalTopologyMode },
		"unknown topology":          func(i *configv1.Infrastructure) { i.Status.ControlPlaneTopology = "" },
		"foreign external platform": func(i *configv1.Infrastructure) { i.Spec.PlatformSpec.External.PlatformName = "Other" },
		"wrong platform":            func(i *configv1.Infrastructure) { i.Status.PlatformStatus.Type = configv1.AWSPlatformType },
		"wrong desired platform":    func(i *configv1.Infrastructure) { i.Spec.PlatformSpec.Type = configv1.AWSPlatformType },
		"missing external spec":     func(i *configv1.Infrastructure) { i.Spec.PlatformSpec.External = nil },
		"missing platform status":   func(i *configv1.Infrastructure) { i.Status.PlatformStatus = nil },
	} {
		t.Run(name, func(t *testing.T) {
			infra := valid()
			change(infra)
			if err := ValidateInfrastructure(infra); err == nil {
				t.Fatal("unsupported infrastructure accepted")
			}
		})
	}
	if err := ValidateInfrastructure(nil); err == nil {
		t.Fatal("nil infrastructure accepted")
	}
}

func TestFeatureGateCompatibility(t *testing.T) {
	gate, err := NewFeatureGate("MachineAPIMigration=true,OtherContainerFeature=true")
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Enabled(featuregate.Feature(apifeatures.FeatureGateMachineAPIMigration)) {
		t.Fatal("migration gate not enabled")
	}
	if _, err := NewFeatureGate("MachineAPIMigration=invalid"); err == nil {
		t.Fatal("invalid feature flag accepted")
	}
}
