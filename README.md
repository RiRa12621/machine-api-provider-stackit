# Machine API provider for STACKIT

**WIP: for local validation and manual review.** This is a native OpenShift
Machine API provider for STACKIT worker virtual machines. It implements the
Machine API Operator's actuator interface and uses `machine.openshift.io/v1beta1`
Machines and MachineSets. It does not create Cluster API infrastructure resources.

The companion [Machine API Operator branch](https://github.com/RiRa12621/machine-api-operator/tree/feat/stackit-provider)
deploys this controller only with explicit experimental opt-in. Both repositories
remain under RiRa12621 for review before any upstream submission.

This contribution has no live cloud validation or supported OpenShift/RHCOS image
combination. Bootstrap, node joining, replacement, upgrades, storage detachment,
credential rotation and complete cloud cleanup must be validated in an isolated
STACKIT project before production use.

## Scope and architecture

* MAO supplies MachineSet replica management, node linking, MachineHealthChecks,
  generic admission, RBAC, certificates and deployment lifecycle.
* This binary uses MAO's shared Machine lifecycle and drain controllers, implements
  STACKIT create/observe/delete operations, and serves four supplemental provider
  admission endpoints. It reports `stackit://<server-UUID>` provider IDs.
* The cluster must already be a standalone OpenShift cluster with both
  Infrastructure platform types `External`, `spec.platformSpec.external.platformName:
  STACKIT`, and control-plane topology `HighlyAvailable` or `SingleReplica`.
  `platformName` is a compatibility check, not an automatic enablement signal.
* Hosted control planes (`controlPlaneTopology: External`) are rejected by both
  the operator integration and provider startup. HyperShift's STACKIT workers
  continue to use CAPSTK, avoiding two systems managing the same workers.
* Only existing projects, networks, security groups and images are referenced.
  New servers receive new image-backed root disks with automatic deletion enabled.
  Additional disks, networks, security groups and images are never deleted by this
  provider. Extra attached disks block server deletion until they are detached.

This does not add installer provisioning, control-plane MachineSet support,
automatic release-image selection, cloud-controller/CSI deployment, registry
storage, or cluster-autoscaler integration. Existing workers are not adopted by
server name; cloud ownership must match the Machine UID and cluster label.

## Prerequisites

1. An existing standalone External/STACKIT OpenShift cluster and MAO built from
   the companion branch. Enable `--enable-stackit-provider` and provide
   `clusterAPIControllerSTACKIT` in MAO's images JSON. See the companion operator's
   `docs/dev/stackit.md` for the operator rollout procedure. No default provider image
   is supplied, and this repository does not publish container images automatically.
2. The existing cluster must have been installed/configured for an external cloud
   controller: `status.platformStatus.external.cloudControllerManager.state` must
   be `External`. An omitted value or `None` does not establish cloud-controller
   node initialization, and this infrastructure field cannot be added or changed
   later. Do not patch status to retrofit it. Deploy and configure a compatible
   STACKIT cloud controller independently, which initializes nodes with matching
   STACKIT provider IDs. Configure CSI separately if persistent workloads need it.
3. An amd64 RHCOS image matching your OpenShift release, imported into the project
   and boot-tested on STACKIT. Native support depends on the
   [Ignition](https://github.com/coreos/ignition/pull/2328) and
   [Afterburn](https://github.com/coreos/afterburn/pull/1307) work. The image must
   contain that support and select the `stackit` platform ID. An arbitrary SKE,
   Linux or OpenStack image is not an established substitute. The provider sends
   raw Ignition as user data (one base64 encoding at the SDK boundary) and enables
   the config drive; that setting alone does not supply bootstrap support. Verify
   metadata delivery and hostname behavior in the image.
4. An existing network in the selected project/region, explicit security groups,
   capacity/quota in the selected availability zone, and connectivity to the
   cluster API, machine-config server, DNS, time servers and image registries.
5. A project-scoped STACKIT service account key allowed to list/get/create/delete
   servers and read networks and volumes, plus the permissions required by server
   creation to attach the selected network/groups and create an image-backed root
   disk. Determine exact roles with your STACKIT administrators; no broad IAM
   role is automatically granted.

## Build and configure

```sh
make build
docker build -t ghcr.io/rira12621/machine-api-provider-stackit:review .
```

Publish a reviewed image to your registry separately and configure MAO with its
digest. The binary is `/machine-controller-manager`; MAO provides namespace,
leader-election, feature-gate and TLS arguments. Admission listens on port 8440
with the service-serving certificate under `/etc/machine-api-operator/tls`.
Health and metrics defaults match MAO (`:9440` and `:8081`).

Credentials and Ignition must be Secrets in the Machine namespace. Neither
environment credentials nor SDK credential files provide a fallback:

```sh
kubectl -n openshift-machine-api create secret generic stackit-machine-api-credentials \
  --from-file=serviceaccount.json=/secure/path/service-account-key.json
```

Use the cluster-generated `worker-user-data` Secret when available. Otherwise
create it from the cluster's valid worker Ignition, not from a generic example:

```sh
kubectl -n openshift-machine-api create secret generic worker-user-data \
  --from-file=userData=/secure/path/worker.ign
```

An optional credentials Secret `project-id` must match the provider spec. Secret
contents are read directly from the API on each reconcile so rotation does not
wait on a cached copy. Credentials and Ignition are never copied into provider
status. Restore missing credentials before deleting provisioned Machines.

Edit [examples/machineset.yaml](examples/machineset.yaml), replacing all example
UUIDs and the cluster identity with `Infrastructure.status.infrastructureName`.
Select a release-matched image, actual machine type, zone and security groups.
Review it with server-side dry-run after both admission backends are ready:

```sh
kubectl apply --dry-run=server -f examples/machineset.yaml
kubectl apply -f examples/machineset.yaml
kubectl -n openshift-machine-api scale machineset example-stackit-workers-eu01-1 --replicas=1
```

The example starts at zero replicas so applying an unreviewed template does not
submit cloud requests. Use one MachineSet per zone. Approve expected worker CSRs
through your cluster's normal process, and verify node health and provider IDs.

The default root disk is 32 GiB (minimum 16 GiB). Project, region, network, image,
machine type, zone, root disk settings, security groups and SSH key are immutable
on a Machine. Update the MachineSet template and replace Machines to change them.
Credential/user-data Secret references may rotate; new user data only affects
future servers. Upload and boot-test a matching RHCOS image for release upgrades.

## Failure recovery and deletion

Creation intent is persisted in `status.providerStatus` **before** the cloud
request. Ownership uses STACKIT-compatible flat labels:
`openshift-machine-uid`, `openshift-cluster-id` and
`openshift-machine-provider=stackit`. The cloud API exposes no idempotency token
for this request. After a timeout, restart, or failed status write, the controller
discovers a server only through those ownership labels. Duplicate matches or
contradictory identities stop reconciliation.

An uncertain create stays `createPending: true` until the server and root disk
can be verified. It is never retried blindly. If no server appears, an
administrator must audit the project, region, Machine UID and cloud request
history before correcting the journal. Do not clear `createPending`, remove
finalizers or recreate the Machine merely to bypass this safeguard: those actions
can create duplicates or leak resources. A definite no-server result established
by a cloud audit can be recovered by clearing the pending flag on the status
subresource, provided no instance/root identity was recorded. Preserve all
recorded ownership fields and document the audit. Deleting a Machine whose initial
credentials failed before any journal was written does not require those missing
credentials.

For normal scale-down, let MAO drain the node and let CSI detach workload volumes.
The actuator validates ownership before deleting a server and retains the Machine
until both the server and its recorded root disk return `NotFound`. A successful
asynchronous DELETE response is not completion. Missing/invalid credentials,
forbidden API access, attached data volumes, mismatched labels or a root disk
without automatic deletion retain the Machine for recovery. User-owned resources
are never repaired or deleted to force progress.

Before dismantling the cluster, remove workloads, PVCs and application load
balancers and verify their cleanup while CSI and the cloud controller are still
running. Keep controllers, Secrets and the Machine namespace until cleanup
finishes. Check STACKIT directly for remaining servers/disks. Disabling the
experimental MAO integration stops its owned deployment/webhooks; it does not
delete Machines or cloud resources.

## Local validation

Use Go 1.26.3 and golangci-lint v2.11.4:

```sh
make check
make setup-envtest
export KUBEBUILDER_ASSETS="$PWD/.envtest/k8s/1.34.1-linux-amd64"
make test-envtest
```

Unit/race tests cover strict provider JSON, admission, MAO flags, credential
isolation, SDK payloads, ownership, creation journaling, recovery and asynchronous
cleanup. Envtest uses a local Kubernetes API server, the pinned upstream Machine
CRD and a fake cloud; it never creates real STACKIT resources. GitHub Actions
runs these local checks without cloud credentials. Live smoke tests and image
publishing are intentionally separate manual steps.

Keep PRs in WIP/draft state until local and live validation evidence has been
reviewed. This repository and the companion operator changes have not been
submitted to upstream maintainers.
