# Group CRD Reference

The Group Custom Resource Definition (CRD) defines autoscaling policies for collections of physical nodes.

## API Version
- **Group**: `infra.homecluster.dev`
- **Version**: `v1alpha1`
- **Kind**: `Group`

## Specification

### GroupSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `scaleDownUtilizationThreshold` | `string` | Yes | CPU utilization threshold for scale-down, a decimal string (`"0.5"`) |
| `scaleDownGpuUtilizationThreshold` | `string` | Yes | GPU utilization threshold for scale-down, a decimal string (`"0.5"`) |
| `scaleDownUnneededTime` | `*metav1.Duration` | Yes | Time a node must be unneeded before scale-down |
| `scaleDownUnreadyTime` | `*metav1.Duration` | Yes | Time an unready node waits before scale-down |
| `maxNodeProvisionTime` | `*metav1.Duration` | Yes | Maximum time to wait for node provisioning |
| `zeroOrMaxNodeScaling` | `bool` | Yes | Whether to scale to zero or max nodes only |
| `ignoreDaemonSetsUtilization` | `bool` | Yes | Ignore DaemonSet pods in utilization calculations |
| `minSize` | `int32` | No | Floor cluster-autoscaler must not scale the group below. Defaults to `0` |
| `nodeReadinessTaints` | `[]corev1.Taint` | No | Taints a machine of this group must be free of before the controller calls the slot ready and uncordons it, on top of the node reporting `Ready`. Defaults to Cilium's `node.cilium.io/agent-not-ready` (`NoExecute`); an empty list gates on `Ready` alone |

## Readiness Gate

`nodeReadinessTaints` is the list the **scale-up** transition into `ready` (`JobCompleted` → Ready)
waits for. The kubelet marks a node Ready long before the CNI has programmed its datapath, and the
pool's workloads do not tolerate the bootstrap taint the CNI stamps while it works, so a slot declared
ready in that window is read by cluster-autoscaler as capacity its pending pod cannot land on - and
the pool buys a second machine for a node that is about to work. The controller holds the scale-up
open until the taint is gone, so pool workloads need no toleration for it.

- The **key** is what the gate matches; Kubernetes keeps one taint per key on a node object, so a
  declaration's `value` and `effect` are documentation and are not compared.
- The CRD defaults the list to Cilium's key (Cilium has stamped `node.cilium.io/agent-not-ready`
  since v1.9; the pool this chart deploys runs 1.16.x). A Group written before the field existed gets
  the gate it was already getting.
- `nodeReadinessTaints: []` turns the gate off: a pool on a CNI that stamps no bootstrap taint gates
  on `Ready` alone.
- These are the taints the gate **reads**. The taints to stamp on the machine are
  `spec.nodeTaints` of the Node CR, and a key must not be in both: the gate's key is stamped and
  removed by its owner, so a declaration that stamps it makes the slot's own scale-up wait for a
  taint the slot keeps putting back. `taints.IsProtectedKey` refuses Cilium's key in `nodeTaints`.
- The gate is bounded: 90s for the node object to register, then 4m for readiness, counted from the
  moment the object appears. A timeout **uncordons** the node, records a `ReadinessGateTimeout`
  Warning on the slot, and completes the transition, because a cordoned node is skipped by
  cluster-autoscaler's scale-down - never drained, never deleted, and paid for forever.

```yaml
spec:
  # The pool boots on a CNI that stamps its own startup taint.
  nodeReadinessTaints:
  - key: node.cilium.io/agent-not-ready
    effect: NoExecute
---
spec:
  # flannel / a cloud CNI: no bootstrap taint, so no gate beyond Ready.
  nodeReadinessTaints: []
```

The keys a slot is gated on are recorded on the slot as the `NodeReadinessTaints` condition, so a slot
that has been booting for ten minutes says what it is waiting for. See
[State Management](../../architecture/state.md).

## Example

```yaml
apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: worker-nodes
  namespace: homelab-autoscaler-system
spec:
  # Keep one node of the pool up; omit for a group that may scale to nothing.
  minSize: 1
  scaleDownUtilizationThreshold: "0.5"
  scaleDownGpuUtilizationThreshold: "0.5"
  scaleDownUnneededTime: "10m"
  scaleDownUnreadyTime: "20m"
  maxNodeProvisionTime: "15m"
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: true
```

## Usage

### Creating a Group

1. **Set scaling thresholds** for CPU and GPU utilization
2. **Configure timing parameters** for scale-down behavior
3. **Apply the Group resource** to your cluster

The size of a group is not part of the spec beyond `minSize`: its max size is the number
of Node CRs labelled `group: <group name>`, so a pool grows and shrinks by committing
slots, not by editing the Group.

```bash
kubectl apply -f group.yaml
```

### Monitoring Group Status

```bash
# Check group status
kubectl get groups -n homelab-autoscaler-system

# Get detailed group information
kubectl describe group worker-nodes -n homelab-autoscaler-system

# Watch group status changes
kubectl get groups -n homelab-autoscaler-system -w
```

## Scaling Behavior

### Scale-Up Triggers
- Pod scheduling failures due to insufficient resources
- CPU/memory pressure on existing nodes
- External scaling requests via gRPC API

### Scale-Down Triggers
- Node utilization below `scaleDownUtilizationThreshold`
- GPU utilization below `scaleDownGpuUtilizationThreshold`
- Node unneeded for longer than `scaleDownUnneededTime`
- Node unready for longer than `scaleDownUnreadyTime`

### Minimum Size

`minSize` is the floor of the group. The gRPC cloud provider reports it to
cluster-autoscaler as the min size of the node group, and refuses its own scale-down
entry points (`NodeGroupDeleteNodes`, `NodeGroupDecreaseTargetSize`) when the request
would take the group below it, so a pool that has to keep a machine up sets `minSize: 1`
and the autoscaler stops reclaiming that node. A pool that used a keepalive pod to pin an
always-on slot above the utilization threshold can drop the pod once the group declares
the floor.

Two things to know:

- `0` is the default and matches the behaviour of every release before the field existed:
  the group may scale down to nothing.
- `maxSize` is **not** a field. The provider derives it by counting the Node CRs labelled
  `group: <group name>`. The CRD therefore cannot assert `minSize <= maxSize`; the gRPC
  boundary does, clamping a `minSize` above the number of slots to that count and refusing
  a scale up that would exceed it.

`minSize: 0` is a supported floor, and a pool that uses it answers one more question: with no slot
registered, cluster-autoscaler sizes the group's node template from
[`spec.nodeCapacity` on the Node CRs](node.md#node-capacity). A pool at zero that declares nothing
has a template with no cpu and no memory, no pod with requests fits it, and `NodeGroupIncreaseSize`
is never called, so the group stays at zero and the floor that works is `minSize: 1`.

### Timing Parameters

| Parameter | Purpose | Typical Value |
|-----------|---------|---------------|
| `scaleDownUnneededTime` | Prevents flapping by requiring sustained low utilization | `10m` |
| `scaleDownUnreadyTime` | Removes unresponsive nodes | `20m` |
| `maxNodeProvisionTime` | Timeout for node startup operations | `15m` |

## Best Practices

### Resource Planning
- Account for startup time in `maxNodeProvisionTime`

### Threshold Tuning
- Start with conservative thresholds (0.5-0.7) and adjust based on workload patterns
- Monitor actual utilization patterns before optimizing thresholds
- Consider workload characteristics (CPU vs memory intensive)

## Troubleshooting

### Common Issues

1. **Nodes not joining group**
   - Check that `group` label is set on Node CRDs

2. **Scaling not working**
   - Verify gRPC server is running and accessible
   - Check controller logs for errors

### Debugging Commands

```bash
# Check group controller logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager

# List nodes in group
kubectl get nodes -l group=worker-nodes

# Check node CRDs
kubectl get nodes.infra.homecluster.dev -l group=worker-nodes

# Verify gRPC server status
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc 50051:50051
```

## Related Documentation

- [Node CRD](node.md) - Individual node management
- [Architecture Overview](../../architecture/overview.md) - System design
