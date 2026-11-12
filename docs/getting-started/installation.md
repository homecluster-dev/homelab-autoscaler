# Installation

Install homelab-autoscaler with the Helm chart. The chart ships the CRDs, the manager (which embeds
the gRPC server cluster-autoscaler talks to) and, optionally, cluster-autoscaler itself.

## Prerequisites

- Kubernetes 1.32+. The chart's default cluster-autoscaler image is `v1.35.0`, and
  cluster-autoscaler is supported a few minor versions below its own: set
  `clusterAutoscaler.image.tag` to your cluster's minor, and never to one older than it. The pair this
  repo's CI runs is Kubernetes 1.35 (`rancher/k3s:v1.35.3-k3s1`,
  `examples/k3d/create-cluster.sh`) with cluster-autoscaler `v1.35.0`
- Helm 3
- kubectl configured for your cluster
- [cert-manager](https://cert-manager.io/docs/installation/) — with the chart defaults
  (`certmanager.enable: true`, `webhook.enable: true`, `metrics.enable: true`) the manager mounts
  certificates cert-manager issues, so its pod stays stuck until cert-manager is installed. Set all
  three to `false` if you do not want it

### Cluster Requirements

- **A machine that is never a slot.** The API server, the manager and cluster-autoscaler must run on
  machines the pool never powers off. The pool's slots are the `Node` CRs you declare, and every one
  of them can be at zero: that is the point of the pool, and the operator's own pods must survive it
- **RBAC enabled** — the chart's `rbac.enable: true` grants the manager rights over
  `groups`, `nodes`, `nodeclaims` (including `/status` and `/finalizers`), Jobs, `corev1.Node`
  objects and events
- **The CRDs** — installed by the chart (`crd.enable: true`), so no cluster-side support work is
  needed beyond `apiextensions.k8s.io/v1`

## Installation Methods

### Method 1: Helm Repository

```bash
helm repo add homelab-autoscaler https://autoscaler.homecluster.dev
helm repo update

# The chart version is the release: 0.2.3 ships image 0.2.3
helm search repo homelab-autoscaler --versions | head

kubectl create namespace homelab-autoscaler-system

helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --version 0.2.3 \
  --wait
```

The namespace is not free: the manager reads `HOMELAB_AUTOSCALER_NAMESPACE`, which the chart defaults
to the release namespace, and the `Group`, `Node` and `NodeClaim` objects of a pool live in it. Keep
the CRs and the release in one namespace.

### Method 2: Local Chart

```bash
# From the project root: renders the CRDs from config/crd/bases into dist/chart/templates/crd/
make helm-sync-crds

helm install homelab-autoscaler ./dist/chart \
  --namespace homelab-autoscaler-system \
  --create-namespace \
  --wait
```

## Configuration

Every key below is a key of `dist/chart/values.yaml`. Read the defaults with
`helm show values homelab-autoscaler/homelab-autoscaler`.

| Key | Default | What it does |
| --- | --- | --- |
| `controllerManager.enabled` | `true` | Deploy the manager Deployment |
| `controllerManager.replicas` | `1` | One replica; the manager elects a leader, so more replicas are hot standby |
| `controllerManager.container.image.repository` | `homeclusterdev/autoscaler` | The manager image |
| `controllerManager.container.image.tag` | `"0.2.3"` | Image tag, pinned to the chart version |
| `controllerManager.container.resources` | cpu `10m`/`500m`, memory `64Mi`/`128Mi` | requests/limits of the manager |
| `controllerManager.container.args` | `--leader-elect`, `--metrics-bind-address=:8443`, `--health-probe-bind-address=:8081` | Manager flags. The gRPC server is enabled by the binary's defaults (`--enable-grpc-server=true`, `--grpc-server-address=:50051`); add flags here to change them |
| `controllerManager.container.livenessProbe` / `.readinessProbe` | `/healthz`, `/readyz` on `8081` | Probes |
| `controllerManager.serviceAccountName` | `homelab-autoscaler-controller-manager` | The ServiceAccount the manager runs as |
| `controllerManager.nodeSelector` / `.tolerations` / `.affinity` | `{}` / `[]` / `{}` | Placement of the manager pod |
| `crd.enable` | `true` | Render the three CRDs into the release |
| `crd.keep` | `true` | Add `helm.sh/resource-policy: keep`, so `helm uninstall` leaves the CRDs (and every Group, Node and NodeClaim) installed |
| `rbac.enable` | `true` | Roles, ClusterRoles and bindings |
| `webhook.enable` | `true` | The validating webhooks: one live claim per slot, a reserved label key, a `kubernetesNodeName` collision. Needs cert-manager |
| `certmanager.enable` | `true` | Issue the webhook and metrics certificates |
| `metrics.enable` | `true` | The `…-controller-manager-metrics-service` on `8443` |
| `prometheus.enable` | `false` | A `ServiceMonitor` for the metrics endpoint. Needs the Prometheus Operator CRDs |
| `networkPolicy.enable` | `false` | NetworkPolicies for the manager |
| `grpcService.enabled` | `true` | The `homelab-autoscaler-grpc-service` Service cluster-autoscaler calls |
| `grpcService.type` | `ClusterIP` | Service type |
| `grpcService.port` | `50051` | Port, on the Service and on the container |
| `clusterAutoscaler.enabled` | `false` | Deploy cluster-autoscaler with the external-gRPC cloud provider pointed at this chart's gRPC Service |
| `clusterAutoscaler.image.repository` / `.tag` / `.pullPolicy` | `registry.k8s.io/autoscaling/cluster-autoscaler`, `v1.35.0@sha256:2fc9433…`, `IfNotPresent` | The cluster-autoscaler image. `tag` carries a digest: replace both together |
| `clusterAutoscaler.grpcProvider.endpoint` | `"{{ include "chart.name" . }}-grpc-service:50051"` | The endpoint written into its `--cloud-config` ConfigMap. It is a Helm template expression, not a literal host |
| `clusterAutoscaler.scaleDownDelay` | `"10m"` | `--scale-down-delay-after-add` |
| `clusterAutoscaler.scaleDownUnneededTime` | `"10m"` | `--scale-down-unneeded-time` |
| `clusterAutoscaler.scaleDownUtilizationThreshold` | `0.5` | `--scale-down-utilization-threshold`, the cluster-wide number; a Group's `spec.scaleDownUtilizationThreshold` is the per-pool one |
| `clusterAutoscaler.skipNodesWithLocalStorage` / `.skipNodesWithSystemPods` | `true` / `true` | The two `--skip-nodes-with-*` flags |
| `clusterAutoscaler.extraArgs` | `[]` | Any further cluster-autoscaler flag, e.g. `--balance-similar-node-groups` |
| `clusterAutoscaler.resources` | cpu `100m`, memory `300Mi` | requests/limits of the cluster-autoscaler pod |
| `clusterAutoscaler.serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | Its ServiceAccount |
| `clusterAutoscaler.nodeSelector` / `.tolerations` / `.affinity`, `.podAnnotations` / `.podLabels` | `{}` / `[]` | Placement and metadata |

There is no `controller.leaderElection` key: `--leader-elect` is already in
`controllerManager.container.args`, and leader election is the manager's own flag.

### Values For A Pool That Scales

```yaml
# values.yaml
controllerManager:
  replicas: 1
  container:
    image:
      repository: homeclusterdev/autoscaler
      tag: "0.2.3"
    resources:
      limits:
        cpu: 500m
        memory: 128Mi
      requests:
        cpu: 10m
        memory: 64Mi

grpcService:
  enabled: true
  type: ClusterIP
  port: 50051

# cluster-autoscaler in-cluster, talking to the gRPC Service this chart deploys
clusterAutoscaler:
  enabled: true
  image:
    repository: registry.k8s.io/autoscaling/cluster-autoscaler
    tag: v1.35.0@sha256:2fc9433dd3b47baef0e68d6b1110b770a3c93db10c7bead4c5bc6cb21c72b875
  scaleDownDelay: "10m"
  scaleDownUnneededTime: "10m"
  scaleDownUtilizationThreshold: 0.5

webhook:
  enable: true
rbac:
  enable: true
prometheus:
  enable: false
certmanager:
  enable: true
```

```bash
helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --values values.yaml \
  --wait
```

Set `clusterAutoscaler.grpcProvider.endpoint` only when cluster-autoscaler runs somewhere the chart's
Service does not reach — a cluster-autoscaler you deploy yourself, or a controller you run on your
laptop against a forwarded port (`examples/k3d/create-cluster.sh` and `development/` do exactly that,
and the [Development Setup](../development/setup.md) page explains the forwarder).

### Values For A Tighter Pool

Longer scale-down windows, a pool that keeps one machine up, metrics scraped:

```yaml
# production-values.yaml
controllerManager:
  replicas: 2            # leader election makes the second replica a hot standby
  container:
    resources:
      limits:
        cpu: 1000m
        memory: 512Mi
      requests:
        cpu: 100m
        memory: 128Mi
  # Keep the manager on a control-plane node, where the pool's machines are not
  nodeSelector:
    node-role.kubernetes.io/control-plane: ""
  tolerations:
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule

clusterAutoscaler:
  enabled: true
  scaleDownDelay: "30m"
  scaleDownUnneededTime: "30m"
  scaleDownUtilizationThreshold: 0.3
  extraArgs:
    - --balance-similar-node-groups
  resources:
    limits:
      cpu: 500m
      memory: 256Mi
    requests:
      cpu: 100m
      memory: 128Mi

prometheus:
  enable: true           # ServiceMonitor; needs the Prometheus Operator CRDs
networkPolicy:
  enable: true
```

Per-pool numbers — the utilization thresholds, the scale-down windows, the provisioning budget, the
floor — are fields of the `Group` CR, not of the chart: see
[Group CRD Reference](../api-reference/crds/group.md). The chart's `clusterAutoscaler.*` values are
cluster-autoscaler's own global flags.

## Post-Installation Setup

### 1. Create A Group

Every field of the spec except `minSize` and `nodeReadinessTaints` is required, and the two
utilization thresholds are **strings**: `0.5` unquoted is rejected by the API server.

```yaml
# group-example.yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: worker-nodes
  namespace: homelab-autoscaler-system
spec:
  # The floor cluster-autoscaler may not scale the pool below. 0 lets it scale to nothing.
  minSize: 0
  scaleDownUtilizationThreshold: "0.5"
  scaleDownGpuUtilizationThreshold: "0.5"
  scaleDownUnneededTime: "10m"
  scaleDownUnreadyTime: "20m"
  maxNodeProvisionTime: "2m"
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: true
```

```bash
kubectl apply -f group-example.yaml
```

There is no `maxSize`: the pool's ceiling is the number of `Node` CRs you label
`group: worker-nodes`, so the pool grows and shrinks by committing slots, not by editing the Group.
There is no `nodeSelector` on a Group either — selection lives on the slots
([`spec.nodeLabels`](../api-reference/crds/node.md#node-labels-and-annotations)) and on the `group`
label that binds a slot to this pool.

`maxNodeProvisionTime` is the budget of one boot: cluster-autoscaler gets it as its own scale-up
termination deadline, and it is the floor of every claim's `spec.deadlineAt` in this pool. Keep it
above the time your machine takes from power-on to a Ready, gate-clear node.

### 2. Declare The Slots

One `Node` CR per machine. `startupPodSpec` / `shutdownPodSpec` are a pod shape the controller wraps
in a Job: `image`, `command`, `args`, `volumes` (Secret / ConfigMap mounts), `serviceAccount`,
`priorityClassName`, `ttlSecondsAfterFinished`, and nothing else — a field it does not know is pruned
silently, so credentials come from a mounted Secret, not from `env`.

```yaml
# node-example.yaml
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
    hourlyRate: "0.05"   # strings, so they are quoted
    podRate: "0.001"
  # What the kubelet reports for this machine, so the pool can scale up from zero slots
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
          -U "$(/etc/ipmi/user)" -P "$(/etc/ipmi/password)" power on
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
          -U "$(/etc/ipmi/user)" -P "$(/etc/ipmi/password)" power soft
    volumes:
      - name: ipmi-credentials
        secretName: ipmi-credentials
        mountPath: /etc/ipmi
    serviceAccount: controller-manager
```

```bash
kubectl apply -f node-example.yaml
```

The Secret is read by the script from its mount: `spec.volumes` takes `{name, mountPath, secretName}`
(or `configMapName`), which is not a Kubernetes pod volume, so the nested `secret: {secretName: …}`
shape is pruned and the script runs with no credentials. The ServiceAccount you name must exist in
the pool's namespace and be allowed to create Jobs and patch node objects;
`controller-manager` is the chart's own, and the [k3d examples](../api-reference/examples/k3d-node.md)
use it for that reason.

Label every machine of the pool with the same `group`, and put machines the scheduler must tell apart
in different Groups: a Group answers cluster-autoscaler with one node template, and a label its slots
disagree on is left out of it.

### 3. Check The Autoscaler Sees The Pool

With `clusterAutoscaler.enabled: true`, cluster-autoscaler runs in the release namespace and dials
`homelab-autoscaler-grpc-service:50051`:

```bash
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-cluster-autoscaler \
  | grep -E "Adding group node-info|Group .* worker-nodes|Registered threshold"
```

You want `Adding group node-info with node group id worker-nodes` in that log. Nothing is in it about
`worker-nodes` means the gRPC endpoint does not answer, and the first thing to read is the manager's
own log:

```bash
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep -i grpc
```

## Verification

```bash
# Manager, and cluster-autoscaler when you enabled it
kubectl get pods -n homelab-autoscaler-system

# The three CRDs
kubectl get crd groups.infra.homecluster.dev nodes.infra.homecluster.dev nodeclaims.infra.homecluster.dev

# The gRPC Service cluster-autoscaler calls, and its endpoints
kubectl get service homelab-autoscaler-grpc-service -n homelab-autoscaler-system
kubectl get endpoints homelab-autoscaler-grpc-service -n homelab-autoscaler-system

# The pool, its slots, the requests: SLOTS ON OFF LASTSCALE HEALTH, and POWERSTATE PHASE GROUP ZONE SINCE ON24H
kubectl get groups -n homelab-autoscaler-system
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
kubectl get nodeclaims -n homelab-autoscaler-system
```

A fresh pool reads `SLOTS 1 / ON 0 / OFF 1`, and the slot reads `off off worker-nodes`: nothing has
asked for the machine, which is the correct state of a pool at rest.

```bash
# Test the gRPC API directly
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc-service 50051:50051 &
grpcurl -plaintext localhost:50051 list
grpcurl -plaintext -d '{}' localhost:50051 \
  clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider/NodeGroups
kill %1
```

## Troubleshooting

### CRDs Missing

```bash
kubectl get crd groups.infra.homecluster.dev nodes.infra.homecluster.dev nodeclaims.infra.homecluster.dev
```

If they are absent (`crd.enable: false`, or a release applied without templates), apply the three
files by name. `kubectl apply -f <a directory URL>` fails: name each file.

```bash
base=https://raw.githubusercontent.com/homecluster-dev/homelab-autoscaler/main/config/crd/bases
kubectl apply -f $base/infra.homecluster.dev_groups.yaml
kubectl apply -f $base/infra.homecluster.dev_nodes.yaml
kubectl apply -f $base/infra.homecluster.dev_nodeclaims.yaml
```

### The Manager Pod Never Becomes Ready

```bash
kubectl describe pod -n homelab-autoscaler-system -l control-plane=controller-manager
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager
```

- `CreateContainerConfigError` naming `webhook-server-cert` / `metrics-server-cert`: cert-manager is
  not installed, or `certmanager.enable` is `true` while it is absent
- `no matches for kind Node in version infra.homecluster.dev/v1alpha1`: the CRDs on the cluster are
  older than the operator. Re-apply the three files above
- RBAC denials (`cannot patch "nodes"` on the core API group, or on `nodeclaims/finalizers`):
  `rbac.enable: false` with a manager that needs the rights

### A Group Is Refused At Apply

The API server names what it wants. The three cases:

- `spec.scaleDownUtilizationThreshold: Invalid value: 0.5: … expected string` — quote the threshold
- `spec: Required value: … scaleDownUnneededTime` — the spec needs all seven fields
- `unknown field "spec.maxSize"` is not what you get: unknown fields are **pruned silently** by a
  structural schema, so a Group with a `maxSize` applies and the number does nothing. The ceiling is
  the slot count

### A Node Is Refused At Apply

- `spec.pricing.hourlyRate: Invalid value: 0.05: … expected string` — quote the prices
- `spec.startupPodSpec.resources: unknown field` is a silent prune, not an error: the Job's pod takes
  no resource requests, and the script that lost its `env` runs without credentials

### Image Pull Fails

```bash
kubectl describe pod -n homelab-autoscaler-system -l control-plane=controller-manager | grep -A3 Image
kubectl get events -n homelab-autoscaler-system --sort-by='.lastTimestamp' | tail
```

The default image is `homeclusterdev/autoscaler:0.2.3` on Docker Hub. Mirror it, and set
`controllerManager.container.image.repository`, if the cluster has no Docker Hub egress.

## Upgrading

```bash
helm repo update
helm upgrade homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --version 0.2.3 \
  --values values.yaml
```

The CRDs are chart templates, so `helm upgrade` updates their schemas — unlike a chart that carries
them in `crds/`, which never replaces them. Read the
[changelog](../releases/changelog.md) before a minor bump: `0.2.0` removed `v1alpha1` and
`Node.spec.powerState`, and that release's migration is documented in
[NodeClaim Design §9](../design/nodeclaim.md#9-migration).

Pin the chart version you run: `helm list -n homelab-autoscaler-system` prints it, and
`--version` on the upgrade is what keeps the image tag and the CRDs of one release together.

## Uninstallation

```bash
helm uninstall homelab-autoscaler -n homelab-autoscaler-system
```

With the default `crd.keep: true` the CRDs, and every Group, Node and NodeClaim in them, survive the
release. To take the pool's data with it, delete the CRDs by name — all three, `nodeclaims` included,
or a pool cannot be reinstalled clean:

```bash
kubectl delete crd groups.infra.homecluster.dev
kubectl delete crd nodes.infra.homecluster.dev
kubectl delete crd nodeclaims.infra.homecluster.dev
kubectl delete namespace homelab-autoscaler-system
```

## Next Steps

1. [Quick Start](quick-start.md) — the same system on k3d, with a power API you can run on a laptop
2. [Group CRD](../api-reference/crds/group.md) and [Node CRD](../api-reference/crds/node.md) — the
   fields you are declaring
3. [Common Issues & Solutions](../troubleshooting/common-issues.md) — the runbooks, read the pool at
   a glance first
