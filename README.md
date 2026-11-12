# Homelab Autoscaler

A Kubernetes operator that scales a homelab cluster of physical machines: it powers machines on when
workload demand needs them and off when the pool is done with them, through
[wake-on-LAN, IPMI, a smart PDU, or any script you can run in a pod](https://autoscaler.homecluster.dev/docs/api-reference/examples/production-examples/).

Scale-up intent is an object. `Node` is what a slot *is*, `NodeClaim` is one request for one machine —
with an attribution, a deadline and an end — and `Node.status` is what the cluster observes. That is
why a GitOps apply cannot revert a scale-up, and why every attempt ends by itself.

## Use of AI in development

The code, the tests and the documentation of this project are written with the help of AI coding
assistants. Every change is read, tested and merged by a human maintainer, who is the one responsible
for what lands: the unit tests, envtest, the integration suite and the k3d end-to-end run — which stops
and starts real cluster nodes — run on every pull request, and releases are cut from `main` by CI.

Treat it like any other open-source project. Read the code you are about to run, check the manifests
against your own cluster, and verify the behaviour on a pool you can afford to empty before it sits in
front of machines that draw power.

## Quick Start

The chart ships the CRDs, the manager (which embeds the gRPC server cluster-autoscaler calls) and,
optionally, cluster-autoscaler itself. cert-manager is required by the chart's defaults: it issues the
webhook and metrics certificates the manager mounts.

```bash
helm repo add homelab-autoscaler https://autoscaler.homecluster.dev
helm repo update

kubectl create namespace homelab-autoscaler-system

helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --version 0.2.3 \
  --set clusterAutoscaler.enabled=true \
  --wait
```

The [Quick Start](docs/getting-started/quick-start.md) runs the same system on k3d in one cluster
you can create in a minute; the [Installation guide](docs/getting-started/installation.md) is the
values reference.

### A Pool Is Two Objects

The pool's policy. Every field of the spec except `minSize` and `nodeReadinessTaints` is required, the
utilization thresholds are strings, and there is no `maxSize`: the pool's ceiling is the number of
`Node` CRs labelled `group: worker-nodes`.

```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: worker-nodes
  namespace: homelab-autoscaler-system
spec:
  minSize: 0
  scaleDownUtilizationThreshold: "0.5"
  scaleDownGpuUtilizationThreshold: "0.5"
  scaleDownUnneededTime: "10m"
  scaleDownUnreadyTime: "20m"
  maxNodeProvisionTime: "15m"
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: true
```

One machine slot. `startupPodSpec` / `shutdownPodSpec` are a `MinimalPodSpec` — image, command, args,
Secret/ConfigMap volumes, serviceAccount — and a field the CRD does not know is pruned silently, so
credentials come from a mounted Secret and not from `env`.

```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: worker-01
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
spec:
  kubernetesNodeName: worker-01
  pricing:
    hourlyRate: "0.05"
    podRate: "0.001"
  nodeCapacity:
    cpu: 1950m
    memory: 3.2Gi
  nodeLabels:
    topology.kubernetes.io/zone: fsn1
  startupPodSpec:
    image: gemtest/ipmitool:latest
    command: ["/bin/sh"]
    args:
      - -c
      - |
        ipmitool -I lanplus -H worker-01-ipmi.homelab.local \
          -U "$(cat /etc/ipmi/user)" -P "$(cat /etc/ipmi/password)" power on
    volumes:
      - name: ipmi-credentials
        secretName: ipmi-credentials
        mountPath: /etc/ipmi
    serviceAccount: controller-manager
  shutdownPodSpec:
    image: gemtest/ipmitool:latest
    command: ["/bin/sh"]
    args:
      - -c
      - |
        ipmitool -I lanplus -H worker-01-ipmi.homelab.local \
          -U "$(cat /etc/ipmi/user)" -P "$(cat /etc/ipmi/password)" power soft
    volumes:
      - name: ipmi-credentials
        secretName: ipmi-credentials
        mountPath: /etc/ipmi
    serviceAccount: controller-manager
```

Watch what the pool is doing:

```bash
kubectl get groups -n homelab-autoscaler-system                     # SLOTS ON OFF LASTSCALE HEALTH
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system # POWERSTATE PHASE GROUP ZONE SINCE ON24H
kubectl get nodeclaim -n homelab-autoscaler-system -o wide          # the requests, and their deadlines
```

## How It Works

```
Cluster Autoscaler ──gRPC──► the operator (CloudProvider) ──► NodeClaim per machine asked for
                                                          └─► a Job per power operation ──► WoL / IPMI / PDU / SSH
```

1. cluster-autoscaler fits a pending pod on the group's node template and calls `IncreaseSize`
2. the provider creates a `NodeClaim` on a free slot, with `requestedAt` and `spec.deadlineAt`
3. the slot's startup Job powers the machine on, the claim moves `pending → booting → ready`
4. the machine registers, clears the pool's readiness gate, and the pod lands
5. on the way down, `DeleteNodes` records the request on the claim and cancels it; the slot's shutdown
   Job powers the machine off, and the claim ends `completed`

A machine the pool no longer wants is a slot with no live claim: that is the whole model, and it is why
a scale-up has an identity, a budget, and an end.

## Limits Worth Knowing

- **No domain metrics.** The chart exposes controller-runtime metrics; the pool's state is on the
  objects. `prometheus.enable: true` renders a ServiceMonitor over the metrics endpoint
- **Bounds are constants.** The resync (5m), the Job monitor poll (30s), the readiness gate (4m), the
  registration window (90s), the attempt bound (3) and the boot budget (12m10s) are not configurable;
  the Group's fields, the slot's fields and the claim's fields are
- **One namespace per release.** The manager reads `HOMELAB_AUTOSCALER_NAMESPACE`, defaulted by the
  chart to the release namespace, and a pool's `Group`, `Node` and `NodeClaim` objects live in it
- **The gRPC server serves plaintext** (`--grpc-server-address=:50051`). Run it on a private network
  or behind your own TLS

[Known Issues](docs/troubleshooting/known-issues.md) is the full list, with the runbooks it links out
to.

## Development

```bash
make test           # unit + envtest
make lint           # golangci-lint
make test-e2e       # the k3d suite: a real autoscaler, real Jobs, k3d containers as machines
make pre-commit     # generate, sync the chart's CRDs, fmt, vet, lint, test, stage
```

See [Development Setup](docs/development/setup.md) for the local-controller / in-cluster-autoscaler
environment, and [E2E Test Flow](docs/development/e2e-test-flow.md) for what the suite proves.

## Documentation

The site's structure, in the order the docs read it:

- [Start Here](docs/intro.md) - what this is, and where to begin
- [Overview](docs/getting-started/overview.md), [Quick Start](docs/getting-started/quick-start.md),
  [Installation](docs/getting-started/installation.md)
- Concepts: [The NodeClaim model](docs/concepts/nodeclaim-model.md),
  [Node labels](docs/concepts/node-labels.md),
  [Read the Pool at a Glance](docs/concepts/pool-at-a-glance.md),
  [Architecture](docs/architecture/overview.md)
- Operations: [Common Issues](docs/troubleshooting/common-issues.md),
  [Debugging Guide](docs/troubleshooting/debugging-guide.md),
  [Known Issues](docs/troubleshooting/known-issues.md)
- Reference: [Group](docs/api-reference/crds/group.md), [Node](docs/api-reference/crds/node.md),
  [NodeClaim](docs/api-reference/crds/nodeclaim.md), [Examples](docs/api-reference/examples/index.md),
  [Events](docs/reference/events.md), [Changelog](docs/releases/changelog.md),
  [Upgrade 0.1 to 0.2](docs/releases/upgrade-0.1-to-0.2.md)
- Contributing: [Development Setup](docs/development/setup.md),
  [CRD Synchronization](docs/development/crd-sync.md),
  [State Management](docs/development/state.md),
  [NodeClaim Design](docs/development/nodeclaim.md),
  [E2E Test Flow](docs/development/e2e-test-flow.md)

The site is at [autoscaler.homecluster.dev](https://autoscaler.homecluster.dev), and the Helm
repository is at the same address.

## Contributing

1. Check [open issues](https://github.com/homecluster-dev/homelab-autoscaler/issues)
2. Follow the [development guide](docs/development/setup.md)
3. Run `make pre-commit` and the tests before you open a PR
4. Write the docs of what you changed in the same PR, against
   [the style guide](DOCUMENTATION_STYLE_GUIDE.md)

## License

Apache License 2.0. Every source file carries the header; the repository has no `LICENSE` file yet, so
read the terms from the headers.
