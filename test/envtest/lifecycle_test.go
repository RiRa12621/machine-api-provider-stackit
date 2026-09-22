//go:build envtest

package envtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sync"
	"testing"
	"time"

	providerapi "github.com/RiRa12621/machine-api-provider-stackit/api/v1alpha1"
	"github.com/RiRa12621/machine-api-provider-stackit/pkg/actuator"
	"github.com/RiRa12621/machine-api-provider-stackit/pkg/cloud"
	providermanager "github.com/RiRa12621/machine-api-provider-stackit/pkg/manager"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	maomachine "github.com/openshift/machine-api-operator/pkg/controller/machine"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	crenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const controllerNamespace = "controller-tests"
const directNamespace = "direct-tests"

func TestMachineLifecycle(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Fatal("KUBEBUILDER_ASSETS must point to installed envtest binaries")
	}
	for _, binary := range []string{"etcd", "kube-apiserver"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatalf("required envtest binary %s is missing or not executable: %v", binary, err)
		}
	}
	_, sourceFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the pinned Machine CRD fixture")
	}
	fixturePath := filepath.Join(filepath.Dir(sourceFile), "testdata")
	environment := &crenvtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{fixturePath}, ErrorIfCRDPathMissing: true}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop local API server: %v", err)
		}
	})
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start local API server: %v", err)
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, machinev1.Install, providerapi.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	apiClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{controllerNamespace, directNamespace} {
		if err := apiClient.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Fatal(err)
		}
		createSecrets(t, apiClient, namespace)
	}
	cloudClient := &localCloud{}
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, Metrics: server.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{controllerNamespace: {}}}})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := providermanager.NewFeatureGate("MachineAPIMigration=false")
	if err != nil {
		t.Fatal(err)
	}
	if err := maomachine.AddWithActuatorOpts(mgr, actuator.New(mgr.GetClient(), mgr.GetAPIReader(), cloudClient.factory), controller.Options{MaxConcurrentReconciles: 1}, gate); err != nil {
		t.Fatal(err)
	}
	managerContext, cancel := context.WithCancel(t.Context())
	managerDone := make(chan error, 1)
	go func() { managerDone <- mgr.Start(managerContext) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-managerDone:
			if err != nil {
				t.Errorf("controller manager: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("controller manager did not stop")
		}
	})
	if !mgr.GetCache().WaitForCacheSync(managerContext) {
		t.Fatal("manager cache did not synchronize")
	}

	t.Run("MAO finalizer waits for server and root volume", func(t *testing.T) { testControllerLifecycle(t, apiClient, cloudClient) })
	t.Run("missing initial credentials do not strand deletion", func(t *testing.T) { testMissingCredentialsDeletion(t, apiClient, cloudClient) })
	t.Run("lost create response is recovered without duplicate submission", func(t *testing.T) { testLostCreateResponse(t, apiClient) })
	t.Run("competing status write prevents cloud submission", func(t *testing.T) { testConcurrentStatusWrite(t, apiClient) })
}

func createSecrets(t *testing.T, c client.Client, namespace string) {
	t.Helper()
	for name, data := range map[string]map[string][]byte{
		"credentials": {actuator.CredentialsKey: []byte(`{"keyType":"test-only"}`)},
		"user-data":   {actuator.UserDataKey: []byte(`{"ignition":{"version":"3.4.0"}}`)},
	} {
		if err := c.Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
}

func newMachine(t *testing.T, c client.Client, namespace, name string, missingCredentials bool) *machinev1.Machine {
	t.Helper()
	spec := &providerapi.STACKITMachineProviderSpec{
		ProjectID: "11111111-1111-4111-8111-111111111111", Region: "eu01", ImageID: "22222222-2222-4222-8222-222222222222",
		NetworkID: "33333333-3333-4333-8333-333333333333", MachineType: "c2i.4", AvailabilityZone: "eu01-1",
		SecurityGroups: []string{"44444444-4444-4444-8444-444444444444"}, CredentialsSecret: &corev1.LocalObjectReference{Name: "credentials"}, UserDataSecret: &corev1.LocalObjectReference{Name: "user-data"},
	}
	if missingCredentials {
		spec.CredentialsSecret.Name = "missing"
	}
	raw, err := providerapi.EncodeSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	machine := &machinev1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{actuator.ClusterLabel: "envtest-cluster"}}, Spec: machinev1.MachineSpec{ProviderSpec: machinev1.ProviderSpec{Value: raw}}}
	if err := c.Create(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	return machine
}

func readMachine(ctx context.Context, c client.Reader, key client.ObjectKey) (*machinev1.Machine, error) {
	machine := &machinev1.Machine{}
	err := c.Get(ctx, key, machine)
	return machine, err
}

func waitFor(t *testing.T, message string, condition func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var lastError error
	for time.Now().Before(deadline) {
		ok, err := condition()
		if ok && err == nil {
			return
		}
		lastError = err
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s: %v", message, lastError)
}

func wakeMachine(t *testing.T, c client.Client, key client.ObjectKey) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		machine, err := readMachine(t.Context(), c, key)
		if err != nil {
			return err
		}
		if machine.Annotations == nil {
			machine.Annotations = map[string]string{}
		}
		machine.Annotations["test.stackit.io/reconcile"] = time.Now().Format(time.RFC3339Nano)
		return c.Update(t.Context(), machine)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func journalVerifier(c client.Reader, key client.ObjectKey) func(context.Context, cloud.CreateServerInput) error {
	return func(ctx context.Context, input cloud.CreateServerInput) error {
		machine, err := readMachine(ctx, c, key)
		if err != nil {
			return err
		}
		status, err := providerapi.DecodeStatus(machine.Status.ProviderStatus)
		if err != nil {
			return err
		}
		if !status.CreatePending || status.SpecHash == "" || status.MachineUID != string(machine.UID) || status.ProjectID == "" || status.Region != "eu01" {
			return fmt.Errorf("creation journal was not durably persisted before cloud submission: %#v", status)
		}
		if input.Labels[actuator.MachineUIDLabel] != string(machine.UID) {
			return fmt.Errorf("create omitted UID ownership")
		}
		if string(input.UserData) != `{"ignition":{"version":"3.4.0"}}` {
			return fmt.Errorf("raw Ignition changed")
		}
		return nil
	}
}

func testControllerLifecycle(t *testing.T, c client.Client, cloudClient *localCloud) {
	key := client.ObjectKey{Namespace: controllerNamespace, Name: "managed"}
	cloudClient.mu.Lock()
	cloudClient.beforeCreate = journalVerifier(c, key)
	cloudClient.mu.Unlock()
	machine := newMachine(t, c, key.Namespace, key.Name, false)
	waitFor(t, "provider ID, addresses and durable recovery status", func() (bool, error) {
		current, err := readMachine(t.Context(), c, key)
		if err != nil {
			return false, err
		}
		status, err := providerapi.DecodeStatus(current.Status.ProviderStatus)
		if err != nil {
			return false, err
		}
		return current.Spec.ProviderID != nil && *current.Spec.ProviderID == "stackit://55555555-5555-4555-8555-555555555555" && len(current.Status.Addresses) == 2 && status.RootVolumeID != "" && !status.CreatePending && slices.Contains(current.Finalizers, machinev1.MachineFinalizer), nil
	})
	creates, _, _, journalErr := cloudClient.counts()
	if creates != 1 || journalErr != nil {
		t.Fatalf("cloud create journal failed: calls=%d,error=%v", creates, journalErr)
	}
	if err := c.Delete(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "asynchronous server delete", func() (bool, error) { _, deletes, _, err := cloudClient.counts(); return deletes == 1, err })
	assertFinalizer(t, c, key)
	cloudClient.removeServer()
	wakeMachine(t, c, key)
	waitFor(t, "root volume cleanup observation", func() (bool, error) { _, _, reads, err := cloudClient.counts(); return reads > 0, err })
	assertFinalizer(t, c, key)
	cloudClient.removeVolume()
	wakeMachine(t, c, key)
	waitFor(t, "Machine deletion after both cloud resources disappear", func() (bool, error) {
		_, err := readMachine(t.Context(), c, key)
		return apierrors.IsNotFound(err), nil
	})
}

func assertFinalizer(t *testing.T, c client.Reader, key client.ObjectKey) {
	t.Helper()
	machine, err := readMachine(t.Context(), c, key)
	if err != nil {
		t.Fatal(err)
	}
	if machine.DeletionTimestamp.IsZero() || !slices.Contains(machine.Finalizers, machinev1.MachineFinalizer) {
		t.Fatalf("cleanup finalizer released early: %#v", machine.ObjectMeta)
	}
}

func testMissingCredentialsDeletion(t *testing.T, c client.Client, cloudClient *localCloud) {
	before, _, _, _ := cloudClient.counts()
	machine := newMachine(t, c, controllerNamespace, "missing-credentials", true)
	key := client.ObjectKeyFromObject(machine)
	waitFor(t, "MAO finalizer on unprovisioned Machine", func() (bool, error) {
		current, err := readMachine(t.Context(), c, key)
		return err == nil && slices.Contains(current.Finalizers, machinev1.MachineFinalizer), err
	})
	if err := c.Delete(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "deletion without initial credentials", func() (bool, error) {
		_, err := readMachine(t.Context(), c, key)
		return apierrors.IsNotFound(err), nil
	})
	after, _, _, _ := cloudClient.counts()
	if before != after {
		t.Fatal("cloud submission occurred without credentials")
	}
}

func testLostCreateResponse(t *testing.T, c client.Client) {
	machine := newMachine(t, c, directNamespace, "lost-response", false)
	key := client.ObjectKeyFromObject(machine)
	cloudClient := &localCloud{lostResponse: true, beforeCreate: journalVerifier(c, key)}
	provider := actuator.New(c, c, cloudClient.factory)
	if err := provider.Create(t.Context(), machine); !errors.Is(err, cloud.ErrTransient) {
		t.Fatalf("expected lost response error, got %v", err)
	}
	persisted, err := readMachine(t.Context(), c, key)
	if err != nil {
		t.Fatal(err)
	}
	status, err := providerapi.DecodeStatus(persisted.Status.ProviderStatus)
	if err != nil {
		t.Fatal(err)
	}
	if !status.CreatePending || status.InstanceID != "" {
		t.Fatalf("lost response did not preserve journal: %#v", status)
	}
	// A fresh actuator simulates process restart, with no in-memory request state.
	restarted := actuator.New(c, c, cloudClient.factory)
	if exists, err := restarted.Exists(t.Context(), persisted); err != nil || !exists {
		t.Fatalf("UID recovery failed: exists=%v,error=%v", exists, err)
	}
	if err := restarted.Update(t.Context(), persisted); err != nil {
		t.Fatal(err)
	}
	persisted, err = readMachine(t.Context(), c, key)
	if err != nil {
		t.Fatal(err)
	}
	status, err = providerapi.DecodeStatus(persisted.Status.ProviderStatus)
	if err != nil {
		t.Fatal(err)
	}
	if status.CreatePending || status.InstanceID == "" || status.RootVolumeID == "" || persisted.Spec.ProviderID == nil || len(persisted.Status.Addresses) != 2 {
		t.Fatalf("recovery did not persist identity/status: %#v", status)
	}
	creates, _, _, journalErr := cloudClient.counts()
	if creates != 1 || journalErr != nil {
		t.Fatalf("duplicate or unjournaled create: %d,%v", creates, journalErr)
	}
}

func testConcurrentStatusWrite(t *testing.T, c client.Client) {
	machine := newMachine(t, c, directNamespace, "status-conflict", false)
	key := client.ObjectKeyFromObject(machine)
	cloudClient := &localCloud{}
	conflictingClient := &statusConflictClient{Client: c, beforePatch: func(ctx context.Context) error {
		fresh, err := readMachine(ctx, c, key)
		if err != nil {
			return err
		}
		phase := "Provisioning"
		fresh.Status.Phase = &phase
		return c.Status().Update(ctx, fresh)
	}}
	provider := actuator.New(conflictingClient, c, cloudClient.factory)
	if err := provider.Create(t.Context(), machine); !apierrors.IsConflict(err) {
		t.Fatalf("expected an API-server optimistic-lock conflict before cloud submission, got %v", err)
	}
	creates, _, _, _ := cloudClient.counts()
	if creates != 0 {
		t.Fatal("cloud submission preceded successful journal CAS")
	}
	persisted, err := readMachine(t.Context(), c, key)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status.Phase == nil || *persisted.Status.Phase != "Provisioning" || persisted.Status.ProviderStatus != nil {
		t.Fatalf("competing status was overwritten: %#v", persisted.Status)
	}
}

// Insert a competing write after the actuator's last read but before its status
// PATCH. The real API server must reject the old resourceVersion, proving the
// creation journal uses a compare-and-swap rather than a blind merge patch.
type statusConflictClient struct {
	client.Client
	beforePatch func(context.Context) error
	once        sync.Once
}

func (c *statusConflictClient) Status() client.SubResourceWriter {
	return &statusConflictWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

type statusConflictWriter struct {
	client.SubResourceWriter
	owner *statusConflictClient
}

func (w *statusConflictWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
	var err error
	w.owner.once.Do(func() { err = w.owner.beforePatch(ctx) })
	if err != nil {
		return err
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}
