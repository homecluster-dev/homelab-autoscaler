# Overview

Homelab Autoscaler scales a Kubernetes cluster of physical machines: it powers real machines on when
cluster-autoscaler has work for them, and off when it has none, over wake-on-LAN, IPMI, BMC, or a
smart PDU. It does not create VMs — the machines exist, and the pool's job is to decide which of them
are up.

## Key Features

- **Physical machine power management** - Wake-on-LAN, IPMI, BMC, smart PDU support
- **Kubernetes-native integration** - three Custom Resource Definitions and their controllers
- **Cluster Autoscaler compatible** - the standard gRPC CloudProvider interface
- **Power-aware scaling** - the pool's ceiling is the slots you commit, and an idle pool is a pool at
  zero

## How It Works

### Scale Up Process
1. Workload demand exceeds available resources
2. Cluster Autoscaler asks the gRPC provider for more nodes
3. The provider creates a `NodeClaim` on each free slot, with a deadline
4. The slot's startup Job powers the machine on, and the claim moves `pending → booting`
5. The machine boots, joins, clears the pool's readiness gate; the claim goes `ready`
6. Pending pods schedule on the new node

### Scale Down Process
1. Node utilization drops below the group's thresholds
2. Cluster Autoscaler drains the node and calls `DeleteNodes`
3. The provider records that request on the slot's claim and cancels it
4. The slot's shutdown Job powers the machine off
5. The claim ends `completed`, the slot reports `off`, the machine is removed from the cluster

## Core Components

### Custom Resources

Three objects, three lifetimes: a pool's policy, a machine's declaration, and a request that ends.

**Group CRD** - autoscaling policy for a pool. Every pool is a set of slots labelled
`group: <name>`; the pool's ceiling is that count, there is no `maxSize`:
```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: worker-nodes
  namespace: homelab-autoscaler-system
spec:
  minSize: 0                      # 0 lets the pool scale to nothing
  scaleDownUtilizationThreshold: "0.5"   # a string, so it is quoted
  scaleDownGpuUtilizationThreshold: "0.5"
  scaleDownUnneededTime: "10m"
  scaleDownUnreadyTime: "20m"
  maxNodeProvisionTime: "2m"      # the budget of one boot
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: true
```

**Node CRD** - one machine slot: what the machine *is*, and nothing about what anyone wants it to do
next. Selection is here, on the slot: the `group` label puts it in a pool, `spec.nodeLabels` is what
a `nodeSelector` matches:
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
    hourlyRate: "0.05"            # strings, so they are quoted
    podRate: "0.001"
  nodeLabels:
    node-type: worker
  startupPodSpec:
    image: gemtest/ipmitool:latest
    command: ["ipmitool"]
    args: ["-H", "worker-01-ipmi", "power", "on"]
  shutdownPodSpec:
    image: gemtest/ipmitool:latest
    command: ["ipmitool"]
    args: ["-H", "worker-01-ipmi", "power", "soft"]
```

**NodeClaim CRD** - one request for one machine. The autoscaler's scale-up is an object with an
attribution, a deadline and an end, which is why a GitOps apply cannot revert it:
```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: NodeClaim
metadata:
  name: worker-01-7f4b9c
  namespace: homelab-autoscaler-system
spec:
  slotRef:
    name: worker-01
  requestedBy: cluster-autoscaler
  request:
    call: IncreaseSize
    delta: 1
    group: worker-nodes
  requestedAt: "2026-03-04T09:38:15Z"
  deadlineAt: "2026-03-04T09:50:25Z"
```

### Controllers
- **Group Controller** - counts the pool's slots and materializes its shape (`slots`, `on`, `off`,
  `health`, the last scale)
- **Slot reconciler** (`Node`) - reads the live claim, the observed power and the node's usability,
  and fires the lifecycle transition that input asks for
- **Claim Controller** (`NodeClaim`) - the deadlines, the attempt bounds, the TTL collection
- **Core Controller** - keeps the `corev1.Node` and the Node CR in agreement

### gRPC Server
Implements Cluster Autoscaler CloudProvider interface for node management.

## Power Management

Supported power control methods:
- Wake-on-LAN packets
- IPMI/BMC interfaces
- Smart PDUs
- Any command that reaches the machine: `spec.startupPodSpec` and `spec.shutdownPodSpec` are an image,
  a command and the Secrets it mounts, run as a Job

## Getting Started

Choose your installation method:
- **[Quick Start](quick-start.md)** - k3d setup for testing
- **[Installation Guide](installation.md)** - Helm deployment for production

The model behind every example on this page is
[The NodeClaim model](../concepts/nodeclaim-model.md); the limits worth knowing before you deploy are
in [Known Issues](../troubleshooting/known-issues.md).

## Use Cases

- **Development clusters** - scale down overnight, up during work hours
- **Batch processing** - scale up for a large job, down when it finishes
- **Power cost** - a pool that is idle is a pool that is off
- **Resource bursting** - temporary workload spikes on machines you already own