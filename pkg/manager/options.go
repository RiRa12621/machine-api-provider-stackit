// Package manager registers the STACKIT actuator with MAO's lifecycle controller.
package manager

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/machine-api-operator/pkg/metrics"
	"k8s.io/apimachinery/pkg/util/validation"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
)

type Options struct {
	Namespace                   string
	LeaderElect                 bool
	LeaderElectionNamespace     string
	LeaderElectionLeaseDuration time.Duration
	MetricsAddress              string
	HealthAddress               string
	WebhookPort                 int
	WebhookCertDir              string
	TLSMinVersion               string
	TLSCipherSuites             string
	MaxConcurrentReconciles     int
	FeatureGates                string
}

func DefaultOptions() Options {
	return Options{
		Namespace: "openshift-machine-api", LeaderElect: true, LeaderElectionLeaseDuration: 120 * time.Second,
		MetricsAddress: metrics.DefaultMachineMetricsAddress, HealthAddress: ":9440",
		WebhookPort: 8440, WebhookCertDir: "/etc/machine-api-operator/tls", TLSMinVersion: "VersionTLS12",
		MaxConcurrentReconciles: 1,
	}
}

// BindFlags accepts the flags emitted by MAO's machine-controller Deployment.
func (o *Options) BindFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.Namespace, "namespace", o.Namespace, "Namespace containing STACKIT Machines and their credential and user-data Secrets")
	flags.BoolVar(&o.LeaderElect, "leader-elect", o.LeaderElect, "Enable leader election")
	flags.StringVar(&o.LeaderElectionNamespace, "leader-elect-resource-namespace", o.LeaderElectionNamespace, "Namespace for the leader election lease (defaults to --namespace)")
	flags.DurationVar(&o.LeaderElectionLeaseDuration, "leader-elect-lease-duration", o.LeaderElectionLeaseDuration, "Leader election lease duration")
	flags.StringVar(&o.MetricsAddress, "metrics-bind-address", o.MetricsAddress, "Metrics listen address")
	flags.StringVar(&o.HealthAddress, "health-addr", o.HealthAddress, "Health and readiness listen address")
	flags.IntVar(&o.WebhookPort, "webhook-port", o.WebhookPort, "Provider admission HTTPS port")
	flags.StringVar(&o.WebhookCertDir, "webhook-cert-dir", o.WebhookCertDir, "Directory containing tls.crt and tls.key for admission")
	flags.StringVar(&o.TLSMinVersion, "tls-min-version", o.TLSMinVersion, "Minimum admission TLS version, VersionTLS12 or VersionTLS13")
	flags.StringVar(&o.TLSCipherSuites, "tls-cipher-suites", o.TLSCipherSuites, "Comma-separated TLS cipher suites, as configured by MAO")
	flags.IntVar(&o.MaxConcurrentReconciles, "max-concurrent-reconciles", o.MaxConcurrentReconciles, "Maximum concurrent Machine reconciliations")
	flags.StringVar(&o.FeatureGates, "feature-gates", o.FeatureGates, "Feature gates passed by MAO; only MachineAPIMigration is consumed")
	klog.InitFlags(flags)
}

func (o Options) Validate() error {
	if problems := validation.IsDNS1123Label(o.Namespace); len(problems) > 0 {
		return fmt.Errorf("invalid --namespace: %s", strings.Join(problems, "; "))
	}
	if o.LeaderElectionNamespace != "" && o.LeaderElectionNamespace != o.Namespace {
		return errors.New("--leader-elect-resource-namespace must match --namespace")
	}
	if o.LeaderElectionLeaseDuration < 3*time.Second {
		return errors.New("--leader-elect-lease-duration must be at least 3s")
	}
	if o.WebhookPort < 1 || o.WebhookPort > 65535 {
		return errors.New("--webhook-port must be between 1 and 65535")
	}
	if o.WebhookCertDir == "" {
		return errors.New("--webhook-cert-dir is required")
	}
	if o.MaxConcurrentReconciles < 1 {
		return errors.New("--max-concurrent-reconciles must be positive")
	}
	_, err := o.TLSConfig()
	return err
}

// TLSConfig honors the cluster TLS profile and disables HTTP/2 on admission.
func (o Options) TLSConfig() (*tls.Config, error) {
	minimum, err := cliflag.TLSVersion(o.TLSMinVersion)
	if err != nil {
		return nil, err
	}
	if minimum < tls.VersionTLS12 {
		return nil, errors.New("admission requires TLS 1.2 or newer")
	}
	var names []string
	if o.TLSCipherSuites != "" {
		names = strings.Split(o.TLSCipherSuites, ",")
	}
	suites, err := cliflag.TLSCipherSuites(names)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: minimum, CipherSuites: suites, NextProtos: []string{"http/1.1"}}, nil
}
