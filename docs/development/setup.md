# Development Setup

A development environment for the homelab-autoscaler operator: the code on your laptop, the cluster in
k3d, and cluster-autoscaler in the cluster calling your local gRPC server.

## Requirements

- Go 1.26 (`go.mod` declares `go 1.26.0`)
- Docker, and [k3d](https://k3d.io) — the dev and e2e path of this repo
- kubectl, Helm 3
- make, python3 (the VM control server of the e2e suite)
- cert-manager, for a cluster that runs the chart with its defaults (`webhook.enable: true`)

[`mise`](https://mise.jdx.dev) pins the tools this repo uses (`mise.toml`: `go`, `helm`); `make`
downloads `controller-gen`, `setup-envtest`, `golangci-lint` and `kustomize` into `bin/` on the first
target that needs them, so nothing else has to be installed by hand.

## Quick Start

```bash
git clone https://github.com/homecluster-dev/homelab-autoscaler.git
cd homelab-autoscaler
make help          # every target, with its description
```

The two commands that answer most questions:

```bash
make test          # unit + envtest: generates, syncs CRDs, runs go test against a local API server
make lint          # golangci-lint, via the pinned binary in bin/
```

## Run The Operator Against A k3d Cluster

```bash
./examples/k3d/create-cluster.sh          # 3 servers, 2 agents, pinned k3s image
export KUBECONFIG=$(pwd)/kubeconfig       # the script writes ./kubeconfig
kubectl config use-context k3d-homelab-autoscaler

make install                              # CRDs from config/crd/bases
make run                                  # the manager on your laptop, webhooks enabled
```

`make run` uses the kubeconfig in your shell environment, so point `KUBECONFIG` at the cluster first.
To run the manager with explicit flags, put the whole command on one line, or end each line with a
single backslash:

```bash
KUBECONFIG=$(pwd)/kubeconfig ENABLE_WEBHOOKS=false go run cmd/main.go \
  --metrics-bind-address=:8443 \
  --health-probe-bind-address=:8081 \
  --leader-elect=false \
  --enable-grpc-server=true \
  --grpc-server-address=:50051
```

`ENABLE_WEBHOOKS=false` is what the dev and e2e setups use: the chart's `ValidatingWebhookConfiguration`
points at an in-cluster service, so a manager that runs outside the cluster must not serve it, and the
admission rules (one live claim per slot, a reserved label key, a `kubernetesNodeName` collision) are
then not enforced — the suite's `internal/webhook/infra/v2alpha1` tests cover that half.

## The Development Path: Local Controller, In-Cluster Autoscaler

`make test-e2e` and `make run-e2e` drive the full loop, and `development/install-dev.sh` is the
environment they run on. It creates the k3d cluster, installs cluster-autoscaler with
`development/development-values.yaml`, and applies `development/local-service-forwarder-dev.yaml`: a
headless Service `homelab-autoscaler-grpc-local` whose Endpoints are the Docker bridge gateway, so the
in-cluster autoscaler dials `homelab-autoscaler-grpc-local:50051` and reaches your laptop's `:50052`.

```bash
make test-e2e        # full run, with cleanup at the end
make run-e2e         # the same run, leaving the cluster up for inspection
make cleanup-e2e     # tear it down
```

The flow, the gates and the suite's clocks are documented in
[E2E Test Flow](../E2E_TEST_FLOW.md).

The power API the pool's Jobs call is `examples/k3d/vm_control_server.py`: an HTTP server on **9052**
that answers `/start?vm=<name>` and `/stop?vm=<name>` with `k3d node start|stop`, so a "machine" is a
container the suite controls.

## Test gRPC

```bash
# In-cluster install: the Service the chart deploys
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc-service 50051:50051 &

# Dev install (the local forwarder, laptop's gRPC on 50052)
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc-local 50052:50051 &

grpcurl -plaintext localhost:50051 list
grpcurl -plaintext -d '{}' localhost:50051 \
  clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider/NodeGroups
```

The full service name is `clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider`
(`proto/cloud_provider.proto`); `externalgrpc.CloudProvider` alone resolves to nothing.

## Common Development Tasks

```bash
make build          # build the manager binary (generates, syncs CRDs, fmt, vet)
make manifests      # controller-gen: CRDs, RBAC, webhook manifests -> config/
make helm-sync-crds # CRDs from config/crd/bases -> dist/chart/templates/crd
make generate       # deepcopy methods
make docker-build   # the manager image
make install        # CRDs only;  make uninstall removes them
make deploy         # the controller into the current kubeconfig
make undeploy       # the controller, leaving the CRDs
make helm-lint      # lint dist/chart
make helm-template  # render the chart's templates
```

See [CRD Synchronization](crd-sync.md) for why `make manifests` alone is not enough for a chart
release.

## Project Structure

```
homelab-autoscaler/
├── api/infra/v2alpha1/     # Group, Node, NodeClaim types (the served and storage version)
├── cmd/main.go             # manager, gRPC server, webhooks
├── config/                 # CRDs, RBAC, manager, webhook, samples (kustomize)
├── dist/chart/             # the Helm chart, with the CRDs as templates
├── development/            # the dev environment: install-dev.sh, values, the gRPC forwarder
├── examples/k3d/           # the k3d fixtures: cluster script, slots, groups, VM control server
├── internal/
│   ├── controller/infra/   # group, slot, claim reconcilers, the lifecycle
│   ├── controller/core/    # the observation reconcilers
│   ├── claim/              # the claim model: budget, attempt, phases, inventory
│   ├── fsm/                # the node state machine, the lifecycle Jobs, the coordination lock
│   ├── phase/              # the derivation of Node.status.phase, and its hygiene test
│   ├── grpcserver/         # the cluster-autoscaler cloud provider
│   ├── autoscaler/         # reading the autoscaler's markers off a node
│   └── webhook/infra/v2alpha1/
├── proto/                  # the cloud-provider proto
└── test/                   # e2e (k3d) and the helpers in test/utils
```

## Key Files

| File | Purpose |
|------|---------|
| `cmd/main.go` | Entry point: flags, managers, the gRPC server, the webhooks |
| `api/infra/v2alpha1/group_types.go` | The pool's policy, and the readiness taints |
| `api/infra/v2alpha1/node_types.go` | The slot: `MinimalPodSpec`, taints, labels, capacity, pricing |
| `api/infra/v2alpha1/nodeclaim_types.go` | The request: phases, reasons, conditions, the deadline |
| `internal/claim/claim.go` | The attempt budget, the attempt bound, the scale-down streak |
| `internal/phase/phase.go` | `Derive`: the one formula behind `Node.status.phase` |
| `internal/grpcserver/server.go` | The RPCs, and where a claim is created |
| `internal/controller/infra/` | The slot reconciler, the claim controller, the lifecycle |
| `Makefile` | Every target above |

## Debugging Tips

```bash
# Structured, verbose logs from a manager on your laptop
KUBECONFIG=$(pwd)/kubeconfig go run cmd/main.go --zap-log-level=debug

# The same, in-cluster
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager -f

# The state a run left behind is on the objects, not in a log
kubectl get groups,nodeclaims -n homelab-autoscaler-system
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
kubectl describe node.infra.homecluster.dev <slot> -n homelab-autoscaler-system
```

The namespace is a configuration, not a constant: the manager reads `HOMELAB_AUTOSCALER_NAMESPACE`,
and the chart defaults it to the release namespace
(`internal/config/namespace.go`). The e2e suite and the fixtures in `examples/k3d/` use
`homelab-autoscaler-system`.

## Contributing

1. `make pre-commit` — generates, syncs the CRDs into the chart, formats, vets, lints, runs the unit
   tests and stages the generated files. `make install-pre-commit` runs it on every commit
2. Add a test with the change: a phase move in `internal/claim`, a derivation in `internal/phase`,
   an RPC in `internal/grpcserver`, and the loop end to end in `test/e2e`
3. Update the docs in the same PR: the CRD pages for a field, the runbooks in
   [Common Issues](../troubleshooting/common-issues.md) for an operator-visible bound, and
   [the changelog](../releases/changelog.md) for a release
4. Write docs against [the documentation style guide](https://github.com/homecluster-dev/homelab-autoscaler/blob/main/DOCUMENTATION_STYLE_GUIDE.md)

## Related Documentation

- [E2E Test Flow](../E2E_TEST_FLOW.md) — the suite, spec by spec
- [Architecture Overview](../architecture/overview.md) — the components
- [State Management](../architecture/state.md) — the input law and the phase table
- [CRD Synchronization](crd-sync.md) — `config/crd/bases` to `dist/chart/templates/crd`
- [Debugging Guide](../troubleshooting/debugging-guide.md) — debugging a cluster
