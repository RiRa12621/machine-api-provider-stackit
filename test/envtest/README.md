# Local Machine API integration tests

These tests run a local API server and etcd with the real OpenShift Machine CRD,
the upstream MAO machine and drain controllers, and an in-memory cloud client.
They do not connect to STACKIT or provision cloud resources.

Set `KUBEBUILDER_ASSETS` to an installed Kubernetes envtest binary directory, then run:

```sh
go test -race -tags=envtest ./test/envtest -timeout=15m
```

Missing binaries are a test failure, not a skipped integration suite. Kubernetes
1.34.1 is the initial local validation target, matching the MAO envtest setup.

`testdata/machines.crd.yaml` is an unmodified copy of
[`install/0000_30_machine-api-operator_02_machine.Default.crd.yaml`](https://github.com/openshift/machine-api-operator/blob/26d7767bee99ff544430fd138571989f52d67c00/install/0000_30_machine-api-operator_02_machine.Default.crd.yaml)
from upstream `openshift/machine-api-operator` commit
`26d7767bee99ff544430fd138571989f52d67c00`, the provider's pinned MAO dependency.
It retains the upstream Apache-2.0 license. No platform-specific CRD modifications
are needed: the provider configuration is embedded in `RawExtension` fields.

Coverage includes the creation journal being durable before cloud submission,
recovery after a lost create response, optimistic concurrency protection,
Machine provider ID and status persistence, asynchronous server and root-volume
deletion holding the MAO finalizer, and deletion after missing initial credentials.
